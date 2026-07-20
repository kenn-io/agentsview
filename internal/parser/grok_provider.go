package parser

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func newGrokProviderFactory(def AgentDef) ProviderFactory {
	return NewSingleFileProviderFactory(
		def,
		grokProviderCapabilities(),
		func(cfg ProviderConfig) singleFileSourceSet {
			return NewSingleFileSourceSet(
				AgentGrok,
				cfg.Roots,
				WithFileDiscovery(grokDiscoverFiles),
				WithFileWatchRoots(grokWatchRoots),
				WithFileChangedPathClassifier(grokClassifyPath),
				WithFileLookup(grokFindFile),
				WithFileFingerprint(grokFingerprintSource),
				WithFileParse(grokParseFile),
			)
		},
	)
}

func grokDiscoverFiles(root string) []singleFileMatch {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []singleFileMatch
	for _, entry := range entries {
		if !isDirOrSymlink(entry, root) {
			continue
		}
		cwdRoot := filepath.Join(root, entry.Name())
		sessionEntries, err := os.ReadDir(cwdRoot)
		if err != nil {
			continue
		}
		for _, sessionEntry := range sessionEntries {
			if !isDirOrSymlink(sessionEntry, cwdRoot) ||
				!IsValidSessionID(sessionEntry.Name()) {
				continue
			}
			summaryPath := filepath.Join(
				cwdRoot, sessionEntry.Name(), "summary.json",
			)
			if match, ok := grokStrictMatch(root, summaryPath); ok {
				out = append(out, match)
			}
		}
	}
	return out
}

func grokWatchRoots(roots []string) []WatchRoot {
	out := make([]WatchRoot, 0, len(roots))
	for _, root := range roots {
		out = append(out, WatchRoot{
			Path:         root,
			Recursive:    true,
			IncludeGlobs: []string{"summary.json", "signals.json", "chat_history.jsonl", "updates.jsonl"},
			DebounceKey:  string(AgentGrok) + ":sessions:" + root,
		})
	}
	return out
}

func grokClassifyPath(
	root, path string, allowMissing bool,
) (singleFileMatch, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return singleFileMatch{}, false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 3 || !IsValidSessionID(parts[1]) ||
		!grokTrackedFileName(parts[2]) {
		return singleFileMatch{}, false
	}
	summaryPath := filepath.Join(root, parts[0], parts[1], "summary.json")
	if allowMissing {
		return singleFileMatch{
			Path:        summaryPath,
			ProjectHint: parts[0],
		}, true
	}
	return grokStrictMatch(root, summaryPath)
}

func grokFindFile(root, rawID string) (singleFileMatch, bool) {
	if !IsValidSessionID(rawID) {
		return singleFileMatch{}, false
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return singleFileMatch{}, false
	}
	for _, entry := range entries {
		if !isDirOrSymlink(entry, root) {
			continue
		}
		summaryPath := filepath.Join(root, entry.Name(), rawID, "summary.json")
		if match, ok := grokStrictMatch(root, summaryPath); ok {
			return match, true
		}
	}
	return singleFileMatch{}, false
}

func grokStrictMatch(root, path string) (singleFileMatch, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return singleFileMatch{}, false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return singleFileMatch{}, false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 3 || parts[2] != "summary.json" ||
		!IsValidSessionID(parts[1]) {
		return singleFileMatch{}, false
	}
	return singleFileMatch{
		Path:        path,
		ProjectHint: parts[0],
	}, true
}

func grokTrackedFileName(name string) bool {
	switch name {
	case "summary.json", "signals.json", "chat_history.jsonl", "updates.jsonl":
		return true
	default:
		return false
	}
}

func grokFingerprintSource(src singleFileSource) (SourceFingerprint, error) {
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
	h := sha256.New()
	if err := addSiblingMetadataFingerprintPart(
		h, "summary", src.Path, info,
	); err != nil {
		return SourceFingerprint{}, err
	}
	companions := grokCompanionFiles(src.Path)
	labels := make([]string, 0, len(companions))
	for label := range companions {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	for _, label := range labels {
		companion := companions[label]
		companionInfo, err := siblingMetadataFileInfo(companion)
		if err != nil {
			return SourceFingerprint{}, err
		}
		if companionInfo == nil {
			continue
		}
		size += companionInfo.Size()
		if ts := companionInfo.ModTime().UnixNano(); ts > mtime {
			mtime = ts
		}
		if err := addSiblingMetadataFingerprintPart(
			h, label, companion, companionInfo,
		); err != nil {
			return SourceFingerprint{}, err
		}
	}
	return SourceFingerprint{
		Size:    size,
		MTimeNS: mtime,
		Hash:    fmt.Sprintf("%x", h.Sum(nil)),
	}, nil
}

func grokCompanionFiles(summaryPath string) map[string]string {
	dir := filepath.Dir(summaryPath)
	return map[string]string{
		"signals":      filepath.Join(dir, "signals.json"),
		"chat_history": filepath.Join(dir, "chat_history.jsonl"),
		"updates":      filepath.Join(dir, "updates.jsonl"),
	}
}

func grokParseFile(
	src singleFileSource, req ParseRequest,
) ([]ParseResult, []string, error) {
	result, err := ParseGrokSummary(src.Path, req.Source.ProjectHint, req.Machine)
	if err != nil {
		return nil, nil, err
	}
	if req.Fingerprint.Size > 0 {
		result.Session.File.Size = req.Fingerprint.Size
	}
	if req.Fingerprint.MTimeNS > 0 {
		result.Session.File.Mtime = req.Fingerprint.MTimeNS
	}
	if req.Fingerprint.Hash != "" {
		result.Session.File.Hash = req.Fingerprint.Hash
	}
	return []ParseResult{result}, nil, nil
}

func grokProviderCapabilities() Capabilities {
	return Capabilities{
		Source: jsonlFileProviderSourceCapabilities(),
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			SessionName:          CapabilitySupported,
			TerminationStatus:    CapabilityNotApplicable,
			MalformedLineCount:   CapabilityNotApplicable,
			AggregateUsageEvents: CapabilitySupported,
		},
	}
}
