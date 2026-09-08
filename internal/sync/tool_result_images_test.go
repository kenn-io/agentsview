package sync

import (
	"context"
	"database/sql"
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

func TestPrepareSessionWritePreservesRawToolResultImageBlocks(t *testing.T) {
	content := `[{"type":"text","text":"before"},{"type":"input_image","image_url":"data:image/png;base64,AAEC"},{"type":"text","text":"after"}]`
	for _, tt := range []struct {
		name   string
		policy config.ToolResultImages
		image  string
	}{
		{name: "keep", policy: config.ToolResultImagesKeep, image: "input_image"},
		{name: "drop", policy: config.ToolResultImagesDrop, image: "agentsview_image"},
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
			assert.Contains(t, call.ResultContent, tt.image)
			if tt.policy == config.ToolResultImagesDrop {
				assert.NotContains(t, call.ResultContent, "input_image", tt.name)
			}
			assert.Contains(t, call.ResultContent, `"text":"before"`)
			assert.Contains(t, call.ResultContent, `"text":"after"`)
			assert.Equal(t, "Bash", call.ToolName)
			assert.Equal(t, "Bash", call.Category)
		})
	}
}

func TestIncrementalSubagentLinksPreserveRawToolResultImageBlocks(t *testing.T) {
	content := `[{"type":"text","text":"before"},{"type":"input_image","image_url":"data:image/png;base64,AAEC"},{"type":"text","text":"after"}]`
	for _, tt := range []struct {
		name   string
		policy config.ToolResultImages
		image  string
	}{
		{name: "keep", policy: config.ToolResultImagesKeep, image: "input_image"},
		{name: "drop", policy: config.ToolResultImagesDrop, image: "agentsview_image"},
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
			assert.Contains(t, result, tt.image)
			assert.Contains(t, result, `"text":"before"`)
			assert.Contains(t, result, `"text":"after"`)
		})
	}
}

func TestReadOnlyResyncReplacementCarriesDropPolicy(t *testing.T) {
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
		ToolResultImages: config.ToolResultImagesDrop,
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
