// ABOUTME: `agentsview parse-diff` — report-only re-parse of session
// ABOUTME: source files diffed against the stored SQLite archive.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

// parseDiffChangedCap caps the non-verbose changed-sessions
// drill-down so a badly drifted archive doesn't flood the terminal.
const parseDiffChangedCap = 20

// ParseDiffConfig holds parsed CLI options for the parse-diff command.
type ParseDiffConfig struct {
	Agents       []string
	Limit        int
	FailOnChange bool
	JSON         bool
	Verbose      bool

	// Stdout and Stderr default to the process streams. The command
	// wires them to cobra's writers so tests can capture output.
	Stdout io.Writer
	Stderr io.Writer
}

func (c ParseDiffConfig) stdout() io.Writer {
	if c.Stdout != nil {
		return c.Stdout
	}
	return os.Stdout
}

func (c ParseDiffConfig) stderr() io.Writer {
	if c.Stderr != nil {
		return c.Stderr
	}
	return os.Stderr
}

func newParseDiffCommand() *cobra.Command {
	var cfg ParseDiffConfig
	cmd := &cobra.Command{
		Use:   "parse-diff",
		Short: "Re-parse session files and diff against the archive",
		Long: "Re-parses session source files with the current binary, runs\n" +
			"the result through the same normalization sync applies, and\n" +
			"compares it against the stored rows. It does not write\n" +
			"session, skip-cache, or sync-state data; opening the archive\n" +
			"still creates the database and runs schema migrations if\n" +
			"needed.\n\n" +
			"Use it to vet parser changes against the real archive before\n" +
			"bumping the data version, or to detect upstream format drift.\n" +
			"Run it against a quiescent archive: sessions still being\n" +
			"written, and sessions last written through the incremental\n" +
			"append path, can show benign drift against a full re-parse. A\n" +
			"freshly resynced archive gives the cleanest baseline.",
		GroupID:      groupData,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			if cfg.Limit < 0 {
				return fmt.Errorf("--limit must be >= 0")
			}
			_, err := parseDiffAgentTypes(cfg.Agents)
			return err
		},
		Run: func(cmd *cobra.Command, args []string) {
			cfg.Stdout = cmd.OutOrStdout()
			cfg.Stderr = cmd.ErrOrStderr()
			runParseDiff(cfg)
		},
	}
	cmd.Flags().StringArrayVar(&cfg.Agents, "agent", nil,
		"Restrict to these agents (repeatable; default: all file-based agents)")
	cmd.Flags().IntVar(&cfg.Limit, "limit", 0,
		"Re-parse only the N most recently modified source files (0 = all)")
	cmd.Flags().BoolVar(&cfg.FailOnChange, "fail-on-change", false,
		"Exit 1 when sessions changed or files failed to parse")
	cmd.Flags().BoolVar(&cfg.JSON, "json", false,
		"Output the full report as JSON")
	cmd.Flags().BoolVarP(&cfg.Verbose, "verbose", "v", false,
		"Show every field diff for every changed session")
	return cmd
}

func runParseDiff(cfg ParseDiffConfig) {
	if doParseDiff(cfg) {
		os.Exit(1)
	}
}

// doParseDiff runs the report-only comparison and reports whether
// --fail-on-change should turn the run into a non-zero exit. It owns
// the deferred db close so runParseDiff can translate the result into
// an exit code without skipping cleanup. It deliberately skips
// setupLogFile: stdout owns the report and engine warnings belong on
// stderr, matching the health command's diagnostic style.
func doParseDiff(cfg ParseDiffConfig) (failed bool) {
	agents, err := parseDiffAgentTypes(cfg.Agents)
	if err != nil {
		fatal("%v", err)
	}

	appCfg, err := config.LoadMinimal()
	if err != nil {
		fatal("loading config: %v", err)
	}
	if err := os.MkdirAll(appCfg.DataDir, 0o755); err != nil {
		fatal("creating data dir: %v", err)
	}

	database, writeLock := mustOpenWriteDB(context.Background(), appCfg)
	defer closeWriteDB(database, writeLock)

	engine := sync.NewDiffEngine(database, sync.EngineConfig{
		AgentDirs:               appCfg.AgentDirs,
		Machine:                 "local",
		BlockedResultCategories: appCfg.ResultContentBlockedCategories,
	})

	opts := sync.ParseDiffOptions{Agents: agents, Limit: cfg.Limit}
	if !cfg.JSON && isTerminalWriter(cfg.stderr()) {
		opts.Progress = parseDiffProgress(cfg.stderr())
	}

	report, err := engine.ParseDiff(context.Background(), opts)
	if err != nil {
		fatal("parse-diff: %v", err)
	}
	// Stamp the archive identity so an attached JSON report is
	// self-describing.
	report.DBPath = appCfg.DBPath

	if cfg.JSON {
		writeJSON(cfg.stdout(), report)
	} else {
		// The header says "all agents" only when the user did not
		// restrict the run; report.Agents always carries the full
		// resolved list.
		agentsLabel := "all agents"
		if len(agents) > 0 {
			agentsLabel = strings.Join(report.Agents, ", ")
		}
		renderParseDiffReport(
			cfg.stdout(), report, appCfg.DBPath, agentsLabel, cfg.Verbose,
		)
	}
	return parseDiffExitFailure(report, cfg.FailOnChange, cfg.stderr())
}

