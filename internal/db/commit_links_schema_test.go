package db_test

import (
	"encoding/json/v2"
	"reflect"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestCommitLinkSchemaAcceptsRequiredNullContext(t *testing.T) {
	registry := huma.NewMapRegistry("#/components/schemas/", huma.DefaultSchemaNamer)
	schema := registry.Schema(reflect.TypeFor[db.CommitLink](), true, "")
	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(`{"session_id":"missing","commit_sha":"abcdef0","resolved":false,"source":"trailer","message_ordinal":null,"call_index":null,"repository_path":"","branch":"","subject":"","committed_at":"","ambiguous":true,"agent":"","project":"","session_started_at":"","web_url":"","session":null,"gone":true,"turn":null}`), &body))
	result := &huma.ValidateResult{}
	huma.Validate(registry, schema, &huma.PathBuffer{}, huma.ModeReadFromServer, body, result)
	assert.Empty(t, result.Errors)
}
