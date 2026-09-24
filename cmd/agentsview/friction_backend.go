package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/friction/review"
)

var errFrictionDigestMissing = errors.New("friction digest not found")

type frictionRunOutcome struct {
	Date        string
	Written     bool
	SummaryJSON []byte
	Human       string
}

type frictionDigestOut struct {
	Date        string
	SummaryJSON []byte
	Markdown    []byte
}

type frictionDigestListItem struct {
	Date            string    `json:"date"`
	Timezone        string    `json:"timezone"`
	RulesVersion    string    `json:"rules_version"`
	BuiltAt         time.Time `json:"built_at"`
	Revision        int       `json:"revision"`
	SessionsScanned int       `json:"sessions_scanned"`
}

type frictionFindingOut struct {
	SessionID      string     `json:"session_id"`
	Kind           string     `json:"kind"`
	Detector       string     `json:"detector"`
	MessageOrdinal *int       `json:"message_ordinal"`
	CallIndex      *int       `json:"call_index"`
	ToolName       string     `json:"tool_name"`
	Label          string     `json:"label"`
	Text           string     `json:"text"`
	Evidence       string     `json:"evidence"`
	Title          string     `json:"title"`
	Fingerprint    string     `json:"fingerprint"`
	OccurredAt     *time.Time `json:"occurred_at"`
	Seq            int        `json:"seq"`
	RulesVersion   string     `json:"rules_version"`
}

type frictionFindingsPage struct {
	Findings   []frictionFindingOut `json:"findings"`
	NextCursor string               `json:"next_cursor"`
}

type frictionPatternsPage struct {
	Patterns   []db.FrictionPattern `json:"patterns"`
	NextCursor string               `json:"next_cursor"`
}

type frictionBackend interface {
	Run(ctx context.Context, req review.RunRequest) ([]frictionRunOutcome, error)
	Digest(ctx context.Context, date string) (frictionDigestOut, error)
	Digests(ctx context.Context, from, to string) ([]frictionDigestListItem, error)
	Findings(ctx context.Context, f db.FrictionFindingFilter) (frictionFindingsPage, error)
	Patterns(ctx context.Context, f db.FrictionPatternFilter) (frictionPatternsPage, error)
}

// resolveFrictionBackend uses --server, else a running (or auto-started)
// daemon, else the local archive directly (AGENTSVIEW_NO_DAEMON=1).
func resolveFrictionBackend(cmd *cobra.Command) (frictionBackend, func(), error) {
	if remote, _ := cmd.Flags().GetString("server"); strings.TrimSpace(remote) != "" {
		token, err := explicitServerToken(cmd)
		if err != nil {
			return nil, nil, err
		}
		return frictionDaemonBackend{baseURL: strings.TrimSpace(remote), token: token}, func() {}, nil
	}
	cfg, err := config.LoadPFlags(cmd.Flags())
	if err != nil {
		return nil, nil, fmt.Errorf("loading config: %w", err)
	}
	tr, err := ensureTransportContext(cmd.Context(), &cfg, transportIntentArchiveWrite, 0)
	if err != nil {
		return nil, nil, err
	}
	if tr.Mode == transportHTTP {
		return frictionDaemonBackend{baseURL: tr.URL, token: cfg.AuthToken}, func() {}, nil
	}
	database, lock, err := openWriteDB(cmd.Context(), cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("opening database: %w", err)
	}
	runner, err := newFrictionRunner(cfg, database, time.Now)
	if err != nil {
		closeWriteDB(database, lock)
		return nil, nil, err
	}
	return frictionLocalBackend{db: database, runner: runner},
		func() { closeWriteDB(database, lock) }, nil
}

// --- local ---

type frictionLocalBackend struct {
	db     *db.DB
	runner *review.Runner
}

