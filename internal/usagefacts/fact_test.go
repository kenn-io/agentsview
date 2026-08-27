package usagefacts

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseJSONStringMatchesEncodingJSON(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
		next  int
		ok    bool
	}{
		{"plain", `"input_tokens":1`, "input_tokens", 14, true},
		{"empty", `""`, "", 2, true},
		{"escaped quote", `"a\"b"`, `a"b`, 6, true},
		{"escaped slash", `"https:\/\/x"`, "https://x", 13, true},
		{"unicode escape", `"\u00e9"`, "é", 8, true},
		{"raw utf8", `"é"`, "é", 4, true},
		{"trailing escape", `"abc\`, "", 5, false},
		{"unterminated", `"abc`, "", 4, false},
		{"not a string", `123`, "", 0, false},
		{"raw control char", "\"a\tb\"", "", 5, false},
		{"invalid utf8", "\"a\xffb\"", "a\ufffdb", 5, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, next, ok := parseJSONString(tc.input, 0)
			assert.Equal(t, tc.ok, ok, "ok")
			assert.Equal(t, tc.next, next, "next")
			assert.Equal(t, tc.want, got, "value")
			var want string
			if !tc.ok {
				require.Error(t, json.Unmarshal([]byte(tc.input), &want))
				return
			}
			require.NoError(t, json.Unmarshal([]byte(tc.input[:tc.next]), &want))
			assert.Equal(t, want, got, "json.Unmarshal parity")
		})
	}
}

func TestParseTokenUsage(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want ParsedTokenUsage
	}{
		{
			name: "all counters and web search",
			raw: `{"input_tokens":100,"output_tokens":"50",` +
				`"reasoning_tokens":25,"cache_creation_input_tokens":20,` +
				`"cache_read_input_tokens":300,"server_tool_use":` +
				`{"web_fetch_requests":9,"web_search_requests":"3"}}`,
			want: ParsedTokenUsage{
				InputTokens: 100, OutputTokens: 50, ReasoningTokens: 25,
				CacheCreationTokens: 20, CacheReadTokens: 300,
				WebSearchRequests: 3,
			},
		},
		{
			name: "leading fields survive truncation",
			raw:  `{"input_tokens":9999,"output_tokens":42,"cache`,
			want: ParsedTokenUsage{InputTokens: 9999, OutputTokens: 42},
		},
		{
			name: "negative and implausible counters are clamped",
			raw: `{"input_tokens":-5,"output_tokens":9999999999999,` +
				`"reasoning_tokens":"-2","cache_read_input_tokens":2000001,` +
				`"server_tool_use":{"web_search_requests":-4}}`,
			want: ParsedTokenUsage{
				OutputTokens:    MaxPlausibleTokens,
				CacheReadTokens: MaxPlausibleTokens,
			},
		},
		{
			name: "malformed JSON",
			raw:  `not-json`,
			want: ParsedTokenUsage{},
		},
		{
			name: "invalid utf8 in unrelated value",
			raw:  "{\"note\":\"a\xffb\",\"output_tokens\":7}",
			want: ParsedTokenUsage{OutputTokens: 7},
		},
		{
			name: "reasoning only",
			raw:  `{"reasoning_tokens":17}`,
			want: ParsedTokenUsage{ReasoningTokens: 17},
		},
		{
			name: "nested cache creation 1h breakdown",
			raw: `{"input_tokens":2,"output_tokens":62,` +
				`"cache_creation_input_tokens":8989,` +
				`"cache_read_input_tokens":15892,` +
				`"cache_creation":{"ephemeral_1h_input_tokens":8989,` +
				`"ephemeral_5m_input_tokens":0}}`,
			want: ParsedTokenUsage{
				InputTokens: 2, OutputTokens: 62,
				CacheCreationTokens: 8989, CacheCreation1hTokens: 8989,
				CacheReadTokens: 15892,
			},
		},
		{
			name: "mixed cache creation TTLs",
			raw: `{"cache_creation_input_tokens":250,` +
				`"cache_creation":{"ephemeral_5m_input_tokens":150,` +
				`"ephemeral_1h_input_tokens":100}}`,
			want: ParsedTokenUsage{
				CacheCreationTokens: 250, CacheCreation1hTokens: 100,
			},
		},
		{
			name: "1h breakdown clamped to the flat total",
			raw: `{"cache_creation":{"ephemeral_1h_input_tokens":500},` +
				`"cache_creation_input_tokens":250}`,
			want: ParsedTokenUsage{
				CacheCreationTokens: 250, CacheCreation1hTokens: 250,
			},
		},
		{
			name: "1h breakdown without a flat total reads as zero",
			raw:  `{"cache_creation":{"ephemeral_1h_input_tokens":500}}`,
			want: ParsedTokenUsage{},
		},
		{
			name: "5m-only breakdown leaves the 1h count zero",
			raw: `{"cache_creation_input_tokens":77,` +
				`"cache_creation":{"ephemeral_5m_input_tokens":77}}`,
			want: ParsedTokenUsage{CacheCreationTokens: 77},
		},
		{
			name: "negative and string 1h counts",
			raw: `{"cache_creation_input_tokens":50,` +
				`"cache_creation":{"ephemeral_1h_input_tokens":"-3"}}`,
			want: ParsedTokenUsage{CacheCreationTokens: 50},
		},
		{
			name: "string-number 1h count is accepted",
			raw: `{"cache_creation_input_tokens":50,` +
				`"cache_creation":{"ephemeral_1h_input_tokens":"20"}}`,
			want: ParsedTokenUsage{
				CacheCreationTokens: 50, CacheCreation1hTokens: 20,
			},
		},
		{
			name: "truncated nested breakdown is ignored",
			raw: `{"cache_creation_input_tokens":50,` +
				`"cache_creation":{"ephemeral_1h_input_tokens":20`,
			want: ParsedTokenUsage{CacheCreationTokens: 50},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ParseTokenUsage(tc.raw))
		})
	}
}

