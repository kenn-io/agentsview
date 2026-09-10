package parser

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.kenn.io/agentsview/internal/pathutil"
)

var _ Provider = (*cursorProvider)(nil)
var _ S3Provider = (*cursorProvider)(nil)

type cursorProviderFactory struct {
	def        AgentDef
	storeIndex *cursorStoreIndex
}

func newCursorProviderFactory(def AgentDef) ProviderFactory {
	return &cursorProviderFactory{
		def:        cloneAgentDef(def),
		storeIndex: newCursorStoreIndex(),
	}
}

func (f *cursorProviderFactory) Definition() AgentDef {
	return cloneAgentDef(f.def)
}

func (f *cursorProviderFactory) Capabilities() Capabilities {
	return cursorProviderCapabilities()
}

func (f *cursorProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	cfg = cfg.Clone()
	sources := newCursorSourceSetWithConfig(cfg)
	sources.storeIndex = f.storeIndex
	return &cursorProvider{
		Def:               cloneAgentDef(f.def),
		Caps:              cursorProviderCapabilities(),
		Config:            cfg,
		DefaultS3Provider: cursorS3Provider,
		sources:           sources,
	}
}

// ResolveMetadataDir returns the sibling .cursor/chats directory for a local
// .cursor/projects root. Remote, rewritten and unrelated roots yield "".
func (f cursorProviderFactory) ResolveMetadataDir(path string) (string, error) {
	root, err := pathutil.ResolveAbsolute(path)
	if err != nil {
		return "", err
	}
	key, err := pathutil.LocalComparisonKey(root)
	if err != nil {
		return "", err
	}
	if filepath.Base(key) != "projects" || filepath.Base(filepath.Dir(key)) != ".cursor" {
		return "", nil
	}
	return pathutil.ResolveAbsolute(filepath.Join(filepath.Dir(root), "chats"))
}

type cursorProvider struct {
	ProviderBase
	DefaultS3Provider
	sources cursorSourceSet
}

func (p *cursorProvider) Discover(ctx context.Context) ([]SourceRef, error) {
	return p.sources.Discover(ctx)
}

func (p *cursorProvider) DiscoverEach(ctx context.Context, yield func(SourceRef) error) error {
	return p.sources.DiscoverEach(ctx, yield)
}

func (p *cursorProvider) WatchPlan(ctx context.Context) (WatchPlan, error) {
	return p.sources.WatchPlan(ctx)
}

func (p *cursorProvider) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	return p.sources.SourcesForChangedPath(ctx, req)
}

func (p *cursorProvider) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	req = ProviderFindRequestWithRawSessionID(p.Def, req)
	return p.sources.FindSource(ctx, req)
}

func (p *cursorProvider) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	return p.sources.Fingerprint(ctx, source)
}

