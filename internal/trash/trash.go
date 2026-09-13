// Package trash moves files to the operating system's trash (recycle
// bin) and records enough manifest state to restore them later.
//
// Backends, in preference order:
//
//   - darwin: ~/.Trash
//   - freedesktop: ~/.local/share/Trash (files/ + info/*.trashinfo)
//   - fallback: an agentsview-owned directory under the data dir,
//     used on Windows (where the recycle bin is not reachable with
//     stdlib only) and whenever the system trash is unavailable or on
//     another device.
//
// Every trashed path is appended to a JSONL manifest under the data
// dir, grouped by batch, so callers can restore a whole batch (or the
// most recent one) without parsing backend-specific state.
package trash

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Backend names recorded in manifest entries.
const (
	BackendDarwin      = "darwin"
	BackendFreedesktop = "freedesktop"
	BackendFallback    = "fallback"
)

// Item is one trashed path as recorded in the manifest.
type Item struct {
	ID           string    `json:"id"`
	BatchID      string    `json:"batchId"`
	OriginalPath string    `json:"originalPath"`
	TrashedPath  string    `json:"trashedPath"`
	Size         int64     `json:"size"`
	SessionID    string    `json:"sessionId,omitempty"`
	Agent        string    `json:"agent,omitempty"`
	DeletedAt    time.Time `json:"deletedAt"`
	Backend      string    `json:"backend"`
}

// Meta carries optional session context aligned by index with the
// paths passed to Trash.
type Meta struct {
	SessionID string
	Agent     string
}

// Errors returned by Store operations.
var (
	// ErrNotFound reports that a manifest entry's trashed file is
	// missing, or that no entry matches a restore request.
	ErrNotFound = errors.New("trash: not found")
)

// backend is the per-platform destination for trashed paths.
type backend interface {
	name() string
	// prepare ensures the destination directories exist.
	prepare() error
	// move moves src into the trash under the given base name and
	// returns the final trashed path.
	move(src, name string) (string, error)
	// cleanup removes backend-side metadata (such as freedesktop
	// trashinfo files) for a trashed item that has been restored.
	cleanup(trashedPath string) error
}

// Store trashes paths and records restore state under dataDir.
type Store struct {
	dataDir string
	impl    backend
	now     func() time.Time
}

// New resolves the best backend for the current platform and returns
// a store rooted at dataDir (used for the manifest and the fallback
// backend).
func New(dataDir string) *Store {
	s := &Store{dataDir: dataDir, now: time.Now}
	s.impl = resolveBackend(dataDir)
	return s
}

// Trash moves every path to the trash as one batch and appends the
// resulting entries to the manifest. Meta, when non-empty, is aligned
// by index with paths. Successful items are returned even when some
// paths fail; the error aggregates the individual failures so callers
// can report a partial batch.
func (s *Store) Trash(paths []string, meta []Meta) ([]Item, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	if err := s.impl.prepare(); err != nil {
		return nil, fmt.Errorf("trash: preparing %s backend: %w", s.impl.name(), err)
	}
	if err := os.MkdirAll(s.manifestDir(), 0o755); err != nil {
		return nil, fmt.Errorf("trash: creating manifest dir: %w", err)
	}

	batch := newBatchID(s.now())
	deleted := s.now().UTC()
	var (
		items []Item
		errs  []error
	)
	for i, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			errs = append(errs, fmt.Errorf("trash: resolving %s: %w", p, err))
			continue
		}
		info, err := os.Lstat(abs)
		if err != nil {
			errs = append(errs, fmt.Errorf("trash: stat %s: %w", abs, err))
			continue
		}
		name := uniqueName(s.impl, filepath.Base(abs))
		trashedPath, movedVia, err := s.moveToTrash(abs, name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		item := Item{
			ID:           fmt.Sprintf("%s/%d", batch, i),
			BatchID:      batch,
			OriginalPath: abs,
			TrashedPath:  trashedPath,
			Size:         info.Size(),
			DeletedAt:    deleted,
			Backend:      movedVia,
		}
		if i < len(meta) {
			item.SessionID = meta[i].SessionID
			item.Agent = meta[i].Agent
		}
		if err := appendManifest(s.manifestPath(), item); err != nil {
			errs = append(errs, fmt.Errorf("trash: recording %s: %w", abs, err))
			continue
		}
		items = append(items, item)
	}
	return items, errors.Join(errs...)
}

// RestoreBatch restores every manifest entry in the batch, in reverse
// trashed order. Entries whose trashed file is missing are skipped and
// reported in the returned error; successfully restored entries are
// removed from the manifest.
func (s *Store) RestoreBatch(batchID string) ([]Item, error) {
	entries, err := readManifest(s.manifestPath())
	if err != nil {
		return nil, err
	}
	var batch []Item
	for _, e := range entries {
		if e.BatchID == batchID {
			batch = append(batch, e)
		}
	}
	if len(batch) == 0 {
		return nil, fmt.Errorf("trash: no manifest entries for batch %s: %w", batchID, ErrNotFound)
	}
	return s.restore(batch)
}

// RestoreLast restores the most recently written batch.
func (s *Store) RestoreLast() ([]Item, error) {
	entries, err := readManifest(s.manifestPath())
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("trash: manifest is empty: %w", ErrNotFound)
	}
	last := entries[len(entries)-1].BatchID
	var batch []Item
	for _, e := range entries {
		if e.BatchID == last {
			batch = append(batch, e)
		}
	}
	return s.restore(batch)
}

