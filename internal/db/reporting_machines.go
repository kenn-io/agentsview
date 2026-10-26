package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"go.kenn.io/agentsview/internal/export"
)

func loadReportingMachineIdentity(ctx context.Context, tx *sql.Tx, schema int, s *reportingExportSource) error {
	if schema != export.ReportingJointSchemaVersion {
		return errors.New("machine snapshots require reporting schema 5")
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT value FROM pg_sync_state
		WHERE key = 'artifact_local_installation_id'), '')`).Scan(&s.localMachineID); err != nil {
		return err
	}
	if strings.TrimSpace(s.localMachineID) == "" {
		return errors.New("machine snapshots require recorded installation identity; run agentsview sync first")
	}
	rows, err := tx.QueryContext(ctx, `SELECT substr(key, length('machine_alias:') + 1), value
		FROM pg_sync_state WHERE key LIKE 'machine\_alias:%' ESCAPE '\'`)
	if err != nil {
		return err
	}
	defer rows.Close()
	s.machineAliases = make(map[string]string)
	for rows.Next() {
		var alias, id string
		if err := rows.Scan(&alias, &id); err != nil {
			return err
		}
		s.machineAliases[alias] = id
	}
	return rows.Err()
}

func (s reportingDaySource) machineID(machine string) string {
	if machine == "" {
		return "archive:" + s.archiveID
	}
	if machine == "local" {
		return s.localMachineID
	}
	if canonical := s.machineAliases[machine]; canonical != "" {
		return canonical
	}
	return machine
}
