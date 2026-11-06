package server

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/friction/review"
	"go.kenn.io/agentsview/internal/stringutil"
	"go.kenn.io/agentsview/internal/timeutil"
)

func (s *Server) registerFrictionRoutes() {
	group := huma.NewGroup(s.api, "/api/v1")
	configureRouteGroup(group, "Friction")

	s.get(group, "/friction/digests", "List friction digests", s.humaListFrictionDigests)
	s.get(group, "/friction/digests/{date}", "Get friction digest", s.humaGetFrictionDigest)
	s.raw(group, http.MethodGet, "/friction/digests/{date}/md",
		"Get friction digest as Markdown", "text/markdown", s.humaFrictionDigestMarkdown)
	s.get(group, "/friction/findings", "List friction findings", s.humaListFrictionFindings)
	s.get(group, "/friction/patterns", "List friction patterns", s.humaListFrictionPatterns)
	s.postLong(group, "/friction/run", "Run friction review", s.humaRunFrictionReview)
	s.registerFrictionIssueRoutes(group)
}

// --- inputs ---

type frictionDigestsInput struct {
	From string `query:"from" doc:"First digest date (YYYY-MM-DD)"`
	To   string `query:"to" doc:"Last digest date (YYYY-MM-DD)"`
}

type frictionDatePathInput struct {
	Date string `path:"date" required:"true" doc:"Digest date (YYYY-MM-DD)"`
}

type frictionFindingsInput struct {
	Date        string `query:"date" doc:"Only findings from sessions in this digest (YYYY-MM-DD)"`
	Kind        string `query:"kind" doc:"correction, error, workaround, deferral, pattern, frustration or interruption"`
	SessionID   string `query:"session_id" doc:"Session ID"`
	Fingerprint string `query:"fingerprint" doc:"Pattern fingerprint (fl1:<sha256>)"`
	Limit       int    `query:"limit" doc:"Page size, 1-1000 (default 100)"`
	Cursor      string `query:"cursor" doc:"Cursor from next_cursor"`
}

type frictionPatternsInput struct {
	Kind   string `query:"kind" doc:"correction, error, workaround, deferral, pattern, frustration or interruption"`
	Since  string `query:"since" doc:"Only patterns last seen on or after this date (YYYY-MM-DD)"`
	Limit  int    `query:"limit" doc:"Page size, 1-1000 (default 100)"`
	Cursor string `query:"cursor" doc:"Cursor from next_cursor"`
}

type frictionRunRequest struct {
	Date    string `json:"date,omitempty" doc:"Digest date (YYYY-MM-DD); omit to catch up every missing complete date"`
	Rebuild bool   `json:"rebuild,omitempty" doc:"Rebuild an existing digest, keeping its membership"`
	DryRun  bool   `json:"dry_run,omitempty" doc:"Compute and render without writing"`
}

type frictionRunInput struct {
	Body frictionRunRequest
}

// --- responses ---

type frictionDigestsResponse struct {
	Digests []frictionDigestItem `json:"digests"`
}

type frictionDigestItem struct {
	Date            string    `json:"date"`
	Timezone        string    `json:"timezone"`
	RulesVersion    string    `json:"rules_version"`
	BuiltAt         time.Time `json:"built_at"`
	Revision        int       `json:"revision"`
	SessionsScanned int       `json:"sessions_scanned"`
}

type frictionDigestResponse struct {
	Date            string            `json:"date"`
	Timezone        string            `json:"timezone"`
	RulesVersion    string            `json:"rules_version"`
	BuiltAt         time.Time         `json:"built_at"`
	Revision        int               `json:"revision"`
	SessionsScanned int               `json:"sessions_scanned"`
	MarkdownSHA256  string            `json:"markdown_sha256"`
	WebURL          string            `json:"web_url,omitempty" doc:"Friction Log page for this digest when public_url is set"`
	Summary         frictionSummary   `json:"summary"`
	Signals         []frictionSignal  `json:"signals"`
	P0Alerts        []frictionP0Alert `json:"p0_alerts"`
	// Patterns are the recurrence rows for this digest's findings.
	Patterns []frictionPatternItem `json:"patterns"`
	// Sessions describes each archived session the digest names, keyed by
	// session ID, so readers can show a title instead of an opaque ID.
	// Sessions no longer in the archive are absent.
	Sessions map[string]frictionSession `json:"sessions"`
}

