package main

import (
	"encoding/json/v2"
	"net"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/server"
)

// seedPlanUsageCLIArchive writes one plan-only codex-cloud session, one
// codex-cloud session with captured tokens and one missed turn, and one
// Claude session with tokens, all on 2026-09-10.
func seedPlanUsageCLIArchive(t *testing.T) {
	t.Helper()
	dataDir := testDataDir(t)
	t.Setenv("TZ", "UTC")
	database := dbtest.OpenTestDBAt(t, sessionsDBPath(dataDir))
	started := "2026-09-10T10:00:00Z"
	session := func(id, agent string) db.Session {
		return db.Session{
			ID: id, Project: "alpha", Machine: "local", Agent: agent,
			StartedAt: &started, EndedAt: &started, CreatedAt: started,
			MessageCount: 3, UserMessageCount: 2, DataVersion: 1,
		}
	}
	messages := func(id, model, tokenUsage string) []db.Message {
		return []db.Message{
			{SessionID: id, Ordinal: 0, Role: "user", Content: "a", Timestamp: started},
			{SessionID: id, Ordinal: 1, Role: "user", Content: "b", Timestamp: started},
			{
				SessionID: id, Ordinal: 2, Role: "assistant", Content: "c", Timestamp: started,
				Model: model, TokenUsage: []byte(tokenUsage),
			},
		}
	}
	_, err := database.WriteSessionBatch([]db.SessionBatchWrite{
		{
			Session:         session("codex-cloud:plan-a", "codex-cloud"),
			Messages:        messages("codex-cloud:plan-a", "gpt-5-codex", ""),
			PlanUsage:       &db.SessionPlanUsage{HasPlanUsage: true, WeeklyPercent: new(1.5)},
			ReplaceMessages: true, DataVersion: 1,
		},
		{
			Session:  session("codex-cloud:partly", "codex-cloud"),
			Messages: messages("codex-cloud:partly", "gpt-5-codex", ""),
			UsageEvents: []db.UsageEvent{{
				SessionID: "codex-cloud:partly", Source: "codex-cloud-request",
				Model: "gpt-5-codex", InputTokens: 100, OutputTokens: 10,
				OccurredAt: started, DedupKey: "turn-1",
			}},
			PlanUsage:       &db.SessionPlanUsage{HasCoverage: true, UncapturedTurns: 1},
			ReplaceMessages: true, DataVersion: 1,
		},
		{
			Session:         session("claude-a", "claude"),
			Messages:        messages("claude-a", "claude-sonnet-4", `{"input_tokens":40,"output_tokens":7}`),
			ReplaceMessages: true, DataVersion: 1,
		},
	})
	require.NoError(t, err)
	require.NoError(t, database.Close())
}

func TestUsageCLILeftOutNote(t *testing.T) {
	seedPlanUsageCLIArchive(t)
	args := []string{
		"usage", "daily", "--since", "2026-09-10", "--until", "2026-09-10",
		"--timezone", "UTC", "--offline", "--no-sync",
	}

	cmd := newRootCommand()
	cmd.SetArgs(args)
	var err error
	stderr := captureStderr(t, func() {
		captureStdout(t, func() { _, err = cmd.ExecuteC() })
	})
	require.NoError(t, err)
	assert.Contains(t, stderr,
		"note: 1 session billed as plan usage, not priced, is left out of these totals.")
	assert.Contains(t, stderr,
		"note: 1 session partly captured; its token counts are low.")

	cmd = newRootCommand()
	cmd.SetArgs(append(args, "--json"))
	stdout := captureStdout(t, func() { _, err = cmd.ExecuteC() })
	require.NoError(t, err)
	var document map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &document))
	assert.InDelta(t, 1, document["planOnlySessions"], 0)
	assert.InDelta(t, 1, document["partlyCapturedSessions"], 0)
}

func TestPlanUsageNotesPlural(t *testing.T) {
	assert.Equal(t, []string{
		"note: 3 sessions billed as plan usage, not priced, are left out of these totals.",
		"note: 2 sessions partly captured; their token counts are low.",
	}, planUsageNotes(dailyUsageReport{PlanOnlySessions: 3, PartlyCapturedSessions: 2}))
	assert.Empty(t, planUsageNotes(dailyUsageReport{}))
}

