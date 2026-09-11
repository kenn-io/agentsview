package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawsync"
)

type rawBranch struct {
	ID, Source, Group, Member, Session, Revision, Manifest, Version string
	Generation                                                      int64
	Active                                                          bool
	Payload                                                         []byte
}

func loadRawBranches(ctx context.Context, q hostedQuerier, where string, arg string) ([]rawBranch, error) {
	rows, err := q.QueryContext(ctx, `SELECT branch_id,source_id,group_id,member_id,session_id,content_revision,manifest_id,processing_version,projection_generation,active,prior_payload FROM raw_session_branches WHERE `+where+`=$1 ORDER BY branch_id`, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var branches []rawBranch
	for rows.Next() {
		var b rawBranch
		if err = rows.Scan(&b.ID, &b.Source, &b.Group, &b.Member, &b.Session, &b.Revision, &b.Manifest, &b.Version, &b.Generation, &b.Active, &b.Payload); err != nil {
			return nil, err
		}
		branches = append(branches, b)
	}
	return branches, rows.Err()
}

func (s *RawProjectionStore) Project(ctx context.Context, lease rawderive.JobLease, m rawsync.CanonicalManifest, parsed rawderive.ParsedManifest) error {
	if err := s.validateManifest(m); err != nil {
		return err
	}
	if parsed.Tombstone != (m.Manifest.Kind == rawsync.ManifestTombstone) {
		return fmt.Errorf("raw projection tombstone mismatch")
	}
	if parsed.Tombstone && len(parsed.Outcome.Results) > 0 {
		return fmt.Errorf("tombstone cannot publish members")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = s.lockHead(ctx, tx, m); err != nil {
		return err
	}
	if err = s.lockLease(ctx, tx, m, lease); err != nil {
		return err
	}
	source := rawSourceID(m)
	prior, err := loadRawBranches(ctx, tx, "source_id", source)
	if err != nil {
		return err
	}
	groups := map[string]string{}
	keys := map[string]string{}
	priorByGroup := map[string]rawBranch{}
	for _, b := range prior {
		groups[b.Group] = ""
		priorByGroup[b.Group] = b
	}
	candidates := map[string]ingest.Candidate{}
	for _, r := range parsed.Outcome.Results {
		c, err := ingest.PrepareCandidate(ctx, r.Result, s.options.Content)
		if err != nil {
			return err
		}
		if slices.Contains(parsed.Outcome.ExcludedSessionIDs, c.Session.ID) {
			continue
		}
		if c.Session.ID == "" || c.Session.Agent == "" {
			return fmt.Errorf("raw projection member identity missing")
		}
		group, key := rawGroupID(m, c.Session)
		if _, duplicate := candidates[group]; duplicate {
			return fmt.Errorf("duplicate raw projection member")
		}
		groups[group] = c.Session.ID
		keys[group] = key
		candidates[group] = c
	}
	ordered := make([]string, 0, len(groups))
	for group := range groups {
		ordered = append(ordered, group)
	}
	slices.Sort(ordered)
	// Lock known and newly introduced public aliases before sorted group locks.
	// This includes first publication, where legacy validation cannot lock a row.
	var aliases []string
	rows, err := tx.QueryContext(ctx, `SELECT alias_id FROM raw_session_public_aliases WHERE group_id=ANY($1) ORDER BY alias_id`, ordered)
	if err != nil {
		return err
	}
	for rows.Next() {
		var alias string
		if err = rows.Scan(&alias); err != nil {
			rows.Close()
			return err
		}
		aliases = append(aliases, alias)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for group, base := range groups {
		if base != "" {
			aliases = append(aliases, base, base+"~"+rawDigest("branch-v1", group, source))
			aliases = append(aliases, pgSessionAliasIDs(candidates[group].Session)...)
		}
	}
	if err = lockHostedAliases(ctx, tx, aliases); err != nil {
		return err
	}
	for _, group := range ordered {
		if base := groups[group]; base != "" {
			_, err = tx.ExecContext(ctx, `INSERT INTO raw_session_groups(group_id,provider,logical_key,base_alias) VALUES($1,$2,$3,$4) ON CONFLICT(group_id) DO NOTHING`, group, candidates[group].Session.Agent, keys[group], base)
			if err != nil {
				return err
			}
		}
		var storedKey string
		err = tx.QueryRowContext(ctx, `SELECT logical_key FROM raw_session_groups WHERE group_id=$1 FOR UPDATE`, group).Scan(&storedKey)
		if err != nil {
			return err
		}
		if key, ok := keys[group]; ok && key != storedKey {
			return fmt.Errorf("raw logical identity digest collision")
		}
	}
	complete := parsed.Tombstone || parsed.Outcome.ResultSetComplete && len(parsed.Outcome.SourceErrors) == 0
	for _, r := range parsed.Outcome.Results {
		if r.DataVersion == parser.DataVersionNeedsRetry || r.RetryReason != "" {
			complete = false
		}
	}
	changed := false
	derivedChanged := false
	for _, group := range ordered {
		c, ok := candidates[group]
		if !ok {
			continue
		}
		previous, exists := priorByGroup[group]
		var old ingest.PreparedSession
		var history *ingest.PriorSession
		if exists {
			old, err = loadRawPayload(ctx, tx, previous.Session)
			if err != nil {
				return err
			}
			metadata, decodeErr := decodeRawPayload(previous.Payload)
			if decodeErr != nil {
				return decodeErr
			}
			old.Session = metadata.Session
			history = &ingest.PriorSession{Session: old.Session, Messages: old.Messages}
		}
		decision, err := ingest.ReconcileProviderHistory(ctx, c, history, s.options.Content)
		if err != nil {
			return err
		}
		var prepared ingest.PreparedSession
		if decision.Action == ingest.HistoryPreserve {
			prepared = old
		} else {
			prepared, err = ingest.Finalize(ctx, decision.Candidate, s.options.Content)
			if err != nil {
				return err
			}
		}
		prepared.Signals = ingest.RefreshSignalRecencyAt(prepared.Session, prepared.Messages, prepared.Signals, s.options.Now())
		revision, err := rawContentRevision(prepared)
		if err != nil {
			return err
		}
		sessionID := "raw-row-" + rawDigest("row-v1", group, revision)
		branchID := rawDigest("branch-v1", group, source)
		payload, err := encodeRawPayload(prepared)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO raw_content_revisions(session_id,group_id,content_revision,payload) VALUES($1,$2,$3,$4) ON CONFLICT(session_id) DO NOTHING`, sessionID, group, revision, payload)
		if err != nil {
			return err
		}
		visibleChange, err := publishRawRecency(ctx, tx, sessionID, prepared.Signals)
		if err != nil {
			return err
		}
		derivedChanged = derivedChanged || visibleChange

		metadata, err := encodeRawPayload(ingest.PreparedSession{Session: prepared.Session})
		if err != nil {
			return err
		}
		observed, err := encodeRawPayload(ingest.PreparedSession{Session: c.Session})
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO raw_session_branches(branch_id,source_id,group_id,member_id,session_id,content_revision,manifest_id,processing_version,projection_generation,active,prior_payload) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,true,$10)
 ON CONFLICT(branch_id) DO UPDATE SET session_id=EXCLUDED.session_id,content_revision=EXCLUDED.content_revision,manifest_id=EXCLUDED.manifest_id,processing_version=EXCLUDED.processing_version,projection_generation=EXCLUDED.projection_generation,active=true,prior_payload=EXCLUDED.prior_payload`, branchID, source, group, c.Session.ID, sessionID, revision, m.ManifestID, lease.ProcessingVersion, lease.ProjectionGeneration, metadata)
		if err != nil {
			return err
		}
		if err = writeRawLinks(ctx, tx, branchID, prepared); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO raw_source_contributions(branch_id,manifest_id,payload,projection_generation,processing_version,prior_contributed) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(branch_id,manifest_id,projection_generation) DO NOTHING`, branchID, m.ManifestID, observed, lease.ProjectionGeneration, lease.ProcessingVersion, decision.PriorContributed)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO raw_session_public_aliases(alias_id,group_id,anchor_branch) VALUES($1,$2,''),($3,$2,$4) ON CONFLICT(alias_id,group_id) DO NOTHING`, groups[group], group, groups[group]+"~"+branchID, branchID)
		if err != nil {
			return err
		}

		for _, alias := range pgSessionAliasIDs(c.Session) {
			_, err = tx.ExecContext(ctx, `INSERT INTO raw_session_public_aliases(alias_id,group_id,anchor_branch) VALUES($1,$2,'') ON CONFLICT(alias_id,group_id) DO NOTHING`, alias, group)
			if err != nil {
				return err
			}
		}
		changed = changed || !exists || !previous.Active || previous.Session != sessionID
	}
	for _, b := range prior {
		_, present := candidates[b.Group]
		excluded := slices.Contains(parsed.Outcome.ExcludedSessionIDs, b.Member)
		if b.Active && (excluded || !present && complete) {
			_, err = tx.ExecContext(ctx, `UPDATE raw_session_branches SET active=false WHERE branch_id=$1`, b.ID)
			if err != nil {
				return err
			}
			changed = true
		}
	}
	if changed || derivedChanged {
		var revision int64
		err = tx.QueryRowContext(ctx, `INSERT INTO raw_corpus_state(singleton,corpus_revision,identity_revision) VALUES(1,1,CASE WHEN $1 THEN 1 ELSE 0 END) ON CONFLICT(singleton) DO UPDATE SET corpus_revision=raw_corpus_state.corpus_revision+1,identity_revision=raw_corpus_state.identity_revision+EXCLUDED.identity_revision RETURNING corpus_revision`, changed).Scan(&revision)
		if err != nil {
			return err
		}
		if changed {
			for _, group := range ordered {
				if err = s.materializeGroup(ctx, tx, group, revision); err != nil {
					return err
				}
			}
		}
	}
	_, err = tx.ExecContext(ctx, rawSourceProofDeleteSQL, source)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO session_sources(branch_id,group_id,source_id,session_id,physical_session_id,manifest_id,content_revision,processing_version,projection_generation) SELECT b.branch_id,b.group_id,b.source_id,b.session_id,s.id,b.manifest_id,b.content_revision,b.processing_version,b.projection_generation FROM raw_session_branches b LEFT JOIN sessions s ON s.id=b.session_id WHERE b.source_id=$1 AND b.active`, source)
	if err != nil {
		return err
	}

	_, err = tx.ExecContext(ctx, `UPDATE raw_source_projections SET last_attempt_manifest_id=$2,successful_manifest_id=CASE WHEN $5 THEN $2 ELSE successful_manifest_id END,membership_complete=$3,diagnostics=$4 WHERE source_id=$1`, source, m.ManifestID, complete, fmt.Sprintf("results=%d errors=%d complete=%t", len(parsed.Outcome.Results), len(parsed.Outcome.SourceErrors), complete), complete || len(candidates) > 0)
	if err != nil {
		return err
	}
	outcome, err := completeProjectionJob(ctx, tx, lease, complete, s.options.RetryPolicy)
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if outcome != 0 {
		return outcome
	}
	return nil
}

// loadRawPayload is transaction-bound and does not use alias resolution.
func loadRawPayload(ctx context.Context, tx *sql.Tx, id string) (ingest.PreparedSession, error) {
	var payload, recency []byte
	err := tx.QueryRowContext(ctx, `SELECT payload,recency_state FROM raw_content_revisions WHERE session_id=$1`, id).Scan(&payload, &recency)
	if err != nil {
		return ingest.PreparedSession{}, err
	}
	p, err := decodeRawPayload(payload)
	if err != nil {
		return p, err
	}
	if string(recency) != "{}" {
		state, err := decodeRawRecency(recency)
		if err != nil {
			return p, err
		}
		state.apply(&p.Signals)
	}
	return p, nil
}
func rawPhysicalExists(ctx context.Context, tx *sql.Tx, id string) (bool, error) {
	var kind string
	err := tx.QueryRowContext(ctx, `SELECT provenance_kind FROM sessions WHERE id=$1`, id).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if kind != "raw" {
		return false, fmt.Errorf("raw physical identity conflicts with legacy proof")
	}
	return true, nil
}
