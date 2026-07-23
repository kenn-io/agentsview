package parser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	Path                   string
	Container              string
	MemberID               string
	ReconciliationIdentity string
	ProjectHint            string
	DiscoveryMTimeNS       int64
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
	discoverEach    func(context.Context, string, func(multiSessionMatch) error) error
	// watchRoots returns the provider WatchPlan roots for the configured roots.
	watchRoots func(roots []string) []WatchRoot
	// classifyPath maps a stored or changed path to its container/member.
	// allowMissing relaxes existence checks so a deleted container (or a sibling
	// such as a SQLite WAL file) still classifies for changed-path tombstones.
	classifyPath func(root, path string, allowMissing bool) (multiSessionMatch, bool)
	// classifyChangedPath optionally resolves one filesystem event directly to
	// the affected members. Providers use this when a shared container can
	// identify a bounded changed batch without reparsing every member.
	classifyChangedPath func(
		context.Context, string, ChangedPathRequest,
	) ([]multiSessionMatch, error)
	// findMember resolves a raw session ID to its member match under one root.
	findMember func(root, rawID string) (multiSessionMatch, bool)
	// reconciliationIdentity restores the stable SourceRef key for an exact
	// member reconstructed from its stored path. Optional; positional sources
	// use the reconciliation cache to avoid rescanning their container.
	reconciliationIdentity func(context.Context, multiSessionMatch) (string, error)
	// storedReconciliationIdentity derives the matching identity from a stored
	// full session ID. It is paired with reconciliationIdentity so the engine
	// does not need provider-specific identity policy.
	storedReconciliationIdentity func(string) string
	// storedPathFallback resolves a stored path that classifyPath could not
	// match directly (for example a canonical remote-sync path that must be
	// mapped back onto a local container). Optional.
	storedPathFallback func(root, path string) (multiSessionMatch, bool)
	// fingerprint returns the source freshness fingerprint (Size/MTime/Hash);
	// the base supplies the Key.
	fingerprint        func(src multiSessionSource) (SourceFingerprint, error)
	fingerprintContext func(context.Context, multiSessionSource) (SourceFingerprint, error)
	// parseContainerOutcome optionally builds the full container outcome
	// directly, for providers that need container-level completeness or
	// no-session semantics beyond a flat []ParseResult.
	parseContainerOutcome func(src multiSessionSource, req ParseRequest) (ParseOutcome, error)
	// parseContainer parses every member of a container into one result each.
	// The full ParseRequest is passed so a closure can read req.Machine and
	// per-request hints such as req.Source.ProjectHint.
	parseContainer func(src multiSessionSource, req ParseRequest) ([]ParseResult, error)
	// parseMember parses a single member; a nil result is a clean no-session.
	parseMember        func(src multiSessionSource, req ParseRequest) (*ParseResult, error)
	parseMemberContext func(context.Context, multiSessionSource, ParseRequest) (*ParseResult, error)
	// memberPresent reports whether a source still exists for RequireFreshSource
	// lookups. Optional; the default treats every source as present.
	memberPresent func(src multiSessionSource) bool
	// batchMemberPresence reports current membership for the stored members of one
	// changed container during a single tombstone pass. Optional; when nil the
	// base falls back to calling memberPresent per member.
	batchMemberPresence func(container multiSessionSource, members []multiSessionSource) map[string]bool
	// freshStoredMember reports whether a stored member source still resolves to
	// the requested raw session ID under RequireFreshSource. Providers with
	// positional member IDs (Aider's run index) set this so a stored path whose
	// index now points at a different run is rejected and re-resolved by raw ID.
	// Optional; when nil only memberPresent gates freshness.
	freshStoredMember func(src multiSessionSource, rawID string) bool
	// stampContainerHash stamps the request fingerprint hash onto every fanned
	// out container result (used when all members share the container's content
	// hash). Member parses are stamped unless preserveMemberHash is set.
	stampContainerHash bool
	// preserveMemberHash keeps a semantic hash produced by parseMember instead
	// of replacing it with the cheaper source-classification fingerprint.
	preserveMemberHash bool
	// unsupportedSource identifies typed parse errors that mean the physical
	// source is valid but its schema is intentionally unsupported.
	unsupportedSource func(error) bool
	// excludedSessionIDs reconciles legacy persisted identities replaced by the
	// current parse result set.
	excludedSessionIDs func(src multiSessionSource, results []ParseResult) []string
}

