package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
)

func TestMergeWatchDepth(t *testing.T) {
	tests := []struct {
		name       string
		recursiveA bool
		depthA     int
		recursiveB bool
		depthB     int
		want       int
	}{
		{"both limited keeps deeper", true, 1, true, 3, 3},
		{"unlimited recursive wins", true, 1, true, 0, 0},
		{"shallow plan does not limit", true, 2, false, 0, 2},
		{"shallow plan depth ignored", false, 5, true, 1, 1},
		{"neither recursive", false, 1, false, 2, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, MergeWatchDepth(
				tt.recursiveA, tt.depthA, tt.recursiveB, tt.depthB,
			))
		})
	}
}

func TestMergeExtraDirectories(t *testing.T) {
	assert.Nil(t, MergeExtraDirectories(nil, nil))
	assert.Equal(t, []string{"*/logs"}, MergeExtraDirectories([]string{"*/logs"}, nil))
	assert.Equal(t, []string{"*/logs", "*/other"}, MergeExtraDirectories(
		[]string{"*/logs"}, []string{"*/other", "*/logs", ""},
	))
}

func TestWatchDepthShouldDropEventAntigravityCLIContainer(t *testing.T) {
	const id = "66666666-7777-8888-9999-aaaaaaaaaaaa"

	layouts := []struct {
		name      string
		setupRoot func(t *testing.T, base string) (container, instanceDir, sessionRoot string)
	}{
		{
			name: "nested instance container (T3 Code)",
			setupRoot: func(t *testing.T, base string) (string, string, string) {
				t.Helper()
				container := filepath.Join(base, ".t3", "userdata", "providers", "antigravity")
				require.NoError(t, os.MkdirAll(container, 0o755))
				instanceDir := filepath.Join(container, "insthash")
				return container, instanceDir, filepath.Join(instanceDir, "antigravity-acp")
			},
		},
		{
			name: "direct instance container",
			setupRoot: func(t *testing.T, base string) (string, string, string) {
				t.Helper()
				container := filepath.Join(base, "my-harness", "profiles")
				require.NoError(t, os.MkdirAll(filepath.Join(container, "existing", "conversations"), 0o755))
				instanceDir := filepath.Join(container, "work")
				return container, instanceDir, instanceDir
			},
		},
		{
			name: "single profile container",
			setupRoot: func(t *testing.T, base string) (string, string, string) {
				t.Helper()
				container := filepath.Join(base, "isolated-profile")
				acp := filepath.Join(container, "antigravity-acp")
				require.NoError(t, os.MkdirAll(acp, 0o755))
				return container, acp, acp
			},
		},
	}

	for _, tc := range layouts {
		t.Run(tc.name, func(t *testing.T) {
			container, instanceDir, sessionRoot := tc.setupRoot(t, t.TempDir())
			provider, ok := parser.NewProvider(parser.AgentAntigravityCLI, parser.ProviderConfig{
				Roots: []string{container},
			})
			require.True(t, ok)
			plan, err := provider.WatchPlan(t.Context())
			require.NoError(t, err)
			require.Len(t, plan.Roots, 1)

			limit := plan.Roots[0].MaxDepth
			patterns := plan.Roots[0].ExtraDirectories

			brainSession := filepath.Join(sessionRoot, "brain", id)
			generated := filepath.Join(brainSession, ".system_generated")
			logsDir := filepath.Join(generated, "logs")

			// Instance creation, session directories, conversations, artifacts,
			// and transcripts must stay visible.
			keepDirs := []string{
				instanceDir,
				sessionRoot,
				filepath.Join(sessionRoot, "conversations"),
				brainSession,
				generated,
				logsDir,
			}
			for _, dir := range keepDirs {
				assert.False(t, watchDepthShouldDropEvent(dir, container, backendItemDirectory, limit, patterns),
					"dir %s must not be dropped", dir)
				assert.True(t, watchDepthIncludePath(container, dir, true, limit, patterns),
					"dir %s must be included in created subtree walk", dir)
			}

			keepFiles := []string{
				filepath.Join(sessionRoot, "history.jsonl"),
				filepath.Join(sessionRoot, "conversations", id+".db-wal"),
				filepath.Join(sessionRoot, "implicit", id+".pb"),
				filepath.Join(sessionRoot, "cache", "last_conversations.json"),
				filepath.Join(brainSession, "plan.md"),
				filepath.Join(brainSession, "plan.md.metadata.json"),
				filepath.Join(logsDir, "transcript.jsonl"),
			}
			for _, file := range keepFiles {
				assert.False(t, watchDepthShouldDropEvent(file, container, backendItemFile, limit, patterns),
					"file %s must not be dropped", file)
				assert.False(t, watchDepthShouldDropEvent(file, container, backendItemUnknown, limit, patterns),
					"unknown-type file %s must not be dropped", file)
				assert.True(t, watchDepthIncludePath(container, file, false, limit, patterns),
					"file %s must be included in created subtree walk", file)
			}

			// Excluded subtrees and noise in unlisted intermediate directories
			// must be dropped so FSEvents never triggers unnecessary scans.
			dropFiles := []string{
				filepath.Join(sessionRoot, "log", "agent.log"),
				filepath.Join(sessionRoot, "builtin", "skills", "guide.md"),
				filepath.Join(brainSession, "scratch", "note.md"),
				filepath.Join(generated, "noise.txt"),
				filepath.Join(generated, "steps", "001.json"),
				filepath.Join(logsDir, "chunks", "001.bin"),
			}
			for _, file := range dropFiles {
				assert.True(t, watchDepthShouldDropEvent(file, container, backendItemFile, limit, patterns),
					"file %s must be dropped", file)
				assert.True(t, watchDepthShouldDropEvent(file, container, backendItemUnknown, limit, patterns),
					"unknown-type file %s must be dropped", file)
				assert.False(t, watchDepthIncludePath(container, file, false, limit, patterns),
					"file %s must not be included in created subtree walk", file)
			}

			dropDirs := []string{
				filepath.Join(brainSession, "scratch"),
				filepath.Join(generated, "steps"),
				filepath.Join(logsDir, "chunks"),
			}
			for _, dir := range dropDirs {
				assert.True(t, watchDepthShouldDropEvent(dir, container, backendItemDirectory, limit, patterns),
					"dir %s must be dropped", dir)
				assert.False(t, watchDepthIncludePath(container, dir, true, limit, patterns),
					"dir %s must not be included in created subtree walk", dir)
			}
		})
	}
}
