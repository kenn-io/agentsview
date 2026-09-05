package parser

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Zed stores every thread in one shared SQLite database
// (threads/threads.db). It is a multi-session container provider: discovery
// surfaces the database as a single source and Parse fans it out into one
// session per thread, addressed by "<db>::<threadID>" virtual paths. All
// behavior is wired into the shared multi-session-container base via options.
func newZedProviderFactory(def AgentDef) ProviderFactory {
	return NewMultiSessionProviderFactory(
		def,
		zedProviderCapabilities(),
		func(cfg ProviderConfig) multiSessionContainerSourceSet {
			return NewMultiSessionContainerSourceSet(
				AgentZed,
				cfg.Roots,
				WithContainerDiscovery(zedDiscoverContainers),
				WithStreamingSourceDiscovery(zedDiscoverEach),
				WithWatchRoots(zedWatchRoots),
				WithChangedPathClassifier(zedClassifyPath),
				WithMemberLookup(zedFindMember),
				WithContextFingerprint(zedFingerprintSource),
				WithContextContainerParse(zedParseContainer),
				WithContextMemberParse(zedParseMember),
				WithMemberPresence(zedMemberPresent),
			)
		},
	)
}

func zedDiscoverEach(
	ctx context.Context, root string, yield func(multiSessionMatch) error,
) error {
	containers := zedDiscoverContainers(root)
	if len(containers) == 0 {
		return nil
	}
	dbPath := containers[0]
	conn, err := OpenZedDB(dbPath)
	if err != nil {
		return err
	}
	defer conn.Close()
	shape, err := inspectZedSchema(ctx, conn)
	if err != nil {
		return wrapZedListingError(err)
	}
	return forEachZedThreadMeta(ctx, conn, dbPath, shape, func(meta ZedThreadMeta) error {
		return yield(multiSessionMatch{
			Path: meta.VirtualPath, Container: dbPath, MemberID: meta.RawID,
		})
	})
}

func zedDiscoverContainers(root string) []string {
	if root == "" {
		return nil
	}
	path := filepath.Join(root, zedThreadsDBRelPath)
	if !IsRegularFile(path) {
		return nil
	}
	return []string{path}
}

func zedWatchRoots(roots []string) []WatchRoot {
	out := make([]WatchRoot, 0, len(roots))
	for _, root := range roots {
		threadsDir := filepath.Join(root, "threads")
		out = append(out, WatchRoot{
			Path:         threadsDir,
			Recursive:    false,
			IncludeGlobs: []string{"threads.db", "threads.db-*"},
			DebounceKey:  string(AgentZed) + ":threads:" + threadsDir,
		})
	}
	return out
}

// zedClassifyPath maps a stored or changed path to its database container and
// thread, reproducing the legacy strict sourceRef / lenient
// sourceRefForChangedPath split: allowMissing relaxes the regular-file check so
// a database delete (or its WAL sibling) still classifies for tombstones. A
// bare "-shm" sibling event is rejected: the provider's own read connections
// rewrite that file, so honoring it would make every scan schedule the next.
func zedClassifyPath(root, path string, allowMissing bool) (multiSessionMatch, bool) {
	return classifySQLiteContainerPath(
		root, path, zedThreadsDBRelPath, allowMissing, true, parseZedVirtualPath,
	)
}

func zedFindMember(root, rawID string) (multiSessionMatch, bool) {
	if root == "" || !IsValidSessionID(rawID) {
		return multiSessionMatch{}, false
	}
	path := filepath.Join(root, zedThreadsDBRelPath)
	if !ZedSQLiteSessionExists(path, rawID) {
		return multiSessionMatch{}, false
	}
	return multiSessionMatch{
		Path:      ZedSQLiteVirtualPath(path, rawID),
		Container: path,
		MemberID:  rawID,
	}, true
}

func zedFingerprintSource(ctx context.Context, src multiSessionSource) (SourceFingerprint, error) {
	info, err := os.Stat(src.Container)
	if err != nil {
		if os.IsNotExist(err) {
			return SourceFingerprint{}, nil
		}
		return SourceFingerprint{}, fmt.Errorf("stat %s: %w", src.Container, err)
	}
	mtime := info.ModTime().UnixNano()
	if src.MemberID != "" {
		sessionMtime, err := ZedSQLiteSourceMtimeContext(ctx, src.Path)
		switch {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return SourceFingerprint{}, err
		case errors.Is(err, sql.ErrNoRows):
			// The thread row is gone but threads.db is still present. Return a
			// keyed-empty fingerprint without error (matching the Shelley and
			// Kiro tombstone behavior) so the engine reaches Parse and
			// force-replaces the deleted thread out of the archive. Falling back
			// to the physical DB size/mtime/hash here would let the engine's
			// pre-parse freshness check skip Parse whenever stored metadata
			// happened to match, stranding the stale thread.
			return SourceFingerprint{}, nil
		case err == nil:
			mtime = sessionMtime
		default:
			return SourceFingerprint{}, err
		}
	} else if compositeMtime, err := sqliteDBCompositeMtime(
		src.Container, sqliteDBJournalSuffixes,
	); err == nil {
		mtime = compositeMtime
	}
	// Zed has no cheap per-thread content digest; legacy sync stored the
	// physical DB hash on virtual thread rows while per-thread updated_at
	// remained the mtime freshness signal.
	hash, err := hashJSONLSourceFile(src.Container)
	if err != nil {
		return SourceFingerprint{}, err
	}
	return SourceFingerprint{
		Size:    info.Size(),
		MTimeNS: mtime,
		Hash:    hash,
	}, nil
}

