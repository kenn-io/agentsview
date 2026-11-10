package db

import (
	"context"
	"fmt"
	"time"

	"go.kenn.io/docbank/document/ocr"
)

type ImageTextOccurrence struct {
	CallID, EventID int64
	Index           int
	Hash            string
	MediaType       string
}
type PendingImageText struct {
	Content string
	Index   int
	Hash    string
}

func (db *DB) PendingImageText(ctx context.Context, processor string, now time.Time, limit int) ([]string, error) {
	rows, err := db.getReader().QueryContext(ctx, `SELECT o.image_hash FROM image_text_occurrences o LEFT JOIN image_text_cache c ON c.image_hash=o.image_hash AND c.processor=?
 WHERE c.image_hash IS NULL OR (c.succeeded=0 AND c.retry_at<=?)
 GROUP BY o.image_hash ORDER BY o.image_hash LIMIT ?`, processor, now.UTC().Format(time.RFC3339), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, err
		}
		out = append(out, hash)
	}
	return out, rows.Err()
}

// ImageTextSources pages retained copies so a widely shared image stays bounded.
func (db *DB) ImageTextSources(ctx context.Context, hash string, offset, limit int) ([]PendingImageText, error) {
	rows, err := db.getReader().QueryContext(ctx, `SELECT COALESCE(e.content,tc.result_content,''),o.image_index,o.image_hash
 FROM image_text_occurrences o LEFT JOIN tool_calls tc ON tc.id=o.call_id LEFT JOIN tool_result_events e ON e.id=o.event_id
 WHERE o.image_hash=? ORDER BY o.session_id,o.call_id,o.event_id,o.image_index LIMIT ? OFFSET ?`, hash, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingImageText
	for rows.Next() {
		var p PendingImageText
		if err := rows.Scan(&p.Content, &p.Index, &p.Hash); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (db *DB) SaveImageText(ctx context.Context, hash string, identity ocr.Identity, text string, failure bool) error {
	if err := db.requireDerivedTextStorage("image text"); err != nil {
		return err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if failure {
		_, err := db.getWriter().ExecContext(ctx, `INSERT INTO image_text_cache(image_hash,processor,provider,model,revision,failures,retry_at) SELECT ?,?,?,?,?,1,? WHERE EXISTS (SELECT 1 FROM image_text_occurrences WHERE image_hash=?)
			ON CONFLICT(image_hash,processor) DO UPDATE SET failures=failures+1, retry_at=strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '+' || min(3600, 60 * (1 << min(failures, 6))) || ' seconds')`, hash, identity.Fingerprint, identity.Provider, identity.Model, identity.Revision, time.Now().UTC().Add(time.Minute).Format(time.RFC3339), hash)
		return err
	}
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO image_text_cache(image_hash,processor,provider,model,revision,text,succeeded,updated_at) SELECT ?,?,?,?,?,?,1,strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE EXISTS (SELECT 1 FROM image_text_occurrences WHERE image_hash=?)
		ON CONFLICT(image_hash,processor) DO UPDATE SET text=excluded.text,succeeded=1,failures=0,retry_at='',updated_at=excluded.updated_at`, hash, identity.Fingerprint, identity.Provider, identity.Model, identity.Revision, text, hash)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM image_text_cache WHERE image_hash=? AND processor<>? AND EXISTS (SELECT 1 FROM image_text_cache WHERE image_hash=? AND processor=? AND succeeded=1)`, hash, identity.Fingerprint, hash, identity.Fingerprint); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) SaveImageTextTerminal(ctx context.Context, hash string, identity ocr.Identity, inspectedTypes map[string]bool) error {
	if err := db.requireDerivedTextStorage("image text"); err != nil {
		return err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	rows, err := db.getWriter().QueryContext(ctx, `SELECT DISTINCT media_type FROM image_text_occurrences WHERE image_hash=?`, hash)
	if err != nil {
		return err
	}
	for rows.Next() {
		var mediaType string
		if err := rows.Scan(&mediaType); err != nil {
			rows.Close()
			return err
		}
		if !inspectedTypes[mediaType] {
			return rows.Close()
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	_, err = db.getWriter().ExecContext(ctx, `INSERT INTO image_text_cache(image_hash,processor,provider,model,revision,failures,retry_at)
 SELECT ?,?,?,?,?,1,'9999-12-31T23:59:59Z' WHERE EXISTS (SELECT 1 FROM image_text_occurrences WHERE image_hash=?)
 ON CONFLICT(image_hash,processor) DO UPDATE SET retry_at=excluded.retry_at`, hash, identity.Fingerprint, identity.Provider, identity.Model, identity.Revision, hash)
	return err
}

func (db *DB) CopyImageTextFrom(ctx context.Context, sourcePath string) error {
	if err := db.requireDerivedTextStorage("image text"); err != nil {
		return err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	conn, err := db.getWriter().Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, `ATTACH DATABASE ? AS old_db`, sourcePath); err != nil {
		return err
	}
	defer func() { _, _ = execWithoutCancel(ctx, conn, `DETACH DATABASE old_db`) }()
	if _, err = conn.ExecContext(ctx, `INSERT OR IGNORE INTO image_text_cache(image_hash,processor,provider,model,revision,text,succeeded,failures,retry_at,updated_at) SELECT image_hash,processor,provider,model,revision,text,succeeded,failures,retry_at,updated_at FROM old_db.image_text_cache c WHERE retry_at<>'9999-12-31T23:59:59Z' AND EXISTS (SELECT 1 FROM image_text_occurrences o WHERE o.image_hash=c.image_hash)`); err != nil {
		return fmt.Errorf("copy image text: %w", err)
	}
	return nil
}
