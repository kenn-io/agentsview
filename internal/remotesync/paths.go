package remotesync

import (
	"fmt"
	pathpkg "path"
	"path/filepath"
	"slices"
	"strings"
)

func safeRemotePathArchiveName(remotePath string) (string, error) {
	p := strings.ReplaceAll(remotePath, `\`, "/")
	if hasDotDotPathComponent(p) {
		return "", fmt.Errorf("unsafe remote path %q: contains '..'", remotePath)
	}
	if p == "." || p == "" {
		return "", nil
	}
	if strings.HasPrefix(p, "//") {
		rest := pathpkg.Clean(strings.TrimLeft(p, "/"))
		if rest == "." || rest == "" {
			return "", fmt.Errorf("unsafe remote path %q: empty UNC path", remotePath)
		}
		return pathpkg.Join("__unc", rest), nil
	}
	if len(p) >= 2 && p[1] == ':' {
		drive := "__drive_" + p[:1]
		rest := strings.TrimLeft(p[2:], "/")
		if rest == "" {
			return drive, nil
		}
		return pathpkg.Join(drive, pathpkg.Clean(rest)), nil
	}
	p = pathpkg.Clean(p)
	if pathpkg.IsAbs(p) {
		p = strings.TrimLeft(p, "/")
	}
	p = strings.TrimLeft(p, "/")
	if p == "" || p == "." {
		return "", nil
	}
	return p, nil
}

func hasDotDotPathComponent(p string) bool {
	return slices.Contains(strings.Split(p, "/"), "..")
}

func remappedRemotePath(tempDir, remotePath string) string {
	local, err := safeRemappedRemotePath(tempDir, remotePath)
	if err != nil {
		return tempDir
	}
	return local
}

func safeRemappedRemotePath(tempDir, remotePath string) (string, error) {
	name, err := safeRemotePathArchiveName(remotePath)
	if err != nil {
		return "", err
	}
	return safeLocalArchivePath(tempDir, name)
}

func safeLocalArchivePath(tempDir, archiveName string) (string, error) {
	raw := strings.ReplaceAll(archiveName, `\`, "/")
	if pathpkg.IsAbs(raw) || hasDotDotPathComponent(raw) {
		return "", fmt.Errorf("unsafe archive path %q", archiveName)
	}
	clean := pathpkg.Clean(raw)
	if clean == "." || clean == "" {
		return tempDir, nil
	}
	parts := strings.Split(clean, "/")
	elems := make([]string, 0, len(parts)+1)
	elems = append(elems, tempDir)
	elems = append(elems, parts...)
	local := filepath.Join(elems...)
	if !within(tempDir, local) {
		return "", fmt.Errorf("unsafe archive path %q escapes extraction dir", archiveName)
	}
	return local, nil
}

func remoteArchiveRel(remoteDir, remotePath string) (string, bool) {
	base, err := safeRemotePathArchiveName(remoteDir)
	if err != nil {
		return "", false
	}
	name, err := safeRemotePathArchiveName(remotePath)
	if err != nil {
		return "", false
	}
	if base == "" {
		return name, true
	}
	if name == base {
		return "", true
	}
	prefix := base + "/"
	if after, ok := strings.CutPrefix(name, prefix); ok {
		return after, true
	}
	return "", false
}

func validateTargetSetPaths(targets TargetSet) error {
	for agent, dirs := range targets.Dirs {
		for _, dir := range dirs {
			if _, err := safeRemotePathArchiveName(dir); err != nil {
				return fmt.Errorf("target dir %s %q: %w", agent, dir, err)
			}
		}
	}
	for _, file := range targets.ExtraFiles {
		if _, err := safeRemotePathArchiveName(file); err != nil {
			return fmt.Errorf("target file %q: %w", file, err)
		}
	}
	return nil
}

func tempPathToRemotePath(
	tempPath string,
	remoteDirs []string,
	tempDirs []string,
) (string, bool) {
	for i, tempDir := range tempDirs {
		rel, ok := localPathRel(tempDir, tempPath)
		if !ok {
			continue
		}
		return joinRemoteRelative(remoteDirs[i], rel), true
	}
	return "", false
}

func localPathRel(base, name string) (string, bool) {
	rel, err := filepath.Rel(base, name)
	if err != nil {
		return "", false
	}
	if rel == "." {
		return "", true
	}
	if filepath.IsAbs(rel) ||
		rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

func joinRemoteRelative(remoteDir, localRel string) string {
	if localRel == "" {
		return remoteDir
	}
	rel := filepath.ToSlash(localRel)
	if strings.Contains(remoteDir, `\`) && !strings.Contains(remoteDir, "/") {
		return strings.TrimRight(remoteDir, `/\`) + `\` +
			strings.ReplaceAll(rel, "/", `\`)
	}
	return strings.TrimRight(remoteDir, `/\`) + "/" + rel
}
