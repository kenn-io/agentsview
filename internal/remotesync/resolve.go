package remotesync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
)

// ResolveTargets resolves the advertised target set for every
// configured agent. A resolution error (an unreadable root, a failed
// provider discovery) fails the whole resolution rather than silently
// narrowing the target set: an agent that vanishes from the response
// looks identical to an uninstalled agent, so the client would evict
// its entire mirror subtree and re-transfer everything once the error
// clears. Failing closed matches how manifest walks already treat
// read errors under dir-scoped roots.
func ResolveTargets(cfg config.Config) (TargetSet, error) {
	dirs := make(map[parser.AgentType][]string)
	files := make(map[parser.AgentType][]string)
	providerExtraFiles := make(map[parser.AgentType][]string)
	var forbiddenRoots []string
	for _, def := range parser.Registry {
		resolvedDirs := cfg.ResolveDirs(def.Type)
		if def.RemoteSyncExcluded {
			for _, dir := range resolvedDirs {
				forbiddenRoots = appendUniqueForbiddenRoot(forbiddenRoots, dir)
			}
			continue
		}
		if !resolveAgentHasOnDiskSource(def) {
			continue
		}
		for _, dir := range resolvedDirs {
			// Remote imports assign the serving host to every transported root.
			// A structured root attributed to another machine would therefore
			// lose its identity in transit, so keep it as a local archive input
			// instead of advertising it for remote sync.
			if !isLocalRemoteSyncSource(cfg, def.Type, dir) {
				forbiddenRoots = appendUniqueForbiddenRoot(forbiddenRoots, dir)
				continue
			}
			if def.Type == parser.AgentHermes {
				hermesDirs, hermesFiles := resolveHermesTargets(dir)
				if len(hermesDirs) > 0 {
					dirs[def.Type] = append(dirs[def.Type], hermesDirs...)
				}
				for _, file := range hermesFiles {
					if !slices.Contains(providerExtraFiles[def.Type], file) {
						providerExtraFiles[def.Type] = append(providerExtraFiles[def.Type], file)
					}
				}
				continue
			}
			if def.Type == parser.AgentAider {
				targets, err := resolveAiderTargets(dir)
				if err != nil {
					return TargetSet{}, err
				}
				if len(targets) > 0 {
					dirs[def.Type] = append(dirs[def.Type], targets...)
				}
				continue
			}
			if def.Type == parser.AgentWindsurf {
				root, targetFiles := resolveWindsurfTarget(dir)
				if root != "" && len(targetFiles) > 0 {
					dirs[def.Type] = append(dirs[def.Type], root)
					files[def.Type] = append(files[def.Type], targetFiles...)
				}
				continue
			}
			if def.Type == parser.AgentRooCode {
				root, targetFiles, err := resolveRooCodeTarget(dir)
				if err != nil {
					return TargetSet{}, err
				}
				if root != "" && len(targetFiles) > 0 {
					dirs[def.Type] = append(dirs[def.Type], root)
					files[def.Type] = append(files[def.Type], targetFiles...)
				}
				continue
			}
			if def.Type == parser.AgentKiloLegacy {
				root, targetFiles, err := resolveKiloLegacyTarget(dir)
				if err != nil {
					return TargetSet{}, err
				}
				if root != "" && len(targetFiles) > 0 {
					dirs[def.Type] = append(dirs[def.Type], root)
					files[def.Type] = append(files[def.Type], targetFiles...)
				}
				continue
			}
			if def.Type == parser.AgentPoolside {
				target := resolvePoolsideTarget(dir)
				if target != "" {
					dirs[def.Type] = append(dirs[def.Type], target)
				}
				continue
			}
			if emptyFileScopeAgent(def.Type) {
				root, targetFiles, err := resolveEditorTarget(def.Type, dir)
				if err != nil {
					return TargetSet{}, err
				}
				if root != "" {
					dirs[def.Type] = append(dirs[def.Type], root)
					if _, exists := files[def.Type]; !exists {
						files[def.Type] = []string{}
					}
					files[def.Type] = append(files[def.Type], targetFiles...)
				}
				continue
			}
			if def.Type == parser.AgentZed {
				root, targetFiles, err := resolveZedTarget(dir)
				if err != nil {
					return TargetSet{}, err
				}
				if root != "" {
					dirs[def.Type] = append(dirs[def.Type], root)
					files[def.Type] = append(files[def.Type], targetFiles...)
				}
				continue
			}
			if info, err := os.Stat(dir); err != nil || !info.IsDir() {
				continue
			}
			dirs[def.Type] = append(dirs[def.Type], dir)
			if def.Type == parser.AgentCodex {
				index := filepath.Join(filepath.Dir(dir), parser.CodexSessionIndexFilename)
				if info, err := os.Stat(index); err == nil && !info.IsDir() {
					if !slices.Contains(providerExtraFiles[def.Type], index) {
						providerExtraFiles[def.Type] = append(
							providerExtraFiles[def.Type], index,
						)
					}
				}
			}
		}
	}
	return filterForbiddenTargets(TargetSet{
		Dirs: dirs, Files: files, ProviderExtraFiles: providerExtraFiles,
		ForbiddenRoots: forbiddenRoots,
	}), nil
}

