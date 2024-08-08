// Copyright 2022 The Nakama Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/rtapi"
	"github.com/heroiclabs/nakama-common/runtime"
	"github.com/heroiclabs/nakama/v3/server/evr"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	findAttemptsExpiry          = time.Minute * 3
	LatencyCacheRefreshInterval = time.Hour * 3
	LatencyCacheExpiry          = time.Hour * 72 // 3 hours

	MatchmakingStorageCollection = "MatchmakingRegistry"
	LatencyCacheStorageKey       = "LatencyCache"
)

var (
	ErrMatchmakingPingTimeout          = status.Errorf(codes.DeadlineExceeded, "Ping timeout")
	ErrMatchmakingTimeout              = status.Errorf(codes.DeadlineExceeded, "Matchmaking timeout")
	ErrMatchmakingNoAvailableServers   = status.Errorf(codes.Unavailable, "No available servers")
	ErrMatchmakingCanceled             = status.Errorf(codes.Canceled, "Matchmaking canceled")
	ErrMatchmakingCanceledByPlayer     = status.Errorf(codes.Canceled, "Matchmaking canceled by player")
	ErrMatchmakingCanceledByParty      = status.Errorf(codes.Aborted, "Matchmaking canceled by party member")
	ErrMatchmakingRestarted            = status.Errorf(codes.Canceled, "matchmaking restarted")
	ErrMatchmakingMigrationRequired    = status.Errorf(codes.FailedPrecondition, "Server upgraded, migration")
	ErrMatchmakingUnknownError         = status.Errorf(codes.Unknown, "Unknown error")
	MatchmakingStreamSubject           = uuid.NewV5(uuid.Nil, "matchmaking").String()
	MatchmakingConfigStorageCollection = "Matchmaker"
	MatchmakingConfigStorageKey        = "config"
)

type LatencyMetric struct {
	Endpoint  evr.Endpoint
	RTT       time.Duration
	Timestamp time.Time
}

// String returns a string representation of the endpoint
func (e *LatencyMetric) String() string {
	return fmt.Sprintf("EndpointRTT(InternalIP=%s, ExternalIP=%s, RTT=%s, Timestamp=%s)", e.Endpoint.InternalIP, e.Endpoint.ExternalIP, e.RTT, e.Timestamp)
}

// ID returns a unique identifier for the endpoint
func (e *LatencyMetric) ID() string {
	return e.Endpoint.GetExternalIP()
}

// The key used for matchmaking properties
func (e *LatencyMetric) AsProperty() (string, float64) {
	k := fmt.Sprintf("rtt%s", ipToKey(e.Endpoint.ExternalIP))
	v := float64(e.RTT / time.Millisecond)
	return k, v
}

// LatencyCache is a cache of broadcaster RTTs for a user
type LatencyCache struct {
	MapOf[string, LatencyMetric]
}

func NewLatencyCache() *LatencyCache {
	return &LatencyCache{
		MapOf[string, LatencyMetric]{},
	}
}

func (c *LatencyCache) SelectPingCandidates(endpoints ...evr.Endpoint) []evr.Endpoint {
	// Initialize candidates with a capacity of 16
	metrics := make([]LatencyMetric, 0, len(endpoints))
	for _, endpoint := range endpoints {
		id := endpoint.GetExternalIP()
		e, ok := c.Load(id)
		if !ok {
			e = LatencyMetric{
				Endpoint:  endpoint,
				RTT:       0,
				Timestamp: time.Now(),
			}
			c.Store(id, e)
		}
		metrics = append(metrics, e)
	}

	sort.SliceStable(endpoints, func(i, j int) bool {
		// sort by expired first
		if time.Since(metrics[i].Timestamp) > LatencyCacheRefreshInterval {
			// Then by those that have responded
			if metrics[j].RTT > 0 {
				// Then by timestamp
				return metrics[i].Timestamp.Before(metrics[j].Timestamp)
			}
		}
		// Otherwise, sort by RTT
		return metrics[i].RTT < metrics[j].RTT
	})

	if len(endpoints) == 0 {
		return []evr.Endpoint{}
	}
	if len(endpoints) > 16 {
		endpoints = endpoints[:16]
	}
	return endpoints
}

type MatchmakerTicket struct {
	ID                string                `json:"id"`
	Presences         []*MatchmakerPresence `json:"presences"`
	SessionID         string                `json:"session_id"`
	MemberPresenceIDs []*PresenceID         `json:"member_presence_ids"`
	MinCount          int                   `json:"min_count"`
	MaxCount          int                   `json:"max_count"`
	CountMultiple     int                   `json:"count_multiple"`
	Query             string                `json:"query"`
	StringProps       map[string]string     `json:"string_properties"`
	NumericProps      map[string]float64    `json:"numeric_properties"`
	PartyID           string                `json:"party_id"`
	CreatedAt         time.Time             `json:"created_at"`
}

// MatchmakingSession represents a user session looking for a match.
type MatchmakingSession struct {
	sync.RWMutex
	Ctx         context.Context
	CtxCancelFn context.CancelCauseFunc

	Logger   *zap.Logger
	nk       runtime.NakamaModule
	registry *MatchmakingRegistry

	UserID        uuid.UUID
	PingResultsCh chan []evr.EndpointPingResult // Channel for ping completion.
	Expiry        time.Time
	Label         *MatchLabel
	Tickets       map[string]*MatchmakerTicket // map[ticketId]TicketMeta
	Party         *PartyGroup
	LatencyCache  *LatencyCache
	Session       *sessionWS
}

func (s *MatchmakingSession) metricsTags() map[string]string {

	partySize := 1
	if s.Party != nil {
		partySize = s.Party.Size()
	}

	return map[string]string{
		"type":       s.Label.LobbyType.String(),
		"mode":       s.Label.Mode.String(),
		"level":      s.Label.Level.String(),
		"role":       strconv.FormatInt(int64(s.Label.TeamIndex), 10),
		"party_size": strconv.FormatInt(int64(partySize), 10),
		"group_id":   s.Label.GetGroupID().String(),
	}
}

// Cancel cancels the matchmaking session with a given reason, and returns the reason.
func (s *MatchmakingSession) Cancel(reason error) error {
	s.Logger.Debug("Canceling matchmaking session.", zap.Error(reason))
	s.Lock()
	defer s.Unlock()
	select {
	case <-s.Ctx.Done():
		return nil
	default:
	}
	s.CtxCancelFn(reason)
	return nil
}

