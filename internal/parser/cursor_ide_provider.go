package parser

import (
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

// Cursor IDE stores every chat session in one shared SQLite database
// (state.vscdb). It is a multi-session container provider: discovery
// surfaces the database as a single source and Parse fans it out into one
// session per composer, addressed by "<db>#<composerID>" virtual paths. All
// behavior is wired into the shared multi-session-container base via
// options, mirroring the Zed provider.
func newCursorIDEProviderFactory(def AgentDef) ProviderFactory {
	return NewMultiSessionProviderFactory(
		def,
		cursorIDEProviderCapabilities(),
		func(cfg ProviderConfig) multiSessionContainerSourceSet {
			return NewMultiSessionContainerSourceSet(
				AgentCursorIDE,
				cfg.Roots,
				WithContainerDiscovery(cursorIDEDiscoverContainers),
				WithWatchRoots(cursorIDEWatchRoots),
				WithChangedPathClassifier(cursorIDEClassifyPath),
				WithMemberLookup(cursorIDEFindMember),
				WithContextFingerprint(cursorIDEFingerprintSource),
				WithContextContainerParse(cursorIDEParseContainer),
				WithContextMemberParse(cursorIDEParseMember),
				WithMemberPresence(cursorIDEMemberPresent),
				WithBatchMemberPresence(cursorIDEBatchMemberPresent),
			)
		},
	)
}

func cursorIDEDiscoverContainers(root string) []string {
	if root == "" {
		return nil
	}
	path := filepath.Join(root, CursorIDEDBRelPath)
	if !IsRegularFile(path) {
		return nil
	}
	return []string{path}
}

// cursorIDEWatchRoots watches the provider root directly: state.vscdb sits
// straight inside it, with no subdirectory like Zed's threads/threads.db.
func cursorIDEWatchRoots(roots []string) []WatchRoot {
	out := make([]WatchRoot, 0, len(roots))
	for _, root := range roots {
		out = append(out, WatchRoot{
			Path:         root,
			Recursive:    false,
			IncludeGlobs: []string{CursorIDEDBRelPath, CursorIDEDBRelPath + "-*"},
			DebounceKey:  string(AgentCursorIDE) + ":state:" + root,
		})
	}
	return out
}

// cursorIDEClassifyPath maps a stored or changed path to its database
// container and composer. allowMissing relaxes the regular-file check so a
// database delete (or its WAL/SHM sibling) still classifies for tombstones.
func cursorIDEClassifyPath(
	root, path string, allowMissing bool,
) (multiSessionMatch, bool) {
	return classifySQLiteContainerPath(
		root, path, CursorIDEDBRelPath, allowMissing, true,
		parseCursorIDEVirtualPath,
	)
}

func cursorIDEFindMember(root, rawID string) (multiSessionMatch, bool) {
	if root == "" || !IsValidSessionID(rawID) {
		return multiSessionMatch{}, false
	}
	dbPath := filepath.Join(root, CursorIDEDBRelPath)
	if !CursorIDEComposerExists(dbPath, rawID) {
		return multiSessionMatch{}, false
	}
	return multiSessionMatch{
		Path:      VirtualSourcePath(dbPath, rawID),
		Container: dbPath,
		MemberID:  rawID,
	}, true
}

// cursorIDEFingerprintSource returns the composite whole-database mtime plus
// a SQLite transaction-state hash for a container source, and a per-composer
// content digest for a member source. Neither hashes the database's full
// contents: state.vscdb is 86MB+ locally and 500MB+ has been reported in the
// wild, and Cursor keeps the file open and mutating, so a full-file hash on
// every fingerprint pass is both slow and would defeat mtime-based freshness
// by always looking "changed". The container hash instead reads only the
// database and WAL headers, and the member digest reads only that composer's
// rows.
func cursorIDEFingerprintSource(
	ctx context.Context, src multiSessionSource,
) (SourceFingerprint, error) {
	info, err := os.Stat(src.Container)
	if err != nil {
		if os.IsNotExist(err) {
			return SourceFingerprint{}, nil
		}
		return SourceFingerprint{}, err
	}
	if src.MemberID == "" {
		mtime := info.ModTime().UnixNano()
		if composite, err := sqliteDBCompositeMtime(
			src.Container, sqliteDBJournalSuffixes,
		); err == nil {
			mtime = composite
		}
		stateHash, err := cursorIDESQLiteStateHash(src.Container)
		if err != nil {
			return SourceFingerprint{}, err
		}
		return SourceFingerprint{
			Size:    info.Size(),
			MTimeNS: mtime,
			Hash:    stateHash,
		}, nil
	}

	conn, err := openCursorIDEDB(src.Container)
	if err != nil {
		return SourceFingerprint{}, err
	}
	defer conn.Close()
	meta, ok, err := loadCursorIDEComposerMeta(ctx, conn, src.MemberID)
	if err != nil {
		return SourceFingerprint{}, err
	}
	if !ok {
		// Composer row is gone but the DB file remains: a keyed-empty
		// fingerprint without error so the engine proceeds to Parse, which
		// force-replaces the deleted composer out of the archive.
		return SourceFingerprint{}, nil
	}
	return SourceFingerprint{
		Size:    info.Size(),
		MTimeNS: cursorIDETime(meta.lastUpdatedAt).UnixNano(),
		Hash:    meta.digest,
	}, nil
}

// cursorIDESQLiteStateHash digests the parts of a SQLite database's on-disk
// state that change with every committed transaction, without reading any
// data pages: the 100-byte main header (whose change counter, schema cookie,
// and version-valid-for fields move on rollback-journal commits) and the WAL
// sibling's size plus 32-byte header (which grows per WAL-mode commit and
// whose salts are re-randomized on every WAL reset). The skip cache keys on
// it (FingerprintHashInCacheKey), so a rewrite that leaves the database's
// size and mtime unchanged still misses the cache and reparses, without the
// full-file hashing this provider deliberately avoids.
func cursorIDESQLiteStateHash(dbPath string) (string, error) {
	h := fnv.New64a()
	f, err := os.Open(dbPath)
	if err != nil {
		return "", fmt.Errorf("reading cursor IDE db header %s: %w", dbPath, err)
	}
	defer f.Close()
	// Fold the file identity in so an atomic replacement of state.vscdb
	// with a different database (a restore or profile switch renamed into
	// place) always changes the cache identity, even when size, mtime, and
	// the headers happen to coincide. On Unix this is inode and device; on
	// Windows the NTFS file index and volume serial read from the handle.
	id, volume := sourceFileHandleIdentity(f)
	_, _ = fmt.Fprintf(h, "%d|%d|", id, volume)
	header := make([]byte, 100)
	n, err := io.ReadFull(f, header)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return "", fmt.Errorf("reading cursor IDE db header %s: %w", dbPath, err)
	}
	_, _ = h.Write(header[:n])
	walPath := dbPath + "-wal"
	if info, err := os.Stat(walPath); err == nil {
		_, _ = fmt.Fprintf(h, "|wal:%d|", info.Size())
		if wal, err := os.Open(walPath); err == nil {
			walHeader := make([]byte, 32)
			wn, _ := io.ReadFull(wal, walHeader)
			_, _ = h.Write(walHeader[:wn])
			_ = wal.Close()
		}
	}
	return strconv.FormatUint(h.Sum64(), 16), nil
}

