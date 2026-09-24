package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction/review"
)

func newFrictionCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "friction",
		Short:        "Build and read the Friction Log (heuristic session review)",
		GroupID:      groupData,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.PersistentFlags().String("server", "", "Remote daemon URL for friction API requests")
	cmd.PersistentFlags().String("server-token-file", "",
		"File containing bearer token for explicit --server requests")
	cmd.AddCommand(newFrictionRunCommand())
	cmd.AddCommand(newFrictionDigestCommand())
	cmd.AddCommand(newFrictionFindingsCommand())
	cmd.AddCommand(newFrictionPatternsCommand())
	cmd.AddCommand(newFrictionFileCommand(), newFrictionLinkCommand(), newFrictionUnlinkCommand())
	return cmd
}

func newFrictionRunCommand() *cobra.Command {
	var req review.RunRequest
	var asJSON bool
	var format string
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Build missing digests, or one date with --date",
		Long: "Without flags, builds every complete local date that has no digest yet\n" +
			"(within [friction] backfill_days). --date builds one date; --rebuild\n" +
			"re-renders an existing one; --dry-run computes without writing.",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if format != "human" && format != "json" {
				return errors.New("--format must be human or json")
			}
			backend, cleanup, err := resolveFrictionBackend(cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			outcomes, err := backend.Run(cmd.Context(), req)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if asJSON || format == "json" {
				for _, o := range outcomes {
					if _, err := w.Write(o.SummaryJSON); err != nil {
						return err
					}
				}
				return nil
			}
			if len(outcomes) == 0 {
				_, err := fmt.Fprintln(w, "No complete dates need a digest.")
				return err
			}
			for _, o := range outcomes {
				human := o.Human
				if !strings.HasSuffix(human, "\n") {
					human += "\n"
				}
				if _, err := io.WriteString(w, human); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&req.Date, "date", "", "Digest date (YYYY-MM-DD)")
	cmd.Flags().BoolVar(&req.Rebuild, "rebuild", false, "Rebuild an existing digest, keeping its membership")
	cmd.Flags().BoolVar(&req.DryRun, "dry-run", false, "Compute and render without writing")
	cmd.Flags().StringVar(&format, "format", "human", "Output format: human or json")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Print the summary JSON object instead of the human summary")
	return cmd
}

func newFrictionDigestCommand() *cobra.Command {
	var date, from, to, format string
	var asJSON bool
	cmd := &cobra.Command{
		Use:          "digest",
		Short:        "Print a stored digest (Markdown or summary JSON)",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if format != "md" && format != "json" {
				return errors.New("--format must be md or json")
			}
			if date != "" && (from != "" || to != "") {
				return errors.New("--date cannot be combined with --from/--to")
			}
			backend, cleanup, err := resolveFrictionBackend(cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			w := cmd.OutOrStdout()
			if from != "" || to != "" {
				items, err := backend.Digests(cmd.Context(), from, to)
				if err != nil {
					return err
				}
				for _, item := range slices.Backward(items) { // oldest first
					out, err := backend.Digest(cmd.Context(), item.Date)
					if err != nil {
						return err
					}
					payload := out.Markdown
					if asJSON || format == "json" {
						payload = out.SummaryJSON
					}
					if _, err := w.Write(payload); err != nil {
						return err
					}
				}
				return nil
			}
			out, err := backend.Digest(cmd.Context(), date)
			if errors.Is(err, errFrictionDigestMissing) {
				if date == "" {
					return errors.New("no friction digests have been built yet; run `agentsview friction run`")
				}
				return fmt.Errorf("no friction digest for %s", date)
			}
			if err != nil {
				return err
			}
			payload := out.Markdown
			if asJSON || format == "json" {
				payload = out.SummaryJSON
			}
			_, err = w.Write(payload)
			return err
		},
	}
	cmd.Flags().StringVar(&date, "date", "", "Digest date (YYYY-MM-DD); default is the latest digest")
	cmd.Flags().StringVar(&from, "from", "", "First date of a range (YYYY-MM-DD)")
	cmd.Flags().StringVar(&to, "to", "", "Last date of a range (YYYY-MM-DD)")
	cmd.Flags().StringVar(&format, "format", "md", "Output format: md or json")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Print summary JSON (alias for --format json)")
	return cmd
}

func newFrictionFindingsCommand() *cobra.Command {
	var f db.FrictionFindingFilter
	var format string
	var asJSON bool
	cmd := &cobra.Command{
		Use:          "findings",
		Short:        "List stored friction findings",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if format != "table" && format != "json" {
				return errors.New("--format must be table or json")
			}
			backend, cleanup, err := resolveFrictionBackend(cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			page, err := backend.Findings(cmd.Context(), f)
			if err != nil {
				return err
			}
			if asJSON || format == "json" {
				return json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout()), page)
			}
			return printFrictionFindings(cmd.OutOrStdout(), page)
		},
	}
	cmd.Flags().StringVar(&f.Date, "date", "", "Only sessions in this digest date (YYYY-MM-DD)")
	cmd.Flags().StringVar(&f.Kind, "kind", "", "correction, error, workaround, deferral, pattern, frustration or interruption")
	cmd.Flags().StringVar(&f.SessionID, "session", "", "Session ID")
	cmd.Flags().IntVar(&f.Limit, "limit", 0, "Page size (default 100, max 1000)")
	cmd.Flags().StringVar(&f.Cursor, "cursor", "", "Cursor from a previous page")
	cmd.Flags().StringVar(&format, "format", "table", "Output format: table or json")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Print JSON (alias for --format json)")
	return cmd
}

