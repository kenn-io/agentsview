package db

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"unicode/utf8"

	"go.kenn.io/agentsview/internal/export"
)

const conversationRevisionKey = "conversation_publication_revision"

var (
	ErrConversationReconciliationRequired = errors.New("conversation reconciliation required")
	ErrConversationRevisionChanged        = errors.New("conversation message revision changed")
	ErrConversationInitializationRequired = errors.New("conversation export initialization is incomplete")
)

type ConversationExportOptions struct {
	Checkpoint string
	Cursor     string
	Limit      int
}

type ConversationChange struct {
	Type      string                  `json:"type"`
	SessionID string                  `json:"session_id"`
	MessageID string                  `json:"message_id"`
	Revision  string                  `json:"revision"`
	Ordinal   int                     `json:"ordinal"`
	Role      string                  `json:"role"`
	Timestamp *string                 `json:"timestamp"`
	Deleted   bool                    `json:"deleted"`
	Gap       string                  `json:"gap,omitempty"`
	Digest    string                  `json:"digest"`
	TextBytes int64                   `json:"text_bytes"`
	Project   export.ProjectReference `json:"project"`
}

type ConversationExportResult struct {
	SchemaVersion int                  `json:"schema_version"`
	ArchiveID     string               `json:"archive_id"`
	DatabaseID    string               `json:"database_id"`
	Changes       []ConversationChange `json:"changes"`
	NextCursor    string               `json:"next_cursor"`
	Checkpoint    string               `json:"checkpoint"`
}

type ConversationMessageOptions struct {
	DatabaseID string
	SessionID  string
	MessageID  string
	Revision   string
	Offset     int64
	MaxBytes   int
}

type ConversationMessage struct {
	ConversationChange
	SchemaVersion int     `json:"schema_version"`
	ArchiveID     string  `json:"archive_id"`
	DatabaseID    string  `json:"database_id"`
	Offset        int64   `json:"offset"`
	NextOffset    int64   `json:"next_offset"`
	Text          *string `json:"text"`
}

type conversationPosition struct {
	Version    int    `json:"version"`
	Kind       string `json:"kind"`
	DatabaseID string `json:"database_id"`
	After      int64  `json:"after,string"`
	Through    int64  `json:"through,string"`
}

const conversationChangeColumns = `session_id, message_id, CAST(revision AS TEXT), ordinal,
	role, NULLIF(timestamp,''), deleted, gap, digest, text_bytes`

func scanConversationChange(row rowScanner, change *ConversationChange) error {
	return row.Scan(&change.SessionID, &change.MessageID, &change.Revision,
		&change.Ordinal, &change.Role, &change.Timestamp, &change.Deleted,
		&change.Gap, &change.Digest, &change.TextBytes)
}

