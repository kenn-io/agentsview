package server_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

const basePath = "/api/v1/analytics/"

// seedStats holds expected values after seeding the database.
type seedStats struct {
	TotalSessions          int
	TotalMessages          int
	ActiveProjects         int
	TotalToolCalls         int
	TotalSkillCalls        int
	Agents                 int
	ActiveDays             int
	TotalOutputTokens      int
	TokenReportingSessions int
	TopSessionOutputTokens int
}

// seedAnalyticsEnv populates the test env with sessions and
// messages suitable for analytics endpoint tests. Some messages
// include tool_calls for tool analytics testing.
func seedAnalyticsEnv(t *testing.T, te *testEnv) seedStats {
	t.Helper()

	type entry struct {
		id, project, agent, started, model string
		msgs                               int
	}
	entries := []entry{
		{"a1", "alpha", "claude", "2024-06-01T09:00:00Z", "claude-3-5-sonnet", 10},
		{"a2", "alpha", "codex", "2024-06-01T14:00:00Z", "gpt-4o", 20},
		{"b1", "beta", "claude", "2024-06-02T10:00:00Z", "claude-3-5-sonnet", 30},
	}

	stats := seedStats{
		TotalSessions: len(entries),
	}

	projects := make(map[string]bool)
	agents := make(map[string]bool)
	days := make(map[string]bool)
	writes := make([]db.SessionBatchWrite, 0, len(entries))

	for _, s := range entries {
		projects[s.project] = true
		agents[s.agent] = true
		if len(s.started) >= 10 {
			days[s.started[:10]] = true
		}

		stats.TotalMessages += s.msgs
		started := s.started
		msgs := buildTestMessages(s.id, s.msgs,
			func(i int, m *db.Message) {
				// Skill analytics now buckets and filters by message
				// timestamp, so align messages with the session window.
				m.Timestamp = started
				m.Model = s.model
				// Add tool calls on every other assistant msg
				if m.Role == "assistant" && i%4 == 1 {
					m.HasToolUse = true
					m.ToolCalls = []db.ToolCall{
						{
							SessionID: s.id,
							ToolName:  "Read",
							Category:  "Read",
							SkillName: "review-code",
						},
					}
					stats.TotalToolCalls++
					stats.TotalSkillCalls++
				}
			},
		)
		writes = append(writes, db.SessionBatchWrite{
			Session: db.Session{
				ID:               s.id,
				Project:          s.project,
				Machine:          "test",
				Agent:            s.agent,
				MessageCount:     s.msgs,
				UserMessageCount: max(s.msgs, 2),
				StartedAt:        &started,
				EndedAt:          &started,
				FirstMessage:     new("Hello"),
			},
			Messages: msgs,
		})
	}
	result, err := te.db.WriteSessionBatchAtomic(writes)
	require.NoError(t, err)
	require.Equal(t, len(entries), result.WrittenSessions)
	require.Equal(t, stats.TotalMessages, result.WrittenMessages)

	stats.ActiveProjects = len(projects)
	stats.Agents = len(agents)
	stats.ActiveDays = len(days)

	return stats
}

func seedAnalyticsTokenEnv(t *testing.T, te *testEnv) seedStats {
	t.Helper()

	type entry struct {
		id, project, agent, started string
		msgs, outputTokens          int
		hasTokens                   bool
	}

	entries := []entry{
		{"tok-01", "alpha", "claude", "2024-06-01T09:00:00Z", 12, 9000, true},
		{"tok-02", "alpha", "codex", "2024-06-01T10:00:00Z", 11, 7000, true},
		{"tok-03", "beta", "claude", "2024-06-01T11:00:00Z", 10, 6000, true},
		{"tok-04", "beta", "codex", "2024-06-01T12:00:00Z", 9, 5000, true},
		{"tok-05", "gamma", "claude", "2024-06-02T09:00:00Z", 8, 4000, true},
		{"tok-06", "gamma", "codex", "2024-06-02T10:00:00Z", 7, 3000, true},
		{"tok-07", "delta", "claude", "2024-06-02T11:00:00Z", 6, 2000, true},
		{"tok-08", "delta", "codex", "2024-06-02T12:00:00Z", 5, 1500, true},
		{"tok-09", "epsilon", "claude", "2024-06-03T09:00:00Z", 4, 1200, true},
		{"tok-10", "epsilon", "codex", "2024-06-03T10:00:00Z", 3, 1100, true},
		{"tok-11", "zeta", "claude", "2024-06-03T11:00:00Z", 2, 1000, true},
		{"tok-12", "zeta", "codex", "2024-06-03T12:00:00Z", 2, 200, true},
		{"tok-missing", "omega", "claude", "2024-06-03T13:00:00Z", 40, 0, false},
	}

	var stats seedStats
	writes := make([]db.SessionBatchWrite, 0, len(entries))
	for _, s := range entries {
		stats.TotalSessions++
		stats.TotalMessages += s.msgs
		started := s.started
		writes = append(writes, db.SessionBatchWrite{
			Session: db.Session{
				ID:                   s.id,
				Project:              s.project,
				Machine:              "test",
				Agent:                s.agent,
				MessageCount:         s.msgs,
				UserMessageCount:     max(s.msgs, 2),
				StartedAt:            &started,
				EndedAt:              &started,
				FirstMessage:         new("Token seeded"),
				TotalOutputTokens:    s.outputTokens,
				HasTotalOutputTokens: s.hasTokens,
			},
			Messages: buildTestMessages(s.id, s.msgs),
		})
		if s.hasTokens {
			stats.TotalOutputTokens += s.outputTokens
			stats.TokenReportingSessions++
			if s.outputTokens > stats.TopSessionOutputTokens {
				stats.TopSessionOutputTokens = s.outputTokens
			}
		}
	}
	result, err := te.db.WriteSessionBatchAtomic(writes)
	require.NoError(t, err)
	require.Equal(t, stats.TotalSessions, result.WrittenSessions)
	require.Equal(t, stats.TotalMessages, result.WrittenMessages)

	return stats
}

