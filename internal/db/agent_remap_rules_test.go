package db

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentRemapTargetEvaluator(t *testing.T) {
	sess := Session{ID: "goose:abc", Agent: "goose"}
	// First-match-by-ID wins; wildcard beats nothing when a specific rule
	// precedes it.
	rules := []AgentRemapRule{
		{ID: 1, SourceAgent: "goose", ModelGlob: "ossington-*",
			TargetAgent: "augure-desktop", Enabled: true},
		{ID: 2, SourceAgent: "goose", TargetAgent: "codex", Enabled: true},
	}
	assert.Equal(t, "augure-desktop",
		AgentRemapTarget(rules, sess, []string{"ossington-5"}))
	assert.Equal(t, "codex", AgentRemapTarget(rules, sess, []string{"gpt-5"}))
	assert.Equal(t, "codex", AgentRemapTarget(rules, sess, nil),
		"wildcard glob matches models-less sessions")

	// Disabled rules are skipped.
	assert.Equal(t, "goose", AgentRemapTarget([]AgentRemapRule{
		{ID: 1, SourceAgent: "goose", TargetAgent: "codex", Enabled: false},
	}, sess, nil))

	// Source agent must match.
	assert.Equal(t, "goose", AgentRemapTarget([]AgentRemapRule{
		{ID: 1, SourceAgent: "codex", TargetAgent: "augure", Enabled: true},
	}, sess, nil))

	// ID prefix must match.
	assert.Equal(t, "goose", AgentRemapTarget([]AgentRemapRule{
		{ID: 1, SourceAgent: "goose", IDPrefix: "codex:",
			TargetAgent: "augure", Enabled: true},
	}, sess, nil))
	assert.Equal(t, "augure", AgentRemapTarget([]AgentRemapRule{
		{ID: 1, SourceAgent: "goose", IDPrefix: "goose:",
			TargetAgent: "augure", Enabled: true},
	}, sess, nil))

	// Any-model semantics: the wildcard rule catches models that the
	// specific glob misses.
	assert.Equal(t, "codex", AgentRemapTarget(rules, sess,
		[]string{"mistral-large-latest", "rosedale-1"}))
	assert.Equal(t, "augure-desktop", AgentRemapTarget(rules, sess,
		[]string{"mistral-large-latest", "ossington-5"}))
}

func TestAgentRemapGlobSemantics(t *testing.T) {
	tests := []struct {
		glob   string
		models []string
		want   bool
	}{
		{glob: "ossington-*", models: []string{"ossington-5"}, want: true},
		{glob: "ossington-*", models: []string{"Ossington-5"}, want: false},
		{glob: "ossington-?", models: []string{"ossington-5"}, want: true},
		{glob: "ossington-?", models: []string{"ossington-55"}, want: false},
		{glob: "rosedale-[0-9]", models: []string{"rosedale-1"}, want: true},
		{glob: "rosedale-[0-9]", models: []string{"rosedale-x"}, want: false},
		{glob: "a[!b]c", models: []string{"abc"}, want: true},
		{glob: "a[!b]c", models: []string{"axc"}, want: false},
		{glob: "a[^b]c", models: []string{"abc"}, want: false},
		{glob: "a[^b]c", models: []string{"axc"}, want: true},
		{glob: "ossington-*|rosedale-*", models: []string{"ossington-5"}, want: true},
		{glob: "ossington-*|rosedale-*", models: []string{"rosedale-1"}, want: true},
		{glob: "ossington-*|rosedale-*", models: []string{"mistral"}, want: false},
		{glob: "tofino-* | ossington-*", models: []string{"tofino-2"}, want: true},
		{glob: "", models: nil, want: true},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want,
			agentRemapGlobMatches(tt.glob, tt.models),
			"glob %q vs %v", tt.glob, tt.models)
	}
}

