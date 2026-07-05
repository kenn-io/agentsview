package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/server"
	"golang.org/x/term"
)

const (
	groupCore  = "core"
	groupData  = "data"
	groupUsage = "usage"
	groupMeta  = "meta"
)

const dataVersionTooNewExitCode = 3

type cliExitError struct {
	code   int
	err    error
	silent bool
}

func (e *cliExitError) Error() string {
	return e.err.Error()
}

func (e *cliExitError) Unwrap() error {
	return e.err
}

func withExitCode(err error, code int) error {
	if err == nil {
		return nil
	}
	return &cliExitError{code: code, err: err}
}

func withSilentExitCode(err error, code int) error {
	if err == nil {
		return nil
	}
	return &cliExitError{code: code, err: err, silent: true}
}

func exitCodeFromError(err error) int {
	var exitErr *cliExitError
	if errors.As(err, &exitErr) {
		return exitErr.code
	}
	return 1
}

func isSilentExitError(err error) bool {
	var exitErr *cliExitError
	if !errors.As(err, &exitErr) || exitErr == nil {
		return false
	}
	return exitErr.silent
}

func newRootCommand() *cobra.Command {
	var showVersion bool

	root := &cobra.Command{
		Use:           "agentsview",
		Short:         "Local web viewer for AI agent sessions",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			if showVersion {
				printVersion(cmd.OutOrStdout())
				return
			}
			_ = cmd.Help()
		},
	}
	root.AddGroup(
		&cobra.Group{ID: groupCore, Title: "Core Commands:"},
		&cobra.Group{ID: groupData, Title: "Data Commands:"},
		&cobra.Group{ID: groupUsage, Title: "Usage Commands:"},
		&cobra.Group{ID: groupMeta, Title: "Other Commands:"},
	)
	root.SetCompletionCommandGroupID(groupMeta)
	root.SetHelpCommandGroupID(groupMeta)

	root.Flags().BoolVarP(
		&showVersion,
		"version",
		"v",
		false,
		"Show version information",
	)

	root.AddCommand(newServeCommand())
	root.AddCommand(newSyncCommand())
	root.AddCommand(newPruneCommand())
	root.AddCommand(newUpdateCommand())
	root.AddCommand(newTokenUseCommand())
	root.AddCommand(newImportCommand())
	root.AddCommand(newExportCommand())
	root.AddCommand(newProjectsCommand())
	root.AddCommand(newHealthCommand())
	root.AddCommand(newUsageCommand())
	root.AddCommand(newActivityCommand())
	root.AddCommand(newPGCommand())
	root.AddCommand(newDuckDBCommand())
	root.AddCommand(newEmbeddingsCommand())
	root.AddCommand(newSessionCommand())
	root.AddCommand(newMCPCommand())
	root.AddCommand(newStatsCommand())
	root.AddCommand(newParseDiffCommand())
	root.AddCommand(newClassifierCommand())
	root.AddCommand(newSecretsCommand())
	root.AddCommand(newDoctorCommand())
	root.AddCommand(newVersionCommand())
	root.AddCommand(newOpenAPICommand())

	defaultHelp := root.HelpFunc()
	root.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		if cmd == root {
			writeRootHelp(cmd.OutOrStdout(), root)
			return
		}
		defaultHelp(cmd, args)
	})

	return root
}

func newServeCommand() *cobra.Command {
	var background bool
	var checkDataVersion bool
	var replace bool
	var pprofEnabled bool
	cmd := &cobra.Command{
		Use:          "serve",
		Short:        "Start server",
		GroupID:      groupCore,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if checkDataVersion {
				cfg, err := config.LoadReadOnly()
				if err != nil {
					return err
				}
				return runServeDataVersionCheck(cfg)
			}
			if background {
				// Acquire the launch lock before loading config; config
				// loading writes config.toml and must be single-writer
				// across concurrent launches.
				runServeBackgroundCommand(
					cmd, serveReplacementOptions{Replace: replace},
				)
				return nil
			}
			runServe(mustLoadConfig(cmd), serveOptions{
				ReplaceDaemon:  replace,
				NoSyncExplicit: cmd.Flags().Changed("no-sync"),
				Pprof:          pprofEnabled,
			})
			return nil
		},
	}
	cmd.Flags().BoolVar(
		&background,
		"background",
		false,
		"Start server in the background and return to the shell",
	)
	cmd.Flags().BoolVar(
		&replace,
		"replace",
		false,
		"Replace a running local daemon before starting",
	)
	cmd.Flags().BoolVar(
		&checkDataVersion,
		"check-data-version",
		false,
		"Check whether the configured database is compatible with this binary",
	)
	_ = cmd.Flags().MarkHidden("check-data-version")
	cmd.Flags().BoolVar(
		&pprofEnabled,
		"pprof",
		false,
		"Serve net/http/pprof under /debug/pprof (developer use)",
	)
	_ = cmd.Flags().MarkHidden("pprof")
	config.RegisterServePFlags(cmd.Flags())
	cmd.AddCommand(newServeStatusCommand())
	cmd.AddCommand(newServeStopCommand())
	return cmd
}

