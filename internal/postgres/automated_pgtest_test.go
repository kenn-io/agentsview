//go:build pgtest

package postgres

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

func TestBackfillIsAutomatedPGUsesBoundedEvidence(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(
		t.Context(), pgURL, "agentsview", local,
		"automated-audit-machine", true,
		storage.PusherOptions{},
	)
	require.NoError(t, err, "creating sync")
	defer ps.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	require.NoError(t, ps.EnsureSchema(ctx), "ensure schema")

	largePrefix := "You are a code reviewer." + strings.Repeat("x", 4<<10)
	lateSubstring := strings.Repeat("z", 2<<10) +
		" invoked by roborev to perform this review"
	multibyteLateSubstring := strings.Repeat("日\\", 450) +
		" invoked by roborev to perform this review"
	largeHuman := strings.Repeat("h", 2<<10)
	rows := []struct {
		id               string
		firstMessage     string
		userMessageCount int
		isAutomated      bool
	}{
		{id: "prefix-first-message", firstMessage: largePrefix, userMessageCount: 1},
		{id: "prefix-first-user", firstMessage: "Generated title", userMessageCount: 1},
		{id: "late-substring", firstMessage: lateSubstring, userMessageCount: 1},
		{
			id: "multibyte-late-substring", firstMessage: multibyteLateSubstring,
			userMessageCount: 1,
		},
		{id: "unicode-exact", firstMessage: "\u2003Warmup\u00a0", userMessageCount: 1},
		{
			id: "stale-multi-turn", firstMessage: "# Fix Request for login flow",
			userMessageCount: 2, isAutomated: true,
		},
		{id: "large-human", firstMessage: largeHuman, userMessageCount: 1},
	}
	for _, row := range rows {
		_, err := ps.DB().ExecContext(ctx,
			`INSERT INTO sessions (
				id, machine, project, agent, first_message,
				user_message_count, is_automated
			 ) VALUES ($1, 'host', 'proj', 'codex', $2, $3, $4)`,
			row.id, row.firstMessage, row.userMessageCount, row.isAutomated,
		)
		require.NoError(t, err, "insert %s", row.id)
	}
	_, err = ps.DB().ExecContext(ctx,
		`INSERT INTO messages (session_id, ordinal, role, content)
		 VALUES ($1, 1, 'user', $2)`,
		"prefix-first-user", largePrefix,
	)
	require.NoError(t, err, "insert prefix-matching first user message")
	_, err = ps.DB().ExecContext(ctx,
		`INSERT INTO messages (session_id, ordinal, role, content, source_subtype)
		 VALUES ($1, 0, 'user', 'Finished successfully', 'tool_result')`,
		"prefix-first-user",
	)
	require.NoError(t, err, "insert orphan result before the first prompt")

	progress, err := backfillIsAutomatedPGWithProgress(ctx, ps.DB())
	require.NoError(t, err, "bounded automation audit")
	assert.Equal(t, len(rows), progress.RowsPrefetched)
	assert.Equal(t, 3, progress.RowsFullText,
		"only truncated non-matches should require complete text")

	for id, want := range map[string]bool{
		"prefix-first-message":     true,
		"prefix-first-user":        true,
		"late-substring":           true,
		"multibyte-late-substring": true,
		"unicode-exact":            true,
		"stale-multi-turn":         false,
		"large-human":              false,
	} {
		var got bool
		require.NoError(t, ps.DB().QueryRowContext(ctx,
			`SELECT is_automated FROM sessions WHERE id = $1`, id,
		).Scan(&got), "query %s", id)
		assert.Equal(t, want, got, id)
	}
}

