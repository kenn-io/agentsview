package sync

import (
	"context"
	"errors"
	"io"
	"log"
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
)

// SourceScanStats describes metadata coverage, without source paths or contents.
// CachedNameBytes counts retained strings, not Go map and allocator overhead.
type SourceScanStats struct {
	Passes          uint64
	Files           uint64
	ChangedPaths    uint64
	CachedFiles     uint64
	CachedNameBytes uint64
	Failures        uint64
	LastDuration    time.Duration
	Saturated       bool
}

type (
	sourceScanSignature struct{ size, mtime, inode, device int64 }
	sourceScanRecord    struct {
		signature sourceScanSignature
		seen      uint64
	}
)

type sourceScanRoot struct {
	plan        WatchRoot
	initialized bool
	saturated   bool
	directories map[string]map[string]sourceScanRecord
}

type sourceScanner struct {
	roots        []sourceScanRoot
	excludes     []string
	generation   uint64
	maxEntries   int
	maxNameBytes int
	entries      int
	nameBytes    int
	counters     SourceScanStats
	snapshot     atomic.Pointer[SourceScanStats]
}

func newSourceScanner(roots []WatchRoot, excludes []string) *sourceScanner {
	s := &sourceScanner{excludes: normalizeExcludePatterns(excludes), maxEntries: 65536, maxNameBytes: 8 << 20}
	for _, root := range roots {
		if len(root.SourceFileGlobs) != 0 {
			s.roots = append(s.roots, sourceScanRoot{plan: root, directories: make(map[string]map[string]sourceScanRecord)})
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

// run owns all mutable scanner state. Acknowledgement waits never hold the
// native event sink lock. The completion-based timer cannot accumulate scans.
func (s *sourceScanner) run(ctx context.Context, emit WatchCallback) {
	for ctx.Err() == nil {
		err := s.scan(ctx, emit)
		if ctx.Err() != nil {
			return
		}
		stats := s.stats()
		log.Printf("source scan: files=%d changed=%d cached=%d name_bytes=%d saturated=%t duration=%s failures=%d",
			stats.Files, stats.ChangedPaths, stats.CachedFiles, stats.CachedNameBytes,
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
	s.generation++
	s.counters.Files = 0
	s.counters.ChangedPaths = 0
	defer func() {
		s.counters.Passes++
		s.counters.CachedFiles = uint64(s.entries)
		s.counters.CachedNameBytes = uint64(s.nameBytes)
		s.counters.LastDuration = time.Since(started)
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
	directory, name string
	signature       sourceScanSignature
}

func (s *sourceScanner) scanRoot(ctx context.Context, root *sourceScanRoot, emit WatchCallback) error {
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
		dir, err := os.Open(directory)
		if err != nil {
			return err
		}
		defer dir.Close() // Read-only directory handle.
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
					if err != nil {
						return err
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
				info, err := entry.Info()
				if entry.Type()&os.ModeSymlink != 0 {
					info, err = os.Stat(path)
				}
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				if err != nil {
					return err
				}
				if !info.Mode().IsRegular() {
					continue
				}
				inode, device := getFileIdentity(path, info)
				signature := sourceScanSignature{size: info.Size(), mtime: info.ModTime().UnixNano(), inode: inode, device: device}
				s.counters.Files++
				records := root.directories[directory]
				prior, cached := records[entry.Name()]
				if cached {
					prior.seen = s.generation
					records[entry.Name()] = prior
				}
				observation := sourceScanObservation{directory: directory, name: entry.Name(), signature: signature}
				if !root.initialized {
					// The first pass seeds metadata before authoritative startup work.
					// Until that work succeeds, no cached signature can suppress dispatch.
					s.retain(root, observation)
				} else if !cached || prior.signature != signature {
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
				return readErr
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
		for _, record := range records {
			if record.seen != s.generation {
				missing = true
				break
			}
		}
	}
	if !root.initialized || missing || root.saturated {
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
					return err
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
				return nil
			}
		}
		if err := emit(ctx, WatchBatch{ReconcileRoots: roots}); err != nil {
			return err
		}
		root.initialized = true
		for directory, records := range root.directories {
			for name, record := range records {
				if record.seen != s.generation {
					delete(records, name)
					s.entries--
					s.nameBytes -= len(name)
				}
			}
			if len(records) == 0 {
				delete(root.directories, directory)
				s.nameBytes -= len(directory)
			}
		}
	}
	return nil
}

func (s *sourceScanner) retain(root *sourceScanRoot, observation sourceScanObservation) {
	records := root.directories[observation.directory]
	if _, exists := records[observation.name]; !exists {
		bytes := len(observation.name)
		if records == nil {
			bytes += len(observation.directory)
		}
		if s.entries == s.maxEntries || s.nameBytes+bytes > s.maxNameBytes {
			s.counters.Saturated = true
			root.saturated = true
			return
		}
		if records == nil {
			records = make(map[string]sourceScanRecord)
			root.directories[observation.directory] = records
		}
		s.entries++
		s.nameBytes += bytes
	}
	records[observation.name] = sourceScanRecord{signature: observation.signature, seen: s.generation}
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
