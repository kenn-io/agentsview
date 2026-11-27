package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/export"
)

const (
	cursorAccountPrefix     = "cursor-account:"
	codexCloudAccountPrefix = "codex-cloud-account:"
)

// machineIdentities maps a session's stored machine key to the identity of
// the machine it originated on. Schema 5 reporting and the change stream
// use the same identities.
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
	// A remote host's recorded origin names its sessions; an adopted alias
	// redirects a former name of this archive. Aliases win, ordered last.
	rows, err := tx.QueryContext(ctx, `
		SELECT substr(key, length('machine_origin:') + 1), value, 0
		FROM pg_sync_state WHERE key LIKE 'machine\_origin:%' ESCAPE '\'
		UNION ALL
		SELECT substr(key, length('machine_alias:') + 1), value, 1
		FROM pg_sync_state WHERE key LIKE 'machine\_alias:%' ESCAPE '\'
		ORDER BY 3`)
	if err != nil {
		return ids, err
	}
	defer rows.Close()
	ids.aliases = make(map[string]string)
	for rows.Next() {
		var alias, id string
		var adopted int
		if err := rows.Scan(&alias, &id, &adopted); err != nil {
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
	if !export.IsJointReportingSchema(schema) {
		return errors.New("machine snapshots require reporting schema 5 or 6")
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

// sessionOrigin returns the key a session's activity is reported under. A
// Codex cloud session fetched for an account reports under that account,
// so every archive that syncs the account names the same origin for it. It
// returns empty for a repeated copy of a chat, which no origin reports.
func (s reportingDaySource) sessionOrigin(session activity.SessionMeta, schema int) string {
	return s.origin(session, schema, s.codexCloudRepeats.activity)
}

// usageOrigin returns the key a usage row's session reports under. Usage
// of a repeated Codex cloud chat has already been narrowed to one copy by
// codexCloudUsage, so that copy reports under its account.
func (s reportingDaySource) usageOrigin(session activity.SessionMeta, schema int) string {
	return s.origin(session, schema, nil)
}

// codexCloudUsage keeps the usage of one copy of each repeated Codex cloud
// chat and reports it under the copy chosen for activity, when this range
// loaded that copy, so the activity and usage sections agree.
func (s reportingDaySource) codexCloudUsage(rows []activity.UsageRow, sessions map[string]activity.SessionMeta) []activity.UsageRow {
	if len(s.codexCloudRepeats.usageOwner) == 0 {
		return rows
	}
	out := make([]activity.UsageRow, 0, len(rows))
	for _, row := range rows {
		owner, repeated := s.codexCloudRepeats.usageOwner[row.SessionID]
		if repeated && owner == "" {
			continue
		}
		if _, loaded := sessions[owner]; repeated && loaded {
			row.SessionID = owner
		}
		out = append(out, row)
	}
	return out
}

func (s reportingDaySource) origin(session activity.SessionMeta, schema int, repeats map[string]bool) string {
	if repeats[session.SessionID] {
		return ""
	}
	if account := s.codexCloudAccounts[session.SessionID]; account != "" {
		return codexCloudAccountPrefix + account
	}
	return s.machineID(session.Machine, schema)
}

func (s reportingDaySource) machineID(machine string, schema int) string {
	machine = machineIdentities{local: s.localMachineID, aliases: s.machineAliases}.origin(machine, s.archiveID)
	if export.IsAccountReportingSchema(schema) && (strings.HasPrefix(machine, cursorAccountPrefix) || strings.HasPrefix(machine, "machine:") ||
		schema == export.ReportingCursorSchemaVersion && strings.HasPrefix(machine, codexCloudAccountPrefix)) {
		digest := sha256.Sum256([]byte(machine))
		return "machine:" + hex.EncodeToString(digest[:])
	}
	return machine
}
