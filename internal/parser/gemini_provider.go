package parser

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var _ Provider = (*geminiProvider)(nil)

type geminiProviderFactory struct {
	def AgentDef
}

func newGeminiProviderFactory(def AgentDef) ProviderFactory {
	return geminiProviderFactory{def: cloneAgentDef(def)}
}

func (f geminiProviderFactory) Definition() AgentDef {
	return cloneAgentDef(f.def)
}

func (f geminiProviderFactory) Capabilities() Capabilities {
	return geminiProviderCapabilities()
}

func (f geminiProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	cfg = cfg.Clone()
	return &geminiProvider{
		ProviderBase: ProviderBase{
			Def:    cloneAgentDef(f.def),
			Caps:   geminiProviderCapabilities(),
			Config: cfg,
		},
		sources: newGeminiSourceSet(cfg.Roots),
	}
}

type geminiProvider struct {
	ProviderBase
	sources geminiSourceSet
}

func (p *geminiProvider) Discover(ctx context.Context) ([]SourceRef, error) {
	return p.sources.Discover(ctx)
}

func (p *geminiProvider) DiscoverEach(ctx context.Context, yield func(SourceRef) error) error {
	return p.sources.DiscoverEach(ctx, yield)
}

func (p *geminiProvider) WatchPlan(ctx context.Context) (WatchPlan, error) {
	return p.sources.WatchPlan(ctx)
}

func (p *geminiProvider) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	return p.sources.SourcesForChangedPath(ctx, req)
}

func (p *geminiProvider) SourceForReconciliation(
	ctx context.Context,
	path, project string,
) (SourceRef, bool, error) {
	return p.sources.SourceForReconciliation(ctx, path, project)
}

func (p *geminiProvider) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	req = ProviderFindRequestWithRawSessionID(p.Def, req)
	return p.sources.FindSource(ctx, req)
}

func (p *geminiProvider) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	return p.sources.Fingerprint(ctx, source)
}

func (p *geminiProvider) Parse(
	ctx context.Context,
	req ParseRequest,
) (ParseOutcome, error) {
	if err := ctx.Err(); err != nil {
		return ParseOutcome{}, err
	}
	path, ok := p.sources.pathFromSource(req.Source)
	if !ok {
		return ParseOutcome{}, fmt.Errorf("gemini source path unavailable")
	}
	machine := firstNonEmptyJSONLString(req.Machine, p.Config.Machine)
	sess, msgs, err := p.parseSession(path, req.Source.ProjectHint, machine)
	if err != nil {
		return ParseOutcome{}, err
	}
	if sess == nil {
		return ParseOutcome{
			ResultSetComplete: true,
			SkipReason:        SkipNoSession,
		}, nil
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

type geminiSource struct {
	Root string
	Path string
}

type geminiSourceSet struct {
	roots []string
}

func newGeminiSourceSet(roots []string) geminiSourceSet {
	return geminiSourceSet{roots: cleanJSONLRoots(roots)}
}

func (s geminiSourceSet) Discover(ctx context.Context) ([]SourceRef, error) {
	var sources []SourceRef
	seen := make(map[string]struct{})
	for _, root := range s.roots {
		rootSources, err := s.discoverRoot(ctx, root)
		if err != nil {
			return nil, err
		}
		for _, source := range rootSources {
			addJSONLSource(source, &sources, seen)
		}
	}
	sortJSONLSources(sources)
	return sources, nil
}

func (s geminiSourceSet) DiscoverEach(ctx context.Context, yield func(SourceRef) error) error {
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return err
		}
		projects, err := newDiscoveryDiskMapForContext(ctx)
		if err != nil {
			return err
		}
		if err := projects.loadGeminiConfig(ctx, root); err != nil {
			return errors.Join(err, projects.close())
		}
		tmpDir := filepath.Join(root, "tmp")
		err = streamDirectoryEntries(ctx, tmpDir, func(projectDir os.DirEntry) error {
			isProjectDir, dirErr := streamingDirCandidateOrIncomplete(
				AgentGemini, "Gemini project directory", projectDir, tmpDir,
			)
			if dirErr != nil {
				return dirErr
			}
			if !isProjectDir {
				return nil
			}
			project, _, err := projects.get(ctx, projectDir.Name())
			if err != nil {
				return err
			}
			if project == "" {
				if isHexHash(projectDir.Name()) {
					project = "unknown"
				} else {
					project = NormalizeName(projectDir.Name())
				}
			}
			chatDir := filepath.Join(tmpDir, projectDir.Name(), geminiChatsDir)
			return streamDirectoryEntries(ctx, chatDir, func(entry os.DirEntry) error {
				if entry.IsDir() || !isGeminiSessionFilename(entry.Name()) {
					return nil
				}
				path := filepath.Join(chatDir, entry.Name())
				source, ok := s.sourceRefForPathWithProjectMap(
					root, path, true, map[string]string{projectDir.Name(): project},
				)
				if ok {
					return yield(source)
				}
				return nil
			})
		})
		err = errors.Join(err, projects.close())
		if err != nil {
			return err
		}
	}
	return nil
}

