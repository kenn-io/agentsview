package parser

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/tidwall/gjson"
)

type junieSourceSet struct {
	JSONLSourceSet
	indexCache *junieIndexCache
}

type junieIndexSummary struct {
	ProjectDir string `json:"projectDir,omitempty"`
	TaskName   string `json:"taskName,omitempty"`
	CreatedAt  int64  `json:"createdAt,omitempty"`
	UpdatedAt  int64  `json:"updatedAt,omitempty"`
}

type junieCachedSummary struct {
	hash    string
	summary junieIndexSummary
	present bool
}

type junieProviderFactory struct {
	ProviderFactory
	indexCache *junieIndexCache
}

type junieProvider struct {
	*SourceSetProvider
	indexCache *junieIndexCache
}

type junieRootState struct {
	identity        os.FileInfo
	watchSummaries  map[string]junieIndexSummary
	activeSummaries map[string]junieIndexSummary
	retryIDs        map[string]struct{}
	plannedIDs      []string
	planReady       bool
	parseSummaries  map[string]junieCachedSummary
}

type junieIndexCache struct {
	mu    sync.Mutex
	roots map[string]*junieRootState
}

func (c *junieIndexCache) stateLocked(root string) *junieRootState {
	root = filepath.Clean(root)
	if c.roots == nil {
		c.roots = make(map[string]*junieRootState)
	}
	state := c.roots[root]
	if state == nil {
		state = &junieRootState{}
		c.roots[root] = state
	}
	return state
}

func (c *junieIndexCache) openRoot(path string) (*os.Root, error) {
	return c.openRootGeneration(path, false)
}

func (c *junieIndexCache) openRootForDiscovery(path string) (*os.Root, error) {
	return c.openRootGeneration(path, true)
}

