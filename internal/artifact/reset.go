package artifact

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/docbank"
	docsqlite "go.kenn.io/docbank/pkg/sqlite"
)

const (
	// ArtifactResetManualCleanupWarning is returned verbatim by every reset
	// surface so operators cannot mistake diagnostic retention for cleanup.
	ArtifactResetManualCleanupWarning = "Cleanup is manual: after diagnosis, remove the moved-aside vault yourself. Repeated resets may accumulate diagnostic vaults and consume disk; AgentsView never deletes them automatically."
	// ArtifactResetForeignRelayWarning explains the one class of artifacts that
	// cannot be reconstructed from the authoritative local SQLite archive.
	ArtifactResetForeignRelayWarning = "Foreign relay artifacts are unavailable until peers resend them; sessions already stored in SQLite are unchanged."
)

// RepositoryResetResult names both preserved vault paths and the local
// publication recreated from SQLite.
type RepositoryResetResult struct {
	VaultRoot      string       `json:"vault_root"`
	DiagnosticRoot string       `json:"diagnostic_root"`
	Export         ExportResult `json:"export"`
}

type repositoryResetHooks struct {
	now           func() time.Time
	resetVault    func(context.Context, docbank.Config, docbank.ResetOptions) (*docbank.Vault, error)
	openRoot      func(string) (*os.Root, error)
	driver        docsqlite.Driver
	export        func(context.Context, *db.DB, ArtifactStore, ExportOptions) (ExportResult, error)
	beforeRelease func() error
}

// ResetRepository explicitly moves the owned Docbank vault aside, creates a
// fresh vault, and republishes this machine's artifacts from SQLite. current is
// the daemon-owned repository when reset runs in-process; direct callers pass
// nil and must already hold the SQLite write-owner lock.
func ResetRepository(
	ctx context.Context,
	dataDir string,
	database *db.DB,
	origin string,
	current *Repository,
) (*Repository, RepositoryResetResult, error) {
	return resetRepositoryWith(ctx, dataDir, database, origin, current, repositoryResetHooks{})
}

// BeginRepositoryReset validates and moves the current vault aside, then opens
// its fresh replacement. beforeRelease runs at Docbank's last boundary before
// it releases and moves the current vault; returning an error leaves the
// current vault in place.
func BeginRepositoryReset(
	ctx context.Context,
	dataDir string,
	origin string,
	current *Repository,
	beforeRelease func() error,
) (*Repository, RepositoryResetResult, error) {
	return beginRepositoryResetWith(ctx, dataDir, origin, current, repositoryResetHooks{
		beforeRelease: beforeRelease,
	})
}

// RepublishRepositoryReset reconstructs the local publication in a fresh vault
// after BeginRepositoryReset. It leaves the fresh repository open on failure
// so an in-process owner can retain it and retry publication later.
func RepublishRepositoryReset(
	ctx context.Context,
	dataDir string,
	database *db.DB,
	origin string,
	fresh *Repository,
	result RepositoryResetResult,
) (RepositoryResetResult, error) {
	return republishRepositoryResetWith(
		ctx, dataDir, database, origin, fresh, result, repositoryResetHooks{},
	)
}

