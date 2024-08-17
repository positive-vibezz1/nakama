package server

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/rtapi"
	"github.com/heroiclabs/nakama-common/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type LobbyGroupMemberStatus struct {
	StartedAt time.Time             `json:"timestamp"`
	PartyID   uuid.UUID             `json:"party_id,omitempty"`
	Label     MatchmakingGroupLabel `json:"label,omitempty"`
}

func (s LobbyGroupMemberStatus) String() string {
	data, _ := json.Marshal(s)
	return string(data)
}

type LobbyGroup struct {
	sync.RWMutex
	name string
	ph   *PartyHandler
}

func (g *LobbyGroup) ID() uuid.UUID {
	if g.ph == nil {
		return uuid.Nil
	}
	return g.ph.ID
}

func (g *LobbyGroup) IDStr() string {
	if g.ph == nil {
		return uuid.Nil.String()
	}
	return g.ph.IDStr
}
func (g *LobbyGroup) GetLeader() *rtapi.UserPresence {
	g.ph.RLock()
	defer g.ph.RUnlock()
	if g.ph.leader == nil {
		return g.ph.expectedInitialLeader
	}
	return g.ph.leader.UserPresence
}

func (g *LobbyGroup) List() []*PartyPresenceListItem {
	if g.ph == nil {
		return nil
	}
	g.ph.Lock()
	defer g.ph.Unlock()
	return g.ph.members.List()
}

func (g *LobbyGroup) Size() int {
	if g.ph == nil {
		return 1
	}
	return g.ph.members.Size()
}

func (g *LobbyGroup) MatchmakerAdd(sessionID, node, query string, minCount, maxCount, countMultiple int, stringProperties map[string]string, numericProperties map[string]float64) (string, []*PresenceID, error) {
	return g.ph.MatchmakerAdd(sessionID, node, query, minCount, maxCount, countMultiple, stringProperties, numericProperties)
}

func JoinLobbyGroup(session *sessionWS) (*LobbyGroup, error) {
	ctx := session.Context()
	groupName, partyID, err := GetLobbyGroupID(ctx, session.pipeline.db, session.UserID().String())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to get party group ID: %v", err)
	}
	if partyID == uuid.Nil {
		partyID = uuid.NewV5(session.id, EntrantIDSalt)
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
	switch err {
	case nil, runtime.ErrPartyJoinRequestAlreadyMember:
		// No-op
	case runtime.ErrPartyFull, runtime.ErrPartyJoinRequestsFull:
		return nil, status.Errorf(codes.ResourceExhausted, "Party is full")
	case runtime.ErrPartyJoinRequestDuplicate:
		return nil, status.Errorf(codes.AlreadyExists, "Duplicate join request")
	}
	if !success {
		return nil, status.Errorf(codes.Internal, "Failed to join party")
	}
	// If successful, the creator becomes the first user to join the party.
	if success, isNew := session.pipeline.tracker.Track(session.Context(), session.ID(), ph.Stream, session.UserID(), presenceMeta); !success {
		_ = session.Send(&rtapi.Envelope{Message: &rtapi.Envelope_Error{Error: &rtapi.Error{
			Code:    int32(rtapi.Error_RUNTIME_EXCEPTION),
			Message: "Error tracking party creation",
		}}}, true)
		return nil, status.Errorf(codes.Internal, "Failed to track party creation")
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
	if ph == nil {
		return nil, status.Errorf(codes.Internal, "Failed to get party handler")
	}
	return &LobbyGroup{
		name: groupName,
		ph:   ph,
	}, nil
}

func LeaveLobbyGroup(s *sessionWS) error {
	ctx := s.Context()
	_, partyID, err := GetLobbyGroupID(ctx, s.pipeline.db, s.UserID().String())
	if err != nil {
		return status.Errorf(codes.Internal, "Failed to get party group ID: %v", err)
	}
	if partyID == uuid.Nil {
		return nil
	}
	s.pipeline.tracker.Untrack(s.ID(), PresenceStream{Mode: StreamModeParty, Subject: partyID, Label: s.pipeline.node}, s.UserID())
	return nil
}