func (s *MatchmakingSession) AddTicket(id string, sessionID string, presences []*MatchmakerPresence, memberPresenceIDs []*PresenceID, partyID, query string, minCount, maxCount, countMultiple int, stringProperties map[string]string, numericProperties map[string]float64) {
	s.Lock()
	defer s.Unlock()

	s.Tickets[id] = &MatchmakerTicket{
		ID:                id,
		Presences:         presences,
		SessionID:         sessionID,
		MemberPresenceIDs: memberPresenceIDs,
		MinCount:          minCount,
		MaxCount:          maxCount,
		CountMultiple:     countMultiple,
		Query:             query,
		StringProps:       stringProperties,
		NumericProps:      numericProperties,
		PartyID:           partyID,
	}

}

func (s *MatchmakingSession) RemoveTicket(ticket string) {
	s.Lock()
	defer s.Unlock()
	delete(s.Tickets, ticket)
}

func (s *MatchmakingSession) Wait() error {
	<-s.Ctx.Done()
	return nil
}

func (s *MatchmakingSession) TicketsCount() int {
	s.RLock()
	defer s.RUnlock()
	return len(s.Tickets)
}

func (s *MatchmakingSession) Context() context.Context {
	s.RLock()
	defer s.RUnlock()
	return s.Ctx
}

func (s *MatchmakingSession) Cause() error {
	if s.Ctx.Err() != context.Cause(s.Ctx) {
		return context.Cause(s.Ctx)
	}
	return nil
}

func (s *MatchmakingSession) GetPartyMatchmakingSessions() []*MatchmakingSession {
	if s.Party == nil || s.Party.Size() <= 1 {
		return []*MatchmakingSession{s}
	}
	msessions := make([]*MatchmakingSession, 0, s.Party.Size())
	for _, member := range s.Party.List() {
		msession, ok := s.registry.GetMatchingBySessionId(uuid.FromStringOrNil(member.Presence.GetSessionId()))
		if !ok {
			s.Logger.Warn("Could not find matching session for user", zap.String("sessionID", member.Presence.GetSessionId()))
			continue
		}
		msessions = append(msessions, msession)
	}
	return msessions
}

// MatchmakingResult represents the outcome of a matchmaking request
type MatchmakingResult struct {
	err     error
	Message string
	Code    evr.LobbySessionFailureErrorCode
	Mode    evr.Symbol
	Channel uuid.UUID
	Logger  *zap.Logger
}

// NewMatchmakingResult initializes a new instance of MatchmakingResult
func NewMatchmakingResult(logger *zap.Logger, mode evr.Symbol, channel uuid.UUID) *MatchmakingResult {
	return &MatchmakingResult{
		Logger:  logger,
		Mode:    mode,
		Channel: channel,
	}
}

// SetErrorFromStatus updates the error and message from a status
func (mr *MatchmakingResult) SetErrorFromStatus(err error) *MatchmakingResult {
	if err == nil {
		return nil
	}
	mr.err = err

	if s, ok := status.FromError(err); ok {
		mr.Message = s.Message()
		mr.Code = determineErrorCode(s.Code())
	} else {
		mr.Message = err.Error()
		mr.Code = evr.LobbySessionFailure_InternalError
	}
	return mr
}

// determineErrorCode maps grpc status codes to evr lobby session failure codes
func determineErrorCode(code codes.Code) evr.LobbySessionFailureErrorCode {
	switch code {
	case codes.DeadlineExceeded:
		return evr.LobbySessionFailure_Timeout0
	case codes.InvalidArgument:
		return evr.LobbySessionFailure_BadRequest
	case codes.Aborted:
		return evr.LobbySessionFailure_BadRequest
	case codes.ResourceExhausted:
		return evr.LobbySessionFailure_ServerIsFull
	case codes.Unavailable:
		return evr.LobbySessionFailure_ServerFindFailed
	case codes.NotFound:
		return evr.LobbySessionFailure_ServerDoesNotExist
	case codes.PermissionDenied, codes.Unauthenticated:
		return evr.LobbySessionFailure_KickedFromLobbyGroup
	case codes.FailedPrecondition:
		return evr.LobbySessionFailure_ServerIsIncompatible

	default:
		return evr.LobbySessionFailure_InternalError
	}
}

// SendErrorToSession sends an error message to a session
func (mr *MatchmakingResult) SendErrorToSession(s *sessionWS, err error) error {
	result := mr.SetErrorFromStatus(err)
	if result == nil {
		return nil
	}
	// If it was cancelled by the user, don't send and error
	if result.err == ErrMatchmakingCanceledByPlayer || result.err.Error() == "context canceled" {
		return nil
	}

	mr.Logger.Warn("Matchmaking error", zap.String("message", result.Message), zap.Error(result.err))
	return s.SendEvr(evr.NewLobbySessionFailure(result.Mode, result.Channel, result.Code, result.Message).Version4())
}

// MatchmakingRegistry is a registry for matchmaking sessions
type MatchmakingRegistry struct {
	sync.RWMutex

	ctx         context.Context
	ctxCancelFn context.CancelFunc

	nk     runtime.NakamaModule
	db     *sql.DB
	logger *zap.Logger

	matchRegistry MatchRegistry
	metrics       Metrics
	config        Config
	evrPipeline   *EvrPipeline

	matchingBySession *MapOf[uuid.UUID, *MatchmakingSession]
	cacheByUserId     *MapOf[uuid.UUID, *LatencyCache]
}

func NewMatchmakingRegistry(logger *zap.Logger, matchRegistry MatchRegistry, matchmaker Matchmaker, metrics Metrics, db *sql.DB, nk runtime.NakamaModule, config Config, evrPipeline *EvrPipeline) *MatchmakingRegistry {
	ctx, ctxCancelFn := context.WithCancel(context.Background())
	c := &MatchmakingRegistry{
		ctx:         ctx,
		ctxCancelFn: ctxCancelFn,

		nk:            nk,
		db:            db,
		logger:        logger,
		matchRegistry: matchRegistry,
		metrics:       metrics,
		config:        config,
		evrPipeline:   evrPipeline,

		matchingBySession: &MapOf[uuid.UUID, *MatchmakingSession]{},
		cacheByUserId:     &MapOf[uuid.UUID, *LatencyCache]{},
	}
	// Set the matchmaker's OnMatchedEntries callback
	matchmaker.OnMatchedEntries(c.matchedEntriesFn)

	return c
}