// List returns every manifest entry in append order.
func (s *Store) List() ([]Item, error) {
	return readManifest(s.manifestPath())
}

// restore moves the batch back to its original paths in reverse order,
// rewrites the manifest without the restored entries, and aggregates
// per-item failures.
func (s *Store) restore(batch []Item) ([]Item, error) {
	var (
		restored []Item
		errs     []error
	)
	for i := len(batch) - 1; i >= 0; i-- {
		item := batch[i]
		target, err := restorePath(item.OriginalPath, s.now())
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := os.Lstat(item.TrashedPath); err != nil {
			errs = append(errs, fmt.Errorf("trash: trashed file %s missing: %w", item.TrashedPath, ErrNotFound))
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			errs = append(errs, fmt.Errorf("trash: recreating parent of %s: %w", target, err))
			continue
		}
		if err := os.Rename(item.TrashedPath, target); err != nil {
			errs = append(errs, fmt.Errorf("trash: restoring %s: %w", item.TrashedPath, err))
			continue
		}
		if err := s.impl.cleanup(item.TrashedPath); err != nil {
			// The file itself is restored; metadata cleanup is
			// best-effort.
			errs = append(errs, fmt.Errorf("trash: cleaning up %s metadata: %w", item.TrashedPath, err))
		}
		restored = append(restored, item)
	}
	if len(restored) > 0 {
		if err := s.removeEntries(restored); err != nil {
			errs = append(errs, err)
		}
	}
	return restored, errors.Join(errs...)
}

// moveToTrash dispatches to the active backend, falling back to the
// agentsview-owned directory when the system trash refuses the move
// (typically a cross-device rename). It returns the trashed path and
// the backend that actually performed the move.
func (s *Store) moveToTrash(abs, name string) (string, string, error) {
	trashed, err := s.impl.move(abs, name)
	if err == nil {
		return trashed, s.impl.name(), nil
	}
	if !isCrossDevice(err) || s.impl.name() == BackendFallback {
		return "", "", fmt.Errorf("trash: moving %s: %w", abs, err)
	}
	fb := fallbackBackend{s.dataDir}
	if err := fb.prepare(); err != nil {
		return "", "", fmt.Errorf("trash: preparing fallback backend: %w", err)
	}
	trashed, err = fb.move(abs, name)
	if err != nil {
		return "", "", fmt.Errorf("trash: moving %s via fallback: %w", abs, err)
	}
	return trashed, BackendFallback, nil
}

func (s *Store) manifestDir() string  { return filepath.Join(s.dataDir, "trash") }
func (s *Store) manifestPath() string { return filepath.Join(s.manifestDir(), "manifest.jsonl") }

// removeEntries rewrites the manifest without the given entry IDs.
func (s *Store) removeEntries(restored []Item) error {
	entries, err := readManifest(s.manifestPath())
	if err != nil {
		return err
	}
	removed := make(map[string]struct{}, len(restored))
	for _, item := range restored {
		removed[item.ID] = struct{}{}
	}
	kept := entries[:0:0]
	for _, e := range entries {
		if _, ok := removed[e.ID]; !ok {
			kept = append(kept, e)
		}
	}
	return rewriteManifest(s.manifestPath(), kept)
}

// newBatchID returns a time-prefixed identifier with a random suffix.
func newBatchID(now time.Time) string {
	var rnd [4]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		// Randomness is only for collision avoidance; the timestamp
		// prefix already makes batches unique within a process.
		rnd[0] = byte(now.Nanosecond())
	}
	return fmt.Sprintf("%s-%s", now.Format("20060102T150405"), hex.EncodeToString(rnd[:]))
}

// uniqueName returns name, or name (2), (3), ... while the target
// exists in the backend's destination directory.
func uniqueName(b backend, name string) string {
	for i := 1; ; i++ {
		candidate := name
		if i > 1 {
			candidate = fmt.Sprintf("%s (%d)%s", strings.TrimSuffix(name, filepath.Ext(name)), i, filepath.Ext(name))
		}
		if _, err := os.Lstat(filepath.Join(destinationRoot(b), candidate)); err != nil {
			return candidate
		}
	}
}

// destinationRoot exposes where a backend places trashed files; used
// only for collision detection.
func destinationRoot(b backend) string {
	if withRoot, ok := b.(interface{ destRoot() string }); ok {
		return withRoot.destRoot()
	}
	return ""
}

// restorePath picks the restore destination: the original path when
// free, otherwise a .restored-<unix> sibling so user files are never
// overwritten.
func restorePath(original string, now time.Time) (string, error) {
	if _, err := os.Lstat(original); errors.Is(err, os.ErrNotExist) {
		return original, nil
	} else if err != nil {
		return "", fmt.Errorf("trash: checking %s: %w", original, err)
	}
	return original + ".restored-" + strconvFormatInt(now.Unix()), nil
}

func isCrossDevice(err error) bool {
	return errors.Is(err, syscall.EXDEV)
}

// copyPath copies a file or directory tree, preserving modes. Used by
// cross-device fallbacks where rename is not possible.
func copyPath(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return copyFile(src, dst, info)
	}
	if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := copyPath(filepath.Join(src, entry.Name()), filepath.Join(dst, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string, info os.FileInfo) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
