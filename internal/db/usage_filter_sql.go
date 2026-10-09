package db

import "strings"

// BuildUsageSourceFilter renders model filters before session predicates.
func BuildUsageSourceFilter(f UsageFilter, b *QueryBuilder, modelCol string) []string {
	var preds []string
	for _, item := range []struct {
		raw     string
		include bool
	}{{f.Model, true}, {f.ExcludeModel, false}} {
		if pred := b.ValuesPredicate(modelCol, CSVFilterValues(item.raw), item.include); pred != "" {
			preds = append(preds, pred)
		}
	}
	return preds
}

// BuildUsageSessionFilter preserves label arrays and the session-stage argument order.
func BuildUsageSessionFilter(f UsageFilter, b *QueryBuilder, sessionID string) []string {
	var preds []string
	for _, item := range []struct {
		col    string
		values []string
	}{{"s.agent", CSVFilterValues(f.Agent)}, {"s.project", f.ProjectFilterLabels()}, {"s.machine", CSVFilterValues(f.Machine)}} {
		if pred := b.ValuesPredicate(item.col, item.values, true); pred != "" {
			preds = append(preds, pred)
		}
	}
	if f.GitBranch != "" {
		preds = append(preds, BranchPairPredicate("s.project", "s.git_branch", f.GitBranch, func(v string) string { return b.Add(v) }))
	}
	for _, item := range []struct {
		col    string
		values []string
	}{{"s.project", f.ExcludedProjectFilterLabels()}, {"s.agent", CSVFilterValues(f.ExcludeAgent)}} {
		if pred := b.ValuesPredicate(item.col, item.values, false); pred != "" {
			preds = append(preds, pred)
		}
	}
	if sessionID != "" {
		preds = append(preds, "s.id = "+b.Add(sessionID))
	}
	if f.MinUserMessages > 0 {
		preds = append(preds, "s.user_message_count >= "+b.Add(f.MinUserMessages))
	}
	scope := NormalizeAutomatedScope(f.AutomatedScope, f.ExcludeAutomated)
	falseLiteral := b.dialect.falseLiteral
	automated := "COALESCE(s.is_automated, " + falseLiteral + ")"
	if f.ExcludeOneShot {
		pred := "s.user_message_count > 1"
		if scope != "human" {
			pred = "(" + pred + " OR " + automated + " = " + b.dialect.trueLiteral + ")"
		}
		preds = append(preds, pred)
	}
	if pred := b.dialect.AutomatedScopePredicate(scope, automated); pred != "" {
		preds = append(preds, pred)
	}
	if f.ActiveSince != "" {
		preds = append(preds, b.activityExpr("s.")+" >= "+b.dialect.activityParam(b.Add(f.ActiveSince)))
	}
	if pred := b.reportTerminationPredicate(f.Termination, "s."); pred != "" {
		preds = append(preds, pred)
	}
	return preds
}

// AppendUsagePredicates keeps the existing backend query indentation.
func AppendUsagePredicates(where string, preds []string, indent string) string {
	if len(preds) == 0 {
		return where
	}
	separator := "\n" + indent + "AND "
	return where + separator + strings.Join(preds, separator)
}
