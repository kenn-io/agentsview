package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/uptrace/bun"
)

const archiveMachineKeysSQL = `SELECT machine FROM sessions
	UNION SELECT machine FROM source_worktree_project_mappings
	UNION SELECT machine FROM source_project_identity_observations
	UNION SELECT machine FROM source_session_project_identity_snapshots
	UNION SELECT machine FROM local_session_source_baselines`

type MachineIdentityCandidate struct {
	Machine       string
	Sessions      int
	WorktreeRules int
}

// ListMachineIdentityCandidates includes orphaned and trashed sessions and
// machines retained only in rules, so ownership can be selected before startup.
func (db *DB) ListMachineIdentityCandidates(ctx context.Context) ([]MachineIdentityCandidate, error) {
	rows, err := db.getReader().QueryContext(ctx, `SELECT machines.machine,
		(SELECT count(*) FROM sessions WHERE machine = machines.machine),
		(SELECT count(*) FROM source_worktree_project_mappings WHERE machine = machines.machine)
		FROM (`+archiveMachineKeysSQL+`) machines ORDER BY machines.machine`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var candidates []MachineIdentityCandidate
	for rows.Next() {
		var candidate MachineIdentityCandidate
		if err := rows.Scan(&candidate.Machine, &candidate.Sessions, &candidate.WorktreeRules); err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}

// EnsureInstallationIdentity adopts the archive's recorded local owner once.
// Archives that predate recorded ownership keep their named machines in place;
// the returned keys are left for an explicit AdoptMachineIdentity decision.
// The installation record is independent of this migration: replacing it starts
// a new installation and must not silently claim the previous one's sessions.
func (db *DB) EnsureInstallationIdentity(ctx context.Context, identity string) ([]string, error) {
	var unowned []string
	err := db.adoptMachineIdentity(ctx, identity, nil, false, &unowned)
	return unowned, err
}

// AdoptMachineIdentity records an operator's explicit ownership selection.
// Callers must hold the archive write-owner lock, with ingestion stopped.
func (db *DB) AdoptMachineIdentity(ctx context.Context, identity string, machines []string) error {
	return db.adoptMachineIdentity(ctx, identity, machines, true, nil)
}

func (db *DB) adoptMachineIdentity(
	ctx context.Context, identity string, machines []string, explicit bool, unowned *[]string,
) error {
	if strings.TrimSpace(identity) == "" || identity == "local" {
		return errors.New("installation identity is required")
	}
	return db.Update(func(tx bun.Tx) error {
		if err := lockArtifactPublicationTx(ctx, tx); err != nil {
			return err
		}
		var installed, former string
		if err := tx.QueryRowContext(ctx, `SELECT
			COALESCE((SELECT value FROM pg_sync_state WHERE key = 'artifact_local_installation_id'), ''),
			COALESCE((SELECT value FROM pg_sync_state WHERE key = 'artifact_local_machine_name'), '')`).Scan(&installed, &former); err != nil {
			return fmt.Errorf("reading installation adoption state: %w", err)
		}
		if !explicit && installed != "" {
			return configureArtifactLocalMachineTx(ctx, tx, identity)
		}
		if !explicit && former == "" {
			keys, err := unownedMachineKeysTx(ctx, tx, identity)
			if err != nil {
				return err
			}
			*unowned = keys
		}
		if explicit {
			for _, machine := range machines {
				var known bool
				if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM (`+archiveMachineKeysSQL+`)
					WHERE machine = ?) OR EXISTS (SELECT 1 FROM pg_sync_state WHERE key = ?)`, machine, MachineAliasKeyPrefix+machine).Scan(&known); err != nil {
					return err
				}
				if strings.TrimSpace(machine) == "" || (!known && machine != former) {
					return fmt.Errorf("machine %q is not recorded in this archive", machine)
				}
			}
		}
		machines = slices.Clone(machines)
		if !explicit && former != "" {
			machines = append(machines, former)
		}
		// The local sentinel has always denoted this archive's own ingestion.
		machines = append(machines, "local", "")
		slices.Sort(machines)
		machines = slices.Compact(machines)
		// Validate the whole selection before flattening aliases. A previous ID
		// and its aliases can be explicitly adopted together in any order.
		for _, machine := range machines {
			var existing string
			err := tx.QueryRowContext(ctx, `SELECT value FROM pg_sync_state WHERE key = ?`, MachineAliasKeyPrefix+machine).Scan(&existing)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if existing != "" && existing != identity && !slices.Contains(machines, existing) {
				return fmt.Errorf("machine %q is already adopted by installation %q; include that installation ID to adopt its history too", machine, existing)
			}
		}
		for _, machine := range machines {
			if machine == identity {
				continue
			}
			if err := adoptMachineRowsTx(ctx, tx, machine, identity); err != nil {
				return err
			}
		}
		// Old hostnames remain filter redirects, not future export authority.
		if _, err := tx.ExecContext(ctx, `DELETE FROM pg_sync_state WHERE key = 'artifact_local_machine_name'`); err != nil {
			return err
		}
		return configureArtifactLocalMachineTx(ctx, tx, identity)
	})
}

// unownedMachineKeysTx lists named machines that no recorded owner explains.
func unownedMachineKeysTx(ctx context.Context, tx bun.Tx, identity string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT machine FROM (`+archiveMachineKeysSQL+`)
		WHERE machine NOT IN ('', 'local', ?) ORDER BY machine`, identity)
	if err != nil {
		return nil, fmt.Errorf("checking archive ownership: %w", err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func adoptMachineRowsTx(ctx context.Context, tx bun.Tx, machine, identity string) error {
	var conflict string
	err := tx.QueryRowContext(ctx, `
		SELECT old.path_prefix FROM source_worktree_project_mappings old
		JOIN source_worktree_project_mappings target ON target.source_archive_id = old.source_archive_id
			AND target.machine = ? AND target.path_prefix = old.path_prefix
		WHERE old.machine = ? AND (old.project != target.project OR old.layout != target.layout
			OR old.enabled != target.enabled OR old.original_project != target.original_project)
		LIMIT 1`, identity, machine).Scan(&conflict)
	if err == nil {
		return fmt.Errorf("conflicting worktree rules for %q on %q and %q; reconcile these rules before adopting the machine", conflict, machine, identity)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM source_worktree_project_mappings
		WHERE machine = ? AND (source_archive_id, path_prefix) IN (
			SELECT source_archive_id, path_prefix FROM source_worktree_project_mappings WHERE machine = ?)`, machine, identity); err != nil {
		return err
	}
	// Aggregates retain the newest observation of a root. Individual session
	// snapshots remain intact, including when two former names shared a root.
	// Canonical observations use UTC microsecond timestamps. Compare within
	// each source archive so distinct archives retain their own observations.
	// Strip Z so whole seconds sort before fractional timestamps.
	for _, pair := range [][2]string{{machine, identity}, {identity, machine}} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM source_project_identity_observations AS old
			WHERE old.machine = ? AND EXISTS (
				SELECT 1 FROM source_project_identity_observations target
				WHERE target.source_archive_id = old.source_archive_id
					AND target.machine = ? AND target.project = old.project
					AND target.root_path = old.root_path AND target.git_remote = old.git_remote
					AND rtrim(target.observed_at, 'Z') >= rtrim(old.observed_at, 'Z')
			)`, pair[0], pair[1]); err != nil {
			return err
		}
	}
	for _, table := range []string{
		"source_worktree_project_mappings", "source_project_identity_observations",
		"source_session_project_identity_snapshots", "local_session_source_baselines",
	} {
		if _, err := tx.ExecContext(ctx, "UPDATE "+table+" SET machine = ? WHERE machine = ?", identity, machine); err != nil {
			return fmt.Errorf("adopting %s: %w", table, err)
		}
	}
	// Keep session IDs and content untouched. Advancing the normal write marker
	// makes both mirrors publish the changed machine, even for orphaned history.
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET machine = ?,
		local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE machine = ?`, identity, machine); err != nil {
		return fmt.Errorf("adopting sessions: %w", err)
	}
	if machine == "" || machine == "local" {
		return nil // A shared mirror cannot redirect every archive's local sentinel.
	}
	// Explicitly adopting a previous installation also moves its proven aliases;
	// keep redirects direct so readers never need chain or cycle resolution.
	if _, err := tx.ExecContext(ctx, `UPDATE pg_sync_state SET value = ?
		WHERE key LIKE 'machine\_alias:%' ESCAPE '\' AND value = ?`, identity, machine); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO pg_sync_state(key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, MachineAliasKeyPrefix+machine, identity)
	return err
}