func runServeDataVersionCheck(cfg config.Config) error {
	err := db.CheckDataVersion(cfg.DBPath)
	if db.IsDataVersionTooNew(err) {
		return withExitCode(err, dataVersionTooNewExitCode)
	}
	return err
}

func newServeStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "status",
		Short:        "Show whether a server is running and where to reach it",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			runServeStatus(mustLoadConfig(cmd))
		},
	}
}

func newServeStopCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "stop",
		Short:        "Stop the running server",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			runServeStop(mustLoadConfig(cmd))
		},
	}
}

func newOpenAPICommand() *cobra.Command {
	return &cobra.Command{
		Use:          "openapi",
		Short:        "Print OpenAPI 3.1 schema",
		GroupID:      groupMeta,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			spec, err := server.OpenAPIJSON(server.VersionInfo{
				Version:   version,
				Commit:    commit,
				BuildDate: buildDate,
			})
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(append(spec, '\n'))
			return err
		},
	}
}

func newSyncCommand() *cobra.Command {
	var cfg SyncConfig
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Sync session data without serving",
		Long: "Sync session data into the local database without starting the\n" +
			"HTTP server.\n\n" +
			"With no --host, sync runs the local sync and then fans out to\n" +
			"every host listed in the [[remote_hosts]] array in config.toml,\n" +
			"syncing each by its configured transport. A failure on one\n" +
			"configured host is logged and the run continues; the command\n" +
			"exits non-zero if any configured host failed.\n\n" +
			"With --host, syncs only that host. A running local daemon may use a\n" +
			"matching configured remote_hosts entry and transport; otherwise,\n" +
			"ad hoc --host sync uses your existing SSH configuration and requires\n" +
			"key-based (passwordless) auth; it never prompts for a password.",
		GroupID:      groupCore,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			if cfg.Host == "" {
				if cmd.Flags().Changed("user") ||
					cmd.Flags().Changed("port") {
					return fmt.Errorf(
						"--user and --port require --host",
					)
				}
			}
			return nil
		},
		Run: func(cmd *cobra.Command, args []string) {
			runSync(cfg)
		},
	}
	cmd.Flags().BoolVar(
		&cfg.Full, "full", false,
		"Force a full resync regardless of data version",
	)
	cmd.Flags().StringVar(
		&cfg.Host, "host", "",
		"SSH hostname for remote sync",
	)
	cmd.Flags().StringVar(
		&cfg.User, "user", "",
		"SSH user for remote sync",
	)
	cmd.Flags().IntVar(
		&cfg.Port, "port", 0,
		"SSH port for remote sync (default: 22)",
	)
	cmd.Flags().StringVar(
		&cfg.CPUProfile, "cpuprofile", "",
		"Write CPU profile to file (developer use)",
	)
	cmd.Flags().StringVar(
		&cfg.MemProfile, "memprofile", "",
		"Write memory profile to file (developer use)",
	)
	cmd.Flags().StringVar(
		&cfg.Trace, "trace", "",
		"Write runtime trace to file (developer use)",
	)
	for _, name := range []string{"cpuprofile", "memprofile", "trace"} {
		if err := cmd.Flags().MarkHidden(name); err != nil {
			panic(err)
		}
	}
	return cmd
}

