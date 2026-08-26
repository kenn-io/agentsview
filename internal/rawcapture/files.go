package rawcapture

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawcheckpoint"
	"go.kenn.io/agentsview/internal/rawsync"
)

type fileOperations struct {
	stat    func(string) (os.FileInfo, error)
	rename  func(string, string) error
	remove  func(string) error
	syncDir func(string) error
}

type scopedCaptureEntry struct {
	planned  parser.RawCaptureEntry
	root     *os.Root
	relative string
}

type capturePlanScope struct {
	entries        []scopedCaptureEntry
	roots          []*os.Root
	captureInfo    os.FileInfo
	configuredInfo os.FileInfo
}

func openCapturePlanScope(plan parser.RawCapturePlan) (*capturePlanScope, error) {
	captureRoot, err := os.OpenRoot(plan.CaptureRoot)
	if err != nil {
		return nil, fmt.Errorf("rawcapture: open capture root: filesystem error")
	}
	captureInfo, err := captureRoot.Stat(".")
	if err != nil {
		_ = captureRoot.Close()
		return nil, fmt.Errorf("rawcapture: stat capture root: filesystem error")
	}
	scope := &capturePlanScope{
		roots: []*os.Root{captureRoot}, captureInfo: captureInfo,
		configuredInfo: captureInfo,
	}
	configuredRoot := captureRoot
	if plan.ConfiguredRoot != plan.CaptureRoot {
		configuredRoot, err = os.OpenRoot(plan.ConfiguredRoot)
		if err != nil {
			_ = captureRoot.Close()
			return nil, fmt.Errorf("rawcapture: open configured root: filesystem error")
		}
		scope.roots = append(scope.roots, configuredRoot)
		scope.configuredInfo, err = configuredRoot.Stat(".")
		if err != nil {
			_ = scope.Close()
			return nil, fmt.Errorf("rawcapture: stat configured root: filesystem error")
		}
	}
	for _, planned := range plan.Entries {
		root := captureRoot
		relative, ok := relativeWithinRoot(plan.CaptureRoot, planned.LocalPath)
		if !ok {
			root = configuredRoot
			relative, ok = relativeWithinRoot(plan.ConfiguredRoot, planned.LocalPath)
		}
		if !ok {
			_ = scope.Close()
			return nil, ErrSourceChanged
		}
		scope.entries = append(scope.entries, scopedCaptureEntry{
			planned: planned, root: root, relative: relative,
		})
	}
	return scope, nil
}

func relativeWithinRoot(root, path string) (string, bool) {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.Clean(relative), true
}

func (s *capturePlanScope) Close() error {
	var closeErr error
	for _, root := range s.roots {
		if err := root.Close(); err != nil {
			closeErr = errors.Join(closeErr, sanitizeFilesystemError(err))
		}
	}
	s.roots = nil
	return closeErr
}

func (s *capturePlanScope) MatchesRoots(plan parser.RawCapturePlan) bool {
	captureInfo, err := os.Stat(plan.CaptureRoot)
	if err != nil || !os.SameFile(s.captureInfo, captureInfo) {
		return false
	}
	configuredInfo, err := os.Stat(plan.ConfiguredRoot)
	return err == nil && os.SameFile(s.configuredInfo, configuredInfo)
}

func defaultFileOperations() fileOperations {
	return fileOperations{
		stat: os.Stat, rename: os.Rename, remove: os.Remove, syncDir: syncDirectory,
	}
}

