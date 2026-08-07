// ABOUTME: Tests that Claude assistant messages record how many billed
// ABOUTME: server-side web searches their WebSearch tool calls performed.
package parser

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// parseClaudeWebSearchRequests parses lines as a Claude transcript and
// returns the web_search_requests recorded on each assistant message that
// carries usage, keyed by the message's source uuid.
func parseClaudeWebSearchRequests(
	t *testing.T, filename string, lines []string,
) map[string]int {
	t.Helper()
	path := createTestFile(t, filename, strings.Join(lines, "\n"))
	results, err := parseClaudeSession(path, "proj", "local")
	require.NoError(t, err)
	require.NotEmpty(t, results)

	out := make(map[string]int)
	for _, msg := range results[0].Messages {
		if len(msg.TokenUsage) == 0 {
			continue
		}
		out[msg.SourceUUID] = int(gjson.GetBytes(
			msg.TokenUsage, usageWebSearchRequestsPath).Int())
	}
	return out
}

func TestClaudeWebSearchRequestsPerMessage(t *testing.T) {
	const userLine = `{"type":"user","timestamp":"2026-07-30T10:00:00Z",` +
		`"uuid":"u1","message":{"content":"find something"},"cwd":"/tmp"}`

	tests := []struct {
		name  string
		lines []string
		want  map[string]int
	}{
		{
			name: "reported counter is used as is",
			lines: []string{
				userLine,
				`{"type":"assistant","timestamp":"2026-07-30T10:00:01Z",` +
					`"uuid":"a1","parentUuid":"u1","message":{"id":"msg_1",` +
					`"model":"claude-sonnet-4-5","content":[{"type":"text",` +
					`"text":"searched"}],"usage":{"input_tokens":10,` +
					`"output_tokens":5,"server_tool_use":` +
					`{"web_search_requests":3,"web_fetch_requests":0}}}}`,
			},
			want: map[string]int{"a1": 3},
		},
		{
			name: "zero counter falls back to linked searchCount",
			lines: []string{
				userLine,
				`{"type":"assistant","timestamp":"2026-07-30T10:00:01Z",` +
					`"uuid":"a1","parentUuid":"u1","message":{"id":"msg_1",` +
					`"model":"claude-sonnet-4-5","content":[{"type":"tool_use",` +
					`"id":"toolu_s1","name":"WebSearch","input":{"query":"q"}}],` +
					`"usage":{"input_tokens":10,"output_tokens":5,` +
					`"server_tool_use":{"web_search_requests":0,` +
					`"web_fetch_requests":0}}}}`,
				`{"type":"user","timestamp":"2026-07-30T10:00:04Z","uuid":"u2",` +
					`"parentUuid":"a1","message":{"content":[{"type":` +
					`"tool_result","tool_use_id":"toolu_s1","content":"hits"}]},` +
					`"toolUseResult":{"query":"q","results":[],` +
					`"durationSeconds":2,"searchCount":1}}`,
			},
			want: map[string]int{"a1": 1},
		},
		{
			name: "reported counter wins over linked searchCount",
			lines: []string{
				userLine,
				`{"type":"assistant","timestamp":"2026-07-30T10:00:01Z",` +
					`"uuid":"a1","parentUuid":"u1","message":{"id":"msg_1",` +
					`"model":"claude-sonnet-4-5","content":[{"type":"tool_use",` +
					`"id":"toolu_s1","name":"WebSearch","input":{"query":"q"}}],` +
					`"usage":{"input_tokens":10,"output_tokens":5,` +
					`"server_tool_use":{"web_search_requests":1}}}}`,
				`{"type":"user","timestamp":"2026-07-30T10:00:04Z","uuid":"u2",` +
					`"parentUuid":"a1","message":{"content":[{"type":` +
					`"tool_result","tool_use_id":"toolu_s1","content":"hits"}]},` +
					`"toolUseResult":{"query":"q","searchCount":1}}`,
			},
			want: map[string]int{"a1": 1},
		},
		{
			name: "several searches in one message are summed",
			lines: []string{
				userLine,
				`{"type":"assistant","timestamp":"2026-07-30T10:00:01Z",` +
					`"uuid":"a1","parentUuid":"u1","message":{"id":"msg_1",` +
					`"model":"claude-sonnet-4-5","content":[{"type":"tool_use",` +
					`"id":"toolu_s1","name":"WebSearch","input":{"query":"a"}},` +
					`{"type":"tool_use","id":"toolu_s2","name":"WebSearch",` +
					`"input":{"query":"b"}}],"usage":{"input_tokens":10,` +
					`"output_tokens":5,"server_tool_use":` +
					`{"web_search_requests":0}}}}`,
				`{"type":"user","timestamp":"2026-07-30T10:00:04Z","uuid":"u2",` +
					`"parentUuid":"a1","message":{"content":[{"type":` +
					`"tool_result","tool_use_id":"toolu_s1","content":"hits"}]},` +
					`"toolUseResult":{"query":"a","searchCount":2}}`,
				`{"type":"user","timestamp":"2026-07-30T10:00:05Z","uuid":"u3",` +
					`"parentUuid":"u2","message":{"content":[{"type":` +
					`"tool_result","tool_use_id":"toolu_s2","content":"hits"}]},` +
					`"toolUseResult":{"query":"b","searchCount":3}}`,
			},
			want: map[string]int{"a1": 5},
		},
		{
			name: "usage without server_tool_use gains the count",
			lines: []string{
				userLine,
				`{"type":"assistant","timestamp":"2026-07-30T10:00:01Z",` +
					`"uuid":"a1","parentUuid":"u1","message":{"id":"msg_1",` +
					`"model":"claude-sonnet-4-5","content":[{"type":"tool_use",` +
					`"id":"toolu_s1","name":"WebSearch","input":{"query":"q"}}],` +
					`"usage":{"input_tokens":10,"output_tokens":5}}}`,
				`{"type":"user","timestamp":"2026-07-30T10:00:04Z","uuid":"u2",` +
					`"parentUuid":"a1","message":{"content":[{"type":` +
					`"tool_result","tool_use_id":"toolu_s1","content":"hits"}]},` +
					`"toolUseResult":{"query":"q","searchCount":1}}`,
			},
			want: map[string]int{"a1": 1},
		},
		{
			name: "no web search leaves usage alone",
			lines: []string{
				userLine,
				`{"type":"assistant","timestamp":"2026-07-30T10:00:01Z",` +
					`"uuid":"a1","parentUuid":"u1","message":{"id":"msg_1",` +
					`"model":"claude-sonnet-4-5","content":[{"type":"tool_use",` +
					`"id":"toolu_r1","name":"Read","input":{"file_path":"/x"}}],` +
					`"usage":{"input_tokens":10,"output_tokens":5,` +
					`"server_tool_use":{"web_search_requests":0}}}}`,
				`{"type":"user","timestamp":"2026-07-30T10:00:04Z","uuid":"u2",` +
					`"parentUuid":"a1","message":{"content":[{"type":` +
					`"tool_result","tool_use_id":"toolu_r1","content":"file"}]},` +
					`"toolUseResult":{"type":"text","file":{"filePath":"/x"}}}`,
			},
			want: map[string]int{"a1": 0},
		},
		{
			name: "a fetch result is not a search",
			lines: []string{
				userLine,
				`{"type":"assistant","timestamp":"2026-07-30T10:00:01Z",` +
					`"uuid":"a1","parentUuid":"u1","message":{"id":"msg_1",` +
					`"model":"claude-sonnet-4-5","content":[{"type":"tool_use",` +
					`"id":"toolu_f1","name":"WebFetch","input":{"url":"u"}}],` +
					`"usage":{"input_tokens":10,"output_tokens":5,` +
					`"server_tool_use":{"web_search_requests":0,` +
					`"web_fetch_requests":1}}}}`,
				`{"type":"user","timestamp":"2026-07-30T10:00:04Z","uuid":"u2",` +
					`"parentUuid":"a1","message":{"content":[{"type":` +
					`"tool_result","tool_use_id":"toolu_f1","content":"page"}]},` +
					`"toolUseResult":{"bytes":10,"code":200,"url":"u"}}`,
			},
			want: map[string]int{"a1": 0},
		},
		{
			name: "a non-WebSearch tool reporting searchCount is ignored",
			lines: []string{
				userLine,
				`{"type":"assistant","timestamp":"2026-07-30T10:00:01Z",` +
					`"uuid":"a1","parentUuid":"u1","message":{"id":"msg_1",` +
					`"model":"claude-sonnet-4-5","content":[{"type":"tool_use",` +
					`"id":"toolu_m1","name":"mcp__search__query",` +
					`"input":{"query":"q"}}],"usage":{"input_tokens":10,` +
					`"output_tokens":5,"server_tool_use":` +
					`{"web_search_requests":0}}}}`,
				`{"type":"user","timestamp":"2026-07-30T10:00:04Z","uuid":"u2",` +
					`"parentUuid":"a1","message":{"content":[{"type":` +
					`"tool_result","tool_use_id":"toolu_m1","content":"hits"}]},` +
					`"toolUseResult":{"query":"q","searchCount":4}}`,
			},
			want: map[string]int{"a1": 0},
		},
		{
			name: "each issuing message keeps its own count",
			lines: []string{
				userLine,
				`{"type":"assistant","timestamp":"2026-07-30T10:00:01Z",` +
					`"uuid":"a1","parentUuid":"u1","message":{"id":"msg_1",` +
					`"model":"claude-sonnet-4-5","content":[{"type":"tool_use",` +
					`"id":"toolu_s1","name":"WebSearch","input":{"query":"a"}}],` +
					`"usage":{"input_tokens":10,"output_tokens":5,` +
					`"server_tool_use":{"web_search_requests":0}}}}`,
				`{"type":"user","timestamp":"2026-07-30T10:00:04Z","uuid":"u2",` +
					`"parentUuid":"a1","message":{"content":[{"type":` +
					`"tool_result","tool_use_id":"toolu_s1","content":"hits"}]},` +
					`"toolUseResult":{"query":"a","searchCount":1}}`,
				`{"type":"assistant","timestamp":"2026-07-30T10:00:06Z",` +
					`"uuid":"a2","parentUuid":"u2","message":{"id":"msg_2",` +
					`"model":"claude-sonnet-4-5","content":[{"type":"text",` +
					`"text":"done"}],"usage":{"input_tokens":20,` +
					`"output_tokens":6,"server_tool_use":` +
					`{"web_search_requests":0}}}}`,
			},
			want: map[string]int{"a1": 1, "a2": 0},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseClaudeWebSearchRequests(
				t, "websearch.jsonl", tt.lines)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestClaudeWebSearchAnnotationPreservesOtherUsageFields(t *testing.T) {
	lines := []string{
		`{"type":"user","timestamp":"2026-07-30T10:00:00Z","uuid":"u1",` +
			`"message":{"content":"find something"},"cwd":"/tmp"}`,
		`{"type":"assistant","timestamp":"2026-07-30T10:00:01Z","uuid":"a1",` +
			`"parentUuid":"u1","message":{"id":"msg_1",` +
			`"model":"claude-sonnet-4-5","content":[{"type":"tool_use",` +
			`"id":"toolu_s1","name":"WebSearch","input":{"query":"q"}}],` +
			`"usage":{"input_tokens":11,"cache_creation_input_tokens":22,` +
			`"cache_read_input_tokens":33,"output_tokens":44,` +
			`"server_tool_use":{"web_search_requests":0,` +
			`"web_fetch_requests":7},"service_tier":"standard"}}}`,
		`{"type":"user","timestamp":"2026-07-30T10:00:04Z","uuid":"u2",` +
			`"parentUuid":"a1","message":{"content":[{"type":"tool_result",` +
			`"tool_use_id":"toolu_s1","content":"hits"}]},` +
			`"toolUseResult":{"query":"q","searchCount":2}}`,
	}
	path := createTestFile(t, "websearch-fields.jsonl", strings.Join(lines, "\n"))
	results, err := parseClaudeSession(path, "proj", "local")
	require.NoError(t, err)
	require.NotEmpty(t, results)

	var usage string
	for _, msg := range results[0].Messages {
		if msg.SourceUUID == "a1" {
			usage = string(msg.TokenUsage)
		}
	}
	require.NotEmpty(t, usage)
	require.True(t, gjson.Valid(usage), "usage stays valid JSON: %s", usage)
	assert.Equal(t, int64(2),
		gjson.Get(usage, usageWebSearchRequestsPath).Int())
	assert.Equal(t, int64(11), gjson.Get(usage, "input_tokens").Int())
	assert.Equal(t, int64(22),
		gjson.Get(usage, "cache_creation_input_tokens").Int())
	assert.Equal(t, int64(33),
		gjson.Get(usage, "cache_read_input_tokens").Int())
	assert.Equal(t, int64(44), gjson.Get(usage, "output_tokens").Int())
	assert.Equal(t, int64(7),
		gjson.Get(usage, "server_tool_use.web_fetch_requests").Int())
	assert.Equal(t, "standard", gjson.Get(usage, "service_tier").String())
}

func TestSetUsageWebSearchRequests(t *testing.T) {
	tests := []struct {
		name  string
		usage string
		count int
		want  string
		ok    bool
	}{
		{
			name:  "replaces an existing zero counter",
			usage: `{"input_tokens":1,"server_tool_use":{"web_search_requests":0,"web_fetch_requests":2}}`,
			count: 3,
			want:  `{"input_tokens":1,"server_tool_use":{"web_search_requests":3,"web_fetch_requests":2}}`,
			ok:    true,
		},
		{
			name:  "adds the key to an existing server_tool_use object",
			usage: `{"input_tokens":1,"server_tool_use":{"web_fetch_requests":2}}`,
			count: 4,
			want:  `{"input_tokens":1,"server_tool_use":{"web_search_requests":4,"web_fetch_requests":2}}`,
			ok:    true,
		},
		{
			name:  "adds the key to an empty server_tool_use object",
			usage: `{"input_tokens":1,"server_tool_use":{}}`,
			count: 5,
			want:  `{"input_tokens":1,"server_tool_use":{"web_search_requests":5}}`,
			ok:    true,
		},
		{
			name:  "adds server_tool_use when absent",
			usage: `{"input_tokens":1,"output_tokens":2}`,
			count: 6,
			want:  `{"server_tool_use":{"web_search_requests":6},"input_tokens":1,"output_tokens":2}`,
			ok:    true,
		},
		{
			name:  "adds server_tool_use to an empty object",
			usage: `{}`,
			count: 7,
			want:  `{"server_tool_use":{"web_search_requests":7}}`,
			ok:    true,
		},
		{
			name:  "refuses a non-object blob",
			usage: `[1,2]`,
			count: 1,
			want:  `[1,2]`,
			ok:    false,
		},
		{
			name:  "refuses malformed JSON",
			usage: `{"input_tokens":`,
			count: 1,
			want:  `{"input_tokens":`,
			ok:    false,
		},
		{
			name:  "refuses a non-object server_tool_use",
			usage: `{"server_tool_use":5}`,
			count: 1,
			want:  `{"server_tool_use":5}`,
			ok:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := setUsageWebSearchRequests(tt.usage, tt.count)
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.want, got)
			if !tt.ok {
				return
			}
			require.True(t, gjson.Valid(got))
			assert.Equal(t, tt.count, usageWebSearchRequests(got))
		})
	}
}
