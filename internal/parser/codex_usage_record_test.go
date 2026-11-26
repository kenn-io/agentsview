package parser

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func codexUsageRecordLine(t *testing.T, ts, responseID string, input, cached, output, reasoning, total int) string {
	t.Helper()
	return mustCodexLine(t, map[string]any{
		"type":      "token_usage_record",
		"timestamp": ts,
		"payload": map[string]any{
			"response_id": responseID,
			"usage": map[string]int{
				"input_tokens": input, "cached_input_tokens": cached,
				"output_tokens": output, "reasoning_output_tokens": reasoning,
				"total_tokens": total,
			},
		},
	})
}

func codexUsageCountLine(t *testing.T, ts string, input, cached, output, reasoning, total int) string {
	t.Helper()
	return codexUsageCountLineWithCumulative(t, ts, 1, input, cached, output, reasoning, total)
}

// codexUsageCountLineWithCumulative writes a token_count whose cumulative
// totals are the per-call counters multiplied by calls.
func codexUsageCountLineWithCumulative(t *testing.T, ts string, calls, input, cached, output, reasoning, total int) string {
	t.Helper()
	usage := map[string]int{
		"input_tokens": input, "cached_input_tokens": cached,
		"output_tokens": output, "reasoning_output_tokens": reasoning,
		"total_tokens": total,
	}
	cumulative := map[string]int{}
	for key, value := range usage {
		cumulative[key] = value * calls
	}
	return mustCodexLine(t, map[string]any{
		"type": "event_msg", "timestamp": ts,
		"payload": map[string]any{
			"type": "token_count",
			"info": map[string]any{
				"last_token_usage": usage, "total_token_usage": cumulative,
				"model_context_window": 272000,
			},
		},
	})
}

func codexUsageFixture(t *testing.T, rows ...string) string {
	t.Helper()
	base := []string{
		testjsonl.CodexSessionMetaJSON("usage-record", "/workspace/project", "codex_cli_rs", tsEarly),
		testjsonl.CodexTurnContextJSON("gpt-5.4", tsEarlyS1),
		testjsonl.CodexMsgJSON("user", "hello", tsEarlyS1),
	}
	return testjsonl.JoinJSONL(append(base, rows...)...)
}

