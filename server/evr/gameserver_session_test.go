package evr

import (
	"encoding/binary"
	"net"
	"testing"

	"github.com/gofrs/uuid/v5"
	"github.com/google/go-cmp/cmp"
)

func TestGameServerSessionParsers(t *testing.T) {
	testCases := []struct {
		name string
		data []byte
		want Message
	}{
		{
			name: "Game Server Registration",
			data: []byte{
				0x6e, 0xb6, 0xc0, 0xe4, 0xaa, 0xb1, 0xef, 0x11, 0xa3, 0xa0, 0x66, 0xd3, 0xff, 0x8a, 0x65, 0x3b,
				0x5b, 0x1f, 0xd2, 0xd4, 0xff, 0x3b, 0xe7, 0x94, 0xc0, 0xa8, 0x38, 0x01, 0x88, 0x1a, 0x00, 0x00,
				0x9e, 0x5b, 0x61, 0x5f, 0x99, 0x8a, 0xd3, 0x2f, 0x0d, 0x91, 0x77, 0x8f, 0xd7, 0x01, 0x2f, 0xc6,
				0x8d, 0x20, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			},
			want: &EchoToolsGameServerRegistrationRequestV1{
				LoginSessionID: uuid.UUID{
					0xe4, 0xc0, 0xb6, 0x6e, 0xb1, 0xaa, 0x11, 0xef,
					0xa3, 0xa0, 0x66, 0xd3, 0xff, 0x8a, 0x65, 0x3b,
				},
				ServerID:      10729610607206735707,
				InternalIP:    net.IPv4(192, 168, 56, 1),
				Port:          6792,
				RegionHash:    Symbol(0x8a995f615b9e0000),
				VersionLock:   132732457728159699,
				TimeStepUsecs: 546162223,
			},
		},

		{
			name: "Game Server Session Status",
			data: []byte{

				0x35, 0xac, 0x45, 0xa9, 0x41, 0xa1, 0x79, 0x43,
				0x8f, 0xbb, 0x74, 0x6b, 0x59, 0x87, 0xf9, 0x98,
				0x46, 0x10, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0xb0, 0x04, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x0c, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x30, 0xf2, 0x74, 0xff, 0xd9, 0x01, 0x00, 0x00,
				0x0c, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			},

			want: &EchoToolsLobbyStatusV1{
				LobbySessionID: uuid.UUID{
					0xfb, 0xb9, 0x0c, 0x3a, 0xf0, 0x7f, 0x4d, 0xab, 0x93, 0x07, 0x2d, 0x7a, 0x69, 0xce, 0x40, 0xd8,
				},
				TimeStepUsecs: 4166,
				TickCount:     26400,
				Slots: []*EntrantData{
					{
						XPID:      NewXPID(12, 4702111234474983745),
						TeamIndex: 0x0a,
						Ping:      0x0,
					},
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			data, _ := WrapBytes(SymbolOf(tc.want), tc.data)

			messages, err := ParsePacket(data)
			if err != nil {
				t.Fatalf(err.Error())
			}

			if diff := cmp.Diff(tc.want, messages[0]); diff != "" {
				t.Errorf("unexpected LobbyFindSessionRequest (-want +got):\n%s", diff)
			}
		})
	}

}

func TestGameServerEntrantDataEncoding(t *testing.T) {
	testCases := []struct {
		name string
		data []byte
		want EntrantData
	}{
		{
			name: "Game Server Session Status",
			data: []byte{
				//0x70, 0x94,
				//0x05,
				//0x6d,
				0xb2, 0x01, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			},
			want: EntrantData{},
		},
	}

	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, 1139)
	t.Errorf("%v", b)

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {

			s := NewEasyStream(DecodeMode, tc.data)

			var m EntrantData
			err := m.Stream(s)
			if err != nil {
				t.Fatalf(err.Error())
			}

			if diff := cmp.Diff(tc.want, m); diff != "" {
				t.Errorf("unexpected EntrantData (-want +got):\n%s", diff)
			}
		})
	}

}
