//go:build pgtest

package postgres

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/storage"
)

func newUsageJournalFixture(t *testing.T) (*db.DB, *Sync, *Store) {
	t.Helper()
	schema := fmt.Sprintf("usage_journal_%x", rand.Uint64())
	local := testDB(t)
	syncer, err := New(t.Context(), testPGURL(t), schema, local, "test-machine", true, storage.PusherOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, syncer.Close()); cleanNamedPGSchema(t, testPGURL(t), schema) })
	require.NoError(t, syncer.EnsureSchema(t.Context()))
	store, err := NewStore(t.Context(), testPGURL(t), schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return local, syncer, store
}

func TestUsageChangeJournalPushPaths(t *testing.T) {
	local, syncer, store := newUsageJournalFixture(t)
	ctx := t.Context()
	push := func(t *testing.T) {
		t.Helper()
		result, err := syncer.Push(ctx, false, nil)
		require.NoError(t, err)
		require.Empty(t, result.Errors)
	}
	// Publish catalog and identity metadata before measuring individual writes.
	push(t)
	session := db.Session{ID: "journal-session", Project: "project-a", Machine: "test-machine", Agent: "codex", CreatedAt: "2026-10-01T09:00:00Z"}
	for _, tc := range []struct {
		name   string
		change func(t *testing.T)
		ids    []string
	}{
		{"new session", func(t *testing.T) { require.NoError(t, local.UpsertSession(ctx, session)) }, []string{session.ID}},
		{"metadata", func(t *testing.T) { require.NoError(t, local.RenameSession(ctx, session.ID, new("Updated title"))) }, []string{session.ID}},
		{"message append", func(t *testing.T) {
			require.NoError(t, local.InsertMessages(ctx, []db.Message{{SessionID: session.ID, Ordinal: 0, Role: "user", Content: "Explain this query"}}))
		}, []string{session.ID}},
		{"usage only", func(t *testing.T) {
			require.NoError(t, local.ReplaceSessionUsageEvents(ctx, session.ID, []db.UsageEvent{{SessionID: session.ID, Source: "provider", Model: "model-a", InputTokens: 1200, OutputTokens: 300, DedupKey: "event-a"}}))
		}, []string{session.ID}},
		{"plan usage only", func(t *testing.T) {
			require.NoError(t, local.ReplaceSessionPlanUsage(ctx, session.ID, fullPlanUsage()))
		}, []string{session.ID}},
		{"pricing", func(t *testing.T) {
			require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{ModelPattern: "model-a", InputPerMTok: money.Money{Microdollars: 1000000}}}))
		}, nil},
		{"cursor usage", func(t *testing.T) {
			require.NoError(t, local.InsertCursorUsageEvents(ctx, []db.CursorUsageEvent{{OccurredAt: "2026-10-01T09:00:00Z", Model: "model-a", InputTokens: 100, DedupKey: "cursor-a"}}))
		}, nil},
		{"project identity", func(t *testing.T) {
			require.NoError(t, local.UpsertProjectIdentityObservation(ctx, export.ProjectIdentityObservation{Project: "project-c", Machine: "test-machine", RootPath: "/workspace/project-c", GitRemote: "https://example.com/project-c.git"}))
		}, nil},
		{"local delete", func(t *testing.T) { require.NoError(t, local.DeleteSession(ctx, session.ID)) }, []string{session.ID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, err := store.UsageChangeRevision(ctx)
			require.NoError(t, err)
			tc.change(t)
			if tc.name == "local delete" {
				result, err := syncer.Push(ctx, true, nil)
				require.NoError(t, err)
				require.Empty(t, result.Errors)
			} else {
				push(t)
			}
			after, err := store.UsageChangeRevision(ctx)
			require.NoError(t, err)
			require.Greater(t, after, before)
			through, ids, err := store.LoadUsageChanges(ctx, before)
			require.NoError(t, err)
			assert.Equal(t, after, through)
			assert.Equal(t, tc.ids, ids)
			push(t)
			unchanged, err := store.UsageChangeRevision(ctx)
			require.NoError(t, err)
			assert.Equal(t, after, unchanged, "unchanged push")
		})
	}
	// A fresh session exercises pg serve's curation writes independently of push.
	session.ID = "curation-session"
	require.NoError(t, local.UpsertSession(ctx, session))
	push(t)
	for _, change := range []func() error{
		func() error { return store.RenameSession(ctx, session.ID, new("Renamed")) },
		func() error { return store.SoftDeleteSession(ctx, session.ID) },
		func() error { _, err := store.RestoreSession(ctx, session.ID); return err },
		func() error { return store.SoftDeleteSession(ctx, session.ID) },
		func() error { _, err := store.DeleteSessionIfTrashed(ctx, session.ID); return err },
	} {
		before, err := store.UsageChangeRevision(ctx)
		require.NoError(t, err)
		require.NoError(t, change())
		after, err := store.UsageChangeRevision(ctx)
		require.NoError(t, err)
		require.Greater(t, after, before)
		through, ids, err := store.LoadUsageChanges(ctx, before)
		require.NoError(t, err)
		assert.Equal(t, after, through)
		assert.Equal(t, []string{session.ID}, ids)
	}
}

