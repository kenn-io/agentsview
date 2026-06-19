package db

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

type placeholderStyle int

const (
	placeholderQuestion placeholderStyle = iota
	placeholderDollar
)

type timestampKind int

const (
	timestampText timestampKind = iota
	timestampUnixSeconds
	timestampTimestamptz
	timestampCast
)

// QueryDialect captures the small set of SQL syntax differences needed by
// shared session-filter and pagination builders. It is intentionally not an
// ORM: callers still own SELECTs, JOINs, backend-specific search paths, and
// table schemas.
type QueryDialect struct {
	name               string
	placeholderStyle   placeholderStyle
	trueLiteral        string
	falseLiteral       string
	dateExpr           string
	dateParam          func(string) string
	activityExpr       string
	activityParam      func(string) string
	cursorActivityExpr string
	cursorParam        func(string) string
	// castCursor wraps a placeholder with the type cast a keyset cursor value of
	// the given kind needs in this dialect.
	castCursor func(string, valueKind) string
	// emptyStringIsNull is true for backends (SQLite) that store unset
	// timestamps as empty strings rather than SQL NULL.
	emptyStringIsNull           bool
	terminationExpr             string
	terminationKind             timestampKind
	caseInsensitiveLike         string
	caseInsensitiveLikeEsc      string
	regexPredicate              func(string, string) string
	sidebarChildRelationships   []string
	canonicalChildRelationships []string
	nullsLast                   bool
}

// SQLiteQueryDialect returns the SQLite SQL fragments used by the local store.
func SQLiteQueryDialect() QueryDialect {
	return QueryDialect{
		name:             "sqlite",
		placeholderStyle: placeholderQuestion,
		trueLiteral:      "1",
		falseLiteral:     "0",
		dateExpr: "date(COALESCE(NULLIF(started_at, ''), " +
			"created_at))",
		dateParam:              func(ph string) string { return ph },
		activityExpr:           "COALESCE(NULLIF(ended_at, ''), NULLIF(started_at, ''), created_at)",
		activityParam:          func(ph string) string { return ph },
		cursorActivityExpr:     "COALESCE(NULLIF(ended_at, ''), NULLIF(started_at, ''), created_at)",
		cursorParam:            func(ph string) string { return ph },
		castCursor:             func(ph string, _ valueKind) string { return ph },
		emptyStringIsNull:      true,
		terminationExpr:        activityExprSQLite,
		terminationKind:        timestampUnixSeconds,
		caseInsensitiveLike:    "LIKE",
		caseInsensitiveLikeEsc: `ESCAPE '\'`,
		regexPredicate: func(col, ph string) string {
			return col + " REGEXP " + ph
		},
		sidebarChildRelationships:   []string{"subagent", "fork"},
		canonicalChildRelationships: []string{"subagent", "fork", "continuation"},
	}
}

// PostgresQueryDialect returns the PostgreSQL SQL fragments used by the
// read-only shared store.
func PostgresQueryDialect() QueryDialect {
	return QueryDialect{
		name:             "postgres",
		placeholderStyle: placeholderDollar,
		trueLiteral:      "TRUE",
		falseLiteral:     "FALSE",
		dateExpr: "DATE(COALESCE(started_at, created_at) " +
			"AT TIME ZONE 'UTC')",
		dateParam:    func(ph string) string { return ph + "::date" },
		activityExpr: "COALESCE(ended_at, started_at, created_at)",
		activityParam: func(ph string) string {
			return ph + "::timestamptz"
		},
		cursorActivityExpr: "COALESCE(ended_at, started_at, created_at)",
		cursorParam: func(ph string) string {
			return ph + "::timestamptz"
		},
		castCursor:             pgCastCursor,
		terminationExpr:        "COALESCE(ended_at, started_at, created_at)",
		terminationKind:        timestampTimestamptz,
		caseInsensitiveLike:    "ILIKE",
		caseInsensitiveLikeEsc: `ESCAPE E'\\'`,
		regexPredicate: func(col, ph string) string {
			return col + " ~* " + ph
		},
		sidebarChildRelationships:   []string{"subagent", "fork"},
		canonicalChildRelationships: []string{"subagent", "fork", "continuation"},
		nullsLast:                   true,
	}
}