type MatchmakingSettings struct {
	DisableArenaBackfill  bool     `json:"disable_arena_backfill,omitempty"` // Disable backfilling for arena matches
	BackfillQueryAddon    string   `json:"backfill_query_addon"`             // Additional query to add to the matchmaking query
	MatchmakingQueryAddon string   `json:"matchmaking_query_addon"`          // Additional query to add to the matchmaking query
	CreateQueryAddon      string   `json:"create_query_addon"`               // Additional query to add to the matchmaking query
	GroupID               string   `json:"group_id"`                         // Group ID to matchmake with
	PriorityBroadcasters  []string `json:"priority_broadcasters"`            // Prioritize these broadcasters
	NextMatchID           MatchID  `json:"next_match_id"`                    // Try to join this match immediately when finding a match
	Verbose               bool     `json:"verbose"`                          // Send the user verbose messages via discord
}

func (MatchmakingSettings) GetStorageID() StorageID {
	return StorageID{
		Collection: MatchmakingConfigStorageCollection,
		Key:        MatchmakingConfigStorageKey,
	}
}

func LoadMatchmakingSettings(ctx context.Context, nk runtime.NakamaModule, userID string) (settings MatchmakingSettings, err error) {
	err = LoadFromStorage(ctx, nk, userID, &settings, true)
	return
}

func StoreMatchmakingSettings(ctx context.Context, nk runtime.NakamaModule, userID string, settings MatchmakingSettings) error {
	return SaveToStorage(ctx, nk, userID, settings)
}

func ipToKey(ip net.IP) string {
	b := ip.To4()
	return fmt.Sprintf("rtt%02x%02x%02x%02x", b[0], b[1], b[2], b[3])
}
func keyToIP(key string) net.IP {
	b, _ := hex.DecodeString(key[3:])
	return net.IPv4(b[0], b[1], b[2], b[3])
}

func (mr *MatchmakingRegistry) matchedEntriesFn(entries [][]*MatchmakerEntry) {
	// Get the matchmaking config from the storage
	config, err := LoadMatchmakingSettings(mr.ctx, mr.nk, SystemUserID)
	if err != nil {
		mr.logger.Error("Failed to load matchmaking config", zap.Error(err))
		return
	}

	for _, entrants := range entries {
		go mr.buildMatch(entrants, config)
	}
}

func (mr *MatchmakingRegistry) listUnfilledLobbies(ctx context.Context, partySize int, mode evr.Symbol, query string) ([]*MatchLabel, error) {
	var err error

	var labels []*MatchLabel

	minSize := 0
	limit := 100
	lobbySize := SocialLobbyMaxSize
	if l, ok := LobbySizeByMode[mode]; ok {
		lobbySize = l
	}

	maxSize := lobbySize - partySize

	// Search for possible matches
	matches, err := mr.listMatches(ctx, limit, minSize+1, maxSize+1, query)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to find matches: %v", err)
	}

	// Create a label slice of the matches
	labels = make([]*MatchLabel, 0, len(matches))
	for _, match := range matches {
		label := &MatchLabel{}
		if err := json.Unmarshal([]byte(match.GetLabel().GetValue()), label); err != nil {
			continue
		}
		labels = append(labels, label)
	}

	return labels, nil
}

func CompactedFrequencySort[T comparable](s []T, desc bool) []T {
	s = s[:]
	// Create a map of the frequency of each item
	frequency := make(map[T]int, len(s))
	for _, item := range s {
		frequency[item]++
	}
	// Sort the items by frequency
	slices.SortStableFunc(s, func(a, b T) int {
		return frequency[a] - frequency[b]
	})
	if desc {
		slices.Reverse(s)
	}
	return slices.Compact(s)
}