func TestAgentRemapGlobSQLAgreement(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "goose:1", "p", func(s *Session) {
		s.Agent = "goose"
		s.StartedAt = new("2026-09-01T00:00:00Z")
	})
	insertMessages(t, d,
		userMsg("goose:1", 0, "hi"),
		Message{SessionID: "goose:1", Ordinal: 1, Role: "assistant",
			Model: "ossington-5"},
	)

	globs := []string{"ossington-*", "Ossington-*", "rosedale-*",
		"ossington-?", "tofino-*", "gpt-*", "",
		"ossington-*|rosedale-*", "ossington-*|Ossington-*",
		"zzz-* | ossington-?", "ossington-[^0]",
		"ossington-[!0]", "ossington-[!5]"}
	for _, glob := range globs {
		tx, err := d.getWriter().BeginTx(ctx, nil)
		require.NoError(t, err)
		eval, evalErr := evaluateAgentRemapTx(ctx, tx, []AgentRemapRule{{
			ID: 1, SourceAgent: "goose", ModelGlob: glob,
			TargetAgent: "augure", Enabled: true,
		}})
		require.NoError(t, evalErr)
		require.NoError(t, tx.Rollback())
		sqlMatch := eval.matched > 0

		goMatch := agentRemapGlobMatches(glob, []string{"ossington-5"})
		assert.Equalf(t, goMatch, sqlMatch,
			"go/sql glob disagreement for %q", glob)
	}
}

func TestAgentRemapRuleCRUD(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	created, err := d.CreateAgentRemapRule(ctx, AgentRemapRule{
		SourceAgent: "goose", ModelGlob: "ossington-*",
		TargetAgent: "augure-desktop", Enabled: true,
	})
	require.NoError(t, err)
	assert.NotZero(t, created.ID)
	assert.True(t, created.Enabled)

	// Upsert on the same identity updates instead of duplicating.
	again, err := d.CreateAgentRemapRule(ctx, AgentRemapRule{
		SourceAgent: "goose", ModelGlob: "ossington-*",
		TargetAgent: "augure", Enabled: false,
	})
	require.NoError(t, err)
	assert.Equal(t, created.ID, again.ID)
	assert.Equal(t, "augure", again.TargetAgent)
	assert.False(t, again.Enabled)

	// Update by id.
	updated, err := d.UpdateAgentRemapRule(ctx, AgentRemapRule{
		ID: created.ID, SourceAgent: "goose", ModelGlob: "ossington-*|rosedale-*",
		TargetAgent: "augure-desktop", Enabled: true,
	})
	require.NoError(t, err)
	assert.Equal(t, "ossington-*|rosedale-*", updated.ModelGlob)

	rules, err := d.ListAgentRemapRules(ctx)
	require.NoError(t, err)
	require.Len(t, rules, 1)

	require.NoError(t, d.DeleteAgentRemapRule(ctx, created.ID))
	rules, err = d.ListAgentRemapRules(ctx)
	require.NoError(t, err)
	assert.Empty(t, rules)

	// Validation errors.
	_, err = d.CreateAgentRemapRule(ctx, AgentRemapRule{
		SourceAgent: "", TargetAgent: "augure"})
	assert.Error(t, err)
	_, err = d.CreateAgentRemapRule(ctx, AgentRemapRule{
		SourceAgent: "goose", TargetAgent: "goose"})
	assert.Error(t, err)
	_, err = d.UpdateAgentRemapRule(ctx, AgentRemapRule{
		ID: 9999, SourceAgent: "goose", TargetAgent: "augure"})
	assert.ErrorIs(t, err, sql.ErrNoRows)
}