func (b frictionLocalBackend) Run(ctx context.Context, req review.RunRequest) ([]frictionRunOutcome, error) {
	if b.runner == nil {
		return nil, errors.New("friction review is disabled; remove [friction] enabled = false from config.toml")
	}
	reports, err := b.runner.Run(ctx, req)
	if err != nil {
		return nil, err
	}
	out := make([]frictionRunOutcome, 0, len(reports))
	for _, r := range reports {
		out = append(out, frictionRunOutcome{
			Date: r.Date, Written: r.Written,
			SummaryJSON: friction.RenderSummaryJSON(r.Snapshot, r.Meta),
			Human:       friction.HumanSummary(r.Snapshot, r.Meta),
		})
	}
	return out, nil
}

func (b frictionLocalBackend) Digest(ctx context.Context, date string) (frictionDigestOut, error) {
	if date == "" {
		latest, err := b.db.LatestFrictionDigestDate(ctx)
		if err != nil {
			return frictionDigestOut{}, err
		}
		if latest == "" {
			return frictionDigestOut{}, errFrictionDigestMissing
		}
		date = latest
	}
	d, err := b.db.GetFrictionDigest(ctx, date)
	if err != nil {
		return frictionDigestOut{}, err
	}
	if d == nil {
		return frictionDigestOut{}, errFrictionDigestMissing
	}
	return frictionDigestOut{Date: d.Date, SummaryJSON: d.SummaryJSON, Markdown: d.Markdown}, nil
}

func (b frictionLocalBackend) Digests(ctx context.Context, from, to string) ([]frictionDigestListItem, error) {
	rows, err := b.db.ListFrictionDigests(ctx, from, to)
	if err != nil {
		return nil, err
	}
	out := make([]frictionDigestListItem, 0, len(rows))
	for _, d := range rows {
		out = append(out, frictionDigestListItem{
			Date: d.Date, Timezone: d.Timezone,
			RulesVersion: d.RulesVersion, BuiltAt: d.BuiltAt.UTC(), Revision: d.Revision,
			SessionsScanned: d.SessionsScanned,
		})
	}
	return out, nil
}

func (b frictionLocalBackend) Findings(ctx context.Context, f db.FrictionFindingFilter) (frictionFindingsPage, error) {
	rows, next, err := b.db.ListFrictionFindings(ctx, f)
	if err != nil {
		return frictionFindingsPage{}, err
	}
	page := frictionFindingsPage{Findings: make([]frictionFindingOut, 0, len(rows)), NextCursor: next}
	for _, r := range rows {
		page.Findings = append(page.Findings, frictionFindingOut{
			SessionID: r.SessionID, Kind: r.Kind, Detector: r.Detector,
			MessageOrdinal: r.MessageOrdinal, CallIndex: r.CallIndex, ToolName: r.ToolName,
			Label: r.Label, Text: r.Text, Evidence: r.Evidence, Title: r.Title,
			Fingerprint: r.Fingerprint, OccurredAt: r.OccurredAt, Seq: r.Seq,
			RulesVersion: r.RulesVersion,
		})
	}
	return page, nil
}

func (b frictionLocalBackend) Patterns(ctx context.Context, f db.FrictionPatternFilter) (frictionPatternsPage, error) {
	rows, next, err := b.db.ListFrictionPatterns(ctx, f)
	if err != nil {
		return frictionPatternsPage{}, err
	}
	return frictionPatternsPage{Patterns: rows, NextCursor: next}, nil
}

// --- daemon ---

type frictionDaemonBackend struct {
	baseURL string
	token   string
}

func (b frictionDaemonBackend) client() (*apiclient.Client, error) {
	return apiclient.NewHTTPClient(b.baseURL, b.token, &http.Client{Timeout: 0})
}

