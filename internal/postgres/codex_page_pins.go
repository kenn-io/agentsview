package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// Before data version 130, a thread row could hold a revert page's messages.
// Publish that page and move its remote pins before replacing the thread row.
// If the original rollout is absent, retire the duplicate thread transcript
// after preserving its curation. The transaction also covers later batches and
// leaves the old row intact if publication fails.
func (s *Sync) migrateCodexPages(
	ctx context.Context, tx *sql.Tx, batch []db.Session,
	markerID string, legacyMarkerMachines []string,
) error {
	threads := make(map[string]db.Session)
	for _, sess := range batch {
		switch sess.Agent {
		case "codex", "traex", "augure-code":
			prefixEnd := strings.LastIndex(sess.ID, ":") + 1
			if prefixEnd > 0 {
				id := sess.ID[:prefixEnd] + parser.CodexThreadIDFromSessionKey(sess.ID[prefixEnd:])
				// A rebuilt trash anchor has no file and may still have an old
				// data version, even after restore. Migrate before publishing its
				// empty transcript when the retained page is in a later batch.
				if sess.ID == id && sess.DataVersion < 130 && sess.FilePath != nil {
					continue
				}
				threads[id] = sess
			}
		}
	}
	if len(threads) == 0 {
		return nil
	}
	// Purge inventories pages under the thread lock. Keep that same lock
	// through publication, including after the thread's migration or retirement.
	rows, err := tx.QueryContext(ctx, `
		SELECT id, COALESCE(file_path, ''), agent, machine, owner_marker, source_archive_id, data_version
		FROM sessions
		WHERE id = ANY($1) AND provenance_kind = 'legacy'
		ORDER BY id FOR UPDATE`, mapKeys(threads))
	if err != nil {
		return fmt.Errorf("finding legacy Codex pages: %w", err)
	}
	defer rows.Close()
	type legacyPage struct {
		id, path, agent, machine, owner, archive string
		dataVersion                              int
	}
	var pages []legacyPage
	for rows.Next() {
		var page legacyPage
		if err := rows.Scan(&page.id, &page.path, &page.agent, &page.machine, &page.owner, &page.archive, &page.dataVersion); err != nil {
			return fmt.Errorf("reading legacy Codex pages: %w", err)
		}
		pages = append(pages, page)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading legacy Codex pages: %w", err)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, old := range pages {
		if old.dataVersion >= 130 || old.path == "" {
			continue
		}
		if !sameSessionOwner(
			old.owner, old.machine, markerID,
			db.MirroredSessionMachine(threads[old.id], s.machine), legacyMarkerMachines,
		) {
			continue
		}
		prefixEnd := strings.LastIndex(old.id, ":") + 1
		key := parser.CodexSessionUUIDFromFilename(old.path[strings.LastIndexAny(old.path, `/\`)+1:])
		if key == "" || key == old.id[prefixEnd:] || parser.CodexThreadIDFromSessionKey(key) != old.id[prefixEnd:] {
			continue
		}
		head, err := s.local.GetArtifactExportSession(ctx, old.id)
		if err != nil {
			return fmt.Errorf("reading original Codex rollout: %w", err)
		}
		pins, err := snapshotPinnedMessages(ctx, tx, old.id)
		if err != nil {
			return err
		}
		// An unpinned thread with a real original rollout is replaced normally,
		// even if the old page has since been removed or filtered out locally.
		hasHead := head != nil && head.FilePath != nil
		if hasHead && len(pins) == 0 {
			continue
		}
		if old.archive != "" && old.archive != s.archiveID {
			return errors.New("cannot migrate Codex pages from another source archive")
		}
		pageID := old.id[:prefixEnd] + key
		// Keep parser-owned names separate from user renames when publishing.
		page, err := s.local.GetArtifactExportSession(ctx, pageID)
		if err != nil {
			return fmt.Errorf("reading retained Codex page: %w", err)
		}
		if page == nil || page.Agent != old.agent {
			return fmt.Errorf("cannot replace legacy Codex thread: retained page %s is unavailable", pageID)
		}
		if projectFailsFilter(page.Project, s.projects, s.excludeProjects) {
			return fmt.Errorf("preserving the Codex page requires including project %q in the push", page.Project)
		}
		var pageExists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM sessions WHERE id = $1)`, pageID).Scan(&pageExists); err != nil {
			return fmt.Errorf("checking retained Codex page: %w", err)
		}
		// Apply a selected empty anchor's current trash scope before the page
		// can inherit it. Its messages remain available for pin matching.
		if head != nil && !hasHead && slices.ContainsFunc(batch, func(sess db.Session) bool { return sess.ID == old.id }) {
			if err := s.pushSession(ctx, tx, *head, markerID, legacyMarkerMachines); err != nil {
				return fmt.Errorf("publishing Codex trash anchor metadata: %w", err)
			}
		}
		if err := s.pushSession(ctx, tx, *page, markerID, legacyMarkerMachines); err != nil {
			return fmt.Errorf("publishing retained Codex page: %w", err)
		}
		if _, err := s.pushMessages(ctx, tx, pageID, true, nil, nil); err != nil {
			return err
		}
		if _, err := s.pushSecretFindings(ctx, tx, pageID); err != nil {
			return err
		}
		if err := restorePinnedMessages(ctx, tx, pageID, pins); err != nil {
			return err
		}
		// SQLite may keep a thread row without a file as a trash-scope anchor.
		// It does not represent a surviving original rollout.
		if !hasHead {
			if err := retireCodexPageThread(ctx, tx, old.id, pageID, pageExists); err != nil {
				return err
			}
		}
	}
	return nil
}

func retireCodexPageThread(ctx context.Context, tx *sql.Tx, threadID, pageID string, pageExists bool) error {
	// Existing page curation is newer than the old thread projection. For a
	// newly published page, transfer remote overrides while keeping the page's
	// source baselines, so the next push does not undo those choices.
	if !pageExists {
		if _, err := tx.ExecContext(ctx, `
			UPDATE sessions page SET
				display_name = CASE WHEN NOT page.prompt_evidence_discarded
					AND old.display_name IS DISTINCT FROM old.source_display_name
					THEN old.display_name ELSE page.display_name END,
				deleted_at = CASE WHEN old.deleted_at IS DISTINCT FROM old.source_deleted_at
					THEN old.deleted_at ELSE page.deleted_at END,
				deletion_cause = CASE WHEN old.deleted_at IS DISTINCT FROM old.source_deleted_at
					THEN old.deletion_cause ELSE page.deletion_cause END
			FROM sessions old WHERE page.id = $1 AND old.id = $2`, pageID, threadID); err != nil {
			return fmt.Errorf("moving Codex page curation: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO starred_sessions (session_id, created_at)
			SELECT $1, created_at FROM starred_sessions WHERE session_id = $2
			ON CONFLICT (session_id) DO NOTHING`, pageID, threadID); err != nil {
			return fmt.Errorf("moving Codex page star: %w", err)
		}
	}
	if err := clearSessionVectorsTx(ctx, tx, threadID); err != nil {
		return err
	}
	// Cascades discard all duplicate content and derived usage. A trashed
	// thread-wide scope still needs an empty anchor for returning pages and
	// restore/purge; retain its ownership and source/remote trash baselines.
	_, err := tx.ExecContext(ctx, `
		WITH retired AS (DELETE FROM sessions WHERE id = $1 RETURNING *)
		INSERT INTO sessions (
			id, machine, owner_marker, project, agent, created_at,
			deleted_at, source_deleted_at, deletion_cause,
			trash_includes_codex_pages, source_trash_includes_codex_pages,
			source_archive_id, source_database_generation, data_version)
		SELECT id, machine, owner_marker, project, agent, created_at,
			deleted_at, source_deleted_at, deletion_cause,
			trash_includes_codex_pages, source_trash_includes_codex_pages,
			source_archive_id, source_database_generation, 130
		FROM retired WHERE trash_includes_codex_pages AND deleted_at IS NOT NULL`, threadID)
	if err != nil {
		return fmt.Errorf("retiring duplicate Codex thread: %w", err)
	}
	return nil
}
