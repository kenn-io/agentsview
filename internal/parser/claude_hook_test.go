package parser

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractClaudeHookSuccessAccepts(t *testing.T) {
	tests := []struct {
		name        string
		line        string
		want        claudeHookSuccess
		stopSummary bool
	}{
		{
			name:        "stop summary filters non-string errors",
			line:        `{"hookErrors":["first",{},null,42,"second"]}`,
			want:        claudeHookSuccess{subtype: "stop_hook_summary", summary: &claudeStopHookSummary{HookErrors: []string{"first", "second"}}},
			stopSummary: true,
		},
		{
			name:        "stop summary payload already classified by caller",
			line:        `{"stopReason":"blocked"}`,
			want:        claudeHookSuccess{subtype: "stop_hook_summary", summary: &claudeStopHookSummary{StopReason: "blocked"}},
			stopSummary: true,
		},
		{
			name: "cancelled without output",
			line: `{"type":"attachment","attachment":{"type":"hook_cancelled","hookName":"PreToolUse:Read","hookEvent":"PreToolUse"}}`,
			want: claudeHookSuccess{subtype: "hook_cancelled", hookName: "PreToolUse:Read", hookEvent: "PreToolUse"},
		},
		{
			name: "additional context joins strings",
			line: `{"type":"attachment","attachment":{"type":"hook_additional_context","content":["first","second"],"hookName":"SessionStart:startup","hookEvent":"SessionStart"}}`,
			want: claudeHookSuccess{subtype: "hook_additional_context", content: "first\nsecond", hookName: "SessionStart:startup", hookEvent: "SessionStart"},
		},
		{
			name: "additional context preserves whitespace",
			line: `{"type":"attachment","attachment":{"type":"hook_additional_context","content":["  "]}}`,
			want: claudeHookSuccess{subtype: "hook_additional_context", content: "  "},
		},
		{
			name: "content with all metadata fields",
			line: `{"type":"attachment","attachment":{"type":"hook_success",` +
				`"hookName":"example-hook","hookEvent":"PostToolUse","command":"echo hook","toolUseID":"tool-1",` +
				`"content":"hook finished"}}`,
			want: claudeHookSuccess{
				subtype:   "hook_success",
				hookName:  "example-hook",
				hookEvent: "PostToolUse",
				command:   "echo hook",
				toolUseID: "tool-1",
				content:   "hook finished",
			},
		},
		{
			name: "stdout holding json without content",
			line: `{"type":"attachment","attachment":{"type":"hook_success",` +
				`"stdout":"{\"status\":\"ok\",\"items\":[1,2]}"}}`,
			want: claudeHookSuccess{
				subtype: "hook_success",
				stdout:  `{"status":"ok","items":[1,2]}`,
			},
		},
		{
			name: "content stdout and stderr kept apart",
			line: `{"type":"attachment","attachment":{"type":"hook_success",` +
				`"content":"body text","stdout":"out text",` +
				`"stderr":"err text"}}`,
			want: claudeHookSuccess{
				subtype: "hook_success",
				content: "body text",
				stdout:  "out text",
				stderr:  "err text",
			},
		},
		{
			name: "stderr only",
			line: `{"type":"attachment","attachment":{"type":"hook_success",` +
				`"stderr":"err text"}}`,
			want: claudeHookSuccess{
				subtype: "hook_success",
				stderr:  "err text",
			},
		},
		{
			name: "metadata missing",
			line: `{"type":"attachment","attachment":{"type":"hook_success",` +
				`"content":"hook finished"}}`,
			want: claudeHookSuccess{
				subtype: "hook_success",
				content: "hook finished",
			},
		},
		{
			name: "metadata empty",
			line: `{"type":"attachment","attachment":{"type":"hook_success",` +
				`"hookName":"","hookEvent":"","content":"hook finished"}}`,
			want: claudeHookSuccess{
				subtype: "hook_success",
				content: "hook finished",
			},
		},
		{
			name: "hook name without hook event infers no event",
			line: `{"type":"attachment","attachment":{"type":"hook_success",` +
				`"hookName":"SessionStart:startup","content":"session started"}}`,
			want: claudeHookSuccess{
				subtype:  "hook_success",
				hookName: "SessionStart:startup",
				content:  "session started",
			},
		},
		{
			name: "multiline cjk and combining marks preserved",
			line: `{"type":"attachment","attachment":{"type":"hook_success",` +
				`"content":"  第一行\n第二行 e\u0301\n"}}`,
			want: claudeHookSuccess{
				subtype: "hook_success",
				content: "  第一行\n第二行 e\u0301\n",
			},
		},
		{
			name: "whitespace kept on every channel and metadata",
			line: `{"type":"attachment","attachment":{"type":"hook_success",` +
				`"hookName":"  example-hook  ","hookEvent":"\tPostToolUse\n",` +
				`"content":"body text","stdout":"  ","stderr":"\n"}}`,
			want: claudeHookSuccess{
				subtype:   "hook_success",
				hookName:  "  example-hook  ",
				hookEvent: "\tPostToolUse\n",
				content:   "body text",
				stdout:    "  ",
				stderr:    "\n",
			},
		},
		{
			name: "unknown members ignored",
			line: `{"type":"attachment","sessionId":"synthetic",` +
				`"attachment":{"type":"hook_success","content":"hook finished",` +
				`"exitCode":0,"extra":{"nested":true}}}`,
			want: claudeHookSuccess{
				subtype: "hook_success",
				content: "hook finished",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := extractClaudeHookSuccess(tt.line, tt.stopSummary)
			require.True(t, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestExtractClaudeHookSuccessRejects(t *testing.T) {
	tests := []struct {
		name        string
		line        string
		stopSummary bool
	}{
		{name: "additional context empty array", line: `{"type":"attachment","attachment":{"type":"hook_additional_context","content":[]}}`},
		{name: "additional context mixed array", line: `{"type":"attachment","attachment":{"type":"hook_additional_context","content":["first",1]}}`},
		{name: "additional context string", line: `{"type":"attachment","attachment":{"type":"hook_additional_context","content":"first"}}`},
		{name: "empty stop summary", stopSummary: true, line: `{"type":"system","subtype":"stop_hook_summary","hookCount":0,"hookInfos":[],"hookErrors":[],"preventedContinuation":false}`},
		{name: "command-only stop summary", stopSummary: true, line: `{"type":"system","subtype":"stop_hook_summary","hookCount":1,"hookInfos":[{"command":"echo hook"}],"hookErrors":[],"preventedContinuation":false}`},
		{name: "stop summary with only non-string errors", stopSummary: true, line: `{"type":"system","subtype":"stop_hook_summary","hookInfos":[{"command":"echo hook"}],"hookErrors":[{},null,42]}`},
		{name: "cancelled string timeout flag", line: `{"type":"attachment","attachment":{"type":"hook_cancelled","timedOut":"true","timeoutMs":60000}}`},
		{name: "cancelled string timeout duration", line: `{"type":"attachment","attachment":{"type":"hook_cancelled","timedOut":true,"timeoutMs":"60000"}}`},
		{name: "attachment missing", line: `{"type":"attachment"}`},
		{
			name: "attachment type wrong",
			line: `{"type":"attachment","attachment":{"type":"queued_command",` +
				`"content":"ok"}}`,
		},
		{
			name: "attachment type unknown",
			line: `{"type":"attachment","attachment":{"type":"hook_error",` +
				`"content":"ok"}}`,
		},
		{
			name: "attachment type padded",
			line: `{"type":"attachment","attachment":{"type":"hook_success ",` +
				`"content":"ok"}}`,
		},
		{
			name: "no output member",
			line: `{"type":"attachment","attachment":{"type":"hook_success"}}`,
		},
		{
			name: "blank content",
			line: `{"type":"attachment","attachment":{"type":"hook_success",` +
				`"content":" \n\t "}}`,
		},
		{
			name: "blank stdout",
			line: `{"type":"attachment","attachment":{"type":"hook_success",` +
				`"stdout":"  "}}`,
		},
		{
			name: "blank stderr",
			line: `{"type":"attachment","attachment":{"type":"hook_success",` +
				`"stderr":"\n"}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := extractClaudeHookSuccess(tt.line, tt.stopSummary)
			require.False(t, ok)
			assert.Equal(t, claudeHookSuccess{}, got)
		})
	}
}

func TestExtractClaudeHookSuccessRejectsInvalidMembers(t *testing.T) {
	members := []string{"hookName", "hookEvent", "command", "toolUseID", "content", "stdout", "stderr"}
	values := []string{
		"null", "true", "false", "42",
		`{"nested":"value"}`,
	}
	for _, member := range members {
		for _, value := range values {
			t.Run(member+"="+value, func(t *testing.T) {
				line := `{"type":"attachment","attachment":{"type":"hook_success",` +
					`"` + member + `":` + value + `,"` +
					claudeHookFallbackMember(member) + `":"text"}}`
				got, ok := extractClaudeHookSuccess(line, false)
				require.False(t, ok)
				assert.Equal(t, claudeHookSuccess{}, got)
			})
		}
	}
}

// claudeHookFallbackMember names a text member that is not the member
// under test, so the payload would still carry text if the member under
// test were accepted.
func claudeHookFallbackMember(member string) string {
	if member == "content" {
		return "stdout"
	}
	return "content"
}