func newPruneCommand() *cobra.Command {
	var project, before, firstMessage string
	var maxMessages int
	var dryRun, yes bool
	cmd := &cobra.Command{
		Use:          "prune",
		Short:        "Delete sessions matching filters",
		GroupID:      groupCore,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			var mm *int
			if maxMessages != -1 {
				mm = &maxMessages
			}
			runPrune(PruneConfig{
				Filter: db.PruneFilter{
					Project:      project,
					MaxMessages:  mm,
					Before:       before,
					FirstMessage: firstMessage,
				},
				DryRun: dryRun,
				Yes:    yes,
			})
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Sessions whose project contains this substring")
	cmd.Flags().IntVar(&maxMessages, "max-messages", -1, "Sessions with at most N user messages")
	cmd.Flags().StringVar(&before, "before", "", "Sessions that ended before this date (YYYY-MM-DD)")
	cmd.Flags().StringVar(&firstMessage, "first-message", "", "Sessions whose first message starts with this text")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show what would be pruned without deleting")
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip confirmation prompt")
	return cmd
}

func newUpdateCommand() *cobra.Command {
	var cfg UpdateConfig
	cmd := &cobra.Command{
		Use:          "update",
		Short:        "Check for and install updates",
		GroupID:      groupMeta,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			runUpdate(cfg)
		},
	}
	cmd.Flags().BoolVar(&cfg.Check, "check", false, "Check for updates without installing")
	cmd.Flags().BoolVar(&cfg.Yes, "yes", false, "Install without confirmation prompt")
	cmd.Flags().BoolVar(&cfg.Force, "force", false, "Force check (ignore cache)")
	return cmd
}

func newTokenUseCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "token-use <session-id>",
		Short:        "Show token usage for a session (JSON)",
		GroupID:      groupData,
		SilenceUsage: true,
		Args:         cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			runTokenUse(args)
		},
	}
}

func newImportCommand() *cobra.Command {
	var importType string
	cmd := &cobra.Command{
		Use:          "import --type <type> <path>",
		Short:        "Import conversations",
		GroupID:      groupData,
		SilenceUsage: true,
		Args:         cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			runImport(ImportConfig{Type: importType, Path: args[0]})
		},
	}
	cmd.Flags().StringVar(&importType, "type", "", "Import type: claude-ai, chatgpt")
	_ = cmd.MarkFlagRequired("type")
	return cmd
}

func newProjectsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "projects",
		Short:        "List projects with session counts",
		GroupID:      groupCore,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			runProjects(outputFormat(cmd) == "json")
		},
	}
	registerFormatFlags(cmd.Flags())
	return cmd
}

func newHealthCommand() *cobra.Command {
	var cfg HealthConfig
	cmd := &cobra.Command{
		Use:   "health [session-id]",
		Short: "Show session health and signals",
		Long: "Without arguments, lists the most recent " +
			"sessions with grade and outcome columns. " +
			"With a session ID, prints detailed signal " +
			"counts for that session.",
		GroupID:      groupCore,
		SilenceUsage: true,
		Args:         cobra.MaximumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			cfg.JSON = outputFormat(cmd) == "json"
			runHealth(cmd, args, cfg)
		},
	}
	registerFormatFlags(cmd.Flags())
	cmd.Flags().IntVar(&cfg.Limit, "limit",
		defaultHealthLimit,
		"Number of sessions to list (max 500)")
	return cmd
}

func newUsageCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "usage",
		Short:        "Token cost tracking and reporting",
		GroupID:      groupUsage,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newUsageDailyCommand())
	cmd.AddCommand(newUsageStatuslineCommand())
	cmd.AddCommand(newUsageCursorCommand())
	return cmd
}

func newUsageDailyCommand() *cobra.Command {
	var cfg UsageDailyConfig
	cmd := &cobra.Command{
		Use:          "daily",
		Short:        "Daily cost summary",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			cfg.JSON = outputFormat(cmd) == "json"
			runUsageDaily(cfg)
		},
	}
	registerFormatFlags(cmd.Flags())
	cmd.Flags().StringVar(&cfg.Since, "since", "", "Start of window (duration like 28d, or YYYY-MM-DD)")
	cmd.Flags().StringVar(&cfg.Until, "until", "", "End of window (duration like 28d, or YYYY-MM-DD)")
	cmd.Flags().BoolVar(&cfg.All, "all", false, "Include all history (overrides default 30-day window)")
	cmd.Flags().StringVar(&cfg.Agent, "agent", "", "Filter by agent name")
	cmd.Flags().BoolVar(&cfg.Breakdown, "breakdown", false, "Show per-model breakdown rows")
	cmd.Flags().BoolVar(&cfg.Offline, "offline", false, "Use fallback pricing only")
	cmd.Flags().BoolVar(&cfg.NoSync, "no-sync", false, "Skip on-demand sync before querying")
	cmd.Flags().StringVar(&cfg.Timezone, "timezone", "", "IANA timezone for date bucketing")
	return cmd
}

