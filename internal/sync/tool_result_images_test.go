package sync

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestDropPolicyNoResyncChurn(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	database.SetToolResultImages(config.ToolResultImagesDrop)
	require.NoError(t, database.UpsertSession(db.Session{
		ID: "resync", Project: "project", Machine: "local", Agent: "codex",
	}))
	messages := []db.Message{{
		SessionID: "resync", Ordinal: 0, Role: "assistant", Content: "answer",
		ToolCalls: []db.ToolCall{{
			ToolUseID: "call", ResultContent: `[{"type":"input_image","image_url":"data:image/png;base64,AAEC"}]`,
			ResultEvents: []db.ToolResultEvent{{
				ToolUseID: "call", Source: "tool", Status: "completed",
				Content: `[{"type":"input_image","image_url":"data:image/png;base64,AAEC"}]`,
			}},
		}},
	}}
	require.NoError(t, database.ReplaceSessionMessages("resync", messages))

	engine := NewEngine(database, EngineConfig{})
	projected, _ := database.ProjectToolResultImages(messages)
	assert.NotContains(t, projected[0].ToolCalls[0].ResultContent, "input_image")
	assert.NotContains(t, projected[0].ToolCalls[0].ResultEvents[0].Content, "input_image")

	var before string
	require.NoError(t, database.Reader().QueryRowContext(context.Background(),
		"SELECT transcript_revision FROM sessions WHERE id = ?", "resync").Scan(&before))
	require.NoError(t, engine.db.ReplaceSessionMessages("resync", projected))
	var after string
	require.NoError(t, database.Reader().QueryRowContext(context.Background(),
		"SELECT transcript_revision FROM sessions WHERE id = ?", "resync").Scan(&after))
	assert.Equal(t, before, after)
}

func TestPrepareSessionWritePreservesDecodedToolResults(t *testing.T) {
	content := `[{"type":"text","text":"before"},{"type":"input_image","image_url":"data:image/png;base64,AAEC"},{"type":"text","text":"after"}]`
	for _, tt := range []struct {
		name   string
		policy config.ToolResultImages
	}{
		{name: "keep", policy: config.ToolResultImagesKeep},
		{name: "drop", policy: config.ToolResultImagesDrop},
	} {
		t.Run(tt.name, func(t *testing.T) {
			database := dbtest.OpenTestDB(t)
			database.SetToolResultImages(tt.policy)
			engine := NewEngine(database, EngineConfig{})
			t.Cleanup(engine.Close)

			_, messages, verdict := engine.prepareSessionWrite(pendingWrite{
				sess: parser.ParsedSession{
					ID: "prepared-" + tt.name, Project: "project", Machine: "local",
					Agent: parser.AgentCodex, StartedAt: time.Unix(1, 0),
					EndedAt: time.Unix(2, 0),
					File:    parser.FileInfo{Path: "session.jsonl"},
				},
				msgs: []parser.ParsedMessage{
					{
						Ordinal: 0, Role: parser.RoleAssistant, Content: "answer",
						ToolCalls: []parser.ParsedToolCall{{
							ToolUseID: "call-1", ToolName: "Bash", Category: "Bash",
							ResultEvents: []parser.ParsedToolResultEvent{{
								ToolUseID: "call-1", AgentID: "agent-1",
								Source: "tool", Status: "completed", Content: "event summary",
							}},
						}},
					},
					{
						Ordinal: 1, Role: parser.RoleUser,
						ToolResults: []parser.ParsedToolResult{{
							ToolUseID: "call-1", ContentLength: len(content), ContentRaw: content,
						}},
					},
				},
			}, nil)
			require.Equal(t, sessionWriteOK, verdict)
			require.Len(t, messages, 1)
			require.Len(t, messages[0].ToolCalls, 1)
			call := messages[0].ToolCalls[0]
			assert.Equal(t, "event summary", call.ResultContent)
			assert.Equal(t, "Bash", call.ToolName)
			assert.Equal(t, "Bash", call.Category)
		})
	}
}

