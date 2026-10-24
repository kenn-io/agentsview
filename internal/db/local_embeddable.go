package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
)

// LocalEmbeddableSource excludes remote mirrors, including during reserved scans.
// Ownership follows the archive's installation identity and legacy local key.
func (db *DB) LocalEmbeddableSource() localEmbeddableSource { return localEmbeddableSource{db} }

type localEmbeddableSource struct{ db *DB }

func (s localEmbeddableSource) ScanEmbeddableUnits(ctx context.Context, since string, scope bool, maxRunes int, fn func(EmbeddableUnit) error) (string, error) {
	release, err := s.db.acquireBulkRead(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	return scanEmbeddableUnits(ctx, s.db.getReader(), since, "", scope, maxRunes, true, fn)
}

func (s localEmbeddableSource) ScanEmbeddableUnitsForSession(ctx context.Context, id string, scope bool, maxRunes int, fn func(EmbeddableUnit) error) error {
	if id == "" {
		return errors.New("session ID is required")
	}
	_, err := scanEmbeddableUnits(ctx, s.db.getReader(), "", id, scope, maxRunes, true, fn)
	return err
}

func (s localEmbeddableSource) WithEmbeddableSnapshot(ctx context.Context, fn func(EmbeddableSnapshot) error) error {
	return s.db.withEmbeddableSnapshot(ctx, true, fn)
}

func (s localEmbeddableSource) GetDatabaseID(ctx context.Context) (string, error) {
	return s.db.GetDatabaseID(ctx)
}

func (s localEmbeddableSource) SessionDeletionPublicationRevision(ctx context.Context) (int64, error) {
	return s.db.SessionDeletionPublicationRevision(ctx)
}

func (s localEmbeddableSource) LoadSessionDeletionChanges(ctx context.Context, after, through int64) ([]string, error) {
	return s.db.LoadSessionDeletionChanges(ctx, after, through)
}

func (s localEmbeddableSource) EmbeddingDataVersion(ctx context.Context, scope bool) (int, error) {
	return s.db.EmbeddingDataVersion(ctx, scope)
}

// EmbeddingCorpusFingerprint identifies corpus membership independently of the
// embedding recipe and the raw archive identity.
func (db *DB) EmbeddingCorpusFingerprint(ctx context.Context) (string, error) {
	return embeddingCorpusFingerprint(ctx, db.getReader(), false)
}

func (s localEmbeddableSource) EmbeddingCorpusFingerprint(ctx context.Context) (string, error) {
	return embeddingCorpusFingerprint(ctx, s.db.getReader(), true)
}

func embeddingCorpusFingerprint(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, localOnly bool,
) (string, error) {
	if !localOnly {
		return "archive:all:v1", nil
	}
	// These are the same two ownership aliases used by the local-only scan.
	// Hashing them also reconciles if this archive's installation identity changes.
	rows, err := q.QueryContext(ctx, `SELECT key, value FROM pg_sync_state
 WHERE key IN ('artifact_local_machine_name', 'artifact_local_installation_id') ORDER BY key`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	h := sha256.New()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%q:%q;", key, value)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return fmt.Sprintf("archive:local:v1:%x", h.Sum(nil)), nil
}
