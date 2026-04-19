package db

import (
	"context"
	"fmt"
	"testing"
	"time"
)

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
	durationMin      float64
	peakContext      int
	hasPeakContext   bool
	totalOutputTok   int
	isAutomated      bool
	relationshipType string
	// totalToolCalls seeds that many rows in the tool_calls table for
	// this session, each attached to a synthetic assistant message.
	totalToolCalls int
	// assistantTurns seeds that many assistant-role messages for this
	// session. Set alongside totalToolCalls so tests can control the
	// tools_per_turn denominator precisely.
	assistantTurns int
}

// hoursAgo returns an RFC3339 timestamp N hours before now in UTC.
// Used to place fixture rows safely inside the default 28-day window.
func hoursAgo(n int) string {
	return time.Now().UTC().Add(-time.Duration(n) * time.Hour).
		Format(time.RFC3339)
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
		if err != nil {
			t.Fatalf(
				"insertSessionFixture %s: parsing startedAt %q: %v",
				f.id, f.startedAt, err,
			)
		}
		dur := time.Duration(f.durationMin * float64(time.Minute))
		endedAt = start.Add(dur).UTC().Format(time.RFC3339Nano)
	}
	insertSession(t, d, f.id, project, func(s *Session) {
		s.Agent = agent
		s.UserMessageCount = f.userMsgs
		s.MessageCount = mc
		if f.startedAt != "" {
			s.StartedAt = Ptr(f.startedAt)
		}
		if endedAt != "" {
			s.EndedAt = Ptr(endedAt)
		}
		s.PeakContextTokens = f.peakContext
		s.HasPeakContextTokens = f.hasPeakContext
		s.TotalOutputTokens = f.totalOutputTok
		s.IsAutomated = f.isAutomated
		s.RelationshipType = f.relationshipType
	})
	seedAssistantActivity(t, d, f.id, f.assistantTurns, f.totalToolCalls)
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
	if err := d.InsertMessages(msgs); err != nil {
		t.Fatalf("seedAssistantActivity %s: InsertMessages: %v",
			sessionID, err)
	}
	if toolCalls == 0 {
		return
	}
	// Distribute tool_calls round-robin across inserted messages so
	// they all attach to a real message row. Rely on the router-like
	// INSERT ... SELECT ordinal to find the message_id.
	for i := range toolCalls {
		ord := (i % n) + 1
		if _, err := d.getWriter().Exec(`
			INSERT INTO tool_calls
				(message_id, session_id, tool_name, category)
			SELECT id, session_id, 'Read', 'file'
			FROM messages
			WHERE session_id = ? AND ordinal = ?`,
			sessionID, ord,
		); err != nil {
			t.Fatalf("seedAssistantActivity %s: tool_call: %v",
				sessionID, err)
		}
	}
}

// floatsClose reports whether a and b are within eps of each other.
// Used by stats tests that compare arithmetic means.
func floatsClose(a, b, eps float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= eps
}

func TestArchetypeLabel(t *testing.T) {
	cases := []struct {
		userMsgs int
		want     string
	}{
		{0, "automation"},
		{1, "automation"},
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
		got := archetypeLabel(c.userMsgs)
		if got != c.want {
			t.Errorf(
				"archetypeLabel(%d): got %q, want %q",
				c.userMsgs, got, c.want,
			)
		}
	}
}

func TestPickMaxLabel_TiesBreakByPriority(t *testing.T) {
	// automation (2) vs deep (2) — priority says automation wins.
	counts := map[string]int{"automation": 2, "deep": 2, "quick": 1}
	priority := []string{
		"automation", "marathon", "deep", "standard", "quick",
	}
	if got := pickMaxLabel(counts, priority); got != "automation" {
		t.Errorf("tie break: got %q want automation", got)
	}
	// PrimaryHuman excludes automation; marathon should win a 1/1/1
	// tie over deep/standard/quick.
	humanCounts := map[string]int{
		"quick": 1, "standard": 1, "deep": 1, "marathon": 1,
	}
	humanPriority := []string{"marathon", "deep", "standard", "quick"}
	if got := pickMaxLabel(humanCounts, humanPriority); got != "marathon" {
		t.Errorf("human tie break: got %q want marathon", got)
	}
	// Strictly greater wins regardless of priority.
	c2 := map[string]int{"quick": 5, "deep": 2}
	if got := pickMaxLabel(c2, priority); got != "quick" {
		t.Errorf("strict max: got %q want quick", got)
	}
}

