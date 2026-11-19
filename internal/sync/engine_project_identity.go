package sync

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
)

type projectIdentityCacheEntry struct {
	rootPath         string
	repositoryPath   string
	gitDir           string
	gitRemoteName    string
	gitRemote        string
	remoteResolution export.ProjectResolution
	remoteCandidates int
	worktreeName     string
	worktreeRootPath string
	worktreeKind     export.WorktreeRelationship
	expiresAt        time.Time
}

type localGitIdentity struct {
	rootPath       string
	repositoryPath string
	gitDir         string
	remotes        map[string]string
	worktreeKind   export.WorktreeRelationship
}

func identityObservationOrZero(
	obs export.ProjectIdentityObservation,
	ok bool,
) export.ProjectIdentityObservation {
	if !ok {
		return export.ProjectIdentityObservation{}
	}
	return obs
}

func (e *Engine) projectIdentityObservation(
	s db.Session,
) (export.ProjectIdentityObservation, bool) {
	project := strings.TrimSpace(s.Project)
	machine := strings.TrimSpace(s.Machine)
	rootPath := strings.TrimSpace(s.Cwd)
	if project == "" || machine == "" {
		return export.ProjectIdentityObservation{}, false
	}

	cached := e.cachedProjectIdentity(machine, rootPath)
	obs := export.ProjectIdentityObservation{
		SessionID:  s.ID,
		Project:    project,
		Machine:    machine,
		RootPath:   cached.rootPath,
		ObservedAt: time.Now().UTC(),
	}
	obs.GitRemoteName = cached.gitRemoteName
	obs.GitRemote = cached.gitRemote
	obs.RemoteResolution = cached.remoteResolution
	obs.RemoteCandidateCount = cached.remoteCandidates
	obs.WorktreeName = cached.worktreeName
	obs.WorktreeRootPath = cached.worktreeRootPath
	obs.RepositoryPath = cached.repositoryPath
	obs.WorktreeRelationship = cached.worktreeKind
	obs.GitBranch = strings.TrimSpace(s.GitBranch)
	if obs.GitBranch != "" {
		obs.CheckoutState = export.CheckoutBranch
	} else {
		obs.CheckoutState, obs.GitBranch = readGitCheckout(
			cached.gitDir, e.mayProbeLocalPath,
		)
	}
	return obs, true
}

func (e *Engine) projectIdentityObservationForWrite(
	pw pendingWrite,
	s db.Session,
) (export.ProjectIdentityObservation, bool) {
	if pw.sourceIdentityUnverified {
		return export.ProjectIdentityObservation{}, false
	}
	obs, ok := e.projectIdentityObservation(s)
	if !ok {
		return obs, false
	}
	// The source records the repository at session time. A checkout may
	// since have disappeared or changed remotes, so prefer that evidence.
	return export.WithRecordedGitRemote(obs, pw.sess.GitRemote), true
}

// mayDiscoverLocalProject reports whether path may be probed on the local
// filesystem. Only sessions recorded on this machine qualify: another
// machine's cwd (e.g. /home/... from a synced Linux host) is meaningless
// here, and on macOS merely stat'ing such paths wakes the /home automounter —
// with tens of thousands of remote sessions and a one-minute cache TTL that
// becomes a sustained automountd/opendirectoryd CPU storm.
// mayProbeLocalPath also guards export.NormalizeRootPath, which resolves
// symlinks and would reach into a protected location on its own.
func (e *Engine) mayDiscoverLocalProject(machine, path string) bool {
	return !e.disableProjectDiscovery && e.idPrefix == "" && e.pathRewriter == nil &&
		e.isLocalMachineAttribution(machine) && e.mayProbeLocalPath(path)
}

