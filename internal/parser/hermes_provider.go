package parser

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"hash"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var _ Provider = (*hermesProvider)(nil)

type hermesProviderFactory struct {
	def AgentDef
}

func newHermesProviderFactory(def AgentDef) ProviderFactory {
	return hermesProviderFactory{def: cloneAgentDef(def)}
}

func (f hermesProviderFactory) Definition() AgentDef {
	return cloneAgentDef(f.def)
}

func (f hermesProviderFactory) Capabilities() Capabilities {
	return hermesProviderCapabilities()
}

func (f hermesProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	cfg = cfg.Clone()
	return &hermesProvider{
		ProviderBase: ProviderBase{
			Def:    cloneAgentDef(f.def),
			Caps:   hermesProviderCapabilities(),
			Config: cfg,
		},
		sources: newHermesSourceSet(cfg.Roots),
	}
}

type hermesProvider struct {
	ProviderBase
	sources hermesSourceSet
}

func (p *hermesProvider) Discover(ctx context.Context) ([]SourceRef, error) {
	return p.sources.Discover(ctx)
}

func (p *hermesProvider) WatchPlan(ctx context.Context) (WatchPlan, error) {
	return p.sources.WatchPlan(ctx)
}

func (p *hermesProvider) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	return p.sources.SourcesForChangedPath(ctx, req)
}

func (p *hermesProvider) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	req = ProviderFindRequestWithRawSessionID(p.Def, req)
	return p.sources.FindSource(ctx, req)
}

func (p *hermesProvider) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	return p.sources.Fingerprint(ctx, source)
}

func WriteHermesSessionJSONL(
	w io.Writer, storedPath string, roots []string, rawSessionID string,
) error {
	var storedStateErr error
	if path := ResolveSourceFilePath(storedPath); filepath.Base(path) == "state.db" {
		if _, err := os.Stat(path); err == nil {
			err = writeHermesStateSessionJSONL(w, path, rawSessionID)
			if err == nil {
				return nil
			}
			var lookupErr hermesStateLookupError
			if !errors.Is(err, os.ErrNotExist) &&
				!errors.As(err, &lookupErr) {
				return err
			}
			storedStateErr = err
		}
		if transcript := findHermesSourceFile(
			filepath.Join(filepath.Dir(path), "sessions"),
			rawSessionID,
		); transcript != "" {
			return copyHermesTranscriptFile(w, transcript)
		}
	}
	provider, ok := NewProvider(AgentHermes, ProviderConfig{Roots: roots})
	if !ok {
		return fmt.Errorf("hermes provider unavailable")
	}
	hp, ok := provider.(*hermesProvider)
	if !ok {
		return fmt.Errorf("hermes provider unavailable")
	}
	source, found, err := hp.FindSource(
		context.Background(),
		FindSourceRequest{RawSessionID: rawSessionID},
	)
	if err != nil {
		return err
	}
	if !found {
		if storedStateErr != nil && !errors.Is(storedStateErr, os.ErrNotExist) {
			return storedStateErr
		}
		return fmt.Errorf(
			"hermes session %s source not found: %w",
			rawSessionID, os.ErrNotExist,
		)
	}
	path, ok := hp.sources.pathFromSource(source)
	if !ok {
		return fmt.Errorf("hermes source path unavailable")
	}
	if filepath.Base(path) == "state.db" {
		return writeHermesStateSessionJSONL(w, path, rawSessionID)
	}
	return copyHermesTranscriptFile(w, path)
}

