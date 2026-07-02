package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/cursorusage"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/pricing"
)

func TestFmtCost(t *testing.T) {
	tests := []struct {
		name string
		in   float64
		want string
	}{
		{"zero is $0.00", 0, "$0.00"},
		{"under half a cent shows <$0.01", 0.001, "<$0.01"},
		{"half a cent rounds up to $0.01", 0.005, "$0.01"},
		{"typical cents", 0.45, "$0.45"},
		{"dollars", 12.34, "$12.34"},
		{"rounds to two decimals", 1.23456, "$1.23"},
		{"large value", 1234.56, "$1234.56"},
		// A negative input shouldn't hit the <$0.01 branch.
		{"negative passes through", -0.42, "$-0.42"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, fmtCost(tc.in),
				"fmtCost(%v)", tc.in)
		})
	}
}

func TestDefaultUsageDateRange(t *testing.T) {
	now := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name  string
		from  string
		to    string
		wantF string
		wantT string
	}{
		{
			name:  "no flags returns 30-day window",
			wantF: "2024-05-16",
			wantT: "2024-06-15",
		},
		{
			name:  "explicit from fills to",
			from:  "2024-01-01",
			wantF: "2024-01-01",
			wantT: "2024-06-15",
		},
		{
			name:  "explicit to fills from",
			to:    "2024-01-31",
			wantF: "2024-01-01",
			wantT: "2024-01-31",
		},
		{
			name:  "explicit range preserved",
			from:  "2024-01-01",
			to:    "2024-01-31",
			wantF: "2024-01-01",
			wantT: "2024-01-31",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotF, gotT := defaultUsageDateRange(tc.from, tc.to, now)
			assert.Equal(t, tc.wantF, gotF)
			assert.Equal(t, tc.wantT, gotT)
		})
	}
}

func TestFetchHTTPDailyUsage(t *testing.T) {
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		assert.Equal(t, "/api/v1/usage/summary", r.URL.Path)
		assert.Equal(t, "2026-06-01", r.URL.Query().Get("from"))
		assert.Equal(t, "2026-06-02", r.URL.Query().Get("to"))
		assert.Equal(t, "America/Chicago", r.URL.Query().Get("timezone"))
		assert.Equal(t, "codex", r.URL.Query().Get("agent"))
		assert.Equal(t, "true", r.URL.Query().Get("no_default_range"))
		assert.Equal(t, "false", r.URL.Query().Get("breakdowns"))
		assert.Equal(t, "false", r.URL.Query().Get("session_counts"))
		assert.Equal(t, "true", r.URL.Query().Get("include_one_shot"))
		assert.Equal(t, "true", r.URL.Query().Get("include_automated"))
		gotAuth = r.Header.Get("Authorization")
		writeJSONResponse(w, sampleDailyUsageJSON)
	}))
	defer ts.Close()

	got, err := fetchHTTPDailyUsage(
		context.Background(),
		transport{Mode: transportHTTP, URL: ts.URL},
		"secret-token",
		dailyUsageQuery{
			Filter: db.UsageFilter{
				From:     "2026-06-01",
				To:       "2026-06-02",
				Timezone: "America/Chicago",
				Agent:    "codex",
			},
			NoDefaultRange: true,
		},
	)
	require.NoError(t, err)
	require.Len(t, got.Daily, 1)
	assert.Equal(t, "Bearer secret-token", gotAuth)
	assert.Equal(t, 10, got.Totals.InputTokens)
	assert.Equal(t, 20, got.Daily[0].OutputTokens)
	assert.Equal(t, 1, got.SessionCounts.Total)
}

func TestFetchHTTPDailyUsagePreservesExcludedSessionFilters(t *testing.T) {
	var gotQuery url.Values
	ts := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		gotQuery = r.URL.Query()
		writeJSONResponse(w, emptyDailyUsageJSON)
	}))
	defer ts.Close()

	_, err := fetchHTTPDailyUsage(
		context.Background(),
		transport{Mode: transportHTTP, URL: ts.URL},
		"",
		dailyUsageQuery{
			Filter: db.UsageFilter{
				From:             "2026-06-01",
				ExcludeOneShot:   true,
				ExcludeAutomated: true,
			},
			NoDefaultRange: true,
		},
	)
	require.NoError(t, err)
	assert.Equal(t, "false", gotQuery.Get("include_one_shot"))
	assert.Equal(t, "false", gotQuery.Get("include_automated"))
}

func TestFetchHTTPDailyUsagePreservesOpenEndedRange(t *testing.T) {
	var gotQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		gotQuery = r.URL.RawQuery
		writeJSONResponse(w, emptyDailyUsageJSON)
	}))
	defer ts.Close()

	_, err := fetchHTTPDailyUsage(
		context.Background(),
		transport{Mode: transportHTTP, URL: ts.URL},
		"",
		dailyUsageQuery{
			Filter:         db.UsageFilter{To: "2026-06-02", Timezone: "UTC"},
			NoDefaultRange: true,
		},
	)
	require.NoError(t, err)
	assert.Contains(t, gotQuery, "no_default_range=true")
	assert.NotContains(t, gotQuery, "from=")
	assert.Contains(t, gotQuery, "to=2026-06-02")
}

