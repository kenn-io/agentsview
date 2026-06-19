package db

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// itoa is a thin alias for strconv.Itoa kept short so seedModelMessages'
// inline JSON construction stays readable.
func itoa(n int) string { return strconv.Itoa(n) }

// sessionFixture is a compact description of a seeded session used by
// session-stats tests. Fields mirror the subset of sessions-table
// columns the stats pipeline actually reads; extend in future tasks.
type sessionFixture struct {
	id           string
	project      string
	agent        string
	userMsgs     int
	messageCount int
	startedAt    string // RFC3339; required to place row in window
	endedAt      string // RFC3339 or ""
	// durationMin, when > 0 and endedAt is empty, derives endedAt as
	// startedAt + durationMin minutes. Ignored if endedAt is set.
	durationMin        float64
	peakContext        int
	hasPeakContext     bool
	totalOutputTok     int
	hasTotalOutputToks bool
	isAutomated        bool
	relationshipType   string
	// totalToolCalls seeds that many rows in the tool_calls table for
	// this session, each attached to a synthetic assistant message.
	totalToolCalls int
	// assistantTurns seeds that many assistant-role messages for this
	// session. Set alongside totalToolCalls so tests can control the
	// tools_per_turn denominator precisely.
	assistantTurns int
	// cwd is the working directory recorded on the session. Consumed by
	// outcome_stats tests that exercise git-repo discovery.
	cwd string
}

// hoursAgo returns an RFC3339 timestamp N hours before now in UTC.
// Used to place fixture rows safely inside the default 28-day window.
func hoursAgo(n int) string {
	return time.Now().UTC().Add(-time.Duration(n) * time.Hour).
		Format(time.RFC3339)
}

func Test_insertSessionFixture_isAutomated_patch(t *testing.T) {
	d := testDB(t)
	insertSessionFixture(t, d, sessionFixture{
		id: "auto-1", userMsgs: 5, startedAt: hoursAgo(1),
		isAutomated: true,
	})
	insertSessionFixture(t, d, sessionFixture{
		id: "human-1", userMsgs: 1, startedAt: hoursAgo(1),
		isAutomated: false,
	})

	var autoFlag, humanFlag int
	require.NoError(t, d.getReader().QueryRow(
		"SELECT is_automated FROM sessions WHERE id = ?", "auto-1",
	).Scan(&autoFlag), "read auto-1")
	require.NoError(t, d.getReader().QueryRow(
		"SELECT is_automated FROM sessions WHERE id = ?", "human-1",
	).Scan(&humanFlag), "read human-1")
	require.Equal(t, 1, autoFlag, "auto-1 is_automated")
	require.Equal(t, 0, humanFlag, "human-1 is_automated")
}

func Test_loadSessionsInWindow_isAutomated(t *testing.T) {
	d := testDB(t)
	insertSessionFixture(t, d, sessionFixture{
		id: "auto", userMsgs: 5, startedAt: hoursAgo(1),
		isAutomated: true,
	})
	insertSessionFixture(t, d, sessionFixture{
		id: "human", userMsgs: 1, startedAt: hoursAgo(1),
		isAutomated: false,
	})

	ctx := t.Context()
	from := time.Now().Add(-24 * time.Hour)
	to := time.Now().Add(1 * time.Hour)
	rows, err := d.loadSessionsInWindow(ctx, StatsFilter{}, from, to)
	require.NoError(t, err, "loadSessionsInWindow")
	byID := map[string]bool{}
	for _, r := range rows {
		byID[r.id] = r.isAutomated
	}
	require.Equal(t, true, byID["auto"], "auto.isAutomated")
	require.Equal(t, false, byID["human"], "human.isAutomated")
}

// insertSessionFixture inserts a sessionFixture via the standard
// UpsertSession path so triggers and defaults stay authoritative.
// Defaults mirror insertSession in db_test.go (machine=local,
// agent=claude) but let tests override agent/project.
func insertSessionFixture(t *testing.T, d *DB, f sessionFixture) {
	t.Helper()
	project := f.project
	if project == "" {
		project = "proj"
	}
	agent := f.agent
	if agent == "" {
		agent = defaultAgent
	}
	// message_count must be > 0 so analytics WHERE clauses don't skip
	// the row; default to userMsgs*2 when not set explicitly.
	mc := f.messageCount
	if mc == 0 {
		mc = f.userMsgs * 2
		if mc == 0 {
			mc = 1
		}
	}
	endedAt := f.endedAt
	if endedAt == "" && f.durationMin > 0 && f.startedAt != "" {
		start, err := time.Parse(time.RFC3339, f.startedAt)
		require.NoError(t, err,
			"insertSessionFixture %s: parsing startedAt %q",
			f.id, f.startedAt)
		dur := time.Duration(f.durationMin * float64(time.Minute))
		endedAt = start.Add(dur).UTC().Format(time.RFC3339Nano)
	}
	insertSession(t, d, f.id, project, func(s *Session) {
		s.Agent = agent
		s.UserMessageCount = f.userMsgs
		s.MessageCount = mc
		if f.startedAt != "" {
			s.StartedAt = new(f.startedAt)
		}
		if endedAt != "" {
			s.EndedAt = new(endedAt)
		}
		s.PeakContextTokens = f.peakContext
		s.HasPeakContextTokens = f.hasPeakContext
		s.TotalOutputTokens = f.totalOutputTok
		// Flip has_total_output_tokens whenever the fixture supplies a
		// non-zero token count; tests that explicitly want to leave the
		// flag false can override via hasTotalOutputToks.
		if f.hasTotalOutputToks || f.totalOutputTok > 0 {
			s.HasTotalOutputTokens = true
		}
		s.IsAutomated = f.isAutomated
		s.RelationshipType = f.relationshipType
		s.Cwd = f.cwd
	})
	seedAssistantActivity(t, d, f.id, f.assistantTurns, f.totalToolCalls)

	// UpsertSession recomputes is_automated from FirstMessage, so a
	// fixture's f.isAutomated alone would be silently clobbered when
	// no first message is set. Patch the column after the upsert so
	// f.isAutomated is the authoritative value the stats pipeline
	// reads. Test-only path; production ingest always flows through
	// UpsertSession's classifier.
	var want int
	if f.isAutomated {
		want = 1
	}
	_, err := d.getWriter().Exec(
		"UPDATE sessions SET is_automated = ? WHERE id = ?",
		want, f.id,
	)
	require.NoError(t, err,
		"insertSessionFixture %s: patch is_automated", f.id)
}

// seedAssistantActivity inserts `turns` assistant messages and
// spreads `toolCalls` rows across them (or across a single synthetic
// message when turns==0 but toolCalls>0). Purpose: let stats tests
// control both the assistant-turn count (denominator of
// tools_per_turn) and the total tool-call count (numerator) without
// reaching into the full parser pipeline.
func seedAssistantActivity(
	t *testing.T, d *DB, sessionID string, turns, toolCalls int,
) {
	t.Helper()
	if turns == 0 && toolCalls == 0 {
		return
	}
	n := turns
	if n == 0 {
		n = 1 // need at least one host message for tool_calls FK
	}
	msgs := make([]Message, 0, n)
	for i := range n {
		msgs = append(msgs, asstMsg(sessionID, i+1, "reply"))
	}
	require.NoError(t, d.InsertMessages(msgs),
		"seedAssistantActivity %s: InsertMessages", sessionID)
	if toolCalls == 0 {
		return
	}
	// Distribute tool_calls round-robin across inserted messages so
	// they all attach to a real message row. Rely on the router-like
	// INSERT ... SELECT ordinal to find the message_id.
	for i := range toolCalls {
		ord := (i % n) + 1
		_, err := d.getWriter().Exec(`
			INSERT INTO tool_calls
				(message_id, session_id, tool_name, category)
			SELECT id, session_id, 'Read', 'file'
			FROM messages
			WHERE session_id = ? AND ordinal = ?`,
			sessionID, ord,
		)
		require.NoError(t, err,
			"seedAssistantActivity %s: tool_call", sessionID)
	}
}

// seedToolCallsByCategory inserts one assistant message per entry in
// categories and a matching tool_calls row. Used by tool_mix tests
// that need precise control over category values (unlike
// seedAssistantActivity, which always writes category='file').
func seedToolCallsByCategory(
	t *testing.T, d *DB, sessionID string, categories []string,
) {
	t.Helper()
	if len(categories) == 0 {
		return
	}
	msgs := make([]Message, 0, len(categories))
	for i, cat := range categories {
		msgs = append(msgs, asstMsg(sessionID, i+1, "reply-"+cat))
	}
	require.NoError(t, d.InsertMessages(msgs),
		"seedToolCallsByCategory %s: InsertMessages", sessionID)
	for i, cat := range categories {
		ord := i + 1
		_, err := d.getWriter().Exec(`
			INSERT INTO tool_calls
				(message_id, session_id, tool_name, category)
			SELECT id, session_id, ?, ?
			FROM messages
			WHERE session_id = ? AND ordinal = ?`,
			cat, cat, sessionID, ord,
		)
		require.NoError(t, err,
			"seedToolCallsByCategory %s: %q", sessionID, cat)
	}
}

// seedModelMessages inserts one assistant message per (model, tokens)
// pair so the model_mix query sees a stable per-message row with known
// output_tokens. Ordinals are taken relative to startOrd so callers can
// layer multiple seed passes onto the same session without colliding.
func seedModelMessages(
	t *testing.T, d *DB, sessionID string, startOrd int,
	pairs []struct {
		model  string
		tokens int
	},
) {
	t.Helper()
	if len(pairs) == 0 {
		return
	}
	msgs := make([]Message, 0, len(pairs))
	for i, p := range pairs {
		m := asstMsg(sessionID, startOrd+i, "reply")
		m.Model = p.model
		m.OutputTokens = p.tokens
		m.HasOutputTokens = true
		// model_mix's eligibility filter (mirrors
		// usageMessageEligibility) requires token_usage != ''. Stamp a
		// minimal JSON blob so these fixtures qualify; the contents
		// don't matter to model_mix, which sums output_tokens.
		m.TokenUsage = json.RawMessage(
			`{"output_tokens":` + itoa(p.tokens) + `}`,
		)
		msgs = append(msgs, m)
	}
	require.NoError(t, d.InsertMessages(msgs),
		"seedModelMessages %s: InsertMessages", sessionID)
}

func TestSessionShapeLabel(t *testing.T) {
	// Automation is decided upstream via sessions.is_automated; this
	// helper classifies only non-automated sessions, so the lower band
	// starts at 0 and includes userMsgs=1.
	cases := []struct {
		userMsgs int
		want     string
	}{
		{0, "quick"},
		{1, "quick"},
		{2, "quick"},
		{5, "quick"},
		{6, "standard"},
		{15, "standard"},
		{16, "deep"},
		{50, "deep"},
		{51, "marathon"},
		{1000, "marathon"},
	}
	for _, c := range cases {
		got := sessionShapeLabel(c.userMsgs)
		assert.Equal(t, c.want, got,
			"sessionShapeLabel(%d)", c.userMsgs)
	}
}

func TestPickMaxLabel_TiesBreakByPriority(t *testing.T) {
	// automation (2) vs deep (2) — priority says automation wins.
	counts := map[string]int{"automation": 2, "deep": 2, "quick": 1}
	priority := []string{
		"automation", "marathon", "deep", "standard", "quick",
	}
	assert.Equal(t, "automation", pickMaxLabel(counts, priority),
		"tie break")
	// PrimaryHuman excludes automation; marathon should win a 1/1/1
	// tie over deep/standard/quick.
	humanCounts := map[string]int{
		"quick": 1, "standard": 1, "deep": 1, "marathon": 1,
	}
	humanPriority := []string{"marathon", "deep", "standard", "quick"}
	assert.Equal(t, "marathon",
		pickMaxLabel(humanCounts, humanPriority),
		"human tie break")
	// Strictly greater wins regardless of priority.
	c2 := map[string]int{"quick": 5, "deep": 2}
	assert.Equal(t, "quick", pickMaxLabel(c2, priority),
		"strict max")
}

