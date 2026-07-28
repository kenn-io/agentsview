package sync_test

import (
	"context"
	"fmt"
	"testing"

	"go.kenn.io/agentsview/internal/parser"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOpenCodeSharedContainerChangeIsPerSessionBounded pins the "background
// sync work is bounded by the changed batch, not total archive size" rule for
// shared SQLite containers.
//
// Every session in an OpenCode root lives in one physical opencode.db. Stamping
// that container's size onto each session's fingerprint made any single
// session's write change every other session's fingerprint, so one changed
// session re-parsed the whole root — on a production container that is
// thousands of sessions re-read out of a multi-GB database every time the
// watcher fires. The per-session composite mtime (session, project, and child
// message/part time_updated) replaces it, so a one-session change must leave
// every other session skipped regardless of how many there are.
func TestOpenCodeSharedContainerChangeIsPerSessionBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	rewritten := make(map[int]int)
	for _, n := range []int{20, 200} {
		t.Run(fmt.Sprintf("sessions_%d", n), func(t *testing.T) {
			env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
			oc := createOpenCodeDB(t, env.opencodeDir)
			oc.addProject(t, "proj", "/home/user/code/app")
			for i := range n {
				seedOpenCodeSQLiteTextSession(
					t, oc, "proj", fmt.Sprintf("ses%05d", i),
					1779012000000, 1779012030000,
					"prompt", "answer",
				)
			}
			require.Equal(t, n,
				env.engine.SyncAll(context.Background(), nil).Synced)

			// Change exactly one session. This also grows the shared
			// container file, which is precisely the signal that used to
			// invalidate every other session in it.
			oc.updateSessionTime(t, "ses00000", 1779015630000)
			oc.replaceTextContent(
				t, "ses00000", "changed prompt", "changed answer",
				1779015600000,
			)

			stats := env.engine.SyncAll(context.Background(), nil)
			require.False(t, stats.Aborted, "sync aborted: %+v", stats)
			assert.Equal(t, 1, stats.Synced,
				"only the changed session may be rewritten")
			assert.Equal(t, n-1, stats.Skipped,
				"every unchanged session in the shared container must skip")
			rewritten[n] = stats.Synced
		})
	}

	assert.Equal(t, rewritten[20], rewritten[200],
		"sessions rewritten for one changed session must not grow with "+
			"container size")
}
