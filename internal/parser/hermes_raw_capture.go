package parser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// A raw source is one home, independent of the database's current presence.
// The configured owner can instead be its sessions directory or profiles parent.
type hermesRawSource struct{ ConfiguredRoot, Home string }

func withHermesRawCapture(agent AgentType, caps Capabilities) Capabilities {
	if agent == AgentHermes {
		caps.RawCapture = RawCaptureCapabilities{Support: CapabilitySupported, Shape: RawCaptureShapeSQLiteArchive, Append: RawCaptureAppendReplaceOnly, Snapshot: RawCaptureSnapshotOnlineBackup}
	}
	return caps
}

func (p *hermesProvider) ConfiguredRoots() []string {
	roots := p.ProviderBase.ConfiguredRoots()
	if p.spec.agent != AgentHermes {
		return roots
	}
	for i, root := range roots {
		root = absoluteHermesPath(root)
		if filepath.Base(root) == "state.db" {
			root = filepath.Dir(root)
		}
		roots[i] = root
	}
	return roots
}

func (p *hermesProvider) DiscoverRawCaptureSourcesEach(ctx context.Context, yield func(SourceRef) error) (bool, error) {
	if p.spec.agent != AgentHermes {
		return false, UnsupportedProviderFeatureError{Provider: p.spec.agent, Feature: ProviderFeatureRawCapture}
	}
	ctx = withRawCaptureStreamingTraversal(ctx)
	seen := make(map[string]bool)
	visit := func(raw hermesRawSource) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := ReportRawCaptureDiscoveryProgress(ctx); err != nil {
			return err
		}
		home, err := validateRawCaptureRoot("Hermes home", raw.Home)
		if err != nil {
			return err
		}
		raw.Home = home
		if seen[home] {
			return nil
		}
		seen[home] = true
		plan, err := hermesRawCapturePlan(ctx, raw)
		if err != nil {
			return err
		}
		if len(plan.Entries) == 0 {
			return nil
		}
		if err := yield(p.hermesRawSourceRef(raw)); err != nil {
			return discoveryYieldError{cause: err}
		}
		return nil
	}
	var scanErr error
	for _, configured := range p.ConfiguredRoots() {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if err := ReportRawCaptureDiscoveryProgress(ctx); err != nil {
			return false, err
		}
		profiles := p.sources.isProfilesContainer(configured)
		sessions := filepath.Base(configured) == "sessions"
		if _, err := os.Stat(configured); err != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			if rootErr, isIncomplete := rawCaptureIncompleteRootError(AgentHermes, configured, err); isIncomplete {
				scanErr = errors.Join(scanErr, rootErr)
				continue
			}
			return false, errors.Join(scanErr, err)
		}
		root, err := validateRawCaptureRoot("configured", configured)
		if err == nil {
			if profiles {
				err = streamDirectoryEntries(ctx, root, func(entry os.DirEntry) error {
					if entry.Type()&os.ModeSymlink != 0 {
						return invalidRawCapturePlan("Hermes profile is a symlink")
					}
					if !entry.IsDir() {
						return nil
					}
					return visit(hermesRawSource{ConfiguredRoot: root, Home: filepath.Join(root, entry.Name())})
				})
			} else {
				home := root
				if sessions {
					home = hermesRawEventPath(filepath.Dir(root))
				}
				err = visit(hermesRawSource{ConfiguredRoot: root, Home: home})
			}
		}
		if err != nil {
			if cause, ok := discoveryYieldCause(err); ok {
				return false, cause
			}
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			scanErr = errors.Join(scanErr, err)
		}
	}
	return scanErr == nil, scanErr
}

func (p *hermesProvider) hermesRawSourceRef(raw hermesRawSource) SourceRef {
	anchor := filepath.Join(raw.Home, "state.db")
	return SourceRef{Provider: AgentHermes, ConfiguredRoot: raw.ConfiguredRoot, Key: anchor, DisplayPath: anchor, FingerprintKey: anchor, Opaque: raw}
}