func (p *hermesProvider) Parse(
	ctx context.Context,
	req ParseRequest,
) (ParseOutcome, error) {
	if err := ctx.Err(); err != nil {
		return ParseOutcome{}, err
	}
	path, ok := p.sources.pathFromSource(req.Source)
	if !ok {
		return ParseOutcome{}, fmt.Errorf("hermes source path unavailable")
	}
	machine := firstNonEmptyJSONLString(req.Machine, p.Config.Machine)
	if filepath.Base(path) == "state.db" {
		results, err := p.parseArchive(path, req.Source.ProjectHint, machine)
		if err != nil {
			return ParseOutcome{}, err
		}
		// Mirror the legacy engine's stampHermesArchiveResults: every archive
		// session's stored file identity is the state.db path with the
		// aggregate (state.db plus transcripts) size and mtime, so a
		// transcript-only change still refreshes the archive's freshness.
		size, mtime := hermesArchiveEffectiveFileInfo(path)
		out := make([]ParseResultOutcome, 0, len(results))
		for i := range results {
			results[i].Session.File.Path = path
			results[i].Session.File.Size = size
			results[i].Session.File.Mtime = mtime
			out = append(out, ParseResultOutcome{
				Result:      results[i],
				DataVersion: DataVersionCurrent,
			})
		}
		return ParseOutcome{
			Results:           out,
			ResultSetComplete: true,
			ForceReplace:      true,
		}, nil
	}

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

type hermesSource struct {
	Root string
	Path string
}

type hermesSourceSet struct {
	roots []string
}

func newHermesSourceSet(roots []string) hermesSourceSet {
	return hermesSourceSet{roots: cleanJSONLRoots(roots)}
}

func (s hermesSourceSet) Discover(ctx context.Context) ([]SourceRef, error) {
	var sources []SourceRef
	seen := make(map[string]struct{})
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, file := range discoverHermesSessions(root) {
			source, ok := s.sourceRef(root, file.Path)
			if !ok {
				continue
			}
			addJSONLSource(source, &sources, seen)
		}
	}
	sortJSONLSources(sources)
	return sources, nil
}

func (s hermesSourceSet) WatchPlan(context.Context) (WatchPlan, error) {
	roots := make([]WatchRoot, 0, len(s.roots))
	for _, root := range s.roots {
		roots = append(roots, hermesWatchRoots(root)...)
	}
	return WatchPlan{Roots: roots}, nil
}

func (s hermesSourceSet) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	allowMissing := jsonlMissingPathFallbackAllowed(req)
	if req.WatchRoot != "" {
		watchRoot := filepath.Clean(req.WatchRoot)
		for _, root := range s.roots {
			if !hermesWatchRootMatches(root, watchRoot) {
				continue
			}
			source, ok := s.sourceForChangedPath(root, req.Path, allowMissing)
			if ok {
				return []SourceRef{source}, nil
			}
		}
		return nil, nil
	}
	for _, root := range s.roots {
		source, ok := s.sourceForChangedPath(root, req.Path, allowMissing)
		if ok {
			return []SourceRef{source}, nil
		}
	}
	return nil, nil
}

func (s hermesSourceSet) FindSource(
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
			if source, ok := s.sourceForPath(root, path); ok {
				return source, true, nil
			}
		}
	}
	if req.RawSessionID == "" {
		return SourceRef{}, false, nil
	}
	for _, root := range s.roots {
		if stateDB, _, ok := hermesStatePaths(root); ok &&
			IsValidSessionID(req.RawSessionID) {
			found, err := hermesStateDBHasSession(stateDB, req.RawSessionID)
			switch {
			case err != nil:
				// Mirror parseArchive: an unreadable or schema-incompatible
				// state.db falls back to transcripts rather than aborting the
				// lookup, so a valid transcript session next to a bad state.db
				// stays resolvable for resync.
				log.Printf(
					"hermes: state db lookup failed for %s: %v; "+
						"falling back to transcripts", stateDB, err,
				)
			case !found:
			default:
				if source, ok := s.sourceRef(root, stateDB); ok {
					return source, true, nil
				}
			}
		}
		transcriptRoot := hermesTranscriptRoot(root)
		path := findHermesSourceFile(transcriptRoot, req.RawSessionID)
		if path == "" {
			continue
		}
		if source, ok := s.sourceRef(root, path); ok {
			return source, true, nil
		}
	}
	return SourceRef{}, false, nil
}

type hermesStateLookupError struct {
	err error
}

func (e hermesStateLookupError) Error() string {
	return e.err.Error()
}

func (e hermesStateLookupError) Unwrap() error {
	return e.err
}

func hermesStateDBHasSession(stateDB string, rawID string) (bool, error) {
	conn, err := sql.Open("sqlite3", "file:"+sqliteURIPath(stateDB)+"?mode=ro")
	if err != nil {
		return false, fmt.Errorf("open hermes state db: %w", err)
	}
	defer conn.Close()

	var found int
	err = conn.QueryRow(
		"SELECT 1 FROM sessions WHERE id = ? LIMIT 1",
		rawID,
	).Scan(&found)
	if err == nil {
		return true, nil
	}
	if err == sql.ErrNoRows {
		return false, nil
	}
	return false, fmt.Errorf("query hermes session %s: %w", rawID, err)
}

