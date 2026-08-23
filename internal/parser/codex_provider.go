package parser

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

var _ Provider = (*codexProvider)(nil)
var _ ActivityHintProvider = (*codexProvider)(nil)
var _ S3Provider = (*codexProvider)(nil)

// codexProviderSpec parameterizes the one shared Codex-format provider
// implementation for Codex and its TraeX fork. Both reuse the same
// discovery, source-lookup, fingerprinting, and parsing code; they differ
// only in the agent label and ID prefix applied via relabel. TraeX parses
// through the Codex rollout reader and then relabels the result onto its
// own agent and ID prefix.
type codexProviderSpec struct {
	agent AgentType
	// relabel rewrites a parsed Codex-format result onto this agent's
	// identity, and is nil for Codex itself. The session is nil on the
	// incremental path, which keeps the stored session ID and only needs
	// the appended message rows relabeled.
	relabel func(*ParsedSession, []ParsedMessage)
}

func codexProviderSpecForAgent(agent AgentType) codexProviderSpec {
	switch agent {
	case AgentTraeX:
		return codexProviderSpec{
			agent:   AgentTraeX,
			relabel: relabelCodexResultAsTraeX,
		}
	default:
		return codexProviderSpec{agent: AgentCodex}
	}
}

type codexProviderFactory struct {
	def             AgentDef
	spec            codexProviderSpec
	cursorCache     *codexCursorCache
	parentTurnCache *codexParentTurnCache
}

func newCodexProviderFactory(def AgentDef) ProviderFactory {
	return &codexProviderFactory{
		def:             cloneAgentDef(def),
		spec:            codexProviderSpecForAgent(AgentCodex),
		cursorCache:     newProductionCodexCursorCache(),
		parentTurnCache: newCodexProductionParentTurnCache(),
	}
}

// newTraeXProviderFactory serves TRAE CLI's rollout archive with the Codex
// provider, relabeling every parsed session onto the traex: ID prefix.
func newTraeXProviderFactory(def AgentDef) ProviderFactory {
	return &codexProviderFactory{
		def:             cloneAgentDef(def),
		spec:            codexProviderSpecForAgent(AgentTraeX),
		cursorCache:     newProductionCodexCursorCache(),
		parentTurnCache: newCodexProductionParentTurnCache(),
	}
}

func (f *codexProviderFactory) Definition() AgentDef {
	return cloneAgentDef(f.def)
}

func (f *codexProviderFactory) Capabilities() Capabilities {
	caps := codexProviderCapabilities()
	if f.spec.agent == AgentCodex {
		caps.Source.S3Discovery = CapabilitySupported
	}
	return caps
}

func (f *codexProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	cfg = cfg.Clone()
	return &codexProvider{
		Def:             cloneAgentDef(f.def),
		Caps:            f.Capabilities(),
		Config:          cfg,
		spec:            f.spec,
		sources:         newCodexSourceSet(f.spec.agent, cfg.Roots),
		cursorCache:     f.cursorCache,
		parentTurnCache: f.parentTurnCache,
	}
}

type codexProvider struct {
	ProviderBase
	spec            codexProviderSpec
	sources         codexSourceSet
	cursorCache     *codexCursorCache
	parentTurnCache *codexParentTurnCache
}

func (p *codexProvider) Discover(ctx context.Context) ([]SourceRef, error) {
	return p.sources.Discover(ctx)
}

func (p *codexProvider) DiscoverEach(ctx context.Context, yield func(SourceRef) error) error {
	return p.sources.DiscoverEach(ctx, yield)
}

func (p *codexProvider) WatchPlan(ctx context.Context) (WatchPlan, error) {
	return p.sources.WatchPlan(ctx)
}

func (p *codexProvider) ActivityHintSources(
	ctx context.Context,
) ([]ActivityHintSource, error) {
	seen := make(map[string]struct{}, len(p.sources.roots))
	sources := make([]ActivityHintSource, 0, len(p.sources.roots))
	for _, root := range p.sources.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if strings.HasPrefix(root, "s3://") {
			continue
		}
		path := filepath.Join(filepath.Dir(filepath.Clean(root)), "history.jsonl")
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		sources = append(sources, ActivityHintSource{Path: path})
	}
	return sources, nil
}

