package parser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var _ Provider = (*positronProvider)(nil)

type positronProviderFactory struct {
	def AgentDef
}

func newPositronProviderFactory(def AgentDef) ProviderFactory {
	return positronProviderFactory{def: cloneAgentDef(def)}
}

func (f positronProviderFactory) Definition() AgentDef {
	return cloneAgentDef(f.def)
}

func (f positronProviderFactory) Capabilities() Capabilities {
	return positronProviderCapabilities()
}

func (f positronProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	cfg = cfg.Clone()
	return &positronProvider{
		ProviderBase: ProviderBase{
			Def:    cloneAgentDef(f.def),
			Caps:   positronProviderCapabilities(),
			Config: cfg,
		},
		sources: newPositronSourceSet(cfg.Roots),
	}
}

type positronProvider struct {
	ProviderBase
	sources positronSourceSet
}

func (p *positronProvider) Discover(ctx context.Context) ([]SourceRef, error) {
	return p.sources.Discover(ctx)
}

func (p *positronProvider) WatchPlan(ctx context.Context) (WatchPlan, error) {
	return p.sources.WatchPlan(ctx)
}

func (p *positronProvider) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	return p.sources.SourcesForChangedPath(ctx, req)
}

func (p *positronProvider) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	req = ProviderFindRequestWithRawSessionID(p.Def, req)
	return p.sources.FindSource(ctx, req)
}

func (p *positronProvider) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	return p.sources.Fingerprint(ctx, source)
}

func (p *positronProvider) Parse(
	ctx context.Context,
	req ParseRequest,
) (ParseOutcome, error) {
	if err := ctx.Err(); err != nil {
		return ParseOutcome{}, err
	}
	path, project, ok := p.sources.pathFromSource(req.Source)
	if !ok {
		return ParseOutcome{}, fmt.Errorf("positron source path unavailable")
	}
	if req.Source.ProjectHint != "" {
		project = req.Source.ProjectHint
	}
	machine := firstNonEmptyJSONLString(req.Machine, p.Config.Machine)
	sess, msgs, err := p.parseSession(path, project, machine)
	if err != nil {
		return ParseOutcome{}, err
	}
	if sess == nil {
		return ParseOutcome{
			ResultSetComplete: true,
			SkipReason:        SkipNoSession,
		}, nil
	}
	if req.Fingerprint.Size > 0 {
		sess.File.Size = req.Fingerprint.Size
	}
	if req.Fingerprint.MTimeNS > 0 {
		sess.File.Mtime = req.Fingerprint.MTimeNS
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

// parseSession parses a Positron Assistant chat session file. The format is
// identical to VSCode Copilot sessions. Returns (nil, nil, nil) if the file is
// empty or contains no meaningful content.
func (p *positronProvider) parseSession(
	path, project, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("stat %s: %w", path, err)
	}

	var data []byte
	if strings.HasSuffix(path, ".jsonl") {
		data, err = reconstructJSONL(path)
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) == 0 {
		return nil, nil, nil
	}

	// Reuse VSCode Copilot parsing logic since formats are identical.
	sess, msgs, err := parseVSCodeCopilotData(data, path, project, machine)
	if err != nil {
		return nil, nil, err
	}
	if sess == nil {
		return nil, nil, nil
	}

	// Override agent type and ID prefix for Positron.
	sess.Agent = AgentPositron
	sess.ID = "positron:" + sess.ID

	sess.File = FileInfo{
		Path:  path,
		Size:  info.Size(),
		Mtime: info.ModTime().UnixNano(),
	}

	return sess, msgs, nil
}

type positronSource struct {
	Root    string
	Path    string
	Project string
}

type positronSourceSet struct {
	roots []string
}

func newPositronSourceSet(roots []string) positronSourceSet {
	return positronSourceSet{roots: cleanJSONLRoots(roots)}
}

func (s positronSourceSet) Discover(ctx context.Context) ([]SourceRef, error) {
	var sources []SourceRef
	seen := make(map[string]struct{})
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, file := range s.discoverSessions(root) {
			source, ok := s.sourceRef(root, file.Path)
			if !ok {
				continue
			}
			source.ProjectHint = file.Project
			addJSONLSource(source, &sources, seen)
		}
	}
	sortJSONLSources(sources)
	return sources, nil
}

