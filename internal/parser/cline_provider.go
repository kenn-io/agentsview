package parser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// newClineProviderFactory creates a provider factory for Cline CLI.
// Cline stores sessions as directories under <root>/data/sessions/<sessionId>/
// with <sessionId>.json (metadata) and <sessionId>.messages.json (transcript).
// Roots may point directly to ~/.cline or to ~/.cline/data/sessions.
func newClineProviderFactory(def AgentDef) ProviderFactory {
	return NewSingleFileProviderFactory(
		def,
		clineProviderCapabilities(),
		func(cfg ProviderConfig) singleFileSourceSet {
			return NewSingleFileSourceSet(
				def.Type,
				cfg.Roots,
				WithStreamingFileDiscovery(clineDiscoverEach),
				WithFileWatchRoots(func(roots []string) []WatchRoot {
					return clineWatchRoots(roots)
				}),
				WithFileChangedPathClassifier(
					func(root, path string, allowMissing bool) (singleFileMatch, bool) {
						return clineClassifyPath(root, path, allowMissing)
					},
				),
				WithFileLookup(func(root, rawID string) (singleFileMatch, bool) {
					return clineFindFile(root, rawID)
				}),
				WithFileFingerprint(func(src singleFileSource) (SourceFingerprint, error) {
					if _, ok := clineClassifyPath(src.Root, src.Path, false); !ok {
						return SourceFingerprint{}, fmt.Errorf(
							"cline source is outside the configured session root: %s",
							src.Path,
						)
					}
					return clineFingerprintSource(src.Path)
				}),
				WithFileStoredSourceHintScope(clineStoredSourceHintScope),
				WithFileParse(func(src singleFileSource, req ParseRequest) ([]ParseResult, []string, error) {
					return clineParseFile(src, req)
				}),
			)
		},
	)
}

// ValidClineSessionID reports whether sessionID is a safe, valid Cline
// session ID that does not escape directories, traverse paths, or contain
// injection characters.
func ValidClineSessionID(sessionID string) bool {
	if sessionID == "" || strings.HasPrefix(sessionID, "_") || strings.HasPrefix(sessionID, ".") ||
		strings.ContainsAny(sessionID, "\\/:\x00") || !isSafeSinglePathComponent(sessionID) {
		return false
	}
	return true
}

// IsClineTeammateMessagesFile reports whether filename represents a Cline
// teammate subagent transcript (e.g. "<agentId>__<suffix>.messages.json")
// belonging to the session directory.
func IsClineTeammateMessagesFile(sessionID, filename string) bool {
	if !strings.HasSuffix(filename, ".messages.json") {
		return false
	}
	if filename == sessionID+".messages.json" {
		return false
	}
	base := strings.TrimSuffix(filename, ".messages.json")
	if base == "" || strings.HasPrefix(base, ".") || strings.HasPrefix(base, "_") ||
		strings.ContainsAny(base, "\\/:\x00") || !isSafeSinglePathComponent(base) {
		return false
	}
	idx := strings.Index(base, "__")
	if idx <= 0 || idx+2 >= len(base) || strings.HasSuffix(base, "__") {
		return false
	}
	return true
}

// ClineResolveSessionsDir resolves the directory containing Cline session
// folders. If root is already a direct sessions directory (named "sessions"
// or ending with "data/sessions"), root is returned; otherwise, "data/sessions"
// under root is returned.
func ClineResolveSessionsDir(root string) string {
	clean := filepath.Clean(root)
	if strings.HasSuffix(filepath.ToSlash(clean), "data/sessions") || filepath.Base(clean) == "sessions" {
		return clean
	}
	return filepath.Join(clean, "data", "sessions")
}

func clineResolveSessionsDir(root string) string {
	return ClineResolveSessionsDir(root)
}