// frictionSession is the part of a session a digest reader needs to
// recognize it. FirstMessage is cut to frictionSessionPreviewBytes.
type frictionSession struct {
	Agent        string `json:"agent"`
	Project      string `json:"project"`
	DisplayName  string `json:"display_name,omitempty"`
	FirstMessage string `json:"first_message,omitempty"`
}

const frictionSessionPreviewBytes = 300

// frictionSummary mirrors the stored summary JSON (friction.RenderSummaryJSON).
type frictionSummary struct {
	Date            string              `json:"date"`
	SchemaVersion   int                 `json:"schema_version"`
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

type frictionSignal struct {
	Kind           string     `json:"kind"`
	Detector       string     `json:"detector"`
	SubjectID      string     `json:"subject_id"`
	SubjectKind    string     `json:"subject_kind"`
	Title          string     `json:"title"`
	Fingerprint    string     `json:"fingerprint"`
	Text           string     `json:"text"`
	ToolName       string     `json:"tool_name"`
	Label          string     `json:"label"`
	Evidence       string     `json:"evidence"`
	MessageOrdinal *int       `json:"message_ordinal"`
	CallIndex      *int       `json:"call_index"`
	OccurredAt     *time.Time `json:"occurred_at"`
	Seat           string     `json:"seat"`
	Agent          string     `json:"agent"`
	Machine        string     `json:"machine"`
	Persona        string     `json:"persona"`
	Channel        string     `json:"channel"`
	SessionURL     string     `json:"session_url"`
}

type frictionP0Alert struct {
	Tool       string   `json:"tool"`
	SubjectIDs []string `json:"subject_ids"`
}

type frictionFindingsResponse struct {
	Findings   []frictionFindingItem `json:"findings"`
	NextCursor string                `json:"next_cursor"`
}

type frictionFindingItem struct {
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

type frictionPatternsResponse struct {
	Patterns   []frictionPatternItem `json:"patterns"`
	NextCursor string                `json:"next_cursor"`
}

type frictionPatternItem struct {
	Fingerprint     string `json:"fingerprint"`
	Kind            string `json:"kind"`
	Title           string `json:"title"`
	FirstSeenDate   string `json:"first_seen_date"`
	LastSeenDate    string `json:"last_seen_date"`
	OccurrenceCount int    `json:"occurrence_count"`
	SessionCount    int    `json:"session_count"`
	LastSubjectID   string `json:"last_subject_id"`
	LastOrdinal     *int   `json:"last_ordinal"`
	// Issue is the Kata issue filed or linked for this pattern.
	Issue *frictionIssueItem `json:"issue,omitempty"`
}

type frictionRunResponse struct {
	Reports []frictionRunReport `json:"reports"`
}

type frictionRunReport struct {
	Date    string          `json:"date"`
	Written bool            `json:"written"`
	Summary frictionSummary `json:"summary"`
}

// --- validation and helpers ---

var frictionKinds = []string{"correction", "error", "workaround", "deferral", "pattern", "frustration", "interruption"}

func validateFrictionEnum(field, value string, allowed []string) error {
	if value == "" || slices.Contains(allowed, value) {
		return nil
	}
	return apiError(http.StatusBadRequest,
		fmt.Sprintf("invalid %s %q: use one of %s", field, value, strings.Join(allowed, ", ")))
}

func validateFrictionLimit(n int) error {
	if n < 0 || n > db.MaxFrictionListLimit {
		return apiError(http.StatusBadRequest,
			fmt.Sprintf("limit must be between 1 and %d", db.MaxFrictionListLimit))
	}
	return nil
}

func frictionStoreError(logPrefix string, err error) error {
	if handled := handleHumaReadOnly(err); handled != nil {
		return handled
	}
	if errors.Is(err, db.ErrInvalidCursor) {
		return apiError(http.StatusBadRequest, "invalid cursor")
	}
	return internalError(logPrefix, err)
}

// frictionReader returns the store's Friction Log reads. Mirrors that do not
// store friction tables answer 501, like other remote-only gaps.
func (s *Server) frictionReader() (db.FrictionReader, error) {
	fr, ok := s.db.(db.FrictionReader)
	if !ok {
		return nil, apiError(http.StatusNotImplemented, "the Friction Log is not available from this backend")
	}
	return fr, nil
}

// frictionPublicURL is the browser address that links are built on.
// public_url is an origin (config rejects a path), so --base-path is added.
func (s *Server) frictionPublicURL() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	base := strings.TrimRight(strings.TrimSpace(s.cfg.PublicURL), "/")
	if base == "" {
		return ""
	}
	return base + s.basePath
}

func (s *Server) frictionRequireAuth() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.RequireAuth
}

