package parser

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

var (
	_ Provider         = (*cortexProvider)(nil)
	_ WatchRootPlanner = (*cortexProvider)(nil)
)

type cortexProviderFactory struct {
	def AgentDef
}

func newCortexProviderFactory(def AgentDef) ProviderFactory {
	return cortexProviderFactory{def: cloneAgentDef(def)}
}

func (f cortexProviderFactory) Definition() AgentDef {
	return cloneAgentDef(f.def)
}

func (f cortexProviderFactory) Capabilities() Capabilities {
	return cortexProviderCapabilities()
}

func (f cortexProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	cfg = cfg.Clone()
	return &cortexProvider{
		ProviderBase: ProviderBase{
			Def:    cloneAgentDef(f.def),
			Caps:   cortexProviderCapabilities(),
			Config: cfg,
		},
		sources: newCortexSourceSet(cfg.Roots),
	}
}

type cortexProvider struct {
	ProviderBase
	sources JSONLSourceSet
}

func (p *cortexProvider) Discover(ctx context.Context) ([]SourceRef, error) {
	return p.sources.Discover(ctx)
}

func (p *cortexProvider) DiscoverEach(ctx context.Context, yield func(SourceRef) error) error {
	return p.sources.DiscoverEach(ctx, yield)
}

func (p *cortexProvider) WatchPlan(ctx context.Context) (WatchPlan, error) {
	return p.sources.WatchPlan(ctx)
}

func (p *cortexProvider) WatchRoots(
	ctx context.Context,
) ([]WatchRoot, error) {
	return p.sources.WatchRoots(ctx)
}

func (p *cortexProvider) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	return p.sources.SourcesForChangedPath(ctx, req)
}

func (p *cortexProvider) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	return p.sources.FindSource(ctx, ProviderFindRequestWithRawSessionID(p.Def, req))
}

func (p *cortexProvider) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	return p.sources.Fingerprint(ctx, source)
}

func (p *cortexProvider) Parse(
	ctx context.Context,
	req ParseRequest,
) (ParseOutcome, error) {
	if err := ctx.Err(); err != nil {
		return ParseOutcome{}, err
	}
	path, ok, err := p.sources.pathFromSource(ctx, req.Source)
	if err != nil {
		return ParseOutcome{}, err
	}
	if !ok {
		return ParseOutcome{}, fmt.Errorf("cortex source path unavailable")
	}
	machine := firstNonEmptyJSONLString(req.Machine, p.Config.Machine)
	sess, msgs, err := p.parseSession(path, machine)
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

func newCortexSourceSet(roots []string) JSONLSourceSet {
	return NewJSONLSourceSet(AgentCortex, roots,
		WithExtensions(".json"),
		WithFollowSymlinkFiles(),
		WithIncludePath(isCortexSourcePath),
		WithSessionIDFromPath(cortexSessionIDFromPath),
		WithProjectHint(func(root, path string) string { return "" }),
		// Cortex's .history.jsonl sidecar participates in source freshness, so
		// fold it into the shared fingerprint (size, mtime, and content hash)
		// via the framework companion hook instead of a bespoke Fingerprint.
		// WithContentHashing preserves the legacy per-agent file_hash, which a
		// resync would otherwise clear to NULL.
		WithCompanionFiles(func(transcriptPath string) []string {
			return []string{cortexHistoryCompanionPath(transcriptPath)}
		}),
		WithCompanionTranscript(func(companionPath string) (string, bool) {
			stem, ok := strings.CutSuffix(companionPath, ".history.jsonl")
			return stem + ".json", ok
		}),
		WithContentHashing(),
	)
}

func isCortexSourcePath(root, path string) bool {
	if !samePath(filepath.Dir(path), filepath.Clean(root)) {
		return false
	}
	return IsCortexSessionFile(filepath.Base(path))
}

func cortexSessionIDFromPath(root, path string) string {
	if !isCortexSourcePath(root, path) {
		return ""
	}
	return strings.TrimSuffix(filepath.Base(path), ".json")
}

func cortexHistoryCompanionPath(path string) string {
	return strings.TrimSuffix(path, ".json") + ".history.jsonl"
}

func cortexProviderCapabilities() Capabilities {
	source := jsonlFileProviderSourceCapabilities()
	source.StreamingDiscovery = CapabilitySupported
	source.WatchRoots = CapabilitySupported
	return Capabilities{
		Source: source,
		Content: ContentCapabilities{
			FirstMessage: CapabilitySupported,
			SessionName:  CapabilitySupported,
			Cwd:          CapabilitySupported,
			ToolCalls:    CapabilitySupported,
			ToolResults:  CapabilitySupported,
		},
	}
}