// resolveEditorTarget asks the parser for the exact session files it would
// consume, then adds only the workspace manifest needed to preserve project
// attribution. The configured editor root remains the authorization boundary.
func resolveEditorTarget(agent parser.AgentType, root string) (string, []string, error) {
	root = filepath.Clean(root)
	ok, err := curatedRoot(root)
	if err != nil || !ok {
		return "", nil, err
	}
	provider, ok := parser.NewProvider(agent, parser.ProviderConfig{Roots: []string{root}})
	if !ok {
		return "", nil, nil
	}
	sources, err := discoverProviderSources(provider)
	if err != nil {
		return "", nil, fmt.Errorf("discover %s remote sync targets under %q: %w",
			agent, root, err)
	}
	seen := make(map[string]struct{})
	var files []string
	for _, source := range sources {
		path := providerDiscoveredPath(source)
		if path == "" {
			continue
		}
		regular, err := regularCuratedFile(root, path)
		if err != nil {
			return "", nil, err
		}
		if !regular {
			continue
		}
		if _, exists := seen[path]; !exists {
			seen[path] = struct{}{}
			files = append(files, path)
		}
		if agent != parser.AgentVSCodeCopilot {
			continue
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || filepath.IsAbs(rel) {
			continue
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) != 4 || parts[0] != "workspaceStorage" ||
			parts[2] != "chatSessions" {
			continue
		}
		workspace := filepath.Join(root, "workspaceStorage", parts[1], "workspace.json")
		regular, err = regularCuratedFile(root, workspace)
		if err != nil {
			return "", nil, err
		}
		if regular {
			if _, exists := seen[workspace]; !exists {
				seen[workspace] = struct{}{}
				files = append(files, workspace)
			}
		}
	}
	sort.Strings(files)
	return root, files, nil
}

// discoverProviderSources prefers the streaming discovery path the
// sync engine uses. Unlike some batch Discover implementations, which
// convert traversal failures into an authoritative empty enumeration,
// DiscoverEach propagates them, so an unreadable root cannot resolve
// to an empty curated target and evict the client's mirror.
func discoverProviderSources(provider parser.Provider) ([]parser.SourceRef, error) {
	streamer, ok := provider.(parser.StreamingDiscoverer)
	if !ok {
		return provider.Discover(context.Background())
	}
	var sources []parser.SourceRef
	err := streamer.DiscoverEach(context.Background(), func(source parser.SourceRef) error {
		sources = append(sources, source)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return sources, nil
}

func isLocalRemoteSyncSource(
	cfg config.Config, agent parser.AgentType, dir string,
) bool {
	machine, ok := cfg.SourceMachines[agent][dir]
	return !ok || machine == "" || machine == cfg.LocalMachineName
}

// filterForbiddenTargets drops resolved targets that lie inside a forbidden
// root before they are advertised. Overlapping directory overrides can nest
// an allowed agent's root beneath an excluded agent's root; advertising the
// nested target would make every honest client echo it back and fail the
// whole request in SelectAllowedTargets (fail closed, HTTP 403) instead of
// syncing the remaining targets. The per-item forbidden checks in
// SelectAllowedTargets stay as defense-in-depth against stale or
// hand-crafted requests. Registry order does not matter here: the filter
// runs after every excluded agent has contributed its roots.
func filterForbiddenTargets(t TargetSet) TargetSet {
	if len(t.ForbiddenRoots) == 0 {
		return t
	}
	forbidden := newForbiddenRootMatcher(t.ForbiddenRoots)
	fileScopedAgents := make(map[parser.AgentType]struct{}, len(t.Files))
	for agent := range t.Files {
		fileScopedAgents[agent] = struct{}{}
	}
	for agent, dirs := range t.Dirs {
		kept := withoutForbidden(dirs, forbidden)
		if len(kept) == 0 {
			delete(t.Dirs, agent)
			continue
		}
		t.Dirs[agent] = kept
	}
	for agent, files := range t.Files {
		kept := withoutForbidden(files, forbidden)
		if len(kept) == 0 {
			if _, hasDirs := t.Dirs[agent]; hasDirs && emptyFileScopeAgent(agent) {
				t.Files[agent] = []string{}
			} else {
				delete(t.Files, agent)
			}
			continue
		}
		t.Files[agent] = kept
	}
	// A file-scoped agent's root is only safe to advertise alongside its
	// curated file list; if filtering removed either half, drop both so the
	// agent cannot degrade to a raw directory target.
	for agent := range fileScopedAgents {
		_, hasFiles := t.Files[agent]
		if !hasFiles || !t.isFileScoped(agent) {
			delete(t.Dirs, agent)
			delete(t.Files, agent)
			continue
		}
		if _, hasDirs := t.Dirs[agent]; !hasDirs {
			delete(t.Dirs, agent)
			delete(t.Files, agent)
		}
	}
	t.ExtraFiles = withoutForbidden(t.ExtraFiles, forbidden)
	for agent, files := range t.ProviderExtraFiles {
		kept := withoutForbidden(files, forbidden)
		if len(kept) == 0 {
			delete(t.ProviderExtraFiles, agent)
			continue
		}
		t.ProviderExtraFiles[agent] = kept
	}
	return t
}

func withoutForbidden(paths []string, forbidden forbiddenRootMatcher) []string {
	var kept []string
	for _, path := range paths {
		if !forbidden.within(path) {
			kept = append(kept, path)
		}
	}
	return kept
}

func appendUniqueForbiddenRoot(roots []string, root string) []string {
	if root == "" {
		return roots
	}
	root = filepath.Clean(root)
	if slices.Contains(roots, root) {
		return roots
	}
	return append(roots, root)
}

func resolveHermesTargets(root string) ([]string, []string) {
	root = filepath.Clean(root)
	if filepath.Base(root) != "profiles" ||
		filepath.Base(filepath.Dir(root)) != ".hermes" {
		return resolveHermesArchiveTarget(root, !isHermesNamedProfileRoot(root))
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, nil
	}
	var dirs []string
	var files []string
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		profileDirs, profileFiles := resolveHermesArchiveTarget(
			filepath.Join(root, entry.Name()), false,
		)
		dirs = append(dirs, profileDirs...)
		files = append(files, profileFiles...)
	}
	return dirs, files
}

func isHermesNamedProfileRoot(root string) bool {
	parent := filepath.Dir(filepath.Clean(root))
	return filepath.Base(parent) == "profiles" &&
		filepath.Base(filepath.Dir(parent)) == ".hermes"
}

func resolveHermesArchiveTarget(root string, allowFlat bool) ([]string, []string) {
	root = filepath.Clean(root)
	sessionsDir := filepath.Join(root, "sessions")
	stateDB := filepath.Join(root, "state.db")
	if allowFlat {
		switch filepath.Base(root) {
		case "sessions":
			sessionsDir = root
			stateDB = filepath.Join(filepath.Dir(root), "state.db")
		case "state.db":
			sessionsDir = filepath.Join(filepath.Dir(root), "sessions")
			stateDB = root
		}
	}

	if info, err := os.Stat(sessionsDir); err == nil && info.IsDir() {
		return []string{sessionsDir}, hermesStateFiles(stateDB, true)
	}
	if regularRemoteSyncFile(stateDB) {
		return []string{stateDB}, hermesStateFiles(stateDB, false)
	}
	if allowFlat && hasHermesTranscriptFile(root) {
		return []string{root}, nil
	}
	return nil, nil
}

func hasHermesTranscriptFile(root string) bool {
	entries, err := os.ReadDir(root)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".jsonl") &&
			(!strings.HasPrefix(name, "session_") || !strings.HasSuffix(name, ".json")) {
			continue
		}
		if regularRemoteSyncFile(filepath.Join(root, name)) {
			return true
		}
	}
	return false
}

