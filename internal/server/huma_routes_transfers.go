package server

import (
	"net/http"
	"reflect"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/remotesync"
)

// These handlers retain their native HTTP streaming and body limits. Describe
// their contracts in the same Huma document used for generated clients.
func (s *Server) describeTransferRoutes() {
	schemas := s.api.OpenAPI().Components.Schemas
	for _, route := range []struct {
		path, summary, tag, responseType string
		request, response                reflect.Type
		handler                          http.HandlerFunc
	}{
		{"/api/v1/remote-sync/manifest", "Read remote source manifest", "RemoteSync", "application/json", reflect.TypeFor[remotesync.TargetSet](), reflect.TypeFor[remotesync.Manifest](), s.remoteSyncManifestHTTP},
		{"/api/v1/remote-sync/archive", "Download remote source archive", "RemoteSync", "application/x-tar", reflect.TypeFor[remotesync.ArchiveRequest](), reflect.TypeFor[string](), s.remoteSyncArchiveHTTP},
		{"/api/v1/artifacts/exchange", "Exchange artifacts with a local folder", "Artifacts", "application/json", reflect.TypeFor[ArtifactExchangeRequest](), reflect.TypeFor[artifact.SyncResult](), s.handleArtifactExchange},
	} {
		requestSchema := schemas.Schema(route.request, true, "")
		responseSchema := schemas.Schema(route.response, true, "")
		if route.responseType == "application/x-tar" {
			responseSchema = &huma.Schema{
				Type:       "string",
				Format:     "binary",
				Extensions: map[string]any{"contentMediaType": route.responseType},
			}
			archiveSchema := schemas.SchemaFromRef(requestSchema.Ref)
			deltaFilesSchema := archiveSchema.Properties["delta_files"]
			if deltaFilesSchema == nil {
				panic("remote archive schema is missing delta_files")
			}
			if deltaFilesSchema.Extensions == nil {
				deltaFilesSchema.Extensions = map[string]any{}
			}
			deltaFilesSchema.Extensions["x-go-type"] = "*[]string"
		}
		success := &huma.Response{Description: "OK", Content: map[string]*huma.MediaType{route.responseType: {Schema: responseSchema}}}
		op := &huma.Operation{
			OperationID: operationID(http.MethodPost, route.path), Method: http.MethodPost, Path: route.path, Summary: route.summary, Tags: []string{route.tag},
			RequestBody: &huma.RequestBody{Required: true, Content: map[string]*huma.MediaType{"application/json": {Schema: requestSchema}}},
			Responses:   map[string]*huma.Response{"200": success},
		}
		if route.tag == "RemoteSync" {
			op.Parameters = []*huma.Param{{Name: remotesync.ProtocolHeader, In: "header", Required: true, Schema: &huma.Schema{Type: "string"}}}
			success.Headers = map[string]*huma.Param{remotesync.ProtocolHeader: {Schema: &huma.Schema{Type: "string"}}}
		}
		for _, status := range []string{"400", "401", "403", "404", "405", "413", "426", "500", "501", "502", "503"} {
			op.Responses[status] = &huma.Response{Description: "Request failed", Content: map[string]*huma.MediaType{"text/plain": {Schema: &huma.Schema{Type: "string"}}}}
		}
		s.api.OpenAPI().AddOperation(op)
		if route.tag != "Artifacts" || s.artifactExchangeRunner != nil {
			s.handleHTTP(op, route.handler)
			// GET also covers HEAD. Keep these paths out of the SPA fallback
			// so the native handler can report an unsupported method.
			s.handleHTTP(&huma.Operation{Method: http.MethodGet, Path: route.path, Hidden: true}, route.handler)
		}
	}
}
