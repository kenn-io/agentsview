package artifact

import (
	"bytes"
	"context"
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
	"go.kenn.io/docbank"
	docsqlite "go.kenn.io/docbank/pkg/sqlite"
)

const (
	repositoryDirectory              = "artifacts"
	repositoryLooseCompressionBytes  = int64(4 << 10)
	repositoryLooseCompressionSaving = 10
	repositoryOwnershipMarker        = ".agentsview-artifact-repository-v1"
	repositoryOwnershipMarkerPending = repositoryOwnershipMarker + ".pending"
	repositoryOwnershipLockSuffix    = ".agentsview-open.lock"
	repositoryOwnershipMarkerContent = "agentsview artifact repository\nversion=1\n"
)

// Repository owns the process's single local artifact store and its Docbank
// vault lifecycle. The retained directory handle is used only to reject
// overlapping external targets; transports receive the logical store instead.
type Repository struct {
	mu sync.Mutex

	backend         ArtifactStore
	store           ArtifactStore
	root            *os.Root
	rootIdentity    fs.FileInfo
	rootPath        string
	rootReservation *repositoryRootReservation
	driver          docsqlite.Driver
	closed          bool
}

// OpenRepository opens the dedicated Docbank vault below dataDir.
func OpenRepository(ctx context.Context, dataDir string) (*Repository, error) {
	return openRepository(ctx, dataDir, nil)
}

func openRepository(
	ctx context.Context, dataDir string, driver docsqlite.Driver,
) (*Repository, error) {
	return openRepositoryWith(ctx, dataDir, driver, repositoryOpenHooks{})
}

type repositoryOpenHooks struct {
	openRoot        func(string) (*os.Root, error)
	validateRoot    func(*os.Root, string) (fs.FileInfo, error)
	beforeVaultOpen func(*os.Root, string) error
	closeVault      func(*docbank.Vault) error
}

