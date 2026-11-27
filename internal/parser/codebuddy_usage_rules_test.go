package parser

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// A rewound branch whose usage row names no model takes the model of the
// response before it, as the conversation does, so usage reports, which
// count only rows with a model, still include it.
func TestCodeBuddyCBCRewoundBranchNamesUsageModel(t *testing.T) {
	home, project := cbcHome(t)
	writeCBC(t, filepath.Join(project, "s1.jsonl"),
		`{"id":"u1","type":"message","role":"user","timestamp":1000,"content":[{"type":"input_text","text":"first try"}]}`,
		`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1001,"content":[{"type":"output_text","text":"old answer"}],"providerData":{"model":"glm-x"}}`,
		`{"id":"mu","parentId":"a1","type":"model-usage","timestamp":1002,"providerData":{"conversationRequestId":"r1","messageId":"m9","rawUsage":{"prompt_tokens":10,"completion_tokens":5}}}`,
		`{"id":"n1","type":"resend-fork-notice","timestamp":1003}`,
		`{"id":"u2","type":"message","role":"user","timestamp":1004,"content":[{"type":"input_text","text":"second try"}]}`,
		`{"id":"a2","parentId":"u2","type":"message","role":"assistant","timestamp":1005,"content":[{"type":"output_text","text":"new answer"}],"providerData":{"model":"glm-x"}}`,
	)
	fork := cbcSession(t, parseCBCHome(t, home), "codebuddy:s1-u1")
	var usage *ParsedMessage
	for i := range fork.Messages {
		if len(fork.Messages[i].TokenUsage) > 0 {
			usage = &fork.Messages[i]
		}
	}
	require.NotNil(t, usage, "the branch keeps the usage row")
	assert.Equal(t, "glm-x", usage.Model)
}

// Rewinding an empty but billed response, while the prompt stays on the
// active path, leaves a branch with usage only. It is kept, so the response
// still counts once.
func TestCodeBuddyCBCRewoundEmptyBilledResponseCounts(t *testing.T) {
	for name, usage := range map[string][]string{
		"usage on the reply": {
			`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1001,"content":[],"providerData":{"conversationRequestId":"r1","messageId":"m1","model":"glm-x","rawUsage":{"prompt_tokens":10,"completion_tokens":5}}}`,
		},
		"usage in its own item": {
			`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1001,"content":[],"providerData":{"conversationRequestId":"r1","messageId":"m1","model":"glm-x"}}`,
			`{"id":"mu1","parentId":"a1","type":"model-usage","timestamp":1001,"providerData":{"conversationRequestId":"r1","messageId":"m1","model":"glm-x","rawUsage":{"prompt_tokens":10,"completion_tokens":5}}}`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			home, project := cbcHome(t)
			lines := []string{`{"id":"u1","type":"message","role":"user","timestamp":1000,"content":[{"type":"input_text","text":"go"}]}`}
			lines = append(lines, usage...)
			lines = append(lines,
				`{"id":"n1","parentId":"u1","type":"resend-fork-notice","timestamp":1002}`,
				`{"id":"a2","parentId":"u1","type":"message","role":"assistant","timestamp":1003,"content":[{"type":"output_text","text":"retried answer"}],"providerData":{"conversationRequestId":"r2","messageId":"m2","model":"glm-x","rawUsage":{"prompt_tokens":12,"completion_tokens":7}}}`,
			)
			writeCBC(t, filepath.Join(project, "s1.jsonl"), lines...)
			total := 0
			ids := map[string]int{}
			for _, r := range parseCBCHome(t, home) {
				total += r.Session.TotalOutputTokens
				for _, m := range r.Messages {
					if len(m.TokenUsage) > 0 {
						ids[m.SourceUUID]++
					}
				}
			}
			assert.Equal(t, 12, total, "the rewound response and the retry both count")
			for id, n := range ids {
				assert.Equal(t, 1, n, "%s counts once", id)
			}
		})
	}
}

