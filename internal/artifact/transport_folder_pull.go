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
	journal, err := openOptionalFolderSubroot(
		t.root,
		folderJournalDirectory,
		"journal",
	)
	if err != nil || journal == nil {
		return result, err
	}
	defer journal.Close()
	head, err := readFolderJournalHead(journal)
	if err != nil {
		return result, err
	}
	if t.pullSequence > head.Sequence {
		return result, fmt.Errorf(
			"%w: artifact journal is behind the durable pull cursor",
			ErrArtifactConflict,
		)
	}
	processed := 0
	var processedBytes int64
	for t.pullSequence < head.Sequence {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if t.maxObjects > 0 && processed >= t.maxObjects {
			result.More = true
			return result, nil
		}
		event, err := readFolderJournalEvent(journal, t.pullSequence+1)
		if err != nil {
			return result, err
		}
		if t.maxBytes > 0 && processed > 0 && event.Size > t.maxBytes-processedBytes {
			result.More = true
			return result, nil
		}
		if event.Origin != publishOrigin && isFolderExchangeKind(event.Kind) {
			if err := t.pullJournalEventLocked(ctx, store, event, &result); err != nil {
				return result, err
			}
		}
		t.pullSequence = event.Sequence
		processed++
		processedBytes += event.Size
	}
	return result, nil
}

func isFolderExchangeKind(kind Kind) bool {
	for _, candidate := range folderExchangeKinds {
		if kind == candidate {
			return true
		}
	}
	return false
}

func (t *folderTransport) pullJournalEventLocked(
	ctx context.Context,
	store ArtifactStore,
	event folderJournalEvent,
	result *ExchangeResult,
) (retErr error) {
	originRoot, err := openFolderSubroot(t.root, event.Origin, "origin")
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, originRoot.Close()) }()
	kindRoot, err := openFolderSubroot(originRoot, string(event.Kind), "kind")
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, kindRoot.Close()) }()
	info, err := kindRoot.Lstat(event.Name)
	if errors.Is(err, fs.ErrNotExist) {
		expected, identityErr := NewIdentity(event.SHA256, event.Size)
		if identityErr != nil {
			return identityErr
		}
		validationErr := validateFolderJournalRejection(
			kindRoot,
			event.Name,
			expected,
		)
		if errors.Is(validationErr, ErrArtifactInvalid) {
			return t.writeFolderJournalRejectionLocked(
				kindRoot,
				event.Name,
				expected,
			)
		}
		return validationErr
	}
	if err != nil {
		return err
	}
	expected, err := NewIdentity(event.SHA256, event.Size)
	if err != nil {
		return err
	}
	_, err = t.pullWireEntryLocked(
		ctx,
		store,
		kindRoot,
		event.Origin,
		event.Kind,
		fs.FileInfoToDirEntry(info),
		&expected,
		result,
	)
	return err
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
				_, err := t.pullWireEntryLocked(
					ctx,
					store,
					kindRoot,
					origin,
					kind,
					entry,
					nil,
					result,
				)
				return err
			},
		)
		closeErr := kindRoot.Close()
		if err != nil || closeErr != nil {
			return errors.Join(err, closeErr)
		}
	}
	return nil
}

type folderPullOutcome struct {
	Quarantined bool
}