func (mr *MatchmakingRegistry) buildMatch(entrants []*MatchmakerEntry, config MatchmakingSettings) {
	logger := mr.logger.With(zap.Int("entrants", len(entrants)))

	logger.Debug("Building match", zap.Any("entrants", entrants))

	// Verify that all entrants have the same group_id

	groupIDs := make([]uuid.UUID, 0, len(entrants))
	for _, e := range entrants {
		channel := uuid.FromStringOrNil(e.StringProperties["group_id"])
		if channel == uuid.Nil {
			continue
		}
		groupIDs = append(groupIDs, channel)
	}

	groupIDs = CompactedFrequencySort(groupIDs, true)
	if len(groupIDs) != 1 {
		logger.Warn("Entrants are not in the same group", zap.Any("groupIDs", groupIDs))
	}
	// Get a map of all broadcasters by their key
	broadcastersByExtIP := make(map[string]evr.Endpoint, 100)
	mr.evrPipeline.broadcasterRegistrationBySession.Range(func(_ string, v *MatchBroadcaster) bool {
		broadcastersByExtIP[v.Endpoint.GetExternalIP()] = v.Endpoint
		return true
	})

	// Create a map of each endpoint and it's latencies to each entrant
	latencies := make(map[string][]int, 100)
	for _, e := range entrants {
		nprops := e.NumericProperties
		//sprops := e.StringProperties

		// loop over the number props and get the latencies
		for k, v := range nprops {
			if strings.HasPrefix(k, "rtt") {
				latencies[k] = append(latencies[k], int(v))
			}
		}
	}

	// Score each endpoint based on the latencies
	scored := make(map[string]int, len(latencies))
	for k, v := range latencies {
		// Sort the latencies
		sort.Ints(v)
		// Get the average
		average := 0
		for _, i := range v {
			average += i
		}
		average /= len(v)
		scored[k] = average
	}
	// Sort the scored endpoints
	sorted := make([]string, 0, len(scored))
	for k := range scored {
		sorted = append(sorted, k)
	}
	sort.SliceStable(sorted, func(i, j int) bool {
		return scored[sorted[i]] < scored[sorted[j]]
	})

	parties := make(map[string][]*MatchmakerEntry, 8)
	for _, e := range entrants {
		id := e.GetPartyId()
		parties[id] = append(parties[id], e)
	}

	logger.Debug("Parties", zap.Any("parties", parties))
	// Get the ml from the first participant
	var ml *MatchLabel

	for _, e := range entrants {
		if s, ok := mr.GetMatchingBySessionId(e.Presence.SessionID); ok {
			ml = s.Label
			break
		}
	}
	if ml == nil {
		mr.logger.Error("No label found")
		return // No label found
	}

	rentrants := make([]runtime.MatchmakerEntry, 0, len(entrants))
	for _, e := range entrants {
		rentrants = append(rentrants, e)
	}

	team1, team2 := mr.evrPipeline.sbmm.BalancedMatchFromCandidate(rentrants)

	metricsTags := map[string]string{
		"type":          strconv.FormatInt(int64(ml.LobbyType), 10),
		"mode":          ml.Mode.String(),
		"level":         ml.Level.String(),
		"group_id":      ml.GetGroupID().String(),
		"entrant_count": strconv.FormatInt(int64(len(team1)+len(team2)), 10),
	}
	mr.metrics.CustomCounter("matchmaking_matched_participant_count", metricsTags, int64(len(entrants)))

	ml.Players = make([]PlayerInfo, 0, len(entrants))
	for i, players := range []RatedEntryTeam{team1, team2} {
		for _, p := range players {
			// Update the label with the teams
			evrID, err := evr.ParseEvrId(p.Entry.GetProperties()["evrid"].(string))
			if err != nil {
				logger.Error("Failed to parse evr id", zap.Error(err))
				continue
			}
			pi := PlayerInfo{
				Team:  TeamIndex(i),
				EvrID: *evrID,
			}
			ml.Players = append(ml.Players, pi)
		}
	}

	// Find a valid participant to get the label from

	ml.SpawnedBy = uuid.Nil.String()

	// Loop until a server becomes available or matchmaking times out.
	timeout := time.After(2 * time.Minute)
	interval := time.NewTicker(10 * time.Second)
	defer interval.Stop()
	select {
	case <-mr.ctx.Done(): // Context cancelled
		return
	default:
	}
	var matchID MatchID

	for {
		var err error
		matchID, err = mr.allocateBroadcaster(groupIDs, config, sorted, ml, true)
		if err != nil {
			mr.logger.Warn("Error allocating broadcaster", zap.Error(err))
		}
		if !matchID.IsNil() {
			break
		}

		select {
		case <-mr.ctx.Done(): // Context cancelled
			return

		case <-timeout: // Timeout

			mr.logger.Info("Matchmaking timeout looking for an available server")
			for _, e := range entrants {
				s, ok := mr.GetMatchingBySessionId(e.Presence.SessionID)
				if !ok {
					logger.Debug("Could not find matching session for user", zap.String("sessionID", e.Presence.SessionID.String()))
					continue
				}
				s.CtxCancelFn(ErrMatchmakingNoAvailableServers)
			}
			return
		case <-interval.C: // List all the unassigned lobbies on this channel
			// Check if the match is still unassigned

		}
	}
	<-time.After(3 * time.Second)
	// Assign the teams to the match, taking one from each team at a time
	// and sending the join instruction to the server

	if matchID.IsNil() {
		logger.Error("matchID is nil")
		return
	}

	presencesByTicket := make(map[string][]*EvrMatchPresence, len(parties))
	for i, players := range []RatedEntryTeam{team1, team2} {
		// Assign each player in the team to the match
		for _, rated := range players {
			entry := rated.Entry
			presence := entry.GetPresence()
			ticket := entry.GetTicket()
			logger := logger.With(zap.String("uid", presence.GetUserId()), zap.String("sid", presence.GetSessionId()), zap.String("ticket", ticket))
			ms, ok := mr.GetMatchingBySessionId(uuid.FromStringOrNil(entry.GetPresence().GetSessionId()))
			if !ok {
				logger.Warn("Could not find matching session for user")
				continue
			}
			// Get the ticket metadata

			ticketMeta, ok := ms.Tickets[ticket]
			if !ok {
				logger.Warn("Could not find ticket metadata for user")
				continue
			}
			matchPresence, err := NewMatchPresenceFromSession(ms, matchID, int(i), ticketMeta.Query)
			if err != nil {
				logger.Warn("Failed to create match presence", zap.Error(err))
				continue
			}
			if _, ok := presencesByTicket[ticket]; !ok {
				presencesByTicket[ticket] = make([]*EvrMatchPresence, 0, len(players))
			}
			presencesByTicket[ticket] = append(presencesByTicket[ticket], matchPresence)
			mr.metrics.CustomTimer("matchmaking_matched_duration", metricsTags, time.Since(ticketMeta.CreatedAt))
		}
	}

	successful := make([]*EvrMatchPresence, 0, len(entrants))
	errored := make([]*EvrMatchPresence, 0, len(entrants))
	for _, presences := range presencesByTicket {
		// Join the presence to the match

		s, e, err := mr.evrPipeline.LobbyJoin(mr.ctx, logger, matchID, presences...)
		if err != nil {
			for _, p := range e {
				logger.Warn("Failed to join presence to matchmade session", zap.String("mid", matchID.UUID().String()), zap.String("uid", p.GetUserId()), zap.Error(err))
			}
			errored = append(errored, e...)
			continue
		}
		successful = append(successful, s...)
		mr.metrics.CustomCounter("match_join_matched_count", metricsTags, int64(len(successful)))

	}

	label, err := MatchLabelByID(mr.ctx, mr.nk, matchID)
	if err != nil {
		logger.Error("Failed to get label from matchID", zap.Error(err))
	}

	teams := make([][]runtime.MatchmakerEntry, 0, len(successful))
	for _, p := range []RatedEntryTeam{team1, team2} {
		team := make([]runtime.MatchmakerEntry, 0, len(p))
		for _, rated := range p {
			team = append(team, rated.Entry)
		}
		teams = append(teams, team)
	}

	logger.Info("Match made", zap.Any("label", label), zap.String("mid", matchID.UUID().String()), zap.Any("teams", teams), zap.Any("errored", errored))
	go mr.SendMatchmakerMatchedNotification(label, teams, errored)

}

func distributeParties(parties [][]*MatchmakerEntry) [][]*MatchmakerEntry {
	// Distribute the players from each party on the two teams.
	// Try to keep the parties together, but the teams must be balanced.
	// The algorithm is greedy and may not always produce the best result.
	// Each team must be 4 players or less
	teams := [][]*MatchmakerEntry{{}, {}}

	// Sort the parties by size, single players last
	sort.SliceStable(parties, func(i, j int) bool {
		if len(parties[i]) == 1 {
			return false
		}
		return len(parties[i]) < len(parties[j])
	})

	// Distribute the parties to the teams
	for _, party := range parties {
		// Find the team with the least players
		team := 0
		for i, t := range teams {
			if len(t) < len(teams[team]) {
				team = i
			}
		}
		teams[team] = append(teams[team], party...)
	}
	// sort the teams by size
	sort.SliceStable(teams, func(i, j int) bool {
		return len(teams[i]) > len(teams[j])
	})

	for i, player := range teams[0] {
		// If the team is more than two players larger than the other team, distribute the players evenly
		if len(teams[0]) > len(teams[1])+1 {
			// Move a player from teams[0] to teams[1]
			teams[1] = append(teams[1], player)
			teams[0] = append(teams[0][:i], teams[0][i+1:]...)
		}
	}

	return teams
}

