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

	// Sessions for brain-artifact mapping: one with both .db and .pb
	// sources, one implicit-only, and one with no source at all.
	bothUUID := "44444444-5555-6666-7777-888888888888"
	implUUID := "55555555-6666-7777-8888-999999999999"
	orphanUUID := "66666666-7777-8888-9999-aaaaaaaaaaaa"
	bothDBPath := filepath.Join(convDir, bothUUID+".db")
	bothPbPath := filepath.Join(convDir, bothUUID+".pb")
	implOnlyPbPath := filepath.Join(implDir, implUUID+".pb")
	require.NoError(t, os.WriteFile(bothDBPath, []byte("db"), 0o644))
	require.NoError(t, os.WriteFile(bothPbPath, []byte("pb"), 0o644))
	require.NoError(t, os.WriteFile(implOnlyPbPath, []byte("pb"), 0o644))

	brainFiles := map[string]string{}
	for _, id := range []string{uuid, dbUUID, bothUUID, implUUID, orphanUUID} {
		brainDir := filepath.Join(dir, "brain", id)
		require.NoError(t, os.MkdirAll(brainDir, 0o755))
		p := filepath.Join(brainDir, "task.md")
		require.NoError(t, os.WriteFile(p, []byte("brain"), 0o644))
		brainFiles[id] = p
	}

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
			name:    "brain artifact maps to db source",
			path:    brainFiles[dbUUID],
			want:    true,
			retPath: dbPath,
		},
		{
			name:    "brain artifact prefers db over pb source",
			path:    brainFiles[bothUUID],
			want:    true,
			retPath: bothDBPath,
		},
		{
			name:    "brain artifact maps to conversations pb source",
			path:    brainFiles[uuid],
			want:    true,
			retPath: pbPath,
		},
		{
			name:    "brain artifact maps to implicit pb source",
			path:    brainFiles[implUUID],
			want:    true,
			retPath: implOnlyPbPath,
		},
		{
			// Deleted brain paths must still classify so the session
			// reparses and drops the stale message.
			name:    "deleted brain artifact maps to db source",
			path:    filepath.Join(dir, "brain", dbUUID, "gone.md"),
			want:    true,
			retPath: dbPath,
		},
		{
			name: "brain artifact without source is ignored",
			path: brainFiles[orphanUUID],
			want: false,
		},
		{
			name: "brain artifact with invalid id is ignored",
			path: filepath.Join(dir, "brain", "not-a-uuid", "task.md"),
			want: false,
		},
		{
			name: "nested brain files are ignored",
			path: filepath.Join(
				dir, "brain", dbUUID, "sub", "task.md",
			),
			want: false,
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
