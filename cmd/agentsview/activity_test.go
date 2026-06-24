package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/pricing"
)

func TestNewActivityCommand_RegistersReport(t *testing.T) {
	cmd := newActivityCommand()
	assert.Equal(t, "activity", cmd.Name())
	sub, _, err := cmd.Find([]string{"report"})
	require.NoError(t, err)
	assert.Equal(t, "report", sub.Name())
}

func TestActivityReportCommand_Flags(t *testing.T) {
	cmd := newActivityReportCommand()
	for _, name := range []string{
		"preset", "date", "from", "to", "timezone",
		"bucket", "project", "agent", "machine", "json", "no-sync",
		"offline",
	} {
		assert.NotNilf(t, cmd.Flags().Lookup(name), "flag --%s must exist", name)
	}
}

func TestActivityReportCommand_HelpText(t *testing.T) {
	cmd := newActivityReportCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--help"})
	require.NoError(t, cmd.Execute())
	out := buf.String()
	assert.Contains(t, out, "--preset")
	assert.Contains(t, out, "--json")
}

func TestResolveActivityReport_BadBucket(t *testing.T) {
	d := newTestDB(t)
	_, err := resolveActivityReport(ActivityReportConfig{
		Preset: "day", Date: "2026-06-16", Timezone: "UTC", Bucket: "2h",
	}, d)
	require.Error(t, err, "off-allow-list bucket is rejected before query")
}

func TestResolveActivityReport_JSONShape(t *testing.T) {
	d := newTestDB(t)
	r, err := resolveActivityReport(ActivityReportConfig{
		Preset: "day", Date: "2026-06-16", Timezone: "UTC",
	}, d)
	require.NoError(t, err)
	assert.Equal(t, "minute", r.BucketUnit)
	assert.Equal(t, "2026-06-16T00:00:00Z", r.RangeStart)
}

func TestActivityReport_UsesDiscoveredDaemon(t *testing.T) {
	dataDir := testDataDir(t)

	var gotQuery url.Values
	ts := daemonRouteTestServer(t, map[string]http.HandlerFunc{
		"/api/v1/activity/report": func(w http.ResponseWriter, r *http.Request) {
			gotQuery = r.URL.Query()
			writeJSONResponse(w, `{
				"timezone":"UTC",
				"range_start":"2026-06-16T00:00:00Z",
				"range_end":"2026-06-17T00:00:00Z",
				"bucket_unit":"1h",
				"bucket_seconds":3600,
				"bucket_count":24,
				"effective_end":"2026-06-17T00:00:00Z",
				"elapsed_bucket_count":24,
				"buckets":[],
				"peak":{"agents":0},
				"totals":{"sessions":3},
				"by_project":[],
				"by_model":[],
				"by_agent":[],
				"by_session":[],
				"intervals":[]
			}`)
		},
	})
	registerSyncRouteTestRuntime(t, dataDir, ts.URL)

	out := captureStdout(t, func() {
		runActivityReport(ActivityReportConfig{
			JSON: true, Preset: "day",
			Date: "2026-06-16", Timezone: "UTC",
		})
	})
	assert.Equal(t, "day", gotQuery.Get("preset"))
	assert.Equal(t, "2026-06-16", gotQuery.Get("date"))
	assert.Equal(t, "UTC", gotQuery.Get("timezone"))

	var payload activity.Report
	require.NoError(t, json.Unmarshal([]byte(out), &payload))
	assert.Equal(t, "UTC", payload.Timezone)
	assert.Equal(t, 3, payload.Totals.Sessions)
	assert.NoFileExists(t, filepath.Join(dataDir, "sessions.db"))
}

// mustLocation loads a named time zone, failing the test if it is unavailable.
func mustLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	require.NoError(t, err)
	return loc
}

func TestFmtRangeBound_RendersInTimezone(t *testing.T) {
	chicago := mustLocation(t, "America/Chicago")
	// 05:00Z is local midnight in Chicago (CDT, UTC-5) in June.
	assert.Equal(t, "2026-06-16", fmtRangeBound("2026-06-16T05:00:00Z", chicago))
	// UTC midnight renders date-only in UTC.
	assert.Equal(t, "2026-06-16", fmtRangeBound("2026-06-16T00:00:00Z", time.UTC))
	// A non-midnight bound keeps the local time component.
	assert.Equal(t, "2026-06-16 12:30", fmtRangeBound("2026-06-16T12:30:00Z", time.UTC))
}

func TestFmtInstant_NilAndTimezone(t *testing.T) {
	assert.Equal(t, "—", fmtInstant(nil, time.UTC))
	chicago := mustLocation(t, "America/Chicago")
	ts := "2026-06-16T05:30:00Z" // 00:30 CDT
	assert.Equal(t, "2026-06-16 00:30", fmtInstant(&ts, chicago))
}

