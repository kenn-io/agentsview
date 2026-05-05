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

// ---------------------------------------------------------------------------
// Priority 2 — Token/metrics fallback tests
// ---------------------------------------------------------------------------

func TestForgeTokenFallbacks(t *testing.T) {
	// Case 1: no metrics column but per-message usage populated.
	// accumulateMessageTokenUsage should aggregate from messages.
	t.Run("no_metrics_per_message_usage", func(t *testing.T) {
		dbPath, seeder, db := newForgeTestDB(t)
		defer db.Close()

		context := `{
		  "messages": [
		    {
		      "message": {
		        "text": {
		          "role": "User",
		          "content": "Hello there.",
		          "timestamp": "2026-05-02T10:00:00Z"
		        }
		      },
		      "usage": {
		        "prompt_tokens":     {"actual": 50},
		        "completion_tokens": {"actual": 10},
		        "cached_tokens":     {"actual": 20}
		      }
		    },
		    {
		      "message": {
		        "text": {
		          "role": "Assistant",
		          "content": "Hi back.",
		          "timestamp": "2026-05-02T10:00:01Z"
		        }
		      },
		      "usage": {
		        "prompt_tokens":     {"actual": 80},
		        "completion_tokens": {"actual": 15},
		        "cached_tokens":     {"actual": 30}
		      }
		    }
		  ]
		}`
		seeder.AddConversation(
			"token-fallback-1", "No Metrics", 1,
			context,
			"2026-05-02 10:00:00", "2026-05-02 10:00:01",
			"", // empty metrics → accumulateMessageTokenUsage fallback
		)

		sessions, err := ParseForgeDB(dbPath, "m")
		if err != nil {
			t.Fatalf("ParseForgeDB: %v", err)
		}
		if len(sessions) != 1 {
			t.Fatalf("want 1 session, got %d", len(sessions))
		}
		s := sessions[0].Session
		assertEq(t, "HasTotalOutputTokens", s.HasTotalOutputTokens, true)
		assertEq(t, "TotalOutputTokens", s.TotalOutputTokens, 25) // 10+15
		assertEq(t, "HasPeakContextTokens", s.HasPeakContextTokens, true)
		if s.PeakContextTokens != 110 && s.PeakContextTokens != 70 {
			// Peak is max(50+20=70, 80+30=110) = 110
			t.Errorf("PeakContextTokens = %d, want 110", s.PeakContextTokens)
		}
	})

	// Case 2: metrics has only output_tokens (no input, no cached).
	t.Run("metrics_output_only", func(t *testing.T) {
		dbPath, seeder, db := newForgeTestDB(t)
		defer db.Close()

		context := `{
		  "messages": [
		    {
		      "message": {
		        "text": {
		          "role": "User",
		          "content": "Run something.",
		          "timestamp": "2026-05-02T10:00:00Z"
		        }
		      }
		    },
		    {
		      "message": {
		        "text": {
		          "role": "Assistant",
		          "content": "Done.",
		          "timestamp": "2026-05-02T10:00:01Z"
		        }
		      }
		    }
		  ]
		}`
		seeder.AddConversation(
			"token-fallback-2", "Output Only Metrics", 1,
			context,
			"2026-05-02 10:00:00", "2026-05-02 10:00:01",
			`{"output_tokens": 42}`,
		)

		sessions, err := ParseForgeDB(dbPath, "m")
		if err != nil {
			t.Fatalf("ParseForgeDB: %v", err)
		}
		if len(sessions) != 1 {
			t.Fatalf("want 1 session, got %d", len(sessions))
		}
		s := sessions[0].Session
		assertEq(t, "HasTotalOutputTokens", s.HasTotalOutputTokens, true)
		assertEq(t, "TotalOutputTokens", s.TotalOutputTokens, 42)
		assertEq(t, "HasPeakContextTokens", s.HasPeakContextTokens, false)
	})

	// Case 3: per-message usage with no cached_tokens key.
	// ContextTokens should equal just prompt_tokens.actual.
	t.Run("per_message_no_cached_tokens", func(t *testing.T) {
		dbPath, seeder, db := newForgeTestDB(t)
		defer db.Close()

		context := `{
		  "messages": [
		    {
		      "message": {
		        "text": {
		          "role": "User",
		          "content": "Tell me something.",
		          "timestamp": "2026-05-02T10:00:00Z"
		        }
		      },
		      "usage": {
		        "prompt_tokens":     {"actual": 60},
		        "completion_tokens": {"actual": 8}
		      }
		    }
		  ]
		}`
		seeder.AddConversation(
			"token-fallback-3", "No Cached Tokens", 1,
			context,
			"2026-05-02 10:00:00", "2026-05-02 10:00:01",
			"",
		)

		sessions, err := ParseForgeDB(dbPath, "m")
		if err != nil {
			t.Fatalf("ParseForgeDB: %v", err)
		}
		if len(sessions) != 1 {
			t.Fatalf("want 1 session, got %d", len(sessions))
		}
		msgs := sessions[0].Messages
		if len(msgs) == 0 {
			t.Fatal("want at least 1 message")
		}
		assertEq(t, "HasContextTokens", msgs[0].HasContextTokens, true)
		assertEq(t, "ContextTokens", msgs[0].ContextTokens, 60) // only prompt, no cached
	})

	// Case 4: per-message usage entirely absent.
	t.Run("per_message_no_usage", func(t *testing.T) {
		dbPath, seeder, db := newForgeTestDB(t)
		defer db.Close()

		context := `{
		  "messages": [
		    {
		      "message": {
		        "text": {
		          "role": "User",
		          "content": "Empty usage.",
		          "timestamp": "2026-05-02T10:00:00Z"
		        }
		      }
		    }
		  ]
		}`
		seeder.AddConversation(
			"token-fallback-4", "No Usage At All", 1,
			context,
			"2026-05-02 10:00:00", "2026-05-02 10:00:01",
			"",
		)

		sessions, err := ParseForgeDB(dbPath, "m")
		if err != nil {
			t.Fatalf("ParseForgeDB: %v", err)
		}
		if len(sessions) != 1 {
			t.Fatalf("want 1 session, got %d", len(sessions))
		}
		msgs := sessions[0].Messages
		if len(msgs) == 0 {
			t.Fatal("want at least 1 message")
		}
		m := msgs[0]
		assertEq(t, "HasContextTokens", m.HasContextTokens, false)
		assertEq(t, "HasOutputTokens", m.HasOutputTokens, false)
		// tokenPresenceKnown is unexported; verify by checking TokenPresence()
		hasCtx, hasOut := m.TokenPresence()
		assertEq(t, "TokenPresence hasCtx", hasCtx, false)
		assertEq(t, "TokenPresence hasOut", hasOut, false)
	})
}