func TestFetchHTTPDailyUsageAllowsDefaultRangeWhenRangeEmpty(t *testing.T) {
	var gotQuery url.Values
	ts := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		gotQuery = r.URL.Query()
		writeJSONResponse(w, emptyDailyUsageJSON)
	}))
	defer ts.Close()

	_, err := fetchHTTPDailyUsage(
		context.Background(),
		transport{Mode: transportHTTP, URL: ts.URL},
		"",
		dailyUsageQuery{
			Filter:         db.UsageFilter{Timezone: "UTC"},
			NoDefaultRange: false,
		},
	)
	require.NoError(t, err)
	assert.Equal(t, "false", gotQuery.Get("no_default_range"))
	assert.NotContains(t, gotQuery, "from")
	assert.NotContains(t, gotQuery, "to")
}

func TestRunUsageDailyUsesDiscoveredDaemon(t *testing.T) {
	dataDir := newAgentDataDir(t)

	var gotPath string
	ts := sessionUsageRuntimeServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		assert.Equal(t, "2026-06-01", r.URL.Query().Get("from"))
		assert.Equal(t, "2026-06-02", r.URL.Query().Get("to"))
		assert.Equal(t, "true", r.URL.Query().Get("no_default_range"))
		assert.Equal(t, "false", r.URL.Query().Get("breakdowns"))
		assert.Equal(t, "true", r.URL.Query().Get("session_counts"))
		writeJSONResponse(w, sampleDailyUsageJSON)
	})
	registerSyncRouteTestRuntime(t, dataDir, ts.URL)

	out := captureStdout(t, func() {
		runUsageDaily(UsageDailyConfig{
			JSON:     true,
			Since:    "2026-06-01",
			Until:    "2026-06-02",
			Timezone: "UTC",
		})
	})

	assert.Equal(t, "/api/v1/usage/summary", gotPath)
	assert.Contains(t, out, `"totalCost": 0.42`)
	assertNoLocalSessionsDB(t, dataDir)
}

// TestRunUsageDailyResolvesDurationSince proves a duration --since (e.g.
// "14d") is resolved to a concrete YYYY-MM-DD before it reaches the query
// layer, not forwarded verbatim.
func TestRunUsageDailyResolvesDurationSince(t *testing.T) {
	dataDir := newAgentDataDir(t)

	var gotFrom string
	ts := sessionUsageRuntimeServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotFrom = r.URL.Query().Get("from")
		assert.Equal(t, "true", r.URL.Query().Get("no_default_range"))
		writeJSONResponse(w, sampleDailyUsageJSON)
	})
	registerSyncRouteTestRuntime(t, dataDir, ts.URL)

	_ = captureStdout(t, func() {
		runUsageDaily(UsageDailyConfig{
			JSON:     true,
			Since:    "14d",
			Timezone: "UTC",
		})
	})

	// Exact date math is pinned by TestResolveUsageWindow; here we only
	// prove the duration resolved to a concrete date, not forwarded as
	// "14d".
	assert.NotEqual(t, "14d", gotFrom, "duration should be resolved, not forwarded")
	assert.Regexp(t, `^\d{4}-\d{2}-\d{2}$`, gotFrom,
		"duration --since should resolve to a concrete YYYY-MM-DD date")
	assertNoLocalSessionsDB(t, dataDir)
}

func TestResolveUsageWindow(t *testing.T) {
	now := time.Date(2026, 4, 18, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name             string
		since, until     string
		wantFrom, wantTo string
		wantErrSubstring string
	}{
		{name: "empty passes through"},
		{name: "duration since anchors at now", since: "14d", wantFrom: "2026-04-04"},
		{name: "Nh duration since", since: "48h", wantFrom: "2026-04-16"},
		{name: "duration since anchors to explicit until", since: "14d",
			until: "2026-04-10", wantFrom: "2026-03-27", wantTo: "2026-04-10"},
		{name: "duration since anchors to duration until", since: "7d",
			until: "30d", wantFrom: "2026-03-12", wantTo: "2026-03-19"},
		{name: "dates pass through", since: "2026-04-01", until: "2026-04-10",
			wantFrom: "2026-04-01", wantTo: "2026-04-10"},
		{name: "equal bounds are a valid single day", since: "2026-04-10",
			until: "2026-04-10", wantFrom: "2026-04-10", wantTo: "2026-04-10"},
		{name: "garbage since", since: "7x", wantErrSubstring: "invalid --since"},
		{name: "garbage until", until: "nope", wantErrSubstring: "invalid --until"},
		{name: "inverted explicit window", since: "2026-06-20", until: "2026-06-13",
			wantErrSubstring: "must not be after"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			from, to, err := resolveUsageWindow(tc.since, tc.until, now, time.UTC)
			if tc.wantErrSubstring != "" {
				require.Error(t, err, "expected an error")
				assert.Contains(t, err.Error(), tc.wantErrSubstring)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantFrom, from, "from")
			assert.Equal(t, tc.wantTo, to, "to")
		})
	}
}

