package server_test

import (
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// TestSessionUsagePlanOnlyFlagsFalse checks that a codex-cloud session
// with only plan usage reports no token data and no cost, and carries its
// plan figures and capture coverage in separate objects, with and without
// sub-agent folding.
func TestSessionUsagePlanOnlyFlagsFalse(t *testing.T) {
	te := setup(t)
	const id = "codex-cloud:thread-plan"
	te.seedSession(t, id, "my-project", 2, func(s *db.Session) {
		s.Agent = "codex-cloud"
	})
	te.seedMessages(t, id, 2, func(i int, m *db.Message) {
		if i == 1 {
			m.Role = "assistant"
			m.Model = "gpt-5-codex"
		}
	})
	// A sub-agent thread makes ?subagents=true combine usage across
	// sessions; the root's plan usage still describes the result.
	te.seedSession(t, id+"-child", "my-project", 1, func(s *db.Session) {
		s.Agent = "codex-cloud"
		parent := id
		s.ParentSessionID = &parent
		s.RelationshipType = "subagent"
	})
	require.NoError(t, te.db.ReplaceSessionPlanUsage(t.Context(), id, &db.SessionPlanUsage{
		HasPlanUsage: true, DataAsOf: "2026-10-02T09:30:00Z", DataStatus: "available",
		UsageSource: "wham", WeeklyPercent: new(2.25), Credits: new("0"),
		Groups: []db.SessionPlanUsageGroup{{
			Model: "gpt-5-codex", ReasoningEffort: "high", Speed: "standard",
			ProductExperience: "codex", WeeklyPercent: new(2.25), Credits: new("0"),
		}},
		Final: true, HasCoverage: true, LastCaptureAt: "2026-10-01T12:30:00Z",
		UncapturedTurns: 2,
	}))

	for _, path := range []string{
		"/api/v1/sessions/" + id + "/usage",
		"/api/v1/sessions/" + id + "/usage?subagents=true",
	} {
		w := te.get(t, path)
		assertStatus(t, w, http.StatusOK)
		var got map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
		assert.Equal(t, false, got["has_token_data"], path)
		assert.Equal(t, false, got["has_cost"], path)
		assert.Equal(t, map[string]any{
			"data_as_of": "2026-10-02T09:30:00Z", "data_status": "available",
			"usage_source": "wham", "weekly_percent": 2.25, "credits": "0",
			"groups": []any{map[string]any{
				"model": "gpt-5-codex", "reasoning_effort": "high", "speed": "standard",
				"product_experience": "codex", "weekly_percent": 2.25,
				"five_hour_percent": nil, "credits": "0",
			}},
			"final": true,
		}, got["plan_usage"], path)
		assert.Equal(t, map[string]any{
			"last_capture_at": "2026-10-01T12:30:00Z", "uncaptured_turns": float64(2),
		}, got["capture_coverage"], path)
	}
}
