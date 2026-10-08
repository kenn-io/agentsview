package readbase

import "go.kenn.io/agentsview/internal/db"

func VisibleArchivesSQL() string {
	return `SELECT DISTINCT source_archive_id
		FROM sessions
		WHERE deleted_at IS NULL AND source_archive_id != ''`
}

func MachinesSQL() string {
	return `SELECT machine FROM sessions WHERE deleted_at IS NULL AND machine != ''
		UNION DISTINCT
		SELECT machine FROM source_worktree_project_mappings WHERE machine != ''
		ORDER BY machine`
}

func SourceArchivesSQL() string {
	return `SELECT source_archive_id, source_archive_salt
		FROM source_archives ORDER BY source_archive_id`
}

// A nil machine selects all machines; non-nil selects the exact value, including empty.
func MappingsSQL(machine *string, dialect db.QueryDialect) (string, []any) {
	query := `SELECT source_archive_id, machine, path_prefix, layout, project,
		original_project, enabled, updated_at
		FROM source_worktree_project_mappings`
	if machine == nil {
		return query + ` ORDER BY source_archive_id, machine, path_prefix`, nil
	}
	b := db.NewQueryBuilder(dialect, 0)
	query += ` WHERE machine = ` + b.Add(*machine) + ` ORDER BY path_prefix, source_archive_id`
	return query, b.Args()
}

// A nil machine selects all machines; non-nil selects the exact value, including empty.
func CandidateRowsSQL(machine *string, dialect db.QueryDialect) (string, []any) {
	query := `SELECT id, machine, project, cwd, COALESCE(file_path, ''),
		project_assigned, source_archive_id
		FROM sessions WHERE deleted_at IS NULL AND source_archive_id != ''`
	b := db.NewQueryBuilder(dialect, 0)
	if machine != nil {
		query += ` AND machine = ` + b.Add(*machine)
	}
	query += ` AND (source_archive_id, machine) IN
		(SELECT source_archive_id, machine FROM source_worktree_project_mappings WHERE enabled`
	if machine != nil {
		query += ` AND machine = ` + b.Add(*machine)
	}
	query += `)`
	if machine == nil {
		return query, nil
	}
	return query, b.Args()
}

func ArchiveSessionsSQL(filter db.ProjectDateFilter, dialect db.QueryDialect) (string, []any) {
	where, args := db.BuildSessionBaseFilterSQL(filter.SessionFilter(), dialect)
	return `SELECT id, project FROM sessions WHERE ` + where + ` ORDER BY id`, args
}
