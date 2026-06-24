package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
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

func TestResolveDefaultSince(t *testing.T) {
	now := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	const utc = "UTC"

	tests := []struct {
		name  string
		since string
		until string
		all   bool
		want  string
	}{
		{
			name: "no flags returns 30-day window",
			want: "2024-05-17",
		},
		{
			name:  "explicit since preserved",
			since: "2024-01-01",
			want:  "2024-01-01",
		},
		{
			name: "all flag disables default",
			all:  true,
			want: "",
		},
		{
			name:  "until without since does not backfill since",
			until: "2024-01-31",
			want:  "",
		},
		{
			name:  "explicit range preserved",
			since: "2024-01-01",
			until: "2024-01-31",
			want:  "2024-01-01",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveDefaultSince(
				tc.since, tc.until, tc.all, now, utc,
			)
			assert.Equal(t, tc.want, got)
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

func TestArchiveQueryBackendNoSyncDoesNotAutostartDaemonForDailyUsage(t *testing.T) {
	newAgentDataDir(t)

	oldStart := startBackgroundServeForTransport
	startBackgroundServeForTransport = func(
		context.Context, *config.Config, time.Duration,
	) (*DaemonRuntime, error) {
		t.Fatal("--no-sync usage must not auto-start a daemon")
		return nil, nil
	}
	t.Cleanup(func() { startBackgroundServeForTransport = oldStart })

	backend, cleanup, err := resolveArchiveQueryBackend(
		context.Background(),
		archiveQueryPolicy{
			NoSync:               true,
			AutoStart:            true,
			ReadOnlyDaemon:       archiveQuerySkipReadOnlyDaemon,
			DirectReadOnlyAction: "refresh usage directly",
		},
	)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	assert.IsType(t, localArchiveQueryBackend{}, backend)
}

func TestArchiveQueryBackendIgnoresReadOnlyDaemonForDailyUsage(t *testing.T) {
	dataDir := newAgentDataDir(t)

	var called bool
	ts := sessionUsageRuntimeServer(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusInternalServerError)
	})
	registerTestRuntime(t, dataDir, ts.URL, true)

	backend, cleanup, err := resolveArchiveQueryBackend(
		context.Background(),
		archiveQueryPolicy{
			AutoStart:            true,
			ReadOnlyDaemon:       archiveQuerySkipReadOnlyDaemon,
			DirectReadOnlyAction: "refresh usage directly",
		},
	)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	assert.IsType(t, localArchiveQueryBackend{}, backend)
	assert.False(t, called)
}

func TestArchiveQueryBackendOfflineSkipsDaemonForDailyUsage(t *testing.T) {
	dataDir := newAgentDataDir(t)
	buildGoldenFixtureDB(t, sessionsDBPath(dataDir))

	var called bool
	ts := sessionUsageRuntimeServer(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusInternalServerError)
	})
	registerSyncRouteTestRuntime(t, dataDir, ts.URL)

	backend, cleanup, err := resolveArchiveQueryBackend(
		context.Background(),
		archiveQueryPolicy{
			Offline:              true,
			AutoStart:            true,
			ReadOnlyDaemon:       archiveQuerySkipReadOnlyDaemon,
			DirectReadOnlyAction: "refresh usage directly",
		},
	)
	require.NoError(t, err)
	t.Cleanup(cleanup)
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
	upsertSession(t, d, "recent", "codex", recent)
	upsertSession(t, d, "old", "codex", old)
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
	assert.Equal(t, 30, all.Totals.InputTokens)
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
	dir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
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