func (e *Engine) cachedProjectIdentity(machine, rootPath string) projectIdentityCacheEntry {
	e.projectIdentityMu.Lock()
	defer e.projectIdentityMu.Unlock()
	if e.projectIdentityCache == nil {
		e.projectIdentityCache = make(map[string]projectIdentityCacheEntry)
	}
	cacheKey := machine + "\x00" + rootPath
	now := time.Now()
	if cached, ok := e.projectIdentityCache[cacheKey]; ok &&
		now.Before(cached.expiresAt) {
		return cached
	}
	identity := projectIdentityCacheEntry{rootPath: rootPath}
	if e.mayDiscoverLocalProject(machine, rootPath) {
		if normalized, ok, err := export.NormalizeRootPath(rootPath); err == nil && ok {
			identity.rootPath = normalized
		}
		if discovered := discoverLocalGitIdentity(
			rootPath, e.mayProbeLocalPath,
		); discovered.rootPath != "" {
			identity.rootPath = discovered.rootPath
			identity.repositoryPath = discovered.repositoryPath
			identity.gitDir = discovered.gitDir
			identity.worktreeRootPath = discovered.rootPath
			identity.worktreeName = filepath.Base(discovered.rootPath)
			identity.worktreeKind = discovered.worktreeKind
			selection := export.ResolveRemoteSelection(discovered.remotes)
			identity.remoteResolution = selection.Resolution
			if selection.Resolution == export.ProjectResolutionUnknown {
				identity.remoteResolution = export.ProjectResolutionResolved
			}
			identity.remoteCandidates = countNormalizedRemoteCandidates(discovered.remotes)
			if selection.Resolution == export.ProjectResolutionResolved {
				identity.gitRemoteName = selection.Name
				identity.gitRemote = selection.Raw
			}
		}
	}
	if identity.worktreeRootPath == "" {
		identity.worktreeName = filepath.Base(identity.rootPath)
		identity.worktreeRootPath = identity.rootPath
		identity.worktreeKind = export.WorktreeUnknown
	}
	identity.expiresAt = now.Add(projectIdentityCacheTTL)
	e.projectIdentityCache[cacheKey] = identity
	return identity
}

func (e *Engine) writeProjectIdentityObservation(
	ctx context.Context, s db.Session,
) error {
	return e.writeProjectIdentityObservationWithSnapshotProject(
		ctx, s, s.Project, "",
	)
}

// writeProjectIdentityObservationWithSnapshotProject prefers recordedRemote,
// the session's source-recorded repository URL, over checkout discovery.
func (e *Engine) writeProjectIdentityObservationWithSnapshotProject(
	ctx context.Context,
	s db.Session,
	snapshotProject string,
	recordedRemote string,
) error {
	obs, ok := e.projectIdentityObservation(s)
	if !ok {
		return nil
	}
	obs = export.WithRecordedGitRemote(obs, recordedRemote)
	snapshot := obs
	snapshot.Project = snapshotProject
	fingerprint := projectIdentityObservationFingerprint(obs) + "\x00" +
		projectIdentityObservationFingerprint(snapshot)
	e.projectIdentityMu.Lock()
	if e.projectIdentityWritten == nil {
		e.projectIdentityWritten = make(map[string]struct{})
	}
	if _, ok := e.projectIdentityWritten[fingerprint]; ok {
		e.projectIdentityMu.Unlock()
		return nil
	}
	e.projectIdentityMu.Unlock()

	if err := e.db.UpsertProjectIdentityObservationWithSnapshotProject(
		ctx, obs, snapshotProject,
	); err != nil {
		return err
	}

	e.projectIdentityMu.Lock()
	e.projectIdentityWritten[fingerprint] = struct{}{}
	e.projectIdentityMu.Unlock()
	return nil
}

func projectIdentityObservationFingerprint(
	obs export.ProjectIdentityObservation,
) string {
	return strings.Join([]string{
		obs.Project,
		obs.SessionID,
		obs.Machine,
		obs.RootPath,
		obs.GitRemote,
		obs.GitRemoteName,
		obs.WorktreeName,
		obs.WorktreeRootPath,
		obs.RepositoryPath,
		string(obs.WorktreeRelationship),
		string(obs.CheckoutState),
		obs.GitBranch,
		string(obs.RemoteResolution),
		strconv.Itoa(obs.RemoteCandidateCount),
	}, "\x00")
}

