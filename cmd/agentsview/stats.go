// ABOUTME: `agentsview stats` top-level command — window-scoped
// ABOUTME: workspace analytics emitting the v1 SessionStats JSON schema.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/wesm/agentsview/internal/config"
	"github.com/wesm/agentsview/internal/db"
	"github.com/wesm/agentsview/internal/service"
)

func newStatsCommand() *cobra.Command {
	var (
		since, until, agent, timezone    string
		includeProjects, excludeProjects []string
	)
	cmd := &cobra.Command{
		Use:          "stats",
		Short:        "Window-scoped workspace analytics (v1 schema)",
		GroupID:      groupData,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, cleanup, err := openStatsService(cmd)
			if err != nil {
				return err
			}
			defer cleanup()

			// "all" is the human-facing default and echoes through to
			// stats.Filters.Agent, but the db layer treats any non-empty
			// Agent as a literal filter. Pass "" when the user hasn't
			// scoped to a specific agent so the window includes every
			// agent's sessions.
			agentFilter := agent
			if agentFilter == "all" {
				agentFilter = ""
			}
			stats, err := svc.Stats(cmd.Context(), service.StatsFilter{
				Since:           since,
				Until:           until,
				Agent:           agentFilter,
				IncludeProjects: includeProjects,
				ExcludeProjects: excludeProjects,
				Timezone:        timezone,
				GHToken:         resolveGitHubToken(cmd.Context()),
			})
			if err != nil {
				return err
			}
			if outputFormat(cmd) == "json" {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(stats)
			}
			return printStatsHuman(cmd.OutOrStdout(), stats)
		},
	}

	cmd.Flags().String("format", "human",
		"Output format: human or json")
	registerStatsFlags(cmd,
		&since, &until, &agent, &timezone,
		&includeProjects, &excludeProjects,
	)
	return cmd
}

// resolveGitHubToken returns a token for `gh search` aggregation,
// preferring AGENTSVIEW_GITHUB_TOKEN (an app-scoped secret) and
// falling back to whatever `gh auth token` prints. Returns "" when
// neither source yields a token; the stats pipeline interprets that
// as "skip PR aggregation" rather than an error.
//
// The flag-based path was removed so the token never appears in
// argv (visible to other local users via ps/proc/cmdline, and
// commonly captured by CI logs and crash reporters).
func resolveGitHubToken(ctx context.Context) string {
	if t := strings.TrimSpace(os.Getenv("AGENTSVIEW_GITHUB_TOKEN")); t != "" {
		return t
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "gh", "auth", "token")
	cmd.Stderr = io.Discard
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// registerStatsFlags wires the `agentsview stats` flags onto cmd. Split
// out so the long flag-registration block doesn't pad newStatsCommand
// past the 100-line cap.
func registerStatsFlags(
	cmd *cobra.Command,
	since, until, agent, timezone *string,
	includeProjects, excludeProjects *[]string,
) {
	f := cmd.Flags()
	f.StringVar(since, "since", "28d",
		"Start of window (duration like 28d, or YYYY-MM-DD)")
	f.StringVar(until, "until", "",
		"End of window (YYYY-MM-DD; default: now)")
	f.StringVar(agent, "agent", "all",
		"Filter by agent (claude, codex, cursor, ... or 'all')")
	f.StringArrayVar(includeProjects, "include-project", nil,
		"Restrict to these projects (repeatable)")
	f.StringArrayVar(excludeProjects, "exclude-project", nil,
		"Exclude these projects (repeatable)")
	f.StringVar(timezone, "timezone", "",
		"Timezone for temporal (default: local system timezone)")
}

// openStatsService opens a SessionService scoped to the local SQLite
// archive. The stats command deliberately bypasses resolveService
// (and the HTTP daemon transport) because the daemon does not yet
// expose a /stats endpoint, and resolveService prefers HTTP when one
// is running. Reading SQLite directly is also write-safe:
// GetSessionStats only reads, so a writable daemon owning the
// database is not disturbed.
func openStatsService(
	cmd *cobra.Command,
) (service.SessionService, func(), error) {
	cfg, err := config.LoadPFlags(cmd.Flags())
	if err != nil {
		return nil, nil, fmt.Errorf("loading config: %w", err)
	}
	applyClassifierConfig(cfg)
	d, err := openDB(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("opening db: %w", err)
	}
	cleanup := func() { d.Close() }
	// Pass a typed *db.DB so directBackend.Stats has the local handle
	// it needs; engine is nil because the CLI never syncs.
	return service.NewDirectBackend(d, nil), cleanup, nil
}

// printStatsHuman renders a human-readable summary of a SessionStats
// payload. Sections driven by nil-pointer fields are omitted when
// absent, and a zero-session window prints a short "no sessions"
// message instead of rows of zeros.
//
// Each helper returns the first write error it encountered so a broken
// pipe or short write surfaces from the command instead of silently
// truncating the output. The first error short-circuits rendering.
func printStatsHuman(w io.Writer, stats *service.SessionStats) error {
	ew := &errWriter{w: w}
	printHeader(ew, stats)
	if stats.Totals.SessionsAll == 0 {
		fmt.Fprintln(ew, "Totals")
		fmt.Fprintln(ew, "  (no sessions in window)")
		return ew.err
	}
	printTotals(ew, stats)
	printArchetypes(ew, stats)
	printSessionShape(ew, stats)
	printVelocity(ew, stats)
	printToolMix(ew, stats)
	printModelMix(ew, stats)
	printAgentPortfolio(ew, stats)
	if stats.CacheEconomics != nil {
		printCacheEconomics(ew, stats.CacheEconomics)
	}
	if stats.Adoption != nil {
		printAdoption(ew, stats.Adoption)
	}
	printTemporal(ew, stats)
	if stats.OutcomeStats != nil {
		printOutcomeStats(ew, stats.OutcomeStats)
	}
	if stats.Outcomes != nil {
		printOutcomes(ew, stats.Outcomes)
	}
	return ew.err
}

// errWriter wraps an io.Writer and remembers the first write error.
// Subsequent writes become no-ops so the rest of the formatter can run
// to completion without re-checking err on every Fprintf, and tabwriter
// flushes propagate failures the same way.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) Write(p []byte) (int, error) {
	if e.err != nil {
		return len(p), nil
	}
	n, err := e.w.Write(p)
	if err != nil {
		e.err = err
	}
	return n, err
}

