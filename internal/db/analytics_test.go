package db

import (
	"context"
	"fmt"
	"testing"
	"time"
)

type seedStats struct {
	TotalSessions          int
	TotalMessages          int
	TotalUserMessages      int
	TotalAssistantMessages int
	ActiveProjects         int
	ActiveDays             int
}

func seedAnalyticsData(t *testing.T, d *DB) seedStats {
	t.Helper()

	type sessionData struct {
		id      string
		project string
		start   string
		end     string
		msgs    int
		agent   string
	}

	sessions := []sessionData{
		// Project A: 3 sessions across 2 days, mixed agents
		{"a1", "project-alpha", "2024-06-01T09:00:00Z", tsMidYear, 10, "claude"},
		{"a2", "project-alpha", "2024-06-01T14:00:00Z", "2024-06-01T15:00:00Z", 20, "codex"},
		{"a3", "project-alpha", "2024-06-03T09:00:00Z", "2024-06-03T10:00:00Z", 5, "claude"},
		// Project B: 2 sessions on 1 day
		{"b1", "project-beta", "2024-06-02T10:00:00Z", "2024-06-02T11:00:00Z", 30, "claude"},
		{"b2", "project-beta", "2024-06-02T15:00:00Z", "2024-06-02T16:00:00Z", 15, "claude"},
	}

	stats := seedStats{}
	projects := make(map[string]bool)
	days := make(map[string]bool)

	for _, sess := range sessions {
		stats.TotalSessions++
		stats.TotalMessages += sess.msgs
		for i := 0; i < sess.msgs; i++ {
			if i%2 == 1 {
				stats.TotalAssistantMessages++
			} else {
				stats.TotalUserMessages++
			}
		}

		projects[sess.project] = true
		if len(sess.start) >= 10 {
			days[sess.start[:10]] = true
		}

		insertSession(t, d, sess.id, sess.project, func(s *Session) {
			s.StartedAt = new(sess.start)
			s.EndedAt = new(sess.end)
			s.MessageCount = sess.msgs
			s.Agent = sess.agent
		})

		msgs := make([]Message, sess.msgs)
		for i := 0; i < sess.msgs; i++ {
			role := "user"
			if i%2 == 1 {
				role = "assistant"
			}
			msgs[i] = Message{
				SessionID:     sess.id,
				Ordinal:       i,
				Role:          role,
				Content:       fmt.Sprintf("msg %d", i),
				ContentLength: 5,
				Timestamp:     tsMidYear,
			}
		}
		insertMessages(t, d, msgs...)
	}

	stats.ActiveProjects = len(projects)
	stats.ActiveDays = len(days)

	return stats
}

func baseFilter() AnalyticsFilter {
	return AnalyticsFilter{
		From:     "2024-06-01",
		To:       "2024-06-03",
		Timezone: "UTC",
	}
}

func emptyFilter() AnalyticsFilter {
	return AnalyticsFilter{
		From:     "2020-01-01",
		To:       "2020-01-02",
		Timezone: "UTC",
	}
}

func mustSummary(
	t *testing.T, d *DB, ctx context.Context, f AnalyticsFilter,
) AnalyticsSummary {
	t.Helper()
	s, err := d.GetAnalyticsSummary(ctx, f)
	if err != nil {
		t.Fatalf("GetAnalyticsSummary: %v", err)
	}
	return s
}

func mustActivity(
	t *testing.T, d *DB, ctx context.Context,
	f AnalyticsFilter, gran string,
) ActivityResponse {
	t.Helper()
	r, err := d.GetAnalyticsActivity(ctx, f, gran)
	if err != nil {
		t.Fatalf("GetAnalyticsActivity: %v", err)
	}
	return r
}

func mustHeatmap(
	t *testing.T, d *DB, ctx context.Context,
	f AnalyticsFilter, metric string,
) HeatmapResponse {
	t.Helper()
	r, err := d.GetAnalyticsHeatmap(ctx, f, metric)
	if err != nil {
		t.Fatalf("GetAnalyticsHeatmap: %v", err)
	}
	return r
}

func mustProjects(
	t *testing.T, d *DB, ctx context.Context,
	f AnalyticsFilter,
) ProjectsAnalyticsResponse {
	t.Helper()
	r, err := d.GetAnalyticsProjects(ctx, f)
	if err != nil {
		t.Fatalf("GetAnalyticsProjects: %v", err)
	}
	return r
}

func TestGetAnalyticsSummary(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	t.Run("EmptyDB", func(t *testing.T) {
		s := mustSummary(t, d, ctx, baseFilter())
		if s.TotalSessions != 0 {
			t.Errorf("TotalSessions = %d, want 0", s.TotalSessions)
		}
	})

	stats := seedAnalyticsData(t, d)

	t.Run("FullRange", func(t *testing.T) {
		s := mustSummary(t, d, ctx, baseFilter())
		if s.TotalSessions != stats.TotalSessions {
			t.Errorf("TotalSessions = %d, want %d", s.TotalSessions, stats.TotalSessions)
		}
		if s.TotalMessages != stats.TotalMessages {
			t.Errorf("TotalMessages = %d, want %d", s.TotalMessages, stats.TotalMessages)
		}
		if s.ActiveProjects != stats.ActiveProjects {
			t.Errorf("ActiveProjects = %d, want %d", s.ActiveProjects, stats.ActiveProjects)
		}
		if s.ActiveDays != stats.ActiveDays {
			t.Errorf("ActiveDays = %d, want %d", s.ActiveDays, stats.ActiveDays)
		}
		if s.MostActive != "project-beta" {
			t.Errorf("MostActive = %q, want project-beta", s.MostActive)
		}
		// 2 projects, both in top 3 → concentration = 1.0
		if s.Concentration != 1.0 {
			t.Errorf("Concentration = %f, want 1.0", s.Concentration)
		}

		// Sorted message counts: [5, 10, 15, 20, 30]
		if s.MedianMessages != 15 {
			t.Errorf("MedianMessages = %d, want 15", s.MedianMessages)
		}
		// P90 index = int(5*0.9) = 4 → value 30
		if s.P90Messages != 30 {
			t.Errorf("P90Messages = %d, want 30", s.P90Messages)
		}

		if s.Agents["claude"] == nil {
			t.Fatal("expected claude agent entry")
		}
		if s.Agents["claude"].Sessions != 4 {
			t.Errorf("claude sessions = %d, want 4",
				s.Agents["claude"].Sessions)
		}
		if s.Agents["codex"] == nil {
			t.Fatal("expected codex agent entry")
		}
		if s.Agents["codex"].Sessions != 1 {
			t.Errorf("codex sessions = %d, want 1",
				s.Agents["codex"].Sessions)
		}
	})

	t.Run("DateSubset", func(t *testing.T) {
		f := AnalyticsFilter{
			From:     "2024-06-01",
			To:       "2024-06-01",
			Timezone: "UTC",
		}
		s := mustSummary(t, d, ctx, f)
		if s.TotalSessions != 2 {
			t.Errorf("TotalSessions = %d, want 2", s.TotalSessions)
		}
	})

	t.Run("MachineFilter", func(t *testing.T) {
		f := baseFilter()
		f.Machine = "nonexistent"
		s := mustSummary(t, d, ctx, f)
		if s.TotalSessions != 0 {
			t.Errorf("TotalSessions = %d, want 0", s.TotalSessions)
		}
	})

	t.Run("EmptyDateRange", func(t *testing.T) {
		s := mustSummary(t, d, ctx, emptyFilter())
		if s.TotalSessions != 0 {
			t.Errorf("TotalSessions = %d, want 0", s.TotalSessions)
		}
	})
}

func TestAnalyticsFilterMachineMultiSelect(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	for _, sess := range []struct {
		id      string
		machine string
	}{
		{"machine-a", "laptop"},
		{"machine-b", "server"},
		{"machine-c", "desktop"},
	} {
		insertSession(t, d, sess.id, "project", func(s *Session) {
			s.Machine = sess.machine
			s.StartedAt = new("2024-06-01T09:00:00Z")
			s.EndedAt = new("2024-06-01T10:00:00Z")
			s.MessageCount = 4
		})
	}

	f := baseFilter()
	f.Machine = "laptop,server"
	s := mustSummary(t, d, ctx, f)
	if s.TotalSessions != 2 {
		t.Fatalf("TotalSessions = %d, want 2", s.TotalSessions)
	}
}

func TestGetAnalyticsActivity(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	stats := seedAnalyticsData(t, d)

	t.Run("DayGranularity", func(t *testing.T) {
		resp := mustActivity(t, d, ctx, baseFilter(), "day")
		if resp.Granularity != "day" {
			t.Errorf("Granularity = %q, want day", resp.Granularity)
		}
		if len(resp.Series) != stats.ActiveDays {
			t.Fatalf("len(Series) = %d, want %d", len(resp.Series), stats.ActiveDays)
		}
		// Day 1: 2 sessions (a1, a2)
		if resp.Series[0].Sessions != 2 {
			t.Errorf("Day1 sessions = %d, want 2",
				resp.Series[0].Sessions)
		}
	})

	t.Run("WeekGranularity", func(t *testing.T) {
		resp := mustActivity(t, d, ctx, baseFilter(), "week")
		// 2024-06-01 is Saturday, 2024-06-03 is Monday
		// So we expect 2 weeks: week of May 27 and week of Jun 3
		if len(resp.Series) != 2 {
			t.Errorf("len(Series) = %d, want 2", len(resp.Series))
		}
	})

	t.Run("MonthGranularity", func(t *testing.T) {
		resp := mustActivity(t, d, ctx, baseFilter(), "month")
		if len(resp.Series) != 1 {
			t.Errorf("len(Series) = %d, want 1", len(resp.Series))
		}
		if resp.Series[0].Sessions != stats.TotalSessions {
			t.Errorf("month sessions = %d, want %d", resp.Series[0].Sessions, stats.TotalSessions)
		}
	})

	t.Run("HasRoleCounts", func(t *testing.T) {
		resp := mustActivity(t, d, ctx, baseFilter(), "day")
		totalUser := 0
		totalAsst := 0
		for _, e := range resp.Series {
			totalUser += e.UserMessages
			totalAsst += e.AssistantMessages
		}
		if totalUser+totalAsst != stats.TotalMessages {
			t.Errorf("total messages = %d, want %d", totalUser+totalAsst, stats.TotalMessages)
		}
		if totalUser != stats.TotalUserMessages {
			t.Errorf("total user messages = %d, want %d", totalUser, stats.TotalUserMessages)
		}
		if totalAsst != stats.TotalAssistantMessages {
			t.Errorf("total assistant messages = %d, want %d", totalAsst, stats.TotalAssistantMessages)
		}
	})
}

