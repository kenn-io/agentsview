package postgres

import (
	"context"
	"database/sql"
)

// Owner list and runtime status use the same caller-owned snapshot. In
// particular, runtime status must not call the owner-only ListRawRollout API or
// start another transaction. Counts are physical and shared groups may appear
// in several scopes; unresolved pins are computed from original retained pins.
func loadRawRolloutDiagnostics(ctx context.Context, tx *sql.Tx, device, provider string) ([]RawRolloutScope, error) {
	scopes, err := loadRawRolloutScopes(ctx, tx)
	if err != nil {
		return nil, err
	}
	selected := []RawRolloutScope{}
	unresolved := map[string]int64{}
	for _, scope := range scopes {
		if device != "" && scope.DeviceID != device || provider != "" && scope.Provider != provider {
			continue
		}
		err = tx.QueryRowContext(ctx, `SELECT count(*) FILTER(WHERE NOT s.rollout_hidden),count(*) FILTER(WHERE s.rollout_hidden) FROM sessions s WHERE s.provenance_kind='raw' AND EXISTS(SELECT 1 FROM raw_session_branches b JOIN raw_source_projections p ON p.source_id=b.source_id WHERE b.group_id=s.raw_group_id AND b.active AND ($1='' OR p.device_id=$1) AND ($2='' OR p.provider=$2))`, scope.DeviceID, scope.Provider).Scan(&scope.Served, &scope.Shadow)
		if err != nil {
			return nil, err
		}
		rows, err := tx.QueryContext(ctx, `SELECT m.legacy_session_id,m.group_id,EXISTS(SELECT 1 FROM pinned_messages pin WHERE pin.session_id=m.legacy_session_id) FROM raw_legacy_supersessions m WHERE EXISTS(SELECT 1 FROM raw_session_branches b JOIN raw_source_projections p ON p.source_id=b.source_id WHERE b.group_id=m.group_id AND b.active AND ($1='' OR p.device_id=$1) AND ($2='' OR p.provider=$2)) ORDER BY m.legacy_session_id`, scope.DeviceID, scope.Provider)
		if err != nil {
			return nil, err
		}
		type mapping struct {
			legacy, group string
			pins          bool
		}
		var mapped []mapping
		for rows.Next() {
			var m mapping
			if err = rows.Scan(&m.legacy, &m.group, &m.pins); err != nil {
				break
			}
			mapped = append(mapped, m)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return nil, err
		}
		scope.Superseded = int64(len(mapped))
		for _, m := range mapped {
			if !m.pins {
				continue
			}
			count, ok := unresolved[m.legacy]
			if !ok {
				carry, err := prepareRawLegacyCarry(ctx, tx, m.legacy, m.group)
				if err != nil {
					return nil, err
				}
				count = int64(carry.UnresolvedPins)
				unresolved[m.legacy] = count
			}
			scope.UnresolvedPins += count
		}
		selected = append(selected, scope)
	}
	return selected, nil
}