func (mr *MatchmakingRegistry) allocateBroadcaster(channels []uuid.UUID, config MatchmakingSettings, sorted []string, label *MatchLabel, start bool) (MatchID, error) {
	// Lock the broadcasters so that they aren't double allocated
	mr.Lock()
	defer mr.Unlock()
	available, err := mr.ListUnassignedLobbies(mr.ctx, channels)
	if err != nil {
		return MatchID{}, err
	}

	availableByExtIP := make(map[string]MatchID, len(available))

	for _, label := range available {
		k := ipToKey(label.Broadcaster.Endpoint.ExternalIP)
		availableByExtIP[k] = label.ID
	}

	// Convert the priority broadcasters to a list of rtt's
	priority := make([]string, 0, len(config.PriorityBroadcasters))
	for i, ip := range config.PriorityBroadcasters {
		priority[i] = ipToKey(net.ParseIP(ip))
	}

	sorted = append(priority, sorted...)

	var matchID MatchID
	var found bool
	for _, k := range sorted {
		// Get the endpoint
		matchID, found = availableByExtIP[k]
		if !found {
			continue
		}
		break
	}
	if matchID.IsNil() {
		return MatchID{}, ErrMatchmakingNoAvailableServers
	}
	// Found a match
	label.SpawnedBy = SystemUserID
	// Instruct the server to prepare the level
	response, err := SignalMatch(mr.ctx, mr.matchRegistry, matchID, SignalPrepareSession, label)
	if err != nil {
		return MatchID{}, fmt.Errorf("error signaling match `%s`: %s: %v", matchID.String(), response, err)
	}
	if start {
		response, err = SignalMatch(mr.ctx, mr.matchRegistry, matchID, SignalStartSession, nil)
		if err != nil {
			return MatchID{}, fmt.Errorf("error signaling match `%s`: %s: %v", matchID.String(), response, err)
		}
	}
	return matchID, nil
}

func (c *MatchmakingRegistry) ListUnassignedLobbies(ctx context.Context, channels []uuid.UUID) ([]*MatchLabel, error) {

	qparts := make([]string, 0, 10)

	// MUST be an unassigned lobby
	qparts = append(qparts, LobbyType(evr.UnassignedLobby).Query(Must, 0))

	if len(channels) > 0 {
		// MUST be hosting for this channel
		qparts = append(qparts, HostedChannels(channels).Query(Must, 0))
	}

	// TODO FIXME Add version lock and appid
	query := strings.Join(qparts, " ")
	c.logger.Debug("Listing unassigned lobbies", zap.String("query", query))
	limit := 200
	minSize, maxSize := 1, 1 // Only the 1 broadcaster should be there.
	matches, err := c.listMatches(ctx, limit, minSize, maxSize, query)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to find matches: %v", err)
	}

	// If no servers are available, return immediately.
	if len(matches) == 0 {
		return nil, ErrMatchmakingNoAvailableServers
	}

	// Create a slice containing the matches' labels
	labels := make([]*MatchLabel, 0, len(matches))
	for _, match := range matches {
		label := &MatchLabel{}
		if err := json.Unmarshal([]byte(match.GetLabel().GetValue()), label); err != nil {
			return nil, status.Errorf(codes.Internal, "Failed to unmarshal match label: %v", err)
		}
		labels = append(labels, label)
	}

	return labels, nil
}

// GetPingCandidates returns a list of endpoints to ping for a user. It also updates the broadcasters.
func (m *MatchmakingSession) GetPingCandidates(endpoints ...evr.Endpoint) (candidates []evr.Endpoint) {

	const LatencyCacheExpiry = 6 * time.Hour

	// Initialize candidates with a capacity of 16
	candidates = make([]evr.Endpoint, 0, 16)

	// Return early if there are no endpoints
	if len(endpoints) == 0 {
		return candidates
	}
	endpoints = endpoints[:]
	uniqued := make([]evr.Endpoint, 0, len(endpoints))

	seen := make(map[string]struct{}, len(endpoints))
	for _, e := range endpoints {
		id := e.GetExternalIP()
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		uniqued = append(uniqued, e)
	}
	endpoints = uniqued

	// Retrieve the user's cache and lock it for use
	cache := m.LatencyCache

	// Get the endpoint latencies from the cache
	entries := make([]LatencyMetric, 0, len(endpoints))

	// Iterate over the endpoints, and load/create their cache entry
	for _, endpoint := range endpoints {
		id := endpoint.GetExternalIP()

		e, ok := cache.Load(id)
		if !ok {
			e = LatencyMetric{
				Endpoint:  endpoint,
				RTT:       0,
				Timestamp: time.Now(),
			}
			cache.Store(id, e)
		}
		entries = append(entries, e)
	}

	// If there are no cache entries, return the empty endpoints
	if len(entries) == 0 {
		return candidates
	}

	// Sort the cache entries by timestamp in descending order.
	// This will prioritize the oldest endpoints first.
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].RTT == 0 && entries[i].Timestamp.Before(entries[j].Timestamp)
	})

	// Fill the rest of the candidates with the oldest entries
	for _, e := range entries {
		endpoint := e.Endpoint
		candidates = append(candidates, endpoint)
		if len(candidates) >= 16 {
			break
		}
	}

	/*
		// Clean up the cache by removing expired entries
		for id, e := range cache.Store {
			if time.Since(e.Timestamp) > LatencyCacheExpiry {
				delete(cache.Store, id)
			}
		}
	*/

	return candidates
}

func (ms *MatchmakingSession) LeavePartyGroup() error {
	s := ms.Session

	_, partyID, err := GetPartyGroupID(ms.Ctx, s.pipeline.db, s.UserID().String())
	if err != nil {
		return status.Errorf(codes.Internal, "Failed to get party group ID: %v", err)
	}
	if partyID == uuid.Nil {
		return nil
	}
	ms.Session.pipeline.tracker.Untrack(s.ID(), PresenceStream{Mode: StreamModeParty, Subject: partyID, Label: ms.registry.evrPipeline.node}, s.UserID())
	ms.Party = nil
	return nil
}

