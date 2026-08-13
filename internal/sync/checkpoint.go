package sync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

const (
	codexCheckpointVersion    = 1
	codexCheckpointAnchorSize = 128 << 10
)

type codexCheckpointDecision int

const (
	// codexCheckpointFallback means the checkpoint cannot prove a safe
	// resume; the caller uses the existing fingerprint/full-parse path.
	codexCheckpointFallback codexCheckpointDecision = iota
	// codexCheckpointUnchanged means stat + checkpoint prove the transcript
	// is byte-identical to the committed prefix; the caller skips without
	// hashing anything.
	codexCheckpointUnchanged
	// codexCheckpointAppend means the checkpoint proves a safe append-only
	// growth; the caller parses only the new tail and resumes the hash.
	codexCheckpointAppend
	// codexCheckpointInvalid means a checkpoint exists but its proof failed
	// (identity changed, truncation, anchor mismatch, missing hash state).
	// The caller must authoritatively reparse and replace stored rows —
	// never resume and never append against the unverified prefix.
	codexCheckpointInvalid
)

type codexCheckpointResult struct {
	fingerprint parser.SourceFingerprint
	checkpoint  *db.ParserCheckpoint
	decision    codexCheckpointDecision
	// hashState is the resumable SHA-256 state after hashing through the
	// current file size, ready for the next checkpoint.
	hashState []byte
}

// codexCheckpointFingerprint tries to resolve a Codex source fingerprint and
// its persisted checkpoint without reading the transcript prefix:
//
//   - unchanged: stat identity + size match the checkpoint offset, so the
//     committed prefix is trusted (append-trust mode) and the file is skipped;
//   - append: identity matches, the file only grew, the tail anchor matches,
//     and a resumable SHA-256 state exists, so the full-file fingerprint is
//     derived by hashing only [offset, size);
//   - otherwise fallback, which keeps every existing conservative path.
func (e *Engine) codexCheckpointFingerprint(
	ctx context.Context,
	source parser.SourceRef,
	file parser.DiscoveredFile,
) (codexCheckpointResult, error) {
	res := codexCheckpointResult{decision: codexCheckpointFallback}
	if e.forceParse || file.ForceParse || e.checkpointAudit.Load() {
		// Audit mode deliberately bypasses the checkpoint gate so the
		// provider's full-source fingerprint can verify content and repair
		// same-stat in-place rewrites that append-trust would otherwise miss.
		return res, nil
	}
	path := providerDiscoveredPath(source)
	if path == "" {
		return res, nil
	}
	lookupPath := path
	if e.pathRewriter != nil {
		lookupPath = e.pathRewriter(path)
	}
	// Resolve the session through the DB so codex-format forks (TraeX)
	// use their real session id prefix, and use the same row to validate
	// the checkpoint against the committed offset/ordinal/hash. A
	// checkpoint that disagrees with the DB (e.g. a crash after a full
	// replacement committed but before its checkpoint upsert) must never
	// seed a resume: mark it invalid so the caller rebuilds
	// authoritatively.
	inc, ok := e.db.GetSessionForIncremental(
		lookupPath, string(file.Agent),
	)
	if !ok {
		return res, nil
	}
	cp, hasCP, err := e.db.GetParserCheckpoint(inc.ID)
	if err != nil {
		return res, fmt.Errorf("loading checkpoint %s: %w", inc.ID, err)
	}
	if !hasCP || cp.Version != codexCheckpointVersion {
		return res, nil
	}
	if cp.SessionID != inc.ID ||
		cp.FilePath != e.effectiveSourcePath(path) ||
		cp.Agent != string(file.Agent) {
		return res, nil
	}
	storedHash, hasStoredHash := e.db.GetFileHashByAgentPath(
		lookupPath, string(file.Agent),
	)
	if !hasStoredHash || storedHash != cp.Hash ||
		inc.FileSize != cp.Offset || inc.NextOrdinal != cp.NextOrdinal {
		// The committed DB prefix is newer than (or inconsistent with) the
		// surviving checkpoint: resuming from the old seed would silently
		// parse against the wrong prefix. Rebuild instead.
		res.decision = codexCheckpointInvalid
		return res, nil
	}
	if e.db.GetDataVersionByAgentPath(lookupPath, string(file.Agent)) <
		db.CurrentDataVersion() {
		return res, nil
	}
	if e.pathNeedsProjectReparse(file.Agent, path) {
		return res, nil
	}
	if file.Agent == parser.AgentCodex && e.codexIndexSessionNameChanged(path) {
		return res, nil
	}

	info, err := os.Stat(path)
	if err != nil {
		return res, nil // missing/raced source: existing path handles it
	}
	inode, device := getFileIdentity(path, info)
	if inode != int64(cp.FileInode) || device != int64(cp.FileDevice) {
		res.decision = codexCheckpointInvalid
		return res, nil
	}
	rawMtime := info.ModTime().UnixNano()
	effectiveMtime := rawMtime
	if file.Agent == parser.AgentCodex {
		effectiveMtime = parser.CodexEffectiveMtime(path, rawMtime)
	}

	if info.Size() == cp.Offset {
		// The mtime gate applies only to the unchanged branch: an append
		// naturally moves the mtime, and the append branch is proven by
		// identity + tail anchor + monotonic size instead. An index-only
		// mtime bump (transcript mtime unchanged) is safe to skip when the
		// title did not change, mirroring shouldSkipCodexFingerprint.
		indexOnlyBump := effectiveMtime != cp.FileMTime &&
			rawMtime <= cp.FileMTime
		if effectiveMtime != cp.FileMTime && !indexOnlyBump {
			return res, nil
		}
		res.decision = codexCheckpointUnchanged
		res.fingerprint = parser.SourceFingerprint{
			Key:     codexCheckpointFingerprintKey(source, path),
			Size:    info.Size(),
			MTimeNS: effectiveMtime,
			Inode:   uint64(inode),
			Device:  uint64(device),
			Hash:    cp.Hash,
		}
		return res, nil
	}
	if info.Size() < cp.Offset {
		res.decision = codexCheckpointInvalid // truncation: never resume
		return res, nil
	}
	if len(cp.HashState) == 0 || len(cp.TailAnchor) == 0 {
		res.decision = codexCheckpointInvalid
		return res, nil
	}
	matches, err := codexCheckpointAnchorMatches(path, cp)
	if err != nil || !matches {
		res.decision = codexCheckpointInvalid
		return res, nil
	}
	_, hash, err := codexResumeHash(
		path, cp.Offset, info.Size(), cp.HashState,
	)
	if err != nil {
		res.decision = codexCheckpointInvalid
		return res, nil
	}
	res.decision = codexCheckpointAppend
	res.checkpoint = cp
	// The incremental path resumes from the OLD state through the committed
	// safe offset (which may stop before a partial tail at EOF); the
	// full-file hash above is only the fingerprint. Advancing the state here
	// would double-count the tail when newOffset < info.Size().
	res.hashState = cp.HashState
	res.fingerprint = parser.SourceFingerprint{
		Key:     codexCheckpointFingerprintKey(source, path),
		Size:    info.Size(),
		MTimeNS: effectiveMtime,
		Inode:   uint64(inode),
		Device:  uint64(device),
		Hash:    hash,
	}
	return res, nil
}