func openRepositoryWith(
	ctx context.Context,
	dataDir string,
	driver docsqlite.Driver,
	hooks repositoryOpenHooks,
) (_ *Repository, retErr error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: repository context is required", ErrArtifactInvalid)
	}
	if strings.TrimSpace(dataDir) == "" {
		return nil, fmt.Errorf("%w: repository data directory is required", ErrArtifactInvalid)
	}
	rootPath, err := canonicalRepositoryRoot(dataDir)
	if err != nil {
		return nil, err
	}
	rootReservation, err := reserveRepositoryRoot(rootPath)
	if err != nil {
		return nil, err
	}
	reservationOpen := true
	defer func() {
		if reservationOpen {
			rootReservation.Close()
		}
	}()
	if hooks.openRoot == nil {
		hooks.openRoot = os.OpenRoot
	}
	if hooks.validateRoot == nil {
		hooks.validateRoot = validateRepositoryRoot
	}
	if hooks.beforeVaultOpen == nil {
		hooks.beforeVaultOpen = func(*os.Root, string) error { return nil }
	}
	if hooks.closeVault == nil {
		hooks.closeVault = func(vault *docbank.Vault) error { return vault.Close() }
	}
	if err := rejectOwnedRepositoryAncestor(rootPath); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(rootPath), 0o700); err != nil {
		return nil, fmt.Errorf("creating artifact repository parent: %w", err)
	}
	ownershipLock, err := acquireRepositoryOwnershipLock(ctx, rootPath)
	if err != nil {
		return nil, err
	}
	lockOpen := true
	defer func() {
		if lockOpen {
			retErr = errors.Join(retErr, ownershipLock.Close())
		}
	}()
	if err := rejectOwnedRepositoryAncestor(rootPath); err != nil {
		return nil, err
	}

	info, err := os.Lstat(rootPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.Mkdir(rootPath, 0o700); err != nil {
			return nil, fmt.Errorf("creating artifact repository root: %w", err)
		}
	case err != nil:
		return nil, fmt.Errorf("checking artifact repository layout: %w", err)
	case !info.IsDir():
		return nil, fmt.Errorf(
			"artifact repository path is not a Docbank vault; move or remove %s and retry",
			rootPath,
		)
	}

	root, err := hooks.openRoot(rootPath)
	if err != nil {
		return nil, fmt.Errorf("opening artifact repository identity: %w", err)
	}
	rootOpen := true
	defer func() {
		if rootOpen {
			retErr = errors.Join(retErr, root.Close())
		}
	}()
	openedIdentity, err := validateRepositoryRoot(root, rootPath)
	if err != nil {
		return nil, err
	}
	if err := ensureRepositoryOwnership(root, rootPath); err != nil {
		return nil, err
	}
	if err := hooks.beforeVaultOpen(root, rootPath); err != nil {
		return nil, err
	}
	preOpenIdentity, err := hooks.validateRoot(root, rootPath)
	if err != nil {
		return nil, err
	}
	if !os.SameFile(openedIdentity, preOpenIdentity) {
		return nil, errors.New("artifact repository root changed while opening")
	}

	vault, err := docbank.New(ctx, docbank.Config{
		Root:   rootPath,
		SQLite: driver,
		LooseCompression: docbank.LooseCompressionOptions{
			Enabled:           true,
			MinBytes:          repositoryLooseCompressionBytes,
			MinSavingsPercent: repositoryLooseCompressionSaving,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("opening artifact repository: %w", err)
	}
	vaultOpen := true
	defer func() {
		if vaultOpen {
			retErr = errors.Join(retErr, hooks.closeVault(vault))
		}
	}()
	postOpenIdentity, err := hooks.validateRoot(root, rootPath)
	if err != nil {
		return nil, err
	}
	if !os.SameFile(openedIdentity, postOpenIdentity) {
		return nil, errors.New("artifact repository root changed while opening")
	}
	if err := ownershipLock.Close(); err != nil {
		return nil, err
	}
	lockOpen = false
	rootOpen = false
	vaultOpen = false
	reservationOpen = false
	repository := &Repository{
		backend:         newDocbankStore(vault),
		root:            root,
		rootIdentity:    openedIdentity,
		rootPath:        rootPath,
		rootReservation: rootReservation,
		driver:          driver,
	}
	repository.store = &repositoryStore{
		ArtifactStore: repository.backend,
		owner:         repository,
	}
	return repository, nil
}

type repositoryStore struct {
	ArtifactStore
	owner *Repository
}

func (s *repositoryStore) NewFolderTransport(target string) (Transport, error) {
	return s.owner.NewFolderTransport(target)
}

func (s *repositoryStore) Close() error { return s.owner.Close() }

func (s *repositoryStore) ArtifactImportScope() *ArtifactImportScope {
	if provider, ok := s.ArtifactStore.(ArtifactImportScopeProvider); ok {
		return provider.ArtifactImportScope()
	}
	return nil
}

func (s *repositoryStore) NotifyArtifactBatch(ctx context.Context) {
	if notifier, ok := s.ArtifactStore.(ArtifactBatchNotifier); ok {
		notifier.NotifyArtifactBatch(ctx)
	}
}

func (s *repositoryStore) ListQuarantined(
	ctx context.Context, cursor Cursor, limit int,
) ([]QuarantinedEntry, Cursor, error) {
	store, ok := s.ArtifactStore.(ArtifactQuarantineStore)
	if !ok {
		return nil, "", ErrArtifactUnsupported
	}
	return store.ListQuarantined(ctx, cursor, limit)
}

func (s *repositoryStore) TrashQuarantined(ctx context.Context, token string) error {
	store, ok := s.ArtifactStore.(ArtifactQuarantineStore)
	if !ok {
		return ErrArtifactUnsupported
	}
	return store.TrashQuarantined(ctx, token)
}

func (s *repositoryStore) Verify(
	ctx context.Context, budget WorkBudget,
) (MaintenanceResult, error) {
	maintainer, ok := s.ArtifactStore.(ArtifactMaintainer)
	if !ok {
		return MaintenanceResult{}, ErrArtifactUnsupported
	}
	return maintainer.Verify(ctx, budget)
}

func (s *repositoryStore) EmptyTrash(
	ctx context.Context, grace time.Duration, budget WorkBudget,
) (MaintenanceResult, error) {
	maintainer, ok := s.ArtifactStore.(ArtifactMaintainer)
	if !ok {
		return MaintenanceResult{}, ErrArtifactUnsupported
	}
	return maintainer.EmptyTrash(ctx, grace, budget)
}

func (s *repositoryStore) GarbageCollect(
	ctx context.Context, budget WorkBudget,
) (MaintenanceResult, error) {
	maintainer, ok := s.ArtifactStore.(ArtifactMaintainer)
	if !ok {
		return MaintenanceResult{}, ErrArtifactUnsupported
	}
	return maintainer.GarbageCollect(ctx, budget)
}

func (s *repositoryStore) Repack(
	ctx context.Context, budget WorkBudget,
) (MaintenanceResult, error) {
	maintainer, ok := s.ArtifactStore.(ArtifactMaintainer)
	if !ok {
		return MaintenanceResult{}, ErrArtifactUnsupported
	}
	return maintainer.Repack(ctx, budget)
}

func (s *repositoryStore) ReleaseCursor(cursor Cursor) error {
	if releaser, ok := s.ArtifactStore.(ArtifactCursorReleaser); ok {
		return releaser.ReleaseCursor(cursor)
	}
	return nil
}

func (s *repositoryStore) RepairContent(
	ctx context.Context, identity Identity, trusted io.Reader,
) error {
	repairer, ok := s.ArtifactStore.(artifactContentRepairer)
	if !ok {
		return ErrArtifactUnsupported
	}
	return repairer.RepairContent(ctx, identity, trusted)
}

func validateRepositoryRoot(root *os.Root, rootPath string) (fs.FileInfo, error) {
	identity, err := root.Stat(".")
	if err != nil {
		return nil, fmt.Errorf("stating artifact repository identity: %w", err)
	}
	current, err := os.Stat(rootPath)
	if err != nil {
		return nil, fmt.Errorf("rechecking artifact repository identity: %w", err)
	}
	if !os.SameFile(identity, current) {
		return nil, errors.New("artifact repository root changed while opening")
	}
	return identity, nil
}

// Store returns the one logical adapter shared by every repository consumer.
func (r *Repository) Store() ArtifactStore {
	if r == nil {
		return nil
	}
	return r.store
}

// RepositoryFromStore returns the repository owner behind the logical store
// returned by Repository.Store. It is used by the daemon's exclusive reset
// lifecycle after all ordinary store leases have drained.
func RepositoryFromStore(store ArtifactStore) (*Repository, bool) {
	owned, ok := store.(*repositoryStore)
	if !ok || owned.owner == nil {
		return nil, false
	}
	return owned.owner, true
}

// Closed reports whether repository ownership has already been released or
// transferred by an explicit reset.
func (r *Repository) Closed() bool {
	if r == nil {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

// NewFolderTransport validates an external wire directory against the
// repository's retained opened-root identity before constructing a transport.
// The transport receives only the external target and never the vault path.
func (r *Repository) NewFolderTransport(target string) (Transport, error) {
	transport, err := openFolderTransport(target)
	if err != nil {
		return nil, err
	}
	identity, err := transport.root.Stat(".")
	if err != nil {
		return nil, errors.Join(err, transport.Close())
	}
	if err := r.validateOpenedExternalTarget(transport.target, identity); err != nil {
		return nil, errors.Join(err, transport.Close())
	}
	return transport, nil
}

// Close waits for active verified readers through the store, then releases the
// retained root identity. It is safe to call more than once.
func (r *Repository) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	storeErr := r.backend.Close()
	rootErr := r.root.Close()
	r.rootReservation.Close()
	return errors.Join(storeErr, rootErr)
}

func (r *Repository) validateExternalTarget(target string) error {
	if r == nil {
		return fmt.Errorf("%w: artifact repository is required", ErrArtifactInvalid)
	}
	if strings.TrimSpace(target) == "" {
		return fmt.Errorf("%w: external artifact target is required", ErrArtifactInvalid)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return fs.ErrClosed
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return fmt.Errorf("resolving external artifact target: %w", err)
	}
	targetCanonical, err := canonicalArtifactPath(targetAbs)
	if err != nil {
		return fmt.Errorf("resolving external artifact target symlinks: %w", err)
	}
	if rootsOverlap(r.rootPath, targetCanonical) ||
		targetIdentityChainContains(targetCanonical, r.rootIdentity) {
		return fmt.Errorf(
			"external artifact target %s must not overlap local artifact repository",
			targetCanonical,
		)
	}
	current, err := os.Stat(r.rootPath)
	if err != nil || !os.SameFile(r.rootIdentity, current) {
		return fmt.Errorf(
			"external artifact target %s cannot prove non-overlap because the local artifact repository is no longer reachable at its canonical path",
			targetCanonical,
		)
	}
	return nil
}

func (r *Repository) validateOpenedExternalTarget(
	targetCanonical string, targetIdentity fs.FileInfo,
) error {
	if r == nil {
		return fmt.Errorf("%w: artifact repository is required", ErrArtifactInvalid)
	}
	if strings.TrimSpace(targetCanonical) == "" || targetIdentity == nil {
		return fmt.Errorf("%w: opened external artifact target is required", ErrArtifactInvalid)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return fs.ErrClosed
	}
	return validateOpenedRootDisjoint(
		r.rootPath, r.rootIdentity, targetCanonical, targetIdentity,
	)
}

func validateOpenedRootDisjoint(
	rootPath string,
	rootIdentity fs.FileInfo,
	targetCanonical string,
	targetIdentity fs.FileInfo,
) error {
	currentTarget, err := os.Stat(targetCanonical)
	if err != nil || !os.SameFile(targetIdentity, currentTarget) {
		return fmt.Errorf(
			"external artifact target %s changed while validating its opened identity",
			targetCanonical,
		)
	}
	if rootsOverlap(rootPath, targetCanonical) ||
		targetIdentityChainContains(targetCanonical, rootIdentity) ||
		targetIdentityChainContains(rootPath, targetIdentity) ||
		os.SameFile(rootIdentity, targetIdentity) {
		return fmt.Errorf(
			"external artifact target %s must not overlap local artifact repository",
			targetCanonical,
		)
	}
	currentRoot, err := os.Stat(rootPath)
	if err != nil || !os.SameFile(rootIdentity, currentRoot) {
		return fmt.Errorf(
			"external artifact target %s cannot prove non-overlap because the local artifact repository is no longer reachable at its canonical path",
			targetCanonical,
		)
	}
	return nil
}

func canonicalRepositoryRoot(dataDir string) (string, error) {
	dataAbs, err := filepath.Abs(dataDir)
	if err != nil {
		return "", fmt.Errorf("resolving artifact repository root: %w", err)
	}
	rootPath, err := canonicalArtifactPath(filepath.Join(dataAbs, repositoryDirectory))
	if err != nil {
		return "", fmt.Errorf("resolving artifact repository root symlinks: %w", err)
	}
	return filepath.Clean(rootPath), nil
}

type repositoryOwnershipLock struct {
	path string
	lock *flock.Flock
}

var liveRepositoryRoots repositoryRootRegistry

type repositoryRootRegistry struct {
	mu    sync.Mutex
	roots map[string]struct{}
}

type repositoryRootReservation struct {
	registry *repositoryRootRegistry
	rootPath string
	once     sync.Once
}

func reserveRepositoryRoot(rootPath string) (*repositoryRootReservation, error) {
	liveRepositoryRoots.mu.Lock()
	defer liveRepositoryRoots.mu.Unlock()
	for reservedRoot := range liveRepositoryRoots.roots {
		if rootsOverlap(rootPath, reservedRoot) {
			return nil, repositoryOverlapLockError(reservedRoot)
		}
	}
	if liveRepositoryRoots.roots == nil {
		liveRepositoryRoots.roots = make(map[string]struct{})
	}
	liveRepositoryRoots.roots[rootPath] = struct{}{}
	return &repositoryRootReservation{
		registry: &liveRepositoryRoots,
		rootPath: rootPath,
	}, nil
}

func (r *repositoryRootReservation) Close() {
	if r == nil || r.registry == nil {
		return
	}
	r.once.Do(func() {
		r.registry.mu.Lock()
		defer r.registry.mu.Unlock()
		delete(r.registry.roots, r.rootPath)
	})
}

func repositoryOverlapLockError(reservedRoot string) error {
	return fmt.Errorf(
		"artifact repository ownership lock: vault is locked by overlapping live repository %s",
		reservedRoot,
	)
}

func acquireRepositoryOwnershipLock(
	ctx context.Context, rootPath string,
) (*repositoryOwnershipLock, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	path := rootPath + repositoryOwnershipLockSuffix
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&fs.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("artifact repository ownership lock has an invalid file type: %s", path)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("checking artifact repository ownership lock: %w", err)
	}
	lock := flock.New(path, flock.SetPermissions(0o600))
	locked, err := lock.TryLock()
	if err != nil {
		return nil, fmt.Errorf("acquiring artifact repository ownership lock: %w", err)
	}
	if !locked {
		return nil, fmt.Errorf("artifact repository ownership lock is held for %s", rootPath)
	}
	lockedInfo, err := lock.Stat()
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("stating artifact repository ownership lock: %w", err),
			lock.Close(),
		)
	}
	currentInfo, err := os.Lstat(path)
	if err != nil || currentInfo.Mode()&fs.ModeSymlink != 0 ||
		!currentInfo.Mode().IsRegular() || !os.SameFile(lockedInfo, currentInfo) {
		return nil, errors.Join(
			errors.New("artifact repository ownership lock changed while opening"),
			lock.Close(),
		)
	}
	return &repositoryOwnershipLock{path: path, lock: lock}, nil
}

