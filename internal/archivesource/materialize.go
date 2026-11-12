package archivesource

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawsync"
	"go.kenn.io/docbank/docbank"
)

// MaterializeSnapshot reconstructs an attached source under private scratch.
// The caller owns the returned materialization and must clean it up.
func MaterializeSnapshot(ctx context.Context, sources []config.ArchiveSource, owner db.ArchiveSessionProvenance, provider string, limit int64) (tree *rawderive.Materialization, result rawsync.CanonicalManifest, retErr error) {
	defer func() {
		if retErr != nil && tree != nil {
			retErr = errors.Join(retErr, tree.Cleanup())
			tree = nil
		}
	}()
	var selected *docbank.BackupReader
	defer func() {
		if selected != nil {
			retErr = errors.Join(retErr, selected.Close())
		}
	}()
	for _, source := range sources {
		if source.Directory != "" || source.Snapshot != owner.Attachment.SnapshotID {
			continue
		}
		repo, err := docbank.OpenBackupRepository(source.Repository)
		if err != nil {
			return nil, rawsync.CanonicalManifest{}, err
		}
		if repo.ID() != owner.Attachment.RepositoryID {
			continue
		}
		if selected != nil {
			return nil, rawsync.CanonicalManifest{}, errors.New("ambiguous configured snapshot")
		}
		selected, err = repo.OpenSnapshot(ctx, source.Snapshot)
		if err != nil {
			return nil, rawsync.CanonicalManifest{}, err
		}
	}
	if selected == nil {
		return nil, rawsync.CanonicalManifest{}, errors.New("owning snapshot is not configured")
	}
	scratch, err := os.MkdirTemp("", "agentsview-snapshot-")
	if err != nil {
		return nil, rawsync.CanonicalManifest{}, err
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(scratch)) }()
	object, err := selected.Extra("application/sessions.db")
	if err != nil {
		return nil, rawsync.CanonicalManifest{}, err
	}
	if object.Bytes < 0 || object.Bytes > sourceScratchBytes {
		return nil, rawsync.CanonicalManifest{}, errors.New("saved archive database exceeds size limit")
	}
	saved := filepath.Join(scratch, "sessions.db")
	file, err := os.OpenFile(saved, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, rawsync.CanonicalManifest{}, err
	}
	writer := &boundedSnapshotWriter{dst: file, remaining: object.Bytes}
	_, copyErr := selected.CopyExtra(ctx, "application/sessions.db", writer)
	if err := errors.Join(copyErr, file.Close()); err != nil {
		return nil, rawsync.CanonicalManifest{}, err
	}
	if writer.remaining != 0 {
		return nil, rawsync.CanonicalManifest{}, errors.New("saved archive database size differs")
	}
	uri := immutableURI(saved)
	if err := validateSavedDatabase(ctx, uri, snapshotObjects{selected}.checkObject); err != nil {
		return nil, rawsync.CanonicalManifest{}, err
	}
	conn, err := sql.Open("sqlite3", uri)
	if err != nil {
		return nil, rawsync.CanonicalManifest{}, err
	}
	defer func() { retErr = errors.Join(retErr, conn.Close()) }()
	archiveID, err := checkSavedSchema(ctx, conn)
	if err != nil {
		return nil, rawsync.CanonicalManifest{}, err
	}
	s := owner.Source
	if archiveID != s.ArchiveID {
		return nil, rawsync.CanonicalManifest{}, errors.New("snapshot archive identity differs")
	}
	var canonical []byte
	var rootProvider, configuredRoot string
	var count int
	err = conn.QueryRowContext(ctx, `SELECT r.provider,r.configured_root_id,s.canonical_json FROM raw_archive_sources s JOIN raw_archive_roots r ON r.id=s.root_id WHERE s.manifest_id=? AND s.root_id=? AND s.source_key=? AND r.device_id=?`, owner.ManifestID, s.RootID, s.SourceKey, s.DeviceID).Scan(&rootProvider, &configuredRoot, &canonical)
	if err != nil {
		return nil, rawsync.CanonicalManifest{}, fmt.Errorf("snapshot source ownership: %w", err)
	}
	if rootProvider != provider {
		return nil, rawsync.CanonicalManifest{}, errors.New("snapshot provider differs")
	}
	err = conn.QueryRowContext(ctx, `SELECT count(*) FROM raw_archive_sessions WHERE device_id=? AND provider=? AND parser_id=? AND root_id=? AND source_key=?`, s.DeviceID, provider, owner.NativeSessionID, s.RootID, s.SourceKey).Scan(&count)
	if err != nil {
		return nil, rawsync.CanonicalManifest{}, err
	}
	if count != 1 {
		return nil, rawsync.CanonicalManifest{}, errors.New("snapshot session ownership differs")
	}
	identity, err := rawsync.NewAuthIdentity(archiveID, s.DeviceID)
	if err != nil {
		return nil, rawsync.CanonicalManifest{}, err
	}
	manifest, err := rawsync.ParseCanonicalManifest(identity, owner.ManifestID, canonical, rawsync.DefaultManifestLimits())
	if err != nil {
		return nil, rawsync.CanonicalManifest{}, err
	}
	if manifest.Manifest.ConfiguredRootID != configuredRoot || manifest.Manifest.SourceKey != s.SourceKey || string(manifest.Manifest.Provider) != provider || manifest.Manifest.Kind != rawsync.ManifestSnapshot {
		return nil, rawsync.CanonicalManifest{}, errors.New("snapshot manifest identity differs")
	}
	tree, err = (rawderive.Materializer{Store: snapshotObjects{selected}, BaseDir: os.TempDir(), MaxTotalBytes: limit}).Materialize(ctx, manifest)
	return tree, manifest, err
}

type boundedSnapshotWriter struct {
	dst       io.Writer
	remaining int64
}

func (w *boundedSnapshotWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, errors.New("saved archive database exceeds recorded size")
	}
	n, err := w.dst.Write(p)
	w.remaining -= int64(n)
	return n, err
}
