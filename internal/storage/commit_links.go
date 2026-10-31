package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

// CommitLinksDDL is shared by transactional replicas. Ownership is per source
// and publication scope; neither table cascades with live-session deletion.
const CommitLinksDDL = `
CREATE TABLE IF NOT EXISTS commit_session_links (
 source_archive_id TEXT NOT NULL,publication_scope TEXT NOT NULL,link_id TEXT NOT NULL,
 session_id TEXT NOT NULL,commit_sha TEXT NOT NULL,payload TEXT NOT NULL,
 PRIMARY KEY(source_archive_id,publication_scope,link_id)
);
CREATE INDEX IF NOT EXISTS idx_commit_session_links_sha ON commit_session_links(commit_sha);
CREATE INDEX IF NOT EXISTS idx_commit_session_links_session ON commit_session_links(session_id);
CREATE TABLE IF NOT EXISTS commit_link_publications (
 source_archive_id TEXT NOT NULL,publication_scope TEXT NOT NULL,
 database_generation TEXT NOT NULL,revision BIGINT NOT NULL,version BIGINT NOT NULL,
 ready BOOLEAN NOT NULL,
 PRIMARY KEY(source_archive_id,publication_scope)
);`

// CurrentCommitLinkPublicationsRelation keeps the newest publication for each
// scope. A newer full publication retires older filtered scopes, and a newer
// filtered publication retires an older full snapshot. Filtered scopes
// published after the latest full snapshot remain independent.
const CurrentCommitLinkPublicationsRelation = `(SELECT source_archive_id,publication_scope,database_generation,revision,version,ready FROM (
 SELECT publication.*,
 row_number() OVER(PARTITION BY source_archive_id,publication_scope ORDER BY version DESC) AS scope_position,
 max(CASE WHEN publication_scope='' THEN version END) OVER(PARTITION BY source_archive_id) AS full_version,
 max(CASE WHEN publication_scope<>'' THEN version END) OVER(PARTITION BY source_archive_id) AS filtered_version
 FROM commit_link_publications publication
) ranked WHERE scope_position=1 AND (
 (publication_scope='' AND (filtered_version IS NULL OR version>filtered_version))
 OR (publication_scope<>'' AND (full_version IS NULL OR version>full_version)))) p`

// CommitLinksRelation chooses the newest active scope-owned copy of each
// observation. Cards are source-owned: no globally matching replica session can enrich them.
const CommitLinksRelation = `(SELECT session_id,commit_sha,payload FROM (
 SELECT c.session_id,c.commit_sha,c.payload,
 row_number() OVER(PARTITION BY c.source_archive_id,c.link_id ORDER BY p.version DESC,c.publication_scope) AS position
 FROM commit_session_links c JOIN ` + CurrentCommitLinkPublicationsRelation + `
 ON p.source_archive_id=c.source_archive_id AND p.publication_scope=c.publication_scope
) candidates WHERE position=1) commit_links`

// PublishCommitLinksSQL replaces one pinned index snapshot atomically. A links-only
// revision is published even when the regular session push has no candidates.
type CommitLinkPublicationHook func(context.Context, *sql.Tx, string, string) error

func PublishCommitLinksSQL(ctx context.Context, target *sql.DB, dialect db.QueryDialect, local *db.DB, projects, excluded []string, hooks ...CommitLinkPublicationHook) error {
	scope := db.CanonicalPushScope(projects, excluded)
	return local.ReadCommitLinkSnapshot(ctx, projects, excluded, func(snapshot db.CommitLinkSnapshot) error {
		tx, err := target.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		b := db.NewQueryBuilder(dialect, 0)
		var generation string
		var revision int64
		err = tx.QueryRowContext(ctx, "SELECT database_generation,revision FROM commit_link_publications WHERE source_archive_id="+b.Add(snapshot.ArchiveID)+" AND publication_scope="+b.Add(scope), b.Args()...).Scan(&generation, &revision)
		if err == nil && generation == snapshot.DatabaseID && revision == snapshot.Revision {
			return nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		b = db.NewQueryBuilder(dialect, 0)
		if _, err := tx.ExecContext(ctx, "DELETE FROM commit_session_links WHERE source_archive_id="+b.Add(snapshot.ArchiveID)+" AND publication_scope="+b.Add(scope), b.Args()...); err != nil {
			return err
		}
		if err := snapshot.Each(func(row db.CommitLinkExport) error {
			b := db.NewQueryBuilder(dialect, 0)
			values := []string{b.Add(snapshot.ArchiveID), b.Add(scope), b.Add(row.ID), b.Add(row.SessionID), b.Add(row.SHA), b.Add(row.Payload)}
			query := "INSERT INTO commit_session_links(source_archive_id,publication_scope,link_id,session_id,commit_sha,payload) VALUES(" + joinCommitValues(values) + ")"
			_, err := tx.ExecContext(ctx, query, b.Args()...)
			return err
		}); err != nil {
			return err
		}
		for _, hook := range hooks {
			if err := hook(ctx, tx, snapshot.ArchiveID, scope); err != nil {
				return err
			}
		}
		b = db.NewQueryBuilder(dialect, 0)
		query := "INSERT INTO commit_link_publications(source_archive_id,publication_scope,database_generation,revision,version,ready) VALUES(" + joinCommitValues([]string{b.Add(snapshot.ArchiveID), b.Add(scope), b.Add(snapshot.DatabaseID), b.Add(snapshot.Revision), b.Add(time.Now().UnixNano()), b.Add(snapshot.Ready)}) + ") ON CONFLICT(source_archive_id,publication_scope) DO UPDATE SET database_generation=excluded.database_generation,revision=excluded.revision,version=excluded.version,ready=excluded.ready"
		if _, err := tx.ExecContext(ctx, query, b.Args()...); err != nil {
			return err
		}
		return tx.Commit()
	})
}

func joinCommitValues(values []string) string {
	out := ""
	for i, v := range values {
		if i > 0 {
			out += ","
		}
		out += v
	}
	return out
}

// ReadPublishedCommitLinks checks readiness in the same statement as its bounded
// candidates, so a concurrent publication cannot turn a pending index into a miss.
func ReadPublishedCommitLinks(ctx context.Context, query func(context.Context, string, ...any) (*sql.Rows, error), dialect db.QueryDialect, relation, publications string, shas []string, id string) ([]db.CommitLink, error) {
	if err := db.ValidateCommitSHAs(shas); err != nil {
		return nil, err
	}
	if id == "" && len(shas) == 0 {
		return []db.CommitLink{}, nil
	}
	b := db.NewQueryBuilder(dialect, 0)
	pred := db.BuildCommitLinksPredicate(b, shas, id)
	sqlText := `SELECT payload FROM (SELECT CASE WHEN length(payload)>65536 THEN '' ELSE payload END AS payload FROM ` + relation + ` WHERE ` + pred + ` LIMIT 4097) bounded UNION ALL SELECT 'index-not-ready' WHERE NOT EXISTS(SELECT 1 FROM ` + publications + `) OR EXISTS(SELECT 1 FROM ` + publications + ` WHERE NOT ready)`
	rows, err := query(ctx, sqlText, b.Args()...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	links, err := db.ScanCommitLinkPayloads(rows)
	if err != nil {
		return nil, err
	}
	if len(shas) == 1 {
		db.MarkCommitAmbiguity(links, shas)
	} else {
		db.MarkCommitAmbiguity(links, nil)
	}
	db.SortCommitLinks(links)
	return links, nil
}
