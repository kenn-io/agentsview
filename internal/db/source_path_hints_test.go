package db

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListStoredSourcePathHintsScopesByAgentAndRoot(t *testing.T) {
	d := testDB(t)
	root := t.TempDir()
	watchRoot := filepath.Join(root, "db")
	childPath := filepath.Join(watchRoot, "sessions", "a.jsonl")
	virtualPath := filepath.Join(watchRoot, "state.sqlite3") + "#session-a"
	uncleanPath := filepath.Join(watchRoot, "nested", "..", "nested", "b.jsonl")
	cleanPath := filepath.Join(watchRoot, "nested", "b.jsonl")
	siblingPath := filepath.Join(root, "db2", "sessions", "other.jsonl")
	otherAgentPath := filepath.Join(watchRoot, "sessions", "other-agent.jsonl")
	deletedPath := filepath.Join(watchRoot, "sessions", "deleted.jsonl")

	insertSessionWithSourcePath(t, d, "claude:child", "claude", childPath)
	insertSessionWithSourcePath(t, d, "claude:child-dup", "claude", childPath)
	insertSessionWithSourcePath(t, d, "claude:virtual", "claude", virtualPath)
	insertSessionWithSourcePath(t, d, "claude:unclean", "claude", uncleanPath)
	insertSessionWithSourcePath(t, d, "claude:sibling", "claude", siblingPath)
	insertSessionWithSourcePath(t, d, "codex:other-agent", "codex", otherAgentPath)
	insertSessionWithSourcePath(t, d, "claude:deleted", "claude", deletedPath)
	require.NoError(t, d.SoftDeleteSession("claude:deleted"))

	got, err := d.ListStoredSourcePathHints("claude", []string{
		filepath.Join(watchRoot, "."),
		filepath.Join(root, "db2", "..", "db"),
	})

	require.NoError(t, err)
	assert.Equal(t, []string{
		cleanPath,
		childPath,
		virtualPath,
	}, got)
}

func TestListStoredSourcePathHintsHandlesHashPathsAndVirtualSuffixes(t *testing.T) {
	d := testDB(t)
	base := t.TempDir()

	hashRoot := filepath.Join(base, "db#prod")
	hashChild := filepath.Join(hashRoot, "sessions", "a.jsonl")
	insertSessionWithSourcePath(t, d, "claude:hash-child", "claude", hashChild)

	dbRoot := filepath.Join(base, "state.sqlite3")
	virtualPath := dbRoot + "#session-a"
	insertSessionWithSourcePath(t, d, "claude:virtual", "claude", virtualPath)

	plainRoot := filepath.Join(base, "db")
	hashSibling := filepath.Join(base, "db#backup", "sessions", "b.jsonl")
	hashVirtualSibling := plainRoot + "#session-b"
	insertSessionWithSourcePath(t, d, "claude:hash-sibling", "claude", hashSibling)
	insertSessionWithSourcePath(
		t, d, "claude:hash-virtual-sibling", "claude", hashVirtualSibling,
	)

	got, err := d.ListStoredSourcePathHints("claude", []string{
		hashRoot,
		dbRoot,
		plainRoot,
	})

	require.NoError(t, err)
	assert.Equal(t, []string{
		hashChild,
		virtualPath,
	}, got)
}

func TestListStoredSourcePathHintsEscapesLikeWildcards(t *testing.T) {
	d := testDB(t)
	base := t.TempDir()
	root := filepath.Join(base, "db%!_root")
	childPath := filepath.Join(root, "session.jsonl")
	insertSessionWithSourcePath(t, d, "claude:wildcard-child", "claude", childPath)

	siblingPath := filepath.Join(base, "dbX!Yroot", "session.jsonl")
	insertSessionWithSourcePath(t, d, "claude:wildcard-sibling", "claude", siblingPath)

	got, err := d.ListStoredSourcePathHints("claude", []string{root})

	require.NoError(t, err)
	assert.Equal(t, []string{childPath}, got)
}

func TestListStoredSourcePathHintsBatchesRootsWithoutTruncating(t *testing.T) {
	d := testDB(t)
	base := t.TempDir()
	var roots []string
	var want []string
	for i := range storedSourcePathHintRootBatchSize + 17 {
		root := filepath.Join(base, fmt.Sprintf("root-%03d", i))
		roots = append(roots, root)
		if i == 0 || i == storedSourcePathHintRootBatchSize+16 {
			path := filepath.Join(root, "session.jsonl")
			insertSessionWithSourcePath(
				t, d, fmt.Sprintf("claude:match-%03d", i), "claude", path,
			)
			want = append(want, path)
		}
	}
	for i := range 250 {
		path := filepath.Join(base, "unrelated", fmt.Sprintf("%03d.jsonl", i))
		insertSessionWithSourcePath(
			t, d, fmt.Sprintf("claude:unrelated-%03d", i), "claude", path,
		)
	}

	got, err := d.ListStoredSourcePathHints("claude", roots)

	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestStoredSourcePathHintsLookupUsesAgentFilePathIndex(t *testing.T) {
	d := testDB(t)
	root := t.TempDir()
	explainSQL, args := storedSourcePathHintQuery("claude", []string{root})
	rows, err := d.getReader().Query("EXPLAIN QUERY PLAN "+explainSQL, args...)
	require.NoError(t, err)
	defer rows.Close()

	var details []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &notused, &detail))
		details = append(details, detail)
	}
	require.NoError(t, rows.Err())

	assert.Contains(
		t,
		strings.Join(details, "\n"),
		"idx_sessions_agent_file_path_active",
	)
}

func insertSessionWithSourcePath(
	t *testing.T,
	d *DB,
	id string,
	agent string,
	path string,
	opts ...func(*Session),
) {
	t.Helper()
	insertSession(t, d, id, "proj", append([]func(*Session){
		func(s *Session) {
			s.Agent = agent
			s.FilePath = &path
		},
	}, opts...)...)
}
