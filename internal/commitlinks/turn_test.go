package commitlinks

import (
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestTrailerTurnIgnoresLaterToolCalls(t *testing.T) {
	entries := []TurnEntry{
		{Ordinal: 0, At: "2026-01-01T11:00:00Z", Prompt: "Fix parser"},
		{Ordinal: 1, At: "2026-01-01T11:01:00Z", Calls: []string{`{"file_path":"/repo/src/parser.go"}`}},
		{Ordinal: 2, At: "2026-01-01T12:00:00Z", Prompt: "A different change"},
		{Ordinal: 3, At: "2026-01-01T12:01:00Z", Calls: []string{`{"file_path":"/repo/src/parser.go"}`}},
	}
	got := SelectTurn(entries, "2026-01-01T11:02:00Z", []string{"src/parser.go"})
	require.NotNil(t, got)
	assert.Equal(t, 0, got.Ordinal)
	assert.Equal(t, "wrote", got.Why)
	assert.Equal(t, "Fix parser", got.Prompt)
	before := SelectTurn(entries, "2026-01-01T11:02:00Z", []string{"README.md"})
	require.NotNil(t, before)
	assert.Equal(t, "before", before.Why)
	match := SelectTurn(entries, "2025-01-01T00:00:00Z", nil)
	require.NotNil(t, match)
	assert.Equal(t, "match", match.Why)
	assert.Nil(t, SelectTurn(nil, "", nil))
}

func TestTrailerTurnCutoff(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		prompt := hegel.Draw(ht, hegel.Text().MinSize(1).MaxSize(2000))
		delta := hegel.Draw(ht, hegel.SampledFrom([]time.Duration{-time.Nanosecond, 0, time.Nanosecond}))
		at := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
		entries := []TurnEntry{{Ordinal: 0, At: at.Add(-time.Minute).Format(time.RFC3339Nano), Prompt: prompt}, {Ordinal: 1, At: at.Add(5*time.Minute + delta).Format(time.RFC3339Nano), Calls: []string{`{"path":"src/parser.go"}`}}}
		got := SelectTurn(entries, at.Format(time.RFC3339Nano), []string{"src/parser.go"})
		require.NotNil(ht, got)
		if delta <= 0 {
			assert.Equal(ht, "wrote", got.Why)
		} else {
			assert.Equal(ht, "before", got.Why)
		}

	}, hegel.WithTestCases(2000))
}

func TestTrailerTurnUnicodeLimit(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		prompt := hegel.Draw(ht, hegel.Text().MinSize(1))
		got := SelectTurn([]TurnEntry{{Ordinal: 0, Prompt: prompt}}, "", nil)
		require.NotNil(ht, got)
		assert.LessOrEqual(ht, utf8.RuneCountInString(got.Prompt), 600)
	}, hegel.WithTestCases(2000))
}
