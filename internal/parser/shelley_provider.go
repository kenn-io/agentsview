package parser

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Shelley stores every conversation in one shared SQLite database
// (shelley.db). It is a multi-session container provider: discovery surfaces
// the database as a single source and Parse fans it out into one session per
// conversation, addressed by "<db>::<conversationID>" virtual paths. All
// behavior is wired into the shared multi-session-container base via options.
func newShelleyProviderFactory(def AgentDef) ProviderFactory {
	return NewMultiSessionProviderFactory(
		def,
		shelleyProviderCapabilities(),
		func(cfg ProviderConfig) multiSessionContainerSourceSet {
			return NewMultiSessionContainerSourceSet(
				AgentShelley,
				cfg.Roots,
				WithContainerDiscovery(shelleyDiscoverContainers),
				WithWatchRoots(shelleyWatchRoots),
				WithChangedPathClassifier(shelleyClassifyPath),
				WithMemberLookup(shelleyFindMember),
				WithFingerprint(shelleyFingerprintSource),
				WithContainerParse(shelleyParseContainer),
				WithMemberParse(shelleyParseMember),
				// Special case: confirm a stored conversation still exists for
				// RequireFreshSource lookups.
				WithMemberPresence(shelleyMemberPresent),
			)
		},
	)
}

func shelleyDiscoverContainers(root string) []string {
	if dbPath := shelleyDBPath(root); dbPath != "" {
		return []string{dbPath}
	}
	return nil
}

func shelleyWatchRoots(roots []string) []WatchRoot {
	out := make([]WatchRoot, 0, len(roots))
	for _, root := range roots {
		out = append(out, WatchRoot{
			Path:         root,
			Recursive:    false,
			IncludeGlobs: []string{shelleyDBName, shelleyDBName + "-*"},
			DebounceKey:  string(AgentShelley) + ":db:" + root,
		})
	}
	return out
}

// shelleyClassifyPath maps a stored or changed path to its database container
// and conversation. allowMissing relaxes the regular-file requirement so a
// database delete (or its WAL/SHM sibling) still classifies for changed-path
// tombstones, reproducing the legacy strict sourceRef / lenient
// sourceRefForChangedPath split.
func shelleyClassifyPath(
	root, path string, allowMissing bool,
) (multiSessionMatch, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	requireRegular := !allowMissing
	if dbPath, conversationID, ok := parseShelleyVirtualPath(path); ok {
		if !shelleyDBUnderRoot(root, dbPath, requireRegular) {
			return multiSessionMatch{}, false
		}
		return multiSessionMatch{
			Path:      path,
			Container: dbPath,
			MemberID:  conversationID,
		}, true
	}
	if shelleyDBUnderRoot(root, path, requireRegular) {
		return multiSessionMatch{Path: path, Container: path}, true
	}
	if allowMissing {
		if dbPath, ok := shelleyDBPathForEvent(root, path); ok {
			return multiSessionMatch{Path: dbPath, Container: dbPath}, true
		}
	}
	return multiSessionMatch{}, false
}

// shelleyFindMember resolves a raw conversation ID to its virtual source path
// inside the shared database. The ID is validated only to reject path-like
// input; all conversations live in one DB.
func shelleyFindMember(root, rawID string) (multiSessionMatch, bool) {
	if root == "" || !IsValidSessionID(rawID) {
		return multiSessionMatch{}, false
	}
	dbPath := shelleyDBPath(root)
	if dbPath == "" || !ShelleyConversationExists(dbPath, rawID) {
		return multiSessionMatch{}, false
	}
	return multiSessionMatch{
		Path:      ShelleyVirtualPath(dbPath, rawID),
		Container: dbPath,
		MemberID:  rawID,
	}, true
}