func (p *codexProvider) DecodeActivityHint(line []byte) (ActivityHint, bool) {
	var row struct {
		SessionID string `json:"session_id"`
		Timestamp int64  `json:"ts"`
	}
	if json.Unmarshal(line, &row) != nil ||
		!isCodexHistorySessionID(row.SessionID) ||
		row.Timestamp <= 0 {
		return ActivityHint{}, false
	}
	return ActivityHint{
		RawSessionID: row.SessionID,
		Timestamp:    time.Unix(row.Timestamp, 0).UTC(),
	}, true
}

func isCodexHistorySessionID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, c := range id {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') &&
				(c < 'a' || c > 'f') &&
				(c < 'A' || c > 'F') {
				return false
			}
		}
	}
	return true
}

func (p *codexProvider) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	return p.sources.SourcesForChangedPath(ctx, req)
}

func (p *codexProvider) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	req = ProviderFindRequestWithRawSessionID(p.Def, req)
	return p.sources.FindSource(ctx, req)
}

// AllSourcePathsForUUID returns every on-disk Codex transcript path under the
// provider's roots whose filename carries the given session UUID, without the
// live-over-archived deduplication Discover applies. A UUID can exist as both a
// live dated copy and a flat archived copy under the same root; the sync engine
// uses the full set so an mtime cutoff can judge each copy independently.
func (p *codexProvider) AllSourcePathsForUUID(uuid string) []string {
	if uuid == "" {
		return nil
	}
	seen := make(map[string]struct{})
	var paths []string
	for _, root := range p.sources.roots {
		for _, path := range p.sources.discoverSessionPaths(root) {
			if CodexSessionUUIDFromFilename(filepath.Base(path)) != uuid {
				continue
			}
			clean := filepath.Clean(path)
			if _, ok := seen[clean]; ok {
				continue
			}
			seen[clean] = struct{}{}
			paths = append(paths, path)
		}
	}
	return paths
}

// SourceRefForPath builds a SourceRef pinned to the exact transcript path,
// without live-over-archived canonicalization. Discovery, raw-ID lookup, and
// fresh stored-source lookup still prefer the live dated transcript, but
// changed-path events and non-fresh stored paths preserve the already-selected
// on-disk copy. The sync engine uses this when its DB-aware or mtime-aware
// logic has chosen a duplicated Codex UUID source (e.g. a stored archived copy
// or the cutoff-newer copy), so that choice is honored instead of being flipped
// back to the preferred dated layout. Returns false when the path is not a
// recognizable Codex source.
func (p *codexProvider) SourceRefForPath(path string) (SourceRef, bool) {
	for _, root := range p.sources.roots {
		if source, ok := p.sources.sourceRef(root, path, true); ok {
			return source, true
		}
		if source, ok := p.sources.directPathSource(root, path, true); ok {
			return source, true
		}
	}
	return SourceRef{}, false
}

func (p *codexProvider) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	return p.sources.Fingerprint(ctx, source)
}

// ComputeMultiFileStatHash implements parser.MultiFileStatHasher over the
// rollout transcript plus its session_index.jsonl sidecar, mirroring the
// sidecar folding of the verified-source gate: an index-only change (a
// thread title rename) breaks the digest even when the transcript is
// byte-identical, so the warm short-circuit can never mask a metadata
// refresh. An absent index contributes a stable (0, 0, 0) tuple, which
// matches modern Codex releases that no longer write the index. The
// digest persists stat-verified freshness in provider_freshness across
// process restarts, sparing a fresh engine the full-content hash that
// Fingerprint performs for every unchanged rollout.
func (p *codexProvider) ComputeMultiFileStatHash(chatPath string) uint64 {
	return fileStatTupleDigest(0xC2, chatPath, codexSessionIndexPath(chatPath))
}

