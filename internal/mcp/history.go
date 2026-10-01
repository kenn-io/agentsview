// ABOUTME: Read-only MCP tools for one session's details: its tool calls,
// ABOUTME: the files sessions edited, and its child sessions.
package mcp

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/secrets"
	"go.kenn.io/agentsview/internal/service"
)

const (
	defaultToolCallLimit     = 50
	maxToolCallLimit         = 200
	defaultToolInputChars    = 500
	maxToolInputChars        = 10000
	defaultRecentEditsLimit  = 10
	maxRecentEditsLimit      = 50
	maxRecentEditsPathLength = 1024
	// redactSlackChars is how far past max_input_chars a tool input is
	// scanned for secrets, longer than any secret the scanner recognizes.
	redactSlackChars = 8192
)

// --- get_tool_calls ---

type toolCallsIn struct {
	SessionID     string `json:"session_id" jsonschema:"Full stored session ID."`
	Category      string `json:"category,omitempty" jsonschema:"Only calls in this category: Read, Edit, Write, Bash, Grep, Glob, Task, Tool, or Other."`
	Limit         int    `json:"limit,omitempty" jsonschema:"Max tool calls, default 50, max 200."`
	Cursor        int    `json:"cursor,omitempty" jsonschema:"Pagination cursor from a previous next_cursor."`
	MaxInputChars int    `json:"max_input_chars,omitempty" jsonschema:"Truncate each tool input to this many characters, default 500, max 10000."`
}

type toolCallOut struct {
	Ordinal           int    `json:"ordinal" jsonschema:"Ordinal of the assistant message that made the call; pass it to get_messages."`
	Timestamp         string `json:"timestamp,omitempty"`
	ToolName          string `json:"tool_name"`
	Category          string `json:"category"`
	Input             string `json:"input" jsonschema:"Tool input JSON with secret-shaped values masked."`
	InputTruncated    bool   `json:"input_truncated,omitempty"`
	SkillName         string `json:"skill_name,omitempty"`
	SubagentSessionID string `json:"subagent_session_id,omitempty"`
	ResultLength      int    `json:"result_length" jsonschema:"Size of the tool result in bytes."`
}

type toolCallsOut struct {
	ToolCalls  []toolCallOut `json:"tool_calls"`
	Total      int           `json:"total" jsonschema:"Calls matching the category filter across the whole session."`
	NextCursor *int          `json:"next_cursor,omitempty"`
}

func (t *toolset) toolCalls(
	ctx context.Context, _ *mcp.CallToolRequest, in toolCallsIn,
) (*mcp.CallToolResult, toolCallsOut, error) {
	if in.Cursor < 0 {
		return nil, toolCallsOut{}, errors.New("cursor must not be negative")
	}
	list, err := t.svc.ToolCalls(ctx, in.SessionID)
	if err != nil {
		return nil, toolCallsOut{}, err
	}
	if len(list.ToolCalls) == 0 {
		// An unknown session also has no tool calls; tell the two apart.
		detail, err := t.svc.Get(ctx, in.SessionID)
		if err != nil {
			return nil, toolCallsOut{}, err
		}
		if detail == nil {
			return nil, toolCallsOut{}, fmt.Errorf("session not found: %s", in.SessionID)
		}
	}
	matching := list.ToolCalls
	if in.Category != "" {
		matching = make([]service.ToolCall, 0, len(list.ToolCalls))
		for _, tc := range list.ToolCalls {
			if strings.EqualFold(tc.Category, in.Category) {
				matching = append(matching, tc)
			}
		}
	}
	limit := clampLimit(in.Limit, defaultToolCallLimit, maxToolCallLimit)
	maxChars := clampLimit(in.MaxInputChars, defaultToolInputChars, maxToolInputChars)
	start := min(in.Cursor, len(matching))
	end := min(start+limit, len(matching))
	out := toolCallsOut{
		ToolCalls: make([]toolCallOut, 0, end-start),
		Total:     len(matching),
	}
	for _, tc := range matching[start:end] {
		input, partial := redactToolInput(tc.InputJSON, maxChars)
		input, cut := truncate(input, maxChars)
		cut = cut || partial
		out.ToolCalls = append(out.ToolCalls, toolCallOut{
			Ordinal: tc.Ordinal, Timestamp: tc.Timestamp,
			ToolName: tc.ToolName, Category: tc.Category,
			Input: input, InputTruncated: cut,
			SkillName: tc.SkillName, SubagentSessionID: tc.SubagentSessionID,
			ResultLength: tc.ResultLength,
		})
	}
	if end < len(matching) {
		out.NextCursor = &end
	}
	return nil, out, nil
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
	SessionID string `json:"session_id" jsonschema:"Full stored ID of the parent session."`
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
	children, err := t.svc.ChildSessions(ctx, in.SessionID)
	if err != nil {
		return nil, childSessionsOut{}, err
	}
	if len(children) == 0 {
		detail, err := t.svc.Get(ctx, in.SessionID)
		if err != nil {
			return nil, childSessionsOut{}, err
		}
		if detail == nil {
			return nil, childSessionsOut{}, fmt.Errorf("session not found: %s", in.SessionID)
		}
	}
	out := childSessionsOut{Sessions: make([]childSession, 0, len(children))}
	for _, s := range children {
		out.Sessions = append(out.Sessions, childSession{
			sessionRow: toSessionRow(s), Relationship: s.RelationshipType,
		})
	}
	return nil, out, nil
}
