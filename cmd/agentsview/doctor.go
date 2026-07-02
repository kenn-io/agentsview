package main

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

const doctorDebugLineLimit = 80

type doctorDataVersionCount struct {
	Version int
	Count   int
}

type doctorAgentRoot struct {
	Agent          parser.AgentType
	Path           string
	UserConfigured bool
	Exists         bool
	Err            error
}

type doctorDBInspection struct {
	DBExists                 bool
	DBReadable               bool
	DBError                  error
	UserVersion              *int
	SessionCounts            []doctorDataVersionCount
	SessionCountsErr         error
	AntigravityCLITotal      int
	AntigravityCLISummary    int
	AntigravityUnknownSchema int
	AntigravityCountsErr     error
}

type doctorSyncReport struct {
	Config config.Config
	doctorDBInspection
	TempFiles           []string
	AgentRoots          []doctorAgentRoot
	DebugLines          []string
	DebugLogErr         error
	HasResyncFailureLog bool
}

func newDoctorCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "doctor",
		Short:        "Collect support diagnostics",
		GroupID:      groupMeta,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newDoctorSyncCommand())
	return cmd
}

func newDoctorSyncCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "sync",
		Short:        "Diagnose startup sync decisions",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.LoadReadOnly()
			if err != nil {
				return err
			}
			return runDoctorSync(cmd.OutOrStdout(), cfg)
		},
	}
}

func runDoctorSync(w io.Writer, cfg config.Config) error {
	report := collectDoctorSyncReport(cfg)
	writeDoctorSyncReport(w, report)
	return nil
}

func collectDoctorSyncReport(cfg config.Config) doctorSyncReport {
	report := doctorSyncReport{Config: cfg}

	report.doctorDBInspection = inspectDoctorDB(cfg.DBPath)
	report.TempFiles = listDoctorResyncTempFiles(cfg.DBPath)
	report.AgentRoots = collectDoctorAgentRoots(cfg)
	report.DebugLines, report.DebugLogErr = readDoctorDebugLines(
		filepath.Join(cfg.DataDir, "debug.log"),
	)
	report.HasResyncFailureLog = doctorDebugLinesMentionResyncFailure(
		report.DebugLines,
	)
	return report
}

func inspectDoctorDB(path string) doctorDBInspection {
	var insp doctorDBInspection
	info, err := os.Stat(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			insp.DBError = err
		}
		return insp
	}
	insp.DBExists = true
	if info.IsDir() {
		insp.DBError = fmt.Errorf("database path is a directory")
		return insp
	}

	conn, err := sql.Open("sqlite3", doctorReadOnlyDSN(path))
	if err != nil {
		insp.DBError = err
		return insp
	}
	defer conn.Close()

	var version int
	if err := conn.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		insp.DBError = err
		return insp
	}
	insp.DBReadable = true
	insp.UserVersion = &version

	rows, err := conn.Query(
		"SELECT data_version, COUNT(*) FROM sessions " +
			"GROUP BY data_version ORDER BY data_version",
	)
	if err != nil {
		insp.SessionCountsErr = err
		return insp
	}
	defer rows.Close()

	for rows.Next() {
		var row doctorDataVersionCount
		if err := rows.Scan(&row.Version, &row.Count); err != nil {
			insp.SessionCountsErr = err
			return insp
		}
		insp.SessionCounts = append(insp.SessionCounts, row)
	}
	if err := rows.Err(); err != nil {
		insp.SessionCountsErr = err
		return insp
	}

	// One scan collects both Antigravity operator counts: antigravity-cli
	// summary-mode sessions (transcript fidelity) and sessions on an
	// unrecognized schema across both Antigravity agents (decode confidence).
	row := conn.QueryRow(
		"SELECT " +
			"COUNT(*) FILTER (WHERE agent = 'antigravity-cli'), " +
			"COUNT(*) FILTER (WHERE agent = 'antigravity-cli' " +
			"AND transcript_fidelity = 'summary'), " +
			"COUNT(*) FILTER (WHERE agent IN ('antigravity', 'antigravity-cli') " +
			"AND source_version LIKE 'agy-schema:%') " +
			"FROM sessions",
	)
	var total, summary, unknownSchema int
	if err := row.Scan(&total, &summary, &unknownSchema); err != nil {
		insp.AntigravityCountsErr = err
		return insp
	}
	insp.AntigravityCLITotal = total
	insp.AntigravityCLISummary = summary
	insp.AntigravityUnknownSchema = unknownSchema
	return insp
}

