package postgres

import (
	"context"
	"database/sql"

	"go.kenn.io/agentsview/internal/rawparity"
)

// RawParityReader adapts the physical loader without opening another connection.
// The caller validates the runtime pool with CheckHostedTenant and owns Tx for
// the full report. Version is the current parser/rules processing version.
type RawParityReader struct {
	Tx      *sql.Tx
	Version string
}

func (r RawParityReader) Candidates(ctx context.Context) ([]rawparity.Candidate, error) {
	if err := checkRawParityTransaction(ctx, r.Tx); err != nil {
		return nil, err
	}
	return loadRawParityCandidates(ctx, r.Tx)
}

// loadRawParityCandidates shares the physical identity query with caller-owned
// write transactions. The public report retains its read-only snapshot guard.
func loadRawParityCandidates(ctx context.Context, tx *sql.Tx) ([]rawparity.Candidate, error) {
	out := []rawparity.Candidate{}

	rows, err := tx.QueryContext(ctx, `SELECT s.id,s.agent,s.machine,s.raw_group_id,
 CASE WHEN s.provenance_kind='legacy' THEN hosted_legacy_alias(s.id)
 ELSE g.base_alias||COALESCE('~'||b.branch,'') END,
 s.agent='piebald' OR s.source_session_id='',COALESCE(b.aliases,'{}'::text[]),COALESCE(b.devices,'{}'::text[])
 FROM sessions s LEFT JOIN raw_session_groups g ON g.group_id=s.raw_group_id
 LEFT JOIN LATERAL (
  SELECT min(b.branch_id) AS branch,array_agg(DISTINCT b.member_id) AS aliases,array_agg(DISTINCT p.device_id) AS devices
  FROM raw_session_branches b JOIN raw_source_projections p ON p.source_id=b.source_id
  WHERE b.session_id=s.id AND b.active AND NOT COALESCE((SELECT c.value::boolean FROM raw_curation c
   WHERE c.group_id=b.group_id AND c.field='excluded' AND c.branch_id IN ('',b.branch_id)
   ORDER BY c.branch_id DESC LIMIT 1),false)
 ) b ON s.provenance_kind='raw'
 ORDER BY s.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c rawparity.Candidate
		if err = rows.Scan(&c.ID, &c.Agent, &c.Machine, &c.Group, &c.PublicID, &c.SourceLocal, &c.Aliases, &c.Devices); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	err = rows.Err()
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (r RawParityReader) Readiness(ctx context.Context, scope rawparity.Scope) ([]rawparity.Readiness, error) {
	rows, err := LoadRawParityReadiness(ctx, r.Tx, RawParityScope{Device: scope.Device, Provider: scope.Provider}, r.Version)
	out := make([]rawparity.Readiness, 0, len(rows))
	for _, s := range rows {
		out = append(out, rawparity.Readiness{Device: s.Device, Provider: s.Provider, Projected: s.Projected, Pending: s.Pending, Failed: s.Failed, Unsupported: s.Unsupported})
	}
	return out, err
}

func (r RawParityReader) Snapshots(ctx context.Context, ids []string) (map[string]rawparity.Snapshot, error) {
	return LoadRawParitySnapshots(ctx, r.Tx, ids)
}

func (r RawParityReader) Links(ctx context.Context, id string) ([]rawparity.Link, error) {
	return LoadRawParityLinks(ctx, r.Tx, id)
}
