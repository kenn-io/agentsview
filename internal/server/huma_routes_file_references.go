package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"go.kenn.io/agentsview/internal/integrations/docbank"
	"go.kenn.io/agentsview/internal/service"
)

// WithFileReferencesResolver transfers resolver lifetime to the server.
func WithFileReferencesResolver(r docbank.Resolver) Option {
	return func(s *Server) { s.fileResolver = r }
}

type fileReferencesInput struct {
	ID              string           `path:"id" required:"true" doc:"Session ID"`
	Intent          string           `header:"X-AgentsView-Lookup-Intent" doc:"Explicit lookup intent: docbank"`
	Binding         string           `query:"binding" doc:"Opaque archive generation and transcript binding"`
	OccurrenceID    string           `query:"occurrence_id" doc:"Local occurrence selector for reference pagination"`
	Offset          int              `query:"offset" doc:"Occurrence offset"`
	Limit           int              `query:"limit" doc:"Occurrence page size, default 20, maximum 50"`
	ReferenceOffset int              `query:"reference_offset" doc:"Raw remote reference offset for the selected occurrence"`
	FirstOrdinal    optionalIntParam `query:"first_ordinal" doc:"First message ordinal, inclusive; requires last_ordinal"`
	LastOrdinal     optionalIntParam `query:"last_ordinal" doc:"Last message ordinal, inclusive; requires first_ordinal"`
	Refresh         bool             `query:"refresh" doc:"Explicitly refresh the first reference page"`
}

func (s *Server) humaFileReferences(ctx context.Context, in *fileReferencesInput) (*jsonOutput[*service.FileReferencesResult], error) {
	if in.Intent != service.FileReferencesIntentValue {
		return nil, apiError(http.StatusBadRequest, "explicit Docbank lookup intent required")
	}
	input := service.FileReferencesInput{SessionID: in.ID, Binding: in.Binding, OccurrenceID: in.OccurrenceID, Offset: in.Offset, Limit: in.Limit, ReferenceOffset: in.ReferenceOffset, Refresh: in.Refresh}
	if in.FirstOrdinal.IsSet {
		input.FirstOrdinal = &in.FirstOrdinal.Value
	}
	if in.LastOrdinal.IsSet {
		input.LastOrdinal = &in.LastOrdinal.Value
	}
	if s.fileResolverErr != nil {
		return nil, apiError(http.StatusServiceUnavailable, service.ErrFileReferencesUnavailable.Error())
	}
	out, err := s.sessions.FileReferences(ctx, input)
	switch {
	case errors.Is(err, service.ErrFileReferencesInput):
		return nil, apiError(http.StatusBadRequest, err.Error())
	case errors.Is(err, service.ErrFileReferencesNotFound):
		return nil, apiError(http.StatusNotFound, err.Error())
	case errors.Is(err, service.ErrSourceChanged):
		return nil, apiErrorWithCode(http.StatusConflict, "source_changed", err.Error())
	case errors.Is(err, service.ErrFileReferencesUnavailable):
		return nil, apiError(http.StatusServiceUnavailable, err.Error())
	case err != nil:
		return nil, serverError(err)
	}
	return &jsonOutput[*service.FileReferencesResult]{Body: out}, nil
}

// Apply outside authentication and schema validation so failures cannot be cached.
func fileReferencesNoStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") && strings.HasSuffix(r.URL.Path, "/file-references") {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}
