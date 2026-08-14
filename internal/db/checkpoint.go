package db

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ParserCheckpointVersion is the codec version for parser_checkpoints rows.
// Bump it when the cursor, hash-state, or anchor encoding changes; a version
// mismatch makes the engine fall back to a full parse instead of resuming.
const ParserCheckpointVersion = 2

// ParserCheckpoint is the machine-local continuation metadata needed to
// resume parsing an append-only transcript without re-reading the committed
// prefix: the committed byte offset, the digest of the tail anchor window
// proving the boundary region is unchanged, and the committed-prefix hash.
// The heavier payload (cursor and resumable hash state) lives in
// ParserCheckpointBlobs so the stat-only freshness gate never reads it.
type ParserCheckpoint struct {
	SessionID        string
	Agent            string
	FilePath         string
	FileInode        uint64
	FileDevice       uint64
	FileMTime        int64
	FileChangeTime   int64
	Offset           int64
	TailAnchorDigest string
	Hash             string
	NextOrdinal      int
	Version          int
	UpdatedAt        string
}

// ParserCheckpointBlobs is the lazy-loaded checkpoint payload.
type ParserCheckpointBlobs struct {
	SessionID string
	Cursor    []byte
	HashState []byte
}

// GetParserCheckpoint loads the checkpoint metadata for a session. ok=false
// means no row exists (legacy session or never checkpointed), which is not
// an error. The blob payload is deliberately not loaded here.
func (db *DB) GetParserCheckpoint(
	sessionID string,
) (*ParserCheckpoint, bool, error) {
	var cp ParserCheckpoint
	var inode, device, nextOrdinal int64
	err := db.getReader().QueryRow(
		`SELECT agent, file_path, file_inode, file_device, file_mtime,
		        file_change_time,
		        offset, tail_anchor_digest, hash,
		        next_ordinal, checkpoint_version, updated_at
		 FROM parser_checkpoints
		 WHERE session_id = ?`,
		sessionID,
	).Scan(
		&cp.Agent, &cp.FilePath, &inode, &device, &cp.FileMTime,
		&cp.FileChangeTime,
		&cp.Offset, &cp.TailAnchorDigest, &cp.Hash,
		&nextOrdinal, &cp.Version, &cp.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf(
			"reading parser checkpoint %s: %w", sessionID, err,
		)
	}
	cp.SessionID = sessionID
	cp.FileInode = uint64(inode)
	cp.FileDevice = uint64(device)
	cp.NextOrdinal = int(nextOrdinal)
	return &cp, true, nil
}

// GetParserCheckpointBlobs loads the lazy checkpoint payload. ok=false when
// no blobs row exists.
func (db *DB) GetParserCheckpointBlobs(
	sessionID string,
) (ParserCheckpointBlobs, bool, error) {
	var b ParserCheckpointBlobs
	err := db.getReader().QueryRow(
		`SELECT cursor, hash_state
		 FROM parser_checkpoint_blobs
		 WHERE session_id = ?`,
		sessionID,
	).Scan(&b.Cursor, &b.HashState)
	if errors.Is(err, sql.ErrNoRows) {
		return ParserCheckpointBlobs{}, false, nil
	}
	if err != nil {
		return ParserCheckpointBlobs{}, false, fmt.Errorf(
			"reading parser checkpoint blobs %s: %w", sessionID, err,
		)
	}
	b.SessionID = sessionID
	return b, true, nil
}

// DeleteParserCheckpoint removes a session's checkpoint rows. Used when a
// source is replaced or deleted so a stale checkpoint can never be resumed.
func (db *DB) DeleteParserCheckpoint(sessionID string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().Begin()
	if err != nil {
		return fmt.Errorf("beginning checkpoint delete tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := deleteParserCheckpointTx(tx, sessionID); err != nil {
		return err
	}
	return tx.Commit()
}

func deleteParserCheckpointTx(tx *sql.Tx, sessionID string) error {
	if _, err := tx.Exec(
		`DELETE FROM parser_checkpoints WHERE session_id = ?`,
		sessionID,
	); err != nil {
		return fmt.Errorf("deleting parser checkpoint %s: %w", sessionID, err)
	}
	if _, err := tx.Exec(
		`DELETE FROM parser_checkpoint_blobs WHERE session_id = ?`,
		sessionID,
	); err != nil {
		return fmt.Errorf(
			"deleting parser checkpoint blobs %s: %w", sessionID, err,
		)
	}
	return nil
}

// UpsertParserCheckpoint stores or replaces a session checkpoint (metadata
// plus blobs) atomically. The full parse path calls this after its session
// rows commit; the incremental path writes the checkpoint inside the same
// transaction as the delta (see WriteSessionIncremental).
func (db *DB) UpsertParserCheckpoint(
	cp ParserCheckpoint, blobs ParserCheckpointBlobs,
) error {
	if cp.Version == 0 {
		cp.Version = ParserCheckpointVersion
	}
	if cp.UpdatedAt == "" {
		cp.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	blobs.SessionID = cp.SessionID
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().Begin()
	if err != nil {
		return fmt.Errorf("beginning checkpoint upsert tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := upsertParserCheckpointExec(tx, cp, blobs); err != nil {
		return err
	}
	return tx.Commit()
}

func upsertParserCheckpointTx(
	tx *sql.Tx, cp ParserCheckpoint, blobs ParserCheckpointBlobs,
) error {
	return upsertParserCheckpointExec(tx, cp, blobs)
}

type sqlExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func upsertParserCheckpointExec(
	exec sqlExecer, cp ParserCheckpoint, blobs ParserCheckpointBlobs,
) error {
	if cp.Version == 0 {
		cp.Version = ParserCheckpointVersion
	}
	if cp.UpdatedAt == "" {
		cp.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if _, err := exec.Exec(
		`INSERT INTO parser_checkpoints (
		    session_id, agent, file_path, file_inode, file_device, file_mtime,
		    file_change_time,
		    offset, tail_anchor_digest, hash,
		    next_ordinal, checkpoint_version, updated_at
		 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(session_id) DO UPDATE SET
		    agent = excluded.agent,
		    file_path = excluded.file_path,
		    file_inode = excluded.file_inode,
		    file_device = excluded.file_device,
		    file_mtime = excluded.file_mtime,
		    file_change_time = excluded.file_change_time,
		    offset = excluded.offset,
		    tail_anchor_digest = excluded.tail_anchor_digest,
		    hash = excluded.hash,
		    next_ordinal = excluded.next_ordinal,
		    checkpoint_version = excluded.checkpoint_version,
		    updated_at = excluded.updated_at`,
		cp.SessionID, cp.Agent, cp.FilePath,
		int64(cp.FileInode), int64(cp.FileDevice), cp.FileMTime,
		cp.FileChangeTime,
		cp.Offset, cp.TailAnchorDigest, cp.Hash,
		cp.NextOrdinal, cp.Version, cp.UpdatedAt,
	); err != nil {
		return fmt.Errorf(
			"upserting parser checkpoint %s: %w", cp.SessionID, err,
		)
	}
	if _, err := exec.Exec(
		`INSERT INTO parser_checkpoint_blobs (session_id, cursor, hash_state)
		 VALUES (?, ?, ?)
		 ON CONFLICT(session_id) DO UPDATE SET
		    cursor = excluded.cursor,
		    hash_state = excluded.hash_state`,
		cp.SessionID, blobs.Cursor, blobs.HashState,
	); err != nil {
		return fmt.Errorf(
			"upserting parser checkpoint blobs %s: %w", cp.SessionID, err,
		)
	}
	return nil
}
