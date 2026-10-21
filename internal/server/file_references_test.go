package server_test

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/fileevidence"
	"go.kenn.io/agentsview/internal/integrations/docbank"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/service"
)

func TestUploadFileEvidence(t *testing.T) {
	env := setup(t)
	env.db.SetAssetsDir(t.TempDir())
	content := strings.Join([]string{
		`{"type":"user","uuid":"u","message":{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"YWxwaGEK"}}]}}`,
		`{"type":"assistant","uuid":"a","parentUuid":"u","message":{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"Bash","input":{"command":"cat example.png"}}]}}`,
		`{"type":"user","uuid":"r","parentUuid":"a","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"YWxwaGEK"}}]}]}}`,
	}, "\n") + "\n"
	w := env.upload(t, "evidence.jsonl", content, "project=example")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 2, decode[uploadResponse](t, w).Messages)
	snapshot, err := env.db.ReadFileEvidenceSnapshot(t.Context(), "evidence", fileevidence.Window{})
	require.NoError(t, err)
	assert.Equal(t, "complete", snapshot.Coverage.State)
	require.Len(t, snapshot.Rows, 2)
	for i, row := range snapshot.Rows {
		assert.Equal(t, i, row.MessageOrdinal)
		assert.Equal(t, "b6a98d9ce9a2d9149288fa3df42d377c3e42737afdcdaf714e33c0a100b51060", row.Record.SHA256)
		require.NotNil(t, row.Record.Size)
		assert.Equal(t, int64(6), *row.Record.Size)
	}
	assert.Equal(t, 0, snapshot.Rows[1].Record.Position.Call)
	assert.Equal(t, "r", snapshot.Rows[1].Record.Position.ResultEntry)
	w = env.get(t, "/api/v1/sessions/evidence/messages?limit=10&asc=true")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var messages service.MessageList
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &messages))
	assert.Equal(t, "complete", messages.FileEvidenceCoverage)
	assert.Len(t, messages.FileEvidence, 2)
	assert.Len(t, messages.Messages, 2)
}

func TestUploadFileEvidenceBlockedResults(t *testing.T) {
	defaults, err := config.Default()
	require.NoError(t, err)
	for _, tc := range []struct {
		name       string
		categories []string
		blocked    int
	}{
		{"default", defaults.ResultContentBlockedCategories, 0},
		{"custom", []string{"  bAsH  "}, 1},
		{"disabled", []string{}, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setup(t, func(c *config.Config) { c.ResultContentBlockedCategories = tc.categories })
			env.db.SetAssetsDir(t.TempDir())
			content := strings.Join([]string{
				`{"type":"user","uuid":"u","message":{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"YWxwaGEK"}}]}}`,
				`{"type":"assistant","uuid":"a","parentUuid":"u","message":{"role":"assistant","content":[{"type":"tool_use","id":"r","name":"Read","input":{"file_path":"example.png"}},{"type":"tool_use","id":"b","name":"Bash","input":{"command":"cat example.png"}}]}}`,
				`{"type":"user","uuid":"result","parentUuid":"a","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"r","content":[{"type":"text","text":"read output"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"YmV0YQo="}}]},{"type":"tool_result","tool_use_id":"b","content":[{"type":"text","text":"bash output"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"Z2FtbWEK"}}]}]}}`,
			}, "\n") + "\n"
			w := env.upload(t, "policy.jsonl", content, "project=example")
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			messages, err := env.db.GetMessages(t.Context(), "policy", 0, 10, true)
			require.NoError(t, err)
			require.Len(t, messages, 2)
			require.Len(t, messages[1].ToolCalls, 2)
			wantHashes := []string{"b6a98d9ce9a2d9149288fa3df42d377c3e42737afdcdaf714e33c0a100b51060"}
			if tc.blocked != 0 {
				// Claude Read also exposes its ineligible path-only occurrence.
				wantHashes = append(wantHashes, "")
			}
			for i, result := range []struct{ text, hash string }{
				{"read output", "f2c82decdd7181cf98945929a62598db7e6b477e11f6e0eb0ae97020eff151ad"},
				{"bash output", "ae9a6306a205417afddd14316cc1d0d5e04a98f1be10865dce643925ee070ce2"},
			} {
				call := messages[1].ToolCalls[i]
				if i == tc.blocked {
					assert.Empty(t, call.ResultContent)
					assert.NoFileExists(t, filepath.Join(env.db.AssetsDir(), result.hash+".png"))
				} else {
					assert.Contains(t, call.ResultContent, result.text)
					assert.FileExists(t, filepath.Join(env.db.AssetsDir(), result.hash+".png"))
					wantHashes = append(wantHashes, result.hash)
				}
			}
			assert.FileExists(t, filepath.Join(env.db.AssetsDir(), wantHashes[0]+".png"))
			snapshot, err := env.db.ReadFileEvidenceSnapshot(t.Context(), "policy", fileevidence.Window{})
			require.NoError(t, err)
			// Session coverage stays complete when any occurrence is retained.
			assert.Equal(t, "complete", snapshot.Coverage.State)
			hashes := make([]string, 0, len(snapshot.Rows))
			for _, row := range snapshot.Rows {
				hashes = append(hashes, row.Record.SHA256)
			}
			assert.ElementsMatch(t, wantHashes, hashes)
			w = env.get(t, "/api/v1/sessions/policy/messages?limit=10&asc=true")
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			var page service.MessageList
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &page))
			svc := service.NewReadOnlyBackend(env.db)
			refs, err := svc.FileReferences(t.Context(), service.FileReferencesInput{SessionID: "policy"})
			require.NoError(t, err)
			var local []service.LocalFileEvidence
			for _, group := range page.FileEvidence {
				local = append(local, group.Records...)
			}
			require.Len(t, local, len(snapshot.Rows))
			require.Len(t, refs.Occurrences, len(local))
			for i, record := range local {
				assert.Equal(t, record, refs.Occurrences[i].LocalFileEvidence)
				if record.ToolCallIndex != nil && record.Kind == "image" {
					assert.Equal(t, new(0), record.ResultEventIndex)
				} else {
					assert.Nil(t, record.ResultEventIndex)
				}
			}
		})
	}
}

