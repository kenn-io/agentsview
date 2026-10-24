package remotesync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"iter"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"go.kenn.io/kit/spill"

	"go.kenn.io/agentsview/internal/parser"
)

// MirrorDir returns the persistent mirror root for a remote host.
// The sanitized host keeps the path readable; the hash suffix
// disambiguates hosts whose sanitized forms collide ("host:8080" vs
// "host_8080"). Keying on the configured host string matches the
// existing DB identity (remote_skipped_files host, session IDPrefix).
func MirrorDir(dataDir, host string) string {
	sum := sha256.Sum256([]byte(host))
	name := sanitizeMirrorHost(host) + "-" + hex.EncodeToString(sum[:4])
	return filepath.Join(dataDir, "remote-mirrors", name)
}

func sanitizeMirrorHost(host string) string {
	var b strings.Builder
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		default:
			b.WriteRune('_')
		}
	}
	s := b.String()
	if len(s) > 40 {
		s = s[:40]
	}
	if s == "" {
		s = "host"
	}
	return s
}

// mirrorListMemoryBytes bounds the fetch and deletion lists a mirror diff
// keeps in memory. A larger list moves to a temporary file next to the
// mirror, so memory stays flat however large the change is.
const mirrorListMemoryBytes = 4 << 20

// MirrorDelta is the result of diffing a manifest against the mirror. Close
// releases the lists.
type MirrorDelta struct {
	// Fetch holds the remote paths of files that are absent from the mirror
	// or differ in size or mtime, in mirror path order.
	Fetch *spill.List
	// Deletions holds the local paths of mirror files absent from the
	// manifest, in mirror path order.
	Deletions *spill.List
	Total     int
	// codexIndexes lists the local paths of Codex session indexes that the
	// sync will replace or delete.
	codexIndexes []string
}

// Close removes any temporary files the lists moved to.
func (d MirrorDelta) Close() error {
	return errors.Join(d.Fetch.Close(), d.Deletions.Close())
}

// mirrorDiff compares a manifest against the mirror one entry at a time. A
// file is fetched when it is absent or differs in size or
// microsecond-truncated mtime; mirror files absent from the manifest are
// queued for deletion. Stat-based diffing is self-healing: a crashed
// extraction leaves mismatched size/mtime and is re-fetched.
//
// The manifest must arrive sorted by mirror path, as BuildManifest sorts it,
// and the mirror is walked in the same order. Only the fetch and deletion
// lists are kept, and they move to disk past mirrorListMemoryBytes.
//
// Mirror content from file-scoped agents (Windsurf) is never in the
// manifest, so the deletion pass removes it every sync. That is
// intentional: those exports are re-extracted from their own full
// archive right after, so only the remote's current export survives,
// the same parity the legacy temp-dir path had.
type mirrorDiff struct {
	root    string
	next    func() (mirrorFile, error, bool)
	stop    func()
	local   *mirrorFile
	lastKey string
	delta   MirrorDelta
}

func newMirrorDiff(mirrorRoot string) *mirrorDiff {
	next, stop := iter.Pull2(sortedMirrorFiles(mirrorRoot))
	return &mirrorDiff{root: mirrorRoot, next: next, stop: stop, delta: MirrorDelta{
		Fetch:     newMirrorList(mirrorRoot, "fetch"),
		Deletions: newMirrorList(mirrorRoot, "deletions"),
	}}
}

// newMirrorList returns an empty delta list whose temporary file, if it needs
// one, sits next to the mirror.
func newMirrorList(mirrorRoot, kind string) *spill.List {
	dir, name := filepath.Split(mirrorRoot)
	return spill.New(dir, "."+name+"-"+kind+"-*", mirrorListMemoryBytes)
}

// manifestOrderError reports a manifest that is not sorted by mirror path.
// Remotes before mirror-path sorting order a Windows manifest by its raw
// backslash paths.
type manifestOrderError struct{ key, last string }

func (e *manifestOrderError) Error() string {
	return fmt.Sprintf("manifest is not sorted by mirror path: %q follows %q", e.key, e.last)
}

func (e *manifestOrderError) FailureSummary() string {
	return "remote sync failed: the remote host runs an older agentsview " +
		"that lists its files in a different order; upgrade agentsview on " +
		"the remote host"
}