func (p *cursorProvider) Parse(
	ctx context.Context,
	req ParseRequest,
) (ParseOutcome, error) {
	if err := ctx.Err(); err != nil {
		return ParseOutcome{}, err
	}
	path, ok := p.sources.pathFromSource(req.Source)
	if !ok {
		return ParseOutcome{}, fmt.Errorf("cursor source path unavailable")
	}
	machine := firstNonEmptyJSONLString(req.Machine, p.Config.Machine)
	cwd := ""
	if req.Source.CwdResolution.State == SourceCwdResolved {
		cwd = req.Source.CwdResolution.Path
	}
	sess, msgs, err := p.parseSession(
		path, req.Source.ProjectHint, cwd, machine,
	)
	if err != nil {
		return ParseOutcome{}, err
	}
	if sess == nil {
		return ParseOutcome{
			ResultSetComplete: true,
			SkipReason:        SkipNoSession,
		}, nil
	}
	storePath, agentID, enrichErr := p.sources.storePathForSource(req.Source, path)
	if enrichErr == nil && storePath != "" {
		enrichErr = enrichCursorSessionFromStore(
			ctx, storePath, agentID, sess, msgs,
		)
		if errors.Is(enrichErr, context.Canceled) ||
			errors.Is(enrichErr, context.DeadlineExceeded) {
			return ParseOutcome{}, enrichErr
		}
		if enrichErr != nil {
			enrichErr = fmt.Errorf("cursor store %s: %w", storePath, enrichErr)
		}
	}
	if errors.Is(enrichErr, errCursorStoreFormat) {
		log.Printf("warning: %v; using Cursor transcript only", enrichErr)
	} else if enrichErr != nil {
		return ParseOutcome{
			SourceErrors: []SourceError{{
				SourceKey:   req.Source.Key,
				DisplayPath: path,
				SessionID:   sess.ID,
				Err:         enrichErr,
				Retryable:   true,
			}},
			ResultSetComplete: true,
		}, nil
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
	return ParseOutcome{
		Results: []ParseResultOutcome{{
			Result: ParseResult{
				Session:  *sess,
				Messages: msgs,
			},
			DataVersion: DataVersionCurrent,
		}},
		ResultSetComplete: true,
	}, nil
}

type cursorSource struct {
	Root string
	Path string
}

type cursorSourceSet struct {
	roots              []string
	metadataDirs       map[string][]string
	storeIndex         *cursorStoreIndex
	resolver           func(string) string
	resolutionResolver func(string, CursorResolveMode, string) SourceCwdResolution
	remote             bool
	remoteRoots        map[string]bool
}

func newCursorSourceSet(roots []string) cursorSourceSet {
	return cursorSourceSet{
		roots:       cleanJSONLRoots(roots),
		storeIndex:  newCursorStoreIndex(),
		remoteRoots: make(map[string]bool),
	}
}

func newCursorSourceSetWithConfig(cfg ProviderConfig) cursorSourceSet {
	s := newCursorSourceSet(cfg.Roots)
	s.metadataDirs = cloneMetadataDirs(cfg.MetadataDirs)
	s.remote = cfg.PathRewriter != nil
	for root, machine := range cfg.SourceMachines {
		if machine != "" && machine != cfg.Machine {
			s.remoteRoots[filepath.Clean(root)] = true
		}
	}
	return s
}

type cursorResolutionCache map[string]SourceCwdResolution

func (s cursorSourceSet) resolveCwd(
	root, projectDir string, mode CursorResolveMode, hint string,
	cache cursorResolutionCache,
) SourceCwdResolution {
	cacheKey := filepath.Clean(root) + "\x00" + projectDir
	if cache != nil && mode == CursorResolvePassiveDiscovery {
		if resolution, ok := cache[cacheKey]; ok {
			return resolution
		}
	}
	var resolution SourceCwdResolution
	if s.remote || s.remoteRoot(root) {
		resolution = SourceCwdResolution{State: SourceCwdRemote}
	} else if s.resolutionResolver != nil {
		resolution = s.resolutionResolver(projectDir, mode, hint)
	} else if s.resolver != nil && mode == CursorResolvePassiveDiscovery {
		if path := s.resolver(projectDir); path != "" {
			resolution = SourceCwdResolution{
				State: SourceCwdResolved,
				Path:  path,
			}
		} else {
			resolution = SourceCwdResolution{State: SourceCwdNone}
		}
	} else {
		resolution = ResolveCursorWorkspaceDirResolution(
			"", projectDir, hint, mode,
		)
	}
	if cache != nil && mode == CursorResolvePassiveDiscovery {
		cache[cacheKey] = resolution
	}
	return resolution
}

func (s cursorSourceSet) remoteRoot(root string) bool {
	clean := filepath.Clean(root)
	if s.remoteRoots[clean] {
		return true
	}
	for configured := range s.remoteRoots {
		if samePath(configured, clean) {
			return true
		}
	}
	return false
}

var cursorS3Provider = DefaultS3Provider{
	Agent:      AgentCursor,
	IDPrefix:   cursorSessionIDPrefix,
	Extensions: []string{".jsonl", ".txt"},
}

func (s cursorSourceSet) Discover(ctx context.Context) ([]SourceRef, error) {
	var sources []SourceRef
	seen := make(map[string]struct{})
	resolutionCache := make(cursorResolutionCache)
	s3FilesByRoot, err := discoverCursorS3ByRoot(ctx, s.roots)
	if err != nil {
		return nil, err
	}
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if isS3URI(root) {
			for _, file := range s3FilesByRoot[root] {
				addJSONLSource(s3SourceRefFromDiscoveredFile(root, file), &sources, seen)
			}
			continue
		}
		for _, chats := range s.chatsDirs(root) {
			s.storeIndex.refresh(chats)
		}
		for _, path := range s.discoverTranscriptPaths(root) {
			source, ok := s.sourceRefWithCache(root, path, resolutionCache)
			if !ok {
				continue
			}
			addJSONLSource(source, &sources, seen)
		}
	}
	sortJSONLSources(sources)
	return sources, nil
}

