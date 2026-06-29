package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/insight"
	"go.kenn.io/agentsview/internal/server"
)

type listInsightsResponse struct {
	Insights []db.Insight `json:"insights"`
}

type failFirstWriteRecorder struct {
	header  http.Header
	writes  int
	status  int
	flushed bool
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(
	req *http.Request,
) (*http.Response, error) {
	return f(req)
}

type readOnlyInsightPersistStore struct {
	db.Store
	insertCalls int
}

func fastInsightLogDrainTimeouts() server.Option {
	return server.WithInsightLogDrainTimeouts(
		50*time.Millisecond,
		150*time.Millisecond,
	)
}

func (s *readOnlyInsightPersistStore) ReadOnly() bool { return true }

func (s *readOnlyInsightPersistStore) InsightGenerationAvailable() bool {
	return true
}

func (s *readOnlyInsightPersistStore) InsertInsight(
	insight db.Insight,
) (int64, error) {
	s.insertCalls++
	return s.Store.InsertInsight(insight)
}

func newFailFirstWriteRecorder() *failFirstWriteRecorder {
	return &failFirstWriteRecorder{
		header: make(http.Header),
		status: http.StatusOK,
	}
}

func (f *failFirstWriteRecorder) Header() http.Header {
	return f.header
}

func (f *failFirstWriteRecorder) WriteHeader(statusCode int) {
	f.status = statusCode
}

func (f *failFirstWriteRecorder) Write(b []byte) (int, error) {
	f.writes++
	if f.writes == 1 {
		return 0, io.ErrClosedPipe
	}
	return len(b), nil
}

func (f *failFirstWriteRecorder) Flush() {
	f.flushed = true
}

func TestListInsights(t *testing.T) {
	assertList := func(
		t *testing.T,
		te *testEnv,
		path string,
		wantStatus int,
		wantCount int,
		wantBody string,
	) {
		t.Helper()
		w := te.get(t, path)
		assertStatus(t, w, wantStatus)

		if wantBody != "" {
			assertBodyContains(t, w, wantBody)
		}

		if wantStatus == http.StatusOK {
			r := decode[listInsightsResponse](t, w)
			require.Len(t, r.Insights, wantCount)
		}
	}

	t.Run("Empty", func(t *testing.T) {
		te := setup(t)
		assertList(t, te, "/api/v1/insights", http.StatusOK, 0, "")
	})

	t.Run("FiltersAndInvalidRequests", func(t *testing.T) {
		te := setup(t)
		te.seedInsight(t, "daily_activity", "2025-01-15", new("my-app"))
		te.seedInsight(t, "agent_analysis", "2025-01-15", nil)

		t.Run("TypeFilter", func(t *testing.T) {
			assertList(t, te, "/api/v1/insights?type=daily_activity",
				http.StatusOK, 1, "")
		})

		te.seedInsight(t, "daily_activity", "2025-01-15", new("other-app"))
		t.Run("WithData", func(t *testing.T) {
			assertList(t, te, "/api/v1/insights", http.StatusOK, 3, "")
		})

		t.Run("InvalidType", func(t *testing.T) {
			assertList(t, te, "/api/v1/insights?type=invalid",
				http.StatusBadRequest, 0, "invalid type")
		})

		t.Run("ReversedDateRange", func(t *testing.T) {
			assertList(t, te,
				"/api/v1/insights?date_from=2026-06-17&date_to=2026-06-16",
				http.StatusBadRequest, 0,
				"date_from must not be after date_to")
		})

		t.Run("InvalidDateFrom", func(t *testing.T) {
			// A non-date value is rejected with 400 before the handler
			// runs (huma's format:"date" query validation), so only the
			// status is asserted; the body message is owned by the
			// framework.
			assertList(t, te, "/api/v1/insights?date_from=not-a-date",
				http.StatusBadRequest, 0, "")
		})
	})

	t.Run("ReturnsAll", func(t *testing.T) {
		te := setup(t)
		te.seedInsight(t, "daily_activity", "2025-01-15", new("my-app"))
		te.seedInsight(t, "daily_activity", "2025-01-16", new("my-app"))
		assertList(t, te, "/api/v1/insights", http.StatusOK, 2, "")
	})
}

func TestGetInsight_Found(t *testing.T) {
	te := setup(t)

	id := te.seedInsight(t, "daily_activity", "2025-01-15",
		new("my-app"))

	w := te.get(t, fmt.Sprintf("/api/v1/insights/%d", id))
	assertStatus(t, w, http.StatusOK)

	r := decode[db.Insight](t, w)
	require.Equal(t, id, r.ID)
	assert.Equal(t, "daily_activity", r.Type)
}

func TestInsightExportHTML(t *testing.T) {
	te := setup(t)
	id := te.seedInsight(t, "daily_activity", "2025-01-15", new("my-app"),
		func(insight *db.Insight) {
			insight.Content = "# Insight\n\n- published finding"
		},
	)

	w := te.get(t, fmt.Sprintf("/api/v1/insights/%d/export", id))
	assertStatus(t, w, http.StatusOK)
	assert.Contains(t, w.Header().Get("Content-Type"), "text/html")
	assert.Contains(t, w.Header().Get("Content-Disposition"), "attachment")
	assert.Contains(t, w.Header().Get("Content-Disposition"), ".html")
	assertBodyContains(t, w, "Daily Activity Insight")
	assertBodyContains(t, w, "# Insight")
	assertBodyContains(t, w, "my-app")
}

func TestInsightMarkdownExport(t *testing.T) {
	te := setup(t)
	content := "# Insight\n\n- stored markdown"
	id := te.seedInsight(t, "daily_activity", "2025-01-15", new("my-app"),
		func(insight *db.Insight) {
			insight.Content = content
		},
	)

	w := te.get(t, fmt.Sprintf("/api/v1/insights/%d/md", id))
	assertStatus(t, w, http.StatusOK)
	assert.Contains(t, w.Header().Get("Content-Type"), "text/markdown")
	assert.Contains(t, w.Header().Get("Content-Disposition"), "inline")
	assert.Contains(t, w.Header().Get("Content-Disposition"), ".md")
	assert.Equal(t, content, w.Body.String())
}

func TestInsightPublish(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		te := setup(t)
		te.srv.SetGithubToken("fake-token")
		id := te.seedInsight(t, "daily_activity", "2025-01-15", new("my-app"),
			func(insight *db.Insight) {
				insight.Content = "# Insight\n\n- publish me"
			},
		)

		originalTransport := http.DefaultTransport
		http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
			require.Equal(t, http.MethodPost, req.Method)
			require.Equal(t, "https://api.github.com/gists", req.URL.String())
			require.Equal(t, "token fake-token", req.Header.Get("Authorization"))

			var payload struct {
				Description string `json:"description"`
				Public      bool   `json:"public"`
				Files       map[string]struct {
					Content string `json:"content"`
				} `json:"files"`
			}
			body, err := io.ReadAll(req.Body)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(body, &payload))
			assert.Equal(t, "Insight: Daily Activity - my-app - 2025-01-15", payload.Description)
			assert.False(t, payload.Public)
			require.Contains(t, payload.Files, "insight-daily_activity-my-app-20250115.html")
			assert.Contains(t,
				payload.Files["insight-daily_activity-my-app-20250115.html"].Content,
				"Daily Activity Insight",
			)
			assert.Contains(t,
				payload.Files["insight-daily_activity-my-app-20250115.html"].Content,
				"# Insight",
			)

			return &http.Response{
				StatusCode: http.StatusCreated,
				Header:     make(http.Header),
				Body: io.NopCloser(strings.NewReader(
					`{"id":"gist123","html_url":"https://gist.github.com/octocat/gist123","owner":{"login":"octocat"}}`,
				)),
			}, nil
		})
		t.Cleanup(func() {
			http.DefaultTransport = originalTransport
		})

		w := te.post(t, fmt.Sprintf("/api/v1/insights/%d/publish?secret=true", id), "{}")
		assertStatus(t, w, http.StatusOK)

		resp := decode[map[string]string](t, w)
		assert.Equal(t, "gist123", resp["gist_id"])
		assert.Equal(t, "https://gist.github.com/octocat/gist123", resp["gist_url"])
		assert.Equal(t,
			"https://gist.githubusercontent.com/octocat/gist123/raw/insight-daily_activity-my-app-20250115.html",
			resp["raw_url"],
		)
		assert.Equal(t,
			"https://htmlpreview.github.io/?https://gist.githubusercontent.com/octocat/gist123/raw/insight-daily_activity-my-app-20250115.html",
			resp["view_url"],
		)
	})

	t.Run("NoToken", func(t *testing.T) {
		t.Setenv("AGENTSVIEW_GITHUB_TOKEN", "")
		t.Setenv("PATH", t.TempDir())
		te := setup(t)
		id := te.seedInsight(t, "daily_activity", "2025-01-15", new("my-app"))

		w := te.post(t, fmt.Sprintf("/api/v1/insights/%d/publish", id), "{}")
		assertStatus(t, w, http.StatusUnauthorized)
	})

	t.Run("ForwardedRequestDoesNotUseGitHubCLIAuthTokenFallback", func(t *testing.T) {
		useGitHubCLIAuthTokenStub(t)
		te := setup(t)
		id := te.seedInsight(t, "daily_activity", "2025-01-15", new("my-app"))

		req := httptest.NewRequest(http.MethodPost,
			fmt.Sprintf("/api/v1/insights/%d/publish", id),
			strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://127.0.0.1:0")
		req.Header.Set("X-Forwarded-For", "203.0.113.10")
		w := httptest.NewRecorder()
		te.handler.ServeHTTP(w, req)

		assertStatus(t, w, http.StatusUnauthorized)
	})

	t.Run("NotFound", func(t *testing.T) {
		te := setup(t)
		te.srv.SetGithubToken("fake-token")

		w := te.post(t, "/api/v1/insights/99999/publish", "{}")
		assertStatus(t, w, http.StatusNotFound)
	})
}

