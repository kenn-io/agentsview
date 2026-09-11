package postgres

import (
	"context"
	"database/sql"
	"errors"
	"go.kenn.io/agentsview/internal/db"
	"slices"
)

// The legacy variant uses a separate namespace and never exposes a raw physical
// row as lookup authority. A retained archive row has no raw rebuild provenance.
const hostedLegacyAliasSQL = `hosted_legacy_alias(id)`

type hostedIdentity struct {
	RawIdentity
	Legacy bool
}

func (h *HostedStore) resolve(ctx context.Context, alias string) (hostedIdentity, error) {
	raw, err := h.core.Resolve(ctx, alias)
	if err != nil {
		return hostedIdentity{}, err
	}
	var legacy string
	err = h.physical.pg.QueryRowContext(ctx, `SELECT id FROM sessions WHERE provenance_kind='legacy' AND (id=$1 OR `+hostedLegacyAliasSQL+`=$1)`, alias).Scan(&legacy)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return hostedIdentity{}, err
	}
	if legacy != "" {
		if raw.State != RawIdentityGone {
			var variant string
			err = h.physical.pg.QueryRowContext(ctx, `SELECT `+hostedLegacyAliasSQL+` FROM sessions WHERE id=$1`, legacy).Scan(&variant)
			if err != nil {
				return hostedIdentity{}, err
			}
			return hostedIdentity{}, &db.SessionIdentityError{State: "ambiguous", Variants: append(raw.Variants, variant)}
		}
		canonical := legacy
		refs := hostedRefs{}
		refs.add(&canonical)
		if err = refs.mapIDs(ctx, h); err != nil {
			return hostedIdentity{}, err
		}
		return hostedIdentity{State: RawIdentityUnique, SessionID: legacy, PublicID: canonical, Legacy: true}, nil
	}
	if raw.State == RawIdentityAmbiguous {
		return hostedIdentity{}, &db.SessionIdentityError{State: "ambiguous", Variants: raw.Variants}
	}
	if raw.State == RawIdentityUnique {
		canonical := raw.SessionID
		r := hostedRefs{}
		r.add(&canonical)
		if err = r.mapIDs(ctx, h); err != nil {
			return hostedIdentity{}, err
		}
		raw.PublicID = canonical
	}
	return hostedIdentity{RawIdentity: raw}, nil
}

// hostedRefs collects only declared relational fields. Pointers always belong to
// a result-owned copy; raw JSON, provider IDs and source text never enter here.
type hostedLink struct {
	owner, alias string
	field        *string
	optional     **string
	plural       *[]string
}
type hostedRefs struct {
	fields []*string
	links  []hostedLink
}

func (r *hostedRefs) link(owner string, p *string) {
	if p != nil && *p != "" {
		r.links = append(r.links, hostedLink{owner: owner, alias: *p, field: p})
	}
}
func (r *hostedRefs) parent(owner string, p **string, plural *[]string) {
	if *p != nil && **p != "" {
		r.links = append(r.links, hostedLink{owner: owner, alias: **p, optional: p, plural: plural})
	}
}

