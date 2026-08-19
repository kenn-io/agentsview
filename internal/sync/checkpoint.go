package sync

import (
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"fmt"
	"io"
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
	// codexCheckpointMissing means a stored Codex session has no usable
	// checkpoint (for example, an archive written before checkpoints
	// existed). Missing optimization state must not turn an unchanged
	// archive into a migration workload. The caller keeps the existing
	// freshness gates and creates a checkpoint on the next real source
	// change that already requires an authoritative parse.
	codexCheckpointMissing
)

type codexCheckpointResult struct {
	fingerprint parser.SourceFingerprint
	checkpoint  *db.ParserCheckpoint
	decision    codexCheckpointDecision
	// seed is the persisted parser cursor for the append resume; loaded
	// lazily from the checkpoint blobs only on the append branch.
	seed []byte
	// hashState is the resumable SHA-256 state through the committed
	// prefix, ready for the incremental resume to continue from.
	hashState []byte
}

// codexCheckpointFingerprint tries to resolve a Codex source fingerprint and
// its persisted checkpoint without reading the transcript prefix:
//
//   - unchanged: stat identity + size match the checkpoint offset, so the
//     committed prefix is trusted (append-trust mode) and the file is skipped;
//   - append: identity matches, the file only grew, the tail anchor digest
//     matches, and a resumable SHA-256 state exists, so the full-file
//     fingerprint is derived by hashing only [offset, size);
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
		res.decision = codexCheckpointMissing
		return res, nil
	}
	if e.pathRewriter != nil {
		// Remote materializations have no trustworthy local identity:
		// the checkpoint gate cannot stat-trust a rewritten path, so the
		// caller deep-verifies on every pass.
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
		inc.FileSize != cp.Offset || inc.NextOrdinal != cp.NextOrdinal ||
		inc.FileMtime != cp.FileMTime {
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
		// Change-time guards same-size same-mtime rewrites for free: a
		// write restores mtime but cannot restore ctime, so a mismatch
		// here proves the bytes changed even when every other identity
		// field matches. Rows without a stored change time (0) stay
		// conservative and rebuild.
		if changeTime, ok := fileChangeTime(path, info); !ok ||
			cp.FileChangeTime == 0 || changeTime != cp.FileChangeTime {
			return res, nil
		}
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
	if cp.TailAnchorDigest == "" {
		res.decision = codexCheckpointInvalid
		return res, nil
	}
	matches, err := codexCheckpointAnchorMatches(path, cp)
	if err != nil || !matches {
		res.decision = codexCheckpointInvalid
		return res, nil
	}
	// The append branch loads the lazy payload (cursor + hash state); the
	// unchanged branch above never touches it.
	blobs, hasBlobs, err := e.db.GetParserCheckpointBlobs(inc.ID)
	if err != nil {
		return res, fmt.Errorf("loading checkpoint blobs %s: %w", inc.ID, err)
	}
	if !hasBlobs || len(blobs.HashState) == 0 {
		res.decision = codexCheckpointInvalid
		return res, nil
	}
	stateDigest, err := codexHashStateDigest(blobs.HashState)
	if err != nil || stateDigest != cp.Hash {
		res.decision = codexCheckpointInvalid
		return res, nil
	}
	_, hash, err := codexResumeHash(
		path, cp.Offset, info.Size(), blobs.HashState,
	)
	if err != nil {
		res.decision = codexCheckpointInvalid
		return res, nil
	}
	res.decision = codexCheckpointAppend
	res.checkpoint = cp
	res.seed = blobs.Cursor
	// The incremental path resumes from the OLD state through the committed
	// safe offset (which may stop before a partial tail at EOF); the
	// full-file hash above is only the fingerprint. Advancing the state here
	// would double-count the tail when newOffset < info.Size().
	res.hashState = blobs.HashState
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

// codexCheckpointAnchorDigest returns the SHA-256 digest of the last
// min(codexCheckpointAnchorSize, offset) bytes of the committed prefix
// [0, offset). The append gate reads that bounded window once and compares
// the digest, instead of storing the raw anchor bytes in the checkpoint row.
func codexCheckpointAnchorDigest(
	path string, offset int64,
) (string, error) {
	if offset <= 0 {
		return "", nil
	}
	start := offset - codexCheckpointAnchorSize
	start = max(start, 0)
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.CopyN(h, f, offset-start); err != nil {
		return "", fmt.Errorf(
			"reading anchor window for %s: %w", path, err,
		)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func codexCheckpointAnchorMatches(
	path string, cp *db.ParserCheckpoint,
) (bool, error) {
	digest, err := codexCheckpointAnchorDigest(path, cp.Offset)
	if err != nil {
		return false, err
	}
	return digest == cp.TailAnchorDigest, nil
}

// codexResumeHashFn is an overridable seam for tests that exercise the
// post-parse reconstruction failure path. Production always resumes through
// codexResumeHash; the checkpoint gate keeps using codexResumeHash directly
// so a test can fail only the engine's later reconstruction.
var codexResumeHashFn = codexResumeHash

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

// buildCodexFullParseCheckpoint assembles the checkpoint row and blob
// payload for a just-parsed full snapshot from the pending write's
// captured state. It returns nil when the write carries no usable resume
// payload (the parse did not end at a safe boundary).
func (e *Engine) buildCodexFullParseCheckpoint(
	path string, pw pendingWrite,
) (*db.ParserCheckpoint, *db.ParserCheckpointBlobs, error) {
	if len(pw.checkpoint) == 0 || len(pw.checkpointHashState) == 0 ||
		pw.checkpointAnchorDigest == "" {
		return nil, nil, nil
	}
	hash, err := codexHashStateDigest(pw.checkpointHashState)
	if err != nil {
		return nil, nil, err
	}
	// Identity comes from the parse snapshot, never from a later path
	// stat: the cursor, hash state, and anchor describe the bytes the
	// parser read, and pairing them with a fresher stat could bless a
	// concurrent rewrite as the parsed content.
	cp := &db.ParserCheckpoint{
		SessionID:        pw.sess.ID,
		Agent:            string(pw.sess.Agent),
		FilePath:         e.effectiveSourcePath(path),
		FileInode:        uint64(pw.sess.File.Inode),
		FileDevice:       uint64(pw.sess.File.Device),
		FileMTime:        pw.sess.File.Mtime,
		FileChangeTime:   pw.sess.File.ChangeTime,
		Offset:           pw.sess.File.Size,
		TailAnchorDigest: pw.checkpointAnchorDigest,
		Hash:             hash,
		NextOrdinal:      pw.sess.MessageCount,
		Version:          codexCheckpointVersion,
	}
	return cp, &db.ParserCheckpointBlobs{
		SessionID: pw.sess.ID,
		Cursor:    pw.checkpoint,
		HashState: pw.checkpointHashState,
	}, nil
}

// buildCodexCheckpoint assembles the checkpoint metadata row and the lazy
// blob payload for a committed prefix of size newOffset. anchorDigest is the
// digest of the prefix's trailing anchor window; callers obtain it either
// from the parser's single-pass capture (full parse) or from a bounded
// read of the file tail (incremental path).
func buildCodexCheckpoint(
	sessionID, agent, storedPath string,
	inode, device uint64,
	mtime, changeTime int64,
	newOffset int64,
	cursor []byte,
	hashState []byte,
	hash string,
	nextOrdinal int,
	anchorDigest string,
) (*db.ParserCheckpoint, db.ParserCheckpointBlobs) {
	return &db.ParserCheckpoint{
			SessionID:        sessionID,
			FileChangeTime:   changeTime,
			Agent:            agent,
			FilePath:         storedPath,
			FileInode:        uint64(inode),
			FileDevice:       uint64(device),
			FileMTime:        mtime,
			Offset:           newOffset,
			TailAnchorDigest: anchorDigest,
			Hash:             hash,
			NextOrdinal:      nextOrdinal,
			Version:          codexCheckpointVersion,
		}, db.ParserCheckpointBlobs{
			SessionID: sessionID,
			Cursor:    cursor,
			HashState: hashState,
		}
}
