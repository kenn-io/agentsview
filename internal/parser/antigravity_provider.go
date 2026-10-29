package parser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

var _ Provider = (*antigravityProvider)(nil)

type antigravityProviderFactory struct {
	def AgentDef
}

func newAntigravityProviderFactory(def AgentDef) ProviderFactory {
	return antigravityProviderFactory{def: cloneAgentDef(def)}
}

func (f antigravityProviderFactory) Definition() AgentDef {
	return cloneAgentDef(f.def)
}

func (f antigravityProviderFactory) Capabilities() Capabilities {
	return antigravityProviderCapabilities()
}

func (f antigravityProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	cfg = cfg.Clone()
	return &antigravityProvider{
		Def:     cloneAgentDef(f.def),
		Caps:    antigravityProviderCapabilities(),
		Config:  cfg,
		sources: newAntigravitySourceSet(cfg),
	}
}

type antigravityProvider struct {
	ProviderBase
	sources antigravitySourceSet
}

func (p *antigravityProvider) Discover(ctx context.Context) ([]SourceRef, error) {
	return p.sources.Discover(ctx)
}

func (p *antigravityProvider) DiscoverEach(ctx context.Context, yield func(SourceRef) error) error {
	return p.sources.DiscoverEach(ctx, yield)
}

func (p *antigravityProvider) WatchPlan(ctx context.Context) (WatchPlan, error) {
	return p.sources.WatchPlan(ctx)
}

func (p *antigravityProvider) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	return p.sources.SourcesForChangedPath(ctx, req)
}

func (p *antigravityProvider) ChangedPathRelevance(
	ctx context.Context,
	req ChangedPathRequest,
) (ChangedPathRelevance, error) {
	if err := ctx.Err(); err != nil {
		return ChangedPathUnclassified, err
	}
	for _, root := range p.sources.roots {
		if req.WatchRoot != "" && !antigravityWatchRootMatches(root, req.WatchRoot) {
			continue
		}
		rel, ok := relUnder(root, filepath.Clean(req.Path))
		if ok && (antigravityConversationIndexPath(rel) ||
			rel == AntigravitySummariesDBName+"-shm") {
			return ChangedPathNonData, nil
		}
	}
	return ChangedPathUnclassified, nil
}

// antigravityConversationIndexPath reports whether rel names the WAL index of
// a conversation database. Every read-only open rewrites that index without
// changing session data; committed writes land in the main file or the -wal.
func antigravityConversationIndexPath(rel string) bool {
	name := filepath.Base(rel)
	return filepath.Dir(rel) == "conversations" &&
		strings.HasSuffix(name, ".db-shm") &&
		IsValidSessionID(strings.TrimSuffix(name, ".db-shm"))
}

func (p *antigravityProvider) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	req = ProviderFindRequestWithRawSessionID(p.Def, req)
	return p.sources.FindSource(ctx, req)
}

func (p *antigravityProvider) SummaryStoreExpectations(
	ctx context.Context, storePath string,
) ([]SummaryStoreExpectation, error) {
	return p.sources.summaryStoreExpectations(ctx, storePath)
}

func (p *antigravityProvider) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	return p.sources.Fingerprint(ctx, source)
}

func (p *antigravityProvider) Parse(
	ctx context.Context,
	req ParseRequest,
) (ParseOutcome, error) {
	if err := ctx.Err(); err != nil {
		return ParseOutcome{}, err
	}
	src, ok := p.sources.sourceFromRef(req.Source)
	if !ok {
		return ParseOutcome{}, errors.New("antigravity source path unavailable")
	}
	if _, err := os.Stat(src.Path); err != nil {
		if os.IsNotExist(err) {
			return ParseOutcome{
				ResultSetComplete: true,
				ForceReplace:      true,
				SkipReason:        SkipNoSession,
			}, nil
		}
		return ParseOutcome{}, fmt.Errorf("stat %s: %w", src.Path, err)
	}
	machine := firstNonEmptyJSONLString(req.Machine, p.Config.Machine)
	project := req.Source.ProjectHint
	if project != "" {
		// A remote workspace names a folder on another machine, so its
		// project comes from the path text alone, as for the CLI provider.
		projectCtx := ctx
		if req.Source.CwdResolution.State == SourceCwdRemote || p.Config.PathRewriter != nil {
			projectCtx = WithoutFilesystemProjectDiscovery(projectCtx)
		}
		if normalized := ExtractProjectFromCwdWithBranchContext(projectCtx, project, ""); normalized != "" {
			project = normalized
		}
	}
	sess, msgs, usageEvents, err := p.parseSession(ctx,
		src.Path,
		project,
		machine,
	)
	if err != nil {
		return ParseOutcome{}, err
	}
	if sess == nil {
		return ParseOutcome{
			ResultSetComplete: true,
			ForceReplace:      true,
			SkipReason:        SkipNoSession,
		}, nil
	}
	if req.Source.CwdResolution.State == SourceCwdResolved {
		sess.Cwd = req.Source.CwdResolution.Path
	}
	if parent := loadAntigravitySummaries(ctx, src.Root).parent(src.ID); parent != "" {
		sess.ParentSessionID = antigravityIDPrefix + parent
		sess.RelationshipType = RelSubagent
	}
	applyAntigravityTitle(ctx, sess, src)
	if req.Fingerprint.Hash != "" {
		sess.File.Hash = req.Fingerprint.Hash
	}
	return ParseOutcome{
		Results: []ParseResultOutcome{{
			Result: ParseResult{
				Session:     *sess,
				Messages:    msgs,
				UsageEvents: usageEvents,
			},
			DataVersion: DataVersionCurrent,
		}},
		ResultSetComplete: true,
		ForceReplace:      true,
	}, nil
}

