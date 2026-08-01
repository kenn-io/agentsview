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
	"strings"
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

type folderTransportPublicationPager interface {
	folderTransportGeneration() string
	folderTransportPage(
		context.Context,
		folderPushCursor,
		int,
		int64,
	) ([]Entry, folderPushCursor, bool, error)
}

func (t *folderTransport) pushLocked(
	ctx context.Context,
	store ArtifactStore,
	origin string,
) (int, bool, error) {
	return t.pushOriginLocked(ctx, store, origin)
}

func (t *folderTransport) pushOriginLocked(
	ctx context.Context,
	store ArtifactStore,
	origin string,
) (published int, more bool, retErr error) {
	if err := validateOriginID(origin); err != nil {
		return 0, false, err
	}
	if pager, ok := store.(folderTransportPublicationPager); ok {
		return t.pushAuthoritativeOriginLocked(ctx, store, pager, origin)
	}
	originRoot, err := t.ensureFolderSubrootLocked(t.root, origin, "origin")
	if err != nil {
		return 0, false, err
	}
	defer func() { retErr = errors.Join(retErr, originRoot.Close()) }()
	if t.pushCursor.Origin != origin {
		t.pushCursor = folderPushCursor{Origin: origin}
	}
	processed := 0
	var processedBytes int64
	for kindIndex := t.pushCursor.KindIndex; kindIndex < len(folderExchangeKinds); kindIndex++ {
		kind := folderExchangeKinds[kindIndex]
		if err := t.removeAbandonedPublishTempsLocked(
			ctx,
			originRoot,
			kind,
		); err != nil {
			return published, false, err
		}
		iterator, err := store.Entries(ctx, origin, kind)
		if err != nil {
			return published, false, err
		}
		if kindIndex == t.pushCursor.KindIndex && t.pushCursor.Offset > 0 {
			if err := discardArtifactEntries(ctx, iterator, t.pushCursor.Offset); err != nil {
				_ = iterator.Close()
				return published, false, err
			}
		}
		count, consumed, bytes, kindMore, iterateErr := t.pushKindLocked(
			ctx,
			store,
			originRoot,
			origin,
			kind,
			iterator,
			t.maxObjects-processed,
			t.maxBytes-processedBytes,
		)
		published += count
		processed += consumed
		processedBytes += bytes
		closeErr := iterator.Close()
		if iterateErr != nil || closeErr != nil {
			return published, false, errors.Join(iterateErr, closeErr)
		}
		if kindMore {
			t.pushCursor.KindIndex = kindIndex
			t.pushCursor.Offset += consumed
			return published, true, nil
		}
		t.pushCursor.KindIndex = kindIndex + 1
		t.pushCursor.Offset = 0
	}
	t.pushCursor = folderPushCursor{}
	return published, false, nil
}

func (t *folderTransport) pushAuthoritativeOriginLocked(
	ctx context.Context,
	store ArtifactStore,
	pager folderTransportPublicationPager,
	origin string,
) (published int, more bool, retErr error) {
	generation := pager.folderTransportGeneration()
	continuing := t.pushCursor.Origin == origin &&
		t.pushCursor.Generation == generation
	if generation == t.publishedGeneration {
		switch {
		case continuing && t.pushCursor.Repair:
		case !t.repairPublished:
			return 0, false, nil
		default:
			t.pushCursor = folderPushCursor{
				Generation: generation,
				Origin:     origin,
				Repair:     true,
			}
		}
	} else if !continuing || t.pushCursor.Repair {
		t.pushCursor = folderPushCursor{
			Generation: generation,
			Origin:     origin,
		}
	}
	entries, next, more, err := pager.folderTransportPage(
		ctx,
		t.pushCursor,
		t.maxObjects,
		t.maxBytes,
	)
	if err != nil {
		return 0, false, err
	}
	originRoot, err := t.ensureFolderSubrootLocked(t.root, origin, "origin")
	if err != nil {
		return 0, false, err
	}
	defer func() { retErr = errors.Join(retErr, originRoot.Close()) }()
	var kindRoot *os.Root
	var openKind Kind
	defer func() {
		if kindRoot != nil {
			retErr = errors.Join(retErr, kindRoot.Close())
		}
	}()
	for _, entry := range entries {
		if kindRoot == nil || openKind != entry.Ref.Kind {
			if kindRoot != nil {
				if err := kindRoot.Close(); err != nil {
					return published, false, err
				}
				kindRoot = nil
			}
			kindRoot, err = t.ensureFolderSubrootLocked(
				originRoot,
				string(entry.Ref.Kind),
				"kind",
			)
			if err != nil {
				return published, false, err
			}
			openKind = entry.Ref.Kind
		}
		// The rejection marker survives a crash after object recreation but
		// before the repair event, so retries still publish the notification.
		rejectionPending := false
		rejectionWireName := ""
		if t.pushCursor.Repair {
			wire, wireErr := ToWireRef(entry.Ref)
			if wireErr != nil {
				return published, false, wireErr
			}
			rejectionPending, err = folderJournalRejectionExists(
				kindRoot,
				wire.Name,
				entry.Identity,
			)
			if err != nil {
				return published, false, err
			}
			rejectionWireName = wire.Name
		}
		created, err := t.publishFolderEntryLocked(ctx, store, kindRoot, entry)
		if err != nil {
			return published, false, err
		}
		if created {
			published++
		}
		if !t.pushCursor.Repair || created || rejectionPending {
			if err := t.appendFolderJournalLocked(ctx, entry); err != nil {
				return published, false, err
			}
			if rejectionPending {
				if err := t.clearFolderJournalRejectionLocked(
					kindRoot,
					rejectionWireName,
				); err != nil {
					return published, false, err
				}
			}
		}
	}
	t.pushCursor = next
	if !more {
		t.publishedGeneration = generation
		t.pushCursor = folderPushCursor{}
	}
	return published, more, nil
}