func TestFromMessage(t *testing.T) {
	fact, ok := FromMessage(MessageInput{
		Ordinal: 4, Role: "assistant", Timestamp: "2024-01-01T00:00:00Z",
		Model: "claude-test", TokenUsage: `{"input_tokens":10,"reasoning_tokens":2,` +
			`"cache_creation_input_tokens":30,` +
			`"cache_creation":{"ephemeral_1h_input_tokens":12}}`,
		ClaudeMessageID: "msg-1", ClaudeRequestID: "req-1", SourceUUID: "source-1",
	})
	require.True(t, ok)
	ordinal := 4
	millis := int64(1704067200000)
	nanos := int64(0)
	assert.Equal(t, Fact{
		Source: "message", MessageOrdinal: &ordinal,
		TimestampMillis: &millis, TimestampNanos: &nanos,
		RawTimestamp: "2024-01-01T00:00:00Z",
		Model:        "claude-test",
		InputTokens:  10, ReasoningTokens: 2,
		CacheCreationTokens: 30, CacheCreation1hTokens: 12,
		RequestScoped:   true,
		ClaudeMessageID: "msg-1", ClaudeRequestID: "req-1",
		SourceUUID: "source-1", TokenEligible: true, ActivityEligible: true,
	}, fact)

	activity, ok := FromMessage(MessageInput{
		Ordinal: 7, Role: "assistant", Timestamp: "", Model: "",
	})
	require.True(t, ok)
	ordinal = 7
	assert.Equal(t, Fact{
		Source: "message", MessageOrdinal: &ordinal,
		UsesSessionStart: true, RequestScoped: true, ActivityEligible: true,
	}, activity)

	malformed, ok := FromMessage(MessageInput{
		Ordinal: 8, Role: "assistant", Timestamp: "not-a-time",
	})
	require.True(t, ok)
	assert.Nil(t, malformed.TimestampMillis)
	assert.Equal(t, "not-a-time", malformed.RawTimestamp)
	assert.False(t, malformed.UsesSessionStart)

	_, ok = FromMessage(MessageInput{Role: "user"})
	assert.False(t, ok)
	_, ok = FromMessage(MessageInput{
		Role: "assistant", Model: "<synthetic>", TokenUsage: `{"input_tokens":1}`,
	})
	assert.False(t, ok)
}

func TestFromEvent(t *testing.T) {
	ordinal := 3
	reportedCost := int64(123456)
	fact, ok := FromEvent(EventInput{
		MessageOrdinal: &ordinal, Source: "goose-request",
		Timestamp: "", Model: "model-x", CostSource: "provider",
		DedupKey:    "session-1:goose-request:event-key",
		InputTokens: -1, OutputTokens: 2_000_001,
		ReasoningTokens: 4, CacheCreationTokens: 5, CacheReadTokens: 6,
		ReportedCostMicrodollars: &reportedCost,
	})
	require.True(t, ok)
	assert.Equal(t, Fact{
		Source: "goose-request", MessageOrdinal: &ordinal,
		UsesSessionStart: true, Model: "model-x",
		OutputTokens: MaxPlausibleTokens, ReasoningTokens: 4,
		CacheCreationTokens: 5, CacheReadTokens: 6,
		ReportedCostMicrodollars: &reportedCost, CostSource: "provider",
		RequestScoped: true,
		UsageDedupKey: "session-1:goose-request:event-key",
		TokenEligible: true, ActivityEligible: true,
	}, fact)

	session, ok := FromEvent(EventInput{
		Source: "session", Model: "model-x", InputTokens: 3_000_000,
		OutputTokens: -2,
	})
	require.True(t, ok)
	assert.Equal(t, int64(3_000_000), session.InputTokens,
		"authoritative session totals may exceed the per-request clamp")
	assert.Zero(t, session.OutputTokens)
	assert.False(t, session.RequestScoped)
	assert.Equal(t, "session-1:session:key",
		EventDedupKey("session-1", "session", "key", 10))
	assert.Equal(t, "session-1:session:id:10",
		EventDedupKey("session-1", "session", "", 10))

	_, ok = FromEvent(EventInput{Source: "session"})
	assert.False(t, ok)
}

func TestParseTimestamp(t *testing.T) {
	millis, raw, fallback := ParseTimestamp("2024-01-01T00:00:00.123456Z")
	require.NotNil(t, millis)
	assert.Equal(t, int64(1704067200123), *millis)
	assert.Equal(t, "2024-01-01T00:00:00.123456Z", raw)
	require.NotNil(t, ParseTimestampNanos("2024-01-01T00:00:00.123456Z"))
	assert.Equal(t, int64(456000),
		*ParseTimestampNanos("2024-01-01T00:00:00.123456Z"))
	assert.Equal(t, int64(999999),
		*ParseTimestampNanos("2500-01-01T00:00:00.123999999Z"))
	assert.False(t, fallback)

	millis, raw, fallback = ParseTimestamp("")
	assert.Nil(t, millis)
	assert.Empty(t, raw)
	assert.True(t, fallback)

	millis, raw, fallback = ParseTimestamp("not-a-time")
	assert.Nil(t, millis)
	assert.Equal(t, "not-a-time", raw)
	assert.False(t, fallback)
}
