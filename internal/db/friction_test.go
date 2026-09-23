package db

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/friction"
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

func frictionFixture(sessionID string) ([]FrictionFinding, *FrictionSessionDims) {
	ord, call := 3, 0
	at := time.Date(2026, 9, 16, 1, 35, 0, 120000000, time.UTC)
	return []FrictionFinding{
		{
			SessionID: sessionID, Kind: "correction",
			Detector: "correction.coding", MessageOrdinal: &ord,
			Text: "no, use the other config file", OccurredAt: &at,
			Title:       "[friction/correction] " + sessionID + ": no, use the other config file",
			Fingerprint: "fl1:aa", Seq: 0,
		},
		{
			SessionID: sessionID, Kind: "error", Detector: "error",
			MessageOrdinal: &ord, CallIndex: &call, ToolName: "Bash",
			Text:  `{"error":"boom","success":false}`,
			Title: "[friction/error] Bash: boom", Fingerprint: "fl1:bb",
			Seq: 1,
		},
		{
			SessionID: sessionID, Kind: "pattern",
			Detector:    "pattern.iteration_runaway",
			Label:       "iteration_runaway",
			Text:        "iteration runaway: 150 tool calls with no intervening user message",
			Evidence:    "150 tool calls without a user message 09:00-09:40",
			Title:       "[friction/pattern] " + sessionID + ": iteration runaway",
			Fingerprint: "fl1:cc", Seq: 2,
		},
	}, &FrictionSessionDims{
		SessionID: sessionID, Seat: "seat-02", DimsSource: "seat_pattern",
	}
}

func TestReplaceSessionFrictionRoundTrip(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "s1", "proj")
	findings, dims := frictionFixture("s1")
	hash := FrictionHash(findings, dims, friction.RulesVersion)

	require.NoError(t, d.ReplaceSessionFriction(ctx, "s1", findings, dims,
		friction.RulesVersion, hash))

	got, err := d.SessionFrictionFindings(ctx, "s1")
	require.NoError(t, err)
	for i := range findings {
		findings[i].RulesVersion = friction.RulesVersion
	}
	assert.Equal(t, findings, got)
	gotDims, err := d.SessionFrictionDims(ctx, "s1")
	require.NoError(t, err)
	assert.Equal(t, dims, gotDims)
	assert.Equal(t, hash, FrictionHash(got, gotDims, friction.RulesVersion),
		"a hash recomputed from stored rows must equal the written hash")

	s, err := d.GetSessionFull(ctx, "s1")
	require.NoError(t, err)
	assert.Equal(t, 3, s.FrictionCount)
	assert.Equal(t, friction.RulesVersion, s.FrictionRulesVersion)
	assert.Equal(t, hash, s.FrictionHash)

	// Replace with nothing: rows and dims go away, version stays current.
	emptyHash := FrictionHash(nil, nil, friction.RulesVersion)
	require.NoError(t, d.ReplaceSessionFriction(ctx, "s1", nil, nil,
		friction.RulesVersion, emptyHash))
	got, err = d.SessionFrictionFindings(ctx, "s1")
	require.NoError(t, err)
	assert.Empty(t, got)
	gotDims, err = d.SessionFrictionDims(ctx, "s1")
	require.NoError(t, err)
	assert.Nil(t, gotDims)
}

func TestFrictionHash(t *testing.T) {
	findings, dims := frictionFixture("s1")
	base := FrictionHash(findings, dims, friction.RulesVersion)
	require.Len(t, base, 64)
	tests := []struct {
		name   string
		mutate func(f []FrictionFinding, d *FrictionSessionDims) ([]FrictionFinding, *FrictionSessionDims, string)
	}{
		{"text", func(f []FrictionFinding, d *FrictionSessionDims) ([]FrictionFinding, *FrictionSessionDims, string) {
			f[0].Text += "!"
			return f, d, friction.RulesVersion
		}},
		{"ordinal nil vs zero", func(f []FrictionFinding, d *FrictionSessionDims) ([]FrictionFinding, *FrictionSessionDims, string) {
			zero := 0
			f[2].MessageOrdinal = &zero
			return f, d, friction.RulesVersion
		}},
		{"order", func(f []FrictionFinding, d *FrictionSessionDims) ([]FrictionFinding, *FrictionSessionDims, string) {
			f[0], f[1] = f[1], f[0]
			return f, d, friction.RulesVersion
		}},
		{"dims seat", func(f []FrictionFinding, d *FrictionSessionDims) ([]FrictionFinding, *FrictionSessionDims, string) {
			d.Seat = "seat-03"
			return f, d, friction.RulesVersion
		}},
		{"dims removed", func(f []FrictionFinding, _ *FrictionSessionDims) ([]FrictionFinding, *FrictionSessionDims, string) {
			return f, nil, friction.RulesVersion
		}},
		{"rules version", func(f []FrictionFinding, d *FrictionSessionDims) ([]FrictionFinding, *FrictionSessionDims, string) {
			return f, d, "friction-v2"
		}},
		{"field boundary", func(f []FrictionFinding, d *FrictionSessionDims) ([]FrictionFinding, *FrictionSessionDims, string) {
			f[1].ToolName, f[1].Label = "Bas", "h"
			return f, d, friction.RulesVersion
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, d := frictionFixture("s1")
			f, d2, rv := tt.mutate(f, d)
			assert.NotEqual(t, base, FrictionHash(f, d2, rv))
		})
	}
	t.Run("session id and row rules version ignored", func(t *testing.T) {
		f, d := frictionFixture("other")
		for i := range f {
			f[i].Title = findings[i].Title
			f[i].RulesVersion = "anything"
		}
		d.SessionID = "other"
		assert.Equal(t, base, FrictionHash(f, d, friction.RulesVersion))
	})
}