type fileReferenceResolver struct {
	calls  int
	closed bool
}

func (r *fileReferenceResolver) Lookup(context.Context, docbank.Lookup) (docbank.Page, error) {
	r.calls++
	return docbank.Page{State: "not_found", ReferenceTotal: new(0)}, nil
}
func (r *fileReferenceResolver) Close() error { r.closed = true; return nil }
func TestFileReferencesHTTPAdmission(t *testing.T) {
	resolver := new(fileReferenceResolver)
	env := setupWithServerOpts(t, []server.Option{server.WithFileReferencesResolver(resolver)}, func(c *config.Config) { c.RequireAuth = true; c.AuthToken = "test-token" })
	dbtest.SeedSession(t, env.db, "example", "example", func(s *db.Session) { s.Agent = "pi" })
	for _, tc := range []struct {
		query, token, intent string
		status               int
	}{
		{"", "", "docbank", 401}, {"", "test-token", "", 400}, {"?offset=-1", "test-token", "docbank", 400}, {"?offset=1", "test-token", "docbank", 400}, {"?binding=old", "test-token", "docbank", 409}, {"?first_ordinal=0", "test-token", "docbank", 400}, {"?occurrence_id=x&binding=old", "test-token", "docbank", 409}, {"", "test-token", "docbank", 200},
	} {
		t.Run(tc.query+tc.token+tc.intent, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/sessions/example/file-references"+tc.query, nil)
			req.Header.Set("Authorization", "Bearer "+tc.token)
			req.Header.Set(service.FileReferencesIntentHeader, tc.intent)
			w := httptest.NewRecorder()
			env.handler.ServeHTTP(w, req)
			assert.Equal(t, tc.status, w.Code, w.Body.String())
			assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
			if tc.status == 200 {
				var out service.FileReferencesResult
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
				assert.Equal(t, "unavailable", out.Coverage)
				assert.NotEmpty(t, out.Binding)
			}
		})
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/sessions/missing/file-references", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set(service.FileReferencesIntentHeader, "docbank")
	w := httptest.NewRecorder()
	env.handler.ServeHTTP(w, req)
	assert.Equal(t, 404, w.Code)
	assert.Zero(t, resolver.calls)
	require.NoError(t, env.srv.Shutdown(t.Context()))
	assert.True(t, resolver.closed)
}