func TestResolveUsageWindowUsesReportTimezone(t *testing.T) {
	loc, err := time.LoadLocation("America/Los_Angeles")
	require.NoError(t, err)
	now := time.Date(2026, 4, 17, 19, 0, 0, 0, loc)

	tests := []struct {
		name             string
		since, until     string
		wantFrom, wantTo string
	}{
		{
			name:     "duration since formats report local date",
			since:    "1d",
			wantFrom: "2026-04-16",
		},
		{
			name:     "duration since anchors to until in report timezone",
			since:    "1d",
			until:    "2026-04-10",
			wantFrom: "2026-04-09",
			wantTo:   "2026-04-10",
		},
		{
			name:     "explicit dates pass through unchanged",
			since:    "2026-04-01",
			until:    "2026-04-10",
			wantFrom: "2026-04-01",
			wantTo:   "2026-04-10",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			from, to, err := resolveUsageWindow(tc.since, tc.until, now, loc)
			require.NoError(t, err)
			assert.Equal(t, tc.wantFrom, from, "from")
			assert.Equal(t, tc.wantTo, to, "to")
		})
	}
}

func TestRunUsageDailyTableSkipsDaemonSessionCounts(t *testing.T) {
	dataDir := newAgentDataDir(t)

	var gotSessionCounts string
	ts := sessionUsageRuntimeServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotSessionCounts = r.URL.Query().Get("session_counts")
		writeJSONResponse(w, sampleDailyUsageJSON)
	})
	registerSyncRouteTestRuntime(t, dataDir, ts.URL)

	out := captureStdout(t, func() {
		runUsageDaily(UsageDailyConfig{Timezone: "UTC"})
	})

	assert.Equal(t, "false", gotSessionCounts)
	assert.Contains(t, out, "TOTAL")
	assertNoLocalSessionsDB(t, dataDir)
}

func TestRunUsageDailyBreakdownUsesDaemonBreakdowns(t *testing.T) {
	dataDir := newAgentDataDir(t)

	var gotBreakdowns string
	ts := sessionUsageRuntimeServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotBreakdowns = r.URL.Query().Get("breakdowns")
		writeJSONResponse(w, sampleDailyUsageJSON)
	})
	registerSyncRouteTestRuntime(t, dataDir, ts.URL)

	_ = captureStdout(t, func() {
		runUsageDaily(UsageDailyConfig{
			JSON:      true,
			Breakdown: true,
			Timezone:  "UTC",
		})
	})

	assert.Equal(t, "true", gotBreakdowns)
	assertNoLocalSessionsDB(t, dataDir)
}

func TestRunUsageDailyDefaultRangeUsesDaemonDefaults(t *testing.T) {
	dataDir := newAgentDataDir(t)

	var gotQuery url.Values
	ts := sessionUsageRuntimeServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		writeJSONResponse(w, sampleDailyUsageJSON)
	})
	registerSyncRouteTestRuntime(t, dataDir, ts.URL)

	out := captureStdout(t, func() {
		runUsageDaily(UsageDailyConfig{
			JSON:     true,
			Timezone: "UTC",
		})
	})

	assert.Equal(t, "false", gotQuery.Get("no_default_range"))
	assert.NotContains(t, gotQuery, "from")
	assert.NotContains(t, gotQuery, "to")
	assert.Contains(t, out, `"totalCost": 0.42`)
	assertNoLocalSessionsDB(t, dataDir)
}

func TestRunUsageDailyAllPreservesEmptyRangeWithDiscoveredDaemon(t *testing.T) {
	dataDir := newAgentDataDir(t)

	var gotQuery url.Values
	ts := sessionUsageRuntimeServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		writeJSONResponse(w, totalCostOnlyUsageJSON)
	})
	registerSyncRouteTestRuntime(t, dataDir, ts.URL)

	out := captureStdout(t, func() {
		runUsageDaily(UsageDailyConfig{
			JSON:     true,
			All:      true,
			Timezone: "UTC",
		})
	})

	assert.Equal(t, "true", gotQuery.Get("no_default_range"))
	assert.NotContains(t, gotQuery, "from")
	assert.NotContains(t, gotQuery, "to")
	assert.Contains(t, out, `"totalCost": 0.42`)
	assertNoLocalSessionsDB(t, dataDir)
}

func TestRunUsageDailyNoSyncUsesDiscoveredDaemon(t *testing.T) {
	dataDir := newAgentDataDir(t)

	var gotPath string
	ts := sessionUsageRuntimeServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		writeJSONResponse(w, totalCostOnlyUsageJSON)
	})
	registerSyncRouteTestRuntime(t, dataDir, ts.URL)

	out := captureStdout(t, func() {
		runUsageDaily(UsageDailyConfig{
			JSON:     true,
			NoSync:   true,
			Since:    "2026-06-01",
			Until:    "2026-06-02",
			Timezone: "UTC",
		})
	})

	assert.Equal(t, "/api/v1/usage/summary", gotPath)
	assert.Contains(t, out, `"totalCost": 0.42`)
	assertNoLocalSessionsDB(t, dataDir)
}

func TestRunUsageDailyOfflineUsesReadOnlyDBWhenWriteLockHeld(t *testing.T) {
	dataDir := setupGoldenStatsDataDir(t)
	writeCustomModelPricingConfig(t, dataDir)

	lock, err := acquireWriteOwnerLock(context.Background(), dataDir)
	require.NoError(t, err)
	defer func() { require.NoError(t, lock.Close()) }()

	out := captureStdout(t, func() {
		runUsageDaily(UsageDailyConfig{
			JSON:     true,
			Offline:  true,
			Since:    "2026-04-01",
			Until:    "2026-04-15",
			Timezone: "UTC",
		})
	})

	assert.Contains(t, out, `"daily"`)
	assert.Contains(t, out, `"totalCost"`)
	var got db.DailyUsageResult
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Greater(t, got.Totals.TotalCost, 600.0,
		"offline read-only usage must preserve custom pricing")
}

