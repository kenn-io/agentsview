package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"time"

	"go.kenn.io/docbank/document/ocr"
)

// ImageTextDataVersion is the first version that maintains image text.
const ImageTextDataVersion = 173

// MirrorDataVersion marks mirror rows carrying image text for older binaries.
func MirrorDataVersion(version int, hasImageText bool) int {
	if hasImageText {
		return max(version, ImageTextDataVersion)
	}
	return version
}

type ImageTextOccurrence struct {
	CallID, EventID int64
	Index           int
	Hash            string
}
type PendingImageText struct {
	Content         string
	CallID, EventID int64
	Index           int
}

// ImageTextRow is one successful extraction at a portable source location.
// SourceKind is "tool_call" for a call's result content and "event" for a
// tool result event; EventIndex is zero for tool calls.
type ImageTextRow struct {
	SessionID      string `json:"session_id"`
	SourceKind     string `json:"source_kind"`
	MessageOrdinal int    `json:"message_ordinal"`
	CallIndex      int    `json:"call_index"`
	EventIndex     int    `json:"event_index"`
	ImageIndex     int    `json:"image_index"`
	ImageHash      string `json:"image_hash"`
	Processor      string `json:"processor"`
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	Revision       string `json:"revision"`
	Text           string `json:"text"`
}

// PendingImageText lists hashes never attempted before due retries, oldest
// retry first, with the newest occurrence first among equally eligible images.
func (db *DB) PendingImageText(ctx context.Context, processor string, now time.Time, limit int) ([]string, error) {
	rows, err := db.getReader().QueryContext(ctx, `SELECT o.image_hash FROM image_text_occurrences o LEFT JOIN image_text_cache c ON c.image_hash=o.image_hash AND c.processor=?
 WHERE c.image_hash IS NULL OR (c.succeeded=0 AND c.retry_at<=?)
 GROUP BY o.image_hash ORDER BY MAX(c.image_hash IS NOT NULL), MAX(c.retry_at), MAX(o.rowid) DESC, o.image_hash LIMIT ?`, processor, now.UTC().Format(time.RFC3339), limit)
	if err != nil {
		return nil, err
	}
	return scanStrings(rows)
}