// frictionRunAllowed reports whether this caller may start a friction build:
// an authenticated server accepts any caller that reached the route, and an
// unauthenticated one accepts only direct localhost requests.
func (s *Server) frictionRunAllowed(ctx context.Context) bool {
	return s.frictionRequireAuth() || isLocalhostContext(ctx)
}

func frictionSignalItems(sigs []friction.Signal, base string) []frictionSignal {
	out := make([]frictionSignal, 0, len(sigs))
	for _, sig := range sigs {
		item := frictionSignal{
			Kind: string(sig.Kind), Detector: sig.Detector, SubjectID: sig.SubjectID,
			SubjectKind: cmp.Or(sig.SubjectKind, friction.SubjectSession),
			Title:       sig.Title(), Fingerprint: sig.Fingerprint(), Text: sig.Text,
			ToolName: sig.ToolName, Label: sig.Label, Evidence: sig.Evidence,
			MessageOrdinal: sig.Ordinal, CallIndex: sig.CallIndex,
			Seat: sig.Dims.Seat, Agent: sig.Dims.Agent, Machine: sig.Dims.Machine,
			Persona: sig.Dims.Persona, Channel: sig.Dims.Channel,
		}
		if item.SubjectKind == friction.SubjectSession {
			// Diagnostic identities are not sessions; they get no link.
			item.SessionURL = friction.SessionURL(base, sig.SubjectID, sig.Ordinal)
		}
		if !sig.OccurredAt.IsZero() {
			item.OccurredAt = new(sig.OccurredAt.UTC())
		}
		out = append(out, item)
	}
	return out
}

// frictionSessions looks up every session a digest's findings and P0 alerts
// name. Diagnostic subjects are producer identities, not sessions.
func (s *Server) frictionSessions(
	ctx context.Context, sigs []friction.Signal, alerts map[string][]string,
) (map[string]frictionSession, error) {
	ids := make(map[string]struct{})
	for _, sig := range sigs {
		if cmp.Or(sig.SubjectKind, friction.SubjectSession) == friction.SubjectSession {
			ids[sig.SubjectID] = struct{}{}
		}
	}
	for _, subjects := range alerts {
		for _, id := range subjects {
			ids[id] = struct{}{}
		}
	}
	out := make(map[string]frictionSession, len(ids))
	for _, id := range slices.Sorted(maps.Keys(ids)) {
		sess, err := s.db.GetSession(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("session %s: %w", id, err)
		}
		if sess == nil {
			continue
		}
		info := frictionSession{Agent: sess.Agent, Project: sess.Project}
		if sess.DisplayName != nil {
			info.DisplayName = *sess.DisplayName
		}
		if sess.FirstMessage != nil {
			info.FirstMessage = stringutil.SafeTruncate(*sess.FirstMessage, frictionSessionPreviewBytes)
		}
		out[id] = info
	}
	return out, nil
}

func frictionP0AlertItems(alerts map[string][]string) []frictionP0Alert {
	out := make([]frictionP0Alert, 0, len(alerts))
	for _, tool := range slices.Sorted(maps.Keys(alerts)) {
		ids := slices.Clone(alerts[tool])
		slices.Sort(ids)
		out = append(out, frictionP0Alert{Tool: tool, SubjectIDs: ids})
	}
	return out
}

func decodeFrictionSummary(raw []byte) (frictionSummary, error) {
	var out frictionSummary
	if err := json.Unmarshal(raw, &out); err != nil {
		return frictionSummary{}, fmt.Errorf("decoding friction summary: %w", err)
	}
	return out, nil
}

func (s *Server) frictionDigestByDate(ctx context.Context, date string) (*db.FrictionDigest, error) {
	if !timeutil.IsValidDate(date) {
		return nil, apiError(http.StatusBadRequest, "invalid date format: use YYYY-MM-DD")
	}
	fr, err := s.frictionReader()
	if err != nil {
		return nil, err
	}
	d, err := fr.GetFrictionDigest(ctx, date)
	if err != nil {
		return nil, frictionStoreError("get friction digest", err)
	}
	if d == nil {
		return nil, apiError(http.StatusNotFound, "friction digest not found")
	}
	return d, nil
}

