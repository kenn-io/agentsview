package artifact

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
)

const (
	transportDirectoryPageSize = 512
	transportWireOverhead      = int64(1 << 20)
	folderCorruptSeparator     = ".corrupt-"
)

var folderExchangeKinds = [...]Kind{
	KindSegments,
	KindManifests,
	KindCheckpoints,
}

func (t *folderTransport) pullLocked(
	ctx context.Context,
	store ArtifactStore,
	publishOrigin string,
) (ExchangeResult, error) {
	var result ExchangeResult
	err := t.visitFolderDirectory(
		ctx,
		t.root,
		".",
		func(entry os.DirEntry) error {
			if entry.Name() == folderMarkerName ||
				entry.Name() == folderExchangeLockName ||
				strings.HasPrefix(entry.Name(), folderMarkerTempPrefix) {
				return nil
			}
			if entry.Name() == publishOrigin {
				return nil
			}
			return t.pullRootEntryLocked(ctx, store, entry, &result)
		},
	)
	return result, err
}

func (t *folderTransport) pullRootEntryLocked(
	ctx context.Context,
	store ArtifactStore,
	entry os.DirEntry,
	result *ExchangeResult,
) error {
	if entry.Type()&os.ModeSymlink != 0 {
		return fmt.Errorf(
			"%w: invalid artifact origin entry",
			ErrArtifactInvalid,
		)
	}
	if err := validateOriginID(entry.Name()); err != nil {
		return fmt.Errorf(
			"%w: invalid artifact origin directory: %v",
			ErrArtifactInvalid,
			err,
		)
	}
	originRoot, err := openFolderSubroot(
		t.root,
		entry.Name(),
		"origin",
	)
	if err != nil {
		return fmt.Errorf("%w: invalid artifact origin entry: %v", ErrArtifactInvalid, err)
	}
	pullErr := t.pullOriginLocked(
		ctx,
		store,
		originRoot,
		entry.Name(),
		result,
	)
	return errors.Join(pullErr, originRoot.Close())
}

func (t *folderTransport) pullOriginLocked(
	ctx context.Context,
	store ArtifactStore,
	originRoot *os.Root,
	origin string,
	result *ExchangeResult,
) error {
	for _, kind := range folderExchangeKinds {
		if err := ctx.Err(); err != nil {
			return err
		}
		kindRoot, err := openOptionalFolderSubroot(
			originRoot,
			string(kind),
			"kind",
		)
		if err != nil {
			return err
		}
		if kindRoot == nil {
			continue
		}
		err = t.visitFolderDirectory(
			ctx,
			kindRoot,
			".",
			func(entry os.DirEntry) error {
				return t.pullWireEntryLocked(
					ctx,
					store,
					kindRoot,
					origin,
					kind,
					entry,
					result,
				)
			},
		)
		closeErr := kindRoot.Close()
		if err != nil || closeErr != nil {
			return errors.Join(err, closeErr)
		}
	}
	return nil
}

