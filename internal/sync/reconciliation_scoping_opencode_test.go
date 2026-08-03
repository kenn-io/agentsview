package sync_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

func seedOpenCodeContainerSessions(
	t *testing.T, oc *openCodeTestDB, base int64, ids ...string,
) {
	t.Helper()
	oc.addProject(t, "proj-1", "/home/user/code/myapp")
	for _, id := range ids {
		oc.addSession(t, id, "proj-1", base, base+5000)
		oc.addMessage(t, id+"-msg-u", id, "user", base)
		oc.addMessage(t, id+"-msg-a", id, "assistant", base+1)
		oc.addTextPart(t, id+"-part-u", id, id+"-msg-u",
			"original question "+id, base)
		oc.addTextPart(t, id+"-part-a", id, id+"-msg-a",
			"original answer "+id, base+1)
	}
}

// TestReconcileProviderRootsOpenCodeContainerSyncsAndTombstonesMembers pins
// the exact-container request: a pass asked about opencode.db itself must
// prove the container's whole virtual membership, syncing a changed member
// and reclaiming a removed one, instead of resolving a proof no member row
// can match.
func TestReconcileProviderRootsOpenCodeContainerSyncsAndTombstonesMembers(
	t *testing.T,
) {
	physicalRoot := t.TempDir()
	aliasParent := t.TempDir()
	aliasRoot := filepath.Join(aliasParent, "opencode-root")
	if err := os.Symlink(physicalRoot, aliasRoot); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	env := setupSingleAgentTestEnvWithDirs(t, parser.AgentOpenCode, []string{aliasRoot})
	oc := createOpenCodeDB(t, physicalRoot)
	base := int64(1704067200000)
	seedOpenCodeContainerSessions(
		t, oc, base, "oc-container-kept", "oc-container-removed",
	)
	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 2, Synced: 2,
	})

	// One member changes and one disappears; the pass is asked about the
	// container path, not the configured root.
	oc.replaceTextContent(t, "oc-container-kept",
		"updated question", "updated answer", base)
	oc.updateSessionTime(t, "oc-container-kept", base+9000)
	oc.deleteParts(t, "oc-container-removed")
	oc.deleteMessages(t, "oc-container-removed")
	oc.mustExec(t, "delete session",
		"DELETE FROM session WHERE id = ?", "oc-container-removed")

	require.NoError(t, env.engine.ReconcileProviderRoots(
		t.Context(), parser.AgentOpenCode, []string{oc.path},
	))

	assertMessageContent(t, env.db, "opencode:oc-container-kept",
		"updated question", "updated answer")
	removed, err := env.db.GetSession(
		context.Background(), "opencode:oc-container-removed",
	)
	require.NoError(t, err)
	assert.Nil(t, removed,
		"a container-scoped pass reclaims a removed member")
}

func TestReconcileBoundedCoverageSourceLeaseTombstonesDeletedMembers(
	t *testing.T,
) {
	env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
	oc := createOpenCodeDB(t, env.opencodeDir)
	base := int64(1704067200000)
	seedOpenCodeContainerSessions(
		t, oc, base, "oc-lease-kept", "oc-lease-removed",
	)
	oc.mustExec(t, "bounded journal event table", `
		CREATE TABLE event (
			id TEXT NOT NULL PRIMARY KEY, aggregate_id TEXT NOT NULL,
			seq INTEGER NOT NULL, type TEXT NOT NULL, data BLOB NOT NULL
		);
		CREATE TABLE event_sequence (id TEXT NOT NULL PRIMARY KEY, owner_id TEXT);
	`)
	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 2, Synced: 2})

	bindings, err := env.engine.BoundedCoverageBindings(
		t.Context(), []sync.BoundedCoverageRoot{{
			Agent: parser.AgentOpenCode, Root: env.opencodeDir,
		}},
	)
	require.NoError(t, err)
	require.Len(t, bindings, 1)
	bindings[0].Generation = 1
	lease, err := env.engine.AdmitBoundedCoverageLease(t.Context(), bindings[0])
	require.NoError(t, err)
	_, err = env.engine.TransitionBoundedCoverageRequest(
		t.Context(), lease, nil, lease.AdmissionCheckpoint, true,
	)
	require.NoError(t, err)

	oc.deleteParts(t, "oc-lease-removed")
	oc.deleteMessages(t, "oc-lease-removed")
	oc.mustExec(t, "delete session",
		"DELETE FROM session WHERE id = ?", "oc-lease-removed")
	require.NoError(t, env.engine.ReconcileBoundedCoverageSourceLease(
		t.Context(), lease, "structural journal evidence",
	))

	removed, err := env.db.GetSession(t.Context(), "opencode:oc-lease-removed")
	require.NoError(t, err)
	assert.Nil(t, removed,
		"lease-bound repair must reconcile container membership, including tombstones")
	kept, err := env.db.GetSession(t.Context(), "opencode:oc-lease-kept")
	require.NoError(t, err)
	assert.NotNil(t, kept)
}

// TestReconcileProviderRootsOpenCodeMemberPassCannotTrustPartialMembership
// pins the trust-promotion invariant end to end: a pass asked about one
// virtual member widens to the whole container, so completing it never
// promotes container-state trust over a sibling it did not verify. Without
// the widening, the member pass records one discovered and one completed
// member, trusts the already-changed container state, and the covering pass
// gate-skips the stale sibling for as long as the database stays unchanged.
func TestReconcileProviderRootsOpenCodeMemberPassCannotTrustPartialMembership(
	t *testing.T,
) {
	env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
	oc := createOpenCodeDB(t, env.opencodeDir)
	base := int64(1704067200000)
	seedOpenCodeContainerSessions(t, oc, base, "oc-member-a", "oc-member-b")
	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 2, Synced: 2,
	})

	// Both members change; the pass is asked about member A only.
	oc.replaceTextContent(t, "oc-member-a",
		"updated question a", "updated answer a", base)
	oc.updateSessionTime(t, "oc-member-a", base+9000)
	oc.replaceTextContent(t, "oc-member-b",
		"updated question b", "updated answer b", base)
	oc.updateSessionTime(t, "oc-member-b", base+9000)

	require.NoError(t, env.engine.ReconcileProviderRoots(
		t.Context(), parser.AgentOpenCode,
		[]string{parser.OpenCodeSQLiteVirtualPath(oc.path, "oc-member-a")},
	))

	// The database does not change again between the passes, so any trust
	// the member pass promoted is exactly what the covering pass consults.
	require.NoError(t, env.engine.ReconcileProviderRoots(
		t.Context(), parser.AgentOpenCode, []string{env.opencodeDir},
	))

	assertMessageContent(t, env.db, "opencode:oc-member-b",
		"updated question b", "updated answer b")
}
