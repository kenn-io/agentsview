package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

// renderStatsHuman renders stats through printStatsHuman and returns the
// captured output, asserting the render itself does not error.
func renderStatsHuman(t *testing.T, stats *db.SessionStats) string {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, printStatsHuman(&buf, stats), "printStatsHuman")
	return buf.String()
}

// assertContainsAll asserts every want substring is present in out.
func assertContainsAll(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		assert.Contains(t, out, w, "missing %q in output", w)
	}
}

// assertContainsNone asserts none of the banned substrings appear in out.
func assertContainsNone(t *testing.T, out string, banned ...string) {
	t.Helper()
	for _, b := range banned {
		assert.NotContains(t, out, b, "unexpected %q in output", b)
	}
}

// setupGoldenStatsDataDir creates a temp data dir, points
// AGENTSVIEW_DATA_DIR at it, pins TZ to UTC for deterministic time
// formatting, seeds the golden fixture DB, and returns the data dir.
// Shared by the stats, usage, and activity command tests that exercise
// the offline/read-only archive query path.
func setupGoldenStatsDataDir(t *testing.T) string {
	t.Helper()
	dataDir := newAgentDataDir(t)
	// TZ is normally pinned by --timezone=UTC, but the environment can
	// still leak into date parsing on some platforms; pin it too.
	t.Setenv("TZ", "UTC")
	buildGoldenFixtureDB(t, sessionsDBPath(dataDir))
	return dataDir
}

// runDefaultStatsJSON runs the `stats --format json` CLI path over the
// golden fixture window and returns the raw JSON output. Callers
// unmarshal into whichever shape they need.
func runDefaultStatsJSON(t *testing.T) string {
	t.Helper()
	out, err := executeCommand(newRootCommand(),
		"stats",
		"--format", "json",
		"--since", "2026-04-01",
		"--until", "2026-04-15",
		"--timezone", "UTC",
	)
	require.NoError(t, err, "stats output:\n%s", out)
	return out
}

// writeCustomModelPricingConfig writes a config.toml under dataDir that sets
// custom per-million-token pricing well above the built-in defaults, so tests
// can assert that custom pricing is applied.
func writeCustomModelPricingConfig(t *testing.T, dataDir string) {
	t.Helper()
	require.NoError(t, os.WriteFile(
		filepath.Join(dataDir, "config.toml"),
		[]byte(`
[custom_model_pricing."claude-sonnet-4-20250514"]
input = 300.0
output = 1500.0
cache_creation = 375.0
cache_read = 30.0

[custom_model_pricing."claude-opus-4-20250514"]
input = 1500.0
output = 7500.0
cache_creation = 1875.0
cache_read = 150.0
`),
		0o600,
	))
}

