package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/pricing"
	"go.kenn.io/agentsview/internal/sync"
)

// quickSyncMargin pads the mtime cutoff backward from the
// last recorded sync start time to catch files modified
// during the prior sync. Smaller values are faster but risk
// missing recent writes; 10s is a safe default.
const quickSyncMargin = 10 * time.Second

// defaultUsageDays is the default lookback window for
// `agentsview usage daily` when neither --since nor --all is
// given. Matches ccusage's default and avoids scanning the
// full history when users usually want recent spend.
const defaultUsageDays = 30

// defaultUsageDateRange mirrors the HTTP usage route's default range:
// fill a missing upper bound with today, then fill a missing lower
// bound relative to that upper bound. Callers that intentionally want
// an open-ended range must set NoDefaultRange instead of calling this.
func defaultUsageDateRange(
	from, to string, now time.Time,
) (string, string) {
	now = now.UTC()
	if to == "" {
		to = now.Format("2006-01-02")
	}
	if from == "" {
		t, err := time.Parse("2006-01-02", to)
		if err != nil {
			t = now
		}
		from = t.AddDate(0, 0, -defaultUsageDays).Format("2006-01-02")
	}
	return from, to
}

type UsageDailyConfig struct {
	JSON      bool
	Since     string
	Until     string
	All       bool
	Agent     string
	Breakdown bool
	Offline   bool
	NoSync    bool
	Timezone  string
}

// resolveUsageWindow resolves the raw --since/--until flags into concrete
// inclusive YYYY-MM-DD bounds. Both accept a duration like 28d or a date,
// the same syntax as `stats`. --until resolves first; a duration --since is
// then measured back from the resolved --until (or from now when --until is
// open), matching how stats anchors a duration window. An inverted explicit
// window is rejected so a reversed range fails loudly instead of returning
// an empty result.
func resolveUsageWindow(
	since, until string, now time.Time, loc *time.Location,
) (string, string, error) {
	if loc == nil {
		loc = time.UTC
	}
	now = now.In(loc)
	// Resolve --until first and keep it as the anchor for --since:
	// ParseWindowPoint measures a duration back from its time argument, so
	// a duration --since is measured from the resolved --until while a date
	// stands alone. --until open leaves the anchor at now.
	anchor := now
	to := ""
	if until != "" {
		t, date, err := resolveUsageWindowPoint(until, now, loc)
		if err != nil {
			return "", "", fmt.Errorf("invalid --until: %w", err)
		}
		anchor, to = t, date
	}
	from := ""
	if since != "" {
		_, date, err := resolveUsageWindowPoint(since, anchor, loc)
		if err != nil {
			return "", "", fmt.Errorf("invalid --since: %w", err)
		}
		from = date
	}
	// Bounds are inclusive, so from == to is a valid single day (hence >
	// not >=). String comparison is valid because YYYY-MM-DD sorts
	// lexically.
	if from != "" && to != "" && from > to {
		return "", "", fmt.Errorf(
			"--since (%s) must not be after --until (%s)", from, to)
	}
	return from, to, nil
}

func resolveUsageWindowPoint(
	raw string, anchor time.Time, loc *time.Location,
) (time.Time, string, error) {
	if t, err := time.ParseInLocation("2006-01-02", raw, loc); err == nil {
		return t, raw, nil
	}
	t, err := db.ParseWindowPoint(raw, anchor)
	if err != nil {
		return time.Time{}, "", err
	}
	return t, t.In(loc).Format("2006-01-02"), nil
}