func TestHeadlessClassificationRepairPG(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })
	local := testDB(t)
	ps, err := New(t.Context(), pgURL, "agentsview", local, "test-machine", true, storage.PusherOptions{})
	require.NoError(t, err)
	defer ps.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	require.NoError(t, ps.EnsureSchema(ctx))
	for _, tc := range []struct {
		id, prompt, kind, relationship string
		parent                         *string
	}{
		{"plain", "Explain this function.", "non-interactive", "", nil},
		{"script", "You are a code reviewer. Review this change.", "non-interactive", "", nil},
		{"child", "Explain this function.", "non-interactive", "", new("plain")},
		{"fork", "Explain this function.", "non-interactive", "fork", nil},
		{"review", "Explain this function.", "roborev", "", nil},
	} {
		_, err = ps.DB().ExecContext(ctx, `INSERT INTO sessions (
			id, machine, project, agent, first_message, session_kind, relationship_type,
			parent_session_id, user_message_count, is_automated, updated_at
		) VALUES ($1, 'host', 'project-a', 'codex', $2, $3, $4, $5, 1, true, '2000-01-01T00:00:00Z')`,
			tc.id, tc.prompt, tc.kind, tc.relationship, tc.parent)
		require.NoError(t, err)
	}
	require.NoError(t, backfillIsAutomatedPG(ctx, ps.DB()))
	for _, tc := range []struct {
		id, relationship string
		automated        bool
	}{
		{"plain", "subagent", false},
		{"script", "subagent", true},
		{"child", "", false},
		{"fork", "fork", false},
		{"review", "", true},
	} {
		var relationship string
		var automated bool
		var updated time.Time
		require.NoError(t, ps.DB().QueryRowContext(ctx, `SELECT relationship_type, is_automated, updated_at FROM sessions WHERE id = $1`, tc.id).Scan(&relationship, &automated, &updated))
		assert.Equal(t, tc.relationship, relationship, tc.id)
		assert.Equal(t, tc.automated, automated, tc.id)
		if tc.relationship == "subagent" {
			assert.True(t, updated.After(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)))
		}
	}
	_, err = ps.DB().ExecContext(ctx, `UPDATE sessions SET relationship_type = '' WHERE id = 'plain'`)
	require.NoError(t, err)
	require.NoError(t, backfillIsAutomatedPG(ctx, ps.DB()))
	var relationship string
	require.NoError(t, ps.DB().QueryRowContext(ctx, `SELECT relationship_type FROM sessions WHERE id = 'plain'`).Scan(&relationship))
	assert.Equal(t, "subagent", relationship, "every audit repairs late arrivals")
	tx, err := ps.DB().BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	require.NoError(t, ps.pushSession(ctx, tx, db.Session{
		ID: "late-worker", Project: "project-a", Agent: "codex", SessionKind: "non-interactive",
		CreatedAt: "2026-10-01T10:00:00Z",
	}, "test-marker", nil))
	require.NoError(t, tx.Commit())
	require.NoError(t, ps.DB().QueryRowContext(ctx, `SELECT relationship_type FROM sessions WHERE id = 'late-worker'`).Scan(&relationship))
	assert.Equal(t, "subagent", relationship)
}

// TestPushSessionTrustsLocalIsAutomated verifies that
// pushSession copies sess.IsAutomated verbatim instead of
// re-running db.IsAutomatedSession on the first_message.
// Achieved by setting a user prefix locally, upserting a
// matching session (so IsAutomated=1), then clearing the
// user prefix BEFORE push and confirming the PG row stays
// IsAutomated=1.
func TestPushSessionTrustsLocalIsAutomated(t *testing.T) {
	t.Cleanup(func() { db.SetUserAutomationPrefixes(nil) })
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)

	// Set a user prefix BEFORE inserting so UpsertSession
	// sets is_automated=1 on the SQLite row.
	db.SetUserAutomationPrefixes([]string{"You are analyzing an essay"})
	fm := "You are analyzing an essay about epistemology."
	require.NoError(t, local.UpsertSession(t.Context(), db.Session{
		ID:               "essay-1",
		Project:          "proj",
		Machine:          "local",
		Agent:            "claude",
		FirstMessage:     &fm,
		MessageCount:     2,
		UserMessageCount: 1,
		CreatedAt:        time.Now().UTC().Format(time.RFC3339Nano),
	}), "upsert")

	// Clear the user prefix so a recompute in pushSession
	// would now classify this row as is_automated=0. If
	// pushSession trusts the local value, PG sees =1 anyway.
	db.SetUserAutomationPrefixes(nil)

	ps, err := New(
		t.Context(), pgURL, "agentsview", local,
		"trust-test-machine", true,
		storage.PusherOptions{},
	)
	require.NoError(t, err, "creating sync")
	defer ps.Close()

	ctx, cancel := context.WithTimeout(
		t.Context(), 30*time.Second,
	)
	defer cancel()

	require.NoError(t, ps.EnsureSchema(ctx), "ensure schema")
	_, err = ps.Push(ctx, false, nil)
	require.NoError(t, err, "push")

	var got bool
	require.NoError(t, ps.DB().QueryRowContext(ctx,
		`SELECT is_automated FROM sessions WHERE id = $1`,
		"essay-1",
	).Scan(&got), "query pg")
	assert.True(t, got,
		"pushSession recomputed is_automated; expected to trust local value")
}

