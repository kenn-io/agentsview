package parser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"go.kenn.io/agentsview/internal/usagefacts"
)

// codebuddyRawCounters reads a usage row's counters as stored, without the
// per-request cap ParseTokenUsage applies, so a test sees a row that kept
// more than the bound.
func codebuddyRawCounters(tokenUsage []byte) usagefacts.ParsedTokenUsage {
	get := func(key string) int64 { return gjson.GetBytes(tokenUsage, key).Int() }
	return usagefacts.ParsedTokenUsage{
		InputTokens: get("input_tokens"), OutputTokens: get("output_tokens"),
		CacheReadTokens:     get("cache_read_input_tokens"),
		CacheCreationTokens: get("cache_creation_input_tokens"),
		ReasoningTokens:     get("reasoning_tokens"),
	}
}

// codebuddyLargeRequestReply is a converted 4.x reply whose request summed
// many model steps: 8,510,000 prompt tokens, 4,500,000 of them cache reads
// and 1,000,000 cache writes, and 2,100,000 output tokens, 2,050,000 of them
// reasoning.
const codebuddyLargeRequestReply = `{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1767225602000,"content":[{"type":"output_text","text":"done"}],"providerData":{"agent":"codebuddy-ide-4.x","conversationRequestId":"req1","messageId":"a1","model":"luna","lastStepInputTokens":90,"rawUsage":{"prompt_tokens":8510000,"completion_tokens":2100000,"prompt_cache_hit_tokens":4500000,"prompt_cache_write_tokens":1000000,"completion_thinking_tokens":2050000}}}`

