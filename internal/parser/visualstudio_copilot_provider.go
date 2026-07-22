package parser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// Visual Studio Copilot normally stores conversations inside shared trace
// files (*_VSGitHubCopilot_traces.jsonl), and newer Visual Studio versions can
// also write extensionless conversation files under .vs/*/copilot-chat/*/sessions.
// It is a multi-session container provider, but unlike the SQLite-backed
// containers it discovers one source per conversation (deduplicated across trace
// files, newest trace wins) plus a bare physical source for any legacy trace
// whose conversation IDs could not be read, so read failures are surfaced instead
// of being silently dropped. Parse of a conversation virtual path yields that one
// session; Parse of a bare trace fans out every conversation in it. All behavior
// is wired into the shared multi-session-container base via options.
func newVisualStudioCopilotProviderFactory(def AgentDef) ProviderFactory {
	return NewMultiSessionProviderFactory(
		def,
		visualStudioCopilotProviderCapabilities(),
		func(cfg ProviderConfig) multiSessionContainerSourceSet {
			return NewMultiSessionContainerSourceSet(
				AgentVSCopilot,
				cfg.Roots,
				WithSourceDiscovery(vsCopilotDiscoverSources),
				WithStreamingSourceDiscovery(vsCopilotDiscoverEach),
				WithWatchRoots(vsCopilotWatchRoots),
				WithChangedPathClassifier(vsCopilotClassifyPath),
				WithMemberLookup(vsCopilotFindMember),
				WithContextFingerprint(vsCopilotFingerprintSourceContext),
				WithContainerParse(vsCopilotParseContainer),
				WithContextMemberParse(vsCopilotParseMemberContext),
				// Every conversation in a trace shares the trace's content hash.
				WithContainerHashStamping(),
			)
		},
	)
}

type vsCopilotDiskCandidate struct {
	Path    string `json:"path"`
	MTimeNS int64  `json:"mtime_ns"`
}

// vsCopilotRootComposite memoizes the shared sibling trace fingerprint for
// one discovery pass. Every trace file in the pass lives in the same
// directory, so the composite (summed size, max mtime) is identical for all
// of them; computing it once keeps discovery linear instead of re-listing
// and re-statting the directory per trace file. The zero value is ready to
// use and covers exactly one pass.
type vsCopilotRootComposite struct {
	done  bool
	size  int64
	mtime int64
	err   error
}

func (c *vsCopilotRootComposite) fingerprint(
	tracePath string,
) (size, mtime int64, err error) {
	if !c.done {
		c.done = true
		c.size, c.mtime, c.err =
			VisualStudioCopilotTraceFingerprintStrict(tracePath)
	}
	return c.size, c.mtime, c.err
}