func TestSessionUsageHumanShowsPlanUsage(t *testing.T) {
	oldLocal := time.Local                       //nolint:forbidigo // Times print in the user's local zone.
	time.Local = time.FixedZone("EST", -5*60*60) //nolint:forbidigo // Times print in the user's local zone.
	t.Cleanup(func() { time.Local = oldLocal })  //nolint:forbidigo // Times print in the user's local zone.
	for _, tc := range []struct {
		name string
		out  sessionUsageOutput
		want []string
	}{
		{
			name: "figures and missed turns",
			out: sessionUsageOutput{
				SessionUsage: db.SessionUsage{SessionID: "codex-cloud:a", Agent: "codex-cloud"},
				Plan: &db.PlanUsageView{
					DataAsOf: "2026-10-02T09:30:00Z", DataStatus: "available",
					WeeklyPercent: new(2.25), FiveHourPercent: new(12.5), Credits: new("0"),
				},
				Coverage: &db.CaptureCoverageView{LastCaptureAt: "2026-10-01T12:30:00Z", UncapturedTurns: 2},
			},
			want: []string{
				"Plan usage:    2.25% of weekly limit, 12.5% of 5-hour limit, 0 credits, as of 2026-10-02 04:30 EST",
				"Captured:      tokens captured live, as of 2026-10-01 07:30 EST; 2 turns not captured, so some of their tokens may be missing from the totals; " +
					"plan usage counts the ones that ran before 2026-10-02 04:30 EST",
			},
		},
		{
			name: "missed turns before any plan usage",
			out: sessionUsageOutput{
				SessionUsage: db.SessionUsage{SessionID: "codex-cloud:j", Agent: "codex-cloud"},
				Coverage:     &db.CaptureCoverageView{LastCaptureAt: "2026-10-01T12:30:00Z", UncapturedTurns: 2},
			},
			want: []string{
				"Captured:      tokens captured live, as of 2026-10-01 07:30 EST; 2 turns not captured, so some of their tokens may be missing from the totals",
			},
		},
		{
			name: "unrounded figures",
			out: sessionUsageOutput{
				SessionUsage: db.SessionUsage{SessionID: "codex-cloud:e", Agent: "codex-cloud"},
				Plan: &db.PlanUsageView{
					DataAsOf: "2026-10-02T09:30:00Z", DataStatus: "available",
					WeeklyPercent: new(2.6076414506625096), FiveHourPercent: new(0.001963673920498831),
					Credits: new("0E-10"),
				},
			},
			want: []string{
				"Plan usage:    2.61% of weekly limit, <0.01% of 5-hour limit, 0 credits, as of 2026-10-02 04:30 EST",
			},
		},
		{
			name: "never checked",
			out: sessionUsageOutput{
				SessionUsage: db.SessionUsage{SessionID: "codex-cloud:b", Agent: "codex-cloud"},
			},
			want: []string{"Plan usage:    not checked yet; a sync checks it when Codex cloud import is on"},
		},
		{
			name: "empty row before the first check",
			out: sessionUsageOutput{
				SessionUsage: db.SessionUsage{SessionID: "codex-cloud:k", Agent: "codex-cloud"},
				Plan:         &db.PlanUsageView{},
			},
			want: []string{"Plan usage:    not checked yet; a sync checks it when Codex cloud import is on"},
		},
		{
			name: "newer failure after figures",
			out: sessionUsageOutput{
				SessionUsage: db.SessionUsage{SessionID: "codex-cloud:g", Agent: "codex-cloud"},
				Plan: &db.PlanUsageView{
					DataAsOf: "2026-10-02T09:30:00Z", DataStatus: "available", WeeklyPercent: new(2.25),
					OutcomeKind: "could_not_check", OutcomeAt: "2026-10-09T09:30:00Z",
				},
				Coverage: &db.CaptureCoverageView{LastCaptureAt: "2026-10-01T12:30:00Z", UncapturedTurns: 2},
			},
			want: []string{
				"Plan usage:    2.25% of weekly limit, as of 2026-10-02 04:30 EST; " +
					"later, AgentsView could not check plan usage (2026-10-09 04:30 EST), so these figures may be out of date",
				"Captured:      tokens captured live, as of 2026-10-01 07:30 EST; " +
					"2 turns not captured, so some of their tokens may be missing from the totals",
			},
		},
		{
			name: "left out after figures",
			out: sessionUsageOutput{
				SessionUsage: db.SessionUsage{SessionID: "codex-cloud:h", Agent: "codex-cloud"},
				Plan: &db.PlanUsageView{
					DataAsOf: "2026-10-02T09:30:00Z", DataStatus: "available", WeeklyPercent: new(2.25),
					OutcomeKind: "left_out", OutcomeAt: "2026-10-03T09:30:00Z",
				},
			},
			want: []string{
				"Plan usage:    2.25% of weekly limit, as of 2026-10-02 04:30 EST; " +
					"later, OpenAI's usage report left this chat out (2026-10-03 04:30 EST), so these figures may be out of date",
			},
		},
		{
			name: "five-hour figure only",
			out: sessionUsageOutput{
				SessionUsage: db.SessionUsage{SessionID: "codex-cloud:i", Agent: "codex-cloud"},
				Plan: &db.PlanUsageView{
					DataAsOf: "2026-10-02T09:30:00Z", DataStatus: "available", FiveHourPercent: new(3.0),
				},
				Coverage: &db.CaptureCoverageView{LastCaptureAt: "2026-10-01T12:30:00Z", UncapturedTurns: 1},
			},
			want: []string{
				"Captured:      tokens captured live, as of 2026-10-01 07:30 EST; 1 turn not captured, so some of its tokens may be missing from the totals; " +
					"plan usage counts it if it ran before 2026-10-02 04:30 EST",
			},
		},
		{
			name: "partial figures",
			out: sessionUsageOutput{
				SessionUsage: db.SessionUsage{SessionID: "codex-cloud:j", Agent: "codex-cloud"},
				Plan: &db.PlanUsageView{
					DataAsOf: "2026-10-02T09:30:00Z", DataStatus: "partial", WeeklyPercent: new(1.0),
				},
				Coverage: &db.CaptureCoverageView{LastCaptureAt: "2026-10-01T12:30:00Z", UncapturedTurns: 1},
			},
			want: []string{
				"Captured:      tokens captured live, as of 2026-10-01 07:30 EST; " +
					"1 turn not captured, so some of its tokens may be missing from the totals",
			},
		},
		{
			name: "too many sub-agents",
			out: sessionUsageOutput{
				SessionUsage: db.SessionUsage{SessionID: "codex-cloud:k", Agent: "codex-cloud"},
				Plan:         &db.PlanUsageView{OutcomeKind: "too_many_descendants", Final: true},
			},
			want: []string{"Plan usage:    too many sub-agent chats for OpenAI's usage report"},
		},
		{
			name: "descendant",
			out: sessionUsageOutput{
				SessionUsage: db.SessionUsage{SessionID: "codex-cloud:c", Agent: "codex-cloud"},
				Plan:         &db.PlanUsageView{OutcomeKind: "counted_under", CountedUnder: "codex-cloud:root"},
			},
			want: []string{"Plan usage:    counted under session codex-cloud:root"},
		},
		{
			name: "settled without a report",
			out: sessionUsageOutput{
				SessionUsage: db.SessionUsage{SessionID: "codex-cloud:d", Agent: "codex-cloud"},
				Plan:         &db.PlanUsageView{OutcomeKind: "left_out", Final: true},
			},
			want: []string{"Plan usage:    not reported by OpenAI"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b strings.Builder
			require.NoError(t, renderSessionUsageHuman(&b, &tc.out))
			for _, line := range tc.want {
				assert.Contains(t, b.String(), line+"\n")
			}
		})
	}

	var plan strings.Builder
	require.NoError(t, renderSessionUsageHuman(&plan, &sessionUsageOutput{
		SessionID: "codex-cloud:f", Agent: "codex-cloud",
		Plan: &db.PlanUsageView{DataAsOf: "2026-10-02T09:30:00Z", DataStatus: "available", WeeklyPercent: new(1.0)},
	}))
	for _, line := range []string{"Output:        n/a\n", "Peak ctx:      n/a\n", "Cost:          billed as plan usage\n"} {
		assert.Contains(t, plan.String(), line, "a plan-only session never shows zero tokens or cost")
	}

	var unchecked strings.Builder
	require.NoError(t, renderSessionUsageHuman(&unchecked, &sessionUsageOutput{
		SessionID: "codex-cloud:l", Agent: "codex-cloud",
	}))
	for _, line := range []string{"Output:        n/a\n", "Peak ctx:      n/a\n", "Cost:          n/a\n"} {
		assert.Contains(t, unchecked.String(), line, "no plan figures: nothing is billed as plan usage yet")
	}

	var b strings.Builder
	require.NoError(t, renderSessionUsageHuman(&b, &sessionUsageOutput{
		SessionID: "claude-a", Agent: "claude",
	}))
	assert.Contains(t, b.String(), "Output:        0\n", "other agents keep their output")
	assert.NotContains(t, b.String(), "Plan usage", "other agents print no plan usage line")
}

