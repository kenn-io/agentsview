package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/mattn/go-sqlite3"
	"go.kenn.io/agentsview/internal/parser"
)

var ErrWorktreeMappingDuplicate = errors.New("worktree mapping already exists")
var ErrWorktreeMappingInvalid = errors.New("invalid worktree mapping")

const (
	WorktreeMappingLayoutExplicit         = "explicit"
	WorktreeMappingLayoutRepoDotWorktrees = "repo_dot_worktrees"
)

type WorktreeProjectMapping struct {
	ID              int64  `json:"id"`
	Machine         string `json:"machine"`
	PathPrefix      string `json:"path_prefix"`
	Layout          string `json:"layout"`
	Project         string `json:"project"`
	OriginalProject string `json:"original_project"`
	Enabled         bool   `json:"enabled"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
}

type ApplyWorktreeProjectMappingsResult struct {
	MatchedSessions int `json:"matched_sessions"`
	UpdatedSessions int `json:"updated_sessions"`
}

func normalizeWorktreeMapping(
	machine string,
	pathPrefix string,
	layout string,
	project string,
) (WorktreeProjectMapping, error) {
	machine = strings.TrimSpace(machine)
	if machine == "" {
		return WorktreeProjectMapping{}, fmt.Errorf("%w: machine is required", ErrWorktreeMappingInvalid)
	}

	pathPrefix = normalizedMappingPath(pathPrefix)
	if pathPrefix == "" || pathPrefix == "." {
		return WorktreeProjectMapping{}, fmt.Errorf("%w: path_prefix is required", ErrWorktreeMappingInvalid)
	}

	layout = strings.TrimSpace(layout)
	if layout == "" {
		layout = WorktreeMappingLayoutExplicit
	}
	switch layout {
	case WorktreeMappingLayoutExplicit, WorktreeMappingLayoutRepoDotWorktrees:
	default:
		return WorktreeProjectMapping{}, fmt.Errorf(
			"%w: layout must be %s or %s",
			ErrWorktreeMappingInvalid,
			WorktreeMappingLayoutExplicit,
			WorktreeMappingLayoutRepoDotWorktrees,
		)
	}

	project = strings.TrimSpace(project)
	if layout == WorktreeMappingLayoutExplicit {
		if project == "" {
			return WorktreeProjectMapping{}, fmt.Errorf(
				"%w: project is required",
				ErrWorktreeMappingInvalid,
			)
		}
		project = parser.NormalizeName(project)
	} else {
		project = ""
	}

	return WorktreeProjectMapping{
		Machine:    machine,
		PathPrefix: pathPrefix,
		Layout:     layout,
		Project:    parser.NormalizeName(project),
	}, nil
}

func normalizedMappingPath(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, `\`, "/"))
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, "//") && !strings.HasPrefix(value, "///") {
		rest := path.Clean(strings.TrimPrefix(value, "//"))
		parts := strings.Split(rest, "/")
		if len(parts) >= 2 && parts[0] != "" && parts[0] != "." &&
			parts[0] != ".." && parts[1] != "" && parts[1] != "." &&
			parts[1] != ".." {
			normalized := "//" + rest
			if len(parts) == 2 {
				return normalized + "/"
			}
			return normalized
		}
	}
	driveAbsolute := len(value) >= 3 && value[1] == ':' && value[2] == '/'
	normalized := path.Clean(value)
	if driveAbsolute && normalized == value[:2] {
		return normalized + "/"
	}
	return normalized
}

func worktreePathMatches(prefix string, cwd string) bool {
	prefix = normalizedMappingPath(prefix)
	cwd = normalizedMappingPath(cwd)
	if prefix == "" || prefix == "." || cwd == "" || cwd == "." {
		return false
	}
	if cwd == prefix {
		return true
	}
	if strings.HasPrefix(prefix, "//") != strings.HasPrefix(cwd, "//") {
		return false
	}
	if len(prefix) == 2 && prefix[1] == ':' {
		return false
	}
	if prefix == "/" {
		return strings.HasPrefix(cwd, "/") && !strings.HasPrefix(cwd, "//")
	}
	return strings.HasPrefix(cwd, strings.TrimSuffix(prefix, "/")+"/")
}

func scanWorktreeMapping(rows *sql.Rows) (WorktreeProjectMapping, error) {
	var m WorktreeProjectMapping
	var enabled int
	if err := rows.Scan(
		&m.ID,
		&m.Machine,
		&m.PathPrefix,
		&m.Layout,
		&m.Project,
		&m.OriginalProject,
		&enabled,
		&m.CreatedAt,
		&m.UpdatedAt,
	); err != nil {
		return m, err
	}
	if m.Layout == "" {
		m.Layout = WorktreeMappingLayoutExplicit
	}
	m.Enabled = enabled != 0
	return m, nil
}

func scanWorktreeMappingRow(row rowScanner) (WorktreeProjectMapping, error) {
	var m WorktreeProjectMapping
	var enabled int
	if err := row.Scan(
		&m.ID,
		&m.Machine,
		&m.PathPrefix,
		&m.Layout,
		&m.Project,
		&m.OriginalProject,
		&enabled,
		&m.CreatedAt,
		&m.UpdatedAt,
	); err != nil {
		return m, err
	}
	if m.Layout == "" {
		m.Layout = WorktreeMappingLayoutExplicit
	}
	m.Enabled = enabled != 0
	return m, nil
}

func (db *DB) ListWorktreeProjectMappings(
	ctx context.Context,
	machine string,
) ([]WorktreeProjectMapping, error) {
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT id, machine, path_prefix, layout, project, original_project,
			enabled, created_at, updated_at
		FROM worktree_project_mappings
		WHERE machine = ?
		ORDER BY path_prefix`, strings.TrimSpace(machine))
	if err != nil {
		return nil, fmt.Errorf("listing worktree mappings: %w", err)
	}
	defer rows.Close()

	mappings := []WorktreeProjectMapping{}
	for rows.Next() {
		m, err := scanWorktreeMapping(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning worktree mapping: %w", err)
		}
		mappings = append(mappings, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating worktree mappings: %w", err)
	}
	return mappings, nil
}