func TestGetSessionStats_TotalsAndArchetypes(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// 5 sessions: 2 automation (userMsgs 0,1),
	//             2 deep (userMsgs 20, 40),
	//             1 marathon (userMsgs 100).
	fixtures := []sessionFixture{
		{id: "s1", userMsgs: 0, startedAt: hoursAgo(5)},
		{id: "s2", userMsgs: 1, startedAt: hoursAgo(5)},
		{id: "s3", userMsgs: 20, startedAt: hoursAgo(5)},
		{id: "s4", userMsgs: 40, startedAt: hoursAgo(5)},
		{id: "s5", userMsgs: 100, startedAt: hoursAgo(5)},
	}
	for _, f := range fixtures {
		insertSessionFixture(t, d, f)
	}

	stats, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d"})
	if err != nil {
		t.Fatalf("GetSessionStats: %v", err)
	}

	if stats.SchemaVersion != 1 {
		t.Errorf("schema_version: got %d want 1", stats.SchemaVersion)
	}
	if stats.Totals.SessionsAll != 5 {
		t.Errorf("sessions_all: got %d want 5",
			stats.Totals.SessionsAll)
	}
	if stats.Totals.SessionsAutomation != 2 {
		t.Errorf("sessions_automation: got %d want 2",
			stats.Totals.SessionsAutomation)
	}
	if stats.Totals.SessionsHuman != 3 {
		t.Errorf("sessions_human: got %d want 3",
			stats.Totals.SessionsHuman)
	}
	// Invariant: human + automation must equal all.
	if stats.Totals.SessionsHuman+stats.Totals.SessionsAutomation !=
		stats.Totals.SessionsAll {
		t.Errorf(
			"invariant: human (%d) + automation (%d) != all (%d)",
			stats.Totals.SessionsHuman,
			stats.Totals.SessionsAutomation,
			stats.Totals.SessionsAll,
		)
	}
	if got := stats.Totals.UserMessagesTotal; got != 0+1+20+40+100 {
		t.Errorf("user_messages_total: got %d want 161", got)
	}

	if stats.Archetypes.Automation != 2 {
		t.Errorf("archetypes.automation: got %d want 2",
			stats.Archetypes.Automation)
	}
	if stats.Archetypes.Quick != 0 {
		t.Errorf("archetypes.quick: got %d want 0",
			stats.Archetypes.Quick)
	}
	if stats.Archetypes.Standard != 0 {
		t.Errorf("archetypes.standard: got %d want 0",
			stats.Archetypes.Standard)
	}
	if stats.Archetypes.Deep != 2 {
		t.Errorf("archetypes.deep: got %d want 2",
			stats.Archetypes.Deep)
	}
	if stats.Archetypes.Marathon != 1 {
		t.Errorf("archetypes.marathon: got %d want 1",
			stats.Archetypes.Marathon)
	}
	// 2 automation, 2 deep — tie broken by priority: automation first.
	if stats.Archetypes.Primary != "automation" {
		t.Errorf("archetypes.primary: got %q want automation",
			stats.Archetypes.Primary)
	}
	// Human subset: 2 deep, 1 marathon. Deep wins.
	if stats.Archetypes.PrimaryHuman != "deep" {
		t.Errorf("archetypes.primary_human: got %q want deep",
			stats.Archetypes.PrimaryHuman)
	}

	// Window bookkeeping: Since = now-28d, Until = now, days = 28.
	if stats.Window.Days != 28 {
		t.Errorf("window.days: got %d want 28", stats.Window.Days)
	}
	if stats.Window.Since == "" || stats.Window.Until == "" {
		t.Errorf("window bounds empty: since=%q until=%q",
			stats.Window.Since, stats.Window.Until)
	}
	if _, err := time.Parse(time.RFC3339, stats.Window.Since); err != nil {
		t.Errorf("window.since not RFC3339: %v", err)
	}
	if _, err := time.Parse(time.RFC3339, stats.Window.Until); err != nil {
		t.Errorf("window.until not RFC3339: %v", err)
	}

	// Filters echo the inputs and default Agent to "all".
	if stats.Filters.Agent != "all" {
		t.Errorf("filters.agent: got %q want all", stats.Filters.Agent)
	}
	if stats.Filters.Timezone != "UTC" {
		t.Errorf("filters.timezone: got %q want UTC",
			stats.Filters.Timezone)
	}
	if stats.Filters.ProjectsExcluded == nil {
		t.Errorf("filters.projects_excluded must be non-nil slice")
	}

	if stats.GeneratedAt == "" {
		t.Errorf("generated_at empty")
	}
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
	if err != nil {
		t.Fatalf("GetSessionStats all: %v", err)
	}
	if all.Totals.SessionsAll != 2 {
		t.Errorf("all agents: got %d want 2",
			all.Totals.SessionsAll)
	}

	onlyClaude, err := d.GetSessionStats(
		ctx, StatsFilter{Since: "28d", Agent: "claude"},
	)
	if err != nil {
		t.Fatalf("GetSessionStats claude: %v", err)
	}
	if onlyClaude.Totals.SessionsAll != 1 {
		t.Errorf("agent=claude: got %d want 1",
			onlyClaude.Totals.SessionsAll)
	}
	if onlyClaude.Filters.Agent != "claude" {
		t.Errorf("agent filter echoed: got %q want claude",
			onlyClaude.Filters.Agent)
	}
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
	if err != nil {
		t.Fatalf("include alpha: %v", err)
	}
	if includeAlpha.Totals.SessionsAll != 2 {
		t.Errorf("include=alpha: got %d want 2",
			includeAlpha.Totals.SessionsAll)
	}

	excludeAlpha, err := d.GetSessionStats(ctx, StatsFilter{
		Since:           "28d",
		ExcludeProjects: []string{"alpha"},
	})
	if err != nil {
		t.Fatalf("exclude alpha: %v", err)
	}
	if excludeAlpha.Totals.SessionsAll != 2 {
		t.Errorf("exclude=alpha: got %d want 2 (beta + gamma)",
			excludeAlpha.Totals.SessionsAll)
	}
}

