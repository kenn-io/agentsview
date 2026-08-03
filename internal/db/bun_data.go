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

type bunProjectInventoryAggregateRow struct {
	Project       string              `bun:"project"`
	Sessions      int                 `bun:"sessions"`
	Machines      int                 `bun:"machines"`
	Agents        int                 `bun:"agents"`
	DistinctCwds  int                 `bun:"distinct_cwds"`
	FirstActivity *bunmodel.Timestamp `bun:"first_activity"`
	LastActivity  *bunmodel.Timestamp `bun:"last_activity"`
}

type bunGovernanceSessionRow struct {
	ID              string `bun:"id"`
	Machine         string `bun:"machine"`
	Project         string `bun:"project"`
	Cwd             string `bun:"cwd"`
	FilePath        string `bun:"file_path"`
	SourceArchiveID string `bun:"source_archive_id"`
}

type bunCandidateSessionRow struct {
	ID                       string `bun:"id"`
	Project                  string `bun:"project"`
	Machine                  string `bun:"machine"`
	Cwd                      string `bun:"cwd"`
	SourceArchiveID          string `bun:"source_archive_id"`
	SourceDatabaseGeneration string `bun:"source_database_generation"`
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
	err := s.consistentView(ctx, func(store bun.IDB) error {
		agg, err := listBunProjectInventoryAggregates(
			ctx, store, s.backend.SessionQueryDialect(),
		)
		if err != nil {
			return err
		}
		rawProjects := make([]string, 0, len(agg))
		for project := range agg {
			rawProjects = append(rawProjects, project)
		}
		projects, _, err := buildBunProjectIdentityMap(ctx, store, rawProjects)
		if err != nil {
			return err
		}

		visibleArchives, err := listBunVisibleArchiveIDs(ctx, store)
		if err != nil {
			return err
		}
		mappingRows, err := listBunProjectMappings(
			ctx, store, nil, visibleArchives,
		)
		if err != nil {
			return err
		}
		governanceRows, err := listBunGovernanceSessions(ctx, store, nil)
		if err != nil {
			return err
		}
		archiveMappings := bunArchiveMappings(mappingRows)
		eval := EvaluateGovernedSessions(
			archiveMappings,
			bunMappingEvaluationRows(governanceRows),
		)
		mappings := make([]WorktreeProjectMapping, len(mappingRows))
		for i, row := range mappingRows {
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
	err := s.consistentView(ctx, func(store bun.IDB) error {
		machines, err := listBunProjectRuleMachines(ctx, store)
		if err != nil {
			return err
		}
		mappingRows, err := listBunProjectMappings(ctx, store, &machine, nil)
		if err != nil {
			return err
		}
		governanceRows, err := listBunGovernanceSessions(ctx, store, &machine)
		if err != nil {
			return err
		}
		eval := EvaluateGovernedSessions(
			bunArchiveMappings(mappingRows),
			bunMappingEvaluationRows(governanceRows),
		)
		rules := make([]ProjectRule, len(mappingRows))
		for i, row := range mappingRows {
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
			Machine: machine, Machines: machines, Rules: rules,
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
	err := s.consistentView(ctx, func(store bun.IDB) error {
		labels, err := listBunCandidateProjectLabels(ctx, store)
		if err != nil {
			return err
		}
		projects, _, err := buildBunProjectIdentityMap(
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

		selected, err := listBunCandidateSessions(
			ctx, store, sortedSetKeys(selectedProjects),
		)
		if err != nil {
			return err
		}
		selectedIDs := make([]string, len(selected))
		for i, session := range selected {
			selectedIDs[i] = session.ID
		}
		snapshots, err := listBunProjectIdentitySnapshots(ctx, store, selectedIDs)
		if err != nil {
			return err
		}
		observations, err := listBunProjectIdentityObservations(
			ctx, store, sortedSetKeys(selectedProjects),
		)
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

func listBunProjectInventoryAggregates(
	ctx context.Context, store bun.IDB, dialect QueryDialect,
) (map[string]projectInventoryAgg, error) {
	startedOrder := dialect.timestampExpr("started_at")
	activityOrder := "COALESCE(" + dialect.timestampExpr("ended_at") + ", " +
		dialect.timestampExpr("started_at") + ")"
	startedRaw := bunNullableTimestamp("started_at")
	activityRaw := "COALESCE(" + bunNullableTimestamp("ended_at") + ", " +
		bunNullableTimestamp("started_at") + ")"
	query := fmt.Sprintf(`
		WITH visible AS (
			SELECT id, project, machine, agent,
				CASE WHEN cwd IS NOT NULL AND cwd != ''
					THEN replace(cwd, '\', '/') END AS normalized_cwd,
				%s AS started_order, %s AS started_raw,
				%s AS activity_order, %s AS activity_raw
			FROM sessions
			WHERE deleted_at IS NULL
		), ranked AS (
			SELECT *,
				ROW_NUMBER() OVER (
					PARTITION BY project
					ORDER BY (started_order IS NULL) ASC,
						started_order ASC, id ASC
				) AS first_rank,
				ROW_NUMBER() OVER (
					PARTITION BY project
					ORDER BY (activity_order IS NULL) ASC,
						activity_order DESC, id DESC
				) AS last_rank
			FROM visible
		)
		SELECT project, COUNT(*) AS sessions,
			COUNT(DISTINCT machine) AS machines,
			COUNT(DISTINCT agent) AS agents,
			COUNT(DISTINCT normalized_cwd) AS distinct_cwds,
			MAX(CASE WHEN first_rank = 1 THEN started_raw END) AS first_activity,
			MAX(CASE WHEN last_rank = 1 THEN activity_raw END) AS last_activity
		FROM ranked
		GROUP BY project
		ORDER BY project`, startedOrder, startedRaw, activityOrder, activityRaw)
	var rows []bunProjectInventoryAggregateRow
	if err := store.NewRaw(query).Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("aggregating canonical project inventory: %w", err)
	}
	result := make(map[string]projectInventoryAgg, len(rows))
	for _, row := range rows {
		agg := projectInventoryAgg{
			sessions: row.Sessions, machines: row.Machines,
			agents: row.Agents, distinctCwds: row.DistinctCwds,
		}
		if row.FirstActivity != nil && !row.FirstActivity.IsZero() {
			value := row.FirstActivity.UTC()
			agg.first = &value
		}
		if row.LastActivity != nil && !row.LastActivity.IsZero() {
			value := row.LastActivity.UTC()
			agg.last = &value
		}
		result[row.Project] = agg
	}
	return result, nil
}

func listBunVisibleArchiveIDs(
	ctx context.Context, store bun.IDB,
) (map[string]struct{}, error) {
	var rows []struct {
		SourceArchiveID string `bun:"source_archive_id"`
	}
	if err := store.NewSelect().Table("sessions").Column("source_archive_id").
		Where("deleted_at IS NULL").Where("source_archive_id != ''").
		Group("source_archive_id").OrderExpr("source_archive_id ASC").
		Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("listing canonical visible source archives: %w", err)
	}
	result := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		result[row.SourceArchiveID] = struct{}{}
	}
	return result, nil
}

func listBunProjectMappings(
	ctx context.Context,
	store bun.IDB,
	machine *string,
	visibleArchives map[string]struct{},
) ([]bunProjectMappingRow, error) {
	if visibleArchives != nil && len(visibleArchives) == 0 {
		return []bunProjectMappingRow{}, nil
	}
	archiveIDs := sortedSetKeys(visibleArchives)
	var rows []bunmodel.SourceWorktreeProjectMapping
	load := func(chunk []string) error {
		var batch []bunmodel.SourceWorktreeProjectMapping
		query := store.NewSelect().Model(&batch)
		if machine != nil {
			query = query.Where("machine = ?", *machine)
		}
		if visibleArchives != nil {
			query = query.Where("source_archive_id IN (?)", bun.List(chunk))
		}
		if err := query.Scan(ctx); err != nil {
			return fmt.Errorf("listing canonical worktree mappings: %w", err)
		}
		rows = append(rows, batch...)
		return nil
	}
	if visibleArchives == nil {
		if err := load(nil); err != nil {
			return nil, err
		}
	} else {
		for start := 0; start < len(archiveIDs); start += bunDataListBatchSize {
			end := min(start+bunDataListBatchSize, len(archiveIDs))
			if err := load(archiveIDs[start:end]); err != nil {
				return nil, err
			}
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if machine != nil {
			if rows[i].PathPrefix != rows[j].PathPrefix {
				return rows[i].PathPrefix < rows[j].PathPrefix
			}
			return rows[i].SourceArchiveID < rows[j].SourceArchiveID
		}
		if rows[i].SourceArchiveID != rows[j].SourceArchiveID {
			return rows[i].SourceArchiveID < rows[j].SourceArchiveID
		}
		if rows[i].Machine != rows[j].Machine {
			return rows[i].Machine < rows[j].Machine
		}
		return rows[i].PathPrefix < rows[j].PathPrefix
	})
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

func listBunProjectRuleMachines(
	ctx context.Context, store bun.IDB,
) ([]string, error) {
	var rows []struct {
		Machine string `bun:"machine"`
	}
	if err := store.NewRaw(`
		SELECT machine FROM sessions
		WHERE deleted_at IS NULL AND machine != ''
		UNION
		SELECT machine FROM source_worktree_project_mappings
		WHERE machine != ''
		ORDER BY machine`).Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("listing canonical project rule machines: %w", err)
	}
	machines := make([]string, len(rows))
	for i, row := range rows {
		machines[i] = row.Machine
	}
	return machines, nil
}

func listBunGovernanceSessions(
	ctx context.Context, store bun.IDB, machine *string,
) ([]bunGovernanceSessionRow, error) {
	var rows []bunGovernanceSessionRow
	query := store.NewSelect().TableExpr("sessions AS session").
		ColumnExpr("session.id AS id").
		ColumnExpr("session.machine AS machine").
		ColumnExpr("session.project AS project").
		ColumnExpr("session.cwd AS cwd").
		ColumnExpr("COALESCE(session.file_path, '') AS file_path").
		ColumnExpr("session.source_archive_id AS source_archive_id").
		Where("session.deleted_at IS NULL").
		Where("session.source_archive_id != ''").
		Where(`EXISTS (
			SELECT 1 FROM source_worktree_project_mappings AS mapping
			WHERE mapping.source_archive_id = session.source_archive_id
			  AND mapping.machine = session.machine
			  AND mapping.enabled = TRUE
		)`)
	if machine != nil {
		query = query.Where("session.machine = ?", *machine)
	}
	if err := query.OrderExpr("session.id ASC").Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("listing canonical governance sessions: %w", err)
	}
	return rows, nil
}

func bunMappingEvaluationRows(
	rows []bunGovernanceSessionRow,
) []MappingEvaluationRow {
	result := make([]MappingEvaluationRow, len(rows))
	for i, row := range rows {
		result[i] = MappingEvaluationRow{
			SessionID: row.ID, Machine: row.Machine, Project: row.Project,
			Cwd: row.Cwd, FilePath: row.FilePath,
			SourceArchiveID: row.SourceArchiveID,
		}
	}
	return result
}

func listBunCandidateProjectLabels(
	ctx context.Context, store bun.IDB,
) (map[string]struct{}, error) {
	var rows []struct {
		Project string `bun:"project"`
	}
	if err := store.NewSelect().Table("sessions").Column("project").
		Where("deleted_at IS NULL").Group("project").OrderExpr("project ASC").
		Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("listing canonical candidate projects: %w", err)
	}
	labels := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		labels[row.Project] = struct{}{}
	}
	return labels, nil
}

func listBunCandidateSessions(
	ctx context.Context, store bun.IDB, projects []string,
) ([]bunCandidateSessionRow, error) {
	rows := make([]bunCandidateSessionRow, 0)
	for start := 0; start < len(projects); start += bunDataListBatchSize {
		end := min(start+bunDataListBatchSize, len(projects))
		var batch []bunCandidateSessionRow
		if err := store.NewSelect().Table("sessions").Column(
			"id", "project", "machine", "cwd", "source_archive_id",
			"source_database_generation",
		).Where("deleted_at IS NULL").
			Where("project IN (?)", bun.List(projects[start:end])).
			OrderExpr("id ASC").Scan(ctx, &batch); err != nil {
			return nil, fmt.Errorf("listing canonical candidate sessions: %w", err)
		}
		rows = append(rows, batch...)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows, nil
}

func formatBunDataTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
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
		if err := store.NewSelect().
			TableExpr("source_session_project_identity_snapshots AS snapshot").
			ColumnExpr("snapshot.*").
			Join("JOIN sessions AS session").
			JoinOn("session.source_archive_id = snapshot.source_archive_id").
			JoinOn("session.source_database_generation = snapshot.source_database_generation").
			JoinOn("session.id = snapshot.source_session_id").
			Where("session.deleted_at IS NULL").
			Where("session.id IN (?)", bun.List(sessionIDs[start:end])).
			OrderExpr("snapshot.source_archive_id ASC").
			OrderExpr("snapshot.source_database_generation ASC").
			OrderExpr("snapshot.source_session_id ASC").Scan(ctx, &rows); err != nil {
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
