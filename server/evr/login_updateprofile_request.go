package evr

import (
	"fmt"

	"github.com/gofrs/uuid/v5"
)

type UpdateClientProfile struct {
	Session       uuid.UUID
	EvrId         XPID
	ClientProfile ClientProfile
}

func (lr *UpdateClientProfile) String() string {
	return fmt.Sprintf("%T(session=%s, xp_id=%s)", lr, lr.Session.String(), lr.EvrId.String())
}

func (m *UpdateClientProfile) Stream(s *EasyStream) error {
	return RunErrorFunctions([]func() error{
		func() error { return s.StreamGUID(&m.Session) },
		func() error { return s.StreamStruct(&m.EvrId) },
		func() error { return s.StreamJson(&m.ClientProfile, true, NoCompression) },
	})
}