// frictionHTTPCheck turns a non-200 into an error carrying the server's message.
func frictionHTTPCheck(op string, resp *http.Response, body []byte, err error) error {
	if resp == nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return errFrictionDigestMissing
	}
	if resp.StatusCode != http.StatusOK {
		var apiErr struct {
			Error string `json:"error"`
		}
		msg := strings.TrimSpace(string(body))
		if json.Unmarshal(body, &apiErr) == nil && apiErr.Error != "" {
			msg = apiErr.Error
		}
		return fmt.Errorf("%s: HTTP %d: %s", op, resp.StatusCode, msg)
	}
	return err
}

func (b frictionDaemonBackend) Run(ctx context.Context, req review.RunRequest) ([]frictionRunOutcome, error) {
	api, err := b.client()
	if err != nil {
		return nil, err
	}
	resp, err := api.PostAPIV1FrictionRunWithResponse(ctx, &apiclient.PostAPIV1FrictionRunRequestOptions{
		Body: &apiclient.PostAPIV1FrictionRunBody{Date: new(req.Date), Rebuild: new(req.Rebuild), DryRun: new(req.DryRun)},
	})
	if resp == nil {
		return nil, fmt.Errorf("friction run: %w", err)
	}
	if err := frictionHTTPCheck("friction run", resp.HTTPResponse, resp.Body, err); err != nil {
		return nil, err
	}
	var body struct {
		Reports []struct {
			Date    string         `json:"date"`
			Written bool           `json:"written"`
			Human   string         `json:"human"`
			Summary jsontext.Value `json:"summary"`
		} `json:"reports"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		return nil, fmt.Errorf("decoding friction run: %w", err)
	}
	out := make([]frictionRunOutcome, 0, len(body.Reports))
	for _, r := range body.Reports {
		summary, err := friction.CanonicalSummaryJSON(r.Summary)
		if err != nil {
			return nil, err
		}
		out = append(out, frictionRunOutcome{Date: r.Date, Written: r.Written, SummaryJSON: summary, Human: r.Human})
	}
	return out, nil
}

func (b frictionDaemonBackend) Digest(ctx context.Context, date string) (frictionDigestOut, error) {
	api, err := b.client()
	if err != nil {
		return frictionDigestOut{}, err
	}
	if date == "" {
		items, err := b.Digests(ctx, "", "")
		if err != nil {
			return frictionDigestOut{}, err
		}
		if len(items) == 0 {
			return frictionDigestOut{}, errFrictionDigestMissing
		}
		date = items[0].Date
	}
	detail, err := api.GetAPIV1FrictionDigestsDateWithResponse(ctx, &apiclient.GetAPIV1FrictionDigestsDateRequestOptions{
		PathParams: &apiclient.GetAPIV1FrictionDigestsDatePath{Date: url.PathEscape(date)},
	})
	if detail == nil {
		return frictionDigestOut{}, fmt.Errorf("friction digest: %w", err)
	}
	if err := frictionHTTPCheck("friction digest", detail.HTTPResponse, detail.Body, err); err != nil {
		return frictionDigestOut{}, err
	}
	var body struct {
		Date    string         `json:"date"`
		Summary jsontext.Value `json:"summary"`
	}
	if err := json.Unmarshal(detail.Body, &body); err != nil {
		return frictionDigestOut{}, fmt.Errorf("decoding friction digest: %w", err)
	}
	summary, err := friction.CanonicalSummaryJSON(body.Summary)
	if err != nil {
		return frictionDigestOut{}, err
	}
	md, err := api.GetAPIV1FrictionDigestsDateMdWithResponse(ctx, &apiclient.GetAPIV1FrictionDigestsDateMdRequestOptions{
		PathParams: &apiclient.GetAPIV1FrictionDigestsDateMdPath{Date: url.PathEscape(date)},
	})
	if md == nil {
		return frictionDigestOut{}, fmt.Errorf("friction digest markdown: %w", err)
	}
	if err := frictionHTTPCheck("friction digest markdown", md.HTTPResponse, md.Body, err); err != nil {
		return frictionDigestOut{}, err
	}
	return frictionDigestOut{Date: body.Date, SummaryJSON: summary, Markdown: md.Body}, nil
}

func (b frictionDaemonBackend) Digests(ctx context.Context, from, to string) ([]frictionDigestListItem, error) {
	api, err := b.client()
	if err != nil {
		return nil, err
	}
	q := &apiclient.GetAPIV1FrictionDigestsQuery{}
	if from != "" {
		q.From = new(from)
	}
	if to != "" {
		q.To = new(to)
	}
	resp, err := api.GetAPIV1FrictionDigestsWithResponse(ctx, &apiclient.GetAPIV1FrictionDigestsRequestOptions{Query: q})
	if resp == nil {
		return nil, fmt.Errorf("friction digests: %w", err)
	}
	if err := frictionHTTPCheck("friction digests", resp.HTTPResponse, resp.Body, err); err != nil {
		return nil, err
	}
	var body struct {
		Digests []frictionDigestListItem `json:"digests"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		return nil, fmt.Errorf("decoding friction digests: %w", err)
	}
	return body.Digests, nil
}

