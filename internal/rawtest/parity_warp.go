package rawtest

import "testing"

const parityWarpSchema = `
CREATE TABLE agent_conversations (
    id INTEGER PRIMARY KEY NOT NULL,
    conversation_id TEXT NOT NULL,
    conversation_data TEXT NOT NULL,
    last_modified_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE UNIQUE INDEX ux_agent_conversations_conversation_id
    ON agent_conversations (conversation_id);

CREATE TABLE ai_queries (
    id INTEGER PRIMARY KEY NOT NULL,
    exchange_id TEXT NOT NULL,
    conversation_id TEXT NOT NULL,
    start_ts DATETIME NOT NULL,
    input TEXT NOT NULL,
    working_directory TEXT,
    output_status TEXT NOT NULL,
    model_id TEXT NOT NULL DEFAULT '',
    planning_model_id TEXT NOT NULL DEFAULT '',
    coding_model_id TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX ux_ai_queries_exchange_id
    ON ai_queries(exchange_id);
`

func WarpParity(t *testing.T, root string) ParityFixture {
	database := paritySQLite(t, root, "warp.sqlite", parityWarpSchema)
	paritySQL(t, database, `INSERT INTO agent_conversations(conversation_id,conversation_data,last_modified_at) VALUES
 ('primary','{"conversation_usage_metadata":{"token_usage":[{"model_id":"gpt-5.4","warp_tokens":17,"byok_tokens":0}],"tool_usage_metadata":{"run_command_stats":{"count":1,"commands_executed":1}}}}','2026-07-06 12:00:10'),('empty','null','2026-07-06 12:00:10');
 INSERT INTO ai_queries(exchange_id,conversation_id,start_ts,input,working_directory,output_status,model_id) VALUES
 ('q1','primary','2026-07-06 12:00:00','[{"Query":{"text":"Inspect Warp.","context":[]}}]','/workspace/project','"Completed"','gpt-5.4'),
 ('q2','empty','2026-07-06 12:00:00','[{"Query":{"text":"No usage available.","context":[]}}]','/workspace/project','"Completed"','gpt-5.4');`)
	return ParityFixture{root, []ParitySession{{ID: "warp:primary", First: "Inspect Warp.", Roles: []string{"user", "assistant"}, Output: 17, HasOutput: true}, {ID: "warp:empty", First: "No usage available.", Roles: []string{"user"}}}}
}
