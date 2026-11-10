//go:build pgtest

package postgres

import (
	"cmp"
	"context"
	"fmt"
	"math/rand/v2"
	"regexp"
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

// seedMarkerPrompts writes each prompt as session fm-<index> into a fresh
// schema of the database at url and returns a store over it with the
// session IDs.
func seedMarkerPrompts(t *testing.T, url, schema string, prompts []markerPrompt) (*Store, []string) {
	t.Helper()
	ctx := t.Context()
	cleanNamedPGSchema(t, url, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, url, schema) })
	pg, err := Open(ctx, url, schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pg.Close()) })
	require.NoError(t, EnsureSchema(ctx, pg, schema))
	tx, err := pg.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	ids := make([]string, len(prompts))
	for i, p := range prompts {
		ids[i] = fmt.Sprintf("fm-%04d", i)
		_, err := tx.ExecContext(ctx,
			`INSERT INTO sessions (id, machine, project, agent) VALUES ($1, 'm', 'p', 'claude')`, ids[i])
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx,
			`INSERT INTO messages (session_id, ordinal, role, content, is_system, source_subtype)
			 VALUES ($1, 0, $2, $3, $4, $5)`,
			ids[i], cmp.Or(p.role, "user"), p.content, p.isSystem, p.sourceSubtype)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
	store, err := NewStore(ctx, url, schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store, ids
}

// scanMarkers counts markers per session through the read the Signals
// panel runs for sessions the memo does not hold.
func scanMarkers(t *testing.T, store *Store, ids []string) []int {
	t.Helper()
	ctx := t.Context()
	versionColumn, err := sessionVersionColumnPG(ctx, store.pg)
	require.NoError(t, err)
	rows := make([]db.SignalRow, len(ids))
	for i, id := range ids {
		rows[i].ID = id
	}
	require.NoError(t, store.populateFrustrationMarkers(ctx, rows, nil, versionColumn))
	counts := make([]int, len(rows))
	for i := range rows {
		counts[i] = rows[i].FrustrationMarkerCount
	}
	return counts
}

// shippedContent returns the sessions whose prompt content the database
// sent back for Go to judge.
func shippedContent(t *testing.T, store *Store, ids []string) map[string]bool {
	t.Helper()
	ctx := t.Context()
	versionColumn, err := sessionVersionColumnPG(ctx, store.pg)
	require.NoError(t, err)
	rows, err := store.pg.QueryContext(ctx, pgFrustrationPrompts(versionColumn),
		append([]any{ids}, pgFrustrationPromptsArgs...)...)
	require.NoError(t, err)
	defer rows.Close()
	shipped := make(map[string]bool)
	for rows.Next() {
		var id, content string
		var version *string
		var decided bool
		require.NoError(t, rows.Scan(&id, &version, &decided, &content))
		if version == nil && !decided {
			shipped[id] = true
		}
	}
	require.NoError(t, rows.Err())
	return shipped
}

// codeFence strips fenced code as signals.IsFrustrationMarker does.
var codeFence = regexp.MustCompile(signals.CodeFencePattern)

// longPrompt repeats line until the prompt is longer than the stripped
// prompts SQL counts exactly, so only the bound can judge its caps ratio.
func longPrompt(line string) string {
	return strings.Repeat(line, pgExactCapsMaxBytes/len(line)+1)
}

