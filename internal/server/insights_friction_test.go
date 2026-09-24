package server_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/insight"
	"go.kenn.io/agentsview/internal/server"
)

const (
	frictionReviewDate     = "2025-01-15"
	frictionReviewEarlier  = "2025-01-10"
	frictionReviewTextMark = "RAW-TEXT-MARKER"
	frictionReviewEvidMark = "RAW-EVIDENCE-MARKER"
	frictionErrTitle       = "[friction/error] Bash: exit status 1"
	frictionWkTitle        = "[friction/workaround] for now: hardcode the port"
	frictionPatTitle       = "[friction/pattern] fr-2: retry loop: `Bash` called 4 times with identical arguments"
	frictionFrustMark      = "FRUSTRATION-RAW"
	frictionFrustTitle     = "[friction/frustration] fr-1: FRUSTRATION-RAW this is still broken"
	frictionIntTitle       = "[friction/interruption] fr-2"
	frictionReviewSummary  = `{
  "corrections": 0,
  "created_issues": [],
  "deferrals": 0,
  "digest_path": "friction:2025-01-15",
  "errors": 2,
  "frustrations": 1,
  "interruptions": 2,
  "p0_alerts": {
    "Bash": [
      "fr-1",
      "fr-2",
      "fr-3"
    ]
  },
  "patterns": 1,
  "schema_version": 3,
  "sessions_scanned": 2,
  "spend": null,
  "tracker_failures": 0,
  "workarounds": 1
}
`
)

func frictionTestFingerprint(title string) string {
	sum := sha256.Sum256([]byte(title))
	return "fl1:" + hex.EncodeToString(sum[:])
}

func frictionTestFinding(session, kind, detector, title string, seq int) db.FrictionFinding {
	ordinal := seq + 1
	return db.FrictionFinding{
		SessionID:      session,
		Kind:           kind,
		Detector:       detector,
		MessageOrdinal: &ordinal,
		ToolName:       "Bash",
		Text:           frictionReviewTextMark + " " + session,
		Evidence:       frictionReviewEvidMark,
		Title:          title,
		Fingerprint:    frictionTestFingerprint(title),
		Seq:            seq,
		RulesVersion:   friction.RulesVersion,
	}
}

func frictionTestDigest(date, summary string, revision int) db.FrictionDigest {
	markdown := []byte("# Friction Log — " + date + "\n")
	sum := sha256.Sum256(markdown)
	return db.FrictionDigest{
		Date:            date,
		Timezone:        "UTC",
		RulesVersion:    friction.RulesVersion,
		BuiltAt:         time.Date(2025, 1, 16, 1, 0, 0, 0, time.UTC),
		Revision:        revision,
		SessionsScanned: 2,
		SnapshotJSON:    []byte(`{}`),
		SummaryJSON:     []byte(summary),
		Markdown:        markdown,
		MarkdownSHA256:  hex.EncodeToString(sum[:]),
		RunID:           "01943a6e-0000-7000-8000-000000000001",
	}
}

