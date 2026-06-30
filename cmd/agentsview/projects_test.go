package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestFetchHTTPProjects(t *testing.T) {
	var gotAuth string
	var gotQuery url.Values
	ts := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		assert.Equal(t, "/api/v1/projects", r.URL.Path)
		gotAuth = r.Header.Get("Authorization")
		gotQuery = r.URL.Query()
		writeJSONResponse(w, `{
			"projects": [
				{"name": "alpha", "session_count": 3},
				{"name": "beta", "session_count": 1}
			]
		}`)
	}))
	defer ts.Close()

	projects, err := fetchHTTPProjects(
		context.Background(),
		transport{Mode: transportHTTP, URL: ts.URL},
		"secret-token",
		true,
		true,
	)

	require.NoError(t, err)
	assert.Equal(t, "Bearer secret-token", gotAuth)
	assert.Equal(t, "false", gotQuery.Get("include_one_shot"))
	assert.Equal(t, "false", gotQuery.Get("include_automated"))
	assert.Equal(t, []db.ProjectInfo{
		{Name: "alpha", SessionCount: 3},
		{Name: "beta", SessionCount: 1},
	}, projects)
}

func TestFetchHTTPProjectsTimesOutStalledDaemon(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		<-r.Context().Done()
	}))
	defer ts.Close()

	oldClient := projectsHTTPClient
	projectsHTTPClient = &http.Client{Timeout: 20 * time.Millisecond}
	t.Cleanup(func() { projectsHTTPClient = oldClient })

	_, err := fetchHTTPProjects(
		context.Background(),
		transport{Mode: transportHTTP, URL: ts.URL},
		"",
		false,
		false,
	)

	require.Error(t, err)
}