// Prompts at each rule's edge. Plain prompts, ASCII plus the symbols in
// signals.PlainPromptSymbols, are judged in SQL and never sent back, except
// long prompts whose caps ratio the bound cannot rule out; the rest are
// sent back with their content and judged in Go.
func TestFrustrationMarkerScanCountsEdgeCases(t *testing.T) {
	cases := []struct {
		name   string
		prompt markerPrompt
		want   int
		inGo   bool
	}{
		{name: "phrase", prompt: markerPrompt{content: "this is broken again"}, want: 1},
		{name: "phrase in capitals", prompt: markerPrompt{content: "This Is Broken, please look"}, want: 1},
		{name: "phrase across word edges", prompt: markerPrompt{content: "the outcome online"}, want: 0},
		{name: "phrase inside a word", prompt: markerPrompt{content: "wtfx is a module name"}, want: 0},
		{name: "underscore is a word character", prompt: markerPrompt{content: "see _wtf_ in the logs"}, want: 0},
		{name: "digit is a word character", prompt: markerPrompt{content: "see 1wtf in the logs"}, want: 0},
		{name: "parenthesis is a boundary", prompt: markerPrompt{content: "(wtf) happened here"}, want: 1},
		{name: "control character is a boundary", prompt: markerPrompt{content: "wtf\x01 is going on"}, want: 1},
		{name: "tab inside a phrase", prompt: markerPrompt{content: "same\terror as before"}, want: 1},
		{name: "vertical tab and form feed collapse", prompt: markerPrompt{content: "come\v\fon then"}, want: 1},
		{name: "spaced bangs are not repeated", prompt: markerPrompt{content: "! ! ! are you sure"}, want: 0},
		{name: "repeated question marks", prompt: markerPrompt{content: "did it pass???"}, want: 1},
		{name: "all caps", prompt: markerPrompt{content: "WHY IS THIS FAILING NOW"}, want: 1},
		{name: "caps ratio at threshold", prompt: markerPrompt{content: "THE api IS down ok"}, want: 1},
		{name: "caps ratio below threshold", prompt: markerPrompt{content: "THE api is down ok"}, want: 0},
		{name: "caps with exactly the minimum of long words", prompt: markerPrompt{content: "THE API FAILS"}, want: 1},
		{name: "caps with too few long words", prompt: markerPrompt{content: "AB CD e f g h"}, want: 0},
		{name: "mixed case is not caps", prompt: markerPrompt{content: "JSONParser HTTPServer APIClient"}, want: 0},
		{name: "one letter words count only toward the word minimum", prompt: markerPrompt{content: "A B CD ef ..."}, want: 0},
		{name: "normalized form too short", prompt: markerPrompt{content: "  NO  NO  NO  "}, want: 0},
		{name: "too short with punctuation", prompt: markerPrompt{content: "short!!!"}, want: 0},
		{name: "nine bytes", prompt: markerPrompt{content: "wtf!!!!!!"}, want: 0},
		{name: "spaces lengthen a short phrase", prompt: markerPrompt{content: "wtf a b c d"}, want: 1},
		{name: "spaces leave a phrase short", prompt: markerPrompt{content: "\twtf a b c\n"}, want: 0},
		{name: "length counts bytes", prompt: markerPrompt{content: "wtf ——"}, want: 1},
		{name: "stripped fence joins a phrase", prompt: markerPrompt{content: "come```x```on, really"}, want: 1},
		{name: "fenced shouting is ignored", prompt: markerPrompt{content: "```\nWTF WTF WTF\n```\nplease look at it"}, want: 0},
		{name: "unclosed fence is kept", prompt: markerPrompt{content: "```\nWTF is going on here"}, want: 1},
		{name: "curly apostrophe is not the phrase", prompt: markerPrompt{content: "it doesn’t work at all"}, want: 0},
		{name: "em dash separates caps words", prompt: markerPrompt{content: "WAIT—STOP—NOW please"}, want: 1},
		{name: "check marks and repeated bangs", prompt: markerPrompt{content: "✅✅ tests pass now!!!"}, want: 1},
		{name: "emoji only around calm text", prompt: markerPrompt{content: "\U0001F680 shipped the release notes"}, want: 0},
		{name: "long calm prompt", prompt: markerPrompt{content: longPrompt("INFO the request finished in time for the next stage of the build pipeline\n")}, want: 0},
		{name: "long shouting prompt", prompt: markerPrompt{content: longPrompt("ERROR THE BUILD FAILED AGAIN ON CI\n")}, want: 1, inGo: true},
		{name: "long prompt at the caps bound", prompt: markerPrompt{content: longPrompt("ERROR API FAILED on the build\n")}, want: 1, inGo: true},
		{name: "long prompt with one capital word", prompt: markerPrompt{content: "OK " + longPrompt("the build is a fail on x\n")}, want: 0},
		{name: "long prompt the bound cannot rule out", prompt: markerPrompt{content: longPrompt("ERROR API the build is a fail on x\n")}, want: 0, inGo: true},
		{name: "long prompt with a phrase", prompt: markerPrompt{content: longPrompt("ERROR API timeout on the build\n") + "this is broken"}, want: 1},
		{name: "long fenced paste around a short prompt", prompt: markerPrompt{content: "```\n" + longPrompt("x := 1\n") + "```\nWHY IS THIS FAILING"}, want: 1},
		{name: "long s folds to s", prompt: markerPrompt{content: "this iſ broken again"}, want: 1, inGo: true},
		{name: "accented caps words", prompt: markerPrompt{content: "ÉCHEC TOTAL DU BUILD"}, want: 1, inGo: true},
		{name: "no-break space collapses", prompt: markerPrompt{content: "come on, fix it"}, want: 1, inGo: true},
		{name: "Kelvin sign folds to k", prompt: markerPrompt{content: "fucK this build"}, want: 1, inGo: true},
		{name: "system row", prompt: markerPrompt{content: "this is broken again", isSystem: true}, want: 0},
		{name: "tool result", prompt: markerPrompt{content: "this is broken again", sourceSubtype: "tool_result"}, want: 0},
		{name: "assistant row", prompt: markerPrompt{content: "this is broken again", role: "assistant"}, want: 0},
	}
	prompts := make([]markerPrompt, len(cases))
	for i, c := range cases {
		prompts[i] = c.prompt
	}
	store, ids := seedMarkerPrompts(t, testPGURL(t), "agentsview_frustration_edges", prompts)

	counts := scanMarkers(t, store, ids)
	shipped := shippedContent(t, store, ids)
	for i, c := range cases {
		assert.Equal(t, c.want, counts[i], c.name)
		assert.Equal(t, c.inGo, shipped[ids[i]], "%s: content sent back", c.name)
	}
}