// hermesStateFiles returns stable, narrowly scoped allowlist paths. SQLite
// companions are transient, so their presence must not change the target set
// between the targets, manifest, and archive requests. Archive and manifest
// writers treat absent entries as optional.
func hermesStateFiles(stateDB string, includeDB bool) []string {
	files := []string{stateDB + "-wal", stateDB + "-shm", stateDB + "-journal"}
	if includeDB {
		files = append([]string{stateDB}, files...)
	}
	return files
}

func resolveAgentHasOnDiskSource(def parser.AgentDef) bool {
	if def.RemoteSyncExcluded {
		return false
	}
	if !def.FileBased {
		return false
	}
	switch parser.ProviderMigrationModes()[def.Type] {
	case parser.ProviderMigrationProviderAuthoritative:
		_, ok := parser.ProviderFactoryByType(def.Type)
		return ok
	default:
		return false
	}
}

func resolveAiderTargets(root string) ([]string, error) {
	if isAiderUnsafeRoot(root) {
		return nil, nil
	}
	provider, ok := parser.NewProvider(parser.AgentAider, parser.ProviderConfig{
		Roots: []string{root},
	})
	if !ok {
		return nil, nil
	}
	// Aider stays on batch Discover: its streaming enumeration yields
	// different source references than the batch path this resolver's
	// history-file filter matches.
	sources, err := provider.Discover(context.Background())
	if err != nil {
		return nil, fmt.Errorf("discover aider remote sync targets under %q: %w",
			root, err)
	}
	out := make([]string, 0, len(sources))
	for _, source := range sources {
		path := providerDiscoveredPath(source)
		if filepath.Base(path) == parser.AiderHistoryFileName() {
			out = append(out, path)
		}
	}
	return out, nil
}

func resolveWindsurfTarget(root string) (string, []string) {
	targetRoot := filepath.Clean(root)
	workspaceRoot := windsurfRemoteWorkspaceRoot(targetRoot)
	if info, err := os.Stat(workspaceRoot); err != nil || !info.IsDir() {
		return "", nil
	}
	files := resolveWindsurfFiles(workspaceRoot)
	if len(files) == 0 {
		return "", nil
	}
	return targetRoot, files
}

