package parser

import (
	"database/sql"
	"path/filepath"
	"testing"
)

const forgeSchema = `
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

type ForgeSeeder struct {
	db *sql.DB
	t  *testing.T
}

func (s *ForgeSeeder) AddConversation(
	conversationID, title string,
	workspaceID int64,
	context, createdAt, updatedAt, metrics string,
) {
	s.t.Helper()
	_, err := s.db.Exec(
		`INSERT INTO conversations
		 (conversation_id, title, workspace_id, context, created_at, updated_at, metrics)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		conversationID, title, workspaceID, context, createdAt, updatedAt, metrics,
	)
	if err != nil {
		s.t.Fatalf("add conversation: %v", err)
	}
}

func newForgeTestDB(t *testing.T) (string, *ForgeSeeder, *sql.DB) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), ".forge.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if _, err := db.Exec(forgeSchema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	seeder := &ForgeSeeder{db: db, t: t}
	return dbPath, seeder, db
}

func seedForgeConversation(t *testing.T, seeder *ForgeSeeder) {
	t.Helper()
	context := `{
	  "conversation_id": "conv-001",
	  "messages": [
	    {
	      "message": {
	        "text": {
	          "role": "System",
	          "content": "<system_information>\n<current_working_directory>/home/mj/dev/projects/agentsview</current_working_directory>\n</system_information>",
	          "model": "gpt-5.4",
	          "timestamp": "2026-05-02T09:58:15.741021507Z"
	        }
	      },
	      "usage": {
	        "prompt_tokens": {"actual": 0},
	        "completion_tokens": {"actual": 0},
	        "cached_tokens": {"actual": 0}
	      }
	    },
	    {
	      "message": {
	        "text": {
	          "role": "User",
	          "content": "Please add Forge support.",
	          "raw_content": {"Text": "Please add Forge support."},
	          "model": "gpt-5.4",
	          "timestamp": "2026-05-02T09:58:16.000000000Z"
	        }
	      },
	      "usage": {
	        "prompt_tokens": {"actual": 100},
	        "completion_tokens": {"actual": 5},
	        "cached_tokens": {"actual": 20}
	      }
	    },
	    {
	      "message": {
	        "text": {
	          "role": "Assistant",
	          "content": "",
	          "tool_calls": [
	            {
	              "name": "read",
	              "call_id": "call_read_1",
	              "arguments": {"file_path": "/tmp/example.go", "show_line_numbers": true}
	            }
	          ],
	          "model": "gpt-5.4",
	          "reasoning_details": [
	            {"text": "Inspecting the code first."}
	          ],
	          "timestamp": "2026-05-02T09:58:17.000000000Z"
	        }
	      },
	      "usage": {
	        "prompt_tokens": {"actual": 120},
	        "completion_tokens": {"actual": 10},
	        "cached_tokens": {"actual": 30}
	      }
	    },
	    {
	      "message": {
	        "tool": {
	          "name": "read",
	          "call_id": "call_read_1",
	          "output": {
	            "is_error": false,
	            "values": [
	              {"text": "<file path=\"/tmp/example.go\">package main</file>"}
	            ]
	          }
	        }
	      }
	    },
	    {
	      "message": {
	        "text": {
	          "role": "Assistant",
	          "content": "Added Forge support.",
	          "raw_content": {"Text": "Added Forge support."},
	          "model": "gpt-5.4",
	          "timestamp": "2026-05-02T09:58:18.000000000Z"
	        }
	      },
	      "usage": {
	        "prompt_tokens": {"actual": 140},
	        "completion_tokens": {"actual": 40},
	        "cached_tokens": {"actual": 35}
	      }
	    }
	  ]
	}`
	metrics := `{"input_tokens":360,"output_tokens":55,"cached_input_tokens":85}`
	seeder.AddConversation(
		"conv-001",
		"Add Forge Support",
		123,
		context,
		"2026-05-02 09:58:15.741021507",
		"2026-05-02 10:00:16.848497543",
		metrics,
	)
}