func frictionPatternItems(rows []db.FrictionPattern) []frictionPatternItem {
	out := make([]frictionPatternItem, 0, len(rows))
	for _, p := range rows {
		out = append(out, frictionPatternItem{
			Fingerprint: p.Fingerprint, Kind: p.Kind, Title: p.Title,
			FirstSeenDate: p.FirstSeenDate, LastSeenDate: p.LastSeenDate,
			OccurrenceCount: p.OccurrenceCount, SessionCount: p.SessionCount,
			LastSubjectID: p.LastSubjectID, LastOrdinal: p.LastOrdinal,
			Issue: frictionIssueItemFrom(p.Issue),
		})
	}
	return out
}

// frictionDigestPatterns returns the recurrence rows for the fingerprints in
// a digest's signals, so readers need not page through every pattern.
func frictionDigestPatterns(
	ctx context.Context, fr db.FrictionReader, sigs []friction.Signal,
) ([]db.FrictionPattern, error) {
	seen := make(map[string]bool, len(sigs))
	fingerprints := make([]string, 0, len(sigs))
	for _, sig := range sigs {
		if fp := sig.Fingerprint(); !seen[fp] {
			seen[fp] = true
			fingerprints = append(fingerprints, fp)
		}
	}
	out := []db.FrictionPattern{}
	for chunk := range slices.Chunk(fingerprints, db.MaxFrictionFingerprints) {
		rows, _, err := fr.ListFrictionPatterns(ctx, db.FrictionPatternFilter{
			Fingerprints: chunk, Limit: len(chunk),
		})
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	return out, nil
}

// --- handlers ---

func (s *Server) humaListFrictionDigests(
	ctx context.Context, in *frictionDigestsInput,
) (*jsonOutput[frictionDigestsResponse], error) {
	if err := validateDateFilterValues("", in.From, in.To, ""); err != nil {
		return nil, err
	}
	fr, err := s.frictionReader()
	if err != nil {
		return nil, err
	}
	rows, err := fr.ListFrictionDigests(ctx, in.From, in.To)
	if err != nil {
		return nil, frictionStoreError("list friction digests", err)
	}
	out := make([]frictionDigestItem, 0, len(rows))
	for _, d := range rows {
		out = append(out, frictionDigestItem{
			Date: d.Date, Timezone: d.Timezone, RulesVersion: d.RulesVersion,
			BuiltAt: d.BuiltAt.UTC(), Revision: d.Revision, SessionsScanned: d.SessionsScanned,
		})
	}
	return &jsonOutput[frictionDigestsResponse]{Body: frictionDigestsResponse{Digests: out}}, nil
}

func (s *Server) humaGetFrictionDigest(
	ctx context.Context, in *frictionDatePathInput,
) (*jsonOutput[frictionDigestResponse], error) {
	d, err := s.frictionDigestByDate(ctx, in.Date)
	if err != nil {
		return nil, err
	}
	summary, err := decodeFrictionSummary(d.SummaryJSON)
	if err != nil {
		return nil, internalError("friction digest "+d.Date, err)
	}
	snap, err := review.DecodeSnapshot(d.SnapshotJSON)
	if err != nil {
		return nil, internalError("friction digest "+d.Date, err)
	}
	fr, err := s.frictionReader()
	if err != nil {
		return nil, err
	}
	patterns, err := frictionDigestPatterns(ctx, fr, snap.Signals)
	if err != nil {
		return nil, frictionStoreError("friction digest patterns", err)
	}
	resp := frictionDigestResponse{
		Date: d.Date, Timezone: d.Timezone, RulesVersion: d.RulesVersion,
		BuiltAt: d.BuiltAt.UTC(), Revision: d.Revision, SessionsScanned: d.SessionsScanned,
		MarkdownSHA256: d.MarkdownSHA256, Summary: summary,
		Signals:  frictionSignalItems(snap.Signals, s.frictionPublicURL()),
		P0Alerts: frictionP0AlertItems(snap.P0Alerts),
		Patterns: frictionPatternItems(patterns),
	}
	resp.Sessions, err = s.frictionSessions(ctx, snap.Signals, snap.P0Alerts)
	if err != nil {
		return nil, frictionStoreError("friction digest sessions", err)
	}
	if base := s.frictionPublicURL(); base != "" {
		resp.WebURL = friction.DigestURL(base, d.Date)
	}
	return &jsonOutput[frictionDigestResponse]{Body: resp}, nil
}

func (s *Server) humaFrictionDigestMarkdown(
	ctx context.Context, in *frictionDatePathInput,
) (*bytesOutput, error) {
	d, err := s.frictionDigestByDate(ctx, in.Date)
	if err != nil {
		return nil, err
	}
	return &bytesOutput{
		ContentType:        "text/markdown; charset=utf-8",
		ContentDisposition: fmt.Sprintf(`inline; filename="friction-log-%s.md"`, d.Date),
		NoSniff:            "nosniff",
		Body:               d.Markdown,
	}, nil
}

func (s *Server) humaListFrictionFindings(
	ctx context.Context, in *frictionFindingsInput,
) (*jsonOutput[frictionFindingsResponse], error) {
	if err := validateDateFilterValues(in.Date, "", "", ""); err != nil {
		return nil, err
	}
	if err := validateFrictionEnum("kind", in.Kind, frictionKinds); err != nil {
		return nil, err
	}
	if err := validateFrictionLimit(in.Limit); err != nil {
		return nil, err
	}
	fr, err := s.frictionReader()
	if err != nil {
		return nil, err
	}
	rows, next, err := fr.ListFrictionFindings(ctx, db.FrictionFindingFilter{
		Date: in.Date, Kind: in.Kind, SessionID: in.SessionID,
		Fingerprint: in.Fingerprint, Limit: in.Limit, Cursor: in.Cursor,
	})
	if err != nil {
		return nil, frictionStoreError("list friction findings", err)
	}
	out := make([]frictionFindingItem, 0, len(rows))
	for _, r := range rows {
		out = append(out, frictionFindingItem{
			SessionID: r.SessionID, Kind: r.Kind, Detector: r.Detector,
			MessageOrdinal: r.MessageOrdinal, CallIndex: r.CallIndex,
			ToolName: r.ToolName, Label: r.Label, Text: r.Text, Evidence: r.Evidence,
			Title: r.Title, Fingerprint: r.Fingerprint, OccurredAt: r.OccurredAt,
			Seq: r.Seq, RulesVersion: r.RulesVersion,
		})
	}
	return &jsonOutput[frictionFindingsResponse]{
		Body: frictionFindingsResponse{Findings: out, NextCursor: next},
	}, nil
}

func (s *Server) humaListFrictionPatterns(
	ctx context.Context, in *frictionPatternsInput,
) (*jsonOutput[frictionPatternsResponse], error) {
	if err := validateDateFilterValues(in.Since, "", "", ""); err != nil {
		return nil, err
	}
	if err := validateFrictionEnum("kind", in.Kind, frictionKinds); err != nil {
		return nil, err
	}
	if err := validateFrictionLimit(in.Limit); err != nil {
		return nil, err
	}
	fr, err := s.frictionReader()
	if err != nil {
		return nil, err
	}
	rows, next, err := fr.ListFrictionPatterns(ctx, db.FrictionPatternFilter{
		Kind: in.Kind, Since: in.Since,
		Limit: in.Limit, Cursor: in.Cursor,
	})
	if err != nil {
		return nil, frictionStoreError("list friction patterns", err)
	}
	return &jsonOutput[frictionPatternsResponse]{
		Body: frictionPatternsResponse{Patterns: frictionPatternItems(rows), NextCursor: next},
	}, nil
}

func (s *Server) humaRunFrictionReview(
	ctx context.Context, in *frictionRunInput,
) (*jsonOutput[frictionRunResponse], error) {
	if !s.frictionRunAllowed(ctx) {
		return nil, apiError(http.StatusForbidden,
			"friction review runs require auth or a localhost request")
	}
	runner := s.frictionRunner
	if runner == nil {
		return nil, apiError(http.StatusServiceUnavailable, "friction review is not enabled")
	}
	req := review.RunRequest{Date: in.Body.Date, Rebuild: in.Body.Rebuild, DryRun: in.Body.DryRun}
	out := []frictionRunReport{}
	var decodeErr error
	err := runner.Run(ctx, req, s.frictionExclusive, func(rep review.Report) {
		summary, err := decodeFrictionSummary(friction.RenderSummaryJSON(rep.Snapshot.DigestSnapshot))
		if err != nil {
			decodeErr = errors.Join(decodeErr, err)
			return
		}
		out = append(out, frictionRunReport{Date: rep.Date, Written: rep.Written, Summary: summary})
	})
	if err != nil && errors.Is(err, review.ErrInvalidRunDate) {
		return nil, apiError(http.StatusBadRequest, err.Error())
	}
	if handled := handleHumaContextError(err); handled != nil {
		return nil, handled
	}
	if handled := handleHumaReadOnly(err); handled != nil {
		return nil, handled
	}
	if err = errors.Join(err, decodeErr); err != nil {
		return nil, internalError("friction review", err)
	}
	return &jsonOutput[frictionRunResponse]{Body: frictionRunResponse{Reports: out}}, nil
}