func TestAgentRemapPreviewAndApply(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	require.NoError(t, d.UpsertSession(Session{
		ID: "goose:dup", Project: "p", Machine: defaultMachine,
		Agent: "goose", MessageCount: 5,
		StartedAt: new("2026-09-01T00:00:00Z"),
	}))
	insertMessages(t, d,
		userMsg("goose:dup", 0, "hi"),
		Message{SessionID: "goose:dup", Ordinal: 1, Role: "assistant",
			Model: "ossington-5"},
	)
	require.NoError(t, d.UpsertSession(Session{
		ID: "goose:plain", Project: "p", Machine: defaultMachine,
		Agent: "goose", MessageCount: 1,
		StartedAt: new("2026-09-02T00:00:00Z"),
	}))

	_, err := d.CreateAgentRemapRule(ctx, AgentRemapRule{
		SourceAgent: "goose", ModelGlob: "ossington-*",
		TargetAgent: "augure-desktop", Enabled: true,
	})
	require.NoError(t, err)

	preview, err := d.PreviewAgentRemapRules(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, preview.Token)
	assert.Equal(t, 1, preview.MatchedSessions)
	require.Len(t, preview.Samples, 1)
	assert.Equal(t, "goose:dup", preview.Samples[0].ID)
	assert.Equal(t, "goose", preview.Samples[0].CurrentAgent)
	assert.Equal(t, "augure-desktop", preview.Samples[0].NextAgent)

	// Apply with wrong token fails.
	_, err = d.ApplyAgentRemapRules(ctx, "stale-token")
	require.ErrorIs(t, err, ErrAgentRemapRulesChanged)

	// Apply with the right token rewrites the agent and bumps
	// local_modified_at; the ID is preserved.
	result, err := d.ApplyAgentRemapRules(ctx, preview.Token)
	require.NoError(t, err)
	assert.Equal(t, 1, result.MatchedSessions)

	sess, err := d.GetSession(ctx, "goose:dup")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, "augure-desktop", sess.Agent)
	assert.True(t, strings.HasPrefix(sess.ID, "goose:"),
		"session ID must keep its source prefix")

	full, err := d.GetSessionFull(ctx, "goose:dup")
	require.NoError(t, err)
	require.NotNil(t, full)
	assert.NotNil(t, full.LocalModifiedAt,
		"apply must bump local_modified_at for mirror propagation")

	// Idempotent: second apply finds nothing.
	preview2, err := d.PreviewAgentRemapRules(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, preview2.MatchedSessions)

	// Session without matching models is untouched.
	sess2, err := d.GetSession(ctx, "goose:plain")
	require.NoError(t, err)
	assert.Equal(t, "goose", sess2.Agent)
}

func TestAgentRemapApplyToSession(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "codex:early", "p", func(s *Session) {
		s.Agent = "codex"
	})
	insertMessages(t, d,
		userMsg("codex:early", 0, "hi"),
		Message{SessionID: "codex:early", Ordinal: 1, Role: "assistant",
			Model: "rosedale-1"},
	)

	_, err := d.CreateAgentRemapRule(ctx, AgentRemapRule{
		SourceAgent: "codex", ModelGlob: "rosedale-*|tofino-*",
		TargetAgent: "augure", Enabled: true,
	})
	require.NoError(t, err)

	got, err := d.ApplyAgentRemapRulesToSession(ctx, "codex:early")
	require.NoError(t, err)
	assert.Equal(t, "augure", got)

	sess, err := d.GetSession(ctx, "codex:early")
	require.NoError(t, err)
	assert.Equal(t, "augure", sess.Agent)

	// No rules: fast-path no-op returns empty; the stored agent is
	// unchanged.
	require.NoError(t, d.DeleteAgentRemapRule(ctx, 1))
	got, err = d.ApplyAgentRemapRulesToSession(ctx, "codex:early")
	require.NoError(t, err)
	assert.Empty(t, got)
	sess, err = d.GetSession(ctx, "codex:early")
	require.NoError(t, err)
	assert.Equal(t, "augure", sess.Agent)
}

// seedRemapFixtures inserts two sessions matching rule.SourceAgent with
// message models, plus one non-matching session.
func seedRemapFixtures(t *testing.T, d *DB) {
	t.Helper()
	require.NoError(t, d.UpsertSession(Session{
		ID: "goose:a", Project: "p", Machine: defaultMachine,
		Agent: "goose", MessageCount: 2, SourceAgent: "goose",
		StartedAt: new("2026-09-01T00:00:00Z"),
	}))
	require.NoError(t, d.UpsertSession(Session{
		ID: "goose:b", Project: "p", Machine: defaultMachine,
		Agent: "goose", MessageCount: 2, SourceAgent: "goose",
		StartedAt: new("2026-09-02T00:00:00Z"),
	}))
	require.NoError(t, d.UpsertSession(Session{
		ID: "codex:c", Project: "p", Machine: defaultMachine,
		Agent: "codex", MessageCount: 1, SourceAgent: "codex",
	}))
	insertMessages(t, d,
		userMsg("goose:a", 0, "hi"),
		Message{SessionID: "goose:a", Ordinal: 1, Role: "assistant",
			Model: "ossington-5"},
		userMsg("goose:b", 0, "hi"),
		Message{SessionID: "goose:b", Ordinal: 1, Role: "assistant",
			Model: "rosedale-1"},
	)
}