type analyticsDBFixture struct {
	files map[string][]byte
	stats seedStats
}

var (
	analyticsFixtureOnce sync.Once
	analyticsFixture     analyticsDBFixture
	analyticsFixtureErr  error

	analyticsTokenFixtureOnce sync.Once
	analyticsTokenFixture     analyticsDBFixture
	analyticsTokenFixtureErr  error
)

func setupAnalyticsEnv(t *testing.T) (*testEnv, seedStats) {
	t.Helper()
	fixture := analyticsFixtureFor(t,
		&analyticsFixtureOnce,
		&analyticsFixture,
		&analyticsFixtureErr,
		"analytics",
		seedAnalyticsEnv,
	)
	return setupWithDBTemplate(t, fixture.files), fixture.stats
}

func setupAnalyticsTokenEnv(t *testing.T) (*testEnv, seedStats) {
	t.Helper()
	fixture := analyticsFixtureFor(t,
		&analyticsTokenFixtureOnce,
		&analyticsTokenFixture,
		&analyticsTokenFixtureErr,
		"analytics-token",
		seedAnalyticsTokenEnv,
	)
	return setupWithDBTemplate(t, fixture.files), fixture.stats
}

func analyticsFixtureFor(
	t *testing.T,
	once *sync.Once,
	fixture *analyticsDBFixture,
	fixtureErr *error,
	name string,
	seed func(*testing.T, *testEnv) seedStats,
) analyticsDBFixture {
	t.Helper()
	once.Do(func() {
		*fixture, *fixtureErr = buildAnalyticsDBFixture(t, name, seed)
	})
	require.NoError(t, *fixtureErr)
	return *fixture
}

func buildAnalyticsDBFixture(
	t *testing.T,
	name string,
	seed func(*testing.T, *testEnv) seedStats,
) (analyticsDBFixture, error) {
	t.Helper()
	dir, err := os.MkdirTemp("", "agentsview-server-"+name+"-*")
	if err != nil {
		return analyticsDBFixture{}, fmt.Errorf(
			"creating analytics fixture dir: %w", err,
		)
	}
	defer os.RemoveAll(dir)

	path := filepath.Join(dir, "test.db")
	dbtest.EnsureTestDBAt(t, path)
	database, err := db.Open(path)
	if err != nil {
		return analyticsDBFixture{}, fmt.Errorf(
			"opening analytics fixture db: %w", err,
		)
	}
	stats := seed(t, &testEnv{db: database})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	checkpointErr := database.CheckpointWALTruncate(ctx)
	closeErr := database.Close()
	if checkpointErr != nil {
		return analyticsDBFixture{}, fmt.Errorf(
			"checkpointing analytics fixture db: %w", checkpointErr,
		)
	}
	if closeErr != nil {
		return analyticsDBFixture{}, fmt.Errorf(
			"closing analytics fixture db: %w", closeErr,
		)
	}
	files, err := readClosedDBFiles(path)
	if err != nil {
		return analyticsDBFixture{}, err
	}
	return analyticsDBFixture{files: files, stats: stats}, nil
}