// resolveRooCodeTarget curates a file-scoped target for a RooCode
// root. The configured directory is VSCode's whole
// globalStorage/rooveterinaryinc.roo-cline tree, which also holds
// settings/mcp_settings.json (MCP env vars, API keys, auth headers),
// caches, and checkpoints — none of which may be archived. Only the
// discovered tasks/<id>/history_item.json files and their
// ui_messages.json siblings are exported.
func resolveRooCodeTarget(root string) (string, []string, error) {
	targetRoot := filepath.Clean(root)
	ok, err := statCuratedDir(targetRoot)
	if err != nil || !ok {
		return "", nil, err
	}
	provider, ok := parser.NewProvider(parser.AgentRooCode, parser.ProviderConfig{
		Roots: []string{targetRoot},
	})
	if !ok {
		return "", nil, nil
	}
	sources, err := discoverProviderSources(provider)
	if err != nil {
		return "", nil, fmt.Errorf("discover roocode remote sync targets under %q: %w",
			targetRoot, err)
	}
	var files []string
	for _, source := range sources {
		historyPath := providerDiscoveredPath(source)
		if historyPath == "" {
			continue
		}
		regular, err := statRegularRemoteSyncFile(historyPath)
		if err != nil {
			return "", nil, err
		}
		if !regular {
			continue
		}
		files = append(files, historyPath)
		msgPath := filepath.Join(filepath.Dir(historyPath), "ui_messages.json")
		regular, err = statRegularRemoteSyncFile(msgPath)
		if err != nil {
			return "", nil, err
		}
		if regular {
			files = append(files, msgPath)
		}
	}
	if len(files) == 0 {
		return "", nil, nil
	}
	return targetRoot, files, nil
}

// resolveKiloLegacyTarget resolves a Kilo Legacy globalStorage root to
// only the per-task session files (task_metadata.json, ui_messages.json,
// api_conversation_history.json). This avoids recursively transferring
// the entire globalStorage directory, which can contain MCP settings,
// API credentials, caches, and other unrelated data.
func resolveKiloLegacyTarget(root string) (string, []string, error) {
	targetRoot := filepath.Clean(root)
	ok, err := statCuratedDir(targetRoot)
	if err != nil || !ok {
		return "", nil, err
	}
	provider, ok := parser.NewProvider(parser.AgentKiloLegacy, parser.ProviderConfig{
		Roots: []string{targetRoot},
	})
	if !ok {
		return "", nil, nil
	}
	sources, err := discoverProviderSources(provider)
	if err != nil {
		return "", nil, fmt.Errorf("discover kilo legacy remote sync targets under %q: %w",
			targetRoot, err)
	}
	var files []string
	for _, source := range sources {
		metadataPath := providerDiscoveredPath(source)
		if metadataPath == "" {
			continue
		}
		regular, err := statRegularRemoteSyncFile(metadataPath)
		if err != nil {
			return "", nil, err
		}
		if !regular {
			continue
		}
		files = append(files, metadataPath)
		taskDir := filepath.Dir(metadataPath)
		for _, name := range []string{
			"ui_messages.json",
			"api_conversation_history.json",
		} {
			sibPath := filepath.Join(taskDir, name)
			regular, err := statRegularRemoteSyncFile(sibPath)
			if err != nil {
				return "", nil, err
			}
			if regular {
				files = append(files, sibPath)
			}
		}
	}
	if len(files) == 0 {
		return "", nil, nil
	}
	return targetRoot, files, nil
}

// curatedRoot reports whether root is a plain directory that may
// anchor a curated target. A missing or non-directory root is a
// legitimately absent target; any other stat failure propagates so an
// unreadable root cannot masquerade as an uninstalled agent.
func curatedRoot(root string) (bool, error) {
	info, err := os.Lstat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat remote sync root %q: %w", root, err)
	}
	return info.IsDir() && info.Mode()&os.ModeSymlink == 0, nil
}

// statCuratedDir is curatedRoot for resolvers that historically follow
// symlinked roots (RooCode, Kilo Legacy): same error contract, but the
// symlink target's type decides.
func statCuratedDir(root string) (bool, error) {
	info, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat remote sync root %q: %w", root, err)
	}
	return info.IsDir(), nil
}

func regularCuratedFile(root, path string) (bool, error) {
	path = filepath.Clean(path)
	rel, err := filepath.Rel(root, path)
	if err != nil || !filepath.IsLocal(rel) || symlinkEscapesRoot(root, path) {
		return false, nil
	}
	return statRegularRemoteSyncFile(path)
}

// statRegularRemoteSyncFile applies the curated-resolver error
// contract to one selected file: a missing file is an omitted target,
// while any other stat failure propagates so it cannot silently
// shrink the curated set and evict the client's mirror copies.
func statRegularRemoteSyncFile(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat remote sync target %q: %w", path, err)
	}
	return info.Mode().IsRegular(), nil
}

func resolveZedTarget(root string) (string, []string, error) {
	root = filepath.Clean(root)
	ok, err := curatedRoot(root)
	if err != nil || !ok {
		return "", nil, err
	}
	dbPath := filepath.Join(root, parser.ZedThreadsDBRelPath)
	ok, err = curatedFileOrMissing(root, dbPath)
	if err != nil || !ok {
		return "", nil, err
	}
	return root, []string{dbPath}, nil
}

// curatedFileOrMissing retains a selected leaf through a deletion race so the
// manifest can omit it and the mirror can evict its stale copy. A stat
// failure other than absence propagates like curatedRoot's.
func curatedFileOrMissing(root, path string) (bool, error) {
	path = filepath.Clean(path)
	rel, err := filepath.Rel(root, path)
	if err != nil || !filepath.IsLocal(rel) || symlinkEscapesRoot(root, path) {
		return false, nil
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat remote sync target %q: %w", path, err)
	}
	return info.Mode().IsRegular(), nil
}

