package parser

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGptmeProviderParsesFixture(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	logsDir := filepath.Join("testdata", "gptme")

	provider, ok := NewProvider(AgentGptme, ProviderConfig{
		Roots:   []string{logsDir},
		Machine: "testmachine",
	})
	require.True(ok)
	source, found, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "2026-06-13-write-hello-world",
	})
	require.NoError(err)
	require.True(found)
	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:  source,
		Machine: "testmachine",
	})
	require.NoError(err)
	require.Len(outcome.Results, 1)

	sess := outcome.Results[0].Result.Session
	msgs := outcome.Results[0].Result.Messages
	assert.Equal("gptme:2026-06-13-write-hello-world", sess.ID)
	assert.Equal("write-hello-world", sess.Project)
	assert.Equal("testmachine", sess.Machine)
	assert.Equal(AgentGptme, sess.Agent)
	assert.Contains(sess.FirstMessage, "hello world")

	// Expect: user, assistant, visible tool output, user, assistant, visible tool output
	// System message is skipped.
	require.Len(msgs, 6)

	user0 := msgs[0]
	assert.Equal(RoleUser, user0.Role)
	assert.False(user0.IsSystem)
	assert.Contains(user0.Content, "hello world")

	asst0 := msgs[1]
	assert.Equal(RoleAssistant, asst0.Role)
	assert.Equal("openrouter/anthropic/claude-sonnet-4-6", asst0.Model)
	assert.Equal(42, asst0.OutputTokens)
	assert.True(asst0.HasOutputTokens)
	assert.Equal(120+80, asst0.ContextTokens) // input + cache_read
	assert.True(asst0.HasContextTokens)

	tool0 := msgs[2]
	assert.Equal(RoleAssistant, tool0.Role)
	assert.False(tool0.IsSystem)
	assert.Contains(tool0.Content, "Saved file")
	assert.Equal(SourceSubtypeToolResult, tool0.SourceSubtype,
		"tool output kept as assistant text is still tool output")

	// Timestamps must parse from the fixture's microsecond format ("2006-01-02T15:04:05.000000").
	// sess.StartedAt comes from the system message (processed before role-skip).
	assert.Equal(time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC), sess.StartedAt)
	assert.Equal(time.Date(2026, 6, 13, 10, 0, 13, 0, time.UTC), sess.EndedAt)
	assert.Equal(time.Date(2026, 6, 13, 10, 0, 1, 0, time.UTC), msgs[0].Timestamp)

	// Accumulated session totals.
	assert.Equal(42+15, sess.TotalOutputTokens)
	assert.Equal(2, sess.UserMessageCount)
}

func TestGptmeProviderDiscoversFixture(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	logsDir := filepath.Join("testdata", "gptme")
	provider, ok := NewProvider(AgentGptme, ProviderConfig{Roots: []string{logsDir}})
	require.True(ok)

	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)
	assert.Equal(AgentGptme, sources[0].Provider)
	assert.Contains(sources[0].DisplayPath, "conversation.jsonl")
}

func TestGptmeProviderFindsFixtureSource(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	logsDir := filepath.Join("testdata", "gptme")
	provider, ok := NewProvider(AgentGptme, ProviderConfig{Roots: []string{logsDir}})
	require.True(ok)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "2026-06-13-write-hello-world",
	})
	require.NoError(err)
	require.True(ok)
	assert.Contains(found.DisplayPath, "conversation.jsonl")

	_, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "nonexistent-session",
	})
	require.NoError(err)
	assert.False(ok)
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