func TestGenerateInsight_Validation(t *testing.T) {
	te := setup(t)

	tests := []struct {
		name     string
		payload  string
		wantBody string
	}{
		{"InvalidType", `{"type":"bad","date_from":"2025-01-15","date_to":"2025-01-15"}`, ""},
		{"InvalidDateFrom", `{"type":"daily_activity","date_from":"bad","date_to":"2025-01-15"}`, "date_from"},
		{"InvalidDateTo", `{"type":"daily_activity","date_from":"2025-01-15","date_to":"bad"}`, "date_to"},
		{"DateToBeforeDateFrom", `{"type":"daily_activity","date_from":"2025-01-16","date_to":"2025-01-15"}`, "date_to must be"},
		{"InvalidJSON", `{bad json`, ""},
		{"InvalidAgent", `{"type":"daily_activity","date_from":"2025-01-15","date_to":"2025-01-15","agent":"gpt"}`, "invalid agent"},
		{"InvalidAutomatedScope", `{"type":"daily_activity","date_from":"2025-01-15","date_to":"2025-01-15","automated_scope":"robots"}`, "automated_scope"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := te.post(t, "/api/v1/insights/generate", tt.payload)

			assertStatus(t, w, http.StatusBadRequest)
			if tt.wantBody != "" {
				assertBodyContains(t, w, tt.wantBody)
			}
		})
	}
}

func TestGenerateInsight_DefaultAgent(t *testing.T) {
	stubGen := func(
		_ context.Context, agent, _ string,
	) (insight.Result, error) {
		assert.Equal(t, "claude", agent, "expected default agent claude")
		return insight.Result{}, fmt.Errorf("stub: no CLI")
	}
	te := setupWithServerOpts(t, []server.Option{
		server.WithGenerateFunc(stubGen),
	})

	w := te.post(t, "/api/v1/insights/generate",
		`{"type":"daily_activity","date_from":"2025-01-15","date_to":"2025-01-15"}`)
	assertStatus(t, w, http.StatusOK)
	assertBodyContains(t, w, "event: error")
	assertBodyContains(t, w, "stub: no CLI")
}

func TestGenerateInsight_PersistsWithReadOnlyStore(t *testing.T) {
	dir := tempDirWithRetryCleanup(t)
	dbPath := filepath.Join(dir, "test.db")
	database, err := db.Open(dbPath)
	require.NoError(t, err, "opening db")
	t.Cleanup(func() { database.Close() })

	store := &readOnlyInsightPersistStore{Store: database}
	cfg := config.Config{
		Host:         "127.0.0.1",
		Port:         0,
		DataDir:      dir,
		DBPath:       dbPath,
		WriteTimeout: 30 * time.Second,
	}
	srv := server.New(cfg, store, nil, server.WithGenerateFunc(func(
		_ context.Context, agent, _ string,
	) (insight.Result, error) {
		assert.Equal(t, "claude", agent)
		return insight.Result{
			Agent:   "claude",
			Content: "persisted from read-only wrapper",
		}, nil
	}))
	te := &testEnv{
		srv:         srv,
		handler:     wrapTestHandler(cfg, srv.Handler()),
		db:          database,
		engine:      nil,
		broadcaster: nil,
		dataDir:     dir,
	}

	assert.True(t, store.ReadOnly())

	w := te.post(t, "/api/v1/insights/generate",
		`{"type":"daily_activity","date_from":"2025-01-15","date_to":"2025-01-15"}`)
	assertStatus(t, w, http.StatusOK)

	events := parseSSE(w.Body.String())
	require.NotEmpty(t, events)
	require.Equal(t, "done", events[len(events)-1].Event)
	assert.Equal(t, 1, store.insertCalls)

	var saved db.Insight
	require.NoError(t, json.Unmarshal([]byte(events[len(events)-1].Data), &saved))
	assert.Equal(t, "persisted from read-only wrapper", saved.Content)
	assert.Equal(t, "claude", saved.Agent)

	stored, err := database.GetInsight(context.Background(), saved.ID)
	require.NoError(t, err, "GetInsight after generate")
	require.NotNil(t, stored)
	assert.Equal(t, saved.ID, stored.ID)
	assert.Equal(t, saved.Content, stored.Content)
}

func TestGenerateInsight_StaysBlockedForReadOnlyStoreWithoutInsightWrites(t *testing.T) {
	dir := tempDirWithRetryCleanup(t)
	dbPath := filepath.Join(dir, "test.db")
	database, err := db.Open(dbPath)
	require.NoError(t, err, "opening db")
	t.Cleanup(func() { database.Close() })

	var called bool
	cfg := config.Config{
		Host:         "127.0.0.1",
		Port:         0,
		DataDir:      dir,
		DBPath:       dbPath,
		WriteTimeout: 30 * time.Second,
	}
	srv := server.New(cfg, readOnlyTestStore{Store: database}, nil, server.WithGenerateFunc(func(
		_ context.Context, _ string, _ string,
	) (insight.Result, error) {
		called = true
		return insight.Result{Agent: "claude", Content: "should not run"}, nil
	}))
	te := &testEnv{
		srv:         srv,
		handler:     wrapTestHandler(cfg, srv.Handler()),
		db:          database,
		engine:      nil,
		broadcaster: nil,
		dataDir:     dir,
	}

	w := te.post(t, "/api/v1/insights/generate",
		`{"type":"daily_activity","date_from":"2025-01-15","date_to":"2025-01-15"}`)
	assertStatus(t, w, http.StatusNotImplemented)
	assertBodyContains(t, w, "read-only mode")
	assert.False(t, called)
}

func TestGenerateCannedInsight_RequiresExplicitOptIn(t *testing.T) {
	te := setup(t)

	w := te.post(t, "/api/v1/insights/generate",
		`{"type":"llm_canned","kind":"prompt_maturity_review","date_from":"2025-01-15","date_to":"2025-01-15","agent":"claude"}`)

	assertStatus(t, w, http.StatusBadRequest)
	assertBodyContains(t, w, "llm_opt_in")
}

func TestGenerateCannedInsight_RejectsInvalidFilterTimezone(t *testing.T) {
	te := setup(t)

	w := te.post(t, "/api/v1/insights/generate",
		`{"type":"llm_canned","kind":"prompt_maturity_review","date_from":"2025-01-15","date_to":"2025-01-15","agent":"claude","llm_opt_in":true,"filters":{"timezone":"Fake/Zone","include_one_shot":false,"automated_scope":"human"}}`)

	assertStatus(t, w, http.StatusBadRequest)
	assertBodyContains(t, w, "invalid timezone: Fake/Zone")
}