func (ms *MatchmakingSession) JoinPartyGroup(ctx context.Context, logger *zap.Logger) error {
	session := ms.Session
	groupName, partyID, err := GetPartyGroupID(ctx, session.pipeline.db, session.UserID().String())
	if err != nil {
		return status.Errorf(codes.Internal, "Failed to get party group ID: %v", err)
	}
	if partyID == uuid.Nil {
		return nil
	}

	maxSize := 4
	open := true

	userPresence := &rtapi.UserPresence{
		UserId:    session.UserID().String(),
		SessionId: session.ID().String(),
		Username:  session.Username(),
	}

	presence := Presence{
		ID: PresenceID{
			Node:      session.pipeline.node,
			SessionID: session.ID(),
		},
		// Presence stream not needed.
		UserID: session.UserID(),
		Meta: PresenceMeta{
			Username: session.Username(),
			// Other meta fields not needed.
		},
	}
	presenceMeta := PresenceMeta{
		Format:   session.Format(),
		Username: session.Username(),
		Status:   "",
	}

	partyRegistry := session.pipeline.partyRegistry.(*LocalPartyRegistry)
	// Check if the party already exists
	ph, ok := partyRegistry.parties.Load(partyID)
	if !ok {

		ph = NewPartyHandler(partyRegistry.logger, partyRegistry, partyRegistry.matchmaker, partyRegistry.tracker, partyRegistry.streamManager, partyRegistry.router, partyID, partyRegistry.node, open, maxSize, userPresence)
		partyRegistry.parties.Store(partyID, ph)
	}

	success, err := partyRegistry.PartyJoinRequest(session.Context(), partyID, session.pipeline.node, &presence)
	session.logger.Debug("Join party request", zap.String("partyID", partyID.String()), zap.String("sessionID", session.ID().String()), zap.String("userID", session.UserID().String()), zap.Bool("success", success), zap.Error(err))
	switch err {
	case nil:

	case runtime.ErrPartyFull, runtime.ErrPartyJoinRequestsFull:
		return status.Errorf(codes.ResourceExhausted, "Party is full")
	case runtime.ErrPartyJoinRequestDuplicate:
		return status.Errorf(codes.AlreadyExists, "Duplicate join request")
	case runtime.ErrPartyJoinRequestAlreadyMember:
		session.logger.Debug("User is already a member of the party")
		// This is not an error, just a no-op.
	}

	// If successful, the creator becomes the first user to join the party.
	if success, isNew := session.pipeline.tracker.Track(session.Context(), session.ID(), ph.Stream, session.UserID(), presenceMeta); !success {
		_ = session.Send(&rtapi.Envelope{Message: &rtapi.Envelope_Error{Error: &rtapi.Error{
			Code:    int32(rtapi.Error_RUNTIME_EXCEPTION),
			Message: "Error tracking party creation",
		}}}, true)
		return status.Errorf(codes.Internal, "Failed to track party creation")
	} else if isNew {
		out := &rtapi.Envelope{Message: &rtapi.Envelope_Party{Party: &rtapi.Party{
			PartyId:   ph.IDStr,
			Open:      open,
			MaxSize:   int32(maxSize),
			Self:      userPresence,
			Leader:    userPresence,
			Presences: []*rtapi.UserPresence{userPresence},
		}}}
		_ = session.Send(out, true)
	}

	ms.Party = &PartyGroup{
		name: groupName,
		ph:   ph,
	}
	partyMembers := make([]string, 0)
	for _, p := range ms.Party.List() {
		partyMembers = append(partyMembers, p.UserPresence.GetUserId())
	}

	logger.Debug("Joined party", zap.String("party_id", partyID.String()), zap.String("party_group", groupName), zap.Any("members", partyMembers))

	return nil
}

// GetCache returns the latency cache for a user
func (r *MatchmakingRegistry) GetCache(userId uuid.UUID) *LatencyCache {
	cache, _ := r.cacheByUserId.LoadOrStore(userId, &LatencyCache{})
	return cache
}

// listMatches returns a list of matches
func (c *MatchmakingRegistry) listMatches(ctx context.Context, limit int, minSize, maxSize int, query string) ([]*api.Match, error) {
	authoritativeWrapper := &wrapperspb.BoolValue{Value: true}
	var labelWrapper *wrapperspb.StringValue
	var queryWrapper *wrapperspb.StringValue
	if query != "" {
		queryWrapper = &wrapperspb.StringValue{Value: query}
	}
	minSizeWrapper := &wrapperspb.Int32Value{Value: int32(minSize)}

	maxSizeWrapper := &wrapperspb.Int32Value{Value: int32(maxSize)}

	matches, _, err := c.matchRegistry.ListMatches(ctx, limit, authoritativeWrapper, labelWrapper, minSizeWrapper, maxSizeWrapper, queryWrapper, nil)
	return matches, err
}

type LatencyCacheStorageObject struct {
	Entries map[string]LatencyMetric `json:"entries"`
}

// LoadLatencyCache loads the latency cache for a user
func (c *MatchmakingRegistry) LoadLatencyCache(ctx context.Context, logger *zap.Logger, session *sessionWS, msession *MatchmakingSession) (*LatencyCache, error) {
	// Load the latency cache
	// retrieve the document from storage
	userId := session.UserID()
	// Get teh user's latency cache
	cache := c.GetCache(userId)
	result, err := StorageReadObjects(ctx, logger, session.pipeline.db, uuid.Nil, []*api.ReadStorageObjectId{
		{
			Collection: MatchmakingStorageCollection,
			Key:        LatencyCacheStorageKey,
			UserId:     userId.String(),
		},
	})
	if err != nil {
		logger.Error("failed to read objects", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "Failed to read latency cache: %v", err)
	}

	objs := result.Objects
	if len(objs) != 0 {
		store := &LatencyCacheStorageObject{}
		if err := json.Unmarshal([]byte(objs[0].Value), store); err != nil {
			return nil, status.Errorf(codes.Internal, "Failed to unmarshal latency cache: %v", err)
		}
		// Load the entries into the cache
		for k, v := range store.Entries {
			cache.Store(k, v)

		}
	}

	return cache, nil
}

// StoreLatencyCache stores the latency cache for a user
func (c *MatchmakingRegistry) StoreLatencyCache(session *sessionWS) {
	cache := c.GetCache(session.UserID())

	if cache == nil {
		return
	}

	store := &LatencyCacheStorageObject{
		Entries: make(map[string]LatencyMetric, 100),
	}

	cache.Range(func(k string, v LatencyMetric) bool {
		store.Entries[k] = v
		return true
	})

	// Save the latency cache
	jsonBytes, err := json.Marshal(store)
	if err != nil {
		session.logger.Error("Failed to marshal latency cache", zap.Error(err))
		return
	}
	data := string(jsonBytes)
	ops := StorageOpWrites{
		{
			OwnerID: session.UserID().String(),
			Object: &api.WriteStorageObject{
				Collection:      MatchmakingStorageCollection,
				Key:             LatencyCacheStorageKey,
				Value:           data,
				PermissionRead:  &wrapperspb.Int32Value{Value: int32(0)},
				PermissionWrite: &wrapperspb.Int32Value{Value: int32(0)},
			},
		},
	}
	if _, _, err = StorageWriteObjects(context.Background(), session.logger, session.pipeline.db, session.metrics, session.storageIndex, true, ops); err != nil {
		session.logger.Error("Failed to save latency cache", zap.Error(err))
	}
}

