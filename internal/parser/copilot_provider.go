package parser

import (
	"context"
	"crypto/sha256"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var _ Provider = (*copilotProvider)(nil)

type copilotProviderFactory struct {
	def AgentDef
}

func newCopilotProviderFactory(def AgentDef) ProviderFactory {
	return copilotProviderFactory{def: cloneAgentDef(def)}
}

func (f copilotProviderFactory) Definition() AgentDef {
	return cloneAgentDef(f.def)
}

func (f copilotProviderFactory) Capabilities() Capabilities {
	return copilotProviderCapabilities()
}

func (f copilotProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	cfg = cfg.Clone()
	return &copilotProvider{
		ProviderBase: ProviderBase{
			Def:    cloneAgentDef(f.def),
			Caps:   copilotProviderCapabilities(),
			Config: cfg,
		},
		sources: newCopilotSourceSet(cfg.Roots),
	}
}

type copilotProvider struct {
	ProviderBase
	sources copilotSourceSet
}

func (p *copilotProvider) Discover(ctx context.Context) ([]SourceRef, error) {
	return p.sources.Discover(ctx)
}

func (p *copilotProvider) WatchPlan(ctx context.Context) (WatchPlan, error) {
	return p.sources.WatchPlan(ctx)
}

func (p *copilotProvider) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	return p.sources.SourcesForChangedPath(ctx, req)
}

func (p *copilotProvider) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	req = ProviderFindRequestWithRawSessionID(p.Def, req)
	return p.sources.FindSource(ctx, req)
}

func (p *copilotProvider) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	return p.sources.Fingerprint(ctx, source)
}

func (p *copilotProvider) Parse(
	ctx context.Context,
	req ParseRequest,
) (ParseOutcome, error) {
	if err := ctx.Err(); err != nil {
		return ParseOutcome{}, err
	}
	path, ok := p.sources.pathFromSource(req.Source)
	if !ok {
		return ParseOutcome{}, fmt.Errorf("copilot source path unavailable")
	}
	machine := firstNonEmptyJSONLString(req.Machine, p.Config.Machine)
	sess, msgs, usage, err := p.parseSession(path, machine)
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
	if req.Fingerprint.Size > 0 {
		sess.File.Size = req.Fingerprint.Size
	}
	if req.Fingerprint.MTimeNS > 0 {
		sess.File.Mtime = req.Fingerprint.MTimeNS
	}
	sess.UsageEvents = usage
	return ParseOutcome{
		Results: []ParseResultOutcome{{
			Result: ParseResult{
				Session:     *sess,
				Messages:    msgs,
				UsageEvents: usage,
			},
			DataVersion: DataVersionCurrent,
		}},
		ResultSetComplete: true,
	}, nil
}

type copilotSource struct {
	Root string
	Path string
}

type copilotSourceSet struct {
	roots []string
}

func newCopilotSourceSet(roots []string) copilotSourceSet {
	return copilotSourceSet{roots: cleanJSONLRoots(roots)}
}

func (s copilotSourceSet) Discover(ctx context.Context) ([]SourceRef, error) {
	var sources []SourceRef
	seen := make(map[string]struct{})
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, path := range s.discoverSessionPaths(root) {
			source, ok := s.sourceRef(root, path)
			if !ok {
				continue
			}
			addJSONLSource(source, &sources, seen)
		}
	}
	sortJSONLSources(sources)
	return sources, nil
}

// discoverSessionPaths finds all Copilot session file paths under
// <root>/session-state/. It supports both the bare layout (<uuid>.jsonl) and
// the directory layout (<uuid>/events.jsonl); when both exist for the same
// session, the directory layout wins and the bare file is dropped so a session
// is not discovered twice.
func (s copilotSourceSet) discoverSessionPaths(root string) []string {
	if root == "" {
		return nil
	}

	stateDir := filepath.Join(root, copilotStateDir)
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return nil
	}

	dirs := make(map[string]struct{})
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		eventsPath := filepath.Join(stateDir, entry.Name(), "events.jsonl")
		if _, err := os.Stat(eventsPath); err == nil {
			dirs[entry.Name()] = struct{}{}
		}
	}

	var paths []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			candidate := filepath.Join(stateDir, name, "events.jsonl")
			if _, err := os.Stat(candidate); err == nil {
				paths = append(paths, candidate)
			}
			continue
		}
		if stem, ok := strings.CutSuffix(name, ".jsonl"); ok {
			if _, dup := dirs[stem]; dup {
				continue
			}
			paths = append(paths, filepath.Join(stateDir, name))
		}
	}
	return paths
}