func (c *junieIndexCache) openRootGeneration(path string, allowRepin bool) (*os.Root, error) {
	path = filepath.Clean(path)
	root, info, err := openValidatedJunieRoot(path)
	if err != nil {
		c.mu.Lock()
		state := c.roots[path]
		c.mu.Unlock()
		if state != nil && state.identity != nil && os.IsNotExist(err) {
			return nil, errors.New("previously opened Junie root is temporarily unavailable")
		}
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.roots[path]
	if state != nil && state.identity != nil && !os.SameFile(state.identity, info) {
		if !allowRepin {
			_ = root.Close()
			return nil, errors.New("junie root identity changed")
		}
		state = nil
	}
	if state == nil {
		if c.roots == nil {
			c.roots = make(map[string]*junieRootState)
		}
		state = &junieRootState{}
		c.roots[path] = state
	}
	state.identity = info
	return root, nil
}

func newJunieProviderFactory(def AgentDef) ProviderFactory {
	indexCache := new(junieIndexCache)
	base := NewSourceSetFactory(
		def,
		junieProviderCapabilities(),
		func(cfg ProviderConfig) SourceSet {
			return newJunieSourceSetWithCache(cfg.Roots, indexCache)
		},
	)
	return &junieProviderFactory{ProviderFactory: base, indexCache: indexCache}
}

func (f *junieProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	return &junieProvider{
		SourceSetProvider: f.ProviderFactory.NewProvider(cfg).(*SourceSetProvider),
		indexCache:        f.indexCache,
	}
}

func (p *junieProvider) AcknowledgeSourceSync(source SourceRef) {
	p.indexCache.acknowledge(source)
}

func newJunieSourceSetWithCache(
	roots []string, indexCache *junieIndexCache,
) junieSourceSet {
	return junieSourceSet{
		JSONLSourceSet: NewDirectoryJSONLSourceSet(AgentJunie, roots,
			WithIncludePath(func(_, path string) bool {
				return filepath.Base(path) == "events.jsonl"
			}),
			WithSessionIDFromPath(func(_, path string) string {
				return filepath.Base(filepath.Dir(path))
			}),
			WithParseFile(indexCache.parseFile),
			WithForceReplace(),
		).JSONLSourceSet,
		indexCache: indexCache,
	}
}

func (s junieSourceSet) Discover(ctx context.Context) ([]SourceRef, error) {
	return collectDiscoveredSources(ctx, s.DiscoverEach)
}

func (s junieSourceSet) DiscoverEach(
	ctx context.Context, yield func(SourceRef) error,
) error {
	if err := s.refreshJunieIndexes(ctx); err != nil {
		return err
	}
	return s.JSONLSourceSet.DiscoverEach(ctx, yield)
}

func (s junieSourceSet) FindSource(
	ctx context.Context, req FindSourceRequest,
) (SourceRef, bool, error) {
	source, found, err := s.JSONLSourceSet.FindSource(ctx, req)
	if err != nil || !found {
		return source, found, err
	}
	src, ok := source.Opaque.(JSONLSource)
	if !ok {
		return SourceRef{}, false, errors.New("junie source path unavailable")
	}
	indexPath := filepath.Join(filepath.Dir(filepath.Dir(src.Path)), "index.jsonl")
	snapshot, present, err := loadJunieIndexSnapshot(ctx, indexPath, s.indexCache.openRoot)
	if err != nil {
		return SourceRef{}, false, err
	}
	if !present {
		return source, true, nil
	}
	s.indexCache.setActiveSnapshot(indexPath, snapshot)
	return source, true, nil
}

func (s junieSourceSet) SourcesForChangedPath(
	ctx context.Context, req ChangedPathRequest,
) ([]SourceRef, error) {
	root, indexPath, ok := s.indexPath(req.Path)
	if !ok {
		return s.JSONLSourceSet.SourcesForChangedPath(ctx, req)
	}
	changedIDs, planned := s.indexCache.takePlannedIDs(indexPath)
	if !planned {
		var err error
		changedIDs, err = s.indexCache.classifyIndexChange(ctx, indexPath)
		if err != nil {
			return nil, err
		}
	}
	sources, unavailableIDs := s.sourcesForSessionIDs(root, changedIDs)
	s.indexCache.retireUnavailable(indexPath, unavailableIDs)
	return sources, nil
}

func (s junieSourceSet) ChangedPathRelevance(
	ctx context.Context, req ChangedPathRequest,
) (ChangedPathRelevance, error) {
	_, indexPath, ok := s.indexPath(req.Path)
	if !ok {
		return ChangedPathUnclassified, nil
	}
	changedIDs, err := s.indexCache.classifyIndexChange(ctx, indexPath)
	if err != nil {
		return ChangedPathUnclassified, err
	}
	s.indexCache.stagePlannedIDs(indexPath, changedIDs)
	if len(changedIDs) == 0 {
		return ChangedPathNonData, nil
	}
	return ChangedPathDataBearing, nil
}

func (s junieSourceSet) indexPath(path string) (string, string, bool) {
	for _, root := range s.roots {
		indexPath := filepath.Join(root, "index.jsonl")
		if samePath(path, indexPath) {
			return root, indexPath, true
		}
	}
	return "", "", false
}

func (s junieSourceSet) sourcesForSessionIDs(
	root string, sessionIDs []string,
) ([]SourceRef, []string) {
	sources := make([]SourceRef, 0, len(sessionIDs))
	unavailableIDs := make([]string, 0)
	for _, sessionID := range sessionIDs {
		path := filepath.Join(root, sessionID, "events.jsonl")
		if !IsDirectoryJSONLPath(root, path) {
			unavailableIDs = append(unavailableIDs, sessionID)
			continue
		}
		dirInfo, err := os.Lstat(filepath.Dir(path))
		if err != nil || !dirInfo.IsDir() {
			unavailableIDs = append(unavailableIDs, sessionID)
			continue
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			unavailableIDs = append(unavailableIDs, sessionID)
			continue
		}
		source, ok := s.sourceRef(root, path, info)
		if !ok {
			unavailableIDs = append(unavailableIDs, sessionID)
			continue
		}
		sources = append(sources, source)
	}
	return sources, unavailableIDs
}

func (c *junieIndexCache) classifyIndexChange(
	ctx context.Context, indexPath string,
) ([]string, error) {
	current, present, err := loadJunieIndexSnapshot(ctx, indexPath, c.openRoot)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.stateLocked(filepath.Dir(indexPath))
	previous := state.watchSummaries
	if previous == nil {
		previous = map[string]junieIndexSummary{}
	}
	if !present {
		// The producer atomically replaces the complete index. A missing file is
		// not a valid replacement snapshot, so retain the last complete view.
		current = previous
	}
	changedIDs := changedJunieSummaryIDs(previous, current)
	state.watchSummaries = current
	state.activeSummaries = current
	if state.retryIDs == nil {
		state.retryIDs = make(map[string]struct{})
	}
	for _, sessionID := range changedIDs {
		state.retryIDs[sessionID] = struct{}{}
	}
	all := make([]string, 0, len(state.retryIDs))
	for sessionID := range state.retryIDs {
		all = append(all, sessionID)
	}
	sort.Strings(all)
	return all, nil
}

func (c *junieIndexCache) stagePlannedIDs(indexPath string, sessionIDs []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.stateLocked(filepath.Dir(indexPath))
	state.plannedIDs = append([]string(nil), sessionIDs...)
	state.planReady = true
}

func (c *junieIndexCache) takePlannedIDs(indexPath string) ([]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.stateLocked(filepath.Dir(indexPath))
	sessionIDs, ready := state.plannedIDs, state.planReady
	state.plannedIDs = nil
	state.planReady = false
	return sessionIDs, ready
}

// retireUnavailable drops pending IDs that produced no source, so a removed or
// otherwise unavailable session cannot stay pending forever. Nothing can parse or
// acknowledge such an ID, and a transcript that reappears still triggers its own
// changed-path event, so keeping it pending only forces archive-wide fallback.
func (c *junieIndexCache) retireUnavailable(indexPath string, sessionIDs []string) {
	if len(sessionIDs) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.stateLocked(filepath.Dir(indexPath))
	for _, sessionID := range sessionIDs {
		delete(state.retryIDs, sessionID)
	}
}

func (s junieSourceSet) Fingerprint(
	ctx context.Context, source SourceRef,
) (SourceFingerprint, error) {
	if err := ctx.Err(); err != nil {
		return SourceFingerprint{}, err
	}
	src, ok := source.Opaque.(JSONLSource)
	if !ok {
		return SourceFingerprint{}, errors.New("junie source path unavailable")
	}
	f, err := openJunieEventStream(src.Path, s.indexCache.openRoot)
	if err != nil {
		return SourceFingerprint{}, fmt.Errorf("open %s: %w", src.Path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return SourceFingerprint{}, fmt.Errorf("stat %s: %w", src.Path, err)
	}
	inode, device := sourceFileIdentity(info)
	h := sha256.New()
	if _, err := io.Copy(h, checkedContextReader{ctx: ctx, reader: f}); err != nil {
		return SourceFingerprint{}, fmt.Errorf("hash %s: %w", src.Path, err)
	}
	fingerprint := SourceFingerprint{
		Key:  firstNonEmptyJSONLString(source.FingerprintKey, source.Key, src.Path),
		Hash: hex.EncodeToString(h.Sum(nil)),
		Size: info.Size(), MTimeNS: info.ModTime().UnixNano(),
		Inode: inode, Device: device,
	}

	indexPath := filepath.Join(filepath.Dir(filepath.Dir(src.Path)), "index.jsonl")
	sessionID := filepath.Base(filepath.Dir(src.Path))
	summary, present, err := s.indexCache.activeSummary(ctx, indexPath, sessionID)
	if err != nil {
		return SourceFingerprint{}, err
	}
	if present {
		// Scope shared index metadata to this session so one summary update does not
		// invalidate every transcript under the Junie root.
		data, err := json.Marshal(summary)
		if err != nil {
			return SourceFingerprint{}, fmt.Errorf("marshal Junie index summary: %w", err)
		}
		sum := sha256.Sum256([]byte(fingerprint.Hash + "\x00" + string(data)))
		fingerprint.Hash = hex.EncodeToString(sum[:])
	}
	s.indexCache.rememberParseSummary(src.Path, fingerprint.Hash, summary, present)
	return fingerprint, nil
}

func changedJunieSummaryIDs(
	previous, current map[string]junieIndexSummary,
) []string {
	changed := make([]string, 0)
	for sessionID, summary := range current {
		if previous[sessionID] != summary {
			changed = append(changed, sessionID)
		}
	}
	for sessionID := range previous {
		if _, present := current[sessionID]; !present {
			changed = append(changed, sessionID)
		}
	}
	sort.Strings(changed)
	return changed
}

func (s junieSourceSet) refreshJunieIndexes(ctx context.Context) error {
	for _, root := range s.roots {
		indexPath := filepath.Join(root, "index.jsonl")
		snapshot, present, err := loadJunieIndexSnapshot(
			ctx, indexPath, s.indexCache.openRootForDiscovery,
		)
		if err != nil {
			return err
		}
		s.indexCache.mu.Lock()
		state := s.indexCache.stateLocked(root)
		if !present {
			if state.watchSummaries != nil {
				snapshot = state.watchSummaries
			} else {
				snapshot = map[string]junieIndexSummary{}
			}
		}
		state.watchSummaries = snapshot
		state.activeSummaries = snapshot
		s.indexCache.mu.Unlock()
	}
	return nil
}

func (c *junieIndexCache) activeSummary(
	ctx context.Context, indexPath, sessionID string,
) (junieIndexSummary, bool, error) {
	root := filepath.Dir(indexPath)
	c.mu.Lock()
	state := c.stateLocked(root)
	snapshot := state.activeSummaries
	c.mu.Unlock()

	if snapshot == nil {
		loaded, present, err := loadJunieIndexSnapshot(ctx, indexPath, c.openRoot)
		if err != nil {
			return junieIndexSummary{}, false, err
		}
		if !present {
			loaded = map[string]junieIndexSummary{}
		}
		c.mu.Lock()
		state = c.stateLocked(root)
		if state.activeSummaries == nil {
			state.activeSummaries = loaded
		}
		snapshot = state.activeSummaries
		c.mu.Unlock()
	}

	summary, present := snapshot[sessionID]
	return summary, present, nil
}

func (c *junieIndexCache) setActiveSnapshot(
	indexPath string, snapshot map[string]junieIndexSummary,
) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stateLocked(filepath.Dir(indexPath)).activeSummaries = snapshot
}

func (c *junieIndexCache) rememberParseSummary(
	path, hash string, summary junieIndexSummary, present bool,
) {
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.stateLocked(filepath.Dir(filepath.Dir(path)))
	if state.parseSummaries == nil {
		state.parseSummaries = make(map[string]junieCachedSummary)
	}
	state.parseSummaries[path] = junieCachedSummary{
		hash: hash, summary: summary, present: present,
	}
}

func (c *junieIndexCache) parseSummary(
	ctx context.Context, path, hash string,
) (junieIndexSummary, bool, error) {
	root := filepath.Dir(filepath.Dir(path))
	c.mu.Lock()
	cached, ok := c.stateLocked(root).parseSummaries[path]
	c.mu.Unlock()
	if ok && cached.hash == hash {
		return cached.summary, cached.present, nil
	}
	indexPath := filepath.Join(root, "index.jsonl")
	return c.activeSummary(ctx, indexPath, filepath.Base(filepath.Dir(path)))
}

func (c *junieIndexCache) acknowledge(source SourceRef) {
	src, ok := source.Opaque.(JSONLSource)
	if !ok {
		return
	}
	root := filepath.Dir(filepath.Dir(src.Path))
	sessionID := filepath.Base(filepath.Dir(src.Path))
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.stateLocked(root).retryIDs, sessionID)
}

