package db

import "strings"

func (b *QueryBuilder) usageCSV(raw string) []string {
	if raw == "" {
		return nil
	}
	if !b.dialect.trimUsageCSV {
		return strings.Split(raw, ",")
	}
	return CSVFilterValues(raw)
}

func (b *QueryBuilder) usageValuesPredicate(col string, values []string, include bool) string {
	if len(values) == 0 {
		return ""
	}
	return valuesPredicate(col, values, b, include)
}

// BuildUsageSourceFilter renders model filters before session predicates.
func BuildUsageSourceFilter(f UsageFilter, b *QueryBuilder, modelCol string) []string {
	var preds []string
	for _, item := range []struct {
		raw     string
		include bool
	}{{f.Model, true}, {f.ExcludeModel, false}} {
		if pred := b.usageValuesPredicate(modelCol, b.usageCSV(item.raw), item.include); pred != "" {
			preds = append(preds, pred)
		}
	}
	return preds
}

// BuildUsageSessionFilter preserves label arrays and the session-stage argument order.
func BuildUsageSessionFilter(f UsageFilter, b *QueryBuilder, sessionID string) []string {
	var preds []string
	for _, item := range []struct {
		col     string
		values  []string
		include bool
	}{{"s.agent", b.usageCSV(f.Agent), true}, {"s.project", f.ProjectFilterLabels(), true}, {"s.machine", b.usageCSV(f.Machine), true}} {
		if pred := b.usageValuesPredicate(item.col, item.values, item.include); pred != "" {
			preds = append(preds, pred)
		}
	}
	if f.GitBranch != "" {
		preds = append(preds, BranchPairPredicate("s.project", "s.git_branch", f.GitBranch, func(v string) string { return b.Add(v) }))
	}
	for _, item := range []struct {
		col    string
		values []string
	}{{"s.project", f.ExcludedProjectFilterLabels()}, {"s.agent", b.usageCSV(f.ExcludeAgent)}} {
		if pred := b.usageValuesPredicate(item.col, item.values, false); pred != "" {
			preds = append(preds, pred)
		}
	}
	if sessionID != "" {
		preds = append(preds, "s.id = "+b.Add(sessionID))
	}
	if f.MinUserMessages > 0 {
		preds = append(preds, "s.user_message_count >= "+b.Add(f.MinUserMessages))
	}
	scope := normalizeAutomatedScope(f.AutomatedScope, f.ExcludeAutomated)
	falseLiteral := b.dialect.falseLiteral
	if b.dialect.usageFalseLiteral != "" {
		falseLiteral = b.dialect.usageFalseLiteral
	}
	automated := "COALESCE(s.is_automated, " + falseLiteral + ")"
	if f.ExcludeOneShot {
		pred := "s.user_message_count > 1"
		if scope != "human" {
			pred = "(" + pred + " OR " + automated + " = " + b.dialect.trueLiteral + ")"
		}
		preds = append(preds, pred)
	}
	if scope == "human" {
		preds = append(preds, automated+" = "+b.dialect.falseLiteral)
	} else if scope == "automated" {
		preds = append(preds, automated+" = "+b.dialect.trueLiteral)
	}
	if f.ActiveSince != "" {
		preds = append(preds, b.reportActivityExpr("s.")+" >= "+b.dialect.activityParam(b.Add(f.ActiveSince)))
	}
	if pred := b.reportTerminationPredicate(f.Termination, "s."); pred != "" {
		preds = append(preds, pred)
	}
	return preds
}

// AppendUsagePredicates keeps the existing backend query indentation.
func AppendUsagePredicates(where string, preds []string, indent string) string {
	for _, pred := range preds {
		where += "\n" + indent + "AND " + pred
	}
	return where
}
