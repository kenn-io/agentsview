package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"go.kenn.io/agentsview/internal/parser"
)

type lookupItem struct {
	SessionID string `json:"session_id"`
}

type lookupCall struct {
	Route                        string
	Name                         string
	Input                        string
	Result                       string
	Start, End                   int64
	Source, StartOrder, EndOrder int
	Missing, Error               bool
}

type lookupScore struct {
	Verdict    string `json:"verdict"`
	CallsToHit *int   `json:"calls_to_hit"`
	MCP        int    `json:"mcp_calls"`
	CLI        int    `json:"cli_calls"`
	Direct     int    `json:"direct_calls"`
	Other      int    `json:"other_calls"`
	Errors     int    `json:"errors"`
}

var (
	lookupCommandPattern        = regexp.MustCompile(`(?i)(sqlite3|sessions\.db|\.claude(?:/projects)?(?:/|\b)|\.codex(?:/sessions)?(?:/|\b)|rollout-[^\s"']*\.jsonl|tool-results/|\bagentsview\s+(?:session|search|db|mcp)\b)`)
	lookupOutputPattern         = regexp.MustCompile(`(?i)(sessions\.db|\.claude(?:/projects)?(?:/|\b)|\.codex(?:/sessions)?(?:/|\b)|rollout-[^\s"']*\.jsonl|tool-results/)`)
	codexMCPInvocationPattern   = regexp.MustCompile(`\btools\s*(?:\.\s*mcp__agentsview__|\[)`)
	mcpErrorPattern             = regexp.MustCompile(`(?i)"is_?error"\s*:\s*true`)
	runningCellPattern          = regexp.MustCompile(`(?i)script (?:still )?running with cell ID\s+([^\s]+)`)
	sessionProviderPrefix       = regexp.MustCompile(`(?i)(?:([a-z][a-z0-9_-]*):|/sessions/([a-z][a-z0-9_-]*)/)$`)
	lookupJSONFence             = regexp.MustCompile("(?s)^\\s*```(?:json)?[ \\t]*\\r?\\n(.*?)\\r?\\n```\\s*$")
	errCaptureIncomplete        = errors.New("capture incomplete")
	errLookupCaptureUnsupported = errors.New("lookup capture format is unsupported")
	errLookupClientFailed       = errors.New("lookup client failed")
)

func routeFor(name, input string) string {
	if name == "ToolSearch" {
		if strings.Contains(strings.ToLower(input), "agentsview") {
			return "other"
		}
		return ""
	}
	if strings.Contains(name, "mcp__agentsview__") || strings.HasPrefix(name, "agentsview:") {
		return "mcp"
	}
	lower := strings.ToLower(input)
	if strings.Contains(lower, "agentsview ") {
		return "cli"
	}
	if lookupCommandPattern.MatchString(input) {
		return "direct"
	}
	if toolDescriptionLookup(lower) || name == "Read" || name == "Grep" || name == "Glob" || name == "Bash" || name == "exec_command" || name == "CommandExecution" {
		return "other"
	}
	return ""
}

func rawText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func resultText(v any) string {
	var parts []string
	var visit func(any)
	visit = func(value any) {
		switch value := value.(type) {
		case string:
			parts = append(parts, value)
		case []any:
			for _, child := range value {
				visit(child)
			}
		case map[string]any:
			parts = append(parts, rawText(value))
			for _, child := range value {
				visit(child)
			}
		}
	}
	visit(v)
	return strings.Join(parts, "\n")
}