// ImageTextSources pages retained copies so a widely shared image stays bounded.
func (db *DB) ImageTextSources(ctx context.Context, hash string, offset, limit int) ([]PendingImageText, error) {
	rows, err := db.getReader().QueryContext(ctx, `SELECT COALESCE(e.content,tc.result_content,''),o.image_index,COALESCE(o.call_id,0),COALESCE(o.event_id,0)
 FROM image_text_occurrences o LEFT JOIN tool_calls tc ON tc.id=o.call_id LEFT JOIN tool_result_events e ON e.id=o.event_id
 WHERE o.image_hash=? ORDER BY o.session_id,o.call_id,o.event_id,o.image_index LIMIT ? OFFSET ?`, hash, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingImageText
	for rows.Next() {
		var p PendingImageText
		if err := rows.Scan(&p.Content, &p.Index, &p.CallID, &p.EventID); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (db *DB) requireImageTextStorage() error {
	if db.ArchiveContent().OmitsToolContent() {
		return fmt.Errorf("%w: image text requires tool result content", ErrArchiveContentExcluded)
	}
	return nil
}

func (db *DB) SaveImageText(ctx context.Context, hash string, identity ocr.Identity, text string, failure bool) error {
	if err := db.requireImageTextStorage(); err != nil {
		return err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if failure {
		_, err := db.getWriter().ExecContext(ctx, `INSERT INTO image_text_cache(id,image_hash,processor,provider,model,revision,failures,retry_at) SELECT (SELECT COALESCE(MIN(id),0)-1 FROM image_text_cache),?,?,?,?,?,1,? WHERE EXISTS (SELECT 1 FROM image_text_occurrences WHERE image_hash=?)
			ON CONFLICT(image_hash,processor) DO UPDATE SET failures=failures+1, retry_at=strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '+' || min(3600, 60 * (1 << min(failures, 6))) || ' seconds')`, hash, identity.Fingerprint, identity.Provider, identity.Model, identity.Revision, time.Now().UTC().Add(time.Minute).Format(time.RFC3339), hash)
		return err
	}
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `INSERT INTO image_text_cache(id,image_hash,processor,provider,model,revision,text,succeeded,updated_at) SELECT (SELECT COALESCE(MIN(id),0)-1 FROM image_text_cache),?,?,?,?,?,?,1,strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE EXISTS (SELECT 1 FROM image_text_occurrences WHERE image_hash=?)
		ON CONFLICT(image_hash,processor) DO UPDATE SET text=excluded.text,succeeded=1,failures=0,retry_at='',updated_at=excluded.updated_at`, hash, identity.Fingerprint, identity.Provider, identity.Model, identity.Revision, text, hash)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM image_text_cache WHERE image_hash=? AND processor<>? AND EXISTS (SELECT 1 FROM image_text_cache WHERE image_hash=? AND processor=? AND succeeded=1)`, hash, identity.Fingerprint, hash, identity.Fingerprint); err != nil {
		return err
	}
	// Advance the mirror cursor without invalidating Recall's content stamp.
	if _, err = tx.ExecContext(ctx, `UPDATE sessions
		SET sync_marker = MAX(COALESCE(sync_marker, ''), strftime('%Y-%m-%dT%H:%M:%fZ','now'))
		WHERE id IN (SELECT session_id FROM image_text_occurrences WHERE image_hash=?)`, hash); err != nil {
		return fmt.Errorf("marking image text sessions modified: %w", err)
	}
	return tx.Commit()
}

// ClearImageText removes the active index and published matches. Cached text
// is removed when image text is disabled.
func (db *DB) ClearImageText(ctx context.Context) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `UPDATE sessions SET sync_marker=MAX(COALESCE(sync_marker,''),strftime('%Y-%m-%dT%H:%M:%fZ','now')) WHERE id IN (SELECT DISTINCT o.session_id FROM image_text_occurrences o JOIN image_text_cache c ON c.image_hash=o.image_hash WHERE c.succeeded=1 AND c.text<>'')`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM image_text_occurrences`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM stats WHERE key=?`, imageTextBackfillKey); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM image_text_cache`); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) CopyImageTextFrom(ctx context.Context, sourcePath string) (bool, error) {
	if err := db.requireImageTextStorage(); err != nil {
		return false, err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	conn, err := db.getWriter().Conn(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, `ATTACH DATABASE ? AS old_db`, sourcePath); err != nil {
		return false, err
	}
	defer func() { _, _ = execWithoutCancel(ctx, conn, `DETACH DATABASE old_db`) }()
	var enabled bool
	if err := conn.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM old_db.stats WHERE key=? AND value!=0)`, imageTextBackfillKey).Scan(&enabled); err != nil {
		return false, err
	}
	if _, err = conn.ExecContext(ctx, `INSERT OR IGNORE INTO image_text_cache(id,image_hash,processor,provider,model,revision,text,succeeded,failures,retry_at,updated_at) SELECT (SELECT COALESCE(MIN(id),0) FROM main.image_text_cache)-ROW_NUMBER() OVER (),image_hash,processor,provider,model,revision,text,succeeded,failures,retry_at,updated_at FROM old_db.image_text_cache c`); err != nil {
		return false, fmt.Errorf("copy image text: %w", err)
	}
	return enabled, nil
}

func (db *DB) SessionImageTextRows(ctx context.Context, id string) ([]ImageTextRow, error) {
	rows, err := db.SessionImageTextRowsBySession(ctx, []string{id})
	return rows[id], err
}

// SessionImageTextRowsBySession reads mirror image text in one archive snapshot.
func (db *DB) SessionImageTextRowsBySession(ctx context.Context, ids []string) (map[string][]ImageTextRow, error) {
	tx, err := db.getReader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := sessionImageTextRows(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]ImageTextRow)
	for _, row := range rows {
		out[row.SessionID] = append(out[row.SessionID], row)
	}
	return out, tx.Commit()
}

func sessionImageTextRows(ctx context.Context, q messageRowsQuerier, ids []string) ([]ImageTextRow, error) {
	selected, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `SELECT o.session_id,
		CASE WHEN o.event_id IS NULL THEN 'tool_call' ELSE 'event' END,
		COALESCE(e.tool_call_message_ordinal,m.ordinal),COALESCE(e.call_index,tc.call_index,0),COALESCE(e.event_index,0),
		o.image_index,o.image_hash,c.processor,c.provider,c.model,c.revision,c.text
		FROM image_text_occurrences o
		JOIN image_text_cache c ON c.image_hash=o.image_hash AND c.succeeded=1 AND c.text<>''
		LEFT JOIN tool_result_events e ON e.id=o.event_id
		LEFT JOIN tool_calls tc ON tc.id=o.call_id
		LEFT JOIN messages m ON m.id=tc.message_id
		WHERE o.session_id IN (SELECT value FROM json_each(?))
		  AND (e.id IS NOT NULL OR m.id IS NOT NULL)
		ORDER BY 1,2,3,4,5,6,8`, string(selected))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ImageTextRow
	for rows.Next() {
		var r ImageTextRow
		if err := rows.Scan(&r.SessionID, &r.SourceKind, &r.MessageOrdinal, &r.CallIndex, &r.EventIndex,
			&r.ImageIndex, &r.ImageHash, &r.Processor, &r.Provider, &r.Model, &r.Revision, &r.Text); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
