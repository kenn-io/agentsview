//go:build pgtest

package postgres

import (
	"context"
	"fmt"
	"math/rand/v2"
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

func TestUsageChangeJournalEmptyArchivePublication(t *testing.T) {
	local, syncer, store := newUsageJournalFixture(t)
	ctx := t.Context()
	require.NoError(t, local.UpsertSession(ctx, db.Session{ID: "session-a", Project: "project-a", Machine: "test-machine", Agent: "codex"}))
	result, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Empty(t, result.Errors)
	empty := testDB(t)
	second, err := New(ctx, testPGURL(t), syncer.schema, empty, "second-machine", true, storage.PusherOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close()) })
	require.NoError(t, second.EnsureSchema(ctx))
	beforeProjects, err := store.BuildProjectIdentityMap(ctx, []string{"project-a"})
	require.NoError(t, err)
	require.NotEmpty(t, beforeProjects["project-a"].ProjectKey)
	before, err := store.UsageChangeRevision(ctx)
	require.NoError(t, err)

	result, err = second.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Empty(t, result.Errors)
	require.Zero(t, result.SessionsPushed)
	afterProjects, err := store.BuildProjectIdentityMap(ctx, []string{"project-a"})
	require.NoError(t, err)
	require.NotEmpty(t, afterProjects["project-a"].ProjectKey)
	require.NotEqual(t, beforeProjects["project-a"].ProjectKey, afterProjects["project-a"].ProjectKey)
	after, ids, err := store.LoadUsageChanges(ctx, before)
	require.NoError(t, err)
	assert.Greater(t, after, before, "a changed project key must advance the revision")
	assert.Empty(t, ids)
}

func TestUsageChangeJournalWriteAfterImmediateStamp(t *testing.T) {
	for _, mode := range []string{"IMMEDIATE", "DEFERRED"} {
		t.Run(mode, func(t *testing.T) {
			_, syncer, store := newUsageJournalFixture(t)
			ctx := t.Context()
			before, err := store.UsageChangeRevision(ctx)
			require.NoError(t, err)
			tx, err := syncer.pg.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer tx.Rollback()
			_, err = tx.ExecContext(ctx, `INSERT INTO sessions(id,machine,project,agent) VALUES('first','machine','project','codex'); SET CONSTRAINTS ALL IMMEDIATE`)
			require.NoError(t, err)
			_, err = tx.ExecContext(ctx, "SET CONSTRAINTS ALL "+mode)
			require.NoError(t, err)
			_, err = tx.ExecContext(ctx, `INSERT INTO sessions(id,machine,project,agent) VALUES('later','machine','project','codex'); UPDATE sessions SET project='changed' WHERE id='first'`)
			require.NoError(t, err)
			require.NoError(t, tx.Commit())
			through, ids, err := store.LoadUsageChanges(ctx, before)
			require.NoError(t, err)
			assert.Greater(t, through, before+1)
			assert.ElementsMatch(t, []string{"first", "later"}, ids)
		})
	}
}