func (b frictionDaemonBackend) Findings(ctx context.Context, f db.FrictionFindingFilter) (frictionFindingsPage, error) {
	api, err := b.client()
	if err != nil {
		return frictionFindingsPage{}, err
	}
	q := &apiclient.GetAPIV1FrictionFindingsQuery{}
	for _, p := range []struct {
		dst **string
		v   string
	}{{&q.Date, f.Date}, {&q.Kind, f.Kind}, {&q.SessionID, f.SessionID}, {&q.Fingerprint, f.Fingerprint}, {&q.Persona, f.Persona}, {&q.Cursor, f.Cursor}} {
		if p.v != "" {
			*p.dst = new(p.v)
		}
	}
	if f.Limit > 0 {
		q.Limit = new(int64(f.Limit))
	}
	resp, err := api.GetAPIV1FrictionFindingsWithResponse(ctx, &apiclient.GetAPIV1FrictionFindingsRequestOptions{Query: q})
	if resp == nil {
		return frictionFindingsPage{}, fmt.Errorf("friction findings: %w", err)
	}
	if err := frictionHTTPCheck("friction findings", resp.HTTPResponse, resp.Body, err); err != nil {
		return frictionFindingsPage{}, err
	}
	var page frictionFindingsPage
	if err := json.Unmarshal(resp.Body, &page); err != nil {
		return frictionFindingsPage{}, fmt.Errorf("decoding friction findings: %w", err)
	}
	return page, nil
}

func (b frictionDaemonBackend) Patterns(ctx context.Context, f db.FrictionPatternFilter) (frictionPatternsPage, error) {
	api, err := b.client()
	if err != nil {
		return frictionPatternsPage{}, err
	}
	q := &apiclient.GetAPIV1FrictionPatternsQuery{}
	for _, p := range []struct {
		dst **string
		v   string
	}{{&q.Kind, f.Kind}, {&q.LinkState, f.LinkState}, {&q.Since, f.Since}, {&q.Persona, f.Persona}, {&q.Cursor, f.Cursor}} {
		if p.v != "" {
			*p.dst = new(p.v)
		}
	}
	if f.Limit > 0 {
		q.Limit = new(int64(f.Limit))
	}
	resp, err := api.GetAPIV1FrictionPatternsWithResponse(ctx, &apiclient.GetAPIV1FrictionPatternsRequestOptions{Query: q})
	if resp == nil {
		return frictionPatternsPage{}, fmt.Errorf("friction patterns: %w", err)
	}
	if err := frictionHTTPCheck("friction patterns", resp.HTTPResponse, resp.Body, err); err != nil {
		return frictionPatternsPage{}, err
	}
	var page frictionPatternsPage
	if err := json.Unmarshal(resp.Body, &page); err != nil {
		return frictionPatternsPage{}, fmt.Errorf("decoding friction patterns: %w", err)
	}
	return page, nil
}