func (s geminiSourceSet) discoverRoot(
	ctx context.Context,
	root string,
) ([]SourceRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sources := make([]SourceRef, 0)
	seen := make(map[string]struct{})
	// Build the project map once per root. It depends only on root, and
	// BuildGeminiProjectMap re-reads and SHA-256-hashes projects.json on every
	// call, so resolving it per source path made discovery scale with session
	// count (the dominant cost on large archives).
	projectMap := buildGeminiProjectMap(root)
	for _, path := range s.discoverSessionPaths(root) {
		source, ok := s.sourceRefForPathWithProjectMap(root, path, true, projectMap)
		if !ok {
			continue
		}
		addJSONLSource(source, &sources, seen)
	}
	sortJSONLSources(sources)
	return sources, nil
}

// discoverSessionPaths finds all Gemini session file paths under the Gemini
// directory (<root>/tmp/<dir>/chats/session-*.json[l]). <dir> is either a
// SHA-256 project hash (old layout) or a project name (new layout); symlinked
// hash directories are followed (matching the watcher). Project resolution is
// applied by sourceRef via BuildGeminiProjectMap/ResolveGeminiProject, so this
// helper only enumerates source paths.
func (s geminiSourceSet) discoverSessionPaths(root string) []string {
	if root == "" {
		return nil
	}

	tmpDir := filepath.Join(root, "tmp")
	hashDirs, err := os.ReadDir(tmpDir)
	if err != nil {
		return nil
	}

	var paths []string
	for _, hd := range hashDirs {
		if !isDirOrSymlink(hd, tmpDir) {
			continue
		}
		chatsDir := filepath.Join(tmpDir, hd.Name(), geminiChatsDir)
		entries, err := os.ReadDir(chatsDir)
		if err != nil {
			continue
		}
		for _, sf := range entries {
			if sf.IsDir() {
				continue
			}
			name := sf.Name()
			if !isGeminiSessionFilename(name) {
				continue
			}
			paths = append(paths, filepath.Join(chatsDir, name))
		}
	}
	return paths
}

func (s geminiSourceSet) WatchPlan(context.Context) (WatchPlan, error) {
	roots := make([]WatchRoot, 0, len(s.roots))
	for _, root := range s.roots {
		tmp := filepath.Join(root, "tmp")
		roots = append(roots, WatchRoot{
			Path:         tmp,
			Recursive:    true,
			IncludeGlobs: []string{"session-*.json", "session-*.jsonl"},
			DebounceKey:  string(AgentGemini) + ":tmp:" + tmp,
		})
		roots = append(roots, WatchRoot{
			Path:         root,
			Recursive:    false,
			IncludeGlobs: []string{"projects.json", "trustedFolders.json"},
			DebounceKey:  string(AgentGemini) + ":projects:" + root,
		})
	}
	return WatchPlan{Roots: roots}, nil
}

func (s geminiSourceSet) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, root := range s.roots {
		if geminiProjectMetadataPath(root, req.Path) {
			return s.discoverRoot(ctx, root)
		}
		source, ok := s.sourceRef(root, req.Path)
		if ok {
			return []SourceRef{source}, nil
		}
		if jsonlMissingPathFallbackAllowed(req) {
			source, ok = s.sourceRefForPath(root, req.Path, false)
			if ok {
				return []SourceRef{source}, nil
			}
		}
	}
	return nil, nil
}

func (s geminiSourceSet) FindSource(
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
		for _, root := range s.roots {
			if source, ok := s.sourceRef(root, path); ok {
				return source, true, nil
			}
		}
	}
	if req.RawSessionID == "" {
		return SourceRef{}, false, nil
	}
	for _, root := range s.roots {
		path := s.findSourceFile(root, req.RawSessionID)
		if path == "" {
			continue
		}
		if source, ok := s.sourceRef(root, path); ok {
			return source, true, nil
		}
	}
	return SourceRef{}, false, nil
}