// parseDiffExitFailure decides whether --fail-on-change should turn the
// run into a non-zero exit and writes the vacuous-run explanation to
// stderr when that is the reason. It is split from doParseDiff's I/O so
// the exit contract is unit-testable, mirroring ParseDiffReport's own
// HasFailures/VacuousResync helpers.
//
// A vacuous run -- this binary's data version is ahead of every
// examined session, so all of them are pending_resync -- detects no
// drift by construction (a resync rewrites every row), so a clean
// result is not evidence the parser is unchanged. Treat it as a gate
// failure rather than a green light, and explain the non-zero exit on
// stderr: the stdout warning is absent under --json and easy to miss in
// CI logs.
func parseDiffExitFailure(
	report *sync.ParseDiffReport, failOnChange bool, stderr io.Writer,
) bool {
	if !failOnChange {
		return false
	}
	vacuous := report.VacuousResync()
	if vacuous {
		fmt.Fprintln(stderr,
			"parse-diff: --fail-on-change failed: the run was vacuous "+
				"(every examined session is pending resync because this "+
				"binary's data version is ahead of the whole archive), so "+
				"no parser drift could be detected. Re-run against a freshly "+
				"resynced archive, or with a binary built before the "+
				"data-version bump, to vet the change.")
	}
	return report.HasFailures() || vacuous
}

// parseDiffProgress returns a simple stderr counter for
// ParseDiffOptions.Progress. It rewrites one line in place and
// terminates it once the last file completes.
func parseDiffProgress(w io.Writer) func(done, total int) {
	return func(done, total int) {
		fmt.Fprintf(w, "\rRe-parsing files: %d/%d", done, total)
		if done >= total {
			fmt.Fprintln(w)
		}
	}
}

