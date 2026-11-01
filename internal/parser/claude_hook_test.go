package parser

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractClaudeHookSuccessAccepts(t *testing.T) {
	tests := []struct {
		name string
		line string
		want claudeHookSuccess
	}{
		{
			name: "content with both metadata fields",
			line: `{"type":"attachment","attachment":{"type":"hook_success",` +
				`"hookName":"example-hook","hookEvent":"PostToolUse",` +
				`"content":"hook finished"}}`,
			want: claudeHookSuccess{
				hookName:  "example-hook",
				hookEvent: "PostToolUse",
				content:   "hook finished",
			},
		},
		{
			name: "stdout holding json without content",
			line: `{"type":"attachment","attachment":{"type":"hook_success",` +
				`"stdout":"{\"status\":\"ok\",\"items\":[1,2]}"}}`,
			want: claudeHookSuccess{
				stdout: `{"status":"ok","items":[1,2]}`,
			},
		},
		{
			name: "stdout and stderr kept apart",
			line: `{"type":"attachment","attachment":{"type":"hook_success",` +
				`"stdout":"out text","stderr":"err text"}}`,
			want: claudeHookSuccess{
				stdout: "out text",
				stderr: "err text",
			},
		},
		{
			name: "content stdout and stderr kept apart",
			line: `{"type":"attachment","attachment":{"type":"hook_success",` +
				`"content":"body text","stdout":"out text",` +
				`"stderr":"err text"}}`,
			want: claudeHookSuccess{
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
				stderr: "err text",
			},
		},
		{
			name: "metadata missing",
			line: `{"type":"attachment","attachment":{"type":"hook_success",` +
				`"content":"hook finished"}}`,
			want: claudeHookSuccess{
				content: "hook finished",
			},
		},
		{
			name: "metadata empty",
			line: `{"type":"attachment","attachment":{"type":"hook_success",` +
				`"hookName":"","hookEvent":"","content":"hook finished"}}`,
			want: claudeHookSuccess{
				content: "hook finished",
			},
		},
		{
			name: "hook name without hook event infers no event",
			line: `{"type":"attachment","attachment":{"type":"hook_success",` +
				`"hookName":"SessionStart:startup","content":"session started"}}`,
			want: claudeHookSuccess{
				hookName: "SessionStart:startup",
				content:  "session started",
			},
		},
		{
			name: "multiline cjk and combining marks preserved",
			line: `{"type":"attachment","attachment":{"type":"hook_success",` +
				`"content":"  第一行\n第二行 e\u0301\n"}}`,
			want: claudeHookSuccess{
				content: "  第一行\n第二行 e\u0301\n",
			},
		},
		{
			name: "whitespace kept on every channel and metadata",
			line: `{"type":"attachment","attachment":{"type":"hook_success",` +
				`"hookName":"  example-hook  ","hookEvent":"\tPostToolUse\n",` +
				`"content":"body text","stdout":"  ","stderr":"\n"}}`,
			want: claudeHookSuccess{
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
				content: "hook finished",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := extractClaudeHookSuccess(tt.line)
			require.True(t, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestExtractClaudeHookSuccessRejects(t *testing.T) {
	tests := []struct {
		name string
		line string
	}{
		{name: "empty line", line: ""},
		{name: "whitespace only", line: " \t\n"},
		{name: "truncated object", line: `{"type":"attachment","attachment":`},
		{
			name: "trailing garbage",
			line: `{"type":"attachment","attachment":{"type":"hook_success",` +
				`"content":"ok"}} trailing`,
		},
		{
			name: "second record",
			line: `{"type":"attachment","attachment":{"type":"hook_success",` +
				`"content":"ok"}}{"type":"attachment"}`,
		},
		{
			name: "trailing closing brace",
			line: `{"type":"attachment","attachment":{"type":"hook_success",` +
				`"content":"ok"}}}`,
		},
		{
			name: "trailing closing bracket",
			line: `{"type":"attachment","attachment":{"type":"hook_success",` +
				`"content":"ok"}}]`,
		},
		{
			name: "trailing closing bracket after whitespace",
			line: `{"type":"attachment","attachment":{"type":"hook_success",` +
				`"content":"ok"}} ]`,
		},
		{name: "array root", line: `[{"type":"attachment"}]`},
		{name: "string root", line: `"attachment"`},
		{name: "number root", line: `42`},
		{name: "null root", line: `null`},
		{
			name: "record type missing",
			line: `{"attachment":{"type":"hook_success","content":"ok"}}`,
		},
		{
			name: "record type wrong",
			line: `{"type":"user","attachment":{"type":"hook_success",` +
				`"content":"ok"}}`,
		},
		{
			name: "record type not a string",
			line: `{"type":7,"attachment":{"type":"hook_success",` +
				`"content":"ok"}}`,
		},
		{name: "attachment missing", line: `{"type":"attachment"}`},
		{
			name: "attachment not an object",
			line: `{"type":"attachment","attachment":"hook_success"}`,
		},
		{
			name: "attachment null",
			line: `{"type":"attachment","attachment":null}`,
		},
		{
			name: "attachment array",
			line: `{"type":"attachment","attachment":[]}`,
		},
		{
			name: "attachment type missing",
			line: `{"type":"attachment","attachment":{"content":"ok"}}`,
		},
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
			name: "attachment type not a string",
			line: `{"type":"attachment","attachment":{"type":true,` +
				`"content":"ok"}}`,
		},
		{
			name: "no output member",
			line: `{"type":"attachment","attachment":{"type":"hook_success"}}`,
		},
		{
			name: "empty output member",
			line: `{"type":"attachment","attachment":{"type":"hook_success",` +
				`"content":""}}`,
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
			got, ok := extractClaudeHookSuccess(tt.line)
			require.False(t, ok)
			assert.Equal(t, claudeHookSuccess{}, got)
		})
	}
}

func TestExtractClaudeHookSuccessRejectsInvalidMembers(t *testing.T) {
	members := []string{"hookName", "hookEvent", "content", "stdout", "stderr"}
	values := []string{
		"null", "true", "false", "42", "1.5",
		`{"nested":"value"}`, `["value"]`,
	}
	for _, member := range members {
		for _, value := range values {
			t.Run(member+"="+value, func(t *testing.T) {
				line := `{"type":"attachment","attachment":{"type":"hook_success",` +
					`"` + member + `":` + value + `,"` +
					claudeHookFallbackMember(member) + `":"text"}}`
				got, ok := extractClaudeHookSuccess(line)
				require.False(t, ok)
				assert.Equal(t, claudeHookSuccess{}, got)
			})
		}
	}
}

func TestExtractClaudeHookSuccessJSONWhitespace(t *testing.T) {
	base := `{"type":"attachment","attachment":{"type":"hook_success","content":"synthetic output"}}`
	tests := []struct {
		name   string
		prefix string
		suffix string
		ok     bool
		want   claudeHookSuccess
	}{
		{
			name:   "JSON whitespace around the document",
			prefix: " \t\r\n",
			suffix: "\n\r\t ",
			ok:     true,
			want:   claudeHookSuccess{content: "synthetic output"},
		},
		{name: "non-JSON leading unicode space", prefix: "\u00a0"},
		{name: "non-JSON trailing vertical tab", suffix: "\v"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := extractClaudeHookSuccess(tt.prefix + base + tt.suffix)
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.want, got)
		})
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