func newUsageStatuslineCommand() *cobra.Command {
	var cfg UsageStatuslineConfig
	cmd := &cobra.Command{
		Use:          "statusline",
		Short:        "One-line cost summary for today",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			runUsageStatusline(cfg)
		},
	}
	cmd.Flags().StringVar(&cfg.Agent, "agent", "", "Filter by agent name")
	cmd.Flags().BoolVar(&cfg.Offline, "offline", false, "Use fallback pricing only")
	cmd.Flags().BoolVar(&cfg.NoSync, "no-sync", false, "Skip on-demand sync before querying")
	return cmd
}

func newActivityCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "activity",
		Short:        "Activity and concurrency reporting",
		GroupID:      groupUsage,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newActivityReportCommand())
	return cmd
}

func newActivityReportCommand() *cobra.Command {
	var cfg ActivityReportConfig
	cmd := &cobra.Command{
		Use:          "report",
		Short:        "Activity report over a date range",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			cfg.JSON = outputFormat(cmd) == "json"
			runActivityReport(cfg)
		},
	}
	cmd.Flags().StringVar(&cfg.Preset, "preset", "", "Range preset: day, week, month, custom")
	cmd.Flags().StringVar(&cfg.Date, "date", "", "Anchor date for presets (YYYY-MM-DD)")
	cmd.Flags().StringVar(&cfg.From, "from", "", "Start instant for custom range (RFC3339)")
	cmd.Flags().StringVar(&cfg.To, "to", "", "End instant for custom range (RFC3339)")
	cmd.Flags().StringVar(&cfg.Timezone, "timezone", "", "IANA timezone for range bucketing")
	cmd.Flags().StringVar(&cfg.Bucket, "bucket", "", "Bucket size: 5m, 15m, 1h, 1d, 1w")
	cmd.Flags().StringVar(&cfg.Project, "project", "", "Filter by project")
	cmd.Flags().StringVar(&cfg.Agent, "agent", "", "Filter by agent name")
	cmd.Flags().StringVar(&cfg.Machine, "machine", "", "Filter by machine name")
	registerFormatFlags(cmd.Flags())
	cmd.Flags().BoolVar(&cfg.NoSync, "no-sync", false, "Skip on-demand sync before querying")
	cmd.Flags().BoolVar(&cfg.Offline, "offline", false, "Use fallback pricing only")
	return cmd
}

func newPGCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "pg",
		Short:        "PostgreSQL sync and serve commands",
		GroupID:      groupData,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newPGPushCommand())
	cmd.AddCommand(newPGStatusCommand())
	cmd.AddCommand(newPGServeCommand())
	cmd.AddCommand(newPGServiceCommand())
	return cmd
}

