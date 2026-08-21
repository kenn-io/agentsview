package server_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/pricing"
)

// TestActivityProjectReclassificationCandidatesRouteRemoved confirms the
// legacy candidates route is no longer part of the API surface. A plain GET
// against the removed path cannot assert 404: any unregistered /api/ path
// falls through the stdlib ServeMux to the SPA catch-all handler and
// returns 200 with index.html (see server.go's handleSPA), which is true of
// every never-registered path and would not distinguish a removed route
// from a typo'd one. Asserting against the OpenAPI document instead proves
// the operation itself is gone.
func TestActivityProjectReclassificationCandidatesRouteRemoved(t *testing.T) {
	te := setup(t)
	w := te.get(t, "/api/openapi.json")
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var spec struct {
		Paths map[string]map[string]any `json:"paths"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &spec))
	assert.NotContains(t, spec.Paths,
		"/api/v1/activity/project-reclassification/candidates")
}

// activityDate is a fixed past calendar day so the activity report is
// deterministic and complete (non-partial) regardless of wall clock.
const activityDate = "2025-06-02"

func TestActivityReportRejectsFilterWhoseSignedIDExceedsLimit(t *testing.T) {
	te := setup(t)
	values := url.Values{
		"preset":     {"day"},
		"date":       {activityDate},
		"timezone":   {"UTC"},
		"project":    {strings.Repeat("\"", 1024)},
		"agent":      {strings.Repeat("\"", 1024)},
		"machine":    {strings.Repeat("\"", 1024)},
		"automation": {"all"},
	}

	req := httptest.NewRequest(
		http.MethodGet, "/api/v1/activity/report?"+values.Encode(), nil,
	)
	req.Header.Set("Accept", "text/event-stream")
	response := httptest.NewRecorder()
	te.handler.ServeHTTP(response, req)
	assertStatus(t, response, http.StatusBadRequest)
	assert.Contains(t, response.Body.String(), "report ID")
}

// seedActivityReportFixture seeds two sessions that overlap in wall-clock
// on activityDate so peak concurrency is 2. Each session gets distinct,
// increasing per-message timestamps inside the day (under the 300s gap
// cap) so the aggregator produces active intervals; the default
// seedMessages timestamp would yield zero activity.
func seedActivityReportFixture(t *testing.T, te *testEnv) {
	t.Helper()

	type entry struct {
		id, project, agent, started, ended string
		msgTimes                           []string
	}
	entries := []entry{
		{
			id: "d1", project: "alpha", agent: "claude",
			started: activityDate + "T10:00:00Z",
			ended:   activityDate + "T10:08:00Z",
			msgTimes: []string{
				activityDate + "T10:00:00Z",
				activityDate + "T10:02:00Z",
				activityDate + "T10:05:00Z",
				activityDate + "T10:07:00Z",
			},
		},
		{
			id: "d2", project: "beta", agent: "codex",
			started: activityDate + "T10:01:00Z",
			ended:   activityDate + "T10:09:00Z",
			msgTimes: []string{
				activityDate + "T10:01:00Z",
				activityDate + "T10:03:00Z",
				activityDate + "T10:06:00Z",
				activityDate + "T10:08:00Z",
			},
		},
	}

	for _, e := range entries {
		started, ended := e.started, e.ended
		te.seedSession(t, e.id, e.project, len(e.msgTimes),
			func(s *db.Session) {
				s.Agent = e.agent
				s.StartedAt = &started
				s.EndedAt = &ended
			},
		)
		times := e.msgTimes
		te.seedMessages(t, e.id, len(times),
			func(i int, m *db.Message) {
				m.Timestamp = times[i]
			},
		)
	}
}

func TestActivityReportEndpoint_Presets(t *testing.T) {
	te := setup(t)
	seedActivityReportFixture(t, te)

	tests := []struct {
		name   string
		params map[string]string
		check  func(t *testing.T, resp activity.Report)
	}{
		{
			name: "day",
			params: map[string]string{
				"preset": "day", "date": activityDate, "timezone": "UTC",
			},
			check: func(t *testing.T, resp activity.Report) {
				assert.Equal(t, 2, resp.Peak.Agents)
				assert.Equal(t, 2, resp.Totals.Sessions)
				assert.Equal(t, "minute", resp.BucketUnit)
				assert.False(t, resp.Partial)
			},
		},
		{
			name: "week",
			params: map[string]string{
				"preset": "week", "date": activityDate, "timezone": "UTC",
			},
			check: func(t *testing.T, resp activity.Report) {
				assert.Equal(t, "hour", resp.BucketUnit, "a 7-day week auto-buckets hourly")
				assert.Equal(t, 168, resp.BucketCount)
			},
		},
		{
			name: "month",
			params: map[string]string{
				"preset": "month", "date": activityDate, "timezone": "UTC",
			},
			check: func(t *testing.T, resp activity.Report) {
				assert.Equal(t, "day", resp.BucketUnit, "a 30-day month auto-buckets daily")
			},
		},
		{
			name: "custom",
			params: map[string]string{
				"preset":   "custom",
				"from":     activityDate + "T00:00:00Z",
				"to":       activityDate + "T23:59:59Z",
				"timezone": "UTC",
			},
			check: func(t *testing.T, resp activity.Report) {
				assert.Equal(t, 2, resp.Totals.Sessions)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := te.get(t, buildPathURL("/api/v1/activity/report", tc.params))
			assertStatus(t, w, http.StatusOK)
			tc.check(t, decode[activity.Report](t, w))
		})
	}
}

func TestActivityReportJSONIncludesExportMetadata(t *testing.T) {
	te := setup(t)
	seedActivityReportMetadataFixture(t, te)

	w := te.get(t, buildPathURL("/api/v1/activity/report", map[string]string{
		"preset": "day", "date": activityDate, "timezone": "UTC",
	}))
	assertStatus(t, w, http.StatusOK)

	resp := decode[activity.Report](t, w)
	assert.Equal(t, export.ActivityReportSchemaVersion,
		resp.SchemaVersion)
	require.NotNil(t, resp.Pricing)
	require.Contains(t, resp.Pricing.Models, "gpt-5.1")
	fallbackModel := activityReportFallbackModel(t)
	require.Contains(t, resp.Pricing.Models, fallbackModel)
	assert.Equal(t, export.CostSourceReported,
		resp.Pricing.Models["gpt-5.1"].CostSource)
	assert.Equal(t, export.CostSourceComputed,
		resp.Pricing.Models[fallbackModel].CostSource)
	assert.True(t, resp.Pricing.Fallback.Used)
	assert.Contains(t, resp.Pricing.Fallback.Models, fallbackModel)
	require.Len(t, resp.Projects, 1)
	for key, project := range resp.Projects {
		assert.NotContains(t, key, "shared-project")
		assert.Equal(t, "shared-project", project.DisplayLabel)
		assert.Equal(t, export.ProjectResolutionUnknown, project.Resolution)
	}
	assert.Equal(t, 2, resp.Totals.Sessions)
	assert.Equal(t, 150, resp.Totals.OutputTokens)
	assert.NotEmpty(t, resp.Buckets)
	assert.Len(t, resp.BySession, 2)
}

func seedActivityReportMetadataFixture(t *testing.T, te *testEnv) {
	t.Helper()
	fallbackModel := activityReportFallbackModel(t)
	seedFallbackModelPricing(t, te.db, fallbackModel)
	started := activityDate + "T10:00:00Z"
	ended := activityDate + "T10:05:00Z"
	for _, id := range []string{"reported-cost", "fallback-cost"} {
		sessionID := "activity-meta-" + id
		te.seedSession(t, sessionID, "shared-project", 2, func(s *db.Session) {
			s.Agent = "codex"
			s.StartedAt = &started
			s.EndedAt = &ended
			s.UserMessageCount = 1
		})
	}
	te.seedMessages(t, "activity-meta-reported-cost", 2,
		func(i int, m *db.Message) {
			m.Timestamp = []string{started, activityDate + "T10:02:00Z"}[i]
			if i == 1 {
				m.Role = "assistant"
				m.Model = "gpt-5.1"
			}
		})
	te.seedMessages(t, "activity-meta-fallback-cost", 2,
		func(i int, m *db.Message) {
			m.Timestamp = []string{started, ended}[i]
			if i == 1 {
				m.Role = "assistant"
				m.Model = fallbackModel
				m.TokenUsage = jsontext.Value(
					`{"input_tokens":200,"output_tokens":100}`)
			}
		})
	cost := money.MustParseDollars("0.25")
	ordinal := 1
	require.NoError(t, te.db.ReplaceSessionUsageEvents(
		"activity-meta-reported-cost", []db.UsageEvent{{
			SessionID: "activity-meta-reported-cost", MessageOrdinal: &ordinal,
			Source: "session", Model: "gpt-5.1", InputTokens: 100,
			OutputTokens: 50, Cost: &cost,
			OccurredAt: activityDate + "T10:02:00Z",
			DedupKey:   "activity-meta-reported-cost:event",
		}},
	))
}

func activityReportFallbackModel(t *testing.T) string {
	t.Helper()
	for _, p := range pricing.FallbackPricing() {
		if p.OutputPerMTok.Microdollars > 0 {
			return p.ModelPattern
		}
	}
	require.FailNow(t, "no fallback model with non-zero output pricing")
	return ""
}

func seedFallbackModelPricing(t *testing.T, database *db.DB, model string) {
	t.Helper()
	for _, p := range pricing.FallbackPricing() {
		if p.ModelPattern != model {
			continue
		}
		require.NoError(t, database.UpsertModelPricing([]db.ModelPricing{{
			ModelPattern:         p.ModelPattern,
			InputPerMTok:         p.InputPerMTok,
			OutputPerMTok:        p.OutputPerMTok,
			CacheCreationPerMTok: p.CacheCreationPerMTok,
			CacheReadPerMTok:     p.CacheReadPerMTok,
		}}))
		return
	}
	require.FailNow(t, "fallback model not found", model)
}

// TestActivityReportEndpoint_IncludesOneShotAndAutomated locks in the
// activity endpoint's hardest binding constraint: unlike the sibling
// analytics endpoints, it sets ExcludeOneShot=false and
// ExcludeAutomated=false (see huma_routes_activity.go), so one-shot
// (user_message_count <= 1) and automated (is_automated=1) sessions
// MUST appear in the report. A refactor flipping those flags to match
// the analytics defaults would drop these sessions and fail here.
func TestActivityReportEndpoint_IncludesOneShotAndAutomated(t *testing.T) {

	te := setup(t)

	// One-shot: a single user message (user_message_count = 1).
	started, ended := activityDate+"T11:00:00Z", activityDate+"T11:04:00Z"
	te.seedSession(t, "oneshot", "alpha", 2, func(s *db.Session) {
		s.Agent = "claude"
		s.StartedAt = &started
		s.EndedAt = &ended
		s.UserMessageCount = 1
	})
	te.seedMessages(t, "oneshot", 2, func(i int, m *db.Message) {
		m.Timestamp = []string{
			activityDate + "T11:00:00Z", activityDate + "T11:02:00Z",
		}[i]
	})

	// Automated: a first message matching a known automated (roborev)
	// prompt prefix, which UpsertSession turns into is_automated = 1.
	autoStart, autoEnd := activityDate+"T12:00:00Z", activityDate+"T12:04:00Z"
	te.seedSession(t, "automated", "beta", 2, func(s *db.Session) {
		s.Agent = "codex"
		s.StartedAt = &autoStart
		s.EndedAt = &autoEnd
		s.FirstMessage = new("You are a code reviewer.")
		s.UserMessageCount = 1
	})
	te.seedMessages(t, "automated", 2, func(i int, m *db.Message) {
		m.Timestamp = []string{
			activityDate + "T12:00:00Z", activityDate + "T12:02:00Z",
		}[i]
	})

	w := te.get(t, buildPathURL("/api/v1/activity/report", map[string]string{
		"preset": "day", "date": activityDate, "timezone": "UTC",
	}))
	assertStatus(t, w, http.StatusOK)

	resp := decode[activity.Report](t, w)
	ids := make(map[string]struct{}, len(resp.BySession))
	for _, s := range resp.BySession {
		ids[s.SessionID] = struct{}{}
	}
	assert.Contains(t, ids, "oneshot",
		"one-shot session must be included (ExcludeOneShot=false)")
	assert.Contains(t, ids, "automated",
		"automated session must be included (ExcludeAutomated=false)")
	assert.Equal(t, 2, resp.Totals.Sessions,
		"both one-shot and automated sessions count toward the total")
}

func TestActivityReportEndpoint_Validation(t *testing.T) {

	te := setup(t)

	ts := activityDate + "T00:00:00Z"
	tests := []struct {
		name   string
		params map[string]string
	}{
		{
			name: "bad date",
			params: map[string]string{
				"preset": "day", "date": "not-a-date", "timezone": "UTC",
			},
		},
		{
			name: "bad timezone",
			params: map[string]string{
				"preset": "day", "date": activityDate, "timezone": "Fake/Zone",
			},
		},
		{
			name: "custom missing bound",
			params: map[string]string{
				"preset": "custom", "from": activityDate + "T00:00:00Z",
			},
		},
		{
			name: "from after to",
			params: map[string]string{
				"preset": "custom",
				"from":   activityDate + "T12:00:00Z",
				"to":     activityDate + "T00:00:00Z",
			},
		},
		{
			name: "zero length range",
			params: map[string]string{
				"preset": "custom", "from": ts, "to": ts,
			},
		},
		{
			name: "range exceeds year",
			params: map[string]string{
				"preset": "custom",
				"from":   "2026-01-01T00:00:00Z",
				"to":     "2027-01-02T00:00:00Z",
			},
		},
		{
			name: "bucket count cap",
			params: map[string]string{
				"preset": "custom",
				"from":   "2026-01-01T00:00:00Z",
				"to":     "2026-12-31T00:00:00Z",
				"bucket": "5m",
			},
		},
		{
			name: "bad automation",
			params: map[string]string{
				"preset": "day", "date": activityDate, "timezone": "UTC",
				"automation": "bogus",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := te.get(t, buildPathURL("/api/v1/activity/report", tc.params))
			assertStatus(t, w, http.StatusBadRequest)
		})
	}
}

// TestActivityReportEndpoint_AutomationFilter confirms the activity endpoint's
// automation query param selects the requested class end to end: the default
// and "all" keep both classes, "interactive" drops automated sessions, and
// "automated" drops interactive ones. It also confirms the response Totals
// carry the automated/interactive session-count split.
func TestActivityReportEndpoint_AutomationFilter(t *testing.T) {

	te := setup(t)

	// Automated: a single-turn session whose first message matches a known
	// automated (roborev) prompt prefix, which the classifier turns into
	// is_automated = 1. UserMessageCount = 1 satisfies the single-turn gate.
	autoStart, autoEnd := activityDate+"T12:00:00Z", activityDate+"T12:04:00Z"
	te.seedSession(t, "automated", "beta", 2, func(s *db.Session) {
		s.Agent = "codex"
		s.StartedAt = &autoStart
		s.EndedAt = &autoEnd
		s.FirstMessage = new("You are a code reviewer.")
		s.UserMessageCount = 1
	})
	te.seedMessages(t, "automated", 2, func(i int, m *db.Message) {
		if i == 0 {
			m.Content = "You are a code reviewer."
		}
		m.Timestamp = []string{
			activityDate + "T12:00:00Z", activityDate + "T12:02:00Z",
		}[i]
	})

	humanStart, humanEnd := activityDate+"T13:00:00Z", activityDate+"T13:04:00Z"
	te.seedSession(t, "human", "alpha", 2, func(s *db.Session) {
		s.Agent = "claude"
		s.StartedAt = &humanStart
		s.EndedAt = &humanEnd
	})
	te.seedMessages(t, "human", 2, func(i int, m *db.Message) {
		m.Timestamp = []string{
			activityDate + "T13:00:00Z", activityDate + "T13:02:00Z",
		}[i]
	})

	tests := []struct {
		name            string
		automation      string
		wantAutomated   int
		wantInteractive int
		wantIDs         []string
	}{
		{"default keeps both", "", 1, 1, []string{"automated", "human"}},
		{"all keeps both", "all", 1, 1, []string{"automated", "human"}},
		{"interactive drops automated", "interactive", 0, 1, []string{"human"}},
		{"automated drops interactive", "automated", 1, 0, []string{"automated"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			params := map[string]string{
				"preset": "day", "date": activityDate, "timezone": "UTC",
			}
			if tc.automation != "" {
				params["automation"] = tc.automation
			}
			w := te.get(t, buildPathURL("/api/v1/activity/report", params))
			assertStatus(t, w, http.StatusOK)
			resp := decode[activity.Report](t, w)
			assert.Equal(t, len(tc.wantIDs), resp.Totals.Sessions)
			assert.Equal(t, tc.wantAutomated, resp.Totals.AutomatedSessions)
			assert.Equal(t, tc.wantInteractive, resp.Totals.InteractiveSessions)
			ids := make(map[string]struct{}, len(resp.BySession))
			for _, s := range resp.BySession {
				ids[s.SessionID] = struct{}{}
			}
			assert.Len(t, ids, len(tc.wantIDs))
			for _, id := range tc.wantIDs {
				assert.Contains(t, ids, id)
			}
		})
	}
}

// TestActivityReportEndpoint_GitBranchFilter guards that /activity/report honors
// the git_branch filter (it previously ignored the param).
func TestActivityReportEndpoint_GitBranchFilter(t *testing.T) {
	te := setup(t)
	seed := []struct {
		id, branch, started, ended string
		times                      []string
	}{
		{"b1", "main", activityDate + "T10:00:00Z", activityDate + "T10:08:00Z",
			[]string{activityDate + "T10:00:00Z", activityDate + "T10:02:00Z",
				activityDate + "T10:05:00Z", activityDate + "T10:07:00Z"}},
		{"b2", "feature-x", activityDate + "T10:01:00Z", activityDate + "T10:09:00Z",
			[]string{activityDate + "T10:01:00Z", activityDate + "T10:03:00Z",
				activityDate + "T10:06:00Z", activityDate + "T10:08:00Z"}},
	}
	for _, e := range seed {
		started, ended, branch := e.started, e.ended, e.branch
		te.seedSession(t, e.id, "alpha", len(e.times), func(s *db.Session) {
			s.GitBranch = branch
			s.StartedAt = &started
			s.EndedAt = &ended
		})
		times := e.times
		te.seedMessages(t, e.id, len(times), func(i int, m *db.Message) {
			m.Timestamp = times[i]
		})
	}

	all := te.get(t, buildPathURL("/api/v1/activity/report", map[string]string{
		"preset": "day", "date": activityDate, "timezone": "UTC",
	}))
	assertStatus(t, all, http.StatusOK)
	assert.Equal(t, 2, decode[activity.Report](t, all).Totals.Sessions)

	filtered := te.get(t, buildPathURL("/api/v1/activity/report", map[string]string{
		"preset": "day", "date": activityDate, "timezone": "UTC",
		"git_branch": db.EncodeBranchFilterToken("alpha", "main"),
	}))
	assertStatus(t, filtered, http.StatusOK)
	assert.Equal(t, 1, decode[activity.Report](t, filtered).Totals.Sessions,
		"git_branch filter restricts the activity report to alpha/main")
}

func TestActivityReportEndpointNegotiatesProgressAndPagesSessions(t *testing.T) {
	te := setup(t)
	seedActivityReportFixture(t, te)
	path := "/api/v1/activity/report?preset=day&date=" + activityDate + "&timezone=UTC"

	plain := te.get(t, path)
	assertStatus(t, plain, http.StatusOK)
	report := decode[activity.Report](t, plain)
	require.NotEmpty(t, report.ReportID)
	require.Len(t, report.BySession, 2)
	assert.Equal(t, 2, report.SessionsTotal)
	assert.NotContains(t, plain.Body.String(), `"intervals"`)

	pageResponse := te.get(t, "/api/v1/activity/report/"+report.ReportID+
		"/sessions?limit=1")
	assertStatus(t, pageResponse, http.StatusOK)
	var first struct {
		Sessions   []activity.SessionRow `json:"sessions"`
		NextCursor string                `json:"next_cursor"`
		Total      int                   `json:"total"`
		Report     *activity.Report      `json:"report"`
	}
	require.NoError(t, json.Unmarshal(pageResponse.Body.Bytes(), &first))
	require.Len(t, first.Sessions, 1)
	require.NotEmpty(t, first.NextCursor)
	assert.Equal(t, 2, first.Total)
	assert.Nil(t, first.Report, "ordinary browser pages omit full report metadata")

	metadataResponse := te.get(t, "/api/v1/activity/report/"+report.ReportID+
		"/sessions?limit=1&include_report=true")
	assertStatus(t, metadataResponse, http.StatusOK)
	var metadataPage struct {
		Sessions []activity.SessionRow `json:"sessions"`
		Report   *activity.Report      `json:"report"`
	}
	require.NoError(t, json.Unmarshal(metadataResponse.Body.Bytes(), &metadataPage))
	require.NotNil(t, metadataPage.Report)
	assert.Equal(t, report.ReportID, metadataPage.Report.ReportID)
	assert.Equal(t, metadataPage.Sessions, metadataPage.Report.BySession)
	secondResponse := te.get(t, "/api/v1/activity/report/"+report.ReportID+
		"/sessions?limit=1&cursor="+url.QueryEscape(first.NextCursor))
	assertStatus(t, secondResponse, http.StatusOK)
	var second struct {
		Sessions []activity.SessionRow `json:"sessions"`
	}
	require.NoError(t, json.Unmarshal(secondResponse.Body.Bytes(), &second))
	require.Len(t, second.Sessions, 1)
	assert.NotEqual(t, first.Sessions[0].SessionID, second.Sessions[0].SessionID)

	nonDefaultResponse := te.get(t, "/api/v1/activity/report/"+report.ReportID+
		"/sessions?limit=1&sort=project&direction=asc&bucket_start=120&bucket_end=122")
	assertStatus(t, nonDefaultResponse, http.StatusOK)
	var nonDefaultFirst struct {
		Sessions   []activity.SessionRow `json:"sessions"`
		NextCursor string                `json:"next_cursor"`
	}
	require.NoError(t, json.Unmarshal(nonDefaultResponse.Body.Bytes(), &nonDefaultFirst))
	require.Len(t, nonDefaultFirst.Sessions, 1)
	require.NotEmpty(t, nonDefaultFirst.NextCursor)

	inheritedResponse := te.get(t, "/api/v1/activity/report/"+report.ReportID+
		"/sessions?cursor="+url.QueryEscape(nonDefaultFirst.NextCursor))
	assertStatus(t, inheritedResponse, http.StatusOK)
	var inherited struct {
		Sessions []activity.SessionRow `json:"sessions"`
	}
	require.NoError(t, json.Unmarshal(inheritedResponse.Body.Bytes(), &inherited))
	require.Len(t, inherited.Sessions, 1)
	assert.NotEqual(t, nonDefaultFirst.Sessions[0].SessionID, inherited.Sessions[0].SessionID)

	for name, query := range map[string]string{
		"sort":         "&sort=agent",
		"direction":    "&direction=desc",
		"bucket start": "&bucket_start=121&bucket_end=122",
		"bucket end":   "&bucket_start=120&bucket_end=123",
	} {
		t.Run("cursor rejects explicit "+name+" mismatch", func(t *testing.T) {
			mismatch := te.get(t, "/api/v1/activity/report/"+report.ReportID+
				"/sessions?cursor="+url.QueryEscape(nonDefaultFirst.NextCursor)+query)
			assertStatus(t, mismatch, http.StatusBadRequest)
		})
	}

	started, ended := activityDate+"T11:00:00Z", activityDate+"T11:03:00Z"
	te.seedSession(t, "d3", "gamma", 2, func(s *db.Session) {
		s.StartedAt, s.EndedAt = &started, &ended
	})
	te.seedMessages(t, "d3", 2, func(i int, message *db.Message) {
		message.Timestamp = []string{started, activityDate + "T11:02:00Z"}[i]
	})
	refreshResponse := te.get(t, "/api/v1/activity/report/"+report.ReportID+
		"/sessions?limit=1")
	assertStatus(t, refreshResponse, http.StatusOK)
	var refreshed struct {
		RefreshRequired bool             `json:"refresh_required"`
		Report          *activity.Report `json:"report"`
	}
	require.NoError(t, json.Unmarshal(refreshResponse.Body.Bytes(), &refreshed))
	assert.True(t, refreshed.RefreshRequired)
	require.NotNil(t, refreshed.Report)
	assert.Equal(t, 3, refreshed.Report.Totals.Sessions)
	assert.NotEqual(t, report.ReportID, refreshed.Report.ReportID)

	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Accept", "text/event-stream")
	recorder := httptest.NewRecorder()
	te.handler.ServeHTTP(recorder, req)
	assertStatus(t, recorder, http.StatusOK)
	assert.True(t, strings.Contains(recorder.Body.String(), "event: progress\n"))
	assert.True(t, strings.Contains(recorder.Body.String(), "event: report\n"))
}

func TestActivityReportSessionPageRefreshesAfterIdentityOnlyChange(t *testing.T) {
	te := setup(t)
	seedActivityReportFixture(t, te)
	path := "/api/v1/activity/report?preset=day&date=" + activityDate +
		"&timezone=UTC"

	initialResponse := te.get(t, path)
	assertStatus(t, initialResponse, http.StatusOK)
	initial := decode[activity.Report](t, initialResponse)
	require.NotEmpty(t, initial.ReportID)

	require.NoError(t, te.db.UpsertProjectIdentityObservation(
		context.Background(), export.ProjectIdentityObservation{
			Project: "alpha", Machine: "test", RootPath: "/fixtures/alpha",
			GitRemote:     "https://example.com/acme/alpha.git",
			GitRemoteName: "origin",
			ObservedAt:    time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC),
		},
	))

	pageResponse := te.get(t, "/api/v1/activity/report/"+initial.ReportID+
		"/sessions?limit=1")
	assertStatus(t, pageResponse, http.StatusOK)
	var page struct {
		RefreshRequired bool             `json:"refresh_required"`
		Report          *activity.Report `json:"report"`
	}
	require.NoError(t, json.Unmarshal(pageResponse.Body.Bytes(), &page))
	assert.True(t, page.RefreshRequired)
	require.NotNil(t, page.Report)
	assert.NotEqual(t, initial.ReportID, page.Report.ReportID)
	var alphaProjectKey string
	for _, row := range page.Report.BySession {
		if row.Project == "alpha" {
			alphaProjectKey = row.ProjectKey
			break
		}
	}
	require.NotEmpty(t, alphaProjectKey)
	project, ok := page.Report.Projects[alphaProjectKey]
	require.True(t, ok)
	require.NotNil(t, project.Identity,
		"replacement report must carry refreshed project identity metadata")
}