// ExportConversationChanges reads compact latest state. Each page has one
// snapshot; rows changed beyond the cycle's upper bound appear next cycle.
func (db *DB) ExportConversationChanges(ctx context.Context, opts ConversationExportOptions) (ConversationExportResult, error) {
	var result ConversationExportResult
	if opts.Checkpoint != "" && opts.Cursor != "" {
		return result, errors.New("checkpoint and cursor are mutually exclusive")
	}
	if opts.Limit == 0 {
		opts.Limit = MaxSessionLimit
	}
	if opts.Limit < 1 || opts.Limit > MaxSessionLimit {
		return result, fmt.Errorf("limit must be between 1 and %d", MaxSessionLimit)
	}
	var position conversationPosition
	encoded, kind := opts.Checkpoint, "checkpoint"
	if opts.Cursor != "" {
		encoded, kind = opts.Cursor, "cursor"
	}
	if encoded != "" {
		data, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil {
			return result, fmt.Errorf("%w: malformed conversation position", ErrInvalidCursor)
		}
		if err := json.Unmarshal(data, &position, json.RejectUnknownMembers(true)); err != nil {
			return result, fmt.Errorf("%w: malformed conversation position", ErrInvalidCursor)
		}
		if position.Version != 1 || position.Kind != kind || position.DatabaseID == "" || position.After < 0 || position.Through < position.After || kind == "checkpoint" && position.After != position.Through {
			return result, fmt.Errorf("%w: invalid conversation position", ErrInvalidCursor)
		}
	}
	tx, err := db.getReader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()
	result.SchemaVersion, result.Changes = 1, []ConversationChange{}
	result.ArchiveID, result.DatabaseID, err = conversationArchiveIdentity(ctx, tx)
	if err != nil {
		return result, err
	}
	var current int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT CAST(value AS INTEGER) FROM archive_metadata WHERE key = ?), 0)`, conversationRevisionKey).Scan(&current); err != nil {
		return result, err
	}
	if position.DatabaseID != "" && (position.DatabaseID != result.DatabaseID || position.Through > current) {
		return result, ErrConversationReconciliationRequired
	}
	if opts.Cursor == "" {
		position.Through = current
	}
	rows, err := tx.QueryContext(ctx, `SELECT 'message',`+conversationChangeColumns+`,revision AS publication_revision
		FROM conversation_messages WHERE revision > ? AND revision <= ?
		UNION ALL SELECT 'session',session_id,'',CAST(revision AS TEXT),0,'',NULL,deleted,gap,'',0,revision
		FROM conversation_session_changes WHERE revision > ? AND revision <= ?
		ORDER BY publication_revision LIMIT ?`, position.After, position.Through, position.After, position.Through, opts.Limit+1)
	if err != nil {
		return result, fmt.Errorf("reading conversation changes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var change ConversationChange
		var revision int64
		if err := rows.Scan(&change.Type, &change.SessionID, &change.MessageID, &change.Revision, &change.Ordinal, &change.Role, &change.Timestamp, &change.Deleted, &change.Gap, &change.Digest, &change.TextBytes, &revision); err != nil {
			return result, err
		}
		result.Changes = append(result.Changes, change)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return result, err
	}
	more := len(result.Changes) > opts.Limit
	if more {
		result.Changes = result.Changes[:opts.Limit]
	}
	if err := db.attachConversationProjects(ctx, tx, result.ArchiveID, result.Changes); err != nil {
		return result, err
	}
	position.Version, position.DatabaseID = 1, result.DatabaseID
	if more {
		position.Kind = "cursor"
		position.After, err = strconv.ParseInt(result.Changes[len(result.Changes)-1].Revision, 10, 64)
		if err != nil {
			return result, err
		}
	} else {
		position.Kind, position.After = "checkpoint", position.Through
	}
	data, err := json.Marshal(position)
	if err != nil {
		return result, err
	}
	token := base64.RawURLEncoding.EncodeToString(data)
	if more {
		result.NextCursor = token
	} else {
		result.Checkpoint = token
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	return result, nil
}

func conversationArchiveIdentity(ctx context.Context, tx *sql.Tx) (string, string, error) {
	for _, table := range []string{"conversation_messages", "conversation_session_changes"} {
		var present bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?
		)`, table).Scan(&present); err != nil {
			return "", "", fmt.Errorf("checking conversation export schema: %w", err)
		}
		if !present {
			return "", "", &SchemaUpgradeRequiredError{Table: table, Column: "session_id"}
		}
	}
	if _, err := sessionExportMetadataValue(ctx, tx, "conversation_export_initialized",
		ErrConversationInitializationRequired, "conversation export initialization"); err != nil {
		return "", "", err
	}
	archiveID, err := sessionExportMetadataValue(ctx, tx, archiveMetadataArchiveIDKey, ErrArchiveIDMissing, "archive id")
	if err != nil {
		return "", "", err
	}
	databaseID, err := sessionExportMetadataValue(ctx, tx, archiveMetadataDatabaseIDKey, ErrDatabaseIDMissing, "database id")
	return archiveID, databaseID, err
}

