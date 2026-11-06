package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/gittrailer"
)

func newGitTrailerCommand() *cobra.Command {
	return &cobra.Command{
		Use: "git-trailer <file>", Hidden: true, DisableFlagParsing: true,
		Run: func(cmd *cobra.Command, args []string) {
			var err error
			if len(args) != 1 {
				err = errors.New("usage: agentsview git-trailer <file>")
			} else {
				var session gittrailer.Session
				session, err = gittrailer.Discover(os.Getenv)
				if err == nil {
					err = gittrailer.Stamp(cmd.Context(), args[0], session)
				}
			}
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", err)
			}
		},
	}
}

func newGitTrailerPrepareCommand() *cobra.Command {
	return &cobra.Command{
		Use: "git-trailer-prepare <file> [source] [commit]", Hidden: true, DisableFlagParsing: true,
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) < 1 || len(args) > 3 {
				return errors.New("usage: agentsview git-trailer-prepare <file> [source] [commit]")
			}
			source := ""
			if len(args) >= 2 {
				source = args[1]
			}
			return gittrailer.PrepareTemplate(args[0], source)
		},
	}
}

func newGitHookCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use: "git-hook", Short: "Manage the optional agent session trailer hook", GroupID: groupMeta,
		Long: "Add an Agent-Session trailer that names the current agent session to commits " +
			"made in a repository. Nothing changes until you install the hook in that repository.",
		Example: "  agentsview git-hook install",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(
		&cobra.Command{
			Use: "install", Short: "Install in the current repository", Args: cobra.NoArgs,
			Long: "Write prepare-commit-msg and commit-msg hooks into the hooks directory Git uses " +
				"for the current repository. A core.hooksPath setting makes them apply to every " +
				"repository sharing that path. An existing different hook is left alone, and the " +
				"command prints the line to add to it. A missing agentsview binary or session " +
				"leaves commit messages unchanged.",
			Example: "  agentsview git-hook install",
			RunE:    func(_ *cobra.Command, _ []string) error { return gittrailer.Install(".") },
		},
	)
	return cmd
}