// TestSessionUsageJSONIncludesPlanUsage checks that `session usage`
// reports a plan-only session's plan usage and coverage the same way from
// the local archive, a read store, and a daemon, with no token data.
func TestSessionUsageJSONIncludesPlanUsage(t *testing.T) {
	dataDir := t.TempDir()
	database := dbtest.OpenTestDBAt(t, sessionsDBPath(dataDir))
	started := "2026-09-10T10:00:00Z"
	const id = "codex-cloud:plan-a"
	_, err := database.WriteSessionBatch([]db.SessionBatchWrite{{
		Session: db.Session{
			ID: id, Project: "alpha", Machine: "local", Agent: "codex-cloud",
			StartedAt: &started, CreatedAt: started, DataVersion: 1,
		},
		PlanUsage: &db.SessionPlanUsage{
			HasPlanUsage: true, DataAsOf: started, WeeklyPercent: new(1.5),
			HasCoverage: true, LastCaptureAt: started, UncapturedTurns: 3,
		},
		DataVersion: 1,
	}})
	require.NoError(t, err)
	wantPlan := &db.PlanUsageView{
		DataAsOf: started, WeeklyPercent: new(1.5), Groups: []db.SessionPlanUsageGroup{},
	}
	wantCoverage := &db.CaptureCoverageView{LastCaptureAt: started, UncapturedTurns: 3}
	query := sessionUsageQuery{SessionID: id, OwnOnly: true, NoSync: true}

	srvURL := startPlanUsageTestDaemon(t, dataDir, database)

	for name, read := range map[string]func() (*sessionUsageOutput, int, error){
		"archive": func() (*sessionUsageOutput, int, error) {
			backend := localArchiveQueryBackend{database: database, offline: true, skipFreshData: true}
			return backend.SessionUsage(t.Context(), query)
		},
		"read store": func() (*sessionUsageOutput, int, error) {
			return storeSessionUsageData("test", config.Config{}, database, query)
		},
		"daemon": func() (*sessionUsageOutput, int, error) {
			return httpSessionUsageData(t.Context(), srvURL, "", query)
		},
	} {
		t.Run(name, func(t *testing.T) {
			out, code, err := read()
			require.NoError(t, err)
			require.NotNil(t, out)
			assert.Equal(t, tokenUseExitOK, code, "plan usage figures are data")
			assert.False(t, out.HasTokenData)
			assert.False(t, out.HasCost)
			assert.Equal(t, wantPlan, out.Plan)
			assert.Equal(t, wantCoverage, out.Coverage)
		})
	}
}

