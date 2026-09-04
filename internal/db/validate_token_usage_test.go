package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// DecodeStoredTokenUsage is the single guard every backend that serves
// messages reads through (internal/db, internal/postgres, internal/duckdb),
// so its contract is pinned here.
//
// The empty case is the one that matters in practice: token_usage is
// "TEXT NOT NULL DEFAULT ”", so nearly every row holds "". Returning a
// non-nil, zero-length jsontext.Value there is what made every duckdb serve
// transcript blank -- it is not valid JSON, omitempty does not skip it, and
// the response fails to marshal. Dropping invalid values is defense at the
// point of consequence: reaching the encoder with one panics the handler,
// and internal/server installs no recover().
func TestDecodeStoredTokenUsage(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{name: "empty yields nil", raw: "", want: ""},
		{name: "valid object preserved", raw: `{"input_tokens":4}`, want: `{"input_tokens":4}`},
		{name: "valid array preserved", raw: `[1,2]`, want: `[1,2]`},
		{name: "truncated object dropped", raw: `{"input_tokens":4`, want: ""},
		{name: "trailing garbage dropped", raw: `{"a":1} x`, want: ""},
		{name: "non-json dropped", raw: `nope`, want: ""},
		{name: "bare whitespace dropped", raw: `   `, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := DecodeStoredTokenUsage(tc.raw)
			assert.Equal(t, tc.want, string(got))
			if tc.want == "" {
				assert.Nil(t, got, "dropped values must be nil, not empty non-nil")
			}
		})
	}
}