// vsCopilotDiscoverEach externalizes the conversation-to-canonical-file index
// to a temporary SQLite table. Trace and VS 2026 trees are read in fixed-size
// directory batches; only one decoded trace line is resident at a time.
func vsCopilotDiscoverEach(
	ctx context.Context, root string, yield func(multiSessionMatch) error,
) (retErr error) {
	index, err := newDiscoveryDiskMapForContext(ctx)
	if err != nil {
		return err
	}
	defer func() {
		retErr = errors.Join(retErr, index.close())
	}()
	remember := func(id, path string, mtimeNS int64) error {
		current, found, err := index.get(ctx, id)
		if err != nil {
			return err
		}
		if found {
			var candidate vsCopilotDiskCandidate
			if json.Unmarshal([]byte(current), &candidate) == nil &&
				(candidate.MTimeNS > mtimeNS ||
					(candidate.MTimeNS == mtimeNS && candidate.Path >= path)) {
				return nil
			}
		}
		encoded, err := json.Marshal(vsCopilotDiskCandidate{Path: path, MTimeNS: mtimeNS})
		if err != nil {
			return err
		}
		return index.put(ctx, id, string(encoded), true)
	}
	root = filepath.Clean(root)
	var composite vsCopilotRootComposite
	err = streamDirectoryEntries(ctx, root, func(entry os.DirEntry) error {
		if entry.IsDir() || !isVisualStudioCopilotTraceFileName(entry.Name()) {
			return nil
		}
		path := filepath.Join(root, entry.Name())
		info, err := entry.Info()
		if err != nil {
			return err
		}
		err = forEachVisualStudioCopilotTraceSpan(ctx, path, func(span vsCopilotSpan) error {
			id := canonicalVisualStudioCopilotConversationID(
				vsCopilotTraceAttrs(span.Attributes)["gen_ai.conversation.id"],
			)
			if id == "" {
				return nil
			}
			encoded, err := json.Marshal(span)
			if err != nil {
				return err
			}
			if err := reconciliationCacheAppend(
				ctx, vsCopilotCachedSpansKey(root, id), string(encoded),
			); err != nil {
				return err
			}
			return remember(id, path, info.ModTime().UnixNano())
		})
		if err != nil {
			return err
		}
		encoded, found, err := reconciliationCacheGet(
			ctx, vsCopilotCachedFingerprintKey(path),
		)
		if err != nil || !found {
			return err
		}
		var fingerprint SourceFingerprint
		if err := json.Unmarshal([]byte(encoded), &fingerprint); err != nil {
			return err
		}
		fingerprint.Size, fingerprint.MTimeNS, err = composite.fingerprint(path)
		if err != nil {
			return err
		}
		encodedBytes, err := json.Marshal(fingerprint)
		if err != nil {
			return err
		}
		return reconciliationCachePut(
			ctx, vsCopilotCachedFingerprintKey(path), string(encodedBytes),
		)
	})
	if err != nil {
		return err
	}
	if err := streamVisualStudioCopilotVS2026Sessions(ctx, root, remember); err != nil {
		return err
	}
	return index.forEach(ctx, func(id, value string) error {
		var candidate vsCopilotDiskCandidate
		if err := json.Unmarshal([]byte(value), &candidate); err != nil {
			return err
		}
		return yield(multiSessionMatch{
			Path:      VisualStudioCopilotVirtualPath(candidate.Path, id),
			Container: candidate.Path, MemberID: id,
			ProjectHint: "visualstudio", DiscoveryMTimeNS: candidate.MTimeNS,
		})
	})
}

// streamVisualStudioCopilotVS2026Sessions follows only the fixed VS 2026
// layout. Expected directory symlinks are safe because traversal has a bounded
// logical depth, and every directory is read through fixed-size batches.
func streamVisualStudioCopilotVS2026Sessions(
	ctx context.Context,
	root string,
	remember func(id, path string, mtimeNS int64) error,
) error {
	switch visualStudioCopilotVS2026RootKind(root) {
	case visualStudioCopilotVS2026SessionsRoot:
		return streamVisualStudioCopilotVS2026SessionDirectory(ctx, root, remember)
	case visualStudioCopilotVS2026ThreadRoot:
		return streamVisualStudioCopilotVS2026ThreadRoot(ctx, root, remember)
	case visualStudioCopilotVS2026CopilotChatRoot:
		return streamVisualStudioCopilotVS2026CopilotChatRoot(ctx, root, remember)
	case visualStudioCopilotVS2026VSRoot:
		return streamVisualStudioCopilotVS2026VSRoot(ctx, root, remember)
	default:
		vsRoot, ok, err := streamVisualStudioCopilotChildDir(ctx, root, ".vs")
		if err != nil || !ok {
			return err
		}
		return streamVisualStudioCopilotVS2026VSRoot(ctx, vsRoot, remember)
	}
}

func streamVisualStudioCopilotVS2026VSRoot(
	ctx context.Context,
	vsRoot string,
	remember func(id, path string, mtimeNS int64) error,
) error {
	return streamVisualStudioCopilotDirectoryCandidates(ctx, vsRoot, func(solutionRoot string) error {
		copilotChatRoot, ok, err := streamVisualStudioCopilotChildDir(
			ctx, solutionRoot, "copilot-chat",
		)
		if err != nil || !ok {
			return err
		}
		return streamVisualStudioCopilotVS2026CopilotChatRoot(
			ctx, copilotChatRoot, remember,
		)
	})
}

func streamVisualStudioCopilotVS2026CopilotChatRoot(
	ctx context.Context,
	copilotChatRoot string,
	remember func(id, path string, mtimeNS int64) error,
) error {
	return streamVisualStudioCopilotDirectoryCandidates(ctx, copilotChatRoot, func(threadRoot string) error {
		return streamVisualStudioCopilotVS2026ThreadRoot(ctx, threadRoot, remember)
	})
}

