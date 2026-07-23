package parser

import "context"

// source_set.go provides the generic plumbing shared by every reusable
// source-set base. A SourceSet owns source resolution and parsing for one
// provider; SourceSetProvider wraps any SourceSet into a full Provider, and
// SourceSetFactory builds those providers from an AgentDef + Capabilities + a
// per-config constructor.
//
// The point is that a base such as multiSessionContainerSourceSet (or
// singleFileSourceSet) implements SourceSet once and reuses this factory and
// this delegating provider, instead of each base re-hand-rolling the
// Definition/Capabilities/NewProvider factory and the six forwarding provider
// methods. Provider-level concerns that every provider shares -- folding the
// raw session ID into FindSource requests, and the request/config machine
// fallback for Parse -- live here so the SourceSet implementations stay focused
// on agent-specific source logic.

// MaterializedFileSource is the Opaque payload for a session whose bytes were
// materialized to a local file outside any configured root -- an S3 object
// fetched to a temp dir. A source set resolves it straight to its Path, so
// provider.Parse can parse the file without re-deriving identity from a
// configured-root layout (the materialized path may not match the provider's
// on-disk convention). The caller supplies the project via SourceRef.ProjectHint
// and applies any source-identity rewrite to the parsed results.
type MaterializedFileSource struct {
	Path string
}

// SourceSet is the source-resolution and parse core that a Provider delegates
// to. It is the Provider interface minus the Definition/Capabilities/config
// plumbing (supplied by SourceSetProvider) and minus ParseIncremental (which
// falls through to the ProviderBase "unsupported" default until a base needs
// it).
type SourceSet interface {
	Discover(context.Context) ([]SourceRef, error)
	WatchPlan(context.Context) (WatchPlan, error)
	SourcesForChangedPath(
		context.Context, ChangedPathRequest,
	) ([]SourceRef, error)
	FindSource(context.Context, FindSourceRequest) (SourceRef, bool, error)
	Fingerprint(context.Context, SourceRef) (SourceFingerprint, error)
	Parse(context.Context, ParseRequest) (ParseOutcome, error)
}

// SourceSetProvider adapts a SourceSet to the Provider interface. It supplies
// the AgentDef/Capabilities/config carried by ProviderBase, forwards the source
// methods to the SourceSet, and applies the two provider-level normalizations
// every provider performs: raw-session-ID injection on FindSource and the
// machine fallback on Parse. ParseIncremental is inherited from ProviderBase
// (unsupported) until a base opts in.
type SourceSetProvider struct {
	ProviderBase
	sources SourceSet
}

var _ StreamingDiscoverer = (*SourceSetProvider)(nil)

func (p *SourceSetProvider) Discover(ctx context.Context) ([]SourceRef, error) {
	return p.sources.Discover(ctx)
}

func (p *SourceSetProvider) DiscoverEach(
	ctx context.Context, yield func(SourceRef) error,
) error {
	discoverer, ok := p.sources.(StreamingDiscoverer)
	if !ok {
		return UnsupportedProviderFeatureError{
			Provider: p.Def.Type,
			Feature:  "streaming discovery",
		}
	}
	return discoverer.DiscoverEach(ctx, yield)
}

func (p *SourceSetProvider) WatchPlan(ctx context.Context) (WatchPlan, error) {
	return p.sources.WatchPlan(ctx)
}

func (p *SourceSetProvider) WatchRoots(
	ctx context.Context,
) ([]WatchRoot, error) {
	if planner, ok := p.sources.(WatchRootPlanner); ok {
		return planner.WatchRoots(ctx)
	}
	plan, err := p.sources.WatchPlan(ctx)
	if err != nil {
		return nil, err
	}
	return watchRootMetadata(plan.Roots), nil
}

func (p *SourceSetProvider) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	return p.sources.SourcesForChangedPath(ctx, req)
}