func (t *folderTransport) pullWireEntryLocked(
	ctx context.Context,
	store ArtifactStore,
	kindRoot *os.Root,
	origin string,
	kind Kind,
	entry os.DirEntry,
	result *ExchangeResult,
) (retErr error) {
	if strings.HasPrefix(entry.Name(), folderPublishTempPrefix) {
		return removeFolderFile(kindRoot, entry.Name())
	}
	if strings.HasPrefix(entry.Name(), folderMarkerTempPrefix) {
		return nil
	}
	if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
		return fmt.Errorf(
			"%w: artifact wire entry is not a regular file",
			ErrArtifactInvalid,
		)
	}
	ref, err := FromWireRef(origin, kind, entry.Name())
	if err != nil {
		if isFolderQuarantineWireName(origin, kind, entry.Name()) {
			return nil
		}
		return err
	}
	wire, err := ToWireRef(ref)
	if err != nil {
		return err
	}
	if wire.Name != entry.Name() {
		return fmt.Errorf(
			"%w: artifact wire name is not canonical",
			ErrArtifactInvalid,
		)
	}

	file, before, err := openFolderRegularFile(kindRoot, entry.Name())
	if err != nil {
		return err
	}
	spool, identity, decodeErr := spoolFolderWire(
		ctx,
		wire,
		file,
		transportWireLimits(kind),
	)
	unchangedErr := verifyFolderFileUnchanged(
		kindRoot,
		entry.Name(),
		file,
		before,
	)
	if unchangedErr != nil {
		if spool != nil {
			_ = closeAndRemoveFolderSpool(spool)
		}
		return errors.Join(unchangedErr, file.Close())
	}
	if err := file.Close(); err != nil {
		if spool != nil {
			_ = closeAndRemoveFolderSpool(spool)
		}
		return err
	}
	if decodeErr != nil {
		if spool != nil {
			_ = closeAndRemoveFolderSpool(spool)
		}
		if errors.Is(decodeErr, ErrArtifactCorrupt) ||
			errors.Is(decodeErr, ErrArtifactInvalid) {
			return t.quarantineFolderEntryLocked(kindRoot, entry.Name())
		}
		return decodeErr
	}
	defer func() {
		retErr = errors.Join(retErr, closeAndRemoveFolderSpool(spool))
	}()
	if err := validateRefIdentity(ref, identity); err != nil {
		if errors.Is(err, ErrArtifactInvalid) {
			return t.quarantineFolderEntryLocked(kindRoot, entry.Name())
		}
		return err
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return err
	}
	recorder, canRecord := store.(transportChangeRecorder)
	existing, statErr := store.Stat(ctx, ref)
	switch {
	case errors.Is(statErr, ErrArtifactNotFound) && !canRecord:
		return errors.New("artifact transport change recorder is required")
	case statErr != nil && !errors.Is(statErr, ErrArtifactNotFound):
		return statErr
	case statErr == nil && existing.Identity != identity:
		return fmt.Errorf(
			"%w: artifact folder object conflicts with local identity",
			ErrArtifactConflict,
		)
	}
	created, err := store.Create(
		ctx,
		ref,
		identity,
		canonicalArtifactMediaType(kind),
		spool,
	)
	if err != nil {
		return err
	}
	if created.Created {
		result.Received++
	}
	if canRecord && (created.Created || kind == KindCheckpoints) {
		if err := recorder.RecordTransportChanged(ctx, created.Entry); err != nil {
			return err
		}
	}
	return nil
}

func isFolderQuarantineWireName(origin string, kind Kind, name string) bool {
	index := strings.LastIndex(name, folderCorruptSeparator)
	if index <= 0 {
		return false
	}
	suffix := name[index+len(folderCorruptSeparator):]
	if len(suffix) != 16 {
		return false
	}
	for _, character := range suffix {
		if (character < '0' || character > '9') &&
			(character < 'a' || character > 'f') {
			return false
		}
	}
	base := name[:index]
	ref, err := FromWireRef(origin, kind, base)
	if err != nil {
		return false
	}
	wire, err := ToWireRef(ref)
	return err == nil && wire.Name == base
}

func (t *folderTransport) QuarantineTransportArtifact(
	ctx context.Context,
	ref Ref,
	expected Identity,
) (retErr error) {
	if ctx == nil {
		return fmt.Errorf(
			"%w: artifact transport context is required",
			ErrArtifactInvalid,
		)
	}
	if err := validateStoreRef(ref); err != nil {
		return err
	}
	if err := validateStoreIdentity(expected); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.root == nil {
		return fs.ErrClosed
	}
	lock, err := t.acquireExchangeLockLocked(ctx)
	if err != nil {
		return err
	}
	defer func() {
		retErr = errors.Join(retErr, lock.Close())
	}()
	if err := t.prepareLocked(); err != nil {
		return err
	}
	originRoot, err := openOptionalFolderSubroot(
		t.root,
		ref.Origin,
		"origin",
	)
	if err != nil || originRoot == nil {
		return err
	}
	defer originRoot.Close()
	kindRoot, err := openOptionalFolderSubroot(
		originRoot,
		string(ref.Kind),
		"kind",
	)
	if err != nil || kindRoot == nil {
		return err
	}
	defer kindRoot.Close()
	wire, err := ToWireRef(ref)
	if err != nil {
		return err
	}
	file, before, err := openFolderRegularFile(kindRoot, wire.Name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	actual, identityErr := folderWireIdentity(
		ctx,
		wire,
		file,
		transportWireLimits(ref.Kind),
	)
	unchangedErr := verifyFolderFileUnchanged(
		kindRoot,
		wire.Name,
		file,
		before,
	)
	closeErr := file.Close()
	if err := errors.Join(identityErr, unchangedErr, closeErr); err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf(
			"%w: artifact folder object changed before quarantine",
			ErrArtifactConflict,
		)
	}
	return t.quarantineFolderEntryLocked(kindRoot, wire.Name)
}

