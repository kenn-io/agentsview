package db

import (
	"context"

	"github.com/uptrace/bun"
)

// BunBackend owns one engine's guarded Bun handle lifecycle.
type BunBackend interface {
	Name() string
	ReadOnly() bool
	Capabilities() BackendCapabilities
	View(context.Context, func(bun.IDB) error) error
	Update(context.Context, func(bun.IDB) error) error
}

// WriteOperation identifies a separately authorized family of mutations.
type WriteOperation uint8

const (
	WriteArchive WriteOperation = iota
	WriteCuration
	WriteInsight
	WriteSessionManagement
	WriteRecall
)

// BackendCapabilities describes features that cannot be inferred from a
// store's coarse public ReadOnly value.
type BackendCapabilities struct {
	Recall bool
	Writes map[WriteOperation]bool
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
		return BackendCapabilities{}
	}
	return BackendCapabilities{
		Recall: true,
		Writes: map[WriteOperation]bool{
			WriteArchive:           true,
			WriteCuration:          true,
			WriteInsight:           true,
			WriteSessionManagement: true,
			WriteRecall:            true,
		},
	}
}

func (b *sqliteBunBackend) View(
	_ context.Context, fn func(bun.IDB) error,
) error {
	b.store.connMu.RLock()
	defer b.store.connMu.RUnlock()
	return fn(b.store.bunReader)
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
