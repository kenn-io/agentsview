package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/spf13/cobra"

	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction/filing"
	"go.kenn.io/agentsview/internal/kata"
	"go.kenn.io/agentsview/internal/service"
)

// frictionFileOutput matches the file route's response, so daemon and local
// runs print the same JSON.
type frictionFileOutput struct {
	Issue   *service.FrictionIssueView `json:"issue,omitempty"`
	Created bool                       `json:"created"`
	Preview *frictionIssuePreview      `json:"preview,omitempty"`
}

type frictionIssuePreview struct {
	Title    string         `json:"title"`
	Body     string         `json:"body"`
	Priority int            `json:"priority"`
	Labels   []string       `json:"labels"`
	Metadata map[string]any `json:"metadata"`
}

// frictionIssueTarget files through a daemon (api) or the local archive
// (filer); exactly one is set.
type frictionIssueTarget struct {
	api     *apiclient.Client
	filer   *filing.Filer
	cleanup func()
}

func newFrictionFileCommand() *cobra.Command {
	var dryRun, asJSON bool
	var format string
	cmd := &cobra.Command{
		Use:   "file <fingerprint>",
		Short: "File a friction pattern to Kata, or link the issue Kata already holds",
		Long: "Files the pattern to the configured Kata project and records the link.\n" +
			"A pattern that is already linked, or that Kata already tracks, is not\n" +
			"filed again. Only the AgentsView hub files. --dry-run prints the issue\n" +
			"without contacting Kata.",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if format != "human" && format != "json" {
				return errors.New("--format must be human or json")
			}
			target, err := resolveFrictionIssueTarget(cmd)
			if err != nil {
				return err
			}
			defer target.cleanup()
			out, err := target.file(cmd.Context(), args[0], dryRun)
			if err != nil {
				return err
			}
			if asJSON || format == "json" {
				return json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout()), out)
			}
			return printFrictionFile(cmd.OutOrStdout(), out)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print the issue without contacting Kata")
	cmd.Flags().StringVar(&format, "format", "human", "Output format: human or json")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Print JSON (alias for --format json)")
	return cmd
}

func newFrictionLinkCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "link <fingerprint> <issue>",
		Short: "Link a friction pattern to an existing Kata issue",
		Long: "Records issue as the pattern's Kata issue, replacing any link. issue is\n" +
			"a qualified ID (agentsview#f001), a short ID (f001) or an issue UID.",
		Args:         cobra.ExactArgs(2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := resolveFrictionIssueTarget(cmd)
			if err != nil {
				return err
			}
			defer target.cleanup()
			issue, err := target.link(cmd.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Linked %s to %s\n",
				sanitizeTerminal(args[0]), sanitizeTerminal(issue.QualifiedID))
			return err
		},
	}
}

func newFrictionUnlinkCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "unlink <fingerprint>",
		Short:        "Remove a friction pattern's Kata issue link",
		Long:         "Removes the local link only; the Kata issue is left as it is.",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := resolveFrictionIssueTarget(cmd)
			if err != nil {
				return err
			}
			defer target.cleanup()
			if err := target.unlink(cmd.Context(), args[0]); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Removed the Kata link for %s; the issue is unchanged.\n",
				sanitizeTerminal(args[0]))
			return err
		},
	}
}

// resolveFrictionIssueTarget follows friction run: --server, else a running
// (or auto-started) daemon, else the local archive (AGENTSVIEW_NO_DAEMON=1).
func resolveFrictionIssueTarget(cmd *cobra.Command) (frictionIssueTarget, error) {
	if pgReadRequested(cmd) {
		return frictionIssueTarget{}, errors.New("--pg is read-only; file from the hub's daemon or with --server")
	}
	if remote, _ := cmd.Flags().GetString("server"); strings.TrimSpace(remote) != "" {
		token, err := explicitServerToken(cmd)
		if err != nil {
			return frictionIssueTarget{}, err
		}
		return frictionIssueHTTPTarget(strings.TrimSpace(remote), token)
	}
	cfg, err := config.LoadPFlagsFor(cmd.Flags(), config.ScopeLocal)
	if err != nil {
		return frictionIssueTarget{}, fmt.Errorf("loading config: %w", err)
	}
	tr, err := ensureTransportContext(cmd.Context(), &cfg, transportIntentArchiveWrite, 0)
	if err != nil {
		return frictionIssueTarget{}, err
	}
	if tr.Mode == transportHTTP {
		return frictionIssueHTTPTarget(tr.URL, cfg.AuthToken)
	}
	if err := cfg.Kata.Validate(); err != nil {
		return frictionIssueTarget{}, err
	}
	conn := kata.NewConn(kata.ConfigFrom(cfg.Kata, filing.EligibleHost(false, cfg.HasPGPushTarget())))
	if err := filing.CheckHub(conn.Config()); err != nil {
		return frictionIssueTarget{}, err
	}
	database, lock, err := openWriteDB(cmd.Context(), cfg)
	if err != nil {
		return frictionIssueTarget{}, fmt.Errorf("opening database: %w", err)
	}
	return frictionIssueTarget{
		filer: &filing.Filer{
			Kata: conn, Store: database,
			PublicURL: strings.TrimRight(strings.TrimSpace(cfg.PublicURL), "/"),
		},
		cleanup: func() { closeWriteDB(database, lock) },
	}, nil
}

