package clickhouse

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
)

const projectIdentityDeleteBatchSize = 300

// syncProjectIdentityObservations publishes project identity observations
// and per-session snapshots up through the local archive's current
// revision, using storedRev as the cursor. It performs a full publication
// when full is set, storedRev predates any real publication, or the
// mirror's source_archives scope looks stale; otherwise it applies the
// compact (storedRev, revision] delta. It returns the revision just
// published so the caller can persist it as mirror metadata's identity
// revision.
func (s *Sync) syncProjectIdentityObservations(
	ctx context.Context, storedRev int64, full bool,
	refreshSessionIDs []string,
) (int64, error) {
	revision, err := s.local.ProjectIdentityPublicationRevision(ctx)
	if err != nil {
		return 0, err
	}
	databaseGeneration, err := s.local.GetDatabaseID(ctx)
	if err != nil {
		return 0, fmt.Errorf("loading source database generation: %w", err)
	}
	archiveID, err := s.local.GetArchiveID(ctx)
	if err != nil {
		return 0, fmt.Errorf("loading source archive id: %w", err)
	}

	fullPublication := full || storedRev <= 0 || storedRev > revision
	if !fullPublication && storedRev == revision {
		present, err := s.identityArchivePresent(ctx, archiveID)
		if err != nil {
			return 0, err
		}
		if present && len(refreshSessionIDs) == 0 {
			return revision, nil
		}
		if !present {
			fullPublication = true
		}
	}

	observations, snapshots, delta, err := s.loadIdentityPublicationScope(
		ctx, fullPublication, storedRev, revision, refreshSessionIDs,
	)
	if err != nil {
		return 0, err
	}
	archiveSalt, err := s.local.GetArchiveSalt(ctx)
	if err != nil {
		return 0, fmt.Errorf("loading source archive salt: %w", err)
	}
	if err := s.writeIdentityPublication(
		ctx, archiveID, archiveSalt, databaseGeneration,
		fullPublication, delta, observations, snapshots, refreshSessionIDs,
	); err != nil {
		return 0, err
	}
	return revision, nil
}

func (s *Sync) identityArchivePresent(
	ctx context.Context, archiveID string,
) (bool, error) {
	var n int64
	if err := s.conn.QueryRowContext(ctx, `
		SELECT toInt64(COUNT(*)) FROM source_archives
		WHERE source_archive_id = ?`, archiveID).Scan(&n); err != nil {
		return false, fmt.Errorf("checking clickhouse project identity publication: %w", err)
	}
	return n > 0, nil
}

// loadIdentityPublicationScope loads either the full in-scope identity
// publication (observations plus session snapshots) or the compact delta
// for (priorRevision, revision], depending on fullPublication.
func (s *Sync) loadIdentityPublicationScope(
	ctx context.Context, fullPublication bool, priorRevision, revision int64,
	refreshSessionIDs []string,
) (
	observations, snapshots []export.ProjectIdentityObservation,
	delta db.ProjectIdentityPublicationDelta, err error,
) {
	if !fullPublication {
		delta, err = s.local.LoadProjectIdentityPublicationDelta(
			ctx, priorRevision, revision, s.projects, s.excludeProjects,
		)
		if err != nil {
			return nil, nil, delta, err
		}
		observations = delta.Observations
		snapshots = delta.Snapshots
	} else {
		observations, err = s.local.ListProjectIdentityObservations(ctx, nil)
		if err != nil {
			return nil, nil, delta, fmt.Errorf(
				"loading project identity observations: %w", err,
			)
		}
		observations = filterIdentityScope(
			observations, s.projects, s.excludeProjects,
		)
		snapshots, err = s.local.ListPublishableSessionProjectIdentitySnapshots(
			ctx, nil, s.projects, s.excludeProjects,
		)
		if err != nil {
			return nil, nil, delta, fmt.Errorf(
				"loading session project identity snapshots: %w", err,
			)
		}
	}
	if len(refreshSessionIDs) > 0 {
		refreshSnapshots, loadErr := s.local.ListPublishableSessionProjectIdentitySnapshots(
			ctx, refreshSessionIDs, s.projects, s.excludeProjects,
		)
		if loadErr != nil {
			return nil, nil, delta, fmt.Errorf(
				"loading refreshed session project identity snapshots: %w",
				loadErr,
			)
		}
		snapshots = mergeProjectIdentitySnapshots(snapshots, refreshSnapshots)
	}
	return observations, snapshots, delta, nil
}