func readClosedDBFiles(path string) (map[string][]byte, error) {
	files := make(map[string][]byte, 3)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		data, err := os.ReadFile(path + suffix)
		if err != nil {
			if suffix != "" && errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf(
				"reading analytics fixture db %s: %w",
				path+suffix, err,
			)
		}
		files[suffix] = data
	}
	return files, nil
}

// buildPathURL constructs an API URL for a given full path and parameters.
func buildPathURL(fullPath string, params map[string]string) string {
	u, _ := url.Parse(fullPath)
	q := u.Query()
	for k, v := range params {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// buildURL constructs an analytics API URL.
func buildURL(path string, params map[string]string) string {
	return buildPathURL(basePath+path, params)
}

// buildURLWithRange constructs an analytics API URL with default from/to params.
func buildURLWithRange(path string, params map[string]string) string {
	if params == nil {
		params = make(map[string]string)
	}
	if _, ok := params["from"]; !ok {
		params["from"] = "2024-06-01"
	}
	if _, ok := params["to"]; !ok {
		params["to"] = "2024-06-03"
	}
	return buildURL(path, params)
}

func TestAnalyticsSummary(t *testing.T) {
	te, stats := setupAnalyticsEnv(t)

	t.Run("OK", func(t *testing.T) {
		w := te.get(t, buildURLWithRange("summary", map[string]string{"timezone": "UTC"}))
		assertStatus(t, w, http.StatusOK)

		resp := decode[db.AnalyticsSummary](t, w)
		assert.Equal(t, stats.TotalSessions, resp.TotalSessions)
		assert.Equal(t, stats.TotalMessages, resp.TotalMessages)
		assert.Equal(t, stats.ActiveProjects, resp.ActiveProjects)
		assert.Equal(t, stats.ActiveDays, resp.ActiveDays)
		assert.Equal(t,
			[]string{"claude-3-5-sonnet", "gpt-4o"},
			resp.Models,
		)
	})

	t.Run("ModelFilter", func(t *testing.T) {
		w := te.get(t, buildURLWithRange("summary", map[string]string{
			"timezone": "UTC",
			"model":    "gpt-4o",
		}))
		assertStatus(t, w, http.StatusOK)

		resp := decode[db.AnalyticsSummary](t, w)
		assert.Equal(t, 1, resp.TotalSessions)
		assert.Equal(t, 20, resp.TotalMessages)
		assert.Equal(t, []string{"gpt-4o"}, resp.Models)
	})

	t.Run("NonUTCTimezone", func(t *testing.T) {
		w := te.get(t, buildURLWithRange("summary", map[string]string{"timezone": "America/New_York"}))
		assertStatus(t, w, http.StatusOK)
	})

	t.Run("InvalidTimezone", func(t *testing.T) {
		w := te.get(t, buildURL("summary", map[string]string{"timezone": "Fake/Zone"}))
		assertStatus(t, w, http.StatusBadRequest)
	})
}

func TestAnalyticsSummary_OutputTokenCoverage(t *testing.T) {
	te, stats := setupAnalyticsTokenEnv(t)

	w := te.get(t, buildURLWithRange("summary", map[string]string{"timezone": "UTC"}))
	assertStatus(t, w, http.StatusOK)

	resp := decode[db.AnalyticsSummary](t, w)
	assert.Equal(t, stats.TotalOutputTokens, resp.TotalOutputTokens)
	assert.Equal(t, stats.TokenReportingSessions, resp.TokenReportingSessions)
}

func TestAnalyticsSummary_DateValidation(t *testing.T) {
	te := setup(t)

	tests := []struct {
		name   string
		params map[string]string
		status int
	}{
		{
			"InvalidFromFormat",
			map[string]string{"from": "not-a-date", "to": "2024-06-03"},
			http.StatusBadRequest,
		},
		{
			"InvalidToFormat",
			map[string]string{"from": "2024-06-01", "to": "06-03-2024"},
			http.StatusBadRequest,
		},
		{
			"FromAfterTo",
			map[string]string{"from": "2024-07-01", "to": "2024-06-01"},
			http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := te.get(t, buildURL("summary", tt.params))
			assertStatus(t, w, tt.status)
		})
	}
}

func TestAnalyticsErrorRedaction(t *testing.T) {
	te, _ := setupAnalyticsEnv(t)

	// Valid request should succeed
	w := te.get(t, buildURLWithRange("summary", nil))
	assertStatus(t, w, http.StatusOK)

	// Force a DB error by closing the database
	te.db.Close()

	endpoints := []string{
		"summary",
		"activity",
		"heatmap",
		"projects",
		"hour-of-week",
		"sessions",
		"velocity",
		"tools",
		"top-sessions",
		"signals",
	}
	for _, ep := range endpoints {
		t.Run(ep, func(t *testing.T) {
			w := te.get(t, buildURLWithRange(ep, nil))
			assertStatus(t, w, http.StatusInternalServerError)
			body := w.Body.String()
			assert.NotContains(t, body, "sql", "response exposes internal error")
			assert.NotContains(t, body, "database", "response exposes internal error")
		})
	}
}

func TestAnalyticsSignalSessionsRejectsUnsupportedSignal(t *testing.T) {
	te, _ := setupAnalyticsEnv(t)

	w := te.get(t, buildURLWithRange("signal-sessions",
		map[string]string{"signal": "not_a_signal"}))
	assertStatus(t, w, http.StatusBadRequest)
}

func TestAnalyticsEndpoints_DefaultParams(t *testing.T) {
	te, _ := setupAnalyticsEnv(t)

	endpoints := []string{
		"summary",
		"activity",
		"heatmap",
		"projects",
		"hour-of-week",
		"sessions",
		"velocity",
		"tools",
		"top-sessions",
		"signals",
	}

	for _, ep := range endpoints {
		t.Run(ep, func(t *testing.T) {
			w := te.get(t, buildURL(ep, nil))
			assertStatus(t, w, http.StatusOK)
		})
	}
}

func TestSessionsDateValidation(t *testing.T) {
	te := setup(t)

	tests := []struct {
		name   string
		params map[string]string
		status int
	}{
		{
			"InvalidDateFormat",
			map[string]string{"date": "not-a-date"},
			http.StatusBadRequest,
		},
		{
			"InvalidDateFromFormat",
			map[string]string{"date_from": "2024/06/01"},
			http.StatusBadRequest,
		},
		{
			"DateFromAfterDateTo",
			map[string]string{"date_from": "2024-07-01", "date_to": "2024-06-01"},
			http.StatusBadRequest,
		},
		{
			"ValidDateRange",
			map[string]string{"date_from": "2024-06-01", "date_to": "2024-06-03"},
			http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := te.get(t, buildPathURL("/api/v1/sessions", tt.params))
			assertStatus(t, w, tt.status)
		})
	}
}

