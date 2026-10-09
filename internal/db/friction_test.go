package db

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/friction"
)

func (d *DB) replaceSessionFriction(
	ctx context.Context, sessionID string,
	findings []FrictionFinding, rulesVersion, hash string,
) error {
	return d.UpdateSessionSignals(ctx, sessionID, SessionSignalUpdate{
		Friction: &SessionFrictionUpdate{
			Findings: findings, RulesVersion: rulesVersion, Hash: hash,
		},
	})
}

func assertFrictionRulesVersion(t *testing.T, d *DB, sessionID, want string) {
	t.Helper()
	session, err := d.GetSessionFull(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, want, session.FrictionRulesVersion)
}

func TestFrictionSessionColumnsMigrateOnOldArchive(t *testing.T) {
	path := createClosedTestDB(t, tempDBPath(t, "sessions.db"), nil)
	execRawSQLite(t, path, "DROP INDEX idx_sessions_friction_rules_version")
	for _, c := range []string{
		"friction_count", "friction_rules_version", "friction_hash",
	} {
		execRawSQLite(t, path, "ALTER TABLE sessions DROP COLUMN "+c)
	}
	execRawSQLite(t, path, "DROP TABLE friction_findings")

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
	plan := strings.Join(explainQueryPlan(t, d, countStaleFrictionSessionsSQL, friction.RulesVersion, friction.SkippedRulesVersion), "\n")
	assert.NotContains(t, plan, "SCAN sessions", "upgraded archives must also seek stale versions")
	assert.Contains(t, plan, "SEARCH sessions")
	for _, table := range []string{"friction_findings"} {
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

func frictionFixture(sessionID string) []FrictionFinding {
	ord, call := 3, 0
	at := time.Date(2026, 9, 16, 1, 35, 0, 120000000, time.UTC)
	findings := []FrictionFinding{
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
	}
	return findings
}

func TestReplaceSessionFrictionRoundTrip(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "s1", "proj")
	findings := frictionFixture("s1")
	hash := FrictionHash(findings, friction.RulesVersion)

	require.NoError(t, d.replaceSessionFriction(ctx, "s1", findings,
		friction.RulesVersion, hash))

	got, err := d.SessionFrictionFindings(ctx, "s1")
	require.NoError(t, err)
	for i := range findings {
		findings[i].RulesVersion = friction.RulesVersion
	}
	assert.Equal(t, findings, got)
	assert.Equal(t, hash, FrictionHash(got, friction.RulesVersion),
		"a hash recomputed from stored rows must equal the written hash")

	s, err := d.GetSessionFull(ctx, "s1")
	require.NoError(t, err)
	assert.Equal(t, 3, s.FrictionCount)
	assert.Equal(t, friction.RulesVersion, s.FrictionRulesVersion)
	assert.Equal(t, hash, s.FrictionHash)

	// Replace with nothing: rows go away, version stays current.
	emptyHash := FrictionHash(nil, friction.RulesVersion)
	require.NoError(t, d.replaceSessionFriction(ctx, "s1", nil,
		friction.RulesVersion, emptyHash))
	got, err = d.SessionFrictionFindings(ctx, "s1")
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestReplaceSessionFrictionAtRevisionUnchangedPreservesModification(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "s1", "proj")
	findings := frictionFixture("s1")
	u := SessionFrictionUpdate{
		Findings: findings, RulesVersion: friction.RulesVersion,
		Hash: FrictionHash(findings, friction.RulesVersion),
	}
	require.NoError(t, d.replaceSessionFriction(ctx, "s1", findings,
		u.RulesVersion, u.Hash))
	expected, err := d.GetSessionFull(ctx, "s1")
	require.NoError(t, err)
	require.NotNil(t, expected)
	const before = "2020-01-01T00:00:00.000Z"
	_, err = d.getWriter().Exec(ctx,
		`UPDATE sessions SET local_modified_at = ? WHERE id = 's1'`, before)
	require.NoError(t, err)
	_, err = d.getWriter().Exec(ctx,
		`UPDATE friction_findings SET created_at = ? WHERE session_id = 's1'`, before)
	require.NoError(t, err)

	applied, err := d.ReplaceSessionFrictionAtRevision(ctx, *expected, u)
	require.NoError(t, err)
	require.True(t, applied)
	var modifiedAt, createdAt string
	require.NoError(t, d.getReader().QueryRow(ctx,
		`SELECT local_modified_at FROM sessions WHERE id = 's1'`).Scan(&modifiedAt))
	assert.Equal(t, before, modifiedAt, "unchanged friction must not queue another mirror push")
	require.NoError(t, d.getReader().QueryRow(ctx,
		`SELECT created_at FROM friction_findings WHERE session_id = 's1' AND seq = 0`).Scan(&createdAt))
	assert.Equal(t, before, createdAt, "unchanged findings must keep their stored rows")

	u.Findings[0].Text = "use the second config file"
	u.Hash = FrictionHash(u.Findings, u.RulesVersion)
	applied, err = d.ReplaceSessionFrictionAtRevision(ctx, *expected, u)
	require.NoError(t, err)
	require.True(t, applied)
	require.NoError(t, d.getReader().QueryRow(ctx,
		`SELECT local_modified_at FROM sessions WHERE id = 's1'`).Scan(&modifiedAt))
	assert.Greater(t, modifiedAt, before, "changed friction must be selected for a mirror push")
	stored, err := d.SessionFrictionFindings(ctx, "s1")
	require.NoError(t, err)
	require.Len(t, stored, 3)
	assert.Equal(t, "use the second config file", stored[0].Text)
}

func TestFrictionHash(t *testing.T) {
	findings := frictionFixture("s1")
	base := FrictionHash(findings, friction.RulesVersion)
	require.Len(t, base, 64)
	tests := []struct {
		name   string
		mutate func(f []FrictionFinding) ([]FrictionFinding, string)
	}{
		{"text", func(f []FrictionFinding) ([]FrictionFinding, string) {
			f[0].Text += "!"
			return f, friction.RulesVersion
		}},
		{"ordinal nil vs zero", func(f []FrictionFinding) ([]FrictionFinding, string) {
			zero := 0
			f[2].MessageOrdinal = &zero
			return f, friction.RulesVersion
		}},
		{"order", func(f []FrictionFinding) ([]FrictionFinding, string) {
			f[0], f[1] = f[1], f[0]
			return f, friction.RulesVersion
		}},
		{"rules version", func(f []FrictionFinding) ([]FrictionFinding, string) {
			return f, "friction-v2"
		}},
		{"field boundary", func(f []FrictionFinding) ([]FrictionFinding, string) {
			f[1].ToolName, f[1].Label = "Bas", "h"
			return f, friction.RulesVersion
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, rv := tt.mutate(frictionFixture("s1"))
			assert.NotEqual(t, base, FrictionHash(f, rv))
		})
	}
	t.Run("session id and row rules version ignored", func(t *testing.T) {
		f := frictionFixture("other")
		for i := range f {
			f[i].Title = findings[i].Title
			f[i].RulesVersion = "anything"
		}
		assert.Equal(t, base, FrictionHash(f, friction.RulesVersion))
	})
}

func TestFrictionHashEncoding(t *testing.T) {
	findings := []FrictionFinding{{
		Kind: "error", Detector: "error", MessageOrdinal: new(0),
		ToolName: "Bash", Text: "é:bad", Title: "failure", Fingerprint: "fl1:x",
		OccurredAt: new(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)),
	}}
	// Fixed digests of the length-prefixed UTF-8 fields. Encoding changes would
	// otherwise make unchanged findings trigger another mirror push.
	assert.Equal(t, "ac8a122b3470eb85ca9ad372af8e804b76a25f2779b9ea9a8ea95d047e58604f",
		FrictionHash(findings, "rules-a"))
}