func TestUsageChangeJournalReaderStraddlesWrite(t *testing.T) {
	_, syncer, store := newUsageJournalFixture(t)
	ctx := t.Context()
	before, err := store.UsageChangeRevision(ctx)
	require.NoError(t, err)
	first, err := syncer.pg.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer first.Rollback()
	_, err = first.ExecContext(ctx, `INSERT INTO sessions(id,machine,project,agent) VALUES('first','machine','project','codex')`)
	require.NoError(t, err)
	second, err := syncer.pg.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer second.Rollback()
	_, err = second.ExecContext(ctx, `INSERT INTO sessions(id,machine,project,agent) VALUES('second','machine','project','codex')`)
	require.NoError(t, err)
	during, err := store.UsageChangeRevision(ctx)
	require.NoError(t, err)
	assert.Equal(t, before, during)
	require.NoError(t, second.Commit())
	middle, err := store.UsageChangeRevision(ctx)
	require.NoError(t, err)
	assert.Equal(t, before+1, middle, "one revision per transaction")
	through, ids, err := store.LoadUsageChanges(ctx, before)
	require.NoError(t, err)
	assert.Equal(t, middle, through)
	assert.Equal(t, []string{"second"}, ids)
	require.NoError(t, first.Commit())
	after, err := store.UsageChangeRevision(ctx)
	require.NoError(t, err)
	assert.Equal(t, middle+1, after)
	through, ids, err = store.LoadUsageChanges(ctx, during)
	assert.Equal(t, after, through)
	require.NoError(t, err)
	assert.Equal(t, []string{"second", "first"}, ids)
	through, ids, err = store.LoadUsageChanges(ctx, after)
	assert.Equal(t, after, through)
	require.NoError(t, err)
	assert.Empty(t, ids)
}

func TestUsageChangeJournalInstallIdempotent(t *testing.T) {
	_, syncer, store := newUsageJournalFixture(t)
	ctx := t.Context()
	before, err := store.UsageChangeRevision(ctx)
	require.NoError(t, err)
	require.Greater(t, before, int64(1000000000000000), "installation seeds a clock-based generation")
	for range 2 {
		require.NoError(t, EnsureSchema(ctx, syncer.pg, syncer.schema))
	}
	after, err := store.UsageChangeRevision(ctx)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	current, err := usageChangeJournalCurrentPG(ctx, syncer.pg)
	require.NoError(t, err)
	assert.True(t, current)
	// The ordinary push fast path upgrades an existing schema too.
	_, err = syncer.pg.ExecContext(ctx, `DROP FUNCTION mark_usage_change() CASCADE; DROP TABLE usage_session_changes; DELETE FROM sync_metadata WHERE key IN ('usage_change_revision','usage_change_journal_version')`)
	require.NoError(t, err)
	absent, err := store.UsageChangeRevision(ctx)
	require.NoError(t, err)
	assert.Zero(t, absent)
	syncer.schemaDone = false
	require.NoError(t, syncer.EnsureSchema(ctx))
	after, err = store.UsageChangeRevision(ctx)
	require.NoError(t, err)
	assert.Greater(t, after, before)
}