// TestPrintActivityReport_SanitizesSessionDerivedStrings confirms the
// human-readable activity output strips control/escape bytes from
// session-derived fields (breakdown keys and session title/project/agent), so
// crafted imported or synced metadata cannot drive terminal escape sequences.
// JSON output is left untouched and is covered separately.
func TestPrintActivityReport_SanitizesSessionDerivedStrings(t *testing.T) {
	mins := 1.0
	// OSC title-set + BEL, then a bare CR overwrite: all control bytes stripped.
	evil := "\x1b]0;pwned\x07safe\rEVIL"
	r := activity.Report{
		Timezone:   "UTC",
		RangeStart: "2026-06-16T00:00:00Z",
		RangeEnd:   "2026-06-17T00:00:00Z",
		BucketUnit: "minute",
		ByProject:  []activity.KeyMinutes{{Key: evil, AgentMinutes: 1}},
		BySession: []activity.SessionRow{{
			SessionID:    "s1",
			Title:        evil,
			Project:      evil,
			Agent:        evil,
			AgentMinutes: &mins,
		}},
	}

	out := captureStdout(t, func() { printActivityReport(r) })

	assert.NotContains(t, out, "\x1b", "ESC must be stripped from output")
	assert.NotContains(t, out, "\x07", "BEL must be stripped from output")
	assert.NotContains(t, out, "\r", "bare CR must be stripped from output")
	assert.Contains(t, out, "safeEVIL",
		"printable text survives once control bytes are removed")
}

// fallbackPricedModel returns a model pattern from the offline fallback table
// that carries a non-zero output rate, so seeding it prices output tokens.
func fallbackPricedModel(t *testing.T) string {
	t.Helper()
	for _, p := range pricing.FallbackPricing() {
		if p.OutputPerMTok > 0 {
			return p.ModelPattern
		}
	}
	require.FailNow(t, "no fallback model with a non-zero output rate")
	return ""
}

// TestResolveActivityReport_PricesFreshDBUsage proves the pricing fix: a fresh
// DB holding unpriced token usage is priced because resolveActivityReportPriced
// seeds the fallback rates before resolving, exactly as runActivityReport does,
// so the report's cost is non-zero.
func TestResolveActivityReport_PricesFreshDBUsage(t *testing.T) {
	d := newTestDB(t)
	model := fallbackPricedModel(t)

	started := "2026-06-15T10:00:00Z"
	ended := "2026-06-15T10:05:00Z"
	usage, err := json.Marshal(map[string]int{"input_tokens": 100, "output_tokens": 500})
	require.NoError(t, err)
	first := "first message"
	_, err = d.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: db.Session{
			ID: "cost-1", Project: "alpha", Machine: "local", Agent: "claude",
			StartedAt: &started, EndedAt: &ended, CreatedAt: started,
			FirstMessage: &first, MessageCount: 2, UserMessageCount: 1,
			RelationshipType: "root", DataVersion: 1,
		},
		Messages: []db.Message{
			{SessionID: "cost-1", Ordinal: 0, Role: "user", Content: "u",
				Timestamp: started, ContentLength: 1},
			{SessionID: "cost-1", Ordinal: 1, Role: "assistant", Content: "a",
				Timestamp: ended, ContentLength: 1, Model: model,
				TokenUsage: usage, OutputTokens: 500, HasOutputTokens: true},
		},
		DataVersion: 1, ReplaceMessages: true,
	}})
	require.NoError(t, err)

	// resolveActivityReportPriced seeds fallback pricing (Offline => no network)
	// exactly as runActivityReport does, so removing that seeding fails here.
	r, err := resolveActivityReportPriced(ActivityReportConfig{
		Preset: "day", Date: "2026-06-15", Timezone: "UTC", Offline: true,
	}, d, nil)
	require.NoError(t, err)
	assert.Equal(t, 500, r.Totals.OutputTokens)
	assert.Greater(t, r.Totals.Cost, 0.0,
		"resolveActivityReportPriced must seed fallback pricing for fresh-DB usage")
}

func TestRunActivityReportOfflineUsesReadOnlyDBWhenWriteLockHeld(t *testing.T) {
	dataDir := setupGoldenStatsDataDir(t)

	lock, err := acquireWriteOwnerLock(context.Background(), dataDir)
	require.NoError(t, err)
	defer func() { require.NoError(t, lock.Close()) }()

	out := captureStdout(t, func() {
		runActivityReport(ActivityReportConfig{
			Preset:   "day",
			Date:     "2026-04-04",
			Timezone: "UTC",
			Offline:  true,
		})
	})

	assert.Contains(t, out, "Activity 2026-04-04 to 2026-04-05")
	assert.Contains(t, out, "Sessions")
}