// resolvePoolsideTarget narrows a Poolside application-data root to
// the trajectories/ subdirectory. The configured root is the entire
// poolside data directory, which may contain config, caches, or
// credentials alongside trajectories. Only the trajectories/ subdirectory
// is parsed, so only it must be archived during remote sync. When the
// root already points to a trajectories/ directory, it is used as-is.
func resolvePoolsideTarget(root string) string {
	clean := filepath.Clean(root)
	if filepath.Base(clean) == "trajectories" {
		info, err := os.Stat(clean)
		if err != nil || !info.IsDir() {
			return ""
		}
		return clean
	}
	trajectoriesDir := filepath.Join(clean, "trajectories")
	info, err := os.Stat(trajectoriesDir)
	if err != nil || !info.IsDir() {
		return ""
	}
	return trajectoriesDir
}

func windsurfRemoteWorkspaceRoot(root string) string {
	clean := filepath.Clean(root)
	if filepath.Base(clean) == "workspaceStorage" {
		return clean
	}
	return filepath.Join(clean, "workspaceStorage")
}

func resolveWindsurfFiles(workspaceRoot string) []string {
	entries, err := os.ReadDir(workspaceRoot)
	if err != nil {
		return nil
	}
	var files []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		workspaceDir := filepath.Join(workspaceRoot, entry.Name())
		dbPath := filepath.Join(workspaceDir, parser.WindsurfStateDBName)
		if !regularRemoteSyncFile(dbPath) {
			continue
		}
		files = append(files, dbPath)
		for _, path := range []string{
			dbPath + "-wal",
			filepath.Join(workspaceDir, "workspace.json"),
		} {
			if regularRemoteSyncFile(path) {
				files = append(files, path)
			}
		}
	}
	sort.Strings(files)
	return files
}

func regularRemoteSyncFile(path string) bool {
	info, err := os.Lstat(path)
	if err != nil {
		return false
	}
	return info.Mode().IsRegular()
}

func providerDiscoveredPath(source parser.SourceRef) string {
	for _, path := range []string{
		source.DisplayPath,
		source.FingerprintKey,
		source.Key,
	} {
		if path != "" {
			return path
		}
	}
	return ""
}

func TargetSetAllowed(allowed TargetSet, requested TargetSet) bool {
	_, ok := SelectAllowedTargets(allowed, requested)
	return ok
}

func SelectAllowedTargets(allowed TargetSet, requested TargetSet) (TargetSet, bool) {
	selected := TargetSet{
		Dirs:           make(map[parser.AgentType][]string),
		ForbiddenRoots: append([]string(nil), allowed.ForbiddenRoots...),
	}
	forbidden := newForbiddenRootMatcher(selected.ForbiddenRoots)
	for agent, dirs := range requested.Dirs {
		allowedDirs := allowed.Dirs[agent]
		fileScoped := allowed.isFileScoped(agent)
		if fileScoped {
			// A request that names the directory without any curated
			// files would select an empty manifest and evict the
			// client's mirror even while sessions exist. Accept it
			// only when the fresh curated scope is itself empty.
			if requestedFiles, ok := requested.Files[agent]; !ok || len(requestedFiles) == 0 {
				emptyScope := emptyFileScopeAgent(agent) &&
					len(allowed.Files[agent]) == 0
				if !emptyScope {
					return TargetSet{}, false
				}
			}
		}
		for _, dir := range dirs {
			selectedDir, ok := selectAllowedString(allowedDirs, dir)
			if !ok {
				return TargetSet{}, false
			}
			if forbidden.within(selectedDir) {
				return TargetSet{}, false
			}
			selected.Dirs[agent] = append(selected.Dirs[agent], selectedDir)
		}
		if fileScoped {
			if selected.Files == nil {
				selected.Files = make(map[parser.AgentType][]string)
			}
			if _, ok := selected.Files[agent]; !ok {
				selected.Files[agent] = []string{}
			}
		}
	}
	for agent, files := range requested.Files {
		allowedFiles := allowed.Files[agent]
		for _, file := range files {
			selectedFile, ok := selectAllowedString(allowedFiles, file)
			if !ok {
				if !authorizedStaleCuratedFile(allowed, forbidden, agent, file, files) {
					return TargetSet{}, false
				}
				// A stale curated reference is authorized but omitted, so
				// the manifest built from the selection agrees the file is
				// gone and the client mirror evicts its copy.
				continue
			}
			if selected.Files == nil {
				selected.Files = make(map[parser.AgentType][]string)
			}
			if forbidden.within(selectedFile) {
				return TargetSet{}, false
			}
			selected.Files[agent] = append(selected.Files[agent], selectedFile)
		}
	}
	for _, file := range requested.ExtraFiles {
		selectedFile, ok := selectAllowedString(allowed.ExtraFiles, file)
		if !ok {
			return TargetSet{}, false
		}
		if forbidden.within(selectedFile) {
			return TargetSet{}, false
		}
		selected.ExtraFiles = append(selected.ExtraFiles, selectedFile)
	}
	for agent, files := range requested.ProviderExtraFiles {
		allowedFiles, ok := allowed.ProviderExtraFiles[agent]
		if !ok {
			return TargetSet{}, false
		}
		for _, file := range files {
			selectedFile, ok := selectAllowedString(allowedFiles, file)
			if !ok || forbidden.within(selectedFile) {
				return TargetSet{}, false
			}
			if selected.ProviderExtraFiles == nil {
				selected.ProviderExtraFiles = make(map[parser.AgentType][]string)
			}
			selected.ProviderExtraFiles[agent] = append(
				selected.ProviderExtraFiles[agent], selectedFile,
			)
		}
	}
	return selected, true
}