func TestGetAnalyticsHeatmap(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	stats := seedAnalyticsData(t, d)

	t.Run("MessageMetric", func(t *testing.T) {
		resp := mustHeatmap(t, d, ctx, baseFilter(), "messages")
		if resp.Metric != "messages" {
			t.Errorf("Metric = %q, want messages", resp.Metric)
		}
		// 3 days in range: Jun 1, 2, 3
		if len(resp.Entries) != stats.ActiveDays {
			t.Fatalf("len(Entries) = %d, want %d", len(resp.Entries), stats.ActiveDays)
		}

		totalMessages := 0
		for _, e := range resp.Entries {
			totalMessages += e.Value
		}
		if totalMessages != stats.TotalMessages {
			t.Errorf("total messages across heatmap = %d, want %d", totalMessages, stats.TotalMessages)
		}

		// Jun 1: 10+20=30, Jun 2: 30+15=45, Jun 3: 5
		if resp.Entries[0].Value != 30 {
			t.Errorf("Jun1 value = %d, want 30", resp.Entries[0].Value)
		}
		if resp.Entries[1].Value != 45 {
			t.Errorf("Jun2 value = %d, want 45", resp.Entries[1].Value)
		}
		if resp.Entries[2].Value != 5 {
			t.Errorf("Jun3 value = %d, want 5", resp.Entries[2].Value)
		}
	})

	t.Run("SessionMetric", func(t *testing.T) {
		resp := mustHeatmap(t, d, ctx, baseFilter(), "sessions")
		if resp.Metric != "sessions" {
			t.Errorf("Metric = %q, want sessions", resp.Metric)
		}

		totalSessions := 0
		for _, e := range resp.Entries {
			totalSessions += e.Value
		}
		if totalSessions != stats.TotalSessions {
			t.Errorf("total sessions across heatmap = %d, want %d", totalSessions, stats.TotalSessions)
		}

		// Jun 1: 2, Jun 2: 2, Jun 3: 1
		if resp.Entries[0].Value != 2 {
			t.Errorf("Jun1 sessions = %d, want 2",
				resp.Entries[0].Value)
		}
	})

	t.Run("LevelsAssigned", func(t *testing.T) {
		resp := mustHeatmap(t, d, ctx, baseFilter(), "messages")
		// All entries should have levels 0-4
		for _, e := range resp.Entries {
			if e.Level < 0 || e.Level > 4 {
				t.Errorf("date %s level = %d, want 0-4",
					e.Date, e.Level)
			}
		}
	})

	t.Run("OutputTokensNoReporting", func(t *testing.T) {
		// When no sessions report token coverage, the
		// output_tokens heatmap must return empty entries
		// rather than a zero-filled date grid.
		resp := mustHeatmap(
			t, d, ctx, baseFilter(), "output_tokens",
		)
		if resp.Metric != "output_tokens" {
			t.Errorf(
				"Metric = %q, want output_tokens",
				resp.Metric,
			)
		}
		if len(resp.Entries) != 0 {
			t.Errorf(
				"len(Entries) = %d, want 0 "+
					"(no sessions report token coverage)",
				len(resp.Entries),
			)
		}
	})

	t.Run("EmptyRange", func(t *testing.T) {
		f := emptyFilter()
		f.To = "2020-01-03"
		resp := mustHeatmap(t, d, ctx, f, "messages")
		if len(resp.Entries) != 3 {
			t.Fatalf("len(Entries) = %d, want 3", len(resp.Entries))
		}
		for _, e := range resp.Entries {
			if e.Value != 0 {
				t.Errorf("date %s value = %d, want 0", e.Date, e.Value)
			}
			if e.Level != 0 {
				t.Errorf("date %s level = %d, want 0", e.Date, e.Level)
			}
		}
	})
}

func TestGetAnalyticsProjects(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	stats := seedAnalyticsData(t, d)

	t.Run("FullRange", func(t *testing.T) {
		resp := mustProjects(t, d, ctx, baseFilter())
		if len(resp.Projects) != stats.ActiveProjects {
			t.Fatalf("len(Projects) = %d, want %d", len(resp.Projects), stats.ActiveProjects)
		}

		totalMessages := 0
		for _, p := range resp.Projects {
			totalMessages += p.Messages
		}
		if totalMessages != stats.TotalMessages {
			t.Errorf("total messages across projects = %d, want %d", totalMessages, stats.TotalMessages)
		}

		// Sorted by message count desc: beta (45) > alpha (35)
		if resp.Projects[0].Name != "project-beta" {
			t.Errorf("first project = %q, want project-beta",
				resp.Projects[0].Name)
		}
		if resp.Projects[0].Messages != 45 {
			t.Errorf("beta messages = %d, want 45",
				resp.Projects[0].Messages)
		}
		if resp.Projects[1].Name != "project-alpha" {
			t.Errorf("second project = %q, want project-alpha",
				resp.Projects[1].Name)
		}
		if resp.Projects[1].Sessions != 3 {
			t.Errorf("alpha sessions = %d, want 3",
				resp.Projects[1].Sessions)
		}
	})

	t.Run("AgentBreakdown", func(t *testing.T) {
		resp := mustProjects(t, d, ctx, baseFilter())
		alpha := resp.Projects[1]
		if alpha.Agents["claude"] != 2 {
			t.Errorf("alpha claude = %d, want 2",
				alpha.Agents["claude"])
		}
		if alpha.Agents["codex"] != 1 {
			t.Errorf("alpha codex = %d, want 1",
				alpha.Agents["codex"])
		}
	})

	t.Run("MedianMessages", func(t *testing.T) {
		resp := mustProjects(t, d, ctx, baseFilter())
		// Alpha counts sorted: [5, 10, 20], median = 10
		alpha := resp.Projects[1]
		if alpha.MedianMessages != 10 {
			t.Errorf("alpha median = %d, want 10",
				alpha.MedianMessages)
		}
	})

	t.Run("EmptyRange", func(t *testing.T) {
		resp := mustProjects(t, d, ctx, emptyFilter())
		if len(resp.Projects) != 0 {
			t.Errorf("len(Projects) = %d, want 0", len(resp.Projects))
		}
	})
}

func TestMedianInt(t *testing.T) {
	tests := []struct {
		name   string
		sorted []int
		want   int
	}{
		{"Empty", []int{}, 0},
		{"Single", []int{5}, 5},
		{"OddCount", []int{1, 3, 7}, 3},
		{"EvenCount", []int{1, 3, 7, 9}, 5},
		{"EvenCountTwo", []int{10, 20}, 15},
		{"EvenCountFour", []int{2, 4, 6, 8}, 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := medianInt(tt.sorted, len(tt.sorted))
			if got != tt.want {
				t.Errorf(
					"medianInt(%v) = %d, want %d",
					tt.sorted, got, tt.want,
				)
			}
		})
	}
}

func TestLocalDate(t *testing.T) {
	utc := time.UTC

	tests := []struct {
		name string
		ts   string
		want string
	}{
		{"RFC3339", "2024-06-01T15:00:00Z", "2024-06-01"},
		{"RFC3339Nano", "2024-06-01T15:00:00.123Z", "2024-06-01"},
		{"NoFraction", "2024-06-01T15:00:00Z", "2024-06-01"},
		{"Fallback10Char", "2024-06-01", "2024-06-01"},
		{"Short", "2024", ""},
		{"Empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := localDate(tt.ts, utc)
			if got != tt.want {
				t.Errorf(
					"localDate(%q) = %q, want %q",
					tt.ts, got, tt.want,
				)
			}
		})
	}
}

func TestMostActiveTieBreak(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Two projects with equal message counts
	insertSession(t, d, "t1", "zebra", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.MessageCount = 20
		s.Agent = "claude"
	})
	insertSession(t, d, "t2", "alpha", func(s *Session) {
		s.StartedAt = new(tsMidYear)
		s.MessageCount = 20
		s.Agent = "claude"
	})

	f := AnalyticsFilter{
		From:     "2024-06-01",
		To:       "2024-06-01",
		Timezone: "UTC",
	}
	s := mustSummary(t, d, ctx, f)

	// Alphabetically, "alpha" < "zebra"
	if s.MostActive != "alpha" {
		t.Errorf(
			"MostActive = %q, want alphabetically first (alpha)",
			s.MostActive,
		)
	}
}

func TestEvenCountMedianInSummary(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// 4 sessions: message counts [5, 10, 20, 30]
	for i, mc := range []int{10, 30, 5, 20} {
		id := fmt.Sprintf("e%d", i)
		insertSession(t, d, id, "proj", func(s *Session) {
			ts := fmt.Sprintf("2024-06-01T%02d:00:00Z", i+9)
			s.StartedAt = &ts
			s.MessageCount = mc
			s.Agent = "claude"
		})
	}

	f := AnalyticsFilter{
		From:     "2024-06-01",
		To:       "2024-06-01",
		Timezone: "UTC",
	}
	s := mustSummary(t, d, ctx, f)

	// Sorted: [5, 10, 20, 30] → median = (10+20)/2 = 15
	if s.MedianMessages != 15 {
		t.Errorf(
			"MedianMessages = %d, want 15", s.MedianMessages,
		)
	}
}

