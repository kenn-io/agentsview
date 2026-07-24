package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const worktreeReclassificationSampleLimit = 10

var ErrWorktreeMappingSetChanged = errors.New("worktree mapping set changed")

type WorktreeReclassificationDraft struct {
	Machine         string `json:"machine"`
	PathPrefix      string `json:"path_prefix"`
	Layout          string `json:"layout"`
	Project         string `json:"project"`
	OriginalProject string `json:"original_project"`
	Enabled         bool   `json:"enabled"`
}

type WorktreeReclassificationProjectSample struct {
	Project string `json:"project"`
	Count   int    `json:"count"`
}

type WorktreeReclassificationSessionSample struct {
	ID             string `json:"id"`
	CurrentProject string `json:"current_project"`
	NextProject    string `json:"next_project"`
	Cwd            string `json:"cwd"`
}

type WorktreeReclassificationPreview struct {
	MappingToken      string                                  `json:"mapping_token"`
	NormalizedProject string                                  `json:"normalized_project"`
	ExistingMappingID *int64                                  `json:"existing_mapping_id,omitempty"`
	MatchedSessions   int                                     `json:"matched_sessions"`
	UpdatedSessions   int                                     `json:"updated_sessions"`
	DistinctProjects  int                                     `json:"distinct_projects"`
	ProjectSamples    []WorktreeReclassificationProjectSample `json:"project_samples"`
	SessionSamples    []WorktreeReclassificationSessionSample `json:"session_samples"`
}

type worktreeReclassificationEvaluation struct {
	matched  int
	updates  []worktreeMappingSessionUpdate
	projects map[string]int
	sessions []WorktreeReclassificationSessionSample
}

func (db *DB) PreviewWorktreeReclassification(
	ctx context.Context,
	draft WorktreeReclassificationDraft,
) (WorktreeReclassificationPreview, error) {
	normalized, err := normalizeWorktreeReclassificationDraft(draft)
	if err != nil {
		return WorktreeReclassificationPreview{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}

	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return WorktreeReclassificationPreview{}, fmt.Errorf(
			"beginning worktree reclassification preview: %w", err,
		)
	}
	defer func() { _ = tx.Rollback() }()

	preview, err := previewWorktreeReclassificationTx(ctx, tx, normalized)
	if err != nil {
		return WorktreeReclassificationPreview{}, err
	}
	if err := tx.Commit(); err != nil {
		return WorktreeReclassificationPreview{}, fmt.Errorf(
			"committing worktree reclassification preview: %w", err,
		)
	}
	return preview, nil
}

func (db *DB) ApplyWorktreeReclassification(
	ctx context.Context,
	draft WorktreeReclassificationDraft,
	acceptedToken string,
	existingMappingID *int64,
) (WorktreeProjectMapping, WorktreeReclassificationPreview, error) {
	if err := db.requireWritable(); err != nil {
		return WorktreeProjectMapping{}, WorktreeReclassificationPreview{}, err
	}
	normalized, err := normalizeWorktreeReclassificationDraft(draft)
	if err != nil {
		return WorktreeProjectMapping{}, WorktreeReclassificationPreview{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}

	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return WorktreeProjectMapping{}, WorktreeReclassificationPreview{}, fmt.Errorf(
			"beginning worktree reclassification apply: %w", err,
		)
	}
	defer func() { _ = tx.Rollback() }()

	stored, err := loadWorktreeMappingsForMachineTx(ctx, tx, normalized.Machine)
	if err != nil {
		return WorktreeProjectMapping{}, WorktreeReclassificationPreview{}, err
	}
	currentToken := worktreeMappingSetToken(stored)
	collision := exactWorktreeMapping(stored, normalized.PathPrefix)
	if acceptedToken == "" || acceptedToken != currentToken ||
		!sameOptionalMappingID(existingMappingID, mappingIDPointer(collision)) {
		return WorktreeProjectMapping{}, WorktreeReclassificationPreview{},
			ErrWorktreeMappingSetChanged
	}

	mapping, err := upsertWorktreeReclassificationMappingTx(
		ctx, tx, normalized, collision,
	)
	if err != nil {
		return WorktreeProjectMapping{}, WorktreeReclassificationPreview{}, err
	}
	active, err := loadActiveWorktreeMappingsTx(ctx, tx, normalized.Machine)
	if err != nil {
		return WorktreeProjectMapping{}, WorktreeReclassificationPreview{}, fmt.Errorf(
			"loading active worktree mappings: %w", err,
		)
	}
	evaluation, err := evaluateWorktreeMappingsTx(
		ctx, tx, normalized.Machine, active, &mapping, "",
	)
	if err != nil {
		return WorktreeProjectMapping{}, WorktreeReclassificationPreview{}, err
	}
	affected := map[string]struct{}{mapping.Project: {}}
	updated := 0
	for _, update := range evaluation.updates {
		changed, updateErr := updateSessionProjectTx(ctx, tx, update, true)
		if updateErr != nil {
			return WorktreeProjectMapping{}, WorktreeReclassificationPreview{}, updateErr
		}
		updated += changed
		if changed > 0 {
			affected[update.currentProject] = struct{}{}
			affected[update.nextProject] = struct{}{}
		}
	}
	if err := rebuildProjectIdentityAggregatesTx(
		ctx, tx, normalized.Machine, sortedSetKeys(affected),
	); err != nil {
		return WorktreeProjectMapping{}, WorktreeReclassificationPreview{}, err
	}
	if err := tx.Commit(); err != nil {
		return WorktreeProjectMapping{}, WorktreeReclassificationPreview{}, fmt.Errorf(
			"committing worktree reclassification apply: %w", err,
		)
	}

	preview := worktreeReclassificationPreviewFromEvaluation(
		worktreeMappingSetTokenWithReplacement(stored, mapping),
		mapping.Project, mappingIDPointer(&mapping), evaluation,
	)
	preview.UpdatedSessions = updated
	return mapping, preview, nil
}

