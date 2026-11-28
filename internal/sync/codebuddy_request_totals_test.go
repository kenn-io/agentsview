package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/usagefacts"
)

// A converted 4.x request that sums more than two million tokens per counter
// counts in full in daily usage and in session usage, from the transcript's
// copy of the total or from the 4.x folder alone, and after that folder is
// deleted. The parser splits the total across rows within the per-request
// bound. Two rewound branches repeat the reply and its parts, and the request
// still counts once; its context stays the last step's prompt.
func TestSyncCodeBuddyConvertedRequestAboveTwoMillionCountsInFull(t *testing.T) {
	const copied = `{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1767225602000,"content":[{"type":"output_text","text":"done"}],"providerData":{"agent":"codebuddy-ide-4.x","conversationRequestId":"req1","messageId":"a1","model":"luna","lastStepInputTokens":90,"rawUsage":{"prompt_tokens":3000000,"completion_tokens":2100000,"prompt_cache_hit_tokens":2990000}}}`
	const bare = `{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1767225602000,"content":[{"type":"output_text","text":"done"}]}`
	for _, tc := range []struct{ name, reply string }{
		{"transcript copy", copied},
		{"4.x folder only", bare},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, project := cbcSyncHome(t)
			legacyRoot := t.TempDir()
			convDir := filepath.Join(legacyRoot, "history", "ws", "conv2")
			require.NoError(t, os.MkdirAll(filepath.Join(convDir, "messages"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(convDir, "index.json"), []byte(`{"messages":[{"id":"a1","role":"assistant"}]}`), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(convDir, "messages", "a1.json"), []byte(`{"role":"assistant","createdAt":"2026-01-01T00:00:02Z","extra":{"requestId":"req1","modelId":"luna","lastStepInputTokens":90,"statsSnapshot":{"inputTokens":3000000,"outputTokens":2100000,"cachedInputTokens":2990000}}}`), 0o600))
			writeCBCSync(t, filepath.Join(project, "conv2.jsonl"),
				`{"type":"session-meta","id":"m","sessionId":"conv2","meta":{"codebuddy.ai/converterVersion":"ide-main-craft-jsonl/34"}}`,
				`{"id":"u1","type":"message","role":"user","timestamp":1767225601000,"content":[{"type":"input_text","text":"go"}]}`,
				tc.reply,
				`{"id":"u2","parentId":"a1","type":"message","role":"user","timestamp":1767225603000,"content":[{"type":"input_text","text":"first follow-up"}]}`,
				`{"id":"a2","parentId":"u2","type":"message","role":"assistant","timestamp":1767225604000,"content":[{"type":"output_text","text":"first"}]}`,
				`{"id":"u3","parentId":"a1","type":"message","role":"user","timestamp":1767225605000,"content":[{"type":"input_text","text":"second follow-up"}]}`,
				`{"id":"a3","parentId":"u3","type":"message","role":"assistant","timestamp":1767225606000,"content":[{"type":"output_text","text":"second"}]}`,
				`{"id":"n1","type":"resend-fork-notice","timestamp":1767225607000}`,
				`{"id":"u4","type":"message","role":"user","timestamp":1767225608000,"content":[{"type":"input_text","text":"start over"}]}`,
				`{"id":"a4","parentId":"u4","type":"message","role":"assistant","timestamp":1767225609000,"content":[{"type":"output_text","text":"fresh"}]}`,
			)
			database := openTestDB(t)
			dollar := money.MustParseDollars("1").Microdollars
			database.SetCustomPricing(map[string]config.CustomModelRate{"luna": {
				InputMicrodollarsPerMTok: dollar, OutputMicrodollarsPerMTok: dollar,
				CacheReadMicrodollarsPerMTok: dollar,
			}})
			engine := newCBCEngine(t, database, home, legacyRoot)
			branches := []string{"codebuddy:conv2-u1", "codebuddy:conv2-u1-u3"}
			check := func(step string) {
				t.Helper()
				for _, id := range branches {
					msgs, err := database.GetMessages(t.Context(), id, 0, 100, true)
					require.NoError(t, err)
					var parts []int
					for i, m := range msgs {
						if strings.HasPrefix(m.SourceUUID, "cb4:req1:a1") {
							parts = append(parts, i)
						}
						// Read the stored counters without the per-request cap,
						// so a row that kept more than the bound fails here.
						for _, key := range []string{"input_tokens", "output_tokens",
							"cache_read_input_tokens", "cache_creation_input_tokens", "reasoning_tokens"} {
							assert.LessOrEqual(t, gjson.GetBytes(m.TokenUsage, key).Int(),
								int64(usagefacts.MaxPlausibleTokens), "%s %s %s", step, id, key)
						}
					}
					require.Len(t, parts, 2, "%s: %s repeats the reply and its second part once", step, id)
					reply, part := msgs[parts[0]], msgs[parts[1]]
					assert.Equal(t, "cb4:req1:a1", reply.SourceUUID, step)
					assert.Equal(t, "done", reply.Content, "%s: the reply keeps the first part", step)
					assert.Equal(t, 90, reply.ContextTokens, "%s: the context is the last step's prompt", step)
					assert.Equal(t, 2_000_000, reply.OutputTokens, step)
					assert.Equal(t, "cb4:req1:a1#2", part.SourceUUID, step)
					assert.Equal(t, parts[0]+1, parts[1], "%s: the second part follows the reply", step)
					assert.True(t, part.IsSystem, "%s: the second part is hidden", step)
					assert.Zero(t, part.ContextTokens, step)
					assert.Equal(t, 100_000, part.OutputTokens, step)
				}

				// 10,000 + 2,100,000 + 2,990,000 tokens at $1 per million.
				want := money.MustParseDollars("5.1")
				totals := cbcDailyTotals(t, database)
				assert.Equal(t, 10_000, totals.InputTokens, step)
				assert.Equal(t, 2_100_000, totals.OutputTokens, "%s: the repeated reply counts once", step)
				assert.Equal(t, 2_990_000, totals.CacheReadTokens, step)
				assert.Equal(t, want, totals.TotalCost, step)

				usage, err := database.GetSessionUsage(t.Context(), branches[0], true)
				require.NoError(t, err)
				require.NotNil(t, usage)
				assert.Equal(t, 2_100_000, usage.TotalOutputTokens, step)
				assert.Equal(t, 90, usage.PeakContextTokens, step)
				assert.Equal(t, want, usage.Cost, step)
				require.Len(t, usage.Breakdown, 2, "%s: one entry per part", step)
				assert.Equal(t, 2_990_000, usage.Breakdown[0].CacheReadInputTokens+
					usage.Breakdown[1].CacheReadInputTokens, step)
				sess, err := database.GetSessionFull(t.Context(), branches[0])
				require.NoError(t, err)
				require.NotNil(t, sess)
				assert.Equal(t, 2_100_000, sess.TotalOutputTokens, step)
				assert.Equal(t, 90, sess.PeakContextTokens, step)
			}
			engine.SyncAll(t.Context(), nil)
			check("first sync")

			require.NoError(t, os.RemoveAll(convDir))
			engine.SyncAll(t.Context(), nil)
			check("sync without the 4.x folder")
			engine.SyncAllForceParse(t.Context(), nil)
			check("forced parse without the 4.x folder")
		})
	}
}