func TestArchiveQueryBackendNoSyncStartsNoSyncDaemonForDailyUsage(t *testing.T) {
	newAgentDataDir(t)
	var started bool
	stubStartBackgroundServeForTransport(t, func(
		_ context.Context, cfg *config.Config, _ time.Duration,
	) (*DaemonRuntime, error) {
		started = true
		assert.True(t, cfg.NoSync)
		return &DaemonRuntime{Host: "127.0.0.1", Port: 12345}, nil
	})

	backend := resolveTestArchiveQueryBackend(t, defaultArchiveQueryPolicy(
		func(p *archiveQueryPolicy) {
			p.NoSync = true
			p.AutoStart = true
		},
	))
	assert.True(t, started)
	assert.IsType(t, daemonArchiveQueryBackend{}, backend)
}

func TestArchiveQueryBackendRefusesReadOnlyDaemonForDailyUsage(t *testing.T) {
	dataDir := newAgentDataDir(t)

	var called bool
	ts := sessionUsageRuntimeServer(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusInternalServerError)
	})
	registerTestRuntime(t, dataDir, ts.URL, true)

	_, cleanup, err := resolveArchiveQueryBackend(
		context.Background(), defaultArchiveQueryPolicy(
			func(p *archiveQueryPolicy) { p.AutoStart = true },
		),
	)
	if cleanup != nil {
		t.Cleanup(cleanup)
	}
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read-only")
	assert.False(t, called)
}

func TestArchiveQueryBackendOfflineSkipsDaemonForDailyUsage(t *testing.T) {
	dataDir := newAgentDataDir(t)
	copyGoldenFixtureDB(t, sessionsDBPath(dataDir))

	var called bool
	ts := sessionUsageRuntimeServer(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusInternalServerError)
	})
	registerSyncRouteTestRuntime(t, dataDir, ts.URL)

	backend := resolveTestArchiveQueryBackend(t, defaultArchiveQueryPolicy(
		func(p *archiveQueryPolicy) {
			p.Offline = true
			p.AutoStart = true
		},
	))
	assert.IsType(t, localArchiveQueryBackend{}, backend)
	assert.False(t, called)
}

func TestLocalArchiveQueryDailyUsageAppliesDefaultRange(t *testing.T) {
	d := newTestDB(t)
	require.NoError(t, d.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:  "test-model",
		InputPerMTok:  1,
		OutputPerMTok: 1,
	}}))

	recent := time.Now().UTC().AddDate(0, 0, -2).Format(time.RFC3339)
	old := time.Now().UTC().AddDate(0, 0, -60).Format(time.RFC3339)
	future := time.Now().UTC().AddDate(0, 0, 2).Format(time.RFC3339)
	upsertSession(t, d, "recent", "codex", recent)
	upsertSession(t, d, "old", "codex", old)
	upsertSession(t, d, "future", "codex", future)
	require.NoError(t, d.InsertMessages([]db.Message{
		{
			SessionID:  "recent",
			Ordinal:    0,
			Role:       "assistant",
			Timestamp:  recent,
			Model:      "test-model",
			TokenUsage: json.RawMessage(`{"input_tokens":10,"output_tokens":1}`),
		},
		{
			SessionID:  "old",
			Ordinal:    0,
			Role:       "assistant",
			Timestamp:  old,
			Model:      "test-model",
			TokenUsage: json.RawMessage(`{"input_tokens":20,"output_tokens":2}`),
		},
		{
			SessionID:  "future",
			Ordinal:    0,
			Role:       "assistant",
			Timestamp:  future,
			Model:      "test-model",
			TokenUsage: json.RawMessage(`{"input_tokens":40,"output_tokens":4}`),
		},
	}))

	backend := localArchiveQueryBackend{
		cfg:           config.Config{},
		database:      d,
		offline:       true,
		skipFreshData: true,
	}
	defaulted, err := backend.DailyUsage(context.Background(), dailyUsageQuery{
		Filter: db.UsageFilter{Timezone: "UTC"},
	})
	require.NoError(t, err)
	assert.Equal(t, 10, defaulted.Totals.InputTokens)

	all, err := backend.DailyUsage(context.Background(), dailyUsageQuery{
		Filter:         db.UsageFilter{Timezone: "UTC"},
		NoDefaultRange: true,
	})
	require.NoError(t, err)
	assert.Equal(t, 70, all.Totals.InputTokens)

	withBreakdowns, err := backend.DailyUsage(context.Background(), dailyUsageQuery{
		Filter:         db.UsageFilter{Timezone: "UTC"},
		NoDefaultRange: true,
		Breakdowns:     true,
	})
	require.NoError(t, err)
	require.NotEmpty(t, withBreakdowns.Daily)
	assert.NotEmpty(t, withBreakdowns.Daily[0].ProjectBreakdowns)
	assert.NotEmpty(t, withBreakdowns.Daily[0].AgentBreakdowns)
}

