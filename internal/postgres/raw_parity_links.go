package postgres

import (
	"context"
	"database/sql"

	"go.kenn.io/agentsview/internal/rawparity"
)

// LoadRawParityLinks resolves each owner edge before merging shared owners. Exact-source
// historical authority prevents fallback after removal/exclusion; otherwise only
// one physical cohort in the same immutable capture scope is admissible.
// Unlike hostedLinkFromSQL, this query retains unresolved edges and never uses
// public served visibility. Hidden and trashed physical targets remain evidence.
// The caller validates the runtime pool with CheckHostedTenant and reuses this
// REPEATABLE READ READ ONLY transaction for all content and readiness reads.
func LoadRawParityLinks(ctx context.Context, tx *sql.Tx, id string) ([]rawparity.Link, error) {
	if err := checkRawParityTransaction(ctx, tx); err != nil {
		return nil, err
	}
	return loadRawParityLinks(ctx, tx, id)
}

func loadRawParityLinks(ctx context.Context, tx *sql.Tx, id string) ([]rawparity.Link, error) {
	rows, err := tx.QueryContext(ctx, `SELECT e.kind,e.ordinal,e.call_index,e.event_index,
 COALESCE(array_agg(DISTINCT target.session_id ORDER BY target.session_id)
  FILTER(WHERE target.session_id IS NOT NULL),'{}'::text[]),bool_or(target.session_id IS NULL)
 FROM raw_session_links e
 JOIN raw_session_branches owner ON owner.branch_id=e.branch_id AND owner.active
 JOIN raw_source_projections owner_source ON owner_source.source_id=owner.source_id
 JOIN LATERAL (
  SELECT EXISTS (
   SELECT 1 FROM raw_session_public_aliases a
   JOIN raw_session_branches b ON b.group_id=a.group_id AND b.source_id=owner.source_id
    AND (a.anchor_branch='' OR a.anchor_branch=b.branch_id)
   WHERE a.alias_id=e.target_alias
  ) AS exact
 ) authority ON true
 LEFT JOIN LATERAL (
  SELECT candidates.session_id,count(*) OVER () AS cohort_count FROM (
   SELECT DISTINCT b.session_id FROM raw_session_public_aliases a
   JOIN raw_session_branches b ON b.group_id=a.group_id AND b.active
    AND (a.anchor_branch='' OR a.anchor_branch=b.branch_id)
   JOIN raw_source_projections target_source ON target_source.source_id=b.source_id
   JOIN sessions materialized ON materialized.id=b.session_id
   WHERE a.alias_id=e.target_alias
    AND (b.source_id=owner.source_id OR (NOT authority.exact
     AND target_source.tenant_id=owner_source.tenant_id
     AND target_source.device_id=owner_source.device_id
     AND target_source.provider=owner_source.provider
     AND target_source.configured_root_id=owner_source.configured_root_id))
    AND NOT COALESCE((SELECT c.value::boolean FROM raw_curation c
     WHERE c.group_id=b.group_id AND c.field='excluded' AND c.branch_id IN ('',b.branch_id)
     ORDER BY c.branch_id DESC LIMIT 1),false)
  ) candidates
 ) target ON authority.exact OR target.cohort_count=1
 WHERE owner.session_id=$1 AND e.kind IN ('parent','call','event')
  AND NOT COALESCE((SELECT c.value::boolean FROM raw_curation c
   WHERE c.group_id=owner.group_id AND c.field='excluded' AND c.branch_id IN ('',owner.branch_id)
   ORDER BY c.branch_id DESC LIMIT 1),false)
 GROUP BY e.kind,e.ordinal,e.call_index,e.event_index
 ORDER BY e.kind,e.ordinal,e.call_index,e.event_index`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	links := []rawparity.Link{}
	for rows.Next() {
		var link rawparity.Link
		if err := rows.Scan(&link.Kind, &link.Ordinal, &link.CallIndex, &link.EventIndex, &link.Targets, &link.Missing); err != nil {
			return nil, err
		}
		links = append(links, link)
	}
	return links, rows.Err()
}