func clineDiscoverEach(
	ctx context.Context, root string, yield func(singleFileMatch) error,
) error {
	sessionsDir := clineResolveSessionsDir(root)
	return streamDirectoryEntries(ctx, sessionsDir, func(entry os.DirEntry) error {
		if !entry.IsDir() || !ValidClineSessionID(entry.Name()) {
			return nil
		}
		sessionID := entry.Name()
		sessionDir := filepath.Join(sessionsDir, sessionID)
		if !clineSessionDirectoryWithinRoot(root, sessionDir, false) {
			return nil
		}
		metaPath := filepath.Join(sessionsDir, sessionID, sessionID+".json")
		info, err := clineRegularFileInfo(metaPath, true)
		if err != nil || info == nil {
			return nil //nolint:nilerr // Missing or unreadable optional session companions are skipped during discovery.
		}

		messagesPath := filepath.Join(
			filepath.Dir(metaPath), sessionID+".messages.json",
		)
		if _, err := clineRegularFileInfo(messagesPath, true); err != nil {
			return nil //nolint:nilerr // Missing or unreadable optional session companions are skipped during discovery.
		}
		return yield(singleFileMatch{Path: metaPath})
	})
}

// clineRegularFileInfo accepts only real regular files. Cline primary files
// must not be read through symlinks because the target is outside the source
// fingerprint and could otherwise be archived as an unrelated session.
func clineRegularFileInfo(path string, allowMissing bool) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) && allowMissing {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("stat %s: source is not a regular file", path)
	}
	return info, nil
}

// clineSessionDirectoryWithinRoot validates the directory component that
// owns a Cline session. The lexical check prevents traversal, and Lstat on
// every component from the configured root through the session directory
// rejects symlinks before any Cline file is read. Walking the components also
// avoids relying on EvalSymlinks, which can fail with Access Denied on Windows
// even for ordinary directories when symlink privileges are unavailable.
// A missing session directory remains valid only for changed-path tombstone
// classification.
func clineSessionDirectoryWithinRoot(
	root, sessionDir string, allowMissing bool,
) bool {
	root = filepath.Clean(root)
	sessionsDir := clineResolveSessionsDir(root)
	sessionDir = filepath.Clean(sessionDir)
	if !isWithinRoot(sessionsDir, sessionDir) || sessionDir == sessionsDir {
		return false
	}

	rel, err := filepath.Rel(root, sessionDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	current := root
	for component := range strings.SplitSeq(rel, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return allowMissing && filepath.Clean(current) == sessionDir
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return false
		}
	}
	return true
}

func clineWatchRoots(roots []string) []WatchRoot {
	out := make([]WatchRoot, 0, len(roots))
	for _, root := range roots {
		sessionsDir := clineResolveSessionsDir(root)
		out = append(out, WatchRoot{
			Path:         sessionsDir,
			Recursive:    true,
			IncludeGlobs: []string{"*.json"},
			DebounceKey:  "cline:sessions:" + root,
		})
	}
	return out
}

func clineClassifyPath(
	root, path string, allowMissing bool,
) (singleFileMatch, bool) {
	sessionDir, sessionID, filename, ok := clineSessionPathParts(
		root, path, allowMissing,
	)
	if !ok || !isClineSessionFileName(sessionID, filename) {
		return singleFileMatch{}, false
	}

	metaPath := filepath.Join(sessionDir, sessionID+".json")
	info, err := clineRegularFileInfo(metaPath, allowMissing)
	if err == nil && info != nil {
		return singleFileMatch{Path: metaPath}, true
	}
	return singleFileMatch{}, false
}

func clineSessionPathParts(
	root, path string, allowMissing bool,
) (sessionDir, sessionID, filename string, ok bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	sessionsDir := clineResolveSessionsDir(root)

	rel, err := filepath.Rel(sessionsDir, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", "", "", false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 2 {
		return "", "", "", false
	}

	sessionID = parts[0]
	filename = parts[1]
	if !ValidClineSessionID(sessionID) {
		return "", "", "", false
	}
	sessionDir = filepath.Join(sessionsDir, sessionID)
	if !clineSessionDirectoryWithinRoot(root, sessionDir, allowMissing) {
		return "", "", "", false
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return "", "", "", false
		}
	} else if !os.IsNotExist(err) {
		return "", "", "", false
	}
	return sessionDir, sessionID, filename, true
}

