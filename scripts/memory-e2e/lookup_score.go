package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type lookupItem struct {
	SessionID string `json:"session_id"`
	Ordinal   *int   `json:"ordinal,omitempty"`
}

type lookupCall struct {
	Route                            string
	Name                             string
	Input                            string
	Result                           string
	Start, End                       int64
	Source, StartOrder, EndOrder     int
	Missing, Error, Empty, Truncated bool
	Nested                           bool
}

type lookupScore struct {
	Verdict        string `json:"verdict"`
	CallsToHit     *int   `json:"calls_to_hit"`
	SupplyingRoute string `json:"supplying_route,omitempty"`
	Route          string `json:"route"`
	MCP            int    `json:"mcp_calls"`
	CLI            int    `json:"cli_calls"`
	Direct         int    `json:"direct_calls"`
	Other          int    `json:"other_calls"`
	Errors         int    `json:"errors"`
	Empty          int    `json:"empty"`
	Truncated      int    `json:"truncated"`
	MissingCalls   int    `json:"missing_calls"`
}

var ordinalPattern = regexp.MustCompile(`(?i)(?:[?&]msg=|["']?(?:match_)?ordinal["']?\s*[:=]\s*|\b(?:message|ordinal)\s+)(\d+)`)
var lookupCommandPattern = regexp.MustCompile(`(?i)(sqlite3|sessions\.db|\.claude/projects/|\.codex/sessions/|rollout-[^\s"']*\.jsonl|tool-results/|\bagentsview\s+(?:session|search|db|mcp)\b)`)
var truncatedResultPattern = regexp.MustCompile(`(?i)"[^"\n]*truncated"\s*:\s*true|output truncated`)
var emptyResultPattern = regexp.MustCompile(`(?i)"(?:matches|messages|results|sessions|tool_calls)"\s*:\s*(?:\[\s*\]|null)`)
var codexMCPInvocationPattern = regexp.MustCompile(`\btools\s*(?:\.\s*mcp__agentsview__|\[)`)
var mcpErrorPattern = regexp.MustCompile(`(?i)"is_?error"\s*:\s*true`)
var runningCellPattern = regexp.MustCompile(`(?i)script (?:still )?running with cell ID\s+([^\s]+)`)
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

