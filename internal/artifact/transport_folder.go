package artifact

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	folderMarkerName         = ".agentsview-artifacts.json"
	folderMarkerMaxBytes     = int64(4 << 10)
	folderFormatName         = "agentsview-normalized-artifacts"
	folderFormatVersion      = 3
	folderMarkerTempPrefix   = ".agentsview-artifacts.tmp-"
	folderMarkerMaxTemps     = 128
	folderExchangeLockName   = ".agentsview-artifacts.lock"
	folderExchangeMaxObjects = 128
	folderExchangeMaxBytes   = int64(64 << 20)
)

type folderMarker struct {
	Format      string `json:"format"`
	NamespaceID string `json:"namespace_id"`
	Version     int    `json:"version"`
}

type folderTransport struct {
	mu sync.Mutex

	target       string
	root         *os.Root
	rootIdentity fs.FileInfo
	namespaceID  string
	closed       bool

	observeDirectoryPage func(int)
	observeStorePage     func(int)
	publishLink          func(*os.Root, string, string) error
	quarantineEntry      func(*os.Root, string) error
	syncDirectory        func(*os.Root) error
	maxObjects           int
	maxBytes             int64
	repairPublished      bool
	pushCursor           folderPushCursor
	stateStore           FolderTransportStateStore
	stateLoaded          bool
	pullSequence         int64
	publishedGeneration  string
}

type folderPushCursor struct {
	Generation           string `json:"generation,omitempty"`
	Origin               string `json:"origin,omitempty"`
	KindIndex            int    `json:"kind_index,omitempty"`
	Offset               int    `json:"offset,omitempty"`
	SegmentIndex         int    `json:"segment_index,omitempty"`
	PublicationSessionID string `json:"publication_session_id,omitempty"`
	Repair               bool   `json:"repair,omitempty"`
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
		target:          canonical,
		root:            root,
		rootIdentity:    identity,
		maxObjects:      opts.MaxObjects,
		maxBytes:        opts.MaxBytes,
		repairPublished: opts.RepairPublished,
		stateStore:      opts.StateStore,
	}
	if transport.maxObjects <= 0 {
		transport.maxObjects = folderExchangeMaxObjects
	}
	if transport.maxBytes <= 0 {
		transport.maxBytes = folderExchangeMaxBytes
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
	if err := t.loadStateLocked(ctx); err != nil {
		return ExchangeResult{}, err
	}
	result, err = t.pullLocked(ctx, store, publishOrigin)
	if err != nil {
		return result, err
	}
	published, more, err := t.pushLocked(ctx, store, publishOrigin)
	result.Published += published
	result.More = result.More || more
	if err != nil {
		return result, err
	}
	if err := t.verifyRootIdentityLocked(); err != nil {
		return result, err
	}
	if err := t.saveStateLocked(ctx); err != nil {
		return result, err
	}
	return result, nil
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
		marker, markerErr := readFolderMarker(t.root)
		if markerErr == nil {
			t.namespaceID = marker.NamespaceID
			return nil
		}
		recovered, recoveryErr := recoverFolderMarkerTemporaries(t.root, true)
		if recoveryErr != nil {
			return errors.Join(markerErr, recoveryErr)
		}
		if !recovered {
			return markerErr
		}
	}
	if !found {
		empty, err := folderRootEmpty(t.root)
		if err != nil {
			return err
		}
		if !empty {
			recovered, err := recoverFolderMarkerTemporaries(t.root, false)
			if err != nil {
				return err
			}
			if !recovered {
				return fmt.Errorf(
					"%w: target is not an agentsview artifact target",
					ErrArtifactInvalid,
				)
			}
		}
	}
	if err := createFolderMarker(t.root); err != nil {
		return fmt.Errorf("initializing agentsview artifact target: %w", err)
	}
	marker, err := readFolderMarker(t.root)
	if err == nil {
		t.namespaceID = marker.NamespaceID
	}
	return err
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
	marker, err := readFolderMarker(t.root)
	if err != nil {
		return err
	}
	if marker.NamespaceID != t.namespaceID {
		return errors.New("artifact folder target marker changed while open")
	}
	return nil
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

