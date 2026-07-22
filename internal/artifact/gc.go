package artifact

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const retentionPageSize = 256

// GCOptions configures conservative logical artifact retention. Store exposes
// canonical references; retention never inspects or removes physical files.
type GCOptions struct {
	Store           ArtifactStore
	Grace           time.Duration
	QuarantineGrace time.Duration
	DryRun          bool
	Now             time.Time
	Logf            func(string, ...any)
}

// GCResult summarizes one logical retention scan.
type GCResult struct {
	DryRun         bool
	Origins        int
	SkippedOrigins int
	Scanned        int
	Candidates     int
	Eligible       int
	KeptByGrace    int
	Deleted        int
	BytesEligible  int64
	BytesDeleted   int64

	QuarantinedScanned  int
	QuarantinedEligible int
	QuarantinedDeleted  int
	QuarantineSkipped   bool
}

type gcOriginClosure struct {
	db   *sql.DB
	dir  string
	path string
}

// GarbageCollect trashes canonical nodes unreachable from each origin's newest
// safe checkpoint. Origins without such a checkpoint are left untouched.
func GarbageCollect(ctx context.Context, opts GCOptions) (_ GCResult, retErr error) {
	if ctx == nil {
		return GCResult{}, fmt.Errorf("artifact gc context is required")
	}
	if opts.Store == nil {
		return GCResult{}, fmt.Errorf("artifact gc store is required")
	}
	if opts.Grace < 0 {
		return GCResult{}, fmt.Errorf("artifact gc grace must be >= 0")
	}
	if opts.QuarantineGrace < 0 {
		return GCResult{}, fmt.Errorf("artifact quarantine grace must be >= 0")
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	result := GCResult{DryRun: opts.DryRun}

	var originCursor Cursor
	defer func() {
		retErr = errors.Join(retErr, releaseArtifactCursor(opts.Store, &originCursor))
	}()
	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		origins, next, err := opts.Store.ListOrigins(ctx, originCursor, retentionPageSize)
		if err != nil {
			return result, fmt.Errorf("listing artifact origins: %w", err)
		}
		originCursor = next
		for _, origin := range origins {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			result.Origins++
			closure, safe, err := collectGCOriginClosure(ctx, opts.Store, origin)
			if err != nil {
				return result, fmt.Errorf("scanning artifact origin %s: %w", origin, err)
			}
			if !safe {
				result.SkippedOrigins++
				logGC(opts, "artifact gc: skipping %s without a safe current checkpoint", origin)
				continue
			}
			collectErr := collectGCOrigin(ctx, opts, origin, closure, now, &result)
			closeErr := closure.Close()
			if err := errors.Join(collectErr, closeErr); err != nil {
				return result, fmt.Errorf("collecting artifact origin %s: %w", origin, err)
			}
		}
		if originCursor == "" {
			break
		}
	}
	if err := collectGCQuarantine(ctx, opts, now, &result); err != nil {
		return result, err
	}
	return result, nil
}

func collectGCOriginClosure(
	ctx context.Context, store ArtifactStore, origin string,
) (gcOriginClosure, bool, error) {
	latest, found, err := latestGCCheckpoint(ctx, store, origin)
	if err != nil || !found {
		return gcOriginClosure{}, false, err
	}
	closure, err := newGCOriginClosure(ctx)
	if err != nil {
		return gcOriginClosure{}, false, err
	}
	safe, collectErr := closure.indexCheckpoint(ctx, store, origin, latest)
	if collectErr != nil || !safe {
		closeErr := closure.Close()
		if collectErr != nil && isUnsafeRetentionArtifact(collectErr) {
			collectErr = nil
		}
		return gcOriginClosure{}, false, errors.Join(collectErr, closeErr)
	}
	return closure, true, nil
}

