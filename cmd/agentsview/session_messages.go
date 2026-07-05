// ABOUTME: `session messages <id>` subcommand — prints a window of
// ABOUTME: messages in JSON or human format.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/service"
)

func newSessionMessagesCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "messages <id>",
		Short:        "Show a window of messages from a session",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			filter, err := messagesFilterFromFlags(cmd)
			if err != nil {
				return err
			}

			svc, cleanup, err := resolveService(cmd)
			if err != nil {
				return err
			}
			defer cleanup()

			id, err := resolveServiceSessionID(cmd.Context(), svc, args[0])
			if err != nil {
				return err
			}
			list, err := svc.Messages(cmd.Context(), id, filter)
			if err != nil {
				return err
			}
			if outputFormat(cmd) == "json" {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(list)
			}
			return printMessagesHuman(cmd.OutOrStdout(), list)
		},
	}
	flags := cmd.Flags()
	flags.Int("from", 0,
		"Starting ordinal (inclusive). Omit for the newest page in "+
			"--direction desc; explicit 0 starts at ordinal 0.")
	flags.Int("limit", 0,
		"Maximum messages to return (0 = server default)")
	flags.String("direction", "asc",
		"Sort direction: asc or desc")
	flags.Int("around", 0,
		"Center a window on this ordinal (use with --before/--after)")
	flags.Int("before", 5, "Messages before --around (default 5)")
	flags.Int("after", 5, "Messages after --around (default 5)")
	flags.String("role", "",
		"Comma-separated roles to include, e.g. user,assistant")
	return cmd
}

// messagesFilterFromFlags builds a service.MessageFilter from the `session
// messages` command's flags. It enforces the CLI-level check that
// --before/--after require --around, and handles the critical
// around-vs-direction/from gotcha: --direction defaults to "asc" and --from
// defaults to 0, so both flags always carry a non-empty value even when the
// user never passed them. Forwarding those defaults on the around path
// would trip the service's around-vs-direction/from mutual-exclusion check
// on every plain `--around N` call, so Direction/From are only forwarded
// when the user actually set the flag; an explicit --from/--direction
// alongside --around still reaches the service, whose error surfaces
// unchanged.
func messagesFilterFromFlags(cmd *cobra.Command) (service.MessageFilter, error) {
	flags := cmd.Flags()

	direction, err := flags.GetString("direction")
	if err != nil {
		return service.MessageFilter{}, err
	}
	if direction != "asc" && direction != "desc" {
		return service.MessageFilter{}, fmt.Errorf(
			"invalid --direction %q: must be asc or desc", direction,
		)
	}
	aroundSet := flags.Changed("around")
	if !aroundSet && (flags.Changed("before") || flags.Changed("after")) {
		return service.MessageFilter{}, fmt.Errorf(
			"--before/--after require --around",
		)
	}

	limit, err := flags.GetInt("limit")
	if err != nil {
		return service.MessageFilter{}, err
	}
	filter := service.MessageFilter{Limit: limit}

	if aroundSet {
		around, err := flags.GetInt("around")
		if err != nil {
			return service.MessageFilter{}, err
		}
		filter.Around = &around
		if flags.Changed("before") {
			before, err := flags.GetInt("before")
			if err != nil {
				return service.MessageFilter{}, err
			}
			filter.Before = &before
		}
		if flags.Changed("after") {
			after, err := flags.GetInt("after")
			if err != nil {
				return service.MessageFilter{}, err
			}
			filter.After = &after
		}
		if flags.Changed("direction") {
			filter.Direction = direction
		}
		if flags.Changed("from") {
			from, err := flags.GetInt("from")
			if err != nil {
				return service.MessageFilter{}, err
			}
			filter.From = &from
		}
	} else {
		filter.Direction = direction
		// Preserve presence: an explicit --from 0 means "start at
		// ordinal 0", not "use the default tail/head".
		if flags.Changed("from") {
			from, err := flags.GetInt("from")
			if err != nil {
				return service.MessageFilter{}, err
			}
			filter.From = &from
		}
	}

	if role, err := flags.GetString("role"); err != nil {
		return service.MessageFilter{}, err
	} else if role != "" {
		filter.Roles = strings.Split(role, ",")
	}

	return filter, nil
}

// printMessagesHuman prints each message as a header block followed
// by its content. Timestamp is trimmed to YYYY-MM-DDTHH:MM:SS.
// Session-derived fields are sanitized so escape sequences embedded
// in agent output can't spoof the terminal.
func printMessagesHuman(w io.Writer, list *service.MessageList) error {
	for _, m := range list.Messages {
		ts := m.Timestamp
		if len(ts) >= 19 {
			ts = ts[:19]
		}
		fmt.Fprintf(w, "--- #%d  %s  %s ---\n",
			m.Ordinal, sanitizeTerminal(m.Role), sanitizeTerminal(ts))
		fmt.Fprintln(w, sanitizeTerminal(m.Content))
		fmt.Fprintln(w)
	}
	return nil
}
