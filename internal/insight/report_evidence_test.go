package insight

import (
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestReportEvidenceFiltersAmbientOpeners(t *testing.T) {
	placeholder := `<external_codex_apps_open_page>{"page_id":null}</external_codex_apps_open_page>`
	task := "Implement the API; only change the handler. Acceptance criteria: verify the tests pass."
	sessions := []db.Session{
		{ID: "real", Agent: "codex", FirstMessage: &task, UserMessageCount: 3},
		{ID: "mixed", Agent: "codex", FirstMessage: new(placeholder + "\n" + task), UserMessageCount: 3},
		{ID: "placeholder1", Agent: "codex", FirstMessage: &placeholder, UserMessageCount: 3},
		{ID: "placeholder2", Agent: "codex", FirstMessage: &placeholder, UserMessageCount: 3},
		{ID: "placeholder3", Agent: "codex", FirstMessage: &placeholder, UserMessageCount: 3},
		{ID: "automated", Agent: "codex", FirstMessage: &task, IsAutomated: true, UserMessageCount: 3},
	}
	got := BuildCannedCoachSummary(sessions)
	baseline := BuildCannedCoachSummary(sessions[:1])
	assert.Equal(t, baseline.PromptMaturity.Score, got.PromptMaturity.Score)
	assert.Equal(t, 2, got.SpecDriven.EligibleSessions)
	assert.Empty(t, got.WorkflowClusters, "excluded openers must not form a repeated human workflow")
	data, err := json.Marshal(got)
	require.NoError(t, err)
	var obj map[string]any
	require.NoError(t, json.Unmarshal(data, &obj))
	assert.Equal(t, float64(2), obj["scored_prompt_sessions"])
	assert.Equal(t, float64(4), obj["excluded_prompt_sessions"])
	assert.NotContains(t, string(data), "external_codex_apps_open_page")
}

func TestReportEvidenceEmptyPromptIsUnknown(t *testing.T) {
	got := BuildCannedCoachSummary([]db.Session{{ID: "empty"}})
	assert.Equal(t, "unknown", got.PromptMaturity.Grade)
	assert.Empty(t, got.PromptMaturity.IssueCounts)
}

func TestReportEvidencePreservesQuotedAndMalformedMarkers(t *testing.T) {
	for _, prompt := range []string{
		"Explain `<external_codex_apps_open_page>` in this log",
		"<external_codex_apps_open_page>not a complete envelope",
		"go",
	} {
		t.Run(prompt, func(t *testing.T) {
			got := BuildCannedCoachSummary([]db.Session{{ID: "real", Agent: "codex", FirstMessage: &prompt}})
			require.Len(t, got.PromptMaturity.SamplePrompts, 1)
			assert.Equal(t, prompt, got.PromptMaturity.SamplePrompts[0].Prompt)
		})
	}
}

func TestReportEvidenceContextCoverageReachesGenerator(t *testing.T) {
	sessions := []db.Session{
		{ID: "tokens", HasContextData: true},
		{ID: "pressure", HasContextData: true, ContextPressureMax: new(0.9)},
		{ID: "pressure-zero", ContextPressureMax: new(0.0)},
		{ID: "unknown"},
	}
	payload := CannedAggregatePayload{Kind: CannedWorkflowHygieneReview, Coach: BuildCannedCoachSummary(sessions)}
	prompt, err := BuildCannedPrompt(payload, "test-hash")
	require.NoError(t, err)
	_, body, ok := strings.Cut(prompt, "Aggregate payload JSON:\n")
	require.True(t, ok)
	var obj map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &obj))
	coach := obj["coach"].(map[string]any)
	assert.Equal(t, map[string]any{
		"session_count":                  float64(4),
		"sessions_with_context_tokens":   float64(2),
		"sessions_with_context_pressure": float64(2),
	}, coach["context_coverage"])
}

func TestReportEvidenceCoverageIsCitableAcrossTemplates(t *testing.T) {
	for kind := range cannedTemplates {
		t.Run(string(kind), func(t *testing.T) {
			payload := CannedAggregatePayload{Kind: kind, Coach: BuildCannedCoachSummary([]db.Session{{ID: "tokens", HasContextData: true}})}
			payload.EvidenceRefs = CannedEvidenceRefs(payload.Signals, nil, payload.Coach)
			ids := make([]string, 0, len(payload.EvidenceRefs))
			for _, ref := range payload.EvidenceRefs {
				ids = append(ids, ref.ID)
			}
			assert.Contains(t, ids, "coach:context_coverage")
			assert.Contains(t, ids, "coach:prompt_coverage")
			prompt, err := BuildCannedPrompt(payload, "hash")
			require.NoError(t, err)
			rules, _, found := strings.Cut(prompt, "Aggregate payload JSON:\n")
			require.True(t, found)
			assert.Contains(t, rules, "counts pressure measurements only")
			assert.Contains(t, rules, "unknown grade is missing evidence")
			assert.Contains(t, rules, "Short approvals can be appropriate")
		})
	}
}

func TestReportEvidenceCoverageChangesCacheIdentity(t *testing.T) {
	payload := CannedAggregatePayload{Kind: CannedWorkflowHygieneReview, Coach: BuildCannedCoachSummary([]db.Session{{ID: "session"}})}
	key := func() string {
		t.Helper()
		hash, err := CannedAggregateHash(payload)
		require.NoError(t, err)
		key, err := CannedCacheKey(payload.Kind, "", "", "", "claude", "", hash, "all", CannedSessionFilters{}, GenerateOptions{})
		require.NoError(t, err)
		return key
	}
	withoutTokens := key()
	payload.Coach.ContextCoverage.SessionsWithContextTokens = 1
	assert.NotEqual(t, withoutTokens, key())
	withTokens := key()
	payload.Coach.ContextCoverage.SessionsWithContextPressure = 1
	assert.NotEqual(t, withTokens, key())
}
