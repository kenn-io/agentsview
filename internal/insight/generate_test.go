package insight

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestParseCodexStream(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		want      string
		wantError string
	}{
		{
			name: "agent messages",
			input: `{"type":"thread.started","thread_id":"abc"}
{"type":"turn.started"}
{"type":"item.started","item":{"id":"m1","type":"agent_message"}}
{"type":"item.updated","item":{"id":"m1","type":"agent_message","text":"partial"}}
{"type":"item.completed","item":{"id":"m1","type":"agent_message","text":"# Summary\nDone."}}
{"type":"turn.completed"}
`,
			want: `# Summary
Done.`,
		},
		{
			name: "multiple messages",
			input: `{"type":"item.completed","item":{"id":"m1","type":"agent_message","text":"First"}}
{"type":"item.completed","item":{"id":"m2","type":"agent_message","text":"Second"}}
`,
			want: `First
Second`,
		},
		{
			name: "turn failed",
			input: `{"type":"turn.started"}
{"type":"turn.failed","error":{"message":"rate limit"}}
`,
			wantError: "rate limit",
		},
		{
			name: "deduplicates by id",
			input: `{"type":"item.updated","item":{"id":"m1","type":"agent_message","text":"v1"}}
{"type":"item.updated","item":{"id":"m1","type":"agent_message","text":"v2"}}
{"type":"item.completed","item":{"id":"m1","type":"agent_message","text":"v3"}}
`,
			want: "v3",
		},
		{
			name: "skips malformed json",
			input: `not valid json
{"type":"item.completed","item":{"id":"m1","type":"agent_message","text":"OK"}}
`,
			want: "OK",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parseCodexStream(strings.NewReader(tt.input), nil)
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("expected error containing %q, got %v", tt.wantError, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result != tt.want {
				t.Errorf("got %q, want %q", result, tt.want)
			}
		})
	}
}

func TestParseStreamJSON(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		want      string
		wantError string
	}{
		{
			name: "result event",
			input: `{"type":"system","subtype":"init"}
{"type":"assistant","message":{"content":"Working..."}}
{"type":"result","result":"# Final Summary"}
`,
			want: "# Final Summary",
		},
		{
			name: "falls back to assistant messages",
			input: `{"type":"assistant","message":{"content":"Part 1"}}
{"type":"assistant","message":{"content":"Part 2"}}
`,
			want: `Part 1
Part 2`,
		},
		{
			name: "gemini format",
			input: `{"type":"system","subtype":"init"}
{"type":"message","role":"assistant","content":"Analysis done.","delta":true}
{"type":"result","result":"# Full Result"}
`,
			want: "# Full Result",
		},
		{
			name: "error event",
			input: `{"type":"system","subtype":"init"}
{"type":"error","error":{"message":"rate limited"}}
`,
			wantError: "rate limited",
		},
		{
			name:  "empty",
			input: "",
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parseStreamJSON(strings.NewReader(tt.input), nil)
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("expected error containing %q, got %v", tt.wantError, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result != tt.want {
				t.Errorf("got %q, want %q", result, tt.want)
			}
		})
	}
}

func TestCollectStreamLines_LargeLine(t *testing.T) {
	longLine := strings.Repeat("x", 3*1024*1024)
	input := longLine + "\nsmall-line\n"
	var got []LogEvent

	done := collectStreamLines(
		strings.NewReader(input), "stderr",
		func(ev LogEvent) {
			got = append(got, ev)
		},
	)
	text := <-done

	if len(got) != 2 {
		t.Fatalf("got %d log events, want 2", len(got))
	}
	if got[0].Stream != "stderr" || len(got[0].Line) != len(longLine) {
		t.Fatalf(
			"first event mismatch: stream=%q len=%d",
			got[0].Stream, len(got[0].Line),
		)
	}
	if got[1].Line != "small-line" {
		t.Fatalf("second line = %q, want %q", got[1].Line, "small-line")
	}
	if !strings.Contains(text, "small-line") {
		t.Fatalf("joined text missing expected line: %q", text)
	}
}

func TestAgentEnv(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-secret")
	t.Setenv("CUSTOM_VAR", "custom-val")

	env := agentEnv()
	envMap := make(map[string]string, len(env))
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		envMap[strings.ToUpper(k)] = v
	}

	// Full env is passed through — no filtering.
	if envMap["ANTHROPIC_API_KEY"] != "sk-secret" {
		t.Error("ANTHROPIC_API_KEY should be preserved")
	}
	if envMap["CUSTOM_VAR"] != "custom-val" {
		t.Error("CUSTOM_VAR should be preserved")
	}
	if v, ok := envMap["CLAUDE_NO_SOUND"]; !ok || v != "1" {
		t.Errorf(
			"CLAUDE_NO_SOUND should be 1, got %q", v,
		)
	}
}