func (s cursorSourceSet) DiscoverEach(ctx context.Context, yield func(SourceRef) error) error {
	resolutionCache := make(cursorResolutionCache)
	s3FilesByRoot, err := discoverCursorS3ByRoot(ctx, s.roots)
	if err != nil {
		return err
	}
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return err
		}
		if isS3URI(root) {
			for _, file := range s3FilesByRoot[root] {
				if err := yield(s3SourceRefFromDiscoveredFile(root, file)); err != nil {
					return err
				}
			}
			continue
		}
		for _, chats := range s.chatsDirs(root) {
			s.storeIndex.refresh(chats)
		}
		resolvedRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			if _, lstatErr := os.Lstat(root); errors.Is(lstatErr, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("resolve cursor root %s: %w", root, err)
		}
		err = streamDirectoryEntries(ctx, root, func(project os.DirEntry) error {
			if !project.IsDir() || project.Type()&os.ModeSymlink != 0 {
				return nil
			}
			dir := filepath.Join(root, project.Name(), "agent-transcripts")
			resolvedDir, resolveErr := filepath.EvalSymlinks(dir)
			if errors.Is(resolveErr, os.ErrNotExist) {
				if _, lstatErr := os.Lstat(dir); errors.Is(lstatErr, os.ErrNotExist) {
					return nil
				}
				return fmt.Errorf("resolve cursor transcripts %s: %w", dir, resolveErr)
			}
			if resolveErr != nil {
				return fmt.Errorf("resolve cursor transcripts %s: %w", dir, resolveErr)
			}
			if !isContainedIn(resolvedDir, resolvedRoot) {
				return nil
			}
			topLevel := make(map[string]struct{})
			var sessionDirs []string
			err := streamDirectoryEntries(ctx, dir, func(entry os.DirEntry) error {
				if !entry.IsDir() {
					if !IsCursorTranscriptExt(entry.Name()) {
						return nil
					}
					if entry.Type().IsRegular() {
						name := entry.Name()
						topLevel[strings.TrimSuffix(name, filepath.Ext(name))] = struct{}{}
					}
					return s.yieldIfCanonical(
						root, filepath.Join(dir, entry.Name()), resolutionCache, yield,
					)
				}
				sessionDir := filepath.Join(dir, entry.Name())
				sessionDirs = append(sessionDirs, sessionDir)
				base := filepath.Join(sessionDir, entry.Name())
				path := ""
				for _, ext := range []string{".jsonl", ".txt"} {
					// A symlink must not reserve the stem or outrank a regular file.
					info, statErr := os.Lstat(base + ext)
					if errors.Is(statErr, os.ErrNotExist) {
						continue
					}
					if statErr != nil {
						return fmt.Errorf("stat cursor candidate %s: %w", base+ext, statErr)
					}
					if info.Mode().IsRegular() {
						path = base + ext
						break
					}
				}
				if path == "" {
					return nil
				}
				topLevel[entry.Name()] = struct{}{}
				return s.yieldIfCanonical(root, path, resolutionCache, yield)
			})
			if err != nil {
				return err
			}
			return s.streamSubagentTranscripts(
				ctx, root, sessionDirs, topLevel, resolutionCache, yield,
			)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (s cursorSourceSet) yieldIfCanonical(
	root, path string,
	resolutionCache cursorResolutionCache,
	yield func(SourceRef) error,
) error {
	source, ok, err := s.streamingSourceRefWithCache(root, path, resolutionCache)
	if err != nil {
		return err
	}
	if ok {
		return yield(source)
	}
	return nil
}

// streamSubagentTranscripts resolves a project's subagent files through the
// same stem map as the batch walk, so a child stem under two parents yields
// one source in both walks and a top-level transcript always shadows it. The
// map holds one entry per child, so streaming stays bounded. A symlinked
// subagents folder is skipped like a symlinked project directory, and each
// folder is read with one ReadDir: it holds a handful of files, so the batched
// streamer that guards huge flat archives costs more than it saves here.
func (s cursorSourceSet) streamSubagentTranscripts(
	ctx context.Context,
	root string,
	sessionDirs []string,
	topLevel map[string]struct{},
	resolutionCache cursorResolutionCache,
	yield func(SourceRef) error,
) error {
	// The streamer reports entries in filesystem order; the batch walk's
	// os.ReadDir sorts, so sort here too or "first parent wins" diverges.
	sort.Strings(sessionDirs)
	subagentSeen := make(map[string]string)
	for _, sessionDir := range sessionDirs {
		if err := ctx.Err(); err != nil {
			return err
		}
		subagentsDir := filepath.Join(sessionDir, cursorSubagentsDirName)
		info, err := os.Lstat(subagentsDir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("stat cursor subagents %s: %w", subagentsDir, err)
		}
		if !info.IsDir() {
			continue
		}
		entries, err := os.ReadDir(subagentsDir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read cursor subagents %s: %w", subagentsDir, err)
		}
		for _, entry := range entries {
			if !entry.Type().IsRegular() || !IsCursorTranscriptExt(entry.Name()) {
				continue
			}
			cursorAddSeen(subagentSeen, entry.Name(), filepath.Join(subagentsDir, entry.Name()))
		}
	}
	stems := make([]string, 0, len(subagentSeen))
	for stem := range subagentSeen {
		if _, shadowed := topLevel[stem]; !shadowed {
			stems = append(stems, stem)
		}
	}
	sort.Strings(stems)
	for _, stem := range stems {
		if err := s.yieldIfCanonical(
			root, subagentSeen[stem], resolutionCache, yield,
		); err != nil {
			return err
		}
	}
	return nil
}

func (s cursorSourceSet) streamingSourceRefWithCache(
	root, path string, resolutionCache cursorResolutionCache,
) (SourceRef, bool, error) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	// A transcript listed by the walk can vanish before this stat — a
	// routine deletion race, not a discovery failure; the next
	// enumeration simply no longer lists it. Genuine read errors and
	// dangling symlinks still propagate, matching
	// cursorFindSourceFileInProjectStrict.
	regular, err := streamingRegularFileCandidate(path)
	if err != nil {
		return SourceRef{}, false, fmt.Errorf("stat cursor transcript %s: %w", path, err)
	}
	if !regular {
		return SourceRef{}, false, nil
	}
	loc, ok := cursorTranscriptLocationInRoot(root, path)
	if !ok {
		return SourceRef{}, false, nil
	}
	selected, err := cursorFindSourceFileInProjectStrict(root, loc)
	if err != nil {
		return SourceRef{}, false, err
	}
	if selected == "" || !samePath(selected, path) {
		return SourceRef{}, false, nil
	}
	project := DecodeCursorProjectDir(loc.ProjectDir)
	if project == "" {
		project = "unknown"
	}
	s.storeIndex.rememberTranscript(root, path)
	return SourceRef{
		Provider: AgentCursor, Key: path, DisplayPath: path,
		FingerprintKey: path, ProjectHint: project,
		CwdResolution: s.resolveCwd(
			root, loc.ProjectDir, CursorResolvePassiveDiscovery, "", resolutionCache,
		),
		Opaque: cursorSource{
			Root: root, Path: path,
		},
	}, true, nil
}

func cursorFindSourceFileInProjectStrict(
	root string, loc cursorTranscriptLocation,
) (string, error) {
	if root == "" || loc.ProjectDir == "" || !IsValidSessionID(loc.RawID) {
		return "", nil
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve cursor root %s: %w", root, err)
	}
	transcriptsDir := filepath.Join(root, loc.ProjectDir, "agent-transcripts")
	for _, candidate := range cursorTranscriptCandidates(transcriptsDir, loc) {
		// Lstat so a symlinked transcript is skipped here as the batch walk
		// and parseSession's O_NOFOLLOW open already skip it.
		info, statErr := os.Lstat(candidate)
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return "", fmt.Errorf("stat cursor transcript %s: %w", candidate, statErr)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		resolved, resolveErr := filepath.EvalSymlinks(candidate)
		if resolveErr != nil {
			return "", fmt.Errorf("resolve cursor transcript %s: %w", candidate, resolveErr)
		}
		if isContainedIn(resolved, resolvedRoot) {
			return candidate, nil
		}
	}
	return "", nil
}

// discoverTranscriptPaths walks a Cursor projects root and returns the primary
// transcript file paths. All paths resolve within the canonical root,
// preventing symlink escapes. Symlinked project directory entries are rejected.
// Cursor uses three layouts: flat (agent-transcripts/<uuid>.{txt,jsonl}),
// nested (agent-transcripts/<uuid>/<uuid>.{txt,jsonl}), and subagent
// (agent-transcripts/<parent>/subagents/<uuid>.{txt,jsonl}); when both .jsonl
// and .txt exist for the same stem, .jsonl is preferred.
func (s cursorSourceSet) discoverTranscriptPaths(projectsDir string) []string {
	if projectsDir == "" {
		return nil
	}

	// Canonicalize root once for containment checks.
	resolvedRoot, err := filepath.EvalSymlinks(projectsDir)
	if err != nil {
		return nil
	}

	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return nil
	}

	var paths []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// Reject symlinked project directory entries.
		if entry.Type()&os.ModeSymlink != 0 {
			continue
		}

		transcriptsDir := filepath.Join(
			projectsDir, entry.Name(), "agent-transcripts",
		)

		// Verify the transcripts directory resolves within
		// the canonical root.
		resolvedDir, err := filepath.EvalSymlinks(transcriptsDir)
		if err != nil {
			continue
		}
		if !isContainedIn(resolvedDir, resolvedRoot) {
			continue
		}

		transcripts, err := os.ReadDir(transcriptsDir)
		if err != nil {
			continue
		}

		// Collect valid transcripts, deduping by basename
		// stem. When both .jsonl and .txt exist for the
		// same session, prefer .jsonl.
		seen := make(map[string]string) // stem -> path
		var subagentsDirs []string
		for _, sf := range transcripts {
			if !sf.IsDir() {
				// Flat layout: file directly in
				// agent-transcripts/.
				name := sf.Name()
				if !IsCursorTranscriptExt(name) {
					continue
				}
				fullPath := filepath.Join(transcriptsDir, name)
				if !IsRegularFile(fullPath) {
					continue
				}
				cursorAddSeen(seen, name, fullPath)
				continue
			}

			// Nested layout: agent-transcripts/<uuid>/
			// containing <uuid>.{txt,jsonl}.
			subDir := filepath.Join(transcriptsDir, sf.Name())
			subEntries, err := os.ReadDir(subDir)
			if err != nil {
				continue
			}
			dirName := sf.Name()
			for _, sub := range subEntries {
				if sub.IsDir() {
					// A symlinked subagents entry reports IsDir false
					// and is skipped with the other non-transcripts.
					if sub.Name() == cursorSubagentsDirName {
						subagentsDirs = append(
							subagentsDirs, filepath.Join(subDir, sub.Name()),
						)
					}
					continue
				}
				name := sub.Name()
				if !IsCursorTranscriptExt(name) {
					continue
				}
				// Only accept files whose stem matches
				// the parent directory name, e.g.
				// <uuid>/<uuid>.jsonl.
				stem := strings.TrimSuffix(name, filepath.Ext(name))
				if stem != dirName {
					continue
				}
				fullPath := filepath.Join(subDir, name)
				if !IsRegularFile(fullPath) {
					continue
				}
				cursorAddSeen(seen, name, fullPath)
			}
		}
		// A session's own transcript outranks a copy in another session's
		// subagents directory regardless of extension, matching
		// cursorTranscriptCandidates, so subagent files fill unseen stems only.
		subagentSeen := make(map[string]string)
		for _, subagentsDir := range subagentsDirs {
			cursorCollectSubagentTranscripts(subagentSeen, subagentsDir)
		}
		for stem, path := range subagentSeen {
			if _, ok := seen[stem]; !ok {
				seen[stem] = path
			}
		}
		for _, path := range seen {
			paths = append(paths, path)
		}
	}
	return paths
}