func TestActiveSinceValidation(t *testing.T) {
	te := setup(t)

	tests := []struct {
		name   string
		path   string
		params map[string]string
		status int
	}{
		{
			"Sessions_InvalidActiveSince",
			"/api/v1/sessions",
			map[string]string{"active_since": "garbage"},
			http.StatusBadRequest,
		},
		{
			"Sessions_ValidActiveSince",
			"/api/v1/sessions",
			map[string]string{"active_since": "2024-06-01T10:00:00Z"},
			http.StatusOK,
		},
		{
			"Sessions_ValidActiveSinceNano",
			"/api/v1/sessions",
			map[string]string{"active_since": "2024-06-01T10:00:00.123456789Z"},
			http.StatusOK,
		},
		{
			"Analytics_InvalidActiveSince",
			basePath + "summary",
			map[string]string{"from": "2024-06-01", "to": "2024-06-03", "active_since": "not-a-timestamp"},
			http.StatusBadRequest,
		},
		{
			"Analytics_ValidActiveSince",
			basePath + "summary",
			map[string]string{"from": "2024-06-01", "to": "2024-06-03", "active_since": "2024-06-01T00:00:00Z"},
			http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := te.get(t, buildPathURL(tt.path, tt.params))
			assertStatus(t, w, tt.status)
		})
	}
}

// TestDateFilterValidation pins that the content-search and secrets-list
// endpoints reject malformed date filters with 400 instead of forwarding them
// to the DB, where a date/timestamptz cast failure would surface as a 500.
func TestDateFilterValidation(t *testing.T) {
	te := setup(t)

	tests := []struct {
		name   string
		path   string
		params map[string]string
		status int
	}{
		{
			"Search_InvalidDate", "/api/v1/search/content",
			map[string]string{"pattern": "x", "date": "not-a-date"},
			http.StatusBadRequest,
		},
		{
			"Search_InvalidDateFrom", "/api/v1/search/content",
			map[string]string{"pattern": "x", "date_from": "2024-13-40"},
			http.StatusBadRequest,
		},
		{
			"Search_DateFromAfterDateTo", "/api/v1/search/content",
			map[string]string{
				"pattern": "x", "date_from": "2024-06-03", "date_to": "2024-06-01",
			},
			http.StatusBadRequest,
		},
		{
			"Search_InvalidActiveSince", "/api/v1/search/content",
			map[string]string{"pattern": "x", "active_since": "garbage"},
			http.StatusBadRequest,
		},
		{
			"Search_ValidDates", "/api/v1/search/content",
			map[string]string{
				"pattern": "x", "date_from": "2024-06-01", "date_to": "2024-06-03",
			},
			http.StatusOK,
		},
		{
			"Secrets_InvalidDateFrom", "/api/v1/secrets",
			map[string]string{"date_from": "bad-date"},
			http.StatusBadRequest,
		},
		{
			"Secrets_ValidDates", "/api/v1/secrets",
			map[string]string{"date_from": "2024-06-01", "date_to": "2024-06-03"},
			http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := te.get(t, buildPathURL(tt.path, tt.params))
			assertStatus(t, w, tt.status)
		})
	}
}