func (db *DB) CreateWorktreeProjectMapping(
	ctx context.Context,
	m WorktreeProjectMapping,
) (WorktreeProjectMapping, error) {
	normalized, err := normalizeWorktreeMapping(
		m.Machine, m.PathPrefix, m.Layout, m.Project,
	)
	if err != nil {
		return WorktreeProjectMapping{}, err
	}
	normalized.OriginalProject = m.OriginalProject

	enabled := 0
	if m.Enabled {
		enabled = 1
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	res, err := db.getWriter().ExecContext(ctx, `
		INSERT INTO worktree_project_mappings
			(machine, path_prefix, layout, project, original_project, enabled)
		VALUES (?, ?, ?, ?, ?, ?)`,
		normalized.Machine,
		normalized.PathPrefix,
		normalized.Layout,
		normalized.Project,
		normalized.OriginalProject,
		enabled,
	)
	if err != nil {
		if isSQLiteUniqueConstraint(err) {
			return WorktreeProjectMapping{}, ErrWorktreeMappingDuplicate
		}
		return WorktreeProjectMapping{}, fmt.Errorf("creating worktree mapping: %w", err)
	}
	normalized.ID, _ = res.LastInsertId()
	return db.getWorktreeProjectMappingLocked(ctx, normalized.Machine, normalized.ID)
}

func (db *DB) UpdateWorktreeProjectMapping(
	ctx context.Context,
	machine string,
	id int64,
	patch WorktreeProjectMapping,
) (WorktreeProjectMapping, error) {
	normalized, err := normalizeWorktreeMapping(machine, patch.PathPrefix, patch.Layout, patch.Project)
	if err != nil {
		return WorktreeProjectMapping{}, err
	}

	enabled := 0
	if patch.Enabled {
		enabled = 1
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	res, err := db.getWriter().ExecContext(ctx, `
		UPDATE worktree_project_mappings
		SET path_prefix = ?,
			layout = ?,
			project = ?,
			original_project = CASE
				WHEN original_project = '' THEN ?
				ELSE original_project
			END,
			enabled = ?,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE id = ? AND machine = ?`,
		normalized.PathPrefix,
		normalized.Layout,
		normalized.Project,
		patch.OriginalProject,
		enabled,
		id,
		normalized.Machine,
	)
	if err != nil {
		if isSQLiteUniqueConstraint(err) {
			return WorktreeProjectMapping{}, ErrWorktreeMappingDuplicate
		}
		return WorktreeProjectMapping{}, fmt.Errorf("updating worktree mapping: %w", err)
	}
	changed, _ := res.RowsAffected()
	if changed == 0 {
		return WorktreeProjectMapping{}, sql.ErrNoRows
	}
	return db.getWorktreeProjectMappingLocked(ctx, normalized.Machine, id)
}

func (db *DB) DeleteWorktreeProjectMapping(
	ctx context.Context,
	machine string,
	id int64,
) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	res, err := db.getWriter().ExecContext(ctx,
		`DELETE FROM worktree_project_mappings WHERE id = ? AND machine = ?`,
		id,
		strings.TrimSpace(machine),
	)
	if err != nil {
		return fmt.Errorf("deleting worktree mapping: %w", err)
	}
	changed, _ := res.RowsAffected()
	if changed == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (db *DB) getWorktreeProjectMappingLocked(
	ctx context.Context,
	machine string,
	id int64,
) (WorktreeProjectMapping, error) {
	row := db.getWriter().QueryRowContext(ctx, `
		SELECT id, machine, path_prefix, layout, project, original_project,
			enabled, created_at, updated_at
		FROM worktree_project_mappings
		WHERE id = ? AND machine = ?`,
		id,
		machine,
	)
	m, err := scanWorktreeMappingRow(row)
	if err != nil {
		return WorktreeProjectMapping{}, err
	}
	return m, nil
}

// GetWorktreeProjectMapping returns a mapping by its globally unique ID.
func (db *DB) GetWorktreeProjectMapping(
	ctx context.Context,
	id int64,
) (WorktreeProjectMapping, error) {
	row := db.getReader().QueryRowContext(ctx, `
		SELECT id, machine, path_prefix, layout, project, original_project,
			enabled, created_at, updated_at
		FROM worktree_project_mappings
		WHERE id = ?`, id)
	return scanWorktreeMappingRow(row)
}

// ListWorktreeProjectMappingMachines returns every distinct machine represented
// by a live session or a stored mapping.
func (db *DB) ListWorktreeProjectMappingMachines(
	ctx context.Context,
) ([]string, error) {
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT machine FROM sessions WHERE deleted_at IS NULL AND machine != ''
		UNION
		SELECT machine FROM worktree_project_mappings WHERE machine != ''
		ORDER BY machine`)
	if err != nil {
		return nil, fmt.Errorf("listing worktree mapping machines: %w", err)
	}
	defer rows.Close()

	machines := []string{}
	for rows.Next() {
		var machine string
		if err := rows.Scan(&machine); err != nil {
			return nil, fmt.Errorf("scanning worktree mapping machine: %w", err)
		}
		machines = append(machines, machine)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating worktree mapping machines: %w", err)
	}
	return machines, nil
}

// ListActiveWorktreeProjectMappingMachines returns the distinct machines with
// at least one enabled mapping. Resync uses this narrower set so applying
// persistent rules does not scan machines that have no active rules.
func (db *DB) ListActiveWorktreeProjectMappingMachines(
	ctx context.Context,
) ([]string, error) {
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT DISTINCT machine
		FROM worktree_project_mappings
		WHERE enabled = 1 AND machine != ''
		ORDER BY machine`)
	if err != nil {
		return nil, fmt.Errorf("listing active worktree mapping machines: %w", err)
	}
	defer rows.Close()

	machines := []string{}
	for rows.Next() {
		var machine string
		if err := rows.Scan(&machine); err != nil {
			return nil, fmt.Errorf("scanning active worktree mapping machine: %w", err)
		}
		machines = append(machines, machine)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating active worktree mapping machines: %w", err)
	}
	return machines, nil
}

func (db *DB) ResolveWorktreeProjectMapping(
	ctx context.Context,
	machine string,
	cwd string,
	currentProject string,
) (string, bool, error) {
	mappings, err := db.activeWorktreeProjectMappings(ctx, machine)
	if err != nil {
		return currentProject, false, err
	}
	project, ok := ResolveWorktreeProjectFromMappings(
		mappings, cwd, currentProject,
	)
	return project, ok, nil
}

// ListActiveWorktreeProjectMappings returns enabled mappings
// for a machine in resolution order, with the longest path
// prefixes first.
func (db *DB) ListActiveWorktreeProjectMappings(
	ctx context.Context,
	machine string,
) ([]WorktreeProjectMapping, error) {
	return db.activeWorktreeProjectMappings(ctx, machine)
}

// CopyWorktreeProjectMappingsFrom copies persistent worktree mappings from a
// source DB into this DB. Omit id so source primary keys cannot shadow
// destination rows. UNIQUE(machine, path_prefix) conflicts preserve the
// destination mapping while filling its set-once original project context.
func (db *DB) CopyWorktreeProjectMappingsFrom(sourcePath string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	ctx := context.Background()
	conn, err := db.getWriter().Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquiring connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(
		ctx, "ATTACH DATABASE ? AS old_db", sourcePath,
	); err != nil {
		return fmt.Errorf("attaching source db: %w", err)
	}
	defer func() {
		_, _ = conn.ExecContext(ctx, "DETACH DATABASE old_db")
	}()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin worktree mapping copy tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if oldDBHasTable(ctx, tx, "worktree_project_mappings") {
		layoutSelect := "'" + WorktreeMappingLayoutExplicit + "'"
		if oldDBHasColumn(ctx, tx, "worktree_project_mappings", "layout") {
			layoutSelect = "layout"
		}
		originalProjectSelect := "''"
		if oldDBHasColumn(ctx, tx, "worktree_project_mappings", "original_project") {
			originalProjectSelect = "original_project"
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO main.worktree_project_mappings
				(machine, path_prefix, layout, project, original_project,
				 enabled, created_at, updated_at)
			SELECT machine, path_prefix, `+layoutSelect+`, project,
				`+originalProjectSelect+`, enabled, created_at, updated_at
			FROM old_db.worktree_project_mappings
			WHERE TRUE
			ON CONFLICT(machine, path_prefix) DO UPDATE SET
				original_project = CASE
					WHEN worktree_project_mappings.original_project = ''
						THEN excluded.original_project
					ELSE worktree_project_mappings.original_project
				END`); err != nil {
			return fmt.Errorf("copying worktree project mappings: %w", err)
		}
	}

	return tx.Commit()
}

// ResolveWorktreeProjectFromMappings applies the same longest
// prefix worktree mapping semantics as ResolveWorktreeProjectMapping
// to an already-loaded mapping set. It defensively sorts a copy so
// callers cannot accidentally depend on input order.
func ResolveWorktreeProjectFromMappings(
	mappings []WorktreeProjectMapping,
	cwd string,
	currentProject string,
) (string, bool) {
	mappings = sortedWorktreeProjectMappings(mappings)
	return ResolveWorktreeProjectFromSortedMappings(
		mappings, cwd, currentProject,
	)
}

// ResolveWorktreeProjectFromSortedMappings applies longest-prefix
// semantics to a mapping set already sorted by descending path prefix
// length. Use this in hot paths with mappings loaded by
// ListActiveWorktreeProjectMappings.
func ResolveWorktreeProjectFromSortedMappings(
	mappings []WorktreeProjectMapping,
	cwd string,
	currentProject string,
) (string, bool) {
	for _, mapping := range mappings {
		if !worktreePathMatches(mapping.PathPrefix, cwd) {
			continue
		}
		if project, ok := resolveWorktreeProjectFromMapping(
			mapping, cwd, currentProject,
		); ok {
			return project, true
		}
	}
	return currentProject, false
}

func sortedWorktreeProjectMappings(
	mappings []WorktreeProjectMapping,
) []WorktreeProjectMapping {
	sorted := append([]WorktreeProjectMapping(nil), mappings...)
	sortWorktreeProjectMappings(sorted)
	return sorted
}

func sortWorktreeProjectMappings(mappings []WorktreeProjectMapping) {
	sort.SliceStable(mappings, func(i, j int) bool {
		left := mappings[i].PathPrefix
		right := mappings[j].PathPrefix
		if len(left) != len(right) {
			return len(left) > len(right)
		}
		return left < right
	})
}

func (db *DB) activeWorktreeProjectMappings(
	ctx context.Context,
	machine string,
) ([]WorktreeProjectMapping, error) {
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT id, machine, path_prefix, layout, project, original_project,
			enabled, created_at, updated_at
		FROM worktree_project_mappings
		WHERE machine = ? AND enabled = 1
		ORDER BY length(path_prefix) DESC, path_prefix`,
		strings.TrimSpace(machine),
	)
	if err != nil {
		return nil, fmt.Errorf("querying active worktree mappings: %w", err)
	}
	defer rows.Close()

	mappings := []WorktreeProjectMapping{}
	for rows.Next() {
		m, err := scanWorktreeMapping(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning active worktree mapping: %w", err)
		}
		mappings = append(mappings, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating active worktree mappings: %w", err)
	}
	return mappings, nil
}

func resolveWorktreeProjectFromMapping(
	mapping WorktreeProjectMapping,
	cwd string,
	currentProject string,
) (string, bool) {
	switch mapping.Layout {
	case "", WorktreeMappingLayoutExplicit:
		if mapping.Project == "" {
			return currentProject, false
		}
		return mapping.Project, true
	case WorktreeMappingLayoutRepoDotWorktrees:
		project, _, ok := resolveRepoDotWorktrees(mapping.PathPrefix, cwd)
		if !ok {
			return currentProject, false
		}
		return project, true
	default:
		return currentProject, false
	}
}

// resolveRepoDotWorktrees resolves a cwd under a repo_dot_worktrees mapping to
// its project name and the repo.worktrees directory shared by all branches.
func resolveRepoDotWorktrees(
	pathPrefix string,
	cwd string,
) (string, string, bool) {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return "", "", false
	}
	pathPrefix = normalizedMappingPath(pathPrefix)
	cwd = normalizedMappingPath(cwd)
	if !worktreePathMatches(pathPrefix, cwd) || cwd == pathPrefix {
		return "", "", false
	}
	rel := strings.TrimPrefix(cwd, strings.TrimSuffix(pathPrefix, "/")+"/")
	if rel == cwd || rel == "" {
		return "", "", false
	}
	idx := strings.IndexRune(rel, '/')
	if idx < 0 {
		return "", "", false
	}
	first := rel[:idx]
	if !strings.HasSuffix(first, ".worktrees") {
		return "", "", false
	}
	repo := strings.TrimSpace(strings.TrimSuffix(first, ".worktrees"))
	if repo == "" {
		return "", "", false
	}
	return parser.NormalizeName(repo), path.Join(pathPrefix, first), true
}

type worktreeMappingSessionRow struct {
	id       string
	machine  string
	project  string
	cwd      string
	filePath string
	matchCwd string
}

type worktreeMappingSessionUpdate struct {
	id             string
	machine        string
	cwd            string
	currentProject string
	nextProject    string
}

func loadActiveWorktreeMappingsTx(
	ctx context.Context,
	tx *sql.Tx,
	machine string,
) ([]WorktreeProjectMapping, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, machine, path_prefix, layout, project, original_project,
			enabled, created_at, updated_at
		FROM worktree_project_mappings
		WHERE machine = ? AND enabled = 1
		ORDER BY length(path_prefix) DESC, path_prefix`,
		machine,
	)
	if err != nil {
		return nil, fmt.Errorf("querying active worktree mappings: %w", err)
	}
	return scanWorktreeMappings(rows)
}

func loadActiveWorktreeMappingsByMachineTx(
	ctx context.Context,
	tx *sql.Tx,
	machines map[string]bool,
) (map[string][]WorktreeProjectMapping, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, machine, path_prefix, layout, project, original_project,
			enabled, created_at, updated_at
		FROM worktree_project_mappings
		WHERE enabled = 1
		ORDER BY machine, length(path_prefix) DESC, path_prefix`,
	)
	if err != nil {
		return nil, fmt.Errorf("querying active worktree mappings: %w", err)
	}
	mappings, err := scanWorktreeMappings(rows)
	if err != nil {
		return nil, err
	}

	mappingsByMachine := map[string][]WorktreeProjectMapping{}
	for _, mapping := range mappings {
		if machines[mapping.Machine] {
			mappingsByMachine[mapping.Machine] = append(
				mappingsByMachine[mapping.Machine],
				mapping,
			)
		}
	}
	return mappingsByMachine, nil
}

func scanWorktreeMappings(rows *sql.Rows) ([]WorktreeProjectMapping, error) {
	defer rows.Close()

	mappings := []WorktreeProjectMapping{}
	for rows.Next() {
		mapping, err := scanWorktreeMapping(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning active worktree mapping: %w", err)
		}
		mappings = append(mappings, mapping)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating active worktree mappings: %w", err)
	}
	return mappings, nil
}

func applyMappingToSessionRow(
	mappings []WorktreeProjectMapping,
	row worktreeMappingSessionRow,
) (worktreeMappingSessionUpdate, bool, bool) {
	matchCwd := row.matchCwd
	if matchCwd == "" {
		matchCwd = row.cwd
	}
	project, ok := ResolveWorktreeProjectFromSortedMappings(mappings, matchCwd, row.project)
	if !ok {
		return worktreeMappingSessionUpdate{}, false, false
	}
	if project == row.project {
		return worktreeMappingSessionUpdate{}, true, false
	}
	return worktreeMappingSessionUpdate{
		id:             row.id,
		machine:        row.machine,
		cwd:            row.cwd,
		currentProject: row.project,
		nextProject:    project,
	}, true, true
}

func applyWorktreeMappingMatchCwdFromSiblings(
	rows []worktreeMappingSessionRow,
	siblingKey func(worktreeMappingSessionRow) string,
	resolveProject func(row worktreeMappingSessionRow, cwd string) (string, bool),
) {
	type siblingCandidate struct {
		cwd     string
		project string
	}
	candidatesBySibling := map[string][]siblingCandidate{}
	unresolvedBySibling := map[string]bool{}
	for _, row := range rows {
		key := siblingKey(row)
		if key == "" || row.cwd == "" {
			continue
		}
		project, ok := resolveProject(row, row.cwd)
		if !ok {
			unresolvedBySibling[key] = true
			continue
		}
		candidates := candidatesBySibling[key]
		alreadySeen := false
		for _, candidate := range candidates {
			if candidate.project == project {
				alreadySeen = true
				break
			}
		}
		if !alreadySeen {
			candidatesBySibling[key] = append(
				candidates, siblingCandidate{cwd: row.cwd, project: project},
			)
		}
	}

	// Only fall back when every non-empty sibling resolves to a mapping and
	// all of them agree on the same project; an unmapped sibling or
	// conflicting projects mean the fallback would be a guess.
	fallbackBySibling := map[string]string{}
	for key, candidates := range candidatesBySibling {
		if len(candidates) == 1 && !unresolvedBySibling[key] {
			fallbackBySibling[key] = candidates[0].cwd
		}
	}

	for i, row := range rows {
		row.matchCwd = row.cwd
		key := siblingKey(row)
		if row.cwd == "" && key != "" {
			row.matchCwd = fallbackBySibling[key]
		}
		rows[i] = row
	}
}

func updateSessionProjectTx(
	ctx context.Context,
	tx *sql.Tx,
	update worktreeMappingSessionUpdate,
	bumpLocalModifiedAt bool,
) (int, error) {
	updateSQL := `
		UPDATE sessions
		SET project = ?
		WHERE id = ?
			AND machine = ?
			AND deleted_at IS NULL
			AND cwd = ?
			AND project = ?`
	if bumpLocalModifiedAt {
		updateSQL = `
			UPDATE sessions
			SET project = ?,
				local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
			WHERE id = ?
				AND machine = ?
				AND deleted_at IS NULL
				AND cwd = ?
				AND project = ?`
	}
	res, err := tx.ExecContext(ctx, updateSQL,
		update.nextProject,
		update.id,
		update.machine,
		update.cwd,
		update.currentProject,
	)
	if err != nil {
		return 0, fmt.Errorf(
			"applying worktree mapping to session %s: %w",
			update.id,
			err,
		)
	}
	changed, _ := res.RowsAffected()
	return int(changed), nil
}

func (db *DB) ApplyWorktreeProjectMappings(
	ctx context.Context,
	machine string,
) (ApplyWorktreeProjectMappingsResult, error) {
	return db.applyWorktreeProjectMappings(ctx, machine, true)
}

func (db *DB) ApplyWorktreeProjectMappingsFromSync(
	ctx context.Context,
	machine string,
) (ApplyWorktreeProjectMappingsResult, error) {
	return db.applyWorktreeProjectMappings(ctx, machine, false)
}

func (db *DB) applyWorktreeProjectMappings(
	ctx context.Context,
	machine string,
	bumpLocalModifiedAt bool,
) (ApplyWorktreeProjectMappingsResult, error) {
	machine = strings.TrimSpace(machine)

	db.mu.Lock()
	defer db.mu.Unlock()

	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return ApplyWorktreeProjectMappingsResult{}, fmt.Errorf(
			"beginning worktree mapping apply: %w", err,
		)
	}
	defer func() { _ = tx.Rollback() }()

	mappings, err := loadActiveWorktreeMappingsTx(ctx, tx, machine)
	if err != nil {
		return ApplyWorktreeProjectMappingsResult{}, fmt.Errorf(
			"loading active worktree mappings: %w", err,
		)
	}

	evaluation, err := evaluateWorktreeMappingsTx(
		ctx, tx, machine, mappings, nil, "",
	)
	if err != nil {
		return ApplyWorktreeProjectMappingsResult{}, err
	}
	result := ApplyWorktreeProjectMappingsResult{
		MatchedSessions: evaluation.matched,
	}
	affected := map[string]struct{}{}
	for _, update := range evaluation.updates {
		changed, err := updateSessionProjectTx(
			ctx, tx, update, bumpLocalModifiedAt,
		)
		if err != nil {
			return result, err
		}
		result.UpdatedSessions += changed
		if changed > 0 {
			affected[update.currentProject] = struct{}{}
			affected[update.nextProject] = struct{}{}
		}
	}
	if len(affected) > 0 {
		if err := rebuildProjectIdentityAggregatesTx(
			ctx, tx, machine, sortedSetKeys(affected),
		); err != nil {
			return result, err
		}
	}
	if err := tx.Commit(); err != nil {
		return result, fmt.Errorf("committing worktree mapping apply: %w", err)
	}
	return result, nil
}