func agentOf(t *testing.T, d *DB, id string) (string, string) {
	t.Helper()
	sess, err := d.GetSession(context.Background(), id)
	require.NoError(t, err, "GetSession %s", id)
	require.NotNil(t, sess)
	return sess.Agent, sess.SourceAgent
}

// TestAgentRemapForwardAndReverseBulk covers the preview/apply flow in both
// directions: goose -> augure forward, then the swapped rule augure ->
// goose reverses it. source_agent and session IDs must never move.
func TestAgentRemapForwardAndReverseBulk(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedRemapFixtures(t, d)

	forward, err := d.CreateAgentRemapRule(ctx, AgentRemapRule{
		SourceAgent: "goose", TargetAgent: "augure", Enabled: true,
	})
	require.NoError(t, err)

	// Forward apply.
	preview, err := d.PreviewAgentRemapRules(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, preview.MatchedSessions)
	_, err = d.ApplyAgentRemapRules(ctx, preview.Token)
	require.NoError(t, err)
	agent, source := agentOf(t, d, "goose:a")
	assert.Equal(t, "augure", agent, "forward agent")
	assert.Equal(t, "goose", source, "forward source_agent immutable")
	agent, _ = agentOf(t, d, "goose:b")
	assert.Equal(t, "augure", agent)
	agent, _ = agentOf(t, d, "codex:c")
	assert.Equal(t, "codex", agent, "non-match untouched")

	// Reverse: swap source and target on the same rule.
	_, err = d.UpdateAgentRemapRule(ctx, AgentRemapRule{
		ID: forward.ID, SourceAgent: "augure", TargetAgent: "goose",
		Enabled: true,
	})
	require.NoError(t, err)
	preview, err = d.PreviewAgentRemapRules(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, preview.MatchedSessions,
		"reverse must find the sessions forward remapped")
	_, err = d.ApplyAgentRemapRules(ctx, preview.Token)
	require.NoError(t, err)
	agent, source = agentOf(t, d, "goose:a")
	assert.Equal(t, "goose", agent, "reverse restored agent")
	assert.Equal(t, "goose", source, "reverse left source_agent alone")

	// Re-apply forward after a reverse: remapping must stay repeatable.
	_, err = d.UpdateAgentRemapRule(ctx, AgentRemapRule{
		ID: forward.ID, SourceAgent: "goose", TargetAgent: "augure",
		Enabled: true,
	})
	require.NoError(t, err)
	preview, err = d.PreviewAgentRemapRules(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, preview.MatchedSessions, "re-forward matches again")
	_, err = d.ApplyAgentRemapRules(ctx, preview.Token)
	require.NoError(t, err)
	agent, source = agentOf(t, d, "goose:a")
	assert.Equal(t, "augure", agent)
	assert.Equal(t, "goose", source)
}

