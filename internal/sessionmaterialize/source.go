package sessionmaterialize

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/archivesource"
	"go.kenn.io/agentsview/internal/capture"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/ctxio"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/pathutil"
	"go.kenn.io/agentsview/internal/remotesync"
)

const maxSourceBytes = 512 << 20

var errSourceSize = errors.New("native source exceeds size limit")

type sourceFile struct {
	rel  string
	data []byte
}

type Result struct {
	Source          string   `json:"source"`
	Paths           []string `json:"paths"`
	Cwd             string   `json:"cwd"`
	Command         string   `json:"command"`
	Status          string   `json:"status"`
	ConfigDir       string   `json:"-"`
	ProjectOverride string   `json:"-"`
	RequestedID     string   `json:"requested_id,omitempty"`
	InstalledPage   string   `json:"installed_page,omitempty"`
}

// Materialize installs a native transcript and its dependencies.
func Materialize(ctx context.Context, cfg config.Config, database *db.DB, session *db.Session, cwd string) (Result, error) {
	if session == nil || (session.Agent != "claude" && session.Agent != "pi" && session.Agent != "codex") {
		return Result{}, errors.New("only Claude, Pi and Codex sessions can currently be installed")
	}
	cwd, err := filepath.Abs(cwd)
	if err != nil {
		return Result{}, err
	}
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		return Result{}, fmt.Errorf("cwd must be an existing directory: %s", cwd)
	}
	if session.Agent == "pi" {
		return materializePi(ctx, cfg, session, cwd)
	}
	if session.Agent == "codex" {
		return materializeCodex(ctx, cfg, database, session, cwd)
	}
	host, id := parser.StripHostPrefix(session.ID)
	if !capture.ValidUUID(id) {
		return Result{}, errors.New("native Claude UUID required; archive-only fork sessions are unsupported")
	}
	owners, err := database.ArchiveSessionOwners(ctx, session.ID)
	if err != nil {
		return Result{}, err
	}
	owners, err = matchingArchiveOwners(session, owners)
	if err != nil {
		return Result{}, err
	}
	var source string
	var files []sourceFile
	retained, err := useRetained(ctx, cfg, database, session, owners)
	if err != nil {
		return Result{}, err
	}
	if retained {
		files, source, err = readRetainedSource(ctx, cfg, database, session, owners, id)
	} else if len(owners) != 0 {
		if owners[0].NativeSessionID != id || owners[0].Source.DeviceID != session.Machine {
			return Result{}, errors.New("archive session identity differs")
		}
		var snapshot string
		files, snapshot, err = readSnapshotSource(ctx, cfg, owners, id)
		source = "snapshot:" + snapshot
	} else if source, err = storedSource(session, host); err != nil {
		return Result{}, err
	} else if strings.HasPrefix(source, "s3://") {
		if host != "" && host != session.Machine {
			return Result{}, errors.New("S3 source host does not match session identity")
		}
		files, err = readS3Source(ctx, cfg, session.Machine, source, id, maxSourceBytes)
	} else if host != "" {
		if !slices.ContainsFunc(cfg.RemoteHosts, func(remote config.RemoteHost) bool { return remote.Host == host }) {
			return Result{}, errors.New("session host is not configured")
		}
		root := remotesync.MirrorDir(cfg.DataDir, host)
		remotePath, ok := strings.CutPrefix(source, host+":")
		if !ok || remotePath == "" {
			return Result{}, errors.New("mirror source host does not match session identity")
		}
		name, err := remotesync.SafeRemappedRemotePath(root, remotePath)
		if err != nil {
			return Result{}, fmt.Errorf("reading native transcript: %w", err)
		}
		lock, err := remotesync.AcquireMirrorLock(ctx, root)
		if err != nil {
			return Result{}, err
		}
		files, err = readLocalSource(ctx, root, name, id, maxSourceBytes)
		if err != nil {
			return Result{}, fmt.Errorf("reading native transcript: %w", errors.Join(err, lock.Close()))
		}
		if err := lock.Close(); err != nil {
			return Result{}, fmt.Errorf("reading native transcript: %w", err)
		}
	} else {
		files, source, err = readConfiguredLocal(ctx, cfg, session, source, id)
	}
	if err != nil {
		return Result{}, fmt.Errorf("reading native transcript: %w", err)
	}
	parent := session.SourceSessionID
	if parent != "" {
		parentHost, rawParent := parser.StripHostPrefix(parent)
		if (strings.Contains(parent, "~") && (parentHost == "" || parentHost != session.Machine)) || !capture.ValidUUID(rawParent) {
			return Result{}, errors.New("invalid source session identity")
		}
		parent = rawParent
	}
	primary, err := clearClaudeWorktree(files[0].data, id, parent)
	if err != nil {
		return Result{}, err
	}
	files[0].data = primary
	dest, home, projectOverride, err := claudeDestination(cwd, id)
	if err != nil {
		return Result{}, err
	}
	installed := make([]installFile, 0, len(files))
	paths := make([]string, 0, len(files))
	base := strings.TrimSuffix(dest, ".jsonl")
	for _, f := range files[1:] {
		p := filepath.Join(base, filepath.FromSlash(f.rel))
		installed = append(installed, installFile{p, f.data})
		paths = append(paths, p)
	}
	installed = append(installed, installFile{dest, files[0].data})
	paths = append(paths, dest)
	changed, err := publishFiles(ctx, installed)
	if err != nil {
		return Result{}, err
	}
	status := "unchanged"
	if changed {
		status = "installed"
	}
	return Result{Source: source, Paths: paths, Cwd: cwd, Status: status, ConfigDir: home, ProjectOverride: projectOverride}, nil
}

