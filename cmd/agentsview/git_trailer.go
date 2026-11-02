package main

import (
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
				err = fmt.Errorf("usage: agentsview git-trailer <file>")
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

func newGitHookCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "git-hook", Short: "Manage the optional agent session trailer hook", GroupID: groupMeta}
	cmd.AddCommand(
		&cobra.Command{
			Use: "install", Short: "Install in the current repository", Args: cobra.NoArgs,
			RunE: func(_ *cobra.Command, _ []string) error { return gittrailer.Install(".") },
		},
	)
	return cmd
}
