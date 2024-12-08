package server

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
	"github.com/heroiclabs/nakama/v3/server/evr"
	"go.uber.org/zap"
)

type TeamAlignments map[string]int // map[UserID]Role

var createSocialMu = sync.Mutex{}

var LobbyTestCounter = 0

var ErrCreateLock = errors.New("failed to acquire create lock")

// lobbyJoinSessionRequest is a request to join a specific existing session.
func (p *EvrPipeline) lobbyFind(ctx context.Context, logger *zap.Logger, session *sessionWS, lobbyParams *LobbySessionParameters) error {

	startTime := time.Now()

	// Do authorization checks related to the guild.
	if err := p.authorizeGuildGroupSession(ctx, session, lobbyParams.GroupID.String()); err != nil {
		return err
	}

	// Restrict matchmaking to public lobbies only
	switch lobbyParams.Mode {
	case evr.ModeArenaPublic, evr.ModeSocialPublic, evr.ModeCombatPublic:
	default:
		return NewLobbyError(BadRequest, fmt.Sprintf("`%s` is an invalid mode for matchmaking.", lobbyParams.Mode.String()))
	}

	// Cancel matchmaking after the timeout.
	ctx, cancel := context.WithTimeoutCause(ctx, lobbyParams.MatchmakingTimeout, ErrMatchmakingTimeout)
	defer cancel()

	// Join the "matchmaking" status stream
	if err := JoinMatchmakingStream(logger, session, lobbyParams); err != nil {
		return fmt.Errorf("failed to join matchmaking stream: %w", err)
	}

	// Monitor the matchmaking status stream, canceling the context if the stream is closed.
	go p.monitorMatchmakingStream(ctx, logger, session, lobbyParams, cancel)

	entrantSessionIDs := []uuid.UUID{session.id}

	var lobbyGroup *LobbyGroup

	if lobbyParams.PartyGroupName != "" {
		var err error
		var isLeader bool
		var memberSessionIDs []uuid.UUID
		lobbyGroup, memberSessionIDs, isLeader, err = p.configureParty(ctx, logger, session, lobbyParams)
		if err != nil {
			return fmt.Errorf("failed to join party: %w", err)
		}

		if !isLeader {
			// Skip following the party leader if the member is not in a match (and going to a social lobby)
			if lobbyParams.Mode != evr.ModeSocialPublic || !lobbyParams.CurrentMatchID.IsNil() {
				return p.PartyFollow(ctx, logger, session, lobbyParams, lobbyGroup)
			}
		} else {

			for _, memberSessionIDs := range memberSessionIDs {

				if memberSessionIDs == session.id {
					continue
				}

				entrantSessionIDs = append(entrantSessionIDs, memberSessionIDs)
			}
		}
	}

	p.metrics.CustomCounter("lobby_find_match", lobbyParams.MetricsTags(), int64(lobbyParams.GetPartySize()))
	logger.Info("Finding match", zap.String("mode", lobbyParams.Mode.String()), zap.Int("party_size", lobbyParams.GetPartySize()))

	defer func() {
		tags := lobbyParams.MetricsTags()
		tags["party_size"] = strconv.Itoa(lobbyParams.GetPartySize())
		p.metrics.CustomTimer("lobby_find_duration", tags, time.Since(startTime))
		logger.Debug("Lobby find complete", zap.String("group_id", lobbyParams.GroupID.String()), zap.Int("party_size", lobbyParams.GetPartySize()), zap.String("mode", lobbyParams.Mode.String()), zap.Int("duration", int(time.Since(startTime).Seconds())))
	}()

	// Construct the entrant presences for the party members.
	entrants, err := PrepareEntrantPresences(ctx, logger, session, lobbyParams, entrantSessionIDs...)
	if err != nil {
		return fmt.Errorf("failed to be party leader.: %w", err)
	}

	// Ping for matches, not social lobbies.
	if lobbyParams.Mode != evr.ModeSocialPublic {
		// Check latency to active game servers.
		if err := p.CheckServerPing(ctx, logger, session); err != nil {
			return fmt.Errorf("failed to check server ping: %w", err)
		}

		// Submit the matchmaking ticket
		if err := p.lobbyMatchMakeWithFallback(ctx, logger, session, lobbyParams, lobbyGroup); err != nil {
			return fmt.Errorf("failed to matchmake: %w", err)
		}
	}

	// Attempt to backfill until the timeout.
	return p.lobbyBackfill(ctx, logger, lobbyParams, entrants...)

}

