package postgres

import (
	"context"
	"encoding/json/v2"
	"errors"

	"go.kenn.io/agentsview/internal/rawsync"
)

// One complete manifest bounds a tick to DefaultManifestLimits().MaxObjects
// (16,384) references and MaxCanonicalBytes. Never split a manifest across ticks.
const rawCustodyManifestBatchSize = 1

// AuditRawCustody is detection only. Call inside the serving process with its
// already-open repository; the SQL-only CLI must never acquire that repository.
func (s *RawProjectionStore) AuditRawCustody(ctx context.Context, objects rawsync.ObjectStore) (RawReconcileBatch, error) {
	var result RawReconcileBatch
	probe, ok := objects.(rawsync.CustodyProber)
	if objects == nil || !ok {
		return result, rawsync.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()
	locked, err := s.lockRawMaintenanceTx(ctx, tx)
	if err != nil {
		return result, err
	}
	if !locked {
		result.Busy = true
		return result, nil
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO raw_maintenance_state(check_name) VALUES('custody') ON CONFLICT DO NOTHING`)
	if err != nil {
		return result, err
	}
	var encoded []byte
	err = tx.QueryRowContext(ctx, `SELECT cursor FROM raw_maintenance_state WHERE tenant_id=$1 AND check_name='custody' FOR UPDATE`, s.options.Tenant).Scan(&encoded)
	if err != nil {
		return result, err
	}
	var cursor []string
	if err = json.Unmarshal(encoded, &cursor); err != nil {
		return result, err
	}
	if len(cursor) != 4 {
		return result, rawsync.ErrInvalid
	}
	rows, err := tx.QueryContext(ctx, `SELECT manifest_id,device_id,canonical_json FROM raw_manifests WHERE tenant_id=$1 AND manifest_id>$2 ORDER BY manifest_id LIMIT $3`, s.options.Tenant, cursor[0], rawCustodyManifestBatchSize)
	if err != nil {
		return result, err
	}
	var manifests []rawsync.CanonicalManifest
	for rows.Next() {
		var id, device string
		var data []byte
		if err = rows.Scan(&id, &device, &data); err != nil {
			rows.Close()
			return result, err
		}
		m, parseErr := rawsync.ParseCanonicalManifest(rawsync.AuthIdentity{TenantID: s.options.Tenant, DeviceID: device}, id, data, rawsync.DefaultManifestLimits())
		if parseErr != nil {
			rows.Close()
			return result, parseErr
		}
		manifests = append(manifests, m)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	for _, m := range manifests {
		check := func(kind string, ref rawsync.ObjectRef) error {
			stat := func() error {
				if kind == "object" {
					return probe.ProbeObject(ctx, s.options.Tenant, ref)
				}
				return probe.ProbeManifest(ctx, m.Identity, m.ManifestID, ref.Length)
			}
			err := stat()
			if err == nil {
				return nil
			}
			if !errors.Is(err, rawsync.ErrCustodyUnavailable) {
				return err
			}
			var exists bool
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM raw_manifests WHERE tenant_id=$1 AND manifest_id=$2)`, s.options.Tenant, m.ManifestID).Scan(&exists); err != nil || !exists {
				return err
			}
			err = stat()
			if err == nil {
				return nil
			}
			if !errors.Is(err, rawsync.ErrCustodyUnavailable) {
				return err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO raw_custody_findings(manifest_id,kind,key,size) VALUES($1,$2,$3,$4) ON CONFLICT(manifest_id,kind,key,size) DO UPDATE SET last_seen=clock_timestamp()`, m.ManifestID, kind, ref.SHA256, ref.Length)
			if err == nil {
				result.Found++
			}
			return err
		}
		if err = check("manifest", rawsync.ObjectRef{SHA256: m.ManifestID, Length: int64(len(m.CanonicalJSON))}); err != nil {
			return result, err
		}
		for _, ref := range m.Objects {
			if err = check("object", ref); err != nil {
				return result, err
			}
		}
		result.Scanned++
		cursor[0] = m.ManifestID
	}
	result.Wrapped = len(manifests) < rawCustodyManifestBatchSize
	if result.Wrapped {
		cursor[0] = ""
	}
	encoded, err = json.Marshal(cursor)
	if err != nil {
		return result, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE raw_maintenance_state SET cursor=$2,last_run_at=clock_timestamp(),found=$3 WHERE tenant_id=$1 AND check_name='custody'`, s.options.Tenant, encoded, result.Found)
	if err != nil {
		return result, err
	}
	return result, tx.Commit()
}