type antigravitySource struct {
	Root string
	Path string
	ID   string
}

type antigravitySourceSet struct {
	roots       []string
	remote      bool
	remoteRoots map[string]bool
}

func newAntigravitySourceSet(cfg ProviderConfig) antigravitySourceSet {
	return antigravitySourceSet{
		roots:       cleanJSONLRoots(cfg.Roots),
		remote:      cfg.PathRewriter != nil,
		remoteRoots: antigravityRemoteRoots(cfg),
	}
}

func (s antigravitySourceSet) Discover(ctx context.Context) ([]SourceRef, error) {
	var sources []SourceRef
	seen := make(map[string]struct{})
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, path := range s.discoverSessionPaths(root) {
			source, ok := s.sourceRef(ctx, root, path, false)
			if ok {
				addJSONLSource(source, &sources, seen)
			}
		}
	}
	sortJSONLSources(sources)
	return sources, nil
}

func (s antigravitySourceSet) DiscoverEach(ctx context.Context, yield func(SourceRef) error) error {
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return err
		}
		dir := filepath.Join(root, "conversations")
		err := streamDirectoryEntries(ctx, dir, func(entry os.DirEntry) error {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".db") ||
				!IsValidSessionID(strings.TrimSuffix(name, ".db")) {
				return nil
			}
			if source, ok := s.sourceRef(ctx, root, filepath.Join(dir, name), false); ok {
				if err := yield(source); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		err = streamDirectoryEntries(ctx, filepath.Join(root, "brain"), func(entry os.DirEntry) error {
			id := entry.Name()
			if !entry.IsDir() || !IsValidSessionID(id) ||
				IsRegularFile(filepath.Join(root, "conversations", id+".db")) {
				return nil
			}
			if source, ok := s.sourceRef(ctx, root, antigravityBrainTranscriptPath(root, id), false); ok {
				return yield(source)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// discoverSessionPaths returns one path per IDE session under root, sorted by
// path: every conversations/<uuid>.db, plus the brain transcript of every
// conversation that has no database of its own. It owns the on-disk discovery
// the package-level DiscoverAntigravitySessions free function used to provide.
func (s antigravitySourceSet) discoverSessionPaths(root string) []string {
	if root == "" {
		return nil
	}
	paths := antigravityBrainTranscriptSources(root)
	dir := filepath.Join(root, "conversations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		slices.Sort(paths)
		return paths
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".db") {
			continue
		}
		id := strings.TrimSuffix(name, ".db")
		if !IsValidSessionID(id) {
			continue
		}
		paths = append(paths, filepath.Join(dir, name))
	}
	slices.Sort(paths)
	return paths
}

// findSourceFile locates the database or standalone brain transcript for a
// conversation under root. It owns the lookup the package-level
// FindAntigravitySourceFile free function used to provide.
func (s antigravitySourceSet) findSourceFile(root, id string) string {
	if root == "" || !IsValidSessionID(id) {
		return ""
	}
	for _, p := range []string{
		filepath.Join(root, "conversations", id+".db"),
		antigravityBrainTranscriptPath(root, id),
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func (s antigravitySourceSet) WatchPlan(context.Context) (WatchPlan, error) {
	roots := make([]WatchRoot, 0, len(s.roots)*4)
	for _, root := range s.roots {
		roots = append(roots,
			WatchRoot{
				Path:         filepath.Join(root, "annotations"),
				Recursive:    false,
				IncludeGlobs: []string{"*.pbtxt"},
				DebounceKey:  string(AgentAntigravity) + ":annotations:" + root,
			},
			// brain/<id>/*.md and the plaintext transcript at
			// brain/<id>/.system_generated/logs/transcript.jsonl are parsed.
			// The other generated trees (.git, scratch, and the rest of
			// .system_generated) stay unwatched so they cannot exhaust the
			// recursive-watch budget.
			WatchRoot{
				Path:      filepath.Join(root, "brain"),
				Recursive: true,
				MaxDepth:  1,
				ExtraDirectories: []string{
					"*/" + antigravityBrainGeneratedDir + "/" + antigravityBrainLogsDir,
				},
				IncludeGlobs: []string{
					"*.md",
					"*.md.metadata.json",
					antigravityBrainTranscriptName,
				},
				DebounceKey: string(AgentAntigravity) + ":brain:" + root,
			},
			WatchRoot{
				Path:         filepath.Join(root, "conversations"),
				Recursive:    false,
				IncludeGlobs: []string{"*.db", "*.db-wal", "*.trajectory.json"},
				DebounceKey:  string(AgentAntigravity) + ":conversations:" + root,
			},
			WatchRoot{
				Path:         root,
				Recursive:    false,
				IncludeGlobs: []string{AntigravitySummariesDBName, AntigravitySummariesDBName + "-wal"},
				DebounceKey:  string(AgentAntigravity) + ":titles:" + root,
			},
		)
	}
	return WatchPlan{Roots: roots}, nil
}

func (s antigravitySourceSet) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, root := range s.roots {
		if req.WatchRoot != "" && !antigravityWatchRootMatches(root, req.WatchRoot) {
			continue
		}
		if antigravitySummariesPath(root, req.Path) {
			// The engine routes store writes through SummaryStoreExpectations,
			// and titles reconcile separately.
			return nil, nil
		}
		source, ok := s.sourceForChangedPath(ctx, root, req.Path)
		if ok {
			return []SourceRef{source}, nil
		}
	}
	return nil, nil
}

func (s antigravitySourceSet) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	if err := ctx.Err(); err != nil {
		return SourceRef{}, false, err
	}
	freshStoredSource := req.RequireFreshSource &&
		(req.StoredFilePath != "" || req.FingerprintKey != "")
	for _, path := range []string{req.StoredFilePath, req.FingerprintKey} {
		if path == "" {
			continue
		}
		for _, root := range s.roots {
			if source, ok := s.sourceRef(ctx, root, path, true); ok {
				src := source.Opaque.(antigravitySource)
				if req.RawSessionID != "" && src.ID != req.RawSessionID {
					continue
				}
				if req.RequireFreshSource && !IsRegularFile(src.Path) {
					continue
				}
				return source, true, nil
			}
		}
	}
	if freshStoredSource {
		return SourceRef{}, false, nil
	}
	if req.RawSessionID == "" {
		return SourceRef{}, false, nil
	}
	for _, root := range s.roots {
		path := s.findSourceFile(root, req.RawSessionID)
		if path == "" {
			continue
		}
		if source, ok := s.sourceRef(ctx, root, path, false); ok {
			return source, true, nil
		}
	}
	return SourceRef{}, false, nil
}

func (s antigravitySourceSet) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	if err := ctx.Err(); err != nil {
		return SourceFingerprint{}, err
	}
	src, ok := s.sourceFromRef(source)
	if !ok {
		return SourceFingerprint{}, errors.New("antigravity source path unavailable")
	}
	key := firstNonEmptyJSONLString(source.FingerprintKey, source.Key, src.Path)
	info, err := AntigravityFileInfo(src.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return SourceFingerprint{Key: key}, nil
		}
		return SourceFingerprint{}, err
	}
	summaries := loadAntigravitySummaries(ctx, src.Root)
	if summaries.err != nil {
		// Without a good read the stored workspace and parent cannot be
		// checked, so the session stays as archived and is retried.
		return SourceFingerprint{}, fmt.Errorf("read %s: %w", AntigravitySummariesDBName, summaries.err)
	}
	hash, err := antigravityCompositeHashWithExtra(
		src.Path,
		antigravityIDECompanionPaths(src.Path),
		func(h interface{ Write([]byte) (int, error) }) error {
			return summaries.writeFingerprint(h, src.ID)
		},
	)
	if err != nil {
		return SourceFingerprint{}, err
	}
	return SourceFingerprint{
		Key:     key,
		Size:    info.Size(),
		MTimeNS: info.ModTime().UnixNano(),
		Hash:    hash,
	}, nil
}

func (s antigravitySourceSet) sourceFromRef(source SourceRef) (antigravitySource, bool) {
	switch src := source.Opaque.(type) {
	case antigravitySource:
		return src, src.Path != ""
	case *antigravitySource:
		if src != nil && src.Path != "" {
			return *src, true
		}
	}
	for _, candidate := range []string{source.DisplayPath, source.FingerprintKey, source.Key} {
		for _, root := range s.roots {
			if src, ok := s.resolveSource(root, candidate, true); ok {
				return src, true
			}
		}
	}
	return antigravitySource{}, false
}

func (s antigravitySourceSet) sourceForChangedPath(
	ctx context.Context, root, path string,
) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if dbPath, _, ok := antigravityConversationDBForPath(root, path); ok {
		return s.sourceRef(ctx, root, dbPath, true)
	}
	if transcriptRoot, _, ok := antigravityBrainTranscriptConversation(path); ok &&
		samePath(transcriptRoot, root) {
		return s.sourceRef(ctx, root, path, true)
	}
	for _, sidecarID := range []func(string, string) (string, bool){
		antigravityIDETrajectoryID, antigravityAnnotationID, antigravityBrainID,
	} {
		if id, ok := sidecarID(root, path); ok {
			dbPath := filepath.Join(root, "conversations", id+".db")
			if !IsRegularFile(dbPath) {
				return SourceRef{}, false
			}
			return s.newSourceRef(ctx, antigravitySource{Root: root, Path: dbPath, ID: id}), true
		}
	}
	return SourceRef{}, false
}

func (s antigravitySourceSet) sourceRef(
	ctx context.Context,
	root, path string,
	allowMissing bool,
) (SourceRef, bool) {
	src, ok := s.resolveSource(root, path, allowMissing)
	if !ok {
		return SourceRef{}, false
	}
	return s.newSourceRef(ctx, src), true
}

// resolveSource finds the conversation a session path belongs to and the
// file that currently holds it.
func (s antigravitySourceSet) resolveSource(
	root, path string,
	allowMissing bool,
) (antigravitySource, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if transcriptRoot, id, ok := antigravityBrainTranscriptConversation(path); ok &&
		samePath(transcriptRoot, root) {
		// A conversation that also has a database is that database's session;
		// the transcript joins it as a companion instead of standing alone.
		dbPath := filepath.Join(root, "conversations", id+".db")
		if IsRegularFile(dbPath) {
			return antigravitySource{Root: root, Path: dbPath, ID: id}, true
		}
		if !allowMissing && !IsRegularFile(path) {
			return antigravitySource{}, false
		}
		return antigravitySource{Root: root, Path: path, ID: id}, true
	}
	dbPath, id, ok := antigravityConversationDBForPath(root, path)
	if !ok || dbPath != path {
		return antigravitySource{}, false
	}
	// A removed database hands the same conversation back to its transcript.
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		transcript := antigravityBrainTranscriptPath(root, id)
		if IsRegularFile(transcript) {
			return antigravitySource{Root: root, Path: transcript, ID: id}, true
		}
	}
	if !allowMissing && !IsRegularFile(path) {
		return antigravitySource{}, false
	}
	return antigravitySource{Root: root, Path: path, ID: id}, true
}