// newUsageJournalRole returns a login URL for a new role holding the grants.
func newUsageJournalRole(t *testing.T, syncer *Sync, grants string) string {
	t.Helper()
	role := fmt.Sprintf("usage_role_%x", rand.Uint64())
	_, err := syncer.pg.ExecContext(t.Context(), `CREATE ROLE `+role+` LOGIN PASSWORD 'test-password'; GRANT USAGE ON SCHEMA `+syncer.schema+` TO `+role+`; `+strings.ReplaceAll(grants, "ROLE", role))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := syncer.pg.ExecContext(context.WithoutCancel(t.Context()), `DROP OWNED BY `+role+`; DROP ROLE `+role)
		require.NoError(t, err)
	})
	roleDSN, err := appendConnParams(testPGURL(t), map[string]string{"user": role, "password": "test-password"})
	require.NoError(t, err)
	return roleDSN
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
	// Each case inserts, updates, and deletes one row; session tables list the
	// session IDs each step journals, and the others only move the revision.
	cases := []struct {
		table string
		steps [3]string
		ids   [3][]string
	}{
		{"sessions", [3]string{`INSERT INTO sessions(id,machine,project,agent) VALUES('row','machine','project','codex')`, `UPDATE sessions SET project='changed' WHERE id='row'`, `DELETE FROM sessions WHERE id='row'`}, [3][]string{{"row"}, {"row"}, {"row"}}},
		{"usage_events", [3]string{`INSERT INTO usage_events(session_id,source,model) VALUES('owner','provider','model-a')`, `UPDATE usage_events SET session_id='other' WHERE session_id IN ('owner','other')`, `DELETE FROM usage_events WHERE session_id='other'`}, [3][]string{{"owner"}, {"other", "owner"}, {"other"}}},
		{"model_pricing", [3]string{`INSERT INTO model_pricing(model_pattern) VALUES('row-model')`, `UPDATE model_pricing SET input_microdollars_per_mtok=1 WHERE model_pattern='row-model'`, `DELETE FROM model_pricing WHERE model_pattern='row-model'`}, [3][]string{}},
		{"model_pricing_bands", [3]string{`INSERT INTO model_pricing_bands(model_pattern,above_input_tokens,input_microdollars_per_mtok,output_microdollars_per_mtok,cache_creation_microdollars_per_mtok,cache_read_microdollars_per_mtok) VALUES('parent-model',1000,0,0,0,0)`, `UPDATE model_pricing_bands SET input_microdollars_per_mtok=1`, `DELETE FROM model_pricing_bands`}, [3][]string{}},
		{"genai_pricing", [3]string{`INSERT INTO genai_pricing(singleton,version,source,data_json) VALUES(1,'v1','source','\x00')`, `UPDATE genai_pricing SET version='v2'`, `DELETE FROM genai_pricing`}, [3][]string{}},
		{"cursor_usage_events", [3]string{`INSERT INTO cursor_usage_events(occurred_at,model) VALUES(now(),'model-a')`, `UPDATE cursor_usage_events SET input_tokens=1`, `DELETE FROM cursor_usage_events`}, [3][]string{}},
		{"source_archives", [3]string{`INSERT INTO source_archives(source_archive_id,source_archive_salt) VALUES('row-archive','salt-a')`, `UPDATE source_archives SET source_archive_salt='salt-b' WHERE source_archive_id='row-archive'`, `DELETE FROM source_archives WHERE source_archive_id='row-archive'`}, [3][]string{}},
		{"source_project_identity_observations", [3]string{`INSERT INTO source_project_identity_observations(source_archive_id,project,machine,observed_at) VALUES('archive','row','machine',now())`, `UPDATE source_project_identity_observations SET git_branch='main' WHERE project='row'`, `DELETE FROM source_project_identity_observations WHERE project='row'`}, [3][]string{}},
		{"source_project_identity_observation_scopes", [3]string{`INSERT INTO source_project_identity_observation_scopes(source_archive_id,project,machine,publication_scope) VALUES('archive','parent','machine','scope')`, `UPDATE source_project_identity_observation_scopes SET publication_scope='changed'`, `DELETE FROM source_project_identity_observation_scopes`}, [3][]string{}},
		{"source_session_project_identity_snapshots", [3]string{`INSERT INTO source_session_project_identity_snapshots(source_archive_id,source_database_generation,source_session_id,project,machine,observed_at) VALUES('archive','generation','row','project','machine',now())`, `UPDATE source_session_project_identity_snapshots SET git_branch='main' WHERE source_session_id='row'`, `DELETE FROM source_session_project_identity_snapshots WHERE source_session_id='row'`}, [3][]string{}},
		{"source_session_project_identity_snapshot_scopes", [3]string{`INSERT INTO source_session_project_identity_snapshot_scopes(source_archive_id,source_database_generation,source_session_id,publication_scope) VALUES('archive','generation','parent','scope')`, `UPDATE source_session_project_identity_snapshot_scopes SET publication_scope='changed'`, `DELETE FROM source_session_project_identity_snapshot_scopes`}, [3][]string{}},
		{"source_worktree_project_mappings", [3]string{`INSERT INTO source_worktree_project_mappings(source_archive_id,machine,path_prefix) VALUES('archive','machine','/row')`, `UPDATE source_worktree_project_mappings SET project='changed' WHERE path_prefix='/row'`, `DELETE FROM source_worktree_project_mappings WHERE path_prefix='/row'`}, [3][]string{}},
		{"source_worktree_project_mapping_scopes", [3]string{`INSERT INTO source_worktree_project_mapping_scopes(source_archive_id,machine,path_prefix,publication_scope) VALUES('archive','machine','/parent','scope')`, `UPDATE source_worktree_project_mapping_scopes SET publication_scope='changed'`, `DELETE FROM source_worktree_project_mapping_scopes`}, [3][]string{}},
	}
	require.Len(t, cases, len(usageChangeTablesPG), "every watched table has a case")
	for i, tc := range cases {
		require.Equal(t, usageChangeTablesPG[i].table, tc.table)
		t.Run(tc.table, func(t *testing.T) {
			for step, query := range tc.steps {
				before, err := store.UsageChangeRevision(ctx)
				require.NoError(t, err)
				_, err = syncer.pg.ExecContext(ctx, query)
				require.NoError(t, err)
				through, ids, err := store.LoadUsageChanges(ctx, before)
				require.NoError(t, err)
				assert.Equal(t, before+1, through, query)
				assert.Equal(t, tc.ids[step], ids, query)
				if tc.table == "usage_events" && step == 0 {
					// Released publishers replace usage without touching sessions.
					_, err = syncer.pg.ExecContext(ctx, `DELETE FROM usage_events WHERE session_id='owner';
 INSERT INTO usage_events(session_id,source,model,input_tokens) VALUES('owner','provider','model-a',200)`)
					require.NoError(t, err)
					replaced, replacedIDs, err := store.LoadUsageChanges(ctx, through)
					require.NoError(t, err)
					assert.Equal(t, through+1, replaced, "delete and reinsert share one revision")
					assert.Equal(t, []string{"owner"}, replacedIDs)
				}
				if step == 1 {
					// Identity mapping upserts can rewrite unchanged rows.
					result, err := syncer.pg.ExecContext(ctx, query)
					require.NoError(t, err)
					affected, err := result.RowsAffected()
					require.NoError(t, err)
					require.EqualValues(t, 1, affected, "unchanged update must match a row")
					unchanged, unchangedIDs, err := store.LoadUsageChanges(ctx, through)
					require.NoError(t, err)
					assert.Equal(t, through, unchanged, query)
					assert.Empty(t, unchangedIDs, query)
				}
			}
		})
	}
}