// mirrorListError reports a delta list that could not be written to or read
// from its temporary file next to the mirror.
type mirrorListError struct {
	op  string
	err error
}

func (e *mirrorListError) Error() string {
	return e.op + " temporary change list next to the mirror: " + e.err.Error()
}

func (e *mirrorListError) Unwrap() error { return e.err }

func (e *mirrorListError) FailureSummary() string {
	return "remote sync failed: could not " + e.op + " a temporary change " +
		"list next to the local mirror; check that the data directory is " +
		"writable and has free space"
}

// add compares one manifest entry.
func (d *mirrorDiff) add(entry ManifestEntry) error {
	local, err := safeRemappedRemotePath(d.root, entry.Path)
	if err != nil {
		return fmt.Errorf("manifest path %q: %w", entry.Path, err)
	}
	if local == d.root {
		return fmt.Errorf("manifest path %q names the mirror root", entry.Path)
	}
	rel, err := filepath.Rel(d.root, local)
	if err != nil {
		return fmt.Errorf("manifest path %q: %w", entry.Path, err)
	}
	key := filepath.ToSlash(rel)
	if d.delta.Total > 0 {
		// Two remote spellings of one path map to the same mirror file.
		if key == d.lastKey {
			return nil
		}
		if key < d.lastKey {
			return &manifestOrderError{key: key, last: d.lastKey}
		}
	} else if err := d.advance(); err != nil {
		return err
	}
	d.lastKey = key
	d.delta.Total++
	for d.local != nil && d.local.rel < key {
		if err := d.remove(*d.local); err != nil {
			return err
		}
		if err := d.advance(); err != nil {
			return err
		}
	}
	if d.local != nil && d.local.rel == key {
		info := d.local.info
		if err := d.advance(); err != nil {
			return err
		}
		if info.Size() == entry.Size &&
			mtimeMicros(info.ModTime().UnixNano()) == mtimeMicros(entry.MtimeNS) {
			return nil
		}
	}
	d.noteCodexIndex(local)
	if err := d.delta.Fetch.Append(entry.Path); err != nil {
		return &mirrorListError{op: "write", err: err}
	}
	return nil
}

// finish queues the mirror files after the last entry for deletion. The
// caller owns the returned delta and must close it.
func (d *mirrorDiff) finish() (MirrorDelta, error) {
	if d.delta.Total == 0 {
		if err := d.advance(); err != nil {
			return MirrorDelta{}, err
		}
	}
	for d.local != nil {
		if err := d.remove(*d.local); err != nil {
			return MirrorDelta{}, err
		}
		if err := d.advance(); err != nil {
			return MirrorDelta{}, err
		}
	}
	d.stop()
	delta := d.delta
	d.delta = MirrorDelta{}
	return delta, nil
}

// close ends the mirror walk and releases lists that finish did not hand
// over. It is safe to call more than once and after finish.
func (d *mirrorDiff) close() error {
	d.stop()
	if d.delta.Fetch == nil {
		return nil
	}
	err := d.delta.Close()
	d.delta = MirrorDelta{}
	return err
}

func (d *mirrorDiff) advance() error {
	file, err, ok := d.next()
	if !ok {
		d.local = nil
		return nil
	}
	if err != nil {
		return err
	}
	d.local = &file
	return nil
}

func (d *mirrorDiff) noteCodexIndex(localPath string) {
	if filepath.Base(localPath) == parser.CodexSessionIndexFilename {
		d.delta.codexIndexes = append(d.delta.codexIndexes, localPath)
	}
}

func (d *mirrorDiff) remove(file mirrorFile) error {
	d.noteCodexIndex(file.path)
	if err := d.delta.Deletions.Append(file.path); err != nil {
		return &mirrorListError{op: "write", err: err}
	}
	return nil
}

// collectPaths reads a whole delta list into memory.
func collectPaths(list *spill.List) ([]string, error) {
	paths := make([]string, 0, list.Len())
	for path, err := range list.All() {
		if err != nil {
			return nil, &mirrorListError{op: "read", err: err}
		}
		paths = append(paths, path)
	}
	return paths, nil
}