func TestWindowBounds(t *testing.T) {
	// Fixed reference time so the tests are deterministic.
	now := time.Date(2026, 4, 18, 12, 0, 0, 0, time.UTC)

	t.Run("default 28d", func(t *testing.T) {
		from, to, days, err := windowBounds(StatsFilter{}, now)
		if err != nil {
			t.Fatalf("windowBounds: %v", err)
		}
		if days != 28 {
			t.Errorf("days: got %d want 28", days)
		}
		if !to.Equal(now) {
			t.Errorf("until: got %v want %v", to, now)
		}
		wantFrom := now.Add(-28 * 24 * time.Hour)
		if !from.Equal(wantFrom) {
			t.Errorf("since: got %v want %v", from, wantFrom)
		}
	})

	t.Run("Nd duration", func(t *testing.T) {
		_, _, days, err := windowBounds(
			StatsFilter{Since: "7d"}, now,
		)
		if err != nil {
			t.Fatalf("windowBounds: %v", err)
		}
		if days != 7 {
			t.Errorf("days: got %d want 7", days)
		}
	})

	t.Run("Nh duration", func(t *testing.T) {
		from, to, _, err := windowBounds(
			StatsFilter{Since: "48h"}, now,
		)
		if err != nil {
			t.Fatalf("windowBounds: %v", err)
		}
		if got := to.Sub(from); got != 48*time.Hour {
			t.Errorf("span: got %v want 48h", got)
		}
	})

	t.Run("bare date", func(t *testing.T) {
		from, _, _, err := windowBounds(
			StatsFilter{Since: "2026-04-01"}, now,
		)
		if err != nil {
			t.Fatalf("windowBounds: %v", err)
		}
		if from.Year() != 2026 || from.Month() != 4 || from.Day() != 1 {
			t.Errorf("since parsed: got %v want 2026-04-01", from)
		}
	})

	t.Run("invalid since", func(t *testing.T) {
		if _, _, _, err := windowBounds(
			StatsFilter{Since: "bogus"}, now,
		); err == nil {
			t.Error("expected error for invalid Since")
		}
	})
}

