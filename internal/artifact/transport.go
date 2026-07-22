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
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

func openArtifactRoot(path, role string) (*os.Root, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("artifact %s root %s is not a directory", role, path)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, fmt.Errorf("opening artifact %s root: %w", role, err)
	}
	openedInfo, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("stating artifact %s root: %w", role, err)
	}
	currentInfo, err := os.Lstat(path)
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	if !currentInfo.IsDir() || !os.SameFile(openedInfo, currentInfo) {
		_ = root.Close()
		return nil, fmt.Errorf("artifact %s root %s changed while opening", role, path)
	}
	return root, nil
}

func quarantineArtifactRoot(root *os.Root, rel string) {
	dst := rel + quarantineSuffix
	_ = root.Remove(dst)
	err := root.Rename(rel, dst)
	path := filepath.Join(root.Name(), rel)
	switch {
	case err == nil:
		log.Printf("artifact: quarantined corrupt artifact %s", path)
	case !errors.Is(err, fs.ErrNotExist):
		log.Printf("artifact: quarantining %s: %v", path, err)
	}
}

func openArtifactSubroot(parent *os.Root, rel, role string) (*os.Root, error) {
	info, err := parent.Lstat(rel)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("artifact %s %s is not a directory", role, rel)
	}
	root, err := parent.OpenRoot(rel)
	if err != nil {
		return nil, fmt.Errorf("opening artifact %s: %w", role, err)
	}
	openedInfo, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("stating artifact %s: %w", role, err)
	}
	currentInfo, err := parent.Lstat(rel)
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	if !currentInfo.IsDir() || !os.SameFile(openedInfo, currentInfo) {
		_ = root.Close()
		return nil, fmt.Errorf("artifact %s %s changed while opening", role, rel)
	}
	return root, nil
}

func openRootRegularFile(root *os.Root, name string) (*os.File, fs.FileInfo, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("artifact source %s is not a regular file",
			filepath.Join(root.Name(), name))
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
	if !after.Mode().IsRegular() || !os.SameFile(before, opened) || !os.SameFile(opened, after) {
		_ = file.Close()
		return nil, nil, fmt.Errorf("artifact source %s changed while opening",
			filepath.Join(root.Name(), name))
	}
	return file, opened, nil
}

func createRootTemp(root *os.Root, dir string) (*os.File, string, error) {
	for range 100 {
		var suffix [8]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return nil, "", err
		}
		rel := filepath.Join(dir, tempFilePrefix+hex.EncodeToString(suffix[:]))
		file, err := root.OpenFile(rel, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return file, rel, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, "", err
		}
	}
	return nil, "", errors.New("creating temporary artifact file: too many collisions")
}

const (
	transportPageSize     = 512
	transportPageMaxBytes = 1 << 20
	quarantineSuffix      = ".corrupt"
)

var transportKinds = [...]Kind{
	KindSegments,
	KindRaw,
	KindManifests,
	KindMeta,
	KindCheckpoints,
}

var errArtifactPathConflict = errors.New("artifact path conflict")

