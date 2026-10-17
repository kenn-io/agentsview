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

// Before data version 131, a thread row could hold a revert page's messages.
// Publish that page and move its remote pins before replacing the thread row.
// If the original rollout is absent, retire the duplicate thread transcript
// after preserving its curation. The transaction also covers later batches and
// leaves the old row intact if publication fails. Before data version 134, a
// page whose header named its own rollout was stored under that rollout id; it
// retires into its canonical page the same way.
func (s *Sync) migrateCodexPages(
	ctx context.Context, tx *sql.Tx, batch []db.Session,
	markerID string, legacyMarkerMachines []string,
) error {
	threads := make(map[string]db.Session)
	byFile := make(map[string]db.Session)
	var files []string
	for _, sess := range batch {
		switch sess.Agent {
		case "codex", "traex", "augure-code":
			prefixEnd := strings.LastIndex(sess.ID, ":") + 1
			if prefixEnd > 0 {
				id := sess.ID[:prefixEnd] + parser.CodexThreadIDFromSessionKey(sess.ID[prefixEnd:])
				// A rebuilt trash anchor has no file and may still have an old
				// data version, even after restore. Migrate before publishing its
				// empty transcript when the retained page is in a later batch.
				if sess.ID == id && sess.DataVersion < 131 && sess.FilePath != nil {
					continue
				}
				threads[id] = sess
				if sess.FilePath != nil {
					path := strings.TrimSuffix((*sess.FilePath)[strings.LastIndexAny(*sess.FilePath, `/\`)+1:], ".zst")
					byFile[path] = sess
					files = append(files, path)
				}
				// Also key by the id a legacy page held before data version 134
				// (the filename without its thread). The old data version is
				// only known after the query below, so this stays unrestricted;
				// the query and the per-page checks filter it.
				if thread := parser.CodexThreadIDFromSessionKey(sess.ID[prefixEnd:]); thread != sess.ID[prefixEnd:] {
					threads[sess.ID[:prefixEnd]+sess.ID[prefixEnd+len(thread)+1:]] = sess
				}
			}
		}
	}
	if len(threads) == 0 {
		return nil
	}
	// Purge inventories pages under the thread lock. Keep that same lock
	// through publication, including after the thread's migration or retirement.
	pages, err := s.lockCodexCorrectionChain(ctx, tx, threads, byFile, files)
	if err != nil {
		return err
	}
	// Corrections can chain (A to B and B to C), so publishing one page may
	// replace another source's row. Capture every source's pins first.
	pagePins, err := snapshotLegacyCodexPagePins(ctx, tx, pages)
	if err != nil {
		return err
	}
	var migrations []codexPageMigration
	for _, old := range pages {
		migration, err := s.prepareCodexPageMigration(ctx, tx, old, batch, threads, pagePins[old.id], markerID, legacyMarkerMachines)
		if err != nil {
			return err
		}
		if migration != nil {
			migrations = append(migrations, *migration)
		}
	}
	return s.publishCodexPageMigrations(ctx, tx, migrations, batch, markerID, legacyMarkerMachines)
}

type legacyCodexPage struct {
	id, path, agent, machine, owner, archive string
	dataVersion                              int
}

// lockLegacyCodexPages locks the legacy rows a batch may migrate: rows under a
// selected thread or legacy page ID, and pre-137 rows filed under a selected
// rollout. A row found by its rollout joins threads under that rollout's
// session.
func lockLegacyCodexPages(
	ctx context.Context, tx *sql.Tx, threads, byFile map[string]db.Session, files []string,
) ([]legacyCodexPage, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, COALESCE(file_path, ''), agent, machine, owner_marker, source_archive_id, data_version
		FROM sessions
		WHERE provenance_kind = 'legacy'
		  AND (id = ANY($1) OR (data_version < 137 AND regexp_replace(regexp_replace(replace(file_path, chr(92), '/'), '^.*/', ''), '\.zst$', '') = ANY($2)))
		ORDER BY id FOR UPDATE`, mapKeys(threads), files)
	if err != nil {
		return nil, fmt.Errorf("finding legacy Codex pages: %w", err)
	}
	defer rows.Close()
	var pages []legacyCodexPage
	for rows.Next() {
		var page legacyCodexPage
		if err := rows.Scan(&page.id, &page.path, &page.agent, &page.machine, &page.owner, &page.archive, &page.dataVersion); err != nil {
			return nil, fmt.Errorf("reading legacy Codex pages: %w", err)
		}
		if _, selected := threads[page.id]; !selected {
			threads[page.id] = byFile[strings.TrimSuffix(page.path[strings.LastIndexAny(page.path, `/\`)+1:], ".zst")]
		}
		pages = append(pages, page)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading legacy Codex pages: %w", err)
	}
	return pages, rows.Close()
}

// snapshotLegacyCodexPagePins captures each locked legacy row's pins before
// any page is published.
func snapshotLegacyCodexPagePins(
	ctx context.Context, tx *sql.Tx, pages []legacyCodexPage,
) (map[string][]savedPostgresPin, error) {
	pins := make(map[string][]savedPostgresPin, len(pages))
	for _, page := range pages {
		saved, err := snapshotPinnedMessages(ctx, tx, page.id)
		if err != nil {
			return nil, err
		}
		pins[page.id] = saved
	}
	return pins, nil
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
			source_archive_id, source_database_generation, 131
		FROM retired WHERE trash_includes_codex_pages AND deleted_at IS NOT NULL`, threadID)
	if err != nil {
		return fmt.Errorf("retiring duplicate Codex thread: %w", err)
	}
	return nil
}

type codexPageMigration struct {
	old                 legacyCodexPage
	page                db.Session
	head                *db.Session
	pins                []savedPostgresPin
	hasHead, pageExists bool
}

// Every outgoing pin is removed before any destination can reuse its ordinal.
// Restore only after all message writes, so later corrections cannot move it again.
func (s *Sync) publishCodexPageMigrations(
	ctx context.Context, tx *sql.Tx, migrations []codexPageMigration, batch []db.Session,
	markerID string, legacyMarkerMachines []string,
) error {
	for _, migration := range migrations {
		if err := clearSnapshottedPins(ctx, tx, migration.pins); err != nil {
			return err
		}
	}
	for _, migration := range migrations {
		if err := s.publishCodexPageMigration(ctx, tx, migration, batch, markerID, legacyMarkerMachines); err != nil {
			return err
		}
	}
	for _, migration := range migrations {
		if err := restorePinnedMessages(ctx, tx, migration.page.ID, migration.pins); err != nil {
			return err
		}
	}
	return nil
}

func (old legacyCodexPage) correction() (id string, threadShape bool) {
	prefixEnd := strings.LastIndex(old.id, ":") + 1
	key := parser.CodexSessionUUIDFromPath(old.path)
	oldKey := old.id[prefixEnd:]
	thread := parser.CodexThreadIDFromSessionKey(key)
	threadShape = old.dataVersion < 131 && thread == oldKey
	rolloutShape := old.dataVersion < 134 && thread != key && key == thread+"_"+oldKey
	misfiled := old.dataVersion < 137 && thread == key
	if old.path == "" || key == "" || key == oldKey || !threadShape && !rolloutShape && !misfiled {
		return "", false
	}
	return old.id[:prefixEnd] + key, threadShape
}

// Follow destinations before publication can overwrite a legacy source row.
func (s *Sync) lockCodexCorrectionChain(
	ctx context.Context, tx *sql.Tx, threads, byFile map[string]db.Session, files []string,
) ([]legacyCodexPage, error) {
	for {
		pages, err := lockLegacyCodexPages(ctx, tx, threads, byFile, files)
		if err != nil {
			return nil, err
		}
		expanded := false
		for _, old := range pages {
			id, _ := old.correction()
			if _, seen := threads[id]; id == "" || seen {
				continue
			}
			page, err := s.local.GetArtifactExportSession(ctx, id)
			if err != nil {
				return nil, fmt.Errorf("reading chained Codex destination: %w", err)
			}
			if page == nil || page.Agent != old.agent {
				continue
			}
			threads[id] = *page
			expanded = true
		}
		if !expanded {
			return pages, nil
		}
	}
}

func (s *Sync) prepareCodexPageMigration(
	ctx context.Context, tx *sql.Tx, old legacyCodexPage, batch []db.Session,
	threads map[string]db.Session, pins []savedPostgresPin, markerID string, legacyMarkerMachines []string,
) (*codexPageMigration, error) {
	syncScope := s.isFiltered() && !slices.ContainsFunc(batch, func(sess db.Session) bool { return sess.ID == old.id })
	pageID, threadShape := old.correction()
	migratePage := pageID != ""
	if migratePage && !threadShape {
		// The filename alone cannot tell a legacy page from one the parser
		// rejected (header session_id differs from the filename thread). The
		// local archive must hold the destination for a legacy page or a
		// misfiled thread before its remote pins can move there.
		canonical, err := s.local.GetArtifactExportSession(ctx, pageID)
		if err != nil {
			return nil, fmt.Errorf("reading retained Codex page: %w", err)
		}
		migratePage = canonical != nil && canonical.Agent == old.agent
	}
	if !syncScope && !migratePage {
		return nil, nil
	}
	if !sameSessionOwner(
		old.owner, old.machine, markerID,
		db.MirroredSessionMachine(threads[old.id], s.machine), legacyMarkerMachines,
	) {
		return nil, nil
	}
	head, err := s.local.GetArtifactExportSession(ctx, old.id)
	if err != nil {
		return nil, fmt.Errorf("reading original Codex rollout: %w", err)
	}
	if err := s.syncCodexPageTrashScope(ctx, tx, old, head, threads[old.id], syncScope); err != nil {
		return nil, err
	}
	if !migratePage {
		return nil, nil
	}

	return s.prepareCodexPageDestination(ctx, tx, old, head, pins, pageID)
}

func (s *Sync) prepareCodexPageDestination(
	ctx context.Context, tx *sql.Tx, old legacyCodexPage, head *db.Session,
	pins []savedPostgresPin, pageID string,
) (*codexPageMigration, error) {
	// An unpinned thread with a real original rollout is replaced normally,
	// even if the old page has since been removed or filtered out locally.
	hasHead := head != nil && head.FilePath != nil
	if hasHead && len(pins) == 0 {
		return nil, nil
	}
	if old.archive != "" && old.archive != s.archiveID {
		return nil, errors.New("cannot migrate Codex pages from another source archive")
	}
	// Keep parser-owned names separate from user renames when publishing.
	page, err := s.local.GetArtifactExportSession(ctx, pageID)
	if err != nil {
		return nil, fmt.Errorf("reading retained Codex page: %w", err)
	}
	if page == nil || page.Agent != old.agent {
		return nil, fmt.Errorf("cannot replace legacy Codex thread: retained page %s is unavailable", pageID)
	}
	if projectFailsFilter(page.Project, s.projects, s.excludeProjects) {
		return nil, fmt.Errorf("preserving the Codex page requires including project %q in the push", page.Project)
	}
	var pageExists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM sessions WHERE id = $1)`, pageID).Scan(&pageExists); err != nil {
		return nil, fmt.Errorf("checking retained Codex page: %w", err)
	}
	return &codexPageMigration{old: old, page: *page, head: head, pins: pins, hasHead: hasHead, pageExists: pageExists}, nil
}

func (s *Sync) syncCodexPageTrashScope(ctx context.Context, tx *sql.Tx, old legacyCodexPage, head *db.Session, selected db.Session, syncScope bool) error {
	if syncScope && head != nil && head.Agent == old.agent && selected.Agent == old.agent &&
		(old.archive == "" || old.archive == s.archiveID) && projectFailsFilter(head.Project, s.projects, s.excludeProjects) {
		// A filtered page restore still ends its source's thread scope.
		// Publish that dependency without the excluded project's content.
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET
			trash_includes_codex_pages = CASE
				WHEN deleted_at IS DISTINCT FROM source_deleted_at
					OR trash_includes_codex_pages IS DISTINCT FROM source_trash_includes_codex_pages
				THEN trash_includes_codex_pages ELSE $2 END,
			source_trash_includes_codex_pages = $2, updated_at = NOW()
			WHERE id = $1 AND source_trash_includes_codex_pages IS DISTINCT FROM $2`, old.id, head.TrashIncludesCodexPages); err != nil {
			return fmt.Errorf("publishing Codex source trash scope: %w", err)
		}
	}
	return nil
}

func (s *Sync) publishCodexPageMigration(
	ctx context.Context, tx *sql.Tx, migration codexPageMigration, batch []db.Session,
	markerID string, legacyMarkerMachines []string,
) error {
	// Apply a selected empty anchor's current trash scope before the page
	// can inherit it. Its messages remain available for pin matching.
	if migration.head != nil && !migration.hasHead && slices.ContainsFunc(batch, func(sess db.Session) bool { return sess.ID == migration.old.id }) {
		if err := s.pushSession(ctx, tx, *migration.head, markerID, legacyMarkerMachines); err != nil {
			return fmt.Errorf("publishing Codex trash anchor metadata: %w", err)
		}
	}
	if err := s.pushSession(ctx, tx, migration.page, markerID, legacyMarkerMachines); err != nil {
		return fmt.Errorf("publishing retained Codex page: %w", err)
	}
	if _, err := s.pushMessages(ctx, tx, migration.page.ID, true, nil, nil); err != nil {
		return err
	}
	if _, err := s.pushSecretFindings(ctx, tx, migration.page.ID); err != nil {
		return err
	}
	// The page row now carries current friction metadata, so the batch
	// loop would treat its findings as already pushed.
	if _, err := s.pushFrictionFindings(ctx, tx, migration.page.ID); err != nil {
		return err
	}
	// SQLite may keep a thread row without a file as a trash-scope anchor.
	// It does not represent a surviving original rollout.
	if !migration.hasHead {
		if err := retireCodexPageThread(ctx, tx, migration.old.id, migration.page.ID, migration.pageExists); err != nil {
			return err
		}
	}
	return nil
}