func (s hermesSourceSet) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	if err := ctx.Err(); err != nil {
		return SourceFingerprint{}, err
	}
	path, ok := s.pathFromSource(source)
	if !ok {
		return SourceFingerprint{}, fmt.Errorf("hermes source path unavailable")
	}
	if filepath.Base(path) == "state.db" {
		return hermesArchiveFingerprint(source, path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return SourceFingerprint{}, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.IsDir() {
		return SourceFingerprint{}, fmt.Errorf("stat %s: source is a directory", path)
	}
	hash, err := hashJSONLSourceFile(path)
	if err != nil {
		return SourceFingerprint{}, err
	}
	return SourceFingerprint{
		Key:     firstNonEmptyJSONLString(source.FingerprintKey, source.Key, path),
		Size:    info.Size(),
		MTimeNS: info.ModTime().UnixNano(),
		Hash:    hash,
	}, nil
}

func (s hermesSourceSet) pathFromSource(source SourceRef) (string, bool) {
	switch src := source.Opaque.(type) {
	case hermesSource:
		return src.Path, src.Path != ""
	case *hermesSource:
		if src != nil && src.Path != "" {
			return src.Path, true
		}
	}
	for _, candidate := range []string{
		source.DisplayPath,
		source.FingerprintKey,
		source.Key,
	} {
		for _, root := range s.roots {
			if ref, ok := s.sourceForPath(root, candidate); ok {
				src := ref.Opaque.(hermesSource)
				return src.Path, true
			}
		}
	}
	return "", false
}

func (s hermesSourceSet) sourceForPath(root, path string) (SourceRef, bool) {
	return s.sourceForChangedPath(root, path, false)
}

func (s hermesSourceSet) sourceForChangedPath(
	root,
	path string,
	allowMissing bool,
) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if stateDB, sessionsDir, ok := hermesStatePaths(root); ok {
		if samePath(path, stateDB) || hermesPathInTranscriptDir(sessionsDir, path) {
			return hermesArchiveSourceRef(root, stateDB)
		}
		return SourceRef{}, false
	}
	if allowMissing {
		if stateDB, sessionsDir, ok := hermesArchivePathsForEvent(root, path); ok &&
			(samePath(path, stateDB) || hermesPathInTranscriptDir(sessionsDir, path)) {
			return hermesArchiveSourceRef(root, stateDB)
		}
		transcriptRoot := hermesTranscriptRoot(root)
		if hermesPathInTranscriptDir(transcriptRoot, path) {
			return hermesTranscriptSourceRef(root, path)
		}
	}
	return s.sourceRef(root, path)
}

func (s hermesSourceSet) sourceRef(root, path string) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if stateDB, _, ok := hermesStatePaths(root); ok && samePath(path, stateDB) {
		return hermesArchiveSourceRef(root, stateDB)
	}
	transcriptRoot := hermesTranscriptRoot(root)
	if !hermesPathInTranscriptDir(transcriptRoot, path) || !IsRegularFile(path) {
		return SourceRef{}, false
	}
	return hermesTranscriptSourceRef(root, path)
}

func hermesArchiveSourceRef(root, stateDB string) (SourceRef, bool) {
	root = filepath.Clean(root)
	stateDB = filepath.Clean(stateDB)
	return SourceRef{
		Provider:       AgentHermes,
		ConfiguredRoot: root,
		Key:            stateDB,
		DisplayPath:    stateDB,
		FingerprintKey: stateDB,
		Opaque: hermesSource{
			Root: root,
			Path: stateDB,
		},
	}, true
}

func hermesTranscriptSourceRef(root, path string) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	return SourceRef{
		Provider:       AgentHermes,
		ConfiguredRoot: root,
		Key:            path,
		DisplayPath:    path,
		FingerprintKey: path,
		Opaque: hermesSource{
			Root: root,
			Path: path,
		},
	}, true
}

