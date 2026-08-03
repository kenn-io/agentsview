package db

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db/bunmodel"
	"go.kenn.io/agentsview/internal/export"
)

const bunDataListBatchSize = 500

// ListProjectIdentityObservations returns canonical source-scoped identity
// observations ordered by archive and raw project identity.
func (s *BunStore) ListProjectIdentityObservations(
	ctx context.Context,
	labels []string,
) ([]export.ProjectIdentityObservation, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var observations []export.ProjectIdentityObservation
	err := s.view(ctx, func(store bun.IDB) error {
		var err error
		observations, err = listBunProjectIdentityObservations(ctx, store, labels)
		return err
	})
	if err != nil {
		return nil, err
	}
	return observations, nil
}

func listBunProjectIdentityObservations(
	ctx context.Context,
	store bun.IDB,
	labels []string,
) ([]export.ProjectIdentityObservation, error) {
	if labels != nil && len(labels) == 0 {
		return []export.ProjectIdentityObservation{}, nil
	}
	if labels == nil {
		return listBunProjectIdentityObservationChunk(ctx, store, nil)
	}

	sortedLabels := slices.Clone(labels)
	slices.Sort(sortedLabels)
	sortedLabels = slices.Compact(sortedLabels)
	observations := make([]export.ProjectIdentityObservation, 0)
	for start := 0; start < len(sortedLabels); start += bunDataListBatchSize {
		end := min(start+bunDataListBatchSize, len(sortedLabels))
		rows, err := listBunProjectIdentityObservationChunk(
			ctx, store, sortedLabels[start:end],
		)
		if err != nil {
			return nil, err
		}
		observations = append(observations, rows...)
	}
	sortBunProjectIdentityObservations(observations)
	return observations, nil
}

func listBunProjectIdentityObservationChunk(
	ctx context.Context,
	store bun.IDB,
	labels []string,
) ([]export.ProjectIdentityObservation, error) {
	var rows []bunmodel.SourceProjectIdentityObservation
	query := store.NewSelect().Model(&rows)
	if len(labels) > 0 {
		query = query.Where("project IN (?)", bun.List(labels))
	}
	if err := query.OrderExpr("source_archive_id ASC").
		OrderExpr("project ASC").OrderExpr("machine ASC").
		OrderExpr("root_path ASC").OrderExpr("git_remote ASC").
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("listing project identity observations: %w", err)
	}
	observations := make([]export.ProjectIdentityObservation, 0, len(rows))
	for _, row := range rows {
		observations = append(observations, projectIdentityObservationFromBunRow(row))
	}
	return observations, nil
}

func projectIdentityObservationFromBunRow(
	row bunmodel.SourceProjectIdentityObservation,
) export.ProjectIdentityObservation {
	return export.ProjectIdentityObservation{
		SourceArchiveID: row.SourceArchiveID, SourceArchiveSalt: row.SourceArchiveSalt,
		Project: row.Project, Machine: row.Machine, RootPath: row.RootPath,
		GitRemote: row.GitRemote, GitRemoteName: row.GitRemoteName,
		RepositoryPath: row.RepositoryPath, WorktreeName: row.WorktreeName,
		WorktreeRootPath:     row.WorktreeRootPath,
		WorktreeRelationship: export.WorktreeRelationship(row.WorktreeRelationship),
		CheckoutState:        export.CheckoutState(row.CheckoutState), GitBranch: row.GitBranch,
		RemoteResolution:     export.ProjectResolution(row.RemoteResolution),
		RemoteCandidateCount: row.RemoteCandidateCount,
		ObservedAt:           row.ObservedAt.UTC(), NormalizedRemote: row.NormalizedRemote,
		KeySource: row.KeySource, Key: row.Key,
	}
}

func sortBunProjectIdentityObservations(
	observations []export.ProjectIdentityObservation,
) {
	sort.SliceStable(observations, func(i, j int) bool {
		left, right := observations[i], observations[j]
		if left.SourceArchiveID != right.SourceArchiveID {
			return left.SourceArchiveID < right.SourceArchiveID
		}
		if left.Project != right.Project {
			return left.Project < right.Project
		}
		if left.Machine != right.Machine {
			return left.Machine < right.Machine
		}
		if left.RootPath != right.RootPath {
			return left.RootPath < right.RootPath
		}
		return left.GitRemote < right.GitRemote
	})
}

