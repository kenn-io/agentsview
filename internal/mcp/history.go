// ABOUTME: Read-only MCP tools for one session's details: its tool calls,
// ABOUTME: the files sessions edited, and its child sessions.
package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/secrets"
	"go.kenn.io/agentsview/internal/service"
)

const (
	defaultToolCallLimit = 50
	// defaultResultToolCallLimit pages one call at a time when results are
	// requested, since each call carries a summary plus its event history.
	defaultResultToolCallLimit = 1
	maxToolCallLimit           = 200
	defaultToolInputChars      = 500
	maxToolInputChars          = 10000
	defaultRecentEditsLimit    = 10
	maxRecentEditsLimit        = 50
	maxRecentEditsPathLength   = 1024
	// redactSlackChars is how far past max_input_chars a tool input is
	// scanned for secrets, longer than any secret the scanner recognizes.
	redactSlackChars = 8192
)

// --- get_tool_calls ---

type toolCallsIn struct {
	ResultCursor   string `json:"result_cursor,omitempty" jsonschema:"Continue a result or event."`
	SessionID      string `json:"session_id" jsonschema:"A full stored session ID or a raw UUID that matches one session."`
	Category       string `json:"category,omitempty" jsonschema:"Only calls in this category: Read, Edit, Write, Bash, Grep, Glob, Task, Tool, or Other."`
	Limit          int    `json:"limit,omitempty" jsonschema:"Default 50 (1 with results), max 200."`
	Cursor         int    `json:"cursor,omitempty" jsonschema:"Pagination cursor from a previous next_cursor."`
	IncludeResults bool   `json:"include_results,omitempty"`
	MaxResultChars int    `json:"max_result_chars,omitempty" jsonschema:"Per result or event: default 1000, max 10000 characters."`
	MaxInputChars  int    `json:"max_input_chars,omitempty" jsonschema:"Per input: default 500, max 10000 characters."`
}

type toolCallOut struct {
	Ordinal           int                  `json:"ordinal"`
	Timestamp         string               `json:"timestamp,omitempty"`
	ToolName          string               `json:"tool_name"`
	Category          *string              `json:"category,omitzero"`
	Input             *string              `json:"input,omitzero"`
	InputTruncated    bool                 `json:"input_truncated,omitempty"`
	SkillName         string               `json:"skill_name,omitempty"`
	SubagentSessionID string               `json:"subagent_session_id,omitempty"`
	Result            string               `json:"result,omitempty"`
	ResultCursor      string               `json:"result_cursor,omitempty"`
	ResultTruncated   bool                 `json:"result_truncated,omitempty"`
	ResultLength      int                  `json:"result_length" jsonschema:"Result size in bytes."`
	ResultEvents      []toolResultEventOut `json:"result_events,omitempty"`
}

type toolResultEventOut struct {
	EventIndex        int    `json:"event_index"`
	AgentID           string `json:"agent_id,omitempty"`
	SubagentSessionID string `json:"subagent_session_id,omitempty"`
	Status            string `json:"status,omitempty"`
	Content           string `json:"content"`
	ContentLength     int    `json:"content_length"`
	Truncated         bool   `json:"truncated,omitempty"`
	ResultCursor      string `json:"result_cursor,omitempty"`
}

type toolCallsOut struct {
	ToolCalls  []toolCallOut `json:"tool_calls"`
	Omitted    int           `json:"omitted,omitempty"`
	Total      *int          `json:"total,omitzero"`
	NextCursor *int          `json:"next_cursor,omitempty"`
}