func (s copilotSourceSet) WatchPlan(context.Context) (WatchPlan, error) {
	roots := make([]WatchRoot, 0, len(s.roots))
	for _, root := range s.roots {
		stateDir := filepath.Join(root, copilotStateDir)
		roots = append(roots, WatchRoot{
			Path:         stateDir,
			Recursive:    true,
			IncludeGlobs: []string{"*.jsonl", "workspace.yaml"},
			DebounceKey:  string(AgentCopilot) + ":state:" + stateDir,
		})
	}
	return WatchPlan{Roots: roots}, nil
}

func (s copilotSourceSet) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, root := range s.roots {
		source, ok := s.sourceForChangedPath(root, req)
		if ok {
			return []SourceRef{source}, nil
		}
	}
	return nil, nil
}

func (s copilotSourceSet) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	if err := ctx.Err(); err != nil {
		return SourceRef{}, false, err
	}
	for _, path := range []string{req.StoredFilePath, req.FingerprintKey} {
		if path == "" {
			continue
		}
		for _, root := range s.roots {
			if source, ok := s.sourceRef(root, path); ok {
				return source, true, nil
			}
		}
	}
	if req.RawSessionID == "" {
		return SourceRef{}, false, nil
	}
	for _, root := range s.roots {
		path := s.findSourceFile(root, req.RawSessionID)
		if path == "" {
			continue
		}
		if source, ok := s.sourceRef(root, path); ok {
			return source, true, nil
		}
	}
	return SourceRef{}, false, nil
}

// findSourceFile locates a Copilot session file by UUID under root. It checks
// the directory layout (<uuid>/events.jsonl) first, then the bare layout
// (<uuid>.jsonl), so the richer directory form takes precedence. Returns "" for
// invalid IDs or when no file resolves.
func (s copilotSourceSet) findSourceFile(root, rawID string) string {
	if root == "" || !IsValidSessionID(rawID) {
		return ""
	}

	stateDir := filepath.Join(root, copilotStateDir)

	dirFmt := filepath.Join(stateDir, rawID, "events.jsonl")
	if _, err := os.Stat(dirFmt); err == nil {
		return dirFmt
	}

	bare := filepath.Join(stateDir, rawID+".jsonl")
	if _, err := os.Stat(bare); err == nil {
		return bare
	}

	return ""
}

func (s copilotSourceSet) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	if err := ctx.Err(); err != nil {
		return SourceFingerprint{}, err
	}
	path, ok := s.pathFromSource(source)
	if !ok {
		return SourceFingerprint{}, fmt.Errorf("copilot source path unavailable")
	}
	info, err := os.Stat(path)
	if err != nil {
		return SourceFingerprint{}, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.IsDir() {
		return SourceFingerprint{}, fmt.Errorf("stat %s: source is a directory", path)
	}
	size := info.Size()
	mtime := info.ModTime().UnixNano()
	if workspace := copilotWorkspacePath(path); workspace != "" {
		if wsInfo, err := os.Stat(workspace); err == nil {
			size += wsInfo.Size()
			if wsMtime := wsInfo.ModTime().UnixNano(); wsMtime > mtime {
				mtime = wsMtime
			}
		}
	}
	fingerprint := SourceFingerprint{
		Key:     firstNonEmptyJSONLString(source.FingerprintKey, source.Key, path),
		Size:    size,
		MTimeNS: mtime,
	}
	h := sha256.New()
	if err := addCopilotFingerprintPart(h, "events", path, info); err != nil {
		return SourceFingerprint{}, err
	}
	if workspace := copilotWorkspacePath(path); workspace != "" {
		if wsInfo, err := os.Stat(workspace); err == nil && !wsInfo.IsDir() {
			if err := addCopilotFingerprintPart(h, "workspace", workspace, wsInfo); err != nil {
				return SourceFingerprint{}, err
			}
		}
	}
	fingerprint.Hash = fmt.Sprintf("%x", h.Sum(nil))
	return fingerprint, nil
}