func doctorReadOnlyDSN(path string) string {
	params := url.Values{}
	params.Set("mode", "ro")
	params.Set("_busy_timeout", "5000")
	params.Set("_foreign_keys", "ON")
	return path + "?" + params.Encode()
}

func listDoctorResyncTempFiles(dbPath string) []string {
	matches, err := filepath.Glob(dbPath + "-resync*")
	if err != nil {
		return nil
	}
	sort.Strings(matches)
	return matches
}

func collectDoctorAgentRoots(cfg config.Config) []doctorAgentRoot {
	roots := make([]doctorAgentRoot, 0)
	for _, def := range parser.Registry {
		userConfigured := cfg.IsUserConfigured(def.Type)
		for _, dir := range cfg.ResolveDirs(def.Type) {
			root := doctorAgentRoot{
				Agent:          def.Type,
				Path:           dir,
				UserConfigured: userConfigured,
			}
			if _, err := os.Stat(dir); err != nil {
				root.Exists = false
				if !errors.Is(err, os.ErrNotExist) {
					root.Err = err
				}
			} else {
				root.Exists = true
			}
			roots = append(roots, root)
		}
	}
	return roots
}

func readDoctorDebugLines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var lines []string
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		if doctorDebugLineRelevant(line) {
			lines = append(lines, line)
		}
	}
	if len(lines) > doctorDebugLineLimit {
		lines = lines[len(lines)-doctorDebugLineLimit:]
	}
	return lines, nil
}

func doctorDebugLineRelevant(line string) bool {
	lower := strings.ToLower(line)
	for _, needle := range []string{
		"data version",
		"resync:",
		"aborting swap",
		"resync aborted",
		"sync complete",
		"failed",
		"warning",
	} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

func doctorDebugLinesMentionResyncFailure(lines []string) bool {
	for _, line := range lines {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "resync aborted") ||
			strings.Contains(lower, "aborting swap") ||
			strings.Contains(lower, "resync swap failed") ||
			strings.Contains(lower, "resync failed") {
			return true
		}
	}
	return false
}

func writeDoctorSyncReport(w io.Writer, report doctorSyncReport) {
	currentVersion := db.CurrentDataVersion()

	fmt.Fprintln(w, "Sync Diagnostics")
	fmt.Fprintf(w, "Version: %s (commit %s, built %s)\n",
		version, commit, buildDate)
	fmt.Fprintf(w, "Data directory: %s\n", report.Config.DataDir)
	fmt.Fprintf(w, "Database: %s\n", report.Config.DBPath)
	fmt.Fprintf(w, "Database exists: %s\n",
		doctorDatabaseExistsLabel(report))
	if report.DBError != nil {
		fmt.Fprintf(w, "Database readable: no (%v)\n", report.DBError)
	} else {
		fmt.Fprintf(w, "Database readable: %s\n",
			doctorYesNo(report.DBReadable))
	}
	if report.UserVersion == nil {
		fmt.Fprintln(w, "SQLite user_version: unavailable")
	} else {
		fmt.Fprintf(w, "SQLite user_version: %d\n", *report.UserVersion)
	}
	fmt.Fprintf(w, "Binary data version: %d\n", currentVersion)
	fmt.Fprintf(w, "Startup sync decision: %s\n",
		doctorStartupDecision(report, currentVersion))

	writeDoctorSessionCounts(w, report)
	writeDoctorSummaryMode(w, report)
	writeDoctorUnknownSchema(w, report)
	writeDoctorTempFiles(w, report.TempFiles)
	writeDoctorAgentRoots(w, report.AgentRoots)
	writeDoctorDebugEvidence(w, report)
	fmt.Fprintf(w, "Likely cause: %s\n",
		doctorLikelyCause(report, currentVersion))
}

func doctorStartupDecision(
	report doctorSyncReport, currentVersion int,
) string {
	if report.DBError != nil {
		return "unknown (database could not be inspected)"
	}
	if report.UserVersion == nil {
		if !report.DBExists {
			return "normal initial sync (database will be created)"
		}
		return "unknown (database version could not be read)"
	}
	if *report.UserVersion < currentVersion {
		return "full data-version resync required"
	}
	if *report.UserVersion > currentVersion {
		return "refuse startup (database requires newer agentsview)"
	}
	return "normal initial sync (no data-version resync)"
}

