package evr

import (
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"

	"github.com/gofrs/uuid/v5"
)

const (
	// Platform represents the platform on which a client may be operating.
	UNK            PlatformID = 0 // Unknown
	STM            PlatformID = 1 // Steam
	PSN            PlatformID = 2 // Playstation
	XBX            PlatformID = 3 // Xbox
	OVR            PlatformID = 4 // Oculus VR user
	OVR_Deprecated PlatformID = 5 // Oculus VR
	BOT            PlatformID = 6 // Bot/AI
	DMO            PlatformID = 7 // Demo (no ovr)
	TEN            PlatformID = 8 // Tencent

	// UserType represents the type of user.
	SystemUser UserType = 0
	GuestUser  UserType = 1
	BotUser    UserType = 2
)

// Platform represents the platforms on which a client may be operating.
type (
	AccountID  uint64
	PlatformID uint32
	UserType   uint8

	ProviderID struct {
		PlatformID PlatformID
		UserType   UserType
		Reserved   uint8
	}
)

func (p ProviderID) String() string {
	text, err := p.PlatformID.MarshalText()
	if err != nil {
		return ""
	}
	return string(text)
}
func (p *PlatformID) UnmarshalText(text []byte) error {
	switch string(text) {
	case "UNK":
		*p = UNK
	case "STM":
		*p = STM
	case "PSN":
		*p = PSN
	case "XBX":
		*p = XBX
	case "OVR_ORG":
		*p = OVR
	case "OVR":
		*p = OVR_Deprecated
	case "BOT":
		*p = BOT
	case "DMO":
		*p = DMO
	case "TEN":
		*p = TEN
	default:
		return fmt.Errorf("invalid platform: %s", text)
	}
	return nil
}

func (p PlatformID) MarshalText() (string, error) {
	switch p {
	case UNK:
		return "UNK", nil
	case STM:
		return "STM", nil
	case PSN:
		return "PSN", nil
	case XBX:
		return "XBX", nil
	case OVR:
		return "OVR-ORG", nil
	case OVR_Deprecated:
		return "OVR", nil
	case BOT:
		return "BOT", nil
	case DMO:
		return "DMO", nil
	case TEN:
		return "TEN", nil
	default:
		return "UNK", nil
	}
}

// XPID represents an identifier for a user on the platform.
type XPID struct {
	ProviderID ProviderID
	AccountID  AccountID
}

func NewXPID(platformID PlatformID, accountID AccountID) XPID {
	return XPID{
		ProviderID: ProviderID{
			PlatformID: platformID,
			UserType:   SystemUser,
		},
		AccountID: accountID,
	}
}

func NewXPIDWithUserType(platformID PlatformID, userType UserType, accountID AccountID) XPID {
	return XPID{
		ProviderID: ProviderID{
			PlatformID: platformID,
			UserType:   userType,
		},
		AccountID: accountID,
	}
}

func (x XPID) IsValid() bool {
	return x.ProviderID.PlatformID > STM && x.ProviderID.PlatformID < TEN && x.AccountID > 0
}

func (x XPID) IsNil() bool {
	return x == XPID{}
}

func (x XPID) UUID() uuid.UUID {
	if x.ProviderID.PlatformID == 0 || x.AccountID == 0 {
		return uuid.Nil
	}
	return uuid.NewV5(uuid.Nil, x.String())
}

func (x XPID) String() string {
	s, _ := x.MarshalText()
	return string(s)
}

func (x XPID) Token() string {
	return x.String()
}

func XPIDFromString(s string) (XPID, error) {
	x := XPID{}

	if err := x.UnmarshalText([]byte(s)); err != nil {
		return XPID{}, fmt.Errorf("failed to unmarshal text: %w", err)
	}
	return x, nil
}

func (x XPID) MarshalText() ([]byte, error) {
	if x.ProviderID.PlatformID == UNK && x.AccountID == 0 {
		return []byte{}, nil
	}
	return []byte(fmt.Sprintf("%s-%d", x.ProviderID.String(), x.AccountID)), nil
}

func (x *XPID) UnmarshalText(text []byte) error {

	platformIDStr, accountIDStr, ok := strings.Cut(string(text), "-")
	if !ok {
		return fmt.Errorf("invalid XPID format")
	}

	platformID, err := strconv.ParseUint(platformIDStr, 10, 32)
	if err != nil {
		return fmt.Errorf("failed to parse platform identifier: %w", err)
	}

	accountID, err := strconv.ParseUint(accountIDStr, 10, 64)
	if err != nil {
		return fmt.Errorf("failed to parse account identifier: %w", err)
	}

	x.ProviderID.PlatformID = PlatformID(platformID)
	x.AccountID = AccountID(accountID)

	return nil
}

func (p *XPID) SizeOf() int {
	return 16
}

func (x *XPID) MarshalBinary() ([]byte, error) {
	data := make([]byte, 16)
	data[0] = byte(x.ProviderID.PlatformID<<4) | byte(x.ProviderID.UserType<<2)
	binary.BigEndian.PutUint64(data[8:], uint64(x.AccountID))
	return data, nil
}

func (x *XPID) UnmarshalBinary(data []byte) error {
	if len(data) < 16 {
		return fmt.Errorf("invalid data length")
	}

	x.ProviderID.PlatformID = PlatformID(data[0] >> 4)
	x.ProviderID.UserType = UserType((data[0] >> 2) & 0x03)

	return nil
}

func (xpi *XPID) Stream(s *EasyStream) error {
	if s.IsReading() {
		b := make([]byte, 16)
		if _, err := s.Read(b); err != nil {
			return fmt.Errorf("failed to read data: %w", err)
		}

		if err := xpi.UnmarshalBinary(b); err != nil {
			return fmt.Errorf("failed to unmarshal binary: %w", err)
		}
		return nil
	}

	b, err := xpi.MarshalBinary()
	if err != nil {
		return fmt.Errorf("failed to marshal binary: %w", err)
	}

	if _, err := s.Write(b); err != nil {
		return fmt.Errorf("failed to write data: %w", err)
	}
	return nil
}
