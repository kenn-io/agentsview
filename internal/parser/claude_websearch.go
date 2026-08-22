// ABOUTME: credits Claude assistant messages with the number of billed
// ABOUTME: server-side web searches their WebSearch tool calls performed.
package parser

import (
	"context"
	"encoding/json/jsontext"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

// claudeWebSearchToolName is Claude Code's built-in web search tool. Only
// that tool's results are treated as billed server-side searches; an MCP
// tool that happens to report a searchCount is not Anthropic spend.
const claudeWebSearchToolName = "WebSearch"

// usageWebSearchRequestsPath is where the Anthropic API reports how many
// server-side web searches a response billed.
const usageWebSearchRequestsPath = "server_tool_use.web_search_requests"

// annotateClaudeWebSearchRequests makes every assistant message's stored
// usage blob report the number of web searches it was billed for.
//
// Anthropic charges a flat fee per server-side web search on top of tokens,
// and reports the count in usage.server_tool_use.web_search_requests. When a
// session drives the API directly that field is already correct and is left
// alone. Claude Code, however, runs the search in an out-of-band side call
// and writes web_search_requests: 0 on the assistant message that issued the
// WebSearch tool_use; the only surviving evidence is searchCount on the
// linked tool result. In that case the count is copied onto the issuing
// assistant message, which is the row that carries the turn's usage.
//
// The two sources are never combined: a message that already reports a
// nonzero counter is authoritative, so a search can only ever be counted
// once.
func annotateClaudeWebSearchRequests(
	messages []ParsedMessage, counts map[string]int,
) {
	_ = annotateClaudeWebSearchRequestsContext(
		context.Background(), messages, counts,
	)
}

func annotateClaudeWebSearchRequestsContext(
	ctx context.Context, messages []ParsedMessage, counts map[string]int,
) error {
	if len(counts) == 0 {
		return ctx.Err()
	}
	for i := range messages {
		if err := ctx.Err(); err != nil {
			return err
		}
		msg := &messages[i]
		if len(msg.TokenUsage) == 0 || len(msg.ToolCalls) == 0 {
			continue
		}
		if usageWebSearchRequests(string(msg.TokenUsage)) > 0 {
			continue
		}
		total := 0
		for _, tc := range msg.ToolCalls {
			if err := ctx.Err(); err != nil {
				return err
			}
			if tc.ToolName != claudeWebSearchToolName ||
				tc.ToolUseID == "" {
				continue
			}
			total += counts[tc.ToolUseID]
		}
		if total <= 0 {
			continue
		}
		updated, ok := setUsageWebSearchRequests(
			string(msg.TokenUsage), total)
		if !ok {
			continue
		}
		msg.TokenUsage = jsontext.Value(updated)
	}
	return ctx.Err()
}

// collectClaudeWebSearchCounts maps a WebSearch tool_use id to the number of
// searches its result reported. Records whose tool result cannot be
// identified unambiguously are skipped rather than guessed at, and the first
// count seen for an id wins so a redelivered line cannot inflate it.
func collectClaudeWebSearchCounts(entries []dagEntry) map[string]int {
	counts, _ := collectClaudeWebSearchCountsContext(
		context.Background(), entries,
	)
	return counts
}

func collectClaudeWebSearchCountsContext(
	ctx context.Context, entries []dagEntry,
) (map[string]int, error) {
	var counts map[string]int
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if e.entryType != "user" {
			continue
		}
		searchCount := gjson.Get(e.line, "toolUseResult.searchCount")
		if !searchCount.Exists() {
			continue
		}
		count := int(searchCount.Int())
		if count <= 0 {
			continue
		}
		id, ok, err := singleClaudeToolResultIDContext(ctx, e.line)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if _, seen := counts[id]; seen {
			continue
		}
		if counts == nil {
			counts = make(map[string]int)
		}
		counts[id] = count
	}
	return counts, ctx.Err()
}

// needsClaudeFullParseForWebSearchCounts reports whether an appended search
// result belongs to a tool call outside the appended batch. In that case the
// stored assistant row must be replaced so its usage can receive the billed
// search count.
func needsClaudeFullParseForWebSearchCounts(entries []dagEntry) bool {
	counts := collectClaudeWebSearchCounts(entries)
	if len(counts) == 0 {
		return false
	}
	appendedToolUseIDs := claudeAppendedToolUseIDs(entries)
	for toolUseID := range counts {
		if _, ok := appendedToolUseIDs[toolUseID]; !ok {
			return true
		}
	}
	return false
}

// singleClaudeToolResultIDContext returns the tool_use_id of a record's only
// tool_result block. Records with no tool result, or with more than one,
// report false: their toolUseResult cannot be attributed to one tool call.
func singleClaudeToolResultIDContext(
	ctx context.Context, line string,
) (string, bool, error) {
	content := gjson.Get(line, "message.content")
	if !content.IsArray() {
		return "", false, ctx.Err()
	}
	id := ""
	ambiguous := false
	var contextErr error
	content.ForEach(func(_, block gjson.Result) bool {
		if err := ctx.Err(); err != nil {
			contextErr = err
			return false
		}
		if block.Get("type").Str != "tool_result" {
			return true
		}
		if id != "" {
			ambiguous = true
			return false
		}
		id = block.Get("tool_use_id").Str
		return true
	})
	if contextErr != nil {
		return "", false, contextErr
	}
	if ambiguous || id == "" {
		return "", false, nil
	}
	return id, true, ctx.Err()
}

// usageWebSearchRequests reads the billed web search count out of a stored
// usage blob. Absent, malformed, or negative values read as zero.
func usageWebSearchRequests(usage string) int {
	value := gjson.Get(usage, usageWebSearchRequestsPath)
	if !value.Exists() {
		return 0
	}
	count := value.Int()
	if count <= 0 {
		return 0
	}
	return int(count)
}

// setUsageWebSearchRequests returns usage with
// server_tool_use.web_search_requests set to count, reporting false when the
// blob is not a JSON object it can edit in place.
//
// The edit is a splice rather than a decode-and-re-encode so the rest of the
// provider's usage object — including fields agentsview does not model, such
// as web_fetch_requests and the cache_creation breakdown — survives byte for
// byte.
func setUsageWebSearchRequests(usage string, count int) (string, bool) {
	trimmed := strings.TrimSpace(usage)
	if !gjson.Valid(trimmed) || !strings.HasPrefix(trimmed, "{") {
		return usage, false
	}
	value := strconv.Itoa(count)

	if existing := gjson.Get(
		trimmed, usageWebSearchRequestsPath,
	); existing.Exists() {
		if existing.Index <= 0 {
			return usage, false
		}
		return trimmed[:existing.Index] + value +
			trimmed[existing.Index+len(existing.Raw):], true
	}

	if serverToolUse := gjson.Get(
		trimmed, "server_tool_use",
	); serverToolUse.Exists() {
		if !serverToolUse.IsObject() || serverToolUse.Index <= 0 {
			return usage, false
		}
		raw := serverToolUse.Raw
		inner := strings.TrimSpace(raw[1 : len(raw)-1])
		replacement := `{"web_search_requests":` + value + `}`
		if inner != "" {
			replacement = `{"web_search_requests":` + value + `,` +
				inner + `}`
		}
		return trimmed[:serverToolUse.Index] + replacement +
			trimmed[serverToolUse.Index+len(raw):], true
	}

	inserted := `"server_tool_use":{"web_search_requests":` + value + `}`
	rest := strings.TrimSpace(trimmed[1:])
	if strings.HasPrefix(rest, "}") {
		return "{" + inserted + "}", true
	}
	return "{" + inserted + "," + trimmed[1:], true
}
