package db

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db/bunmodel"
)

// BunBackend owns one engine's guarded Bun handle lifecycle. ConsistentView
// callbacks may be replayed by adapters that cannot hold one remote snapshot;
// callbacks must stage results and publish them only after ConsistentView
// returns successfully.
type BunBackend interface {
	Name() string
	ReadOnly() bool
	Capabilities() BackendCapabilities
	SessionQueryDialect() QueryDialect
	SessionVersion(context.Context, bun.IDB, string) (int, int64, error)
	View(context.Context, func(bun.IDB) error) error
	ConsistentView(context.Context, func(bun.IDB) error) error
	Update(context.Context, func(bun.IDB) error) error
}

type bunSessionFullHydrator interface {
	HydrateSessionFull(context.Context, bun.IDB, *Session) error
}

// BunTablePresenceProbe is implemented by adapters whose compatible read-only
// schemas may omit optional canonical tables.
type BunTablePresenceProbe interface {
	BunTableExists(context.Context, bun.IDB, string) (bool, error)
}

// WriteOperation identifies a separately authorized family of mutations.
type WriteOperation uint8

const (
	WriteArchive WriteOperation = iota
	WriteCuration
	WriteInsight // insertion/generation
	WriteInsightDelete
	WriteSessionManagement
	WriteRecall
)

// BackendCapabilities describes features that cannot be inferred from a
// store's coarse public ReadOnly value.
type BackendCapabilities struct {
	Recall           bool
	FullText         FullTextCapability
	Writes           map[WriteOperation]bool
	SessionMutations SessionMutationAdapter
}

// SessionMutationAdapter owns engine-specific timestamp expressions and
// transaction-scoped side effects around the canonical session mutations.
type SessionMutationAdapter interface {
	ApplyTouch(*bun.UpdateQuery, bunmodel.Timestamp)
	ApplySoftDelete(*bun.UpdateQuery, bunmodel.Timestamp)
	AfterRestore(context.Context, bun.Tx, string) error
	BeforeDelete(context.Context, bun.Tx, []string) error
}

// AllowsWrite reports whether an operation family is authorized.
func (c BackendCapabilities) AllowsWrite(operation WriteOperation) bool {
	return c.Writes[operation]
}

type sqliteBunBackend struct {
	store *DB
}

var _ BunBackend = (*sqliteBunBackend)(nil)

func (*sqliteBunBackend) Name() string { return "sqlite" }

func (b *sqliteBunBackend) ReadOnly() bool { return b.store.readOnly }

func (b *sqliteBunBackend) Capabilities() BackendCapabilities {
	if b.store.readOnly {
		return BackendCapabilities{
			Recall: true, FullText: sqliteFullTextCapability{store: b.store},
		}
	}
	return BackendCapabilities{
		Recall:           true,
		FullText:         sqliteFullTextCapability{store: b.store},
		SessionMutations: sqliteSessionMutationAdapter{},
		Writes: map[WriteOperation]bool{
			WriteArchive:           true,
			WriteCuration:          true,
			WriteInsight:           true,
			WriteInsightDelete:     true,
			WriteSessionManagement: true,
			WriteRecall:            true,
		},
	}
}

func (*sqliteBunBackend) BunTableExists(
	ctx context.Context, store bun.IDB, table string,
) (bool, error) {
	var exists bool
	err := store.NewRaw(`SELECT EXISTS (
		SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?
	)`, table).Scan(ctx, &exists)
	return exists, err
}

type sqliteSessionMutationAdapter struct{}

func (sqliteSessionMutationAdapter) ApplyTouch(
	query *bun.UpdateQuery, now bunmodel.Timestamp,
) {
	query.Set("local_modified_at = ?", now)
}

func (adapter sqliteSessionMutationAdapter) ApplySoftDelete(
	query *bun.UpdateQuery, now bunmodel.Timestamp,
) {
	query.Set("deleted_at = ?", now)
	adapter.ApplyTouch(query, now)
}

func (sqliteSessionMutationAdapter) AfterRestore(
	ctx context.Context, tx bun.Tx, id string,
) error {
	if _, err := tx.NewDelete().Table("local_session_source_baselines").
		Where("session_id = ?", id).Exec(ctx); err != nil {
		return fmt.Errorf("clearing restored session %s source baseline: %w", id, err)
	}
	return nil
}

func (sqliteSessionMutationAdapter) BeforeDelete(
	_ context.Context, tx bun.Tx, ids []string,
) error {
	for _, id := range ids {
		if err := deleteSessionMessagesTx(tx.Tx, id); err != nil {
			return fmt.Errorf("pre-deleting session %s messages: %w", id, err)
		}
	}
	return nil
}

func (*sqliteBunBackend) SessionQueryDialect() QueryDialect {
	return SQLiteBunSessionQueryDialect()
}

func (*sqliteBunBackend) SessionVersion(
	ctx context.Context, store bun.IDB, id string,
) (int, int64, error) {
	return FileSessionVersion(ctx, store, id)
}

func (*sqliteBunBackend) HydrateSessionFull(
	ctx context.Context, store bun.IDB, session *Session,
) error {
	var operational struct {
		NextOrdinal          int     `bun:"next_ordinal"`
		LastEntryUUID        *string `bun:"last_entry_uuid"`
		ClaudeLinearParse    *bool   `bun:"claude_linear_parse"`
		LastWriteIncremental bool    `bun:"last_write_incremental"`
	}
	if err := store.NewSelect().Table("sessions").
		Column("next_ordinal", "last_entry_uuid", "claude_linear_parse", "last_write_incremental").
		Where("id = ?", session.ID).Scan(ctx, &operational); err != nil {
		return err
	}
	session.NextOrdinal = operational.NextOrdinal
	session.LastEntryUUID = operational.LastEntryUUID
	session.ClaudeLinearParse = operational.ClaudeLinearParse
	session.LastWriteIncremental = operational.LastWriteIncremental
	return nil
}

func (b *sqliteBunBackend) View(
	_ context.Context, fn func(bun.IDB) error,
) error {
	b.store.connMu.RLock()
	defer b.store.connMu.RUnlock()
	return fn(b.store.bunReader)
}

func (b *sqliteBunBackend) ConsistentView(
	ctx context.Context, fn func(bun.IDB) error,
) error {
	b.store.connMu.RLock()
	defer b.store.connMu.RUnlock()
	return b.store.bunReader.RunInTx(
		ctx, &sql.TxOptions{ReadOnly: true},
		func(_ context.Context, tx bun.Tx) error { return fn(tx) },
	)
}

func (b *sqliteBunBackend) Update(
	_ context.Context, fn func(bun.IDB) error,
) error {
	b.store.mu.Lock()
	defer b.store.mu.Unlock()
	b.store.connMu.RLock()
	defer b.store.connMu.RUnlock()
	if b.store.readOnly {
		return ErrReadOnly
	}
	if b.store.bunWriter == nil {
		if b.store.writerClosed.Load() {
			return ErrWriterClosed
		}
		return ErrReadOnly
	}
	return fn(b.store.bunWriter)
}