// TestPrintStatsHuman_Populated exercises the happy path with
// every optional section present. It does not pin exact text — the
// golden-file test in Task 20 owns that — but it guards the sections
// and nil-pointer branches that are hardest to eyeball in the stub.
func TestPrintStatsHuman_Populated(t *testing.T) {
	prsOpened := 12
	prsMerged := 9
	stats := &db.SessionStats{
		SchemaVersion: 1,
		Window: db.StatsWindow{
			Since: "2026-03-21T00:00:00Z",
			Until: "2026-04-18T00:00:00Z",
			Days:  28,
		},
		Filters: db.StatsFilters{
			Agent:            "all",
			ProjectsExcluded: []string{},
			Timezone:         "America/New_York",
		},
		Totals: db.StatsTotals{
			SessionsAll:        11905,
			SessionsHuman:      322,
			SessionsAutomation: 11583,
			MessagesTotal:      109324,
			UserMessagesTotal:  3012,
		},
		Archetypes: db.StatsArchetypes{
			Automation:   11583,
			Quick:        125,
			Standard:     101,
			Deep:         79,
			Marathon:     17,
			Primary:      "automation",
			PrimaryHuman: "quick",
		},
		Distributions: db.StatsDistributions{
			DurationMinutes: db.ScopedDistributionPair{
				ScopeAll:   db.ScopedDistribution{Mean: 14.7},
				ScopeHuman: db.ScopedDistribution{Mean: 22.0},
			},
			UserMessages: db.ScopedDistributionPair{
				ScopeAll:   db.ScopedDistribution{Mean: 11.2},
				ScopeHuman: db.ScopedDistribution{Mean: 7.2},
			},
			PeakContextTokens: db.PeakContextDistribution{
				ScopeAll:  db.ScopedDistribution{Mean: 48000},
				NullCount: 0,
			},
			ToolsPerTurn: db.ScopedDistributionPair{
				ScopeAll: db.ScopedDistribution{Mean: 2.3},
			},
		},
		Velocity: db.StatsVelocity{
			TurnCycleSeconds: db.StatsPercentiles{
				P50: 20, P90: 90, Mean: 45,
			},
			FirstResponseSeconds: db.StatsPercentiles{
				P50: 5, P90: 15, Mean: 8,
			},
			MessagesPerActiveHour: 120.0,
		},
		ToolMix: db.StatsToolMix{
			ByCategory: map[string]int{
				"Bash": 1234, "Edit": 876, "Read": 543,
				"Grep": 321, "Glob": 210, "Write": 50,
			},
			TotalCalls: 3234,
		},
		ModelMix: db.StatsModelMix{
			ByTokens: map[string]int64{
				"claude-opus-4-7":   5600000,
				"claude-sonnet-4-6": 1200000,
			},
		},
		AgentPortfolio: db.StatsAgentPortfolio{
			BySessions: map[string]int{"claude": 11905, "codex": 234},
			ByTokens:   map[string]int64{"claude": 6800000, "codex": 120000},
			ByMessages: map[string]int{"claude": 109000, "codex": 2100},
			Primary:    "claude",
		},
		CacheEconomics: &db.StatsCacheEconomics{
			ClaudeOnly: true,
			CacheHitRatio: db.CacheHitRatioDistribution{
				Overall: 0.78,
			},
			DollarsSavedVsUncached: 88.54,
			DollarsSpent:           42.13,
		},
		Adoption: &db.StatsAdoption{
			ClaudeOnly:          true,
			PlanModeRate:        0.12,
			SubagentsPerSession: 0.3,
			DistinctSkills:      8,
		},
		Temporal: db.StatsTemporal{
			HourlyUTC: []db.TemporalHourlyUTCEntry{
				{TS: "2026-04-01T00:00:00Z", Sessions: 3, UserMessages: 12},
				{TS: "2026-04-01T01:00:00Z", Sessions: 2, UserMessages: 8},
			},
			ReporterTimezone: "America/New_York",
		},
		OutcomeStats: &db.StatsOutcomeStats{
			ReposActive:  3,
			Commits:      84,
			LOCAdded:     5421,
			LOCRemoved:   1823,
			FilesChanged: 127,
			PRsOpened:    &prsOpened,
			PRsMerged:    &prsMerged,
		},
		Outcomes: &db.StatsOutcomes{
			ClaudeOnly:            true,
			Success:               280,
			Failure:               14,
			Unknown:               28,
			GradeDistribution:     map[string]int{"A": 120, "B": 95, "C": 52, "D": 13, "F": 0},
			ToolRetryRate:         0.064,
			CompactionsPerSession: 0.1,
			AvgEditChurn:          1.2,
		},
		GeneratedAt: "2026-04-18T00:00:00Z",
	}

	out := renderStatsHuman(t, stats)
	require.GreaterOrEqual(t, len(out), 200,
		"output suspiciously short (%d bytes):\n%s", len(out), out)

	// Guard every major section header so accidental drops are caught.
	assertContainsAll(t, out,
		"Session window:",
		"Totals",
		"Archetypes",
		"Session shape",
		"Velocity",
		"Tool mix",
		"Model mix",
		"Agent portfolio",
		"Cache economics",
		"Adoption",
		"Temporal",
		"Outcome stats",
		"Outcomes",
	)

	// Thousands separators must be applied to large counts.
	assert.Contains(t, out, "11,905",
		"expected thousands separator for 11,905")
}

// TestPrintStatsHuman_Empty guards the zero-session short
// circuit: no optional sections, just the header + "no sessions".
func TestPrintStatsHuman_Empty(t *testing.T) {
	stats := &db.SessionStats{
		SchemaVersion: 1,
		Window: db.StatsWindow{
			Since: "2026-04-11T00:00:00Z",
			Until: "2026-04-18T00:00:00Z",
			Days:  7,
		},
		Filters: db.StatsFilters{
			Agent:            "all",
			ProjectsExcluded: []string{},
			Timezone:         "UTC",
		},
	}

	out := renderStatsHuman(t, stats)
	assert.Contains(t, out, "no sessions",
		"expected zero-session placeholder in output")
	// No optional section headers should appear.
	assertContainsNone(t, out,
		"Archetypes", "Velocity", "Cache economics", "Outcomes")
}

