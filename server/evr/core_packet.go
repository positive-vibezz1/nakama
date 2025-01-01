package evr

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"slices"
)

var (
	MessageMarker     = []byte{246, 64, 187, 120, 162, 231, 140, 187}
	MaxPacketLength   = 1024 * 1024 * 10 // 10MB
	MaxMessageLength  = 0x8000           // 32KB
	ErrInvalidPacket  = errors.New("invalid packet")
	ErrSymbolNotFound = errors.New("symbol not found")
	ErrParseError     = errors.New("parse error")

	SymbolTypes = map[uint64]Message{
		// This is the complete list of implemented message types.
		/*
			0x4c1fed6cb4d96c64: (*SNSLobbySmiteEntrant)(nil),
			0x013e99cb47eb3669: (*GenericMessage)(nil),
			0x35d810572a230837: (*GenericMessageNotify)(nil),
			0x80119c19ac72d695: (*MatchEnded)(nil),
		*/
		0x0dabc24265508a82: (*ReconcileIAPResult)(nil),
		0x1225133828150da3: (*OtherUserProfileFailure)(nil),
		0x1230073227050cb5: (*OtherUserProfileSuccess)(nil),
		0x1231172031050cb2: (*OtherUserProfileRequest)(nil),
		0x128b777ae0ebb650: (*LobbyMatchmakerStatusRequest)(nil),
		0x1bd0fc454c85573c: (*ReconcileIAP)(nil),
		0x244b47685187eae1: (*RemoteLogSet)(nil),
		0x2f03468f77ffb211: (*LobbyJoinSessionRequest)(nil),
		0x312c2a01819aa3f5: (*LobbyFindSessionRequest)(nil),
		0x43e6963ac76beee4: (*STcpConnectionUnrequireEvent)(nil),
		0xb99f11d6ea5cb1f1: (*LobbySessionFailurev1)(nil),
		0x4ae8365ebc45f96a: (*LobbySessionFailurev2)(nil),
		0x4ae8365ebc45f96b: (*LobbySessionFailurev3)(nil),
		0x4ae8365ebc45f96c: (*LobbySessionFailurev4)(nil),
		0x599a6b1bbda3cc13: (*LobbyCreateSessionRequest)(nil),
		0xfabf5f8719bfebf3: (*LobbyPingRequest)(nil),
		0x6047d0043033ae4f: (*LobbyPingResponse)(nil),
		0x6c8f16cd9f8964c5: (*ChannelInfoResponse)(nil),
		0x6d4de3650ee3110e: (*LobbySessionSuccessv4)(nil),
		0x6d4de3650ee3110f: (*LobbySessionSuccessv5)(nil),
		0x6d54a19a3ff24415: (*UpdateClientProfile)(nil),
		0x7777777777770000: (*GameServerSessionStart)(nil),
		0x7777777777770100: (*BroadcasterSessionStarted)(nil),
		0x7777777777770200: (*BroadcasterSessionEnded)(nil),
		0x7777777777770300: (*BroadcasterPlayerSessionsLocked)(nil),
		0x7777777777770400: (*BroadcasterPlayerSessionsUnlocked)(nil),
		0x7777777777770500: (*GameServerJoinAttempt)(nil),
		0x7777777777770600: (*GameServerJoinAllowed)(nil),
		0x7777777777770700: (*GameServerJoinRejected)(nil),
		0x7777777777770800: (*GameServerPlayerRemoved)(nil),
		0x7777777777770900: (*BroadcasterChallengeRequest)(nil),
		0x7777777777770a00: (*GameServerChallengeResponse)(nil),
		0x7777777777777777: (*BroadcasterRegistrationRequest)(nil),
		0x82869f0b37eb4378: (*ConfigRequest)(nil),
		0xb9cdaf586f7bd012: (*ConfigSuccess)(nil),
		0x9e687a63dddd3870: (*ConfigFailure)(nil),
		0x8d5ad3c4f2166c6c: (*FindServerRegionInfo)(nil),
		0x8da9eb83ffee9fd6: (*LobbyPendingSessionCancel)(nil),
		0x8f28cf33dabfbecb: (*LobbyMatchmakerStatus)(nil),
		0x90758e58515724e0: (*ChannelInfoRequest)(nil),
		0x9af2fab2a0c81a05: (*LobbyPlayerSessionsRequest)(nil),
		0xa1b9cae1f8588968: (*LobbyEntrantsV2)(nil),
		0xa1b9cae1f8588969: (*LobbyEntrantsV3)(nil),
		0xbdb41ea9e67b200a: (*LoginRequest)(nil),
		0xa5acc1a90d0cce47: (*LoginSuccess)(nil),
		0xa5b9d5a3021ccf51: (*LoginFailure)(nil),
		0xb56f25c7dfe6ffc9: (*BroadcasterRegistrationFailure)(nil),
		0xb57a31cdd0f6fedf: (*BroadcasterRegistrationSuccess)(nil),
		0xd06ae97220a7b41f: (*DocumentFailure)(nil),
		0xd07ffd782fb7b509: (*DocumentSuccess)(nil),
		0xd2986849b36b9c72: (*UserServerProfileUpdateRequest)(nil),
		0xd299785ba56b9c75: (*UserServerProfileUpdateSuccess)(nil),
		0xe4b9b1cab57e8988: (*LobbyStatusNotify)(nil),
		0xed5be2c3632155f1: (*GameSettings)(nil),
		0xf24185da0edef641: (*UpdateProfileFailure)(nil),
		0xf25491d001cef757: (*UpdateProfileSuccess)(nil),
		0xfb632e5a38ec8c61: (*LoggedInUserProfileFailure)(nil),
		0xfb763a5037fc8d77: (*LoggedInUserProfileSuccess)(nil),
		0xfb772a4221fc8d70: (*LoggedInUserProfileRequest)(nil),
		0xfcced6f169822bb8: (*DocumentRequest)(nil),
		0xff71856af7e0fbd9: (*LobbyEntrantsV0)(nil),
		//0x080495a43a6b7251: (*EarlyQuitConfig)(nil),
		0xe581ba9febf68535: (*EchoToolsGameServerRegistrationRequestV1)(nil),
		0x353172e01aa544a5: (*EchoToolsLobbySessionStartV1)(nil),
		0x350d1070be48ebcb: (*EchoToolsLobbySessionStartedV1)(nil),
		0x352768e50db544a5: (*EchoToolsLobbySessionEndedV1)(nil),
		0x7d5abda8e440b617: (*EchoToolsLobbySessionErroredV1)(nil),
		0xa2a42bc683ebb3fd: (*EchoToolsLobbySessionLockV1)(nil),
		0x6fe3fd47131b6713: (*EchoToolsLobbySessionUnlockV1)(nil),
		0x66b54df504afebcd: (*EchoToolsLobbyEntrantNewV1)(nil),
		0x174e85ca13e1a637: (*EchoToolsLobbyEntrantAllowV1)(nil),
		0x04488cca00e1a637: (*EchoToolsLobbyEntrantRejectV1)(nil),
		0xe5ef595892ea3d99: (*EchoToolsLobbyEntrantRemovedV1)(nil),
		0xa2a423c894e1b3fd: (*EchoToolsLobbySessionDataV1)(nil),
		0xb26450c1a5ba5d79: (*EchoToolsLobbyStatusV1)(nil),
	}

	// Create a reverse lookup map for the symbol types.
	reverseSymbolTypes = make(map[string]uint64, len(SymbolTypes))
)

