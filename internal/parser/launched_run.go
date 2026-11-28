package parser

import (
	"context"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"path"
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

var (
	launchedUUID       = `[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`
	launchedOutputID   = regexp.MustCompile(`(?im)\AOpenAI Codex v[^\r\n]*\r?\n--------\r?\n(?:[^\r\n]*\r?\n)*?session id:[ \t]*(` + launchedUUID + `)[ \t]*\r?\n--------(?:\r?\n|$)`)
	launchedValidID    = regexp.MustCompile(`(?i)^` + launchedUUID + `$`)
	launchedOutputHint = regexp.MustCompile(`(?i)session id:|session_id|thread_id`)
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
		case "-r", "--resume", "-c", "--continue":
			resumed = resumed || agent == "claude"
		}
	}
	return ids, jsonEvents, resumed || agent == "codex" && len(positional) > 1 && positional[1] == "resume"
}

// launchedRunID returns the one run ID a starting shell call shows.
func launchedRunID(ctx context.Context, input, output string) string {
	commands, ok := ShellInputCommands(input)
	if !ok {
		return ""
	}
	output = launchedRunOutput(output)
	ids := map[string]bool{}
	launches, prefix := 0, ""
	unsafeOutput, codexJSON := false, false
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
			call, ok := node.(*syntax.CallExpr)
			if !ok {
				return true
			}
			args, _ := unwrapPRCommand(ctx, call.Args)
			if inner, ok := shellCommand(args); ok {
				scan(inner, depth+1)
				return false
			}
			words, _ := shellWords(args)
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
			explicit, jsonEvents, resumed := launchedRunArgs(name, words)
			if resumed {
				unsafeOutput = true
				return false
			}
			launches++
			if name == "codex" {
				prefix, codexJSON = "codex:", jsonEvents
			}
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
	if !unsafeOutput && (prefix == "" || codexJSON || launchedOutputID.MatchString(output)) {
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
		return envelope.Output
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
	for i, line := range strings.Split(text, "\n") {
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
	return launchedOutputHint.MatchString(text) && len(launchedOutputIDs(launchedRunOutput(text))) > 0
}

// annotateClaudeLaunchedRuns associates a Bash call with its own results.
func annotateClaudeLaunchedRuns(ctx context.Context, messages []ParsedMessage) error {
	calls := make(map[string]*ParsedToolCall)
	outputs := make(map[string]string)
	for i := range messages {
		if err := contextErrEvery(ctx, i); err != nil {
			return err
		}
		for j := range messages[i].ToolCalls {
			if call := &messages[i].ToolCalls[j]; call.Category == "Bash" && call.ToolUseID != "" && call.SubagentSessionID == "" && mentionsLauncher(call.InputJSON) {
				calls[call.ToolUseID] = call
			}
		}
		for _, result := range messages[i].ToolResults {
			if calls[result.ToolUseID] != nil {
				outputs[result.ToolUseID] += DecodeContent(result.ContentRaw) + "\n"
			}
		}
	}
	for id, call := range calls {
		call.SubagentSessionID = launchedRunID(ctx, call.InputJSON, outputs[id])
	}
	return ctx.Err()
}