func TestGetSessionStats_TotalsAndArchetypes(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// 5 sessions: 2 automation (is_automated=true),
	//             2 deep (userMsgs 20, 40),
	//             1 marathon (userMsgs 100).
	// Automation is now authoritative via sessions.is_automated; the
	// two short rows carry the flag so they flow through the automation
	// branch regardless of user_message_count.
	fixtures := []sessionFixture{
		{id: "s1", userMsgs: 0, startedAt: hoursAgo(5), isAutomated: true},
		{id: "s2", userMsgs: 1, startedAt: hoursAgo(5), isAutomated: true},
		{id: "s3", userMsgs: 20, startedAt: hoursAgo(5)},
		{id: "s4", userMsgs: 40, startedAt: hoursAgo(5)},
		{id: "s5", userMsgs: 100, startedAt: hoursAgo(5)},
	}
	for _, f := range fixtures {
		insertSessionFixture(t, d, f)
	}

	stats, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d"})
	require.NoError(t, err, "GetSessionStats")

	assert.Equal(t, 1, stats.SchemaVersion, "schema_version: got")
	assert.Equal(t, 5, stats.Totals.SessionsAll, "sessions_all")
	assert.Equal(t, 2, stats.Totals.SessionsAutomation,
		"sessions_automation")
	assert.Equal(t, 3, stats.Totals.SessionsHuman, "sessions_human")
	// Invariant: human + automation must equal all.
	assert.Equal(t, stats.Totals.SessionsAll,
		stats.Totals.SessionsHuman+stats.Totals.SessionsAutomation,
		"invariant: human (%d) + automation (%d) != all (%d)",
		stats.Totals.SessionsHuman,
		stats.Totals.SessionsAutomation,
		stats.Totals.SessionsAll)
	assert.Equal(t, 161, stats.Totals.UserMessagesTotal,
		"user_messages_total")

	assert.Equal(t, 2, stats.Archetypes.Automation,
		"archetypes.automation")
	assert.Equal(t, 0, stats.Archetypes.Quick, "archetypes.quick")
	assert.Equal(t, 0, stats.Archetypes.Standard,
		"archetypes.standard")
	assert.Equal(t, 2, stats.Archetypes.Deep, "archetypes.deep")
	assert.Equal(t, 1, stats.Archetypes.Marathon,
		"archetypes.marathon")
	// 2 automation, 2 deep — tie broken by priority: automation first.
	assert.Equal(t, "automation", stats.Archetypes.Primary,
		"archetypes.primary")
	// Human subset: 2 deep, 1 marathon. Deep wins.
	assert.Equal(t, "deep", stats.Archetypes.PrimaryHuman,
		"archetypes.primary_human")

	// Window bookkeeping: Since = now-28d, Until = now, days = 28.
	assert.Equal(t, 28, stats.Window.Days, "window.days: got")
	assert.NotEmpty(t, stats.Window.Since,
		"window.since (until=%q)", stats.Window.Until)
	assert.NotEmpty(t, stats.Window.Until,
		"window.until (since=%q)", stats.Window.Since)
	_, errSince := time.Parse(time.RFC3339, stats.Window.Since)
	assert.NoError(t, errSince, "window.since not RFC3339")
	_, errUntil := time.Parse(time.RFC3339, stats.Window.Until)
	assert.NoError(t, errUntil, "window.until not RFC3339")

	// Filters echo the inputs and default Agent to "all".
	assert.Equal(t, "all", stats.Filters.Agent, "filters.agent")
	assert.Equal(t, "UTC", stats.Filters.Timezone,
		"filters.timezone")
	assert.NotNil(t, stats.Filters.ProjectsExcluded,
		"filters.projects_excluded must be non-nil slice")

	assert.NotEmpty(t, stats.GeneratedAt, "generated_at")
}

func Test_computeTotalsAndArchetypes_flagAuthority(t *testing.T) {
	d := testDB(t)
	// Short non-automated session — must count as human, bucket as "quick".
	insertSessionFixture(t, d, sessionFixture{
		id: "short-human", userMsgs: 1, startedAt: hoursAgo(1),
		isAutomated: false,
	})
	// Automated session — bucket as "automation" regardless of its
	// userMsgs shape. userMsgs=7 is chosen so that under the old
	// heuristic this row would have landed in "standard", making the
	// Archetypes.Quick == 1 assertion a real regression guard: old
	// code produces Quick=0, new code produces Quick=1 from the
	// short-human fixture.
	insertSessionFixture(t, d, sessionFixture{
		id: "auto", userMsgs: 7, startedAt: hoursAgo(1),
		isAutomated: true,
	})

	got, err := d.GetSessionStats(t.Context(), StatsFilter{Since: "1d"})
	require.NoError(t, err, "GetSessionStats")
	require.Equal(t, 1, got.Totals.SessionsHuman, "SessionsHuman")
	require.Equal(t, 1, got.Totals.SessionsAutomation, "SessionsAutomation")
	require.Equal(t, 1, got.Archetypes.Quick, "Archetypes.Quick")
	require.Equal(t, 1, got.Archetypes.Automation, "Archetypes.Automation")
}

func TestGetSessionStats_FilterByAgent(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSessionFixture(t, d, sessionFixture{
		id: "c1", agent: "claude", userMsgs: 10,
		startedAt: hoursAgo(3),
	})
	insertSessionFixture(t, d, sessionFixture{
		id: "x1", agent: "codex", userMsgs: 10,
		startedAt: hoursAgo(3),
	})

	all, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d"})
	require.NoError(t, err, "GetSessionStats all")
	assert.Equal(t, 2, all.Totals.SessionsAll, "all agents")

	onlyClaude, err := d.GetSessionStats(
		ctx, StatsFilter{Since: "28d", Agent: "claude"},
	)
	require.NoError(t, err, "GetSessionStats claude")
	assert.Equal(t, 1, onlyClaude.Totals.SessionsAll, "agent=claude")
	assert.Equal(t, "claude", onlyClaude.Filters.Agent,
		"agent filter echoed")

	// Comma-separated agents with surrounding whitespace must match
	// every listed agent; the CSV values are trimmed before filtering.
	multi, err := d.GetSessionStats(
		ctx, StatsFilter{Since: "28d", Agent: "claude, codex"},
	)
	require.NoError(t, err, "GetSessionStats multi-agent")
	assert.Equal(t, 2, multi.Totals.SessionsAll,
		"comma-separated agents with whitespace match both")
}

func TestGetSessionStats_FilterByProject(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	for i, p := range []string{"alpha", "alpha", "beta", "gamma"} {
		insertSessionFixture(t, d, sessionFixture{
			id:        fmt.Sprintf("p%d", i),
			project:   p,
			userMsgs:  10,
			startedAt: hoursAgo(2),
		})
	}

	includeAlpha, err := d.GetSessionStats(ctx, StatsFilter{
		Since:           "28d",
		IncludeProjects: []string{"alpha"},
	})
	require.NoError(t, err, "include alpha")
	assert.Equal(t, 2, includeAlpha.Totals.SessionsAll,
		"include=alpha")

	excludeAlpha, err := d.GetSessionStats(ctx, StatsFilter{
		Since:           "28d",
		ExcludeProjects: []string{"alpha"},
	})
	require.NoError(t, err, "exclude alpha")
	assert.Equal(t, 2, excludeAlpha.Totals.SessionsAll,
		"exclude=alpha want 2 (beta + gamma)")
}

func TestWindowBounds(t *testing.T) {
	// Fixed reference time so the tests are deterministic.
	now := time.Date(2026, 4, 18, 12, 0, 0, 0, time.UTC)

	t.Run("default 28d", func(t *testing.T) {
		from, to, days, err := windowBounds(StatsFilter{}, now)
		require.NoError(t, err, "windowBounds")
		assert.Equal(t, 28, days, "days: got")
		assert.True(t, to.Equal(now),
			"until: got %v want %v", to, now)
		wantFrom := now.Add(-28 * 24 * time.Hour)
		assert.True(t, from.Equal(wantFrom),
			"since: got %v want %v", from, wantFrom)
	})

	t.Run("Nd duration", func(t *testing.T) {
		_, _, days, err := windowBounds(
			StatsFilter{Since: "7d"}, now,
		)
		require.NoError(t, err, "windowBounds")
		assert.Equal(t, 7, days, "days: got")
	})

	t.Run("Nh duration", func(t *testing.T) {
		from, to, _, err := windowBounds(
			StatsFilter{Since: "48h"}, now,
		)
		require.NoError(t, err, "windowBounds")
		assert.Equal(t, 48*time.Hour, to.Sub(from), "span")
	})

	t.Run("bare date", func(t *testing.T) {
		from, _, _, err := windowBounds(
			StatsFilter{Since: "2026-04-01"}, now,
		)
		require.NoError(t, err, "windowBounds")
		assert.Equal(t, 2026, from.Year(),
			"since parsed: got %v want 2026-04-01", from)
		assert.Equal(t, time.April, from.Month(),
			"since parsed: got %v want 2026-04-01", from)
		assert.Equal(t, 1, from.Day(),
			"since parsed: got %v want 2026-04-01", from)
	})

	t.Run("invalid since", func(t *testing.T) {
		_, _, _, err := windowBounds(
			StatsFilter{Since: "bogus"}, now,
		)
		assert.Error(t, err, "expected error for invalid Since")
	})
}

func TestGetSessionStats_Distributions(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Five sessions chosen to place one row in each interesting bucket
	// for duration and peak_context. is_automated drives the scope_human
	// filter: a,b → automation (isAutomated=true); c,d,e → human.
	fixtures := []struct {
		id             string
		userMsgs       int
		peakCtx        int
		durMin         float64
		toolCalls      int
		assistantTurns int
		isAutomated    bool
	}{
		{"a", 0, 2_000, 0.5, 0, 0, true},
		{"b", 1, 8_000, 0.9, 1, 1, true},
		{"c", 3, 25_000, 10.0, 6, 3, false},
		{"d", 10, 60_000, 25.0, 15, 10, false},
		{"e", 30, 150_000, 120.0, 30, 30, false},
	}
	for _, f := range fixtures {
		insertSessionFixture(t, d, sessionFixture{
			id:             f.id,
			agent:          "claude",
			userMsgs:       f.userMsgs,
			peakContext:    f.peakCtx,
			hasPeakContext: true,
			durationMin:    f.durMin,
			startedAt:      hoursAgo(10),
			totalToolCalls: f.toolCalls,
			assistantTurns: f.assistantTurns,
			isAutomated:    f.isAutomated,
		})
	}

	stats, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d"})
	require.NoError(t, err, "GetSessionStats")

	// duration scope_all: 0.5→bucket0, 0.9→bucket0, 10→bucket2,
	// 25→bucket3, 120→bucket5 (top).
	gotAll := stats.Distributions.DurationMinutes.ScopeAll.Buckets
	wantCountsAll := []int{2, 0, 1, 1, 0, 1}
	require.Len(t, gotAll, len(wantCountsAll),
		"duration scope_all buckets")
	for i, w := range wantCountsAll {
		assert.Equal(t, w, gotAll[i].Count,
			"duration scope_all bucket %d", i)
	}
	// duration scope_human (c,d,e): bucket2=1, bucket3=1, bucket5=1.
	gotHuman := stats.Distributions.DurationMinutes.ScopeHuman.Buckets
	wantCountsHuman := []int{0, 0, 1, 1, 0, 1}
	require.Len(t, gotHuman, len(wantCountsHuman),
		"duration scope_human buckets")
	for i, w := range wantCountsHuman {
		assert.Equal(t, w, gotHuman[i].Count,
			"duration scope_human bucket %d", i)
	}

	// Means (arithmetic over included sessions).
	wantAllMean := (0.5 + 0.9 + 10 + 25 + 120) / 5.0
	gotAllMean := stats.Distributions.DurationMinutes.ScopeAll.Mean
	assert.InDelta(t, wantAllMean, gotAllMean, 0.01,
		"duration scope_all mean")
	wantHumanMean := (10.0 + 25.0 + 120.0) / 3.0
	gotHumanMean := stats.Distributions.DurationMinutes.ScopeHuman.Mean
	assert.InDelta(t, wantHumanMean, gotHumanMean, 0.01,
		"duration scope_human mean")

	// user_messages scope_all uses userMessagesEdgesAll
	// ([0,2),[2,6),[6,16),[16,31),[31,51),[51,inf)):
	// 0→0, 1→0, 3→1, 10→2, 30→3.
	gotUM := stats.Distributions.UserMessages.ScopeAll.Buckets
	wantUM := []int{2, 1, 1, 1, 0, 0}
	require.Len(t, gotUM, len(wantUM),
		"user_messages scope_all buckets")
	for i, w := range wantUM {
		assert.Equal(t, w, gotUM[i].Count,
			"user_messages scope_all bucket %d", i)
	}
	// user_messages scope_human uses userMessagesEdgesHuman (5 buckets,
	// dropping the automation band): 3→0, 10→1, 30→2.
	gotUMH := stats.Distributions.UserMessages.ScopeHuman.Buckets
	wantUMH := []int{1, 1, 1, 0, 0}
	require.Len(t, gotUMH, len(wantUMH),
		"user_messages scope_human buckets")
	for i, w := range wantUMH {
		assert.Equal(t, w, gotUMH[i].Count,
			"user_messages scope_human bucket %d", i)
	}

	// peak_context scope_all: 2k→0, 8k→0, 25k→1, 60k→2, 150k→4.
	gotPCAll := stats.Distributions.PeakContextTokens.ScopeAll.Buckets
	wantPCAll := []int{2, 1, 1, 0, 1, 0}
	for i, w := range wantPCAll {
		assert.Equal(t, w, gotPCAll[i].Count,
			"peak_context scope_all bucket %d", i)
	}
	// peak_context scope_human (c,d,e): 25k→1, 60k→2, 150k→4.
	gotPC := stats.Distributions.PeakContextTokens.ScopeHuman.Buckets
	assert.Equal(t, 1, gotPC[1].Count,
		"peak_context scope_human: %+v", gotPC)
	assert.Equal(t, 1, gotPC[2].Count,
		"peak_context scope_human: %+v", gotPC)
	assert.Equal(t, 1, gotPC[4].Count,
		"peak_context scope_human: %+v", gotPC)
	assert.False(t, stats.Distributions.PeakContextTokens.ClaudeOnly,
		"peak_context.claude_only is always false since #646")
	assert.Equal(t, 0,
		stats.Distributions.PeakContextTokens.NullCount,
		"peak_context.null_count")

	// tools_per_turn: a skipped (assistantTurns==0),
	// b=1/1=1, c=6/3=2, d=15/10=1.5, e=30/30=1.
	// toolsPerTurnEdges = [0,1,2,4,7,11,+Inf].
	gotTPT := stats.Distributions.ToolsPerTurn.ScopeAll.Buckets
	wantTPT := []int{0, 3, 1, 0, 0, 0}
	require.Len(t, gotTPT, len(wantTPT),
		"tools_per_turn scope_all buckets")
	for i, w := range wantTPT {
		assert.Equal(t, w, gotTPT[i].Count,
			"tools_per_turn scope_all bucket %d", i)
	}
}

