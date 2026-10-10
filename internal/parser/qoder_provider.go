package parser

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
)

func newQoderProviderFactory(def AgentDef) ProviderFactory {
	return NewSourceSetFactory(
		def,
		qoderProviderCapabilities(),
		func(cfg ProviderConfig) SourceSet {
			var titleRoots []string
			if cfg.PathRewriter == nil {
				for _, root := range cfg.Roots {
					owner := cfg.SourceMachines[root]
					if owner == "" || owner == cfg.Machine {
						titleRoots = append(titleRoots, root)
					}
				}
			}
			return newQoderSourceSetWithTitleRoots(cfg.Roots, titleRoots)
		},
	)
}

func newQoderSourceSet(roots []string) JSONLSourceSet {
	return newQoderSourceSetWithTitleRoots(roots, roots)
}

func newQoderSourceSetWithTitleRoots(roots, titleRoots []string) JSONLSourceSet {
	return NewJSONLSourceSet(AgentQoder, roots,
		WithRecursive(),
		WithSymlinkFollowing(),
		WithContentHashing(),
		WithIncludePath(isQoderSourcePath),
		WithProjectHint(qoderProjectHintFromPath),
		WithSessionIDFromPath(qoderSessionIDFromPath),
		WithLookupIDValid(isQoderLookupID),
		// The title database lives in Application Support, outside every
		// session root, and the database is bound to this provider's roots.
		// The ParseRequest carries no roots, so capture them here.
		WithParseFile(func(ctx context.Context, path string, req ParseRequest) ([]ParseResult, []string, error) {
			// Filtering foreign roots alone leaves a local ancestor eligible.
			// Resolve ownership against all configured roots before lookup.
			ownerRoot := ""
			cleanPath := filepath.Clean(path)
			bestLength := 0
			for _, root := range roots {
				cleanRoot := filepath.Clean(root)
				if (cleanPath == cleanRoot || strings.HasPrefix(cleanPath, cleanRoot+string(filepath.Separator))) && len(cleanRoot) > bestLength {
					ownerRoot = root
					bestLength = len(cleanRoot)
				}
			}
			parseRoots := titleRoots
			if ownerRoot == "" || !slices.Contains(titleRoots, ownerRoot) {
				parseRoots = nil
			}
			return parseQoderFile(ctx, path, parseRoots, req)
		}),
		WithForceReplace(),
		WithCompanionFiles(qoderCompanionFiles),
		WithCompanionTranscript(qoderCompanionTranscript),
		WithExtraWatchRoots(qoderTitleDatabaseWatchRoots(titleRoots)...),
	)
}

func parseQoderFile(
	ctx context.Context, path string, roots []string, req ParseRequest,
) ([]ParseResult, []string, error) {
	results, excluded, err := ParseQoderSessionWithExclusions(
		path, req.Source.ProjectHint, req.Machine,
	)
	if err != nil {
		return nil, nil, err
	}
	if _, _, isSubagent := qoderPathIDs(path, req.Source.ProjectHint); !isSubagent {
		// A subagent transcript has no title of its own and must never
		// inherit its parent's; skip the lookup entirely.
		retryReason := applyQoderTitles(ctx, path, roots, results)
		for i := range results {
			results[i].TitleRetryReason = retryReason
		}
	}
	for i := range results {
		if req.Fingerprint.Size > 0 {
			results[i].Session.File.Size = req.Fingerprint.Size
		}
		if req.Fingerprint.MTimeNS > 0 {
			results[i].Session.File.Mtime = req.Fingerprint.MTimeNS
		}
		if req.Fingerprint.Hash != "" {
			results[i].Session.File.Hash = req.Fingerprint.Hash
		}
	}
	return results, excluded, nil
}

