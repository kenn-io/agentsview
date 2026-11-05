//go:build pgtest

package postgres

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

type frictionFixtureFinding struct {
	sid, kind, detector, fp string
	seq                     int
	ord                     *int
}

var frictionFixtureFindings = []frictionFixtureFinding{
	{"claude:s1", "correction", "correction.coding", "fl1:aaa", 0, new(3)},
	{"claude:s1", "error", "error", "fl1:bbb", 1, new(5)},
	{"claude:s2", "pattern", "pattern.retry_loop", "fl1:ccc", 0, nil},
	{"claude:gone", "error", "error", "fl1:bbb", 0, new(1)},
}

var frictionFixtureAt = time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)

func frictionFixtureDigests() []struct {
	d        db.FrictionDigest
	subject  string
	patterns []db.FrictionPatternUpdate
} {
	built := time.Date(2026, 9, 15, 1, 0, 0, 0, time.UTC)
	mk := func(date string) db.FrictionDigest {
		return db.FrictionDigest{
			Date: date, Timezone: "UTC", RulesVersion: "friction-v1",
			BuiltAt: built, Revision: 1, SessionsScanned: 1,
			SnapshotJSON: []byte("{}"), SummaryJSON: []byte("{}\n"),
			Markdown:       []byte("# Friction Log — " + date + "\n"),
			MarkdownSHA256: "sha-" + date, RunID: "run-" + date,
		}
	}
	return []struct {
		d        db.FrictionDigest
		subject  string
		patterns []db.FrictionPatternUpdate
	}{
		{mk("2026-09-13"), "claude:s2", []db.FrictionPatternUpdate{
			{Fingerprint: "fl1:ccc", Kind: "pattern", Title: "ccc", Date: "2026-09-13", SubjectID: "claude:s2", Occurrences: 1},
		}},
		{mk("2026-09-14"), "claude:s1", []db.FrictionPatternUpdate{
			{Fingerprint: "fl1:aaa", Kind: "correction", Title: "aaa", Date: "2026-09-14", SubjectID: "claude:s1", Ordinal: new(3), Occurrences: 1},
			{Fingerprint: "fl1:bbb", Kind: "error", Title: "bbb", Date: "2026-09-14", SubjectID: "claude:s1", Ordinal: new(5), Occurrences: 2},
		}},
	}
}

func seedFrictionSQLite(t *testing.T) *db.DB {
	t.Helper()
	d := dbtest.OpenTestDB(t)
	ctx := t.Context()
	bySession := map[string][]db.FrictionFinding{}
	for _, f := range frictionFixtureFindings {
		dbtest.SeedSession(t, d, f.sid, "proj")
		bySession[f.sid] = append(bySession[f.sid], db.FrictionFinding{
			SessionID: f.sid, Kind: f.kind, Detector: f.detector,
			MessageOrdinal: f.ord, Title: "[friction/" + f.kind + "] " + f.sid,
			Fingerprint: f.fp, OccurredAt: &frictionFixtureAt, Seq: f.seq,
			RulesVersion: "friction-v1",
		})
	}
	for sid, rows := range bySession {
		require.NoError(t, d.UpdateSessionSignals(ctx, sid, db.SessionSignalUpdate{
			Friction: &db.SessionFrictionUpdate{Findings: rows, RulesVersion: "friction-v1", Hash: "hash-" + sid},
		}))
	}
	require.NoError(t, d.SoftDeleteSession(ctx, "claude:gone"))
	for _, dg := range frictionFixtureDigests() {
		require.NoError(t, d.SaveFrictionDigest(ctx, dg.d,
			[]db.FrictionDigestSubject{{SubjectID: dg.subject, Date: dg.d.Date, SubjectKind: "session"}},
			dg.patterns))
	}
	return d
}