func TestIncrementalSubagentLinksPreserveDecodedToolResults(t *testing.T) {
	content := `[{"type":"text","text":"before"},{"type":"input_image","image_url":"data:image/png;base64,AAEC"},{"type":"text","text":"after"}]`
	for _, tt := range []struct {
		name   string
		policy config.ToolResultImages
	}{
		{name: "keep", policy: config.ToolResultImagesKeep},
		{name: "drop", policy: config.ToolResultImagesDrop},
	} {
		t.Run(tt.name, func(t *testing.T) {
			database := dbtest.OpenTestDB(t)
			database.SetToolResultImages(tt.policy)
			require.NoError(t, database.UpsertSession(db.Session{
				ID: "incremental-link-" + tt.name, Agent: string(parser.AgentClaude),
				Project: "project", Machine: "local", MessageCount: 1,
			}))
			require.NoError(t, database.InsertMessages([]db.Message{{
				SessionID: "incremental-link-" + tt.name, Ordinal: 0,
				Role: "assistant", ToolCalls: []db.ToolCall{{
					ToolUseID: "call-1", ToolName: "Task", Category: "Task",
				}},
			}}))

			engine := NewEngine(database, EngineConfig{Machine: "local"})
			t.Cleanup(engine.Close)
			require.NoError(t, engine.writeIncremental(&incrementalUpdate{
				sessionID: "incremental-link-" + tt.name,
				machine:   "local", project: "project", msgCount: 1,
				links: []parser.ClaudeSubagentLink{{
					ToolUseID: "call-1", ResultContentRaw: content,
					ResultContentLen: len(content), HasResult: true,
				}},
			}))

			messages, err := database.GetAllMessages(
				context.Background(), "incremental-link-"+tt.name,
			)
			require.NoError(t, err)
			require.Len(t, messages, 1)
			result := messages[0].ToolCalls[0].ResultContent
			assert.Equal(t, "beforeafter", result)
		})
	}
}

func TestEngineImagePolicyOverridesDatabaseForBulkAppendAndLink(t *testing.T) {
	const raw = `[ {"type":"text","text":"before"},{"type":"input_image","image_url":"data:image/png;base64,AAEC"},{"type":"text","text":"after"} ]`
	const want = `[{"type":"text","text":"before"},{"byte_size":3,"media_type":"image/png","sha256":"","text":"[Image: image/png, 3 bytes]","type":"agentsview_image","version":1},{"type":"text","text":"after"}]`
	const linkWant = "beforeafter"

	database := dbtest.OpenTestDB(t)
	database.SetToolResultImages(config.ToolResultImagesKeep)
	engine := NewEngine(database, EngineConfig{
		Machine:          "local",
		ToolResultImages: config.ToolResultImagesDrop,
	})
	t.Cleanup(engine.Close)

	bulkID := "engine-policy-bulk"
	bulk := engine.writeBatchBulkWithOutcome([]pendingWrite{{
		sess: parser.ParsedSession{
			ID: bulkID, Project: "project", Machine: "local",
			Agent: parser.AgentClaude, StartedAt: time.Unix(1, 0),
		},
		msgs: []parser.ParsedMessage{{
			Ordinal: 0, Role: parser.RoleAssistant, Content: "answer",
			ToolCalls: []parser.ParsedToolCall{{
				ToolUseID: "bulk-call", ToolName: "Bash", Category: "Bash",
				ResultEvents: []parser.ParsedToolResultEvent{{
					ToolUseID: "bulk-call", Source: "tool", Status: "completed",
					Content: raw,
				}},
			}},
		}},
	}}, true)
	require.Equal(t, 1, bulk.writtenSessions)
	require.Equal(t, 0, bulk.failedSessions)

	messages, err := database.GetAllMessages(t.Context(), bulkID)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	require.Len(t, messages[0].ToolCalls, 1)
	assert.Equal(t, want, messages[0].ToolCalls[0].ResultContent)
	assert.Equal(t, len(want), messages[0].ToolCalls[0].ResultContentLength)

	appendMessages := append([]db.Message(nil), messages...)
	appendMessages = append(appendMessages, db.Message{
		SessionID: bulkID, Ordinal: 1, Role: "assistant",
		ToolCalls: []db.ToolCall{{
			ToolUseID: "append-call", ResultContent: raw,
			ResultEvents: []db.ToolResultEvent{{
				ToolUseID: "append-call", Source: "tool", Status: "completed",
				Content: raw,
			}},
		}},
	})
	require.NoError(t, engine.writeMessages(bulkID, appendMessages))

	messages, err = database.GetAllMessages(t.Context(), bulkID)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, want, messages[1].ToolCalls[0].ResultContent)
	assert.Equal(t, len(want), messages[1].ToolCalls[0].ResultContentLength)

	linkID := "engine-policy-link"
	require.NoError(t, database.UpsertSession(db.Session{
		ID: linkID, Agent: string(parser.AgentClaude), Project: "project",
		Machine: "local", MessageCount: 1,
	}))
	require.NoError(t, database.InsertMessages([]db.Message{{
		SessionID: linkID, Ordinal: 0, Role: "assistant",
		ToolCalls: []db.ToolCall{{
			ToolUseID: "link-call", ToolName: "Task", Category: "Task",
		}},
	}}))
	require.NoError(t, engine.writeIncremental(&incrementalUpdate{
		sessionID: linkID, machine: "local", project: "project", msgCount: 1,
		links: []parser.ClaudeSubagentLink{{
			ToolUseID: "link-call", ResultContentRaw: raw,
			ResultContentLen: len(raw), HasResult: true,
		}},
	}))

	messages, err = database.GetAllMessages(t.Context(), linkID)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, linkWant, messages[0].ToolCalls[0].ResultContent)
	assert.Equal(t, len(linkWant), messages[0].ToolCalls[0].ResultContentLength)
}