// storedSource returns the archived source path. Only an unqualified local
// session may lack one; mirror and S3 sessions need it to find their original.
func storedSource(session *db.Session, host string) (string, error) {
	if session.FilePath != nil && *session.FilePath != "" {
		return *session.FilePath, nil
	}
	if host == "" {
		return "", nil
	}
	return "", errors.New("native transcript is unavailable from configured sources")
}

func readConfiguredLocal(ctx context.Context, cfg config.Config, session *db.Session, stored, id string) ([]sourceFile, string, error) {
	roots := configuredRoots(cfg, session, parser.AgentClaude)
	provider, ok := parser.NewProvider(parser.AgentClaude, parser.ProviderConfig{Roots: roots, Machine: cfg.InstallationID, SourceMachines: cfg.SourceMachines[parser.AgentClaude]})
	discoverer, streaming := provider.(parser.StreamingDiscoverer)
	allRoots := localRoots(cfg, parser.AgentClaude)
	recorded, recordedOK := parser.NewProvider(parser.AgentClaude, parser.ProviderConfig{Roots: allRoots})
	if !ok || !streaming || !recordedOK {
		return nil, "", errors.New("native source provider unavailable")
	}
	name, err := uniqueLocalSource(ctx, recorded, stored, scopedDiscover(cfg, session, parser.AgentClaude, discoverer.DiscoverEach), func(name string) (bool, error) {
		return filepath.Base(name) == id+".jsonl", nil
	})
	if err != nil {
		return nil, "", err
	}
	// Preserve the configured-root boundary for sources inside it. External
	// linked projects are confined to the provider-selected project instead.
	boundary := filepath.Dir(name)
	for _, root := range allRoots {
		if within(root, name) {
			boundary = root
			break
		}
	}
	files, err := readLocalSource(ctx, boundary, name, id, maxSourceBytes)
	return files, name, err
}

// localRoots excludes remote object stores from recorded filesystem lookup.
func localRoots(cfg config.Config, agent parser.AgentType) []string {
	return slices.DeleteFunc(cfg.ResolveDirs(agent), func(root string) bool {
		return strings.HasPrefix(root, "s3://")
	})
}

