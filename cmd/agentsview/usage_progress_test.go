package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

func TestUsageCommandsDeferStartupSyncOnlyForDaily(t *testing.T) {
	for _, command := range []string{"daily", "statusline", "session usage", "token-use"} {
		t.Run(command, func(t *testing.T) {
			newAgentDataDir(t)
			var ts *httptest.Server
			if command == "daily" || command == "statusline" {
				ts = sessionUsageRuntimeServer(t, func(w http.ResponseWriter, r *http.Request) {
					if command == "daily" {
						writeUsageStreamResponse(t, w, r, sampleDailyUsageJSON)
					} else {
						writeJSONResponse(w, sampleDailyUsageJSON)
					}
				})
			} else {
				ts, _ = newRemoteUsageServer(t, remoteUsageSpec{canonicalID: "codex:session-a"})
			}
			started := false
			stubStartBackgroundServeForTransport(t, func(_ context.Context, cfg *config.Config, _ time.Duration) (*DaemonRuntime, error) {
				started = true
				assert.Equal(t, command == "daily", cfg.SkipInitialSync)
				assert.False(t, cfg.NoSync)
				return daemonRuntimeFromTestURL(t, ts.URL), nil
			})
			captureStdout(t, func() {
				switch command {
				case "daily":
					runUsageDaily(UsageDailyConfig{JSON: true, Timezone: "UTC"})
				case "statusline":
					runUsageStatusline(UsageStatuslineConfig{JSON: true})
				case "session usage":
					cmd := sessionUsageCommand(t, "session", "usage", "codex:session-a")
					_, _, err := sessionUsageDataForCommand(cmd, "codex:session-a")
					require.NoError(t, err)
				case "token-use":
					_, _, err := sessionUsageData("codex:session-a")
					require.NoError(t, err)
				}
			})
			assert.True(t, started, "exercise daemon startup rather than an existing daemon")
		})
	}
}

func TestUsageProgressRejectsDaemonWithoutStreamEndpoint(t *testing.T) {
	err := daemonRuntimeCompatibilityError(&DaemonRuntime{
		API: 8, Data: db.CurrentDataVersion(),
	})
	require.ErrorContains(t, err, "restart the daemon")
}

func writeUsageStreamResponse(t *testing.T, w http.ResponseWriter, r *http.Request, body string) {
	t.Helper()
	assert.Equal(t, "/api/v1/usage/summary/stream", r.URL.Path)
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, "event: done\ndata: ", strings.ReplaceAll(body, "\n", ""), "\n\n")
}

func TestFetchHTTPDailyUsageStreamsProgressAndResult(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/usage/summary/stream", r.URL.Path)
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: progress\ndata: {\"detail\":\"Preparing usage data for 2 sessions in this report\"}\n\n")
		http.NewResponseController(w).Flush()
		fmt.Fprint(w, "event: done\ndata: ", strings.ReplaceAll(sampleDailyUsageJSON, "\n", ""), "\n\n")
	}))
	t.Cleanup(ts.Close)
	var phases []string
	got, err := fetchHTTPDailyUsage(context.Background(), transport{URL: ts.URL}, "test-token", dailyUsageQuery{
		Progress: func(phase string) { phases = append(phases, phase) },
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"Preparing usage data for 2 sessions in this report"}, phases)
	require.Len(t, got.Daily, 1)
	assert.Equal(t, "2026-06-01", got.Daily[0].Date)
	assert.Equal(t, int64(420_000), got.Totals.TotalCost.Microdollars)
}

func TestReadUsageSummaryStreamReportsFailures(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"failed query", "event: error\ndata: {\"error\":\"could not read usage data\"}\n\n", "could not read usage data"},
		{"interrupted report", "event: progress\ndata: {\"detail\":\"Calculating daily totals\"}\n\n", "connection closed before the report finished"},
		{"invalid progress", "event: progress\ndata: invalid\n\n", "reading progress"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := readUsageSummaryStream(strings.NewReader(tc.input), func(string) {})
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestUsageProgressPrintsSlowWorkToStderr(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var output syncBuffer
		print, finish := newUsageProgressPrinter(&output)
		defer finish()
		print("Reading archived sessions for this report")
		assert.Empty(t, output.String(), "a warm report should stay quiet")
		print("Preparing usage data for 2 sessions in this report")
		time.Sleep(time.Second)
		synctest.Wait()
		assert.Contains(t, output.String(), "Preparing usage data for 2 sessions in this report (1s)")
		time.Sleep(5 * time.Second)
		synctest.Wait()
		assert.Contains(t, output.String(), "(6s)", "long preparation must keep reporting its elapsed time")
	})
}
