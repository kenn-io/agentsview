package main

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"github.com/wesm/agentsview/internal/config"
	"github.com/wesm/agentsview/internal/db"
	"golang.org/x/term"
)

const (
	groupCore  = "core"
	groupData  = "data"
	groupUsage = "usage"
	groupMeta  = "meta"
)

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
	root.AddCommand(newProjectsCommand())
	root.AddCommand(newHealthCommand())
	root.AddCommand(newUsageCommand())
	root.AddCommand(newPGCommand())
	root.AddCommand(newSessionCommand())
	root.AddCommand(newStatsCommand())
	root.AddCommand(newClassifierCommand())
	root.AddCommand(newVersionCommand())

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
	cmd := &cobra.Command{
		Use:          "serve",
		Short:        "Start server",
		GroupID:      groupCore,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			runServe(mustLoadConfig(cmd))
		},
	}
	config.RegisterServePFlags(cmd.Flags())
	return cmd
}

func newSyncCommand() *cobra.Command {
	var cfg SyncConfig
	cmd := &cobra.Command{
		Use:          "sync",
		Short:        "Sync session data without serving",
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
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:          "projects",
		Short:        "List projects with session counts",
		GroupID:      groupCore,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			runProjects(jsonOutput)
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON array")
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
			runHealth(args, cfg)
		},
	}
	cmd.Flags().BoolVar(&cfg.JSON, "json", false,
		"Output as JSON")
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
			runUsageDaily(cfg)
		},
	}
	cmd.Flags().BoolVar(&cfg.JSON, "json", false, "Output as JSON")
	cmd.Flags().StringVar(&cfg.Since, "since", "", "Start date (YYYY-MM-DD)")
	cmd.Flags().StringVar(&cfg.Until, "until", "", "End date (YYYY-MM-DD)")
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
	return cmd
}

func newPGPushCommand() *cobra.Command {
	var cfg PGPushConfig
	cmd := &cobra.Command{
		Use:          "push",
		Short:        "Push local data to PostgreSQL",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			runPGPush(cfg)
		},
	}
	cmd.Flags().BoolVar(&cfg.Full, "full", false, "Force full local resync and PG push")
	cmd.Flags().StringVar(&cfg.ProjectsFlag, "projects", "", "Comma-separated list of projects to push (inclusive)")
	cmd.Flags().StringVar(&cfg.ExcludeProjects, "exclude-projects", "", "Comma-separated list of projects to exclude from push")
	cmd.Flags().BoolVar(&cfg.AllProjects, "all-projects", false, "Ignore configured project filters for this run")
	return cmd
}

func newPGStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "status",
		Short:        "Show PG sync status",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			runPGStatus()
		},
	}
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
	fmt.Fprintln(w, "Syncs Claude Code, Codex, Copilot CLI, Gemini CLI, OpenCode,")
	fmt.Fprintln(w, "Cursor, Amp, and Piebald session data into SQLite, serves analytics,")
	fmt.Fprintln(w, "and exposes session browser via local web UI.")
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
	fmt.Fprintln(w, "  COPILOT_DIR             Copilot CLI directory")
	fmt.Fprintln(w, "  GEMINI_DIR              Gemini CLI directory")
	fmt.Fprintln(w, "  OPENCODE_DIR            OpenCode data directory")
	fmt.Fprintln(w, "  CURSOR_PROJECTS_DIR     Cursor projects directory")
	fmt.Fprintln(w, "  IFLOW_DIR               iFlow projects directory")
	fmt.Fprintln(w, "  AMP_DIR                 Amp threads directory")
	fmt.Fprintln(w, "  PIEBALD_DIR             Piebald data directory")
	fmt.Fprintln(w, "  AGENTSVIEW_DATA_DIR     Data directory (database, config)")
	fmt.Fprintln(w, "  AGENTSVIEW_PG_URL       PostgreSQL connection URL for sync")
	fmt.Fprintln(w, "  AGENTSVIEW_PG_MACHINE   Machine name for PG sync")
	fmt.Fprintln(w, "  AGENTSVIEW_PG_SCHEMA    PG schema name (default \"agentsview\")")
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
