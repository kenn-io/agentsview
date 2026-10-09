package service

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

// SessionAnnotator writes caller-supplied session metadata: labels and
// launcher-supplied parent links. Both accept session IDs that have not
// synced yet; reads go through the session itself. Backends without a
// writable archive return db.ErrReadOnly.
type SessionAnnotator interface {
	// UpdateSessionLabels removes every label first when replace is set, then
	// applies remove and add.
	UpdateSessionLabels(
		ctx context.Context, id string, add, remove []string, replace bool,
	) (*db.SessionLabels, error)
	// SetSessionParent records a launcher parent; an empty parentID removes
	// it and returns ErrNoSessionParent when none is recorded.
	SetSessionParent(ctx context.Context, id, parentID string) (*db.SessionExternalParent, error)
}

// ErrNoSessionParent reports that a session has no launcher-supplied parent.
var ErrNoSessionParent = errors.New("no parent link recorded for session")

var _ SessionAnnotator = (*directBackend)(nil)

// annotationWriter is the write surface shared by *db.DB and *sync.Engine.
type annotationWriter interface {
	SetSessionLabels(ctx context.Context, id string, labels []string) (db.SessionLabels, error)
	UpdateSessionLabels(ctx context.Context, id string, add, remove []string) (db.SessionLabels, error)
	SetSessionExternalParent(ctx context.Context, id, parentID string) (db.SessionExternalParent, error)
	ClearSessionExternalParent(ctx context.Context, id string) (db.SessionExternalParent, error)
}

func (b *directBackend) annotationWriter() (annotationWriter, error) {
	switch {
	case b.engine != nil:
		return b.engine, nil
	case b.local != nil:
		return b.local, nil
	}
	return nil, db.ErrReadOnly
}

func (b *directBackend) UpdateSessionLabels(
	ctx context.Context, id string, add, remove []string, replace bool,
) (*db.SessionLabels, error) {
	w, err := b.annotationWriter()
	if err != nil {
		return nil, err
	}
	var labels db.SessionLabels
	if replace {
		labels, err = w.SetSessionLabels(ctx, id, add)
	} else {
		labels, err = w.UpdateSessionLabels(ctx, id, add, remove)
	}
	if err != nil {
		return nil, err
	}
	return &labels, nil
}

func (b *directBackend) SetSessionParent(
	ctx context.Context, id, parentID string,
) (*db.SessionExternalParent, error) {
	w, err := b.annotationWriter()
	if err != nil {
		return nil, err
	}
	var link db.SessionExternalParent
	if strings.TrimSpace(parentID) == "" {
		link, err = w.ClearSessionExternalParent(ctx, id)
	} else {
		link, err = w.SetSessionExternalParent(ctx, id, parentID)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoSessionParent
	}
	if err != nil {
		return nil, err
	}
	return &link, nil
}
