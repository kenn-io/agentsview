package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
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
	Preset   string
	Date     string
	From     string
	To       string
	Timezone string
	Bucket   string
	Project  string
	Branch   string
	Agent    string
	Machine  string
	JSON     bool
	NoSync   bool
	Offline  bool
}

var activityReportNow = time.Now

// runActivityReport syncs, resolves the range, runs the report, and prints it.
func runActivityReport(cfg ActivityReportConfig) {
	ctx := context.Background()
	if _, err := branchFilterToken(cfg.Project, cfg.Branch); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
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
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(r); err != nil {
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
	gitBranch, err := branchFilterToken(cfg.Project, cfg.Branch)
	if err != nil {
		return activity.Report{}, err
	}
	setIfNotEmpty("git_branch", gitBranch)

	endpoint := strings.TrimSuffix(tr.URL, "/") +
		"/api/v1/activity/report?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return activity.Report{}, err
	}
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}
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
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return activity.Report{}, err
	}
	if r.Projects == nil {
		r.Projects = map[string]export.ProjectMapEntry{}
	}
	return r, nil
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
		return activity.Report{}, err
	}

	gitBranch, err := branchFilterToken(cfg.Project, cfg.Branch)
	if err != nil {
		return activity.Report{}, err
	}
	f := db.AnalyticsFilter{
		Timezone:         tz,
		Project:          cfg.Project,
		GitBranch:        gitBranch,
		Agent:            cfg.Agent,
		Machine:          cfg.Machine,
		ExcludeOneShot:   false,
		ExcludeAutomated: false,
	}
	return database.GetActivityReport(context.Background(), f, q)
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
