package parser

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	ProviderFeatureFingerprint          = "fingerprint"
	ProviderFeatureParse                = "parse"
	ProviderFeatureWatchRoots           = "watch roots"
	ProviderFeatureActivityHints        = "activity hints"
	ProviderFeatureChangedPathRelevance = "changed path relevance"
)

// ErrUnsupportedProviderFeature identifies optional provider behavior that is
// intentionally absent. Callers use errors.Is to distinguish this from I/O or
// parse failures.
var ErrUnsupportedProviderFeature = errors.New("unsupported provider feature")

// UnsupportedProviderFeatureError wraps ErrUnsupportedProviderFeature with the
// provider and feature names that produced it.
type UnsupportedProviderFeatureError struct {
	Provider AgentType
	Feature  string
}

func (err UnsupportedProviderFeatureError) Error() string {
	if err.Provider == "" {
		return "unsupported provider feature " + err.Feature
	}
	return string(err.Provider) + ": unsupported provider feature " + err.Feature
}

func (err UnsupportedProviderFeatureError) Unwrap() error {
	return ErrUnsupportedProviderFeature
}

// ProviderFactory is the registry surface for creating config-bound provider
// instances.
type ProviderFactory interface {
	Definition() AgentDef
	Capabilities() Capabilities
	NewProvider(ProviderConfig) Provider
}

// ProviderConfig is copied into a provider instance at construction time.
type ProviderConfig struct {
	Roots   []string
	Machine string
	// StableSourceSnapshots reports that source files cannot change during
	// parsing. Bounded capture enables it after copying quiescent transcripts
	// so providers can classify an invalid end-of-file record as durable.
	StableSourceSnapshots bool
	// SourceMachines labels configured roots with their source machine. A
	// provider must treat roots owned by another machine as remote metadata.
	SourceMachines map[string]string
	// PathRewriter maps an on-disk source path to its canonical stored form.
	// It is non-nil only during remote (SSH) sync, where source files are read
	// from a temporary extraction directory but must keep a stable identity
	// across syncs. Providers whose session IDs are derived from the source
	// path (Aider) use it to seed those IDs from the canonical remote path
	// rather than the changing temp path. Most providers ignore it.
	PathRewriter func(string) string
	// SQLiteContainerUnchangedSinceTrust reports that the shared SQLite
	// container at dbPath is byte-identical to the last pass that verified
	// every one of its sessions, as captured before this discovery began.
	// Providers that fan such a container out to per-session sources may
	// answer discovery for it with the bounded watermark-only listing: the
	// caller's container gate will skip every member before fingerprinting,
	// so computing the full child digest would be archive-sized work for
	// values nothing reads. Nil (the default, and every non-discovery
	// construction) means no container is trusted and listings stay
	// full-fidelity.
	SQLiteContainerUnchangedSinceTrust func(dbPath string) bool
}

// Clone returns an independent config snapshot.
func (cfg ProviderConfig) Clone() ProviderConfig {
	cfg.Roots = cfg.RootsCopy()
	cfg.SourceMachines = maps.Clone(cfg.SourceMachines)
	return cfg
}

// RootsCopy returns an independent roots slice.
func (cfg ProviderConfig) RootsCopy() []string {
	return append([]string(nil), cfg.Roots...)
}

// Provider is the target parser/source facade. Providers own source shape,
// source identity, freshness, and lookup; the engine consumes SourceRefs,
// SourceFingerprints, and normalized ParseResults without knowing whether the
// backing data is a file, virtual DB row, sidecar set, remote canonical path, or
// multi-session container.
type Provider interface {
	Definition() AgentDef
	Capabilities() Capabilities

	Discover(context.Context) ([]SourceRef, error)
	WatchPlan(context.Context) (WatchPlan, error)
	SourcesForChangedPath(context.Context, ChangedPathRequest) ([]SourceRef, error)
	FindSource(context.Context, FindSourceRequest) (SourceRef, bool, error)
	Fingerprint(context.Context, SourceRef) (SourceFingerprint, error)

	// Parse returns a normalized outcome for one logical source. A non-nil
	// error is a whole-source failure, including context cancellation; callers
	// must ignore the returned ParseOutcome. Partial multi-session success is
	// represented by a nil error with successful Results plus SourceErrors for
	// isolated per-session failures.
	Parse(context.Context, ParseRequest) (ParseOutcome, error)
	ParseIncremental(
		context.Context,
		IncrementalRequest,
	) (IncrementalOutcome, IncrementalStatus, error)

	// ResolveReconciliationScopes maps one reconciliation request onto the
	// provider's configured topology. The provider that owns the format is the
	// only authority for alias, container, and gateway relationships between
	// roots; callers consume the returned plan verbatim and never recompute
	// ancestor/descendant, filename, or alias relationships themselves. A
	// returned error means the topology could not be resolved and the request
	// must fail closed: no coverage credit and no deletion authority.
	ResolveReconciliationScopes(
		context.Context, ReconciliationScopeRequest,
	) (ReconciliationScopePlan, error)
}

// ReconciliationScopeRequest carries the reconciliation roots a caller asked
// about, exactly as supplied. Callers must not pre-expand or normalize them;
// the provider owns the mapping onto its configured topology.
type ReconciliationScopeRequest struct {
	Roots []string
}

// ReconciliationScope is one provider-resolved unit of reconciliation
// authority. Its four fields are consumed independently: TraversalRoots are
// the roots discovery must walk (a provider may require a wider gateway than
// the request, such as a Claude projects directory for a requested project
// child); PhysicalProofScopes bound which discovered sources and stored
// ownership rows the pass may claim; CoverageIdentities are credited against
// the plan's RequiredCoverageIdentities only once the scope's admitted stream
// completes; RetryRoots are the caller's own requested roots, so a failed
// scope retries at the width the caller asked for rather than the traversal
// width.
type ReconciliationScope struct {
	TraversalRoots      []string
	PhysicalProofScopes []StoredSourceHintScope
	CoverageIdentities  []string
	RetryRoots          []string
}