func (p *SourceSetProvider) StoredSourceHintScopes(
	req ChangedPathRequest,
) []StoredSourceHintScope {
	resolver, ok := p.sources.(StoredSourceHintScopeProvider)
	if !ok {
		return nil
	}
	return resolver.StoredSourceHintScopes(req)
}

func (p *SourceSetProvider) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	return p.sources.FindSource(
		ctx, ProviderFindRequestWithRawSessionID(p.Def, req),
	)
}

func (p *SourceSetProvider) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	return p.sources.Fingerprint(ctx, source)
}

func (p *SourceSetProvider) RestoreCachedSourceState(
	ctx context.Context, source SourceRef,
) (bool, error) {
	restorer, ok := p.sources.(CachedSourceStateRestorer)
	if !ok {
		return false, nil
	}
	return restorer.RestoreCachedSourceState(ctx, source)
}

func (p *SourceSetProvider) Parse(
	ctx context.Context,
	req ParseRequest,
) (ParseOutcome, error) {
	req.Machine = firstNonEmptyJSONLString(req.Machine, p.Config.Machine)
	return p.sources.Parse(ctx, req)
}

func (p *SourceSetProvider) SourceForReconciliation(
	ctx context.Context, path, project string,
) (SourceRef, bool, error) {
	resolver, ok := p.sources.(ReconciliationSourceResolver)
	if !ok {
		return SourceRef{}, false, nil
	}
	return resolver.SourceForReconciliation(ctx, path, project)
}

func (p *SourceSetProvider) ReconciliationMemberIdentity(
	fullSessionID string,
) string {
	resolver, ok := p.sources.(ReconciliationMemberIdentityResolver)
	if !ok {
		return ""
	}
	return resolver.ReconciliationMemberIdentity(fullSessionID)
}

func (p *SourceSetProvider) PersistentArchiveSource(
	path string, fullSessionID string,
) (string, bool) {
	resolver, ok := p.sources.(PersistentArchiveSourceResolver)
	if !ok {
		return "", false
	}
	return resolver.PersistentArchiveSource(path, fullSessionID)
}

// SourceSetFactory is the generic ProviderFactory for any SourceSet-backed
// provider. build constructs the SourceSet from the cloned per-provider config
// (roots, machine, path rewriter), so a base captures whatever config it needs
// in a closure rather than threading it through a struct.
type SourceSetFactory struct {
	def   AgentDef
	caps  Capabilities
	build func(cfg ProviderConfig) SourceSet
}

// NewSourceSetFactory advertises StreamingDiscovery up front because every
// source-set base streams. A wrapper still cannot invent streaming for a
// collecting source set: NewProvider downgrades the capability when the built
// SourceSet does not implement StreamingDiscoverer.
func NewSourceSetFactory(
	def AgentDef,
	caps Capabilities,
	build func(cfg ProviderConfig) SourceSet,
) ProviderFactory {
	caps.Source.StreamingDiscovery = CapabilitySupported
	return SourceSetFactory{
		def:   cloneAgentDef(def),
		caps:  withWatchRootPlanningCapability(caps),
		build: build,
	}
}

func (f SourceSetFactory) Definition() AgentDef {
	return cloneAgentDef(f.def)
}

func (f SourceSetFactory) Capabilities() Capabilities {
	return f.caps
}

func (f SourceSetFactory) NewProvider(cfg ProviderConfig) Provider {
	cfg = cfg.Clone()
	sources := f.build(cfg)
	caps := f.caps
	if _, ok := sources.(StreamingDiscoverer); !ok {
		caps.Source.StreamingDiscovery = CapabilityUnsupported
	}
	if _, ok := sources.(ReconciliationSourceResolver); !ok {
		caps.Source.SharedContainerSource = CapabilityUnsupported
	}
	if _, ok := sources.(StoredSourceHintScopeProvider); !ok {
		caps.Source.StoredSourceHints = CapabilityUnsupported
	}
	return &SourceSetProvider{
		ProviderBase: ProviderBase{
			Def:    cloneAgentDef(f.def),
			Caps:   caps,
			Config: cfg,
		},
		sources: sources,
	}
}
