//go:build !(windows && arm64)

package duckdb

import (
	"cmp"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/signals"
)

// markerPrompt is one stored message of a frustration marker fixture. Each
// prompt is the only message of its session, so a session's count is the
// prompt's verdict.
type markerPrompt struct {
	content       string
	role          string
	isSystem      bool
	sourceSubtype string
}

// writeMarkerPrompts writes each prompt as session fm-<index> into a new
// mirror file at path and returns the session IDs.
func writeMarkerPrompts(t *testing.T, path string, prompts []markerPrompt) []string {
	t.Helper()
	ctx := t.Context()
	conn, err := Open(ctx, path)
	require.NoError(t, err)
	defer func() { require.NoError(t, conn.Close()) }()
	require.NoError(t, EnsureSchema(ctx, conn))
	c, err := conn.Conn(ctx)
	require.NoError(t, err)
	defer func() { require.NoError(t, c.Close()) }()
	ids := make([]string, len(prompts))
	require.NoError(t, c.Raw(func(raw any) error {
		batch, err := newDuckAppendBatch(ctx, raw, map[string][]columnSpec{"messages": {
			{"session_id", "session_id TEXT"},
			{"ordinal", "ordinal INTEGER"},
			{"role", "role TEXT"},
			{"content", "content TEXT"},
			{"is_system", "is_system BOOLEAN"},
			{"source_subtype", "source_subtype TEXT"},
		}})
		if err != nil {
			return err
		}
		for i, p := range prompts {
			ids[i] = fmt.Sprintf("fm-%04d", i)
			err := batch.add(ctx, "messages", ids[i], 0, cmp.Or(p.role, "user"),
				p.content, p.isSystem, p.sourceSubtype)
			if err != nil {
				return err
			}
		}
		return batch.flush(ctx)
	}))
	return ids
}

// seedMarkerPrompts writes the prompts into a fresh mirror file and
// returns a store over it with the session IDs. The store has no path, so
// the frustration memo stays out of the way.
func seedMarkerPrompts(t *testing.T, prompts []markerPrompt) (*Store, []string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	ids := writeMarkerPrompts(t, path, prompts)
	conn, err := OpenReadOnly(t.Context(), path)
	require.NoError(t, err)
	store := NewStoreFromDB(conn)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store, ids
}

// scanMarkers counts markers per session through the read the Signals
// panel runs for sessions without memoized counts.
func scanMarkers(t *testing.T, store *Store, ids []string) []int {
	t.Helper()
	rows := make([]db.SignalRow, len(ids))
	for i, id := range ids {
		rows[i].ID = id
	}
	require.NoError(t, store.duckPopulateFrustrationMarkers(t.Context(), rows, nil))
	counts := make([]int, len(rows))
	for i := range rows {
		counts[i] = rows[i].FrustrationMarkerCount
	}
	return counts
}

// shippedContent returns the sessions whose prompt content DuckDB sent
// back for Go to judge.
func shippedContent(t *testing.T, store *Store, ids []string) map[string]bool {
	t.Helper()
	rows, err := store.queryContext(t.Context(), duckFrustrationPromptsSQL, duckFrustrationPromptsArgs(ids)...)
	require.NoError(t, err)
	defer rows.Close()
	shipped := make(map[string]bool)
	for rows.Next() {
		var id, content string
		var decided bool
		require.NoError(t, rows.Scan(&id, &decided, &content))
		if decided {
			assert.Empty(t, content, "%s: judged prompt came back with content", id)
		} else {
			shipped[id] = true
		}
	}
	require.NoError(t, rows.Err())
	return shipped
}