func newPGPushCommand() *cobra.Command {
	var cfg PGPushConfig
	cmd := &cobra.Command{
		Use:          "push [target]",
		Short:        "Push local data to PostgreSQL",
		SilenceUsage: true,
		Args:         cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			targetName := ""
			if len(args) == 1 {
				targetName = args[0]
			}
			if cfg.AllTargets && cfg.Watch {
				return fmt.Errorf(
					"pg push --watch: %w",
					fmt.Errorf(
						"--all cannot be combined with --watch",
					),
				)
			}
			if cfg.Watch {
				if err := runPGPushWatch(cfg, targetName); err != nil {
					return fmt.Errorf("pg push --watch: %w", err)
				}
				return nil
			}
			if cmd.Flags().Changed("debounce") || cmd.Flags().Changed("interval") {
				fmt.Fprintln(os.Stderr,
					"warning: --debounce and --interval have no effect without --watch")
			}
			if err := runPGPush(cfg, targetName); err != nil {
				return fmt.Errorf("pg push: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&cfg.AllTargets, "all", false, "Push every configured PG target sequentially")
	cmd.Flags().BoolVar(&cfg.Full, "full", false, "Force full local resync and PG push")
	cmd.Flags().StringVar(&cfg.ProjectsFlag, "projects", "", "Comma-separated list of projects to push (inclusive)")
	cmd.Flags().StringVar(&cfg.ExcludeProjects, "exclude-projects", "", "Comma-separated list of projects to exclude from push")
	cmd.Flags().BoolVar(&cfg.AllProjects, "all-projects", false, "Ignore configured project filters for this run")
	cmd.Flags().BoolVar(&cfg.Watch, "watch", false, "Run continuously, pushing on change plus a periodic floor")
	cmd.Flags().DurationVar(&cfg.Debounce, "debounce", defaultWatchDebounce, "Coalesce window after a change before pushing (--watch only)")
	cmd.Flags().DurationVar(&cfg.Interval, "interval", defaultWatchInterval, "Periodic floor push interval (--watch only)")
	return cmd
}

func newPGStatusCommand() *cobra.Command {
	var cfg PGStatusConfig
	cmd := &cobra.Command{
		Use:          "status [target]",
		Short:        "Show PG sync status",
		SilenceUsage: true,
		Args:         cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			targetName := ""
			if len(args) == 1 {
				targetName = args[0]
			}
			if err := runPGStatus(targetName, cfg); err != nil {
				return fmt.Errorf("pg status: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&cfg.AllTargets, "all", false, "Show status for every configured PG target")
	cmd.Flags().StringVar(&cfg.ProjectsFlag, "projects", "", "Comma-separated list of projects whose push status to show")
	cmd.Flags().StringVar(&cfg.ExcludeProjects, "exclude-projects", "", "Comma-separated list of excluded projects whose push status to show")
	cmd.Flags().BoolVar(&cfg.AllProjects, "all-projects", false, "Ignore configured project filters for this status")
	return cmd
}

func newPGServeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "serve",
		Short:        "Serve from PostgreSQL (read-only)",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			appCfg, basePath, err := loadPGServeConfig(cmd)
			if err != nil {
				fatal("%v", err)
			}
			runPGServe(appCfg, basePath)
		},
	}
	cmd.Flags().String(
		"base-path",
		"",
		"URL prefix for reverse-proxy subpath (e.g. /agentsview)",
	)
	config.RegisterServePFlags(cmd.Flags())
	return cmd
}

func newDuckDBCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "duckdb",
		Short:        "DuckDB sync and serve commands",
		GroupID:      groupData,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newDuckDBPushCommand())
	cmd.AddCommand(newDuckDBStatusCommand())
	cmd.AddCommand(newDuckDBServeCommand())
	cmd.AddCommand(newDuckDBQuackCommand())
	return cmd
}

func newDuckDBPushCommand() *cobra.Command {
	var cfg DuckDBPushConfig
	cmd := &cobra.Command{
		Use:          "push",
		Short:        "Push local data to DuckDB",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			runDuckDBPush(cfg)
		},
	}
	cmd.Flags().BoolVar(&cfg.Full, "full", false, "Force full local resync and DuckDB push")
	cmd.Flags().StringVar(&cfg.ProjectsFlag, "projects", "", "Comma-separated list of projects to push (inclusive)")
	cmd.Flags().StringVar(&cfg.ExcludeProjects, "exclude-projects", "", "Comma-separated list of projects to exclude from push")
	cmd.Flags().BoolVar(&cfg.AllProjects, "all-projects", false, "Ignore configured project filters for this run")
	cmd.Flags().BoolVar(&cfg.Watch, "watch", false, "Continue watching local files and pushing changes")
	cmd.Flags().DurationVar(&cfg.Debounce, "debounce", defaultWatchDebounce, "Coalesce window after a change before pushing (--watch only)")
	cmd.Flags().DurationVar(&cfg.Interval, "interval", defaultWatchInterval, "Periodic floor push interval (--watch only)")
	return cmd
}

func newDuckDBStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "status",
		Short:        "Show DuckDB sync status",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			runDuckDBStatus()
		},
	}
}

func newDuckDBServeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "serve",
		Short:        "Serve from DuckDB (read-only)",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			appCfg, basePath, err := loadDuckDBServeConfig(cmd)
			if err != nil {
				fatal("%v", err)
			}
			runDuckDBServe(appCfg, basePath)
			return nil
		},
	}
	cmd.Flags().String(
		"base-path",
		"",
		"URL prefix for reverse-proxy subpath (e.g. /agentsview)",
	)
	config.RegisterServePFlags(cmd.Flags())
	return cmd
}

func newDuckDBQuackCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "quack",
		Short:        "Quack remote protocol commands",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	var serveCfg DuckDBQuackServeConfig
	serveCmd := &cobra.Command{
		Use:          "serve",
		Short:        "Expose local DuckDB over Quack",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			runDuckDBQuackServe(serveCfg)
		},
	}
	serveCmd.Flags().StringVar(
		&serveCfg.Bind, "bind", "quack:127.0.0.1:9494",
		"Quack bind URI",
	)
	serveCmd.Flags().StringVar(
		&serveCfg.Path, "path", "",
		"DuckDB mirror path (defaults to [duckdb].path)",
	)
	serveCmd.Flags().StringVar(
		&serveCfg.Token, "token", "",
		"Quack authentication token (required unless configured)",
	)
	serveCmd.Flags().BoolVar(
		&serveCfg.AllowInsecure, "allow-insecure", false,
		"Allow non-loopback Quack binding",
	)
	cmd.AddCommand(serveCmd)
	return cmd
}

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "version",
		Short:        "Show version information",
		GroupID:      groupMeta,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			printVersion(cmd.OutOrStdout())
		},
	}
}

func printVersion(w io.Writer) {
	fmt.Fprintf(
		w,
		"agentsview %s (commit %s, built %s)\n",
		version,
		commit,
		buildDate,
	)
}

func writeRootHelp(w io.Writer, root *cobra.Command) {
	fmt.Fprintf(w, "agentsview %s - local web viewer for AI agent sessions\n\n", version)
	fmt.Fprintln(w, "Syncs session data from supported AI coding agents into SQLite,")
	fmt.Fprintln(w, "serves analytics, and exposes a session browser via local web UI.")
	fmt.Fprintln(w)
	renderRootUsage(w, root)
	fmt.Fprintln(w)
	renderRootCommands(w, root)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Flags:")
	fmt.Fprint(w, root.Flags().FlagUsagesWrapped(flagHelpWidth(w)))
	fmt.Fprintln(w, "Environment variables:")
	fmt.Fprintln(w, "  CLAUDE_PROJECTS_DIR     Claude Code projects directory")
	fmt.Fprintln(w, "  CODEX_SESSIONS_DIR      Codex sessions directory")
	fmt.Fprintln(w, "  COPILOT_DIR             Copilot sessions or exported JetBrains Copilot directory")
	fmt.Fprintln(w, "  GEMINI_DIR              Gemini CLI directory")
	fmt.Fprintln(w, "  OPENCODE_DIR            OpenCode data directory")
	fmt.Fprintln(w, "  CURSOR_PROJECTS_DIR     Cursor projects directory")
	fmt.Fprintln(w, "  IFLOW_DIR               iFlow projects directory")
	fmt.Fprintln(w, "  AMP_DIR                 Amp threads directory")
	fmt.Fprintln(w, "  ZED_DIR                 Zed data directory")
	fmt.Fprintln(w, "  QWEN_PROJECTS_DIR       Qwen Code projects directory")
	fmt.Fprintln(w, "  QWENPAW_DIR             QwenPaw workspaces directory")
	fmt.Fprintln(w, "  OMP_DIR                 OhMyPi sessions directory")
	fmt.Fprintln(w, "  DEEPSEEK_TUI_SESSIONS_DIR")
	fmt.Fprintln(w, "                          DeepSeek TUI sessions directory")
	fmt.Fprintln(w, "  QCLAW_DIR               QClaw agents directory")
	fmt.Fprintln(w, "  WORKBUDDY_PROJECTS_DIR  WorkBuddy projects directory")
	fmt.Fprintln(w, "  PIEBALD_DIR             Piebald data directory")
	fmt.Fprintln(w, "  AGENTSVIEW_DATA_DIR     Data directory (database, config)")
	fmt.Fprintln(w, "  AGENTSVIEW_PG_URL       PostgreSQL connection URL for sync")
	fmt.Fprintln(w, "  AGENTSVIEW_PG_MACHINE   Machine name for PG sync")
	fmt.Fprintln(w, "  AGENTSVIEW_PG_SCHEMA    PG schema name (default \"agentsview\")")
	fmt.Fprintln(w, "  AGENTSVIEW_DUCKDB_PATH  DuckDB mirror database path")
	fmt.Fprintln(w, "  AGENTSVIEW_DUCKDB_URL   Quack connection URL for DuckDB serve")
	fmt.Fprintln(w, "  AGENTSVIEW_DUCKDB_TOKEN Quack authentication token")
	fmt.Fprintln(w, "  AGENTSVIEW_DUCKDB_MACHINE")
	fmt.Fprintln(w, "                          Machine name for DuckDB sync")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Watcher excludes:")
	fmt.Fprintln(w, "  Add \"watch_exclude_patterns\" to ~/.agentsview/config.toml")
	fmt.Fprintln(w, "  to skip directory names/patterns while recursively watching roots.")
	fmt.Fprintln(w, "  Example:")
	fmt.Fprintln(w, "  watch_exclude_patterns = [\".git\", \"node_modules\", \".next\", \"dist\"]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Multiple directories:")
	fmt.Fprintln(w, "  Add arrays to ~/.agentsview/config.toml to scan multiple locations:")
	fmt.Fprintln(w, "  claude_project_dirs = [\"/path/one\", \"/path/two\"]")
	fmt.Fprintln(w, "  codex_sessions_dirs = [\"/codex/a\", \"/codex/b\"]")
	fmt.Fprintln(w, "  When set, these override default directory. Environment variables")
	fmt.Fprintln(w, "  override config file arrays.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Remote hosts:")
	fmt.Fprintln(w, "  Add a [[remote_hosts]] array to ~/.agentsview/config.toml so that")
	fmt.Fprintln(w, "  \"agentsview sync\" (no --host) also syncs each configured host:")
	fmt.Fprintln(w, "  [[remote_hosts]]")
	fmt.Fprintln(w, "  host = \"devbox1\"")
	fmt.Fprintln(w, "  transport = \"ssh\" # optional; default")
	fmt.Fprintln(w, "  user = \"jesse\"  # optional")
	fmt.Fprintln(w, "  port = 22        # optional")
	fmt.Fprintln(w, "  Requires key-based (passwordless) SSH to each host.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  For daemon-backed HTTP sync over a private network such as Tailscale:")
	fmt.Fprintln(w, "  [[remote_hosts]]")
	fmt.Fprintln(w, "  host = \"devbox1\"")
	fmt.Fprintln(w, "  transport = \"http\"")
	fmt.Fprintln(w, "  url = \"http://devbox1.tailnet.ts.net:8080\"")
	fmt.Fprintln(w, "  token = \"remote-token\" # required; remote daemon auth_token")
	fmt.Fprintln(w, "  Each host must be unique.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  Top-level daemon_idle_timeout = \"0s\" keeps serve --background nodes alive.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Data stored in ~/.agentsview/ by default.")
}