func TestParseForgeDB_StandardConversation(t *testing.T) {
	dbPath, seeder, db := newForgeTestDB(t)
	defer db.Close()
	seedForgeConversation(t, seeder)

	sessions, err := ParseForgeDB(dbPath, "testmachine")
	if err != nil {
		t.Fatalf("ParseForgeDB: %v", err)
	}

	assertEq(t, "sessions len", len(sessions), 1)
	s := sessions[0]
	assertEq(t, "ID", s.Session.ID, "forge:conv-001")
	assertEq(t, "Agent", s.Session.Agent, AgentForge)
	assertEq(t, "Machine", s.Session.Machine, "testmachine")
	assertEq(t, "Project", s.Session.Project, "agentsview")
	assertEq(t, "DisplayName", s.Session.DisplayName, "Add Forge Support")
	assertEq(t, "UserMessageCount", s.Session.UserMessageCount, 1)
	assertEq(t, "FirstMessage", s.Session.FirstMessage, "Please add Forge support.")
	assertEq(t, "Cwd", s.Session.Cwd, "/home/mj/dev/projects/agentsview")
	assertEq(t, "File.Path", s.Session.File.Path, dbPath+"#conv-001")
	assertEq(t, "HasTotalOutputTokens", s.Session.HasTotalOutputTokens, true)
	assertEq(t, "TotalOutputTokens", s.Session.TotalOutputTokens, 55)
	assertEq(t, "HasPeakContextTokens", s.Session.HasPeakContextTokens, true)
	assertEq(t, "PeakContextTokens", s.Session.PeakContextTokens, 445)

	assertEq(t, "messages len", len(s.Messages), 4)
	assertEq(t, "tool result role", s.Messages[2].Role, RoleUser)
	assertEq(t, "assistant tool call count", len(s.Messages[1].ToolCalls), 1)
	assertEq(t, "assistant tool name", s.Messages[1].ToolCalls[0].ToolName, "read")
	assertEq(t, "assistant tool category", s.Messages[1].ToolCalls[0].Category, "Read")
	assertEq(t, "tool result count", len(s.Messages[2].ToolResults), 1)
	assertEq(t, "tool result id", s.Messages[2].ToolResults[0].ToolUseID, "call_read_1")
	assertEq(t, "final assistant content", s.Messages[3].Content, "Added Forge support.")
}

func TestParseForgeSession_SingleConversation(t *testing.T) {
	dbPath, seeder, db := newForgeTestDB(t)
	defer db.Close()
	seedForgeConversation(t, seeder)

	sess, msgs, err := ParseForgeSession(dbPath, "conv-001", "testmachine")
	if err != nil {
		t.Fatalf("ParseForgeSession: %v", err)
	}
	if sess == nil {
		t.Fatal("expected non-nil session")
	}

	assertEq(t, "ID", sess.ID, "forge:conv-001")
	assertEq(t, "Agent", sess.Agent, AgentForge)
	assertEq(t, "msgs len", len(msgs), 4)
	assertEq(t, "msgs[0].Role", msgs[0].Role, RoleUser)
	assertEq(t, "msgs[0].Content", msgs[0].Content, "Please add Forge support.")
	assertEq(t, "msgs[1].Role", msgs[1].Role, RoleAssistant)
	assertEq(t, "msgs[1].HasThinking", msgs[1].HasThinking, true)
	assertEq(t, "msgs[1].HasToolUse", msgs[1].HasToolUse, true)
}

func TestListForgeSessionMeta(t *testing.T) {
	dbPath, seeder, db := newForgeTestDB(t)
	defer db.Close()
	seedForgeConversation(t, seeder)

	metas, err := ListForgeSessionMeta(dbPath)
	if err != nil {
		t.Fatalf("ListForgeSessionMeta: %v", err)
	}

	assertEq(t, "metas len", len(metas), 1)
	assertEq(t, "SessionID", metas[0].SessionID, "conv-001")
	assertEq(t, "VirtualPath", metas[0].VirtualPath, dbPath+"#conv-001")
	if metas[0].FileMtime == 0 {
		t.Error("expected non-zero FileMtime")
	}
}

func TestCollectForgeToolCalls_TaskSubagentIDPrefixed(t *testing.T) {
	dbPath, seeder, db := newForgeTestDB(t)
	defer db.Close()

	context := `{
	  "messages": [
	    {
	      "message": {
	        "text": {
	          "role": "User",
	          "content": "Run a subtask.",
	          "timestamp": "2026-05-02T10:00:00Z"
	        }
	      }
	    },
	    {
	      "message": {
	        "text": {
	          "role": "Assistant",
	          "content": "",
	          "tool_calls": [
	            {
	              "name": "task",
	              "call_id": "call_task_1",
	              "arguments": {"session_id": "child-conv-001", "prompt": "do the thing"}
	            }
	          ],
	          "timestamp": "2026-05-02T10:00:01Z"
	        }
	      }
	    }
	  ]
	}`
	seeder.AddConversation(
		"parent-conv", "Parent", 1, context,
		"2026-05-02 10:00:00", "2026-05-02 10:00:01", "",
	)

	sess, msgs, err := ParseForgeSession(dbPath, "parent-conv", "m")
	if err != nil {
		t.Fatalf("ParseForgeSession: %v", err)
	}
	if sess == nil {
		t.Fatal("expected non-nil session")
	}
	if len(msgs) == 0 {
		t.Fatal("expected messages")
	}
	var taskCall *ParsedToolCall
	for i := range msgs {
		for j := range msgs[i].ToolCalls {
			if msgs[i].ToolCalls[j].ToolName == "task" {
				taskCall = &msgs[i].ToolCalls[j]
			}
		}
	}
	if taskCall == nil {
		t.Fatal("expected task tool call")
	}
	assertEq(t, "SubagentSessionID", taskCall.SubagentSessionID, "forge:child-conv-001")
}

func TestFindForgeDBPath(t *testing.T) {
	dir := t.TempDir()
	assertEq(t, "not found", FindForgeDBPath(dir), "")

	dbPath, _, db := newForgeTestDB(t)
	defer db.Close()
	assertEq(t, "found", FindForgeDBPath(filepath.Dir(dbPath)), dbPath)
}
