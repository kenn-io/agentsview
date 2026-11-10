package servicehttp

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/db"
)

func (b *httpBackend) AppendDiagnostic(ctx context.Context, e db.DiagnosticEvent) (db.DiagnosticEvent, error) {
	if b.readOnly {
		return db.DiagnosticEvent{}, db.ErrReadOnly
	}
	api, err := b.apiClient(b.client)
	if err != nil {
		return db.DiagnosticEvent{}, err
	}
	body := &apiclient.PostAPIV1DiagnosticsBody{ID: e.ID, Source: e.Source, Message: e.Message}
	if e.OccurredAtSupplied || !e.OccurredAt.IsZero() {
		body.OccurredAt = &e.OccurredAt
	}
	response, err := api.PostAPIV1DiagnosticsWithResponse(ctx, &apiclient.PostAPIV1DiagnosticsRequestOptions{Body: body})
	if response == nil {
		return db.DiagnosticEvent{}, err
	}
	if err = serviceResponseError(response.HTTPResponse, response.Body, err); err != nil {
		if errors.Is(err, errHTTPNotFound) {
			return db.DiagnosticEvent{}, fmt.Errorf("diagnostic writes are unavailable: %w", err)
		}
		return db.DiagnosticEvent{}, err
	}
	if response.JSON200 == nil {
		return db.DiagnosticEvent{}, errors.New("append diagnostic: empty response")
	}
	result := response.JSON200
	e = db.DiagnosticEvent{ID: result.ID, Source: result.Source, Message: result.Message, OccurredAt: result.OccurredAt, AcceptedAt: result.AcceptedAt}
	return e, nil
}
