package db

import (
	"context"
	"database/sql"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClassifyIssueFailure(t *testing.T) {
	tests := []struct {
		name, tool, status, input, result, reason string
		failed                                    bool
	}{
		{"successful apply patch", "apply_patch", "completed", "apply_patch failed_edit", "Done!", "", false},
		{"successful psql", "shell_command", "success", "psql -f migration.sql", "INSERT 0 1", "", false},
		{"successful GitHub call", "shell_command", "completed", "gh api repos/example", "github response received", "", false},
		{"successful credential check", "shell_command", "succeeded", "check credential", "credential is present", "", false},
		{"successful wrapped tool discovery", "exec", "completed", "inspect tool schema", "Script completed\nOutput: timeout_ms controls the request timeout", "", false},
		{"successful documentation output", "webfetch", "completed", "fetch documentation", "Error handling, failed retries, and timeout configuration", "", false},
		{"successful read containing error", "read_file", "completed", "read source", "error: this is example source text", "", false},
		{"successful read containing test summary", "read_file", "completed", "read log", "3 tests failed, 10 passed", "", false},
		{"quoted error with exit zero", "shell_command", "", "run build", `output="error: quoted text" process exited with code 0`, "", false},
		{"completed ParserError", "shell_command", "completed", "powershell command", "ParserError: unexpected token", "windows_shell", true},
		{"completed exit code one", "shell_command", "completed", "run command", "Process exited with code 1", "command_failure", true},
		{"lowercase wrapped failure", "shell_command", "completed", "run command", "script failed\nexit code: 2\noutput:\nAccess is denied", "permission_auth", true},
		{"completed invalid context", "apply_patch", "completed", "apply_patch", "Invalid Context 42", "failed_edit", true},
		{"nonzero wins over exit zero", "shell_command", "completed", "run command", "Process exited with code 0; Process exited with code 1", "command_failure", true},
		{"plain successful output", "apply_patch", "", "apply_patch", "patch applied", "", false},
		{"failed patch", "apply_patch", "errored", "apply_patch", "invalid context", "failed_edit", true},
		{"failed psql", "shell_command", "errored", "psql -f migration.sql", "relation exists", "build_test", true},
		{"failed GitHub call", "shell_command", "error", "gh api repos/example", "request rejected", "git_github_ci", true},
		{"input words do not choose failure family", "exec", "error", "tool schema mentions timeout and network", "request rejected", "command_failure", true},
		{"bash quoting failure", "shell_command", "error", "bash script", "unexpected EOF while looking for matching `'`", "shell_syntax", true},
		{"nonzero PowerShell", "shell_command", "", "powershell command", "ParserError: process exited with code 1", "windows_shell", true},
		{"search no match", "rg", "errored", `rg "error" files`, "process exited with code 1", "", false},
		{"shell search no match", "shell_command", "completed", `{"command":"rg missing files"}`, "Script failed\nExit code: 1", "", false},
		{"shell search real error", "shell_command", "completed", `{"command":"rg missing absent-dir"}`, "absent-dir: no such file or directory\nExit code: 2", "missing_file", true},
		{"logical test failure with exit zero", "shell_command", "completed", "run tests", "3 tests failed, 10 passed\nProcess exited with code 0", "build_test", true},
		{"GitHub API failure with exit zero", "shell_command", "completed", "gh api repos/example/issues/42", "HTTP status 500\nProcess exited with code 0", "git_github_ci", true},
		{"GitHub issue failure", "shell_command", "error", "inspect https://github.com/example/project/issues/42", "request rejected", "git_github_ci", true},
		{"cancelled", "tool", "cancelled", "operation", "", "generic_tool_failure", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, failed := ClassifyIssueFailure(tt.tool, tt.status, tt.input, tt.result)
			assert.Equal(t, tt.failed, failed)
			assert.Equal(t, tt.reason, reason)
		})
	}
}

func TestHasLogicalFailure(t *testing.T) {
	tests := []struct {
		name, input, result string
		want                bool
	}{
		{"failed test count", "run tests", "3 tests failed, 10 passed", true},
		{"npm error line", "npm test", "npm ERR! lifecycle failed", true},
		{"fatal line", "git fetch", "fatal: repository unavailable", true},
		{"HTTP failure", "request API", "HTTP status 500", true},
		{"unrelated status", "show status", "status 500", false},
		{"successful tests", "run tests", "10 tests passed", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, hasLogicalFailure(tt.input, strings.ToLower(tt.result)))
		})
	}
}

func TestCanonicalGitHubReference(t *testing.T) {
	tests := []struct {
		name, input, want string
	}{
		{"URL", "https://github.com/Owner/Repo/issues/42", "owner/repo#42"},
		{"mixed-case URL", "HTTPS://GitHub.Com/Owner/Repo/Issues/43", "owner/repo#43"},
		{"short reference", "Owner/Repo#44", "owner/repo#44"},
		{"URL takes priority", "https://github.com/one/repo/issues/45 and two/repo#46", "one/repo#45"},
		{"unrelated", "build completed", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, canonicalGitHubReference(tt.input))
		})
	}
}

