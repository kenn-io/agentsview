package db

import "strings"

// BuildAnalyticsWhere shares filter decisions; callers append their date and time predicates in query order.
func BuildAnalyticsWhere(f AnalyticsFilter, b *QueryBuilder, prefix, sessionID string, datePreds []string) string {
	q := func(col string) string { return prefix + col }
	if sessionID == "" {
		sessionID = q("id")
	}
	preds := append([]string{q("message_count") + " > 0", RelationshipExclusionSQL(f.IncludeSubagents, f.IncludeForks, prefix), q("deleted_at") + " IS NULL"}, datePreds...)
	if values := CSVFilterValues(f.Machine); len(values) > 0 {
		preds = append(preds, inPredicate(q("machine"), values, b))
	}
	if f.Project != "" {
		preds = append(preds, q("project")+" = "+b.Add(f.Project))
	}
	if f.GitBranch != "" {
		preds = append(preds, BranchPairPredicate(q("project"), q("git_branch"), f.GitBranch, func(v string) string { return b.Add(v) }))
	}
	if values := CSVFilterValues(f.Agent); len(values) > 0 {
		preds = append(preds, inPredicate(q("agent"), values, b))
	}
	if values := CSVFilterValues(f.Model); len(values) > 0 {
		pred := inPredicate("m.model", values, b)
		if b.dialect.messageMembership != nil {
			preds = append(preds, b.dialect.messageMembership(sessionID, pred))
		} else {
			preds = append(preds, "EXISTS (SELECT 1 FROM messages m WHERE m.session_id = "+sessionID+" AND "+pred+")")
		}
	}
	if f.MinUserMessages > 0 {
		preds = append(preds, q("user_message_count")+" >= "+b.Add(f.MinUserMessages))
	}
	scope := NormalizeAutomatedScope(f.AutomatedScope, f.ExcludeAutomated)
	if f.ExcludeOneShot {
		pred := q("user_message_count") + " > 1"
		if scope != "human" {
			pred = "(" + pred + " OR " + q("is_automated") + " = " + b.dialect.trueLiteral + ")"
		}
		pred = f.oneShotExclusionSQL(pred, prefix)
		preds = append(preds, pred)
	}
	if pred := b.dialect.AutomatedScopePredicate(scope, q("is_automated")); pred != "" {
		preds = append(preds, pred)
	}
	if f.ExcludeInteractive {
		preds = append(preds, q("is_automated")+" = "+b.dialect.trueLiteral)
	}
	if f.ActiveSince != "" {
		preds = append(preds, b.activityExpr(prefix)+" >= "+b.dialect.activityParam(b.Add(f.ActiveSince)))
	}
	if pred := b.reportTerminationPredicate(f.Termination, prefix); pred != "" {
		preds = append(preds, pred)
	}
	return strings.Join(preds, " AND ")
}

func (b *QueryBuilder) activityExpr(prefix string) string {
	return "COALESCE(" + b.dialect.timestampExpr(prefix+"ended_at") + ", " + b.dialect.timestampExpr(prefix+"started_at") + ", " + prefix + "created_at)"
}

func (b *QueryBuilder) reportTerminationPredicate(status, prefix string) string {
	activity := b.activityExpr(prefix)
	if b.dialect.terminationKind == timestampUnixSeconds {
		activity = "CAST(strftime('%s', " + activity + ") AS INTEGER)"
	}
	return renderTerminationPredicate(status, activity, prefix+"termination_status", b.terminationParam)
}
