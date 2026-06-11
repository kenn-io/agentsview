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
	geminiMap := make(map[string]map[string]string)

	tests := []struct {
		name    string
		path    string
		want    bool
		retPath string // expected Path in DiscoveredFile
	}{
		{
			name:    "conversations db file is classified",
			path:    dbPath,
			want:    true,
			retPath: dbPath,
		},
		{
			name:    "db wal maps to db file",
			path:    dbWalPath,
			want:    true,
			retPath: dbPath,
		},
		{
			name:    "db shm maps to db file",
			path:    dbShmPath,
			want:    true,
			retPath: dbPath,
		},
		{
			name:    "annotation maps to db file",
			path:    annPath,
			want:    true,
			retPath: dbPath,
		},
		{
			name:    "brain artifact maps to db file",
			path:    brainMdPath,
			want:    true,
			retPath: dbPath,
		},
		{
			name:    "brain artifact metadata maps to db file",
			path:    brainMetaPath,
			want:    true,
			retPath: dbPath,
		},
		{
			// Deleted sidecar paths must still classify so the
			// session reparses and drops the stale message.
			name:    "deleted annotation maps to db file",
			path:    filepath.Join(annDir, bare+".pbtxt"),
			want:    true,
			retPath: bareDBPath,
		},
		{
			name: "deleted brain artifact maps to db file",
			path: filepath.Join(
				dir, "brain", bare, "gone.md",
			),
			want:    true,
			retPath: bareDBPath,
		},
		{
			name: "annotation without db is ignored",
			path: orphanAnnPath,
			want: false,
		},
		{
			name: "brain artifact without db is ignored",
			path: orphanBrainPath,
			want: false,
		},
		{
			name: "annotation with invalid id is ignored",
			path: badAnnPath,
			want: false,
		},
		{
			name: "unrelated conversations file is ignored",
			path: filepath.Join(convDir, "readme.md"),
			want: false,
		},
		{
			name: "nested files under conversations subdirs are ignored",
			path: filepath.Join(convDir, "subdir", uuid+".db"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := eng.classifyOnePath(tt.path, geminiMap)
			assert.Equal(t, tt.want, ok)
			if ok {
				assert.Equal(t, parser.AgentAntigravity, got.Agent)
				assert.Equal(t, tt.retPath, got.Path)
			}
		})
	}
}
