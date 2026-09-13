package rawtest

import "testing"

const parityPiebaldSchema = `CREATE TABLE projects (
			id INTEGER PRIMARY KEY,
			directory TEXT NOT NULL,
			name TEXT NOT NULL
		);
CREATE TABLE chats (
			id INTEGER PRIMARY KEY,
			title TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			is_deleted BOOLEAN NOT NULL DEFAULT 0,
			message_count INTEGER NOT NULL DEFAULT 0,
			current_directory TEXT,
			worktree_path TEXT,
			branch_name TEXT,
			project_id INTEGER
		);
CREATE TABLE messages (
			id INTEGER PRIMARY KEY,
			parent_chat_id INTEGER NOT NULL,
			parent_message_id INTEGER,
			role TEXT NOT NULL,
			model TEXT,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			input_tokens BIGINT,
			output_tokens BIGINT,
			reasoning_tokens BIGINT,
			cache_read_tokens BIGINT,
			cache_write_tokens BIGINT,
			status TEXT NOT NULL,
			finish_reason TEXT,
			error TEXT,
			enabled INTEGER NOT NULL DEFAULT 1
		);
CREATE TABLE message_parts (
			id INTEGER PRIMARY KEY,
			parent_chat_message_id INTEGER NOT NULL,
			part_index INTEGER NOT NULL,
			part_type TEXT NOT NULL
		);
CREATE TABLE message_part_text (
			message_part_id INTEGER PRIMARY KEY,
			is_thinking BOOLEAN NOT NULL DEFAULT FALSE
		);
CREATE TABLE message_content_nodes (
			id INTEGER PRIMARY KEY,
			parent_text_part_id INTEGER NOT NULL,
			node_index INTEGER NOT NULL,
			node_type TEXT NOT NULL
		);
CREATE TABLE message_node_text (
			node_id INTEGER PRIMARY KEY,
			content TEXT NOT NULL
		);
CREATE TABLE message_part_tool_call (
			message_part_id INTEGER PRIMARY KEY,
			provider_tool_use_id TEXT NOT NULL,
			tool_name TEXT NOT NULL,
			tool_input TEXT NOT NULL,
			tool_result TEXT,
			tool_error TEXT,
			tool_state TEXT NOT NULL DEFAULT 'pending',
			sub_agent_chat_id INTEGER
		);`

func PiebaldParity(t *testing.T, root string) ParityFixture {
	database := paritySQLite(t, root, "app.db", parityPiebaldSchema)
	paritySQL(t, database, `INSERT INTO chats(id,title,created_at,updated_at,message_count,current_directory) VALUES (42,'Piebald review','2026-07-06T12:00:00Z','2026-07-06T12:00:10Z',6,'/workspace/project'),(43,'Zero review','2026-07-06T12:00:00Z','2026-07-06T12:00:10Z',1,'/workspace/project');
 INSERT INTO messages(id,parent_chat_id,parent_message_id,role,model,created_at,updated_at,input_tokens,output_tokens,status,enabled) VALUES
 (100,42,NULL,'user',NULL,'2026-07-06T12:00:01Z','2026-07-06T12:00:01Z',NULL,NULL,'completed',1),
 (101,42,100,'assistant','gpt-5.4','2026-07-06T12:00:02Z','2026-07-06T12:00:02Z',100,7,'completed',1),
 (102,42,101,'user',NULL,'2026-07-06T12:00:03Z','2026-07-06T12:00:03Z',NULL,NULL,'completed',1),
 (103,42,102,'assistant','gpt-5.4','2026-07-06T12:00:04Z','2026-07-06T12:00:04Z',0,0,'completed',1),
 (200,42,101,'user',NULL,'2026-07-06T12:00:05Z','2026-07-06T12:00:05Z',NULL,NULL,'completed',0),
 (201,42,200,'assistant','gpt-5.4','2026-07-06T12:00:06Z','2026-07-06T12:00:06Z',20,3,'completed',1),
 (300,43,NULL,'assistant','gpt-5.4','2026-07-06T12:00:00Z','2026-07-06T12:00:00Z',0,0,'completed',1);
 INSERT INTO message_parts VALUES (500,101,1,'tool_call');
 INSERT INTO message_part_tool_call(message_part_id,provider_tool_use_id,tool_name,tool_input,tool_result,tool_state) VALUES(500,'read-1','Read','{"file_path":"file.go"}','package fixture','completed');`)
	for _, row := range []struct {
		id   int
		text string
	}{{100, "Inspect Piebald."}, {101, "Reading."}, {102, "Continue."}, {103, "Recorded."}, {200, "Inspect fork."}, {201, "Fork inspected."}, {300, "Zero usage."}} {
		paritySQL(t, database, `INSERT INTO message_parts VALUES(?,?,0,'text');`, row.id, row.id)
		paritySQL(t, database, `INSERT INTO message_part_text VALUES(?,0);`, row.id)
		paritySQL(t, database, `INSERT INTO message_content_nodes VALUES(?,?,0,'text');`, row.id, row.id)
		paritySQL(t, database, `INSERT INTO message_node_text VALUES(?,?);`, row.id, row.text)
	}
	return ParityFixture{root, []ParitySession{{ID: "piebald:42", MessageUsage: map[int]ParityMessageUsage{0: {}, 1: {Context: 100, Output: 7, Present: true}, 3: {Present: true}}, Title: "Piebald review", First: "Inspect Piebald.", Roles: []string{"user", "assistant", "user", "assistant"}, Output: 7, HasOutput: true, ToolName: "Read", ToolResult: "package fixture"}, {ID: "piebald:42-200", Parent: "piebald:42", First: "Inspect fork.", Roles: []string{"user", "assistant"}, Output: 3, HasOutput: true}, {ID: "piebald:43", Title: "Zero review", Roles: []string{"assistant"}, HasOutput: true}}}
}
