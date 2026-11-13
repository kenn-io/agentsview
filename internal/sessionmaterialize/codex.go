package sessionmaterialize

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
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
	"go.kenn.io/agentsview/internal/rawpath"
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
			snapshot, err := archivesource.ReadCodexSnapshot(ctx, cfg.ArchiveSources, owners, maxSourceBytes, func(root string, names []string, read func(context.Context, string, int64) ([]byte, error)) error {
				var err error
				archived := make(map[string]bool)
				for _, name := range names {
					archived[name] = codexArchived(root, name)
				}
				pages, err = codexLineage(ctx, thread, names, archived, read)
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
	archived := make(map[string]bool)
	var read func(context.Context, string, int64) ([]byte, error)
	var closeSource func() error
	if host != "" && host != session.Machine && strings.HasPrefix(source, "s3://") {
		return Result{}, errors.New("native Codex S3 source host differs")
	}
	if strings.HasPrefix(source, "s3://") {
		root := ""
		var matchingRoots []string
		for _, candidate := range cfg.ResolveDirs(parser.AgentCodex) {
			if !strings.HasPrefix(candidate, "s3://") {
				continue
			}
			machine := s3SourceMachine(cfg, parser.AgentCodex, candidate)
			if machine == session.Machine {
				matchingRoots = append(matchingRoots, candidate)
				if strings.HasPrefix(source, strings.TrimSuffix(candidate, "/")+"/") && len(candidate) > len(root) {
					root = candidate
				}
			}
		}
		p, valid := parser.S3ProviderFor(parser.AgentCodex)
		if root == "" || !valid || p.S3SessionID(source) != raw {
			return Result{}, errors.New("native Codex S3 source identity differs")
		}
		roots := []string{root}
		base := path.Base(root)
		if base == "sessions" || base == "archived_sessions" {
			for _, candidate := range matchingRoots {
				other := path.Base(candidate)
				if candidate != root && path.Dir(candidate) == path.Dir(root) && (other == "sessions" || other == "archived_sessions") {
					roots = append(roots, candidate)
				}
			}
		}
		sizes := make(map[string]int64)
		relative := make(map[string]string)
		for _, candidate := range roots {
			objects, err := parser.ListS3ObjectsContext(ctx, candidate)
			if err != nil {
				return Result{}, err
			}
			for _, obj := range objects {
				rel, ok := strings.CutPrefix(obj.URI, strings.TrimSuffix(candidate, "/")+"/")
				if !ok || !p.S3Scanner().Keep(rel, strings.Split(rel, "/")) {
					continue
				}
				if _, exists := sizes[obj.URI]; !exists {
					names = append(names, obj.URI)
				}
				archived[obj.URI] = codexArchived(candidate, rel)
				sizes[obj.URI] = obj.Size
				relative[obj.URI] = rel
			}
		}
		read = func(ctx context.Context, name string, limit int64) (_ []byte, readErr error) {
			if size, ok := sizes[name]; !ok || size < 0 || size > limit {
				return nil, errors.New("native Codex S3 source unavailable or exceeds size limit")
			}
			if err := rawpath.Validate(relative[name], rawpath.DefaultMaxBytes); err != nil {
				return nil, fmt.Errorf("invalid selected Codex S3 path: %w", err)
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
		archiveRoots := make(map[string]bool)
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
				other := map[string]string{"sessions": "archived_sessions", "archived_sessions": "sessions"}[base]
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
			if source == "" {
				return Result{}, errors.New("native Codex resume requires an original transcript")
			}
			anchor, err = pathutil.ResolveAbsolute(source)
			if err != nil {
				return Result{}, err
			}
			candidates := configuredRoots(cfg, session, parser.AgentCodex)
			if info, statErr := os.Stat(anchor); statErr == nil && info.Mode().IsRegular() {
				candidates = localRoots(cfg, parser.AgentCodex)
			} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
				return Result{}, statErr
			}
			for _, candidate := range candidates {
				resolved, err := pathutil.ResolveAbsolute(candidate)
				if err != nil {
					return Result{}, err
				}
				rel, err := filepath.Rel(resolved, anchor)
				if err == nil && filepath.IsLocal(rel) && len(candidate) > len(root) {
					root = candidate
				}
			}
			if root == "" {
				return Result{}, errors.New("native Codex source outside configured roots")
			}
			roots, archiveRoots = codexConfiguredGroup(cfg, candidates, root)
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
			name := filepath.ToSlash(ref.DisplayPath)
			names = append(names, name)
			var matched string
			for _, candidate := range roots {
				rel, err := filepath.Rel(candidate, ref.DisplayPath)
				if err == nil && filepath.IsLocal(rel) && len(candidate) > len(matched) {
					matched = candidate
					if isArchive, known := archiveRoots[candidate]; known {
						archived[name] = isArchive
					} else {
						archived[name] = codexArchived(candidate, filepath.ToSlash(rel))
					}
				}
			}
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
	pages, err := codexLineage(ctx, thread, names, archived, read)
	if closeSource != nil {
		err = errors.Join(err, closeSource())
		closeSource = nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("reading native Codex transcript: %w", err)
	}
	return installCodex(ctx, cwd, source, session.ID, pages)
}

// codexConfiguredGroup joins only configured native roots proven to share a home.
func codexConfiguredGroup(cfg config.Config, candidates []string, selected string) ([]string, map[string]bool) {
	roots := []string{selected}
	archived := make(map[string]bool)
	metadata := cfg.ProviderMetadata[parser.AgentCodex]
	for _, home := range metadata[selected] {
		live, liveErr := pathutil.ResolveAbsolute(filepath.Join(home, "sessions"))
		archive, archiveErr := pathutil.ResolveAbsolute(filepath.Join(home, "archived_sessions"))
		selectedPath, selectedErr := pathutil.ResolveAbsolute(selected)
		if liveErr != nil || archiveErr != nil || selectedErr != nil || (selectedPath != live && selectedPath != archive) {
			continue
		}
		for _, candidate := range candidates {
			resolved, err := pathutil.ResolveAbsolute(candidate)
			if err != nil || (resolved != live && resolved != archive) {
				continue
			}
			archived[candidate] = resolved == archive
			if !slices.Contains(roots, candidate) {
				roots = append(roots, candidate)
			}
		}
	}
	if len(metadata[selected]) == 0 {
		for _, candidate := range candidates {
			if (filepath.Base(selected) == "sessions" || filepath.Base(selected) == "archived_sessions") && filepath.Dir(candidate) == filepath.Dir(selected) && (filepath.Base(candidate) == "sessions" || filepath.Base(candidate) == "archived_sessions") && !slices.Contains(roots, candidate) {
				roots = append(roots, candidate)
			}
		}
	}
	return roots, archived
}

// codexArchived uses only the selected root and its relative coordinates.
func codexArchived(root, relative string) bool {
	root = strings.ReplaceAll(root, `\`, "/")
	return path.Base(root) == "archived_sessions" || strings.HasPrefix(relative, "archived_sessions/")
}

func installCodex(ctx context.Context, cwd, source, requestedID string, pages []codexPage) (Result, error) {
	if pages == nil {
		return Result{}, errors.New("native Codex lineage is unavailable")
	}
	root, err := capture.ProducerRoot(capture.ProviderCodex, "")
	if err != nil {
		return Result{}, err
	}
	files := make([]installFile, 0, len(pages))
	paths := make([]string, 0, len(pages))
	for _, page := range pages {
		date, _, err := codexDate(page.name)
		if err != nil {
			return Result{}, err
		}
		base := path.Base(page.name)
		if err := rawpath.Validate(base, rawpath.DefaultMaxBytes); err != nil {
			return Result{}, fmt.Errorf("invalid native Codex destination: %w", err)
		}
		rel := path.Join(date, base)
		if err := rawpath.Validate(rel, rawpath.DefaultMaxBytes); err != nil {
			return Result{}, fmt.Errorf("invalid native Codex destination: %w", err)
		}
		dest := filepath.Join(root, filepath.FromSlash(rel))
		if check, err := filepath.Rel(root, dest); err != nil || !filepath.IsLocal(check) {
			return Result{}, errors.New("native Codex destination outside root")
		}
		files = append(files, installFile{dest, page.data})
		paths = append(paths, dest)
	}
	// Native lookup can find the same rollout in either tree, under another
	// date or compression spelling. Refuse different raw bytes before writing.
	for _, nativeRoot := range []string{root, filepath.Join(filepath.Dir(root), "archived_sessions")} {
		if _, err := os.Lstat(nativeRoot); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return Result{}, err
		}
		resolvedRoot, err := filepath.EvalSymlinks(nativeRoot)
		if err != nil {
			return Result{}, err
		}
		info, err := os.Stat(resolvedRoot)
		if err != nil {
			return Result{}, err
		}
		if !info.IsDir() {
			return Result{}, fmt.Errorf("invalid native Codex root: %s", nativeRoot)
		}
		err = filepath.WalkDir(resolvedRoot, func(name string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			key := parser.CodexSessionUUIDFromFilename(entry.Name())
			thread := parser.CodexThreadIDFromSessionKey(key)
			rollout := strings.TrimPrefix(key, thread+"_")
			for _, page := range pages {
				if key == "" || rollout != page.rollout {
					continue
				}
				if !entry.Type().IsRegular() {
					return fmt.Errorf("invalid native Codex destination: %s", name)
				}
				data, err := readStableFile(ctx, resolvedRoot, name, maxSourceBytes)
				if err != nil {
					return err
				}
				if !bytes.Equal(data, page.data) {
					return fmt.Errorf("native Codex destination differs: %s", name)
				}
			}
			return nil
		})
		if err != nil {
			return Result{}, err
		}
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