func resetRepositoryWith(
	ctx context.Context,
	dataDir string,
	database *db.DB,
	origin string,
	current *Repository,
	hooks repositoryResetHooks,
) (returned *Repository, result RepositoryResetResult, retErr error) {
	if ctx == nil {
		return nil, result, fmt.Errorf("%w: repository reset context is required", ErrArtifactInvalid)
	}
	if database == nil || database.ReadOnly() {
		return nil, result, fmt.Errorf("%w: writable repository reset database is required", ErrArtifactInvalid)
	}
	originalBeforeRelease := hooks.beforeRelease
	var pending db.ArtifactResetRepublishPending
	markerPrepared := false
	hooks.beforeRelease = func() error {
		if strings.TrimSpace(origin) != "" {
			var err error
			pending, err = PrepareRepositoryResetRepublish(ctx, database, dataDir, origin)
			if err != nil {
				return err
			}
			markerPrepared = true
		}
		if originalBeforeRelease != nil {
			return originalBeforeRelease()
		}
		return nil
	}
	returned, result, retErr = beginRepositoryResetWith(
		ctx, dataDir, origin, current, hooks,
	)
	if retErr != nil {
		if markerPrepared {
			if _, statErr := os.Lstat(result.DiagnosticRoot); errors.Is(statErr, fs.ErrNotExist) {
				_, clearErr := database.ClearArtifactResetRepublishPending(
					context.WithoutCancel(ctx), pending,
				)
				retErr = errors.Join(retErr, clearErr)
			}
		}
		return nil, result, retErr
	}
	result, retErr = republishRepositoryResetWith(
		ctx, dataDir, database, origin, returned, result, hooks,
	)
	if retErr != nil {
		retErr = errors.Join(retErr, returned.Close())
		return nil, result, retErr
	}
	return returned, result, nil
}

func beginRepositoryResetWith(
	ctx context.Context,
	dataDir string,
	origin string,
	current *Repository,
	hooks repositoryResetHooks,
) (returned *Repository, result RepositoryResetResult, retErr error) {
	if ctx == nil {
		return nil, result, fmt.Errorf("%w: repository reset context is required", ErrArtifactInvalid)
	}
	origin = strings.TrimSpace(origin)
	if origin != "" {
		if err := validateOriginID(origin); err != nil {
			return nil, result, err
		}
	}
	rootPath, err := canonicalRepositoryRoot(dataDir)
	if err != nil {
		return nil, result, err
	}
	if hooks.now == nil {
		hooks.now = time.Now
	}
	if hooks.resetVault == nil {
		hooks.resetVault = docbank.ResetVault
	}
	if hooks.openRoot == nil {
		hooks.openRoot = os.OpenRoot
	}
	var reservation *repositoryRootReservation
	currentReleased := false
	reservationTransferred := false
	if current != nil {
		current.mu.Lock()
		defer current.mu.Unlock()
		if current.closed || current.rootReservation == nil || current.root == nil {
			return nil, result, errors.New("artifact repository is not available for reset")
		}
		if current.rootPath != rootPath {
			return nil, result, errors.New("artifact repository reset target does not match the current owner")
		}
		reservation = current.rootReservation
	} else {
		reservation, err = reserveRepositoryRoot(rootPath)
		if err != nil {
			return nil, result, err
		}
	}
	defer func() {
		if reservationTransferred || (current != nil && !currentReleased) {
			return
		}
		reservation.Close()
	}()

	ownershipLock, err := acquireRepositoryOwnershipLock(ctx, rootPath)
	if err != nil {
		return nil, result, err
	}
	defer func() {
		if err := ownershipLock.Close(); err != nil {
			if returned != nil {
				err = errors.Join(err, returned.Close())
				returned = nil
				reservationTransferred = false
			}
			retErr = errors.Join(retErr, err)
		}
	}()

	if current != nil {
		if err := validateRepositoryForReset(current.root, rootPath); err != nil {
			return nil, result, err
		}
	} else {
		root, err := hooks.openRoot(rootPath)
		if err != nil {
			return nil, result, fmt.Errorf("opening artifact repository for reset: %w", err)
		}
		validationErr := validateRepositoryForReset(root, rootPath)
		closeErr := root.Close()
		if err := errors.Join(validationErr, closeErr); err != nil {
			return nil, result, err
		}
	}

	diagnosticRoot, err := nextRepositoryDiagnosticRoot(ctx, rootPath, hooks.now())
	if err != nil {
		return nil, result, err
	}
	result = RepositoryResetResult{VaultRoot: rootPath, DiagnosticRoot: diagnosticRoot}
	driver := hooks.driver
	if driver == nil && current != nil {
		driver = current.driver
	}
	config := docbank.Config{
		Root:   rootPath,
		SQLite: driver,
		LooseCompression: docbank.LooseCompressionOptions{
			Enabled:           true,
			MinBytes:          repositoryLooseCompressionBytes,
			MinSavingsPercent: repositoryLooseCompressionSaving,
		},
	}
	releaseCurrent := hooks.beforeRelease
	if current != nil {
		releaseCurrent = func() error {
			if hooks.beforeRelease != nil {
				if err := hooks.beforeRelease(); err != nil {
					return err
				}
			}
			currentReleased = true
			current.closed = true
			current.rootReservation = nil
			return errors.Join(current.backend.Close(), current.root.Close())
		}
	}
	vault, err := hooks.resetVault(ctx, config, docbank.ResetOptions{
		DiagnosticRoot: diagnosticRoot,
		ReleaseCurrent: releaseCurrent,
	})
	if err != nil {
		return nil, result, repositoryResetErrorIfMoved(result, err)
	}

	root, err := hooks.openRoot(rootPath)
	if err != nil {
		return nil, result, repositoryResetAfterMoveError(
			result, errors.Join(err, vault.Close()),
		)
	}
	rootIdentity, err := validateRepositoryRoot(root, rootPath)
	if err == nil {
		err = publishRepositoryOwnershipMarker(root)
	}
	if err == nil {
		_, err = validateRepositoryRoot(root, rootPath)
	}
	if err != nil {
		return nil, result, repositoryResetAfterMoveError(
			result, errors.Join(err, root.Close(), vault.Close()),
		)
	}

	backend := newDocbankStore(vault)
	fresh := &Repository{
		backend:         backend,
		root:            root,
		rootIdentity:    rootIdentity,
		rootPath:        rootPath,
		rootReservation: reservation,
		driver:          driver,
	}
	fresh.store = &repositoryStore{ArtifactStore: backend, owner: fresh}
	reservationTransferred = true
	returned = fresh
	return returned, result, nil
}