// GetConversationMessage reads a byte-bounded, revision-pinned stored text chunk.
// Project evidence and the body are resolved in the same archive snapshot.
func (db *DB) GetConversationMessage(ctx context.Context, opts ConversationMessageOptions) (ConversationMessage, error) {
	var result ConversationMessage
	if db.usageOnlyStorage() {
		return result, fmt.Errorf("%w: conversation text is unavailable under archive_content=usage", ErrArchiveContentExcluded)
	}
	if opts.DatabaseID == "" || opts.SessionID == "" || opts.MessageID == "" || opts.Revision == "" {
		return result, errors.New("database id, session id, message id and revision are required")
	}
	if opts.MaxBytes == 0 {
		opts.MaxBytes = 64 << 10
	}
	if opts.Offset < 0 || opts.MaxBytes < utf8.UTFMax {
		return result, errors.New("offset must be nonnegative and max bytes must be at least 4")
	}
	tx, err := db.getReader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()
	result.ArchiveID, result.DatabaseID, err = conversationArchiveIdentity(ctx, tx)
	if err != nil {
		return result, err
	}
	if result.DatabaseID != opts.DatabaseID {
		return ConversationMessage{}, ErrConversationReconciliationRequired
	}
	result.SchemaVersion, result.Type = 1, "message"
	err = scanConversationChange(tx.QueryRowContext(ctx, `SELECT `+conversationChangeColumns+` FROM conversation_messages WHERE session_id = ? AND message_id = ?`, opts.SessionID, opts.MessageID), &result.ConversationChange)
	if err != nil {
		return result, err
	}
	if result.Deleted || result.Revision != opts.Revision {
		return ConversationMessage{}, ErrConversationRevisionChanged
	}
	if opts.Offset > result.TextBytes {
		return ConversationMessage{}, errors.New("offset exceeds message text length")
	}
	changes := []ConversationChange{result.ConversationChange}
	if err := db.attachConversationProjects(ctx, tx, result.ArchiveID, changes); err != nil {
		return ConversationMessage{}, err
	}
	result.Project = changes[0].Project
	var body sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT substr(CAST(body AS BLOB), ?, ?) FROM conversation_messages WHERE session_id = ? AND message_id = ?`, opts.Offset+1, opts.MaxBytes, opts.SessionID, opts.MessageID).Scan(&body); err != nil {
		return ConversationMessage{}, err
	}
	result.Offset, result.NextOffset = opts.Offset, opts.Offset
	if body.Valid {
		chunk := body.String
		if len(chunk) > 0 && !utf8.RuneStart(chunk[0]) {
			return ConversationMessage{}, errors.New("offset is not a UTF-8 boundary")
		}
		for !utf8.ValidString(chunk) && len(chunk) > 0 {
			chunk = chunk[:len(chunk)-1]
		}
		result.Text, result.NextOffset = new(chunk), opts.Offset+int64(len(chunk))
	}
	if err := tx.Commit(); err != nil {
		return ConversationMessage{}, err
	}
	return result, nil
}

func (db *DB) attachConversationProjects(ctx context.Context, tx *sql.Tx, archiveID string, changes []ConversationChange) error {
	if len(changes) == 0 {
		return nil
	}
	ids := make([]string, 0, len(changes))
	for _, change := range changes {
		if !change.Deleted {
			ids = append(ids, change.SessionID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	snapshots, err := db.listSessionProjectIdentitySnapshotsFrom(ctx, tx, ids)
	if err != nil {
		return err
	}
	salt, err := sessionExportMetadataValue(ctx, tx, archiveMetadataArchiveSaltKey, ErrArchiveSaltMissing, "archive salt")
	if err != nil {
		return err
	}
	salt, err = validateArchiveSalt(salt)
	if err != nil {
		return err
	}
	for i := range changes {
		if changes[i].Deleted {
			continue
		}
		obs, ok := snapshots[changes[i].SessionID]
		if !ok {
			if err := tx.QueryRowContext(ctx, `SELECT project, machine FROM sessions WHERE id = ?`, changes[i].SessionID).Scan(&obs.Project, &obs.Machine); err != nil {
				return err
			}
			snapshots[changes[i].SessionID] = obs
		}
		changes[i].Project = export.ResolveProjectReferenceFromObservation(obs, export.IdentityScope{ArchiveID: archiveID, ArchiveSalt: salt})
	}
	return nil
}

type conversationRow struct {
	ConversationChange
	sourceID string
	body     *string
}

func conversationRowsTx(tx transactionQueries, sessionID string) ([]conversationRow, error) {
	rows, err := tx.Query(`SELECT `+conversationChangeColumns+`, source_id, body FROM conversation_messages WHERE session_id = ? AND removed = 0 ORDER BY ordinal`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []conversationRow
	for rows.Next() {
		var row conversationRow
		if err := rows.Scan(&row.SessionID, &row.MessageID, &row.Revision, &row.Ordinal, &row.Role, &row.Timestamp, &row.Deleted, &row.Gap, &row.Digest, &row.TextBytes, &row.sourceID, &row.body); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func conversationRowFromMessage(m Message) (conversationRow, bool) {
	if m.IsSystem || m.Role != "user" && m.Role != "assistant" || m.SourceSubtype == "tool_result" {
		return conversationRow{}, false
	}
	row := conversationRow{Type: "message", SessionID: m.SessionID, Ordinal: m.Ordinal, Role: m.Role, Timestamp: optionalStringPtr(m.Timestamp), sourceID: m.SourceUUID}
	if m.Content == "" {
		row.Gap = "visible_text_unavailable"
	} else {
		row.body = new(SanitizeUTF8(m.Content))
		digest := sha256.Sum256([]byte(*row.body))
		row.Digest, row.TextBytes = hex.EncodeToString(digest[:]), int64(len(*row.body))
		if row.sourceID == "" {
			row.Gap = "identity_unavailable"
		}
	}
	return row, true
}

// reconcileConversationMessagesTx runs before physical row replacement. Only
// native source identity or an unchanged complete projection preserves IDs;
// content digests are equality evidence, never logical message identifiers.
func reconcileConversationMessagesTx(tx transactionQueries, sessionID string, msgs []Message, replace, usageOnly bool) error {
	if replace && usageOnly {
		// Usage storage omits messages and text, so its projection cannot prove
		// deletion or changed identity. Preserve existing IDs as policy gaps;
		// the session gap covers activity without retained message records.
		return clearUsageOnlyConversationTx(tx, sessionID)
	}
	var incoming []conversationRow
	counts := map[string]int{}
	for _, msg := range msgs {
		msg.SessionID = sessionID
		row, ok := conversationRowFromMessage(msg)
		if !ok {
			continue
		}
		incoming = append(incoming, row)
		if row.sourceID != "" {
			counts[row.sourceID]++
		}
	}
	var old []conversationRow
	var err error
	if replace {
		old, err = conversationRowsTx(tx, sessionID)
		if err != nil {
			return err
		}
	}
	equal := replace && len(old) == len(incoming)
	for i := range incoming {
		if equal && !conversationRowsEqual(old[i], incoming[i]) {
			equal = false
		}
	}
	retained := map[string]bool{}
	for i := range incoming {
		row := &incoming[i]
		if equal {
			row.MessageID = old[i].MessageID
			// Identity gaps survive an unchanged projection; policy gaps must
			// reflect this write even when stored text is still absent.
			if old[i].Gap != "archive_content_excluded" {
				row.Gap = old[i].Gap
			}
		} else if row.sourceID != "" {
			var count int
			var id string
			var removed bool
			err := tx.QueryRow(`SELECT COUNT(*),COALESCE(MAX(message_id),''),COALESCE(MAX(removed),0)
			 FROM conversation_messages WHERE session_id=? AND source_id=?`, sessionID, row.sourceID).Scan(&count, &id, &removed)
			if err != nil {
				return err
			}
			if count == 1 && counts[row.sourceID] == 1 && (replace || removed) {
				row.MessageID = id
			} else if count > 0 || counts[row.sourceID] > 1 {
				// A native ID can restore its sole tombstone, but a reused source
				// ID cannot identify which occurrence survived a replacement.
				if !replace {
					if _, err := tx.Exec(`UPDATE conversation_messages SET gap='identity_ambiguous' WHERE session_id=? AND source_id=? AND removed=0`, sessionID, row.sourceID); err != nil {
						return err
					}
				}
				row.Gap = "identity_ambiguous"
			}
		}
		if row.MessageID == "" && row.body != nil && replace && len(old) > 0 && row.sourceID == "" {
			row.Gap = "identity_ambiguous"
		}
		if row.MessageID == "" {
			var id [16]byte
			if _, err := rand.Read(id[:]); err != nil {
				return err
			}
			row.MessageID = hex.EncodeToString(id[:])
		}
		if usageOnly {
			row.Gap = "archive_content_excluded"
		}
		retained[row.MessageID] = true
	}
	for _, row := range old {
		if !retained[row.MessageID] {
			if _, err := tx.Exec(`UPDATE conversation_messages SET deleted = 1, removed = 1, body = NULL WHERE session_id = ? AND message_id = ?`, sessionID, row.MessageID); err != nil {
				return err
			}
		}
	}
	for _, row := range incoming {
		if err := putConversationRowTx(tx, row); err != nil {
			return err
		}
	}
	if replace && !usageOnly {
		var hadGap, deleted bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM conversation_session_changes WHERE session_id=? AND gap='archive_content_excluded'),
		 COALESCE((SELECT deleted_at IS NOT NULL FROM sessions WHERE id=?),1)`, sessionID, sessionID).Scan(&hadGap, &deleted); err != nil {
			return err
		}
		if hadGap {
			return putConversationSessionChangeTx(tx, sessionID, "", deleted)
		}
	}
	return nil
}

