package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func TestClassifyOnePath_AntigravityCLI(t *testing.T) {
	dir := t.TempDir()
	uuid := "11111111-2222-3333-4444-555555555555"
	dbUUID := "33333333-4444-5555-6666-777777777777"

	// Create conversations and implicit subdirectories
	convDir := filepath.Join(dir, "conversations")
	implDir := filepath.Join(dir, "implicit")
	require.NoError(t, os.MkdirAll(convDir, 0o755))
	require.NoError(t, os.MkdirAll(implDir, 0o755))

	// Files under conversations
	pbPath := filepath.Join(convDir, uuid+".pb")
	trajPath := filepath.Join(convDir, uuid+".trajectory.json")
	dbPath := filepath.Join(convDir, dbUUID+".db")
	dbWalPath := dbPath + "-wal"
	dbShmPath := dbPath + "-shm"

	require.NoError(t, os.WriteFile(pbPath, []byte("pb-data"), 0o644))
	require.NoError(t, os.WriteFile(trajPath, []byte("trajectory-data"), 0o644))
	require.NoError(t, os.WriteFile(dbPath, []byte("sqlite-data"), 0o644))
	require.NoError(t, os.WriteFile(dbWalPath, []byte("wal-data"), 0o644))
	require.NoError(t, os.WriteFile(dbShmPath, []byte("shm-data"), 0o644))

	// Files under implicit
	implPbPath := filepath.Join(implDir, uuid+".pb")
	implTrajPath := filepath.Join(implDir, uuid+".trajectory.json")

	require.NoError(t, os.WriteFile(implPbPath, []byte("pb-data"), 0o644))
	require.NoError(t, os.WriteFile(implTrajPath, []byte("trajectory-data"), 0o644))

	eng := &Engine{
		agentDirs: map[parser.AgentType][]string{
			parser.AgentAntigravityCLI: {dir},
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
			name:    "conversations pb file is classified",
			path:    pbPath,
			want:    true,
			retPath: pbPath,
		},
		{
			name:    "conversations trajectory file maps to pb file",
			path:    trajPath,
			want:    true,
			retPath: pbPath,
		},
		{
			name:    "conversations db file is classified",
			path:    dbPath,
			want:    true,
			retPath: dbPath,
		},
		{
			name:    "conversations db wal maps to db file",
			path:    dbWalPath,
			want:    true,
			retPath: dbPath,
		},
		{
			name:    "conversations db shm maps to db file",
			path:    dbShmPath,
			want:    true,
			retPath: dbPath,
		},
		{
			name:    "implicit pb file is classified",
			path:    implPbPath,
			want:    true,
			retPath: implPbPath,
		},
		{
			name:    "implicit trajectory file maps to implicit pb file",
			path:    implTrajPath,
			want:    true,
			retPath: implPbPath,
		},
		{
			name: "unrelated files are ignored",
			path: filepath.Join(convDir, "readme.md"),
			want: false,
		},
		{
			name: "nested files under subdirs are ignored",
			path: filepath.Join(convDir, "subdir", uuid+".pb"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := eng.classifyOnePath(tt.path, geminiMap)
			assert.Equal(t, tt.want, ok)
			if ok {
				assert.Equal(t, parser.AgentAntigravityCLI, got.Agent)
				assert.Equal(t, tt.retPath, got.Path)
			}
		})
	}

	// Test missing pb file behavior
	t.Run("trajectory without pb is ignored", func(t *testing.T) {
		orphanUUID := "22222222-3333-4444-5555-666666666666"
		orphanTraj := filepath.Join(convDir, orphanUUID+".trajectory.json")
		require.NoError(t, os.WriteFile(orphanTraj, []byte("orphan"), 0o644))

		_, ok := eng.classifyOnePath(orphanTraj, geminiMap)
		assert.False(t, ok, "should not classify sidecar when pb file does not exist")
	})
}
