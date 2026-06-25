package parser

import (
	"context"
	"fmt"
)

// multi_session_container.go provides a reusable source-set, provider, and
// factory for agents whose physical source is a *container* of many sessions
// surfaced to the engine as virtual per-member paths. Shelley (one SQLite DB ->
// many conversations) and Aider (one history file -> many runs) are the first
// two providers built on it; Zed, Kiro, OpenCode, and the other multi-session
// containers can follow.
//
// All agent-specific behavior is supplied through functional options (with*()),
// so a new special case is added as a new option rather than by widening a
// constructor or growing an interface.

// multiSessionSource is the engine-visible Opaque payload for a container
// source. MemberID == "" means the source is the whole container (fan out every
// member on parse); a non-empty MemberID identifies a single member.
type multiSessionSource struct {
	Root      string
	Path      string
	Container string
	MemberID  string
}

// multiSessionMatch is what a classifier or member lookup resolves to: the
// canonical source path (a container path or a virtual member path) plus the
// physical container and, for a member, its ID. ProjectHint is surfaced on the
// SourceRef for providers that attribute a project at discovery time.
type multiSessionMatch struct {
	Path        string
	Container   string
	MemberID    string
	ProjectHint string
}

type multiSessionConfig struct {
	// discoverContainers returns the physical container paths under one root;
	// each becomes a whole-container source that fans out on parse.
	discoverContainers func(root string) []string
	// discoverSources returns fully-formed matches under one root, for providers
	// that surface individual members (or a mix of members and containers) at
	// discovery time rather than one source per container. Mutually exclusive
	// with discoverContainers.
	discoverSources func(root string) []multiSessionMatch
	// watchRoots returns the provider WatchPlan roots for the configured roots.
	watchRoots func(roots []string) []WatchRoot
	// classifyPath maps a stored or changed path to its container/member.
	// allowMissing relaxes existence checks so a deleted container (or a sibling
	// such as a SQLite WAL file) still classifies for changed-path tombstones.
	classifyPath func(root, path string, allowMissing bool) (multiSessionMatch, bool)
	// findMember resolves a raw session ID to its member match under one root.
	findMember func(root, rawID string) (multiSessionMatch, bool)
	// storedPathFallback resolves a stored path that classifyPath could not
	// match directly (for example a canonical remote-sync path that must be
	// mapped back onto a local container). Optional.
	storedPathFallback func(root, path string) (multiSessionMatch, bool)
	// fingerprint returns the source freshness fingerprint (Size/MTime/Hash);
	// the base supplies the Key.
	fingerprint func(src multiSessionSource) (SourceFingerprint, error)
	// parseContainer parses every member of a container into one result each.
	// The full ParseRequest is passed so a closure can read req.Machine and
	// per-request hints such as req.Source.ProjectHint.
	parseContainer func(src multiSessionSource, req ParseRequest) ([]ParseResult, error)
	// parseMember parses a single member; a nil result is a clean no-session.
	parseMember func(src multiSessionSource, req ParseRequest) (*ParseResult, error)
	// memberPresent reports whether a source still exists for RequireFreshSource
	// lookups. Optional; the default treats every source as present.
	memberPresent func(src multiSessionSource) bool
	// stampContainerHash stamps the request fingerprint hash onto every fanned
	// out container result (used when all members share the container's content
	// hash). Member parses are always stamped.
	stampContainerHash bool
}

type multiSessionOption func(*multiSessionConfig)

func withContainerDiscovery(fn func(root string) []string) multiSessionOption {
	return func(c *multiSessionConfig) { c.discoverContainers = fn }
}

func withSourceDiscovery(
	fn func(root string) []multiSessionMatch,
) multiSessionOption {
	return func(c *multiSessionConfig) { c.discoverSources = fn }
}

func withWatchRoots(fn func(roots []string) []WatchRoot) multiSessionOption {
	return func(c *multiSessionConfig) { c.watchRoots = fn }
}

func withChangedPathClassifier(
	fn func(root, path string, allowMissing bool) (multiSessionMatch, bool),
) multiSessionOption {
	return func(c *multiSessionConfig) { c.classifyPath = fn }
}