// ReconciliationScopePlan is the complete resolution of one request against
// one provider's configured topology. An empty plan means the request touches
// nothing the provider owns and the pass is a bounded no-op for it.
// RequiredCoverageIdentities enumerate every coverage identity the provider's
// full configured scope carries; a pass holds full-coverage deletion
// authority only when the completed scopes' CoverageIdentities include all of
// them.
type ReconciliationScopePlan struct {
	Scopes                     []ReconciliationScope
	RequiredCoverageIdentities []string
}

// ReconciliationSourceResolver rebuilds the exact source emitted by streaming
// discovery without routing the virtual path back through changed-path
// classification. Shared-container providers use it to avoid rescanning every
// member in the container once per spool page/member.
type ReconciliationSourceResolver interface {
	SourceForReconciliation(
		context.Context, string, string,
	) (SourceRef, bool, error)
}

// ReconciliationSourceRank is compared lexicographically after configured-root
// priority when authoritative discovery finds duplicate logical sources.
type ReconciliationSourceRank struct {
	Class   int64
	Recency int64
}

// ReconciliationSourceRanker declares provider-specific duplicate ordering so
// authoritative reconciliation selects the same source as FindSource.
type ReconciliationSourceRanker interface {
	ReconciliationSourceRank(SourceRef) ReconciliationSourceRank
}

// ReconciliationMemberIdentityResolver derives the stable logical-member
// identity stored in a full session ID. Providers whose virtual paths can be
// reused declare this alongside SourceRef.ReconciliationIdentity so the engine
// can validate both the path and the member during authoritative discovery.
type ReconciliationMemberIdentityResolver interface {
	ReconciliationMemberIdentity(fullSessionID string) string
}

// ReconciliationAggregateMemberResolver maps an ownership row whose stored
// FilePath is a still-present multi-member container to the source paths
// streamed discovery may emit for that member. Container discovery records
// the container path itself for every member, so a removed member never
// trips the missing-path stat; the engine verifies such rows against
// streamed membership explicitly and tombstones rows none of the candidate
// paths represent. Returning nil opts the row out of the check.
type ReconciliationAggregateMemberResolver interface {
	ReconciliationAggregateMemberPaths(
		filePath string, fullSessionID string,
	) []string
}

// PersistentArchiveSourceResolver validates that a stored source belongs to a
// provider-specific persistent container and returns its physical path. The
// engine must not infer this policy from provider-neutral virtual-path syntax:
// hybrid providers can also own ordinary files whose paths contain the same
// delimiter.
type PersistentArchiveSourceResolver interface {
	PersistentArchiveSource(
		path string, fullSessionID string,
	) (physicalPath string, ok bool)
}

// StoredSourceHintScope identifies one bounded stored-path prefix. Providers
// explicitly declare virtual-member ownership because filename shape cannot
// distinguish extensionless containers from ordinary paths.
type StoredSourceHintScope struct {
	Path                  string
	IncludeVirtualMembers bool
}

// StoredSourceHintScopeProvider derives the stored-source scopes that can
// affect one changed path without reading the archive database.
type StoredSourceHintScopeProvider interface {
	StoredSourceHintScopes(ChangedPathRequest) []StoredSourceHintScope
}

// MultiFileStatHasher is an optional capability for providers whose on-disk
// source layout spans multiple sibling files (Codebuff's
// chat-messages.json plus run-state.json and chat-meta.json in the same
// directory, for example). It computes a stable per-component stat digest
// that the engine's stat-only pre-check compares against a value persisted
// in the provider_freshness side-table so warm sync over an unchanged
// archive can short-circuit to zero provider.Fingerprint calls while still
// catching same-size companion rewrites, offsetting size deltas, and
// sibling-only directory mutations.
//
// Implementations live on the provider implementation; the engine type-
// asserts a constructed provider against MultiFileStatHasher and caches
// the result. Single-file providers whose Fingerprint content-hashes the
// source (Claude, Codex) also implement it: their digest folds the
// change-time term, so a persisted stat digest preserves in-place-rewrite
// detection across process restarts without re-reading unchanged content
// on every pass. Providers that implement neither behavior take the
// existing stat-only composite path.
type MultiFileStatHasher interface {
	// ComputeMultiFileStatHash stats the chat path plus any companion
	// siblings declared by the provider and returns a stable per-source
	// digest. The hasher performs its own os.Stat so the engine does not
	// need to supply FileInfo.
	ComputeMultiFileStatHash(chatPath string) uint64
}

// ProviderBase is embedded by concrete providers to make optional source
// methods callable with zero-value no-op behavior.
type ProviderBase struct {
	Def    AgentDef
	Caps   Capabilities
	Config ProviderConfig
}

func (b ProviderBase) Definition() AgentDef {
	return cloneAgentDef(b.Def)
}

func (b ProviderBase) Capabilities() Capabilities {
	return b.Caps
}

func (b ProviderBase) Discover(context.Context) ([]SourceRef, error) {
	return nil, nil
}

func (b ProviderBase) WatchPlan(context.Context) (WatchPlan, error) {
	return WatchPlan{}, nil
}

func (b ProviderBase) SourcesForChangedPath(
	context.Context,
	ChangedPathRequest,
) ([]SourceRef, error) {
	return nil, nil
}

func (b ProviderBase) FindSource(
	context.Context,
	FindSourceRequest,
) (SourceRef, bool, error) {
	return SourceRef{}, false, nil
}