func (t *toolset) toolCalls(
	ctx context.Context, _ *mcp.CallToolRequest, in toolCallsIn,
) (*mcp.CallToolResult, toolCallsOut, error) {
	if in.ResultCursor != "" {
		return t.toolResultContinuation(ctx, in)
	}
	if in.Cursor < 0 {
		return nil, toolCallsOut{}, errors.New("cursor must not be negative")
	}
	detail, err := t.resolveSession(ctx, in.SessionID)
	if err != nil {
		return nil, toolCallsOut{}, err
	}
	in.SessionID = detail.ID
	list, err := t.svc.ToolCalls(ctx, detail.ID)
	if err != nil {
		return nil, toolCallsOut{}, err
	}
	type positionedCall struct {
		service.ToolCall
		position int
	}
	matching := make([]positionedCall, 0, len(list.ToolCalls))
	positions := make(map[int]int)
	for _, tc := range list.ToolCalls {
		pos := positions[tc.Ordinal]
		positions[tc.Ordinal]++
		if in.Category == "" || strings.EqualFold(tc.Category, in.Category) {
			matching = append(matching, positionedCall{tc, pos})
		}
	}
	limit := toolCallLimit(in)
	maxChars := clampLimit(in.MaxInputChars, defaultToolInputChars, maxToolInputChars)
	start := min(in.Cursor, len(matching))
	end := min(start+limit, len(matching))
	out := toolCallsOut{
		ToolCalls: make([]toolCallOut, 0, end-start),
		Total:     new(len(matching)),
	}
	results := make(map[int]db.Message)
	if in.IncludeResults {
		for _, tc := range matching[start:end] {
			if _, ok := results[tc.Ordinal]; ok {
				continue
			}
			zero := 0
			page, err := t.svc.Messages(ctx, in.SessionID, service.MessageFilter{Around: &tc.Ordinal, Before: &zero, After: &zero})
			if err != nil {
				return nil, toolCallsOut{}, err
			}
			if len(page.Messages) != 1 || page.Messages[0].Ordinal != tc.Ordinal {
				return nil, toolCallsOut{}, fmt.Errorf("%w: cited message no longer exists", service.ErrSourceChanged)
			}
			results[tc.Ordinal] = page.Messages[0]
		}
	}
	for _, tc := range matching[start:end] {
		call, err := toolCallPage(tc.ToolCall, tc.position, results[tc.Ordinal], in, maxChars)
		if err != nil {
			return nil, toolCallsOut{}, err
		}
		out.ToolCalls = append(out.ToolCalls, call)
	}
	if end < len(matching) {
		out.NextCursor = &end
	}
	if !fitsResponse(out) {
		items := out.ToolCalls
		n := fitPrefix(len(items), func(k int) bool {
			out.ToolCalls = items[:k]
			out.Omitted = len(items) - k
			next := start + max(k, 1)
			out.NextCursor = &next
			return fitsResponse(out)
		})
		if n == 0 {
			out.Omitted = len(items) - 1
			first := matching[start]
			if !fitToolCall(&out, first.ToolCall, first.position, results[first.Ordinal], in, maxChars) {
				return nil, toolCallsOut{}, errors.New("tool call metadata exceeds response budget")
			}
		}
	}
	return nil, out, nil
}

// resultBoundaries lists the result lengths where continuation fields change:
// the summary's and each saved event's masked length, within the result limit.
func resultBoundaries(m db.Message, position int, in toolCallsIn) []int {
	maxResult := clampLimit(in.MaxResultChars, 1000, 10000)
	boundaries := []int{1, maxResult + 1}
	if in.IncludeResults {
		stored := m.ToolCalls[position]
		texts := []string{stored.ResultContent}
		for _, ev := range stored.ResultEvents {
			texts = append(texts, ev.Content)
		}
		for _, text := range texts {
			if n := utf8.RuneCountInString(secrets.RedactJSON(text)); n > 1 && n <= maxResult {
				boundaries = append(boundaries, n)
			}
		}
	}
	slices.Sort(boundaries)
	return boundaries
}

// fitToolCall searches between result lengths where continuation fields change.
func fitToolCall(out *toolCallsOut, tc service.ToolCall, position int, m db.Message, in toolCallsIn, maxInput int) bool {
	boundaries := resultBoundaries(m, position, in)
	best := 0
	build := func(resultChars, inputChars int) bool {
		narrow := in
		narrow.MaxResultChars = resultChars
		call, _ := toolCallPage(tc, position, m, narrow, inputChars)
		out.ToolCalls = []toolCallOut{call}
		return fitsResponse(out)
	}
	for i := 0; i+1 < len(boundaries); i++ {
		lo, hi := boundaries[i], boundaries[i+1]-1
		if lo > hi || !build(lo, 1) {
			continue
		}
		k := fitPrefix(hi-lo, func(k int) bool { return build(lo+k, 1) })
		best = lo + k
	}
	if best == 0 {
		return false
	}
	fitPrefix(maxInput-1, func(k int) bool { return build(best, k+1) })
	return true
}