func TestAnalyticsTimezone(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Session at 2024-06-01T23:00:00Z = 2024-06-02 in UTC+5
	insertSession(t, d, "tz1", "tz-project", func(s *Session) {
		s.StartedAt = new("2024-06-01T23:00:00Z")
		s.MessageCount = 10
		s.Agent = "claude"
	})
	insertMessages(t, d, userMsg("tz1", 0, "late night"))

	t.Run("UTCBucket", func(t *testing.T) {
		f := AnalyticsFilter{
			From:     "2024-06-01",
			To:       "2024-06-02",
			Timezone: "UTC",
		}
		resp := mustHeatmap(t, d, ctx, f, "messages")
		// In UTC, this is Jun 1
		if resp.Entries[0].Value != 10 {
			t.Errorf("Jun1 UTC value = %d, want 10",
				resp.Entries[0].Value)
		}
		if resp.Entries[1].Value != 0 {
			t.Errorf("Jun2 UTC value = %d, want 0",
				resp.Entries[1].Value)
		}
	})

	t.Run("PlusFiveBucket", func(t *testing.T) {
		f := AnalyticsFilter{
			From:     "2024-06-01",
			To:       "2024-06-02",
			Timezone: "Asia/Karachi", // UTC+5
		}
		resp := mustHeatmap(t, d, ctx, f, "messages")
		// In UTC+5, 23:00Z = 04:00 Jun 2
		if resp.Entries[0].Value != 0 {
			t.Errorf("Jun1 PKT value = %d, want 0",
				resp.Entries[0].Value)
		}
		if resp.Entries[1].Value != 10 {
			t.Errorf("Jun2 PKT value = %d, want 10",
				resp.Entries[1].Value)
		}
	})
}

func TestAnalyticsCanceledContext(t *testing.T) {
	d := testDB(t)
	ctx := canceledCtx()

	f := baseFilter()

	tests := []struct {
		name string
		fn   func() error
	}{
		{"Summary", func() error {
			_, err := d.GetAnalyticsSummary(ctx, f)
			return err
		}},
		{"Activity", func() error {
			_, err := d.GetAnalyticsActivity(ctx, f, "day")
			return err
		}},
		{"Heatmap", func() error {
			_, err := d.GetAnalyticsHeatmap(ctx, f, "messages")
			return err
		}},
		{"Projects", func() error {
			_, err := d.GetAnalyticsProjects(ctx, f)
			return err
		}},
		{"HourOfWeek", func() error {
			_, err := d.GetAnalyticsHourOfWeek(ctx, f)
			return err
		}},
		{"SessionShape", func() error {
			_, err := d.GetAnalyticsSessionShape(ctx, f)
			return err
		}},
		{"Velocity", func() error {
			_, err := d.GetAnalyticsVelocity(ctx, f)
			return err
		}},
		{"Tools", func() error {
			_, err := d.GetAnalyticsTools(ctx, f)
			return err
		}},
		{"TopSessions", func() error {
			_, err := d.GetAnalyticsTopSessions(
				ctx, f, "messages",
			)
			return err
		}},
		{"Signals", func() error {
			_, err := d.GetAnalyticsSignals(ctx, f)
			return err
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requireCanceledErr(t, tt.fn())
		})
	}
}

func TestConcentrationTopThree(t *testing.T) {
	ctx := context.Background()

	t.Run("OneProject", func(t *testing.T) {
		d := testDB(t)
		insertSession(t, d, "c1", "solo", func(s *Session) {
			s.StartedAt = new("2024-06-01T09:00:00Z")
			s.MessageCount = 50
			s.Agent = "claude"
		})
		f := AnalyticsFilter{
			From:     "2024-06-01",
			To:       "2024-06-01",
			Timezone: "UTC",
		}
		s := mustSummary(t, d, ctx, f)
		if s.Concentration != 1.0 {
			t.Errorf(
				"Concentration = %f, want 1.0",
				s.Concentration,
			)
		}
	})

	t.Run("TwoProjects", func(t *testing.T) {
		d := testDB(t)
		insertSession(t, d, "c1", "alpha", func(s *Session) {
			s.StartedAt = new("2024-06-01T09:00:00Z")
			s.MessageCount = 35
			s.Agent = "claude"
		})
		insertSession(t, d, "c2", "beta", func(s *Session) {
			s.StartedAt = new("2024-06-01T10:00:00Z")
			s.MessageCount = 45
			s.Agent = "claude"
		})
		f := AnalyticsFilter{
			From:     "2024-06-01",
			To:       "2024-06-01",
			Timezone: "UTC",
		}
		s := mustSummary(t, d, ctx, f)
		// Both in top 3 → concentration = 1.0
		if s.Concentration != 1.0 {
			t.Errorf(
				"Concentration = %f, want 1.0",
				s.Concentration,
			)
		}
	})

	t.Run("FourProjects", func(t *testing.T) {
		d := testDB(t)
		for i, tc := range []struct {
			proj string
			msgs int
		}{
			{"p1", 40}, {"p2", 30}, {"p3", 20}, {"p4", 10},
		} {
			id := fmt.Sprintf("c%d", i)
			insertSession(t, d, id, tc.proj, func(s *Session) {
				ts := fmt.Sprintf(
					"2024-06-01T%02d:00:00Z", i+9,
				)
				s.StartedAt = &ts
				s.MessageCount = tc.msgs
				s.Agent = "claude"
			})
		}
		f := AnalyticsFilter{
			From:     "2024-06-01",
			To:       "2024-06-01",
			Timezone: "UTC",
		}
		s := mustSummary(t, d, ctx, f)
		// Top 3: 40+30+20 = 90, total = 100
		if s.Concentration != 0.9 {
			t.Errorf(
				"Concentration = %f, want 0.9",
				s.Concentration,
			)
		}
	})
}

func TestGetAnalyticsHourOfWeek(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	t.Run("EmptyDB", func(t *testing.T) {
		resp, err := d.GetAnalyticsHourOfWeek(ctx, baseFilter())
		if err != nil {
			t.Fatalf("GetAnalyticsHourOfWeek: %v", err)
		}
		if len(resp.Cells) != 168 {
			t.Errorf("len(Cells) = %d, want 168",
				len(resp.Cells))
		}
		for _, c := range resp.Cells {
			if c.Messages != 0 {
				t.Errorf(
					"day=%d hour=%d messages=%d, want 0",
					c.DayOfWeek, c.Hour, c.Messages,
				)
			}
		}
	})

	// Seed sessions with known UTC times:
	// 2024-06-01 is Saturday, 09:00 UTC
	insertSession(t, d, "hw1", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.MessageCount = 2
		s.Agent = "claude"
	})
	insertMessages(t, d,
		Message{
			SessionID: "hw1", Ordinal: 0, Role: "user",
			Content: "hi", ContentLength: 2,
			Timestamp: "2024-06-01T09:00:00Z",
		},
		Message{
			SessionID: "hw1", Ordinal: 1, Role: "assistant",
			Content: "hello", ContentLength: 5,
			Timestamp: "2024-06-01T09:30:00Z",
		},
	)

	// 23:00 UTC on a Saturday
	insertSession(t, d, "hw2", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T23:00:00Z")
		s.MessageCount = 1
		s.Agent = "claude"
	})
	insertMessages(t, d, Message{
		SessionID: "hw2", Ordinal: 0, Role: "user",
		Content: "late", ContentLength: 4,
		Timestamp: "2024-06-01T23:00:00Z",
	})

	t.Run("UTCBucketing", func(t *testing.T) {
		resp, err := d.GetAnalyticsHourOfWeek(ctx, baseFilter())
		if err != nil {
			t.Fatalf("GetAnalyticsHourOfWeek: %v", err)
		}
		// Saturday = ISO day 5 (Mon=0)
		// hour 9: 2 messages (user@09:00 + assistant@09:30)
		satH9 := findHOWCell(resp.Cells, 5, 9)
		if satH9 != 2 {
			t.Errorf("Sat 09:xx = %d, want 2", satH9)
		}
		satH23 := findHOWCell(resp.Cells, 5, 23)
		if satH23 != 1 {
			t.Errorf("Sat 23:00 = %d, want 1", satH23)
		}
	})

	t.Run("TimezoneShift", func(t *testing.T) {
		f := AnalyticsFilter{
			From:     "2024-06-01",
			To:       "2024-06-03",
			Timezone: "Asia/Karachi", // UTC+5
		}
		resp, err := d.GetAnalyticsHourOfWeek(ctx, f)
		if err != nil {
			t.Fatalf("GetAnalyticsHourOfWeek: %v", err)
		}
		// 23:00 UTC Sat → 04:00 Sun in UTC+5
		// Sunday = ISO day 6
		sunH4 := findHOWCell(resp.Cells, 6, 4)
		if sunH4 != 1 {
			t.Errorf(
				"Sun 04:00 PKT = %d, want 1", sunH4,
			)
		}
		// 09:00 UTC Sat → 14:00 Sat in UTC+5
		// 09:30 UTC Sat → 14:30 Sat in UTC+5
		// Both fall in hour 14
		satH14 := findHOWCell(resp.Cells, 5, 14)
		if satH14 != 2 {
			t.Errorf(
				"Sat 14:xx PKT = %d, want 2", satH14,
			)
		}
	})
}

func findHOWCell(cells []HourOfWeekCell, dow, hour int) int {
	for _, c := range cells {
		if c.DayOfWeek == dow && c.Hour == hour {
			return c.Messages
		}
	}
	return -1
}