func TestGenerateCannedInsight_ReturnsValidationDetail(t *testing.T) {
	stubGen := func(
		_ context.Context, _, _ string, _ insight.LogFunc,
	) (insight.Result, error) {
		return insight.Result{
			Agent: "claude",
			Model: "test-model",
			Content: `{
				"schema_version":"llm_insight.v1",
				"kind":"model_cost_review",
				"summary":"Cache behavior needs a closer look.",
				"confidence":"medium",
				"recommendations":[{
					"title":"Review cache misses",
					"rationale":"The usage aggregates suggest cache misses.",
					"actions":["Review expensive sessions"],
					"evidence_refs":["usage:cache_behavior"],
					"impact":"medium",
					"effort":"low"
				}],
				"risks":[],
				"evidence_refs":["usage:cache_behavior"]
			}`,
		}, nil
	}
	te := setupWithServerOpts(t, []server.Option{
		server.WithGenerateStreamFunc(stubGen),
	})

	w := te.post(t, "/api/v1/insights/generate",
		`{"type":"llm_canned","kind":"model_cost_review","date_from":"2025-01-15","date_to":"2025-01-15","agent":"claude","llm_opt_in":true}`)
	assertStatus(t, w, http.StatusOK)
	assertBodyContains(t, w, "event: error")
	assertBodyContains(t, w,
		"generated insight failed validation: unknown envelope evidence_ref: usage:cache_behavior")
}

func TestGenerateCannedInsight_SaveCacheAndPreserveSignals(t *testing.T) {
	var calls atomic.Int32
	stubGen := func(
		_ context.Context, agent, prompt string, _ insight.LogFunc,
	) (insight.Result, error) {
		calls.Add(1)
		if agent != "claude" {
			t.Fatalf("agent = %q, want claude", agent)
		}
		if !strings.Contains(prompt, "Do not recalculate, override") {
			t.Fatalf("prompt missing score boundary: %s", prompt)
		}
		return insight.Result{
			Agent: "claude",
			Model: "test-model",
			Content: `{
				"schema_version":"llm_insight.v1",
				"kind":"prompt_maturity_review",
				"summary":"Prompt starts are mostly healthy, with a few places to tighten acceptance criteria.",
				"confidence":"medium",
				"recommendations":[{
					"title":"Add explicit verification asks",
					"rationale":"The selected aggregate has scored sessions and outcome data, so verification wording can be improved without changing scores.",
					"actions":["Add acceptance criteria to implementation prompts","Ask for validation commands in task handoffs"],
					"evidence_refs":["signals:score_distribution","signals:outcomes"],
					"impact":"medium",
					"effort":"low"
				}],
				"risks":[{
					"title":"Evidence is aggregate-only",
					"explanation":"This recommendation does not inspect raw transcript text.",
					"evidence_refs":["signals:score_distribution"]
				}],
				"evidence_refs":["signals:score_distribution","signals:outcomes"]
			}`,
		}, nil
	}
	te := setupWithServerOpts(t, []server.Option{
		server.WithGenerateStreamFunc(stubGen),
	})
	te.seedSession(t, "quality-1", "my-app", 6)
	score := 86
	grade := "B"
	if err := te.db.UpdateSessionSignals("quality-1", db.SessionSignalUpdate{
		ToolFailureSignalCount: 2,
		ToolRetryCount:         1,
		Outcome:                "completed",
		OutcomeConfidence:      "high",
		EndedWithRole:          "assistant",
		HealthScore:            &score,
		HealthGrade:            &grade,
		HasToolCalls:           true,
		HasContextData:         true,
	}); err != nil {
		t.Fatalf("UpdateSessionSignals: %v", err)
	}
	before, err := te.db.GetSession(context.Background(), "quality-1")
	if err != nil {
		t.Fatalf("GetSession before: %v", err)
	}

	payload := `{"type":"llm_canned","kind":"prompt_maturity_review","date_from":"2025-01-15","date_to":"2025-01-15","project":"my-app","agent":"claude","llm_opt_in":true}`
	w := te.post(t, "/api/v1/insights/generate", payload)
	assertStatus(t, w, http.StatusOK)
	events := parseSSE(w.Body.String())
	if len(events) == 0 || events[len(events)-1].Event != "done" {
		t.Fatalf("expected done event, got %s", w.Body.String())
	}

	var saved db.Insight
	if err := json.Unmarshal([]byte(events[len(events)-1].Data), &saved); err != nil {
		t.Fatalf("decode saved insight: %v", err)
	}
	if saved.Type != insight.CannedType {
		t.Fatalf("Type = %q, want %q", saved.Type, insight.CannedType)
	}
	if saved.Kind != "prompt_maturity_review" {
		t.Fatalf("Kind = %q", saved.Kind)
	}
	if saved.SchemaVersion != insight.CannedSchemaVersion ||
		saved.TemplateID == "" || saved.AggregateHash == "" ||
		saved.CacheKey == "" || saved.CacheStatus != "fresh" ||
		saved.ProvenanceJSON == "" || saved.StructuredJSON == "" {
		t.Fatalf("missing canned metadata: %+v", saved)
	}
	if !strings.Contains(saved.Content, "AI-generated recommendation") {
		t.Fatalf("content missing generated label: %s", saved.Content)
	}

	after, err := te.db.GetSession(context.Background(), "quality-1")
	if err != nil {
		t.Fatalf("GetSession after: %v", err)
	}
	if *after.HealthScore != *before.HealthScore ||
		*after.HealthGrade != *before.HealthGrade ||
		after.ToolFailureSignalCount != before.ToolFailureSignalCount ||
		after.ToolRetryCount != before.ToolRetryCount {
		t.Fatalf("canonical signals changed: before=%+v after=%+v", before, after)
	}

	w = te.post(t, "/api/v1/insights/generate", payload)
	assertStatus(t, w, http.StatusOK)
	if calls.Load() != 1 {
		t.Fatalf("generator calls = %d, want 1 after cache hit", calls.Load())
	}
	events = parseSSE(w.Body.String())
	foundCacheHit := false
	var cached db.Insight
	for _, ev := range events {
		if ev.Event == "status" && strings.Contains(ev.Data, "cache_hit") {
			foundCacheHit = true
		}
		if ev.Event == "done" {
			if err := json.Unmarshal([]byte(ev.Data), &cached); err != nil {
				t.Fatalf("decode cached insight: %v", err)
			}
		}
	}
	if !foundCacheHit {
		t.Fatalf("expected cache_hit status, got %s", w.Body.String())
	}
	if cached.CacheStatus != "hit" {
		t.Fatalf("cached CacheStatus = %q, want hit", cached.CacheStatus)
	}
	if !strings.Contains(cached.ProvenanceJSON, `"cache_status":"hit"`) {
		t.Fatalf("cached provenance missing hit status: %s", cached.ProvenanceJSON)
	}
	stored, err := te.db.GetInsight(context.Background(), saved.ID)
	if err != nil {
		t.Fatalf("GetInsight stored: %v", err)
	}
	if stored == nil || stored.CacheStatus != "fresh" ||
		!strings.Contains(stored.ProvenanceJSON, `"cache_status":"fresh"`) {
		t.Fatalf("stored insight should keep original provenance: %+v", stored)
	}
}

