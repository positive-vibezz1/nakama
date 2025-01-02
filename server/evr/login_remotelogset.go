package evr

import (
	"fmt"

	"encoding/binary"
)

type LoggingLevel uint64

const (
	Debug   LoggingLevel = 0x1
	Info    LoggingLevel = 0x2
	Warning LoggingLevel = 0x4
	Error   LoggingLevel = 0x8
	Default LoggingLevel = 0xE
	Any     LoggingLevel = 0xF
)

type RemoteLogSet struct {
	XPID     XPID
	Unk0     uint64
	Unk1     uint64
	Unk2     uint64
	Unk3     uint64
	LogLevel LoggingLevel
	Logs     []string
}

func (m RemoteLogSet) String() string {
	return fmt.Sprintf("%T{xp_id=%s,log_level=%d, num_logs=%d}", m, m.XPID.String(), m.LogLevel, len(m.Logs))
}

func (m *RemoteLogSet) Stream(s *EasyStream) error {
	return RunErrorFunctions([]func() error{
		func() error { return s.StreamStruct(&m.XPID) },
		func() error { return s.StreamNumber(binary.LittleEndian, &m.Unk0) },
		func() error { return s.StreamNumber(binary.LittleEndian, &m.Unk1) },
		func() error { return s.StreamNumber(binary.LittleEndian, &m.Unk2) },
		func() error { return s.StreamNumber(binary.LittleEndian, &m.Unk3) },
		func() error { return s.StreamNumber(binary.LittleEndian, &m.LogLevel) },
		func() error { return s.StreamStringTable(&m.Logs) },
	})
}

type RemoteLogString string

func (m RemoteLogString) String() string {
	return string(m)
}