// Random prompts built from ASCII words, whitespace, fences, and symbols,
// with some runes outside the plain set mixed in, must count exactly as
// signals.IsFrustrationMarker counts them. Only prompts with such runes, or
// stripped prompts too long to count exactly, may come back with content.
func TestFrustrationMarkerScanMatchesGoOnRandomPrompts(t *testing.T) {
	plain := []string{
		"the", "build", "fails", "again", "please", "fix", "it", "a", "I", "x", "_", "x1", "9",
		"WHY", "API", "OK", "NOW", "BROKEN", "STOP", "CI", "JSONParser",
		"come", "on", "come on", "Come On", "wtf", "WTF", "doesn't", "DOESN'T", "does not", "work",
		"same", "error", "fucking", "you", "broke", "won't", "why", "this is broken", "still",
		"!", "!!", "!!!", "?", "??", "???", "'", "-", "123", ".", "(", ")",
		"```", "—", "’", "…", "→", "✅", "❌", "\U0001F680",
		"§", "×", "─", "", "️", "\x01", "\x7f",
	}
	separators := []string{" ", " ", " ", "  ", "\n", "\t", "\v", "\f", "\r\n", ""}
	// Runes outside the plain set: letters, spaces, and runes that lowercase
	// or fold to ASCII.
	other := []string{
		" ", "ſ", "İ", "K", "é", "ÉCHEC", "中文",
		" ", "ª", " ",
	}
	// Lines for long prompts: calm, shouting, and in between.
	lines := []string{
		"INFO the request finished in time for the next stage of the build\n",
		"ERROR THE BUILD FAILED AGAIN ON CI\n",
		"ERROR API timeout on the build\n",
		"ERROR API the build is a fail on x\n",
		"ERROR API FAILED on the build\n",
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
		if rng.IntN(4) == 0 {
			// A long paste, sometimes fenced, around the short prompt.
			paste := strings.Repeat(lines[rng.IntN(len(lines))], 500+rng.IntN(500))
			if rng.IntN(2) == 0 {
				paste = "```\n" + paste + "```\n"
			}
			prompts[i].content = paste + b.String()
		} else {
			prompts[i].content = b.String()
		}
	}
	store, ids := seedMarkerPrompts(t, testPGURL(t), "agentsview_frustration_random", prompts)

	counts := scanMarkers(t, store, ids)
	shipped := shippedContent(t, store, ids)
	var sqlMarkers, sqlCalm, longInSQL, longInGo int
	for i, p := range prompts {
		want := 0
		if signals.IsFrustrationMarker(p.content) {
			want = 1
		}
		assert.Equal(t, want, counts[i], "prompt %q", p.content)
		long := len(codeFence.ReplaceAllString(p.content, " ")) > pgExactCapsMaxBytes
		if hasOther[i] {
			assert.True(t, shipped[ids[i]], "prompt %q: content sent back", p.content)
			continue
		}
		if !long {
			assert.False(t, shipped[ids[i]], "prompt %q: content sent back", p.content)
		}
		switch {
		case long && shipped[ids[i]]:
			longInGo++
		case long:
			longInSQL++
		case want == 1:
			sqlMarkers++
		default:
			sqlCalm++
		}
	}
	// The corpus must exercise each verdict of the SQL path.
	assert.Greater(t, sqlMarkers, 300)
	assert.Greater(t, sqlCalm, 300)
	assert.Greater(t, longInSQL, 20)
	assert.Greater(t, longInGo, 20)
}

// A LATIN1 database reached with a UTF8 client encoding converts every
// bound parameter to LATIN1 before the statement runs, so the read must not
// bind the plain-alphabet pattern there. Go judges every prompt instead.
func TestFrustrationMarkerScanOnLatin1Database(t *testing.T) {
	admin, err := Open(t.Context(), testPGURL(t), "public", false)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, admin.Close()) })
	database := fmt.Sprintf("frustration_latin1_%x", rand.Uint64())
	if _, err = admin.ExecContext(t.Context(),
		`CREATE DATABASE `+database+` ENCODING 'LATIN1' LC_COLLATE 'C' LC_CTYPE 'C' TEMPLATE template0`); err != nil {
		t.Skipf("cannot create a LATIN1 database: %v", err)
	}
	t.Cleanup(func() {
		_, err := admin.ExecContext(context.WithoutCancel(t.Context()), `DROP DATABASE IF EXISTS `+database+` WITH (FORCE)`)
		assert.NoError(t, err)
	})
	dsn := mustParseURL(t, testPGURL(t))
	dsn.Path = "/" + database
	query := dsn.Query()
	query.Set("client_encoding", "UTF8")
	dsn.RawQuery = query.Encode()

	store, ids := seedMarkerPrompts(t, dsn.String(), "agentsview_frustration_latin1", []markerPrompt{
		{content: "this is broken again"},
		{content: "THE api is down ok"},
		{content: "ÉCHEC TOTAL DU BUILD"},
		{content: "this is broken again", isSystem: true},
	})

	assert.Equal(t, []int{1, 0, 1, 0}, scanMarkers(t, store, ids))
}