const gcReachabilitySchema = `
CREATE TABLE sessions (
    gid TEXT PRIMARY KEY,
    manifest_hash TEXT NOT NULL
) WITHOUT ROWID;
CREATE TABLE live (
    kind TEXT NOT NULL,
    name TEXT NOT NULL,
    PRIMARY KEY (kind, name)
) WITHOUT ROWID;
CREATE TABLE manifests (
    hash TEXT PRIMARY KEY,
    gid TEXT NOT NULL
) WITHOUT ROWID;
`

func newGCOriginClosure(ctx context.Context) (_ gcOriginClosure, retErr error) {
	dir, err := os.MkdirTemp("", "agentsview-artifact-retention-")
	if err != nil {
		return gcOriginClosure{}, err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, os.RemoveAll(dir))
		}
	}()
	path := filepath.Join(dir, "reachability.sqlite3")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return gcOriginClosure{}, err
	}
	if err := file.Close(); err != nil {
		return gcOriginClosure{}, err
	}
	database, err := sql.Open("sqlite3", path)
	if err != nil {
		return gcOriginClosure{}, err
	}
	closure := gcOriginClosure{db: database, dir: dir, path: path}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, database.Close())
		}
	}()
	database.SetMaxOpenConns(1)
	if _, err := database.ExecContext(ctx, `
PRAGMA journal_mode = OFF;
PRAGMA synchronous = OFF;
PRAGMA temp_store = FILE;
`); err != nil {
		return gcOriginClosure{}, err
	}
	if _, err := database.ExecContext(ctx, gcReachabilitySchema); err != nil {
		return gcOriginClosure{}, err
	}
	return closure, nil
}

func (c *gcOriginClosure) Close() error {
	if c == nil {
		return nil
	}
	database := c.db
	c.db = nil
	var closeErr error
	if database != nil {
		closeErr = database.Close()
	}
	removeErr := os.RemoveAll(c.dir)
	c.dir = ""
	c.path = ""
	return errors.Join(closeErr, removeErr)
}

type gcCheckpointHeader struct {
	version  int
	origin   string
	sequence int
}