func recoverFolderMarkerTemporaries(
	root *os.Root,
	invalidFinal bool,
) (_ bool, retErr error) {
	directory, err := root.Open(".")
	if err != nil {
		return false, fmt.Errorf("opening artifact target directory: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, directory.Close()) }()

	names := make([]string, 0, 1)
	foundFinal := false
	foundValidTemporary := false
	for {
		entries, err := directory.ReadDir(1)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return false, fmt.Errorf("reading artifact target directory: %w", err)
		}
		name := entries[0].Name()
		if invalidFinal && name == folderMarkerName {
			info, err := root.Lstat(name)
			if err != nil {
				return false, fmt.Errorf("stating invalid marker: %w", err)
			}
			if !info.Mode().IsRegular() {
				return false, nil
			}
			foundFinal = true
			continue
		}
		if !isFolderMarkerTemporaryName(name) ||
			len(names) >= folderMarkerMaxTemps {
			return false, nil
		}
		info, err := root.Lstat(name)
		if err != nil {
			return false, fmt.Errorf("stating marker temporary: %w", err)
		}
		if !info.Mode().IsRegular() {
			return false, nil
		}
		if invalidFinal {
			if _, err := readFolderMarkerNamed(root, name); err == nil {
				foundValidTemporary = true
			}
		}
		names = append(names, name)
	}
	if invalidFinal && (!foundFinal || !foundValidTemporary) {
		return false, nil
	}
	if !invalidFinal && len(names) == 0 {
		return true, nil
	}
	if invalidFinal {
		if err := removeFolderFile(root, folderMarkerName); err != nil {
			return false, fmt.Errorf("removing invalid marker: %w", err)
		}
	}
	for _, name := range names {
		if err := removeFolderFile(root, name); err != nil {
			return false, fmt.Errorf("removing marker temporary: %w", err)
		}
	}
	if err := syncFolderDirectory(root); err != nil {
		return false, fmt.Errorf("syncing recovered artifact target: %w", err)
	}
	return true, nil
}

func isFolderMarkerTemporaryName(name string) bool {
	suffix, found := strings.CutPrefix(name, folderMarkerTempPrefix)
	if !found || len(suffix) != 16 {
		return false
	}
	for _, character := range suffix {
		if (character < '0' || character > '9') &&
			(character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func validateFolderMarker(root *os.Root) error {
	_, err := readFolderMarker(root)
	return err
}

func readFolderMarker(root *os.Root) (folderMarker, error) {
	return readFolderMarkerNamed(root, folderMarkerName)
}

func readFolderMarkerNamed(root *os.Root, name string) (folderMarker, error) {
	file, _, err := openFolderRegularFile(root, name)
	if err != nil {
		return folderMarker{}, fmt.Errorf("invalid agentsview artifact target marker: %w", err)
	}
	defer file.Close()

	limited := io.LimitReader(file, folderMarkerMaxBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return folderMarker{}, fmt.Errorf("invalid agentsview artifact target marker: %w", err)
	}
	if int64(len(body)) > folderMarkerMaxBytes {
		return folderMarker{}, errors.New("invalid agentsview artifact target marker: marker is too large")
	}
	var marker folderMarker
	if err := json.Unmarshal(
		body, &marker, json.RejectUnknownMembers(true),
	); err != nil {
		return folderMarker{}, fmt.Errorf("invalid agentsview artifact target marker: %w", err)
	}
	if marker.Format != folderFormatName || marker.Version != folderFormatVersion {
		return folderMarker{}, errors.New("invalid agentsview artifact target marker: unsupported format")
	}
	if len(marker.NamespaceID) != 32 {
		return folderMarker{}, errors.New("invalid agentsview artifact target marker: invalid namespace ID")
	}
	if _, err := hex.DecodeString(marker.NamespaceID); err != nil {
		return folderMarker{}, errors.New("invalid agentsview artifact target marker: invalid namespace ID")
	}
	return marker, nil
}

func createFolderMarker(root *os.Root) (retErr error) {
	var namespace [16]byte
	if _, err := rand.Read(namespace[:]); err != nil {
		return err
	}
	body, err := json.Marshal(folderMarker{
		Format:      folderFormatName,
		NamespaceID: hex.EncodeToString(namespace[:]),
		Version:     folderFormatVersion,
	})
	if err != nil {
		return err
	}
	body = append(body, '\n')
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
	if _, err := file.Write(body); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Sync(); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Close(); err != nil {
		return err
	}

	if err := root.Link(tempName, folderMarkerName); err != nil {
		if fallbackErr := createFolderMarkerExclusive(root, body); fallbackErr != nil {
			return errors.Join(err, fallbackErr)
		}
	}
	if err := root.Remove(tempName); err != nil {
		return err
	}
	tempExists = false
	return syncFolderDirectory(root)
}

func createFolderMarkerExclusive(root *os.Root, body []byte) error {
	return createFolderMarkerExclusiveWithWriter(
		root,
		body,
		func(file *os.File, body []byte) (int, error) {
			return file.Write(body)
		},
	)
}

func createFolderMarkerExclusiveWithWriter(
	root *os.Root,
	body []byte,
	write func(*os.File, []byte) (int, error),
) (retErr error) {
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
	closed := false
	keep := false
	defer func() {
		if !closed {
			retErr = errors.Join(retErr, file.Close())
		}
		if !keep {
			retErr = errors.Join(
				retErr,
				removeFolderFile(root, folderMarkerName),
			)
		}
	}()
	written, err := write(file, body)
	if err != nil {
		return err
	}
	if written != len(body) {
		return io.ErrShortWrite
	}
	if err := file.Sync(); err != nil {
		return err
	}
	closeErr := file.Close()
	closed = true
	if closeErr != nil {
		return closeErr
	}
	keep = true
	return nil
}

type folderTransportPersistedState struct {
	PublishedGeneration string           `json:"published_generation,omitempty"`
	PullSequence        int64            `json:"pull_sequence,omitempty"`
	Push                folderPushCursor `json:"push"`
}

func (t *folderTransport) loadStateLocked(ctx context.Context) error {
	if t.stateLoaded || t.stateStore == nil {
		t.stateLoaded = true
		return nil
	}
	body, err := t.stateStore.LoadFolderTransportState(ctx, t.namespaceID)
	if err != nil {
		return err
	}
	if body != "" {
		var state folderTransportPersistedState
		if err := json.Unmarshal(
			[]byte(body), &state, json.RejectUnknownMembers(true),
		); err != nil {
			return fmt.Errorf("decoding artifact folder continuation state: %w", err)
		}
		if err := validateFolderTransportState(state); err != nil {
			return err
		}
		t.pushCursor = state.Push
		t.pullSequence = state.PullSequence
		t.publishedGeneration = state.PublishedGeneration
	}
	t.stateLoaded = true
	return nil
}

func validateFolderTransportState(state folderTransportPersistedState) error {
	push := state.Push
	if state.PullSequence < 0 ||
		push.KindIndex < 0 ||
		push.Offset < 0 ||
		push.SegmentIndex < 0 {
		return errors.New("decoding artifact folder continuation state: negative cursor")
	}
	if push.Origin != "" {
		if err := validateOriginID(push.Origin); err != nil {
			return fmt.Errorf("decoding artifact folder continuation state: %w", err)
		}
	}
	return nil
}

func (t *folderTransport) saveStateLocked(ctx context.Context) error {
	if t.stateStore == nil {
		return nil
	}
	body, err := json.Marshal(folderTransportPersistedState{
		PublishedGeneration: t.publishedGeneration,
		PullSequence:        t.pullSequence,
		Push:                t.pushCursor,
	})
	if err != nil {
		return err
	}
	return t.stateStore.SaveFolderTransportState(ctx, t.namespaceID, string(body))
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

func syncFolderDirectory(root *os.Root) (retErr error) {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, directory.Close()) }()
	if err := directory.Sync(); err != nil &&
		!isFolderDirectorySyncUnsupported(err) {
		return err
	}
	return nil
}

func (t *folderTransport) syncFolderDirectoryLocked(root *os.Root) error {
	if t.syncDirectory != nil {
		return t.syncDirectory(root)
	}
	return syncFolderDirectory(root)
}
