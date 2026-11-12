package archivesource

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawsync"
	"go.kenn.io/docbank/docbank"
)

// MaterializeSnapshot reconstructs an attached source under private scratch.
// The caller owns the returned materialization and must clean it up.
func MaterializeSnapshot(ctx context.Context, sources []config.ArchiveSource, owners []db.ArchiveSessionProvenance, provider string, limit int64) (tree *rawderive.Materialization, primary string, attachment db.ArchiveAttachmentKey, retErr error) {
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
	var owner db.ArchiveSessionProvenance
	var candidateErr error
	for _, source := range sources {
		if source.Directory != "" || !slices.ContainsFunc(owners, func(o db.ArchiveSessionProvenance) bool { return o.Attachment.SnapshotID == source.Snapshot }) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, "", db.ArchiveAttachmentKey{}, err
		}
		repo, err := docbank.OpenBackupRepository(source.Repository)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return nil, "", db.ArchiveAttachmentKey{}, err
			}
			candidateErr = errors.Join(candidateErr, err)
			continue
		}
		i := slices.IndexFunc(owners, func(o db.ArchiveSessionProvenance) bool {
			return o.Attachment.RepositoryID == repo.ID() && o.Attachment.SnapshotID == source.Snapshot
		})
		if i < 0 {
			continue
		}
		selected, err = repo.OpenSnapshot(ctx, source.Snapshot)
		if err != nil {
			// LoadManifest reads exactly this path before opening the index or members.
			var pe *os.PathError
			if errors.Is(err, os.ErrNotExist) && errors.As(err, &pe) && pe.Path == source.Snapshot+".mvmanifest" {
				candidateErr = errors.Join(candidateErr, err)
				continue
			}
			return nil, "", db.ArchiveAttachmentKey{}, err
		}
		owner = owners[i]
		break
	}
	if selected == nil {
		if candidateErr != nil {
			return nil, "", db.ArchiveAttachmentKey{}, candidateErr
		}
		if slices.ContainsFunc(owners, func(owner db.ArchiveSessionProvenance) bool {
			return strings.HasPrefix(owner.Attachment.SnapshotID, "directory:")
		}) {
			return nil, "", db.ArchiveAttachmentKey{}, errors.New("directory captures cannot materialize native sessions; configure the owning snapshot")
		}
		return nil, "", db.ArchiveAttachmentKey{}, errors.New("owning snapshot is not configured")
	}
	if err := readRecoveryMetadata(ctx, selected); err != nil {
		return nil, "", db.ArchiveAttachmentKey{}, err
	}
	scratch, err := os.MkdirTemp("", "agentsview-snapshot-")
	if err != nil {
		return nil, "", db.ArchiveAttachmentKey{}, err
	}
	defer func() {
		if scratch != "" {
			retErr = errors.Join(retErr, os.RemoveAll(scratch))
		}
	}()
	object, err := selected.Extra("application/sessions.db")
	if err != nil {
		return nil, "", db.ArchiveAttachmentKey{}, err
	}
	if object.Bytes < 0 || object.Bytes > sourceScratchBytes {
		return nil, "", db.ArchiveAttachmentKey{}, errors.New("saved archive database exceeds size limit")
	}
	saved := filepath.Join(scratch, "sessions.db")
	if err := copyDatabase(ctx, selected, saved, object.Bytes, nil); err != nil {
		return nil, "", db.ArchiveAttachmentKey{}, err
	}
	uri := immutableURI(saved)
	conn, err := sql.Open("sqlite3", uri)
	if err != nil {
		return nil, "", db.ArchiveAttachmentKey{}, err
	}
	defer func() {
		if conn != nil {
			retErr = errors.Join(retErr, conn.Close())
		}
	}()
	archiveID, err := checkSavedSchema(ctx, conn)
	if err != nil {
		return nil, "", db.ArchiveAttachmentKey{}, err
	}
	s := owner.Source
	if archiveID != s.ArchiveID {
		return nil, "", db.ArchiveAttachmentKey{}, errors.New("snapshot archive identity differs")
	}
	var canonical []byte
	var configuredRoot, originalRoot string
	var count int
	err = conn.QueryRowContext(ctx, `SELECT r.configured_root_id,r.original_path,s.canonical_json FROM raw_archive_sources s JOIN raw_archive_roots r ON r.id=s.root_id WHERE s.manifest_id=? AND s.root_id=? AND s.source_key=? AND r.device_id=? AND r.provider=?`, owner.ManifestID, s.RootID, s.SourceKey, s.DeviceID, provider).Scan(&configuredRoot, &originalRoot, &canonical)
	if err != nil {
		return nil, "", db.ArchiveAttachmentKey{}, fmt.Errorf("snapshot source ownership: %w", err)
	}
	err = conn.QueryRowContext(ctx, `SELECT count(*) FROM raw_archive_sessions WHERE device_id=? AND provider=? AND parser_id=? AND root_id=? AND source_key=?`, s.DeviceID, provider, owner.NativeSessionID, s.RootID, s.SourceKey).Scan(&count)
	if err != nil {
		return nil, "", db.ArchiveAttachmentKey{}, err
	}
	if count != 1 {
		return nil, "", db.ArchiveAttachmentKey{}, errors.New("snapshot session ownership differs")
	}
	identity, err := rawsync.NewAuthIdentity(archiveID, s.DeviceID)
	if err != nil {
		return nil, "", db.ArchiveAttachmentKey{}, err
	}
	manifest, err := rawsync.ParseCanonicalManifest(identity, owner.ManifestID, canonical, rawsync.DefaultManifestLimits())
	if err != nil {
		return nil, "", db.ArchiveAttachmentKey{}, err
	}
	if manifest.Manifest.ConfiguredRootID != configuredRoot || manifest.Manifest.SourceKey != s.SourceKey || string(manifest.Manifest.Provider) != provider {
		return nil, "", db.ArchiveAttachmentKey{}, errors.New("snapshot manifest identity differs")
	}
	bound, primary, err := snapshotPrimaryPath(s.SourceKey, originalRoot, owner.NativeSessionID, manifest.Manifest.Entries)
	if err != nil {
		return nil, "", db.ArchiveAttachmentKey{}, err
	}
	base := strings.TrimSuffix(primary, ".jsonl") + "/"
	bound.Path = primary
	entries, err := selectedInventory(ctx, conn, s.RootID, bound, base)
	if err != nil {
		return nil, "", db.ArchiveAttachmentKey{}, err
	}
	if !slices.ContainsFunc(entries, func(e rawsync.Entry) bool {
		return e.Path == primary && e.Length == bound.Length && slices.Equal(e.Objects, bound.Objects)
	}) {
		return nil, "", db.ArchiveAttachmentKey{}, errors.New("snapshot transcript inventory differs from manifest")
	}
	if err := conn.Close(); err != nil {
		conn = nil
		return nil, "", db.ArchiveAttachmentKey{}, err
	}
	conn = nil
	if err := os.RemoveAll(scratch); err != nil {
		return nil, "", db.ArchiveAttachmentKey{}, err
	}
	scratch = ""
	tree, err = (rawderive.Materializer{Store: snapshotObjects{selected}, BaseDir: os.TempDir(), MaxTotalBytes: limit}).MaterializeEntries(ctx, identity, manifest.Manifest.CapturedAt, entries)
	return tree, primary, owner.Attachment, err
}