func readTransportPage(body io.Reader) ([]byte, error) {
	page, err := io.ReadAll(io.LimitReader(body, transportPageMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(page) > transportPageMaxBytes {
		return nil, fmt.Errorf("%w: transport page response exceeds %d bytes",
			ErrArtifactInvalid, transportPageMaxBytes)
	}
	return page, nil
}

// Transport exchanges immutable artifacts through the canonical logical store
// boundary. A transport never receives or derives the store's physical root.
type Transport interface {
	Prepare(context.Context, ArtifactStore) error
	Exchange(context.Context, ArtifactStore) error
}

// transportRepairStore is implemented by a store decorator that binds the
// durable SQLite repair queue and StoreImportCoordinator to a canonical store.
// The point lookup keeps equal-name repair checks bounded; completion owns the
// exact-identity repair, acknowledgement, and coalesced import scheduling.
type transportRepairStore interface {
	PendingTransportRepair(context.Context, Ref) (Entry, bool, error)
	RepairTransportArtifact(context.Context, Entry, io.Reader) error
}

// folderTransport owns only its external wire directory. It deliberately does
// not wrap that directory in filesystemStore: the share has its own layout,
// encoding, confinement, and no-follow rules.
type folderTransport struct {
	mu     sync.Mutex
	target string
	root   *os.Root
	closed bool
}

func (t *folderTransport) Prepare(ctx context.Context, _ ArtifactStore) error {
	if err := requireTransportContext(ctx); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return fs.ErrClosed
	}
	if strings.TrimSpace(t.target) == "" || t.root == nil {
		return fmt.Errorf("%w: artifact sync target is required", ErrArtifactInvalid)
	}
	_, err := t.root.Stat(".")
	return err
}

func (t *folderTransport) Exchange(
	ctx context.Context, local ArtifactStore,
) (retErr error) {
	if err := validateTransportStore(ctx, local); err != nil {
		return err
	}
	defer func() {
		if retErr == nil {
			NotifyArtifactBatch(ctx, local)
		}
	}()
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.root == nil {
		return fs.ErrClosed
	}
	target := t.root

	// Pull the external share first. Each directory page is processed before
	// the next one is read, and local membership is a point lookup.
	if err := visitFolderOrigins(ctx, target, func(origin string) error {
		for _, kind := range transportKinds {
			if err := visitFolderKind(ctx, target, origin, kind, func(wire WireRef) error {
				ref, err := FromWireRef(wire.Origin, wire.Kind, wire.Name)
				if err != nil {
					return err
				}
				entry, found, err := findStoreEntry(ctx, local, ref)
				if err != nil {
					return err
				}
				if found {
					repaired, err := repairQueuedTransportArtifact(ctx, local, wire,
						func(consume func(io.Reader) error) error {
							file, _, openErr := openRootRegularFile(target, folderWirePath(wire))
							if openErr != nil {
								return openErr
							}
							defer file.Close()
							return consume(file)
						})
					if err != nil {
						return err
					}
					if repaired {
						return nil
					}
					if kind == KindCheckpoints {
						return compareFolderCheckpoint(ctx, local, entry, target, wire)
					}
					return nil
				}
				if err := receiveFolderWire(ctx, local, target, wire); err != nil {
					if errors.Is(err, ErrArtifactInvalid) || errors.Is(err, ErrArtifactCorrupt) {
						return nil
					}
					return err
				}
				return nil
			}); err != nil {
				return fmt.Errorf("fetching %s artifacts for %s: %w", kind, origin, err)
			}
		}
		return nil
	}); err != nil {
		return err
	}

	// Publish local pages in dependency-before-checkpoint order. Remote
	// membership remains a confined point lookup, so no full share index is
	// materialized.
	return visitTransportStoreOrigins(ctx, local, func(origin string) error {
		for _, kind := range transportKinds {
			if err := visitStoreKind(ctx, local, origin, kind, func(entry Entry) error {
				wire, err := ToWireRef(entry.Ref)
				if err != nil {
					return err
				}
				has, err := folderHasWire(target, wire)
				if err != nil {
					return err
				}
				if has {
					repaired, err := repairQueuedTransportArtifact(ctx, local, wire,
						func(consume func(io.Reader) error) error {
							file, _, openErr := openRootRegularFile(target, folderWirePath(wire))
							if openErr != nil {
								return openErr
							}
							defer file.Close()
							return consume(file)
						})
					if err != nil {
						return err
					}
					if repaired {
						return nil
					}
					if kind == KindCheckpoints {
						return compareFolderCheckpoint(ctx, local, entry, target, wire)
					}
					matches, err := folderWireMatchesEntry(ctx, target, wire, entry)
					if errors.Is(err, fs.ErrPermission) {
						return nil
					}
					if err == nil && matches {
						return nil
					}
					if err != nil && !errors.Is(err, ErrArtifactInvalid) && !errors.Is(err, ErrArtifactCorrupt) {
						return err
					}
					quarantineArtifactRoot(target, folderWirePath(wire))
					return publishFolderWire(ctx, local, target, entry)
				}
				return publishFolderWire(ctx, local, target, entry)
			}); err != nil {
				return fmt.Errorf("publishing %s artifacts for %s: %w", kind, origin, err)
			}
		}
		return nil
	})
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

func openFolderTransport(target string) (*folderTransport, error) {
	if strings.TrimSpace(target) == "" {
		return nil, fmt.Errorf("%w: artifact sync target is required", ErrArtifactInvalid)
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return nil, fmt.Errorf("creating artifact sync target: %w", err)
	}
	canonical, err := canonicalArtifactPath(target)
	if err != nil {
		return nil, fmt.Errorf("resolving artifact sync target: %w", err)
	}
	root, err := openArtifactRoot(canonical, "target exchange")
	if err != nil {
		return nil, err
	}
	return &folderTransport{target: canonical, root: root}, nil
}

func requireTransportContext(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("%w: context is required", ErrArtifactInvalid)
	}
	return ctx.Err()
}

func validateTransportStore(ctx context.Context, store ArtifactStore) error {
	if err := requireTransportContext(ctx); err != nil {
		return err
	}
	if store == nil || isTypedNil(store) {
		return fmt.Errorf("%w: artifact store is required", ErrArtifactInvalid)
	}
	return nil
}

func visitTransportStoreOrigins(
	ctx context.Context, store ArtifactStore, visit func(string) error,
) (retErr error) {
	var cursor Cursor
	defer func() { retErr = errors.Join(retErr, releaseArtifactCursor(store, &cursor)) }()
	var guard boundedCursorCycleGuard
	for {
		origins, next, err := store.ListOrigins(ctx, cursor, transportPageSize)
		if err != nil {
			return err
		}
		cursor = next
		for _, origin := range origins {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := visit(origin); err != nil {
				return err
			}
		}
		if cursor == "" {
			return nil
		}
		if guard.Observe(cursor) {
			return errors.New("artifact origin cursor cycle")
		}
	}
}

type artifactItem struct {
	kind string
	name string
}

func indexItems(index OriginArtifactIndex) []artifactItem {
	items := make([]artifactItem, 0,
		len(index.Segments)+len(index.Raw)+len(index.Manifests)+len(index.Meta)+len(index.Checkpoints))
	for _, group := range []struct {
		kind  string
		names []string
	}{
		{KindSegments, index.Segments},
		{KindRaw, index.Raw},
		{KindManifests, index.Manifests},
		{KindMeta, index.Meta},
		{KindCheckpoints, index.Checkpoints},
	} {
		for _, name := range group.names {
			items = append(items, artifactItem{kind: group.kind, name: name})
		}
	}
	return items
}

func visitStoreKind(
	ctx context.Context,
	store ArtifactStore,
	origin string,
	kind Kind,
	visit func(Entry) error,
) (retErr error) {
	var cursor Cursor
	defer func() { retErr = errors.Join(retErr, releaseArtifactCursor(store, &cursor)) }()
	var guard boundedCursorCycleGuard
	for {
		page, err := store.List(ctx, origin, kind, cursor, transportPageSize)
		if err != nil {
			return err
		}
		cursor = page.Next
		for _, entry := range page.Items {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := visit(entry); err != nil {
				return err
			}
		}
		if cursor == "" {
			return nil
		}
		if guard.Observe(cursor) {
			return errors.New("artifact list cursor cycle")
		}
	}
}

func findStoreEntry(
	ctx context.Context, store ArtifactStore, ref Ref,
) (Entry, bool, error) {
	entry, err := store.Stat(ctx, ref)
	if errors.Is(err, ErrArtifactNotFound) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, err
	}
	return entry, true, nil
}