func (b ProviderBase) Fingerprint(
	context.Context,
	SourceRef,
) (SourceFingerprint, error) {
	return SourceFingerprint{}, b.unsupported(ProviderFeatureFingerprint)
}

func (b ProviderBase) ParseIncremental(
	context.Context,
	IncrementalRequest,
) (IncrementalOutcome, IncrementalStatus, error) {
	return IncrementalOutcome{}, IncrementalUnsupported, nil
}

func (b ProviderBase) unsupported(feature string) error {
	return UnsupportedProviderFeatureError{
		Provider: b.Def.Type,
		Feature:  feature,
	}
}

// ResolveReconciliationScopes supplies the generic directory topology every
// provider inherits: each configured root is one atomic coverage unit, a
// requested descendant traverses from its deepest configured ancestor while
// proving only the descendant, and an ancestor request splits into the
// configured roots it covers without claiming unrelated paths beneath it.
func (b ProviderBase) ResolveReconciliationScopes(
	_ context.Context, req ReconciliationScopeRequest,
) (ReconciliationScopePlan, error) {
	if err := ValidateReconciliationScopeRoots(
		b.Def.Type, b.Config.Roots, req.Roots,
	); err != nil {
		return ReconciliationScopePlan{}, err
	}
	return directoryReconciliationScopePlan(b.Config.Roots, req.Roots), nil
}

// isRemoteReconciliationScopeRoot mirrors the sync engine's remote-root
// filter: remote object roots are owned by the remote sync path and resolve
// no local reconciliation scope.
func isRemoteReconciliationScopeRoot(root string) bool {
	return strings.HasPrefix(strings.ToLower(root), "s3://")
}

// ValidateReconciliationScopeRoots rejects a local root that cannot become a
// bounded proof scope. A scope proves itself by matching stored source paths
// against a path prefix, and discovery writes those paths by joining the
// configured spelling with each relative source path. A root that cleans to
// "." has no spelling to join: discovery emits bare filenames, so no prefix
// bounds them and the stored-hint normalizer discards the scope, which would
// credit coverage over zero rows. Every other spelling, relative or absolute,
// yields a usable prefix. Remote roots are exempt; a local pass never covers
// an object store.
func ValidateReconciliationScopeRoots(
	agent AgentType, configuredRoots, requestedRoots []string,
) error {
	for _, group := range [][]string{configuredRoots, requestedRoots} {
		for _, root := range group {
			if strings.TrimSpace(root) == "" ||
				isRemoteReconciliationScopeRoot(root) ||
				filepath.Clean(root) != "." {
				continue
			}
			return fmt.Errorf(
				"%s reconciliation root %q resolves to the working "+
					"directory: sources discovered under it are stored "+
					"without a directory prefix, so no reconciliation scope "+
					"can prove what it covers",
				agent, root,
			)
		}
	}
	return nil
}

// cleanReconciliationScopeRoot normalizes one local root for comparison and
// coverage identity. Blank roots must be discarded before this runs: cleaning
// "" resolves to the process working directory, which can encompass configured
// roots. This is deliberately not the spelling a scope proves itself with; see
// reconciliationProofSpelling.
func cleanReconciliationScopeRoot(root string) string {
	cleaned := filepath.Clean(root)
	abs, err := filepath.Abs(cleaned)
	if err != nil {
		return cleaned
	}
	return abs
}

// reconciliationProofSpelling is the spelling a scope proves itself with:
// the configured one, cleaned. Comparison and coverage identity absolutize so
// a request naming the same directory differently still matches, but proof
// matches stored source paths as a prefix, and those were written by joining
// the configured spelling. Absolutizing proof would leave a relative root's
// stored paths unreachable, and would rewrite a rooted-but-driveless root such
// as "/sessions" onto the current Windows drive, which discovery never used.
func reconciliationProofSpelling(root string) string {
	return filepath.Clean(root)
}

// reconciliationScopeSamePath and reconciliationScopeWithinOrSame compare with
// filepath.Rel semantics so equality matches the platform: case-folded per
// element on Windows, byte-exact elsewhere.
func reconciliationScopeSamePath(a, b string) bool {
	rel, err := filepath.Rel(a, b)
	return err == nil && rel == "."
}

