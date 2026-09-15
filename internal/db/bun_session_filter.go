package db

import (
	"fmt"
	"strings"
	"time"
)

const (
	sidebarChildRelationshipsSQL   = "'subagent', 'fork'"
	canonicalChildRelationshipsSQL = "'subagent', 'fork', 'continuation'"
)

// bunFilterArgs collects Bun's portable question-mark bind parameters. The
// only backend variation retained by common session queries is SQLite's
// instant-order transform for its shipped text timestamps.
type bunFilterArgs struct {
	timestampOrderExpr func(string) string
	args               []any
}

func newBunFilterArgs(timestampOrderExpr func(string) string) *bunFilterArgs {
	if timestampOrderExpr == nil {
		timestampOrderExpr = func(column string) string { return column }
	}
	return &bunFilterArgs{timestampOrderExpr: timestampOrderExpr}
}

func (b *bunFilterArgs) bind(value any) string {
	b.args = append(b.args, value)
	return "?"
}

func (b *bunFilterArgs) values() []any {
	return append([]any(nil), b.args...)
}

func (b *bunFilterArgs) timestamp(column string) string {
	return b.timestampOrderExpr(column)
}

func NormalizeSessionTimezone(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "UTC", nil
	}
	if name == "Local" {
		return "", fmt.Errorf("invalid timezone: %s", name)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return "", fmt.Errorf("invalid timezone: %s", name)
	}
	return loc.String(), nil
}

func sessionDateBoundary(date, timezone string, nextDay bool) string {
	name, err := NormalizeSessionTimezone(timezone)
	if err != nil {
		name = "UTC"
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		loc = time.UTC
	}
	boundary, err := time.ParseInLocation(time.DateOnly, date, loc)
	if err != nil {
		return date
	}
	if nextDay {
		boundary = boundary.AddDate(0, 0, 1)
	}
	return boundary.UTC().Format(time.RFC3339)
}

