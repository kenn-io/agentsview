package parser

import (
	"context"
	"fmt"
)

// Icodemate hosts two storage families under one provider and one agent ID:
// the VSCode-extension OpenCode storage (see icodemate.go) and the terminal
// CLI Claude-format projects root (see icodemate_cli.go). IcodemateProvider
// splits the configured roots by on-disk layout and delegates the family each
// root owns: OpenCode-layout roots go to the shared openCodeFormatSourceSet
// (SQLite and storage/session_diff), and Claude-format projects roots go to
// the icodemateCLISourceSet. Both families label their parsed sessions onto
// AgentIcodemate with the icodemate: ID prefix, so one source sees one agent.
//
// The two layouts never overlap on disk, so every SourceSet method is either a
// per-source route (Parse, Fingerprint) or a merged fan-out (Discover, watch,
// find). Reconciliation semantics are preserved per family: OpenCode's shared
// icodemate.db container keeps its atomic container scopes, while CLI projects
// roots fall through to the generic directory plan.

var (
	_ Provider                          = (*icodemateProvider)(nil)
	_ StreamingDiscoverer               = (*icodemateProvider)(nil)
	_ ChangedPathRelevanceProvider      = (*icodemateProvider)(nil)
	_ ReconciliationSourceStateResolver = (*icodemateProvider)(nil)
)

type icodemateProviderFactory struct {
	def AgentDef
}

func newIcodemateProviderFactory(def AgentDef) ProviderFactory {
	return icodemateProviderFactory{def: cloneAgentDef(def)}
}

func (f icodemateProviderFactory) Definition() AgentDef {
	return cloneAgentDef(f.def)
}

func (f icodemateProviderFactory) Capabilities() Capabilities {
	return icodemateProviderCapabilities()
}

func (f icodemateProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	cfg = cfg.Clone()
	opencodeRoots, cliRoots := splitIcodemateRoots(cfg.Roots)
	opencodeCfg := cfg
	opencodeCfg.Roots = opencodeRoots
	provider := &icodemateProvider{
		opencode: &openCodeFormatProvider{
			sources: newOpenCodeFormatSourceSet(
				opencodeRoots,
				openCodeProviderSpecForAgent(AgentIcodemate),
				cfg.SQLiteContainerListsWatermarkOnly,
			),
		},
		cli:      newIcodemateCLISourceSet(cliRoots),
		allRoots: cfg.Roots,
		// allSources classifies changed paths and maps reconciliation scopes
		// across every configured icodemate root, not just the OpenCode-format
		// subset the inner opencode source set is built over. A root whose
		// OpenCode layout has not materialized yet (still-empty directory)
		// must keep icodemate.db container semantics, and CLI jsonl paths
		// under any root stay unclassified.
		allSources: newOpenCodeFormatSourceSet(
			cfg.Roots,
			openCodeProviderSpecForAgent(AgentIcodemate),
			cfg.SQLiteContainerListsWatermarkOnly,
		),
	}
	provider.ProviderBase = ProviderBase{
		Def:    cloneAgentDef(f.def),
		Caps:   icodemateProviderCapabilities(),
		Config: cfg,
	}
	provider.opencode.ProviderBase = ProviderBase{
		Def:    cloneAgentDef(f.def),
		Caps:   openCodeFormatProviderCapabilities(),
		Config: opencodeCfg,
	}
	return provider
}

// icodemateProviderCapabilities combines the OpenCode container mechanics of
// the VS Code extension with the Claude-compatible multi-session semantics of
// terminal CLI transcripts.
func icodemateProviderCapabilities() Capabilities {
	caps := openCodeFormatProviderCapabilities()
	caps.Source.MultiSessionSource = CapabilitySupported
	caps.Source.ExcludedSessions = CapabilitySupported
	caps.Source.ForceReplaceOnParse = CapabilitySupported
	caps.Source.S3Discovery = CapabilitySupported
	caps.Content.SessionName = CapabilitySupported
	caps.Content.GitBranch = CapabilitySupported
	caps.Content.Subagents = CapabilitySupported
	caps.Content.ToolResults = CapabilitySupported
	caps.Content.TerminationStatus = CapabilitySupported
	caps.Content.MalformedLineCount = CapabilitySupported
	caps.Content.TruncationStatus = CapabilitySupported
	caps.Content.StopReason = CapabilitySupported
	return caps
}