func timestampMS(value any) int64 {
	s, _ := value.(string)
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

func extractLookupCalls(ctx context.Context, client clientName, transcripts [][]byte) ([]lookupCall, error) {
	var calls []lookupCall
	for source, data := range transcripts {
		var events []map[string]any
		scanErr := scanJSONLEvents(data, 32*1024*1024, false, func(event map[string]any) { events = append(events, event) })
		var sourceCalls []lookupCall
		if client == clientClaude {
			var err error
			sourceCalls, err = claudeLookupCalls(ctx, events)
			if err != nil {
				return calls, fmt.Errorf("decode source %d: %w", source+1, err)
			}
		} else {
			sourceCalls = codexLookupCalls(ctx, events)
		}
		for i := range sourceCalls {
			sourceCalls[i].Source = source + 1
		}
		calls = append(calls, sourceCalls...)
		if scanErr != nil {
			return calls, fmt.Errorf("decode source %d: %w", source+1, scanErr)
		}
	}
	sort.SliceStable(calls, func(i, j int) bool { return calls[i].Start < calls[j].Start })
	return calls, nil
}

func claudeLookupCalls(ctx context.Context, events []map[string]any) ([]lookupCall, error) {
	calls := make([]lookupCall, 0)
	pending := map[string]int{}
	for line, e := range events {
		if e["type"] == "api-request-blob" {
			continue
		}
		msg, _ := e["message"].(map[string]any)
		blocks, _ := msg["content"].([]any)
		content, err := json.Marshal(blocks)
		if err != nil {
			return calls, err
		}
		_, _, _, _, _, toolUses, toolResults := parser.ExtractTextContent(
			parser.WithoutFilesystemProjectDiscovery(ctx),
			gjson.ParseBytes(content),
		)
		at := timestampMS(e["timestamp"])
		for _, toolUse := range toolUses {
			name := toolUse.ToolName
			route := routeFor(name, toolUse.InputJSON)
			if route == "" {
				continue
			}
			if name == "ToolSearch" {
				name = "tool discovery"
			}
			pending[toolUse.ToolUseID] = len(calls)
			calls = append(calls, lookupCall{Name: name, Input: toolUse.InputJSON, Route: route, Start: at, End: at, StartOrder: line + 1, Missing: true})
		}
		for _, toolResult := range toolResults {
			idx, ok := pending[toolResult.ToolUseID]
			if !ok {
				continue
			}
			c := &calls[idx]
			c.Result = lookupResultText(toolResult.ContentRaw)
			c.Missing = toolResult.ContentRaw == "" || toolResult.ContentRaw == "null"
			c.End = at
			c.EndOrder = line + 1
			c.Error = toolResult.Status == "errored"
			upgradeLookupRoute(c)
		}
	}
	return calls, nil
}

func lookupResultText(raw string) string {
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return raw
	}
	return resultText(value)
}

