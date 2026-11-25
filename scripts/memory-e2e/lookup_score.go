package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

type lookupItem struct {
	SessionID string `json:"session_id"`
}

type lookupCall struct {
	Route                            string
	Name                             string
	Input                            string
	Result                           string
	Start, End                       int64
	Source, StartOrder, EndOrder     int
	Missing, Error, Empty, Truncated bool
}

type lookupScore struct {
	Verdict      string `json:"verdict"`
	CallsToHit   *int   `json:"calls_to_hit"`
	MCP          int    `json:"mcp_calls"`
	CLI          int    `json:"cli_calls"`
	Direct       int    `json:"direct_calls"`
	Other        int    `json:"other_calls"`
	Errors       int    `json:"errors"`
	Empty        int    `json:"empty"`
	Truncated    int    `json:"truncated"`
	MissingCalls int    `json:"missing_calls"`
}

var lookupCommandPattern = regexp.MustCompile(`(?i)(sqlite3|sessions\.db|\.claude/projects/|\.codex/sessions/|rollout-[^\s"']*\.jsonl|tool-results/|\bagentsview\s+(?:session|search|db|mcp)\b)`)
var lookupOutputPattern = regexp.MustCompile(`(?i)(sessions\.db|\.claude/projects/|\.codex/sessions/|rollout-[^\s"']*\.jsonl|tool-results/)`)
var truncatedResultPattern = regexp.MustCompile(`(?i)"[^"\n]*truncated"\s*:\s*true|output truncated`)
var emptyResultPattern = regexp.MustCompile(`(?i)"(?:matches|messages|results|sessions|tool_calls)"\s*:\s*(?:\[\s*\]|null)`)
var codexMCPInvocationPattern = regexp.MustCompile(`\btools\s*(?:\.\s*mcp__agentsview__|\[)`)
var mcpErrorPattern = regexp.MustCompile(`(?i)"is_?error"\s*:\s*true`)
var runningCellPattern = regexp.MustCompile(`(?i)script (?:still )?running with cell ID\s+([^\s]+)`)
var sessionProviderPrefix = regexp.MustCompile(`(?i)(?:([a-z][a-z0-9_-]*):|/sessions/([a-z][a-z0-9_-]*)/)$`)
var errCaptureIncomplete = errors.New("capture incomplete")
var errLookupClientFailed = errors.New("lookup client failed")