func (p *EvrPipeline) configureParty(ctx context.Context, logger *zap.Logger, session *sessionWS, lobbyParams *LobbySessionParameters) (*LobbyGroup, []uuid.UUID, bool, error) {

	// Join the party if a player has a party group id set.
	// The lobby group is the party that the user is currently in.
	lobbyGroup, isLeader, err := JoinPartyGroup(session, lobbyParams.PartyGroupName, lobbyParams.PartyID, lobbyParams.CurrentMatchID)
	if err != nil {
		return nil, nil, false, fmt.Errorf("failed to join party group: %w", err)
	}
	logger.Debug("Joined party group", zap.String("partyID", lobbyGroup.IDStr()))

	// If this is the leader, then set the presence status to the current match ID.
	if isLeader {

		// If there are more than one player in the party, wait for the other players to start matchmaking.
		if lobbyGroup.Size() > 1 {
			select {
			case <-ctx.Done():
				return nil, nil, false, ctx.Err()
			case <-time.After(10 * time.Second):
			}
		}
		stream := lobbyParams.MatchmakingStream()

		memberUsernames := make([]string, 0, lobbyGroup.Size())

		for _, member := range lobbyGroup.List() {
			if member.Presence.GetSessionId() == session.id.String() {
				continue
			}
			memberUsernames = append(memberUsernames, member.Presence.GetUsername())

			meta, err := p.runtimeModule.StreamUserGet(stream.Mode, stream.Subject.String(), stream.Subcontext.String(), stream.Label, member.Presence.GetUserId(), member.Presence.GetSessionId())
			if err != nil {
				return nil, nil, false, fmt.Errorf("failed to get party stream: %w", err)
			} else if meta == nil {
				logger.Warn("Party member is not following the leader", zap.String("uid", member.Presence.GetUserId()), zap.String("sid", member.Presence.GetSessionId()), zap.String("leader_sid", session.id.String()))
				if err := p.runtimeModule.StreamUserKick(stream.Mode, stream.Subject.String(), stream.Subcontext.String(), stream.Label, member.Presence); err != nil {
					return nil, nil, false, fmt.Errorf("failed to kick party member: %w", err)
				}
			}
		}

		partySize := lobbyGroup.Size()
		logger.Debug("Party is ready", zap.String("leader", session.id.String()), zap.Int("size", partySize), zap.Strings("members", memberUsernames))

		lobbyParams.SetPartySize(partySize)

	}

	memberSessionIDs := []uuid.UUID{session.id}
	// Add the party members to the sessionID slice
	for _, member := range lobbyGroup.List() {
		if member.Presence.GetSessionId() == session.id.String() {
			continue
		}
		memberSessionIDs = append(memberSessionIDs, uuid.FromStringOrNil(member.Presence.GetSessionId()))
	}

	return lobbyGroup, memberSessionIDs, isLeader, nil
}

func (p *EvrPipeline) monitorMatchmakingStream(ctx context.Context, logger *zap.Logger, session *sessionWS, lobbyParams *LobbySessionParameters, cancelFn context.CancelFunc) {

	// Monitor the stream and cancel the context (and matchmaking) if the stream is closed.
	// This stream tracks the user's matchmaking status.
	// This stream is untracked when the user cancels matchmaking.

	stream := lobbyParams.MatchmakingStream()
	defer LeaveMatchmakingStream(logger, session)
	for {
		select {
		case <-ctx.Done():
			// Check if the cancel was because of a timeout
			return
		case <-time.After(1 * time.Second):
		}

		// Check if the matchmaking stream has been closed.  (i.e. the user has canceled matchmaking)
		if session.tracker.GetLocalBySessionIDStreamUserID(session.id, stream, session.userID) == nil {
			cancelFn()
		}
	}
}

