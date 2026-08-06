package parser

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrimeAgentProviderParsesFlatSessionAndAttributedUsage(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "019c1234-session.jsonl")
	writeSourceFile(t, sourcePath, strings.Join([]string{
		`{"type":"session","version":3,"id":"019c1234-session","timestamp":"2026-08-06T12:00:00Z","cwd":"/work/prime-project","parentSession":"/config/.prime/agent/sessions/019c0000-parent.jsonl"}`,
		`{"type":"session_info","id":"info-1","parentId":null,"timestamp":"2026-08-06T12:00:01Z","name":"Investigate scheduler"}`,
		`{"type":"message","id":"user-1","parentId":"info-1","timestamp":"2026-08-06T12:00:02Z","message":{"role":"user","content":"Inspect the scheduler."}}`,
		`{"type":"message","id":"assistant-1","parentId":"user-1","timestamp":"2026-08-06T12:00:03Z","message":{"role":"assistant","content":[{"type":"text","text":"I found the issue."}],"provider":"prime-inference","model":"intellect-3","usage":{"input":10,"output":1,"cacheRead":0,"cacheWrite":0}}}`,
		`{"type":"child_usage_attributed","id":"usage-1","parentId":"assistant-1","timestamp":"2026-08-06T12:00:04Z","targetId":"assistant-1","childUsage":{"input":20,"output":6,"cacheRead":5,"cacheWrite":2},"aggregateUsage":{"input":30,"output":7,"cacheRead":5,"cacheWrite":2}}`,
		"",
	}, "\n"))

	provider, ok := NewProvider(AgentPrimeAgent, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, AgentPrimeAgent, sources[0].Provider)
	assert.Equal(t, sourcePath, sources[0].DisplayPath)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~prime-agent:019c1234-session",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sourcePath, found.DisplayPath)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: sources[0],
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)

	result := outcome.Results[0].Result
	assert.Equal(t, "prime-agent:019c1234-session", result.Session.ID)
	assert.Equal(t, AgentPrimeAgent, result.Session.Agent)
	assert.Equal(t, "prime_project", result.Session.Project)
	assert.Equal(t, "devbox", result.Session.Machine)
	assert.Equal(t, "Investigate scheduler", result.Session.SessionName)
	assert.Equal(t, "prime-agent:019c0000-parent", result.Session.ParentSessionID)
	assert.Equal(t, 37, result.Session.PeakContextTokens)
	assert.Equal(t, 7, result.Session.TotalOutputTokens)

	require.Len(t, result.Messages, 2)
	assert.Equal(t, "Inspect the scheduler.", result.Messages[0].Content)
	assert.Equal(t, "I found the issue.", result.Messages[1].Content)
	assert.Equal(t, "intellect-3", result.Messages[1].Model)
	assert.JSONEq(t, `{
		"input_tokens": 30,
		"output_tokens": 7,
		"cache_read_input_tokens": 5,
		"cache_creation_input_tokens": 2
	}`, string(result.Messages[1].TokenUsage))
}