func TestUsageChangeJournalPushPaths(t *testing.T) {
	local, syncer, store := newUsageJournalFixture(t)
	ctx := t.Context()
	push := func(t *testing.T, full bool) {
		t.Helper()
		result, err := syncer.Push(ctx, full, nil)
		require.NoError(t, err)
		require.Empty(t, result.Errors)
	}
	// Publish catalog and identity metadata before measuring individual writes.
	push(t, false)
	session := db.Session{ID: "journal-session", Project: "project-a", Machine: "test-machine", Agent: "codex", CreatedAt: "2026-10-01T09:00:00Z"}
	for _, tc := range []struct {
		name   string
		change func(t *testing.T)
		ids    []string
		full   bool
	}{
		{"new session", func(t *testing.T) { require.NoError(t, local.UpsertSession(ctx, session)) }, []string{session.ID}, false},
		{"fresh publisher unchanged sessions", func(t *testing.T) {
			var hashBefore string
			require.NoError(t, syncer.pg.QueryRowContext(ctx, `SELECT coalesce(automation_audit_hash, '') FROM sessions WHERE id = $1`, session.ID).Scan(&hashBefore))
			require.Empty(t, hashBefore, "published session awaits automation audit")
			fresh, err := New(ctx, testPGURL(t), syncer.schema, local, "test-machine", true, storage.PusherOptions{})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, fresh.Close()) })
			result, err := fresh.Push(ctx, false, nil)
			require.NoError(t, err)
			require.Empty(t, result.Errors)
			assert.Zero(t, result.SessionsPushed)
			var hashAfter string
			require.NoError(t, syncer.pg.QueryRowContext(ctx, `SELECT coalesce(automation_audit_hash, '') FROM sessions WHERE id = $1`, session.ID).Scan(&hashAfter))
			assert.NotEmpty(t, hashAfter, "fresh publisher stamps the automation audit")
		}, nil, false},
		{"message append", func(t *testing.T) {
			require.NoError(t, local.InsertMessages(ctx, []db.Message{{SessionID: session.ID, Ordinal: 0, Role: "user", Content: "Explain this query"}}))
		}, []string{session.ID}, false},
		{"message rewrite", func(t *testing.T) {
			require.NoError(t, local.ReplaceSessionMessages(ctx, session.ID, []db.Message{{SessionID: session.ID, Ordinal: 0, Role: "user", Content: "Explain the changed query"}}))
		}, []string{session.ID}, false},
		{"rename", func(t *testing.T) {
			require.NoError(t, local.RenameSession(ctx, session.ID, new("Query explanation")))
		}, []string{session.ID}, false},
		{"usage only", func(t *testing.T) {
			require.NoError(t, local.ReplaceSessionUsageEvents(ctx, session.ID, []db.UsageEvent{{SessionID: session.ID, Source: "provider", Model: "model-a", InputTokens: 1200, OutputTokens: 300, DedupKey: "event-a"}}))
		}, []string{session.ID}, false},
		{"plan usage only", func(t *testing.T) {
			require.NoError(t, local.ReplaceSessionPlanUsage(ctx, session.ID, fullPlanUsage()))
		}, []string{session.ID}, false},
		{"pricing", func(t *testing.T) {
			require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{ModelPattern: "model-a", InputPerMTok: money.Money{Microdollars: 1000000}}}))
		}, nil, false},
		{"openrouter bookkeeping", func(t *testing.T) {
			require.NoError(t, local.SetPricingMeta(pricing.OpenRouterModelsMetaKey, pricing.EncodeOpenRouterModels([]string{"model-a"})))
		}, nil, false},
		{"project identity", func(t *testing.T) {
			require.NoError(t, local.UpsertProjectIdentityObservation(ctx, export.ProjectIdentityObservation{Project: "project-c", Machine: "test-machine", RootPath: "/workspace/project-c", GitRemote: "https://example.com/project-c.git"}))
		}, nil, false},
		{"soft delete", func(t *testing.T) {
			require.NoError(t, local.SoftDeleteSession(ctx, session.ID))
		}, []string{session.ID}, false},
		{"hard delete", func(t *testing.T) {
			require.NoError(t, local.DeleteSession(ctx, session.ID))
		}, []string{session.ID}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, err := store.UsageChangeRevision(ctx)
			require.NoError(t, err)
			tc.change(t)
			push(t, tc.full)
			after, err := store.UsageChangeRevision(ctx)
			require.NoError(t, err)
			if tc.name == "fresh publisher unchanged sessions" {
				assert.Equal(t, before, after, "audit bookkeeping must preserve the revision")
				_, ids, err := store.LoadUsageChanges(ctx, before)
				require.NoError(t, err)
				assert.Empty(t, ids, "audit bookkeeping must leave sessions out of the journal")
				return
			}
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
			push(t, false)
			unchanged, err := store.UsageChangeRevision(ctx)
			require.NoError(t, err)
			assert.Equal(t, after, unchanged, "unchanged push")
		})
	}
}

