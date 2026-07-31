package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sync"
)

const (
	transportStorePageSize  = 512
	folderPublishTempPrefix = ".agentsview-artifact-publish-"
	folderCopyBufferSize    = 32 << 10
)

var folderCopyBufferPool = sync.Pool{
	New: func() any {
		return new([folderCopyBufferSize]byte)
	},
}

func (t *folderTransport) pushLocked(
	ctx context.Context,
	store ArtifactStore,
	origin string,
) (int, error) {
	return t.pushOriginLocked(ctx, store, origin)
}

func (t *folderTransport) pushOriginLocked(
	ctx context.Context,
	store ArtifactStore,
	origin string,
) (published int, retErr error) {
	if err := validateOriginID(origin); err != nil {
		return 0, err
	}
	originRoot, err := ensureFolderSubroot(t.root, origin, "origin")
	if err != nil {
		return 0, err
	}
	defer func() { retErr = errors.Join(retErr, originRoot.Close()) }()
	for _, kind := range folderExchangeKinds {
		iterator, err := store.Entries(ctx, origin, kind)
		if err != nil {
			return published, err
		}
		count, iterateErr := t.pushKindLocked(
			ctx,
			store,
			originRoot,
			origin,
			kind,
			iterator,
		)
		published += count
		closeErr := iterator.Close()
		if iterateErr != nil || closeErr != nil {
			return published, errors.Join(iterateErr, closeErr)
		}
	}
	return published, nil
}

func (t *folderTransport) pushKindLocked(
	ctx context.Context,
	store ArtifactStore,
	originRoot *os.Root,
	origin string,
	kind Kind,
	iterator EntryIterator,
) (published int, retErr error) {
	var kindRoot *os.Root
	defer func() {
		if kindRoot != nil {
			retErr = errors.Join(retErr, kindRoot.Close())
		}
	}()
	for {
		page, nextErr := iterator.Next(ctx, transportStorePageSize)
		t.observeStorePageLocked(len(page))
		if len(page) > 0 && kindRoot == nil {
			var err error
			kindRoot, err = ensureFolderSubroot(
				originRoot,
				string(kind),
				"kind",
			)
			if err != nil {
				return published, err
			}
		}
		for _, entry := range page {
			if entry.Ref.Origin != origin || entry.Ref.Kind != kind {
				return published, fmt.Errorf(
					"%w: artifact iterator returned an entry outside its collection",
					ErrArtifactInvalid,
				)
			}
			created, err := t.publishFolderEntryLocked(
				ctx,
				store,
				kindRoot,
				entry,
			)
			if err != nil {
				return published, err
			}
			if created {
				published++
			}
		}
		if errors.Is(nextErr, io.EOF) {
			return published, nil
		}
		if nextErr != nil {
			return published, nextErr
		}
	}
}

func (t *folderTransport) publishFolderEntryLocked(
	ctx context.Context,
	store ArtifactStore,
	kindRoot *os.Root,
	entry Entry,
) (created bool, retErr error) {
	if err := validateStoreRef(entry.Ref); err != nil {
		return false, err
	}
	if err := validateStoreIdentity(entry.Identity); err != nil {
		return false, err
	}
	if err := validateRefIdentity(entry.Ref, entry.Identity); err != nil {
		return false, err
	}
	limits := transportWireLimits(entry.Ref.Kind)
	if entry.Identity.Size > limits.MaxDecodedBytes {
		return false, fmt.Errorf(
			"%w: artifact exceeds decoded size limit: %d bytes > %d",
			ErrArtifactInvalid,
			entry.Identity.Size,
			limits.MaxDecodedBytes,
		)
	}
	wire, err := ToWireRef(entry.Ref)
	if err != nil {
		return false, err
	}
	found, matches, err := t.folderWireMatchesEntryLocked(
		ctx,
		kindRoot,
		wire,
		entry,
	)
	if err != nil {
		return false, err
	}
	if found {
		if matches {
			return false, nil
		}
		return false, fmt.Errorf(
			"%w: artifact folder object conflicts with local identity",
			ErrArtifactConflict,
		)
	}

	tempName, temp, err := createFolderTemp(kindRoot, folderPublishTempPrefix)
	if err != nil {
		return false, err
	}
	tempExists := true
	defer func() {
		if tempExists {
			retErr = errors.Join(retErr, removeFolderFile(kindRoot, tempName))
		}
	}()
	opened, reader, err := store.Open(ctx, entry.Ref)
	if err != nil {
		return false, errors.Join(err, temp.Close())
	}
	if opened.Ref != entry.Ref || opened.Identity != entry.Identity {
		return false, errors.Join(
			fmt.Errorf(
				"%w: artifact changed between iteration and open",
				ErrArtifactConflict,
			),
			reader.Close(),
			temp.Close(),
		)
	}
	encodeErr := EncodeWire(ctx, entry.Ref, reader, temp)
	verifyErr := reader.Verify()
	readerCloseErr := reader.Close()
	tempSyncErr := temp.Sync()
	tempCloseErr := temp.Close()
	if err := errors.Join(
		encodeErr,
		verifyErr,
		readerCloseErr,
		tempSyncErr,
		tempCloseErr,
	); err != nil {
		return false, err
	}

	created, err = t.publishFolderTempLocked(
		ctx,
		kindRoot,
		tempName,
		wire,
		entry,
	)
	if err != nil {
		return false, err
	}
	if err := kindRoot.Remove(tempName); err != nil {
		return false, err
	}
	tempExists = false
	syncFolderDirectoryBestEffort(kindRoot)
	return created, nil
}

