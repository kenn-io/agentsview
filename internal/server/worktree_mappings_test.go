package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestRemoteMachineWorktreeMappingsAPI(t *testing.T) {
	te := setup(t)
	prefix := filepath.Join(t.TempDir(), "app.worktrees")
	require.NoError(t, te.db.UpsertSession(db.Session{
		ID: "remote-session", Machine: "host-a.example", Agent: "claude",
		Project: "branch_label", Cwd: filepath.Join(prefix, "feature"),
	}), "insert remote session")

	created := postWorktreeMapping(t, te, map[string]any{
		"path_prefix":      prefix,
		"project":          "canonical-app",
		"original_project": "branch_label",
		"machine":          "host-a.example",
	})
	require.Equal(t, "host-a.example", created.Machine)
	require.Equal(t, db.WorktreeMappingLayoutExplicit, created.Layout)
	require.Equal(t, "canonical_app", created.Project)
	require.Equal(t, "branch_label", created.OriginalProject)
	require.True(t, created.Enabled, "created mapping should default enabled")

	var list struct {
		Machine      string                      `json:"machine"`
		LocalMachine string                      `json:"local_machine"`
		Machines     []string                    `json:"machines"`
		Mappings     []db.WorktreeProjectMapping `json:"mappings"`
	}
	w := te.get(t, "/api/v1/settings/worktree-mappings?machine=host-a.example")
	assertStatus(t, w, http.StatusOK)
	decodeInto(t, w, &list)
	require.Equal(t, "host-a.example", list.Machine)
	assert.Equal(t, "test", list.LocalMachine)
	assert.Equal(t, []string{"host-a.example"}, list.Machines)
	require.Len(t, list.Mappings, 1)

	updated := putWorktreeMapping(t, te, created.ID, map[string]any{
		"path_prefix":      prefix,
		"project":          "canonical-app-v2",
		"original_project": "replacement-label",
		"machine":          "host-b.example",
		"enabled":          true,
	})
	assert.True(t, updated.Enabled)
	assert.Equal(t, "host-a.example", updated.Machine,
		"mapping ID determines the machine on edit")
	assert.Equal(t, db.WorktreeMappingLayoutExplicit, updated.Layout)
	assert.Equal(t, "canonical_app_v2", updated.Project)
	assert.Equal(t, "branch_label", updated.OriginalProject,
		"HTTP edits cannot overwrite original project")

	w = te.post(t, "/api/v1/settings/worktree-mappings/apply", `{
		"machine": "host-a.example"
	}`)
	assertStatus(t, w, http.StatusOK)
	var applied struct {
		Machine         string `json:"machine"`
		MatchedSessions int    `json:"matched_sessions"`
		UpdatedSessions int    `json:"updated_sessions"`
	}
	decodeInto(t, w, &applied)
	assert.Equal(t, "host-a.example", applied.Machine)
	assert.Equal(t, 1, applied.MatchedSessions)
	assert.Equal(t, 1, applied.UpdatedSessions)
	sess, err := te.db.GetSession(context.Background(), "remote-session")
	require.NoError(t, err)
	assert.Equal(t, "canonical_app_v2", sess.Project)

	req := httptest.NewRequest(
		http.MethodDelete,
		"/api/v1/settings/worktree-mappings/"+
			strconv.FormatInt(created.ID, 10),
		nil,
	)
	req.Header.Set("Origin", "http://127.0.0.1:0")
	delW := httptest.NewRecorder()
	te.handler.ServeHTTP(delW, req)
	assertStatus(t, delW, http.StatusNoContent)

	w = te.get(t, "/api/v1/settings/worktree-mappings?machine=host-a.example")
	assertStatus(t, w, http.StatusOK)
	decodeInto(t, w, &list)
	assert.Empty(t, list.Mappings, "remote mappings after delete should be empty")
}

func TestWorktreeReclassificationPreviewReturns503WhileWriterClosed(t *testing.T) {
	te := setup(t)
	require.NoError(t, te.db.CloseWriter())
	t.Cleanup(func() { require.NoError(t, te.db.ReopenWriter()) })

	w := te.post(t, "/api/v1/settings/worktree-mappings/preview", `{
		"machine": "host-a.example",
		"path_prefix": "/srv/worktrees/example",
		"project": "canonical-example"
	}`)
	require.Equal(t, http.StatusServiceUnavailable, w.Code,
		"body: %s", w.Body.String())
	assert.Equal(t, "5", w.Header().Get("Retry-After"))
}

