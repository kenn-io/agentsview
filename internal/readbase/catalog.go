package readbase

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
)

// CatalogBackend supplies catalog SQL and backend-specific typed loaders.
type CatalogBackend interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	VisibleArchivesSQL() string
	MappingsSQL(*string) (string, []any)
	CandidateRowsSQL(*string) (string, []any)
	MachinesSQL() string
	ArchiveSessionsSQL(db.ProjectDateFilter) (string, []any)
	SourceArchivesSQL() string
	InventoryAggregate(context.Context, db.ProjectDateFilter) (map[string]db.ProjectInventoryAgg, error)
	ListProjectIdentityObservations(context.Context, []string) ([]export.ProjectIdentityObservation, error)
	// Inventory and candidates use the backend identity map so ClickHouse retains its cache.
	BuildProjectIdentityMap(context.Context, []string) (map[string]export.ProjectMapEntry, error)
	WorktreeCandidateSessions(context.Context, []string) ([]db.WorktreeCandidateSession, error)
}

// Catalog owns multi-archive catalog reads; each backend supplies every SQL operation.
type Catalog struct {
	backend CatalogBackend
	name    string
}

func NewCatalog(backend CatalogBackend, name string) *Catalog {
	return &Catalog{backend: backend, name: name}
}

type projectMappingRow struct {
	archiveID string
	mapping   db.WorktreeProjectMapping
}

type archiveCandidateSessionRef struct {
	id, project string
}

func (s *Catalog) GetProjectInventory(ctx context.Context, filter db.ProjectDateFilter) (db.ProjectInventory, error) {
	agg, err := s.backend.InventoryAggregate(ctx, filter)
	if err != nil {
		return db.ProjectInventory{}, err
	}

	rawProjects := make([]string, 0, len(agg))
	for project := range agg {
		rawProjects = append(rawProjects, project)
	}
	mappings, eval, err := s.projectInventoryGovernance(ctx)
	if err != nil {
		return db.ProjectInventory{}, err
	}
	projects, err := s.backend.BuildProjectIdentityMap(ctx, rawProjects)
	if err != nil {
		return db.ProjectInventory{}, err
	}
	rows, totalSessions := db.BuildProjectInventoryRows(agg, rawProjects, projects)
	db.AnnotateProjectInventoryRows(rows, mappings, eval, projects)

	return db.ProjectInventory{
		Projects:         rows,
		TotalProjects:    len(rows),
		TotalSessions:    totalSessions,
		GovernedSessions: eval.GovernedSessions,
	}, nil
}

func (s *Catalog) projectInventoryGovernance(
	ctx context.Context,
) ([]db.WorktreeProjectMapping, db.GovernedEvaluation, error) {
	visibleArchives, err := s.projectInventoryVisibleArchives(ctx)
	if err != nil {
		return nil, db.GovernedEvaluation{}, err
	}
	archiveMappings, rows, err := s.projectInventoryMappings(ctx, nil, visibleArchives)
	if err != nil {
		return nil, db.GovernedEvaluation{}, err
	}
	flatMappings := make([]db.WorktreeProjectMapping, len(rows))
	for i, row := range rows {
		flatMappings[i] = row.mapping
	}
	candidates, err := s.ProjectInventoryCandidateRows(ctx, nil)
	if err != nil {
		return nil, db.GovernedEvaluation{}, err
	}

	eval := db.EvaluateGovernedSessions(archiveMappings, candidates)
	return flatMappings, eval, nil
}