func republishRepositoryResetWith(
	ctx context.Context,
	dataDir string,
	database *db.DB,
	origin string,
	fresh *Repository,
	result RepositoryResetResult,
	hooks repositoryResetHooks,
) (RepositoryResetResult, error) {
	if ctx == nil {
		return result, repositoryResetAfterMoveError(
			result, fmt.Errorf("%w: repository reset context is required", ErrArtifactInvalid),
		)
	}
	// Cancellation must be observed before touching SQLite. Shutdown can cancel
	// this phase after the filesystem commit and then tear the database down.
	if err := ctx.Err(); err != nil {
		return result, repositoryResetAfterMoveError(result, err)
	}
	if database == nil || database.ReadOnly() {
		return result, repositoryResetAfterMoveError(
			result, fmt.Errorf("%w: writable repository reset database is required", ErrArtifactInvalid),
		)
	}
	origin = strings.TrimSpace(origin)
	if origin == "" {
		return result, nil
	}
	if err := validateOriginID(origin); err != nil {
		return result, repositoryResetAfterMoveError(result, err)
	}
	if fresh == nil || fresh.Closed() {
		return result, repositoryResetAfterMoveError(
			result, errors.New("fresh artifact repository is not available for republish"),
		)
	}
	if hooks.export == nil {
		hooks.export = func(
			ctx context.Context, database *db.DB, store ArtifactStore, opts ExportOptions,
		) (ExportResult, error) {
			return ExportToStore(ctx, database, store, opts)
		}
	}
	var (
		recovered bool
		err       error
	)
	result.Export, recovered, err = recoverRepositoryResetRepublishWith(
		ctx, database, fresh.Store(), origin, hooks.export,
	)
	if err != nil {
		return result, repositoryResetAfterMoveError(result, err)
	}
	if recovered {
		return result, nil
	}
	recorder := NewMetadataRecorder(database, MetadataRecorderOptions{
		Origin: origin,
		Store:  fresh.Store(),
	})
	if _, err := recorder.materializeCurrentState(ctx); err != nil {
		return result, repositoryResetAfterMoveError(result, err)
	}
	result.Export, err = hooks.export(
		ctx, database, fresh.Store(), ExportOptions{Origin: origin, Full: true},
	)
	if err != nil {
		return result, repositoryResetAfterMoveError(result, err)
	}
	return result, nil
}

