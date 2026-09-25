package parser

import (
	"context"
	"errors"
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
	return piProviderCapabilities(f.def.Type)
}

func (f piProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	cfg = cfg.Clone()
	return &piProvider{
		Def:     cloneAgentDef(f.def),
		Caps:    piProviderCapabilities(f.def.Type),
		Config:  cfg,
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

func (p *piProvider) DiscoverEach(ctx context.Context, yield func(SourceRef) error) error {
	return p.sources.DiscoverEach(ctx, func(source SourceRef) error {
		src, ok := source.Opaque.(JSONLSource)
		if !ok || !IsPiSessionFile(src.Path) {
			return nil
		}
		return yield(source)
	})
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
			if p.Def.Type != AgentPrimeAgent || req.RawSessionID == "" {
				return source, true, nil
			}
			src, isJSONL := source.Opaque.(JSONLSource)
			if !isJSONL {
				continue
			}
			headerID, valid := piSessionHeaderID(src.Path)
			if valid && (headerID == req.RawSessionID ||
				(headerID == "" &&
					piSessionIDFromPath("", src.Path) == req.RawSessionID)) {
				return source, true, nil
			}
		}
	}
	if req.RawSessionID == "" || !IsValidSessionID(req.RawSessionID) {
		return SourceRef{}, false, nil
	}
	if p.Def.Type == AgentPrimeAgent {
		return p.sourceForPrimeSessionID(ctx, req.RawSessionID)
	}
	if p.Def.Type == AgentOMP {
		return p.sourceForHeaderSessionID(ctx, req.RawSessionID)
	}
	for _, root := range p.Config.Roots {
		source, ok, err := p.sourceForSessionID(ctx, root, req.RawSessionID)
		if err != nil || ok {
			return source, ok, err
		}
	}
	// Native Pi default filenames are timestamp-prefixed, so a bare header
	// UUID lookup finds nothing by filename. Fall back to scanning session
	// headers only after the filename/directory lookup misses, so files with
	// no header still resolve by their filename-derived identity.
	if p.Def.Type == AgentPi {
		return p.sourceForHeaderSessionID(ctx, req.RawSessionID)
	}
	return SourceRef{}, false, nil
}

func (p *piProvider) sourceForPrimeSessionID(
	ctx context.Context,
	sessionID string,
) (SourceRef, bool, error) {
	for _, root := range p.Config.Roots {
		direct := filepath.Join(root, sessionID+".jsonl")
		source, ok, err := p.sources.sourceForPath(ctx, direct)
		if err != nil {
			return SourceRef{}, false, err
		}
		if !ok {
			continue
		}
		src, ok := source.Opaque.(JSONLSource)
		if !ok {
			continue
		}
		headerID, valid := piSessionHeaderID(src.Path)
		if valid && (headerID == "" || headerID == sessionID) {
			return source, true, nil
		}
	}
	return p.sourceForHeaderSessionID(ctx, sessionID)
}

func (p *piProvider) sourceForSessionID(
	ctx context.Context,
	root string,
	sessionID string,
) (SourceRef, bool, error) {
	direct := filepath.Join(root, sessionID+".jsonl")
	if source, ok, err := p.sources.sourceForPath(ctx, direct); err != nil || ok {
		return source, ok, err
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return SourceRef{}, false, nil //nolint:nilerr // Unavailable optional discovery roots have no matching source.
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

func (p *piProvider) sourceForHeaderSessionID(
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
		headerID, ok := piSessionHeaderID(src.Path)
		if !ok {
			continue
		}
		if headerID == sessionID ||
			(headerID == "" && piSessionIDFromPath("", src.Path) == sessionID) {
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
		return ParseOutcome{}, errors.New("pi source path unavailable")
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
		ForceReplace:      p.Def.Type == AgentPrimeAgent,
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
	// Prime Agent writes current sessions directly under its flat sessions
	// root. Its producer migrates the older per-project layout before normal
	// session listing, so discovery mirrors the current persisted boundary.
	if agent == AgentPrimeAgent {
		return NewJSONLSourceSet(agent, roots,
			WithFollowSymlinkFiles(),
			WithIncludePath(isPiSourcePath),
			WithProjectHint(func(root, path string) string { return "" }),
			WithSessionIDFromPath(piSessionIDFromPath),
			WithContentHashing(),
		)
	}

	// Pi's native session-dir override writes transcripts directly into the
	// chosen directory, and default homes group them by project. The
	// pi-subagents extension nests subagent runs several levels below the
	// project directory, so transcript depth is not capped.
	if agent == AgentPi {
		return NewJSONLSourceSet(agent, roots,
			WithRecursive(),
			WithSymlinkFollowing(),
			WithIncludePath(isPiSourcePathAtAnyDepth),
			WithProjectHint(func(root, path string) string { return "" }),
			WithSessionIDFromPath(piSessionIDFromPath),
			WithContentHashing(),
		)
	}

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

// isPiSourcePathAtAnyDepth accepts a Pi transcript anywhere under a configured
// sessions root: directly inside it (the native session-dir override), one
// project directory down (default homes), or deeper, where pi-subagents keeps
// each run's child session under the parent transcript's name
// (<project>/<parent>/<runId>/run-N/session.jsonl). A delegated child can
// spawn its own children, so the depth is not capped; only empty and traversal
// segments are rejected. IsPiSessionFile still filters the file contents, so a
// nested .jsonl that is not a Pi session is not indexed.
func isPiSourcePathAtAnyDepth(root, path string) bool {
	if !isPiSourcePath(root, path) {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil {
		return false
	}
	for part := range strings.SplitSeq(rel, string(filepath.Separator)) {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
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

func piProviderCapabilities(agent AgentType) Capabilities {
	caps := Capabilities{
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
	caps.Source.StreamingDiscovery = CapabilitySupported
	if agent == AgentPrimeAgent {
		caps.Source.ForceReplaceOnParse = CapabilitySupported
	}
	return caps
}
