package why

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/service"
	"hegel.dev/go/hegel"
)

func recordedLink(sha, id, prompt string) service.CommitSessionLink {
	zero := 0
	return service.CommitSessionLink{SessionID: id, CommitSHA: sha, Source: "transcript", Resolved: true, MessageOrdinal: &zero, CallIndex: &zero,
		Session: &service.CommitSession{ID: id}, Agent: "codex", Project: "example", Turn: &service.CommitTurn{Ordinal: 0, Prompt: prompt, Why: "commit-call"}}
}
func TestExplainKeepsFirstCardAndCapsSessions(t *testing.T) {
	a := recordedLink(firstSHA, "codex:one", "prompt A #123")
	trailer := a
	trailer.Source = "trailer"
	trailer.Turn = &service.CommitTurn{Prompt: "hidden #999", Why: "before"}
	b := recordedLink(secondSHA, "codex:one", "prompt B #456")
	c := recordedLink(firstSHA, "codex:two", "")
	c.Turn = nil
	gone := service.CommitSessionLink{SessionID: "codex:gone", CommitSHA: firstSHA, Source: "trailer", Gone: true}
	in := &Inspection{Lines: []Line{}, Commits: []Commit{{SHA: firstSHA, Message: "Fix project#abcd"}, {SHA: secondSHA}}, Older: 9}
	calls := 0
	result, err := Explain(t.Context(), in, func(ctx context.Context, shas []string) (map[string][]service.CommitSessionLink, error) {
		calls++
		assert.Equal(t, []string{firstSHA, secondSHA}, shas)
		return map[string][]service.CommitSessionLink{firstSHA: {a, trailer, a, c, gone, recordedLink(firstSHA, "codex:four", "not admitted #777")}, secondSHA: {b}}, nil
	})
	require.NoError(t, err)
	assert.Equal(t, 1, calls)
	assert.True(t, result.Explained)
	assert.Equal(t, 9, result.Older)
	require.Len(t, result.Refs, 5)
	assert.Equal(t, a, result.Refs[0].Card)
	assert.Equal(t, "session:codex:one", result.Refs[0].Key)
	assert.Contains(t, result.Refs[0].FoundBy, firstSHA)
	assert.Contains(t, result.Refs[0].FoundBy, secondSHA)
	assert.Equal(t, "issue:project#abcd", result.Refs[3].Key)
	assert.Equal(t, "issue:#123", result.Refs[4].Key)
	require.Len(t, result.Steps, 3)
	assert.Equal(t, 3, result.Steps[1].Found)
	assert.Equal(t, 2, result.Steps[2].Found)
	var out bytes.Buffer
	require.NoError(t, WriteHuman(&out, result))
	text := out.String()
	assert.Contains(t, text, "recorded commit call")
	assert.Contains(t, text, "gone")
	assert.Contains(t, text, "prompt A")
	assert.NotContains(t, text, "prompt B")
	assert.Equal(t, 1, strings.Count(text, "prompt A"))
	assert.Contains(t, text, "see r1")
}
func TestExplainUncertainAndNoRecord(t *testing.T) {
	for _, link := range []service.CommitSessionLink{
		{SessionID: "gone", CommitSHA: firstSHA, Source: "trailer", Gone: true},
		func() service.CommitSessionLink {
			l := recordedLink(firstSHA, "ambiguous", "maybe")
			l.Ambiguous = true
			return l
		}(),
	} {
		result, err := Explain(t.Context(), &Inspection{Question: Question{Commit: firstSHA}, Commits: []Commit{{SHA: firstSHA}}}, func(context.Context, []string) (map[string][]service.CommitSessionLink, error) {
			return map[string][]service.CommitSessionLink{firstSHA: {link}}, nil
		})
		require.NoError(t, err)
		assert.False(t, result.Explained)
		var out bytes.Buffer
		require.NoError(t, WriteHuman(&out, result))
		assert.Contains(t, out.String(), "No record explains this commit.")
	}
	sentinel := errors.New("archive down")
	_, err := Explain(t.Context(), &Inspection{Commits: []Commit{{SHA: firstSHA}}}, func(context.Context, []string) (map[string][]service.CommitSessionLink, error) { return nil, sentinel })
	assert.ErrorIs(t, err, sentinel)
	result, err := Explain(t.Context(), &Inspection{}, func(context.Context, []string) (map[string][]service.CommitSessionLink, error) {
		t.Fatal("must skip lookup")
		return nil, nil
	})
	require.NoError(t, err)
	assert.False(t, result.Explained)
	require.Len(t, result.Steps, 3)
	assert.Equal(t, "skipped: no committed lines", result.Steps[1].Asked)
	raw, err := json.Marshal(result)
	require.NoError(t, err)
	var object map[string]any
	require.NoError(t, json.Unmarshal(raw, &object))
	assert.Len(t, object, 6)
	assert.Equal(t, []any{}, object["lines"])
	assert.Equal(t, []any{}, object["refs"])
	for _, step := range object["steps"].([]any) {
		assert.Nil(t, step.(map[string]any)["error"])
	}
}
func TestExplainGeneratedFirstCards(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		ids := hegel.Draw(ht, hegel.Lists(hegel.Integers(0, 12)).MaxSize(100))
		links := make([]service.CommitSessionLink, 0, len(ids))
		expected := []string{}
		seen := map[int]bool{}
		for i, id := range ids {
			key := fmt.Sprintf("codex:%d", id)
			links = append(links, recordedLink(firstSHA, key, fmt.Sprintf("prompt %d", i)))
			if !seen[id] && len(expected) < 3 {
				expected = append(expected, key)
				seen[id] = true
			}
		}
		result, err := Explain(t.Context(), &Inspection{Commits: []Commit{{SHA: firstSHA}}}, func(context.Context, []string) (map[string][]service.CommitSessionLink, error) {
			return map[string][]service.CommitSessionLink{firstSHA: links}, nil
		})
		require.NoError(ht, err)
		require.Len(ht, result.Refs, len(expected))
		for i, id := range expected {
			ref := result.Refs[i]
			assert.Equal(ht, fmt.Sprintf("r%d", i+1), ref.ID)
			assert.Equal(ht, "session:"+id, ref.Key)
			for _, link := range links {
				if link.SessionID == id {
					assert.Equal(ht, link, ref.Card)
					break
				}
			}
		}
		raw, err := json.Marshal(result)
		require.NoError(ht, err)
		var decoded map[string]any
		require.NoError(ht, json.Unmarshal(raw, &decoded))
		assert.Len(ht, decoded, 6)
		assert.Equal(ht, len(expected), len(decoded["refs"].([]any)))
	}, hegel.WithTestCases(2000))
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("output failed") }
func TestHumanOutputSanitizesFieldsAndPropagatesFailure(t *testing.T) {
	link := recordedLink(firstSHA, "codex:test", "a\x1b]0;title\x07\n\u009b\u202e"+strings.Repeat("界", 700))
	link.CommitSHA = "\x1bmalicioushash"
	result, err := Explain(t.Context(), &Inspection{Commits: []Commit{{SHA: firstSHA, Author: "bad\rname", Subject: "subject\x1b"}}}, func(context.Context, []string) (map[string][]service.CommitSessionLink, error) {
		return map[string][]service.CommitSessionLink{firstSHA: {link}}, nil
	})
	require.NoError(t, err)
	var out bytes.Buffer
	require.NoError(t, WriteHuman(&out, result))
	text := out.String()
	for _, bad := range []string{"\x1b", "\x07", "\r", "\u009b", "\u202e"} {
		assert.NotContains(t, text, bad)
	}
	assert.LessOrEqual(t, strings.Count(text, "界"), 600)
	assert.ErrorContains(t, WriteHuman(failingWriter{}, result), "output failed")
}