// filterIdentityScope restricts a full-publication listing to the push
// scope. The delta path does not need this: LoadProjectIdentityPublicationDelta
// applies projects/excludeProjects in SQL.
func filterIdentityScope(
	items []export.ProjectIdentityObservation, projects, excludeProjects []string,
) []export.ProjectIdentityObservation {
	if len(projects) == 0 && len(excludeProjects) == 0 {
		return items
	}
	out := items[:0]
	for _, item := range items {
		if projectMatchesPushScope(item.Project, projects, excludeProjects) {
			out = append(out, item)
		}
	}
	return out
}

func mergeProjectIdentitySnapshots(
	base, refresh []export.ProjectIdentityObservation,
) []export.ProjectIdentityObservation {
	merged := make(map[string]export.ProjectIdentityObservation, len(base)+len(refresh))
	for _, snapshot := range base {
		merged[snapshot.SessionID] = snapshot
	}
	for _, snapshot := range refresh {
		merged[snapshot.SessionID] = snapshot
	}
	out := make([]export.ProjectIdentityObservation, 0, len(merged))
	for _, snapshot := range merged {
		out = append(out, snapshot)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].SessionID < out[j].SessionID
	})
	return out
}

func (s *Sync) writeIdentityPublication(
	ctx context.Context,
	archiveID, archiveSalt, databaseGeneration string,
	fullPublication bool,
	delta db.ProjectIdentityPublicationDelta,
	observations, snapshots []export.ProjectIdentityObservation,
	refreshSessionIDs []string,
) error {
	version := newPushVersion()
	if err := s.upsertSourceArchiveScope(ctx, archiveID, archiveSalt, version); err != nil {
		return err
	}
	if !fullPublication {
		if err := s.deleteProjectIdentityDelta(
			ctx, archiveID, databaseGeneration,
			delta.ObservationDeletes, delta.SnapshotDeletes,
		); err != nil {
			return err
		}
	}
	for i, obs := range observations {
		obs.SourceArchiveID = archiveID
		obs.SourceArchiveSalt = archiveSalt
		observations[i] = export.SanitizeStoredProjectIdentityObservation(obs)
	}
	if err := s.publishProjectIdentityObservations(
		ctx, archiveID, fullPublication, observations, version,
	); err != nil {
		return err
	}
	for i := range snapshots {
		snapshots[i] = export.SanitizeStoredProjectIdentityObservation(snapshots[i])
	}
	if err := s.publishSessionProjectIdentitySnapshots(
		ctx, archiveID, databaseGeneration, fullPublication,
		snapshots, refreshSessionIDs, version,
	); err != nil {
		return err
	}
	return nil
}

