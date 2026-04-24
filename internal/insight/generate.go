package insight

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
)

// geminiInsightModel is the model passed to the gemini CLI
// for insight generation.
const geminiInsightModel = "gemini-3.1-pro-preview"

// claudeStdoutLogMaxBytes bounds Claude stdout log payloads sent to
// SSE/UI so a single large JSON response does not overwhelm the log
// stream. Full stdout is still parsed for result extraction.
const claudeStdoutLogMaxBytes = 16 * 1024

// Result holds the output from an AI agent invocation.
type Result struct {
	Content string
	Agent   string
	Model   string
}

// ValidAgentNames lists the supported insight agent names in display
// order. ValidAgents is a lookup set derived from it.
var ValidAgentNames = []string{
	"claude",
	"codex",
	"copilot",
	"gemini",
	"kiro",
}

// ValidAgents is the set of supported insight agent names.
var ValidAgents = func() map[string]bool {
	m := make(map[string]bool, len(ValidAgentNames))
	for _, name := range ValidAgentNames {
		m[name] = true
	}
	return m
}()

// GenerateFunc is the signature for insight generation,
// allowing tests to substitute a stub.
type GenerateFunc func(
	ctx context.Context, agent, prompt string,
) (Result, error)

// LogEvent represents one log line emitted by an insight
// agent process.
type LogEvent struct {
	Stream string `json:"stream"` // stdout|stderr
	Line   string `json:"line"`
}

// LogFunc receives real-time log events from the agent
// subprocess.
type LogFunc func(LogEvent)

// GenerateStreamFunc is like GenerateFunc but includes a
// log callback for streaming stdout/stderr events.
type GenerateStreamFunc func(
	ctx context.Context, agent, prompt string, onLog LogFunc,
) (Result, error)

// Generate invokes an AI agent CLI to generate an insight.
// The agent parameter selects which CLI to use (claude,
// codex, gemini). The prompt is passed via stdin.
func Generate(
	ctx context.Context, agent, prompt string,
) (Result, error) {
	return GenerateStream(ctx, agent, prompt, nil)
}

// GenerateStream invokes an AI agent CLI to generate an
// insight while optionally streaming process logs.
func GenerateStream(
	ctx context.Context, agent, prompt string, onLog LogFunc,
) (Result, error) {
	if !ValidAgents[agent] {
		return Result{}, fmt.Errorf(
			"unsupported agent: %s", agent,
		)
	}

	path, err := exec.LookPath(agentBinary(agent))
	if err != nil {
		return Result{}, fmt.Errorf(
			"%s CLI not found: %w", agent, err,
		)
	}

	switch agent {
	case "codex":
		return generateCodex(ctx, path, prompt, onLog)
	case "copilot":
		return generateCopilot(ctx, path, prompt, onLog)
	case "gemini":
		return generateGemini(ctx, path, prompt, onLog)
	case "kiro":
		return generateKiro(ctx, path, prompt, onLog)
	default:
		return generateClaude(ctx, path, prompt, onLog)
	}
}

// agentEnv returns the current environment with
// CLAUDE_NO_SOUND=1 appended.
//
// The full environment is passed through intentionally.
// Agent CLIs need provider auth (API keys, tokens, config
// paths) that vary across providers, users, and deployment
// methods (env vars, desktop.env, persisted login). An
// allowlist is brittle here: every new provider or config
// var requires a code change, and missing one breaks auth.
// Sandboxing is handled by CLI flags (--tools, --sandbox,
// --config-dir, etc.), not by env filtering.
func agentEnv() []string {
	return append(os.Environ(), "CLAUDE_NO_SOUND=1")
}

