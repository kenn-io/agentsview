package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

func replacePGSessionAliases(
	ctx context.Context, tx *sql.Tx, sess db.Session,
) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM session_aliases WHERE session_id = $1`,
		sess.ID,
	); err != nil {
		return fmt.Errorf("deleting pg session aliases for %s: %w", sess.ID, err)
	}
	for _, aliasID := range pgSessionAliasIDs(sess) {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO session_aliases (session_id, alias_id)
			 VALUES ($1, $2)
			 ON CONFLICT (session_id, alias_id) DO NOTHING`,
			sess.ID, aliasID,
		); err != nil {
			return fmt.Errorf(
				"storing pg session alias %s for %s: %w",
				aliasID, sess.ID, err,
			)
		}
	}
	return nil
}

func pgSessionAliasIDs(sess db.Session) []string {
	if sess.FilePath == nil {
		return nil
	}
	aliasID := pgVibeFallbackAliasID(sess.ID, sess.Agent, *sess.FilePath)
	if aliasID == "" {
		return nil
	}
	return []string{aliasID}
}

func pgSessionTombstoneIDs(sess db.Session) []string {
	ids := append([]string{sess.ID}, pgSessionAliasIDs(sess)...)
	return uniqueNonEmptyStrings(ids)
}

func pgVibeFallbackAliasID(id, agent, filePath string) string {
	if agent != "vibe" || filePath == "" {
		return ""
	}
	dir := filepath.Base(filepath.Dir(filePath))
	if !strings.HasPrefix(dir, "session_") {
		return ""
	}
	fallbackID := "vibe:" + dir
	if idx := strings.LastIndex(id, "vibe:"); idx > 0 {
		fallbackID = id[:idx] + fallbackID
	}
	if fallbackID == id {
		return ""
	}
	return fallbackID
}

func insertPGExcludedSessionIDs(
	ctx context.Context, execer pgSessionExecer, ids []string,
) error {
	ids = uniqueNonEmptyStrings(ids)
	if len(ids) == 0 {
		return nil
	}
	if _, err := execer.ExecContext(ctx,
		`INSERT INTO excluded_sessions (id)
		 SELECT unnest($1::text[])
		 ON CONFLICT (id) DO NOTHING`,
		ids,
	); err != nil {
		return fmt.Errorf("recording pg excluded session ids: %w", err)
	}
	return nil
}

func uniqueNonEmptyStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

// Record thread scope before exact aliases, so a later per-file tombstone
// cannot narrow an existing permanent whole-thread exclusion.
func insertPGTrashedSessionExclusions(ctx context.Context, tx *sql.Tx, ids, scopedIDs []string) error {
	if len(scopedIDs) > 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO excluded_sessions (id, include_codex_pages, owner_marker, machine, agent)
			SELECT id, TRUE, owner_marker, machine, agent FROM sessions WHERE id = ANY($1)
			ON CONFLICT (id) DO UPDATE SET
				owner_marker = CASE WHEN excluded_sessions.include_codex_pages
					THEN excluded_sessions.owner_marker ELSE EXCLUDED.owner_marker END,
				machine = CASE WHEN excluded_sessions.include_codex_pages
					THEN excluded_sessions.machine ELSE EXCLUDED.machine END,
				agent = CASE WHEN excluded_sessions.include_codex_pages
					THEN excluded_sessions.agent ELSE EXCLUDED.agent END,
				include_codex_pages = TRUE`, scopedIDs); err != nil {
			return fmt.Errorf("recording excluded Codex threads: %w", err)
		}
	}
	return insertPGExcludedSessionIDs(ctx, tx, ids)
}
