package artifact

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

const (
	folderMarkerName       = ".agentsview-artifacts.json"
	folderMarkerMaxBytes   = int64(4 << 10)
	folderFormatName       = "agentsview-normalized-artifacts"
	folderFormatVersion    = 2
	folderMarkerTempPrefix = ".agentsview-artifacts.tmp-"
	folderExchangeLockName = ".agentsview-artifacts.lock"
)

var folderMarkerBody = []byte(
	"{\"format\":\"agentsview-normalized-artifacts\",\"version\":2}\n",
)

type folderMarker struct {
	Format  string `json:"format"`
	Version int    `json:"version"`
}

type folderTransport struct {
	mu sync.Mutex

	target       string
	root         *os.Root
	rootIdentity fs.FileInfo
	closed       bool

	observeDirectoryPage func(int)
	observeStorePage     func(int)
	publishLink          func(*os.Root, string, string) error
}

// OpenFolderTransport opens or initializes a marked artifact exchange target.
// Existing nonempty directories are never adopted without a valid marker.
func OpenFolderTransport(
	target string,
	opts FolderTransportOptions,
) (_ Transport, retErr error) {
	if strings.TrimSpace(target) == "" {
		return nil, fmt.Errorf("%w: artifact folder target is required", ErrArtifactInvalid)
	}
	canonical, err := canonicalArtifactPath(target)
	if err != nil {
		return nil, fmt.Errorf("resolving artifact folder target: %w", err)
	}
	if err := rejectFolderTargetOverlap(canonical, opts.ForbiddenRoots); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(canonical, 0o755); err != nil {
		return nil, fmt.Errorf("creating artifact folder target: %w", err)
	}
	root, identity, err := openFolderTargetRoot(canonical)
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, root.Close())
		}
	}()
	transport := &folderTransport{
		target:       canonical,
		root:         root,
		rootIdentity: identity,
	}
	if err := transport.prepareMarker(); err != nil {
		return nil, err
	}
	return transport, nil
}