func (db *DB) ApplyWorktreeProjectMappingToSession(
	ctx context.Context,
	machine string,
	sessionID string,
	cwd string,
	currentProject string,
) (bool, error) {
	return db.applyWorktreeProjectMappingToSession(
		ctx, machine, sessionID, cwd, currentProject, true,
	)
}

func (db *DB) ApplyWorktreeProjectMappingToSessionFromSync(
	ctx context.Context,
	machine string,
	sessionID string,
	cwd string,
	currentProject string,
) (bool, error) {
	return db.applyWorktreeProjectMappingToSession(
		ctx, machine, sessionID, cwd, currentProject, false,
	)
}

func (db *DB) applyWorktreeProjectMappingToSession(
	ctx context.Context,
	machine string,
	sessionID string,
	cwd string,
	currentProject string,
	bumpLocalModifiedAt bool,
) (bool, error) {
	if err := db.requireWritable(); err != nil {
		return false, err
	}
	machine = strings.TrimSpace(machine)
	sessionID = strings.TrimSpace(sessionID)
	if machine == "" || sessionID == "" {
		return false, nil
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf(
			"beginning worktree mapping session apply: %w", err,
		)
	}
	defer func() { _ = tx.Rollback() }()

	mappings, err := loadActiveWorktreeMappingsTx(ctx, tx, machine)
	if err != nil {
		return false, fmt.Errorf("loading active worktree mappings: %w", err)
	}

	evaluation, err := evaluateWorktreeMappingsTx(
		ctx, tx, machine, mappings, nil, sessionID,
	)
	if err != nil {
		return false, fmt.Errorf(
			"evaluating session %s for worktree mapping apply: %w",
			sessionID,
			err,
		)
	}
	if len(evaluation.updates) == 0 {
		return false, nil
	}

	changed, err := updateSessionProjectTx(
		ctx, tx, evaluation.updates[0], bumpLocalModifiedAt,
	)
	if err != nil {
		return false, err
	}
	if changed > 0 {
		update := evaluation.updates[0]
		if err := reconcileSessionProjectIdentityAggregatesTx(ctx, tx, sessionID, []string{
			update.currentProject,
			update.nextProject,
		}); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf(
			"committing worktree mapping session apply: %w", err,
		)
	}
	return changed > 0, nil
}

