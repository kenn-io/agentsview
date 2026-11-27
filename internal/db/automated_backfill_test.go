package db

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
)

func TestHeadlessClassificationRepairOnOpen(t *testing.T) {
	d := testDB(t)
	for _, tc := range []struct {
		id, agent, entrypoint, prompt, kind, relationship string
		parent                                            *string
	}{
		{"plain", "codex", "", "Explain this function.", "non-interactive", "", nil},
		{"script", "codex", "", "You are a code reviewer. Review this change.", "non-interactive", "", nil},
		{"claude-panel", "claude", "sdk-cli", "You are a code reviewer. Review this change.", "", "subagent", nil},
		{"child", "codex", "", "Explain this function.", "non-interactive", "", Ptr("plain")},
		{"fork", "codex", "", "Explain this function.", "non-interactive", "fork", nil},
		{"review", "codex", "", "Explain this function.", "roborev", "", nil},
		{"claude-roborev", "claude", "roborev", "# UX review\nCheck the CLI output.", "", "", nil},
	} {
		insertSession(t, d, tc.id, "project-a", func(s *Session) {
			s.Agent = tc.agent
			s.Entrypoint = tc.entrypoint
			s.FirstMessage = Ptr(tc.prompt)
			s.SessionKind = tc.kind
			s.RelationshipType = tc.relationship
			s.ParentSessionID = tc.parent
			s.UserMessageCount = 1
		})
	}
	_, err := d.getWriter().Exec(t.Context(), `DROP TRIGGER trg_sessions_classification_journal_update`)
	require.NoError(t, err)
	_, err = d.getWriter().Exec(t.Context(), `UPDATE sessions SET relationship_type = '' WHERE id = 'plain'`)
	require.NoError(t, err)
	_, err = d.getWriter().Exec(t.Context(), `UPDATE sessions SET relationship_type = 'subagent', parser_relationship_type = 'subagent' WHERE id IN ('script', 'claude-panel')`)
	require.NoError(t, err)
	_, err = d.getWriter().Exec(t.Context(), `UPDATE sessions SET is_automated = 1, local_modified_at = '2000-01-01T00:00:00.000Z'`)
	require.NoError(t, err)
	_, err = d.getWriter().Exec(t.Context(), `UPDATE sessions SET is_automated = 0 WHERE id = 'claude-roborev'`)
	require.NoError(t, err)
	_, err = d.getWriter().Exec(t.Context(), `UPDATE stats SET value = 'old-classifier' WHERE key = ?`, ClassifierHashKey)
	require.NoError(t, err)
	path := d.Path()
	require.NoError(t, d.Close())
	reopened, err := Open(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reopened.Close() })
	for _, tc := range []struct {
		id, relationship string
		automated        bool
		bumped           bool
	}{
		{"plain", "subagent", false, true},
		{"script", "", true, true},
		{"claude-panel", "", true, true},
		{"child", "", false, true},
		{"fork", "fork", false, true},
		{"review", "", true, false},
		{"claude-roborev", "", true, true},
	} {
		stored, err := reopened.GetSessionFull(t.Context(), tc.id)
		require.NoError(t, err)
		require.NotNil(t, stored)
		assert.Equal(t, tc.relationship, stored.RelationshipType, tc.id)
		assert.Equal(t, tc.automated, stored.IsAutomated, tc.id)
		if tc.bumped {
			require.NotNil(t, stored.LocalModifiedAt)
			assert.Greater(t, *stored.LocalModifiedAt, "2000-01-01T00:00:00.000Z", tc.id)
		}
	}

	revision, err := reopened.SessionDeletionPublicationRevision(t.Context())
	require.NoError(t, err)
	changes, err := reopened.LoadSessionDeletionChanges(t.Context(), 0, revision)
	require.NoError(t, err)
	assert.Contains(t, changes, "plain")
	assert.Contains(t, changes, "script")
	assert.Contains(t, changes, "claude-panel")
	assert.Contains(t, changes, "claude-roborev")
	_, err = reopened.getWriter().Exec(t.Context(), `UPDATE sessions SET relationship_type = '' WHERE id = 'plain'`)
	require.NoError(t, err)
	require.NoError(t, reopened.Close())
	reopened, err = Open(t.Context(), path)
	require.NoError(t, err)
	defer reopened.Close()
	stored, err := reopened.GetSession(t.Context(), "plain")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, "subagent", stored.RelationshipType, "matching-hash audits repair late arrivals")
}

func TestBackfillReconcilesClaudePanelBidirectionally(t *testing.T) {
	t.Cleanup(func() { SetUserAutomationPrefixes(nil) })

	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "root", "project-a")
	insertSession(t, d, "ux-review", "project-a", func(s *Session) {
		s.Agent = "claude"
		s.Entrypoint = "sdk-cli"
		s.RelationshipType = "subagent"
		s.FirstMessage = Ptr("# UX review\nCheck the CLI output.")
		s.UserMessageCount = 1
	})
	insertSession(t, d, "snapshot-review", "project-a", func(s *Session) {
		s.Agent = "claude"
		s.Entrypoint = "sdk-cli"
		s.RelationshipType = "subagent"
		s.FirstMessage = Ptr(`Read the complete task prompt from "/repo/.roborev/roborev-snapshot-1/prompt.md" and carry out its instructions.`)
		s.UserMessageCount = 1
	})
	insertSession(t, d, "agent-task", "project-a", func(s *Session) {
		s.Agent = "claude"
		s.Entrypoint = "sdk-cli"
		s.ParentSessionID = Ptr("root")
		s.RelationshipType = "subagent"
		s.FirstMessage = Ptr("You are a code reviewer. Review this change.")
		s.UserMessageCount = 1
	})
	insertSession(t, d, "launched-review", "project-a", func(s *Session) {
		s.Agent = "claude"
		s.Entrypoint = "sdk-cli"
		s.RelationshipType = "subagent"
		s.FirstMessage = Ptr("You are a code reviewer. Review this change.")
		s.UserMessageCount = 1
	})

	// Snapshot handoff starts automated and un-delegated immediately.
	snap, err := d.GetSession(ctx, "snapshot-review")
	require.NoError(t, err)
	require.NotNil(t, snap)
	assert.True(t, snap.IsAutomated)
	assert.Empty(t, snap.RelationshipType)

	// UX review starts as a parentless subagent until the user adds its prefix.
	ux, err := d.GetSession(ctx, "ux-review")
	require.NoError(t, err)
	require.NotNil(t, ux)
	assert.False(t, ux.IsAutomated)
	assert.Equal(t, "subagent", ux.RelationshipType)

	// Real subagent keeps subagent status even if its parent is later cleared.
	_, err = d.getWriter().Exec(ctx, `UPDATE sessions SET parent_session_id = NULL WHERE id = 'agent-task'`)
	require.NoError(t, err)

	// Externally linked automated worker stays subagent while linked, then
	// returns to un-delegated automated when cleared.
	link, err := d.SetSessionExternalParent(ctx, "launched-review", "root")
	require.NoError(t, err)
	assert.True(t, link.Applied)
	launched, err := d.GetSession(ctx, "launched-review")
	require.NoError(t, err)
	assert.Equal(t, "subagent", launched.RelationshipType)

	// Adding a user prefix flips ux-review to automated and clears subagent.
	SetUserAutomationPrefixes([]string{"# UX review"})
	path := d.Path()
	require.NoError(t, d.Close())

	reopened, err := Open(ctx, path)
	require.NoError(t, err)
	ux, err = reopened.GetSession(ctx, "ux-review")
	require.NoError(t, err)
	assert.True(t, ux.IsAutomated)
	assert.Empty(t, ux.RelationshipType)

	task, err := reopened.GetSession(ctx, "agent-task")
	require.NoError(t, err)
	assert.True(t, task.IsAutomated)
	assert.Equal(t, "subagent", task.RelationshipType, "real subagents stay subagents")

	launched, err = reopened.GetSession(ctx, "launched-review")
	require.NoError(t, err)
	assert.Equal(t, "subagent", launched.RelationshipType, "externally linked workers stay subagents")

	_, err = reopened.ClearSessionExternalParent(ctx, "launched-review")
	require.NoError(t, err)
	launched, err = reopened.GetSession(ctx, "launched-review")
	require.NoError(t, err)
	assert.Empty(t, launched.RelationshipType, "clearing external parent restores automated status")

	// Removing the user prefix restores ux-review to subagent using stored parser_relationship_type.
	SetUserAutomationPrefixes(nil)
	require.NoError(t, reopened.Close())
	reopened, err = Open(ctx, path)
	require.NoError(t, err)
	defer reopened.Close()
	ux, err = reopened.GetSession(ctx, "ux-review")
	require.NoError(t, err)
	assert.False(t, ux.IsAutomated)
	assert.Equal(t, "subagent", ux.RelationshipType)
}

