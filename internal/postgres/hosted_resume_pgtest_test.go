//go:build pgtest

package postgres

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestHostedResumeUsesCapturedProviderIdentity(t *testing.T) {
	for _, tc := range []struct {
		agent   parser.AgentType
		command string
	}{
		{parser.AgentCodex, "codex resume 019fbcca-9fd4-7d20-83dc-0762b2f839b3"},
		{parser.AgentTraeX, "traex resume 019fbcca-9fd4-7d20-83dc-0762b2f839b3"},
		{parser.AgentAugureCode, "augure resume 019fbcca-9fd4-7d20-83dc-0762b2f839b3"},
	} {
		t.Run(string(tc.agent), func(t *testing.T) {
			root := t.TempDir()
			const native = "019fbcca-9fd4-7d20-83dc-0762b2f839b3"
			path := filepath.Join(root, "2026", "08", "01", "rollout-2026-08-01T18-07-03-"+native+".jsonl")
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
			body := testjsonl.JoinJSONL(
				testjsonl.CodexSessionMetaJSON(native, "/work/project", "codex-tui", "2026-08-01T18:07:03.636Z"),
				testjsonl.CodexMsgJSON("user", "Check this example", "2026-08-01T18:07:04Z"),
			)
			require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
			provider, ok := parser.NewProvider(tc.agent, parser.ProviderConfig{Roots: []string{root}, Machine: "capture-device"})
			require.True(t, ok)
			ctx := parser.WithoutFilesystemProjectDiscovery(t.Context())
			sources, err := provider.Discover(ctx)
			require.NoError(t, err)
			require.Len(t, sources, 1)
			outcome, err := provider.Parse(ctx, parser.ParseRequest{Source: sources[0], Machine: "capture-device"})
			require.NoError(t, err)
			require.Len(t, outcome.Results, 1)
			require.Empty(t, outcome.Results[0].Result.Session.SourceSessionID)
			f := newProjectionFixture(t)
			m, _ := f.accept(t, "device-a", "capture-a", "", tc.agent)
			require.NoError(t, f.sink.Project(ctx, f.lease(t, m), m, rawderive.ParsedManifest{Outcome: outcome}))
			h, err := NewHostedStore(f.dsn, f.schema, f.tenant, false)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, h.Close()) })
			cfg, err := config.Default()
			require.NoError(t, err)
			cfg.DataDir = t.TempDir()
			app := server.New(cfg, h, nil)
			// Both a base URL and an anchored variant must recover the native ID,
			// without passing a public branch suffix or private row ID to the CLI.
			for _, id := range []string{outcome.Results[0].Result.Session.ID, f.alias(t, m)} {
				req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/sessions/"+id+"/resume", strings.NewReader(`{"command_only":true}`))
				req.Host = "localhost:8080"
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Origin", "http://localhost:8080")
				w := httptest.NewRecorder()
				app.Handler().ServeHTTP(w, req)
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				var response struct {
					Command  string `json:"command"`
					Launched bool   `json:"launched"`
				}
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
				assert.Equal(t, tc.command, response.Command)
				assert.False(t, response.Launched)
			}
		})
	}
}