func emitLog(onLog LogFunc, stream, line string) {
	if onLog == nil {
		return
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	onLog(LogEvent{
		Stream: stream,
		Line:   line,
	})
}

func truncateLogLine(line string, maxBytes int) string {
	if maxBytes <= 0 || len(line) <= maxBytes {
		return line
	}
	omitted := len(line) - maxBytes
	return fmt.Sprintf(
		"%s... [truncated %d bytes]",
		line[:maxBytes], omitted,
	)
}

func collectStreamLines(
	r io.Reader, stream string, onLog LogFunc,
) <-chan string {
	ch := make(chan string, 1)
	go func() {
		defer close(ch)
		br := bufio.NewReader(r)
		var lines []string
		for {
			line, err := br.ReadString('\n')
			trimmed := strings.TrimRight(line, "\r\n")
			if trimmed != "" {
				lines = append(lines, trimmed)
				emitLog(onLog, stream, trimmed)
			}
			if err == nil {
				continue
			}
			if err == io.EOF {
				break
			}
			emitLog(
				onLog, "stderr",
				fmt.Sprintf("read %s: %v", stream, err),
			)
			_, _ = io.Copy(io.Discard, br)
			break
		}
		ch <- strings.Join(lines, "\n")
	}()
	return ch
}

// generateClaude invokes `claude -p --output-format json`.
func generateClaude(
	ctx context.Context, path, prompt string, onLog LogFunc,
) (Result, error) {
	cmd := exec.CommandContext(
		ctx, path,
		"-p", "--output-format", "json",
		"--no-session-persistence",
		"--tools", "",
	)
	cmd.Env = agentEnv()
	cmd.Stdin = strings.NewReader(prompt)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, fmt.Errorf(
			"create stdout pipe: %w", err,
		)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return Result{}, fmt.Errorf(
			"create stderr pipe: %w", err,
		)
	}

	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("start claude: %w", err)
	}

	stderrDone := collectStreamLines(
		stderrPipe, "stderr", onLog,
	)
	stdoutBytes, readErr := io.ReadAll(stdoutPipe)
	if readErr != nil {
		_, _ = io.Copy(io.Discard, stdoutPipe)
	}
	stderrText := <-stderrDone
	runErr := cmd.Wait()
	if readErr != nil {
		return Result{}, fmt.Errorf(
			"read claude stdout: %w", readErr,
		)
	}

	emitLog(
		onLog,
		"stdout",
		truncateLogLine(string(stdoutBytes), claudeStdoutLogMaxBytes),
	)

	// Honor context cancellation over salvaging stdout, but
	// only when the command actually failed. A successful
	// cmd.Run with a race-y post-completion cancel should
	// still return the valid result.
	if runErr != nil && ctx.Err() != nil {
		return Result{}, fmt.Errorf(
			"claude CLI cancelled: %w", ctx.Err(),
		)
	}

	// Claude Code CLI outputs a JSON array of events when
	// invoked with -p --output-format json. Find the element
	// with type="result" and extract its result field.
	// Also accept the legacy single-object format as a
	// fallback for older Claude CLI versions.
	content, model := parseCLIResult(stdoutBytes)
	if strings.TrimSpace(content) != "" {
		return Result{
			Content: content,
			Agent:   "claude",
			Model:   model,
		}, nil
	}

	if runErr != nil {
		return Result{}, fmt.Errorf(
			"claude CLI failed: %w\nstderr: %s",
			runErr, stderrText,
		)
	}

	return Result{}, fmt.Errorf(
		"claude returned empty result\nraw: %s",
		string(stdoutBytes),
	)
}

// parseCLIResult extracts the result text and model from
// claude CLI output. Claude Code (v2+) outputs a JSON array
// of events; we find type="result" and read its result field.
// Falls back to the legacy single-object format for older
// versions: {"result":"...","model":"..."}.
func parseCLIResult(data []byte) (result, model string) {
	// Try JSON array format (Claude Code v2+).
	var events []json.RawMessage
	if json.Unmarshal(data, &events) == nil {
		for _, raw := range events {
			var ev struct {
				Type       string                     `json:"type"`
				Result     string                     `json:"result"`
				ModelUsage map[string]json.RawMessage `json:"modelUsage"`
			}
			if json.Unmarshal(raw, &ev) != nil {
				continue
			}
			if ev.Type == "result" &&
				strings.TrimSpace(ev.Result) != "" {
				if len(ev.ModelUsage) > 0 {
					keys := make([]string, 0, len(ev.ModelUsage))
					for k := range ev.ModelUsage {
						keys = append(keys, k)
					}
					sort.Strings(keys)
					model = keys[0]
				}
				return ev.Result, model
			}
		}
	}
	// Fall back to legacy single-object format.
	var resp struct {
		Result string `json:"result"`
		Model  string `json:"model"`
	}
	if json.Unmarshal(data, &resp) == nil {
		return resp.Result, resp.Model
	}
	return "", ""
}