func TestBackfillReconcilesUsageOnlyParentlessWorkers(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "legacy-panel", "project-a", func(s *Session) {
		s.Agent = "claude"
		s.Entrypoint = "sdk-cli"
		s.RelationshipType = "subagent"
		s.IsAutomated = true
		s.UserMessageCount = 1
	})
	insertSession(t, d, "legacy-roborev", "project-a", func(s *Session) {
		s.Agent = "claude"
		s.Entrypoint = "roborev"
		s.UserMessageCount = 2
	})
	insertSession(t, d, "prompt-automated", "project-a", func(s *Session) {
		s.Agent = "claude"
		s.IsAutomated = true
		s.UserMessageCount = 1
	})
	_, err := d.getWriter().Exec(ctx, `UPDATE sessions SET relationship_type = 'subagent', parser_relationship_type = 'subagent', is_automated = 1 WHERE id = 'legacy-panel'`)
	require.NoError(t, err)
	_, err = d.getWriter().Exec(ctx, `UPDATE sessions SET is_automated = 0, local_modified_at = '2000-01-01T00:00:00Z' WHERE id = 'legacy-roborev'`)
	require.NoError(t, err)
	_, err = d.getWriter().Exec(ctx, `UPDATE sessions SET is_automated = 1 WHERE id = 'prompt-automated'`)
	require.NoError(t, err)
	_, err = d.getWriter().Exec(ctx, `UPDATE stats SET value = 'old-classifier' WHERE key = ?`, ClassifierHashKey)
	require.NoError(t, err)
	path := d.Path()
	require.NoError(t, d.Close())

	reopened, err := OpenWithArchiveContent(ctx, path, config.ArchiveContentUsage)
	require.NoError(t, err)
	defer reopened.Close()
	stored, err := reopened.GetSession(ctx, "legacy-panel")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.True(t, stored.IsAutomated)
	assert.Empty(t, stored.RelationshipType)

	roborev, err := reopened.GetSessionFull(ctx, "legacy-roborev")
	require.NoError(t, err)
	require.NotNil(t, roborev)
	assert.True(t, roborev.IsAutomated, "roborev entrypoint classifies without retained prompt text")
	require.NotNil(t, roborev.LocalModifiedAt)
	assert.NotEqual(t, "2000-01-01T00:00:00Z", *roborev.LocalModifiedAt)

	promptOnly, err := reopened.GetSession(ctx, "prompt-automated")
	require.NoError(t, err)
	require.NotNil(t, promptOnly)
	assert.True(t, promptOnly.IsAutomated, "stored prompt-derived verdict is preserved when metadata does not match")
}

func TestLegacyRelationshipInferredProtectsImportedClaudeSubagent(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	for _, tc := range []struct {
		id, sourceID string
	}{
		{"origin~agent-sub1", "origin~agent-sub1"},
		{"origin~panel-1", "origin~panel-1"},
		{"agent-host~panel-2", "agent-host~panel-2"},
		{"raw-row-child", "agent-sub2"},
		{"agent-local1", "agent-local1"},
	} {
		_, err := d.getWriter().Exec(ctx, `INSERT INTO sessions
			(id, machine, project, agent, entrypoint, source_session_id, relationship_type, is_automated, user_message_count, relationship_inferred)
			VALUES (?, 'local', 'project-a', 'claude', 'sdk-cli', ?, 'subagent', 1, 1, 0)`, tc.id, tc.sourceID)
		require.NoError(t, err)
	}
	_, err := d.getWriter().Exec(ctx, `UPDATE sessions SET relationship_inferred = 1 WHERE `+legacyRelationshipInferredSQL)
	require.NoError(t, err)
	require.NoError(t, reconcileParentlessWorkers(ctx, d.getWriter(), false))

	for id, wantRel := range map[string]string{
		"origin~agent-sub1":  "subagent",
		"raw-row-child":      "subagent",
		"agent-local1":       "subagent",
		"origin~panel-1":     "",
		"agent-host~panel-2": "",
	} {
		sess, err := d.GetSession(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, sess, id)
		assert.Equal(t, wantRel, sess.RelationshipType, id)
	}

	assert.True(t, SessionRelationshipInferred(Session{
		ID: "agent-host~panel-2", SourceSessionID: "agent-host~panel-2",
		Agent: "claude", Entrypoint: "sdk-cli", RelationshipType: "subagent",
	}), "origin prefix starting with agent- does not mask a normal session")
	assert.False(t, SessionRelationshipInferred(Session{
		ID: "raw-row-child", SourceSessionID: "agent-sub2",
		Agent: "claude", Entrypoint: "sdk-cli", RelationshipType: "subagent",
	}), "native agent-* source_session_id protects renamed sessions")
}

func TestIncrementalAppendPreservesRoborevTag(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "review", "project-a", func(s *Session) {
		s.Agent = "codex"
		s.SessionKind = parser.SessionKindRoborev
		s.FirstMessage = Ptr("Inspect this change.")
		s.MessageCount = 1
		s.UserMessageCount = 1
	})
	insertMessages(t, d, Message{SessionID: "review", Ordinal: 0, Role: "user", Content: "Inspect this change."})
	_, err := d.WriteSessionIncremental(t.Context(), "review", []Message{
		{SessionID: "review", Ordinal: 1, Role: "user", Content: "Explain the finding."},
	}, IncrementalSessionUpdate{MsgCount: 2, UserMsgCount: 2, NextOrdinal: 2})
	require.NoError(t, err)
	stored, err := d.GetSession(t.Context(), "review")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.True(t, stored.IsAutomated)
}