func cursorCollectSubagentTranscripts(seen map[string]string, subagentsDir string) {
	entries, err := os.ReadDir(subagentsDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !IsCursorTranscriptExt(entry.Name()) {
			continue
		}
		fullPath := filepath.Join(subagentsDir, entry.Name())
		if !IsRegularFile(fullPath) {
			continue
		}
		cursorAddSeen(seen, entry.Name(), fullPath)
	}
}

// cursorAddSeen inserts a transcript path into the seen map, preferring .jsonl
// over .txt when both exist for the same stem.
func cursorAddSeen(seen map[string]string, name, fullPath string) {
	stem := strings.TrimSuffix(name, filepath.Ext(name))
	if prev, ok := seen[stem]; ok {
		if strings.HasSuffix(prev, ".txt") &&
			strings.HasSuffix(name, ".jsonl") {
			seen[stem] = fullPath
		}
		return
	}
	seen[stem] = fullPath
}

func (s cursorSourceSet) WatchPlan(context.Context) (WatchPlan, error) {
	roots := make([]WatchRoot, 0, len(s.roots)*2)
	seenChats := make(map[string]struct{})
	for _, root := range s.roots {
		if isS3URI(root) {
			continue
		}
		roots = append(roots, WatchRoot{
			Path:         root,
			Recursive:    true,
			IncludeGlobs: []string{"*.jsonl", "*.txt"},
			DebounceKey:  string(AgentCursor) + ":transcripts:" + root,
		})
		for _, chats := range s.chatsDirs(root) {
			chats = filepath.Clean(chats)
			if _, ok := seenChats[chats]; ok {
				continue
			}
			seenChats[chats] = struct{}{}
			roots = append(roots, WatchRoot{
				Path:        chats,
				Recursive:   true,
				DebounceKey: string(AgentCursor) + ":store:" + chats,
			})
		}
	}
	return WatchPlan{Roots: roots}, nil
}

