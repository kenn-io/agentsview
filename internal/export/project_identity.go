package export

import (
	"crypto/sha256"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

const (
	ProjectIdentityKeySourceGitRemote = "git_remote"
	ProjectIdentityKeySourceRootPath  = "root_path"
)

// NormalizeGitRemote converts discoverable network remotes to a stable
// host/path label. Local filesystem remotes are intentionally ignored because
// they are machine-specific and cannot identify a project across archives.
func NormalizeGitRemote(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "/") {
		return "", false
	}
	if strings.HasPrefix(strings.ToLower(raw), "file://") {
		return "", false
	}
	if looksWindowsDrivePath(raw) {
		return "", false
	}

	var host, repoPath string
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return "", false
		}
		switch strings.ToLower(u.Scheme) {
		case "ssh", "git", "https", "http":
		default:
			return "", false
		}
		host = u.Hostname()
		repoPath = u.EscapedPath()
		if p, err := url.PathUnescape(repoPath); err == nil {
			repoPath = p
		}
	} else {
		var ok bool
		host, repoPath, ok = splitSCPGitRemote(raw)
		if !ok {
			return "", false
		}
	}

	host = strings.ToLower(strings.TrimSpace(host))
	repoPath = strings.TrimSpace(repoPath)
	if host == "" || repoPath == "" {
		return "", false
	}
	cleaned := path.Clean("/" + strings.ReplaceAll(repoPath, "\\", "/"))
	cleaned = strings.Trim(cleaned, "/")
	cleaned = strings.TrimSuffix(cleaned, ".git")
	cleaned = strings.Trim(cleaned, "/")
	if cleaned == "" || cleaned == "." {
		return "", false
	}
	return host + "/" + cleaned, true
}

func SanitizeGitRemoteForStorage(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if _, ok := NormalizeGitRemote(raw); !ok {
		return ""
	}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return ""
		}
		u.User = nil
		u.RawQuery = ""
		u.ForceQuery = false
		u.Fragment = ""
		sanitized := u.String()
		if _, ok := NormalizeGitRemote(sanitized); !ok {
			return ""
		}
		return sanitized
	}
	host, repoPath, ok := splitSCPGitRemote(raw)
	if !ok {
		return ""
	}
	sanitized := host + ":" + repoPath
	if _, ok := NormalizeGitRemote(sanitized); !ok {
		return ""
	}
	return sanitized
}

func SanitizeStoredProjectIdentityObservation(
	obs ProjectIdentityObservation,
) ProjectIdentityObservation {
	obs.GitRemote = SanitizeGitRemoteForStorage(obs.GitRemote)
	identity := BuildStoredProjectIdentity(ProjectIdentityInput{
		RootPath:         obs.RootPath,
		GitRemote:        obs.GitRemote,
		GitRemoteName:    obs.GitRemoteName,
		WorktreeName:     obs.WorktreeName,
		WorktreeRootPath: obs.WorktreeRootPath,
	})
	obs.NormalizedRemote = identity.NormalizedRemote
	obs.KeySource = identity.KeySource
	obs.Key = identity.Key
	return obs
}

func SelectRemote(remotes map[string]string) (name string, raw string, ok bool) {
	if raw, exists := remotes["origin"]; exists {
		if _, normalized := NormalizeGitRemote(raw); normalized {
			return "origin", raw, true
		}
	}
	names := make([]string, 0, len(remotes))
	for n := range remotes {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if _, normalized := NormalizeGitRemote(remotes[n]); normalized {
			return n, remotes[n], true
		}
	}
	return "", "", false
}

func NormalizeRootPath(raw string) (normalized string, ok bool, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.Contains(raw, "://") {
		return "", false, nil
	}
	if looksWindowsDrivePath(raw) {
		normalized := normalizeWindowsDriveRootPath(raw)
		if runtime.GOOS == "windows" {
			if resolved, ok := resolveStoredRootPath(normalized); ok {
				return resolved, true, nil
			}
		}
		return normalized, true, nil
	}
	if looksRemotePrefixed(raw) {
		return "", false, nil
	}
	if !filepath.IsAbs(raw) {
		return "", false, nil
	}
	cleaned := filepath.Clean(raw)
	resolved, err := filepath.EvalSymlinks(cleaned)
	if err != nil && !os.IsNotExist(err) {
		return "", false, err
	}
	if err != nil {
		resolved = cleaned
	}
	abs, err := filepath.Abs(resolved)
	if err != nil {
		return "", false, err
	}
	return filepath.Clean(abs), true, nil
}

func NormalizeStoredRootPath(raw string) (normalized string, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.Contains(raw, "://") {
		return "", false
	}
	if looksWindowsDrivePath(raw) {
		normalized := normalizeWindowsDriveRootPath(raw)
		if runtime.GOOS == "windows" {
			if resolved, ok := resolveStoredRootPath(normalized); ok {
				return resolved, true
			}
		}
		return normalized, true
	}
	if looksRemotePrefixed(raw) {
		return "", false
	}
	if strings.HasPrefix(raw, "/") {
		cleaned := path.Clean("/" + strings.TrimLeft(
			strings.ReplaceAll(raw, "\\", "/"), "/",
		))
		if resolved, ok := resolveStoredRootPath(cleaned); ok {
			return resolved, true
		}
		return cleaned, true
	}
	if !filepath.IsAbs(raw) {
		return "", false
	}
	cleaned := filepath.Clean(raw)
	if resolved, ok := resolveStoredRootPath(cleaned); ok {
		return resolved, true
	}
	return filepath.ToSlash(cleaned), true
}