func streamVisualStudioCopilotVS2026ThreadRoot(
	ctx context.Context,
	threadRoot string,
	remember func(id, path string, mtimeNS int64) error,
) error {
	sessionsRoot, ok, err := streamVisualStudioCopilotChildDir(ctx, threadRoot, "sessions")
	if err != nil || !ok {
		return err
	}
	return streamVisualStudioCopilotVS2026SessionDirectory(ctx, sessionsRoot, remember)
}

func streamVisualStudioCopilotVS2026SessionDirectory(
	ctx context.Context,
	sessionsRoot string,
	remember func(id, path string, mtimeNS int64) error,
) error {
	return streamDirectoryEntries(ctx, sessionsRoot, func(entry os.DirEntry) error {
		if entry.IsDir() || !isVisualStudioCopilotVS2026SessionFileName(entry.Name()) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		path := filepath.Join(sessionsRoot, entry.Name())
		return remember(
			canonicalVisualStudioCopilotConversationID(entry.Name()),
			path,
			info.ModTime().UnixNano(),
		)
	})
}

func streamVisualStudioCopilotDirectoryCandidates(
	ctx context.Context,
	parent string,
	visit func(string) error,
) error {
	return streamDirectoryEntries(ctx, parent, func(entry os.DirEntry) error {
		candidate, err := streamingDirOrSymlinkCandidate(entry, parent)
		if err != nil {
			return fmt.Errorf(
				"stat Visual Studio Copilot directory candidate %s: %w",
				filepath.Join(parent, entry.Name()), err,
			)
		}
		if !candidate {
			return nil
		}
		return visit(filepath.Join(parent, entry.Name()))
	})
}

func streamVisualStudioCopilotChildDir(
	ctx context.Context,
	parent string,
	name string,
) (string, bool, error) {
	var fallback string
	err := streamDirectoryEntries(ctx, parent, func(entry os.DirEntry) error {
		if !strings.EqualFold(entry.Name(), name) {
			return nil
		}
		candidate, err := streamingDirOrSymlinkCandidate(entry, parent)
		if err != nil {
			return fmt.Errorf(
				"stat Visual Studio Copilot directory %s: %w",
				filepath.Join(parent, entry.Name()), err,
			)
		}
		if !candidate {
			return nil
		}
		path := filepath.Join(parent, entry.Name())
		if entry.Name() == name {
			fallback = path
			return errStopStreamingDiscovery
		}
		if fallback == "" || path < fallback {
			fallback = path
		}
		return nil
	})
	if errors.Is(err, errStopStreamingDiscovery) {
		return fallback, true, nil
	}
	if err != nil {
		return "", false, err
	}
	return fallback, fallback != "", nil
}