func printHeader(w io.Writer, s *service.SessionStats) {
	fmt.Fprintf(w, "Session window: %s -> %s (%d days)\n",
		s.Window.Since, s.Window.Until, s.Window.Days)
	agent := s.Filters.Agent
	if agent == "" {
		agent = "all"
	}
	fmt.Fprintf(w, "Agent filter:   %s\n", agent)
	fmt.Fprintf(w, "Timezone:       %s\n", s.Filters.Timezone)
	if len(s.Filters.ProjectsIncluded) > 0 {
		fmt.Fprintf(w, "Include:        %s\n",
			strings.Join(s.Filters.ProjectsIncluded, ", "))
	}
	if len(s.Filters.ProjectsExcluded) > 0 {
		fmt.Fprintf(w, "Exclude:        %s\n",
			strings.Join(s.Filters.ProjectsExcluded, ", "))
	}
	fmt.Fprintln(w)
}

func printTotals(w io.Writer, s *service.SessionStats) {
	fmt.Fprintln(w, "Totals")
	fmt.Fprintf(w, "  Sessions:              %s (human %s, automation %s)\n",
		fmtInt(s.Totals.SessionsAll),
		fmtInt(s.Totals.SessionsHuman),
		fmtInt(s.Totals.SessionsAutomation))
	fmt.Fprintf(w, "  Messages:              %s (user %s)\n",
		fmtInt(s.Totals.MessagesTotal),
		fmtInt(s.Totals.UserMessagesTotal))
	fmt.Fprintln(w)
}

func printArchetypes(w io.Writer, s *service.SessionStats) {
	a := s.Archetypes
	fmt.Fprintln(w, "Archetypes")
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	rows := []struct {
		name  string
		count int
	}{
		{"Automation", a.Automation},
		{"Quick", a.Quick},
		{"Standard", a.Standard},
		{"Deep", a.Deep},
		{"Marathon", a.Marathon},
	}
	for _, r := range rows {
		fmt.Fprintf(tw, "  %s\t%s\n", r.name, fmtInt(r.count))
	}
	tw.Flush()
	if a.Primary != "" {
		fmt.Fprintf(w, "  Primary: %s  (primary_human: %s)\n",
			a.Primary, a.PrimaryHuman)
	}
	fmt.Fprintln(w)
}

func printSessionShape(w io.Writer, s *service.SessionStats) {
	d := s.Distributions
	fmt.Fprintln(w, "Session shape (means)")
	fmt.Fprintf(w, "  Duration (min):        mean=%s (scope_all); mean=%s (scope_human)\n",
		fmtFloat(d.DurationMinutes.ScopeAll.Mean),
		fmtFloat(d.DurationMinutes.ScopeHuman.Mean))
	fmt.Fprintf(w, "  User messages:         mean=%s (scope_all); mean=%s (scope_human)\n",
		fmtFloat(d.UserMessages.ScopeAll.Mean),
		fmtFloat(d.UserMessages.ScopeHuman.Mean))
	fmt.Fprintf(w, "  Peak context (tokens): mean=%s (scope_all); null_count=%s\n",
		fmtInt64(int64(d.PeakContextTokens.ScopeAll.Mean+0.5)),
		fmtInt(d.PeakContextTokens.NullCount))
	fmt.Fprintf(w, "  Tools per turn:        mean=%s (scope_all); mean=%s (scope_human)\n",
		fmtFloat(d.ToolsPerTurn.ScopeAll.Mean),
		fmtFloat(d.ToolsPerTurn.ScopeHuman.Mean))
	fmt.Fprintln(w)
}

