package postgres

import (
	"context"
	"database/sql"
	"errors"
	"slices"
)

type RawIdentityState string

const (
	RawIdentityUnique    RawIdentityState = "unique"
	RawIdentityAmbiguous RawIdentityState = "ambiguous"
	RawIdentityGone      RawIdentityState = "gone"
)

// RawIdentity resolves aliases only. SessionID is private storage metadata for
// the hosted adapter; callers must map it before returning a public response.
type RawIdentity struct {
	State                                                                                         RawIdentityState
	GroupID, AnchorBranch, SessionID, ContentRevision, PublicID                                   string
	Variants                                                                                      []string
	ProjectionGeneration, SelectedGeneration, CorpusRevision, IdentityRevision, SelectionRevision int64
}

func (s *RawProjectionStore) Resolve(ctx context.Context, alias string) (RawIdentity, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return RawIdentity{}, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := resolveRawIdentity(ctx, tx, alias)
	if err != nil {
		return result, err
	}
	return result, tx.Commit()
}
func resolveRawIdentity(ctx context.Context, tx *sql.Tx, alias string) (RawIdentity, error) {
	result := RawIdentity{State: RawIdentityGone}
	stateErr := tx.QueryRowContext(ctx, `SELECT corpus_revision,identity_revision,selection_revision FROM raw_corpus_state WHERE singleton=1`).Scan(&result.CorpusRevision, &result.IdentityRevision, &result.SelectionRevision)
	if stateErr != nil && !errors.Is(stateErr, sql.ErrNoRows) {
		return result, stateErr
	}
	rows, err := tx.QueryContext(ctx, `SELECT group_id,anchor_branch FROM raw_session_public_aliases WHERE alias_id=$1 ORDER BY group_id`, alias)
	if err != nil {
		return result, err
	}
	type target struct{ group, anchor string }
	var targets []target
	for rows.Next() {
		var t target
		if err = rows.Scan(&t.group, &t.anchor); err != nil {
			rows.Close()
			return result, err
		}
		targets = append(targets, t)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	if len(targets) == 0 {
		return result, nil
	}
	if len(targets) > 1 {
		var active []target
		for _, t := range targets {
			variants, err := rawGroupVariants(ctx, tx, t.group)
			if err != nil {
				return result, err
			}
			if len(variants) > 0 {
				active = append(active, t)
				result.Variants = append(result.Variants, variants...)
			}
		}
		if len(active) > 1 {
			result.State = RawIdentityAmbiguous
			slices.Sort(result.Variants)
			return result, nil
		}
		if len(active) == 0 {
			return result, nil
		}
		targets = active
		result.Variants = nil
	}

	result.GroupID = targets[0].group
	result.AnchorBranch = targets[0].anchor
	branches, err := loadRawBranches(ctx, tx, "group_id", result.GroupID)
	if err != nil {
		return result, err
	}
	var base string
	if err = tx.QueryRowContext(ctx, `SELECT base_alias FROM raw_session_groups WHERE group_id=$1`, result.GroupID).Scan(&base); err != nil {
		return result, err
	}
	overlays, err := loadRawOverlays(ctx, tx, result.GroupID)
	if err != nil {
		return result, err
	}
	cohorts := map[string]rawBranch{}
	var anchor *rawBranch
	for _, b := range branches {
		if !b.Active || overlays.boolean(b.ID, "excluded") {
			continue
		}
		if _, ok := cohorts[b.Session]; !ok {
			cohorts[b.Session] = b
		}
		if b.ID == result.AnchorBranch {
			copy := b
			anchor = &copy
		}
	}
	for _, b := range cohorts {
		result.Variants = append(result.Variants, base+"~"+b.ID)
	}
	slices.Sort(result.Variants)
	if result.AnchorBranch != "" && anchor == nil || len(cohorts) == 0 {
		return result, nil
	}
	if result.AnchorBranch == "" {
		if len(cohorts) > 1 {
			result.State = RawIdentityAmbiguous
			return result, nil
		}
		for _, b := range cohorts {
			copy := b
			anchor = &copy
		}
	}
	if anchor == nil {
		return result, errors.New("raw identity cohort has no active anchor")
	}
	result.State = RawIdentityUnique
	result.SessionID = anchor.Session
	result.ContentRevision = anchor.Revision
	result.ProjectionGeneration = anchor.Generation
	result.PublicID = base
	var aliases int
	if err = tx.QueryRowContext(ctx, `SELECT count(DISTINCT a.group_id) FROM raw_session_public_aliases a JOIN sessions s ON s.raw_group_id=a.group_id WHERE a.alias_id=$1`, base).Scan(&aliases); err != nil {
		return result, err
	}
	if len(cohorts) > 1 || aliases > 1 {
		result.PublicID = base + "~" + cohorts[anchor.Session].ID
	}
	err = tx.QueryRowContext(ctx, `SELECT projection_generation FROM raw_source_projections WHERE source_id=$1`, anchor.Source).Scan(&result.SelectedGeneration)
	return result, err
}
func rawGroupVariants(ctx context.Context, tx *sql.Tx, group string) ([]string, error) {
	branches, err := loadRawBranches(ctx, tx, "group_id", group)
	if err != nil {
		return nil, err
	}
	overlays, err := loadRawOverlays(ctx, tx, group)
	if err != nil {
		return nil, err
	}
	var base string
	if err = tx.QueryRowContext(ctx, `SELECT base_alias FROM raw_session_groups WHERE group_id=$1`, group).Scan(&base); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var variants []string
	for _, b := range branches {
		if b.Active && !overlays.boolean(b.ID, "excluded") && !seen[b.Session] {
			seen[b.Session] = true
			variants = append(variants, base+"~"+b.ID)
		}
	}
	slices.Sort(variants)
	return variants, nil
}