func codexCheckpointFingerprintKey(
	source parser.SourceRef, path string,
) string {
	for _, candidate := range []string{
		source.FingerprintKey, source.Key,
	} {
		if candidate != "" {
			return candidate
		}
	}
	return path
}

// codexCheckpointTailAnchor returns the last min(anchorSize, offset) bytes of
// the committed prefix [0, offset).
func codexCheckpointTailAnchor(
	path string, offset int64,
) ([]byte, error) {
	if offset <= 0 {
		return nil, nil
	}
	start := offset - codexCheckpointAnchorSize
	if start < 0 {
		start = 0
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	anchor, err := io.ReadAll(io.LimitReader(f, offset-start))
	if err != nil {
		return nil, err
	}
	if int64(len(anchor)) != offset-start {
		return nil, fmt.Errorf(
			"short anchor read for %s: got %d want %d",
			path, len(anchor), offset-start,
		)
	}
	return anchor, nil
}

func codexCheckpointAnchorMatches(
	path string, cp *db.ParserCheckpoint,
) (bool, error) {
	anchor, err := codexCheckpointTailAnchor(path, cp.Offset)
	if err != nil {
		return false, err
	}
	return bytes.Equal(anchor, cp.TailAnchor), nil
}

// codexResumeHash continues a persisted SHA-256 state over [offset, size) and
// returns the new state plus the full-file digest.
func codexResumeHash(
	path string, offset, size int64, state []byte,
) ([]byte, string, error) {
	h := sha256.New()
	unmarshaler, ok := h.(encoding.BinaryUnmarshaler)
	if !ok {
		return nil, "", fmt.Errorf("sha256 does not support state restore")
	}
	if err := unmarshaler.UnmarshalBinary(state); err != nil {
		return nil, "", fmt.Errorf("restoring hash state: %w", err)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, "", err
	}
	if _, err := io.CopyN(h, f, size-offset); err != nil {
		return nil, "", fmt.Errorf(
			"hashing appended bytes %d..%d of %s: %w",
			offset, size, path, err,
		)
	}
	newState, err := h.(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil {
		return nil, "", fmt.Errorf("marshaling hash state: %w", err)
	}
	return newState, hex.EncodeToString(h.Sum(nil)), nil
}

// codexBuildInitialHashState hashes the whole file once and returns the
// resumable SHA-256 state. Called when persisting a checkpoint after a full
// parse; the full parse already read the file, so this is the one-time cost
// that makes every later append Θ(d).
func codexBuildInitialHashState(
	path string, size int64,
) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyN(h, f, size); err != nil {
		return nil, fmt.Errorf(
			"hashing full source %s: %w", path, err,
		)
	}
	marshaler, ok := h.(encoding.BinaryMarshaler)
	if !ok {
		return nil, fmt.Errorf("sha256 does not support state capture")
	}
	return marshaler.MarshalBinary()
}

// codexHashStateDigest finalizes a resumable SHA-256 state into its digest.
func codexHashStateDigest(state []byte) (string, error) {
	h := sha256.New()
	unmarshaler, ok := h.(encoding.BinaryUnmarshaler)
	if !ok {
		return "", fmt.Errorf("sha256 does not support state restore")
	}
	if err := unmarshaler.UnmarshalBinary(state); err != nil {
		return "", fmt.Errorf("restoring hash state: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// persistFullParseCheckpoint stores a session's checkpoint after its full
// parse rows committed. It re-reads the committed file size and next ordinal
// from the database so the checkpoint always matches what a future
// incremental parse will see.
func (e *Engine) persistFullParseCheckpoint(
	ctx context.Context, pw pendingWrite,
) {
	if len(pw.checkpoint) == 0 {
		return
	}
	path := pw.sess.File.Path
	if path == "" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		log.Printf("checkpoint stat %s: %v", path, err)
		return
	}
	mtime := info.ModTime().UnixNano()
	if pw.sess.Agent == parser.AgentCodex {
		mtime = parser.CodexEffectiveMtime(path, mtime)
	}
	lookupPath := path
	if e.pathRewriter != nil {
		lookupPath = e.pathRewriter(path)
	}
	inc, ok := e.db.GetSessionForIncremental(
		lookupPath, string(pw.sess.Agent),
	)
	if !ok {
		// The session row committed but has no incremental bookkeeping
		// (e.g. zero messages); do not persist a resume state that could
		// disagree with the stored cursor.
		return
	}
	// The checkpoint must describe exactly the committed prefix: hash only
	// [0, inc.FileSize), never the live file's current size. If the
	// transcript kept growing while the full parse wrote, hashing
	// info.Size() would poison the resumable state with bytes that the next
	// incremental resume would hash a second time.
	committed := inc.FileSize
	if committed <= 0 || committed > info.Size() {
		log.Printf("checkpoint bounds %s: committed=%d live=%d", path, committed, info.Size())
		return
	}
	hashState, err := codexBuildInitialHashState(path, committed)
	if err != nil {
		log.Printf("checkpoint hash %s: %v", path, err)
		return
	}
	committedHash, err := codexHashStateDigest(hashState)
	if err != nil {
		log.Printf("checkpoint digest %s: %v", path, err)
		return
	}
	cp, buildErr := buildCodexCheckpoint(
		inc.ID,
		string(pw.sess.Agent),
		e.effectiveSourcePath(path),
		info,
		committed,
		mtime,
		pw.checkpoint,
		hashState,
		committedHash,
		inc.NextOrdinal,
	)
	if buildErr != nil {
		log.Printf("checkpoint build %s: %v", path, buildErr)
		return
	}
	if err := e.db.UpsertParserCheckpoint(*cp); err != nil {
		log.Printf("checkpoint persist %s: %v", path, err)
	}
}

// buildCodexCheckpoint constructs the next checkpoint for a committed prefix
// of size newOffset with the given cursor/hash continuation state.
func buildCodexCheckpoint(
	sessionID, agent, storedPath string,
	info os.FileInfo,
	newOffset int64,
	mtime int64,
	cursor []byte,
	hashState []byte,
	hash string,
	nextOrdinal int,
) (*db.ParserCheckpoint, error) {
	anchor, err := codexCheckpointTailAnchor(storedPath, newOffset)
	if err != nil {
		return nil, fmt.Errorf(
			"building checkpoint anchor %s at %d: %w",
			storedPath, newOffset, err,
		)
	}
	inode, device := getFileIdentity(storedPath, info)
	return &db.ParserCheckpoint{
		SessionID:   sessionID,
		Agent:       agent,
		FilePath:    storedPath,
		FileInode:   uint64(inode),
		FileDevice:  uint64(device),
		FileMTime:   mtime,
		Offset:      newOffset,
		TailAnchor:  anchor,
		Cursor:      cursor,
		HashState:   hashState,
		Hash:        hash,
		NextOrdinal: nextOrdinal,
		Version:     codexCheckpointVersion,
	}, nil
}