func TestValidAgents(t *testing.T) {
	for _, agent := range []string{
		"claude", "codex", "copilot", "gemini", "kiro",
	} {
		if !ValidAgents[agent] {
			t.Errorf("%s should be valid", agent)
		}
	}
	if ValidAgents["gpt"] {
		t.Error("gpt should not be valid")
	}
}

func createMockBinary(
	t *testing.T, stdout string, exitCode int, writeArgs bool, name string,
) (bin, argsFile string) {
	t.Helper()
	dir := t.TempDir()
	dataFile := filepath.Join(dir, "stdout.txt")
	if err := os.WriteFile(dataFile, []byte(stdout), 0o644); err != nil {
		t.Fatal(err)
	}

	if writeArgs {
		argsFile = filepath.Join(dir, "args.txt")
	}

	if runtime.GOOS == "windows" {
		bin = filepath.Join(dir, name+".cmd")
		var script string
		if writeArgs {
			script = fmt.Sprintf("@echo %%* > %q\r\n@type %q\r\n@exit /b %d\r\n", argsFile, dataFile, exitCode)
		} else {
			script = fmt.Sprintf("@type %q\r\n@exit /b %d\r\n", dataFile, exitCode)
		}
		if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		return bin, argsFile
	}

	bin = filepath.Join(dir, name)
	var script string
	if writeArgs {
		script = fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %s\ncat %s\nexit %d\n", shellQuote(argsFile), shellQuote(dataFile), exitCode)
	} else {
		script = fmt.Sprintf("#!/bin/sh\ncat %s\nexit %d\n", shellQuote(dataFile), exitCode)
	}
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argsFile
}

// fakeClaudeBin writes a script that prints the given stdout
// and exits with the given code, ignoring all flags. Uses a
// .cmd batch file on Windows and a shell script elsewhere.
func fakeClaudeBin(
	t *testing.T, stdout string, exitCode int,
) string {
	bin, _ := createMockBinary(t, stdout, exitCode, false, "claude")
	return bin
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func TestGenerateClaude_CLIFlags(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test not supported on windows")
	}

	stdout := `[{"type":"result","result":"OK","modelUsage":{"m1":{}}}]`
	bin, argsFile := createMockBinary(
		t, stdout, 0, true, "claude",
	)

	result, err := generateClaude(
		context.Background(), bin, "test prompt", nil,
	)
	if err != nil {
		t.Fatalf("generateClaude: %v", err)
	}
	if result.Content != "OK" {
		t.Errorf("Content = %q, want OK", result.Content)
	}

	argsData, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("reading args: %v", err)
	}
	args := strings.Split(
		strings.TrimSpace(string(argsData)), "\n",
	)

	// The empty string value for --tools is lost by the
	// shell printf, so verify args as a joined string.
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-p",
		"--output-format json",
		"--no-session-persistence",
		"--tools",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf(
				"args %q missing %q", joined, want,
			)
		}
	}
}

func TestGenerateCodex_CLIFlags(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test not supported on windows")
	}

	stdout := `{"type":"item.completed","item":{"id":"m1","type":"agent_message","text":"OK"}}
`
	bin, argsFile := createMockBinary(
		t, stdout, 0, true, "codex",
	)

	result, err := generateCodex(
		context.Background(), bin, "test prompt", nil,
	)
	if err != nil {
		t.Fatalf("generateCodex: %v", err)
	}
	if result.Content != "OK" {
		t.Errorf("Content = %q, want OK", result.Content)
	}

	argsData, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("reading args: %v", err)
	}
	args := strings.Split(
		strings.TrimSpace(string(argsData)), "\n",
	)

	wantArgs := []string{
		"exec", "--json",
		"--sandbox", "read-only",
		"--skip-git-repo-check",
		"--ephemeral",
		"-",
	}
	if len(args) != len(wantArgs) {
		t.Fatalf("args = %v, want %v", args, wantArgs)
	}
	for i, want := range wantArgs {
		if args[i] != want {
			t.Errorf(
				"arg[%d] = %q, want %q",
				i, args[i], want,
			)
		}
	}
}

