package server_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
)

func TestDataProjectsEndpoint(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "alpha-1", "alpha", 1, func(s *db.Session) {
		s.Machine = "m1"
		s.Cwd = "/w/a"
	})
	te.seedSession(t, "beta-1", "beta", 1, func(s *db.Session) {
		s.Machine = "m2"
		s.Cwd = "/w/b"
	})

	w := te.get(t, "/api/v1/data/projects")
	assertStatus(t, w, http.StatusOK)

	var inv db.ProjectInventory
	decodeInto(t, w, &inv)
	assert.Equal(t, 2, inv.TotalProjects)
	assert.Equal(t, 2, inv.TotalSessions)
	require.Len(t, inv.Projects, 2)
}

func TestDataProjectRulesEndpoint(t *testing.T) {
	te := setup(t)
	_, err := te.db.CreateWorktreeProjectMapping(context.Background(), db.WorktreeProjectMapping{
		Machine: "ws", PathPrefix: "/work", Layout: db.WorktreeMappingLayoutExplicit,
		Project: "outer", Enabled: true,
	})
	require.NoError(t, err, "create /work mapping")
	te.seedSession(t, "repo-1", "misc", 1, func(s *db.Session) {
		s.Machine = "ws"
		s.Cwd = "/work/a"
	})

	w := te.get(t, "/api/v1/data/project-rules?machine=ws")
	assertStatus(t, w, http.StatusOK)

	var rules db.ProjectRules
	decodeInto(t, w, &rules)
	assert.Equal(t, "ws", rules.Machine)
	assert.Contains(t, rules.Machines, "ws")
	require.Len(t, rules.Rules, 1)
	assert.Equal(t, "/work", rules.Rules[0].PathPrefix)
	assert.Equal(t, 1, rules.Rules[0].GovernedSessions)
}

func TestDataProjectRulesDefaultsToLocalMachine(t *testing.T) {
	te := setup(t)

	w := te.get(t, "/api/v1/data/project-rules")
	assertStatus(t, w, http.StatusOK)

	var body struct {
		Machine      string `json:"machine"`
		LocalMachine string `json:"local_machine"`
	}
	decodeInto(t, w, &body)
	assert.Equal(t, "test", body.LocalMachine)
	assert.Equal(t, body.LocalMachine, body.Machine)
}

func TestDataProjectReclassificationCandidatesEndpoint(t *testing.T) {
	te := setup(t)
	const rawProject = "branch-label"
	te.seedSession(t, "selected", rawProject, 1, func(s *db.Session) {
		s.Machine = "host-a.example"
		s.Cwd = "/srv/worktrees/example/selected"
	})

	projects, err := te.db.BuildProjectIdentityMap(context.Background(), []string{rawProject})
	require.NoError(t, err)

	w := te.get(t, buildPathURL(
		"/api/v1/data/project-reclassification/candidates",
		map[string]string{
			"project_label": export.SafeProjectDisplayLabel(rawProject),
			"project_key":   projects[rawProject].ProjectKey,
		},
	))
	assertStatus(t, w, http.StatusOK)

	var response struct {
		Candidates []db.WorktreeReclassificationCandidate `json:"candidates"`
	}
	decodeInto(t, w, &response)
	require.Len(t, response.Candidates, 1)
	assert.Equal(t, "host-a.example", response.Candidates[0].Machine)
	assert.Equal(t, 1, response.Candidates[0].ContributingSessions)
}

func TestDataProjectReclassificationCandidatesMissingProjectKey(t *testing.T) {
	te := setup(t)
	for _, key := range []string{"", "   "} {
		w := te.get(t, buildPathURL(
			"/api/v1/data/project-reclassification/candidates",
			map[string]string{"project_label": "example", "project_key": key},
		))
		assertStatus(t, w, http.StatusBadRequest)
	}
}

func TestDataRoutesRegistered(t *testing.T) {
	te := setup(t)
	paths := []string{
		"/api/v1/data/projects",
		"/api/v1/data/project-rules",
		buildPathURL(
			"/api/v1/data/project-reclassification/candidates",
			map[string]string{"project_label": "example", "project_key": "pl1-example"},
		),
	}
	for _, path := range paths {
		w := te.get(t, path)
		assert.NotEqual(t, http.StatusNotFound, w.Code, "path %q must be registered", path)
		assert.Contains(t, w.Header().Get("Content-Type"), "application/json",
			"path %q must be routed to a JSON handler, not the SPA fallback", path)
	}
}
