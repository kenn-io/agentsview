package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

func newDBAdoptMachineCommand() *cobra.Command {
	var noLocalSessions, list bool
	cmd := &cobra.Command{
		Use:          "adopt-machine [old-machine ...]",
		Short:        "Assign historical local machine keys to this installation",
		Long:         "Assign historical local machine keys to this installation. Select only keys whose sessions belong to this installation. Stop the daemon first. Sessions and worktree rules move together; old keys remain filter redirects.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, machines []string) error {
			if list {
				if len(machines) != 0 || noLocalSessions {
					return fmt.Errorf("use --list alone to inspect machine keys")
				}
				cfg, err := config.LoadReadOnly()
				if err != nil {
					return err
				}
				database, err := openReadOnlyDB(cfg)
				if err != nil {
					return err
				}
				defer database.Close()
				candidates, err := database.ListMachineIdentityCandidates(cmd.Context())
				if err != nil {
					return err
				}
				out := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
				fmt.Fprintln(out, "Machine key\tSessions\tWorktree rules")
				for _, candidate := range candidates {
					fmt.Fprintf(out, "%q\t%d\t%d\n", candidate.Machine, candidate.Sessions, candidate.WorktreeRules)
				}
				return out.Flush()
			}
			if (len(machines) == 0) != noLocalSessions {
				return fmt.Errorf("select one or more old machine keys, or use --no-local-sessions alone")
			}
			cfg, err := config.LoadMinimal()
			if err != nil {
				return err
			}
			database, lock, err := openWriteDBWith(cmd.Context(), cfg, func(cfg config.Config) (*db.DB, error) {
				applyClassifierConfig(cfg)
				database, err := db.Open(cfg.DBPath)
				if err != nil {
					return nil, err
				}
				if err := database.AdoptMachineIdentity(cmd.Context(), cfg.InstallationID, machines); err != nil {
					database.Close()
					return nil, err
				}
				return database, nil
			})
			if err != nil {
				return err
			}
			defer closeWriteDB(database, lock)
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Archive ownership recorded for installation %s.\n", cfg.InstallationID)
			return err
		},
	}
	cmd.Flags().BoolVar(&noLocalSessions, "no-local-sessions", false,
		"Confirm that existing named machines all belong to other installations")
	cmd.Flags().BoolVar(&list, "list", false, "List archived machine keys without starting a daemon or changing the archive")
	return cmd
}