func seedFrictionPG(t *testing.T, store *Store) {
	t.Helper()
	pg := store.DB()
	ctx := t.Context()
	for _, table := range []string{"friction_findings", "friction_digest_sessions", "friction_patterns", "friction_digests", "friction_issue_links"} {
		_, err := pg.ExecContext(ctx, "DELETE FROM "+table)
		require.NoError(t, err, table)
	}
	seen := map[string]bool{}
	for _, f := range frictionFixtureFindings {
		if !seen[f.sid] {
			seen[f.sid] = true
			_, err := pg.ExecContext(ctx, `DELETE FROM sessions WHERE id = $1`, f.sid)
			require.NoError(t, err)
			var deleted any
			if f.sid == "claude:gone" {
				deleted = frictionFixtureAt
			}
			_, err = pg.ExecContext(ctx, `
				INSERT INTO sessions (id, machine, project, agent, first_message,
					message_count, user_message_count, deleted_at)
				VALUES ($1, 'test-machine', 'proj', 'claude', 'test', 1, 0, $2)`,
				f.sid, deleted)
			require.NoError(t, err, "insert session %s", f.sid)
		}
		_, err := pg.ExecContext(ctx, `
			INSERT INTO friction_findings (session_id, kind, detector,
				message_ordinal, call_index, tool_name, label, text, evidence,
				title, fingerprint, occurred_at, seq, rules_version)
			VALUES ($1,$2,$3,$4,NULL,'','','','',$5,$6,$7,$8,'friction-v1')`,
			f.sid, f.kind, f.detector, f.ord, "[friction/"+f.kind+"] "+f.sid,
			f.fp, frictionFixtureAt, f.seq)
		require.NoError(t, err, "insert finding")
	}
	for _, dg := range frictionFixtureDigests() {
		require.NoError(t, store.SaveFrictionDigest(ctx, dg.d,
			[]db.FrictionDigestSubject{{SubjectID: dg.subject, Date: dg.d.Date, SubjectKind: "session"}},
			dg.patterns))
	}
}

