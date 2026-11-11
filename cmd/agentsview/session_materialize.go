package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sessionmaterialize"
	"go.kenn.io/agentsview/internal/shellquote"
)

func newSessionMaterializeCommand() *cobra.Command {
	var cwd string
	cmd := &cobra.Command{
		Use:   "materialize <id>",
		Short: "Install original session files for native resume",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("server") || cmd.Flags().Changed("pg") {
				return errors.New("session materialize requires the local archive; --server and --pg are unsupported")
			}
			if cwd == "" {
				return errors.New("--cwd is required")
			}
			cfg, err := config.LoadReadOnlyFor(config.ScopeLocal)
			if err != nil {
				return err
			}
			database, err := db.OpenReadOnly(cmd.Context(), cfg.DBPath)
			if err != nil {
				return err
			}
			defer database.Close()
			id, err := resolveSessionID(cmd.Context(), database, args[0])
			if err != nil {
				return err
			}
			if id == "" {
				return fmt.Errorf("session not in local archive: %s", args[0])
			}
			session, err := database.GetSessionFull(cmd.Context(), id)
			if err != nil {
				return err
			}
			result, err := sessionmaterialize.Materialize(cmd.Context(), cfg, session, cwd)
			if err != nil {
				return err
			}
			_, nativeID := parser.StripHostPrefix(id)
			if runtime.GOOS == "windows" {
				result.Command = "Set-Location -LiteralPath " + shellquote.PowerShell(result.Cwd) + "; if ($?) { claude --resume " + shellquote.PowerShell(nativeID) + " }"
			} else {
				result.Command = "cd " + shellquote.POSIX(result.Cwd) + " && claude --resume " + shellquote.POSIX(nativeID)
			}
			if outputFormat(cmd) == "json" {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), result.Command)
			return err
		},
	}
	cmd.Flags().StringVar(&cwd, "cwd", "", "Existing local checkout for the resumed session")
	return cmd
}