type MultiSessionOption func(*multiSessionConfig)

func multiSessionContainerSourceCapabilities(
	compositeFingerprint CapabilitySupport,
	storedSourceHints CapabilitySupport,
) SourceCapabilities {
	return SourceCapabilities{
		DiscoverSources:       CapabilitySupported,
		StreamingDiscovery:    CapabilitySupported,
		WatchSources:          CapabilitySupported,
		ClassifyChangedPath:   CapabilitySupported,
		StoredSourceHints:     storedSourceHints,
		FindSource:            CapabilitySupported,
		CompositeFingerprint:  compositeFingerprint,
		IncrementalAppend:     CapabilityNotApplicable,
		MultiSessionSource:    CapabilitySupported,
		SharedContainerSource: CapabilitySupported,
		PerSessionErrors:      CapabilityNotApplicable,
		ExcludedSessions:      CapabilityNotApplicable,
		ForceReplaceOnParse:   CapabilitySupported,
	}
}

func WithContainerDiscovery(fn func(root string) []string) MultiSessionOption {
	return func(c *multiSessionConfig) { c.discoverContainers = fn }
}

func WithSourceDiscovery(
	fn func(root string) []multiSessionMatch,
) MultiSessionOption {
	return func(c *multiSessionConfig) { c.discoverSources = fn }
}

func WithStreamingSourceDiscovery(
	fn func(context.Context, string, func(multiSessionMatch) error) error,
) MultiSessionOption {
	return func(c *multiSessionConfig) { c.discoverEach = fn }
}

func WithWatchRoots(fn func(roots []string) []WatchRoot) MultiSessionOption {
	return func(c *multiSessionConfig) { c.watchRoots = fn }
}

func WithChangedPathClassifier(
	fn func(root, path string, allowMissing bool) (multiSessionMatch, bool),
) MultiSessionOption {
	return func(c *multiSessionConfig) { c.classifyPath = fn }
}

func WithChangedPathMembers(
	fn func(
		context.Context, string, ChangedPathRequest,
	) ([]multiSessionMatch, error),
) MultiSessionOption {
	return func(c *multiSessionConfig) { c.classifyChangedPath = fn }
}

func WithMemberLookup(
	fn func(root, rawID string) (multiSessionMatch, bool),
) MultiSessionOption {
	return func(c *multiSessionConfig) { c.findMember = fn }
}

func WithReconciliationIdentity(
	fn func(context.Context, multiSessionMatch) (string, error),
	stored func(string) string,
) MultiSessionOption {
	return func(c *multiSessionConfig) {
		c.reconciliationIdentity = fn
		c.storedReconciliationIdentity = stored
	}
}

func WithStoredPathFallback(
	fn func(root, path string) (multiSessionMatch, bool),
) MultiSessionOption {
	return func(c *multiSessionConfig) { c.storedPathFallback = fn }
}

func WithFingerprint(
	fn func(src multiSessionSource) (SourceFingerprint, error),
) MultiSessionOption {
	return func(c *multiSessionConfig) { c.fingerprint = fn }
}

func WithContextFingerprint(
	fn func(context.Context, multiSessionSource) (SourceFingerprint, error),
) MultiSessionOption {
	return func(c *multiSessionConfig) { c.fingerprintContext = fn }
}

func WithContainerParse(
	fn func(src multiSessionSource, req ParseRequest) ([]ParseResult, error),
) MultiSessionOption {
	return func(c *multiSessionConfig) { c.parseContainer = fn }
}

func WithContainerParseOutcome(
	fn func(src multiSessionSource, req ParseRequest) (ParseOutcome, error),
) MultiSessionOption {
	return func(c *multiSessionConfig) { c.parseContainerOutcome = fn }
}

func WithMemberParse(
	fn func(src multiSessionSource, req ParseRequest) (*ParseResult, error),
) MultiSessionOption {
	return func(c *multiSessionConfig) { c.parseMember = fn }
}

