package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

// HealthConfig configures the `health` command.
type HealthConfig struct {
	JSON  bool
	Limit int
}

const (
	defaultHealthLimit = 20
	maxHealthLimit     = db.MaxSessionLimit
)

func runHealth(args []string, cfg HealthConfig) {
	appCfg, err := config.LoadMinimal()
	if err != nil {
		fatal("loading config: %v", err)
	}
	database, err := openReadOnlyDB(appCfg)
	if err != nil {
		fatal("opening database: %v", err)
	}
	defer database.Close()

	ctx := context.Background()

	if len(args) == 0 {
		runHealthList(ctx, database, cfg)
		return
	}
	runHealthDetail(ctx, database, args[0], cfg.JSON)
}

func runHealthList(
	ctx context.Context, database *db.DB, cfg HealthConfig,
) {
	limit := cfg.Limit
	if limit <= 0 {
		limit = defaultHealthLimit
	}
	if limit > maxHealthLimit {
		limit = maxHealthLimit
	}

	page, err := database.ListSessions(ctx, db.SessionFilter{
		Limit: limit,
	})
	if err != nil {
		fatal("listing sessions: %v", err)
	}

	if cfg.JSON {
		writeJSON(os.Stdout, page.Sessions)
		return
	}

	if len(page.Sessions) == 0 {
		fmt.Println("No sessions found.")
		return
	}
	printHealthList(os.Stdout, page.Sessions)
}

func runHealthDetail(
	ctx context.Context, database *db.DB,
	sessionID string, asJSON bool,
) {
	resolved, err := resolveSessionID(ctx, database, sessionID)
	if err != nil {
		fatal("resolving session id: %v", err)
	}
	if resolved == "" {
		fmt.Fprintf(os.Stderr,
			"session not found: %s\n", sessionID)
		os.Exit(1)
	}
	sess, err := database.GetSessionFull(ctx, resolved)
	if err != nil {
		fatal("getting session: %v", err)
	}
	if sess == nil {
		fmt.Fprintf(os.Stderr,
			"session not found: %s\n", sessionID)
		os.Exit(1)
	}

	if asJSON {
		writeJSON(os.Stdout, sess)
		return
	}
	printHealthDetail(os.Stdout, *sess)
}

// resolveLookupLimit caps the partial-match query for
// ambiguity detection. The previous limit of 5 was a real
// hole: if the exact ID was in the top 5 but a colliding
// short-ID match fell outside that window, the function would
// silently resolve to the exact match instead of reporting
// ambiguity. A limit this high also acts as a defensive bound
// -- a user with hundreds of substring matches has bigger
// problems than a missed ambiguity warning.
const resolveLookupLimit = 1000

// resolveSessionID returns the unique session id matching the
// input (full ID or substring against id), or "" when no row
// matches. Substring matching covers the short IDs shown in
// `health` list output.
//
// When the input matches multiple rows, an exact full-ID match
// wins as long as no other matching row's displayed short ID
// also equals the input. This preserves the natural case where
// a local ID is a substring of a host-prefixed remote ID
// (host~<id>) -- the user typed the full local ID and clearly
// meant that row -- while still flagging true display
// collisions where two sessions are indistinguishable in the
// list output.
func resolveSessionID(
	ctx context.Context, database *db.DB, partial string,
) (string, error) {
	matches, err := database.FindSessionIDsByPartial(
		ctx, partial, resolveLookupLimit,
	)
	if err != nil {
		return "", err
	}
	switch len(matches) {
	case 0:
		return "", nil
	case 1:
		return matches[0], nil
	}

	exact := ""
	for _, m := range matches {
		if m == partial {
			exact = m
			break
		}
	}
	if exact != "" {
		for _, m := range matches {
			if m != exact && shortID(m) == partial {
				return "", ambiguousMatchErr(partial, matches)
			}
		}
		return exact, nil
	}
	return "", ambiguousMatchErr(partial, matches)
}

func ambiguousMatchErr(partial string, matches []string) error {
	return fmt.Errorf(
		"ambiguous id %q matches %d sessions: %s",
		partial, len(matches), strings.Join(matches, ", "),
	)
}

func printHealthList(w io.Writer, sessions []db.Session) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw,
		"DATE\tAGENT\tGRADE\tOUTCOME\tMSGS\tFAILS\tPROJECT\tID")
	fmt.Fprintln(tw,
		"----\t-----\t-----\t-------\t----\t-----\t-------\t--")
	for _, s := range sessions {
		fmt.Fprintf(tw,
			"%s\t%s\t%s\t%s\t%d\t%d\t%s\t%s\n",
			shortDate(sessionDisplayTime(s)),
			truncate(s.Agent, 10),
			gradeCell(s.HealthGrade),
			outcomeCell(s.Outcome),
			s.MessageCount,
			s.FinalFailureStreak,
			truncate(s.Project, 30),
			shortID(s.ID),
		)
	}
	_ = tw.Flush()
}

