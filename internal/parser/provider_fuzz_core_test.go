package parser

import "testing"

func init() {
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentClaude,
		path:  "-home-user-code-app/0e4ba1a2-6f43-4c47-9c35-4d1f2c3b8a10.jsonl",
		seeds: []providerFuzzSeed{
			{file: "claude/valid_session.jsonl", want: "Fix the login bug"},
			{file: "claude/tool_call_pending.jsonl", want: "Read auth.ts"},
		},
	})
	registerProviderFuzzTarget(providerFuzzTarget{
		agent: AgentCodex,
		path:  "2024/01/01/rollout-2024-01-01T10-00-00-0e4ba1a2-6f43-4c47-9c35-4d1f2c3b8a10.jsonl",
		seeds: []providerFuzzSeed{
			{file: "codex/standard_session.jsonl", want: "Add rate limiting"},
			{file: "codex/function_calls.jsonl", want: "Run the tests"},
		},
	})
}

func FuzzProviderClaude(f *testing.F) { fuzzProvider(f, AgentClaude) }

func FuzzProviderCodex(f *testing.F) { fuzzProvider(f, AgentCodex) }
