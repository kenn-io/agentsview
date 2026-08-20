package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
)

// ActivityReportConfig holds the flags for `agentsview activity report`.
type ActivityReportConfig struct {
	Preset            string
	Date              string
	From              string
	To                string
	Timezone          string
	Bucket            string
	Project           string
	Agent             string
	Machine           string
	JSON              bool
	NoSync            bool
	Offline           bool
	ProgressWriter    io.Writer
	SessionsLimit     int
	SessionsReportID  string
	SessionsCursor    string
	SessionsSort      string
	SessionsDirection string
	SessionsBucket    string
}

var activityReportNow = time.Now

// runActivityReport syncs, resolves the range, runs the report, and prints it.
func runActivityReport(cfg ActivityReportConfig) {
	cfg.ProgressWriter = os.Stderr
	ctx := context.Background()
	backend, cleanup, err := resolveArchiveQueryBackend(ctx, archiveQueryPolicy{
		Offline:              cfg.Offline,
		NoSync:               cfg.NoSync,
		ReadOnlyDaemon:       archiveQueryUseReadOnlyDaemon,
		DirectReadOnlyAction: "query activity directly",
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer closeArchiveQueryBackend(cleanup)

	r, err := backend.ActivityReport(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	writeActivityReport(r, cfg.JSON)
}

func writeActivityReport(r activity.Report, jsonOutput bool) {
	if jsonOutput {
		enc := jsontext.NewEncoder(os.Stdout, jsontext.WithIndent("  "))
		if err := json.MarshalEncode(enc, r); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	printActivityReport(r)
}

func fetchHTTPActivityReport(
	ctx context.Context, tr transport, authToken string, cfg ActivityReportConfig,
) (activity.Report, error) {
	if cfg.SessionsReportID != "" {
		return fetchHTTPActivitySessionPage(
			ctx, tr, authToken, cfg, activity.Report{},
		)
	}
	q := url.Values{}
	setIfNotEmpty := func(k, v string) {
		if v != "" {
			q.Set(k, v)
		}
	}
	setIfNotEmpty("preset", cfg.Preset)
	setIfNotEmpty("date", cfg.Date)
	setIfNotEmpty("from", cfg.From)
	setIfNotEmpty("to", cfg.To)
	setIfNotEmpty("timezone", cfg.Timezone)
	setIfNotEmpty("bucket", cfg.Bucket)
	setIfNotEmpty("project", cfg.Project)
	setIfNotEmpty("agent", cfg.Agent)
	setIfNotEmpty("machine", cfg.Machine)

	endpoint := strings.TrimSuffix(tr.URL, "/") +
		"/api/v1/activity/report?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return activity.Report{}, err
	}
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}
	req.Header.Set("Accept", "text/event-stream, application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return activity.Report{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return activity.Report{}, fmt.Errorf(
			"activity report: HTTP %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)),
		)
	}
	var r activity.Report
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		var onProgress func(activity.Progress)
		if cfg.ProgressWriter != nil {
			onProgress = newActivityProgressPrinter(cfg.ProgressWriter)
		}
		r, err = parseDaemonPushSSE[activity.Report, activity.Progress](resp.Body, onProgress)
		if err != nil {
			return activity.Report{}, err
		}
	} else if err := json.UnmarshalRead(resp.Body, &r); err != nil {
		return activity.Report{}, err
	}
	if r.Projects == nil {
		r.Projects = map[string]export.ProjectMapEntry{}
	}
	if activitySessionPageCustomized(cfg) {
		r, err = fetchHTTPActivitySessionPage(ctx, tr, authToken, cfg, r)
		if err != nil {
			return activity.Report{}, err
		}
	}
	return r, nil
}

func activitySessionPageCustomized(cfg ActivityReportConfig) bool {
	return cfg.SessionsReportID != "" || cfg.SessionsCursor != "" ||
		cfg.SessionsBucket != "" ||
		cfg.SessionsLimit > 0 && cfg.SessionsLimit != activity.DefaultSessionPageLimit ||
		cfg.SessionsSort != "" && cfg.SessionsSort != string(activity.SessionSortAgentMinutes) ||
		cfg.SessionsDirection != "" && cfg.SessionsDirection != "desc"
}

