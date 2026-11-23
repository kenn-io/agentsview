package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/secrets"
)

// ErrConversationSessionNotFound reports a session the archive has never
// held, or no longer holds any record of.
var ErrConversationSessionNotFound = errors.New("conversation session not found")

// ConversationSessionOptions selects one session's conversation stream.
type ConversationSessionOptions struct {
	TextMode  string
	SessionID string
	// MaxBytes bounds the text in one chunk record.
	MaxBytes int
	// ToolSummaries adds the session's tool_summary record.
	ToolSummaries bool
	// RevealSecrets writes stored text unchanged instead of masking detected
	// secrets.
	RevealSecrets bool
}

// ConversationSessionHeader opens a session stream. Revision is the archive's
// publication revision in the snapshot the stream reads; every change in the
// stream is at or below it. Session and Project are nil for a session the
// archive no longer holds.
type ConversationSessionHeader struct {
	Type          string                   `json:"type"`
	SchemaVersion int                      `json:"schema_version"`
	TextMode      string                   `json:"text_mode,omitempty"`
	ArchiveID     string                   `json:"archive_id"`
	DatabaseID    string                   `json:"database_id"`
	SessionID     string                   `json:"session_id"`
	Revision      string                   `json:"revision"`
	Session       *ConversationSession     `json:"session"`
	Project       *export.ProjectReference `json:"project"`
}

// ConversationSessionChange is one change row of the session, without the
// session and project fields the header already carries. A message whose
// text is readable is followed by its ConversationSessionText records.
type ConversationSessionChange struct {
	Type      string  `json:"type"`
	MessageID string  `json:"message_id,omitempty"`
	Revision  string  `json:"revision"`
	Ordinal   int     `json:"ordinal"`
	Role      string  `json:"role,omitempty"`
	Timestamp *string `json:"timestamp"`
	Deleted   bool    `json:"deleted"`
	Gap       string  `json:"gap,omitempty"`
	Digest    string  `json:"digest,omitempty"`
	// TextBytes counts the text the stream writes, which is masked when
	// Redacted is set. Digest still names the stored text.
	TextBytes   int64                    `json:"text_bytes"`
	Redacted    bool                     `json:"redacted,omitzero"`
	ToolSummary *ConversationToolSummary `json:"tool_summary,omitempty"`
}

// ConversationSessionText is one bounded UTF-8 chunk of the message before it.
type ConversationSessionText struct {
	Type      string `json:"type"`
	MessageID string `json:"message_id"`
	Offset    int64  `json:"offset"`
	Text      string `json:"text"`
}

// ConversationSessionTrailer closes a complete stream. Messages counts the
// message records, including tombstones.
type ConversationSessionTrailer struct {
	Type     string `json:"type"`
	Messages int    `json:"messages"`
}

