package main

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/why"
)

type whyServiceResolver func(*cobra.Command) (service.SessionService, func(), error)

func newWhyCommand() *cobra.Command {
	return newWhyCommandWithServiceResolver(resolveCommitLookupService)
}
func newWhyCommandWithServiceResolver(resolve whyServiceResolver) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "why <path>[:<start>[-<end>]]",
		Short:   "Find recorded agent sessions behind local lines or a commit",
		Long:    "Show stored commit-session evidence and associated prompts using local Git blame.\nAlways reads stored records without requesting sync or starting a daemon.\nA recorded commit call is context, not proof of authorship of every change.",
		GroupID: groupData, Args: cobra.MaximumNArgs(1), SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			defer func() {
				if err != nil {
					err = whyCommandError{cause: err}
				}
			}()
			remote, _ := cmd.Flags().GetString("server")
			if remote != "" && pgReadRequested(cmd) {
				return errors.New("--server and --pg are mutually exclusive")
			}
			path := ""
			if len(args) > 0 {
				path = args[0]
			}
			commit, _ := cmd.Flags().GetString("commit")
			rev, _ := cmd.Flags().GetString("rev")
			literal, _ := cmd.Flags().GetBool("literal-path")
			req, err := why.ParseRequest(path, commit, rev, literal)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 60*time.Second)
			defer cancel()
			cmd.SetContext(ctx)
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			in, err := why.Inspect(ctx, cwd, req)
			if err != nil {
				return err
			}
			result, err := why.Explain(ctx, in, func(ctx context.Context, shas []string) (map[string][]service.CommitSessionLink, error) {
				svc, cleanup, err := resolve(cmd)
				if cleanup != nil {
					defer cleanup()
				}
				if err != nil {
					return nil, err
				}
				return svc.CommitSessions(ctx, shas)
			})
			if err != nil {
				return err
			}
			if outputFormat(cmd) == "json" {
				return why.WriteJSON(cmd.OutOrStdout(), result)
			}
			return why.WriteHuman(cmd.OutOrStdout(), result)
		},
	}
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return whyCommandError{cause: err} })
	registerFormatFlags(cmd.Flags())
	cmd.Flags().String("commit", "", "Inspect one local commit SHA instead of a path")
	cmd.Flags().String("rev", "", "Blame a historical revision (default: working tree)")
	cmd.Flags().Bool("literal-path", false, "Treat the whole path literally, including numeric colon suffixes")
	cmd.Flags().Bool("no-sync", false, "Compatibility flag: why always reads stored records without sync")
	cmd.Flags().String("server", "", "Remote daemon or hosted archive URL")
	cmd.Flags().String("server-token-file", "", "File containing bearer token for explicit --server requests")
	cmd.Flags().Bool("pg", false, "Read from configured PostgreSQL")
	return cmd
}

type whyCommandError struct{ cause error }

func (e whyCommandError) Error() string { return why.TerminalText(e.cause.Error()) }
func (e whyCommandError) Unwrap() error { return e.cause }
