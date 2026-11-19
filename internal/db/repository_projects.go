package db

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"slices"

	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/parser"
)

// RepositoryProjectChange groups proposed repairs by source and destination.
type RepositoryProjectChange struct {
	Repository string `json:"repository"`
	From       string `json:"from"`
	To         string `json:"to"`
	Reason     string `json:"reason"`
	Sessions   int    `json:"sessions"`
}

type RepositoryProjectRepair struct {
	Changes           []RepositoryProjectChange `json:"changes"`
	Sessions          int                       `json:"sessions"`
	AmbiguousSessions int                       `json:"ambiguous_sessions"`
}

type repositoryProjectName struct{ project, reason string }

// Existing archives need the same attribution as new imports, even when their
// source files and worktrees are gone. Commit the completion marker with the
// metadata changes so an interrupted upgrade retries on the next writable open.
func (db *DB) migrateRepositoryProjects(ctx context.Context, progress OpenProgressFunc) error {
	return db.Update(ctx, func(tx *sql.Tx) error {
		var done bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM stats WHERE key = 'repository_projects_repaired' AND value = 1
		)`).Scan(&done); err != nil || done {
			return err
		}
		progress.report("Repairing repository project names")
		if _, err := repairRepositoryProjectsTx(ctx, tx, "", true); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO stats (key, value)
			VALUES ('repository_projects_repaired', 1)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`)
		return err
	})
}

// Cache within a transaction, never across writes or archives. A saved choice
// prevents later imports or pruning from changing an established name.
type repositoryProjectResolver struct {
	tx       *sql.Tx
	hasNames bool
	names    map[string]repositoryProjectName
	mappings map[string][]WorktreeProjectMapping
}

