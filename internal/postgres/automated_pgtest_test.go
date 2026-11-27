//go:build pgtest

package postgres

import (
	"context"
	"fmt"
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
	t.Parallel()
	pgURL := testPGSchemaURL(t, "agentsview")

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
	t.Parallel()
	pgURL := testPGSchemaURL(t, "agentsview")
	local := testDB(t)
	ps, err := New(t.Context(), pgURL, "agentsview", local, "test-machine", true, storage.PusherOptions{})
	require.NoError(t, err)
	defer ps.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	require.NoError(t, ps.EnsureSchema(ctx))
	for _, tc := range []struct {
		id, agent, entrypoint, prompt, kind, relationship string
		parent, parserParent                              *string
		userCount                                         int
	}{
		{"plain", "codex", "", "Explain this function.", "non-interactive", "", nil, nil, 1},
		{"script", "codex", "", "You are a code reviewer. Review this change.", "non-interactive", "subagent", nil, nil, 1},
		{"claude-panel", "claude", "sdk-cli", "You are a code reviewer. Review this change.", "", "subagent", nil, nil, 1},
		{"legacy-claude-panel", "claude", "sdk-cli", "You are a code reviewer. Review this change.", "", "subagent", nil, nil, 1},
		{"origin~agent-child", "claude", "sdk-cli", "You are a code reviewer. Review this change.", "", "subagent", nil, nil, 1},
		{"claude-roborev", "claude", "roborev", "# UX review\nCheck the CLI output.", "", "", nil, nil, 2},
		{"real-child", "claude", "sdk-cli", "You are a code reviewer. Review this change.", "", "subagent", nil, new("plain"), 1},
		{"child", "codex", "", "Explain this function.", "non-interactive", "", new("plain"), new("plain"), 1},
		{"fork", "codex", "", "Explain this function.", "non-interactive", "fork", nil, nil, 1},
		{"review", "codex", "", "Explain this function.", "roborev", "", nil, nil, 1},
	} {
		_, err = ps.DB().ExecContext(ctx, `INSERT INTO sessions (
			id, machine, project, agent, entrypoint, first_message, session_kind, relationship_type,
			parent_session_id, parser_parent_session_id, user_message_count, is_automated, updated_at
		) VALUES ($1, 'host', 'project-a', $2, $3, $4, $5, $6, $7, $8, $9, true, '2000-01-01T00:00:00Z')`,
			tc.id, tc.agent, tc.entrypoint, tc.prompt, tc.kind, tc.relationship, tc.parent, tc.parserParent, tc.userCount)
		require.NoError(t, err)
	}
	// Current pushers send the parser provenance; older ones leave it NULL
	// (tested by legacy-claude-panel and origin~agent-child).
	_, err = ps.DB().ExecContext(ctx, `UPDATE sessions
		SET parser_relationship_type = 'subagent', relationship_inferred = TRUE
		WHERE id = 'claude-panel'`)
	require.NoError(t, err)
	require.NoError(t, backfillIsAutomatedPG(ctx, ps.DB()))
	for _, tc := range []struct {
		id, relationship string
		automated        bool
		bumped           bool
	}{
		{"plain", "subagent", false, true},
		{"script", "", true, true},
		{"claude-panel", "", true, true},
		{"legacy-claude-panel", "", true, true},
		{"origin~agent-child", "subagent", true, false},
		{"claude-roborev", "", true, false},
		{"real-child", "subagent", true, false},
		{"child", "", false, false},
		{"fork", "fork", false, false},
		{"review", "", true, false},
	} {
		var relationship string
		var automated bool
		var updated time.Time
		require.NoError(t, ps.DB().QueryRowContext(ctx, `SELECT relationship_type, is_automated, updated_at FROM sessions WHERE id = $1`, tc.id).Scan(&relationship, &automated, &updated))
		assert.Equal(t, tc.relationship, relationship, tc.id)
		assert.Equal(t, tc.automated, automated, tc.id)
		if tc.bumped {
			assert.True(t, updated.After(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)), tc.id)
		}
	}
	progress, err := backfillIsAutomatedPGWithProgress(ctx, ps.DB())
	require.NoError(t, err)
	assert.Zero(t, progress.RowsPrefetched, "reconciled rows stay stamped for incremental audits")
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
	pgURL := testPGSchemaURL(t, "agentsview")

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
	t.Parallel()
	pgURL := testPGSchemaURL(t, "agentsview")
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
	t.Parallel()
	pgURL := testPGSchemaURL(t, "agentsview")
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
	result, err := ps.pushBatch(t.Context(), []db.Session{*sess}, true, marker, nil, nil, nil, &pushed)
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
			url := testPGSchemaURL(t, "agentsview")
			if !takeFreshSchemaCopy(t, url, "agentsview") {
				cleanPGSchema(t, url)
			}
			t.Cleanup(func() { db.SetUserAutomationPrefixes(nil) })
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
	t.Parallel()
	pgURL := testPGSchemaURL(t, "agentsview")
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
	ps, err := New(t.Context(), pgURL, "agentsview", local, "usage-audit-machine", true, storage.PusherOptions{})
	require.NoError(t, err)
	defer ps.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	require.NoError(t, ps.EnsureSchema(ctx))
	_, err = ps.Push(ctx, false, nil)
	require.NoError(t, err)
	var before bool
	require.NoError(t, ps.DB().QueryRowContext(ctx,
		`SELECT is_automated FROM sessions WHERE id = 'automated'`,
	).Scan(&before))
	require.True(t, before)

	// Full-content rows lacking evidence must still have stale flags corrected.
	_, err = ps.DB().ExecContext(ctx, `INSERT INTO sessions (id, machine, project, agent, user_message_count, is_automated) VALUES ('empty-full', 'other-machine', 'project', 'claude', 1, true)`)
	require.NoError(t, err)
	// Legacy headless rows predate non-interactive session metadata.
	_, err = ps.DB().ExecContext(ctx, `INSERT INTO sessions (id, machine, project, agent, session_kind, entrypoint, user_message_count, is_automated, prompt_evidence_discarded) VALUES
		('codex-headless', 'other-machine', 'project', 'codex', 'headless', '', 2, true, true),
		('codex-interactive', 'other-machine', 'project', 'codex', '', '', 2, false, true),
		('claude-roborev-discarded', 'other-machine', 'project', 'claude', '', 'roborev', 2, false, true),
		('headless-full', 'other-machine', 'project', 'codex', 'headless', '', 2, true, false)`)
	require.NoError(t, err)
	require.NoError(t, EnsureSchema(ctx, ps.DB(), "agentsview"))
	for id, want := range map[string]bool{
		"automated":                true,
		"interactive":              false,
		"empty-full":               false,
		"codex-headless":           true,
		"codex-interactive":        false,
		"claude-roborev-discarded": true,
		"headless-full":            false,
	} {
		var got bool
		require.NoError(t, ps.DB().QueryRowContext(ctx,
			`SELECT is_automated FROM sessions WHERE id = $1`, id,
		).Scan(&got))
		assert.Equal(t, want, got, id)
	}
}