func (p *codexProvider) Parse(
	ctx context.Context,
	req ParseRequest,
) (ParseOutcome, error) {
	if err := ctx.Err(); err != nil {
		return ParseOutcome{}, err
	}
	path, ok := p.sources.pathFromSource(req.Source)
	if !ok {
		return ParseOutcome{}, fmt.Errorf("codex source path unavailable")
	}
	if req.ForceParse && p.spec.agent == AgentCodex {
		EvictCodexSessionIndexForSession(path)
	}
	machine := firstNonEmptyJSONLString(req.Machine, p.Config.Machine)
	parentID, parentResolved := p.codexParentResolution(ctx, path)
	sess, msgs, err := p.parseSessionContext(ctx, path, machine, false)
	if err != nil {
		return ParseOutcome{}, err
	}
	if sess == nil {
		return ParseOutcome{
			ResultSetComplete: true,
			SkipReason:        SkipNoSession,
		}, nil
	}
	if p.spec.relabel != nil {
		p.spec.relabel(sess, msgs)
	}
	if req.Fingerprint.Hash != "" {
		sess.File.Hash = req.Fingerprint.Hash
	}
	result := ParseResultOutcome{
		Result: ParseResult{
			Session:  *sess,
			Messages: msgs,
		},
		DataVersion: DataVersionCurrent,
	}
	if parentID != "" && !parentResolved {
		result.DataVersion = DataVersionNeedsRetry
		result.RetryReason = "codex parent turns unresolved for " + parentID
	}
	return ParseOutcome{
		Results:           []ParseResultOutcome{result},
		ResultSetComplete: true,
		// A requested full parse is the authoritative message set, so
		// force-replace the stored rows; this remains distinct from the provider's
		// incremental append path and preserves the legacy behavior where a late
		// token_count line appended to
		// an existing turn rewrites the stored message instead of being dropped by
		// an append-only write.
		ForceReplace: true,
	}, nil
}

func (p *codexProvider) ParseIncremental(
	ctx context.Context,
	req IncrementalRequest,
) (IncrementalOutcome, IncrementalStatus, error) {
	if err := ctx.Err(); err != nil {
		return IncrementalOutcome{}, IncrementalUnsupported, err
	}
	path, ok := p.sources.pathFromSource(req.Source)
	if !ok {
		return IncrementalOutcome{}, IncrementalUnsupported,
			fmt.Errorf("codex source path unavailable")
	}
	if req.Offset < 0 || req.Fingerprint.Size < req.Offset {
		return IncrementalOutcome{ForceReplace: true},
			IncrementalNeedsFullParse, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return IncrementalOutcome{}, IncrementalNeedsFullParse, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return IncrementalOutcome{}, IncrementalNeedsFullParse, err
	}
	inode, device := sourceFileIdentity(info)
	if (req.Fingerprint.Inode != 0 && req.Fingerprint.Inode != inode) ||
		(req.Fingerprint.Device != 0 && req.Fingerprint.Device != device) ||
		info.Size() < req.Fingerprint.Size {
		return IncrementalOutcome{ForceReplace: true},
			IncrementalNeedsFullParse, nil
	}
	safe, err := codexSafeResumeOffsetFile(f, req.Offset)
	if err != nil {
		return IncrementalOutcome{}, IncrementalNeedsFullParse, err
	}
	if !safe {
		return IncrementalOutcome{ForceReplace: true},
			IncrementalNeedsFullParse, nil
	}
	if req.Fingerprint.Size == req.Offset {
		return IncrementalOutcome{}, IncrementalNoNewData, nil
	}

	result, err := p.parseSessionFromSnapshot(
		path,
		req.Offset,
		req.StartOrdinal,
		false,
		f,
		info,
		req.Fingerprint.Size,
	)
	if err != nil {
		if IsIncrementalFullParseFallback(err) {
			return IncrementalOutcome{ForceReplace: true},
				IncrementalNeedsFullParse, nil
		}
		return IncrementalOutcome{}, IncrementalNeedsFullParse, err
	}
	if !result.initialCursor.firstUserSeen && result.cursor.firstUserSeen {
		// The stored session has no genuine first prompt yet, so appending this
		// tail would leave its persisted FirstMessage preview stale. A full parse
		// can rebuild both the messages and the session summary atomically.
		return IncrementalOutcome{ForceReplace: true},
			IncrementalNeedsFullParse, nil
	}
	p.cursorCache.Put(
		path,
		req.Offset,
		result.inode,
		result.device,
		result.initialCursor,
	)
	if result.consumedBytes == 0 {
		return IncrementalOutcome{}, IncrementalNoNewData, nil
	}
	p.cursorCache.Put(
		path,
		req.Offset+result.consumedBytes,
		result.inode,
		result.device,
		result.cursor,
	)

	if p.spec.relabel != nil {
		p.spec.relabel(nil, result.messages)
	}

	totalOut, peakCtx, hasTotalOut, hasPeakCtx :=
		codexProviderTokenTotals(result.messages)
	termination := codexIncrementalTermination(result.cursor.lastTaskEvent)
	return IncrementalOutcome{
		SessionID:            req.SessionID,
		Messages:             result.messages,
		EndedAt:              result.endedAt,
		ConsumedBytes:        result.consumedBytes,
		MessageCount:         len(result.messages),
		UserMessageCount:     codexProviderUserMessageCount(result.messages),
		TotalOutputTokens:    totalOut,
		PeakContextTokens:    peakCtx,
		HasTotalOutputTokens: hasTotalOut,
		HasPeakContextTokens: hasPeakCtx,
		TerminationStatus:    termination,
	}, IncrementalApplied, nil
}

