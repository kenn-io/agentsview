package remotesync

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sort"

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
			dbPath + "-shm",
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
