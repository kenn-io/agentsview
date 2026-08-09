package export

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"unicode"
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
		scheme := strings.ToLower(u.Scheme)
		switch scheme {
		case "ssh", "git", "https", "http":
		default:
			return "", false
		}
		host = normalizeGitRemoteHost(u.Hostname(), scheme, u.Port())
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
		if suffix := strings.IndexAny(repoPath, "?#"); suffix >= 0 {
			repoPath = repoPath[:suffix]
		}
		host = normalizeGitRemoteHost(host, "", "")
	}

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

func normalizeGitRemoteHost(host, scheme, port string) string {
	host = strings.TrimSuffix(
		strings.ToLower(strings.TrimSpace(host)), ".")
	if parsed := net.ParseIP(host); parsed != nil {
		host = parsed.String()
		if parsed.To4() != nil {
			if port != "" && !defaultGitRemotePort(scheme, port) {
				return net.JoinHostPort(host, port)
			}
			return host
		}
		if port == "" || defaultGitRemotePort(scheme, port) {
			return "[" + host + "]"
		}
		return net.JoinHostPort(host, port)
	}
	if port != "" && !defaultGitRemotePort(scheme, port) {
		return net.JoinHostPort(host, port)
	}
	return host
}

func defaultGitRemotePort(scheme, port string) bool {
	switch scheme {
	case "ssh":
		return port == "22"
	case "git":
		return port == "9418"
	case "https":
		return port == "443"
	case "http":
		return port == "80"
	default:
		return false
	}
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
	if suffix := strings.IndexAny(repoPath, "?#"); suffix >= 0 {
		repoPath = repoPath[:suffix]
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
	if obs.RemoteResolution == ProjectResolutionAmbiguous {
		obs.GitRemote = ""
		obs.GitRemoteName = ""
		obs.NormalizedRemote = ""
		obs.KeySource = ""
		obs.Key = ""
		return obs
	}
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
	selection := ResolveRemoteSelection(remotes)
	if selection.Resolution == ProjectResolutionResolved {
		return selection.Name, selection.Raw, true
	}
	return "", "", false
}

func ResolveRemoteSelection(remotes map[string]string) RemoteSelection {
	if raw, exists := remotes["origin"]; exists {
		if normalized, ok := NormalizeGitRemote(raw); ok {
			return RemoteSelection{
				Resolution: ProjectResolutionResolved,
				Name:       "origin",
				Raw:        raw,
				Normalized: normalized,
			}
		}
	}

	names := make([]string, 0, len(remotes))
	for name := range remotes {
		names = append(names, name)
	}
	sort.Strings(names)

	byRemote := make(map[string]RemoteSelection)
	for _, name := range names {
		raw := remotes[name]
		normalized, ok := NormalizeGitRemote(raw)
		if !ok {
			continue
		}
		if _, exists := byRemote[normalized]; !exists {
			byRemote[normalized] = RemoteSelection{
				Resolution: ProjectResolutionResolved,
				Name:       name,
				Raw:        raw,
				Normalized: normalized,
			}
		}
	}

	switch len(byRemote) {
	case 0:
		return RemoteSelection{Resolution: ProjectResolutionUnknown}
	case 1:
		for _, selection := range byRemote {
			return selection
		}
	default:
		return RemoteSelection{Resolution: ProjectResolutionAmbiguous}
	}
	return RemoteSelection{Resolution: ProjectResolutionUnknown}
}

func ResolveProjectReference(
	input ProjectIdentityInput,
	scope IdentityScope,
) ProjectReference {
	reference := ProjectReference{
		ProjectKey:   projectLabelKey(scope, input.DisplayLabel),
		DisplayLabel: safeProjectMetadata(input.DisplayLabel),
		Resolution:   ProjectResolutionUnknown,
		Worktree: WorktreeReference{
			Relationship: input.WorktreeKind,
		},
		Checkout: CheckoutReference{State: CheckoutUnknown},
	}
	if reference.Worktree.Relationship == "" {
		reference.Worktree.Relationship = WorktreeUnknown
	}
	if !validWorktreeRelationship(reference.Worktree.Relationship) {
		reference.Worktree.Relationship = WorktreeUnknown
	}
	if input.Detached {
		reference.Checkout.State = CheckoutDetached
	} else if branch := safeProjectMetadata(input.GitBranch); branch != "" {
		reference.Checkout.State = CheckoutBranch
		reference.Checkout.Branch = branch
	}
	if reference.Worktree.Relationship == WorktreeNone {
		reference.Checkout = CheckoutReference{State: CheckoutUnknown}
	}

	rootPath := input.RootPath
	if rootPath == "" {
		rootPath = input.WorktreeRootPath
	}
	normalizedRoot, hasRoot := NormalizeStoredRootPath(rootPath)
	hasLocalScope := strings.TrimSpace(scope.ArchiveID) != "" &&
		strings.TrimSpace(scope.ArchiveSalt) != "" &&
		strings.TrimSpace(scope.MachineID) != ""
	rootKey := ""
	if hasRoot && hasLocalScope {
		rootKey = scopedProjectKey(
			"r1", "agentsview/root/v1", scope.ArchiveSalt,
			scope.ArchiveID, scope.MachineID, normalizedRoot,
		)
	}

	worktreeRoot := input.WorktreeRootPath
	actualWorktree := reference.Worktree.Relationship == WorktreeMain ||
		reference.Worktree.Relationship == WorktreeLinked
	if normalized, ok := NormalizeStoredRootPath(worktreeRoot); ok && actualWorktree && hasLocalScope {
		reference.Worktree.WorktreeKey = scopedProjectKey(
			"wt1", "agentsview/worktree/v1", scope.ArchiveSalt,
			scope.ArchiveID, scope.MachineID, normalized,
		)
	}
	hasRepositoryContext := strings.TrimSpace(input.RepositoryPath) != "" ||
		reference.Worktree.Relationship == WorktreeMain ||
		reference.Worktree.Relationship == WorktreeLinked
	localRepositoryKey := ""
	if hasRoot && hasLocalScope && hasRepositoryContext {
		repositoryPath := input.RepositoryPath
		if repositoryPath == "" {
			repositoryPath = rootPath
		}
		normalizedRepository, ok := NormalizeStoredRootPath(repositoryPath)
		if !ok {
			normalizedRepository = normalizedRoot
		}
		localRepositoryKey = scopedProjectKey(
			"repo1", "agentsview/repository/root/v1", scope.ArchiveSalt,
			scope.ArchiveID, scope.MachineID, normalizedRepository,
		)
		reference.Worktree.RepositoryKey = localRepositoryKey
	}
	if input.RemoteSelection.Resolution == ProjectResolutionAmbiguous {
		reference.Resolution = ProjectResolutionAmbiguous
		return reference
	}

	normalizedRemote := input.RemoteSelection.Normalized
	if normalizedRemote == "" {
		normalizedRemote, _ = NormalizeGitRemote(input.GitRemote)
	}
	if normalizedRemote != "" {
		repositoryKey := scopedProjectKey("repo1", "agentsview/repository/git/v1", normalizedRemote)
		reference.Resolution = ProjectResolutionResolved
		reference.Identity = &ProjectIdentity{
			Key:              scopedProjectKey("p1", "agentsview/project/git/v1", normalizedRemote),
			Kind:             ProjectKindGitRemote,
			NormalizedRemote: normalizedRemote,
			RootKey:          rootKey,
			RepositoryKey:    repositoryKey,
		}
		reference.Worktree.RepositoryKey = repositoryKey
		return reference
	}

	if !hasRoot || !hasLocalScope {
		return reference
	}
	if localRepositoryKey == "" {
		return reference
	}
	reference.Resolution = ProjectResolutionResolved
	reference.Identity = &ProjectIdentity{
		Key: scopedProjectKey(
			"p1", "agentsview/project/root/v1", localRepositoryKey,
		),
		Kind:          ProjectKindMachineRoot,
		RootKey:       rootKey,
		RepositoryKey: localRepositoryKey,
	}
	return reference
}

// AggregateIdentityScope derives a deterministic private label-key scope for
// an unselected shared-store dashboard. Per-observation project identities
// remain archive-scoped; only aggregate catalog keys use this synthetic scope.
func AggregateIdentityScope(scopes []IdentityScope) IdentityScope {
	parts := make([]string, 0, len(scopes)*2)
	for _, scope := range scopes {
		if strings.TrimSpace(scope.ArchiveID) == "" ||
			strings.TrimSpace(scope.ArchiveSalt) == "" {
			continue
		}
		parts = append(parts, scope.ArchiveID+"\x00"+scope.ArchiveSalt)
	}
	if len(parts) == 0 {
		return IdentityScope{}
	}
	sort.Strings(parts)
	return IdentityScope{
		ArchiveID: scopedProjectKey(
			"as1", append([]string{"agentsview/archive-set/v1"}, parts...)...,
		),
		ArchiveSalt: scopedProjectKey(
			"ass1", append([]string{"agentsview/archive-set-salt/v1"}, parts...)...,
		),
	}
}

// LegacySharedStoreIdentityScope provides a deterministic response scope for
// PostgreSQL and DuckDB stores populated before source archive identities were
// published. Shared-store catalog keys are response-scoped selectors, not
// durable project identities; this fallback prevents unrelated labels from
// collapsing without scanning or mutating legacy session rows.
func LegacySharedStoreIdentityScope() IdentityScope {
	return IdentityScope{
		ArchiveID: scopedProjectKey(
			"as1", "agentsview/shared-store/legacy/v1",
		),
		ArchiveSalt: scopedProjectKey(
			"ass1", "agentsview/shared-store/legacy-salt/v1",
		),
	}
}

func ResolveProjectReferenceFromObservation(
	obs ProjectIdentityObservation,
	archiveScope IdentityScope,
) ProjectReference {
	selection := RemoteSelection{Resolution: obs.RemoteResolution}
	if normalized, ok := NormalizeGitRemote(obs.GitRemote); ok {
		selection.Name = obs.GitRemoteName
		selection.Raw = obs.GitRemote
		selection.Normalized = normalized
		if selection.Resolution == "" ||
			selection.Resolution == ProjectResolutionUnknown {
			selection.Resolution = ProjectResolutionResolved
		}
	}
	archiveScope.MachineID = obs.Machine
	return ResolveProjectReference(ProjectIdentityInput{
		DisplayLabel:     obs.Project,
		RootPath:         obs.RootPath,
		GitRemote:        obs.GitRemote,
		GitRemoteName:    obs.GitRemoteName,
		RemoteSelection:  selection,
		RepositoryPath:   obs.RepositoryPath,
		WorktreeName:     obs.WorktreeName,
		WorktreeRootPath: obs.WorktreeRootPath,
		WorktreeKind:     obs.WorktreeRelationship,
		GitBranch:        obs.GitBranch,
		Detached:         obs.CheckoutState == CheckoutDetached,
	}, archiveScope)
}

func scopedProjectKey(prefix string, parts ...string) string {
	h := sha256.New()
	var size [4]byte
	for _, part := range parts {
		binary.BigEndian.PutUint32(size[:], uint32(len(part)))
		_, _ = h.Write(size[:])
		_, _ = h.Write([]byte(part))
	}
	return prefix + ":sha256:" + fmt.Sprintf("%x", h.Sum(nil))
}

func projectLabelKey(scope IdentityScope, label string) string {
	if strings.TrimSpace(scope.ArchiveID) == "" ||
		strings.TrimSpace(scope.ArchiveSalt) == "" {
		return ""
	}
	return scopedProjectKey(
		"pl1", "agentsview/project-label/v1", scope.ArchiveSalt,
		scope.ArchiveID, label,
	)
}

func safeProjectMetadata(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "/") ||
		strings.HasPrefix(value, `\`) || filepath.IsAbs(value) ||
		looksWindowsDrivePath(value) ||
		looksURLScheme(value) {
		return ""
	}
	return value
}

func looksURLScheme(value string) bool {
	colon := strings.IndexByte(value, ':')
	if colon <= 0 {
		return false
	}
	for i, char := range value[:colon] {
		if (char >= 'a' && char <= 'z') ||
			(char >= 'A' && char <= 'Z') ||
			(i > 0 && ((char >= '0' && char <= '9') || char == '+' || char == '-' || char == '.')) {
			continue
		}
		return false
	}
	rest := value[colon+1:]
	for _, char := range rest {
		return !unicode.IsSpace(char)
	}
	return true
}

func SafeProjectDisplayLabel(value string) string {
	return safeProjectMetadata(value)
}

func ProjectMapForWire(
	projects map[string]ProjectMapEntry,
) map[string]ProjectMapEntry {
	out := make(map[string]ProjectMapEntry, len(projects))
	labels := make([]string, 0, len(projects))
	for rawLabel := range projects {
		labels = append(labels, rawLabel)
	}
	sort.Strings(labels)
	for _, rawLabel := range labels {
		entry := projects[rawLabel]
		entry.DisplayLabel = safeProjectMetadata(rawLabel)
		key := entry.ProjectKey
		if key == "" {
			continue
		}
		out[key] = entry
	}
	return out
}

func ProjectKeyForEntry(entry ProjectMapEntry) string {
	return entry.ProjectKey
}

func validWorktreeRelationship(value WorktreeRelationship) bool {
	switch value {
	case WorktreeMain, WorktreeLinked, WorktreeNone, WorktreeUnknown:
		return true
	default:
		return false
	}
}

func NormalizeRootPath(raw string) (normalized string, ok bool, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.Contains(raw, "://") {
		return "", false, nil
	}
	if looksWindowsDrivePath(raw) {
		normalized := normalizeWindowsDriveRootPath(raw)
		if runtime.GOOS == "windows" {
			if resolved, ok := resolveLiveRootPath(normalized); ok {
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
	resolved := cleaned
	if !IsAutomountNamespacePath(runtime.GOOS, cleaned) {
		resolved, err = filepath.EvalSymlinks(cleaned)
		if err != nil && !os.IsNotExist(err) {
			return "", false, err
		}
		if err != nil {
			resolved = cleaned
		}
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
		return normalizeWindowsDriveRootPath(raw), true
	}
	if looksRemotePrefixed(raw) {
		return "", false
	}
	if strings.HasPrefix(raw, "/") {
		cleaned := path.Clean("/" + strings.TrimLeft(
			strings.ReplaceAll(raw, "\\", "/"), "/",
		))
		return cleaned, true
	}
	if !filepath.IsAbs(raw) {
		return "", false
	}
	cleaned := filepath.Clean(raw)
	return filepath.ToSlash(cleaned), true
}

func BuildProjectIdentity(input ProjectIdentityInput) StoredProjectIdentity {
	if normalized, ok := NormalizeGitRemote(input.GitRemote); ok {
		return StoredProjectIdentity{
			Key:              projectIdentityKey(ProjectIdentityKeySourceGitRemote, normalized),
			KeySource:        ProjectIdentityKeySourceGitRemote,
			NormalizedRemote: normalized,
		}
	}
	if input.GitRemote == "" && input.WorktreeRootPath != "" {
		input.RootPath = input.WorktreeRootPath
	}
	if normalized, ok, err := NormalizeRootPath(input.RootPath); err == nil && ok {
		return StoredProjectIdentity{
			Key:          projectIdentityKey(ProjectIdentityKeySourceRootPath, normalized),
			KeySource:    ProjectIdentityKeySourceRootPath,
			RootPath:     normalized,
			MachineLocal: true,
		}
	}
	return StoredProjectIdentity{}
}

func BuildStoredProjectIdentity(input ProjectIdentityInput) StoredProjectIdentity {
	if normalized, ok := NormalizeGitRemote(input.GitRemote); ok {
		return StoredProjectIdentity{
			Key:              projectIdentityKey(ProjectIdentityKeySourceGitRemote, normalized),
			KeySource:        ProjectIdentityKeySourceGitRemote,
			NormalizedRemote: normalized,
		}
	}
	if input.GitRemote == "" && input.WorktreeRootPath != "" {
		input.RootPath = input.WorktreeRootPath
	}
	if normalized, ok := NormalizeStoredRootPath(input.RootPath); ok {
		return StoredProjectIdentity{
			Key:          projectIdentityKey(ProjectIdentityKeySourceRootPath, normalized),
			KeySource:    ProjectIdentityKeySourceRootPath,
			RootPath:     normalized,
			MachineLocal: true,
		}
	}
	return StoredProjectIdentity{}
}

func BuildProjectsMap(
	rowLabels []string,
	observations []ProjectIdentityObservation,
) map[string]ProjectMapEntry {
	return BuildProjectsMapWithScope(rowLabels, observations, IdentityScope{})
}

// ProjectCatalogIdentity returns a detached identity suitable for a
// response-level project catalog. Remote-backed catalog entries omit the
// machine-local root; complete root facts remain on session references.
func ProjectCatalogIdentity(identity *ProjectIdentity) *ProjectIdentity {
	if identity == nil {
		return nil
	}
	copy := *identity
	if copy.Kind == ProjectKindGitRemote {
		copy.RootKey = ""
	}
	return &copy
}

func BuildProjectsMapWithScope(
	rowLabels []string,
	observations []ProjectIdentityObservation,
	archiveScope IdentityScope,
) map[string]ProjectMapEntry {
	out := make(map[string]ProjectMapEntry, len(rowLabels))
	for _, label := range rowLabels {
		if _, exists := out[label]; !exists {
			out[label] = ProjectMapEntry{
				ProjectKey: projectLabelKey(archiveScope, label),
				Resolution: ProjectResolutionUnknown,
			}
		}
	}

	type candidate struct {
		identity ProjectIdentity
	}
	grouped := map[string]map[string]candidate{}
	ambiguous := map[string]bool{}
	for _, obs := range observations {
		scope := archiveScope
		if obs.SourceArchiveID != "" && obs.SourceArchiveSalt != "" {
			scope.ArchiveID = obs.SourceArchiveID
			scope.ArchiveSalt = obs.SourceArchiveSalt
		}
		entry := out[obs.Project]
		if entry.ProjectKey == "" {
			entry.ProjectKey = projectLabelKey(scope, obs.Project)
			out[obs.Project] = entry
		}
		if obs.RemoteResolution == ProjectResolutionAmbiguous {
			ambiguous[obs.Project] = true
			continue
		}
		reference := ResolveProjectReferenceFromObservation(obs, scope)
		if reference.Resolution == ProjectResolutionAmbiguous {
			ambiguous[obs.Project] = true
			continue
		}
		if reference.Identity == nil {
			continue
		}
		identity := *ProjectCatalogIdentity(reference.Identity)
		if _, ok := grouped[obs.Project]; !ok {
			grouped[obs.Project] = map[string]candidate{}
		}
		grouped[obs.Project][identity.Key] = candidate{identity: identity}
	}
	for project, candidates := range grouped {
		projectKey := out[project].ProjectKey
		if ambiguous[project] {
			out[project] = ProjectMapEntry{
				ProjectKey: projectKey, Resolution: ProjectResolutionAmbiguous,
			}
			continue
		}
		switch len(candidates) {
		case 0:
			out[project] = ProjectMapEntry{
				ProjectKey: projectKey, Resolution: ProjectResolutionUnknown,
			}
		case 1:
			for _, c := range candidates {
				identity := c.identity
				out[project] = ProjectMapEntry{
					ProjectKey: projectKey, Resolution: ProjectResolutionResolved,
					Identity: &identity,
				}
			}
		default:
			out[project] = ProjectMapEntry{
				ProjectKey: projectKey, Resolution: ProjectResolutionAmbiguous,
			}
		}
	}
	for project := range ambiguous {
		if _, exists := grouped[project]; !exists {
			out[project] = ProjectMapEntry{
				ProjectKey: out[project].ProjectKey,
				Resolution: ProjectResolutionAmbiguous,
			}
		}
	}
	return out
}

func PublicProjectIdentityFromStored(stored StoredProjectIdentity) (ProjectIdentity, bool) {
	if stored.KeySource != ProjectIdentityKeySourceGitRemote || stored.NormalizedRemote == "" {
		return ProjectIdentity{}, false
	}
	repositoryKey := scopedProjectKey("repo1", "agentsview/repository/git/v1", stored.NormalizedRemote)
	return ProjectIdentity{
		Key:              scopedProjectKey("p1", "agentsview/project/git/v1", stored.NormalizedRemote),
		Kind:             ProjectKindGitRemote,
		NormalizedRemote: stored.NormalizedRemote,
		RepositoryKey:    repositoryKey,
	}, true
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

// IsAutomountNamespacePath reports whether p lies inside a macOS
// automounter namespace (/home, /net, /Network/Servers). On darwin, merely
// stat'ing such a path wakes automountd, which resolves the map through
// opendirectoryd, and negative results are not cached. Live capture skips
// filesystem resolution for these paths and uses the cleaned path directly.
// goos is a parameter so the predicate is testable off-darwin.
func IsAutomountNamespacePath(goos, p string) bool {
	if goos != "darwin" {
		return false
	}
	for _, ns := range [...]string{"/home", "/net", "/Network/Servers"} {
		if p == ns || strings.HasPrefix(p, ns+"/") {
			return true
		}
	}
	return false
}

// protectedUserDataDirs are home-relative locations macOS guards behind a TCC
// consent prompt. Reading a file under one of them from a bundled app makes
// macOS attribute the request to the bundle and ask the user for access, so a
// first sync that probes recorded working directories produces prompts the
// user has no way to connect to anything they did.
var protectedUserDataDirs = [...]string{
	"Desktop",
	"Documents",
	"Downloads",
	"Movies",
	"Music",
	"Pictures",
	// Cloud providers publish their files as File Provider domains under
	// Library/CloudStorage (Dropbox, OneDrive, Google Drive, Box); iCloud
	// Drive uses Library/Mobile Documents. Both prompt on first access.
	"Library/CloudStorage",
	"Library/Mobile Documents",
	// Dropbox keeps ~/Dropbox pointing into its File Provider domain, so a
	// recorded working directory names it without mentioning CloudStorage.
	"Dropbox",
}

// IsProtectedUserDataPath reports whether p lies inside a macOS TCC-protected
// user data location under home. Passive discovery skips these paths so
// importing an archive cannot trigger consent prompts; the user opts in with
// scan_protected_paths when they want Git-derived identity there instead.
//
// Comparison folds case because the startup volume is case-insensitive by
// default, so a working directory recorded as ~/documents names the protected
// folder just as ~/Documents does. goos and home are parameters so the
// predicate is testable off-darwin.
func IsProtectedUserDataPath(goos, home, p string) bool {
	if goos != "darwin" || strings.TrimSpace(home) == "" || !filepath.IsAbs(p) {
		return false
	}
	cleaned := strings.ToLower(filepath.Clean(p))
	for _, dir := range protectedUserDataDirs {
		root := strings.ToLower(filepath.Join(
			filepath.Clean(home), filepath.FromSlash(dir),
		))
		if cleaned == root ||
			strings.HasPrefix(cleaned, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// maxProtectedPathLinkHops bounds symlink resolution in
// ResolvesIntoProtectedUserDataPath. Hitting the bound treats the path as
// protected, so a link loop fails toward skipping the probe rather than
// looping forever or letting the caller resolve the loop itself.
const maxProtectedPathLinkHops = 40

// LocalPathProbeClass classifies whether passive discovery may touch a local
// path on disk. Safe paths may be probed. Protected paths raise a macOS TCC
// consent prompt and may only be probed under an explicit user opt-in.
// Automount-namespace paths wake automountd on any stat and must never be
// probed, opt-in or not — the two unsafe classes exist so callers cannot
// treat one override as permission for the other.
type LocalPathProbeClass int

const (
	LocalPathProbeSafe LocalPathProbeClass = iota
	LocalPathProbeProtectedUserData
	LocalPathProbeAutomountNamespace
)

// ClassifyLocalPathProbe classifies p for passive probing once symlinks are
// followed. IsProtectedUserDataPath and IsAutomountNamespacePath compare
// lexically, so a working directory like ~/code/proj where ~/code links into
// ~/Documents or /home would pass them and the caller's own Stat or
// EvalSymlinks would then reach into the guarded location. This walk resolves
// one component at a time and checks every candidate lexically before
// touching it with Lstat, so answering the question never enters a protected
// folder or wakes the automounter itself. Unresolvable links classify as
// protected; a missing path component means nothing beyond it can be opened,
// so the remainder is safe.
func ClassifyLocalPathProbe(goos, home, p string) LocalPathProbeClass {
	if goos != "darwin" || !filepath.IsAbs(p) {
		return LocalPathProbeSafe
	}
	// Classify a literal automount input before resolving home, so a
	// caller probing /home paths never pays for home resolution at all.
	if IsAutomountNamespacePath(goos, filepath.Clean(p)) {
		return LocalPathProbeAutomountNamespace
	}
	return classifyLocalPathProbe(goos, probeHomesFor(goos, home), p, 0)
}

// probeHomesCache memoizes probeHomesFor per (goos, home): home never changes
// within a process, and classification runs for every session parse and
// identity-cache miss, so resolving home's symlinks each call is waste.
var probeHomesCache sync.Map

// probeHomesFor returns home plus its symlink-resolved form for lexical
// comparison: once the classification walk hops through a link, it continues
// on resolved prefixes, and a home behind a symlinked ancestor (e.g.
// /var -> /private/var) would never match the unresolved form. Resolution
// walks component-by-component and aborts if any candidate lands in an
// automounter namespace, so a home under /home, or linked through /net,
// never wakes automountd. An unresolvable home leaves the protected checks
// comparing the raw form only.
func probeHomesFor(goos, home string) []string {
	home = strings.TrimSpace(home)
	if home == "" || !filepath.IsAbs(home) {
		return nil
	}
	key := goos + "\x00" + home
	if cached, ok := probeHomesCache.Load(key); ok {
		homes, _ := cached.([]string)
		return homes
	}
	homes := []string{home}
	if resolved, ok := resolvePathAvoidingAutomount(goos, home, 0); ok &&
		filepath.Clean(resolved) != filepath.Clean(home) {
		homes = append(homes, resolved)
	}
	probeHomesCache.Store(key, homes)
	return homes
}

// resolvePathAvoidingAutomount resolves p's symlinks one component at a
// time, refusing (ok=false) as soon as any candidate lies inside a macOS
// automounter namespace so the resolution itself never wakes automountd.
// Unresolvable paths also report ok=false; callers fall back to the raw form.
func resolvePathAvoidingAutomount(
	goos, p string, hops int,
) (resolved string, ok bool) {
	if hops > maxProtectedPathLinkHops {
		return "", false
	}
	if IsAutomountNamespacePath(goos, filepath.Clean(p)) {
		return "", false
	}
	rest := splitAbsPathComponents(p)
	current := string(filepath.Separator)
	for i, component := range rest {
		next := filepath.Join(current, component)
		info, err := osLstat(next)
		if err != nil {
			return "", false
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(next)
			if err != nil {
				return "", false
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(current, target)
			}
			respliced := filepath.Join(
				append([]string{target}, rest[i+1:]...)...,
			)
			return resolvePathAvoidingAutomount(goos, respliced, hops+1)
		}
		current = next
	}
	return current, true
}

func protectedUnderAnyHome(goos string, homes []string, p string) bool {
	for _, home := range homes {
		if IsProtectedUserDataPath(goos, home, p) {
			return true
		}
	}
	return false
}

// osLstat is indirected through a var so tests can assert the resolver never
// touches a namespace it must stay out of. Production code always uses
// os.Lstat via this binding.
var osLstat = os.Lstat

func classifyLocalPathProbe(
	goos string, homes []string, p string, hops int,
) LocalPathProbeClass {
	// The automount check runs per resolution step so a symlink hopping
	// into /home, /net, or /Network/Servers is caught too, not only a
	// literal input. A namespace prefix always survives filepath.Clean,
	// so checking the full path covers every intermediate component.
	if IsAutomountNamespacePath(goos, filepath.Clean(p)) {
		return LocalPathProbeAutomountNamespace
	}
	if hops > maxProtectedPathLinkHops {
		return LocalPathProbeProtectedUserData
	}
	if protectedUnderAnyHome(goos, homes, p) {
		return LocalPathProbeProtectedUserData
	}
	rest := splitAbsPathComponents(p)
	current := string(filepath.Separator)
	for i, component := range rest {
		next := filepath.Join(current, component)
		if protectedUnderAnyHome(goos, homes, next) {
			return LocalPathProbeProtectedUserData
		}
		info, err := osLstat(next)
		if err != nil {
			return LocalPathProbeSafe
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(next)
			if err != nil {
				return LocalPathProbeProtectedUserData
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(current, target)
			}
			resolved := filepath.Join(
				append([]string{target}, rest[i+1:]...)...,
			)
			return classifyLocalPathProbe(goos, homes, resolved, hops+1)
		}
		current = next
	}
	return LocalPathProbeSafe
}

func splitAbsPathComponents(p string) []string {
	trimmed := strings.TrimPrefix(
		filepath.Clean(p), string(filepath.Separator),
	)
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, string(filepath.Separator))
}

func resolveLiveRootPath(cleaned string) (string, bool) {
	if IsAutomountNamespacePath(runtime.GOOS, cleaned) {
		return "", false
	}
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