func reconciliationScopeWithinOrSame(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || rel != ".." &&
		!strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// descendantProofSpelling re-expresses a requested descendant in the spelling
// its gateway's stored sources carry. Requests arrive absolute because they
// come from watch and polling roots, but a source discovered under a relative
// configured root was stored relative, so an absolute descendant prefix would
// match none of them.
func descendantProofSpelling(gatewayClean, gatewayProof, descendant string) string {
	rel, err := filepath.Rel(gatewayClean, descendant)
	if err != nil {
		return descendant
	}
	if rel == "." {
		return gatewayProof
	}
	return filepath.Join(gatewayProof, rel)
}

// reconciliationContainerTopology reports the multi-member container that
// atomically owns a requested path: the container file itself, a WAL or SHM
// sidecar, or a virtual member spelling. The planner widens such a request to
// the whole container so no scope can prove a strict subset of a container's
// members: a partial proof would either admit nothing (a bare container path
// matches no member row) or let a completed pass promote container-state
// trust over members it never verified. Classification must not stat, so a
// deleted container still resolves and its members stay reclaimable.
type reconciliationContainerTopology func(requested string) (container string, ok bool)

func directoryReconciliationScopePlan(
	configuredRoots, requestedRoots []string,
) ReconciliationScopePlan {
	return containerAwareReconciliationScopePlan(
		configuredRoots, requestedRoots, nil,
	)
}

func containerAwareReconciliationScopePlan(
	configuredRoots, requestedRoots []string,
	containers reconciliationContainerTopology,
) ReconciliationScopePlan {
	type configuredScopeRoot struct {
		// display is the configured spelling handed back for traversal so
		// discovery sees exactly the roots it was configured with.
		display string
		// clean is the absolute form used for comparison and coverage
		// identity; proof is the configured form stored sources carry.
		clean string
		proof string
	}
	var configured []configuredScopeRoot
	var required []string
	for _, root := range configuredRoots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		if isRemoteReconciliationScopeRoot(root) {
			// A remote root is never coverable by a local pass; keeping its
			// identity required makes full-coverage authority unreachable.
			required = append(required, root)
			continue
		}
		clean := cleanReconciliationScopeRoot(root)
		if slices.ContainsFunc(configured, func(existing configuredScopeRoot) bool {
			return reconciliationScopeSamePath(existing.clean, clean)
		}) {
			continue
		}
		configured = append(configured, configuredScopeRoot{
			display: root, clean: clean,
			proof: reconciliationProofSpelling(root),
		})
		required = append(required, clean)
	}
	plan := ReconciliationScopePlan{RequiredCoverageIdentities: required}
	fullIndex := make(map[string]int, len(configured))
	appendRetry := func(index int, raw string) {
		scope := &plan.Scopes[index]
		if !slices.Contains(scope.RetryRoots, raw) {
			scope.RetryRoots = append(scope.RetryRoots, raw)
		}
	}
	type descendantScope struct {
		gateway configuredScopeRoot
		proof   string
		virtual bool
		retry   []string
	}
	var descendants []descendantScope
	for _, raw := range requestedRoots {
		if strings.TrimSpace(raw) == "" || isRemoteReconciliationScopeRoot(raw) {
			continue
		}
		requested := cleanReconciliationScopeRoot(raw)
		virtual := false
		if containers != nil {
			if container, ok := containers(requested); ok {
				requested = cleanReconciliationScopeRoot(container)
				virtual = true
			}
		}
		exact := false
		for _, cfg := range configured {
			if !reconciliationScopeWithinOrSame(cfg.clean, requested) {
				continue
			}
			// The request names the configured root exactly or an ancestor
			// covering it: the whole configured root is one atomic scope.
			exact = exact || reconciliationScopeSamePath(cfg.clean, requested)
			if index, ok := fullIndex[cfg.clean]; ok {
				appendRetry(index, raw)
				continue
			}
			fullIndex[cfg.clean] = len(plan.Scopes)
			plan.Scopes = append(plan.Scopes, ReconciliationScope{
				TraversalRoots:      []string{cfg.display},
				PhysicalProofScopes: []StoredSourceHintScope{{Path: cfg.proof}},
				CoverageIdentities:  []string{cfg.clean},
				RetryRoots:          []string{raw},
			})
		}
		if exact {
			continue
		}
		// A requested descendant traverses from its deepest configured
		// ancestor (providers may treat the configured root as a layout
		// gateway) while proving only the descendant, so coverage stays
		// incomplete and no sibling is claimed. Covering some other
		// configured root as an ancestor above does not discharge this:
		// with roots /a and /a/b/c, a request for /a/b claims /a/b/c fully
		// and still proves /a/b under /a, or removals under /a/b would
		// silently stop reconciling. Only an exact configured-root match
		// makes a descendant scope redundant.
		var gateway *configuredScopeRoot
		for i, cfg := range configured {
			if reconciliationScopeSamePath(requested, cfg.clean) ||
				!reconciliationScopeWithinOrSame(requested, cfg.clean) {
				continue
			}
			if gateway == nil || reconciliationScopeWithinOrSame(
				cfg.clean, gateway.clean,
			) {
				gateway = &configured[i]
			}
		}
		if gateway == nil {
			continue
		}
		merged := false
		for i := range descendants {
			if reconciliationScopeSamePath(descendants[i].proof, requested) {
				if !slices.Contains(descendants[i].retry, raw) {
					descendants[i].retry = append(descendants[i].retry, raw)
				}
				descendants[i].virtual = descendants[i].virtual || virtual
				merged = true
				break
			}
		}
		if !merged {
			descendants = append(descendants, descendantScope{
				gateway: *gateway, proof: requested, virtual: virtual,
				retry: []string{raw},
			})
		}
	}
	for _, descendant := range descendants {
		gateway := descendant.gateway
		if index, ok := fullIndex[gateway.clean]; ok {
			// The same request set already claims the full gateway; the
			// descendant is subsumed and only contributes retry identities.
			for _, raw := range descendant.retry {
				appendRetry(index, raw)
			}
			continue
		}
		plan.Scopes = append(plan.Scopes, ReconciliationScope{
			TraversalRoots: []string{gateway.display},
			PhysicalProofScopes: []StoredSourceHintScope{{
				Path: descendantProofSpelling(gateway.clean, gateway.proof,
					descendant.proof),
				IncludeVirtualMembers: descendant.virtual,
			}},
			RetryRoots: descendant.retry,
		})
	}
	return plan
}

// SourceCwdState identifies the authority a provider has for a source's local
// working directory. Providers that do not participate leave the state at its
// zero value.
type SourceCwdState uint8

const (
	SourceCwdUnspecified SourceCwdState = iota
	SourceCwdResolved
	SourceCwdNone
	SourceCwdAmbiguous
	SourceCwdUnavailable
	SourceCwdRemote
)

// SourceCwdResolution carries optional, provider-derived Cwd metadata. Path is
// populated only for SourceCwdResolved; the other states describe what the
// provider knows without exposing provider-specific filesystem knowledge.
type SourceCwdResolution struct {
	State SourceCwdState
	Path  string
}

