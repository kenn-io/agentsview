package archivesource

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawsync"
	"go.kenn.io/docbank/docbank"
)

// MaterializeSnapshot reconstructs an attached source under private scratch.
// The caller owns the returned materialization and must clean it up.
func MaterializeSnapshot(ctx context.Context, sources []config.ArchiveSource, owners []db.ArchiveSessionProvenance, provider string, limit int64) (tree *rawderive.Materialization, primary string, snapshot string, retErr error) {
	return materializeSnapshot(ctx, sources, owners, provider, limit, nil)
}

// ReadCodexSnapshot retains the snapshot lease while rollout pages are read.
func ReadCodexSnapshot(ctx context.Context, sources []config.ArchiveSource, owners []db.ArchiveSessionProvenance, limit int64, use func(string, []string, []string, func(context.Context, string, int64) ([]byte, error), CodexAncestry) error) (string, error) {
	_, _, snapshot, err := materializeSnapshot(ctx, sources, owners, "codex", limit, use)
	return snapshot, err
}

func materializeSnapshot(ctx context.Context, sources []config.ArchiveSource, owners []db.ArchiveSessionProvenance, provider string, limit int64, use func(string, []string, []string, func(context.Context, string, int64) ([]byte, error), CodexAncestry) error) (tree *rawderive.Materialization, primary string, snapshot string, retErr error) {
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
			return nil, "", "", err
		}
		repo, err := docbank.OpenBackupRepository(source.Repository)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return nil, "", "", err
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
			if pe, ok := errors.AsType[*os.PathError](err); errors.Is(err, os.ErrNotExist) && ok && pe.Path == source.Snapshot+".mvmanifest" {
				candidateErr = errors.Join(candidateErr, err)
				continue
			}
			return nil, "", "", err
		}
		owner = owners[i]
		break
	}
	if selected == nil {
		if candidateErr != nil {
			return nil, "", "", candidateErr
		}
		if slices.ContainsFunc(owners, func(owner db.ArchiveSessionProvenance) bool {
			return strings.HasPrefix(owner.Attachment.SnapshotID, "directory:")
		}) {
			return nil, "", "", errors.New("directory captures cannot materialize native sessions; configure the owning snapshot")
		}
		return nil, "", "", errors.New("owning snapshot is not configured")
	}
	if err := readRecoveryMetadata(ctx, selected); err != nil {
		return nil, "", "", err
	}
	scratch, err := os.MkdirTemp("", "agentsview-snapshot-")
	if err != nil {
		return nil, "", "", err
	}
	defer func() {
		if scratch != "" {
			retErr = errors.Join(retErr, os.RemoveAll(scratch))
		}
	}()
	object, err := selected.Extra("application/sessions.db")
	if err != nil {
		return nil, "", "", err
	}
	if object.Bytes < 0 || object.Bytes > sourceScratchBytes {
		return nil, "", "", errors.New("saved archive database exceeds size limit")
	}
	saved := filepath.Join(scratch, "sessions.db")
	if err := copyDatabase(ctx, selected, saved, object.Bytes, nil); err != nil {
		return nil, "", "", err
	}
	uri := immutableURI(saved)
	conn, err := sql.Open("sqlite3", uri)
	if err != nil {
		return nil, "", "", err
	}
	defer func() {
		if conn != nil {
			retErr = errors.Join(retErr, conn.Close())
		}
	}()
	archiveID, err := checkSavedSchema(ctx, conn)
	if err != nil {
		return nil, "", "", err
	}
	s := owner.Source
	if archiveID != s.ArchiveID {
		return nil, "", "", errors.New("snapshot archive identity differs")
	}
	var canonical []byte
	var configuredRoot, originalRoot, originalPath string
	err = conn.QueryRowContext(ctx, `SELECT r.configured_root_id,r.original_path,s.original_path,s.canonical_json FROM raw_archive_sources s JOIN raw_archive_roots r ON r.id=s.root_id WHERE s.manifest_id=? AND s.root_id=? AND s.source_key=? AND r.device_id=? AND r.provider=?`, owner.ManifestID, s.RootID, s.SourceKey, s.DeviceID, provider).Scan(&configuredRoot, &originalRoot, &originalPath, &canonical)
	if err != nil {
		return nil, "", "", fmt.Errorf("snapshot source ownership: %w", err)
	}
	var boundRoot, boundSource string
	err = conn.QueryRowContext(ctx, `SELECT root_id,source_key FROM raw_archive_sessions WHERE device_id=? AND provider=? AND parser_id=?`, s.DeviceID, provider, owner.NativeSessionID).Scan(&boundRoot, &boundSource)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, "", "", err
	}
	if err == nil && (boundRoot != s.RootID || boundSource != s.SourceKey) {
		return nil, "", "", errors.New("snapshot session ownership differs")
	}
	identity, err := rawsync.NewAuthIdentity(archiveID, s.DeviceID)
	if err != nil {
		return nil, "", "", err
	}
	manifest, bound, primary, err := acceptedSource(identity, acceptedRow{manifestID: owner.ManifestID, configuredRoot: configuredRoot, originalRoot: originalRoot, originalPath: originalPath, sourceKey: s.SourceKey, canonical: canonical}, provider, owner.NativeSessionID)
	if err != nil {
		return nil, "", "", err
	}
	base := strings.TrimSuffix(primary, ".jsonl") + "/"
	bound.Path = primary
	if use != nil {
		return nil, "", owner.Attachment.SnapshotID, readCodexCatalog(ctx, conn, s.RootID, originalRoot, bound, identity, manifest, snapshotObjects{selected}, limit, use)
	}
	entries, err := selectedInventory(ctx, conn, s.RootID, bound, base)
	if err != nil {
		return nil, "", "", err
	}
	if err := conn.Close(); err != nil {
		conn = nil
		return nil, "", "", err
	}
	conn = nil
	if err := os.RemoveAll(scratch); err != nil {
		return nil, "", "", err
	}
	scratch = ""
	tree, err = (rawderive.Materializer{Store: snapshotObjects{selected}, BaseDir: os.TempDir(), MaxTotalBytes: limit}).MaterializeEntries(ctx, identity, manifest.Manifest.CapturedAt, entries)
	return tree, primary, owner.Attachment.SnapshotID, err
}

