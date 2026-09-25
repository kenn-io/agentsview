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
	svc service.SessionService
	now func() time.Time
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
	Results        []sessionHit `json:"results"`
	NextCursor     *int         `json:"next_cursor,omitempty"`
	ExcludedActive int          `json:"excluded_active,omitempty"`
}

func (t *toolset) searchSessions(
	ctx context.Context, _ *mcp.CallToolRequest, in searchSessionsIn,
) (*mcp.CallToolResult, searchSessionsOut, error) {
	if in.SessionID != "" {
		detail, err := t.svc.Get(ctx, in.SessionID)
		if err != nil {
			return nil, searchSessionsOut{}, err
		}
		if detail == nil {
			ids, err := t.svc.FindSessionIDsByRawSuffix(ctx, in.SessionID, 2)
			if err != nil {
				return nil, searchSessionsOut{}, err
			}
			if len(ids) > 1 {
				return nil, searchSessionsOut{}, fmt.Errorf(
					"ambiguous session UUID %q: use a full stored session ID",
					in.SessionID,
				)
			}
			if len(ids) == 1 {
				detail, err = t.svc.Get(ctx, ids[0])
				if err != nil {
					return nil, searchSessionsOut{}, err
				}
			}
		}
		if detail == nil {
			return nil, searchSessionsOut{}, fmt.Errorf(
				"session not found: %s", in.SessionID,
			)
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

// --- list_sessions ---

type listSessionsIn struct {
	Project          string `json:"project,omitempty" jsonschema:"Filter by project name."`
	Agent            string `json:"agent,omitempty" jsonschema:"Filter by agent (e.g. claude, codex, gemini, antigravity)."`
	Machine          string `json:"machine,omitempty" jsonschema:"Filter by machine name."`
	DateFrom         string `json:"date_from,omitempty" jsonschema:"Only sessions on or after this date (YYYY-MM-DD)."`
	DateTo           string `json:"date_to,omitempty" jsonschema:"Only sessions on or before this date (YYYY-MM-DD)."`
	ActiveSince      string `json:"active_since,omitempty" jsonschema:"Only sessions active since this RFC3339 timestamp."`
	IncludeAutomated bool   `json:"include_automated,omitempty" jsonschema:"Include automated (non-interactive) sessions."`
	Limit            int    `json:"limit,omitempty" jsonschema:"Max results, default 20, max 100."`
	Cursor           string `json:"cursor,omitempty" jsonschema:"Pagination cursor from a previous next_cursor."`
}

type sessionRow struct {
	WebURL           string `json:"web_url,omitempty" jsonschema:"Browser URL for this session; use this URL when linking to it."`
	SessionID        string `json:"session_id"`
	Project          string `json:"project,omitempty"`
	Machine          string `json:"machine,omitempty"`
	Agent            string `json:"agent"`
	Name             string `json:"name"`
	StartedAt        string `json:"started_at"`
	EndedAt          string `json:"ended_at"`
	MessageCount     int    `json:"message_count" jsonschema:"Total stored messages for the session across all roles, including system messages. get_messages always drops system messages, so no roles filter makes its returned count match this; reconcile instead via message_count = returned + filtered summed over a full get_messages pagination sweep."`
	UserMessageCount int    `json:"user_message_count" jsonschema:"Stored user-role messages, excluding those flagged as system-injected."`
	OutputTokens     int64  `json:"output_tokens,omitempty"`
	Outcome          string `json:"outcome,omitempty"`
	HealthGrade      string `json:"health_grade,omitempty"`
	GitBranch        string `json:"git_branch,omitempty"`
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
		Project:          in.Project,
		Agent:            in.Agent,
		Machine:          in.Machine,
		DateFrom:         in.DateFrom,
		DateTo:           in.DateTo,
		ActiveSince:      in.ActiveSince,
		IncludeAutomated: in.IncludeAutomated,
		Cursor:           in.Cursor,
		Limit:            clampLimit(in.Limit, defaultListLimit, maxListLimit),
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
	}
}

// --- get_session_overview ---

type sessionOverviewIn struct {
	SessionID string `json:"session_id" jsonschema:"The session to summarize."`
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
	detail, err := t.svc.Get(ctx, in.SessionID)
	if err != nil {
		return nil, sessionOverviewOut{}, err
	}
	if detail == nil {
		return nil, sessionOverviewOut{}, fmt.Errorf(
			"session not found: %s", in.SessionID)
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
	msgs, err := t.svc.Messages(ctx, in.SessionID, service.MessageFilter{
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
	SessionID string `json:"session_id" jsonschema:"The session to read."`
	From      *int   `json:"from,omitempty" jsonschema:"Ordinal to start from (e.g. match_ordinal from search_sessions). Ordinal 0 is a valid anchor (the first message). Mutually exclusive with around."`
	Direction string `json:"direction,omitempty" jsonschema:"asc (default, oldest first) or desc (newest first). Mutually exclusive with around."`
	Limit     int    `json:"limit,omitempty" jsonschema:"Max messages scanned, default 20, max 100. System/tool messages are filtered after this limit, so a page can return fewer; use next_from to continue. Not used with around; before/after control the window size on that path."`
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
	Messages []messageOut `json:"messages"`
	Filtered int          `json:"filtered,omitempty" jsonschema:"How many of this page's scanned messages were excluded by the role/system filter (omitted when zero). Summed across a full pagination sweep, filtered plus the returned messages adds up to the session's message_count."`
	// NextFrom is set off the last scanned ordinal (not the last visible
	// one), so paging stays reliable even though filtering can make a page
	// return fewer than the limit.
	NextFrom           *int   `json:"next_from,omitempty" jsonschema:"Anchor for the next page's from parameter when more messages may remain; absent means the end. Filtering can make a page return fewer than limit messages, so keep paging until next_from is absent, not until a page comes back short."`
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
	attachBodyCursors(&out, res.EvidenceSource, in.SessionID, in.Roles)
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
	attachBodyCursors(&out, res.EvidenceSource, in.SessionID, in.Roles)
	return nil, out, nil
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
		out.Messages[i].BodyCursor = encodeMessageBodyCursor(messageBodyCursor{
			Version: 2, EvidenceSource: evidenceSource, SessionID: sessionID,
			Revision: out.TranscriptRevision, Ordinal: out.Messages[i].Ordinal,
			Offset: utf8.RuneCountInString(out.Messages[i].Content), NextFrom: nextFrom,
			Roles: roles,
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
	return nil, out, nil
}

// --- search_content ---

type searchContentIn struct {
	Pattern          string `json:"pattern" jsonschema:"Natural-language query for semantic/hybrid, whitespace-separated literals for terms, or exact substring/regex for lexical search across message text and tool inputs/results."`
	Mode             string `json:"mode,omitempty" jsonschema:"substring (default), regex, terms, semantic, or hybrid. Prefer hybrid or semantic for contextual questions when a vector search index is configured."`
	Scope            string `json:"scope,omitempty" jsonschema:"Semantic/hybrid/terms result scope: top, all, or subordinate (default all)."`
	Project          string `json:"project,omitempty" jsonschema:"Restrict to one project."`
	Agent            string `json:"agent,omitempty" jsonschema:"Restrict to one agent."`
	SessionID        string `json:"session_id,omitempty" jsonschema:"Restrict to one exact full stored session ID."`
	GitBranch        string `json:"git_branch,omitempty" jsonschema:"Restrict to one exact git branch."`
	CurrentSessionID string `json:"current_session_id,omitempty" jsonschema:"Exclude this exact full stored session ID instead of applying the recent-activity heuristic."`
	DateFrom         string `json:"date_from,omitempty" jsonschema:"Only sessions on or after this date (YYYY-MM-DD)."`
	DateTo           string `json:"date_to,omitempty" jsonschema:"Only sessions on or before this date (YYYY-MM-DD)."`
	Limit            int    `json:"limit,omitempty" jsonschema:"Max matches, default 10, max 50."`
	Cursor           int    `json:"cursor,omitempty" jsonschema:"Pagination cursor from a previous next_cursor."`
	IncludeActive    bool   `json:"include_active,omitempty" jsonschema:"Include matches from sessions active in the last 10 minutes. Default false: the conversation you are in right now is also recorded, so without this exclusion you would find yourself."`
	IncludeOneShot   bool   `json:"include_one_shot,omitempty" jsonschema:"Include one-shot sessions. Default false."`
	IncludeAutomated bool   `json:"include_automated,omitempty" jsonschema:"Include automated sessions. Default false."`
	Context          int    `json:"context,omitempty" jsonschema:"Messages of context before/after each match (max 10)."`
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
	Location        string   `json:"location" jsonschema:"Where the match occurred: one of message, tool_input, or tool_result."`
	Role            string   `json:"role,omitempty"`
	Ordinal         int      `json:"ordinal"`
	Timestamp       string   `json:"timestamp"`
	Snippet         string   `json:"snippet"`
	Score           *float64 `json:"score,omitempty" jsonschema:"Relevance score for semantic/hybrid modes; omitted for substring/regex/fts."`
	OrdinalRange    [2]int   `json:"ordinal_range" jsonschema:"[start, end] ordinals of the conversation unit containing this match; equal to the match ordinal for single-message units."`
	Subordinate     bool     `json:"subordinate,omitzero" jsonschema:"True when this match belongs to a subordinate unit: a sidechain run, or a subagent/fork session."`
	Relationship    string   `json:"relationship,omitempty" jsonschema:"The matched session's relationship to its parent (for example subagent or fork), when it has one."`
	ParentSessionID string   `json:"parent_session_id,omitempty" jsonschema:"The parent session ID, when the matched session has one."`
	Sidechain       bool     `json:"is_sidechain,omitzero" jsonschema:"True when the matched message itself is flagged as a sidechain message."`
	// ContextBefore/ContextAfter are populated when Context > 0: the N
	// messages immediately before/after this match, content truncated to
	// 500 characters.
	ContextBefore      []contextMessage `json:"context_before,omitempty"`
	ContextAfter       []contextMessage `json:"context_after,omitempty"`
	TranscriptRevision string           `json:"transcript_revision,omitempty" jsonschema:"Transcript revision observed with this evidence; pass it as expected_revision to get_messages."`
}

type searchContentOut struct {
	Matches        []contentMatch `json:"matches"`
	NextCursor     *int           `json:"next_cursor,omitempty"`
	ExcludedActive int            `json:"excluded_active,omitempty"`
	// EffectiveMode and EffectiveScope report the defaults the search
	// resolved, which the caller cannot see in its own request.
	EffectiveMode  string                  `json:"effective_mode"`
	EffectiveScope string                  `json:"effective_scope,omitempty"`
	Exclusions     searchContentExclusions `json:"exclusions"`
	RevisionBound  bool                    `json:"revision_bound" jsonschema:"True when every returned match was captured with a transcript revision and can be opened with a revision-bound get_messages call."`
}

// searchContentExclusions reports which default exclusions applied, so an
// empty result can be told apart from one the defaults hid.
type searchContentExclusions struct {
	CurrentSessionID string `json:"current_session_id,omitempty"`
	RecentActive     bool   `json:"recent_active"`
	OneShot          bool   `json:"one_shot"`
	Automated        bool   `json:"automated"`
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

func (t *toolset) searchContent(
	ctx context.Context, _ *mcp.CallToolRequest, in searchContentIn,
) (*mcp.CallToolResult, searchContentOut, error) {
	if in.Scope != "" && !db.ContentSearchModeSupportsScope(in.Mode) {
		return nil, searchContentOut{}, errors.New(db.ContentSearchScopeUnsupportedMsg)
	}
	if err := validateSearchDates(in.DateFrom, in.DateTo); err != nil {
		return nil, searchContentOut{}, err
	}
	currentSession := strings.TrimSpace(in.CurrentSessionID)
	excludeSessions := db.NormalizeExcludeSessionIDs([]string{currentSession})
	res, err := t.svc.SearchContent(ctx, service.ContentSearchRequest{
		Pattern:           in.Pattern,
		Mode:              in.Mode,
		Scope:             in.Scope,
		Project:           in.Project,
		Agent:             in.Agent,
		SessionID:         in.SessionID,
		GitBranchExact:    in.GitBranch,
		DateFrom:          in.DateFrom,
		DateTo:            in.DateTo,
		Limit:             clampLimit(in.Limit, defaultSearchLimit, maxContentSearchLimit),
		Cursor:            in.Cursor,
		Context:           in.Context,
		IncludeOneShot:    in.IncludeOneShot,
		IncludeAutomated:  in.IncludeAutomated,
		ExcludeSessionIDs: excludeSessions,
	})
	if err != nil {
		return nil, searchContentOut{}, err
	}
	now := t.clock()
	// The self-reference guard excludes matches from sessions active in the
	// last 10 minutes. A match carries only its own timestamp, but a
	// long-running session can match on an old message while still being
	// active now, so exclude by the session's activity (ended_at, falling
	// back to started_at) like search_sessions does -- not by the match
	// timestamp. Activity is looked up once per session and cached.
	activity := make(map[string]string, len(res.Matches))
	out := searchContentOut{
		Matches:       make([]contentMatch, 0, len(res.Matches)),
		EffectiveMode: cmp.Or(in.Mode, "substring"),
		Exclusions: searchContentExclusions{
			CurrentSessionID: currentSession,
			RecentActive:     currentSession == "" && !in.IncludeActive,
			OneShot:          !in.IncludeOneShot, Automated: !in.IncludeAutomated,
		},
		RevisionBound: res.RevisionBound,
	}
	if db.ContentSearchModeSupportsScope(in.Mode) {
		out.EffectiveScope = cmp.Or(in.Scope, "all")
	}
	for _, m := range res.Matches {
		if currentSession == "" && !in.IncludeActive {
			ts, ok := t.lookupActivity(ctx, m.SessionID, activity)
			if !ok {
				// Lookup failed: fall back to the match timestamp so the
				// guard degrades to its prior behavior rather than erroring.
				ts = m.Timestamp
			}
			if isActiveSince(ts, now) {
				out.ExcludedActive++
				continue
			}
		}
		out.Matches = append(out.Matches, contentMatch{
			WebURL: m.WebURL, SessionID: m.SessionID, Project: m.Project, Agent: m.Agent,
			Location: m.Location, Role: m.Role, Ordinal: m.Ordinal,
			Timestamp: m.Timestamp, Snippet: m.Snippet, Score: m.Score,
			OrdinalRange: m.OrdinalRange, Subordinate: m.Subordinate,
			Relationship: m.Relationship, ParentSessionID: m.ParentSessionID,
			Sidechain:          m.Sidechain,
			ContextBefore:      toContextMessages(m.ContextBefore),
			ContextAfter:       toContextMessages(m.ContextAfter),
			TranscriptRevision: m.TranscriptRevision,
		})
	}
	if res.NextCursor > 0 && len(res.Matches) > 0 {
		nc := res.NextCursor
		out.NextCursor = &nc
	}
	return nil, out, nil
}

// --- get_usage_summary ---

type usageSummaryIn struct {
	From    string `json:"from,omitempty" jsonschema:"Range start date (YYYY-MM-DD)."`
	To      string `json:"to,omitempty" jsonschema:"Range end date (YYYY-MM-DD)."`
	Project string `json:"project,omitempty" jsonschema:"Filter by project."`
	Agent   string `json:"agent,omitempty" jsonschema:"Filter by agent."`
	Machine string `json:"machine,omitempty" jsonschema:"Filter by machine."`
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