func (c *Capturer) captureFile(
	ctx context.Context,
	observed observedCaptureEntry,
) (entry rawcheckpoint.CapturedEntry, installed bool, resultErr error) {
	if err := ctx.Err(); err != nil {
		return rawcheckpoint.CapturedEntry{}, false, err
	}
	planned := observed.planned
	source := observed.file
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return rawcheckpoint.CapturedEntry{}, false, fmt.Errorf("rawcapture: seek %q: filesystem error", planned.Path)
	}
	before, err := source.Stat()
	if err != nil {
		return rawcheckpoint.CapturedEntry{}, false, fmt.Errorf("rawcapture: stat %q: filesystem error", planned.Path)
	}
	beforeIdentity := stableFileIdentity(source, before)
	if !before.Mode().IsRegular() || !stableFileInfo(observed.info, before) ||
		beforeIdentity == "" || beforeIdentity != observed.identity {
		return rawcheckpoint.CapturedEntry{}, false, ErrSourceChanged
	}
	temporary, err := os.CreateTemp(c.store.CaptureTempDir(), "capture-*")
	if err != nil {
		return rawcheckpoint.CapturedEntry{}, false, fmt.Errorf(
			"rawcapture: create temporary object: %w", sanitizeFilesystemError(err),
		)
	}
	temporaryPath := temporary.Name()
	defer func() {
		if err := c.files.remove(temporaryPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			resultErr = errors.Join(resultErr, fmt.Errorf(
				"%w: remove temporary object: %w",
				errCleanupIncomplete, sanitizeFilesystemError(err),
			))
		}
	}()

	hash := sha256.New()
	written, err := copyContext(ctx, io.MultiWriter(temporary, hash), source)
	if err != nil {
		temporary.Close()
		return rawcheckpoint.CapturedEntry{}, false, fmt.Errorf(
			"rawcapture: read %q: %w", planned.Path, sanitizeFilesystemError(err),
		)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return rawcheckpoint.CapturedEntry{}, false, fmt.Errorf(
			"rawcapture: sync temporary object: %w", sanitizeFilesystemError(err),
		)
	}
	if err := temporary.Close(); err != nil {
		return rawcheckpoint.CapturedEntry{}, false, fmt.Errorf(
			"rawcapture: close temporary object: %w", sanitizeFilesystemError(err),
		)
	}
	if c.capturePhase != nil {
		c.capturePhase(capturePhaseAfterRead, planned.LocalPath)
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	afterHandle, err := source.Stat()
	if err != nil {
		return rawcheckpoint.CapturedEntry{}, false, fmt.Errorf("rawcapture: restat %q: filesystem error", planned.Path)
	}
	afterFile, err := observed.root.Open(observed.relative)
	if err != nil {
		return rawcheckpoint.CapturedEntry{}, false, ErrSourceChanged
	}
	afterPath, pathErr := afterFile.Stat()
	afterIdentity := ""
	if pathErr == nil {
		afterIdentity = stableFileIdentity(afterFile, afterPath)
	}
	_ = afterFile.Close()
	if pathErr != nil || !os.SameFile(before, afterPath) || !stableFileInfo(before, afterHandle) ||
		afterIdentity != beforeIdentity ||
		written != before.Size() {
		return rawcheckpoint.CapturedEntry{}, false, ErrSourceChanged
	}
	verifiedDigest, verifiedBytes, err := hashScopedFileContext(
		ctx, observed.root, observed.relative,
	)
	if err != nil || verifiedBytes != written || verifiedDigest != digest {
		return rawcheckpoint.CapturedEntry{}, false, ErrSourceChanged
	}

	ref, err := rawsync.NewObjectRef(digest, written)
	if err != nil {
		return rawcheckpoint.CapturedEntry{}, false, err
	}
	entry = rawcheckpoint.CapturedEntry{
		Path:         planned.Path,
		Length:       written,
		ModTimeNS:    before.ModTime().UnixNano(),
		FileIdentity: beforeIdentity,
		PrefixSHA256: digest,
		Appendable:   planned.Appendable,
		Objects:      []rawsync.ObjectRef{ref},
	}
	installed, err = c.installObject(ctx, temporaryPath, ref)
	return entry, installed, err
}

