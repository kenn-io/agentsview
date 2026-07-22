package parser

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var _ Provider = (*vscodeCopilotProvider)(nil)

type vscodeCopilotProviderFactory struct {
	def AgentDef
}

func newVSCodeCopilotProviderFactory(def AgentDef) ProviderFactory {
	return vscodeCopilotProviderFactory{def: cloneAgentDef(def)}
}

func (f vscodeCopilotProviderFactory) Definition() AgentDef {
	return cloneAgentDef(f.def)
}

func (f vscodeCopilotProviderFactory) Capabilities() Capabilities {
	return vscodeCopilotProviderCapabilities()
}

func (f vscodeCopilotProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	cfg = cfg.Clone()
	return &vscodeCopilotProvider{
		ProviderBase: ProviderBase{
			Def:    cloneAgentDef(f.def),
			Caps:   vscodeCopilotProviderCapabilities(),
			Config: cfg,
		},
		sources: newVSCodeCopilotSourceSet(cfg.Roots),
	}
}

type vscodeCopilotProvider struct {
	ProviderBase
	sources vscodeCopilotSourceSet
}

func (p *vscodeCopilotProvider) Discover(ctx context.Context) ([]SourceRef, error) {
	return p.sources.Discover(ctx)
}

func (p *vscodeCopilotProvider) DiscoverEach(ctx context.Context, yield func(SourceRef) error) error {
	return p.sources.DiscoverEach(ctx, yield)
}

func (p *vscodeCopilotProvider) WatchPlan(ctx context.Context) (WatchPlan, error) {
	return p.sources.WatchPlan(ctx)
}

func (p *vscodeCopilotProvider) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	return p.sources.SourcesForChangedPath(ctx, req)
}

func (p *vscodeCopilotProvider) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	req = ProviderFindRequestWithRawSessionID(p.Def, req)
	return p.sources.FindSource(ctx, req)
}

func (p *vscodeCopilotProvider) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	return p.sources.Fingerprint(ctx, source)
}

