package remotesync

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
)

func ResolveTargets(cfg config.Config) TargetSet {
	dirs := make(map[parser.AgentType][]string)
	files := make(map[parser.AgentType][]string)
	var extra []string
	for _, def := range parser.Registry {
		if !resolveAgentHasOnDiskSource(def) {
			continue
		}
		for _, dir := range cfg.ResolveDirs(def.Type) {
			if def.Type == parser.AgentHermes {
				hermesDirs, hermesFiles := resolveHermesTargets(dir)
				if len(hermesDirs) > 0 {
					dirs[def.Type] = append(dirs[def.Type], hermesDirs...)
				}
				for _, file := range hermesFiles {
					if !slices.Contains(extra, file) {
						extra = append(extra, file)
					}
				}
				continue
			}
			if def.Type == parser.AgentOmnigent {
				root, targetFiles := resolveOmnigentTarget(dir)
				if root != "" && len(targetFiles) > 0 {
					dirs[def.Type] = append(dirs[def.Type], root)
					files[def.Type] = append(files[def.Type], targetFiles...)
				}
				continue
			}
			if def.Type == parser.AgentAider {
				targets := resolveAiderTargets(dir)
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
				root, targetFiles := resolveRooCodeTarget(dir)
				if root != "" && len(targetFiles) > 0 {
					dirs[def.Type] = append(dirs[def.Type], root)
					files[def.Type] = append(files[def.Type], targetFiles...)
				}
				continue
			}
			if def.Type == parser.AgentKiloLegacy {
				root, targetFiles := resolveKiloLegacyTarget(dir)
				if root != "" && len(targetFiles) > 0 {
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
					if !slices.Contains(extra, index) {
						extra = append(extra, index)
					}
				}
			}
		}
	}
	return TargetSet{Dirs: dirs, Files: files, ExtraFiles: extra}
}