func TestFormatDailyUsageJSON(t *testing.T) {
	result := db.DailyUsageResult{
		Daily: []db.DailyUsageEntry{
			{
				Date:                "2024-06-15",
				InputTokens:         50000,
				OutputTokens:        12000,
				CacheCreationTokens: 8000,
				CacheReadTokens:     30000,
				TotalCost:           0.45,
				ModelsUsed:          []string{"claude-sonnet-4-20250514"},
				ModelBreakdowns: []db.ModelBreakdown{
					{
						ModelName:           "claude-sonnet-4-20250514",
						InputTokens:         50000,
						OutputTokens:        12000,
						CacheCreationTokens: 8000,
						CacheReadTokens:     30000,
						Cost:                0.45,
					},
				},
			},
		},
		Totals: db.UsageTotals{
			InputTokens:         50000,
			OutputTokens:        12000,
			CacheCreationTokens: 8000,
			CacheReadTokens:     30000,
			TotalCost:           0.45,
		},
	}

	out, err := json.Marshal(result)
	require.NoError(t, err, "json.Marshal failed")

	var decoded map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(out, &decoded),
		"json.Unmarshal failed")

	assert.Contains(t, decoded, "daily", "missing 'daily' key in JSON output")
	assert.Contains(t, decoded, "totals", "missing 'totals' key in JSON output")

	// Verify daily array has expected entry
	var daily []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(decoded["daily"], &daily),
		"parsing daily array")
	require.Len(t, daily, 1, "daily length")

	// Check expected fields exist in daily entry
	wantFields := []string{
		"date", "inputTokens", "outputTokens",
		"cacheCreationTokens", "cacheReadTokens",
		"totalCost", "modelsUsed", "modelBreakdowns",
	}
	for _, f := range wantFields {
		assert.Contains(t, daily[0], f,
			"missing field %q in daily entry", f)
	}

	// Verify totals fields
	var totals map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(decoded["totals"], &totals),
		"parsing totals")
	totalFields := []string{
		"inputTokens", "outputTokens",
		"cacheCreationTokens", "cacheReadTokens",
		"totalCost",
	}
	for _, f := range totalFields {
		assert.Contains(t, totals, f,
			"missing field %q in totals", f)
	}
}

func TestNewUsageCursorCommandUsesConfigFallbacksAndSharedPagination(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	require.NoError(t, os.WriteFile(
		filepath.Join(dataDir, "config.toml"),
		[]byte(
			"cursor_admin_api_key = 'config-key'\n"+
				"cursor_admin_email = 'config@example.com'\n"+
				"cursor_admin_user_id = '152683922'\n",
		),
		0o600,
	), "write config")

	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/teams/filtered-usage-events", r.URL.Path)
		require.Equal(t, http.MethodPost, r.Method)
		user, pass, ok := r.BasicAuth()
		require.True(t, ok, "basic auth")
		assert.Equal(t, "config-key", user)
		assert.Empty(t, pass)

		var req map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req), "decode request")
		requests = append(requests, req)

		page, _ := req["page"].(float64)
		switch int(page) {
		case 1:
			_, _ = w.Write([]byte(`{
				"totalUsageEventsCount": 2,
				"usageEvents": [{
					"timestamp": "1778753100000",
					"model": "claude-4.6-opus-high-thinking",
					"kind": "USAGE_EVENT_KIND_USAGE_BASED",
					"tokenUsage": {
						"inputTokens": 1234,
						"outputTokens": 567,
						"cacheWriteTokens": 12,
						"cacheReadTokens": 34
					},
					"chargedCents": 15.66,
					"cursorTokenFee": 3.32,
					"userId": "152683922",
					"userEmail": "config@example.com",
					"isHeadless": false
				}]
			}`))
		case 2:
			_, _ = w.Write([]byte(`{
				"totalUsageEventsCount": 2,
				"usageEvents": [{
					"timestamp": "2026-05-14T11:05:00Z",
					"model": "gpt-5.4",
					"kind": "USAGE_EVENT_KIND_USAGE_BASED",
					"tokenUsage": {
						"inputTokens": 1,
						"outputTokens": 2,
						"cacheWriteTokens": 3,
						"cacheReadTokens": 4
					},
					"chargedCents": 1.5,
					"cursorTokenFee": 0.5,
					"userId": "152683922",
					"userEmail": "config@example.com",
					"isHeadless": true
				}]
			}`))
		default:
			t.Fatalf("unexpected page request: %#v", req)
		}
	}))
	t.Cleanup(server.Close)

	origNewCursorUsageClient := newCursorUsageClient
	newCursorUsageClient = func(apiKey string) *cursorusage.Client {
		return cursorusage.NewClientWithBaseURL(server.URL, apiKey)
	}
	t.Cleanup(func() {
		newCursorUsageClient = origNewCursorUsageClient
	})

	cmd := newUsageCursorCommand()
	cmd.SetArgs([]string{
		"--since", "2026-05-14",
		"--until", "2026-05-14",
		"--page-size", "1",
	})
	out := captureStdout(t, func() {
		require.NoError(t, cmd.Execute(), "Execute")
	})
	assert.Contains(t, out, "Fetched 2 Cursor usage events into the archive")

	require.Len(t, requests, 2, "request count")
	for _, req := range requests {
		assert.Equal(t, "config@example.com", req["email"])
		assert.Equal(t, float64(152683922), req["userId"])
		assert.Equal(t, float64(1), req["pageSize"])
		assert.IsType(t, float64(0), req["startDate"])
		assert.IsType(t, float64(0), req["endDate"])
	}
	assert.Equal(t, float64(1), requests[0]["page"])
	assert.Equal(t, float64(2), requests[1]["page"])

	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))

	var count int
	require.NoError(t, database.Reader().QueryRow(
		"SELECT count(*) FROM cursor_usage_events",
	).Scan(&count))
	assert.Equal(t, 2, count)

	rows, err := database.Reader().Query(
		`SELECT occurred_at, model, input_tokens, output_tokens,
			cache_write_tokens, cache_read_tokens, user_email, is_headless
		FROM cursor_usage_events
		ORDER BY occurred_at ASC`,
	)
	require.NoError(t, err, "query cursor events")
	defer rows.Close()

	type storedEvent struct {
		occurredAt       string
		model            string
		inputTokens      int
		outputTokens     int
		cacheWriteTokens int
		cacheReadTokens  int
		userEmail        string
		isHeadless       int
	}
	var got []storedEvent
	for rows.Next() {
		var ev storedEvent
		require.NoError(t, rows.Scan(
			&ev.occurredAt,
			&ev.model,
			&ev.inputTokens,
			&ev.outputTokens,
			&ev.cacheWriteTokens,
			&ev.cacheReadTokens,
			&ev.userEmail,
			&ev.isHeadless,
		))
		got = append(got, ev)
	}
	require.NoError(t, rows.Err(), "iterate cursor events")
	require.Len(t, got, 2)
	assert.Equal(t, "2026-05-14T10:05:00Z", got[0].occurredAt)
	assert.Equal(t, "claude-4.6-opus-high-thinking", got[0].model)
	assert.Equal(t, 1234, got[0].inputTokens)
	assert.Equal(t, 567, got[0].outputTokens)
	assert.Equal(t, 12, got[0].cacheWriteTokens)
	assert.Equal(t, 34, got[0].cacheReadTokens)
	assert.Equal(t, "config@example.com", got[0].userEmail)
	assert.Equal(t, 0, got[0].isHeadless)
	assert.Equal(t, "2026-05-14T11:05:00Z", got[1].occurredAt)
	assert.Equal(t, 1, got[1].inputTokens)
	assert.Equal(t, 2, got[1].outputTokens)
	assert.Equal(t, 3, got[1].cacheWriteTokens)
	assert.Equal(t, 4, got[1].cacheReadTokens)
	assert.Equal(t, 1, got[1].isHeadless)
}