type repositoryResetExportFunc func(
	context.Context, *db.DB, ArtifactStore, ExportOptions,
) (ExportResult, error)

// RecoverRepositoryResetRepublish completes a durable interrupted repository
// reset before ordinary publication or exchange begins. Only the store owned by
// the matching repository root may consume and clear the marker.
func RecoverRepositoryResetRepublish(
	ctx context.Context,
	database *db.DB,
	store ArtifactStore,
	origin string,
) (ExportResult, bool, error) {
	return recoverRepositoryResetRepublishWith(
		ctx, database, store, origin,
		func(
			ctx context.Context, database *db.DB, store ArtifactStore, opts ExportOptions,
		) (ExportResult, error) {
			return ExportToStore(ctx, database, store, opts)
		},
	)
}

// PublishRepositoryArtifacts completes any durable interrupted reset before
// performing an ordinary export. A completed full recovery already satisfies
// the requested publication and is returned directly.
func PublishRepositoryArtifacts(
	ctx context.Context,
	database *db.DB,
	store ArtifactStore,
	opts ExportOptions,
) (ExportResult, error) {
	result, recovered, err := RecoverRepositoryResetRepublish(
		ctx, database, store, opts.Origin,
	)
	if err != nil || recovered {
		return result, err
	}
	return ExportToStore(ctx, database, store, opts)
}

func recoverRepositoryResetRepublishWith(
	ctx context.Context,
	database *db.DB,
	store ArtifactStore,
	origin string,
	export repositoryResetExportFunc,
) (ExportResult, bool, error) {
	if ctx == nil {
		return ExportResult{}, false, errors.New("artifact reset recovery context is required")
	}
	if err := ctx.Err(); err != nil {
		return ExportResult{}, false, err
	}
	if database == nil || database.ReadOnly() {
		return ExportResult{}, false, errors.New("writable artifact reset database is required")
	}
	origin = strings.TrimSpace(origin)
	if err := validateOriginID(origin); err != nil {
		return ExportResult{}, false, err
	}
	pending, found, err := database.ArtifactResetRepublishPending(ctx)
	if err != nil || !found {
		return ExportResult{}, false, err
	}
	repository, owned := RepositoryFromStore(store)
	if !owned {
		return ExportResult{}, false, nil
	}
	fingerprint, err := repositoryFingerprint(repository)
	if err != nil {
		return ExportResult{}, false, err
	}
	if pending.RootFingerprint != fingerprint || pending.Origin != origin {
		return ExportResult{}, false,
			errors.New("artifact reset republish state does not match the repository")
	}
	if export == nil {
		return ExportResult{}, false, errors.New("artifact reset recovery exporter is required")
	}
	recorder := NewMetadataRecorder(database, MetadataRecorderOptions{
		Origin: origin,
		Store:  store,
	})
	if _, err := recorder.materializeCurrentStateAtHLC(ctx, pending.BaselineHLC); err != nil {
		return ExportResult{}, false, err
	}
	result, err := export(ctx, database, store, ExportOptions{Origin: origin, Full: true})
	if err != nil {
		return ExportResult{}, false, err
	}
	cleared, err := database.ClearArtifactResetRepublishPending(ctx, pending)
	if err != nil {
		return ExportResult{}, false, err
	}
	if !cleared {
		return ExportResult{}, false,
			errors.New("artifact reset republish state changed before completion")
	}
	return result, true, nil
}