func TestGenerateCannedInsight_ModelCostPromptIncludesModelBreakdown(t *testing.T) {
	var capturedPrompt string
	stubGen := func(
		_ context.Context, _, prompt string, _ insight.LogFunc,
	) (insight.Result, error) {
		capturedPrompt = prompt
		return insight.Result{
			Agent: "claude",
			Model: "test-model",
			Content: `{
				"schema_version":"llm_insight.v1",
				"kind":"model_cost_review",
				"summary":"Model costs are concentrated in the supplied breakdown.",
				"confidence":"high",
				"recommendations":[{
					"title":"Review the highest cost model",
					"rationale":"The deterministic model breakdown identifies the cost concentration.",
					"actions":["Compare the top model against lower-cost alternatives for routine tasks"],
					"evidence_refs":["usage:model_breakdown"],
					"impact":"medium",
					"effort":"low"
				}],
				"risks":[],
				"evidence_refs":["usage:model_breakdown"]
			}`,
		}, nil
	}
	te := setupWithServerOpts(t, []server.Option{
		server.WithGenerateStreamFunc(stubGen),
	})
	if err := te.db.UpsertModelPricing([]db.ModelPricing{
		{
			ModelPattern:         "claude-opus-4-7",
			InputPerMTok:         15,
			OutputPerMTok:        75,
			CacheCreationPerMTok: 18.75,
			CacheReadPerMTok:     1.5,
		},
		{
			ModelPattern:         "claude-sonnet-4-6",
			InputPerMTok:         3,
			OutputPerMTok:        15,
			CacheCreationPerMTok: 3.75,
			CacheReadPerMTok:     0.3,
		},
	}); err != nil {
		t.Fatalf("UpsertModelPricing: %v", err)
	}
	te.seedSession(t, "usage-1", "my-app", 4)
	te.seedMessages(t, "usage-1", 4, func(i int, m *db.Message) {
		switch i {
		case 1:
			m.Model = "claude-opus-4-7"
			m.TokenUsage = json.RawMessage(
				`{"input_tokens":1000,"output_tokens":200,"cache_creation_input_tokens":100,"cache_read_input_tokens":50}`)
		case 3:
			m.Model = "claude-sonnet-4-6"
			m.TokenUsage = json.RawMessage(
				`{"input_tokens":2000,"output_tokens":300,"cache_creation_input_tokens":0,"cache_read_input_tokens":400}`)
		}
	})

	payload := `{"type":"llm_canned","kind":"model_cost_review","date_from":"2025-01-15","date_to":"2025-01-15","project":"my-app","agent":"claude","llm_opt_in":true}`
	w := te.post(t, "/api/v1/insights/generate", payload)
	assertStatus(t, w, http.StatusOK)
	assertBodyContains(t, w, "event: done")

	for _, want := range []string{
		`"model_breakdowns"`,
		`"model_name":"claude-opus-4-7"`,
		`"model_name":"claude-sonnet-4-6"`,
		`usage:model_breakdown`,
	} {
		if !strings.Contains(capturedPrompt, want) {
			t.Fatalf("prompt missing %q: %s", want, capturedPrompt)
		}
	}
	if strings.Contains(capturedPrompt, "Model mix is not directly observable") {
		t.Fatalf("prompt should include observable model mix: %s", capturedPrompt)
	}
}

func TestGenerateCannedInsight_CoachSummaryUsesAllPages(t *testing.T) {
	var generatedPrompt string
	stubGen := func(
		_ context.Context, _ string, prompt string, _ insight.LogFunc,
	) (insight.Result, error) {
		generatedPrompt = prompt
		return insight.Result{
			Agent: "claude",
			Model: "test-model",
			Content: `{
				"schema_version":"llm_insight.v1",
				"kind":"prompt_maturity_review",
				"summary":"Coach-derived prompt maturity evidence is available for the selected scope.",
				"confidence":"medium",
				"recommendations":[{
					"title":"Tighten repeated implementation prompts",
					"rationale":"The Coach prompt maturity aggregate covers the selected sessions without changing canonical scores.",
					"actions":["Add explicit verification steps to repeated prompts"],
					"evidence_refs":["coach:prompt_maturity"],
					"impact":"medium",
					"effort":"low"
				}],
				"risks":[],
				"evidence_refs":["coach:prompt_maturity"]
			}`,
		}, nil
	}
	te := setupWithServerOpts(t, []server.Option{
		server.WithGenerateStreamFunc(stubGen),
	})
	prompt := "Implement paged workflow review with acceptance criteria and verify output"
	for i := range db.MaxSessionLimit + 1 {
		te.seedSession(t, fmt.Sprintf("coach-%03d", i), "my-app", 3,
			func(s *db.Session) {
				s.FirstMessage = &prompt
				s.HasToolCalls = true
			})
	}

	w := te.post(t, "/api/v1/insights/generate",
		`{"type":"llm_canned","kind":"prompt_maturity_review","date_from":"2025-01-15","date_to":"2025-01-15","project":"my-app","agent":"claude","llm_opt_in":true}`)

	assertStatus(t, w, http.StatusOK)
	if !strings.Contains(generatedPrompt, `"session_count":501`) {
		t.Fatalf("generated prompt missing all-page Coach session count: %s",
			generatedPrompt)
	}
}

func TestGenerateCannedInsight_AutomatedScopeOnlyAutomated(t *testing.T) {
	var generatedPrompt string
	stubGen := func(
		_ context.Context, _ string, prompt string, _ insight.LogFunc,
	) (insight.Result, error) {
		generatedPrompt = prompt
		return insight.Result{
			Agent: "claude",
			Model: "test-model",
			Content: `{
				"schema_version":"llm_insight.v1",
				"kind":"prompt_maturity_review",
				"summary":"Automated review prompts are isolated for this recommendation.",
				"confidence":"medium",
				"recommendations":[{
					"title":"Separate review automation from human work",
					"rationale":"The Coach prompt maturity aggregate covers the selected automated scope.",
					"actions":["Review automated sessions separately from human sessions"],
					"evidence_refs":["coach:prompt_maturity"],
					"impact":"medium",
					"effort":"low"
				}],
				"risks":[],
				"evidence_refs":["coach:prompt_maturity"]
			}`,
		}, nil
	}
	te := setupWithServerOpts(t, []server.Option{
		server.WithGenerateStreamFunc(stubGen),
	})
	humanPrompt := "Implement the checkout fix with tests and verification"
	te.seedSession(t, "human-session", "my-app", 4, func(s *db.Session) {
		s.FirstMessage = &humanPrompt
		s.UserMessageCount = 2
	})
	autoPrompt := "You are a code reviewer. Review the diff."
	te.seedSession(t, "auto-session", "my-app", 2, func(s *db.Session) {
		s.FirstMessage = &autoPrompt
		s.UserMessageCount = 1
		s.IsAutomated = true
	})

	w := te.post(t, "/api/v1/insights/generate",
		`{"type":"llm_canned","kind":"prompt_maturity_review","date_from":"2025-01-15","date_to":"2025-01-15","project":"my-app","agent":"claude","llm_opt_in":true,"automated_scope":"automated"}`)

	assertStatus(t, w, http.StatusOK)
	if !strings.Contains(generatedPrompt, `"automated_scope":"automated"`) {
		t.Fatalf("generated prompt missing automated scope: %s", generatedPrompt)
	}
	if !strings.Contains(generatedPrompt, `"session_count":1`) {
		t.Fatalf("generated prompt should include only automated session: %s", generatedPrompt)
	}
	if strings.Contains(generatedPrompt, "human-session") {
		t.Fatalf("generated prompt included human session: %s", generatedPrompt)
	}
}