// Prompts at each rule's edge. Plain prompts, ASCII plus
// signals.PlainPromptSymbols, are judged in DuckDB and never sent back;
// the rest are sent back with their content and judged in Go.
func TestDuckFrustrationMarkerScanCountsEdgeCases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		prompt markerPrompt
		want   int
		inGo   bool
	}{
		{name: "phrase", prompt: markerPrompt{content: "this is broken again"}, want: 1},
		{name: "phrase in capitals", prompt: markerPrompt{content: "THIS IS broken again, see"}, want: 1},
		{name: "phrase across word edges", prompt: markerPrompt{content: "the outcome online"}, want: 0},
		{name: "phrase split by a newline and a tab", prompt: markerPrompt{content: "why\n\twon't it build"}, want: 1},
		{name: "all caps", prompt: markerPrompt{content: "WHY IS THIS FAILING NOW"}, want: 1},
		{name: "caps ratio at threshold", prompt: markerPrompt{content: "THE api IS down ok"}, want: 1},
		{name: "caps ratio below threshold", prompt: markerPrompt{content: "THE api is down ok"}, want: 0},
		{name: "digits split caps words", prompt: markerPrompt{content: "BUILD1FAILED2again ok"}, want: 1},
		{name: "one letter words count only toward the word minimum", prompt: markerPrompt{content: "A B CD ef ..."}, want: 0},
		{name: "two long words are too few", prompt: markerPrompt{content: "A B ABC DEF ..."}, want: 0},
		{name: "normalized form too short", prompt: markerPrompt{content: "  NO  NO  NO  "}, want: 0},
		{name: "too short with punctuation", prompt: markerPrompt{content: "short!!!"}, want: 0},
		{name: "symbols count in bytes", prompt: markerPrompt{content: "✅✅✅ !!!"}, want: 1},
		{name: "spaced letters collapse below the minimum", prompt: markerPrompt{content: "A" + strings.Repeat(" ", 70) + "B C D!"}, want: 0},
		{name: "long blank start before a phrase", prompt: markerPrompt{content: strings.Repeat(" \n", 50) + "wtf is going on"}, want: 1},
		{name: "vertical tab and form feed collapse", prompt: markerPrompt{content: "come\v\fon then"}, want: 1},
		{name: "stripped fence joins a phrase", prompt: markerPrompt{content: "come```x```on, really"}, want: 1},
		{name: "fenced shouting is ignored", prompt: markerPrompt{content: "```\nWTF WTF WTF\n```\nplease look at it"}, want: 0},
		{name: "every fence is stripped", prompt: markerPrompt{content: "ok ```a``` fine ```WTF``` calm text here"}, want: 0},
		{name: "unclosed fence is kept", prompt: markerPrompt{content: "```\nWTF is going on here"}, want: 1},
		{name: "curly apostrophe is not the phrase", prompt: markerPrompt{content: "it doesn’t work at all"}, want: 0},
		{name: "em dash separates caps words", prompt: markerPrompt{content: "WAIT—STOP—NOW please"}, want: 1},
		{name: "check marks and repeated bangs", prompt: markerPrompt{content: "✅✅ tests pass now!!!"}, want: 1},
		{name: "emoji only around calm text", prompt: markerPrompt{content: "\U0001F680 shipped the release notes"}, want: 0},
		{name: "long s folds to s", prompt: markerPrompt{content: "this iſ broken again"}, want: 1, inGo: true},
		{name: "accented caps words", prompt: markerPrompt{content: "ÉCHEC TOTAL DU BUILD"}, want: 1, inGo: true},
		{name: "no-break space collapses", prompt: markerPrompt{content: "come on, fix it"}, want: 1, inGo: true},
		{name: "calm text in another script", prompt: markerPrompt{content: "中文 the build is green"}, want: 0, inGo: true},
		{name: "system row", prompt: markerPrompt{content: "this is broken again", isSystem: true}, want: 0},
		{name: "tool result", prompt: markerPrompt{content: "this is broken again", sourceSubtype: "tool_result"}, want: 0},
		{name: "assistant row", prompt: markerPrompt{content: "this is broken again", role: "assistant"}, want: 0},
	}
	prompts := make([]markerPrompt, len(cases))
	for i, c := range cases {
		prompts[i] = c.prompt
	}
	store, ids := seedMarkerPrompts(t, prompts)

	counts := scanMarkers(t, store, ids)
	shipped := shippedContent(t, store, ids)
	for i, c := range cases {
		assert.Equal(t, c.want, counts[i], c.name)
		assert.Equal(t, c.inGo, shipped[ids[i]], "%s: content sent back", c.name)
	}
}

