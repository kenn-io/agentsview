package db

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionIDsUnderPath(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	sep := string(filepath.Separator)
	root := filepath.Join(sep, "cell", "v2-sessions")
	paths := map[string]string{
		"in-1":    filepath.Join(root, "ag-1", ".claude-shared", "projects", "-w", "a.jsonl"),
		"in-2":    filepath.Join(root, "ag-2", ".claude-shared", "projects", "-w", "b.jsonl"),
		"sibling": filepath.Join(sep, "cell", "v2-sessions-old", "ag-1", "c.jsonl"),
		"case":    filepath.Join(sep, "CELL", "v2-sessions", "ag-1", "d.jsonl"),
		"deleted": filepath.Join(root, "ag-1", ".claude-shared", "projects", "-w", "e.jsonl"),
	}
	for id, p := range paths {
		p := p
		require.NoError(t, d.UpsertSession(ctx, Session{ID: id, Project: "p", Machine: "local", Agent: "claude", FilePath: &p}))
	}
	require.NoError(t, d.UpsertSession(ctx, Session{ID: "no-path", Project: "p", Machine: "local", Agent: "claude"}))
	require.NoError(t, d.SoftDeleteSession(ctx, "deleted"))

	for _, dir := range []string{root, root + sep} {
		ids, err := d.SessionIDsUnderPath(ctx, dir)
		require.NoError(t, err)
		assert.Equal(t, []string{"in-1", "in-2"}, ids, dir)
	}
}