type codexSource struct {
	Root   string
	Path   string
	UUID   string
	Layout CodexLayout
}

type codexSourceSet struct {
	// agent labels the sources this set emits. Codex-format forks share
	// the layout but must not share a discovery namespace: keying sources
	// by agent keeps a TraeX UUID from colliding with a Codex one.
	agent AgentType
	roots []string
}

func newCodexSourceSet(agent AgentType, roots []string) codexSourceSet {
	if agent == "" {
		agent = AgentCodex
	}
	return codexSourceSet{agent: agent, roots: cleanJSONLRoots(roots)}
}

// ownsCodexSidecars reports whether this source set's agent is the one that
// owns Codex's out-of-band files: the session_index.jsonl sidecar and the
// s3://.../raw/codex/... archive layout. Only Codex does. A fork writes
// neither, so it must not watch, fan out on, or import them -- importing an
// s3:// root through the Codex S3 scanner would stamp AgentCodex and silently
// move the sessions into Codex's identity namespace.
func (s codexSourceSet) ownsCodexSidecars() bool {
	return s.agent == AgentCodex
}

func (s codexSourceSet) Discover(ctx context.Context) ([]SourceRef, error) {
	return s.discover(ctx, func(string) bool { return true })
}

func (s codexSourceSet) DiscoverEach(
	ctx context.Context, yield func(SourceRef) error,
) error {
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return err
		}
		if strings.HasPrefix(root, "s3://") {
			if !s.ownsCodexSidecars() {
				continue
			}
			for _, file := range s3PrefixScan(root, codexS3Scanner()) {
				if err := yield(s3SourceRefFromDiscoveredFile(root, file)); err != nil {
					return err
				}
			}
			continue
		}
		err := streamDirectoryTree(ctx, root, func(path string, entry os.DirEntry) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !isCodexSessionFilename(entry.Name()) {
				return nil
			}
			source, ok := s.sourceRef(root, path, true)
			if !ok {
				if _, _, supported := CodexSessionPathInfo(root, path); supported {
					source, ok = s.directPathSource(root, path, true)
				}
			}
			if !ok {
				return nil
			}
			return yield(source)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (s codexSourceSet) discover(
	ctx context.Context,
	includeRoot func(string) bool,
) ([]SourceRef, error) {
	var sources []SourceRef
	byKey := make(map[string]SourceRef)
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !includeRoot(root) {
			continue
		}
		if strings.HasPrefix(root, "s3://") {
			// s3:// roots have no local layout to walk: enumerate the objects
			// directly and carry each one's durable metadata in the Opaque
			// payload. Each object is its own session keyed by URI, so the
			// live-over-archived preference (which inspects a local codexSource
			// layout) does not apply here.
			if !s.ownsCodexSidecars() {
				continue
			}
			for _, file := range s3PrefixScan(root, codexS3Scanner()) {
				source := s3SourceRefFromDiscoveredFile(root, file)
				if _, ok := byKey[source.Key]; ok {
					continue
				}
				byKey[source.Key] = source
			}
			continue
		}
		for _, path := range s.discoverSessionPaths(root) {
			source, ok := s.sourceRef(root, path, true)
			if !ok {
				source, ok = s.directPathSource(root, path, true)
			}
			if !ok {
				continue
			}
			if current, ok := byKey[source.Key]; ok &&
				!preferCodexSource(source, current) {
				continue
			}
			byKey[source.Key] = source
		}
	}
	for _, source := range byKey {
		sources = append(sources, source)
	}
	sortJSONLSources(sources)
	return sources, nil
}