func TestUpdateSessionSignalsPersistsFriction(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "s1", "proj")
	findings, dims := frictionFixture("s1")
	hash := FrictionHash(findings, dims, friction.RulesVersion)

	// Friction nil leaves stored friction alone.
	require.NoError(t, d.ReplaceSessionFriction(ctx, "s1", findings, dims,
		friction.RulesVersion, hash))
	require.NoError(t, d.UpdateSessionSignals(ctx, "s1", SessionSignalUpdate{}))
	got, err := d.SessionFrictionFindings(ctx, "s1")
	require.NoError(t, err)
	assert.Len(t, got, 3)

	// Friction set replaces it in the signal transaction.
	require.NoError(t, d.UpdateSessionSignals(ctx, "s1", SessionSignalUpdate{
		Friction: &SessionFrictionUpdate{
			Findings: findings[:1], RulesVersion: friction.RulesVersion,
			Hash: FrictionHash(findings[:1], nil, friction.RulesVersion),
		},
	}))
	got, err = d.SessionFrictionFindings(ctx, "s1")
	require.NoError(t, err)
	assert.Len(t, got, 1)
	gotDims, err := d.SessionFrictionDims(ctx, "s1")
	require.NoError(t, err)
	assert.Nil(t, gotDims)
}

func TestReplaceSessionFrictionUsageOnlySettlesEmpty(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "s1", "proj")
	d.SetArchiveContent(config.ArchiveContentUsage)
	findings, dims := frictionFixture("s1")

	require.NoError(t, d.ReplaceSessionFriction(ctx, "s1", findings, dims,
		friction.RulesVersion, "ignored"))

	got, err := d.SessionFrictionFindings(ctx, "s1")
	require.NoError(t, err)
	assert.Empty(t, got, "usage-only archives store no transcript-derived text")
	s, err := d.GetSessionFull(ctx, "s1")
	require.NoError(t, err)
	assert.Zero(t, s.FrictionCount)
	assert.Equal(t, friction.RulesVersion, s.FrictionRulesVersion)
	assert.Equal(t, FrictionHash(nil, nil, friction.RulesVersion), s.FrictionHash)
}

func TestReplaceSessionFrictionAtRevision(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "s1", "proj")
	require.NoError(t, d.ReplaceSessionMessages(ctx, "s1", []Message{
		{SessionID: "s1", Ordinal: 0, Role: "user", Content: "hello"},
	}))
	s, err := d.GetSessionFull(ctx, "s1")
	require.NoError(t, err)
	require.NotNil(t, s.TranscriptRevision)
	findings, _ := frictionFixture("s1")
	u := SessionFrictionUpdate{
		Findings: findings, RulesVersion: friction.RulesVersion,
		Hash: FrictionHash(findings, nil, friction.RulesVersion),
	}

	applied, err := d.ReplaceSessionFrictionAtRevision(ctx, "s1", "stale-rev", u)
	require.NoError(t, err)
	assert.False(t, applied, "a changed transcript must reject the snapshot")
	got, err := d.SessionFrictionFindings(ctx, "s1")
	require.NoError(t, err)
	assert.Empty(t, got)

	applied, err = d.ReplaceSessionFrictionAtRevision(ctx, "s1", *s.TranscriptRevision, u)
	require.NoError(t, err)
	assert.True(t, applied)
	got, err = d.SessionFrictionFindings(ctx, "s1")
	require.NoError(t, err)
	assert.Len(t, got, 3)
}

func TestStaleFrictionSessions(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	for _, id := range []string{"a", "b", "c", "empty"} {
		insertSession(t, d, id, "proj")
	}
	_, err := d.getWriter().Exec(ctx, `UPDATE sessions SET message_count = 0 WHERE id = 'empty'`)
	require.NoError(t, err)
	require.NoError(t, d.ReplaceSessionFriction(ctx, "b", nil, nil,
		friction.RulesVersion, FrictionHash(nil, nil, friction.RulesVersion)))
	require.NoError(t, d.ReplaceSessionFriction(ctx, "c", nil, nil,
		"friction-v0", FrictionHash(nil, nil, "friction-v0")))

	ids, err := d.StaleFrictionSessions(ctx, friction.RulesVersion, 10)
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "c", "empty"}, ids,
		"stale means version differs; empty sessions still settle once")

	ids, err = d.StaleFrictionSessions(ctx, friction.RulesVersion, 1)
	require.NoError(t, err)
	assert.Equal(t, []string{"a"}, ids)
}

func TestFrictionReadsPropagateTableProbeError(t *testing.T) {
	d := testDB(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := d.SessionFrictionFindings(ctx, "s1")
	require.ErrorIs(t, err, context.Canceled)
	_, err = d.SessionFrictionDims(ctx, "s1")
	require.ErrorIs(t, err, context.Canceled)
}
