package rawtest

import "testing"

const parityGooseSchema = `
	CREATE TABLE schema_version (
		version INTEGER PRIMARY KEY,
		applied_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);
	INSERT INTO schema_version (version) VALUES (15);
	CREATE TABLE sessions (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL DEFAULT '',
		description TEXT NOT NULL DEFAULT '',
		user_set_name BOOLEAN DEFAULT FALSE,
		session_type TEXT NOT NULL DEFAULT 'user',
		working_dir TEXT NOT NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		extension_data TEXT DEFAULT '{}',
		total_tokens INTEGER,
		input_tokens INTEGER,
		output_tokens INTEGER,
		cache_read_tokens INTEGER,
		cache_write_tokens INTEGER,
		accumulated_total_tokens INTEGER,
		accumulated_input_tokens INTEGER,
		accumulated_output_tokens INTEGER,
		accumulated_cache_read_tokens INTEGER,
		accumulated_cache_write_tokens INTEGER,
		accumulated_cost REAL,
		schedule_id TEXT,
		recipe_json TEXT,
		user_recipe_values_json TEXT,
		provider_name TEXT,
		model_config_json TEXT,
		goose_mode TEXT NOT NULL DEFAULT 'auto',
		archived_at TIMESTAMP,
		project_id TEXT,
		parent_session_id TEXT
	);
	CREATE TABLE messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		message_id TEXT,
		session_id TEXT NOT NULL REFERENCES sessions(id),
		role TEXT NOT NULL,
		content_json TEXT NOT NULL,
		created_timestamp INTEGER NOT NULL,
		timestamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		tokens INTEGER,
		metadata_json TEXT
	);
	CREATE INDEX idx_messages_session ON messages(session_id);
	CREATE TABLE usage_ledger (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
		created_timestamp INTEGER NOT NULL,
		model TEXT,
		input_tokens INTEGER,
		output_tokens INTEGER,
		total_tokens INTEGER,
		cache_read_tokens INTEGER,
		cache_write_tokens INTEGER,
		cost REAL,
		cost_source TEXT,
		is_compaction INTEGER DEFAULT 0
	);
	CREATE INDEX idx_usage_ledger_session ON usage_ledger(session_id);
`

func GooseParity(t *testing.T, root string) ParityFixture {
	database := paritySQLite(t, root, "sessions.db", parityGooseSchema)
	paritySQL(t, database, `INSERT INTO sessions (id,name,working_dir,created_at,updated_at,session_type,parent_session_id,provider_name,model_config_json) VALUES
 ('primary','Goose review','/workspace/project','2026-07-06 12:00:00','2026-07-06 12:00:10','sub_agent','billing','synthetic','{"model_name":"gpt-5.4"}'),
 ('billing','Billing only','/workspace/project','2026-07-06 12:00:00','2026-07-06 12:00:10','user',NULL,'synthetic','{"model_name":"gpt-5.4"}');
 INSERT INTO messages(session_id,role,content_json,created_timestamp) VALUES
 ('primary','user','[{"type":"text","text":"Inspect Goose."}]',1783339200),
 ('primary','assistant','[{"type":"toolRequest","id":"read-1","toolCall":{"status":"success","value":{"name":"Read","arguments":{"file_path":"file.go"}}}}]',1783339201),
 ('primary','user','[{"type":"toolResponse","id":"read-1","toolResult":{"status":"success","value":{"content":[{"type":"text","text":"package fixture"}]}}}]',1783339202);
 INSERT INTO usage_ledger(session_id,created_timestamp,model,input_tokens,output_tokens,total_tokens,cost,cost_source) VALUES
 ('primary',1783339203,'gpt-5.4',100,7,107,NULL,NULL),('primary',1783339204,'gpt-5.4',0,0,0,0,'provider_reported'),('billing',1783339203,'gpt-5.4',200,17,217,0.01,'provider_reported');`)
	return ParityFixture{root, []ParitySession{{ID: "goose:primary", Usage: []ParityUsage{{Input: 100, Output: 7}, {Cost: new(int64(0))}}, Title: "Goose review", Parent: "goose:billing", First: "Inspect Goose.", Roles: []string{"user", "assistant"}, Output: 7, HasOutput: true, ToolName: "Read", ToolResult: "package fixture"}, {ID: "goose:billing", Usage: []ParityUsage{{Input: 200, Output: 17, Cost: new(int64(10000))}}, Title: "Billing only", Output: 17, HasOutput: true}}}
}