func TestEngineImagePolicyDeduplicatesHistoricalRawLinkedResult(t *testing.T) {
	const raw = `[{"type":"text","text":"before"},{"type":"input_image","image_url":"data:image/png;base64,AAEC"},{"type":"text","text":"after"}]`
	const linkWant = "beforeafter"
	database := dbtest.OpenTestDB(t)
	database.SetToolResultImages(config.ToolResultImagesKeep)
	require.NoError(t, database.UpsertSession(db.Session{
		ID: "historical-link", Agent: string(parser.AgentClaude), Project: "project",
		Machine: "local", MessageCount: 1,
	}))
	require.NoError(t, database.InsertMessages([]db.Message{{
		SessionID: "historical-link", Ordinal: 0, Role: "assistant",
		ToolCalls: []db.ToolCall{{
			ToolUseID: "link-call", ToolName: "Task", Category: "Task",
			ResultContent: raw,
			ResultEvents: []db.ToolResultEvent{{
				ToolUseID: "link-call", Source: "tool", Status: "completed",
				Content: raw,
			}},
		}},
	}}))

	engine := NewEngine(database, EngineConfig{
		Machine: "local", ToolResultImages: config.ToolResultImagesDrop,
	})
	t.Cleanup(engine.Close)
	require.NoError(t, engine.writeIncremental(&incrementalUpdate{
		sessionID: "historical-link", machine: "local", project: "project", msgCount: 1,
		links: []parser.ClaudeSubagentLink{{
			ToolUseID: "link-call", ResultContentRaw: raw,
			ResultContentLen: len(raw), HasResult: true,
		}},
	}))

	var storedSummary string
	require.NoError(t, database.Reader().QueryRowContext(
		t.Context(), "SELECT COALESCE(result_content, '') FROM tool_calls WHERE session_id = ?", "historical-link",
	).Scan(&storedSummary))
	assert.Equal(t, linkWant, storedSummary)

	messages, err := database.GetAllMessages(t.Context(), "historical-link")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	require.Len(t, messages[0].ToolCalls, 1)
	assert.Equal(t, linkWant, messages[0].ToolCalls[0].ResultContent)
	assert.Equal(t, len(linkWant), messages[0].ToolCalls[0].ResultContentLength)
}