func TestGenerateCannedInsight_UsesSessionFilterPayload(t *testing.T) {
	var calls atomic.Int32
	var generatedPrompts []string
	stubGen := func(
		_ context.Context, _ string, prompt string, _ insight.LogFunc,
	) (insight.Result, error) {
		calls.Add(1)
		generatedPrompts = append(generatedPrompts, prompt)
		return insight.Result{
			Agent: "claude",
			Model: "test-model",
			Content: `{
				"schema_version":"llm_insight.v1",
				"kind":"prompt_maturity_review",
				"summary":"Prompt maturity evidence is scoped to the active dashboard filters.",
				"confidence":"medium",
				"recommendations":[{
					"title":"Keep filtered recommendations scoped",
					"rationale":"The Coach prompt maturity aggregate covers only the selected session cohort.",
					"actions":["Generate recommendations from the same filters used by the dashboard"],
					"evidence_refs":["coach:prompt_maturity"],
					"impact":"medium",
					"effort":"low"
				}],
				"risks":[],
				"evidence_refs":["coach:prompt_maturity"]
			}`,
		}, nil
	}
	te := setupWithServerOpts(t, []server.Option{
		server.WithGenerateStreamFunc(stubGen),
	})
	clean := "clean"
	codexPrompt := "Implement filtered recommendations with acceptance criteria and verification"
	claudePrompt := "Implement agent-specific recommendations with acceptance criteria and verification"
	wrongMachinePrompt := "Implement workstation filter bypass with acceptance criteria"
	oneShotPrompt := "Fix it"
	te.seedSession(t, "codex-match", "my-app", 4, func(s *db.Session) {
		s.Agent = "codex"
		s.Machine = "workstation"
		s.UserMessageCount = 3
		s.FirstMessage = &codexPrompt
		s.HasToolCalls = true
		s.TerminationStatus = &clean
	})
	te.seedSession(t, "claude-match", "my-app", 4, func(s *db.Session) {
		s.Agent = "claude"
		s.Machine = "workstation"
		s.UserMessageCount = 3
		s.FirstMessage = &claudePrompt
		s.HasToolCalls = true
		s.TerminationStatus = &clean
	})
	te.seedSession(t, "wrong-machine", "my-app", 4, func(s *db.Session) {
		s.Agent = "codex"
		s.Machine = "other-host"
		s.UserMessageCount = 3
		s.FirstMessage = &wrongMachinePrompt
		s.HasToolCalls = true
		s.TerminationStatus = &clean
	})
	te.seedSession(t, "one-shot", "my-app", 1, func(s *db.Session) {
		s.Agent = "codex"
		s.Machine = "workstation"
		s.UserMessageCount = 1
		s.FirstMessage = &oneShotPrompt
		s.HasToolCalls = true
		s.TerminationStatus = &clean
	})

	firstPayload := `{"type":"llm_canned","kind":"prompt_maturity_review","date_from":"2025-01-15","date_to":"2025-01-15","project":"my-app","agent":"claude","llm_opt_in":true,"filters":{"timezone":"America/New_York","agent":"codex","machine":"workstation","termination":"clean","min_user_messages":2,"include_one_shot":false,"automated_scope":"human"}}`
	w := te.post(t, "/api/v1/insights/generate", firstPayload)
	assertStatus(t, w, http.StatusOK)
	require.Equal(t, int32(1), calls.Load())
	require.Len(t, generatedPrompts, 1)
	assert.Contains(t, generatedPrompts[0], `"timezone":"America/New_York"`)
	assert.Contains(t, generatedPrompts[0], `"agent":"codex"`)
	assert.Contains(t, generatedPrompts[0], `"session_count":1`)
	assert.Contains(t, generatedPrompts[0], codexPrompt)
	assert.NotContains(t, generatedPrompts[0], claudePrompt)
	assert.NotContains(t, generatedPrompts[0], wrongMachinePrompt)
	assert.NotContains(t, generatedPrompts[0], oneShotPrompt)

	secondPayload := `{"type":"llm_canned","kind":"prompt_maturity_review","date_from":"2025-01-15","date_to":"2025-01-15","project":"my-app","agent":"claude","llm_opt_in":true,"filters":{"timezone":"America/New_York","agent":"claude","machine":"workstation","termination":"clean","min_user_messages":2,"include_one_shot":false,"automated_scope":"human"}}`
	w = te.post(t, "/api/v1/insights/generate", secondPayload)
	assertStatus(t, w, http.StatusOK)
	require.Equal(t, int32(2), calls.Load())
	require.Len(t, generatedPrompts, 2)
	assert.Contains(t, generatedPrompts[1], `"agent":"claude"`)
	assert.Contains(t, generatedPrompts[1], `"session_count":1`)
	assert.Contains(t, generatedPrompts[1], claudePrompt)
	assert.NotContains(t, generatedPrompts[1], codexPrompt)
}

func TestGenerateCannedInsight_CoachSummaryUsesFilterTimezone(t *testing.T) {
	var generatedPrompt string
	stubGen := func(
		_ context.Context, _ string, prompt string, _ insight.LogFunc,
	) (insight.Result, error) {
		generatedPrompt = prompt
		return insight.Result{
			Agent: "claude",
			Model: "test-model",
			Content: `{
				"schema_version":"llm_insight.v1",
				"kind":"prompt_maturity_review",
				"summary":"Prompt maturity evidence is scoped to the requested local day.",
				"confidence":"medium",
				"recommendations":[{
					"title":"Keep local-day Coach scope aligned",
					"rationale":"Coach prompt maturity uses the same local-date filter as the canned payload.",
					"actions":["Generate recommendations from the selected local day"],
					"evidence_refs":["coach:prompt_maturity"],
					"impact":"medium",
					"effort":"low"
				}],
				"risks":[],
				"evidence_refs":["coach:prompt_maturity"]
			}`,
		}, nil
	}
	te := setupWithServerOpts(t, []server.Option{
		server.WithGenerateStreamFunc(stubGen),
	})

	localDayPrompt := "Implement local-day filtering with acceptance criteria and verification steps"
	previousLocalDayPrompt := "Implement previous-day filtering with acceptance criteria and verification steps"
	te.seedSession(t, "local-day-match", "my-app", 4, func(s *db.Session) {
		started := "2025-01-16T07:30:00Z"
		ended := "2025-01-16T07:45:00Z"
		s.StartedAt = &started
		s.EndedAt = &ended
		s.FirstMessage = &localDayPrompt
	})
	te.seedSession(t, "previous-local-day", "my-app", 4, func(s *db.Session) {
		started := "2025-01-15T01:00:00Z"
		ended := "2025-01-15T01:15:00Z"
		s.StartedAt = &started
		s.EndedAt = &ended
		s.FirstMessage = &previousLocalDayPrompt
	})

	payload := `{"type":"llm_canned","kind":"prompt_maturity_review","date_from":"2025-01-15","date_to":"2025-01-15","project":"my-app","agent":"claude","llm_opt_in":true,"filters":{"timezone":"America/Los_Angeles","include_one_shot":false,"automated_scope":"human"}}`
	w := te.post(t, "/api/v1/insights/generate", payload)
	assertStatus(t, w, http.StatusOK)

	assert.Contains(t, generatedPrompt, `"timezone":"America/Los_Angeles"`)
	assert.Contains(t, generatedPrompt, `"session_count":1`)
	assert.Contains(t, generatedPrompt, localDayPrompt)
	assert.NotContains(t, generatedPrompt, previousLocalDayPrompt)
}

func TestGenerateCannedInsight_CoachSummaryUsesTopLevelTimezone(t *testing.T) {
	var generatedPrompt string
	stubGen := func(
		_ context.Context, _ string, prompt string, _ insight.LogFunc,
	) (insight.Result, error) {
		generatedPrompt = prompt
		return insight.Result{
			Agent: "claude",
			Model: "test-model",
			Content: `{
				"schema_version":"llm_insight.v1",
				"kind":"prompt_maturity_review",
				"summary":"Prompt maturity evidence is scoped to the requested local day.",
				"confidence":"medium",
				"recommendations":[{
					"title":"Keep local-day Coach scope aligned",
					"rationale":"Coach prompt maturity uses the same local-date filter as the canned payload.",
					"actions":["Generate recommendations from the selected local day"],
					"evidence_refs":["coach:prompt_maturity"],
					"impact":"medium",
					"effort":"low"
				}],
				"risks":[],
				"evidence_refs":["coach:prompt_maturity"]
			}`,
		}, nil
	}
	te := setupWithServerOpts(t, []server.Option{
		server.WithGenerateStreamFunc(stubGen),
	})

	localDayPrompt := "Implement local-day filtering with acceptance criteria and verification steps"
	previousLocalDayPrompt := "Implement previous-day filtering with acceptance criteria and verification steps"
	te.seedSession(t, "local-day-match", "my-app", 4, func(s *db.Session) {
		started := "2025-01-16T07:30:00Z"
		ended := "2025-01-16T07:45:00Z"
		s.StartedAt = &started
		s.EndedAt = &ended
		s.FirstMessage = &localDayPrompt
	})
	te.seedSession(t, "previous-local-day", "my-app", 4, func(s *db.Session) {
		started := "2025-01-15T01:00:00Z"
		ended := "2025-01-15T01:15:00Z"
		s.StartedAt = &started
		s.EndedAt = &ended
		s.FirstMessage = &previousLocalDayPrompt
	})

	payload := `{"type":"llm_canned","kind":"prompt_maturity_review","date_from":"2025-01-15","date_to":"2025-01-15","project":"my-app","agent":"claude","llm_opt_in":true,"timezone":"America/Los_Angeles"}`
	w := te.post(t, "/api/v1/insights/generate", payload)
	assertStatus(t, w, http.StatusOK)

	assert.Contains(t, generatedPrompt, `"timezone":"America/Los_Angeles"`)
	assert.Contains(t, generatedPrompt, `"session_count":1`)
	assert.Contains(t, generatedPrompt, localDayPrompt)
	assert.NotContains(t, generatedPrompt, previousLocalDayPrompt)
}

