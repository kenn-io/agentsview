package server

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToolCallIndexRequiredForFileReferenceJoin(t *testing.T) {
	spec := OpenAPISpec(VersionInfo{})
	schema := spec.Components.Schemas.Map()["DbToolCall"]
	require.NotNil(t, schema)
	assert.Contains(t, schema.Required, "call_index")
}
