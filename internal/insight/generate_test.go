package insight

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantError)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, result)
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
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantError)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, result)
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

	require.Len(t, got, 2)
	assert.Equal(t, "stderr", got[0].Stream)
	assert.Len(t, got[0].Line, len(longLine))
	assert.Equal(t, "small-line", got[1].Line)
	assert.Contains(t, text, "small-line")
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
	assert.Equal(t, "sk-secret", envMap["ANTHROPIC_API_KEY"], "ANTHROPIC_API_KEY should be preserved")
	assert.Equal(t, "custom-val", envMap["CUSTOM_VAR"], "CUSTOM_VAR should be preserved")
	assert.Equal(t, "1", envMap["CLAUDE_NO_SOUND"])
}

func TestValidAgents(t *testing.T) {
	for _, agent := range []string{
		"claude", "codex", "copilot", "gemini", "kiro",
	} {
		assert.True(t, ValidAgents[agent], "%s should be valid", agent)
	}
	assert.False(t, ValidAgents["gpt"], "gpt should not be valid")
}

func createMockBinary(
	t *testing.T, stdout string, exitCode int, writeArgs bool, name string,
) (bin, argsFile string) {
	t.Helper()
	dir := t.TempDir()
	dataFile := filepath.Join(dir, "stdout.txt")
	require.NoError(t, os.WriteFile(dataFile, []byte(stdout), 0o644))

	if writeArgs {
		argsFile = filepath.Join(dir, "args.txt")
	}

	if runtime.GOOS == "windows" {
		bin = filepath.Join(dir, name+".cmd")
		var script string
		if writeArgs {
			script = fmt.Sprintf(
				"@echo off\r\n@break > %q\r\n@:write_args\r\n@if \"%%~1\"==\"\" goto done_args\r\n@>> %q echo %%~1\r\n@shift\r\n@goto write_args\r\n@:done_args\r\n@type %q\r\n@exit /b %d\r\n",
				argsFile,
				argsFile,
				dataFile,
				exitCode,
			)
		} else {
			script = fmt.Sprintf("@type %q\r\n@exit /b %d\r\n", dataFile, exitCode)
		}
		require.NoError(t, os.WriteFile(bin, []byte(script), 0o755))
		return bin, argsFile
	}

	bin = filepath.Join(dir, name)
	var script string
	if writeArgs {
		script = fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %s\nprintf '%%s' %s\nexit %d\n", shellQuote(argsFile), shellQuote(stdout), exitCode)
	} else {
		script = fmt.Sprintf("#!/bin/sh\nprintf '%%s' %s\nexit %d\n", shellQuote(stdout), exitCode)
	}
	require.NoError(t, os.WriteFile(bin, []byte(script), 0o755))
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

func readArgLines(t *testing.T, argsFile string) []string {
	t.Helper()
	argsData, err := os.ReadFile(argsFile)
	require.NoError(t, err, "reading args")
	lines := strings.Split(strings.TrimSpace(string(argsData)), "\n")
	args := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			args = append(args, line)
		}
	}
	return args
}

func TestGenerateStreamWithOptions_UsesConfiguredBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test not supported on windows")
	}

	stdout := `[{"type":"result","result":"OK","modelUsage":{"m1":{}}}]`
	bin, _ := createMockBinary(t, stdout, 0, false, "custom-claude")
	t.Setenv("PATH", "/bin:/usr/bin")

	result, err := GenerateStreamWithOptions(
		context.Background(), "claude", "test prompt", nil,
		GenerateOptions{
			Agents: map[string]AgentConfig{
				"claude": {Binary: bin},
			},
		},
	)
	require.NoError(t, err)
	assert.Equal(t, "OK", result.Content)
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
	require.NoError(t, err)
	assert.Equal(t, "OK", result.Content)

	argsData, err := os.ReadFile(argsFile)
	require.NoError(t, err, "reading args")
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
		assert.Contains(t, joined, want)
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
	require.NoError(t, err)
	assert.Equal(t, "OK", result.Content)

	argsData, err := os.ReadFile(argsFile)
	require.NoError(t, err, "reading args")
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
	assert.Equal(t, wantArgs, args)
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
	require.NoError(t, err)
	assert.Equal(t, "Hello from copilot", result.Content)
	assert.Equal(t, "copilot", result.Agent)

	argsData, err := os.ReadFile(argsFile)
	require.NoError(t, err, "reading args")
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
	assert.Equal(t, wantArgs, args)
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
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty result")
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
	require.NoError(t, err)
	assert.Contains(t, result.Content, "\n\n", "blank lines lost")
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
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantResult, result.Content)
			assert.Equal(t, "claude", result.Agent)
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

