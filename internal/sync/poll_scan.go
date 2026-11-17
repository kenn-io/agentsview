package sync

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"hash/maphash"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	gosync "sync"

	"go.kenn.io/agentsview/internal/parser"
)

const (
	// pollScanMaxRows bounds retained snapshot rows across every polled root,
	// about 16 MiB. A root that does not fit falls back to reconciliation.
	pollScanMaxRows = 1 << 20
	// pollScanMaxChangedPaths caps changed-path work for one root. A larger
	// change set is cheaper to reconcile than to sync path by path.
	pollScanMaxChangedPaths = 1024
	pollScanPageSize        = 256
)

// errPollScanIneligible means a metadata scan cannot stand in for
// reconciliation of a root on this pass.
var errPollScanIneligible = errors.New("poll scan cannot compare this root")

type pollScanKey struct {
	agent parser.AgentType
	root  string
}

// pollScanRow is the whole per-file cost of a snapshot: hashes of the path and
// of its metadata signature. Hashes never leave the process, so a per-process
// seed suffices. A path collision makes two files share a row, so a change to
// either is still seen; a signature collision hides one change with odds of
// 2^-64.
type pollScanRow struct{ path, signature uint64 }

// pollScanSignature changes on every write, replacement, or metadata change
// that a parser could observe. Change time covers an equal-size rewrite whose
// modification time was restored.
type pollScanSignature struct {
	size, mtime, ctime, inode, device int64
}

type pollScanState struct {
	mu        gosync.Mutex
	seed      maphash.Seed
	seeded    bool
	snapshots map[pollScanKey][]pollScanRow
	rows      int // Rows across committed snapshots.
	maxRows   int // Zero means pollScanMaxRows; tests lower it.
}

// PollProviderRootsGrouped is the scheduled polling pass. For providers that
// declare PollMetadataScan, it compares each root's file metadata with the
// previous pass. Changed files go through the changed-path sync that native
// watcher events use. A root without a previous snapshot, a root with a
// vanished file, and any root the scan cannot fully compare go through
// ReconcileProviderRootsGrouped, as does every root while subagent linking is
// pending. A disappeared file is left to that provider-scoped reconciliation,
// so the caller's availability checks decide the deletion. authoritative
// reconciles every root, still recording snapshots for the next pass.
//
// A snapshot is taken before the archive work it describes and kept only after
// that work succeeds, so it never claims a change the archive has not applied.
// Snapshots for roots absent from groups are released.
func (e *Engine) PollProviderRootsGrouped(
	ctx context.Context, groups []ProviderRootsGroup, authoritative bool,
) error {
	state := &e.pollScans
	state.mu.Lock()
	defer state.mu.Unlock()
	// Failed subagent linking is retried by a reconciliation epilogue, which an
	// unchanged pass would otherwise skip.
	authoritative = authoritative || e.PendingSubagentLinks()
	if !state.seeded {
		state.seed = maphash.MakeSeed()
		state.seeded = true
	}
	live := make(map[pollScanKey]struct{})
	for _, group := range groups {
		for _, root := range group.Roots {
			live[pollScanKey{group.Agent, filepath.Clean(root)}] = struct{}{}
		}
	}
	for key := range state.snapshots {
		if _, ok := live[key]; !ok {
			state.forget(key)
		}
	}

	var (
		reconcile  []ProviderRootsGroup
		afterRecon = make(map[pollScanKey][]pollScanRow)
		afterSync  = make(map[pollScanKey][]pollScanRow)
		changed    = make(map[pollScanKey][]string)
		// staged counts rows that pending snapshots add beyond the committed
		// ones they replace, so every root in a pass shares one row budget. A
		// shrinking snapshot frees nothing until its work succeeds.
		staged  int
		maxRows = cmp.Or(state.maxRows, pollScanMaxRows)
	)
	for _, group := range groups {
		var full []string
		for _, root := range group.Roots {
			key := pollScanKey{group.Agent, filepath.Clean(root)}
			plan, ok := e.pollScanPlan(ctx, group.Agent, key.root)
			if !ok {
				state.forget(key)
				full = append(full, root)
				continue
			}
			previous := state.snapshots[key]
			budget := maxRows - state.rows - staged + len(previous)
			result, err := scanPollRoots(ctx, state.seed, plan, previous, budget)
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			switch {
			case err != nil:
				if !errors.Is(err, errPollScanIneligible) {
					log.Printf("poll scan %s: reconciling instead: %v", group.Agent, err)
				}
				state.forget(key)
				full = append(full, root)
			case authoritative || previous == nil || result.vanished:
				afterRecon[key] = result.rows
				staged += max(0, len(result.rows)-len(previous))
				full = append(full, root)
			case len(result.changed) > 0:
				afterSync[key] = result.rows
				staged += max(0, len(result.rows)-len(previous))
				changed[key] = result.changed
			}
		}
		if len(full) > 0 {
			reconcile = append(reconcile, ProviderRootsGroup{Agent: group.Agent, Roots: full})
		}
	}

	// A changed-path sync marks a path missing when it no longer exists.
	// Send a root whose changed file has already gone to reconciliation, so
	// only the caller's availability checks decide that deletion.
	var changedPaths []string
	for key, paths := range changed {
		if pollScanPathsPresent(paths) {
			changedPaths = append(changedPaths, paths...)
			continue
		}
		afterRecon[key] = afterSync[key]
		delete(afterSync, key)
		reconcile = append(reconcile, ProviderRootsGroup{Agent: key.agent, Roots: []string{key.root}})
	}
	slices.Sort(changedPaths)

	var errs []error
	if len(changedPaths) > 0 {
		if err := e.syncPollScanPaths(ctx, changedPaths); err != nil {
			errs = append(errs, err)
		} else {
			state.keep(afterSync)
		}
	}
	if len(reconcile) > 0 {
		if err := e.ReconcileProviderRootsGrouped(ctx, reconcile); err != nil {
			errs = append(errs, err)
		} else {
			state.keep(afterRecon)
		}
	}
	return errors.Join(errs...)
}