func printHealthDetail(w io.Writer, s db.Session) {
	fmt.Fprintf(w, "Session:  %s\n", s.ID)
	fmt.Fprintf(w, "Project:  %s\n", nonEmpty(s.Project, "(none)"))
	fmt.Fprintf(w, "Agent:    %s\n", nonEmpty(s.Agent, "(unknown)"))
	if s.GitBranch != "" {
		fmt.Fprintf(w, "Branch:   %s\n", s.GitBranch)
	}
	if s.Cwd != "" {
		fmt.Fprintf(w, "Cwd:      %s\n", s.Cwd)
	}
	fmt.Fprintf(w, "Started:  %s\n", longDate(strDeref(s.StartedAt)))
	fmt.Fprintf(w, "Ended:    %s\n", longDate(strDeref(s.EndedAt)))
	fmt.Fprintf(w, "Messages: %d (%d user)\n",
		s.MessageCount, s.UserMessageCount)
	fmt.Fprintln(w)

	fmt.Fprintln(w, "Health")
	fmt.Fprintf(w, "  Grade:   %s%s\n",
		gradeCell(s.HealthGrade),
		formatScore(s.HealthScore))
	fmt.Fprintf(w, "  Outcome: %s%s\n",
		outcomeCell(s.Outcome),
		formatConfidence(s.OutcomeConfidence, s.EndedWithRole))
	fmt.Fprintln(w)

	fmt.Fprintln(w, "Signals")
	fmt.Fprintf(w, "  Tool failures:        %d\n",
		s.ToolFailureSignalCount)
	fmt.Fprintf(w, "  Tool retries:         %d\n", s.ToolRetryCount)
	fmt.Fprintf(w, "  Edit churn:           %d\n", s.EditChurnCount)
	fmt.Fprintf(w, "  Consecutive fails:    %d\n",
		s.ConsecutiveFailureMax)
	fmt.Fprintf(w, "  Final failure streak: %d\n",
		s.FinalFailureStreak)
	fmt.Fprintf(w, "  Secret findings:      %d\n", s.SecretLeakCount)
	fmt.Fprintf(w, "  Compactions:          %s\n",
		formatCompactions(
			s.CompactionCount, s.MidTaskCompactionCount,
		))
	fmt.Fprintf(w, "  Context pressure:     %s\n",
		formatPressure(s.ContextPressureMax))

	if s.SignalsPendingSince != nil {
		fmt.Fprintln(w)
		fmt.Fprintf(w,
			"Signals pending since %s (deferred recompute).\n",
			*s.SignalsPendingSince)
	}
}

func gradeCell(g *string) string {
	if g == nil || *g == "" {
		return "-"
	}
	return *g
}

func outcomeCell(o string) string {
	if o == "" {
		return "-"
	}
	return o
}

func formatScore(score *int) string {
	if score == nil {
		return ""
	}
	return fmt.Sprintf(" (score %d)", *score)
}

func formatConfidence(conf, endedWith string) string {
	parts := []string{}
	if conf != "" {
		parts = append(parts, conf+" confidence")
	}
	if endedWith != "" {
		parts = append(parts, "ended with "+endedWith)
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

func formatCompactions(total, midTask int) string {
	if total == 0 {
		return "0"
	}
	if midTask == 0 {
		return fmt.Sprintf("%d", total)
	}
	return fmt.Sprintf("%d (%d mid-task)", total, midTask)
}

func formatPressure(p *float64) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", *p*100)
}

func sessionDisplayTime(s db.Session) string {
	if s.EndedAt != nil && *s.EndedAt != "" {
		return *s.EndedAt
	}
	if s.StartedAt != nil && *s.StartedAt != "" {
		return *s.StartedAt
	}
	return s.CreatedAt
}

func shortDate(ts string) string {
	if ts == "" {
		return "-"
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		if t, err = time.Parse(time.RFC3339, ts); err != nil {
			return ts
		}
	}
	return t.Local().Format("2006-01-02")
}

func longDate(ts string) string {
	if ts == "" {
		return "-"
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		if t, err = time.Parse(time.RFC3339, ts); err != nil {
			return ts
		}
	}
	return t.Local().Format("2006-01-02 15:04")
}

func strDeref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

func shortID(id string) string {
	if i := strings.LastIndex(id, "~"); i >= 0 {
		id = id[i+1:]
	}
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func writeJSON(w io.Writer, v any) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fatal("encoding json: %v", err)
	}
}
