package server_test

import (
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/server"
)

func TestNativeAndLegacyMessageBodiesMatchOpenAPISchema(t *testing.T) {
	te := setup(t)
	dbtest.SeedNativeSearch(t, te.db)
	w := te.get(t, "/api/v1/sessions/native-search/messages")
	assertStatus(t, w, http.StatusOK)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	spec := server.OpenAPISpec(server.VersionInfo{})
	schema := spec.Paths["/api/v1/sessions/{id}/messages"].Get.Responses["200"].Content["application/json"].Schema
	result := &huma.ValidateResult{}
	huma.Validate(spec.Components.Schemas, schema, &huma.PathBuffer{}, huma.ModeReadFromServer, body, result)
	assert.Empty(t, result.Errors, "saved native and NULL legacy layouts must both be valid API responses")
}
