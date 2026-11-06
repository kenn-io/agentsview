//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
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
	"go.kenn.io/agentsview/internal/pricing"
	"go.kenn.io/agentsview/internal/storage"
)

const dropUsageChangeJournalSQL = `DROP TABLE usage_session_changes; DROP FUNCTION mark_usage_change() CASCADE; DROP FUNCTION stamp_usage_change();
 DELETE FROM sync_metadata WHERE key IN ('usage_change_revision','usage_change_journal_version')`

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
		{"openrouter bookkeeping", func(t *testing.T) {
			require.NoError(t, local.SetPricingMeta(pricing.OpenRouterModelsMetaKey, pricing.EncodeOpenRouterModels([]string{"model-a"})))
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
			if tc.name == "openrouter bookkeeping" {
				var published bool
				require.NoError(t, syncer.pg.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM model_pricing WHERE model_pattern = $1)`, pricing.OpenRouterModelsMetaKey).Scan(&published))
				require.True(t, published, "push publishes the bookkeeping row")
				assert.Equal(t, before, after)
				return
			}
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
	installed, err := usageChangeJournalInstalledPG(ctx, syncer.pg)
	require.NoError(t, err)
	assert.True(t, installed)
	var triggers int
	require.NoError(t, syncer.pg.QueryRowContext(ctx, `SELECT count(*) FROM pg_trigger WHERE tgname LIKE 'trg_uc_%' AND tgrelid IN (SELECT oid FROM pg_class WHERE relnamespace = current_schema()::regnamespace)`).Scan(&triggers))
	assert.Equal(t, 3*len(usageChangeTablesPG)+1, triggers)
	// The ordinary push fast path upgrades an existing schema too.
	_, err = syncer.pg.ExecContext(ctx, dropUsageChangeJournalSQL)
	require.NoError(t, err)
	absent, err := store.UsageChangeRevision(ctx)
	require.NoError(t, err)
	assert.Zero(t, absent)
	require.True(t, pushSchemaCurrent(ctx, syncer.pg))
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
	_, err = syncer.pg.ExecContext(ctx, dropUsageChangeJournalSQL)
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
	_, err = syncer.pg.ExecContext(ctx, dropUsageChangeJournalSQL)
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
	_, err := syncer.pg.ExecContext(ctx, dropUsageChangeJournalSQL)
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
	_, err := syncer.pg.ExecContext(ctx, `INSERT INTO sync_metadata(key,value) VALUES('usage_change_journal_version','2')
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

// v0.46.1 replaces usage events without touching sessions when a session has
// no new messages (push.go replaceUsageEvents with no updated_at bump).
func TestUsageChangeJournalReleasedUsageOnlyWrite(t *testing.T) {
	_, syncer, store := newUsageJournalFixture(t)
	ctx := t.Context()
	_, err := syncer.pg.ExecContext(ctx, `INSERT INTO sessions(id,machine,project,agent) VALUES('hermes','machine','project','hermes');
 INSERT INTO usage_events(session_id,source,model,input_tokens,dedup_key) VALUES('hermes','provider','model-a',100,'event-a')`)
	require.NoError(t, err)
	before, err := store.UsageChangeRevision(ctx)
	require.NoError(t, err)
	tx, err := syncer.pg.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `DELETE FROM usage_events WHERE session_id = $1`, "hermes")
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `INSERT INTO usage_events(session_id,source,model,input_tokens,dedup_key) VALUES('hermes','provider','model-a',200,'event-a')`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	through, ids, err := store.LoadUsageChanges(ctx, before)
	require.NoError(t, err)
	assert.Greater(t, through, before)
	assert.Equal(t, []string{"hermes"}, ids)
}

func TestUsageChangeJournalWatchedTables(t *testing.T) {
	_, syncer, store := newUsageJournalFixture(t)
	ctx := t.Context()
	_, err := syncer.pg.ExecContext(ctx, `INSERT INTO sessions(id,machine,project,agent) VALUES('owner','machine','project','codex'),('other','machine','project','codex');
 INSERT INTO model_pricing(model_pattern) VALUES('parent-model');
 INSERT INTO source_project_identity_observations(source_archive_id,project,machine,observed_at) VALUES('archive','parent','machine',now());
 INSERT INTO source_session_project_identity_snapshots(source_archive_id,source_database_generation,source_session_id,project,machine,observed_at) VALUES('archive','generation','parent','project','machine',now());
 INSERT INTO source_worktree_project_mappings(source_archive_id,machine,path_prefix) VALUES('archive','machine','/parent')`)
	require.NoError(t, err)
	type step struct {
		query string
		ids   []string
	}
	cases := []struct {
		table string
		steps []step
	}{
		{"sessions", []step{
			{`INSERT INTO sessions(id,machine,project,agent) VALUES('row','machine','project','codex')`, []string{"row"}},
			{`UPDATE sessions SET project='changed' WHERE id='row'`, []string{"row"}},
			{`DELETE FROM sessions WHERE id='row'`, []string{"row"}},
		}},
		{"usage_events", []step{
			{`INSERT INTO usage_events(session_id,source,model) VALUES('owner','provider','model-a')`, []string{"owner"}},
			{`UPDATE usage_events SET session_id='other' WHERE session_id='owner'`, []string{"other", "owner"}},
			{`DELETE FROM usage_events WHERE session_id='other'`, []string{"other"}},
		}},
		{"model_pricing", []step{
			{`INSERT INTO model_pricing(model_pattern) VALUES('row-model')`, nil},
			{`UPDATE model_pricing SET input_microdollars_per_mtok=1 WHERE model_pattern='row-model'`, nil},
			{`DELETE FROM model_pricing WHERE model_pattern='row-model'`, nil},
		}},
		{"model_pricing_bands", []step{
			{`INSERT INTO model_pricing_bands(model_pattern,above_input_tokens,input_microdollars_per_mtok,output_microdollars_per_mtok,cache_creation_microdollars_per_mtok,cache_read_microdollars_per_mtok) VALUES('parent-model',1000,0,0,0,0)`, nil},
			{`UPDATE model_pricing_bands SET input_microdollars_per_mtok=1`, nil},
			{`DELETE FROM model_pricing_bands`, nil},
		}},
		{"genai_pricing", []step{
			{`INSERT INTO genai_pricing(singleton,version,source,data_json) VALUES(1,'v1','source','\x00')`, nil},
			{`UPDATE genai_pricing SET version='v2'`, nil},
			{`DELETE FROM genai_pricing`, nil},
		}},
		{"cursor_usage_events", []step{
			{`INSERT INTO cursor_usage_events(occurred_at,model) VALUES(now(),'model-a')`, nil},
			{`UPDATE cursor_usage_events SET input_tokens=1`, nil},
			{`DELETE FROM cursor_usage_events`, nil},
		}},
		{"source_project_identity_observations", []step{
			{`INSERT INTO source_project_identity_observations(source_archive_id,project,machine,observed_at) VALUES('archive','row','machine',now())`, nil},
			{`UPDATE source_project_identity_observations SET git_branch='main' WHERE project='row'`, nil},
			{`DELETE FROM source_project_identity_observations WHERE project='row'`, nil},
		}},
		{"source_project_identity_observation_scopes", []step{
			{`INSERT INTO source_project_identity_observation_scopes(source_archive_id,project,machine,publication_scope) VALUES('archive','parent','machine','scope')`, nil},
			{`UPDATE source_project_identity_observation_scopes SET publication_scope='changed'`, nil},
			{`DELETE FROM source_project_identity_observation_scopes`, nil},
		}},
		{"source_session_project_identity_snapshots", []step{
			{`INSERT INTO source_session_project_identity_snapshots(source_archive_id,source_database_generation,source_session_id,project,machine,observed_at) VALUES('archive','generation','row','project','machine',now())`, nil},
			{`UPDATE source_session_project_identity_snapshots SET git_branch='main' WHERE source_session_id='row'`, nil},
			{`DELETE FROM source_session_project_identity_snapshots WHERE source_session_id='row'`, nil},
		}},
		{"source_session_project_identity_snapshot_scopes", []step{
			{`INSERT INTO source_session_project_identity_snapshot_scopes(source_archive_id,source_database_generation,source_session_id,publication_scope) VALUES('archive','generation','parent','scope')`, nil},
			{`UPDATE source_session_project_identity_snapshot_scopes SET publication_scope='changed'`, nil},
			{`DELETE FROM source_session_project_identity_snapshot_scopes`, nil},
		}},
		{"source_worktree_project_mappings", []step{
			{`INSERT INTO source_worktree_project_mappings(source_archive_id,machine,path_prefix) VALUES('archive','machine','/row')`, nil},
			{`UPDATE source_worktree_project_mappings SET project='changed' WHERE path_prefix='/row'`, nil},
			{`DELETE FROM source_worktree_project_mappings WHERE path_prefix='/row'`, nil},
		}},
		{"source_worktree_project_mapping_scopes", []step{
			{`INSERT INTO source_worktree_project_mapping_scopes(source_archive_id,machine,path_prefix,publication_scope) VALUES('archive','machine','/parent','scope')`, nil},
			{`UPDATE source_worktree_project_mapping_scopes SET publication_scope='changed'`, nil},
			{`DELETE FROM source_worktree_project_mapping_scopes`, nil},
		}},
	}
	tables := make([]string, len(cases))
	for i, tc := range cases {
		tables[i] = tc.table
	}
	watched := make([]string, len(usageChangeTablesPG))
	for i, table := range usageChangeTablesPG {
		watched[i] = table.table
	}
	require.Equal(t, watched, tables, "every watched table has a case")
	for _, tc := range cases {
		t.Run(tc.table, func(t *testing.T) {
			for _, step := range tc.steps {
				before, err := store.UsageChangeRevision(ctx)
				require.NoError(t, err)
				_, err = syncer.pg.ExecContext(ctx, step.query)
				require.NoError(t, err)
				through, ids, err := store.LoadUsageChanges(ctx, before)
				require.NoError(t, err)
				assert.Equal(t, before+1, through, step.query)
				assert.Equal(t, step.ids, ids, step.query)
			}
		})
	}
}

func TestUsageChangeJournalReaderStraddlesPush(t *testing.T) {
	local, syncer, store := newUsageJournalFixture(t)
	ctx := t.Context()
	reader, err := syncer.pg.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	require.NoError(t, err)
	defer reader.Rollback()
	var started int64
	require.NoError(t, reader.QueryRowContext(ctx, `SELECT value::bigint FROM sync_metadata WHERE key = 'usage_change_revision'`).Scan(&started))
	require.NoError(t, local.UpsertSession(ctx, db.Session{ID: "straddled", Machine: "test-machine", Project: "project-a", Agent: "codex", CreatedAt: "2026-10-01T09:00:00Z"}))
	require.NoError(t, local.ReplaceSessionUsageEvents(ctx, "straddled", []db.UsageEvent{{SessionID: "straddled", Source: "provider", Model: "model-a", InputTokens: 100, DedupKey: "event-a"}}))
	result, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Empty(t, result.Errors)
	// The reader's report predates the push it finishes after.
	var events int
	require.NoError(t, reader.QueryRowContext(ctx, `SELECT count(*) FROM usage_events WHERE session_id = 'straddled'`).Scan(&events))
	assert.Zero(t, events)
	require.NoError(t, reader.Commit())
	through, ids, err := store.LoadUsageChanges(ctx, started)
	require.NoError(t, err)
	assert.Greater(t, through, started)
	assert.Equal(t, []string{"straddled"}, ids)
}

func TestUsageChangeJournalMissingCounter(t *testing.T) {
	_, syncer, store := newUsageJournalFixture(t)
	ctx := t.Context()
	_, err := syncer.pg.ExecContext(ctx, `DELETE FROM sync_metadata WHERE key = 'usage_change_revision'`)
	require.NoError(t, err)
	_, err = syncer.pg.ExecContext(ctx, `INSERT INTO sessions(id,machine,project,agent) VALUES('uncounted','machine','project','codex')`)
	require.NoError(t, err, "session writes continue without the counter row")
	through, ids, err := store.LoadUsageChanges(ctx, 0)
	require.NoError(t, err)
	assert.Greater(t, through, int64(1000000000000000), "the counter restarts from the clock")
	assert.Contains(t, ids, "uncounted")
}

func TestUsageChangeJournalLateGrantedReader(t *testing.T) {
	_, syncer, store := newUsageJournalFixture(t)
	ctx := t.Context()
	_, err := syncer.pg.ExecContext(ctx, `INSERT INTO sessions(id,machine,project,agent) VALUES('visible','machine','project','codex')`)
	require.NoError(t, err)
	revision, err := store.UsageChangeRevision(ctx)
	require.NoError(t, err)
	require.Positive(t, revision)
	role := fmt.Sprintf("usage_reader_%x", rand.Uint64())
	_, err = syncer.pg.ExecContext(ctx, `CREATE ROLE `+role+` LOGIN PASSWORD 'test-password'`)
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanup := context.WithoutCancel(ctx)
		_, err := syncer.pg.ExecContext(cleanup, `DROP OWNED BY `+role+`; DROP ROLE `+role)
		require.NoError(t, err)
	})
	// Granted after installation, so the journal itself stays unreadable.
	_, err = syncer.pg.ExecContext(ctx, `GRANT USAGE ON SCHEMA `+syncer.schema+` TO `+role+`; GRANT SELECT ON sessions, sync_metadata TO `+role)
	require.NoError(t, err)
	roleURL, err := url.Parse(testPGURL(t))
	require.NoError(t, err)
	roleURL.User = url.UserPassword(role, "test-password")
	reader, err := NewStore(ctx, roleURL.String(), syncer.schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	revision, err = reader.UsageChangeRevision(ctx)
	require.NoError(t, err)
	assert.Zero(t, revision)
	through, ids, err := reader.LoadUsageChanges(ctx, 0)
	require.NoError(t, err)
	assert.Zero(t, through)
	assert.Empty(t, ids)
}
