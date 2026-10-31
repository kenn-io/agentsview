package postgres

import (
	"context"
	"database/sql"
	"errors"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

func (s *Store) CommitLinks(ctx context.Context, shas []string, id string) ([]db.CommitLink, error) {
	rows, err := storage.ReadPublishedCommitLinks(ctx, s.pg.QueryContext, db.PostgresQueryDialect(), curatedCommitLinksRelation, storage.CurrentCommitLinkPublicationsRelation, shas, id)
	if isUndefinedTable(err) || isUndefinedColumn(err) {
		return nil, db.ErrCommitIndexUnavailable
	}
	return rows, err
}

// Hosted provenance needs an explicit public-identity projection. Never forward physical IDs.
func (h *HostedStore) CommitLinks(context.Context, []string, string) ([]db.CommitLink, error) {
	return nil, db.ErrCommitIndexUnavailable
}

// Clear retained context in the existing curation transaction. Native SQL keeps
// deletion compatible with servers that do not support PL/pgSQL triggers.
func clearDeletedCommitSessionsPG(ctx context.Context, tx *sql.Tx, ids []string) error {
	var present int
	err := tx.QueryRowContext(ctx, "SELECT 1 FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = $1", "commit_session_links").Scan(&present)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE commit_session_links c SET payload=(c.payload::jsonb || '{"session":null,"gone":true,"turn":null,"agent":"","project":"","session_started_at":"","web_url":""}'::jsonb)::text FROM sessions s
 WHERE s.id=ANY($1) AND c.session_id=s.id AND c.source_archive_id=s.source_archive_id`, ids)
	return err
}

const curatedCommitLinksRelation = `(SELECT session_id,commit_sha,payload FROM (
 SELECT c.session_id,c.commit_sha,
 CASE WHEN NOT EXISTS(SELECT 1 FROM sessions own WHERE own.id=c.session_id AND own.source_archive_id=c.source_archive_id)
 THEN (c.payload::jsonb || '{"session":null,"gone":true,"turn":null,"agent":"","project":"","session_started_at":"","web_url":""}'::jsonb)::text ELSE c.payload END AS payload,
 row_number() OVER(PARTITION BY c.source_archive_id,c.link_id ORDER BY p.version DESC,c.publication_scope) AS position
 FROM commit_session_links c JOIN ` + storage.CurrentCommitLinkPublicationsRelation + ` ON p.source_archive_id=c.source_archive_id AND p.publication_scope=c.publication_scope
 WHERE NOT EXISTS(SELECT 1 FROM sessions s WHERE s.id=c.session_id AND s.source_archive_id=c.source_archive_id AND s.deleted_at IS NOT NULL)
) candidates WHERE position=1) commit_links`

func (s *Sync) publishCommitLinks(ctx context.Context) error {
	return storage.PublishCommitLinksSQL(ctx, s.pg, db.PostgresQueryDialect(), s.local, s.projects, s.excludeProjects, sanitizeCommitPublicationPG)
}

// Excluded physical sessions must not regain stored prompt text when unrelated
// source changes publish a fresh snapshot. Apply this before the marker commits.
func sanitizeCommitPublicationPG(ctx context.Context, tx *sql.Tx, archive, scope string) error {
	_, err := tx.ExecContext(ctx, `UPDATE commit_session_links c SET payload=(c.payload::jsonb || '{"session":null,"gone":true,"turn":null,"agent":"","project":"","session_started_at":"","web_url":""}'::jsonb)::text
 WHERE c.source_archive_id=$1 AND c.publication_scope=$2 AND NOT EXISTS(SELECT 1 FROM sessions s WHERE s.id=c.session_id AND s.source_archive_id=c.source_archive_id)`, archive, scope)
	return err
}