// summaryStoreExpectations lists the session of every conversation the store
// at storePath names, with the values a parse would store.
func (s antigravitySourceSet) summaryStoreExpectations(
	ctx context.Context, storePath string,
) ([]SummaryStoreExpectation, error) {
	var expectations []SummaryStoreExpectation
	for _, root := range s.roots {
		if !antigravitySummariesPath(root, storePath) {
			continue
		}
		summaries := loadAntigravitySummaries(ctx, root)
		if summaries.err != nil {
			return nil, summaries.err
		}
		for id := range summaries.byID {
			src, ok := s.resolveSource(root, s.findSourceFile(root, id), false)
			if !ok {
				continue
			}
			parent := ""
			if p := summaries.parent(id); p != "" {
				parent = antigravityIDPrefix + p
			}
			// Only the store names an app or IDE parent.
			expectations = append(expectations, summaryExpectation(
				s.newSourceRef(ctx, src), antigravityIDPrefix+id, parent, true,
			))
		}
	}
	return expectations, ctx.Err()
}

// newSourceRef builds the source for one conversation. Its working directory
// comes from the workspace the summary store records. A remote root keeps
// that workspace only as a project hint, because it names a folder on
// another machine.
func (s antigravitySourceSet) newSourceRef(ctx context.Context, src antigravitySource) SourceRef {
	summaries := loadAntigravitySummaries(ctx, src.Root)
	resolution := summaries.cwdResolution(src.ID)
	project := resolution.Path
	if s.remote || antigravityRemoteRoot(s.remoteRoots, src.Root) {
		project = summaries.byID[src.ID].workspace
		resolution = SourceCwdResolution{State: SourceCwdRemote}
		if summaries.err != nil || summaries.missing {
			// A remote cwd stays empty either way, but its project and parent
			// also came from the store, so they must not be cleared.
			resolution = SourceCwdResolution{State: SourceCwdUnavailable}
		}
	}
	return SourceRef{
		Provider:       AgentAntigravity,
		Key:            src.Path,
		DisplayPath:    src.Path,
		FingerprintKey: src.Path,
		ProjectHint:    project,
		CwdResolution:  resolution,
		Opaque:         src,
	}
}