func repairQueuedTransportArtifact(
	ctx context.Context,
	store ArtifactStore,
	wire WireRef,
	open func(func(io.Reader) error) error,
) (repaired bool, retErr error) {
	queue, ok := store.(transportRepairStore)
	if !ok {
		return false, nil
	}
	ref, err := FromWireRef(wire.Origin, wire.Kind, wire.Name)
	if err != nil {
		return false, err
	}
	request, pending, err := queue.PendingTransportRepair(ctx, ref)
	if err != nil || !pending {
		return false, err
	}
	err = open(func(body io.Reader) (callbackErr error) {
		spool, identity, err := spoolCanonicalWire(
			ctx, wire, body, transportWireLimits(wire.Kind),
		)
		if err != nil {
			return err
		}
		defer func() {
			callbackErr = errors.Join(callbackErr, closeAndRemoveTransportSpool(spool))
		}()
		if identity != request.Identity {
			return fmt.Errorf(
				"%w: trusted repair identity differs for %s/%s/%s",
				ErrArtifactConflict, ref.Origin, ref.Kind, ref.Name,
			)
		}
		return queue.RepairTransportArtifact(ctx, request, spool)
	})
	return err == nil, err
}

func visitFolderOrigins(
	ctx context.Context, root *os.Root, visit func(string) error,
) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := dir.ReadDir(transportPageSize)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		for _, entry := range entries {
			origin := entry.Name()
			if validateOriginID(origin) != nil {
				continue
			}
			info, statErr := root.Lstat(origin)
			if statErr != nil {
				return statErr
			}
			if !info.IsDir() {
				return fmt.Errorf("artifact origin %s is not a directory", origin)
			}
			if err := visit(origin); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
	}
}