// seedFrictionReviewDigest writes an earlier digest (so the Bash error is
// recurring) and the 2025-01-15 digest with two sessions and five patterns,
// including one frustration and one interruption pattern (spec §6.8).
func seedFrictionReviewDigest(t *testing.T, te *testEnv) {
	t.Helper()
	ctx := t.Context()
	te.seedSession(t, "fr-1", "my-app", 4)
	te.seedSession(t, "fr-2", "my-app", 4)
	require.NoError(t, te.db.ReplaceSessionFriction(ctx, "fr-1", []db.FrictionFinding{
		frictionTestFinding("fr-1", "error", "error", frictionErrTitle, 0),
		frictionTestFinding("fr-1", "workaround", "workaround", frictionWkTitle, 1),
		frictionTestFinding("fr-1", "frustration", "frustration", frictionFrustTitle, 2),
	}, nil, friction.RulesVersion, "hash-fr-1"))
	require.NoError(t, te.db.ReplaceSessionFriction(ctx, "fr-2", []db.FrictionFinding{
		frictionTestFinding("fr-2", "error", "error", frictionErrTitle, 0),
		frictionTestFinding("fr-2", "pattern", "pattern.retry_loop", frictionPatTitle, 1),
		frictionTestFinding("fr-2", "interruption", "interruption", frictionIntTitle, 2),
		frictionTestFinding("fr-2", "interruption", "interruption", frictionIntTitle, 3),
	}, nil, friction.RulesVersion, "hash-fr-2"))

	require.NoError(t, te.db.SaveFrictionDigest(ctx,
		frictionTestDigest(frictionReviewEarlier, frictionReviewSummary, 1),
		nil,
		[]db.FrictionPatternUpdate{{
			Fingerprint: frictionTestFingerprint(frictionErrTitle), Kind: "error",
			Title: frictionErrTitle, Date: frictionReviewEarlier, SubjectID: "fr-old", Occurrences: 1,
		}},
	))
	updates := []db.FrictionPatternUpdate{
		{Fingerprint: frictionTestFingerprint(frictionErrTitle), Kind: "error", Title: frictionErrTitle, Date: frictionReviewDate, SubjectID: "fr-2", Occurrences: 2},
		{Fingerprint: frictionTestFingerprint(frictionWkTitle), Kind: "workaround", Title: frictionWkTitle, Date: frictionReviewDate, SubjectID: "fr-1", Occurrences: 1},
		{Fingerprint: frictionTestFingerprint(frictionPatTitle), Kind: "pattern", Title: frictionPatTitle, Date: frictionReviewDate, SubjectID: "fr-2", Occurrences: 1},
		{Fingerprint: frictionTestFingerprint(frictionFrustTitle), Kind: "frustration", Title: frictionFrustTitle, Date: frictionReviewDate, SubjectID: "fr-1", Occurrences: 1},
		{Fingerprint: frictionTestFingerprint(frictionIntTitle), Kind: "interruption", Title: frictionIntTitle, Date: frictionReviewDate, SubjectID: "fr-2", Occurrences: 2},
	}
	require.NoError(t, te.db.SaveFrictionDigest(ctx,
		frictionTestDigest(frictionReviewDate, frictionReviewSummary, 1),
		[]db.FrictionDigestSubject{
			{SubjectID: "fr-1", Date: frictionReviewDate, SubjectKind: "session"},
			{SubjectID: "fr-2", Date: frictionReviewDate, SubjectKind: "session"},
		},
		updates,
	))
}

const frictionReviewEnvelope = `{
	"schema_version":"llm_insight.v1",
	"kind":"friction_review",
	"summary":"Bash failures dominate this digest and recur from earlier days.",
	"confidence":"medium",
	"recommendations":[{
		"title":"Stabilize the shell environment",
		"rationale":"The top pattern is a recurring Bash error in both sessions.",
		"actions":["Pin the toolchain the failing command uses"],
		"evidence_refs":["friction:pattern:01","friction:p0_alerts"],
		"impact":"medium",
		"effort":"low"
	}],
	"risks":[],
	"evidence_refs":["friction:summary"]
}`

const frictionReviewRequest = `{"type":"llm_canned","kind":"friction_review","llm_opt_in":true,"agent":"claude","date_from":"2025-01-15","date_to":"2025-01-15"}`

func newFrictionReviewEnv(t *testing.T, calls *atomic.Int32, prompt *string) *testEnv {
	t.Helper()
	te := setupWithServerOpts(t, []server.Option{
		server.WithGenerateFunc(func(_ context.Context, _ string, p string) (insight.Result, error) {
			calls.Add(1)
			if prompt != nil {
				*prompt = p
			}
			return insight.Result{Agent: "claude", Model: "test-model", Content: frictionReviewEnvelope}, nil
		}),
	})
	seedFrictionReviewDigest(t, te)
	return te
}

func doneInsight(t *testing.T, body string) (db.Insight, []SSEEvent) {
	t.Helper()
	events := parseSSE(body)
	require.NotEmpty(t, events)
	last := events[len(events)-1]
	require.Equal(t, "done", last.Event, body)
	var saved db.Insight
	require.NoError(t, json.Unmarshal([]byte(last.Data), &saved))
	return saved, events
}