func TestFirstIssueLineSkipsExecutionWrapper(t *testing.T) {
	result := "Script failed\nWall time: 0.4 seconds\nProcess exited with code 1\nFinal output:\nParserError: unexpected token"
	assert.Equal(t, "ParserError: unexpected token", firstIssueLine(result, "run command"))
	blocks := `[{"type":"text","text":"Script failed\r\nWall time 0.2 seconds\r\nScript error:\r\nExit code 1\r\nFinal output:\r\nParserError: unexpected token"}]`
	assert.Equal(t, "ParserError: unexpected token", firstIssueLine(blocks, "run command"))
	truncatedBlocks := `[{"type":"input_text","text":"Script failed\nWall time 0.2 seconds\n"},{"type":"input_text","text":"Script error:\nExit code 1\nFinal output:\nParserError: truncated content block`
	assert.Equal(t, "ParserError: truncated content block", firstIssueLine(truncatedBlocks, "run command"))
	assert.Equal(t, "ParserError: unexpected token", firstIssueLine(`Script failed\r\nWall time 0.2 seconds\r\nExit code 1\r\nFinal output:\r\nParserError: unexpected token`, "run command"))
	assert.Equal(t, "run command", firstIssueLine("Script failed\nExit code: 1", `{"command":"run command"}`))
	assert.Equal(t, "fatal: repository unavailable", firstIssueLine("Preparing repository checkout\nfatal: repository unavailable", "git fetch"))
}

func TestJoinIssueReviewResultPreservesFailureTail(t *testing.T) {
	result := JoinIssueReviewResult(strings.Repeat("progress ", 200), "ParserError: failure near the tail")
	assert.Equal(t, "ParserError: failure near the tail", firstIssueLine(result, "run command"))
}

func TestIssueFailureConfidencePrefersStructuredEvidence(t *testing.T) {
	assert.Equal(t, "high", issueFailureConfidence("errored", "run", "request rejected"))
	assert.Equal(t, "high", issueFailureConfidence("completed", "run", "Exit code: 2"))
	assert.Equal(t, "medium", issueFailureConfidence("completed", "run", "ParserError: unexpected token"))
}

func TestEffectiveIssueTool(t *testing.T) {
	tests := []struct {
		name, tool, input, want string
	}{
		{"direct tool", "shell_command", "go test ./...", "shell_command"},
		{"single nested tool", "exec", "const r = await tools.shell_command({command: \"go test ./...\"}); text(r)", "shell_command"},
		{"repeated same nested tool", "functions.exec", "await Promise.all([tools.view_image(a), tools.view_image(b)])", "view_image"},
		{"mixed nested tools", "exec", "await Promise.all([tools.shell_command(a), tools.view_image(b)])", "exec"},
		{"tool discovery wrapper", "exec", "ALL_TOOLS.filter(x => x.name.includes('git'))", "exec"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, effectiveIssueTool(tt.tool, tt.input))
		})
	}

	response := AnalyzeIssueReview(
		[]IssueReviewSession{{ID: "s1", Project: "alpha", Date: "2026-08-01"}},
		nil,
		[]IssueReviewToolCall{{SessionID: "s1", Tool: "exec", Input: "await tools.apply_patch(patch)", Result: "Invalid Context 42", EventStatus: "errored"}},
		nil,
		IssueReviewQuery{Limit: 100},
	)
	findings := findingsByReason(response.Findings)["failed_edit"]
	require.Len(t, findings, 1)
	assert.Equal(t, "apply_patch", findings[0].Tool)
}

func TestSanitizeTelemetryTail(t *testing.T) {
	raw := `router failed: error="denied" token="quoted-secret" credential=plain-secret path="C:\Users\alice\private.txt" cwd=/home/alice/private Bearer bearer-secret`
	got := sanitizeTelemetryTail(raw)
	for _, secret := range []string{"quoted-secret", "plain-secret", `C:\Users\alice`, "/home/alice", "bearer-secret"} {
		assert.NotContains(t, got, secret)
	}
	assert.Contains(t, got, "token=<redacted>")
	assert.Contains(t, got, "credential=<redacted>")
	assert.Contains(t, got, "path=<path>")
	assert.Contains(t, got, "cwd=<path>")
}

func TestNormalizeIssueTextCollapsesVolatileValues(t *testing.T) {
	left := `RUN "C:\work\alpha\build.ps1" "/home/alice/run/" 2026-08-09 123.45 915e83b`
	right := `run "C:\work\beta\build.ps1" "/srv/build/run/" 2025-01-02 999.10 abcdef0`
	assert.Equal(t, normalizeIssueText(left), normalizeIssueText(right))
	assert.Equal(t, `run "<path>" "<path>" #-#-# # #`, normalizeIssueText(left))
}

func TestAnalyzeIssueReviewRedactsFindingText(t *testing.T) {
	secretValue := "sk-ant-api03-" + "Nc6Mp1Hj9Bg3Tf5Ds8Lr0E"
	escapedValue := "N5LWA1Fcx0KoUYBsEedwj2PMOphtXgC6aRkv3DJQ"
	telemetrySecret := "telemetry-secret-value"
	windowsPath := `C:\Users\alice\private\build.log`
	windowsSlashPath := "C:/Users/alice/private/build.log"
	unixPath := "/home/alice/private/build.log"
	response := AnalyzeIssueReview(
		[]IssueReviewSession{{ID: "s1", Project: "alpha", Date: "2026-08-01"}},
		nil,
		[]IssueReviewToolCall{{SessionID: "s1", Tool: "shell_command", Input: `{"command":"KEY=\"` + escapedValue + `\"; run ` + windowsPath + `"}`, Result: "error: token=" + secretValue + " paths=" + windowsSlashPath + " and " + unixPath, EventStatus: "errored"}},
		[]IssueReviewTelemetry{{SessionID: "s1", Target: "codex_core::tools::router", Level: "ERROR", Body: "router failed token=" + telemetrySecret}},
		IssueReviewQuery{Limit: 100},
	)
	require.NotEmpty(t, response.Findings)
	for _, finding := range response.Findings {
		for _, private := range []string{secretValue, escapedValue, telemetrySecret, windowsPath, windowsSlashPath, unixPath} {
			assert.NotContains(t, finding.Signature, private)
			assert.NotContains(t, finding.Recommendation, private)
		}
		for _, evidence := range finding.Evidence {
			for _, private := range []string{secretValue, escapedValue, telemetrySecret, windowsPath, windowsSlashPath, unixPath} {
				assert.NotContains(t, evidence.Excerpt, private)
			}
		}
	}
}

