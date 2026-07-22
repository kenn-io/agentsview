package parser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var (
	_ Provider         = (*commandCodeProvider)(nil)
	_ WatchRootPlanner = (*commandCodeProvider)(nil)
)

type commandCodeProviderFactory struct {
	def AgentDef
}

func newCommandCodeProviderFactory(def AgentDef) ProviderFactory {
	return commandCodeProviderFactory{def: cloneAgentDef(def)}
}

func (f commandCodeProviderFactory) Definition() AgentDef {
	return cloneAgentDef(f.def)
}

func (f commandCodeProviderFactory) Capabilities() Capabilities {
	return commandCodeProviderCapabilities()
}

func (f commandCodeProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	cfg = cfg.Clone()
	return &commandCodeProvider{
		ProviderBase: ProviderBase{
			Def:    cloneAgentDef(f.def),
			Caps:   commandCodeProviderCapabilities(),
			Config: cfg,
		},
		sources: newCommandCodeSourceSet(cfg.Roots),
	}
}

type commandCodeProvider struct {
	ProviderBase
	sources DirectoryJSONLSourceSet
}

func (p *commandCodeProvider) Discover(ctx context.Context) ([]SourceRef, error) {
	return p.sources.Discover(ctx)
}

func (p *commandCodeProvider) DiscoverEach(ctx context.Context, yield func(SourceRef) error) error {
	return p.sources.DiscoverEach(ctx, yield)
}

func (p *commandCodeProvider) WatchPlan(ctx context.Context) (WatchPlan, error) {
	return p.sources.WatchPlan(ctx)
}

func (p *commandCodeProvider) WatchRoots(
	ctx context.Context,
) ([]WatchRoot, error) {
	return p.sources.WatchRoots(ctx)
}

func (p *commandCodeProvider) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	return p.sources.SourcesForChangedPath(ctx, req)
}

func (p *commandCodeProvider) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	return p.sources.FindSource(ctx, ProviderFindRequestWithRawSessionID(p.Def, req))
}

func (p *commandCodeProvider) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	return p.sources.Fingerprint(ctx, source)
}

func (p *commandCodeProvider) Parse(
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
		return ParseOutcome{}, fmt.Errorf("commandcode source path unavailable")
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
	// Use the shared fingerprint hash (which folds the .meta.json companion via
	// WithCompanionFiles + WithContentHashing) rather than recomputing a bespoke
	// transcript-only hash, so file_hash stays consistent with the fingerprint.
	if req.Fingerprint.Hash != "" {
		sess.File.Hash = req.Fingerprint.Hash
	}
	// Mirror the legacy effective-info behavior: the transcript's
	// freshness identity (size and mtime) includes the .meta.json
	// companion so a title-only rename triggers a reparse.
	if size, mtime, ok := commandCodeEffectiveFileInfo(path); ok {
		sess.File.Size = size
		sess.File.Mtime = mtime
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

func newCommandCodeSourceSet(roots []string) DirectoryJSONLSourceSet {
	return NewDirectoryJSONLSourceSet(AgentCommandCode, roots,
		WithSymlinkFollowing(),
		WithIncludePath(isCommandCodeSourcePath),
		WithProjectHint(func(root, path string) string { return "" }),
		WithSessionIDFromPath(commandCodeSessionIDFromPath),
		// Command Code's .meta.json sidecar participates in source freshness, so
		// fold it into the shared fingerprint (size, mtime, and content hash) via
		// the framework companion hook instead of a bespoke Fingerprint.
		// WithContentHashing preserves the legacy per-agent file_hash, which a
		// resync would otherwise clear to NULL.
		WithCompanionFiles(func(transcriptPath string) []string {
			return []string{commandCodeMetaCompanionPath(transcriptPath)}
		}),
		WithCompanionTranscript(func(companionPath string) (string, bool) {
			stem, ok := strings.CutSuffix(companionPath, ".meta.json")
			return stem + ".jsonl", ok
		}),
		WithContentHashing(),
	)
}

func isCommandCodeSourcePath(root, path string) bool {
	name := filepath.Base(path)
	if !strings.HasSuffix(name, ".jsonl") ||
		strings.HasSuffix(name, ".checkpoints.jsonl") ||
		strings.HasSuffix(name, ".prompts.jsonl") {
		return false
	}
	return IsValidSessionID(strings.TrimSuffix(name, ".jsonl"))
}

func commandCodeSessionIDFromPath(root, path string) string {
	name := filepath.Base(path)
	if !isCommandCodeSourcePath(root, path) {
		return ""
	}
	return strings.TrimSuffix(name, ".jsonl")
}

func commandCodeMetaCompanionPath(path string) string {
	return strings.TrimSuffix(path, ".jsonl") + ".meta.json"
}

// commandCodeEffectiveFileInfo returns the combined size and mtime of the
// transcript and its optional .meta.json companion. The bool is false only
// when the transcript itself cannot be stat'd.
func commandCodeEffectiveFileInfo(path string) (int64, int64, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, false
	}
	size := info.Size()
	mtime := info.ModTime().UnixNano()
	if metaInfo, ok, err := commandCodeCompanionInfo(
		commandCodeMetaCompanionPath(path),
	); err == nil && ok && metaInfo != nil {
		size += metaInfo.Size()
		if metaMtime := metaInfo.ModTime().UnixNano(); metaMtime > mtime {
			mtime = metaMtime
		}
	}
	return size, mtime, true
}

func commandCodeCompanionInfo(path string) (os.FileInfo, bool, error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.IsDir() {
		return nil, false, nil
	}
	return info, true, nil
}

func commandCodeProviderCapabilities() Capabilities {
	source := jsonlFileProviderSourceCapabilities()
	source.StreamingDiscovery = CapabilitySupported
	source.WatchRoots = CapabilitySupported
	return Capabilities{
		Source: source,
		Content: ContentCapabilities{
			FirstMessage:       CapabilitySupported,
			SessionName:        CapabilitySupported,
			Cwd:                CapabilitySupported,
			GitBranch:          CapabilitySupported,
			Thinking:           CapabilitySupported,
			ToolCalls:          CapabilitySupported,
			ToolResults:        CapabilitySupported,
			MalformedLineCount: CapabilitySupported,
		},
	}
}
