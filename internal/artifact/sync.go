package artifact

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

const maxArtifactSyncDrainRounds = 32

// SyncOptions selects one explicit artifact-folder exchange.
type SyncOptions struct {
	DataDir        string
	Target         string
	Origin         string
	ForbiddenRoots []string
	Full           bool
}

// SyncResult summarizes one bounded export, exchange, and import pass.
type SyncResult struct {
	Origin             string `json:"origin"`
	ExportedSessions   int    `json:"exported_sessions"`
	RejectedSessions   int    `json:"rejected_sessions"`
	ImportedSessions   int    `json:"imported_sessions"`
	ImportedMessages   int    `json:"imported_messages"`
	Quarantined        int    `json:"quarantined"`
	ReceivedArtifacts  int    `json:"received_artifacts"`
	PublishedArtifacts int    `json:"published_artifacts"`
	More               bool   `json:"more"`
}

// Sync opens the local artifact repository on demand and performs one bounded
// folder exchange. The repository is not created unless Sync is called.
func Sync(
	ctx context.Context,
	database *db.DB,
	opts SyncOptions,
) (_ SyncResult, retErr error) {
	if ctx == nil {
		return SyncResult{}, fmt.Errorf(
			"%w: artifact sync context is required",
			ErrArtifactInvalid,
		)
	}
	if database == nil {
		return SyncResult{}, errors.New("artifact sync database is required")
	}
	if err := validateArtifactSyncOptions(opts); err != nil {
		return SyncResult{}, err
	}
	if opts.DataDir == "" {
		return SyncResult{}, errors.New("artifact sync data directory is required")
	}
	repository, err := OpenRepository(ctx, opts.DataDir)
	if err != nil {
		return SyncResult{}, err
	}
	defer func() { retErr = errors.Join(retErr, repository.Close()) }()
	return SyncWithRepository(ctx, database, repository, opts)
}

// SyncWithRepository performs one folder exchange through a caller-owned
// repository. The repository remains open when the call returns.
func SyncWithRepository(
	ctx context.Context,
	database *db.DB,
	repository *Repository,
	opts SyncOptions,
) (_ SyncResult, retErr error) {
	if ctx == nil {
		return SyncResult{}, fmt.Errorf(
			"%w: artifact sync context is required",
			ErrArtifactInvalid,
		)
	}
	if database == nil {
		return SyncResult{}, errors.New("artifact sync database is required")
	}
	if err := validateArtifactSyncOptions(opts); err != nil {
		return SyncResult{}, err
	}
	if repository == nil || repository.Closed() {
		return SyncResult{}, errors.New("artifact sync repository is required")
	}
	if err := ctx.Err(); err != nil {
		return SyncResult{}, err
	}

	forbidden := make([]string, 0, len(opts.ForbiddenRoots)+1)
	forbidden = append(forbidden, opts.ForbiddenRoots...)
	forbidden = append(forbidden, repository.rootPath)
	transport, err := OpenFolderTransport(
		opts.Target,
		FolderTransportOptions{
			ForbiddenRoots: forbidden,
			StateStore:     databaseFolderTransportState{database: database},
		},
	)
	if err != nil {
		return SyncResult{}, err
	}
	defer func() { retErr = errors.Join(retErr, transport.Close()) }()

	origin, err := resolveSyncOrigin(database, opts.Origin)
	if err != nil {
		return SyncResult{}, err
	}
	result := SyncResult{Origin: origin}
	exported, exportMore, err := drainArtifactSyncExports(
		ctx,
		database,
		repository.Content(),
		origin,
		opts.Full,
	)
	if err != nil {
		return result, err
	}
	result.ExportedSessions = exported.ExportedSessions
	result.RejectedSessions = exported.RejectedSessions

	coordinated := &coordinatedTransportStore{
		ArtifactStore: repository.Content(),
		quarantine:    transport,
	}
	coordinator := NewStoreImportCoordinator(
		database,
		coordinated,
		origin,
	)
	coordinated.record = coordinator.RecordChanged
	publications, err := newAuthoritativePublicationStore(
		ctx,
		database,
		coordinated,
		origin,
	)
	if err != nil {
		return result, err
	}
	exchanged, err := transport.Exchange(ctx, publications, origin)
	if err != nil {
		return result, err
	}
	result.ReceivedArtifacts = exchanged.Received
	result.PublishedArtifacts = exchanged.Published

	importMore, err := drainArtifactSyncImports(
		ctx,
		coordinator,
		&result,
	)
	if err != nil {
		return result, err
	}
	result.More = exportMore || exchanged.More || importMore
	return result, nil
}

type databaseFolderTransportState struct {
	database *db.DB
}