func TestUsageChangeJournalRestrictedRole(t *testing.T) {
	local, syncer, store := newUsageJournalFixture(t)
	ctx := t.Context()
	role := fmt.Sprintf("usage_publisher_%x", rand.Uint64())
	_, err := syncer.pg.ExecContext(ctx, `CREATE ROLE `+role+` LOGIN PASSWORD 'test-password'`)
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanup := context.WithoutCancel(ctx)
		_, err := syncer.pg.ExecContext(cleanup, `DROP OWNED BY `+role+`; DROP ROLE `+role)
		require.NoError(t, err)
	})
	_, err = syncer.pg.ExecContext(ctx, `GRANT USAGE ON SCHEMA `+syncer.schema+` TO `+role+`;
 GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA `+syncer.schema+` TO `+role+`;
 GRANT USAGE ON ALL SEQUENCES IN SCHEMA `+syncer.schema+` TO `+role+`;
 REVOKE ALL ON usage_session_changes FROM `+role)
	require.NoError(t, err)
	// The role already reads sessions when the owner installs the journal.
	_, err = syncer.pg.ExecContext(ctx, `DROP FUNCTION mark_usage_change() CASCADE; DROP TABLE usage_session_changes;
 DELETE FROM sync_metadata WHERE key IN ('usage_change_revision','usage_change_journal_version')`)
	require.NoError(t, err)
	require.NoError(t, ensureUsageChangeJournalPG(ctx, syncer.pg))
	roleURL, err := url.Parse(testPGURL(t))
	require.NoError(t, err)
	roleURL.User = url.UserPassword(role, "test-password")
	publisher, err := New(ctx, roleURL.String(), syncer.schema, local, "test-machine", true, storage.PusherOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, publisher.Close()) })
	restricted, err := NewStore(ctx, roleURL.String(), syncer.schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restricted.Close()) })
	require.NoError(t, local.UpsertSession(ctx, db.Session{ID: "restricted", Machine: "test-machine", Project: "project-a", Agent: "codex"}))
	before, err := store.UsageChangeRevision(ctx)
	require.NoError(t, err)
	result, err := publisher.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Empty(t, result.Errors)
	require.NoError(t, restricted.RenameSession(ctx, "restricted", new("Renamed")))
	after, err := store.UsageChangeRevision(ctx)
	require.NoError(t, err)
	assert.Greater(t, after, before)
	through, ids, err := store.LoadUsageChanges(ctx, before)
	require.NoError(t, err)
	assert.Equal(t, after, through)
	assert.Equal(t, []string{"restricted"}, ids)
	restrictedThrough, restrictedIDs, err := restricted.LoadUsageChanges(ctx, before)
	assert.Equal(t, after, restrictedThrough)
	require.NoError(t, err)
	assert.Equal(t, []string{"restricted"}, restrictedIDs)
	// A publisher without CREATE can continue on a schema lacking the journal.
	_, err = syncer.pg.ExecContext(ctx, `DROP FUNCTION mark_usage_change() CASCADE; DROP TABLE usage_session_changes; DELETE FROM sync_metadata WHERE key IN ('usage_change_revision','usage_change_journal_version')`)
	require.NoError(t, err)
	publisher.schemaDone = false
	require.NoError(t, publisher.EnsureSchema(ctx))
	missing, err := store.UsageChangeRevision(ctx)
	require.NoError(t, err)
	assert.Zero(t, missing)
}