func Test_computeDistributions_scopeHuman_flag(t *testing.T) {
	d := testDB(t)
	// Short non-automated: must count in scope_human.
	insertSessionFixture(t, d, sessionFixture{
		id: "short-human", userMsgs: 1, durationMin: 3,
		startedAt: hoursAgo(1), isAutomated: false,
	})
	// Multi-turn automated: must be excluded from scope_human.
	insertSessionFixture(t, d, sessionFixture{
		id: "auto-long", userMsgs: 4, durationMin: 30,
		startedAt: hoursAgo(1), isAutomated: true,
	})

	got, err := d.GetSessionStats(t.Context(), StatsFilter{Since: "1d"})
	require.NoError(t, err, "GetSessionStats")
	// scope_all has both rows — mean ~= 16.5.
	allMean := got.Distributions.DurationMinutes.ScopeAll.Mean
	require.InDelta(t, 16.5, allMean, 1.5,
		"scope_all duration mean")
	// scope_human has only the non-automated short session — mean ~= 3.
	humanMean := got.Distributions.DurationMinutes.ScopeHuman.Mean
	require.InDelta(t, 3.0, humanMean, 1.0,
		"scope_human duration mean (short-human only)")

	humanUserMessages := got.Distributions.UserMessages.ScopeHuman
	require.Equal(t, 0.0, humanUserMessages.Mean,
		"scope_human user_messages mean want 0 (<2 filtered)")
	bucketedHumanMessages := 0
	for _, bucket := range humanUserMessages.Buckets {
		bucketedHumanMessages += bucket.Count
	}
	require.Equal(t, 0, bucketedHumanMessages, "scope_human user_messages bucket total")
}

func TestGetSessionStats_Distributions_NullPeakContext(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// One Claude session lacks peak-context data; it must land in
	// NullCount rather than any peak_context bucket (including bucket 0).
	insertSessionFixture(t, d, sessionFixture{
		id: "np1", agent: "claude", userMsgs: 5,
		startedAt:   hoursAgo(5),
		durationMin: 3.0,
		// peakContext left at zero value AND hasPeakContext=false
	})
	insertSessionFixture(t, d, sessionFixture{
		id: "wp1", agent: "claude", userMsgs: 5,
		startedAt:      hoursAgo(5),
		durationMin:    3.0,
		peakContext:    20_000,
		hasPeakContext: true,
	})
	// Session from an agent that never reports peak context in this
	// window must NOT increment NullCount: such agents are outside the
	// metric entirely, only data-less rows of peak-context-reporting
	// agents tally as null (#646).
	insertSessionFixture(t, d, sessionFixture{
		id: "cx1", agent: "codex", userMsgs: 5,
		startedAt:   hoursAgo(5),
		durationMin: 3.0,
		// hasPeakContext left at false
	})

	stats, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d"})
	require.NoError(t, err, "GetSessionStats")

	pc := stats.Distributions.PeakContextTokens
	assert.Equal(t, 1, pc.NullCount,
		"null_count want 1 (only np1; codex cx1 must not count)")
	total := 0
	for _, b := range pc.ScopeAll.Buckets {
		total += b.Count
	}
	assert.Equal(t, 1, total,
		"scope_all bucket total want 1 "+
			"(the one Claude session with hasPeakContext=true)")
}

func TestGetSessionStats_Distributions_PeakContextNonClaude(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Regression for #646: hermes (and kimi/forge/zed) sessions carry
	// peak_context_tokens, but the distribution only counted rows with
	// agent == "claude" — an agent-filtered stats run reported all-zero
	// buckets despite has_peak_context_tokens being true on every row.
	insertSessionFixture(t, d, sessionFixture{
		id: "h1", agent: "hermes", userMsgs: 5,
		startedAt:      hoursAgo(5),
		durationMin:    3.0,
		peakContext:    25_000,
		hasPeakContext: true,
	})
	insertSessionFixture(t, d, sessionFixture{
		id: "h2", agent: "hermes", userMsgs: 5,
		startedAt:      hoursAgo(5),
		durationMin:    3.0,
		peakContext:    120_000,
		hasPeakContext: true,
	})
	// A hermes row without the data lands in NullCount, because hermes
	// demonstrably reports the metric in this window.
	insertSessionFixture(t, d, sessionFixture{
		id: "h3", agent: "hermes", userMsgs: 5,
		startedAt:   hoursAgo(5),
		durationMin: 3.0,
		// hasPeakContext left at false
	})

	stats, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d", Agent: "hermes"})
	require.NoError(t, err, "GetSessionStats")

	pc := stats.Distributions.PeakContextTokens
	total := 0
	for _, b := range pc.ScopeAll.Buckets {
		total += b.Count
	}
	assert.Equal(t, 2, total,
		"scope_all bucket total want 2 (both hermes rows with data): %+v",
		pc.ScopeAll.Buckets)
	// peakContextEdges: 25k → [10k,50k) bucket 1; 120k → [100k,150k) bucket 3.
	assert.Equal(t, 1, pc.ScopeAll.Buckets[1].Count,
		"25k hermes session in bucket 1: %+v", pc.ScopeAll.Buckets)
	assert.Equal(t, 1, pc.ScopeAll.Buckets[3].Count,
		"120k hermes session in bucket 3: %+v", pc.ScopeAll.Buckets)
	assert.InDelta(t, (25_000.0+120_000.0)/2.0, pc.ScopeAll.Mean, 0.01,
		"scope_all mean over the two hermes rows with data")
	assert.Equal(t, 1, pc.NullCount,
		"null_count want 1 (h3: hermes reports the metric, h3 lacks it)")
	assert.False(t, pc.ClaudeOnly, "claude_only must be false")
}

// seedVelocityMessages inserts len(offsetsSec) messages for sessionID,
// alternating user/assistant starting at role[0], with timestamps at
// startedAt+offsetsSec[i]. Used by velocity tests that need precise
// intervals between adjacent messages. Returns nothing; panics via t
// on any insert error.
func seedVelocityMessages(
	t *testing.T, d *DB, sessionID, startedAt string,
	offsetsSec []int,
) {
	t.Helper()
	start, err := time.Parse(time.RFC3339, startedAt)
	require.NoError(t, err,
		"seedVelocityMessages %s: parse startedAt %q",
		sessionID, startedAt)
	msgs := make([]Message, 0, len(offsetsSec))
	for i, off := range offsetsSec {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		ts := start.Add(time.Duration(off) * time.Second).
			UTC().Format(time.RFC3339)
		msgs = append(msgs, Message{
			SessionID:     sessionID,
			Ordinal:       i,
			Role:          role,
			Content:       fmt.Sprintf("m%d", i),
			ContentLength: 5,
			Timestamp:     ts,
		})
	}
	require.NoError(t, d.InsertMessages(msgs),
		"seedVelocityMessages %s: InsertMessages", sessionID)
}

func TestGetSessionStats_Velocity(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Two sessions with carefully chosen per-message gaps so the
	// expected percentile/mean/hourly values are determined.
	//
	// Session v1: 6 msgs at offsets 0,10,20,25,35,50 (seconds).
	//   Turn cycles (user→assistant): 10, 5, 15.
	//   First response: 10.
	//   Adjacent gaps: 10,10,5,10,15 = 50s active.
	// Session v2: 4 msgs at offsets 0,30,60,80.
	//   Turn cycles: 30, 20.
	//   First response: 30.
	//   Adjacent gaps: 30,30,20 = 80s active.
	//
	// Combined: turn cycles=[5,10,15,20,30], first responses=[10,30],
	// active seconds=130, messages=10.
	start := time.Now().UTC().Add(-5 * time.Hour).
		Format(time.RFC3339)

	insertSessionFixture(t, d, sessionFixture{
		id: "v1", agent: "claude", userMsgs: 3,
		messageCount: 6, startedAt: start,
	})
	seedVelocityMessages(t, d, "v1", start,
		[]int{0, 10, 20, 25, 35, 50})

	insertSessionFixture(t, d, sessionFixture{
		id: "v2", agent: "claude", userMsgs: 2,
		messageCount: 4, startedAt: start,
	})
	seedVelocityMessages(t, d, "v2", start,
		[]int{0, 30, 60, 80})

	stats, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d"})
	require.NoError(t, err, "GetSessionStats")

	// Turn cycle seconds, sorted = [5,10,15,20,30].
	// percentileFloat: P50 idx=int(5*0.5)=2 → 15, P90 idx=4 → 30.
	// Mean = (5+10+15+20+30)/5 = 16.
	tc := stats.Velocity.TurnCycleSeconds
	assert.Equal(t, 15.0, tc.P50, "TurnCycleSeconds.P50")
	assert.Equal(t, 30.0, tc.P90, "TurnCycleSeconds.P90")
	assert.InDelta(t, 16.0, tc.Mean, 0.001,
		"TurnCycleSeconds.Mean")

	// First response seconds, sorted = [10,30].
	// percentileFloat: P50 idx=int(2*0.5)=1 → 30, P90 idx=1 → 30.
	// Mean = (10+30)/2 = 20.
	fr := stats.Velocity.FirstResponseSeconds
	assert.Equal(t, 30.0, fr.P50, "FirstResponseSeconds.P50")
	assert.Equal(t, 30.0, fr.P90, "FirstResponseSeconds.P90")
	assert.InDelta(t, 20.0, fr.Mean, 0.001,
		"FirstResponseSeconds.Mean")

	// MessagesPerActiveHour: active seconds=130, messages=10.
	// activeMinutes = 130/60, per-hour = 10 / (activeMinutes/60)
	//               = 10 * 60 / (130/60) = 36000/130 ≈ 276.923.
	want := 36000.0 / 130.0
	assert.InDelta(t, want, stats.Velocity.MessagesPerActiveHour,
		0.01, "MessagesPerActiveHour")
}

// Empty case: no sessions at all. The velocity accumulator stays zeroed
// and every output field must read as 0 rather than NaN / unset.
func TestGetSessionStats_Velocity_Empty(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	stats, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d"})
	require.NoError(t, err, "GetSessionStats")

	tc := stats.Velocity.TurnCycleSeconds
	assert.Equal(t, 0.0, tc.P50, "TurnCycleSeconds.P50 want zero: %+v", tc)
	assert.Equal(t, 0.0, tc.P90, "TurnCycleSeconds.P90 want zero: %+v", tc)
	assert.Equal(t, 0.0, tc.Mean, "TurnCycleSeconds.Mean want zero: %+v", tc)
	fr := stats.Velocity.FirstResponseSeconds
	assert.Equal(t, 0.0, fr.P50, "FirstResponseSeconds.P50 want zero: %+v", fr)
	assert.Equal(t, 0.0, fr.P90, "FirstResponseSeconds.P90 want zero: %+v", fr)
	assert.Equal(t, 0.0, fr.Mean, "FirstResponseSeconds.Mean want zero: %+v", fr)
	assert.Equal(t, 0.0, stats.Velocity.MessagesPerActiveHour,
		"MessagesPerActiveHour")
}

