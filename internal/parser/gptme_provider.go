package parser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type gptmeProviderFactory struct {
	def AgentDef
}

func newGptmeProviderFactory(def AgentDef) ProviderFactory {
	return gptmeProviderFactory{def: cloneAgentDef(def)}
}

func (f gptmeProviderFactory) Definition() AgentDef {
	return cloneAgentDef(f.def)
}

func (f gptmeProviderFactory) Capabilities() Capabilities {
	return gptmeProviderCapabilities()
}

func (f gptmeProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	cfg = cfg.Clone()
	return &gptmeProvider{
		ProviderBase: ProviderBase{
			Def:    cloneAgentDef(f.def),
			Caps:   gptmeProviderCapabilities(),
			Config: cfg,
		},
		sources: newGptmeSourceSet(cfg.Roots),
	}
}

type gptmeProvider struct {
	ProviderBase
	sources JSONLSourceSet
}

func (p *gptmeProvider) Discover(ctx context.Context) ([]SourceRef, error) {
	sources, err := p.sources.Discover(ctx)
	if err != nil {
		return nil, err
	}
	return p.filterSources(sources), nil
}

func (p *gptmeProvider) WatchPlan(ctx context.Context) (WatchPlan, error) {
	return p.sources.WatchPlan(ctx)
}

func (p *gptmeProvider) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	sources, err := p.sources.SourcesForChangedPath(ctx, req)
	if err != nil {
		return nil, err
	}
	filtered := p.filterSources(sources)
	if len(filtered) > 0 {
		return filtered, nil
	}
	source, ok := p.sourceForEventPath(req)
	if !ok {
		return nil, nil
	}
	return []SourceRef{source}, nil
}

func (p *gptmeProvider) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	if err := ctx.Err(); err != nil {
		return SourceRef{}, false, err
	}
	for _, path := range []string{
		req.StoredFilePath,
		req.FingerprintKey,
	} {
		if path == "" {
			continue
		}
		if source, ok, err := p.sourceForExistingPath(ctx, path); err != nil {
			return SourceRef{}, false, err
		} else if ok {
			return source, true, nil
		}
	}
	for _, id := range []string{
		req.RawSessionID,
		p.rawSessionIDFromFull(req.FullSessionID),
	} {
		if id == "" {
			continue
		}
		if source, ok, err := p.sourceForSessionID(ctx, id); err != nil {
			return SourceRef{}, false, err
		} else if ok {
			return source, true, nil
		}
	}
	return SourceRef{}, false, nil
}

func (p *gptmeProvider) sourceForExistingPath(
	ctx context.Context,
	path string,
) (SourceRef, bool, error) {
	source, ok, err := p.sources.sourceForPath(ctx, path)
	if err != nil {
		return SourceRef{}, false, err
	}
	if ok && p.isSource(source) {
		return source, true, nil
	}
	return SourceRef{}, false, nil
}

func (p *gptmeProvider) sourceForSessionID(
	ctx context.Context,
	id string,
) (SourceRef, bool, error) {
	for _, root := range p.Config.Roots {
		path := filepath.Join(root, id, "conversation.jsonl")
		if source, ok, err := p.sourceForExistingPath(ctx, path); err != nil {
			return SourceRef{}, false, err
		} else if ok {
			return source, true, nil
		}
	}
	return SourceRef{}, false, nil
}

func (p *gptmeProvider) rawSessionIDFromFull(id string) string {
	if id == "" {
		return ""
	}
	_, rawID := StripHostPrefix(id)
	if !strings.HasPrefix(rawID, p.Def.IDPrefix) {
		return ""
	}
	return strings.TrimPrefix(rawID, p.Def.IDPrefix)
}

func (p *gptmeProvider) sourceForEventPath(req ChangedPathRequest) (SourceRef, bool) {
	if req.Path == "" {
		return SourceRef{}, false
	}
	if req.WatchRoot != "" {
		root := filepath.Clean(req.WatchRoot)
		if !p.hasRoot(root) {
			return SourceRef{}, false
		}
		return gptmeSourceRef(root, filepath.Clean(req.Path))
	}
	for _, root := range p.Config.Roots {
		if source, ok := gptmeSourceRef(root, filepath.Clean(req.Path)); ok {
			return source, true
		}
	}
	return SourceRef{}, false
}

func (p *gptmeProvider) hasRoot(root string) bool {
	for _, configured := range p.Config.Roots {
		if samePath(configured, root) {
			return true
		}
	}
	return false
}

func (p *gptmeProvider) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	return p.sources.Fingerprint(ctx, source)
}

func (p *gptmeProvider) Parse(
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
		return ParseOutcome{}, fmt.Errorf("gptme source path unavailable")
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

func (p *gptmeProvider) filterSources(sources []SourceRef) []SourceRef {
	if len(sources) == 0 {
		return nil
	}
	filtered := sources[:0]
	for _, source := range sources {
		if p.isSource(source) {
			filtered = append(filtered, source)
		}
	}
	return filtered
}

func (p *gptmeProvider) isSource(source SourceRef) bool {
	src, ok := source.Opaque.(JSONLSource)
	if !ok {
		return false
	}
	return isGptmeConversationPath(src.Root, src.Path)
}

func newGptmeSourceSet(roots []string) JSONLSourceSet {
	return newJSONLSourceSet(AgentGptme, roots,
		withRecursive(),
		withContentHashing(),
		withSymlinkFollowing(),
		withInclude(func(path string, info os.FileInfo) bool {
			return !info.IsDir() && filepath.Base(path) == "conversation.jsonl"
		}),
		withProjectHint(func(root, path string) string {
			sessionID := gptmeSessionIDFromPath(root, path)
			if sessionID == "" {
				return ""
			}
			return gptmeProjectFromSessionName(sessionID)
		}),
		withSessionIDFromPath(gptmeSessionIDFromPath),
	)
}

func gptmeProviderCapabilities() Capabilities {
	return Capabilities{
		Source: SourceCapabilities{
			DiscoverSources:      CapabilitySupported,
			WatchSources:         CapabilitySupported,
			ClassifyChangedPath:  CapabilitySupported,
			FindSource:           CapabilitySupported,
			CompositeFingerprint: CapabilitySupported,
			MultiSessionSource:   CapabilityNotApplicable,
			PerSessionErrors:     CapabilityNotApplicable,
			ExcludedSessions:     CapabilityNotApplicable,
			ForceReplaceOnParse:  CapabilityNotApplicable,
		},
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			Model:                CapabilitySupported,
			PerMessageTokenUsage: CapabilitySupported,
		},
	}
}

func isGptmeConversationPath(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	return len(parts) == 2 && parts[1] == "conversation.jsonl" &&
		parts[0] != "." && parts[0] != ".." && parts[0] != ""
}

func gptmeSessionIDFromPath(root, path string) string {
	if !isGptmeConversationPath(root, path) {
		return ""
	}
	return filepath.Base(filepath.Dir(path))
}

func gptmeSourceRef(root, path string) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if !isGptmeConversationPath(root, path) {
		return SourceRef{}, false
	}
	sessionID := gptmeSessionIDFromPath(root, path)
	return SourceRef{
		Provider:       AgentGptme,
		Key:            path,
		DisplayPath:    path,
		FingerprintKey: path,
		ProjectHint:    gptmeProjectFromSessionName(sessionID),
		Opaque: JSONLSource{
			Root:    root,
			Path:    path,
			RelPath: filepath.Join(sessionID, "conversation.jsonl"),
		},
	}, true
}
