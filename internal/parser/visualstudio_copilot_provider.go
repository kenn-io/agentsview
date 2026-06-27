package parser

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Visual Studio Copilot stores conversations inside shared trace files
// (*_VSGitHubCopilot_traces.jsonl). It is a multi-session container provider,
// but unlike the SQLite-backed containers it discovers one source per
// conversation (deduplicated across trace files, newest trace wins) plus a bare
// physical source for any trace whose conversation IDs could not be read, so
// the read failure surfaces instead of being silently dropped. Parse of a
// conversation virtual path yields that one session; Parse of a bare trace fans
// out every conversation in it. All behavior is wired into the shared
// multi-session-container base via options.
func newVisualStudioCopilotProviderFactory(def AgentDef) ProviderFactory {
	return NewMultiSessionProviderFactory(
		def,
		visualStudioCopilotProviderCapabilities(),
		func(cfg ProviderConfig) multiSessionContainerSourceSet {
			return NewMultiSessionContainerSourceSet(
				AgentVSCopilot,
				cfg.Roots,
				WithSourceDiscovery(vsCopilotDiscoverSources),
				WithWatchRoots(vsCopilotWatchRoots),
				WithChangedPathClassifier(vsCopilotClassifyPath),
				WithMemberLookup(vsCopilotFindMember),
				WithFingerprint(vsCopilotFingerprintSource),
				WithContainerParse(vsCopilotParseContainer),
				WithMemberParse(vsCopilotParseMember),
				// Every conversation in a trace shares the trace's content hash.
				WithContainerHashStamping(),
			)
		},
	)
}

// vsCopilotDiscoverSources emits one match per conversation (virtual path) plus
// a bare physical match for each unreadable trace, mirroring the legacy
// per-conversation discovery.
func vsCopilotDiscoverSources(root string) []multiSessionMatch {
	var out []multiSessionMatch
	for _, file := range discoverVisualStudioCopilotSessionFilesUnderRoot(root) {
		match, ok := vsCopilotDiscoveredMatch(root, file.Path)
		if !ok {
			continue
		}
		match.ProjectHint = file.Project
		out = append(out, match)
	}
	return out
}

// vsCopilotDiscoveredMatch classifies a discovery path. Discovery emits either a
// <traceFile>#<conversationID> virtual path for a readable trace, or a bare
// physical trace path for one whose conversation IDs could not be read. The
// unreadable physical file must still become a source so the engine surfaces
// the read failure instead of silently dropping it; the regular-file
// requirement is therefore relaxed for the bare physical trace (which os.ReadDir
// already enumerated) while virtual paths keep validating that their backing
// trace exists.
func vsCopilotDiscoveredMatch(root, path string) (multiSessionMatch, bool) {
	if match, ok := vsCopilotClassifyPath(root, path, false); ok {
		return match, true
	}
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if _, _, ok := splitVisualStudioCopilotVirtualPath(path); ok {
		return multiSessionMatch{}, false
	}
	if !visualStudioCopilotTraceUnderRoot(root, path, false) {
		return multiSessionMatch{}, false
	}
	return multiSessionMatch{
		Path:        path,
		Container:   path,
		ProjectHint: "visualstudio",
	}, true
}

func discoverVisualStudioCopilotSessionFilesUnderRoot(
	vsRoot string,
) []DiscoveredFile {
	if vsRoot == "" {
		return nil
	}
	entries, err := os.ReadDir(vsRoot)
	if err != nil {
		return nil
	}
	files := discoverVisualStudioCopilotSessionFiles(vsRoot, entries)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files
}

func vsCopilotWatchRoots(roots []string) []WatchRoot {
	out := make([]WatchRoot, 0, len(roots))
	for _, root := range roots {
		out = append(out, WatchRoot{
			Path:         root,
			Recursive:    false,
			IncludeGlobs: []string{"*_VSGitHubCopilot_traces.jsonl"},
			DebounceKey:  string(AgentVSCopilot) + ":traces:" + root,
		})
	}
	return out
}

