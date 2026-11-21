// ABOUTME: The six read-only MCP tools, implemented as thin adapters
// ABOUTME: over service.SessionService (the same seam the CLI and HTTP
// ABOUTME: handlers use).
package mcp

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

// toolset holds the dependencies shared by every tool handler. now is
// injectable so tests can control the self-reference exclusion window.
type toolset struct {
	svc     service.SessionService
	now     func() time.Time
	version string
}

func (t *toolset) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

func strval(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// sessionActivity returns a session's most-recent activity timestamp,
// mirroring the DB's canonical activity expression: prefer ended_at, then
// started_at (each only when non-empty), then created_at. created_at is
// always set, so a freshly created/synced session with no parsed start/end
// is still treated as active rather than slipping past the self-reference
// guard.
func sessionActivity(s db.Session) string {
	if ts := strval(s.EndedAt); ts != "" {
		return ts
	}
	if ts := strval(s.StartedAt); ts != "" {
		return ts
	}
	return s.CreatedAt
}

// isSystemMessage reports whether a message is system content that
// get_messages and the overview tail must never surface: the persisted
// is_system flag, or a legacy system prefix on a user message (the prefix
// rule catches older sessions parsed before is_system was backfilled).
func isSystemMessage(m db.Message) bool {
	return m.IsSystem || db.IsSystemPrefixed(m.Content, m.Role)
}

// lookupActivity returns a session's activity timestamp (ended_at, then
// started_at, then created_at) via the service, cached per call so each
// session is fetched at most once. ok is false when the lookup fails, so
// callers can pick their own fallback.
func (t *toolset) lookupActivity(
	ctx context.Context, id string, cache map[string]string,
) (string, bool) {
	if a, ok := cache[id]; ok {
		return a, true
	}
	if d, err := t.svc.Get(ctx, id); err == nil && d != nil {
		a := sessionActivity(d.Session)
		cache[id] = a
		return a, true
	}
	return "", false
}

// resolveSession looks up a session by its full stored ID or by a raw
// UUID that matches exactly one stored ID. Missing and ambiguous IDs are
// errors.
func (t *toolset) resolveSession(
	ctx context.Context, id string,
) (*service.SessionDetail, error) {
	detail, err := t.svc.Get(ctx, id)
	if err != nil || detail != nil {
		return detail, err
	}
	if strings.ContainsAny(id, ":~") {
		return nil, fmt.Errorf("%w: %s", service.ErrSessionNotFound, id)
	}
	ids, err := t.svc.FindSessionIDsByRawSuffix(ctx, id, 5)
	if err != nil {
		return nil, err
	}
	if len(ids) > 1 {
		return nil, fmt.Errorf(
			"ambiguous session UUID %q: up to five matches %q; use a full stored session ID", id, ids,
		)
	}
	if len(ids) == 1 {
		detail, err = t.svc.Get(ctx, ids[0])
		if err != nil {
			return nil, err
		}
	}
	if detail == nil {
		return nil, fmt.Errorf("%w: %s", service.ErrSessionNotFound, id)
	}
	return detail, nil
}

func (t *toolset) resolveOptionalSessionID(ctx context.Context, id string) (string, error) {
	if id == "" {
		return id, nil
	}
	detail, err := t.resolveSession(ctx, id)
	if errors.Is(err, service.ErrSessionNotFound) || errors.Is(err, service.ErrRawLookupUnsupported) {
		return id, nil
	}
	if err != nil {
		return "", err
	}
	return detail.ID, nil
}

// --- search_sessions ---

type searchSessionsIn struct {
	DateFrom      string `json:"date_from,omitempty" jsonschema:"Only sessions on or after this date (YYYY-MM-DD)."`
	DateTo        string `json:"date_to,omitempty" jsonschema:"Only sessions on or before this date (YYYY-MM-DD)."`
	Query         string `json:"query,omitempty" jsonschema:"Search terms across all agent sessions. Required unless session_id is set. Every term must appear (AND), not an exact phrase; wrap the whole query in double quotes for an exact phrase, e.g. \"build failed\". Punctuation in a term (hyphens, colons) is handled safely."`
	SessionID     string `json:"session_id,omitempty" jsonschema:"Look up one session by its raw UUID or full stored session ID. Returns one metadata row, includes active sessions, and takes precedence over the other search arguments. Missing IDs and ambiguous raw UUIDs return errors."`
	Project       string `json:"project,omitempty" jsonschema:"Restrict to one project (repo/directory name)."`
	Sort          string `json:"sort,omitempty" jsonschema:"relevance (default) or recency."`
	Limit         int    `json:"limit,omitempty" jsonschema:"Max results, default 10, max 30."`
	Cursor        int    `json:"cursor,omitempty" jsonschema:"Pagination cursor from a previous next_cursor."`
	IncludeActive bool   `json:"include_active,omitempty" jsonschema:"Include sessions active in the last 10 minutes. Default false: the conversation you are in right now is also recorded, so without this exclusion you would find yourself."`
}

type sessionHit struct {
	WebURL       string `json:"web_url,omitempty" jsonschema:"Browser URL for this session; use this URL when linking to it."`
	SessionID    string `json:"session_id"`
	Project      string `json:"project,omitempty"`
	Agent        string `json:"agent"`
	Name         string `json:"name"`
	EndedAt      string `json:"ended_at"`
	Snippet      string `json:"snippet"`
	MatchOrdinal int    `json:"match_ordinal"`
}

type searchSessionsOut struct {
	Results        []sessionHit                `json:"results"`
	NextCursor     *int                        `json:"next_cursor,omitempty"`
	ExcludedActive int                         `json:"excluded_active,omitempty"`
	Hints          []service.ContentSearchHint `json:"hints,omitempty"`
}

func (t *toolset) searchSessions(
	ctx context.Context, _ *mcp.CallToolRequest, in searchSessionsIn,
) (*mcp.CallToolResult, searchSessionsOut, error) {
	if in.SessionID != "" {
		detail, err := t.resolveSession(ctx, in.SessionID)
		if err != nil {
			return nil, searchSessionsOut{}, err
		}
		row := toSessionRow(detail.Session)
		ended := row.EndedAt
		if ended == "" {
			ended = row.StartedAt
		}
		return nil, searchSessionsOut{Results: []sessionHit{{
			SessionID: row.SessionID,
			WebURL:    row.WebURL,
			Project:   row.Project,
			Agent:     row.Agent,
			Name:      row.Name,
			EndedAt:   ended,
		}}}, nil
	}
	res, err := t.svc.Search(ctx, service.SearchRequest{
		DateFrom: in.DateFrom,
		DateTo:   in.DateTo,
		Query:    in.Query,
		Project:  in.Project,
		Sort:     in.Sort,
		Cursor:   in.Cursor,
		Limit:    clampLimit(in.Limit, defaultSearchLimit, maxSearchLimit),
	})
	if err != nil {
		return nil, searchSessionsOut{}, err
	}
	now := t.clock()
	activity := make(map[string]string)
	out := searchSessionsOut{Results: make([]sessionHit, 0, len(res.Results))}
	for _, r := range res.Results {
		if !in.IncludeActive {
			act := r.SessionEndedAt
			if act == "" {
				// Search results COALESCE only ended_at/started_at; a current,
				// timestampless session has an empty SessionEndedAt, so fall
				// back to created_at via a lookup like search_content does.
				if a, ok := t.lookupActivity(ctx, r.SessionID, activity); ok {
					act = a
				}
			}
			if isActiveSince(act, now) {
				out.ExcludedActive++
				continue
			}
		}
		name, _ := truncate(r.Name, nameMaxChars)
		out.Results = append(out.Results, sessionHit{
			SessionID:    r.SessionID,
			WebURL:       r.WebURL,
			Project:      r.Project,
			Agent:        r.Agent,
			Name:         name,
			EndedAt:      r.SessionEndedAt,
			Snippet:      r.Snippet,
			MatchOrdinal: r.Ordinal,
		})
	}
	if res.NextCursor > 0 && len(res.Results) > 0 {
		nc := res.NextCursor
		out.NextCursor = &nc
	}
	if len(out.Results) == 0 && out.ExcludedActive > 0 {
		message := fmt.Sprintf("Sessions active in the last %g minutes hidden from this page: %d. "+
			"Retry this request with include_active: true to see them; they may include your current conversation.",
			activeExclusionWindow.Minutes(), out.ExcludedActive)
		if out.NextCursor != nil {
			message += " To see later matches without including active sessions, pass next_cursor as cursor."
		}
		out.Hints = []service.ContentSearchHint{{
			Code:      "excluded_active",
			Message:   message,
			RetryWith: map[string]any{"include_active": true},
		}}
	}
	return nil, out, nil
}

// --- query_recall ---

type queryRecallIn struct {
	Query           string `json:"query" jsonschema:"Natural-language query over distilled recall entries."`
	Mode            string `json:"mode,omitempty" jsonschema:"Retrieval mode: lexical (default), vector, or hybrid."`
	Project         string `json:"project,omitempty" jsonschema:"Filter by project name."`
	CWD             string `json:"cwd,omitempty" jsonschema:"Filter by working directory."`
	GitBranch       string `json:"git_branch,omitempty" jsonschema:"Filter by git branch."`
	Agent           string `json:"agent,omitempty" jsonschema:"Filter by agent."`
	Type            string `json:"type,omitempty" jsonschema:"Filter by recall entry type."`
	Scope           string `json:"scope,omitempty" jsonschema:"Filter by recall scope."`
	ExtractorMethod string `json:"extractor_method,omitempty" jsonschema:"Filter by extractor method."`
	TrustedOnly     bool   `json:"trusted_only,omitempty" jsonschema:"Return only human-reviewed, transferable entries with verified provenance."`
	Limit           int    `json:"limit,omitempty" jsonschema:"Max results, default 10, max 30."`
}

type queryRecallOut struct {
	Mode        string            `json:"mode"`
	QueryID     string            `json:"query_id,omitempty"`
	Entries     []db.RecallResult `json:"entries"`
	TrustedOnly bool              `json:"trusted_only"`
}

func (t *toolset) queryRecall(
	ctx context.Context, _ *mcp.CallToolRequest, in queryRecallIn,
) (*mcp.CallToolResult, queryRecallOut, error) {
	result, err := t.svc.QueryRecallEntries(ctx, service.RecallQuery{
		Query: in.Query, Mode: in.Mode,
		Project: in.Project, CWD: in.CWD, GitBranch: in.GitBranch,
		Agent: in.Agent, Type: in.Type, Scope: in.Scope,
		ExtractorMethod: in.ExtractorMethod, TrustedOnly: in.TrustedOnly,
		Limit:         clampLimit(in.Limit, defaultSearchLimit, maxSearchLimit),
		SkipRecording: true,
	})
	if err != nil {
		return nil, queryRecallOut{}, err
	}
	return nil, queryRecallOut{
		Mode: result.Mode, QueryID: result.QueryID,
		Entries: result.RecallEntries, TrustedOnly: result.TrustedOnly,
	}, nil
}

type listRecallIn struct {
	UpdatedSince    string `json:"updated_since,omitempty" jsonschema:"Include entries updated at or after this RFC3339 timestamp."`
	CreatedSince    string `json:"created_since,omitempty" jsonschema:"Include entries created at or after this RFC3339 timestamp."`
	Project         string `json:"project,omitempty"`
	CWD             string `json:"cwd,omitempty"`
	GitBranch       string `json:"git_branch,omitempty"`
	Agent           string `json:"agent,omitempty"`
	Type            string `json:"type,omitempty"`
	Scope           string `json:"scope,omitempty"`
	Status          string `json:"status,omitempty"`
	ExtractorMethod string `json:"extractor_method,omitempty"`
	SourceSessionID string `json:"source_session_id,omitempty" jsonschema:"Filter by a full stored session ID or a raw UUID that matches one session. An unindexed or trashed ID is used as given."`
	SourceRunID     string `json:"source_run_id,omitempty"`
	TrustedOnly     bool   `json:"trusted_only,omitempty"`
	Cursor          string `json:"cursor,omitempty" jsonschema:"Cursor from a previous next_cursor."`
	Limit           int    `json:"limit,omitempty" jsonschema:"Max results, default 20, max 100."`
}

type listRecallOut struct {
	Entries     []db.RecallEntry `json:"entries"`
	TrustedOnly bool             `json:"trusted_only"`
	NextCursor  string           `json:"next_cursor,omitempty"`
}

func (t *toolset) listRecall(ctx context.Context, _ *mcp.CallToolRequest, in listRecallIn) (*mcp.CallToolResult, listRecallOut, error) {
	id, err := t.resolveOptionalSessionID(ctx, in.SourceSessionID)
	if err != nil {
		return nil, listRecallOut{}, err
	}
	result, err := t.svc.ListRecallEntries(ctx, service.RecallFilter{
		UpdatedSince: in.UpdatedSince, CreatedSince: in.CreatedSince, Project: in.Project,
		CWD: in.CWD, GitBranch: in.GitBranch, Agent: in.Agent, Type: in.Type,
		Scope: in.Scope, Status: in.Status, ExtractorMethod: in.ExtractorMethod,
		SourceSessionID: id, SourceRunID: in.SourceRunID, TrustedOnly: in.TrustedOnly, Cursor: in.Cursor,
		Limit: clampLimit(in.Limit, defaultListLimit, maxListLimit),
	})
	if err != nil {
		return nil, listRecallOut{}, err
	}
	entries := make([]db.RecallEntry, 0, len(result.RecallEntries))
	for _, entry := range result.RecallEntries {
		entries = append(entries, entry.RecallEntry)
	}
	return nil, listRecallOut{Entries: entries, TrustedOnly: result.TrustedOnly, NextCursor: result.NextCursor}, nil
}

// --- list_sessions ---

type listSessionsIn struct {
	Project          string   `json:"project,omitempty" jsonschema:"Filter by project name."`
	Agent            string   `json:"agent,omitempty" jsonschema:"Filter by agent (e.g. claude, codex, gemini, antigravity)."`
	Machine          string   `json:"machine,omitempty" jsonschema:"Exact ID or alias, comma-separated IDs or aliases, or one unique whole display name. IDs and aliases take precedence; unknown or ambiguous selectors return an error."`
	DateFrom         string   `json:"date_from,omitempty" jsonschema:"Only sessions on or after this date (YYYY-MM-DD)."`
	DateTo           string   `json:"date_to,omitempty" jsonschema:"Only sessions on or before this date (YYYY-MM-DD)."`
	ActiveSince      string   `json:"active_since,omitempty" jsonschema:"Only sessions active since this RFC3339 timestamp."`
	IncludeOneShot   *bool    `json:"include_one_shot,omitempty" jsonschema:"Include one-shot sessions (a single user message). Default false."`
	IncludeAutomated *bool    `json:"include_automated,omitempty" jsonschema:"Include automated runs identified by roborev tags or matching prompt patterns. Default false."`
	Labels           []string `json:"labels,omitempty" jsonschema:"Only sessions carrying every one of these exact labels, such as ticket=example."`
	PR               string   `json:"pr,omitempty" jsonschema:"Only sessions linked to a pull request: owner/repo, owner/repo#123, or github.com::owner/repo#123. Includes URLs in user prompts; a mention does not mean work was done on the PR."`
	Limit            int      `json:"limit,omitempty" jsonschema:"Max results, default 20, max 100."`
	Cursor           string   `json:"cursor,omitempty" jsonschema:"Pagination cursor from a previous next_cursor."`
}

type sessionRow struct {
	WebURL           string      `json:"web_url,omitempty" jsonschema:"Browser URL for this session; use this URL when linking to it."`
	SessionID        string      `json:"session_id"`
	Project          string      `json:"project,omitempty"`
	Machine          string      `json:"machine,omitempty"`
	Agent            string      `json:"agent"`
	Name             string      `json:"name"`
	StartedAt        string      `json:"started_at"`
	EndedAt          string      `json:"ended_at"`
	MessageCount     int         `json:"message_count" jsonschema:"Total stored messages for the session across all roles, including system messages. get_messages always drops system messages, so no roles filter makes its returned count match this; reconcile instead via message_count = returned + filtered summed over a full get_messages pagination sweep."`
	UserMessageCount int         `json:"user_message_count" jsonschema:"Stored user-role messages, excluding those flagged as system-injected."`
	OutputTokens     int64       `json:"output_tokens,omitempty"`
	Outcome          string      `json:"outcome,omitempty"`
	HealthGrade      string      `json:"health_grade,omitempty"`
	GitBranch        string      `json:"git_branch,omitempty"`
	ParentSessionID  string      `json:"parent_session_id,omitempty"`
	Labels           []string    `json:"labels,omitempty"`
	PullRequests     []db.PRLink `json:"pull_requests,omitempty" jsonschema:"Pull or merge requests the session's transcript linked to."`
}

type listSessionsOut struct {
	Sessions   []sessionRow `json:"sessions"`
	Total      int          `json:"total"`
	NextCursor string       `json:"next_cursor,omitempty"`
}

func (t *toolset) listSessions(
	ctx context.Context, _ *mcp.CallToolRequest, in listSessionsIn,
) (*mcp.CallToolResult, listSessionsOut, error) {
	res, err := t.svc.List(ctx, service.ListFilter{
		Project:            in.Project,
		Agent:              in.Agent,
		Machine:            in.Machine,
		DateFrom:           in.DateFrom,
		DateTo:             in.DateTo,
		ActiveSince:        in.ActiveSince,
		IncludeOneShot:     in.IncludeOneShot != nil && *in.IncludeOneShot,
		OneShotFilterSet:   in.IncludeOneShot != nil,
		IncludeAutomated:   in.IncludeAutomated != nil && *in.IncludeAutomated,
		AutomatedFilterSet: in.IncludeAutomated != nil,
		Labels:             in.Labels,
		PR:                 in.PR,
		Cursor:             in.Cursor,
		Limit:              clampLimit(in.Limit, defaultListLimit, maxListLimit),
	})
	if err != nil {
		return nil, listSessionsOut{}, err
	}
	out := listSessionsOut{
		Sessions:   make([]sessionRow, 0, len(res.Sessions)),
		Total:      res.Total,
		NextCursor: res.NextCursor,
	}
	for _, s := range res.Sessions {
		out.Sessions = append(out.Sessions, toSessionRow(s))
	}
	return nil, out, nil
}

func toSessionRow(s db.Session) sessionRow {
	name := strval(s.DisplayName)
	if name == "" {
		name = strval(s.FirstMessage)
	}
	name, _ = truncate(name, nameMaxChars)
	return sessionRow{
		SessionID:        s.ID,
		WebURL:           s.WebURL,
		Project:          s.Project,
		Machine:          s.Machine,
		Agent:            s.Agent,
		Name:             name,
		StartedAt:        strval(s.StartedAt),
		EndedAt:          strval(s.EndedAt),
		MessageCount:     s.MessageCount,
		UserMessageCount: s.UserMessageCount,
		OutputTokens:     int64(s.TotalOutputTokens),
		Outcome:          s.Outcome,
		HealthGrade:      strval(s.HealthGrade),
		GitBranch:        s.GitBranch,
		ParentSessionID:  strval(s.ParentSessionID),
		Labels:           s.Labels,
		PullRequests:     s.PRLinks,
	}
}

// --- get_session_overview ---

type sessionOverviewIn struct {
	SessionID string `json:"session_id" jsonschema:"A full stored session ID or a raw UUID that matches one session."`
}

type overviewMessage struct {
	Ordinal   int    `json:"ordinal"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	Truncated bool   `json:"truncated,omitempty"`
}

type sessionOverviewOut struct {
	Session      sessionRow        `json:"session"`
	CWD          string            `json:"cwd,omitempty"`
	IsAutomated  bool              `json:"is_automated"`
	FirstMessage string            `json:"first_message"`
	LastMessages []overviewMessage `json:"last_messages"`
}

func (t *toolset) sessionOverview(
	ctx context.Context, _ *mcp.CallToolRequest, in sessionOverviewIn,
) (*mcp.CallToolResult, sessionOverviewOut, error) {
	detail, err := t.resolveSession(ctx, in.SessionID)
	if err != nil {
		return nil, sessionOverviewOut{}, err
	}
	out := sessionOverviewOut{
		Session:      toSessionRow(detail.Session),
		CWD:          detail.Cwd,
		IsAutomated:  detail.IsAutomated,
		FirstMessage: strval(detail.FirstMessage),
	}
	// Tail of the conversation: how did it end? desc returns
	// newest-first; collect the newest few then restore chronological
	// order for display.
	msgs, err := t.svc.Messages(ctx, detail.ID, service.MessageFilter{
		Direction: "desc",
		Limit:     overviewTailFetch,
	})
	if err != nil {
		return nil, sessionOverviewOut{}, err
	}
	for _, m := range msgs.Messages {
		if isSystemMessage(m) || !roleAllowed(m.Role, nil) {
			continue
		}
		content, cut := truncate(m.Content, overviewMaxChars)
		out.LastMessages = append(out.LastMessages, overviewMessage{
			Ordinal: m.Ordinal, Role: m.Role, Content: content, Truncated: cut,
		})
		if len(out.LastMessages) == overviewLastMessages {
			break
		}
	}
	for i, j := 0, len(out.LastMessages)-1; i < j; i, j = i+1, j-1 {
		out.LastMessages[i], out.LastMessages[j] =
			out.LastMessages[j], out.LastMessages[i]
	}
	return nil, out, nil
}

// --- get_messages ---

type getMessagesIn struct {
	SessionID string `json:"session_id" jsonschema:"A full stored session ID or a raw UUID that matches one session."`
	From      *int   `json:"from,omitempty" jsonschema:"Ordinal to start from (e.g. match_ordinal from search_sessions). Ordinal 0 is a valid anchor (the first message). Mutually exclusive with around."`
	Direction string `json:"direction,omitempty" jsonschema:"asc (default, oldest first) or desc (newest first). Mutually exclusive with around."`
	Limit     int    `json:"limit,omitempty" jsonschema:"Max messages scanned, default 20, max 100. Filtering or the byte budget can return fewer. Around uses before/after instead."`
	// Around switches to a symmetric window centered on an ordinal instead
	// of linear pagination; it is mutually exclusive with From/Direction.
	Around             *int     `json:"around,omitempty" jsonschema:"Center a symmetric window on this ordinal (e.g. match_ordinal from search_content or search_sessions), instead of linear pagination. Mutually exclusive with from and direction."`
	Before             *int     `json:"before,omitempty" jsonschema:"Messages of context before the around anchor, default 5. Requires around; replaces limit on that path."`
	After              *int     `json:"after,omitempty" jsonschema:"Messages of context after the around anchor, default 5. Requires around; replaces limit on that path."`
	Roles              []string `json:"roles,omitempty" jsonschema:"Roles to include, e.g. tool. Default: user and assistant only. System messages are always excluded."`
	MaxCharsPerMessage int      `json:"max_chars_per_message,omitempty" jsonschema:"Truncate each message to this many characters, default 2000, max 20000."`
	ExpectedRevision   string   `json:"expected_revision,omitempty" jsonschema:"Transcript revision cited by search or an earlier read. Returns source_changed when it no longer matches."`
	BodyCursor         string   `json:"body_cursor,omitempty" jsonschema:"Opaque continuation for the remainder of one oversized message. Use before advancing next_from."`
}

type messageOut struct {
	Ordinal    int    `json:"ordinal"`
	Role       string `json:"role"`
	Content    string `json:"content"`
	Timestamp  string `json:"timestamp,omitempty"`
	Model      string `json:"model,omitempty"`
	HasToolUse bool   `json:"has_tool_use,omitempty"`
	FullLength int    `json:"full_length,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
	BodyCursor string `json:"body_cursor,omitempty"`
}

type getMessagesOut struct {
	Messages     []messageOut `json:"messages"`
	Omitted      int          `json:"omitted,omitempty"`
	MessageCount int          `json:"message_count,omitempty" jsonschema:"Session message count when cut."`
	Filtered     int          `json:"filtered,omitempty" jsonschema:"Scanned messages excluded by role/system filter. Across all pages, filtered plus returned messages equals message_count."`
	// NextFrom is set off the last scanned ordinal (not the last visible
	// one), so paging stays reliable even though filtering can make a page
	// return fewer than the limit.
	NextFrom           *int   `json:"next_from,omitempty" jsonschema:"Next page's from anchor. Page until absent; filtering or the byte budget can make pages short."`
	TranscriptRevision string `json:"transcript_revision"`
}

type messageBodyCursor struct {
	Version        int      `json:"v"`
	EvidenceSource string   `json:"s"`
	SessionID      string   `json:"i"`
	Revision       string   `json:"r"`
	Ordinal        int      `json:"o"`
	Offset         int      `json:"p"`
	NextFrom       *int     `json:"n,omitempty"`
	Roles          []string `json:"l,omitempty"`
}

func encodeMessageBodyCursor(cursor messageBodyCursor) string {
	raw, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeMessageBodyCursor(raw string) (messageBodyCursor, error) {
	payload, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return messageBodyCursor{}, errors.New("invalid body_cursor")
	}
	var cursor messageBodyCursor
	if err := json.Unmarshal(payload, &cursor); err != nil || cursor.Version != 2 ||
		cursor.EvidenceSource == "" || cursor.SessionID == "" || cursor.Revision == "" ||
		cursor.Ordinal < 0 || cursor.Offset <= 0 {
		return messageBodyCursor{}, errors.New("invalid body_cursor")
	}
	return cursor, nil
}

// filterAndMapMessage applies the get_messages role/system contract to one
// message -- always drop system content (IsSystem or a legacy system
// content prefix), then require its role to be in roles (nil/empty roles
// falls back to user+assistant via roleAllowed) -- and shapes the survivor
// into a messageOut truncated to maxChars. ok is false when the message was
// filtered, so the caller can count it.
func filterAndMapMessage(m db.Message, roles []string, maxChars int) (messageOut, bool) {
	if isSystemMessage(m) || !roleAllowed(m.Role, roles) {
		return messageOut{}, false
	}
	content, cut := truncate(m.Content, maxChars)
	mo := messageOut{
		Ordinal:    m.Ordinal,
		Role:       m.Role,
		Content:    content,
		Timestamp:  m.Timestamp,
		Model:      m.Model,
		HasToolUse: m.HasToolUse,
		Truncated:  cut,
	}
	if cut {
		mo.FullLength = utf8.RuneCountInString(m.Content)
	}
	return mo, true
}

func (t *toolset) getMessages(
	ctx context.Context, _ *mcp.CallToolRequest, in getMessagesIn,
) (*mcp.CallToolResult, getMessagesOut, error) {
	if in.BodyCursor != "" {
		return t.getMessageBodyContinuation(ctx, in)
	}
	detail, err := t.resolveSession(ctx, in.SessionID)
	if errors.Is(err, service.ErrSessionNotFound) && in.ExpectedRevision != "" {
		return nil, getMessagesOut{}, fmt.Errorf("%w: cited session no longer exists", service.ErrSourceChanged)
	}
	if err != nil {
		return nil, getMessagesOut{}, err
	}
	in.SessionID = detail.ID
	if in.Around != nil {
		return t.getMessagesAround(ctx, in)
	}
	// From is a *int so an explicit ordinal 0 (a valid match_ordinal)
	// anchors at the first message rather than being mistaken for
	// "omitted". A nil From lets the service default: desc to
	// newest-first, asc to oldest-first. Before/After are forwarded
	// unused so the service can reject them with "before/after require
	// around" when set without Around.
	limit := clampLimit(in.Limit, defaultMessageLimit, maxMessageLimit)
	res, err := t.svc.Messages(ctx, in.SessionID, service.MessageFilter{
		From:             in.From,
		Direction:        in.Direction,
		Limit:            limit,
		Before:           in.Before,
		After:            in.After,
		ExpectedRevision: in.ExpectedRevision,
	})
	if err != nil {
		return nil, getMessagesOut{}, err
	}
	maxChars := clampLimit(
		in.MaxCharsPerMessage, defaultMaxCharsPerMessage, maxMaxCharsPerMessage)
	out := getMessagesOut{
		Messages:           make([]messageOut, 0, len(res.Messages)),
		TranscriptRevision: res.TranscriptRevision,
	}
	for _, m := range res.Messages {
		mo, ok := filterAndMapMessage(m, in.Roles, maxChars)
		if !ok {
			out.Filtered++
			continue
		}
		out.Messages = append(out.Messages, mo)
	}
	// A full raw page means more rows may remain. Anchor next_from just past
	// the last scanned ordinal in the scan direction (asc moves up, desc
	// moves down) so the caller can continue; the From anchor is inclusive,
	// so +/-1 avoids re-returning the boundary message.
	if limit > 0 && len(res.Messages) == limit {
		last := res.Messages[len(res.Messages)-1].Ordinal
		next := last + 1
		if in.Direction == "desc" {
			next = last - 1
		}
		if next >= 0 {
			out.NextFrom = &next
		}
	}
	if err := t.fitMessagePage(ctx, &out, res.Messages, res.EvidenceSource, in); err != nil {
		return nil, getMessagesOut{}, err
	}
	return nil, out, nil
}

// getMessagesAround handles the symmetric-window retrieval path (Around
// set). Unlike the linear path, an empty Roles must be translated to the
// MCP default (user, assistant) before it reaches the service: an empty
// service.MessageFilter.Roles means "all roles" there, not "the MCP
// default", and would leak tool/system-role dumps into the window. From and
// Direction are forwarded unused so the service can reject them as mutually
// exclusive with Around. The service's around window always includes the
// anchor row regardless of role, so it is subject to the same
// filterAndMapMessage post-filter as every other message; a suppressed
// anchor is counted in Filtered like any other dropped message. next_from
// is anchored on the last returned ordinal (there is no scan direction in a
// symmetric window), and Limit/Before/After select the window, not the
// linear path's Limit.
func (t *toolset) getMessagesAround(
	ctx context.Context, in getMessagesIn,
) (*mcp.CallToolResult, getMessagesOut, error) {
	roles := in.Roles
	if len(roles) == 0 {
		roles = []string{"user", "assistant"}
	}
	res, err := t.svc.Messages(ctx, in.SessionID, service.MessageFilter{
		From:             in.From,
		Direction:        in.Direction,
		Around:           in.Around,
		Before:           in.Before,
		After:            in.After,
		Roles:            roles,
		ExpectedRevision: in.ExpectedRevision,
	})
	if err != nil {
		return nil, getMessagesOut{}, err
	}
	maxChars := clampLimit(
		in.MaxCharsPerMessage, defaultMaxCharsPerMessage, maxMaxCharsPerMessage)
	out := getMessagesOut{
		Messages:           make([]messageOut, 0, len(res.Messages)),
		TranscriptRevision: res.TranscriptRevision,
	}
	for _, m := range res.Messages {
		mo, ok := filterAndMapMessage(m, roles, maxChars)
		if !ok {
			out.Filtered++
			continue
		}
		out.Messages = append(out.Messages, mo)
	}
	if len(out.Messages) > 0 {
		next := out.Messages[len(out.Messages)-1].Ordinal + 1
		out.NextFrom = &next
	}
	if err := t.fitMessagePage(ctx, &out, res.Messages, res.EvidenceSource, in); err != nil {
		return nil, getMessagesOut{}, err
	}
	return nil, out, nil
}

// fitMessagePage preserves the scan position when the budget cuts a page.
func (t *toolset) fitMessagePage(ctx context.Context, out *getMessagesOut, scanned []db.Message, source string, in getMessagesIn) error {
	items, baseNext, filtered := out.Messages, out.NextFrom, out.Filtered
	attachBodyCursors(out, source, in.SessionID, in.Roles)
	if fitsResponse(out) {
		return nil
	}
	detail, err := t.svc.Get(ctx, in.SessionID)
	if err != nil {
		return err
	}
	if detail == nil {
		return fmt.Errorf("session not found: %s", in.SessionID)
	}
	out.MessageCount = detail.MessageCount
	cut := func(k int) {
		out.Messages, out.Omitted = items[:k], len(items)-k
		out.NextFrom, out.Filtered = baseNext, filtered
		if k > 0 && k < len(items) {
			last := items[k-1].Ordinal
			next := last + 1
			if in.Direction == "desc" {
				next = last - 1
			}
			out.NextFrom, out.Filtered = &next, 0
			for _, m := range scanned {
				if m.Ordinal == last {
					break
				}
				if isSystemMessage(m) || !roleAllowed(m.Role, in.Roles) {
					out.Filtered++
				}
			}
		}
	}
	n := fitPrefix(len(items), func(k int) bool {
		cut(k)
		attachBodyCursors(out, source, in.SessionID, in.Roles)
		return fitsResponse(out)
	})
	if i := anchorBeyond(items, in.Around, n); i >= 0 {
		first := items[0].Ordinal
		out.Omitted, out.Filtered = len(items)-1, 0
		return fitSingle(out, items[i:i+1], &first, source, in)
	}
	if n == 0 {
		cut(1)
		return fitSingle(out, items[:1], out.NextFrom, source, in)
	}
	return nil
}

// anchorBeyond returns the index of the around anchor when the cut page left
// it out, or -1.
func anchorBeyond(items []messageOut, around *int, n int) int {
	if around != nil {
		for i := max(n, 1); i < len(items); i++ {
			if items[i].Ordinal == *around {
				return i
			}
		}
	}
	return -1
}

// fitSingle returns msgs[0] alone, truncating its body if needed, and
// continues from next.
func fitSingle(out *getMessagesOut, msgs []messageOut, next *int, source string, in getMessagesIn) error {
	out.Messages, out.NextFrom = msgs, next
	attachBodyCursors(out, source, in.SessionID, in.Roles)
	if fitsResponse(out) {
		return nil
	}
	roles := in.Roles
	if len(roles) > 0 {
		roles = []string{msgs[0].Role}
	}
	out.Messages, out.NextFrom = msgs[:0], next
	progress := fitOneMessage(out, &msgs[0], messageBodyCursor{EvidenceSource: source, SessionID: in.SessionID, Revision: out.TranscriptRevision, Ordinal: msgs[0].Ordinal, NextFrom: next, Roles: roles})
	out.Messages = msgs
	attachBodyCursors(out, source, in.SessionID, in.Roles)
	if !progress || !fitsResponse(out) {
		return errors.New("message metadata exceeds response budget")
	}
	return nil
}

func fitOneMessage(out *getMessagesOut, item *messageOut, cursor messageBodyCursor) bool {
	hadContent := item.Content != ""
	item.Truncated = true
	if item.FullLength == 0 {
		item.FullLength = utf8.RuneCountInString(item.Content)
	}
	start := cursor.Offset
	cursor.Offset = item.FullLength
	item.BodyCursor = encodeMessageBodyCursor(cursor)
	item.Content = fitText(item.Content, responseBudgetBytes-responseSize(out), func(text string) any { m := *item; m.Content = text; return m })
	cursor.Offset = start + utf8.RuneCountInString(item.Content)
	item.BodyCursor = encodeMessageBodyCursor(cursor)
	if cursor.EvidenceSource == "" || cursor.Revision == "" {
		item.BodyCursor = ""
	}
	return !hadContent || item.Content != ""
}

func attachBodyCursors(
	out *getMessagesOut, evidenceSource, sessionID string, roles []string,
) {
	nextFrom := out.NextFrom
	hasBodyCursor := false
	for i := range out.Messages {
		if !out.Messages[i].Truncated {
			continue
		}
		// A cursor is only usable when both revision-bound read bindings
		// are present: decodeMessageBodyCursor rejects cursors without a
		// transcript revision or evidence source, so emitting one would
		// advertise a continuation that can never run (for example against
		// a backend that does not expose the bindings). Leave pagination
		// in place instead.
		if evidenceSource == "" || out.TranscriptRevision == "" {
			continue
		}
		hasBodyCursor = true
		cursorRoles := roles
		if len(roles) > 0 {
			cursorRoles = []string{out.Messages[i].Role}
		}
		out.Messages[i].BodyCursor = encodeMessageBodyCursor(messageBodyCursor{
			Version: 2, EvidenceSource: evidenceSource, SessionID: sessionID,
			Revision: out.TranscriptRevision, Ordinal: out.Messages[i].Ordinal,
			Offset: utf8.RuneCountInString(out.Messages[i].Content), NextFrom: nextFrom,
			Roles: cursorRoles,
		})
	}
	if hasBodyCursor {
		out.NextFrom = nil
	}
}

func (t *toolset) getMessageBodyContinuation(
	ctx context.Context, in getMessagesIn,
) (*mcp.CallToolResult, getMessagesOut, error) {
	if in.From != nil || in.Around != nil || in.Direction != "" ||
		in.Before != nil || in.After != nil || len(in.Roles) > 0 {
		return nil, getMessagesOut{}, errors.New("body_cursor is mutually exclusive with message navigation and roles")
	}
	cursor, err := decodeMessageBodyCursor(in.BodyCursor)
	if err != nil {
		return nil, getMessagesOut{}, err
	}
	detail, err := t.resolveSession(ctx, in.SessionID)
	if errors.Is(err, service.ErrSessionNotFound) {
		return nil, getMessagesOut{}, fmt.Errorf("%w: cited session no longer exists", service.ErrSourceChanged)
	}
	if err != nil {
		return nil, getMessagesOut{}, err
	}
	in.SessionID = detail.ID
	if cursor.SessionID != in.SessionID ||
		(in.ExpectedRevision != "" && in.ExpectedRevision != cursor.Revision) {
		return nil, getMessagesOut{}, fmt.Errorf("%w: body cursor does not match request", service.ErrSourceChanged)
	}
	zero := 0
	res, err := t.svc.Messages(ctx, in.SessionID, service.MessageFilter{
		Around: &cursor.Ordinal, Before: &zero, After: &zero,
		ExpectedRevision: cursor.Revision, EvidenceSource: cursor.EvidenceSource,
	})
	if err != nil {
		return nil, getMessagesOut{}, err
	}
	if len(res.Messages) != 1 || res.Messages[0].Ordinal != cursor.Ordinal {
		return nil, getMessagesOut{}, fmt.Errorf("%w: cited message no longer exists", service.ErrSourceChanged)
	}
	message := res.Messages[0]
	maxChars := clampLimit(in.MaxCharsPerMessage, defaultMaxCharsPerMessage, maxMaxCharsPerMessage)
	// A body continuation must honor the same role/system visibility
	// contract as the listing path: never surface content that
	// filterAndMapMessage would have filtered, whatever the cursor
	// claims. The issuing roles ride inside the cursor, so an
	// explicit-role listing continues under exactly the roles that made
	// the message visible. The message is still sliced from the raw
	// content below, so the mapped output is intentionally discarded.
	if _, ok := filterAndMapMessage(message, cursor.Roles, maxChars); !ok {
		return nil, getMessagesOut{}, fmt.Errorf("%w: cited message is not available", service.ErrSourceChanged)
	}
	runes := []rune(message.Content)
	if cursor.Offset >= len(runes) {
		return nil, getMessagesOut{}, errors.New("invalid body_cursor offset")
	}
	end := min(cursor.Offset+maxChars, len(runes))
	mo := messageOut{
		Ordinal: message.Ordinal, Role: message.Role, Content: string(runes[cursor.Offset:end]),
		Timestamp: message.Timestamp, Model: message.Model, HasToolUse: message.HasToolUse,
		FullLength: len(runes), Truncated: end < len(runes),
	}
	out := getMessagesOut{Messages: []messageOut{mo}, TranscriptRevision: res.TranscriptRevision}
	if end < len(runes) {
		cursor.Offset = end
		out.Messages[0].BodyCursor = encodeMessageBodyCursor(cursor)
	} else {
		out.NextFrom = cursor.NextFrom
	}
	if !fitsResponse(out) {
		out.NextFrom = nil
		cursor.Offset = end - utf8.RuneCountInString(mo.Content)
		item := out.Messages[0]
		out.Messages = []messageOut{}
		progress := fitOneMessage(&out, &item, cursor)
		out.Messages = []messageOut{item}
		if !progress || !fitsResponse(out) {
			return nil, getMessagesOut{}, errors.New("message metadata exceeds response budget")
		}
	}
	return nil, out, nil
}

// --- search_content ---

type searchContentIn struct {
	Pattern          string   `json:"pattern,omitempty" jsonschema:"Semantic/hybrid query, whitespace-separated literals for terms, substring, or regex over messages, thinking, and tools. Required unless concepts or session_id is set. Fully quoted substring matches exact text. Mutually exclusive with concepts."`
	Concepts         []string `json:"concepts,omitempty" jsonschema:"Two to five concepts with evidence in the same session, possibly across messages. Always uses semantic mode, does not page, and is mutually exclusive with pattern."`
	Mode             string   `json:"mode,omitempty" jsonschema:"substring (default), regex, terms, semantic, or hybrid. Prefer hybrid for contextual questions. Hybrid returns keyword matches with a note when search by meaning is unavailable; semantic requires a ready index."`
	Scope            string   `json:"scope,omitempty" jsonschema:"Substring/regex/terms/semantic/hybrid result scope: top, all (default), or subordinate."`
	Project          string   `json:"project,omitempty" jsonschema:"Restrict to one project."`
	Agent            string   `json:"agent,omitempty" jsonschema:"Restrict to one agent."`
	SessionID        string   `json:"session_id,omitempty" jsonschema:"Full stored session ID or raw UUID matching one session. Without pattern or concepts, list its messages, ignoring mode and include flags and including active, one-shot, and automated sessions."`
	GitBranch        string   `json:"git_branch,omitempty" jsonschema:"Restrict to one exact git branch."`
	CurrentSessionID string   `json:"current_session_id,omitempty" jsonschema:"Exclude this full stored session ID or a raw UUID that matches one session instead of applying the recent-activity heuristic. An unindexed ID is excluded as given."`
	DateFrom         string   `json:"date_from,omitempty" jsonschema:"Only sessions on or after this date (YYYY-MM-DD)."`
	DateTo           string   `json:"date_to,omitempty" jsonschema:"Only sessions on or before this date (YYYY-MM-DD)."`
	Limit            int      `json:"limit,omitempty" jsonschema:"Max matches, default 10, max 50."`
	Cursor           int      `json:"cursor,omitempty" jsonschema:"Pagination cursor from a previous next_cursor; also pass activity_cutoff when the recent-activity heuristic applies."`
	ActivityCutoff   string   `json:"activity_cutoff,omitempty" jsonschema:"RFC3339 cutoff returned by the previous page. Required with a positive cursor when the recent-activity heuristic applies."`
	IncludeActive    bool     `json:"include_active,omitempty" jsonschema:"Include sessions active in the last 10 minutes in pattern and concept searches. Default false to exclude the current conversation."`
	IncludeOneShot   bool     `json:"include_one_shot,omitempty" jsonschema:"Include one-shot sessions in pattern and concept searches. Default false."`
	IncludeAutomated bool     `json:"include_automated,omitempty" jsonschema:"Include automated sessions in pattern and concept searches. Default false."`
	Context          int      `json:"context,omitempty" jsonschema:"Messages of context before/after each match (max 10)."`
}

// contextMessage is a truncated view of a service-level db.Message, used
// for search_content's inline context_before/context_after.
type contextMessage struct {
	Ordinal int    `json:"ordinal"`
	Role    string `json:"role"`
	Content string `json:"content"`
}

func toContextMessages(msgs []db.Message) []contextMessage {
	if len(msgs) == 0 {
		return nil
	}
	out := make([]contextMessage, 0, len(msgs))
	for _, m := range msgs {
		content, _ := truncate(m.Content, contextMessageMaxChars)
		out = append(out, contextMessage{
			Ordinal: m.Ordinal, Role: m.Role, Content: content,
		})
	}
	return out
}

type contentMatch struct {
	WebURL          string   `json:"web_url,omitempty" jsonschema:"Browser URL for this session; use this URL when linking to it."`
	SessionID       string   `json:"session_id"`
	Project         string   `json:"project,omitempty"`
	Agent           string   `json:"agent"`
	Location        string   `json:"location" jsonschema:"Where the match occurred: one of message, thinking, tool_input, or tool_result."`
	Role            string   `json:"role,omitempty"`
	Ordinal         int      `json:"ordinal"`
	Timestamp       string   `json:"timestamp"`
	Snippet         string   `json:"snippet"`
	MatchedWords    int      `json:"matched_words" jsonschema:"Number of retained query words the match holds in ranked word search; unquoted stop words are dropped. Zero for other searches."`
	Score           *float64 `json:"score,omitempty" jsonschema:"Relevance score for semantic/hybrid modes; omitted for substring/regex/fts."`
	OrdinalRange    [2]int   `json:"ordinal_range" jsonschema:"[start, end] ordinals of the conversation unit containing this match; equal to the match ordinal for single-message units."`
	Subordinate     bool     `json:"subordinate,omitzero" jsonschema:"True when this match belongs to a subordinate unit: a sidechain run, or a subagent/fork session."`
	Relationship    string   `json:"relationship,omitempty" jsonschema:"The matched session's relationship to its parent (for example subagent or fork), when it has one."`
	ParentSessionID string   `json:"parent_session_id,omitempty" jsonschema:"The parent session ID, when the matched session has one."`
	Sidechain       bool     `json:"is_sidechain,omitzero" jsonschema:"True when the matched message itself is flagged as a sidechain message."`
	// ContextBefore/ContextAfter are populated when Context > 0: the N
	// messages immediately before/after this match, content truncated to
	// 500 characters.
	ContextBefore      []contextMessage     `json:"context_before,omitempty"`
	ContextAfter       []contextMessage     `json:"context_after,omitempty"`
	TranscriptRevision string               `json:"transcript_revision,omitempty" jsonschema:"Transcript revision observed with this evidence; pass it as expected_revision to get_messages."`
	ConceptEvidence    []db.ConceptEvidence `json:"concept_evidence,omitempty" jsonschema:"With concepts: each concept's strongest match in this session, in request order. score is then the mean of these scores."`
}

type searchContentOut struct {
	Note           string                      `json:"note,omitempty"`
	Hints          []service.ContentSearchHint `json:"hints,omitempty"`
	Matches        []contentMatch              `json:"matches"`
	Omitted        int                         `json:"omitted,omitempty"`
	Total          int                         `json:"total,omitempty"`
	TotalCapped    bool                        `json:"total_capped,omitempty" jsonschema:"True when total is a lower bound."`
	NextCursor     *int                        `json:"next_cursor,omitempty"`
	ActivityCutoff string                      `json:"activity_cutoff,omitempty" jsonschema:"Effective RFC3339 activity cutoff; pass it unchanged with next_cursor as cursor to continue."`
	ExcludedActive int                         `json:"excluded_active,omitempty"`
	// EffectiveMode and EffectiveScope report the defaults the search
	// resolved, which the caller cannot see in its own request.
	EffectiveMode      string                  `json:"effective_mode"`
	EffectiveScope     string                  `json:"effective_scope,omitempty"`
	Exclusions         searchContentExclusions `json:"exclusions"`
	RevisionBound      bool                    `json:"revision_bound" jsonschema:"True when every returned match was captured with a transcript revision and can be opened with a revision-bound get_messages call."`
	Coverage           service.MemoryCoverage  `json:"coverage" jsonschema:"Current lexical and semantic memory coverage from the same readiness provider as get_memory_status."`
	CandidateTruncated bool                    `json:"candidate_truncated,omitempty" jsonschema:"With concepts: true when a concept's candidate window filled or more sessions matched than limit, so some sessions may be missing."`
}

type memoryStatusIn struct{}

func (t *toolset) getMemoryStatus(
	ctx context.Context, _ *mcp.CallToolRequest, _ memoryStatusIn,
) (*mcp.CallToolResult, service.MemoryStatus, error) {
	status, err := service.GetMemoryStatus(ctx, t.svc)
	if err != nil {
		return nil, service.MemoryStatus{}, err
	}
	if status.ServerVersion == "" {
		status.ServerVersion = t.version
	}
	return nil, status, nil
}

// searchContentExclusions reports which default exclusions applied, so an
// empty result can be told apart from one the defaults hid.
type searchContentExclusions struct {
	CurrentSessionID string `json:"current_session_id,omitempty"`
	RecentActive     bool   `json:"recent_active"`
	OneShot          bool   `json:"one_shot"`
	Automated        bool   `json:"automated"`
	Children         bool   `json:"children"`
}

func validateSearchDates(from, to string) error {
	for _, date := range []string{from, to} {
		if date == "" {
			continue
		}
		if parsed, err := time.Parse(time.DateOnly, date); err != nil || parsed.Format(time.DateOnly) != date {
			return errors.New("invalid date format: use YYYY-MM-DD")
		}
	}
	if from != "" && to != "" && from > to {
		return errors.New("date_from must not be after date_to")
	}
	return nil
}

func (t *toolset) countContentMatches(ctx context.Context, req service.ContentSearchRequest, legacy bool, keep func(db.ContentMatch) bool) (int, bool, error) {
	req.Context, req.Limit = 0, maxContentSearchLimit
	cursor, total, scanned := 0, 0, 0
	for {
		req.Cursor = cursor
		res, err := t.svc.SearchContent(ctx, req)
		if err != nil {
			return 0, false, err
		}
		for _, m := range res.Matches {
			if !legacy || keep(m) {
				total++
			}
		}
		scanned += len(res.Matches)
		more := res.NextCursor > 0 && len(res.Matches) > 0
		if !more || scanned >= contentTotalCap {
			return min(total, contentTotalCap), more || total > contentTotalCap, nil
		}
		cursor = res.NextCursor
	}
}

func (t *toolset) searchContent(
	ctx context.Context, _ *mcp.CallToolRequest, in searchContentIn,
) (*mcp.CallToolResult, searchContentOut, error) {
	if in.Cursor < 0 {
		return nil, searchContentOut{}, errors.New("cursor must not be negative")
	}
	listing := strings.TrimSpace(in.Pattern) == "" && len(in.Concepts) == 0 && in.SessionID != ""
	if listing {
		in.Pattern, in.Mode = "^", "regex"
		in.IncludeActive, in.IncludeOneShot, in.IncludeAutomated = true, true, true
	} else if strings.TrimSpace(in.Pattern) == "" && len(in.Concepts) == 0 {
		return nil, searchContentOut{}, errors.New("pattern or concepts required")
	}
	if in.SessionID != "" {
		detail, err := t.resolveSession(ctx, in.SessionID)
		if err != nil {
			return nil, searchContentOut{}, err
		}
		in.SessionID = detail.ID
	}
	limit := clampLimit(in.Limit, defaultSearchLimit, maxContentSearchLimit)
	normalized, err := service.NormalizeContentSearchRequest(service.ContentSearchRequest{
		Pattern: in.Pattern, Concepts: in.Concepts, Mode: in.Mode,
		Limit: limit, Cursor: in.Cursor,
	})
	if err != nil {
		return nil, searchContentOut{}, err
	}
	mode := normalized.Mode
	scope := in.Scope
	if scope == "" && db.ContentSearchModeSupportsScope(mode) {
		scope = "all"
	}
	if in.Scope != "" && !db.ContentSearchModeSupportsScope(mode) {
		return nil, searchContentOut{}, errors.New(db.ContentSearchScopeUnsupportedMsg)
	}
	if err := validateSearchDates(in.DateFrom, in.DateTo); err != nil {
		return nil, searchContentOut{}, err
	}
	var cutoffTime time.Time
	if in.ActivityCutoff != "" {
		cutoffTime, err = time.Parse(time.RFC3339, in.ActivityCutoff)
		if err != nil {
			return nil, searchContentOut{}, errors.New("invalid activity_cutoff: use RFC3339")
		}
	}
	currentSession, err := t.resolveOptionalSessionID(ctx, strings.TrimSpace(in.CurrentSessionID))
	if err != nil {
		return nil, searchContentOut{}, err
	}
	excludeSessions := db.NormalizeExcludeSessionIDs([]string{currentSession})
	cutoff := ""
	if currentSession == "" && !in.IncludeActive {
		cutoff = in.ActivityCutoff
		if cutoff == "" {
			if in.Cursor > 0 {
				return nil, searchContentOut{}, errors.New("activity_cutoff is required with a positive cursor: restart search_content without a cursor, then pass both next_cursor as cursor and activity_cutoff")
			}
			cutoffTime = t.clock().Add(-activeExclusionWindow)
			cutoff = cutoffTime.UTC().Format(time.RFC3339Nano)
		} else if cutoffTime.After(t.clock().Add(-activeExclusionWindow)) {
			return nil, searchContentOut{}, errors.New("activity_cutoff must not be later than the recent-activity boundary: restart search_content without a cursor or activity_cutoff")
		}
	}
	req := service.ContentSearchRequest{
		Pattern:            in.Pattern,
		Concepts:           in.Concepts,
		Mode:               mode,
		KeywordFallback:    mode == "hybrid",
		Scope:              scope,
		Project:            in.Project,
		Agent:              in.Agent,
		SessionID:          in.SessionID,
		GitBranchExact:     in.GitBranch,
		DateFrom:           in.DateFrom,
		DateTo:             in.DateTo,
		Limit:              limit,
		Cursor:             in.Cursor,
		Context:            in.Context,
		IncludeOneShot:     in.IncludeOneShot,
		IncludeAutomated:   in.IncludeAutomated,
		ExcludeSessionIDs:  excludeSessions,
		ExcludeActiveSince: cutoff,
	}
	if listing {
		req.Sources = []string{"messages"}
	}
	res, err := t.searchContentMatches(ctx, req, listing)
	if err != nil {
		return nil, searchContentOut{}, err
	}
	legacy := cutoff != "" && !res.ActiveFilterApplied
	out := searchContentOut{
		Matches:        make([]contentMatch, 0, len(res.Matches)),
		ActivityCutoff: cutoff,
		EffectiveMode:  cmp.Or(res.EffectiveMode, mode, "substring"),
		Note:           res.Note,
		Exclusions: searchContentExclusions{
			CurrentSessionID: currentSession,
			RecentActive:     currentSession == "" && !in.IncludeActive,
			OneShot:          !in.IncludeOneShot, Automated: !in.IncludeAutomated,
			Children: scope == "top" || mode == "fts" && in.SessionID == "",
		},
		RevisionBound:      res.RevisionBound,
		Coverage:           service.NormalizeMemoryCoverage(res.Coverage),
		CandidateTruncated: res.CandidateTruncated,
	}
	if db.ContentSearchModeSupportsScope(mode) {
		out.EffectiveScope = scope
	}
	activity := make(map[string]string)
	var keepMatch func(db.ContentMatch) bool
	if cutoff != "" {
		keepMatch = func(m db.ContentMatch) bool {
			ts, ok := t.lookupActivity(ctx, m.SessionID, activity)
			if !ok {
				ts = m.Timestamp
			}
			return !isActiveSince(ts, cutoffTime.Add(activeExclusionWindow))
		}
	}
	rawIndexes := make([]int, 0, len(res.Matches))
	for i, m := range res.Matches {
		if legacy && !keepMatch(m) {
			out.ExcludedActive++
			continue
		}
		rawIndexes = append(rawIndexes, i)
		out.Matches = append(out.Matches, contentMatch{
			WebURL: m.WebURL, SessionID: m.SessionID, Project: m.Project, Agent: m.Agent,
			Location: m.Location, Role: m.Role, Ordinal: m.Ordinal,
			Timestamp: m.Timestamp, Snippet: m.Snippet, Score: m.Score,
			MatchedWords: m.MatchedWords,
			OrdinalRange: m.OrdinalRange, Subordinate: m.Subordinate,
			Relationship: m.Relationship, ParentSessionID: m.ParentSessionID,
			Sidechain:          m.Sidechain,
			ContextBefore:      toContextMessages(m.ContextBefore),
			ContextAfter:       toContextMessages(m.ContextAfter),
			TranscriptRevision: m.TranscriptRevision,
			ConceptEvidence:    m.ConceptEvidence,
		})
	}
	if res.NextCursor > 0 && len(res.Matches) > 0 {
		nc := res.NextCursor
		out.NextCursor = &nc
	}
	out.Hints = service.SearchContentHints(ctx, t.svc, req, res, false, keepMatch)
	// Hints come only with an empty page.
	if len(in.Concepts) == 0 && !fitsResponse(out) {
		items := out.Matches
		ranked := mode == "semantic" || mode == "hybrid"
		if ranked {
			out.NextCursor, out.Note = nil, strings.TrimSpace(out.Note+" Reduce limit to fit ranked results in one response.")
		} else if len(items) > 0 {
			out.Total, out.TotalCapped, err = t.countContentMatches(ctx, req, legacy, keepMatch)
			if err != nil {
				return nil, searchContentOut{}, err
			}
		}
		n := fitPrefix(len(items), func(k int) bool {
			out.Matches, out.Omitted = items[:k], len(items)-k
			if !ranked && k > 0 {
				next := in.Cursor + rawIndexes[k-1] + 1
				out.NextCursor = &next
			}
			return fitsResponse(out)
		})
		if n == 0 {
			return nil, searchContentOut{}, errors.New("search metadata exceeds response budget")
		}
	}
	return nil, out, nil
}

func (t *toolset) searchContentMatches(ctx context.Context, req service.ContentSearchRequest, listing bool) (*service.ContentSearchResult, error) {
	res, err := t.svc.SearchContent(ctx, req)
	if err != nil || !listing {
		return res, err
	}
	for i := range res.Matches {
		match := &res.Matches[i]
		messages, err := t.svc.Messages(ctx, match.SessionID, service.MessageFilter{
			From: &match.Ordinal, Limit: 1, ExpectedRevision: match.TranscriptRevision,
		})
		if err != nil {
			return nil, err
		}
		if len(messages.Messages) != 1 || messages.Messages[0].Ordinal != match.Ordinal {
			return nil, service.ErrSourceChanged
		}
		body := cmp.Or(messages.Messages[0].Content, messages.Messages[0].ThinkingText)
		match.Snippet = (db.ContentSearchFilter{}).BuildSnippet(body, 0, 0)
		_, end := db.SnippetBounds(body, 0, 0, db.ContentSnippetRadius)
		if end < len(body) {
			match.Snippet += "..."
		}
	}
	return res, nil
}

// --- get_usage_summary ---

type usageSummaryIn struct {
	From    string `json:"from,omitempty" jsonschema:"Range start date (YYYY-MM-DD)."`
	To      string `json:"to,omitempty" jsonschema:"Range end date (YYYY-MM-DD)."`
	Project string `json:"project,omitempty" jsonschema:"Filter by project."`
	Agent   string `json:"agent,omitempty" jsonschema:"Filter by agent."`
	Machine string `json:"machine,omitempty" jsonschema:"Exact ID or alias, comma-separated IDs or aliases, or one unique whole display name. IDs and aliases take precedence; unknown or ambiguous selectors return an error."`
}

func (t *toolset) usageSummary(
	ctx context.Context, _ *mcp.CallToolRequest, in usageSummaryIn,
) (*mcp.CallToolResult, *service.UsageSummaryResult, error) {
	res, err := t.svc.UsageSummary(ctx, service.UsageRequest{
		From:    in.From,
		To:      in.To,
		Project: in.Project,
		Agent:   in.Agent,
		Machine: in.Machine,
		// The usage summary surface counts one-shot sessions by default
		// (matching the REST endpoint), since cost analysis wants every
		// session, not just multi-turn ones.
		IncludeOneShot: true,
	})
	if err != nil {
		return nil, nil, err
	}
	return nil, res, nil
}

type fileReferencesIn struct {
	SessionID string `json:"session_id" jsonschema:"A full stored session ID or a raw UUID that matches one session."`
}

func (t *toolset) fileReferences(ctx context.Context, _ *mcp.CallToolRequest, in fileReferencesIn) (*mcp.CallToolResult, service.FileReferencesResult, error) {
	detail, err := t.resolveSession(ctx, in.SessionID)
	if err != nil {
		return nil, service.FileReferencesResult{}, err
	}
	result, err := service.FileReferences(ctx, t.svc, detail.ID)
	return nil, result, err
}

type compactionSummariesIn struct {
	SessionID   string `json:"session_id,omitempty" jsonschema:"A full stored session ID or a raw UUID that matches one session."`
	ActiveSince string `json:"active_since,omitempty" jsonschema:"RFC3339 timestamp."`
	Cursor      string `json:"cursor,omitempty" jsonschema:"Session cursor; resume with the same active_since."`
	Limit       uint   `json:"limit,omitempty" jsonschema:"Sessions per page; 0 uses the default 20, max 100."`
}

func (t *toolset) compactionSummaries(ctx context.Context, _ *mcp.CallToolRequest, in compactionSummariesIn) (*mcp.CallToolResult, service.CompactionSummariesResult, error) {
	if in.SessionID != "" && in.ActiveSince == "" {
		detail, err := t.resolveSession(ctx, in.SessionID)
		if err != nil {
			return nil, service.CompactionSummariesResult{}, err
		}
		in.SessionID = detail.ID
	}
	out, err := service.CompactionSummaries(ctx, t.svc, service.CompactionSummariesRequest{SessionID: in.SessionID, ActiveSince: in.ActiveSince, Cursor: in.Cursor, Limit: int(min(in.Limit, service.MaxCompactionSessionLimit))})
	return nil, out, err
}