// An original counter that reports zero wins over the normalized usage
// object's positive one, as the original prompt and output counts do, and a
// missing original counter falls back to the normalized one.
func TestCodeBuddyCBCRawZeroCountersWin(t *testing.T) {
	for _, tc := range []struct {
		name, usage            string
		read, write, reasoning int64
	}{
		{"raw zeros win", `"rawUsage":{"prompt_tokens":10,"completion_tokens":5,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"completion_thinking_tokens":0},"usage":{"cacheReadInputTokens":7,"cacheCreationInputTokens":3,"outputTokensDetails":{"reasoning_tokens":2}}`, 0, 0, 0},
		{"missing raw counters fall back", `"rawUsage":{"prompt_tokens":10,"completion_tokens":5},"usage":{"cacheReadInputTokens":7,"cacheCreationInputTokens":3,"outputTokensDetails":{"reasoning_tokens":2}}`, 7, 3, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, project := cbcHome(t)
			writeCBC(t, filepath.Join(project, "s1.jsonl"),
				`{"id":"u1","type":"message","role":"user","timestamp":1000,"content":[{"type":"input_text","text":"hi"}]}`,
				`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1001,"content":[{"type":"output_text","text":"hello"}],"providerData":{"conversationRequestId":"r1","messageId":"m1","model":"glm-x",`+tc.usage+`}}`,
			)
			reply := cbcSession(t, parseCBCHome(t, home), "codebuddy:s1").Messages[1]
			assert.Equal(t, tc.read, gjson.GetBytes(reply.TokenUsage, "cache_read_input_tokens").Int())
			assert.Equal(t, tc.write, gjson.GetBytes(reply.TokenUsage, "cache_creation_input_tokens").Int())
			assert.Equal(t, tc.reasoning, gjson.GetBytes(reply.TokenUsage, "reasoning_tokens").Int())
			for _, key := range []string{"cache_read_input_tokens", "cache_creation_input_tokens", "reasoning_tokens"} {
				assert.True(t, gjson.GetBytes(reply.TokenUsage, key).Exists(), "%s is a reported counter", key)
			}
		})
	}
}

// A partial rewind keeps the shared first turn in the conversation. The
// branch holds only the rewound turn, whose usage names no model, so it takes
// the model the shared turn named.
func TestCodeBuddyCBCPartialRewindBranchNamesUsageModel(t *testing.T) {
	home, project := cbcHome(t)
	writeCBC(t, filepath.Join(project, "s1.jsonl"),
		`{"id":"u1","type":"message","role":"user","timestamp":1000,"content":[{"type":"input_text","text":"first"}]}`,
		`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1001,"content":[{"type":"output_text","text":"shared answer"}],"providerData":{"model":"glm-x"}}`,
		`{"id":"u2","parentId":"a1","type":"message","role":"user","timestamp":1002,"content":[{"type":"input_text","text":"second try"}]}`,
		`{"id":"a2","parentId":"u2","type":"message","role":"assistant","timestamp":1003,"content":[{"type":"output_text","text":"old answer"}]}`,
		`{"id":"mu","parentId":"a2","type":"model-usage","timestamp":1004,"providerData":{"conversationRequestId":"r2","messageId":"m2","rawUsage":{"prompt_tokens":10,"completion_tokens":5}}}`,
		`{"id":"n1","parentId":"a1","type":"resend-fork-notice","timestamp":1005}`,
		`{"id":"u3","parentId":"a1","type":"message","role":"user","timestamp":1006,"content":[{"type":"input_text","text":"third try"}]}`,
		`{"id":"a3","parentId":"u3","type":"message","role":"assistant","timestamp":1007,"content":[{"type":"output_text","text":"new answer"}],"providerData":{"model":"glm-x"}}`,
	)
	fork := cbcSession(t, parseCBCHome(t, home), "codebuddy:s1-u2")
	var usage *ParsedMessage
	for i := range fork.Messages {
		if len(fork.Messages[i].TokenUsage) > 0 {
			usage = &fork.Messages[i]
		}
	}
	require.NotNil(t, usage, "the branch keeps the usage row")
	assert.Equal(t, "glm-x", usage.Model)
}

// A side question that failed or was cancelled after CodeBuddy saved part of
// an answer shows the answer and the status, so it does not read as done.
func TestCodeBuddySideQuestionKeepsStatusWithAnswer(t *testing.T) {
	home, project := cbcHome(t)
	writeCBC(t, filepath.Join(project, "run-a.jsonl"),
		`{"id":"u1","type":"message","role":"user","timestamp":1000,"sessionId":"run-a","content":[{"type":"input_text","text":"hi"}]}`,
		`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1001,"content":[{"type":"output_text","text":"hello"}]}`,
	)
	writeCBCCatalog(t, home,
		`INSERT INTO conversations (id, title, runtime_session_id) VALUES ('conv-a', 'Title', 'run-a')`,
		`INSERT INTO btw_entries VALUES ('conv-a', '[{"id":"b1","question":"q1","answer":"partial","status":"error","error":"quota","createdAt":2000},{"id":"b2","question":"q2","answer":"half","status":"cancelled","createdAt":3000}]')`,
	)
	btw := cbcSession(t, parseCBCHome(t, home), "codebuddy:run-a:btw")
	require.Len(t, btw.Messages, 4)
	assert.Equal(t, "partial\n\n[Side question failed] quota", btw.Messages[1].Content)
	assert.Equal(t, "half\n\n[Side question cancelled]", btw.Messages[3].Content)
}