func WithContextMemberParse(
	fn func(context.Context, multiSessionSource, ParseRequest) (*ParseResult, error),
) MultiSessionOption {
	return func(c *multiSessionConfig) { c.parseMemberContext = fn }
}

func WithMemberPresence(fn func(src multiSessionSource) bool) MultiSessionOption {
	return func(c *multiSessionConfig) { c.memberPresent = fn }
}

func WithBatchMemberPresence(
	fn func(container multiSessionSource, members []multiSessionSource) map[string]bool,
) MultiSessionOption {
	return func(c *multiSessionConfig) { c.batchMemberPresence = fn }
}

func WithFreshStoredMember(
	fn func(src multiSessionSource, rawID string) bool,
) MultiSessionOption {
	return func(c *multiSessionConfig) { c.freshStoredMember = fn }
}

func WithContainerHashStamping() MultiSessionOption {
	return func(c *multiSessionConfig) { c.stampContainerHash = true }
}

func WithMemberResultHashPreservation() MultiSessionOption {
	return func(c *multiSessionConfig) { c.preserveMemberHash = true }
}

func WithUnsupportedSourceError(fn func(error) bool) MultiSessionOption {
	return func(c *multiSessionConfig) { c.unsupportedSource = fn }
}

func WithExcludedSessionIDs(
	fn func(src multiSessionSource, results []ParseResult) []string,
) MultiSessionOption {
	return func(c *multiSessionConfig) { c.excludedSessionIDs = fn }
}

func NewMultiSessionContainerSourceSet(
	agent AgentType,
	roots []string,
	opts ...MultiSessionOption,
) multiSessionContainerSourceSet {
	cfg := multiSessionConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}
	switch {
	case cfg.discoverContainers == nil && cfg.discoverSources == nil && cfg.discoverEach == nil:
		panic("multi-session container: missing WithContainerDiscovery or WithSourceDiscovery")
	case cfg.watchRoots == nil:
		panic("multi-session container: missing WithWatchRoots")
	case cfg.classifyPath == nil:
		panic("multi-session container: missing WithChangedPathClassifier")
	case cfg.findMember == nil:
		panic("multi-session container: missing WithMemberLookup")
	case cfg.fingerprint == nil && cfg.fingerprintContext == nil:
		panic("multi-session container: missing WithFingerprint")
	case cfg.parseContainer == nil && cfg.parseContainerOutcome == nil:
		panic("multi-session container: missing WithContainerParse or WithContainerParseOutcome")
	case cfg.parseMember == nil && cfg.parseMemberContext == nil:
		panic("multi-session container: missing WithMemberParse")
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
			if match.Path != "" {
				addJSONLSource(s.sourceRef(root, match), &sources, seen)
			}
		}
	}
	sortJSONLSources(sources)
	return sources, nil
}

func (s multiSessionContainerSourceSet) DiscoverEach(
	ctx context.Context, yield func(SourceRef) error,
) error {
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.cfg.discoverEach != nil {
			if err := s.cfg.discoverEach(ctx, root, func(match multiSessionMatch) error {
				if match.Path == "" {
					return nil
				}
				return yield(s.sourceRef(root, match))
			}); err != nil {
				return err
			}
			continue
		}
		for _, match := range s.discoverMatches(root) {
			if match.Path == "" {
				continue
			}
			if err := yield(s.sourceRef(root, match)); err != nil {
				return err
			}
		}
	}
	return nil
}

// discoverMatches yields the discovery matches for one root: either the
// member-level matches from WithSourceDiscovery, or one whole-container match
// per path from WithContainerDiscovery.
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
		if match, ok := s.cfg.classifyPath(root, container, false); ok {
			out = append(out, match)
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
		if s.cfg.classifyChangedPath != nil {
			matches, err := s.cfg.classifyChangedPath(ctx, root, req)
			if err != nil {
				return nil, err
			}
			if len(matches) == 0 {
				continue
			}
			sources := make([]SourceRef, 0, len(matches))
			for _, match := range matches {
				sources = append(sources, s.sourceRef(root, match))
			}
			return sources, nil
		}
		match, ok := s.cfg.classifyPath(root, req.Path, true)
		if !ok {
			continue
		}
		tombstones := s.changedPathTombstones(root, match, req.StoredSourcePaths)
		sources := make([]SourceRef, 0, 1+len(tombstones))
		if req.EventKind != "remove" ||
			len(tombstones) == 0 ||
			IsRegularFile(match.Container) {
			sources = append(sources, s.sourceRef(root, match))
		}
		sources = append(sources, tombstones...)
		return sources, nil
	}
	return nil, nil
}

