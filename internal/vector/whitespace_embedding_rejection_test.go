package vector

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestWhitespaceOnlyEmbeddingInputIsPermanent(t *testing.T) {
	err := &HTTPStatusError{
		Status: http.StatusBadRequest,
		Body:   `{"error":{"message":"Invalid embedding input: Input strings must not be empty or whitespace-only","type":"embedding_error"}}`,
	}

	assert.True(t, err.Permanent())
}

func TestSchemaValidationEmptyInputIsNotPermanent(t *testing.T) {
	err := &HTTPStatusError{
		Status: http.StatusBadRequest,
		Body:   `{"error":{"message":"schema violation: input must be a non-empty string","type":"invalid_request_error"}}`,
	}

	assert.False(t, err.Permanent())
}

func TestWhitespaceOnlyModelValidationIsNotPermanent(t *testing.T) {
	err := &HTTPStatusError{
		Status: http.StatusBadRequest,
		Body:   `{"error":{"message":"model must not be a whitespace-only string","type":"invalid_request_error"}}`,
	}

	assert.False(t, err.Permanent())
}

func TestWhitespaceOnlySchemaValidationIsNotPermanent(t *testing.T) {
	err := &HTTPStatusError{
		Status: http.StatusBadRequest,
		Body:   `{"error":{"message":"schema validation: input string must not be empty or whitespace-only","type":"invalid_request_error"}}`,
	}

	assert.False(t, err.Permanent())
}

func TestWhitespaceOnlyEmbeddingErrorTypeIsNotPermanentWithoutInputContext(t *testing.T) {
	err := &HTTPStatusError{
		Status: http.StatusBadRequest,
		Body:   `{"error":{"message":"model must not be empty or whitespace-only","type":"embedding_error"}}`,
	}

	assert.False(t, err.Permanent())
}