func (s *pollScanState) forget(key pollScanKey) {
	s.rows -= len(s.snapshots[key])
	delete(s.snapshots, key)
}

func (s *pollScanState) keep(snapshots map[pollScanKey][]pollScanRow) {
	if s.snapshots == nil {
		s.snapshots = make(map[pollScanKey][]pollScanRow)
	}
	for key, rows := range snapshots {
		s.rows += len(rows) - len(s.snapshots[key])
		s.snapshots[key] = rows
	}
}

func pollScanPathsPresent(paths []string) bool {
	for _, path := range paths {
		if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
			return false
		}
	}
	return true
}

// syncPollScanPaths attempts every page, so one failing source cannot keep
// later changes out of the archive.
func (e *Engine) syncPollScanPaths(ctx context.Context, paths []string) error {
	var errs []error
	for page := range slices.Chunk(paths, pollScanPageSize) {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		if err := e.SyncPathsContext(ctx, page); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("sync changed polled paths: %w", err)
	}
	return nil
}

// pollScanPlan returns the files one configured root's provider watches,
// when the provider declares that they are everything polling acts on.
func (e *Engine) pollScanPlan(
	ctx context.Context, agent parser.AgentType, root string,
) ([]parser.WatchRoot, bool) {
	if agent == "" || isRemoteReconciliationRoot(root) {
		return nil, false
	}
	factory := e.sources().providerFactories[agent]
	if factory == nil ||
		factory.Capabilities().Source.PollMetadataScan != parser.CapabilitySupported {
		return nil, false
	}
	provider := factory.NewProvider(parser.ProviderConfig{
		Roots: []string{root}, Machine: e.machine,
		SourceMachines: e.sources().sourceMachines[agent],
		PathRewriter:   e.pathRewriter,
	})
	plan, err := provider.WatchPlan(ctx)
	if err != nil || len(plan.Roots) == 0 {
		return nil, false
	}
	for _, watch := range plan.Roots {
		if len(watch.IncludeGlobs) == 0 || len(watch.ExcludeGlobs) > 0 ||
			len(watch.ExtraDirectories) > 0 || watch.MaxDepth != 0 {
			return nil, false
		}
	}
	return plan.Roots, true
}