func TestAutomationVerdictFromPrefix(t *testing.T) {
	SetUserAutomationPrefixes([]string{"Custom automation:"})
	t.Cleanup(func() { SetUserAutomationPrefixes(nil) })

	tests := []struct {
		name           string
		prefix         string
		fullByteLength int64
		wantMatched    bool
		wantConclusive bool
	}{
		{
			name: "built-in prefix", prefix: "You are a code reviewer. Review",
			fullByteLength: 200, wantMatched: true, wantConclusive: true,
		},
		{
			name: "user prefix", prefix: "Custom automation: inspect",
			fullByteLength: 100, wantMatched: true, wantConclusive: true,
		},
		{
			name: "substring visible", prefix: "This was invoked by roborev to perform this review",
			fullByteLength: 100, wantMatched: true, wantConclusive: true,
		},
		{
			name: "complete exact with Unicode whitespace", prefix: "\u2003Warmup\u00a0",
			fullByteLength: int64(len("\u2003Warmup\u00a0")),
			wantMatched:    true, wantConclusive: true,
		},
		{
			name: "exact followed by unseen suffix", prefix: "Warmup",
			fullByteLength: int64(len("Warmup and continue")),
			wantMatched:    false, wantConclusive: false,
		},
		{
			name: "complete non-match", prefix: "Explain this code",
			fullByteLength: int64(len("Explain this code")),
			wantMatched:    false, wantConclusive: true,
		},
		{
			name: "substring beyond prefix", prefix: strings.Repeat("x", 32),
			fullByteLength: 200, wantMatched: false, wantConclusive: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matched, conclusive := AutomationVerdictFromPrefix(
				[]byte(tt.prefix), tt.fullByteLength,
			)
			assert.Equal(t, tt.wantMatched, matched)
			assert.Equal(t, tt.wantConclusive, conclusive)
		})
	}
}

