package parser

import (
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"regexp"
	"strings"

	"mvdan.cc/sh/v3/syntax"

	"go.kenn.io/agentsview/internal/shellquote"
)

// OneShotRunArgs reports whether argv starts a one-shot agent run.
func OneShotRunArgs(agent string, args []string) bool {
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return false
		}
		if agent == "codex" && (arg == "exec" || arg == "e") || agent == "claude" && (arg == "-p" || arg == "--print") {
			return true
		}
		if !strings.HasPrefix(arg, "-") {
			if agent == "codex" {
				return false
			}
			continue
		}
		if !strings.Contains(arg, "=") && launchedFlagTakesValue(agent, arg) {
			i++
		}
	}
	return false
}

func launchedFlagTakesValue(agent, flag string) bool {
	if agent == "codex" {
		switch flag {
		case "-C", "--cd", "-c", "--config", "-m", "--model", "-p", "--profile", "-s", "--sandbox", "-a", "--ask-for-approval", "-i", "--image", "--add-dir", "--enable", "--disable", "--remote", "--remote-auth-token-env", "--local-provider", "--output-schema", "-o", "--output-last-message":
			return true
		}
	} else {
		switch flag {
		case "--model", "--add-dir", "--append-system-prompt", "--append-system-prompt-file", "--system-prompt", "--system-prompt-file", "--agent", "--agents", "--settings", "--setting-sources", "--session-id", "--permission-mode", "--output-format", "--input-format", "--json-schema", "--effort", "--fallback-model", "--name", "-n", "--max-budget-usd", "--mcp-config", "--plugin-dir", "--tools", "--allowedTools", "--allowed-tools", "--disallowedTools", "--disallowed-tools":
			return true
		}
	}
	return false
}

var launchedUUID = `[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`
var launchedOutputID = regexp.MustCompile(`(?im)^session id:[ \t]*(` + launchedUUID + `)[ \t]*\r?$`)
var launchedValidID = regexp.MustCompile(`(?i)^` + launchedUUID + `$`)
var launchedOutputHint = regexp.MustCompile(`(?i)session id:|session_id|thread_id`)

// IsLaunchedClaudeSessionID reports whether id can name a launched Claude session.
func IsLaunchedClaudeSessionID(id string) bool { return launchedValidID.MatchString(id) }

