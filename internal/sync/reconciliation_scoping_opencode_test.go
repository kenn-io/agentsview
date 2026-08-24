package sync_test

import (
	"context"
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
	env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
	oc := createOpenCodeDB(t, env.opencodeDir)
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
	assert.NotNil(t, removed,
		"a container-scoped pass leaves a removed member browsable")
	archived, err := env.db.GetSessionFull(
		context.Background(), "opencode:oc-container-removed",
	)
	require.NoError(t, err)
	assertSourceMissingState(t, archived)
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