// TestFmtInt64 covers the thousands-separator helper.
func TestFmtInt64(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{5, "5"},
		{999, "999"},
		{1000, "1,000"},
		{12345, "12,345"},
		{123456, "123,456"},
		{1234567, "1,234,567"},
		{-1234, "-1,234"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, fmtInt64(c.in),
			"fmtInt64(%d)", c.in)
	}
}

func TestStatsCommand_OutcomeFlagsRegistered(t *testing.T) {
	cmd := newStatsCommand()
	for _, name := range []string{
		"include-git-outcomes",
		"include-github-outcomes",
	} {
		assert.NotNil(t, cmd.Flags().Lookup(name),
			"missing --%s flag", name)
	}
}

// updateGolden toggles regeneration of stats_golden.json.
// Pass `go test ./cmd/agentsview -run TestStatsGolden -update`
// after intentionally changing the fixture or the stats pipeline.
var updateGolden = flag.Bool(
	"update", false,
	"rewrite golden files under testdata/ instead of comparing",
)

// TestStatsGolden is the end-to-end guard for the v1 JSON schema: it
// seeds a deterministic fixture DB, runs the full `stats --format
// json` CLI path through the root command, and compares the parsed
// output to testdata/stats_golden.json.
//
// Determinism comes from four levers:
//  1. Absolute --since/--until dates so windowBounds never reads
//     time.Now for the window boundary.
//  2. --timezone=UTC so Temporal.ReporterTimezone is a fixed string
//     independent of the host's TZ env.
//  3. Session and message timestamps are absolute RFC3339 strings
//     inside that window, so temporal.hourly_utc keys are stable.
//  4. GeneratedAt is stripped from both sides before comparison
//     because GetSessionStats stamps it from time.Now().
//
// Regenerate after an intentional change with:
//
//	go test ./cmd/agentsview -run TestStatsGolden -update
func TestStatsGolden(t *testing.T) {
	setupGoldenStatsDataDir(t)

	out := runDefaultStatsJSON(t)

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &got),
		"unmarshal stats output, output:\n%s", out)
	delete(got, "generated_at")

	goldenPath := filepath.Join(
		"testdata", "stats_golden.json",
	)
	if *updateGolden {
		buf, err := json.MarshalIndent(got, "", "  ")
		require.NoError(t, err, "marshal golden")
		buf = append(buf, '\n')
		require.NoError(t, os.MkdirAll(
			filepath.Dir(goldenPath), 0o755,
		), "mkdir testdata")
		require.NoError(t, os.WriteFile(
			goldenPath, buf, 0o644,
		), "write golden")
		t.Logf("rewrote %s (%d bytes)", goldenPath, len(buf))
		return
	}

	raw, err := os.ReadFile(goldenPath)
	require.NoError(t, err, "read golden (run with -update to generate)")
	var want map[string]any
	require.NoError(t, json.Unmarshal(raw, &want), "unmarshal golden")
	delete(want, "generated_at")

	if !assert.Equal(t, want, got) {
		gotBuf, _ := json.MarshalIndent(got, "", "  ")
		wantBuf, _ := json.MarshalIndent(want, "", "  ")
		t.Fatalf(
			"stats JSON mismatch — regenerate with "+
				"`go test ./cmd/agentsview -run "+
				"TestStatsGolden -update` if intentional.\n"+
				"--- got ---\n%s\n--- want ---\n%s",
			gotBuf, wantBuf,
		)
	}
}

func TestStatsReadOnlyOpenAppliesCustomPricing(t *testing.T) {
	dataDir := setupGoldenStatsDataDir(t)
	writeCustomModelPricingConfig(t, dataDir)

	out := runDefaultStatsJSON(t)
	var got db.SessionStats
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.NotNil(t, got.CacheEconomics)
	assert.Greater(t, got.CacheEconomics.DollarsSpent, 600.0,
		"custom pricing should be applied to the read-only stats DB handle")
}