// generateCodex invokes `codex exec` in read-only sandbox
// and parses the JSONL stream for agent_message items.
func generateCodex(
	ctx context.Context, path, prompt string, onLog LogFunc,
) (Result, error) {
	cmd := exec.CommandContext(
		ctx, path,
		"exec", "--json",
		"--sandbox", "read-only",
		"--skip-git-repo-check",
		"--ephemeral",
		"-",
	)
	cmd.Env = agentEnv()
	cmd.Stdin = strings.NewReader(prompt)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, fmt.Errorf(
			"create stdout pipe: %w", err,
		)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return Result{}, fmt.Errorf(
			"create stderr pipe: %w", err,
		)
	}

	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf(
			"start codex: %w", err,
		)
	}

	stderrDone := collectStreamLines(
		stderrPipe, "stderr", onLog,
	)
	content, parseErr := parseCodexStream(stdoutPipe, onLog)

	// Drain remaining stdout so cmd.Wait doesn't block.
	if parseErr != nil {
		_, _ = io.Copy(io.Discard, stdoutPipe)
	}

	stderrText := <-stderrDone
	if waitErr := cmd.Wait(); waitErr != nil {
		if parseErr != nil {
			return Result{}, fmt.Errorf(
				"codex failed: %w (parse: %v)\nstderr: %s",
				waitErr, parseErr, stderrText,
			)
		}
		return Result{}, fmt.Errorf(
			"codex failed: %w\nstderr: %s",
			waitErr, stderrText,
		)
	}
	if parseErr != nil {
		return Result{}, fmt.Errorf(
			"parse codex stream: %w\nstderr: %s",
			parseErr, stderrText,
		)
	}

	return Result{
		Content: content,
		Agent:   "codex",
	}, nil
}

// codexEvent represents a JSONL event from codex --json.
type codexEvent struct {
	Type  string `json:"type"`
	Error struct {
		Message string `json:"message,omitempty"`
	} `json:"error,omitempty"`
	Item struct {
		ID   string `json:"id,omitempty"`
		Type string `json:"type,omitempty"`
		Text string `json:"text,omitempty"`
	} `json:"item,omitempty"`
}

// parseCodexStream reads codex JSONL and extracts
// agent_message text from item.completed/item.updated events.
func parseCodexStream(
	r io.Reader, onLog LogFunc,
) (string, error) {
	br := bufio.NewReader(r)
	messages := make([]string, 0)
	indexByID := make(map[string]int)

	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return "", fmt.Errorf("read stream: %w", err)
		}

		trimmed := strings.TrimSpace(line)
		emitLog(onLog, "stdout", trimmed)
		if trimmed != "" {
			var ev codexEvent
			if json.Unmarshal(
				[]byte(trimmed), &ev,
			) == nil {
				if ev.Type == "turn.failed" ||
					ev.Type == "error" {
					msg := ev.Error.Message
					if msg == "" {
						msg = "codex stream error"
					}
					return "", fmt.Errorf(
						"codex: %s", msg,
					)
				}

				isMsg := (ev.Type == "item.completed" ||
					ev.Type == "item.updated") &&
					ev.Item.Type == "agent_message" &&
					ev.Item.Text != ""
				if isMsg {
					if ev.Item.ID == "" {
						messages = append(
							messages, ev.Item.Text,
						)
					} else if idx, ok := indexByID[ev.Item.ID]; ok {
						messages[idx] = ev.Item.Text
					} else {
						indexByID[ev.Item.ID] = len(messages)
						messages = append(
							messages, ev.Item.Text,
						)
					}
				}
			}
		}

		if err == io.EOF {
			break
		}
	}

	return strings.Join(messages, "\n"), nil
}

