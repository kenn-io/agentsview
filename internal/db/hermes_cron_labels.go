package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/export"
)

// ApplyHermesCronUsageLabels selects one recorded title per requested Usage job.
func ApplyHermesCronUsageLabels(ctx context.Context, query func(context.Context, string, ...any) (*sql.Rows, error), dialect QueryDialect, projects map[string]export.ProjectMapEntry) error {
	var labels []string
	for project := range projects {
		if strings.HasPrefix(project, "hermes-cron/") {
			labels = append(labels, project)
		}
	}
	started := "started_at"
	if dialect.emptyStringIsNull {
		started = "NULLIF(started_at, '')"
	}
	return queryChunked(labels, func(chunk []string) error {
		b := NewQueryBuilder(dialect, 0)
		rows, err := query(ctx, `SELECT project, session_name FROM (
			SELECT project, session_name, ROW_NUMBER() OVER (PARTITION BY project ORDER BY `+started+` DESC NULLS LAST, id DESC) AS cron_rank
			FROM sessions WHERE agent = 'hermes' AND deleted_at IS NULL AND session_name LIKE '% · %' AND `+inPredicate("project", chunk, b)+`
		) ranked WHERE cron_rank = 1`, b.Args()...)
		if err != nil {
			return fmt.Errorf("reading Hermes cron Usage labels: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var project, title string
			if err := rows.Scan(&project, &title); err != nil {
				return err
			}
			entry := projects[project]
			if name := export.HermesCronRecordedName(project, title); name != "" {
				entry.DisplayLabel = name + " · " + project
			}
			projects[project] = entry
		}
		return rows.Err()
	})
}