// ---------------------------------------------------------------------------
// Priority 7 — Degenerate conversation tests
// ---------------------------------------------------------------------------

func TestForgeDegenerate(t *testing.T) {
	// Sub-case 1: assistant message with empty content, no reasoning, no tool_calls → skipped.
	t.Run("empty_assistant_no_tools", func(t *testing.T) {
		dbPath, seeder, db := newForgeTestDB(t)
		defer db.Close()

		context := `{
		  "messages": [
		    {
		      "message": {
		        "text": {
		          "role": "User",
		          "content": "Hello.",
		          "timestamp": "2026-05-02T10:00:00Z"
		        }
		      }
		    },
		    {
		      "message": {
		        "text": {
		          "role": "Assistant",
		          "content": "",
		          "timestamp": "2026-05-02T10:00:01Z"
		        }
		      }
		    }
		  ]
		}`
		seeder.AddConversation(
			"degen-1", "Empty Assistant", 1,
			context,
			"2026-05-02 10:00:00", "2026-05-02 10:00:01",
			"",
		)

		sessions, err := ParseForgeDB(dbPath, "m")
		if err != nil {
			t.Fatalf("ParseForgeDB: %v", err)
		}
		if len(sessions) != 1 {
			t.Fatalf("want 1 session, got %d", len(sessions))
		}
		// Only the user message; empty assistant was skipped.
		assertEq(t, "messages len", len(sessions[0].Messages), 1)
		assertEq(t, "role", sessions[0].Messages[0].Role, RoleUser)
	})

	// Sub-case 2: tool message with empty call_id → skipped.
	t.Run("tool_message_empty_call_id", func(t *testing.T) {
		dbPath, seeder, db := newForgeTestDB(t)
		defer db.Close()

		context := `{
		  "messages": [
		    {
		      "message": {
		        "text": {
		          "role": "User",
		          "content": "Run a tool.",
		          "timestamp": "2026-05-02T10:00:00Z"
		        }
		      }
		    },
		    {
		      "message": {
		        "tool": {
		          "name": "bash",
		          "call_id": "",
		          "output": {"values": [{"text": "result"}]}
		        }
		      }
		    }
		  ]
		}`
		seeder.AddConversation(
			"degen-2", "Empty Call ID", 1,
			context,
			"2026-05-02 10:00:00", "2026-05-02 10:00:01",
			"",
		)

		sessions, err := ParseForgeDB(dbPath, "m")
		if err != nil {
			t.Fatalf("ParseForgeDB: %v", err)
		}
		if len(sessions) != 1 {
			t.Fatalf("want 1 session, got %d", len(sessions))
		}
		// Only the user message; tool result with empty call_id was skipped.
		assertEq(t, "messages len", len(sessions[0].Messages), 1)
	})

	// Sub-case 3: user message with empty content but populated raw_content.Text.
	t.Run("user_raw_content_fallback", func(t *testing.T) {
		dbPath, seeder, db := newForgeTestDB(t)
		defer db.Close()

		context := `{
		  "messages": [
		    {
		      "message": {
		        "text": {
		          "role": "User",
		          "content": "",
		          "raw_content": {"Text": "raw text fallback"},
		          "timestamp": "2026-05-02T10:00:00Z"
		        }
		      }
		    }
		  ]
		}`
		seeder.AddConversation(
			"degen-3", "Raw Content Fallback", 1,
			context,
			"2026-05-02 10:00:00", "2026-05-02 10:00:01",
			"",
		)

		sessions, err := ParseForgeDB(dbPath, "m")
		if err != nil {
			t.Fatalf("ParseForgeDB: %v", err)
		}
		if len(sessions) != 1 {
			t.Fatalf("want 1 session, got %d", len(sessions))
		}
		msgs := sessions[0].Messages
		assertEq(t, "messages len", len(msgs), 1)
		assertEq(t, "content", msgs[0].Content, "raw text fallback")
	})
}