// IsCursorIDEContainerSource reports whether source addresses a whole
// state.vscdb container rather than one "<db>#<composerID>" virtual member.
// The sync engine's skip-cache freshness gate exempts whole containers from
// the stored-row hash check: the archive holds only virtual member rows for
// them, and the cache identity already carries the container state hash.
func IsCursorIDEContainerSource(source SourceRef) bool {
	if source.Provider != AgentCursorIDE {
		return false
	}
	path := providerSourcePath(source)
	if path == "" {
		return false
	}
	_, _, virtual := parseCursorIDEVirtualPath(path)
	return !virtual
}

func cursorIDEMemberPresent(src multiSessionSource) bool {
	if src.MemberID == "" {
		return IsRegularFile(src.Container)
	}
	return CursorIDEComposerExists(src.Container, src.MemberID)
}

// cursorIDEBatchMemberPresent reports current composer membership for the
// stored members of one changed container over a single read connection,
// instead of one CursorIDEComposerExists database open per member. On any
// failure it reports every member present, so a transiently unreadable
// database never tombstones archived sessions.
func cursorIDEBatchMemberPresent(
	container multiSessionSource, members []multiSessionSource,
) map[string]bool {
	present := make(map[string]bool, len(members))
	existing := make(map[string]struct{})
	conn, err := openCursorIDEDB(container.Container)
	if err == nil {
		defer conn.Close()
		ids, listErr := listCursorIDEComposerIDs(context.Background(), conn)
		err = listErr
		for _, id := range ids {
			existing[id] = struct{}{}
		}
	}
	for _, member := range members {
		if err != nil {
			present[member.Path] = true
			continue
		}
		_, ok := existing[member.MemberID]
		present[member.Path] = ok
	}
	return present
}