func TestAutomationAuditPGIncremental(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })
	local := testDB(t)
	ps, err := New(t.Context(), pgURL, "agentsview", local, "audit-machine", true, storage.PusherOptions{})
	require.NoError(t, err)
	defer ps.Close()
	require.NoError(t, ps.EnsureSchema(t.Context()))
	_, err = ps.DB().ExecContext(t.Context(), `INSERT INTO sessions (id, machine, project, agent, first_message, user_message_count)
	 VALUES ('audit-a', 'host', 'project', 'codex', 'Explain this function.', 1),
	 ('audit-b', 'host', 'project', 'codex', 'Explain this function.', 1)`)
	require.NoError(t, err)
	progress, err := backfillIsAutomatedPGWithProgress(t.Context(), ps.DB())
	require.NoError(t, err)
	assert.Equal(t, 2, progress.RowsPrefetched)
	progress, err = backfillIsAutomatedPGWithProgress(t.Context(), ps.DB())
	require.NoError(t, err)
	assert.Zero(t, progress.RowsPrefetched, "an unchanged archive has no audit candidates")
	for _, change := range []string{
		`first_message = 'You are a code reviewer. Review this change.'`,
		`user_message_count = 2`,
		`session_kind = 'roborev'`,
		`is_automated = FALSE`,
		`transcript_revision = 'next'`,
		`prompt_evidence_discarded = TRUE`,
	} {
		_, err = ps.DB().ExecContext(t.Context(), `UPDATE sessions SET `+change+` WHERE id = 'audit-a'`)
		require.NoError(t, err)
		progress, err = backfillIsAutomatedPGWithProgress(t.Context(), ps.DB())
		require.NoError(t, err)
		assert.Equal(t, 1, progress.RowsPrefetched, change)
		progress, err = backfillIsAutomatedPGWithProgress(t.Context(), ps.DB())
		require.NoError(t, err)
		assert.Zero(t, progress.RowsPrefetched, change)
	}
	_, err = ps.DB().ExecContext(t.Context(), `UPDATE sessions SET first_message = first_message, project = 'renamed' WHERE id = 'audit-b'`)
	require.NoError(t, err)
	progress, err = backfillIsAutomatedPGWithProgress(t.Context(), ps.DB())
	require.NoError(t, err)
	assert.Zero(t, progress.RowsPrefetched, "unchanged evidence stays clean")

	_, err = ps.DB().ExecContext(t.Context(), `DELETE FROM sync_metadata WHERE key = $1`, db.ClassifierHashKey)
	require.NoError(t, err)
	progress, err = backfillIsAutomatedPGWithProgress(t.Context(), ps.DB())
	require.NoError(t, err)
	assert.Equal(t, 2, progress.RowsPrefetched, "explicit classifier rebuild resets all stamps")
}