func runUsageDaily(cfg UsageDailyConfig) {
	tz := cfg.Timezone
	if tz == "" {
		tz = localTimezone()
	}

	loc, err := time.LoadLocation(tz)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: invalid --timezone: %v\n", err)
		os.Exit(1)
	}

	since, until, err := resolveUsageWindow(cfg.Since, cfg.Until, time.Now(), loc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	filter := db.UsageFilter{
		From:     since,
		To:       until,
		Agent:    cfg.Agent,
		Timezone: tz,
	}
	noDefaultRange := cfg.All || cfg.Since != "" || cfg.Until != ""

	ctx := context.Background()
	backend, cleanup, err := resolveArchiveQueryBackend(ctx, archiveQueryPolicy{
		Offline:              cfg.Offline,
		NoSync:               cfg.NoSync,
		AutoStart:            true,
		ReadOnlyDaemon:       archiveQuerySkipReadOnlyDaemon,
		DirectReadOnlyAction: "refresh usage directly",
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer closeArchiveQueryBackend(cleanup)

	result, err := backend.DailyUsage(ctx, dailyUsageQuery{
		Filter:         filter,
		NoDefaultRange: noDefaultRange,
		Breakdowns:     cfg.Breakdown,
		SessionCounts:  cfg.JSON,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if cfg.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(result); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	printDailyTable(result, cfg.Breakdown)
	if note := noTokenDataNote(cfg.Agent, result.Totals); note != "" {
		fmt.Fprintln(os.Stderr, note)
	}
}

// noTokenDataNote returns a one-line stderr note for a zero usage result when
// the user has filtered to a Copilot agent whose records do not include token
// or cost data that agentsview can total. It returns "" when the filter is not
// a Copilot agent or real token/cost data exists. This is an agent-property
// statement (issue #349) shown in response to an explicit --agent the user
// typed, so it needs no session-presence check; it is appropriate even for an
// empty window.
func noTokenDataNote(agent string, totals db.UsageTotals) string {
	if !db.IsCopilotAgentFilter(agent) || !db.NoTokenData(totals) {
		return ""
	}
	return "note: these GitHub Copilot records do not include token " +
		"or cost data that agentsview can total."
}

type UsageStatuslineConfig struct {
	Agent   string
	Offline bool
	NoSync  bool
}

func runUsageStatusline(cfg UsageStatuslineConfig) {
	today := time.Now().Format("2006-01-02")
	filter := db.UsageFilter{
		From:     today,
		To:       today,
		Agent:    cfg.Agent,
		Timezone: localTimezone(),
	}

	ctx := context.Background()
	backend, cleanup, err := resolveArchiveQueryBackend(ctx, archiveQueryPolicy{
		Offline:              cfg.Offline,
		NoSync:               cfg.NoSync,
		AutoStart:            true,
		ReadOnlyDaemon:       archiveQuerySkipReadOnlyDaemon,
		DirectReadOnlyAction: "refresh usage directly",
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer closeArchiveQueryBackend(cleanup)

	result, err := backend.DailyUsage(ctx, dailyUsageQuery{
		Filter:         filter,
		NoDefaultRange: true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	printUsageStatusline(result, cfg.Agent)
}

func printUsageStatusline(result db.DailyUsageResult, agent string) {
	if agent != "" {
		fmt.Printf("%s today (%s)\n",
			fmtCost(result.Totals.TotalCost), agent)
	} else {
		fmt.Printf("%s today\n",
			fmtCost(result.Totals.TotalCost))
	}
}

func applyCustomPricing(database *db.DB, cfg config.Config) {
	if len(cfg.CustomModelPricing) == 0 {
		return
	}
	database.SetCustomPricing(cfg.CustomModelPricing)
}

// ensureFreshData makes sure the database reflects recent
// session file changes before serving a usage query.
//
// Decision tree:
//  1. If the stored data version is stale (parser changes on
//     upgrade), run a full resync.
//  2. If a server process is active (via kit runtime record), trust
//     its file watcher and skip on-demand sync. This avoids
//     duplicate work and write contention.
//  3. Otherwise, run a quick incremental sync scoped to files
//     modified since the last recorded sync start time, with
//     a small safety margin.
//
// Callers that need stale data (e.g. offline benchmarks) can
// bypass via skip=true.
func ensureFreshData(
	ctx context.Context, appCfg config.Config, database *db.DB, skip bool,
) {
	if skip {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}

	// Silence engine worker log.Printf lines (e.g. "db:
	// InsertMessages (N msgs)") for both branches so --json and
	// statusline output stay clean. Progress goes to stderr
	// below to stay out of stdout-bound payloads.
	origLog := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(origLog)

	if database.NeedsResync() {
		engine := sync.NewEngine(database, sync.EngineConfig{
			AgentDirs: appCfg.AgentDirs,
			Machine:   "local",
		})
		defer engine.Close()
		fmt.Fprintln(os.Stderr,
			"Data version changed, running full resync...")
		t := time.Now()
		progress := newResyncProgressPrinter(os.Stderr, time.Now)
		stats := engine.ResyncAll(ctx, progress.Print)
		progress.Finish()
		printSyncSummaryStderr(stats, t)
		return
	}

	// Skip on-demand sync only when a writable local daemon is
	// already keeping the SQLite archive fresh. pg serve daemons
	// (read-only) do not sync the local DB, so we still want to
	// run our own sync when only one of those is present.
	if IsLocalDaemonActive(appCfg.DataDir, appCfg.AuthToken) {
		return
	}

	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: appCfg.AgentDirs,
		Machine:   "local",
	})
	defer engine.Close()

	since := engine.LastSyncStartedAt()
	if !since.IsZero() {
		since = since.Add(-quickSyncMargin)
	}

	engine.SyncAllSince(ctx, since, func(sync.Progress) {})
}

// printSyncSummaryStderr mirrors printSyncSummary but writes to
// stderr so it does not pollute stdout-bound JSON or statusline output.
func printSyncSummaryStderr(stats sync.SyncStats, t time.Time) {
	summary := fmt.Sprintf(
		"\nSync complete: %d sessions synced",
		stats.Synced,
	)
	if stats.OrphanedCopied > 0 {
		summary += fmt.Sprintf(
			", %d archived sessions preserved",
			stats.OrphanedCopied,
		)
	}
	if stats.Failed > 0 {
		summary += fmt.Sprintf(", %d failed", stats.Failed)
	}
	summary += fmt.Sprintf(
		" in %s\n", time.Since(t).Round(time.Millisecond),
	)
	summary += formatAnomalySummary(stats.Anomalies)
	fmt.Fprint(os.Stderr, summary)
	for _, w := range stats.Warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}
}

// seedPricing ensures fallback rates are present in
// model_pricing, then kicks off a background LiteLLM refresh.
//
// Fallback rates are only upserted when the stored seed
// version differs from pricing.FallbackVersion (or is
// absent). This avoids overwriting live LiteLLM rates on
// every restart while still propagating corrected fallback
// rates when the binary is upgraded.
func seedPricing(database *db.DB) {
	if err := seedFallbackPricing(database); err != nil {
		log.Printf("pricing seed: %v", err)
	}
	go refreshPricingFromLiteLLM(database)
}

func seedFallbackPricing(database *db.DB) error {
	const metaKey = "_fallback_version"
	stored, err := database.GetPricingMeta(metaKey)
	if err != nil {
		return err
	}
	if stored == pricing.FallbackVersion {
		return nil
	}
	if err := upsertPricing(
		database, pricing.FallbackPricing(),
	); err != nil {
		return err
	}
	return database.SetPricingMeta(metaKey, pricing.FallbackVersion)
}

// refreshPricingFromLiteLLM fetches the upstream LiteLLM
// catalog and upserts it over whatever is in the table. Called
// from a goroutine after the synchronous fallback seed so a
// slow or failing fetch never blocks server startup.
func refreshPricingFromLiteLLM(database *db.DB) {
	prices, err := pricing.FetchLiteLLMPricing()
	if err != nil {
		log.Printf(
			"pricing refresh: litellm fetch failed: %v", err,
		)
		return
	}
	if err := upsertPricing(database, prices); err != nil {
		log.Printf("pricing refresh: upsert failed: %v", err)
	}
}

// pricingRefreshMetaKey marks the last time the CLI tried to
// refresh model_pricing from LiteLLM. Cooldown is enforced
// against this value, win or fail, so a repeatedly-failing
// fetch (offline, DNS broken) does not block every CLI call.
const pricingRefreshMetaKey = "_litellm_last_attempt"

// pricingRefreshCooldown is the minimum interval between
// CLI-triggered LiteLLM fetches. Short enough that a newly
// released model gets priced within hours of the user noticing,
// long enough that statusline-style repeated CLI invocations
// don't hammer LiteLLM when a session uses a truly unpriced
// model (e.g. a local Ollama model).
const pricingRefreshCooldown = time.Hour

// refreshPricingIfStale fetches the LiteLLM pricing catalog
// and upserts it when the last attempt is older than cooldown
// (or has never run). The fetcher is injectable for tests so
// the cooldown logic can be exercised without network. Returns
// true when an upsert succeeded; callers can re-query pricing
// after a true result. Errors from the fetch are returned so
// the caller can emit a warning; cooldown is recorded before
// the fetch so a persistent failure won't retry every call.
func refreshPricingIfStale(
	database *db.DB,
	fetch func() ([]pricing.ModelPricing, error),
	cooldown time.Duration,
	now time.Time,
) (bool, error) {
	stored, err := database.GetPricingMeta(pricingRefreshMetaKey)
	if err != nil {
		return false, fmt.Errorf(
			"reading pricing refresh meta: %w", err)
	}
	if stored != "" {
		last, perr := time.Parse(time.RFC3339, stored)
		if perr == nil && now.Sub(last) < cooldown {
			return false, nil
		}
	}
	if err := database.SetPricingMeta(
		pricingRefreshMetaKey, now.UTC().Format(time.RFC3339),
	); err != nil {
		return false, fmt.Errorf(
			"recording pricing refresh attempt: %w", err)
	}
	prices, err := fetch()
	if err != nil {
		return false, err
	}
	if err := upsertPricing(database, prices); err != nil {
		return false, err
	}
	return true, nil
}

func ensurePricing(database *db.DB, offline bool) {
	if _, err := ensurePricingWithFetcher(
		database, offline, pricing.FetchLiteLLMPricing, time.Now(),
	); err != nil {
		fmt.Fprintf(os.Stderr,
			"warning: pricing refresh failed: %v\n", err)
	}
}

func ensureUsagePricing(
	database *db.DB, offline bool,
	custom map[string]config.CustomModelRate,
) {
	if offline && database.ReadOnly() {
		applyFallbackPricing(database, custom)
		return
	}
	ensurePricing(database, offline)
}

func applyFallbackPricing(
	database *db.DB, custom map[string]config.CustomModelRate,
) {
	rates := make(map[string]config.CustomModelRate)
	for _, p := range pricing.FallbackPricing() {
		// These keys are the same concrete model-pattern keys that the
		// model_pricing table stores. SQLite usage lookups run the merged map
		// through pricing.Resolve, so normalized/canonical aliases still match
		// when this read-only path cannot seed model_pricing rows.
		rates[p.ModelPattern] = config.CustomModelRate{
			Input:         p.InputPerMTok,
			Output:        p.OutputPerMTok,
			CacheCreation: p.CacheCreationPerMTok,
			CacheRead:     p.CacheReadPerMTok,
		}
	}
	maps.Copy(rates, custom)
	database.SetCustomPricing(rates)
}

func ensurePricingWithFetcher(
	database *db.DB, offline bool,
	fetch func() ([]pricing.ModelPricing, error),
	now time.Time,
) (bool, error) {
	if offline {
		return false, upsertPricing(database, pricing.FallbackPricing())
	}

	if err := seedFallbackPricing(database); err != nil {
		return false, err
	}

	return refreshPricingIfStale(
		database, fetch, pricingRefreshCooldown, now,
	)
}

func fetchHTTPDailyUsage(
	ctx context.Context,
	tr transport,
	authToken string,
	query dailyUsageQuery,
) (db.DailyUsageResult, error) {
	filter := query.Filter
	q := url.Values{}
	q.Set("no_default_range", strconv.FormatBool(query.NoDefaultRange))
	q.Set("breakdowns", strconv.FormatBool(query.Breakdowns))
	q.Set("session_counts", strconv.FormatBool(query.SessionCounts))
	setIfNotEmpty := func(k, v string) {
		if v != "" {
			q.Set(k, v)
		}
	}
	setIfNotEmpty("from", filter.From)
	setIfNotEmpty("to", filter.To)
	setIfNotEmpty("timezone", filter.Timezone)
	setIfNotEmpty("agent", filter.Agent)
	setIfNotEmpty("project", filter.Project)
	setIfNotEmpty("machine", filter.Machine)
	setIfNotEmpty("exclude_project", filter.ExcludeProject)
	setIfNotEmpty("exclude_agent", filter.ExcludeAgent)
	setIfNotEmpty("exclude_model", filter.ExcludeModel)
	setIfNotEmpty("model", filter.Model)
	setIfNotEmpty("active_since", filter.ActiveSince)
	setIfNotEmpty("termination", filter.Termination)
	if filter.MinUserMessages > 0 {
		q.Set("min_user_messages", fmt.Sprint(filter.MinUserMessages))
	}
	q.Set("include_one_shot", strconv.FormatBool(!filter.ExcludeOneShot))
	q.Set("include_automated", strconv.FormatBool(!filter.ExcludeAutomated))

	endpoint := strings.TrimSuffix(tr.URL, "/") +
		"/api/v1/usage/summary?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return db.DailyUsageResult{}, err
	}
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return db.DailyUsageResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return db.DailyUsageResult{}, fmt.Errorf(
			"usage summary: HTTP %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)),
		)
	}
	var out struct {
		Totals        db.UsageTotals        `json:"totals"`
		Daily         []db.DailyUsageEntry  `json:"daily"`
		SessionCounts db.UsageSessionCounts `json:"sessionCounts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return db.DailyUsageResult{}, err
	}
	return db.DailyUsageResult{
		Daily:         out.Daily,
		Totals:        out.Totals,
		SessionCounts: out.SessionCounts,
	}, nil
}

// upsertPricing copies pricing rows into the db.ModelPricing
// shape and upserts them. Shared by ensurePricing (CLI),
// seedPricing (startup fallback), and
// refreshPricingFromLiteLLM (async refresh).
func upsertPricing(
	database *db.DB, prices []pricing.ModelPricing,
) error {
	dbPrices := make([]db.ModelPricing, len(prices))
	for i, p := range prices {
		dbPrices[i] = db.ModelPricing{
			ModelPattern:         p.ModelPattern,
			InputPerMTok:         p.InputPerMTok,
			OutputPerMTok:        p.OutputPerMTok,
			CacheCreationPerMTok: p.CacheCreationPerMTok,
			CacheReadPerMTok:     p.CacheReadPerMTok,
		}
	}
	return database.UpsertModelPricing(dbPrices)
}

func printDailyTable(
	result db.DailyUsageResult, breakdown bool,
) {
	w := tabwriter.NewWriter(
		os.Stdout, 0, 4, 2, ' ', 0,
	)

	fmt.Fprintln(w,
		"DATE\tINPUT\tOUTPUT\tCACHE_CR\tCACHE_RD\tCOST\tMODELS")
	fmt.Fprintln(w,
		"----\t-----\t------\t--------\t--------\t----\t------")

	for _, day := range result.Daily {
		models := joinModels(day.ModelsUsed)
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\t%s\t%s\n",
			day.Date,
			day.InputTokens,
			day.OutputTokens,
			day.CacheCreationTokens,
			day.CacheReadTokens,
			fmtCost(day.TotalCost),
			models,
		)

		if breakdown {
			for _, mb := range day.ModelBreakdowns {
				fmt.Fprintf(w,
					"  %s\t%d\t%d\t%d\t%d\t%s\t\n",
					mb.ModelName,
					mb.InputTokens,
					mb.OutputTokens,
					mb.CacheCreationTokens,
					mb.CacheReadTokens,
					fmtCost(mb.Cost),
				)
			}
		}
	}

	fmt.Fprintln(w,
		"----\t-----\t------\t--------\t--------\t----\t------")
	fmt.Fprintf(w, "TOTAL\t%d\t%d\t%d\t%d\t%s\t\n",
		result.Totals.InputTokens,
		result.Totals.OutputTokens,
		result.Totals.CacheCreationTokens,
		result.Totals.CacheReadTokens,
		fmtCost(result.Totals.TotalCost),
	)

	w.Flush()
}

// localTimezone returns the IANA name of the system's local timezone.
func localTimezone() string {
	return time.Now().Location().String()
}

// fmtCost formats a dollar amount with two decimal places,
// matching conventional currency display. Non-zero values
// under half a cent would otherwise round to "$0.00" and
// read as "free", so they render as "<$0.01" instead.
func fmtCost(v float64) string {
	if v > 0 && v < 0.005 {
		return "<$0.01"
	}
	return fmt.Sprintf("$%.2f", v)
}

func joinModels(models []string) string {
	if len(models) == 0 {
		return ""
	}
	var s strings.Builder
	s.WriteString(models[0])
	for _, m := range models[1:] {
		s.WriteString(", " + m)
	}
	return s.String()
}