func (t *toolset) toolResultContinuation(ctx context.Context, in toolCallsIn) (*mcp.CallToolResult, toolCallsOut, error) {
	cursor, err := decodeToolResultCursor(in.ResultCursor)
	if err != nil {
		return nil, toolCallsOut{}, errors.New("invalid result_cursor")
	}
	detail, err := t.resolveSession(ctx, in.SessionID)
	if err != nil {
		return nil, toolCallsOut{}, err
	}
	if cursor.SessionID != detail.ID {
		return nil, toolCallsOut{}, errors.New("invalid result_cursor")
	}
	zero := 0
	page, err := t.svc.Messages(ctx, detail.ID, service.MessageFilter{Around: &cursor.Ordinal, Before: &zero, After: &zero})
	if err != nil {
		return nil, toolCallsOut{}, err
	}
	if len(page.Messages) != 1 || page.Messages[0].Ordinal != cursor.Ordinal || cursor.Position >= len(page.Messages[0].ToolCalls) {
		return nil, toolCallsOut{}, fmt.Errorf("%w: cited tool call no longer exists", service.ErrSourceChanged)
	}
	call, err := continueToolResult(cursor, page.Messages[0].ToolCalls[cursor.Position], clampLimit(in.MaxResultChars, 1000, 10000))
	if err != nil {
		return nil, toolCallsOut{}, err
	}
	return nil, toolCallsOut{ToolCalls: []toolCallOut{call}}, nil
}

// toolCallPage builds one call's page: its masked input cut to inputChars and,
// when requested, its result text cut to in.MaxResultChars.
func toolCallPage(tc service.ToolCall, position int, m db.Message, in toolCallsIn, inputChars int) (toolCallOut, error) {
	input, partial := redactToolInput(tc.InputJSON, inputChars)
	input, cut := truncate(input, inputChars)
	call := toolCallOut{
		Ordinal: tc.Ordinal, Timestamp: tc.Timestamp,
		ToolName: tc.ToolName, Category: new(tc.Category),
		Input: new(input), InputTruncated: cut || partial,
		SkillName: tc.SkillName, SubagentSessionID: tc.SubagentSessionID,
		ResultLength: tc.ResultLength,
	}
	if in.IncludeResults {
		return call, fillToolResult(&call, m, tc, position, in)
	}
	return call, nil
}

// toolCallLimit is the page size: a page with results defaults to one call.
func toolCallLimit(in toolCallsIn) int {
	def := defaultToolCallLimit
	if in.IncludeResults {
		def = defaultResultToolCallLimit
	}
	return clampLimit(in.Limit, def, maxToolCallLimit)
}

// fillToolResult checks that the stored call is the one cited and adds its
// masked summary and saved events to call.
func fillToolResult(call *toolCallOut, m db.Message, tc service.ToolCall, position int, in toolCallsIn) error {
	if position >= len(m.ToolCalls) {
		return fmt.Errorf("%w: cited tool call no longer matches", service.ErrSourceChanged)
	}
	stored := m.ToolCalls[position]
	if m.Timestamp != tc.Timestamp || stored.ToolUseID != tc.ToolUseID || stored.ToolName != tc.ToolName || stored.Category != tc.Category || stored.InputJSON != tc.InputJSON || stored.SkillName != tc.SkillName || stored.SubagentSessionID != tc.SubagentSessionID || stored.ResultContentLength != tc.ResultLength {
		return fmt.Errorf("%w: cited tool call no longer matches", service.ErrSourceChanged)
	}
	base := toolResultCursor{Version: toolResultCursorVersion, SessionID: in.SessionID, Ordinal: tc.Ordinal, Position: position}
	maxChars := clampLimit(in.MaxResultChars, 1000, 10000)
	summary := secrets.RedactJSON(stored.ResultContent)
	call.Result, call.ResultTruncated, call.ResultCursor = firstResultPage(base, nil, summary, maxChars)
	if sole, ok := db.SoleToolResultContent(stored.ResultEvents); ok && len(stored.ResultEvents) == 1 && sole.AgentID == "" && sole.SubagentSessionID == "" && sole.Status == "" && sole.Content == stored.ResultContent {
		return nil
	}
	for _, ev := range stored.ResultEvents {
		if ev.Content != "" || ev.Status != "" {
			call.ResultEvents = append(call.ResultEvents, resultEventPage(base, ev, maxChars))
		}
	}
	return nil
}

// firstResultPage returns the first maxChars of masked text and the cursor
// that continues it, or none when the text fits.
func firstResultPage(base toolResultCursor, event *int, text string, maxChars int) (string, bool, string) {
	page, cut := truncate(text, maxChars)
	if !cut {
		return page, false, ""
	}
	base.Event = event
	base.Digest = textDigest(text)
	base.Offset = utf8.RuneCountInString(page)
	return page, true, encodeToolResultCursor(base)
}