// configuredRoots selects roots belonging to the archived session's machine.
func configuredRoots(cfg config.Config, session *db.Session, agent parser.AgentType) []string {
	var roots []string
	for _, root := range localRoots(cfg, agent) {
		if machineOwns(cfg, session, cfg.SourceMachines[agent][root]) {
			roots = append(roots, root)
		}
	}
	return roots
}

// machineOwns reports whether a source label belongs to the archived session.
func machineOwns(cfg config.Config, session *db.Session, machine string) bool {
	if machine == "" {
		machine = cfg.InstallationID
	}
	return machine == session.Machine || (session.Machine == cfg.LocalMachineName && machine == cfg.InstallationID)
}

// pathMachine follows ingest's most-specific configured root attribution.
func pathMachine(cfg config.Config, agent parser.AgentType, name string) (string, bool) {
	key, err := pathutil.LocalComparisonKey(name)
	if err != nil {
		return "", false
	}
	var best, machine string
	for root, label := range cfg.SourceMachines[agent] {
		rootKey, err := pathutil.LocalComparisonKey(root)
		if err != nil || len(rootKey) <= len(best) {
			continue
		}
		if rel, err := filepath.Rel(rootKey, key); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			best, machine = rootKey, label
		}
	}
	return machine, best != ""
}

// scopedDiscover filters fallback candidates by ingest's machine attribution.
func scopedDiscover(cfg config.Config, session *db.Session, agent parser.AgentType, discover func(context.Context, func(parser.SourceRef) error) error) func(context.Context, func(parser.SourceRef) error) error {
	return func(ctx context.Context, yield func(parser.SourceRef) error) error {
		return discover(ctx, func(ref parser.SourceRef) error {
			if machine, ok := pathMachine(cfg, agent, ref.DisplayPath); ok && !machineOwns(cfg, session, machine) {
				return nil
			}
			return yield(ref)
		})
	}
}

// uniqueLocalSource keeps an archived path that still resolves exactly.
// Otherwise it discovers a matching transcript under the provider's roots;
// incomplete discovery or a second physical file fails instead of guessing,
// and aliases of one file, such as overlapping roots or symlinks, count once.
func uniqueLocalSource(ctx context.Context, provider parser.Provider, stored string, discover func(context.Context, func(parser.SourceRef) error) error, matches func(string) (bool, error)) (string, error) {
	if ref, found, err := provider.FindSource(ctx, parser.FindSourceRequest{StoredFilePath: stored}); err != nil || found {
		return ref.DisplayPath, err
	}
	var match string
	var matchInfo os.FileInfo
	err := discover(ctx, func(ref parser.SourceRef) error {
		name := filepath.Clean(ref.DisplayPath)
		matched, err := matches(name)
		if err != nil {
			return err
		}
		if !matched {
			return nil
		}
		info, err := os.Stat(name)
		if err != nil {
			return err
		}
		if matchInfo == nil {
			match, matchInfo = name, info
		} else if !os.SameFile(matchInfo, info) {
			return fmt.Errorf("ambiguous native transcript: %s and %s", match, name)
		}
		return nil
	})
	if err == nil && matchInfo == nil {
		err = errors.New("native transcript is unavailable from configured sources")
	}
	return match, err
}