// splitIcodemateRoots partitions configured roots by on-disk layout. A root
// with an OpenCode storage directory or icodemate.db is owned by the
// OpenCode-format source set; anything else (the terminal CLI's
// projects/<project>/*.jsonl layout) is owned by the CLI source set.
func splitIcodemateRoots(roots []string) (opencodeRoots, cliRoots []string) {
	for _, root := range roots {
		if resolveOpenCodeFormatSource(icodemateFmt, root).Mode != "" {
			opencodeRoots = append(opencodeRoots, root)
		} else {
			cliRoots = append(cliRoots, root)
		}
	}
	return opencodeRoots, cliRoots
}

type icodemateProvider struct {
	ProviderBase
	opencode *openCodeFormatProvider
	cli      *icodemateCLISourceSet
	// allRoots carries every configured root so reconciliation validates and
	// walks the full configured scope, not just the OpenCode subset the inner
	// opencode provider is built over.
	allRoots []string
	// allSources is the OpenCode-format source set built over every
	// configured root for classification and reconciliation only. Discovery
	// and parsing stay family-split so each layout is owned where it exists;
	// allSources exists so icodemate.db container semantics hold for roots
	// whose OpenCode layout has not materialized yet and so CLI jsonl paths
	// classify as unclassified (the Claude-style fallback) under any root.
	allSources openCodeFormatSourceSet
}

func (p *icodemateProvider) Discover(ctx context.Context) ([]SourceRef, error) {
	opencodeSources, err := p.opencode.Discover(ctx)
	if err != nil {
		return nil, err
	}
	cliSources, err := p.cli.Discover(ctx)
	if err != nil {
		return nil, err
	}
	return append(opencodeSources, cliSources...), nil
}

func (p *icodemateProvider) DiscoverEach(
	ctx context.Context, yield func(SourceRef) error,
) error {
	if err := p.opencode.DiscoverEach(ctx, yield); err != nil {
		return err
	}
	return p.cli.DiscoverEach(ctx, yield)
}

func (p *icodemateProvider) WatchPlan(ctx context.Context) (WatchPlan, error) {
	opencodePlan, err := p.opencode.WatchPlan(ctx)
	if err != nil {
		return WatchPlan{}, err
	}
	cliPlan, err := p.cli.WatchPlan(ctx)
	if err != nil {
		return WatchPlan{}, err
	}
	merged := make([]WatchRoot, 0, len(opencodePlan.Roots)+len(cliPlan.Roots))
	merged = append(merged, opencodePlan.Roots...)
	merged = append(merged, cliPlan.Roots...)
	return WatchPlan{Roots: merged}, nil
}

func (p *icodemateProvider) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	opencodeSources, err := p.opencode.SourcesForChangedPath(ctx, req)
	if err != nil {
		return nil, err
	}
	cliSources, err := p.cli.SourcesForChangedPath(ctx, req)
	if err != nil {
		return nil, err
	}
	if len(cliSources) == 0 {
		return opencodeSources, nil
	}
	if len(opencodeSources) == 0 {
		return cliSources, nil
	}
	seen := make(map[string]struct{}, len(opencodeSources)+len(cliSources))
	merged := make([]SourceRef, 0, len(opencodeSources)+len(cliSources))
	for _, src := range append(opencodeSources, cliSources...) {
		if _, ok := seen[src.Key]; ok {
			continue
		}
		seen[src.Key] = struct{}{}
		merged = append(merged, src)
	}
	return merged, nil
}