func (s cursorSourceSet) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.WatchRoot != "" {
		watchRoot := filepath.Clean(req.WatchRoot)
		if s.hasRoot(watchRoot) {
			source, ok := s.sourceForPathInRoot(watchRoot, req.Path)
			if !ok {
				return nil, nil
			}
			return []SourceRef{source}, nil
		}
		if projectsRoot, ok := s.projectsRootForChats(watchRoot); ok {
			return s.sourcesForStorePath(projectsRoot, req.Path)
		}
		return nil, nil
	}
	for _, root := range s.roots {
		source, ok := s.sourceForPathInRoot(root, req.Path)
		if ok {
			return []SourceRef{source}, nil
		}
	}
	for _, root := range s.roots {
		if sources, err := s.sourcesForStorePath(root, req.Path); err != nil {
			return nil, err
		} else if len(sources) > 0 {
			return sources, nil
		}
	}
	return nil, nil
}

func (s cursorSourceSet) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	if err := ctx.Err(); err != nil {
		return SourceRef{}, false, err
	}
	for _, path := range []string{req.StoredFilePath, req.FingerprintKey} {
		if path == "" {
			continue
		}
		if source, ok := s.sourceForPath(path); ok {
			return source, true, nil
		}
	}
	if req.RawSessionID == "" {
		return SourceRef{}, false, nil
	}
	for _, root := range s.roots {
		path := cursorFindSourceFile(root, req.RawSessionID)
		if path == "" {
			continue
		}
		if source, ok := s.sourceRef(root, path); ok {
			return source, true, nil
		}
	}
	return SourceRef{}, false, nil
}