// generateCopilot invokes `copilot -p <prompt> --silent`.
// The prompt is passed as the -p argument (copilot does not
// read prompts from stdin). Output is plain text on stdout.
func generateCopilot(
	ctx context.Context, path, prompt string, onLog LogFunc,
) (Result, error) {
	cmd := exec.CommandContext(
		ctx, path,
		"-p", prompt,
		"--silent",
		"--no-custom-instructions",
		"--no-ask-user",
		"--disable-builtin-mcps",
	)
	cmd.Env = agentEnv()

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, fmt.Errorf(
			"create stdout pipe: %w", err,
		)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return Result{}, fmt.Errorf(
			"create stderr pipe: %w", err,
		)
	}

	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf(
			"start copilot: %w", err,
		)
	}

	stderrDone := collectStreamLines(
		stderrPipe, "stderr", onLog,
	)
	// Read stdout raw to preserve blank lines in plain
	// text output (collectStreamLines drops empty lines).
	stdoutBytes, readErr := io.ReadAll(stdoutPipe)
	stderrText := <-stderrDone
	runErr := cmd.Wait()

	if readErr != nil {
		return Result{}, fmt.Errorf(
			"read copilot stdout: %w", readErr,
		)
	}

	emitLog(onLog, "stdout", string(stdoutBytes))

	if runErr != nil && ctx.Err() != nil {
		return Result{}, fmt.Errorf(
			"copilot CLI cancelled: %w", ctx.Err(),
		)
	}
	if runErr != nil {
		return Result{}, fmt.Errorf(
			"copilot CLI failed: %w\nstderr: %s",
			runErr, stderrText,
		)
	}

	content := strings.TrimSpace(string(stdoutBytes))
	if content == "" {
		return Result{}, fmt.Errorf(
			"copilot returned empty result",
		)
	}

	return Result{
		Content: content,
		Agent:   "copilot",
	}, nil
}

// generateGemini invokes `gemini --output-format stream-json`
// and parses the JSONL stream for result/assistant messages.
func generateGemini(
	ctx context.Context, path, prompt string, onLog LogFunc,
) (Result, error) {
	cmd := exec.CommandContext(
		ctx, path,
		"--model", geminiInsightModel,
		"--output-format", "stream-json",
		"--sandbox",
	)
	cmd.Env = agentEnv()
	cmd.Stdin = strings.NewReader(prompt)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, fmt.Errorf(
			"create stdout pipe: %w", err,
		)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return Result{}, fmt.Errorf(
			"create stderr pipe: %w", err,
		)
	}

	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf(
			"start gemini: %w", err,
		)
	}

	stderrDone := collectStreamLines(
		stderrPipe, "stderr", onLog,
	)
	content, parseErr := parseStreamJSON(stdoutPipe, onLog)

	// Drain remaining stdout so cmd.Wait doesn't block.
	if parseErr != nil {
		_, _ = io.Copy(io.Discard, stdoutPipe)
	}

	stderrText := <-stderrDone
	if waitErr := cmd.Wait(); waitErr != nil {
		if parseErr != nil {
			return Result{}, fmt.Errorf(
				"gemini failed: %w (parse: %v)\nstderr: %s",
				waitErr, parseErr, stderrText,
			)
		}
		return Result{}, fmt.Errorf(
			"gemini failed: %w\nstderr: %s",
			waitErr, stderrText,
		)
	}
	if parseErr != nil {
		return Result{}, fmt.Errorf(
			"parse gemini stream: %w\nstderr: %s",
			parseErr, stderrText,
		)
	}

	return Result{
		Content: content,
		Agent:   "gemini",
		Model:   geminiInsightModel,
	}, nil
}

// streamMessage represents a JSONL event from stream-json
// output (shared format between Claude and Gemini CLIs).
type streamMessage struct {
	Type    string `json:"type"`
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
	Message struct {
		Content string `json:"content,omitempty"`
	} `json:"message,omitempty"`
	Result string `json:"result,omitempty"`
	Error  struct {
		Message string `json:"message,omitempty"`
	} `json:"error,omitempty"`
}

