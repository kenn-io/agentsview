package db

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A truncated token_usage blob (produced by a torn read of a JSONL line
// still being written) is stored verbatim by the parsers, which use
// gjson's raw slice without checking validity. It only fails later, when
// the API marshals the response, because jsontext.Value re-parses its
// bytes at marshal time. That surfaced as an unrecovered handler panic
// and a dropped connection (ERR_EMPTY_RESPONSE) on
// GET /api/v1/sessions/{id}/messages.
func TestSanitizeMessageBlanksInvalidTokenUsage(t *testing.T) {
	for _, tc := range []struct {
		name      string
		usage     string
		want      string
		wantCount int
	}{
		{
			name:  "valid usage preserved",
			usage: `{"input_tokens":4,"output_tokens":9}`,
			want:  `{"input_tokens":4,"output_tokens":9}`,
		},
		{
			name:  "empty usage preserved",
			usage: "",
			want:  "",
		},
		{
			name:      "truncated object blanked",
			usage:     `{"input_tokens":4,"cache_read_input_tokens":123`,
			want:      "",
			wantCount: 1,
		},
		{
			name:      "trailing garbage blanked",
			usage:     `{"input_tokens":4} trailing`,
			want:      "",
			wantCount: 1,
		},
		{
			name:      "non-json blanked",
			usage:     `not json at all`,
			want:      "",
			wantCount: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := Message{Role: "assistant"}
			if tc.usage != "" {
				m.TokenUsage = jsontext.Value(tc.usage)
			}
			stats := SanitizeMessage(&m)
			assert.Equal(t, tc.want, string(m.TokenUsage))
			assert.Equal(t, tc.wantCount, stats.TokenUsageBlanked)
		})
	}
}

// Sanitizing must leave every row marshalable, since that is the failure
// the pass exists to prevent.
func TestSanitizeMessageLeavesRowMarshalable(t *testing.T) {
	m := Message{
		Role:       "assistant",
		TokenUsage: jsontext.Value(`{"input_tokens":4,"cache_read_input_tokens":123`),
	}
	_, err := json.Marshal(struct {
		Messages []Message `json:"messages"`
	}{Messages: []Message{m}})
	require.Error(t, err, "precondition: invalid usage must fail to marshal")

	SanitizeMessage(&m)

	_, err = json.Marshal(struct {
		Messages []Message `json:"messages"`
	}{Messages: []Message{m}})
	require.NoError(t, err)
}

// ValidateAndSanitize is the seam every write path shares, so the guard
// must hold there and not only in SanitizeMessage.
func TestValidateAndSanitizeBlanksInvalidTokenUsage(t *testing.T) {
	msgs := []Message{
		{Role: "assistant", TokenUsage: jsontext.Value(`{"input_tokens":1`)},
		{Role: "assistant", TokenUsage: jsontext.Value(`{"input_tokens":2}`)},
	}
	stats := ValidateAndSanitize(nil, msgs, nil)
	assert.Equal(t, 1, stats.TokenUsageBlanked)
	assert.Empty(t, string(msgs[0].TokenUsage))
	assert.Equal(t, `{"input_tokens":2}`, string(msgs[1].TokenUsage))
}

// The pass must stay idempotent: running it over its own output produces
// no further fixes (see the idempotency invariant in validate.go).
func TestSanitizeMessageTokenUsageIdempotent(t *testing.T) {
	m := Message{
		Role:       "assistant",
		TokenUsage: jsontext.Value(`{"input_tokens":4,"cache_read`),
	}
	first := SanitizeMessage(&m)
	require.Equal(t, 1, first.TokenUsageBlanked)

	second := SanitizeMessage(&m)
	assert.Equal(t, 0, second.TokenUsageBlanked)
	assert.Empty(t, string(m.TokenUsage))
}

// DecodeStoredTokenUsage is exported because every backend that serves
// messages needs the identical guard (internal/postgres/messages.go,
// internal/duckdb/messages.go), so its contract is pinned here.
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