func TestWorktreeMappingsAPIHandlesLayouts(t *testing.T) {
	te := setup(t)
	root := t.TempDir()
	layoutPrefix := filepath.Join(root, "service")
	layoutRoot := filepath.Join(layoutPrefix, "service.worktrees")

	created := postWorktreeMapping(t, te, map[string]any{
		"path_prefix": layoutPrefix,
		"layout":      db.WorktreeMappingLayoutRepoDotWorktrees,
	})
	require.Equal(t, db.WorktreeMappingLayoutRepoDotWorktrees, created.Layout)
	require.Empty(t, created.Project)
	require.True(t, created.Enabled, "created mapping should default enabled")

	var list struct {
		Machine  string                      `json:"machine"`
		Mappings []db.WorktreeProjectMapping `json:"mappings"`
	}
	w := te.get(t, "/api/v1/settings/worktree-mappings")
	assertStatus(t, w, http.StatusOK)
	decodeInto(t, w, &list)
	require.Equal(t, "test", list.Machine)
	require.Len(t, list.Mappings, 1)
	assert.Equal(t, db.WorktreeMappingLayoutRepoDotWorktrees, list.Mappings[0].Layout)
	assert.Empty(t, list.Mappings[0].Project)

	updated := putWorktreeMapping(t, te, created.ID, map[string]any{
		"path_prefix": layoutPrefix,
		"layout":      db.WorktreeMappingLayoutRepoDotWorktrees,
		"enabled":     false,
	})
	assert.False(t, updated.Enabled, "updated mapping should be disabled")
	assert.Equal(t, db.WorktreeMappingLayoutRepoDotWorktrees, updated.Layout)
	assert.Empty(t, updated.Project)

	w = postRawWorktreeMapping(t, te, map[string]any{
		"path_prefix": layoutRoot,
		"layout":      "bogus",
	})
	assertStatus(t, w, http.StatusBadRequest)

	w = postRawWorktreeMapping(t, te, map[string]any{
		"path_prefix": layoutRoot,
		"layout":      db.WorktreeMappingLayoutExplicit,
	})
	assertStatus(t, w, http.StatusBadRequest)
}

func TestWorktreeMappingsAPIApply(t *testing.T) {
	te := setup(t)
	prefix := filepath.Join(t.TempDir(), "app.worktrees")
	_ = postWorktreeMapping(t, te, map[string]any{
		"path_prefix": prefix,
		"project":     "canonical-app",
	})
	require.NoError(t, te.db.UpsertSession(db.Session{
		ID:      "s1",
		Machine: "test",
		Agent:   "claude",
		Project: "feature_login",
		Cwd:     filepath.Join(prefix, "feature-login"),
	}))

	w := te.post(t, "/api/v1/settings/worktree-mappings/apply", `{}`)
	assertStatus(t, w, http.StatusOK)
	var resp struct {
		Machine         string `json:"machine"`
		MatchedSessions int    `json:"matched_sessions"`
		UpdatedSessions int    `json:"updated_sessions"`
	}
	decodeInto(t, w, &resp)
	assert.Equal(t, "test", resp.Machine)
	assert.Equal(t, 1, resp.MatchedSessions)
	assert.Equal(t, 1, resp.UpdatedSessions)
	sess, err := te.db.GetSession(context.Background(), "s1")
	require.NoError(t, err)
	assert.Equal(t, "canonical_app", sess.Project)
}

func TestWorktreeMappingsAPIRejectsRemoteMode(t *testing.T) {
	te := setupPGMode(t)
	w := te.get(t, "/api/v1/settings/worktree-mappings")
	assertStatus(t, w, http.StatusNotImplemented)

	w = te.post(t, "/api/v1/settings/worktree-mappings", `{
		"path_prefix": "/tmp/app.worktrees",
		"project": "app"
	}`)
	assertStatus(t, w, http.StatusNotImplemented)

	w = te.post(t, "/api/v1/settings/worktree-mappings/apply", `{}`)
	assertStatus(t, w, http.StatusNotImplemented)
}

