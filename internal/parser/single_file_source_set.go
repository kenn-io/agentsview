package parser

import (
	"context"
	"fmt"
	"path/filepath"
)

// single_file_source_set.go provides a reusable SourceSet for providers whose
// physical source is a single file that parses into exactly one session: no
// virtual member paths and no fan-out. Reasonix (transcript + .jsonl.meta
// sidecar) is the first provider built on it; the other sidecar-fingerprint
// providers (vibe, commandcode, ...) can follow.
//
// Like multiSessionContainerSourceSet, all agent-specific behavior is supplied
// through functional options (withFile*()), and the type implements SourceSet
// so it plugs into newSourceSetFactory. The composite/sidecar fingerprint
// variance lives entirely inside each provider's withFileFingerprint closure,
// so the base stays agnostic about sidecars until a shared helper is warranted.

// singleFileSource is the engine-visible Opaque payload for a single-file
// source: one physical file under a configured root.
type singleFileSource struct {
	Root string
	Path string
}

// singleFileMatch is what discovery, classification, and lookup resolve to: the
// canonical source path plus an optional project hint surfaced on the SourceRef.
type singleFileMatch struct {
	Path        string
	ProjectHint string
}

func (m singleFileMatch) toSource(root string) singleFileSource {
	return singleFileSource{Root: root, Path: m.Path}
}

type singleFileConfig struct {
	// discoverFiles returns the source files under one root.
	discoverFiles func(root string) []singleFileMatch
	// watchRoots returns the provider WatchPlan roots for the configured roots.
	watchRoots func(roots []string) []WatchRoot
	// classifyPath maps a stored or changed path (including a sidecar event) to
	// its source. allowMissing relaxes existence checks for changed-path
	// tombstones.
	classifyPath func(root, path string, allowMissing bool) (singleFileMatch, bool)
	// findFile resolves a raw session ID to its source under one root.
	findFile func(root, rawID string) (singleFileMatch, bool)
	// fingerprint returns the source freshness fingerprint (Size/MTime/Hash);
	// the base supplies the Key. Sidecar/composite folding lives here.
	fingerprint func(src singleFileSource) (SourceFingerprint, error)
	// parseFile parses the single file into zero or more sessions plus the IDs
	// of any sessions to exclude (remove). Empty results with no exclusions is a
	// clean no-session. The full ParseRequest is passed so the closure can apply
	// its own fingerprint stamping and project-hint fallback.
	parseFile func(src singleFileSource, req ParseRequest) ([]ParseResult, []string, error)
	// alwaysComplete reports the result set as complete even when parseFile
	// yields nothing, instead of emitting SkipNoSession. Providers whose parse
	// drives session removal through exclusions (cowork) set this.
	alwaysComplete bool
}

type singleFileOption func(*singleFileConfig)

func withFileDiscovery(
	fn func(root string) []singleFileMatch,
) singleFileOption {
	return func(c *singleFileConfig) { c.discoverFiles = fn }
}

func withFileWatchRoots(
	fn func(roots []string) []WatchRoot,
) singleFileOption {
	return func(c *singleFileConfig) { c.watchRoots = fn }
}

func withFileChangedPathClassifier(
	fn func(root, path string, allowMissing bool) (singleFileMatch, bool),
) singleFileOption {
	return func(c *singleFileConfig) { c.classifyPath = fn }
}

func withFileLookup(
	fn func(root, rawID string) (singleFileMatch, bool),
) singleFileOption {
	return func(c *singleFileConfig) { c.findFile = fn }
}

func withFileFingerprint(
	fn func(src singleFileSource) (SourceFingerprint, error),
) singleFileOption {
	return func(c *singleFileConfig) { c.fingerprint = fn }
}

func withFileParse(
	fn func(src singleFileSource, req ParseRequest) ([]ParseResult, []string, error),
) singleFileOption {
	return func(c *singleFileConfig) { c.parseFile = fn }
}

// withAlwaysCompleteResultSet reports the result set as complete even when a
// parse yields no sessions, instead of skipping. Used by providers whose parse
// removes sessions via exclusions.
func withAlwaysCompleteResultSet() singleFileOption {
	return func(c *singleFileConfig) { c.alwaysComplete = true }
}

func newSingleFileSourceSet(
	agent AgentType,
	roots []string,
	opts ...singleFileOption,
) singleFileSourceSet {
	cfg := singleFileConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}
	switch {
	case cfg.discoverFiles == nil:
		panic("single-file source set: missing withFileDiscovery")
	case cfg.watchRoots == nil:
		panic("single-file source set: missing withFileWatchRoots")
	case cfg.classifyPath == nil:
		panic("single-file source set: missing withFileChangedPathClassifier")
	case cfg.findFile == nil:
		panic("single-file source set: missing withFileLookup")
	case cfg.fingerprint == nil:
		panic("single-file source set: missing withFileFingerprint")
	case cfg.parseFile == nil:
		panic("single-file source set: missing withFileParse")
	}
	return singleFileSourceSet{
		agent: agent,
		roots: cleanJSONLRoots(roots),
		cfg:   cfg,
	}
}

type singleFileSourceSet struct {
	agent AgentType
	roots []string
	cfg   singleFileConfig
}

var _ SourceSet = singleFileSourceSet{}

func (s singleFileSourceSet) Discover(
	ctx context.Context,
) ([]SourceRef, error) {
	var sources []SourceRef
	seen := make(map[string]struct{})
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, match := range s.cfg.discoverFiles(root) {
			if match.Path == "" {
				continue
			}
			addJSONLSource(s.sourceRef(root, match), &sources, seen)
		}
	}
	sortJSONLSources(sources)
	return sources, nil
}