func (p *vscodeCopilotProvider) Parse(
	ctx context.Context,
	req ParseRequest,
) (ParseOutcome, error) {
	if err := ctx.Err(); err != nil {
		return ParseOutcome{}, err
	}
	path, project, ok := p.sources.pathFromSource(req.Source)
	if !ok {
		return ParseOutcome{}, fmt.Errorf("vscode copilot source path unavailable")
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
	if req.Fingerprint.Hash != "" {
		sess.File.Hash = req.Fingerprint.Hash
		sess.File.Size = req.Fingerprint.Size
		sess.File.Mtime = req.Fingerprint.MTimeNS
	}
	return ParseOutcome{
		Results: []ParseResultOutcome{{
			Result: ParseResult{
				Session:     *sess,
				Messages:    msgs,
				UsageEvents: sess.UsageEvents,
			},
			DataVersion: DataVersionCurrent,
		}},
		ResultSetComplete: true,
	}, nil
}

type vscodeCopilotSource struct {
	Root    string
	Path    string
	Project string
}

type vscodeCopilotSourceSet struct {
	roots []string
}

func newVSCodeCopilotSourceSet(roots []string) vscodeCopilotSourceSet {
	return vscodeCopilotSourceSet{roots: cleanJSONLRoots(roots)}
}

func (s vscodeCopilotSourceSet) Discover(ctx context.Context) ([]SourceRef, error) {
	var sources []SourceRef
	seen := make(map[string]struct{})
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, file := range s.discoverSessionFiles(root) {
			// discoverSessionFiles already resolved the workspace project once
			// per workspace dir; thread it so sourceRef does not re-read the
			// workspace manifest for every session.
			source, ok := s.sourceRefWithProject(root, file.Path, file.Project)
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

func (s vscodeCopilotSourceSet) DiscoverEach(ctx context.Context, yield func(SourceRef) error) error {
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return err
		}
		yieldDir := func(dir, project string) error {
			return streamVSCodeSessionFiles(ctx, dir, project, AgentVSCodeCopilot, func(file DiscoveredFile) error {
				source, ok := s.sourceRefWithProject(root, file.Path, file.Project)
				if !ok {
					return nil
				}
				source.ProjectHint = file.Project
				return yield(source)
			})
		}
		workspaceRoot := filepath.Join(root, "workspaceStorage")
		if err := streamDirectoryEntries(ctx, workspaceRoot, func(entry os.DirEntry) error {
			if !entry.IsDir() {
				return nil
			}
			hashPath := filepath.Join(workspaceRoot, entry.Name())
			project := readVSCodeWorkspaceManifest(hashPath)
			if project == "" {
				project = "unknown"
			}
			return yieldDir(filepath.Join(hashPath, "chatSessions"), project)
		}); err != nil {
			return err
		}
		for _, subdir := range []string{
			"globalStorage/emptyWindowChatSessions",
			"globalStorage/transferredChatSessions",
		} {
			if err := yieldDir(filepath.Join(root, subdir), "empty-window"); err != nil {
				return err
			}
		}
	}
	return nil
}

func streamVSCodeSessionFiles(
	ctx context.Context, dir, project string, agent AgentType,
	yield func(DiscoveredFile) error,
) error {
	err := streamDirectoryEntries(ctx, dir, func(entry os.DirEntry) error {
		if entry.IsDir() {
			return nil
		}
		name := entry.Name()
		ext := filepath.Ext(name)
		if ext != ".json" && ext != ".jsonl" {
			return nil
		}
		stem := strings.TrimSuffix(name, ext)
		if !IsValidSessionID(stem) {
			return nil
		}
		if ext == ".json" && IsRegularFile(filepath.Join(dir, stem+".jsonl")) {
			return nil
		}
		return yield(DiscoveredFile{
			Path: filepath.Join(dir, name), Project: project, Agent: agent,
		})
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// discoverSessionFiles traverses the VSCode workspaceStorage directory to find
// chatSessions/*.json and *.jsonl files. When both formats exist for the same
// session UUID, the .jsonl file takes priority. It also checks
// globalStorage/emptyWindowChatSessions and transferredChatSessions. The root
// should point to e.g.
//
//	~/Library/Application Support/Code/User (macOS)
//	~/.config/Code/User (Linux)
func (s vscodeCopilotSourceSet) discoverSessionFiles(
	vscodeUserDir string,
) []DiscoveredFile {
	if vscodeUserDir == "" {
		return nil
	}

	var files []DiscoveredFile

	// 1. Scan workspaceStorage/<hash>/chatSessions/*.{json,jsonl}
	wsDir := filepath.Join(vscodeUserDir, "workspaceStorage")
	hashDirs, err := os.ReadDir(wsDir)
	if err == nil {
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

			// Read workspace.json to get project name
			project := readVSCodeWorkspaceManifest(hashPath)
			if project == "" {
				project = "unknown"
			}

			files = append(files,
				discoverVSCodeSessionFiles(
					chatDir, sessionFiles, project,
					AgentVSCodeCopilot,
				)...,
			)
		}
	}

	// 2. Scan globalStorage/emptyWindowChatSessions/*.{json,jsonl}
	for _, subdir := range []string{
		"globalStorage/emptyWindowChatSessions",
		"globalStorage/transferredChatSessions",
	} {
		globalDir := filepath.Join(vscodeUserDir, subdir)
		globalFiles, err := os.ReadDir(globalDir)
		if err != nil {
			continue
		}
		files = append(files,
			discoverVSCodeSessionFiles(
				globalDir, globalFiles, "empty-window",
				AgentVSCodeCopilot,
			)...,
		)
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return files
}

func (s vscodeCopilotSourceSet) WatchPlan(context.Context) (WatchPlan, error) {
	roots := make([]WatchRoot, 0, len(s.roots)*2)
	for _, root := range s.roots {
		workspace := filepath.Join(root, "workspaceStorage")
		roots = append(roots, WatchRoot{
			Path:         workspace,
			Recursive:    true,
			IncludeGlobs: []string{"*.json", "*.jsonl"},
			DebounceKey:  string(AgentVSCodeCopilot) + ":workspace:" + workspace,
		})
		global := filepath.Join(root, "globalStorage")
		roots = append(roots, WatchRoot{
			Path:         global,
			Recursive:    true,
			IncludeGlobs: []string{"*.json", "*.jsonl"},
			DebounceKey:  string(AgentVSCodeCopilot) + ":global:" + global,
		})
	}
	return WatchPlan{Roots: roots}, nil
}

func (s vscodeCopilotSourceSet) SourcesForChangedPath(
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

func (s vscodeCopilotSourceSet) FindSource(
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

// findSourceFile locates a VSCode Copilot session file by UUID (.jsonl
// preferred over .json) across workspaceStorage and the global session dirs.
func (s vscodeCopilotSourceSet) findSourceFile(
	vscodeUserDir, rawID string,
) string {
	if vscodeUserDir == "" || !IsValidSessionID(rawID) {
		return ""
	}

	// Search through workspaceStorage
	wsDir := filepath.Join(vscodeUserDir, "workspaceStorage")
	hashDirs, err := os.ReadDir(wsDir)
	if err == nil {
		for _, entry := range hashDirs {
			if !entry.IsDir() {
				continue
			}
			base := filepath.Join(
				wsDir, entry.Name(), "chatSessions",
			)
			// Prefer .jsonl
			for _, ext := range []string{".jsonl", ".json"} {
				candidate := filepath.Join(
					base, rawID+ext,
				)
				if _, err := os.Stat(candidate); err == nil {
					return candidate
				}
			}
		}
	}

	// Check global dirs
	for _, subdir := range []string{
		"globalStorage/emptyWindowChatSessions",
		"globalStorage/transferredChatSessions",
	} {
		base := filepath.Join(vscodeUserDir, subdir)
		for _, ext := range []string{".jsonl", ".json"} {
			candidate := filepath.Join(base, rawID+ext)
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
	}

	return ""
}

func (s vscodeCopilotSourceSet) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	if err := ctx.Err(); err != nil {
		return SourceFingerprint{}, err
	}
	path, _, ok := s.pathFromSource(source)
	if !ok {
		return SourceFingerprint{}, fmt.Errorf("vscode copilot source path unavailable")
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

func (s vscodeCopilotSourceSet) pathFromSource(source SourceRef) (string, string, bool) {
	switch src := source.Opaque.(type) {
	case vscodeCopilotSource:
		return src.Path, src.Project, src.Path != ""
	case *vscodeCopilotSource:
		if src != nil && src.Path != "" {
			return src.Path, src.Project, true
		}
	}
	for _, candidate := range []string{source.DisplayPath, source.FingerprintKey, source.Key} {
		for _, root := range s.roots {
			if ref, ok := s.sourceRef(root, candidate); ok {
				src := ref.Opaque.(vscodeCopilotSource)
				return src.Path, src.Project, true
			}
		}
	}
	return "", "", false
}

func (s vscodeCopilotSourceSet) sourceRef(root, path string) (SourceRef, bool) {
	return s.sourceRefWithProject(root, path, "")
}

// sourceRefWithProject builds a SourceRef using a caller-supplied workspace
// project. Discovery resolves the project once per workspace dir and threads it
// for every session under that dir; single-path callers pass "" and the
// workspace manifest is read for that one path.
func (s vscodeCopilotSourceSet) sourceRefWithProject(
	root, path, project string,
) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rel, ok := relUnder(root, path)
	if !ok {
		return SourceRef{}, false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) == 4 &&
		parts[0] == "workspaceStorage" &&
		parts[2] == "chatSessions" &&
		isVSCodeCopilotSessionPath(parts[3]) {
		if promoted := vscodeCopilotPreferredExistingPath(path); promoted != "" {
			path = promoted
		}
		if !IsRegularFile(path) {
			return SourceRef{}, false
		}
		if project == "" {
			hashDir := filepath.Join(root, "workspaceStorage", parts[1])
			project = readVSCodeWorkspaceManifest(hashDir)
		}
		if project == "" {
			project = "unknown"
		}
		return s.newSourceRef(root, path, project), true
	}
	if len(parts) == 3 &&
		parts[0] == "globalStorage" &&
		(parts[1] == "emptyWindowChatSessions" ||
			parts[1] == "transferredChatSessions") &&
		isVSCodeCopilotSessionPath(parts[2]) {
		if promoted := vscodeCopilotPreferredExistingPath(path); promoted != "" {
			path = promoted
		}
		if !IsRegularFile(path) {
			return SourceRef{}, false
		}
		return s.newSourceRef(root, path, "empty-window"), true
	}
	return SourceRef{}, false
}

func (s vscodeCopilotSourceSet) sourceRefForChangedPath(
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

func (s vscodeCopilotSourceSet) syntheticSourceRef(
	root, path string,
) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rel, ok := relUnder(root, path)
	if !ok {
		return SourceRef{}, false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) == 4 &&
		parts[0] == "workspaceStorage" &&
		parts[2] == "chatSessions" &&
		isVSCodeCopilotSessionPath(parts[3]) {
		if promoted := vscodeCopilotPreferredExistingPath(path); promoted != "" {
			path = promoted
		}
		hashDir := filepath.Join(root, "workspaceStorage", parts[1])
		project := ReadVSCodeWorkspaceManifest(hashDir)
		if project == "" {
			project = "unknown"
		}
		return s.newSourceRef(root, path, project), true
	}
	if len(parts) == 3 &&
		parts[0] == "globalStorage" &&
		(parts[1] == "emptyWindowChatSessions" ||
			parts[1] == "transferredChatSessions") &&
		isVSCodeCopilotSessionPath(parts[2]) {
		if promoted := vscodeCopilotPreferredExistingPath(path); promoted != "" {
			path = promoted
		}
		return s.newSourceRef(root, path, "empty-window"), true
	}
	return SourceRef{}, false
}

func (s vscodeCopilotSourceSet) sourcesForWorkspaceManifest(
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
	project := ReadVSCodeWorkspaceManifest(hashDir)
	if project == "" {
		project = "unknown"
	}
	files := discoverVSCodeSessionFiles(
		chatDir, entries, project, AgentVSCodeCopilot,
	)
	sources := make([]SourceRef, 0, len(files))
	seen := make(map[string]struct{}, len(files))
	for _, file := range files {
		source, ok := s.sourceRef(root, file.Path)
		if !ok {
			continue
		}
		source.ProjectHint = file.Project
		addJSONLSource(source, &sources, seen)
	}
	sortJSONLSources(sources)
	return sources
}

func (s vscodeCopilotSourceSet) workspaceManifestForSource(path string) string {
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

func (s vscodeCopilotSourceSet) newSourceRef(root, path, project string) SourceRef {
	return SourceRef{
		Provider:       AgentVSCodeCopilot,
		Key:            path,
		DisplayPath:    path,
		FingerprintKey: path,
		ProjectHint:    project,
		Opaque: vscodeCopilotSource{
			Root:    root,
			Path:    path,
			Project: project,
		},
	}
}

func isVSCodeCopilotSessionPath(name string) bool {
	return strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".jsonl")
}

func vscodeCopilotPreferredExistingPath(path string) string {
	if base, ok := strings.CutSuffix(path, ".json"); ok {
		candidate := base + ".jsonl"
		if IsRegularFile(candidate) {
			return candidate
		}
	}
	if IsRegularFile(path) {
		return path
	}
	if base, ok := strings.CutSuffix(path, ".jsonl"); ok {
		candidate := base + ".json"
		if IsRegularFile(candidate) {
			return candidate
		}
	}
	return ""
}

func vscodeCopilotJSONLPreferredOver(path string) bool {
	base, ok := strings.CutSuffix(path, ".json")
	if !ok {
		return false
	}
	return IsRegularFile(base + ".jsonl")
}

func vscodeCopilotSourceHash(path, workspacePath string) (string, error) {
	hash, err := hashJSONLSourceFile(path)
	if err != nil {
		return "", err
	}
	if workspacePath == "" {
		return hash, nil
	}
	workspaceHash, err := hashJSONLSourceFile(workspacePath)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, _ = h.Write([]byte("chat\x00" + hash + "\x00workspace\x00" + workspaceHash))
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func vscodeCopilotProviderCapabilities() Capabilities {
	return Capabilities{
		Source: SourceCapabilities{
			DiscoverSources:      CapabilitySupported,
			StreamingDiscovery:   CapabilitySupported,
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
