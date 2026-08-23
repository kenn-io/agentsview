package parser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// t3code stores every thread in one shared SQLite database
// (userdata/state.sqlite). It is a multi-session container provider:
// discovery surfaces the database as a single source and Parse fans it out
// into one session per thread, addressed by "<db>#<threadID>" virtual paths.
// All behavior is wired into the shared multi-session-container base via
// options.
func newT3ProviderFactory(def AgentDef) ProviderFactory {
	return NewMultiSessionProviderFactory(
		def,
		t3ProviderCapabilities(),
		func(cfg ProviderConfig) multiSessionContainerSourceSet {
			return NewMultiSessionContainerSourceSet(
				AgentT3,
				cfg.Roots,
				WithContainerDiscovery(t3DiscoverContainers),
				WithStreamingSourceDiscovery(t3DiscoverEach),
				WithWatchRoots(t3WatchRoots),
				WithChangedPathClassifier(t3ClassifyPath),
				WithMemberLookup(t3FindMember),
				WithContextFingerprint(t3FingerprintSource),
				WithContextContainerParse(t3ParseContainer),
				WithContextMemberParse(t3ParseMember),
				WithMemberPresence(t3MemberPresent),
			)
		},
	)
}

func t3DiscoverEach(
	ctx context.Context, root string, yield func(multiSessionMatch) error,
) error {
	dbPath := t3DBPath(root)
	if dbPath == "" {
		return nil
	}
	conn, err := OpenT3DB(dbPath)
	if err != nil {
		return err
	}
	defer conn.Close()
	return ForEachT3ThreadMeta(ctx, conn, dbPath, func(meta T3ThreadMeta) error {
		return yield(multiSessionMatch{
			Path: meta.VirtualPath, Container: dbPath, MemberID: meta.RawID,
		})
	})
}

func t3DiscoverContainers(root string) []string {
	if dbPath := t3DBPath(root); dbPath != "" {
		return []string{dbPath}
	}
	return nil
}

func t3WatchRoots(roots []string) []WatchRoot {
	out := make([]WatchRoot, 0, len(roots))
	for _, root := range roots {
		out = append(out, WatchRoot{
			Path:         root,
			Recursive:    false,
			IncludeGlobs: []string{t3DBName, t3DBName + "-*"},
			DebounceKey:  string(AgentT3) + ":db:" + root,
		})
	}
	return out
}

// t3ClassifyPath maps a stored or changed path to its database container and
// thread. allowMissing relaxes the regular-file requirement so a database
// delete (or its WAL/SHM sibling) still classifies for changed-path
// tombstones.
func t3ClassifyPath(root, path string, allowMissing bool) (multiSessionMatch, bool) {
	return classifySQLiteContainerPath(
		root, path, t3DBName, allowMissing, false, parseT3VirtualPath,
	)
}

// t3FindMember resolves a raw thread ID to its virtual source path inside the
// shared database. The ID is validated only to reject path-like input; all
// threads live in one DB.
func t3FindMember(root, rawID string) (multiSessionMatch, bool) {
	if root == "" || !IsValidSessionID(rawID) {
		return multiSessionMatch{}, false
	}
	dbPath := t3DBPath(root)
	if dbPath == "" || !T3ThreadExists(dbPath, rawID) {
		return multiSessionMatch{}, false
	}
	return multiSessionMatch{
		Path:      T3VirtualPath(dbPath, rawID),
		Container: dbPath,
		MemberID:  rawID,
	}, true
}

