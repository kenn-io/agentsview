package parser

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Vibe stores each session in <root>/session_<YYYYMMDD>_<HHMMSS>_<uuid>/, with a
// messages.jsonl transcript and a sibling meta.json. It is a single-file
// provider: one transcript parses into one session, with a composite fingerprint
// folding in meta.json and a fallback-ID exclusion when meta.json later supplies
// a different session_id. All behavior is wired into the shared single-file base
// via options.
func newVibeProviderFactory(def AgentDef) ProviderFactory {
	return NewSingleFileProviderFactory(
		def,
		vibeProviderCapabilities(),
		func(cfg ProviderConfig) singleFileSourceSet {
			return NewSingleFileSourceSet(
				AgentVibe,
				cfg.Roots,
				WithFileDiscovery(vibeDiscoverFiles),
				WithFileWatchRoots(vibeWatchRoots),
				WithFileChangedPathClassifier(vibeClassifyPath),
				WithFileLookup(vibeFindFile),
				WithFileFingerprint(vibeFingerprintSource),
				WithFileParse(vibeParseFile),
			)
		},
	)
}

func vibeDiscoverFiles(root string) []singleFileMatch {
	var out []singleFileMatch
	for _, path := range discoverVibeSessionPaths(root) {
		if match, ok := vibeStrictMatch(root, path); ok {
			out = append(out, match)
		}
	}
	return out
}

// discoverVibeSessionPaths finds all Vibe messages.jsonl paths under root.
// Symlinked session directories are followed (matching the watcher), but only
// session_-prefixed directories that hold a regular messages.jsonl qualify.
func discoverVibeSessionPaths(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var paths []string
	for _, entry := range entries {
		if !isDirOrSymlink(entry, root) {
			continue
		}
		if !isVibeSessionDirName(entry.Name()) {
			continue
		}
		messagesPath := filepath.Join(root, entry.Name(), "messages.jsonl")
		if isVibeMessagesFile(messagesPath) {
			paths = append(paths, messagesPath)
		}
	}
	return paths
}

func vibeWatchRoots(roots []string) []WatchRoot {
	out := make([]WatchRoot, 0, len(roots))
	for _, root := range roots {
		out = append(out, WatchRoot{
			Path:         root,
			Recursive:    true,
			IncludeGlobs: []string{"messages.jsonl", "meta.json"},
			DebounceKey:  string(AgentVibe) + ":sessions:" + root,
		})
	}
	return out
}

