package main

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sessionmaterialize"
	"go.kenn.io/agentsview/internal/shellquote"
)

func newSessionMaterializeCommand() *cobra.Command {
	var cwd string
	cmd := &cobra.Command{
		Use:   "materialize <id>",
		Short: "Install original session files for native resume",
		Long: "Install a Claude session with its companions or a Pi transcript from configured local or mirror sources; Claude also supports S3 roots and snapshots. Print a resume command for a local checkout. " +
			"Requires a local archive with conversation content; the original files must total at most 512 MiB. " +
			"End the running session and wait for any S3 uploader to finish before materializing. " +
			"Local retained and directory captures and Codex are not yet supported.",
		Example: "  agentsview session materialize <id> --cwd <path>",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			if cmd.Flags().Changed("server") || cmd.Flags().Changed("server-token-file") || cmd.Flags().Changed("pg") {
				return errors.New("session materialize requires the local archive; --server, --server-token-file and --pg are unsupported")
			}
			if cwd == "" {
				return errors.New("--cwd is required")
			}
			cfg, err := config.LoadReadOnlyFor(config.ScopeLocal)
			if err != nil {
				return err
			}
			if cfg.ArchiveContent.UsageOnly() {
				return errors.New("session materialize is unavailable with archive_content=usage")
			}
			database, err := openReadOnlyDB(ctx, cfg)
			if err != nil {
				return err
			}
			defer database.Close()
			id, err := resolveSessionID(ctx, database, args[0])
			if err != nil {
				return err
			}
			if id == "" {
				return fmt.Errorf("session not in local archive: %s", args[0])
			}
			session, err := database.GetSessionFull(ctx, id)
			if err != nil {
				return err
			}
			result, err := sessionmaterialize.Materialize(ctx, cfg, database, session, cwd)
			if err != nil {
				return err
			}
			_, nativeID := parser.StripHostPrefix(id)
			if session.Agent == "pi" {
				result.Command = piResumeCommand(result.Cwd, result.Paths[0], runtime.GOOS == "windows")
			} else {
				result.Command = claudeResumeCommand(result.Cwd, nativeID, result.ConfigDir, result.ProjectOverride, runtime.GOOS == "windows")
			}
			if outputFormat(cmd) == "json" {
				if err := json.MarshalWrite(cmd.OutOrStdout(), result); err != nil {
					return err
				}
				_, err := fmt.Fprintln(cmd.OutOrStdout())
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), result.Command)
			return err
		},
	}
	cmd.Flags().StringVar(&cwd, "cwd", "", "Existing local checkout for the resumed session")
	return cmd
}

func claudeResumeCommand(cwd, id, home, project string, windows bool) string {
	if home == "" {
		if windows {
			return "Set-Location -LiteralPath " + shellquote.PowerShell(cwd) + "; if ($?) { claude --resume " + shellquote.PowerShell(id) + " }"
		}
		return "cd " + shellquote.POSIX(cwd) + " && claude --resume " + shellquote.POSIX(id)
	}
	if windows {
		return "Set-Location -LiteralPath " + shellquote.PowerShell(cwd) + "; if ($?) { & { $oldHome = $env:CLAUDE_CONFIG_DIR; $oldProject = $env:CLAUDE_CODE_PROJECT_DIR_NAME; try { $env:CLAUDE_CONFIG_DIR = " + shellquote.PowerShell(home) + "; $env:CLAUDE_CODE_PROJECT_DIR_NAME = " + shellquote.PowerShell(project) + "; claude --resume " + shellquote.PowerShell(id) + " } finally { $env:CLAUDE_CONFIG_DIR = $oldHome; $env:CLAUDE_CODE_PROJECT_DIR_NAME = $oldProject } } }"
	}
	return "cd " + shellquote.POSIX(cwd) + " && CLAUDE_CONFIG_DIR=" + shellquote.POSIX(home) + " CLAUDE_CODE_PROJECT_DIR_NAME=" + shellquote.POSIX(project) + " claude --resume " + shellquote.POSIX(id)
}

func piResumeCommand(cwd, path string, windows bool) string {
	if windows {
		return "Set-Location -LiteralPath " + shellquote.PowerShell(cwd) + "; if ($?) { pi --session " + shellquote.PowerShell(path) + " }"
	}
	return "cd " + shellquote.POSIX(cwd) + " && pi --session " + shellquote.POSIX(path)
}
