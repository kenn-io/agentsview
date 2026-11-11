package sessionmaterialize

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"go.kenn.io/agentsview/internal/capture"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/remotesync"
)

const maxSourceBytes = 512 << 20

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
}

// Materialize installs a Claude transcript and its own native dependencies.
func Materialize(ctx context.Context, cfg config.Config, session *db.Session, cwd string) (Result, error) {
	if session == nil || session.Agent != "claude" {
		return Result{}, errors.New("materialization is not yet supported for this provider")
	}
	cwd, err := filepath.Abs(cwd)
	if err != nil {
		return Result{}, err
	}
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		return Result{}, fmt.Errorf("cwd must be an existing directory: %s", cwd)
	}
	host, id := parser.StripHostPrefix(session.ID)
	if !capture.ValidUUID(id) {
		return Result{}, errors.New("invalid Claude session ID")
	}
	if session.FilePath == nil || *session.FilePath == "" {
		return Result{}, errors.New("native transcript is unavailable from configured sources")
	}
	source := *session.FilePath
	var files []sourceFile
	if strings.HasPrefix(source, "s3://") {
		if host != "" && host != session.Machine {
			return Result{}, errors.New("S3 source host does not match session identity")
		}
		files, err = readS3Source(ctx, cfg, session.Machine, source, id)
	} else if host != "" {
		allowed := false
		for _, remote := range cfg.RemoteHosts {
			if remote.Host == host {
				allowed = true
				break
			}
		}
		if !allowed {
			return Result{}, errors.New("session host is not configured")
		}
		root := remotesync.MirrorDir(cfg.DataDir, host)
		var name string
		name, err = remotesync.SafeRemappedRemotePath(root, source)
		if err == nil {
			lock, lockErr := remotesync.AcquireMirrorLock(ctx, root)
			if lockErr != nil {
				return Result{}, lockErr
			}
			files, err = readLocalSource(ctx, root, name, id)
			err = errors.Join(err, lock.Close())
		}
	} else {
		files, err = readConfiguredLocal(ctx, cfg, session, source, id)
	}
	if err != nil {
		return Result{}, fmt.Errorf("reading native transcript: %w", err)
	}
	if len(files) == 0 {
		return Result{}, errors.New("native transcript is unavailable from configured sources")
	}
	primary, err := clearClaudeWorktree(files[0].data, id)
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

func readConfiguredLocal(ctx context.Context, cfg config.Config, session *db.Session, stored, id string) ([]sourceFile, error) {
	var roots []string
	for _, root := range cfg.ResolveDirs(parser.AgentClaude) {
		if strings.HasPrefix(root, "s3://") {
			continue
		}
		machine := cfg.SourceMachines[parser.AgentClaude][root]
		if machine == "" {
			machine = cfg.InstallationID
		}
		if machine == session.Machine || (session.Machine == cfg.LocalMachineName && machine == cfg.InstallationID) {
			roots = append(roots, root)
		}
	}
	provider, ok := parser.NewProvider(parser.AgentClaude, parser.ProviderConfig{Roots: roots, Machine: cfg.InstallationID, SourceMachines: cfg.SourceMachines[parser.AgentClaude]})
	if !ok {
		return nil, errors.New("Claude source provider unavailable")
	}
	ref, found, err := provider.FindSource(ctx, parser.FindSourceRequest{RawSessionID: id, FullSessionID: session.ID, StoredFilePath: stored, PreferStoredSource: true})
	if err != nil {
		return nil, err
	}
	if !found || ref.Provider != parser.AgentClaude {
		return nil, errors.New("native transcript is unavailable from configured sources")
	}
	if filepath.Clean(ref.DisplayPath) != filepath.Clean(stored) {
		return nil, errors.New("source lookup did not resolve the archived transcript")
	}
	for _, root := range roots {
		if within(root, ref.DisplayPath) {
			return readLocalSource(ctx, root, ref.DisplayPath, id)
		}
	}
	return nil, errors.New("source is outside configured roots")
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

func readLocalSource(ctx context.Context, root, name, id string) ([]sourceFile, error) {
	if filepath.Base(name) != id+".jsonl" || !within(root, name) {
		return nil, errors.New("source identity or root mismatch")
	}
	data, info, err := readFile(ctx, name)
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
				if !within(root, name) {
					return errors.New("sidecar escapes source root")
				}
				return nil
			}
			if !entry.Type().IsRegular() || !within(root, name) {
				return errors.New("invalid sidecar")
			}
			rel, err := filepath.Rel(base, name)
			if err != nil {
				return err
			}
			data, info, err := readFile(ctx, name)
			if err != nil {
				return err
			}
			snapshots[name] = info
			total += int64(len(data))
			if total > maxSourceBytes {
				return errors.New("native source exceeds size limit")
			}
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

func readFile(ctx context.Context, name string) ([]byte, os.FileInfo, error) {
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
	data, err := readBounded(contextReader{ctx: ctx, r: f}, maxSourceBytes)
	if err != nil {
		return nil, nil, err
	}
	after, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	current, err := os.Stat(name)
	if err != nil || !os.SameFile(before, current) || !os.SameFile(before, after) || before.Size() != after.Size() || before.ModTime() != after.ModTime() || after.Size() != current.Size() || after.ModTime() != current.ModTime() {
		return nil, nil, fmt.Errorf("source changed while reading %s; retry when it is idle", name)
	}
	return data, after, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
func readBounded(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("native source exceeds size limit")
	}
	return data, nil
}

func readS3Source(ctx context.Context, cfg config.Config, machine, uri, id string) ([]sourceFile, error) {
	p, ok := parser.S3ProviderFor(parser.AgentClaude)
	if !ok || p.S3SessionID(uri) != id {
		return nil, errors.New("S3 source identity mismatch")
	}
	allowed := false
	for _, root := range cfg.ResolveDirs(parser.AgentClaude) {
		if strings.HasPrefix(root, "s3://") && strings.HasPrefix(uri, strings.TrimSuffix(root, "/")+"/") {
			m := parser.S3MachineFromRoot(root, parser.AgentClaude)
			if m == "" {
				m = cfg.SourceMachines[parser.AgentClaude][root]
				if m == "" {
					m = cfg.InstallationID
				}
			}
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
	if _, err := p.S3TempRelPath(uri); err != nil {
		return nil, err
	}
	var total int64
	fetch := func(uri string) ([]byte, error) {
		r, err := parser.FetchS3ObjectContext(ctx, uri)
		if err != nil {
			return nil, err
		}
		defer r.Close()
		data, err := readBounded(contextReader{ctx: ctx, r: r}, maxSourceBytes-total)
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
		prefix := base + "/" + sub
		objects, err := parser.ListS3ObjectsContext(ctx, prefix)
		if err != nil {
			return nil, err
		}
		for _, obj := range objects {
			rel, ok := strings.CutPrefix(obj.URI, base+"/")
			if ok && obj.Size == 0 && strings.HasPrefix(rel, sub+"/") && strings.HasSuffix(rel, "/") {
				trimmed := strings.TrimSuffix(rel, "/")
				if !strings.Contains(trimmed, `\`) && path.Clean(trimmed) == trimmed {
					continue
				}
			}
			if !ok || !strings.HasPrefix(rel, sub+"/") || path.Clean(rel) != rel || strings.Contains(rel, `\`) {
				return nil, errors.New("unsafe S3 sidecar path")
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