func TestAutomationAuditPGMessageOnlyPush(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })
	local := testDB(t)
	ps, err := New(t.Context(), pgURL, "agentsview", local, "audit-machine", true, storage.PusherOptions{})
	require.NoError(t, err)
	defer ps.Close()
	require.NoError(t, ps.EnsureSchema(t.Context()))
	require.NoError(t, local.UpsertSession(t.Context(), db.Session{ID: "message-only", Machine: "local", Project: "project", Agent: "codex", FirstMessage: new("Generated title"), UserMessageCount: 1, MessageCount: 1}))
	require.NoError(t, local.InsertMessages(t.Context(), []db.Message{{SessionID: "message-only", Role: "user", Content: "Explain this function."}}))
	_, err = ps.Push(t.Context(), true, nil)
	require.NoError(t, err)
	require.NoError(t, backfillIsAutomatedPG(t.Context(), ps.DB()))
	sess, err := local.GetSessionFull(t.Context(), "message-only")
	require.NoError(t, err)
	require.NotNil(t, sess)
	var revision string
	require.NoError(t, ps.DB().QueryRowContext(t.Context(), `SELECT transcript_revision FROM sessions WHERE id='message-only'`).Scan(&revision))
	// Push loads session metadata before opening its message snapshot.
	// Local sync can replace messages between those reads.
	require.NoError(t, local.ReplaceSessionMessages(t.Context(), "message-only", []db.Message{{SessionID: "message-only", Role: "user", Content: "Warmup"}}))
	marker, err := ps.pushMarkerID(t.Context())
	require.NoError(t, err)
	var pushed []db.Session
	result, err := ps.pushBatch(t.Context(), []db.Session{*sess}, true, marker, nil, nil, &pushed)
	require.NoError(t, err)
	require.True(t, result.ok)
	var currentRevision string
	require.NoError(t, ps.DB().QueryRowContext(t.Context(), `SELECT transcript_revision FROM sessions WHERE id='message-only'`).Scan(&currentRevision))
	assert.Equal(t, revision, currentRevision)
	progress, err := backfillIsAutomatedPGWithProgress(t.Context(), ps.DB())
	require.NoError(t, err)
	assert.Equal(t, 1, progress.RowsPrefetched)
	var automated bool
	require.NoError(t, ps.DB().QueryRowContext(t.Context(), `SELECT is_automated FROM sessions WHERE id='message-only'`).Scan(&automated))
	assert.True(t, automated, "updated_at invalidates the replaced prompt even with an unchanged revision")
}

func TestAutomationAuditPGConcurrentClassifiers(t *testing.T) {
	for _, older := range []bool{false, true} {
		t.Run(fmt.Sprint("older=", older), func(t *testing.T) {
			url := testPGURL(t)
			cleanPGSchema(t, url)
			t.Cleanup(func() { cleanPGSchema(t, url); db.SetUserAutomationPrefixes(nil) })
			ps, err := New(t.Context(), url, "agentsview", testDB(t), "host", true, storage.PusherOptions{})
			require.NoError(t, err)
			defer ps.Close()
			require.NoError(t, ps.EnsureSchema(t.Context()))
			_, err = ps.DB().ExecContext(t.Context(), `INSERT INTO sessions(id,machine,project,agent,first_message,user_message_count) VALUES ('concurrent','host','project','codex','audit-prefix job',1)`)
			require.NoError(t, err)
			oldHash := db.ClassifierHash()
			cfg, err := pgx.ParseConfig(url)
			require.NoError(t, err)
			cfg.RuntimeParams["search_path"] = "agentsview"
			tracer := &hostedPendingSelectionTracer{argument: "concurrent"}
			cfg.Tracer = tracer
			auditA := stdlib.OpenDB(*cfg)
			defer auditA.Close()
			db.SetUserAutomationPrefixes([]string{"audit-prefix"})
			if !older {
				tracer.after = func() {
					tracer.after = nil
					db.SetUserAutomationPrefixes(nil)
					require.NoError(t, backfillIsAutomatedPG(t.Context(), ps.DB()))
					db.SetUserAutomationPrefixes([]string{"audit-prefix"})
				}
			}
			require.NoError(t, backfillIsAutomatedPG(t.Context(), auditA))
			if older {
				// An older publisher finishes its hash write after computing no verdict changes.
				_, err = ps.DB().ExecContext(t.Context(), `INSERT INTO sync_metadata(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value`, db.ClassifierHashKey, oldHash)
				require.NoError(t, err)
				db.SetUserAutomationPrefixes(nil)
			}
			progress, err := backfillIsAutomatedPGWithProgress(t.Context(), auditA)
			require.NoError(t, err)
			assert.Equal(t, 1, progress.RowsPrefetched, "a different per-row classifier remains stale despite the global hash")
			var automated bool
			require.NoError(t, ps.DB().QueryRowContext(t.Context(), `SELECT is_automated FROM sessions WHERE id='concurrent'`).Scan(&automated))
			assert.Equal(t, !older, automated)
			progress, err = backfillIsAutomatedPGWithProgress(t.Context(), auditA)
			require.NoError(t, err)
			assert.Zero(t, progress.RowsPrefetched)
		})
	}
}