func (s *Sync) upsertSourceArchiveScope(
	ctx context.Context, archiveID, archiveSalt string, version uint64,
) error {
	var existingSalt string
	err := s.conn.QueryRowContext(ctx, `
		SELECT source_archive_salt
		FROM source_archives
		WHERE source_archive_id = ?`, archiveID).Scan(&existingSalt)
	if err == nil {
		if existingSalt != archiveSalt {
			return fmt.Errorf("archive salt mismatch for %q", archiveID)
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("reading clickhouse source archive scope: %w", err)
	}
	if err := insertRows(ctx, s.conn, "source_archives", [][]any{
		{archiveID, archiveSalt, version},
	}); err != nil {
		return fmt.Errorf("upserting clickhouse source archive scope: %w", err)
	}
	if _, err := s.conn.ExecContext(ctx,
		"DELETE FROM source_archives WHERE source_archive_id = ? AND push_version < ?",
		archiveID, version,
	); err != nil {
		return fmt.Errorf("deleting older clickhouse source archive rows: %w", err)
	}
	if err := s.conn.QueryRowContext(ctx, `
		SELECT source_archive_salt
		FROM source_archives
		WHERE source_archive_id = ?`, archiveID).Scan(&existingSalt); err != nil {
		return fmt.Errorf("verifying clickhouse source archive scope: %w", err)
	}
	if existingSalt != archiveSalt {
		return fmt.Errorf("archive salt mismatch for %q", archiveID)
	}
	return nil
}

func (s *Sync) deleteProjectIdentityDelta(
	ctx context.Context,
	archiveID, databaseGeneration string,
	observationKeys []db.ProjectIdentityObservationKey,
	snapshotKeys []db.SessionProjectIdentitySnapshotKey,
) error {
	for start := 0; start < len(observationKeys); start += projectIdentityDeleteBatchSize {
		end := min(start+projectIdentityDeleteBatchSize, len(observationKeys))
		args := []any{archiveID}
		tuples := make([]string, 0, end-start)
		for _, key := range observationKeys[start:end] {
			tuples = append(tuples, "(?, ?, ?, ?)")
			args = append(args, key.Project, key.Machine, key.RootPath, key.GitRemote)
		}
		if _, err := s.conn.ExecContext(ctx, `
			DELETE FROM source_project_identity_observations
			WHERE source_archive_id = ?
			  AND (project, machine, root_path, git_remote) IN (`+
			strings.Join(tuples, ", ")+`)`, args...); err != nil {
			return fmt.Errorf("deleting clickhouse project identity observation delta: %w", err)
		}
	}
	for start := 0; start < len(snapshotKeys); start += projectIdentityDeleteBatchSize {
		end := min(start+projectIdentityDeleteBatchSize, len(snapshotKeys))
		args := []any{archiveID, databaseGeneration}
		tuples := make([]string, 0, end-start)
		for _, key := range snapshotKeys[start:end] {
			args = append(args, key.SessionID, key.Project)
			tuples = append(tuples, "(?, ?)")
		}
		if _, err := s.conn.ExecContext(ctx, `
			DELETE FROM source_session_project_identity_snapshots
			WHERE source_archive_id = ?
			  AND source_database_generation = ?
			  AND (source_session_id, project) IN (`+
			strings.Join(tuples, ", ")+`)`,
			args...,
		); err != nil {
			return fmt.Errorf("deleting clickhouse session identity snapshot delta: %w", err)
		}
	}
	return nil
}

func (s *Sync) publishProjectIdentityObservations(
	ctx context.Context,
	archiveID string,
	fullPublication bool,
	observations []export.ProjectIdentityObservation,
	version uint64,
) error {
	plan := planProjectIdentityObservationSync(observations)
	toInsert := make([]export.ProjectIdentityObservation, 0,
		len(plan.realRemote)+len(plan.ambiguous)+len(plan.fallbacks))
	toInsert = append(toInsert, plan.realRemote...)
	toInsert = append(toInsert, plan.ambiguous...)
	if fullPublication {
		toInsert = append(toInsert, plan.fallbacks...)
	} else {
		if err := s.deleteProjectIdentityFallbackRows(ctx, plan.realRoots); err != nil {
			return err
		}
		fallbacks, err := s.projectIdentityFallbacksWithoutRealRemote(ctx, plan.fallbacks)
		if err != nil {
			return err
		}
		toInsert = append(toInsert, fallbacks...)
	}

	rows := make([][]any, 0, len(toInsert))
	for _, obs := range toInsert {
		rows = append(rows, projectIdentityObservationRow(obs, version))
	}
	if err := insertRows(ctx, s.conn, "source_project_identity_observations", rows); err != nil {
		return fmt.Errorf("syncing clickhouse project identity observations: %w", err)
	}
	if fullPublication {
		if _, err := s.conn.ExecContext(ctx, `
			DELETE FROM source_project_identity_observations
			WHERE source_archive_id = ? AND push_version < ?`,
			archiveID, version,
		); err != nil {
			return fmt.Errorf("deleting older clickhouse project identity observations: %w", err)
		}
		return nil
	}
	return s.deleteOlderObservationKeys(ctx, archiveID, toInsert, version)
}

func (s *Sync) deleteOlderObservationKeys(
	ctx context.Context,
	archiveID string,
	observations []export.ProjectIdentityObservation,
	version uint64,
) error {
	for start := 0; start < len(observations); start += projectIdentityDeleteBatchSize {
		end := min(start+projectIdentityDeleteBatchSize, len(observations))
		args := []any{archiveID}
		tuples := make([]string, 0, end-start)
		for _, obs := range observations[start:end] {
			tuples = append(tuples, "(?, ?, ?, ?)")
			args = append(args, obs.Project, obs.Machine, obs.RootPath, obs.GitRemote)
		}
		args = append(args, version)
		if _, err := s.conn.ExecContext(ctx, `
			DELETE FROM source_project_identity_observations
			WHERE source_archive_id = ?
			  AND (project, machine, root_path, git_remote) IN (`+
			strings.Join(tuples, ", ")+`)
			  AND push_version < ?`, args...); err != nil {
			return fmt.Errorf("deleting older clickhouse project identity observations: %w", err)
		}
	}
	return nil
}

func (s *Sync) publishSessionProjectIdentitySnapshots(
	ctx context.Context,
	archiveID, databaseGeneration string,
	fullPublication bool,
	snapshots []export.ProjectIdentityObservation,
	refreshSessionIDs []string,
	version uint64,
) error {
	rows := make([][]any, 0, len(snapshots))
	sessionIDs := make([]string, 0, len(snapshots))
	for _, snap := range snapshots {
		rows = append(rows, sessionProjectIdentitySnapshotRow(
			archiveID, databaseGeneration, snap, version,
		))
		sessionIDs = append(sessionIDs, snap.SessionID)
	}
	if err := insertRows(ctx, s.conn, "source_session_project_identity_snapshots", rows); err != nil {
		return fmt.Errorf("syncing clickhouse session project identity snapshots: %w", err)
	}
	if fullPublication {
		if _, err := s.conn.ExecContext(ctx, `
			DELETE FROM source_session_project_identity_snapshots
			WHERE source_archive_id = ? AND push_version < ?`,
			archiveID, version,
		); err != nil {
			return fmt.Errorf("deleting older clickhouse session identity snapshots: %w", err)
		}
		return nil
	}
	if err := s.deleteOlderSnapshotSessionIDs(
		ctx, archiveID, refreshSessionIDs, version,
	); err != nil {
		return err
	}
	return s.deleteOlderSnapshotKeys(ctx, archiveID, databaseGeneration, sessionIDs, version)
}

func (s *Sync) deleteOlderSnapshotSessionIDs(
	ctx context.Context, archiveID string, sessionIDs []string, version uint64,
) error {
	for batch := range idBatches(uniqueIDs(sessionIDs)) {
		placeholders, args := inArgs(batch)
		args = append([]any{archiveID}, args...)
		args = append(args, version)
		if _, err := s.conn.ExecContext(ctx, `
			DELETE FROM source_session_project_identity_snapshots
			WHERE source_archive_id = ?
			  AND source_session_id IN (`+placeholders+`)
			  AND push_version < ?`, args...); err != nil {
			return fmt.Errorf(
				"deleting older clickhouse session identity snapshots by session id: %w", err,
			)
		}
	}
	return nil
}

func (s *Sync) deleteOlderSnapshotKeys(
	ctx context.Context,
	archiveID, databaseGeneration string,
	sessionIDs []string,
	version uint64,
) error {
	for batch := range idBatches(uniqueIDs(sessionIDs)) {
		placeholders, args := inArgs(batch)
		args = append([]any{archiveID, databaseGeneration}, args...)
		args = append(args, version)
		if _, err := s.conn.ExecContext(ctx, `
			DELETE FROM source_session_project_identity_snapshots
			WHERE source_archive_id = ?
			  AND source_database_generation = ?
			  AND source_session_id IN (`+placeholders+`)
			  AND push_version < ?`, args...); err != nil {
			return fmt.Errorf("deleting older clickhouse session identity snapshots: %w", err)
		}
	}
	return nil
}

func projectIdentityObservationRow(obs export.ProjectIdentityObservation, version uint64) []any {
	return []any{
		obs.SourceArchiveID, obs.SourceArchiveSalt,
		obs.Project, obs.Machine, obs.RootPath, obs.GitRemote,
		obs.GitRemoteName, obs.RepositoryPath, obs.WorktreeName,
		obs.WorktreeRootPath, string(obs.WorktreeRelationship),
		string(obs.CheckoutState),
		obs.GitBranch, string(obs.RemoteResolution),
		int64(obs.RemoteCandidateCount), observedAtValue(obs.ObservedAt),
		obs.NormalizedRemote, obs.KeySource, obs.Key,
		version,
	}
}

func sessionProjectIdentitySnapshotRow(
	archiveID, databaseGeneration string,
	obs export.ProjectIdentityObservation,
	version uint64,
) []any {
	return []any{
		archiveID, databaseGeneration, obs.SessionID,
		obs.Project, obs.Machine, obs.RootPath, obs.GitRemote,
		obs.GitRemoteName, obs.RepositoryPath, obs.WorktreeName,
		obs.WorktreeRootPath, string(obs.WorktreeRelationship),
		string(obs.CheckoutState),
		obs.GitBranch, string(obs.RemoteResolution),
		int64(obs.RemoteCandidateCount), observedAtValue(obs.ObservedAt),
		obs.NormalizedRemote, obs.KeySource, obs.Key,
		version,
	}
}

func observedAtValue(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	utc := t.UTC()
	return &utc
}

type projectIdentityRootKey struct {
	archiveID string
	project   string
	machine   string
	rootPath  string
}

func observationRootKey(obs export.ProjectIdentityObservation) projectIdentityRootKey {
	return projectIdentityRootKey{
		archiveID: obs.SourceArchiveID,
		project:   obs.Project,
		machine:   obs.Machine,
		rootPath:  obs.RootPath,
	}
}

type projectIdentityObservationPlan struct {
	realRemote []export.ProjectIdentityObservation
	ambiguous  []export.ProjectIdentityObservation
	fallbacks  []export.ProjectIdentityObservation
	realRoots  []projectIdentityRootKey
}

// planProjectIdentityObservationSync reduces a batch to the final state of
// applying the DuckDB per-row upsert in order: the last observation per
// conflict key wins. Ordinary empty-remote fallbacks never survive
// alongside real-remote evidence for the same root, while ambiguous
// observations always survive because they are conflicting evidence rather
// than root-derived fallbacks.
func planProjectIdentityObservationSync(
	observations []export.ProjectIdentityObservation,
) projectIdentityObservationPlan {
	type conflictKey struct {
		root      projectIdentityRootKey
		gitRemote string
	}
	keyOrder := make([]conflictKey, 0, len(observations))
	latest := make(map[conflictKey]export.ProjectIdentityObservation,
		len(observations))
	realRootSet := make(map[projectIdentityRootKey]bool)

	var plan projectIdentityObservationPlan
	for _, obs := range observations {
		key := conflictKey{
			root: observationRootKey(obs), gitRemote: obs.GitRemote,
		}
		previous, seen := latest[key]
		if !seen {
			keyOrder = append(keyOrder, key)
		} else if key.gitRemote == "" &&
			previous.RemoteResolution == export.ProjectResolutionAmbiguous &&
			obs.RemoteResolution != export.ProjectResolutionAmbiguous {
			continue
		}
		latest[key] = obs
		if obs.GitRemote != "" && !realRootSet[key.root] {
			realRootSet[key.root] = true
			plan.realRoots = append(plan.realRoots, key.root)
		}
	}
	for _, key := range keyOrder {
		obs := latest[key]
		if obs.GitRemote != "" {
			plan.realRemote = append(plan.realRemote, obs)
			continue
		}
		if obs.RemoteResolution == export.ProjectResolutionAmbiguous {
			plan.ambiguous = append(plan.ambiguous, obs)
			continue
		}
		if !realRootSet[key.root] {
			plan.fallbacks = append(plan.fallbacks, obs)
		}
	}
	return plan
}

func (s *Sync) deleteProjectIdentityFallbackRows(
	ctx context.Context,
	roots []projectIdentityRootKey,
) error {
	for start := 0; start < len(roots); start += projectIdentityDeleteBatchSize {
		end := min(start+projectIdentityDeleteBatchSize, len(roots))
		tuples, tupleArgs := rootKeyTupleArgs(roots[start:end])
		args := append([]any{export.ProjectResolutionAmbiguous}, tupleArgs...)
		if _, err := s.conn.ExecContext(ctx, `
			DELETE FROM source_project_identity_observations
			WHERE git_remote = ''
			  AND remote_resolution != ?
			  AND (source_archive_id, project, machine, root_path) IN (`+tuples+`)`,
			args...,
		); err != nil {
			return fmt.Errorf(
				"removing stale clickhouse project identity root fallbacks: %w", err,
			)
		}
	}
	return nil
}

func rootKeyTupleArgs(keys []projectIdentityRootKey) (string, []any) {
	tuples := make([]string, len(keys))
	args := make([]any, 0, len(keys)*4)
	for i, key := range keys {
		tuples[i] = "(?, ?, ?, ?)"
		args = append(args, key.archiveID, key.project, key.machine, key.rootPath)
	}
	return strings.Join(tuples, ", "), args
}

func (s *Sync) projectIdentityFallbacksWithoutRealRemote(
	ctx context.Context,
	candidates []export.ProjectIdentityObservation,
) ([]export.ProjectIdentityObservation, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	shadowed := make(map[projectIdentityRootKey]bool)
	for start := 0; start < len(candidates); start += projectIdentityDeleteBatchSize {
		end := min(start+projectIdentityDeleteBatchSize, len(candidates))
		keys := make([]projectIdentityRootKey, 0, end-start)
		for _, obs := range candidates[start:end] {
			keys = append(keys, observationRootKey(obs))
		}
		tuples, tupleArgs := rootKeyTupleArgs(keys)
		args := append([]any{export.ProjectResolutionAmbiguous}, tupleArgs...)
		rows, err := s.conn.QueryContext(ctx, `
			SELECT DISTINCT source_archive_id, project, machine, root_path
			FROM source_project_identity_observations
			WHERE (git_remote != '' OR remote_resolution = ?)
			  AND (source_archive_id, project, machine, root_path) IN (`+tuples+`)`,
			args...,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"checking clickhouse project identity remote observations: %w", err,
			)
		}
		if err := scanProjectIdentityRootKeys(rows, shadowed); err != nil {
			return nil, err
		}
	}
	out := make([]export.ProjectIdentityObservation, 0, len(candidates))
	for _, obs := range candidates {
		if !shadowed[observationRootKey(obs)] {
			out = append(out, obs)
		}
	}
	return out, nil
}

func scanProjectIdentityRootKeys(
	rows *sql.Rows, out map[projectIdentityRootKey]bool,
) error {
	defer rows.Close()
	for rows.Next() {
		var key projectIdentityRootKey
		if err := rows.Scan(
			&key.archiveID, &key.project, &key.machine, &key.rootPath,
		); err != nil {
			return fmt.Errorf(
				"scanning clickhouse project identity remote observation: %w", err,
			)
		}
		out[key] = true
	}
	return rows.Err()
}