func TestReplaceSessionFrictionLargeSet(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "s1", "proj")
	seed := frictionFixture("s1")
	findings := make([]FrictionFinding, 151)
	for i := range findings {
		findings[i] = seed[i%len(seed)]
		findings[i].Seq = i
		findings[i].RulesVersion = "rules-a"
	}
	require.NoError(t, d.replaceSessionFriction(ctx, "s1", findings,
		"rules-a", FrictionHash(findings, "rules-a")))
	got, err := d.SessionFrictionFindings(ctx, "s1")
	require.NoError(t, err)
	assert.Equal(t, findings, got)
}

func TestUpdateSessionSignalsPersistsFriction(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "s1", "proj")
	findings := frictionFixture("s1")
	hash := FrictionHash(findings, friction.RulesVersion)

	// Friction nil leaves stored friction alone.
	require.NoError(t, d.replaceSessionFriction(ctx, "s1", findings,
		friction.RulesVersion, hash))
	require.NoError(t, d.UpdateSessionSignals(ctx, "s1", SessionSignalUpdate{}))
	got, err := d.SessionFrictionFindings(ctx, "s1")
	require.NoError(t, err)
	assert.Len(t, got, 3)

	// Friction set replaces it in the signal transaction.
	require.NoError(t, d.UpdateSessionSignals(ctx, "s1", SessionSignalUpdate{
		Friction: &SessionFrictionUpdate{
			Findings: findings[:1], RulesVersion: friction.RulesVersion,
			Hash: FrictionHash(findings[:1], friction.RulesVersion),
		},
	}))
	got, err = d.SessionFrictionFindings(ctx, "s1")
	require.NoError(t, err)
	assert.Len(t, got, 1)
}