func normalizeFlagHelpWidth(width int) int {
	if width <= 0 {
		return 80
	}
	if width > 160 {
		return 160
	}
	return width
}

func flagHelpWidth(w io.Writer) int {
	file, ok := w.(*os.File)
	if !ok {
		return 80
	}
	width, _, err := term.GetSize(int(file.Fd()))
	if err != nil {
		return 80
	}
	return normalizeFlagHelpWidth(width)
}

func renderRootUsage(w io.Writer, root *cobra.Command) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintf(w, "  %s [flags]\n", root.CommandPath())
	fmt.Fprintf(w, "  %s <command> [flags]\n", root.CommandPath())
}

func renderRootCommands(w io.Writer, root *cobra.Command) {
	for _, group := range root.Groups() {
		cmds := groupedRootCommands(root, group.ID)
		if len(cmds) == 0 {
			continue
		}
		fmt.Fprintf(w, "%s\n", group.Title)
		for _, cmd := range cmds {
			fmt.Fprintf(w, "  %-22s %s\n", commandPath(root, cmd), cmd.Short)
		}
		fmt.Fprintln(w)
	}
}

func groupedRootCommands(root *cobra.Command, groupID string) []*cobra.Command {
	var grouped []*cobra.Command
	for _, cmd := range root.Commands() {
		if !cmd.IsAvailableCommand() || cmd.Hidden || cmd.GroupID != groupID {
			continue
		}
		grouped = append(grouped, cmd)
		if !shouldListRootChildren(cmd) {
			continue
		}
		for _, child := range cmd.Commands() {
			if !child.IsAvailableCommand() || child.Hidden {
				continue
			}
			grouped = append(grouped, child)
		}
	}
	slices.SortStableFunc(grouped, func(a, b *cobra.Command) int {
		return strings.Compare(commandPath(root, a), commandPath(root, b))
	})
	return grouped
}

func shouldListRootChildren(cmd *cobra.Command) bool {
	return cmd.Name() != "completion"
}

func commandPath(root, cmd *cobra.Command) string {
	return strings.TrimPrefix(cmd.CommandPath(), root.CommandPath()+" ")
}
