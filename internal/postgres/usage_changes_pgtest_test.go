//go:build pgtest

package postgres

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/url"
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
		{"plan usage", func(t *testing.T) {
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
			ids, err := store.LoadUsageChanges(ctx, before, after)
			require.NoError(t, err)
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
		ids, err := store.LoadUsageChanges(ctx, before, after)
		require.NoError(t, err)
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
	ids, err := store.LoadUsageChanges(ctx, before, middle)
	require.NoError(t, err)
	assert.Equal(t, []string{"second"}, ids)
	require.NoError(t, first.Commit())
	after, err := store.UsageChangeRevision(ctx)
	require.NoError(t, err)
	assert.Equal(t, middle+1, after)
	ids, err = store.LoadUsageChanges(ctx, during, after)
	require.NoError(t, err)
	assert.Equal(t, []string{"second", "first"}, ids)
	_, err = store.LoadUsageChanges(ctx, after, before)
	require.Error(t, err)
	ids, err = store.LoadUsageChanges(ctx, after, after)
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
	_, err = syncer.pg.ExecContext(ctx, `DROP FUNCTION mark_usage_change() CASCADE; DROP TABLE usage_session_changes; DELETE FROM sync_metadata WHERE key='usage_change_revision'`)
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
	ids, err := store.LoadUsageChanges(ctx, before, after)
	require.NoError(t, err)
	assert.Equal(t, []string{"restricted"}, ids)
	_, err = restricted.LoadUsageChanges(ctx, before, after)
	require.Error(t, err)
	// A publisher without CREATE can continue on a schema lacking the journal.
	_, err = syncer.pg.ExecContext(ctx, `DROP FUNCTION mark_usage_change() CASCADE; DROP TABLE usage_session_changes; DELETE FROM sync_metadata WHERE key='usage_change_revision'`)
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
	_, err := syncer.pg.ExecContext(ctx, `INSERT INTO sessions(id,machine,project,agent) VALUES('a','machine','project','codex'),('b','machine','project','codex');
 INSERT INTO usage_events(session_id,source,model) VALUES('a','provider','model-a');
 INSERT INTO session_plan_usage(session_id) VALUES('a')`)
	require.NoError(t, err)
	for _, tc := range []struct {
		name, query string
		ids         []string
	}{
		{"usage event", `UPDATE usage_events SET session_id='b' WHERE session_id='a'`, []string{"a", "b"}},
		{"plan usage", `UPDATE session_plan_usage SET session_id='b' WHERE session_id='a'`, []string{"a", "b"}},
		{"session identity", `UPDATE sessions SET id='c' WHERE id='a'`, []string{"a", "c"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, err := store.UsageChangeRevision(ctx)
			require.NoError(t, err)
			_, err = syncer.pg.ExecContext(ctx, tc.query)
			require.NoError(t, err)
			after, err := store.UsageChangeRevision(ctx)
			require.NoError(t, err)
			assert.Equal(t, before+1, after)
			ids, err := store.LoadUsageChanges(ctx, before, after)
			require.NoError(t, err)
			assert.Equal(t, tc.ids, ids)
		})
	}
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
	ids, err := store.LoadUsageChanges(ctx, before, after)
	require.NoError(t, err)
	assert.Equal(t, []string{"b", "c"}, ids)
}