func TestReplaceSessionFrictionUsageOnlySettlesEmpty(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "s1", "proj")
	d.SetArchiveContent(config.ArchiveContentUsage)
	findings := frictionFixture("s1")

	require.NoError(t, d.replaceSessionFriction(ctx, "s1", findings,
		friction.RulesVersion, "ignored"))

	got, err := d.SessionFrictionFindings(ctx, "s1")
	require.NoError(t, err)
	assert.Empty(t, got, "usage-only archives store no transcript-derived text")
	s, err := d.GetSessionFull(ctx, "s1")
	require.NoError(t, err)
	assert.Zero(t, s.FrictionCount)
	assert.Equal(t, friction.RulesVersion, s.FrictionRulesVersion)
	assert.Equal(t, FrictionHash(nil, friction.RulesVersion), s.FrictionHash)
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
	findings := frictionFixture("s1")
	u := SessionFrictionUpdate{
		Findings: findings, RulesVersion: friction.RulesVersion,
		Hash: FrictionHash(findings, friction.RulesVersion),
	}

	stale := *s
	stale.TranscriptRevision = new("stale-rev")
	applied, err := d.ReplaceSessionFrictionAtRevision(ctx, stale, u)
	require.NoError(t, err)
	assert.False(t, applied, "a changed transcript must reject the snapshot")
	got, err := d.SessionFrictionFindings(ctx, "s1")
	require.NoError(t, err)
	assert.Empty(t, got)

	_, err = d.getWriter().ExecContext(ctx, `
		UPDATE sessions
		SET parent_session_id = 'parent', relationship_type = 'subagent'
		WHERE id = 's1'`)
	require.NoError(t, err)
	applied, err = d.ReplaceSessionFrictionAtRevision(ctx, *s, u)
	require.NoError(t, err)
	assert.False(t, applied, "a hierarchy change must reject a snapshot computed before relinking")
	got, err = d.SessionFrictionFindings(ctx, "s1")
	require.NoError(t, err)
	assert.Empty(t, got)

	s, err = d.GetSessionFull(ctx, "s1")
	require.NoError(t, err)
	applied, err = d.ReplaceSessionFrictionAtRevision(ctx, *s, u)
	require.NoError(t, err)
	assert.True(t, applied)
	got, err = d.SessionFrictionFindings(ctx, "s1")
	require.NoError(t, err)
	assert.Len(t, got, 3)
}

