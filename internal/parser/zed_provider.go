package parser

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
				WithWatchRoots(zedWatchRoots),
				WithChangedPathClassifier(zedClassifyPath),
				WithMemberLookup(zedFindMember),
				WithFingerprint(zedFingerprintSource),
				WithContainerParse(zedParseContainer),
				WithMemberParse(zedParseMember),
				WithMemberPresence(zedMemberPresent),
			)
		},
	)
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
// a database delete (or its WAL/SHM sibling) still classifies for tombstones.
func zedClassifyPath(root, path string, allowMissing bool) (multiSessionMatch, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	requireRegular := !allowMissing
	if dbPath, sessionID, ok := parseZedVirtualPath(path); ok {
		if !zedDBUnderRoot(root, dbPath, requireRegular) {
			return multiSessionMatch{}, false
		}
		return multiSessionMatch{
			Path:      path,
			Container: dbPath,
			MemberID:  sessionID,
		}, true
	}
	if zedDBUnderRoot(root, path, requireRegular) {
		return multiSessionMatch{Path: path, Container: path}, true
	}
	if allowMissing {
		if dbPath, ok := zedDBPathForEvent(root, path); ok {
			return multiSessionMatch{Path: dbPath, Container: dbPath}, true
		}
	}
	return multiSessionMatch{}, false
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

func zedFingerprintSource(src multiSessionSource) (SourceFingerprint, error) {
	info, err := os.Stat(src.Container)
	if err != nil {
		if os.IsNotExist(err) {
			return SourceFingerprint{}, nil
		}
		return SourceFingerprint{}, fmt.Errorf("stat %s: %w", src.Container, err)
	}
	mtime := info.ModTime().UnixNano()
	if src.MemberID != "" {
		sessionMtime, err := ZedSQLiteSourceMtime(src.Path)
		switch {
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
		}
		// A non-ErrNoRows error (unreadable DB, non-virtual path) keeps the
		// physical DB mtime fallback, preserving the prior behavior for
		// transient failures.
	} else if compositeMtime, err := sqliteDBCompositeMtime(src.Container); err == nil {
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
	src multiSessionSource, req ParseRequest,
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
	return parseZedThreadFromDB(
		conn, src.Container, src.MemberID, req.Machine, dbInfo,
	)
}

func zedParseContainer(
	src multiSessionSource, req ParseRequest,
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
	metas, err := ListZedThreadMetas(conn, src.Container)
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
		result, err := parseZedThreadFromDB(
			conn, src.Container, meta.RawID, req.Machine, dbInfo,
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

func zedDBUnderRoot(root, dbPath string, requireRegular bool) bool {
	root = filepath.Clean(root)
	dbPath = filepath.Clean(dbPath)
	rel, ok := relUnder(root, dbPath)
	if !ok || filepath.ToSlash(rel) != "threads/threads.db" {
		return false
	}
	return !requireRegular || IsRegularFile(dbPath)
}

func zedDBPathForEvent(root, path string) (string, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rel, ok := relUnder(root, path)
	if !ok {
		return "", false
	}
	relSlash := filepath.ToSlash(rel)
	if relSlash == "threads/threads.db" ||
		(filepath.ToSlash(filepath.Dir(rel)) == "threads" &&
			strings.HasPrefix(filepath.Base(rel), "threads.db-")) {
		return filepath.Join(root, zedThreadsDBRelPath), true
	}
	return "", false
}

func sqliteDBCompositeMtime(dbPath string) (int64, error) {
	var maxMtime int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
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
	return Capabilities{
		Source: multiSessionContainerSourceCapabilities(
			CapabilitySupported,
			CapabilitySupported,
		),
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
	}
}