func decodeLines(data []byte) ([]map[string]any, error) {
	var out []map[string]any
	s := bufio.NewScanner(bytes.NewReader(data))
	s.Buffer(make([]byte, 64*1024), 32*1024*1024)
	for s.Scan() {
		if len(bytes.TrimSpace(s.Bytes())) == 0 {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal(s.Bytes(), &event); err != nil {
			return nil, err
		}
		out = append(out, event)
	}
	return out, s.Err()
}

func extractLookupCalls(client clientName, transcripts [][]byte) ([]lookupCall, error) {
	var calls []lookupCall
	for source, data := range transcripts {
		events, err := decodeLines(data)
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
		c.Nested = true
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
}

func lookupPreflightComplete(calls []lookupCall) bool {
	var search, read bool
	for _, c := range calls {
		if c.Route != "mcp" || c.Missing || c.Error || c.Empty || c.Result == "" || c.Start <= 0 || c.End < c.Start {
			continue
		}
		if c.Name == "search_content" || strings.HasSuffix(c.Name, "__search_content") {
			search = strings.Contains(c.Result, `"session_id"`)
		}
		if c.Name == "get_messages" || strings.HasSuffix(c.Name, "__get_messages") {
			read = strings.Contains(c.Result, `"ordinal"`) && strings.Contains(c.Result, `"content"`)
		}
	}
	return search && read
}

func lookupToolDeliveryMode(client clientName, calls []lookupCall) string {
	if client == clientClaude {
		return "native"
	}
	var direct, nested bool
	for _, c := range calls {
		if c.Route != "mcp" {
			continue
		}
		if c.Nested {
			nested = true
		} else {
			direct = true
		}
	}
	switch {
	case direct && nested:
		return "mixed"
	case nested:
		return "code-mode-only"
	case direct:
		return "direct"
	default:
		return "unknown"
	}
}

func containsItem(text string, item lookupItem) bool {
	id := strings.TrimPrefix(item.SessionID, "codex:")
	if id == "" || !strings.Contains(strings.ToLower(text), strings.ToLower(id)) {
		return false
	}
	if item.Ordinal == nil {
		return true
	}
	plain := strings.NewReplacer("`", "", "*", "", "~", "").Replace(text)
	for _, match := range ordinalPattern.FindAllStringSubmatch(plain, -1) {
		n, _ := strconv.Atoi(match[1])
		if n == *item.Ordinal {
			return true
		}
	}
	return false
}

func callContainsItem(call lookupCall, item lookupItem) bool {
	if containsItem(call.Result, item) {
		return true
	}
	if !containsSessionID(call.Input, item.SessionID) {
		return false
	}
	if item.Ordinal == nil {
		return true
	}
	return containsOrdinal(call.Result, *item.Ordinal)
}

func callContainsEvidence(call lookupCall, item lookupItem, phrases []string) bool {
	if !callContainsItem(call, item) || !containsAnswerEvidence(call.Result, phrases) {
		return false
	}
	if call.Route != "mcp" {
		return true
	}
	parsed := false
	for _, line := range strings.Split(call.Result, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var result any
		if json.Unmarshal([]byte(line), &result) != nil {
			continue
		}
		parsed = true
		if resultContainsMessageEvidence(result, containsSessionID(call.Input, item.SessionID), false, item, phrases) {
			return true
		}
	}
	return !parsed
}

func resultContainsMessageEvidence(
	value any, sessionMatched, ordinalMatched bool, item lookupItem, phrases []string,
) bool {
	switch value := value.(type) {
	case string:
		var nested any
		if json.Unmarshal([]byte(value), &nested) == nil {
			return resultContainsMessageEvidence(nested, sessionMatched, ordinalMatched, item, phrases)
		}
		sessionMatched = sessionMatched || containsSessionID(value, item.SessionID)
		ordinalMatched = ordinalMatched || item.Ordinal == nil || containsOrdinal(value, *item.Ordinal)
		return sessionMatched && ordinalMatched && containsAnswerEvidence(value, phrases)
	case []any:
		for _, child := range value {
			if resultContainsMessageEvidence(child, sessionMatched, ordinalMatched, item, phrases) {
				return true
			}
		}
	case map[string]any:
		for _, key := range []string{"session_id", "id"} {
			if id, ok := value[key].(string); ok {
				sessionMatched = containsSessionID(id, item.SessionID)
				break
			}
		}
		if item.Ordinal != nil {
			for _, key := range []string{"ordinal", "match_ordinal"} {
				if ordinal, ok := value[key]; ok {
					ordinalMatched = ordinalMatchesValue(ordinal, *item.Ordinal)
					break
				}
			}
		}
		if sessionMatched && (item.Ordinal == nil || ordinalMatched) && containsAnswerEvidence(directText(value), phrases) {
			return true
		}
		for _, child := range value {
			if resultContainsMessageEvidence(child, sessionMatched, ordinalMatched, item, phrases) {
				return true
			}
		}
	}
	return false
}

func ordinalMatchesValue(value any, expected int) bool {
	switch value := value.(type) {
	case float64:
		return int(value) == expected
	case int:
		return value == expected
	case string:
		ordinal, err := strconv.Atoi(value)
		return err == nil && ordinal == expected
	default:
		return false
	}
}

func directText(value map[string]any) string {
	var parts []string
	for key, child := range value {
		switch child := child.(type) {
		case string, float64, int, bool:
			parts = append(parts, key+":"+fmt.Sprint(child))
		}
	}
	return strings.Join(parts, " ")
}

func containsSessionID(text, sessionID string) bool {
	id := strings.TrimPrefix(sessionID, "codex:")
	return id != "" && strings.Contains(strings.ToLower(text), strings.ToLower(id))
}

func containsOrdinal(text string, ordinal int) bool {
	plain := strings.NewReplacer("`", "", "*", "", "~", "").Replace(text)
	for _, match := range ordinalPattern.FindAllStringSubmatch(plain, -1) {
		n, _ := strconv.Atoi(match[1])
		if n == ordinal {
			return true
		}
	}
	return false
}

func containsAnswerEvidence(text string, phrases []string) bool {
	if len(phrases) == 0 {
		return true
	}
	normalized := normalizePhrase(text)
	for _, phrase := range phrases {
		if phrase == "" || !strings.Contains(normalized, normalizePhrase(phrase)) {
			return false
		}
	}
	return true
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
		if !c.Error && !c.Missing && (hit == nil || c.End < hit.End) {
			for _, item := range cited {
				if callContainsEvidence(*c, item, phrases) {
					hit = c
					break
				}
			}
		}
	}
	s.Route = lookupRoute(calls)
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
	s.SupplyingRoute = hit.Route
	return s
}

func lookupRoute(calls []lookupCall) string {
	mcp, cli, direct := false, false, false
	first := ""
	for _, c := range calls {
		if c.Route == "other" {
			continue
		}
		if first == "" {
			first = c.Route
		}
		switch c.Route {
		case "mcp":
			mcp = true
		case "cli":
			cli = true
		case "direct":
			direct = true
		}
	}
	switch {
	case mcp && (cli || direct) && first == "mcp":
		return "fell-back"
	case mcp && (cli || direct) || cli && direct:
		return "mixed"
	case mcp:
		return "mcp-only"
	case cli:
		return "cli-only"
	case direct:
		return "direct-only"
	default:
		return "none"
	}
}

func scoreError(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprint(err)
}