func (l *repositoryOwnershipLock) Close() error {
	if l == nil || l.lock == nil {
		return nil
	}
	if err := l.lock.Close(); err != nil {
		return fmt.Errorf("releasing artifact repository ownership lock %s: %w", l.path, err)
	}
	return nil
}

func rejectOwnedRepositoryAncestor(rootPath string) error {
	for ancestor := filepath.Dir(rootPath); ; ancestor = filepath.Dir(ancestor) {
		owned, err := pathHasRepositoryOwnershipMarker(ancestor)
		if err != nil {
			return fmt.Errorf("checking artifact repository ancestors: %w", err)
		}
		if owned {
			return repositoryOverlapLockError(ancestor)
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return nil
		}
	}
}

func pathHasRepositoryOwnershipMarker(path string) (bool, error) {
	marker := filepath.Join(path, repositoryOwnershipMarker)
	info, err := os.Lstat(marker)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.Mode().IsRegular() ||
		info.Size() != int64(len(repositoryOwnershipMarkerContent)) {
		return false, nil
	}
	content, err := os.ReadFile(marker)
	if err != nil {
		return false, err
	}
	return bytes.Equal(content, []byte(repositoryOwnershipMarkerContent)), nil
}

func ensureRepositoryOwnership(root *os.Root, rootPath string) error {
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return fmt.Errorf("checking artifact repository layout: %w", err)
	}
	if len(entries) == 0 {
		if _, err := validateRepositoryRoot(root, rootPath); err != nil {
			return err
		}
		if err := publishRepositoryOwnershipMarker(root); err != nil {
			return fmt.Errorf("publishing artifact repository ownership: %w", err)
		}
		return nil
	}

	hasMarker := false
	hasPending := false
	for _, entry := range entries {
		switch entry.Name() {
		case repositoryOwnershipMarker:
			hasMarker = true
		case repositoryOwnershipMarkerPending:
			hasPending = true
		}
	}
	if !hasMarker {
		if hasPending && len(entries) == 1 {
			if err := validateRepositoryMarker(root, repositoryOwnershipMarkerPending); err != nil {
				return invalidRepositoryMarkerError(rootPath, err)
			}
			if _, err := validateRepositoryRoot(root, rootPath); err != nil {
				return err
			}
			if err := completeRepositoryOwnershipMarker(root); err != nil {
				return fmt.Errorf("recovering artifact repository ownership: %w", err)
			}
			return nil
		}
		for _, entry := range entries {
			if knownDocbankRootEntry(entry.Name()) {
				return fmt.Errorf(
					"artifact repository is not owned by AgentsView; move or remove %s and retry",
					rootPath,
				)
			}
		}
		return fmt.Errorf(
			"artifact repository contains the old loose artifact layout; move or remove %s and retry",
			rootPath,
		)
	}
	if err := validateRepositoryMarker(root, repositoryOwnershipMarker); err != nil {
		return invalidRepositoryMarkerError(rootPath, err)
	}
	for _, entry := range entries {
		if entry.Name() == repositoryOwnershipMarker {
			continue
		}
		if entry.Name() == repositoryOwnershipMarkerPending {
			if err := validateRepositoryMarker(root, repositoryOwnershipMarkerPending); err != nil {
				return invalidRepositoryMarkerError(rootPath, err)
			}
			continue
		}
		entryInfo, infoErr := entry.Info()
		if infoErr != nil {
			return fmt.Errorf("checking artifact repository layout: %w", infoErr)
		}
		if !validDocbankRootEntry(entry.Name(), entryInfo) {
			if knownDocbankRootEntry(entry.Name()) {
				return fmt.Errorf(
					"artifact repository is not a valid Docbank vault; move or remove %s and retry: %s has an invalid file type",
					rootPath, entry.Name(),
				)
			}
			return fmt.Errorf(
				"artifact repository contains the old loose artifact layout; move or remove %s and retry",
				rootPath,
			)
		}
	}
	if hasPending {
		if _, err := validateRepositoryRoot(root, rootPath); err != nil {
			return err
		}
		if err := root.Remove(repositoryOwnershipMarkerPending); err != nil {
			return fmt.Errorf("removing completed artifact repository ownership state: %w", err)
		}
		if err := syncRepositoryRoot(root); err != nil {
			return fmt.Errorf("syncing artifact repository ownership: %w", err)
		}
	}
	return nil
}