func countNormalizedRemoteCandidates(remotes map[string]string) int {
	unique := make(map[string]struct{}, len(remotes))
	for _, raw := range remotes {
		if normalized, ok := export.NormalizeGitRemote(raw); ok {
			unique[normalized] = struct{}{}
		}
	}
	return len(unique)
}

func discoverLocalGitIdentity(
	cwd string, mayProbe func(string) bool,
) localGitIdentity {
	if !safeLocalAbsolutePath(cwd) {
		return localGitIdentity{}
	}
	// Skip macOS automounter namespaces: probing them wakes
	// automountd/opendirectoryd for paths that virtually never exist
	// locally (see export.IsAutomountNamespacePath).
	if export.IsAutomountNamespacePath(runtime.GOOS, filepath.Clean(cwd)) {
		return localGitIdentity{}
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(cwd))
	if err != nil {
		return localGitIdentity{}
	}
	root := findLocalGitRoot(resolved, mayProbe)
	if root == "" {
		return localGitIdentity{}
	}
	gitDir, commonDir, relationship := gitDirectoryContext(root, mayProbe)
	result := localGitIdentity{
		rootPath:       root,
		repositoryPath: repositoryPathForGitContext(root, commonDir, mayProbe),
		gitDir:         gitDir,
		worktreeKind:   relationship,
	}
	// The common directory comes from gitfile contents and can point
	// anywhere, including a protected location the vetted cwd never named,
	// and the config file inside a vetted one can itself be a symlink out;
	// vetting the exact file path covers both.
	if commonDir != "" {
		configPath := filepath.Join(commonDir, "config")
		if mayProbe(configPath) {
			result.remotes = readGitRemotes(configPath)
		}
	}
	return result
}

func safeLocalAbsolutePath(p string) bool {
	p = strings.TrimSpace(p)
	if p == "" || strings.Contains(p, "://") {
		return false
	}
	if looksWindowsDrivePath(p) {
		return runtime.GOOS == "windows" && filepath.IsAbs(p)
	}
	if looksRemotePrefixedPath(p) {
		return false
	}
	return filepath.IsAbs(p)
}