func (p *hermesProvider) PlanRawCapture(ctx context.Context, source SourceRef) (RawCapturePlan, error) {
	raw, ok := source.Opaque.(hermesRawSource)
	if !ok || p.spec.agent != AgentHermes || source.Provider != AgentHermes || source.Key != filepath.Join(raw.Home, "state.db") {
		return RawCapturePlan{}, invalidRawCapturePlan("Hermes archive source unavailable")
	}
	return hermesRawCapturePlan(ctx, raw)
}

func hermesRawCapturePlan(ctx context.Context, raw hermesRawSource) (RawCapturePlan, error) {
	if err := ctx.Err(); err != nil {
		return RawCapturePlan{}, err
	}
	plan := RawCapturePlan{ConfiguredRoot: raw.ConfiguredRoot, CaptureRoot: raw.Home, SourceKey: filepath.Join(raw.Home, "state.db")}
	database := filepath.Join(raw.Home, "state.db")
	if info, err := os.Lstat(database); err == nil {
		if !info.Mode().IsRegular() {
			return RawCapturePlan{}, invalidRawCapturePlan("Hermes database is not a regular file")
		}
		plan.SnapshotPath = "state.db"
		plan.Entries = append(plan.Entries, RawCaptureEntry{Path: "state.db", LocalPath: database})
	} else if !errors.Is(err, os.ErrNotExist) {
		return RawCapturePlan{}, err
	}
	transcripts := filepath.Join(raw.Home, "sessions")
	if info, err := os.Lstat(transcripts); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return RawCapturePlan{}, invalidRawCapturePlan("Hermes sessions is not a directory")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		// Match the legacy parser: a DB home uses sessions/, while a file-only
		// configured root can directly contain the supported transcript files.
		if plan.SnapshotPath != "" {
			return plan, nil
		}
		transcripts = raw.Home
	} else {
		return RawCapturePlan{}, err
	}
	err := streamDirectoryEntries(ctx, transcripts, func(entry os.DirEntry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		local := filepath.Join(transcripts, entry.Name())
		if !hermesPathInTranscriptDir(transcripts, local) {
			return nil
		}
		info, err := os.Lstat(local)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return invalidRawCapturePlan("Hermes transcript is not a regular file")
		}
		plan.Entries = append(plan.Entries, RawCaptureEntry{Path: "sessions/" + entry.Name(), LocalPath: local})
		return nil
	})
	return plan, err
}