func selectAllowedString(allowed []string, requested string) (string, bool) {
	for _, value := range allowed {
		if value == requested {
			return value, true
		}
	}
	return "", false
}

// rooCodeSessionFileShape reports whether rel — a slash-separated path
// relative to a RooCode root — names exactly a session file the
// provider would discover: tasks/<taskID>/history_item.json or
// tasks/<taskID>/ui_messages.json. Task IDs starting with "_" or "."
// are rejected, matching discovery's marker-directory skip.
func rooCodeSessionFileShape(rel string) bool {
	parts := strings.Split(rel, "/")
	if len(parts) != 3 || parts[0] != "tasks" {
		return false
	}
	taskID := parts[1]
	if taskID == "" || strings.HasPrefix(taskID, "_") ||
		strings.HasPrefix(taskID, ".") {
		return false
	}
	return parts[2] == "history_item.json" || parts[2] == "ui_messages.json"
}

// kiloLegacySessionFileShape reports whether rel — a slash-separated
// path relative to a Kilo Legacy root — names exactly a session file
// the provider would discover: tasks/<taskID>/task_metadata.json,
// tasks/<taskID>/ui_messages.json, or
// tasks/<taskID>/api_conversation_history.json. Task IDs starting
// with "_" or "." are rejected, matching discovery's marker-directory
// skip.
func kiloLegacySessionFileShape(rel string) bool {
	parts := strings.Split(rel, "/")
	if len(parts) != 3 || parts[0] != "tasks" {
		return false
	}
	taskID := parts[1]
	if taskID == "" || strings.HasPrefix(taskID, "_") ||
		strings.HasPrefix(taskID, ".") {
		return false
	}
	switch parts[2] {
	case "task_metadata.json", "ui_messages.json", "api_conversation_history.json":
		return true
	}
	return false
}

// authorizedStaleCuratedFile reports whether a curated file request
// that missed the fresh per-request resolution is still authorized
// under a verbatim or snapshot file-scoped agent's allowed root — the
// deletion race between a client's target fetch and its next request,
// or a discovery preference flip (Cursor and VS Code prefer one
// transcript extension over its sibling). An authorized stale file is
// always omitted from the selection, never streamed: the strict shape
// keeps everything else under the root (settings/mcp_settings.json,
// checkpoints, caches) unreachable, and the symlink walk rejects
// components that would escape the root.
func authorizedStaleCuratedFile(
	allowed TargetSet, forbidden forbiddenRootMatcher, agent parser.AgentType,
	file string, requestedFiles []string,
) bool {
	if !verbatimFileScopedAgent(agent) && !snapshotFileScopedAgent(agent) {
		return false
	}
	if !isAbsRemotePath(file) {
		return false
	}
	if _, err := safeRemotePathArchiveName(file); err != nil {
		return false
	}
	for _, dir := range allowed.Dirs[agent] {
		if forbidden.within(dir) {
			continue
		}
		if remotePathDialect(file) != remotePathDialect(dir) {
			continue
		}
		rel, ok := remoteArchiveRel(dir, file)
		if !ok || rel == "" {
			continue
		}
		if agent == parser.AgentVSCodeCopilot && isVSCodeWorkspaceMetadata(rel) {
			if !vscodeWorkspaceMetadataTiedToAllowedSession(
				allowed, dir, file, requestedFiles,
			) {
				continue
			}
		} else if !sessionFileShape(agent, rel) {
			continue
		}
		if symlinkEscapesRoot(dir, file) {
			return false
		}
		if forbidden.within(file) {
			return false
		}
		info, err := os.Lstat(file)
		if os.IsNotExist(err) {
			return true
		}
		if err != nil || !info.Mode().IsRegular() {
			return false
		}
		if agent == parser.AgentVSCodeCopilot && isVSCodeWorkspaceMetadata(rel) &&
			vscodeWorkspaceChatVanished(allowed, forbidden, dir, rel, requestedFiles) {
			return true
		}
		return hasPreferredCuratedSibling(dir, allowed.Files[agent], rel)
	}
	return false
}

func hasPreferredCuratedSibling(root string, allowedFiles []string, rel string) bool {
	separator := strings.LastIndexByte(rel, '/')
	dir, name := rel[:separator+1], rel[separator+1:]
	stem := strings.TrimSuffix(name, filepath.Ext(name))
	for _, file := range allowedFiles {
		candidate, ok := remoteArchiveRel(root, file)
		if !ok {
			continue
		}
		candidateSeparator := strings.LastIndexByte(candidate, '/')
		candidateDir := candidate[:candidateSeparator+1]
		candidateName := candidate[candidateSeparator+1:]
		if candidateDir == dir &&
			strings.TrimSuffix(candidateName, filepath.Ext(candidateName)) == stem &&
			filepath.Ext(candidateName) != filepath.Ext(name) {
			return true
		}
	}
	return false
}