// snapshotPrimaryPath binds a canonical entry to its capture-root coordinate.
func snapshotPrimaryPath(sourceKey, originalRoot, nativeID string, entries []rawsync.Entry) (rawsync.Entry, string, error) {
	portable := func(name string) string { return path.Clean(strings.ReplaceAll(name, `\`, "/")) }
	key, root := portable(sourceKey), strings.TrimSuffix(portable(originalRoot), "/")
	var bound rawsync.Entry
	for _, entry := range entries {
		if path.Base(entry.Path) == nativeID+".jsonl" && rawderive.SourceKeyEndsWithEntry(key, entry.Path) {
			if bound.Path != "" {
				return rawsync.Entry{}, "", errors.New("ambiguous snapshot transcript source")
			}
			bound = entry
		}
	}
	if bound.Path == "" {
		return rawsync.Entry{}, "", errors.New("snapshot transcript source differs")
	}
	providerRoot := strings.TrimSuffix(strings.TrimSuffix(key, bound.Path), "/")
	var relativeRoot string
	if providerRoot != root {
		var ok bool
		relativeRoot, ok = strings.CutPrefix(providerRoot, root+"/")
		if !ok || relativeRoot == "" || path.Clean(relativeRoot) != relativeRoot {
			return rawsync.Entry{}, "", errors.New("snapshot transcript outside capture root")
		}
	}
	return bound, path.Join(relativeRoot, bound.Path), nil
}

// selectedInventory chooses immutable accepted bytes, or one unambiguous supplemental version.
func selectedInventory(ctx context.Context, conn *sql.DB, rootID string, bound rawsync.Entry, base string) (_ []rawsync.Entry, retErr error) {
	tools, agents := base+"tool-results/", base+"subagents/"
	rows, err := conn.QueryContext(ctx, `SELECT path,sha256,size,mod_time_ns,covered FROM raw_archive_files WHERE root_id=? AND (path=? OR substr(path,1,length(?))=? OR substr(path,1,length(?))=?) ORDER BY path`, rootID, bound.Path, tools, tools, agents, agents)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	type choice struct{ covered, supplemental []rawsync.Entry }
	choices := map[string]choice{}
	for rows.Next() {
		var e rawsync.Entry
		var digest string
		var covered bool
		if err := rows.Scan(&e.Path, &digest, &e.Length, &e.ModTimeNS, &covered); err != nil {
			return nil, err
		}
		e.Type = "file"
		e.Objects = []rawsync.ObjectRef{{SHA256: digest, Length: e.Length}}
		c := choices[e.Path]
		if e.Path == bound.Path {
			if e.Length == bound.Length && slices.Equal(e.Objects, bound.Objects) {
				c.covered = append(c.covered, e)
			}
		} else if covered {
			c.covered = append(c.covered, e)
		} else {
			c.supplemental = append(c.supplemental, e)
		}
		choices[e.Path] = c
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(choices[bound.Path].covered) != 1 {
		return nil, errors.New("snapshot transcript inventory differs from manifest")
	}
	var entries []rawsync.Entry
	for _, c := range choices {
		versions := c.covered
		if len(versions) == 0 {
			versions = c.supplemental
		}
		if len(versions) != 1 {
			return nil, errors.New("ambiguous snapshot inventory versions")
		}
		entries = append(entries, versions[0])
	}
	slices.SortFunc(entries, func(a, b rawsync.Entry) int { return strings.Compare(a.Path, b.Path) })
	return entries, nil
}
