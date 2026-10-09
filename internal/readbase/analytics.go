package readbase

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

const topSessionsLimit = 10

// AnalyticsBackend supplies every SQL operation and backend-specific typed loader.
type AnalyticsBackend interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	Sessions(ctx context.Context, f db.AnalyticsFilter, includeDate, includeTime bool, extraPred string, extraArgs []any) ([]AnalyticsSession, error)
	Summary(ctx context.Context, f db.AnalyticsFilter) (db.AnalyticsSummary, bool, error)
	ActivityBucketsSQL(f db.AnalyticsFilter, granularity string) (string, []any)
	ActivityAgentsSQL(f db.AnalyticsFilter, granularity string) (string, []any)
	HeatmapSQL(f db.AnalyticsFilter, metric string) (string, []any)
	HourOfWeekSQL(f db.AnalyticsFilter) (string, []any)
	SummaryAgentsSQL(f db.AnalyticsFilter) (string, []any)
	VisitTools(ctx context.Context, f db.AnalyticsFilter, ids []string, emit func(sessionID, category, name, timestamp string, count int)) error
	VisitSkills(ctx context.Context, f db.AnalyticsFilter, ids []string, emit func(sessionID, name, timestamp string, count int)) error
	ToolSessionWindow(db.AnalyticsFilter) (string, []any)
	// TopSessionsSQL returns an ordered query without LIMIT or a trailing semicolon.
	TopSessionsSQL(f db.AnalyticsFilter, metric string, includeTime bool) (string, []any)
	ScanTopSession(*sql.Rows) (db.TopSession, error)
	TrendsSQL() string
	FormatTime(any) string
	MessageScope(ctx context.Context, ids []string, f db.AnalyticsFilter, includeContent bool) (db.MessageScope, error)
	ModelsSQL(ids []string) (string, []any)
	ModelTimesSQL(ids []string) (string, []any)
	Autonomy(ctx context.Context, ids []string, f db.AnalyticsFilter) (map[string]int, error)
	VelocityMessages(ctx context.Context, ids []string, f db.AnalyticsFilter, loc *time.Location) (map[string][]db.TimingMessage, error)
	VelocityToolCounts(ctx context.Context, ids []string, f db.AnalyticsFilter) (map[string]int, error)
	ToolCountsSQL(ids []string) (string, []any)
	SignalMessagesSQL(ids []string) (string, []any)
	CandidateMessagesSQL(ids []string, includeContent bool) (string, []any)
	// Session metadata supplies ClickHouse cache versions; DuckDB uses only rows.
	PopulateFrustrationMarkers(ctx context.Context, rows []db.SignalRow, sessions []AnalyticsSession) error
}

// Analytics owns DuckDB and ClickHouse report orchestration.
type Analytics struct {
	backend AnalyticsBackend
	name    string
}

func NewAnalytics(backend AnalyticsBackend, name string) *Analytics {
	return &Analytics{backend: backend, name: name}
}

// AnalyticsSession is the candidate row shared by reports and backend caches.
type AnalyticsSession struct {
	ID                          string
	Project                     string
	Machine                     string
	Agent                       string
	FirstMessage                *string
	DisplayName                 *string
	StartedAt                   string
	EndedAt                     string
	CreatedAt                   string
	MessageCount                int
	UserMessageCount            int
	TotalOutputTokens           int
	HasTotalOutputTokens        bool
	IsAutomated                 bool
	TerminationStatus           *string
	HealthScore                 *int
	HealthGrade                 *string
	Outcome                     string
	OutcomeConfidence           string
	ToolFailures                int
	ToolRetries                 int
	EditChurn                   int
	Compactions                 int
	MidTaskCompactions          int
	ContextPressureMax          *float64
	QualitySignalVersion        int
	ShortPromptCount            int
	UnstructuredStart           bool
	MissingSuccessCriteriaCount int
	MissingVerificationCount    int
	DuplicatePromptCount        int
	NoCodeContextCount          int
	RunawayToolLoopCount        int
	FrustrationMarkerCount      int
	PushVersion                 uint64 // ClickHouse cache version; zero for DuckDB.
}

func (s *Analytics) analyticsSessionsModelTimeFiltered(
	ctx context.Context, f db.AnalyticsFilter, includeDate bool,
) ([]AnalyticsSession, error) {
	sessions, err := s.analyticsSessionsFiltered(ctx, f, includeDate, false, "", nil)
	if err != nil {
		return nil, err
	}
	candidateIDs := make([]string, 0, len(sessions))
	for _, session := range sessions {
		candidateIDs = append(candidateIDs, session.ID)
	}
	scope, err := s.backend.MessageScope(ctx, candidateIDs, f, false)
	if err != nil {
		return nil, err
	}
	out := make([]AnalyticsSession, 0, len(sessions))
	for _, session := range sessions {
		if _, ok := scope[session.ID]; ok {
			out = append(out, session)
		}
	}
	return out, nil
}