func TestResolveCursorUsageWindowUntilOnlyUsesDefaultLookback(t *testing.T) {
	loc := time.UTC

	start, end, err := resolveCursorUsageWindow(UsageCursorConfig{
		Until: "2026-05-31",
	}, loc)

	require.NoError(t, err)
	assert.Equal(t, time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), start)
	assert.Equal(t,
		time.Date(2026, 5, 31, 23, 59, 59, int(999*time.Millisecond), time.UTC),
		end,
	)
}

func TestResolveCursorUsageWindowRejectsInvertedRange(t *testing.T) {
	_, _, err := resolveCursorUsageWindow(UsageCursorConfig{
		Since: "2026-06-01",
		Until: "2026-05-31",
	}, time.UTC)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "after until")
}

func TestNewUsageCursorCommandExplicitMemberFilterDoesNotReuseConfigSibling(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantEmail any
		wantUser  any
	}{
		{
			name:      "explicit email replaces both configured filters",
			args:      []string{"--email", "other@example.com"},
			wantEmail: "other@example.com",
			wantUser:  nil,
		},
		{
			name:      "explicit empty email clears both configured filters",
			args:      []string{"--email="},
			wantEmail: nil,
			wantUser:  nil,
		},
		{
			name:      "explicit user id replaces both configured filters",
			args:      []string{"--user-id", "987654321"},
			wantEmail: nil,
			wantUser:  float64(987654321),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
			require.NoError(t, os.WriteFile(
				filepath.Join(dataDir, "config.toml"),
				[]byte(
					"cursor_admin_api_key = 'config-key'\n"+
						"cursor_admin_email = 'config@example.com'\n"+
						"cursor_admin_user_id = '152683922'\n",
				),
				0o600,
			), "write config")

			var request map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.NoError(t, json.NewDecoder(r.Body).Decode(&request),
					"decode request")
				_, _ = w.Write([]byte(`{
					"totalUsageEventsCount": 0,
					"usageEvents": []
				}`))
			}))
			t.Cleanup(server.Close)

			origNewCursorUsageClient := newCursorUsageClient
			newCursorUsageClient = func(apiKey string) *cursorusage.Client {
				return cursorusage.NewClientWithBaseURL(server.URL, apiKey)
			}
			t.Cleanup(func() {
				newCursorUsageClient = origNewCursorUsageClient
			})

			args := []string{
				"--since", "2026-05-14",
				"--until", "2026-05-14",
			}
			args = append(args, tc.args...)

			cmd := newUsageCursorCommand()
			cmd.SetArgs(args)
			require.NoError(t, cmd.Execute(), "Execute")

			require.NotNil(t, request, "request")
			assert.Equal(t, tc.wantEmail, request["email"])
			assert.Equal(t, tc.wantUser, request["userId"])
		})
	}
}