// Random prompts built from ASCII words, whitespace, fences, and symbols,
// with some runes outside the plain set mixed in, must count exactly as
// signals.IsFrustrationMarker counts them, and only the prompts with such
// runes may come back with their content.
func TestDuckFrustrationMarkerScanMatchesGoOnRandomPrompts(t *testing.T) {
	t.Parallel()
	plain := []string{
		"the", "build", "fails", "again", "please", "fix", "it", "a", "I", "x",
		"WHY", "API", "OK", "NOW", "BROKEN", "STOP", "CI", "A1B", "X2",
		"come", "on", "come on", "wtf", "WTF", "doesn't", "does not", "work",
		"same", "error", "fucking", "you", "broke", "won't", "why", "this is broken",
		"still", "!", "!!", "!!!", "?", "??", "???", "'", "-", "_", "123", ".",
		"```", "—", "’", "…", "→", "✅", "❌", "\U0001F680",
		"§", "×", "─", "", "️",
	}
	separators := []string{" ", " ", " ", "  ", "\n", "\t", "\v", "\f", "\r\n", "", strings.Repeat(" ", 40)}
	// Runes outside the plain set: letters, spaces, and runes that
	// lowercase or fold to ASCII.
	other := []string{
		" ", "ſ", "İ", "K", "é", "ÉCHEC", "中文",
		" ", "ª", "Ⓐ",
	}
	rng := rand.New(rand.NewPCG(1, 2))
	const n = 3000
	prompts := make([]markerPrompt, n)
	hasOther := make([]bool, n)
	for i := range prompts {
		var b strings.Builder
		for range 1 + rng.IntN(24) {
			if rng.IntN(40) == 0 {
				b.WriteString(other[rng.IntN(len(other))])
				hasOther[i] = true
			} else {
				b.WriteString(plain[rng.IntN(len(plain))])
			}
			b.WriteString(separators[rng.IntN(len(separators))])
		}
		prompts[i].content = b.String()
	}
	store, ids := seedMarkerPrompts(t, prompts)

	counts := scanMarkers(t, store, ids)
	shipped := shippedContent(t, store, ids)
	var plainMarkers, plainCalm int
	for i, p := range prompts {
		want := 0
		if signals.IsFrustrationMarker(p.content) {
			want = 1
		}
		assert.Equal(t, want, counts[i], "prompt %q", p.content)
		assert.Equal(t, hasOther[i], shipped[ids[i]], "prompt %q: content sent back", p.content)
		if !hasOther[i] {
			if want == 1 {
				plainMarkers++
			} else {
				plainCalm++
			}
		}
	}
	// The corpus must exercise both verdicts of the DuckDB path.
	assert.Greater(t, plainMarkers, 300)
	assert.Greater(t, plainCalm, 300)
}

// The marker SQL relies on DuckDB treating every plain symbol as Go does:
// lower and upper leave it alone, and (?i) folds none to an ASCII letter.
// DuckDB's case tables come from utf8proc, so a DuckDB upgrade that
// changes one fails here.
func TestDuckPlainPromptSymbolsAreInertInDuckDB(t *testing.T) {
	t.Parallel()
	runes := signals.PlainPromptSymbolRunes()
	symbols := make([]string, len(runes))
	for i, r := range runes {
		symbols[i] = string(r)
	}
	duck := openTestDuckDB(t)
	rows, err := duck.QueryContext(t.Context(), `
		SELECT s FROM (SELECT unnest(?) AS s)
		WHERE lower(s) <> s OR upper(s) <> s OR regexp_matches(s, '(?i)[a-z]')
			OR strlen(s) <> octet_length(encode(s))`, symbols)
	require.NoError(t, err)
	defer rows.Close()
	var changed []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		changed = append(changed, fmt.Sprintf("U+%04X", []rune(s)[0]))
	}
	require.NoError(t, rows.Err())
	assert.Empty(t, changed)
}