func TestGenerateCannedInsight_RejectsOversizedFocus(t *testing.T) {
	te := setup(t)
	longFocus := strings.Repeat("x", insight.MaxCannedFocusRunes+1)
	body, err := json.Marshal(map[string]any{
		"type":       "llm_canned",
		"kind":       "prompt_maturity_review",
		"date_from":  "2025-01-15",
		"date_to":    "2025-01-15",
		"agent":      "claude",
		"llm_opt_in": true,
		"prompt":     longFocus,
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	w := te.post(t, "/api/v1/insights/generate", string(body))

	assertStatus(t, w, http.StatusBadRequest)
	assertBodyContains(t, w, "prompt is too long")
}

func TestGenerateCannedInsight_NormalizesFocusBeforeCaching(t *testing.T) {
	var calls atomic.Int32
	var generatedPrompt string
	stubGen := func(
		_ context.Context, _ string, prompt string, _ insight.LogFunc,
	) (insight.Result, error) {
		calls.Add(1)
		generatedPrompt = prompt
		return insight.Result{
			Agent: "claude",
			Model: "test-model",
			Content: `{
				"schema_version":"llm_insight.v1",
				"kind":"prompt_maturity_review",
				"summary":"Prompt starts are mostly healthy, with a few places to tighten acceptance criteria.",
				"confidence":"medium",
				"recommendations":[{
					"title":"Add explicit verification asks",
					"rationale":"The selected aggregate has scored sessions and outcome data, so verification wording can be improved without changing scores.",
					"actions":["Add acceptance criteria to implementation prompts","Ask for validation commands in task handoffs"],
					"evidence_refs":["aggregate:empty"],
					"impact":"medium",
					"effort":"low"
				}],
				"risks":[{
					"title":"Evidence is aggregate-only",
					"explanation":"This recommendation does not inspect raw transcript text.",
					"evidence_refs":["aggregate:empty"]
				}],
				"evidence_refs":["aggregate:empty"]
			}`,
		}, nil
	}
	te := setupWithServerOpts(t, []server.Option{
		server.WithGenerateStreamFunc(stubGen),
	})

	padded := strings.Repeat(" ", 5) + "Focus on retries" + strings.Repeat("\n", 4)
	body, err := json.Marshal(map[string]any{
		"type":       "llm_canned",
		"kind":       "prompt_maturity_review",
		"date_from":  "2025-01-15",
		"date_to":    "2025-01-15",
		"agent":      "claude",
		"llm_opt_in": true,
		"prompt":     padded,
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	w := te.post(t, "/api/v1/insights/generate", string(body))
	assertStatus(t, w, http.StatusOK)
	events := parseSSE(w.Body.String())
	if len(events) == 0 || events[len(events)-1].Event != "done" {
		t.Fatalf("expected done event, got %s", w.Body.String())
	}
	var saved db.Insight
	if err := json.Unmarshal([]byte(events[len(events)-1].Data), &saved); err != nil {
		t.Fatalf("decode saved insight: %v", err)
	}
	if saved.Prompt == nil || *saved.Prompt != "Focus on retries" {
		t.Fatalf("saved Prompt = %v, want trimmed focus", saved.Prompt)
	}
	if strings.Contains(generatedPrompt, padded) ||
		!strings.Contains(generatedPrompt, "Focus on retries") {
		t.Fatalf("generated prompt did not normalize focus: %q", generatedPrompt)
	}

	trimmedPayload := `{"type":"llm_canned","kind":"prompt_maturity_review","date_from":"2025-01-15","date_to":"2025-01-15","agent":"claude","llm_opt_in":true,"prompt":"Focus on retries"}`
	w = te.post(t, "/api/v1/insights/generate", trimmedPayload)
	assertStatus(t, w, http.StatusOK)
	if calls.Load() != 1 {
		t.Fatalf("generator calls = %d, want 1 after normalized cache hit", calls.Load())
	}
	assertBodyContains(t, w, "cache_hit")
}

func TestGenerateInsight_ErrorMessageStripsStderr(t *testing.T) {
	stubGen := func(
		_ context.Context, _, _ string,
	) (insight.Result, error) {
		return insight.Result{}, fmt.Errorf(
			"claude CLI failed: exit status 1\nstderr: some debug output",
		)
	}
	te := setupWithServerOpts(t, []server.Option{
		server.WithGenerateFunc(stubGen),
	})

	w := te.post(t, "/api/v1/insights/generate",
		`{"type":"daily_activity","date_from":"2025-01-15","date_to":"2025-01-15"}`)
	assertStatus(t, w, http.StatusOK)
	body := w.Body.String()
	require.Contains(t, body, "claude CLI failed: exit status 1")
	require.NotContains(t, body, "some debug output",
		"expected stderr to be stripped from client message")
}

func TestGenerateInsight_ErrorMessageStripsRaw(t *testing.T) {
	stubGen := func(
		_ context.Context, _, _ string,
	) (insight.Result, error) {
		return insight.Result{}, fmt.Errorf(
			"claude returned empty result\nraw: {\"type\":\"result\",\"result\":\"\"}",
		)
	}
	te := setupWithServerOpts(t, []server.Option{
		server.WithGenerateFunc(stubGen),
	})

	w := te.post(t, "/api/v1/insights/generate",
		`{"type":"daily_activity","date_from":"2025-01-15","date_to":"2025-01-15"}`)
	assertStatus(t, w, http.StatusOK)
	body := w.Body.String()
	require.Contains(t, body, "claude returned empty result")
	require.NotContains(t, body, `"type":"result"`,
		"expected raw payload to be stripped from client message")
}

func TestGenerateInsight_InitialStatusWriteFailureSkipsGeneration(t *testing.T) {
	var called atomic.Bool
	te := setupWithServerOpts(t, []server.Option{
		server.WithGenerateStreamFunc(func(
			_ context.Context, _ string, _ string, _ insight.LogFunc,
		) (insight.Result, error) {
			called.Store(true)
			return insight.Result{Content: "should not run"}, nil
		}),
	})

	req := httptest.NewRequest(
		http.MethodPost, "/api/v1/insights/generate",
		strings.NewReader(
			`{"type":"daily_activity","date_from":"2025-01-15","date_to":"2025-01-15","agent":"claude"}`,
		),
	)
	req.Header.Set("Content-Type", "application/json")

	w := newFailFirstWriteRecorder()
	te.handler.ServeHTTP(w, req)

	require.False(t, called.Load(),
		"generation should not run when initial SSE status write fails")
}

func TestGenerateInsight_StreamsLogs(t *testing.T) {
	stubGen := func(
		_ context.Context, _ string, _ string, onLog insight.LogFunc,
	) (insight.Result, error) {
		onLog(insight.LogEvent{
			Stream: "stdout",
			Line:   `{"type":"system","status":"ready"}`,
		})
		onLog(insight.LogEvent{
			Stream: "stderr",
			Line:   "rate limit warning",
		})
		return insight.Result{
			Content: "# Insight",
			Agent:   "claude",
			Model:   "test-model",
		}, nil
	}
	te := setupWithServerOpts(t, []server.Option{
		server.WithGenerateStreamFunc(stubGen),
	})

	w := te.post(t, "/api/v1/insights/generate",
		`{"type":"daily_activity","date_from":"2025-01-15","date_to":"2025-01-15","agent":"claude"}`)
	assertStatus(t, w, http.StatusOK)

	events := parseSSE(w.Body.String())
	require.GreaterOrEqual(t, len(events), 4, "expected >=4 SSE events: %s", w.Body.String())
	require.Equal(t, "status", events[0].Event, "first event")
	require.Equal(t, "log", events[1].Event, "events: %#v", events)
	require.Equal(t, "log", events[2].Event, "events: %#v", events)
	require.Equal(t, "done", events[len(events)-1].Event, "last event")

	var log1 insight.LogEvent
	require.NoError(t, json.Unmarshal([]byte(events[1].Data), &log1))
	require.Equal(t, "stdout", log1.Stream)

	var log2 insight.LogEvent
	require.NoError(t, json.Unmarshal([]byte(events[2].Data), &log2))
	require.Equal(t, "stderr", log2.Stream)
}

type slowFlushRecorder struct {
	*httptest.ResponseRecorder
	delay time.Duration
	mu    sync.Mutex
}

func (f *slowFlushRecorder) Write(
	b []byte,
) (int, error) {
	time.Sleep(f.delay)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ResponseRecorder.Write(b)
}

func (f *slowFlushRecorder) Flush() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ResponseRecorder.Flush()
}

func (f *slowFlushRecorder) BodyString() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Body.String()
}

type slowLogRecorder struct {
	*httptest.ResponseRecorder
	delay time.Duration
	mu    sync.Mutex
}

func (f *slowLogRecorder) Write(
	b []byte,
) (int, error) {
	if strings.HasPrefix(string(b), "event: log\n") {
		time.Sleep(f.delay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ResponseRecorder.Write(b)
}

func (f *slowLogRecorder) Flush() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ResponseRecorder.Flush()
}

func (f *slowLogRecorder) BodyString() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Body.String()
}

type blockingLogRecorder struct {
	*httptest.ResponseRecorder
	release <-chan struct{}
	mu      sync.Mutex
}

func (f *blockingLogRecorder) Write(
	b []byte,
) (int, error) {
	if strings.HasPrefix(string(b), "event: log\n") {
		<-f.release
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ResponseRecorder.Write(b)
}

func (f *blockingLogRecorder) Flush() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ResponseRecorder.Flush()
}

func (f *blockingLogRecorder) BodyString() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Body.String()
}

type firstLogDelayRecorder struct {
	*httptest.ResponseRecorder
	delay time.Duration
	once  sync.Once
	mu    sync.Mutex
}

func (f *firstLogDelayRecorder) Write(
	b []byte,
) (int, error) {
	if strings.HasPrefix(string(b), "event: log\n") {
		f.once.Do(func() {
			time.Sleep(f.delay)
		})
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ResponseRecorder.Write(b)
}

func (f *firstLogDelayRecorder) Flush() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ResponseRecorder.Flush()
}

func (f *firstLogDelayRecorder) BodyString() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Body.String()
}

