package db

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
)

// ParserCheckpointVersion is the codec version for parser_checkpoints rows.
// Bump it when the cursor, hash-state, or anchor encoding changes; a version
// mismatch makes the engine fall back to a full parse instead of resuming.
const ParserCheckpointVersion = 1

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

// keptUnder reports whether an archive with the given content policy may
// store the payload. Archives that omit tool content must not store
// transcript bytes. A crypto/sha256 state buffers the input after the last
// 64-byte block, so only a state saved at a block boundary qualifies. A Codex
// cursor holds session metadata, digests, and pending tool call IDs and names,
// which transcript archives already store but usage archives do not. Unknown
// encodings are refused.
func (b ParserCheckpointBlobs) keptUnder(content config.ArchiveContent) bool {
	if !content.OmitsToolContent() {
		return true
	}
	if len(b.Cursor) > 0 && (content.UsageOnly() || !parser.IsCodexCursor(b.Cursor)) {
		return false
	}
	aligned := blockAlignedSHA256State(b.HashState)
	return aligned != nil && bytes.Equal(aligned, b.HashState)
}

// sha256StateSize is the crypto/sha256 marshaled state size: "sha\x03",
// eight 32-bit chaining words, the block buffer, and the big-endian count of
// bytes hashed.
const sha256StateSize = 4 + 32 + sha256.BlockSize + 8

// SHA256StateOffset returns how many bytes a marshaled crypto/sha256 state
// has hashed. ok is false for an unknown encoding.
func SHA256StateOffset(state []byte) (n int64, ok bool) {
	if len(state) != sha256StateSize || string(state[:4]) != "sha\x03" {
		return 0, false
	}
	return int64(binary.BigEndian.Uint64(state[sha256StateSize-8:])), true
}

// blockAlignedSHA256State rewinds a marshaled crypto/sha256 state to its last
// 64-byte block boundary, where it buffers no input. The chaining values
// already cover every complete block, so it clears the buffer and rounds the
// count down. It returns nil for an unknown encoding.
func blockAlignedSHA256State(state []byte) []byte {
	n, ok := SHA256StateOffset(state)
	if !ok {
		return nil
	}
	aligned := slices.Clone(state)
	clear(aligned[36 : sha256StateSize-8])
	binary.BigEndian.PutUint64(aligned[sha256StateSize-8:], uint64(n-n%sha256.BlockSize))
	return aligned
}

// GetParserCheckpoint loads the checkpoint metadata for a session. ok=false
// means no row exists (legacy session or never checkpointed), which is not
// an error. The blob payload is deliberately not loaded here.
func (db *DB) GetParserCheckpoint(ctx context.Context,
	sessionID string,
) (*ParserCheckpoint, bool, error) {
	var cp ParserCheckpoint
	var inode, device, nextOrdinal int64
	err := db.getReader().QueryRow(ctx,
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
func (db *DB) GetParserCheckpointBlobs(ctx context.Context,
	sessionID string,
) (ParserCheckpointBlobs, bool, error) {
	var b ParserCheckpointBlobs
	err := db.getReader().QueryRow(ctx,
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
func (db *DB) DeleteParserCheckpoint(ctx context.Context, sessionID string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning checkpoint delete tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := deleteParserCheckpointTx(ctx, tx, sessionID); err != nil {
		return err
	}
	return tx.Commit()
}

func deleteParserCheckpointTx(ctx context.Context, tx *sql.Tx, sessionID string) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM parser_checkpoints WHERE session_id = ?`,
		sessionID,
	); err != nil {
		return fmt.Errorf("deleting parser checkpoint %s: %w", sessionID, err)
	}
	if _, err := tx.ExecContext(ctx,
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
func (db *DB) UpsertParserCheckpoint(ctx context.Context,
	cp ParserCheckpoint, blobs ParserCheckpointBlobs,
) error {
	blobs.SessionID = cp.SessionID
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning checkpoint upsert tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := db.upsertParserCheckpointTx(ctx, tx, cp, blobs); err != nil {
		return err
	}
	return tx.Commit()
}

// RecordSourceTouch stores a new modified time for a session whose source
// bytes are proven unchanged, so later passes skip it by stat alone. The
// checkpoint, when one exists, moves with the session row: the checkpoint gate
// rejects a checkpoint whose modified time differs from the row's.
func (db *DB) RecordSourceTouch(ctx context.Context,
	sessionID string, mtime, changeTime int64,
) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning source touch tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`UPDATE sessions SET file_mtime = ? WHERE id = ?`, mtime, sessionID,
	); err != nil {
		return fmt.Errorf("recording source touch %s: %w", sessionID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE parser_checkpoints SET file_mtime = ?, file_change_time = ?
		 WHERE session_id = ?`, mtime, changeTime, sessionID,
	); err != nil {
		return fmt.Errorf("recording checkpoint touch %s: %w", sessionID, err)
	}
	return tx.Commit()
}

// upsertParserCheckpointTx stores a checkpoint in the form the archive policy
// allows, or deletes the session's checkpoint when the policy allows none.
// Archives that omit tool content store the hash state at its last block
// boundary; a resume reads the offset from the state.
func (db *DB) upsertParserCheckpointTx(
	ctx context.Context, tx *sql.Tx, cp ParserCheckpoint, blobs ParserCheckpointBlobs,
) error {
	content := db.ArchiveContent()
	if content.OmitsToolContent() {
		blobs.HashState = blockAlignedSHA256State(blobs.HashState)
	}
	if !blobs.keptUnder(content) {
		return deleteParserCheckpointTx(ctx, tx, cp.SessionID)
	}
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