func withMemberLookup(
	fn func(root, rawID string) (multiSessionMatch, bool),
) multiSessionOption {
	return func(c *multiSessionConfig) { c.findMember = fn }
}

func withStoredPathFallback(
	fn func(root, path string) (multiSessionMatch, bool),
) multiSessionOption {
	return func(c *multiSessionConfig) { c.storedPathFallback = fn }
}

func withFingerprint(
	fn func(src multiSessionSource) (SourceFingerprint, error),
) multiSessionOption {
	return func(c *multiSessionConfig) { c.fingerprint = fn }
}

func withContainerParse(
	fn func(src multiSessionSource, req ParseRequest) ([]ParseResult, error),
) multiSessionOption {
	return func(c *multiSessionConfig) { c.parseContainer = fn }
}

func withMemberParse(
	fn func(src multiSessionSource, req ParseRequest) (*ParseResult, error),
) multiSessionOption {
	return func(c *multiSessionConfig) { c.parseMember = fn }
}

func withMemberPresence(fn func(src multiSessionSource) bool) multiSessionOption {
	return func(c *multiSessionConfig) { c.memberPresent = fn }
}

func withContainerHashStamping() multiSessionOption {
	return func(c *multiSessionConfig) { c.stampContainerHash = true }
}

func newMultiSessionContainerSourceSet(
	agent AgentType,
	roots []string,
	opts ...multiSessionOption,
) multiSessionContainerSourceSet {
	cfg := multiSessionConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}
	switch {
	case cfg.discoverContainers == nil && cfg.discoverSources == nil:
		panic("multi-session container: missing withContainerDiscovery or withSourceDiscovery")
	case cfg.watchRoots == nil:
		panic("multi-session container: missing withWatchRoots")
	case cfg.classifyPath == nil:
		panic("multi-session container: missing withChangedPathClassifier")
	case cfg.findMember == nil:
		panic("multi-session container: missing withMemberLookup")
	case cfg.fingerprint == nil:
		panic("multi-session container: missing withFingerprint")
	case cfg.parseContainer == nil:
		panic("multi-session container: missing withContainerParse")
	case cfg.parseMember == nil:
		panic("multi-session container: missing withMemberParse")
	}
	return multiSessionContainerSourceSet{
		agent: agent,
		roots: cleanJSONLRoots(roots),
		cfg:   cfg,
	}
}

type multiSessionContainerSourceSet struct {
	agent AgentType
	roots []string
	cfg   multiSessionConfig
}

func (s multiSessionContainerSourceSet) Discover(
	ctx context.Context,
) ([]SourceRef, error) {
	var sources []SourceRef
	seen := make(map[string]struct{})
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, match := range s.discoverMatches(root) {
			if match.Path == "" {
				continue
			}
			addJSONLSource(s.sourceRef(root, match), &sources, seen)
		}
	}
	sortJSONLSources(sources)
	return sources, nil
}

// discoverMatches yields the discovery matches for one root: either the
// member-level matches from withSourceDiscovery, or one whole-container match
// per path from withContainerDiscovery.
func (s multiSessionContainerSourceSet) discoverMatches(
	root string,
) []multiSessionMatch {
	if s.cfg.discoverSources != nil {
		return s.cfg.discoverSources(root)
	}
	containers := s.cfg.discoverContainers(root)
	out := make([]multiSessionMatch, 0, len(containers))
	for _, container := range containers {
		if container == "" {
			continue
		}
		out = append(out, multiSessionMatch{Path: container, Container: container})
	}
	return out
}

func (s multiSessionContainerSourceSet) WatchPlan(
	context.Context,
) (WatchPlan, error) {
	return WatchPlan{Roots: s.cfg.watchRoots(s.roots)}, nil
}

func (s multiSessionContainerSourceSet) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, root := range s.roots {
		if match, ok := s.cfg.classifyPath(root, req.Path, true); ok {
			return []SourceRef{s.sourceRef(root, match)}, nil
		}
	}
	return nil, nil
}

