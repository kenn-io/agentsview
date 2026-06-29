package parser

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGptmeProviderParsesFixture(t *testing.T) {
	logsDir := filepath.Join("testdata", "gptme")

	provider, ok := NewProvider(AgentGptme, ProviderConfig{
		Roots:   []string{logsDir},
		Machine: "testmachine",
	})
	require.True(t, ok)
	source, found, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "2026-06-13-write-hello-world",
	})
	require.NoError(t, err)
	require.True(t, found)
	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:  source,
		Machine: "testmachine",
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)

	sess := outcome.Results[0].Result.Session
	msgs := outcome.Results[0].Result.Messages
	assert.Equal(t, "gptme:2026-06-13-write-hello-world", sess.ID)
	assert.Equal(t, "write-hello-world", sess.Project)
	assert.Equal(t, "testmachine", sess.Machine)
	assert.Equal(t, AgentGptme, sess.Agent)
	assert.Contains(t, sess.FirstMessage, "hello world")

	// Expect: user, assistant, visible tool output, user, assistant, visible tool output
	// System message is skipped.
	require.Len(t, msgs, 6)

	user0 := msgs[0]
	assert.Equal(t, RoleUser, user0.Role)
	assert.False(t, user0.IsSystem)
	assert.Contains(t, user0.Content, "hello world")

	asst0 := msgs[1]
	assert.Equal(t, RoleAssistant, asst0.Role)
	assert.Equal(t, "openrouter/anthropic/claude-sonnet-4-6", asst0.Model)
	assert.Equal(t, 42, asst0.OutputTokens)
	assert.True(t, asst0.HasOutputTokens)
	assert.Equal(t, 120+80, asst0.ContextTokens) // input + cache_read
	assert.True(t, asst0.HasContextTokens)

	tool0 := msgs[2]
	assert.Equal(t, RoleAssistant, tool0.Role)
	assert.False(t, tool0.IsSystem)
	assert.Contains(t, tool0.Content, "Saved file")

	// Timestamps must parse from the fixture's microsecond format ("2006-01-02T15:04:05.000000").
	// sess.StartedAt comes from the system message (processed before role-skip).
	assert.Equal(t, time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC), sess.StartedAt)
	assert.Equal(t, time.Date(2026, 6, 13, 10, 0, 13, 0, time.UTC), sess.EndedAt)
	assert.Equal(t, time.Date(2026, 6, 13, 10, 0, 1, 0, time.UTC), msgs[0].Timestamp)

	// Accumulated session totals.
	assert.Equal(t, 42+15, sess.TotalOutputTokens)
	assert.Equal(t, 2, sess.UserMessageCount)
}

func TestGptmeProviderDiscoversFixture(t *testing.T) {
	logsDir := filepath.Join("testdata", "gptme")
	provider, ok := NewProvider(AgentGptme, ProviderConfig{Roots: []string{logsDir}})
	require.True(t, ok)

	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, AgentGptme, sources[0].Provider)
	assert.Contains(t, sources[0].DisplayPath, "conversation.jsonl")
}

func TestGptmeProviderFindsFixtureSource(t *testing.T) {
	logsDir := filepath.Join("testdata", "gptme")
	provider, ok := NewProvider(AgentGptme, ProviderConfig{Roots: []string{logsDir}})
	require.True(t, ok)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "2026-06-13-write-hello-world",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Contains(t, found.DisplayPath, "conversation.jsonl")

	_, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "nonexistent-session",
	})
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestGptmeProjectFromSessionName(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"2026-06-13-write-hello-world", "write-hello-world"},
		{"2026-06-13-162241-feat-tts-fix", "feat-tts-fix"},
		{"2026-06-13-my-project-longer", "my-project-longer"},
		{"no-date-here", "no-date-here"},
		{"2026-06-13", "2026-06-13"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := gptmeProjectFromSessionName(c.name)
			assert.Equal(t, c.want, got)
		})
	}
}
