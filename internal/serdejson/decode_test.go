package serdejson

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecode(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    any
		wantErr bool
	}{
		{"object with numbers", `{"a":1,"b":[true,null,"x"]}`, map[string]any{"a": Number("1"), "b": []any{true, nil, "x"}}, false},
		{"duplicate key keeps last", `{"a":1,"a":2}`, map[string]any{"a": Number("2")}, false},
		{"u64 above 2^53 keeps literal", `18446744073709551615`, Number("18446744073709551615"), false},
		{"literal unicode", `"😀"`, "\U0001F600", false},
		{"escaped surrogate pair", `"\ud83d\ude00"`, "\U0001F600", false},
		{"trailing value", `1 2`, nil, true},
		{"trailing garbage", `{} x`, nil, true},
		{"trailing comma", `[1,]`, nil, true},
		{"leading zero", `01`, nil, true},
		{"bare dot", `1.`, nil, true},
		{"NaN", `NaN`, nil, true},
		{"lone surrogate", `"\ud800"`, nil, true},
		{"invalid utf8", "\"\xff\"", nil, true},
		{"float out of range", `1E400`, nil, true},
		{"empty", ``, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Decode([]byte(tt.in))
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
