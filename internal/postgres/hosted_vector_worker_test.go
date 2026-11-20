package postgres

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/kit/embedclient"
)

func TestHostedEmbeddingFailureCause(t *testing.T) {
	t.Run("dimension mismatch", func(t *testing.T) {
		err := &embedclient.VectorError{Err: errors.New("has 3 dimensions, expected 2")}
		cause := embeddingFailureCause(fmt.Errorf("private-session private message synthetic-secret: %w", err))
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