func vscodeWorkspaceChatVanished(
	allowed TargetSet, forbidden forbiddenRootMatcher, root, rel string,
	requestedFiles []string,
) bool {
	parts := strings.Split(rel, "/")
	for _, file := range requestedFiles {
		chatRel, ok := remoteArchiveRel(root, file)
		if !ok {
			continue
		}
		chatParts := strings.Split(chatRel, "/")
		if len(chatParts) != 4 || chatParts[0] != "workspaceStorage" ||
			chatParts[1] != parts[1] || chatParts[2] != "chatSessions" ||
			!vscodeChatSessionFileShape(chatParts[3]) {
			continue
		}
		if !isAbsRemotePath(file) || remotePathDialect(file) != remotePathDialect(root) {
			continue
		}
		if _, err := safeRemotePathArchiveName(file); err != nil ||
			symlinkEscapesRoot(root, file) || forbidden.within(file) {
			continue
		}
		if _, err := os.Lstat(file); !os.IsNotExist(err) {
			continue
		}
		for _, allowedFile := range allowed.Files[parser.AgentVSCodeCopilot] {
			allowedRel, ok := remoteArchiveRel(root, allowedFile)
			if ok && strings.HasPrefix(allowedRel, "workspaceStorage/"+parts[1]+"/chatSessions/") {
				return false
			}
		}
		return true
	}
	return false
}

// sessionFileShape reports whether rel names exactly a session file
// for the given agent type.
func sessionFileShape(agent parser.AgentType, rel string) bool {
	switch agent {
	case parser.AgentKiloLegacy:
		return kiloLegacySessionFileShape(rel)
	case parser.AgentCursor:
		_, ok := parser.ParseCursorTranscriptRelPath(rel)
		return ok
	case parser.AgentVSCodeCopilot:
		parts := strings.Split(rel, "/")
		if len(parts) == 4 && parts[0] == "workspaceStorage" &&
			parts[2] == "chatSessions" {
			return parser.IsValidSessionID(strings.TrimSuffix(parts[3], filepath.Ext(parts[3]))) &&
				(filepath.Ext(parts[3]) == ".json" || filepath.Ext(parts[3]) == ".jsonl")
		}
		if len(parts) == 3 && parts[0] == "globalStorage" &&
			(parts[1] == "emptyWindowChatSessions" || parts[1] == "transferredChatSessions") {
			return parser.IsValidSessionID(strings.TrimSuffix(parts[2], filepath.Ext(parts[2]))) &&
				(filepath.Ext(parts[2]) == ".json" || filepath.Ext(parts[2]) == ".jsonl")
		}
		return false
	case parser.AgentZed:
		return filepath.ToSlash(filepath.Clean(rel)) == parser.ZedThreadsDBRelPath
	default:
		return rooCodeSessionFileShape(rel)
	}
}

func vscodeWorkspaceMetadataTiedToAllowedSession(
	allowed TargetSet, root, file string, requestedFiles []string,
) bool {
	rel, ok := remoteArchiveRel(root, file)
	if !ok {
		return false
	}
	parts := strings.Split(rel, "/")
	if !isVSCodeWorkspaceMetadata(rel) {
		return false
	}
	selectedFiles := slices.Concat(
		allowed.Files[parser.AgentVSCodeCopilot], requestedFiles,
	)
	for _, selected := range selectedFiles {
		selectedRel, ok := remoteArchiveRel(root, selected)
		if !ok {
			continue
		}
		selectedParts := strings.Split(selectedRel, "/")
		if len(selectedParts) != 4 || selectedParts[0] != "workspaceStorage" ||
			selectedParts[1] != parts[1] || selectedParts[2] != "chatSessions" {
			continue
		}
		if vscodeChatSessionFileShape(selectedParts[3]) {
			return true
		}
	}
	return false
}

func isVSCodeWorkspaceMetadata(rel string) bool {
	parts := strings.Split(rel, "/")
	return len(parts) == 3 && parts[0] == "workspaceStorage" &&
		parts[1] != "" && parts[2] == "workspace.json"
}

func vscodeChatSessionFileShape(name string) bool {
	ext := filepath.Ext(name)
	return (ext == ".json" || ext == ".jsonl") &&
		parser.IsValidSessionID(strings.TrimSuffix(name, ext))
}

func isAiderUnsafeRoot(dir string) bool {
	if dir == "" {
		return true
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return false
	}
	return filepath.Clean(dir) == filepath.Clean(home)
}

// SelectAllowedFiles validates a delta-archive file list: every entry
// must be under an allowed dir, exactly an allowed root (some agents,
// like Aider, resolve individual history files into Dirs), or exactly
// an allowed extra file. Only absolute request paths can match an
// allowed root; the absolute check is remote-OS neutral because
// request paths echo the server's own manifest, not local-OS paths.
// Any disallowed entry rejects the whole request (fail closed, like
// SelectAllowedTargets). Path traversal is rejected by
// safeRemotePathArchiveName before any prefix comparison; prefix
// comparisons additionally require matching path dialects and reject
// symlinked ancestors that would escape the allowed root.
func SelectAllowedFiles(allowed TargetSet, files []string) ([]string, bool) {
	forbidden := newForbiddenRootMatcher(allowed.ForbiddenRoots)
	selected := make([]string, 0, len(files))
	for _, file := range files {
		canonical, ok := selectAllowedFile(allowed, forbidden, file, files)
		if !ok {
			return nil, false
		}
		if canonical != "" {
			selected = append(selected, canonical)
		}
	}
	return selected, true
}

