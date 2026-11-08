package db

import (
	"database/sql"
	"strings"
)

// SessionExclusionCounts describes the CLI's default exclusions. Child counts
// sample the first MaxSessionLimit expanded rows when children are excluded.
type SessionExclusionCounts struct {
	OneShot            int  `json:"one_shot"`
	Automated          int  `json:"automated"`
	Children           int  `json:"children"`
	ChildrenLowerBound bool `json:"children_lower_bound"`
}

// WantsExclusionCounts keeps cursor pages and fully included lists cheap.
func (f SessionFilter) WantsExclusionCounts() bool {
	return f.ExclusionCounts != "" && f.Cursor == "" &&
		(f.ExcludeOneShot || f.ExcludeAutomated || !f.IncludeChildren)
}

// BuildSessionExclusionCountsSQL counts the visible and expanded selections in
// one scan. All selections use the same canonical filter builder and numbering.
func BuildSessionExclusionCountsSQL(f SessionFilter, dialect QueryDialect) (string, []any) {
	b := NewQueryBuilder(dialect, 0)
	count := func(filter SessionFilter) string {
		return dialect.SignedAggregate("COUNT(CASE WHEN " + buildSessionFilterWithBuilder(filter, b, "") + " THEN 1 END)")
	}
	visible := count(f)
	oneShot, automated := "0", "0"
	if f.ExcludeOneShot {
		expanded := f
		expanded.ExcludeOneShot = false
		oneShot = count(expanded)
	}
	if f.ExcludeAutomated {
		expanded := f
		expanded.ExcludeAutomated = false
		automated = count(expanded)
	}
	return "SELECT " + strings.Join([]string{visible, oneShot, automated}, ", ") + " FROM sessions", b.Args()
}

// SetExpandedTotals retains the original nonnegative exclusion differences.
func (c *SessionExclusionCounts) SetExpandedTotals(visible, oneShot, automated int) {
	c.OneShot = max(0, oneShot-visible)
	c.Automated = max(0, automated-visible)
}

// BuildSessionExclusionChildrenSQL preserves the expanded tree and sort while
// fetching only the relationship used by the bounded child probe.
func BuildSessionExclusionChildrenSQL(f SessionFilter, dialect QueryDialect) (string, []any) {
	included := f.IncludeChildren
	f.IncludeChildren, f.ExcludeOneShot, f.ExcludeAutomated = true, false, false
	b := NewQueryBuilder(dialect, 0)
	where := buildSessionFilterWithBuilder(f, b, "")
	if included {
		return "SELECT " + dialect.SignedAggregate("COUNT(*)") + " FROM sessions WHERE " + where, b.Args()
	}
	query := "SELECT relationship_type FROM sessions WHERE " + where + " " + b.OrderByClause(ResolveSort(f), f) + " " + b.Limit(MaxSessionLimit+1)
	return query, b.Args()
}

// ReadChildren commits a count only after the whole probe succeeds. Callers
// retain the visible page and flat counts when this best-effort query fails.
func (c *SessionExclusionCounts) ReadChildren(rows *sql.Rows, f SessionFilter, visible int) error {
	children, sampled := 0, 0
	if f.IncludeChildren {
		if rows.Next() {
			var total int
			if err := rows.Scan(&total); err != nil {
				return err
			}
			children = max(0, total-visible-c.OneShot-c.Automated)
		}
	} else {
		for rows.Next() {
			var relationship string
			if err := rows.Scan(&relationship); err != nil {
				return err
			}
			if sampled < MaxSessionLimit && (relationship == "subagent" || relationship == "fork") {
				children++
			}
			sampled++
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	c.Children = children
	c.ChildrenLowerBound = children > 0 && sampled > MaxSessionLimit
	return nil
}