func codexLookupCalls(ctx context.Context, events []map[string]any) []lookupCall {
	calls := make([]lookupCall, 0)
	type script struct {
		start, end           int64
		input                string
		mcp, commands        int
		cell                 string
		startOrder, endOrder int
	}
	scripts := make([]script, 0)
	pending := map[string]int{}
	pendingCalls := map[string]int{}
	pendingWaits := map[string]int{}
	for line, e := range events {
		p, _ := e["payload"].(map[string]any)
		kind, _ := p["type"].(string)
		at := timestampMS(e["timestamp"])
		if e["type"] == "response_item" {
			name, _ := p["name"].(string)
			id, _ := p["call_id"].(string)
			if (kind == "function_call" || kind == "custom_tool_call") && name == "exec" {
				scripts = append(scripts, script{start: at, startOrder: line + 1, input: rawText(p["arguments"]) + rawText(p["input"])})
				pending[id] = len(scripts) - 1
			} else if (kind == "function_call" || kind == "custom_tool_call") && name == "wait" {
				var args struct {
					CellID string `json:"cell_id"`
				}
				if json.Unmarshal([]byte(rawText(p["arguments"])), &args) == nil {
					for i := len(scripts) - 1; i >= 0; i-- {
						if args.CellID != "" && scripts[i].cell == args.CellID {
							pendingWaits[id] = i
							scripts[i].end = 0
							break
						}
					}
				}
			} else if kind == "function_call" || kind == "custom_tool_call" {
				route := routeFor(name, rawText(p["arguments"])+rawText(p["input"]))
				if route != "" && name != "wait" {
					pendingCalls[id] = len(calls)
					calls = append(calls, lookupCall{Name: name, Input: rawText(p["arguments"]) + rawText(p["input"]), Route: route, Start: at, End: at, StartOrder: line + 1, Missing: true})
				}
			} else if kind == "function_call_output" || kind == "custom_tool_call_output" {
				if i, ok := pending[id]; ok {
					scripts[i].endOrder = line + 1
					if match := runningCellPattern.FindStringSubmatch(resultText(p["output"])); len(match) == 2 {
						scripts[i].cell = match[1]
					} else {
						scripts[i].end = at
					}
				}
				if i, ok := pendingWaits[id]; ok {
					scripts[i].endOrder = line + 1
					if runningCellPattern.MatchString(resultText(p["output"])) {
						scripts[i].end = 0
					} else {
						scripts[i].end = at
						scripts[i].cell = ""
					}
				}
				if i, ok := pendingCalls[id]; ok {
					calls[i].Result = resultText(p["output"])
					calls[i].Missing = p["output"] == nil
					calls[i].End = at
					calls[i].EndOrder = line + 1
					status, _ := p["status"].(string)
					calls[i].Error = status == "failed" || (calls[i].Route == "mcp" && mcpErrorPattern.MatchString(calls[i].Result))
					upgradeLookupRoute(&calls[i])
				}
			}
		}
		if e["type"] != "event_msg" || kind != "item_completed" {
			continue
		}
		item, _ := p["item"].(map[string]any)
		typ, _ := item["type"].(string)
		var c lookupCall
		c.Start = numberAsInt64(p["started_at_ms"])
		c.End = numberAsInt64(p["completed_at_ms"])
		c.EndOrder = line + 1
		parent := -1
		for i := range slices.Backward(scripts) {
			if c.Start > 0 && scripts[i].start <= c.Start && (scripts[i].end == 0 || scripts[i].end >= c.Start) {
				parent = i
				break
			}
		}
		if parent < 0 {
			continue
		}
		switch typ {
		case "McpToolCall":
			server, _ := item["server"].(string)
			tool, _ := item["tool"].(string)
			if server != "agentsview" {
				continue
			}
			c.Name = tool
			c.Input = rawText(item["arguments"])
			c.Route = "mcp"
			c.Result = resultText(item["result"])
			c.Missing = item["result"] == nil
			status, _ := item["status"].(string)
			c.Error = status == "failed" || item["error"] != nil
			if result, ok := item["result"].(map[string]any); ok {
				if isError, _ := result["isError"].(bool); isError {
					c.Error = true
				}
			}
		case "CommandExecution":
			c.Name = typ
			c.Input = rawText(item["command"])
			c.Route = routeFor(typ, rawText(item["command"]))
			c.Result = rawText(item["aggregated_output"])
			c.Missing = item["aggregated_output"] == nil
			if code, ok := item["exit_code"].(float64); ok {
				c.Error = code != 0
			}
		default:
			continue
		}
		upgradeLookupRoute(&c)
		calls = append(calls, c)
		if c.Route == "mcp" {
			scripts[parent].mcp++
		} else {
			scripts[parent].commands++
		}
	}
	for _, s := range scripts {
		if (codexMCPInvocationPattern.MatchString(s.input) && s.mcp == 0) || ((lookupCommandPattern.MatchString(s.input) || strings.Contains(s.input, "exec_command")) && s.commands == 0) {
			calls = append(calls, lookupCall{Name: "exec", Route: "other", Start: s.start, End: s.end, StartOrder: s.startOrder, EndOrder: s.endOrder, Missing: true})
		}
		if strings.Contains(s.input, "ALL_TOOLS") || strings.Contains(s.input, "Object.keys(tools)") {
			calls = append(calls, lookupCall{Name: "tool discovery", Route: "other", Start: s.start, End: s.end, StartOrder: s.startOrder, EndOrder: s.endOrder, Missing: s.end == 0})
		}
	}
	return calls
}

func upgradeLookupRoute(c *lookupCall) {
	if c.Route == "other" && lookupOutputPattern.MatchString(c.Result) {
		c.Route = "direct"
	}
}

func scoreLookupQuestion(q lookupQuestion, answer string, calls []lookupCall, runErr error) lookupScore {
	s := lookupScore{Verdict: "failed"}
	if calls == nil {
		calls = make([]lookupCall, 0)
	}
	for i := range calls {
		switch calls[i].Route {
		case "mcp":
			s.MCP++
		case "cli":
			s.CLI++
		case "direct":
			s.Direct++
		default:
			s.Other++
		}
		if calls[i].Error {
			s.Errors++
		}
	}

	if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) || errors.Is(runErr, errLookupClientFailed) {
		return s
	}
	if errors.Is(runErr, errLookupCaptureUnsupported) {
		s.Verdict = "unscored"
		return s
	}
	sourceID, valid := lookupAnswerSource(q, answer)
	captureIncomplete := lookupCaptureIncomplete(runErr)
	if !valid || (runErr != nil && !captureIncomplete) {
		return s
	}
	if captureIncomplete {
		s.Verdict = "unscored"
		return s
	}

	ordered := make([]int, len(calls))
	for i := range ordered {
		ordered[i] = i
	}
	sort.SliceStable(ordered, func(i, j int) bool { return calls[ordered[i]].End < calls[ordered[j]].End })
	var hit *lookupCall
	hitIndex := -1
	for _, index := range ordered {
		c := &calls[index]
		if c.Error || c.Missing || strings.TrimSpace(c.Result) == "" {
			continue
		}
		name := strings.TrimPrefix(strings.TrimPrefix(c.Name, "mcp__agentsview__"), "agentsview:")
		sessionRead := c.Route == "cli" || c.Route == "direct" || (c.Route == "mcp" && strings.HasPrefix(name, "get_"))
		if containsSessionID(c.Result, sourceID) || (sessionRead && containsSessionID(c.Input, sourceID)) {
			hit, hitIndex = c, index
			break
		}
	}
	if hit == nil {
		if !lookupCallsComplete(calls) {
			s.Verdict = "unscored"
		}
		return s
	}
	if hit.Start <= 0 || hit.End < hit.Start {
		s.Verdict = "unscored"
		return s
	}
	count := 0
	for i, c := range calls {
		lower := strings.ToLower(c.Input)
		if c.Route == "other" && i != hitIndex && c.Name != "tool discovery" && c.Name != "exec" && !toolDescriptionLookup(lower) {
			continue
		}
		if c.Start > hit.End {
			continue
		}
		if c.Start <= 0 || c.End < c.Start {
			if c.Source != 0 && c.Source == hit.Source && c.StartOrder > 0 && hit.EndOrder > 0 && c.StartOrder > hit.EndOrder {
				continue
			}
			s.Verdict = "unscored"
			return s
		}
		count++
		if c.Missing {
			s.Verdict = "unscored"
			return s
		}
	}
	s.Verdict = "success"
	s.CallsToHit = &count
	return s
}

