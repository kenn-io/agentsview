package sync

import (
	"fmt"
	"os"
	"path/filepath"
	gosync "sync"
	"testing"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeAiderRepoSession writes one Aider history file with a single run under
// <root>/<repo> and returns the derived session ID.
func writeAiderRepoSession(t *testing.T, root, repo, prompt string) (path, id string) {
	t.Helper()
	repoDir := filepath.Join(root, repo)
	require.NoError(t, os.MkdirAll(repoDir, 0o755))
	path = filepath.Join(repoDir, parser.AiderHistoryFileName())
	body := "# aider chat started at 2026-06-09 14:01:00\n#### " + prompt + "\nanswer\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	rawID, ok := parser.AiderRawIDAt(path, 0)
	require.True(t, ok)
	return path, "aider:" + rawID
}

// writeClaudeCorpus writes n minimal Claude sessions under dir and returns
// their derived session IDs.
func writeClaudeCorpus(t *testing.T, dir string, n int) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := range n {
		name := fmt.Sprintf("claude-%04d", i)
		path := filepath.Join(dir, "project", name+".jsonl")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(
			testjsonl.NewSessionBuilder().
				AddClaudeUser("2024-01-01T00:00:00Z", fmt.Sprintf("hi %d", i)).
				String(),
		), 0o644))
		ids = append(ids, name)
	}
	return ids
}

// lstatRecorder wraps os.Lstat and records every path stat-checked so a scoped
// reconciliation can prove it never enumerated another provider's sources.
type lstatRecorder struct {
	mu    gosync.Mutex
	paths []string
}

func (r *lstatRecorder) stat(path string) (os.FileInfo, error) {
	r.mu.Lock()
	r.paths = append(r.paths, path)
	r.mu.Unlock()
	return os.Lstat(path)
}

func (r *lstatRecorder) countUnder(dir string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, p := range r.paths {
		if samePathOrDescendant(cleanRootPath(p), cleanRootPath(dir)) {
			count++
		}
	}
	return count
}

func TestReconcileProviderRootsDoesNotExpandAcrossProviders(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	base := t.TempDir()
	aiderRoot := filepath.Join(base, "aider")
	// claudeDir is a descendant of aiderRoot: the overlap that
	// logicalRootsForWatchRoots would otherwise expand across providers.
	claudeDir := filepath.Join(aiderRoot, "claude")
	require.NoError(t, os.MkdirAll(claudeDir, 0o755))

	const aiderCount = 5
	aiderIDs := make([]string, 0, aiderCount)
	aiderPaths := make([]string, 0, aiderCount)
	for i := range aiderCount {
		path, id := writeAiderRepoSession(
			t, aiderRoot, fmt.Sprintf("repo%d", i), fmt.Sprintf("prompt %d", i),
		)
		aiderIDs = append(aiderIDs, id)
		aiderPaths = append(aiderPaths, path)
	}
	claudeIDs := writeClaudeCorpus(t, claudeDir, 100)

	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentAider:  {aiderRoot},
			parser.AgentClaude: {claudeDir},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	require.Equal(t, aiderCount+len(claudeIDs),
		engine.SyncAll(t.Context(), nil).Synced, "cold pass ingests every source")

	// Delete one Aider source under the opted-in root; the scoped pass must
	// tombstone it exactly as a full pass would.
	require.NoError(t, os.Remove(aiderPaths[2]))

	rec := &lstatRecorder{}
	engine.lstat = rec.stat

	require.NoError(t, engine.ReconcileProviderRoots(
		t.Context(), parser.AgentAider, []string{aiderRoot}))

	// Deletion within scope is preserved.
	deleted, err := database.GetSessionFull(t.Context(), aiderIDs[2])
	require.NoError(t, err)
	require.NotNil(t, deleted)
	require.NotNil(t, deleted.DeletionCause)
	assert.Equal(t, "source_missing", *deleted.DeletionCause)

	// Surviving Aider sources stay active.
	for i, id := range aiderIDs {
		if i == 2 {
			continue
		}
		active, err := database.GetSession(t.Context(), id)
		require.NoError(t, err)
		assert.NotNil(t, active, "surviving Aider session must remain active")
	}

	// No Claude session may be tombstoned by an Aider-scoped pass.
	for _, id := range claudeIDs {
		active, err := database.GetSession(t.Context(), id)
		require.NoError(t, err)
		assert.NotNil(t, active, "agent-scoped pass must not tombstone another provider")
	}

	// The scoped pass must not enumerate Claude sources: no stat under
	// claudeDir, and rehydration bounded by the Aider corpus.
	assert.Zero(t, rec.countUnder(claudeDir),
		"agent-scoped reconciliation must not stat other providers' sources")
	assert.LessOrEqual(t, engine.LastReconciliationResult().Metrics.MaxRehydratedSources,
		aiderCount, "rehydration must stay bounded by the scoped provider's corpus")
}