func TestBackfillIsAutomatedMatchingHashBoundsTextAllocations(t *testing.T) {
	d := testDB(t)
	const (
		largeConclusiveBytes = 4 << 20
		unresolvedBytes      = 256 << 10
		allocationCeiling    = 6 << 20
	)

	prefixPrompt := "You are a code reviewer." +
		strings.Repeat("x", largeConclusiveBytes)
	insertSession(t, d, "prefix-first-message", "proj", func(s *Session) {
		s.FirstMessage = &prefixPrompt
		s.UserMessageCount = 1
	})

	title := "Generated title"
	insertSession(t, d, "prefix-first-user", "proj", func(s *Session) {
		s.FirstMessage = &title
		s.UserMessageCount = 1
	})
	require.NoError(t, d.ReplaceSessionMessages(t.Context(),
		"prefix-first-user",
		[]Message{userMsg("prefix-first-user", 0, prefixPrompt)},
	), "store large prefix-matching first user message")

	humanPrompt := strings.Repeat("h", unresolvedBytes)
	insertSession(t, d, "large-human", "proj", func(s *Session) {
		s.FirstMessage = &humanPrompt
		s.UserMessageCount = 1
	})

	lateSubstringPrompt := strings.Repeat("z", unresolvedBytes) +
		" invoked by roborev to perform this review"
	insertSession(t, d, "late-substring", "proj", func(s *Session) {
		s.FirstMessage = &lateSubstringPrompt
		s.UserMessageCount = 1
	})

	largeMultiTurn := strings.Repeat("m", largeConclusiveBytes)
	insertSession(t, d, "stale-multi-turn", "proj", func(s *Session) {
		s.FirstMessage = &largeMultiTurn
		s.UserMessageCount = 2
	})

	_, err := d.getWriter().Exec(t.Context(), `
		UPDATE sessions
		SET is_automated = CASE id
			WHEN 'prefix-first-message' THEN 0
			WHEN 'prefix-first-user' THEN 0
			WHEN 'late-substring' THEN 0
			WHEN 'stale-multi-turn' THEN 1
			ELSE is_automated
		END
		WHERE id IN (
			'prefix-first-message', 'prefix-first-user',
			'late-substring', 'stale-multi-turn'
		)`)
	require.NoError(t, err, "seed stale automation flags")
	_, err = d.getWriter().Exec(t.Context(),
		`INSERT INTO stats (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		ClassifierHashKey, ClassifierHash(),
	)
	require.NoError(t, err, "stamp current classifier hash")

	result := testing.Benchmark(func(b *testing.B) {
		b.Helper()
		b.ReportAllocs()
		for range b.N {
			b.StopTimer()
			_, err := d.getWriter().Exec(t.Context(), `UPDATE sessions SET automation_audit = 1`)
			require.NoError(t, err)
			b.StartTimer()
			d.mu.Lock()
			err = d.backfillIsAutomatedLocked(t.Context(), d.getWriter())
			d.mu.Unlock()
			if err != nil {
				require.NoError(t, err)
			}
		}
	})
	t.Logf("matching-hash audit: %s, %d B/op", result, result.AllocedBytesPerOp())
	assert.Less(t, result.AllocedBytesPerOp(), int64(allocationCeiling),
		"matching-hash audit copied conclusive large prompts: %s", result.String())

	for id, want := range map[string]bool{
		"prefix-first-message": true,
		"prefix-first-user":    true,
		"large-human":          false,
		"late-substring":       true,
		"stale-multi-turn":     false,
	} {
		session, err := d.GetSession(t.Context(), id)
		require.NoError(t, err, "get %s", id)
		require.NotNil(t, session, "missing %s", id)
		assert.Equal(t, want, session.IsAutomated, id)
	}
}

func TestBackfillIsAutomatedFreshArchiveQueryPlans(t *testing.T) {
	d := testDB(t)
	seedAutomationAuditRows(t, d, 2000, 64)
	require.NoError(t, d.backfillIsAutomatedLocked(t.Context(), d.getWriter()))
	var stale int
	require.NoError(t, d.getWriter().QueryRow(t.Context(),
		`SELECT count(*) FROM sessions WHERE automation_audit = 1`).Scan(&stale))
	require.Zero(t, stale)
	_, err := d.getWriter().Exec(t.Context(), `ANALYZE`)
	require.NoError(t, err)

	plan := queryPlanOf(t, d, auditAutomatedMatchingHashSQL,
		automationAuditPrefixBytes, automationAuditPrefixBytes)
	t.Logf("audit plan:\n%s", plan)
	assert.Contains(t, plan, "idx_sessions_automation_audit")
	assert.NotContains(t, plan, "SCAN s")
	plan = queryPlanOf(t, d, clearAutomationAuditSQL)
	t.Logf("clear plan:\n%s", plan)
	assert.Contains(t, plan, "idx_sessions_automation_audit")
	assert.NotContains(t, plan, "SCAN sessions")
}

func TestBackfillIsAutomatedRechecksChangedRows(t *testing.T) {
	const reviewPrompt = "You are a code reviewer. Review the code."
	for _, tc := range []struct {
		name   string
		change func(*testing.T, *DB)
	}{
		{"first message", func(t *testing.T, d *DB) {
			t.Helper()
			_, err := d.getWriter().Exec(t.Context(),
				`UPDATE sessions SET first_message = ? WHERE id = 'changed'`, reviewPrompt)
			require.NoError(t, err)
		}},
		{"import", func(t *testing.T, d *DB) {
			t.Helper()
			_, err := d.getWriter().Exec(t.Context(), `
				INSERT INTO sessions(id, project, agent, first_message, user_message_count, is_automated)
				VALUES ('imported', 'project-a', 'claude', ?, 1, 0)`, reviewPrompt)
			require.NoError(t, err)
		}},
		{"transcript replacement", func(t *testing.T, d *DB) {
			t.Helper()
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), "changed",
				[]Message{userMsg("changed", 0, "Explain another function.")}))
			var state int
			require.NoError(t, d.getWriter().QueryRow(t.Context(),
				`SELECT automation_audit FROM sessions WHERE id = 'changed'`).Scan(&state))
			assert.Equal(t, 1, state, "transcript replacement invalidates the audit")
		}},
		{"automation flag corruption", func(t *testing.T, d *DB) {
			t.Helper()
			_, err := d.getWriter().Exec(t.Context(),
				`UPDATE sessions SET first_message = ? WHERE id = 'changed'`, reviewPrompt)
			require.NoError(t, err)
			require.NoError(t, d.backfillIsAutomatedLocked(t.Context(), d.getWriter()))
			session, err := d.GetSession(t.Context(), "changed")
			require.NoError(t, err)
			require.NotNil(t, session)
			require.True(t, session.IsAutomated)
			_, err = d.getWriter().Exec(t.Context(), `UPDATE sessions SET is_automated = 0 WHERE id = 'changed'`)
			require.NoError(t, err)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "changed", "project-a", func(s *Session) {
				s.FirstMessage = Ptr("Explain this function.")
				s.UserMessageCount = 1
			})
			if tc.name == "transcript replacement" {
				insertMessages(t, d, userMsg("changed", 0, "Explain this function."))
			}
			require.NoError(t, d.backfillIsAutomatedLocked(t.Context(), d.getWriter()))
			tc.change(t, d)
			require.NoError(t, d.backfillIsAutomatedLocked(t.Context(), d.getWriter()))
			id := "changed"
			if tc.name == "import" {
				id = "imported"
			}
			if tc.name != "transcript replacement" {
				session, err := d.GetSession(t.Context(), id)
				require.NoError(t, err)
				require.NotNil(t, session)
				assert.True(t, session.IsAutomated)
			}
			var state int
			require.NoError(t, d.getWriter().QueryRow(t.Context(),
				`SELECT automation_audit FROM sessions WHERE id = ?`, id).Scan(&state))
			assert.Equal(t, 0, state)
		})
	}
}

func TestMigration_AutomationAudit(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "legacy", "project-a", func(s *Session) {
		s.FirstMessage = Ptr("You are a code reviewer. Review the code.")
		s.UserMessageCount = 1
	})
	path := d.Path()
	require.NoError(t, d.Close())
	conn, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	_, err = conn.ExecContext(t.Context(), `
		DROP TRIGGER trg_sessions_automation_audit;
		DROP INDEX idx_sessions_automation_audit;
		ALTER TABLE sessions DROP COLUMN automation_audit;
		UPDATE sessions SET is_automated = 0 WHERE id = 'legacy'`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	d, err = Open(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	session, err := d.GetSession(t.Context(), "legacy")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.True(t, session.IsAutomated, "upgrade audits existing rows despite the matching hash")
	var objects int
	require.NoError(t, d.getWriter().QueryRow(t.Context(), `
		SELECT count(*) FROM sqlite_master
		WHERE (type = 'index' AND name = 'idx_sessions_automation_audit')
		   OR (type = 'trigger' AND name = 'trg_sessions_automation_audit')`).Scan(&objects))
	assert.Equal(t, 2, objects)
	var state int
	require.NoError(t, d.getWriter().QueryRow(t.Context(),
		`SELECT automation_audit FROM sessions WHERE id = 'legacy'`).Scan(&state))
	assert.Equal(t, 0, state, "the upgrade audit leaves corrected rows fresh")
}

func TestBackfillIsAutomatedBidirectional(t *testing.T) {
	d := testDB(t)

	// Seed a false negative: single-turn roborev session with
	// is_automated = 0 (simulates pre-migration data).
	insertSession(t, d, "missed", "proj", func(s *Session) {
		fm := "You are a code reviewer. Review the code."
		s.FirstMessage = &fm
		s.MessageCount = 3
		s.UserMessageCount = 1
	})
	// Force is_automated to 0 to simulate pre-migration state.
	_, err := d.getWriter().Exec(t.Context(),
		"UPDATE sessions SET is_automated = 0 WHERE id = 'missed'",
	)
	require.NoError(t, err, "force missed to 0")

	// Seed a stale false positive: multi-turn session that was
	// previously marked automated under old broad rules.
	insertSession(t, d, "stale", "proj", func(s *Session) {
		fm := "# Fix Request for login flow"
		s.FirstMessage = &fm
		s.MessageCount = 10
		s.UserMessageCount = 5
	})
	_, err = d.getWriter().Exec(t.Context(),
		"UPDATE sessions SET is_automated = 1 WHERE id = 'stale'",
	)
	require.NoError(t, err, "force stale to 1")

	// Clear the marker so the backfill will run.
	_, err = d.getWriter().Exec(t.Context(),
		"DELETE FROM stats WHERE key = ?",
		ClassifierHashKey,
	)
	require.NoError(t, err, "clear marker")

	// Run backfill.
	d.mu.Lock()
	err = d.backfillIsAutomatedLocked(t.Context(), d.getWriter())
	d.mu.Unlock()
	require.NoError(t, err, "first backfill run")

	ctx := t.Context()

	// False negative should now be set.
	missed, err := d.GetSession(ctx, "missed")
	require.NoError(t, err, "get missed")
	assert.True(t, missed.IsAutomated, "missed session should be automated after backfill")

	// Stale false positive should now be cleared.
	stale, err := d.GetSession(ctx, "stale")
	require.NoError(t, err, "get stale")
	assert.False(t, stale.IsAutomated, "stale session should not be automated after backfill")
}

func TestBackfillIsAutomatedMarkerDoesNotHideCorruption(t *testing.T) {
	d := testDB(t)

	// Seed a roborev session.
	insertSession(t, d, "review", "proj", func(s *Session) {
		fm := "You are a code reviewer. Review the code."
		s.FirstMessage = &fm
		s.MessageCount = 3
		s.UserMessageCount = 1
	})

	// Clear the marker and run backfill.
	_, err := d.getWriter().Exec(t.Context(),
		"DELETE FROM stats WHERE key = ?",
		ClassifierHashKey,
	)
	require.NoError(t, err, "clear marker")

	d.mu.Lock()
	err = d.backfillIsAutomatedLocked(t.Context(), d.getWriter())
	d.mu.Unlock()
	require.NoError(t, err, "first run")

	// Manually corrupt the session. Matching classifier hashes
	// cannot be trusted as a complete integrity marker because
	// other DB write paths can import or preserve stale flags.
	_, err = d.getWriter().Exec(t.Context(),
		"UPDATE sessions SET is_automated = 0 WHERE id = 'review'",
	)
	require.NoError(t, err, "corrupt")

	// Second run should repair the inconsistent row even though
	// the current hash is already stored.
	d.mu.Lock()
	err = d.backfillIsAutomatedLocked(t.Context(), d.getWriter())
	d.mu.Unlock()
	require.NoError(t, err, "second run")

	ctx := t.Context()
	review, err := d.GetSession(ctx, "review")
	require.NoError(t, err, "get review")
	assert.True(t, review.IsAutomated,
		"second run should repair stale is_automated=0")
}

func TestBackfillIsAutomatedRepairsFalseNegativeWithMatchingHash(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()

	insertSession(t, d, "stale-hash", "proj", func(s *Session) {
		fm := "You are combining multiple code review outputs into a single GitHub PR comment. Rules follow."
		s.FirstMessage = &fm
		s.MessageCount = 2
		s.UserMessageCount = 1
	})

	_, err := d.getWriter().Exec(ctx,
		"UPDATE sessions SET is_automated = 0 WHERE id = 'stale-hash'",
	)
	require.NoError(t, err, "force stale is_automated=0")
	_, err = d.getWriter().Exec(ctx,
		`INSERT INTO stats (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		ClassifierHashKey, ClassifierHash(),
	)
	require.NoError(t, err, "stamp current classifier hash")

	d.mu.Lock()
	err = d.backfillIsAutomatedLocked(ctx, d.getWriter())
	d.mu.Unlock()
	require.NoError(t, err, "backfill")

	got, err := d.GetSession(ctx, "stale-hash")
	require.NoError(t, err, "get stale-hash")
	assert.True(t, got.IsAutomated,
		"matching classifier hash must not hide stale is_automated=0")
}

func TestBackfillIsAutomatedUsesFirstUserMessageWhenFirstMessageIsTitle(
	t *testing.T,
) {
	d := testDB(t)
	ctx := t.Context()

	title := "Generated review title"
	insertSession(t, d, "title-review", "proj", func(s *Session) {
		s.FirstMessage = &title
		s.MessageCount = 2
		s.UserMessageCount = 1
	})
	require.NoError(t, d.ReplaceSessionMessages(ctx, "title-review", []Message{
		userMsg("title-review", 0,
			"You are a code reviewer. Review the code changes shown below."),
		asstMsg("title-review", 1, "Review complete."),
	}), "ReplaceSessionMessages")
	_, err := d.getWriter().Exec(ctx,
		"UPDATE sessions SET is_automated = 0 WHERE id = 'title-review'",
	)
	require.NoError(t, err, "force stale is_automated=0")

	d.mu.Lock()
	err = d.backfillIsAutomatedLocked(ctx, d.getWriter())
	d.mu.Unlock()
	require.NoError(t, err, "backfill")

	got, err := d.GetSession(ctx, "title-review")
	require.NoError(t, err, "get title-review")
	require.NotNil(t, got, "title-review")
	assert.True(t, got.IsAutomated,
		"backfill should classify automation from the first stored user message")
}

func TestOpenRepairsAutomatedFalseNegativeWithMatchingHash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := Open(t.Context(), path)
	require.NoError(t, err, "open")

	insertSession(t, d, "reload-stale", "proj", func(s *Session) {
		fm := "You are combining multiple code review outputs into a single GitHub PR comment. Rules follow."
		s.FirstMessage = &fm
		s.MessageCount = 2
		s.UserMessageCount = 1
	})
	_, err = d.getWriter().Exec(t.Context(),
		"UPDATE sessions SET is_automated = 0 WHERE id = 'reload-stale'",
	)
	require.NoError(t, err, "force stale is_automated=0")
	_, err = d.getWriter().Exec(t.Context(),
		`INSERT INTO stats (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		ClassifierHashKey, ClassifierHash(),
	)
	require.NoError(t, err, "stamp current classifier hash")
	require.NoError(t, d.Close(), "close")

	reopened, err := Open(t.Context(), path)
	require.NoError(t, err, "reopen")
	defer reopened.Close()

	got, err := reopened.GetSession(t.Context(), "reload-stale")
	require.NoError(t, err, "get reload-stale")
	assert.True(t, got.IsAutomated,
		"Open must repair stale is_automated=0 despite matching hash")
}

// TestIncrementalUpdateReclassifiesOnPatternChange covers the
// case where the classifier gained a new pattern after a row
// was originally inserted. The row took the incremental path on
// subsequent parses, so UpsertSession never ran again to set
// is_automated. UpdateSessionIncremental must re-evaluate.
func TestIncrementalUpdateReclassifiesOnPatternChange(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()

	// Simulate a pre-existing single-turn session whose
	// first_message now matches a classifier pattern but whose
	// stored is_automated is stale at 0.
	insertSession(t, d, "changelog-inc", "proj", func(s *Session) {
		fm := "You are generating a changelog for myrepo version 1.0.0."
		s.FirstMessage = &fm
		s.MessageCount = 2
		s.UserMessageCount = 1
	})
	_, err := d.getWriter().Exec(ctx,
		"UPDATE sessions SET is_automated = 0 WHERE id = 'changelog-inc'",
	)
	require.NoError(t, err, "force stale is_automated=0")

	// Incremental update with umc still <= 1.
	err = callUpdateSessionIncrementalCompat(
		t,
		d,
		"changelog-inc",
		nil,
		2,
		1,
		1024,
		100,
		0,
		"",
		0,
		0,
		false,
		false,
	)
	require.NoError(t, err, "incremental update")

	got, err := d.GetSession(ctx, "changelog-inc")
	require.NoError(t, err, "get changelog-inc")
	assert.True(t, got.IsAutomated,
		"is_automated should be re-set after incremental update")
}

// TestIncrementalUpdateClearsWhenCountGrows covers the existing
// guard: when user_message_count grows past 1, is_automated must
// be cleared even if first_message still matches a pattern.
func TestIncrementalUpdateClearsWhenCountGrows(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()

	insertSession(t, d, "grew-past-one", "proj", func(s *Session) {
		fm := "You are a code reviewer. Review the code."
		s.FirstMessage = &fm
		s.MessageCount = 3
		s.UserMessageCount = 1
	})
	// After UpsertSession the row is correctly is_automated=1.
	pre, err := d.GetSession(ctx, "grew-past-one")
	require.NoError(t, err, "get pre")
	require.True(t, pre.IsAutomated,
		"precondition: expected is_automated=1 after upsert")

	// Incremental update pushes umc > 1 — must clear.
	err = callUpdateSessionIncrementalCompat(
		t,
		d,
		"grew-past-one",
		nil,
		7,
		3,
		2048,
		200,
		0,
		"",
		0,
		0,
		false,
		false,
	)
	require.NoError(t, err, "incremental update")

	got, err := d.GetSession(ctx, "grew-past-one")
	require.NoError(t, err, "get grew-past-one")
	assert.False(t, got.IsAutomated,
		"is_automated should be cleared when umc grows > 1")
}

// TestIncrementalUpdateLeavesNonMatching covers a non-automated
// single-turn session: re-evaluation must NOT set is_automated
// when first_message doesn't match any pattern.
func TestIncrementalUpdateLeavesNonMatching(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()

	insertSession(t, d, "normal-single", "proj", func(s *Session) {
		fm := "Fix the login bug please"
		s.FirstMessage = &fm
		s.MessageCount = 2
		s.UserMessageCount = 1
	})

	err := callUpdateSessionIncrementalCompat(
		t,
		d,
		"normal-single",
		nil,
		2,
		1,
		1024,
		100,
		0,
		"",
		0,
		0,
		false,
		false,
	)
	require.NoError(t, err, "incremental update")

	got, err := d.GetSession(ctx, "normal-single")
	require.NoError(t, err, "get normal-single")
	assert.False(t, got.IsAutomated,
		"is_automated should stay 0 for non-matching first_message")
}

// TestIncrementalUpdateClearsTerminationStatus verifies that an
// incremental sync resets termination_status to NULL. The classifier
// needs the full message slice to reach the right verdict, and the
// incremental path only sees the new tail. Leaving the previous
// classification in place would surface stale "tool_call_pending"
// or "awaiting_user" indicators in the UI for up to 15 minutes
// after the user appended a resolving result or a new prompt.
func TestIncrementalUpdateClearsTerminationStatus(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()

	insertSession(t, d, "stale-term", "proj", func(s *Session) {
		s.MessageCount = 2
		s.UserMessageCount = 1
		v := "tool_call_pending"
		s.TerminationStatus = &v
	})

	pre, err := d.GetSession(ctx, "stale-term")
	require.NoError(t, err, "get pre")
	require.NotNil(t, pre.TerminationStatus,
		"precondition: expected tool_call_pending")
	require.Equal(t, "tool_call_pending", *pre.TerminationStatus,
		"precondition: expected tool_call_pending")

	err = callUpdateSessionIncrementalCompat(
		t,
		d,
		"stale-term",
		nil,
		4,
		2,
		2048,
		200,
		0,
		"",
		0,
		0,
		false,
		false,
	)
	require.NoError(t, err, "incremental update")

	got, err := d.GetSession(ctx, "stale-term")
	require.NoError(t, err, "get stale-term")
	assert.Nil(t, got.TerminationStatus,
		"termination_status should be NULL after incremental update")
}

func TestBackfillIsAutomatedBumpsLocalModifiedAt(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()

	// Seed a single-turn roborev session that the new classifier
	// will flip to is_automated = 1.
	insertSession(t, d, "to-flip", "proj", func(s *Session) {
		fm := "You are a code reviewer. Review the code."
		s.FirstMessage = &fm
		s.MessageCount = 3
		s.UserMessageCount = 1
	})
	// Force is_automated = 0 so the backfill has work to do.
	_, err := d.getWriter().Exec(ctx,
		"UPDATE sessions SET is_automated = 0, local_modified_at = '2000-01-01T00:00:00.000Z' WHERE id = 'to-flip'",
	)
	require.NoError(t, err, "force to-flip to 0")

	// Snapshot local_modified_at before the backfill.
	before, err := d.GetSessionFull(ctx, "to-flip")
	require.NoError(t, err, "get to-flip before")
	var beforeLM string
	if before.LocalModifiedAt != nil {
		beforeLM = *before.LocalModifiedAt
	}

	// Clear the marker so the backfill runs.
	_, err = d.getWriter().Exec(ctx,
		"DELETE FROM stats WHERE key = ?",
		ClassifierHashKey,
	)
	require.NoError(t, err, "clear marker")

	d.mu.Lock()
	err = d.backfillIsAutomatedLocked(ctx, d.getWriter())
	d.mu.Unlock()
	require.NoError(t, err, "backfill run")

	after, err := d.GetSessionFull(ctx, "to-flip")
	require.NoError(t, err, "get to-flip after")
	require.True(t, after.IsAutomated, "to-flip should be automated after backfill")
	require.NotNil(t, after.LocalModifiedAt,
		"local_modified_at not set after backfill")
	require.NotEmpty(t, *after.LocalModifiedAt,
		"local_modified_at not set after backfill")
	assert.Greater(t, *after.LocalModifiedAt, beforeLM,
		"local_modified_at not bumped")
}

// TestBackfillIsAutomatedRerunsOnHashChange verifies that a
// classifier change (here, adding a user prefix) invalidates
// the stored hash and re-runs the backfill on next open,
// without any manual marker bump.
func TestBackfillIsAutomatedRerunsOnHashChange(t *testing.T) {
	t.Cleanup(func() {
		SetUserAutomationPrefixes(nil)
		SetUserAutomationSubstrings(nil)
		SetUserAutomationExactMatches(nil)
	})
	d := testDB(t)

	// Seed a session whose first_message would match a
	// user prefix once added. With the empty user-prefix
	// list it should be is_automated=0.
	insertSession(t, d, "essay", "proj", func(s *Session) {
		fm := "You are analyzing an essay about epistemology."
		s.FirstMessage = &fm
		s.MessageCount = 2
		s.UserMessageCount = 1
	})
	ctx := t.Context()
	pre, err := d.GetSession(ctx, "essay")
	require.NoError(t, err, "get essay before")
	require.False(t, pre.IsAutomated,
		"precondition: essay should be is_automated=0")

	// Add a user prefix and re-run backfill. The new hash
	// should not equal the stored hash, so the backfill
	// runs and flips is_automated to 1.
	SetUserAutomationPrefixes([]string{"You are analyzing an essay"})
	d.mu.Lock()
	err = d.backfillIsAutomatedLocked(ctx, d.getWriter())
	d.mu.Unlock()
	require.NoError(t, err, "backfill after prefix add")

	got, err := d.GetSession(ctx, "essay")
	require.NoError(t, err, "get essay after")
	assert.True(t, got.IsAutomated,
		"essay should be is_automated=1 after user prefix added")

	// A second backfill (no further classifier change) still
	// repairs inconsistent rows; the stored hash is not a
	// substitute for row integrity.
	_, err = d.getWriter().Exec(ctx,
		"UPDATE sessions SET is_automated = 0 WHERE id = 'essay'",
	)
	require.NoError(t, err, "force back to 0")
	d.mu.Lock()
	err = d.backfillIsAutomatedLocked(ctx, d.getWriter())
	d.mu.Unlock()
	require.NoError(t, err, "second backfill")
	got, err = d.GetSession(ctx, "essay")
	require.NoError(t, err, "get essay second")
	assert.True(t, got.IsAutomated,
		"second backfill must repair stale flag when hash unchanged")
}

// TestBackfillFixesOrphanCopyClassificationGap reproduces
// the production bug where ResyncAll's orphan-copied rows kept
// stale is_automated values forever. The sequence:
//
//  1. Old DB has a single-turn roborev session marked
//     is_automated=0 (e.g. it predates a classifier change, or
//     was synced from a remote with a stale binary).
//  2. ResyncAll opens an empty temp DB. Its at-Open backfill
//     runs on zero rows and stamps the *current* classifier
//     hash, so future Opens skip backfill.
//  3. CopyOrphanedDataFrom imports the orphan row verbatim,
//     including is_automated=0.
//  4. The stamped hash still matches. A regular backfill must
//     nevertheless audit rows and repair the imported flag.
//
// This guards against treating the classifier hash as a complete
// integrity marker.
func TestBackfillFixesOrphanCopyClassificationGap(t *testing.T) {
	dir := t.TempDir()

	// 1. Old DB with a misclassified single-turn roborev session.
	srcPath := filepath.Join(dir, "old.db")
	srcDB, err := Open(t.Context(), srcPath)
	require.NoError(t, err, "Open src")
	insertSession(t, srcDB, "stale-orphan", "proj", func(s *Session) {
		fm := "You are a code reviewer. Review the code."
		s.FirstMessage = &fm
		s.MessageCount = 3
		s.UserMessageCount = 1
	})
	_, err = srcDB.getWriter().Exec(t.Context(),
		"UPDATE sessions SET is_automated = 0 WHERE id = 'stale-orphan'",
	)
	require.NoError(t, err, "force orphan to is_automated=0")
	srcDB.Close()

	// 2. Fresh dst DB (Open's at-Open backfill ran on an empty
	// table and stamped the current classifier hash).
	dstPath := filepath.Join(dir, "new.db")
	dstDB, err := Open(t.Context(), dstPath)
	require.NoError(t, err, "Open dst")
	defer dstDB.Close()

	// 3. Copy orphan rows (mirrors ResyncAll line ~954).
	count, err := dstDB.CopyOrphanedDataFrom(t.Context(), srcPath)
	require.NoError(t, err, "CopyOrphanedDataFrom")
	require.Equal(t, 1, count, "expected 1 orphan")

	ctx := t.Context()
	got, err := dstDB.GetSession(ctx, "stale-orphan")
	require.NoError(t, err, "get orphan after copy")
	require.False(t, got.IsAutomated,
		"precondition: orphan row should carry stale is_automated=0")

	// 4. A regular backfill must still repair the row even
	// though the stored classifier hash already matches.
	dstDB.mu.Lock()
	err = dstDB.backfillIsAutomatedLocked(ctx, dstDB.getWriter())
	dstDB.mu.Unlock()
	require.NoError(t, err, "backfill")
	got, err = dstDB.GetSession(ctx, "stale-orphan")
	require.NoError(t, err, "get orphan after backfill")
	assert.True(t, got.IsAutomated,
		"backfill must reclassify orphan-copied rows so they don't keep stale is_automated values")
}

// TestForceBackfillIsAutomatedRunsDespiteMatchingHash verifies
// that ForceBackfillIsAutomated reclassifies even when the
// stored hash matches the current classifier hash. This is the
// safety net ResyncAll relies on after CopyOrphanedDataFrom
// imports rows whose is_automated values were computed against
// the old DB but are now stamped under the temp DB's hash.
func TestForceBackfillIsAutomatedRunsDespiteMatchingHash(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()

	// Seed a single-turn roborev session that should be
	// is_automated = 1 under the current classifier.
	insertSession(t, d, "stuck", "proj", func(s *Session) {
		fm := "You are a code reviewer. Review the code."
		s.FirstMessage = &fm
		s.MessageCount = 3
		s.UserMessageCount = 1
	})

	// Force is_automated = 0 to simulate a row imported via
	// CopyOrphanedDataFrom from a DB whose classifier set was
	// stale at the time the flag was computed.
	_, err := d.getWriter().Exec(ctx,
		"UPDATE sessions SET is_automated = 0 WHERE id = 'stuck'",
	)
	require.NoError(t, err, "force stuck to 0")

	// Stamp the current hash so a *plain* backfill would
	// short-circuit (mirrors ResyncAll's temp DB state after
	// at-Open backfill ran on an empty table).
	_, err = d.getWriter().Exec(ctx,
		`INSERT INTO stats (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		ClassifierHashKey, ClassifierHash(),
	)
	require.NoError(t, err, "stamp current hash")

	// The force path must reclassify regardless of the stored
	// hash and re-stamp the current classifier hash.
	require.NoError(t, d.ForceBackfillIsAutomated(ctx), "force backfill")

	got, err := d.GetSession(ctx, "stuck")
	require.NoError(t, err, "get stuck after force")
	assert.True(t, got.IsAutomated,
		"ForceBackfillIsAutomated must flip stuck to is_automated=1")

	// And the hash must be re-stamped after the force run so
	// subsequent Opens don't re-do the work.
	var stored string
	err = d.getWriter().QueryRow(ctx,
		`SELECT value FROM stats WHERE key = ?`,
		ClassifierHashKey,
	).Scan(&stored)
	require.NoError(t, err, "read hash after force")
	assert.Equal(t, ClassifierHash(), stored,
		"stored hash not refreshed after force")
}

func TestAutomationIgnoresToolResultAsFirstPrompt(t *testing.T) {
	for _, automated := range []bool{false, true} {
		name := "interactive"
		prompt, result := "Explain this code.", "You are a code reviewer. Review the diff."
		if automated {
			name = "automated"
			prompt, result = result, prompt
		}
		t.Run(name, func(t *testing.T) {
			database := testDB(t)
			messages := []Message{
				{SessionID: "orphan", Ordinal: 0, Role: "user", Content: result, SourceSubtype: parser.SourceSubtypeToolResult},
				{SessionID: "orphan", Ordinal: 1, Role: "user", Content: prompt},
			}
			assert.Equal(t, automated, IsAutomatedTranscript(1, messages, nil))
			require.NoError(t, database.UpsertSession(t.Context(), Session{
				ID: "orphan", Agent: "codex", Project: "project", Machine: "local", UserMessageCount: 1,
			}))
			require.NoError(t, database.InsertMessages(t.Context(), messages))
			stored, err := database.GetSessionFull(t.Context(), "orphan")
			require.NoError(t, err)
			require.NotNil(t, stored)
			assert.Equal(t, automated, stored.IsAutomated, "write-time classification")
			require.NoError(t, database.ForceBackfillIsAutomated(t.Context()))
			stored, err = database.GetSessionFull(t.Context(), "orphan")
			require.NoError(t, err)
			require.NotNil(t, stored)
			assert.Equal(t, automated, stored.IsAutomated, "audit classification")
		})
	}
}

func TestSpawnedHeadlessWorkerStaysSubagentAfterParentDeletion(t *testing.T) {
	for _, link := range []string{"spawn edge", "launcher"} {
		t.Run(link, func(t *testing.T) {
			d := testDB(t)
			ctx := t.Context()
			worker := func(s *Session) {
				s.Agent = "codex"
				s.SessionKind = parser.SessionKindNonInteractive
				s.FirstMessage = Ptr(`Read the complete task prompt from "/tmp/prompt.md" and carry out its instructions.`)
				s.UserMessageCount = 1
			}
			insertSession(t, d, "worker", "project-a", worker)
			stored, err := d.GetSession(ctx, "worker")
			require.NoError(t, err)
			require.True(t, stored.IsAutomated)
			require.Empty(t, stored.RelationshipType, "an unlinked automated headless run counts as Automated")

			if link == "spawn edge" {
				_, err = d.WriteSessionBatchAtomic(ctx, []SessionBatchWrite{{
					Session:         Session{ID: "parent", Project: "project-a", Machine: defaultMachine, Agent: defaultAgent, MessageCount: 1},
					ReplaceMessages: true,
					Messages: []Message{{
						SessionID: "parent", Ordinal: 0, Role: "assistant", Content: "spawn", HasToolUse: true,
						ToolCalls: []ToolCall{{ToolName: "Agent", Category: "Task", SubagentSessionID: "worker"}},
					}},
				}})
				require.NoError(t, err)
				_, err = d.LinkSubagentSessionsContext(ctx)
				require.NoError(t, err)
			} else {
				insertSession(t, d, "parent", "project-a")
				_, err = d.SetSessionExternalParent(ctx, "worker", "parent")
				require.NoError(t, err)
			}
			stored, err = d.GetSession(ctx, "worker")
			require.NoError(t, err)
			require.Equal(t, Ptr("parent"), stored.ParentSessionID)
			require.Equal(t, "subagent", stored.RelationshipType)

			require.NoError(t, d.DeleteSession(ctx, "parent"))
			if link == "spawn edge" {
				_, err = d.getWriter().Exec(ctx, clearDanglingSubagentParentQuery("(?)"), "worker")
				require.NoError(t, err)
			}
			// A later sync rewrites the worker from its source, which has no parent.
			insertSession(t, d, "worker", "project-a", worker)
			stored, err = d.GetSession(ctx, "worker")
			require.NoError(t, err)
			assert.Nil(t, stored.ParentSessionID)
			assert.Equal(t, "subagent", stored.RelationshipType, "rewrite after parent deletion")

			_, err = d.getWriter().Exec(ctx, `UPDATE stats SET value = 'old-classifier' WHERE key = ?`, ClassifierHashKey)
			require.NoError(t, err)
			path := d.Path()
			require.NoError(t, d.Close())
			reopened, err := Open(ctx, path)
			require.NoError(t, err)
			defer reopened.Close()
			stored, err = reopened.GetSession(ctx, "worker")
			require.NoError(t, err)
			assert.True(t, stored.IsAutomated)
			assert.Equal(t, "subagent", stored.RelationshipType, "classifier change after parent deletion")
		})
	}
}

func TestAutomatedBackfillRefreshesSignals(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()

	// 1. Two-message prompt-file review stored before upgrade as non-automated
	//    with completed/medium outcome and a 100/A health score.
	reviewPrompt := `Read the complete task prompt from "/tmp/review.md" and follow all instructions.`
	insertSession(t, d, "review-to-auto", "project-a", func(s *Session) {
		s.Agent = "claude"
		s.Entrypoint = "sdk-cli"
		s.FirstMessage = Ptr(reviewPrompt)
		s.MessageCount = 2
		s.UserMessageCount = 1
	})
	require.NoError(t, d.InsertMessages(ctx, []Message{
		{SessionID: "review-to-auto", Ordinal: 0, Role: "user", Content: reviewPrompt},
		{SessionID: "review-to-auto", Ordinal: 1, Role: "assistant", Content: "Looks good."},
	}))
	_, err := d.getWriter().Exec(ctx,
		`UPDATE sessions
		 SET is_automated = 0,
		     outcome = 'completed',
		     outcome_confidence = 'medium',
		     ended_with_role = 'assistant',
		     health_score = 100,
		     health_grade = 'A',
		     quality_signal_version = ?
		 WHERE id = 'review-to-auto'`,
		CurrentQualitySignalVersion,
	)
	require.NoError(t, err)

	// 2. Two-message interactive session previously marked automated with
	//    unknown/low outcome and nil health.
	insertSession(t, d, "auto-to-human", "project-a", func(s *Session) {
		s.Agent = "codex"
		s.FirstMessage = Ptr("Explain this function.")
		s.MessageCount = 2
		s.UserMessageCount = 1
	})
	require.NoError(t, d.InsertMessages(ctx, []Message{
		{SessionID: "auto-to-human", Ordinal: 0, Role: "user", Content: "Explain this function."},
		{SessionID: "auto-to-human", Ordinal: 1, Role: "assistant", Content: "It adds two numbers."},
	}))
	_, err = d.getWriter().Exec(ctx,
		`UPDATE sessions
		 SET is_automated = 1,
		     outcome = 'unknown',
		     outcome_confidence = 'low',
		     ended_with_role = 'assistant',
		     health_score = NULL,
		     health_grade = NULL,
		     quality_signal_version = ?
		 WHERE id = 'auto-to-human'`,
		CurrentQualitySignalVersion,
	)
	require.NoError(t, err)

	// 3. Row with intentionally empty signals whose metadata flips is_automated
	//    to true must preserve empty signal fields.
	insertSession(t, d, "usage-only-empty", "project-a", func(s *Session) {
		s.Agent = "codex"
		s.SessionKind = parser.SessionKindRoborev
		s.MessageCount = 2
		s.UserMessageCount = 1
	})
	_, err = d.getWriter().Exec(ctx,
		`UPDATE sessions
		 SET is_automated = 0,
		     outcome = '',
		     outcome_confidence = '',
		     ended_with_role = '',
		     health_score = NULL,
		     health_grade = NULL,
		     quality_signal_version = ?
		 WHERE id = 'usage-only-empty'`,
		CurrentQualitySignalVersion,
	)
	require.NoError(t, err)

	_, err = d.getWriter().Exec(ctx, `UPDATE stats SET value = 'old-classifier' WHERE key = ?`, ClassifierHashKey)
	require.NoError(t, err)
	require.NoError(t, d.backfillIsAutomatedLocked(ctx, d.getWriter()))

	review, err := d.GetSessionFull(ctx, "review-to-auto")
	require.NoError(t, err)
	assert.True(t, review.IsAutomated)
	assert.Equal(t, "unknown", review.Outcome)
	assert.Equal(t, "low", review.OutcomeConfidence)
	assert.Nil(t, review.HealthScore)
	assert.Nil(t, review.HealthGrade)

	human, err := d.GetSessionFull(ctx, "auto-to-human")
	require.NoError(t, err)
	assert.False(t, human.IsAutomated)
	assert.Equal(t, "completed", human.Outcome)
	assert.Equal(t, "medium", human.OutcomeConfidence)
	require.NotNil(t, human.HealthScore)
	assert.Equal(t, 100, *human.HealthScore)
	require.NotNil(t, human.HealthGrade)
	assert.Equal(t, "A", *human.HealthGrade)

	usageOnly, err := d.GetSessionFull(ctx, "usage-only-empty")
	require.NoError(t, err)
	assert.True(t, usageOnly.IsAutomated)
	assert.Empty(t, usageOnly.Outcome)
	assert.Empty(t, usageOnly.OutcomeConfidence)
	assert.Nil(t, usageOnly.HealthScore)
	assert.Nil(t, usageOnly.HealthGrade)
}

func TestAutomatedBackfillSignalRefreshFailureRollsBackAndRetries(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()

	reviewPrompt := `Read the complete task prompt from "/tmp/review.md" and follow all instructions.`
	insertSession(t, d, "review-retry", "project-a", func(s *Session) {
		s.Agent = "claude"
		s.Entrypoint = "sdk-cli"
		s.FirstMessage = Ptr(reviewPrompt)
		s.MessageCount = 2
		s.UserMessageCount = 1
	})
	require.NoError(t, d.InsertMessages(ctx, []Message{
		{SessionID: "review-retry", Ordinal: 0, Role: "user", Content: reviewPrompt},
		{SessionID: "review-retry", Ordinal: 1, Role: "assistant", Content: "Looks good."},
	}))
	_, err := d.getWriter().Exec(ctx,
		`UPDATE sessions
		 SET is_automated = 0,
		     outcome = 'completed',
		     outcome_confidence = 'medium',
		     ended_with_role = 'assistant',
		     health_score = 100,
		     health_grade = 'A',
		     quality_signal_version = ?
		 WHERE id = 'review-retry'`,
		CurrentQualitySignalVersion,
	)
	require.NoError(t, err)
	_, err = d.getWriter().Exec(ctx, `UPDATE stats SET value = 'old-classifier' WHERE key = ?`, ClassifierHashKey)
	require.NoError(t, err)

	// Fail the signal refresh UPDATE once to simulate an interrupted startup
	// between flag write and signal refresh.
	_, err = d.getWriter().Exec(ctx, `CREATE TRIGGER fail_signal_refresh
		BEFORE UPDATE OF outcome ON sessions
		WHEN NEW.id = 'review-retry' AND NEW.outcome = 'unknown'
		BEGIN
			SELECT RAISE(ABORT, 'simulated signal refresh failure');
		END`)
	require.NoError(t, err)

	err = d.backfillIsAutomatedLocked(ctx, d.getWriter())
	require.Error(t, err)

	// Both is_automated and signals must have rolled back together.
	mid, err := d.GetSessionFull(ctx, "review-retry")
	require.NoError(t, err)
	assert.False(t, mid.IsAutomated)
	assert.Equal(t, "completed", mid.Outcome)

	_, err = d.getWriter().Exec(ctx, `DROP TRIGGER fail_signal_refresh`)
	require.NoError(t, err)

	require.NoError(t, d.backfillIsAutomatedLocked(ctx, d.getWriter()))
	after, err := d.GetSessionFull(ctx, "review-retry")
	require.NoError(t, err)
	assert.True(t, after.IsAutomated)
	assert.Equal(t, "unknown", after.Outcome)
	assert.Equal(t, "low", after.OutcomeConfidence)
	assert.Nil(t, after.HealthScore)
	assert.Nil(t, after.HealthGrade)
}