func (s *Catalog) projectInventoryVisibleArchives(
	ctx context.Context,
) (map[string]struct{}, error) {
	rows, err := s.backend.QueryContext(ctx, s.backend.VisibleArchivesSQL())
	if err != nil {
		return nil, fmt.Errorf("listing %s visible source archives: %w", s.name, err)
	}
	defer rows.Close()

	out := map[string]struct{}{}
	for rows.Next() {
		var archiveID string
		if err := rows.Scan(&archiveID); err != nil {
			return nil, fmt.Errorf("scanning %s visible source archive: %w", s.name, err)
		}
		out[archiveID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating %s visible source archives: %w", s.name, err)
	}
	return out, nil
}

// A nil machine selects all machines; a pointer to an empty string selects that exact value.
func (s *Catalog) projectInventoryMappings(
	ctx context.Context, machine *string, visibleArchives map[string]struct{},
) ([]db.ArchiveMappings, []projectMappingRow, error) {
	query, args := s.backend.MappingsSQL(machine)
	rows, err := s.backend.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("listing %s worktree mappings: %w", s.name, err)
	}
	defer rows.Close()

	byArchive := map[string][]db.WorktreeProjectMapping{}
	var archiveOrder []string
	var flat []projectMappingRow
	for rows.Next() {
		var archiveID string
		var m db.WorktreeProjectMapping
		if err := rows.Scan(
			&archiveID, &m.Machine, &m.PathPrefix, &m.Layout, &m.Project,
			&m.OriginalProject, &m.Enabled, &m.UpdatedAt,
		); err != nil {
			return nil, nil, fmt.Errorf("scanning %s worktree mapping: %w", s.name, err)
		}
		if visibleArchives != nil {
			if _, ok := visibleArchives[archiveID]; !ok {
				continue
			}
		}
		if _, seen := byArchive[archiveID]; !seen {
			archiveOrder = append(archiveOrder, archiveID)
		}
		byArchive[archiveID] = append(byArchive[archiveID], m)
		flat = append(flat, projectMappingRow{archiveID: archiveID, mapping: m})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterating %s worktree mappings: %w", s.name, err)
	}

	archiveMappings := make([]db.ArchiveMappings, len(archiveOrder))
	for i, archiveID := range archiveOrder {
		archiveMappings[i] = db.ArchiveMappings{
			SourceArchiveID: archiveID,
			Mappings:        byArchive[archiveID],
		}
	}
	return archiveMappings, flat, nil
}

