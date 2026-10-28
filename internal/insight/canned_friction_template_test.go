package insight

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testFrictionReviewPayload(revision int) CannedAggregatePayload {
	review := CannedFrictionReviewInput{
		Date:         "2025-01-15",
		Timezone:     "UTC",
		RulesVersion: "friction-v1",
		Revision:     revision,
		Summary: map[string]any{
			"schema_version":   float64(1),
			"sessions_scanned": float64(2),
			"errors":           float64(2),
			"frustrations":     float64(1),
			"interruptions":    float64(2),
			"p0_alerts":        map[string]any{"Bash": []any{"s1", "s2", "s3"}},
		},
		TopPatterns: []CannedFrictionPattern{{
			Ref: "friction:pattern:01", Fingerprint: "fl1:bb", Kind: "error",
			Title: "[friction/error] Bash: exit status 1", DigestOccurrences: 2,
			DigestSessions: 2, FirstSeenDate: "2025-01-10", Recurring: true,
		}},
		P0Alerts: []CannedFrictionP0{{Tool: "Bash", Sessions: 3}},
	}
	payload := CannedAggregatePayload{
		Kind:           CannedFrictionReview,
		DateFrom:       "2025-01-15",
		DateTo:         "2025-01-15",
		AutomatedScope: "all",
		Filters:        CannedSessionFilters{Timezone: "UTC", IncludeOneShot: true, AutomatedScope: "all"},
		Friction:       &review,
	}
	payload.EvidenceRefs = CannedFrictionEvidenceRefs(review)
	return payload
}

func TestCannedAggregateHashOmitsNilFriction(t *testing.T) {
	payload := CannedAggregatePayload{
		Kind:     CannedPromptMaturityReview,
		DateFrom: "2025-01-15",
		DateTo:   "2025-01-15",
		EvidenceRefs: []CannedEvidenceRef{
			{ID: "signals:score_distribution", Description: "scores"},
		},
	}
	data, err := canonicalJSON(payload)
	require.NoError(t, err)
	assert.NotContains(t, string(data), `"friction"`)
	sum := sha256.Sum256(data)
	hash, err := CannedAggregateHash(payload)
	require.NoError(t, err)
	assert.Equal(t, hex.EncodeToString(sum[:]), hash,
		"session-signal templates must hash the full payload exactly as before")
}

func TestBuildCannedPromptFrictionReview(t *testing.T) {
	payload := testFrictionReviewPayload(1)
	hash, err := CannedAggregateHash(payload)
	require.NoError(t, err)
	prompt, err := BuildCannedPrompt(payload, hash)
	require.NoError(t, err)
	for _, want := range []string{
		"Template ID: friction_review",
		"Friction review template rules:",
		"Frustration pattern titles are withheld",
		`"interruptions":2`,
		"Do not propose filing, closing, reopening, or editing tracker issues",
		"- friction:pattern:01",
		"- friction:p0_alerts",
		`"top_patterns"`,
		"[friction/error] Bash: exit status 1",
		hash,
	} {
		assert.Contains(t, prompt, want)
	}
	for _, notWant := range []string{`"signals"`, `"coach"`, `"usage"`, `"filters"`} {
		assert.NotContains(t, prompt, notWant,
			"friction_review must not send empty session aggregates to the model")
	}
}

func TestRenderCannedMarkdownFrictionBoundary(t *testing.T) {
	env := CannedRecommendationEnvelope{
		SchemaVersion: CannedSchemaVersion, Kind: CannedFrictionReview,
		Summary: "Bash failures dominate.", Confidence: "medium",
		Recommendations: []CannedRecommendation{{Title: "t", Rationale: "r", Actions: []string{"a"}, EvidenceRefs: []string{"friction:summary"}, Impact: "low", Effort: "low"}},
	}
	tests := []struct {
		name    string
		kind    CannedKind
		want    string
		notWant string
	}{
		{
			name: "friction_review", kind: CannedFrictionReview,
			want:    "> Model-written summary of the Friction Log digest for 2025-01-15. Findings, digests, and tracker issues were not modified.",
			notWant: "Deterministic health scores and signal rows were not modified.",
		},
		{
			name: "session_signal_template_unchanged", kind: CannedToolReliabilityReview,
			want:    "> Generated recommendation text. Deterministic health scores and signal rows were not modified.",
			notWant: "Friction Log digest",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := env
			e.Kind = tt.kind
			md := RenderCannedMarkdown(e, CannedProvenance{TemplateID: string(tt.kind), DateFrom: "2025-01-15"})
			assert.True(t, strings.HasPrefix(md, "# AI-generated recommendation\n\n"))
			assert.Contains(t, md, tt.want)
			assert.NotContains(t, md, tt.notWant)
		})
	}
}

func TestNewCannedProvenanceFrictionSourceVersions(t *testing.T) {
	prov, err := NewCannedProvenance(testFrictionReviewPayload(3), "hash", "key", "fresh",
		"claude", "model", time.Date(2025, 1, 16, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"friction_rules":           "friction-v1",
		"friction_summary_schema":  "1",
		"friction_digest_revision": "3",
	}, prov.SourceVersions)
	assert.Equal(t, "friction_review", prov.TemplateID)
	assert.Equal(t, "2025-01-15", prov.DateFrom)
}