// The transaction that commits later gets the later revision, whichever began first.
func TestUsageChangeJournalCommitOrder(t *testing.T) {
	_, syncer, store := newUsageJournalFixture(t)
	ctx := t.Context()
	before, err := store.UsageChangeRevision(ctx)
	require.NoError(t, err)
	first, err := syncer.pg.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer first.Rollback()
	_, err = first.ExecContext(ctx, `INSERT INTO sessions(id,machine,project,agent) VALUES('first','machine','project','codex')`)
	require.NoError(t, err)
	_, err = syncer.pg.ExecContext(ctx, `INSERT INTO sessions(id,machine,project,agent) VALUES('second','machine','project','codex')`)
	require.NoError(t, err)
	middle, ids, err := store.LoadUsageChanges(ctx, before)
	require.NoError(t, err)
	assert.Equal(t, before+1, middle)
	assert.Equal(t, []string{"second"}, ids)
	require.NoError(t, first.Commit())
	after, ids, err := store.LoadUsageChanges(ctx, middle)
	require.NoError(t, err)
	assert.Equal(t, middle+1, after)
	assert.Equal(t, []string{"first"}, ids)
}

func TestUsageChangeJournalInstallIdempotent(t *testing.T) {
	_, syncer, store := newUsageJournalFixture(t)
	ctx := t.Context()
	before, err := store.UsageChangeRevision(ctx)
	require.NoError(t, err)
	require.Greater(t, before, int64(1000000000000000), "installation seeds a clock-based revision")
	require.NoError(t, EnsureSchema(ctx, syncer.pg, syncer.schema))
	after, err := store.UsageChangeRevision(ctx)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	// Both serve setup branches must install a missing journal.
	_, err = syncer.pg.ExecContext(ctx, `INSERT INTO sessions(id,machine,project,agent) VALUES('repair','machine','project','codex')`)
	require.NoError(t, err)
	for _, repair := range []bool{false, true} {
		_, err = syncer.pg.ExecContext(ctx, dropUsageChangeJournalSQL)
		require.NoError(t, err)
		if repair {
			_, err = syncer.pg.ExecContext(ctx, `DELETE FROM sync_metadata WHERE key=$1`, tokenCoverageRepairMetadataKey)
		} else {
			err = markTokenCoverageRepairDone(ctx, syncer.pg)
		}
		require.NoError(t, err)
		require.NoError(t, EnsureSchema(ctx, syncer.pg, syncer.schema))
		after, err = store.UsageChangeRevision(ctx)
		require.NoError(t, err)
		assert.Positive(t, after, "serve setup installs the journal with repair=%t", repair)
	}
	// The ordinary push fast path upgrades an existing schema too.
	_, err = syncer.pg.ExecContext(ctx, dropUsageChangeJournalSQL)
	require.NoError(t, err)
	require.True(t, pushSchemaCurrent(ctx, syncer.pg))
	syncer.schemaDone = false
	require.NoError(t, syncer.EnsureSchema(ctx))
	after, err = store.UsageChangeRevision(ctx)
	require.NoError(t, err)
	assert.Greater(t, after, before)
}