// discoverSessionPaths finds all Codex JSONL session file paths under
// sessionsDir, covering both the standard year/month/day layout and a flat
// archived directory. Paths are returned sorted for deterministic discovery,
// matching the behavior the package-level entrypoint provided before the fold.
func (s codexSourceSet) discoverSessionPaths(sessionsDir string) []string {
	var paths []string

	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !isCodexSessionFilename(entry.Name()) {
			continue
		}
		paths = append(paths, filepath.Join(sessionsDir, entry.Name()))
	}

	walkCodexDayDirs(sessionsDir, func(dayPath string) bool {
		dayEntries, err := os.ReadDir(dayPath)
		if err != nil {
			return true
		}
		for _, sf := range dayEntries {
			if sf.IsDir() {
				continue
			}
			if !isCodexSessionFilename(sf.Name()) {
				continue
			}
			paths = append(paths, filepath.Join(dayPath, sf.Name()))
		}
		return true
	})

	slices.Sort(paths)
	return paths
}

// findSourceFile resolves a Codex session file by UUID under sessionsDir.
// It prefers the standard year/month/day live path when present, then falls
// back to a flat archived directory entry, matching the lookup precedence the
// package-level entrypoint provided before the fold.
func (s codexSourceSet) findSourceFile(sessionsDir, sessionID string) string {
	if !IsValidSessionID(sessionID) {
		return ""
	}

	var archived string
	entries, err := os.ReadDir(sessionsDir)
	if err == nil {
		for _, f := range entries {
			if f.IsDir() {
				continue
			}
			name := f.Name()
			if !isCodexSessionFilename(name) {
				continue
			}
			if extractUUIDFromRollout(name) == sessionID {
				archived = filepath.Join(sessionsDir, name)
				break
			}
		}
	}

	var live string
	walkCodexDayDirs(sessionsDir, func(dayPath string) bool {
		if live != "" {
			return false
		}
		dayEntries, err := os.ReadDir(dayPath)
		if err != nil {
			return true
		}
		for _, f := range dayEntries {
			if f.IsDir() {
				continue
			}
			name := f.Name()
			if !isCodexSessionFilename(name) {
				continue
			}
			if extractUUIDFromRollout(name) == sessionID {
				live = filepath.Join(dayPath, name)
				return false
			}
		}
		return true
	})
	if live != "" {
		return live
	}
	return archived
}

func (s codexSourceSet) WatchPlan(context.Context) (WatchPlan, error) {
	roots := make([]WatchRoot, 0, len(s.roots)*2)
	seenShallow := make(map[string]struct{})
	for _, root := range s.roots {
		if isS3URI(root) {
			continue
		}
		roots = append(roots, WatchRoot{
			Path:         root,
			Recursive:    true,
			IncludeGlobs: []string{"*.jsonl"},
			DebounceKey:  string(s.agent) + ":sessions:" + root,
		})
		if !s.ownsCodexSidecars() {
			continue
		}
		for _, shallow := range ResolveCodexShallowWatchRoots(root) {
			shallow = filepath.Clean(shallow)
			if _, ok := seenShallow[shallow]; ok {
				continue
			}
			seenShallow[shallow] = struct{}{}
			roots = append(roots, WatchRoot{
				Path:         shallow,
				Recursive:    false,
				IncludeGlobs: []string{CodexSessionIndexFilename},
				DebounceKey:  string(s.agent) + ":index:" + shallow,
			})
		}
	}
	return WatchPlan{Roots: roots}, nil
}

