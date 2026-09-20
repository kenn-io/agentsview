package server

import (
	"net/http"
	"reflect"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

type recallEntriesResponse struct {
	Entries     []db.RecallResult `json:"entries"`
	TrustedOnly bool              `json:"trusted_only"`
	NextCursor  string            `json:"next_cursor"`
	ResultCap   *int              `json:"result_cap,omitempty"`
}

// Register the existing handlers through Huma while retaining their query
// validation, streaming request bodies, timeout handling, and error responses.
func (s *Server) registerRecallRoutes() {
	schemas := s.api.OpenAPI().Components.Schemas
	for _, route := range []struct {
		method, path, summary       string
		handler                     http.HandlerFunc
		response                    reflect.Type
		strings, integers, booleans string
		body                        reflect.Type
		contentType                 string
	}{
		{http.MethodGet, "/entries", "List recall entries", s.handleListRecallEntries, reflect.TypeFor[recallEntriesResponse](), "q project cwd git_branch agent type scope status review_state extractor_method source_session_id source_episode_id source_run_id supersedes_entry_id superseded_by_entry_id cursor", "limit", "trusted_only", nil, ""},
		{http.MethodGet, "/entries/{id}", "Get recall entry", s.handleGetRecallEntry, reflect.TypeFor[db.RecallEntry](), "", "", "", nil, ""},
		{http.MethodGet, "/extraction/status", "Get recall extraction status", s.handleRecallExtractionStatus, reflect.TypeFor[recallExtractionStatusResponse](), "", "", "", nil, ""},
		{http.MethodGet, "/extraction/progress", "Get recall extraction progress", s.handleRecallExtractionProgress, reflect.TypeFor[recallExtractProgressResponse](), "generation state cursor", "limit", "", nil, ""},
		{http.MethodPost, "/extraction/activate", "Activate recall extraction", s.handleRecallExtractionActivate, nil, "", "", "", nil, ""},
		{http.MethodPost, "/extraction/generations/{fingerprint}/retire", "Retire recall extraction", s.handleRecallExtractionRetire, nil, "", "", "", nil, ""},
		{http.MethodPost, "/query", "Query recall entries", s.handleQueryRecallEntries, reflect.TypeFor[service.RecallQueryResult](), "", "", "", reflect.TypeFor[service.RecallQuery](), "application/json"},
		{http.MethodPost, "/import", "Import recall entries", s.handleImportRecallEntries, reflect.TypeFor[db.RecallImportResult](), "", "", "dry_run require_existing_sessions allow_placeholder_sessions allow_production_import", reflect.TypeFor[string](), "application/x-ndjson"},
	} {
		path := "/api/v1/recall" + route.path
		op := &huma.Operation{OperationID: operationID(route.method, path), Method: route.method, Path: path, Summary: route.summary, Tags: []string{"Recall"}, Responses: map[string]*huma.Response{}}
		if route.response == nil {
			op.Responses["204"] = &huma.Response{Description: "No content"}
		} else {
			op.Responses["200"] = &huma.Response{Description: "OK", Content: map[string]*huma.MediaType{"application/json": {Schema: schemas.Schema(route.response, true, "")}}}
		}
		for _, status := range []string{"400", "401", "403", "404", "409", "500", "501", "503", "504"} {
			op.Responses[status] = &huma.Response{Description: "API error", Content: map[string]*huma.MediaType{"application/json": {Schema: schemas.Schema(reflect.TypeFor[apiResponseError](), true, "")}}}
		}
		for _, params := range []struct{ kind, names string }{{"string", route.strings}, {"integer", route.integers}, {"boolean", route.booleans}} {
			for name := range strings.FieldsSeq(params.names) {
				op.Parameters = append(op.Parameters, &huma.Param{Name: name, In: "query", Schema: &huma.Schema{Type: params.kind}})
			}
		}
		for _, name := range []string{"id", "fingerprint"} {
			if strings.Contains(path, "{"+name+"}") {
				op.Parameters = append(op.Parameters, &huma.Param{Name: name, In: "path", Required: true, Schema: &huma.Schema{Type: "string"}})
			}
		}
		if route.body != nil {
			bodySchema := schemas.Schema(route.body, true, "")
			if route.contentType == "application/x-ndjson" {
				bodySchema = &huma.Schema{
					Type:       "string",
					Format:     "binary",
					Extensions: map[string]any{"contentMediaType": route.contentType},
				}
			}
			op.RequestBody = &huma.RequestBody{Required: true, Content: map[string]*huma.MediaType{route.contentType: {Schema: bodySchema}}}
		}
		handler := s.withTimeout(route.method+" "+path, route.handler)
		s.api.OpenAPI().AddOperation(op)
		s.api.Adapter().Handle(op, func(ctx huma.Context) {
			r, w := humago.Unwrap(ctx)
			handler.ServeHTTP(w, r)
		})
	}
}
