// ABOUTME: `session watch <id>` subcommand — streams NDJSON events
// ABOUTME: describing session updates until the context is cancelled.
package main

import (
	"encoding/json"

	"github.com/spf13/cobra"
)

func newSessionWatchCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "watch <id>",
		Short:        "Stream NDJSON events as the session updates",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := rejectFormatFlags(
				cmd, "session watch", "NDJSON",
			); err != nil {
				return err
			}
			svc, cleanup, err := resolvePersistentService(cmd)
			if err != nil {
				return err
			}
			defer cleanup()

			id, err := resolveServiceSessionID(cmd.Context(), svc, args[0])
			if err != nil {
				return err
			}
			ch, err := svc.Watch(cmd.Context(), id)
			if err != nil {
				return err
			}
			enc := json.NewEncoder(cmd.OutOrStdout())
			for ev := range ch {
				if err := enc.Encode(ev); err != nil {
					return err
				}
			}
			return nil
		},
	}
}
