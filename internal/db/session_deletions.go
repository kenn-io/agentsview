package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const archiveMetadataSessionDeletionRevisionKey = "session_deletion_publication_revision"

// SessionDeletionPublicationRevision is an O(1) change token for hard
// session deletions, advanced by SQLite triggers.
func (db *DB) SessionDeletionPublicationRevision(ctx context.Context) (int64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var raw string
	err := db.getReader().QueryRowContext(ctx,
		`SELECT value FROM archive_metadata WHERE key = ?`,
		archiveMetadataSessionDeletionRevisionKey,
	).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading session deletion publication revision: %w", err)
	}
	revision, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || revision < 0 {
		return 0, fmt.Errorf("invalid session deletion publication revision %q", raw)
	}
	return revision, nil
}

// SessionDeletionTombstone identifies one hard-deleted session a mirror
// must remove. Project is retained so filtered mirrors can skip
// out-of-scope tombstones.
type SessionDeletionTombstone struct {
	SessionID string
	Project   string
}

// LoadSessionDeletionDelta returns tombstones journaled in
// (afterRevision, throughRevision], filtered to the given project scope.
func (db *DB) LoadSessionDeletionDelta(
	ctx context.Context, afterRevision, throughRevision int64,
	projects, excludeProjects []string,
) ([]SessionDeletionTombstone, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if afterRevision < 0 || throughRevision < afterRevision {
		return nil, fmt.Errorf(
			"invalid session deletion publication window (%d, %d]",
			afterRevision, throughRevision,
		)
	}
	if afterRevision == throughRevision {
		return nil, nil
	}

	where, args := sessionDeletionPublicationChangeWhere(
		"c", afterRevision, throughRevision, projects, excludeProjects,
	)
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT c.session_id, c.project
		FROM session_deletion_changes c
		`+where+` AND c.deleted = 1
		ORDER BY c.revision`, args...)
	if err != nil {
		return nil, fmt.Errorf("listing session deletion tombstones: %w", err)
	}
	defer rows.Close()

	var tombstones []SessionDeletionTombstone
	for rows.Next() {
		var t SessionDeletionTombstone
		if err := rows.Scan(&t.SessionID, &t.Project); err != nil {
			return nil, fmt.Errorf("scanning session deletion tombstone: %w", err)
		}
		tombstones = append(tombstones, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating session deletion tombstones: %w", err)
	}
	return tombstones, nil
}

func sessionDeletionPublicationChangeWhere(
	alias string,
	afterRevision, throughRevision int64,
	projects, excludeProjects []string,
) (string, []any) {
	where := "WHERE " + alias + ".revision > ? AND " + alias + ".revision <= ?"
	args := []any{afterRevision, throughRevision}
	appendProjects := func(values []string, negate bool) {
		if len(values) == 0 {
			return
		}
		placeholders := make([]string, len(values))
		for i, value := range values {
			placeholders[i] = "?"
			args = append(args, value)
		}
		op := " IN "
		if negate {
			op = " NOT IN "
		}
		where += " AND " + alias + ".project" + op +
			"(" + strings.Join(placeholders, ",") + ")"
	}
	appendProjects(projects, false)
	appendProjects(excludeProjects, true)
	return where, args
}
