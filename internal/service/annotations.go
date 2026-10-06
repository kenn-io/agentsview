package service

import (
	"context"
	"database/sql"
	"errors"

	"go.kenn.io/agentsview/internal/db"
)

// SessionAnnotator reads and writes caller-supplied session metadata:
// labels and launcher-supplied parent links. Both accept session IDs that
// have not synced yet. Backends without a writable archive return
// db.ErrReadOnly from the write methods.
type SessionAnnotator interface {
	SessionLabels(ctx context.Context, id string) (*db.SessionLabels, error)
	SetSessionLabels(ctx context.Context, id string, labels []string) (*db.SessionLabels, error)
	UpdateSessionLabels(
		ctx context.Context, id string, add, remove []string,
	) (*db.SessionLabels, error)
	// SessionParent returns nil when no launcher-supplied link is recorded.
	SessionParent(ctx context.Context, id string) (*db.SessionExternalParent, error)
	SetSessionParent(
		ctx context.Context, id, parentID, relationshipType string,
	) (*db.SessionExternalParent, error)
	// ClearSessionParent returns ErrNoSessionParent when no link is recorded.
	ClearSessionParent(ctx context.Context, id string) (*db.SessionExternalParent, error)
}

// ErrNoSessionParent reports that a session has no launcher-supplied parent.
var ErrNoSessionParent = errors.New("no parent link recorded for session")

var _ SessionAnnotator = (*directBackend)(nil)

func (b *directBackend) SessionLabels(
	ctx context.Context, id string,
) (*db.SessionLabels, error) {
	return labelsResult(db.ReadSessionLabels(ctx, b.db, id))
}

func (b *directBackend) SetSessionLabels(
	ctx context.Context, id string, labels []string,
) (*db.SessionLabels, error) {
	switch {
	case b.engine != nil:
		return labelsResult(b.engine.SetSessionLabels(ctx, id, labels))
	case b.local != nil:
		return labelsResult(b.local.SetSessionLabels(ctx, id, labels))
	}
	return nil, db.ErrReadOnly
}

func (b *directBackend) UpdateSessionLabels(
	ctx context.Context, id string, add, remove []string,
) (*db.SessionLabels, error) {
	switch {
	case b.engine != nil:
		return labelsResult(b.engine.UpdateSessionLabels(ctx, id, add, remove))
	case b.local != nil:
		return labelsResult(b.local.UpdateSessionLabels(ctx, id, add, remove))
	}
	return nil, db.ErrReadOnly
}

func (b *directBackend) SessionParent(
	ctx context.Context, id string,
) (*db.SessionExternalParent, error) {
	if b.local == nil {
		return nil, db.ErrReadOnly
	}
	link, err := b.local.GetSessionExternalParent(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &link, nil
}

func (b *directBackend) SetSessionParent(
	ctx context.Context, id, parentID, relationshipType string,
) (*db.SessionExternalParent, error) {
	var (
		link db.SessionExternalParent
		err  error
	)
	switch {
	case b.engine != nil:
		link, err = b.engine.SetSessionExternalParent(ctx, id, parentID, relationshipType)
	case b.local != nil:
		link, err = b.local.SetSessionExternalParent(ctx, id, parentID, relationshipType)
	default:
		return nil, db.ErrReadOnly
	}
	if err != nil {
		return nil, err
	}
	return &link, nil
}

func (b *directBackend) ClearSessionParent(
	ctx context.Context, id string,
) (*db.SessionExternalParent, error) {
	var (
		link db.SessionExternalParent
		err  error
	)
	switch {
	case b.engine != nil:
		link, err = b.engine.ClearSessionExternalParent(ctx, id)
	case b.local != nil:
		link, err = b.local.ClearSessionExternalParent(ctx, id)
	default:
		return nil, db.ErrReadOnly
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoSessionParent
	}
	if err != nil {
		return nil, err
	}
	return &link, nil
}

func labelsResult(labels db.SessionLabels, err error) (*db.SessionLabels, error) {
	if err != nil {
		return nil, err
	}
	if labels.Labels == nil {
		labels.Labels = []string{}
	}
	return &labels, nil
}