// DuckDBQueryDialect returns DuckDB-oriented SQL fragments for renderer tests
// and future backend use. It does not couple to internal/duckdb.
func DuckDBQueryDialect() QueryDialect {
	return QueryDialect{
		name:             "duckdb",
		placeholderStyle: placeholderQuestion,
		trueLiteral:      "TRUE",
		falseLiteral:     "FALSE",
		dateExpr:         "CAST(COALESCE(started_at, created_at) AS DATE)",
		dateParam: func(ph string) string {
			return "CAST(" + ph + " AS DATE)"
		},
		activityExpr:       "COALESCE(ended_at, started_at, created_at)",
		activityParam:      func(ph string) string { return "CAST(" + ph + " AS TIMESTAMP)" },
		cursorActivityExpr: "COALESCE(ended_at, started_at, created_at)",
		cursorParam: func(ph string) string {
			return "CAST(" + ph + " AS TIMESTAMP)"
		},
		castCursor:             duckCastCursor,
		terminationExpr:        "COALESCE(ended_at, started_at, created_at)",
		terminationKind:        timestampCast,
		caseInsensitiveLike:    "ILIKE",
		caseInsensitiveLikeEsc: `ESCAPE '\'`,
		regexPredicate: func(col, ph string) string {
			return "regexp_matches(" + col + ", " + ph + ")"
		},
		sidebarChildRelationships:   []string{"subagent", "fork"},
		canonicalChildRelationships: []string{"subagent", "fork", "continuation"},
		nullsLast:                   true,
	}
}

func (d QueryDialect) placeholder(n int) string {
	if d.placeholderStyle == placeholderDollar {
		return fmt.Sprintf("$%d", n)
	}
	return "?"
}

// Qualify renders a safely quoted identifier path. Empty catalog/schema parts
// are skipped. Invalid identifiers panic because callers should only pass
// static backend-owned names, never user input.
func (d QueryDialect) Qualify(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		if !safeIdentifierRE.MatchString(p) {
			panic("unsafe SQL identifier: " + p)
		}
		out = append(out, `"`+p+`"`)
	}
	return strings.Join(out, ".")
}

var safeIdentifierRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// QueryBuilder allocates dialect placeholders and collects bind parameters.
type QueryBuilder struct {
	dialect QueryDialect
	n       int
	args    []any
}

// NewQueryBuilder creates a builder whose first placeholder follows startIndex.
// For PostgreSQL, startIndex is the number of existing parameters.
func NewQueryBuilder(dialect QueryDialect, startIndex int) *QueryBuilder {
	return &QueryBuilder{dialect: dialect, n: startIndex}
}

func (b *QueryBuilder) Add(v any) string {
	b.n++
	b.args = append(b.args, v)
	return b.dialect.placeholder(b.n)
}

func (b *QueryBuilder) Args() []any {
	return append([]any{}, b.args...)
}

// ContainsPredicate renders a parameterized case-insensitive substring match.
func (b *QueryBuilder) ContainsPredicate(col, pattern string) string {
	ph := b.Add("%" + EscapeLikePattern(pattern) + "%")
	return col + " " + b.dialect.caseInsensitiveLike + " " + ph + " " +
		b.dialect.caseInsensitiveLikeEsc
}

// RegexPredicate renders a parameterized regex predicate in the dialect's
// native syntax. Backends may still choose to evaluate regexes in Go.
func (b *QueryBuilder) RegexPredicate(col, pattern string) string {
	return b.dialect.regexPredicate(col, b.Add(pattern))
}

// CursorBeforePredicate renders the keyset pagination predicate used by
// session list queries ordered by recent activity DESC, id DESC.
func (b *QueryBuilder) CursorBeforePredicate(cur SessionCursor) string {
	ea := b.dialect.cursorParam(b.Add(cur.EndedAt))
	id := b.Add(cur.ID)
	return "(" + b.dialect.cursorActivityExpr + ", id) < (" + ea + ", " + id + ")"
}

func pgCastCursor(ph string, kind valueKind) string {
	switch kind {
	case kindTimestamp:
		return ph + "::timestamptz"
	case kindInt:
		return ph + "::bigint"
	case kindReal:
		return ph + "::double precision"
	default:
		return ph
	}
}

func duckCastCursor(ph string, kind valueKind) string {
	switch kind {
	case kindTimestamp:
		return "CAST(" + ph + " AS TIMESTAMP)"
	case kindInt:
		return "CAST(" + ph + " AS BIGINT)"
	case kindReal:
		return "CAST(" + ph + " AS DOUBLE)"
	default:
		return ph
	}
}

// timestampExpr returns a column reference that treats unset timestamps as NULL.
// SQLite stores empty strings for missing timestamps; other backends use real
// NULLs, so the column reference passes through unchanged.
func (d QueryDialect) timestampExpr(col string) string {
	if d.emptyStringIsNull {
		return "NULLIF(" + col + ", '')"
	}
	return col
}