// buildGoldenFixtureDB seeds a deterministic session set into a fresh
// SQLite database at dbPath. The fixture exercises the full v1 schema:
//
//   - Three agents (claude, codex, cursor) so agent_portfolio has variety.
//   - User-message counts spanning every archetype bucket so archetypes,
//     distributions.user_messages, and primary/primary_human all resolve.
//   - Two assistant models (sonnet, opus) so model_mix has >1 row.
//   - A handful of peak_context_tokens values so peak_context has a
//     non-zero mean and a non-zero null_count.
//   - Tool calls across three categories plus the three adoption
//     tool_names (ExitPlanMode, Task, Skill) so tool_mix and adoption
//     are both populated.
//   - A couple of sessions with outcome + health_grade set so
//     outcomes.success / failure / grade_distribution are non-trivial.
//
// No cwd is set on any session so outcome_stats stays nil (git
// integration is out of scope for this test). No GH_TOKEN env is
// propagated, so PRsOpened/PRsMerged stay nil regardless.
func buildGoldenFixtureDB(t *testing.T, dbPath string) {
	t.Helper()
	d, err := db.Open(dbPath)
	require.NoError(t, err, "open fixture db")
	t.Cleanup(func() { d.Close() })

	require.NoError(t, d.UpsertModelPricing([]db.ModelPricing{
		{
			ModelPattern:         "claude-sonnet-4-20250514",
			InputPerMTok:         3.0,
			OutputPerMTok:        15.0,
			CacheCreationPerMTok: 3.75,
			CacheReadPerMTok:     0.30,
		},
		{
			ModelPattern:         "claude-opus-4-20250514",
			InputPerMTok:         15.0,
			OutputPerMTok:        75.0,
			CacheCreationPerMTok: 18.75,
			CacheReadPerMTok:     1.50,
		},
	}), "seed pricing")

	for _, spec := range goldenFixtureSessions {
		seedGoldenSession(t, d, spec)
	}
}

// goldenSessionSpec fully describes a fixture session. The slice
// goldenFixtureSessions is the single source of truth for the golden
// file; edit carefully, then regenerate with -update.
type goldenSessionSpec struct {
	id           string
	project      string
	agent        string
	model        string // empty -> no assistant model/token_usage seeded
	startedAt    string // RFC3339 UTC
	durationMin  int    // minutes; adds to startedAt to compute ended_at
	userMsgs     int    // user-message rows seeded under this session
	peakContext  int    // peak_context_tokens (0 -> not set)
	outcome      string // sessions.outcome column
	healthGrade  string // sessions.health_grade column
	toolCategory string // tool_calls.category (empty -> no tool calls)
	toolName     string // tool_calls.tool_name (empty -> "Read")
	toolCount    int    // number of tool_calls rows to insert
	skillName    string // populated for Skill tool_calls
	retryCount   int    // sessions.tool_retry_count
	editChurn    int    // sessions.edit_churn_count
	compactions  int    // sessions.compaction_count
}

