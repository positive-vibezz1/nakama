package evr

import (
	"fmt"

	"github.com/gofrs/uuid/v5"
)

type LoginSuccess struct {
	Session uuid.UUID
	EvrId   XPID
}

func NewLoginSuccess(session uuid.UUID, evrId XPID) *LoginSuccess {
	return &LoginSuccess{
		Session: session,
		EvrId:   evrId,
	}
}

func (m LoginSuccess) String() string {
	return fmt.Sprintf("%T(session=%v, user_id=%s)",
		m, m.Session, m.EvrId.String())
}

func (m *LoginSuccess) Stream(s *EasyStream) error {
	return RunErrorFunctions([]func() error{
		func() error { return s.StreamGUID(&m.Session) },
		func() error { return s.StreamStruct(&m.EvrId) },
	})
}

func (m *LoginSuccess) GetLoginSessionID() uuid.UUID {
	return m.Session
}

func (m *LoginSuccess) GetXPID() XPID {
	return m.EvrId
}