func repositoryFingerprint(repository *Repository) (string, error) {
	if repository == nil {
		return "", errors.New("artifact repository is required")
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	if repository.closed {
		return "", errors.New("artifact repository is closed")
	}
	digest := sha256.Sum256([]byte(repository.rootPath))
	return hex.EncodeToString(digest[:]), nil
}

// PrepareRepositoryResetRepublish advances the local metadata clock and
// persists the durable reset intent before Docbank releases the current vault.
// Callers must invoke their short lifecycle commit only after this returns.
func PrepareRepositoryResetRepublish(
	ctx context.Context,
	database *db.DB,
	dataDir string,
	origin string,
) (db.ArtifactResetRepublishPending, error) {
	if ctx == nil {
		return db.ArtifactResetRepublishPending{}, errors.New("artifact reset republish context is required")
	}
	if err := ctx.Err(); err != nil {
		return db.ArtifactResetRepublishPending{}, err
	}
	if database == nil || database.ReadOnly() {
		return db.ArtifactResetRepublishPending{}, errors.New("writable artifact reset database is required")
	}
	origin = strings.TrimSpace(origin)
	if err := validateOriginID(origin); err != nil {
		return db.ArtifactResetRepublishPending{}, err
	}
	fingerprint, err := repositoryRootFingerprint(dataDir)
	if err != nil {
		return db.ArtifactResetRepublishPending{}, err
	}
	stamp, err := NewHLCClock(database, HLCClockOptions{}).Next()
	if err != nil {
		return db.ArtifactResetRepublishPending{}, err
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return db.ArtifactResetRepublishPending{}, fmt.Errorf("generating artifact reset token: %w", err)
	}
	pending := db.ArtifactResetRepublishPending{
		Version:         1,
		RootFingerprint: fingerprint,
		Origin:          origin,
		Token:           hex.EncodeToString(tokenBytes),
		BaselineHLC:     stamp.String(),
	}
	if err := database.SetArtifactResetRepublishPending(ctx, pending); err != nil {
		return db.ArtifactResetRepublishPending{}, err
	}
	return pending, nil
}

func repositoryRootFingerprint(dataDir string) (string, error) {
	root, err := canonicalRepositoryRoot(dataDir)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(root))
	return hex.EncodeToString(digest[:]), nil
}

func validateRepositoryForReset(root *os.Root, rootPath string) error {
	if _, err := validateRepositoryRoot(root, rootPath); err != nil {
		return err
	}
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return fmt.Errorf("checking artifact repository reset layout: %w", err)
	}
	hasMarker := false
	for _, entry := range entries {
		if entry.Name() == repositoryOwnershipMarker {
			hasMarker = true
			if err := validateRepositoryMarker(root, repositoryOwnershipMarker); err != nil {
				return invalidRepositoryMarkerError(rootPath, err)
			}
			continue
		}
		if entry.Name() == repositoryOwnershipMarkerPending {
			if err := validateRepositoryMarker(root, repositoryOwnershipMarkerPending); err != nil {
				return invalidRepositoryMarkerError(rootPath, err)
			}
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("checking artifact repository reset layout: %w", err)
		}
		if !validDocbankRootEntry(entry.Name(), info) {
			return fmt.Errorf("artifact repository is not a valid owned Docbank vault: %s", rootPath)
		}
	}
	if !hasMarker {
		return fmt.Errorf("artifact repository is not owned by AgentsView: %s", rootPath)
	}
	return nil
}

func nextRepositoryDiagnosticRoot(
	ctx context.Context, rootPath string, now time.Time,
) (string, error) {
	base := rootPath + ".reset-" + now.UTC().Format("20060102T150405.000000000Z")
	for sequence := 0; ; sequence++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		candidate := base
		if sequence > 0 {
			candidate += "." + strconv.Itoa(sequence)
		}
		_, err := os.Lstat(candidate)
		if errors.Is(err, fs.ErrNotExist) {
			return candidate, nil
		}
		if err != nil {
			return "", fmt.Errorf("checking artifact reset diagnostic path: %w", err)
		}
	}
}

func repositoryResetErrorIfMoved(result RepositoryResetResult, err error) error {
	if _, statErr := os.Lstat(result.DiagnosticRoot); statErr == nil {
		return repositoryResetAfterMoveError(result, err)
	}
	return err
}

func repositoryResetAfterMoveError(result RepositoryResetResult, err error) error {
	return fmt.Errorf(
		"artifact reset failed after moving the original vault to %s; the fresh vault path is %s; preserve both paths for manual recovery: %w",
		result.DiagnosticRoot, result.VaultRoot, err,
	)
}
