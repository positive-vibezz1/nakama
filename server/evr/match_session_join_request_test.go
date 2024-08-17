package evr

import (
	"testing"

	"github.com/gofrs/uuid/v5"
	"github.com/google/go-cmp/cmp"
	"github.com/samber/lo"
)

func TestLobbyJoinSessionRequest(t *testing.T) {
	testCases := []struct {
		name string
		data []byte
		want *LobbyJoinSessionRequest
	}{
		{
			name: "Regular join",
			data: []byte{
				0xb6, 0x6f, 0xc1, 0xe7, 0xb7, 0xfb, 0xee, 0x11,
				0xb1, 0x92, 0x66, 0xd3, 0xff, 0x8a, 0x65, 0x3b,
				0x0d, 0x91, 0x77, 0x8f, 0xd7, 0x01, 0x2f, 0xc6,
				0xf8, 0xf4, 0x9f, 0xa8, 0xb1, 0xd0, 0xe8, 0xc8,
				0x01, 0x63, 0x8e, 0x64, 0xb9, 0xfb, 0xee, 0x11,
				0xad, 0x13, 0x66, 0xd3, 0xff, 0x8a, 0x65, 0x3b,
				0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x03, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x7b, 0x22, 0x61, 0x70, 0x70, 0x69, 0x64, 0x22,
				0x3a, 0x22, 0x31, 0x33, 0x36, 0x39, 0x30, 0x37,
				0x38, 0x34, 0x30, 0x39, 0x38, 0x37, 0x33, 0x34,
				0x30, 0x32, 0x22, 0x7d, 0x00, 0x04, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x16, 0xa9, 0x53,
				0x29, 0xef, 0x14, 0x0e, 0x00, 0xff, 0xff, 0x0a,
			},
			want: &LobbyJoinSessionRequest{
				LobbyID:        uuid.FromStringOrNil("e7c16fb6-fbb7-11ee-b192-66d3ff8a653b"),
				VersionLock:    -4166109104957845235,
				Platform:       ToSymbol("OVR"),
				LoginSessionID: uuid.Must(uuid.FromString("648e6301-fbb9-11ee-ad13-66d3ff8a653b")),

				Flags: 3,
				SessionSettings: LobbySessionSettings{
					AppID: "1369078409873402",
					Mode:  0,
					Level: int64(LevelUnspecified),
				},
				Entrants: []Entrant{
					{
						EvrID: *lo.Must(ParseEvrId("OVR-ORG-3963667097037078")),
						Role:  -1,
					},
				},
			},
		},
		{
			name: "Moderator",
			/* -moderator -lobbyid e7c16fb6-fbb7-11ee-b192-66d3ff8a653b */
			data: []byte{
				0xb6, 0x6f, 0xc1, 0xe7, 0xb7, 0xfb, 0xee, 0x11,
				0xb1, 0x92, 0x66, 0xd3, 0xff, 0x8a, 0x65, 0x3b,
				0x0d, 0x91, 0x77, 0x8f, 0xd7, 0x01, 0x2f, 0xc6,
				0xf8, 0xf4, 0x9f, 0xa8, 0xb1, 0xd0, 0xe8, 0xc8,
				0x21, 0x31, 0xf8, 0xe8, 0xb8, 0xfb, 0xee, 0x11,
				0x91, 0x82, 0x66, 0xd3, 0xff, 0x8a, 0x65, 0x3b,
				0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x03, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x7b, 0x22, 0x61, 0x70, 0x70, 0x69, 0x64, 0x22,
				0x3a, 0x22, 0x31, 0x33, 0x36, 0x39, 0x30, 0x37,
				0x38, 0x34, 0x30, 0x39, 0x38, 0x37, 0x33, 0x34,
				0x30, 0x32, 0x22, 0x7d, 0x00, 0x04, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x16, 0xa9, 0x53,
				0x29, 0xef, 0x14, 0x0e, 0x00, 0x04, 0x00, 0x0a,
			},

			want: &LobbyJoinSessionRequest{
				LobbyID:        uuid.FromStringOrNil("e7c16fb6-fbb7-11ee-b192-66d3ff8a653b"),
				VersionLock:    -4166109104957845235,
				Platform:       ToSymbol("OVR"),
				LoginSessionID: uuid.Must(uuid.FromString("e8f83121-fbb8-11ee-9182-66d3ff8a653b")),

				Flags: 3,
				SessionSettings: LobbySessionSettings{
					AppID: "1369078409873402",
					Mode:  0,
					Level: int64(LevelUnspecified),
				},
				Entrants: []Entrant{
					{
						EvrID: *lo.Must(ParseEvrId("OVR_ORG-3963667097037078")),
						Role:  4,
					},
				},
			},
		},
		{
			name: "Moderator2",
			data: []byte{
				0x07, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x0d, 0x91, 0x77, 0x8f, 0xd7, 0x01, 0x2f, 0xc6,
				0xf8, 0xf4, 0x9f, 0xa8, 0xb1, 0xd0, 0xe8, 0xc8,
				0xb6, 0x6f, 0xc1, 0xe7, 0xb7, 0xfb, 0xee, 0x11,
				0xb1, 0x92, 0x66, 0xd3, 0xff, 0x8a, 0x65, 0x3b,
				0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x0b, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x7b, 0x22, 0x61, 0x70, 0x70, 0x69, 0x64, 0x22,
				0x3a, 0x22, 0x31, 0x33, 0x36, 0x39, 0x30, 0x37,
				0x38, 0x34, 0x30, 0x39, 0x38, 0x37, 0x33, 0x34,
				0x30, 0x32, 0x22, 0x7d, 0x00, 0x04, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x16, 0xa9, 0x53,
				0x29, 0xef, 0x14, 0x0e, 0x00, 0x04, 0x00, 0x0a,
			},
			want: &LobbyJoinSessionRequest{
				LobbyID:        uuid.Nil,
				VersionLock:    -4166109104957845235,
				Platform:       ToSymbol("OVR"),
				LoginSessionID: uuid.Must(uuid.FromString("e7c16fb6-fbb7-11ee-b192-66d3ff8a653b")),
				OtherEvrID:     *lo.Must(ParseEvrId("DMO-1")),
				Flags:          11,
				SessionSettings: LobbySessionSettings{
					AppID: "1369078409873402",
					Mode:  0,
					Level: int64(LevelUnspecified),
				},
				Entrants: []Entrant{
					{
						EvrID: *lo.Must(ParseEvrId("OVR_ORG-3963667097037078")),
						Role:  4,
					},
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			data, _ := WrapBytes(SymbolOf(&LobbyJoinSessionRequest{}), tc.data)

			messages, err := ParsePacket(data)
			if err != nil {
				t.Fatalf(err.Error())
			}

			if diff := cmp.Diff(tc.want, messages[0]); diff != "" {
				t.Errorf("unexpected LobbyJoinSessionRequest (-want +got):\n%s", diff)
			}
		})
	}

}
