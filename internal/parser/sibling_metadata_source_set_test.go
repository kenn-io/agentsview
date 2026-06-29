package parser

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSiblingMetadataSourceSetMapsSiblingEventsToPrimarySource(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(root, "session_123")
	messagesPath := filepath.Join(sessionDir, "messages.jsonl")
	metaPath := filepath.Join(sessionDir, "meta.json")
	writeSourceFile(t, messagesPath, "{\"role\":\"user\"}\n")
	writeSourceFile(t, metaPath, "{\"title\":\"Session\"}\n")

	sources := NewSiblingMetadataSourceSet(
		AgentVibe,
		[]string{root},
		JSONLSourceSetOptions{
			Recursive:  true,
			Extensions: []string{".jsonl"},
			IncludePath: func(root, path string) bool {
				return filepath.Base(path) == "messages.jsonl"
			},
		},
		SiblingMetadataSourceSetOptions{
			SiblingGlobs: []string{"meta.json"},
			SiblingPaths: func(root, sourcePath string) []string {
				return []string{filepath.Join(filepath.Dir(sourcePath), "meta.json")}
			},
			SourcePathForSibling: func(root, siblingPath string) (string, bool) {
				if filepath.Base(siblingPath) != "meta.json" {
					return "", false
				}
				return filepath.Join(filepath.Dir(siblingPath), "messages.jsonl"), true
			},
		},
	)

	plan, err := sources.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.ElementsMatch(t, []string{"*.jsonl", "meta.json"}, plan.Roots[0].IncludeGlobs)

	changed, err := sources.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: metaPath, EventKind: "write", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, messagesPath, changed[0].DisplayPath)

	fingerprint, err := sources.Fingerprint(context.Background(), changed[0])
	require.NoError(t, err)
	assert.Equal(t, messagesPath, fingerprint.Key)
	assert.NotZero(t, fingerprint.Size)
	assert.NotZero(t, fingerprint.MTimeNS)
	assert.NotEmpty(t, fingerprint.Hash)
}

func TestSiblingMetadataSourceSetFingerprintsSourceWithoutOpaque(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(root, "session_123")
	messagesPath := filepath.Join(sessionDir, "messages.jsonl")
	metaPath := filepath.Join(sessionDir, "meta.json")
	writeSourceFile(t, messagesPath, "{\"role\":\"user\"}\n")
	writeSourceFile(t, metaPath, "{\"title\":\"Session\"}\n")

	sources := NewSiblingMetadataSourceSet(
		AgentVibe,
		[]string{root},
		JSONLSourceSetOptions{Recursive: true},
		SiblingMetadataSourceSetOptions{
			SiblingPaths: func(root, sourcePath string) []string {
				return []string{filepath.Join(filepath.Dir(sourcePath), "meta.json")}
			},
		},
	)
	source, ok, err := sources.FindSource(
		context.Background(),
		FindSourceRequest{StoredFilePath: messagesPath},
	)
	require.NoError(t, err)
	require.True(t, ok)
	source.Opaque = nil

	fingerprint, err := sources.Fingerprint(context.Background(), source)

	require.NoError(t, err)
	assert.Equal(t, messagesPath, fingerprint.Key)
	assert.NotEmpty(t, fingerprint.Hash)
}