func TestEngineImagePolicyDeduplicatesLateProjectedResult(t *testing.T) {
	const raw = `[{"type":"text","text":"before"},{"type":"input_image","image_url":"data:image/png;base64,AAEC"},{"type":"text","text":"after"}]`
	const want = `[{"type":"text","text":"before"},{"byte_size":3,"media_type":"image/png","sha256":"","text":"[Image: image/png, 3 bytes]","type":"agentsview_image","version":1},{"type":"text","text":"after"}]`
	database := dbtest.OpenTestDB(t)
	database.SetToolResultImages(config.ToolResultImagesKeep)
	require.NoError(t, database.UpsertSession(db.Session{
		ID: "late-result", Agent: string(parser.AgentCodex), Project: "project",
		Machine: "local", MessageCount: 1,
	}))
	require.NoError(t, database.InsertMessages([]db.Message{{
		SessionID: "late-result", Ordinal: 0, Role: "assistant",
		ToolCalls: []db.ToolCall{{ToolUseID: "late-call", ToolName: "Bash", Category: "Bash"}},
	}}))

	engine := NewEngine(database, EngineConfig{
		Machine: "local", ToolResultImages: config.ToolResultImagesDrop,
	})
	t.Cleanup(engine.Close)
	require.NoError(t, engine.writeIncremental(&incrementalUpdate{
		sessionID: "late-result", machine: "local", project: "project", msgCount: 1,
		toolCallUpdates: []parser.ParsedToolCallUpdate{{
			ToolUseID: "late-call", MessageOrdinal: 0, CallIndex: 0,
			ResultEvents: []parser.ParsedToolResultEvent{{
				ToolUseID: "late-call", Source: "function_call_output", Content: raw,
			}},
		}},
	}))

	var storedSummary, storedEvent string
	require.NoError(t, database.Reader().QueryRowContext(
		t.Context(), "SELECT COALESCE(result_content, '') FROM tool_calls WHERE session_id = ?", "late-result",
	).Scan(&storedSummary))
	require.NoError(t, database.Reader().QueryRowContext(
		t.Context(), "SELECT content FROM tool_result_events WHERE session_id = ?", "late-result",
	).Scan(&storedEvent))
	assert.Empty(t, storedSummary)
	assert.Equal(t, want, storedEvent)
}