func (p *icodemateProvider) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	if src, ok, err := p.opencode.FindSource(ctx, req); ok || err != nil {
		return src, ok, err
	}
	req = ProviderFindRequestWithRawSessionID(p.Def, req)
	return p.cli.FindSource(ctx, req)
}

func (p *icodemateProvider) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	switch source.Opaque.(type) {
	case openCodeFormatSource, *openCodeFormatSource:
		return p.opencode.Fingerprint(ctx, source)
	case claudeSource, *claudeSource, MaterializedFileSource:
		return p.cli.Fingerprint(ctx, source)
	default:
		return SourceFingerprint{}, fmt.Errorf("icodemate source path unavailable")
	}
}

func (p *icodemateProvider) Parse(
	ctx context.Context,
	req ParseRequest,
) (ParseOutcome, error) {
	switch req.Source.Opaque.(type) {
	case openCodeFormatSource, *openCodeFormatSource:
		return p.opencode.Parse(ctx, req)
	case claudeSource, *claudeSource, MaterializedFileSource:
		req.Machine = firstNonEmptyJSONLString(req.Machine, p.Config.Machine)
		return p.cli.Parse(ctx, req)
	default:
		return ParseOutcome{}, fmt.Errorf("icodemate source path unavailable")
	}
}

// ChangedPathRelevance classifies paths under any configured icodemate root
// with OpenCode-format SQLite sidecar semantics via allSources, so a root
// whose OpenCode layout has not materialized yet (still-empty directory)
// keeps its icodemate.db container rules. CLI project jsonl paths classify as
// unclassified (the same fallback Claude-style sources use), so watch
// adapters never misfile a CLI change under OpenCode layout rules.
func (p *icodemateProvider) ChangedPathRelevance(
	ctx context.Context,
	req ChangedPathRequest,
) (ChangedPathRelevance, error) {
	return p.allSources.ChangedPathRelevance(ctx, req)
}

// SourceForReconciliation delegates to the all-roots OpenCode source set,
// which is the only family with a shared container (icodemate.db) whose
// virtual members must be rehydrated exactly during reconciliation streaming.
// CLI project transcripts are plain single-file sources and are rehydrated
// through the generic source resolver path instead.
func (p *icodemateProvider) SourceForReconciliation(
	ctx context.Context, path, project string,
) (SourceRef, bool, error) {
	return p.allSources.SourceForReconciliation(ctx, path, project)
}

func (p *icodemateProvider) SourceForReconciliationWithState(
	ctx context.Context, path, project string, state ReconciliationSourceState,
) (SourceRef, bool, error) {
	return p.allSources.SourceForReconciliationWithState(
		ctx, path, project, state,
	)
}

func (p *icodemateProvider) ReconciliationSourceState(
	source SourceRef,
) (ReconciliationSourceState, bool) {
	return p.allSources.reconciliationSourceState(source)
}

func (p *icodemateProvider) ApplyReconciliationSourceState(
	source *SourceRef, state ReconciliationSourceState,
) error {
	return p.allSources.applyReconciliationSourceState(source, state)
}

// ResolveReconciliationScopes preserves the OpenCode container topology for
// the shared icodemate.db under every configured icodemate root (allSources)
// while letting CLI projects roots fall through to the generic directory
// plan. The inner opencode source set's reconciliation container only
// recognizes the OpenCode subset, so every other requested root is scoped as
// a plain directory.
func (p *icodemateProvider) ResolveReconciliationScopes(
	_ context.Context, req ReconciliationScopeRequest,
) (ReconciliationScopePlan, error) {
	if err := ValidateReconciliationScopeRoots(
		p.Def.Type, p.allRoots, req.Roots,
	); err != nil {
		return ReconciliationScopePlan{}, err
	}
	return containerAwareReconciliationScopePlan(
		p.allRoots, req.Roots, p.allSources.reconciliationContainer,
	), nil
}
