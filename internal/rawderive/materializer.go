package rawderive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/rawsync"
)

// ObjectStore provides authenticated access to immutable custody objects.
type ObjectStore interface {
	CopyObject(
		context.Context,
		string,
		rawsync.ObjectRef,
		io.Writer,
	) (rawsync.ObjectInfo, error)
}

// Materializer reconstructs one canonical source generation in an isolated
// filesystem tree.
type Materializer struct {
	Store         ObjectStore
	BaseDir       string
	MaxTotalBytes int64
	// removeTree overrides tree removal for tests. Production removal
	// chmods directories writable again and removes the private tree.
	removeTree func(string) error
}

// Materialization owns one private, read-only reconstructed source tree.
type Materialization struct {
	root       string
	entries    map[string]string
	removeTree func(string) error
	mu         sync.Mutex
}

// Root returns the private materialization root.
func (m *Materialization) Root() string {
	if m == nil {
		return ""
	}
	return m.root
}

// EntryPath resolves a canonical manifest entry inside the private tree.
func (m *Materialization) EntryPath(entry string) (string, error) {
	if m == nil {
		return "", fmt.Errorf("%w: materialization is missing", rawsync.ErrInvalid)
	}
	path, ok := m.entries[entry]
	if !ok {
		return "", fmt.Errorf("%w: materialized entry %q", rawsync.ErrNotFound, entry)
	}
	return path, nil
}

var errMaterializationCleanup = errors.New("materialization cleanup failed")

// Cleanup removes the private tree. It is safe to call more than once and
// stays retryable: a transient removal failure is reported without latching,
// so a later call can succeed once the underlying condition clears. Errors
// never expose the raw tree location, and every failure carries
// errMaterializationCleanup so callers can report the cleanup stage
// distinctly in sanitized diagnostics.
func (m *Materialization) Cleanup() error {
	if m == nil || m.root == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	remove := m.removeTree
	if remove == nil {
		remove = removeMaterializationTree
	}
	if err := remove(m.root); err != nil {
		return fmt.Errorf("%w: cleaning raw materialization: %w",
			errMaterializationCleanup,
			redactedProviderError{
				message: strings.ReplaceAll(err.Error(), m.root, "<materialized>"),
				cause:   err,
			},
		)
	}
	return nil
}

func removeMaterializationTree(root string) error {
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err == nil && entry.IsDir() {
			_ = os.Chmod(path, 0o700)
		}
		return nil
	})
	return os.RemoveAll(root)
}

// Materialize verifies and reconstructs every object in manifest before making
// the resulting source tree read-only.
func (m Materializer) Materialize(
	ctx context.Context,
	manifest rawsync.CanonicalManifest,
) (_ *Materialization, resultErr error) {
	if m.Store == nil {
		return nil, fmt.Errorf("%w: object store is required", rawsync.ErrInvalid)
	}
	if strings.TrimSpace(m.BaseDir) == "" {
		return nil, fmt.Errorf("%w: materialization base directory is required", rawsync.ErrInvalid)
	}
	if m.MaxTotalBytes <= 0 {
		return nil, fmt.Errorf("%w: materialization byte limit must be positive", rawsync.ErrInvalid)
	}
	if err := rawsync.ValidateCanonicalManifest(manifest); err != nil {
		return nil, err
	}
	var total int64
	for _, entry := range manifest.Manifest.Entries {
		if entry.Length > m.MaxTotalBytes-total {
			return nil, fmt.Errorf(
				"%w: materialization exceeds %d bytes", rawsync.ErrInvalid, m.MaxTotalBytes,
			)
		}
		total += entry.Length
	}

	root, err := os.MkdirTemp(m.BaseDir, "agentsview-raw-")
	if err != nil {
		return nil, fmt.Errorf("creating raw materialization: %w", err)
	}
	result := &Materialization{
		root: root, entries: make(map[string]string), removeTree: m.removeTree,
	}
	defer func() {
		if resultErr != nil {
			if cleanupErr := result.Cleanup(); cleanupErr != nil {
				resultErr = errors.Join(resultErr, cleanupErr)
			}
		}
	}()

	for _, entry := range manifest.Manifest.Entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		target := filepath.Join(root, filepath.FromSlash(entry.Path))
		if err := ensureMaterializedPath(root, target); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return nil, fmt.Errorf("creating raw materialization directory: %w", err)
		}
		if err := m.materializeEntry(
			ctx, manifest.Identity.TenantID, entry, manifest.Manifest.CapturedAt, target,
		); err != nil {
			return nil, err
		}
		result.entries[entry.Path] = target
	}
	if err := makeMaterializationReadOnly(root); err != nil {
		return nil, err
	}
	return result, nil
}

func (m Materializer) materializeEntry(
	ctx context.Context,
	tenantID string,
	entry rawsync.Entry,
	capturedAt time.Time,
	target string,
) (resultErr error) {
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("creating materialized raw entry: %w", err)
	}
	defer func() {
		if file == nil {
			return
		}
		if err := file.Close(); err != nil && resultErr == nil {
			resultErr = fmt.Errorf("closing materialized raw entry: %w", err)
		}
	}()

	var written int64
	for _, object := range entry.Objects {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := m.Store.CopyObject(ctx, tenantID, object, file)
		if err != nil {
			return fmt.Errorf("copying raw object %s: %w", object.SHA256, err)
		}
		if info.Ref != object {
			return fmt.Errorf("%w: raw object identity is inconsistent", rawsync.ErrInvalid)
		}
		written += object.Length
	}
	if written != entry.Length {
		return fmt.Errorf("%w: materialized entry length is inconsistent", rawsync.ErrInvalid)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("closing materialized raw entry: %w", err)
	}
	file = nil
	// Restore the captured source modification time so downstream parsers
	// cannot observe the worker-local creation time. Generations captured
	// before mod-time carriage normalize to the capture instant instead.
	modTime := capturedAt
	if entry.ModTimeNS != 0 {
		modTime = time.Unix(0, entry.ModTimeNS)
	}
	if err := os.Chtimes(target, modTime, modTime); err != nil {
		return fmt.Errorf("restoring materialized raw entry mod time: %w", err)
	}
	if err := os.Chmod(target, 0o400); err != nil {
		return fmt.Errorf("making materialized raw entry read-only: %w", err)
	}
	return nil
}

func ensureMaterializedPath(root, target string) error {
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == "." || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return fmt.Errorf("%w: materialized entry escapes its root", rawsync.ErrInvalid)
	}
	return nil
}

func makeMaterializationReadOnly(root string) error {
	var directories []string
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			directories = append(directories, path)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("walking raw materialization: %w", err)
	}
	for _, directory := range slices.Backward(directories) {
		if err := os.Chmod(directory, 0o500); err != nil {
			return fmt.Errorf("making raw materialization read-only: %w", err)
		}
	}
	return nil
}