func (s multiSessionContainerSourceSet) StoredSourceHintScopes(
	req ChangedPathRequest,
) []StoredSourceHintScope {
	for _, root := range s.roots {
		match, ok := s.cfg.classifyPath(root, req.Path, true)
		if !ok || match.Container == "" {
			continue
		}
		if match.MemberID != "" && match.Path != "" {
			return []StoredSourceHintScope{{Path: match.Path}}
		}
		return []StoredSourceHintScope{{
			Path: match.Container, IncludeVirtualMembers: true,
		}}
	}
	return nil
}

func (s multiSessionContainerSourceSet) SourceForReconciliation(
	ctx context.Context, path, project string,
) (SourceRef, bool, error) {
	if err := ctx.Err(); err != nil {
		return SourceRef{}, false, err
	}
	for _, root := range s.roots {
		match, ok := s.cfg.classifyPath(root, path, false)
		if !ok {
			continue
		}
		if project != "" {
			match.ProjectHint = project
		}
		if s.cfg.reconciliationIdentity != nil {
			identity, err := s.cfg.reconciliationIdentity(ctx, match)
			if err != nil {
				return SourceRef{}, false, err
			}
			match.ReconciliationIdentity = identity
		}
		return s.sourceRef(root, match), true, nil
	}
	return SourceRef{}, false, nil
}

func (s multiSessionContainerSourceSet) PersistentArchiveSource(
	path string, fullSessionID string,
) (string, bool) {
	def, ok := AgentByType(s.agent)
	if !ok {
		return "", false
	}
	rawSessionID := ProviderRawSessionIDFromFull(def, fullSessionID)
	for _, root := range s.roots {
		match, ok := s.cfg.classifyPath(root, path, true)
		if ok && match.MemberID != "" && match.MemberID == rawSessionID &&
			match.Container != "" {
			return match.Container, true
		}
	}
	return "", false
}

func (s multiSessionContainerSourceSet) ReconciliationMemberIdentity(
	fullSessionID string,
) string {
	if s.cfg.storedReconciliationIdentity == nil {
		return ""
	}
	return s.cfg.storedReconciliationIdentity(fullSessionID)
}

// changedPathTombstones emits a per-member source for every stored member that
// belongs to the changed container, is no longer present, yet whose container
// file still exists. The whole-container source re-writes the surviving
// members; without these, a member row deleted from a still-present container
// is never force-replaced and a stale session lingers, diverging from the
// db-backed providers that drop it. A vanished shared container still yields no
// tombstones, preserving the archive when the whole source file disappears; a
// vanished one-file-per-member container emits the stored member tombstone so
// the deleted row is force-replaced instead of lingering forever.
func (s multiSessionContainerSourceSet) changedPathTombstones(
	root string,
	changed multiSessionMatch,
	storedPaths []string,
) []SourceRef {
	if changed.MemberID != "" || changed.Container == "" {
		return nil
	}
	containerExists := IsRegularFile(changed.Container)
	var tombstones []SourceRef
	seen := make(map[string]struct{})
	var members []multiSessionMatch
	for _, stored := range storedPaths {
		match, ok := s.cfg.classifyPath(root, stored, true)
		if !ok || match.MemberID == "" {
			continue
		}
		if !samePath(match.Container, changed.Container) {
			continue
		}
		if !containerExists {
			if !multiSessionMatchOwnsContainer(match) {
				continue
			}
			if current, ok := s.cfg.findMember(root, match.MemberID); ok &&
				!samePath(current.Container, match.Container) {
				if _, dup := seen[current.Path]; dup {
					continue
				}
				seen[current.Path] = struct{}{}
				tombstones = append(tombstones, s.sourceRef(root, current))
				continue
			}
			if _, dup := seen[match.Path]; dup {
				continue
			}
			seen[match.Path] = struct{}{}
			tombstones = append(tombstones, s.sourceRef(root, match))
			continue
		}
		members = append(members, match)
	}
	present := s.batchMemberPresence(changed.toSource(root), members)
	for _, match := range members {
		if s.memberPresentForTombstone(match.toSource(root), present) {
			continue
		}
		if _, dup := seen[match.Path]; dup {
			continue
		}
		seen[match.Path] = struct{}{}
		tombstones = append(tombstones, s.sourceRef(root, match))
	}
	return tombstones
}