// ProjectInventoryCandidateRows treats nil as all machines and non-nil as an exact match, including empty.
func (s *Catalog) ProjectInventoryCandidateRows(
	ctx context.Context, machine *string,
) ([]db.MappingEvaluationRow, error) {
	query, args := s.backend.CandidateRowsSQL(machine)
	rows, err := s.backend.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf(
			"querying %s project inventory candidate sessions: %w", s.name, err)
	}
	defer rows.Close()

	var out []db.MappingEvaluationRow
	for rows.Next() {
		var row db.MappingEvaluationRow
		if err := rows.Scan(
			&row.SessionID, &row.Machine, &row.Project, &row.Cwd, &row.FilePath,
			&row.ProjectAssigned, &row.SourceArchiveID,
		); err != nil {
			return nil, fmt.Errorf(
				"scanning %s project inventory candidate session: %w", s.name, err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf(
			"iterating %s project inventory candidate sessions: %w", s.name, err)
	}
	return out, nil
}

func (s *Catalog) ListProjectRules(ctx context.Context, machine string) (db.ProjectRules, error) {
	machine = strings.TrimSpace(machine)

	machines, err := s.projectRulesMachines(ctx)
	if err != nil {
		return db.ProjectRules{}, fmt.Errorf("listing %s project rule machines: %w", s.name, err)
	}

	archiveMappings, rows, err := s.projectInventoryMappings(ctx, &machine, nil)
	if err != nil {
		return db.ProjectRules{}, err
	}

	sessionsByRule, err := s.projectRulesGovernedCounts(ctx, machine, archiveMappings)
	if err != nil {
		return db.ProjectRules{}, err
	}

	rules := make([]db.ProjectRule, len(rows))
	for i, row := range rows {
		rules[i] = db.ProjectRule{
			WorktreeProjectMapping: row.mapping,
			SourceArchiveID:        row.archiveID,
			GovernedSessions: sessionsByRule[db.GovernedRuleKey{
				SourceArchiveID: row.archiveID,
				Machine:         row.mapping.Machine,
				PathPrefix:      row.mapping.PathPrefix,
			}],
		}
	}

	return db.ProjectRules{Machine: machine, Machines: machines, Rules: rules}, nil
}

func (s *Catalog) projectRulesMachines(ctx context.Context) ([]string, error) {
	rows, err := s.backend.QueryContext(ctx, s.backend.MachinesSQL())
	if err != nil {
		return nil, fmt.Errorf("listing %s project rule machines: %w", s.name, err)
	}
	defer rows.Close()

	machines := []string{}
	for rows.Next() {
		var machine string
		if err := rows.Scan(&machine); err != nil {
			return nil, fmt.Errorf("scanning %s project rule machine: %w", s.name, err)
		}
		machines = append(machines, machine)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating %s project rule machines: %w", s.name, err)
	}
	return machines, nil
}

func (s *Catalog) projectRulesGovernedCounts(
	ctx context.Context, machine string, archiveMappings []db.ArchiveMappings,
) (map[db.GovernedRuleKey]int, error) {
	hasEnabled := false
	for _, am := range archiveMappings {
		for _, m := range am.Mappings {
			if m.Enabled {
				hasEnabled = true
				break
			}
		}
	}
	if !hasEnabled {
		return nil, nil
	}

	candidates, err := s.ProjectInventoryCandidateRows(ctx, &machine)
	if err != nil {
		return nil, err
	}

	eval := db.EvaluateGovernedSessions(archiveMappings, candidates)
	return eval.SessionsByRule, nil
}

func (s *Catalog) ListArchiveWorktreeCandidates(
	ctx context.Context,
	request db.ArchiveWorktreeCandidateRequest,
) ([]db.WorktreeReclassificationCandidate, error) {
	if strings.TrimSpace(request.ProjectKey) == "" {
		return nil, errors.New("project_key is required")
	}
	sessions, err := s.archiveWorktreeCandidateSessions(ctx, request.ProjectDateFilter)
	if err != nil {
		return nil, err
	}
	labels := make(map[string]struct{})
	for _, session := range sessions {
		labels[session.project] = struct{}{}
	}
	projects, err := s.backend.BuildProjectIdentityMap(ctx, db.SortedKeys(labels))
	if err != nil {
		return nil, err
	}
	selectedProjects := db.SelectWorktreeCandidateProjects(
		request, labels, projects,
	)
	if len(selectedProjects) == 0 {
		return []db.WorktreeReclassificationCandidate{}, nil
	}

	selectedIDs := make([]string, 0, len(sessions))
	for _, session := range sessions {
		if _, ok := selectedProjects[session.project]; !ok {
			continue
		}
		selectedIDs = append(selectedIDs, session.id)
	}
	return s.worktreeCandidatesFromSelection(ctx, selectedIDs, selectedProjects)
}

func (s *Catalog) archiveWorktreeCandidateSessions(
	ctx context.Context,
	filter db.ProjectDateFilter,
) ([]archiveCandidateSessionRef, error) {
	query, args := s.backend.ArchiveSessionsSQL(filter)
	rows, err := s.backend.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying %s archive worktree candidate sessions: %w", s.name, err)
	}
	defer rows.Close()
	var sessions []archiveCandidateSessionRef
	for rows.Next() {
		var session archiveCandidateSessionRef
		if err := rows.Scan(&session.id, &session.project); err != nil {
			return nil, fmt.Errorf("scanning %s archive worktree candidate session: %w", s.name, err)
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating %s archive worktree candidate sessions: %w", s.name, err)
	}
	return sessions, nil
}

func (s *Catalog) worktreeCandidatesFromSelection(
	ctx context.Context,
	selectedIDs []string,
	selectedProjects map[string]struct{},
) ([]db.WorktreeReclassificationCandidate, error) {
	sessions, err := s.backend.WorktreeCandidateSessions(ctx, selectedIDs)
	if err != nil {
		return nil, err
	}
	observations, err := s.backend.ListProjectIdentityObservations(
		ctx, db.SortedKeys(selectedProjects))
	if err != nil {
		return nil, err
	}
	return db.BuildWorktreeCandidates(sessions, observations), nil
}

func (s *Catalog) BuildProjectIdentityMap(
	ctx context.Context,
	labels []string,
) (map[string]export.ProjectMapEntry, error) {
	if labels != nil && len(labels) == 0 {
		return map[string]export.ProjectMapEntry{}, nil
	}
	observations, err := s.backend.ListProjectIdentityObservations(ctx, labels)
	if err != nil {
		return nil, err
	}
	scope, err := s.sourceArchiveIdentityScope(ctx, observations)
	if err != nil {
		return nil, err
	}
	return export.BuildProjectsMapWithScope(labels, observations, scope), nil
}

func (s *Catalog) sourceArchiveIdentityScope(
	ctx context.Context,
	observations []export.ProjectIdentityObservation,
) (export.IdentityScope, error) {
	rows, err := s.backend.QueryContext(ctx, s.backend.SourceArchivesSQL())
	if err != nil {
		return export.IdentityScope{}, fmt.Errorf(
			"listing %s source archives: %w", s.name, err,
		)
	}
	defer rows.Close()

	var scopes []export.IdentityScope
	for rows.Next() {
		var scope export.IdentityScope
		if err := rows.Scan(&scope.ArchiveID, &scope.ArchiveSalt); err != nil {
			return export.IdentityScope{}, fmt.Errorf(
				"scanning %s source archive: %w", s.name, err,
			)
		}
		scopes = append(scopes, scope)
	}
	if err := rows.Err(); err != nil {
		return export.IdentityScope{}, fmt.Errorf(
			"iterating %s source archives: %w", s.name, err,
		)
	}
	if len(scopes) == 1 {
		return scopes[0], nil
	}
	if len(scopes) == 0 {
		return db.ObservationIdentityScope(observations), nil
	}
	return export.AggregateIdentityScope(scopes), nil
}