func (c gcOriginClosure) indexCheckpoint(
	ctx context.Context, store ArtifactStore, origin string, latest Entry,
) (safe bool, retErr error) {
	if latest.Identity.Size > checkpointDecodedLimit {
		return false, nil
	}
	actual, reader, err := store.Open(ctx, latest.Ref)
	if err != nil {
		return false, err
	}
	readerOpen := true
	defer func() {
		if readerOpen {
			retErr = errors.Join(retErr, reader.Close())
		}
	}()
	if actual.Identity != latest.Identity || actual.Ref != latest.Ref {
		return false, fmt.Errorf("%w: artifact changed during retention read", ErrArtifactCorrupt)
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	committed := false
	defer func() {
		if !committed {
			retErr = errors.Join(retErr, tx.Rollback())
		}
	}()
	limited := &io.LimitedReader{R: reader, N: checkpointDecodedLimit + 1}
	decoder := json.NewDecoder(limited)
	header, err := decodeGCCheckpoint(ctx, decoder, tx, origin)
	if err != nil {
		return false, err
	}
	if err := consumeGCCheckpointWhitespace(ctx, decoder.Buffered()); err != nil {
		return false, err
	}
	if err := consumeGCCheckpointWhitespace(ctx, limited); err != nil {
		return false, err
	}
	if limited.N == 0 {
		return false, fmt.Errorf("%w: checkpoint exceeds retention read limit", ErrArtifactInvalid)
	}
	if err := reader.Verify(); err != nil {
		return false, err
	}
	cp := checkpoint{Version: header.version, Origin: header.origin, Sequence: header.sequence}
	if err := validateCheckpoint(&cp, origin); err != nil {
		return false, fmt.Errorf("%w: %v", ErrArtifactInvalid, err)
	}
	if err := validateCheckpointSequenceIdentity(cp, latest.Ref.Name); err != nil {
		return false, fmt.Errorf("%w: %v", ErrArtifactInvalid, err)
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	committed = true
	closeErr := reader.Close()
	readerOpen = false
	if closeErr != nil {
		return false, closeErr
	}
	return c.indexCheckpointDependencies(ctx, store, origin, latest.Ref.Name)
}

func decodeGCCheckpoint(
	ctx context.Context, decoder *json.Decoder, tx *sql.Tx, origin string,
) (gcCheckpointHeader, error) {
	invalid := func(format string, args ...any) (gcCheckpointHeader, error) {
		return gcCheckpointHeader{}, fmt.Errorf("%w: %s", ErrArtifactInvalid, fmt.Sprintf(format, args...))
	}
	token, err := decoder.Token()
	if err != nil {
		return invalid("decoding checkpoint: %v", err)
	}
	if token != json.Delim('{') {
		return invalid("checkpoint must be a JSON object")
	}
	var header gcCheckpointHeader
	var seenVersion, seenOrigin, seenSequence, seenSessions bool
	for decoder.More() {
		if err := ctx.Err(); err != nil {
			return gcCheckpointHeader{}, err
		}
		fieldToken, err := decoder.Token()
		if err != nil {
			return invalid("decoding checkpoint field: %v", err)
		}
		field, ok := fieldToken.(string)
		if !ok {
			return invalid("checkpoint field name is not a string")
		}
		switch field {
		case "v":
			if seenVersion {
				return invalid("checkpoint contains duplicate field %q", field)
			}
			seenVersion = true
			if err := decoder.Decode(&header.version); err != nil {
				return invalid("decoding checkpoint version: %v", err)
			}
		case "origin":
			if seenOrigin {
				return invalid("checkpoint contains duplicate field %q", field)
			}
			seenOrigin = true
			if err := decoder.Decode(&header.origin); err != nil {
				return invalid("decoding checkpoint origin: %v", err)
			}
		case "seq":
			if seenSequence {
				return invalid("checkpoint contains duplicate field %q", field)
			}
			seenSequence = true
			if err := decoder.Decode(&header.sequence); err != nil {
				return invalid("decoding checkpoint sequence: %v", err)
			}
		case "sessions":
			if seenSessions {
				return invalid("checkpoint contains duplicate field %q", field)
			}
			seenSessions = true
			if err := decodeGCCheckpointSessions(ctx, decoder, tx, origin); err != nil {
				return gcCheckpointHeader{}, err
			}
		default:
			return invalid("checkpoint contains unknown field %q", field)
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return invalid("decoding checkpoint close: %v", err)
	}
	for _, required := range []struct {
		name string
		seen bool
	}{
		{name: "v", seen: seenVersion},
		{name: "origin", seen: seenOrigin},
		{name: "seq", seen: seenSequence},
		{name: "sessions", seen: seenSessions},
	} {
		if !required.seen {
			return invalid("checkpoint is missing required field %q", required.name)
		}
	}
	return header, nil
}

func decodeGCCheckpointSessions(
	ctx context.Context, decoder *json.Decoder, tx *sql.Tx, origin string,
) error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrArtifactInvalid, fmt.Sprintf(format, args...))
	}
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return invalid("checkpoint sessions must be a JSON object")
	}
	for decoder.More() {
		if err := ctx.Err(); err != nil {
			return err
		}
		gidToken, err := decoder.Token()
		if err != nil {
			return invalid("decoding checkpoint session id: %v", err)
		}
		gid, ok := gidToken.(string)
		if !ok || gid == "" || !strings.HasPrefix(gid, origin+"~") {
			return invalid("checkpoint contains invalid session id %q", gid)
		}
		var manifestHash string
		if err := decoder.Decode(&manifestHash); err != nil {
			return invalid("decoding checkpoint manifest hash: %v", err)
		}
		if err := validateHashHex(manifestHash); err != nil {
			return invalid("checkpoint session %s has invalid manifest hash: %v", gid, err)
		}
		result, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO sessions (gid, manifest_hash) VALUES (?, ?)`,
			gid, manifestHash)
		if err != nil {
			return err
		}
		inserted, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if inserted != 1 {
			return invalid("checkpoint contains duplicate session id %q", gid)
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return invalid("decoding checkpoint sessions close: %v", err)
	}
	return nil
}

func consumeGCCheckpointWhitespace(ctx context.Context, reader io.Reader) error {
	var buffer [32 << 10]byte
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		count, err := reader.Read(buffer[:])
		for _, value := range buffer[:count] {
			switch value {
			case ' ', '\t', '\r', '\n':
			default:
				return fmt.Errorf("%w: trailing checkpoint data", ErrArtifactInvalid)
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

type gcReachabilityTX interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (c gcOriginClosure) indexCheckpointDependencies(
	ctx context.Context, store ArtifactStore, origin, checkpointName string,
) (safe bool, retErr error) {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	committed := false
	defer func() {
		if !committed {
			retErr = errors.Join(retErr, tx.Rollback())
		}
	}()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO live (kind, name) VALUES (?, ?)`, KindCheckpoints, checkpointName); err != nil {
		return false, err
	}
	lastGID := ""
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		var gid, manifestHash string
		err := tx.QueryRowContext(ctx, `
SELECT gid, manifest_hash
FROM sessions
WHERE gid > ?
ORDER BY gid
LIMIT 1`, lastGID).Scan(&gid, &manifestHash)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return false, err
		}
		lastGID = gid
		claimed, err := claimGCManifest(ctx, tx, manifestHash, gid)
		if err != nil {
			return false, err
		}
		if !claimed {
			return false, fmt.Errorf(
				"%w: manifest %s is referenced by multiple sessions", ErrArtifactInvalid, manifestHash)
		}
		manifestRef := Ref{
			Origin: origin, Kind: KindManifests, Name: manifestHash + ".json",
		}
		manifestEntry, err := store.Stat(ctx, manifestRef)
		if err != nil {
			return false, err
		}
		manifestData, err := readGCStoreArtifact(ctx, store, manifestEntry, manifestDecodedLimit)
		if err != nil {
			return false, err
		}
		if hashHex(manifestData) != manifestHash {
			return false, fmt.Errorf("%w: manifest hash mismatch", ErrArtifactInvalid)
		}
		manifest, err := decodeManifestWithLimits(manifestData, productionArtifactLimits())
		if err != nil || validateManifest(manifest, origin, gid) != nil {
			return false, fmt.Errorf("%w: invalid manifest", ErrArtifactInvalid)
		}
		if err := validateManifestReferencesWithLimits(manifest, productionArtifactLimits()); err != nil {
			return false, fmt.Errorf("%w: invalid manifest references: %v", ErrArtifactInvalid, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO live (kind, name) VALUES (?, ?)`, KindManifests, manifestRef.Name); err != nil {
			return false, err
		}
		for _, segmentHash := range manifest.Segments {
			inserted, err := insertGCLiveRef(ctx, tx, KindSegments, segmentHash+".ndjson")
			if err != nil {
				return false, err
			}
			if !inserted {
				continue
			}
			segmentRef := Ref{
				Origin: origin, Kind: KindSegments, Name: segmentHash + ".ndjson",
			}
			entry, err := store.Stat(ctx, segmentRef)
			if err != nil {
				return false, err
			}
			segmentData, err := readGCStoreArtifact(ctx, store, entry, segmentDecodedLimit)
			if err != nil {
				return false, err
			}
			if hashHex(segmentData) != segmentHash {
				return false, fmt.Errorf("%w: segment hash mismatch", ErrArtifactInvalid)
			}
			if _, err := preflightSegmentData(segmentData, productionArtifactLimits()); err != nil {
				return false, fmt.Errorf("%w: invalid segment: %v", ErrArtifactInvalid, err)
			}
		}
		if manifest.RawSource != nil && manifest.RawSource.Hash != "" {
			inserted, err := insertGCLiveRef(ctx, tx, KindRaw, manifest.RawSource.Hash)
			if err != nil {
				return false, err
			}
			if !inserted {
				continue
			}
			rawRef := Ref{Origin: origin, Kind: KindRaw, Name: manifest.RawSource.Hash}
			entry, err := store.Stat(ctx, rawRef)
			if err != nil {
				return false, err
			}
			if manifest.RawSource.Size != 0 && entry.Identity.Size != manifest.RawSource.Size {
				return false, fmt.Errorf("%w: raw source size mismatch", ErrArtifactInvalid)
			}
			if err := verifyGCStoreArtifact(ctx, store, entry); err != nil {
				return false, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	committed = true
	return true, nil
}

func claimGCManifest(
	ctx context.Context, tx gcReachabilityTX, hash, gid string,
) (bool, error) {
	result, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO manifests (hash, gid) VALUES (?, ?)`, hash, gid)
	if err != nil {
		return false, err
	}
	inserted, err := result.RowsAffected()
	if err != nil || inserted == 1 {
		return inserted == 1, err
	}
	var existingGID string
	if err := tx.QueryRowContext(ctx,
		`SELECT gid FROM manifests WHERE hash = ?`, hash).Scan(&existingGID); err != nil {
		return false, err
	}
	return existingGID == gid, nil
}