func (s singleFileSourceSet) WatchPlan(
	context.Context,
) (WatchPlan, error) {
	return WatchPlan{Roots: s.cfg.watchRoots(s.roots)}, nil
}

func (s singleFileSourceSet) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	allowMissing := jsonlMissingPathFallbackAllowed(req)
	// A watch event may originate from a configured root or one of its watched
	// subdirectories; resolve it back to the owning configured root before
	// classifying, so per-subdir watches attribute correctly.
	if req.WatchRoot != "" {
		watchRoot := filepath.Clean(req.WatchRoot)
		for _, configured := range s.roots {
			if watchRoot == configured || samePath(watchRoot, configured) ||
				pathUnderRoot(configured, watchRoot) {
				if match, ok := s.cfg.classifyPath(configured, req.Path, allowMissing); ok {
					return []SourceRef{s.sourceRef(configured, match)}, nil
				}
				return nil, nil
			}
		}
		return nil, nil
	}
	for _, root := range s.roots {
		if match, ok := s.cfg.classifyPath(root, req.Path, allowMissing); ok {
			return []SourceRef{s.sourceRef(root, match)}, nil
		}
	}
	return nil, nil
}

func (s singleFileSourceSet) FindSource(
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
			if match, ok := s.cfg.classifyPath(root, path, false); ok {
				return s.sourceRef(root, match), true, nil
			}
		}
	}
	if req.RawSessionID == "" {
		return SourceRef{}, false, nil
	}
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return SourceRef{}, false, err
		}
		if match, ok := s.cfg.findFile(root, req.RawSessionID); ok {
			return s.sourceRef(root, match), true, nil
		}
	}
	return SourceRef{}, false, nil
}

func (s singleFileSourceSet) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	if err := ctx.Err(); err != nil {
		return SourceFingerprint{}, err
	}
	src, ok := s.sourceFromRef(source)
	if !ok {
		return SourceFingerprint{}, fmt.Errorf(
			"%s source path unavailable", s.agent,
		)
	}
	fingerprint, err := s.cfg.fingerprint(src)
	if err != nil {
		return SourceFingerprint{}, err
	}
	fingerprint.Key = firstNonEmptyJSONLString(
		source.FingerprintKey, source.Key, src.Path,
	)
	return fingerprint, nil
}

// Parse resolves the request's source and parses its single file into one
// session. It satisfies the SourceSet interface; sourceSetProvider applies the
// request/config machine fallback before calling in, so req.Machine is already
// resolved here.
func (s singleFileSourceSet) Parse(
	ctx context.Context,
	req ParseRequest,
) (ParseOutcome, error) {
	if err := ctx.Err(); err != nil {
		return ParseOutcome{}, err
	}
	src, ok := s.sourceFromRef(req.Source)
	if !ok {
		return ParseOutcome{}, fmt.Errorf("%s source path unavailable", s.agent)
	}
	results, excluded, err := s.cfg.parseFile(src, req)
	if err != nil {
		return ParseOutcome{}, err
	}
	if !s.cfg.alwaysComplete && len(results) == 0 && len(excluded) == 0 {
		return ParseOutcome{
			ResultSetComplete: true,
			SkipReason:        SkipNoSession,
		}, nil
	}
	out := make([]ParseResultOutcome, 0, len(results))
	for i := range results {
		out = append(out, ParseResultOutcome{
			Result:      results[i],
			DataVersion: DataVersionCurrent,
		})
	}
	return ParseOutcome{
		Results:            out,
		ExcludedSessionIDs: excluded,
		ResultSetComplete:  true,
	}, nil
}

func (s singleFileSourceSet) sourceRef(
	root string, match singleFileMatch,
) SourceRef {
	return SourceRef{
		Provider:       s.agent,
		Key:            match.Path,
		DisplayPath:    match.Path,
		FingerprintKey: match.Path,
		ProjectHint:    match.ProjectHint,
		Opaque:         match.toSource(root),
	}
}

func (s singleFileSourceSet) sourceFromRef(
	source SourceRef,
) (singleFileSource, bool) {
	switch src := source.Opaque.(type) {
	case singleFileSource:
		return src, src.Path != ""
	case *singleFileSource:
		if src != nil && src.Path != "" {
			return *src, true
		}
	}
	for _, candidate := range []string{
		source.DisplayPath, source.FingerprintKey, source.Key,
	} {
		if candidate == "" {
			continue
		}
		for _, root := range s.roots {
			if match, ok := s.cfg.classifyPath(root, candidate, false); ok {
				return match.toSource(root), true
			}
		}
	}
	return singleFileSource{}, false
}

// newSingleFileProviderFactory builds a ProviderFactory for a single-file
// provider. It is a thin adapter over the generic sourceSetFactory; the build
// closure constructs the agent's configured source set.
func newSingleFileProviderFactory(
	def AgentDef,
	caps Capabilities,
	build func(cfg ProviderConfig) singleFileSourceSet,
) ProviderFactory {
	return newSourceSetFactory(
		def, caps,
		func(cfg ProviderConfig) SourceSet { return build(cfg) },
	)
}

// pathUnderRoot reports whether candidate is root itself or nested under it.
func pathUnderRoot(root, candidate string) bool {
	_, ok := relUnder(filepath.Clean(root), filepath.Clean(candidate))
	return ok
}