// SourceRef is the engine-visible handle for provider-owned source data. It is
// the only source identity the engine should carry between discovery, changed
// path classification, lookup, fingerprinting, parsing, skip-cache checks, and
// persisted session metadata.
type SourceRef struct {
	// Provider identifies the provider that created this source and must match
	// the provider instance used for subsequent operations.
	Provider AgentType
	// ConfiguredRoot preserves the configured root that produced this source.
	// It may differ from the canonical source path when a provider collapses a
	// directory onto a sibling container file.
	ConfiguredRoot string
	// Key is stable within the provider across process restarts. It is suitable
	// for dedupe and diagnostics, but not necessarily for DB freshness checks.
	Key string
	// DisplayPath is human-readable and may be a virtual path. For filesystem
	// sources it is usually the path users expect to inspect. For shared stores
	// it may be a provider virtual path such as "<db>#<sessionID>".
	DisplayPath string
	// FingerprintKey is the persisted lookup key for source metadata,
	// skip-cache, and parser data-version freshness checks. Providers should set
	// it to the same identity they store in ParsedSession.File.Path whenever
	// practical, and migrated providers must keep it compatible with legacy
	// file_path values unless a documented provider-specific transition handles
	// old rows.
	FingerprintKey string
	// ReconciliationIdentity is an optional stable logical-member identity used
	// with DisplayPath to distinguish virtual sources whose positional path can
	// later be reused by a different member. It is never persisted as source
	// metadata and is consumed only by authoritative discovery reconciliation.
	ReconciliationIdentity string
	// ProjectHint is advisory metadata for UI grouping and may be empty.
	ProjectHint string
	// CwdResolution is optional source metadata used by generic sync freshness
	// and write preparation. Its zero value preserves existing provider behavior.
	CwdResolution SourceCwdResolution
	// DiscoveryMTimeNS is an optional per-source modification time in Unix
	// nanoseconds captured at discovery. Providers whose sources are virtual --
	// a shared store fanned out to one source per session, where DisplayPath is
	// "<db>#<sessionID>" and os.Stat cannot resolve a real mtime -- set it so
	// ordering consumers (parse-diff's --limit sampler) can rank sources by each
	// session's real mtime instead of a failed stat that collapses to 0. Zero
	// means unset. It is advisory ordering metadata only: it is never persisted
	// and must not be used for skip-cache or data-version freshness, which go
	// through Fingerprint.
	DiscoveryMTimeNS int64
	// Opaque is in-memory-only source state: never persisted and never required
	// for lookup from persisted rows, so any source that must survive a restart
	// has to be recoverable from Key, DisplayPath, FingerprintKey,
	// FindSourceRequest, or discovery. It is normally provider-private, with one
	// defined exception -- a few engine-recognized payload types that the engine
	// may construct and type-assert to thread in-memory metadata across the
	// discovery/sync/parse boundary: S3DiscoveredSource (discovery -> sync object
	// metadata) and MaterializedFileSource (sync -> provider parse of a fetched
	// S3 temp file). Providers must not depend on either for persisted-row lookup.
	Opaque any
}

// WatchPlan describes provider-owned filesystem watch roots. Provider
// WatchPlans are authoritative for migrated providers; legacy AgentDef watch
// fields are fallback compatibility only.
type WatchPlan struct {
	Roots []WatchRoot
}

// WatchRootPlanner is the optional bounded scheduling surface for providers
// and source sets. Unlike WatchPlan, it returns only watcher root metadata and
// must not discover sources to derive parser-only include globs.
type WatchRootPlanner interface {
	WatchRoots(context.Context) ([]WatchRoot, error)
}

// ResolveWatchRoots returns the bounded root-planning capability when a
// provider advertises it. Providers that have not migrated yet retain their
// WatchPlan behavior, but parser-only include/exclude globs are never carried
// into watcher scheduling.
func ResolveWatchRoots(
	ctx context.Context,
	provider Provider,
) ([]WatchRoot, error) {
	if provider.Capabilities().Source.WatchRoots == CapabilitySupported {
		planner, ok := provider.(WatchRootPlanner)
		if !ok {
			return nil, UnsupportedProviderFeatureError{
				Provider: provider.Definition().Type,
				Feature:  ProviderFeatureWatchRoots,
			}
		}
		roots, err := planner.WatchRoots(ctx)
		if err != nil {
			return nil, err
		}
		return watchRootMetadata(roots), nil
	}
	plan, err := provider.WatchPlan(ctx)
	if err != nil {
		return nil, err
	}
	return watchRootMetadata(plan.Roots), nil
}

func watchRootMetadata(roots []WatchRoot) []WatchRoot {
	out := make([]WatchRoot, 0, len(roots))
	for _, root := range roots {
		out = append(out, WatchRoot{
			Path:        root.Path,
			Recursive:   root.Recursive,
			DebounceKey: root.DebounceKey,
		})
	}
	return out
}

// WatchRoot is one filesystem root the engine should watch. Recursive roots
// observe nested source creation. Non-recursive roots observe only direct child
// changes and must not be treated as covering missing nested provider roots
// unless caller-specific creation handling documents that equivalence.
type WatchRoot struct {
	Path         string
	Recursive    bool
	IncludeGlobs []string
	ExcludeGlobs []string
	DebounceKey  string
}

// ActivityHintSource is one bounded append-only signal a provider exposes to
// identify sessions with newly submitted activity.
type ActivityHintSource struct {
	Path string
}

// ActivityHint identifies one recently active provider-native session.
// Raw prompt content is intentionally absent from this cross-package contract.
type ActivityHint struct {
	RawSessionID string
	Timestamp    time.Time
}

// ActivityHintProvider owns activity-hint path derivation and record decoding.
// Scheduling, cursors, and retention remain sync-layer responsibilities.
type ActivityHintProvider interface {
	ActivityHintSources(context.Context) ([]ActivityHintSource, error)
	DecodeActivityHint([]byte) (ActivityHint, bool)
}