// filterMirrorList copies the paths that keep retains, batch by batch, into a
// new list next to the mirror. The caller must close the returned list.
func filterMirrorList(
	mirrorRoot, kind string, list *spill.List,
	keep func([]string) ([]string, error),
) (_ *spill.List, err error) {
	kept := newMirrorList(mirrorRoot, kind)
	defer func() {
		if err != nil {
			err = errors.Join(err, kept.Close())
		}
	}()
	for batch, err := range pathBatches(list, mirrorBatchBytes) {
		if err != nil {
			return nil, err
		}
		paths, err := keep(batch)
		if err != nil {
			return nil, err
		}
		for _, path := range paths {
			if err := kept.Append(path); err != nil {
				return nil, &mirrorListError{op: "write", err: err}
			}
		}
	}
	return kept, nil
}

// mirrorBatchBytes bounds the paths in one batch of a large mirror change,
// and so the delta file list of one archive request.
const mirrorBatchBytes = 4 << 20

// pathBatches yields a delta list in order, in batches whose paths total at
// most maxBytes. A longer path forms a batch of its own.
func pathBatches(list *spill.List, maxBytes int) iter.Seq2[[]string, error] {
	return func(yield func([]string, error) bool) {
		var batch []string
		size := 0
		for path, err := range list.All() {
			if err != nil {
				yield(nil, &mirrorListError{op: "read", err: err})
				return
			}
			if len(batch) > 0 && size+len(path) > maxBytes {
				if !yield(batch, nil) {
					return
				}
				batch, size = nil, 0
			}
			batch = append(batch, path)
			size += len(path)
		}
		if len(batch) > 0 {
			yield(batch, nil)
		}
	}
}

// mtimeMicros truncates a Unix-nanosecond mtime to microseconds. Some
// mirror filesystems store coarser timestamps than the remote's (NTFS
// keeps 100ns units), so exact nanosecond equality could mismatch
// forever and defeat the delta.
func mtimeMicros(ns int64) int64 { return ns / 1000 }

// mirrorFile is one regular file in the mirror. rel is its slash-separated
// path under the mirror root.
type mirrorFile struct {
	rel  string
	path string
	info fs.FileInfo
}

// sortedMirrorFiles yields the mirror's regular files in byte order of rel,
// the order BuildManifest sorts by. A directory sorts as its name plus "/",
// so "a-b" comes before everything under "a".
func sortedMirrorFiles(mirrorRoot string) iter.Seq2[mirrorFile, error] {
	return func(yield func(mirrorFile, error) bool) {
		walkSortedMirrorDir(mirrorRoot, "", yield)
	}
}

func walkSortedMirrorDir(
	dir, prefix string, yield func(mirrorFile, error) bool,
) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return true
		}
		yield(mirrorFile{}, fmt.Errorf("walk mirror %q: %w", dir, err))
		return false
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int {
		return strings.Compare(mirrorSortName(a), mirrorSortName(b))
	})
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		rel := prefix + entry.Name()
		if entry.IsDir() {
			if !walkSortedMirrorDir(path, rel+"/", yield) {
				return false
			}
			continue
		}
		info, err := entry.Info()
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			yield(mirrorFile{}, fmt.Errorf("walk mirror %q: %w", path, err))
			return false
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if !yield(mirrorFile{rel: rel, path: path, info: info}, nil) {
			return false
		}
	}
	return true
}

func mirrorSortName(entry fs.DirEntry) string {
	if entry.IsDir() {
		return entry.Name() + "/"
	}
	return entry.Name()
}

// ApplyMirrorDeletions removes mirror files that no longer exist on
// the remote, then prunes directories the removals left empty so a
// remote path that changed type (a directory replaced by a file) can
// be re-created by the next extraction. Every path must lie inside
// the mirror root; deletion is the only destructive mirror operation,
// so it fails closed on any path that escapes. DB sessions are never
// deleted (the engine runs in Ephemeral mode); this only trims the
// transfer cache.
func ApplyMirrorDeletions(mirrorRoot string, deletions []string) error {
	for _, path := range deletions {
		if path == mirrorRoot || !within(mirrorRoot, path) {
			return fmt.Errorf("mirror deletion %q escapes mirror root %q", path, mirrorRoot)
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("delete mirror file %q: %w", path, err)
		}
		pruneEmptyMirrorDirs(mirrorRoot, filepath.Dir(path))
	}
	return nil
}

