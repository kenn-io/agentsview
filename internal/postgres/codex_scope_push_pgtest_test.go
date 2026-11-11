//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPushSessionWithCodexScope(t *testing.T) {
	syncer, local, pg, ctx := newSessionProvenancePushSync(t, "agentsview_codex_scope_push_test")
	seedProvenanceSession(t, local, "session-1", "workstation", "project", "")
	result, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 1, result.SessionsPushed)
	var id string
	require.NoError(t, pg.QueryRowContext(ctx, `SELECT id FROM sessions WHERE id = $1`, "session-1").Scan(&id))
	require.Equal(t, "session-1", id)
}
