package parser

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var _ Provider = (*commandCodeProvider)(nil)

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

func (p *commandCodeProvider) WatchPlan(ctx context.Context) (WatchPlan, error) {
	plan, err := p.sources.WatchPlan(ctx)
	if err != nil {
		return WatchPlan{}, err
	}
	for i := range plan.Roots {
		plan.Roots[i].IncludeGlobs = append(
			plan.Roots[i].IncludeGlobs,
			"*.meta.json",
		)
	}
	return plan, nil
}

func (p *commandCodeProvider) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	sources, err := p.sources.SourcesForChangedPath(ctx, req)
	if err != nil || len(sources) > 0 {
		return sources, err
	}
	if source, ok, err := p.sourceForMetaCompanion(ctx, req); err != nil {
		return nil, err
	} else if ok {
		return []SourceRef{source}, nil
	}
	return nil, nil
}

func (p *commandCodeProvider) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	return p.sources.FindSource(ctx, providerFindRequestWithRawSessionID(p.Def, req))
}

func (p *commandCodeProvider) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	if err := ctx.Err(); err != nil {
		return SourceFingerprint{}, err
	}
	path, ok, err := p.sources.pathFromSource(ctx, source)
	if err != nil {
		return SourceFingerprint{}, err
	}
	if !ok {
		return SourceFingerprint{}, fmt.Errorf("commandcode source path unavailable")
	}
	info, err := os.Stat(path)
	if err != nil {
		return SourceFingerprint{}, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.IsDir() {
		return SourceFingerprint{}, fmt.Errorf("stat %s: source is a directory", path)
	}
	fingerprint := SourceFingerprint{
		Key: firstNonEmptyJSONLString(
			source.FingerprintKey,
			source.Key,
			path,
		),
		Size:    info.Size(),
		MTimeNS: info.ModTime().UnixNano(),
	}

	h := sha256.New()
	if err := addCommandCodeFingerprintPart(h, "transcript", path, info); err != nil {
		return SourceFingerprint{}, err
	}
	metaPath := commandCodeMetaCompanionPath(path)
	if metaInfo, ok, err := commandCodeCompanionInfo(metaPath); err != nil {
		return SourceFingerprint{}, err
	} else if ok && metaInfo != nil {
		fingerprint.Size += metaInfo.Size()
		if mtime := metaInfo.ModTime().UnixNano(); mtime > fingerprint.MTimeNS {
			fingerprint.MTimeNS = mtime
		}
		if err := addCommandCodeFingerprintPart(h, "meta", metaPath, metaInfo); err != nil {
			return SourceFingerprint{}, err
		}
	}
	fingerprint.Hash = fmt.Sprintf("%x", h.Sum(nil))
	return fingerprint, nil
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
	if hash, err := hashJSONLSourceFile(path); err == nil {
		sess.File.Hash = hash
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
	return newDirectoryJSONLSourceSet(AgentCommandCode, roots,
		withSymlinkFollowing(),
		withIncludePath(isCommandCodeSourcePath),
		withProjectHint(func(root, path string) string { return "" }),
		withSessionIDFromPath(commandCodeSessionIDFromPath),
	)
}

func (p *commandCodeProvider) sourceForMetaCompanion(
	ctx context.Context,
	req ChangedPathRequest,
) (SourceRef, bool, error) {
	if req.Path == "" {
		return SourceRef{}, false, nil
	}
	path := filepath.Clean(req.Path)
	stem, ok := strings.CutSuffix(filepath.Base(path), ".meta.json")
	if !ok || !IsValidSessionID(stem) {
		return SourceRef{}, false, nil
	}
	transcriptPath := filepath.Join(filepath.Dir(path), stem+".jsonl")
	if _, err := os.Stat(transcriptPath); err != nil {
		return SourceRef{}, false, nil
	}
	source, ok, err := p.sources.sourceForPath(ctx, transcriptPath)
	if err != nil {
		return SourceRef{}, false, err
	}
	if !ok {
		return SourceRef{}, false, nil
	}
	if req.WatchRoot != "" {
		root := filepath.Clean(req.WatchRoot)
		src := source.Opaque.(JSONLSource)
		if !samePath(root, src.Root) {
			return SourceRef{}, false, nil
		}
	}
	return source, true, nil
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

func addCommandCodeFingerprintPart(
	h interface{ Write([]byte) (int, error) },
	label string,
	path string,
	info os.FileInfo,
) error {
	hash, err := hashJSONLSourceFile(path)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(
		h,
		"%s:%s:%d:%d:%s\n",
		label,
		filepath.Base(path),
		info.Size(),
		info.ModTime().UnixNano(),
		hash,
	)
	return nil
}

func commandCodeProviderCapabilities() Capabilities {
	return Capabilities{
		Source: jsonlFileProviderSourceCapabilities(),
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
