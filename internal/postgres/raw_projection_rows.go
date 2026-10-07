package postgres

import (
	"context"
	"database/sql"
	"reflect"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
)

const (
	rawSourceProofDeleteSQL = `DELETE FROM session_sources WHERE source_id=$1`
	rawSourceProofAttachSQL = `UPDATE session_sources p SET physical_session_id=b.session_id FROM raw_session_branches b WHERE b.branch_id=p.branch_id AND b.active AND b.session_id=$1`
	rawSourceProofDetachSQL = `UPDATE session_sources SET physical_session_id=NULL WHERE physical_session_id=$1`
	rawGroupContentsSQL     = `SELECT id,raw_content_revision FROM sessions WHERE raw_group_id=$1 AND raw_group_id<>'' UNION SELECT session_id,content_revision FROM raw_session_branches WHERE group_id=$1 AND active ORDER BY 1`
)

// rawEmbeddingChange is a physical session whose embeddings must follow a
// materialization once that materialization has a corpus revision.
type rawEmbeddingChange struct{ session, revision, action string }

// publishRawRevision issues the corpus revision for rows the caller has already
// written and queues their embedding work under it. It locks the corpus row,
// which manifest acceptance and every other publication also update, so
// nothing after it in the transaction may wait on another transaction's lock.
// The outbox rows record the selection revision read under that lock, which is
// the one the transaction commits with.
func publishRawRevision(ctx context.Context, tx *sql.Tx, identityChanged bool, changes []rawEmbeddingChange) error {
	var corpus int64
	err := tx.QueryRowContext(ctx, `INSERT INTO raw_corpus_state(singleton,corpus_revision,identity_revision) VALUES(1,1,CASE WHEN $1 THEN 1 ELSE 0 END) ON CONFLICT(singleton) DO UPDATE SET corpus_revision=raw_corpus_state.corpus_revision+1,identity_revision=raw_corpus_state.identity_revision+EXCLUDED.identity_revision RETURNING corpus_revision`, identityChanged).Scan(&corpus)
	if err != nil || len(changes) == 0 {
		return err
	}
	sessions := make([]string, len(changes))
	revisions := make([]string, len(changes))
	actions := make([]string, len(changes))
	for i, c := range changes {
		sessions[i], revisions[i], actions[i] = c.session, c.revision, c.action
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO raw_embedding_outbox(session_id,corpus_revision,content_revision,action,selection_revision) SELECT c.session_id,$2,c.content_revision,c.action,s.selection_revision FROM unnest($1::text[],$3::text[],$4::text[]) AS c(session_id,content_revision,action) CROSS JOIN raw_corpus_state s WHERE s.singleton=1 ON CONFLICT DO NOTHING`, sessions, corpus, revisions, actions)
	return err
}

// materializeGroup requires the caller to hold the group's row lock. It
// returns the embedding changes for the caller to pass to publishRawRevision.
func (s *RawProjectionStore) materializeGroup(ctx context.Context, tx *sql.Tx, group string) ([]rawEmbeddingChange, error) {
	if err := reconcileRawPrefixes(ctx, tx, group); err != nil {
		return nil, err
	}
	branches, err := loadRawBranches(ctx, tx, "group_id", group)
	if err != nil {
		return nil, err
	}
	overlays, err := loadRawOverlays(ctx, tx, group)
	if err != nil {
		return nil, err
	}
	cohorts := map[string][]rawBranch{}
	for _, b := range branches {
		if b.Active && !overlays.boolean(b.ID, "excluded") {
			cohorts[b.Session] = append(cohorts[b.Session], b)
		}
	}
	rows, err := tx.QueryContext(ctx, rawGroupContentsSQL, group)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type content struct{ id, revision string }
	var contents []content
	var changes []rawEmbeddingChange
	for rows.Next() {
		var c content
		if err = rows.Scan(&c.id, &c.revision); err != nil {
			rows.Close()
			return nil, err
		}
		contents = append(contents, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, c := range contents {
		exists, err := rawPhysicalExists(ctx, tx, c.id)
		if err != nil {
			return nil, err
		}
		members := cohorts[c.id]
		action := "reconcile"
		if len(members) == 0 {
			if !exists {
				continue
			}
			if _, err = tx.ExecContext(ctx, rawSourceProofDetachSQL, c.id); err != nil {
				return nil, err
			}
			if _, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE id=$1`, c.id); err != nil {
				return nil, err
			}
			action = "remove"
		} else {
			if !exists {
				payload, err := loadRawPayload(ctx, tx, c.id)
				if err != nil {
					return nil, err
				}
				if err = s.writePayload(ctx, tx, c.id, group, c.revision, payload); err != nil {
					return nil, err
				}
			}
			if _, err = tx.ExecContext(ctx, rawSourceProofAttachSQL, c.id); err != nil {
				return nil, err
			}
			if err = s.materializeCuration(ctx, tx, group, c.id, members); err != nil {
				return nil, err
			}
		}
		if !exists || action == "remove" {
			changes = append(changes, rawEmbeddingChange{session: c.id, revision: c.revision, action: action})
		}
	}
	return changes, nil
}

func (s *RawProjectionStore) writePayload(ctx context.Context, tx *sql.Tx, id, group, revision string, p ingest.PreparedSession) error {
	p.Session.ID = id
	p.Session.FilePath = nil
	p.Session.Machine = "hosted"
	p.Session.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	p.Session.TranscriptRevision = &revision
	p.Session.DataVersion = db.CurrentDataVersion()
	// Full shared preparation computes signals independently of Session. Copy
	// matching scalar fields, then the quality scalar fields expected by the
	// existing SQL kernel. This does not encode public JSON or omit hidden fields.
	copyRawSignalFields(&p.Session, p.Signals)
	for i := range p.UsageEvents {
		p.UsageEvents[i].SessionID = id
	}
	for i := range p.Findings {
		p.Findings[i].SessionID = id
	}
	if err := writePGSession(ctx, tx, p.Session, "raw-projection:"+group, nil, pgSessionWriteOptions{Machine: "hosted", UsageOnly: s.options.Content.ArchiveContent.UsageOnly(), SkipAliases: true}); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET provenance_kind='raw',raw_group_id=$2,raw_content_revision=$3 WHERE id=$1`, id, group, revision); err != nil {
		return err
	}
	if err := bulkInsertMessages(ctx, tx, id, p.Messages); err != nil {
		return err
	}
	if err := bulkInsertToolCalls(ctx, tx, id, p.Messages); err != nil {
		return err
	}
	if err := bulkInsertToolResultEvents(ctx, tx, id, p.Messages); err != nil {
		return err
	}
	if err := bulkInsertUsageEvents(ctx, tx, p.UsageEvents); err != nil {
		return err
	}
	return bulkInsertSecretFindings(ctx, tx, id, p.Findings)
}

func copyRawSignalFields(session *db.Session, signals db.SessionSignalUpdate) {
	target := reflect.ValueOf(session).Elem()
	for _, source := range []reflect.Value{reflect.ValueOf(signals), reflect.ValueOf(signals.QualitySignals)} {
		for i := range source.NumField() {
			name := source.Type().Field(i).Name
			if name == "Version" {
				name = "QualitySignalVersion"
			}
			field := target.FieldByName(name)
			if field.IsValid() && field.CanSet() && field.Type() == source.Field(i).Type() {
				field.Set(source.Field(i))
			}
		}
	}
}