// discoverSessions finds all chat session files under a Positron User
// directory. The structure mirrors VSCode:
// <userDir>/workspaceStorage/<hash>/chatSessions/<uuid>.{json,jsonl}. When a
// .jsonl and .json sibling exist for the same UUID, the .jsonl is preferred.
func (s positronSourceSet) discoverSessions(userDir string) []DiscoveredFile {
	if userDir == "" {
		return nil
	}

	var files []DiscoveredFile

	// Scan workspaceStorage/<hash>/chatSessions/*.{json,jsonl}.
	wsDir := filepath.Join(userDir, "workspaceStorage")
	hashDirs, err := os.ReadDir(wsDir)
	if err != nil {
		return nil
	}

	for _, entry := range hashDirs {
		if !entry.IsDir() {
			continue
		}

		hashPath := filepath.Join(wsDir, entry.Name())
		chatDir := filepath.Join(hashPath, "chatSessions")
		sessionFiles, err := os.ReadDir(chatDir)
		if err != nil {
			continue
		}

		project := positronWorkspaceProject(userDir, entry.Name())
		files = append(files,
			discoverVSCodeSessionFiles(
				chatDir, sessionFiles, project, AgentPositron,
			)...,
		)
	}

	return files
}

// findSourceFile locates a Positron session file by its raw ID (prefix already
// stripped), preferring .jsonl over .json. Returns "" when no matching file
// exists.
func (s positronSourceSet) findSourceFile(userDir, rawID string) string {
	if userDir == "" || !IsValidSessionID(rawID) {
		return ""
	}

	wsDir := filepath.Join(userDir, "workspaceStorage")
	hashDirs, err := os.ReadDir(wsDir)
	if err != nil {
		return ""
	}

	for _, entry := range hashDirs {
		if !entry.IsDir() {
			continue
		}
		base := filepath.Join(wsDir, entry.Name(), "chatSessions")
		// Prefer .jsonl over .json.
		for _, ext := range []string{".jsonl", ".json"} {
			candidate := filepath.Join(base, rawID+ext)
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
	}

	return ""
}

func (s positronSourceSet) WatchPlan(context.Context) (WatchPlan, error) {
	roots := make([]WatchRoot, 0, len(s.roots))
	for _, root := range s.roots {
		workspace := filepath.Join(root, "workspaceStorage")
		roots = append(roots, WatchRoot{
			Path:         workspace,
			Recursive:    true,
			IncludeGlobs: []string{"*.json", "*.jsonl"},
			DebounceKey:  string(AgentPositron) + ":workspace:" + workspace,
		})
	}
	return WatchPlan{Roots: roots}, nil
}

func (s positronSourceSet) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, root := range s.roots {
		sources := s.sourcesForWorkspaceManifest(root, req.Path)
		if len(sources) > 0 {
			return sources, nil
		}
		source, ok := s.sourceRefForChangedPath(root, req)
		if ok {
			return []SourceRef{source}, nil
		}
	}
	return nil, nil
}