func (p *hermesProvider) RawCaptureSourcesForChangedPath(ctx context.Context, req ChangedPathRequest) ([]SourceRef, error) {
	if p.spec.agent != AgentHermes {
		return nil, UnsupportedProviderFeatureError{Provider: p.spec.agent, Feature: ProviderFeatureRawCapture}
	}
	ctx = withRawCaptureStreamingTraversal(ctx)
	var sources []SourceRef
	changed := hermesRawEventPath(req.Path)
	seen := make(map[string]bool)
	visit := func(raw hermesRawSource) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if req.WatchRoot != "" {
			watch := hermesRawEventPath(req.WatchRoot)
			// A sessions watch also observes its sibling DB through the home root.
			if !hermesPathWithinOrSame(raw.ConfiguredRoot, watch) && !hermesPathWithinOrSame(raw.Home, watch) && !hermesPathWithinOrSame(watch, raw.Home) {
				return nil
			}
		}
		info, err := os.Lstat(raw.Home)
		if errors.Is(err, os.ErrNotExist) {
			return nil // The periodic audit handles vanished homes.
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return invalidRawCapturePlan("Hermes profile is a symlink")
		}
		if !info.IsDir() {
			return nil
		}
		home, err := validateRawCaptureRoot("Hermes home", raw.Home)
		if err != nil {
			return err
		}
		raw.Home = home
		if seen[home] {
			return nil
		}
		seen[home] = true
		plan, err := hermesRawCapturePlan(ctx, raw)
		if err != nil {
			return err
		}
		if len(plan.Entries) != 0 {
			sources = append(sources, p.hermesRawSourceRef(raw))
		}
		return nil
	}
	for _, configured := range p.ConfiguredRoots() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		root := hermesRawEventPath(configured)
		if p.sources.isProfilesContainer(configured) {
			if !hermesPathWithinOrSame(changed, root) {
				continue
			}
			if samePath(root, changed) {
				err := streamDirectoryEntries(ctx, root, func(entry os.DirEntry) error {
					if !entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
						return nil
					}
					return visit(hermesRawSource{ConfiguredRoot: root, Home: filepath.Join(root, entry.Name())})
				})
				if err != nil {
					return nil, err
				}
				continue
			}
			relative, err := filepath.Rel(root, changed)
			if err != nil {
				return nil, err
			}
			home := filepath.Join(root, strings.Split(relative, string(filepath.Separator))[0])
			if hermesRawChangeAffectsHome(changed, home, root) {
				if err := visit(hermesRawSource{ConfiguredRoot: root, Home: home}); err != nil {
					return nil, err
				}
			}
			continue
		}
		home := root
		if filepath.Base(configured) == "sessions" {
			home = hermesRawEventPath(filepath.Dir(root))
		}
		if hermesRawChangeAffectsHome(changed, home, root) {
			if err := visit(hermesRawSource{ConfiguredRoot: root, Home: home}); err != nil {
				return nil, err
			}
		}
	}
	return sources, nil
}

func hermesRawChangeAffectsHome(changed, home, configured string) bool {
	if samePath(changed, home) || samePath(changed, configured) || samePath(changed, filepath.Join(home, "sessions")) {
		return true
	}
	if !hermesPathWithinOrSame(changed, home) {
		return false
	}
	state := filepath.Join(home, "state.db")
	return hermesStatePathAffectsArchive(changed, state) || samePath(changed, state+"-shm") || samePath(changed, state+"-journal") || hermesPathInTranscriptDir(hermesTranscriptRoot(home), changed)
}

func (p *hermesProvider) RawSnapshotSessions(ctx context.Context, source SourceRef) ([]SourceRef, error) {
	raw, ok := source.Opaque.(hermesRawSource)
	if !ok || p.spec.agent != AgentHermes {
		return nil, fmt.Errorf("%w: Hermes raw source unavailable", ErrInvalidRawCapturePlan)
	}
	return newHermesSourceSet(AgentHermes, []string{raw.Home}).Discover(ctx)
}

var (
	_ StreamingRawCaptureSourceProvider = (*hermesProvider)(nil)
	_ RawCaptureProvider                = (*hermesProvider)(nil)
	_ RawSnapshotSessionProvider        = (*hermesProvider)(nil)
)

// Resolve the existing ancestor too, so remove/WAL events keep matching after
// the leaf disappears, including through a configured symlink spelling.
func hermesRawEventPath(path string) string {
	path = absoluteHermesPath(path)
	current := path
	for {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			relative, err := filepath.Rel(current, path)
			if err == nil {
				return filepath.Join(resolved, relative)
			}
			return path
		}
		parent := filepath.Dir(current)
		if parent == current {
			return path
		}
		current = parent
	}
}

// Reproduce the client's ordinary home spelling without worker OS semantics.
func hermesRawClientHome(sourcePath string) string {
	windows := strings.Contains(sourcePath, `\`)
	sourcePath = strings.ReplaceAll(sourcePath, `\`, "/")
	home := path.Dir(sourcePath)
	if path.Base(sourcePath) != "state.db" && path.Base(home) == "sessions" {
		home = path.Dir(home)
	}
	if strings.HasPrefix(sourcePath, "//") {
		home = "/" + home // path.Dir collapses the UNC prefix.
	}
	if windows {
		home = strings.ReplaceAll(home, "/", `\`)
	}
	return home
}