func lookupAnswerSource(q lookupQuestion, answer string) (string, bool) {
	if len(q.ExpectedAnswer) == 0 {
		return "", false
	}
	var values map[string]string
	if err := json.Unmarshal([]byte(lookupJSONFence.ReplaceAllString(answer, "$1")), &values); err != nil || len(values) != len(q.ExpectedAnswer)+1 {
		return "", false
	}
	sourceID, ok := values["source_session_id"]
	if !ok {
		return "", false
	}
	matchedSource := ""
	for _, item := range q.Expected {
		_, raw, prefixed := strings.Cut(item.SessionID, ":")
		if sourceID == item.SessionID || (prefixed && sourceID == raw) {
			if matchedSource != "" {
				return "", false
			}
			matchedSource = item.SessionID
		}
	}
	if matchedSource == "" {
		return "", false
	}
	for key, expected := range q.ExpectedAnswer {
		if !equalLookupAnswer(values[key], expected) {
			return "", false
		}
	}
	return matchedSource, true
}

func equalLookupAnswer(value, expected string) bool {
	value = strings.ToLower(strings.Join(strings.Fields(value), " "))
	expected = strings.ToLower(strings.Join(strings.Fields(expected), " "))
	return value != "" && value == expected
}

func containsSessionID(text, sessionID string) bool {
	text, sessionID = strings.ToLower(text), strings.ToLower(sessionID)
	provider, id, prefixed := strings.Cut(sessionID, ":")
	if !prefixed {
		id, provider = sessionID, ""
	}
	if id == "" {
		return false
	}
	for start := 0; start < len(text); {
		offset := strings.Index(text[start:], id)
		if offset < 0 {
			return false
		}
		at := start + offset
		end := at + len(id)
		if end < len(text) && sessionIDContinuation(text[end]) {
			start = end
			continue
		}
		prefix := sessionProviderPrefix.FindStringSubmatch(text[:at])
		if len(prefix) == 0 {
			if at == 0 || !asciiAlphaNumeric(text[at-1]) {
				return true
			}
		} else if provider == "" || prefix[1] == provider || prefix[2] == provider {
			return true
		}
		start = end
	}
	return false
}

func sessionIDContinuation(char byte) bool {
	return asciiAlphaNumeric(char) || char == '-' || char == '_'
}

func asciiAlphaNumeric(char byte) bool {
	return char >= 'a' && char <= 'z' || char >= '0' && char <= '9'
}

func lookupCallsComplete(calls []lookupCall) bool {
	for _, call := range calls {
		if call.Missing || call.Start <= 0 || call.End < call.Start {
			return false
		}
	}
	return true
}

func lookupCaptureIncomplete(runErr error) bool {
	return errors.Is(runErr, errCaptureIncomplete) &&
		!errors.Is(runErr, context.Canceled) &&
		!errors.Is(runErr, context.DeadlineExceeded) &&
		!errors.Is(runErr, errLookupClientFailed)
}

func toolDescriptionLookup(input string) bool {
	return strings.Contains(input, "tool") &&
		(strings.Contains(input, "description") || strings.Contains(input, "all_tools"))
}
