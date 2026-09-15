package storetest

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db/bunmodel"
)

const (
	rootNewID      = "bun-contract-root-new"
	childID        = "bun-contract-child"
	childNoStartID = "bun-contract-child-no-start"
	rootOldID      = "bun-contract-root-old"
	deletedID      = "bun-contract-deleted"
	activeID       = "bun-contract-active"
)

// Fixture names the literal rows used by every core-store backend contract.
type Fixture struct {
	RootNewID      string
	ChildID        string
	ChildNoStartID string
	RootOldID      string
	DeletedID      string
	ActiveID       string
}

// CoreFixture returns the backend-independent fixture identity.
func CoreFixture() Fixture {
	return Fixture{
		RootNewID:      rootNewID,
		ChildID:        childID,
		ChildNoStartID: childNoStartID,
		RootOldID:      rootOldID,
		DeletedID:      deletedID,
		ActiveID:       activeID,
	}
}

// InsertBunCoreFixture inserts the canonical rows used by RunCoreContract.
// Rows are inserted individually so a default-valued field in one fixture row
// cannot suppress a non-default field in another row's generated INSERT.
func InsertBunCoreFixture(
	ctx context.Context,
	store bun.IDB,
	archiveID string,
	generation string,
) error {
	if _, err := store.NewInsert().Model(&bunmodel.SourceArchive{
		SourceArchiveID: archiveID, SourceArchiveSalt: "contract-salt",
	}).Exec(ctx); err != nil {
		return fmt.Errorf("inserting contract source archive: %w", err)
	}

	sessions, messages, call, events := bunCoreRows(archiveID, generation)
	for index := range sessions {
		if _, err := store.NewInsert().Model(&sessions[index]).Exec(ctx); err != nil {
			return fmt.Errorf("inserting contract session %s: %w", sessions[index].ID, err)
		}
	}
	for index := range messages {
		if _, err := store.NewInsert().Model(&messages[index]).Exec(ctx); err != nil {
			return fmt.Errorf("inserting contract message %d: %w", messages[index].Ordinal, err)
		}
	}
	if _, err := store.NewInsert().Model(&call).Exec(ctx); err != nil {
		return fmt.Errorf("inserting contract tool call: %w", err)
	}
	for index := range events {
		if _, err := store.NewInsert().Model(&events[index]).Exec(ctx); err != nil {
			return fmt.Errorf("inserting contract tool event %d: %w", events[index].EventIndex, err)
		}
	}
	return nil
}