// Single session with one user→assistant turn. One sample point feeds
// both the turn-cycle and first-response series, so P50 / P90 / Mean
// must all collapse to the same value.
func TestGetSessionStats_Velocity_SingleTurn(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// 2 msgs at offsets 0,60 (seconds): user→assistant delta = 60s.
	// Adjacent gap = 60s → activeMinutes = 1, totalMsgs = 2,
	// MessagesPerActiveHour = 2 / (1/60) = 120.
	start := time.Now().UTC().Add(-3 * time.Hour).
		Format(time.RFC3339)
	insertSessionFixture(t, d, sessionFixture{
		id: "s1", agent: "claude", userMsgs: 1,
		messageCount: 2, startedAt: start,
	})
	seedVelocityMessages(t, d, "s1", start, []int{0, 60})

	stats, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d"})
	require.NoError(t, err, "GetSessionStats")

	tc := stats.Velocity.TurnCycleSeconds
	assert.Equal(t, 60.0, tc.P50, "TurnCycleSeconds.P50")
	assert.Equal(t, 60.0, tc.P90, "TurnCycleSeconds.P90")
	assert.InDelta(t, 60.0, tc.Mean, 0.001,
		"TurnCycleSeconds.Mean")
	fr := stats.Velocity.FirstResponseSeconds
	assert.Equal(t, 60.0, fr.P50, "FirstResponseSeconds.P50")
	assert.Equal(t, 60.0, fr.P90, "FirstResponseSeconds.P90")
	assert.InDelta(t, 60.0, fr.Mean, 0.001,
		"FirstResponseSeconds.Mean")
	assert.Greater(t, stats.Velocity.MessagesPerActiveHour, 0.0,
		"MessagesPerActiveHour want > 0")
	want := 120.0
	assert.InDelta(t, want, stats.Velocity.MessagesPerActiveHour,
		0.001, "MessagesPerActiveHour")
}

// Zero-active-minutes boundary: two messages share a timestamp so the
// only adjacent gap is 0 (failing the gap > 0 guard). activeMinutes
// stays 0, totalMsgs is never bumped, and MessagesPerActiveHour must
// remain 0 even though the session survived the len(msgs) >= 2 filter.
func TestGetSessionStats_Velocity_ZeroActive(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	start := time.Now().UTC().Add(-2 * time.Hour).
		Format(time.RFC3339)
	insertSessionFixture(t, d, sessionFixture{
		id: "z1", agent: "claude", userMsgs: 1,
		messageCount: 2, startedAt: start,
	})
	seedVelocityMessages(t, d, "z1", start, []int{0, 0})

	stats, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d"})
	require.NoError(t, err, "GetSessionStats")

	assert.Equal(t, 0.0, stats.Velocity.MessagesPerActiveHour,
		"MessagesPerActiveHour")
}

func TestGetSessionStats_ToolMixAndModelMix(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Session tm1: 4 tool_calls across 3 categories (Bash×2, Edit, Read).
	insertSessionFixture(t, d, sessionFixture{
		id: "tm1", agent: "claude", userMsgs: 5,
		startedAt: hoursAgo(4),
	})
	seedToolCallsByCategory(t, d, "tm1",
		[]string{"Bash", "Bash", "Edit", "Read"})

	// Session tm2: 2 tool_calls (Grep, Bash).
	insertSessionFixture(t, d, sessionFixture{
		id: "tm2", agent: "claude", userMsgs: 3,
		startedAt: hoursAgo(3),
	})
	seedToolCallsByCategory(t, d, "tm2",
		[]string{"Grep", "Bash"})

	// Session mm1: 2 claude-opus-4-7 assistant messages (1000 + 2000).
	insertSessionFixture(t, d, sessionFixture{
		id: "mm1", agent: "claude", userMsgs: 2,
		startedAt: hoursAgo(2),
	})
	seedModelMessages(t, d, "mm1", 1, []struct {
		model  string
		tokens int
	}{
		{"claude-opus-4-7", 1000},
		{"claude-opus-4-7", 2000},
	})

	// Session mm2: 1 claude-sonnet-4-6 assistant message (500 tokens).
	insertSessionFixture(t, d, sessionFixture{
		id: "mm2", agent: "claude", userMsgs: 2,
		startedAt: hoursAgo(2),
	})
	seedModelMessages(t, d, "mm2", 1, []struct {
		model  string
		tokens int
	}{
		{"claude-sonnet-4-6", 500},
	})

	stats, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d"})
	require.NoError(t, err, "GetSessionStats")

	wantCats := map[string]int{
		"Bash": 3,
		"Edit": 1,
		"Read": 1,
		"Grep": 1,
	}
	gotCats := stats.ToolMix.ByCategory
	assert.Len(t, gotCats, len(wantCats),
		"ToolMix.ByCategory len (got=%v)", gotCats)
	for cat, want := range wantCats {
		assert.Equal(t, want, gotCats[cat],
			"ToolMix.ByCategory[%q]", cat)
	}
	assert.Equal(t, 6, stats.ToolMix.TotalCalls, "ToolMix.TotalCalls")

	wantTokens := map[string]int64{
		"claude-opus-4-7":   3000,
		"claude-sonnet-4-6": 500,
	}
	gotTokens := stats.ModelMix.ByTokens
	assert.Len(t, gotTokens, len(wantTokens),
		"ModelMix.ByTokens len (got=%v)", gotTokens)
	for model, want := range wantTokens {
		assert.Equal(t, want, gotTokens[model],
			"ModelMix.ByTokens[%q]", model)
	}
}

// Window and agent filters must gate both mixes: tool_calls and
// messages attached to sessions outside the window or not matching
// the agent filter must not appear in ToolMix or ModelMix.
func TestGetSessionStats_ToolMixAndModelMix_Filters(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// In-window claude session: should contribute to both mixes.
	// seedToolCallsByCategory uses ordinals 1..2; seedModelMessages
	// starts at 3 to avoid the UNIQUE(session_id, ordinal) collision.
	insertSessionFixture(t, d, sessionFixture{
		id: "in1", agent: "claude", userMsgs: 3,
		startedAt: hoursAgo(2),
	})
	seedToolCallsByCategory(t, d, "in1", []string{"Bash", "Read"})
	seedModelMessages(t, d, "in1", 3, []struct {
		model  string
		tokens int
	}{
		{"claude-opus-4-7", 800},
	})

	// Out-of-window session (50 days old): must be excluded entirely.
	oldStart := time.Now().UTC().Add(-50 * 24 * time.Hour).
		Format(time.RFC3339)
	insertSessionFixture(t, d, sessionFixture{
		id: "old1", agent: "claude", userMsgs: 3,
		startedAt: oldStart,
	})
	seedToolCallsByCategory(t, d, "old1", []string{"Edit", "Edit"})
	seedModelMessages(t, d, "old1", 3, []struct {
		model  string
		tokens int
	}{
		{"claude-opus-4-7", 9000},
	})

	// Wrong-agent session inside the window: excluded by Agent=claude.
	insertSessionFixture(t, d, sessionFixture{
		id: "cx1", agent: "codex", userMsgs: 3,
		startedAt: hoursAgo(2),
	})
	seedToolCallsByCategory(t, d, "cx1", []string{"Grep"})
	seedModelMessages(t, d, "cx1", 2, []struct {
		model  string
		tokens int
	}{
		{"codex-gpt-5", 7000},
	})

	stats, err := d.GetSessionStats(ctx, StatsFilter{
		Since: "28d", Agent: "claude",
	})
	require.NoError(t, err, "GetSessionStats")

	// Only in1's 2 tool_calls survive.
	assert.Equal(t, 2, stats.ToolMix.TotalCalls, "ToolMix.TotalCalls")
	assert.Equal(t, 1, stats.ToolMix.ByCategory["Bash"],
		"ToolMix.ByCategory: want Bash=1 Read=1, got %v",
		stats.ToolMix.ByCategory)
	assert.Equal(t, 1, stats.ToolMix.ByCategory["Read"],
		"ToolMix.ByCategory: want Bash=1 Read=1, got %v",
		stats.ToolMix.ByCategory)
	assert.Equal(t, 0, stats.ToolMix.ByCategory["Edit"],
		"out-of-window Edit leaked")
	assert.Equal(t, 0, stats.ToolMix.ByCategory["Grep"],
		"wrong-agent Grep leaked")

	// Only in1's 800 tokens survive.
	assert.Equal(t, int64(800),
		stats.ModelMix.ByTokens["claude-opus-4-7"],
		"ModelMix.ByTokens[claude-opus-4-7]")
	assert.NotContains(t, stats.ModelMix.ByTokens, "codex-gpt-5",
		"wrong-agent model leaked")
}

// Empty-window case: no sessions → both mixes must serialize as empty
// maps (not nil) so the JSON output keeps stable keys.
func TestGetSessionStats_ToolMixAndModelMix_Empty(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	stats, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d"})
	require.NoError(t, err, "GetSessionStats")
	assert.NotNil(t, stats.ToolMix.ByCategory,
		"ToolMix.ByCategory: want non-nil map")
	assert.Equal(t, 0, stats.ToolMix.TotalCalls,
		"ToolMix.TotalCalls")
	assert.NotNil(t, stats.ModelMix.ByTokens,
		"ModelMix.ByTokens: want non-nil map")
}

// AgentPortfolio aggregates session, message, and output-token counts
// per agent across the window. Primary names the agent with the most
// sessions, with alphabetical tie-breaking for determinism.
func TestGetSessionStats_AgentPortfolio(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// 3 claude sessions: messages 5,7,10 → 22; tokens 100,200,300 → 600.
	claude := []struct {
		id     string
		msgs   int
		tokens int
	}{
		{"cl1", 5, 100},
		{"cl2", 7, 200},
		{"cl3", 10, 300},
	}
	for _, c := range claude {
		insertSessionFixture(t, d, sessionFixture{
			id: c.id, agent: "claude", userMsgs: 3,
			messageCount:   c.msgs,
			totalOutputTok: c.tokens,
			startedAt:      hoursAgo(5),
		})
	}
	// 2 codex sessions: messages 3,6 → 9; tokens 50,100 → 150.
	codex := []struct {
		id     string
		msgs   int
		tokens int
	}{
		{"cx1", 3, 50},
		{"cx2", 6, 100},
	}
	for _, c := range codex {
		insertSessionFixture(t, d, sessionFixture{
			id: c.id, agent: "codex", userMsgs: 3,
			messageCount:   c.msgs,
			totalOutputTok: c.tokens,
			startedAt:      hoursAgo(5),
		})
	}
	// 1 cursor session: messages 4; tokens 80.
	insertSessionFixture(t, d, sessionFixture{
		id: "cu1", agent: "cursor", userMsgs: 3,
		messageCount:   4,
		totalOutputTok: 80,
		startedAt:      hoursAgo(5),
	})

	stats, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d"})
	require.NoError(t, err, "GetSessionStats")

	ap := stats.AgentPortfolio
	wantSessions := map[string]int{"claude": 3, "codex": 2, "cursor": 1}
	assert.Len(t, ap.BySessions, len(wantSessions),
		"BySessions len (got=%v)", ap.BySessions)
	for k, v := range wantSessions {
		assert.Equal(t, v, ap.BySessions[k],
			"BySessions[%q]", k)
	}

	wantMessages := map[string]int{"claude": 22, "codex": 9, "cursor": 4}
	for k, v := range wantMessages {
		assert.Equal(t, v, ap.ByMessages[k],
			"ByMessages[%q]", k)
	}

	wantTokens := map[string]int64{"claude": 600, "codex": 150, "cursor": 80}
	for k, v := range wantTokens {
		assert.Equal(t, v, ap.ByTokens[k],
			"ByTokens[%q]", k)
	}

	assert.Equal(t, "claude", ap.Primary, "Primary")
}