// ResolveActivityHintProvider returns the optional activity-hint contract only
// when the provider explicitly advertises it.
func ResolveActivityHintProvider(
	provider Provider,
) (ActivityHintProvider, bool, error) {
	if provider.Capabilities().Source.ActivityHints != CapabilitySupported {
		return nil, false, nil
	}
	hints, ok := provider.(ActivityHintProvider)
	if !ok {
		return nil, false, UnsupportedProviderFeatureError{
			Provider: provider.Definition().Type,
			Feature:  ProviderFeatureActivityHints,
		}
	}
	return hints, true, nil
}

// ChangedPathRelevance distinguishes paths that are known to carry no source
// data from paths that should keep the watch-triggered push active.
type ChangedPathRelevance uint8

const (
	ChangedPathUnclassified ChangedPathRelevance = iota
	ChangedPathNonData
	ChangedPathDataBearing
)

// ChangedPathRelevanceProvider owns the bounded, engine-free path decision
// needed by watch-trigger adapters.
type ChangedPathRelevanceProvider interface {
	ChangedPathRelevance(context.Context, ChangedPathRequest) (ChangedPathRelevance, error)
}

// ResolveChangedPathRelevance returns the optional path-relevance contract
// only when the provider explicitly advertises it.
func ResolveChangedPathRelevance(
	ctx context.Context,
	provider Provider,
	req ChangedPathRequest,
) (ChangedPathRelevance, error) {
	if provider.Capabilities().Source.ChangedPathRelevance != CapabilitySupported {
		return ChangedPathUnclassified, nil
	}
	relevance, ok := provider.(ChangedPathRelevanceProvider)
	if !ok {
		return ChangedPathUnclassified, UnsupportedProviderFeatureError{
			Provider: provider.Definition().Type,
			Feature:  ProviderFeatureChangedPathRelevance,
		}
	}
	return relevance.ChangedPathRelevance(ctx, req)
}

// ChangedPathRequest is passed back to providers for authoritative changed-path
// classification.
type ChangedPathRequest struct {
	Path      string
	EventKind string
	// WatchRoot is the provider WatchRoot that observed Path. Providers should
	// use it to scope classification and avoid returning sources from unrelated
	// configured roots that happen to match the same basename or raw ID.
	WatchRoot string
	// StoredSourcePaths are optional provider-persisted source paths already
	// known to the caller for this watch root. Providers that model a shared
	// physical file as virtual per-session sources use these to emit tombstone
	// sources when a DB row or DB file has disappeared and can no longer be
	// rediscovered from current metadata. Hints are advisory: providers must
	// still validate ownership against the changed path/watch root before
	// emitting them.
	StoredSourcePaths []string
	// AllowWatermarkOnlySources tells providers that fan a shared container
	// out to per-session virtual sources that the caller's downstream
	// freshness gate can cheaply skip watermark-only sources, so the provider
	// may answer with a bounded session-row listing instead of computing
	// every session's full child digest. Callers that consume the returned
	// sources directly (reconciliation, tombstoning) must leave this unset to
	// keep full-fidelity fingerprint metadata.
	AllowWatermarkOnlySources bool
	// StoredMemberFreshnessPage, set only alongside AllowWatermarkOnlySources,
	// pages the caller's stored freshness for the changed shared container in
	// ascending virtual-path order. A provider answering with the bounded
	// watermark listing merges against it during the stream and omits members
	// whose carried watermark is already covered, so a one-session write
	// emits one source without materializing the container's full membership.
	// Nil means the caller holds no stored authority and every source must be
	// returned. Callers gate this on a container capture taken before the
	// listing and re-request unfiltered when that capture goes stale.
	StoredMemberFreshnessPage StoredMemberFreshnessPager
}

// StoredMemberFreshness is one stored virtual member's coverage authority: a
// listed source at Path whose carried watermark is at or below
// CoveredThroughNS is provably unchanged and may be omitted from a
// changed-path listing. Rows the caller cannot vouch for (a stale data
// version, an unparseable stored identity) are simply not emitted, which
// keeps their sources listed.
type StoredMemberFreshness struct {
	Path             string
	CoveredThroughNS int64
}

// StoredMemberFreshnessPager returns stored freshness rows strictly after
// afterPath in ascending path order, at most limit of them, and whether the
// container's stored membership is exhausted. Implementations must be safe to
// call repeatedly within one listing.
type StoredMemberFreshnessPager func(
	ctx context.Context, afterPath string, limit int,
) ([]StoredMemberFreshness, bool, error)

// FindSourceRequest contains lookup inputs and persisted source hints for
// provider-owned source resolution. RawSessionID and FullSessionID identify the
// requested logical session. StoredFilePath and FingerprintKey are advisory
// hints from the archive, not authoritative filesystem paths; providers should
// try them first when useful, but provider-owned identity decides whether the
// source belongs to the requested session.
type FindSourceRequest struct {
	RawSessionID  string
	FullSessionID string
	// StoredFilePath is the persisted sessions.file_path value, which may be a
	// provider virtual path and may be stale after a move, deletion, or remote
	// sync identity rewrite.
	StoredFilePath string
	// FingerprintKey is the persisted source freshness key when the caller has
	// one distinct from StoredFilePath.
	FingerprintKey string
	// RequireFreshSource asks the provider to verify the source against current
	// provider metadata before returning it. A stale hint may still be used to
	// find the current source, but if the provider cannot prove the requested
	// session exists now it should return found=false rather than a tombstone.
	RequireFreshSource bool
	// PreferStoredSource asks the provider to return a valid StoredFilePath
	// source as-is rather than re-resolving it to a different but equivalent
	// source (for example a duplicate of the same session in another on-disk
	// layout). Source-lookup callers set it so an explicitly stored or pinned
	// source path is preserved; sync processing leaves it false so duplicate
	// sources still canonicalize to a single location.
	PreferStoredSource bool
}