func (t *folderTransport) Prepare(ctx context.Context, _ ArtifactStore) error {
	if ctx == nil {
		return fmt.Errorf("%w: artifact transport context is required", ErrArtifactInvalid)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.root == nil {
		return fs.ErrClosed
	}
	return t.prepareLocked()
}

func (t *folderTransport) Exchange(
	ctx context.Context,
	store ArtifactStore,
	publishOrigin string,
) (result ExchangeResult, retErr error) {
	if ctx == nil {
		return ExchangeResult{}, fmt.Errorf(
			"%w: artifact transport context is required",
			ErrArtifactInvalid,
		)
	}
	if store == nil {
		return ExchangeResult{}, fmt.Errorf(
			"%w: artifact store is required",
			ErrArtifactInvalid,
		)
	}
	if err := validateOriginID(publishOrigin); err != nil {
		return ExchangeResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return ExchangeResult{}, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.root == nil {
		return ExchangeResult{}, fs.ErrClosed
	}
	lock, err := t.acquireExchangeLockLocked(ctx)
	if err != nil {
		return ExchangeResult{}, err
	}
	defer func() {
		retErr = errors.Join(retErr, lock.Close())
	}()
	if err := t.prepareLocked(); err != nil {
		return ExchangeResult{}, err
	}
	result, err = t.pullLocked(ctx, store, publishOrigin)
	if err != nil {
		return result, err
	}
	published, err := t.pushLocked(ctx, store, publishOrigin)
	result.Published += published
	if err != nil {
		return result, err
	}
	if err := t.verifyRootIdentityLocked(); err != nil {
		return result, err
	}
	return result, nil
}

func (t *folderTransport) acquireExchangeLockLocked(
	ctx context.Context,
) (*flock.Flock, error) {
	lock := flock.New(filepath.Join(t.target, folderExchangeLockName))
	locked, err := lock.TryLockContext(ctx, 100*time.Millisecond)
	if err != nil {
		_ = lock.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("acquiring artifact folder exchange lock: %w", err)
	}
	if !locked {
		_ = lock.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, errors.New("artifact folder exchange is already running")
	}
	return lock, nil
}

func (t *folderTransport) Close() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	t.closed = true
	if t.root == nil {
		return nil
	}
	err := t.root.Close()
	t.root = nil
	return err
}

func (t *folderTransport) prepareMarker() error {
	found, err := folderMarkerExists(t.root)
	if err != nil {
		return err
	}
	if found {
		return validateFolderMarker(t.root)
	}
	empty, err := folderRootEmpty(t.root)
	if err != nil {
		return err
	}
	if !empty {
		return fmt.Errorf(
			"%w: target is not an agentsview artifact target",
			ErrArtifactInvalid,
		)
	}
	if err := createFolderMarker(t.root); err != nil {
		return fmt.Errorf("initializing agentsview artifact target: %w", err)
	}
	return validateFolderMarker(t.root)
}

func (t *folderTransport) verifyRootIdentityLocked() error {
	opened, err := t.root.Stat(".")
	if err != nil {
		return fmt.Errorf("stating opened artifact folder target: %w", err)
	}
	current, err := os.Stat(t.target)
	if err != nil {
		return fmt.Errorf("stating artifact folder target: %w", err)
	}
	if !current.IsDir() ||
		!os.SameFile(t.rootIdentity, opened) ||
		!os.SameFile(opened, current) {
		return errors.New("artifact folder target changed while open")
	}
	return nil
}

func (t *folderTransport) prepareLocked() error {
	if err := t.verifyRootIdentityLocked(); err != nil {
		return err
	}
	return validateFolderMarker(t.root)
}

func openFolderTargetRoot(path string) (*os.Root, fs.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("stating artifact folder target: %w", err)
	}
	if !info.IsDir() {
		return nil, nil, errors.New("artifact folder target is not a directory")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, nil, fmt.Errorf("opening artifact folder target: %w", err)
	}
	opened, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, nil, fmt.Errorf("stating opened artifact folder target: %w", err)
	}
	current, err := os.Stat(path)
	if err != nil || !current.IsDir() || !os.SameFile(opened, current) {
		_ = root.Close()
		if err != nil {
			return nil, nil, fmt.Errorf("restating artifact folder target: %w", err)
		}
		return nil, nil, errors.New("artifact folder target changed while opening")
	}
	return root, opened, nil
}

func rejectFolderTargetOverlap(target string, forbidden []string) error {
	for _, candidate := range forbidden {
		if strings.TrimSpace(candidate) == "" {
			continue
		}
		canonical, err := canonicalArtifactPath(candidate)
		if err != nil {
			return fmt.Errorf("resolving protected artifact root: %w", err)
		}
		overlap, err := pathsOverlap(target, canonical)
		if err != nil {
			return fmt.Errorf("checking protected artifact root: %w", err)
		}
		if overlap {
			return fmt.Errorf(
				"%w: artifact folder target overlaps a protected root",
				ErrArtifactInvalid,
			)
		}
	}
	return nil
}

func pathsOverlap(left, right string) (bool, error) {
	if pathContains(left, right) || pathContains(right, left) {
		return true, nil
	}
	// Filesystem case sensitivity can vary by mount and even by directory, so
	// fail closed for case aliases on every platform. Identity checks below
	// still cover non-textual aliases such as symlinks.
	foldedLeft := strings.ToLower(left)
	foldedRight := strings.ToLower(right)
	if pathContains(foldedLeft, foldedRight) ||
		pathContains(foldedRight, foldedLeft) {
		return true, nil
	}
	leftContains, err := pathContainsByIdentity(left, right)
	if err != nil || leftContains {
		return leftContains, err
	}
	return pathContainsByIdentity(right, left)
}