func TestRefreshPricingIfStale_FreshAttemptSkipsFetch(t *testing.T) {
	d := newTestDB(t)
	now := pricingTestNow()

	// Last attempt 10 minutes ago, cooldown 1 hour: skip.
	prev := seedPricingAttempt(t, d, now, 10*time.Minute)

	fetcher := &pricingFetchRecorder{}
	refreshed, err := refreshPricingIfStale(
		d, fetcher.fetch, pricingTestCooldown, now,
	)
	require.NoError(t, err)
	assert.False(t, refreshed, "refreshed = true, want false within cooldown")
	assert.Zero(t, fetcher.calls, "fetch should not run within cooldown")

	// Meta value preserved (we did not overwrite it).
	assertPricingAttemptMeta(t, d, prev)
}

func TestRefreshPricingIfStale_StaleTriggersFetch(t *testing.T) {
	d := newTestDB(t)
	now := pricingTestNow()

	// Last attempt 2 hours ago, cooldown 1 hour: refresh.
	seedPricingAttempt(t, d, now, 2*time.Hour)

	fetcher := &pricingFetchRecorder{rows: []pricing.ModelPricing{{
		ModelPattern:  "gpt-5.5",
		InputPerMTok:  1.25,
		OutputPerMTok: 10.0,
	}}}
	refreshed, err := refreshPricingIfStale(
		d, fetcher.fetch, pricingTestCooldown, now,
	)
	require.NoError(t, err)
	require.True(t, refreshed, "refreshed = false, want true after cooldown")

	// Pricing row written.
	p, err := d.GetModelPricing("gpt-5.5")
	require.NoError(t, err)
	require.NotNil(t, p, "gpt-5.5 row missing")
	assert.Equal(t, 10.0, p.OutputPerMTok)

	// Meta updated to now.
	assertPricingAttemptMeta(t, d, now.Format(time.RFC3339))
}

func TestRefreshPricingIfStale_NeverAttemptedTriggersFetch(t *testing.T) {
	d := newTestDB(t)
	now := pricingTestNow()

	fetcher := &pricingFetchRecorder{}
	refreshed, err := refreshPricingIfStale(
		d, fetcher.fetch, pricingTestCooldown, now,
	)
	require.NoError(t, err)
	assert.Equal(t, 1, fetcher.calls, "fetch should run when meta empty")
	assert.True(t, refreshed, "refreshed = false, want true on first attempt")
}

func TestRefreshPricingIfStale_FetchFailureRecordsAttempt(t *testing.T) {
	d := newTestDB(t)
	now := pricingTestNow()

	wantErr := errors.New("network down")
	fetcher := &pricingFetchRecorder{err: wantErr}
	refreshed, err := refreshPricingIfStale(
		d, fetcher.fetch, pricingTestCooldown, now,
	)
	assert.ErrorIs(t, err, wantErr)
	assert.False(t, refreshed, "refreshed = true, want false on fetch failure")

	// Cooldown still recorded so a persistent failure doesn't
	// retry on every CLI call.
	assertPricingAttemptMeta(t, d, now.Format(time.RFC3339))

	// A second call within cooldown skips the fetch entirely.
	second := &pricingFetchRecorder{}
	_, err = refreshPricingIfStale(
		d, second.fetch, pricingTestCooldown, now.Add(time.Minute),
	)
	require.NoError(t, err)
	assert.Zero(t, second.calls, "second call should be suppressed by cooldown")
}

func TestEnsurePricingWithFetcherSkipsFetchWithinCooldown(t *testing.T) {
	d := newTestDB(t)
	now := pricingTestNow()

	seedPricingAttempt(t, d, now, 10*time.Minute)

	fetcher := &pricingFetchRecorder{rows: []pricing.ModelPricing{{
		ModelPattern:  "network-only-model",
		InputPerMTok:  1,
		OutputPerMTok: 1,
	}}}
	refreshed, err := ensurePricingWithFetcher(d, false, fetcher.fetch, now)
	require.NoError(t, err)
	assert.False(t, refreshed)
	assert.Zero(t, fetcher.calls, "fetch should not run within cooldown")

	fallback, err := d.GetModelPricing("gpt-5.5")
	require.NoError(t, err)
	require.NotNil(t, fallback, "fallback pricing should be seeded")

	networkOnly, err := d.GetModelPricing("network-only-model")
	require.NoError(t, err)
	assert.Nil(t, networkOnly, "cooldown should prevent network upsert")
}

// sampleDailyUsageJSON is a full usage summary body with a single day and
// non-zero totals, shared by the HTTP and daemon usage tests.
const sampleDailyUsageJSON = `{
	"from": "2026-06-01",
	"to": "2026-06-02",
	"totals": {
		"inputTokens": 10,
		"outputTokens": 20,
		"totalCost": 0.42
	},
	"daily": [{
		"date": "2026-06-01",
		"inputTokens": 10,
		"outputTokens": 20,
		"totalCost": 0.42,
		"modelsUsed": ["gpt-5.1"]
	}],
	"sessionCounts": {
		"total": 1,
		"byProject": {"proj": 1},
		"byAgent": {"codex": 1}
	}
}`

// emptyDailyUsageJSON is an empty usage summary used when the test only
// inspects the outbound request.
const emptyDailyUsageJSON = `{"totals":{},"daily":[]}`

