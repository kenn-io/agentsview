package service

import (
	"context"

	"go.kenn.io/agentsview/internal/db"
)

// DiagnosticAppender records failures outside agent sessions.
type DiagnosticAppender interface {
	AppendDiagnostic(context.Context, db.DiagnosticEvent) (db.DiagnosticEvent, error)
}

func (b *directBackend) AppendDiagnostic(ctx context.Context, e db.DiagnosticEvent) (db.DiagnosticEvent, error) {
	if b.local == nil {
		return db.DiagnosticEvent{}, db.ErrReadOnly
	}
	return b.local.AppendDiagnostic(ctx, e)
}