func (s codexSourceSet) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.ownsCodexSidecars() &&
		filepath.Base(req.Path) == CodexSessionIndexFilename {
		return s.sourcesForIndexPath(ctx, req.Path)
	}
	for _, root := range s.roots {
		source, ok := s.sourceRef(root, req.Path, true)
		if !ok {
			source, ok = s.directPathSource(root, req.Path, true)
		}
		if ok {
			return []SourceRef{source}, nil
		}
		if !jsonlMissingPathFallbackAllowed(req) {
			continue
		}
		source, ok = s.sourceRef(root, req.Path, false)
		if !ok {
			source, ok = s.directPathSource(root, req.Path, false)
		}
		if ok {
			return []SourceRef{source}, nil
		}
	}
	return nil, nil
}

func (s codexSourceSet) FindSource(
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
			if source, ok := s.sourceRef(root, path, true); ok {
				if !req.RequireFreshSource || req.PreferStoredSource {
					return source, true, nil
				}
				return s.canonicalSource(ctx, source)
			}
			if source, ok := s.directPathSource(root, path, true); ok {
				if !req.RequireFreshSource || req.PreferStoredSource {
					return source, true, nil
				}
				return s.canonicalSource(ctx, source)
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
		if source, ok := s.sourceRef(root, path, true); ok {
			return s.canonicalSource(ctx, source)
		}
	}
	return SourceRef{}, false, nil
}

func (s codexSourceSet) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	if err := ctx.Err(); err != nil {
		return SourceFingerprint{}, err
	}
	path, ok := s.pathFromSource(source)
	if !ok {
		return SourceFingerprint{}, fmt.Errorf("codex source path unavailable")
	}
	info, err := os.Stat(path)
	if err != nil {
		return SourceFingerprint{}, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.IsDir() {
		return SourceFingerprint{}, fmt.Errorf("stat %s: source is a directory", path)
	}
	hash, err := hashJSONLSourceFileContext(ctx, path)
	if err != nil {
		return SourceFingerprint{}, err
	}
	inode, device := sourceFileIdentity(info)
	mtime := info.ModTime().UnixNano()
	if s.agent == AgentCodex {
		mtime = CodexEffectiveMtime(path, mtime)
	}
	return SourceFingerprint{
		Key:     firstNonEmptyJSONLString(source.FingerprintKey, source.Key, path),
		Size:    info.Size(),
		MTimeNS: mtime,
		Inode:   inode,
		Device:  device,
		Hash:    hash,
	}, nil
}

func (s codexSourceSet) pathFromSource(source SourceRef) (string, bool) {
	switch src := source.Opaque.(type) {
	case codexSource:
		return src.Path, src.Path != ""
	case *codexSource:
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
		for _, root := range s.roots {
			if ref, ok := s.sourceRef(root, candidate, true); ok {
				src := ref.Opaque.(codexSource)
				return src.Path, true
			}
			if ref, ok := s.directPathSource(root, candidate, true); ok {
				src := ref.Opaque.(codexSource)
				return src.Path, true
			}
		}
	}
	return "", false
}

func (s codexSourceSet) sourcesForIndexPath(
	ctx context.Context,
	indexPath string,
) ([]SourceRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	indexDir := filepath.Dir(indexPath)
	return s.discover(ctx, func(root string) bool {
		return filepath.Dir(root) == indexDir
	})
}

func (s codexSourceSet) sourceRef(
	root string,
	path string,
	requireRegular bool,
) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	layout, uuid, ok := CodexSessionPathInfo(root, path)
	if !ok || uuid == "" {
		return SourceRef{}, false
	}
	if requireRegular && !IsRegularFile(path) {
		return SourceRef{}, false
	}
	return SourceRef{
		Provider:       s.agent,
		Key:            codexSourceKey(s.agent, uuid),
		DisplayPath:    path,
		FingerprintKey: path,
		Opaque: codexSource{
			Root:   root,
			Path:   path,
			UUID:   uuid,
			Layout: layout,
		},
	}, true
}