func TestGenerateFrictionReviewInsight_SavesAndCaches(t *testing.T) {
	var calls atomic.Int32
	var prompt string
	te := newFrictionReviewEnv(t, &calls, &prompt)

	w := te.post(t, "/api/v1/insights/generate", frictionReviewRequest)
	assertStatus(t, w, http.StatusOK)
	saved, _ := doneInsight(t, w.Body.String())

	assert.Equal(t, insight.CannedType, saved.Type)
	assert.Equal(t, "friction_review", saved.Kind)
	assert.Equal(t, frictionReviewDate, saved.DateFrom)
	assert.Equal(t, frictionReviewDate, saved.DateTo)
	assert.Equal(t, "fresh", saved.CacheStatus)
	assert.Contains(t, saved.Content,
		"Model-written summary of the Friction Log digest for 2025-01-15.")
	assert.Contains(t, saved.ProvenanceJSON, `"friction_digest_revision":"1"`)

	for _, want := range []string{
		"Template ID: friction_review",
		frictionErrTitle,
		`"recurring":true`,
		`"digest_occurrences":2`,
		`"frustrations":1`,
		`"interruptions":2`,
		`"kind":"frustration"`,
		`"kind":"interruption"`,
		frictionIntTitle,
		"- friction:pattern:01",
		"- friction:p0_alerts",
	} {
		assert.Contains(t, prompt, want)
	}
	for _, notWant := range []string{frictionReviewTextMark, frictionReviewEvidMark, frictionFrustMark, `"signals"`} {
		assert.NotContains(t, prompt, notWant)
	}

	w = te.post(t, "/api/v1/insights/generate", frictionReviewRequest)
	assertStatus(t, w, http.StatusOK)
	cached, events := doneInsight(t, w.Body.String())
	assert.Equal(t, int32(1), calls.Load(), "same digest must be a cache hit")
	assert.Equal(t, "hit", cached.CacheStatus)
	assert.Equal(t, saved.ID, cached.ID)
	sawHit := false
	for _, ev := range events {
		if ev.Event == "status" && ev.Data == `{"phase":"cache_hit"}` {
			sawHit = true
		}
	}
	assert.True(t, sawHit, "expected cache_hit status event")
}

type frictionStateSnapshot struct {
	Digest   db.FrictionDigest
	Digests  []db.FrictionDigest
	Patterns []db.FrictionPattern
	Findings []db.FrictionFinding
}

func snapshotFrictionState(t *testing.T, te *testEnv) frictionStateSnapshot {
	t.Helper()
	ctx := t.Context()
	digest, err := te.db.GetFrictionDigest(ctx, frictionReviewDate)
	require.NoError(t, err)
	require.NotNil(t, digest)
	digests, err := te.db.ListFrictionDigests(ctx, "2025-01-01", "2025-12-31")
	require.NoError(t, err)
	patterns, _, err := te.db.ListFrictionPatterns(ctx, db.FrictionPatternFilter{Limit: 1000})
	require.NoError(t, err)
	findings, _, err := te.db.ListFrictionFindings(ctx, db.FrictionFindingFilter{Limit: 1000})
	require.NoError(t, err)
	return frictionStateSnapshot{Digest: *digest, Digests: digests, Patterns: patterns, Findings: findings}
}

func TestGenerateFrictionReviewInsight_WritesNoFrictionState(t *testing.T) {
	var calls atomic.Int32
	te := newFrictionReviewEnv(t, &calls, nil)
	before := snapshotFrictionState(t, te)
	insightsBefore, err := te.db.ListInsights(t.Context(), db.InsightFilter{})
	require.NoError(t, err)

	w := te.post(t, "/api/v1/insights/generate", frictionReviewRequest)
	assertStatus(t, w, http.StatusOK)
	doneInsight(t, w.Body.String())

	assert.Equal(t, before, snapshotFrictionState(t, te),
		"generation must not write friction_findings, friction_digests or friction_patterns")
	insightsAfter, err := te.db.ListInsights(t.Context(), db.InsightFilter{})
	require.NoError(t, err)
	assert.Len(t, insightsAfter, len(insightsBefore)+1, "only the insights table gains a row")
}