func TestParseLogFieldsQuotedUnquotedAndMalformed(t *testing.T) {
	fields := parseLogFields(`tool_name="shell command" call_id=call-1 total_duration_ms="31000" malformed="unterminated`)
	assert.Equal(t, "shell command", fields["tool_name"])
	assert.Equal(t, "call-1", fields["call_id"])
	assert.Equal(t, "31000", fields["total_duration_ms"])
	assert.Equal(t, `"unterminated`, fields["malformed"])

	fields = parseLogFields(`call_id=call-2 total_duration_ms=not-a-number`)
	assert.Equal(t, "not-a-number", fields["total_duration_ms"])
}

func TestAnalyzeIssueReviewRecoveryAndOptimizationStatus(t *testing.T) {
	sessions := []IssueReviewSession{
		{ID: "s1", Project: "alpha", Date: "2026-08-01"},
		{ID: "s2", Project: "beta", Date: "2026-08-02"},
	}
	longInput := strings.Repeat("deploy verification step; ", 9)
	calls := []IssueReviewToolCall{
		{SessionID: "s1", Tool: "apply_patch", Input: "replace expected block in file", Result: "invalid context", EventStatus: "errored", MessageOrdinal: 1},
		{SessionID: "s1", Tool: "apply_patch", Input: "replace expected block in file", Result: "Done!", EventStatus: "completed", MessageOrdinal: 2},
		{SessionID: "s1", Tool: "status", Input: "check", Result: "ok", EventStatus: "completed", MessageOrdinal: 3},
		{SessionID: "s1", Tool: "status", Input: "check", Result: "ok", EventStatus: "completed", MessageOrdinal: 4},
		{SessionID: "s1", Tool: "status", Input: "check", Result: "ok", EventStatus: "completed", MessageOrdinal: 5},
		{SessionID: "s1", Tool: "read_file", Input: `{"path":"notes.md"}`, Result: "ok", EventStatus: "completed", MessageOrdinal: 6},
		{SessionID: "s1", Tool: "read_file", Input: `{"path":"notes.md"}`, Result: "ok", EventStatus: "completed", MessageOrdinal: 7},
		{SessionID: "s1", Tool: "read_file", Input: `{"path":"notes.md"}`, Result: "ok", EventStatus: "completed", MessageOrdinal: 8},
		{SessionID: "s1", Tool: "shell_command", Input: longInput, Result: "ok", EventStatus: "completed", MessageOrdinal: 9},
		{SessionID: "s1", Tool: "shell_command", Input: "open missing file", Result: "file not found", EventStatus: "errored", MessageOrdinal: 10},
		{SessionID: "s1", Tool: "status", Input: "repair state", Result: "ok", EventStatus: "completed", MessageOrdinal: 11},
		{SessionID: "s1", Tool: "shell_command", Input: "open missing file", Result: "ok", EventStatus: "completed", MessageOrdinal: 12},
		{SessionID: "s2", Tool: "shell_command", Input: longInput, Result: "ok", EventStatus: "completed", MessageOrdinal: 1},
	}
	response := AnalyzeIssueReview(sessions, nil, calls, nil, IssueReviewQuery{Limit: 100})
	byReason := findingsByReason(response.Findings)
	require.NotEmpty(t, byReason["failed_edit"])
	assert.Equal(t, "recovered", byReason["failed_edit"][0].Status)
	assert.True(t, byReason["failed_edit"][0].Evidence[0].Recovered)
	require.NotEmpty(t, byReason["repeated_polling"])
	assert.Equal(t, "observed", byReason["repeated_polling"][0].Status)
	assert.False(t, byReason["repeated_polling"][0].Evidence[0].Recovered)
	require.NotEmpty(t, byReason["repeated_read"])
	assert.Contains(t, byReason["repeated_read"][0].Recommendation, "Cache this stable read")
	require.NotEmpty(t, byReason["repeated_workflow"])
	assert.Equal(t, "recurring", byReason["repeated_workflow"][0].Status)
	assert.False(t, byReason["repeated_workflow"][0].Evidence[0].Recovered)
	assert.Equal(t, "skill", byReason["repeated_workflow"][0].RecommendationType)
	require.NotEmpty(t, byReason["missing_file"])
	assert.Equal(t, "recovered", byReason["missing_file"][0].Status)
	assert.True(t, byReason["missing_file"][0].Evidence[0].Recovered)
}

func TestAnalyzeIssueReviewRecoveryAllowsDiagnostics(t *testing.T) {
	calls := []IssueReviewToolCall{
		{SessionID: "s1", Tool: "shell_command", Input: `{"command":"open missing file","timeout_ms":1000}`, Result: "file not found", EventStatus: "errored", MessageOrdinal: 1},
		{SessionID: "s1", Tool: "read_file", Input: `{"path":"notes.md"}`, Result: "ok", EventStatus: "completed", MessageOrdinal: 2},
		{SessionID: "s1", Tool: "status", Input: "check", Result: "ok", EventStatus: "completed", MessageOrdinal: 3},
		{SessionID: "s1", Tool: "shell_command", Input: `{"timeout_ms":3000,"command":"open missing file"}`, Result: "ok", EventStatus: "completed", MessageOrdinal: 4},
	}
	response := AnalyzeIssueReview([]IssueReviewSession{{ID: "s1", Project: "alpha", Date: "2026-08-01"}}, nil, calls, nil, IssueReviewQuery{Limit: 100})
	findings := findingsByReason(response.Findings)["missing_file"]
	require.Len(t, findings, 1)
	assert.Equal(t, "recovered", findings[0].Status)
	assert.True(t, findings[0].Evidence[0].Recovered)
}

