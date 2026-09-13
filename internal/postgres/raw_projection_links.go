package postgres

import (
	"context"
	"database/sql"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/rawderive"
)

// These builders are the shared authority boundary for live relationship
// reads and sealed parity capture. Callers may add stricter materialization
// proof, but cannot redefine exact-source or cohort authority.
func hostedLinkExactAuthoritySQL(alias, ownerSource string) string {
	return `EXISTS(SELECT 1 FROM raw_session_public_aliases a JOIN raw_session_branches b
		ON b.group_id=a.group_id AND b.source_id=` + ownerSource + ` AND (a.anchor_branch='' OR a.anchor_branch=b.branch_id)
		WHERE a.alias_id=` + alias + `)`
}

func hostedLinkCohortAuthoritySQL(targetSource, ownerSource string) string {
	return strings.Join([]string{
		targetSource + `.tenant_id=` + ownerSource + `.tenant_id`,
		targetSource + `.device_id=` + ownerSource + `.device_id`,
		targetSource + `.provider=` + ownerSource + `.provider`,
		targetSource + `.configured_root_id=` + ownerSource + `.configured_root_id`,
	}, " AND ")
}

func readHostedLinkTarget(ctx context.Context, q hostedQuerier, ownerSource, alias string) (rawderive.ParityMemberKey, bool, error) {
	var exactAuthority bool
	if err := q.QueryRowContext(ctx, `SELECT `+hostedLinkExactAuthoritySQL("$1", "$2"), alias, ownerSource).Scan(&exactAuthority); err != nil {
		return rawderive.ParityMemberKey{}, false, err
	}
	query := `SELECT b.session_id,min(b.source_id),min(g.logical_key) FROM raw_session_public_aliases a
		JOIN raw_session_branches b ON b.group_id=a.group_id AND b.source_id=$2 AND (a.anchor_branch='' OR a.anchor_branch=b.branch_id)
		JOIN raw_session_groups g ON g.group_id=b.group_id
		JOIN sessions materialized ON materialized.id=b.session_id AND materialized.provenance_kind='raw'
		 AND materialized.raw_group_id=b.group_id AND materialized.raw_content_revision=b.content_revision
		WHERE a.alias_id=$1 GROUP BY b.session_id ORDER BY b.session_id LIMIT 2`
	if !exactAuthority {
		query = `SELECT ss.physical_session_id,min(b.source_id),min(g.logical_key) FROM raw_source_projections owner_source
		JOIN raw_source_projections target_source ON ` + hostedLinkCohortAuthoritySQL("target_source", "owner_source") + `
		JOIN raw_session_branches b ON b.source_id=target_source.source_id AND b.active
		JOIN raw_session_groups g ON g.group_id=b.group_id
		JOIN raw_session_public_aliases a ON a.group_id=b.group_id AND (a.anchor_branch='' OR a.anchor_branch=b.branch_id)
		JOIN session_sources ss ON ss.branch_id=b.branch_id AND ss.group_id=b.group_id AND ss.source_id=b.source_id
		 AND ss.session_id=b.session_id AND ss.manifest_id=b.manifest_id AND ss.content_revision=b.content_revision
		 AND ss.processing_version=b.processing_version AND ss.projection_generation=b.projection_generation
		JOIN sessions materialized ON materialized.id=ss.physical_session_id AND materialized.provenance_kind='raw'
		 AND materialized.raw_group_id=b.group_id AND materialized.raw_content_revision=b.content_revision
		WHERE owner_source.source_id=$2 AND a.alias_id=$1
		 AND NOT COALESCE((SELECT c.value::boolean FROM raw_curation c WHERE c.group_id=b.group_id AND c.field='excluded'
		  AND c.branch_id IN ('',b.branch_id) ORDER BY c.branch_id DESC LIMIT 1),false)
		GROUP BY ss.physical_session_id ORDER BY ss.physical_session_id LIMIT 2`
	}
	rows, err := q.QueryContext(ctx, query, alias, ownerSource)
	if err != nil {
		return rawderive.ParityMemberKey{}, false, err
	}
	defer rows.Close()
	var target rawderive.ParityMemberKey
	var physical string
	count := 0
	for rows.Next() {
		if err = rows.Scan(&physical, &target.SourceID, &target.LogicalKey); err != nil {
			return rawderive.ParityMemberKey{}, false, err
		}
		count++
	}
	if err = rows.Err(); err != nil {
		return rawderive.ParityMemberKey{}, false, err
	}
	if count != 1 {
		return rawderive.ParityMemberKey{}, count > 1 || exactAuthority, nil
	}
	target.Kind = "session"
	return target, false, nil
}

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
var hostedLinkFromSQL = ` FROM raw_session_links e
 JOIN raw_session_branches owner ON owner.branch_id=e.branch_id AND owner.active
 JOIN sessions own_session ON own_session.id=owner.session_id
 JOIN raw_source_projections owner_source ON owner_source.source_id=owner.source_id
 JOIN LATERAL (
	SELECT ` + hostedLinkExactAuthoritySQL("e.target_alias", "owner.source_id") + ` AS exact
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
		 AND ` + hostedLinkCohortAuthoritySQL("target_source", "owner_source") + `))
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