// Tie-break: two agents at equal session counts must resolve to the
// lexicographically smallest agent name. claude vs codex both at 2 →
// claude wins because "claude" < "codex".
func TestGetSessionStats_AgentPortfolio_TieBreak(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	for _, id := range []string{"cl1", "cl2"} {
		insertSessionFixture(t, d, sessionFixture{
			id: id, agent: "claude", userMsgs: 3,
			messageCount: 4, totalOutputTok: 100,
			startedAt: hoursAgo(5),
		})
	}
	for _, id := range []string{"cx1", "cx2"} {
		insertSessionFixture(t, d, sessionFixture{
			id: id, agent: "codex", userMsgs: 3,
			messageCount: 4, totalOutputTok: 100,
			startedAt: hoursAgo(5),
		})
	}
	insertSessionFixture(t, d, sessionFixture{
		id: "cu1", agent: "cursor", userMsgs: 3,
		messageCount: 4, totalOutputTok: 100,
		startedAt: hoursAgo(5),
	})

	stats, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d"})
	require.NoError(t, err, "GetSessionStats")
	require.Equal(t, 2, stats.AgentPortfolio.BySessions["claude"],
		"precondition: claude/codex must tie at 2 (got %v)",
		stats.AgentPortfolio.BySessions)
	require.Equal(t, 2, stats.AgentPortfolio.BySessions["codex"],
		"precondition: claude/codex must tie at 2 (got %v)",
		stats.AgentPortfolio.BySessions)
	assert.Equal(t, "claude", stats.AgentPortfolio.Primary,
		"Primary under tie want claude (alphabetical tie-break)")
}

// Empty-window case: AgentPortfolio maps must be non-nil (JSON encodes
// {} not null) and Primary must be empty without crashing.
func TestGetSessionStats_AgentPortfolio_Empty(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	stats, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d"})
	require.NoError(t, err, "GetSessionStats")
	ap := stats.AgentPortfolio
	assert.NotNil(t, ap.BySessions,
		"BySessions: want non-nil map")
	assert.NotNil(t, ap.ByMessages,
		"ByMessages: want non-nil map")
	assert.NotNil(t, ap.ByTokens,
		"ByTokens: want non-nil map")
	assert.NotNil(t, ap.BySessionsHuman,
		"BySessionsHuman: want non-nil map")
	assert.NotNil(t, ap.ByMessagesHuman,
		"ByMessagesHuman: want non-nil map")
	assert.NotNil(t, ap.ByTokensHuman,
		"ByTokensHuman: want non-nil map")
	assert.Empty(t, ap.BySessions,
		"empty window: got non-empty maps %+v", ap)
	assert.Empty(t, ap.ByMessages,
		"empty window: got non-empty maps %+v", ap)
	assert.Empty(t, ap.ByTokens,
		"empty window: got non-empty maps %+v", ap)
	assert.Empty(t, ap.BySessionsHuman,
		"empty window: got non-empty human maps %+v", ap)
	assert.Empty(t, ap.ByMessagesHuman,
		"empty window: got non-empty human maps %+v", ap)
	assert.Empty(t, ap.ByTokensHuman,
		"empty window: got non-empty human maps %+v", ap)
	assert.Empty(t, ap.Primary, "Primary")
	assert.Empty(t, ap.PrimaryHuman, "PrimaryHuman")
}

func Test_computeAgentPortfolio_humanScoped(t *testing.T) {
	d := testDB(t)
	insertSessionFixture(t, d, sessionFixture{
		id: "claude-human", agent: "claude", userMsgs: 3,
		startedAt: hoursAgo(1), totalOutputTok: 100,
		isAutomated: false,
	})
	insertSessionFixture(t, d, sessionFixture{
		id: "codex-auto", agent: "codex", userMsgs: 1,
		startedAt: hoursAgo(1), totalOutputTok: 50,
		isAutomated: true,
	})
	insertSessionFixture(t, d, sessionFixture{
		id: "gemini-auto", agent: "gemini", userMsgs: 1,
		startedAt: hoursAgo(1), totalOutputTok: 25,
		isAutomated: true,
	})

	got, err := d.GetSessionStats(t.Context(), StatsFilter{Since: "1d"})
	require.NoError(t, err, "GetSessionStats")
	ap := got.AgentPortfolio

	// All-sessions view: every agent present.
	require.Equal(t, 1, ap.BySessions["claude"],
		"BySessions = %v, want claude=1,codex=1,gemini=1", ap.BySessions)
	require.Equal(t, 1, ap.BySessions["codex"],
		"BySessions = %v, want claude=1,codex=1,gemini=1", ap.BySessions)
	require.Equal(t, 1, ap.BySessions["gemini"],
		"BySessions = %v, want claude=1,codex=1,gemini=1", ap.BySessions)
	// primary ties on count; lexicographic min wins → claude.
	require.Equal(t, "claude", ap.Primary, "Primary")

	// Human-scoped view: only claude.
	require.NotContains(t, ap.BySessionsHuman, "codex",
		"BySessionsHuman must exclude codex: %v",
		ap.BySessionsHuman)
	require.NotContains(t, ap.BySessionsHuman, "gemini",
		"BySessionsHuman must exclude gemini: %v",
		ap.BySessionsHuman)
	require.Equal(t, 1, ap.BySessionsHuman["claude"], "BySessionsHuman[claude]")
	require.Equal(t, int64(100), ap.ByTokensHuman["claude"], "ByTokensHuman[claude]")
	require.Equal(t, "claude", ap.PrimaryHuman, "PrimaryHuman")
}

// cacheTokenBreakdown names the four token dimensions the cache
// economics section reads per assistant message.
type cacheTokenBreakdown struct {
	input         int
	output        int
	cacheCreation int
	cacheRead     int
}

// seedCacheEconomicsMessage inserts one assistant message whose
// token_usage JSON carries a full input/output/cache_* breakdown.
// The helper lives next to seedModelMessages so cache-economics tests
// can set the four cache-dimension counts directly without teaching
// seedModelMessages about fields it doesn't use.
func seedCacheEconomicsMessage(
	t *testing.T, d *DB, sessionID string, ordinal int,
	model string, b cacheTokenBreakdown,
) {
	t.Helper()
	payload := fmt.Sprintf(
		`{"input_tokens":%d,"output_tokens":%d,`+
			`"cache_creation_input_tokens":%d,`+
			`"cache_read_input_tokens":%d}`,
		b.input, b.output, b.cacheCreation, b.cacheRead,
	)
	m := asstMsg(sessionID, ordinal, "reply")
	m.Model = model
	m.OutputTokens = b.output
	m.HasOutputTokens = true
	m.TokenUsage = json.RawMessage(payload)
	require.NoError(t, d.InsertMessages([]Message{m}),
		"seedCacheEconomicsMessage %s ord=%d", sessionID, ordinal)
}

// TestGetSessionStats_CacheEconomics exercises the cache-economics
// computation end-to-end: three Claude sessions with known token
// mixes across two models, one codex session that must NOT affect
// the output (cache economics is Claude-only). The test locks the
// weighted-mean overall rule, the bucket assignment, and the two
// dollar calculations against hand-computed values.
func TestGetSessionStats_CacheEconomics(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	require.NoError(t, d.UpsertModelPricing([]ModelPricing{
		{
			ModelPattern:         "claude-opus-4-7",
			InputPerMTok:         15.0,
			OutputPerMTok:        75.0,
			CacheCreationPerMTok: 18.75,
			CacheReadPerMTok:     1.5,
		},
		{
			ModelPattern:         "claude-sonnet-4-6",
			InputPerMTok:         3.0,
			OutputPerMTok:        15.0,
			CacheCreationPerMTok: 3.75,
			CacheReadPerMTok:     0.3,
		},
	}), "UpsertModelPricing")

	// ce1 (opus): ratio = 9000 / (1000+9000+100) = 9000/10100 ≈ 0.8911
	// → cacheHitRatioEdges bucket 3 ([0.75, 0.95)).
	insertSessionFixture(t, d, sessionFixture{
		id: "ce1", agent: "claude", userMsgs: 3,
		startedAt: hoursAgo(5),
	})
	seedCacheEconomicsMessage(t, d, "ce1", 1, "claude-opus-4-7",
		cacheTokenBreakdown{
			input: 1000, output: 500,
			cacheCreation: 100, cacheRead: 9000,
		})

	// ce2 (sonnet): ratio = 3000/(500+3000+50) = 3000/3550 ≈ 0.8451
	// → bucket 3.
	insertSessionFixture(t, d, sessionFixture{
		id: "ce2", agent: "claude", userMsgs: 3,
		startedAt: hoursAgo(4),
	})
	seedCacheEconomicsMessage(t, d, "ce2", 1, "claude-sonnet-4-6",
		cacheTokenBreakdown{
			input: 500, output: 200,
			cacheCreation: 50, cacheRead: 3000,
		})

	// ce3 (opus): ratio = 100/(100+100+0) = 0.5
	// → bucket 2 ([0.5, 0.75)).
	insertSessionFixture(t, d, sessionFixture{
		id: "ce3", agent: "claude", userMsgs: 3,
		startedAt: hoursAgo(3),
	})
	seedCacheEconomicsMessage(t, d, "ce3", 1, "claude-opus-4-7",
		cacheTokenBreakdown{
			input: 100, output: 50,
			cacheCreation: 0, cacheRead: 100,
		})

	// Codex session with non-empty token_usage: must NOT influence
	// any cache_economics number (section is Claude-only).
	insertSessionFixture(t, d, sessionFixture{
		id: "cx1", agent: "codex", userMsgs: 3,
		startedAt: hoursAgo(2),
	})
	seedCacheEconomicsMessage(t, d, "cx1", 1, "gpt-5",
		cacheTokenBreakdown{
			input: 100000, output: 50000,
			cacheCreation: 0, cacheRead: 0,
		})

	stats, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d"})
	require.NoError(t, err, "GetSessionStats")

	ce := stats.CacheEconomics
	require.NotNil(t, ce, "CacheEconomics: want populated")
	assert.True(t, ce.ClaudeOnly, "ClaudeOnly")

	// Overall = sum(cache_read) / sum(denominator) (weighted mean).
	// = (9000 + 3000 + 100) / (10100 + 3550 + 200)
	// = 12100 / 13850 ≈ 0.873646.
	wantOverall := 12100.0 / 13850.0
	assert.InDelta(t, wantOverall, ce.CacheHitRatio.Overall, 1e-6,
		"CacheHitRatio.Overall")

	wantBuckets := []int{0, 0, 1, 2, 0}
	require.Len(t, ce.CacheHitRatio.Buckets, len(wantBuckets),
		"CacheHitRatio.Buckets")
	for i, w := range wantBuckets {
		assert.Equal(t, w, ce.CacheHitRatio.Buckets[i].Count,
			"CacheHitRatio.Buckets[%d]", i)
	}

	// Per-message cost (rates in $/MTok, so divide by 1e6):
	//   ce1 opus = (1000*15 + 500*75 + 100*18.75 + 9000*1.5)/1e6
	//           = (15000 + 37500 + 1875 + 13500)/1e6 = 0.067875
	//   ce2 sonn = (500*3 + 200*15 + 50*3.75 + 3000*0.3)/1e6
	//           = (1500 + 3000 + 187.5 + 900)/1e6 = 0.0055875
	//   ce3 opus = (100*15 + 50*75 + 0 + 100*1.5)/1e6
	//           = (1500 + 3750 + 150)/1e6 = 0.0054
	wantSpent := 0.067875 + 0.0055875 + 0.0054
	assert.InDelta(t, wantSpent, ce.DollarsSpent, 1e-9, "DollarsSpent")

	// cost_without_cache reprices input + cache_creation + cache_read
	// at the input rate, keeping output unchanged. cache_creation
	// tokens would still have been sent as ordinary input in the
	// counterfactual, so they are not zeroed (matches usage.go and
	// frontend/src/lib/utils/usageSavings.ts).
	//   ce1 opus = (15*(1000+100+9000) + 75*500)/1e6
	//           = (15*10100 + 37500)/1e6 = 0.189
	//   ce2 sonn = (3*(500+50+3000) + 15*200)/1e6
	//           = (3*3550 + 3000)/1e6 = 0.01365
	//   ce3 opus = (15*(100+0+100) + 75*50)/1e6
	//           = (3000 + 3750)/1e6 = 0.00675
	wantWithoutCache := 0.189 + 0.01365 + 0.00675
	wantSavings := wantWithoutCache - wantSpent
	assert.InDelta(t, wantSavings, ce.DollarsSavedVsUncached, 1e-9,
		"DollarsSavedVsUncached")
}

// TestGetSessionStats_CacheEconomics_NoClaude verifies that the
// pointer remains nil when the window contains zero Claude sessions.
// A zero-valued *StatsCacheEconomics would emit "claude_only":false,
// "overall":0, etc. — false negatives the profile page would render
// as a legitimate empty cache-economics section.
func TestGetSessionStats_CacheEconomics_NoClaude(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Pricing is present so the nil result isn't an artifact of a
	// missing pricing map.
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern: "claude-sonnet-4-6",
		InputPerMTok: 3.0, OutputPerMTok: 15.0,
		CacheCreationPerMTok: 3.75, CacheReadPerMTok: 0.3,
	}}), "UpsertModelPricing")

	insertSessionFixture(t, d, sessionFixture{
		id: "cx1", agent: "codex", userMsgs: 3,
		startedAt: hoursAgo(2),
	})
	seedCacheEconomicsMessage(t, d, "cx1", 1, "gpt-5",
		cacheTokenBreakdown{input: 1000, output: 500})

	stats, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d"})
	require.NoError(t, err, "GetSessionStats")
	assert.Nil(t, stats.CacheEconomics, "CacheEconomics")
}