func TestReadOnlyResyncReplacementCarriesImagePolicy(t *testing.T) {
	for _, mode := range []config.ToolResultImages{config.ToolResultImagesDrop, config.ToolResultImagesOffload} {
		t.Run(string(mode), func(t *testing.T) {
			root := t.TempDir()
			archivePath := filepath.Join(t.TempDir(), "archive.db")
			sourcePath := filepath.Join(root, "project", "keep0.jsonl")
			require.NoError(t, os.MkdirAll(filepath.Dir(sourcePath), 0o755))
			require.NoError(t, os.WriteFile(sourcePath, []byte(
				testjsonl.NewSessionBuilder().
					AddClaudeUser("2026-01-01T00:00:00Z", "hello").
					AddClaudeAssistant("2026-01-01T00:00:01Z", "hi").
					String(),
			), 0o644))

			writable, err := db.Open(archivePath)
			require.NoError(t, err)
			engine := NewEngine(writable, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}},
				Machine:   "local",
			})
			require.Equal(t, 1, engine.SyncAll(context.Background(), nil).Synced)
			copiedContent := `[{"type":"text","text":"before"},{"type":"input_image","image_url":"data:image/png;base64,AAEC"},{"type":"text","text":"after"}]`
			for _, id := range []string{"trashed", "source-missing"} {
				filePath := filepath.Join(root, id+".jsonl")
				require.NoError(t, writable.UpsertSession(db.Session{
					ID: id, Project: "archived", Machine: "local",
					Agent: string(parser.AgentClaude), MessageCount: 1,
					FilePath: &filePath,
				}))
				require.NoError(t, writable.InsertMessages([]db.Message{{
					SessionID: id, Ordinal: 0, Role: "assistant",
					ToolCalls: []db.ToolCall{{
						ToolUseID:     "copied-call",
						ResultContent: copiedContent,
						ResultEvents: []db.ToolResultEvent{{
							ToolUseID: "copied-call", Source: "tool",
							Status: "completed", Content: copiedContent,
						}},
					}},
				}}))
			}
			require.NoError(t, writable.SoftDeleteSession("trashed"))
			require.NoError(t, writable.Update(func(tx *sql.Tx) error {
				_, err := tx.Exec(
					"UPDATE sessions SET source_missing_at = ? WHERE id = ?",
					"2026-01-01T00:00:00Z", "source-missing",
				)
				return err
			}))
			engine.Close()
			require.NoError(t, writable.Close())

			readOnly, err := db.OpenReadOnly(archivePath)
			require.NoError(t, err)
			resyncEngine := NewEngine(readOnly, EngineConfig{
				AgentDirs:        map[parser.AgentType][]string{parser.AgentClaude: {root}},
				Machine:          "local",
				ToolResultImages: mode, AssetsDir: t.TempDir(),
			})
			t.Cleanup(resyncEngine.Close)
			t.Cleanup(func() { require.NoError(t, readOnly.Close()) })

			content := `[ {"type":"input_image","image_url":"data:image/png;base64,AAEC"} ]`
			tempPath := archivePath + resyncTempSuffix
			operations := productionRebuildOperations
			operations.rebuildFTS = func(database *db.DB) error {
				messages, err := database.GetAllMessages(context.Background(), "keep0")
				if err != nil {
					return err
				}
				if len(messages) == 0 {
					return fmt.Errorf("resync test session was not rebuilt")
				}
				messages[0].ToolCalls = []db.ToolCall{{
					ToolUseID:     "call-image",
					ResultContent: content,
					ResultEvents: []db.ToolResultEvent{{
						ToolUseID: "call-image", Source: "tool",
						Status: "completed", Content: content,
					}},
				}}
				return database.ReplaceSessionMessages("keep0", messages)
			}
			stats, err := resyncEngine.resyncBuildLocked(
				context.Background(), nil, RebuildOptions{}, operations, false,
			)
			require.NoError(t, err)
			assert.False(t, stats.Aborted)

			replacement, err := db.Open(tempPath)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, replacement.Close()) })
			messages, err := replacement.GetAllMessages(context.Background(), "keep0")
			require.NoError(t, err)
			require.Len(t, messages, 2)
			assert.NotContains(t, messages[0].ToolCalls[0].ResultContent, "input_image")
			assert.NotContains(t, messages[0].ToolCalls[0].ResultEvents[0].Content, "input_image")
			for _, id := range []string{"trashed", "source-missing"} {
				messages, err := replacement.GetAllMessages(context.Background(), id)
				require.NoError(t, err)
				require.Len(t, messages, 1)
				require.Len(t, messages[0].ToolCalls, 1)
				assert.NotContains(t, messages[0].ToolCalls[0].ResultContent, "input_image")
				require.Len(t, messages[0].ToolCalls[0].ResultEvents, 1)
				assert.NotContains(t,
					messages[0].ToolCalls[0].ResultEvents[0].Content,
					"input_image",
				)
				var storedEvent string
				require.NoError(t, replacement.Reader().QueryRowContext(
					context.Background(),
					`SELECT content FROM tool_result_events
			 WHERE session_id = ? AND tool_call_message_ordinal = ?
			   AND call_index = ?`, id, 0, 0,
				).Scan(&storedEvent))
				assert.NotContains(t, storedEvent, "input_image")
				if mode == config.ToolResultImagesOffload {
					assert.Contains(t, storedEvent, `"image_ref":"asset://`)
				}
			}

		})
	}
}