func TestFrictionListParity(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)
	store, err := NewStore(t.Context(), pgURL, testSchema, true)
	require.NoError(t, err)
	defer store.Close()
	seedFrictionPG(t, store)
	local := seedFrictionSQLite(t)
	ctx := t.Context()

	findingFilters := map[string]db.FrictionFindingFilter{
		"all":         {},
		"kind":        {Kind: "error"},
		"session":     {SessionID: "claude:s2"},
		"fingerprint": {Fingerprint: "fl1:bbb"},
		"date":        {Date: "2026-09-14"},
		"page_one":    {Limit: 2},
		"page_two":    {Limit: 2, Cursor: "2"},
	}
	for name, f := range findingFilters {
		t.Run("findings_"+name, func(t *testing.T) {
			want, wantNext, err := local.ListFrictionFindings(ctx, f)
			require.NoError(t, err)
			got, gotNext, err := store.ListFrictionFindings(ctx, f)
			require.NoError(t, err)
			require.Len(t, got, len(want))
			for i := range want {
				require.NotNil(t, got[i].OccurredAt)
				assert.True(t, want[i].OccurredAt.Equal(*got[i].OccurredAt))
				want[i].OccurredAt, got[i].OccurredAt = nil, nil
			}
			assert.Equal(t, want, got)
			assert.Equal(t, wantNext, gotNext)
		})
	}

	for name, rng := range map[string][2]string{"all": {"", ""}, "from": {"2026-09-14", ""}, "to": {"", "2026-09-13"}} {
		t.Run("digests_"+name, func(t *testing.T) {
			want, err := local.ListFrictionDigests(ctx, rng[0], rng[1])
			require.NoError(t, err)
			got, err := store.ListFrictionDigests(ctx, rng[0], rng[1])
			require.NoError(t, err)
			require.Len(t, got, len(want))
			for i := range want {
				assert.True(t, want[i].BuiltAt.Equal(got[i].BuiltAt))
				want[i].BuiltAt, got[i].BuiltAt = time.Time{}, time.Time{}
			}
			assert.Equal(t, want, got)
		})
	}

	// Both stores join the same link into pattern reads.
	link := db.FrictionIssueLink{
		Fingerprint: "fl1:aaa", KataInstanceUID: "01J00000000000000000000009",
		KataProjectUID: "01J00000000000000000000001", IssueUID: "01J0ABCDEF0000000000000001",
		QualifiedID: "agentsview#f001", WebURL: "https://kata.example.test/issues/f001",
		Source: db.FrictionIssueSourceCreated, LinkedAt: time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC),
	}
	require.NoError(t, local.SaveFrictionIssueLink(ctx, link))
	store.DetectFrictionAvailability(ctx)
	assert.True(t, store.FrictionIssueLinksWritable())
	require.NoError(t, store.SaveFrictionIssueLink(ctx, link))

	patternFilters := map[string]db.FrictionPatternFilter{
		"all":             {},
		"kind":            {Kind: "pattern"},
		"since":           {Since: "2026-09-14"},
		"page":            {Limit: 1},
		"fingerprints":    {Fingerprints: []string{"fl1:ccc", "fl1:aaa", "fl1:missing"}},
		"no_fingerprints": {Fingerprints: []string{}},
	}
	for name, f := range patternFilters {
		t.Run("patterns_"+name, func(t *testing.T) {
			want, wantNext, err := local.ListFrictionPatterns(ctx, f)
			require.NoError(t, err)
			got, gotNext, err := store.ListFrictionPatterns(ctx, f)
			require.NoError(t, err)
			assert.Equal(t, want, got)
			assert.Equal(t, wantNext, gotNext)
		})
	}

	t.Run("bad_cursor", func(t *testing.T) {
		_, _, err := store.ListFrictionFindings(ctx, db.FrictionFindingFilter{Cursor: "nope"})
		require.ErrorIs(t, err, db.ErrInvalidCursor)
	})

	t.Run("issue_link_replace_and_delete", func(t *testing.T) {
		manual := link
		manual.IssueUID, manual.QualifiedID, manual.Source = "01J0ABCDEF0000000000000002", "agentsview#f002", db.FrictionIssueSourceManual
		for name, s := range map[string]interface {
			db.FrictionReader
			db.FrictionIssueLinkWriter
		}{"sqlite": local, "postgres": store} {
			require.NoError(t, s.SaveFrictionIssueLink(ctx, manual), name)
			rows, _, err := s.ListFrictionPatterns(ctx, db.FrictionPatternFilter{Fingerprints: []string{"fl1:aaa"}})
			require.NoError(t, err, name)
			require.Len(t, rows, 1, name)
			assert.Equal(t, &manual, rows[0].Issue, name)

			found := manual
			found.IssueUID, found.QualifiedID, found.Source = "01J0ABCDEF0000000000000003", "agentsview#f003", db.FrictionIssueSourceFound
			got, err := s.AddFrictionIssueLink(ctx, found)
			require.NoError(t, err, name)
			assert.Equal(t, manual, got, "%s: adding keeps the link a person chose", name)

			removed, err := s.DeleteFrictionIssueLink(ctx, "fl1:aaa")
			require.NoError(t, err, name)
			assert.True(t, removed, name)
			removed, err = s.DeleteFrictionIssueLink(ctx, "fl1:aaa")
			require.NoError(t, err, name)
			assert.False(t, removed, name)
			rows, _, err = s.ListFrictionPatterns(ctx, db.FrictionPatternFilter{Fingerprints: []string{"fl1:aaa"}})
			require.NoError(t, err, name)
			assert.Nil(t, rows[0].Issue, name)
			got, err = s.AddFrictionIssueLink(ctx, found)
			require.NoError(t, err, name)
			assert.Equal(t, found, got, "%s: adding to an unlinked pattern saves it", name)
			_, err = s.DeleteFrictionIssueLink(ctx, "fl1:aaa")
			require.NoError(t, err, name)
		}
	})
}

// A read-only serve role cannot add friction_issue_links to a schema that
// predates it; pattern reads must still work and show no links.
func TestFrictionPatternsReadWithoutTheLinksTable(t *testing.T) {
	const schema = "agentsview_friction_nolinks_test"
	pgURL := testPGURL(t)
	cleanNamedPGSchema(t, pgURL, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })
	ctx := t.Context()
	pg, err := Open(ctx, pgURL, schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pg.Close()) })
	_, err = pg.ExecContext(ctx, "CREATE SCHEMA "+schema)
	require.NoError(t, err)
	statements := append(strings.Split(coreDDL, ";\n")[:2], strings.Split(frictionReviewDDL, ";\n")...)
	for _, statement := range statements {
		if strings.TrimSpace(statement) != "" {
			_, err = pg.ExecContext(ctx, statement)
			require.NoError(t, err)
		}
	}
	_, err = pg.ExecContext(ctx, "DROP TABLE friction_issue_links")
	require.NoError(t, err)
	_, err = pg.ExecContext(ctx, `INSERT INTO friction_patterns (fingerprint, kind, title,
		first_seen_date, last_seen_date, occurrence_count, session_count, last_subject_id)
		VALUES ('fl1:aaa', 'error', 't', '2026-09-14', '2026-09-14', 2, 1, 's')`)
	require.NoError(t, err)
	// A direct store, as friction patterns --pg opens one, runs no startup
	// probe.
	store := &Store{pg: pg, ownedDB: pg}

	rows, _, err := store.ListFrictionPatterns(ctx, db.FrictionPatternFilter{})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "fl1:aaa", rows[0].Fingerprint)
	assert.Nil(t, rows[0].Issue)
	store.DetectFrictionAvailability(ctx)
	assert.False(t, store.FrictionIssueLinksWritable(), "filing refuses before contacting Kata")

	// Once a migration adds the table, the same store reads links.
	for _, statement := range strings.Split(frictionReviewDDL, ";\n") {
		if strings.Contains(statement, "friction_issue_links") {
			_, err = pg.ExecContext(ctx, statement)
			require.NoError(t, err)
		}
	}
	link := db.FrictionIssueLink{
		Fingerprint: "fl1:aaa", KataInstanceUID: "i", KataProjectUID: "p", IssueUID: "u",
		QualifiedID: "agentsview#f001", Source: db.FrictionIssueSourceManual,
		LinkedAt: time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC),
	}
	store.DetectFrictionAvailability(ctx)
	assert.True(t, store.FrictionIssueLinksWritable())
	require.NoError(t, store.SaveFrictionIssueLink(ctx, link))
	rows, _, err = store.ListFrictionPatterns(ctx, db.FrictionPatternFilter{})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, &link, rows[0].Issue)
}