// findSourceFile locates a Gemini session file by its session UUID under root,
// searching all project hash directories. The session filename embeds the first
// eight characters of the UUID, so candidates are pre-filtered on that prefix
// before confirming the recorded sessionId matches.
func (s geminiSourceSet) findSourceFile(root, sessionID string) string {
	if root == "" || !IsValidSessionID(sessionID) ||
		len(sessionID) < 8 {
		return ""
	}

	tmpDir := filepath.Join(root, "tmp")
	hashDirs, err := os.ReadDir(tmpDir)
	if err != nil {
		return ""
	}

	for _, hd := range hashDirs {
		if !isDirOrSymlink(hd, tmpDir) {
			continue
		}
		chatsDir := filepath.Join(tmpDir, hd.Name(), geminiChatsDir)
		entries, err := os.ReadDir(chatsDir)
		if err != nil {
			continue
		}
		for _, sf := range entries {
			if sf.IsDir() {
				continue
			}
			name := sf.Name()
			if !isGeminiSessionFilename(name) {
				continue
			}
			if strings.Contains(name, sessionID[:8]) {
				path := filepath.Join(chatsDir, name)
				if confirmGeminiSessionID(path, sessionID) {
					return path
				}
			}
		}
	}
	return ""
}

// confirmGeminiSessionID reads the sessionId field from a Gemini file to
// confirm it matches the expected ID.
func confirmGeminiSessionID(path, sessionID string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return GeminiSessionID(data) == sessionID
}

func (s geminiSourceSet) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	if err := ctx.Err(); err != nil {
		return SourceFingerprint{}, err
	}
	root, path, ok := s.rootPathFromSource(source)
	if !ok {
		return SourceFingerprint{}, fmt.Errorf("gemini source path unavailable")
	}
	info, err := os.Stat(path)
	if err != nil {
		return SourceFingerprint{}, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.IsDir() {
		return SourceFingerprint{}, fmt.Errorf("stat %s: source is a directory", path)
	}
	fingerprint := SourceFingerprint{
		Key:     firstNonEmptyJSONLString(source.FingerprintKey, source.Key, path),
		Size:    info.Size(),
		MTimeNS: info.ModTime().UnixNano(),
	}
	h := sha256.New()
	if err := addGeminiFingerprintPart(h, "session", path, info); err != nil {
		return SourceFingerprint{}, err
	}
	if _, err := fmt.Fprintf(
		h, "project\x00%s\x00",
		s.resolvedProjectForFingerprint(root, path, source),
	); err != nil {
		return SourceFingerprint{}, err
	}
	fingerprint.Hash = fmt.Sprintf("%x", h.Sum(nil))
	return fingerprint, nil
}