// A 4.x request total sums every step's prompt. Its context window is the
// last step's prompt when the record has one, and unknown otherwise, never
// the summed prompt. An output-only total with a last step still has one.
func TestCodeBuddyLegacyRequestContextIsLastStep(t *testing.T) {
	for _, tc := range []struct {
		name, extra string
		context     int
		hasContext  bool
	}{
		{"summed prompt without last step", `{"requestId":"r","statsSnapshot":{"inputTokens":900000,"outputTokens":40}}`, 0, false},
		{"output only with last step", `{"requestId":"r","lastStepInputTokens":70,"statsSnapshot":{"outputTokens":40}}`, 70, true},
		{"last step", `{"requestId":"r","lastStepInputTokens":70,"statsSnapshot":{"inputTokens":900000,"outputTokens":40}}`, 70, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg, ok := codebuddyLegacyRequestUsage(gjson.Parse(tc.extra))
			require.True(t, ok)
			assert.Equal(t, tc.context, msg.ContextTokens)
			assert.Equal(t, tc.hasContext, msg.HasContextTokens)
			assert.Equal(t, 40, msg.OutputTokens)
		})
	}
}

// A corrupt 4.x counter far beyond any request is left whole for the
// backends to cap, instead of splitting into millions of rows.
func TestCodeBuddySplitRequestTotalRejectsImplausibleCounter(t *testing.T) {
	msg := ParsedMessage{TokenUsage: []byte(`{"input_tokens":1000000000000000,"output_tokens":40}`)}
	assert.Empty(t, codebuddySplitRequestTotal(&msg))
	msg = ParsedMessage{TokenUsage: []byte(`{"input_tokens":5000000,"output_tokens":40}`), SourceUUID: "cb4:r:m"}
	assert.Len(t, codebuddySplitRequestTotal(&msg), 2, "a plausible total above the bound still splits")
}

// A converted reply with no text, or a usage-only record, whose 4.x request
// total reports input alone keeps a hidden usage row with that input, though
// its context size is unknown and it has no output count.
func TestCodeBuddyConvertedInputOnlyTotalKeepsRow(t *testing.T) {
	home, project := cbcHome(t)
	writeCBC(t, filepath.Join(project, "conv1.jsonl"),
		`{"type":"session-meta","id":"m","sessionId":"conv1","meta":{"codebuddy.ai/converterVersion":"ide-main-craft-jsonl/34"}}`,
		`{"id":"u1","type":"message","role":"user","timestamp":1767225601000,"content":[{"type":"input_text","text":"go"}]}`,
		`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1767225602000,"content":[],"providerData":{"agent":"codebuddy-ide-4.x","conversationRequestId":"req1","messageId":"a1","model":"glm-x","rawUsage":{"prompt_tokens":300}}}`,
		`{"id":"mu","parentId":"a1","type":"model-usage","timestamp":1767225603000,"providerData":{"agent":"codebuddy-ide-4.x","conversationRequestId":"req2","messageId":"a2","model":"glm-x","rawUsage":{"prompt_tokens":500}}}`,
	)
	main := cbcSession(t, parseCBCHome(t, home), "codebuddy:conv1")
	byID := map[string]ParsedMessage{}
	for _, m := range main.Messages {
		byID[m.SourceUUID] = m
	}
	for id, input := range map[string]int64{"cb4:req1:a1": 300, "cb4:req2:a2": 500} {
		row, ok := byID[id]
		require.True(t, ok, "%s keeps its row", id)
		assert.True(t, row.IsSystem, id)
		assert.Equal(t, input, gjson.GetBytes(row.TokenUsage, "input_tokens").Int(), id)
		assert.False(t, row.HasContextTokens, "%s has no known context size", id)
	}
}

// A converted reply whose transcript copy carries an empty usage block, such
// as rawUsage: {}, still counts the 4.x request total its folder recorded.
func TestCodeBuddyConvertedEmptyUsageBlockTakesLegacyTotal(t *testing.T) {
	home, project := cbcHome(t)
	legacyRoot := t.TempDir()
	convDir := filepath.Join(legacyRoot, "history", "ws", "conv1")
	writeCBC(t, filepath.Join(convDir, "index.json"), `{"messages":[{"id":"a1","role":"assistant"}]}`)
	writeCBC(t, filepath.Join(convDir, "messages", "a1.json"),
		`{"role":"assistant","createdAt":"2026-01-01T00:00:02Z","extra":{"requestId":"req1","modelId":"glm-x","statsSnapshot":{"inputTokens":300,"outputTokens":40}}}`)
	for _, block := range []string{`"rawUsage":{}`, `"usage":{}`} {
		t.Run(block, func(t *testing.T) {
			writeCBC(t, filepath.Join(project, "conv1.jsonl"),
				`{"type":"session-meta","id":"m","sessionId":"conv1","meta":{"codebuddy.ai/converterVersion":"ide-main-craft-jsonl/34"}}`,
				`{"id":"u1","type":"message","role":"user","timestamp":1767225601000,"content":[{"type":"input_text","text":"go"}]}`,
				`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1767225602000,"content":[{"type":"output_text","text":"done"}],"providerData":{"agent":"codebuddy-ide-4.x","conversationRequestId":"req1","messageId":"a1",`+block+`}}`,
			)
			main := cbcSession(t, parseCBCHome(t, home, legacyRoot), "codebuddy:conv1")
			assert.Equal(t, 40, main.Session.TotalOutputTokens)
		})
	}
}
