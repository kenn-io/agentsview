package clickhouse

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

// Rows land before the source/scope publication pointer. Interrupted writes
// remain invisible; replacing a scope publishes removals without live joins.
const commitLinksDDL = `CREATE TABLE IF NOT EXISTS commit_session_links (
 source_archive_id String,publication_scope String,publication_version UInt64,
 link_id String,session_id String,commit_sha String,payload String,push_version UInt64
) ENGINE=ReplacingMergeTree(push_version) ORDER BY(source_archive_id,publication_scope,publication_version,link_id)`
const commitPublicationsDDL = `CREATE TABLE IF NOT EXISTS commit_link_publications (
 source_archive_id String,publication_scope String,database_generation String,
 revision Int64,version UInt64,ready Bool,push_version UInt64
) ENGINE=ReplacingMergeTree(push_version) ORDER BY(source_archive_id,publication_scope)`
const commitLinksRelation = `(SELECT session_id,commit_sha,payload FROM (
 SELECT c.session_id,c.commit_sha,c.payload,row_number() OVER(PARTITION BY c.source_archive_id,c.link_id ORDER BY p.version DESC,c.publication_scope) AS position
 FROM commit_session_links c JOIN ` + storage.CurrentCommitLinkPublicationsRelation + ` ON p.source_archive_id=c.source_archive_id AND p.publication_scope=c.publication_scope AND p.version=c.publication_version
) candidates WHERE position=1) commit_links`

func (s *Store) CommitLinks(ctx context.Context, shas []string, id string) ([]db.CommitLink, error) {
	return storage.ReadPublishedCommitLinks(ctx, s.conn.QueryContext, db.ClickHouseQueryDialect(), commitLinksRelation, storage.CurrentCommitLinkPublicationsRelation, shas, id)
}

func (s *Sync) publishCommitLinks(ctx context.Context) error {
	scope := db.CanonicalPushScope(s.projects, s.excludeProjects)
	return s.local.ReadCommitLinkSnapshot(ctx, s.projects, s.excludeProjects, func(snapshot db.CommitLinkSnapshot) error {
		var generation string
		var revision int64
		err := s.conn.QueryRowContext(ctx, `SELECT database_generation,revision FROM commit_link_publications WHERE source_archive_id=? AND publication_scope=?`, snapshot.ArchiveID, scope).Scan(&generation, &revision)
		if err == nil && generation == snapshot.DatabaseID && revision == snapshot.Revision {
			return nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		version := uint64(time.Now().UnixNano())
		batch := make([]db.CommitLinkExport, 0, 256)
		flush := func() error {
			if len(batch) == 0 {
				return nil
			}
			tx, err := s.conn.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			stmt, err := tx.PrepareContext(ctx, `INSERT INTO commit_session_links(source_archive_id,publication_scope,publication_version,link_id,session_id,commit_sha,payload,push_version)`)
			if err != nil {
				return err
			}
			defer stmt.Close()
			for _, row := range batch {
				if _, err := stmt.ExecContext(ctx, snapshot.ArchiveID, scope, version, row.ID, row.SessionID, row.SHA, row.Payload, version); err != nil {
					return err
				}
			}
			if err := tx.Commit(); err != nil {
				return err
			}
			batch = batch[:0]
			return nil
		}
		if err := snapshot.Each(func(row db.CommitLinkExport) error {
			batch = append(batch, row)
			if len(batch) == cap(batch) {
				return flush()
			}
			return nil
		}); err != nil {
			return err
		}
		if err := flush(); err != nil {
			return err
		}

		if _, err := s.conn.ExecContext(ctx, `INSERT INTO commit_link_publications(source_archive_id,publication_scope,database_generation,revision,version,ready,push_version) VALUES(?,?,?,?,?,?,?)`, snapshot.ArchiveID, scope, snapshot.DatabaseID, snapshot.Revision, version, snapshot.Ready, version); err != nil {
			return err
		}
		// Cleanup follows publication and never removes another source/scope.
		_, err = s.conn.ExecContext(ctx, `DELETE FROM commit_session_links WHERE source_archive_id=? AND publication_scope=? AND publication_version < `+strconv.FormatUint(version, 10), snapshot.ArchiveID, scope)
		return err
	})
}
