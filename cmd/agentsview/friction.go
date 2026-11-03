package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/friction/review"
	"go.kenn.io/agentsview/internal/service"
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
	cmd.PersistentFlags().Bool("pg", false, "Read friction data from configured PostgreSQL")
	cmd.AddCommand(newFrictionRunCommand())
	cmd.AddCommand(newFrictionDigestCommand())
	cmd.AddCommand(newFrictionFindingsCommand())
	cmd.AddCommand(newFrictionPatternsCommand())
	return cmd
}

// resolveFrictionService returns a service that can read the Friction Log.
func resolveFrictionService(cmd *cobra.Command) (service.FrictionService, func(), error) {
	svc, cleanup, err := resolveService(cmd)
	if err != nil {
		return nil, nil, err
	}
	fs, ok := svc.(service.FrictionService)
	if !ok || !service.SupportsFriction(svc) {
		cleanup()
		return nil, nil, errors.New("this backend does not serve the Friction Log")
	}
	return fs, cleanup, nil
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
			if pgReadRequested(cmd) {
				return errors.New("friction run builds digests in the local archive or daemon; --pg is read-only")
			}
			reports, err := runFrictionReview(cmd, req)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if asJSON || format == "json" {
				for _, r := range reports {
					if _, err := w.Write(r.Summary); err != nil {
						return err
					}
				}
				return nil
			}
			if len(reports) == 0 {
				_, err := fmt.Fprintln(w, "No complete dates need a digest.")
				return err
			}
			for _, r := range reports {
				line, err := frictionRunLine(r)
				if err != nil {
					return err
				}
				if _, err := fmt.Fprintln(w, line); err != nil {
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

// frictionRunReport is one built (or dry-run) date. Summary holds the
// summary JSON in friction.RenderSummaryJSON form.
type frictionRunReport struct {
	Date    string
	Written bool
	Summary []byte
}

// runFrictionReview runs on --server, else a running (or auto-started)
// daemon, else the local archive directly (AGENTSVIEW_NO_DAEMON=1).
func runFrictionReview(cmd *cobra.Command, req review.RunRequest) ([]frictionRunReport, error) {
	if remote, _ := cmd.Flags().GetString("server"); strings.TrimSpace(remote) != "" {
		token, err := explicitServerToken(cmd)
		if err != nil {
			return nil, err
		}
		return runFrictionReviewHTTP(cmd.Context(), strings.TrimSpace(remote), token, req)
	}
	// Like other archive commands, load only local settings so an unrelated
	// section cannot block a run; [friction] is checked before running here.
	cfg, err := config.LoadPFlagsFor(cmd.Flags(), config.ScopeLocal)
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}
	tr, err := ensureTransportContext(cmd.Context(), &cfg, transportIntentArchiveWrite, 0)
	if err != nil {
		return nil, err
	}
	if tr.Mode == transportHTTP {
		return runFrictionReviewHTTP(cmd.Context(), tr.URL, cfg.AuthToken, req)
	}
	if err := cfg.Friction.Validate(); err != nil {
		return nil, err
	}
	database, lock, err := openWriteDB(cmd.Context(), cfg)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}
	defer closeWriteDB(database, lock)
	runner, err := newFrictionRunner(cfg, database, time.Now)
	if err != nil {
		return nil, err
	}
	if runner == nil {
		return nil, errors.New("friction review is disabled; set [friction] enabled = true in config.toml")
	}
	var out []frictionRunReport
	err = runner.Run(cmd.Context(), req, nil, func(r review.Report) {
		out = append(out, frictionRunReport{
			Date: r.Date, Written: r.Written,
			Summary: friction.RenderSummaryJSON(r.Snapshot.DigestSnapshot),
		})
	})
	return out, err
}

func runFrictionReviewHTTP(
	ctx context.Context, baseURL, token string, req review.RunRequest,
) ([]frictionRunReport, error) {
	api, err := apiclient.NewHTTPClient(baseURL, token, &http.Client{})
	if err != nil {
		return nil, err
	}
	resp, err := api.PostAPIV1FrictionRunWithResponse(ctx, &apiclient.PostAPIV1FrictionRunRequestOptions{
		Body: &apiclient.PostAPIV1FrictionRunBody{Date: new(req.Date), Rebuild: new(req.Rebuild), DryRun: new(req.DryRun)},
	})
	if resp == nil {
		return nil, fmt.Errorf("friction run: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var apiErr struct {
			Error string `json:"error"`
		}
		msg := strings.TrimSpace(string(resp.Body))
		if json.Unmarshal(resp.Body, &apiErr) == nil && apiErr.Error != "" {
			msg = apiErr.Error
		}
		return nil, fmt.Errorf("friction run: HTTP %d: %s", resp.StatusCode, msg)
	}
	if err != nil {
		return nil, fmt.Errorf("friction run: %w", err)
	}
	var body struct {
		Reports []struct {
			Date    string         `json:"date"`
			Written bool           `json:"written"`
			Summary jsontext.Value `json:"summary"`
		} `json:"reports"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		return nil, fmt.Errorf("decoding friction run: %w", err)
	}
	out := make([]frictionRunReport, 0, len(body.Reports))
	for _, r := range body.Reports {
		summary, err := friction.CanonicalSummaryJSON(r.Summary)
		if err != nil {
			return nil, err
		}
		out = append(out, frictionRunReport{Date: r.Date, Written: r.Written, Summary: summary})
	}
	return out, nil
}

// frictionRunLine summarizes one report for a terminal.
func frictionRunLine(r frictionRunReport) (string, error) {
	var s struct {
		SessionsScanned int                 `json:"sessions_scanned"`
		Corrections     int                 `json:"corrections"`
		Errors          int                 `json:"errors"`
		Workarounds     int                 `json:"workarounds"`
		Deferrals       int                 `json:"deferrals"`
		Patterns        int                 `json:"patterns"`
		Frustrations    int                 `json:"frustrations"`
		Interruptions   int                 `json:"interruptions"`
		P0Alerts        map[string][]string `json:"p0_alerts"`
	}
	if err := json.Unmarshal(r.Summary, &s); err != nil {
		return "", fmt.Errorf("decoding friction summary for %s: %w", r.Date, err)
	}
	state := "written"
	if !r.Written {
		state = "not written"
	}
	return fmt.Sprintf(
		"%s: %d session(s) scanned; %d correction(s), %d error(s), %d workaround(s), "+
			"%d deferral(s), %d pattern(s), %d frustration(s), %d interruption(s); %d P0 alert(s) (%s)",
		r.Date, s.SessionsScanned, s.Corrections, s.Errors, s.Workarounds,
		s.Deferrals, s.Patterns, s.Frustrations, s.Interruptions, len(s.P0Alerts), state,
	), nil
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
			fs, cleanup, err := resolveFrictionService(cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			w := cmd.OutOrStdout()
			write := func(d *service.FrictionDigestView) error {
				payload := []byte(d.Markdown)
				if asJSON || format == "json" {
					payload = d.Summary
				}
				_, err := w.Write(payload)
				return err
			}
			if from != "" || to != "" {
				items, err := fs.FrictionDigests(cmd.Context(), from, to)
				if err != nil {
					return err
				}
				for _, item := range slices.Backward(items) { // oldest first
					d, err := fs.FrictionDigest(cmd.Context(), item.Date)
					if err != nil {
						return err
					}
					if err := write(d); err != nil {
						return err
					}
				}
				return nil
			}
			d, err := fs.FrictionDigest(cmd.Context(), date)
			if errors.Is(err, service.ErrFrictionDigestNotFound) {
				if date == "" {
					return errors.New("no friction digests have been built yet; run `agentsview friction run`")
				}
				return fmt.Errorf("no friction digest for %s", date)
			}
			if err != nil {
				return err
			}
			return write(d)
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
	var f service.FrictionFindingFilter
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
			fs, cleanup, err := resolveFrictionService(cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			page, err := fs.FrictionFindings(cmd.Context(), f)
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
	var f service.FrictionPatternFilter
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
			fs, cleanup, err := resolveFrictionService(cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			page, err := fs.FrictionPatterns(cmd.Context(), f)
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
	cmd.Flags().IntVar(&f.Limit, "limit", 0, "Page size (default 100, max 1000)")
	cmd.Flags().StringVar(&f.Cursor, "cursor", "", "Cursor from a previous page")
	cmd.Flags().StringVar(&format, "format", "table", "Output format: table or json")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Print JSON (alias for --format json)")
	return cmd
}

func printFrictionFindings(w io.Writer, page *service.FrictionFindingPage) error {
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

func printFrictionPatterns(w io.Writer, page *service.FrictionPatternPage) error {
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