func (c *Capturer) captureAppendFile(
	ctx context.Context,
	observed observedCaptureEntry,
	base rawcheckpoint.CapturedEntry,
) (entry rawcheckpoint.CapturedEntry, installed bool, resultErr error) {
	if err := ctx.Err(); err != nil {
		return rawcheckpoint.CapturedEntry{}, false, err
	}
	planned := observed.planned
	source := observed.file
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return rawcheckpoint.CapturedEntry{}, false, fmt.Errorf("rawcapture: seek %q: filesystem error", planned.Path)
	}
	before, err := source.Stat()
	if err != nil {
		return rawcheckpoint.CapturedEntry{}, false, ErrSourceChanged
	}
	beforeIdentity := stableFileIdentity(source, before)
	if !before.Mode().IsRegular() || !stableFileInfo(observed.info, before) ||
		before.Size() <= base.Length ||
		beforeIdentity == "" || beforeIdentity != observed.identity ||
		beforeIdentity != base.FileIdentity {
		return rawcheckpoint.CapturedEntry{}, false, ErrSourceChanged
	}
	temporary, err := os.CreateTemp(c.store.CaptureTempDir(), "capture-append-*")
	if err != nil {
		return rawcheckpoint.CapturedEntry{}, false, fmt.Errorf(
			"rawcapture: create temporary object: %w", sanitizeFilesystemError(err),
		)
	}
	temporaryPath := temporary.Name()
	defer func() {
		if err := c.files.remove(temporaryPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			resultErr = errors.Join(resultErr, fmt.Errorf(
				"%w: remove temporary object: %w",
				errCleanupIncomplete, sanitizeFilesystemError(err),
			))
		}
	}()

	prefixHash := sha256.New()
	fullHash := sha256.New()
	prefixBytes, err := copyContext(
		ctx, io.MultiWriter(prefixHash, fullHash), io.LimitReader(source, base.Length),
	)
	if err != nil || prefixBytes != base.Length ||
		hex.EncodeToString(prefixHash.Sum(nil)) != base.PrefixSHA256 {
		temporary.Close()
		return rawcheckpoint.CapturedEntry{}, false, ErrSourceChanged
	}
	suffixHash := sha256.New()
	suffixBytes, err := copyContext(ctx, io.MultiWriter(temporary, suffixHash, fullHash), source)
	if err != nil || suffixBytes != before.Size()-base.Length {
		temporary.Close()
		return rawcheckpoint.CapturedEntry{}, false, ErrSourceChanged
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return rawcheckpoint.CapturedEntry{}, false, fmt.Errorf(
			"rawcapture: sync temporary object: %w", sanitizeFilesystemError(err),
		)
	}
	if err := temporary.Close(); err != nil {
		return rawcheckpoint.CapturedEntry{}, false, fmt.Errorf(
			"rawcapture: close temporary object: %w", sanitizeFilesystemError(err),
		)
	}
	if c.capturePhase != nil {
		c.capturePhase(capturePhaseAfterRead, planned.LocalPath)
	}
	afterHandle, err := source.Stat()
	if err != nil {
		return rawcheckpoint.CapturedEntry{}, false, fmt.Errorf("rawcapture: restat %q: filesystem error", planned.Path)
	}
	afterFile, err := observed.root.Open(observed.relative)
	if err != nil {
		return rawcheckpoint.CapturedEntry{}, false, ErrSourceChanged
	}
	afterPath, pathErr := afterFile.Stat()
	afterIdentity := ""
	if pathErr == nil {
		afterIdentity = stableFileIdentity(afterFile, afterPath)
	}
	_ = afterFile.Close()
	if pathErr != nil || !os.SameFile(before, afterPath) || !stableFileInfo(before, afterHandle) ||
		afterIdentity != beforeIdentity {
		return rawcheckpoint.CapturedEntry{}, false, ErrSourceChanged
	}
	fullDigest := hex.EncodeToString(fullHash.Sum(nil))
	verifiedDigest, verifiedBytes, err := hashScopedFileContext(
		ctx, observed.root, observed.relative,
	)
	if err != nil || verifiedBytes != before.Size() || verifiedDigest != fullDigest {
		return rawcheckpoint.CapturedEntry{}, false, ErrSourceChanged
	}
	ref, err := rawsync.NewObjectRef(hex.EncodeToString(suffixHash.Sum(nil)), suffixBytes)
	if err != nil {
		return rawcheckpoint.CapturedEntry{}, false, err
	}
	entry = cloneCapturedEntry(base)
	entry.Length = before.Size()
	entry.ModTimeNS = before.ModTime().UnixNano()
	entry.FileIdentity = beforeIdentity
	entry.PrefixSHA256 = fullDigest
	entry.Objects = append(entry.Objects, ref)
	installed, err = c.installObject(ctx, temporaryPath, ref)
	return entry, installed, err
}