// LaunchedRunSessionIDs extracts unambiguous IDs from starting shell calls.
func LaunchedRunSessionIDs(ctx context.Context, commands []string, output string) []string {
	ids := map[string]bool{}
	var launches []string
	unsafeOutput := false
	conflictingExplicit := false
	var scan func(string, int)
	scan = func(command string, depth int) {
		if depth > 8 || ctx.Err() != nil {
			return
		}
		command = strings.TrimSpace(command)
		if strings.HasPrefix(command, "& ") {
			command = strings.TrimSpace(command[2:])
		}
		file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
		if err != nil {
			return
		}
		syntax.Walk(file, func(node syntax.Node) bool {
			if _, ok := node.(*syntax.FuncDecl); ok {
				unsafeOutput = true
				return false
			}
			if stmt, ok := node.(*syntax.Stmt); ok && (len(stmt.Redirs) > 0 || stmt.Background) {
				unsafeOutput = true
			}
			call, ok := node.(*syntax.CallExpr)
			if !ok {
				return true
			}
			args, _ := unwrapPRCommand(ctx, call.Args)
			if inner, ok := shellCommand(args); ok {
				scan(inner, depth+1)
				return false
			}
			words, literal := shellWords(args)
			if len(words) == 0 || !literal[0] {
				unsafeOutput = true
				return false
			}
			name := strings.ToLower(strings.ReplaceAll(words[0], `\`, "/"))
			name = name[strings.LastIndex(name, "/")+1:]
			name = strings.TrimSuffix(name, ".exe")
			if name != "codex" && name != "claude" || !OneShotRunArgs(name, words) {
				// A literal cd before the launch is setup; other commands may
				// contribute an unrelated ID to the combined shell output.
				raw, rawLiteral := shellWords(call.Args)
				if len(call.Assigns) != 0 || len(raw) == 0 || len(raw) > 2 || raw[0] != "cd" || !rawLiteral[0] || len(launches) != 0 || len(raw) == 2 && (!rawLiteral[1] || strings.HasPrefix(raw[1], "-") || strings.ContainsAny(raw[1], "\r\n")) {
					unsafeOutput = true
				}
				return false
			}
			if isResumedRun(name, words) {
				unsafeOutput = true
				return false
			}
			syntax.Walk(call, func(node syntax.Node) bool {
				switch node.(type) {
				case *syntax.CmdSubst, *syntax.ProcSubst:
					unsafeOutput = true
				}
				return true
			})
			prefix := ""
			if name == "codex" {
				prefix = "codex:"
			}
			explicit := map[string]bool{}
			for i := 1; i < len(words); i++ {
				if !literal[i] {
					continue
				}
				id := ""
				if strings.HasPrefix(words[i], "--session-id=") {
					id = strings.TrimPrefix(words[i], "--session-id=")
				} else if words[i] == "--session-id" && i+1 < len(words) && literal[i+1] {
					id = words[i+1]
				}
				if launchedValidID.MatchString(id) {
					explicit[prefix+strings.ToLower(id)] = true
				}
			}
			if len(explicit) > 1 {
				conflictingExplicit = true
			}
			if len(explicit) == 1 {
				for id := range explicit {
					ids[id] = true
				}
			}
			launches = append(launches, prefix)
			return false
		})
	}
	for _, command := range commands {
		scan(command, 0)
	}
	if len(launches) == 1 && !unsafeOutput && !conflictingExplicit {
		for _, id := range launchedOutputIDs(output) {
			if strings.HasPrefix(id, "codex:") == (launches[0] == "codex:") {
				ids[id] = true
			}
		}
	}
	if len(launches) != 1 || len(ids) != 1 || conflictingExplicit {
		return nil
	}
	var result []string
	for id := range ids {
		result = append(result, id)
	}
	return result
}

// isResumedRun checks control arguments, not words in the prompt.
func isResumedRun(agent string, words []string) bool {
	if agent == "codex" {
		for i := 1; i < len(words); i++ {
			if words[i] != "exec" && words[i] != "e" {
				if words[i] == "--" || !strings.HasPrefix(words[i], "-") {
					return false
				}
				if launchedFlagTakesValue(agent, words[i]) {
					i++
				}
				continue
			}
			for i++; i < len(words); i++ {
				arg := words[i]
				if arg == "--" {
					return false
				}
				if arg == "resume" || arg == "--resume" || arg == "--continue" {
					return true
				}
				if !strings.HasPrefix(arg, "-") {
					return false
				}
				if launchedFlagTakesValue(agent, arg) {
					i++
				}
			}
			return false
		}
		return false
	}
	for i := 1; i < len(words); i++ {
		word := words[i]
		if word == "--" {
			break
		}
		if word == "--resume" || word == "--continue" || word == "-r" || word == "-c" || strings.HasPrefix(word, "--resume=") || strings.HasPrefix(word, "--continue=") {
			return true
		}
		if launchedFlagTakesValue(agent, word) {
			i++
		}
	}
	return false
}

// launchedRunOutput unwraps the shell tool's output envelope.
func launchedRunOutput(text string) string {
	var envelope struct {
		Output string `json:"output"`
	}
	if json.Unmarshal([]byte(text), &envelope) == nil && envelope.Output != "" {
		return envelope.Output
	}
	return text
}

func launchedRunID(ctx context.Context, input, output string) string {
	commands, ok := ShellInputCommands(input)
	if !ok {
		return ""
	}
	ids := LaunchedRunSessionIDs(ctx, commands, launchedRunOutput(output))
	if len(ids) != 1 {
		return ""
	}
	return ids[0]
}

// ShellInputCommands decodes shell text and argv-list tool inputs.
func ShellInputCommands(input string) ([]string, bool) {
	if !jsontext.Value(input).IsValid() {
		return []string{input}, strings.TrimSpace(input) != ""
	}
	var payload struct {
		Command     jsontext.Value `json:"command"`
		CommandLine string         `json:"CommandLine"`
		Cmd         string         `json:"cmd"`
		Commands    []string       `json:"commands"`
	}
	if jsonv2.Unmarshal([]byte(input), &payload) != nil {
		return nil, false
	}
	if len(payload.Command) > 0 && string(payload.Command) != "null" {
		var command string
		if jsonv2.Unmarshal(payload.Command, &command) == nil {
			return []string{command}, true
		}
		var argv []string
		if jsonv2.Unmarshal(payload.Command, &argv) != nil || len(argv) == 0 {
			return nil, false
		}
		for i, arg := range argv {
			argv[i] = shellquote.POSIX(arg)
		}
		return []string{strings.Join(argv, " ")}, true
	}
	if payload.CommandLine != "" {
		return []string{payload.CommandLine}, true
	}
	if payload.Commands != nil {
		return payload.Commands, true
	}
	return []string{payload.Cmd}, true
}

func launchedOutputIDs(text string) []string {
	text = launchedRunOutput(text)
	var ids []string
	for _, match := range launchedOutputID.FindAllStringSubmatch(text, -1) {
		ids = append(ids, "codex:"+strings.ToLower(match[1]))
	}
	for _, line := range strings.Split(text, "\n") {
		if !strings.Contains(line, `"thread_id"`) && !strings.Contains(line, `"session_id"`) {
			continue
		}
		var event struct {
			Type      string `json:"type"`
			SessionID string `json:"session_id"`
			ThreadID  string `json:"thread_id"`
		}
		if json.Unmarshal([]byte(line), &event) != nil {
			continue
		}
		if event.Type == "thread.started" && launchedValidID.MatchString(event.ThreadID) {
			ids = append(ids, "codex:"+strings.ToLower(event.ThreadID))
		}
		if (event.Type == "system" || event.Type == "result") && launchedValidID.MatchString(event.SessionID) {
			ids = append(ids, strings.ToLower(event.SessionID))
		}
	}
	return ids
}

func hasLaunchedOutputID(text string) bool {
	return launchedOutputHint.MatchString(text) && len(launchedOutputIDs(text)) > 0
}

// annotateClaudeLaunchedRuns associates a Bash call with its own results.
func annotateClaudeLaunchedRuns(ctx context.Context, messages []ParsedMessage) error {
	calls := make(map[string]*ParsedToolCall)
	for i := range messages {
		if err := contextErrEvery(ctx, i); err != nil {
			return err
		}
		for j := range messages[i].ToolCalls {
			call := &messages[i].ToolCalls[j]
			if call.ToolUseID != "" {
				calls[call.ToolUseID] = call
			}
		}
	}
	results := make(map[string][]string)
	for i := range messages {
		if err := contextErrEvery(ctx, i); err != nil {
			return err
		}
		for _, result := range messages[i].ToolResults {
			call := calls[result.ToolUseID]
			if call == nil || call.Category != "Bash" {
				continue
			}
			lower := strings.ToLower(call.InputJSON)
			if strings.Contains(lower, "codex") || strings.Contains(lower, "claude") {
				results[result.ToolUseID] = append(results[result.ToolUseID], DecodeContent(result.ContentRaw))
			}
		}
	}
	for i := range messages {
		if err := contextErrEvery(ctx, i); err != nil {
			return err
		}
		for j := range messages[i].ToolCalls {
			call := &messages[i].ToolCalls[j]
			if call.Category != "Bash" || call.ToolUseID == "" || call.SubagentSessionID != "" {
				continue
			}
			lower := strings.ToLower(call.InputJSON)
			if !strings.Contains(lower, "codex") && !strings.Contains(lower, "claude") {
				continue
			}
			output := ""
			if calls[call.ToolUseID] == call {
				output = strings.Join(results[call.ToolUseID], "\n")
			}
			call.SubagentSessionID = launchedRunID(ctx, call.InputJSON, output)
		}
	}
	return ctx.Err()
}