func within(root, name string) bool {
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	name, err = filepath.EvalSymlinks(name)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, name)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func readLocalSource(ctx context.Context, root, name, id string, limit int64) ([]sourceFile, error) {
	if filepath.Base(name) != id+".jsonl" || !within(root, name) {
		return nil, errors.New("source identity or root mismatch")
	}
	data, info, err := readFile(ctx, name, limit)
	if err != nil {
		return nil, err
	}
	snapshots := map[string]os.FileInfo{name: info}
	out := []sourceFile{{data: data}}
	total := int64(len(data))
	base := strings.TrimSuffix(name, ".jsonl")
	for _, sub := range []string{"tool-results", "subagents"} {
		top := filepath.Join(base, sub)
		err = filepath.WalkDir(top, func(name string, entry fs.DirEntry, walkErr error) error {
			if errors.Is(walkErr, os.ErrNotExist) && name == top {
				return nil
			}
			if walkErr != nil {
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			if !entry.Type().IsRegular() || !within(root, name) {
				return fmt.Errorf("invalid session file: %s", name)
			}
			rel, err := filepath.Rel(base, name)
			if err != nil {
				return err
			}
			data, info, err := readFile(ctx, name, limit-total)
			if err != nil {
				return fmt.Errorf("cannot read %s: %w", rel, err)
			}
			snapshots[name] = info
			total += int64(len(data))
			out = append(out, sourceFile{rel: filepath.ToSlash(rel), data: data})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	for name, before := range snapshots {
		after, err := os.Stat(name)
		if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || before.ModTime() != after.ModTime() {
			return nil, fmt.Errorf("source changed while reading %s; retry when it is idle", name)
		}
	}
	return out, nil
}

func readFile(ctx context.Context, name string, limit int64) ([]byte, os.FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	f, err := os.Open(name)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if before.Size() > limit {
		return nil, nil, errSourceSize
	}
	data, err := readBounded(ctxio.Reader{Context: ctx, Reader: f}, limit)
	if err != nil {
		return nil, nil, err
	}
	return data, before, nil
}

func readBounded(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errSourceSize
	}
	return data, nil
}

func s3SourceMachine(cfg config.Config, agent parser.AgentType, root string) string {
	if machine := parser.S3MachineFromRoot(root, agent); machine != "" {
		return machine
	}
	if machine := cfg.SourceMachines[agent][root]; machine != "" {
		return machine
	}
	return cfg.InstallationID
}

func readS3Source(ctx context.Context, cfg config.Config, machine, uri, id string, limit int64) ([]sourceFile, error) {
	p, ok := parser.S3ProviderFor(parser.AgentClaude)
	if !ok || p.S3SessionID(uri) != id {
		return nil, errors.New("S3 source identity mismatch")
	}
	allowed := false
	for _, root := range cfg.ResolveDirs(parser.AgentClaude) {
		if strings.HasPrefix(root, "s3://") && strings.HasPrefix(uri, strings.TrimSuffix(root, "/")+"/") {
			m := s3SourceMachine(cfg, parser.AgentClaude, root)
			rel := strings.TrimPrefix(uri, strings.TrimSuffix(root, "/")+"/")
			scan := p.S3Scanner()
			if m == machine && path.Clean(rel) == rel && scan.Keep(rel, strings.Split(rel, "/")) {
				allowed = true
				break
			}
		}
	}
	if !allowed {
		return nil, errors.New("S3 source is outside configured roots")
	}
	var total int64
	fetch := func(uri string) ([]byte, error) {
		r, err := parser.FetchS3ObjectContext(ctx, uri)
		if err != nil {
			return nil, err
		}
		defer r.Close()
		data, err := readBounded(ctxio.Reader{Context: ctx, Reader: r}, limit-total)
		total += int64(len(data))
		return data, err
	}
	data, err := fetch(uri)
	if err != nil {
		return nil, err
	}
	out := []sourceFile{{data: data}}
	base := strings.TrimSuffix(uri, ".jsonl")
	for _, sub := range []string{"tool-results", "subagents"} {
		prefix := base + "/" + sub + "/"
		objects, err := parser.ListS3ObjectsContext(ctx, prefix)
		if err != nil {
			return nil, err
		}
		for _, obj := range objects {
			if obj.Size == 0 && strings.HasSuffix(obj.URI, "/") {
				continue
			}
			rel, ok := strings.CutPrefix(obj.URI, base+"/")
			if _, err := filepath.Localize(rel); !ok || err != nil || !strings.HasPrefix(rel, sub+"/") || strings.Contains(rel, `\`) {
				return nil, errors.New("unsafe S3 file path")
			}
			if obj.Size > limit-total {
				return nil, fmt.Errorf("cannot read %s: %w", rel, errSourceSize)
			}
			data, err := fetch(obj.URI)
			if err != nil {
				return nil, err
			}
			out = append(out, sourceFile{rel: rel, data: data})
		}
	}
	return out, nil
}

// readSnapshotSource extracts only the selected transcript and its native companions.
func readSnapshotSource(ctx context.Context, cfg config.Config, owners []db.ArchiveSessionProvenance, id string) (_ []sourceFile, snapshot string, retErr error) {
	tree, primary, snapshot, err := archivesource.MaterializeSnapshot(ctx, cfg.ArchiveSources, owners, "claude", maxSourceBytes)
	if err != nil {
		return nil, "", err
	}
	defer func() { retErr = errors.Join(retErr, tree.Cleanup()) }()
	name, err := tree.EntryPath(primary)
	if err != nil {
		return nil, "", err
	}
	files, err := readLocalSource(ctx, tree.Root(), name, id, maxSourceBytes)
	return files, snapshot, err
}

// useRetained keeps configured snapshots authoritative before considering directories.
func useRetained(ctx context.Context, cfg config.Config, database *db.DB, session *db.Session, owners []db.ArchiveSessionProvenance) (bool, error) {
	if len(owners) != 0 {
		for _, source := range cfg.ArchiveSources {
			if source.Directory == "" && slices.ContainsFunc(owners, func(o db.ArchiveSessionProvenance) bool { return o.Attachment.SnapshotID == source.Snapshot }) {
				return false, nil
			}
		}
		return slices.ContainsFunc(owners, func(o db.ArchiveSessionProvenance) bool {
			return strings.HasPrefix(o.Attachment.SnapshotID, "directory:")
		}), nil
	}
	if session.FilePath != nil && strings.HasPrefix(*session.FilePath, "archive://") {
		return true, nil
	}
	return archivesource.HasRetainedSession(ctx, database, session.ID)
}

func readRetainedSource(ctx context.Context, cfg config.Config, database *db.DB, session *db.Session, owners []db.ArchiveSessionProvenance, native string) (_ []sourceFile, source string, retErr error) {
	stored := ""
	if session.FilePath != nil {
		stored = *session.FilePath
	}
	tree, primary, source, err := archivesource.Retained(ctx, cfg, database, session.ID, session.Machine, native, "claude", stored, owners, maxSourceBytes, nil)
	if err != nil {
		return nil, "", err
	}
	defer func() { retErr = errors.Join(retErr, tree.Cleanup()) }()
	name, err := tree.EntryPath(primary)
	if err != nil {
		return nil, "", err
	}
	files, err := readLocalSource(ctx, tree.Root(), name, native, maxSourceBytes)
	return files, source, err
}

func matchingArchiveOwners(session *db.Session, owners []db.ArchiveSessionProvenance) ([]db.ArchiveSessionProvenance, error) {
	if session.FilePath == nil || len(owners) == 0 {
		return owners, nil
	}
	rest, ok := strings.CutPrefix(*session.FilePath, "archive://")
	if !ok {
		return owners, nil
	}
	root, encoded, found := strings.Cut(rest, "/")
	key, err := url.PathUnescape(encoded)
	if !found || err != nil {
		return nil, errors.New("archive source binding differs")
	}
	matched := slices.DeleteFunc(slices.Clone(owners), func(owner db.ArchiveSessionProvenance) bool {
		return owner.Source.RootID != root || owner.Source.SourceKey != key
	})
	if len(matched) == 0 {
		return nil, errors.New("archive source binding differs")
	}
	return matched, nil
}