// Marshal returns the wire-format encoding of multiple messages.
func Marshal(msgs ...Message) ([]byte, error) {
	var errs error
	b := make([]byte, 0)
	for _, m := range msgs {
		// Encode the message.
		s := NewEasyStream(EncodeMode, []byte{})
		if err := m.Stream(s); err != nil {
			errs = errors.Join(fmt.Errorf("could not stream message:%s", err), errs)
			continue
		}
		// Write the message type symbol.
		typ := reflect.TypeOf(m).String()
		sym, ok := reverseSymbolTypes[typ]
		if !ok {
			errs = errors.Join(ErrSymbolNotFound, fmt.Errorf("message type %T", m), errs)
			continue
		}
		// Write the Header (Marker + Symbol + Data Length)
		b = append(b, MessageMarker...)
		b = appendUint64(b, uint64(sym))
		b = appendUint64(b, uint64(s.Len()))
		// Write the message data.
		b = append(b, s.Bytes()...)
	}
	return b, errs
}

func WrapBytes(symbol Symbol, data []byte) ([]byte, error) {
	b := make([]byte, 0)

	// Write the Header (Marker + Symbol + Data Length)
	b = append(b, MessageMarker...)
	b = appendUint64(b, uint64(symbol))
	b = appendUint64(b, uint64(len(data)))
	// Write the message data.
	b = append(b, data...)
	return b, nil
}