func fetchHTTPActivitySessionPage(
	ctx context.Context,
	tr transport,
	authToken string,
	cfg ActivityReportConfig,
	report activity.Report,
) (activity.Report, error) {
	reportID := report.ReportID
	if cfg.SessionsReportID != "" {
		reportID = cfg.SessionsReportID
	}
	if reportID == "" {
		return activity.Report{}, fmt.Errorf(
			"daemon does not support Activity session paging",
		)
	}
	options, err := activitySessionPageOptions(cfg, nil)
	if err != nil {
		return activity.Report{}, err
	}
	query := url.Values{}
	query.Set("limit", strconv.Itoa(options.Limit))
	if cfg.SessionsCursor != "" {
		query.Set("cursor", cfg.SessionsCursor)
		if cfg.SessionsSort != "" {
			query.Set("sort", string(options.Sort))
		}
		if cfg.SessionsDirection != "" {
			query.Set("direction", options.Direction)
		}
	} else {
		query.Set("sort", string(options.Sort))
		query.Set("direction", options.Direction)
	}
	if options.Bucket != nil {
		query.Set("bucket", strconv.Itoa(*options.Bucket))
	}
	if cfg.SessionsReportID != "" {
		query.Set("include_report", "true")
	}
	endpoint := strings.TrimSuffix(tr.URL, "/") + "/api/v1/activity/report/" +
		url.PathEscape(reportID) + "/sessions?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return activity.Report{}, err
	}
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return activity.Report{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		return activity.Report{}, fmt.Errorf(
			"activity sessions: HTTP %d: %s",
			response.StatusCode, strings.TrimSpace(string(body)),
		)
	}
	var page struct {
		ReportID        string                `json:"report_id"`
		Sessions        []activity.SessionRow `json:"sessions"`
		NextCursor      string                `json:"next_cursor"`
		Total           int                   `json:"total"`
		RefreshRequired bool                  `json:"refresh_required"`
		Report          *activity.Report      `json:"report"`
	}
	if err := json.UnmarshalRead(response.Body, &page); err != nil {
		return activity.Report{}, err
	}
	if page.Report != nil {
		return *page.Report, nil
	}
	if cfg.SessionsReportID != "" {
		return activity.Report{}, fmt.Errorf(
			"daemon did not return the requested Activity report generation",
		)
	}
	report.BySession = page.Sessions
	report.SessionsNextCursor = page.NextCursor
	report.SessionsTotal = page.Total
	return report, nil
}

func newActivityProgressPrinter(writer io.Writer) func(activity.Progress) {
	var lastPhase activity.ProgressPhase
	var lastRows int64
	return func(progress activity.Progress) {
		if progress.Phase == lastPhase && progress.RowsProcessed-lastRows < 10_000 {
			return
		}
		lastPhase, lastRows = progress.Phase, progress.RowsProcessed
		switch progress.Phase {
		case activity.ProgressScanningActivity:
			fmt.Fprintf(writer, "activity: scanning rows (%d processed)\n", progress.RowsProcessed)
		case activity.ProgressFinalizing, activity.ProgressDone:
			fmt.Fprintf(writer, "activity: %s (%d sessions)\n",
				strings.ReplaceAll(string(progress.Phase), "_", " "),
				progress.SessionsProcessed)
		default:
			fmt.Fprintf(writer, "activity: %s\n",
				strings.ReplaceAll(string(progress.Phase), "_", " "))
		}
	}
}

type cliActivitySessionCursor struct {
	Version   int                     `json:"v"`
	Schema    int                     `json:"schema"`
	Digest    string                  `json:"digest"`
	Offset    int                     `json:"offset"`
	Sort      activity.SessionSort    `json:"sort"`
	Direction string                  `json:"direction"`
	Bucket    *int                    `json:"bucket,omitempty"`
	Query     cliActivityCursorQuery  `json:"query"`
	Filter    cliActivityCursorFilter `json:"filter"`
}

type cliActivityCursorQuery struct {
	Timezone      string              `json:"timezone"`
	RangeStart    time.Time           `json:"range_start"`
	RangeEnd      time.Time           `json:"range_end"`
	EffectiveEnd  time.Time           `json:"effective_end"`
	Partial       bool                `json:"partial"`
	Bucket        activity.BucketSpec `json:"bucket"`
	GapCapSeconds float64             `json:"gap_cap_seconds"`
}

type cliActivityCursorFilter struct {
	Timezone         string `json:"timezone"`
	Project          string `json:"project,omitempty"`
	Agent            string `json:"agent,omitempty"`
	Machine          string `json:"machine,omitempty"`
	ExcludeOneShot   bool   `json:"exclude_one_shot"`
	ExcludeAutomated bool   `json:"exclude_automated"`
}