// totalCostOnlyUsageJSON carries a non-zero total cost but no daily rows.
const totalCostOnlyUsageJSON = `{"totals":{"totalCost":0.42},"daily":[]}`

// pricingTestCooldown is the cooldown used by the pricing refresh tests.
const pricingTestCooldown = time.Hour

// newAgentDataDir creates a temp data dir and points AGENTSVIEW_DATA_DIR at it.
func newAgentDataDir(t *testing.T) string {
	t.Helper()
	dir := testDataDir(t)
	return dir
}

// sessionsDBPath returns the canonical sessions.db path under dataDir.
func sessionsDBPath(dataDir string) string {
	return filepath.Join(dataDir, "sessions.db")
}

// assertNoLocalSessionsDB fails if a local sessions.db was created, which would
// mean a remote/daemon path unexpectedly opened a local database.
func assertNoLocalSessionsDB(t *testing.T, dataDir string) {
	t.Helper()
	assert.NoFileExists(t, sessionsDBPath(dataDir))
}

// writeJSONResponse writes body as a JSON HTTP response.
func writeJSONResponse(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

// pricingTestNow is the fixed clock used by the pricing refresh tests.
func pricingTestNow() time.Time {
	return time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
}

// seedPricingAttempt records a pricing refresh attempt aged `age` before now
// and returns the RFC3339 timestamp written.
func seedPricingAttempt(
	t *testing.T, d *db.DB, now time.Time, age time.Duration,
) string {
	t.Helper()
	ts := now.Add(-age).Format(time.RFC3339)
	require.NoError(t, d.SetPricingMeta(pricingRefreshMetaKey, ts))
	return ts
}

// assertPricingAttemptMeta asserts the stored refresh attempt timestamp.
func assertPricingAttemptMeta(t *testing.T, d *db.DB, want string) {
	t.Helper()
	got, err := d.GetPricingMeta(pricingRefreshMetaKey)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

// pricingFetchRecorder is a fake pricing fetcher that records call counts and
// returns canned rows or an error.
type pricingFetchRecorder struct {
	calls int
	rows  []pricing.ModelPricing
	err   error
}

func (f *pricingFetchRecorder) fetch() ([]pricing.ModelPricing, error) {
	f.calls++
	return f.rows, f.err
}

// zeroTotalsCopilotUsageJSON is a daily-usage summary with sessions present
// but zero token/cost totals — the "no token data" case.
const zeroTotalsCopilotUsageJSON = `{
  "daily": [],
  "totals": {"inputTokens":0,"outputTokens":0,"cacheCreationTokens":0,"cacheReadTokens":0,"totalCost":0},
  "sessionCounts": {"total":2,"byProject":{},"byAgent":{"copilot":2}}
}`

func TestRunUsageDailyHintsNoTokenDataForCopilot(t *testing.T) {
	dataDir := newAgentDataDir(t)
	ts := sessionUsageRuntimeServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(w, zeroTotalsCopilotUsageJSON)
	})
	registerSyncRouteTestRuntime(t, dataDir, ts.URL)

	stderr := captureStderr(t, func() {
		runUsageDaily(UsageDailyConfig{Agent: "copilot", Timezone: "UTC"})
	})

	assert.Contains(t, stderr, "Copilot")
	assert.Contains(t, stderr, "do not include token or cost data")
}

func TestRunUsageDailyNoHintWithoutAgentFilter(t *testing.T) {
	dataDir := newAgentDataDir(t)
	ts := sessionUsageRuntimeServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(w, zeroTotalsCopilotUsageJSON)
	})
	registerSyncRouteTestRuntime(t, dataDir, ts.URL)

	stderr := captureStderr(t, func() {
		runUsageDaily(UsageDailyConfig{Timezone: "UTC"})
	})

	assert.NotContains(t, stderr, "token")
}

func TestRunUsageDailyNoHintWhenDataPresent(t *testing.T) {
	dataDir := newAgentDataDir(t)
	ts := sessionUsageRuntimeServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(w, sampleDailyUsageJSON)
	})
	registerSyncRouteTestRuntime(t, dataDir, ts.URL)

	stderr := captureStderr(t, func() {
		runUsageDaily(UsageDailyConfig{Agent: "codex", Timezone: "UTC"})
	})

	assert.NotContains(t, stderr, "token-usage")
}

func TestNoTokenDataNote(t *testing.T) {
	zero := db.UsageTotals{}
	withData := db.UsageTotals{OutputTokens: 5}
	copilotNote := "note: these GitHub Copilot records do not include token " +
		"or cost data that agentsview can total."
	cases := []struct {
		name   string
		agent  string
		totals db.UsageTotals
		want   string
	}{
		{"no agent filter", "", zero, ""},
		{"non-copilot agent with zero totals", "codex", zero, ""},
		{"copilot with token data", "copilot", withData, ""},
		{"copilot with zero totals", "copilot", zero, copilotNote},
		{"vscode-copilot with zero totals", "vscode-copilot", zero, copilotNote},
		{"all-copilot CSV filter", "copilot,vscode-copilot", zero, copilotNote},
		{"mixed CSV filter", "copilot,claude", zero, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want,
				noTokenDataNote(tc.agent, tc.totals))
		})
	}
}
