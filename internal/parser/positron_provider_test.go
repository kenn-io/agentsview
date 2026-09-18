package parser

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPositronProviderSourceMethods(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	sessionID := "positron-provider"
	hashDir := filepath.Join(root, "workspaceStorage", "workspace-hash")
	chatDir := filepath.Join(hashDir, "chatSessions")
	workspacePath := filepath.Join(hashDir, "workspace.json")
	sourcePath := filepath.Join(chatDir, sessionID+".jsonl")
	writeSourceFile(t, workspacePath,
		`{"folder":"file:///Users/alice/code/positron-app"}`)
	writeSourceFile(t, sourcePath,
		vscodeCopilotProviderJSONL(sessionID, "Hello Positron"))

	provider, ok := NewProvider(AgentPositron, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)

	plan, err := provider.WatchPlan(t.Context())
	require.NoError(err)
	require.Len(plan.Roots, 1)
	assert.Equal(filepath.Join(root, "workspaceStorage"), plan.Roots[0].Path)
	assert.True(plan.Roots[0].Recursive)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 1)
	assert.Equal(sourcePath, discovered[0].DisplayPath)
	assert.Equal("positron-app", discovered[0].ProjectHint)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		FullSessionID: "host~positron:" + sessionID,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(sourcePath, found.DisplayPath)

	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: sourcePath, EventKind: "write"},
	)
	require.NoError(err)
	require.Len(changed, 1)
	assert.Equal(sourcePath, changed[0].DisplayPath)

	fingerprint, err := provider.Fingerprint(t.Context(), found)
	require.NoError(err)
	assert.Equal(sourcePath, fingerprint.Key)
	assert.Positive(fingerprint.Size)
	assert.Positive(fingerprint.MTimeNS)
	assert.NotEmpty(fingerprint.Hash)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:      found,
		Fingerprint: fingerprint,
	})
	require.NoError(err)
	require.True(outcome.ResultSetComplete)
	require.Len(outcome.Results, 1)
	require.False(outcome.ForceReplace)
	result := outcome.Results[0]
	assert.Equal(DataVersionCurrent, result.DataVersion)
	assert.Equal("positron:"+sessionID, result.Result.Session.ID)
	assert.Equal(AgentPositron, result.Result.Session.Agent)
	assert.Equal("positron-app", result.Result.Session.Project)
	assert.Equal("devbox", result.Result.Session.Machine)
	assert.Equal(fingerprint.Hash, result.Result.Session.File.Hash)
	assert.Len(result.Result.Messages, 2)
}

func TestPositronProviderClassifiesDeletedAndMetadataPaths(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	hashDir := filepath.Join(root, "workspaceStorage", "workspace-hash")
	chatDir := filepath.Join(hashDir, "chatSessions")
	workspacePath := filepath.Join(hashDir, "workspace.json")
	sourcePath := filepath.Join(chatDir, "metadata.jsonl")
	writeSourceFile(t, workspacePath,
		`{"folder":"file:///Users/alice/code/positron-app"}`)
	writeSourceFile(t, sourcePath,
		vscodeCopilotProviderJSONL("metadata", "Hello metadata"))

	provider, ok := NewProvider(AgentPositron, ProviderConfig{Roots: []string{root}})
	require.True(ok)

	metadataChanged, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: workspacePath, EventKind: "write"},
	)
	require.NoError(err)
	require.Len(metadataChanged, 1)
	assert.Equal(sourcePath, metadataChanged[0].DisplayPath)

	beforeMetadata, err := provider.Fingerprint(t.Context(), metadataChanged[0])
	require.NoError(err)
	writeSourceFile(t, workspacePath,
		`{"folder":"file:///Users/alice/code/positron-renamed-app"}`)
	afterMetadata, err := provider.Fingerprint(t.Context(), metadataChanged[0])
	require.NoError(err)
	assert.NotEqual(beforeMetadata.Hash, afterMetadata.Hash)

	require.NoError(os.Remove(sourcePath))
	deleted, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: sourcePath, EventKind: "remove"},
	)
	require.NoError(err)
	require.Len(deleted, 1)
	assert.Equal(sourcePath, deleted[0].DisplayPath)
}