func (s *Analytics) getAnalyticsFilteredMessageStats(
	ctx context.Context,
	sessionIDs []string,
	f db.AnalyticsFilter,
) (map[string]db.MessageStats, error) {
	scope, err := s.backend.MessageScope(ctx, sessionIDs, f, false)
	if err != nil {
		return nil, err
	}
	if scope == nil {
		return map[string]db.MessageStats{}, nil
	}
	return scope.StatsBySession(), nil
}

func (s *Analytics) analyticsSessionsWithModelMessageCounts(
	ctx context.Context, f db.AnalyticsFilter,
) ([]AnalyticsSession, error) {
	sessions, err := s.analyticsSessions(ctx, f)
	if err != nil || strings.TrimSpace(f.Model) == "" || len(sessions) == 0 {
		return sessions, err
	}

	sessionIDs := make([]string, 0, len(sessions))
	for _, session := range sessions {
		sessionIDs = append(sessionIDs, session.ID)
	}
	stats, err := s.getAnalyticsFilteredMessageStats(
		ctx, sessionIDs, f,
	)
	if err != nil {
		return nil, err
	}
	for i := range sessions {
		stat := stats[sessions[i].ID]
		sessions[i].MessageCount = stat.Messages
		sessions[i].TotalOutputTokens = stat.OutputTokens
		sessions[i].HasTotalOutputTokens = stat.HasOutputTokens
	}
	return sessions, nil
}

func (s *Analytics) getAnalyticsSummaryWithModelCounts(
	ctx context.Context, f db.AnalyticsFilter,
) (db.AnalyticsSummary, error) {
	sessions, err := s.analyticsSessionsWithModelMessageCounts(ctx, f)
	if err != nil {
		return db.AnalyticsSummary{}, err
	}

	resp := db.AnalyticsSummary{
		Agents: map[string]*db.AgentSummary{},
		Models: []string{},
	}
	if len(sessions) == 0 {
		return resp, nil
	}

	days := map[string]bool{}
	projects := map[string]int{}
	msgCounts := make([]int, 0, len(sessions))
	sessionIDs := make([]string, 0, len(sessions))

	for _, session := range sessions {
		date := AnalyticsLocalDate(AnalyticsDateTime(session), f.Timezone)
		resp.TotalSessions++
		resp.TotalMessages += session.MessageCount
		if session.HasTotalOutputTokens {
			resp.TotalOutputTokens += session.TotalOutputTokens
			resp.TokenReportingSessions++
		}
		days[date] = true
		projects[session.Project] += session.MessageCount
		msgCounts = append(msgCounts, session.MessageCount)
		sessionIDs = append(sessionIDs, session.ID)

		if resp.Agents[session.Agent] == nil {
			resp.Agents[session.Agent] = &db.AgentSummary{}
		}
		resp.Agents[session.Agent].Sessions++
		resp.Agents[session.Agent].Messages += session.MessageCount
	}

	var models []string
	if strings.TrimSpace(f.Model) != "" {
		models, err = s.filteredModels(
			ctx, sessionIDs, f,
		)
	} else {
		models, err = s.models(ctx, sessionIDs)
	}
	if err != nil {
		return db.AnalyticsSummary{}, err
	}
	resp.Models = models
	resp.ActiveProjects = len(projects)
	resp.ActiveDays = len(days)
	resp.AvgMessages = db.Round1(float64(resp.TotalMessages) / float64(resp.TotalSessions))

	sort.Ints(msgCounts)
	resp.MedianMessages = db.MedianInt(msgCounts, len(msgCounts))
	if n := len(msgCounts); n > 0 {
		resp.P90Messages = msgCounts[min(int(math.Floor(float64(n)*0.9))+1, n)-1]
	}

	maxMsgs := -1
	for _, name := range db.SortedKeys(projects) {
		if projects[name] > maxMsgs {
			maxMsgs = projects[name]
			resp.MostActive = name
		}
	}

	if resp.TotalMessages > 0 {
		counts := make([]int, 0, len(projects))
		for _, count := range projects {
			counts = append(counts, count)
		}
		sort.Sort(sort.Reverse(sort.IntSlice(counts)))
		topSum := 0
		for _, count := range counts[:min(3, len(counts))] {
			topSum += count
		}
		resp.Concentration = math.Round(
			float64(topSum)/float64(resp.TotalMessages)*1000,
		) / 1000
	}
	return resp, nil
}