func activitySessionPageOptions(
	cfg ActivityReportConfig,
	cursor *cliActivitySessionCursor,
) (activity.SessionPageOptions, error) {
	options := activity.SessionPageOptions{
		Limit: cfg.SessionsLimit, Sort: activity.SessionSort(cfg.SessionsSort),
		Direction: cfg.SessionsDirection,
	}
	if cfg.SessionsBucket != "" {
		bucket, err := strconv.Atoi(cfg.SessionsBucket)
		if err != nil || bucket < 0 {
			return activity.SessionPageOptions{}, fmt.Errorf(
				"invalid sessions bucket %q", cfg.SessionsBucket,
			)
		}
		options.Bucket = &bucket
	}
	var continuation *activity.SessionPageOptions
	if cursor != nil {
		continuation = &activity.SessionPageOptions{
			Sort: cursor.Sort, Direction: cursor.Direction, Bucket: cursor.Bucket,
		}
	}
	resolved, err := activity.ResolveSessionPageOptions(
		options, continuation, activity.SessionPageOptionPresence{
			Sort: cfg.SessionsSort != "", Direction: cfg.SessionsDirection != "",
			Bucket: cfg.SessionsBucket != "",
		},
	)
	if err != nil && cursor != nil {
		return activity.SessionPageOptions{}, fmt.Errorf("invalid sessions cursor")
	}
	return resolved, err
}

// resolveActivityReportPriced seeds fallback pricing so fresh-DB token usage is
// costed, then resolves the report. runActivityReport and the pricing test
// share this seam so the test exercises the same seeding the command performs.
func resolveActivityReportPriced(
	cfg ActivityReportConfig, database *db.DB,
	customPricing map[string]config.CustomModelRate,
) (activity.Report, error) {
	ensureUsagePricing(database, cfg.Offline, customPricing)
	return resolveActivityReport(cfg, database)
}

// resolveActivityReport defaults the timezone and date, resolves the range
// query, and runs the report against the database. It is the testable seam:
// all validation (timezone, bounds, bucket allow-list, range limits) happens
// inside activity.ResolveQuery before any database query.
func resolveActivityReport(
	cfg ActivityReportConfig, database *db.DB,
) (activity.Report, error) {
	var q activity.Query
	var f db.AnalyticsFilter
	var cursor *cliActivitySessionCursor
	if cfg.SessionsCursor != "" {
		decoded, decodeErr := decodeCLIActivitySessionCursor(database, cfg.SessionsCursor)
		if decodeErr != nil {
			return activity.Report{}, decodeErr
		}
		cursor = &decoded
	}
	options, err := activitySessionPageOptions(cfg, cursor)
	if err != nil {
		return activity.Report{}, err
	}
	if cursor != nil {
		q, f, err = cursor.selection()
		if err != nil {
			return activity.Report{}, fmt.Errorf("invalid sessions cursor")
		}
		options.Offset = cursor.Offset
	} else {
		q, f, err = resolveCLIActivitySelection(cfg)
		if err != nil {
			return activity.Report{}, err
		}
	}

	var onProgress activity.ProgressFunc
	if cfg.ProgressWriter != nil {
		onProgress = newActivityProgressPrinter(cfg.ProgressWriter)
	}
	artifacts, err := database.BuildActivityReportArtifacts(
		context.Background(), f, q, onProgress,
	)
	if err != nil {
		return activity.Report{}, err
	}
	digest, err := activity.ArtifactDigest(artifacts)
	if err != nil {
		return activity.Report{}, err
	}
	if cursor != nil && cursor.Digest != digest {
		return activity.Report{}, fmt.Errorf("invalid sessions cursor")
	}
	page, err := activity.PageSessions(artifacts.Sessions, artifacts.Membership, options)
	if err != nil {
		return activity.Report{}, err
	}
	report := artifacts.Report
	report.BySession = page.Sessions
	report.SessionsTotal = page.Total
	if page.HasNext {
		payload, marshalErr := json.Marshal(newCLIActivitySessionCursor(
			digest, page.Next, options, q, f,
		))
		if marshalErr != nil {
			return activity.Report{}, marshalErr
		}
		report.SessionsNextCursor, err = database.EncodeActivityReportToken(payload)
		if err != nil {
			return activity.Report{}, err
		}
	}
	return report, nil
}