func shelleyFingerprintSource(src multiSessionSource) (SourceFingerprint, error) {
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
		if compositeMtime, err := sqliteDBCompositeMtime(src.Container); err == nil {
			fingerprint.MTimeNS = compositeMtime
		}
		fingerprint.Hash, err = hashJSONLSourceFile(src.Container)
		if err != nil {
			return SourceFingerprint{}, err
		}
		return fingerprint, nil
	}

	conn, err := OpenShelleyDB(src.Container)
	if err != nil {
		return SourceFingerprint{}, err
	}
	defer conn.Close()
	metas, err := ListShelleyConversationMetas(conn, src.Container)
	if err != nil {
		return SourceFingerprint{}, err
	}
	for _, meta := range metas {
		if meta.RawID != src.MemberID {
			continue
		}
		fingerprint.MTimeNS = meta.FileMtime
		fingerprint.Hash = meta.Fingerprint
		return fingerprint, nil
	}
	// The conversation row is gone but the database file is still present.
	// Return a keyed-empty fingerprint without error (matching the db-backed
	// and Kiro tombstone behavior) so the engine proceeds to Parse rather than
	// aborting on the fingerprint. Parse then force-replaces the deleted
	// conversation out of the archive; erroring here would strand the stale
	// session because the engine fingerprints before parsing.
	return SourceFingerprint{}, nil
}

func shelleyMemberPresent(src multiSessionSource) bool {
	if src.MemberID == "" {
		return IsRegularFile(src.Container)
	}
	return ShelleyConversationExists(src.Container, src.MemberID)
}

func shelleyParseMember(
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
		return nil, fmt.Errorf("invalid Shelley session ID: %s", src.MemberID)
	}
	conn, err := OpenShelleyDB(src.Container)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return parseShelleyConversationFromDB(
		conn, src.Container, src.MemberID, req.Machine, dbInfo,
	)
}

func shelleyParseContainer(
	src multiSessionSource, req ParseRequest,
) ([]ParseResult, error) {
	dbInfo, err := os.Stat(src.Container)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat %s: %w", src.Container, err)
	}
	conn, err := OpenShelleyDB(src.Container)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	metas, err := ListShelleyConversationMetas(conn, src.Container)
	if err != nil {
		return nil, err
	}
	results := make([]ParseResult, 0, len(metas))
	for _, meta := range metas {
		result, err := parseShelleyConversationFromDB(
			conn, src.Container, meta.RawID, req.Machine, dbInfo,
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

// shelleyDBPath resolves the shared shelley.db under root, returning "" when
// the root holds no Shelley database.
func shelleyDBPath(root string) string {
	if root == "" {
		return ""
	}
	path := filepath.Join(root, shelleyDBName)
	if !IsRegularFile(path) {
		return ""
	}
	return path
}

func shelleyDBUnderRoot(root, dbPath string, requireRegular bool) bool {
	root = filepath.Clean(root)
	dbPath = filepath.Clean(dbPath)
	rel, ok := relUnder(root, dbPath)
	if !ok || filepath.ToSlash(rel) != shelleyDBName {
		return false
	}
	return !requireRegular || IsRegularFile(dbPath)
}

func shelleyDBPathForEvent(root, path string) (string, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rel, ok := relUnder(root, path)
	if !ok {
		return "", false
	}
	if filepath.ToSlash(rel) == shelleyDBName ||
		(filepath.Dir(rel) == "." &&
			strings.HasPrefix(filepath.Base(rel), shelleyDBName+"-")) {
		return filepath.Join(root, shelleyDBName), true
	}
	return "", false
}

// parseShelleyVirtualPath splits a Shelley virtual source path into its
// physical shelley.db path and raw conversation ID. The container basename
// must be shelley.db and the conversation ID must be non-empty.
func parseShelleyVirtualPath(path string) (string, string, bool) {
	return ParseVirtualSourcePathForBase(path, shelleyDBName)
}

func shelleyProviderCapabilities() Capabilities {
	return Capabilities{
		Source: multiSessionContainerSourceCapabilities(
			CapabilitySupported,
			CapabilitySupported,
		),
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			SessionName:          CapabilitySupported,
			Cwd:                  CapabilitySupported,
			Relationships:        CapabilitySupported,
			Thinking:             CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			PerMessageTokenUsage: CapabilitySupported,
			Model:                CapabilitySupported,
		},
	}
}