func TestCodexTokenUsageRecordAndMirror(t *testing.T) {
	const usage = `{"input_tokens":60,"output_tokens":12,"cache_read_input_tokens":40,"reasoning_tokens":5}`
	for _, tc := range []struct {
		name string
		rows string
		want int
	}{
		{
			name: "record mirror counts once",
			rows: testjsonl.JoinJSONL(
				testjsonl.CodexMsgJSON("assistant", "reply", tsEarlyS5),
				codexUsageRecordLine(t, tsLate, "response-1", 100, 40, 12, 5, 112),
				codexUsageCountLine(t, tsLate, 100, 40, 12, 5, 112),
			),
			want: 1,
		},
		{name: "count-only later turn with same counters counts after mirror expires", rows: testjsonl.JoinJSONL(
			testjsonl.CodexMsgJSON("assistant", "first", tsEarlyS5),
			codexUsageRecordLine(t, tsLate, "response-1", 100, 40, 12, 5, 112),
			codexUsageCountLine(t, tsLate, 100, 40, 12, 5, 112),
			testjsonl.CodexTurnContextJSON("gpt-5.4", tsLateS5),
			testjsonl.CodexMsgJSON("assistant", "second", tsLateS5),
			codexUsageCountLineWithCumulative(t, tsLateS5, 2, 100, 40, 12, 5, 112),
		), want: 2},
		{name: "record mirror after intervening transcript lines counts once", rows: testjsonl.JoinJSONL(
			testjsonl.CodexMsgJSON("assistant", "reply", tsEarlyS5),
			codexUsageRecordLine(t, tsLate, "response-1", 100, 40, 12, 5, 112),
			testjsonl.CodexFunctionCallOutputJSON("call-1", "ok", tsLate),
			codexUsageCountLine(t, tsLate, 100, 40, 12, 5, 112),
		), want: 1},
		{
			name: "lone record counts",
			rows: testjsonl.JoinJSONL(
				testjsonl.CodexMsgJSON("assistant", "reply", tsEarlyS5),
				codexUsageRecordLine(t, tsLate, "response-1", 100, 40, 12, 5, 112),
			),
			want: 1,
		},
		{
			name: "record repeats dedupe",
			rows: testjsonl.JoinJSONL(
				testjsonl.CodexMsgJSON("assistant", "reply", tsEarlyS5),
				codexUsageRecordLine(t, tsLate, "response-1", 100, 40, 12, 5, 112),
				codexUsageRecordLine(t, tsLate, "response-1", 100, 40, 12, 5, 112),
			),
			want: 1,
		},
		{
			name: "record repeats without response id dedupe",
			rows: testjsonl.JoinJSONL(
				testjsonl.CodexMsgJSON("assistant", "reply", tsEarlyS5),
				codexUsageRecordLine(t, tsLate, "", 100, 40, 12, 5, 112),
				codexUsageRecordLine(t, tsLate, "", 100, 40, 12, 5, 112),
			),
			want: 1,
		},
		{
			name: "record repeats after its mirror dedupe",
			rows: testjsonl.JoinJSONL(
				testjsonl.CodexMsgJSON("assistant", "reply", tsEarlyS5),
				codexUsageRecordLine(t, tsLate, "response-1", 100, 40, 12, 5, 112),
				codexUsageCountLine(t, tsLate, 100, 40, 12, 5, 112),
				codexUsageRecordLine(t, tsLate, "response-1", 100, 40, 12, 5, 112),
			),
			want: 1,
		},
		{
			name: "mirror repeated after a repeated record dedupes",
			rows: testjsonl.JoinJSONL(
				testjsonl.CodexMsgJSON("assistant", "reply", tsEarlyS5),
				codexUsageRecordLine(t, tsLate, "response-1", 100, 40, 12, 5, 112),
				codexUsageCountLine(t, tsLate, 100, 40, 12, 5, 112),
				codexUsageRecordLine(t, tsLate, "response-1", 100, 40, 12, 5, 112),
				codexUsageCountLine(t, tsLate, 100, 40, 12, 5, 112),
			),
			want: 1,
		},
		{
			name: "count-only response with equal counters after a repeated record counts",
			rows: testjsonl.JoinJSONL(
				testjsonl.CodexMsgJSON("assistant", "first", tsEarlyS5),
				codexUsageRecordLine(t, tsLate, "response-1", 100, 40, 12, 5, 112),
				codexUsageCountLine(t, tsLate, 100, 40, 12, 5, 112),
				codexUsageRecordLine(t, tsLate, "response-1", 100, 40, 12, 5, 112),
				testjsonl.CodexMsgJSON("assistant", "second", tsLate),
				codexUsageCountLineWithCumulative(t, tsLate, 2, 100, 40, 12, 5, 112),
			),
			want: 2,
		},
		{
			name: "equal counters from distinct responses count twice",
			rows: testjsonl.JoinJSONL(
				testjsonl.CodexMsgJSON("assistant", "first", tsEarlyS5),
				codexUsageRecordLine(t, tsLate, "response-1", 100, 40, 12, 5, 112),
				testjsonl.CodexMsgJSON("assistant", "second", tsLate),
				codexUsageRecordLine(t, tsLate, "response-2", 100, 40, 12, 5, 112),
			),
			want: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, messages := runCodexParserTest(t, "usage-record.jsonl", codexUsageFixture(t, tc.rows), false)
			var got int
			for _, message := range messages {
				if len(message.TokenUsage) > 0 {
					got++
					assert.JSONEq(t, usage, string(message.TokenUsage))
				}
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestCodexTokenCountRepeatAfterDistinctRecordKeepsRecordMirror(t *testing.T) {
	content := codexUsageFixture(t,
		testjsonl.CodexMsgJSON("assistant", "first", tsEarlyS5),
		codexUsageCountLine(t, tsLate, 80, 30, 10, 4, 90),
		testjsonl.CodexMsgJSON("assistant", "second", tsLate),
		codexUsageRecordLine(t, tsLate, "response-2", 100, 40, 12, 5, 112),
		codexUsageCountLine(t, tsLate, 80, 30, 10, 4, 90),
		codexUsageCountLine(t, tsLate, 100, 40, 12, 5, 112),
	)
	_, messages := runCodexParserTest(t, "usage-repeat-distinct.jsonl", content, false)
	var usages []string
	for _, message := range messages {
		if len(message.TokenUsage) > 0 {
			usages = append(usages, string(message.TokenUsage))
		}
	}
	require.Len(t, usages, 2)
	assert.JSONEq(t, `{"input_tokens":50,"output_tokens":10,"cache_read_input_tokens":30,"reasoning_tokens":4}`, usages[0])
	assert.JSONEq(t, `{"input_tokens":60,"output_tokens":12,"cache_read_input_tokens":40,"reasoning_tokens":5}`, usages[1])
}

func TestCodexPendingMirrorExpiresAtTurnAcrossCheckpoint(t *testing.T) {
	prefix := codexUsageFixture(t,
		testjsonl.CodexMsgJSON("assistant", "first", tsEarlyS5),
		codexUsageRecordLine(t, tsLate, "", 100, 40, 12, 5, 112),
	)
	tail := testjsonl.JoinJSONL(
		testjsonl.CodexTurnContextJSON("gpt-5.4", tsLateS5),
		testjsonl.CodexMsgJSON("assistant", "second", tsLateS5),
		codexUsageCountLineWithCumulative(t, tsLateS5, 2, 100, 40, 12, 5, 112),
	) + "\n"

	fullPath := createTestFile(t, "usage-turn-full.jsonl", prefix+tail)
	_, fullMessages, err := parseCodexTestSession(t, fullPath, "local", false)
	require.NoError(t, err)
	var fullUsage int
	for _, message := range fullMessages {
		if len(message.TokenUsage) > 0 {
			fullUsage++
		}
	}
	require.Equal(t, 2, fullUsage, "a later turn's count-only usage is not the earlier record's mirror")

	incrementalPath := createTestFile(t, "usage-turn-incremental.jsonl", prefix)
	provider := newCodexTestProvider(t)
	_, prefixMessages, err := provider.parseSession(incrementalPath, "local", false)
	require.NoError(t, err)
	prefixInfo, err := os.Stat(incrementalPath)
	require.NoError(t, err)
	file, err := os.OpenFile(incrementalPath, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = file.WriteString(tail)
	require.NoError(t, err)
	require.NoError(t, file.Close())

	incremental, err := provider.parseSessionFromDetailed(t.Context(), incrementalPath, prefixInfo.Size(), len(prefixMessages), false)
	require.NoError(t, err)
	require.NotNil(t, incremental)
	got := append(append([]ParsedMessage(nil), prefixMessages...), incremental.messages...)
	var incrementalUsage int
	for _, message := range got {
		if len(message.TokenUsage) > 0 {
			incrementalUsage++
		}
	}
	assert.Equal(t, fullUsage, incrementalUsage)
}

func TestCodexRecordMirrorUsesCumulativeTotal(t *testing.T) {
	for _, tc := range []struct {
		name  string
		calls int
		want  int
	}{
		{name: "cumulative grows by the record is its mirror", calls: 2, want: 2},
		{name: "cumulative grows past the record is a separate response", calls: 3, want: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prefix := codexUsageFixture(t,
				testjsonl.CodexMsgJSON("assistant", "first", tsEarlyS5),
				codexUsageCountLineWithCumulative(t, tsLate, 1, 100, 40, 12, 5, 112),
				testjsonl.CodexMsgJSON("assistant", "second", tsLate),
				codexUsageRecordLine(t, tsLate, "", 100, 40, 12, 5, 112),
			)
			tail := testjsonl.JoinJSONL(
				testjsonl.CodexMsgJSON("assistant", "third", tsLate),
				codexUsageCountLineWithCumulative(t, tsLate, tc.calls, 100, 40, 12, 5, 112),
			) + "\n"
			countUsage := func(messages []ParsedMessage) int {
				var got int
				for _, message := range messages {
					if len(message.TokenUsage) > 0 {
						got++
					}
				}
				return got
			}

			fullPath := createTestFile(t, "usage-cumulative-full.jsonl", prefix+tail)
			_, fullMessages, err := parseCodexTestSession(t, fullPath, "local", false)
			require.NoError(t, err)
			assert.Equal(t, tc.want, countUsage(fullMessages))

			incrementalPath := createTestFile(t, "usage-cumulative-incremental.jsonl", prefix)
			provider := newCodexTestProvider(t)
			_, prefixMessages, err := provider.parseSession(incrementalPath, "local", false)
			require.NoError(t, err)
			prefixInfo, err := os.Stat(incrementalPath)
			require.NoError(t, err)
			file, err := os.OpenFile(incrementalPath, os.O_APPEND|os.O_WRONLY, 0o644)
			require.NoError(t, err)
			_, err = file.WriteString(tail)
			require.NoError(t, err)
			require.NoError(t, file.Close())
			incremental, err := provider.parseSessionFromDetailed(t.Context(), incrementalPath, prefixInfo.Size(), len(prefixMessages), false)
			require.NoError(t, err)
			require.NotNil(t, incremental)
			got := append(append([]ParsedMessage(nil), prefixMessages...), incremental.messages...)
			assert.Equal(t, tc.want, countUsage(got))
		})
	}
}

func TestCodexMirrorAfterTwoRecordsCountsEachOnce(t *testing.T) {
	content := codexUsageFixture(t,
		testjsonl.CodexMsgJSON("assistant", "first", tsEarlyS5),
		codexUsageCountLineWithCumulative(t, tsLate, 1, 100, 40, 12, 5, 112),
		testjsonl.CodexMsgJSON("assistant", "second", tsLate),
		codexUsageRecordLine(t, tsLate, "response-2", 100, 40, 12, 5, 112),
		testjsonl.CodexMsgJSON("assistant", "third", tsLate),
		codexUsageRecordLine(t, tsLate, "response-3", 100, 40, 12, 5, 112),
		codexUsageCountLineWithCumulative(t, tsLate, 3, 100, 40, 12, 5, 112),
	)
	_, messages := runCodexParserTest(t, "usage-two-records.jsonl", content, false)
	var got int
	for _, message := range messages {
		if len(message.TokenUsage) > 0 {
			got++
		}
	}
	assert.Equal(t, 3, got)
}

func TestCodexIdlessRecordAfterDistinctUsageCounts(t *testing.T) {
	content := codexUsageFixture(t,
		testjsonl.CodexMsgJSON("assistant", "reply", tsEarlyS5),
		codexUsageRecordLine(t, tsLate, "", 100, 40, 12, 5, 112),
		codexUsageCountLine(t, tsLate, 110, 40, 14, 6, 124),
		codexUsageRecordLine(t, tsLate, "", 100, 40, 12, 5, 112),
	)
	_, messages := runCodexParserTest(t, "usage-idless-distinct.jsonl", content, false)
	var got int
	for _, message := range messages {
		if len(message.TokenUsage) > 0 {
			got++
		}
	}
	assert.Equal(t, 3, got)
}

func TestCodexTokenCountRepeatAfterRecordMirrorStillDedupes(t *testing.T) {
	content := codexUsageFixture(t,
		testjsonl.CodexMsgJSON("assistant", "reply", tsEarlyS5),
		codexUsageRecordLine(t, tsLate, "response-1", 100, 40, 12, 5, 112),
		codexUsageCountLine(t, tsLate, 100, 40, 12, 5, 112),
		codexUsageCountLine(t, tsLate, 100, 40, 12, 5, 112),
	)
	_, messages := runCodexParserTest(t, "usage-repeat.jsonl", content, false)
	require.Len(t, messages, 2)
	assert.JSONEq(t, `{"input_tokens":60,"output_tokens":12,"cache_read_input_tokens":40,"reasoning_tokens":5}`,
		string(messages[1].TokenUsage))
}

func TestCodexTokenCountDifferentFromRecordStillApplies(t *testing.T) {
	content := codexUsageFixture(t,
		testjsonl.CodexMsgJSON("assistant", "reply", tsEarlyS5),
		codexUsageRecordLine(t, tsLate, "response-1", 100, 40, 12, 5, 112),
		codexUsageCountLine(t, tsLate, 110, 40, 14, 6, 124),
	)
	_, messages := runCodexParserTest(t, "usage-different.jsonl", content, false)
	require.Len(t, messages, 3)
	assert.JSONEq(t, `{"input_tokens":60,"output_tokens":12,"cache_read_input_tokens":40,"reasoning_tokens":5}`,
		string(messages[1].TokenUsage))
	assert.JSONEq(t, `{"input_tokens":70,"output_tokens":14,"cache_read_input_tokens":40,"reasoning_tokens":6}`,
		string(messages[2].TokenUsage))
}

func TestCodexRecordMirrorIncrementalMatchesFullParse(t *testing.T) {
	prefix := codexUsageFixture(t,
		testjsonl.CodexMsgJSON("assistant", "reply", tsEarlyS5),
		codexUsageRecordLine(t, tsLate, "response-1", 100, 40, 12, 5, 112),
	)
	tail := codexUsageCountLine(t, tsLate, 100, 40, 12, 5, 112) + "\n"

	fullPath := createTestFile(t, "usage-full.jsonl", prefix+tail)
	full, fullMessages, err := parseCodexTestSession(t, fullPath, "local", false)
	require.NoError(t, err)
	require.NotNil(t, full)

	incrementalPath := createTestFile(t, "usage-incremental.jsonl", prefix)
	provider := newCodexTestProvider(t)
	_, prefixMessages, err := provider.parseSession(incrementalPath, "local", false)
	require.NoError(t, err)
	prefixInfo, err := os.Stat(incrementalPath)
	require.NoError(t, err)
	file, err := os.OpenFile(incrementalPath, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = file.WriteString(tail)
	require.NoError(t, err)
	require.NoError(t, file.Close())

	incremental, err := provider.parseSessionFromDetailed(t.Context(), incrementalPath, prefixInfo.Size(), len(prefixMessages), false)
	require.NoError(t, err)
	require.NotNil(t, incremental)
	require.Empty(t, incremental.messages, "the mirrored token_count must not create a second message")
	assert.Equal(t, fullMessages, prefixMessages)
}

func TestCodexRecordRepeatAfterMirrorIncrementalDedupesWithoutResponseID(t *testing.T) {
	prefix := codexUsageFixture(t,
		testjsonl.CodexMsgJSON("assistant", "reply", tsEarlyS5),
		codexUsageRecordLine(t, tsLate, "", 100, 40, 12, 5, 112),
		codexUsageCountLine(t, tsLate, 100, 40, 12, 5, 112),
	)
	tail := codexUsageRecordLine(t, tsLate, "", 100, 40, 12, 5, 112) + "\n"

	fullPath := createTestFile(t, "usage-idless-repeat-full.jsonl", prefix+tail)
	full, fullMessages, err := parseCodexTestSession(t, fullPath, "local", false)
	require.NoError(t, err)
	require.NotNil(t, full)

	incrementalPath := createTestFile(t, "usage-idless-repeat-incremental.jsonl", prefix)
	provider := newCodexTestProvider(t)
	_, prefixMessages, err := provider.parseSession(incrementalPath, "local", false)
	require.NoError(t, err)
	prefixInfo, err := os.Stat(incrementalPath)
	require.NoError(t, err)
	file, err := os.OpenFile(incrementalPath, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = file.WriteString(tail)
	require.NoError(t, err)
	require.NoError(t, file.Close())

	incremental, err := provider.parseSessionFromDetailed(t.Context(), incrementalPath, prefixInfo.Size(), len(prefixMessages), false)
	require.NoError(t, err)
	require.NotNil(t, incremental)
	require.Empty(t, incremental.messages)
	assert.Equal(t, fullMessages, prefixMessages)
}

func TestCodexCursorCheckpointPersistsRecordMirrorState(t *testing.T) {
	seed := codexCursorState{
		lastTokenUsageDigest:         tokenUsageDigest(`{"input_tokens":100,"cached_input_tokens":40,"output_tokens":12,"reasoning_output_tokens":5,"total_tokens":112}`),
		lastTokenUsageRecordDigest:   tokenUsageDigest(`{"input_tokens":100,"cached_input_tokens":40,"output_tokens":12,"reasoning_output_tokens":5,"total_tokens":112}`),
		pendingUsageRecordDigest:     tokenUsageDigest(`{"input_tokens":100,"cached_input_tokens":40,"output_tokens":12,"reasoning_output_tokens":5,"total_tokens":112}`),
		lastTokenUsageSeen:           true,
		pendingUsageRecord:           true,
		pendingUsageRecordResponseID: "response-1",
		lastTokenUsageResponseID:     "response-1",
		lastTokenCountSeen:           true,
		lastTokenCountTotalKnown:     true,
		lastTokenCountTotal:          224,
		recordTotalSinceTokenCount:   112,
	}
	blob, err := seed.MarshalBinary()
	require.NoError(t, err)
	var got codexCursorState
	require.NoError(t, got.UnmarshalBinary(blob))
	assert.Equal(t, seed.lastTokenUsageDigest, got.lastTokenUsageDigest)
	assert.Equal(t, seed.lastTokenUsageRecordDigest, got.lastTokenUsageRecordDigest)
	assert.Equal(t, seed.lastTokenUsageSeen, got.lastTokenUsageSeen)
	assert.Equal(t, seed.lastTokenUsageResponseID, got.lastTokenUsageResponseID)
	assert.Equal(t, seed.pendingUsageRecord, got.pendingUsageRecord)
	assert.Equal(t, seed.pendingUsageRecordDigest, got.pendingUsageRecordDigest)
	assert.Equal(t, seed.lastTokenCountSeen, got.lastTokenCountSeen)
	assert.Equal(t, seed.lastTokenCountTotalKnown, got.lastTokenCountTotalKnown)
	assert.Equal(t, seed.lastTokenCountTotal, got.lastTokenCountTotal)
	assert.Equal(t, seed.recordTotalSinceTokenCount, got.recordTotalSinceTokenCount)
	assert.Equal(t, seed.pendingUsageRecordResponseID, got.pendingUsageRecordResponseID)

	// A version 6 checkpoint has no record-mirror state. Rejecting it sends
	// the caller to a full parse, as for any other version mismatch.
	blob[0] = 6
	require.Error(t, got.UnmarshalBinary(blob))
}
