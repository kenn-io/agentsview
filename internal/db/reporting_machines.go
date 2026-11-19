package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"go.kenn.io/agentsview/internal/export"
)

// machineIdentities maps a session's stored machine key to the identity of
// the machine it originated on. Per-machine reporting snapshots and the
// change stream use the same rule, so their identities compare equal.
type machineIdentities struct {
	// local is this installation's identity, or empty when the archive has
	// none recorded.
	local   string
	aliases map[string]string
}

func loadMachineIdentities(ctx context.Context, tx *sql.Tx) (machineIdentities, error) {
	var ids machineIdentities
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT value FROM pg_sync_state
		WHERE key = 'artifact_local_installation_id'), '')`).Scan(&ids.local); err != nil {
		return ids, err
	}
	if strings.TrimSpace(ids.local) == "" {
		ids.local = ""
	}
	rows, err := tx.QueryContext(ctx, `SELECT substr(key, length('machine_alias:') + 1), value
		FROM pg_sync_state WHERE key LIKE 'machine\_alias:%' ESCAPE '\'`)
	if err != nil {
		return ids, err
	}
	defer rows.Close()
	ids.aliases = make(map[string]string)
	for rows.Next() {
		var alias, id string
		if err := rows.Scan(&alias, &id); err != nil {
			return ids, err
		}
		ids.aliases[alias] = id
	}
	return ids, errors.Join(rows.Err(), rows.Close())
}

// origin returns the originating machine identity for a stored machine key.
// Usage recorded without a session belongs to the archive itself. It returns
// empty for this installation's own sessions when no local identity is
// recorded.
func (m machineIdentities) origin(machine, archiveID string) string {
	if machine == "" {
		return "archive:" + archiveID
	}
	if machine == "local" {
		return m.local
	}
	if canonical := m.aliases[machine]; canonical != "" {
		return canonical
	}
	return machine
}

func loadReportingMachineIdentity(ctx context.Context, tx *sql.Tx, schema int, s *reportingExportSource) error {
	if schema != export.ReportingJointSchemaVersion {
		return errors.New("machine snapshots require reporting schema 5")
	}
	ids, err := loadMachineIdentities(ctx, tx)
	if err != nil {
		return err
	}
	if ids.local == "" {
		return errors.New("machine snapshots require recorded installation identity; run agentsview source sync first")
	}
	s.localMachineID, s.machineAliases = ids.local, ids.aliases
	return nil
}

func (s reportingDaySource) machineID(machine string) string {
	return machineIdentities{local: s.localMachineID, aliases: s.machineAliases}.origin(machine, s.archiveID)
}