// RemoveMirrorTypeConflicts removes local directories that occupy the
// mirror path of a file about to be fetched. The deletion pass cannot
// see them: MirrorDiff only tracks regular files, so a directory left
// behind by a crashed extraction (created as a parent, never filled)
// would make every subsequent extraction of that file fail.
// Confinement mirrors ApplyMirrorDeletions: destructive operations
// fail closed on any path outside the mirror root.
func RemoveMirrorTypeConflicts(mirrorRoot string, fetch []string) error {
	for _, remotePath := range fetch {
		local, err := safeRemappedRemotePath(mirrorRoot, remotePath)
		if err != nil {
			return fmt.Errorf("fetch path %q: %w", remotePath, err)
		}
		info, err := os.Lstat(local)
		if err != nil || !info.IsDir() {
			continue
		}
		if local == mirrorRoot || !within(mirrorRoot, local) {
			return fmt.Errorf(
				"mirror conflict %q escapes mirror root %q", local, mirrorRoot,
			)
		}
		if err := os.RemoveAll(local); err != nil {
			return fmt.Errorf("remove conflicting mirror dir %q: %w", local, err)
		}
	}
	return nil
}

// RemoveMirrorFetchFiles evicts the old mirror copy of each requested file
// after its replacement archive has downloaded successfully. A live remote
// file can disappear after the manifest advertised it; archive writers omit
// that vanished file, and removing the old copy ensures it cannot remain as a
// stale SQLite sidecar. The next extraction restores every file that was
// actually present in the downloaded archive.
func RemoveMirrorFetchFiles(mirrorRoot string, fetch []string) error {
	for _, remotePath := range fetch {
		local, err := safeRemappedRemotePath(mirrorRoot, remotePath)
		if err != nil {
			return fmt.Errorf("fetch path %q: %w", remotePath, err)
		}
		if local == mirrorRoot || !within(mirrorRoot, local) {
			return fmt.Errorf(
				"mirror fetch eviction %q escapes mirror root %q", local, mirrorRoot,
			)
		}
		if err := os.Remove(local); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("evict mirror file %q: %w", local, err)
		}
		pruneEmptyMirrorDirs(mirrorRoot, filepath.Dir(local))
	}
	return nil
}

// pruneEmptyMirrorDirs best-effort removes now-empty directories from
// dir upward, never including the mirror root itself. os.Remove fails
// on a non-empty directory, which terminates the walk.
func pruneEmptyMirrorDirs(mirrorRoot, dir string) {
	for dir != mirrorRoot && within(mirrorRoot, dir) {
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// MirrorLockHandle holds the exclusive per-mirror flock.
type MirrorLockHandle struct {
	lock *flock.Flock
}

// AcquireMirrorLock takes an exclusive lock serializing mirror
// mutation AND import across processes. The lock file lives NEXT TO
// the mirror root (<mirror>.lock), never inside it: an in-mirror lock
// file would be absent from every manifest and deleted while held,
// silently ending mutual exclusion. Callers hold the lock through
// import because extraction truncates files in place and the engine
// reads the mirror during SyncAll.
func AcquireMirrorLock(
	ctx context.Context, mirrorRoot string,
) (*MirrorLockHandle, error) {
	lockPath, err := canonicalMirrorLockPath(mirrorRoot)
	if err != nil {
		return nil, err
	}
	lock := flock.New(lockPath)
	locked, err := lock.TryLockContext(ctx, 250*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("acquire mirror lock %s: %w", lock.Path(), err)
	}
	if !locked {
		return nil, fmt.Errorf("mirror lock %s is held by another sync", lock.Path())
	}
	return &MirrorLockHandle{lock: lock}, nil
}

// Close releases the mirror lock.
func (h *MirrorLockHandle) Close() error {
	if h == nil || h.lock == nil {
		return nil
	}
	if err := h.lock.Unlock(); err != nil {
		return fmt.Errorf("release mirror lock %s: %w", h.lock.Path(), err)
	}
	return nil
}
