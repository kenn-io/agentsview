package parser

import (
	"context"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"path"
	"regexp"
	"slices"
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

var (
	launchedUUID       = `[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`
	launchedOutputID   = regexp.MustCompile(`(?im)\AOpenAI Codex v[^\r\n]*\r?\n--------\r?\n(?:[^\r\n]*\r?\n)*?session id:[ \t]*(` + launchedUUID + `)[ \t]*\r?\n--------(?:\r?\n|$)`)
	launchedValidID    = regexp.MustCompile(`(?i)^` + launchedUUID + `$`)
	launchedOutputHint = regexp.MustCompile(`(?i)session id:|session_id|thread_id`)
	launchedExitCode   = regexp.MustCompile(`(?m)^(?:Process exited with code |Exit code: )(-?[0-9]+)\r?$`)
)

// IsLaunchedClaudeSessionID reports whether id can name a launched Claude session.
func IsLaunchedClaudeSessionID(id string) bool { return launchedValidID.MatchString(id) }

// mentionsLauncher skips shell calls that cannot start a run.
func mentionsLauncher(input string) bool {
	lower := strings.ToLower(input)
	return strings.Contains(lower, "codex") || strings.Contains(lower, "claude")
}

// launchedRunArgs reads a launch's options in their command-line roles.
func launchedRunArgs(agent string, words []string) (ids map[string]bool, jsonEvents, resumed bool) {
	ids = map[string]bool{}
	var positional []string
	for i := 1; i < len(words) && words[i] != "--"; i++ {
		name, value, hasValue := strings.Cut(words[i], "=")
		if !strings.HasPrefix(name, "-") {
			positional = append(positional, words[i])
			continue
		}
		if !hasValue && launchedFlagTakesValue(agent, name) && i+1 < len(words) {
			i++
			value = words[i]
		}
		switch name {
		case "--session-id":
			if launchedValidID.MatchString(value) {
				ids[strings.ToLower(value)] = true
			}
		case "--json":
			jsonEvents = true
		case "--output-format":
			jsonEvents = agent == "claude" && (value == "json" || value == "stream-json")
		case "-r", "--resume", "-c", "--continue", "--from-pr", "--teleport":
			resumed = resumed || agent == "claude"
		}
	}
	return ids, jsonEvents, resumed || agent == "codex" && len(positional) > 1 && positional[1] == "resume"
}

// quotedSubstitution reports a word that is one double-quoted $(...) and nothing else.
func quotedSubstitution(word *syntax.Word) bool {
	if len(word.Parts) != 1 {
		return false
	}
	quoted, ok := word.Parts[0].(*syntax.DblQuoted)
	if !ok || quoted.Dollar || len(quoted.Parts) != 1 {
		return false
	}
	_, ok = quoted.Parts[0].(*syntax.CmdSubst)
	return ok
}

func fixedSubstitutionRole(agent string, words []string, literal []bool, i int) bool {
	for j := 1; j < i; j++ {
		if !literal[j] {
			continue
		}
		if words[j] == "--" {
			return true
		}
		if j == i-1 && agent == "claude" && (words[j] == "-p" || words[j] == "--print") {
			return true
		}
		if launchedFlagTakesValue(agent, words[j]) {
			j++
		}
	}
	return i == len(words)-1
}

// launchedRunID returns the one run ID a starting shell call shows.
func launchedRunID(ctx context.Context, input, output string) string {
	if command, ok := codeModeCommand(input); ok {
		input = command
		output = codeModeOutput(output)
	} else if !jsontext.Value(input).IsValid() && strings.Contains(input, "tools.exec_command") {
		return ""
	}
	commands, ok := ShellInputCommands(input)
	if !ok {
		return ""
	}
	output = launchedRunOutput(output)
	ids := map[string]bool{}
	launches, prefix := 0, ""
	unsafeOutput, jsonOutput := false, false
	var scan func(string, int)
	scan = func(command string, depth int) {
		if depth > 8 || ctx.Err() != nil {
			return
		}
		// PowerShell starts a quoted executable path with the call operator.
		command = strings.TrimPrefix(strings.TrimSpace(command), "& ")
		file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
		if err != nil {
			return
		}
		syntax.Walk(file, func(node syntax.Node) bool {
			if _, ok := node.(*syntax.FuncDecl); ok {
				return false
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
			name := ""
			if len(words) > 0 {
				name = strings.TrimSuffix(path.Base(strings.ToLower(strings.ReplaceAll(words[0], `\`, "/"))), ".exe")
			}
			if name != "codex" && name != "claude" || !OneShotRunArgs(name, words) {
				// A cd prints no ID; any other command may add an unrelated one.
				if raw, _ := shellWords(call.Args); len(raw) == 0 || len(raw) > 2 || raw[0] != "cd" {
					unsafeOutput = true
				}
				return false
			}
			for i := 1; i < len(words); i++ {
				if literal[i] {
					if launchedFlagTakesValue(name, words[i]) && i+1 < len(words) {
						i++
						if !literal[i] && !quotedValue(args[i]) && !quotedSubstitution(args[i]) {
							unsafeOutput = true
							return false
						}
					}
					continue
				}
				// Quoted substitutions must occupy a fixed argument role.
				if (!quotedSubstitution(args[i]) || !fixedSubstitutionRole(name, words, literal, i)) && (!quotedValue(args[i]) || words[i-1] != "--") {
					unsafeOutput = true
					return false
				}
			}
			explicit, jsonEvents, resumed := launchedRunArgs(name, words)
			if resumed {
				unsafeOutput = true
				return false
			}
			launches++
			if name == "codex" {
				prefix = "codex:"
			}
			jsonOutput = jsonEvents
			for id := range explicit {
				ids[prefix+id] = true
			}
			return false
		})
	}
	for _, command := range commands {
		scan(command, 0)
	}
	if launches != 1 {
		return ""
	}
	if !unsafeOutput && (jsonOutput || prefix != "" && launchedOutputID.MatchString(output)) {
		for _, id := range launchedOutputIDs(output) {
			if strings.HasPrefix(id, "codex:") == (prefix != "") {
				ids[id] = true
			}
		}
	}
	for id := range ids {
		if len(ids) == 1 {
			return id
		}
	}
	return ""
}

// launchedRunOutput unwraps the shell tool's output envelope.
func launchedRunOutput(text string) string {
	var envelope struct {
		Output string `json:"output"`
	}
	if jsonv2.Unmarshal([]byte(text), &envelope) == nil && envelope.Output != "" {
		text = envelope.Output
	}
	if i := strings.Index(text, "\nOutput:\n"); i >= 0 {
		header := text[:i]
		if strings.HasPrefix(header, "Chunk ID: ") || strings.HasPrefix(header, "Wall time: ") || strings.HasPrefix(header, "Process exited with code ") || strings.HasPrefix(header, "Exit code: ") || strings.HasPrefix(header, "Script completed\n") {
			return text[i+len("\nOutput:\n"):]
		}
	}
	return text
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

// launchedOutputIDs reads Codex's startup banner or the first JSON event.
func launchedOutputIDs(text string) []string {
	if match := launchedOutputID.FindStringSubmatch(text); len(match) != 0 {
		return []string{"codex:" + strings.ToLower(match[1])}
	}
	var ids []string
	var events []jsontext.Value
	lines := strings.Split(text, "\n")
	if jsonv2.Unmarshal([]byte(text), &events) == nil {
		lines = nil
		for _, event := range events {
			lines = append(lines, string(event))
		}
	}
	for i, line := range lines {
		var event struct {
			Type      string `json:"type"`
			SessionID string `json:"session_id"`
			ThreadID  string `json:"thread_id"`
		}
		if !strings.Contains(line, `_id"`) || jsonv2.Unmarshal([]byte(line), &event) != nil {
			continue
		}
		if i == 0 && event.Type == "thread.started" && launchedValidID.MatchString(event.ThreadID) {
			ids = append(ids, "codex:"+strings.ToLower(event.ThreadID))
		}
		if (event.Type == "system" || event.Type == "result") && launchedValidID.MatchString(event.SessionID) {
			ids = append(ids, strings.ToLower(event.SessionID))
		}
	}
	return ids
}

func hasLaunchedOutputID(text string) bool {
	return launchedOutputHint.MatchString(text) && len(launchedOutputIDs(launchedRunOutput(codeModeOutput(text)))) > 0
}

func launchedResultID(ctx context.Context, input, output string, failed bool) (id string) {
	if id = launchedRunID(ctx, input, output); failed && !slices.Contains(launchedOutputIDs(launchedRunOutput(codeModeOutput(output))), id) {
		return ""
	}
	return id
}

func launchedResultFailed(output string) bool {
	output = codeModeOutput(output)
	var envelope struct {
		Output string `json:"output"`
	}
	if jsonv2.Unmarshal([]byte(output), &envelope) == nil && envelope.Output != "" {
		output = envelope.Output
	}
	if strings.HasPrefix(output, "Script completed\n") {
		output = launchedRunOutput(output)
	}
	header, _, ok := strings.Cut(output, "\nOutput:\n")
	if !ok || !strings.HasPrefix(header, "Chunk ID: ") && !strings.HasPrefix(header, "Wall time: ") && !strings.HasPrefix(header, "Process exited with code ") && !strings.HasPrefix(header, "Exit code: ") {
		return false
	}
	match := launchedExitCode.FindStringSubmatch(header)
	return len(match) > 1 && strings.TrimLeft(match[1], "0") != ""
}

// collectLaunchedResults keeps output from the calls' results.
func collectLaunchedResults(calls map[string]*ParsedToolCall, outputs map[string]string, failed map[string]bool, results []ParsedToolResult) {
	for _, result := range results {
		if calls[result.ToolUseID] != nil {
			failed[result.ToolUseID] = failed[result.ToolUseID] || result.Status == "errored"
			outputs[result.ToolUseID] += DecodeContent(result.ContentRaw) + "\n"
		}
	}
}

// annotateClaudeLaunchedRuns associates a Bash call with its own results.
// Other agents that reuse Claude's transcript format skip it.
func annotateClaudeLaunchedRuns(ctx context.Context, messages []ParsedMessage, foreign bool) error {
	if foreign {
		return nil
	}
	calls := make(map[string]*ParsedToolCall)
	outputs, failed := make(map[string]string), make(map[string]bool)
	for i := range messages {
		if err := contextErrEvery(ctx, i); err != nil {
			return err
		}
		for j := range messages[i].ToolCalls {
			if call := &messages[i].ToolCalls[j]; call.Category == "Bash" && call.ToolUseID != "" && call.SubagentSessionID == "" && mentionsLauncher(call.InputJSON) {
				calls[call.ToolUseID] = call
			}
		}
		collectLaunchedResults(calls, outputs, failed, messages[i].ToolResults)
	}
	for id, call := range calls {
		call.SubagentSessionID = launchedResultID(ctx, call.InputJSON, outputs[id], failed[id])
	}
	return ctx.Err()
}