func resultEventPage(base toolResultCursor, ev db.ToolResultEvent, maxChars int) toolResultEventOut {
	out := toolResultEventOut{EventIndex: ev.EventIndex, AgentID: ev.AgentID, SubagentSessionID: ev.SubagentSessionID, Status: ev.Status, ContentLength: ev.ContentLength}
	out.Content, out.Truncated, out.ResultCursor = firstResultPage(base, &ev.EventIndex, secrets.RedactJSON(ev.Content), maxChars)
	return out
}

// continueToolResult returns the next chunk of the one summary or event the
// cursor names, once its masked text still matches the cursor's digest.
func continueToolResult(cursor toolResultCursor, tc db.ToolCall, maxChars int) (toolCallOut, error) {
	call := toolCallOut{Ordinal: cursor.Ordinal, ToolName: tc.ToolName, ResultLength: tc.ResultContentLength}
	text, event := secrets.RedactJSON(tc.ResultContent), toolResultEventOut{}
	if cursor.Event != nil {
		i := slices.IndexFunc(tc.ResultEvents, func(ev db.ToolResultEvent) bool { return ev.EventIndex == *cursor.Event })
		if i < 0 {
			return call, fmt.Errorf("%w: cited result event no longer exists", service.ErrSourceChanged)
		}
		ev := tc.ResultEvents[i]
		text = secrets.RedactJSON(ev.Content)
		event = toolResultEventOut{EventIndex: ev.EventIndex, AgentID: ev.AgentID, SubagentSessionID: ev.SubagentSessionID, Status: ev.Status, ContentLength: ev.ContentLength}
	}
	if !bytes.Equal(cursor.Digest, textDigest(text)) {
		return call, fmt.Errorf("%w: result changed; restart from get_tool_calls with include_results", service.ErrSourceChanged)
	}
	runes := []rune(text)
	if cursor.Offset >= len(runes) {
		return call, errors.New("invalid result_cursor offset")
	}
	// page places s, and the cursor that follows it, where the cursor names.
	page := func(s string) toolCallOut {
		next, after := "", cursor
		after.Offset += utf8.RuneCountInString(s)
		if after.Offset < len(runes) {
			next = encodeToolResultCursor(after)
		}
		out := call
		if cursor.Event == nil {
			out.Result, out.ResultTruncated, out.ResultCursor = s, next != "", next
			return out
		}
		ev := event
		ev.Content, ev.Truncated, ev.ResultCursor = s, next != "", next
		out.ResultEvents = []toolResultEventOut{ev}
		return out
	}
	window := string(runes[cursor.Offset:min(cursor.Offset+maxChars, len(runes))])
	chunk := fitText(window, responseBudgetBytes, func(s string) any { return toolCallsOut{ToolCalls: []toolCallOut{page(s)}} })
	return page(chunk), nil
}

const toolResultCursorVersion = 1

// toolResultCursor binds a continuation to the exact masked text it pages
// through, by digest, so unrelated transcript growth does not invalidate it.
type toolResultCursor struct {
	Version   int    `json:"v"`
	SessionID string `json:"i"`
	Digest    []byte `json:"d"`
	Ordinal   int    `json:"o"`
	Position  int    `json:"c"`
	Event     *int   `json:"e,omitempty"`
	Offset    int    `json:"p"`
}

func textDigest(text string) []byte {
	sum := sha256.Sum256([]byte(text))
	return sum[:]
}

func encodeToolResultCursor(cursor toolResultCursor) string {
	raw, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeToolResultCursor(raw string) (toolResultCursor, error) {
	payload, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return toolResultCursor{}, errors.New("invalid result_cursor")
	}
	var cursor toolResultCursor
	if err := json.Unmarshal(payload, &cursor); err != nil || cursor.Version != toolResultCursorVersion || cursor.SessionID == "" || len(cursor.Digest) != sha256.Size || cursor.Ordinal < 0 || cursor.Position < 0 || (cursor.Event != nil && *cursor.Event < 0) || cursor.Offset <= 0 {
		return toolResultCursor{}, errors.New("invalid result_cursor")
	}
	return cursor, nil
}