func conversationRowsEqual(a, b conversationRow) bool {
	sameTime := a.Timestamp == nil && b.Timestamp == nil || a.Timestamp != nil && b.Timestamp != nil && *a.Timestamp == *b.Timestamp
	return a.Ordinal == b.Ordinal && a.Role == b.Role && sameTime && a.sourceID == b.sourceID && a.Digest == b.Digest && (a.body == nil) == (b.body == nil)
}

func putConversationRowTx(tx transactionQueries, row conversationRow) error {
	_, err := tx.Exec(`INSERT INTO conversation_messages (session_id,message_id,ordinal,role,timestamp,source_id,body,digest,text_bytes,gap,deleted)
		VALUES (?,?,?,?,COALESCE(?,''),?,?,?,?,?,COALESCE((SELECT deleted_at IS NOT NULL FROM sessions WHERE id = ?),0))
		ON CONFLICT(session_id,message_id) DO UPDATE SET ordinal=excluded.ordinal,role=excluded.role,timestamp=excluded.timestamp,source_id=excluded.source_id,
		body=excluded.body,digest=excluded.digest,text_bytes=excluded.text_bytes,gap=excluded.gap,deleted=excluded.deleted,removed=0
		WHERE ordinal IS NOT excluded.ordinal OR role IS NOT excluded.role OR timestamp IS NOT excluded.timestamp OR source_id IS NOT excluded.source_id
		OR body IS NOT excluded.body OR digest IS NOT excluded.digest OR text_bytes IS NOT excluded.text_bytes OR gap IS NOT excluded.gap OR deleted IS NOT excluded.deleted OR removed != 0`,
		row.SessionID, row.MessageID, row.Ordinal, row.Role, row.Timestamp, row.sourceID, row.body, row.Digest, row.TextBytes, row.Gap, row.SessionID)
	return err
}

