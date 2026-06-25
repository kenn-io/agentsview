package parser

import "context"

// source_set.go provides the generic plumbing shared by every reusable
// source-set base. A SourceSet owns source resolution and parsing for one
// provider; sourceSetProvider wraps any SourceSet into a full Provider, and
// sourceSetFactory builds those providers from an AgentDef + Capabilities + a
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

// SourceSet is the source-resolution and parse core that a Provider delegates
// to. It is the Provider interface minus the Definition/Capabilities/config
// plumbing (supplied by sourceSetProvider) and minus ParseIncremental (which
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

// sourceSetProvider adapts a SourceSet to the Provider interface. It supplies
// the AgentDef/Capabilities/config carried by ProviderBase, forwards the source
// methods to the SourceSet, and applies the two provider-level normalizations
// every provider performs: raw-session-ID injection on FindSource and the
// machine fallback on Parse. ParseIncremental is inherited from ProviderBase
// (unsupported) until a base opts in.
type sourceSetProvider struct {
	ProviderBase
	sources SourceSet
}

func (p *sourceSetProvider) Discover(ctx context.Context) ([]SourceRef, error) {
	return p.sources.Discover(ctx)
}

func (p *sourceSetProvider) WatchPlan(ctx context.Context) (WatchPlan, error) {
	return p.sources.WatchPlan(ctx)
}

func (p *sourceSetProvider) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	return p.sources.SourcesForChangedPath(ctx, req)
}

func (p *sourceSetProvider) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	return p.sources.FindSource(
		ctx, providerFindRequestWithRawSessionID(p.Def, req),
	)
}

func (p *sourceSetProvider) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	return p.sources.Fingerprint(ctx, source)
}

func (p *sourceSetProvider) Parse(
	ctx context.Context,
	req ParseRequest,
) (ParseOutcome, error) {
	req.Machine = firstNonEmptyJSONLString(req.Machine, p.Config.Machine)
	return p.sources.Parse(ctx, req)
}

// sourceSetFactory is the generic ProviderFactory for any SourceSet-backed
// provider. build constructs the SourceSet from the cloned per-provider config
// (roots, machine, path rewriter), so a base captures whatever config it needs
// in a closure rather than threading it through a struct.
type sourceSetFactory struct {
	def   AgentDef
	caps  Capabilities
	build func(cfg ProviderConfig) SourceSet
}

func newSourceSetFactory(
	def AgentDef,
	caps Capabilities,
	build func(cfg ProviderConfig) SourceSet,
) ProviderFactory {
	return sourceSetFactory{
		def:   cloneAgentDef(def),
		caps:  caps,
		build: build,
	}
}

func (f sourceSetFactory) Definition() AgentDef {
	return cloneAgentDef(f.def)
}

func (f sourceSetFactory) Capabilities() Capabilities {
	return f.caps
}

func (f sourceSetFactory) NewProvider(cfg ProviderConfig) Provider {
	cfg = cfg.Clone()
	return &sourceSetProvider{
		ProviderBase: ProviderBase{
			Def:    cloneAgentDef(f.def),
			Caps:   f.caps,
			Config: cfg,
		},
		sources: f.build(cfg),
	}
}