func TestAutomationAuditPGRestoresClaudeSDKWorker(t *testing.T) {
	t.Cleanup(func() { db.SetUserAutomationPrefixes(nil) })
	pgURL := testPGSchemaURL(t, "agentsview")
	local := testDB(t)
	// The pusher has no rule for this prompt, so the run stays a subagent.
	require.NoError(t, local.UpsertSession(t.Context(), db.Session{
		ID: "panel", Machine: "local", Project: "project", Agent: "claude",
		Entrypoint: "sdk-cli", RelationshipType: "subagent",
		FirstMessage: new("# UX review\nCheck the CLI output."), UserMessageCount: 1, MessageCount: 1,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}))
	ps, err := New(t.Context(), pgURL, "agentsview", local, "panel-machine", true, storage.PusherOptions{})
	require.NoError(t, err)
	defer ps.Close()
	require.NoError(t, ps.EnsureSchema(t.Context()))
	_, err = ps.Push(t.Context(), false, nil)
	require.NoError(t, err)
	stored := func() (string, bool) {
		t.Helper()
		var relationship string
		var automated bool
		require.NoError(t, ps.DB().QueryRowContext(t.Context(),
			`SELECT relationship_type, is_automated FROM sessions WHERE id = 'panel'`,
		).Scan(&relationship, &automated))
		return relationship, automated
	}

	// A server audit with a matching rule counts the run as Automated.
	db.SetUserAutomationPrefixes([]string{"# UX review"})
	require.NoError(t, backfillIsAutomatedPG(t.Context(), ps.DB()))
	relationship, automated := stored()
	assert.True(t, automated)
	assert.Empty(t, relationship)

	// Without the rule, the audit restores the parser's subagent role.
	db.SetUserAutomationPrefixes(nil)
	require.NoError(t, backfillIsAutomatedPG(t.Context(), ps.DB()))
	relationship, automated = stored()
	assert.False(t, automated)
	assert.Equal(t, "subagent", relationship)

	// Legacy sdk-cli rows (NULL parser_relationship_type) also clear and restore.
	_, err = ps.DB().ExecContext(t.Context(), `INSERT INTO sessions
		(id, machine, project, agent, entrypoint, relationship_type, first_message, user_message_count)
		VALUES ('legacy-panel', 'host', 'project', 'claude', 'sdk-cli', 'subagent', '# UX review\nCheck the CLI output.', 1)`)
	require.NoError(t, err)
	db.SetUserAutomationPrefixes([]string{"# UX review"})
	require.NoError(t, backfillIsAutomatedPG(t.Context(), ps.DB()))
	require.NoError(t, ps.DB().QueryRowContext(t.Context(),
		`SELECT relationship_type, is_automated FROM sessions WHERE id = 'legacy-panel'`,
	).Scan(&relationship, &automated))
	assert.True(t, automated)
	assert.Empty(t, relationship)

	db.SetUserAutomationPrefixes(nil)
	require.NoError(t, backfillIsAutomatedPG(t.Context(), ps.DB()))
	require.NoError(t, ps.DB().QueryRowContext(t.Context(),
		`SELECT relationship_type, is_automated FROM sessions WHERE id = 'legacy-panel'`,
	).Scan(&relationship, &automated))
	assert.False(t, automated)
	assert.Equal(t, "subagent", relationship)
}