func resolveOmnigentTarget(root string) (string, []string) {
	root = filepath.Clean(root)
	chatDB := filepath.Join(root, parser.OmnigentDBName)
	info, err := os.Lstat(chatDB)
	if err != nil || !info.Mode().IsRegular() {
		return "", nil
	}
	files := []string{chatDB}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		path := chatDB + suffix
		info, err := os.Lstat(path)
		if err == nil && info.Mode().IsRegular() {
			files = append(files, path)
		}
	}
	return root, files
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
	if def.Type == parser.AgentTrae {
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

func resolveAiderTargets(root string) []string {
	if isAiderUnsafeRoot(root) {
		return nil
	}
	provider, ok := parser.NewProvider(parser.AgentAider, parser.ProviderConfig{
		Roots: []string{root},
	})
	if !ok {
		return nil
	}
	sources, err := provider.Discover(context.Background())
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(sources))
	for _, source := range sources {
		path := providerDiscoveredPath(source)
		if filepath.Base(path) == parser.AiderHistoryFileName() {
			out = append(out, path)
		}
	}
	return out
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
func resolveRooCodeTarget(root string) (string, []string) {
	targetRoot := filepath.Clean(root)
	if info, err := os.Stat(targetRoot); err != nil || !info.IsDir() {
		return "", nil
	}
	provider, ok := parser.NewProvider(parser.AgentRooCode, parser.ProviderConfig{
		Roots: []string{targetRoot},
	})
	if !ok {
		return "", nil
	}
	sources, err := provider.Discover(context.Background())
	if err != nil {
		return "", nil
	}
	var files []string
	for _, source := range sources {
		historyPath := providerDiscoveredPath(source)
		if historyPath == "" || !regularRemoteSyncFile(historyPath) {
			continue
		}
		files = append(files, historyPath)
		msgPath := filepath.Join(filepath.Dir(historyPath), "ui_messages.json")
		if regularRemoteSyncFile(msgPath) {
			files = append(files, msgPath)
		}
	}
	if len(files) == 0 {
		return "", nil
	}
	return targetRoot, files
}

// resolveKiloLegacyTarget resolves a Kilo Legacy globalStorage root to
// only the per-task session files (task_metadata.json, ui_messages.json,
// api_conversation_history.json). This avoids recursively transferring
// the entire globalStorage directory, which can contain MCP settings,
// API credentials, caches, and other unrelated data.
func resolveKiloLegacyTarget(root string) (string, []string) {
	targetRoot := filepath.Clean(root)
	if info, err := os.Stat(targetRoot); err != nil || !info.IsDir() {
		return "", nil
	}
	provider, ok := parser.NewProvider(parser.AgentKiloLegacy, parser.ProviderConfig{
		Roots: []string{targetRoot},
	})
	if !ok {
		return "", nil
	}
	sources, err := provider.Discover(context.Background())
	if err != nil {
		return "", nil
	}
	var files []string
	for _, source := range sources {
		metadataPath := providerDiscoveredPath(source)
		if metadataPath == "" || !regularRemoteSyncFile(metadataPath) {
			continue
		}
		files = append(files, metadataPath)
		taskDir := filepath.Dir(metadataPath)
		for _, name := range []string{
			"ui_messages.json",
			"api_conversation_history.json",
		} {
			sibPath := filepath.Join(taskDir, name)
			if regularRemoteSyncFile(sibPath) {
				files = append(files, sibPath)
			}
		}
	}
	if len(files) == 0 {
		return "", nil
	}
	return targetRoot, files
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
		Dirs: make(map[parser.AgentType][]string),
	}
	for agent, dirs := range requested.Dirs {
		allowedDirs := allowed.Dirs[agent]
		if _, fileScoped := allowed.Files[agent]; fileScoped {
			requestedFiles, ok := requested.Files[agent]
			if !ok || len(requestedFiles) == 0 {
				return TargetSet{}, false
			}
		}
		for _, dir := range dirs {
			selectedDir, ok := selectAllowedString(allowedDirs, dir)
			if !ok {
				return TargetSet{}, false
			}
			selected.Dirs[agent] = append(selected.Dirs[agent], selectedDir)
		}
	}
	for agent, files := range requested.Files {
		allowedFiles, ok := allowed.Files[agent]
		if !ok {
			return TargetSet{}, false
		}
		for _, file := range files {
			selectedFile, ok := selectAllowedString(allowedFiles, file)
			if !ok {
				// Targets are re-resolved for every request, so a
				// session file deleted between the client's target
				// fetch and this request is no longer in the fresh
				// resolution. Failing the whole request would abort
				// the sync over a routine deletion race; the archive
				// writers tolerate missing files, so authorize
				// session-shaped paths under a still-allowed root.
				if !verbatimSessionFileUnderAllowedRoot(allowed, agent, file) {
					return TargetSet{}, false
				}
				selectedFile = file
			}
			if selected.Files == nil {
				selected.Files = make(map[parser.AgentType][]string)
			}
			selected.Files[agent] = append(selected.Files[agent], selectedFile)
		}
	}
	for _, file := range requested.ExtraFiles {
		selectedFile, ok := selectAllowedString(allowed.ExtraFiles, file)
		if !ok {
			return TargetSet{}, false
		}
		selected.ExtraFiles = append(selected.ExtraFiles, selectedFile)
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

// verbatimSessionFileUnderAllowedRoot authorizes a session-shaped file
// under a verbatim file-scoped agent's still-allowed root when the
// file itself is absent from the fresh per-request resolution — the
// deletion race between a client's target fetch and its next request.
// The strict shape keeps everything else under the root
// (settings/mcp_settings.json, checkpoints, caches) unreachable, and
// the symlink walk rejects components that would escape the root.
func verbatimSessionFileUnderAllowedRoot(
	allowed TargetSet, agent parser.AgentType, file string,
) bool {
	if !verbatimFileScopedAgent(agent) {
		return false
	}
	if !isAbsRemotePath(file) {
		return false
	}
	if _, err := safeRemotePathArchiveName(file); err != nil {
		return false
	}
	for _, dir := range allowed.Dirs[agent] {
		if remotePathDialect(file) != remotePathDialect(dir) {
			continue
		}
		rel, ok := remoteArchiveRel(dir, file)
		if !ok || rel == "" {
			continue
		}
		if !sessionFileShape(agent, rel) {
			continue
		}
		if symlinkEscapesRoot(dir, file) {
			return false
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
	default:
		return rooCodeSessionFileShape(rel)
	}
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
	selected := make([]string, 0, len(files))
	for _, file := range files {
		canonical, ok := selectAllowedFile(allowed, file)
		if !ok {
			return nil, false
		}
		selected = append(selected, canonical)
	}
	return selected, true
}

func selectAllowedFile(allowed TargetSet, file string) (string, bool) {
	if canonical, ok := selectAllowedString(allowed.ExtraFiles, file); ok {
		return canonical, true
	}
	for agent, files := range allowed.Files {
		if !verbatimFileScopedAgent(agent) {
			continue
		}
		// Verbatim file-scoped agents (RooCode) delta-stream exactly
		// their curated files; the exact-match requirement keeps
		// settings and caches under their directory unreachable. A
		// session-shaped file missing from the fresh resolution is
		// still authorized (deletion race); WriteArchiveFiles then
		// skips it because its delta roots come from the same fresh
		// resolution.
		if canonical, ok := selectAllowedString(files, file); ok {
			return canonical, true
		}
		if verbatimSessionFileUnderAllowedRoot(allowed, agent, file) {
			return file, true
		}
	}
	if !isAbsRemotePath(file) {
		return "", false
	}
	if _, err := safeRemotePathArchiveName(file); err != nil {
		return "", false
	}
	for agent, dirs := range allowed.Dirs {
		if _, fileScoped := allowed.Files[agent]; fileScoped {
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
				// non-regular entries.
				return file, true
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