func TestAnalyzeIssueReviewRecoveryStopsAtMutation(t *testing.T) {
	for _, tool := range []string{"apply_patch", "write_file"} {
		t.Run(tool, func(t *testing.T) {
			calls := []IssueReviewToolCall{
				{SessionID: "s1", Tool: "shell_command", Input: `{"command":"open missing file"}`, Result: "file not found", EventStatus: "errored", MessageOrdinal: 1},
				{SessionID: "s1", Tool: tool, Input: "change file", Result: "ok", EventStatus: "completed", MessageOrdinal: 2},
				{SessionID: "s1", Tool: "shell_command", Input: `{"command":"open missing file"}`, Result: "ok", EventStatus: "completed", MessageOrdinal: 3},
			}
			response := AnalyzeIssueReview([]IssueReviewSession{{ID: "s1", Project: "alpha", Date: "2026-08-01"}}, nil, calls, nil, IssueReviewQuery{Limit: 100})
			findings := findingsByReason(response.Findings)["missing_file"]
			require.Len(t, findings, 1)
			assert.NotEqual(t, "recovered", findings[0].Status)
			assert.False(t, findings[0].Evidence[0].Recovered)
		})
	}
}

func TestIsAssistantBlockerRequiresConcreteBroadFailure(t *testing.T) {
	message := IssueReviewMessage{SourceType: "event_msg", SourceSubtype: "commentary"}
	assert.False(t, isAssistantBlocker(message, "The project hit a major milestone and the release remains on schedule."))
	assert.True(t, isAssistantBlocker(message, "The database dump finished, but checkpoint finalization hit a local PowerShell argument bug while reading its count manifest."))
	assert.True(t, isAssistantBlocker(message, "The drill exposed a normal isolated-container setup issue before restore."))
}

func TestAnalyzeIssueReviewFlagsOnlyPersistentRepeatedWaits(t *testing.T) {
	tests := []struct {
		name, wantReason string
		count            int
	}{
		{name: "three waits remain normal", count: 3},
		{name: "four waits are persistent polling", count: 4, wantReason: "repeated_polling"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := make([]IssueReviewToolCall, tt.count)
			for i := range calls {
				calls[i] = IssueReviewToolCall{SessionID: "s1", Tool: "wait", Input: "job-1", Result: "still running", EventStatus: "completed", MessageOrdinal: i + 1}
			}
			response := AnalyzeIssueReview([]IssueReviewSession{{ID: "s1", Project: "alpha", Date: "2026-08-01"}}, nil, calls, nil, IssueReviewQuery{Limit: 100})
			findings := findingsByReason(response.Findings)["repeated_polling"]
			if tt.wantReason == "" {
				assert.Empty(t, findings)
				return
			}
			require.Len(t, findings, 1)
			assert.Equal(t, tt.wantReason, findings[0].ReasonCode)
			assert.Equal(t, tt.count, findings[0].Occurrences)
		})
	}
}

func TestAnalyzeIssueReviewDeduplicatesImportedCopies(t *testing.T) {
	sessions := []IssueReviewSession{{ID: "s1", Project: "alpha", Date: "2026-08-01"}, {ID: "s2", Project: "alpha", Date: "2026-08-02"}}
	messages := []IssueReviewMessage{
		{SessionID: "s1", Role: "user", StableID: "message-1", Content: "Initial request"},
		{SessionID: "s2", Role: "user", StableID: "message-1", Content: "Initial request"},
		{SessionID: "s1", Role: "user", StableID: "message-2", Content: "something is off"},
		{SessionID: "s2", Role: "user", StableID: "message-2", Content: "something is off"},
	}
	calls := []IssueReviewToolCall{
		{SessionID: "s1", Tool: "shell_command", ToolUseID: "call-1", Input: "run", Result: "file not found", EventStatus: "errored"},
		{SessionID: "s2", Tool: "shell_command", ToolUseID: "call-1", Input: "run", Result: "file not found", EventStatus: "errored"},
	}

	response := AnalyzeIssueReview(sessions, messages, calls, nil, IssueReviewQuery{Limit: 100})
	assert.Equal(t, 4, response.ScannedMessages)
	assert.Equal(t, 2, response.AnalyzedMessages)
	assert.Equal(t, 2, response.DuplicateMessages)
	assert.Equal(t, 2, response.ScannedToolCalls)
	assert.Equal(t, 1, response.AnalyzedToolCalls)
	assert.Equal(t, 1, response.DuplicateToolCalls)
	require.Len(t, findingsByReason(response.Findings)["missing_file"], 1)
	assert.Equal(t, 1, findingsByReason(response.Findings)["missing_file"][0].Occurrences)
}

