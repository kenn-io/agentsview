package db

import (
	"context"
	"errors"
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