// isTerminalWriter reports whether w is an interactive terminal, so
// the carriage-return progress ticker never spams piped output or CI
// logs.
func isTerminalWriter(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// parseDiffAgentTypes validates --agent values against the parser
// registry and returns the corresponding agent types, de-duplicated
// in flag order. An empty input means "every supported agent" and
// returns nil.
func parseDiffAgentTypes(names []string) ([]parser.AgentType, error) {
	if len(names) == 0 {
		return nil, nil
	}
	out := make([]parser.AgentType, 0, len(names))
	seen := make(map[parser.AgentType]bool, len(names))
	for _, raw := range names {
		name := strings.ToLower(strings.TrimSpace(raw))
		def, ok := parser.AgentByType(parser.AgentType(name))
		if !ok {
			return nil, fmt.Errorf(
				"unknown agent %q (supported: %s)",
				raw,
				strings.Join(parseDiffSupportedAgents(), ", "),
			)
		}
		if !parseDiffAgentSupported(def) {
			return nil, fmt.Errorf(
				"agent %q is not supported by parse-diff "+
					"(no on-disk source to re-parse)",
				raw,
			)
		}
		if !seen[def.Type] {
			seen[def.Type] = true
			out = append(out, def.Type)
		}
	}
	return out, nil
}

// parseDiffSupportedAgents lists the agent types parse-diff can re-parse.
func parseDiffSupportedAgents() []string {
	var names []string
	for _, def := range parser.Registry {
		if parseDiffAgentSupported(def) {
			names = append(names, string(def.Type))
		}
	}
	return names
}

func parseDiffAgentSupported(def parser.AgentDef) bool {
	if !def.FileBased {
		return false
	}
	switch parser.ProviderMigrationModes()[def.Type] {
	case parser.ProviderMigrationProviderAuthoritative:
		_, ok := parser.ProviderFactoryByType(def.Type)
		return ok
	default:
		return false
	}
}

// renderParseDiffReport writes the human-readable report. An empty
// archive renders a zero-count summary with no tables. Every value
// that originates in session files or archive rows (IDs, paths,
// agents, field values, error reasons) passes through
// sanitizeTerminal: session content can carry ESC/OSC sequences that
// would otherwise reach the terminal. JSON output is left raw.
func renderParseDiffReport(
	w io.Writer, r *sync.ParseDiffReport, dbPath, agentsLabel string,
	verbose bool,
) {
	fmt.Fprintf(w,
		"Parse diff: %d files re-parsed (%s) against %s (data version %d)\n",
		r.FilesExamined, sanitizeTerminal(agentsLabel),
		sanitizeTerminal(dbPath), r.DataVersion)
	if r.FilesLimited {
		fmt.Fprintln(w,
			"Note: --limit truncated discovery; totals cover a sample.")
	}
	fmt.Fprintln(w)

	if r.VacuousResync() {
		fmt.Fprintln(w,
			"Warning: this binary's data version is ahead of every "+
				"examined session, so all of them are pending resync "+
				"and no drift can be detected. Run parse-diff with a "+
				"binary built before the data-version bump, or against "+
				"a freshly resynced archive, to vet a parser change.")
		fmt.Fprintln(w)
	}

	renderParseDiffSummary(w, r.Totals)
	renderParseDiffFieldCounts(w, r.FieldCounts)
	renderParseDiffParseErrors(w, r.Sessions)
	renderParseDiffChanged(w, r.Sessions, verbose)
	renderParseDiffRaced(w, r.Sessions, verbose)
	if verbose {
		renderParseDiffPendingResync(w, r.Sessions)
	}

	fmt.Fprintf(w, "%d sessions changed, %d identical.\n",
		r.Totals.Changed, r.Totals.Identical)
}

// renderParseDiffPendingResync lists pending-resync sessions that
// carry attached field diffs (verbose only). These are not counted as
// drift — a resync rewrites them — but showing the diffs lets an
// operator see what would change, which is the only signal available
// when the whole run is vacuous (data version ahead of the archive).
func renderParseDiffPendingResync(w io.Writer, sessions []sync.SessionDiff) {
	var pending []sync.SessionDiff
	for _, s := range sessions {
		if s.Class == sync.DiffPendingResync && len(s.Fields) > 0 {
			pending = append(pending, s)
		}
	}
	if len(pending) == 0 {
		return
	}
	fmt.Fprintln(w,
		"Pending-resync sessions (not counted; resync rewrites these)")
	for _, s := range pending {
		renderParseDiffSessionVerbose(w, s)
	}
	fmt.Fprintln(w)
}

// renderParseDiffParseErrors lists files the current binary could not
// parse, with the path and error. Parse errors trip --fail-on-change,
// so the human report must explain why a run failed; the summary count
// alone is not actionable.
func renderParseDiffParseErrors(w io.Writer, sessions []sync.SessionDiff) {
	var errs []sync.SessionDiff
	for _, s := range sessions {
		if s.Class == sync.DiffParseError {
			errs = append(errs, s)
		}
	}
	if len(errs) == 0 {
		return
	}
	fmt.Fprintln(w, "Parse errors")
	for _, s := range errs {
		path := s.FilePath
		if path == "" {
			path = "(unknown file)"
		}
		fmt.Fprintf(w, "  %s  %s\n    %s\n",
			sanitizeTerminal(s.Agent), sanitizeTerminal(path),
			sanitizeTerminal(s.Reason))
	}
	fmt.Fprintln(w)
}

// renderParseDiffSummary prints one line per non-zero total, with
// short explanations for the non-obvious buckets. Examined always
// prints so an empty archive still renders a summary.
func renderParseDiffSummary(w io.Writer, t sync.ParseDiffTotals) {
	fmt.Fprintln(w, "Summary")
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	line := func(name string, n int, note string) {
		if n == 0 && name != "Examined" {
			return
		}
		fmt.Fprintf(tw, "  %s\t%d\t%s\n", name, n, note)
	}
	line("Examined", t.Examined,
		"(stored sessions compared against a fresh parse)")
	line("Identical", t.Identical, "")
	line("Changed", t.Changed, "")
	line("Pending resync", t.PendingResync,
		"(stored data version behind; next resync rewrites these)")
	line("New on disk", t.NewOnDisk,
		"(no stored row; sync would add them)")
	line("Skipped", t.Skipped,
		"(source not re-parsed: missing, remote, trashed, or not sampled)")
	line("Raced", t.Raced,
		"(source changed mid-run; inconclusive, not counted as drift)")
	line("Excluded by parser", t.ExcludedByParser,
		"(parser intentionally drops these; sync would delete them)")
	line("Parse errors", t.ParseErrors,
		"(current binary failed to parse the source file)")
	line("Needs retry", t.NeedsRetry,
		"(transient low-fidelity parse; differences expected)")
	line("Informational only", t.InformationalOnly,
		"(identical except informational diffs)")
	tw.Flush()
	fmt.Fprintln(w)
}

// renderParseDiffFieldCounts prints the changed-field histogram,
// sorted by count descending with alphabetical tie-breaks.
func renderParseDiffFieldCounts(w io.Writer, counts map[string]int) {
	if len(counts) == 0 {
		return
	}
	fmt.Fprintln(w, "Changed fields (sessions affected)")
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	for _, e := range sortedIntMap(counts) {
		fmt.Fprintf(tw, "  %s\t%d\n", sanitizeTerminal(e.key), e.val)
	}
	tw.Flush()
	fmt.Fprintln(w)
}

// renderParseDiffChanged prints the changed-sessions drill-down:
// one compact line per session, capped unless verbose; verbose
// prints a block per session with every field diff.
func renderParseDiffChanged(
	w io.Writer, sessions []sync.SessionDiff, verbose bool,
) {
	var changed []sync.SessionDiff
	for _, s := range sessions {
		if s.Class == sync.DiffChanged {
			changed = append(changed, s)
		}
	}
	if len(changed) == 0 {
		return
	}
	fmt.Fprintln(w, "Changed sessions")
	if verbose {
		for _, s := range changed {
			renderParseDiffSessionVerbose(w, s)
		}
		fmt.Fprintln(w)
		return
	}
	shown := changed
	if len(shown) > parseDiffChangedCap {
		shown = shown[:parseDiffChangedCap]
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	for _, s := range shown {
		fmt.Fprintf(tw, "  %s\t%s\t%s\n",
			sanitizeTerminal(s.Agent),
			sanitizeTerminal(shortID(s.SessionID)),
			sanitizeTerminal(parseDiffFieldSummary(s.Fields)))
	}
	tw.Flush()
	if extra := len(changed) - len(shown); extra > 0 {
		fmt.Fprintf(w, "  ... (%d more; use --verbose or --json)\n",
			extra)
	}
	fmt.Fprintln(w)
}

// renderParseDiffRaced lists sessions whose would-be change was
// reclassified as a live-write skew: the on-disk source advanced past
// the snapshot mtime, so the change is a torn comparison rather than
// parser drift. These never trip --fail-on-change, but surfacing them
// tells the operator a comparison was inconclusive and the run should
// be repeated against a quiescent archive. Compact by default; verbose
// shows the masked field diffs.
func renderParseDiffRaced(
	w io.Writer, sessions []sync.SessionDiff, verbose bool,
) {
	var raced []sync.SessionDiff
	for _, s := range sessions {
		if s.Class == sync.DiffRaced {
			raced = append(raced, s)
		}
	}
	if len(raced) == 0 {
		return
	}
	fmt.Fprintln(w,
		"Raced sessions (source changed mid-run; not counted as drift)")
	if verbose {
		for _, s := range raced {
			renderParseDiffSessionVerbose(w, s)
		}
		fmt.Fprintln(w)
		return
	}
	shown := raced
	if len(shown) > parseDiffChangedCap {
		shown = shown[:parseDiffChangedCap]
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	for _, s := range shown {
		fmt.Fprintf(tw, "  %s\t%s\t%s\n",
			sanitizeTerminal(s.Agent),
			sanitizeTerminal(shortID(s.SessionID)),
			sanitizeTerminal(parseDiffFieldSummary(s.Fields)))
	}
	tw.Flush()
	if extra := len(raced) - len(shown); extra > 0 {
		fmt.Fprintf(w, "  ... (%d more; use --verbose or --json)\n",
			extra)
	}
	fmt.Fprintln(w)
}

// renderParseDiffSessionVerbose prints one changed session with
// every field diff: Field, Stored -> Parsed, Detail, and an
// [informational] tag where applicable.
func renderParseDiffSessionVerbose(w io.Writer, s sync.SessionDiff) {
	fmt.Fprintf(w, "  %s  %s",
		sanitizeTerminal(s.Agent), sanitizeTerminal(s.SessionID))
	if s.FilePath != "" {
		fmt.Fprintf(w, "  %s", sanitizeTerminal(s.FilePath))
	}
	fmt.Fprintln(w)
	for _, f := range s.Fields {
		fmt.Fprintf(w, "    %s: %s -> %s",
			sanitizeTerminal(f.Field),
			sanitizeTerminal(f.Stored), sanitizeTerminal(f.Parsed))
		if f.Detail != "" {
			fmt.Fprintf(w, " (%s)", sanitizeTerminal(f.Detail))
		}
		if f.Informational {
			fmt.Fprint(w, " [informational]")
		}
		fmt.Fprintln(w)
	}
}

// parseDiffFieldSummary renders the compact field list for one
// changed-session line: the non-informational field names in diff
// order. If every diff is informational (defensive; such sessions
// are classified identical), the names are tagged instead.
func parseDiffFieldSummary(fields []sync.FieldDiff) string {
	names := make([]string, 0, len(fields))
	for _, f := range fields {
		if !f.Informational {
			names = append(names, f.Field)
		}
	}
	if len(names) == 0 {
		for _, f := range fields {
			names = append(names, f.Field+" [informational]")
		}
	}
	return strings.Join(names, ", ")
}