func TestAnalyzeIssueReviewGroupsGitHubReferences(t *testing.T) {
	sessions := []IssueReviewSession{{ID: "s1", Project: "alpha", Date: "2026-08-01"}, {ID: "s2", Project: "beta", Date: "2026-08-02"}}
	calls := []IssueReviewToolCall{
		{SessionID: "s1", Tool: "shell_command", ToolUseID: "call-1", Input: "gh issue view https://github.com/Owner/Repo/issues/42", Result: "ok", EventStatus: "completed"},
		{SessionID: "s2", Tool: "shell_command", ToolUseID: "call-2", Input: "gh issue view owner/repo#42", Result: "ok", EventStatus: "completed"},
		{SessionID: "s2", Tool: "shell_command", ToolUseID: "call-3", Input: "gh issue view owner/repo#43", Result: "ok", EventStatus: "completed"},
	}

	response := AnalyzeIssueReview(sessions, nil, calls, nil, IssueReviewQuery{Limit: 100})
	findings := findingsByReason(response.Findings)["github_issue_reference"]
	require.Len(t, findings, 2)
	byReference := map[string]IssueReviewFinding{}
	for _, finding := range findings {
		byReference[finding.GitHubReference] = finding
	}
	assert.Equal(t, 2, byReference["owner/repo#42"].Occurrences)
	assert.Equal(t, 1, byReference["owner/repo#43"].Occurrences)
	assert.Contains(t, byReference["owner/repo#42"].Recommendation, "owner/repo#42")
}

func TestAnalyzeIssueReviewScansLongCorrectionsAndCommentary(t *testing.T) {
	session := IssueReviewSession{ID: "s1", Project: "alpha", Date: "2026-08-01"}
	messages := []IssueReviewMessage{
		{SessionID: "s1", Role: "user", Content: "Initial request", Ordinal: 0},
		{SessionID: "s1", Role: "user", Content: strings.Repeat("context ", 200) + "something is off with that result", Ordinal: 1},
		{SessionID: "s1", Role: "assistant", Content: strings.Repeat("detail ", 200) + "root cause confirmed: the command failed because there is no such file or directory", Ordinal: 2, SourceType: "event_msg", SourceSubtype: "commentary"},
	}

	response := AnalyzeIssueReview([]IssueReviewSession{session}, messages, nil, nil, IssueReviewQuery{Limit: 100})
	byReason := findingsByReason(response.Findings)
	require.NotEmpty(t, byReason["user_correction"])
	require.NotEmpty(t, byReason["missing_file"])
}

func TestAnalyzeIssueReviewFindsRepeatedUserRequests(t *testing.T) {
	sessions := []IssueReviewSession{
		{ID: "s1", Project: "alpha", Date: "2026-08-01"},
		{ID: "s2", Project: "beta", Date: "2026-08-02"},
		{ID: "s3", Project: "beta", Date: "2026-08-03"},
	}
	messages := []IssueReviewMessage{
		{SessionID: "s1", Role: "user", Content: "Please audit project 123 and suggest a reusable verification workflow", Ordinal: 0},
		{SessionID: "s2", Role: "user", Content: "Please audit project 456 and suggest a reusable verification workflow", Ordinal: 0},
		{SessionID: "s3", Role: "user", Content: "Please audit project 789 but only summarize the current test output", Ordinal: 0},
		{SessionID: "s3", Role: "user", Content: "<environment_context>repeated injected context that must be ignored</environment_context>", Ordinal: 1},
	}

	response := AnalyzeIssueReview(sessions, messages, nil, nil, IssueReviewQuery{Reason: "repeated_question", Limit: 100})
	require.Len(t, response.Findings, 1)
	finding := response.Findings[0]
	assert.Equal(t, 2, finding.Occurrences)
	assert.Equal(t, 2, finding.SessionCount)
	assert.Equal(t, 2, finding.ProjectCount)
	assert.Equal(t, "high", finding.Confidence)
	assert.Equal(t, "skill", finding.RecommendationType)
}

func TestAnalyzeIssueReviewIgnoresHarnessEnvelopes(t *testing.T) {
	sessions := []IssueReviewSession{{ID: "s1"}, {ID: "s2"}}
	tests := []struct {
		name, marker string
	}{
		{name: "task notification", marker: "<task-notification>"},
		{name: "subagent notification", marker: "<subagent_notification>"},
		{name: "follow-up instruction", marker: "Perform any necessary follow-up actions in response to the subagent completion above"},
		{name: "brief result instruction", marker: "Briefly inform the user about the task result"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content := tt.marker + " repeated orchestration request across chats"
			messages := []IssueReviewMessage{
				{SessionID: "s1", Role: "user", Content: content},
				{SessionID: "s2", Role: "user", Content: content},
			}
			response := AnalyzeIssueReview(sessions, messages, nil, nil, IssueReviewQuery{Reason: "repeated_question", Limit: 100})
			assert.Empty(t, response.Findings)
		})
	}
}

func TestAnalyzeIssueReviewSlowToolUsesAllMeasuredSamples(t *testing.T) {
	session := IssueReviewSession{ID: "s1", Project: "alpha", Date: "2026-08-01"}
	durations := []*int64{ms(10000), ms(20000), ms(31000), ms(40000), ms(130000), nil, nil, ms(-1)}
	calls := make([]IssueReviewToolCall, len(durations))
	for i, duration := range durations {
		calls[i] = IssueReviewToolCall{SessionID: "s1", Tool: "builder", Input: "run step " + string(rune('a'+i)), Result: "ok", EventStatus: "completed", MessageOrdinal: i + 1, DurationMS: duration}
	}
	response := AnalyzeIssueReview([]IssueReviewSession{session}, nil, calls, nil, IssueReviewQuery{Limit: 100})
	findings := findingsByReason(response.Findings)["slow_tool"]
	require.Len(t, findings, 1)
	finding := findings[0]
	assert.Equal(t, 3, finding.Occurrences)
	require.NotNil(t, finding.P95DurationMS)
	assert.EqualValues(t, 130000, *finding.P95DurationMS)
	assert.InDelta(t, 5.0/8.0, finding.DurationCoverage, 0.0001)
	assert.Equal(t, "high", finding.Severity)
	assert.EqualValues(t, 201000, finding.TotalDurationMS)
	assert.EqualValues(t, 111000, finding.WastedDurationMS)
	assert.False(t, finding.Evidence[0].Recovered)
	assert.False(t, math.Signbit(finding.DurationCoverage))
}

