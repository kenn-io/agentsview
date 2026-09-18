package service_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/servicehttp"
)

func TestHTTPBackendStats(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Parallel()
	var gotPath string
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			gotQuery = r.URL.Query()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"schema_version": 1,
				"window": {"since": "2026-04-01T00:00:00Z", "until": "2026-04-15T00:00:00Z", "days": 14},
				"filters": {"agent": "codex", "projects_included": ["alpha"], "projects_excluded": ["beta"], "timezone": "UTC"},
				"totals": {"sessions_all": 7}
			}`))
		}))
	t.Cleanup(srv.Close)
	svc := servicehttp.NewHTTPBackend(srv.URL, "", false, "")

	stats, err := svc.Stats(t.Context(), service.StatsFilter{
		Since:                 "2026-04-01",
		Until:                 "2026-04-15",
		Agent:                 "codex",
		IncludeOneShot:        true,
		IncludeAutomated:      true,
		IncludeProjects:       []string{"alpha"},
		ExcludeProjects:       []string{"beta"},
		Timezone:              "UTC",
		IncludeGitOutcomes:    true,
		IncludeGitHubOutcomes: true,
	})

	require.NoError(err)
	require.NotNil(stats)
	assert.Equal("/api/v1/session-stats", gotPath)
	assert.Equal("2026-04-01", gotQuery.Get("since"))
	assert.Equal("2026-04-15", gotQuery.Get("until"))
	assert.Equal("codex", gotQuery.Get("agent"))
	assert.Equal("true", gotQuery.Get("include_one_shot"))
	assert.Equal("true", gotQuery.Get("include_automated"))
	assert.Equal("alpha", gotQuery.Get("include_project"))
	assert.Equal("beta", gotQuery.Get("exclude_project"))
	assert.Equal("UTC", gotQuery.Get("timezone"))
	assert.Equal("true", gotQuery.Get("include_git_outcomes"))
	assert.Equal("true", gotQuery.Get("include_github_outcomes"))
	assert.Equal(7, stats.Totals.SessionsAll)
}

func TestHTTPBackendStatsDisablesDefaultVisibilityWithExplicitIncludes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Parallel()
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			gotQuery = r.URL.Query()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"schema_version":1,"totals":{"sessions_all":2}}`))
		}))
	t.Cleanup(srv.Close)
	svc := servicehttp.NewHTTPBackend(srv.URL, "", false, "")

	stats, err := svc.Stats(t.Context(), service.StatsFilter{
		Since: "28d",
		Agent: "all",
	})

	require.NoError(err)
	require.NotNil(stats)
	assert.Equal("true", gotQuery.Get("include_one_shot"))
	assert.Equal("true", gotQuery.Get("include_automated"))
}