func TestAnalyticsActivity(t *testing.T) {
	te, stats := setupAnalyticsEnv(t)

	tests := []struct {
		name        string
		granularity string
		wantStatus  int
	}{
		{"DayGranularity", "day", http.StatusOK},
		{"WeekGranularity", "week", http.StatusOK},
		{"DefaultGranularity", "", http.StatusOK},
		{"InvalidGranularity", "hour", http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params := make(map[string]string)
			if tt.granularity != "" {
				params["granularity"] = tt.granularity
			}
			w := te.get(t, buildURLWithRange("activity", params))
			assertStatus(t, w, tt.wantStatus)

			if tt.wantStatus == http.StatusOK {
				resp := decode[db.ActivityResponse](t, w)
				expectedGran := tt.granularity
				if expectedGran == "" {
					expectedGran = "day" // default
				}
				assert.Equal(t, expectedGran, resp.Granularity)
				if expectedGran == "day" {
					require.Len(t, resp.Series, stats.ActiveDays)
					totalUser := 0
					totalAsst := 0
					for _, e := range resp.Series {
						totalUser += e.UserMessages
						totalAsst += e.AssistantMessages
					}
					assert.Equal(t, stats.TotalMessages, totalUser+totalAsst)
				}
			}
		})
	}
}

func TestAnalyticsHeatmap(t *testing.T) {
	te, stats := setupAnalyticsEnv(t)

	tests := []struct {
		name        string
		metric      string
		wantStatus  int
		wantEntries int
	}{
		{"MessageMetric", "messages", http.StatusOK, 3},
		{"SessionMetric", "sessions", http.StatusOK, 3},
		{"DefaultMetric", "", http.StatusOK, 3},
		{"InvalidMetric", "bytes", http.StatusBadRequest, -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params := make(map[string]string)
			if tt.metric != "" {
				params["metric"] = tt.metric
			}
			w := te.get(t, buildURLWithRange("heatmap", params))
			assertStatus(t, w, tt.wantStatus)

			if tt.wantStatus == http.StatusOK {
				resp := decode[db.HeatmapResponse](t, w)
				expectedMetric := tt.metric
				if expectedMetric == "" {
					expectedMetric = "messages" // default
				}
				assert.Equal(t, expectedMetric, resp.Metric)
				if tt.wantEntries >= 0 {
					assert.Len(t, resp.Entries, tt.wantEntries)
				}
				if tt.wantEntries > 0 {
					total := 0
					for _, e := range resp.Entries {
						total += e.Value
					}
					switch expectedMetric {
					case "messages":
						assert.Equal(t, stats.TotalMessages, total)
					case "sessions":
						assert.Equal(t, stats.TotalSessions, total)
					}
				}
			}
		})
	}

	t.Run("ClampedRange", func(t *testing.T) {
		// Request a range >366 days; entries should be clamped
		params := map[string]string{
			"from": "2022-01-01",
			"to":   "2024-06-03",
		}
		w := te.get(t, buildURL("heatmap", params))
		assertStatus(t, w, http.StatusOK)

		resp := decode[db.HeatmapResponse](t, w)
		assert.LessOrEqual(t, len(resp.Entries), db.MaxHeatmapDays)
		require.NotEmpty(t, resp.EntriesFrom)
		assert.Greater(t, resp.EntriesFrom, "2022-01-01",
			"EntriesFrom should be later than 2022-01-01")
	})

	t.Run("ShortRange_NoClamping", func(t *testing.T) {
		// A 3-day range should not be clamped
		w := te.get(t, buildURLWithRange("heatmap", nil))
		assertStatus(t, w, http.StatusOK)

		resp := decode[db.HeatmapResponse](t, w)
		assert.Equal(t, "2024-06-01", resp.EntriesFrom)
	})

	t.Run("Levels_FromClampedWindow", func(t *testing.T) {
		// Seed a historical outlier far outside the clamped
		// window. Levels should be based only on displayed data.
		oldDate := "2020-01-15T10:00:00Z"
		te.seedSession(t, "old-outlier", "gamma", 500,
			func(sess *db.Session) {
				sess.Agent = "claude"
				sess.StartedAt = &oldDate
				sess.EndedAt = &oldDate
			},
		)
		te.seedMessages(t, "old-outlier", 500)

		// Request range covering both old and recent data
		params := map[string]string{
			"from": "2019-01-01",
			"to":   "2024-06-03",
		}
		w := te.get(t, buildURL("heatmap", params))
		assertStatus(t, w, http.StatusOK)

		resp := decode[db.HeatmapResponse](t, w)

		// The outlier at 2020-01-15 should be clamped out.
		// Verify no entry has the outlier date.
		for _, e := range resp.Entries {
			assert.NotEqual(t, "2020-01-15", e.Date,
				"outlier date should be outside clamped window")
		}

		// Levels should reflect the recent data (max ~30 msgs),
		// not the 500-message outlier.
		assert.Less(t, resp.Levels.L4, 500,
			"L4 should be << 500 (outlier leaked into levels)")
	})
}