// cursorFindSourceFile finds a Cursor transcript file by session UUID across a
// projects root, preferring .jsonl over .txt. A top-level transcript wins over
// one stored in another session's subagents directory. Returns "" if no
// matching file resolves within the canonical root.
func cursorFindSourceFile(projectsDir, sessionID string) string {
	if projectsDir == "" || !IsValidSessionID(sessionID) {
		return ""
	}

	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return ""
	}

	resolvedRoot, err := filepath.EvalSymlinks(projectsDir)
	if err != nil {
		return ""
	}

	for _, ext := range []string{".jsonl", ".txt"} {
		target := sessionID + ext
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			transcriptsDir := filepath.Join(
				projectsDir, entry.Name(), "agent-transcripts",
			)
			// Nested layout first (matches discovery
			// precedence), then flat layout.
			for _, candidate := range []string{
				filepath.Join(transcriptsDir, sessionID, target),
				filepath.Join(transcriptsDir, target),
			} {
				if cursorTranscriptContained(candidate, resolvedRoot) {
					return candidate
				}
			}
		}
	}

	var subagentsDirs []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		transcriptsDir := filepath.Join(
			projectsDir, entry.Name(), "agent-transcripts",
		)
		sessions, err := os.ReadDir(transcriptsDir)
		if err != nil {
			continue
		}
		for _, session := range sessions {
			name := session.Name()
			if !session.IsDir() || name == sessionID || !IsValidSessionID(name) {
				continue
			}
			subagentsDirs = append(subagentsDirs, filepath.Join(
				transcriptsDir, name, cursorSubagentsDirName,
			))
		}
	}
	for _, ext := range []string{".jsonl", ".txt"} {
		for _, subagentsDir := range subagentsDirs {
			candidate := filepath.Join(subagentsDir, sessionID+ext)
			if cursorTranscriptContained(candidate, resolvedRoot) {
				return candidate
			}
		}
	}
	return ""
}

func cursorTranscriptContained(candidate, resolvedRoot string) bool {
	if !IsRegularFile(candidate) {
		return false
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return false
	}
	return isContainedIn(resolved, resolvedRoot)
}