func TestFilterIssueReviewResponseControlsAndPagination(t *testing.T) {
	response := IssueReviewResponse{Findings: []IssueReviewFinding{
		{ID: "impact", ReasonCode: "missing_file", Tool: "shell_command", Sources: []string{"tool_result"}, Severity: "high", Confidence: "high", Status: "recurring", RecommendationType: "skill", Occurrences: 5, SessionCount: 3, ProjectCount: 2, WastedDurationMS: 900, TotalDurationMS: 900, LastSeen: "2026-08-01", rank: 500},
		{ID: "frequency", ReasonCode: "timeout", Tool: "exec", Sources: []string{"codex_log"}, Severity: "medium", Confidence: "high", Status: "observed", RecommendationType: "tool_fix", Occurrences: 10, SessionCount: 2, ProjectCount: 1, WastedDurationMS: 100, TotalDurationMS: 100, LastSeen: "2026-08-02", rank: 400},
		{ID: "recent", ReasonCode: "network", Tool: "webfetch", Sources: []string{"tool_result"}, Severity: "low", Confidence: "medium", Status: "open", RecommendationType: "rule", Occurrences: 3, SessionCount: 1, ProjectCount: 1, WastedDurationMS: 200, TotalDurationMS: 200, LastSeen: "2026-08-05", rank: 300},
		{ID: "waste", ReasonCode: "missing_dependency", Tool: "shell_command", Sources: []string{"tool_execution"}, Severity: "medium", Confidence: "medium", Status: "recovered", RecommendationType: "script", Occurrences: 2, SessionCount: 1, ProjectCount: 1, WastedDurationMS: 5000, TotalDurationMS: 500, LastSeen: "2026-08-03", rank: 200},
		{ID: "duration", ReasonCode: "build_test", Tool: "shell_command", Sources: []string{"tool_execution"}, Severity: "high", Confidence: "high", Status: "open", RecommendationType: "script", Occurrences: 2, SessionCount: 1, ProjectCount: 1, WastedDurationMS: 300, TotalDurationMS: 9000, LastSeen: "2026-08-04", rank: 100},
	}}

	filtered := filterIssueReviewResponse(response, IssueReviewQuery{
		Reason: "missing_file", Tool: "shell_command", Source: "tool_result",
		Severity: "high", Confidence: "high", Status: "recurring",
		RecommendationType: "skill", MinOccurrences: 5, MinSessions: 3,
		MinProjects: 2, MinWastedDurationMS: 900, Limit: 100,
	})
	require.Len(t, filtered.Findings, 1)
	assert.Equal(t, "impact", filtered.Findings[0].ID)

	for mode, want := range map[string]string{
		"impact": "impact", "frequency": "frequency", "recent": "recent",
		"waste": "waste", "duration": "duration",
	} {
		t.Run("sort_"+mode, func(t *testing.T) {
			got := filterIssueReviewResponse(response, IssueReviewQuery{Sort: mode, Limit: 100})
			require.NotEmpty(t, got.Findings)
			assert.Equal(t, want, got.Findings[0].ID)
		})
	}

	page := filterIssueReviewResponse(response, IssueReviewQuery{Sort: "impact", Offset: 1, Limit: 2})
	assert.Equal(t, 5, page.TotalFindings)
	assert.True(t, page.Truncated)
	require.Len(t, page.Findings, 2)
	assert.Equal(t, []string{"frequency", "recent"}, []string{page.Findings[0].ID, page.Findings[1].ID})

	last := filterIssueReviewResponse(response, IssueReviewQuery{Sort: "impact", Offset: 4, Limit: 2})
	assert.False(t, last.Truncated)
	require.Len(t, last.Findings, 1)
	assert.Equal(t, "duration", last.Findings[0].ID)
}