func (p *EvrPipeline) newSocialLobby(ctx context.Context, logger *zap.Logger, versionLock evr.Symbol, groupID uuid.UUID) (*MatchLabel, error) {
	if createSocialMu.TryLock() {
		go func() {
			// Hold the lock for enough time to create the server
			<-time.After(5 * time.Second)
			createSocialMu.Unlock()
		}()
	} else {
		return nil, ErrFailedToAcquireLock
	}

	metricsTags := map[string]string{
		"version_lock": versionLock.String(),
		"group_id":     groupID.String(),
	}

	p.metrics.CustomCounter("lobby_create_social", metricsTags, 1)

	qparts := []string{
		"+label.open:T",
		"+label.lobby_type:unassigned",
		"+label.broadcaster.regions:/(default)/",
		fmt.Sprintf("+label.broadcaster.group_ids:/(%s)/", Query.Escape(groupID.String())),
		//fmt.Sprintf("+label.broadcaster.version_lock:%s", versionLock.String()),
	}

	query := strings.Join(qparts, " ")

	labels, err := lobbyListGameServers(ctx, p.runtimeModule, query)
	if err != nil {
		logger.Warn("Failed to list game servers", zap.Any("query", query), zap.Error(err))
		return nil, err
	}

	if len(labels) == 0 {
		return nil, NewLobbyError(ServerFindFailed, "failed to find server")
	}

	// Retrieve the latency history of all online public players.
	// Identify servers where the majority of players have a ping other than 999 (or 0).
	// Sort servers by those with a ping less than 250 for all players.
	// Select the server with the best average ping for the highest number of players.
	label := &MatchLabel{}

	rttByPlayerByExtIP, err := rttByPlayerByExtIP(ctx, logger, p.db, p.runtimeModule, groupID.String())
	if err != nil {
		logger.Warn("Failed to get RTT by player by extIP", zap.Error(err))
	} else {
		extIPs := sortByGreatestPlayerAvailability(rttByPlayerByExtIP)
		for _, extIP := range extIPs {
			for _, l := range labels {
				if l.Broadcaster.Endpoint.GetExternalIP() == extIP {
					label = l
					break
				}
			}
		}
	}

	// If no label was found, just pick a random one
	if label.ID.IsNil() {
		label = labels[rand.Intn(len(labels))]
	}

	matchID := label.ID
	settings := &MatchSettings{
		Mode:      evr.ModeSocialPublic,
		Level:     evr.LevelSocial,
		SpawnedBy: SystemUserID,
		GroupID:   groupID,
		StartTime: time.Now().UTC(),
	}
	label, err = LobbyPrepareSession(ctx, p.runtimeModule, matchID, settings)
	if err != nil {
		logger.Error("Failed to prepare session", zap.Error(err), zap.String("mid", matchID.String()))
		return nil, err
	}

	return label, nil
}

