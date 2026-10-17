package sync

import (
	"cmp"
	"context"
	"errors"
	"hash/maphash"
	"io"
	"log"
	"math/bits"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

const (
	sourceScanInterval = 30 * time.Second
	sourceScanPageSize = 256
	// An overflowed root cannot compare uncached sources, so it audits them
	// through provider reconciliation on this pass cadence instead of
	// resubmitting them every pass.
	sourceScanOverflowAuditPasses = 10
	sourceScanMaxEntries          = 1 << 20
	sourceScanMaxDirectoryBytes   = 8 << 20
)

// SourceScanStats describes metadata coverage, without source paths or contents.
// CachedDirectoryBytes counts retained directory strings, not map overhead.
// Saturated reports whether the most recent pass could not cache every source.
type SourceScanStats struct {
	Passes               uint64
	Files                uint64
	ChangedPaths         uint64
	CachedFiles          uint64
	CachedDirectoryBytes uint64
	Failures             uint64
	LastDuration         time.Duration
	Saturated            bool
}

type sourceScanSignature struct{ size, mtime, inode, device int64 }

// sourceScanRow is the whole per-file cache cost: hashes of the basename and
// signature. Hashes never leave the process, so a per-process seed suffices.
// A basename collision only repeats dispatch; a signature collision hides one
// change with 2^-64 odds.
type sourceScanRow struct{ name, signature uint64 }

// sourceScanDirectory keeps rows sorted by name hash. Admissions wait in added
// until the pass ends, then merge into an exactly sized slice.
type sourceScanDirectory struct {
	rows  []sourceScanRow
	seen  []uint64 // Bitset parallel to rows for the current pass.
	added []sourceScanRow
}

func (d *sourceScanDirectory) find(name uint64) (int, bool) {
	return slices.BinarySearchFunc(d.rows, name, func(row sourceScanRow, name uint64) int { return cmp.Compare(row.name, name) })
}

func (d *sourceScanDirectory) markSeen(i int) { d.seen[i/64] |= 1 << (i % 64) }

func (d *sourceScanDirectory) isSeen(i int) bool { return d.seen[i/64]&(1<<(i%64)) != 0 }

func (d *sourceScanDirectory) markAllSeen() {
	for i := range d.rows {
		d.markSeen(i)
	}
}

func (d *sourceScanDirectory) missing() bool {
	seen := 0
	for _, word := range d.seen {
		seen += bits.OnesCount64(word)
	}
	return seen < len(d.rows)
}

type sourceScanRoot struct {
	plan        WatchRoot
	initialized bool
	directories map[string]*sourceScanDirectory
	// overflowed is set when this pass left a source uncached. auditPending
	// survives until a reconciliation covers every source left uncached.
	overflowed       bool
	auditPending     bool
	passesSinceAudit int
}

type sourceScanner struct {
	roots             []sourceScanRoot
	excludes          []string
	seed              maphash.Seed
	maxEntries        int
	maxDirectoryBytes int
	entries           int
	directoryBytes    int
	counters          SourceScanStats
	snapshot          atomic.Pointer[SourceScanStats]
}

func newSourceScanner(roots []WatchRoot, excludes []string) *sourceScanner {
	s := &sourceScanner{
		excludes: normalizeExcludePatterns(excludes), seed: maphash.MakeSeed(),
		maxEntries: sourceScanMaxEntries, maxDirectoryBytes: sourceScanMaxDirectoryBytes,
	}
	for _, root := range roots {
		if len(root.SourceFileGlobs) != 0 {
			s.roots = append(s.roots, sourceScanRoot{plan: root, directories: make(map[string]*sourceScanDirectory)})
		}
	}
	return s
}

func (s *sourceScanner) stats() SourceScanStats {
	if snapshot := s.snapshot.Load(); snapshot != nil {
		return *snapshot
	}
	return SourceScanStats{}
}

// cached reports whether a source path has a retained signature.
func (s *sourceScanner) cached(path string) bool {
	name := maphash.String(s.seed, filepath.Base(path))
	for _, root := range s.roots {
		if directory := root.directories[filepath.Dir(path)]; directory != nil {
			if _, found := directory.find(name); found {
				return true
			}
		}
	}
	return false
}

// run owns all mutable scanner state. Acknowledgement waits never hold the
// native event sink lock. The completion-based timer cannot accumulate scans.
func (s *sourceScanner) run(ctx context.Context, emit WatchCallback) {
	for ctx.Err() == nil {
		err := s.scan(ctx, emit)
		if ctx.Err() != nil {
			return
		}
		stats := s.stats()
		log.Printf("source scan: files=%d changed=%d cached=%d directory_bytes=%d saturated=%t duration=%s failures=%d",
			stats.Files, stats.ChangedPaths, stats.CachedFiles, stats.CachedDirectoryBytes,
			stats.Saturated, stats.LastDuration.Round(time.Millisecond), stats.Failures)
		if err != nil {
			log.Printf("source scan: incomplete metadata coverage")
		}
		timer := time.NewTimer(sourceScanInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (s *sourceScanner) scan(ctx context.Context, emit WatchCallback) error {
	started := time.Now()
	s.counters.Files = 0
	s.counters.ChangedPaths = 0
	defer func() {
		s.counters.Passes++
		s.counters.CachedFiles = uint64(s.entries)
		s.counters.CachedDirectoryBytes = uint64(s.directoryBytes)
		s.counters.LastDuration = time.Since(started)
		s.counters.Saturated = slices.ContainsFunc(s.roots, func(root sourceScanRoot) bool { return root.overflowed })
		snapshot := s.counters
		s.snapshot.Store(&snapshot)
	}()
	var failures error
	for i := range s.roots {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.scanRoot(ctx, &s.roots[i], emit); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.counters.Failures++
			failures = errors.Join(failures, err)
		}
	}
	return failures
}

type sourceScanObservation struct {
	directory string
	name      string
	row       sourceScanRow
	index     int // Position in the directory's rows, or -1 when uncached.
}

func (s *sourceScanner) scanRoot(ctx context.Context, root *sourceScanRoot, emit WatchCallback) error {
	root.overflowed = false
	// Missing or unreadable roots carry no authority to remove archived sessions.
	info, err := os.Stat(root.plan.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return nil
	}
	prune := false
	defer func() { s.compact(root, prune) }()
	// An unreadable subtree keeps its cached rows, since an unread listing is
	// not disappearance evidence. Its siblings still receive coverage.
	var incomplete error
	skip := func(path string, err error) {
		incomplete = errors.Join(incomplete, err)
		for directory, records := range root.directories {
			if samePathOrDescendant(directory, path) {
				records.markAllSeen()
			}
		}
	}
	pending := make([]sourceScanObservation, 0, sourceScanPageSize)
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		paths := make([]string, 0, len(pending))
		for _, observation := range pending {
			paths = append(paths, filepath.Join(observation.directory, observation.name))
		}
		if err := emit(ctx, WatchBatch{Paths: paths}); err != nil {
			return err
		}
		for _, observation := range pending {
			s.retain(root, observation)
		}
		s.counters.ChangedPaths += uint64(len(pending))
		pending = pending[:0]
		return nil
	}
	var visit func(string) error
	visit = func(directory string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if shouldExcludeForRoot(s.excludes, directory, root.plan.Path) {
			return nil
		}
		dir, err := openSourceScanDirectory(directory)
		if err != nil {
			if directory == root.plan.Path {
				return err
			}
			if !errors.Is(err, os.ErrNotExist) {
				skip(directory, err)
			}
			return nil
		}
		defer dir.Close() // Read-only directory handle.
		records := root.directories[directory]
		for {
			entries, readErr := dir.ReadDir(sourceScanPageSize)
			for _, entry := range entries {
				if err := ctx.Err(); err != nil {
					return err
				}
				path := filepath.Join(directory, entry.Name())
				if shouldExcludeForRoot(s.excludes, path, root.plan.Path) {
					continue
				}
				isDir := entry.IsDir()
				if root.plan.FollowChildDirectorySymlinks && directory == root.plan.Path && entry.Type()&os.ModeSymlink != 0 {
					target, err := os.Stat(path)
					if errors.Is(err, os.ErrNotExist) {
						continue // A dangling project link has nothing to cover.
					}
					if err != nil {
						skip(path, err)
						continue
					}
					isDir = target.IsDir()
				}
				if isDir {
					if root.plan.Recursive {
						if err := visit(path); err != nil {
							return err
						}
					}
					continue
				}
				if !s.matches(root.plan.SourceFileGlobs, entry.Name()) {
					continue
				}
				signature, regular, err := sourceScanFileSignature(path, entry)
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				observation := sourceScanObservation{directory: directory, name: entry.Name(), index: -1}
				observation.row.name = maphash.String(s.seed, entry.Name())
				if records != nil {
					if i, found := records.find(observation.row.name); found {
						observation.index = i
						if err != nil || regular {
							records.markSeen(i)
						}
					}
				}
				if err != nil {
					incomplete = errors.Join(incomplete, err)
					continue
				}
				if !regular {
					continue
				}
				s.counters.Files++
				observation.row.signature = maphash.Comparable(s.seed, signature)
				switch {
				case !root.initialized:
					// The first pass seeds metadata before authoritative startup work.
					// Until that work succeeds, no cached signature can suppress dispatch.
					s.retain(root, observation)
				case observation.index >= 0 && records.rows[observation.index].signature == observation.row.signature:
				case observation.index < 0 && !s.admits(root, directory):
					// A full cache cannot compare this source; the overflow audit covers it.
					root.overflowed = true
				default:
					pending = append(pending, observation)
					if len(pending) == sourceScanPageSize {
						if err := flush(); err != nil {
							return err
						}
					}
				}
			}
			runtime.Gosched() // Native event collection runs independently of scan pages.
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			if readErr != nil {
				skip(directory, readErr)
				return nil
			}
		}
	}
	if err := visit(root.plan.Path); err != nil {
		return err
	}
	if err := flush(); err != nil {
		return err
	}
	missing := false
	for _, records := range root.directories {
		if records.missing() {
			missing = true
			break
		}
	}
	if root.overflowed {
		root.auditPending = true
	}
	root.passesSinceAudit++
	audit := root.auditPending && (!root.overflowed || root.passesSinceAudit >= sourceScanOverflowAuditPasses)
	if !root.initialized || missing || audit {
		// The provider must perform its own complete discovery before applying
		// deletion. A cache absence is only a request to check that authority.
		roots := []string{root.plan.Path}
		if len(root.plan.SourceScanScopes) > 0 {
			roots = nil
			var missingScopes []string
			for _, scope := range root.plan.SourceScanScopes {
				info, err := os.Stat(scope.SyncDir)
				if errors.Is(err, os.ErrNotExist) {
					missingScopes = append(missingScopes, scope.SyncDir)
					continue
				}
				if err != nil {
					return errors.Join(incomplete, err)
				}
				if !info.IsDir() {
					missingScopes = append(missingScopes, scope.SyncDir)
					continue
				}
				if !slices.Contains(roots, scope.SyncDir) {
					roots = append(roots, scope.SyncDir)
				}
			}
			roots = slices.DeleteFunc(roots, func(root string) bool {
				for _, missing := range missingScopes {
					if samePathOrDescendant(root, missing) || samePathOrDescendant(missing, root) {
						return true
					}
				}
				return false
			})
			if len(roots) == 0 {
				return incomplete
			}
		}
		if err := emit(ctx, WatchBatch{ReconcileRoots: roots}); err != nil {
			return errors.Join(incomplete, err)
		}
		root.initialized = true
		root.passesSinceAudit = 0
		root.auditPending = root.overflowed
		prune = true
	}
	return incomplete
}

// openSourceScanDirectory lists through Root.Open, which shares delete access
// on Windows, so a directory held across a paused page acknowledgement can
// still be renamed. The root handle lacks that sharing and closes at once.
func openSourceScanDirectory(path string) (*os.File, error) {
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	dir, err := root.Open(".")
	if closeErr := root.Close(); closeErr != nil && err == nil {
		_ = dir.Close()
		return nil, closeErr
	}
	return dir, err
}

func (s *sourceScanner) admits(root *sourceScanRoot, directory string) bool {
	if s.entries >= s.maxEntries {
		return false
	}
	_, exists := root.directories[directory]
	return exists || s.directoryBytes+len(directory) <= s.maxDirectoryBytes
}

func (s *sourceScanner) retain(root *sourceScanRoot, observation sourceScanObservation) {
	records := root.directories[observation.directory]
	if observation.index >= 0 && records != nil {
		records.rows[observation.index].signature = observation.row.signature
		return
	}
	if !s.admits(root, observation.directory) {
		root.overflowed = true
		return
	}
	if records == nil {
		records = &sourceScanDirectory{}
		root.directories[observation.directory] = records
		s.directoryBytes += len(observation.directory)
	}
	records.added = append(records.added, observation.row)
	s.entries++
}

// compact ends a pass. It merges admissions, drops rows that a successful
// reconciliation did not see, and clears seen bits for the next pass.
func (s *sourceScanner) compact(root *sourceScanRoot, prune bool) {
	for directory, records := range root.directories {
		if len(records.added) == 0 && (!prune || !records.missing()) {
			clear(records.seen)
			continue
		}
		slices.SortFunc(records.added, func(a, b sourceScanRow) int { return cmp.Compare(a.name, b.name) })
		rows := make([]sourceScanRow, 0, len(records.rows)+len(records.added))
		keep := func(row sourceScanRow) {
			if len(rows) > 0 && rows[len(rows)-1].name == row.name {
				rows[len(rows)-1] = row // Colliding basenames share one row.
				s.entries--
				return
			}
			rows = append(rows, row)
		}
		i, j := 0, 0
		for i < len(records.rows) || j < len(records.added) {
			if j == len(records.added) || (i < len(records.rows) && records.rows[i].name < records.added[j].name) {
				if !prune || records.isSeen(i) {
					keep(records.rows[i])
				} else {
					s.entries--
				}
				i++
				continue
			}
			keep(records.added[j])
			j++
		}
		if len(rows) == 0 {
			delete(root.directories, directory)
			s.directoryBytes -= len(directory)
			continue
		}
		if len(rows) < cap(rows) {
			rows = slices.Clone(rows)
		}
		records.rows = rows
		records.added = []sourceScanRow{}
		records.seen = make([]uint64, (len(rows)+63)/64)
	}
}

func (*sourceScanner) matches(globs []string, name string) bool {
	for _, glob := range globs {
		if matched, _ := filepath.Match(glob, name); matched {
			return true
		}
	}
	return false
}

// SourceScanStats returns the most recently completed metadata pass.
func (w *Watcher) SourceScanStats() SourceScanStats {
	if w.scanner == nil {
		return SourceScanStats{}
	}
	return w.scanner.stats()
}

// OwnsSourceScanScope identifies configured scopes with shared metadata
// coverage, so the daemon does not also run whole-root polling for them.
func (w *Watcher) OwnsSourceScanScope(agent, directory string) bool {
	if w.scanner == nil {
		return false
	}
	for _, root := range w.scanner.roots {
		for _, scope := range root.plan.SourceScanScopes {
			if scope.Agent == agent && filepath.Clean(scope.SyncDir) == filepath.Clean(directory) {
				return true
			}
		}
	}
	return false
}

type sourceScanAcknowledgement struct {
	done chan error
	once sync.Once
}

func (a *sourceScanAcknowledgement) acknowledgeLifecycle(uint64) { a.once.Do(func() { a.done <- nil }) }

func (a *sourceScanAcknowledgement) rejectLifecycle(err error) { a.once.Do(func() { a.done <- err }) }

func (w *Watcher) emitSourceScan(ctx context.Context, batch WatchBatch) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ack := &sourceScanAcknowledgement{done: make(chan error, 1)}
	batch.lifecycleTokens = []backendLifecycleToken{{gate: ack, generation: 1}}
	w.eventSink.RetainRetryImmediate(batch)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-ack.done:
		return err
	}
}

// Scan pages already have bounded work and a completion-based cadence. They
// share the native dispatcher but must not pay its event-storm floor per page.
func (s *watchEventSink) sourceScanPending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.absorbHandoff()
	for token := range s.pending.lifecycle {
		if _, ok := token.gate.(*sourceScanAcknowledgement); ok {
			return true
		}
	}
	return false
}
