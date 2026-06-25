package parser

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var _ Provider = (*cortexProvider)(nil)

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

func (p *cortexProvider) WatchPlan(ctx context.Context) (WatchPlan, error) {
	plan, err := p.sources.WatchPlan(ctx)
	if err != nil {
		return WatchPlan{}, err
	}
	for i := range plan.Roots {
		plan.Roots[i].IncludeGlobs = append(
			plan.Roots[i].IncludeGlobs,
			"*.history.jsonl",
		)
	}
	return plan, nil
}

func (p *cortexProvider) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	sources, err := p.sources.SourcesForChangedPath(ctx, req)
	if err != nil || len(sources) > 0 {
		return sources, err
	}
	if source, ok, err := p.sourceForHistoryCompanion(ctx, req); err != nil {
		return nil, err
	} else if ok {
		return []SourceRef{source}, nil
	}
	return nil, nil
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
	if err := ctx.Err(); err != nil {
		return SourceFingerprint{}, err
	}
	path, ok, err := p.sources.pathFromSource(ctx, source)
	if err != nil {
		return SourceFingerprint{}, err
	}
	if !ok {
		return SourceFingerprint{}, fmt.Errorf("cortex source path unavailable")
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
	if err := addCortexFingerprintPart(h, "metadata", path, info); err != nil {
		return SourceFingerprint{}, err
	}
	historyPath := cortexHistoryCompanionPath(path)
	if historyInfo, ok, err := cortexCompanionInfo(historyPath); err != nil {
		return SourceFingerprint{}, err
	} else if ok && historyInfo != nil {
		fingerprint.Size += historyInfo.Size()
		mtime := historyInfo.ModTime().UnixNano()
		if mtime > fingerprint.MTimeNS {
			fingerprint.MTimeNS = mtime
		}
		if err := addCortexFingerprintPart(h, "history", historyPath, historyInfo); err != nil {
			return SourceFingerprint{}, err
		}
	}
	fingerprint.Hash = fmt.Sprintf("%x", h.Sum(nil))
	return fingerprint, nil
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
	)
}

func (p *cortexProvider) sourceForHistoryCompanion(
	ctx context.Context,
	req ChangedPathRequest,
) (SourceRef, bool, error) {
	if req.Path == "" {
		return SourceRef{}, false, nil
	}
	path := filepath.Clean(req.Path)
	for _, root := range p.sources.roots {
		if req.WatchRoot != "" && !samePath(req.WatchRoot, root) {
			continue
		}
		source, ok, err := cortexSourceForHistoryCompanion(ctx, p.sources, root, path)
		if err != nil {
			return SourceRef{}, false, err
		}
		if ok {
			return source, true, nil
		}
	}
	return SourceRef{}, false, nil
}

func cortexSourceForHistoryCompanion(
	ctx context.Context,
	sources JSONLSourceSet,
	root string,
	path string,
) (SourceRef, bool, error) {
	root = filepath.Clean(root)
	if !samePath(filepath.Dir(path), root) {
		return SourceRef{}, false, nil
	}
	stem, ok := strings.CutSuffix(filepath.Base(path), ".history.jsonl")
	if !ok || !IsCortexSessionFile(stem+".json") {
		return SourceRef{}, false, nil
	}
	metadataPath := filepath.Join(root, stem+".json")
	if source, ok, err := sources.sourceForPath(ctx, metadataPath); err != nil {
		return SourceRef{}, false, err
	} else if ok {
		return source, true, nil
	}
	return SourceRef{}, false, nil
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

func cortexCompanionInfo(path string) (os.FileInfo, bool, error) {
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

func addCortexFingerprintPart(
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

func cortexProviderCapabilities() Capabilities {
	return Capabilities{
		Source: jsonlFileProviderSourceCapabilities(),
		Content: ContentCapabilities{
			FirstMessage: CapabilitySupported,
			SessionName:  CapabilitySupported,
			Cwd:          CapabilitySupported,
			ToolCalls:    CapabilitySupported,
			ToolResults:  CapabilitySupported,
		},
	}
}