func TestGetAnalyticsSessionShape(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	t.Run("EmptyDB", func(t *testing.T) {
		resp, err := d.GetAnalyticsSessionShape(
			ctx, baseFilter(),
		)
		if err != nil {
			t.Fatalf("GetAnalyticsSessionShape: %v", err)
		}
		if resp.Count != 0 {
			t.Errorf("Count = %d, want 0", resp.Count)
		}
	})

	// Session with 10 messages, 1h duration
	insertSession(t, d, "ss1", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.EndedAt = new("2024-06-01T10:00:00Z")
		s.MessageCount = 10
		s.Agent = "claude"
	})
	// 5 user + 5 assistant, assistant has tool_use
	for i := range 10 {
		role := "user"
		hasTool := false
		if i%2 == 1 {
			role = "assistant"
			hasTool = true
		}
		insertMessages(t, d, Message{
			SessionID: "ss1", Ordinal: i, Role: role,
			Content:       fmt.Sprintf("msg %d", i),
			ContentLength: 10, HasToolUse: hasTool,
			Timestamp: "2024-06-01T09:00:00Z",
		})
	}

	// Session with 25 messages, no ended_at
	insertSession(t, d, "ss2", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-02T10:00:00Z")
		s.MessageCount = 25
		s.Agent = "claude"
	})
	for i := range 25 {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		insertMessages(t, d, Message{
			SessionID: "ss2", Ordinal: i, Role: role,
			Content:       fmt.Sprintf("msg %d", i),
			ContentLength: 10,
			Timestamp:     "2024-06-02T10:00:00Z",
		})
	}

	t.Run("FullRange", func(t *testing.T) {
		resp, err := d.GetAnalyticsSessionShape(
			ctx, baseFilter(),
		)
		if err != nil {
			t.Fatalf("GetAnalyticsSessionShape: %v", err)
		}
		if resp.Count != 2 {
			t.Errorf("Count = %d, want 2", resp.Count)
		}

		// Length: 10 → "6-15", 25 → "16-30"
		lenMap := bucketMap(resp.LengthDistribution)
		if lenMap["6-15"] != 1 {
			t.Errorf("6-15 = %d, want 1", lenMap["6-15"])
		}
		if lenMap["16-30"] != 1 {
			t.Errorf("16-30 = %d, want 1", lenMap["16-30"])
		}

		// Duration: only ss1 has both start/end (60m → "1-2h")
		durMap := bucketMap(resp.DurationDistribution)
		if durMap["1-2h"] != 1 {
			t.Errorf("1-2h = %d, want 1", durMap["1-2h"])
		}
		totalDur := 0
		for _, b := range resp.DurationDistribution {
			totalDur += b.Count
		}
		if totalDur != 1 {
			t.Errorf(
				"total duration entries = %d, want 1",
				totalDur,
			)
		}
	})

	t.Run("Autonomy", func(t *testing.T) {
		resp, err := d.GetAnalyticsSessionShape(
			ctx, baseFilter(),
		)
		if err != nil {
			t.Fatalf("GetAnalyticsSessionShape: %v", err)
		}
		// ss1: 5 user, 5 assistant w/ tool → ratio 5/5=1.0 → "1-2"
		// ss2: 13 user, 0 tool → ratio 0/13=0 → "<0.5"
		autoMap := bucketMap(resp.AutonomyDistribution)
		if autoMap["1-2"] != 1 {
			t.Errorf("1-2 = %d, want 1", autoMap["1-2"])
		}
		if autoMap["<0.5"] != 1 {
			t.Errorf("<0.5 = %d, want 1", autoMap["<0.5"])
		}
	})

	t.Run("EmptyRange", func(t *testing.T) {
		resp, err := d.GetAnalyticsSessionShape(
			ctx, emptyFilter(),
		)
		if err != nil {
			t.Fatalf("GetAnalyticsSessionShape: %v", err)
		}
		if resp.Count != 0 {
			t.Errorf("Count = %d, want 0", resp.Count)
		}
	})
}

func bucketMap(
	buckets []DistributionBucket,
) map[string]int {
	m := make(map[string]int)
	for _, b := range buckets {
		m[b.Label] = b.Count
	}
	return m
}

func assertEq[T comparable](t *testing.T, name string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

type testClock struct {
	curr time.Time
}

func newTestClock(start string) *testClock {
	t, _ := time.Parse(time.RFC3339, start)
	return &testClock{curr: t}
}

func (c *testClock) Now() string {
	return c.curr.Format(time.RFC3339)
}

func (c *testClock) Next(d time.Duration) string {
	c.curr = c.curr.Add(d)
	return c.Now()
}

func insertConversation(t *testing.T, d *DB, id, proj, agent, start string, delays []time.Duration) {
	t.Helper()
	clock := newTestClock(start)

	insertSession(t, d, id, proj, func(s *Session) {
		s.StartedAt = new(start)
		s.MessageCount = len(delays)
		s.Agent = agent
		if len(delays) > 0 {
			endClock := newTestClock(start)
			for _, delay := range delays {
				endClock.Next(delay)
			}
			s.EndedAt = new(endClock.Now())
		}
	})

	var msgs []Message
	for i, delay := range delays {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, Message{
			SessionID:     id,
			Ordinal:       i,
			Role:          role,
			Content:       fmt.Sprintf("msg %d", i),
			ContentLength: 5,
			Timestamp:     clock.Next(delay),
		})
	}
	if len(msgs) > 0 {
		insertMessages(t, d, msgs...)
	}
}

func TestGetAnalyticsVelocity_Metrics(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	t.Run("EmptyDB", func(t *testing.T) {
		resp, err := d.GetAnalyticsVelocity(ctx, baseFilter())
		if err != nil {
			t.Fatalf("GetAnalyticsVelocity: %v", err)
		}
		assertEq(t, "len(ByAgent)", len(resp.ByAgent), 0)
	})

	// Session with messages at precise timestamps (10s apart)
	insertConversation(t, d, "v1", "proj", "claude", "2024-06-01T09:00:00Z", []time.Duration{
		0, 10 * time.Second, 10 * time.Second, 10 * time.Second, 10 * time.Second, 10 * time.Second,
	})

	t.Run("TurnCycle", func(t *testing.T) {
		resp, err := d.GetAnalyticsVelocity(ctx, baseFilter())
		if err != nil {
			t.Fatalf("GetAnalyticsVelocity: %v", err)
		}
		assertEq(t, "TurnCycle P50", resp.Overall.TurnCycleSec.P50, 10.0)
	})

	t.Run("FirstResponse", func(t *testing.T) {
		resp, err := d.GetAnalyticsVelocity(ctx, baseFilter())
		if err != nil {
			t.Fatalf("GetAnalyticsVelocity: %v", err)
		}
		assertEq(t, "FirstResponse P50", resp.Overall.FirstResponseSec.P50, 10.0)
	})

	t.Run("Throughput", func(t *testing.T) {
		resp, err := d.GetAnalyticsVelocity(ctx, baseFilter())
		if err != nil {
			t.Fatalf("GetAnalyticsVelocity: %v", err)
		}
		// Active time: 5 gaps of 10s = 50s ≈ 0.833 min
		// 6 msgs / 0.833 = ~7.2 msgs/min
		if resp.Overall.MsgsPerActiveMin < 7.0 || resp.Overall.MsgsPerActiveMin > 7.5 {
			t.Errorf("MsgsPerActiveMin = %f, want ~7.2", resp.Overall.MsgsPerActiveMin)
		}
	})

	t.Run("ByAgent", func(t *testing.T) {
		resp, err := d.GetAnalyticsVelocity(ctx, baseFilter())
		if err != nil {
			t.Fatalf("GetAnalyticsVelocity: %v", err)
		}
		assertEq(t, "len(ByAgent)", len(resp.ByAgent), 1)
		assertEq(t, "ByAgent[0].Label", resp.ByAgent[0].Label, "claude")
		assertEq(t, "ByAgent[0].Sessions", resp.ByAgent[0].Sessions, 1)
	})

	t.Run("ByComplexity", func(t *testing.T) {
		resp, err := d.GetAnalyticsVelocity(ctx, baseFilter())
		if err != nil {
			t.Fatalf("GetAnalyticsVelocity: %v", err)
		}
		assertEq(t, "len(ByComplexity)", len(resp.ByComplexity), 1)
		assertEq(t, "ByComplexity[0].Label", resp.ByComplexity[0].Label, "1-15")
	})
}