func (s cursorSourceSet) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	if err := ctx.Err(); err != nil {
		return SourceFingerprint{}, err
	}
	path, ok := s.pathFromSource(source)
	if !ok {
		return SourceFingerprint{}, fmt.Errorf("cursor source path unavailable")
	}
	info, err := os.Stat(path)
	if err != nil {
		return SourceFingerprint{}, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.IsDir() {
		return SourceFingerprint{}, fmt.Errorf("stat %s: source is a directory", path)
	}
	hash := ""
	if info.Size() <= maxCursorTranscriptSize {
		hash, err = hashJSONLSourceFile(path)
		if err != nil {
			return SourceFingerprint{}, err
		}
	}
	mtime := info.ModTime().UnixNano()
	storePath, _, err := s.storePathForSource(source, path)
	if err != nil {
		return SourceFingerprint{}, err
	}
	if storePath != "" {
		if composite, err := sqliteDBCompositeMtime(
			storePath, sqliteDBJournalSuffixes,
		); err == nil && composite > mtime {
			mtime = composite
		}
		stateHash, err := cursorIDESQLiteStateHash(storePath)
		if err != nil {
			return SourceFingerprint{}, fmt.Errorf("fingerprint Cursor store %s: %w", storePath, err)
		}
		if hash == "" {
			hash = "store:" + stateHash
		} else {
			hash = hash + "|store:" + stateHash
		}
	}
	return SourceFingerprint{
		Key:     firstNonEmptyJSONLString(source.FingerprintKey, source.Key, path),
		Size:    info.Size(),
		MTimeNS: mtime,
		Hash:    hash,
	}, nil
}

func (s cursorSourceSet) pathFromSource(source SourceRef) (string, bool) {
	switch src := source.Opaque.(type) {
	case cursorSource:
		return src.Path, src.Path != ""
	case *cursorSource:
		if src != nil && src.Path != "" {
			return src.Path, true
		}
	case MaterializedFileSource:
		return src.Path, src.Path != ""
	}
	for _, candidate := range []string{
		source.DisplayPath,
		source.FingerprintKey,
		source.Key,
	} {
		if ref, ok := s.sourceForPath(candidate); ok {
			src := ref.Opaque.(cursorSource)
			return src.Path, true
		}
	}
	return "", false
}

func (s cursorSourceSet) sourceForPath(path string) (SourceRef, bool) {
	for _, root := range s.roots {
		if source, ok := s.sourceForPathInRoot(root, path); ok {
			return source, true
		}
	}
	return SourceRef{}, false
}

func (s cursorSourceSet) sourceForPathInRoot(
	root string,
	path string,
) (SourceRef, bool) {
	loc, ok := cursorTranscriptLocationInRoot(root, path)
	if !ok {
		return SourceRef{}, false
	}
	// Changed and stored paths have not passed discovery's project-wide
	// deduplication. Resolve only this stem across all parents.
	loc.ParentRawID = ""
	selected := cursorFindSourceFileInProject(root, loc)
	if selected == "" {
		transcriptsDir := filepath.Join(root, loc.ProjectDir, "agent-transcripts")
		parents, err := os.ReadDir(transcriptsDir)
		if err != nil {
			return SourceRef{}, false
		}
		seen := make(map[string]string)
		for _, parent := range parents {
			if !parent.IsDir() || parent.Name() == loc.RawID || !IsValidSessionID(parent.Name()) {
				continue
			}
			subagentsDir := filepath.Join(transcriptsDir, parent.Name(), cursorSubagentsDirName)
			info, err := os.Lstat(subagentsDir)
			if err != nil || !info.IsDir() {
				continue
			}
			loc.ParentRawID = parent.Name()
			if candidate := cursorFindSourceFileInProject(root, loc); candidate != "" {
				cursorAddSeen(seen, filepath.Base(candidate), candidate)
			}
		}
		selected = seen[loc.RawID]
	}
	if selected == "" {
		return SourceRef{}, false
	}
	return s.sourceRef(root, selected)
}

func (s cursorSourceSet) sourceRef(root, path string) (SourceRef, bool) {
	return s.sourceRefWithCache(root, path, nil)
}

func (s cursorSourceSet) sourceRefWithCache(
	root, path string, resolutionCache cursorResolutionCache,
) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if !IsRegularFile(path) {
		return SourceRef{}, false
	}
	loc, ok := cursorTranscriptLocationInRoot(root, path)
	if !ok {
		return SourceRef{}, false
	}
	selected := cursorFindSourceFileInProject(root, loc)
	if selected == "" || !samePath(selected, path) {
		return SourceRef{}, false
	}
	project := DecodeCursorProjectDir(loc.ProjectDir)
	if project == "" {
		project = "unknown"
	}
	s.storeIndex.rememberTranscript(root, path)
	return SourceRef{
		Provider:       AgentCursor,
		Key:            path,
		DisplayPath:    path,
		FingerprintKey: path,
		ProjectHint:    project,
		CwdResolution: s.resolveCwd(
			root, loc.ProjectDir, CursorResolvePassiveDiscovery, "", resolutionCache,
		),
		Opaque: cursorSource{
			Root: root,
			Path: path,
		},
	}, true
}

func (s cursorSourceSet) hasRoot(root string) bool {
	for _, configured := range s.roots {
		if samePath(root, configured) {
			return true
		}
	}
	return false
}

