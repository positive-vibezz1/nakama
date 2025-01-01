package evr

import (
	"fmt"
	"strconv"
)

type Symbol uint64

func (s Symbol) HexString() string {
	str := strconv.FormatUint(uint64(s), 16)
	return fmt.Sprintf("0x%016s", str)
}

// A symbol token is a symbol converted to a string.
// It either uses the cache to convert back to a string,
// or returns the hex string representation of the token.
// ToSymbol will detect 0x prefixed hex strings.
func (s Symbol) Token() SymbolToken {
	t, ok := SymbolCache[s]
	if !ok {
		// If it's not found, just return the number as a hex string
		t = SymbolToken(s.HexString())
	}
	return t
}

func (s Symbol) MarshalText() ([]byte, error) {
	v := s.Token().String()
	return []byte(v), nil
}

func (s *Symbol) UnmarshalText(data []byte) error {
	v := string(data)
	*s = ToSymbol(v)
	return nil
}

func (s Symbol) String() string {
	return s.Token().String()
}

func (s Symbol) IsNil() bool {
	return s == 0
}

// A symbol token is a symbol converted to a string.
// It either uses the cache to convert back to a string,
// or returns the hex string representation of the token.
// ToSymbol will detect 0x prefixed hex strings.
type SymbolToken string

func (t SymbolToken) Symbol() Symbol {
	return ToSymbol(t)
}
func (t SymbolToken) String() string {
	return string(t)
}

// ToSymbol converts a string value to a symbol.
func ToSymbol(v any) Symbol {
	// if it's a number, return it as an uint64
	switch t := v.(type) {
	case Symbol:
		return t
	case int:
		return Symbol(t)
	case int64:
		return Symbol(t)
	case uint64:
		return Symbol(t)
	case SymbolToken:
		return ToSymbol(string(t))
	case string:
		str := t
		// Empty string returns 0
		if len(str) == 0 {
			return Symbol(0)
		}
		// if it's a hex represenatation, return it's value
		if len(str) == 18 && str[:2] == "0x" {
			if s, err := strconv.ParseUint(string(str[2:]), 16, 64); err == nil {
				return Symbol(s)
			}
		}
		// Convert it to a symbol
		symbol := uint64(0xffffffffffffffff)
		// lowercase the string
		for i := range str {
			a := str[i] + ' '
			if str[i] < 'A' || str[i] >= '[' {
				a = str[i]
			}
			symbol = uint64(a) ^ symbolSeed[symbol>>0x38&0xff] ^ symbol<<8
		}
		return Symbol(symbol)
	default:
		panic(fmt.Errorf("invalid type: %T", v))
	}
}