func (s *Analytics) getAnalyticsActivityFilteredByModelTime(
	ctx context.Context, f db.AnalyticsFilter, granularity string,
) (db.ActivityResponse, error) {
	sessions, err := s.analyticsSessions(ctx, f)
	if err != nil {
		return db.ActivityResponse{}, err
	}
	sessionIDs := make([]string, 0, len(sessions))
	for _, session := range sessions {
		sessionIDs = append(sessionIDs, session.ID)
	}
	messageStats, err := s.getAnalyticsFilteredMessageStats(
		ctx, sessionIDs, f,
	)
	if err != nil {
		return db.ActivityResponse{}, err
	}
	toolCounts, err := s.filteredToolCounts(
		ctx, sessionIDs, f,
	)
	if err != nil {
		return db.ActivityResponse{}, err
	}

	out := db.ActivityResponse{Granularity: granularity}
	buckets := map[string]*db.ActivityEntry{}
	for _, session := range sessions {
		date := db.BucketDate(
			AnalyticsLocalDate(AnalyticsDateTime(session), f.Timezone),
			granularity,
		)
		entry := buckets[date]
		if entry == nil {
			entry = &db.ActivityEntry{
				Date:    date,
				ByAgent: map[string]int{},
			}
			buckets[date] = entry
		}
		entry.Sessions++
		stat := messageStats[session.ID]
		entry.Messages += stat.Messages
		entry.UserMessages += stat.UserMessages
		entry.AssistantMessages += stat.AssistantMessages
		entry.ThinkingMessages += stat.ThinkingMessages
		entry.ToolCalls += toolCounts[session.ID]
		entry.ByAgent[session.Agent] += stat.Messages
	}

	for _, key := range db.SortedKeys(buckets) {
		entry := buckets[key]
		if entry == nil {
			continue
		}
		out.Series = append(out.Series, *entry)
	}
	return out, nil
}

func (s *Analytics) GetAnalyticsActivity(
	ctx context.Context, f db.AnalyticsFilter, granularity string,
) (db.ActivityResponse, error) {
	if granularity == "" {
		granularity = "day"
	}
	if strings.TrimSpace(f.Model) != "" {
		return s.getAnalyticsActivityFilteredByModelTime(
			ctx, f, granularity,
		)
	}
	buckets, err := s.queryActivityBuckets(ctx, f, granularity)
	if err != nil {
		return db.ActivityResponse{}, err
	}
	if err := s.addActivityAgentCounts(ctx, f, granularity, buckets); err != nil {
		return db.ActivityResponse{}, err
	}
	out := db.ActivityResponse{Granularity: granularity}
	keys := db.SortedKeys(buckets)
	for _, key := range keys {
		entry, ok := buckets[key]
		if !ok || entry == nil {
			continue
		}
		out.Series = append(out.Series, *entry)
	}
	return out, nil
}

func (s *Analytics) GetAnalyticsProjects(
	ctx context.Context, f db.AnalyticsFilter,
) (db.ProjectsAnalyticsResponse, error) {
	// Per-project aggregate: count subagent sessions (mirrors SQLite).
	f.IncludeSubagents = true
	sessions, err := s.analyticsSessionsWithModelMessageCounts(ctx, f)
	if err != nil {
		return db.ProjectsAnalyticsResponse{}, err
	}
	type acc struct {
		row    db.ProjectAnalytics
		counts []int
		days   map[string]int
	}
	byProject := map[string]*acc{}
	for _, r := range sessions {
		a := byProject[r.Project]
		if a == nil {
			a = &acc{
				row:  db.ProjectAnalytics{Name: r.Project, Agents: map[string]int{}},
				days: map[string]int{},
			}
			byProject[r.Project] = a
		}
		date := AnalyticsLocalDate(AnalyticsDateTime(r), f.Timezone)
		if a.row.FirstSession == "" || date < a.row.FirstSession {
			a.row.FirstSession = date
		}
		if date > a.row.LastSession {
			a.row.LastSession = date
		}
		a.row.Sessions++
		a.row.Messages += r.MessageCount
		a.row.Agents[r.Agent]++
		a.counts = append(a.counts, r.MessageCount)
		a.days[date] += r.MessageCount
	}
	resp := db.ProjectsAnalyticsResponse{}
	for _, name := range db.SortedKeys(byProject) {
		a, ok := byProject[name]
		if !ok || a == nil {
			continue
		}
		sort.Ints(a.counts)
		a.row.AvgMessages = db.Round1(float64(a.row.Messages) / float64(a.row.Sessions))
		a.row.MedianMessages = db.MedianInt(a.counts, len(a.counts))
		if len(a.days) > 0 {
			a.row.DailyTrend = db.Round1(float64(a.row.Messages) / float64(len(a.days)))
		}
		resp.Projects = append(resp.Projects, a.row)
	}
	sort.Slice(resp.Projects, func(i, j int) bool {
		if resp.Projects[i].Messages != resp.Projects[j].Messages {
			return resp.Projects[i].Messages > resp.Projects[j].Messages
		}
		return resp.Projects[i].Name < resp.Projects[j].Name
	})
	return resp, nil
}

func (s *Analytics) getAnalyticsHourOfWeekFilteredByModel(
	ctx context.Context, f db.AnalyticsFilter,
) (db.HourOfWeekResponse, error) {
	sessions, err := s.analyticsSessionsFiltered(ctx, f, true, false, "", nil)
	if err != nil {
		return db.HourOfWeekResponse{}, err
	}
	sessionIDs := make([]string, 0, len(sessions))
	for _, session := range sessions {
		sessionIDs = append(sessionIDs, session.ID)
	}

	scopeFilter := f
	scopeFilter.DayOfWeek = nil
	scopeFilter.Hour = nil
	scope, err := s.backend.MessageScope(
		ctx, sessionIDs, scopeFilter, false,
	)
	if err != nil {
		return db.HourOfWeekResponse{}, err
	}

	var grid [7][24]int
	for _, msgs := range scope {
		for _, m := range msgs {
			if !m.HasLocalTime {
				continue
			}
			dow := (int(m.LocalTime.Weekday()) + 6) % 7
			grid[dow][m.LocalTime.Hour()]++
		}
	}

	return db.HourOfWeekResponseFromGrid(grid), nil
}