func (s codexSourceSet) directPathSource(
	root string,
	path string,
	requireRegular bool,
) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if !strings.HasSuffix(path, ".jsonl") || !pathUnderRoot(root, path) {
		return SourceRef{}, false
	}
	if requireRegular && !IsRegularFile(path) {
		return SourceRef{}, false
	}
	return SourceRef{
		Provider:       s.agent,
		Key:            path,
		DisplayPath:    path,
		FingerprintKey: path,
		Opaque: codexSource{
			Root: root,
			Path: path,
		},
	}, true
}

func (s codexSourceSet) canonicalSource(
	ctx context.Context,
	source SourceRef,
) (SourceRef, bool, error) {
	src, ok := source.Opaque.(codexSource)
	if !ok || src.UUID == "" {
		return source, true, nil
	}
	best := source
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return SourceRef{}, false, err
		}
		path := s.findSourceFile(root, src.UUID)
		if path == "" {
			continue
		}
		candidate, ok := s.sourceRef(root, path, true)
		if !ok {
			continue
		}
		if preferCodexSource(candidate, best) {
			best = candidate
		}
	}
	return best, true, nil
}

func codexSourceKey(agent AgentType, uuid string) string {
	return string(agent) + ":" + uuid
}

// CodexSourceKey is the discovery identity of a Codex-format session UUID
// under the given agent. Every on-disk copy of a duplicated UUID shares this
// key, so the sync engine's reconciliation index resolves same-UUID
// replacements with one bounded lookup instead of an archive walk. The agent
// keeps Codex and its TraeX fork in separate identity namespaces even when
// both archives happen to hold the same UUID.
func CodexSourceKey(agent AgentType, uuid string) string {
	return codexSourceKey(agent, uuid)
}

func preferCodexSource(candidate, current SourceRef) bool {
	cand := candidate.Opaque.(codexSource)
	curr := current.Opaque.(codexSource)
	if cand.Layout != curr.Layout {
		return cand.Layout == CodexLayoutDated
	}
	return candidate.DisplayPath < current.DisplayPath
}

func codexProviderUserMessageCount(messages []ParsedMessage) int {
	count := 0
	for _, message := range messages {
		if message.Role == RoleUser && message.Content != "" {
			count++
		}
	}
	return count
}

func codexProviderTokenTotals(
	messages []ParsedMessage,
) (totalOut int, peakCtx int, hasTotalOut bool, hasPeakCtx bool) {
	for _, message := range messages {
		if message.HasOutputTokens {
			totalOut += message.OutputTokens
			hasTotalOut = true
		}
		if message.HasContextTokens &&
			(!hasPeakCtx || message.ContextTokens > peakCtx) {
			peakCtx = message.ContextTokens
			hasPeakCtx = true
		}
	}
	return totalOut, peakCtx, hasTotalOut, hasPeakCtx
}

func codexIncrementalTermination(
	lastTaskEvent string,
) *TerminationStatus {
	status := classifyCodexTermination(lastTaskEvent)
	if status == "" {
		return nil
	}
	return &status
}

func codexProviderCapabilities() Capabilities {
	return Capabilities{
		Source: SourceCapabilities{
			DiscoverSources:      CapabilitySupported,
			StreamingDiscovery:   CapabilitySupported,
			WatchSources:         CapabilitySupported,
			ActivityHints:        CapabilitySupported,
			ClassifyChangedPath:  CapabilitySupported,
			FindSource:           CapabilitySupported,
			CompositeFingerprint: CapabilitySupported,
			IncrementalAppend:    CapabilitySupported,
			MultiSessionSource:   CapabilityNotApplicable,
			PerSessionErrors:     CapabilityNotApplicable,
			ExcludedSessions:     CapabilityNotApplicable,
			ForceReplaceOnParse:  CapabilitySupported,
			VerifiedLocalStat:    CapabilitySupported,
			MultiFileStatHash:    CapabilitySupported,
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
			ToolResultEvents:     CapabilitySupported,
			PerMessageTokenUsage: CapabilitySupported,
			TerminationStatus:    CapabilitySupported,
			Model:                CapabilitySupported,
		},
		Sync: ProviderSyncSemantics{
			FingerprintHashInCacheKey:           true,
			FingerprintHashRequiredForFreshness: true,
			SkipCacheFreshWithoutStoredRow:      true,
		},
	}
}