// readCodexCatalog keeps its backing reader open through the synchronous callback.
func readCodexCatalog(ctx context.Context, conn *sql.DB, rootID, originalRoot string, bound rawsync.Entry, identity rawsync.AuthIdentity, manifest rawsync.CanonicalManifest, objects rawderive.ObjectStore, limit int64, use func(string, []string, []string, func(context.Context, string, int64) ([]byte, error), CodexAncestry) error) error {
	rows, err := conn.QueryContext(ctx, `SELECT path,sha256,size,mod_time_ns,covered FROM raw_archive_files WHERE root_id=? AND (path LIKE '%.jsonl' OR path LIKE '%.jsonl.zst') ORDER BY path`, rootID)
	if err != nil {
		return err
	}
	conflicts := make(map[string]bool)
	items, err := inventoryEntries(rows, bound, conflicts)
	if err != nil {
		return err
	}
	byPath := make(map[string]rawsync.Entry, len(items))
	names := make([]string, 0, len(items))
	for _, entry := range items {
		byPath[entry.Path] = entry
		names = append(names, entry.Path)
	}
	ancestors := codexCompanions(manifest.Manifest.Entries, names, byPath)
	read := codexCatalogReader(byPath, conflicts, identity, manifest, objects, limit)
	return use(originalRoot, names, ancestors, read, codexAncestry(conn, identity, manifest, byPath))
}