func (s databaseFolderTransportState) LoadFolderTransportState(
	_ context.Context,
	namespaceID string,
) (string, error) {
	return s.database.GetSyncState("artifact_transport_" + namespaceID)
}

func (s databaseFolderTransportState) SaveFolderTransportState(
	_ context.Context,
	namespaceID string,
	value string,
) error {
	return s.database.SetSyncState("artifact_transport_"+namespaceID, value)
}

func validateArtifactSyncOptions(opts SyncOptions) error {
	if strings.TrimSpace(opts.Target) == "" {
		return fmt.Errorf(
			"%w: artifact folder target is required",
			ErrArtifactInvalid,
		)
	}
	if opts.Origin != "" {
		if err := validateOriginID(opts.Origin); err != nil {
			return err
		}
	}
	return nil
}

func resolveSyncOrigin(database *db.DB, configured string) (string, error) {
	if configured == "" {
		return EnsureOrigin(database)
	}
	if err := validateOriginID(configured); err != nil {
		return "", err
	}
	if err := AdoptOrigin(database, configured); err != nil {
		return "", err
	}
	return configured, nil
}

func drainArtifactSyncExports(
	ctx context.Context,
	database *db.DB,
	store ArtifactStore,
	origin string,
	full bool,
) (ExportResult, bool, error) {
	return drainArtifactSyncExportsWithRounds(
		ctx,
		database,
		store,
		origin,
		full,
		maxArtifactSyncDrainRounds,
	)
}

func drainArtifactSyncExportsWithRounds(
	ctx context.Context,
	database artifactExportStore,
	store ArtifactStore,
	origin string,
	full bool,
	rounds int,
) (ExportResult, bool, error) {
	var result ExportResult
	if full {
		exported, err := ExportToStore(
			ctx,
			database,
			store,
			ExportOptions{Origin: origin, Full: true},
		)
		mergeSyncExportResult(&result, exported)
		if errors.Is(err, ErrArtifactExportUnsettled) {
			return result, true, nil
		}
		return result, false, err
	}
	for range rounds {
		exported, err := ExportToStore(
			ctx,
			database,
			store,
			ExportOptions{Origin: origin},
		)
		if err != nil {
			return result, false, err
		}
		mergeSyncExportResult(&result, exported)
		pending, err := database.CountPendingArtifactExports(ctx)
		if err != nil {
			return result, false, err
		}
		if pending == 0 {
			return result, false, nil
		}
	}
	return result, true, nil
}

func mergeSyncExportResult(target *ExportResult, page ExportResult) {
	target.ExportedSessions += page.ExportedSessions
	target.RejectedSessions += page.RejectedSessions
	target.CheckpointCreated = target.CheckpointCreated ||
		page.CheckpointCreated
	if page.CheckpointSequence > target.CheckpointSequence {
		target.CheckpointSequence = page.CheckpointSequence
	}
}

func drainArtifactSyncImports(
	ctx context.Context,
	coordinator *StoreImportCoordinator,
	result *SyncResult,
) (bool, error) {
	return drainArtifactSyncImportsWithRounds(
		ctx,
		coordinator,
		result,
		maxArtifactSyncDrainRounds,
	)
}

type artifactImportFinalizer interface {
	Finalize(context.Context) (ImportResult, error)
}

func drainArtifactSyncImportsWithRounds(
	ctx context.Context,
	coordinator artifactImportFinalizer,
	result *SyncResult,
	rounds int,
) (bool, error) {
	for range rounds {
		imported, err := coordinator.Finalize(ctx)
		if err != nil {
			return false, err
		}
		result.ImportedSessions += imported.Sessions
		result.ImportedMessages += imported.Messages
		result.Quarantined += imported.Quarantined
		if !imported.More {
			return false, nil
		}
	}
	return true, nil
}

type transportRemoteQuarantiner interface {
	QuarantineTransportArtifact(context.Context, Ref, Identity) error
}

type coordinatedTransportStore struct {
	ArtifactStore
	record     func(context.Context, Entry) error
	quarantine Transport
}

func (s *coordinatedTransportStore) RecordTransportChanged(
	ctx context.Context,
	entry Entry,
) error {
	if s.record == nil {
		return errors.New("artifact transport change recorder is required")
	}
	return s.record(ctx, entry)
}

func (s *coordinatedTransportStore) Quarantine(
	ctx context.Context,
	ref Ref,
	reason string,
) error {
	entry, err := s.Stat(ctx, ref)
	if err != nil {
		return err
	}
	if remote, ok := s.quarantine.(transportRemoteQuarantiner); ok {
		if err := remote.QuarantineTransportArtifact(
			ctx,
			ref,
			entry.Identity,
		); err != nil && !errors.Is(err, ErrArtifactConflict) {
			return err
		}
	}
	return s.ArtifactStore.Quarantine(ctx, ref, reason)
}
