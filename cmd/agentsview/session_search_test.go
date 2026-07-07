package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

func TestSessionSearchFlagValidation(t *testing.T) {
	cmd := newSessionSearchCommand()
	cmd.SetArgs([]string{"needle", "--regex", "--fts"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
}

// TestSessionSearchSinceMutuallyExclusiveWithActiveSince verifies the error
// is returned before any service/DB access is attempted (no data dir is set
// up here), matching --regex/--fts's fail-fast validation style.
func TestSessionSearchSinceMutuallyExclusiveWithActiveSince(t *testing.T) {
	cmd := newSessionSearchCommand()
	cmd.SetArgs([]string{
		"needle", "--since", "14d", "--active-since", "2024-01-01T00:00:00Z",
	})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(),
		"--since and --active-since are mutually exclusive")
}

func TestSessionSearchSinceRejectsInvalidFormat(t *testing.T) {
	cmd := newSessionSearchCommand()
	cmd.SetArgs([]string{"needle", "--since", "3x"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid --since")
}

// seedSearchMessage inserts a single message for sessionID carrying content,
// so `session search <pattern>` has something to match.
func seedSearchMessage(t *testing.T, dataDir, sessionID, content string) {
	t.Helper()
	d, err := db.Open(filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	require.NoError(t, d.InsertMessages([]db.Message{{
		SessionID:     sessionID,
		Ordinal:       1,
		Role:          "user",
		Content:       content,
		ContentLength: len(content),
		Timestamp:     "2026-04-01T00:00:00Z",
	}}))
}

// TestSessionSearchSinceFiltersByActivity is the end-to-end regression test
// for the CRITICAL requirement that --since actually narrows results by
// resolving to the same active_since window --active-since already
// threads through to the search filter: a session active within the
// window survives, one outside it does not.
func TestSessionSearchSinceFiltersByActivity(t *testing.T) {
	dataDir := newAgentDataDir(t)
	seedSessionsWithOpts(t, dataDir,
		activitySeed("fresh", 2*time.Hour),
		activitySeed("stale", 20*24*time.Hour),
	)
	seedSearchMessage(t, dataDir, "fresh", "needle in fresh session")
	seedSearchMessage(t, dataDir, "stale", "needle in stale session")

	out, err := executeCommand(newRootCommand(),
		"session", "search", "needle", "--since", "7d", "--format", "json")
	require.NoError(t, err)

	got := decodeCLIJSON[service.ContentSearchResult](t, out)
	require.Len(t, got.Matches, 1)
	assert.Equal(t, "fresh", got.Matches[0].SessionID)
}

func TestSessionSearchFTSWithToolSource(t *testing.T) {
	cmd := newSessionSearchCommand()
	cmd.SetArgs([]string{"needle", "--fts", "--in", "tool_result"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "messages only")
}

func TestResolveContentSearchModeMapping(t *testing.T) {
	tests := []struct {
		name                                     string
		useRegex, useFTS, useSemantic, useHybrid bool
		wantMode                                 string
	}{
		{name: "default substring", wantMode: "substring"},
		{name: "regex", useRegex: true, wantMode: "regex"},
		{name: "fts", useFTS: true, wantMode: "fts"},
		{name: "semantic", useSemantic: true, wantMode: "semantic"},
		{name: "hybrid", useHybrid: true, wantMode: "hybrid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mode, err := resolveContentSearchMode(
				tt.useRegex, tt.useFTS, tt.useSemantic, tt.useHybrid, nil)
			require.NoError(t, err)
			assert.Equal(t, tt.wantMode, mode)
		})
	}
}

func TestResolveContentSearchModeMutualExclusion(t *testing.T) {
	tests := []struct {
		name                                     string
		useRegex, useFTS, useSemantic, useHybrid bool
	}{
		{name: "regex and fts", useRegex: true, useFTS: true},
		{name: "semantic and hybrid", useSemantic: true, useHybrid: true},
		{name: "regex and semantic", useRegex: true, useSemantic: true},
		{name: "fts and hybrid", useFTS: true, useHybrid: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := resolveContentSearchMode(
				tt.useRegex, tt.useFTS, tt.useSemantic, tt.useHybrid, nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "mutually exclusive")
		})
	}
}

func TestSessionSearchSemanticWithToolSource(t *testing.T) {
	cmd := newSessionSearchCommand()
	cmd.SetArgs([]string{"needle", "--semantic", "--in", "tool_input"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "messages only")
}

func TestSessionSearchHybridWithToolSource(t *testing.T) {
	cmd := newSessionSearchCommand()
	cmd.SetArgs([]string{"needle", "--hybrid", "--in", "tool_result"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "messages only")
}

// TestSessionSearchScopeRequiresSemanticOrHybrid verifies --scope fails
// fast (before any service/DB access) when set without --semantic/--hybrid.
func TestSessionSearchScopeRequiresSemanticOrHybrid(t *testing.T) {
	for _, args := range [][]string{
		{"needle", "--scope", "top"},
		{"needle", "--fts", "--scope", "all"},
		{"needle", "--regex", "--scope", "subordinate"},
	} {
		cmd := newSessionSearchCommand()
		cmd.SetArgs(args)
		err := cmd.Execute()
		require.Error(t, err, "args %v", args)
		assert.Contains(t, err.Error(), "--semantic or --hybrid", "args %v", args)
	}
}

// TestSessionSearchScopeRejectsInvalidValue verifies the value gate fires
// at the CLI boundary rather than deep in the store.
func TestSessionSearchScopeRejectsInvalidValue(t *testing.T) {
	cmd := newSessionSearchCommand()
	cmd.SetArgs([]string{"needle", "--semantic", "--scope", "bogus"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "top, all, or subordinate")
}

func TestValidateScopeFlag(t *testing.T) {
	tests := []struct {
		name                   string
		scope                  string
		useSemantic, useHybrid bool
		wantErr                string
	}{
		{name: "empty scope always valid"},
		{name: "top with semantic", scope: "top", useSemantic: true},
		{name: "all with hybrid", scope: "all", useHybrid: true},
		{name: "subordinate with semantic", scope: "subordinate", useSemantic: true},
		{name: "scope without mode flag", scope: "top",
			wantErr: "--semantic or --hybrid"},
		{name: "invalid value", scope: "bogus", useSemantic: true,
			wantErr: "top, all, or subordinate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateScopeFlag(tt.scope, tt.useSemantic, tt.useHybrid)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestPrintContentMatchesHumanShowsScoreForScoredMatches(t *testing.T) {
	score := 0.834
	res := &service.ContentSearchResult{
		Matches: []db.ContentMatch{
			{
				SessionID: "sess1",
				Project:   "proj",
				Location:  "message",
				Ordinal:   3,
				Snippet:   "hello world",
				Score:     &score,
			},
			{
				SessionID: "sess2",
				Project:   "proj",
				Location:  "message",
				Ordinal:   1,
				Snippet:   "no score here",
			},
		},
	}
	var buf bytes.Buffer
	require.NoError(t, printContentMatchesHuman(&buf, res))
	out := buf.String()
	assert.Contains(t, out, "score=0.83")
	lines := bytes.Split(buf.Bytes(), []byte("\n"))
	require.NotEmpty(t, lines)
	assert.NotContains(t, string(lines[2]), "score=",
		"unscored match should not print a score")
}

func TestPrintContentMatchesHumanShowsContext(t *testing.T) {
	res := &service.ContentSearchResult{
		Matches: []db.ContentMatch{
			{
				SessionID: "sess1", Project: "proj", Location: "message",
				Ordinal: 5, Snippet: "the match line",
				ContextBefore: []db.Message{
					{Role: "user", Content: "earlier question"},
				},
				ContextAfter: []db.Message{
					{Role: "assistant", Content: "later reply"},
				},
			},
		},
	}
	var buf bytes.Buffer
	require.NoError(t, printContentMatchesHuman(&buf, res))
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	require.Len(t, lines, 4)
	assert.Equal(t, "  user: earlier question", lines[0])
	assert.Contains(t, lines[1], "sess1")
	assert.Contains(t, lines[2], "the match line")
	assert.Equal(t, "  assistant: later reply", lines[3])
}

func TestPrintContentMatchesHumanTruncatesContextLine(t *testing.T) {
	longContent := strings.Repeat("a", 250)
	res := &service.ContentSearchResult{
		Matches: []db.ContentMatch{
			{
				SessionID: "sess1", Ordinal: 1, Snippet: "match",
				ContextBefore: []db.Message{{Role: "user", Content: longContent}},
			},
		},
	}
	var buf bytes.Buffer
	require.NoError(t, printContentMatchesHuman(&buf, res))
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	require.NotEmpty(t, lines)
	require.True(t, strings.HasPrefix(lines[0], "  user: "))
	body := strings.TrimPrefix(lines[0], "  user: ")
	assert.LessOrEqual(t, len([]rune(body)), 201)
	assert.True(t, strings.HasSuffix(body, "…"))
}

func TestContentMatchJSONRoundTripsContext(t *testing.T) {
	res := service.ContentSearchResult{
		Matches: []db.ContentMatch{
			{
				SessionID: "sess1", Ordinal: 5,
				ContextBefore: []db.Message{{Role: "user", Ordinal: 3, Content: "before"}},
				ContextAfter:  []db.Message{{Role: "assistant", Ordinal: 7, Content: "after"}},
			},
			{SessionID: "sess2", Ordinal: 1},
		},
	}
	data, err := json.Marshal(res)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"context_before"`)
	assert.Contains(t, string(data), `"context_after"`)

	var decoded service.ContentSearchResult
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.Len(t, decoded.Matches, 2)
	require.Len(t, decoded.Matches[0].ContextBefore, 1)
	assert.Equal(t, "before", decoded.Matches[0].ContextBefore[0].Content)
	assert.Empty(t, decoded.Matches[1].ContextBefore)
}

func TestContentMatchJSONRoundTripsScore(t *testing.T) {
	score := 0.5
	res := service.ContentSearchResult{
		Matches: []db.ContentMatch{
			{SessionID: "sess1", Ordinal: 1, Score: &score},
			{SessionID: "sess2", Ordinal: 2},
		},
	}
	data, err := json.Marshal(res)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"score":0.5`)

	var decoded service.ContentSearchResult
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.Len(t, decoded.Matches, 2)
	require.NotNil(t, decoded.Matches[0].Score)
	assert.InDelta(t, score, *decoded.Matches[0].Score, 0.0001)
	assert.Nil(t, decoded.Matches[1].Score)
}

// TestPrintContentMatchesHumanRendersUnitRangeAndSubMarker pins the human
// rendering for run-grouped semantic/hybrid hits: a multi-message unit
// renders "#<start>-<end> @<anchor>", a subordinate hit gains a "sub"
// marker, and a single-ordinal hit keeps today's plain "#<ordinal>" form.
func TestPrintContentMatchesHumanRendersUnitRangeAndSubMarker(t *testing.T) {
	score := 0.91
	res := &service.ContentSearchResult{
		Matches: []db.ContentMatch{
			{
				SessionID: "sess1", Project: "proj", Location: "message",
				Ordinal: 19, OrdinalRange: [2]int{12, 40},
				Subordinate: true, Score: &score, Snippet: "ranged hit",
			},
			{
				SessionID: "sess2", Project: "proj", Location: "message",
				Ordinal: 5, OrdinalRange: [2]int{5, 5},
				Snippet: "single-message unit",
			},
		},
	}
	var buf bytes.Buffer
	require.NoError(t, printContentMatchesHuman(&buf, res))
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	require.Len(t, lines, 4)

	assert.Contains(t, lines[0], "#12-40 @19", "range with anchor marker")
	assert.Contains(t, lines[0], " sub", "subordinate marker")
	assert.Contains(t, lines[0], "score=0.91")

	assert.Contains(t, lines[2], "#5", "single-ordinal hit keeps the plain form")
	assert.NotContains(t, lines[2], "@", "no anchor marker for single-ordinal hits")
	assert.NotContains(t, lines[2], " sub", "no subordinate marker for top-level hits")
}