func (s cursorSourceSet) chatsDirs(root string) []string {
	if isS3URI(root) || s.remote || s.remoteRoot(root) {
		return nil
	}
	clean := filepath.Clean(root)
	if dirs := s.metadataDirs[clean]; len(dirs) > 0 {
		return dirs
	}
	for configured, dirs := range s.metadataDirs {
		if samePath(configured, clean) {
			return dirs
		}
	}
	return nil
}

func (s cursorSourceSet) projectsRootForChats(chatsRoot string) (string, bool) {
	chatsRoot = filepath.Clean(chatsRoot)
	for _, root := range s.roots {
		for _, chats := range s.chatsDirs(root) {
			if samePath(chats, chatsRoot) {
				return root, true
			}
		}
	}
	return "", false
}

func (s cursorSourceSet) storePathForSource(
	source SourceRef, path string,
) (string, string, error) {
	root := ""
	switch src := source.Opaque.(type) {
	case cursorSource:
		root = src.Root
	case *cursorSource:
		if src != nil {
			root = src.Root
		}
	}
	if root == "" {
		for _, candidate := range s.roots {
			loc, ok := cursorTranscriptLocationInRoot(candidate, path)
			if ok && IsValidSessionID(loc.RawID) {
				root = candidate
				break
			}
		}
	}
	if root == "" {
		return "", "", nil
	}
	agentID := cursorRawIDFromTranscriptPath(path)
	if !IsValidSessionID(agentID) {
		return "", "", nil
	}
	storePath, err := s.storePathForRawID(root, agentID)
	return storePath, agentID, err
}

func (s cursorSourceSet) storePathForRawID(root, agentID string) (string, error) {
	for _, chats := range s.chatsDirs(root) {
		if !s.storeIndex.initialized(chats) {
			s.storeIndex.refresh(chats)
		}
		if path, err := s.storeIndex.path(chats, agentID); path != "" || err != nil {
			return path, err
		}
	}
	return "", nil
}

func (s cursorSourceSet) sourcesForStorePath(root, path string) ([]SourceRef, error) {
	agentID, ok := cursorStoreAgentIDFromPath(path)
	if !ok {
		return nil, nil
	}
	underMetadata := false
	matchingChats := ""
	for _, chats := range s.chatsDirs(root) {
		if !isContainedIn(filepath.Clean(path), filepath.Clean(chats)) {
			continue
		}
		underMetadata = true
		matchingChats = chats
		break
	}
	if !underMetadata {
		return nil, nil
	}
	s.storeIndex.remember(
		matchingChats, agentID, filepath.Join(filepath.Dir(path), "store.db"),
	)
	transcript := s.storeIndex.transcriptPath(root, agentID)
	if transcript == "" {
		transcript = cursorFindSourceFile(root, agentID)
	}
	source, ok := s.sourceRef(root, transcript)
	if !ok {
		return nil, nil
	}
	return []SourceRef{source}, nil
}

func cursorFindSourceFileInProject(root string, loc cursorTranscriptLocation) string {
	if root == "" || loc.ProjectDir == "" || !IsValidSessionID(loc.RawID) {
		return ""
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return ""
	}
	transcriptsDir := filepath.Join(root, loc.ProjectDir, "agent-transcripts")
	for _, candidate := range cursorTranscriptCandidates(transcriptsDir, loc) {
		if cursorTranscriptContained(candidate, resolvedRoot) {
			return candidate
		}
	}
	return ""
}

func cursorProviderCapabilities() Capabilities {
	return Capabilities{
		Source: SourceCapabilities{
			DiscoverSources:      CapabilitySupported,
			StreamingDiscovery:   CapabilitySupported,
			WatchSources:         CapabilitySupported,
			ClassifyChangedPath:  CapabilitySupported,
			FindSource:           CapabilitySupported,
			CompositeFingerprint: CapabilitySupported,
			MultiSessionSource:   CapabilityNotApplicable,
			PerSessionErrors:     CapabilityNotApplicable,
			ExcludedSessions:     CapabilityNotApplicable,
			ForceReplaceOnParse:  CapabilityNotApplicable,
			S3Discovery:          CapabilitySupported,
		},
		Content: ContentCapabilities{
			FirstMessage:     CapabilitySupported,
			Thinking:         CapabilitySupported,
			ToolCalls:        CapabilitySupported,
			ToolResults:      CapabilitySupported,
			ToolResultEvents: CapabilitySupported,
		},
		Sync: ProviderSyncSemantics{
			FingerprintHashInCacheKey:           true,
			FingerprintHashRequiredForFreshness: true,
		},
	}
}
