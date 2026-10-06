// ABOUTME: `session label` and `session parent` subcommands — record
// ABOUTME: caller-supplied labels and launcher parent links on sessions.
package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

func newSessionLabelCommand() *cobra.Command {
	var (
		remove   []string
		replace  bool
		clearAll bool
	)
	cmd := &cobra.Command{
		Use:   "label <session-id> [label...]",
		Short: "Show, add, or remove session labels",
		Long: "Labels are free-form tags such as ticket=ABC-123 or role=reviewer.\n" +
			"With no labels and no flags, prints the session's labels. Labels\n" +
			"may be recorded before the session has synced; they appear once it\n" +
			"does. Filter with `session list --label`.",
		Example: "  agentsview session label <id> ticket=ABC-123 role=reviewer\n" +
			"  agentsview session label <id> --remove role=reviewer\n" +
			"  agentsview session label <id> --replace nightly\n" +
			"  agentsview session label <id> --clear",
		Args:         cobra.MinimumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			id, labels := args[0], args[1:]
			if clearAll && (replace || len(labels) > 0 || len(remove) > 0) {
				return errors.New("--clear cannot be combined with labels, --replace, or --remove")
			}
			if replace && len(remove) > 0 {
				return errors.New("--replace cannot be combined with --remove")
			}
			write := clearAll || replace || len(labels) > 0 || len(remove) > 0
			annotator, cleanup, err := resolveSessionAnnotator(cmd, write)
			if err != nil {
				return err
			}
			defer cleanup()

			var result *db.SessionLabels
			switch {
			case clearAll:
				result, err = annotator.SetSessionLabels(cmd.Context(), id, []string{})
			case replace:
				result, err = annotator.SetSessionLabels(cmd.Context(), id, labels)
			case write:
				result, err = annotator.UpdateSessionLabels(cmd.Context(), id, labels, remove)
			default:
				result, err = annotator.SessionLabels(cmd.Context(), id)
			}
			if err != nil {
				return err
			}
			if outputFormat(cmd) == "json" {
				return json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout()), result)
			}
			printSessionLabelsHuman(cmd.OutOrStdout(), result)
			return nil
		},
	}
	flags := cmd.Flags()
	flags.StringArrayVar(&remove, "remove", nil,
		"Remove this label (repeatable)")
	flags.BoolVar(&replace, "replace", false,
		"Replace every existing label with the given labels")
	flags.BoolVar(&clearAll, "clear", false,
		"Remove every label")
	return cmd
}

func newSessionParentCommand() *cobra.Command {
	var clearAll bool
	cmd := &cobra.Command{
		Use:   "parent <session-id> [parent-session-id]",
		Short: "Show, set, or remove a launcher-supplied parent session",
		Long: "Records which session launched this one, for example an orchestrator\n" +
			"that starts worker sessions as separate processes. The link appears\n" +
			"in the session tree only while the transcript itself names no\n" +
			"parent: parser-derived subagent, fork, and continuation links win.\n" +
			"The link makes the session a subagent of its launcher. It may be\n" +
			"recorded before the session has synced.",
		Example: "  agentsview session parent <worker-id> <manager-id>\n" +
			"  agentsview session parent <worker-id> --clear",
		Args:         cobra.RangeArgs(1, 2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			parentID := ""
			if len(args) == 2 {
				parentID = args[1]
			}
			if clearAll && parentID != "" {
				return errors.New("--clear cannot be combined with a parent session id")
			}
			annotator, cleanup, err := resolveSessionAnnotator(cmd, clearAll || parentID != "")
			if err != nil {
				return err
			}
			defer cleanup()

			var link *db.SessionExternalParent
			switch {
			case clearAll:
				link, err = annotator.ClearSessionParent(cmd.Context(), id)
			case parentID != "":
				link, err = annotator.SetSessionParent(cmd.Context(), id, parentID)
			default:
				link, err = annotator.SessionParent(cmd.Context(), id)
			}
			if err != nil {
				return err
			}
			if outputFormat(cmd) == "json" {
				return json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout()), link)
			}
			printSessionParentHuman(cmd.OutOrStdout(), id, link, clearAll)
			return nil
		},
	}
	cmd.Flags().BoolVar(&clearAll, "clear", false,
		"Remove the launcher-supplied parent link")
	return cmd
}

func resolveSessionAnnotator(
	cmd *cobra.Command, write bool,
) (service.SessionAnnotator, func(), error) {
	resolve := resolveService
	if write {
		resolve = resolveWritableService
	}
	svc, cleanup, err := resolve(cmd)
	if err != nil {
		return nil, nil, err
	}
	annotator, ok := svc.(service.SessionAnnotator)
	if !ok {
		cleanup()
		return nil, nil, errors.New(
			"this session backend does not store labels or parent links")
	}
	return annotator, cleanup, nil
}

func printSessionLabelsHuman(w io.Writer, labels *db.SessionLabels) {
	if len(labels.Labels) == 0 {
		fmt.Fprintf(w, "%s: (no labels)\n", sanitizeTerminal(labels.SessionID))
	} else {
		fmt.Fprintf(w, "%s: %s\n", sanitizeTerminal(labels.SessionID),
			sanitizeTerminal(strings.Join(labels.Labels, ", ")))
	}
	if !labels.SessionFound {
		fmt.Fprintln(w, "note: session not synced yet; labels apply once it is")
	}
}

func printSessionParentHuman(
	w io.Writer, id string, link *db.SessionExternalParent, cleared bool,
) {
	switch {
	case link == nil:
		fmt.Fprintf(w, "%s: no launcher-supplied parent\n", sanitizeTerminal(id))
		return
	case cleared:
		fmt.Fprintf(w, "%s: removed parent %s\n", sanitizeTerminal(id),
			sanitizeTerminal(link.ParentSessionID))
		return
	}
	fmt.Fprintf(w, "%s: parent %s (%s)\n", sanitizeTerminal(id),
		sanitizeTerminal(link.ParentSessionID),
		sanitizeTerminal(link.RelationshipType))
	switch {
	case !link.SessionFound:
		fmt.Fprintln(w, "note: session not synced yet; the link applies once it is")
	case !link.Applied:
		fmt.Fprintln(w, "note: the transcript names its own parent, which takes precedence")
	}
}
