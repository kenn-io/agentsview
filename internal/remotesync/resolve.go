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

func resolveAgentHasOnDiskSource(def parser.AgentDef) bool {
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
				return TargetSet{}, false
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
	if !isAbsRemotePath(file) {
		return "", false
	}
	if _, err := safeRemotePathArchiveName(file); err != nil {
		return "", false
	}
	for agent, dirs := range allowed.Dirs {
		if _, fileScoped := allowed.Files[agent]; fileScoped {
			// File-scoped agents (Windsurf) export a curated, sanitized
			// file list, not a raw directory walk. Accepting a delta
			// request by directory prefix would stream the raw file
			// (e.g. an unsanitized state.vscdb or a secret) that the
			// full-archive writer never exposes. Such agents fall back
			// to the full-archive flow, so a legitimate client never
			// requests these as deltas.
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
