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
					return clineFingerprintSource(src.Path)
				}),
				WithFileParse(func(src singleFileSource, req ParseRequest) ([]ParseResult, []string, error) {
					return clineParseFile(src, req)
				}),
			)
		},
	)
}

func clineResolveSessionsDir(root string) string {
	clean := filepath.Clean(root)
	if strings.HasSuffix(filepath.ToSlash(clean), "data/sessions") || filepath.Base(clean) == "sessions" {
		return clean
	}
	return filepath.Join(clean, "data", "sessions")
}

func clineDiscoverEach(
	ctx context.Context, root string, yield func(singleFileMatch) error,
) error {
	sessionsDir := clineResolveSessionsDir(root)
	return streamDirectoryEntries(ctx, sessionsDir, func(entry os.DirEntry) error {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), "_") || strings.HasPrefix(entry.Name(), ".") {
			return nil
		}
		sessionID := entry.Name()
		metaPath := filepath.Join(sessionsDir, sessionID, sessionID+".json")
		info, err := os.Lstat(metaPath)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return fmt.Errorf("stat cline session %s: %w", metaPath, err)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		return yield(singleFileMatch{Path: metaPath})
	})
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
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	sessionsDir := clineResolveSessionsDir(root)

	rel, err := filepath.Rel(sessionsDir, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return singleFileMatch{}, false
	}

	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 2 {
		return singleFileMatch{}, false
	}

	sessionID := parts[0]
	filename := parts[1]

	if strings.HasPrefix(sessionID, "_") || strings.HasPrefix(sessionID, ".") {
		return singleFileMatch{}, false
	}

	if filename != sessionID+".json" && filename != sessionID+".messages.json" {
		return singleFileMatch{}, false
	}

	metaPath := filepath.Join(sessionsDir, sessionID, sessionID+".json")
	if allowMissing {
		return singleFileMatch{Path: metaPath}, true
	}
	if IsRegularFile(metaPath) {
		return singleFileMatch{Path: metaPath}, true
	}
	return singleFileMatch{}, false
}

func clineFindFile(root, rawID string) (singleFileMatch, bool) {
	if !isSafeSinglePathComponent(rawID) {
		return singleFileMatch{}, false
	}
	sessionsDir := clineResolveSessionsDir(root)
	metaPath := filepath.Join(sessionsDir, rawID, rawID+".json")
	if !isWithinRoot(sessionsDir, metaPath) {
		return singleFileMatch{}, false
	}
	if IsRegularFile(metaPath) {
		return singleFileMatch{Path: metaPath}, true
	}
	return singleFileMatch{}, false
}

func clineParseFile(
	src singleFileSource, req ParseRequest,
) ([]ParseResult, []string, error) {
	sess, msgs, err := parseClineSession(
		src.Path, req.Source.ProjectHint, req.Machine,
	)
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

	return []ParseResult{{
		Session:     *sess,
		Messages:    msgs,
		UsageEvents: sess.UsageEvents,
	}}, nil, nil
}

func clineProviderCapabilities() Capabilities {
	return Capabilities{
		Source: jsonlFileProviderSourceCapabilities(),
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