func TestUsageChangeJournalMovedRowsAndRollback(t *testing.T) {
	_, syncer, store := newUsageJournalFixture(t)
	ctx := t.Context()
	_, err := syncer.pg.ExecContext(ctx, `INSERT INTO sessions(id,machine,project,agent) VALUES('a','machine','project','codex'),('b','machine','project','codex')`)
	require.NoError(t, err)
	beforeMove, err := store.UsageChangeRevision(ctx)
	require.NoError(t, err)
	_, err = syncer.pg.ExecContext(ctx, `UPDATE sessions SET id='c' WHERE id='a'`)
	require.NoError(t, err)
	throughMove, movedIDs, err := store.LoadUsageChanges(ctx, beforeMove)
	require.NoError(t, err)
	assert.Equal(t, beforeMove+1, throughMove)
	assert.Equal(t, []string{"a", "c"}, movedIDs)
	before, err := store.UsageChangeRevision(ctx)
	require.NoError(t, err)
	_, err = syncer.pg.ExecContext(ctx, `UPDATE sessions SET project=project`)
	require.NoError(t, err)
	unchanged, err := store.UsageChangeRevision(ctx)
	require.NoError(t, err)
	assert.Equal(t, before, unchanged)
	tx, err := syncer.pg.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `UPDATE sessions SET project='rolled-back'; SET CONSTRAINTS ALL IMMEDIATE`)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())
	unchanged, err = store.UsageChangeRevision(ctx)
	require.NoError(t, err)
	assert.Equal(t, before, unchanged)
	_, err = syncer.pg.ExecContext(ctx, `UPDATE sessions SET project='committed'`)
	require.NoError(t, err)
	after, err := store.UsageChangeRevision(ctx)
	require.NoError(t, err)
	assert.Equal(t, before+1, after, "one increment for multiple rows, even after rollback on a pooled connection")
	through, ids, err := store.LoadUsageChanges(ctx, before)
	require.NoError(t, err)
	assert.Equal(t, after, through)
	assert.Equal(t, []string{"b", "c"}, ids)
}

func TestUsageChangeJournalInstallAllowsReaders(t *testing.T) {
	_, syncer, _ := newUsageJournalFixture(t)
	ctx := t.Context()
	_, err := syncer.pg.ExecContext(ctx, `DROP FUNCTION mark_usage_change() CASCADE; DROP TABLE usage_session_changes;
 DELETE FROM sync_metadata WHERE key IN ('usage_change_revision','usage_change_journal_version');
 CREATE FUNCTION mark_usage_change() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NULL; END;';
 CREATE CONSTRAINT TRIGGER trg_uc_sessions_insert AFTER INSERT ON sessions DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION mark_usage_change('id')`)
	require.NoError(t, err)
	tx, err := syncer.pg.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	require.NoError(t, installUsageChangeJournalPG(ctx, tx))
	reader, err := syncer.pg.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer reader.Rollback()
	_, err = reader.ExecContext(ctx, `SET LOCAL lock_timeout = '1s'; SELECT count(*) FROM sessions`)
	require.NoError(t, err, "journal installation must allow session reads before it commits")
}

func TestUsageChangeJournalNewerVersion(t *testing.T) {
	_, syncer, store := newUsageJournalFixture(t)
	ctx := t.Context()
	_, err := syncer.pg.ExecContext(ctx, `INSERT INTO sync_metadata(key,value) VALUES('usage_change_journal_version','999')
 ON CONFLICT(key) DO UPDATE SET value=excluded.value;
 CREATE OR REPLACE FUNCTION mark_usage_change() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NULL; END;'`)
	require.NoError(t, err)
	require.NoError(t, ensureUsageChangeJournalPG(ctx, syncer.pg))
	var body string
	require.NoError(t, syncer.pg.QueryRowContext(ctx, `SELECT prosrc FROM pg_proc WHERE oid=to_regprocedure('mark_usage_change()')`).Scan(&body))
	assert.Equal(t, "BEGIN RETURN NULL; END;", body, "an older binary must preserve the newer journal")
	revision, err := store.UsageChangeRevision(ctx)
	require.NoError(t, err)
	assert.Zero(t, revision)
	through, ids, err := store.LoadUsageChanges(ctx, 0)
	require.NoError(t, err)
	assert.Zero(t, through)
	assert.Empty(t, ids)
}