func loadJunieIndexSnapshot(
	ctx context.Context, path string, openRoot junieRootOpener,
) (map[string]junieIndexSummary, bool, error) {
	root, err := openRoot(filepath.Dir(path))
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("open Junie index root %s: %w", path, err)
	}
	defer root.Close()

	name := filepath.Base(path)
	info, err := root.Lstat(name)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("stat Junie index %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("junie index %s is not a regular file", path)
	}
	f, err := openJuniePinnedFile(root, name, info)
	if err != nil {
		return nil, false, fmt.Errorf("open Junie index %s: %w", path, err)
	}
	defer f.Close()

	summaries := make(map[string]junieIndexSummary)
	lr := newLineReaderContext(ctx, f, maxLineSize)
	defer releaseLineReader(lr)
	lineNumber := 0
	for {
		line, ok := lr.next()
		if !ok {
			break
		}
		lineNumber++
		if err := contextErrEvery(ctx, lineNumber); err != nil {
			return nil, false, err
		}
		if !gjson.Valid(line) {
			return nil, false, fmt.Errorf("reading Junie index %s: invalid JSON at line %d", path, lineNumber)
		}
		sessionID := gjson.Get(line, "sessionId").Str
		if sessionID == "" {
			return nil, false, fmt.Errorf("reading Junie index %s: missing sessionId at line %d", path, lineNumber)
		}
		summaries[sessionID] = parseJunieIndexSummary(line)
	}
	if err := lr.Err(); err != nil {
		return nil, false, fmt.Errorf("reading Junie index %s: %w", path, err)
	}
	return summaries, true, nil
}

