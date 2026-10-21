package archivesource

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"strings"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawsync"
	syncer "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/docbank/docbank"
)

const sourceScratchBytes = 16 << 30

type snapshotObjects struct{ reader *docbank.BackupReader }

func (s snapshotObjects) CopyObject(ctx context.Context, _ string, ref rawsync.ObjectRef, dst io.Writer) (rawsync.ObjectInfo, error) {
	if err := s.checkObject(ctx, "", ref); err != nil {
		return rawsync.ObjectInfo{}, err
	}
	if _, err := s.reader.CopyObject(ctx, ref.SHA256, dst); err != nil {
		return rawsync.ObjectInfo{}, err
	}
	return rawsync.ObjectInfo{Ref: ref}, nil
}

func (s snapshotObjects) checkObject(_ context.Context, _ string, ref rawsync.ObjectRef) error {
	object, err := s.reader.Object(ref.SHA256)
	if err != nil {
		return err
	}
	if object.Bytes != ref.Length {
		return errors.New("archive source object differs from its recorded size")
	}
	return nil
}

func (i *Index) provenance(source Source) db.ArchiveSourceProvenance {
	return db.ArchiveSourceProvenance{
		Attachment: db.ArchiveAttachmentKey{RepositoryID: i.Info.RepositoryID, SnapshotID: i.Info.SnapshotID},
		Source:     db.ArchiveSourceIdentity{ArchiveID: i.ArchiveID, DeviceID: source.Root.DeviceID, RootID: source.Root.ID, SourceKey: source.SourceKey}, ManifestID: source.ManifestID,
	}
}

func (i *Index) syncSource(ctx context.Context, repository string, source Source, cfg config.Config,
	engine *syncer.Engine, scratch, target string, progress syncer.ProgressFunc,
) (_ syncer.SyncStats, retErr error) {
	var cleanup func() error
	defer func() {
		if cleanup != nil {
			retErr = errors.Join(retErr, cleanup())
		}
	}()
	p := i.provenance(source)
	return engine.SyncAttachedSource(ctx, p, target, func() (syncer.EngineConfig, string, func() error, error) {
		identity, err := rawsync.NewAuthIdentity(i.ArchiveID, source.Root.DeviceID)
		if err != nil {
			return syncer.EngineConfig{}, "", nil, err
		}
		manifest, err := rawsync.ParseCanonicalManifest(identity, source.ManifestID, source.CanonicalJSON, rawsync.DefaultManifestLimits())
		if err != nil {
			return syncer.EngineConfig{}, "", nil, err
		}
		if err := rawderive.CleanAbandonedMaterializations(scratch); err != nil {
			return syncer.EngineConfig{}, "", nil, err
		}
		materialized, err := i.materialize(ctx, repository, manifest, scratch)
		if err != nil {
			return syncer.EngineConfig{}, "", nil, err
		}
		cleanup = materialized.Cleanup
		storedPath := "archive://" + source.Root.ID + "/" + url.PathEscape(source.SourceKey)
		prepared, err := rawderive.PrepareLocalSource(ctx, manifest, materialized, source.Root.DeviceID, storedPath)
		if err != nil {
			return syncer.EngineConfig{}, "", nil, err
		}
		suppressions, err := i.Suppressions(ctx, source.Root.DeviceID)
		if err != nil {
			return syncer.EngineConfig{}, "", nil, err
		}
		policy := make(map[string]db.RawArchiveSuppression, len(suppressions))
		for _, suppression := range suppressions {
			policy[suppression.ParserID] = suppression
		}
		prepared.Config.IDPrefix = source.Root.DeviceID + "~"
		prepared.Config.ArchiveContent = cfg.ArchiveContent
		prepared.Config.ToolResultImages = cfg.ToolResultImages
		prepared.Config.BlockedResultCategories = cfg.ResultContentBlockedCategories
		prepared.Config.AssetsDir = filepath.Join(cfg.DataDir, "assets")
		prepared.Config.ArchiveSessionPolicy = func(_ context.Context, s *db.Session, native string) (bool, error) {
			if strings.Contains(native, "~") {
				return false, errors.New("parser returned a transport-qualified session identity")
			}
			if d, ok := rawderive.SuppressionFor(policy, native); ok && (d.Provider == "" || d.Provider == source.Root.Provider) {
				return false, nil
			}
			if s.Machine != source.Root.DeviceID || s.ID != source.Root.DeviceID+"~"+native {
				return false, fmt.Errorf("%w: parser source identity differs", db.ErrArchiveSourceConflict)
			}
			// Continuations and forks share the accepted source, never its scratch paths.
			s.FilePath = new(storedPath)
			return true, nil
		}
		prepared.Config.ArchiveWriteProvenance = func(_ *db.Session, native string) (db.ArchiveSessionProvenance, error) {
			return db.ArchiveSessionProvenance{ArchiveSourceProvenance: p, NativeSessionID: native}, nil
		}
		return prepared.Config, prepared.Path, cleanup, nil
	}, progress)
}

func (i *Index) materialize(ctx context.Context, repository string, manifest rawsync.CanonicalManifest, scratch string) (*rawderive.Materialization, error) {
	if i.objects != nil {
		return (rawderive.Materializer{Store: i.objects, BaseDir: scratch, MaxTotalBytes: sourceScratchBytes}).Materialize(ctx, manifest)
	}
	reader, err := i.OpenReader(ctx, repository)
	if err != nil {
		return nil, err
	}
	materialized, err := (rawderive.Materializer{Store: snapshotObjects{reader}, BaseDir: scratch, MaxTotalBytes: sourceScratchBytes}).Materialize(ctx, manifest)
	err = errors.Join(err, reader.Close())
	if err != nil && materialized != nil {
		err = errors.Join(err, materialized.Cleanup())
		materialized = nil
	}
	return materialized, err
}