func (s *Analytics) GetAnalyticsSessionShape(
	ctx context.Context, f db.AnalyticsFilter,
) (db.SessionShapeResponse, error) {
	sessions, err := s.analyticsSessions(ctx, f)
	if err != nil {
		return db.SessionShapeResponse{}, err
	}
	modelFilter := strings.TrimSpace(f.Model) != ""
	lengths := map[string]int{}
	durations := map[string]int{}
	ids := []string{}
	for _, r := range sessions {
		ids = append(ids, r.ID)
		if !modelFilter {
			lengths[db.LengthBucket(r.MessageCount)]++
		}
		if start, okS := ParseAnalyticsTime(r.StartedAt); okS {
			if end, okE := ParseAnalyticsTime(r.EndedAt); okE && !end.Before(start) {
				durations[db.DurationBucket(end.Sub(start).Minutes())]++
			}
		}
	}
	autonomy := map[string]int{}
	if modelFilter && len(ids) > 0 {
		stats, err := s.getAnalyticsFilteredMessageStats(ctx, ids, f)
		if err != nil {
			return db.SessionShapeResponse{}, err
		}
		lengths = map[string]int{}
		for _, r := range sessions {
			stat := stats[r.ID]
			lengths[db.LengthBucket(stat.Messages)]++
			if stat.UserMessages > 0 {
				ratio := float64(stat.ToolUseMessages) /
					float64(stat.UserMessages)
				autonomy[db.AutonomyBucket(ratio)]++
			}
		}
	} else if len(ids) > 0 {
		autonomy, err = s.backend.Autonomy(ctx, ids, f)
		if err != nil {
			return db.SessionShapeResponse{}, err
		}
	}
	return db.SessionShapeResponse{
		Count:                len(sessions),
		LengthDistribution:   db.LengthDistributionBuckets(lengths),
		DurationDistribution: db.DurationDistributionBuckets(durations),
		AutonomyDistribution: db.AutonomyDistributionBuckets(autonomy),
	}, nil
}

func (s *Analytics) GetAnalyticsSignals(
	ctx context.Context, f db.AnalyticsFilter,
) (db.SignalsAnalyticsResponse, error) {
	sessions, err := s.analyticsSessions(ctx, f)
	if err != nil {
		return db.SignalsAnalyticsResponse{}, err
	}
	rows := signalRowsFromSessions(sessions, f)
	if err := s.backend.PopulateFrustrationMarkers(ctx, rows, sessions); err != nil {
		return db.SignalsAnalyticsResponse{}, err
	}
	return db.AggregateSignals(rows), nil
}

func (s *Analytics) GetAnalyticsSignalSessions(
	ctx context.Context,
	f db.AnalyticsFilter,
	signal string,
	limit int,
) (db.SignalSessionsResponse, error) {
	if !db.IsSupportedAnalyticsSignal(signal) {
		return db.SignalSessionsResponse{}, db.ErrUnsupportedAnalyticsSignal
	}
	if limit <= 0 || limit > 20 {
		limit = 10
	}
	sessions, err := s.analyticsSessions(ctx, f)
	if err != nil {
		return db.SignalSessionsResponse{}, err
	}
	rows := signalRowsFromSessions(sessions, f)
	if err := s.backend.PopulateFrustrationMarkers(ctx, rows, sessions); err != nil {
		return db.SignalSessionsResponse{}, err
	}
	candidates := db.SignalCandidates(rows, signal, limit)
	messages, err := s.signalMessages(ctx, candidates, f)
	if err != nil {
		return db.SignalSessionsResponse{}, err
	}
	return db.SignalSessionsResponse{
		Signal:   signal,
		Sessions: db.BuildSignalExamples(candidates, messages, signal),
	}, nil
}

func (s *Analytics) analyticsSessions(ctx context.Context, f db.AnalyticsFilter) ([]AnalyticsSession, error) {
	return s.analyticsSessionsFiltered(ctx, f, true, true, "", nil)
}

func (s *Analytics) analyticsSessionsFiltered(ctx context.Context, f db.AnalyticsFilter, includeDate, includeTime bool, extraPred string, extraArgs []any) ([]AnalyticsSession, error) {
	if includeTime && f.HasTimeFilter() && strings.TrimSpace(f.Model) != "" {
		return s.analyticsSessionsModelTimeFiltered(ctx, f, includeDate)
	}
	return s.backend.Sessions(ctx, f, includeDate, includeTime, extraPred, extraArgs)
}