func (p *EvrPipeline) lobbyBackfill(ctx context.Context, logger *zap.Logger, lobbyParams *LobbySessionParameters, entrants ...*EvrMatchPresence) error {

	// Default backfill interval
	interval := 15 * time.Second

	// Early quitters have a shorter backfill interval.
	if lobbyParams.IsEarlyQuitter {
		interval = 3 * time.Second
	}

	if lobbyParams.Mode == evr.ModeSocialPublic {
		interval = 1 * time.Second
	}

	// If the player has backfill disabled, set the backfill interval to an extreme number.
	if lobbyParams.DisableArenaBackfill && lobbyParams.Mode == evr.ModeArenaPublic {
		// Set a long backfill interval for arena matches.
		interval = 15 * time.Minute
	}

	// Backfill search query
	// Maximum RTT for a server to be considered for backfill

	includeRankPercentile := false
	includeMaxRTT := false

	// Only use rank percentile for arena matches.
	if lobbyParams.Mode == evr.ModeArenaPublic {
		includeRankPercentile = true
		includeMaxRTT = true
	}

	query := lobbyParams.BackfillSearchQuery(includeRankPercentile, includeMaxRTT)

	rtts := lobbyParams.latencyHistory.LatestRTTs()

	cycleCount := 0
	backfillMultipler := 1.5

	fallbackTimer := time.NewTimer(time.Duration(backfillMultipler * float64(p.matchmakingTicketTimeout())))

	for {

		var err error
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-fallbackTimer.C:
			query = lobbyParams.BackfillSearchQuery(false, false)
		case <-time.After(interval):
		}

		// List all matches that are open and have available slots.
		matches, err := ListMatchStates(ctx, p.runtimeModule, query)
		if err != nil {
			return fmt.Errorf("failed to list matches: %w", err)
		}

		cycleCount++
		if len(matches) > 0 {
			logger.Debug("Found matches", zap.Int("count", len(matches)), zap.Any("query", query), zap.Int("cycle", cycleCount))
		} else {
			if cycleCount%10 == 0 {
				logger.Debug("No matches found", zap.Any("query", query), zap.Int("cycle", cycleCount))
			}
		}

		// Sort the matches by open slots and then by latency
		slices.SortFunc(matches, func(a, b *MatchLabelMeta) int {

			// By rank percentile difference
			rankDeltaA := math.Abs(a.State.RankPercentile - lobbyParams.RankPercentile)
			rankDeltaB := math.Abs(b.State.RankPercentile - lobbyParams.RankPercentile)

			// if the rank delta is more than 0.15, sort by rank delta
			if rankDeltaA > 0.15 && rankDeltaB > 0.15 {
				if rankDeltaA < rankDeltaB {
					return -1
				}

				if rankDeltaA > rankDeltaB {
					return 1
				}
			}

			// Sort by largest population
			if s := b.State.PlayerCount - a.State.PlayerCount; s != 0 {
				return s
			}

			if s := b.State.PlayerCount - a.State.PlayerCount; s != 0 {
				return s
			}

			// If the open slots are the same, sort by latency
			return rtts[a.State.Broadcaster.Endpoint.GetExternalIP()] - rtts[b.State.Broadcaster.Endpoint.GetExternalIP()]
		})

		var selected *MatchLabel

		team := evr.TeamBlue

		for _, labelMeta := range matches {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}

			l := labelMeta.State

			// if the match is too new, skip it. (except social lobbies)
			if lobbyParams.Mode != evr.ModeSocialPublic && time.Since(l.CreatedAt) < 10*time.Second {
				continue
			}

			// Check if the match is full
			if l.OpenPlayerSlots() < len(entrants) {
				continue
			}

			// Social lobbies can only have one team
			if lobbyParams.Mode == evr.ModeSocialPublic {
				team = evr.TeamSocial

			} else {

				// Determine which team has the least players
				team = evr.TeamBlue
				if l.RoleCount(evr.TeamOrange) < l.RoleCount(evr.TeamBlue) {
					team = evr.TeamOrange
				}
			}

			// Only join the player to a team that has similar rank

			if n, err := l.OpenSlotsByRole(team); err != nil {
				logger.Warn("Failed to get open slots by role", zap.Error(err))
				continue
			} else if n < len(entrants) {
				continue
			}

			// Match found
			selected = l
			break
		}

		// If the lobby is social, create a new social lobby.
		if selected == nil && lobbyParams.Mode == evr.ModeSocialPublic {
			// Create a new social lobby
			selected, err = p.newSocialLobby(ctx, logger, lobbyParams.VersionLock, lobbyParams.GroupID)
			if err != nil {
				// If the error is a lock error, just try again.
				if err == ErrFailedToAcquireLock {
					// Wait a few seconds to give time for the server to be created.
					<-time.After(2 * time.Second)
					continue
				}

				// This should't happen unless there's no servers available.
				return NewLobbyErrorf(ServerFindFailed, "failed to find social lobby: %w", err)
			} else {
				<-time.After(1 * time.Second)
			}

		}

		// If no match was found, continue.
		if selected == nil {
			continue
		}

		// Set the role alignment for each entrant in the party
		for _, e := range entrants {
			e.RoleAlignment = team
		}

		logger := logger.With(zap.String("mid", selected.ID.UUID.String()))

		logger.Debug("Joining backfill match.")
		p.metrics.CustomCounter("lobby_join_backfill", lobbyParams.MetricsTags(), int64(lobbyParams.GetPartySize()))

		// Player members will detect the join.
		if err := p.LobbyJoinEntrants(logger, selected, entrants...); err != nil {
			// Send the error to the client
			// If it's full just try again.
			if LobbyErrorCode(err) == ServerIsFull {
				logger.Warn("Server is full, ignoring.")
				continue
			}
			return fmt.Errorf("failed to join backfill match: %w", err)
		}
		return nil
	}
}

func (p *EvrPipeline) CheckServerPing(ctx context.Context, logger *zap.Logger, session *sessionWS) error {
	// Check latency to active game servers.
	doneCh := make(chan error)

	go func() {
		defer close(doneCh)

		// Wait for the client to be ready.
		<-time.After(1 * time.Second)

		activeEndpoints := make([]evr.Endpoint, 0, 100)
		p.broadcasterRegistrationBySession.Range(func(_ string, b *MatchBroadcaster) bool {
			activeEndpoints = append(activeEndpoints, b.Endpoint)
			return true
		})

		if err := PingGameServers(ctx, logger, session, p.db, activeEndpoints); err != nil {
			doneCh <- err
		}
		doneCh <- nil
	}()

	// Wait for the ping response to complete
	var err error
	select {
	case <-time.After(5 * time.Second):
		logger.Warn("Timed out waiting for ping responses message.")
	case err = <-doneCh:
	}
	return err
}