type pollScanResult struct {
	rows     []pollScanRow // Sorted by path hash.
	changed  []string      // New or changed files; empty without a previous snapshot.
	vanished bool          // A previous row has no current file.
}

// scanPollRoots lists every file the plan covers and compares it with
// previous. Any entry the scan cannot read, a symbolic link, a platform
// without change times, more than maxRows files, or more than
// pollScanMaxChangedPaths changes makes the root ineligible.
func scanPollRoots(
	ctx context.Context, seed maphash.Seed, plan []parser.WatchRoot,
	previous []pollScanRow, maxRows int,
) (pollScanResult, error) {
	var result pollScanResult
	var visit func(dir string, watch parser.WatchRoot) error
	visit = func(dir string, watch parser.WatchRoot) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := readPollScanDirectory(dir)
		if err != nil {
			return fmt.Errorf("%w: read directory: %w", errPollScanIneligible, err)
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			path := filepath.Join(dir, entry.Name())
			switch {
			case entry.Type()&(fs.ModeSymlink|fs.ModeIrregular) != 0:
				if pollScanLinkIgnored(path, entry.Name(), watch) {
					continue
				}
				// Discovery may follow links and junctions that this scan does not.
				return errPollScanIneligible
			case entry.IsDir():
				if watch.Recursive {
					if err := visit(path, watch); err != nil {
						return err
					}
				}
				continue
			case !entry.Type().IsRegular() || !pollScanMatches(watch.IncludeGlobs, entry.Name()):
				continue
			}
			signature, err := pollScanFileSignature(path, entry)
			if errors.Is(err, fs.ErrNotExist) {
				// Removed after listing: a previous row reports it as vanished.
				continue
			}
			if err != nil {
				return fmt.Errorf("%w: %w", errPollScanIneligible, err)
			}
			if len(result.rows) >= maxRows {
				return errPollScanIneligible
			}
			row := pollScanRow{
				path:      maphash.String(seed, path),
				signature: maphash.Comparable(seed, signature),
			}
			result.rows = append(result.rows, row)
			if previous == nil {
				continue
			}
			if i, found := findPollScanRow(previous, row.path); !found ||
				previous[i].signature != row.signature {
				if len(result.changed) == pollScanMaxChangedPaths {
					return errPollScanIneligible
				}
				result.changed = append(result.changed, path)
			}
		}
		return nil
	}
	for _, watch := range plan {
		info, err := os.Lstat(watch.Path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return pollScanResult{}, fmt.Errorf("%w: %w", errPollScanIneligible, err)
		}
		if !info.IsDir() {
			return pollScanResult{}, errPollScanIneligible
		}
		if err := visit(watch.Path, watch); err != nil {
			return pollScanResult{}, err
		}
	}
	slices.SortFunc(result.rows, func(a, b pollScanRow) int {
		return cmp.Or(cmp.Compare(a.path, b.path), cmp.Compare(a.signature, b.signature))
	})
	result.rows = slices.CompactFunc(result.rows, func(a, b pollScanRow) bool {
		return a.path == b.path
	})
	// An empty root still has a snapshot, unlike a root never scanned.
	if result.rows == nil {
		result.rows = []pollScanRow{}
	}
	result.rows = slices.Clip(result.rows)
	for _, row := range previous {
		if _, found := findPollScanRow(result.rows, row.path); !found {
			result.vanished = true
			break
		}
	}
	return result, nil
}

// pollScanLinkIgnored reports whether a link or junction cannot lead to a
// watched file: its name matches no glob, and it is not a directory the scan
// would descend into.
func pollScanLinkIgnored(path, name string, watch parser.WatchRoot) bool {
	if pollScanMatches(watch.IncludeGlobs, name) {
		return false
	}
	if !watch.Recursive {
		return true
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func findPollScanRow(rows []pollScanRow, path uint64) (int, bool) {
	return slices.BinarySearchFunc(rows, path, func(row pollScanRow, path uint64) int {
		return cmp.Compare(row.path, path)
	})
}

func pollScanMatches(globs []string, name string) bool {
	for _, glob := range globs {
		if matched, _ := filepath.Match(glob, name); matched {
			return true
		}
	}
	return false
}