func TestWorktreeMappingsAPIMalformedIDIsNotFound(t *testing.T) {
	te := setup(t)
	req := httptest.NewRequest(
		http.MethodPut,
		"/api/v1/settings/worktree-mappings/apply",
		bytes.NewReader([]byte(`{}`)),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://127.0.0.1:0")
	w := httptest.NewRecorder()
	te.handler.ServeHTTP(w, req)
	assertStatus(t, w, http.StatusNotFound)
}

func TestWorktreePreviewAPIUsesFullArchiveAndBoundsSamples(t *testing.T) {
	te := setup(t)
	for i := range 12 {
		id := "preview-" + strconv.Itoa(i)
		require.NoError(t, te.db.UpsertSession(db.Session{
			ID: id, Machine: "host-a.example", Agent: "codex",
			Project: "branch-" + strconv.Itoa(i),
			Cwd:     "/srv/worktrees/example/" + id,
		}), "seed preview session")
	}

	w := te.post(t, "/api/v1/settings/worktree-mappings/preview", `{
		"machine": "host-a.example",
		"path_prefix": "/srv/worktrees/example",
		"project": "canonical-example",
		"original_project": "branch-label"
	}`)
	assertStatus(t, w, http.StatusOK)
	preview := decode[db.WorktreeReclassificationPreview](t, w)
	assert.Equal(t, 12, preview.MatchedSessions)
	assert.Equal(t, 12, preview.UpdatedSessions)
	assert.Equal(t, 12, preview.DistinctProjects)
	assert.Len(t, preview.ProjectSamples, 10)
	assert.Len(t, preview.SessionSamples, 10)
	assert.NotEmpty(t, preview.MappingToken)

	w = te.post(t, "/api/v1/settings/worktree-mappings/preview", `{
		"machine": "host-a.example",
		"path_prefix": "",
		"project": "canonical-example"
	}`)
	assertStatus(t, w, http.StatusBadRequest)
}

func TestActivityProjectReclassificationAPIRejectsStaleToken(t *testing.T) {
	te := setup(t)
	require.NoError(t, te.db.UpsertSession(db.Session{
		ID: "stale-session", Machine: "host-a.example", Agent: "codex",
		Project: "branch-label", Cwd: "/srv/worktrees/example/feature",
	}))

	previewW := te.post(t, "/api/v1/settings/worktree-mappings/preview", `{
		"machine": "host-a.example",
		"path_prefix": "/srv/worktrees/example",
		"project": "canonical-example",
		"original_project": "branch-label"
	}`)
	assertStatus(t, previewW, http.StatusOK)
	preview := decode[db.WorktreeReclassificationPreview](t, previewW)
	_ = postWorktreeMapping(t, te, map[string]any{
		"machine": "host-a.example", "path_prefix": "/another/root",
		"project": "another-project",
	})

	w := te.post(t, "/api/v1/settings/worktree-mappings/reclassify", `{
		"machine": "host-a.example",
		"path_prefix": "/srv/worktrees/example",
		"project": "canonical-example",
		"original_project": "branch-label",
		"mapping_token": "`+preview.MappingToken+`"
	}`)
	assertStatus(t, w, http.StatusConflict)

	w = te.post(t, "/api/v1/settings/worktree-mappings/preview", `{
		"machine": "host-a.example",
		"path_prefix": "/srv/worktrees/example",
		"project": "canonical-example",
		"original_project": "branch-label"
	}`)
	assertStatus(t, w, http.StatusOK)
	preview = decode[db.WorktreeReclassificationPreview](t, w)
	w = te.post(t, "/api/v1/settings/worktree-mappings/reclassify", `{
		"machine": "host-a.example",
		"path_prefix": "/srv/worktrees/example",
		"project": "canonical-example",
		"original_project": "branch-label",
		"mapping_token": "`+preview.MappingToken+`"
	}`)
	assertStatus(t, w, http.StatusOK)
	var applied struct {
		Mapping db.WorktreeProjectMapping          `json:"mapping"`
		Result  db.WorktreeReclassificationPreview `json:"result"`
	}
	decodeInto(t, w, &applied)
	assert.Equal(t, "canonical_example", applied.Mapping.Project)
	assert.Equal(t, 1, applied.Result.UpdatedSessions)
	session, err := te.db.GetSession(context.Background(), "stale-session")
	require.NoError(t, err)
	assert.Equal(t, "canonical_example", session.Project)
}

func TestWorktreePreviewAndReclassificationAPIsRejectRemoteMode(t *testing.T) {
	te := setupPGMode(t)
	w := te.post(t, "/api/v1/settings/worktree-mappings/preview", `{
		"machine": "host-a.example", "path_prefix": "/srv/example",
		"project": "example"
	}`)
	assertStatus(t, w, http.StatusNotImplemented)
	w = te.post(t, "/api/v1/settings/worktree-mappings/reclassify", `{
		"machine": "host-a.example", "path_prefix": "/srv/example",
		"project": "example", "mapping_token": "token"
	}`)
	assertStatus(t, w, http.StatusNotImplemented)
}

func postWorktreeMapping(
	t *testing.T,
	te *testEnv,
	body map[string]any,
) db.WorktreeProjectMapping {
	t.Helper()
	data, err := json.Marshal(body)
	require.NoError(t, err)
	w := te.post(
		t,
		"/api/v1/settings/worktree-mappings",
		string(data),
	)
	assertStatus(t, w, http.StatusCreated)
	return decode[db.WorktreeProjectMapping](t, w)
}

func putWorktreeMapping(
	t *testing.T,
	te *testEnv,
	id int64,
	body map[string]any,
) db.WorktreeProjectMapping {
	t.Helper()
	data, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(
		http.MethodPut,
		"/api/v1/settings/worktree-mappings/"+
			strconv.FormatInt(id, 10),
		bytes.NewReader(data),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://127.0.0.1:0")
	w := httptest.NewRecorder()
	te.handler.ServeHTTP(w, req)
	assertStatus(t, w, http.StatusOK)
	return decode[db.WorktreeProjectMapping](t, w)
}

func postRawWorktreeMapping(
	t *testing.T,
	te *testEnv,
	body map[string]any,
) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	require.NoError(t, err)
	return te.post(t, "/api/v1/settings/worktree-mappings", string(data))
}

func decodeInto(
	t *testing.T,
	w *httptest.ResponseRecorder,
	target any,
) {
	t.Helper()
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), target),
		"decoding JSON; body: %s", w.Body.String())
}