func vsCopilotCachedSpansKey(root, conversationID string) string {
	return "visualstudio-copilot:spans:" + filepath.Clean(root) + "\x00" +
		canonicalVisualStudioCopilotConversationID(conversationID)
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
	if visualStudioCopilotVS2026SessionUnderRoot(root, path) {
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
	vsRoot = filepath.Clean(vsRoot)
	if vsRoot == "" {
		return nil
	}
	bestByConversation := map[string]visualStudioCopilotCandidate{}
	filesByConversation := map[string]DiscoveredFile{}
	filesByPath := map[string]DiscoveredFile{}

	entries, err := os.ReadDir(vsRoot)
	if err == nil {
		for _, file := range discoverVisualStudioCopilotSessionFiles(vsRoot, entries) {
			if !vsCopilotRememberVirtualDiscoveredFile(
				file,
				bestByConversation,
				filesByConversation,
			) {
				filesByPath[file.Path] = file
			}
		}
	}

	for _, file := range discoverVisualStudioCopilotVS2026SessionFiles(vsRoot) {
		vsCopilotRememberVirtualDiscoveredFile(
			file,
			bestByConversation,
			filesByConversation,
		)
	}

	files := make([]DiscoveredFile, 0, len(filesByConversation)+len(filesByPath))
	for _, file := range filesByConversation {
		files = append(files, file)
	}
	for _, file := range filesByPath {
		files = append(files, file)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files
}

func vsCopilotRememberVirtualDiscoveredFile(
	file DiscoveredFile,
	bestByConversation map[string]visualStudioCopilotCandidate,
	filesByConversation map[string]DiscoveredFile,
) bool {
	path, conversationID, ok := splitVisualStudioCopilotVirtualPath(file.Path)
	if !ok {
		return false
	}
	mtime := time.Time{}
	if info, err := os.Stat(path); err == nil {
		mtime = info.ModTime()
	}
	current := bestByConversation[conversationID]
	if !visualStudioCopilotCandidateWins(path, mtime, current) {
		return true
	}
	bestByConversation[conversationID] = visualStudioCopilotCandidate{
		path:  path,
		mtime: mtime,
	}
	filesByConversation[conversationID] = file
	return true
}

func discoverVisualStudioCopilotVS2026SessionFiles(
	root string,
) []DiscoveredFile {
	root = filepath.Clean(root)
	switch visualStudioCopilotVS2026RootKind(root) {
	case visualStudioCopilotVS2026SessionsRoot:
		return discoverVisualStudioCopilotVS2026SessionFilesInDirectory(root)
	case visualStudioCopilotVS2026ThreadRoot:
		sessionsRoot, ok := visualStudioCopilotChildDir(root, "sessions")
		if !ok {
			return nil
		}
		return discoverVisualStudioCopilotVS2026SessionFilesInDirectory(sessionsRoot)
	case visualStudioCopilotVS2026CopilotChatRoot:
		return discoverVisualStudioCopilotVS2026SessionFilesInCopilotChatRoot(root)
	case visualStudioCopilotVS2026VSRoot:
		return discoverVisualStudioCopilotVS2026SessionFilesInVSRoot(root)
	default:
		vsRoot, ok := visualStudioCopilotChildDir(root, ".vs")
		if !ok {
			return nil
		}
		return discoverVisualStudioCopilotVS2026SessionFilesInVSRoot(vsRoot)
	}
}

func discoverVisualStudioCopilotVS2026SessionFilesInVSRoot(
	vsRoot string,
) []DiscoveredFile {
	solutions, err := os.ReadDir(vsRoot)
	if err != nil {
		return nil
	}
	out := []DiscoveredFile{}
	for _, solution := range solutions {
		if !isDirOrSymlink(solution, vsRoot) {
			continue
		}
		copilotChatRoot, ok := visualStudioCopilotChildDir(
			filepath.Join(vsRoot, solution.Name()),
			"copilot-chat",
		)
		if !ok {
			continue
		}
		out = append(
			out,
			discoverVisualStudioCopilotVS2026SessionFilesInCopilotChatRoot(
				copilotChatRoot,
			)...,
		)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func discoverVisualStudioCopilotVS2026SessionFilesInCopilotChatRoot(
	copilotChatRoot string,
) []DiscoveredFile {
	threads, err := os.ReadDir(copilotChatRoot)
	if err != nil {
		return nil
	}
	out := []DiscoveredFile{}
	for _, thread := range threads {
		if !isDirOrSymlink(thread, copilotChatRoot) {
			continue
		}
		sessionsRoot, ok := visualStudioCopilotChildDir(
			filepath.Join(copilotChatRoot, thread.Name()),
			"sessions",
		)
		if !ok {
			continue
		}
		out = append(
			out,
			discoverVisualStudioCopilotVS2026SessionFilesInDirectory(
				sessionsRoot,
			)...,
		)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func discoverVisualStudioCopilotVS2026SessionFilesInDirectory(
	dir string,
) []DiscoveredFile {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]DiscoveredFile, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !isVisualStudioCopilotVS2026SessionFileName(name) {
			continue
		}
		path := filepath.Join(dir, name)
		if !isVisualStudioCopilotVS2026SessionPath(path) {
			continue
		}
		out = append(out, DiscoveredFile{
			Path:    VisualStudioCopilotVirtualPath(path, name),
			Project: "visualstudio",
			Agent:   AgentVSCopilot,
		})
	}
	return out
}

func visualStudioCopilotChildDir(parent, name string) (string, bool) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return "", false
	}
	for _, entry := range entries {
		if isDirOrSymlink(entry, parent) && entry.Name() == name {
			return filepath.Join(parent, entry.Name()), true
		}
	}
	for _, entry := range entries {
		if isDirOrSymlink(entry, parent) && strings.EqualFold(entry.Name(), name) {
			return filepath.Join(parent, entry.Name()), true
		}
	}
	return "", false
}

func vsCopilotWatchRoots(roots []string) []WatchRoot {
	out := make([]WatchRoot, 0, len(roots)*2)
	for _, root := range roots {
		root = filepath.Clean(root)
		if root == "" || root == "." {
			continue
		}
		includeSessionRoots := true
		switch visualStudioCopilotVS2026RootKind(root) {
		case visualStudioCopilotVS2026SessionsRoot:
			out = append(out, vsCopilotVS2026SessionWatchRoot(root))
			includeSessionRoots = false
		case visualStudioCopilotVS2026VSRoot,
			visualStudioCopilotVS2026CopilotChatRoot,
			visualStudioCopilotVS2026ThreadRoot:
			out = append(out, WatchRoot{
				Path:         root,
				Recursive:    true,
				IncludeGlobs: []string{"*"},
				DebounceKey:  string(AgentVSCopilot) + ":sessions:" + root,
			})
		default:
			out = append(out, WatchRoot{
				Path:         root,
				Recursive:    false,
				IncludeGlobs: []string{"*_VSGitHubCopilot_traces.jsonl"},
				DebounceKey:  string(AgentVSCopilot) + ":traces:" + root,
			})
			if vsRoot, ok := visualStudioCopilotChildDir(root, ".vs"); ok {
				out = append(out, WatchRoot{
					Path:         vsRoot,
					Recursive:    true,
					IncludeGlobs: []string{"*"},
					DebounceKey:  string(AgentVSCopilot) + ":sessions:" + vsRoot,
				})
			}
		}
		if includeSessionRoots {
			for _, sessionsRoot := range visualStudioCopilotVS2026SessionRoots(root) {
				out = append(out, vsCopilotVS2026SessionWatchRoot(sessionsRoot))
			}
		}
	}
	return out
}

func vsCopilotVS2026SessionWatchRoot(root string) WatchRoot {
	return WatchRoot{
		Path:         root,
		Recursive:    false,
		IncludeGlobs: []string{"*"},
		DebounceKey:  string(AgentVSCopilot) + ":sessions:" + root,
	}
}

func visualStudioCopilotVS2026SessionRoots(root string) []string {
	root = filepath.Clean(root)
	var roots []string
	switch visualStudioCopilotVS2026RootKind(root) {
	case visualStudioCopilotVS2026SessionsRoot:
		roots = append(roots, root)
	case visualStudioCopilotVS2026ThreadRoot:
		if sessionsRoot, ok := visualStudioCopilotChildDir(root, "sessions"); ok {
			roots = append(roots, sessionsRoot)
		}
	case visualStudioCopilotVS2026CopilotChatRoot:
		roots = visualStudioCopilotVS2026SessionRootsInCopilotChatRoot(root)
	case visualStudioCopilotVS2026VSRoot:
		roots = visualStudioCopilotVS2026SessionRootsInVSRoot(root)
	default:
		if vsRoot, ok := visualStudioCopilotChildDir(root, ".vs"); ok {
			roots = visualStudioCopilotVS2026SessionRootsInVSRoot(vsRoot)
		}
	}
	sort.Strings(roots)
	return slices.Compact(roots)
}

func visualStudioCopilotVS2026SessionRootsInVSRoot(
	vsRoot string,
) []string {
	solutions, err := os.ReadDir(vsRoot)
	if err != nil {
		return nil
	}
	out := []string{}
	for _, solution := range solutions {
		if !isDirOrSymlink(solution, vsRoot) {
			continue
		}
		copilotChatRoot, ok := visualStudioCopilotChildDir(
			filepath.Join(vsRoot, solution.Name()),
			"copilot-chat",
		)
		if !ok {
			continue
		}
		out = append(
			out,
			visualStudioCopilotVS2026SessionRootsInCopilotChatRoot(
				copilotChatRoot,
			)...,
		)
	}
	sort.Strings(out)
	return slices.Compact(out)
}

func visualStudioCopilotVS2026SessionRootsInCopilotChatRoot(
	copilotChatRoot string,
) []string {
	threads, err := os.ReadDir(copilotChatRoot)
	if err != nil {
		return nil
	}
	out := []string{}
	for _, thread := range threads {
		if !isDirOrSymlink(thread, copilotChatRoot) {
			continue
		}
		sessionsRoot, ok := visualStudioCopilotChildDir(
			filepath.Join(copilotChatRoot, thread.Name()),
			"sessions",
		)
		if !ok {
			continue
		}
		out = append(out, sessionsRoot)
	}
	sort.Strings(out)
	return slices.Compact(out)
}

// vsCopilotClassifyPath maps a stored or changed path to its trace container and
// conversation. A virtual path normally requires its backing container to exist;
// VS 2026 one-file member tombstones relax that under allowMissing. A bare trace
// path also relaxes the regular-file check under allowMissing so a deleted trace
// still classifies for changed-path tombstones.
func vsCopilotClassifyPath(
	root, path string, allowMissing bool,
) (multiSessionMatch, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if tracePath, conversationID, ok :=
		splitVisualStudioCopilotVirtualPath(path); ok {
		if isVisualStudioCopilotVS2026SessionPath(tracePath) {
			conversationID = canonicalVisualStudioCopilotConversationID(
				conversationID,
			)
			if !visualStudioCopilotVS2026SessionUnderRoot(root, tracePath) ||
				(!allowMissing && !IsRegularFile(tracePath)) {
				return multiSessionMatch{}, false
			}
			return multiSessionMatch{
				Path:        VisualStudioCopilotVirtualPath(tracePath, conversationID),
				Container:   tracePath,
				MemberID:    conversationID,
				ProjectHint: "visualstudio",
			}, true
		}
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
	if isVisualStudioCopilotVS2026SessionPath(path) &&
		visualStudioCopilotVS2026SessionUnderRoot(root, path) {
		conversationID := canonicalVisualStudioCopilotConversationID(
			filepath.Base(path),
		)
		if allowMissing && !IsRegularFile(path) {
			return multiSessionMatch{
				Path:        path,
				Container:   path,
				ProjectHint: "visualstudio",
			}, true
		}
		return multiSessionMatch{
			Path:        VisualStudioCopilotVirtualPath(path, conversationID),
			Container:   path,
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

// findVisualStudioCopilotSourceFile locates the canonical source for one
// conversation ID, deduplicating across legacy traces and VS 2026 session
// files with the same newest-mtime, then path tie-breaker used by discovery.
func findVisualStudioCopilotSourceFile(root, rawID string) string {
	rawID = canonicalVisualStudioCopilotConversationID(rawID)
	if root == "" || !IsValidSessionID(rawID) {
		return ""
	}
	for _, file := range discoverVisualStudioCopilotSessionFilesUnderRoot(root) {
		_, conversationID, ok := splitVisualStudioCopilotVirtualPath(file.Path)
		if ok && sameVisualStudioCopilotConversationID(conversationID, rawID) {
			return file.Path
		}
	}
	return ""
}

func visualStudioCopilotVS2026SessionUnderRoot(
	root, path string,
) bool {
	rel, ok := relUnder(root, path)
	if !ok {
		return false
	}
	if !isVisualStudioCopilotVS2026SessionPath(path) {
		return false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	switch visualStudioCopilotVS2026RootKind(root) {
	case visualStudioCopilotVS2026SessionsRoot:
		return len(parts) == 1
	case visualStudioCopilotVS2026ThreadRoot:
		return len(parts) == 2 && strings.EqualFold(parts[0], "sessions")
	case visualStudioCopilotVS2026CopilotChatRoot:
		return len(parts) == 3 && strings.EqualFold(parts[1], "sessions")
	case visualStudioCopilotVS2026VSRoot:
		return len(parts) == 5 &&
			strings.EqualFold(parts[1], "copilot-chat") &&
			strings.EqualFold(parts[3], "sessions")
	default:
		return len(parts) == 6 &&
			strings.EqualFold(parts[0], ".vs") &&
			strings.EqualFold(parts[2], "copilot-chat") &&
			strings.EqualFold(parts[4], "sessions")
	}
}

type visualStudioCopilotVS2026RootMode int

const (
	visualStudioCopilotVS2026ProjectRoot visualStudioCopilotVS2026RootMode = iota
	visualStudioCopilotVS2026VSRoot
	visualStudioCopilotVS2026CopilotChatRoot
	visualStudioCopilotVS2026ThreadRoot
	visualStudioCopilotVS2026SessionsRoot
)

func visualStudioCopilotVS2026RootKind(
	root string,
) visualStudioCopilotVS2026RootMode {
	root = filepath.Clean(root)
	switch {
	case strings.EqualFold(filepath.Base(root), "sessions") &&
		strings.EqualFold(filepath.Base(filepath.Dir(filepath.Dir(root))), "copilot-chat"):
		return visualStudioCopilotVS2026SessionsRoot
	case strings.EqualFold(filepath.Base(filepath.Dir(root)), "copilot-chat"):
		return visualStudioCopilotVS2026ThreadRoot
	case strings.EqualFold(filepath.Base(root), "copilot-chat"):
		return visualStudioCopilotVS2026CopilotChatRoot
	case strings.EqualFold(filepath.Base(root), ".vs"):
		return visualStudioCopilotVS2026VSRoot
	default:
		return visualStudioCopilotVS2026ProjectRoot
	}
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

func vsCopilotCachedFingerprintKey(path string) string {
	return "visualstudio-copilot:fingerprint:" + filepath.Clean(path)
}

func vsCopilotFingerprintSourceContext(
	ctx context.Context, src multiSessionSource,
) (SourceFingerprint, error) {
	encoded, found, err := reconciliationCacheGet(
		ctx, vsCopilotCachedFingerprintKey(src.Container),
	)
	if err != nil {
		return SourceFingerprint{}, err
	}
	if !found {
		return vsCopilotFingerprintSource(src)
	}
	var fingerprint SourceFingerprint
	if err := json.Unmarshal([]byte(encoded), &fingerprint); err != nil {
		return SourceFingerprint{}, fmt.Errorf("decode cached Visual Studio Copilot fingerprint: %w", err)
	}
	return fingerprint, nil
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

func vsCopilotParseMemberContext(
	ctx context.Context, src multiSessionSource, req ParseRequest,
) (*ParseResult, error) {
	encoded, found, err := reconciliationCacheGet(
		ctx, vsCopilotCachedSpansKey(src.Root, src.MemberID),
	)
	if err != nil {
		return nil, err
	}
	if !found || isVisualStudioCopilotVS2026SessionPath(src.Container) {
		return vsCopilotParseMember(src, req)
	}
	retained := conservativeDecodedRetainedBytes(int64(len(encoded)))
	observeStreamingRetainedBytes(ctx, retained)
	defer observeStreamingRetainedBytes(ctx, -retained)
	observeReconciliationRetainedMember(ctx, AgentVSCopilot, retained)
	var spans []vsCopilotSpan
	for line := range strings.SplitSeq(encoded, "\n") {
		var span vsCopilotSpan
		if err := json.Unmarshal([]byte(line), &span); err != nil {
			return nil, fmt.Errorf("decode cached Visual Studio Copilot span: %w", err)
		}
		prepareVisualStudioCopilotSpan(&span)
		spans = append(spans, span)
	}
	project := firstNonEmptyJSONLString(req.Source.ProjectHint, "visualstudio")
	sess, msgs, err := buildVisualStudioCopilotConversationFromSpans(
		src.Container, src.MemberID, project, req.Machine, spans,
	)
	if err != nil || sess == nil {
		return nil, err
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
// Visual Studio Copilot validation: the container must name a trace file path and
// source ID must be a valid conversation ID. It returns ok=false for a plain
// trace-file path.
func splitVisualStudioCopilotVirtualPath(
	sourcePath string,
) (tracePath, conversationID string, ok bool) {
	tracePath, conversationID, ok = ParseVirtualSourcePath(sourcePath)
	if !ok {
		return "", "", false
	}
	if !isVisualStudioCopilotConversationPath(tracePath) ||
		!IsValidSessionID(conversationID) {
		return "", "", false
	}
	conversationID = canonicalVisualStudioCopilotConversationID(conversationID)
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
		Source: multiSessionContainerSourceCapabilities(
			CapabilitySupported,
			CapabilitySupported,
		),
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			AggregateUsageEvents: CapabilitySupported,
			Model:                CapabilitySupported,
		},
	}
}
