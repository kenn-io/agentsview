package archivesource

import (
	"context"
	"errors"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/rawarchive"
)

// unboundRetainedSource finds a unique accepted source by its recorded path.
// materializeCatalog authenticates the selected source and native identity.
func unboundRetainedSource(ctx context.Context, q catalogQuery, installation, machine, provider, native, stored string) (db.ArchiveSessionProvenance, bool, error) {
	var owner db.ArchiveSessionProvenance
	if installation == "" || stored == "" || strings.HasPrefix(stored, "archive://") || strings.Contains(native, "~") || (provider != "claude" && provider != "codex") {
		return owner, false, nil
	}
	var recorded, alias string
	err := q.QueryRowContext(ctx, `SELECT COALESCE((SELECT value FROM pg_sync_state WHERE key='artifact_local_installation_id'),''),COALESCE((SELECT value FROM pg_sync_state WHERE key=?),'')`, db.MachineAliasKeyPrefix+machine).Scan(&recorded, &alias)
	if err != nil {
		return owner, false, err
	}
	if (recorded != "" && recorded != installation) || !rawarchive.MachineOwnedBy(machine, installation, installation, map[string]string{machine: alias}) {
		return owner, false, nil
	}
	rows, err := q.QueryContext(ctx, `SELECT id,original_path FROM raw_archive_roots WHERE device_id=? AND provider=?`, installation, provider)
	if err != nil {
		return owner, false, err
	}
	type root struct{ id, path string }
	var roots []root
	for rows.Next() {
		var r root
		if err := rows.Scan(&r.id, &r.path); err != nil {
			return owner, false, errors.Join(err, rows.Close())
		}
		roots = append(roots, r)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return owner, false, err
	}
	base := stored[strings.LastIndexAny(stored, `/\`)+1:]
	count := 0
	for _, root := range roots {
		aliases, err := retainedRootAliases(ctx, q, root.id)
		if err != nil {
			return owner, false, err
		}
		matches, err := retainedPathSources(ctx, q, root.id, root.path, base, stored, aliases)
		if err != nil {
			return owner, false, err
		}
		for _, match := range matches {
			owner = match
			owner.Source.DeviceID, owner.Source.RootID, owner.NativeSessionID = installation, root.id, native
			count++
		}
	}
	return owner, count == 1, nil
}

func retainedRootAliases(ctx context.Context, q catalogQuery, root string) (_ []string, retErr error) {
	rows, err := q.QueryContext(ctx, `SELECT alias FROM raw_archive_root_aliases WHERE root_id=?`, root)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	var aliases []string
	for rows.Next() {
		var alias string
		if err := rows.Scan(&alias); err != nil {
			return nil, err
		}
		aliases = append(aliases, alias)
	}
	return aliases, rows.Err()
}

func retainedPathSources(ctx context.Context, q catalogQuery, root, original, base, stored string, aliases []string) (_ []db.ArchiveSessionProvenance, retErr error) {
	rows, err := q.QueryContext(ctx, `SELECT manifest_id,source_key,original_path FROM raw_archive_sources WHERE root_id=? AND substr(original_path,-length(?))=?`, root, base, base)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	var matches []db.ArchiveSessionProvenance
	for rows.Next() {
		var owner db.ArchiveSessionProvenance
		var source string
		if err := rows.Scan(&owner.ManifestID, &owner.Source.SourceKey, &source); err != nil {
			return nil, err
		}
		if slices.Contains(rawarchive.EquivalentSourcePaths([]string{source}, original, aliases), stored) {
			matches = append(matches, owner)
		}
	}
	return matches, rows.Err()
}