func (s copilotSourceSet) pathFromSource(source SourceRef) (string, bool) {
	switch src := source.Opaque.(type) {
	case copilotSource:
		return src.Path, src.Path != ""
	case *copilotSource:
		if src != nil && src.Path != "" {
			return src.Path, true
		}
	}
	for _, candidate := range []string{source.DisplayPath, source.FingerprintKey, source.Key} {
		for _, root := range s.roots {
			if ref, ok := s.sourceRef(root, candidate); ok {
				src := ref.Opaque.(copilotSource)
				return src.Path, true
			}
		}
	}
	return "", false
}

func (s copilotSourceSet) sourceForChangedPath(
	root string,
	req ChangedPathRequest,
) (SourceRef, bool) {
	path := req.Path
	if filepath.Base(path) == "workspace.yaml" {
		return s.sourceRef(root, filepath.Join(filepath.Dir(path), "events.jsonl"))
	}
	if source, ok := s.sourceRef(root, path); ok {
		return source, true
	}
	if !jsonlMissingPathFallbackAllowed(req) {
		return SourceRef{}, false
	}
	if filepath.Base(path) == "events.jsonl" {
		barePath := filepath.Join(
			root,
			copilotStateDir,
			filepath.Base(filepath.Dir(path))+".jsonl",
		)
		if source, ok := s.sourceRef(root, barePath); ok {
			return source, true
		}
	}
	return s.sourceRefForPath(root, path, false)
}

func (s copilotSourceSet) sourceRef(root, path string) (SourceRef, bool) {
	return s.sourceRefForPath(root, path, true)
}

func (s copilotSourceSet) sourceRefForPath(
	root, path string,
	requireRegular bool,
) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rel, ok := relUnder(root, path)
	if !ok || (requireRegular && !IsRegularFile(path)) {
		return SourceRef{}, false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) == 3 &&
		parts[0] == copilotStateDir &&
		parts[2] == "events.jsonl" {
		return s.newSourceRef(root, path), true
	}
	if len(parts) == 2 &&
		parts[0] == copilotStateDir &&
		strings.HasSuffix(parts[1], ".jsonl") {
		stem := strings.TrimSuffix(parts[1], ".jsonl")
		if dirPath := s.findSourceFile(root, stem); dirPath != "" &&
			dirPath != path {
			return s.sourceRef(root, dirPath)
		}
		return s.newSourceRef(root, path), true
	}
	return SourceRef{}, false
}

func (s copilotSourceSet) newSourceRef(root, path string) SourceRef {
	return SourceRef{
		Provider:       AgentCopilot,
		Key:            path,
		DisplayPath:    path,
		FingerprintKey: path,
		Opaque: copilotSource{
			Root: root,
			Path: path,
		},
	}
}

func copilotWorkspacePath(eventsPath string) string {
	if filepath.Base(eventsPath) != "events.jsonl" {
		return ""
	}
	return filepath.Join(filepath.Dir(eventsPath), "workspace.yaml")
}

func addCopilotFingerprintPart(
	h hash.Hash,
	label string,
	path string,
	info os.FileInfo,
) error {
	if _, err := fmt.Fprintf(
		h,
		"%s\x00%s\x00%d\x00%d\x00",
		label,
		path,
		info.Size(),
		info.ModTime().UnixNano(),
	); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("hash %s: %w", path, err)
	}
	return nil
}

func copilotProviderCapabilities() Capabilities {
	return Capabilities{
		Source: SourceCapabilities{
			DiscoverSources:      CapabilitySupported,
			WatchSources:         CapabilitySupported,
			ClassifyChangedPath:  CapabilitySupported,
			FindSource:           CapabilitySupported,
			CompositeFingerprint: CapabilitySupported,
			IncrementalAppend:    CapabilityNotApplicable,
			MultiSessionSource:   CapabilityNotApplicable,
			PerSessionErrors:     CapabilityNotApplicable,
			ExcludedSessions:     CapabilityNotApplicable,
			ForceReplaceOnParse:  CapabilityNotApplicable,
		},
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			Cwd:                  CapabilitySupported,
			Thinking:             CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			PerMessageTokenUsage: CapabilitySupported,
			AggregateUsageEvents: CapabilitySupported,
			Model:                CapabilitySupported,
		},
	}
}
