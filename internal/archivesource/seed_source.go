package archivesource

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/rawarchive"
)

// unboundRetainedSource finds a unique accepted source by its recorded path.
// materializeCatalog authenticates the selected source and native identity.
func unboundRetainedSource(ctx context.Context, q catalogQuery, installation, machine, provider, native, stored string) (owner db.ArchiveSessionProvenance, found bool, retErr error) {
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
	base := stored[strings.LastIndexAny(stored, `/\`)+1:]
	rows, err := q.QueryContext(ctx, `SELECT s.root_id,r.original_path,s.manifest_id,s.source_key,s.original_path,a.alias
		FROM raw_archive_roots r JOIN raw_archive_sources s ON s.root_id=r.id LEFT JOIN raw_archive_root_aliases a ON a.root_id=r.id
		WHERE r.device_id=? AND r.provider=? AND substr(s.original_path,-length(?))=?`, installation, provider, base, base)
	if err != nil {
		return owner, false, err
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	matches := make(map[db.ArchiveSessionProvenance]bool)
	for rows.Next() {
		var candidate db.ArchiveSessionProvenance
		var root, source string
		var alias sql.NullString
		if err := rows.Scan(&candidate.Source.RootID, &root, &candidate.ManifestID, &candidate.Source.SourceKey, &source, &alias); err != nil {
			return owner, false, err
		}
		var aliases []string
		if alias.Valid {
			aliases = []string{alias.String}
		}
		if slices.Contains(rawarchive.EquivalentSourcePaths([]string{source}, root, aliases), stored) {
			matches[candidate], owner = true, candidate
		}
	}
	owner.Source.DeviceID, owner.NativeSessionID = installation, native
	return owner, len(matches) == 1, rows.Err()
}