func cursorIDEParseMember(
	ctx context.Context, src multiSessionSource, req ParseRequest,
) (*ParseResult, error) {
	dbInfo, err := os.Stat(src.Container)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if !IsValidSessionID(src.MemberID) {
		return nil, nil
	}
	conn, err := openCursorIDEDB(src.Container)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return parseCursorIDEComposer(
		ctx, conn, src.Container, src.MemberID, req.Machine, dbInfo,
	)
}

func cursorIDEParseContainer(
	ctx context.Context, src multiSessionSource, req ParseRequest,
) ([]ParseResult, error) {
	dbInfo, err := os.Stat(src.Container)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	conn, err := openCursorIDEDB(src.Container)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	ids, err := listCursorIDEComposerIDs(ctx, conn)
	if err != nil {
		return nil, err
	}
	results := make([]ParseResult, 0, len(ids))
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		result, err := parseCursorIDEComposer(
			ctx, conn, src.Container, id, req.Machine, dbInfo,
		)
		if err != nil {
			return nil, err
		}
		if result == nil {
			continue
		}
		results = append(results, *result)
	}
	return results, nil
}

// parseCursorIDEVirtualPath splits a Cursor IDE virtual source path into its
// physical state.vscdb path and raw composer ID.
func parseCursorIDEVirtualPath(path string) (string, string, bool) {
	dbPath, composerID, ok := ParseVirtualSourcePathForBase(path, CursorIDEDBRelPath)
	if !ok || !IsValidSessionID(composerID) {
		return "", "", false
	}
	return dbPath, composerID, true
}

func cursorIDEProviderCapabilities() Capabilities {
	// Stored-source hints feed the base's changed-path tombstones: a
	// state.vscdb change event checks the archived members of that container
	// and force-replaces the ones whose composerData row is gone, so a chat
	// deleted in Cursor IDE is retired without waiting for an archive audit.
	source := multiSessionContainerSourceCapabilities(
		CapabilitySupported,
		CapabilitySupported,
	)
	source.PersistentArchive = CapabilitySupported
	return Capabilities{
		Source: source,
		Content: ContentCapabilities{
			FirstMessage: CapabilitySupported,
			SessionName:  CapabilitySupported,
			Cwd:          CapabilitySupported,
			GitBranch:    CapabilitySupported,
			ToolCalls:    CapabilitySupported,
			ToolResults:  CapabilitySupported,
		},
		Sync: ProviderSyncSemantics{
			// The fingerprint hash participates in the skip-cache key and in
			// stored-row freshness (Omnigent precedent): a database rewrite
			// that leaves size and mtime unchanged still changes the
			// container's transaction-state hash and so misses the cache,
			// and a member whose lastUpdatedAt is stale is still reparsed
			// when its content digest moved.
			FingerprintHashInCacheKey:           true,
			FingerprintHashRequiredForFreshness: true,
			// Compare the per-composer digest, not just the
			// lastUpdatedAt-derived mtime, when dropping unchanged fan-out
			// results: an edit that leaves lastUpdatedAt untouched must
			// still rewrite the session (Zed cannot do this because it has
			// no per-member digest).
			UnchangedResults: UnchangedResultMTimeAndHash,
		},
	}
}