func TestReplaceSessionFrictionAtRevisionRejectsDerivedInputChanges(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
	}{
		{
			name:  "agent",
			query: `UPDATE sessions SET agent = 'codex' WHERE id = ?`,
		},
		{
			name:  "context pressure",
			query: `UPDATE sessions SET context_pressure_max = 0.75 WHERE id = ?`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			ctx := t.Context()
			id := "friction-input-race"
			insertSession(t, d, id, "proj")
			require.NoError(t, d.ReplaceSessionMessages(ctx, id, []Message{
				{SessionID: id, Ordinal: 0, Role: "user", Content: "hello"},
			}))
			expected, err := d.GetSessionFull(ctx, id)
			require.NoError(t, err)
			require.NotNil(t, expected)

			baseline := frictionFixture(id)
			require.NoError(t, d.replaceSessionFriction(ctx, id, baseline,
				"friction-v1", FrictionHash(baseline, "friction-v1"),
			))
			_, err = d.getWriter().ExecContext(ctx, tc.query, id)
			require.NoError(t, err)

			stale := []FrictionFinding{{
				SessionID: id, Kind: "correction", Detector: "correction.coding",
				Text: "stale finding", Title: "stale finding",
				Fingerprint: "stale-finding",
			}}
			update := SessionFrictionUpdate{
				Findings: stale, RulesVersion: "friction-v1",
				Hash: FrictionHash(stale, "friction-v1"),
			}
			applied, err := d.ReplaceSessionFrictionAtRevision(ctx, *expected, update)
			require.NoError(t, err)
			require.False(t, applied,
				"changes to friction inputs must reject the old findings snapshot")

			got, err := d.SessionFrictionFindings(ctx, id)
			require.NoError(t, err)
			require.Len(t, got, len(baseline))
			require.Equal(t, baseline[0].Fingerprint, got[0].Fingerprint)
			current, err := d.GetSessionFull(ctx, id)
			require.NoError(t, err)
			require.Empty(t, current.FrictionRulesVersion,
				"a rejected snapshot must remain eligible for a later backfill")
		})
	}
}

func TestUpsertSessionHierarchyChangeInvalidatesFriction(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	session := Session{
		ID: "child", Project: "proj", Machine: "local", Agent: "claude",
	}
	require.NoError(t, d.UpsertSession(ctx, session))
	require.NoError(t, d.replaceSessionFriction(ctx, session.ID, nil,
		friction.RulesVersion,
		FrictionHash(nil, friction.RulesVersion)))

	parent := "parent"
	session.ParentSessionID = &parent
	session.RelationshipType = "subagent"
	require.NoError(t, d.UpsertSession(ctx, session))
	stored, err := d.GetSessionFull(ctx, session.ID)
	require.NoError(t, err)
	assert.Equal(t, parent, *stored.ParentSessionID)
	assert.Equal(t, "subagent", stored.RelationshipType)
	assert.Empty(t, stored.FrictionRulesVersion,
		"a hierarchy change must queue friction recomputation")
}

func TestUpsertSessionFrictionFreshness(t *testing.T) {
	tests := []struct {
		name      string
		update    func(*Session)
		wantStale bool
	}{
		{name: "agent", update: func(s *Session) { s.Agent = "codex" }, wantStale: true},
		{name: "machine", update: func(s *Session) { s.Machine = "remote" }},
		{name: "project", update: func(s *Session) { s.Project = "other-project" }},
		{name: "cwd", update: func(s *Session) { s.Cwd = "/other/workspace" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			ctx := t.Context()
			session := Session{
				ID: "session", Project: "project", Machine: "local",
				Agent: "claude", Cwd: "/workspace",
			}
			require.NoError(t, d.UpsertSession(ctx, session))
			require.NoError(t, d.replaceSessionFriction(ctx, session.ID,
				nil, friction.RulesVersion,
				FrictionHash(nil, friction.RulesVersion)))
			stored, err := d.GetSessionFull(ctx, session.ID)
			require.NoError(t, err)
			require.Equal(t, friction.RulesVersion, stored.FrictionRulesVersion,
				"setup must represent current findings")

			tt.update(&session)
			require.NoError(t, d.UpsertSession(ctx, session))

			stale, err := d.StaleFrictionSessions(ctx, 10)
			require.NoError(t, err)
			if tt.wantStale {
				assert.Equal(t, []string{"session"}, stale,
					"changing the detector must queue recomputation")
			} else {
				assert.Empty(t, stale, "display metadata does not affect friction findings")
				assertFrictionRulesVersion(t, d, session.ID, friction.RulesVersion)
			}
			applied, err := d.ReplaceSessionFrictionAtRevision(ctx, *stored, SessionFrictionUpdate{
				RulesVersion: friction.RulesVersion,
				Hash:         FrictionHash(nil, friction.RulesVersion),
			})
			require.NoError(t, err)
			assert.Equal(t, !tt.wantStale, applied,
				"snapshot publication must guard the inputs used by friction detectors")
		})
	}
}