func codebuddyLargeRequestLegacyDir(t *testing.T, stats string) string {
	t.Helper()
	legacyRoot := t.TempDir()
	convDir := filepath.Join(legacyRoot, "history", "ws", "conv2")
	require.NoError(t, os.MkdirAll(filepath.Join(convDir, "messages"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(convDir, "index.json"), []byte(`{"messages":[{"id":"a1","role":"assistant"}]}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(convDir, "messages", "a1.json"), []byte(`{"role":"assistant","createdAt":"2026-01-01T00:00:02Z","extra":{"requestId":"req1","modelId":"luna","lastStepInputTokens":90,"statsSnapshot":`+stats+`}}`), 0o600))
	return legacyRoot
}

// A 4.x request total above two million tokens per counter is split across
// rows that each stay within that bound and together sum to the total,
// whether the converted transcript carries it, only the 4.x folder does, or
// the transcript no longer holds the reply. The reply keeps the first part,
// its identity, and the last step's context; each further part is a hidden
// usage row right after it.
func TestCodeBuddyConvertedRequestTotalAboveTwoMillionSplitsExactly(t *testing.T) {
	const stats = `{"inputTokens":8510000,"outputTokens":2100000,"cachedInputTokens":4500000,"cacheWriteTokens":1000000,"thinkingTokens":2050000}`
	const bare = `{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1767225602000,"content":[{"type":"output_text","text":"done"}]}`
	for _, tc := range []struct {
		name    string
		lines   []string
		visible bool
	}{
		{"transcript copy and 4.x folder", []string{codebuddyLargeRequestReply}, true},
		{"4.x folder only", []string{bare}, true},
		{"reply gone from the transcript", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, project := cbcHome(t)
			legacyRoot := codebuddyLargeRequestLegacyDir(t, stats)
			writeCBC(t, filepath.Join(project, "conv2.jsonl"), append([]string{
				`{"type":"session-meta","id":"m","sessionId":"conv2","meta":{"codebuddy.ai/converterVersion":"ide-main-craft-jsonl/34"}}`,
				`{"id":"u1","type":"message","role":"user","timestamp":1767225601000,"content":[{"type":"input_text","text":"go"}]}`,
			}, tc.lines...)...)
			main := cbcSession(t, parseCBCHome(t, home, legacyRoot), "codebuddy:conv2")
			first := -1
			var usage []ParsedMessage
			for i, m := range main.Messages {
				if len(m.TokenUsage) == 0 {
					continue
				}
				if first < 0 {
					first = i
				}
				usage = append(usage, m)
			}
			require.Len(t, usage, 3, "the largest counter needs three parts")
			for k, m := range usage {
				assert.Equal(t, first+k, m.Ordinal, "the parts are adjacent")
			}
			assert.Equal(t, "cb4:req1:a1", usage[0].SourceUUID)
			assert.Equal(t, tc.visible, !usage[0].IsSystem, "the reply keeps the first part")
			assert.Equal(t, 90, usage[0].ContextTokens, "the context is the last step's prompt")
			assert.True(t, usage[0].HasContextTokens)
			var sum usagefacts.ParsedTokenUsage
			outputs := 0
			for k, m := range usage {
				parsed := codebuddyRawCounters(m.TokenUsage)
				for _, v := range []int64{parsed.InputTokens, parsed.OutputTokens,
					parsed.CacheReadTokens, parsed.CacheCreationTokens, parsed.ReasoningTokens} {
					assert.LessOrEqual(t, v, int64(usagefacts.MaxPlausibleTokens), "part %d", k+1)
				}
				assert.LessOrEqual(t, parsed.ReasoningTokens, parsed.OutputTokens, "part %d", k+1)
				assert.Equal(t, int(parsed.OutputTokens), m.OutputTokens, "part %d", k+1)
				assert.Equal(t, "luna", m.Model, "part %d", k+1)
				assert.Equal(t, usage[0].Timestamp, m.Timestamp, "part %d", k+1)
				sum.InputTokens += parsed.InputTokens
				sum.OutputTokens += parsed.OutputTokens
				sum.CacheReadTokens += parsed.CacheReadTokens
				sum.CacheCreationTokens += parsed.CacheCreationTokens
				sum.ReasoningTokens += parsed.ReasoningTokens
				outputs += m.OutputTokens
				if k == 0 {
					continue
				}
				assert.Equal(t, "cb4:req1:a1#"+string(rune('1'+k)), m.SourceUUID)
				assert.True(t, m.IsSystem, "part %d is a hidden usage row", k+1)
				assert.Empty(t, m.Content)
				assert.Zero(t, m.ContextTokens, "part %d has no context", k+1)
				assert.False(t, m.HasContextTokens, "part %d has no context", k+1)
			}
			assert.Equal(t, usagefacts.ParsedTokenUsage{
				InputTokens: 3_010_000, OutputTokens: 2_100_000,
				CacheReadTokens: 4_500_000, CacheCreationTokens: 1_000_000,
				ReasoningTokens: 2_050_000,
			}, sum, "the parts sum to the request total")
			assert.Equal(t, 2_100_000, outputs)
			assert.Equal(t, 2_100_000, main.Session.TotalOutputTokens)
			assert.Equal(t, 90, main.Session.PeakContextTokens)
		})
	}
}

// Usage within the per-request bound, and a single CLI response above it,
// stay on one row; the CLI response keeps the bound like any model call.
func TestCodeBuddyUsageWithinBoundStaysUnsplit(t *testing.T) {
	home, project := cbcHome(t)
	writeCBC(t, filepath.Join(project, "s1.jsonl"),
		`{"id":"u1","type":"message","role":"user","timestamp":1767225601000,"content":[{"type":"input_text","text":"go"}]}`,
		`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1767225602000,"content":[{"type":"output_text","text":"cli"}],"providerData":{"conversationRequestId":"r1","messageId":"m1","stepSeq":1,"model":"glm-x","rawUsage":{"prompt_tokens":3000000,"completion_tokens":5}}}`,
		`{"id":"u2","parentId":"a1","type":"message","role":"user","timestamp":1767225603000,"content":[{"type":"input_text","text":"again"}]}`,
		`{"id":"a2","parentId":"u2","type":"message","role":"assistant","timestamp":1767225604000,"content":[{"type":"output_text","text":"small"}],"providerData":{"agent":"codebuddy-ide-4.x","conversationRequestId":"r2","messageId":"a2","model":"glm-x","rawUsage":{"prompt_tokens":300,"completion_tokens":4}}}`,
	)
	msgs := cbcSession(t, parseCBCHome(t, home), "codebuddy:s1").Messages
	require.Len(t, msgs, 4, "no row is split")
	for _, m := range msgs {
		assert.False(t, strings.Contains(m.SourceUUID, "#"), m.SourceUUID)
	}
	assert.Equal(t, "cb4:r2:a2", msgs[3].SourceUUID)
	assert.Equal(t, int64(300), usagefacts.ParseTokenUsage(string(msgs[3].TokenUsage)).InputTokens)
	assert.Equal(t, int64(usagefacts.MaxPlausibleTokens),
		usagefacts.ParseTokenUsage(string(msgs[1].TokenUsage)).InputTokens,
		"one CLI model call stays within the per-request bound")
}
