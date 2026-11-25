package db

import (
	"context"
	"database/sql"
	"fmt"

	"go.kenn.io/agentsview/internal/parser"
)

// copyRebuiltPRLinks preserves saved usage-only links for matching and mapped
// sessions. Codex remapping applies before version 137; the repository-change
// marker was introduced in version 190.
func (d *DB) copyRebuiltPRLinks(ctx context.Context, tx *sql.Tx) error {
	if !oldDBHasTable(ctx, tx, "sessions") {
		return nil
	}
	if !d.usageOnlyStorage() {
		ids, err := sessionIDsTx(ctx, tx, "id IN (SELECT id FROM old_db.sessions) AND id IN (SELECT session_id FROM tool_calls WHERE category IN ('Bash','Task'))")
		if err != nil {
			return fmt.Errorf("listing sessions for PR link refresh: %w", err)
		}
		for _, id := range ids {
			if err := refreshSessionPromptPRLinksTx(ctx, tx, id); err != nil {
				return fmt.Errorf("refreshing copied session PR links: %w", err)
			}
		}
		return nil
	}
	column := "structured_pr_links"
	if !oldDBHasColumn(ctx, tx, "sessions", column) {
		column = "pr_links"
	}
	if !oldDBHasColumn(ctx, tx, "sessions", column) {
		return nil
	}
	marker := "0"
	if oldDBHasColumn(ctx, tx, "sessions", "pr_repository_changed") {
		marker = "old.pr_repository_changed"
	}
	rows, err := tx.QueryContext(ctx, `SELECT s.id, old.`+column+`, `+marker+` FROM old_db.sessions old JOIN main.sessions s ON s.id=COALESCE((SELECT target_id FROM _codex_page_metadata WHERE source_id=old.id), old.id) WHERE old.`+column+`<>'' OR `+marker+`<>0 ORDER BY old.id`)
	if err != nil {
		return fmt.Errorf("listing saved PR links: %w", err)
	}
	defer rows.Close()
	type savedLinks struct {
		id, links string
		changed   bool
	}
	var saved []savedLinks
	for rows.Next() {
		var entry savedLinks
		if err := rows.Scan(&entry.id, &entry.links, &entry.changed); err != nil {
			return err
		}
		saved = append(saved, entry)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, entry := range saved {
		var current string
		if err := tx.QueryRowContext(ctx, `SELECT structured_pr_links FROM sessions WHERE id=?`, entry.id).Scan(&current); err != nil {
			return err
		}
		links := EncodePRLinks(parser.CombinePRLinks(DecodePRLinks(entry.links), DecodePRLinks(current)))
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET structured_pr_links=?,pr_links=?,pr_repository_changed=MAX(pr_repository_changed,?) WHERE id=?`, links, links, entry.changed, entry.id); err != nil {
			return err
		}
	}
	return nil
}