// startPlanUsageTestDaemon serves database over HTTP the way a daemon does
// and returns its URL. The server only accepts requests whose Host names
// its own port, so the listener is bound before the server is built.
func startPlanUsageTestDaemon(t *testing.T, dataDir string, database *db.DB) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := httptest.NewUnstartedServer(server.New(config.Config{
		Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port,
		DataDir: dataDir, WriteTimeout: 30 * time.Second,
	}, database, nil).Handler())
	require.NoError(t, srv.Listener.Close())
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestDaemonDailyUsageCountsPlanUsage checks that `usage daily` against a
// daemon reports the same plan-only and partly-captured counts as a local
// read.
func TestDaemonDailyUsageCountsPlanUsage(t *testing.T) {
	seedPlanUsageCLIArchive(t)
	dataDir := os.Getenv("AGENTSVIEW_DATA_DIR")
	database := dbtest.OpenTestDBAt(t, sessionsDBPath(dataDir))
	srvURL := startPlanUsageTestDaemon(t, dataDir, database)

	got, err := fetchHTTPDailyUsage(t.Context(), transport{URL: srvURL}, "", dailyUsageQuery{
		Filter:         db.UsageFilter{From: "2026-09-10", To: "2026-09-10", Timezone: "UTC"},
		NoDefaultRange: true,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, got.PlanOnlySessions)
	assert.Equal(t, 1, got.PartlyCapturedSessions)
	assert.Equal(t, 140, got.Totals.InputTokens, "the claude and captured codex-cloud tokens")
}
