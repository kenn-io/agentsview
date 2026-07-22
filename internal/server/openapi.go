package server

import (
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"go.kenn.io/agentsview/internal/config"
)

// OpenAPISpec returns the same Huma OpenAPI document served by /api/openapi.json
// without requiring a database or a running HTTP server.
func OpenAPISpec(version VersionInfo, opts ...Option) *huma.OpenAPI {
	s := &Server{
		cfg: config.Config{
			WriteTimeout: 30 * time.Second,
		},
		mux:                    http.NewServeMux(),
		version:                version,
		documentArtifactRoutes: true,
	}
	for _, opt := range opts {
		opt(s)
	}

	configureHumaErrors()
	s.api = humago.New(s.mux, s.humaConfig())
	s.registerTypedAPIRoutes()
	return s.api.OpenAPI()
}

// OpenAPIJSON serializes the generated OpenAPI document as JSON.
func OpenAPIJSON(version VersionInfo, opts ...Option) ([]byte, error) {
	return OpenAPISpec(version, opts...).MarshalJSON()
}