func TestExplainFirstAmbiguousCardRemainsUncertain(t *testing.T) {
	first := recordedLink(firstSHA, "codex:one", "first")
	first.Ambiguous = true
	later := recordedLink(secondSHA, "codex:one", "later")
	result, err := Explain(t.Context(), &Inspection{Commits: []Commit{{SHA: firstSHA}, {SHA: secondSHA}}}, func(context.Context, []string) (map[string][]service.CommitSessionLink, error) {
		return map[string][]service.CommitSessionLink{firstSHA: {first}, secondSHA: {later}}, nil
	})
	require.NoError(t, err)
	require.Len(t, result.Refs, 1)
	assert.Equal(t, first, result.Refs[0].Card)
	assert.False(t, result.Explained)
}

func TestExplainMarksAmbiguousLaterAssociation(t *testing.T) {
	first := recordedLink(firstSHA, "codex:one", "first")
	later := recordedLink(secondSHA, "codex:one", "later")
	later.Ambiguous = true
	result, err := Explain(t.Context(), &Inspection{Commits: []Commit{{SHA: firstSHA}, {SHA: secondSHA}}}, func(context.Context, []string) (map[string][]service.CommitSessionLink, error) {
		return map[string][]service.CommitSessionLink{firstSHA: {first}, secondSHA: {later}}, nil
	})
	require.NoError(t, err)
	require.Len(t, result.Refs, 1)
	assert.Equal(t, first, result.Refs[0].Card)
	assert.True(t, result.Explained)
	assert.Contains(t, result.Refs[0].FoundBy, secondSHA)
	assert.Contains(t, result.Refs[0].FoundBy, "ambiguous evidence")
	raw, err := json.Marshal(result)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "ambiguous evidence")
	var out bytes.Buffer
	require.NoError(t, WriteHuman(&out, result))
	assert.Contains(t, out.String(), "Session codex:one: see r1 [ambiguous evidence]")
}