func TestBackfillIsAutomatedPGPreservesUsageOnlyClassification(t *testing.T) {
	pgURL := testPGURL(t)
	const schema = "agentsview_raw_privilege_test"
	const role = "agentsview_raw_restricted"
	const rolePassword = "agentsview_raw_restricted_pw"

	admin, err := Open(t.Context(), pgURL, schema, true)
	require.NoError(t, err, "Open admin")
	t.Cleanup(func() { _ = admin.Close() })
	_, err = admin.ExecContext(t.Context(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
	require.NoError(t, err, "drop schema")
	require.NoError(t, EnsureSchema(t.Context(), admin, schema))
	require.True(t, pushSchemaCurrent(t.Context(), admin),
		"fixture must exercise the schema-current sync fast path")

	local := testDB(t)
	local.SetArchiveContent(config.ArchiveContentUsage)
	for _, tc := range []struct {
		id, prompt string
	}{
		{"automated", "You are a code reviewer. Review this change."},
		{"interactive", "Explain this function."},
	} {
		require.NoError(t, local.UpsertSession(t.Context(), db.Session{
			ID: tc.id, Project: "project", Agent: "claude", Machine: "local",
			FirstMessage: &tc.prompt, UserMessageCount: 1, MessageCount: 2,
			CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		}))
		require.NoError(t, local.InsertMessages(t.Context(), []db.Message{
			{SessionID: tc.id, Ordinal: 0, Role: "user", Content: tc.prompt},
			{SessionID: tc.id, Ordinal: 1, Role: "assistant", Content: "Finished.", Model: "model-a"},
		}))
	}
	ps, err := New(t.Context(), pgURL, schema, local, "usage-audit-machine", true, storage.PusherOptions{})
	require.NoError(t, err)
	defer ps.Close()
	_, err = ps.Push(t.Context(), false, nil)
	require.NoError(t, err)
	var before bool
	require.NoError(t, admin.QueryRowContext(t.Context(), `SELECT is_automated FROM sessions WHERE id = 'automated'`).Scan(&before))
	require.True(t, before)
	// Full-content rows lacking evidence must still have stale flags corrected.
	_, err = admin.ExecContext(t.Context(), `INSERT INTO sessions (id, machine, project, agent, user_message_count, is_automated) VALUES ('empty-full', 'other-machine', 'project', 'claude', 1, true)`)
	require.NoError(t, err)
	// Legacy headless rows predate non-interactive session metadata.
	_, err = admin.ExecContext(t.Context(), `INSERT INTO sessions (id, machine, project, agent, session_kind, user_message_count, is_automated, prompt_evidence_discarded) VALUES
		('codex-headless', 'other-machine', 'project', 'codex', 'headless', 2, true, true),
		('codex-interactive', 'other-machine', 'project', 'codex', '', 2, false, true),
		('headless-full', 'other-machine', 'project', 'codex', 'headless', 2, true, false)`)
	require.NoError(t, err)
	wantVerdicts := map[string]bool{"automated": true, "interactive": false, "empty-full": false, "codex-headless": true, "codex-interactive": false, "headless-full": false}
	t.Run("tracking", func(t *testing.T) {
		require.NoError(t, backfillIsAutomatedPG(t.Context(), admin))
		for id, want := range wantVerdicts {
			var got bool
			require.NoError(t, admin.QueryRowContext(t.Context(),
				`SELECT is_automated FROM sessions WHERE id = $1`, id,
			).Scan(&got))
			assert.Equal(t, want, got, id)
		}
	})

	_, _ = admin.ExecContext(t.Context(), `DROP OWNED BY `+role)
	_, _ = admin.ExecContext(t.Context(), `DROP ROLE IF EXISTS `+role)
	_, err = admin.ExecContext(t.Context(), `CREATE ROLE `+role+` LOGIN PASSWORD '`+rolePassword+`'`)
	require.NoError(t, err, "create restricted role")
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.WithoutCancel(t.Context()), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
		_, _ = admin.ExecContext(context.WithoutCancel(t.Context()), `DROP OWNED BY `+role)
		_, _ = admin.ExecContext(context.WithoutCancel(t.Context()), `DROP ROLE IF EXISTS `+role)
	})
	for _, grant := range []string{
		`GRANT USAGE ON SCHEMA ` + schema + ` TO ` + role,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA ` + schema + ` TO ` + role,
		`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA ` + schema + ` TO ` + role,
	} {
		_, err = admin.ExecContext(t.Context(), grant)
		require.NoError(t, err, grant)
	}

	restrictedURL, err := url.Parse(pgURL)
	require.NoError(t, err)
	restrictedURL.User = url.UserPassword(role, rolePassword)
	restricted, err := Open(t.Context(), restrictedURL.String(), schema, true)
	require.NoError(t, err, "Open restricted")
	t.Cleanup(func() { _ = restricted.Close() })

	indexes := []string{"idx_sessions_data_version", "idx_sessions_parentless_workers", "idx_sessions_source_missing"}
	for _, index := range indexes {
		_, err = admin.ExecContext(t.Context(), `DROP INDEX `+index)
		require.NoError(t, err)
	}
	_, err = admin.ExecContext(t.Context(), `DROP TRIGGER trg_sessions_automation_audit ON sessions;
 DROP INDEX idx_sessions_automation_audit; ALTER TABLE sessions DROP COLUMN automation_audit_hash;
 INSERT INTO sessions(id,machine,project,agent,first_message,user_message_count) VALUES
 ('restricted-stale','host','project','codex','Warmup',1), ('restricted-stable','host','project','codex','Explain this function.',1)`)
	require.NoError(t, err)
	for _, full := range []bool{false, true} {
		var progress automatedAuditPGProgress
		var verdicts []automationVerdictPG
		if full {
			verdicts, err = auditAutomatedFullPG(t.Context(), restricted, db.SnapshotAutomationClassifier(), &progress, "xmin")
		} else {
			verdicts, err = auditAutomatedMatchingHashPG(t.Context(), restricted, db.SnapshotAutomationClassifier(), &progress, "xmin", nil)
		}
		require.NoError(t, err)
		require.Len(t, verdicts, 1, "full=%t: unchanged fallback verdicts must be discarded before write-back", full)
		assert.Equal(t, "restricted-stale", verdicts[0].id)
	}
	t.Run("fallback", func(t *testing.T) {
		for _, hash := range []string{db.ClassifierHash(), "old-classifier"} {
			_, err = admin.ExecContext(t.Context(), `UPDATE sessions SET is_automated = TRUE WHERE id IN ('empty-full', 'headless-full')`)
			require.NoError(t, err)
			_, err = admin.ExecContext(t.Context(), `UPDATE sync_metadata SET value = $1 WHERE key = $2`, hash, db.ClassifierHashKey)
			require.NoError(t, err)
			require.NoError(t, backfillIsAutomatedPG(t.Context(), restricted))
			for id, want := range wantVerdicts {
				var got bool
				require.NoError(t, restricted.QueryRowContext(t.Context(), `SELECT is_automated FROM sessions WHERE id = $1`, id).Scan(&got))
				assert.Equal(t, want, got, "hash=%s id=%s", hash, id)
			}
		}
	})
	assert.True(t, pushSchemaCurrent(t.Context(), restricted), "tracking stays optional for existing push roles")
	syncer := &Sync{pg: restricted, schema: schema}
	require.NoError(t, syncer.EnsureSchema(t.Context()),
		"restricted preparation must tolerate index and raw custody DDL permissions")
	assert.True(t, syncer.schemaDone)
	for _, index := range indexes {
		assert.False(t, pgHasIndex(t.Context(), admin, index), "restricted publisher cannot install %s", index)
	}
	var automated bool
	require.NoError(t, restricted.QueryRowContext(t.Context(), `SELECT is_automated FROM sessions WHERE id='restricted-stale'`).Scan(&automated))
	assert.True(t, automated)
	require.NoError(t, EnsureSchema(t.Context(), admin, schema))
	progress, err := backfillIsAutomatedPGWithProgress(t.Context(), restricted)
	require.NoError(t, err)
	assert.Zero(t, progress.RowsPrefetched, "owner installation enables incremental restricted publication")
	writable, err := CanWriteRawSyncSchema(t.Context(), restricted, schema)
	require.NoError(t, err)
	assert.True(t, writable,
		"DML-capable role must remain eligible when raw custody DDL is restricted")
}