func frictionIssueHTTPTarget(baseURL, token string) (frictionIssueTarget, error) {
	api, err := apiclient.NewHTTPClient(baseURL, token, &http.Client{})
	if err != nil {
		return frictionIssueTarget{}, err
	}
	return frictionIssueTarget{api: api, cleanup: func() {}}, nil
}

func (t frictionIssueTarget) file(ctx context.Context, fingerprint string, dryRun bool) (frictionFileOutput, error) {
	if t.filer != nil {
		if dryRun {
			req, err := t.filer.Preview(ctx, fingerprint)
			return frictionFileOutput{Preview: &frictionIssuePreview{
				Title: req.Title, Body: req.Body, Priority: req.Priority, Labels: req.Labels, Metadata: req.Metadata,
			}}, err
		}
		res, err := t.filer.File(ctx, fingerprint)
		if err != nil {
			return frictionFileOutput{}, err
		}
		return frictionFileOutput{Issue: frictionIssueOutput(res.Link), Created: res.Created}, nil
	}
	resp, err := t.api.PostAPIV1FrictionPatternsFingerprintIssueWithResponse(ctx,
		&apiclient.PostAPIV1FrictionPatternsFingerprintIssueRequestOptions{
			PathParams: &apiclient.PostAPIV1FrictionPatternsFingerprintIssuePath{Fingerprint: fingerprint},
			Body:       &apiclient.PostAPIV1FrictionPatternsFingerprintIssueBody{DryRun: new(dryRun)},
		})
	if resp == nil {
		return frictionFileOutput{}, fmt.Errorf("friction file: %w", err)
	}
	var out frictionFileOutput
	if err := frictionIssueHTTPResult("friction file", resp.HTTPResponse, resp.Body, err, &out); err != nil {
		return frictionFileOutput{}, err
	}
	return out, nil
}

func (t frictionIssueTarget) link(ctx context.Context, fingerprint, ref string) (*service.FrictionIssueView, error) {
	if t.filer != nil {
		link, err := t.filer.Link(ctx, fingerprint, ref)
		if err != nil {
			return nil, err
		}
		return frictionIssueOutput(link), nil
	}
	resp, err := t.api.PutAPIV1FrictionPatternsFingerprintIssueWithResponse(ctx,
		&apiclient.PutAPIV1FrictionPatternsFingerprintIssueRequestOptions{
			PathParams: &apiclient.PutAPIV1FrictionPatternsFingerprintIssuePath{Fingerprint: fingerprint},
			Body:       &apiclient.PutAPIV1FrictionPatternsFingerprintIssueBody{Issue: ref},
		})
	if resp == nil {
		return nil, fmt.Errorf("friction link: %w", err)
	}
	var out struct {
		Issue *service.FrictionIssueView `json:"issue"`
	}
	if err := frictionIssueHTTPResult("friction link", resp.HTTPResponse, resp.Body, err, &out); err != nil {
		return nil, err
	}
	if out.Issue == nil {
		return nil, errors.New("friction link: response has no issue")
	}
	return out.Issue, nil
}

func (t frictionIssueTarget) unlink(ctx context.Context, fingerprint string) error {
	if t.filer != nil {
		return t.filer.Unlink(ctx, fingerprint)
	}
	resp, err := t.api.DeleteAPIV1FrictionPatternsFingerprintIssueWithResponse(ctx,
		&apiclient.DeleteAPIV1FrictionPatternsFingerprintIssueRequestOptions{
			PathParams: &apiclient.DeleteAPIV1FrictionPatternsFingerprintIssuePath{Fingerprint: fingerprint},
		})
	if resp == nil {
		return fmt.Errorf("friction unlink: %w", err)
	}
	return frictionIssueHTTPResult("friction unlink", resp.HTTPResponse, resp.Body, err, nil)
}

// frictionIssueHTTPResult turns a route response into out, or an error that
// carries the server's message.
func frictionIssueHTTPResult(op string, resp *http.Response, body []byte, err error, out any) error {
	if resp == nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiErr struct {
			Error string `json:"error"`
		}
		msg := strings.TrimSpace(string(body))
		if json.Unmarshal(body, &apiErr) == nil && apiErr.Error != "" {
			msg = apiErr.Error
		}
		return fmt.Errorf("%s: HTTP %d: %s", op, resp.StatusCode, msg)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decoding %s response: %w", op, err)
	}
	return nil
}

func frictionIssueOutput(l db.FrictionIssueLink) *service.FrictionIssueView {
	return &service.FrictionIssueView{
		QualifiedID: l.QualifiedID, WebURL: l.WebURL, IssueUID: l.IssueUID,
		Source: l.Source, LinkedAt: l.LinkedAt.UTC(),
	}
}

func printFrictionFile(w io.Writer, out frictionFileOutput) error {
	if p := out.Preview; p != nil {
		_, err := fmt.Fprintf(w, "Title:    %s\nPriority: %d\nLabels:   %s\n\n%s",
			sanitizeTerminal(p.Title), p.Priority, sanitizeTerminal(strings.Join(p.Labels, ", ")),
			sanitizeTerminal(p.Body))
		return err
	}
	if out.Issue == nil {
		return errors.New("friction file: response has no issue")
	}
	verb := "Linked the existing Kata issue"
	if out.Created {
		verb = "Filed"
	}
	line := verb + " " + out.Issue.QualifiedID
	if out.Issue.WebURL != "" {
		line += ": " + out.Issue.WebURL
	}
	_, err := fmt.Fprintln(w, sanitizeTerminal(line))
	return err
}