// parseStreamJSON reads stream-json JSONL and extracts the
// result text. Prefers type=result, falls back to collecting
// assistant messages.
func parseStreamJSON(
	r io.Reader, onLog LogFunc,
) (string, error) {
	br := bufio.NewReader(r)
	var lastResult string
	var assistantMsgs []string

	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return "", fmt.Errorf("read stream: %w", err)
		}

		trimmed := strings.TrimSpace(line)
		emitLog(onLog, "stdout", trimmed)
		if trimmed != "" {
			var msg streamMessage
			if json.Unmarshal(
				[]byte(trimmed), &msg,
			) != nil {
				continue
			}
			if msg.Type == "error" {
				m := msg.Error.Message
				if m == "" {
					m = "stream error"
				}
				return "", fmt.Errorf(
					"stream: %s", m,
				)
			}
			if msg.Type == "message" &&
				msg.Role == "assistant" &&
				msg.Content != "" {
				assistantMsgs = append(
					assistantMsgs, msg.Content,
				)
			}
			if msg.Type == "assistant" &&
				msg.Message.Content != "" {
				assistantMsgs = append(
					assistantMsgs,
					msg.Message.Content,
				)
			}
			if msg.Type == "result" &&
				msg.Result != "" {
				lastResult = msg.Result
			}
		}

		if err == io.EOF {
			break
		}
	}

	if lastResult != "" {
		return lastResult, nil
	}
	if len(assistantMsgs) > 0 {
		return strings.Join(assistantMsgs, "\n"), nil
	}
	return "", nil
}

// agentBinary maps an agent name to its CLI binary name.
func agentBinary(agent string) string {
	if agent == "kiro" {
		return "kiro-cli"
	}
	return agent
}

// ansiRE matches ANSI escape sequences.
var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)

// generateKiro invokes `kiro-cli chat --no-interactive` with
// the prompt on stdin and strips ANSI escape codes from output.
//
// Note: kiro-cli does not have a hard no-tools/read-only mode
// like Claude's --tools "" or Codex's --sandbox read-only.
// --trust-tools= disables tool trust prompts but does not
// prevent tool execution. Use only with trusted session data.
// The working directory is set to os.TempDir() to limit
// exposure if tools are triggered.
func generateKiro(
	ctx context.Context, path, prompt string, onLog LogFunc,
) (Result, error) {
	cmd := exec.CommandContext(
		ctx, path,
		"chat",
		"--no-interactive",
		"--trust-tools=",
		"--wrap", "never",
	)
	cmd.Env = agentEnv()
	cmd.Dir = os.TempDir()
	cmd.Stdin = strings.NewReader(prompt)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, fmt.Errorf("create stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return Result{}, fmt.Errorf("create stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("start kiro-cli: %w", err)
	}

	stderrDone := collectStreamLines(stderrPipe, "stderr", onLog)
	stdoutBytes, readErr := io.ReadAll(stdoutPipe)
	stderrText := <-stderrDone
	runErr := cmd.Wait()

	if readErr != nil {
		return Result{}, fmt.Errorf("read kiro stdout: %w", readErr)
	}

	emitLog(onLog, "stdout", string(stdoutBytes))

	if runErr != nil && ctx.Err() != nil {
		return Result{}, fmt.Errorf("kiro-cli cancelled: %w", ctx.Err())
	}
	if runErr != nil {
		return Result{}, fmt.Errorf(
			"kiro-cli failed: %w\nstderr: %s", runErr, stderrText,
		)
	}

	// Strip ANSI escape codes and the trust/timing banners.
	clean := ansiRE.ReplaceAllString(string(stdoutBytes), "")
	var lines []string
	for line := range strings.SplitSeq(clean, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "Agents can sometimes") ||
			strings.HasPrefix(trimmed, "Learn more at") ||
			strings.HasPrefix(trimmed, "▸ Time:") {
			continue
		}
		lines = append(lines, line)
	}
	content := strings.TrimSpace(strings.Join(lines, "\n"))
	if content == "" {
		return Result{}, fmt.Errorf("kiro returned empty result")
	}

	return Result{
		Content: content,
		Agent:   "kiro",
	}, nil
}
