package db

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ParserCheckpointVersion is the codec version for parser_checkpoints rows.
// Bump it when the cursor or hash-state encoding changes; a version mismatch
// makes the engine fall back to a full parse instead of resuming.
const ParserCheckpointVersion = 1

// ParserCheckpoint is the machine-local continuation state needed to resume
// parsing an append-only transcript without re-reading the committed prefix:
// the committed byte offset, a tail anchor proving the boundary region is
// unchanged, the provider cursor, and the resumable SHA-256 state.
type ParserCheckpoint struct {
	SessionID   string
	Agent       string
	FilePath    string
	FileInode   uint64
	FileDevice  uint64
	FileMTime   int64
	Offset      int64
	TailAnchor  []byte
	Cursor      []byte
	HashState   []byte
	Hash        string
	NextOrdinal int
	Version     int
	UpdatedAt   string
}

// GetParserCheckpoint loads the checkpoint for a session. ok=false means no
// row exists (legacy session or never checkpointed), which is not an error.
func (db *DB) GetParserCheckpoint(
	sessionID string,
) (*ParserCheckpoint, bool, error) {
	var cp ParserCheckpoint
	var inode, device, nextOrdinal int64
	err := db.getReader().QueryRow(
		`SELECT agent, file_path, file_inode, file_device, file_mtime,
		        offset, tail_anchor, cursor, hash_state, hash,
		        next_ordinal, checkpoint_version, updated_at
		 FROM parser_checkpoints
		 WHERE session_id = ?`,
		sessionID,
	).Scan(
		&cp.Agent, &cp.FilePath, &inode, &device, &cp.FileMTime,
		&cp.Offset, &cp.TailAnchor, &cp.Cursor, &cp.HashState, &cp.Hash,
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

// DeleteParserCheckpoint removes a session's checkpoint. Used when a source
// is replaced or deleted so a stale checkpoint can never be resumed.
func (db *DB) DeleteParserCheckpoint(sessionID string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	_, err := db.getWriter().Exec(
		`DELETE FROM parser_checkpoints WHERE session_id = ?`,
		sessionID,
	)
	if err != nil {
		return fmt.Errorf("deleting parser checkpoint %s: %w", sessionID, err)
	}
	return nil
}

// UpsertParserCheckpoint stores or replaces a session checkpoint. The full
// parse path calls this after its session rows commit; the incremental path
// writes the checkpoint inside the same transaction as the delta
// (see WriteSessionIncremental).
func (db *DB) UpsertParserCheckpoint(cp ParserCheckpoint) error {
	if cp.Version == 0 {
		cp.Version = ParserCheckpointVersion
	}
	if cp.UpdatedAt == "" {
		cp.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	return upsertParserCheckpointExec(db.getWriter(), cp)
}

func upsertParserCheckpointTx(tx *sql.Tx, cp ParserCheckpoint) error {
	return upsertParserCheckpointExec(tx, cp)
}

type sqlExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func upsertParserCheckpointExec(exec sqlExecer, cp ParserCheckpoint) error {
	if cp.Version == 0 {
		cp.Version = ParserCheckpointVersion
	}
	if cp.UpdatedAt == "" {
		cp.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	_, err := exec.Exec(
		`INSERT INTO parser_checkpoints (
		    session_id, agent, file_path, file_inode, file_device, file_mtime,
		    offset, tail_anchor, cursor, hash_state, hash,
		    next_ordinal, checkpoint_version, updated_at
		 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(session_id) DO UPDATE SET
		    agent = excluded.agent,
		    file_path = excluded.file_path,
		    file_inode = excluded.file_inode,
		    file_device = excluded.file_device,
		    file_mtime = excluded.file_mtime,
		    offset = excluded.offset,
		    tail_anchor = excluded.tail_anchor,
		    cursor = excluded.cursor,
		    hash_state = excluded.hash_state,
		    hash = excluded.hash,
		    next_ordinal = excluded.next_ordinal,
		    checkpoint_version = excluded.checkpoint_version,
		    updated_at = excluded.updated_at`,
		cp.SessionID, cp.Agent, cp.FilePath,
		int64(cp.FileInode), int64(cp.FileDevice), cp.FileMTime,
		cp.Offset, cp.TailAnchor, cp.Cursor, cp.HashState, cp.Hash,
		cp.NextOrdinal, cp.Version, cp.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf(
			"upserting parser checkpoint %s: %w", cp.SessionID, err,
		)
	}
	return nil
}
