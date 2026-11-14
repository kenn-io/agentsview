package sync

import (
	"path/filepath"
	gosync "sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

// TestReconcileProviderRootsGroupedYieldsToWaitingSyncBetweenGroups is the
// starvation gate for issue 2567. An unwatched-poll pass reconciles one group
// per provider and can run for hours. A live watcher sync that starts waiting
// while the first group runs must complete before the second group starts,
// not after the whole batch and its epilogue.
func TestReconcileProviderRootsGroupedYieldsToWaitingSyncBetweenGroups(t *testing.T) {
	database := openTestDB(t)
	rootA := filepath.Join(t.TempDir(), "claude-a")
	rootB := filepath.Join(t.TempDir(), "claude-b")
	// rootLive is configured but belongs to no poll group, so only the live
	// sync can write its session.
	rootLive := filepath.Join(t.TempDir(), "claude-live")
	writeGroupedClaudeFixture(t, rootA, "grouped-a")
	writeGroupedClaudeFixture(t, rootB, "grouped-b")
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {rootA, rootB, rootLive},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	groupStarted := make(chan int, 2)
	releaseFirst := make(chan struct{})
	// Release the parked first group even when an assertion stops the test
	// early; otherwise it keeps holding syncMu and engine.Close hangs.
	release := gosync.OnceFunc(func() { close(releaseFirst) })
	t.Cleanup(release)
	liveSyncedBeforeSecond := make(chan bool, 1)
	engine.groupedReconcileGroupHook = func(index int) {
		groupStarted <- index
		switch index {
		case 0:
			<-releaseFirst
		case 1:
			session, err := database.GetSession(t.Context(), "live-session")
			liveSyncedBeforeSecond <- err == nil && session != nil
		}
	}

	grouped := make(chan error, 1)
	go func() {
		grouped <- engine.ReconcileProviderRootsGrouped(t.Context(), []ProviderRootsGroup{
			{Agent: parser.AgentClaude, Roots: []string{rootA}},
			{Agent: parser.AgentClaude, Roots: []string{rootB}},
		})
	}()
	require.Equal(t, 0, requireReceiveWithin(t, groupStarted, 10*time.Second))

	writeGroupedClaudeFixture(t, rootLive, "live-session")
	live := make(chan error, 1)
	go func() {
		live <- engine.SyncPathsContext(t.Context(), []string{
			filepath.Join(rootLive, "project", "live-session.jsonl"),
		})
	}()
	require.Eventually(t, func() bool { return engine.syncMu.waiting() > 0 },
		10*time.Second, time.Millisecond, "the live sync must be waiting on syncMu")
	release()

	require.Equal(t, 1, requireReceiveWithin(t, groupStarted, 10*time.Second))
	assert.True(t, requireReceiveWithin(t, liveSyncedBeforeSecond, 10*time.Second),
		"a sync waiting during one group must run before the next group starts")
	require.NoError(t, requireReceiveWithin(t, live, 10*time.Second))
	require.NoError(t, requireReceiveWithin(t, grouped, 10*time.Second))

	for _, id := range []string{"grouped-a", "grouped-b", "live-session"} {
		session, err := database.GetSession(t.Context(), id)
		require.NoError(t, err)
		assert.NotNil(t, session, "session %s must be synced", id)
	}
}
