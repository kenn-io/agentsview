package main

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitvec "go.kenn.io/kit/vector"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/storage"
)

// The key read from AGENTSVIEW_EMBEDDINGS_API_KEY_FILE reaches the wire
// through the adopted recipe's query encoder.
func TestDeploymentEmbeddingKeyFileReachesEncoder(t *testing.T) {
	isolateDeploymentEnv(t)
	var gotAuth, gotModel string
	endpoint := newEmbeddingsStubServer(t, 4, func(r *http.Request, model string) {
		gotAuth = r.Header.Get("Authorization")
		gotModel = model
	})
	t.Cleanup(endpoint.Close)
	keyFile := filepath.Join(t.TempDir(), "embed-key")
	require.NoError(t, os.WriteFile(keyFile, []byte("embedding-secret\n"), 0o600))
	t.Setenv("AGENTSVIEW_MODE", "pg-serve")
	t.Setenv("AGENTSVIEW_EMBEDDINGS_ENDPOINT", endpoint.URL+"/v1")
	t.Setenv("AGENTSVIEW_EMBEDDINGS_API_KEY_FILE", keyFile)

	cfg, err := config.LoadMinimal()
	require.NoError(t, err)
	adopted, err := adoptReplicaVectorConfig(t.Context(), cfg,
		recipeProvider{gens: []storage.VectorGenerationInfo{publishedRecipe()}}, nil)
	require.NoError(t, err)
	enc, err := newVectorQueryEncoder(adopted.Vector.Embeddings, "")
	require.NoError(t, err)
	vec, err := kitvec.EncodeOne(t.Context(), enc, "query")
	require.NoError(t, err)
	assert.Equal(t, []float32{1, 1, 1, 1}, []float32(vec))
	assert.Equal(t, "Bearer embedding-secret", gotAuth)
	assert.Equal(t, "published-model", gotModel)
}