func (s *Analytics) GetAnalyticsSummary(
	ctx context.Context, f db.AnalyticsFilter,
) (db.AnalyticsSummary, error) {
	// Sum/count aggregate: count subagent sessions (mirrors SQLite).
	f.IncludeSubagents = true
	if strings.TrimSpace(f.Model) != "" {
		return s.getAnalyticsSummaryWithModelCounts(ctx, f)
	}
	resp, found, err := s.backend.Summary(ctx, f)
	if err != nil || !found {
		return resp, err
	}

	agentQuery, agentArgs := s.backend.SummaryAgentsSQL(f)
	agentRows, err := s.backend.QueryContext(ctx, agentQuery, agentArgs...)
	if err != nil {
		return db.AnalyticsSummary{}, fmt.Errorf("querying %s analytics summary agents: %w", s.name, err)
	}
	defer agentRows.Close()
	for agentRows.Next() {
		var agent string
		var summary db.AgentSummary
		if err := agentRows.Scan(&agent, &summary.Sessions, &summary.Messages); err != nil {
			return db.AnalyticsSummary{}, fmt.Errorf("scanning %s analytics summary agent: %w", s.name, err)
		}
		resp.Agents[agent] = &summary
	}
	if err := agentRows.Err(); err != nil {
		return db.AnalyticsSummary{}, fmt.Errorf("iterating %s analytics summary agents: %w", s.name, err)
	}
	sessions, err := s.analyticsSessions(ctx, f)
	if err != nil {
		return db.AnalyticsSummary{}, err
	}
	sessionIDs := make([]string, 0, len(sessions))
	for _, sess := range sessions {
		sessionIDs = append(sessionIDs, sess.ID)
	}
	var models []string
	if f.HasTimeFilter() {
		models, err = s.filteredModels(
			ctx, sessionIDs, f,
		)
	} else {
		models, err = s.models(
			ctx, sessionIDs,
		)
	}
	if err != nil {
		return db.AnalyticsSummary{}, err
	}
	resp.Models = models
	return resp, nil
}

func (s *Analytics) queryActivityBuckets(
	ctx context.Context, f db.AnalyticsFilter, granularity string,
) (map[string]*db.ActivityEntry, error) {
	query, queryArgs := s.backend.ActivityBucketsSQL(f, granularity)
	rows, err := s.backend.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("querying %s analytics activity buckets: %w", s.name, err)
	}
	defer rows.Close()
	buckets := map[string]*db.ActivityEntry{}
	for rows.Next() {
		entry := db.ActivityEntry{ByAgent: map[string]int{}}
		if err := rows.Scan(
			&entry.Date,
			&entry.Sessions,
			&entry.Messages,
			&entry.UserMessages,
			&entry.AssistantMessages,
			&entry.ThinkingMessages,
			&entry.ToolCalls,
		); err != nil {
			return nil, fmt.Errorf("scanning %s analytics activity bucket: %w", s.name, err)
		}
		buckets[entry.Date] = &entry
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating %s analytics activity buckets: %w", s.name, err)
	}
	return buckets, nil
}

func (s *Analytics) addActivityAgentCounts(
	ctx context.Context, f db.AnalyticsFilter, granularity string,
	buckets map[string]*db.ActivityEntry,
) error {
	query, queryArgs := s.backend.ActivityAgentsSQL(f, granularity)
	rows, err := s.backend.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return fmt.Errorf("querying %s analytics activity agents: %w", s.name, err)
	}
	defer rows.Close()
	for rows.Next() {
		var bucket, agent string
		var count int
		if err := rows.Scan(&bucket, &agent, &count); err != nil {
			return fmt.Errorf("scanning %s analytics activity agent: %w", s.name, err)
		}
		if entry, ok := buckets[bucket]; ok {
			entry.ByAgent[agent] = count
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating %s analytics activity agents: %w", s.name, err)
	}
	return nil
}

func (s *Analytics) GetAnalyticsHeatmap(
	ctx context.Context, f db.AnalyticsFilter, metric string,
) (db.HeatmapResponse, error) {
	if metric == "" {
		metric = "messages"
	}
	if strings.TrimSpace(f.Model) != "" &&
		(metric == "messages" || metric == "output_tokens" ||
			metric == "sessions") {
		sessions, err := s.analyticsSessionsWithModelMessageCounts(ctx, f)
		if err != nil {
			return db.HeatmapResponse{}, err
		}
		counts := map[string]int{}
		for _, session := range sessions {
			date := AnalyticsLocalDate(AnalyticsDateTime(session), f.Timezone)
			switch metric {
			case "sessions":
				counts[date]++
			case "output_tokens":
				if session.HasTotalOutputTokens {
					counts[date] += session.TotalOutputTokens
				}
			default:
				counts[date] += session.MessageCount
			}
		}
		return db.BuildHeatmapResponse(f.From, f.To, metric, counts), nil
	}
	query, queryArgs := s.backend.HeatmapSQL(f, metric)
	rows, err := s.backend.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return db.HeatmapResponse{}, fmt.Errorf("querying %s analytics heatmap: %w", s.name, err)
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var date string
		var value int
		if err := rows.Scan(&date, &value); err != nil {
			return db.HeatmapResponse{}, fmt.Errorf("scanning %s analytics heatmap: %w", s.name, err)
		}
		counts[date] = value
	}
	if err := rows.Err(); err != nil {
		return db.HeatmapResponse{}, fmt.Errorf("iterating %s analytics heatmap: %w", s.name, err)
	}
	return db.BuildHeatmapResponse(f.From, f.To, metric, counts), nil
}