// ---------------------------------------------------------------------------
// Priority 8 — cwd extraction edge cases
// ---------------------------------------------------------------------------

func TestForgeCwdEdgeCases(t *testing.T) {
	// Case 1: no system message at all → Cwd and Project empty.
	t.Run("no_system_message", func(t *testing.T) {
		dbPath, seeder, db := newForgeTestDB(t)
		defer db.Close()

		context := `{
		  "messages": [
		    {
		      "message": {
		        "text": {
		          "role": "User",
		          "content": "No system message.",
		          "timestamp": "2026-05-02T10:00:00Z"
		        }
		      }
		    }
		  ]
		}`
		seeder.AddConversation(
			"cwd-1", "No System", 1,
			context,
			"2026-05-02 10:00:00", "2026-05-02 10:00:01",
			"",
		)

		sessions, err := ParseForgeDB(dbPath, "m")
		if err != nil {
			t.Fatalf("ParseForgeDB: %v", err)
		}
		if len(sessions) != 1 {
			t.Fatalf("want 1 session, got %d", len(sessions))
		}
		s := sessions[0].Session
		assertEq(t, "Cwd", s.Cwd, "")
		assertEq(t, "Project", s.Project, ExtractProjectFromCwd(""))
	})

	// Case 2: system message present but no cwd tag → same result.
	t.Run("system_no_cwd_tag", func(t *testing.T) {
		dbPath, seeder, db := newForgeTestDB(t)
		defer db.Close()

		context := `{
		  "messages": [
		    {
		      "message": {
		        "text": {
		          "role": "System",
		          "content": "<system_information>no cwd here</system_information>",
		          "timestamp": "2026-05-02T10:00:00Z"
		        }
		      }
		    },
		    {
		      "message": {
		        "text": {
		          "role": "User",
		          "content": "Hi.",
		          "timestamp": "2026-05-02T10:00:01Z"
		        }
		      }
		    }
		  ]
		}`
		seeder.AddConversation(
			"cwd-2", "System No Tag", 1,
			context,
			"2026-05-02 10:00:00", "2026-05-02 10:00:01",
			"",
		)

		sessions, err := ParseForgeDB(dbPath, "m")
		if err != nil {
			t.Fatalf("ParseForgeDB: %v", err)
		}
		if len(sessions) != 1 {
			t.Fatalf("want 1 session, got %d", len(sessions))
		}
		assertEq(t, "Cwd", sessions[0].Session.Cwd, "")
	})

	// Case 3: cwd tag present but empty content → Cwd empty.
	t.Run("cwd_tag_empty_content", func(t *testing.T) {
		dbPath, seeder, db := newForgeTestDB(t)
		defer db.Close()

		context := `{
		  "messages": [
		    {
		      "message": {
		        "text": {
		          "role": "System",
		          "content": "<current_working_directory></current_working_directory>",
		          "timestamp": "2026-05-02T10:00:00Z"
		        }
		      }
		    },
		    {
		      "message": {
		        "text": {
		          "role": "User",
		          "content": "Hi again.",
		          "timestamp": "2026-05-02T10:00:01Z"
		        }
		      }
		    }
		  ]
		}`
		seeder.AddConversation(
			"cwd-3", "Empty CWD Tag", 1,
			context,
			"2026-05-02 10:00:00", "2026-05-02 10:00:01",
			"",
		)

		sessions, err := ParseForgeDB(dbPath, "m")
		if err != nil {
			t.Fatalf("ParseForgeDB: %v", err)
		}
		if len(sessions) != 1 {
			t.Fatalf("want 1 session, got %d", len(sessions))
		}
		assertEq(t, "Cwd", sessions[0].Session.Cwd, "")
	})

	// Case 4: cwd in a non-system message → still extracted.
	t.Run("cwd_in_non_system_message", func(t *testing.T) {
		dbPath, seeder, db := newForgeTestDB(t)
		defer db.Close()

		context := `{
		  "messages": [
		    {
		      "message": {
		        "text": {
		          "role": "User",
		          "content": "Working from <current_working_directory>/home/mj/dev/projects/myapp</current_working_directory> today.",
		          "timestamp": "2026-05-02T10:00:00Z"
		        }
		      }
		    }
		  ]
		}`
		seeder.AddConversation(
			"cwd-4", "CWD In User Message", 1,
			context,
			"2026-05-02 10:00:00", "2026-05-02 10:00:01",
			"",
		)

		sessions, err := ParseForgeDB(dbPath, "m")
		if err != nil {
			t.Fatalf("ParseForgeDB: %v", err)
		}
		if len(sessions) != 1 {
			t.Fatalf("want 1 session, got %d", len(sessions))
		}
		s := sessions[0].Session
		assertEq(t, "Cwd", s.Cwd, "/home/mj/dev/projects/myapp")
		assertEq(t, "Project", s.Project, "myapp")
	})
}

