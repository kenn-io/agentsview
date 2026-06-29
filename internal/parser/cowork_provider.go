package parser

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Cowork stores each session as a Claude-format transcript
// (.claude/projects/**/<id>.jsonl) with a sibling local_<uuid>.json metadata
// file, plus per-subagent transcripts. It is a single-file provider whose parse
// can yield multiple sessions (the main conversation and its subagents) and
// drive removals via excluded session IDs. All behavior is wired into the
// shared single-file base via options.
func newCoworkProviderFactory(def AgentDef) ProviderFactory {
	return NewSingleFileProviderFactory(
		def,
		coworkProviderCapabilities(),
		func(cfg ProviderConfig) singleFileSourceSet {
			return NewSingleFileSourceSet(
				AgentCowork,
				cfg.Roots,
				WithFileDiscovery(coworkDiscoverFiles),
				WithFileWatchRoots(coworkWatchRoots),
				WithFileChangedPathClassifier(coworkClassifyPath),
				WithFileLookup(coworkFindFile),
				WithFileFingerprint(coworkFingerprintSource),
				WithFileParse(coworkParseFile),
				// Parse removes stale subagents via exclusions, so an empty
				// result set is still a complete (not skipped) parse.
				WithAlwaysCompleteResultSet(),
			)
		},
	)
}

func coworkDiscoverFiles(root string) []singleFileMatch {
	var out []singleFileMatch
	walkCoworkSessions(root, func(transcript string) {
		if match, ok := coworkTranscriptMatch(root, transcript); ok {
			out = append(out, match)
		}
	})
	return out
}

func coworkWatchRoots(roots []string) []WatchRoot {
	out := make([]WatchRoot, 0, len(roots))
	for _, root := range roots {
		out = append(out, WatchRoot{
			Path:         root,
			Recursive:    true,
			IncludeGlobs: []string{"local_*.json", "*.jsonl"},
			DebounceKey:  string(AgentCowork) + ":metadata:" + root,
		})
	}
	return out
}

// coworkClassifyPath maps a stored or changed path to its session transcript. A
// transcript path classifies directly; a metadata path resolves to the
// session's main transcript so a title rename is picked up. Under allowMissing a
// metadata path whose transcript was deleted still resolves via on-disk
// scanning.
func coworkClassifyPath(
	root, path string, allowMissing bool,
) (singleFileMatch, bool) {
	transcript, ok := classifyCoworkPath(root, path)
	if !ok && allowMissing {
		transcript, ok = coworkTranscriptForMetadataPath(root, path)
	}
	if !ok {
		return singleFileMatch{}, false
	}
	return coworkTranscriptMatch(root, transcript)
}

func coworkFindFile(root, rawID string) (singleFileMatch, bool) {
	path := coworkFindSourceFile(root, rawID)
	if path == "" {
		return singleFileMatch{}, false
	}
	return coworkTranscriptMatch(root, path)
}

// coworkTranscriptMatch validates a transcript path under root and builds a
// match carrying the project hint read from the session's metadata. It
// reproduces the legacy sourceRef checks.
func coworkTranscriptMatch(root, path string) (singleFileMatch, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if _, ok := relUnder(root, path); !ok {
		return singleFileMatch{}, false
	}
	metaPath := coworkMetaPathForTranscript(path)
	if metaPath == "" {
		return singleFileMatch{}, false
	}
	if !isCoworkTranscriptPath(root, path) {
		return singleFileMatch{}, false
	}
	return singleFileMatch{
		Path:        path,
		ProjectHint: coworkProjectName(readCoworkMeta(metaPath)),
	}, true
}

func coworkFingerprintSource(
	src singleFileSource,
) (SourceFingerprint, error) {
	info, err := os.Stat(src.Path)
	if err != nil {
		return SourceFingerprint{}, fmt.Errorf("stat %s: %w", src.Path, err)
	}
	if info.IsDir() {
		return SourceFingerprint{}, fmt.Errorf(
			"stat %s: source is a directory", src.Path,
		)
	}
	hash, err := hashJSONLSourceFile(src.Path)
	if err != nil {
		return SourceFingerprint{}, err
	}
	return SourceFingerprint{
		Size:    info.Size(),
		MTimeNS: CoworkSessionMtime(src.Path, info.ModTime().UnixNano()),
		Hash:    hash,
	}, nil
}

func coworkParseFile(
	src singleFileSource, req ParseRequest,
) ([]ParseResult, []string, error) {
	results, excluded, err := parseCoworkSession(src.Path, req.Machine)
	if err != nil {
		return nil, nil, err
	}
	if req.Fingerprint.Hash != "" {
		for i := range results {
			results[i].Session.File.Hash = req.Fingerprint.Hash
		}
	}
	return results, excluded, nil
}

