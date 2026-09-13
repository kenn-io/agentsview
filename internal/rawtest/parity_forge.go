package rawtest

import "testing"

const parityForgeSchema = `
CREATE TABLE conversations (
    conversation_id TEXT PRIMARY KEY NOT NULL,
    title TEXT,
    workspace_id BIGINT NOT NULL,
    context TEXT,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP,
    metrics TEXT
);
CREATE INDEX idx_conversations_workspace_created ON conversations(workspace_id, created_at DESC);
CREATE INDEX idx_conversations_active_workspace_updated
ON conversations(workspace_id, updated_at DESC)
WHERE context IS NOT NULL;
`

func ForgeParity(t *testing.T, root string) ParityFixture {
	database := paritySQLite(t, root, ".forge.db", parityForgeSchema)
	paritySQL(t, database, `INSERT INTO conversations VALUES ('primary','Forge review',1,?, '2026-07-06 12:00:00','2026-07-06 12:00:10',NULL),('zero','Zero review',1,?,'2026-07-06 12:00:00','2026-07-06 12:00:10','{"input_tokens":0,"output_tokens":0}');`,
		`{"messages":[{"message":{"text":{"role":"User","timestamp":"2026-07-06T12:00:00Z","content":"Inspect Forge."}}},{"message":{"text":{"role":"Assistant","timestamp":"2026-07-06T12:00:01Z","content":"Inspection complete.","model":"gpt-5.4","tool_calls":[{"name":"read","call_id":"read-1","arguments":{"file_path":"file.go"}}]}},"usage":{"prompt_tokens":{"actual":100},"completion_tokens":{"actual":7}}},{"message":{"tool":{"name":"read","call_id":"read-1","output":{"is_error":false,"values":[{"text":"package fixture"}]}}}}]}`,
		`{"messages":[{"message":{"text":{"role":"User","timestamp":"2026-07-06T12:00:00Z","content":"Zero usage."}}},{"message":{"text":{"role":"Assistant","timestamp":"2026-07-06T12:00:01Z","content":"Recorded."}}}]}`)
	return ParityFixture{root, []ParitySession{{ID: "forge:primary", Title: "Forge review", First: "Inspect Forge.", Roles: []string{"user", "assistant"}, Output: 7, HasOutput: true, ToolName: "read", ToolResult: "package fixture"}, {ID: "forge:zero", Title: "Zero review", First: "Zero usage.", Roles: []string{"user", "assistant"}, HasOutput: true}}}
}
