package parser

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// newRooCodeProviderFactory creates a provider factory for RooCode.
// RooCode stores sessions as task directories under <root>/tasks/<taskId>/
// with history_item.json (metadata) and ui_messages.json (transcript).
func newRooCodeProviderFactory(def AgentDef) ProviderFactory {
	return NewSingleFileProviderFactory(
		def,
		rooCodeProviderCapabilities(),
		func(cfg ProviderConfig) singleFileSourceSet {
			return NewSingleFileSourceSet(
				def.Type,
				cfg.Roots,
				WithStreamingFileDiscovery(rooCodeDiscoverEach),
				WithFileWatchRoots(func(roots []string) []WatchRoot {
					return rooCodeWatchRoots(roots)
				}),
				WithFileChangedPathClassifier(
					func(root, path string, allowMissing bool) (singleFileMatch, bool) {
						return rooCodeClassifyPath(root, path, allowMissing)
					},
				),
				WithFileLookup(func(root, rawID string) (singleFileMatch, bool) {
					return rooCodeFindFile(root, rawID)
				}),
				WithFileFingerprint(func(src singleFileSource) (SourceFingerprint, error) {
					return rooCodeFingerprintSource(src.Path)
				}),
				WithFileParse(func(src singleFileSource, req ParseRequest) ([]ParseResult, []string, error) {
					return rooCodeParseFile(src, req)
				}),
			)
		},
	)
}

// rooCodeDiscoverEach streams the RooCode session sources under a root.
// root is the globalStorage directory; sessions live under
// <root>/tasks/<taskId>/history_item.json. A missing tasks directory is a
// legitimately empty scope, but any other traversal failure propagates so the
// sync engine treats the scope as incomplete instead of tombstoning every
// baselined session against an authoritative empty enumeration.
func rooCodeDiscoverEach(
	ctx context.Context, root string, yield func(singleFileMatch) error,
) error {
	tasksDir := filepath.Join(root, "tasks")
	return streamDirectoryEntries(ctx, tasksDir, func(entry os.DirEntry) error {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), "_") {
			return nil
		}
		historyPath := filepath.Join(tasksDir, entry.Name(), "history_item.json")
		if !IsRegularFile(historyPath) {
			return nil
		}
		return yield(singleFileMatch{Path: historyPath})
	})
}

// rooCodeWatchRoots creates watch plans for each root.
// Watches the tasks/ subdirectory recursively for history_item.json
// and ui_messages.json changes.
func rooCodeWatchRoots(roots []string) []WatchRoot {
	out := make([]WatchRoot, 0, len(roots))
	for _, root := range roots {
		tasksDir := filepath.Join(root, "tasks")
		out = append(out, WatchRoot{
			Path:         tasksDir,
			Recursive:    true,
			IncludeGlobs: []string{"history_item.json", "ui_messages.json"},
			DebounceKey:  "roocode:sessions:" + root,
		})
	}
	return out
}

// rooCodeClassifyPath maps a changed path back to its source
// history_item.json. Paths are shaped like:
// <root>/tasks/<taskId>/history_item.json or .../ui_messages.json
func rooCodeClassifyPath(
	root, path string, allowMissing bool,
) (singleFileMatch, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)

	// The path should be under root/tasks/.
	tasksRoot := filepath.Join(root, "tasks")
	rel, err := filepath.Rel(tasksRoot, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return singleFileMatch{}, false
	}

	parts := strings.Split(rel, string(filepath.Separator))
	// Expected: <taskId>/<filename>
	if len(parts) != 2 {
		return singleFileMatch{}, false
	}

	taskID := parts[0]
	filename := parts[1]
	if filename != "history_item.json" && filename != "ui_messages.json" {
		return singleFileMatch{}, false
	}

	// Skip _index.json and other underscore-prefixed metadata files.
	if strings.HasPrefix(taskID, "_") {
		return singleFileMatch{}, false
	}

	historyPath := filepath.Join(tasksRoot, taskID, "history_item.json")

	if allowMissing {
		return singleFileMatch{Path: historyPath}, true
	}

	if IsRegularFile(historyPath) {
		return singleFileMatch{Path: historyPath}, true
	}
	return singleFileMatch{}, false
}

// rooCodeFindFile finds a session by raw task ID under the root.
func rooCodeFindFile(root, rawID string) (singleFileMatch, bool) {
	historyPath := filepath.Join(root, "tasks", rawID, "history_item.json")
	if IsRegularFile(historyPath) {
		return singleFileMatch{Path: historyPath}, true
	}
	return singleFileMatch{}, false
}

// rooCodeParseFile parses a single RooCode session from a task directory.
func rooCodeParseFile(
	src singleFileSource, req ParseRequest,
) ([]ParseResult, []string, error) {
	taskDir := filepath.Dir(src.Path)

	sess, msgs, err := parseRooCodeSession(
		taskDir, req.Source.ProjectHint, req.Machine,
	)
	if err != nil {
		return nil, nil, err
	}
	if sess == nil {
		return nil, nil, nil
	}

	// Apply fingerprint metadata.
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

func rooCodeProviderCapabilities() Capabilities {
	return Capabilities{
		Source: jsonlFileProviderSourceCapabilities(),
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			SessionName:          CapabilitySupported,
			Cwd:                  CapabilitySupported,
			Thinking:             CapabilitySupported,
			Model:                CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			ToolResultEvents:     CapabilitySupported,
			Subagents:            CapabilitySupported,
			AggregateUsageEvents: CapabilitySupported,
			Relationships:        CapabilitySupported,
			TerminationStatus:    CapabilitySupported,
			MalformedLineCount:   CapabilityNotApplicable,
		},
	}
}