func routeFor(name, input string) string {
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
	if strings.Contains(lower, "tool") && (strings.Contains(lower, "description") || strings.Contains(lower, "all_tools")) {
		return "other"
	}
	if name == "Read" || name == "Grep" || name == "Glob" || name == "Bash" || name == "exec_command" || name == "CommandExecution" {
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
	if n, ok := value.(float64); ok {
		return int64(n)
	}
	s, _ := value.(string)
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

func extractLookupCalls(client clientName, transcripts [][]byte) ([]lookupCall, error) {
	var calls []lookupCall
	for source, data := range transcripts {
		var events []map[string]any
		err := scanJSONLEvents(data, 32*1024*1024, false, func(event map[string]any) { events = append(events, event) })
		var sourceCalls []lookupCall
		if client == clientClaude {
			sourceCalls = claudeLookupCalls(events)
		} else {
			sourceCalls = codexLookupCalls(events)
		}
		for i := range sourceCalls {
			sourceCalls[i].Source = source + 1
		}
		calls = append(calls, sourceCalls...)
		if err != nil {
			return calls, fmt.Errorf("decode source %d: %w", source+1, err)
		}
	}
	sort.SliceStable(calls, func(i, j int) bool { return calls[i].Start < calls[j].Start })
	return calls, nil
}

func claudeLookupCalls(events []map[string]any) []lookupCall {
	var calls []lookupCall
	pending := map[string]int{}
	for line, e := range events {
		if e["type"] == "api-request-blob" {
			continue
		}
		msg, _ := e["message"].(map[string]any)
		blocks, _ := msg["content"].([]any)
		at := timestampMS(e["timestamp"])
		for _, raw := range blocks {
			b, _ := raw.(map[string]any)
			switch b["type"] {
			case "tool_use":
				name, _ := b["name"].(string)
				id, _ := b["id"].(string)
				route := routeFor(name, rawText(b["input"]))
				if route == "" {
					continue
				}
				pending[id] = len(calls)
				calls = append(calls, lookupCall{Name: name, Input: rawText(b["input"]), Route: route, Start: at, End: at, StartOrder: line + 1, Missing: true})
			case "tool_result":
				id, _ := b["tool_use_id"].(string)
				idx, ok := pending[id]
				if !ok {
					continue
				}
				c := &calls[idx]
				c.Result = resultText(b["content"])
				c.Missing = b["content"] == nil
				c.End = at
				c.EndOrder = line + 1
				c.Error, _ = b["is_error"].(bool)
				resultFlags(c)
			}
		}
	}
	return calls
}

func codexLookupCalls(events []map[string]any) []lookupCall {
	var calls []lookupCall
	type script struct {
		start, end           int64
		input                string
		mcp, commands        int
		cell                 string
		startOrder, endOrder int
	}
	var scripts []script
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
					resultFlags(&calls[i])
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
		for i := len(scripts) - 1; i >= 0; i-- {
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
		resultFlags(&c)
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

func resultFlags(c *lookupCall) {
	c.Truncated = truncatedResultPattern.MatchString(c.Result)
	c.Empty = emptyResultPattern.MatchString(c.Result)
	if c.Route == "other" && lookupOutputPattern.MatchString(c.Result) {
		c.Route = "direct"
	}
}

func containsItem(text string, item lookupItem) bool {
	return containsSessionID(text, item.SessionID)
}

// lookupEvidence keeps search results for different sessions separate.
func lookupEvidence(call lookupCall, item lookupItem) string {
	if sessionReadMatches(call, item) || (call.Route == "direct" && strings.Contains(call.Result, `"type":"session_meta"`) && containsItem(call.Result, item)) {
		return call.Result
	}
	var evidence []string
	var visit func(any) bool
	visit = func(value any) bool {
		switch value := value.(type) {
		case string:
			var decoded any
			if json.Unmarshal([]byte(value), &decoded) == nil {
				return visit(decoded)
			}
			if containsItem(value, item) {
				evidence = append(evidence, value)
				return true
			}
		case []any:
			found := false
			for _, child := range value {
				found = visit(child) || found
			}
			return found
		case map[string]any:
			found := false
			for _, child := range value {
				if _, nested := child.(map[string]any); nested {
					found = visit(child) || found
				} else if _, nested := child.([]any); nested {
					found = visit(child) || found
				}
			}
			if !found && containsItem(rawText(value), item) {
				evidence = append(evidence, rawText(value))
				return true
			}
			return found
		}
		return false
	}
	for _, line := range strings.Split(call.Result, "\n") {
		var value any
		if json.Unmarshal([]byte(line), &value) == nil {
			visit(value)
		} else {
			visit(line)
		}
	}
	return strings.Join(evidence, "\n")
}

var ownerAnswerPattern = regexp.MustCompile(`(?i)\b(?:maple(?:\s+team)?\s+(?:now\s+|currently\s+)?(?:owns|took over|is (?:the |now the |currently the )?owner)|(?:owner|team)(?:\s+is|:)\s+(?:the\s+)?maple(?:\s+team)?)\b`)
var negatedOwnerPattern = regexp.MustCompile(`(?i)\b(?:not|never|neither|isn't|wasn't)\b[^.!?\n]*\bmaple\b|\bmaple\b[^.!?\n]*\b(?:does not|doesn't|is not|isn't|was not|wasn't|never)\b`)

func scoreLookupQuestion(q lookupQuestion, answer string, calls []lookupCall, runErr error) lookupScore {
	s := scoreLookup(answer, q.Expected, q.Phrases, calls, runErr)
	shortAnswer := answer
	for _, item := range q.Expected {
		shortAnswer = strings.ReplaceAll(shortAnswer, item.SessionID, "")
	}
	shortAnswer = normalizePhrase(shortAnswer)
	if q.Name == "owner" && strings.TrimSpace(answer) != "" && (negatedOwnerPattern.MatchString(answer) || (!ownerAnswerPattern.MatchString(answer) && shortAnswer != "maple" && shortAnswer != "maple team")) {
		s.Verdict = "failed"
		s.CallsToHit = nil
	}
	return s
}

func sessionReadMatches(call lookupCall, item lookupItem) bool {
	if call.Route == "direct" || call.Route == "cli" {
		return containsSessionID(call.Input, item.SessionID)
	}
	if call.Route != "mcp" {
		return false
	}
	for _, name := range []string{"get_messages", "get_tool_calls", "get_session_overview", "get_child_sessions"} {
		if call.Name == name || strings.HasSuffix(call.Name, "__"+name) {
			return containsSessionID(call.Input, item.SessionID)
		}
	}
	return false
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
		if end < len(text) && (text[end] >= 'a' && text[end] <= 'z' || text[end] >= '0' && text[end] <= '9' || text[end] == '-' || text[end] == '_') {
			start = end
			continue
		}
		prefix := sessionProviderPrefix.FindStringSubmatch(text[:at])
		if len(prefix) == 0 {
			if at == 0 || !(text[at-1] >= 'a' && text[at-1] <= 'z' || text[at-1] >= '0' && text[at-1] <= '9') {
				return true
			}
		} else if provider == "" || prefix[1] == provider || prefix[2] == provider {
			return true
		}
		start = end
	}
	return false
}

func scoreLookup(answer string, expected []lookupItem, phrases []string, calls []lookupCall, runErr error) lookupScore {
	s := lookupScore{Verdict: "failed"}
	captureIncomplete := errors.Is(runErr, errCaptureIncomplete) &&
		!errors.Is(runErr, context.Canceled) &&
		!errors.Is(runErr, context.DeadlineExceeded) &&
		!errors.Is(runErr, errLookupClientFailed)
	var cited []lookupItem
	for _, item := range expected {
		if containsItem(answer, item) {
			cited = append(cited, item)
		}
	}
	correct := (runErr == nil || captureIncomplete) && len(cited) > 0
	for _, phrase := range phrases {
		correct = correct && strings.Contains(normalizePhrase(answer), normalizePhrase(phrase))
	}
	var hit *lookupCall
	for i := range calls {
		c := &calls[i]
		if c.Missing {
			s.MissingCalls++
		}
		switch c.Route {
		case "mcp":
			s.MCP++
		case "cli":
			s.CLI++
		case "direct":
			s.Direct++
		default:
			s.Other++
		}
		if c.Error {
			s.Errors++
		}
		if c.Empty {
			s.Empty++
		}
		if c.Truncated {
			s.Truncated++
		}
	}
	ordered := make([]int, len(calls))
	for i := range ordered {
		ordered[i] = i
	}
	sort.SliceStable(ordered, func(i, j int) bool { return calls[ordered[i]].End < calls[ordered[j]].End })
	for _, item := range cited {
		var evidence strings.Builder
		for _, index := range ordered {
			c := &calls[index]
			if c.Error || c.Missing || c.Empty || strings.TrimSpace(c.Result) == "" {
				continue
			}
			part := lookupEvidence(*c, item)
			if part == "" {
				continue
			}
			evidence.WriteString(" ")
			evidence.WriteString(part)
			complete := true
			for _, phrase := range phrases {
				complete = complete && strings.Contains(normalizePhrase(evidence.String()), normalizePhrase(phrase))
			}
			if complete {
				if hit == nil || c.End < hit.End {
					hit = c
				}
				break
			}
		}
	}
	if captureIncomplete && strings.TrimSpace(answer) == "" {
		s.Verdict = "unscored"
		return s
	}
	if !correct {
		return s
	}
	if captureIncomplete {
		s.Verdict = "unscored"
		return s
	}
	if hit == nil {
		s.Verdict = "unscored"
		return s
	}
	if hit.Start <= 0 || hit.End < hit.Start {
		s.Verdict = "unscored"
		return s
	}
	count := 0
	for _, c := range calls {
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
		if c.Start <= hit.End {
			count++
			if c.Missing {
				s.Verdict = "unscored"
				return s
			}
		}
	}
	s.Verdict = "success"
	s.CallsToHit = &count
	return s
}

func scoreError(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprint(err)
}