func visitFolderKind(
	ctx context.Context,
	root *os.Root,
	origin string,
	kind Kind,
	visit func(WireRef) error,
) error {
	rel := filepath.Join(origin, string(kind))
	info, err := root.Lstat(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("artifact kind %s is not a directory", kind)
	}
	kindRoot, err := openArtifactSubroot(root, rel, "folder kind")
	if err != nil {
		return err
	}
	defer kindRoot.Close()
	dir, err := kindRoot.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := dir.ReadDir(transportPageSize)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		for _, entry := range entries {
			name := entry.Name()
			if isTempArtifactEntry(name) {
				continue
			}
			ref, refErr := FromWireRef(origin, kind, name)
			if refErr != nil {
				continue
			}
			fileInfo, statErr := kindRoot.Lstat(name)
			if statErr != nil {
				return statErr
			}
			if !fileInfo.Mode().IsRegular() {
				return fmt.Errorf("artifact %s/%s is not a regular file", kind, name)
			}
			wire, refErr := ToWireRef(ref)
			if refErr != nil {
				return refErr
			}
			if err := visit(wire); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
	}
}

func folderWirePath(wire WireRef) string {
	return filepath.Join(wire.Origin, string(wire.Kind), wire.Name)
}

func folderHasWire(root *os.Root, wire WireRef) (bool, error) {
	info, err := root.Lstat(folderWirePath(wire))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("artifact destination %s is not a regular file", folderWirePath(wire))
	}
	return true, nil
}

func folderWireMatchesEntry(
	ctx context.Context, root *os.Root, wire WireRef, entry Entry,
) (bool, error) {
	file, _, err := openRootRegularFile(root, folderWirePath(wire))
	if err != nil {
		return false, err
	}
	defer file.Close()
	identity, err := wireIdentity(ctx, wire, file, transportWireLimits(wire.Kind))
	if err != nil {
		return false, err
	}
	return identity == entry.Identity, nil
}

func receiveFolderWire(
	ctx context.Context, store ArtifactStore, root *os.Root, wire WireRef,
) error {
	file, _, err := openRootRegularFile(root, folderWirePath(wire))
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = CreateFromWire(ctx, store, wire, file, transportWireLimits(wire.Kind))
	return err
}

func publishFolderWire(
	ctx context.Context, store ArtifactStore, root *os.Root, entry Entry,
) (retErr error) {
	wire, err := ToWireRef(entry.Ref)
	if err != nil {
		return err
	}
	spool, size, _, err := spoolWireArtifact(ctx, store, entry)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, closeAndRemoveTransportSpool(spool)) }()
	rel := folderWirePath(wire)
	if err := root.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		return err
	}
	tmp, tmpRel, err := createRootTemp(root, filepath.Dir(rel))
	if err != nil {
		return err
	}
	defer func() { _ = root.Remove(tmpRel) }()
	if _, err := io.CopyN(tmp, spool, size); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := root.Link(tmpRel, rel); err != nil {
		if errors.Is(err, fs.ErrExist) {
			has, statErr := folderHasWire(root, wire)
			if statErr != nil {
				return statErr
			}
			if has {
				matches, matchErr := folderWireMatchesEntry(ctx, root, wire, entry)
				if matchErr == nil && matches {
					return nil
				}
				return fmt.Errorf("%w: immutable artifact collision at %s: %v",
					ErrArtifactConflict, rel, matchErr)
			}
		}
		return fmt.Errorf("publishing immutable artifact %s: %w", rel, err)
	}
	return nil
}

func compareFolderCheckpoint(
	ctx context.Context, store ArtifactStore, local Entry, root *os.Root, wire WireRef,
) error {
	ref, err := FromWireRef(wire.Origin, wire.Kind, wire.Name)
	if err != nil {
		return err
	}
	file, _, err := openRootRegularFile(root, folderWirePath(wire))
	if err != nil {
		return err
	}
	defer file.Close()
	return compareOrRepairCheckpoint(ctx, store, local, wire, file,
		"target", ref.Origin, ref.Name)
}

func spoolWireArtifact(
	ctx context.Context, store ArtifactStore, expected Entry,
) (_ *os.File, _ int64, _ string, retErr error) {
	entry, reader, err := store.Open(ctx, expected.Ref)
	if err != nil {
		return nil, 0, "", err
	}
	defer func() { retErr = errors.Join(retErr, reader.Close()) }()
	if entry.Ref != expected.Ref || entry.Identity != expected.Identity {
		return nil, 0, "", fmt.Errorf("%w: artifact changed while opening", ErrArtifactConflict)
	}
	spool, err := os.CreateTemp("", "agentsview-artifact-wire-*")
	if err != nil {
		return nil, 0, "", err
	}
	cleanup := true
	defer func() {
		if cleanup {
			retErr = errors.Join(retErr, closeAndRemoveTransportSpool(spool))
		}
	}()
	if err := spool.Chmod(0o600); err != nil {
		return nil, 0, "", err
	}
	hasher := sha256.New()
	if err := EncodeWire(ctx, expected.Ref, reader, io.MultiWriter(spool, hasher)); err != nil {
		return nil, 0, "", err
	}
	if err := reader.Verify(); err != nil {
		return nil, 0, "", err
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, "", err
	}
	info, err := spool.Stat()
	if err != nil {
		return nil, 0, "", err
	}
	if err := spool.Sync(); err != nil {
		return nil, 0, "", err
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return nil, 0, "", err
	}
	cleanup = false
	return spool, info.Size(), hex.EncodeToString(hasher.Sum(nil)), nil
}