func TestExplanationMatchesCanonicalAvailability(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		available := hegel.Draw(ht, hegel.Booleans())
		gone := !available
		ambiguous := hegel.Draw(ht, hegel.Booleans())
		hasTurn := hegel.Draw(ht, hegel.Booleans())
		first := recordedLink(firstSHA, "codex:one", "canonical")
		if !available {
			first.Session = nil
		}
		first.Gone = gone
		first.Ambiguous = ambiguous
		if !hasTurn {
			first.Turn = nil
		}
		later := recordedLink(secondSHA, "codex:one", "later")
		result, err := Explain(t.Context(), &Inspection{Commits: []Commit{{SHA: firstSHA}, {SHA: secondSHA}}}, func(context.Context, []string) (map[string][]service.CommitSessionLink, error) {
			return map[string][]service.CommitSessionLink{firstSHA: {first}, secondSHA: {later}}, nil
		})
		require.NoError(ht, err)
		assert.Equal(ht, available && !gone && !ambiguous, result.Explained)
	}, hegel.WithTestCases(2000))
}
func TestAssociationsConserveDistinctCommitSourcePairs(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		total := hegel.Draw(ht, hegel.Integers(1, 6))
		duplicates := hegel.Draw(ht, hegel.Integers(1, 100))
		twoSources := hegel.Draw(ht, hegel.Booleans())
		in := &Inspection{}
		rows := map[string][]service.CommitSessionLink{}
		for n := 0; n < total; n++ {
			sha := fmt.Sprintf("%040x", n+1)
			in.Commits = append(in.Commits, Commit{SHA: sha})
			for d := 0; d < duplicates; d++ {
				link := recordedLink(sha, "codex:one", "context")
				rows[sha] = append(rows[sha], link)
				if twoSources {
					link.Source = "trailer"
					rows[sha] = append(rows[sha], link)
				}
			}
		}
		result, err := Explain(t.Context(), in, func(context.Context, []string) (map[string][]service.CommitSessionLink, error) { return rows, nil })
		require.NoError(ht, err)
		require.Len(ht, result.Refs, 1)
		pairs := 1
		if twoSources {
			pairs = 2
		}
		for _, commit := range in.Commits {
			assert.Equal(ht, pairs, strings.Count(result.Refs[0].FoundBy, commit.SHA))
		}
		assert.Equal(ht, total, strings.Count(result.Refs[0].FoundBy, "transcript"))
		if twoSources {
			assert.Equal(ht, total, strings.Count(result.Refs[0].FoundBy, "trailer"))
		}
	}, hegel.WithTestCases(2000))
}
func TestCanonicalPromptNeverMovesAcrossCommits(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		tag := hegel.Draw(ht, hegel.Text().Alphabet("abcdef0123456789").MinSize(1).MaxSize(40))
		hasTurn := hegel.Draw(ht, hegel.Booleans())
		a := recordedLink(firstSHA, "codex:one", "canonical-prompt-"+tag)
		b := recordedLink(secondSHA, "codex:one", "other-prompt-"+tag)
		if !hasTurn {
			a.Turn = nil
		}
		result, err := Explain(t.Context(), &Inspection{Commits: []Commit{{SHA: firstSHA}, {SHA: secondSHA}}}, func(context.Context, []string) (map[string][]service.CommitSessionLink, error) {
			return map[string][]service.CommitSessionLink{firstSHA: {a}, secondSHA: {b}}, nil
		})
		require.NoError(ht, err)
		var out bytes.Buffer
		require.NoError(ht, WriteHuman(&out, result))
		want := 0
		if hasTurn {
			want = 1
		}
		assert.Equal(ht, want, strings.Count(out.String(), "canonical-prompt-"+tag))
		assert.NotContains(ht, out.String(), "other-prompt-"+tag)
	}, hegel.WithTestCases(2000))
}

type partialFailureWriter struct {
	limit   int
	written bytes.Buffer
	err     error
}

func (w *partialFailureWriter) Write(p []byte) (int, error) {
	n := min(w.limit, len(p))
	_, _ = w.written.Write(p[:n])
	w.limit -= n
	if w.limit > 0 {
		return n, nil
	}
	return n, w.err
}
func TestOutputFailuresPropagateAtEveryOffset(t *testing.T) {
	result, err := Explain(t.Context(), &Inspection{}, nil)
	require.NoError(t, err)
	hegel.Test(t, func(ht *hegel.T) {
		jsonMode := hegel.Draw(ht, hegel.Booleans())
		write := WriteHuman
		if jsonMode {
			write = WriteJSON
		}
		var raw bytes.Buffer
		require.NoError(ht, write(&raw, result))
		offset := hegel.Draw(ht, hegel.Integers(0, raw.Len()-1))
		sentinel := errors.New("consumer closed output")
		writer := &partialFailureWriter{limit: offset, err: sentinel}
		assert.ErrorIs(ht, write(writer, result), sentinel)
		assert.Equal(ht, raw.String()[:offset], writer.written.String())
	}, hegel.WithTestCases(2000))
}