// goldenFixtureSessions is deliberately small (11 rows) to keep the
// golden JSON under ~5 KB while still covering every section. Session
// IDs are deterministic and grouped by agent for readability.
var goldenFixtureSessions = []goldenSessionSpec{
	{
		id: "c-auto-01", project: "proj-alpha", agent: "claude",
		model:     "claude-sonnet-4-20250514",
		startedAt: "2026-04-05T10:00:00Z", durationMin: 5,
		userMsgs: 1, outcome: "completed", healthGrade: "A",
	},
	{
		id: "c-auto-02", project: "proj-alpha", agent: "claude",
		model:     "claude-sonnet-4-20250514",
		startedAt: "2026-04-05T11:00:00Z", durationMin: 4,
		userMsgs: 1, outcome: "completed", healthGrade: "A",
	},
	{
		id: "c-quick-01", project: "proj-alpha", agent: "claude",
		model:     "claude-sonnet-4-20250514",
		startedAt: "2026-04-06T10:00:00Z", durationMin: 15,
		userMsgs: 3, peakContext: 20000,
		outcome: "completed", healthGrade: "A",
		toolCategory: "file", toolName: "Read", toolCount: 4,
	},
	{
		id: "c-quick-02", project: "proj-beta", agent: "claude",
		model:     "claude-opus-4-20250514",
		startedAt: "2026-04-07T10:00:00Z", durationMin: 20,
		userMsgs: 3, outcome: "completed", healthGrade: "B",
		toolCategory: "shell", toolName: "Bash", toolCount: 3,
	},
	{
		id: "c-std-01", project: "proj-beta", agent: "claude",
		model:     "claude-sonnet-4-20250514",
		startedAt: "2026-04-08T10:00:00Z", durationMin: 45,
		userMsgs: 10, peakContext: 55000,
		outcome: "completed", healthGrade: "B",
		toolCategory: "file", toolName: "Edit", toolCount: 6,
		retryCount: 1, editChurn: 2,
	},
	{
		id: "c-deep-01", project: "proj-beta", agent: "claude",
		model:     "claude-opus-4-20250514",
		startedAt: "2026-04-09T10:00:00Z", durationMin: 120,
		userMsgs: 30, peakContext: 95000,
		outcome: "completed", healthGrade: "A",
		toolCategory: "search", toolName: "Grep", toolCount: 8,
		retryCount: 2, compactions: 1,
	},
	{
		id: "c-deep-02", project: "proj-gamma", agent: "claude",
		model:     "claude-opus-4-20250514",
		startedAt: "2026-04-10T10:00:00Z", durationMin: 150,
		userMsgs: 25, peakContext: 75000,
		outcome: "errored", healthGrade: "C",
		toolCategory: "other", toolName: "ExitPlanMode", toolCount: 1,
	},
	{
		id: "c-marathon-01", project: "proj-gamma", agent: "claude",
		model:     "claude-sonnet-4-20250514",
		startedAt: "2026-04-11T10:00:00Z", durationMin: 240,
		userMsgs: 80, peakContext: 130000,
		outcome: "completed", healthGrade: "A",
		toolCategory: "other", toolName: "Task", toolCount: 2,
		retryCount: 3, compactions: 2, editChurn: 5,
	},
	{
		id: "cx-std-01", project: "proj-alpha", agent: "codex",
		startedAt: "2026-04-08T14:00:00Z", durationMin: 30,
		userMsgs: 10,
	},
	{
		id: "cu-quick-01", project: "proj-beta", agent: "cursor",
		startedAt: "2026-04-09T14:00:00Z", durationMin: 10,
		userMsgs: 3,
	},
	{
		id: "c-skill-01", project: "proj-alpha", agent: "claude",
		model:     "claude-sonnet-4-20250514",
		startedAt: "2026-04-12T10:00:00Z", durationMin: 25,
		userMsgs: 8, outcome: "completed", healthGrade: "B",
		toolCategory: "other", toolName: "Skill", toolCount: 2,
		skillName: "summarize",
	},
}

// goldenOutputTokens returns the assistant output_tokens for the i-th
// (zero-based) user/assistant turn in a fixture session. seedGoldenSession
// and buildGoldenMessages both call this so the precomputed session total
// and the per-message token_usage never drift apart.
func goldenOutputTokens(i int) int {
	return 200 + 30*i
}

// seedGoldenSession persists one goldenSessionSpec: the session row,
// N user messages + N assistant messages at 1-minute spacing, optional
// token_usage on assistant messages, and optional tool_calls rows.
// All timestamps are derived from spec.startedAt so the fixture is
// trivially regenerable.
func seedGoldenSession(
	t *testing.T, d *db.DB, spec goldenSessionSpec,
) {
	t.Helper()

	startedAt := spec.startedAt
	endedAt := addMinutes(startedAt, spec.durationMin)
	// Pre-compute total_output_tokens by summing the per-assistant
	// output_tokens the message builder will stamp. Kept in sync with
	// buildGoldenMessages via goldenOutputTokens so agent_portfolio.by_tokens
	// has meaningful non-zero values.
	totalOutput := 0
	if spec.model != "" {
		for i := 0; i < spec.userMsgs; i++ {
			totalOutput += goldenOutputTokens(i)
		}
	}
	session := db.Session{
		ID:               spec.id,
		Project:          spec.project,
		Machine:          "golden-host",
		Agent:            spec.agent,
		StartedAt:        &startedAt,
		EndedAt:          &endedAt,
		UserMessageCount: spec.userMsgs,
		// UserMsgs x 2 gives one assistant per user; ensures
		// message_count > 0 even for 1-user "automation" rows.
		MessageCount:         spec.userMsgs * 2,
		PeakContextTokens:    spec.peakContext,
		HasPeakContextTokens: spec.peakContext > 0,
		TotalOutputTokens:    totalOutput,
		HasTotalOutputTokens: totalOutput > 0,
	}
	require.NoError(t, d.UpsertSession(session), "upsert %s", spec.id)

	if spec.outcome != "" || spec.healthGrade != "" ||
		spec.retryCount > 0 || spec.editChurn > 0 ||
		spec.compactions > 0 {
		var grade *string
		if spec.healthGrade != "" {
			g := spec.healthGrade
			grade = &g
		}
		require.NoError(t, d.UpdateSessionSignals(spec.id, db.SessionSignalUpdate{
			Outcome:         spec.outcome,
			HealthGrade:     grade,
			ToolRetryCount:  spec.retryCount,
			EditChurnCount:  spec.editChurn,
			CompactionCount: spec.compactions,
		}), "update signals %s", spec.id)
	}

	msgs := buildGoldenMessages(spec)
	if len(msgs) > 0 {
		require.NoError(t, d.InsertMessages(msgs),
			"insert messages %s", spec.id)
	}
}