func newFrictionPatternsCommand() *cobra.Command {
	var f db.FrictionPatternFilter
	var linked, unlinked bool
	var format string
	var asJSON bool
	cmd := &cobra.Command{
		Use:          "patterns",
		Short:        "List recurring friction patterns",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if format != "table" && format != "json" {
				return errors.New("--format must be table or json")
			}
			switch {
			case linked:
				f.LinkState = db.FrictionLinkStateLinked
			case unlinked:
				f.LinkState = db.FrictionLinkStateUnlinked
			}
			backend, cleanup, err := resolveFrictionBackend(cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			page, err := backend.Patterns(cmd.Context(), f)
			if err != nil {
				return err
			}
			if asJSON || format == "json" {
				return json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout()), page)
			}
			return printFrictionPatterns(cmd.OutOrStdout(), page)
		},
	}
	cmd.Flags().StringVar(&f.Kind, "kind", "", "correction, error, workaround, deferral, pattern, frustration or interruption")
	cmd.Flags().StringVar(&f.Since, "since", "", "Only patterns last seen on or after this date (YYYY-MM-DD)")
	cmd.Flags().BoolVar(&linked, "linked", false, "Only patterns linked to a Kata issue")
	cmd.Flags().BoolVar(&unlinked, "unlinked", false, "Only patterns without a Kata issue")
	cmd.MarkFlagsMutuallyExclusive("linked", "unlinked")
	cmd.Flags().IntVar(&f.Limit, "limit", 0, "Page size (default 100, max 1000)")
	cmd.Flags().StringVar(&f.Cursor, "cursor", "", "Cursor from a previous page")
	cmd.Flags().StringVar(&format, "format", "table", "Output format: table or json")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Print JSON (alias for --format json)")
	return cmd
}

func printFrictionFindings(w io.Writer, page frictionFindingsPage) error {
	if len(page.Findings) == 0 {
		_, err := fmt.Fprintln(w, "(no friction findings)")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "SESSION\tMSG\tKIND\tDETECTOR\tTITLE")
	for _, f := range page.Findings {
		msg := "-"
		if f.MessageOrdinal != nil {
			msg = strconv.Itoa(*f.MessageOrdinal)
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", sanitizeTerminal(f.SessionID), msg,
			sanitizeTerminal(f.Kind), sanitizeTerminal(f.Detector), sanitizeTerminal(f.Title))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if page.NextCursor != "" {
		_, err := fmt.Fprintf(w, "next page: --cursor %s\n", page.NextCursor)
		return err
	}
	return nil
}

func printFrictionPatterns(w io.Writer, page frictionPatternsPage) error {
	if len(page.Patterns) == 0 {
		_, err := fmt.Fprintln(w, "(no friction patterns)")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "COUNT\tSESSIONS\tLAST SEEN\tKIND\tFINGERPRINT\tTITLE")
	for _, p := range page.Patterns {
		_, _ = fmt.Fprintf(tw, "%d\t%d\t%s\t%s\t%s\t%s\n", p.OccurrenceCount, p.SessionCount,
			sanitizeTerminal(p.LastSeenDate), sanitizeTerminal(p.Kind),
			sanitizeTerminal(p.Fingerprint), sanitizeTerminal(p.Title))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if page.NextCursor != "" {
		_, err := fmt.Fprintf(w, "next page: --cursor %s\n", page.NextCursor)
		return err
	}
	return nil
}
