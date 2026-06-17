package ssh

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func newRemoteSkipTestDB(t *testing.T) *db.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	database, err := db.Open(dbPath)
	require.NoError(t, err, "opening test db")
	t.Cleanup(func() { _ = database.Close() })
	return database
}

const vsCopilotRemoteSkipTracePath = "/remote/agent/foo_VSGitHubCopilot_traces.jsonl"

// TestMigrateVisualStudioCopilotRemoteSkipsRemovesStaleTracePaths
// verifies that stale Visual Studio Copilot trace skips, both
// physical and virtual, are scrubbed from a host's remote skip
// cache and the cleaned cache is persisted, while unrelated
// entries are preserved.
func TestMigrateVisualStudioCopilotRemoteSkipsRemovesStaleTracePaths(
	t *testing.T,
) {
	database := newRemoteSkipTestDB(t)
	const host = "remote-host"
	virtualPath := parser.VisualStudioCopilotVirtualPath(
		vsCopilotRemoteSkipTracePath,
		"4a8f63f6-7626-4416-a874-fc7bd2c3f005",
	)
	const unrelated = "/remote/agent/.codex/sessions/abc.jsonl"
	seed := map[string]int64{
		vsCopilotRemoteSkipTracePath: 111,
		virtualPath:                  222,
		unrelated:                    333,
	}
	require.NoError(
		t, database.ReplaceRemoteSkippedFiles(host, seed),
	)

	rs := &RemoteSync{Host: host, DB: database}
	cleaned := rs.migrateVisualStudioCopilotRemoteSkips(seed)

	assert.NotContains(t, cleaned, vsCopilotRemoteSkipTracePath)
	assert.NotContains(t, cleaned, virtualPath)
	assert.Contains(t, cleaned, unrelated)

	persisted, err := database.LoadRemoteSkippedFiles(host)
	require.NoError(t, err)
	assert.NotContains(t, persisted, vsCopilotRemoteSkipTracePath)
	assert.NotContains(t, persisted, virtualPath)
	assert.Contains(t, persisted, unrelated)
}

// TestMigrateVisualStudioCopilotRemoteSkipsIsOneTimePerHost
// verifies the scrub runs once per host: a conversation skip
// legitimately re-cached after the first pass is preserved
// rather than being filtered out on every later sync.
func TestMigrateVisualStudioCopilotRemoteSkipsIsOneTimePerHost(
	t *testing.T,
) {
	database := newRemoteSkipTestDB(t)
	const host = "remote-host"

	require.NoError(t, database.ReplaceRemoteSkippedFiles(
		host, map[string]int64{vsCopilotRemoteSkipTracePath: 111},
	))
	rs := &RemoteSync{Host: host, DB: database}

	first := rs.migrateVisualStudioCopilotRemoteSkips(
		map[string]int64{vsCopilotRemoteSkipTracePath: 111},
	)
	assert.NotContains(t, first, vsCopilotRemoteSkipTracePath)

	fresh := map[string]int64{vsCopilotRemoteSkipTracePath: 222}
	require.NoError(
		t, database.ReplaceRemoteSkippedFiles(host, fresh),
	)
	second := rs.migrateVisualStudioCopilotRemoteSkips(fresh)
	assert.Contains(
		t, second, vsCopilotRemoteSkipTracePath,
		"migration must not re-scrub after the one-time pass",
	)
}

// TestMigrateVisualStudioCopilotRemoteSkipsPerHost verifies the
// one-time flag is tracked per host: scrubbing one host does not
// suppress the scrub for a different host's stale entries.
func TestMigrateVisualStudioCopilotRemoteSkipsPerHost(
	t *testing.T,
) {
	database := newRemoteSkipTestDB(t)
	require.NoError(t, database.ReplaceRemoteSkippedFiles(
		"host-a", map[string]int64{vsCopilotRemoteSkipTracePath: 1},
	))
	require.NoError(t, database.ReplaceRemoteSkippedFiles(
		"host-b", map[string]int64{vsCopilotRemoteSkipTracePath: 2},
	))

	rsA := &RemoteSync{Host: "host-a", DB: database}
	rsA.migrateVisualStudioCopilotRemoteSkips(
		map[string]int64{vsCopilotRemoteSkipTracePath: 1},
	)

	rsB := &RemoteSync{Host: "host-b", DB: database}
	cleanedB := rsB.migrateVisualStudioCopilotRemoteSkips(
		map[string]int64{vsCopilotRemoteSkipTracePath: 2},
	)
	assert.NotContains(t, cleanedB, vsCopilotRemoteSkipTracePath)

	persistedB, err := database.LoadRemoteSkippedFiles("host-b")
	require.NoError(t, err)
	assert.NotContains(t, persistedB, vsCopilotRemoteSkipTracePath)
}