func hermesWatchRoots(root string) []WatchRoot {
	root = filepath.Clean(root)
	if stateDB, sessionsDir, ok := hermesArchiveRootPaths(root); ok {
		watchRoots := []WatchRoot{{
			Path:         filepath.Dir(stateDB),
			Recursive:    false,
			IncludeGlobs: []string{"state.db"},
			DebounceKey:  string(AgentHermes) + ":archive:" + root,
		}}
		watchRoots = append(watchRoots, WatchRoot{
			Path:         sessionsDir,
			Recursive:    true,
			IncludeGlobs: []string{"*.jsonl", "session_*.json"},
			DebounceKey:  string(AgentHermes) + ":sessions:" + root,
		})
		return watchRoots
	}
	return []WatchRoot{{
		Path:         root,
		Recursive:    true,
		IncludeGlobs: []string{"state.db", "*.jsonl", "session_*.json"},
		DebounceKey:  string(AgentHermes) + ":sessions:" + root,
	}}
}

func ResolveHermesWatchRoots(root string) []string {
	root = filepath.Clean(root)
	if _, sessionsDir, ok := hermesArchiveRootPaths(root); ok {
		return []string{sessionsDir}
	}
	return []string{root}
}

func ResolveHermesShallowWatchRoots(root string) []string {
	root = filepath.Clean(root)
	if stateDB, _, ok := hermesArchiveRootPaths(root); ok {
		return []string{filepath.Dir(stateDB)}
	}
	return nil
}

func hermesWatchRootMatches(root, watchRoot string) bool {
	root = filepath.Clean(root)
	watchRoot = filepath.Clean(watchRoot)
	if samePath(root, watchRoot) {
		return true
	}
	if stateDB, sessionsDir, ok := hermesArchiveRootPaths(root); ok {
		return samePath(watchRoot, filepath.Dir(stateDB)) ||
			samePath(watchRoot, sessionsDir)
	}
	switch filepath.Base(root) {
	case "state.db":
		return samePath(watchRoot, filepath.Dir(root)) ||
			samePath(watchRoot, filepath.Join(filepath.Dir(root), "sessions"))
	case "sessions":
		return samePath(watchRoot, filepath.Dir(root))
	default:
		return samePath(watchRoot, filepath.Join(root, "sessions"))
	}
}

func hermesArchivePathsForEvent(root, path string) (stateDB, sessionsDir string, ok bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	switch {
	case filepath.Base(root) == "state.db":
		stateDB = root
		sessionsDir = filepath.Join(filepath.Dir(root), "sessions")
	case filepath.Base(root) == "sessions":
		stateDB = filepath.Join(filepath.Dir(root), "state.db")
		sessionsDir = root
	case samePath(path, filepath.Join(root, "state.db")) ||
		IsRegularFile(filepath.Join(root, "state.db")):
		stateDB = filepath.Join(root, "state.db")
		sessionsDir = filepath.Join(root, "sessions")
	default:
		return "", "", false
	}
	return stateDB, sessionsDir, true
}

func hermesArchiveRootPaths(root string) (stateDB, sessionsDir string, ok bool) {
	root = filepath.Clean(root)
	if stateDB, sessionsDir, ok := hermesStatePaths(root); ok {
		return stateDB, sessionsDir, true
	}
	switch filepath.Base(root) {
	case "state.db":
		return root, filepath.Join(filepath.Dir(root), "sessions"), true
	case "sessions":
		return filepath.Join(filepath.Dir(root), "state.db"), root, true
	default:
		stateDB = filepath.Join(root, "state.db")
		sessionsDir = filepath.Join(root, "sessions")
		if IsRegularFile(stateDB) {
			return stateDB, sessionsDir, true
		}
		if info, err := os.Stat(sessionsDir); err == nil && info.IsDir() {
			return stateDB, sessionsDir, true
		}
		return "", "", false
	}
}

func hermesTranscriptRoot(root string) string {
	root = filepath.Clean(root)
	if _, sessionsDir, ok := hermesStatePaths(root); ok {
		return sessionsDir
	}
	childSessions := filepath.Join(root, "sessions")
	if info, err := os.Stat(childSessions); err == nil && info.IsDir() {
		return childSessions
	}
	return root
}

func hermesPathInTranscriptDir(dir, path string) bool {
	dir = filepath.Clean(dir)
	path = filepath.Clean(path)
	if !samePath(filepath.Dir(path), dir) {
		return false
	}
	name := filepath.Base(path)
	if strings.HasSuffix(name, ".jsonl") {
		return true
	}
	return strings.HasSuffix(name, ".json") && strings.HasPrefix(name, "session_")
}