func (t *folderTransport) removeAbandonedPublishTempsLocked(
	ctx context.Context,
	originRoot *os.Root,
	kind Kind,
) (retErr error) {
	kindRoot, err := openOptionalFolderSubroot(
		originRoot,
		string(kind),
		"kind",
	)
	if err != nil || kindRoot == nil {
		return err
	}
	defer func() {
		retErr = errors.Join(retErr, kindRoot.Close())
	}()
	return t.visitFolderDirectory(
		ctx,
		kindRoot,
		".",
		func(entry os.DirEntry) error {
			if strings.HasPrefix(entry.Name(), folderPublishTempPrefix) {
				return removeFolderFile(kindRoot, entry.Name())
			}
			return nil
		},
	)
}

func (t *folderTransport) pushKindLocked(
	ctx context.Context,
	store ArtifactStore,
	originRoot *os.Root,
	origin string,
	kind Kind,
	iterator EntryIterator,
	maxObjects int,
	maxBytes int64,
) (published int, processed int, processedBytes int64, more bool, retErr error) {
	var kindRoot *os.Root
	defer func() {
		if kindRoot != nil {
			retErr = errors.Join(retErr, kindRoot.Close())
		}
	}()
	for {
		pageLimit := transportStorePageSize
		if maxObjects > 0 {
			remaining := maxObjects - processed
			if remaining <= 0 {
				return published, processed, processedBytes, true, nil
			}
			pageLimit = min(pageLimit, remaining)
		}
		page, nextErr := iterator.Next(ctx, pageLimit)
		t.observeStorePageLocked(len(page))
		if len(page) > 0 && kindRoot == nil {
			var err error
			kindRoot, err = t.ensureFolderSubrootLocked(
				originRoot,
				string(kind),
				"kind",
			)
			if err != nil {
				return published, processed, processedBytes, false, err
			}
		}
		for _, entry := range page {
			if maxBytes > 0 && processed > 0 && entry.Identity.Size > maxBytes-processedBytes {
				return published, processed, processedBytes, true, nil
			}
			if entry.Ref.Origin != origin || entry.Ref.Kind != kind {
				return published, processed, processedBytes, false, fmt.Errorf(
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
				return published, processed, processedBytes, false, err
			}
			if created {
				published++
			}
			if err := t.appendFolderJournalLocked(ctx, entry); err != nil {
				return published, processed, processedBytes, false, err
			}
			processed++
			processedBytes += entry.Identity.Size
		}
		if errors.Is(nextErr, io.EOF) {
			return published, processed, processedBytes, false, nil
		}
		if nextErr != nil {
			return published, processed, processedBytes, false, nextErr
		}
	}
}

func discardArtifactEntries(
	ctx context.Context,
	iterator EntryIterator,
	count int,
) error {
	for count > 0 {
		page, err := iterator.Next(ctx, min(count, transportStorePageSize))
		count -= len(page)
		if errors.Is(err, io.EOF) {
			if count == 0 {
				return nil
			}
			return fmt.Errorf("%w: artifact publication cursor is past EOF", ErrArtifactConflict)
		}
		if err != nil {
			return err
		}
	}
	return nil
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
	if err := t.syncFolderDirectoryLocked(kindRoot); err != nil {
		return false, err
	}
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

func (t *folderTransport) ensureFolderSubrootLocked(
	parent *os.Root,
	name string,
	role string,
) (*os.Root, error) {
	root, err := openOptionalFolderSubroot(parent, name, role)
	if err != nil {
		return nil, err
	}
	if root != nil {
		if err := t.syncFolderDirectoryLocked(parent); err != nil {
			return nil, errors.Join(err, root.Close())
		}
		return root, nil
	}
	if err := parent.Mkdir(name, 0o755); err != nil &&
		!errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	if err := t.syncFolderDirectoryLocked(parent); err != nil {
		return nil, err
	}
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