func (s multiSessionContainerSourceSet) batchMemberPresence(
	container multiSessionSource,
	members []multiSessionMatch,
) map[string]bool {
	if s.cfg.batchMemberPresence == nil || len(members) == 0 {
		return nil
	}
	batch := make([]multiSessionSource, 0, len(members))
	for _, member := range members {
		batch = append(batch, member.toSource(container.Root))
	}
	return s.cfg.batchMemberPresence(container, batch)
}

func (s multiSessionContainerSourceSet) memberPresentForTombstone(
	src multiSessionSource,
	present map[string]bool,
) bool {
	if present != nil {
		if ok, found := present[src.Path]; found {
			return ok
		}
	}
	return s.memberPresent(src)
}

func multiSessionMatchOwnsContainer(
	match multiSessionMatch,
) bool {
	return multiSessionMemberOwnsContainer(match.MemberID, match.Container)
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
			memberSrc := match.toSource(root)
			if req.RequireFreshSource && !s.memberPresent(memberSrc) {
				continue
			}
			if s.staleStoredMember(memberSrc, req) {
				continue
			}
			return s.sourceRef(root, match), true, nil
		}
		if s.cfg.storedPathFallback != nil {
			for _, root := range s.roots {
				if match, ok := s.cfg.storedPathFallback(root, path); ok {
					if s.staleStoredMember(match.toSource(root), req) {
						continue
					}
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
	var fingerprint SourceFingerprint
	var err error
	if s.cfg.fingerprintContext != nil {
		fingerprint, err = s.cfg.fingerprintContext(ctx, src)
	} else {
		fingerprint, err = s.cfg.fingerprint(src)
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) &&
			multiSessionSourceOwnsContainer(src) {
			fingerprint = SourceFingerprint{}
		} else {
			return SourceFingerprint{}, err
		}
	}
	fingerprint.Key = firstNonEmptyJSONLString(
		source.FingerprintKey, source.Key, src.Path,
	)
	return fingerprint, nil
}

func (s multiSessionContainerSourceSet) parse(
	ctx context.Context, src multiSessionSource, req ParseRequest,
) (ParseOutcome, error) {
	fingerprintHash := req.Fingerprint.Hash
	if src.MemberID != "" {
		var result *ParseResult
		var err error
		if s.cfg.parseMemberContext != nil {
			result, err = s.cfg.parseMemberContext(ctx, src, req)
		} else {
			result, err = s.cfg.parseMember(src, req)
		}
		if err != nil {
			if s.cfg.unsupportedSource != nil && s.cfg.unsupportedSource(err) {
				return unsupportedMultiSessionOutcome(), nil
			}
			return ParseOutcome{}, err
		}
		if result == nil {
			return s.skipOutcome(src), nil
		}
		if fingerprintHash != "" && !s.cfg.preserveMemberHash {
			result.Session.File.Hash = fingerprintHash
		}
		return ParseOutcome{
			Results: []ParseResultOutcome{{
				Result:      *result,
				DataVersion: DataVersionCurrent,
			}},
			ExcludedSessionIDs: s.excludedSessionIDs(src, []ParseResult{*result}),
			ResultSetComplete:  true,
			ForceReplace:       true,
		}, nil
	}

	if s.cfg.parseContainerOutcome != nil {
		outcome, err := s.cfg.parseContainerOutcome(src, req)
		if err != nil {
			return ParseOutcome{}, err
		}
		if fingerprintHash != "" && s.cfg.stampContainerHash {
			for i := range outcome.Results {
				outcome.Results[i].Result.Session.File.Hash = fingerprintHash
			}
		}
		return outcome, nil
	}

	results, err := s.cfg.parseContainer(src, req)
	if err != nil {
		if s.cfg.unsupportedSource != nil && s.cfg.unsupportedSource(err) {
			return unsupportedMultiSessionOutcome(), nil
		}
		return ParseOutcome{}, err
	}
	if len(results) == 0 {
		return s.skipOutcome(src), nil
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
		Results:            out,
		ExcludedSessionIDs: s.excludedSessionIDs(src, results),
		ResultSetComplete:  true,
		ForceReplace:       true,
	}, nil
}

func (s multiSessionContainerSourceSet) excludedSessionIDs(
	src multiSessionSource, results []ParseResult,
) []string {
	if s.cfg.excludedSessionIDs == nil {
		return nil
	}
	return s.cfg.excludedSessionIDs(src, results)
}

func unsupportedMultiSessionOutcome() ParseOutcome {
	return ParseOutcome{
		ResultSetComplete: true,
		SkipReason:        SkipUnsupportedSource,
	}
}

// skipOutcome builds the "no session" outcome for a container/member that
// produced no results. When the backing container file is gone, whole-container
// sources preserve the stored sessions because the archive survives a vanished
// source file. Member tombstones for one-file-per-member layouts still
// force-replace so a deleted session row does not linger forever.
func (s multiSessionContainerSourceSet) skipOutcome(src multiSessionSource) ParseOutcome {
	if src.Container != "" && !IsRegularFile(src.Container) {
		if multiSessionSourceOwnsContainer(src) {
			return ParseOutcome{
				ResultSetComplete: true,
				ForceReplace:      true,
				SkipReason:        SkipNoSession,
			}
		}
		return ParseOutcome{
			ResultSetComplete: true,
			SkipReason:        SkipNoSession,
		}
	}
	return ParseOutcome{
		ResultSetComplete: true,
		ForceReplace:      true,
		SkipReason:        SkipNoSession,
	}
}

func multiSessionSourceOwnsContainer(src multiSessionSource) bool {
	return multiSessionMemberOwnsContainer(src.MemberID, src.Container)
}

func multiSessionMemberOwnsContainer(memberID, container string) bool {
	if memberID == "" {
		return false
	}
	base := filepath.Base(container)
	if memberID == base {
		return true
	}
	return isVisualStudioCopilotVS2026SessionID(memberID) &&
		isVisualStudioCopilotVS2026SessionID(base) &&
		strings.EqualFold(memberID, base)
}

func (s multiSessionContainerSourceSet) memberPresent(src multiSessionSource) bool {
	if s.cfg.memberPresent == nil {
		return true
	}
	return s.cfg.memberPresent(src)
}

// staleStoredMember reports whether a RequireFreshSource lookup must reject a
// stored member source because it no longer resolves to the requested raw
// session ID. It only fires when the provider supplies freshStoredMember and a
// raw ID is available, so providers with stable member IDs are unaffected.
func (s multiSessionContainerSourceSet) staleStoredMember(
	src multiSessionSource, req FindSourceRequest,
) bool {
	if !req.RequireFreshSource || req.RawSessionID == "" {
		return false
	}
	if s.cfg.freshStoredMember == nil {
		return false
	}
	return !s.cfg.freshStoredMember(src, req.RawSessionID)
}

func (s multiSessionContainerSourceSet) sourceRef(
	root string, match multiSessionMatch,
) SourceRef {
	return SourceRef{
		Provider:               s.agent,
		Key:                    match.Path,
		DisplayPath:            match.Path,
		FingerprintKey:         match.Path,
		ReconciliationIdentity: match.ReconciliationIdentity,
		ProjectHint:            match.ProjectHint,
		DiscoveryMTimeNS:       match.DiscoveryMTimeNS,
		Opaque:                 match.toSource(root),
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
// interface; SourceSetProvider applies the request/config machine fallback
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
	return s.parse(ctx, src, req)
}

// NewMultiSessionProviderFactory builds a ProviderFactory for a multi-session
// container provider. It is a thin adapter over the generic SourceSetFactory;
// the build closure constructs the agent's configured source set.
func NewMultiSessionProviderFactory(
	def AgentDef,
	caps Capabilities,
	build func(cfg ProviderConfig) multiSessionContainerSourceSet,
) ProviderFactory {
	return NewSourceSetFactory(
		def, caps,
		func(cfg ProviderConfig) SourceSet { return build(cfg) },
	)
}