func TestGetAnalyticsVelocity_EdgeCases(t *testing.T) {
	ctx := context.Background()

	t.Run("LargeCycleExcluded", func(t *testing.T) {
		d := testDB(t)
		insertConversation(t, d, "v2", "proj", "claude", "2024-06-01T09:00:00Z", []time.Duration{
			0, 45 * time.Minute,
		})
		resp, err := d.GetAnalyticsVelocity(ctx, baseFilter())
		if err != nil {
			t.Fatalf("GetAnalyticsVelocity: %v", err)
		}
		assertEq(t, "TurnCycle P50", resp.Overall.TurnCycleSec.P50, 0.0)
	})

	t.Run("EmptyTimestampsSkipped", func(t *testing.T) {
		d := testDB(t)
		insertSession(t, d, "v3", "proj", func(s *Session) {
			s.StartedAt = new("2024-06-01T09:00:00Z")
			s.MessageCount = 2
			s.Agent = "claude"
		})
		insertMessages(t, d,
			Message{SessionID: "v3", Ordinal: 0, Role: "user", Content: "q", ContentLength: 1, Timestamp: ""},
			Message{SessionID: "v3", Ordinal: 1, Role: "assistant", Content: "a", ContentLength: 1, Timestamp: ""},
		)
		resp, err := d.GetAnalyticsVelocity(ctx, baseFilter())
		if err != nil {
			t.Fatalf("GetAnalyticsVelocity: %v", err)
		}
		assertEq(t, "TurnCycle P50", resp.Overall.TurnCycleSec.P50, 0.0)
	})

	t.Run("AssistantBeforeUser", func(t *testing.T) {
		d := testDB(t)
		insertSession(t, d, "v4", "proj", func(s *Session) {
			s.StartedAt = new("2024-06-01T09:00:00Z")
			s.MessageCount = 3
			s.Agent = "claude"
		})
		insertMessages(t, d,
			Message{SessionID: "v4", Ordinal: 0, Role: "assistant", Content: "system greeting", ContentLength: 15, Timestamp: "2024-06-01T09:00:00Z"},
			Message{SessionID: "v4", Ordinal: 1, Role: "user", Content: "hi", ContentLength: 2, Timestamp: "2024-06-01T09:00:10Z"},
			Message{SessionID: "v4", Ordinal: 2, Role: "assistant", Content: "hello", ContentLength: 5, Timestamp: "2024-06-01T09:00:20Z"},
		)
		resp, err := d.GetAnalyticsVelocity(ctx, baseFilter())
		if err != nil {
			t.Fatalf("GetAnalyticsVelocity: %v", err)
		}
		assertEq(t, "FirstResponse P50", resp.Overall.FirstResponseSec.P50, 10.0)
	})

	t.Run("OrdinalVsTimestampSkew", func(t *testing.T) {
		d := testDB(t)
		insertSession(t, d, "v5", "proj", func(s *Session) {
			s.StartedAt = new("2024-06-01T09:00:00Z")
			s.MessageCount = 3
			s.Agent = "claude"
		})
		insertMessages(t, d,
			Message{SessionID: "v5", Ordinal: 0, Role: "user", Content: "setup", ContentLength: 5, Timestamp: "2024-06-01T09:00:00Z"},
			Message{SessionID: "v5", Ordinal: 1, Role: "user", Content: "real question", ContentLength: 13, Timestamp: "2024-06-01T09:00:30Z"},
			Message{SessionID: "v5", Ordinal: 2, Role: "assistant", Content: "answer", ContentLength: 6, Timestamp: "2024-06-01T09:00:20Z"},
		)
		resp, err := d.GetAnalyticsVelocity(ctx, baseFilter())
		if err != nil {
			t.Fatalf("GetAnalyticsVelocity: %v", err)
		}
		assertEq(t, "FirstResponse P50", resp.Overall.FirstResponseSec.P50, 20.0)
	})

	t.Run("NegativeDeltaClampsToZero", func(t *testing.T) {
		d := testDB(t)
		insertSession(t, d, "v6", "proj", func(s *Session) {
			s.StartedAt = new("2024-06-01T09:00:00Z")
			s.MessageCount = 2
			s.Agent = "claude"
		})
		insertMessages(t, d,
			Message{SessionID: "v6", Ordinal: 0, Role: "user", Content: "hello", ContentLength: 5, Timestamp: "2024-06-01T09:00:30Z"},
			Message{SessionID: "v6", Ordinal: 1, Role: "assistant", Content: "hi", ContentLength: 2, Timestamp: "2024-06-01T09:00:10Z"},
		)
		resp, err := d.GetAnalyticsVelocity(ctx, baseFilter())
		if err != nil {
			t.Fatalf("GetAnalyticsVelocity: %v", err)
		}
		assertEq(t, "FirstResponse P50", resp.Overall.FirstResponseSec.P50, 0.0)
	})
}

func TestGetAnalyticsVelocity_ToolUsage(t *testing.T) {
	ctx := context.Background()

	t.Run("ToolCallsPerActiveMin", func(t *testing.T) {
		d := testDB(t)
		insertSession(t, d, "vt1", "proj", func(s *Session) {
			s.StartedAt = new("2024-06-01T09:00:00Z")
			s.MessageCount = 4
			s.Agent = "claude"
		})
		insertMessages(t, d,
			Message{SessionID: "vt1", Ordinal: 0, Role: "user", Content: "hi", ContentLength: 2, Timestamp: "2024-06-01T09:00:00Z"},
			Message{SessionID: "vt1", Ordinal: 1, Role: "assistant", Content: "hello", ContentLength: 5, Timestamp: "2024-06-01T09:00:10Z"},
			Message{SessionID: "vt1", Ordinal: 2, Role: "user", Content: "do X", ContentLength: 4, Timestamp: "2024-06-01T09:00:20Z"},
			Message{
				SessionID: "vt1", Ordinal: 3, Role: "assistant", Content: "done", ContentLength: 4,
				Timestamp: "2024-06-01T09:00:30Z", HasToolUse: true,
				ToolCalls: []ToolCall{
					{SessionID: "vt1", ToolName: "Read", Category: "Read"},
					{SessionID: "vt1", ToolName: "Bash", Category: "Bash"},
					{SessionID: "vt1", ToolName: "Edit", Category: "Edit"},
				},
			},
		)
		resp, err := d.GetAnalyticsVelocity(ctx, baseFilter())
		if err != nil {
			t.Fatalf("GetAnalyticsVelocity: %v", err)
		}
		assertEq(t, "ToolCallsPerActiveMin", resp.Overall.ToolCallsPerActiveMin, 6.0)
	})

	t.Run("ToolCallsByAgentBreakdown", func(t *testing.T) {
		d := testDB(t)
		insertSession(t, d, "vta1", "proj", func(s *Session) {
			s.StartedAt = new("2024-06-01T09:00:00Z")
			s.MessageCount = 2
			s.Agent = "claude"
		})
		insertMessages(t, d,
			Message{SessionID: "vta1", Ordinal: 0, Role: "user", Content: "q", ContentLength: 1, Timestamp: "2024-06-01T09:00:00Z"},
			Message{
				SessionID: "vta1", Ordinal: 1, Role: "assistant", Content: "a", ContentLength: 1,
				Timestamp: "2024-06-01T09:00:30Z", HasToolUse: true,
				ToolCalls: []ToolCall{{SessionID: "vta1", ToolName: "Read", Category: "Read"}},
			},
		)
		insertSession(t, d, "vta2", "proj", func(s *Session) {
			s.StartedAt = new("2024-06-01T10:00:00Z")
			s.MessageCount = 2
			s.Agent = "codex"
		})
		insertMessages(t, d,
			Message{SessionID: "vta2", Ordinal: 0, Role: "user", Content: "q", ContentLength: 1, Timestamp: "2024-06-01T10:00:00Z"},
			Message{
				SessionID: "vta2", Ordinal: 1, Role: "assistant", Content: "a", ContentLength: 1,
				Timestamp: "2024-06-01T10:00:30Z", HasToolUse: true,
				ToolCalls: []ToolCall{
					{SessionID: "vta2", ToolName: "Bash", Category: "Bash"},
					{SessionID: "vta2", ToolName: "Edit", Category: "Edit"},
				},
			},
		)
		resp, err := d.GetAnalyticsVelocity(ctx, baseFilter())
		if err != nil {
			t.Fatalf("GetAnalyticsVelocity: %v", err)
		}
		if len(resp.ByAgent) < 2 {
			t.Fatalf("ByAgent has %d entries, want >= 2", len(resp.ByAgent))
		}
		agentMap := make(map[string]VelocityBreakdown)
		for _, b := range resp.ByAgent {
			agentMap[b.Label] = b
		}
		assertEq(t, "claude ToolCallsPerActiveMin", agentMap["claude"].Overview.ToolCallsPerActiveMin, 2.0)
		assertEq(t, "codex ToolCallsPerActiveMin", agentMap["codex"].Overview.ToolCallsPerActiveMin, 4.0)
	})

	t.Run("ToolCallsPerActiveMinZero", func(t *testing.T) {
		d := testDB(t)
		insertConversation(t, d, "vt2", "proj", "claude", "2024-06-01T09:00:00Z", []time.Duration{
			0, 10 * time.Second,
		})
		resp, err := d.GetAnalyticsVelocity(ctx, baseFilter())
		if err != nil {
			t.Fatalf("GetAnalyticsVelocity: %v", err)
		}
		assertEq(t, "ToolCallsPerActiveMin", resp.Overall.ToolCallsPerActiveMin, 0.0)
	})
}

func TestVelocityChunkedQuery(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Seed >500 sessions to exercise chunked IN-clause logic.
	const n = 600
	for i := range n {
		id := fmt.Sprintf("chunk-%d", i)
		insertSession(t, d, id, "proj", func(s *Session) {
			s.StartedAt = new("2024-06-01T09:00:00Z")
			s.MessageCount = 2
			s.Agent = "claude"
		})
		insertMessages(t, d,
			Message{
				SessionID: id, Ordinal: 0, Role: "user",
				Content: "q", ContentLength: 1,
				Timestamp: "2024-06-01T09:00:00Z",
			},
			Message{
				SessionID: id, Ordinal: 1,
				Role:    "assistant",
				Content: "a", ContentLength: 1,
				Timestamp: "2024-06-01T09:00:10Z",
			},
		)
	}

	// Velocity must not fail even with >500 sessions
	resp, err := d.GetAnalyticsVelocity(ctx, baseFilter())
	if err != nil {
		t.Fatalf("GetAnalyticsVelocity with %d sessions: %v",
			n, err)
	}
	if resp.ByComplexity[0].Sessions != n {
		t.Errorf("sessions = %d, want %d",
			resp.ByComplexity[0].Sessions, n)
	}

	// SessionShape must not fail either
	shape, err := d.GetAnalyticsSessionShape(ctx, baseFilter())
	if err != nil {
		t.Fatalf(
			"GetAnalyticsSessionShape with %d sessions: %v",
			n, err,
		)
	}
	if shape.Count != n {
		t.Errorf("Count = %d, want %d", shape.Count, n)
	}
}

func TestPercentileFloat(t *testing.T) {
	tests := []struct {
		name   string
		sorted []float64
		pct    float64
		want   float64
	}{
		{"Empty", []float64{}, 0.5, 0},
		{"Single", []float64{5.0}, 0.5, 5.0},
		{"P50Odd", []float64{1, 3, 7}, 0.5, 3.0},
		{"P90", []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 0.9, 10.0},
		{"P50Even", []float64{1, 2, 3, 4}, 0.5, 3.0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := percentileFloat(tt.sorted, tt.pct)
			if got != tt.want {
				t.Errorf("percentileFloat(%v, %f) = %f, want %f",
					tt.sorted, tt.pct, got, tt.want)
			}
		})
	}
}