// BuildProjectIdentityMap resolves raw labels from canonical source-scoped
// observations and the complete set of contributing archive identities.
func (s *BunStore) BuildProjectIdentityMap(
	ctx context.Context,
	labels []string,
) (map[string]export.ProjectMapEntry, error) {
	if labels != nil && len(labels) == 0 {
		return map[string]export.ProjectMapEntry{}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var projects map[string]export.ProjectMapEntry
	err := s.view(ctx, func(store bun.IDB) error {
		observations, err := listBunProjectIdentityObservations(ctx, store, labels)
		if err != nil {
			return err
		}
		scope, err := bunSourceArchiveIdentityScope(ctx, store, observations)
		if err != nil {
			return err
		}
		projects = export.BuildProjectsMapWithScope(labels, observations, scope)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return projects, nil
}

func bunSourceArchiveIdentityScope(
	ctx context.Context,
	store bun.IDB,
	observations []export.ProjectIdentityObservation,
) (export.IdentityScope, error) {
	var rows []bunmodel.SourceArchive
	if err := store.NewSelect().Model(&rows).
		OrderExpr("source_archive_id ASC").Scan(ctx); err != nil {
		return export.IdentityScope{}, fmt.Errorf("listing source archives: %w", err)
	}
	scopes := make([]export.IdentityScope, 0, len(rows))
	for _, row := range rows {
		scopes = append(scopes, export.IdentityScope{
			ArchiveID: row.SourceArchiveID, ArchiveSalt: row.SourceArchiveSalt,
		})
	}
	switch len(scopes) {
	case 0:
		return bunObservationIdentityScope(observations), nil
	case 1:
		return scopes[0], nil
	default:
		return export.AggregateIdentityScope(scopes), nil
	}
}

func bunObservationIdentityScope(
	observations []export.ProjectIdentityObservation,
) export.IdentityScope {
	unique := make(map[string]export.IdentityScope)
	for _, observation := range observations {
		scope := export.IdentityScope{
			ArchiveID:   strings.TrimSpace(observation.SourceArchiveID),
			ArchiveSalt: strings.TrimSpace(observation.SourceArchiveSalt),
		}
		if scope.ArchiveID == "" || scope.ArchiveSalt == "" {
			continue
		}
		unique[scope.ArchiveID+"\x00"+scope.ArchiveSalt] = scope
	}
	if len(unique) == 0 {
		return export.LegacySharedStoreIdentityScope()
	}
	scopes := make([]export.IdentityScope, 0, len(unique))
	for _, scope := range unique {
		scopes = append(scopes, scope)
	}
	if len(scopes) == 1 {
		return scopes[0]
	}
	return export.AggregateIdentityScope(scopes)
}

type bunDataSessionRow struct {
	ID                       string              `bun:"id"`
	Project                  string              `bun:"project"`
	Machine                  string              `bun:"machine"`
	Agent                    string              `bun:"agent"`
	Cwd                      string              `bun:"cwd"`
	StartedAt                *bunmodel.Timestamp `bun:"started_at"`
	EndedAt                  *bunmodel.Timestamp `bun:"ended_at"`
	FilePath                 *string             `bun:"file_path"`
	SourceArchiveID          string              `bun:"source_archive_id"`
	SourceDatabaseGeneration string              `bun:"source_database_generation"`
}

type bunProjectMappingRow struct {
	archiveID string
	mapping   WorktreeProjectMapping
}

// GetProjectInventory aggregates the canonical visible session and mapping
// rows through the same Bun handle on every backend.
func (s *BunStore) GetProjectInventory(ctx context.Context) (ProjectInventory, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var inventory ProjectInventory
	err := s.view(ctx, func(store bun.IDB) error {
		sessions, err := listBunDataSessions(ctx, store)
		if err != nil {
			return err
		}
		mappingRows, err := listBunProjectMappings(ctx, store)
		if err != nil {
			return err
		}

		agg := aggregateBunProjectInventory(sessions)
		rawProjects := make([]string, 0, len(agg))
		for project := range agg {
			rawProjects = append(rawProjects, project)
		}
		projects, _, err := buildBunProjectIdentityMap(ctx, store, rawProjects)
		if err != nil {
			return err
		}

		visibleArchives := make(map[string]struct{})
		for _, session := range sessions {
			if session.SourceArchiveID != "" {
				visibleArchives[session.SourceArchiveID] = struct{}{}
			}
		}
		visibleMappings := filterBunProjectMappings(mappingRows, func(row bunProjectMappingRow) bool {
			_, ok := visibleArchives[row.archiveID]
			return ok
		})
		archiveMappings := bunArchiveMappings(visibleMappings)
		eval := EvaluateGovernedSessions(
			archiveMappings,
			bunMappingEvaluationRows(sessions, visibleMappings, ""),
		)
		mappings := make([]WorktreeProjectMapping, len(visibleMappings))
		for i, row := range visibleMappings {
			mappings[i] = row.mapping
		}

		rows, totalSessions := buildProjectInventoryRows(agg, rawProjects, projects)
		annotateProjectInventoryRows(rows, mappings, eval, projects)
		inventory = ProjectInventory{
			Projects: rows, TotalProjects: len(rows), TotalSessions: totalSessions,
			GovernedSessions: eval.GovernedSessions,
		}
		return nil
	})
	if err != nil {
		return ProjectInventory{}, err
	}
	return inventory, nil
}

// ListProjectRules returns every canonical rule for one machine and evaluates
// it only against sessions from the same source archive.
func (s *BunStore) ListProjectRules(
	ctx context.Context, machine string,
) (ProjectRules, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	machine = strings.TrimSpace(machine)
	var result ProjectRules
	err := s.view(ctx, func(store bun.IDB) error {
		sessions, err := listBunDataSessions(ctx, store)
		if err != nil {
			return err
		}
		mappingRows, err := listBunProjectMappings(ctx, store)
		if err != nil {
			return err
		}

		machineSet := make(map[string]struct{})
		for _, session := range sessions {
			if session.Machine != "" {
				machineSet[session.Machine] = struct{}{}
			}
		}
		for _, row := range mappingRows {
			if row.mapping.Machine != "" {
				machineSet[row.mapping.Machine] = struct{}{}
			}
		}
		selected := filterBunProjectMappings(mappingRows, func(row bunProjectMappingRow) bool {
			return row.mapping.Machine == machine
		})
		sort.SliceStable(selected, func(i, j int) bool {
			if selected[i].mapping.PathPrefix != selected[j].mapping.PathPrefix {
				return selected[i].mapping.PathPrefix < selected[j].mapping.PathPrefix
			}
			return selected[i].archiveID < selected[j].archiveID
		})
		eval := EvaluateGovernedSessions(
			bunArchiveMappings(selected),
			bunMappingEvaluationRows(sessions, selected, machine),
		)
		rules := make([]ProjectRule, len(selected))
		for i, row := range selected {
			rules[i] = ProjectRule{
				WorktreeProjectMapping: row.mapping,
				SourceArchiveID:        row.archiveID,
				GovernedSessions: eval.SessionsByRule[GovernedRuleKey{
					SourceArchiveID: row.archiveID,
					Machine:         row.mapping.Machine,
					PathPrefix:      row.mapping.PathPrefix,
				}],
			}
		}
		result = ProjectRules{
			Machine: machine, Machines: sortedSetKeys(machineSet), Rules: rules,
		}
		return nil
	})
	if err != nil {
		return ProjectRules{}, err
	}
	return result, nil
}

// ListArchiveWorktreeCandidates builds canonical project candidates from
// visible sessions, exact-generation snapshots, and identity observations.
func (s *BunStore) ListArchiveWorktreeCandidates(
	ctx context.Context,
	request ArchiveWorktreeCandidateRequest,
) ([]WorktreeReclassificationCandidate, error) {
	if strings.TrimSpace(request.ProjectKey) == "" {
		return nil, fmt.Errorf("project_key is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var candidates []WorktreeReclassificationCandidate
	err := s.view(ctx, func(store bun.IDB) error {
		sessions, err := listBunDataSessions(ctx, store)
		if err != nil {
			return err
		}
		labels := make(map[string]struct{})
		for _, session := range sessions {
			labels[session.Project] = struct{}{}
		}
		projects, observations, err := buildBunProjectIdentityMap(
			ctx, store, sortedSetKeys(labels),
		)
		if err != nil {
			return err
		}
		selectedProjects := SelectWorktreeCandidateProjects(request, labels, projects)
		if len(selectedProjects) == 0 {
			candidates = []WorktreeReclassificationCandidate{}
			return nil
		}

		selected := make([]bunDataSessionRow, 0)
		selectedIDs := make([]string, 0)
		for _, session := range sessions {
			if _, ok := selectedProjects[session.Project]; !ok {
				continue
			}
			selected = append(selected, session)
			selectedIDs = append(selectedIDs, session.ID)
		}
		snapshots, err := listBunProjectIdentitySnapshots(ctx, store, selectedIDs)
		if err != nil {
			return err
		}
		details := make([]WorktreeCandidateSession, 0, len(selected))
		for _, session := range selected {
			detail := WorktreeCandidateSession{
				ID: session.ID, Project: session.Project,
				Machine: session.Machine, Cwd: session.Cwd,
			}
			if snapshot, ok := snapshots[bunSnapshotKey{
				archiveID:  session.SourceArchiveID,
				generation: session.SourceDatabaseGeneration,
				sessionID:  session.ID,
			}]; ok {
				detail.Snapshot = projectIdentitySnapshotFromBunRow(snapshot)
				detail.HasSnapshot = true
			}
			details = append(details, detail)
		}
		candidates = BuildWorktreeCandidates(details, observations)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return candidates, nil
}

func listBunDataSessions(
	ctx context.Context, store bun.IDB,
) ([]bunDataSessionRow, error) {
	var rows []bunDataSessionRow
	if err := store.NewSelect().Table("sessions").Column(
		"id", "project", "machine", "agent", "cwd", "started_at", "ended_at",
		"file_path", "source_archive_id", "source_database_generation",
	).Where("deleted_at IS NULL").OrderExpr("id ASC").Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("listing canonical data sessions: %w", err)
	}
	return rows, nil
}

func aggregateBunProjectInventory(
	sessions []bunDataSessionRow,
) map[string]projectInventoryAgg {
	type accumulator struct {
		projectInventoryAgg
		machines map[string]struct{}
		agents   map[string]struct{}
		cwds     map[string]struct{}
	}
	accumulators := make(map[string]*accumulator)
	for _, session := range sessions {
		acc := accumulators[session.Project]
		if acc == nil {
			acc = &accumulator{
				machines: make(map[string]struct{}), agents: make(map[string]struct{}),
				cwds: make(map[string]struct{}),
			}
			accumulators[session.Project] = acc
		}
		acc.sessions++
		acc.machines[session.Machine] = struct{}{}
		acc.agents[session.Agent] = struct{}{}
		if cwd := strings.ReplaceAll(session.Cwd, `\`, "/"); cwd != "" {
			acc.cwds[cwd] = struct{}{}
		}
		if session.StartedAt != nil && !session.StartedAt.IsZero() {
			started := session.StartedAt.UTC()
			acc.first = minTimePtr(acc.first, &started)
		}
		activity := session.EndedAt
		if activity == nil || activity.IsZero() {
			activity = session.StartedAt
		}
		if activity != nil && !activity.IsZero() {
			ended := activity.UTC()
			acc.last = maxTimePtr(acc.last, &ended)
		}
	}
	out := make(map[string]projectInventoryAgg, len(accumulators))
	for project, acc := range accumulators {
		acc.projectInventoryAgg.machines = len(acc.machines)
		acc.projectInventoryAgg.agents = len(acc.agents)
		acc.distinctCwds = len(acc.cwds)
		out[project] = acc.projectInventoryAgg
	}
	return out
}

func listBunProjectMappings(
	ctx context.Context, store bun.IDB,
) ([]bunProjectMappingRow, error) {
	var rows []bunmodel.SourceWorktreeProjectMapping
	if err := store.NewSelect().Model(&rows).
		OrderExpr("source_archive_id ASC").OrderExpr("machine ASC").
		OrderExpr("path_prefix ASC").Scan(ctx); err != nil {
		return nil, fmt.Errorf("listing canonical worktree mappings: %w", err)
	}
	result := make([]bunProjectMappingRow, len(rows))
	for i, row := range rows {
		result[i] = bunProjectMappingRow{
			archiveID: row.SourceArchiveID,
			mapping: WorktreeProjectMapping{
				ID: row.ID, Machine: row.Machine, PathPrefix: row.PathPrefix,
				Layout: row.Layout, Project: row.Project,
				OriginalProject: row.OriginalProject, Enabled: row.Enabled,
				CreatedAt: formatBunDataTime(row.CreatedAt.Time),
				UpdatedAt: formatBunDataTime(row.UpdatedAt.Time),
			},
		}
	}
	return result, nil
}

func formatBunDataTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func filterBunProjectMappings(
	rows []bunProjectMappingRow,
	keep func(bunProjectMappingRow) bool,
) []bunProjectMappingRow {
	result := make([]bunProjectMappingRow, 0, len(rows))
	for _, row := range rows {
		if keep(row) {
			result = append(result, row)
		}
	}
	return result
}

func bunArchiveMappings(rows []bunProjectMappingRow) []ArchiveMappings {
	byArchive := make(map[string][]WorktreeProjectMapping)
	order := make([]string, 0)
	for _, row := range rows {
		if _, ok := byArchive[row.archiveID]; !ok {
			order = append(order, row.archiveID)
		}
		byArchive[row.archiveID] = append(byArchive[row.archiveID], row.mapping)
	}
	result := make([]ArchiveMappings, len(order))
	for i, archiveID := range order {
		result[i] = ArchiveMappings{
			SourceArchiveID: archiveID, Mappings: byArchive[archiveID],
		}
	}
	return result
}

func bunMappingEvaluationRows(
	sessions []bunDataSessionRow,
	mappings []bunProjectMappingRow,
	machine string,
) []MappingEvaluationRow {
	type scope struct{ archiveID, machine string }
	enabled := make(map[scope]struct{})
	for _, row := range mappings {
		if row.mapping.Enabled {
			enabled[scope{archiveID: row.archiveID, machine: row.mapping.Machine}] = struct{}{}
		}
	}
	result := make([]MappingEvaluationRow, 0)
	for _, session := range sessions {
		if machine != "" && session.Machine != machine {
			continue
		}
		if session.SourceArchiveID == "" {
			continue
		}
		if _, ok := enabled[scope{
			archiveID: session.SourceArchiveID, machine: session.Machine,
		}]; !ok {
			continue
		}
		filePath := ""
		if session.FilePath != nil {
			filePath = *session.FilePath
		}
		result = append(result, MappingEvaluationRow{
			SessionID: session.ID, Machine: session.Machine,
			Project: session.Project, Cwd: session.Cwd, FilePath: filePath,
			SourceArchiveID: session.SourceArchiveID,
		})
	}
	return result
}

func buildBunProjectIdentityMap(
	ctx context.Context,
	store bun.IDB,
	labels []string,
) (map[string]export.ProjectMapEntry, []export.ProjectIdentityObservation, error) {
	observations, err := listBunProjectIdentityObservations(ctx, store, labels)
	if err != nil {
		return nil, nil, err
	}
	scope, err := bunSourceArchiveIdentityScope(ctx, store, observations)
	if err != nil {
		return nil, nil, err
	}
	return export.BuildProjectsMapWithScope(labels, observations, scope), observations, nil
}

type bunSnapshotKey struct {
	archiveID  string
	generation string
	sessionID  string
}

func listBunProjectIdentitySnapshots(
	ctx context.Context,
	store bun.IDB,
	sessionIDs []string,
) (map[bunSnapshotKey]bunmodel.SourceSessionProjectIdentitySnapshot, error) {
	result := make(map[bunSnapshotKey]bunmodel.SourceSessionProjectIdentitySnapshot)
	for start := 0; start < len(sessionIDs); start += bunDataListBatchSize {
		end := min(start+bunDataListBatchSize, len(sessionIDs))
		var rows []bunmodel.SourceSessionProjectIdentitySnapshot
		if err := store.NewSelect().Model(&rows).
			Where("source_session_id IN (?)", bun.List(sessionIDs[start:end])).
			OrderExpr("source_archive_id ASC").
			OrderExpr("source_database_generation ASC").
			OrderExpr("source_session_id ASC").Scan(ctx); err != nil {
			return nil, fmt.Errorf("listing canonical project identity snapshots: %w", err)
		}
		for _, row := range rows {
			result[bunSnapshotKey{
				archiveID:  row.SourceArchiveID,
				generation: row.SourceDatabaseGeneration,
				sessionID:  row.SourceSessionID,
			}] = row
		}
	}
	return result, nil
}

func projectIdentitySnapshotFromBunRow(
	row bunmodel.SourceSessionProjectIdentitySnapshot,
) export.ProjectIdentityObservation {
	return export.ProjectIdentityObservation{
		SourceArchiveID: row.SourceArchiveID, Project: row.Project,
		Machine: row.Machine, RootPath: row.RootPath, GitRemote: row.GitRemote,
		GitRemoteName: row.GitRemoteName, RepositoryPath: row.RepositoryPath,
		WorktreeName: row.WorktreeName, WorktreeRootPath: row.WorktreeRootPath,
		WorktreeRelationship: export.WorktreeRelationship(row.WorktreeRelationship),
		CheckoutState:        export.CheckoutState(row.CheckoutState), GitBranch: row.GitBranch,
		RemoteResolution:     export.ProjectResolution(row.RemoteResolution),
		RemoteCandidateCount: row.RemoteCandidateCount, ObservedAt: row.ObservedAt.UTC(),
		NormalizedRemote: row.NormalizedRemote, KeySource: row.KeySource, Key: row.Key,
	}
}
