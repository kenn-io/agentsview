package archivesource

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawsync"
	"go.kenn.io/docbank/docbank"
)

// MaterializeSnapshot reconstructs an attached source under private scratch.
// The caller owns the returned materialization and must clean it up.
func MaterializeSnapshot(ctx context.Context, sources []config.ArchiveSource, owners []db.ArchiveSessionProvenance, provider string, limit int64) (tree *rawderive.Materialization, result rawsync.CanonicalManifest, attachment db.ArchiveAttachmentKey, retErr error) {
	defer func() {
		if retErr != nil && tree != nil {
			retErr = errors.Join(retErr, tree.Cleanup())
			tree = nil
		}
	}()
	if len(owners) == 0 {
		return nil, rawsync.CanonicalManifest{}, db.ArchiveAttachmentKey{}, errors.New("snapshot ownership is missing")
	}
	for _, candidate := range owners[1:] {
		if candidate.Source != owners[0].Source || candidate.ManifestID != owners[0].ManifestID || candidate.NativeSessionID != owners[0].NativeSessionID {
			return nil, rawsync.CanonicalManifest{}, db.ArchiveAttachmentKey{}, errors.New("conflicting snapshot source identities")
		}
	}
	var selected *docbank.BackupReader
	defer func() {
		if selected != nil {
			retErr = errors.Join(retErr, selected.Close())
		}
	}()
	var owner db.ArchiveSessionProvenance
	var candidateErr error
	var directoryOnly bool
	for _, source := range sources {
		if source.Directory != "" {
			directoryOnly = true
			continue
		}
		for _, candidate := range owners {
			if source.Snapshot != candidate.Attachment.SnapshotID {
				continue
			}
			repo, err := docbank.OpenBackupRepository(source.Repository)
			if err != nil {
				candidateErr = errors.Join(candidateErr, err)
				continue
			}
			if repo.ID() != candidate.Attachment.RepositoryID {
				continue
			}
			selected, err = repo.OpenSnapshot(ctx, source.Snapshot)
			if err != nil {
				return nil, rawsync.CanonicalManifest{}, db.ArchiveAttachmentKey{}, err
			}
			owner = candidate
			break
		}
		if selected != nil {
			break
		}
	}
	if selected == nil {
		if candidateErr != nil {
			return nil, rawsync.CanonicalManifest{}, db.ArchiveAttachmentKey{}, candidateErr
		}
		if directoryOnly {
			return nil, rawsync.CanonicalManifest{}, db.ArchiveAttachmentKey{}, errors.New("directory captures cannot materialize native sessions; configure the owning snapshot")
		}
		return nil, rawsync.CanonicalManifest{}, db.ArchiveAttachmentKey{}, errors.New("owning snapshot is not configured")
	}
	if err := readRecoveryMetadata(ctx, selected); err != nil {
		return nil, rawsync.CanonicalManifest{}, db.ArchiveAttachmentKey{}, err
	}
	scratch, err := os.MkdirTemp("", "agentsview-snapshot-")
	if err != nil {
		return nil, rawsync.CanonicalManifest{}, db.ArchiveAttachmentKey{}, err
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(scratch)) }()
	object, err := selected.Extra("application/sessions.db")
	if err != nil {
		return nil, rawsync.CanonicalManifest{}, db.ArchiveAttachmentKey{}, err
	}
	if object.Bytes < 0 || object.Bytes > sourceScratchBytes {
		return nil, rawsync.CanonicalManifest{}, db.ArchiveAttachmentKey{}, errors.New("saved archive database exceeds size limit")
	}
	saved := filepath.Join(scratch, "sessions.db")
	if err := copyDatabase(ctx, selected, saved, object.Bytes, nil); err != nil {
		return nil, rawsync.CanonicalManifest{}, db.ArchiveAttachmentKey{}, err
	}
	uri := immutableURI(saved)
	conn, err := sql.Open("sqlite3", uri)
	if err != nil {
		return nil, rawsync.CanonicalManifest{}, db.ArchiveAttachmentKey{}, err
	}
	defer func() { retErr = errors.Join(retErr, conn.Close()) }()
	archiveID, err := checkSavedSchema(ctx, conn)
	if err != nil {
		return nil, rawsync.CanonicalManifest{}, db.ArchiveAttachmentKey{}, err
	}
	s := owner.Source
	if archiveID != s.ArchiveID {
		return nil, rawsync.CanonicalManifest{}, db.ArchiveAttachmentKey{}, errors.New("snapshot archive identity differs")
	}
	var canonical []byte
	var rootProvider, configuredRoot, originalRoot string
	var count int
	err = conn.QueryRowContext(ctx, `SELECT r.provider,r.configured_root_id,r.original_path,s.canonical_json FROM raw_archive_sources s JOIN raw_archive_roots r ON r.id=s.root_id WHERE s.manifest_id=? AND s.root_id=? AND s.source_key=? AND r.device_id=?`, owner.ManifestID, s.RootID, s.SourceKey, s.DeviceID).Scan(&rootProvider, &configuredRoot, &originalRoot, &canonical)
	if err != nil {
		return nil, rawsync.CanonicalManifest{}, db.ArchiveAttachmentKey{}, fmt.Errorf("snapshot source ownership: %w", err)
	}
	if rootProvider != provider {
		return nil, rawsync.CanonicalManifest{}, db.ArchiveAttachmentKey{}, errors.New("snapshot provider differs")
	}
	err = conn.QueryRowContext(ctx, `SELECT count(*) FROM raw_archive_sessions WHERE device_id=? AND provider=? AND parser_id=? AND root_id=? AND source_key=?`, s.DeviceID, provider, owner.NativeSessionID, s.RootID, s.SourceKey).Scan(&count)
	if err != nil {
		return nil, rawsync.CanonicalManifest{}, db.ArchiveAttachmentKey{}, err
	}
	if count != 1 {
		return nil, rawsync.CanonicalManifest{}, db.ArchiveAttachmentKey{}, errors.New("snapshot session ownership differs")
	}
	identity, err := rawsync.NewAuthIdentity(archiveID, s.DeviceID)
	if err != nil {
		return nil, rawsync.CanonicalManifest{}, db.ArchiveAttachmentKey{}, err
	}
	manifest, err := rawsync.ParseCanonicalManifest(identity, owner.ManifestID, canonical, rawsync.DefaultManifestLimits())
	if err != nil {
		return nil, rawsync.CanonicalManifest{}, db.ArchiveAttachmentKey{}, err
	}
	if manifest.Manifest.ConfiguredRootID != configuredRoot || manifest.Manifest.SourceKey != s.SourceKey || string(manifest.Manifest.Provider) != provider || manifest.Manifest.Kind != rawsync.ManifestSnapshot {
		return nil, rawsync.CanonicalManifest{}, db.ArchiveAttachmentKey{}, errors.New("snapshot manifest identity differs")
	}
	var primary string
	for _, entry := range manifest.Manifest.Entries {
		if path.Base(entry.Path) == owner.NativeSessionID+".jsonl" {
			if primary != "" {
				return nil, rawsync.CanonicalManifest{}, db.ArchiveAttachmentKey{}, errors.New("ambiguous snapshot transcript")
			}
			primary = entry.Path
		}
	}
	if primary == "" {
		return nil, rawsync.CanonicalManifest{}, db.ArchiveAttachmentKey{}, errors.New("snapshot transcript is unavailable")
	}
	portable := func(name string) string { return path.Clean(strings.ReplaceAll(name, `\`, "/")) }
	if path.Join(portable(originalRoot), primary) != portable(s.SourceKey) {
		return nil, rawsync.CanonicalManifest{}, db.ArchiveAttachmentKey{}, errors.New("snapshot transcript source differs")
	}
	// Subagent transcripts are captured as independent sources. Select only
	// source keys beneath this session's own native subagent directory.
	base := strings.TrimSuffix(primary, ".jsonl") + "/"
	prefix := strings.TrimSuffix(portable(s.SourceKey), ".jsonl") + "/subagents/"
	rows, err := conn.QueryContext(ctx, `SELECT source_key,manifest_id,canonical_json FROM raw_archive_sources WHERE root_id=? AND substr(replace(source_key,char(92),'/'),1,length(?))=?`, s.RootID, prefix, prefix)
	if err != nil {
		return nil, rawsync.CanonicalManifest{}, db.ArchiveAttachmentKey{}, err
	}
	manifests := []rawsync.CanonicalManifest{manifest}
	for rows.Next() {
		var key, manifestID string
		var data []byte
		if err := rows.Scan(&key, &manifestID, &data); err != nil {
			rows.Close()
			return nil, rawsync.CanonicalManifest{}, db.ArchiveAttachmentKey{}, err
		}
		child, err := rawsync.ParseCanonicalManifest(identity, manifestID, data, rawsync.DefaultManifestLimits())
		if err != nil {
			rows.Close()
			return nil, rawsync.CanonicalManifest{}, db.ArchiveAttachmentKey{}, err
		}
		if child.Manifest.ConfiguredRootID != configuredRoot || child.Manifest.SourceKey != key || string(child.Manifest.Provider) != provider || child.Manifest.Kind != rawsync.ManifestSnapshot {
			rows.Close()
			return nil, rawsync.CanonicalManifest{}, db.ArchiveAttachmentKey{}, errors.New("snapshot subagent identity differs")
		}
		rel, ok := strings.CutPrefix(portable(key), strings.TrimSuffix(portable(originalRoot), "/")+"/")
		if !ok || !strings.HasPrefix(rel, base+"subagents/") {
			rows.Close()
			return nil, rawsync.CanonicalManifest{}, db.ArchiveAttachmentKey{}, errors.New("snapshot subagent path differs")
		}
		found := false
		for _, entry := range child.Manifest.Entries {
			if entry.Path == rel {
				found = true
			}
		}
		if !found {
			rows.Close()
			return nil, rawsync.CanonicalManifest{}, db.ArchiveAttachmentKey{}, errors.New("snapshot subagent transcript is unavailable")
		}
		manifests = append(manifests, child)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, rawsync.CanonicalManifest{}, db.ArchiveAttachmentKey{}, err
	}
	selectEntry := func(source rawsync.CanonicalManifest, entry rawsync.Entry) bool {
		if source.ManifestID == manifest.ManifestID {
			return entry.Path == primary || strings.HasPrefix(entry.Path, base+"tool-results/")
		}
		// One captured subagent source can own continuation transcripts.
		return strings.HasPrefix(entry.Path, base+"subagents/")
	}
	tree, err = (rawderive.Materializer{Store: snapshotObjects{selected}, BaseDir: os.TempDir(), MaxTotalBytes: limit, SelectEntry: selectEntry}).MaterializeMany(ctx, manifests)
	return tree, manifest, owner.Attachment, err
}