func putConversationSessionChangeTx(tx transactionQueries, sessionID, gap string, deleted bool) error {
	var equal bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM conversation_session_changes WHERE session_id=? AND gap=? AND deleted=?)`, sessionID, gap, deleted).Scan(&equal); err != nil {
		return err
	}
	if equal {
		return nil
	}
	var revision int64
	if err := tx.QueryRow(`INSERT INTO archive_metadata(key,value) VALUES(?,'1')
	 ON CONFLICT(key) DO UPDATE SET value=CAST(CAST(value AS INTEGER)+1 AS TEXT) RETURNING CAST(value AS INTEGER)`, conversationRevisionKey).Scan(&revision); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT INTO conversation_session_changes(session_id,revision,gap,deleted) VALUES(?,?,?,?)
	 ON CONFLICT(session_id) DO UPDATE SET revision=excluded.revision,gap=excluded.gap,deleted=excluded.deleted`, sessionID, revision, gap, deleted)
	return err
}

func clearUsageOnlyConversationTx(tx transactionQueries, sessionID string) error {
	if _, err := tx.Exec(`UPDATE conversation_messages SET body=NULL,digest='',text_bytes=0,gap='archive_content_excluded' WHERE session_id=? AND removed=0`, sessionID); err != nil {
		return err
	}
	var deleted bool
	if err := tx.QueryRow(`SELECT deleted_at IS NOT NULL FROM sessions WHERE id=?`, sessionID).Scan(&deleted); errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	return putConversationSessionChangeTx(tx, sessionID, "archive_content_excluded", deleted)
}