// TestGetSessionStats_CacheEconomics_ZeroDenominatorSkipped seeds one
// Claude session whose only message has zero input/cache tokens. The
// per-session ratio denominator is zero so the session must be
// skipped from both the histogram and the overall weighted mean,
// without tripping the nil-vs-populated rule.
func TestGetSessionStats_CacheEconomics_ZeroDenominatorSkipped(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern: "claude-opus-4-7",
		InputPerMTok: 15.0, OutputPerMTok: 75.0,
		CacheCreationPerMTok: 18.75, CacheReadPerMTok: 1.5,
	}}), "UpsertModelPricing")

	// Session with a contributing denominator.
	insertSessionFixture(t, d, sessionFixture{
		id: "ok", agent: "claude", userMsgs: 3,
		startedAt: hoursAgo(3),
	})
	seedCacheEconomicsMessage(t, d, "ok", 1, "claude-opus-4-7",
		cacheTokenBreakdown{
			input: 100, output: 50, cacheRead: 300,
		})

	// Session with zero denominator — must be skipped from the
	// histogram, not crash on divide-by-zero.
	insertSessionFixture(t, d, sessionFixture{
		id: "z", agent: "claude", userMsgs: 3,
		startedAt: hoursAgo(2),
	})
	seedCacheEconomicsMessage(t, d, "z", 1, "claude-opus-4-7",
		cacheTokenBreakdown{input: 0, output: 10, cacheRead: 0})

	stats, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d"})
	require.NoError(t, err, "GetSessionStats")
	ce := stats.CacheEconomics
	require.NotNil(t, ce, "CacheEconomics: want populated")
	// Only "ok" contributes: ratio = 300 / (100+300) = 0.75 → bucket 3.
	total := 0
	for _, b := range ce.CacheHitRatio.Buckets {
		total += b.Count
	}
	assert.Equal(t, 1, total,
		"histogram total want 1 (zero-denom skipped)")
	assert.Equal(t, 1, ce.CacheHitRatio.Buckets[3].Count,
		"bucket 3 [0.75,0.95)")
	// Overall = 300/400 = 0.75 exactly (zero-denom session excluded
	// from both numerator and denominator).
	assert.InDelta(t, 0.75, ce.CacheHitRatio.Overall, 1e-9, "Overall")
}

func TestPickPrimaryAgent(t *testing.T) {
	cases := []struct {
		name  string
		input map[string]int
		want  string
	}{
		{
			name:  "empty map",
			input: map[string]int{},
			want:  "",
		},
		{
			name:  "single agent",
			input: map[string]int{"claude": 5},
			want:  "claude",
		},
		{
			name:  "strict max wins",
			input: map[string]int{"claude": 3, "codex": 2, "cursor": 1},
			want:  "claude",
		},
		{
			name:  "alphabetical tie-break across two",
			input: map[string]int{"codex": 2, "claude": 2},
			want:  "claude",
		},
		{
			name: "alphabetical tie-break across three",
			input: map[string]int{
				"codex": 2, "claude": 2, "cursor": 2,
			},
			want: "claude",
		},
		{
			name:  "tie below max ignored",
			input: map[string]int{"codex": 3, "claude": 2, "cursor": 2},
			want:  "codex",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, pickPrimaryAgent(c.input),
				"pickPrimaryAgent(%v)", c.input)
		})
	}
}

// hoursAgoAt returns an RFC3339 timestamp for (now - hours, shifted to
// the given minute and second of that hour). Used by temporal tests
// that need to place messages inside a specific UTC hour boundary.
func hoursAgoAt(hours, minute, second int) string {
	t := time.Now().UTC().
		Add(-time.Duration(hours) * time.Hour).
		Truncate(time.Hour).
		Add(time.Duration(minute) * time.Minute).
		Add(time.Duration(second) * time.Second)
	return t.Format(time.RFC3339)
}

// utcHourBoundary returns the UTC-hour-boundary TS string (what
// Temporal.HourlyUTC entries use) for (now - hours).
func utcHourBoundary(hours int) string {
	t := time.Now().UTC().
		Add(-time.Duration(hours) * time.Hour).
		Truncate(time.Hour)
	return t.Format("2006-01-02T15:00:00Z")
}

// findHourlyUTC returns the entry matching ts, or nil when absent.
// Shared by every temporal test that checks per-hour numbers.
func findHourlyUTC(
	entries []TemporalHourlyUTCEntry, ts string,
) *TemporalHourlyUTCEntry {
	for i := range entries {
		if entries[i].TS == ts {
			return &entries[i]
		}
	}
	return nil
}

func TestGetSessionStats_Temporal_HourlyGrouping(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Three hours of activity: H-5 (2 user msgs in one session),
	// H-4 (1 user msg in a different session), H-3 (2 user msgs
	// across two sessions). Assistant messages are ignored.
	insertSessionFixture(t, d, sessionFixture{
		id: "s1", userMsgs: 2, startedAt: hoursAgo(6),
	})
	insertSessionFixture(t, d, sessionFixture{
		id: "s2", userMsgs: 1, startedAt: hoursAgo(5),
	})
	insertSessionFixture(t, d, sessionFixture{
		id: "s3", userMsgs: 1, startedAt: hoursAgo(4),
	})
	insertSessionFixture(t, d, sessionFixture{
		id: "s4", userMsgs: 1, startedAt: hoursAgo(4),
	})
	insertMessages(t, d,
		userMsgAt("s1", 0, "a", hoursAgoAt(5, 5, 0)),
		userMsgAt("s1", 1, "b", hoursAgoAt(5, 45, 0)),
		asstMsgAt("s1", 2, "ignored", hoursAgoAt(5, 50, 0)),
		userMsgAt("s2", 0, "c", hoursAgoAt(4, 10, 0)),
		userMsgAt("s3", 0, "d", hoursAgoAt(3, 30, 0)),
		userMsgAt("s4", 0, "e", hoursAgoAt(3, 45, 0)),
	)

	stats, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d"})
	require.NoError(t, err, "GetSessionStats")
	hours := stats.Temporal.HourlyUTC
	require.Len(t, hours, 3,
		"hourly_utc want 3 entries: %+v", hours)

	// Entries must be sorted by TS ascending.
	for i := 1; i < len(hours); i++ {
		assert.Less(t, hours[i-1].TS, hours[i].TS,
			"hourly_utc not ascending")
	}

	// H-5: 2 user messages, 1 distinct session.
	if e := findHourlyUTC(hours, utcHourBoundary(5)); assert.NotNilf(t, e,
		"missing hour entry %q", utcHourBoundary(5)) {
		assert.Equal(t, 2, e.UserMessages, "H-5 user_messages")
		assert.Equal(t, 1, e.Sessions, "H-5 sessions: got")
	}
	// H-4: 1 user message, 1 session.
	if e := findHourlyUTC(hours, utcHourBoundary(4)); assert.NotNilf(t, e,
		"missing hour entry %q", utcHourBoundary(4)) {
		assert.Equal(t, 1, e.UserMessages, "H-4 user_messages")
		assert.Equal(t, 1, e.Sessions, "H-4 sessions: got")
	}
	// H-3: 2 user messages from 2 distinct sessions.
	if e := findHourlyUTC(hours, utcHourBoundary(3)); assert.NotNilf(t, e,
		"missing hour entry %q", utcHourBoundary(3)) {
		assert.Equal(t, 2, e.UserMessages, "H-3 user_messages")
		assert.Equal(t, 2, e.Sessions, "H-3 sessions: got")
	}
}

func TestGetSessionStats_Temporal_MidnightBoundary(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Pick a fixed day inside the default 28d window and seed one
	// message 1 second before midnight UTC and one 1 second after.
	// They MUST land in different hour entries.
	day := time.Now().UTC().
		Add(-5 * 24 * time.Hour).Truncate(24 * time.Hour)
	before := day.Add(23*time.Hour + 59*time.Minute + 59*time.Second)
	after := day.Add(24 * time.Hour).Add(1 * time.Second)

	insertSessionFixture(t, d, sessionFixture{
		id: "s1", userMsgs: 2,
		startedAt: before.Format(time.RFC3339),
	})
	insertMessages(t, d,
		userMsgAt("s1", 0, "late", before.Format(time.RFC3339)),
		userMsgAt("s1", 1, "early", after.Format(time.RFC3339)),
	)

	stats, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d"})
	require.NoError(t, err, "GetSessionStats")
	beforeTS := before.Truncate(time.Hour).
		Format("2006-01-02T15:00:00Z")
	afterTS := after.Truncate(time.Hour).
		Format("2006-01-02T15:00:00Z")
	require.NotEqual(t, afterTS, beforeTS,
		"test setup: before %q and after %q collapsed to "+
			"the same hour", beforeTS, afterTS)
	if e := findHourlyUTC(stats.Temporal.HourlyUTC, beforeTS); assert.NotNilf(t, e,
		"missing before-midnight hour %q", beforeTS) {
		assert.Equal(t, 1, e.UserMessages,
			"before-midnight user_messages")
	}
	if e := findHourlyUTC(stats.Temporal.HourlyUTC, afterTS); assert.NotNilf(t, e,
		"missing after-midnight hour %q", afterTS) {
		assert.Equal(t, 1, e.UserMessages,
			"after-midnight user_messages")
	}
}

func TestGetSessionStats_Temporal_OutOfWindowExcluded(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// One session inside the window, one outside. With Since=2d, the
	// out-of-window session should not contribute sessionIDs, so its
	// messages are absent from hourly_utc even if their timestamps
	// fall within the window-viewed hours.
	insertSessionFixture(t, d, sessionFixture{
		id: "in", userMsgs: 1, startedAt: hoursAgo(10),
	})
	insertSessionFixture(t, d, sessionFixture{
		id: "out", userMsgs: 1,
		// 40 days ago, well past Since=2d.
		startedAt: time.Now().UTC().
			Add(-40 * 24 * time.Hour).Format(time.RFC3339),
	})
	insertMessages(t, d,
		userMsgAt("in", 0, "ok", hoursAgoAt(10, 0, 0)),
		userMsgAt("out", 0, "nope",
			hoursAgoAt(10, 30, 0)), // same hour as "in"
	)

	stats, err := d.GetSessionStats(ctx, StatsFilter{Since: "2d"})
	require.NoError(t, err, "GetSessionStats")
	// Exactly one hour bucket, with a single user message (from "in").
	require.Len(t, stats.Temporal.HourlyUTC, 1,
		"hourly_utc want 1 entry: %+v", stats.Temporal.HourlyUTC)
	got := stats.Temporal.HourlyUTC[0]
	assert.Equal(t, 1, got.UserMessages, "in-window user_messages")
	assert.Equal(t, 1, got.Sessions, "in-window sessions: got")
}