func TestAutomationAuditPGKeepsWaitingLauncherLinkAutomated(t *testing.T) {
	pgURL := testPGSchemaURL(t, "agentsview")
	local := testDB(t)
	require.NoError(t, local.UpsertSession(t.Context(), db.Session{
		ID: "review", Machine: "local", Project: "project", Agent: "codex",
		SessionKind:  "non-interactive",
		FirstMessage: new("You are a code reviewer. Review this change."), UserMessageCount: 1, MessageCount: 1,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}))
	// The launcher's session is not in the archive, so the link waits.
	link, err := local.SetSessionExternalParent(t.Context(), "review", "missing-launcher")
	require.NoError(t, err)
	require.False(t, link.Changed)
	sqliteRow, err := local.GetSessionFull(t.Context(), "review")
	require.NoError(t, err)
	require.True(t, sqliteRow.IsAutomated)
	require.Empty(t, sqliteRow.RelationshipType)
	ps, err := New(t.Context(), pgURL, "agentsview", local, "review-machine", true, storage.PusherOptions{})
	require.NoError(t, err)
	defer ps.Close()
	require.NoError(t, ps.EnsureSchema(t.Context()))
	_, err = ps.Push(t.Context(), false, nil)
	require.NoError(t, err)

	require.NoError(t, backfillIsAutomatedPG(t.Context(), ps.DB()))
	var relationship, externalParent string
	var automated bool
	require.NoError(t, ps.DB().QueryRowContext(t.Context(),
		`SELECT relationship_type, is_automated, COALESCE(external_parent_session_id, '')
		 FROM sessions WHERE id = 'review'`,
	).Scan(&relationship, &automated, &externalParent))
	require.Equal(t, "missing-launcher", externalParent)
	assert.True(t, automated)
	assert.Empty(t, relationship, "SQLite counts a run with a waiting launcher link as Automated")
}

// reconcileClearTracer runs after when the audit's statement that clears
// automated parentless workers finishes.
type reconcileClearTracer struct{ after func() }

func (tracer *reconcileClearTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "SET relationship_type = ''") {
		return context.WithValue(ctx, tracer, true)
	}
	return ctx
}

func (tracer *reconcileClearTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if ctx.Value(tracer) == true && tracer.after != nil {
		after := tracer.after
		tracer.after = nil
		after()
	}
}

func TestAutomationAuditPGLeavesConcurrentPushUnstamped(t *testing.T) {
	t.Parallel()
	url := testPGSchemaURL(t, "agentsview")
	ps, err := New(t.Context(), url, "agentsview", testDB(t), "host", true, storage.PusherOptions{})
	require.NoError(t, err)
	defer ps.Close()
	require.NoError(t, ps.EnsureSchema(t.Context()))
	_, err = ps.DB().ExecContext(t.Context(), `INSERT INTO sessions
		(id, machine, project, agent, session_kind, relationship_type, first_message, user_message_count)
		VALUES ('race', 'host', 'project', 'codex', 'non-interactive', 'subagent', 'You are a code reviewer. Review this change.', 1)`)
	require.NoError(t, err)
	cfg, err := pgx.ParseConfig(url)
	require.NoError(t, err)
	cfg.RuntimeParams["search_path"] = "agentsview"
	// A push lands between the audit's clear and its stamp, replacing the
	// prompt the audit classified.
	tracer := &reconcileClearTracer{after: func() {
		_, err := ps.DB().ExecContext(t.Context(), `UPDATE sessions
			SET first_message = 'Explain this function.', updated_at = NOW() WHERE id = 'race'`)
		require.NoError(t, err)
	}}
	cfg.Tracer = tracer
	audit := stdlib.OpenDB(*cfg)
	defer audit.Close()
	require.NoError(t, backfillIsAutomatedPG(t.Context(), audit))
	require.Nil(t, tracer.after, "the audit cleared the worker")

	var hash *string
	require.NoError(t, ps.DB().QueryRowContext(t.Context(),
		`SELECT automation_audit_hash FROM sessions WHERE id = 'race'`).Scan(&hash))
	assert.Nil(t, hash, "the pushed version was never audited")
	progress, err := backfillIsAutomatedPGWithProgress(t.Context(), audit)
	require.NoError(t, err)
	assert.Equal(t, 1, progress.RowsPrefetched)
	var relationship string
	var automated bool
	require.NoError(t, ps.DB().QueryRowContext(t.Context(),
		`SELECT relationship_type, is_automated FROM sessions WHERE id = 'race'`).Scan(&relationship, &automated))
	assert.False(t, automated)
	assert.Equal(t, "subagent", relationship)
}
