package postgres

import (
	"context"
	"database/sql"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
)

const rawLinksDDL = `CREATE TABLE IF NOT EXISTS raw_session_links (
 branch_id TEXT NOT NULL,kind TEXT NOT NULL,ordinal INTEGER NOT NULL,call_index INTEGER NOT NULL,event_index INTEGER NOT NULL,target_alias TEXT NOT NULL,
 PRIMARY KEY(branch_id,kind,ordinal,call_index,event_index)
);`

// Only source-owned typed relationships are recorded. Missing source proof
// cannot redirect a link to a different device's equally named session.
func writeRawLinks(ctx context.Context, tx *sql.Tx, branch string, p ingest.PreparedSession) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM raw_session_links WHERE branch_id=$1`, branch); err != nil {
		return err
	}
	add := func(kind string, ordinal, call, event int, target string) error {
		if target == "" {
			return nil
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO raw_session_links(branch_id,kind,ordinal,call_index,event_index,target_alias) VALUES($1,$2,$3,$4,$5,$6)`, branch, kind, ordinal, call, event, target)
		return err
	}
	if p.Session.ParentSessionID != nil {
		if err := add("parent", -1, -1, -1, *p.Session.ParentSessionID); err != nil {
			return err
		}
	}
	for _, m := range p.Messages {
		for ci, c := range m.ToolCalls {
			if err := add("call", m.Ordinal, ci, -1, c.SubagentSessionID); err != nil {
				return err
			}
			for ei, e := range c.ResultEvents {
				if err := add("event", m.Ordinal, ci, ei, e.SubagentSessionID); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// Exact-source authority survives removal and exclusion. Only an edge without
// that historical authority may use a unique materialized content cohort from
// the same immutable capture scope. Count per owner edge before merging owners:
// equal proofs coalesce, while independently proven plural parents survive.
const hostedLinkFromSQL = ` FROM raw_session_links e
 JOIN raw_session_branches owner ON owner.branch_id=e.branch_id AND owner.active
 JOIN sessions own_session ON own_session.id=owner.session_id
 JOIN raw_source_projections owner_source ON owner_source.source_id=owner.source_id
 JOIN LATERAL (
   SELECT EXISTS (
     SELECT 1 FROM raw_session_public_aliases a
     JOIN raw_session_branches b ON b.group_id=a.group_id AND b.source_id=owner.source_id
       AND (a.anchor_branch='' OR a.anchor_branch=b.branch_id)
     WHERE a.alias_id=e.target_alias
   ) AS exact
 ) authority ON true
 JOIN LATERAL (
   SELECT candidates.session_id,count(*) OVER () AS cohort_count FROM (
     SELECT DISTINCT b.session_id
     FROM raw_session_public_aliases a
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
 JOIN sessions target_session ON target_session.id=target.session_id
 WHERE NOT COALESCE((SELECT c.value::boolean FROM raw_curation c WHERE c.group_id=owner.group_id AND c.field='excluded' AND c.branch_id IN ('',owner.branch_id) ORDER BY c.branch_id DESC LIMIT 1),false) `

func (s *Store) sessionDialect() db.QueryDialect {
	d := db.PostgresQueryDialect()
	if !s.hostedRelations {
		return d
	}
	return d.WithParentRelation(func(child, parent string) string {
		return "((" + child + ".provenance_kind='legacy' AND " + child + ".parent_session_id=" + parent + ".id) OR (" + child + ".provenance_kind='raw' AND EXISTS(SELECT 1 " + hostedLinkFromSQL + " AND e.kind='parent' AND owner.session_id=" + child + ".id AND target.session_id=" + parent + ".id)))"
	})
}
