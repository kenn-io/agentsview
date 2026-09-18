package parser

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeStatDigestFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func TestFileStatTupleDigestRejectsUnavailableChangeTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	writeStatDigestFile(t, path, "unchanged\n")

	digest := fileStatTupleDigestWithChangeTime(
		func(string, os.FileInfo) (int64, bool) {
			return 0, false
		},
		0xC1,
		path,
	)

	assert.Zero(t, digest,
		"size and mtime alone must not produce a trusted digest")
}

func TestClaudeProviderComputesMultiFileStatHash(t *testing.T) {
	provider, ok := NewProvider(AgentClaude, ProviderConfig{
		Machine: "local",
	})
	require.True(t, ok)
	hasher, ok := provider.(MultiFileStatHasher)
	require.True(t, ok, "claude provider must implement MultiFileStatHasher")
	assert.Equal(t, CapabilitySupported,
		provider.Capabilities().Source.MultiFileStatHash,
		"claude must declare MultiFileStatHash so the engine registers the hasher")

	path := filepath.Join(t.TempDir(), "session.jsonl")
	writeStatDigestFile(t, path, "line one\n")

	first := hasher.ComputeMultiFileStatHash(path)
	info, err := os.Stat(path)
	require.NoError(t, err)
	if _, changeTimeOK := codexIndexChangeTime(path, info); !changeTimeOK {
		assert.Zero(t, first,
			"unavailable change-time must disable the stat digest")
		return
	}
	require.NotZero(t, first)
	assert.Equal(t, first, hasher.ComputeMultiFileStatHash(path),
		"digest must be stable while the transcript stat is unchanged")

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString("line two\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	assert.NotEqual(t, first, hasher.ComputeMultiFileStatHash(path),
		"an appended transcript must change the digest")
}

func TestCodexProviderComputesMultiFileStatHash(t *testing.T) {
	root := t.TempDir()
	provider := newCodexTestProvider(t, root)
	hasher, ok := any(provider).(MultiFileStatHasher)
	require.True(t, ok, "codex provider must implement MultiFileStatHasher")
	assert.Equal(t, CapabilitySupported,
		provider.Capabilities().Source.MultiFileStatHash,
		"codex must declare MultiFileStatHash so the engine registers the hasher")

	path := filepath.Join(root, "sessions", "rollout-2026-01-01.jsonl")
	writeStatDigestFile(t, path, "{}\n")

	first := hasher.ComputeMultiFileStatHash(path)
	info, err := os.Stat(path)
	require.NoError(t, err)
	if _, changeTimeOK := codexIndexChangeTime(path, info); !changeTimeOK {
		assert.Zero(t, first,
			"unavailable change-time must disable the stat digest")
		return
	}
	require.NotZero(t, first)
	assert.Equal(t, first, hasher.ComputeMultiFileStatHash(path),
		"digest must be stable while transcript and index stats are unchanged")

	// A session_index.jsonl change (e.g. a thread title rename) must break
	// the digest even when the transcript itself is untouched, so the warm
	// short-circuit can never mask an index-only metadata refresh.
	indexPath := filepath.Join(root, CodexSessionIndexFilename)
	writeStatDigestFile(t, indexPath, `{"id":"x","name":"Renamed"}`+"\n")
	withIndex := hasher.ComputeMultiFileStatHash(path)
	indexInfo, err := os.Stat(indexPath)
	require.NoError(t, err)
	if _, changeTimeOK := codexIndexChangeTime(
		indexPath, indexInfo,
	); !changeTimeOK {
		assert.Zero(t, withIndex,
			"an existing sidecar without change-time must disable the digest")
		return
	}
	assert.NotEqual(t, first, withIndex,
		"an appearing session index must change the digest")

	future := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(indexPath, future, future))
	assert.NotEqual(t, withIndex, hasher.ComputeMultiFileStatHash(path),
		"an index mtime change must change the digest")
}