// SourceFingerprint is the provider-normalized source freshness identity. The
// engine uses Key plus size/mtime/hash fields for skip-cache, data-version, and
// source metadata compatibility, including PostgreSQL push/read parity. Key
// should normally match SourceRef.FingerprintKey or ParsedSession.File.Path.
type SourceFingerprint struct {
	Key     string
	Size    int64
	MTimeNS int64
	Inode   uint64
	Device  uint64
	Hash    string
}

// ParseRequest is the full-parse provider input.
type ParseRequest struct {
	Source      SourceRef
	Fingerprint SourceFingerprint
	Machine     string
	ForceParse  bool
}

// ParseOutcome is the full-parse provider output. It is meaningful only when
// Provider.Parse returns a nil error. Providers own persisted source identity:
// each ParseResult.Result.Session.File.Path must be the same provider identity
// used for source metadata lookups and PostgreSQL/session metadata compatibility
// (usually SourceRef.FingerprintKey). For multi-session sources, every returned
// result must use a session-scoped path when the backing source can produce more
// than one logical session.
type ParseOutcome struct {
	Results            []ParseResultOutcome
	ExcludedSessionIDs []string
	SourceErrors       []SourceError
	ResultSetComplete  bool
	ForceReplace       bool
	SkipReason         SkipReason
}

// ParseResultOutcome pairs a normalized parse result with per-session retry and
// data-version state.
type ParseResultOutcome struct {
	Result      ParseResult
	DataVersion DataVersionState
	RetryReason string
}

// SourceError reports a per-session parse failure from a multi-session source.
// Providers use the Parse error return instead when a failure cannot be
// isolated to a persisted full session ID.
type SourceError struct {
	SourceKey   string
	DisplayPath string
	SessionID   string
	Err         error
	Retryable   bool
}

// DataVersionState describes whether a parsed result is current for this parser
// data version. Data-version freshness is per result; clean skip-cache
// persistence is still source-scoped and must be suppressed by callers when any
// result needs retry, any source error exists, or the result set is incomplete.
type DataVersionState uint8

const (
	DataVersionUnspecified DataVersionState = iota
	DataVersionCurrent
	DataVersionNeedsRetry
)

// SkipReason explains provider-level intentional skips. A provider skip is an
// explicit source-level outcome, not a nil parse result. Callers may record a
// clean skip-cache entry only when the skip is complete, non-erroring, and keyed
// by the provider fingerprint/source identity.
type SkipReason uint8

const (
	SkipNone SkipReason = iota
	SkipNoSession
	SkipUnsupportedSource
	SkipNonInteractive
	SkipShadowedBySidecar
)

// IncrementalRequest is the append-only parse input.
type IncrementalRequest struct {
	Source       SourceRef
	Fingerprint  SourceFingerprint
	SessionID    string
	Offset       int64
	StartOrdinal int
	Machine      string
	// LastEntryUUID is the UUID of the last entry stored for this
	// session, used by DAG-aware parsers (Claude) to detect when an
	// appended tail forks away from the stored tip and must trigger a
	// full reparse instead of a naive append.
	LastEntryUUID string
	// StoredAgentLabel, StoredEntrypoint and StoredSessionKind are the
	// session identity values already persisted for this session. Claude
	// identity is first-non-empty-wins across the file, and real CLI
	// transcripts carry a top-level entrypoint on most lines, so the
	// incremental parser escalates to a full parse only when an appended
	// non-empty value could fill a still-empty stored field.
	StoredAgentLabel  string
	StoredEntrypoint  string
	StoredSessionKind string
	// StoredClaudeLinearParse mirrors the session's persisted
	// claude_linear_parse flag: whether the last full parse fell back
	// to linear processing. Linearity is monotonic across appends, so
	// a true value lets the Claude incremental parser skip fork
	// detection — the full parser processes such files in line order
	// regardless of parent uuids. nil (unknown, legacy rows) or false
	// keeps strict fork detection.
	StoredClaudeLinearParse *bool
	// StoredLastClaudeMessageID is the provider message id of the last
	// stored assistant message for this session (empty when none), or
	// nil when the call site cannot supply it. The Claude incremental
	// parser uses it to keep routine queued-command appends
	// incremental: the queued-command masking fallback fires only when
	// the appended assistant head continues exactly this message id.
	// nil keeps the conservative fallback.
	StoredLastClaudeMessageID *string
}

// IncrementalOutcome is the append-only parse output.
type IncrementalOutcome struct {
	SessionID            string
	Messages             []ParsedMessage
	SubagentLinks        []ClaudeSubagentLink
	EndedAt              time.Time
	ConsumedBytes        int64
	MessageCount         int
	UserMessageCount     int
	TotalOutputTokens    int
	PeakContextTokens    int
	HasTotalOutputTokens bool
	HasPeakContextTokens bool
	TerminationStatus    *TerminationStatus
	ForceReplace         bool
}

// IncrementalStatus describes how an incremental parse attempt should proceed.
type IncrementalStatus uint8

const (
	IncrementalUnsupported IncrementalStatus = iota
	IncrementalNoNewData
	IncrementalApplied
	IncrementalNeedsFullParse
)

// ErrIncrementalNeedsFullParse signals that a provider declined the
// appended tail and the caller must fall back to a full parse that
// replaces stored rows. It is provider-agnostic; parser-internal
// fallbacks (Claude, Codex) are mapped to it at the provider seam.
var ErrIncrementalNeedsFullParse = fmt.Errorf(
	"incremental parse: appended lines require a replacing full parse",
)

