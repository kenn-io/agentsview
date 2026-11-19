package db

import (
	"context"
	"fmt"
	"log"
)

// StoredDialogue returns the dialogue_text value every backend writes for a
// message. A dialogue projection that repeats content on a message without
// thinking text is stored as NULL, and isContent reports that case. Search
// reads COALESCE(dialogue_text, content), so the NULL finds the same text
// without a second copy. A message with thinking text keeps its copy: search
// treats a NULL dialogue beside thinking text as a legacy row whose content
// still contains the thinking.
//
// The SQLite archive records isContent in messages.dialogue_is_content and
// the hosted raw dialogue table in raw_message_dialogue.is_content, so their
// readers restore the projection. The mirrors only search the column and keep
// no marker.
func StoredDialogue(content, thinking string, dialogue *string) (stored *string, isContent bool) {
	if dialogue != nil && thinking == "" && *dialogue == content {
		return nil, true
	}
	return dialogue, false
}

// restoreDialogue rebuilds a projection that SQLite stored as NULL because it
// repeated Content.
func restoreDialogue(m *Message, isContent bool) {
	if isContent {
		m.DialogueText = new(m.Content)
	}
}

// SQL that reads the full dialogue projection of a SQLite messages row, for
// readers that build messages or digests in SQL. Search reads
// COALESCE(dialogue_text, content), which needs no flag.
const (
	sqliteDialogueExprM    = `CASE WHEN m.dialogue_is_content THEN m.content ELSE m.dialogue_text END`
	sqliteDialogueExprBare = `CASE WHEN dialogue_is_content THEN content ELSE dialogue_text END`
)

// addDialogueIsContent adds messages.dialogue_is_content and moves existing
// duplicate dialogue into it. Both happen in one transaction, so the column's
// presence means the rewrite finished.
//
// The rewrite leaves COALESCE(dialogue_text, content) unchanged on every row,
// so the full-text indexes need no work. messages_au and the writer's CJK temp
// trigger compare dialogue_text itself and would delete and reinsert each
// row's identical index entry, so they are dropped for the UPDATE and
// restored.
func addDialogueIsContent(ctx context.Context, w *writerHandle, progress OpenProgressFunc) error {
	var present bool
	if err := w.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pragma_table_info('messages')
		WHERE name = 'dialogue_is_content')`).Scan(&present); err != nil {
		return fmt.Errorf("probing messages.dialogue_is_content: %w", err)
	}
	if present {
		return nil
	}
	progress.report("Removing duplicate dialogue text")
	tx, err := w.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting dialogue storage migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var hadUpdateTrigger bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master
		WHERE type = 'trigger' AND name = 'messages_au')`).Scan(&hadUpdateTrigger); err != nil {
		return fmt.Errorf("probing message update FTS trigger: %w", err)
	}
	var hadCJKUpdateTrigger bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_temp_master
		WHERE type = 'trigger' AND name = 'messages_cjk_au')`).Scan(&hadCJKUpdateTrigger); err != nil {
		return fmt.Errorf("probing CJK update FTS trigger: %w", err)
	}
	for _, statement := range []string{
		"ALTER TABLE messages ADD COLUMN dialogue_is_content INTEGER NOT NULL DEFAULT 0",
		"DROP TRIGGER IF EXISTS messages_au",
		"DROP TRIGGER IF EXISTS temp.messages_cjk_au",
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrating dialogue storage: %w", err)
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE messages SET dialogue_text = NULL, dialogue_is_content = 1
		WHERE dialogue_text = content AND thinking_text = ''`)
	if err != nil {
		return fmt.Errorf("deduplicating dialogue text: %w", err)
	}
	moved, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("counting deduplicated dialogue: %w", err)
	}
	if hadUpdateTrigger {
		if _, err := tx.ExecContext(ctx, messagesAUTriggerDDL); err != nil {
			return fmt.Errorf("restoring message update FTS trigger: %w", err)
		}
	}
	if hadCJKUpdateTrigger {
		if _, err := tx.ExecContext(ctx, schemaCJKFTSTriggers); err != nil {
			return fmt.Errorf("restoring CJK update FTS trigger: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing dialogue storage migration: %w", err)
	}
	log.Printf("migration: added column messages.dialogue_is_content; dropped %d duplicate dialogue copies", moved)
	return nil
}
