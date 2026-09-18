package server_test

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestRemoteMachineWorktreeMappingsAPI(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	te := setup(t)
	require.NoError(te.db.SetSyncState(db.MachineAliasKeyPrefix+"old-owner", "host-a.example"))
	prefix := filepath.Join(t.TempDir(), "app.worktrees")
	require.NoError(te.db.UpsertSession(db.Session{
		ID: "remote-session", Machine: "host-a.example", Agent: "claude",
		Project: "branch_label", Cwd: filepath.Join(prefix, "feature"),
	}), "insert remote session")

	created := postWorktreeMapping(t, te, map[string]any{
		"path_prefix":      prefix,
		"project":          "canonical-app",
		"original_project": "branch_label",
		"machine":          "host-a.example",
	})
	require.Equal("host-a.example", created.Machine)
	require.Equal(db.WorktreeMappingLayoutExplicit, created.Layout)
	require.Equal("canonical_app", created.Project)
	require.Equal("branch_label", created.OriginalProject)
	require.True(created.Enabled, "created mapping should default enabled")

	var list struct {
		Machine      string                      `json:"machine"`
		LocalMachine string                      `json:"local_machine"`
		Machines     []string                    `json:"machines"`
		Mappings     []db.WorktreeProjectMapping `json:"mappings"`
	}
	w := te.get(t, "/api/v1/settings/worktree-mappings?machine=old-owner")
	assertStatus(t, w, http.StatusOK)
	decodeInto(t, w, &list)
	require.Equal("host-a.example", list.Machine)
	assert.Equal("test", list.LocalMachine)
	assert.Equal([]string{"host-a.example"}, list.Machines)
	require.Len(list.Mappings, 1)

	updated := putWorktreeMapping(t, te, created.ID, map[string]any{
		"path_prefix":      prefix,
		"project":          "canonical-app-v2",
		"original_project": "replacement-label",
		"machine":          "host-b.example",
		"enabled":          true,
	})
	assert.True(updated.Enabled)
	assert.Equal("host-a.example", updated.Machine,
		"mapping ID determines the machine on edit")
	assert.Equal(db.WorktreeMappingLayoutExplicit, updated.Layout)
	assert.Equal("canonical_app_v2", updated.Project)
	assert.Equal("branch_label", updated.OriginalProject,
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
	assert.Equal("host-a.example", applied.Machine)
	assert.Equal(1, applied.MatchedSessions)
	assert.Equal(1, applied.UpdatedSessions)
	sess, err := te.db.GetSession(t.Context(), "remote-session")
	require.NoError(err)
	assert.Equal("canonical_app_v2", sess.Project)

	req := httptest.NewRequestWithContext(t.Context(),
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
	assert.Empty(list.Mappings, "remote mappings after delete should be empty")
}

func TestWorktreeMappingsAPIHandlesLayouts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	te := setup(t)
	root := t.TempDir()
	layoutPrefix := filepath.Join(root, "service")
	layoutRoot := filepath.Join(layoutPrefix, "service.worktrees")

	created := postWorktreeMapping(t, te, map[string]any{
		"path_prefix": layoutPrefix,
		"layout":      db.WorktreeMappingLayoutRepoDotWorktrees,
	})
	require.Equal(db.WorktreeMappingLayoutRepoDotWorktrees, created.Layout)
	require.Empty(created.Project)
	require.True(created.Enabled, "created mapping should default enabled")

	var list struct {
		Machine  string                      `json:"machine"`
		Mappings []db.WorktreeProjectMapping `json:"mappings"`
	}
	w := te.get(t, "/api/v1/settings/worktree-mappings")
	assertStatus(t, w, http.StatusOK)
	decodeInto(t, w, &list)
	require.Equal("test", list.Machine)
	require.Len(list.Mappings, 1)
	assert.Equal(db.WorktreeMappingLayoutRepoDotWorktrees, list.Mappings[0].Layout)
	assert.Empty(list.Mappings[0].Project)

	updated := putWorktreeMapping(t, te, created.ID, map[string]any{
		"path_prefix": layoutPrefix,
		"layout":      db.WorktreeMappingLayoutRepoDotWorktrees,
		"enabled":     false,
	})
	assert.False(updated.Enabled, "updated mapping should be disabled")
	assert.Equal(db.WorktreeMappingLayoutRepoDotWorktrees, updated.Layout)
	assert.Empty(updated.Project)

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
	assert := assert.New(t)
	require := require.New(t)

	te := setup(t)
	prefix := filepath.Join(t.TempDir(), "app.worktrees")
	_ = postWorktreeMapping(t, te, map[string]any{
		"path_prefix": prefix,
		"project":     "canonical-app",
	})
	require.NoError(te.db.UpsertSession(db.Session{
		ID:      "s1",
		Machine: "test",
		Agent:   "claude",
		Project: "feature_login",
		Cwd:     filepath.Join(prefix, "feature-login"),
	}))

	events, unsubscribe := te.broadcaster.Subscribe()
	defer unsubscribe()
	w := te.post(t, "/api/v1/settings/worktree-mappings/apply", `{}`)
	assertStatus(t, w, http.StatusOK)
	var resp struct {
		Machine         string `json:"machine"`
		MatchedSessions int    `json:"matched_sessions"`
		UpdatedSessions int    `json:"updated_sessions"`
	}
	decodeInto(t, w, &resp)
	assert.Equal("test", resp.Machine)
	assert.Equal(1, resp.MatchedSessions)
	assert.Equal(1, resp.UpdatedSessions)
	sess, err := te.db.GetSession(t.Context(), "s1")
	require.NoError(err)
	assert.Equal("canonical_app", sess.Project)
	select {
	case event := <-events:
		assert.Equal("sessions", event.Scope)
	default:
		t.Fatal("apply mappings did not publish the changed session projects")
	}

	w = te.post(t, "/api/v1/settings/worktree-mappings/apply", `{}`)
	assertStatus(t, w, http.StatusOK)
	select {
	case event := <-events:
		t.Fatalf("unchanged mapping apply published unexpected event: %q", event.Scope)
	default:
	}
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
	req := httptest.NewRequestWithContext(t.Context(),
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
	assert := assert.New(t)

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
	assert.Equal(12, preview.MatchedSessions)
	assert.Equal(12, preview.UpdatedSessions)
	assert.Equal(12, preview.DistinctProjects)
	assert.Len(preview.ProjectSamples, 10)
	assert.Len(preview.SessionSamples, 10)
	assert.NotEmpty(preview.MappingToken)

	w = te.post(t, "/api/v1/settings/worktree-mappings/preview", `{
		"machine": "host-a.example",
		"path_prefix": "",
		"project": "canonical-example"
	}`)
	assertStatus(t, w, http.StatusBadRequest)
}

func TestWorktreeReclassificationAPILocalNoSyncMode(t *testing.T) {
	require := require.New(t)

	te := setupNoSyncMode(t)
	require.NoError(te.db.UpsertSession(db.Session{
		ID: "no-sync-session", Machine: "test", Agent: "codex",
		Project: "branch-label", Cwd: "/srv/worktrees/example/feature",
	}))

	previewW := te.post(t, "/api/v1/settings/worktree-mappings/preview", `{
		"machine": "test",
		"path_prefix": "/srv/worktrees/example",
		"project": "canonical-example",
		"original_project": "branch-label"
	}`)
	assertStatus(t, previewW, http.StatusOK)
	preview := decode[db.WorktreeReclassificationPreview](t, previewW)
	require.NotEmpty(preview.MappingToken)

	w := te.post(t, "/api/v1/settings/worktree-mappings/reclassify", `{
		"machine": "test",
		"path_prefix": "/srv/worktrees/example",
		"project": "canonical-example",
		"original_project": "branch-label",
		"mapping_token": "`+preview.MappingToken+`"
	}`)
	assertStatus(t, w, http.StatusOK)
	session, err := te.db.GetSession(t.Context(), "no-sync-session")
	require.NoError(err)
	assert.Equal(t, "canonical_example", session.Project)
}

func TestSessionProjectAssignmentAPILocalNoSyncMode(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	te := setupNoSyncMode(t)
	require.NoError(te.db.UpsertSession(db.Session{
		ID: "temporary-session", Machine: "test", Agent: "codex",
		Project: "temporary", Cwd: "/tmp/agent-run",
	}))

	w := te.put(t,
		"/api/v1/settings/session-project-assignments/temporary-session",
		`{"project":"real-project"}`,
	)
	assertStatus(t, w, http.StatusOK)
	assignment := decode[db.SessionProjectAssignment](t, w)
	assert.Equal("temporary-session", assignment.SessionID)
	assert.Equal("real_project", assignment.Project)

	session, err := te.db.GetSession(t.Context(), "temporary-session")
	require.NoError(err)
	assert.Equal("real_project", session.Project)
}

func TestClearSessionProjectAssignmentAPILocalNoSyncMode(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	te := setupNoSyncMode(t)
	require.NoError(te.db.UpsertSession(db.Session{
		ID: "temporary-session", Machine: "test", Agent: "codex",
		Project: "temporary", Cwd: "/work/project/run",
	}))
	_, err := te.db.CreateWorktreeProjectMapping(t.Context(),
		db.WorktreeProjectMapping{
			Machine: "test", PathPrefix: "/work/project",
			Project: "mapped-project", Enabled: true,
		})
	require.NoError(err)
	w := te.put(t,
		"/api/v1/settings/session-project-assignments/temporary-session",
		`{"project":"manual-project"}`,
	)
	assertStatus(t, w, http.StatusOK)
	events, unsubscribe := te.broadcaster.Subscribe()
	defer unsubscribe()

	w = te.del(t,
		"/api/v1/settings/session-project-assignments/temporary-session")
	assertStatus(t, w, http.StatusOK)
	cleared := decode[db.ClearedSessionProjectAssignment](t, w)
	assert.Equal("temporary-session", cleared.SessionID)
	assert.Equal("mapped_project", cleared.Project)
	session, err := te.db.GetSession(t.Context(), "temporary-session")
	require.NoError(err)
	assert.Equal("mapped_project", session.Project)
	assert.False(session.ProjectAssigned)

	select {
	case event := <-events:
		assert.Equal("sessions", event.Scope)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for sessions event")
	}
}

func TestActivityProjectReclassificationAPIRejectsStaleToken(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	te := setup(t)
	require.NoError(te.db.UpsertSession(db.Session{
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
	events, unsubscribe := te.broadcaster.Subscribe()
	defer unsubscribe()
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
	assert.Equal("canonical_example", applied.Mapping.Project)
	assert.Equal(1, applied.Result.UpdatedSessions)
	session, err := te.db.GetSession(t.Context(), "stale-session")
	require.NoError(err)
	assert.Equal("canonical_example", session.Project)
	select {
	case event := <-events:
		assert.Equal("sessions", event.Scope)
	default:
		t.Fatal("reclassification did not publish the changed session projects")
	}
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
	req := httptest.NewRequestWithContext(t.Context(),
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