func TestAnalyticsHeatmap_OutputTokens(t *testing.T) {
	te, stats := setupAnalyticsTokenEnv(t)

	w := te.get(t, buildURLWithRange("heatmap", map[string]string{
		"timezone": "UTC",
		"metric":   "output_tokens",
	}))
	assertStatus(t, w, http.StatusOK)

	resp := decode[db.HeatmapResponse](t, w)
	require.Equal(t, "output_tokens", resp.Metric)

	total := 0
	for _, e := range resp.Entries {
		total += e.Value
	}
	assert.Equal(t, stats.TotalOutputTokens, total)
}

func TestAnalyticsHeatmap_OutputTokensNoReporting(
	t *testing.T,
) {
	te, _ := setupAnalyticsEnv(t)

	w := te.get(t, buildURLWithRange("heatmap", map[string]string{
		"timezone": "UTC",
		"metric":   "output_tokens",
	}))
	assertStatus(t, w, http.StatusOK)

	resp := decode[db.HeatmapResponse](t, w)
	require.Equal(t, "output_tokens", resp.Metric)
	assert.Empty(t, resp.Entries,
		"no sessions report token coverage")
}

func TestAnalyticsProjects(t *testing.T) {
	te, stats := setupAnalyticsEnv(t)

	t.Run("OK", func(t *testing.T) {
		w := te.get(t, buildURLWithRange("projects", nil))
		assertStatus(t, w, http.StatusOK)

		resp := decode[db.ProjectsAnalyticsResponse](t, w)
		require.Len(t, resp.Projects, stats.ActiveProjects)

		total := 0
		for _, p := range resp.Projects {
			total += p.Messages
		}
		assert.Equal(t, stats.TotalMessages, total)
	})

	t.Run("MachineFilter", func(t *testing.T) {
		w := te.get(t, buildURLWithRange("projects", map[string]string{"machine": "nonexistent"}))
		assertStatus(t, w, http.StatusOK)

		resp := decode[db.ProjectsAnalyticsResponse](t, w)
		assert.Empty(t, resp.Projects)
	})
}

func TestAnalyticsHourOfWeek(t *testing.T) {
	te, _ := setupAnalyticsEnv(t)

	t.Run("OK", func(t *testing.T) {
		w := te.get(t, buildURLWithRange("hour-of-week", map[string]string{"timezone": "UTC"}))
		assertStatus(t, w, http.StatusOK)

		resp := decode[db.HourOfWeekResponse](t, w)
		assert.Len(t, resp.Cells, 168)
	})
}

func TestAnalyticsSessionShape(t *testing.T) {
	te, stats := setupAnalyticsEnv(t)

	t.Run("OK", func(t *testing.T) {
		w := te.get(t, buildURLWithRange("sessions", map[string]string{"timezone": "UTC"}))
		assertStatus(t, w, http.StatusOK)

		resp := decode[db.SessionShapeResponse](t, w)
		assert.Equal(t, stats.TotalSessions, resp.Count)
	})
}

func TestAnalyticsVelocity(t *testing.T) {
	te, stats := setupAnalyticsEnv(t)

	t.Run("OK", func(t *testing.T) {
		w := te.get(t, buildURLWithRange("velocity", map[string]string{"timezone": "UTC"}))
		assertStatus(t, w, http.StatusOK)

		resp := decode[db.VelocityResponse](t, w)
		assert.Len(t, resp.ByAgent, stats.Agents)
	})
}

