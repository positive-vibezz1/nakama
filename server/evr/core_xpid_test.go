package evr

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/gofrs/uuid/v5"
)

func TestEvrId_UUID(t *testing.T) {
	type fields struct {
		PlatformCode PlatformID
		AccountId    AccountID
	}
	tests := []struct {
		name   string
		fields fields
		want   uuid.UUID
	}{
		{
			name: "valid UUID",
			fields: fields{
				PlatformCode: 1,
				AccountId:    1,
			},
			want: uuid.FromStringOrNil("496d8944-6159-5c53-bdc8-1cab22f9d28d"),
		},
		{
			name: "invalid PlatformCode",
			fields: fields{
				PlatformCode: 0,
				AccountId:    12341234,
			},
			want: uuid.Nil,
		},
		{
			name: "invalid AccountId",
			fields: fields{
				PlatformCode: 4,
				AccountId:    0,
			},
			want: uuid.Nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			xpi := NewXPID(tt.fields.PlatformCode, tt.fields.AccountId)
			if got := xpi.UUID(); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("EvrId.UUID = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEvrId_Equal(t *testing.T) {

	xpID1 := NewXPID(1, 1)
	xpID2 := NewXPID(1, 1)

	if xpID1 != xpID2 {
		t.Errorf("EvrId.Equal() = %v, want %v", xpID1, xpID2)
	}
}

func TestEvrId_Equals(t *testing.T) {
	type fields struct {
		PlatformCode PlatformID
		AccountId    AccountID
	}
	type args struct {
		other XPID
	}
	tests := []struct {
		name   string
		fields fields
		args   args
		want   bool
	}{
		{
			name: "valid",
			fields: fields{
				PlatformCode: 1,
				AccountId:    1,
			},
			args: args{
				NewXPID(1, 1),
			},
			want: true,
		},
		{
			name: "invalid PlatformCode",
			fields: fields{
				PlatformCode: 0,
				AccountId:    1,
			},
			args: args{
				NewXPID(1, 1),
			},
			want: false,
		},
		{
			name: "invalid AccountId",
			fields: fields{
				PlatformCode: 1,
				AccountId:    0,
			},
			args: args{
				NewXPID(1, 1),
			},
			want: false,
		},
		{
			name: "invalid",
			fields: fields{
				PlatformCode: 1,
				AccountId:    1,
			},
			args: args{
				NewXPID(2, 2),
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			xpi := NewXPID(tt.fields.PlatformCode, tt.fields.AccountId)
			if got := xpi == tt.args.other; got != tt.want {
				t.Errorf("EvrId.Equals() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEvrId_IsNil(t *testing.T) {
	type fields struct {
		PlatformCode PlatformID
		AccountId    AccountID
	}
	tests := []struct {
		name   string
		fields fields
		want   bool
	}{
		{
			name: "valid",
			fields: fields{
				PlatformCode: 0,
				AccountId:    0,
			},
			want: true,
		},
		{
			name: "invalid PlatformCode",
			fields: fields{
				PlatformCode: 0,
				AccountId:    1,
			},
			want: false,
		},
		{
			name: "invalid AccountId",
			fields: fields{
				PlatformCode: 1,
				AccountId:    0,
			},
			want: false,
		},
		{
			name: "invalid",
			fields: fields{
				PlatformCode: 1,
				AccountId:    1,
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			xpi := NewXPID(tt.fields.PlatformCode, tt.fields.AccountId)
			if got := xpi.IsNil(); got != tt.want {
				t.Errorf("EvrId.IsNil() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEvrId_MarshalText(t *testing.T) {
	type fields struct {
		PlatformCode PlatformID
		AccountId    AccountID
	}
	tests := []struct {
		name    string
		fields  fields
		want    []byte
		wantErr bool
	}{
		{
			name: "valid",
			fields: fields{
				PlatformCode: 1,
				AccountId:    1,
			},
			want:    []byte("STM-1"),
			wantErr: false,
		},
		{
			name: "invalid PlatformCode",
			fields: fields{
				PlatformCode: 0,
				AccountId:    1,
			},
			want:    []byte("UNK-1"),
			wantErr: false,
		},
		{
			name: "invalid AccountId",
			fields: fields{
				PlatformCode: 1,
				AccountId:    0,
			},
			want:    []byte("STM-0"),
			wantErr: false,
		},
		{
			name: "invalid",
			fields: fields{
				PlatformCode: 0,
				AccountId:    0,
			},
			want:    []byte(""),
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := NewXPID(tt.fields.PlatformCode, tt.fields.AccountId)
			got, err := e.MarshalText()
			if (err != nil) != tt.wantErr {
				t.Errorf("EvrId.MarshalText() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("EvrId.MarshalText() = `%v`, want `%v`", string(got), string(tt.want))
			}
		})
	}
}

func TestEvrId_UnmarshalJSON(t *testing.T) {
	type fields struct {
		PlatformCode PlatformID
		AccountId    AccountID
	}
	type args struct {
		b []byte
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		wantErr bool
	}{
		{
			name: "valid",
			fields: fields{
				PlatformCode: 1,
				AccountId:    1,
			},
			args: args{
				b: []byte(`"STM-1"`),
			},
			wantErr: false,
		},
		{
			name: "invalid PlatformCode",
			fields: fields{
				PlatformCode: 0,
				AccountId:    1,
			},
			args: args{
				b: []byte(`"UNK-1"`),
			},
			wantErr: false,
		},
		{
			name: "invalid AccountId",
			fields: fields{
				PlatformCode: 1,
				AccountId:    0,
			},
			args: args{
				b: []byte(`"STM-0"`),
			},
			wantErr: false,
		},
		{
			name: "invalid",
			fields: fields{
				PlatformCode: 0,
				AccountId:    0,
			},
			args: args{
				b: []byte(`""`),
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := NewXPID(tt.fields.PlatformCode, tt.fields.AccountId)
			if err := json.Unmarshal(tt.args.b, e); (err != nil) != tt.wantErr {
				t.Errorf("EvrId.UnmarshalJSON() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestXPIDFromString(t *testing.T) {
	type args struct {
		s string
	}
	tests := []struct {
		name    string
		args    args
		want    XPID
		wantErr bool
	}{
		{
			name: "valid",
			args: args{
				s: "STM-1",
			},
			want:    NewXPID(1, 1),
			wantErr: false,
		},
		{
			name: "invalid PlatformCode",
			args: args{
				s: "UNK-1",
			},
			want:    NewXPID(0, 1),
			wantErr: false,
		},
		{
			name: "invalid AccountId",
			args: args{
				s: "STM-0",
			},
			want:    NewXPID(1, 0),
			wantErr: false,
		},
		{
			name: "OVR_ORG-3963667097037078",
			args: args{
				s: "OVR_ORG-3963667097037078",
			},
			want:    NewXPID(4, 3963667097037078),
			wantErr: false,
		},
		{
			name: "OVR-ORG-3963667097037078",
			args: args{
				s: "OVR-ORG-3963667097037078",
			},
			want:    NewXPID(4, 3963667097037078),
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := XPIDFromString(tt.args.s)
			if (err != nil) != tt.wantErr {
				t.Errorf("XPIDFromString() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("XPIDFromString() = %v, want %v", got, tt.want)
			}
		})
	}
}
