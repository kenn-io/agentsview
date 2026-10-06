package server_test

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

func seedAnnotatedSession(t *testing.T, te *testEnv, id string, prs []db.PRLink) {
	t.Helper()
	require.NoError(t, te.db.UpsertSession(t.Context(), db.Session{
		ID: id, Machine: "test", Agent: "claude", Project: "proj",
		MessageCount: 4, UserMessageCount: 2, PRLinks: prs,
	}))
}

func listedSessionIDs(t *testing.T, te *testEnv, query url.Values) []string {
	t.Helper()
	w := te.get(t, "/api/v1/sessions?"+query.Encode())
	assertStatus(t, w, http.StatusOK)
	list := decode[service.SessionList](t, w)
	ids := []string{}
	for _, s := range list.Sessions {
		ids = append(ids, s.ID)
	}
	return ids
}

func TestSessionLabelsAPI(t *testing.T) {
	te := setup(t)
	seedAnnotatedSession(t, te, "worker", nil)
	seedAnnotatedSession(t, te, "other", nil)

	w := te.put(t, "/api/v1/sessions/worker/labels",
		`{"labels":["ticket=ABC-123","role=reviewer"]}`)
	assertStatus(t, w, http.StatusOK)
	labels := decode[db.SessionLabels](t, w)
	assert.True(t, labels.SessionFound)
	assert.Equal(t, []string{"role=reviewer", "ticket=ABC-123"}, labels.Labels)

	w = te.patch(t, "/api/v1/sessions/worker/labels",
		`{"add":["nightly"],"remove":["role=reviewer"]}`)
	assertStatus(t, w, http.StatusOK)
	labels = decode[db.SessionLabels](t, w)
	assert.Equal(t, []string{"nightly", "ticket=ABC-123"}, labels.Labels)

	w = te.get(t, "/api/v1/sessions/worker")
	assertStatus(t, w, http.StatusOK)
	detail := decode[service.SessionDetail](t, w)
	assert.Equal(t, []string{"nightly", "ticket=ABC-123"}, detail.Labels)

	assert.Equal(t, []string{"worker"}, listedSessionIDs(t, te, url.Values{
		"label": {"nightly", "ticket=ABC-123"},
	}))
	assert.Equal(t, []string{}, listedSessionIDs(t, te, url.Values{
		"label": {"nightly", "role=reviewer"},
	}))

	// Labels for a session that has not synced yet are accepted.
	w = te.put(t, "/api/v1/sessions/not-yet-synced/labels", `{"labels":["queued"]}`)
	assertStatus(t, w, http.StatusOK)
	assert.False(t, decode[db.SessionLabels](t, w).SessionFound)

	w = te.put(t, "/api/v1/sessions/worker/labels", `{"labels":["=missing-key"]}`)
	assertStatus(t, w, http.StatusBadRequest)

	w = te.put(t, "/api/v1/sessions/worker/labels", `{"labels":[]}`)
	assertStatus(t, w, http.StatusOK)
	assert.Equal(t, []string{}, decode[db.SessionLabels](t, w).Labels)

	// Cleared values stay in the body so a client merging it over a cached
	// session drops them.
	w = te.get(t, "/api/v1/sessions/worker")
	assertStatus(t, w, http.StatusOK)
	assert.Contains(t, w.Body.String(), `"labels":[]`)
	assert.Contains(t, w.Body.String(), `"pr_links":[]`)
}

func TestSessionPRFilterAPI(t *testing.T) {
	te := setup(t)
	seedAnnotatedSession(t, te, "linked", []db.PRLink{{
		URL: "https://github.com/owner/repo/pull/7", Host: "github.com",
		Repository: "owner/repo", Number: 7, Source: "transcript",
	}})
	seedAnnotatedSession(t, te, "unlinked", nil)

	assert.Equal(t, []string{"linked"},
		listedSessionIDs(t, te, url.Values{"pr": {"owner/repo#7"}}))
	assert.Equal(t, []string{},
		listedSessionIDs(t, te, url.Values{"pr": {"owner/repo#8"}}))

	w := te.get(t, "/api/v1/sessions?pr=not-a-repo")
	assertStatus(t, w, http.StatusBadRequest)

	w = te.get(t, "/api/v1/sessions/linked")
	assertStatus(t, w, http.StatusOK)
	detail := decode[service.SessionDetail](t, w)
	require.Len(t, detail.PRLinks, 1)
	assert.Equal(t, "https://github.com/owner/repo/pull/7", detail.PRLinks[0].URL)
}

func TestSessionParentAPI(t *testing.T) {
	te := setup(t)
	seedAnnotatedSession(t, te, "manager", nil)

	w := te.put(t, "/api/v1/sessions/worker/parent",
		`{"parent_session_id":"manager"}`)
	assertStatus(t, w, http.StatusOK)
	link := decode[db.SessionExternalParent](t, w)
	assert.False(t, link.SessionFound)
	assert.Equal(t, "subagent", link.RelationshipType)

	seedAnnotatedSession(t, te, "worker", nil)
	w = te.get(t, "/api/v1/sessions/manager/children")
	assertStatus(t, w, http.StatusOK)
	assert.Contains(t, w.Body.String(), `"id":"worker"`)

	w = te.get(t, "/api/v1/sessions/worker/parent")
	assertStatus(t, w, http.StatusOK)
	assert.True(t, decode[db.SessionExternalParent](t, w).Applied)

	w = te.put(t, "/api/v1/sessions/manager/parent",
		`{"parent_session_id":"worker"}`)
	assertStatus(t, w, http.StatusBadRequest)

	w = te.del(t, "/api/v1/sessions/worker/parent")
	assertStatus(t, w, http.StatusOK)
	w = te.get(t, "/api/v1/sessions/worker/parent")
	assertStatus(t, w, http.StatusNotFound)
	w = te.del(t, "/api/v1/sessions/worker/parent")
	assertStatus(t, w, http.StatusNotFound)
}

func TestSessionAnnotationWritesUnavailableOnReadOnlyStore(t *testing.T) {
	te := setupPGMode(t)
	w := te.put(t, "/api/v1/sessions/any/labels", `{"labels":["x"]}`)
	assertStatus(t, w, http.StatusNotImplemented)
	w = te.put(t, "/api/v1/sessions/any/parent", `{"parent_session_id":"p"}`)
	assertStatus(t, w, http.StatusNotImplemented)
}

func TestSessionLabelsReadableOnReadOnlyStore(t *testing.T) {
	te := setupPGMode(t)
	seedAnnotatedSession(t, te, "worker", nil)
	_, err := te.db.SetSessionLabels(t.Context(), "worker", []string{"ticket=ABC-123"})
	require.NoError(t, err)

	w := te.get(t, "/api/v1/sessions/worker/labels")
	assertStatus(t, w, http.StatusOK)
	labels := decode[db.SessionLabels](t, w)
	assert.True(t, labels.SessionFound)
	assert.Equal(t, []string{"ticket=ABC-123"}, labels.Labels)

	w = te.get(t, "/api/v1/sessions/unknown/labels")
	assertStatus(t, w, http.StatusOK)
	labels = decode[db.SessionLabels](t, w)
	assert.False(t, labels.SessionFound)
	assert.Equal(t, []string{}, labels.Labels)
}