func TestUsageChangeJournalNewerVersion(t *testing.T) {
	_, syncer, store := newUsageJournalFixture(t)
	ctx := t.Context()
	_, err := syncer.pg.ExecContext(ctx, dropUsageChangeJournalSQL)
	require.NoError(t, err)
	_, err = syncer.pg.ExecContext(ctx, `INSERT INTO sync_metadata(key,value) VALUES('usage_change_journal_version','2')`)
	require.NoError(t, err)
	ensureUsageChangeJournalPG(ctx, syncer.pg)
	// Optional setup swallows installation errors. Check the installer directly
	// so a duplicate version key cannot disguise a missing version guard.
	tx, err := syncer.pg.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	require.NoError(t, installUsageChangeJournalPG(ctx, tx))
	require.NoError(t, tx.Commit())
	var exists bool
	require.NoError(t, syncer.pg.QueryRowContext(ctx, `SELECT to_regclass('usage_session_changes') IS NOT NULL`).Scan(&exists))
	assert.False(t, exists, "a newer journal version must remain untouched")
	through, ids, err := store.LoadUsageChanges(ctx, 0)
	require.NoError(t, err)
	assert.Zero(t, through)
	assert.Empty(t, ids)
}

func TestUsageChangeJournalRestrictedRole(t *testing.T) {
	local, syncer, store := newUsageJournalFixture(t)
	ctx := t.Context()
	roleURL := newUsageJournalRole(t, syncer, `GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA `+syncer.schema+` TO ROLE;
 GRANT USAGE ON ALL SEQUENCES IN SCHEMA `+syncer.schema+` TO ROLE`)
	// The role already reads sessions when the owner installs the journal.
	_, err := syncer.pg.ExecContext(ctx, dropUsageChangeJournalSQL)
	require.NoError(t, err)
	ensureUsageChangeJournalPG(ctx, syncer.pg)
	publisher, err := New(ctx, roleURL, syncer.schema, local, "test-machine", true, storage.PusherOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, publisher.Close()) })
	restricted, err := NewStore(ctx, roleURL, syncer.schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restricted.Close()) })
	require.NoError(t, local.UpsertSession(ctx, db.Session{ID: "restricted", Machine: "test-machine", Project: "project-a", Agent: "codex"}))
	before, err := store.UsageChangeRevision(ctx)
	require.NoError(t, err)
	result, err := publisher.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Empty(t, result.Errors)
	through, ids, err := restricted.LoadUsageChanges(ctx, before)
	require.NoError(t, err)
	assert.Greater(t, through, before)
	assert.Equal(t, []string{"restricted"}, ids)
	// A publisher without CREATE can continue on a schema lacking the journal.
	_, err = syncer.pg.ExecContext(ctx, dropUsageChangeJournalSQL)
	require.NoError(t, err)
	publisher.schemaDone = false
	require.NoError(t, publisher.EnsureSchema(ctx))
	missing, err := store.UsageChangeRevision(ctx)
	require.NoError(t, err)
	assert.Zero(t, missing)
}

func TestUsageChangeJournalLateGrantedReader(t *testing.T) {
	_, syncer, _ := newUsageJournalFixture(t)
	ctx := t.Context()
	// Granted after installation, so the journal itself stays unreadable.
	reader, err := NewStore(ctx, newUsageJournalRole(t, syncer, `GRANT SELECT ON sessions, sync_metadata TO ROLE`), syncer.schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	through, ids, err := reader.LoadUsageChanges(ctx, 0)
	require.NoError(t, err)
	assert.Zero(t, through)
	assert.Empty(t, ids)
}