func PrepareEntrantPresences(ctx context.Context, logger *zap.Logger, session *sessionWS, params *LobbySessionParameters, sessionIDs ...uuid.UUID) ([]*EvrMatchPresence, error) {

	entrantPresences := make([]*EvrMatchPresence, 0, len(sessionIDs))
	for _, sessionID := range sessionIDs {
		session := session.sessionRegistry.Get(sessionID)
		if session == nil {
			logger.Warn("Session not found", zap.String("sid", sessionID.String()))
			continue
		}

		presence, err := EntrantPresenceFromLobbyParams(session, params)
		if err != nil {
			return nil, fmt.Errorf("failed to create entrant presences: %w", err)
		}

		entrantPresences = append(entrantPresences, presence)
	}

	if len(entrantPresences) == 0 {
		logger.Error("No entrants found. Cancelling matchmaking.")
		return nil, NewLobbyError(InternalError, "no entrants found")
	}

	return entrantPresences, nil
}

func (p *EvrPipeline) PartyFollow(ctx context.Context, logger *zap.Logger, session *sessionWS, params *LobbySessionParameters, lobbyGroup *LobbyGroup) error {

	logger.Debug("User is member of party", zap.String("leader", lobbyGroup.GetLeader().GetUsername()))

	// This is a party member, wait for the party leader to join a match, or cancel matchmaking.
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
			// Time for the party leader to join a match.
		}
		leader := lobbyGroup.GetLeader()
		if leader == nil {
			return NewLobbyError(BadRequest, "party leader not found")
		}

		leaderUserID := uuid.FromStringOrNil(leader.UserId)
		// Check if the leader has changed to this player.
		if leader.SessionId == session.id.String() {
			return NewLobbyError(BadRequest, "party leader has changed (to this player). Canceling matchmaking.")
		}

		leaderSessionID := uuid.FromStringOrNil(leader.SessionId)
		stream := PresenceStream{
			Mode:    StreamModeService,
			Subject: leaderSessionID,
			Label:   StreamLabelMatchService,
		}

		// Check if the leader is still matchmaking. If so, continue waiting.
		if p := session.pipeline.tracker.GetLocalBySessionIDStreamUserID(leaderSessionID, params.MatchmakingStream(), leaderUserID); p != nil {
			// Leader is still matchmaking.
			continue
		}

		// Check if the party leader is still in a lobby/match.
		presence := session.pipeline.tracker.GetLocalBySessionIDStreamUserID(leaderSessionID, stream, leaderUserID)
		if presence == nil {
			return NewLobbyError(BadRequest, fmt.Sprintf("party leader `%s` is no longer in a match.", leader.UserId))
		}

		// Check if the party leader is in a match.
		leaderMatchID := MatchIDFromStringOrNil(presence.GetStatus())
		if leaderMatchID.IsNil() {
			continue
		}

		// Wait 3 seconds, then check if this player is in the match as well (i.e. the matchmaker sent them to a match)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}

		stream = PresenceStream{
			Mode:    StreamModeService,
			Subject: session.id,
			Label:   StreamLabelMatchService,
		}
		memberMatchID := MatchID{}
		presence = session.pipeline.tracker.GetLocalBySessionIDStreamUserID(session.id, stream, session.userID)
		if presence != nil {
			memberMatchID = MatchIDFromStringOrNil(presence.GetStatus())
		}

		if memberMatchID == leaderMatchID {
			// The leader is in a match, and this player is in the same match.
			continue
		} else {
			// If the leader is in a different public lobby, try to join it.
			label, err := MatchLabelByID(ctx, p.runtimeModule, leaderMatchID)
			if err != nil {
				return fmt.Errorf("failed to get match by session id: %w", err)
			} else if label == nil {
				continue
			}

			if !label.Open || label.PlayerCount >= label.PlayerLimit {
				// The leader's match is full.
				continue
			}

			switch label.Mode {

			case evr.ModeSocialPrivate, evr.ModeSocialPublic, evr.ModeCombatPublic, evr.ModeArenaPublic:
				// Join the leader's match.
				logger.Debug("Joining leader's lobby", zap.String("mid", leaderMatchID.String()))

				if err := p.lobbyJoin(ctx, logger, session, params, leaderMatchID); err != nil {
					code := LobbyErrorCode(err)
					if code == ServerIsFull || code == ServerIsLocked {
						<-time.After(5 * time.Second)
						continue
					}
					return fmt.Errorf("failed to join leader's social lobby: %w", err)
				}
				return nil
			default:
				// The leader is in a non-public match.
			}
		}
		// The leader is in a match, but this player is not.
		return NewLobbyError(ServerIsLocked, "party leader is in a match")
	}

}

// Wrapper for the matchRegistry.ListMatches function.
func listMatches(ctx context.Context, nk runtime.NakamaModule, limit int, minSize int, maxSize int, query string) ([]*api.Match, error) {
	return nk.MatchList(ctx, limit, true, "", &minSize, &maxSize, query)
}