func TestGenerateFrictionReviewInsight_RerenderedDigestMissesCache(t *testing.T) {
	var calls atomic.Int32
	te := newFrictionReviewEnv(t, &calls, nil)

	w := te.post(t, "/api/v1/insights/generate", frictionReviewRequest)
	assertStatus(t, w, http.StatusOK)
	doneInsight(t, w.Body.String())

	relinked := `{"corrections":0,"created_issues":[{"backend":"kata","id":"#7","title":"` +
		frictionErrTitle + `","url":""}],"deferrals":0,"digest_path":"friction:2025-01-15","errors":2,"frustrations":1,"interruptions":2,"p0_alerts":{"Bash":["fr-1","fr-2","fr-3"]},"patterns":1,"schema_version":3,"sessions_scanned":2,"spend":null,"tracker_failures":0,"workarounds":1}` + "\n"
	require.NoError(t, te.db.UpdateFrictionDigestRender(t.Context(), frictionReviewDate,
		[]byte("# Friction Log — 2025-01-15\n\nrerendered\n"), []byte(relinked), 2))

	w = te.post(t, "/api/v1/insights/generate", frictionReviewRequest)
	assertStatus(t, w, http.StatusOK)
	fresh, _ := doneInsight(t, w.Body.String())
	assert.Equal(t, int32(2), calls.Load(), "a re-rendered digest must not reuse the old summary")
	assert.Equal(t, "fresh", fresh.CacheStatus)
	assert.Contains(t, fresh.ProvenanceJSON, `"friction_digest_revision":"2"`)
}

func TestGenerateFrictionReviewInsight_Validation(t *testing.T) {
	var calls atomic.Int32
	te := newFrictionReviewEnv(t, &calls, nil)
	tests := []struct {
		name       string
		payload    string
		wantStatus int
		wantBody   string
	}{
		{"RequiresOptIn", `{"type":"llm_canned","kind":"friction_review","date_from":"2025-01-15","date_to":"2025-01-15"}`, http.StatusBadRequest, "llm_opt_in"},
		{"RejectsRange", `{"type":"llm_canned","kind":"friction_review","llm_opt_in":true,"date_from":"2025-01-14","date_to":"2025-01-15"}`, http.StatusBadRequest, "date_from must equal date_to"},
		{"RejectsProject", `{"type":"llm_canned","kind":"friction_review","llm_opt_in":true,"project":"my-app","date_from":"2025-01-15","date_to":"2025-01-15"}`, http.StatusBadRequest, "does not accept project or session filters"},
		{"RejectsSessionID", `{"type":"llm_canned","kind":"friction_review","llm_opt_in":true,"session_id":"fr-1","date_from":"2025-01-15","date_to":"2025-01-15"}`, http.StatusBadRequest, "session_id is only supported for agent_analysis"},
		{"RejectsAutomatedScope", `{"type":"llm_canned","kind":"friction_review","llm_opt_in":true,"automated_scope":"human","date_from":"2025-01-15","date_to":"2025-01-15"}`, http.StatusBadRequest, "does not accept project or session filters"},
		{"RejectsTimezone", `{"type":"llm_canned","kind":"friction_review","llm_opt_in":true,"timezone":"America/New_York","date_from":"2025-01-15","date_to":"2025-01-15"}`, http.StatusBadRequest, "does not accept project or session filters"},
		{"RejectsFilters", `{"type":"llm_canned","kind":"friction_review","llm_opt_in":true,"filters":{"agent":"claude"},"date_from":"2025-01-15","date_to":"2025-01-15"}`, http.StatusBadRequest, "does not accept project or session filters"},
		{"MissingDigest", `{"type":"llm_canned","kind":"friction_review","llm_opt_in":true,"date_from":"2025-01-20","date_to":"2025-01-20"}`, http.StatusNotFound, "friction digest not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := te.post(t, "/api/v1/insights/generate", tt.payload)
			assertStatus(t, w, tt.wantStatus)
			assertBodyContains(t, w, tt.wantBody)
		})
	}
	assert.Equal(t, int32(0), calls.Load(), "rejected requests never reach the model")
}