// vibeClassifyPath maps a messages.jsonl or meta.json event path to its session
// transcript. Under allowMissing a transcript that does not (yet) exist still
// classifies via the session directory name, so a metadata-only event or a
// deletion still resolves.
func vibeClassifyPath(
	root, path string, allowMissing bool,
) (singleFileMatch, bool) {
	rel, ok := vibeRelPath(root, path)
	if !ok {
		return singleFileMatch{}, false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 2 || !isVibeSessionDirName(parts[0]) {
		return singleFileMatch{}, false
	}
	messagesPath := filepath.Join(filepath.Clean(root), parts[0], "messages.jsonl")
	switch parts[1] {
	case "messages.jsonl":
		if allowMissing {
			return vibeMatchFromSessionDir(parts[0], messagesPath)
		}
		return vibeStrictMatch(root, messagesPath)
	case "meta.json":
		if allowMissing && !isVibeMessagesFile(messagesPath) {
			return vibeMatchFromSessionDir(parts[0], messagesPath)
		}
		return vibeStrictMatch(root, messagesPath)
	default:
		return singleFileMatch{}, false
	}
}

// vibeStrictMatch requires the messages.jsonl to exist as a regular file under a
// session directory before classifying it.
func vibeStrictMatch(root, path string) (singleFileMatch, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if !isVibeMessagesFile(path) {
		return singleFileMatch{}, false
	}
	rel, ok := vibeRelPath(root, path)
	if !ok {
		return singleFileMatch{}, false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 2 || !isVibeSessionDirName(parts[0]) ||
		parts[1] != "messages.jsonl" {
		return singleFileMatch{}, false
	}
	return vibeMatchFromSessionDir(parts[0], path)
}

func vibeMatchFromSessionDir(sessionDir, path string) (singleFileMatch, bool) {
	if !isVibeSessionDirName(sessionDir) {
		return singleFileMatch{}, false
	}
	return singleFileMatch{Path: path, ProjectHint: sessionDir}, true
}

func vibeFindFile(root, rawID string) (singleFileMatch, bool) {
	path := findVibeSourceFile(root, rawID)
	if path == "" {
		return singleFileMatch{}, false
	}
	return vibeStrictMatch(root, path)
}

// findVibeSourceFile locates a Vibe session by ID under root. The ID is the
// session_id from meta.json (a uuid), which usually differs from the session
// directory name, so a direct directory-name path is tried before scanning
// meta.json files.
func findVibeSourceFile(root, sessionID string) string {
	if messagesPath := filepath.Join(
		root, sessionID, "messages.jsonl",
	); isVibeMessagesFile(messagesPath) {
		return messagesPath
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if !isDirOrSymlink(entry, root) ||
			!strings.HasPrefix(entry.Name(), "session_") {
			continue
		}
		messagesPath := filepath.Join(root, entry.Name(), "messages.jsonl")
		if !isVibeMessagesFile(messagesPath) {
			continue
		}
		metaPath := filepath.Join(root, entry.Name(), "meta.json")
		if meta, err := parseVibeMetadata(metaPath); err == nil &&
			meta.SessionID == sessionID {
			return messagesPath
		}
	}
	return ""
}

// isVibeMessagesFile reports whether path is an existing regular file.
func isVibeMessagesFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info == nil {
		return false
	}
	return !info.IsDir()
}

func vibeFingerprintSource(src singleFileSource) (SourceFingerprint, error) {
	info, err := os.Stat(src.Path)
	if err != nil {
		return SourceFingerprint{}, fmt.Errorf("stat %s: %w", src.Path, err)
	}
	if info.IsDir() {
		return SourceFingerprint{}, fmt.Errorf(
			"stat %s: source is a directory", src.Path,
		)
	}
	size := info.Size()
	mtime := info.ModTime().UnixNano()
	metaPath := vibeMetaPath(src.Path)
	if metaInfo, err := os.Stat(metaPath); err == nil {
		size += metaInfo.Size()
		if metaMTime := metaInfo.ModTime().UnixNano(); metaMTime > mtime {
			mtime = metaMTime
		}
	}
	hash, err := hashJSONLSourceFile(src.Path)
	if err != nil {
		return SourceFingerprint{}, err
	}
	return SourceFingerprint{
		Size:    size,
		MTimeNS: mtime,
		Hash:    hash,
	}, nil
}

func vibeParseFile(
	src singleFileSource, req ParseRequest,
) ([]ParseResult, []string, error) {
	sess, msgs, usageEvents, err := parseVibeSession(src.Path, "", req.Machine)
	if err != nil {
		return nil, nil, err
	}
	if sess == nil {
		return nil, nil, nil
	}
	if req.Fingerprint.Size > 0 {
		sess.File.Size = req.Fingerprint.Size
	}
	if req.Fingerprint.MTimeNS > 0 {
		sess.File.Mtime = req.Fingerprint.MTimeNS
	}
	if req.Fingerprint.Hash != "" {
		sess.File.Hash = req.Fingerprint.Hash
	}
	excluded := vibeProviderExcludedSessionIDs(src.Path, sess.ID)
	return []ParseResult{{
		Session:     *sess,
		Messages:    msgs,
		UsageEvents: usageEvents,
	}}, excluded, nil
}

func vibeRelPath(root, path string) (string, bool) {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil || rel == "." || rel == "" {
		return "", false
	}
	if strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return "", false
	}
	for part := range strings.SplitSeq(rel, string(filepath.Separator)) {
		if part == "" || part == "." || part == ".." {
			return "", false
		}
	}
	return rel, true
}

func isVibeSessionDirName(name string) bool {
	return strings.HasPrefix(name, "session_") && strings.Contains(name, "_")
}

func vibeMetaPath(messagesPath string) string {
	return filepath.Join(filepath.Dir(messagesPath), "meta.json")
}

func vibeProviderExcludedSessionIDs(path, currentID string) []string {
	fallbackID := string(AgentVibe) + ":" + filepath.Base(filepath.Dir(path))
	if currentID == "" || currentID == fallbackID {
		return nil
	}
	return []string{fallbackID}
}

func vibeProviderCapabilities() Capabilities {
	return Capabilities{
		Source: SourceCapabilities{
			DiscoverSources:      CapabilitySupported,
			WatchSources:         CapabilitySupported,
			ClassifyChangedPath:  CapabilitySupported,
			FindSource:           CapabilitySupported,
			CompositeFingerprint: CapabilitySupported,
			MultiSessionSource:   CapabilityNotApplicable,
			PerSessionErrors:     CapabilityNotApplicable,
			ExcludedSessions:     CapabilitySupported,
			ForceReplaceOnParse:  CapabilityNotApplicable,
		},
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			SessionName:          CapabilitySupported,
			Cwd:                  CapabilitySupported,
			GitBranch:            CapabilitySupported,
			Relationships:        CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			AggregateUsageEvents: CapabilitySupported,
			Model:                CapabilitySupported,
		},
	}
}