func (s multiSessionContainerSourceSet) FindSource(
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
			match, ok := s.cfg.classifyPath(root, path, false)
			if !ok {
				continue
			}
			source := s.sourceRef(root, match)
			if req.RequireFreshSource && !s.memberPresent(match.toSource(root)) {
				continue
			}
			return source, true, nil
		}
		if s.cfg.storedPathFallback != nil {
			for _, root := range s.roots {
				if match, ok := s.cfg.storedPathFallback(root, path); ok {
					return s.sourceRef(root, match), true, nil
				}
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
		if match, ok := s.cfg.findMember(root, req.RawSessionID); ok {
			return s.sourceRef(root, match), true, nil
		}
	}
	return SourceRef{}, false, nil
}

func (s multiSessionContainerSourceSet) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	if err := ctx.Err(); err != nil {
		return SourceFingerprint{}, err
	}
	src, ok := s.sourceFromRef(source)
	if !ok {
		return SourceFingerprint{}, fmt.Errorf("%s source path unavailable", s.agent)
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

func (s multiSessionContainerSourceSet) parse(
	src multiSessionSource, req ParseRequest,
) (ParseOutcome, error) {
	fingerprintHash := req.Fingerprint.Hash
	if src.MemberID != "" {
		result, err := s.cfg.parseMember(src, req)
		if err != nil {
			return ParseOutcome{}, err
		}
		if result == nil {
			return multiSessionSkipOutcome(), nil
		}
		if fingerprintHash != "" {
			result.Session.File.Hash = fingerprintHash
		}
		return ParseOutcome{
			Results: []ParseResultOutcome{{
				Result:      *result,
				DataVersion: DataVersionCurrent,
			}},
			ResultSetComplete: true,
			ForceReplace:      true,
		}, nil
	}

	results, err := s.cfg.parseContainer(src, req)
	if err != nil {
		return ParseOutcome{}, err
	}
	if len(results) == 0 {
		return multiSessionSkipOutcome(), nil
	}
	out := make([]ParseResultOutcome, 0, len(results))
	for i := range results {
		if fingerprintHash != "" && s.cfg.stampContainerHash {
			results[i].Session.File.Hash = fingerprintHash
		}
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

func multiSessionSkipOutcome() ParseOutcome {
	return ParseOutcome{
		ResultSetComplete: true,
		ForceReplace:      true,
		SkipReason:        SkipNoSession,
	}
}

func (s multiSessionContainerSourceSet) memberPresent(src multiSessionSource) bool {
	if s.cfg.memberPresent == nil {
		return true
	}
	return s.cfg.memberPresent(src)
}

func (s multiSessionContainerSourceSet) sourceRef(
	root string, match multiSessionMatch,
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

func (m multiSessionMatch) toSource(root string) multiSessionSource {
	return multiSessionSource{
		Root:      root,
		Path:      m.Path,
		Container: m.Container,
		MemberID:  m.MemberID,
	}
}

func (s multiSessionContainerSourceSet) sourceFromRef(
	source SourceRef,
) (multiSessionSource, bool) {
	switch src := source.Opaque.(type) {
	case multiSessionSource:
		return src, src.Container != "" && src.Path != ""
	case *multiSessionSource:
		if src != nil && src.Container != "" && src.Path != "" {
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
	return multiSessionSource{}, false
}

var _ SourceSet = multiSessionContainerSourceSet{}

// Parse resolves the request's source and parses it: a member source yields one
// result, a container source fans out every member. It satisfies the SourceSet
// interface; sourceSetProvider applies the request/config machine fallback
// before calling in, so req.Machine is already resolved here.
func (s multiSessionContainerSourceSet) Parse(
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
	return s.parse(src, req)
}

// newMultiSessionProviderFactory builds a ProviderFactory for a multi-session
// container provider. It is a thin adapter over the generic sourceSetFactory;
// the build closure constructs the agent's configured source set.
func newMultiSessionProviderFactory(
	def AgentDef,
	caps Capabilities,
	build func(cfg ProviderConfig) multiSessionContainerSourceSet,
) ProviderFactory {
	return newSourceSetFactory(
		def, caps,
		func(cfg ProviderConfig) SourceSet { return build(cfg) },
	)
}