func hermesArchiveFingerprint(source SourceRef, stateDB string) (SourceFingerprint, error) {
	stateInfo, err := os.Stat(stateDB)
	if err != nil {
		return SourceFingerprint{}, fmt.Errorf("stat %s: %w", stateDB, err)
	}
	if stateInfo.IsDir() {
		return SourceFingerprint{}, fmt.Errorf("stat %s: source is a directory", stateDB)
	}
	fingerprint := SourceFingerprint{
		Key: firstNonEmptyJSONLString(
			source.FingerprintKey,
			source.Key,
			stateDB,
		),
		Size:    stateInfo.Size(),
		MTimeNS: stateInfo.ModTime().UnixNano(),
	}
	h := sha256.New()
	if err := addHermesFingerprintPart(h, "state", stateDB, stateInfo); err != nil {
		return SourceFingerprint{}, err
	}
	_, sessionsDir, _ := hermesStatePaths(stateDB)
	for _, file := range discoverHermesTranscriptFiles(sessionsDir) {
		info, err := os.Stat(file.Path)
		if err != nil {
			return SourceFingerprint{}, fmt.Errorf("stat %s: %w", file.Path, err)
		}
		fingerprint.Size += info.Size()
		if mtime := info.ModTime().UnixNano(); mtime > fingerprint.MTimeNS {
			fingerprint.MTimeNS = mtime
		}
		if err := addHermesFingerprintPart(h, "transcript", file.Path, info); err != nil {
			return SourceFingerprint{}, err
		}
	}
	fingerprint.Hash = fmt.Sprintf("%x", h.Sum(nil))
	return fingerprint, nil
}

// hermesArchiveEffectiveFileInfo returns the aggregate size and mtime of a
// Hermes archive: the state.db plus every transcript file in its sessions
// directory. It reproduces the legacy engine's hermesArchiveEffectiveInfo so a
// transcript-only change shifts the stored archive freshness even though the
// state.db itself is unchanged. The transcript set matches the legacy
// hermesArchiveTranscriptFiles: every .jsonl and session_*.json file directly
// under the sessions directory, without the .jsonl/.json dedup used elsewhere.
func hermesArchiveEffectiveFileInfo(stateDB string) (int64, int64) {
	info, err := os.Stat(stateDB)
	if err != nil {
		return 0, 0
	}
	size := info.Size()
	mtime := info.ModTime().UnixNano()
	_, sessionsDir, ok := hermesStatePaths(stateDB)
	if !ok {
		return size, mtime
	}
	for _, path := range hermesArchiveTranscriptFiles(sessionsDir) {
		fileInfo, err := os.Stat(path)
		if err != nil || fileInfo == nil || fileInfo.IsDir() {
			continue
		}
		size += fileInfo.Size()
		if fileMtime := fileInfo.ModTime().UnixNano(); fileMtime > mtime {
			mtime = fileMtime
		}
	}
	return size, mtime
}

// hermesArchiveTranscriptFiles lists every .jsonl and session_*.json file
// directly under sessionsDir, sorted by path. It mirrors the legacy engine
// helper of the same name so the provider's effective-info aggregation matches
// historical behavior exactly.
func hermesArchiveTranscriptFiles(sessionsDir string) []string {
	if sessionsDir == "" {
		return nil
	}
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return nil
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(name, ".jsonl") ||
			strings.HasPrefix(name, "session_") && strings.HasSuffix(name, ".json") {
			paths = append(paths, filepath.Join(sessionsDir, name))
		}
	}
	sort.Strings(paths)
	return paths
}

func addHermesFingerprintPart(
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

func hermesProviderCapabilities() Capabilities {
	return Capabilities{
		Source: SourceCapabilities{
			DiscoverSources:      CapabilitySupported,
			WatchSources:         CapabilitySupported,
			ClassifyChangedPath:  CapabilitySupported,
			FindSource:           CapabilitySupported,
			CompositeFingerprint: CapabilitySupported,
			MultiSessionSource:   CapabilitySupported,
			PerSessionErrors:     CapabilityNotApplicable,
			ExcludedSessions:     CapabilityNotApplicable,
			ForceReplaceOnParse:  CapabilitySupported,
		},
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			SessionName:          CapabilitySupported,
			Relationships:        CapabilitySupported,
			Thinking:             CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			AggregateUsageEvents: CapabilitySupported,
			Model:                CapabilitySupported,
		},
	}
}