// geminiSessionDirHash extracts the tmp/<dirHash>/chats component that keys
// the session's project resolution.
func geminiSessionDirHash(root, path string) (string, bool) {
	rel, ok := relUnder(filepath.Clean(root), filepath.Clean(path))
	if !ok {
		return "", false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 4 || parts[0] != "tmp" || parts[2] != geminiChatsDir {
		return "", false
	}
	return parts[1], true
}

// resolvedProjectForFingerprint returns the only metadata this session's
// parse consumes: its resolved project name. Root-wide metadata files must
// not leak into unrelated sessions' fingerprints (they used to
// mass-invalidate the whole root on any edit).
func (s geminiSourceSet) resolvedProjectForFingerprint(
	root, path string, source SourceRef,
) string {
	if source.ProjectHint != "" {
		return source.ProjectHint
	}
	dirHash, ok := geminiSessionDirHash(root, path)
	if !ok {
		return ""
	}
	return ResolveGeminiProject(dirHash, buildGeminiProjectMap(root))
}

func (s geminiSourceSet) pathFromSource(source SourceRef) (string, bool) {
	_, path, ok := s.rootPathFromSource(source)
	return path, ok
}

func (s geminiSourceSet) rootPathFromSource(source SourceRef) (string, string, bool) {
	switch src := source.Opaque.(type) {
	case geminiSource:
		return src.Root, src.Path, src.Path != ""
	case *geminiSource:
		if src != nil && src.Path != "" {
			return src.Root, src.Path, true
		}
	}
	for _, candidate := range []string{source.DisplayPath, source.FingerprintKey, source.Key} {
		for _, root := range s.roots {
			if ref, ok := s.sourceRef(root, candidate); ok {
				src := ref.Opaque.(geminiSource)
				return src.Root, src.Path, true
			}
		}
	}
	return "", "", false
}

func (s geminiSourceSet) sourceRef(root, path string) (SourceRef, bool) {
	return s.sourceRefForPath(root, path, true)
}

// SourceForReconciliation rebuilds an exact source already admitted by
// streaming discovery. The discovered project hint is authoritative here, so
// rehydration must not rebuild root-wide Gemini project metadata per source.
func (s geminiSourceSet) SourceForReconciliation(
	ctx context.Context, path, project string,
) (SourceRef, bool, error) {
	if err := ctx.Err(); err != nil {
		return SourceRef{}, false, err
	}
	for _, root := range s.roots {
		source, ok := s.sourceRefForPathWithProjectMap(
			root, path, true, map[string]string{},
		)
		if !ok {
			continue
		}
		if project != "" {
			source.ProjectHint = project
		}
		return source, true, nil
	}
	return SourceRef{}, false, nil
}

func (s geminiSourceSet) sourceRefForPath(
	root, path string,
	requireRegular bool,
) (SourceRef, bool) {
	return s.sourceRefForPathWithProjectMap(root, path, requireRegular, nil)
}

// sourceRefForPathWithProjectMap builds a SourceRef using a caller-supplied
// project map. Discovery builds the map once per root and passes it for every
// source; single-path callers pass nil and the map is built for that one path.
func (s geminiSourceSet) sourceRefForPathWithProjectMap(
	root, path string,
	requireRegular bool,
	projectMap map[string]string,
) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rel, ok := relUnder(root, path)
	if !ok || (requireRegular && !IsRegularFile(path)) {
		return SourceRef{}, false
	}
	sepParts := strings.Split(filepath.ToSlash(rel), "/")
	if len(sepParts) != 4 ||
		sepParts[0] != "tmp" ||
		sepParts[2] != geminiChatsDir ||
		!isGeminiSessionFilename(sepParts[3]) {
		return SourceRef{}, false
	}
	if projectMap == nil {
		projectMap = buildGeminiProjectMap(root)
	}
	project := ResolveGeminiProject(sepParts[1], projectMap)
	return SourceRef{
		Provider:       AgentGemini,
		Key:            path,
		DisplayPath:    path,
		FingerprintKey: path,
		ProjectHint:    project,
		Opaque: geminiSource{
			Root: root,
			Path: path,
		},
	}, true
}

// buildGeminiProjectMap indirects BuildGeminiProjectMap so discovery can build
// the project map once per root and tests can observe how often it runs.
var buildGeminiProjectMap = BuildGeminiProjectMap

// IsGeminiProjectMetadataFile reports whether path names one of the
// root-level Gemini project-metadata files whose changes fan out to every
// session under the root. The Gemini watch plan only emits these names from
// the non-recursive root watch, so a basename check is sufficient for
// callers without the root at hand.
func IsGeminiProjectMetadataFile(path string) bool {
	base := filepath.Base(path)
	return base == "projects.json" || base == "trustedFolders.json"
}

func geminiProjectMetadataPath(root, path string) bool {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rel, ok := relUnder(root, path)
	if !ok {
		return false
	}
	rel = filepath.ToSlash(rel)
	return rel == "projects.json" || rel == "trustedFolders.json"
}

func addGeminiFingerprintPart(
	h hash.Hash,
	label string,
	path string,
	info os.FileInfo,
) error {
	if _, err := fmt.Fprintf(
		h,
		"%s\x00%s\x00%d\x00%d\x00",
		label,
		path,
		info.Size(),
		info.ModTime().UnixNano(),
	); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("hash %s: %w", path, err)
	}
	return nil
}

func geminiProviderCapabilities() Capabilities {
	return Capabilities{
		Source: SourceCapabilities{
			DiscoverSources:      CapabilitySupported,
			StreamingDiscovery:   CapabilitySupported,
			WatchSources:         CapabilitySupported,
			ClassifyChangedPath:  CapabilitySupported,
			FindSource:           CapabilitySupported,
			CompositeFingerprint: CapabilitySupported,
			IncrementalAppend:    CapabilityNotApplicable,
			MultiSessionSource:   CapabilityNotApplicable,
			PerSessionErrors:     CapabilityNotApplicable,
			ExcludedSessions:     CapabilityNotApplicable,
			ForceReplaceOnParse:  CapabilityNotApplicable,
		},
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			Thinking:             CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			PerMessageTokenUsage: CapabilitySupported,
			Model:                CapabilitySupported,
		},
	}
}
