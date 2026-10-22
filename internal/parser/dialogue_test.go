package parser

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCodexDialogueExcludesStructuredPayloads(t *testing.T) {
	const transcript = `{"type":"session_meta","payload":{"id":"dialogue","cwd":"/tmp/sample"},"timestamp":"2026-10-01T10:00:00Z"}
{"type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"EXCLUDED instructions"}]},"timestamp":"2026-10-01T10:00:01Z"}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Explain literal [Tool: Read] and <thinking> markers"}]},"timestamp":"2026-10-01T10:00:02Z"}
{"type":"response_item","payload":{"type":"reasoning","summary":[{"type":"summary_text","text":"EXCLUDED reasoning"}]},"timestamp":"2026-10-01T10:00:03Z"}
{"type":"response_item","payload":{"type":"message","role":"assistant","channel":"commentary","content":[{"type":"output_text","text":"I am checking the parser."}]},"timestamp":"2026-10-01T10:00:04Z"}
{"type":"response_item","payload":{"type":"function_call","name":"exec_command","call_id":"call-1","arguments":"{\"cmd\":\"EXCLUDED command\"}"},"timestamp":"2026-10-01T10:00:05Z"}
{"type":"response_item","payload":{"type":"function_call_output","call_id":"call-1","output":"EXCLUDED result"},"timestamp":"2026-10-01T10:00:06Z"}
{"type":"response_item","payload":{"type":"message","role":"assistant","channel":"final","content":[{"type":"output_text","text":"The markers are ordinary text."}]},"timestamp":"2026-10-01T10:00:07Z"}`
	session, messages := runCodexParserTest(t, "dialogue.jsonl", transcript, false)
	require.NotNil(t, session)
	assert.Equal(t, "codex:dialogue", session.ID)
	require.Len(t, messages, 4)
	for i, want := range []string{"Explain literal [Tool: Read] and <thinking> markers", "I am checking the parser.", "", "The markers are ordinary text."} {
		require.NotNil(t, messages[i].DialogueText, "message %d must carry its classification", i)
		assert.Equal(t, want, *messages[i].DialogueText)
	}
}

func TestStructuredParsersRetainDialogueProjection(t *testing.T) {
	const blocks = `[{"type":"text","text":"Literal [Read: quokka.txt] [Thinking] quotation"},{"type":"thinking","thinking":"reasoningneedle"},{"type":"tool_use","id":"call_a","name":"Read","input":{"file_path":"quokka.txt"}}]`
	for _, tc := range []struct {
		name  string
		agent AgentType
	}{
		{"openclaw", AgentOpenClaw},
		{"qclaw", AgentQClaw},
		{"devin", AgentDevin},
		{"openhands-message", AgentOpenHands},
		{"openhands-action", AgentOpenHands},
		{"cursor", AgentCursor},
		{"claude", AgentClaude},
		{"iflow", AgentIflow},
		{"pi", AgentPi},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var message ParsedMessage
			switch tc.agent {
			case AgentClaude, AgentIflow, AgentPi:
				root := t.TempDir()
				path := filepath.Join(root, "session-dialogue.jsonl")
				body := `{"type":"assistant","uuid":"a","sessionId":"dialogue","timestamp":"2026-05-01T10:00:00Z","message":{"role":"assistant","content":` + blocks + `}}`
				if tc.agent == AgentPi {
					body = `{"type":"session","version":3,"id":"dialogue","timestamp":"2026-05-01T10:00:00Z"}
{"type":"message","id":"a","message":{"role":"assistant","content":[{"type":"text","text":"Literal [Read: quokka.txt] [Thinking] quotation"},{"type":"thinking","thinking":"reasoningneedle"},{"type":"toolCall","id":"call-a","name":"read","arguments":{"file_path":"quokka.txt"}}]}}`
				}
				require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
				provider, ok := NewProvider(tc.agent, ProviderConfig{Roots: []string{root}, Machine: "local"})
				require.True(t, ok)
				var opaque any = JSONLSource{Root: root, Path: path}
				if tc.agent == AgentClaude {
					opaque = MaterializedFileSource{Path: path}
				}
				outcome, err := provider.Parse(t.Context(), ParseRequest{Source: SourceRef{Provider: tc.agent, DisplayPath: path, Opaque: opaque}})
				require.NoError(t, err)
				require.Len(t, outcome.Results, 1)
				require.Len(t, outcome.Results[0].Result.Messages, 1)
				message = outcome.Results[0].Result.Messages[0]
			case AgentOpenClaw:
				b := newOpenClawRecordBuilder()
				require.NoError(t, b.consume(`{"type":"message","message":{"role":"assistant","content":`+blocks+`}}`, false))
				require.Len(t, b.messages, 1)
				message = b.messages[0]
			case AgentQClaw:
				path, _ := writeQClawTestFile(t, "agent", `{"type":"session","id":"dialogue"}`, `{"type":"message","message":{"role":"assistant","content":`+blocks+`}}`)
				_, messages, err := parseQClawSessionForTest(t, path, "project", "local")
				require.NoError(t, err)
				require.Len(t, messages, 1)
				message = messages[0]
			case AgentDevin:
				var ok bool
				message, ok = parseDevinStep("dialogue", gjson.Parse(`{"source":"agent","message":`+blocks+`}`), 0, "")
				require.True(t, ok)
				stored, ok, err := parseDevinDBMessageNode("dialogue", devinMessageNodeRow{ChatMessage: `{"role":"assistant","content":` + blocks + `}`}, 0, "")
				require.NoError(t, err)
				require.True(t, ok)
				assert.Equal(t, message.DialogueText, stored.DialogueText)
			case AgentOpenHands:
				var ok bool
				if tc.name == "openhands-action" {
					message, ok, _ = parseOpenHandsActionEvent(gjson.Parse(`{"thought":[{"type":"text","text":"Literal [Read: quokka.txt] [Thinking] quotation"}],"thinking_blocks":[{"type":"thinking","thinking":"reasoningneedle"},{"type":"thinking","thinking":"actionreasoningneedle"}],"tool_name":"terminal","tool_call_id":"call_a","tool_call":{"id":"call_a","name":"terminal","arguments":"{\"command\":\"tail -40 /tmp/server.log\"}"},"action":{"command":"tail -40 /tmp/server.log","kind":"TerminalAction"}}`), 0, "", time.Time{})
				} else {
					message, ok, _ = parseOpenHandsMessageEvent(gjson.Parse(`{"llm_message":{"role":"assistant","content":`+blocks+`}}`), 0, "", time.Time{})
				}
				require.True(t, ok)
			case AgentCursor:
				messages := parseCursorJSONL(`{"role":"assistant","message":{"content":` + blocks + `}}`)
				require.Len(t, messages, 1)
				message = messages[0]
			default:
				require.FailNow(t, "unexpected provider")
			}
			want := "Literal [Read: quokka.txt] [Thinking] quotation"
			assert.Equal(t, new(want), message.DialogueText)
		})
	}
}