// TestAgentRemapForwardAndReverseSingle covers the incremental
// single-session path in both directions.
func TestAgentRemapForwardAndReverseSingle(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedRemapFixtures(t, d)

	rule, err := d.CreateAgentRemapRule(ctx, AgentRemapRule{
		SourceAgent: "goose", TargetAgent: "augure", Enabled: true,
	})
	require.NoError(t, err)

	got, err := d.ApplyAgentRemapRulesToSession(ctx, "goose:a")
	require.NoError(t, err)
	assert.Equal(t, "augure", got)
	agent, source := agentOf(t, d, "goose:a")
	assert.Equal(t, "augure", agent)
	assert.Equal(t, "goose", source)

	// Idempotent re-run: already at target, no change.
	got, err = d.ApplyAgentRemapRulesToSession(ctx, "goose:a")
	require.NoError(t, err)
	assert.Equal(t, "augure", got)

	// Reverse by swapping the rule.
	_, err = d.UpdateAgentRemapRule(ctx, AgentRemapRule{
		ID: rule.ID, SourceAgent: "augure", TargetAgent: "goose",
		Enabled: true,
	})
	require.NoError(t, err)
	got, err = d.ApplyAgentRemapRulesToSession(ctx, "goose:a")
	require.NoError(t, err)
	assert.Equal(t, "goose", got, "reverse restored agent")
	agent, source = agentOf(t, d, "goose:a")
	assert.Equal(t, "goose", agent)
	assert.Equal(t, "goose", source)
}

// TestAgentRemapForwardAndReverseBatch covers the full-parse batch path in
// both directions, including the changed-row accounting: a session whose
// agent was concurrently rewritten must not be reported as remapped.
func TestAgentRemapForwardAndReverseBatch(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedRemapFixtures(t, d)

	rule, err := d.CreateAgentRemapRule(ctx, AgentRemapRule{
		SourceAgent: "goose", TargetAgent: "augure", Enabled: true,
	})
	require.NoError(t, err)

	matched, err := d.ApplyAgentRemapRulesToSessions(ctx,
		[]string{"goose:a", "goose:b"})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"goose:a", "goose:b"}, matched)
	agent, source := agentOf(t, d, "goose:a")
	assert.Equal(t, "augure", agent)
	assert.Equal(t, "goose", source)

	// Concurrent relabel: goose:a was already moved by someone else, so
	// the guard must skip it and it must not appear in matched.
	_, err = d.getWriter().Exec(
		`UPDATE sessions SET agent = 'codex' WHERE id = 'goose:a'`)
	require.NoError(t, err)
	_, err = d.UpdateAgentRemapRule(ctx, AgentRemapRule{
		ID: rule.ID, SourceAgent: "augure", TargetAgent: "goose",
		Enabled: true,
	})
	require.NoError(t, err)
	matched, err = d.ApplyAgentRemapRulesToSessions(ctx,
		[]string{"goose:a", "goose:b"})
	require.NoError(t, err)
	assert.Equal(t, []string{"goose:b"}, matched,
		"only the still-guarded session is reported")
	agent, _ = agentOf(t, d, "goose:a")
	assert.Equal(t, "codex", agent, "clobbered relabel preserved")
	agent, _ = agentOf(t, d, "goose:b")
	assert.Equal(t, "goose", agent, "reverse restored goose:b")
}

// TestAgentRemapSingleSessionReportsStoredAgentOnRace covers the guard-miss
// window: another writer rewrites the session's agent between the caller's
// read and the guarded remap write. The single-session path must report the
// agent actually stored — not the target it failed to write.
// TestAgentRemapGuardedHelperReportsStoredAgent covers the guard-miss
// branch of remapSessionAgentGuarded directly: a stale currentAgent makes
// the guarded UPDATE match nothing, and the helper must report the agent
// actually stored — or the empty string once the row is soft-deleted.
func TestAgentRemapGuardedHelperReportsStoredAgent(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedRemapFixtures(t, d)

	// Relabel goose:a behind the helper's back so its guard (built on the
	// stale agent 'goose') matches nothing.
	_, err := d.getWriter().Exec(
		`UPDATE sessions SET agent = 'codex' WHERE id = 'goose:a'`)
	require.NoError(t, err)

	tx, err := d.getWriter().BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	// Guard miss on a live row: report the stored agent.
	got, err := remapSessionAgentGuarded(ctx, tx, "goose:a", "goose", "augure")
	require.NoError(t, err)
	assert.Equal(t, "codex", got,
		"must report the stored agent, not the unwritten target")

	// Guard miss on a soft-deleted row: report nothing — the row counts
	// as gone for every caller's purposes.
	_, err = tx.Exec(
		`UPDATE sessions SET deleted_at = '2026-09-16T00:00:00Z'
		 WHERE id = 'goose:b'`)
	require.NoError(t, err)
	got, err = remapSessionAgentGuarded(ctx, tx, "goose:b", "goose", "augure")
	require.NoError(t, err)
	assert.Empty(t, got, "soft-deleted row must report empty")

	// The contended write still wins when the guard matches.
	got, err = remapSessionAgentGuarded(ctx, tx, "goose:a", "codex", "augure")
	require.NoError(t, err)
	assert.Equal(t, "augure", got)

	require.NoError(t, tx.Commit())

	agent, source := agentOf(t, d, "goose:a")
	assert.Equal(t, "augure", agent)
	assert.Equal(t, "goose", source, "source_agent untouched")
}

