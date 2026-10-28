package archivesource

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

// The catalog term re-registers a directory whose accepted sources changed.
// Unchanged sources keep their completion, so registration reparses nothing.
func attachmentTarget(cfg config.Config, catalog string) (string, error) {
	claude, err := completionTarget(Source{Root: db.RawArchiveRoot{Provider: "claude"}}, cfg)
	if err != nil {
		return "", err
	}
	codex, err := completionTarget(Source{Root: db.RawArchiveRoot{Provider: "codex"}}, cfg)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(claude+codex+catalog))), nil
}

func (i *Index) register(ctx context.Context, database *db.DB, cfg config.Config) error {
	key := db.ArchiveAttachmentKey{RepositoryID: i.Info.RepositoryID, SnapshotID: i.Info.SnapshotID}
	if err := database.RegisterArchiveAttachment(ctx, key, i.ArchiveID); err != nil {
		return err
	}
	target, err := attachmentTarget(cfg, i.catalog)
	if err != nil {
		return err
	}
	status, err := database.ArchiveAttachmentStatus(ctx, key)
	if err != nil {
		return err
	}
	if status.Target == target {
		return i.configureMachineName(ctx, database)
	}
	var targets []db.ArchiveSourceTarget
	after := ""
	for {
		sources, err := i.Sources(ctx, after, 128)
		if err != nil {
			return err
		}
		if len(sources) == 0 {
			break
		}
		for _, source := range sources {
			wanted, err := completionTarget(source, cfg)
			if err != nil {
				return err
			}
			targets = append(targets, db.ArchiveSourceTarget{Provenance: i.provenance(source), Target: wanted, MachineLabel: source.Root.Machine})
			after = source.ManifestID
		}
	}
	if err := database.SetArchiveAttachmentTargets(ctx, key, target, targets); err != nil {
		return err
	}
	return i.configureMachineName(ctx, database)
}

func (i *Index) source(ctx context.Context, manifestID string) (Source, error) {
	var s Source
	err := i.database.QueryRowContext(ctx, `SELECT s.manifest_id,s.root_id,s.source_key,s.original_path,s.canonical_json,
  r.id,r.device_id,r.machine,r.provider,r.original_path,r.configured_root_id
  FROM sources s JOIN roots r ON r.id=s.root_id WHERE s.manifest_id=?`, manifestID).
		Scan(&s.ManifestID, &s.RootID, &s.SourceKey, &s.OriginalPath, &s.CanonicalJSON,
			&s.Root.ID, &s.Root.DeviceID, &s.Root.Machine, &s.Root.Provider, &s.Root.OriginalPath, &s.Root.ConfiguredRootID)
	return s, err
}

// configureMachineName applies the configured label, or restores each
// registered machine's captured label when the setting is omitted.
func (i *Index) configureMachineName(ctx context.Context, database *db.DB) error {
	labels := make(map[string]string)
	if i.machineName == "" {
		var err error
		if labels, err = i.capturedMachineNames(ctx); err != nil {
			return err
		}
	} else {
		var count int
		var id string
		if err := i.database.QueryRowContext(ctx, `SELECT count(DISTINCT device_id),coalesce(min(device_id),'') FROM roots`).Scan(&count, &id); err != nil {
			return err
		}
		if count != 1 {
			return errors.New("machine_name requires a single-machine archive; omit it to keep the captured names")
		}
		labels[id] = i.machineName
	}
	return database.Update(ctx, func(tx *sql.Tx) error {
		for id, label := range labels {
			var attached bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM archive_source_bindings WHERE archive_id=? AND device_id=?)`, i.ArchiveID, id).Scan(&attached); err != nil {
				return err
			}
			if !attached {
				if i.machineName == "" {
					// Supplemental-only machines have no attached sessions to label.
					continue
				}
				return errors.New("archive machine has not been registered")
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO pg_sync_state(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value WHERE value!=excluded.value`, db.MachineLabelKeyPrefix+id, label); err != nil {
				return err
			}
		}
		return nil
	})
}

func (i *Index) capturedMachineNames(ctx context.Context) (map[string]string, error) {
	rows, err := i.database.QueryContext(ctx, `SELECT device_id,min(machine) FROM roots GROUP BY device_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	labels := make(map[string]string)
	for rows.Next() {
		var id, machine string
		if err := rows.Scan(&id, &machine); err != nil {
			return nil, err
		}
		labels[id] = machine
	}
	return labels, rows.Err()
}