// vsCopilotClassifyPath maps a stored or changed path to its trace container and
// conversation. A virtual path always requires its backing trace to exist; a
// bare trace path relaxes the regular-file check under allowMissing so a deleted
// trace still classifies for changed-path tombstones.
func vsCopilotClassifyPath(
	root, path string, allowMissing bool,
) (multiSessionMatch, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if tracePath, conversationID, ok :=
		splitVisualStudioCopilotVirtualPath(path); ok {
		if !visualStudioCopilotTraceUnderRoot(root, tracePath, true) {
			return multiSessionMatch{}, false
		}
		return multiSessionMatch{
			Path:        path,
			Container:   tracePath,
			MemberID:    conversationID,
			ProjectHint: "visualstudio",
		}, true
	}
	if visualStudioCopilotTraceUnderRoot(root, path, !allowMissing) {
		return multiSessionMatch{
			Path:        path,
			Container:   path,
			ProjectHint: "visualstudio",
		}, true
	}
	return multiSessionMatch{}, false
}

func vsCopilotFindMember(root, rawID string) (multiSessionMatch, bool) {
	path := findVisualStudioCopilotSourceFile(root, rawID)
	if path == "" {
		return multiSessionMatch{}, false
	}
	return vsCopilotClassifyPath(root, path, false)
}

// findVisualStudioCopilotSourceFile locates a trace file by conversation UUID
// and returns a conversation-scoped <traceFile>#<conversationID> virtual path.
func findVisualStudioCopilotSourceFile(root, rawID string) string {
	if root == "" || !IsValidSessionID(rawID) {
		return ""
	}
	return findVisualStudioCopilotTraceSourceFile(root, rawID)
}

func vsCopilotFingerprintSource(
	src multiSessionSource,
) (SourceFingerprint, error) {
	size, mtime, err := VisualStudioCopilotTraceFingerprintStrict(src.Container)
	if err != nil {
		return SourceFingerprint{}, err
	}
	hash, err := hashJSONLSourceFile(src.Container)
	if err != nil {
		return SourceFingerprint{}, err
	}
	return SourceFingerprint{
		Size:    size,
		MTimeNS: mtime,
		Hash:    hash,
	}, nil
}

func vsCopilotParseMember(
	src multiSessionSource, req ParseRequest,
) (*ParseResult, error) {
	project := firstNonEmptyJSONLString(req.Source.ProjectHint, "visualstudio")
	sess, msgs, err := parseVisualStudioCopilotConversation(
		src.Container, src.MemberID, project, req.Machine,
	)
	if err != nil {
		return nil, err
	}
	if sess == nil {
		return nil, nil
	}
	return &ParseResult{Session: *sess, Messages: msgs}, nil
}

func vsCopilotParseContainer(
	src multiSessionSource, req ParseRequest,
) ([]ParseResult, error) {
	ids, err := VisualStudioCopilotFileConversationIDs(src.Container)
	if err != nil {
		return nil, err
	}
	project := firstNonEmptyJSONLString(req.Source.ProjectHint, "visualstudio")
	results := make([]ParseResult, 0, len(ids))
	for _, id := range ids {
		sess, msgs, err := parseVisualStudioCopilotConversation(
			src.Container, id, project, req.Machine,
		)
		if err != nil {
			return nil, err
		}
		if sess == nil {
			continue
		}
		results = append(results, ParseResult{Session: *sess, Messages: msgs})
	}
	return results, nil
}

// splitVisualStudioCopilotVirtualPath splits a <traceFile>#<conversationID>
// virtual source path into its physical trace file and conversation ID. It
// builds on the provider-neutral ParseVirtualSourcePath splitter and adds the
// Visual Studio Copilot validation: the container must name a trace file and the
// source ID must be a valid conversation ID. It returns ok=false for a plain
// trace-file path.
func splitVisualStudioCopilotVirtualPath(
	sourcePath string,
) (tracePath, conversationID string, ok bool) {
	tracePath, conversationID, ok = ParseVirtualSourcePath(sourcePath)
	if !ok {
		return "", "", false
	}
	if !IsVisualStudioCopilotTraceFile(tracePath) ||
		!IsValidSessionID(conversationID) {
		return "", "", false
	}
	return tracePath, conversationID, true
}

func visualStudioCopilotTraceUnderRoot(
	root, path string,
	requireRegular bool,
) bool {
	rel, ok := relUnder(root, path)
	if !ok || strings.Contains(filepath.ToSlash(rel), "/") {
		return false
	}
	if !IsVisualStudioCopilotTraceFile(path) {
		return false
	}
	return !requireRegular || IsRegularFile(path)
}

func visualStudioCopilotProviderCapabilities() Capabilities {
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
			ExcludedSessions:     CapabilityNotApplicable,
			ForceReplaceOnParse:  CapabilitySupported,
		},
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			AggregateUsageEvents: CapabilitySupported,
			Model:                CapabilitySupported,
		},
	}
}