// ProviderFactories returns one provider factory for every registered agent.
func ProviderFactories() []ProviderFactory {
	factories := make([]ProviderFactory, 0, len(Registry))
	for _, def := range Registry {
		factories = append(factories, providerFactoryForDef(def))
	}
	return factories
}

func providerFactoryForDef(def AgentDef) ProviderFactory {
	def = cloneAgentDef(def)
	switch def.Type {
	case AgentAntigravity:
		return newAntigravityProviderFactory(def)
	case AgentAntigravityCLI:
		return newAntigravityCLIProviderFactory(def)
	case AgentAider:
		return newAiderProviderFactory(def)
	case AgentAmp:
		return newAmpProviderFactory(def)
	case AgentClaude:
		return newClaudeProviderFactory(def)
	case AgentOpenClaude:
		return newOpenClaudeProviderFactory(def)
	case AgentClaudeAI:
		return newImportOnlyProviderFactory(def)
	case AgentCommandCode:
		return newCommandCodeProviderFactory(def)
	case AgentCodex:
		return newCodexProviderFactory(def)
	case AgentTraeX:
		return newTraeXProviderFactory(def)
	case AgentCopilot:
		return newCopilotProviderFactory(def)
	case AgentCowork:
		return newCoworkProviderFactory(def)
	case AgentCortex:
		return newCortexProviderFactory(def)
	case AgentCursor:
		return newCursorProviderFactory(def)
	case AgentChatGPT:
		return newImportOnlyProviderFactory(def)
	case AgentDeepSeekTUI:
		return newDeepSeekTUIProviderFactory(def)
	case AgentDeepSeekHarness:
		return newDeepSeekHarnessProviderFactory(def)
	case AgentForge:
		return newForgeProviderFactory(def)
	case AgentDevin:
		return newDevinProviderFactory(def)
	case AgentHermes:
		return newHermesProviderFactory(def)
	case AgentGrok:
		return newGrokProviderFactory(def)
	case AgentGoose:
		return newGooseProviderFactory(def)
	case AgentIflow:
		return newIflowProviderFactory(def)
	case AgentGptme:
		return newGptmeProviderFactory(def)
	case AgentGemini:
		return newGeminiProviderFactory(def)
	case AgentGeminiApps:
		return newImportOnlyProviderFactory(def)
	case AgentKimi:
		return newKimiProviderFactory(def)
	case AgentKimiWork:
		return newKimiWorkProviderFactory(def)
	case AgentKiro:
		return newKiroProviderFactory(def)
	case AgentKiroIDE:
		return newKiroIDEProviderFactory(def)
	case AgentKilo:
		return newKiloProviderFactory(def)
	case AgentKiloLegacy:
		return newKiloLegacyProviderFactory(def)
	case AgentMiMoCode:
		return newMiMoCodeProviderFactory(def)
	case AgentIcodemate:
		return newIcodemateProviderFactory(def)
	case AgentOpenHands:
		return newOpenHandsProviderFactory(def)
	case AgentOpenCode:
		return newOpenCodeProviderFactory(def)
	case AgentOMP:
		return newPiProviderFactory(def)
	case AgentOpenClaw:
		return newOpenClawProviderFactory(def)
	case AgentPiebald:
		return newPiebaldProviderFactory(def)
	case AgentPoolside:
		return newPoolsideProviderFactory(def)
	case AgentPi:
		return newPiProviderFactory(def)
	case AgentPrimeAgent:
		return newPiProviderFactory(def)
	case AgentPositron:
		return newPositronProviderFactory(def)
	case AgentPositAssistant:
		return newPositAssistantProviderFactory(def)
	case AgentQClaw:
		return newQClawProviderFactory(def)
	case AgentQwen:
		return newQwenProviderFactory(def)
	case AgentQwenPaw:
		return newQwenPawProviderFactory(def)
	case AgentQoder:
		return newQoderProviderFactory(def)
	case AgentReasonix:
		return newReasonixProviderFactory(def)
	case AgentOmnigent:
		return newOmnigentProviderFactory(def)
	case AgentShelley:
		return newShelleyProviderFactory(def)
	case AgentVSCopilot:
		return newVisualStudioCopilotProviderFactory(def)
	case AgentVSCodeCopilot:
		return newVSCodeCopilotProviderFactory(def)
	case AgentWindsurf:
		return newWindsurfProviderFactory(def)
	case AgentTrae:
		return newTraeProviderFactory(def)
	case AgentVibe:
		return newVibeProviderFactory(def)
	case AgentZCode:
		return newZcodeProviderFactory(def)
	case AgentWarp:
		return newWarpProviderFactory(def)
	case AgentWorkBuddy:
		return newWorkBuddyProviderFactory(def)
	case AgentZencoder:
		return newZencoderProviderFactory(def)
	case AgentZed:
		return newZedProviderFactory(def)
	case AgentRooCode:
		return newRooCodeProviderFactory(def)
	case AgentCodebuff:
		return newCodebuffProviderFactory(def)
	default:
		panic("missing provider factory for " + string(def.Type))
	}
}

// ProviderFactoryByType returns the factory for an agent type.
func ProviderFactoryByType(t AgentType) (ProviderFactory, bool) {
	for _, factory := range ProviderFactories() {
		if factory.Definition().Type == t {
			return factory, true
		}
	}
	return nil, false
}

// NewProvider constructs a config-bound provider for an agent type.
func NewProvider(t AgentType, cfg ProviderConfig) (Provider, bool) {
	factory, ok := ProviderFactoryByType(t)
	if !ok {
		return nil, false
	}
	return factory.NewProvider(cfg), true
}

func cloneAgentDef(def AgentDef) AgentDef {
	def.DefaultDirs = append([]string(nil), def.DefaultDirs...)
	def.WatchSubdirs = append([]string(nil), def.WatchSubdirs...)
	return def
}
