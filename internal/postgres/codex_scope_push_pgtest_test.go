//go:build pgtest

package postgres

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestPushSessionWithCodexScope(t *testing.T) {
	syncer, local, pg, ctx := newSessionProvenancePushSync(t, "agentsview_codex_scope_push_test")
	seedProvenanceSession(t, local, "session-1", "workstation", "project", "")
	result, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 1, result.SessionsPushed)
	const marker = "0123456789abcdef0123456789abcdef"
	const suffix = "_99999999-9999-4999-8999-999999999999"
	threads := []string{
		"codex:11111111-1111-4111-8111-111111111111",
		"codex:22222222-2222-4222-8222-222222222222",
		"codex:33333333-3333-4333-8333-333333333333",
	}
	for i, machine := range []string{"old-machine", "old-alias", "unrelated"} {
		_, err = pg.ExecContext(ctx, `INSERT INTO excluded_sessions (id, owner_marker, machine, agent, include_codex_pages) VALUES ($1, '', $2, 'codex', TRUE)`, threads[i], machine)
		require.NoError(t, err)
	}
	_, err = pg.ExecContext(ctx, `INSERT INTO sync_metadata (key, value) VALUES ($1, 'old-machine'), ($2, '["old-alias"]'), ($3, 'unrelated'), ($4, 'unrelated')`,
		"push_marker:"+marker+":scope:0000000000000001", "push_marker_machine_aliases:"+marker+":scope:0000000000000002", "xpush_marker:"+marker+":scope:0000000000000003", "pushXmarker:"+marker+":scope:0000000000000004")
	require.NoError(t, err)
	sessions := []db.Session{
		{ID: threads[0] + suffix, Agent: "codex", Machine: "workstation"},
		{ID: threads[1] + suffix, Agent: "codex", Machine: "workstation"},
		{ID: threads[2] + suffix, Agent: "codex", Machine: "workstation"},
	}
	excluded, err := readPGExcludedSessionIDs(ctx, pg, slices.Values(sessions), marker, "workstation", nil)
	require.NoError(t, err)
	require.Equal(t, map[string]bool{sessions[0].ID: true, sessions[1].ID: true}, excluded)
}
