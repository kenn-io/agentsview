// ABOUTME: The six read-only MCP tools, implemented as thin adapters
// ABOUTME: over service.SessionService (the same seam the CLI and HTTP
// ABOUTME: handlers use).
package mcp

import (
	"context"
	"fmt"
	"time"

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
	Query         string `json:"query" jsonschema:"Search terms across all agent sessions. Every term must appear (AND), not an exact phrase; wrap the whole query in double quotes for an exact phrase, e.g. \"build failed\". Punctuation in a term (hyphens, colons) is handled safely."`
	Project       string `json:"project,omitempty" jsonschema:"Restrict to one project (repo/directory name)."`
	Sort          string `json:"sort,omitempty" jsonschema:"relevance (default) or recency."`
	Limit         int    `json:"limit,omitempty" jsonschema:"Max results, default 10, max 30."`
	Cursor        int    `json:"cursor,omitempty" jsonschema:"Pagination cursor from a previous next_cursor."`
	IncludeActive bool   `json:"include_active,omitempty" jsonschema:"Include sessions active in the last 10 minutes. Default false: the conversation you are in right now is also recorded, so without this exclusion you would find yourself."`
}

type sessionHit struct {
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
	res, err := t.svc.Search(ctx, service.SearchRequest{
		Query:   buildSearchQuery(in.Query),
		Project: in.Project,
		Sort:    in.Sort,
		Cursor:  in.Cursor,
		Limit:   clampLimit(in.Limit, defaultSearchLimit, maxSearchLimit),
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
	SessionID          string   `json:"session_id" jsonschema:"The session to read."`
	From               *int     `json:"from,omitempty" jsonschema:"Ordinal to start from (e.g. match_ordinal from search_sessions). Ordinal 0 is a valid anchor (the first message)."`
	Direction          string   `json:"direction,omitempty" jsonschema:"asc (default, oldest first) or desc (newest first)."`
	Limit              int      `json:"limit,omitempty" jsonschema:"Max messages scanned, default 20, max 100. System/tool messages are filtered after this limit, so a page can return fewer; use next_from to continue."`
	Roles              []string `json:"roles,omitempty" jsonschema:"Roles to include, e.g. tool. Default: user and assistant only. System messages are always excluded."`
	MaxCharsPerMessage int      `json:"max_chars_per_message,omitempty" jsonschema:"Truncate each message to this many characters, default 2000, max 20000."`
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
}

type getMessagesOut struct {
	Messages []messageOut `json:"messages"`
	Filtered int          `json:"filtered,omitempty" jsonschema:"How many of this page's scanned messages were excluded by the role/system filter (omitted when zero). Summed across a full pagination sweep, filtered plus the returned messages adds up to the session's message_count."`
	// NextFrom is set off the last scanned ordinal (not the last visible
	// one), so paging stays reliable even though filtering can make a page
	// return fewer than the limit.
	NextFrom *int `json:"next_from,omitempty" jsonschema:"Anchor for the next page's from parameter when more messages may remain; absent means the end. Filtering can make a page return fewer than limit messages, so keep paging until next_from is absent, not until a page comes back short."`
}

func (t *toolset) getMessages(
	ctx context.Context, _ *mcp.CallToolRequest, in getMessagesIn,
) (*mcp.CallToolResult, getMessagesOut, error) {
	// From is a *int so an explicit ordinal 0 (a valid match_ordinal)
	// anchors at the first message rather than being mistaken for
	// "omitted". A nil From lets the service default: desc to
	// newest-first, asc to oldest-first.
	limit := clampLimit(in.Limit, defaultMessageLimit, maxMessageLimit)
	res, err := t.svc.Messages(ctx, in.SessionID, service.MessageFilter{
		From:      in.From,
		Direction: in.Direction,
		Limit:     limit,
	})
	if err != nil {
		return nil, getMessagesOut{}, err
	}
	maxChars := clampLimit(
		in.MaxCharsPerMessage, defaultMaxCharsPerMessage, maxMaxCharsPerMessage)
	out := getMessagesOut{Messages: make([]messageOut, 0, len(res.Messages))}
	for _, m := range res.Messages {
		if isSystemMessage(m) || !roleAllowed(m.Role, in.Roles) {
			out.Filtered++
			continue
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
			mo.FullLength = m.ContentLength
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
	return nil, out, nil
}

// --- search_content ---

type searchContentIn struct {
	Pattern       string `json:"pattern" jsonschema:"Exact substring or regex to find across message text and tool inputs/results."`
	Mode          string `json:"mode,omitempty" jsonschema:"substring (default) or regex."`
	Project       string `json:"project,omitempty" jsonschema:"Restrict to one project."`
	Agent         string `json:"agent,omitempty" jsonschema:"Restrict to one agent."`
	DateFrom      string `json:"date_from,omitempty" jsonschema:"Only sessions on or after this date (YYYY-MM-DD)."`
	DateTo        string `json:"date_to,omitempty" jsonschema:"Only sessions on or before this date (YYYY-MM-DD)."`
	Limit         int    `json:"limit,omitempty" jsonschema:"Max matches, default 10, max 30."`
	Cursor        int    `json:"cursor,omitempty" jsonschema:"Pagination cursor from a previous next_cursor."`
	IncludeActive bool   `json:"include_active,omitempty" jsonschema:"Include matches from sessions active in the last 10 minutes. Default false: the conversation you are in right now is also recorded, so without this exclusion you would find yourself."`
}

type contentMatch struct {
	SessionID string `json:"session_id"`
	Project   string `json:"project,omitempty"`
	Agent     string `json:"agent"`
	Location  string `json:"location" jsonschema:"Where the match occurred: one of message, tool_input, or tool_result."`
	Role      string `json:"role,omitempty"`
	Ordinal   int    `json:"ordinal"`
	Timestamp string `json:"timestamp"`
	Snippet   string `json:"snippet"`
}

type searchContentOut struct {
	Matches        []contentMatch `json:"matches"`
	NextCursor     *int           `json:"next_cursor,omitempty"`
	ExcludedActive int            `json:"excluded_active,omitempty"`
}

func (t *toolset) searchContent(
	ctx context.Context, _ *mcp.CallToolRequest, in searchContentIn,
) (*mcp.CallToolResult, searchContentOut, error) {
	res, err := t.svc.SearchContent(ctx, service.ContentSearchRequest{
		Pattern:  in.Pattern,
		Mode:     in.Mode,
		Project:  in.Project,
		Agent:    in.Agent,
		DateFrom: in.DateFrom,
		DateTo:   in.DateTo,
		Limit:    clampLimit(in.Limit, defaultSearchLimit, maxSearchLimit),
		Cursor:   in.Cursor,
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
	out := searchContentOut{Matches: make([]contentMatch, 0, len(res.Matches))}
	for _, m := range res.Matches {
		if !in.IncludeActive {
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
			SessionID: m.SessionID, Project: m.Project, Agent: m.Agent,
			Location: m.Location, Role: m.Role, Ordinal: m.Ordinal,
			Timestamp: m.Timestamp, Snippet: m.Snippet,
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