func (s positronSourceSet) FindSource(
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

func (s positronSourceSet) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	if err := ctx.Err(); err != nil {
		return SourceFingerprint{}, err
	}
	path, _, ok := s.pathFromSource(source)
	if !ok {
		return SourceFingerprint{}, fmt.Errorf("positron source path unavailable")
	}
	info, err := os.Stat(path)
	if err != nil {
		return SourceFingerprint{}, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.IsDir() {
		return SourceFingerprint{}, fmt.Errorf("stat %s: source is a directory", path)
	}
	fingerprint := SourceFingerprint{
		Key:     firstNonEmptyJSONLString(source.FingerprintKey, source.Key, path),
		Size:    info.Size(),
		MTimeNS: info.ModTime().UnixNano(),
	}
	workspacePath := s.workspaceManifestForSource(path)
	if workspacePath != "" {
		if workspaceInfo, err := os.Stat(workspacePath); err == nil {
			fingerprint.Size += workspaceInfo.Size()
			if mtime := workspaceInfo.ModTime().UnixNano(); mtime > fingerprint.MTimeNS {
				fingerprint.MTimeNS = mtime
			}
		}
	}
	fingerprint.Hash, err = vscodeCopilotSourceHash(path, workspacePath)
	if err != nil {
		return SourceFingerprint{}, err
	}
	return fingerprint, nil
}

func (s positronSourceSet) pathFromSource(source SourceRef) (string, string, bool) {
	switch src := source.Opaque.(type) {
	case positronSource:
		return src.Path, src.Project, src.Path != ""
	case *positronSource:
		if src != nil && src.Path != "" {
			return src.Path, src.Project, true
		}
	}
	for _, candidate := range []string{source.DisplayPath, source.FingerprintKey, source.Key} {
		for _, root := range s.roots {
			if ref, ok := s.sourceRef(root, candidate); ok {
				src := ref.Opaque.(positronSource)
				return src.Path, src.Project, true
			}
		}
	}
	return "", "", false
}

func (s positronSourceSet) sourceRef(root, path string) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rel, ok := relUnder(root, path)
	if !ok {
		return SourceRef{}, false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 4 ||
		parts[0] != "workspaceStorage" ||
		parts[2] != "chatSessions" ||
		!isVSCodeCopilotSessionPath(parts[3]) {
		return SourceRef{}, false
	}
	if promoted := vscodeCopilotPreferredExistingPath(path); promoted != "" {
		path = promoted
	}
	if !IsRegularFile(path) {
		return SourceRef{}, false
	}
	project := positronWorkspaceProject(root, parts[1])
	return s.newSourceRef(root, path, project), true
}

func (s positronSourceSet) sourceRefForChangedPath(
	root string,
	req ChangedPathRequest,
) (SourceRef, bool) {
	path := req.Path
	if req.EventKind != "remove" && vscodeCopilotJSONLPreferredOver(path) {
		return SourceRef{}, false
	}
	if source, ok := s.sourceRef(root, path); ok {
		return source, true
	}
	return s.syntheticSourceRef(root, path)
}

func (s positronSourceSet) syntheticSourceRef(
	root, path string,
) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rel, ok := relUnder(root, path)
	if !ok {
		return SourceRef{}, false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 4 ||
		parts[0] != "workspaceStorage" ||
		parts[2] != "chatSessions" ||
		!isVSCodeCopilotSessionPath(parts[3]) {
		return SourceRef{}, false
	}
	if promoted := vscodeCopilotPreferredExistingPath(path); promoted != "" {
		path = promoted
	}
	project := positronWorkspaceProject(root, parts[1])
	return s.newSourceRef(root, path, project), true
}

func (s positronSourceSet) sourcesForWorkspaceManifest(
	root, path string,
) []SourceRef {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rel, ok := relUnder(root, path)
	if !ok {
		return nil
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 3 ||
		parts[0] != "workspaceStorage" ||
		parts[2] != "workspace.json" {
		return nil
	}
	hashDir := filepath.Join(root, "workspaceStorage", parts[1])
	chatDir := filepath.Join(hashDir, "chatSessions")
	entries, err := os.ReadDir(chatDir)
	if err != nil {
		return nil
	}
	project := positronWorkspaceProject(root, parts[1])
	files := discoverVSCodeSessionFiles(chatDir, entries, project, AgentPositron)
	sources := make([]SourceRef, 0, len(files))
	seen := make(map[string]struct{}, len(files))
	for _, file := range files {
		source, ok := s.sourceRef(root, file.Path)
		if !ok {
			continue
		}
		source.Provider = AgentPositron
		source.ProjectHint = file.Project
		addJSONLSource(source, &sources, seen)
	}
	sortJSONLSources(sources)
	return sources
}

func (s positronSourceSet) workspaceManifestForSource(path string) string {
	for _, root := range s.roots {
		root = filepath.Clean(root)
		rel, ok := relUnder(root, path)
		if !ok {
			continue
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) == 4 &&
			parts[0] == "workspaceStorage" &&
			parts[2] == "chatSessions" &&
			isVSCodeCopilotSessionPath(parts[3]) {
			workspacePath := filepath.Join(
				root,
				"workspaceStorage",
				parts[1],
				"workspace.json",
			)
			if IsRegularFile(workspacePath) {
				return workspacePath
			}
		}
	}
	return ""
}

func (s positronSourceSet) newSourceRef(root, path, project string) SourceRef {
	return SourceRef{
		Provider:       AgentPositron,
		Key:            path,
		DisplayPath:    path,
		FingerprintKey: path,
		ProjectHint:    project,
		Opaque: positronSource{
			Root:    root,
			Path:    path,
			Project: project,
		},
	}
}

func positronWorkspaceProject(root, hash string) string {
	hashDir := filepath.Join(root, "workspaceStorage", hash)
	project := ReadVSCodeWorkspaceManifest(hashDir)
	if project == "" {
		project = "unknown"
	}
	return project
}

func positronProviderCapabilities() Capabilities {
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
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			Thinking:             CapabilitySupported,
			AggregateUsageEvents: CapabilitySupported,
			Model:                CapabilitySupported,
		},
	}
}