func TestGenerateCopilot_CLIFlags(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test not supported on windows")
	}

	bin, argsFile := createMockBinary(
		t, "Hello from copilot", 0, true, "copilot",
	)

	result, err := generateCopilot(
		context.Background(), bin, "test prompt", nil,
	)
	if err != nil {
		t.Fatalf("generateCopilot: %v", err)
	}
	if result.Content != "Hello from copilot" {
		t.Errorf(
			"Content = %q, want %q",
			result.Content, "Hello from copilot",
		)
	}
	if result.Agent != "copilot" {
		t.Errorf("Agent = %q, want copilot", result.Agent)
	}

	argsData, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("reading args: %v", err)
	}
	args := strings.Split(
		strings.TrimSpace(string(argsData)), "\n",
	)

	wantArgs := []string{
		"-p", "test prompt",
		"--silent",
		"--no-custom-instructions",
		"--no-ask-user",
		"--disable-builtin-mcps",
	}
	if len(args) != len(wantArgs) {
		t.Fatalf("args = %v, want %v", args, wantArgs)
	}
	for i, want := range wantArgs {
		if args[i] != want {
			t.Errorf(
				"arg[%d] = %q, want %q",
				i, args[i], want,
			)
		}
	}
}

func TestGenerateCopilot_EmptyResult(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test not supported on windows")
	}

	bin, _ := createMockBinary(
		t, "", 0, false, "copilot",
	)

	_, err := generateCopilot(
		context.Background(), bin, "test", nil,
	)
	if err == nil {
		t.Fatal("expected error for empty result")
	}
	if !strings.Contains(err.Error(), "empty result") {
		t.Errorf("error = %q, want empty result", err)
	}
}

func TestGenerateCopilot_PreservesBlankLines(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test not supported on windows")
	}

	multiParagraph := "# Summary\n\nParagraph one.\n\nParagraph two.\n"
	bin, _ := createMockBinary(
		t, multiParagraph, 0, false, "copilot",
	)

	result, err := generateCopilot(
		context.Background(), bin, "test", nil,
	)
	if err != nil {
		t.Fatalf("generateCopilot: %v", err)
	}
	if !strings.Contains(result.Content, "\n\n") {
		t.Errorf(
			"blank lines lost: %q", result.Content,
		)
	}
}

func TestGenerateClaude_SalvageOnNonZeroExit(t *testing.T) {
	tests := []struct {
		name       string
		stdout     string
		exitCode   int
		wantResult string
		wantErr    bool
	}{
		{
			name:       "non-zero exit with valid result",
			stdout:     `[{"type":"result","result":"# Analysis\nDone.","modelUsage":{"m1":{}}}]`,
			exitCode:   1,
			wantResult: "# Analysis\nDone.",
		},
		{
			name:     "non-zero exit with empty result",
			stdout:   `[{"type":"result","result":"","modelUsage":{"m1":{}}}]`,
			exitCode: 1,
			wantErr:  true,
		},
		{
			name:     "non-zero exit with invalid JSON",
			stdout:   `not json`,
			exitCode: 1,
			wantErr:  true,
		},
		{
			name:     "non-zero exit with no stdout",
			stdout:   "",
			exitCode: 1,
			wantErr:  true,
		},
		{
			name:       "zero exit with valid result",
			stdout:     `[{"type":"result","result":"OK","modelUsage":{"m2":{}}}]`,
			exitCode:   0,
			wantResult: "OK",
		},
		{
			name:     "zero exit with empty result",
			stdout:   `[{"type":"result","result":"","modelUsage":{"m2":{}}}]`,
			exitCode: 0,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin := fakeClaudeBin(
				t, tt.stdout, tt.exitCode,
			)
			result, err := generateClaude(
				context.Background(), bin, "test", nil,
			)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.Content != tt.wantResult {
				t.Errorf(
					"content = %q, want %q",
					result.Content, tt.wantResult,
				)
			}
			if result.Agent != "claude" {
				t.Errorf(
					"agent = %q, want claude",
					result.Agent,
				)
			}
		})
	}
}

// fakeGeminiBin writes a script that records its argv to an
// args file, then prints stream-json output. This lets tests
// verify both the CLI flags and the parsed result.
func fakeGeminiBin(
	t *testing.T, stdout string, exitCode int,
) (bin, argsFile string) {
	return createMockBinary(t, stdout, exitCode, true, "gemini")
}