func parseJunieIndexSummary(line string) junieIndexSummary {
	summary := parseJunieSessionSummary(line)
	var createdAt, updatedAt int64
	if !summary.createdAt.IsZero() {
		createdAt = summary.createdAt.UnixMilli()
	}
	if !summary.updatedAt.IsZero() {
		updatedAt = summary.updatedAt.UnixMilli()
	}
	return junieIndexSummary{
		ProjectDir: summary.projectDir,
		TaskName:   summary.taskName,
		CreatedAt:  createdAt,
		UpdatedAt:  updatedAt,
	}
}

func (c *junieIndexCache) parseFile(
	ctx context.Context, path string, req ParseRequest,
) ([]ParseResult, []string, error) {
	indexSummary, present, err := c.parseSummary(ctx, path, req.Fingerprint.Hash)
	if err != nil {
		return nil, nil, err
	}
	summary := junieSessionSummary{}
	if present {
		summary = indexSummary.sessionSummary()
	}
	sess, msgs, complete, err := parseJunieSessionWithSummary(
		ctx, path, req.Machine, summary, present, c.openRoot,
	)
	if err != nil {
		return nil, nil, err
	}
	if !complete || sess == nil {
		return nil, nil, nil
	}
	if req.Fingerprint.Hash != "" {
		sess.File.Hash = req.Fingerprint.Hash
	}
	if req.Fingerprint.Size > 0 {
		sess.File.Size = req.Fingerprint.Size
	}
	if req.Fingerprint.MTimeNS > 0 {
		sess.File.Mtime = req.Fingerprint.MTimeNS
	}
	return []ParseResult{{Session: *sess, Messages: msgs, UsageEvents: sess.UsageEvents}}, nil, nil
}

func junieProviderCapabilities() Capabilities {
	source := jsonlFileProviderSourceCapabilities()
	source.ForceReplaceOnParse = CapabilitySupported
	source.ChangedPathRelevance = CapabilitySupported
	return Capabilities{
		Source: source,
		Sync: ProviderSyncSemantics{
			FingerprintHashInCacheKey:           true,
			FingerprintHashRequiredForFreshness: true,
		},
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			SessionName:          CapabilitySupported,
			Cwd:                  CapabilitySupported,
			AggregateUsageEvents: CapabilitySupported,
			MalformedLineCount:   CapabilitySupported,
		},
	}
}