func writeDoctorSessionCounts(w io.Writer, report doctorSyncReport) {
	fmt.Fprintln(w, "Session data versions:")
	if report.DBError != nil {
		fmt.Fprintf(w, "  unavailable: %v\n", report.DBError)
		return
	}
	if report.SessionCountsErr != nil {
		fmt.Fprintf(w, "  unavailable: %v\n", report.SessionCountsErr)
		return
	}
	if len(report.SessionCounts) == 0 {
		fmt.Fprintln(w, "  none")
		return
	}
	for _, row := range report.SessionCounts {
		fmt.Fprintf(w, "  version %d: %d\n", row.Version, row.Count)
	}
}

func writeDoctorSummaryMode(w io.Writer, report doctorSyncReport) {
	if report.AntigravityCountsErr != nil || report.AntigravityCLISummary == 0 {
		return
	}
	fmt.Fprintf(w,
		"antigravity-cli: %d sessions, %d in summary mode\n"+
			"  -> install agy-reader for full transcripts: "+
			"https://github.com/mjacobs/agy-reader\n",
		report.AntigravityCLITotal, report.AntigravityCLISummary)
}

// writeDoctorUnknownSchema surfaces Antigravity sessions decoded from an
// unrecognized (newer) schema fingerprint (source_version "agy-schema:..."),
// where the heuristic decode may be incomplete or wrong. It stays silent on a
// clean archive or when the antigravity-counts query failed.
func writeDoctorUnknownSchema(w io.Writer, report doctorSyncReport) {
	if report.AntigravityCountsErr != nil ||
		report.AntigravityUnknownSchema == 0 {
		return
	}
	fmt.Fprintf(w,
		"%d session(s) on unrecognized Antigravity schema (agy-schema:...)\n"+
			"  -> a newer Antigravity build changed the schema; "+
			"the decode is heuristic and may be incomplete\n",
		report.AntigravityUnknownSchema)
}

func writeDoctorTempFiles(w io.Writer, files []string) {
	fmt.Fprintln(w, "Resync temp files:")
	if len(files) == 0 {
		fmt.Fprintln(w, "  none")
		return
	}
	for _, file := range files {
		fmt.Fprintf(w, "  %s\n", file)
	}
}

func writeDoctorAgentRoots(w io.Writer, roots []doctorAgentRoot) {
	fmt.Fprintln(w, "Agent roots:")
	if len(roots) == 0 {
		fmt.Fprintln(w, "  none")
		return
	}
	for _, root := range roots {
		source := "default"
		if root.UserConfigured {
			source = "configured"
		}
		status := "ok"
		if root.Err != nil {
			status = "error: " + root.Err.Error()
		} else if !root.Exists {
			status = "missing"
		}
		fmt.Fprintf(w, "  %s: %s (%s, %s)\n",
			root.Agent, root.Path, status, source)
	}
}

func writeDoctorDebugEvidence(
	w io.Writer, report doctorSyncReport,
) {
	fmt.Fprintln(w, "Recent debug.log evidence:")
	if report.DebugLogErr != nil {
		fmt.Fprintf(w, "  unavailable: %v\n", report.DebugLogErr)
		return
	}
	if len(report.DebugLines) == 0 {
		fmt.Fprintln(w, "  none")
		return
	}
	for _, line := range report.DebugLines {
		fmt.Fprintf(w, "  %s\n", line)
	}
}

func doctorLikelyCause(
	report doctorSyncReport, currentVersion int,
) string {
	if report.DBError != nil {
		return "database could not be inspected; check database path and permissions"
	}
	if !report.DBExists {
		return "database does not exist yet; the next startup will create it"
	}
	if report.UserVersion == nil {
		return "database version could not be read; inspect database readability and permissions"
	}
	if *report.UserVersion < currentVersion {
		if report.HasResyncFailureLog {
			return "previous data-version resync likely aborted before completion"
		}
		if doctorHasMissingUserConfiguredRoot(report.AgentRoots) {
			return "one or more configured agent roots are missing; resync may be aborting during discovery"
		}
		return "SQLite user_version is stale; inspect debug.log for resync aborts or failures"
	}
	if *report.UserVersion > currentVersion {
		return "SQLite user_version is newer than this binary. Run \"agentsview update\" or install the latest AgentsView release before serving or syncing"
	}
	return "data-version resync is not expected; Running initial sync... is normal incremental startup work"
}

func doctorDatabaseExistsLabel(report doctorSyncReport) string {
	if report.DBError != nil && !report.DBExists {
		return "unknown"
	}
	return doctorYesNo(report.DBExists)
}

func doctorHasMissingUserConfiguredRoot(roots []doctorAgentRoot) bool {
	for _, root := range roots {
		if root.UserConfigured && !root.Exists {
			return true
		}
	}
	return false
}

func doctorYesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}
