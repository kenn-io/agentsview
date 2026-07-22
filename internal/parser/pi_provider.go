package parser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var _ Provider = (*piProvider)(nil)

type piProviderFactory struct {
	def AgentDef
}

func newPiProviderFactory(def AgentDef) ProviderFactory {
	return piProviderFactory{def: cloneAgentDef(def)}
}

func (f piProviderFactory) Definition() AgentDef {
	return cloneAgentDef(f.def)
}

func (f piProviderFactory) Capabilities() Capabilities {
	return piProviderCapabilities()
}

func (f piProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	cfg = cfg.Clone()
	return &piProvider{
		ProviderBase: ProviderBase{
			Def:    cloneAgentDef(f.def),
			Caps:   piProviderCapabilities(),
			Config: cfg,
		},
		sources: newPiSourceSet(f.def.Type, cfg.Roots),
	}
}

type piProvider struct {
	ProviderBase
	sources JSONLSourceSet
}

func (p *piProvider) Discover(ctx context.Context) ([]SourceRef, error) {
	sources, err := p.sources.Discover(ctx)
	if err != nil {
		return nil, err
	}
	return p.filterDiscoveredSources(sources), nil
}

func (p *piProvider) WatchPlan(ctx context.Context) (WatchPlan, error) {
	return p.sources.WatchPlan(ctx)
}

func (p *piProvider) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	sources, err := p.sources.SourcesForChangedPath(ctx, req)
	if err != nil || len(sources) == 0 {
		return sources, err
	}
	if jsonlMissingPathFallbackAllowed(req) {
		return sources, nil
	}
	return p.filterDiscoveredSources(sources), nil
}

func (p *piProvider) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	if err := ctx.Err(); err != nil {
		return SourceRef{}, false, err
	}
	req = ProviderFindRequestWithRawSessionID(p.Def, req)
	for _, path := range []string{
		req.StoredFilePath,
		req.FingerprintKey,
	} {
		if path == "" {
			continue
		}
		if source, ok, err := p.sources.sourceForPath(ctx, path); err != nil {
			return SourceRef{}, false, err
		} else if ok {
			return source, true, nil
		}
	}
	if req.RawSessionID == "" || !IsValidSessionID(req.RawSessionID) {
		return SourceRef{}, false, nil
	}
	if p.Def.Type == AgentOMP {
		return p.sourceForOMPHeaderSessionID(ctx, req.RawSessionID)
	}
	for _, root := range p.Config.Roots {
		source, ok, err := p.sourceForSessionID(ctx, root, req.RawSessionID)
		if err != nil || ok {
			return source, ok, err
		}
	}
	return SourceRef{}, false, nil
}

func (p *piProvider) sourceForSessionID(
	ctx context.Context,
	root string,
	sessionID string,
) (SourceRef, bool, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return SourceRef{}, false, nil
	}
	target := sessionID + ".jsonl"
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return SourceRef{}, false, err
		}
		if !isDirOrSymlink(entry, root) {
			continue
		}
		candidate := filepath.Join(root, entry.Name(), target)
		source, ok, err := p.sources.sourceForPath(ctx, candidate)
		if err != nil {
			return SourceRef{}, false, err
		}
		if ok {
			return source, true, nil
		}
	}
	return SourceRef{}, false, nil
}

func (p *piProvider) sourceForOMPHeaderSessionID(
	ctx context.Context,
	sessionID string,
) (SourceRef, bool, error) {
	sources, err := p.sources.Discover(ctx)
	if err != nil {
		return SourceRef{}, false, err
	}
	for _, source := range sources {
		if err := ctx.Err(); err != nil {
			return SourceRef{}, false, err
		}
		src, ok := source.Opaque.(JSONLSource)
		if !ok {
			continue
		}
		if ompHeaderSessionID(src.Path) == sessionID {
			return source, true, nil
		}
	}
	return SourceRef{}, false, nil
}

func (p *piProvider) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	return p.sources.Fingerprint(ctx, source)
}

func (p *piProvider) Parse(
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
		return ParseOutcome{}, fmt.Errorf("pi source path unavailable")
	}
	machine := firstNonEmptyJSONLString(req.Machine, p.Config.Machine)
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

func (p *piProvider) filterDiscoveredSources(sources []SourceRef) []SourceRef {
	filtered := sources[:0]
	for _, source := range sources {
		src, ok := source.Opaque.(JSONLSource)
		if !ok || !IsPiSessionFile(src.Path) {
			continue
		}
		filtered = append(filtered, source)
	}
	return filtered
}

func newPiSourceSet(agent AgentType, roots []string) JSONLSourceSet {
	// OMP nests subagent transcripts one directory deeper than the main
	// session (<project>/<session>/<agent>.jsonl), so it cannot use the
	// strict two-segment DirectoryJSONLSourceSet layout the other pi-family
	// agents rely on. It gets a recursive set that accepts any nested .jsonl.
	if agent == AgentOMP {
		return NewJSONLSourceSet(agent, roots,
			WithRecursive(),
			WithSymlinkFollowing(),
			WithIncludePath(isOMPSourcePath),
			WithProjectHint(func(root, path string) string { return "" }),
			WithSessionIDFromPath(piSessionIDFromPath),
			WithContentHashing(),
		)
	}
	return NewDirectoryJSONLSourceSet(agent, roots,
		WithSymlinkFollowing(),
		WithIncludePath(isPiSourcePath),
		WithProjectHint(func(root, path string) string { return "" }),
		// Pi/OMP persisted a full-file content hash (file_hash) in the legacy
		// per-agent parse. Without this the provider fingerprint hash is empty
		// and a resync clears the stored file_hash to NULL.
		WithSessionIDFromPath(piSessionIDFromPath),
		WithContentHashing(),
	).JSONLSourceSet
}

func isPiSourcePath(root, path string) bool {
	return strings.HasSuffix(filepath.Base(path), ".jsonl")
}

// isOMPSourcePath accepts OMP transcripts at the main-session depth
// (<project>/<session>.jsonl) and at any deeper subagent depth
// (<project>/<session>/<agent>.jsonl and further nesting). Non-.jsonl
// companions (.md, .bash.log) and root-level files are rejected.
func isOMPSourcePath(root, path string) bool {
	if !strings.HasSuffix(filepath.Base(path), ".jsonl") {
		return false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) < 2 {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func piSessionIDFromPath(root, path string) string {
	if !isPiSourcePath(root, path) {
		return ""
	}
	return strings.TrimSuffix(filepath.Base(path), ".jsonl")
}

func piProviderCapabilities() Capabilities {
	return Capabilities{
		Source: jsonlFileProviderSourceCapabilities(),
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			SessionName:          CapabilitySupported,
			Cwd:                  CapabilitySupported,
			Relationships:        CapabilitySupported,
			Thinking:             CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			PerMessageTokenUsage: CapabilitySupported,
			Model:                CapabilitySupported,
		},
	}
}
