package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFrictionSchemaExists(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	tests := []struct {
		table   string
		columns []string
	}{
		{"friction_findings", []string{
			"id", "session_id", "kind", "detector", "message_ordinal",
			"call_index", "tool_name", "label", "text", "evidence",
			"title", "fingerprint", "occurred_at", "seq",
			"rules_version", "created_at",
		}},
		{"friction_session_dims", []string{
			"session_id", "seat", "persona", "channel", "dims_source",
			"review_excluded",
		}},
		{"sessions", []string{
			"friction_count", "friction_rules_version", "friction_hash",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.table, func(t *testing.T) {
			rows, err := d.getReader().QueryContext(ctx,
				"SELECT name FROM pragma_table_info(?)", tt.table)
			require.NoError(t, err)
			defer rows.Close()
			have := map[string]bool{}
			for rows.Next() {
				var name string
				require.NoError(t, rows.Scan(&name))
				have[name] = true
			}
			require.NoError(t, rows.Err())
			for _, c := range tt.columns {
				assert.True(t, have[c], "%s.%s missing", tt.table, c)
			}
		})
	}
	for _, index := range []string{
		"idx_friction_findings_session",
		"idx_friction_findings_fingerprint",
		"idx_friction_findings_kind",
	} {
		var present bool
		require.NoError(t, d.getReader().QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM sqlite_master
			 WHERE type = 'index' AND name = ?)`, index,
		).Scan(&present))
		assert.True(t, present, index)
	}
}

func TestFrictionSessionColumnsMigrateOnOldArchive(t *testing.T) {
	path := createClosedTestDB(t, tempDBPath(t, "sessions.db"), nil)
	for _, c := range []string{
		"friction_count", "friction_rules_version", "friction_hash",
	} {
		execRawSQLite(t, path, "ALTER TABLE sessions DROP COLUMN "+c)
	}
	execRawSQLite(t, path, "DROP TABLE friction_findings")
	execRawSQLite(t, path, "DROP TABLE friction_session_dims")

	d, err := Open(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	insertSession(t, d, "s1", "proj")

	s, err := d.GetSessionFull(t.Context(), "s1")
	require.NoError(t, err)
	require.NotNil(t, s)
	assert.Zero(t, s.FrictionCount)
	assert.Empty(t, s.FrictionRulesVersion)
	assert.Empty(t, s.FrictionHash)
	for _, table := range []string{"friction_findings", "friction_session_dims"} {
		var present bool
		require.NoError(t, d.getReader().QueryRow(t.Context(),
			`SELECT EXISTS(SELECT 1 FROM sqlite_master
			 WHERE type = 'table' AND name = ?)`, table,
		).Scan(&present))
		assert.True(t, present, "%s table missing after writable open", table)
	}
}

func TestReplaceSessionMessagesRevokesFrictionVersion(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "s1", "proj")
	require.NoError(t, d.ReplaceSessionMessages(ctx, "s1", []Message{
		{SessionID: "s1", Ordinal: 0, Role: "user", Content: "first"},
	}))
	_, err := d.getWriter().Exec(ctx,
		`UPDATE sessions SET friction_rules_version = 'friction-v1',
		 friction_hash = 'h' WHERE id = 's1'`)
	require.NoError(t, err)

	require.NoError(t, d.ReplaceSessionMessages(ctx, "s1", []Message{
		{SessionID: "s1", Ordinal: 0, Role: "user", Content: "changed"},
	}))

	var version string
	require.NoError(t, d.getReader().QueryRow(ctx,
		"SELECT friction_rules_version FROM sessions WHERE id = 's1'",
	).Scan(&version))
	assert.Empty(t, version, "a transcript change must force a friction recompute")
}
