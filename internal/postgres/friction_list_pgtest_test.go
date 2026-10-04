//go:build pgtest

package postgres

import (
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
		require.NoError(t, d.ReplaceSessionFriction(ctx, sid, rows, nil, "friction-v1", "hash-"+sid))
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
	for _, table := range []string{"friction_findings", "friction_digest_sessions", "friction_patterns", "friction_digests"} {
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
	store, err := NewStore(pgURL, testSchema, true)
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

	patternFilters := map[string]db.FrictionPatternFilter{
		"all":      {},
		"kind":     {Kind: "pattern"},
		"since":    {Since: "2026-09-14"},
		"linked":   {LinkState: db.FrictionLinkStateLinked},
		"unlinked": {LinkState: db.FrictionLinkStateUnlinked},
		"page":     {Limit: 1},
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
}