func newRepositoryProjectResolver(ctx context.Context, tx *sql.Tx) (*repositoryProjectResolver, error) {
	var hasNames bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'repository_project_names'
	)`).Scan(&hasNames); err != nil {
		return nil, err
	}
	return &repositoryProjectResolver{
		tx: tx, hasNames: hasNames,
		names: map[string]repositoryProjectName{}, mappings: map[string][]WorktreeProjectMapping{},
	}, nil
}

func (r *repositoryProjectResolver) name(ctx context.Context, remote string) (repositoryProjectName, error) {
	if name, ok := r.names[remote]; ok {
		return name, nil
	}
	var name repositoryProjectName
	if r.hasNames {
		err := r.tx.QueryRowContext(ctx, `SELECT project, reason FROM repository_project_names
			WHERE normalized_remote = ?`, remote).Scan(&name.project, &name.reason)
		if err == nil {
			r.names[remote] = name
			return name, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return name, err
		}
	}
	base := parser.NormalizeName(path.Base(remote))
	rows, err := r.tx.QueryContext(ctx, `SELECT DISTINCT project, repository_path, worktree_relationship, recorded_git_remote
		FROM session_project_identity_snapshots WHERE normalized_remote = ? AND remote_resolution = 'resolved'`, remote)
	if err != nil {
		return name, err
	}
	defer rows.Close()
	aliases := map[string]bool{}
	exact := false
	for rows.Next() {
		var project, root, recorded string
		var relationship export.WorktreeRelationship
		if err := rows.Scan(&project, &root, &relationship, &recorded); err != nil {
			return name, err
		}
		if recorded != "" {
			if sourceRemote, ok := export.NormalizeGitRemote(recorded); !ok || sourceRemote != remote {
				continue
			}
		}
		exact = exact || project == base
		if project != "" && root != "" && (relationship == export.WorktreeMain || relationship == export.WorktreeLinked) {
			aliases[project] = true
		}
	}
	if err := rows.Err(); err != nil {
		return name, err
	}
	switch {
	case exact:
		name = repositoryProjectName{base, "repository name"}
	case len(aliases) == 1:
		for alias := range aliases {
			name = repositoryProjectName{alias, "known checkout"}
		}
	case len(aliases) == 0:
		name = repositoryProjectName{base, "recorded repository"}
	default:
		// Conflicting checkout names need an explicit project rule.
	}
	r.names[remote] = name
	return name, nil
}

type repositoryProjectCandidate struct {
	worktreeMappingSessionUpdate
	remote string
}

func repositoryProjectCandidates(ctx context.Context, tx *sql.Tx, sessionID, project string) ([]repositoryProjectCandidate, error) {
	query := `SELECT s.id, s.machine, s.cwd, s.project, snap.recorded_git_remote
		FROM sessions s JOIN session_project_identity_snapshots snap ON snap.session_id = s.id
		WHERE s.deleted_at IS NULL AND s.project = snap.project
		AND snap.recorded_git_remote != ''
		AND snap.worktree_relationship IN ('', 'unknown')
		AND NOT EXISTS (SELECT 1 FROM session_project_assignments a WHERE a.session_id = s.id)`
	var args []any
	if sessionID != "" {
		query += " AND s.id = ?"
		args = append(args, sessionID)
	}
	if project != "" {
		query += " AND s.project = ?"
		args = append(args, project)
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var candidates []repositoryProjectCandidate
	for rows.Next() {
		var c repositoryProjectCandidate
		var raw string
		if err := rows.Scan(&c.id, &c.machine, &c.cwd, &c.currentProject, &raw); err != nil {
			return nil, err
		}
		// Producer evidence wins over a possibly unrelated repository on the
		// receiving host. Local/file remotes cannot identify a repository here.
		if remote, ok := export.NormalizeGitRemote(raw); ok {
			c.remote = remote
			candidates = append(candidates, c)
		}
	}
	return candidates, rows.Err()
}

func (r *repositoryProjectResolver) resolve(ctx context.Context, c repositoryProjectCandidate) (repositoryProjectName, bool, error) {
	mappings, ok := r.mappings[c.machine]
	if !ok {
		var err error
		mappings, err = loadActiveWorktreeMappingsTx(ctx, r.tx, c.machine)
		if err != nil {
			return repositoryProjectName{}, false, err
		}
		r.mappings[c.machine] = mappings
	}
	if _, matched := ResolveWorktreeProjectFromMappings(mappings, c.cwd, c.currentProject); matched {
		return repositoryProjectName{}, false, nil
	}
	name, err := r.name(ctx, c.remote)
	return name, name.project == "", err
}

func (r *repositoryProjectResolver) apply(ctx context.Context, c repositoryProjectCandidate, name repositoryProjectName) error {
	if _, err := r.tx.ExecContext(ctx, `INSERT INTO repository_project_names (normalized_remote, project, reason)
		VALUES (?, ?, ?) ON CONFLICT(normalized_remote) DO NOTHING`, c.remote, name.project, name.reason); err != nil {
		return err
	}
	if c.currentProject == name.project {
		return nil
	}
	c.nextProject = name.project
	if _, err := updateSessionProjectTx(ctx, r.tx, c.worktreeMappingSessionUpdate, true); err != nil {
		return err
	}
	return reconcileSessionProjectIdentityAggregatesTx(ctx, r.tx, c.id, []string{c.currentProject, name.project})
}

// Resolve the effective label before publishing aggregate evidence. Snapshot
// reconciliation must see both the final session project and the new remote
// observation, especially when repairing older root-only evidence.
func attributeRepositoryObservationTx(ctx context.Context, tx *sql.Tx, obs export.ProjectIdentityObservation, sourceProject string) (export.ProjectIdentityObservation, error) {
	if !obs.SourceRecorded || obs.SessionID == "" || obs.Project != sourceProject ||
		obs.WorktreeRelationship != export.WorktreeUnknown {
		return obs, nil
	}
	remote, ok := export.NormalizeGitRemote(obs.GitRemote)
	if !ok {
		return obs, nil
	}
	c := repositoryProjectCandidate{remote: remote}
	err := tx.QueryRowContext(ctx, `SELECT s.id, s.machine, s.cwd, s.project FROM sessions s
		WHERE s.id = ? AND s.project = ? AND s.deleted_at IS NULL
		AND NOT EXISTS (SELECT 1 FROM session_project_identity_snapshots snap
			WHERE snap.session_id = s.id AND snap.worktree_relationship NOT IN ('', 'unknown'))
		AND NOT EXISTS (SELECT 1 FROM session_project_assignments a WHERE a.session_id = s.id)`,
		obs.SessionID, sourceProject).Scan(&c.id, &c.machine, &c.cwd, &c.currentProject)
	if errors.Is(err, sql.ErrNoRows) {
		return obs, nil
	}
	if err != nil {
		return obs, err
	}
	r, err := newRepositoryProjectResolver(ctx, tx)
	if err != nil {
		return obs, err
	}
	name, _, err := r.resolve(ctx, c)
	if err != nil || name.project == "" {
		return obs, err
	}
	if err := r.apply(ctx, c, name); err != nil {
		return obs, err
	}
	obs.Project = name.project
	return obs, nil
}

// Restore automatic attribution after clearing an explicit assignment.
func applyRecordedRepositoryProjectTx(ctx context.Context, tx *sql.Tx, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	candidates, err := repositoryProjectCandidates(ctx, tx, sessionID, "")
	if err != nil || len(candidates) == 0 {
		return err
	}
	r, err := newRepositoryProjectResolver(ctx, tx)
	if err != nil {
		return err
	}
	for _, c := range candidates {
		name, _, err := r.resolve(ctx, c)
		if err != nil {
			return err
		}
		if name.project != "" {
			if err := r.apply(ctx, c, name); err != nil {
				return err
			}
		}
	}
	return nil
}

// PreviewRepositoryProjects uses only saved metadata and does not open sources
// or checkouts. It also works on archives created before repository names were
// persisted. Project is an exact source-label filter; empty means all labels.
func (db *DB) PreviewRepositoryProjects(ctx context.Context, project string) (RepositoryProjectRepair, error) {
	release, err := db.acquireBulkRead(ctx)
	if err != nil {
		return RepositoryProjectRepair{}, err
	}
	defer release()
	tx, err := db.getReader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return RepositoryProjectRepair{}, err
	}
	defer func() { _ = tx.Rollback() }()
	return repairRepositoryProjectsTx(ctx, tx, project, false)
}

// RepairRepositoryProjects changes project metadata atomically, keeping
// transcripts, message identities, and producer snapshots intact.
func (db *DB) RepairRepositoryProjects(ctx context.Context, project string) (RepositoryProjectRepair, error) {
	if err := db.requireWritable(); err != nil {
		return RepositoryProjectRepair{}, err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return RepositoryProjectRepair{}, err
	}
	defer func() { _ = tx.Rollback() }()
	report, err := repairRepositoryProjectsTx(ctx, tx, project, true)
	if err != nil {
		return RepositoryProjectRepair{}, fmt.Errorf("repairing repository projects: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return RepositoryProjectRepair{}, err
	}
	return report, nil
}

func repairRepositoryProjectsTx(ctx context.Context, tx *sql.Tx, project string, apply bool) (RepositoryProjectRepair, error) {
	report := RepositoryProjectRepair{Changes: []RepositoryProjectChange{}}
	candidates, err := repositoryProjectCandidates(ctx, tx, "", project)
	if err != nil {
		return report, err
	}
	r, err := newRepositoryProjectResolver(ctx, tx)
	if err != nil {
		return report, err
	}
	type changeKey struct{ remote, from, to string }
	changes := map[changeKey]RepositoryProjectChange{}
	// Resolve every name before applying changes so preview and apply share
	// one set of source evidence and naming decisions.
	for _, c := range candidates {
		name, ambiguous, err := r.resolve(ctx, c)
		if err != nil {
			return report, err
		}
		if ambiguous {
			report.AmbiguousSessions++
		}
		if name.project == "" || name.project == c.currentProject {
			continue
		}
		key := changeKey{c.remote, c.currentProject, name.project}
		change := changes[key]
		change.Repository, change.From, change.To, change.Reason = c.remote, c.currentProject, name.project, name.reason
		change.Sessions++
		changes[key] = change
		report.Sessions++
	}
	if apply {
		for _, c := range candidates {
			name, _, err := r.resolve(ctx, c)
			if err != nil {
				return report, err
			}
			if name.project != "" {
				if err := r.apply(ctx, c, name); err != nil {
					return report, err
				}
			}
		}
	}
	for _, change := range changes {
		report.Changes = append(report.Changes, change)
	}
	slices.SortFunc(report.Changes, func(a, b RepositoryProjectChange) int {
		if a.Repository != b.Repository {
			return cmp.Compare(a.Repository, b.Repository)
		}
		return cmp.Compare(a.From, b.From)
	})
	return report, nil
}