func antigravityConversationDBForPath(root, path string) (string, string, bool) {
	rel, ok := relUnder(filepath.Clean(root), filepath.Clean(path))
	if !ok {
		return "", "", false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 2 || parts[0] != "conversations" {
		return "", "", false
	}
	// A bare "-shm" event never resolves to the session: every parse's
	// read-only open rewrites that index, so honoring it would make each
	// parse schedule the next one. Committed writes land in the main file
	// or the -wal.
	name := strings.TrimSuffix(parts[1], "-wal")
	if !strings.HasSuffix(name, ".db") {
		return "", "", false
	}
	id := strings.TrimSuffix(name, ".db")
	if !IsValidSessionID(id) {
		return "", "", false
	}
	return filepath.Join(root, "conversations", id+".db"), id, true
}

// antigravityIDETrajectoryID reports whether path is a
// conversations/<id>.trajectory.json agy-reader sidecar under root and,
// if so, returns the session id so a sidecar write routes back to its
// .db source for re-sync.
func antigravityIDETrajectoryID(root, path string) (string, bool) {
	rel, ok := relUnder(filepath.Clean(root), filepath.Clean(path))
	if !ok {
		return "", false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 2 || parts[0] != "conversations" ||
		!strings.HasSuffix(parts[1], ".trajectory.json") {
		return "", false
	}
	id := strings.TrimSuffix(parts[1], ".trajectory.json")
	return id, IsValidSessionID(id)
}

func antigravityAnnotationID(root, path string) (string, bool) {
	rel, ok := relUnder(filepath.Clean(root), filepath.Clean(path))
	if !ok {
		return "", false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 2 || parts[0] != "annotations" ||
		!strings.HasSuffix(parts[1], ".pbtxt") {
		return "", false
	}
	id := strings.TrimSuffix(parts[1], ".pbtxt")
	return id, IsValidSessionID(id)
}

func antigravityBrainID(root, path string) (string, bool) {
	rel, ok := relUnder(filepath.Clean(root), filepath.Clean(path))
	if !ok {
		return "", false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 3 || parts[0] != "brain" {
		return "", false
	}
	return parts[1], IsValidSessionID(parts[1])
}

func antigravityWatchRootMatches(root, watchRoot string) bool {
	watchRoot = filepath.Clean(watchRoot)
	for _, subdir := range []string{"annotations", "brain", "conversations"} {
		if samePath(watchRoot, filepath.Join(root, subdir)) {
			return true
		}
	}
	return samePath(watchRoot, filepath.Clean(root))
}

func antigravityProviderCapabilities() Capabilities {
	source := jsonlFileProviderSourceCapabilities()
	source.StreamingDiscovery = CapabilitySupported
	source.ForceReplaceOnParse = CapabilitySupported
	source.ChangedPathRelevance = CapabilitySupported
	return Capabilities{
		Source: source,
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			SessionName:          CapabilitySupported,
			Cwd:                  CapabilitySupported,
			Relationships:        CapabilitySupported,
			Thinking:             CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			PerMessageTokenUsage: CapabilitySupported,
			AggregateUsageEvents: CapabilitySupported,
			Model:                CapabilitySupported,
		},
		// The fingerprint covers the summary store's workspace and parent,
		// which change without touching the conversation's own files. A
		// store that is missing or unreadable leaves archived sessions as
		// they are, since a reparse would clear both.
		Sync: ProviderSyncSemantics{
			FingerprintHashInCacheKey:           true,
			FingerprintHashRequiredForFreshness: true,
			UnavailableCwdKeepsArchivedSession:  true,
		},
	}
}