type deadlineAwareBlockingLogRecorder struct {
	*httptest.ResponseRecorder
	handlerReturned     <-chan struct{}
	postReturnWrites    atomic.Int32
	postReturnAttempted chan struct{}
	deadlineUpdates     chan struct{}
	mu                  sync.Mutex
	writeDeadline       time.Time
}

func newDeadlineAwareBlockingLogRecorder(
	handlerReturned <-chan struct{},
) *deadlineAwareBlockingLogRecorder {
	return &deadlineAwareBlockingLogRecorder{
		ResponseRecorder:    httptest.NewRecorder(),
		handlerReturned:     handlerReturned,
		postReturnAttempted: make(chan struct{}, 1),
		deadlineUpdates:     make(chan struct{}, 1),
	}
}

func (f *deadlineAwareBlockingLogRecorder) SetWriteDeadline(t time.Time) error {
	f.mu.Lock()
	f.writeDeadline = t
	f.mu.Unlock()
	select {
	case f.deadlineUpdates <- struct{}{}:
	default:
	}
	return nil
}

func (f *deadlineAwareBlockingLogRecorder) Write(
	b []byte,
) (int, error) {
	if f.handlerReturned != nil {
		select {
		case <-f.handlerReturned:
			f.postReturnWrites.Add(1)
			select {
			case f.postReturnAttempted <- struct{}{}:
			default:
			}
		default:
		}
	}

	if strings.HasPrefix(string(b), "event: log\n") {
		for {
			f.mu.Lock()
			deadline := f.writeDeadline
			f.mu.Unlock()
			if !deadline.IsZero() && !deadline.After(time.Now()) {
				return 0, os.ErrDeadlineExceeded
			}
			<-f.deadlineUpdates
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ResponseRecorder.Write(b)
}

func (f *deadlineAwareBlockingLogRecorder) Flush() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ResponseRecorder.Flush()
}

func (f *deadlineAwareBlockingLogRecorder) PostReturnWrites() int32 {
	return f.postReturnWrites.Load()
}

func (f *deadlineAwareBlockingLogRecorder) PostReturnAttempted() <-chan struct{} {
	return f.postReturnAttempted
}

func (f *deadlineAwareBlockingLogRecorder) BodyString() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Body.String()
}

func TestGenerateInsight_LogDropSummaryAndCompletion(t *testing.T) {
	stubGen := func(
		_ context.Context, _ string, _ string, onLog insight.LogFunc,
	) (insight.Result, error) {
		for i := range 5000 {
			onLog(insight.LogEvent{
				Stream: "stdout",
				Line:   fmt.Sprintf("line-%d", i),
			})
		}
		return insight.Result{
			Content: "# Insight",
			Agent:   "claude",
		}, nil
	}
	te := setupWithServerOpts(t, []server.Option{
		server.WithGenerateStreamFunc(stubGen),
	})

	req := httptest.NewRequest(
		http.MethodPost, "/api/v1/insights/generate",
		strings.NewReader(
			`{"type":"daily_activity","date_from":"2025-01-15","date_to":"2025-01-15","agent":"claude"}`,
		),
	)
	req.Header.Set("Content-Type", "application/json")
	w := &slowFlushRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		delay:            4 * time.Millisecond,
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		te.handler.ServeHTTP(w, req)
	}()

	select {
	case <-done:
	case <-time.After(8 * time.Second):
		require.Fail(t, "timed out waiting for generate handler")
	}

	assertStatus(t, w.ResponseRecorder, http.StatusOK)
	events := parseSSE(w.BodyString())

	foundDone := false
	foundDropSummary := false
	for _, ev := range events {
		if ev.Event == "done" {
			foundDone = true
		}
		if ev.Event != "log" {
			continue
		}
		var line insight.LogEvent
		if json.Unmarshal([]byte(ev.Data), &line) != nil {
			continue
		}
		if line.Stream == "stderr" &&
			strings.Contains(line.Line, "dropped ") &&
			strings.Contains(line.Line, "slow client") {
			foundDropSummary = true
		}
	}
	require.True(t, foundDropSummary,
		"expected dropped-log summary event, got %d events", len(events))
	require.True(t, foundDone, "expected done event")
}

func TestGenerateInsight_LogDrainTimeoutReturnsWithoutHang(t *testing.T) {
	stubGen := func(
		_ context.Context, _ string, _ string, onLog insight.LogFunc,
	) (insight.Result, error) {
		for i := range 10 {
			onLog(insight.LogEvent{
				Stream: "stdout",
				Line:   fmt.Sprintf("slow-line-%d", i),
			})
		}
		return insight.Result{
			Content: "# Insight",
			Agent:   "claude",
		}, nil
	}
	te := setupWithServerOpts(t, []server.Option{
		server.WithGenerateStreamFunc(stubGen),
		fastInsightLogDrainTimeouts(),
	})

	req := httptest.NewRequest(
		http.MethodPost, "/api/v1/insights/generate",
		strings.NewReader(
			`{"type":"daily_activity","date_from":"2025-01-15","date_to":"2025-01-15","agent":"claude"}`,
		),
	)
	req.Header.Set("Content-Type", "application/json")
	w := &slowLogRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		delay:            75 * time.Millisecond,
	}

	started := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		te.handler.ServeHTTP(w, req)
	}()

	select {
	case <-done:
	case <-time.After(12 * time.Second):
		require.Fail(t, "timed out waiting for generate handler completion")
	}
	require.LessOrEqual(t, time.Since(started), 7*time.Second,
		"handler should return within bounded timeout handling")

	assertStatus(t, w.ResponseRecorder, http.StatusOK)
	events := parseSSE(w.BodyString())
	for _, ev := range events {
		require.NotEqual(t, "done", ev.Event,
			"did not expect done event when timeout path is triggered")
	}
}

