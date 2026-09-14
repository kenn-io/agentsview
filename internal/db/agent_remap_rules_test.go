package db

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func agentRemapTimePtr(s string) *string { return &s }

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
		{glob: "a[!b]c", models: []string{"abc"}, want: false},
		{glob: "a[!b]c", models: []string{"axc"}, want: true},
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
		s.StartedAt = agentRemapTimePtr("2026-09-01T00:00:00Z")
	})
	insertMessages(t, d,
		userMsg("goose:1", 0, "hi"),
		Message{SessionID: "goose:1", Ordinal: 1, Role: "assistant",
			Model: "ossington-5"},
	)

	globs := []string{"ossington-*", "Ossington-*", "rosedale-*",
		"ossington-?", "tofino-*", "gpt-*", "",
		"ossington-*|rosedale-*", "ossington-*|Ossington-*",
		"zzz-* | ossington-?"}
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
		StartedAt: agentRemapTimePtr("2026-09-01T00:00:00Z"),
	}))
	insertMessages(t, d,
		userMsg("goose:dup", 0, "hi"),
		Message{SessionID: "goose:dup", Ordinal: 1, Role: "assistant",
			Model: "ossington-5"},
	)
	require.NoError(t, d.UpsertSession(Session{
		ID: "goose:plain", Project: "p", Machine: defaultMachine,
		Agent: "goose", MessageCount: 1,
		StartedAt: agentRemapTimePtr("2026-09-02T00:00:00Z"),
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