func insertGCLiveRef(
	ctx context.Context, tx gcReachabilityTX, kind Kind, name string,
) (bool, error) {
	result, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO live (kind, name) VALUES (?, ?)`, kind, name)
	if err != nil {
		return false, err
	}
	inserted, err := result.RowsAffected()
	return inserted == 1, err
}

func (c gcOriginClosure) contains(ctx context.Context, kind Kind, name string) (bool, error) {
	var one int
	err := c.db.QueryRowContext(ctx,
		`SELECT 1 FROM live WHERE kind = ? AND name = ?`, kind, name).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func latestGCCheckpoint(
	ctx context.Context, store ArtifactStore, origin string,
) (_ Entry, found bool, retErr error) {
	var cursor Cursor
	defer func() { retErr = errors.Join(retErr, releaseArtifactCursor(store, &cursor)) }()
	var latest Entry
	for {
		page, err := store.List(ctx, origin, KindCheckpoints, cursor, retentionPageSize)
		if err != nil {
			return Entry{}, false, err
		}
		for _, entry := range page.Items {
			if _, err := checkpointSequence(entry.Ref.Name); err != nil {
				continue
			}
			latest = entry
			found = true
		}
		cursor = page.Next
		if cursor == "" {
			return latest, found, nil
		}
	}
}

func collectGCOrigin(
	ctx context.Context,
	opts GCOptions,
	origin string,
	closure gcOriginClosure,
	now time.Time,
	result *GCResult,
) error {
	for _, kind := range []Kind{KindCheckpoints, KindManifests, KindSegments, KindRaw} {
		var cursor Cursor
		for {
			page, err := opts.Store.List(ctx, origin, kind, cursor, retentionPageSize)
			if err != nil {
				_ = releaseArtifactCursor(opts.Store, &cursor)
				return err
			}
			cursor = page.Next
			for _, entry := range page.Items {
				if err := ctx.Err(); err != nil {
					_ = releaseArtifactCursor(opts.Store, &cursor)
					return err
				}
				result.Scanned++
				live, err := closure.contains(ctx, kind, entry.Ref.Name)
				if err != nil {
					_ = releaseArtifactCursor(opts.Store, &cursor)
					return err
				}
				if live {
					continue
				}
				result.Candidates++
				if entry.Modified.Add(opts.Grace).After(now) {
					result.KeptByGrace++
					continue
				}
				result.Eligible++
				result.BytesEligible += entry.Identity.Size
				if opts.DryRun {
					logGC(opts, "artifact gc: would trash %s/%s/%s (%d bytes)",
						entry.Ref.Origin, entry.Ref.Kind, entry.Ref.Name, entry.Identity.Size)
					continue
				}
				if err := opts.Store.Trash(ctx, entry.Ref); err != nil {
					if errors.Is(err, ErrArtifactNotFound) {
						continue
					}
					_ = releaseArtifactCursor(opts.Store, &cursor)
					return err
				}
				result.Deleted++
				result.BytesDeleted += entry.Identity.Size
				logGC(opts, "artifact gc: trashed %s/%s/%s (%d bytes)",
					entry.Ref.Origin, entry.Ref.Kind, entry.Ref.Name, entry.Identity.Size)
			}
			if cursor == "" {
				break
			}
		}
	}
	return nil
}

func readGCStoreArtifact(
	ctx context.Context, store ArtifactStore, entry Entry, limit int64,
) (_ []byte, retErr error) {
	actual, reader, err := store.Open(ctx, entry.Ref)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, reader.Close()) }()
	if actual.Identity != entry.Identity || actual.Ref != entry.Ref {
		return nil, fmt.Errorf("%w: artifact changed during retention read", ErrArtifactCorrupt)
	}
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: artifact exceeds retention read limit", ErrArtifactCorrupt)
	}
	if err := reader.Verify(); err != nil {
		return nil, err
	}
	return data, nil
}

func verifyGCStoreArtifact(
	ctx context.Context, store ArtifactStore, entry Entry,
) (retErr error) {
	actual, reader, err := store.Open(ctx, entry.Ref)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, reader.Close()) }()
	if actual.Identity != entry.Identity || actual.Ref != entry.Ref {
		return fmt.Errorf("%w: artifact changed during retention read", ErrArtifactCorrupt)
	}
	if _, err := io.Copy(io.Discard, reader); err != nil {
		return err
	}
	return reader.Verify()
}

func isUnsafeRetentionArtifact(err error) bool {
	return errors.Is(err, ErrArtifactNotFound) ||
		errors.Is(err, ErrArtifactCorrupt) ||
		errors.Is(err, ErrArtifactUnsupported) ||
		errors.Is(err, ErrArtifactInvalid)
}

// collectGCQuarantine is filled in by stores that expose logical quarantine
// paging. Stores without that optional capability are reported and skipped.
func collectGCQuarantine(
	ctx context.Context, opts GCOptions, now time.Time, result *GCResult,
) (retErr error) {
	quarantine, ok := opts.Store.(ArtifactQuarantineStore)
	if !ok {
		result.QuarantineSkipped = true
		logGC(opts, "artifact gc: quarantine retention unsupported by store")
		return nil
	}
	var cursor Cursor
	defer func() {
		retErr = errors.Join(retErr, releaseArtifactCursor(opts.Store, &cursor))
	}()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		items, next, err := quarantine.ListQuarantined(ctx, cursor, retentionPageSize)
		if err != nil {
			return fmt.Errorf("listing artifact quarantine: %w", err)
		}
		cursor = next
		for _, entry := range items {
			if err := ctx.Err(); err != nil {
				return err
			}
			result.QuarantinedScanned++
			if entry.Modified.Add(opts.QuarantineGrace).After(now) {
				continue
			}
			result.QuarantinedEligible++
			if opts.DryRun {
				continue
			}
			if err := quarantine.TrashQuarantined(ctx, entry.Token); err != nil {
				if errors.Is(err, ErrArtifactNotFound) {
					continue
				}
				return fmt.Errorf("trashing artifact quarantine: %w", err)
			}
			result.QuarantinedDeleted++
		}
		if cursor == "" {
			return nil
		}
	}
}

// QuarantinedEntry names one hidden logical artifact retained for diagnosis.
type QuarantinedEntry struct {
	Token    string
	Ref      Ref
	Identity Identity
	Modified time.Time
}

// ArtifactQuarantineStore is an optional stable-page view of hidden quarantine
// nodes. Tokens are opaque and may only be passed back to TrashQuarantined.
type ArtifactQuarantineStore interface {
	ListQuarantined(context.Context, Cursor, int) ([]QuarantinedEntry, Cursor, error)
	TrashQuarantined(context.Context, string) error
}

func logGC(opts GCOptions, format string, args ...any) {
	if opts.Logf != nil {
		opts.Logf(format, args...)
	}
}
