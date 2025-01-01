package evr

import (
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/gofrs/uuid/v5"
	"github.com/samber/lo"
)

// LobbyPlayerSessionsRequest is a message from client to server, asking it to obtain game server sessions for a given list of user identifiers.
type LobbyPlayerSessionsRequest struct {
	LoginSessionID uuid.UUID
	EvrId          XPID
	LobbyID        uuid.UUID
	Platform       Symbol
	PlayerXPIDs    []XPID
}

func (m *LobbyPlayerSessionsRequest) Stream(s *EasyStream) error {
	playerCount := uint64(len(m.PlayerXPIDs))
	return RunErrorFunctions([]func() error{
		func() error { return s.StreamGUID(&m.LoginSessionID) },
		func() error { return s.StreamStruct(&m.EvrId) },
		func() error { return s.StreamGUID(&m.LobbyID) },
		func() error { return s.StreamNumber(binary.LittleEndian, &m.Platform) },
		func() error { return s.StreamNumber(binary.LittleEndian, &playerCount) },
		func() error {
			if s.Mode == DecodeMode {
				m.PlayerXPIDs = make([]XPID, playerCount)
			}
			for i := range m.PlayerXPIDs {
				if err := s.StreamStruct(&m.PlayerXPIDs[i]); err != nil {
					return err
				}
			}
			return nil
		},
	})
}

func (m *LobbyPlayerSessionsRequest) String() string {
	xpIDstrs := strings.Join(lo.Map(m.PlayerXPIDs, func(id XPID, i int) string { return id.Token() }), ", ")
	return fmt.Sprintf("%T(login_session_id=%s, xp_id=%s, lobby_id=%s, xp_ids=%s)", m, m.LoginSessionID, m.EvrId, m.LobbyID, xpIDstrs)
}

func (m *LobbyPlayerSessionsRequest) GetLoginSessionID() uuid.UUID {
	return m.LoginSessionID
}

func (m *LobbyPlayerSessionsRequest) GetXPID() XPID {
	return m.EvrId
}

func (m *LobbyPlayerSessionsRequest) LobbySessionID() uuid.UUID {
	return m.LobbyID
}