// TestAgentRemapSingleSessionSkipsWhenNoRuleMatches covers the public
// single-session path when a concurrent relabel moves the row before the
// call: the freshly-read agent matches no rule, so the no-match early
// return fires and the stored agent is reported unchanged.
func TestAgentRemapSingleSessionSkipsWhenNoRuleMatches(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedRemapFixtures(t, d)

	_, err := d.CreateAgentRemapRule(ctx, AgentRemapRule{
		SourceAgent: "goose", TargetAgent: "augure", Enabled: true,
	})
	require.NoError(t, err)

	// Relabel before the call: the apply reads codex as the current agent,
	// no rule matches codex, and the row keeps its codex label.
	_, err = d.getWriter().Exec(
		`UPDATE sessions SET agent = 'codex' WHERE id = 'goose:a'`)
	require.NoError(t, err)

	got, err := d.ApplyAgentRemapRulesToSession(ctx, "goose:a")
	require.NoError(t, err)
	assert.Equal(t, "codex", got,
		"must report the stored agent when no rule matches it")

	agent, source := agentOf(t, d, "goose:a")
	assert.Equal(t, "codex", agent, "relabel preserved")
	assert.Equal(t, "goose", source, "source_agent untouched")

	// The uncontended sibling still remaps normally.
	got, err = d.ApplyAgentRemapRulesToSession(ctx, "goose:b")
	require.NoError(t, err)
	assert.Equal(t, "augure", got)
}

// TestAgentRemapSingleSessionDeletedAfterRead covers the guard-miss variant
// where the row is soft-deleted between read and write: nothing is stored,
// so the path reports the empty string.
func TestAgentRemapSingleSessionDeletedAfterRead(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedRemapFixtures(t, d)

	_, err := d.CreateAgentRemapRule(ctx, AgentRemapRule{
		SourceAgent: "goose", TargetAgent: "augure", Enabled: true,
	})
	require.NoError(t, err)

	_, err = d.getWriter().Exec(
		`UPDATE sessions SET deleted_at = '2026-09-16T00:00:00Z'
		 WHERE id = 'goose:a'`)
	require.NoError(t, err)

	// GetSession filters deleted rows, so this returns early with no
	// match; the empty string means "nothing remapped".
	got, err := d.ApplyAgentRemapRulesToSession(ctx, "goose:a")
	require.NoError(t, err)
	assert.Empty(t, got, "deleted session must not report a remap")
}

// TestAgentRemapSingleSessionNoRules covers the fast-path returns: no
// rules, no enabled rules, and an unknown session all report the empty
// string without touching the row.
func TestAgentRemapSingleSessionNoRules(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedRemapFixtures(t, d)

	got, err := d.ApplyAgentRemapRulesToSession(ctx, "goose:a")
	require.NoError(t, err)
	assert.Empty(t, got, "no rules at all")

	_, err = d.CreateAgentRemapRule(ctx, AgentRemapRule{
		SourceAgent: "goose", TargetAgent: "augure", Enabled: false,
	})
	require.NoError(t, err)

	got, err = d.ApplyAgentRemapRulesToSession(ctx, "goose:a")
	require.NoError(t, err)
	assert.Empty(t, got, "rules exist but none enabled")

	got, err = d.ApplyAgentRemapRulesToSession(ctx, "missing:id")
	require.NoError(t, err)
	assert.Empty(t, got, "unknown session")
}