func (t *folderTransport) quarantineFolderEntryLocked(
	root *os.Root,
	name string,
) error {
	if _, err := root.Lstat(name); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	for range 100 {
		var suffix [8]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return err
		}
		destination := name + folderCorruptSeparator +
			hex.EncodeToString(suffix[:])
		err := root.Rename(name, destination)
		if err == nil {
			syncFolderDirectoryBestEffort(root)
			return nil
		}
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
	}
	return errors.New("quarantining artifact wire entry: too many collisions")
}

func (t *folderTransport) visitFolderDirectory(
	ctx context.Context,
	root *os.Root,
	name string,
	visit func(os.DirEntry) error,
) (retErr error) {
	directory, err := root.Open(name)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, directory.Close()) }()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		page, readErr := directory.ReadDir(transportDirectoryPageSize)
		if len(page) > 0 && t.observeDirectoryPage != nil {
			t.observeDirectoryPage(len(page))
		}
		for _, entry := range page {
			if err := visit(entry); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

func openFolderSubroot(
	parent *os.Root,
	name string,
	role string,
) (*os.Root, error) {
	before, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.IsDir() {
		return nil, fmt.Errorf("artifact %s entry is not a directory", role)
	}
	root, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	after, err := parent.Lstat(name)
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	if !after.IsDir() ||
		!os.SameFile(before, opened) ||
		!os.SameFile(opened, after) {
		_ = root.Close()
		return nil, fmt.Errorf("artifact %s entry changed while opening", role)
	}
	return root, nil
}

func openOptionalFolderSubroot(
	parent *os.Root,
	name string,
	role string,
) (*os.Root, error) {
	root, err := openFolderSubroot(parent, name, role)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return root, err
}

func spoolFolderWire(
	ctx context.Context,
	wire WireRef,
	source io.Reader,
	limits WireLimits,
) (_ *os.File, _ Identity, retErr error) {
	spool, err := os.CreateTemp("", "agentsview-artifact-wire-*")
	if err != nil {
		return nil, Identity{}, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			retErr = errors.Join(retErr, closeAndRemoveFolderSpool(spool))
		}
	}()
	if err := spool.Chmod(0o600); err != nil {
		return nil, Identity{}, err
	}
	hasher := sha256.New()
	if err := DecodeWire(
		ctx,
		wire,
		source,
		io.MultiWriter(spool, hasher),
		limits,
	); err != nil {
		return nil, Identity{}, err
	}
	info, err := spool.Stat()
	if err != nil {
		return nil, Identity{}, err
	}
	identity, err := NewIdentity(
		hex.EncodeToString(hasher.Sum(nil)),
		info.Size(),
	)
	if err != nil {
		return nil, Identity{}, err
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return nil, Identity{}, err
	}
	cleanup = false
	return spool, identity, nil
}

func verifyFolderFileUnchanged(
	root *os.Root,
	name string,
	file *os.File,
	before fs.FileInfo,
) error {
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	after, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if !after.Mode().IsRegular() ||
		!os.SameFile(before, opened) ||
		!os.SameFile(opened, after) ||
		before.Size() != opened.Size() ||
		before.ModTime() != opened.ModTime() ||
		opened.Size() != after.Size() ||
		opened.ModTime() != after.ModTime() {
		return errors.New("artifact wire entry changed while reading")
	}
	return nil
}

func transportWireLimits(kind Kind) WireLimits {
	var decoded int64
	switch kind {
	case KindManifests, KindMeta:
		decoded = manifestDecodedLimit
	case KindSegments, KindCheckpoints:
		decoded = segmentDecodedLimit
	case KindRaw:
		decoded = maxRawSourceSize
	default:
		decoded = 1
	}
	return WireLimits{
		MaxEncodedBytes: decoded + transportWireOverhead,
		MaxDecodedBytes: decoded,
	}
}

func closeAndRemoveFolderSpool(file *os.File) error {
	if file == nil {
		return nil
	}
	name := file.Name()
	return errors.Join(file.Close(), removeFolderSpool(name))
}

func removeFolderSpool(name string) error {
	err := os.Remove(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