func TestGenerateGemini_ModelFlag(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test not supported on windows")
	}

	streamJSON := `{"type":"message","role":"assistant","content":"Hello"}
{"type":"result","result":"# Analysis"}
`

	bin, argsFile := fakeGeminiBin(t, streamJSON, 0)

	result, err := generateGemini(
		context.Background(), bin, "test prompt", nil,
	)
	if err != nil {
		t.Fatalf("generateGemini: %v", err)
	}

	if result.Content != "# Analysis" {
		t.Errorf("Content = %q, want %q",
			result.Content, "# Analysis")
	}
	if result.Agent != "gemini" {
		t.Errorf("Agent = %q, want gemini", result.Agent)
	}
	if result.Model != geminiInsightModel {
		t.Errorf("Model = %q, want %q",
			result.Model, geminiInsightModel)
	}

	// Verify the CLI was invoked with --model flag.
	argsData, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("reading args: %v", err)
	}
	args := strings.Split(
		strings.TrimSpace(string(argsData)), "\n",
	)

	wantArgs := []string{
		"--model", geminiInsightModel,
		"--output-format", "stream-json",
		"--sandbox",
	}
	if len(args) != len(wantArgs) {
		t.Fatalf("args = %v, want %v", args, wantArgs)
	}
	for i, want := range wantArgs {
		if args[i] != want {
			t.Errorf("arg[%d] = %q, want %q",
				i, args[i], want)
		}
	}
}

func TestGenerateClaude_CancelledContext(t *testing.T) {
	// Pre-cancelled context: cmd.Run fails (runErr != nil)
	// and ctx.Err() != nil → cancellation error.
	bin := fakeClaudeBin(
		t, `[{"type":"result","result":"OK","modelUsage":{"m1":{}}}]`, 0,
	)
	ctx, cancel := context.WithCancel(
		context.Background(),
	)
	cancel()

	_, err := generateClaude(ctx, bin, "test", nil)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
	if !strings.Contains(err.Error(), "cancel") {
		t.Errorf("error = %q, want cancel", err)
	}
}

func TestGenerateClaude_SuccessNotDiscarded(t *testing.T) {
	// Successful cmd.Run should return the result even if
	// the context is not fresh (regression test for gating
	// ctx.Err() on runErr != nil).
	bin := fakeClaudeBin(
		t, `[{"type":"result","result":"OK","modelUsage":{"m1":{}}}]`, 0,
	)
	result, err := generateClaude(
		context.Background(), bin, "test", nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Content != "OK" {
		t.Errorf("content = %q, want OK", result.Content)
	}
}

func TestParseCLIResult(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantResult string
		wantModel  string
	}{
		{
			name:       "array format with result event",
			input:      `[{"type":"result","result":"# Summary","modelUsage":{"claude-3":{}}}]`,
			wantResult: "# Summary",
			wantModel:  "claude-3",
		},
		{
			name:       "legacy single-object format",
			input:      `{"result":"legacy result","model":"old-model"}`,
			wantResult: "legacy result",
			wantModel:  "old-model",
		},
		{
			name:       "array with no result event",
			input:      `[{"type":"system_prompt","content":"hello"},{"type":"turn","result":""}]`,
			wantResult: "",
			wantModel:  "",
		},
		{
			name:       "empty input",
			input:      ``,
			wantResult: "",
			wantModel:  "",
		},
		{
			name:       "garbage input",
			input:      `not json at all`,
			wantResult: "",
			wantModel:  "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, model := parseCLIResult([]byte(tc.input))
			if result != tc.wantResult {
				t.Errorf("result: got %q, want %q", result, tc.wantResult)
			}
			if model != tc.wantModel {
				t.Errorf("model: got %q, want %q", model, tc.wantModel)
			}
		})
	}
}

func TestGenerateClaude_TruncatesLargeStdoutLogEvent(t *testing.T) {
	largeResult := strings.Repeat("x", claudeStdoutLogMaxBytes*2)
	stdout := fmt.Sprintf(`[{"type":"result","result":%q,"modelUsage":{"m1":{}}}]`, largeResult)
	bin := fakeClaudeBin(t, stdout, 0)

	var logs []LogEvent
	result, err := generateClaude(
		context.Background(),
		bin,
		"test",
		func(ev LogEvent) { logs = append(logs, ev) },
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Content != largeResult {
		t.Fatalf("result content was truncated unexpectedly")
	}

	var stdoutLog string
	for _, ev := range logs {
		if ev.Stream == "stdout" {
			stdoutLog = ev.Line
			break
		}
	}
	if stdoutLog == "" {
		t.Fatalf("expected stdout log event")
	}
	if !strings.Contains(stdoutLog, "[truncated ") {
		t.Fatalf("expected truncation marker in stdout log, got %q", stdoutLog)
	}
	if len(stdoutLog) >= len(stdout) {
		t.Fatalf("expected truncated stdout log to be smaller than raw payload")
	}
}