func TestAnalyticsTools(t *testing.T) {
	te, stats := setupAnalyticsEnv(t)

	t.Run("OK", func(t *testing.T) {
		w := te.get(t, buildURLWithRange("tools", map[string]string{"timezone": "UTC"}))
		assertStatus(t, w, http.StatusOK)

		resp := decode[db.ToolsAnalyticsResponse](t, w)
		assert.Equal(t, stats.TotalToolCalls, resp.TotalCalls)
		assert.NotEmpty(t, resp.ByCategory)
		assert.Len(t, resp.ByAgent, stats.Agents)
	})

	t.Run("WithProjectFilter", func(t *testing.T) {
		w := te.get(t, buildURLWithRange("tools", map[string]string{"project": "alpha", "timezone": "UTC"}))
		assertStatus(t, w, http.StatusOK)

		resp := decode[db.ToolsAnalyticsResponse](t, w)
		assert.NotZero(t, resp.TotalCalls, "TotalCalls for alpha")
	})

	t.Run("InvalidTimezone", func(t *testing.T) {
		w := te.get(t, buildURL("tools", map[string]string{"timezone": "Fake/Zone"}))
		assertStatus(t, w, http.StatusBadRequest)
	})
}

func TestAnalyticsSkills(t *testing.T) {
	te, stats := setupAnalyticsEnv(t)

	t.Run("OK", func(t *testing.T) {
		w := te.get(t, buildURLWithRange("skills", map[string]string{"timezone": "UTC"}))
		assertStatus(t, w, http.StatusOK)

		resp := decode[db.SkillsAnalyticsResponse](t, w)
		assert.Equal(t, stats.TotalSkillCalls, resp.TotalSkillCalls)
		assert.Equal(t, 1, resp.DistinctSkills)
		require.NotEmpty(t, resp.BySkill)
		assert.Equal(t, "review-code", resp.BySkill[0].SkillName)
	})

	t.Run("WithProjectFilter", func(t *testing.T) {
		w := te.get(t, buildURLWithRange("skills", map[string]string{"project": "alpha", "timezone": "UTC"}))
		assertStatus(t, w, http.StatusOK)

		resp := decode[db.SkillsAnalyticsResponse](t, w)
		assert.NotZero(t, resp.TotalSkillCalls, "TotalSkillCalls for alpha")
	})

	t.Run("InvalidTimezone", func(t *testing.T) {
		w := te.get(t, buildURL("skills", map[string]string{"timezone": "Fake/Zone"}))
		assertStatus(t, w, http.StatusBadRequest)
	})
}

func TestAnalyticsTopSessions(t *testing.T) {
	te, stats := setupAnalyticsEnv(t)

	tests := []struct {
		name       string
		metric     string
		project    string
		wantStatus int
	}{
		{"ByMessages", "messages", "", http.StatusOK},
		{"ByDuration", "duration", "", http.StatusOK},
		{"DefaultMetric", "", "", http.StatusOK},
		{"InvalidMetric", "bytes", "", http.StatusBadRequest},
		{"WithProjectFilter", "", "alpha", http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params := make(map[string]string)
			if tt.metric != "" {
				params["metric"] = tt.metric
			}
			if tt.project != "" {
				params["project"] = tt.project
			}
			if tt.wantStatus == http.StatusOK {
				params["timezone"] = "UTC"
			}

			w := te.get(t, buildURLWithRange("top-sessions", params))
			assertStatus(t, w, tt.wantStatus)

			if tt.wantStatus == http.StatusOK {
				resp := decode[db.TopSessionsResponse](t, w)
				expectedMetric := tt.metric
				if expectedMetric == "" {
					expectedMetric = "messages"
				}
				assert.Equal(t, expectedMetric, resp.Metric)
				if tt.project == "" {
					expected := min(stats.TotalSessions, 10)
					assert.Len(t, resp.Sessions, expected)
				}
				if tt.project != "" {
					assert.NotEmpty(t, resp.Sessions, "project %q", tt.project)
					for _, s := range resp.Sessions {
						assert.Equal(t, tt.project, s.Project)
					}
				}
			}
		})
	}
}

func TestAnalyticsTopSessions_OutputTokens(t *testing.T) {
	te, stats := setupAnalyticsTokenEnv(t)

	w := te.get(t, buildURLWithRange("top-sessions", map[string]string{
		"timezone": "UTC",
		"metric":   "output_tokens",
	}))
	assertStatus(t, w, http.StatusOK)

	resp := decode[db.TopSessionsResponse](t, w)
	require.Equal(t, "output_tokens", resp.Metric)
	require.NotEmpty(t, resp.Sessions)
	assert.Equal(t, stats.TopSessionOutputTokens, resp.Sessions[0].OutputTokens)
}