func normalizeWorktreeReclassificationDraft(
	draft WorktreeReclassificationDraft,
) (WorktreeProjectMapping, error) {
	normalized, err := normalizeWorktreeMapping(
		draft.Machine, draft.PathPrefix, draft.Layout, draft.Project,
	)
	if err != nil {
		return WorktreeProjectMapping{}, err
	}
	normalized.OriginalProject = strings.TrimSpace(draft.OriginalProject)
	normalized.Enabled = draft.Enabled
	return normalized, nil
}

func previewWorktreeReclassificationTx(
	ctx context.Context,
	tx *sql.Tx,
	draft WorktreeProjectMapping,
) (WorktreeReclassificationPreview, error) {
	stored, err := loadWorktreeMappingsForMachineTx(ctx, tx, draft.Machine)
	if err != nil {
		return WorktreeReclassificationPreview{}, err
	}
	collision := exactWorktreeMapping(stored, draft.PathPrefix)
	effective := overlayWorktreeMapping(stored, draft, collision)
	evaluation, err := evaluateWorktreeMappingsTx(
		ctx, tx, draft.Machine, enabledWorktreeMappings(effective), &draft, "",
	)
	if err != nil {
		return WorktreeReclassificationPreview{}, err
	}
	return worktreeReclassificationPreviewFromEvaluation(
		worktreeMappingSetToken(stored), draft.Project,
		mappingIDPointer(collision), evaluation,
	), nil
}