func EscapeLikePattern(value string) string {
	return strings.NewReplacer(
		`\`, `\\`, `%`, `\%`, `_`, `\_`,
	).Replace(value)
}

func buildBunSessionFilter(
	filter SessionFilter, timestampOrderExpr func(string) string,
) (string, []any) {
	builder := newBunFilterArgs(timestampOrderExpr)
	return buildBunSessionFilterWithQualifier(filter, builder, ""), builder.values()
}

func buildBunSessionFilterForAlias(
	filter SessionFilter,
	timestampOrderExpr func(string) string,
	alias string,
) (string, []any) {
	builder := newBunFilterArgs(timestampOrderExpr)
	return buildBunSessionFilterWithQualifier(filter, builder, alias), builder.values()
}

func buildBunSessionBaseFilter(
	filter SessionFilter, timestampOrderExpr func(string) string,
) (string, []any) {
	builder := newBunFilterArgs(timestampOrderExpr)
	predicates := []string{"message_count > 0", "deleted_at IS NULL"}
	filterPredicates, oneShot := bunSessionFilterPredicates(
		filter, builder, func(column string) string { return column },
	)
	predicates = append(predicates, filterPredicates...)
	if oneShot != "" {
		predicates = append(predicates, oneShot)
	}
	return strings.Join(predicates, " AND "), builder.values()
}

// BunSessionBaseFilter is the child-neutral form used by semantic capability
// queries that apply their own root/child scope.
func BunSessionBaseFilter(filter SessionFilter) (string, []any) {
	return buildBunSessionBaseFilter(filter, nil)
}

func buildBunSessionFilterWithQualifier(
	filter SessionFilter, builder *bunFilterArgs, qualifier string,
) string {
	qualify := func(column string) string {
		if qualifier == "" {
			return column
		}
		return qualifier + "." + column
	}
	predicates := []string{
		qualify("message_count") + " > 0",
		qualify("deleted_at") + " IS NULL",
	}
	if !filter.IncludeChildren {
		predicates = append(predicates, qualify("relationship_type")+
			" NOT IN ("+sidebarChildRelationshipsSQL+")")
	}
	if !filter.IncludeChildren {
		filterPredicates, oneShot := bunSessionFilterPredicates(
			filter, builder, qualify,
		)
		predicates = append(predicates, filterPredicates...)
		if oneShot != "" {
			predicates = append(predicates, oneShot)
		}
		return strings.Join(predicates, " AND ")
	}

	baseWhere := strings.Join(predicates, " AND ")
	rootPredicates, oneShot := bunSessionFilterPredicates(
		filter, builder, func(column string) string { return "root_session." + column },
	)
	if oneShot != "" {
		rootPredicates = append(rootPredicates, oneShot)
	}
	rootPredicates = append(rootPredicates,
		bunCanonicalRootWhere("root_session", filter.IncludeOrphans))
	childAutomation := bunAutomationScopePredicate(filter, "s")
	if childAutomation != "" {
		childAutomation = " AND " + childAutomation
	}
	tree := "WITH RECURSIVE tree(id) AS (" +
		"SELECT root_session.id FROM sessions root_session" +
		" WHERE root_session.message_count > 0" +
		" AND root_session.deleted_at IS NULL AND " +
		strings.Join(rootPredicates, " AND ") +
		" UNION SELECT s.id FROM sessions s" +
		" JOIN tree t ON s.parent_session_id = t.id" +
		" WHERE s.message_count > 0 AND s.deleted_at IS NULL" +
		childAutomation + ") SELECT id FROM tree"
	return baseWhere + " AND " + qualify("id") + " IN (" + tree + ")"
}

func bunCanonicalRootWhere(sessionAlias string, includeOrphans bool) string {
	child := sessionAlias + ".relationship_type IN (" +
		canonicalChildRelationshipsSQL + ")"
	root := "NOT (" + child + ")"
	if !includeOrphans {
		return root
	}
	orphan := `NOT EXISTS (
		SELECT 1 FROM sessions AS parent
		WHERE parent.id = ` + sessionAlias + `.parent_session_id
	)`
	return "(" + root + " OR (" + child + " AND " + orphan + "))"
}

func bunAutomationScopePredicate(filter SessionFilter, sessionAlias string) string {
	column := "is_automated"
	if sessionAlias != "" {
		column = sessionAlias + "." + column
	}
	switch normalizeAutomatedScope(filter.AutomatedScope, filter.ExcludeAutomated) {
	case "human":
		return column + " = FALSE"
	case "automated":
		return column + " = TRUE"
	default:
		return ""
	}
}

func bunSessionFilterPredicates(
	filter SessionFilter,
	builder *bunFilterArgs,
	qualify func(string) string,
) ([]string, string) {
	var predicates []string
	if filter.Project != "" {
		predicates = append(predicates, qualify("project")+" = "+builder.bind(filter.Project))
	}
	if filter.ExcludeProject != "" {
		predicates = append(predicates,
			qualify("project")+" != "+builder.bind(filter.ExcludeProject))
	}
	if filter.Machine != "" {
		predicates = append(predicates,
			bunInPredicate(qualify("machine"), splitCSV(filter.Machine), builder))
	}
	if filter.GitBranch != "" {
		predicates = append(predicates, BranchPairPredicate(
			qualify("project"), qualify("git_branch"), filter.GitBranch,
			func(value string) string { return builder.bind(value) },
		))
	}
	if filter.Agent != "" {
		predicates = append(predicates,
			bunInPredicate(qualify("agent"), splitCSV(filter.Agent), builder))
	}
	dateStart := func() string {
		return builder.timestamp("COALESCE(" +
			bunNullableTimestamp(qualify("started_at")) + ", " + qualify("created_at") + ")")
	}
	dateEnd := func() string {
		outerID := qualify("id")
		if outerID == "id" {
			outerID = "session.id"
		}
		latestMessage := `(SELECT m.timestamp FROM messages AS m
			WHERE m.session_id = ` + outerID + `
			  AND ` + bunNullableTimestamp("m.timestamp") + ` IS NOT NULL
			ORDER BY ` + builder.timestamp(bunNullableTimestamp("m.timestamp")) +
			` DESC, m.timestamp DESC LIMIT 1)`
		return builder.timestamp("COALESCE(" +
			bunNullableTimestamp(qualify("ended_at")) + ", " + latestMessage + ", " +
			bunNullableTimestamp(qualify("started_at")) + ", " + qualify("created_at") + ")")
	}
	boundaryParam := func(value string) string {
		return builder.timestamp(builder.bind(value))
	}
	if filter.Date != "" {
		predicates = append(predicates, "("+dateEnd()+" >= "+boundaryParam(
			sessionDateBoundary(filter.Date, filter.Timezone, false))+" AND "+
			dateStart()+" < "+boundaryParam(
			sessionDateBoundary(filter.Date, filter.Timezone, true))+")")
	}
	if filter.DateFrom != "" {
		predicates = append(predicates, dateEnd()+" >= "+boundaryParam(
			sessionDateBoundary(filter.DateFrom, filter.Timezone, false)))
	}
	if filter.DateTo != "" {
		predicates = append(predicates, dateStart()+" < "+boundaryParam(
			sessionDateBoundary(filter.DateTo, filter.Timezone, true)))
	}
	if filter.ActiveSince != "" {
		predicates = append(predicates,
			dateEnd()+" >= "+boundaryParam(filter.ActiveSince))
	}
	if filter.MinMessages > 0 {
		predicates = append(predicates,
			qualify("message_count")+" >= "+builder.bind(filter.MinMessages))
	}
	if filter.MaxMessages > 0 {
		predicates = append(predicates,
			qualify("message_count")+" <= "+builder.bind(filter.MaxMessages))
	}
	if filter.MinUserMessages > 0 {
		predicates = append(predicates,
			qualify("user_message_count")+" >= "+builder.bind(filter.MinUserMessages))
	}
	if predicate := bunTerminationPredicate(filter.Termination, builder, qualify); predicate != "" {
		predicates = append(predicates, predicate)
	}
	predicates, oneShot := appendBunSessionVisibilityPredicates(
		predicates, filter, builder, qualify,
	)
	if len(filter.Outcome) > 0 {
		predicates = append(predicates,
			bunInPredicate(qualify("outcome"), filter.Outcome, builder))
	}
	if len(filter.HealthGrade) > 0 {
		predicates = append(predicates,
			bunInPredicate(qualify("health_grade"), filter.HealthGrade, builder))
	}
	if filter.MinToolFailures != nil {
		predicates = append(predicates, qualify("tool_failure_signal_count")+
			" >= "+builder.bind(*filter.MinToolFailures))
	}
	if filter.HasSecret {
		predicate := qualify("secret_leak_count") + " > 0"
		versions := nonEmpty(filter.SecretsRulesVersions)
		if len(versions) > 0 {
			predicate += " AND " + bunInPredicate(
				qualify("secrets_rules_version"), versions, builder)
		}
		predicates = append(predicates, predicate)
	}
	if filter.Starred {
		predicates = append(predicates,
			"EXISTS (SELECT 1 FROM starred_sessions AS starred WHERE starred.session_id = "+
				qualify("id")+")")
	}
	return predicates, oneShot
}

func appendBunSessionVisibilityPredicates(
	predicates []string,
	filter SessionFilter,
	builder *bunFilterArgs,
	qualify func(string) string,
) ([]string, string) {
	scope := normalizeAutomatedScope(filter.AutomatedScope, filter.ExcludeAutomated)
	oneShot := ""
	if filter.ExcludeOneShot {
		predicate := bunOneShotPredicate(filter, qualify, scope)
		if filter.IncludeChildren {
			oneShot = predicate
		} else {
			predicates = append(predicates, predicate)
		}
	}
	switch scope {
	case "human":
		predicates = append(predicates, qualify("is_automated")+" = FALSE")
	case "automated":
		predicates = append(predicates, qualify("is_automated")+" = TRUE")
	}
	return predicates, oneShot
}

func bunOneShotPredicate(
	filter SessionFilter, qualify func(string) string, scope string,
) string {
	conditions := []string{qualify("user_message_count") + " > 1"}
	if scope != "human" {
		conditions = append(conditions, qualify("is_automated")+" = TRUE")
	}
	if filter.ChildExemptOneShot {
		conditions = append(conditions,
			qualify("relationship_type")+" IN ("+sidebarChildRelationshipsSQL+")",
			qualify("parent_session_id")+" <> ''")
	}
	if len(conditions) == 1 {
		return conditions[0]
	}
	return "(" + strings.Join(conditions, " OR ") + ")"
}

func bunInPredicate(column string, values []string, builder *bunFilterArgs) string {
	if len(values) == 0 {
		return "1 = 0"
	}
	if len(values) == 1 {
		return column + " = " + builder.bind(values[0])
	}
	placeholders := make([]string, len(values))
	for index, value := range values {
		placeholders[index] = builder.bind(value)
	}
	return column + " IN (" + strings.Join(placeholders, ",") + ")"
}

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

const (
	branchFilterSep = "\x1f"
	branchListSep   = "\x1e"
)

func EncodeBranchFilterToken(project, branch string) string {
	return project + branchFilterSep + branch
}

func SplitBranchFilterTokens(value string) []BranchInfo {
	parts := strings.Split(value, branchListSep)
	out := make([]BranchInfo, 0, len(parts))
	for _, part := range parts {
		project, branch, ok := strings.Cut(part, branchFilterSep)
		if !ok {
			continue
		}
		out = append(out, BranchInfo{
			Project: project, Branch: branch,
			Token: EncodeBranchFilterToken(project, branch),
		})
	}
	return out
}

func BranchPairPredicate(
	projectColumn, branchColumn, tokens string,
	placeholder func(string) string,
) string {
	pairs := SplitBranchFilterTokens(tokens)
	if len(pairs) == 0 {
		return "1 = 0"
	}
	parts := make([]string, len(pairs))
	for index, pair := range pairs {
		parts[index] = "(" + projectColumn + " = " + placeholder(pair.Project) +
			" AND " + branchColumn + " = " + placeholder(pair.Branch) + ")"
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return "(" + strings.Join(parts, " OR ") + ")"
}

func BranchPairClauseArgs(
	projectColumn, branchColumn, tokens string, args []any,
) (string, []any) {
	clause := BranchPairPredicate(
		projectColumn, branchColumn, tokens,
		func(value string) string {
			args = append(args, value)
			return "?"
		},
	)
	return clause, args
}

func nonEmpty(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func bunTerminationPredicate(
	status string, builder *bunFilterArgs, qualify func(string) string,
) string {
	return bunTerminationPredicateAt(status, builder, qualify, time.Now().UTC())
}

func bunTerminationPredicateAt(
	status string,
	builder *bunFilterArgs,
	qualify func(string) string,
	reference time.Time,
) string {
	if status == "" || status == "all" {
		return ""
	}
	activity := builder.timestamp("COALESCE(" +
		bunNullableTimestamp(qualify("ended_at")) + ", " +
		bunNullableTimestamp(qualify("started_at")) + ", " + qualify("created_at") + ")")
	parameter := func(value time.Time) string {
		return builder.timestamp(builder.bind(value))
	}
	activeCutoff := reference.Add(-activeWindow)
	staleCutoff := reference.Add(-staleWindow)
	flagged := qualify("termination_status") +
		" IN ('tool_call_pending', 'truncated')"
	var predicates []string
	for part := range strings.SplitSeq(status, ",") {
		switch strings.TrimSpace(part) {
		case "active":
			predicates = append(predicates, activity+" > "+parameter(activeCutoff))
		case "stale":
			predicates = append(predicates, "("+activity+" > "+parameter(staleCutoff)+
				" AND "+activity+" <= "+parameter(activeCutoff)+" AND "+flagged+")")
		case "unclean":
			predicates = append(predicates,
				"("+activity+" <= "+parameter(staleCutoff)+" AND "+flagged+")")
		case "clean":
			predicates = append(predicates, qualify("termination_status")+" = 'clean'")
		case "awaiting_user":
			predicates = append(predicates,
				qualify("termination_status")+" = 'awaiting_user'")
		}
	}
	if len(predicates) == 0 {
		return ""
	}
	if len(predicates) == 1 {
		return predicates[0]
	}
	return "(" + strings.Join(predicates, " OR ") + ")"
}
