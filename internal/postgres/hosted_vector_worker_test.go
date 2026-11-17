package postgres

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/vector"
	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedconfig"
)

func TestHostedEmbeddingFailureCause(t *testing.T) {
	t.Run("dimension mismatch", func(t *testing.T) {
		endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0,0]}]}`))
		}))
		t.Cleanup(endpoint.Close)
		encode, err := vector.NewEncoder(vector.EncoderConfig{
			Model:      embedconfig.Model{Name: "fixture", Dimensions: 2, Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationNone},
			Deployment: embedconfig.Deployment{BaseURL: endpoint.URL},
			Transport:  embedconfig.Transport{Timeout: 5 * time.Second},
			APIKey:     "synthetic-secret", MaxRetries: 1,
		}, embedconfig.RoleDocument)
		require.NoError(t, err)
		_, err = vector.EncodeUnitDocuments(t.Context(), []db.EmbeddableUnit{{SessionID: "private-session", Kind: "user", Content: "private message"}}, 128, encode)
		require.Error(t, err)
		cause := embeddingFailureCause(fmt.Errorf("private message synthetic-secret: %w", err))
		assert.Equal(t, "embedding endpoint returned an invalid vector: has 3 dimensions, expected 2", cause)
		assert.NotContains(t, cause, "private message")
		assert.NotContains(t, cause, "synthetic-secret")
		assert.NotContains(t, cause, "private-session")
	})
	t.Run("provider reason", func(t *testing.T) {
		err := &embedclient.APIError{StatusCode: http.StatusBadRequest, Reason: embedclient.ReasonInvalidRequest}
		assert.Equal(t, "embedding endpoint returned HTTP 400 (invalid request)", embeddingFailureCause(err))
	})
}