func (c *Capturer) captureReusedFile(
	ctx context.Context,
	observed observedCaptureEntry,
	base rawcheckpoint.CapturedEntry,
) (rawcheckpoint.CapturedEntry, error) {
	planned := observed.planned
	file := observed.file
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return rawcheckpoint.CapturedEntry{}, fmt.Errorf(
			"rawcapture: seek %q: filesystem error", planned.Path,
		)
	}
	before, err := file.Stat()
	if err != nil {
		return rawcheckpoint.CapturedEntry{}, ErrSourceChanged
	}
	identity := stableFileIdentity(file, before)
	if !before.Mode().IsRegular() || before.Size() != base.Length ||
		identity == "" || identity != observed.identity || identity != base.FileIdentity {
		return rawcheckpoint.CapturedEntry{}, ErrSourceChanged
	}
	hash := sha256.New()
	length, err := copyContext(ctx, hash, file)
	if err != nil {
		return rawcheckpoint.CapturedEntry{}, fmt.Errorf(
			"rawcapture: read %q: %w", planned.Path, sanitizeFilesystemError(err),
		)
	}
	afterHandle, err := file.Stat()
	if err != nil {
		return rawcheckpoint.CapturedEntry{}, fmt.Errorf(
			"rawcapture: restat %q: filesystem error", planned.Path,
		)
	}
	afterFile, err := observed.root.Open(observed.relative)
	if err != nil {
		return rawcheckpoint.CapturedEntry{}, ErrSourceChanged
	}
	afterPath, pathErr := afterFile.Stat()
	afterIdentity := ""
	if pathErr == nil {
		afterIdentity = stableFileIdentity(afterFile, afterPath)
	}
	_ = afterFile.Close()
	if pathErr != nil || !os.SameFile(before, afterPath) || !stableFileInfo(before, afterHandle) ||
		afterIdentity != identity ||
		length != base.Length || hex.EncodeToString(hash.Sum(nil)) != base.PrefixSHA256 {
		return rawcheckpoint.CapturedEntry{}, ErrSourceChanged
	}
	entry := cloneCapturedEntry(base)
	entry.ModTimeNS = before.ModTime().UnixNano()
	entry.FileIdentity = identity
	return entry, nil
}

func (c *Capturer) verifyCapturedFile(
	ctx context.Context,
	observed observedCaptureEntry,
	captured capturedFileState,
) error {
	planned := observed.planned
	file, err := observed.root.Open(observed.relative)
	if err != nil {
		return ErrSourceChanged
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return ErrSourceChanged
	}
	identity := stableFileIdentity(file, before)
	if !before.Mode().IsRegular() || !stableFileInfo(observed.info, before) ||
		before.Size() != captured.length || before.ModTime().UnixNano() != captured.modTimeNS ||
		identity == "" || identity != observed.identity || identity != captured.fileIdentity {
		return ErrSourceChanged
	}
	hash := sha256.New()
	length, err := copyContext(ctx, hash, file)
	if err != nil {
		return fmt.Errorf("rawcapture: validate %q: %w", planned.Path, sanitizeFilesystemError(err))
	}
	afterHandle, err := file.Stat()
	if err != nil {
		return fmt.Errorf("rawcapture: restat %q after final validation: filesystem error", planned.Path)
	}
	afterFile, err := observed.root.Open(observed.relative)
	if err != nil {
		return ErrSourceChanged
	}
	afterPath, pathErr := afterFile.Stat()
	afterIdentity := ""
	if pathErr == nil {
		afterIdentity = stableFileIdentity(afterFile, afterPath)
	}
	_ = afterFile.Close()
	if pathErr != nil || !os.SameFile(before, afterPath) || !stableFileInfo(before, afterHandle) ||
		afterIdentity != identity ||
		length != captured.length || hex.EncodeToString(hash.Sum(nil)) != captured.prefixSHA256 {
		return ErrSourceChanged
	}
	return nil
}