func pathContains(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return relative == "." ||
		(relative != ".." &&
			!strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func pathContainsByIdentity(parent, child string) (bool, error) {
	parentInfo, err := os.Stat(parent)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	}
	current := child
	for {
		info, err := os.Stat(current)
		switch {
		case err == nil && os.SameFile(parentInfo, info):
			return true, nil
		case err == nil, errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return false, err
		}
		next := filepath.Dir(current)
		if next == current {
			return false, nil
		}
		current = next
	}
}

func folderMarkerExists(root *os.Root) (bool, error) {
	info, err := root.Lstat(folderMarkerName)
	switch {
	case err == nil:
		if !info.Mode().IsRegular() {
			return false, errors.New("invalid agentsview artifact target marker")
		}
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("stating agentsview artifact target marker: %w", err)
	}
}

func folderRootEmpty(root *os.Root) (bool, error) {
	directory, err := root.Open(".")
	if err != nil {
		return false, fmt.Errorf("opening artifact target directory: %w", err)
	}
	defer directory.Close()
	entries, err := directory.ReadDir(1)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("reading artifact target directory: %w", err)
	}
	return len(entries) == 0, nil
}

func validateFolderMarker(root *os.Root) error {
	file, _, err := openFolderRegularFile(root, folderMarkerName)
	if err != nil {
		return fmt.Errorf("invalid agentsview artifact target marker: %w", err)
	}
	defer file.Close()

	limited := io.LimitReader(file, folderMarkerMaxBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Errorf("invalid agentsview artifact target marker: %w", err)
	}
	if int64(len(body)) > folderMarkerMaxBytes {
		return errors.New("invalid agentsview artifact target marker: marker is too large")
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	var marker folderMarker
	if err := decoder.Decode(&marker); err != nil {
		return fmt.Errorf("invalid agentsview artifact target marker: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("invalid agentsview artifact target marker: trailing content")
	}
	if marker.Format != folderFormatName || marker.Version != folderFormatVersion {
		return errors.New("invalid agentsview artifact target marker: unsupported format")
	}
	return nil
}

func createFolderMarker(root *os.Root) (retErr error) {
	tempName, file, err := createFolderTemp(root, folderMarkerTempPrefix)
	if err != nil {
		return err
	}
	tempExists := true
	defer func() {
		if tempExists {
			retErr = errors.Join(retErr, removeFolderFile(root, tempName))
		}
	}()
	if _, err := file.Write(folderMarkerBody); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Sync(); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Close(); err != nil {
		return err
	}

	if err := root.Link(tempName, folderMarkerName); err != nil {
		if fallbackErr := createFolderMarkerExclusive(root); fallbackErr != nil {
			return errors.Join(err, fallbackErr)
		}
	}
	if err := root.Remove(tempName); err != nil {
		return err
	}
	tempExists = false
	syncFolderDirectoryBestEffort(root)
	return nil
}

func createFolderMarkerExclusive(root *os.Root) error {
	file, err := root.OpenFile(
		folderMarkerName,
		os.O_WRONLY|os.O_CREATE|os.O_EXCL,
		0o600,
	)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return validateFolderMarker(root)
		}
		return err
	}
	if _, err := file.Write(folderMarkerBody); err != nil {
		return errors.Join(err, file.Close())
	}
	return errors.Join(file.Sync(), file.Close())
}

func createFolderTemp(
	root *os.Root,
	prefix string,
) (string, *os.File, error) {
	for range 100 {
		var suffix [8]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return "", nil, err
		}
		name := prefix + hex.EncodeToString(suffix[:])
		file, err := root.OpenFile(
			name,
			os.O_WRONLY|os.O_CREATE|os.O_EXCL,
			0o600,
		)
		if err == nil {
			return name, file, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", nil, err
		}
	}
	return "", nil, errors.New("creating artifact folder temporary file: too many collisions")
}

func openFolderRegularFile(
	root *os.Root,
	name string,
) (*os.File, fs.FileInfo, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, nil, errors.New("artifact folder entry is not a regular file")
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, nil, err
	}
	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	after, err := root.Lstat(name)
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	if !after.Mode().IsRegular() ||
		!os.SameFile(before, opened) ||
		!os.SameFile(opened, after) {
		_ = file.Close()
		return nil, nil, errors.New("artifact folder entry changed while opening")
	}
	return file, before, nil
}

func removeFolderFile(root *os.Root, name string) error {
	err := root.Remove(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func syncFolderDirectoryBestEffort(root *os.Root) {
	directory, err := root.Open(".")
	if err != nil {
		return
	}
	_ = directory.Sync()
	_ = directory.Close()
}