func (t *folderTransport) pullWireEntryLocked(
	ctx context.Context,
	store ArtifactStore,
	kindRoot *os.Root,
	origin string,
	kind Kind,
	entry os.DirEntry,
	expected *Identity,
	result *ExchangeResult,
) (_ folderPullOutcome, retErr error) {
	if strings.HasPrefix(entry.Name(), folderPublishTempPrefix) {
		return folderPullOutcome{}, removeFolderFile(kindRoot, entry.Name())
	}
	if strings.HasPrefix(entry.Name(), folderMarkerTempPrefix) {
		return folderPullOutcome{}, nil
	}
	if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
		return folderPullOutcome{}, fmt.Errorf(
			"%w: artifact wire entry is not a regular file",
			ErrArtifactInvalid,
		)
	}
	ref, err := FromWireRef(origin, kind, entry.Name())
	if err != nil {
		if isFolderQuarantineWireName(origin, kind, entry.Name()) {
			return folderPullOutcome{}, nil
		}
		return folderPullOutcome{}, err
	}
	wire, err := ToWireRef(ref)
	if err != nil {
		return folderPullOutcome{}, err
	}
	if wire.Name != entry.Name() {
		return folderPullOutcome{}, fmt.Errorf(
			"%w: artifact wire name is not canonical",
			ErrArtifactInvalid,
		)
	}

	file, before, err := openFolderRegularFile(kindRoot, entry.Name())
	if err != nil {
		return folderPullOutcome{}, err
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
		return folderPullOutcome{}, errors.Join(unchangedErr, file.Close())
	}
	if err := file.Close(); err != nil {
		if spool != nil {
			_ = closeAndRemoveFolderSpool(spool)
		}
		return folderPullOutcome{}, err
	}
	if decodeErr != nil {
		if spool != nil {
			_ = closeAndRemoveFolderSpool(spool)
		}
		if errors.Is(decodeErr, ErrArtifactCorrupt) ||
			errors.Is(decodeErr, ErrArtifactInvalid) {
			err := t.rejectFolderEntryLocked(
				kindRoot,
				entry.Name(),
				expected,
			)
			return folderPullOutcome{Quarantined: err == nil}, err
		}
		return folderPullOutcome{}, decodeErr
	}
	defer func() {
		retErr = errors.Join(retErr, closeAndRemoveFolderSpool(spool))
	}()
	if err := validateRefIdentity(ref, identity); err != nil {
		if errors.Is(err, ErrArtifactInvalid) {
			quarantineErr := t.rejectFolderEntryLocked(
				kindRoot,
				entry.Name(),
				expected,
			)
			return folderPullOutcome{Quarantined: quarantineErr == nil}, quarantineErr
		}
		return folderPullOutcome{}, err
	}
	if expected != nil && identity != *expected {
		return folderPullOutcome{}, fmt.Errorf(
			"%w: artifact journal identity mismatch",
			ErrArtifactConflict,
		)
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return folderPullOutcome{}, err
	}
	recorder, canRecord := store.(transportChangeRecorder)
	existing, statErr := store.Stat(ctx, ref)
	switch {
	case errors.Is(statErr, ErrArtifactNotFound) && !canRecord:
		return folderPullOutcome{}, errors.New("artifact transport change recorder is required")
	case statErr != nil && !errors.Is(statErr, ErrArtifactNotFound):
		return folderPullOutcome{}, statErr
	case statErr == nil && existing.Identity != identity:
		return folderPullOutcome{}, fmt.Errorf(
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
		return folderPullOutcome{}, err
	}
	if created.Created {
		result.Received++
	}
	if canRecord && (created.Created || kind == KindCheckpoints) {
		if err := recorder.RecordTransportChanged(ctx, created.Entry); err != nil {
			return folderPullOutcome{}, err
		}
	}
	return folderPullOutcome{}, nil
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
	if err := t.writeFolderJournalRejectionLocked(
		kindRoot,
		wire.Name,
		expected,
	); err != nil {
		return err
	}
	return t.quarantineFolderEntryLocked(kindRoot, wire.Name)
}

func (t *folderTransport) rejectFolderEntryLocked(
	root *os.Root,
	name string,
	expected *Identity,
) error {
	if expected != nil {
		if err := t.writeFolderJournalRejectionLocked(
			root,
			name,
			*expected,
		); err != nil {
			return err
		}
	}
	return t.quarantineFolderEntryLocked(root, name)
}

func (t *folderTransport) quarantineFolderEntryLocked(
	root *os.Root,
	name string,
) error {
	if t.quarantineEntry != nil {
		return t.quarantineEntry(root, name)
	}
	return t.quarantineFolderEntryDurablyLocked(root, name)
}

func (t *folderTransport) quarantineFolderEntryDurablyLocked(
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
			return t.syncFolderDirectoryLocked(root)
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