func isCoworkTranscriptPath(root, path string) bool {
	rel, ok := relUnder(root, path)
	if !ok || filepath.Ext(path) != ".jsonl" {
		return false
	}
	sep := string(filepath.Separator)
	parts := strings.Split(rel, sep)
	n := len(parts)
	base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if n >= 5 && parts[n-4] == ".claude" && parts[n-3] == "projects" {
		return IsValidSessionID(base)
	}
	if !strings.Contains(sep+rel, sep+".claude"+sep+"projects"+sep) ||
		!slices.Contains(parts, "subagents") {
		return false
	}
	return strings.HasPrefix(base, "agent-")
}

func coworkTranscriptForMetadataPath(root, path string) (string, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rel, ok := relUnder(root, path)
	if !ok || !isCoworkMetaFileName(filepath.Base(rel)) {
		return "", false
	}
	sessionDir := strings.TrimSuffix(path, ".json")
	resolvedSessionDir, err := filepath.EvalSymlinks(sessionDir)
	if err != nil {
		return "", false
	}
	projectsDir := filepath.Join(sessionDir, ".claude", "projects")
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return "", false
	}
	var found string
	for _, entry := range entries {
		if !isDirOrSymlink(entry, projectsDir) {
			continue
		}
		projectDir := filepath.Join(projectsDir, entry.Name())
		files, err := os.ReadDir(projectDir)
		if err != nil {
			continue
		}
		for _, file := range files {
			if file.IsDir() {
				continue
			}
			name := file.Name()
			if !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			stem := strings.TrimSuffix(name, ".jsonl")
			if !IsValidSessionID(stem) || strings.HasPrefix(stem, "agent-") {
				continue
			}
			candidate := filepath.Join(projectDir, name)
			if !validCoworkMainTranscriptCandidate(resolvedSessionDir, candidate) {
				continue
			}
			if found != "" {
				return "", false
			}
			found = candidate
		}
	}
	return found, found != ""
}

func validCoworkMainTranscriptCandidate(resolvedSessionDir, candidate string) bool {
	if !IsRegularFile(candidate) {
		return false
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return false
	}
	return isContainedIn(resolved, resolvedSessionDir)
}

// coworkFindSourceFile locates a cowork transcript by its raw session ID
// (the cliSessionId or "agent-<id>" subagent id, with the "cowork:" prefix
// already stripped).
func coworkFindSourceFile(root, sessionID string) string {
	if !IsValidSessionID(sessionID) {
		return ""
	}
	target := sessionID + ".jsonl"
	var found string
	walkCoworkSessions(root, func(transcript string) {
		if found == "" && filepath.Base(transcript) == target {
			found = transcript
		}
	})
	return found
}

// classifyCoworkPath reports whether a changed path under a cowork root is a
// cowork session transcript (main or subagent) or its sibling metadata file,
// and returns the transcript file that should be (re)parsed. Metadata changes
// (e.g. a title rename) resolve to the session's main transcript so the rename
// is picked up.
func classifyCoworkPath(root, path string) (string, bool) {
	rel, ok := relUnder(root, path)
	if !ok {
		return "", false
	}
	sep := string(filepath.Separator)
	parts := strings.Split(rel, sep)
	n := len(parts)
	base := parts[n-1]

	if strings.HasSuffix(base, ".jsonl") {
		// Must live under a .claude/projects/ subtree.
		marker := sep + ".claude" + sep + "projects" + sep
		if !strings.Contains(sep+rel, marker) {
			return "", false
		}
		stem := strings.TrimSuffix(base, ".jsonl")
		if strings.HasPrefix(stem, "agent-") {
			// Subagent transcript: <enc>/<cli>/subagents/**/agent-*.jsonl.
			if slices.Contains(parts, "subagents") {
				return path, true
			}
			return "", false
		}
		// Main transcript: <enc>/<cliSessionId>.jsonl directly under projects.
		if n >= 5 && parts[n-4] == ".claude" && parts[n-3] == "projects" &&
			IsValidSessionID(stem) {
			return path, true
		}
		return "", false
	}

	// Metadata: <orgId>/<workspaceId>/local_<uuid>.json
	if isCoworkMetaFileName(base) {
		meta := readCoworkMeta(path)
		if meta.CliSessionID == "" {
			return "", false
		}
		sessionDir := strings.TrimSuffix(path, ".json")
		if main, _ := resolveCoworkSession(
			sessionDir, meta.CliSessionID,
		); main != "" {
			return main, true
		}
	}
	return "", false
}

func coworkProviderCapabilities() Capabilities {
	return Capabilities{
		Source: SourceCapabilities{
			DiscoverSources:      CapabilitySupported,
			WatchSources:         CapabilitySupported,
			ClassifyChangedPath:  CapabilitySupported,
			FindSource:           CapabilitySupported,
			CompositeFingerprint: CapabilitySupported,
			IncrementalAppend:    CapabilityNotApplicable,
			MultiSessionSource:   CapabilitySupported,
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
			Subagents:            CapabilitySupported,
			Thinking:             CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			PerMessageTokenUsage: CapabilitySupported,
			TerminationStatus:    CapabilitySupported,
			MalformedLineCount:   CapabilitySupported,
			Model:                CapabilitySupported,
			StopReason:           CapabilitySupported,
		},
	}
}
