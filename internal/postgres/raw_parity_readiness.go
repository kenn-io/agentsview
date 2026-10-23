package postgres

import (
	"context"
	"database/sql"

	"go.kenn.io/agentsview/internal/parser"
)

// RawParityScope selects source heads. Empty fields mean all devices/providers;
// legacy machine names are not device IDs and do not participate in this filter.
type RawParityScope struct {
	Device   string `json:"device"`
	Provider string `json:"provider"`
}

// RawParityReadiness counts each current source head once, independently of how
// many physical sessions it projects or shares with other devices. A source is
// projected only after complete publication of the requested processing version.
// Unsupported means the running binary cannot capture/parse this provider.
type RawParityReadiness struct {
	Device      string `json:"device"`
	Provider    string `json:"provider"`
	Projected   int    `json:"projected"`
	Pending     int    `json:"pending"`
	Failed      int    `json:"failed"`
	Unsupported int    `json:"unsupported"`
}

// LoadRawParityReadiness uses the same validated hosted runtime transaction as
// LoadRawParitySnapshots. It does not schedule jobs or change projection state.
// Results are ordered by device/provider; empty filters select all source heads.
func LoadRawParityReadiness(ctx context.Context, tx *sql.Tx, scope RawParityScope, version string) ([]RawParityReadiness, error) {
	if err := validateRawIngestProcessingVersion(version); err != nil {
		return nil, err
	}
	if err := checkRawParityTransaction(ctx, tx); err != nil {
		return nil, err
	}
	return loadRawParityReadiness(ctx, tx, scope, version)
}

func loadRawParityReadiness(ctx context.Context, tx *sql.Tx, scope RawParityScope, version string) ([]RawParityReadiness, error) {
	if err := validateRawIngestProcessingVersion(version); err != nil {
		return nil, err
	}
	var supported []string
	for _, factory := range parser.ProviderFactories() {
		if factory.Capabilities().RawCapture.Support == parser.CapabilitySupported {
			supported = append(supported, string(factory.Definition().Type))
		}
	}
	rows, err := tx.QueryContext(ctx, `WITH heads AS (
	 SELECT h.device_id,h.provider,
	 CASE WHEN NOT h.provider=ANY($4::text[]) THEN 'unsupported'
	 WHEN j.state='complete' AND p.membership_complete AND p.successful_manifest_id=h.manifest_id THEN 'projected'
	 WHEN j.state='failed' THEN 'failed'
	 ELSE 'pending' END AS state
	 FROM raw_source_heads h
	 LEFT JOIN raw_source_projections p ON p.tenant_id=h.tenant_id AND p.device_id=h.device_id
	  AND p.provider=h.provider AND p.configured_root_id=h.configured_root_id AND p.source_key_sha256=h.source_key_sha256
	  AND p.selected_manifest_id=h.manifest_id AND p.processing_version=$3
	 LEFT JOIN raw_ingest_jobs j ON j.tenant_id=p.tenant_id AND j.id=p.selected_job_id
	  AND j.manifest_id=h.manifest_id AND j.processing_version=$3 AND j.stage='parse'
	  AND j.projection_generation=p.projection_generation AND j.projection_selected
	 WHERE ($1='' OR h.device_id=$1) AND ($2='' OR h.provider=$2)
	)
	SELECT device_id,provider,count(*) FILTER(WHERE state='projected'),count(*) FILTER(WHERE state='pending'),
	 count(*) FILTER(WHERE state='failed'),count(*) FILTER(WHERE state='unsupported')
	FROM heads GROUP BY device_id,provider ORDER BY device_id,provider`, scope.Device, scope.Provider, version, supported)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RawParityReadiness{}
	for rows.Next() {
		var r RawParityReadiness
		if err := rows.Scan(&r.Device, &r.Provider, &r.Projected, &r.Pending, &r.Failed, &r.Unsupported); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
