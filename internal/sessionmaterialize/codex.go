package sessionmaterialize

import (
	"context"
	"errors"
	"fmt"
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
	"go.kenn.io/agentsview/internal/remotesync"
)

func materializeCodex(ctx context.Context, cfg config.Config, database *db.DB, session *db.Session, cwd string) (_ Result, retErr error) {
	host, raw := parser.StripHostPrefix(session.ID)
	key, ok := strings.CutPrefix(raw, "codex:")
	thread := parser.CodexThreadIDFromSessionKey(key)
	if !ok || !capture.ValidUUID(thread) {
		return Result{}, errors.New("invalid native Codex session identity")
	}
	if database != nil {
		owners, err := database.ArchiveSessionOwners(ctx, session.ID)
		if err != nil {
			return Result{}, err
		}
		if len(owners) > 0 {
			if owners[0].NativeSessionID != raw || owners[0].Source.DeviceID != session.Machine {
				return Result{}, errors.New("archive session identity differs")
			}
			var pages []codexPage
			snapshot, err := archivesource.ReadCodexSnapshot(ctx, cfg.ArchiveSources, owners, maxSourceBytes, func(names []string, read func(context.Context, string, int64) ([]byte, error)) error {
				var err error
				pages, err = codexLineage(ctx, thread, names, read)
				return err
			})
			if err != nil {
				return Result{}, fmt.Errorf("reading native Codex transcript: %w", err)
			}
			return installCodex(ctx, cwd, "snapshot:"+snapshot, session.ID, pages)
		}
	}
	source, err := storedSource(session, host)
	if err != nil {
		return Result{}, err
	}
	var names []string
	var read func(context.Context, string, int64) ([]byte, error)
	var closeSource func() error
	if host != "" && strings.HasPrefix(source, "s3://") {
		return Result{}, errors.New("native Codex S3 source host differs")
	}
	if strings.HasPrefix(source, "s3://") {
		root := ""
		for _, candidate := range cfg.ResolveDirs(parser.AgentCodex) {
			if !strings.HasPrefix(candidate, "s3://") || !strings.HasPrefix(source, strings.TrimSuffix(candidate, "/")+"/") {
				continue
			}
			machine := parser.S3MachineFromRoot(candidate, parser.AgentCodex)
			if machine == "" {
				machine = cfg.SourceMachines[parser.AgentCodex][candidate]
				if machine == "" {
					machine = cfg.InstallationID
				}
			}
			if machine == session.Machine && len(candidate) > len(root) {
				root = candidate
			}
		}
		p, valid := parser.S3ProviderFor(parser.AgentCodex)
		if root == "" || !valid || p.S3SessionID(source) != raw {
			return Result{}, errors.New("native Codex S3 source identity differs")
		}
		roots := []string{root}
		base := path.Base(root)
		if base == "sessions" || base == "archived_sessions" {
			for _, candidate := range cfg.ResolveDirs(parser.AgentCodex) {
				machine := parser.S3MachineFromRoot(candidate, parser.AgentCodex)
				if machine == "" {
					machine = cfg.SourceMachines[parser.AgentCodex][candidate]
					if machine == "" {
						machine = cfg.InstallationID
					}
				}
				other := path.Base(candidate)
				if candidate != root && strings.HasPrefix(candidate, "s3://") && path.Dir(candidate) == path.Dir(root) && (other == "sessions" || other == "archived_sessions") && machine == session.Machine {
					roots = append(roots, candidate)
				}
			}
		}
		sizes := make(map[string]int64)
		for _, candidate := range roots {
			objects, err := parser.ListS3ObjectsContext(ctx, candidate)
			if err != nil {
				return Result{}, err
			}
			for _, obj := range objects {
				rel, ok := strings.CutPrefix(obj.URI, strings.TrimSuffix(candidate, "/")+"/")
				if !ok || path.Clean(rel) != rel || !p.S3Scanner().Keep(rel, strings.Split(rel, "/")) {
					continue
				}
				if _, exists := sizes[obj.URI]; !exists {
					names = append(names, obj.URI)
				}
				sizes[obj.URI] = obj.Size
			}
		}
		read = func(ctx context.Context, name string, limit int64) (_ []byte, readErr error) {
			if size, ok := sizes[name]; !ok || size < 0 || size > limit {
				return nil, errors.New("native Codex S3 source unavailable or exceeds size limit")
			}
			r, err := parser.FetchS3ObjectContext(ctx, name)
			if err != nil {
				return nil, err
			}
			defer func() { readErr = errors.Join(readErr, r.Close()) }()
			return readBounded(ctxio.Reader{Context: ctx, Reader: r}, limit)
		}
	} else {
		var root string
		var roots []string
		anchor := source
		if host != "" {
			if !slices.ContainsFunc(cfg.RemoteHosts, func(remote config.RemoteHost) bool { return remote.Host == host }) {
				return Result{}, errors.New("session host is not configured")
			}
			remote, ok := strings.CutPrefix(source, host+":")
			if !ok {
				return Result{}, errors.New("mirror source host differs")
			}
			mirror := remotesync.MirrorDir(cfg.DataDir, host)
			name, err := remotesync.SafeRemappedRemotePath(mirror, remote)
			if err != nil {
				return Result{}, errors.New("native Codex mirror source is unavailable")
			}
			anchor = name
			root = filepath.Dir(name)
			if !within(mirror, root) {
				return Result{}, errors.New("native Codex mirror source is unavailable")
			}
			for dir := root; dir != mirror && within(mirror, dir); dir = filepath.Dir(dir) {
				if filepath.Base(dir) == "sessions" || filepath.Base(dir) == "archived_sessions" {
					root = dir
					break
				}
			}
			lock, err := remotesync.AcquireMirrorLock(ctx, mirror)
			if err != nil {
				return Result{}, err
			}
			closeSource = lock.Close
			defer func() {
				if closeSource != nil {
					retErr = errors.Join(retErr, closeSource())
				}
			}()
			roots = []string{root}
			if base := filepath.Base(root); base == "sessions" || base == "archived_sessions" {
				other := "sessions"
				if base == other {
					other = "archived_sessions"
				}
				sibling := filepath.Join(filepath.Dir(root), other)
				if _, err := os.Lstat(sibling); err == nil {
					if !within(mirror, sibling) {
						return Result{}, errors.New("native Codex mirror source is unavailable")
					}
					roots = append(roots, sibling)
				} else if !errors.Is(err, os.ErrNotExist) {
					return Result{}, err
				}
			}
		} else {
			for _, candidate := range configuredRoots(cfg, session, parser.AgentCodex) {
				rel, err := filepath.Rel(candidate, source)
				if err == nil && filepath.IsLocal(rel) && len(candidate) > len(root) {
					root = candidate
				}
			}
			if root == "" {
				return Result{}, errors.New("native Codex source outside configured roots")
			}
			for _, candidate := range configuredRoots(cfg, session, parser.AgentCodex) {
				if candidate == root || ((filepath.Base(root) == "sessions" || filepath.Base(root) == "archived_sessions") && filepath.Dir(candidate) == filepath.Dir(root) && (filepath.Base(candidate) == "sessions" || filepath.Base(candidate) == "archived_sessions")) {
					roots = append(roots, candidate)
				}
			}
		}
		if _, err := os.Lstat(anchor); err == nil {
			if !within(root, anchor) {
				return Result{}, errors.New("native Codex source outside configured root")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return Result{}, err
		}
		provider, valid := parser.NewProvider(parser.AgentCodex, parser.ProviderConfig{Roots: roots})
		discovery, streaming := provider.(parser.StreamingDiscoverer)
		if !valid || !streaming {
			return Result{}, errors.New("native Codex source provider unavailable")
		}
		ref, found, err := provider.FindSource(ctx, parser.FindSourceRequest{StoredFilePath: anchor})
		if err != nil {
			return Result{}, err
		}
		if found && parser.CodexThreadIDFromSessionKey(parser.CodexSessionUUIDFromFilename(filepath.Base(ref.DisplayPath))) != thread {
			return Result{}, errors.New("native Codex source identity differs")
		}
		if err := discovery.DiscoverEach(ctx, func(ref parser.SourceRef) error {
			names = append(names, filepath.ToSlash(ref.DisplayPath))
			return nil
		}); err != nil {
			return Result{}, err
		}
		read = func(ctx context.Context, name string, limit int64) ([]byte, error) {
			name = filepath.FromSlash(name)
			for _, candidate := range roots {
				if within(candidate, name) {
					return readStableFile(ctx, candidate, name, limit)
				}
			}
			return nil, errors.New("native Codex source outside configured roots")
		}
	}
	pages, err := codexLineage(ctx, thread, names, read)
	if closeSource != nil {
		err = errors.Join(err, closeSource())
		closeSource = nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("reading native Codex transcript: %w", err)
	}
	return installCodex(ctx, cwd, source, session.ID, pages)
}

func installCodex(ctx context.Context, cwd, source, requestedID string, pages []codexPage) (Result, error) {
	root, err := capture.ProducerRoot(capture.ProviderCodex, "")
	if err != nil {
		return Result{}, err
	}
	files := make([]installFile, 0, len(pages))
	paths := make([]string, 0, len(pages))
	for _, page := range pages {
		date, err := codexDate(page.name)
		if err != nil {
			return Result{}, err
		}
		dest := filepath.Join(root, filepath.FromSlash(date), path.Base(page.name))
		files = append(files, installFile{dest, page.data})
		paths = append(paths, dest)
	}
	changed, err := publishFiles(ctx, files)
	if err != nil {
		return Result{}, err
	}
	status := "unchanged"
	if changed {
		status = "installed"
	}
	return Result{Source: source, Paths: paths, Cwd: cwd, Status: status, RequestedID: requestedID, InstalledPage: path.Base(pages[len(pages)-1].name)}, nil
}