func invalidRepositoryMarkerError(rootPath string, err error) error {
	return fmt.Errorf(
		"artifact repository has an invalid AgentsView ownership marker; move or remove %s and retry: %w",
		rootPath, err,
	)
}

func validateRepositoryMarker(root *os.Root, name string) error {
	info, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.Mode().IsRegular() ||
		info.Size() != int64(len(repositoryOwnershipMarkerContent)) {
		return errors.New("marker must be one regular file with the exact versioned contents")
	}
	content, err := root.ReadFile(name)
	if err != nil {
		return err
	}
	if !bytes.Equal(content, []byte(repositoryOwnershipMarkerContent)) {
		return errors.New("marker contents do not match the supported version")
	}
	return nil
}

func publishRepositoryOwnershipMarker(root *os.Root) (retErr error) {
	pending, err := root.OpenFile(
		repositoryOwnershipMarkerPending,
		os.O_WRONLY|os.O_CREATE|os.O_EXCL,
		0o600,
	)
	if err != nil {
		return err
	}
	pendingOpen := true
	defer func() {
		if pendingOpen {
			retErr = errors.Join(retErr, pending.Close())
		}
		if retErr != nil {
			removeErr := root.Remove(repositoryOwnershipMarkerPending)
			if !errors.Is(removeErr, fs.ErrNotExist) {
				retErr = errors.Join(retErr, removeErr)
			}
		}
	}()
	written, err := io.WriteString(pending, repositoryOwnershipMarkerContent)
	if err != nil {
		return err
	}
	if written != len(repositoryOwnershipMarkerContent) {
		return io.ErrShortWrite
	}
	if err := pending.Sync(); err != nil {
		return err
	}
	if err := pending.Close(); err != nil {
		return err
	}
	pendingOpen = false
	return completeRepositoryOwnershipMarker(root)
}