// selectAllowedFile validates the request string against the allowed sets
// first and checks forbidden roots only on a match. The forbidden check
// canonicalizes its argument with filesystem access, which must never run
// on an unmatched client-supplied path: on Windows a raw request naming
// \\attacker\share would otherwise force an outbound SMB connection.
// Every accept path below is either a server-derived string or anchored
// under a trusted allowed root before the matcher sees it. An empty
// canonical result with ok=true means the request is authorized but the
// file is omitted from the delta (a stale curated reference).
func selectAllowedFile(
	allowed TargetSet, forbidden forbiddenRootMatcher, file string,
	requestedFiles []string,
) (string, bool) {
	if canonical, ok := selectAllowedString(allowed.AllExtraFiles(), file); ok {
		return canonical, !forbidden.within(canonical)
	}
	for agent, files := range allowed.Files {
		if !verbatimFileScopedAgent(agent) && !snapshotFileScopedAgent(agent) {
			continue
		}
		// Verbatim and snapshot file-scoped agents delta-stream exactly
		// their curated files; the exact-match requirement keeps
		// settings and caches under their directory unreachable. A
		// session-shaped file missing from the fresh resolution is
		// still authorized (deletion race) but omitted, so the delta
		// archive and manifest agree that the file no longer exists.
		if canonical, ok := selectAllowedString(files, file); ok {
			return canonical, !forbidden.within(canonical)
		}
		if authorizedStaleCuratedFile(allowed, forbidden, agent, file, requestedFiles) {
			return "", true
		}
	}
	if !isAbsRemotePath(file) {
		return "", false
	}
	if _, err := safeRemotePathArchiveName(file); err != nil {
		return "", false
	}
	for agent, dirs := range allowed.Dirs {
		if allowed.isFileScoped(agent) ||
			verbatimFileScopedAgent(agent) || snapshotFileScopedAgent(agent) {
			// File-scoped agents export a curated file list, not a raw
			// directory walk. Accepting a delta request by directory
			// prefix would stream a raw file (an unsanitized
			// state.vscdb, an mcp_settings.json secret) that the
			// archive writer never exposes. Verbatim agents already
			// matched by exact file above; sanitized agents (Windsurf)
			// fall back to the full-archive flow, so a legitimate
			// client never requests these as deltas.
			continue
		}
		for _, dir := range dirs {
			if remotePathDialect(file) != remotePathDialect(dir) {
				// Archive-name remapping flattens dialects into one
				// namespace (`C:\x` and `/__drive_C/x` both remap to
				// `__drive_C/x`), so a cross-dialect prefix match would
				// validate a request the archive writer then reads at a
				// literal path outside the allowed root.
				continue
			}
			if _, ok := remoteArchiveRel(dir, file); ok {
				if symlinkEscapesRoot(dir, file) {
					return "", false
				}
				// Exact root matches are allowed: file roots (Aider
				// history files) must stream, and a directory root
				// yields nothing because WriteArchiveFiles skips
				// non-regular entries. The file is anchored under the
				// trusted dir at this point, so the forbidden check may
				// canonicalize it.
				return file, !forbidden.within(file)
			}
		}
	}
	return "", false
}

type pathDialect int

const (
	dialectPOSIX pathDialect = iota
	dialectDrive
	dialectUNC
)

// remotePathDialect classifies an absolute remote path as POSIX,
// Windows drive-letter, or UNC. Delta validation requires the
// requested file and the allowed root to share a dialect before any
// archive-name prefix comparison.
func remotePathDialect(p string) pathDialect {
	if strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, "//") {
		return dialectUNC
	}
	if len(p) >= 2 && p[1] == ':' {
		return dialectDrive
	}
	return dialectPOSIX
}

// symlinkEscapesRoot reports whether the allowed root or any path
// component between it and the requested file's parent is a symlink.
// BuildManifest and the full-archive walk never traverse symlinks, so
// delta validation must not either: a symlinked component would let a
// delta request stream entries no manifest ever lists, and with a
// symlinked root that includes files outside the lexical allowed
// directory. Missing components are not escapes: a vanished file is
// skipped by WriteArchiveFiles, and a missing root has nothing under
// it to stream.
func symlinkEscapesRoot(root, file string) bool {
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return false
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 {
		return true
	}
	rel, err := filepath.Rel(root, filepath.Dir(file))
	if err != nil || rel == "." || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		// Exact root matches (Aider file roots, where Dir(file) is the
		// root's parent) and files directly under the root have no
		// intermediate components to check; the root's own ancestors
		// are operator-configured territory. A component merely named
		// with a ".." prefix (e.g. "..alias") is NOT a parent escape
		// and falls through to the symlink walk below.
		return false
	}
	dir := root
	for part := range strings.SplitSeq(filepath.ToSlash(rel), "/") {
		if part == "" || part == "." {
			continue
		}
		dir = filepath.Join(dir, part)
		info, err := os.Lstat(dir)
		if err != nil {
			return false
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return true
		}
	}
	return false
}

// isAbsRemotePath reports whether a requested path is absolute in any
// remote-OS form: POSIX rooted, UNC, or Windows drive-letter. Host
// filepath.IsAbs semantics would wrongly reject POSIX paths on
// Windows and drive paths on Unix, and requests are validated against
// the server's own resolved targets regardless of the local OS.
func isAbsRemotePath(p string) bool {
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\\`) {
		return true
	}
	return len(p) >= 3 && p[1] == ':' && (p[2] == '/' || p[2] == '\\')
}
