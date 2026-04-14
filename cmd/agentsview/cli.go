package main

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/wesm/agentsview/internal/config"
)

const (
	groupCore = "core"
	groupData = "data"
	groupMeta = "meta"
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
			runServe(changedFlagArgs(cmd.Flags()))
		},
	}
	root.AddGroup(
		&cobra.Group{ID: groupCore, Title: "Core Commands:"},
		&cobra.Group{ID: groupData, Title: "Data Commands:"},
		&cobra.Group{ID: groupMeta, Title: "Other Commands:"},
	)
	root.SetCompletionCommandGroupID(groupMeta)
	root.SetHelpCommandGroupID(groupMeta)

	config.RegisterServePFlags(root.Flags())
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
	root.AddCommand(newPGCommand())
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
			runServe(changedFlagArgs(cmd.Flags()))
		},
	}
	config.RegisterServePFlags(cmd.Flags())
	return cmd
}

func newSyncCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "sync",
		Short:        "Sync session data without serving",
		GroupID:      groupCore,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			runSync(changedFlagArgs(cmd.Flags()))
		},
	}
	cmd.Flags().Bool(
		"full",
		false,
		"Force a full resync regardless of data version",
	)
	return cmd
}

func newPruneCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "prune",
		Short:        "Delete sessions matching filters",
		GroupID:      groupCore,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			runPrune(changedFlagArgs(cmd.Flags()))
		},
	}
	cmd.Flags().String(
		"project",
		"",
		"Sessions whose project contains this substring",
	)
	cmd.Flags().Int(
		"max-messages",
		-1,
		"Sessions with at most N user messages",
	)
	cmd.Flags().String(
		"before",
		"",
		"Sessions that ended before this date (YYYY-MM-DD)",
	)
	cmd.Flags().String(
		"first-message",
		"",
		"Sessions whose first message starts with this text",
	)
	cmd.Flags().Bool(
		"dry-run",
		false,
		"Show what would be pruned without deleting",
	)
	cmd.Flags().Bool(
		"yes",
		false,
		"Skip confirmation prompt",
	)
	return cmd
}

func newUpdateCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "update",
		Short:        "Check for and install updates",
		GroupID:      groupMeta,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			runUpdate(changedFlagArgs(cmd.Flags()))
		},
	}
	cmd.Flags().Bool(
		"check",
		false,
		"Check for updates without installing",
	)
	cmd.Flags().Bool(
		"yes",
		false,
		"Install without confirmation prompt",
	)
	cmd.Flags().Bool(
		"force",
		false,
		"Force check (ignore cache)",
	)
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
	cmd := &cobra.Command{
		Use:          "import --type <type> <path>",
		Short:        "Import conversations",
		GroupID:      groupData,
		SilenceUsage: true,
		Args:         cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			runImport(append(changedFlagArgs(cmd.Flags()), args...))
		},
	}
	cmd.Flags().String(
		"type",
		"",
		"Import type: claude-ai, chatgpt",
	)
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
			runProjects(changedFlagArgs(cmd.Flags()))
		},
	}
	cmd.Flags().Bool("json", false, "Output as JSON array")
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
	cmd := &cobra.Command{
		Use:          "push",
		Short:        "Push local data to PostgreSQL",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			runPGPush(changedFlagArgs(cmd.Flags()))
		},
	}
	cmd.Flags().Bool("full", false, "Force full local resync and PG push")
	cmd.Flags().String(
		"projects",
		"",
		"Comma-separated list of projects to push (inclusive)",
	)
	cmd.Flags().String(
		"exclude-projects",
		"",
		"Comma-separated list of projects to exclude from push",
	)
	cmd.Flags().Bool(
		"all-projects",
		false,
		"Ignore configured project filters for this run",
	)
	return cmd
}

func newPGStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "status",
		Short:        "Show PG sync status",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			runPGStatus(nil)
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
			runPGServe(changedFlagArgs(cmd.Flags()))
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

func changedFlagArgs(fs *pflag.FlagSet) []string {
	if fs == nil {
		return nil
	}
	args := make([]string, 0, fs.NFlag()*2)
	fs.VisitAll(func(f *pflag.Flag) {
		if !f.Changed {
			return
		}
		name := "--" + f.Name
		if f.Value.Type() == "bool" {
			if f.Value.String() == "true" {
				args = append(args, name)
			} else {
				args = append(args, name+"="+f.Value.String())
			}
			return
		}
		args = append(args, name, f.Value.String())
	})
	return args
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
	fmt.Fprintln(w, "Cursor, and Amp session data into SQLite, serves analytics,")
	fmt.Fprintln(w, "and exposes session browser via local web UI.")
	fmt.Fprintln(w)
	renderRootUsage(w, root)
	fmt.Fprintln(w)
	renderRootCommands(w, root)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Flags:")
	fmt.Fprint(w, root.Flags().FlagUsagesWrapped(80))
	fmt.Fprintln(w, "Environment variables:")
	fmt.Fprintln(w, "  CLAUDE_PROJECTS_DIR     Claude Code projects directory")
	fmt.Fprintln(w, "  CODEX_SESSIONS_DIR      Codex sessions directory")
	fmt.Fprintln(w, "  COPILOT_DIR             Copilot CLI directory")
	fmt.Fprintln(w, "  GEMINI_DIR              Gemini CLI directory")
	fmt.Fprintln(w, "  OPENCODE_DIR            OpenCode data directory")
	fmt.Fprintln(w, "  CURSOR_PROJECTS_DIR     Cursor projects directory")
	fmt.Fprintln(w, "  IFLOW_DIR               iFlow projects directory")
	fmt.Fprintln(w, "  AMP_DIR                 Amp threads directory")
	fmt.Fprintln(w, "  AGENT_VIEWER_DATA_DIR   Data directory (database, config)")
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

func commandUsage(root, cmd *cobra.Command) string {
	path := commandPath(root, cmd)
	flags := ""
	if cmd.Flags().HasAvailableFlags() {
		flags = " [flags]"
	}
	if cmd.Args == nil {
		return root.Name() + " " + path + flags
	}
	if strings.Contains(cmd.Use, "<") || strings.Contains(cmd.Use, "[") {
		return root.Name() + " " + path + flags
	}
	return root.Name() + " " + path + flags
}

func commandPath(root, cmd *cobra.Command) string {
	return strings.TrimPrefix(cmd.CommandPath(), root.CommandPath()+" ")
}

func execute() error {
	cmd := newRootCommand()
	cmd.SetOut(os.Stdout)
	cmd.SetErr(os.Stderr)
	return cmd.Execute()
}