func resolveCLIActivitySelection(
	cfg ActivityReportConfig,
) (activity.Query, db.AnalyticsFilter, error) {
	tz := cfg.Timezone
	if tz == "" {
		tz = localTimezone()
	}

	date := cfg.Date
	if cfg.Preset != "custom" && cfg.From == "" && date == "" {
		date = todayIn(tz)
	}

	input := activity.QueryInput{
		Preset:         cfg.Preset,
		Date:           date,
		From:           cfg.From,
		To:             cfg.To,
		Timezone:       tz,
		BucketOverride: cfg.Bucket,
	}
	q, err := activity.ResolveQuery(input, activityReportNow())
	if err != nil {
		return activity.Query{}, db.AnalyticsFilter{}, err
	}

	f := db.AnalyticsFilter{
		Timezone:         tz,
		Project:          cfg.Project,
		Agent:            cfg.Agent,
		Machine:          cfg.Machine,
		ExcludeOneShot:   false,
		ExcludeAutomated: false,
	}
	return q, f, nil
}

func decodeCLIActivitySessionCursor(
	database *db.DB,
	token string,
) (cliActivitySessionCursor, error) {
	payload, err := database.DecodeActivityReportToken(token)
	if err != nil {
		return cliActivitySessionCursor{}, fmt.Errorf("invalid sessions cursor: %w", err)
	}
	var cursor cliActivitySessionCursor
	if err := json.Unmarshal(payload, &cursor); err != nil ||
		cursor.Version != 2 || cursor.Schema != export.ActivityReportSchemaVersion ||
		cursor.Offset < 0 || cursor.Digest == "" {
		return cliActivitySessionCursor{}, fmt.Errorf("invalid sessions cursor")
	}
	return cursor, nil
}

func (cursor cliActivitySessionCursor) selection() (
	activity.Query, db.AnalyticsFilter, error,
) {
	loc, err := time.LoadLocation(cursor.Query.Timezone)
	if err != nil || cursor.Query.Timezone != cursor.Filter.Timezone {
		return activity.Query{}, db.AnalyticsFilter{}, fmt.Errorf("invalid timezone")
	}
	q := activity.Query{
		Timezone: cursor.Query.Timezone, Loc: loc,
		RangeStart: cursor.Query.RangeStart, RangeEnd: cursor.Query.RangeEnd,
		EffectiveEnd: cursor.Query.EffectiveEnd, Partial: cursor.Query.Partial,
		Bucket: cursor.Query.Bucket, GapCapSeconds: cursor.Query.GapCapSeconds,
	}
	if err := activity.ValidateResolvedQuery(q); err != nil {
		return activity.Query{}, db.AnalyticsFilter{}, err
	}
	f := db.AnalyticsFilter{
		Timezone: cursor.Filter.Timezone, Project: cursor.Filter.Project,
		Agent: cursor.Filter.Agent, Machine: cursor.Filter.Machine,
		ExcludeOneShot:   cursor.Filter.ExcludeOneShot,
		ExcludeAutomated: cursor.Filter.ExcludeAutomated,
	}
	return q, f, nil
}

func newCLIActivitySessionCursor(
	digest string,
	offset int,
	options activity.SessionPageOptions,
	q activity.Query,
	f db.AnalyticsFilter,
) cliActivitySessionCursor {
	return cliActivitySessionCursor{
		Version: 2, Schema: export.ActivityReportSchemaVersion,
		Digest: digest, Offset: offset, Sort: options.Sort,
		Direction: options.Direction, Bucket: options.Bucket,
		Query: cliActivityCursorQuery{
			Timezone: q.Timezone, RangeStart: q.RangeStart, RangeEnd: q.RangeEnd,
			EffectiveEnd: q.EffectiveEnd, Partial: q.Partial,
			Bucket: q.Bucket, GapCapSeconds: q.GapCapSeconds,
		},
		Filter: cliActivityCursorFilter{
			Timezone: f.Timezone, Project: f.Project, Agent: f.Agent,
			Machine: f.Machine, ExcludeOneShot: f.ExcludeOneShot,
			ExcludeAutomated: f.ExcludeAutomated,
		},
	}
}

// todayIn returns today's date as YYYY-MM-DD in the given IANA timezone,
// falling back to the local zone when tz is unknown.
func todayIn(tz string) string {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.Local
	}
	return activityReportNow().In(loc).Format("2006-01-02")
}