// TestSessionCountConsistency verifies that session counts from
// /api/v1/sessions, /api/v1/stats, and /api/v1/analytics/summary
// all agree. This catches regressions where one endpoint counts
// sub-agent, fork, or empty sessions that others exclude.
func TestSessionCountConsistency(t *testing.T) {
	te := setup(t)

	// Seed root sessions with messages (should be counted).
	for i := range 5 {
		id := fmt.Sprintf("root-%d", i)
		te.seedSession(t, id, "proj-a", 10,
			func(s *db.Session) {
				s.Agent = "claude"
				s.StartedAt = new(
					"2024-06-01T09:00:00Z")
				s.EndedAt = new(
					"2024-06-01T10:00:00Z")
			},
		)
		te.seedMessages(t, id, 10)
	}

	// Seed sub-agent sessions. These are excluded from the session
	// list and db-wide stats (navigation surfaces) but counted in the
	// analytics summary (a token/session aggregate), so the two counts
	// intentionally differ.
	for i := range 3 {
		id := fmt.Sprintf("subagent-%d", i)
		te.seedSession(t, id, "proj-a", 8,
			func(s *db.Session) {
				s.Agent = "claude"
				s.ParentSessionID = new("root-0")
				s.RelationshipType = "subagent"
				s.StartedAt = new(
					"2024-06-01T09:00:00Z")
				s.EndedAt = new(
					"2024-06-01T10:00:00Z")
			},
		)
		te.seedMessages(t, id, 8)
	}

	// Seed fork sessions (should NOT be counted).
	for i := range 2 {
		id := fmt.Sprintf("fork-%d", i)
		te.seedSession(t, id, "proj-a", 6,
			func(s *db.Session) {
				s.Agent = "claude"
				s.ParentSessionID = new("root-1")
				s.RelationshipType = "fork"
				s.StartedAt = new(
					"2024-06-01T09:00:00Z")
				s.EndedAt = new(
					"2024-06-01T10:00:00Z")
			},
		)
		te.seedMessages(t, id, 6)
	}

	// Seed empty sessions (should NOT be counted).
	for i := range 4 {
		id := fmt.Sprintf("empty-%d", i)
		te.seedSession(t, id, "proj-a", 0,
			func(s *db.Session) {
				s.Agent = "claude"
				s.StartedAt = new(
					"2024-06-01T09:00:00Z")
				s.EndedAt = new(
					"2024-06-01T10:00:00Z")
			},
		)
	}

	// Seed continuation sessions (SHOULD be counted).
	te.seedSession(t, "cont-0", "proj-a", 5,
		func(s *db.Session) {
			s.Agent = "claude"
			s.ParentSessionID = new("root-2")
			s.RelationshipType = "continuation"
			s.StartedAt = new(
				"2024-06-01T09:00:00Z")
			s.EndedAt = new(
				"2024-06-01T10:00:00Z")
		},
	)
	te.seedMessages(t, "cont-0", 5)

	// Navigation surfaces (list, stats) exclude subagents: 5 root + 1
	// continuation. The analytics summary additionally counts the 3
	// subagents, since their messages and tokens are real spend.
	wantNavCount := 6
	wantAnalyticsCount := 9

	// 1. Session list
	w := te.get(t, "/api/v1/sessions")
	assertStatus(t, w, http.StatusOK)
	listResp := decode[sessionListResponse](t, w)

	// 2. Stats
	w = te.get(t, "/api/v1/stats")
	assertStatus(t, w, http.StatusOK)
	statsResp := decode[db.Stats](t, w)

	// 3. Analytics summary
	w = te.get(t, buildURLWithRange("summary", map[string]string{
		"timezone": "UTC",
	}))
	assertStatus(t, w, http.StatusOK)
	summaryResp := decode[db.AnalyticsSummary](t, w)

	assert.Equal(t, wantNavCount, listResp.Total, "session list total")
	assert.Equal(t, wantNavCount, statsResp.SessionCount, "stats session_count")
	assert.Equal(t, wantAnalyticsCount, summaryResp.TotalSessions,
		"analytics total_sessions counts subagents")

	// List and stats (navigation) agree; analytics counts subagents on
	// top, so it is intentionally higher.
	require.Equal(t, listResp.Total, statsResp.SessionCount,
		"navigation session counts disagree: list=%d stats=%d",
		listResp.Total, statsResp.SessionCount)
	require.Greater(t, summaryResp.TotalSessions, listResp.Total,
		"analytics should count more than navigation: analytics=%d list=%d",
		summaryResp.TotalSessions, listResp.Total)
}