// Stop stops the matchmaking registry
func (c *MatchmakingRegistry) Stop() {
	c.ctxCancelFn()
}

// GetMatchingBySessionId returns the matching session for a given session ID
func (c *MatchmakingRegistry) GetMatchingBySessionId(sessionId uuid.UUID) (session *MatchmakingSession, ok bool) {
	session, ok = c.matchingBySession.Load(sessionId)
	return session, ok
}

// Delete removes a matching session from the registry
func (c *MatchmakingRegistry) Delete(sessionId uuid.UUID) {
	c.matchingBySession.Delete(sessionId)
}

// Add adds a matching session to the registry
func (c *MatchmakingRegistry) Create(ctx context.Context, logger *zap.Logger, session *sessionWS, ml *MatchLabel, timeout time.Duration) (*MatchmakingSession, error) {
	// Check if there is an existing session
	if _, ok := c.GetMatchingBySessionId(session.ID()); ok {
		// Cancel it
		c.Cancel(session.ID(), ErrMatchmakingCanceledByPlayer)
	}

	// Set defaults for the matching label
	ml.Open = true // Open for joining
	ml.MaxSize = SocialLobbyMaxSize
	if l, ok := LobbySizeByMode[ml.Mode]; ok {
		ml.MaxSize = uint8(l)
	}

	// Set defaults for public matches
	switch {
	case ml.Mode == evr.ModeSocialPrivate || ml.Mode == evr.ModeSocialPublic:
		ml.Level = evr.LevelSocial // Include the level in the search

	case ml.Mode == evr.ModeArenaPublic:
		ml.TeamSize = 4

	case ml.Mode == evr.ModeCombatPublic:
		ml.TeamSize = 5

	default: // Privates
		ml.TeamSize = 5
	}

	profile, err := c.evrPipeline.profileRegistry.Load(ctx, session.userID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to load user profile: %v", err)
	}
	rating := profile.GetRating()

	ctx = context.WithValue(ctx, ctxRatingKey{}, rating)

	logger = logger.With(zap.String("msid", session.ID().String()))
	ctx, cancel := context.WithCancelCause(ctx)
	msession := &MatchmakingSession{
		Ctx:         ctx,
		CtxCancelFn: cancel,

		Logger:        logger,
		nk:            c.nk,
		registry:      c,
		UserID:        session.UserID(),
		PingResultsCh: make(chan []evr.EndpointPingResult),
		Expiry:        time.Now().UTC().Add(findAttemptsExpiry),
		Label:         ml,
		Tickets:       make(map[string]*MatchmakerTicket),
		Session:       session,
	}

	// Load the latency cache
	cache, err := c.LoadLatencyCache(ctx, logger, session, msession)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to load latency cache: %v", err)
	}
	msession.LatencyCache = cache

	// Start the matchmaking session
	go func() {
		// Create a timer for this session
		startedAt := time.Now().UTC()
		metricsTags := map[string]string{
			"mode":     ml.Mode.String(),
			"group_id": ml.GroupID.String(),
			"level":    ml.Level.String(),
			"team":     strconv.FormatInt(int64(ml.TeamIndex), 10),
			"result":   "success",
		}

		defer func() {
			c.metrics.CustomTimer("matchmaking_session_duration", metricsTags, time.Since(startedAt))
		}()

		defer cancel(nil)
		var err error
		select {
		case <-ctx.Done():
			if ctx.Err() != context.Cause(ctx) {
				err = context.Cause(ctx)
			} else {
				err = nil
			}

		case <-time.After(timeout):
			// Timeout
			err = ErrMatchmakingTimeout
		}
		switch err {
		case nil:
			metricsTags["result"] = "success"
		case ErrMatchmakingCanceledByPlayer, ErrMatchmakingCanceledByParty:
			metricsTags["result"] = "canceled"
		case ErrMatchmakingTimeout:
			metricsTags["result"] = "timeout"
			NewMatchmakingResult(logger, ml.Mode, *ml.GroupID).SendErrorToSession(session, err)
		default:
			metricsTags["result"] = "error"
			defer NewMatchmakingResult(logger, ml.Mode, *ml.GroupID).SendErrorToSession(session, err)
		}

		c.StoreLatencyCache(session)
		c.Delete(session.id)
		if err := session.matchmaker.RemoveSessionAll(session.id.String()); err != nil {
			logger.Error("Failed to remove session from matchmaker", zap.Error(err))
		}
	}()
	c.Add(session.id, msession)
	return msession, nil
}

func (c *MatchmakingRegistry) Cancel(sessionId uuid.UUID, reason error) {
	if session, ok := c.GetMatchingBySessionId(sessionId); ok {
		c.logger.Debug("Canceling matchmaking session", zap.String("reason", reason.Error()))
		session.Cancel(reason)
	}
}

func (c *MatchmakingRegistry) Add(id uuid.UUID, s *MatchmakingSession) {
	c.matchingBySession.Store(id, s)

}

func (r *MatchmakingRegistry) GetActiveGameServerEndpoints() []evr.Endpoint {
	endpoints := make([]evr.Endpoint, 0, 100)
	r.evrPipeline.broadcasterRegistrationBySession.Range(func(_ string, b *MatchBroadcaster) bool {
		endpoints = append(endpoints, b.Endpoint)
		return true
	})
	return endpoints
}

// GetLatencies returns the cached latencies for a user
func (c *MatchmakingRegistry) GetLatencies(userId uuid.UUID, endpoints []evr.Endpoint) []LatencyMetric {

	cache := c.GetCache(userId)

	if cache == nil {
		return nil
	}

	result := make([]LatencyMetric, 0, len(endpoints))

	if len(endpoints) == 0 {

		// Return all the latencies
		cache.Range(func(k string, v LatencyMetric) bool {
			result = append(result, v)
			return true
		})

	} else {

		// Get the latencies for the endpoints
		for _, e := range endpoints {
			id := e.GetExternalIP()
			r, ok := cache.Load(id)
			if !ok {
				// Create the endpoint and add it to the cache
				r = LatencyMetric{
					Endpoint:  e,
					RTT:       0,
					Timestamp: time.Now(),
				}
				cache.Store(id, r)
			}
			result = append(result, r)
		}
	}
	return result
}