func (s *Analytics) GetAnalyticsHourOfWeek(
	ctx context.Context, f db.AnalyticsFilter,
) (db.HourOfWeekResponse, error) {
	if strings.TrimSpace(f.Model) != "" {
		return s.getAnalyticsHourOfWeekFilteredByModel(ctx, f)
	}
	query, queryArgs := s.backend.HourOfWeekSQL(f)
	rows, err := s.backend.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return db.HourOfWeekResponse{}, fmt.Errorf("querying %s analytics hour-of-week: %w", s.name, err)
	}
	defer rows.Close()
	var grid [7][24]int
	for rows.Next() {
		var day, hour, messages int
		if err := rows.Scan(&day, &hour, &messages); err != nil {
			return db.HourOfWeekResponse{}, fmt.Errorf("scanning %s analytics hour-of-week: %w", s.name, err)
		}
		if day < 0 || day > 6 || hour < 0 || hour > 23 {
			continue
		}
		grid[day][hour] = messages
	}
	if err := rows.Err(); err != nil {
		return db.HourOfWeekResponse{}, fmt.Errorf("iterating %s analytics hour-of-week: %w", s.name, err)
	}
	return db.HourOfWeekResponseFromGrid(grid), nil
}

func (s *Analytics) GetAnalyticsTools(ctx context.Context, f db.AnalyticsFilter) (db.ToolsAnalyticsResponse, error) {
	ids, meta, err := s.toolSessions(ctx, f)
	if err != nil {
		return db.ToolsAnalyticsResponse{}, err
	}
	if len(ids) == 0 {
		return db.BuildToolsAnalytics(nil), nil
	}
	var rows []db.ToolAnalyticsRow
	err = s.backend.VisitTools(ctx, f, ids, func(id, category, name, timestamp string, count int) {
		session, ok := meta[id]
		if !ok {
			return
		}
		_, date, keep := f.ResolveSkillRowTime(timestamp, AnalyticsDateTime(session))
		if !keep {
			return
		}
		rows = append(rows, db.ToolAnalyticsRow{SessionID: id, Category: category, ToolName: name, Agent: session.Agent, Count: count, Date: date})
	})
	if err != nil {
		return db.ToolsAnalyticsResponse{}, err
	}
	return db.BuildToolsAnalytics(rows), nil
}

func (s *Analytics) GetAnalyticsSkills(ctx context.Context, f db.AnalyticsFilter, granularity string) (db.SkillsAnalyticsResponse, error) {
	ids, meta, err := s.toolSessions(ctx, f)
	if err != nil {
		return db.SkillsAnalyticsResponse{}, err
	}
	if len(ids) == 0 {
		return db.BuildSkillsAnalytics(nil, f.From, f.To, granularity), nil
	}
	var rows []db.SkillAnalyticsRow
	err = s.backend.VisitSkills(ctx, f, ids, func(id, name, timestamp string, count int) {
		session, ok := meta[id]
		if !ok {
			return
		}
		usedTS, date, keep := f.ResolveSkillRowTime(timestamp, AnalyticsDateTime(session))
		if !keep {
			return
		}
		rows = append(rows, db.SkillAnalyticsRow{SessionID: id, SkillName: name, Agent: session.Agent, Project: session.Project, Date: date, LastUsedAt: usedTS, Count: count})
	})
	if err != nil {
		return db.SkillsAnalyticsResponse{}, err
	}
	return db.BuildSkillsAnalytics(rows, f.From, f.To, granularity), nil
}

func (s *Analytics) toolSessions(ctx context.Context, f db.AnalyticsFilter) ([]string, map[string]AnalyticsSession, error) {
	pred, args := s.backend.ToolSessionWindow(f)
	sessions, err := s.analyticsSessionsFiltered(ctx, f, false, false, pred, args)
	if err != nil {
		return nil, nil, err
	}
	ids := make([]string, 0, len(sessions))
	meta := make(map[string]AnalyticsSession, len(sessions))
	for _, session := range sessions {
		ids = append(ids, session.ID)
		meta[session.ID] = session
	}
	return ids, meta, nil
}