func fakeGeminiBinWithEnvCapture(
	t *testing.T, stdout string, exitCode int,
) (bin, argsFile, envFile string) {
	t.Helper()
	dir := t.TempDir()
	dataFile := filepath.Join(dir, "stdout.txt")
	require.NoError(t, os.WriteFile(dataFile, []byte(stdout), 0o644))
	argsFile = filepath.Join(dir, "args.txt")
	envFile = filepath.Join(dir, "env.txt")

	if runtime.GOOS == "windows" {
		bin = filepath.Join(dir, "gemini.cmd")
		script := fmt.Sprintf(
			"@echo off\r\n@break > %q\r\n@:write_args\r\n@if \"%%~1\"==\"\" goto done_args\r\n@>> %q echo %%~1\r\n@shift\r\n@goto write_args\r\n@:done_args\r\n@echo %%GEMINI_SANDBOX%%> %q\r\n@type %q\r\n@exit /b %d\r\n",
			argsFile,
			argsFile,
			envFile,
			dataFile,
			exitCode,
		)
		require.NoError(t, os.WriteFile(bin, []byte(script), 0o755))
		return bin, argsFile, envFile
	}

	bin = filepath.Join(dir, "gemini")
	script := fmt.Sprintf(
		"#!/bin/sh\nprintf '%%s\\n' \"$@\" > %s\nprintf '%%s' \"$GEMINI_SANDBOX\" > %s\ncat %s\nexit %d\n",
		shellQuote(argsFile),
		shellQuote(envFile),
		shellQuote(dataFile),
		exitCode,
	)
	require.NoError(t, os.WriteFile(bin, []byte(script), 0o755))
	return bin, argsFile, envFile
}

func TestGenerateGemini_ModelFlag(t *testing.T) {
	streamJSON := `{"type":"message","role":"assistant","content":"Hello"}
{"type":"result","result":"# Analysis"}
`

	bin, argsFile := fakeGeminiBin(t, streamJSON, 0)

	result, err := generateGemini(
		context.Background(), bin, "test prompt", nil,
		AgentConfig{AllowUnsafe: true},
	)
	require.NoError(t, err)

	assert.Equal(t, "# Analysis", result.Content)
	assert.Equal(t, "gemini", result.Agent)
	assert.Equal(t, geminiInsightModel, result.Model)

	// Verify the CLI was invoked with --model flag.
	args := readArgLines(t, argsFile)

	wantArgs := []string{
		"--model", geminiInsightModel,
		"--output-format", "stream-json",
	}
	assert.Equal(t, wantArgs, args)
}

func TestGenerateGemini_RequiresSandboxOrUnsafeOptIn(t *testing.T) {
	bin, _ := fakeGeminiBin(t, "", 0)

	_, err := generateGemini(
		context.Background(), bin, "test prompt", nil,
		AgentConfig{},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "explicit sandbox or unsafe opt-in")
}

func TestGenerateGemini_SetsSandboxEnv(t *testing.T) {
	streamJSON := `{"type":"result","result":"# Analysis"}`
	bin, argsFile, envFile := fakeGeminiBinWithEnvCapture(
		t, streamJSON, 0,
	)

	result, err := generateGemini(
		context.Background(), bin, "test prompt", nil,
		AgentConfig{Sandbox: "sandbox-exec"},
	)
	require.NoError(t, err)
	assert.Equal(t, "# Analysis", result.Content)
	assert.Equal(t, []string{
		"--model", geminiInsightModel,
		"--output-format", "stream-json",
	}, readArgLines(t, argsFile))
	envData, err := os.ReadFile(envFile)
	require.NoError(t, err)
	assert.Equal(t, "sandbox-exec", strings.TrimSpace(string(envData)))
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
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cancel")
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
	require.NoError(t, err)
	assert.Equal(t, "OK", result.Content)
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
			assert.Equal(t, tc.wantResult, result)
			assert.Equal(t, tc.wantModel, model)
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
	require.NoError(t, err)
	assert.Equal(t, largeResult, result.Content, "result content was truncated unexpectedly")

	var stdoutLog string
	for _, ev := range logs {
		if ev.Stream == "stdout" {
			stdoutLog = ev.Line
			break
		}
	}
	require.NotEmpty(t, stdoutLog, "expected stdout log event")
	assert.Contains(t, stdoutLog, "[truncated ", "expected truncation marker in stdout log")
	assert.Less(t, len(stdoutLog), len(stdout), "expected truncated stdout log to be smaller than raw payload")
}