func t3FingerprintSource(
	ctx context.Context, src multiSessionSource,
) (SourceFingerprint, error) {
	info, err := os.Stat(src.Container)
	if err != nil {
		if os.IsNotExist(err) {
			return SourceFingerprint{}, nil
		}
		return SourceFingerprint{}, fmt.Errorf("stat %s: %w", src.Container, err)
	}
	fingerprint := SourceFingerprint{
		Size:    info.Size(),
		MTimeNS: info.ModTime().UnixNano(),
	}
	if src.MemberID == "" {
		// Whole-container fingerprint. t3 runs with WAL enabled, so the
		// committed state can advance while state.sqlite's own mtime stands
		// still; fold the journal siblings in so a commit is still seen.
		if compositeMtime, err := sqliteDBCompositeMtime(
			src.Container, sqliteDBJournalSuffixes,
		); err == nil {
			fingerprint.MTimeNS = compositeMtime
		}
		return fingerprint, nil
	}

	conn, err := OpenT3DB(src.Container)
	if err != nil {
		return SourceFingerprint{}, err
	}
	defer conn.Close()
	meta, found, err := T3ThreadMetaByID(ctx, conn, src.Container, src.MemberID)
	if err != nil {
		return SourceFingerprint{}, err
	}
	if found {
		fingerprint.MTimeNS = meta.FileMtime
		return fingerprint, nil
	}
	// The thread row is gone but the database file is still present. Return a
	// keyed-empty fingerprint without error (matching Shelley and Kiro
	// tombstone behavior) so the engine proceeds to Parse rather than aborting
	// on the fingerprint. Parse then force-replaces the deleted thread out of
	// the archive; erroring here would strand the stale session because the
	// engine fingerprints before parsing.
	return SourceFingerprint{}, nil
}

func t3MemberPresent(src multiSessionSource) bool {
	if src.MemberID == "" {
		return IsRegularFile(src.Container)
	}
	return T3ThreadExists(src.Container, src.MemberID)
}

func t3ParseMember(
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
		return nil, fmt.Errorf("invalid t3 thread ID: %s", src.MemberID)
	}
	conn, err := OpenT3DB(src.Container)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return parseT3ThreadFromDB(
		ctx, conn, src.Container, src.MemberID, req.Machine, dbInfo,
	)
}

func t3ParseContainer(
	ctx context.Context, src multiSessionSource, req ParseRequest,
) ([]ParseResult, error) {
	dbInfo, err := os.Stat(src.Container)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat %s: %w", src.Container, err)
	}
	conn, err := OpenT3DB(src.Container)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	shape, err := inspectT3Schema(ctx, conn)
	if err != nil {
		return nil, err
	}
	metas, err := ListT3ThreadMetas(conn, src.Container)
	if err != nil {
		return nil, err
	}
	results := make([]ParseResult, 0, len(metas))
	for _, meta := range metas {
		result, err := parseT3ThreadFromDBWithSchema(
			ctx, conn, shape, src.Container, meta.RawID, req.Machine, dbInfo,
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

// t3DBPath resolves the shared state.sqlite under root, returning "" when the
// root holds no t3 database.
func t3DBPath(root string) string {
	if root == "" {
		return ""
	}
	path := filepath.Join(root, t3DBName)
	if !IsRegularFile(path) {
		return ""
	}
	return path
}

func t3ProviderCapabilities() Capabilities {
	source := multiSessionContainerSourceCapabilities(
		// No composite fingerprint: t3 timestamps carry milliseconds, so the
		// per-thread mtime already separates two writes the second-precision
		// siblings would need a content digest to tell apart. Hashing a
		// multi-megabyte state.sqlite on every fingerprint would buy nothing.
		CapabilityUnsupported,
		CapabilitySupported,
	)
	source.PersistentArchive = CapabilitySupported
	return Capabilities{
		Source: source,
		Content: ContentCapabilities{
			FirstMessage: CapabilitySupported,
			SessionName:  CapabilitySupported,
			Cwd:          CapabilitySupported,
			Model:        CapabilitySupported,
			// The projections hold the visible transcript only: thinking,
			// tool calls, tool results, and per-message token usage live in
			// the event log and the provider's own session files, not in
			// projection_thread_messages.
			Thinking:             CapabilityUnsupported,
			ToolCalls:            CapabilityUnsupported,
			ToolResults:          CapabilityUnsupported,
			PerMessageTokenUsage: CapabilityUnsupported,
			Relationships:        CapabilityUnsupported,
		},
		Sync: ProviderSyncSemantics{
			UnchangedResults: UnchangedResultMTime,
		},
	}
}
