package sync

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// TestObserveProviderSourceParsesCodexSourceWithIndexTitle exercises the folded
// Codex provider end to end through ObserveProviderSource. The legacy
// ParseCodexSession entrypoint was deleted in the fold, so this replaces the
// shadow-baseline comparison with provider-API coverage that pins the parsed
// session shape: discovery finds the dated transcript, the sibling
// session_index.jsonl supplies the thread title as session_name, and the
// observed parse output and data-version planning match the source.
func TestObserveProviderSourceParsesCodexSourceWithIndexTitle(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "sessions")
	uuid := "019eb791-cf7d-75c1-8439-9ed74c12abcd"
	sourcePath := filepath.Join(
		root,
		"2026",
		"06",
		"11",
		"rollout-2026-06-11T12-44-06-"+uuid+".jsonl",
	)
	writeProviderShadowSourceFile(
		t,
		sourcePath,
		testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON(
				uuid,
				"/home/user/code/api",
				"codex_cli_rs",
				"2026-06-11T12:44:06Z",
			),
			testjsonl.CodexMsgJSON("user", "provider question", "2026-06-11T12:44:07Z"),
		),
	)
	writeProviderShadowSourceFile(
		t,
		filepath.Join(base, parser.CodexSessionIndexFilename),
		`{"id":"`+uuid+`","thread_name":"Provider title","updated_at":"2026-06-11T17:34:20Z"}`+"\n",
	)

	provider, ok := parser.NewProvider(parser.AgentCodex, parser.ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	observation, err := ObserveProviderSource(context.Background(), provider, ProviderObserveRequest{
		Source:  sources[0],
		Machine: "devbox",
	})
	require.NoError(t, err)
	require.Len(t, observation.Results, 1)

	session := observation.Results[0].Session
	assert.Equal(t, "codex:"+uuid, session.ID)
	assert.Equal(t, parser.AgentCodex, session.Agent)
	assert.Equal(t, "devbox", session.Machine)
	assert.Equal(t, "/home/user/code/api", session.Cwd)
	assert.Equal(t, "Provider title", session.SessionName)
	assert.Equal(t, "provider question", session.FirstMessage)
	assert.Equal(t, sourcePath, session.File.Path)
	assert.Equal(t, observation.Fingerprint.Hash, session.File.Hash)

	require.Len(t, observation.Results[0].Messages, 1)
	assert.Equal(t, parser.RoleUser, observation.Results[0].Messages[0].Role)

	assert.Equal(t, []string{session.ID}, observation.Planned.DataVersionSessionIDs())
	assert.Empty(t, observation.Planned.Diagnostics)
}