func TestGetSessionStats_Temporal_SessionsDistinctPerHour(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Same session sends 3 user messages in H-6 (counts as 1 session,
	// 3 user_messages) and 1 user message in H-5 (counts as 1 session,
	// 1 user_message). The session appears in both hour entries.
	insertSessionFixture(t, d, sessionFixture{
		id: "s1", userMsgs: 4, startedAt: hoursAgo(7),
	})
	insertMessages(t, d,
		userMsgAt("s1", 0, "a", hoursAgoAt(6, 5, 0)),
		userMsgAt("s1", 1, "b", hoursAgoAt(6, 25, 0)),
		userMsgAt("s1", 2, "c", hoursAgoAt(6, 55, 0)),
		userMsgAt("s1", 3, "d", hoursAgoAt(5, 15, 0)),
	)

	stats, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d"})
	require.NoError(t, err, "GetSessionStats")
	if e := findHourlyUTC(
		stats.Temporal.HourlyUTC, utcHourBoundary(6),
	); assert.NotNil(t, e, "missing H-6 entry") {
		assert.Equal(t, 3, e.UserMessages, "H-6 user_messages")
		assert.Equal(t, 1, e.Sessions,
			"H-6 sessions (same session 3 msgs)")
	}
	if e := findHourlyUTC(
		stats.Temporal.HourlyUTC, utcHourBoundary(5),
	); assert.NotNil(t, e, "missing H-5 entry") {
		assert.Equal(t, 1, e.UserMessages, "H-5 user_messages")
		assert.Equal(t, 1, e.Sessions, "H-5 sessions: got")
	}
}

func TestGetSessionStats_Temporal_EmptyWindowEmptySlice(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	stats, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d"})
	require.NoError(t, err, "GetSessionStats")
	assert.NotNil(t, stats.Temporal.HourlyUTC,
		"hourly_utc must be a non-nil empty slice, got nil")
	assert.Len(t, stats.Temporal.HourlyUTC, 0, "hourly_utc: got len")
	// Reporter timezone should still be populated (claim in the spec).
	assert.NotEmpty(t, stats.Temporal.ReporterTimezone,
		"reporter_timezone must be populated even when "+
			"hourly_utc is empty")
	// JSON encoding must emit [] not null.
	raw, err := json.Marshal(stats.Temporal.HourlyUTC)
	require.NoError(t, err, "json.Marshal")
	assert.Equal(t, "[]", string(raw), "hourly_utc JSON")
}

func TestGetSessionStats_Temporal_ReporterTimezone_FilterWins(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	stats, err := d.GetSessionStats(ctx, StatsFilter{
		Since:    "28d",
		Timezone: "America/New_York",
	})
	require.NoError(t, err, "GetSessionStats")
	assert.Equal(t, "America/New_York", stats.Temporal.ReporterTimezone,
		"reporter_timezone")
}

func TestReporterTimezone_Precedence(t *testing.T) {
	prev, hadTZ := os.LookupEnv("TZ")
	t.Cleanup(func() {
		if hadTZ {
			_ = os.Setenv("TZ", prev)
		} else {
			_ = os.Unsetenv("TZ")
		}
	})

	// Filter wins over env.
	err := os.Setenv("TZ", "Europe/Berlin")
	require.NoError(t, err, "set TZ")
	assert.Equal(t, "Asia/Tokyo",
		reporterTimezone(StatsFilter{Timezone: "Asia/Tokyo"}),
		"filter wins")

	// No filter → env wins.
	assert.Equal(t, "Europe/Berlin",
		reporterTimezone(StatsFilter{}), "env wins")

	// No filter, no env → time.Local fallback.
	err = os.Unsetenv("TZ")
	require.NoError(t, err, "unset TZ")
	got := reporterTimezone(StatsFilter{})
	assert.NotEmpty(t, got, "time.Local fallback: got empty string")
	assert.Equal(t, time.Local.String(), got, "time.Local fallback")
}

func TestGetSessionStats_Temporal_FilterByAgentFlowsThrough(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Two sessions, same hour, different agents. Filter=claude must
	// leave the codex session's messages out of hourly_utc.
	insertSessionFixture(t, d, sessionFixture{
		id: "c1", agent: "claude", userMsgs: 1,
		startedAt: hoursAgo(4),
	})
	insertSessionFixture(t, d, sessionFixture{
		id: "x1", agent: "codex", userMsgs: 1,
		startedAt: hoursAgo(4),
	})
	insertMessages(t, d,
		userMsgAt("c1", 0, "hi", hoursAgoAt(3, 10, 0)),
		userMsgAt("x1", 0, "hi", hoursAgoAt(3, 20, 0)),
	)

	stats, err := d.GetSessionStats(ctx, StatsFilter{
		Since: "28d", Agent: "claude",
	})
	require.NoError(t, err, "GetSessionStats")
	if e := findHourlyUTC(
		stats.Temporal.HourlyUTC, utcHourBoundary(3),
	); assert.NotNil(t, e, "missing H-3 entry") {
		assert.Equal(t, 1, e.UserMessages,
			"filter=claude user_messages")
		assert.Equal(t, 1, e.Sessions, "filter=claude sessions")
	}
}

func TestGetSessionStats_Temporal_IgnoresAssistantMessages(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Only assistant messages in a session — hourly_utc must be empty.
	insertSessionFixture(t, d, sessionFixture{
		id: "only-asst", userMsgs: 0, messageCount: 2,
		startedAt: hoursAgo(3),
	})
	insertMessages(t, d,
		asstMsgAt("only-asst", 0, "a", hoursAgoAt(2, 5, 0)),
		asstMsgAt("only-asst", 1, "b", hoursAgoAt(2, 10, 0)),
	)

	stats, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d"})
	require.NoError(t, err, "GetSessionStats")
	assert.Empty(t, stats.Temporal.HourlyUTC,
		"hourly_utc should be empty when only assistant msgs")
}

func TestGetSessionStats_Temporal_SkipsEmptyTimestamps(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// User message with empty timestamp must not bucket to epoch or
	// anywhere else — strftime returns NULL for empty strings and we
	// drop the row.
	insertSessionFixture(t, d, sessionFixture{
		id: "s1", userMsgs: 2, startedAt: hoursAgo(3),
	})
	insertMessages(t, d,
		userMsgAt("s1", 0, "good", hoursAgoAt(2, 15, 0)),
		userMsgAt("s1", 1, "blank", ""),
	)

	stats, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d"})
	require.NoError(t, err, "GetSessionStats")
	require.Len(t, stats.Temporal.HourlyUTC, 1,
		"hourly_utc want 1 entry: %+v", stats.Temporal.HourlyUTC)
	assert.Equal(t, 1, stats.Temporal.HourlyUTC[0].UserMessages,
		"user_messages want 1 (blank skipped)")
}

// TestGetSessionStats_Outcomes_Happy seeds five Claude sessions spanning
// the full outcome vocabulary that agentsview actually stores
// ("completed", "abandoned", "errored", "unknown" — see
// internal/signals/outcome.go) and asserts every field on StatsOutcomes.
// Per-row tool/retry/compaction/churn values are deliberately asymmetric
// (distinct sums: retries=7, compactions=10, churn=15) so a field-swap
// regression in the loader or aggregator would be caught.
func TestGetSessionStats_Outcomes_Happy(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// s-a: completed / grade A / 2 tools / 1 retry / 3 compactions / 5 churn
	insertSessionFixture(t, d, sessionFixture{
		id: "s-a", userMsgs: 4, startedAt: hoursAgo(6),
		totalToolCalls: 2, assistantTurns: 2,
	})
	updateSignals(t, d, "s-a", SessionSignalUpdate{
		Outcome:         "completed",
		HealthGrade:     new("A"),
		ToolRetryCount:  1,
		CompactionCount: 3,
		EditChurnCount:  5,
	})

	// s-b: completed / grade B / 4 tools / 0 retries / 1 compaction / 0 churn
	insertSessionFixture(t, d, sessionFixture{
		id: "s-b", userMsgs: 3, startedAt: hoursAgo(5),
		totalToolCalls: 4, assistantTurns: 3,
	})
	updateSignals(t, d, "s-b", SessionSignalUpdate{
		Outcome:         "completed",
		HealthGrade:     new("B"),
		ToolRetryCount:  0,
		CompactionCount: 1,
		EditChurnCount:  0,
	})

	// s-c: abandoned / grade C / 6 tools / 3 retries / 0 compactions / 4 churn
	insertSessionFixture(t, d, sessionFixture{
		id: "s-c", userMsgs: 6, startedAt: hoursAgo(4),
		totalToolCalls: 6, assistantTurns: 4,
	})
	updateSignals(t, d, "s-c", SessionSignalUpdate{
		Outcome:         "abandoned",
		HealthGrade:     new("C"),
		ToolRetryCount:  3,
		CompactionCount: 0,
		EditChurnCount:  4,
	})

	// s-d: errored / grade D / 8 tools / 2 retries / 2 compactions / 6 churn
	insertSessionFixture(t, d, sessionFixture{
		id: "s-d", userMsgs: 5, startedAt: hoursAgo(3),
		totalToolCalls: 8, assistantTurns: 5,
	})
	updateSignals(t, d, "s-d", SessionSignalUpdate{
		Outcome:         "errored",
		HealthGrade:     new("D"),
		ToolRetryCount:  2,
		CompactionCount: 2,
		EditChurnCount:  6,
	})

	// s-e: unknown / no grade / 5 tools / 1 retry / 4 compactions / 0 churn.
	// Explicit "unknown" exercises the default branch; the nil HealthGrade
	// pointer must NOT add an entry to GradeDistribution.
	insertSessionFixture(t, d, sessionFixture{
		id: "s-e", userMsgs: 2, startedAt: hoursAgo(2),
		totalToolCalls: 5, assistantTurns: 2,
	})
	updateSignals(t, d, "s-e", SessionSignalUpdate{
		Outcome:         "unknown",
		HealthGrade:     nil,
		ToolRetryCount:  1,
		CompactionCount: 4,
		EditChurnCount:  0,
	})

	stats, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d"})
	require.NoError(t, err, "GetSessionStats")
	out := stats.Outcomes
	require.NotNil(t, out, "Outcomes: want populated")
	assert.True(t, out.ClaudeOnly, "ClaudeOnly")
	// Two "completed" -> Success.
	assert.Equal(t, 2, out.Success, "Success: got")
	// One "abandoned" + one "errored" -> Failure.
	assert.Equal(t, 2, out.Failure, "Failure: got")
	// One explicit "unknown" -> Unknown.
	assert.Equal(t, 1, out.Unknown, "Unknown: got")
	require.NotNil(t, out.GradeDistribution,
		"GradeDistribution: want non-nil")
	wantGrades := map[string]int{"A": 1, "B": 1, "C": 1, "D": 1}
	assert.Len(t, out.GradeDistribution, len(wantGrades),
		"GradeDistribution size (%+v)", out.GradeDistribution)
	for grade, want := range wantGrades {
		assert.Equal(t, want, out.GradeDistribution[grade],
			"GradeDistribution[%q]", grade)
	}
	assert.NotContains(t, out.GradeDistribution, "",
		"GradeDistribution: empty-string key present (%+v)",
		out.GradeDistribution)
	// ToolRetryRate = (1+0+3+2+1) / (2+4+6+8+5) = 7/25 = 0.28
	assert.InDelta(t, 0.28, out.ToolRetryRate, 1e-9,
		"ToolRetryRate")
	// CompactionsPerSession = (3+1+0+2+4) / 5 = 10/5 = 2.0
	assert.InDelta(t, 2.0, out.CompactionsPerSession, 1e-9,
		"CompactionsPerSession")
	// AvgEditChurn = (5+0+4+6+0) / 5 = 15/5 = 3.0
	assert.InDelta(t, 3.0, out.AvgEditChurn, 1e-9, "AvgEditChurn")
}

// TestGetSessionStats_Outcomes_NoClaude verifies that Outcomes stays
// nil — NOT zero-valued — when the window contains zero Claude
// sessions. A *StatsOutcomes with ClaudeOnly=false would misrepresent
// a pure codex workload as having an outcome signal of all-zeroes.
func TestGetSessionStats_Outcomes_NoClaude(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSessionFixture(t, d, sessionFixture{
		id: "cx1", agent: "codex", userMsgs: 3, startedAt: hoursAgo(2),
	})
	updateSignals(t, d, "cx1", SessionSignalUpdate{
		Outcome:        "completed",
		HealthGrade:    new("A"),
		ToolRetryCount: 5,
	})

	stats, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d"})
	require.NoError(t, err, "GetSessionStats")
	assert.Nil(t, stats.Outcomes, "Outcomes")
}

// TestGetSessionStats_Outcomes_NoGrade verifies that a Claude session
// with an empty health_grade still produces a populated Outcomes
// pointer, with a non-nil empty GradeDistribution map (not nil) and
// zeroed rates when no tools were recorded.
func TestGetSessionStats_Outcomes_NoGrade(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSessionFixture(t, d, sessionFixture{
		id: "ng", userMsgs: 2, startedAt: hoursAgo(2),
	})
	updateSignals(t, d, "ng", SessionSignalUpdate{
		Outcome: "completed",
		// HealthGrade left nil — stored as NULL, loaded as "".
	})

	stats, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d"})
	require.NoError(t, err, "GetSessionStats")
	out := stats.Outcomes
	require.NotNil(t, out, "Outcomes: want populated")
	assert.NotNil(t, out.GradeDistribution,
		"GradeDistribution: want empty map")
	assert.Empty(t, out.GradeDistribution, "GradeDistribution")
	assert.Equal(t, 0.0, out.ToolRetryRate,
		"ToolRetryRate want 0 (no tools)")
	assert.Equal(t, 1, out.Success, "Success: got")
}

// seedToolCallsByName inserts one assistant message per entry in calls and
// a matching tool_calls row with the requested tool_name and skill_name.
// Used by the adoption tests, which key off tool_name (not category) and
// need to populate skill_name for the Skill tool.
func seedToolCallsByName(
	t *testing.T, d *DB, sessionID string, calls []toolCallSeed,
) {
	t.Helper()
	if len(calls) == 0 {
		return
	}
	msgs := make([]Message, 0, len(calls))
	for i, c := range calls {
		msgs = append(msgs, asstMsg(sessionID, i+1, "reply-"+c.toolName))
	}
	require.NoError(t, d.InsertMessages(msgs),
		"seedToolCallsByName %s: InsertMessages", sessionID)
	for i, c := range calls {
		ord := i + 1
		var skill any
		if c.skillName != "" {
			skill = c.skillName
		}
		_, err := d.getWriter().Exec(`
			INSERT INTO tool_calls
				(message_id, session_id, tool_name, category, skill_name)
			SELECT id, session_id, ?, ?, ?
			FROM messages
			WHERE session_id = ? AND ordinal = ?`,
			c.toolName, c.toolName, skill, sessionID, ord,
		)
		require.NoError(t, err,
			"seedToolCallsByName %s: %q", sessionID, c.toolName)
	}
}

// toolCallSeed describes one tool_calls row for seedToolCallsByName.
// skillName is written only for tool_name = "Skill"; other rows leave
// the column NULL.
type toolCallSeed struct {
	toolName  string
	skillName string
}

// TestGetSessionStats_Adoption_Happy seeds four Claude sessions with
// deliberately asymmetric adoption signals and asserts every field on
// StatsAdoption. A fifth codex session must not influence any number
// (adoption is Claude-only).
//
// Rates are hand-computed against the seed:
//   - PlanModeRate: 2 of 4 Claude sessions have >=1 ExitPlanMode -> 0.5
//   - SubagentsPerSession: 3 Task calls across 4 sessions -> 0.75
//   - DistinctSkills: {"brainstorm", "writing-plans", "brainstorm"}
//     -> 2 distinct names
func TestGetSessionStats_Adoption_Happy(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// ad1: one ExitPlanMode, zero Task, one Skill("brainstorm").
	insertSessionFixture(t, d, sessionFixture{
		id: "ad1", agent: "claude", userMsgs: 4,
		startedAt: hoursAgo(6),
	})
	seedToolCallsByName(t, d, "ad1", []toolCallSeed{
		{toolName: "ExitPlanMode"},
		{toolName: "Skill", skillName: "brainstorm"},
	})

	// ad2: zero ExitPlanMode, two Task calls, one Skill("writing-plans").
	insertSessionFixture(t, d, sessionFixture{
		id: "ad2", agent: "claude", userMsgs: 5,
		startedAt: hoursAgo(5),
	})
	seedToolCallsByName(t, d, "ad2", []toolCallSeed{
		{toolName: "Task"},
		{toolName: "Task"},
		{toolName: "Skill", skillName: "writing-plans"},
	})

	// ad3: one ExitPlanMode, one Task, one Skill("brainstorm") —
	// duplicate skill name must collapse in DistinctSkills.
	insertSessionFixture(t, d, sessionFixture{
		id: "ad3", agent: "claude", userMsgs: 3,
		startedAt: hoursAgo(4),
	})
	seedToolCallsByName(t, d, "ad3", []toolCallSeed{
		{toolName: "ExitPlanMode"},
		{toolName: "Task"},
		{toolName: "Skill", skillName: "brainstorm"},
	})

	// ad4: nothing interesting — exercises the denominator.
	insertSessionFixture(t, d, sessionFixture{
		id: "ad4", agent: "claude", userMsgs: 3,
		startedAt: hoursAgo(3),
	})
	seedToolCallsByName(t, d, "ad4", []toolCallSeed{
		{toolName: "Read"},
	})

	// cx1 (codex) with matching tool names — must be excluded.
	insertSessionFixture(t, d, sessionFixture{
		id: "cx1", agent: "codex", userMsgs: 3,
		startedAt: hoursAgo(2),
	})
	seedToolCallsByName(t, d, "cx1", []toolCallSeed{
		{toolName: "ExitPlanMode"},
		{toolName: "Task"},
		{toolName: "Skill", skillName: "codex-only"},
	})

	stats, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d"})
	require.NoError(t, err, "GetSessionStats")
	ad := stats.Adoption
	require.NotNil(t, ad, "Adoption: want populated")
	assert.True(t, ad.ClaudeOnly, "ClaudeOnly")
	// 2 of 4 Claude sessions have ExitPlanMode -> 0.5.
	assert.InDelta(t, 0.5, ad.PlanModeRate, 1e-9, "PlanModeRate")
	assert.GreaterOrEqual(t, ad.PlanModeRate, 0.0,
		"PlanModeRate out of [0,1]")
	assert.LessOrEqual(t, ad.PlanModeRate, 1.0,
		"PlanModeRate out of [0,1]")
	// 3 Task calls across 4 Claude sessions -> 0.75.
	assert.InDelta(t, 0.75, ad.SubagentsPerSession, 1e-9,
		"SubagentsPerSession")
	// {"brainstorm","writing-plans","brainstorm"} -> 2 distinct.
	assert.Equal(t, 2, ad.DistinctSkills, "DistinctSkills: got")
}

// TestGetSessionStats_Adoption_NoClaude verifies that Adoption stays
// nil — NOT zero-valued — when the window has no Claude sessions. A
// *StatsAdoption with ClaudeOnly=false would misrepresent a pure codex
// workload as having legitimate all-zero adoption signal.
func TestGetSessionStats_Adoption_NoClaude(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSessionFixture(t, d, sessionFixture{
		id: "cx1", agent: "codex", userMsgs: 3,
		startedAt: hoursAgo(2),
	})
	seedToolCallsByName(t, d, "cx1", []toolCallSeed{
		{toolName: "ExitPlanMode"},
		{toolName: "Task"},
		{toolName: "Skill", skillName: "brainstorm"},
	})

	stats, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d"})
	require.NoError(t, err, "GetSessionStats")
	assert.Nil(t, stats.Adoption, "Adoption")
}

// skipIfNoGit lets CI environments without git on PATH pass cleanly
// instead of failing the outcome_stats suite. The stats pipeline
// tolerates missing git (computeOutcomeStats silently leaves the field
// nil when the exec fails), but tests that seed a real fixture repo
// require the binary.
func skipIfNoGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not available on PATH: %v", err)
	}
}