// InsertSQLiteCoreFixture inserts the same literal contract through the
// shipped SQLite aliases, including tool_calls.message_id.
func InsertSQLiteCoreFixture(
	ctx context.Context,
	tx *sql.Tx,
	archiveID string,
	generation string,
) error {
	parentID := rootNewID
	rows := []struct {
		id, project, machine, agent, branch, relationship string
		parent                                            *string
		messageCount, userMessageCount                    int
		startedAt, endedAt, deletedAt                     any
		createdAt                                         string
	}{
		{
			id: rootNewID, project: "alpha", machine: "host-a", agent: "codex",
			branch: "main", messageCount: 3, userMessageCount: 1,
			startedAt: "2026-08-02T10:00:00Z", endedAt: "2026-08-02T10:03:00Z",
			createdAt: "2026-08-02T10:00:00Z",
		},
		{
			id: childID, project: "alpha", machine: "host-a", agent: "codex",
			relationship: "subagent", parent: &parentID,
			messageCount: 1, userMessageCount: 1,
			startedAt: "2026-08-02T10:00:30Z", endedAt: "2026-08-02T10:01:00Z",
			createdAt: "2026-08-02T10:00:30Z",
		},
		{
			id: childNoStartID, project: "alpha", machine: "host-a", agent: "codex",
			relationship: "subagent", parent: &parentID,
			messageCount: 1, userMessageCount: 1,
			createdAt: "2026-08-02T10:00:45Z",
		},
		{
			id: rootOldID, project: "alpha", machine: "host-b", agent: "claude",
			branch: "release", messageCount: 2, userMessageCount: 2,
			startedAt: "2026-08-01T09:00:00Z", endedAt: "2026-08-01T09:02:00Z",
			createdAt: "2026-08-01T09:00:00Z",
		},
		{
			id: deletedID, project: "beta", machine: "host-c", agent: "codex",
			messageCount: 1, userMessageCount: 1,
			deletedAt: "2026-08-02T11:00:00Z", createdAt: "2026-08-02T11:00:00Z",
		},
		{
			id: activeID, project: "termination", machine: "host-a", agent: "codex",
			messageCount: 1, userMessageCount: 1,
			startedAt: "2099-01-01T00:00:00Z", endedAt: "2099-01-01T00:01:00Z",
			createdAt: "2099-01-01T00:00:00Z",
		},
	}
	for _, row := range rows {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO sessions (
				id, project, machine, agent, git_branch, relationship_type,
				parent_session_id, message_count, user_message_count,
				started_at, ended_at, deleted_at, created_at,
				source_archive_id, source_database_generation,
					file_path, file_size, file_mtime, file_inode, file_device,
					file_hash, local_modified_at
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			row.id, row.project, row.machine, row.agent, row.branch, row.relationship,
			row.parent, row.messageCount, row.userMessageCount, row.startedAt,
			row.endedAt, row.deletedAt, row.createdAt, archiveID, generation,
			contractFilePath(row.id), contractFileSize(row.id),
			contractFileMtime(row.id), contractFileInode(row.id),
			contractFileDevice(row.id), contractFileHash(row.id),
			contractLocalModifiedAt(row.id),
		)
		if err != nil {
			return fmt.Errorf("inserting SQLite contract session %s: %w", row.id, err)
		}
	}

	messageRows := []struct {
		ordinal                  int
		role, content, timestamp string
		hasToolUse               bool
		model                    string
	}{
		{ordinal: 0, role: "user", content: "question", timestamp: "2026-08-02T10:00:00Z"},
		{ordinal: 1, role: "assistant", content: "working", timestamp: "2026-08-02T10:01:00Z", hasToolUse: true, model: "model-a"},
		{ordinal: 2, role: "assistant", content: "done", timestamp: "2026-08-02T10:02:00Z", model: "model-a"},
	}
	for _, row := range messageRows {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO messages (
				session_id, ordinal, role, content, content_length,
				timestamp, has_tool_use, model, token_usage
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, '{}')`,
			rootNewID, row.ordinal, row.role, row.content, len(row.content),
			row.timestamp, row.hasToolUse, row.model,
		)
		if err != nil {
			return fmt.Errorf("inserting SQLite contract message %d: %w", row.ordinal, err)
		}
	}
	var messageID int64
	if err := tx.QueryRowContext(ctx, `
		SELECT id FROM messages WHERE session_id = ? AND ordinal = 1`,
		rootNewID,
	).Scan(&messageID); err != nil {
		return fmt.Errorf("selecting SQLite contract message id: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tool_calls (
			message_id, session_id, message_ordinal, call_index,
			tool_name, category, tool_use_id, input_json
		) VALUES (?, ?, 1, 0, 'Read', 'Read', 'call-1', '{"file_path":"README.md"}')`,
		messageID, rootNewID,
	); err != nil {
		return fmt.Errorf("inserting SQLite contract tool call: %w", err)
	}
	for _, event := range []struct {
		status, content, timestamp string
		index                      int
	}{
		{status: "started", timestamp: "2026-08-02T10:01:05Z", index: 0},
		{status: "completed", content: "ok", timestamp: "2026-08-02T10:01:20Z", index: 1},
	} {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO tool_result_events (
				session_id, tool_call_message_ordinal, call_index,
				source, status, content, content_length, timestamp, event_index
			) VALUES (?, 1, 0, 'tool_execution', ?, ?, ?, ?, ?)`,
			rootNewID, event.status, event.content, len(event.content),
			event.timestamp, event.index,
		)
		if err != nil {
			return fmt.Errorf("inserting SQLite contract event %d: %w", event.index, err)
		}
	}
	return nil
}

func bunCoreRows(
	archiveID string,
	generation string,
) ([]bunmodel.Session, []bunmodel.Message, bunmodel.ToolCall, []bunmodel.ToolResultEvent) {
	timestamp := func(value string) *bunmodel.Timestamp {
		parsed, err := bunmodel.ParseTimestamp(value)
		if err != nil {
			panic(err)
		}
		return &parsed
	}
	requiredTimestamp := func(value string) bunmodel.Timestamp { return *timestamp(value) }
	parentID := rootNewID
	fileMtime := int64(42)
	fileSize := int64(2048)
	fileInode := int64(73)
	fileDevice := int64(19)
	fileHash := "hash"
	rootFilePath := "fixtures/root-new.jsonl"
	childFilePath := "fixtures/child.jsonl"
	deletedAt := timestamp("2026-08-02T11:00:00Z")
	sessions := []bunmodel.Session{
		{
			ID: rootNewID, Project: "alpha", Machine: "host-a", Agent: "codex",
			GitBranch: "main", MessageCount: 3, UserMessageCount: 1,
			StartedAt: timestamp("2026-08-02T10:00:00Z"),
			EndedAt:   timestamp("2026-08-02T10:03:00Z"),
			CreatedAt: requiredTimestamp("2026-08-02T10:00:00Z"),
			FilePath:  &rootFilePath, FileSize: &fileSize, FileMtime: &fileMtime,
			FileInode: &fileInode, FileDevice: &fileDevice, FileHash: &fileHash,
			LocalModifiedAt:          timestamp("2026-08-02T10:04:00Z"),
			SourceArchiveID:          archiveID,
			SourceDatabaseGeneration: generation,
		},
		{
			ID: childID, Project: "alpha", Machine: "host-a", Agent: "codex",
			RelationshipType: "subagent", ParentSessionID: &parentID,
			MessageCount: 1, UserMessageCount: 1,
			StartedAt: timestamp("2026-08-02T10:00:30Z"),
			EndedAt:   timestamp("2026-08-02T10:01:00Z"),
			CreatedAt: requiredTimestamp("2026-08-02T10:00:30Z"),
			FilePath:  &childFilePath, FileSize: &fileSize, FileMtime: &fileMtime,
			FileInode: &fileInode, FileDevice: &fileDevice, FileHash: &fileHash,
			LocalModifiedAt: timestamp("2026-08-02T10:04:00Z"),
			SourceArchiveID: archiveID, SourceDatabaseGeneration: generation,
		},
		{
			ID: childNoStartID, Project: "alpha", Machine: "host-a", Agent: "codex",
			RelationshipType: "subagent", ParentSessionID: &parentID,
			MessageCount: 1, UserMessageCount: 1,
			CreatedAt:       requiredTimestamp("2026-08-02T10:00:45Z"),
			SourceArchiveID: archiveID, SourceDatabaseGeneration: generation,
		},
		{
			ID: rootOldID, Project: "alpha", Machine: "host-b", Agent: "claude",
			GitBranch: "release", MessageCount: 2, UserMessageCount: 2,
			StartedAt:       timestamp("2026-08-01T09:00:00Z"),
			EndedAt:         timestamp("2026-08-01T09:02:00Z"),
			CreatedAt:       requiredTimestamp("2026-08-01T09:00:00Z"),
			SourceArchiveID: archiveID, SourceDatabaseGeneration: generation,
		},
		{
			ID: deletedID, Project: "beta", Machine: "host-c", Agent: "codex",
			MessageCount: 1, UserMessageCount: 1, DeletedAt: deletedAt,
			CreatedAt:       requiredTimestamp("2026-08-02T11:00:00Z"),
			SourceArchiveID: archiveID, SourceDatabaseGeneration: generation,
		},
		{
			ID: activeID, Project: "termination", Machine: "host-a", Agent: "codex",
			MessageCount: 1, UserMessageCount: 1,
			StartedAt:       timestamp("2099-01-01T00:00:00Z"),
			EndedAt:         timestamp("2099-01-01T00:01:00Z"),
			CreatedAt:       requiredTimestamp("2099-01-01T00:00:00Z"),
			SourceArchiveID: archiveID, SourceDatabaseGeneration: generation,
		},
	}
	messageIDs := []int64{801, 802, 803}
	messages := []bunmodel.Message{
		{ID: &messageIDs[0], SessionID: rootNewID, Ordinal: 0, Role: "user", Content: "question", ContentLength: 8, Timestamp: timestamp("2026-08-02T10:00:00Z"), TokenUsage: json.RawMessage(`{}`)},
		{ID: &messageIDs[1], SessionID: rootNewID, Ordinal: 1, Role: "assistant", Content: "working", ContentLength: 7, Timestamp: timestamp("2026-08-02T10:01:00Z"), HasToolUse: true, Model: "model-a", TokenUsage: json.RawMessage(`{}`)},
		{ID: &messageIDs[2], SessionID: rootNewID, Ordinal: 2, Role: "assistant", Content: "done", ContentLength: 4, Timestamp: timestamp("2026-08-02T10:02:00Z"), Model: "model-a", TokenUsage: json.RawMessage(`{}`)},
	}
	toolID := int64(901)
	input := `{"file_path":"README.md"}`
	call := bunmodel.ToolCall{
		ID: &toolID, MessageID: &messageIDs[1], SessionID: rootNewID,
		MessageOrdinal: 1, CallIndex: 0, ToolName: "Read", Category: "Read",
		ToolUseID: "call-1", InputJSON: &input,
	}
	eventIDs := []int64{1001, 1002}
	events := []bunmodel.ToolResultEvent{
		{ID: &eventIDs[0], SessionID: rootNewID, ToolCallMessageOrdinal: 1, CallIndex: 0, Source: "tool_execution", Status: "started", Content: "", Timestamp: timestamp("2026-08-02T10:01:05Z"), EventIndex: 0},
		{ID: &eventIDs[1], SessionID: rootNewID, ToolCallMessageOrdinal: 1, CallIndex: 0, Source: "tool_execution", Status: "completed", Content: "ok", ContentLength: 2, Timestamp: timestamp("2026-08-02T10:01:20Z"), EventIndex: 1},
	}
	return sessions, messages, call, events
}

func contractFileMtime(id string) any {
	if id == rootNewID || id == childID {
		return int64(42)
	}
	return nil
}

func contractFileHash(id string) any {
	if id == rootNewID || id == childID {
		return "hash"
	}
	return nil
}

func contractLocalModifiedAt(id string) any {
	if id == rootNewID || id == childID {
		return "2026-08-02T10:04:00Z"
	}
	return nil
}

func contractFilePath(id string) any {
	switch id {
	case rootNewID:
		return "fixtures/root-new.jsonl"
	case childID:
		return "fixtures/child.jsonl"
	default:
		return nil
	}
}

func contractFileSize(id string) any {
	if id == rootNewID || id == childID {
		return int64(2048)
	}
	return nil
}

func contractFileInode(id string) any {
	if id == rootNewID || id == childID {
		return int64(73)
	}
	return nil
}

func contractFileDevice(id string) any {
	if id == rootNewID || id == childID {
		return int64(19)
	}
	return nil
}