func (c *Capturer) installObject(
	ctx context.Context,
	temporaryPath string,
	ref rawsync.ObjectRef,
) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	destination := c.store.ObjectPath(ref)
	if info, err := c.files.stat(destination); err == nil {
		if !info.Mode().IsRegular() || info.Size() != ref.Length {
			return false, fmt.Errorf("rawcapture: existing object has conflicting size")
		}
		digest, length, err := hashFileContext(ctx, destination)
		if err != nil || digest != ref.SHA256 || length != ref.Length {
			return false, fmt.Errorf("rawcapture: existing object failed verification")
		}
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf(
			"rawcapture: stat object destination: %w", sanitizeFilesystemError(err),
		)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return false, fmt.Errorf(
			"rawcapture: create object directory: %w", sanitizeFilesystemError(err),
		)
	}
	if err := c.syncObjectDirectoryHierarchy(); err != nil {
		return false, err
	}
	if err := c.files.rename(temporaryPath, destination); err != nil {
		return false, fmt.Errorf(
			"rawcapture: install object: %w", sanitizeFilesystemError(err),
		)
	}
	if err := c.files.syncDir(filepath.Dir(destination)); err != nil {
		return true, fmt.Errorf("rawcapture: sync object directory: filesystem error")
	}
	return true, nil
}

func (c *Capturer) syncObjectDirectoryHierarchy() error {
	spoolDir := filepath.Dir(c.store.CaptureTempDir())
	for _, directory := range []string{
		filepath.Dir(spoolDir),
		spoolDir,
		filepath.Join(spoolDir, "objects"),
		filepath.Join(spoolDir, "objects", "sha256"),
	} {
		if err := c.files.syncDir(directory); err != nil {
			return fmt.Errorf("rawcapture: sync object directory hierarchy: filesystem error")
		}
	}
	return nil
}

func stableFileInfo(a, b os.FileInfo) bool {
	return a.Mode() == b.Mode() && a.Size() == b.Size() &&
		a.ModTime().Equal(b.ModTime()) && os.SameFile(a, b)
}

func copyContext(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	buffer := make([]byte, 64<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		read, readErr := source.Read(buffer)
		if read > 0 {
			written, writeErr := destination.Write(buffer[:read])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
			if written != read {
				return total, io.ErrShortWrite
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return total, nil
			}
			return total, readErr
		}
	}
}

func hashFileContext(ctx context.Context, path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	length, err := copyContext(ctx, hash, file)
	if err != nil {
		return "", length, err
	}
	return hex.EncodeToString(hash.Sum(nil)), length, nil
}

func hashScopedFileContext(
	ctx context.Context,
	root *os.Root,
	relative string,
) (string, int64, error) {
	file, err := root.Open(relative)
	if err != nil {
		return "", 0, ErrSourceChanged
	}
	defer file.Close()
	return hashOpenFileContext(ctx, file)
}

func hashOpenFileContext(ctx context.Context, file *os.File) (string, int64, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", 0, err
	}
	hash := sha256.New()
	length, err := copyContext(ctx, hash, file)
	if err != nil {
		return "", length, err
	}
	return hex.EncodeToString(hash.Sum(nil)), length, nil
}

func hashOpenFilePrefixContext(
	ctx context.Context,
	file *os.File,
	limit int64,
) (string, int64, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", 0, err
	}
	hash := sha256.New()
	length, err := copyContext(ctx, hash, io.LimitReader(file, limit))
	if err != nil {
		return "", length, err
	}
	return hex.EncodeToString(hash.Sum(nil)), length, nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil && runtime.GOOS != "windows" {
		// Windows does not support syncing directory handles. The atomic rename
		// remains the durability boundary available there.
		return err
	}
	return nil
}

func sanitizeFilesystemError(err error) error {
	if pathErr, ok := errors.AsType[*os.PathError](err); ok {
		return pathErr.Err
	}
	if linkErr, ok := errors.AsType[*os.LinkError](err); ok {
		return linkErr.Err
	}
	return err
}
