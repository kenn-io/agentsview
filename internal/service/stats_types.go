// internal/service/stats_types.go
package service

import "go.kenn.io/agentsview/internal/db"

// StatsFilter mirrors the session-stats CLI flag set.
type StatsFilter struct {
	ApplyDefaultVisibility bool     `json:"-"`
	Since                  string   `json:"since,omitempty"`
	Until                  string   `json:"until,omitempty"`
	Agent                  string   `json:"agent,omitempty"`
	IncludeOneShot         bool     `json:"include_one_shot,omitempty"`
	IncludeAutomated       bool     `json:"include_automated,omitempty"`
	IncludeProjects        []string `json:"include_projects,omitempty"`
	ExcludeProjects        []string `json:"exclude_projects,omitempty"`
	Timezone               string   `json:"timezone,omitempty"`
	IncludeGitOutcomes     bool     `json:"include_git_outcomes,omitempty"`
	IncludeGitHubOutcomes  bool     `json:"include_github_outcomes,omitempty"`
	GHToken                string   `json:"-"`
}

// SessionStats is the transport-neutral response type; currently just
// an alias for db.SessionStats (the database package already carries
// the full schema with json tags).
type SessionStats = db.SessionStats
