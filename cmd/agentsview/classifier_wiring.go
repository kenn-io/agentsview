// ABOUTME: applyClassifierConfig installs user-defined classifier
// ABOUTME: patterns and pricing aliases into package singletons.
package main

import (
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/pricing"
)

// applyClassifierConfig installs user-defined classifier
// patterns into the db package singleton, and custom_model_pricing
// price_as aliases into the pricing package. Every command that
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
	pricing.SetConfiguredAliases(config.PricingAliases(cfg.CustomModelPricing))
}