func TestGetAnalyticsTools(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	t.Run("EmptyDB", func(t *testing.T) {
		resp, err := d.GetAnalyticsTools(ctx, baseFilter())
		if err != nil {
			t.Fatalf("GetAnalyticsTools: %v", err)
		}
		if resp.TotalCalls != 0 {
			t.Errorf("TotalCalls = %d, want 0",
				resp.TotalCalls)
		}
		if len(resp.ByCategory) != 0 {
			t.Errorf("len(ByCategory) = %d, want 0",
				len(resp.ByCategory))
		}
	})

	// Seed sessions with tool_calls.
	insertSession(t, d, "t1", "alpha", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.MessageCount = 3
		s.Agent = "claude"
	})
	m1 := asstMsg("t1", 0, "[Read: a.go]")
	m1.HasToolUse = true
	m1.ToolCalls = []ToolCall{
		{SessionID: "t1", ToolName: "Read", Category: "Read"},
		{SessionID: "t1", ToolName: "Read", Category: "Read"},
	}
	m2 := asstMsg("t1", 1, "[Bash: ls]")
	m2.HasToolUse = true
	m2.ToolCalls = []ToolCall{
		{SessionID: "t1", ToolName: "Bash", Category: "Bash"},
	}
	m3 := asstMsg("t1", 2, "[Edit: b.go]")
	m3.HasToolUse = true
	m3.ToolCalls = []ToolCall{
		{SessionID: "t1", ToolName: "Edit", Category: "Edit"},
	}
	insertMessages(t, d, m1, m2, m3)

	insertSession(t, d, "t2", "beta", func(s *Session) {
		s.StartedAt = new("2024-06-02T10:00:00Z")
		s.MessageCount = 1
		s.Agent = "codex"
	})
	m4 := asstMsg("t2", 0, "[Read: c.go]")
	m4.HasToolUse = true
	m4.ToolCalls = []ToolCall{
		{SessionID: "t2", ToolName: "Read", Category: "Read"},
		{SessionID: "t2", ToolName: "Grep", Category: "Grep"},
	}
	insertMessages(t, d, m4)

	t.Run("TotalCalls", func(t *testing.T) {
		resp, err := d.GetAnalyticsTools(ctx, baseFilter())
		if err != nil {
			t.Fatalf("GetAnalyticsTools: %v", err)
		}
		// 2 Read + 1 Bash + 1 Edit + 1 Read + 1 Grep = 6
		if resp.TotalCalls != 6 {
			t.Errorf("TotalCalls = %d, want 6",
				resp.TotalCalls)
		}
	})

	t.Run("ByCategory", func(t *testing.T) {
		resp, err := d.GetAnalyticsTools(ctx, baseFilter())
		if err != nil {
			t.Fatalf("GetAnalyticsTools: %v", err)
		}
		catMap := make(map[string]int)
		for _, c := range resp.ByCategory {
			catMap[c.Category] = c.Count
		}
		if catMap["Read"] != 3 {
			t.Errorf("Read = %d, want 3", catMap["Read"])
		}
		if catMap["Bash"] != 1 {
			t.Errorf("Bash = %d, want 1", catMap["Bash"])
		}
		if catMap["Edit"] != 1 {
			t.Errorf("Edit = %d, want 1", catMap["Edit"])
		}
		if catMap["Grep"] != 1 {
			t.Errorf("Grep = %d, want 1", catMap["Grep"])
		}
		// Sorted by count desc: Read first
		if resp.ByCategory[0].Category != "Read" {
			t.Errorf("first category = %q, want Read",
				resp.ByCategory[0].Category)
		}
	})

	t.Run("ByCategoryPct", func(t *testing.T) {
		resp, err := d.GetAnalyticsTools(ctx, baseFilter())
		if err != nil {
			t.Fatalf("GetAnalyticsTools: %v", err)
		}
		// Read: 3/6 = 50%
		if resp.ByCategory[0].Pct != 50.0 {
			t.Errorf("Read pct = %f, want 50.0",
				resp.ByCategory[0].Pct)
		}
	})

	t.Run("ByAgent", func(t *testing.T) {
		resp, err := d.GetAnalyticsTools(ctx, baseFilter())
		if err != nil {
			t.Fatalf("GetAnalyticsTools: %v", err)
		}
		if len(resp.ByAgent) != 2 {
			t.Fatalf("len(ByAgent) = %d, want 2",
				len(resp.ByAgent))
		}
		// Alphabetical: claude, codex
		if resp.ByAgent[0].Agent != "claude" {
			t.Errorf("first agent = %q, want claude",
				resp.ByAgent[0].Agent)
		}
		if resp.ByAgent[0].Total != 4 {
			t.Errorf("claude total = %d, want 4",
				resp.ByAgent[0].Total)
		}
		if resp.ByAgent[1].Agent != "codex" {
			t.Errorf("second agent = %q, want codex",
				resp.ByAgent[1].Agent)
		}
		if resp.ByAgent[1].Total != 2 {
			t.Errorf("codex total = %d, want 2",
				resp.ByAgent[1].Total)
		}
	})

	t.Run("Trend", func(t *testing.T) {
		resp, err := d.GetAnalyticsTools(ctx, baseFilter())
		if err != nil {
			t.Fatalf("GetAnalyticsTools: %v", err)
		}
		// 2024-06-01 is Saturday, 2024-06-02 is Sunday.
		// Both in same ISO week (May 27 week start).
		// But 2024-06-03 is Monday, different week.
		// So trend should have 1 entry (week of May 27).
		if len(resp.Trend) != 1 {
			t.Fatalf("len(Trend) = %d, want 1",
				len(resp.Trend))
		}
		total := 0
		for _, v := range resp.Trend[0].ByCat {
			total += v
		}
		if total != 6 {
			t.Errorf("week total = %d, want 6", total)
		}
	})

	t.Run("ProjectFilter", func(t *testing.T) {
		f := baseFilter()
		f.Project = "alpha"
		resp, err := d.GetAnalyticsTools(ctx, f)
		if err != nil {
			t.Fatalf("GetAnalyticsTools: %v", err)
		}
		if resp.TotalCalls != 4 {
			t.Errorf("TotalCalls = %d, want 4",
				resp.TotalCalls)
		}
	})

	t.Run("EmptyDateRange", func(t *testing.T) {
		resp, err := d.GetAnalyticsTools(
			ctx, emptyFilter(),
		)
		if err != nil {
			t.Fatalf("GetAnalyticsTools: %v", err)
		}
		if resp.TotalCalls != 0 {
			t.Errorf("TotalCalls = %d, want 0",
				resp.TotalCalls)
		}
	})
}

func TestGetAnalyticsToolsCanceled(t *testing.T) {
	d := testDB(t)
	ctx := canceledCtx()
	_, err := d.GetAnalyticsTools(ctx, baseFilter())
	requireCanceledErr(t, err)
}

func TestActivityToolAndThinkingCounts(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "at1", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.MessageCount = 3
		s.Agent = "claude"
	})

	// User message, assistant with thinking, assistant with tool
	u := userMsg("at1", 0, "hello")
	a1 := asstMsg("at1", 1, "thinking response")
	a1.HasThinking = true
	a2 := asstMsg("at1", 2, "[Read: a.go]")
	a2.HasToolUse = true
	a2.ToolCalls = []ToolCall{
		{SessionID: "at1", ToolName: "Read", Category: "Read"},
		{SessionID: "at1", ToolName: "Bash", Category: "Bash"},
	}
	insertMessages(t, d, u, a1, a2)

	resp := mustActivity(t, d, ctx, baseFilter(), "day")
	if len(resp.Series) == 0 {
		t.Fatal("expected non-empty series")
	}

	entry := resp.Series[0]
	if entry.ThinkingMessages != 1 {
		t.Errorf("ThinkingMessages = %d, want 1",
			entry.ThinkingMessages)
	}
	if entry.ToolCalls != 2 {
		t.Errorf("ToolCalls = %d, want 2",
			entry.ToolCalls)
	}
}

func TestGetAnalyticsTopSessions(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	t.Run("EmptyDB", func(t *testing.T) {
		resp, err := d.GetAnalyticsTopSessions(
			ctx, baseFilter(), "messages",
		)
		if err != nil {
			t.Fatalf("GetAnalyticsTopSessions: %v", err)
		}
		if len(resp.Sessions) != 0 {
			t.Errorf("len(Sessions) = %d, want 0",
				len(resp.Sessions))
		}
		if resp.Metric != "messages" {
			t.Errorf("Metric = %q, want messages",
				resp.Metric)
		}
	})

	stats := seedAnalyticsData(t, d)

	t.Run("ByMessages", func(t *testing.T) {
		resp, err := d.GetAnalyticsTopSessions(
			ctx, baseFilter(), "messages",
		)
		if err != nil {
			t.Fatalf("GetAnalyticsTopSessions: %v", err)
		}
		if len(resp.Sessions) != stats.TotalSessions {
			t.Fatalf("len(Sessions) = %d, want %d",
				len(resp.Sessions), stats.TotalSessions)
		}
		// First should be the session with most messages (b1=30)
		if resp.Sessions[0].MessageCount != 30 {
			t.Errorf("top session messages = %d, want 30",
				resp.Sessions[0].MessageCount)
		}
		if resp.Sessions[0].Project != "project-beta" {
			t.Errorf("top session project = %q, want project-beta",
				resp.Sessions[0].Project)
		}
	})

	t.Run("ByDuration", func(t *testing.T) {
		resp, err := d.GetAnalyticsTopSessions(
			ctx, baseFilter(), "duration",
		)
		if err != nil {
			t.Fatalf("GetAnalyticsTopSessions: %v", err)
		}
		if resp.Metric != "duration" {
			t.Errorf("Metric = %q, want duration",
				resp.Metric)
		}
		// All seeded sessions have 1h duration except a1
		// which runs from 09:00 to midyear
		if len(resp.Sessions) == 0 {
			t.Fatal("expected non-empty sessions")
		}
		// All sessions should have positive duration
		for _, s := range resp.Sessions {
			if s.DurationMin <= 0 {
				t.Errorf("session %s duration = %f, want > 0",
					s.ID, s.DurationMin)
			}
		}
	})

	t.Run("DefaultMetric", func(t *testing.T) {
		resp, err := d.GetAnalyticsTopSessions(
			ctx, baseFilter(), "",
		)
		if err != nil {
			t.Fatalf("GetAnalyticsTopSessions: %v", err)
		}
		if resp.Metric != "messages" {
			t.Errorf("Metric = %q, want messages",
				resp.Metric)
		}
	})

	t.Run("ProjectFilter", func(t *testing.T) {
		f := baseFilter()
		f.Project = "project-alpha"
		resp, err := d.GetAnalyticsTopSessions(
			ctx, f, "messages",
		)
		if err != nil {
			t.Fatalf("GetAnalyticsTopSessions: %v", err)
		}
		if len(resp.Sessions) != 3 {
			t.Errorf("len(Sessions) = %d, want 3",
				len(resp.Sessions))
		}
		for _, s := range resp.Sessions {
			if s.Project != "project-alpha" {
				t.Errorf("session project = %q, want project-alpha",
					s.Project)
			}
		}
	})

	t.Run("EmptyDateRange", func(t *testing.T) {
		resp, err := d.GetAnalyticsTopSessions(
			ctx, emptyFilter(), "messages",
		)
		if err != nil {
			t.Fatalf("GetAnalyticsTopSessions: %v", err)
		}
		if len(resp.Sessions) != 0 {
			t.Errorf("len(Sessions) = %d, want 0",
				len(resp.Sessions))
		}
	})
}