func looksRemotePrefixedPath(p string) bool {
	colon := strings.Index(p, ":")
	if colon <= 0 {
		return false
	}
	prefix := p[:colon]
	return !strings.ContainsAny(prefix, `/\`)
}

func looksWindowsDrivePath(p string) bool {
	if len(p) < 3 || p[1] != ':' {
		return false
	}
	drive := p[0]
	if (drive < 'A' || drive > 'Z') && (drive < 'a' || drive > 'z') {
		return false
	}
	return p[2] == '\\' || p[2] == '/'
}

func findLocalGitRoot(start string, mayProbe func(string) bool) string {
	dir := filepath.Clean(start)
	for {
		if gitEntryMarksRoot(filepath.Join(dir, ".git"), mayProbe) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// gitEntryMarksRoot reports whether gitPath denotes a git directory or
// gitfile, following a symlink only when its target passes mayProbe. A
// refused link still marks a root: it is a repo boundary we must not look
// through, and gitDirectoryContext refuses the reads under it.
func gitEntryMarksRoot(gitPath string, mayProbe func(string) bool) bool {
	info, err := os.Lstat(gitPath)
	if err != nil {
		return false
	}
	if info.Mode()&os.ModeSymlink != 0 {
		if !mayProbe(gitPath) {
			return true
		}
		followed, err := os.Stat(gitPath)
		if err != nil {
			return false
		}
		return followed.IsDir() || followed.Mode().IsRegular()
	}
	return info.IsDir() || info.Mode().IsRegular()
}

func gitDirectoryContext(
	root string, mayProbe func(string) bool,
) (gitDir, commonDir string, relationship export.WorktreeRelationship) {
	gitPath := filepath.Join(root, ".git")
	// root is vetted, but the .git entry itself can be a symlink into a
	// guarded location; classification follows links, so vetting the exact
	// path keeps both the type probe and the gitfile read (and every later
	// HEAD, config, and commondir read under the returned directories)
	// from traversing into it.
	if !mayProbe(gitPath) {
		return "", "", export.WorktreeUnknown
	}
	if info, err := os.Stat(gitPath); err == nil && info.IsDir() {
		return gitPath, gitPath, export.WorktreeMain
	}
	data, err := os.ReadFile(gitPath)
	if err != nil {
		return "", "", export.WorktreeUnknown
	}
	line := strings.TrimSpace(string(data))
	line = strings.TrimPrefix(line, "gitdir:")
	line = strings.TrimSpace(line)
	if line == "" {
		return "", "", export.WorktreeUnknown
	}
	if !filepath.IsAbs(line) {
		line = filepath.Join(root, line)
	}
	// The gitfile target comes from file contents, not from the vetted
	// cwd: a linked worktree in an unguarded directory can point at a
	// main repository inside a protected folder, and reading commondir or
	// HEAD there would raise the consent prompt the cwd gate prevented.
	if !mayProbe(line) {
		return "", "", export.WorktreeUnknown
	}
	commonDir = line
	relationship = export.WorktreeMain
	// The commondir file sits inside the vetted gitdir, but as a symlink
	// it can lead anywhere; vet the exact path (classification follows
	// links) before reading through it.
	commondirPath := filepath.Join(line, "commondir")
	if !mayProbe(commondirPath) {
		return filepath.Clean(line), filepath.Clean(commonDir), relationship
	}
	if data, err := os.ReadFile(commondirPath); err == nil {
		common := strings.TrimSpace(string(data))
		if filepath.IsAbs(common) {
			commonDir = common
		} else {
			commonDir = filepath.Clean(filepath.Join(line, common))
		}
		relationship = export.WorktreeLinked
	}
	return filepath.Clean(line), filepath.Clean(commonDir), relationship
}

func repositoryPathForGitContext(
	root, commonDir string, mayProbe func(string) bool,
) string {
	repositoryPath := commonDir
	if commonDir == "" {
		repositoryPath = root
	} else if filepath.Base(commonDir) == ".git" {
		repositoryPath = filepath.Dir(commonDir)
	}
	// Storing the path string is harmless; resolving its symlinks is a
	// filesystem walk that must respect the protected-path policy.
	if !mayProbe(repositoryPath) {
		return filepath.Clean(repositoryPath)
	}
	if resolved, err := filepath.EvalSymlinks(repositoryPath); err == nil {
		return resolved
	}
	return filepath.Clean(repositoryPath)
}

func readGitCheckout(
	gitDir string, mayProbe func(string) bool,
) (export.CheckoutState, string) {
	if gitDir == "" {
		return export.CheckoutUnknown, ""
	}
	// HEAD sits inside the vetted git directory, but as a symlink it can
	// lead anywhere; vet the exact path before reading through it.
	headPath := filepath.Join(gitDir, "HEAD")
	if !mayProbe(headPath) {
		return export.CheckoutUnknown, ""
	}
	data, err := os.ReadFile(headPath)
	if err != nil {
		return export.CheckoutUnknown, ""
	}
	head := strings.TrimSpace(string(data))
	const branchPrefix = "ref: refs/heads/"
	if after, ok := strings.CutPrefix(head, branchPrefix); ok {
		branch := strings.TrimSpace(after)
		if branch != "" {
			return export.CheckoutBranch, branch
		}
	}
	if head != "" {
		return export.CheckoutDetached, ""
	}
	return export.CheckoutUnknown, ""
}

func readGitRemotes(configPath string) map[string]string {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil
	}
	remotes := map[string]string{}
	var current string
	for line := range strings.SplitSeq(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			current = remoteNameFromGitConfigSection(trimmed)
			continue
		}
		if current == "" || !strings.HasPrefix(trimmed, "url") {
			continue
		}
		key, value, ok := strings.Cut(trimmed, "=")
		if !ok || strings.TrimSpace(key) != "url" {
			continue
		}
		remotes[current] = strings.TrimSpace(value)
	}
	return remotes
}

func remoteNameFromGitConfigSection(section string) string {
	section = strings.Trim(section, "[]")
	if !strings.HasPrefix(section, `remote `) {
		return ""
	}
	name := strings.TrimSpace(strings.TrimPrefix(section, `remote `))
	return strings.Trim(name, `"`)
}