func (s *Analytics) GetAnalyticsTopSessions(
	ctx context.Context, f db.AnalyticsFilter, metric string,
) (db.TopSessionsResponse, error) {
	switch metric {
	case "", "messages":
		metric = "messages"
	case "duration", "output_tokens":
	default:
		metric = "messages"
	}

	if strings.TrimSpace(f.Model) != "" &&
		(metric == "messages" || metric == "output_tokens") {
		sessions, err := s.analyticsSessionsWithModelMessageCounts(ctx, f)
		if err != nil {
			return db.TopSessionsResponse{}, err
		}
		sort.SliceStable(sessions, func(i, j int) bool {
			if metric == "output_tokens" {
				if sessions[i].TotalOutputTokens != sessions[j].TotalOutputTokens {
					return sessions[i].TotalOutputTokens >
						sessions[j].TotalOutputTokens
				}
			} else if sessions[i].MessageCount != sessions[j].MessageCount {
				return sessions[i].MessageCount >
					sessions[j].MessageCount
			}
			return sessions[i].ID < sessions[j].ID
		})

		out := db.TopSessionsResponse{Metric: metric}
		for i := range sessions {
			if metric == "output_tokens" &&
				!sessions[i].HasTotalOutputTokens {
				continue
			}
			if len(out.Sessions) >= topSessionsLimit {
				break
			}
			startedAt := sessions[i].StartedAt
			endedAt := sessions[i].EndedAt
			out.Sessions = append(out.Sessions, db.TopSession{
				ID:                sessions[i].ID,
				Project:           sessions[i].Project,
				FirstMessage:      sessions[i].FirstMessage,
				DisplayName:       sessions[i].DisplayName,
				MessageCount:      sessions[i].MessageCount,
				OutputTokens:      sessions[i].TotalOutputTokens,
				DurationMin:       sessionDurationMinutes(sessions[i]),
				StartedAt:         &startedAt,
				EndedAt:           &endedAt,
				TerminationStatus: sessions[i].TerminationStatus,
			})
		}
		return out, nil
	}

	includeTime := true
	var pairedSet map[string]bool
	if f.HasTimeFilter() && strings.TrimSpace(f.Model) != "" {
		// Filtering paired IDs after the query avoids unbounded SQL parameter lists.
		paired, err := s.analyticsSessionsModelTimeFiltered(ctx, f, true)
		if err != nil {
			return db.TopSessionsResponse{}, err
		}
		if len(paired) == 0 {
			return db.TopSessionsResponse{Metric: metric}, nil
		}
		pairedSet = make(map[string]bool, len(paired))
		for _, session := range paired {
			pairedSet[session.ID] = true
		}
		includeTime = false
	}
	query, args := s.backend.TopSessionsSQL(f, metric, includeTime)
	if pairedSet == nil {
		query += fmt.Sprintf("\n\t\tLIMIT %d", topSessionsLimit)
	}
	rows, err := s.backend.QueryContext(ctx, query, args...)
	if err != nil {
		return db.TopSessionsResponse{}, fmt.Errorf("querying %s analytics top sessions: %w", s.name, err)
	}
	defer rows.Close()
	out := db.TopSessionsResponse{Metric: metric}
	for rows.Next() {
		row, err := s.backend.ScanTopSession(rows)
		if err != nil {
			return db.TopSessionsResponse{}, err
		}
		if pairedSet != nil && !pairedSet[row.ID] {
			continue
		}
		row.DurationMin = db.Round1(row.DurationMin)
		row.ActiveDurationMin = db.Round1(row.ActiveDurationMin)
		out.Sessions = append(out.Sessions, row)
		if pairedSet != nil && len(out.Sessions) >= topSessionsLimit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return db.TopSessionsResponse{}, fmt.Errorf("iterating %s analytics top sessions: %w", s.name, err)
	}
	return out, nil
}

func (s *Analytics) GetTrendsTerms(
	ctx context.Context, f db.AnalyticsFilter,
	terms []db.TrendTermInput, granularity string,
) (db.TrendsTermsResponse, error) {
	if granularity == "" {
		granularity = "week"
	}
	acc := db.NewTrendAccumulator(f.From, f.To, granularity, terms)
	sessionFilter := f
	sessionFilter.From = ""
	sessionFilter.To = ""
	sessionFilter.Model = ""
	sessionFilter.DayOfWeek = nil
	sessionFilter.Hour = nil
	sessions, err := s.analyticsSessions(ctx, sessionFilter)
	if err != nil {
		return db.TrendsTermsResponse{}, err
	}
	allowedSessions := make(map[string]bool, len(sessions))
	for _, sess := range sessions {
		allowedSessions[sess.ID] = true
	}
	if len(allowedSessions) == 0 {
		return acc.Response(), nil
	}
	loc := AnalyticsLocation(f.Timezone)
	flt := f.MessageScopeFilter()
	modelFiltering := len(flt.Models) > 0
	rows, err := s.backend.QueryContext(ctx, s.backend.TrendsSQL())
	if err != nil {
		return db.TrendsTermsResponse{}, err
	}
	defer rows.Close()
	type trendRow struct {
		sessionID string
		role      string
		isSystem  bool
		model     string
		content   string
		msgTS     any
		startedAt any
		createdAt any
	}
	processRow := func(sessionID, content string, local time.Time) {
		if !allowedSessions[sessionID] {
			return
		}
		acc.Add(content, local)
	}
	emit := func(m db.ScopedMessage) {
		if !m.HasLocalTime {
			return
		}
		processRow(m.SessionID, m.Content, m.LocalTime)
	}
	reducer := db.NewScopeReducer(flt, emit)
	for rows.Next() {
		var row trendRow
		var ordinal int
		if err := rows.Scan(&row.sessionID, &ordinal, &row.role, &row.isSystem, &row.model, &row.content, &row.msgTS, &row.startedAt, &row.createdAt); err != nil {
			return db.TrendsTermsResponse{}, err
		}
		ts := cmp.Or(s.backend.FormatTime(row.msgTS), s.backend.FormatTime(row.startedAt), s.backend.FormatTime(row.createdAt))
		local, has := AnalyticsLocalTime(ts, loc)
		if !modelFiltering {
			if has && flt.MatchesDayHour(local, true) {
				processRow(row.sessionID, row.content, local)
			}
			continue
		}
		if err := reducer.Push(db.MessageInput{
			SessionID:    row.sessionID,
			Ordinal:      ordinal,
			Role:         row.role,
			Model:        row.model,
			IsSystem:     row.isSystem,
			LocalTime:    local,
			HasLocalTime: has,
			Content:      row.content,
		}); err != nil {
			return db.TrendsTermsResponse{}, err
		}
	}
	if err := rows.Err(); err != nil {
		return db.TrendsTermsResponse{}, err
	}
	return acc.Response(), nil
}