func TestBuildWhereProjectFilter(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedAnalyticsData(t, d)

	t.Run("SummaryWithProject", func(t *testing.T) {
		f := baseFilter()
		f.Project = "project-alpha"
		s := mustSummary(t, d, ctx, f)
		if s.TotalSessions != 3 {
			t.Errorf("TotalSessions = %d, want 3",
				s.TotalSessions)
		}
	})

	t.Run("SummaryWithNonexistentProject", func(t *testing.T) {
		f := baseFilter()
		f.Project = "nonexistent"
		s := mustSummary(t, d, ctx, f)
		if s.TotalSessions != 0 {
			t.Errorf("TotalSessions = %d, want 0",
				s.TotalSessions)
		}
	})
}

func TestTimeFilter(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Create sessions with messages at known day/hour combos.
	// 2024-06-01 = Saturday (ISO dow 5)
	// 2024-06-03 = Monday   (ISO dow 0)
	insertSession(t, d, "tf1", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.EndedAt = new("2024-06-01T10:00:00Z")
		s.MessageCount = 2
		s.Agent = "claude"
	})
	insertMessages(t, d,
		Message{
			SessionID: "tf1", Ordinal: 0, Role: "user",
			Timestamp: "2024-06-01T09:05:00Z",
			Content:   "hello", ContentLength: 5,
		},
		Message{
			SessionID: "tf1", Ordinal: 1, Role: "assistant",
			Timestamp: "2024-06-01T09:10:00Z",
			Content:   "hi", ContentLength: 2,
		},
	)

	insertSession(t, d, "tf2", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T14:00:00Z")
		s.EndedAt = new("2024-06-01T15:00:00Z")
		s.MessageCount = 1
		s.Agent = "claude"
	})
	insertMessages(t, d, Message{
		SessionID: "tf2", Ordinal: 0, Role: "user",
		Timestamp: "2024-06-01T14:05:00Z",
		Content:   "world", ContentLength: 5,
	})

	insertSession(t, d, "tf3", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-03T09:00:00Z")
		s.EndedAt = new("2024-06-03T10:00:00Z")
		s.MessageCount = 1
		s.Agent = "claude"
	})
	insertMessages(t, d, Message{
		SessionID: "tf3", Ordinal: 0, Role: "user",
		Timestamp: "2024-06-03T09:30:00Z",
		Content:   "test", ContentLength: 4,
	})

	f := AnalyticsFilter{
		From:     "2024-06-01",
		To:       "2024-06-03",
		Timezone: "UTC",
	}

	t.Run("FilterByHour", func(t *testing.T) {
		ff := f
		hour := 9
		ff.Hour = &hour
		s := mustSummary(t, d, ctx, ff)
		// tf1 and tf3 have messages at hour 9
		if s.TotalSessions != 2 {
			t.Errorf("TotalSessions = %d, want 2",
				s.TotalSessions)
		}
	})

	t.Run("FilterByDow", func(t *testing.T) {
		ff := f
		dow := 5 // Saturday
		ff.DayOfWeek = &dow
		s := mustSummary(t, d, ctx, ff)
		// tf1 and tf2 are on Saturday
		if s.TotalSessions != 2 {
			t.Errorf("TotalSessions = %d, want 2",
				s.TotalSessions)
		}
	})

	t.Run("FilterByDowAndHour", func(t *testing.T) {
		ff := f
		dow := 5
		hour := 14
		ff.DayOfWeek = &dow
		ff.Hour = &hour
		s := mustSummary(t, d, ctx, ff)
		// Only tf2 has messages on Saturday at hour 14
		if s.TotalSessions != 1 {
			t.Errorf("TotalSessions = %d, want 1",
				s.TotalSessions)
		}
	})

	t.Run("NoTimeFilter", func(t *testing.T) {
		s := mustSummary(t, d, ctx, f)
		// All 3 sessions
		if s.TotalSessions != 3 {
			t.Errorf("TotalSessions = %d, want 3",
				s.TotalSessions)
		}
	})
}

func TestAnalyticsFilterAgentAndMinUserMessages(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "c1", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.EndedAt = new("2024-06-01T10:00:00Z")
		s.MessageCount = 10
		s.UserMessageCount = 5
		s.Agent = "claude"
	})
	insertSession(t, d, "c2", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T11:00:00Z")
		s.EndedAt = new("2024-06-01T12:00:00Z")
		s.MessageCount = 4
		s.UserMessageCount = 1
		s.Agent = "claude"
	})
	insertSession(t, d, "x1", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T14:00:00Z")
		s.EndedAt = new("2024-06-01T15:00:00Z")
		s.MessageCount = 20
		s.UserMessageCount = 8
		s.Agent = "codex"
	})

	f := baseFilter()

	t.Run("NoFilters", func(t *testing.T) {
		s := mustSummary(t, d, ctx, f)
		if s.TotalSessions != 3 {
			t.Errorf("TotalSessions = %d, want 3",
				s.TotalSessions)
		}
	})

	t.Run("AgentOnly", func(t *testing.T) {
		af := f
		af.Agent = "claude"
		s := mustSummary(t, d, ctx, af)
		if s.TotalSessions != 2 {
			t.Errorf("TotalSessions = %d, want 2",
				s.TotalSessions)
		}
	})

	t.Run("MinUserMessagesOnly", func(t *testing.T) {
		af := f
		af.MinUserMessages = 5
		s := mustSummary(t, d, ctx, af)
		if s.TotalSessions != 2 {
			t.Errorf("TotalSessions = %d, want 2",
				s.TotalSessions)
		}
	})

	t.Run("AgentAndMinUserMessages", func(t *testing.T) {
		af := f
		af.Agent = "claude"
		af.MinUserMessages = 2
		s := mustSummary(t, d, ctx, af)
		if s.TotalSessions != 1 {
			t.Errorf("TotalSessions = %d, want 1",
				s.TotalSessions)
		}
	})

	t.Run("ActiveSince", func(t *testing.T) {
		af := f
		af.ActiveSince = "2024-06-01T13:00:00Z"
		s := mustSummary(t, d, ctx, af)
		if s.TotalSessions != 1 {
			t.Errorf("TotalSessions = %d, want 1",
				s.TotalSessions)
		}
	})
}

func TestAutonomyExcludesSystemMessages(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "s1", "proj", func(s *Session) {
		s.Agent = "zencoder"
		s.StartedAt = new(tsMidYear)
		s.EndedAt = new(tsMidYear)
		s.MessageCount = 4
	})

	msgs := []Message{
		{SessionID: "s1", Ordinal: 0, Role: "user",
			Content: "system banner", ContentLength: 13,
			Timestamp: tsMidYear, IsSystem: true},
		{SessionID: "s1", Ordinal: 1, Role: "user",
			Content: "real question", ContentLength: 13,
			Timestamp: tsMidYear},
		{SessionID: "s1", Ordinal: 2, Role: "assistant",
			Content: "answer", ContentLength: 6,
			Timestamp: tsMidYear, HasToolUse: true},
		{SessionID: "s1", Ordinal: 3, Role: "user",
			Content: "finish marker", ContentLength: 13,
			Timestamp: tsMidYear, IsSystem: true},
	}
	insertMessages(t, d, msgs...)

	resp, err := d.GetAnalyticsSessionShape(
		context.Background(),
		AnalyticsFilter{
			From: "2024-01-01", To: "2024-12-31",
		},
	)
	requireNoError(t, err, "GetAnalyticsSessionShape")

	// The autonomy ratio should be based on 1 real user message
	// (not 3), yielding ratio = 1/1 = 1.0 -> bucket "1-2".
	for _, b := range resp.AutonomyDistribution {
		if b.Label == "1-2" && b.Count == 1 {
			return // found the expected bucket
		}
	}
	t.Errorf("expected autonomy bucket '1-2' with count 1, got %v",
		resp.AutonomyDistribution)
}

func TestActivityExcludesSystemUserMessages(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "s1", "proj", func(s *Session) {
		s.Agent = "zencoder"
		s.StartedAt = new(tsMidYear)
		s.EndedAt = new(tsMidYear)
		s.MessageCount = 3
	})

	msgs := []Message{
		{SessionID: "s1", Ordinal: 0, Role: "user",
			Content: "system banner", ContentLength: 13,
			Timestamp: tsMidYear, IsSystem: true},
		{SessionID: "s1", Ordinal: 1, Role: "user",
			Content: "real question", ContentLength: 13,
			Timestamp: tsMidYear},
		{SessionID: "s1", Ordinal: 2, Role: "assistant",
			Content: "answer", ContentLength: 6,
			Timestamp: tsMidYear},
	}
	insertMessages(t, d, msgs...)

	resp, err := d.GetAnalyticsActivity(
		context.Background(),
		AnalyticsFilter{
			From: "2024-01-01", To: "2024-12-31",
		},
		"day",
	)
	requireNoError(t, err, "GetAnalyticsActivity")

	if len(resp.Series) != 1 {
		t.Fatalf("got %d entries, want 1", len(resp.Series))
	}
	entry := resp.Series[0]
	// 3 total messages but only 1 real user message
	if entry.Messages != 3 {
		t.Errorf("Messages = %d, want 3", entry.Messages)
	}
	if entry.UserMessages != 1 {
		t.Errorf("UserMessages = %d, want 1 (system excluded)",
			entry.UserMessages)
	}
	if entry.AssistantMessages != 1 {
		t.Errorf("AssistantMessages = %d, want 1",
			entry.AssistantMessages)
	}
}