func completeRepositoryOwnershipMarker(root *os.Root) error {
	if err := root.Link(repositoryOwnershipMarkerPending, repositoryOwnershipMarker); err != nil {
		return err
	}
	if err := root.Remove(repositoryOwnershipMarkerPending); err != nil {
		return err
	}
	return syncRepositoryRoot(root)
}

func syncRepositoryRoot(root *os.Root) (retErr error) {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, directory.Close()) }()
	return directory.Sync()
}

func validDocbankRootEntry(name string, info fs.FileInfo) bool {
	if info.Mode()&fs.ModeSymlink != 0 {
		return false
	}
	switch name {
	case "blobs", "logs":
		return info.IsDir()
	case "docbank.db", "docbank.db-wal", "docbank.db-shm", "config.toml", "vault.lock":
		return info.Mode().IsRegular()
	default:
		return strings.HasPrefix(name, "daemon.") && strings.HasSuffix(name, ".json") &&
			info.Mode().IsRegular()
	}
}

func knownDocbankRootEntry(name string) bool {
	switch name {
	case "blobs", "logs", "docbank.db", "docbank.db-wal", "docbank.db-shm",
		"config.toml", "vault.lock":
		return true
	default:
		return strings.HasPrefix(name, "daemon.") && strings.HasSuffix(name, ".json")
	}
}

func targetIdentityChainContains(target string, identity fs.FileInfo) bool {
	current := target
	for {
		info, err := os.Stat(current)
		if err == nil {
			if os.SameFile(identity, info) {
				return true
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return false
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false
		}
		current = parent
	}
}