func (db *DB) ApplyWorktreeProjectMappingsToSessionsByPath(
	ctx context.Context,
	filePath string,
) (ApplyWorktreeProjectMappingsResult, error) {
	return db.applyWorktreeProjectMappingsToSessionsByPath(
		ctx, filePath, true,
	)
}

func (db *DB) ApplyWorktreeProjectMappingsToSessionsByPathFromSync(
	ctx context.Context,
	filePath string,
) (ApplyWorktreeProjectMappingsResult, error) {
	return db.applyWorktreeProjectMappingsToSessionsByPath(
		ctx, filePath, false,
	)
}

func (db *DB) applyWorktreeProjectMappingsToSessionsByPath(
	ctx context.Context,
	filePath string,
	bumpLocalModifiedAt bool,
) (ApplyWorktreeProjectMappingsResult, error) {
	if err := db.requireWritable(); err != nil {
		return ApplyWorktreeProjectMappingsResult{}, err
	}
	if filePath == "" {
		return ApplyWorktreeProjectMappingsResult{}, nil
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return ApplyWorktreeProjectMappingsResult{}, fmt.Errorf(
			"beginning worktree mapping path apply: %w", err,
		)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `
		SELECT id, machine, project, cwd, file_path
		FROM sessions
		WHERE file_path = ? AND deleted_at IS NULL`,
		filePath,
	)
	if err != nil {
		return ApplyWorktreeProjectMappingsResult{}, fmt.Errorf(
			"querying sessions for worktree mapping path apply: %w", err,
		)
	}

	var sessions []worktreeMappingSessionRow
	machines := map[string]bool{}
	for rows.Next() {
		var row worktreeMappingSessionRow
		var rowFilePath sql.NullString
		if err := rows.Scan(
			&row.id, &row.machine, &row.project, &row.cwd, &rowFilePath,
		); err != nil {
			rows.Close()
			return ApplyWorktreeProjectMappingsResult{}, fmt.Errorf(
				"scanning session for worktree mapping path apply: %w",
				err,
			)
		}
		if rowFilePath.Valid {
			row.filePath = rowFilePath.String
		}
		sessions = append(sessions, row)
		machines[row.machine] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return ApplyWorktreeProjectMappingsResult{}, fmt.Errorf(
			"iterating sessions for worktree mapping path apply: %w",
			err,
		)
	}
	if err := rows.Close(); err != nil {
		return ApplyWorktreeProjectMappingsResult{}, fmt.Errorf(
			"closing worktree mapping path apply rows: %w", err,
		)
	}
	if len(sessions) == 0 {
		return ApplyWorktreeProjectMappingsResult{}, nil
	}

	mappingsByMachine, err := loadActiveWorktreeMappingsByMachineTx(
		ctx, tx, machines,
	)
	if err != nil {
		return ApplyWorktreeProjectMappingsResult{}, fmt.Errorf(
			"loading active worktree mappings: %w", err,
		)
	}

	applyWorktreeMappingMatchCwdFromSiblings(sessions, func(row worktreeMappingSessionRow) string {
		return row.machine + "|" + strings.TrimSpace(row.filePath)
	}, func(row worktreeMappingSessionRow, cwd string) (string, bool) {
		return ResolveWorktreeProjectFromSortedMappings(
			mappingsByMachine[row.machine], cwd, row.project,
		)
	})

	var result ApplyWorktreeProjectMappingsResult
	for _, session := range sessions {
		update, matched, shouldUpdate := applyMappingToSessionRow(
			mappingsByMachine[session.machine],
			session,
		)
		if !matched {
			continue
		}
		result.MatchedSessions++
		if !shouldUpdate {
			continue
		}
		changed, err := updateSessionProjectTx(
			ctx, tx, update, bumpLocalModifiedAt,
		)
		if err != nil {
			return result, err
		}
		result.UpdatedSessions += changed
	}
	if err := tx.Commit(); err != nil {
		return result, fmt.Errorf(
			"committing worktree mapping path apply: %w", err,
		)
	}
	return result, nil
}

func isSQLiteUniqueConstraint(err error) bool {
	var sqliteErr sqlite3.Error
	return errors.As(err, &sqliteErr) &&
		sqliteErr.ExtendedCode == sqlite3.ErrConstraintUnique
}
