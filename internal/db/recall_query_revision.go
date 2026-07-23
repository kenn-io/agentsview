package db

import (
	"context"
	"fmt"
	"strconv"
)

const recallQueryRevisionPrefix = "query-v1:"

// RecallQueryRevision returns the monotonic revision used to keep ranked
// pagination on one stable entry-and-evidence corpus.
//
// An empty revision means the read-only archive predates query revision
// tracking. Callers may still serve one ranked page, but must not issue a
// continuation cursor they cannot validate.
func (db *DB) RecallQueryRevision(ctx context.Context) (string, error) {
	hasState, err := db.recallTableExists(ctx, "recall_query_state")
	if err != nil {
		return "", err
	}
	if !hasState {
		return "", nil
	}
	var revision int64
	if err := db.getReader().QueryRowContext(ctx, `
		SELECT revision FROM recall_query_state WHERE singleton = 1
	`).Scan(&revision); err != nil {
		return "", fmt.Errorf("reading recall query revision: %w", err)
	}
	return recallQueryRevisionPrefix + strconv.FormatInt(revision, 10), nil
}