func TestGetAnalyticsIssueReviewFiltersAndEvidence(t *testing.T) {
	database := testDB(t)
	started := "2026-08-01T10:00:00Z"
	insertSession(t, database, "s1", "alpha", func(session *Session) {
		session.StartedAt = &started
		session.Cwd = `C:\work\alpha`
		session.Outcome = "errored"
		session.MessageCount = 2
	})
	insertSession(t, database, "s2", "alpha", func(session *Session) {
		session.StartedAt = &started
		session.Cwd = `C:\work\alpha`
		session.Outcome = "errored"
		session.MessageCount = 1
	})
	insertMessages(t, database,
		userMsgAt("s1", 0, "Run the Windows build", started),
		Message{
			SessionID: "s1", Ordinal: 1, Role: "assistant", Content: "running", Timestamp: started, HasToolUse: true,
			ToolCalls: []ToolCall{{
				SessionID: "s1", ToolName: "shell_command", ToolUseID: "call-1", InputJSON: `{"command":"bad syntax"}`,
				ResultEvents: []ToolResultEvent{
					{ToolUseID: "call-1", Source: "tool_execution", Status: "started", Timestamp: "2026-08-01T10:00:00Z", EventIndex: 0},
					{ToolUseID: "call-1", Source: "tool_execution", Status: "errored", Content: "ParserError: unexpected token", Timestamp: "2026-08-01T10:00:02Z", EventIndex: 1},
				},
			}},
		},
		userMsgAt("s2", 0, "Check the same project", started),
	)
	allSessions, err := database.issueReviewSessions(context.Background(), AnalyticsFilter{From: "2026-08-01", To: "2026-08-01", Timezone: "UTC"}, IssueReviewQuery{})
	require.NoError(t, err)
	require.Len(t, allSessions, 2)
	assert.Equal(t, "unknown", allSessions[0].Outcome)
	assert.Equal(t, `C:\work\alpha`, allSessions[0].CWD)

	response, err := database.GetAnalyticsIssueReview(context.Background(), AnalyticsFilter{From: "2026-08-01", To: "2026-08-01", Timezone: "UTC"}, IssueReviewQuery{Folder: `C:\work\alpha`, Outcome: "unknown", Reason: "windows_shell", Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, 2, response.ScannedSessions)
	assert.Equal(t, 1, response.ScannedToolCalls)
	require.Len(t, response.Findings, 1)
	finding := response.Findings[0]
	assert.Equal(t, "windows_shell", finding.ReasonCode)
	require.Len(t, finding.Evidence, 1)
	assert.Equal(t, "s1", finding.Evidence[0].SessionID)
	assert.Equal(t, `C:\work\alpha`, finding.Evidence[0].CWD)
	require.NotNil(t, finding.Evidence[0].MessageOrdinal)
	assert.Equal(t, 1, *finding.Evidence[0].MessageOrdinal)
	require.NotNil(t, finding.Evidence[0].CallIndex)
	assert.Equal(t, 0, *finding.Evidence[0].CallIndex)
	require.NotNil(t, finding.Evidence[0].DurationMS)
	assert.EqualValues(t, 2000, *finding.Evidence[0].DurationMS)
	require.Len(t, response.Facets.Session, 2)
	for _, facet := range response.Facets.Session {
		assert.NotEmpty(t, facet.Value)
		assert.NotEmpty(t, facet.Label)
	}

	chat, err := database.GetAnalyticsIssueReview(context.Background(), AnalyticsFilter{From: "2026-08-01", To: "2026-08-01", Timezone: "UTC"}, IssueReviewQuery{SessionID: "s1", Reason: "windows_shell", Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, 1, chat.ScannedSessions)
	require.Len(t, chat.Findings, 1)
	assert.Equal(t, "s1", chat.Findings[0].Evidence[0].SessionID)

	filtered, err := database.GetAnalyticsIssueReview(context.Background(), AnalyticsFilter{From: "2026-08-01", To: "2026-08-01", Timezone: "UTC"}, IssueReviewQuery{Reason: "missing_file", Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, filtered.Findings)
}

func TestGetAnalyticsIssueReviewCollectsRepeatedUserRequests(t *testing.T) {
	database := testDB(t)
	started := "2026-08-01T10:00:00Z"
	for _, sessionID := range []string{"s1", "s2"} {
		insertSession(t, database, sessionID, "alpha", func(session *Session) {
			session.StartedAt = &started
			session.MessageCount = 1
		})
		insertMessages(t, database, userMsgAt(sessionID, 0, "Can you check why this build keeps failing and create a reusable fix", started))
	}

	response, err := database.GetAnalyticsIssueReview(context.Background(), AnalyticsFilter{From: "2026-08-01", To: "2026-08-01", Timezone: "UTC"}, IssueReviewQuery{Reason: "repeated_question", MinSessions: 2, Limit: 10})
	require.NoError(t, err)
	require.Len(t, response.Findings, 1)
	assert.Equal(t, 2, response.Findings[0].SessionCount)
}

func TestGetAnalyticsIssueReviewDetectsFirstSelectedCorrection(t *testing.T) {
	database := testDB(t)
	started := "2026-08-01T10:00:00Z"
	insertSession(t, database, "s1", "alpha", func(session *Session) {
		session.StartedAt = &started
		session.MessageCount = 2
	})
	insertMessages(t, database,
		userMsgAt("s1", 0, "Run the build", started),
		userMsgAt("s1", 1, "No, that is not correct; use the verified x64 compiler for this build", started),
	)

	response, err := database.GetAnalyticsIssueReview(context.Background(), AnalyticsFilter{From: "2026-08-01", To: "2026-08-01", Timezone: "UTC"}, IssueReviewQuery{Reason: "user_correction", Limit: 10})
	require.NoError(t, err)
	require.Len(t, response.Findings, 1)
	assert.Equal(t, 1, response.Findings[0].Occurrences)
	require.Len(t, response.Findings[0].Evidence, 1)
	assert.Equal(t, 1, *response.Findings[0].Evidence[0].MessageOrdinal)
}

func TestIssueReviewRowsPreservesLongFailureTail(t *testing.T) {
	database := testDB(t)
	started := "2026-08-01T10:00:00Z"
	insertSession(t, database, "s1", "alpha", func(session *Session) {
		session.StartedAt = &started
		session.MessageCount = 2
	})
	failure := "Script failed\n" + strings.Repeat("progress output ", 200) + "\nParserError: stable tail failure"
	success := strings.Repeat("completed output ", 200) + "\nSUCCESS_TAIL_SENTINEL"
	insertMessages(t, database,
		userMsgAt("s1", 0, "Run the build", started),
		Message{SessionID: "s1", Ordinal: 1, Role: "assistant", Content: "running", Timestamp: started, HasToolUse: true, ToolCalls: []ToolCall{
			{SessionID: "s1", ToolName: "shell_command", ToolUseID: "call-tail", CallIndex: 0, InputJSON: `{"command":"build"}`, ResultEvents: []ToolResultEvent{{ToolUseID: "call-tail", Source: "tool_execution", Status: "completed", Content: failure, Timestamp: started}}},
			{SessionID: "s1", ToolName: "shell_command", ToolUseID: "call-success", CallIndex: 1, InputJSON: `{"command":"check"}`, ResultEvents: []ToolResultEvent{{ToolUseID: "call-success", Source: "tool_execution", Status: "completed", Content: success, Timestamp: started}}},
		}},
	)

	_, calls, err := database.issueReviewRows(context.Background(), []IssueReviewSession{{ID: "s1"}})
	require.NoError(t, err)
	require.Len(t, calls, 2)
	byID := map[string]IssueReviewToolCall{calls[0].ToolUseID: calls[0], calls[1].ToolUseID: calls[1]}
	assert.Contains(t, byID["call-tail"].Result, "ParserError: stable tail failure")
	assert.Equal(t, "ParserError: stable tail failure", firstIssueLine(byID["call-tail"].Result, byID["call-tail"].Input))
	assert.NotContains(t, byID["call-success"].Result, "SUCCESS_TAIL_SENTINEL")
}

func TestReadIssueReviewTelemetryReportsAvailability(t *testing.T) {
	ctx := context.Background()
	sessions := []IssueReviewSession{{ID: "s1"}, {ID: "s2"}}
	missing := filepath.Join(t.TempDir(), "missing.sqlite")
	rows, status := readIssueReviewTelemetry(ctx, missing, sessions, nil)
	assert.Empty(t, rows)
	assert.Equal(t, "missing", status)

	malformed := filepath.Join(t.TempDir(), "malformed.sqlite")
	require.NoError(t, os.WriteFile(malformed, []byte("not sqlite"), 0o600))
	rows, status = readIssueReviewTelemetry(ctx, malformed, sessions, nil)
	assert.Empty(t, rows)
	assert.Equal(t, "unavailable", status)

	available := filepath.Join(t.TempDir(), "logs.sqlite")
	conn, err := sql.Open("sqlite3", available)
	require.NoError(t, err)
	_, err = conn.Exec(`CREATE TABLE logs (thread_id TEXT,target TEXT,level TEXT,feedback_log_body TEXT,ts INTEGER,ts_nanos INTEGER,id INTEGER)`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	readonly, err := sql.Open("sqlite3", makeDSN(available, true))
	require.NoError(t, err)
	var count int
	require.NoError(t, readonly.QueryRow(`SELECT COUNT(*) FROM logs WHERE thread_id IN (?)`, "s1").Scan(&count))
	require.NoError(t, readonly.Close())
	rows, status = readIssueReviewTelemetry(ctx, available, sessions, nil)
	assert.Empty(t, rows)
	assert.Equal(t, "available", status)
}

func TestGetAnalyticsIssueReviewCacheAndForcedRefresh(t *testing.T) {
	database := testDB(t)
	started := "2026-08-01T10:00:00Z"
	seed := func(sessionID, callID string) {
		insertSession(t, database, sessionID, "alpha", func(session *Session) {
			session.StartedAt = &started
			session.Cwd = "C:\\work\\alpha"
			session.Outcome = "errored"
			session.MessageCount = 2
		})
		insertMessages(t, database,
			userMsgAt(sessionID, 0, "Open the required file", started),
			Message{
				SessionID: sessionID, Ordinal: 1, Role: "assistant", Content: "opening", Timestamp: started, HasToolUse: true,
				ToolCalls: []ToolCall{{
					SessionID: sessionID, ToolName: "shell_command", ToolUseID: callID, InputJSON: "{\"command\":\"open missing.txt\"}",
					ResultEvents: []ToolResultEvent{{ToolUseID: callID, Source: "tool_execution", Status: "errored", Content: "file not found", Timestamp: started}},
				}},
			},
		)
	}
	seed("s1", "call-1")
	filter := AnalyticsFilter{From: "2026-08-01", To: "2026-08-01", Timezone: "UTC"}
	query := IssueReviewQuery{Reason: "missing_file", Limit: 10}

	first, err := database.GetAnalyticsIssueReview(context.Background(), filter, query)
	require.NoError(t, err)
	require.Len(t, first.Findings, 1)
	assert.Equal(t, 1, first.Findings[0].Occurrences)

	seed("s2", "call-2")
	cached, err := database.GetAnalyticsIssueReview(context.Background(), filter, query)
	require.NoError(t, err)
	require.Len(t, cached.Findings, 1)
	assert.Equal(t, 1, cached.Findings[0].Occurrences)
	assert.Equal(t, first.GeneratedAt, cached.GeneratedAt)

	alternate := query
	alternate.Tool = "not-present"
	filtered, err := database.GetAnalyticsIssueReview(context.Background(), filter, alternate)
	require.NoError(t, err)
	assert.Empty(t, filtered.Findings)
	assert.Equal(t, first.GeneratedAt, filtered.GeneratedAt)

	query.Refresh = true
	refreshed, err := database.GetAnalyticsIssueReview(context.Background(), filter, query)
	require.NoError(t, err)
	require.Len(t, refreshed.Findings, 1)
	assert.Equal(t, 2, refreshed.Findings[0].Occurrences)
}

func findingsByReason(findings []IssueReviewFinding) map[string][]IssueReviewFinding {
	out := map[string][]IssueReviewFinding{}
	for _, finding := range findings {
		out[finding.ReasonCode] = append(out[finding.ReasonCode], finding)
	}
	return out
}

func ms(value int64) *int64 { return &value }
