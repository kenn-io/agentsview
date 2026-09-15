package server_test

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestAgentRemapRulesEndpoints(t *testing.T) {
	te := setup(t)

	// Empty list.
	w := te.get(t, "/api/v1/settings/agent-remap-rules")
	assertStatus(t, w, http.StatusOK)
	var rules []db.AgentRemapRule
	decodeInto(t, w, &rules)
	assert.Empty(t, rules)

	// Create.
	w = te.post(t, "/api/v1/settings/agent-remap-rules", `{
		"source_agent": "goose",
		"model_glob": "ossington-*|rosedale-*|tofino-*",
		"target_agent": "augure-desktop"
	}`)
	assertStatus(t, w, http.StatusCreated)
	var created db.AgentRemapRule
	decodeInto(t, w, &created)
	require.NotZero(t, created.ID)

	// Validation: unknown agent rejected.
	w = te.post(t, "/api/v1/settings/agent-remap-rules", `{
		"source_agent": "goose", "target_agent": "nonsense"
	}`)
	assertStatus(t, w, http.StatusBadRequest)

	// Validation: same source and target rejected.
	w = te.post(t, "/api/v1/settings/agent-remap-rules", `{
		"source_agent": "goose", "target_agent": "goose"
	}`)
	assertStatus(t, w, http.StatusBadRequest)

	// Update.
	w = te.put(t, "/api/v1/settings/agent-remap-rules/"+itoa(created.ID), `{
		"source_agent": "goose", "model_glob": "ossington-*",
		"target_agent": "augure-desktop", "enabled": true
	}`)
	assertStatus(t, w, http.StatusOK)

	// Delete.
	w = te.del(t, "/api/v1/settings/agent-remap-rules/"+itoa(created.ID))
	assertStatus(t, w, http.StatusNoContent)

	// List is empty again.
	w = te.get(t, "/api/v1/settings/agent-remap-rules")
	assertStatus(t, w, http.StatusOK)
	decodeInto(t, w, &rules)
	assert.Empty(t, rules)
}

func TestAgentRemapPreviewAndApplyEndpoint(t *testing.T) {
	te := setup(t)

	te.seedSession(t, "goose:dup", "p", 5, func(s *db.Session) {
		s.Agent = "goose"
		s.FirstMessage = new("Hello world")
	})
	te.seedMessages(t, "goose:dup", 2, func(i int, m *db.Message) {
		if i == 1 {
			m.Model = "ossington-5"
		}
	})

	w := te.post(t, "/api/v1/settings/agent-remap-rules", `{
		"source_agent": "goose", "model_glob": "ossington-*",
		"target_agent": "augure-desktop"
	}`)
	assertStatus(t, w, http.StatusCreated)

	w = te.post(t, "/api/v1/settings/agent-remap-rules/preview", `{}`)
	assertStatus(t, w, http.StatusOK)
	var preview db.AgentRemapPreview
	decodeInto(t, w, &preview)
	require.NotEmpty(t, preview.Token)
	assert.Equal(t, 1, preview.MatchedSessions)

	// Wrong token conflicts.
	w = te.post(t, "/api/v1/settings/agent-remap-rules/apply",
		`{"token": "stale"}`)
	assertStatus(t, w, http.StatusConflict)

	w = te.post(t, "/api/v1/settings/agent-remap-rules/apply",
		`{"token": "`+preview.Token+`"}`)
	assertStatus(t, w, http.StatusOK)
	var applied db.AgentRemapPreview
	decodeInto(t, w, &applied)
	assert.Equal(t, 1, applied.MatchedSessions)

	sess, err := te.db.GetSession(t.Context(), "goose:dup")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, "augure-desktop", sess.Agent)
}

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}