func wireIdentity(
	ctx context.Context, wire WireRef, src io.Reader, limits WireLimits,
) (Identity, error) {
	hasher := sha256.New()
	var size int64
	writer := writerFunc(func(p []byte) (int, error) {
		n, err := hasher.Write(p)
		size += int64(n)
		return n, err
	})
	if err := DecodeWire(ctx, wire, src, writer, limits); err != nil {
		return Identity{}, err
	}
	return NewIdentity(hex.EncodeToString(hasher.Sum(nil)), size)
}

func compareOrRepairCheckpoint(
	ctx context.Context,
	store ArtifactStore,
	local Entry,
	wire WireRef,
	remote io.Reader,
	remoteLabel, origin, name string,
) (retErr error) {
	spool, identity, err := spoolCanonicalWire(ctx, wire, remote, transportWireLimits(wire.Kind))
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, closeAndRemoveTransportSpool(spool)) }()
	if identity != local.Identity {
		return fmt.Errorf(
			"%w: checkpoint %s/%s differs between the local store and the %s; was this origin's artifact store rebuilt or its origin id reused?",
			errArtifactPathConflict, origin, name, remoteLabel,
		)
	}
	if err := verifyStoreEntry(ctx, store, local); err == nil {
		return nil
	} else if !errors.Is(err, ErrArtifactCorrupt) && !errors.Is(err, ErrArtifactInvalid) {
		return err
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if repairer, ok := store.(artifactContentRepairer); ok {
		return repairer.RepairContent(ctx, local.Identity, spool)
	}
	if err := store.Quarantine(ctx, local.Ref, "transport verified-read failure"); err != nil {
		return err
	}
	_, err = store.Create(ctx, local.Ref, local.Identity,
		canonicalArtifactMediaType(local.Ref.Kind), spool)
	return err
}

func verifyStoreEntry(ctx context.Context, store ArtifactStore, expected Entry) (retErr error) {
	entry, reader, err := store.Open(ctx, expected.Ref)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, reader.Close()) }()
	if entry.Identity != expected.Identity {
		return fmt.Errorf("%w: artifact identity changed while opening", ErrArtifactCorrupt)
	}
	if _, err := io.Copy(io.Discard, &wireContextReader{ctx: ctx, reader: reader}); err != nil {
		return err
	}
	return reader.Verify()
}

func spoolCanonicalWire(
	ctx context.Context, wire WireRef, src io.Reader, limits WireLimits,
) (_ *os.File, _ Identity, retErr error) {
	spool, err := os.CreateTemp("", "agentsview-artifact-canonical-*")
	if err != nil {
		return nil, Identity{}, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			retErr = errors.Join(retErr, closeAndRemoveTransportSpool(spool))
		}
	}()
	if err := spool.Chmod(0o600); err != nil {
		return nil, Identity{}, err
	}
	hasher := sha256.New()
	var size int64
	writer := writerFunc(func(p []byte) (int, error) {
		n, err := io.MultiWriter(spool, hasher).Write(p)
		size += int64(n)
		return n, err
	})
	if err := DecodeWire(ctx, wire, src, writer, limits); err != nil {
		return nil, Identity{}, err
	}
	identity, err := NewIdentity(hex.EncodeToString(hasher.Sum(nil)), size)
	if err != nil {
		return nil, Identity{}, err
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return nil, Identity{}, err
	}
	cleanup = false
	return spool, identity, nil
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func transportWireLimits(kind Kind) WireLimits {
	var decoded int64
	switch kind {
	case KindManifests, KindMeta:
		decoded = manifestDecodedLimit
	case KindSegments, KindCheckpoints:
		decoded = segmentDecodedLimit
	case KindRaw:
		decoded = 1 << 40
	default:
		decoded = 1
	}
	return WireLimits{MaxEncodedBytes: decoded + (1 << 20), MaxDecodedBytes: decoded}
}

func closeAndRemoveTransportSpool(file *os.File) error {
	if file == nil {
		return nil
	}
	name := file.Name()
	return errors.Join(file.Close(), removeTransportSpool(name))
}

func removeTransportSpool(name string) error {
	err := os.Remove(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