func (r *hostedRefs) add(p *string) {
	if p != nil && *p != "" {
		r.fields = append(r.fields, p)
	}
}
func (r *hostedRefs) mapIDs(ctx context.Context, h *HostedStore) error {
	edges := map[string][]string{}
	owners := make([]string, 0, len(r.links))
	for _, l := range r.links {
		owners = append(owners, l.owner)
	}
	slices.Sort(owners)
	owners = slices.Compact(owners)
	if len(owners) > 0 {
		rows, err := h.physical.pg.QueryContext(ctx, `SELECT DISTINCT owner.session_id,e.target_alias,target.session_id `+hostedLinkFromSQL+` AND owner.session_id=ANY($1)`, owners)
		if err != nil {
			return err
		}
		for rows.Next() {
			var owner, alias, target string
			if err = rows.Scan(&owner, &alias, &target); err != nil {
				rows.Close()
				return err
			}
			key := owner + "\x00" + alias
			edges[key] = append(edges[key], target)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	ids := make([]string, 0, len(r.fields))
	for _, p := range r.fields {
		ids = append(ids, *p)
	}
	for _, l := range r.links {
		ids = append(ids, l.owner, l.alias)
		ids = append(ids, edges[l.owner+"\x00"+l.alias]...)
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if len(ids) == 0 {
		return nil
	}
	rows, err := h.physical.pg.QueryContext(ctx, `
 WITH targets AS (SELECT id,provenance_kind,raw_group_id FROM sessions WHERE id=ANY($1)),
 groups AS (SELECT DISTINCT raw_group_id AS group_id FROM targets WHERE raw_group_id<>''),
 cohorts AS (SELECT s.raw_group_id AS group_id,count(*) AS n FROM sessions s JOIN groups g ON g.group_id=s.raw_group_id GROUP BY s.raw_group_id),
 representatives AS (SELECT b.session_id,min(b.branch_id) AS branch FROM raw_session_branches b JOIN groups g ON g.group_id=b.group_id WHERE b.active AND NOT COALESCE((SELECT c.value::boolean FROM raw_curation c WHERE c.group_id=b.group_id AND c.field='excluded' AND c.branch_id IN ('',b.branch_id) ORDER BY c.branch_id DESC LIMIT 1),false) GROUP BY b.session_id)
 SELECT t.id,t.provenance_kind, CASE WHEN t.provenance_kind='legacy' THEN
  CASE WHEN EXISTS(SELECT 1 FROM raw_session_groups g JOIN sessions s ON s.raw_group_id=g.group_id WHERE g.base_alias=t.id) THEN hosted_legacy_alias(t.id) ELSE t.id END
 ELSE CASE WHEN c.n>1 OR EXISTS(SELECT 1 FROM sessions l WHERE l.id=g.base_alias AND l.provenance_kind='legacy') OR (SELECT count(DISTINCT a.group_id) FROM raw_session_public_aliases a JOIN sessions s ON s.raw_group_id=a.group_id WHERE a.alias_id=g.base_alias)>1 THEN g.base_alias||'~'||r.branch ELSE g.base_alias END END
 FROM targets t LEFT JOIN raw_session_groups g ON g.group_id=t.raw_group_id LEFT JOIN cohorts c ON c.group_id=g.group_id LEFT JOIN representatives r ON r.session_id=t.id`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	mapped := map[string]string{}
	kinds := map[string]string{}
	for rows.Next() {
		var id, kind string
		var alias sql.NullString
		if err = rows.Scan(&id, &kind, &alias); err != nil {
			return err
		}
		mapped[id] = alias.String
		kinds[id] = kind
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for _, p := range r.fields {
		*p = mapped[*p]
	}
	for _, l := range r.links {
		targets := edges[l.owner+"\x00"+l.alias]
		if kinds[l.owner] == "legacy" {
			targets = []string{l.alias}
		}
		variants := make([]string, 0, len(targets))
		for _, id := range targets {
			if mapped[id] != "" {
				variants = append(variants, mapped[id])
			}
		}
		slices.Sort(variants)
		variants = slices.Compact(variants)
		if l.plural != nil {
			*l.plural = nil
			if len(variants) > 1 {
				*l.plural = variants
			}
		}
		if len(variants) > 1 && l.plural == nil {
			return &db.SessionIdentityError{State: "ambiguous", Variants: variants}
		}
		value := ""
		if len(variants) == 1 {
			value = variants[0]
		}
		if l.field != nil {
			*l.field = value
		}
		if l.optional != nil {
			*l.optional = nil
			if value != "" {
				*l.optional = &value
			}
		}
	}
	return nil
}
func (r *hostedRefs) session(s *db.Session) {
	r.add(&s.ID)
	r.parent(s.ID, &s.ParentSessionID, &s.ParentSessionIDs)
	s.ParserParentSessionID = nil
}
func (r *hostedRefs) messages(msgs []db.Message) []db.Message {
	msgs = slices.Clone(msgs)
	for i := range msgs {
		m := &msgs[i]
		r.add(&m.SessionID)
		m.ToolCalls = slices.Clone(m.ToolCalls)
		for j := range m.ToolCalls {
			c := &m.ToolCalls[j]
			r.add(&c.SessionID)
			r.link(m.SessionID, &c.SubagentSessionID)
			c.ResultEvents = slices.Clone(c.ResultEvents)
			for k := range c.ResultEvents {
				r.link(m.SessionID, &c.ResultEvents[k].SubagentSessionID)
			}
		}
	}
	return msgs
}

// resolveBatch resolves collection inputs in one query. Singular curation keeps
// the richer core resolver and its transaction/group-lock scope.
func (h *HostedStore) resolveBatch(ctx context.Context, aliases []string, exclude, strict bool) ([]string, error) {
	if len(aliases) == 0 {
		return nil, nil
	}
	rows, err := h.physical.pg.QueryContext(ctx, `SELECT DISTINCT a.alias_id,b.session_id FROM raw_session_public_aliases a JOIN raw_session_branches b ON b.group_id=a.group_id AND b.active AND (a.anchor_branch='' OR a.anchor_branch=b.branch_id) JOIN sessions s ON s.id=b.session_id WHERE a.alias_id=ANY($1) AND NOT COALESCE((SELECT c.value::boolean FROM raw_curation c WHERE c.group_id=b.group_id AND c.field='excluded' AND c.branch_id IN ('',b.branch_id) ORDER BY c.branch_id DESC LIMIT 1),false)
 UNION SELECT requested.alias,id FROM unnest($1::text[]) requested(alias) JOIN sessions s ON (s.id=requested.alias OR hosted_legacy_alias(s.id)=requested.alias) WHERE s.provenance_kind='legacy'`, aliases)
	if err != nil {
		return nil, err
	}
	targets := map[string][]string{}
	for rows.Next() {
		var alias, id string
		if err = rows.Scan(&alias, &id); err != nil {
			rows.Close()
			return nil, err
		}
		targets[alias] = append(targets[alias], id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, alias := range aliases {
		matches := targets[alias]
		if len(matches) == 0 && strict {
			return nil, &db.SessionIdentityError{State: "gone"}
		}
		if len(matches) > 1 && !exclude {
			r := hostedRefs{}
			for i := range matches {
				r.add(&matches[i])
			}
			if err = r.mapIDs(ctx, h); err != nil {
				return nil, err
			}
			slices.Sort(matches)
			return nil, &db.SessionIdentityError{State: "ambiguous", Variants: matches}
		}
		ids = append(ids, matches...)
	}
	return ids, nil
}