func TestDropPolicyProjectsVisualStudioCopilotArchiveMerge(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	content := `[{"type":"input_image","image_url":"data:image/png;base64,AAEC"}]`
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sessionID := "copilot"
	require.NoError(t, database.UpsertSession(db.Session{
		ID: sessionID, Project: "project", Machine: "local",
		Agent: string(parser.AgentVSCopilot), MessageCount: 1,
	}))
	database.SetToolResultImages(config.ToolResultImagesKeep)
	require.NoError(t, database.InsertMessages([]db.Message{{
		SessionID: sessionID, Ordinal: 0, Role: "assistant",
		Content: "old", Timestamp: ts.Format(time.RFC3339Nano),
		ToolCalls: []db.ToolCall{{
			ToolUseID: "call", ResultContent: content,
			ResultEvents: []db.ToolResultEvent{{
				ToolUseID: "call", Source: "tool", Status: "completed",
				Content: content,
			}},
		}},
	}}))
	database.SetToolResultImages(config.ToolResultImagesDrop)
	engine := NewEngine(database, EngineConfig{Machine: "local"})
	_, projected, verdict := engine.prepareSessionWrite(pendingWrite{
		sess: parser.ParsedSession{
			ID: sessionID, Project: "project", Machine: "local",
			Agent: parser.AgentVSCopilot, MessageCount: 1,
			StartedAt: ts, EndedAt: ts,
			File: parser.FileInfo{Path: "copilot.trace", Size: 2},
		},
		msgs: []parser.ParsedMessage{{
			Ordinal: 0, Role: parser.RoleAssistant,
			Content: "newer content", ContentLength: len("newer content"),
			Timestamp: ts,
			ToolCalls: []parser.ParsedToolCall{{
				ToolUseID: "call",
				ResultEvents: []parser.ParsedToolResultEvent{{
					ToolUseID: "call", Source: "tool", Status: "completed",
					Content: content,
				}},
			}},
		}},
	}, nil)
	require.Equal(t, sessionWriteOK, verdict)
	assert.NotContains(t, projected[0].ToolCalls[0].ResultContent, "input_image")
	assert.NotContains(t, projected[0].ToolCalls[0].ResultEvents[0].Content, "input_image")
}

func TestCodexImageRetentionAcrossFullAndLateResults(t *testing.T) {
	for _, mode := range []config.ToolResultImages{config.ToolResultImagesDrop, config.ToolResultImagesOffload} {
		t.Run(string(mode), func(t *testing.T) {
			const uuid = "019eb791-cf7d-75c1-8439-9ed74c122b06"
			const raw = `[{"type":"input_image","image_url":"data:image/png;base64,AAEC"}]`
			const later = `[{"type":"input_image","image_url":"data:image/png;base64,AwQF"}]`
			const want = `[{"byte_size":3,"media_type":"image/png","sha256":"","text":"[Image: image/png, 3 bytes]","type":"agentsview_image","version":1}]`
			for _, threshold := range []int64{1, 1 << 30} {
				t.Run(fmt.Sprint(threshold), func(t *testing.T) {
					root := t.TempDir()
					day := filepath.Join(root, "2024", "01", "01")
					require.NoError(t, os.MkdirAll(day, 0o755))
					path := filepath.Join(day, "rollout-2024-01-01T10-00-00-"+uuid+".jsonl")
					transcript := testjsonl.JoinJSONL(
						testjsonl.CodexSessionMetaJSON(uuid, root, "user", "2024-01-01T10:00:00Z"),
						testjsonl.CodexMsgJSON("user", "show image", "2024-01-01T10:00:01Z"),
						testjsonl.CodexFunctionCallWithCallIDJSON("exec_command", "call", `{}`, "2024-01-01T10:00:02Z"),
						testjsonl.CodexFunctionCallOutputJSON("call", json.RawMessage(raw), "2024-01-01T10:00:03Z"),
					)
					require.NoError(t, os.WriteFile(path, []byte(transcript), 0o600))
					database := openTestDB(t)
					database.SetToolResultImages(config.ToolResultImagesKeep)
					engine := NewEngine(database, EngineConfig{Machine: "local", Ephemeral: true,
						AgentDirs:        map[parser.AgentType][]string{parser.AgentCodex: {root}},
						ToolResultImages: mode, AssetsDir: t.TempDir(), StagedCodexParseMinBytes: threshold,
						DisableFilesystemProjectDiscovery: true,
					})
					t.Cleanup(engine.Close)
					stats := engine.SyncAll(t.Context(), nil)
					require.Equal(t, 1, stats.Synced)
					for _, late := range []bool{false, true} {
						if late {
							require.NoError(t, engine.writeIncremental(&incrementalUpdate{
								sessionID: "codex:" + uuid, machine: "local", project: "project", msgCount: 2,
								toolCallUpdates: []parser.ParsedToolCallUpdate{{ToolUseID: "call", MessageOrdinal: 1, CallIndex: 0, ResultEvents: []parser.ParsedToolResultEvent{{
									ToolUseID: "call", Source: "function_call_output", Content: later,
								}}}},
							}))
						}
						messages, err := database.GetAllMessages(t.Context(), "codex:"+uuid)
						require.NoError(t, err)
						var calls []db.ToolCall
						for _, message := range messages {
							calls = append(calls, message.ToolCalls...)
						}
						require.Len(t, calls, 1)
						if mode == config.ToolResultImagesDrop {
							assert.Equal(t, want, calls[0].ResultContent)
						} else {
							assert.Contains(t, calls[0].ResultContent, `"image_ref":"asset://`)
						}
						count := 1
						if late {
							count = 2
						}
						require.Len(t, calls[0].ResultEvents, count)
						for _, event := range calls[0].ResultEvents {
							if mode == config.ToolResultImagesDrop {
								assert.Equal(t, want, event.Content)
							} else {
								assert.Contains(t, event.Content, `"image_ref":"asset://`)
							}
							assert.Equal(t, len(event.Content), event.ContentLength)
						}
					}
				})
			}

		})
	}
}

