package db

// ProjectRule is one worktree mapping rule plus its provenance and current
// governed-session count. Rule identity across archives is
// (SourceArchiveID, Machine, PathPrefix), not the embedded
// WorktreeProjectMapping's ID/CreatedAt: those fields are local-archive-only
// conveniences populated from the SQLite rules table. When this type is
// served from a PostgreSQL or DuckDB mirror, the mirror tables carry neither
// column, so ID is zero and CreatedAt is empty — mirrors are read-only, so
// id-based mutations (enable/disable/delete) never apply there anyway.
type ProjectRule struct {
	WorktreeProjectMapping
	SourceArchiveID  string `json:"source_archive_id"`
	GovernedSessions int    `json:"governed_sessions"`
}

// ProjectRules is the full rules read for one machine: every rule for that
// machine (enabled and disabled) plus the machine typeahead list.
type ProjectRules struct {
	Machine  string        `json:"machine"`
	Machines []string      `json:"machines"`
	Rules    []ProjectRule `json:"rules"`
}