func (*MatchmakingSession) BuildQuery(ctx context.Context, nk runtime.NakamaModule, db *sql.DB, userID string, evrID string, groupID string, mode evr.Symbol, latencies []LatencyMetric, partyGroup *PartyGroup) (query string, stringProps map[string]string, numericProps map[string]float64, err error) {
	// Create the properties maps
	stringProps = make(map[string]string)
	numericProps = make(map[string]float64, len(latencies))
	qparts := make([]string, 0, 10)

	stringProps["evrid"] = evrID
	// Add this user's ID to the string props
	stringProps["uid"] = userID

	// Add a property of the external IP's RTT
	for _, b := range latencies {
		if b.RTT == 0 {
			continue
		}

		msecs := mroundRTT(b.RTT, 10).Milliseconds()
		// Turn the IP into a hex string like 127.0.0.1 -> 7f000001
		ip := ipToKey(b.Endpoint.ExternalIP)
		// Add the property
		numericProps[ip] = float64(msecs)

		// TODO FIXME Add second matchmaking ticket for over 150ms
		n := 0
		switch {
		case msecs < 25:
			n = 10
		case msecs < 40:
			n = 7
		case msecs < 60:
			n = 5
		case msecs < 80:
			n = 2
		}
		// Add a score for each endpoint
		qparts = append(qparts, fmt.Sprintf("properties.%s:<=%d^%d", ip, msecs+15, n))
	}
	// MUST be the same mode
	qparts = append(qparts, "+properties.mode:"+mode.String())
	stringProps["mode"] = mode.String()

	_, groupMetadata, err := GetGuildGroupMetadata(ctx, nk, groupID)
	if err != nil {
		return "", nil, nil, err
	}

	groupIDs, err := GetGuildGroupIDsByUser(ctx, db, userID)
	if err != nil {
		return "", nil, nil, err
	}
	/*
		allGroupIDs := make([]string, 0)
			if partyGroup != nil {
				// Create a list of groups that all party members have in common
				userGroups := make([][]string, 0)

				for _, p := range partyGroup.List() {
					groupMap, err := GetGuildGroupIDsByUser(ctx, db, p.Presence.GetUserId())
					if err != nil {
						return "", nil, nil, err
					}
					groupIDs := make([]string, 0, len(groupMap))
					for _, g := range groupMap {
						groupIDs = append(groupIDs, g)
					}

					userGroups = append(userGroups, groupIDs)
				}

				for _, g := range userGroups[0] {
					found := true
					for _, u := range userGroups {
						if !slices.Contains(u, g) {
							found = false
							break
						}
					}
					if found {
						allGroupIDs = append(allGroupIDs, g)
					}
				}
			} else {
				groupMap, err := GetGuildGroupIDsByUser(ctx, db, userID)
				if err != nil {
					return "", nil, nil, err
				}
				for _, g := range groupMap {
					allGroupIDs = append(allGroupIDs, g)
				}

			}
	*/
	for _, id := range groupIDs {
		// Add the properties
		// Strip out the hyphens from the group ID
		s := strings.ReplaceAll(id, "-", "")
		stringProps[s] = "T"

		qparts = append(qparts, fmt.Sprintf("properties.group_id:%s", groupID))
		stringProps["group_id"] = groupID

		// If this is the user's current channel, then give it a +3 boost
		if id == groupID {
			if groupMetadata.MembersOnlyMatchmaking {
				qparts = append(qparts, fmt.Sprintf("+properties.%s:T", s))
			} else {
				qparts = append(qparts, fmt.Sprintf("properties.%s:T^10", s))
			}
		} else {
			qparts = append(qparts, fmt.Sprintf("properties.%s:T", s))
		}
	}

	query = strings.Join(qparts, " ")
	// TODO Promote friends
	// TODO Avoid ghosted
	return query, stringProps, numericProps, nil
}

func (c *MatchmakingRegistry) SessionsByMode() map[evr.Symbol]map[uuid.UUID]*MatchmakingSession {

	sessionByMode := make(map[evr.Symbol]map[uuid.UUID]*MatchmakingSession)

	c.matchingBySession.Range(func(sid uuid.UUID, ms *MatchmakingSession) bool {
		if sessionByMode[ms.Label.Mode] == nil {
			sessionByMode[ms.Label.Mode] = make(map[uuid.UUID]*MatchmakingSession)
		}

		sessionByMode[ms.Label.Mode][sid] = ms
		return true
	})
	return sessionByMode
}

func (r *MatchmakingRegistry) SendMatchmakerMatchedNotification(label *MatchLabel, teams [][]runtime.MatchmakerEntry, errored []*EvrMatchPresence) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	logger := r.logger

	bot := r.evrPipeline.discordRegistry.GetBot()
	if bot == nil {
		return
	}

	groupID := label.GetGroupID()
	if groupID == uuid.Nil {
		return
	}

	_, groupMetadata, err := GetGuildGroupMetadata(ctx, r.nk, groupID.String())
	if err != nil {
		r.logger.Warn("Failed to get guild group metadata", zap.Error(err))
		return
	}

	channelID := ""
	switch label.Mode {
	case evr.ModeCombatPublic:
		channelID = groupMetadata.CombatMatchmakingChannelID
	case evr.ModeArenaPublic:
		channelID = groupMetadata.ArenaMatchmakingChannelID
	}
	if channelID == "" {
		return
	}

	teamstrs := make([]string, 0, len(teams))
	for _, team := range teams {
		names := make([]string, 0, len(team))
		for _, p := range team {
			account, err := r.nk.AccountGetId(ctx, p.GetPresence().GetUserId())
			if err != nil {
				logger.Warn("Failed to get account", zap.Error(err))
				continue
			}
			names = append(names, escapeDiscordString(account.GetUser().GetDisplayName()))
		}
		teamstrs = append(teamstrs, strings.Join(names, ", "))
	}

	msg := fmt.Sprintf("Match made:\n%s vs %s", teamstrs[0], teamstrs[1])

	embed := discordgo.MessageEmbed{
		Title:       "Matchmaking",
		Description: msg,
		Color:       0x00cc00,
	}

	// Notify the channel that this person started queuing
	message, err := bot.ChannelMessageSendEmbed(channelID, &embed)
	if err != nil {
		logger.Warn("Failed to send message", zap.Error(err))
	}
	go func() {
		// Delete the message when the player stops matchmaking
		select {
		case <-time.After(15 * time.Minute):
			if message != nil {
				err := bot.ChannelMessageDelete(channelID, message.ID)
				if err != nil {
					logger.Warn("Failed to delete message", zap.Error(err))
				}
			}
		}
	}()
}
