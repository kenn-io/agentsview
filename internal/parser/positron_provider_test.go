package parser

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPositronProviderFactoryReplacesLegacyAdapter(t *testing.T) {
	factory, ok := ProviderFactoryByType(AgentPositron)
	require.True(t, ok)
	require.NotNil(t, factory)

	provider, ok := NewProvider(AgentPositron, ProviderConfig{
		Roots:   []string{t.TempDir()},
		Machine: "devbox",
	})
	require.True(t, ok)
	require.NotNil(t, provider)
}

func TestPositronProviderSourceMethods(t *testing.T) {
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
	require.True(t, ok)

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, filepath.Join(root, "workspaceStorage"), plan.Roots[0].Path)
	assert.True(t, plan.Roots[0].Recursive)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, sourcePath, discovered[0].DisplayPath)
	assert.Equal(t, "positron-app", discovered[0].ProjectHint)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~positron:" + sessionID,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sourcePath, found.DisplayPath)

	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: sourcePath, EventKind: "write"},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, sourcePath, changed[0].DisplayPath)

	fingerprint, err := provider.Fingerprint(context.Background(), found)
	require.NoError(t, err)
	assert.Equal(t, sourcePath, fingerprint.Key)
	assert.Positive(t, fingerprint.Size)
	assert.Positive(t, fingerprint.MTimeNS)
	assert.NotEmpty(t, fingerprint.Hash)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      found,
		Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.True(t, outcome.ResultSetComplete)
	require.Len(t, outcome.Results, 1)
	require.False(t, outcome.ForceReplace)
	result := outcome.Results[0]
	assert.Equal(t, DataVersionCurrent, result.DataVersion)
	assert.Equal(t, "positron:"+sessionID, result.Result.Session.ID)
	assert.Equal(t, AgentPositron, result.Result.Session.Agent)
	assert.Equal(t, "positron-app", result.Result.Session.Project)
	assert.Equal(t, "devbox", result.Result.Session.Machine)
	assert.Equal(t, fingerprint.Hash, result.Result.Session.File.Hash)
	assert.Len(t, result.Result.Messages, 2)
}

func TestPositronProviderClassifiesDeletedAndMetadataPaths(t *testing.T) {
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
	require.True(t, ok)

	metadataChanged, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: workspacePath, EventKind: "write"},
	)
	require.NoError(t, err)
	require.Len(t, metadataChanged, 1)
	assert.Equal(t, sourcePath, metadataChanged[0].DisplayPath)

	beforeMetadata, err := provider.Fingerprint(context.Background(), metadataChanged[0])
	require.NoError(t, err)
	writeSourceFile(t, workspacePath,
		`{"folder":"file:///Users/alice/code/positron-renamed-app"}`)
	afterMetadata, err := provider.Fingerprint(context.Background(), metadataChanged[0])
	require.NoError(t, err)
	assert.NotEqual(t, beforeMetadata.Hash, afterMetadata.Hash)

	require.NoError(t, os.Remove(sourcePath))
	deleted, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: sourcePath, EventKind: "remove"},
	)
	require.NoError(t, err)
	require.Len(t, deleted, 1)
	assert.Equal(t, sourcePath, deleted[0].DisplayPath)
}