func loadWorktreeMappingsForMachineTx(
	ctx context.Context,
	tx *sql.Tx,
	machine string,
) ([]WorktreeProjectMapping, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, machine, path_prefix, layout, project, original_project,
			enabled, created_at, updated_at
		FROM worktree_project_mappings
		WHERE machine = ?
		ORDER BY id`, machine)
	if err != nil {
		return nil, fmt.Errorf("querying worktree mapping set: %w", err)
	}
	return scanWorktreeMappings(rows)
}

func exactWorktreeMapping(
	mappings []WorktreeProjectMapping,
	pathPrefix string,
) *WorktreeProjectMapping {
	normalizedPrefix := normalizedMappingPath(pathPrefix)
	for i := range mappings {
		if normalizedMappingPath(mappings[i].PathPrefix) == normalizedPrefix {
			mapping := mappings[i]
			return &mapping
		}
	}
	return nil
}

func overlayWorktreeMapping(
	stored []WorktreeProjectMapping,
	draft WorktreeProjectMapping,
	collision *WorktreeProjectMapping,
) []WorktreeProjectMapping {
	out := make([]WorktreeProjectMapping, 0, len(stored)+1)
	for _, mapping := range stored {
		if collision != nil && mapping.ID == collision.ID {
			continue
		}
		out = append(out, mapping)
	}
	if collision != nil {
		draft.ID = collision.ID
		draft.CreatedAt = collision.CreatedAt
		draft.UpdatedAt = collision.UpdatedAt
	}
	return append(out, draft)
}

func enabledWorktreeMappings(
	mappings []WorktreeProjectMapping,
) []WorktreeProjectMapping {
	active := make([]WorktreeProjectMapping, 0, len(mappings))
	for _, mapping := range mappings {
		if mapping.Enabled {
			active = append(active, mapping)
		}
	}
	sortWorktreeProjectMappings(active)
	return active
}

func evaluateWorktreeMappingsTx(
	ctx context.Context,
	tx *sql.Tx,
	machine string,
	mappings []WorktreeProjectMapping,
	scope *WorktreeProjectMapping,
	sessionID string,
) (worktreeReclassificationEvaluation, error) {
	sessions, err := loadWorktreeMappingSessionsTx(ctx, tx, machine, sessionID)
	if err != nil {
		return worktreeReclassificationEvaluation{}, err
	}

	applyWorktreeMappingMatchCwdFromSiblings(sessions,
		func(row worktreeMappingSessionRow) string {
			return strings.TrimSpace(row.filePath)
		},
		func(row worktreeMappingSessionRow, cwd string) (string, bool) {
			return ResolveWorktreeProjectFromSortedMappings(
				mappings, cwd, row.project,
			)
		},
	)

	evaluation := worktreeReclassificationEvaluation{projects: map[string]int{}}
	for _, row := range sessions {
		if sessionID != "" && row.id != sessionID {
			continue
		}
		if scope != nil {
			matchCwd := row.matchCwd
			if matchCwd == "" {
				matchCwd = row.cwd
			}
			if !worktreePathMatches(scope.PathPrefix, matchCwd) {
				continue
			}
			if _, ok := resolveWorktreeProjectFromMapping(*scope, matchCwd, row.project); !ok {
				continue
			}
		}
		update, matched, shouldUpdate := applyMappingToSessionRow(mappings, row)
		if !matched {
			continue
		}
		evaluation.matched++
		if !shouldUpdate {
			continue
		}
		evaluation.updates = append(evaluation.updates, update)
		evaluation.projects[update.currentProject]++
		evaluation.sessions = append(evaluation.sessions,
			WorktreeReclassificationSessionSample{
				ID: update.id, CurrentProject: update.currentProject,
				NextProject: update.nextProject, Cwd: update.cwd,
			})
	}
	return evaluation, nil
}

func loadWorktreeMappingSessionsTx(
	ctx context.Context,
	tx *sql.Tx,
	machine string,
	sessionID string,
) ([]worktreeMappingSessionRow, error) {
	query := `
		SELECT id, project, cwd, file_path
		FROM sessions
		WHERE machine = ? AND deleted_at IS NULL
		ORDER BY id`
	args := []any{machine}
	if sessionID != "" {
		query = `
			SELECT id, project, cwd, file_path
			FROM sessions
			WHERE machine = ? AND deleted_at IS NULL
				AND (id = ? OR (
					file_path IS NOT NULL AND file_path != ''
					AND file_path = (
						SELECT file_path FROM sessions
						WHERE machine = ? AND id = ? AND deleted_at IS NULL
					)
				))
			ORDER BY id`
		args = []any{machine, sessionID, machine, sessionID}
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf(
			"querying sessions for worktree mapping evaluation: %w", err,
		)
	}
	var sessions []worktreeMappingSessionRow
	for rows.Next() {
		var row worktreeMappingSessionRow
		var filePath sql.NullString
		row.machine = machine
		if err := rows.Scan(&row.id, &row.project, &row.cwd, &filePath); err != nil {
			rows.Close()
			return nil, fmt.Errorf(
				"scanning session for worktree mapping evaluation: %w", err,
			)
		}
		if filePath.Valid {
			row.filePath = filePath.String
		}
		sessions = append(sessions, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf(
			"iterating sessions for worktree mapping evaluation: %w", err,
		)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf(
			"closing worktree mapping evaluation rows: %w", err,
		)
	}
	return sessions, nil
}

func worktreeReclassificationPreviewFromEvaluation(
	token string,
	normalizedProject string,
	existingMappingID *int64,
	evaluation worktreeReclassificationEvaluation,
) WorktreeReclassificationPreview {
	projectNames := make([]string, 0, len(evaluation.projects))
	for project := range evaluation.projects {
		projectNames = append(projectNames, project)
	}
	sort.Strings(projectNames)
	projectLimit := min(len(projectNames), worktreeReclassificationSampleLimit)
	projectSamples := make([]WorktreeReclassificationProjectSample, 0, projectLimit)
	for _, project := range projectNames[:projectLimit] {
		projectSamples = append(projectSamples, WorktreeReclassificationProjectSample{
			Project: project, Count: evaluation.projects[project],
		})
	}
	sessionLimit := min(len(evaluation.sessions), worktreeReclassificationSampleLimit)
	sessionSamples := append([]WorktreeReclassificationSessionSample(nil),
		evaluation.sessions[:sessionLimit]...)
	return WorktreeReclassificationPreview{
		MappingToken: token, NormalizedProject: normalizedProject,
		ExistingMappingID: existingMappingID,
		MatchedSessions:   evaluation.matched,
		UpdatedSessions:   len(evaluation.updates),
		DistinctProjects:  len(evaluation.projects),
		ProjectSamples:    projectSamples, SessionSamples: sessionSamples,
	}
}

func worktreeMappingSetToken(mappings []WorktreeProjectMapping) string {
	sorted := append([]WorktreeProjectMapping(nil), mappings...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	hash := sha256.New()
	for _, mapping := range sorted {
		fields := []string{
			strconv.FormatInt(mapping.ID, 10), mapping.Machine,
			normalizedMappingPath(mapping.PathPrefix), mapping.Layout,
			mapping.Project, mapping.OriginalProject,
			strconv.FormatBool(mapping.Enabled), mapping.UpdatedAt,
		}
		for _, field := range fields {
			_, _ = fmt.Fprintf(hash, "%d:%s", len(field), field)
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func worktreeMappingSetTokenWithReplacement(
	stored []WorktreeProjectMapping,
	mapping WorktreeProjectMapping,
) string {
	return worktreeMappingSetToken(overlayWorktreeMapping(
		stored, mapping, exactWorktreeMapping(stored, mapping.PathPrefix),
	))
}

func mappingIDPointer(mapping *WorktreeProjectMapping) *int64 {
	if mapping == nil {
		return nil
	}
	id := mapping.ID
	return &id
}

func sameOptionalMappingID(left, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func upsertWorktreeReclassificationMappingTx(
	ctx context.Context,
	tx *sql.Tx,
	draft WorktreeProjectMapping,
	existing *WorktreeProjectMapping,
) (WorktreeProjectMapping, error) {
	enabled := 0
	if draft.Enabled {
		enabled = 1
	}
	if existing == nil {
		result, err := tx.ExecContext(ctx, `
			INSERT INTO worktree_project_mappings
				(machine, path_prefix, layout, project, original_project, enabled)
			VALUES (?, ?, ?, ?, ?, ?)`,
			draft.Machine, draft.PathPrefix, draft.Layout, draft.Project,
			draft.OriginalProject, enabled,
		)
		if err != nil {
			return WorktreeProjectMapping{}, fmt.Errorf(
				"creating worktree reclassification mapping: %w", err,
			)
		}
		draft.ID, _ = result.LastInsertId()
	} else {
		draft.ID = existing.ID
		_, err := tx.ExecContext(ctx, `
			UPDATE worktree_project_mappings
			SET path_prefix = ?, layout = ?, project = ?,
				original_project = CASE
					WHEN original_project = '' THEN ? ELSE original_project END,
				enabled = ?,
				updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
			WHERE id = ? AND machine = ?`,
			draft.PathPrefix, draft.Layout, draft.Project,
			draft.OriginalProject, enabled, draft.ID, draft.Machine,
		)
		if err != nil {
			if isSQLiteUniqueConstraint(err) {
				return WorktreeProjectMapping{}, ErrWorktreeMappingDuplicate
			}
			return WorktreeProjectMapping{}, fmt.Errorf(
				"updating worktree reclassification mapping: %w", err,
			)
		}
	}
	mapping, err := scanWorktreeMappingRow(tx.QueryRowContext(ctx, `
		SELECT id, machine, path_prefix, layout, project, original_project,
			enabled, created_at, updated_at
		FROM worktree_project_mappings WHERE id = ? AND machine = ?`,
		draft.ID, draft.Machine))
	if err != nil {
		return WorktreeProjectMapping{}, fmt.Errorf(
			"reading worktree reclassification mapping: %w", err,
		)
	}
	return mapping, nil
}
