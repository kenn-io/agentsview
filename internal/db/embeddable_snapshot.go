package db

import (
	"context"
	"database/sql"
	"errors"
)

// EmbeddableSnapshot scans a stable archive while its writer is reserved.
// It is valid only inside WithEmbeddableSnapshot's callback.
type EmbeddableSnapshot interface {
	ScanEmbeddableUnits(context.Context, string, bool, int, func(EmbeddableUnit) error) (string, error)
	ScanEmbeddableUnitsForSession(context.Context, string, bool, int, func(EmbeddableUnit) error) error
}

// WithEmbeddableSnapshot reserves the SQLite writer across local validation and
// vector installation. The callback must not write this archive or use network I/O.
func (db *DB) WithEmbeddableSnapshot(ctx context.Context, fn func(EmbeddableSnapshot) error) error {
	return db.withEmbeddableSnapshot(ctx, false, fn)
}

func (db *DB) withEmbeddableSnapshot(ctx context.Context, localOnly bool, fn func(EmbeddableSnapshot) error) (retErr error) {
	if err := db.lockWriterCtx(ctx); err != nil {
		return err
	}
	defer db.mu.Unlock()
	conn, err := db.getWriter().Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	defer func() {
		_, err := conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		retErr = errors.Join(retErr, err)
	}()
	return fn(embeddableSnapshot{conn: conn, localOnly: localOnly})
}

type embeddableSnapshot struct {
	conn      *sql.Conn
	localOnly bool
}

func (s embeddableSnapshot) EmbeddingDataVersion(ctx context.Context, includeAutomated bool) (int, error) {
	return embeddingDataVersion(ctx, s.conn, includeAutomated)
}

func (s embeddableSnapshot) ScanEmbeddableUnits(ctx context.Context, since string, scope bool, limit int, fn func(EmbeddableUnit) error) (string, error) {
	return scanEmbeddableUnits(ctx, s.conn, since, "", scope, limit, s.localOnly, fn)
}

func (s embeddableSnapshot) ScanEmbeddableUnitsForSession(ctx context.Context, id string, scope bool, limit int, fn func(EmbeddableUnit) error) error {
	if id == "" {
		return errors.New("session ID is required")
	}
	_, err := scanEmbeddableUnits(ctx, s.conn, "", id, scope, limit, s.localOnly, fn)
	return err
}