func AnalyticsDateTime(r AnalyticsSession) string {
	if r.StartedAt != "" {
		return r.StartedAt
	}
	return r.CreatedAt
}

func AnalyticsLocalDate(ts, tz string) string {
	t, ok := ParseAnalyticsTime(ts)
	if !ok {
		return ""
	}
	return t.In(AnalyticsLocation(tz)).Format("2006-01-02")
}

func AnalyticsLocation(tz string) *time.Location {
	return db.LoadLocationOr(tz, time.UTC)
}

func ParseAnalyticsTime(ts string) (time.Time, bool) {
	trimmed := strings.TrimSpace(ts)
	for _, candidate := range []struct{ layout, value string }{
		{time.RFC3339Nano, trimmed},
		{"2006-01-02 15:04:05", trimmed},
		// The space-separated offset format historically rejects surrounding whitespace.
		{"2006-01-02 15:04:05.999999-07", ts},
	} {
		if t, err := time.Parse(candidate.layout, candidate.value); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

func sessionDurationMinutes(session AnalyticsSession) float64 {
	startedAt, okStart := ParseAnalyticsTime(session.StartedAt)
	endedAt, okEnd := ParseAnalyticsTime(session.EndedAt)
	if !okStart || !okEnd || endedAt.Before(startedAt) {
		return 0
	}
	return db.Round1(endedAt.Sub(startedAt).Minutes())
}

func signalRowsFromSessions(
	sessions []AnalyticsSession,
	f db.AnalyticsFilter,
) []db.SignalRow {
	rows := make([]db.SignalRow, 0, len(sessions))
	for _, r := range sessions {
		rows = append(rows, db.SignalRow{
			ID:                          r.ID,
			Agent:                       r.Agent,
			Project:                     r.Project,
			FirstMessage:                r.FirstMessage,
			IsAutomated:                 r.IsAutomated,
			Date:                        AnalyticsLocalDate(AnalyticsDateTime(r), f.Timezone),
			HealthScore:                 r.HealthScore,
			HealthGrade:                 r.HealthGrade,
			Outcome:                     r.Outcome,
			OutcomeConfidence:           r.OutcomeConfidence,
			ToolFailureSignalCount:      r.ToolFailures,
			ToolRetryCount:              r.ToolRetries,
			EditChurnCount:              r.EditChurn,
			CompactionCount:             r.Compactions,
			MidTaskCompactionCount:      r.MidTaskCompactions,
			ContextPressureMax:          r.ContextPressureMax,
			QualitySignalVersion:        r.QualitySignalVersion,
			ShortPromptCount:            r.ShortPromptCount,
			UnstructuredStart:           r.UnstructuredStart,
			MissingSuccessCriteriaCount: r.MissingSuccessCriteriaCount,
			MissingVerificationCount:    r.MissingVerificationCount,
			DuplicatePromptCount:        r.DuplicatePromptCount,
			NoCodeContextCount:          r.NoCodeContextCount,
			RunawayToolLoopCount:        r.RunawayToolLoopCount,
			FrustrationMarkerCount:      r.FrustrationMarkerCount,
		})
	}
	return rows
}

// ScanAnalyticsSummary reads and closes an aggregate result after its first Next.
func ScanAnalyticsSummary(rows *sql.Rows, resp db.AnalyticsSummary, backend string) (db.AnalyticsSummary, error) {
	if err := rows.Scan(
		&resp.TotalSessions,
		&resp.TotalMessages,
		&resp.TotalOutputTokens,
		&resp.TokenReportingSessions,
		&resp.ActiveProjects,
		&resp.ActiveDays,
		&resp.AvgMessages,
		&resp.MedianMessages,
		&resp.P90Messages,
		&resp.MostActive,
		&resp.Concentration,
	); err != nil {
		rows.Close()
		return db.AnalyticsSummary{}, fmt.Errorf("scanning %s analytics summary: %w", backend, err)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return db.AnalyticsSummary{}, fmt.Errorf("iterating %s analytics summary: %w", backend, err)
	}
	if err := rows.Close(); err != nil {
		return db.AnalyticsSummary{}, fmt.Errorf("closing %s analytics summary rows: %w", backend, err)
	}

	return resp, nil
}