// OrderByClause renders the session-list ordering for a resolved sort and
// direction, with id as a same-direction tie-breaker so keyset pagination is
// deterministic. The sort expression may add bind parameters (secrets sort), so
// callers must render this at its textual position.
func (b *QueryBuilder) OrderByClause(sp SessionSort, desc bool, f SessionFilter) string {
	orderExpr := sp.orderExpr(b, desc, f)
	dir := "ASC"
	if desc {
		dir = "DESC"
	}
	if orderExpr == "id" {
		return "ORDER BY id " + dir
	}
	return "ORDER BY " + orderExpr + " " + dir + ", id " + dir
}

// CursorPredicate renders the keyset pagination predicate matching an
// OrderByClause built from the same sort and direction. The bound value is cast
// per dialect for the column's kind; it must already be the Go type produced by
// SessionSort.CursorPredicateValue.
func (b *QueryBuilder) CursorPredicate(
	sp SessionSort, desc bool, f SessionFilter, value any, id string,
) string {
	orderExpr := sp.orderExpr(b, desc, f)
	op := ">"
	if desc {
		op = "<"
	}
	if orderExpr == "id" {
		return "id " + op + " " + b.dialect.castCursor(b.Add(id), kindText)
	}
	vp := b.dialect.castCursor(b.Add(value), sp.kind)
	ip := b.Add(id)
	return "(" + orderExpr + ", id) " + op + " (" + vp + ", " + ip + ")"
}

// LimitOffset renders a parameterized LIMIT/OFFSET clause.
func (b *QueryBuilder) LimitOffset(limit, offset int) string {
	limitPH := b.Add(limit)
	offsetPH := b.Add(offset)
	return "LIMIT " + limitPH + " OFFSET " + offsetPH
}

// Limit renders a parameterized LIMIT clause.
func (b *QueryBuilder) Limit(limit int) string {
	return "LIMIT " + b.Add(limit)
}

// NullsLast returns an ORDER BY expression with dialect-appropriate NULL
// placement when the backend supports it.
func (d QueryDialect) NullsLast(expr string) string {
	if d.nullsLast {
		return expr + " NULLS LAST"
	}
	return expr
}