func TestUsageChangeJournalPricingSentinels(t *testing.T) {
	_, syncer, store := newUsageJournalFixture(t)
	ctx := t.Context()
	for _, table := range []string{"model_pricing", "model_pricing_bands"} {
		t.Run(table, func(t *testing.T) {
			columns, values := "model_pattern", "'_sentinel'"
			if table == "model_pricing_bands" {
				_, err := syncer.pg.ExecContext(ctx, `INSERT INTO model_pricing(model_pattern) VALUES('_sentinel'),('_sentinel_updated'),('real-model')`)
				require.NoError(t, err)
				columns += ",above_input_tokens,input_microdollars_per_mtok,output_microdollars_per_mtok,cache_creation_microdollars_per_mtok,cache_read_microdollars_per_mtok"
				values += ",0,0,0,0,0"
			}
			before, err := store.UsageChangeRevision(ctx)
			require.NoError(t, err)
			for _, step := range []struct {
				query string
				delta int64
			}{
				{`INSERT INTO ` + table + `(` + columns + `) VALUES(` + values + `)`, 0},
				{`UPDATE ` + table + ` SET model_pattern='_sentinel_updated' WHERE model_pattern='_sentinel'`, 0},
				{`UPDATE ` + table + ` SET model_pattern='real-model' WHERE model_pattern='_sentinel_updated'`, 1},
				{`UPDATE ` + table + ` SET input_microdollars_per_mtok=123 WHERE model_pattern='real-model'`, 1},
				{`UPDATE ` + table + ` SET model_pattern='_sentinel_updated' WHERE model_pattern='real-model'`, 1},
				{`DELETE FROM ` + table + ` WHERE model_pattern='_sentinel_updated'`, 0},
				{`INSERT INTO ` + table + `(` + columns + `) VALUES(` + strings.Replace(values, "'_sentinel'", "'real-model'", 1) + `)`, 1},
				{`DELETE FROM ` + table + ` WHERE model_pattern='real-model'`, 1},
			} {
				_, err = syncer.pg.ExecContext(ctx, step.query)
				require.NoError(t, err)
				after, err := store.UsageChangeRevision(ctx)
				require.NoError(t, err)
				assert.Equal(t, before+step.delta, after, step.query)
				before = after
			}
		})
	}
}

func TestUsageChangeJournalRepeatedSessionChange(t *testing.T) {
	_, syncer, store := newUsageJournalFixture(t)
	ctx := t.Context()
	before, err := store.UsageChangeRevision(ctx)
	require.NoError(t, err)
	_, err = syncer.pg.ExecContext(ctx, `INSERT INTO sessions(id,machine,project,agent) VALUES('repeated','machine','project','codex')`)
	require.NoError(t, err)
	probe, err := store.UsageChangeRevision(ctx)
	require.NoError(t, err)
	// A second push replaces this session's journal row after a consumer's probe.
	_, err = syncer.pg.ExecContext(ctx, `UPDATE sessions SET project='changed' WHERE id='repeated'`)
	require.NoError(t, err)
	through, ids, err := store.LoadUsageChanges(ctx, before)
	require.NoError(t, err)
	assert.Equal(t, probe+1, through)
	assert.Equal(t, []string{"repeated"}, ids)
	_, err = syncer.pg.ExecContext(ctx, `UPDATE sessions SET project='changed-again' WHERE id='repeated'`)
	require.NoError(t, err)
	next, ids, err := store.LoadUsageChanges(ctx, through)
	require.NoError(t, err)
	assert.Equal(t, through+1, next)
	assert.Equal(t, []string{"repeated"}, ids)
}

func TestUsageChangeJournalOlderVersion(t *testing.T) {
	_, syncer, store := newUsageJournalFixture(t)
	ctx := t.Context()
	_, err := syncer.pg.ExecContext(ctx, `UPDATE sync_metadata SET value='0' WHERE key='usage_change_journal_version';
 DROP TRIGGER trg_uc_sessions_delete ON sessions;
 INSERT INTO sessions(id,machine,project,agent) VALUES('deleted','machine','project','codex')`)
	require.NoError(t, err)
	require.NoError(t, ensureUsageChangeJournalPG(ctx, syncer.pg))
	before, err := store.UsageChangeRevision(ctx)
	require.NoError(t, err)
	require.Positive(t, before)
	_, err = syncer.pg.ExecContext(ctx, `DELETE FROM sessions WHERE id='deleted'`)
	require.NoError(t, err)
	through, ids, err := store.LoadUsageChanges(ctx, before)
	require.NoError(t, err)
	assert.Equal(t, before+1, through)
	assert.Equal(t, []string{"deleted"}, ids)
}