// printActivityReport renders the human-readable report: a header, totals,
// peak concurrency, top breakdowns, and top sessions. It deliberately omits
// the dense per-bucket timeline, which only the --json output exposes.
func printActivityReport(r activity.Report) {
	loc, err := time.LoadLocation(r.Timezone)
	if err != nil {
		loc = time.UTC
	}
	fmt.Printf(
		"Activity %s to %s (%s, %s buckets)\n",
		fmtRangeBound(r.RangeStart, loc), fmtRangeBound(r.RangeEnd, loc),
		r.Timezone, r.BucketUnit,
	)
	if r.Partial {
		fmt.Printf("Partial range, data as of %s\n", fmtInstant(r.AsOf, loc))
	}
	fmt.Println()

	printActivityTotals(r.Totals)
	fmt.Println()
	printActivityPeak(r.Peak, loc)
	fmt.Println()
	printKeyMinutes("By project", r.ByProject)
	printKeyMinutes("By model", r.ByModel)
	printKeyMinutes("By agent", r.ByAgent)
	printActivitySessions(r.BySession)
}

// printActivityTotals prints the totals block via a tabwriter.
func printActivityTotals(t activity.Totals) {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "Active minutes\t%.1f\n", t.ActiveMinutes)
	fmt.Fprintf(w, "Idle minutes\t%.1f\n", t.IdleMinutes)
	fmt.Fprintf(w, "Agent minutes\t%.1f\n", t.AgentMinutes)
	fmt.Fprintf(w, "Sessions\t%d (%d untimed)\n", t.Sessions, t.UntimedSessions)
	fmt.Fprintf(w, "Distinct projects\t%d\n", t.DistinctProjects)
	fmt.Fprintf(w, "Distinct models\t%d\n", t.DistinctModels)
	fmt.Fprintf(w, "Output tokens\t%d\n", t.OutputTokens)
	fmt.Fprintf(w, "Cost\t%s\n", fmtCost(t.Cost))
	w.Flush()
}

// printActivityPeak prints peak concurrency and when it occurred, in loc.
func printActivityPeak(p activity.Peak, loc *time.Location) {
	fmt.Printf("Peak concurrency: %d agents at %s\n",
		p.Agents, fmtInstant(p.At, loc))
}

// printKeyMinutes prints the top 5 rows of a key/agent-minutes breakdown.
func printKeyMinutes(label string, rows []activity.KeyMinutes) {
	fmt.Printf("%s (top 5):\n", label)
	if len(rows) == 0 {
		fmt.Println("  (none)")
		fmt.Println()
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	for _, row := range topKeyMinutes(rows, 5) {
		fmt.Fprintf(w, "  %s\t%.1f min\n",
			sanitizeTerminal(row.Key), row.AgentMinutes)
	}
	w.Flush()
	fmt.Println()
}

// printActivitySessions prints the top 5 sessions by appearance order.
func printActivitySessions(rows []activity.SessionRow) {
	fmt.Println("Top sessions (top 5):")
	if len(rows) == 0 {
		fmt.Println("  (none)")
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "  TITLE\tPROJECT\tAGENT\tMINUTES\tCOST")
	limit := min(len(rows), 5)
	for _, s := range rows[:limit] {
		fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\n",
			sanitizeTerminal(s.Title), sanitizeTerminal(s.Project),
			sanitizeTerminal(s.Agent),
			fmtMinutes(s.AgentMinutes), fmtCost(s.Cost),
		)
	}
	w.Flush()
}

// topKeyMinutes returns the first n rows of rows (already sorted by the query).
func topKeyMinutes(rows []activity.KeyMinutes, n int) []activity.KeyMinutes {
	return rows[:min(len(rows), n)]
}

// fmtRangeBound renders an RFC3339 range bound in loc, dropping the time
// component when the local wall time is exactly midnight.
func fmtRangeBound(ts string, loc *time.Location) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	t = t.In(loc)
	if t.Hour() == 0 && t.Minute() == 0 && t.Second() == 0 {
		return t.Format("2006-01-02")
	}
	return t.Format("2006-01-02 15:04")
}

// fmtMinutes renders an agent-minutes value, printing a dash for untimed
// sessions whose pointer is nil.
func fmtMinutes(m *float64) string {
	if m == nil {
		return "—"
	}
	return fmt.Sprintf("%.1f", *m)
}

// fmtInstant renders a nullable RFC3339 instant in loc as "YYYY-MM-DD HH:MM",
// printing a dash when nil.
func fmtInstant(ts *string, loc *time.Location) string {
	if ts == nil {
		return "—"
	}
	if t, err := time.Parse(time.RFC3339, *ts); err == nil {
		return t.In(loc).Format("2006-01-02 15:04")
	}
	return *ts
}
