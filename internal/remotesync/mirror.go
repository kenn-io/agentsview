package remotesync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gofrs/flock"
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

// MirrorDelta is the result of diffing a manifest against the mirror.
type MirrorDelta struct {
	Fetch     []string
	Deletions []string
	Total     int
}

// MirrorDiff stat-walks the mirror and compares it against the
// manifest. A file is fetched when it is absent or differs in size or
// microsecond-truncated mtime; mirror files absent from the manifest
// are queued for deletion. Stat-based diffing is self-healing: a
// crashed extraction leaves mismatched size/mtime and is re-fetched.
//
// Mirror content from file-scoped agents (Windsurf) is never in the
// manifest, so the deletion pass removes it every sync. That is
// intentional: those exports are re-extracted from their own full
// archive right after, so only the remote's current export survives —
// the same parity the legacy temp-dir path had.
func MirrorDiff(mirrorRoot string, m Manifest) (MirrorDelta, error) {
	delta := MirrorDelta{Total: len(m.Files)}
	expected := make(map[string]ManifestEntry, len(m.Files))
	for _, entry := range m.Files {
		local, err := safeRemappedRemotePath(mirrorRoot, entry.Path)
		if err != nil {
			return MirrorDelta{}, fmt.Errorf("manifest path %q: %w", entry.Path, err)
		}
		expected[local] = entry
	}
	local, err := mirrorFiles(mirrorRoot)
	if err != nil {
		return MirrorDelta{}, err
	}
	for localPath, entry := range expected {
		info, ok := local[localPath]
		if !ok || info.Size() != entry.Size ||
			mtimeMicros(info.ModTime().UnixNano()) != mtimeMicros(entry.MtimeNS) {
			delta.Fetch = append(delta.Fetch, entry.Path)
		}
	}
	for localPath := range local {
		if _, ok := expected[localPath]; !ok {
			delta.Deletions = append(delta.Deletions, localPath)
		}
	}
	sort.Strings(delta.Fetch)
	sort.Strings(delta.Deletions)
	return delta, nil
}

// mtimeMicros truncates a Unix-nanosecond mtime to microseconds. Some
// mirror filesystems store coarser timestamps than the remote's (NTFS
// keeps 100ns units), so exact nanosecond equality could mismatch
// forever and defeat the delta.
func mtimeMicros(ns int64) int64 { return ns / 1000 }

func mirrorFiles(mirrorRoot string) (map[string]os.FileInfo, error) {
	files := make(map[string]os.FileInfo)
	err := filepath.WalkDir(mirrorRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		files[path] = info
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk mirror %q: %w", mirrorRoot, err)
	}
	return files, nil
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