func TestGetAnalyticsSignals(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	t.Run("EmptyDB", func(t *testing.T) {
		resp, err := d.GetAnalyticsSignals(ctx, baseFilter())
		if err != nil {
			t.Fatalf("GetAnalyticsSignals: %v", err)
		}
		assertEq(t, "ScoredSessions", resp.ScoredSessions, 0)
		assertEq(t, "UnscoredSessions",
			resp.UnscoredSessions, 0)
		assertEq(t, "len(Trend)", len(resp.Trend), 0)
		assertEq(t, "len(ByAgent)", len(resp.ByAgent), 0)
		assertEq(t, "len(ByProject)", len(resp.ByProject), 0)
	})

	// Seed sessions with signal data.
	// UpsertSession only writes core fields; signal columns
	// are written by UpdateSessionSignals.
	insertSession(t, d, "sig1", "alpha", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.MessageCount = 10
		s.Agent = "claude"
	})
	cp1 := 0.6
	updateSignals(t, d, "sig1", SessionSignalUpdate{
		HealthScore:            new(85),
		HealthGrade:            new("B"),
		Outcome:                "completed",
		OutcomeConfidence:      "high",
		ToolFailureSignalCount: 2,
		ToolRetryCount:         1,
		CompactionCount:        1,
		ContextPressureMax:     &cp1,
	})
	insertSession(t, d, "sig2", "alpha", func(s *Session) {
		s.StartedAt = new("2024-06-01T14:00:00Z")
		s.MessageCount = 5
		s.Agent = "codex"
	})
	cp2 := 0.9
	updateSignals(t, d, "sig2", SessionSignalUpdate{
		HealthScore:            new(45),
		HealthGrade:            new("D"),
		Outcome:                "errored",
		OutcomeConfidence:      "medium",
		ToolFailureSignalCount: 5,
		ToolRetryCount:         3,
		EditChurnCount:         2,
		ContextPressureMax:     &cp2,
	})
	insertSession(t, d, "sig3", "beta", func(s *Session) {
		s.StartedAt = new("2024-06-02T10:00:00Z")
		s.MessageCount = 8
		s.Agent = "claude"
	})
	updateSignals(t, d, "sig3", SessionSignalUpdate{
		Outcome:           "abandoned",
		OutcomeConfidence: "low",
		CompactionCount:   3,
		// No health score (unscored)
	})

	t.Run("ScoredVsUnscored", func(t *testing.T) {
		resp, err := d.GetAnalyticsSignals(ctx, baseFilter())
		if err != nil {
			t.Fatalf("GetAnalyticsSignals: %v", err)
		}
		assertEq(t, "ScoredSessions", resp.ScoredSessions, 2)
		assertEq(t, "UnscoredSessions",
			resp.UnscoredSessions, 1)
	})

	t.Run("GradeDistribution", func(t *testing.T) {
		resp, err := d.GetAnalyticsSignals(ctx, baseFilter())
		if err != nil {
			t.Fatalf("GetAnalyticsSignals: %v", err)
		}
		assertEq(t, "grade B", resp.GradeDistribution["B"], 1)
		assertEq(t, "grade D", resp.GradeDistribution["D"], 1)
	})

	t.Run("AvgHealthScore", func(t *testing.T) {
		resp, err := d.GetAnalyticsSignals(ctx, baseFilter())
		if err != nil {
			t.Fatalf("GetAnalyticsSignals: %v", err)
		}
		if resp.AvgHealthScore == nil {
			t.Fatal("AvgHealthScore is nil")
		}
		// (85 + 45) / 2 = 65.0
		assertEq(t, "AvgHealthScore",
			*resp.AvgHealthScore, 65.0)
	})

	t.Run("OutcomeDistribution", func(t *testing.T) {
		resp, err := d.GetAnalyticsSignals(ctx, baseFilter())
		if err != nil {
			t.Fatalf("GetAnalyticsSignals: %v", err)
		}
		assertEq(t, "completed",
			resp.OutcomeDistribution["completed"], 1)
		assertEq(t, "errored",
			resp.OutcomeDistribution["errored"], 1)
		assertEq(t, "abandoned",
			resp.OutcomeDistribution["abandoned"], 1)
	})

	t.Run("ToolHealth", func(t *testing.T) {
		resp, err := d.GetAnalyticsSignals(ctx, baseFilter())
		if err != nil {
			t.Fatalf("GetAnalyticsSignals: %v", err)
		}
		// 2 + 5 + 0 = 7
		assertEq(t, "TotalFailureSignals",
			resp.ToolHealth.TotalFailureSignals, 7)
		// 1 + 3 + 0 = 4
		assertEq(t, "TotalRetries",
			resp.ToolHealth.TotalRetries, 4)
		// 0 + 2 + 0 = 2
		assertEq(t, "TotalEditChurn",
			resp.ToolHealth.TotalEditChurn, 2)
		// sig1 and sig2 have failures
		assertEq(t, "SessionsWithFailures",
			resp.ToolHealth.SessionsWithFailures, 2)
	})

	t.Run("ContextHealth", func(t *testing.T) {
		resp, err := d.GetAnalyticsSignals(ctx, baseFilter())
		if err != nil {
			t.Fatalf("GetAnalyticsSignals: %v", err)
		}
		// sig1 (1) and sig3 (3) have compaction > 0
		assertEq(t, "SessionsWithCompaction",
			resp.ContextHealth.SessionsWithCompaction, 2)
		// sig1 and sig2 have context_pressure_max
		assertEq(t, "SessionsWithContextData",
			resp.ContextHealth.SessionsWithContextData, 2)
		// sig2 has pressure >= 0.8
		assertEq(t, "HighPressureSessions",
			resp.ContextHealth.HighPressureSessions, 1)
	})

	t.Run("Trend", func(t *testing.T) {
		resp, err := d.GetAnalyticsSignals(ctx, baseFilter())
		if err != nil {
			t.Fatalf("GetAnalyticsSignals: %v", err)
		}
		// 2 dates: 2024-06-01 (2 sessions), 2024-06-02 (1)
		assertEq(t, "len(Trend)", len(resp.Trend), 2)
		// Sorted by date
		assertEq(t, "Trend[0].Date",
			resp.Trend[0].Date, "2024-06-01")
		assertEq(t, "Trend[0].SessionCount",
			resp.Trend[0].SessionCount, 2)
		assertEq(t, "Trend[0].Completed",
			resp.Trend[0].Completed, 1)
		assertEq(t, "Trend[0].Errored",
			resp.Trend[0].Errored, 1)
		assertEq(t, "Trend[1].Date",
			resp.Trend[1].Date, "2024-06-02")
		assertEq(t, "Trend[1].Abandoned",
			resp.Trend[1].Abandoned, 1)
	})

	t.Run("ByAgent", func(t *testing.T) {
		resp, err := d.GetAnalyticsSignals(ctx, baseFilter())
		if err != nil {
			t.Fatalf("GetAnalyticsSignals: %v", err)
		}
		// 2 agents: claude (2 sessions), codex (1)
		assertEq(t, "len(ByAgent)", len(resp.ByAgent), 2)
		// Alphabetical: claude first
		assertEq(t, "ByAgent[0].Agent",
			resp.ByAgent[0].Agent, "claude")
		assertEq(t, "ByAgent[0].SessionCount",
			resp.ByAgent[0].SessionCount, 2)
		assertEq(t, "ByAgent[1].Agent",
			resp.ByAgent[1].Agent, "codex")
	})

	t.Run("ByProject", func(t *testing.T) {
		resp, err := d.GetAnalyticsSignals(ctx, baseFilter())
		if err != nil {
			t.Fatalf("GetAnalyticsSignals: %v", err)
		}
		// 2 projects: alpha (2), beta (1)
		// Sorted by session count desc
		assertEq(t, "len(ByProject)", len(resp.ByProject), 2)
		assertEq(t, "ByProject[0].Project",
			resp.ByProject[0].Project, "alpha")
		assertEq(t, "ByProject[0].SessionCount",
			resp.ByProject[0].SessionCount, 2)
	})

	t.Run("ProjectFilter", func(t *testing.T) {
		f := baseFilter()
		f.Project = "beta"
		resp, err := d.GetAnalyticsSignals(ctx, f)
		if err != nil {
			t.Fatalf("GetAnalyticsSignals: %v", err)
		}
		assertEq(t, "ScoredSessions", resp.ScoredSessions, 0)
		assertEq(t, "UnscoredSessions",
			resp.UnscoredSessions, 1)
		assertEq(t, "len(ByProject)", len(resp.ByProject), 1)
	})

	t.Run("EmptyDateRange", func(t *testing.T) {
		resp, err := d.GetAnalyticsSignals(
			ctx, emptyFilter(),
		)
		if err != nil {
			t.Fatalf("GetAnalyticsSignals: %v", err)
		}
		assertEq(t, "ScoredSessions", resp.ScoredSessions, 0)
	})
}

func TestLocalTime(t *testing.T) {
	tests := []struct {
		name  string
		ts    string
		valid bool
	}{
		{"RFC3339", "2024-06-01T15:00:00Z", true},
		{"RFC3339Nano", "2024-06-01T15:00:00.123Z", true},
		{"NoFraction", "2024-06-01T15:00:00Z", true},
		{"BadFormat", "2024-06-01", false},
		{"Empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ok := localTime(tt.ts, time.UTC)
			if ok != tt.valid {
				t.Errorf("localTime(%q) ok = %v, want %v",
					tt.ts, ok, tt.valid)
			}
		})
	}
}
