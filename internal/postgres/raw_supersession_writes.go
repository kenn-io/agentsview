package postgres

import (
	"context"
	"database/sql"
	"slices"

	"github.com/jackc/pgx/v5/pgconn"
)

// Ordinary writers hold rollout shared. Their identity closure starts at the
// changed groups/aliases and follows legacy canonical/public aliases, existing
// maps and collision groups. It does not take every tenant group lock on each
// projection. Discovery and matching both stay within this complete closure.
type rawDerivedLocks struct{ Groups, Aliases, Legacy []string }

func canonicalRawStrings(values []string) []string {
	values = append([]string{}, values...)
	slices.Sort(values)
	return slices.Compact(values)
}
func rawDerivedRetry() error {
	return &pgconn.PgError{Code: "40001", Message: "raw derived identity changed during preparation; retry transaction"}
}

func discoverRawDerived(ctx context.Context, tx *sql.Tx, groups, aliases []string) (rawDerivedLocks, error) {
	p := rawDerivedLocks{Groups: canonicalRawStrings(groups), Aliases: canonicalRawStrings(aliases), Legacy: []string{}}
	for {
		before := p
		found, err := rawStringRows(ctx, tx, `SELECT alias_id FROM raw_session_public_aliases WHERE group_id=ANY($1) ORDER BY alias_id`, p.Groups)
		if err != nil {
			return p, err
		}
		p.Aliases = canonicalRawStrings(append(p.Aliases, found...))
		found, err = rawStringRows(ctx, tx, `SELECT id FROM sessions WHERE provenance_kind='legacy' AND id=ANY($1) UNION SELECT id FROM sessions WHERE provenance_kind='legacy' AND hosted_legacy_alias(id)=ANY($1) UNION SELECT legacy_session_id FROM raw_legacy_supersessions WHERE group_id=ANY($2) ORDER BY 1`, p.Aliases, p.Groups)
		if err != nil {
			return p, err
		}
		p.Legacy = canonicalRawStrings(append(p.Legacy, found...))
		for _, id := range p.Legacy {
			p.Aliases = append(p.Aliases, id, legacyPublicVariant(id))
		}
		p.Aliases = canonicalRawStrings(p.Aliases)
		found, err = rawStringRows(ctx, tx, `SELECT group_id FROM raw_session_public_aliases WHERE alias_id=ANY($1) UNION SELECT group_id FROM raw_legacy_supersessions WHERE legacy_session_id=ANY($2) ORDER BY 1`, p.Aliases, p.Legacy)
		if err != nil {
			return p, err
		}
		p.Groups = canonicalRawStrings(append(p.Groups, found...))
		if slices.Equal(before.Groups, p.Groups) && slices.Equal(before.Aliases, p.Aliases) && slices.Equal(before.Legacy, p.Legacy) {
			return p, nil
		}
	}
}

// Must precede every group/session lock. Waiting for numeric alias buckets can
// expose a new closure in RC; abort rather than acquiring additional buckets in
// a different order. A complete retry discovers the enlarged set from scratch.
func lockRawDerivedAliases(ctx context.Context, tx *sql.Tx, groups, aliases []string) (rawDerivedLocks, error) {
	p, err := discoverRawDerived(ctx, tx, groups, aliases)
	if err != nil {
		return p, err
	}
	if err = lockHostedAliases(ctx, tx, p.Aliases); err != nil {
		return p, err
	}
	checked, err := discoverRawDerived(ctx, tx, groups, aliases)
	if err != nil {
		return p, err
	}
	if !slices.Equal(p.Groups, checked.Groups) || !slices.Equal(p.Aliases, checked.Aliases) || !slices.Equal(p.Legacy, checked.Legacy) {
		return p, rawDerivedRetry()
	}
	return p, nil
}
func (p rawDerivedLocks) lockGroups(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `SELECT group_id FROM raw_session_groups WHERE group_id=ANY($1) ORDER BY group_id FOR UPDATE`, p.Groups)
	return err
}
func (p rawDerivedLocks) lockSessions(ctx context.Context, tx *sql.Tx) error {
	// Sessions are FK parents for carried/cache/pin rows. Their UPDATE locks also
	// exclude competing inserts into absent children, while group locks serialize
	// raw overlays and immutable content membership. Legacy originals stay intact.
	_, err := tx.ExecContext(ctx, `SELECT id FROM sessions WHERE (raw_group_id<>'' AND raw_group_id=ANY($1)) OR id=ANY($2) ORDER BY id FOR UPDATE`, p.Groups, p.Legacy)
	return err
}

// All ordinary materialization/source/proof writes finish before this helper.
// It stages cache/carry first and returns visibility for the caller's final
// publication phase. No visibility UPDATE is hidden inside materializeGroup.
func (s *RawProjectionStore) prepareRawDerivedWrites(ctx context.Context, tx *sql.Tx, locks rawDerivedLocks) (rawSupersessionPlan, error) {
	var out rawSupersessionPlan
	scopes, err := loadRawRolloutScopes(ctx, tx)
	if err != nil {
		return out, err
	}
	// No-scope/default-active is not activation authority. Supported last-clear
	// already removes all maps; do not create or repair them from ordinary work.
	if len(scopes) == 0 || len(locks.Groups) == 0 {
		return out, nil
	}
	served, err := rawServedGroupsWithin(ctx, tx, scopes, locks.Groups)
	if err != nil {
		return out, err
	}
	plan, err := prepareRawSupersessionsWithin(ctx, tx, served, locks.Groups, locks.Legacy)
	if err != nil {
		return out, err
	}
	for _, change := range plan.Changes {
		if (change.Before != "" && !slices.Contains(locks.Groups, change.Before)) || (change.After != "" && !slices.Contains(locks.Groups, change.After)) {
			return out, rawDerivedRetry()
		}
		if !slices.Contains(locks.Legacy, change.Legacy) {
			return out, rawDerivedRetry()
		}
		out.Changes = append(out.Changes, change)
	}
	for _, visibility := range plan.Visibility {
		if !slices.Contains(locks.Legacy, visibility.Legacy) {
			return out, rawDerivedRetry()
		}
		out.Visibility = append(out.Visibility, visibility)
	}
	out.IdentityChanged = len(out.Changes) > 0 || len(out.Visibility) > 0
	// New physical rows were created by this transaction under the held groups;
	// every pre-existing legacy/session target was locked before materialization.
	// Outbox FK parents and relation access are acquired before legacy triggers
	// can take corpus. New revision keys exclude outbox unique-key competitors.
	if _, err = tx.ExecContext(ctx, `LOCK TABLE raw_embedding_outbox IN ROW EXCLUSIVE MODE`); err != nil {
		return out, err
	}
	if _, err = tx.ExecContext(ctx, `SELECT session_id FROM raw_content_revisions WHERE group_id=ANY($1) ORDER BY session_id FOR KEY SHARE`, locks.Groups); err != nil {
		return out, err
	}
	for _, change := range out.Changes {
		if change.After == "" {
			continue
		}
		carry, err := prepareRawLegacyCarry(ctx, tx, change.Legacy, change.After)
		if err != nil {
			return out, err
		}
		if !slices.Contains(locks.Groups, carry.Group) {
			return out, rawDerivedRetry()
		}
		if err = s.applyRawLegacyCarry(ctx, tx, carry); err != nil {
			return out, err
		}
	}
	if err = writeRawSupersessionCache(ctx, tx, out); err != nil {
		return out, err
	}
	return out, nil
}
