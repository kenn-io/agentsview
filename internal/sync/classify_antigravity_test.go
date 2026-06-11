package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func TestClassifyOnePath_Antigravity(t *testing.T) {
	dir := t.TempDir()
	uuid := "11111111-2222-3333-4444-555555555555"
	orphan := "22222222-3333-4444-5555-666666666666"
	// Session whose sidecars were deleted: only the .db remains.
	bare := "33333333-4444-5555-6666-777777777777"

	convDir := filepath.Join(dir, "conversations")
	annDir := filepath.Join(dir, "annotations")
	brainDir := filepath.Join(dir, "brain", uuid)
	orphanBrainDir := filepath.Join(dir, "brain", orphan)
	require.NoError(t, os.MkdirAll(convDir, 0o755))
	require.NoError(t, os.MkdirAll(annDir, 0o755))
	require.NoError(t, os.MkdirAll(brainDir, 0o755))
	require.NoError(t, os.MkdirAll(orphanBrainDir, 0o755))

	dbPath := filepath.Join(convDir, uuid+".db")
	dbWalPath := dbPath + "-wal"
	dbShmPath := dbPath + "-shm"
	annPath := filepath.Join(annDir, uuid+".pbtxt")
	brainMdPath := filepath.Join(brainDir, "task.md")
	brainMetaPath := filepath.Join(brainDir, "task.md.metadata.json")
	bareDBPath := filepath.Join(convDir, bare+".db")

	// Sidecars whose conversation .db does not exist.
	orphanAnnPath := filepath.Join(annDir, orphan+".pbtxt")
	orphanBrainPath := filepath.Join(orphanBrainDir, "task.md")
	// Annotation-like file whose stem is not a session id.
	badAnnPath := filepath.Join(annDir, "readme.pbtxt")

	for _, p := range []string{
		dbPath, dbWalPath, dbShmPath, annPath, brainMdPath,
		brainMetaPath, orphanAnnPath, orphanBrainPath, badAnnPath,
		bareDBPath, filepath.Join(convDir, "readme.md"),
	} {
		require.NoError(t, os.WriteFile(p, []byte("x"), 0o644))
	}

	eng := &Engine{
		agentDirs: map[parser.AgentType][]string{
			parser.AgentAntigravity: {dir},
		},
	}

	tests := []struct {
		name      string
		path      string
		wantPaths []string
	}{
		{
			name:      "conversations db file is classified",
			path:      dbPath,
			wantPaths: []string{dbPath},
		},
		{
			name:      "db wal maps to db file",
			path:      dbWalPath,
			wantPaths: []string{dbPath},
		},
		{
			name:      "db shm maps to db file",
			path:      dbShmPath,
			wantPaths: []string{dbPath},
		},
		{
			name:      "annotation maps to db file",
			path:      annPath,
			wantPaths: []string{dbPath},
		},
		{
			name:      "brain artifact maps to db file",
			path:      brainMdPath,
			wantPaths: []string{dbPath},
		},
		{
			name:      "brain artifact metadata maps to db file",
			path:      brainMetaPath,
			wantPaths: []string{dbPath},
		},
		{
			// Deleted sidecar paths must still classify so the
			// session reparses and drops the stale message.
			name:      "deleted annotation maps to db file",
			path:      filepath.Join(annDir, bare+".pbtxt"),
			wantPaths: []string{bareDBPath},
		},
		{
			name: "deleted brain artifact maps to db file",
			path: filepath.Join(
				dir, "brain", bare, "gone.md",
			),
			wantPaths: []string{bareDBPath},
		},
		{
			name:      "annotation without db is ignored",
			path:      orphanAnnPath,
			wantPaths: nil,
		},
		{
			name:      "brain artifact without db is ignored",
			path:      orphanBrainPath,
			wantPaths: nil,
		},
		{
			name:      "annotation with invalid id is ignored",
			path:      badAnnPath,
			wantPaths: nil,
		},
		{
			name:      "unrelated conversations file is ignored",
			path:      filepath.Join(convDir, "readme.md"),
			wantPaths: nil,
		},
		{
			name:      "nested files under conversations subdirs are ignored",
			path:      filepath.Join(convDir, "subdir", uuid+".db"),
			wantPaths: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := eng.classifyPaths([]string{tt.path})
			var gotPaths []string
			for _, df := range got {
				assert.Equal(t, parser.AgentAntigravity, df.Agent)
				gotPaths = append(gotPaths, df.Path)
			}
			assert.ElementsMatch(t, tt.wantPaths, gotPaths)
		})
	}
}
