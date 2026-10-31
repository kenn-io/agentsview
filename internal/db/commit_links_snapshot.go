package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"slices"

	"go.kenn.io/agentsview/internal/config"
)

// CommitLinkExport is one bounded, source-owned card in a replica snapshot.
type CommitLinkExport struct{ ID, SessionID, SHA, Payload string }

// CommitLinkSnapshot is valid only during ReadCommitLinkSnapshot's callback.
// Each streams rows without retaining the complete index in memory.
type CommitLinkSnapshot struct {
	ArchiveID, DatabaseID string
	Revision              int64
	Ready                 bool
	Each                  func(func(CommitLinkExport) error) error
}

// ReadCommitLinkSnapshot pins publication metadata, session visibility and cards
// in one archive read transaction. The callback must not write to this archive.
func (d *DB) ReadCommitLinkSnapshot(ctx context.Context, projects, excluded []string, consume func(CommitLinkSnapshot) error) error {
	release, err := d.acquireBulkRead(ctx)
	if err != nil {
		return err
	}
	defer release()
	tx, err := d.getReader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	snapshot := CommitLinkSnapshot{}
	if err := tx.QueryRowContext(ctx, "SELECT value FROM archive_metadata WHERE key=?", archiveMetadataArchiveIDKey).Scan(&snapshot.ArchiveID); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, "SELECT value FROM archive_metadata WHERE key=?", archiveMetadataDatabaseIDKey).Scan(&snapshot.DatabaseID); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, "SELECT revision,ready FROM commit_link_publication_state WHERE singleton=1").Scan(&snapshot.Revision, &snapshot.Ready); err != nil {
		return err
	}
	snapshot.Each = func(emit func(CommitLinkExport) error) error {
		rows, err := tx.QueryContext(ctx, "SELECT id,payload,source_revision,project_hint FROM commit_session_links ORDER BY session_id,id")
		if err != nil {
			return err
		}
		defer rows.Close()
		lastKey := ""
		var bound commitSessionBinding
		for rows.Next() {
			var row CommitLinkExport
			var raw, revision, hint string
			if err := rows.Scan(&row.ID, &raw, &revision, &hint); err != nil {
				return err
			}
			if len(raw) > 64<<10 {
				return ErrCommitLookupTooLarge
			}
			var link CommitLink
			if err := json.Unmarshal([]byte(raw), &link); err != nil {
				return err
			}
			if link.Source == "transcript" && d.ArchiveContent() != config.ArchiveContentFull {
				continue
			}
			key := link.Source + ":" + link.SessionID
			if key != lastKey {
				bound, err = readCommitSessionBinding(ctx, tx, link)
				if err != nil {
					return err
				}
				lastKey = key
			}
			link.Ambiguous = link.Ambiguous || bound.ambiguous
			link.SessionID = bound.session.id
			project := hint
			if bound.missing {
				unavailableCommit(&link)
			} else if bound.session.deleted.Valid {
				continue
			} else {
				project = bound.session.project
				hydrateCommit(&link, bound.session, revision)
			}
			filtered := len(projects) > 0 || len(excluded) > 0
			if filtered && project == "" {
				continue
			}
			if len(projects) > 0 && !slices.Contains(projects, project) || slices.Contains(excluded, project) {
				continue
			}
			if d.ArchiveContent() == config.ArchiveContentUsage {
				link.Subject = ""
				link.RepositoryPath = ""
				link.Branch = ""
				link.Turn = nil
			}
			data, err := json.Marshal(link)
			if err != nil {
				return err
			}
			row.SessionID = link.SessionID
			row.SHA = link.CommitSHA
			row.Payload = string(data)
			if err := emit(row); err != nil {
				return err
			}
		}
		return rows.Err()
	}
	return consume(snapshot)
}
