package db

import "strings"

// BuildAnalyticsWhere shares filter decisions; callers append their date and time predicates in query order.
func BuildAnalyticsWhere(f AnalyticsFilter, b *QueryBuilder, prefix, sessionID string, datePreds []string) string {
	q := func(col string) string { return prefix + col }
	if sessionID == "" {
		sessionID = q("id")
	}
	preds := append([]string{q("message_count") + " > 0", RelationshipExclusionSQL(f.IncludeSubagents, f.IncludeForks, prefix), q("deleted_at") + " IS NULL"}, datePreds...)
	for _, item := range []struct{ col, raw string }{{"machine", f.Machine}, {"project", f.Project}, {"git_branch", f.GitBranch}, {"agent", f.Agent}, {"model", f.Model}} {
		if item.raw == "" {
			continue
		}
		switch item.col {
		case "project":
			preds = append(preds, q("project")+" = "+b.Add(item.raw))
		case "git_branch":
			preds = append(preds, BranchPairPredicate(q("project"), q("git_branch"), item.raw, func(v string) string { return b.Add(v) }))
		case "model":
			if values := CSVFilterValues(item.raw); len(values) > 0 {
				pred := inPredicate("m.model", values, b)
				if b.dialect.messageMembership != nil {
					preds = append(preds, b.dialect.messageMembership(sessionID, pred))
				} else {
					preds = append(preds, "EXISTS (SELECT 1 FROM messages m WHERE m.session_id = "+sessionID+" AND "+pred+")")
				}
			}
		default:
			if values := CSVFilterValues(item.raw); len(values) > 0 {
				preds = append(preds, inPredicate(q(item.col), values, b))
			}
		}
	}
	if f.MinUserMessages > 0 {
		preds = append(preds, q("user_message_count")+" >= "+b.Add(f.MinUserMessages))
	}
	scope := normalizeAutomatedScope(f.AutomatedScope, f.ExcludeAutomated)
	if f.ExcludeOneShot {
		pred := q("user_message_count") + " > 1"
		if scope != "human" {
			pred = "(" + pred + " OR " + q("is_automated") + " = " + b.dialect.trueLiteral + ")"
		}
		pred = strings.ReplaceAll(f.OneShotExclusionSQL(pred), "relationship_type", q("relationship_type"))
		preds = append(preds, pred)
	}
	if pred := automationScopePredicate(SessionFilter{AutomatedScope: scope}, b.dialect, strings.TrimSuffix(prefix, ".")); pred != "" {
		preds = append(preds, pred)
	}
	if f.ExcludeInteractive {
		preds = append(preds, q("is_automated")+" = "+b.dialect.trueLiteral)
	}
	if f.ActiveSince != "" {
		preds = append(preds, b.reportActivityExpr(prefix)+" >= "+b.dialect.activityParam(b.Add(f.ActiveSince)))
	}
	if pred := b.reportTerminationPredicate(f.Termination, prefix); pred != "" {
		preds = append(preds, pred)
	}
	return strings.Join(preds, " AND ")
}

func (b *QueryBuilder) reportActivityExpr(prefix string) string {
	return strings.NewReplacer("ended_at", prefix+"ended_at", "started_at", prefix+"started_at", "created_at", prefix+"created_at").Replace(b.dialect.cursorActivityExpr)
}

func (b *QueryBuilder) reportTerminationPredicate(status, prefix string) string {
	dialect := b.dialect
	b.dialect.terminationExpr = strings.NewReplacer("ended_at", prefix+"ended_at", "started_at", prefix+"started_at", "created_at", prefix+"created_at").Replace(dialect.terminationExpr)
	b.dialect.reportTermination = true
	defer func() { b.dialect = dialect }()
	return terminationPredicate(status, b, func(col string) string { return prefix + col })
}