func TestCodexDropImagesNeverEnterScratch(t *testing.T) {
	sink, err := newCodexStagingSink(t.TempDir(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sink.Close()) })
	sink.toolResultImages = config.ToolResultImagesDrop
	sink.AppendMessage(parser.ParsedMessage{ToolCalls: []parser.ParsedToolCall{{ToolUseID: "call", Category: "Bash"}}})
	const raw = `[{"type":"input_image","image_url":"data:image/png;base64,AAEC"}]`
	const want = `[{"byte_size":3,"media_type":"image/png","sha256":"","text":"[Image: image/png, 3 bytes]","type":"agentsview_image","version":1}]`
	for _, agent := range []string{"agent-a", "agent-b"} {
		sink.AppendToolResultEvent("call", nil, parser.ParsedToolResultEvent{
			ToolUseID: "call", AgentID: agent, Source: "function_call_output", Content: raw,
		})
	}
	require.NoError(t, sink.Err())
	summary, length, err := sink.ResolveSummary(t.Context(), db.StagedToolCallKey("call", 0))
	require.NoError(t, err)
	assert.Equal(t, "agent-a:\n"+want+"\n\nagent-b:\n"+want, summary)
	assert.Equal(t, len(summary), length)
	var content string
	var eventLength int
	require.NoError(t, sink.scratch.QueryRow("SELECT content, content_length FROM stage_events LIMIT 1").Scan(&content, &eventLength))
	assert.Equal(t, want, content)
	assert.Equal(t, len(want), eventLength)
	bytes, err := os.ReadFile(sink.Path())
	require.NoError(t, err)
	assert.NotContains(t, string(bytes), "data:image/png;base64,AAEC")
}

func TestCodexDropImagesNeverEnterScratchWhenToolContentOmitted(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	database.SetArchiveContent(config.ArchiveContentTranscripts)
	sink, err := newCodexStagingSink(t.TempDir(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sink.Close()) })
	sink.database = database
	sink.toolResultImages = config.ToolResultImagesDrop
	sink.AppendMessage(parser.ParsedMessage{ToolCalls: []parser.ParsedToolCall{{ToolUseID: "call", Category: "Bash"}}})
	const raw = `[{"type":"input_image","image_url":"data:image/png;base64,AAEC"}]`
	const want = `[{"byte_size":3,"media_type":"image/png","sha256":"","text":"[Image: image/png, 3 bytes]","type":"agentsview_image","version":1}]`
	sink.AppendToolResultEvent("call", nil, parser.ParsedToolResultEvent{
		ToolUseID: "call", Source: "function_call_output", Content: raw,
	})
	require.NoError(t, sink.Err())
	var content string
	var contentLength int
	require.NoError(t, sink.scratch.QueryRow(
		"SELECT content, content_length FROM stage_events LIMIT 1",
	).Scan(&content, &contentLength))
	assert.Equal(t, want, content)
	assert.Equal(t, len(want), contentLength)
	bytes, err := os.ReadFile(sink.Path())
	require.NoError(t, err)
	assert.NotContains(t, string(bytes), "data:image/png;base64,AAEC")
}