// SplitPacket splits the packet into individual messages.
func SplitPacket(data []byte) [][]byte {
	return bytes.Split(data, MessageMarker)
}

var ignoredSymbols = []uint64{
	0x4c1fed6cb4d96c64,
	0x013e99cb47eb3669,
	0x35d810572a230837,
	0x80119c19ac72d695,
}

// ParsePacket parses the wire-format packet in data and places the result in m.
// The provided message must be mutable (e.g., a non-nil pointer to a slice).
func ParsePacket(data []byte) ([]Message, error) {
	var err error

	// Split the packet into individual messages.
	chunks := bytes.Split(data, MessageMarker)

	messages := make([]Message, 0, len(chunks))

	for _, b := range chunks {
		if len(b) == 0 {
			// Skip empty messages.
			continue
		}
		buf := bytes.NewBuffer(b)
		// Verify packet length.
		if buf.Len() < 16 {
			return nil, errors.Join(ErrInvalidPacket, ErrInvalidPacket)
		}
		// Read the message type and data length.
		sym := dUint64(buf.Next(8))

		// Ignore specific messages
		if slices.Contains(ignoredSymbols, sym) {
			continue
		}

		l := int(dUint64(buf.Next(8)))
		// Verify the message data can be read from the rest of the packet.
		if buf.Len() != l {
			return nil, errors.Join(ErrInvalidPacket, fmt.Errorf("truncated packet (expected %d bytes, got %d)", l, buf.Len()))
		}
		// Read the payload.
		b = buf.Next(l)
		// Unmarshal the message.
		typ, ok := SymbolTypes[sym]
		if !ok || typ == nil {
			// Skip unimplemented message types.
			continue
		}

		// Create a new message of the correct type and unmarshal the data into it.
		message := reflect.New(reflect.TypeOf(typ).Elem()).Interface().(Message)
		if err = message.Stream(NewEasyStream(DecodeMode, b)); err != nil {
			return nil, fmt.Errorf("Stream error: %T: %w", typ, err)

		}
		messages = append(messages, message)
	}
	return messages, err
}

// AppendUint64 appends the (little-endian) byte representation of v to b and returns the resulting slice.
func appendUint64(b []byte, v uint64) []byte {
	return append(b,
		byte(v),
		byte(v>>8),
		byte(v>>16),
		byte(v>>24),
		byte(v>>32),
		byte(v>>40),
		byte(v>>48),
		byte(v>>56),
	)
}

// Uint64 decodes a little-endian uint64 from the provided byte slice.
func dUint64(b []byte) uint64 {
	_ = b[7] // bounds check hint to compiler; see golang.org/issue/14808
	return uint64(b[0]) |
		uint64(b[1])<<8 |
		uint64(b[2])<<16 |
		uint64(b[3])<<24 |
		uint64(b[4])<<32 |
		uint64(b[5])<<40 |
		uint64(b[6])<<48 |
		uint64(b[7])<<56
}

func init() {
	// Populate the new map
	for key, value := range SymbolTypes {
		typeName := reflect.TypeOf(value).String()
		reverseSymbolTypes[typeName] = key
	}
}

// SymbolOf returns the type symbol of the message.
func SymbolOf(m Message) Symbol {
	typ := reflect.TypeOf(m).String()
	sym, ok := reverseSymbolTypes[typ]
	if !ok {
		panic(fmt.Errorf("Symbol not found: %T", m))
	}
	return Symbol(sym)
}

// MessageTypeOf returns a new instance of the message type.
func MessageTypeOf(s Symbol) Message {
	if m, ok := SymbolTypes[uint64(s)]; ok {
		// return a new instance of the message type
		return reflect.New(reflect.TypeOf(m).Elem()).Interface().(Message)
	}
	return nil
}
