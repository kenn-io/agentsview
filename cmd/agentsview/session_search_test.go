package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

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