func (t *folderTransport) publishFolderTempLocked(
	ctx context.Context,
	root *os.Root,
	tempName string,
	wire WireRef,
	entry Entry,
) (bool, error) {
	link := t.publishLink
	if link == nil {
		link = func(root *os.Root, oldName, newName string) error {
			return root.Link(oldName, newName)
		}
	}
	err := link(root, tempName, wire.Name)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrExist) {
		return t.compareExistingFolderWireLocked(ctx, root, wire, entry)
	}
	created, fallbackErr := copyFolderTempExclusive(
		root,
		tempName,
		wire.Name,
	)
	if fallbackErr == nil && created {
		return true, nil
	}
	if errors.Is(fallbackErr, fs.ErrExist) {
		return t.compareExistingFolderWireLocked(ctx, root, wire, entry)
	}
	return false, errors.Join(err, fallbackErr)
}

func (t *folderTransport) compareExistingFolderWireLocked(
	ctx context.Context,
	root *os.Root,
	wire WireRef,
	entry Entry,
) (bool, error) {
	found, matches, err := t.folderWireMatchesEntryLocked(
		ctx,
		root,
		wire,
		entry,
	)
	if err != nil {
		return false, err
	}
	if !found {
		return false, errors.New("artifact folder object disappeared during publication")
	}
	if !matches {
		return false, fmt.Errorf(
			"%w: artifact folder object conflicts with local identity",
			ErrArtifactConflict,
		)
	}
	return false, nil
}

func (t *folderTransport) folderWireMatchesEntryLocked(
	ctx context.Context,
	root *os.Root,
	wire WireRef,
	entry Entry,
) (found bool, matches bool, retErr error) {
	file, before, err := openFolderRegularFile(root, wire.Name)
	if errors.Is(err, fs.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	identity, decodeErr := folderWireIdentity(
		ctx,
		wire,
		file,
		transportWireLimits(wire.Kind),
	)
	unchangedErr := verifyFolderFileUnchanged(
		root,
		wire.Name,
		file,
		before,
	)
	closeErr := file.Close()
	if unchangedErr != nil || closeErr != nil {
		return true, false, errors.Join(unchangedErr, closeErr)
	}
	if decodeErr != nil {
		if errors.Is(decodeErr, ErrArtifactCorrupt) ||
			errors.Is(decodeErr, ErrArtifactInvalid) {
			if err := t.quarantineFolderEntryLocked(root, wire.Name); err != nil {
				return true, false, err
			}
			return false, false, nil
		}
		return true, false, decodeErr
	}
	return true, identity == entry.Identity, nil
}

func folderWireIdentity(
	ctx context.Context,
	wire WireRef,
	source io.Reader,
	limits WireLimits,
) (Identity, error) {
	hasher := sha256.New()
	counter := &countingWriter{writer: hasher}
	if err := DecodeWire(ctx, wire, source, counter, limits); err != nil {
		return Identity{}, err
	}
	return NewIdentity(
		hex.EncodeToString(hasher.Sum(nil)),
		counter.written,
	)
}

type countingWriter struct {
	writer  io.Writer
	written int64
}

func (w *countingWriter) Write(buffer []byte) (int, error) {
	count, err := w.writer.Write(buffer)
	w.written += int64(count)
	return count, err
}

func ensureFolderSubroot(
	parent *os.Root,
	name string,
	role string,
) (*os.Root, error) {
	root, err := openOptionalFolderSubroot(parent, name, role)
	if err != nil {
		return nil, err
	}
	if root != nil {
		return root, nil
	}
	if err := parent.Mkdir(name, 0o755); err != nil &&
		!errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	syncFolderDirectoryBestEffort(parent)
	return openFolderSubroot(parent, name, role)
}

func copyFolderTempExclusive(
	root *os.Root,
	sourceName string,
	destinationName string,
) (created bool, retErr error) {
	source, _, err := openFolderRegularFile(root, sourceName)
	if err != nil {
		return false, err
	}
	defer func() { retErr = errors.Join(retErr, source.Close()) }()
	destination, err := root.OpenFile(
		destinationName,
		os.O_WRONLY|os.O_CREATE|os.O_EXCL,
		0o600,
	)
	if err != nil {
		return false, err
	}
	keep := false
	defer func() {
		retErr = errors.Join(retErr, destination.Close())
		if !keep {
			retErr = errors.Join(
				retErr,
				removeFolderFile(root, destinationName),
			)
		}
	}()
	buffer := folderCopyBufferPool.Get().(*[folderCopyBufferSize]byte)
	_, copyErr := io.CopyBuffer(destination, source, buffer[:])
	folderCopyBufferPool.Put(buffer)
	if copyErr != nil {
		return false, copyErr
	}
	if err := destination.Sync(); err != nil {
		return false, err
	}
	keep = true
	return true, nil
}

func (t *folderTransport) observeStorePageLocked(size int) {
	if size > 0 && t.observeStorePage != nil {
		t.observeStorePage(size)
	}
}