func zedMemberPresent(src multiSessionSource) bool {
	if src.MemberID == "" {
		return IsRegularFile(src.Container)
	}
	return ZedSQLiteSessionExists(src.Container, src.MemberID)
}

func zedParseMember(
	ctx context.Context, src multiSessionSource, req ParseRequest,
) (*ParseResult, error) {
	dbInfo, err := os.Stat(src.Container)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat %s: %w", src.Container, err)
	}
	if !IsValidSessionID(src.MemberID) {
		return nil, fmt.Errorf("invalid Zed session ID: %s", src.MemberID)
	}
	conn, err := OpenZedDB(src.Container)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	shape, err := inspectZedSchema(ctx, conn)
	if err != nil {
		return nil, wrapZedLoadingError(src.MemberID, err)
	}
	return parseZedThreadFromDBWithSchema(
		ctx, conn, src.Container, src.MemberID, req.Machine, dbInfo, shape,
	)
}

func zedParseContainer(
	ctx context.Context, src multiSessionSource, req ParseRequest,
) ([]ParseResult, error) {
	dbInfo, err := os.Stat(src.Container)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat %s: %w", src.Container, err)
	}
	conn, err := OpenZedDB(src.Container)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	shape, err := inspectZedSchema(ctx, conn)
	if err != nil {
		return nil, wrapZedListingError(err)
	}
	var metas []ZedThreadMeta
	err = forEachZedThreadMeta(ctx, conn, src.Container, shape, func(meta ZedThreadMeta) error {
		metas = append(metas, meta)
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Zed has no per-thread content digest; stamp the physical DB hash on every
	// fanned-out thread row, mirroring the legacy fan-out. Computed here rather
	// than via the base's hash stamping because the value is the DB's own hash,
	// not the request fingerprint.
	dbHash, _ := hashJSONLSourceFile(src.Container)
	results := make([]ParseResult, 0, len(metas))
	for _, meta := range metas {
		result, err := parseZedThreadFromDBWithSchema(
			ctx, conn, src.Container, meta.RawID, req.Machine, dbInfo, shape,
		)
		if err != nil {
			return nil, err
		}
		if result == nil {
			continue
		}
		if dbHash != "" {
			result.Session.File.Hash = dbHash
		}
		results = append(results, *result)
	}
	return results, nil
}

// sqliteDBJournalSuffixes is the sibling-file suffix list for
// sqliteDBCompositeMtime: the database file itself plus its WAL. The "-shm"
// index is left out on purpose. Every committed write lands in the main file
// or the WAL, while readers, including this process's own scan connections,
// rewrite the shared-memory file, so folding its mtime in would make every
// scan report the container as changed and schedule the next scan.
var sqliteDBJournalSuffixes = []string{"", "-wal"}

// sqliteDBCompositeMtime returns the freshest mtime across a SQLite database
// file and the listed sibling suffixes (e.g. "-wal", "-shm").
func sqliteDBCompositeMtime(dbPath string, suffixes []string) (int64, error) {
	var maxMtime int64
	for _, suffix := range suffixes {
		info, err := os.Stat(dbPath + suffix)
		if err != nil {
			continue
		}
		if mtime := info.ModTime().UnixNano(); mtime > maxMtime {
			maxMtime = mtime
		}
	}
	if maxMtime == 0 {
		return 0, &os.PathError{Op: "stat", Path: dbPath, Err: os.ErrNotExist}
	}
	return maxMtime, nil
}

// parseZedVirtualPath splits a Zed virtual source path into its physical
// threads.db path and raw thread ID. The container basename must be threads.db
// and the thread ID must pass IsValidSessionID so path-like input is rejected.
func parseZedVirtualPath(path string) (string, string, bool) {
	dbPath, sessionID, ok := ParseVirtualSourcePathForBase(path, "threads.db")
	if !ok || !IsValidSessionID(sessionID) {
		return "", "", false
	}
	return dbPath, sessionID, true
}

func zedProviderCapabilities() Capabilities {
	source := multiSessionContainerSourceCapabilities(
		CapabilitySupported,
		CapabilitySupported,
	)
	source.PersistentArchive = CapabilitySupported
	return Capabilities{
		Source: source,
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			SessionName:          CapabilitySupported,
			Cwd:                  CapabilitySupported,
			Thinking:             CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			AggregateUsageEvents: CapabilitySupported,
			Model:                CapabilitySupported,
		},
		Sync: ProviderSyncSemantics{
			UnchangedResults: UnchangedResultMTime,
		},
	}
}