func codexCatalogReader(byPath map[string]rawsync.Entry, conflicts map[string]bool, identity rawsync.AuthIdentity, manifest rawsync.CanonicalManifest, objects rawderive.ObjectStore, limit int64) func(context.Context, string, int64) ([]byte, error) {
	return func(ctx context.Context, name string, requested int64) (_ []byte, readErr error) {
		entry, ok := byPath[name]
		if conflicts[name] {
			return nil, errors.New("conflicting saved file versions")
		}
		if !ok || requested <= 0 || entry.Length > requested {
			return nil, errors.New("snapshot rollout unavailable or exceeds size limit")
		}
		m := rawderive.Materializer{Store: objects, BaseDir: os.TempDir(), MaxTotalBytes: min(limit, requested)}
		materialized, err := m.MaterializeEntries(ctx, identity, manifest.Manifest.CapturedAt, []rawsync.Entry{entry})
		if err != nil {
			return nil, err
		}
		defer func() { readErr = errors.Join(readErr, materialized.Cleanup()) }()
		filename, err := materialized.EntryPath(entry.Path)
		if err != nil {
			return nil, err
		}
		file, err := os.Open(filename)
		if err != nil {
			return nil, err
		}
		defer func() { readErr = errors.Join(readErr, file.Close()) }()
		return io.ReadAll(io.LimitReader(file, entry.Length+1))
	}
}

// codexCompanions adds only manifest-authenticated rollouts outside the root inventory.
func codexCompanions(entries []rawsync.Entry, names []string, byPath map[string]rawsync.Entry) []string {
	rootKeys := make(map[string]bool, len(names))
	for _, name := range names {
		rootKeys[parser.CodexSessionUUIDFromFilename(path.Base(name))] = true
	}
	seen := make(map[string][]rawsync.Entry)
	var ancestors []string
	for _, entry := range entries {
		key := parser.CodexSessionUUIDFromFilename(path.Base(entry.Path))
		if key == "" || rootKeys[key] || slices.ContainsFunc(seen[key], func(other rawsync.Entry) bool {
			return other.Length == entry.Length && slices.Equal(other.Objects, entry.Objects)
		}) {
			continue
		}
		seen[key] = append(seen[key], entry)
		byPath[entry.Path] = entry
		ancestors = append(ancestors, entry.Path)
	}
	return ancestors
}

// snapshotPrimaryPath binds a canonical entry to its capture-root coordinate.
func snapshotPrimaryPath(sourceKey, originalPath, originalRoot, nativeID string, entries []rawsync.Entry) (rawsync.Entry, string, error) {
	portable := func(name string) string { return path.Clean(strings.ReplaceAll(name, `\`, "/")) }
	key, source, root := portable(sourceKey), portable(originalPath), strings.TrimSuffix(portable(originalRoot), "/")
	var bound rawsync.Entry
	for _, entry := range entries {
		matches := path.Base(entry.Path) == nativeID+".jsonl"
		if native, ok := strings.CutPrefix(nativeID, "codex:"); ok {
			matches = parser.CodexSessionUUIDFromFilename(path.Base(entry.Path)) == native
		}
		keyMatches := rawderive.SourceKeyEndsWithEntry(key, entry.Path)
		if strings.HasPrefix(key, "codex:") {
			keyMatches = key == parser.CodexSourceKey(parser.AgentCodex, parser.CodexSessionUUIDFromFilename(path.Base(entry.Path)))
		}
		if matches && keyMatches && rawderive.SourceKeyEndsWithEntry(source, entry.Path) {
			if bound.Path != "" {
				return rawsync.Entry{}, "", errors.New("ambiguous snapshot transcript source")
			}
			bound = entry
		}
	}
	if bound.Path == "" {
		return rawsync.Entry{}, "", errors.New("snapshot transcript source differs")
	}
	providerRoot := strings.TrimSuffix(strings.TrimSuffix(source, bound.Path), "/")
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
	return inventoryEntries(rows, bound, nil)
}

func inventoryEntries(rows *sql.Rows, bound rawsync.Entry, conflicts map[string]bool) (_ []rawsync.Entry, retErr error) {
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
		return nil, errors.New("saved transcript does not match snapshot")
	}
	var entries []rawsync.Entry
	for _, c := range choices {
		versions := c.covered
		if len(versions) == 0 {
			versions = c.supplemental
		}
		if len(versions) != 1 {
			if conflicts == nil {
				return nil, errors.New("conflicting saved file versions")
			}
			conflicts[versions[0].Path] = true
		}
		entries = append(entries, versions[0])
	}
	slices.SortFunc(entries, func(a, b rawsync.Entry) int { return strings.Compare(a.Path, b.Path) })
	return entries, nil
}
