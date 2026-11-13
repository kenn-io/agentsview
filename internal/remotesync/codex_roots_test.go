package remotesync

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func TestCodexMirrorRootsPersistence(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mirror")
	p := &PreparedRemote{root: root, targets: TargetSet{Dirs: map[parser.AgentType][]string{parser.AgentCodex: {"/one", "/two"}}}}
	ctx := t.Context()
	require.NoError(t, p.persistCodexRoots(ctx))
	got, err := LoadMirrorCodexRoots(ctx, root)
	require.NoError(t, err)
	assert.Equal(t, []string{"/one", "/two"}, got)
	old := time.Unix(100, 0)
	name := mirrorCodexRootsPath(root)
	require.NoError(t, os.Chtimes(name, old, old))
	require.NoError(t, p.persistCodexRoots(ctx))
	info, err := os.Stat(name)
	require.NoError(t, err)
	assert.Equal(t, old, info.ModTime())
	p.targets.Dirs[parser.AgentCodex] = nil
	require.NoError(t, p.persistCodexRoots(ctx))
	got, err = LoadMirrorCodexRoots(ctx, root)
	require.NoError(t, err)
	assert.Empty(t, got)
	newer := []byte(`{"version":2,"dirs":["/future"]}`)
	require.NoError(t, os.WriteFile(name, newer, 0o600))
	require.NoError(t, p.persistCodexRoots(ctx))
	data, err := os.ReadFile(name)
	require.NoError(t, err)
	assert.Equal(t, newer, data)
	_, err = LoadMirrorCodexRoots(ctx, root)
	require.ErrorContains(t, err, "unsupported")
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	require.ErrorIs(t, p.persistCodexRoots(cancelled), context.Canceled)
	_, err = LoadMirrorCodexRoots(cancelled, root)
	require.ErrorIs(t, err, context.Canceled)
}