func TestStaleFrictionSessions(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	for _, id := range []string{"a", "b", "c", "empty"} {
		insertSession(t, d, id, "proj")
	}
	_, err := d.getWriter().Exec(ctx, `UPDATE sessions SET message_count = 0 WHERE id = 'empty'`)
	require.NoError(t, err)
	require.NoError(t, d.replaceSessionFriction(ctx, "b", nil,
		friction.RulesVersion, FrictionHash(nil, friction.RulesVersion)))
	require.NoError(t, d.replaceSessionFriction(ctx, "c", nil,
		"friction-v0", FrictionHash(nil, "friction-v0")))
	insertSession(t, d, "skipped", "proj")
	skipped := SkippedFriction()
	require.NoError(t, d.replaceSessionFriction(ctx, "skipped", nil,
		skipped.RulesVersion, skipped.Hash))

	ids, err := d.StaleFrictionSessions(ctx, 10)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"a", "c", "empty"}, ids,
		"stale means version differs; skipped is current; empty sessions still settle once")
	count, err := d.CountStaleFrictionSessions(ctx)
	require.NoError(t, err)
	assert.Equal(t, 3, count, "progress includes every stale session across all pages")

	ids, err = d.StaleFrictionSessions(ctx, 1)
	require.NoError(t, err)
	assert.Len(t, ids, 1)
}

func TestFrictionBackfillStateRoundTrip(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	state, err := d.FrictionBackfillState(ctx)
	require.NoError(t, err)
	assert.Empty(t, state)
	require.NoError(t, d.MarkFrictionBackfill(ctx, "running", 10, 4, ""))
	require.NoError(t, d.MarkFrictionBackfill(ctx, "completed", 10, 10, ""))
	state, err = d.FrictionBackfillState(ctx)
	require.NoError(t, err)
	assert.Equal(t, "completed", state)
	var total, done int
	require.NoError(t, d.getReader().QueryRow(ctx,
		`SELECT total_items, completed_items FROM background_migrations WHERE name = ?`,
		FrictionBackfillName).Scan(&total, &done))
	assert.Equal(t, 10, total)
	assert.Equal(t, 10, done)
}

func TestCountStaleFrictionSessionsSeeksPastCurrentArchive(t *testing.T) {
	for _, size := range []int{1, 10000} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			d := testDB(t)
			ctx := t.Context()
			_, err := d.getWriter().Exec(ctx, `
				WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x < ?)
				INSERT INTO sessions(id, project, friction_rules_version)
				SELECT printf('session-%06d', x), 'project-a', ? FROM n`, size, friction.RulesVersion)
			require.NoError(t, err)
			count, err := d.CountStaleFrictionSessions(ctx)
			require.NoError(t, err)
			assert.Zero(t, count)
			plan := strings.Join(explainQueryPlan(t, d, countStaleFrictionSessionsSQL, friction.RulesVersion, friction.SkippedRulesVersion), "\n")
			assert.NotContains(t, plan, "SCAN sessions", "completed archives must not scan current sessions")
			assert.Contains(t, plan, "SEARCH sessions")
			plan = strings.Join(explainQueryPlan(t, d, staleFrictionSessionsSQL, friction.RulesVersion, friction.SkippedRulesVersion, 20), "\n")
			assert.NotContains(t, plan, "SCAN sessions", "backfill pages must not walk current sessions")
			assert.Contains(t, plan, "idx_sessions_friction_rules_version",
				"backfill pages seek the version index rather than walking ids")

			_, err = d.getWriter().Exec(ctx, `INSERT INTO sessions(id, project, friction_rules_version)
				VALUES ('empty-version', 'project-a', ''), ('old-version', 'project-a', 'friction-v0'),
				       ('newer-version', 'project-a', 'friction-v99')`)
			require.NoError(t, err)
			count, err = d.CountStaleFrictionSessions(ctx)
			require.NoError(t, err)
			assert.Equal(t, 3, count, "every differing version needs recomputation")
		})
	}
}

