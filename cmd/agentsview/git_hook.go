package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/gittrailer"
)

func newGitHookCommand() *cobra.Command {
	parent := &cobra.Command{Use: "git-hook", Short: "Manage optional Git commit session trailer hooks", GroupID: groupMeta, Args: cobra.NoArgs}
	for _, action := range []string{"install", "uninstall", "status"} {
		var repo string
		var global bool
		cmd := &cobra.Command{
			Use: action, Short: action + " agent session trailer hooks", Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				dir, err := gittrailer.HooksDir(cmd.Context(), repo, global)
				if err != nil {
					return err
				}
				switch action {
				case "install":
					err = gittrailer.Install(dir)
				case "uninstall":
					err = gittrailer.Uninstall(dir)
				}
				if err != nil {
					return err
				}
				statuses, err := gittrailer.Status(dir)
				if err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), "Hooks directory: "+dir)
				for _, status := range statuses {
					state := "not installed"
					if status.Installed {
						state = "installed"
					}
					fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", status.Name, state)
				}
				return nil
			},
		}
		cmd.Flags().StringVar(&repo, "repo", "", "Repository path (defaults to the current directory)")
		cmd.Flags().BoolVar(&global, "global", false, "Use the existing absolute global core.hooksPath")
		cmd.MarkFlagsMutuallyExclusive("repo", "global")
		parent.AddCommand(cmd)
	}
	return parent
}