func isClineSessionFileName(sessionID, filename string) bool {
	return filename == sessionID+".json" ||
		filename == sessionID+".messages.json" ||
		IsClineTeammateMessagesFile(sessionID, filename)
}

// clineStoredSourceHintScope is the single-file stored-source-hint-scope hook
// for Cline: for any changed metadata or teammate transcript path, classify
// the path (allowing missing tombstone paths), resolve the owning session
// directory, and return it as the complete-result ownership scope. Teammate
// paths are ordinary descendants of the session directory, not path#member
// virtual paths, so IncludeVirtualMembers stays false.
func clineStoredSourceHintScope(root, path string) (StoredSourceHintScope, bool) {
	sessionDir, sessionID, filename, ok := clineSessionPathParts(root, path, true)
	if !ok || !isClineSessionFileName(sessionID, filename) {
		return StoredSourceHintScope{}, false
	}
	return StoredSourceHintScope{
		Path:                  sessionDir,
		IncludeVirtualMembers: false,
	}, true
}

// clineFindFile resolves a raw Cline session ID back to the owning session
// metadata file. Accepted shapes are the parent ID "<id>" and the teammate ID
// "<id>__teamtask__<suffix>" that Cline writes into teammate transcripts.
func clineFindFile(root, rawID string) (singleFileMatch, bool) {
	if !isSafeSinglePathComponent(rawID) || strings.ContainsAny(rawID, "\\/:\x00") {
		return singleFileMatch{}, false
	}
	sessionID := rawID
	if before, suffix, found := strings.Cut(rawID, "__teamtask__"); found {
		if suffix == "" {
			return singleFileMatch{}, false
		}
		sessionID = before
	}
	if !ValidClineSessionID(sessionID) {
		return singleFileMatch{}, false
	}
	sessionsDir := clineResolveSessionsDir(root)
	sessionDir := filepath.Join(sessionsDir, sessionID)
	if !clineSessionDirectoryWithinRoot(root, sessionDir, false) {
		return singleFileMatch{}, false
	}
	metaPath := filepath.Join(sessionDir, sessionID+".json")
	if !isWithinRoot(sessionsDir, metaPath) {
		return singleFileMatch{}, false
	}
	if _, err := clineRegularFileInfo(metaPath, false); err == nil {
		return singleFileMatch{Path: metaPath}, true
	}
	return singleFileMatch{}, false
}

func clineParseFile(
	src singleFileSource, req ParseRequest,
) ([]ParseResult, []string, error) {
	if _, ok := clineClassifyPath(src.Root, src.Path, false); !ok {
		return nil, nil, fmt.Errorf(
			"cline source is outside the configured session root: %s", src.Path,
		)
	}
	results, err := parseClineSessionWithTeammates(
		src.Path, req.Source.ProjectHint, req.Machine,
	)
	if err != nil {
		return nil, nil, err
	}
	if len(results) == 0 {
		return nil, nil, nil
	}

	if req.Fingerprint.Size > 0 {
		results[0].Session.File.Size = req.Fingerprint.Size
	}
	if req.Fingerprint.MTimeNS > 0 {
		results[0].Session.File.Mtime = req.Fingerprint.MTimeNS
	}
	if req.Fingerprint.Hash != "" {
		for i := range results {
			results[i].Session.File.Hash = req.Fingerprint.Hash
		}
	}

	return results, nil, nil
}

func clineProviderCapabilities() Capabilities {
	sourceCaps := jsonlFileProviderSourceCapabilities()
	sourceCaps.StoredSourceHints = CapabilitySupported
	return Capabilities{
		Source: sourceCaps,
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			SessionName:          CapabilitySupported,
			Cwd:                  CapabilitySupported,
			GitBranch:            CapabilitySupported,
			Thinking:             CapabilitySupported,
			Model:                CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			ToolResultEvents:     CapabilitySupported,
			PerMessageTokenUsage: CapabilitySupported,
			AggregateUsageEvents: CapabilitySupported,
			TerminationStatus:    CapabilitySupported,
			MalformedLineCount:   CapabilityNotApplicable,
		},
	}
}
