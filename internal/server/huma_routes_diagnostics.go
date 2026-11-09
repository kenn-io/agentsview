package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

type diagnosticAppendInput struct {
	Body struct {
		ID         string     `json:"id" required:"true" format:"uuid"`
		Source     string     `json:"source" required:"true" minLength:"1" maxLength:"64"`
		Message    string     `json:"message" required:"true" minLength:"1" maxLength:"500"`
		OccurredAt *time.Time `json:"occurred_at,omitempty"`
	}
}

func (s *Server) humaAppendDiagnostic(ctx context.Context, in *diagnosticAppendInput) (*jsonOutput[db.DiagnosticEvent], error) {
	if !s.frictionRunAllowed(ctx) {
		return nil, apiError(http.StatusForbidden, "diagnostic writes require auth or a localhost request")
	}
	local, ok := s.db.(*db.DB)
	if !ok || local.ReadOnly() || local.ArchiveContent().UsageOnly() {
		return nil, apiError(http.StatusServiceUnavailable, "diagnostic writes are unavailable")
	}
	e := db.DiagnosticEvent{ID: in.Body.ID, Source: in.Body.Source, Message: in.Body.Message}
	if in.Body.OccurredAt != nil {
		e.OccurredAt = *in.Body.OccurredAt
		e.OccurredAtSupplied = true
	}
	var stored db.DiagnosticEvent
	err := s.SerializeArchiveWrite(ctx, func() error { var err error; stored, err = local.AppendDiagnostic(ctx, e); return err })
	if errors.Is(err, db.ErrInvalidDiagnostic) {
		return nil, apiError(http.StatusUnprocessableEntity, err.Error())
	}
	if errors.Is(err, db.ErrDiagnosticConflict) {
		return nil, apiError(http.StatusConflict, err.Error())
	}
	if handled := handleHumaReadOnly(err); handled != nil {
		return nil, handled
	}
	if err != nil {
		return nil, internalError("append diagnostic", err)
	}
	return &jsonOutput[db.DiagnosticEvent]{Body: stored}, nil
}