// qoderTitleDatabaseWatchRoots returns the watch roots for the title
// databases reachable from the configured Qoder session roots. Only roots
// whose client can be identified unambiguously are registered, and only when
// the database already exists: a database created later is picked up by the
// next title sweep rather than by inventing a path for a platform this
// machine does not have.
func qoderTitleDatabaseWatchRoots(roots []string) []WatchRoot {
	var watchRoots []WatchRoot
	for _, databasePath := range qoderTitleDatabasePathsForRoots(roots) {
		if !IsRegularFile(databasePath) {
			continue
		}
		dir := filepath.Dir(databasePath)
		watchRoots = append(watchRoots, WatchRoot{
			Path:         dir,
			Recursive:    false,
			IncludeGlobs: []string{qoderAppDatabaseName, qoderAppDatabaseName + "-wal"},
			DebounceKey:  string(AgentQoder) + ":titles:" + dir,
		})
	}
	return watchRoots
}

func isQoderSourcePath(root, path string) bool {
	parts, ok := qoderPathParts(root, path)
	if !ok {
		return false
	}
	switch len(parts) {
	case 1:
		// SharedClientCache flat layout: file directly under root.
		stem, ok := strings.CutSuffix(parts[0], ".jsonl")
		return ok &&
			!strings.HasPrefix(stem, "agent-") &&
			IsValidQoderSessionID(stem)
	case 2:
		stem, ok := strings.CutSuffix(parts[1], ".jsonl")
		return ok &&
			!strings.HasPrefix(stem, "agent-") &&
			IsValidQoderSessionID(stem)
	case 4:
		stem, ok := strings.CutSuffix(parts[3], ".jsonl")
		return ok &&
			IsValidQoderSessionID(parts[1]) &&
			parts[2] == "subagents" &&
			strings.HasPrefix(stem, "agent-") &&
			IsValidQoderSessionID(stem)
	default:
		return false
	}
}

func qoderProjectHintFromPath(root, path string) string {
	parts, ok := qoderPathParts(root, path)
	if !ok {
		return ""
	}
	switch len(parts) {
	case 1:
		// SharedClientCache flat layout: synthesize a project hint
		// from the parent dir basename (e.g. "cli").
		return qoderFlatProjectHint(root)
	case 2, 4:
		return DecodeQoderProjectDir(parts[0])
	default:
		return ""
	}
}

func qoderSessionIDFromPath(root, path string) string {
	if !isQoderSourcePath(root, path) {
		return ""
	}
	parts, _ := qoderPathParts(root, path)
	stem := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if len(parts) == 4 {
		return parts[1] + ":subagent:" + stem
	}
	return stem
}

func isQoderLookupID(rawID string) bool {
	if rawID == "" {
		return false
	}
	sessionID, subagentID, hasSubagent := strings.Cut(rawID, ":subagent:")
	if !IsValidQoderSessionID(sessionID) {
		return false
	}
	return !hasSubagent ||
		strings.HasPrefix(subagentID, "agent-") &&
			IsValidQoderSessionID(subagentID)
}

func qoderPathParts(root, path string) ([]string, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return nil, false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, false
		}
	}
	return parts, true
}

func qoderCompanionFiles(path string) []string {
	stem, ok := strings.CutSuffix(path, ".jsonl")
	if ok {
		return []string{stem + "-session.json"}
	}
	return nil
}

func qoderCompanionTranscript(companionPath string) (string, bool) {
	stem, ok := strings.CutSuffix(companionPath, "-session.json")
	return stem + ".jsonl", ok
}

func qoderProviderCapabilities() Capabilities {
	source := jsonlFileProviderSourceCapabilities()
	source.MultiSessionSource = CapabilitySupported
	source.ExcludedSessions = CapabilitySupported
	source.ForceReplaceOnParse = CapabilitySupported
	return Capabilities{
		Source: source,
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			SessionName:          CapabilitySupported,
			Cwd:                  CapabilitySupported,
			Relationships:        CapabilitySupported,
			Subagents:            CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			PerMessageTokenUsage: CapabilitySupported,
			MalformedLineCount:   CapabilitySupported,
			Model:                CapabilitySupported,
		},
		Sync: ProviderSyncSemantics{
			FingerprintHashInCacheKey:           true,
			FingerprintHashRequiredForFreshness: true,
		},
	}
}
