//go:build chtest

package clickhouse

import (
	"cmp"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/clickhouse/chtest"
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

// seedMarkerPrompts writes each prompt as session fm-<index> into a fresh
// mirror and returns a store over it with the session IDs.
func seedMarkerPrompts(t *testing.T, prompts []markerPrompt) (*Store, []string) {
	t.Helper()
	ctx := t.Context()
	dsn, database := chtest.FreshDatabase(t)
	target := Target{URL: dsn, Database: database}
	require.NoError(t, EnsureSchema(ctx, target))
	store, err := NewStore(ctx, target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	tx, err := store.DB().BeginTx(ctx, nil)
	require.NoError(t, err)
	stmt, err := tx.PrepareContext(ctx,
		"INSERT INTO messages (session_id, ordinal, role, content, is_system, source_subtype)")
	require.NoError(t, err)
	ids := make([]string, len(prompts))
	for i, p := range prompts {
		ids[i] = fmt.Sprintf("fm-%04d", i)
		role := cmp.Or(p.role, "user")
		_, err := stmt.ExecContext(ctx, ids[i], 0, role, p.content, p.isSystem, p.sourceSubtype)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
	return store, ids
}

// scanMarkers counts markers per session through the read the Signals
// panel runs for sessions without stored counts.
func scanMarkers(t *testing.T, store *Store, ids []string) []int {
	t.Helper()
	rows := make([]db.SignalRow, len(ids))
	for i, id := range ids {
		rows[i].ID = id
	}
	require.NoError(t, store.chPopulateFrustrationMarkers(t.Context(), rows, nil))
	counts := make([]int, len(rows))
	for i := range rows {
		counts[i] = rows[i].FrustrationMarkerCount
	}
	return counts
}

// shippedContent returns the sessions whose prompt content ClickHouse sent
// back for Go to judge.
func shippedContent(t *testing.T, store *Store, ids []string) map[string]bool {
	t.Helper()
	ctx, ph, err := analyticsSessionIDsContext(t.Context(), ids)
	require.NoError(t, err)
	rows, err := store.queryContext(ctx, fmt.Sprintf(chFrustrationPromptsSQL, ph), chFrustrationPromptsArgs...)
	require.NoError(t, err)
	defer rows.Close()
	shipped := make(map[string]bool)
	for rows.Next() {
		var id, content string
		var decided bool
		require.NoError(t, rows.Scan(&id, &decided, &content))
		if !decided {
			shipped[id] = true
		}
	}
	require.NoError(t, rows.Err())
	return shipped
}

// Prompts at each rule's edge. Plain prompts, ASCII plus the symbols in
// signals.PlainPromptSymbols, are judged in ClickHouse and never sent back;
// the rest are sent back with their content and judged in Go.
func TestFrustrationMarkerScanCountsEdgeCases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		prompt markerPrompt
		want   int
		inGo   bool
	}{
		{name: "phrase", prompt: markerPrompt{content: "this is broken again"}, want: 1},
		{name: "phrase across word edges", prompt: markerPrompt{content: "the outcome online"}, want: 0},
		{name: "all caps", prompt: markerPrompt{content: "WHY IS THIS FAILING NOW"}, want: 1},
		{name: "caps ratio at threshold", prompt: markerPrompt{content: "THE api IS down ok"}, want: 1},
		{name: "caps ratio below threshold", prompt: markerPrompt{content: "THE api is down ok"}, want: 0},
		{name: "one letter words count only toward the word minimum", prompt: markerPrompt{content: "A B CD ef ..."}, want: 0},
		{name: "normalized form too short", prompt: markerPrompt{content: "  NO  NO  NO  "}, want: 0},
		{name: "too short with punctuation", prompt: markerPrompt{content: "short!!!"}, want: 0},
		{name: "vertical tab and form feed collapse", prompt: markerPrompt{content: "come\v\fon then"}, want: 1},
		{name: "stripped fence joins a phrase", prompt: markerPrompt{content: "come```x```on, really"}, want: 1},
		{name: "fenced shouting is ignored", prompt: markerPrompt{content: "```\nWTF WTF WTF\n```\nplease look at it"}, want: 0},
		{name: "unclosed fence is kept", prompt: markerPrompt{content: "```\nWTF is going on here"}, want: 1},
		{name: "curly apostrophe is not the phrase", prompt: markerPrompt{content: "it doesn\u2019t work at all"}, want: 0},
		{name: "em dash separates caps words", prompt: markerPrompt{content: "WAIT\u2014STOP\u2014NOW please"}, want: 1},
		{name: "check marks and repeated bangs", prompt: markerPrompt{content: "\u2705\u2705 tests pass now!!!"}, want: 1},
		{name: "emoji only around calm text", prompt: markerPrompt{content: "\U0001F680 shipped the release notes"}, want: 0},
		{name: "long s folds to s", prompt: markerPrompt{content: "this i\u017f broken again"}, want: 1, inGo: true},
		{name: "accented caps words", prompt: markerPrompt{content: "\u00c9CHEC TOTAL DU BUILD"}, want: 1, inGo: true},
		{name: "no-break space collapses", prompt: markerPrompt{content: "come\u00a0on, fix it"}, want: 1, inGo: true},
		{name: "invalid UTF-8", prompt: markerPrompt{content: "wtf is this \xff"}, want: 1, inGo: true},
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
func TestFrustrationMarkerScanMatchesGoOnRandomPrompts(t *testing.T) {
	t.Parallel()
	plain := []string{
		"the", "build", "fails", "again", "please", "fix", "it", "a", "I", "x",
		"WHY", "API", "OK", "NOW", "BROKEN", "STOP", "CI",
		"come", "on", "come on", "wtf", "WTF", "doesn't", "does not", "work",
		"same", "error", "fucking", "you", "broke", "won't", "why", "this is broken",
		"still", "!", "!!", "!!!", "?", "??", "???", "'", "-", "_", "123", ".",
		"```", "\u2014", "\u2019", "\u2026", "\u2192", "\u2705", "\u274c", "\U0001F680",
		"\u00a7", "\u00d7", "\u2500", "\ue000", "\ufe0f",
	}
	separators := []string{" ", " ", " ", "  ", "\n", "\t", "\v", "\f", "\r\n", ""}
	// Runes outside the plain set: letters, spaces, a rune that lowercases
	// to ASCII, and an invalid byte.
	other := []string{
		"\u00a0", "\u017f", "\u0130", "\u212a", "\u00e9", "\u00c9CHEC", "\u4e2d\u6587",
		"\u2003", "\u00aa", "\xff",
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
	// The corpus must exercise both verdicts of the ClickHouse path.
	assert.Greater(t, plainMarkers, 300)
	assert.Greater(t, plainCalm, 300)
}

// A push republishes a changed session under a newer version, and the older
// rows stay until a merge collapses them. Only the newest version of each
// message counts, and a version written twice counts once.
func TestFrustrationMarkerScanReadsNewestMessageVersion(t *testing.T) {
	t.Parallel()
	store, _ := seedMarkerPrompts(t, nil)
	// Keep every version in its own part, as before a merge.
	_, err := store.DB().ExecContext(t.Context(), "SYSTEM STOP MERGES messages")
	require.NoError(t, err)
	insert := func(id string, version uint64, content string, isSystem bool) {
		t.Helper()
		_, err := store.DB().ExecContext(t.Context(), `INSERT INTO messages
			(session_id, ordinal, role, content, is_system, push_version) VALUES (?, 0, 'user', ?, ?, ?)`,
			id, content, isSystem, version)
		require.NoError(t, err)
	}
	const marker, calm = "this is broken again", "please add the export"
	insert("now-system", 1, marker, false)
	insert("now-system", 2, marker, true)
	insert("now-marker", 1, calm, false)
	insert("now-marker", 2, marker, false)
	insert("now-calm", 1, marker, false)
	insert("now-calm", 2, calm, false)
	insert("written-twice", 1, marker, false)
	insert("written-twice", 1, marker, false)

	ids := []string{"now-system", "now-marker", "now-calm", "written-twice"}
	assert.Equal(t, []int{0, 1, 0, 1}, scanMarkers(t, store, ids))
}