func TestOrphanCopyPreservesFriction(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()
	srcPath := filepath.Join(dir, "old.db")
	src, err := Open(ctx, srcPath)
	require.NoError(t, err)
	insertSession(t, src, "s1", "proj")
	insertMessages(t, src, userMsg("s1", 0, "hello"), asstMsg("s1", 1, "reply"))
	findings := frictionFixture("s1")
	hash := FrictionHash(findings, friction.RulesVersion)
	require.NoError(t, src.replaceSessionFriction(ctx, "s1", findings,
		friction.RulesVersion, hash))
	_, err = src.getWriter().Exec(ctx,
		"UPDATE friction_findings SET created_at = '2020-01-01T00:00:00.000Z'")
	require.NoError(t, err)
	require.NoError(t, src.Close())

	dst, err := Open(ctx, filepath.Join(dir, "new.db"))
	require.NoError(t, err)
	defer dst.Close()
	count, err := dst.CopyOrphanedDataFrom(srcPath)
	require.NoError(t, err)
	require.Equal(t, 1, count)

	got, err := dst.SessionFrictionFindings(ctx, "s1")
	require.NoError(t, err)
	require.Len(t, got, 3)
	s, err := dst.GetSessionFull(ctx, "s1")
	require.NoError(t, err)
	assert.Equal(t, 3, s.FrictionCount)
	assert.Equal(t, friction.RulesVersion, s.FrictionRulesVersion)
	assert.Equal(t, hash, s.FrictionHash)
	var createdAt string
	require.NoError(t, dst.getReader().QueryRow(ctx,
		"SELECT created_at FROM friction_findings LIMIT 1").Scan(&createdAt))
	assert.Equal(t, "2020-01-01T00:00:00.000Z", createdAt)
}

func TestOrphanCopyFromArchiveWithoutFrictionTables(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()
	srcPath := filepath.Join(dir, "old.db")
	src, err := Open(ctx, srcPath)
	require.NoError(t, err)
	insertSession(t, src, "s1", "proj")
	insertMessages(t, src, userMsg("s1", 0, "hello"))
	require.NoError(t, src.Close())
	execRawSQLite(t, srcPath, "DROP TABLE friction_findings")
	execRawSQLite(t, srcPath, "DROP INDEX idx_sessions_friction_rules_version")
	for _, c := range []string{"friction_count", "friction_rules_version", "friction_hash"} {
		execRawSQLite(t, srcPath, "ALTER TABLE sessions DROP COLUMN "+c)
	}

	dst, err := Open(ctx, filepath.Join(dir, "new.db"))
	require.NoError(t, err)
	defer dst.Close()
	count, err := dst.CopyOrphanedDataFrom(srcPath)
	require.NoError(t, err)
	require.Equal(t, 1, count)
	s, err := dst.GetSessionFull(ctx, "s1")
	require.NoError(t, err)
	assert.Empty(t, s.FrictionRulesVersion, "an old source leaves the session stale for backfill")
}

func TestTrashedCopyPreservesFriction(t *testing.T) {
	src := testDB(t)
	insertSession(t, src, "s1", "proj")
	findings := frictionFixture("s1")
	hash := FrictionHash(findings, friction.RulesVersion)
	require.NoError(t, src.replaceSessionFriction(t.Context(), "s1", findings,
		friction.RulesVersion, hash))
	require.NoError(t, src.SoftDeleteSession(t.Context(), "s1"))

	dst := testDB(t)
	ids, err := dst.CopyTrashedDataFrom(src.Path())
	require.NoError(t, err)
	assert.Equal(t, []string{"s1"}, ids)
	got, err := dst.SessionFrictionFindings(t.Context(), "s1")
	require.NoError(t, err)
	require.Len(t, got, len(findings))
	s, err := dst.GetSessionFull(t.Context(), "s1")
	require.NoError(t, err)
	assert.Equal(t, len(findings), s.FrictionCount)
	assert.Equal(t, hash, s.FrictionHash)
}
