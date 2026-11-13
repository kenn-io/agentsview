package server_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// exportFakeAWSKey is an obviously fake AWS access key ID, split so this file
// does not hold the whole token.
const exportFakeAWSKey = "AKIA" + "7QHWN2DKR4FYPLJM"

const exportMaskedAWSKey = "AKIA…PLJM"

func seedSecretSession(t *testing.T, te *testEnv) {
	t.Helper()
	te.seedSession(t, "leaky", "my-app", 2)
	te.seedMessages(t, "leaky", 2, func(i int, m *db.Message) {
		if i == 0 {
			m.Content = "deploy with " + exportFakeAWSKey + " please"
		}
	})
}

func TestSessionExportsMaskSecretsUnlessRevealed(t *testing.T) {
	te := setup(t)
	seedSecretSession(t, te)

	for _, path := range []string{
		"/api/v1/sessions/leaky/export",
		"/api/v1/sessions/leaky/md",
	} {
		t.Run(path, func(t *testing.T) {
			w := te.get(t, path)
			require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
			assert.NotContains(t, w.Body.String(), exportFakeAWSKey)
			assert.Contains(t, w.Body.String(), exportMaskedAWSKey)

			w = te.get(t, path+"?reveal=true")
			require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
			assert.Contains(t, w.Body.String(), exportFakeAWSKey)
		})
	}
}

// TestMarkdownExportMasksSecretsInToolArguments covers a key that JSON escapes
// hide from a plain-text scan: in serialized tool input, the "n" of "\n" sits
// against the key and removes its word boundary.
func TestMarkdownExportMasksSecretsInToolArguments(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "tool-leak", "my-app", 1)
	input := `{"file_path":"creds.txt","content":"[default]\n` + exportFakeAWSKey + `\n"}`
	require.NoError(t, te.db.InsertMessages(t.Context(), []db.Message{{
		SessionID: "tool-leak", Role: "assistant", Content: "writing creds",
		ToolCalls: []db.ToolCall{{ToolUseID: "call", ToolName: "Write", Category: "Write", InputJSON: input}},
	}}))

	w := te.get(t, "/api/v1/sessions/tool-leak/md")
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.NotContains(t, w.Body.String(), exportFakeAWSKey)
	assert.Contains(t, w.Body.String(), exportMaskedAWSKey)
	assert.Contains(t, w.Body.String(), "creds.txt", "unmasked arguments are kept")
}

func TestSessionExportRevealRejectsProxiedCallers(t *testing.T) {
	te := setup(t)
	seedSecretSession(t, te)

	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/api/v1/sessions/leaky/export?reveal=true"},
		{http.MethodGet, "/api/v1/sessions/leaky/md?reveal=true"},
		{http.MethodPost, "/api/v1/sessions/leaky/publish?reveal=true"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			w := te.wrappedRequest(tc.method, tc.path,
				withHeader("X-Forwarded-For", "203.0.113.10"))
			assert.Equal(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())
			assert.NotContains(t, w.Body.String(), exportFakeAWSKey)
		})
	}
}