// Filing needs every link write: the replacing save's ON CONFLICT DO UPDATE
// needs UPDATE, and unlinking needs DELETE. A role missing any of them must
// fail the probe, or filing creates a Kata issue it cannot record.
func TestFrictionIssueLinkProbeNeedsEveryWritePrivilege(t *testing.T) {
	const schema = "agentsview_friction_privs_test"
	const role = "agentsview_friction_link_writer"
	const password = "agentsview_friction_link_writer_pw"
	pgURL := testPGURL(t)
	cleanNamedPGSchema(t, pgURL, schema)
	ctx := t.Context()
	pg, err := Open(ctx, pgURL, schema, true)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pg.ExecContext(context.WithoutCancel(ctx), "DROP OWNED BY "+role)
		_, _ = pg.ExecContext(context.WithoutCancel(ctx), "DROP ROLE IF EXISTS "+role)
		cleanNamedPGSchema(t, pgURL, schema)
		require.NoError(t, pg.Close())
	})
	_, err = pg.ExecContext(ctx, "CREATE SCHEMA "+schema)
	require.NoError(t, err)
	for _, statement := range append(strings.Split(coreDDL, ";\n")[:2], strings.Split(frictionReviewDDL, ";\n")...) {
		if strings.TrimSpace(statement) != "" {
			_, err = pg.ExecContext(ctx, statement)
			require.NoError(t, err)
		}
	}
	_, _ = pg.ExecContext(ctx, "DROP OWNED BY "+role)
	_, _ = pg.ExecContext(ctx, "DROP ROLE IF EXISTS "+role)
	_, err = pg.ExecContext(ctx, "CREATE ROLE "+role+" LOGIN PASSWORD '"+password+"'")
	require.NoError(t, err)
	_, err = pg.ExecContext(ctx, "GRANT USAGE ON SCHEMA "+schema+" TO "+role)
	require.NoError(t, err)
	roleURL, err := url.Parse(pgURL)
	require.NoError(t, err)
	roleURL.User = url.UserPassword(role, password)

	for _, tt := range []struct {
		privileges string
		writable   bool
	}{
		{privileges: "SELECT, INSERT"},
		{privileges: "SELECT, INSERT, UPDATE"},
		{privileges: "SELECT, INSERT, UPDATE, DELETE", writable: true},
	} {
		t.Run(tt.privileges, func(t *testing.T) {
			_, err := pg.ExecContext(ctx, "REVOKE ALL ON "+schema+".friction_issue_links FROM "+role)
			require.NoError(t, err)
			_, err = pg.ExecContext(ctx, "GRANT "+tt.privileges+" ON "+schema+".friction_issue_links TO "+role)
			require.NoError(t, err)
			rolePG, err := Open(ctx, roleURL.String(), schema, true)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, rolePG.Close()) })
			store := &Store{pg: rolePG, ownedDB: rolePG}
			store.DetectFrictionAvailability(ctx)
			assert.Equal(t, tt.writable, store.FrictionIssueLinksWritable())
		})
	}
}
