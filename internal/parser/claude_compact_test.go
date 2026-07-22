package parser

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCompactClaudeEntry(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
	}{
		{
			name: "assistant entry keeps whitelisted fields in fixed order",
			line: `{"type":"assistant","uuid":"u1","parentUuid":"p1",` +
				`"timestamp":"2026-01-02T03:04:05Z","cwd":"/tmp/x",` +
				`"message":{"model":"m1","content":[{"type":"text","text":"hi"}],` +
				`"id":"msg1","usage":{"input_tokens":1},"stop_reason":"end_turn"}}`,
			want: `{"uuid":"u1","parentUuid":"p1",` +
				`"timestamp":"2026-01-02T03:04:05Z",` +
				`"message":{"content":[{"type":"text","text":"hi"}],` +
				`"id":"msg1","stop_reason":"end_turn","model":"m1",` +
				`"usage":{"input_tokens":1}}}`,
		},
		{
			name: "missing groups and fields are omitted",
			line: `{"uuid":"u2","sessionId":"s","gitBranch":"main"}`,
			want: `{"uuid":"u2"}`,
		},
		{
			name: "empty entry",
			line: `{}`,
			want: `{}`,
		},
		{
			name: "null values are preserved",
			line: `{"parentUuid":null,"isSidechain":false,"isMeta":true}`,
			want: `{"parentUuid":null,"isSidechain":false,"isMeta":true}`,
		},
		{
			name: "duplicate keys keep the first occurrence",
			line: `{"uuid":"first","uuid":"second",` +
				`"message":{"id":"m-first"},"message":{"id":"m-second"}}`,
			want: `{"uuid":"first","message":{"id":"m-first"}}`,
		},
		{
			name: "duplicate keys inside a group keep the first occurrence",
			line: `{"message":{"model":"a","model":"b"}}`,
			want: `{"message":{"model":"a"}}`,
		},
		{
			name: "non-object message is dropped",
			line: `{"uuid":"u3","message":"oops"}`,
			want: `{"uuid":"u3"}`,
		},
		{
			name: "snapshot and toolUseResult groups",
			line: `{"uuid":"u4","snapshot":{"timestamp":"t1","extra":1},` +
				`"toolUseResult":{"agentId":"a1",` +
				`"persistedOutputPath":"/p","stdout":"noise"}}`,
			want: `{"uuid":"u4","snapshot":{"timestamp":"t1"},` +
				`"toolUseResult":{"agentId":"a1",` +
				`"persistedOutputPath":"/p"}}`,
		},
		{
			name: "requestId and compact summary flags",
			line: `{"requestId":"req_1","isCompactSummary":true,` +
				`"isSidechain":true}`,
			want: `{"isCompactSummary":true,"isSidechain":true,` +
				`"requestId":"req_1"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, compactClaudeEntry([]byte(tt.line)))
		})
	}
}

// compactClaudeEntry runs once per retained message during a full
// parse, so its per-call cost is a direct multiplier on archive-scale
// sync CPU. Keep it single-pass; see the benchmark before adding
// per-field gjson lookups.
func BenchmarkCompactClaudeEntry(b *testing.B) {
	line := []byte(`{"type":"assistant","uuid":"3f2a0d2e-1111-4222-8333-444455556666",` +
		`"parentUuid":"9a8b7c6d-1111-4222-8333-444455556666",` +
		`"timestamp":"2026-01-02T03:04:05.678Z","sessionId":"sess-1",` +
		`"cwd":"/Users/test/code/project","gitBranch":"feature/x",` +
		`"version":"2.1.0","requestId":"req_011CRnStkoAQ",` +
		`"message":{"model":"claude-sonnet-4-20250514","id":"msg_01ABC",` +
		`"type":"message","role":"assistant",` +
		`"content":[{"type":"text","text":"Let me look at the sync engine and ` +
		`figure out why the batch writer retries are not sharing one code path."},` +
		`{"type":"tool_use","id":"toolu_42","name":"Read",` +
		`"input":{"file_path":"internal/sync/engine.go"}}],` +
		`"stop_reason":"tool_use","usage":{"input_tokens":1200,"output_tokens":340,` +
		`"cache_creation_input_tokens":90,"cache_read_input_tokens":15000}}}`)
	b.SetBytes(int64(len(line)))
	for b.Loop() {
		_ = compactClaudeEntry(line)
	}
}