// EscapeLikePattern escapes SQL LIKE wildcard characters so a bind parameter
// is treated as literal user text.
func EscapeLikePattern(s string) string {
	r := strings.NewReplacer(
		`\`, `\\`, `%`, `\%`, `_`, `\_`,
	)
	return r.Replace(s)
}

// BuildSessionFilterSQL returns a WHERE clause and args for SessionFilter.
func BuildSessionFilterSQL(
	f SessionFilter, dialect QueryDialect,
) (string, []any) {
	b := NewQueryBuilder(dialect, 0)
	where := buildSessionFilterWithBuilder(f, b, "")
	return where, b.Args()
}

// BuildSessionBaseFilterSQL returns the base sidebar/list predicates without the
// child-relationship exclusion. Callers that handle root-vs-child selection
// separately should use this to avoid diverging filter logic across backends.
func BuildSessionBaseFilterSQL(
	f SessionFilter, dialect QueryDialect,
) (string, []any) {
	b := NewQueryBuilder(dialect, 0)
	preds := []string{
		"message_count > 0",
		"deleted_at IS NULL",
	}
	filterPreds, oneShotPred := sessionFilterPredicates(f, b, func(col string) string { return col })
	preds = append(preds, filterPreds...)
	if oneShotPred != "" {
		preds = append(preds, oneShotPred)
	}
	return strings.Join(preds, " AND "), b.Args()
}

func (d QueryDialect) SidebarChildRelationshipsSQL() string {
	quoted := make([]string, 0, len(d.sidebarChildRelationships))
	for _, rel := range d.sidebarChildRelationships {
		quoted = append(quoted, "'"+rel+"'")
	}
	return strings.Join(quoted, ", ")
}

func (d QueryDialect) CanonicalChildRelationshipsSQL() string {
	quoted := make([]string, 0, len(d.canonicalChildRelationships))
	for _, rel := range d.canonicalChildRelationships {
		quoted = append(quoted, "'"+rel+"'")
	}
	return strings.Join(quoted, ", ")
}

func SidebarChildRelationshipPredicate(dialect QueryDialect, sessionAlias string) string {
	return sessionAlias + ".relationship_type IN (" + dialect.SidebarChildRelationshipsSQL() + ")"
}

func CanonicalChildRelationshipPredicate(dialect QueryDialect, sessionAlias string) string {
	return sessionAlias + ".relationship_type IN (" + dialect.CanonicalChildRelationshipsSQL() + ")"
}

func SidebarOrphanPredicate(sessionAlias, parentAlias string) string {
	return `NOT EXISTS (
			SELECT 1
			FROM sessions ` + parentAlias + `
			WHERE ` + parentAlias + `.id = ` + sessionAlias + `.parent_session_id
		)`
}

func BuildCanonicalRootWhere(dialect QueryDialect, sessionAlias string, includeOrphans bool) string {
	base := `NOT (` + CanonicalChildRelationshipPredicate(dialect, sessionAlias) + `)`
	if !includeOrphans {
		return base
	}
	return `(` + base + ` OR (` +
		CanonicalChildRelationshipPredicate(dialect, sessionAlias) + ` AND ` +
		SidebarOrphanPredicate(sessionAlias, "parent") + `))`
}

func buildSessionFilterWithBuilder(
	f SessionFilter, b *QueryBuilder, qualifier string,
) string {
	q := func(col string) string {
		if qualifier == "" {
			return col
		}
		return qualifier + "." + col
	}

	basePreds := []string{
		q("message_count") + " > 0",
		q("deleted_at") + " IS NULL",
	}
	if !f.IncludeChildren {
		basePreds = append(basePreds,
			q("relationship_type")+" NOT IN ("+b.dialect.SidebarChildRelationshipsSQL()+")")
	}

	if !f.IncludeChildren {
		filterPreds, oneShotPred := sessionFilterPredicates(f, b, q)
		allPreds := append(basePreds, filterPreds...)
		if oneShotPred != "" {
			allPreds = append(allPreds, oneShotPred)
		}
		return strings.Join(allPreds, " AND ")
	}

	baseWhere := strings.Join(basePreds, " AND ")
	rootFilter, oneShotPred := sessionFilterPredicates(f, b, func(col string) string {
		return "root_session." + col
	})
	rootMatchParts := append([]string{}, rootFilter...)
	if oneShotPred != "" {
		rootMatchParts = append(rootMatchParts, oneShotPred)
	}
	rootMatchParts = append(rootMatchParts,
		BuildCanonicalRootWhere(b.dialect, "root_session", f.IncludeOrphans))
	rootMatch := strings.Join(rootMatchParts, " AND ")

	cte := "WITH RECURSIVE tree(id) AS (" +
		"SELECT root_session.id FROM sessions root_session" +
		" WHERE root_session.message_count > 0" +
		" AND root_session.deleted_at IS NULL AND " +
		rootMatch +
		" UNION " +
		"SELECT s.id FROM sessions s" +
		" JOIN tree t ON s.parent_session_id = t.id" +
		" WHERE s.message_count > 0 AND s.deleted_at IS NULL" +
		") SELECT id FROM tree"

	return baseWhere + " AND " + q("id") + " IN (" + cte + ")"
}

func sessionFilterPredicates(
	f SessionFilter, b *QueryBuilder, q func(string) string,
) ([]string, string) {
	var preds []string
	if f.Project != "" {
		preds = append(preds, q("project")+" = "+b.Add(f.Project))
	}
	if f.ExcludeProject != "" {
		preds = append(preds,
			q("project")+" != "+b.Add(f.ExcludeProject))
	}
	if f.Machine != "" {
		preds = append(preds,
			inPredicate(q("machine"), splitCSV(f.Machine), b))
	}
	if f.Agent != "" {
		preds = append(preds,
			inPredicate(q("agent"), splitCSV(f.Agent), b))
	}
	if f.Date != "" {
		preds = append(preds, b.dialect.dateExpr+" = "+
			b.dialect.dateParam(b.Add(f.Date)))
	}
	if f.DateFrom != "" {
		preds = append(preds, b.dialect.dateExpr+" >= "+
			b.dialect.dateParam(b.Add(f.DateFrom)))
	}
	if f.DateTo != "" {
		preds = append(preds, b.dialect.dateExpr+" <= "+
			b.dialect.dateParam(b.Add(f.DateTo)))
	}
	if f.ActiveSince != "" {
		preds = append(preds, b.dialect.activityExpr+" >= "+
			b.dialect.activityParam(b.Add(f.ActiveSince)))
	}
	if f.MinMessages > 0 {
		preds = append(preds,
			q("message_count")+" >= "+b.Add(f.MinMessages))
	}
	if f.MaxMessages > 0 {
		preds = append(preds,
			q("message_count")+" <= "+b.Add(f.MaxMessages))
	}
	if f.MinUserMessages > 0 {
		preds = append(preds,
			q("user_message_count")+" >= "+b.Add(f.MinUserMessages))
	}
	if pred := terminationPredicate(f.Termination, b, q); pred != "" {
		preds = append(preds, pred)
	}

	oneShotPred := ""
	if f.ExcludeOneShot {
		pred := q("user_message_count") + " > 1"
		if !f.ExcludeAutomated {
			pred = "(" + q("user_message_count") + " > 1 OR " +
				q("is_automated") + " = " +
				b.dialect.trueLiteral + ")"
		}
		if f.IncludeChildren {
			oneShotPred = pred
		} else {
			preds = append(preds, pred)
		}
	}
	if f.ExcludeAutomated {
		preds = append(preds, q("is_automated")+" = "+
			b.dialect.falseLiteral)
	}
	if len(f.Outcome) > 0 {
		preds = append(preds,
			inPredicate(q("outcome"), f.Outcome, b))
	}
	if len(f.HealthGrade) > 0 {
		preds = append(preds,
			inPredicate(q("health_grade"), f.HealthGrade, b))
	}
	if f.MinToolFailures != nil {
		preds = append(preds,
			q("tool_failure_signal_count")+" >= "+
				b.Add(*f.MinToolFailures))
	}
	if f.HasSecret {
		pred := q("secret_leak_count") + " > 0"
		versions := nonEmpty(f.SecretsRulesVersions)
		if len(versions) > 0 {
			pred += " AND " + inPredicate(
				q("secrets_rules_version"), versions, b)
		}
		preds = append(preds, pred)
	}
	if f.Starred {
		preds = append(preds,
			"EXISTS (SELECT 1 FROM starred_sessions ss WHERE ss.session_id = "+
				q("id")+")")
	}
	return preds, oneShotPred
}

// buildSessionBaseFilter returns a WHERE clause and args containing the base
// predicates (message_count > 0, deleted_at IS NULL) plus user-facing filter
// predicates (project, machine, agent, date, etc.) WITHOUT the relationship_type
// exclusion. Callers that handle root-vs-child discrimination externally (e.g.
// via buildCanonicalRootWhere) should use this instead of buildSessionFilter.
func buildSessionBaseFilter(f SessionFilter) (string, []any) {
	return BuildSessionBaseFilterSQL(f, SQLiteQueryDialect())
}

func inPredicate(col string, values []string, b *QueryBuilder) string {
	if len(values) == 0 {
		return "1 = 0"
	}
	if len(values) == 1 {
		return col + " = " + b.Add(values[0])
	}
	placeholders := make([]string, len(values))
	for i, v := range values {
		placeholders[i] = b.Add(v)
	}
	return col + " IN (" + strings.Join(placeholders, ",") + ")"
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func nonEmpty(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func terminationPredicate(
	status string, b *QueryBuilder, q func(string) string,
) string {
	if status == "" || status == "all" {
		return ""
	}
	now := time.Now().UTC()
	activeCutoff := now.Add(-activeWindow)
	staleCutoff := now.Add(-staleWindow)
	flagged := q("termination_status") +
		" IN ('tool_call_pending', 'truncated')"

	parts := strings.Split(status, ",")
	preds := make([]string, 0, len(parts))
	for _, p := range parts {
		switch strings.TrimSpace(p) {
		case "active":
			preds = append(preds, b.dialect.terminationExpr+" > "+
				b.terminationParam(activeCutoff))
		case "stale":
			preds = append(preds, "("+
				b.dialect.terminationExpr+" > "+
				b.terminationParam(staleCutoff)+" AND "+
				b.dialect.terminationExpr+" <= "+
				b.terminationParam(activeCutoff)+" AND "+
				flagged+")")
		case "unclean":
			preds = append(preds, "("+
				b.dialect.terminationExpr+" <= "+
				b.terminationParam(staleCutoff)+" AND "+
				flagged+")")
		case "clean":
			preds = append(preds,
				q("termination_status")+" = 'clean'")
		case "awaiting_user":
			preds = append(preds,
				q("termination_status")+" = 'awaiting_user'")
		}
	}
	if len(preds) == 0 {
		return ""
	}
	if len(preds) == 1 {
		return preds[0]
	}
	return "(" + strings.Join(preds, " OR ") + ")"
}

func (b *QueryBuilder) terminationParam(t time.Time) string {
	switch b.dialect.terminationKind {
	case timestampUnixSeconds:
		return b.Add(t.Unix())
	case timestampCast:
		return b.dialect.activityParam(b.Add(t.Format(time.RFC3339)))
	default:
		return b.dialect.activityParam(b.Add(t))
	}
}