// ExportConversationSession writes one session's conversation as records:
// a header, its session change if any, each message with its text in
// revision order, its tool summary when asked, and a trailer. It reads one
// snapshot, so the records agree with each other and with a changes walk at
// the header's revision. A stream without its trailer is incomplete.
//
//nolint:gocyclo // Keep the snapshot, row order and per-message text checks in one read.
func (db *DB) ExportConversationSession(ctx context.Context, opts ConversationSessionOptions, write func(any) error) error {
	mode, err := conversationTextMode(opts.TextMode)
	if err != nil {
		return err
	}
	if db.usageOnlyStorage() {
		return fmt.Errorf("%w: conversation text is unavailable under archive_content=usage", ErrArchiveContentExcluded)
	}
	if opts.SessionID == "" {
		return errors.New("session id is required")
	}
	if opts.MaxBytes == 0 {
		opts.MaxBytes = 64 << 10
	}
	if opts.MaxBytes < utf8.UTFMax {
		return errors.New("max bytes must be at least 4")
	}
	// As with the changes listing, only a writable handle builds a cold
	// archive's projection here; the CLI builds it through the writer owner.
	if !db.readOnly {
		active, err := db.ConversationExportActive(ctx)
		if err != nil {
			return err
		}
		if !active {
			if _, err := db.EnsureConversationExportInitialized(ctx); err != nil {
				return err
			}
		}
	}
	tx, err := db.getReader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireConversationTextSchema(ctx, tx, mode); err != nil {
		return err
	}
	header := ConversationSessionHeader{Type: "header", SchemaVersion: 1, TextMode: mode, SessionID: opts.SessionID}
	header.ArchiveID, header.DatabaseID, err = conversationArchiveIdentity(ctx, tx)
	if err != nil {
		return err
	}
	if opts.ToolSummaries {
		if err := requireConversationToolSchema(ctx, tx); err != nil {
			return err
		}
	}
	var current int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT CAST(value AS INTEGER) FROM archive_metadata WHERE key = ?), 0)`, conversationRevisionKey).Scan(&current); err != nil {
		return err
	}
	header.Revision = strconv.FormatInt(current, 10)
	var known bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE id=?)`, opts.SessionID).Scan(&known); err != nil {
		return err
	}
	if known {
		// Attach through a live change, as a walk would, so the header carries
		// the classification and project a change row would carry.
		probe := []ConversationChange{{SessionID: opts.SessionID}}
		if err := db.attachConversationMetadata(ctx, tx, header.ArchiveID, probe, "visible"); err != nil {
			return err
		}
		header.Session, header.Project = probe[0].Session, &probe[0].Project
	}

	rows, err := tx.QueryContext(ctx, `SELECT 'message',`+conversationColumnsForMode(mode)+`,revision AS publication_revision
		FROM conversation_messages WHERE session_id = ? AND revision <= ?
		UNION ALL SELECT 'session',session_id,'',CAST(revision AS TEXT),0,'',NULL,deleted,gap,'',0,revision
		FROM conversation_session_changes WHERE session_id = ? AND revision <= ?
		ORDER BY publication_revision`, opts.SessionID, current, opts.SessionID, current)
	if err != nil {
		return fmt.Errorf("reading session conversation: %w", err)
	}
	defer rows.Close()
	var changes []ConversationChange
	for rows.Next() {
		var change ConversationChange
		var revision int64
		if err := rows.Scan(&change.Type, &change.SessionID, &change.MessageID, &change.Revision, &change.Ordinal, &change.Role,
			&change.Timestamp, &change.Deleted, &change.Gap, &change.Digest, &change.TextBytes, &revision); err != nil {
			return err
		}
		changes = append(changes, change)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if !known && len(changes) == 0 {
		return fmt.Errorf("%w: %s", ErrConversationSessionNotFound, opts.SessionID)
	}
	var summary []ConversationChange
	if opts.ToolSummaries && known {
		change := ConversationChange{Type: conversationToolChangeType, SessionID: opts.SessionID}
		err := tx.QueryRowContext(ctx, `SELECT CAST(t.revision AS TEXT),s.deleted_at IS NOT NULL,
			 CASE WHEN EXISTS(SELECT 1 FROM conversation_session_changes c WHERE c.session_id=t.session_id AND c.gap='archive_content_excluded')
			  THEN 'archive_content_excluded' ELSE '' END
			FROM conversation_tool_changes t JOIN sessions s ON s.id=t.session_id WHERE t.session_id=?`, opts.SessionID).
			Scan(&change.Revision, &change.Deleted, &change.Gap)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("reading session tool summary: %w", err)
		}
		if err == nil {
			summary = []ConversationChange{change}
			if err := attachConversationToolSummaries(ctx, tx, summary, time.Now().UTC()); err != nil {
				return err
			}
		}
	}

	if err := write(header); err != nil {
		return err
	}
	messages := 0
	for _, change := range changes {
		record := ConversationSessionChange{
			Type: change.Type, MessageID: change.MessageID, Revision: change.Revision, Ordinal: change.Ordinal,
			Role: change.Role, Timestamp: change.Timestamp, Deleted: change.Deleted, Gap: change.Gap, Digest: change.Digest, TextBytes: change.TextBytes,
		}
		if change.Type != "message" {
			if err := write(record); err != nil {
				return err
			}
			continue
		}
		messages++
		var text string
		if change.Digest != "" && !change.Deleted {
			// The projection holds the digest; the archived message holds the text.
			var content string
			if err := tx.QueryRowContext(ctx, `SELECT `+conversationBodyColumn(mode)+` FROM conversation_messages c JOIN messages m ON m.session_id = c.session_id AND m.ordinal = c.ordinal
			 WHERE c.session_id = ? AND c.message_id = ?`, opts.SessionID, change.MessageID).Scan(&content); err != nil {
				return fmt.Errorf("reading message %s text: %w", change.MessageID, err)
			}
			text = SanitizeUTF8(content)
			if sum := sha256.Sum256([]byte(text)); hex.EncodeToString(sum[:]) != change.Digest {
				return fmt.Errorf("message %s text does not match its digest", change.MessageID)
			}
			if !opts.RevealSecrets {
				if masked := secrets.Redact(text); masked != text {
					text, record.Redacted = masked, true
					record.TextBytes = int64(len(text))
				}
			}
		}
		if err := write(record); err != nil {
			return err
		}
		for offset := 0; offset < len(text); {
			end := min(len(text), offset+opts.MaxBytes)
			for end > offset && end < len(text) && !utf8.RuneStart(text[end]) {
				end--
			}
			if err := write(ConversationSessionText{Type: "text", MessageID: change.MessageID, Offset: int64(offset), Text: strings.Clone(text[offset:end])}); err != nil {
				return err
			}
			offset = end
		}
	}
	for _, change := range summary {
		if err := write(ConversationSessionChange{Type: change.Type, Revision: change.Revision, Deleted: change.Deleted, Gap: change.Gap, ToolSummary: change.ToolSummary}); err != nil {
			return err
		}
	}
	if err := write(ConversationSessionTrailer{Type: "trailer", Messages: messages}); err != nil {
		return err
	}
	return tx.Commit()
}