// statsRunGit executes a git subcommand inside repo and fails the test
// on error. Kept local to the stats_test file because the git package
// test helpers are unexported.
func statsRunGit(t *testing.T, repo string, env []string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %s\n%s",
		strings.Join(args, " "), out)
}

// statsInitRepo creates a fresh git repo under t.TempDir() with a
// deterministic author identity (test@example.com). Signing is disabled
// so the tests don't hang on a GPG prompt when the host has commit
// signing enabled globally.
func statsInitRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	statsRunGit(t, repo, nil, "init", "-q", "-b", "main")
	statsRunGit(t, repo, nil, "config", "user.email", "test@example.com")
	statsRunGit(t, repo, nil, "config", "user.name", "Test User")
	statsRunGit(t, repo, nil, "config", "commit.gpgsign", "false")
	return repo
}

// statsCommitFile writes content into repo/relpath, stages it, and
// commits as test@example.com with message. Used by outcome_stats tests
// that need a handful of commits with known LOC footprints.
func statsCommitFile(
	t *testing.T, repo, relpath, content, message string,
) {
	t.Helper()
	p := filepath.Join(repo, relpath)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755),
		"mkdir %s", filepath.Dir(p))
	require.NoError(t, os.WriteFile(p, []byte(content), 0o644),
		"write %s", p)
	env := []string{
		"GIT_AUTHOR_NAME=Test User",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test User",
		"GIT_COMMITTER_EMAIL=test@example.com",
	}
	statsRunGit(t, repo, nil, "add", "-A")
	statsRunGit(t, repo, env, "commit", "-q", "-m", message)
}

// TestGetSessionStats_OutcomeStats_DefaultDisabled verifies that plain
// stats runs do not touch git-derived outcome aggregation, even when
// sessions carry cwd values inside a real repository.
func TestGetSessionStats_OutcomeStats_DefaultDisabled(t *testing.T) {
	skipIfNoGit(t)
	d := testDB(t)
	ctx := context.Background()

	repo := statsInitRepo(t)
	statsCommitFile(t, repo, "a.txt", "a1\n", "c1")
	insertSessionFixture(t, d, sessionFixture{
		id: "os-default", agent: "claude", userMsgs: 5,
		startedAt: hoursAgo(5), cwd: repo,
	})

	stats, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d"})
	require.NoError(t, err, "GetSessionStats")
	require.Nil(t, stats.OutcomeStats, "OutcomeStats")
}

// TestGetSessionStats_OutcomeStats_Happy seeds sessions whose cwd
// points inside a real fixture repo and asserts that the outcome_stats
// section surfaces the author-filtered commit totals. PRsOpened /
// PRsMerged must stay nil because no GHToken is supplied — the JSON
// contract distinguishes "gh not configured" (nil) from "gh configured,
// zero PRs" (pointer to 0).
func TestGetSessionStats_OutcomeStats_Happy(t *testing.T) {
	skipIfNoGit(t)
	d := testDB(t)
	ctx := context.Background()

	repo := statsInitRepo(t)
	// Three commits by test@example.com with known LOC counts.
	//   c1 a.txt:      +3 -0 (new file, 3 lines)
	//   c2 a.txt:      +2 -0 (append 2 lines)
	//   c3 b.txt:      +4 -0 (new file, 4 lines)
	statsCommitFile(t, repo, "a.txt", "a1\na2\na3\n", "c1")
	statsCommitFile(t, repo, "a.txt", "a1\na2\na3\na4\na5\n", "c2")
	statsCommitFile(t, repo, "b.txt", "b1\nb2\nb3\nb4\n", "c3")

	// Two Claude sessions with cwds inside the repo — one at the root,
	// one in a subdirectory. Both should collapse to the same repo and
	// counted once in ReposActive.
	sub := filepath.Join(repo, "subdir")
	err := os.MkdirAll(sub, 0o755)
	require.NoError(t, err, "mkdir sub")
	insertSessionFixture(t, d, sessionFixture{
		id: "os1", agent: "claude", userMsgs: 5,
		startedAt: hoursAgo(5), cwd: repo,
	})
	insertSessionFixture(t, d, sessionFixture{
		id: "os2", agent: "claude", userMsgs: 4,
		startedAt: hoursAgo(4), cwd: sub,
	})

	stats, err := d.GetSessionStats(ctx, StatsFilter{
		Since: "28d", IncludeGitOutcomes: true,
	})
	require.NoError(t, err, "GetSessionStats")
	out := stats.OutcomeStats
	require.NotNil(t, out, "OutcomeStats: want populated")
	assert.Equal(t, 1, out.ReposActive, "ReposActive: got")
	assert.Equal(t, 3, out.Commits, "Commits: got")
	assert.Equal(t, 9, out.LOCAdded, "LOCAdded: got")
	assert.Equal(t, 0, out.LOCRemoved, "LOCRemoved: got")
	// Each commit touches one file: c1 a.txt, c2 a.txt, c3 b.txt -> 3.
	assert.Equal(t, 3, out.FilesChanged, "FilesChanged: got")
	assert.Nil(t, out.PRsOpened, "PRsOpened want nil (no GHToken)")
	assert.Nil(t, out.PRsMerged, "PRsMerged want nil (no GHToken)")
}

// TestGetSessionStats_OutcomeStats_NoCwd verifies that sessions without
// a recorded cwd leave OutcomeStats nil — a pure non-git workload must
// not surface a fabricated all-zero outcome row. The JSON contract uses
// omitempty + nil pointer so the section is absent entirely rather than
// serialising as {"repos_active":0,...}.
func TestGetSessionStats_OutcomeStats_NoCwd(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSessionFixture(t, d, sessionFixture{
		id: "nc1", agent: "claude", userMsgs: 5,
		startedAt: hoursAgo(3),
		// cwd intentionally empty
	})

	stats, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d"})
	require.NoError(t, err, "GetSessionStats")
	assert.Nil(t, stats.OutcomeStats, "OutcomeStats")
}

// TestGetSessionStats_OutcomeStats_CwdOutsideRepo verifies that a cwd
// pointing at a non-git directory (no .git anywhere up the tree) is
// treated the same as an empty cwd: DiscoverRepos returns nothing and
// OutcomeStats stays nil. Guards against silently reporting zeros for
// non-git workflows that happen to record a cwd.
func TestGetSessionStats_OutcomeStats_CwdOutsideRepo(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// t.TempDir() is nested under Go's test temp root, which is not
	// itself inside a git repo on any supported platform.
	nonRepo := t.TempDir()
	insertSessionFixture(t, d, sessionFixture{
		id: "nr1", agent: "claude", userMsgs: 5,
		startedAt: hoursAgo(3), cwd: nonRepo,
	})

	stats, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d"})
	require.NoError(t, err, "GetSessionStats")
	assert.Nil(t, stats.OutcomeStats, "OutcomeStats")
}