func printVelocity(w io.Writer, s *service.SessionStats) {
	v := s.Velocity
	fmt.Fprintln(w, "Velocity")
	fmt.Fprintf(w, "  Turn cycle (s):        p50=%s p90=%s mean=%s\n",
		fmtFloat(v.TurnCycleSeconds.P50),
		fmtFloat(v.TurnCycleSeconds.P90),
		fmtFloat(v.TurnCycleSeconds.Mean))
	fmt.Fprintf(w, "  First response (s):    p50=%s p90=%s mean=%s\n",
		fmtFloat(v.FirstResponseSeconds.P50),
		fmtFloat(v.FirstResponseSeconds.P90),
		fmtFloat(v.FirstResponseSeconds.Mean))
	fmt.Fprintf(w, "  Messages per hour:     %s\n",
		fmtFloat(v.MessagesPerActiveHour))
	fmt.Fprintln(w)
}

func printToolMix(w io.Writer, s *service.SessionStats) {
	m := s.ToolMix
	if m.TotalCalls == 0 && len(m.ByCategory) == 0 {
		return
	}
	fmt.Fprintln(w, "Tool mix (top 5 categories)")
	entries := sortedIntMap(m.ByCategory)
	top := entries
	if len(top) > 5 {
		top = top[:5]
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	for _, e := range top {
		name := e.key
		if name == "" {
			name = "(uncategorized)"
		}
		fmt.Fprintf(tw, "  %s\t%s\n", name, fmtInt(e.val))
	}
	tw.Flush()
	fmt.Fprintf(w, "  (total tool calls: %s)\n", fmtInt(m.TotalCalls))
	fmt.Fprintln(w)
}

func printModelMix(w io.Writer, s *service.SessionStats) {
	m := s.ModelMix
	if len(m.ByTokens) == 0 {
		return
	}
	fmt.Fprintln(w, "Model mix (tokens)")
	entries := sortedInt64Map(m.ByTokens)
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	for _, e := range entries {
		fmt.Fprintf(tw, "  %s\t%s\n", e.key, fmtInt64(e.val))
	}
	tw.Flush()
	fmt.Fprintln(w)
}

func printAgentPortfolio(w io.Writer, s *service.SessionStats) {
	p := s.AgentPortfolio
	if len(p.BySessions) == 0 {
		return
	}
	fmt.Fprintln(w, "Agent portfolio")
	entries := sortedIntMap(p.BySessions)
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	for _, e := range entries {
		agent := e.key
		sessions := e.val
		tokens := p.ByTokens[agent]
		msgs := p.ByMessages[agent]
		marker := ""
		if agent == p.Primary {
			marker = "  [primary]"
		}
		fmt.Fprintf(tw, "  %s\t%s sessions\t%s tokens\t%s msgs%s\n",
			agent,
			fmtInt(sessions),
			fmtInt64(tokens),
			fmtInt(msgs),
			marker)
	}
	tw.Flush()
	fmt.Fprintln(w)
}

func printCacheEconomics(w io.Writer, c *db.StatsCacheEconomics) {
	fmt.Fprintln(w, "Cache economics (claude-only)")
	fmt.Fprintf(w, "  Overall hit ratio:   %.2f\n",
		c.CacheHitRatio.Overall)
	fmt.Fprintf(w, "  $ spent:             $%.2f\n", c.DollarsSpent)
	fmt.Fprintf(w, "  $ saved vs uncached: $%.2f\n",
		c.DollarsSavedVsUncached)
	fmt.Fprintln(w)
}

func printAdoption(w io.Writer, a *db.StatsAdoption) {
	fmt.Fprintln(w, "Adoption (claude-only)")
	fmt.Fprintf(w, "  Plan mode rate:      %.0f%%\n",
		a.PlanModeRate*100)
	fmt.Fprintf(w, "  Subagents/session:   %s\n",
		fmtFloat(a.SubagentsPerSession))
	fmt.Fprintf(w, "  Distinct skills:     %s\n",
		fmtInt(a.DistinctSkills))
	fmt.Fprintln(w)
}

func printTemporal(w io.Writer, s *service.SessionStats) {
	t := s.Temporal
	active := 0
	for _, h := range t.HourlyUTC {
		if h.Sessions > 0 || h.UserMessages > 0 {
			active++
		}
	}
	if active == 0 && t.ReporterTimezone == "" {
		return
	}
	fmt.Fprintln(w, "Temporal")
	fmt.Fprintf(w, "  Hours with activity: %s\n", fmtInt(active))
	if t.ReporterTimezone != "" {
		fmt.Fprintf(w, "  Reporter timezone:   %s\n", t.ReporterTimezone)
	}
	fmt.Fprintln(w)
}

func printOutcomeStats(w io.Writer, o *db.StatsOutcomeStats) {
	fmt.Fprintln(w, "Outcome stats (git)")
	fmt.Fprintf(w, "  Repos active:        %s\n", fmtInt(o.ReposActive))
	fmt.Fprintf(w, "  Commits:             %s\n", fmtInt(o.Commits))
	fmt.Fprintf(w, "  LOC added/removed:   +%s / -%s\n",
		fmtInt(o.LOCAdded), fmtInt(o.LOCRemoved))
	fmt.Fprintf(w, "  Files changed:       %s\n", fmtInt(o.FilesChanged))
	if o.PRsOpened != nil {
		fmt.Fprintf(w, "  PRs opened:          %s\n", fmtInt(*o.PRsOpened))
	}
	if o.PRsMerged != nil {
		fmt.Fprintf(w, "  PRs merged:          %s\n", fmtInt(*o.PRsMerged))
	}
	fmt.Fprintln(w)
}

func printOutcomes(w io.Writer, o *db.StatsOutcomes) {
	fmt.Fprintln(w, "Outcomes")
	fmt.Fprintf(w, "  Success / Failure / Unknown: %s / %s / %s\n",
		fmtInt(o.Success), fmtInt(o.Failure), fmtInt(o.Unknown))
	if len(o.GradeDistribution) > 0 {
		fmt.Fprintf(w, "  Grade distribution: %s\n",
			formatGrades(o.GradeDistribution))
	}
	fmt.Fprintf(w, "  Tool retry rate:     %.1f%%\n",
		o.ToolRetryRate*100)
	fmt.Fprintf(w, "  Compactions/session: %s\n",
		fmtFloat(o.CompactionsPerSession))
	fmt.Fprintf(w, "  Avg edit churn:      %s\n",
		fmtFloat(o.AvgEditChurn))
	fmt.Fprintln(w)
}

// formatGrades renders a grade histogram in canonical A..F order so
// two runs on the same data produce identical text.
func formatGrades(g map[string]int) string {
	order := []string{"A", "B", "C", "D", "F"}
	seen := map[string]bool{}
	parts := make([]string, 0, len(g))
	for _, k := range order {
		if v, ok := g[k]; ok {
			parts = append(parts, fmt.Sprintf("%s=%s", k, fmtInt(v)))
			seen[k] = true
		}
	}
	// Append any extra keys we didn't know about, sorted, for safety.
	extras := make([]string, 0)
	for k := range g {
		if !seen[k] {
			extras = append(extras, k)
		}
	}
	sort.Strings(extras)
	for _, k := range extras {
		parts = append(parts, fmt.Sprintf("%s=%s", k, fmtInt(g[k])))
	}
	return strings.Join(parts, " ")
}

// fmtInt formats an integer with ASCII thousands separators.
func fmtInt(n int) string {
	return fmtInt64(int64(n))
}

// fmtInt64 is the int64 variant of fmtInt.
func fmtInt64(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		if neg {
			return "-" + s
		}
		return s
	}
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	// Compute the length of the leading group (1-3 digits).
	lead := len(s) % 3
	if lead == 0 {
		lead = 3
	}
	b.WriteString(s[:lead])
	for i := lead; i < len(s); i += 3 {
		b.WriteByte(',')
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

// fmtFloat renders a float with one decimal place, matching the style
// of the target output. Zero renders as "0".
func fmtFloat(f float64) string {
	if f == 0 {
		return "0"
	}
	return strconv.FormatFloat(f, 'f', 1, 64)
}

// kvInt is a sortable (key, int) pair used when ordering maps for
// deterministic, human-friendly output (largest first, ties broken
// alphabetically).
type kvInt struct {
	key string
	val int
}

// kvInt64 is the int64 variant of kvInt.
type kvInt64 struct {
	key string
	val int64
}

func sortedIntMap(m map[string]int) []kvInt {
	out := make([]kvInt, 0, len(m))
	for k, v := range m {
		out = append(out, kvInt{k, v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].val != out[j].val {
			return out[i].val > out[j].val
		}
		return out[i].key < out[j].key
	})
	return out
}

func sortedInt64Map(m map[string]int64) []kvInt64 {
	out := make([]kvInt64, 0, len(m))
	for k, v := range m {
		out = append(out, kvInt64{k, v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].val != out[j].val {
			return out[i].val > out[j].val
		}
		return out[i].key < out[j].key
	})
	return out
}
