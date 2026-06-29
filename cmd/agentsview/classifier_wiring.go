// ABOUTME: applyClassifierConfig installs user-defined
// ABOUTME: classifier patterns into the db package singleton.
package main

import (
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

// applyClassifierConfig installs user-defined classifier
// patterns into the db package singleton. Every command that
// loads config and may open SQLite or PostgreSQL must call
// this BEFORE db.Open / postgres.Open / postgres.NewStore /
// postgres.New / postgres.EnsureSchema. Silent by design so
// it's safe to call from quiet CLI paths (statusline, JSON
// output, etc.); see the db.SetUserAutomation* setters for
// rationale. The static guardrail test in
// classifier_wiring_test.go (Task 7) enforces this rule.
func applyClassifierConfig(cfg config.Config) {
	db.SetUserAutomationPrefixes(cfg.Automated.Prefixes)
	db.SetUserAutomationSubstrings(cfg.Automated.Substrings)
	db.SetUserAutomationExactMatches(cfg.Automated.ExactMatches)
}
