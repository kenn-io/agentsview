//go:build pgtest

package postgres

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func supersessionFixture(t *testing.T) (projectionFixture, string) {
	t.Helper()
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "carry", "")
	outcome := projectionOutcome("retained")
	outcome.Outcome.Results[0].Result.Messages[0].SourceUUID = "uuid"
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, outcome))
	_, err := f.runtime.Exec(`INSERT INTO sessions(id,project,machine,agent,display_name,deleted_at,labels) VALUES('codex:portable','legacy','device','codex','legacy title',clock_timestamp(),ARRAY['retained-label']); INSERT INTO starred_sessions(session_id) VALUES('codex:portable'); INSERT INTO messages(session_id,ordinal,role,content,content_length,source_uuid) VALUES('codex:portable',9,'user','retained',8,'uuid'); INSERT INTO pinned_messages(session_id,message_id,ordinal,source_uuid,note) VALUES('codex:portable',9,9,'uuid','legacy note')`)
	require.NoError(t, err)
	var group string
	require.NoError(t, f.runtime.QueryRow(`SELECT group_id FROM raw_session_groups`).Scan(&group))
	return f, group
}

func TestRawSupersessionUnresolvedPins(t *testing.T) {
	f, group := supersessionFixture(t)
	_, err := f.runtime.Exec(`INSERT INTO messages(session_id,ordinal,role,content,content_length,source_uuid) VALUES('codex:portable',10,'user','changed',7,'uuid'),('codex:portable',11,'user','missing',7,'absent'); INSERT INTO pinned_messages(session_id,message_id,ordinal,source_uuid,note) VALUES('codex:portable',10,10,'uuid','reused'),('codex:portable',11,11,'absent','missing')`)
	require.NoError(t, err)
	tx, err := f.runtime.BeginTx(t.Context(), &sql.TxOptions{})
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	carry, err := prepareRawLegacyCarry(t.Context(), tx, "codex:portable", group)
	require.NoError(t, err)
	assert.Equal(t, 2, carry.UnresolvedPins)
	require.Len(t, carry.Pins, 1)
	assert.Equal(t, "legacy note", carry.Pins[0].Note)
}