func TestToolResultImagesOffloadFullIngest(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	engine := NewEngine(database, EngineConfig{Machine: "local", ToolResultImages: config.ToolResultImages("offload"), AssetsDir: t.TempDir()})
	t.Cleanup(engine.Close)
	outcome := engine.writeBatchBulkWithOutcome([]pendingWrite{{
		sess: parser.ParsedSession{ID: "offload-full", Project: "project", Machine: "local", Agent: parser.AgentCodex, StartedAt: time.Unix(1, 0)},
		msgs: []parser.ParsedMessage{{Ordinal: 0, Role: parser.RoleAssistant, Content: "answer", ToolCalls: []parser.ParsedToolCall{{ToolUseID: "image", ToolName: "Bash", Category: "Bash", ResultEvents: []parser.ParsedToolResultEvent{{ToolUseID: "image", Source: "tool", Status: "completed", Content: `[{"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jRZkAAAAASUVORK5CYII="}]`}}}}}},
	}}, false)
	require.NotNil(t, outcome)
	messages, err := database.GetAllMessages(context.Background(), "offload-full")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	require.Len(t, messages[0].ToolCalls, 1)
	require.Len(t, messages[0].ToolCalls[0].ResultEvents, 1)
	require.Contains(t, messages[0].ToolCalls[0].ResultEvents[0].Content, `"image_ref":"asset://`)
	objects, err := os.ReadDir(database.AssetsDir())
	require.NoError(t, err)
	require.Len(t, objects, 1)
	body, err := os.ReadFile(filepath.Join(database.AssetsDir(), objects[0].Name()))
	require.NoError(t, err)
	require.Len(t, body, 68)
	assert.Equal(t, []byte{137, 80, 78, 71, 13, 10, 26, 10}, body[:8])
}

func TestToolResultImagesStagedRoute(t *testing.T) {
	for _, omitted := range []bool{false, true} {
		for _, blocked := range []bool{false, true} {
			t.Run(fmt.Sprintf("omitted=%t/blocked=%t", omitted, blocked), func(t *testing.T) {
				database := dbtest.OpenTestDB(t)
				database.SetAssetsDir(t.TempDir())
				if omitted {
					database.SetArchiveContent(config.ArchiveContentTranscripts)
				}
				sink, err := newCodexStagingSink(t.TempDir(), map[string]bool{"Bash": blocked})
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, sink.Close()) })
				sink.database = database
				sink.toolResultImages = config.ToolResultImagesOffload
				sink.AppendMessage(parser.ParsedMessage{ToolCalls: []parser.ParsedToolCall{{ToolUseID: "call", Category: "Bash"}}})
				const raw = `[{"type":"input_image","image_url":"data:image/png;base64,AAEC"}]`
				sink.AppendToolResultEvent("call", nil, parser.ParsedToolResultEvent{ToolUseID: "call", Source: "function_call_output", Content: raw})
				require.NoError(t, sink.Err())
				var content string
				var length int
				require.NoError(t, sink.scratch.QueryRow("SELECT content, content_length FROM stage_events LIMIT 1").Scan(&content, &length))
				entries, err := os.ReadDir(database.AssetsDir())
				require.NoError(t, err)
				if blocked || omitted {
					assert.Empty(t, entries)
				} else {
					require.Len(t, entries, 1)
					body, err := os.ReadFile(filepath.Join(database.AssetsDir(), entries[0].Name()))
					require.NoError(t, err)
					assert.Equal(t, []byte{0, 1, 2}, body)
					assert.Contains(t, content, `"image_ref":"asset://`)
					assert.Equal(t, len(content), length)
				}
				if blocked {
					assert.Empty(t, content)
					assert.Equal(t, len(raw), length)
				}
			})
		}
	}
}