// buildGoldenMessages returns interleaved user/assistant messages.
// Assistant messages carry model + token_usage when spec.model is
// non-empty so cache_economics + model_mix pick them up. When the
// spec requests tool calls, they attach to the first N assistant
// messages (round-robin when toolCount exceeds userMsgs) so every
// tool_call resolves to a real message_id via InsertMessages.
func buildGoldenMessages(spec goldenSessionSpec) []db.Message {
	out := make([]db.Message, 0, spec.userMsgs*2)
	toolsBuilt := 0
	toolName := spec.toolName
	if toolName == "" && spec.toolCount > 0 {
		toolName = "Read"
	}
	for i := 0; i < spec.userMsgs; i++ {
		ts := addMinutes(spec.startedAt, i)
		out = append(out, db.Message{
			SessionID:     spec.id,
			Ordinal:       i * 2,
			Role:          "user",
			Content:       "u",
			ContentLength: 1,
			Timestamp:     ts,
		})
		// Offset the assistant reply by a fixed 10 seconds so
		// velocity.first_response_seconds and turn_cycle_seconds
		// have non-zero distributions in the golden output.
		asst := db.Message{
			SessionID:     spec.id,
			Ordinal:       i*2 + 1,
			Role:          "assistant",
			Content:       "a",
			ContentLength: 1,
			Timestamp:     addSeconds(ts, 10),
		}
		if spec.model != "" {
			asst.Model = spec.model
			// Stable, small token counts so cache_economics numbers
			// are deterministic. Vary per-message by ordinal so
			// different sessions accumulate differently.
			input := 400 + 50*i
			output := goldenOutputTokens(i)
			cacheCr := 100 + 20*i
			cacheRd := 600 + 100*i
			asst.TokenUsage = fmt.Appendf(nil,
				`{"input_tokens":%d,"output_tokens":%d,`+
					`"cache_creation_input_tokens":%d,`+
					`"cache_read_input_tokens":%d}`,
				input, output, cacheCr, cacheRd,
			)
			asst.OutputTokens = output
			asst.HasOutputTokens = true
		}
		out = append(out, asst)
	}
	// Distribute tool_calls round-robin across the assistant
	// messages just appended so each call has a valid host, even
	// when toolCount > userMsgs.
	for toolsBuilt < spec.toolCount {
		asstIdx := 1 + 2*(toolsBuilt%spec.userMsgs) // ordinal of nth asst
		for j := range out {
			if out[j].Ordinal != asstIdx {
				continue
			}
			out[j].HasToolUse = true
			out[j].ToolCalls = append(out[j].ToolCalls, db.ToolCall{
				ToolName:  toolName,
				Category:  spec.toolCategory,
				SkillName: spec.skillName,
				ToolUseID: fmt.Sprintf("%s-tc-%d",
					spec.id, toolsBuilt),
			})
			break
		}
		toolsBuilt++
	}
	return out
}

// addMinutes parses an RFC3339 timestamp and returns it + n minutes,
// formatted back to RFC3339 UTC. All fixture timestamps are UTC with
// a "Z" suffix, so time.Parse is exact round-trip.
func addMinutes(ts string, n int) string {
	return addDuration(ts, time.Duration(n)*time.Minute)
}

// addSeconds is the second-granularity sibling of addMinutes, used by
// the message builder to offset assistant replies from their user
// prompts so velocity percentiles exercise non-zero values.
func addSeconds(ts string, n int) string {
	return addDuration(ts, time.Duration(n)*time.Second)
}

func addDuration(ts string, d time.Duration) string {
	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		// Only called with literals we control, so this is a
		// programmer error; surface it in the test diff instead of
		// a hidden panic.
		return "INVALID:" + ts
	}
	return parsed.Add(d).UTC().Format(time.RFC3339)
}
