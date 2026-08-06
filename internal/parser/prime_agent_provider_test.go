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
	// Prime Agent v0.7.0 can allocate the transcript filename before the
	// session header, so their UUIDs are not guaranteed to match.
	sourcePath := filepath.Join(root, "019c1111-file.jsonl")
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
	assert.True(t, outcome.ForceReplace)

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

func TestPrimeAgentParentSessionPathSeparators(t *testing.T) {
	tests := []struct {
		name   string
		header string
	}{
		{
			name:   "POSIX",
			header: `{"type":"session","version":3,"id":"child","timestamp":"2026-08-06T12:00:00Z","cwd":"/work/project","parentSession":"/home/user/.prime/agent/sessions/parent-session.jsonl"}`,
		},
		{
			name:   "Windows",
			header: `{"type":"session","version":3,"id":"child","timestamp":"2026-08-06T12:00:00Z","cwd":"C:\\work\\project","parentSession":"C:\\Users\\user\\.prime\\agent\\sessions\\parent-session.jsonl"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content := strings.Join([]string{
				tt.header,
				`{"type":"message","id":"user-1","parentId":null,"timestamp":"2026-08-06T12:00:01Z","message":{"role":"user","content":"hello"}}`,
				"",
			}, "\n")
			session, _ := parsePiLikeTestSession(t, AgentPrimeAgent, content)
			assert.Equal(t, "prime-agent:parent-session", session.ParentSessionID)
		})
	}
}

func TestPrimeAgentAttributedUsageUsesLastKnownTargetAggregate(t *testing.T) {
	content := strings.Join([]string{
		`{"type":"session","version":3,"id":"usage-session","timestamp":"2026-08-06T12:00:00Z","cwd":"/work/project"}`,
		`{"type":"message","id":"user-1","parentId":null,"timestamp":"2026-08-06T12:00:01Z","message":{"role":"user","content":"hello"}}`,
		`{"type":"message","id":"assistant-1","parentId":"user-1","timestamp":"2026-08-06T12:00:02Z","message":{"role":"assistant","content":"hi","model":"gpt-5.4-mini","usage":{"input":10,"output":1}}}`,
		`{"type":"child_usage_attributed","id":"unknown-usage","parentId":"assistant-1","timestamp":"2026-08-06T12:00:03Z","targetId":"missing-assistant","aggregateUsage":{"input":900,"output":90}}`,
		`{"type":"child_usage_attributed","id":"first-usage","parentId":"assistant-1","timestamp":"2026-08-06T12:00:04Z","targetId":"assistant-1","aggregateUsage":{"input":20,"output":3,"cacheRead":4,"cacheWrite":1}}`,
		`{"type":"child_usage_attributed","id":"last-usage","parentId":"first-usage","timestamp":"2026-08-06T12:00:05Z","targetId":"assistant-1","aggregateUsage":{"input":30,"output":7,"cacheRead":5,"cacheWrite":2}}`,
		"",
	}, "\n")

	session, messages := parsePiLikeTestSession(t, AgentPrimeAgent, content)
	require.Len(t, messages, 2)
	assert.Equal(t, 37, messages[1].ContextTokens)
	assert.Equal(t, 7, messages[1].OutputTokens)
	assert.Equal(t, 37, session.PeakContextTokens)
	assert.Equal(t, 7, session.TotalOutputTokens)
}

func TestPrimeAgentFindSourceVerifiesDirectFilenameHeader(t *testing.T) {
	tests := []struct {
		name       string
		directID   string
		fallbackID string
		wantFile   string
	}{
		{
			name:     "matching direct header",
			directID: "target-session",
			wantFile: "target-session.jsonl",
		},
		{
			name:       "mismatched direct header falls back to matching header",
			directID:   "different-session",
			fallbackID: "target-session",
			wantFile:   "actual-transcript.jsonl",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			directPath := filepath.Join(root, "target-session.jsonl")
			writeSourceFile(t, directPath,
				`{"type":"session","version":3,"id":"`+tt.directID+`","timestamp":"2026-08-06T12:00:00Z","cwd":"/work/project"}`+"\n")
			if tt.fallbackID != "" {
				writeSourceFile(t, filepath.Join(root, "actual-transcript.jsonl"),
					`{"type":"session","version":3,"id":"`+tt.fallbackID+`","timestamp":"2026-08-06T12:00:00Z","cwd":"/work/project"}`+"\n")
			}

			provider, ok := NewProvider(AgentPrimeAgent, ProviderConfig{
				Roots: []string{root},
			})
			require.True(t, ok)
			found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
				FullSessionID: "prime-agent:target-session",
			})
			require.NoError(t, err)
			require.True(t, ok)
			assert.Equal(t, filepath.Join(root, tt.wantFile), found.DisplayPath)
		})
	}
}