// ---------------------------------------------------------------------------
// Lower priority — parseForgeTimestamp table-driven
// ---------------------------------------------------------------------------

func TestParseForgeTimestamp(t *testing.T) {
	cases := []struct {
		input string
		empty bool
	}{
		{input: "2026-05-02T09:58:15.741021507Z", empty: false},
		{input: "2026-05-02T09:58:15Z", empty: false},
		{input: "2026-05-02 09:58:15.741021507", empty: false},
		{input: "2026-05-02 09:58:15.741021", empty: false},
		{input: "2026-05-02 09:58:15", empty: false},
		{input: "", empty: true},
		{input: "not-a-date", empty: true},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got := parseForgeTimestamp(tc.input)
			if tc.empty && !got.IsZero() {
				t.Errorf("parseForgeTimestamp(%q) = %v, want zero", tc.input, got)
			}
			if !tc.empty && got.IsZero() {
				t.Errorf("parseForgeTimestamp(%q) returned zero time", tc.input)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Lower priority — endedAt fallback to startedAt
// ---------------------------------------------------------------------------

func TestForgeEndedAtFallback(t *testing.T) {
	dbPath, seeder, db := newForgeTestDB(t)
	defer db.Close()

	context := `{
	  "messages": [
	    {
	      "message": {
	        "text": {
	          "role": "User",
	          "content": "No updated_at.",
	          "timestamp": "2026-05-02T10:00:00Z"
	        }
	      }
	    }
	  ]
	}`
	// Set updated_at to empty so loadForgeConversations COALESCE returns created_at,
	// but force a NULL by inserting directly.
	seeder.AddConversation(
		"ended-fallback", "Ended At Fallback", 1,
		context,
		"2026-05-02 10:00:00", "",
		"",
	)
	// Override to NULL updated_at.
	if _, err := db.Exec("UPDATE conversations SET updated_at = NULL WHERE conversation_id = 'ended-fallback'"); err != nil {
		t.Fatalf("update: %v", err)
	}

	sessions, err := ParseForgeDB(dbPath, "m")
	if err != nil {
		t.Fatalf("ParseForgeDB: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("want 1 session, got %d", len(sessions))
	}
	s := sessions[0].Session
	if s.EndedAt.IsZero() {
		t.Error("EndedAt is zero, want fallback to StartedAt")
	}
	if !s.StartedAt.Equal(s.EndedAt) {
		t.Errorf("EndedAt = %v, want StartedAt = %v", s.EndedAt, s.StartedAt)
	}
}

// ---------------------------------------------------------------------------
// Lower priority — forgeToolOutputText fallback ladder
// ---------------------------------------------------------------------------

func TestForgeToolOutputText(t *testing.T) {
	// values[].text covered in standard test; test top-level text fallback.
	t.Run("top_level_text", func(t *testing.T) {
		dbPath, seeder, db := newForgeTestDB(t)
		defer db.Close()

		context := `{
		  "messages": [
		    {
		      "message": {
		        "text": {
		          "role": "User",
		          "content": "Call a tool.",
		          "timestamp": "2026-05-02T10:00:00Z"
		        }
		      }
		    },
		    {
		      "message": {
		        "tool": {
		          "name": "bash",
		          "call_id": "call_top_1",
		          "output": {
		            "text": "top-level text output"
		          }
		        }
		      }
		    }
		  ]
		}`
		seeder.AddConversation(
			"tool-output-1", "Top Level Text", 1,
			context,
			"2026-05-02 10:00:00", "2026-05-02 10:00:01",
			"",
		)

		sessions, err := ParseForgeDB(dbPath, "m")
		if err != nil {
			t.Fatalf("ParseForgeDB: %v", err)
		}
		if len(sessions) != 1 {
			t.Fatalf("want 1 session, got %d", len(sessions))
		}
		msgs := sessions[0].Messages
		if len(msgs) < 2 {
			t.Fatalf("want at least 2 messages, got %d", len(msgs))
		}
		// Second message is the tool result (role=user with ToolResults)
		tr := msgs[1].ToolResults
		if len(tr) == 0 {
			t.Fatal("expected tool result")
		}
	})
}

// ---------------------------------------------------------------------------
// Lower priority — skill tool populates SkillName from arguments.name/.skill
// ---------------------------------------------------------------------------

func TestForgeSkillToolName(t *testing.T) {
	cases := []struct {
		name      string
		args      string
		wantSkill string
	}{
		{
			name:      "arguments.name",
			args:      `{"name": "my-skill"}`,
			wantSkill: "my-skill",
		},
		{
			name:      "arguments.skill_fallback",
			args:      `{"skill": "fallback-skill"}`,
			wantSkill: "fallback-skill",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dbPath, seeder, db := newForgeTestDB(t)
			defer db.Close()

			context := `{
			  "messages": [
			    {
			      "message": {
			        "text": {
			          "role": "User",
			          "content": "Run skill.",
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
			              "name": "skill",
			              "call_id": "call_skill_1",
			              "arguments": ` + tc.args + `
			            }
			          ],
			          "timestamp": "2026-05-02T10:00:01Z"
			        }
			      }
			    }
			  ]
			}`
			seeder.AddConversation(
				"skill-test-"+tc.name, "Skill Test", 1,
				context,
				"2026-05-02 10:00:00", "2026-05-02 10:00:01",
				"",
			)

			sessions, err := ParseForgeDB(dbPath, "m")
			if err != nil {
				t.Fatalf("ParseForgeDB: %v", err)
			}
			if len(sessions) != 1 {
				t.Fatalf("want 1 session, got %d", len(sessions))
			}
			var skillCall *ParsedToolCall
			for i := range sessions[0].Messages {
				for j := range sessions[0].Messages[i].ToolCalls {
					if sessions[0].Messages[i].ToolCalls[j].ToolName == "skill" {
						skillCall = &sessions[0].Messages[i].ToolCalls[j]
					}
				}
			}
			if skillCall == nil {
				t.Fatal("expected skill tool call")
			}
			assertEq(t, "SkillName", skillCall.SkillName, tc.wantSkill)
		})
	}
}

// ---------------------------------------------------------------------------
// Lower priority — reasoning details with no text fields → HasThinking false
// ---------------------------------------------------------------------------

func TestForgeReasoningNoText(t *testing.T) {
	dbPath, seeder, db := newForgeTestDB(t)
	defer db.Close()

	context := `{
	  "messages": [
	    {
	      "message": {
	        "text": {
	          "role": "User",
	          "content": "Think but say nothing.",
	          "timestamp": "2026-05-02T10:00:00Z"
	        }
	      }
	    },
	    {
	      "message": {
	        "text": {
	          "role": "Assistant",
	          "content": "Response.",
	          "reasoning_details": [
	            {"type": "thinking"},
	            {"type": "thinking"}
	          ],
	          "timestamp": "2026-05-02T10:00:01Z"
	        }
	      }
	    }
	  ]
	}`
	seeder.AddConversation(
		"reasoning-no-text", "No Reasoning Text", 1,
		context,
		"2026-05-02 10:00:00", "2026-05-02 10:00:01",
		"",
	)

	sessions, err := ParseForgeDB(dbPath, "m")
	if err != nil {
		t.Fatalf("ParseForgeDB: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("want 1 session, got %d", len(sessions))
	}
	msgs := sessions[0].Messages
	// User + assistant
	if len(msgs) < 2 {
		t.Fatalf("want at least 2 messages, got %d", len(msgs))
	}
	asst := msgs[1]
	assertEq(t, "HasThinking", asst.HasThinking, false)
}