func TestGenerateInsight_LogDrainTimeoutReportsBufferedDrops(t *testing.T) {
	stubGen := func(
		_ context.Context, _ string, _ string, onLog insight.LogFunc,
	) (insight.Result, error) {
		for i := range 10 {
			onLog(insight.LogEvent{
				Stream: "stdout",
				Line:   fmt.Sprintf("slow-line-%d", i),
			})
		}
		return insight.Result{
			Content: "# Insight",
			Agent:   "claude",
		}, nil
	}
	te := setupWithServerOpts(t, []server.Option{
		server.WithGenerateStreamFunc(stubGen),
		fastInsightLogDrainTimeouts(),
	})

	req := httptest.NewRequest(
		http.MethodPost, "/api/v1/insights/generate",
		strings.NewReader(
			`{"type":"daily_activity","date_from":"2025-01-15","date_to":"2025-01-15","agent":"claude"}`,
		),
	)
	req.Header.Set("Content-Type", "application/json")
	w := &firstLogDelayRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		delay:            75 * time.Millisecond,
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		te.handler.ServeHTTP(w, req)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		require.Fail(t, "timed out waiting for generate handler completion")
	}

	assertStatus(t, w.ResponseRecorder, http.StatusOK)
	events := parseSSE(w.BodyString())
	foundTimeoutError := false
	foundDropSummary := false
	for _, ev := range events {
		require.NotEqual(t, "done", ev.Event,
			"did not expect done event when timeout path is triggered")
		if ev.Event == "error" &&
			strings.Contains(ev.Data, "timed out before completion") {
			foundTimeoutError = true
		}
		if ev.Event != "log" {
			continue
		}
		var line insight.LogEvent
		if json.Unmarshal([]byte(ev.Data), &line) != nil {
			continue
		}
		if line.Stream != "stderr" ||
			!strings.HasPrefix(line.Line, "dropped ") ||
			!strings.Contains(line.Line, "log stream timeout") {
			continue
		}
		parts := strings.SplitN(line.Line, " ", 3)
		if len(parts) < 3 {
			continue
		}
		dropped, err := strconv.Atoi(parts[1])
		if err != nil {
			continue
		}
		// 10 events were enqueued; timeout truncation should account
		// for most buffered entries that were never flushed.
		require.GreaterOrEqual(t, dropped, 8,
			"expected timeout drop summary >=8 (%q)", line.Line)
		foundDropSummary = true
	}
	require.True(t, foundTimeoutError,
		"expected timeout error event, got %d events", len(events))
	require.True(t, foundDropSummary,
		"expected timeout-aware drop summary, got %d events", len(events))
}

func TestGenerateInsight_LogDrainTimeoutBoundedWhenWriterStuck(t *testing.T) {
	stubGen := func(
		_ context.Context, _ string, _ string, onLog insight.LogFunc,
	) (insight.Result, error) {
		onLog(insight.LogEvent{Stream: "stdout", Line: "stuck-line"})
		return insight.Result{Content: "# Insight", Agent: "claude"}, nil
	}
	te := setupWithServerOpts(t, []server.Option{
		server.WithGenerateStreamFunc(stubGen),
		fastInsightLogDrainTimeouts(),
	})

	req := httptest.NewRequest(
		http.MethodPost, "/api/v1/insights/generate",
		strings.NewReader(
			`{"type":"daily_activity","date_from":"2025-01-15","date_to":"2025-01-15","agent":"claude"}`,
		),
	)
	req.Header.Set("Content-Type", "application/json")
	release := make(chan struct{})
	w := &blockingLogRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		release:          release,
	}

	started := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		te.handler.ServeHTTP(w, req)
	}()

	select {
	case <-done:
	case <-time.After(7 * time.Second):
		require.Fail(t, "timed out waiting for bounded timeout behavior")
	}
	elapsed := time.Since(started)
	require.LessOrEqual(t, elapsed, 6*time.Second,
		"handler returned too slowly for stuck writer path")
	close(release)

	assertStatus(t, w.ResponseRecorder, http.StatusOK)
	events := parseSSE(w.BodyString())
	for _, ev := range events {
		require.NotEqual(t, "done", ev.Event,
			"did not expect done event on stuck writer timeout path")
	}
}

func TestGenerateInsight_LogDrainTimeoutForceUnblocksAndNoPostReturnWrites(t *testing.T) {
	stubGen := func(
		_ context.Context, _ string, _ string, onLog insight.LogFunc,
	) (insight.Result, error) {
		onLog(insight.LogEvent{Stream: "stdout", Line: "force-unblock-line"})
		return insight.Result{Content: "# Insight", Agent: "claude"}, nil
	}
	te := setupWithServerOpts(t, []server.Option{
		server.WithGenerateStreamFunc(stubGen),
		fastInsightLogDrainTimeouts(),
	})

	req := httptest.NewRequest(
		http.MethodPost, "/api/v1/insights/generate",
		strings.NewReader(
			`{"type":"daily_activity","date_from":"2025-01-15","date_to":"2025-01-15","agent":"claude"}`,
		),
	)
	req.Header.Set("Content-Type", "application/json")
	handlerReturned := make(chan struct{})
	w := newDeadlineAwareBlockingLogRecorder(handlerReturned)

	done := make(chan struct{})
	go func() {
		defer close(done)
		te.handler.ServeHTTP(w, req)
		close(handlerReturned)
	}()

	select {
	case <-done:
	case <-time.After(8 * time.Second):
		require.Fail(t, "timed out waiting for forced-unblock completion")
	}

	select {
	case <-w.PostReturnAttempted():
		require.Fail(t, "expected no writes after handler return")
	case <-time.After(300 * time.Millisecond):
	}
	require.Zero(t, w.PostReturnWrites(), "expected no writes after handler return")

	assertStatus(t, w.ResponseRecorder, http.StatusOK)
	events := parseSSE(w.BodyString())
	foundTimeoutError := false
	for _, ev := range events {
		require.NotEqual(t, "done", ev.Event,
			"did not expect done event on forced-unblock timeout path")
		if ev.Event == "error" &&
			strings.Contains(ev.Data, "timed out before completion") {
			foundTimeoutError = true
		}
	}
	require.True(t, foundTimeoutError, "expected timeout error event")
}

func TestDeleteInsight_Found(t *testing.T) {
	te := setup(t)

	id := te.seedInsight(t, "daily_activity", "2025-01-15",
		new("my-app"))

	w := te.del(t, fmt.Sprintf("/api/v1/insights/%d", id))
	assertStatus(t, w, http.StatusNoContent)

	// Verify it's gone.
	w = te.get(t, fmt.Sprintf("/api/v1/insights/%d", id))
	assertStatus(t, w, http.StatusNotFound)
}

func TestInsight_ResourceErrors(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		status int
	}{
		{"Get_NotFound", http.MethodGet, "/api/v1/insights/99999", http.StatusNotFound},
		{"Get_InvalidID", http.MethodGet, "/api/v1/insights/abc", http.StatusBadRequest},
		{"Delete_NotFound", http.MethodDelete, "/api/v1/insights/99999", http.StatusNotFound},
		{"Delete_InvalidID", http.MethodDelete, "/api/v1/insights/abc", http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			te := setup(t)
			if tt.method == http.MethodGet {
				w := te.get(t, tt.path)
				assertStatus(t, w, tt.status)
			} else {
				w := te.del(t, tt.path)
				assertStatus(t, w, tt.status)
			}
		})
	}
}

// --- helpers ---

func (te *testEnv) seedInsight(
	t *testing.T,
	typ, date string,
	project *string,
	opts ...func(*db.Insight),
) int64 {
	t.Helper()
	insight := db.Insight{
		Type:     typ,
		DateFrom: date,
		DateTo:   date,
		Project:  project,
		Agent:    "claude",
		Content:  "Test insight content",
	}
	for _, opt := range opts {
		opt(&insight)
	}
	id, err := te.db.InsertInsight(insight)
	require.NoError(t, err)
	return id
}