// redactToolInput masks secrets in each decoded JSON string of a tool input,
// so JSON escapes inside a multiline private key cannot hide it from the
// scanner. Everything between strings is copied as stored. It stops once the
// output passes maxChars plus some slack, and when decoding fails partway it
// keeps only the checked prefix; both report partial. Input that is not JSON
// at all is masked as plain text.
func redactToolInput(raw string, maxChars int) (string, bool) {
	bound := utf8.UTFMax*maxChars + redactSlackChars
	dec := jsontext.NewDecoder(strings.NewReader(raw),
		jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true))
	var out strings.Builder
	copied, checked := 0, 0
	for checked <= bound {
		tok, err := dec.ReadToken()
		if errors.Is(err, io.EOF) {
			out.WriteString(raw[copied:])
			return out.String(), false
		}
		if err != nil {
			if checked == 0 {
				// Scan all of it: a cut could split a key's markers.
				return secrets.Redact(raw), false
			}
			break
		}
		end := int(dec.InputOffset())
		if tok.Kind() == '"' {
			value := tok.String()
			if masked := secrets.Redact(value); masked != value {
				// Delimiters and whitespace hold no quote, so the token's
				// own opening quote is the first one after checked.
				start := checked + strings.IndexByte(raw[checked:end], '"')
				out.WriteString(raw[copied:start])
				quoted, _ := jsontext.AppendQuote(nil, masked)
				out.Write(quoted)
				copied = end
			}
		}
		checked = end
	}
	out.WriteString(raw[copied:checked])
	return out.String(), true
}

// --- get_recent_edits ---

type recentEditsIn struct {
	Path    string `json:"path,omitempty" jsonschema:"Case-insensitive file path substring, e.g. internal/mcp/server.go. Omit to list every recently edited file."`
	Project string `json:"project,omitempty" jsonschema:"Restrict to one project."`
	Limit   int    `json:"limit,omitempty" jsonschema:"Max files, default 10, max 50."`
	Cursor  int    `json:"cursor,omitempty" jsonschema:"Pagination cursor from a previous next_cursor."`
}

type recentEditsOut struct {
	Files      []db.RecentEditFile `json:"files" jsonschema:"Edited files, most recent edit first. Each lists up to 20 of its newest edits with the session that made them."`
	NextCursor *int                `json:"next_cursor,omitempty"`
}

func (t *toolset) recentEdits(
	ctx context.Context, _ *mcp.CallToolRequest, in recentEditsIn,
) (*mcp.CallToolResult, recentEditsOut, error) {
	if in.Cursor < 0 {
		return nil, recentEditsOut{}, errors.New("cursor must not be negative")
	}
	if utf8.RuneCountInString(in.Path) > maxRecentEditsPathLength {
		return nil, recentEditsOut{}, fmt.Errorf("path must be at most %d characters", maxRecentEditsPathLength)
	}
	res, err := t.svc.RecentEdits(ctx, service.RecentEditsFilter{
		Project: in.Project,
		Search:  in.Path,
		Limit:   clampLimit(in.Limit, defaultRecentEditsLimit, maxRecentEditsLimit),
		Offset:  in.Cursor,
	})
	if err != nil {
		return nil, recentEditsOut{}, err
	}
	out := recentEditsOut{Files: res.Files}
	if out.Files == nil {
		out.Files = []db.RecentEditFile{}
	}
	if res.HasMore {
		next := in.Cursor + len(res.Files)
		out.NextCursor = &next
	}
	return nil, out, nil
}

// --- get_child_sessions ---

type childSessionsIn struct {
	SessionID string `json:"session_id" jsonschema:"A full stored session ID or a raw UUID that matches one session."`
}

type childSession struct {
	sessionRow   `json:",inline"`
	Relationship string `json:"relationship,omitempty" jsonschema:"How the child relates to its parent, such as subagent or fork."`
}

type childSessionsOut struct {
	Sessions []childSession `json:"sessions"`
}

func (t *toolset) childSessions(
	ctx context.Context, _ *mcp.CallToolRequest, in childSessionsIn,
) (*mcp.CallToolResult, childSessionsOut, error) {
	detail, err := t.resolveSession(ctx, in.SessionID)
	if err != nil {
		return nil, childSessionsOut{}, err
	}
	children, err := t.svc.ChildSessions(ctx, detail.ID)
	if err != nil {
		return nil, childSessionsOut{}, err
	}
	out := childSessionsOut{Sessions: make([]childSession, 0, len(children))}
	for _, s := range children {
		out.Sessions = append(out.Sessions, childSession{
			sessionRow: toSessionRow(s), Relationship: s.RelationshipType,
		})
	}
	return nil, out, nil
}