func TestGetSessionStats_Distributions(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Five sessions chosen to place one row in each interesting bucket
	// for duration and peak_context. userMsgs drives archetype/scope:
	// a,b → automation (userMsgs <= 1); c,d,e → human.
	fixtures := []struct {
		id             string
		userMsgs       int
		peakCtx        int
		durMin         float64
		toolCalls      int
		assistantTurns int
	}{
		{"a", 0, 2_000, 0.5, 0, 0},
		{"b", 1, 8_000, 0.9, 1, 1},
		{"c", 3, 25_000, 10.0, 6, 3},
		{"d", 10, 60_000, 25.0, 15, 10},
		{"e", 30, 150_000, 120.0, 30, 30},
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
		})
	}

	stats, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d"})
	if err != nil {
		t.Fatalf("GetSessionStats: %v", err)
	}

	// duration scope_all: 0.5→bucket0, 0.9→bucket0, 10→bucket2,
	// 25→bucket3, 120→bucket5 (top).
	gotAll := stats.Distributions.DurationMinutes.ScopeAll.Buckets
	wantCountsAll := []int{2, 0, 1, 1, 0, 1}
	if len(gotAll) != len(wantCountsAll) {
		t.Fatalf("duration scope_all: got %d buckets, want %d",
			len(gotAll), len(wantCountsAll))
	}
	for i, w := range wantCountsAll {
		if gotAll[i].Count != w {
			t.Errorf("duration scope_all bucket %d: got %d want %d",
				i, gotAll[i].Count, w)
		}
	}
	// duration scope_human (c,d,e): bucket2=1, bucket3=1, bucket5=1.
	gotHuman := stats.Distributions.DurationMinutes.ScopeHuman.Buckets
	wantCountsHuman := []int{0, 0, 1, 1, 0, 1}
	if len(gotHuman) != len(wantCountsHuman) {
		t.Fatalf("duration scope_human: got %d buckets, want %d",
			len(gotHuman), len(wantCountsHuman))
	}
	for i, w := range wantCountsHuman {
		if gotHuman[i].Count != w {
			t.Errorf("duration scope_human bucket %d: got %d want %d",
				i, gotHuman[i].Count, w)
		}
	}

	// Means (arithmetic over included sessions).
	wantAllMean := (0.5 + 0.9 + 10 + 25 + 120) / 5.0
	gotAllMean := stats.Distributions.DurationMinutes.ScopeAll.Mean
	if !floatsClose(gotAllMean, wantAllMean, 0.01) {
		t.Errorf("duration scope_all mean: got %v want %v",
			gotAllMean, wantAllMean)
	}
	wantHumanMean := (10.0 + 25.0 + 120.0) / 3.0
	gotHumanMean := stats.Distributions.DurationMinutes.ScopeHuman.Mean
	if !floatsClose(gotHumanMean, wantHumanMean, 0.01) {
		t.Errorf("duration scope_human mean: got %v want %v",
			gotHumanMean, wantHumanMean)
	}

	// user_messages scope_all uses userMessagesEdgesAll
	// ([0,2),[2,6),[6,16),[16,31),[31,51),[51,inf)):
	// 0→0, 1→0, 3→1, 10→2, 30→3.
	gotUM := stats.Distributions.UserMessages.ScopeAll.Buckets
	wantUM := []int{2, 1, 1, 1, 0, 0}
	if len(gotUM) != len(wantUM) {
		t.Fatalf("user_messages scope_all: got %d buckets, want %d",
			len(gotUM), len(wantUM))
	}
	for i, w := range wantUM {
		if gotUM[i].Count != w {
			t.Errorf("user_messages scope_all bucket %d: got %d want %d",
				i, gotUM[i].Count, w)
		}
	}
	// user_messages scope_human uses userMessagesEdgesHuman (5 buckets,
	// dropping the automation band): 3→0, 10→1, 30→2.
	gotUMH := stats.Distributions.UserMessages.ScopeHuman.Buckets
	wantUMH := []int{1, 1, 1, 0, 0}
	if len(gotUMH) != len(wantUMH) {
		t.Fatalf("user_messages scope_human: got %d buckets, want %d",
			len(gotUMH), len(wantUMH))
	}
	for i, w := range wantUMH {
		if gotUMH[i].Count != w {
			t.Errorf("user_messages scope_human bucket %d: got %d want %d",
				i, gotUMH[i].Count, w)
		}
	}

	// peak_context scope_all: 2k→0, 8k→0, 25k→1, 60k→2, 150k→4.
	gotPCAll := stats.Distributions.PeakContextTokens.ScopeAll.Buckets
	wantPCAll := []int{2, 1, 1, 0, 1, 0}
	for i, w := range wantPCAll {
		if gotPCAll[i].Count != w {
			t.Errorf("peak_context scope_all bucket %d: got %d want %d",
				i, gotPCAll[i].Count, w)
		}
	}
	// peak_context scope_human (c,d,e): 25k→1, 60k→2, 150k→4.
	gotPC := stats.Distributions.PeakContextTokens.ScopeHuman.Buckets
	if gotPC[1].Count != 1 || gotPC[2].Count != 1 || gotPC[4].Count != 1 {
		t.Errorf("peak_context scope_human: %+v", gotPC)
	}
	if !stats.Distributions.PeakContextTokens.ClaudeOnly {
		t.Errorf("peak_context.claude_only: got false want true")
	}
	if stats.Distributions.PeakContextTokens.NullCount != 0 {
		t.Errorf("peak_context.null_count: got %d want 0",
			stats.Distributions.PeakContextTokens.NullCount)
	}

	// tools_per_turn: a=0/1=0, b=1/1=1, c=6/3=2, d=15/10=1.5, e=30/30=1.
	// toolsPerTurnEdges = [0,1,2,4,7,11,+Inf].
	gotTPT := stats.Distributions.ToolsPerTurn.ScopeAll.Buckets
	wantTPT := []int{1, 3, 1, 0, 0, 0}
	if len(gotTPT) != len(wantTPT) {
		t.Fatalf("tools_per_turn scope_all: got %d buckets, want %d",
			len(gotTPT), len(wantTPT))
	}
	for i, w := range wantTPT {
		if gotTPT[i].Count != w {
			t.Errorf("tools_per_turn scope_all bucket %d: got %d want %d",
				i, gotTPT[i].Count, w)
		}
	}
}

func TestGetSessionStats_Distributions_NullPeakContext(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// One session lacks peak-context data; it must land in NullCount
	// rather than any peak_context bucket (including bucket 0).
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

	stats, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d"})
	if err != nil {
		t.Fatalf("GetSessionStats: %v", err)
	}

	pc := stats.Distributions.PeakContextTokens
	if pc.NullCount != 1 {
		t.Errorf("null_count: got %d want 1", pc.NullCount)
	}
	total := 0
	for _, b := range pc.ScopeAll.Buckets {
		total += b.Count
	}
	if total != 1 {
		t.Errorf("scope_all bucket total: got %d want 1 "+
			"(the one session with hasPeakContext=true)", total)
	}
}