func BuildProjectIdentity(input ProjectIdentityInput) ProjectIdentity {
	if normalized, ok := NormalizeGitRemote(input.GitRemote); ok {
		return ProjectIdentity{
			Key:              projectIdentityKey(ProjectIdentityKeySourceGitRemote, normalized),
			KeySource:        ProjectIdentityKeySourceGitRemote,
			NormalizedRemote: normalized,
		}
	}
	if input.GitRemote == "" && input.WorktreeRootPath != "" {
		input.RootPath = input.WorktreeRootPath
	}
	if normalized, ok, err := NormalizeRootPath(input.RootPath); err == nil && ok {
		return ProjectIdentity{
			Key:          projectIdentityKey(ProjectIdentityKeySourceRootPath, normalized),
			KeySource:    ProjectIdentityKeySourceRootPath,
			RootPath:     normalized,
			MachineLocal: true,
		}
	}
	return ProjectIdentity{}
}

func BuildStoredProjectIdentity(input ProjectIdentityInput) ProjectIdentity {
	if normalized, ok := NormalizeGitRemote(input.GitRemote); ok {
		return ProjectIdentity{
			Key:              projectIdentityKey(ProjectIdentityKeySourceGitRemote, normalized),
			KeySource:        ProjectIdentityKeySourceGitRemote,
			NormalizedRemote: normalized,
		}
	}
	if input.GitRemote == "" && input.WorktreeRootPath != "" {
		input.RootPath = input.WorktreeRootPath
	}
	if normalized, ok := NormalizeStoredRootPath(input.RootPath); ok {
		return ProjectIdentity{
			Key:          projectIdentityKey(ProjectIdentityKeySourceRootPath, normalized),
			KeySource:    ProjectIdentityKeySourceRootPath,
			RootPath:     normalized,
			MachineLocal: true,
		}
	}
	return ProjectIdentity{}
}

func BuildProjectsMap(
	rowLabels []string,
	observations []ProjectIdentityObservation,
) map[string]ProjectMapEntry {
	out := make(map[string]ProjectMapEntry, len(rowLabels))
	for _, label := range rowLabels {
		if _, exists := out[label]; !exists {
			out[label] = ProjectMapEntry{Resolution: ProjectResolutionUnknown}
		}
	}

	type candidate struct {
		identity ProjectIdentity
	}
	grouped := map[string]map[string]candidate{}
	for _, obs := range observations {
		identity := BuildStoredProjectIdentity(ProjectIdentityInput{
			RootPath:         obs.RootPath,
			GitRemote:        obs.GitRemote,
			GitRemoteName:    obs.GitRemoteName,
			WorktreeName:     obs.WorktreeName,
			WorktreeRootPath: obs.WorktreeRootPath,
		})
		if identity.Key == "" {
			continue
		}
		if _, ok := grouped[obs.Project]; !ok {
			grouped[obs.Project] = map[string]candidate{}
		}
		grouped[obs.Project][identity.Key] = candidate{identity: identity}
	}
	for project, candidates := range grouped {
		switch len(candidates) {
		case 0:
			out[project] = ProjectMapEntry{Resolution: ProjectResolutionUnknown}
		case 1:
			for _, c := range candidates {
				identity := c.identity
				out[project] = ProjectMapEntry{
					Resolution: ProjectResolutionResolved,
					Identity:   &identity,
				}
			}
		default:
			out[project] = ProjectMapEntry{Resolution: ProjectResolutionAmbiguous}
		}
	}
	return out
}

func projectIdentityKey(source, normalized string) string {
	sum := sha256.Sum256([]byte(source + "\n" + normalized))
	return "sha256:" + fmt.Sprintf("%x", sum)
}

func looksRemotePrefixed(path string) bool {
	colon := strings.Index(path, ":")
	if colon <= 0 {
		return false
	}
	prefix := path[:colon]
	return !strings.ContainsAny(prefix, `/\`)
}

func looksWindowsDrivePath(path string) bool {
	if len(path) < 3 || path[1] != ':' {
		return false
	}
	drive := path[0]
	if (drive < 'A' || drive > 'Z') && (drive < 'a' || drive > 'z') {
		return false
	}
	return path[2] == '\\' || path[2] == '/'
}

func splitSCPGitRemote(raw string) (host, repoPath string, ok bool) {
	if at := strings.LastIndex(raw, "@"); at >= 0 {
		rest := raw[at+1:]
		colon := strings.Index(rest, ":")
		if colon <= 0 || colon == len(rest)-1 {
			return "", "", false
		}
		return rest[:colon], rest[colon+1:], true
	}
	before, after, ok := strings.Cut(raw, ":")
	if !ok {
		return "", "", false
	}
	return before, after, true
}

func normalizeWindowsDriveRootPath(raw string) string {
	normalized := strings.ReplaceAll(raw, "\\", "/")
	drive := strings.ToUpper(normalized[:1]) + normalized[1:2]
	rest := path.Clean("/" + strings.TrimLeft(normalized[2:], "/"))
	if rest == "/" {
		return drive + "/"
	}
	return drive + rest
}

func resolveStoredRootPath(cleaned string) (string, bool) {
	resolved, err := filepath.EvalSymlinks(filepath.FromSlash(cleaned))
	if err != nil {
		return "", false
	}
	abs, err := filepath.Abs(resolved)
	if err != nil {
		return "", false
	}
	return filepath.ToSlash(filepath.Clean(abs)), true
}
