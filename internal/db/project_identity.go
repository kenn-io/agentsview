package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/export"
)

const archiveMetadataDatabaseIDKey = "database_id"

var ErrDatabaseIDMissing = errors.New("database id is missing")

func (db *DB) GetDatabaseID(ctx context.Context) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var id string
	err := db.getReader().QueryRowContext(ctx,
		`SELECT value FROM archive_metadata WHERE key = ?`,
		archiveMetadataDatabaseIDKey,
	).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrDatabaseIDMissing
		}
		return "", fmt.Errorf("reading database id: %w", err)
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return "", ErrDatabaseIDMissing
	}
	return id, nil
}

func (db *DB) GetOrCreateDatabaseID(ctx context.Context) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	id, err := db.GetDatabaseID(ctx)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, ErrDatabaseIDMissing) {
		return "", err
	}
	if err := db.requireWritable(); err != nil {
		return "", ErrDatabaseIDMissing
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	err = db.getWriter().QueryRowContext(ctx,
		`SELECT value FROM archive_metadata WHERE key = ?`,
		archiveMetadataDatabaseIDKey,
	).Scan(&id)
	if err == nil && strings.TrimSpace(id) != "" {
		return id, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("reading database id: %w", err)
	}
	id, err = newUUIDv4()
	if err != nil {
		return "", err
	}
	if _, err := db.getWriter().ExecContext(ctx, `
		INSERT INTO archive_metadata (key, value)
		VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET
			value = excluded.value,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE trim(archive_metadata.value) = ''`,
		archiveMetadataDatabaseIDKey, id,
	); err != nil {
		return "", fmt.Errorf("creating database id: %w", err)
	}
	var persisted string
	if err := db.getWriter().QueryRowContext(ctx,
		`SELECT value FROM archive_metadata WHERE key = ?`,
		archiveMetadataDatabaseIDKey,
	).Scan(&persisted); err != nil {
		return "", fmt.Errorf("rereading database id: %w", err)
	}
	return persisted, nil
}

func (db *DB) SetDatabaseIDForTest(ctx context.Context, id string) error {
	if err := db.requireWritable(); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("database id is required")
	}

	db.mu.Lock()
	defer db.mu.Unlock()
	_, err := db.getWriter().ExecContext(ctx, `
		INSERT INTO archive_metadata (key, value)
		VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET
			value = excluded.value,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')`,
		archiveMetadataDatabaseIDKey, id,
	)
	if err != nil {
		return fmt.Errorf("setting database id: %w", err)
	}
	return nil
}

func (db *DB) UpsertProjectIdentityObservation(
	ctx context.Context,
	obs export.ProjectIdentityObservation,
) error {
	if err := db.requireWritable(); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	obs, err := normalizeProjectIdentityObservation(obs)
	if err != nil {
		return err
	}

	db.mu.Lock()
	defer db.mu.Unlock()
	if err := upsertProjectIdentityObservationExec(
		ctx, db.getWriter(), db.getWriter().QueryRowContext, obs,
	); err != nil {
		return err
	}
	return nil
}

func upsertProjectIdentityObservationTx(
	tx *sql.Tx,
	obs export.ProjectIdentityObservation,
) error {
	normalized, err := normalizeProjectIdentityObservation(obs)
	if err != nil {
		return err
	}
	if err := upsertProjectIdentityObservationExec(
		context.Background(), tx,
		func(ctx context.Context, query string, args ...any) rowScanner {
			return tx.QueryRowContext(ctx, query, args...)
		},
		normalized,
	); err != nil {
		return err
	}
	return nil
}

type contextExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

type contextQueryRow func(context.Context, string, ...any) rowScanner

func upsertProjectIdentityObservationExec(
	ctx context.Context,
	exec contextExecer,
	queryRow contextQueryRow,
	obs export.ProjectIdentityObservation,
) error {
	return upsertProjectIdentityObservationExecExcludingRemote(
		ctx, exec, queryRow, obs, "")
}

func upsertProjectIdentityObservationExecExcludingRemote(
	ctx context.Context,
	exec contextExecer,
	queryRow contextQueryRow,
	obs export.ProjectIdentityObservation,
	excludeRemote string,
) error {
	if obs.GitRemote == "" {
		var exists int
		query := `
			SELECT 1 FROM project_identity_observations
			WHERE project = ? AND machine = ? AND root_path = ?
			  AND git_remote != ''`
		args := []any{obs.Project, obs.Machine, obs.RootPath}
		if excludeRemote != "" {
			query += ` AND git_remote != ?`
			args = append(args, excludeRemote)
		}
		query += ` LIMIT 1`
		err := queryRow(ctx, `
			`+strings.TrimSpace(query),
			args...,
		).Scan(&exists)
		if err == nil && exists == 1 {
			return nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("checking project identity remote observation: %w", err)
		}
	} else if _, err := exec.ExecContext(ctx, `
		DELETE FROM project_identity_observations
		WHERE project = ? AND machine = ? AND root_path = ?
		  AND git_remote = ''`,
		obs.Project, obs.Machine, obs.RootPath,
	); err != nil {
		return fmt.Errorf("removing stale project identity root fallback: %w", err)
	}

	_, err := exec.ExecContext(ctx, `
		INSERT INTO project_identity_observations (
			project, machine, root_path, git_remote, git_remote_name,
			worktree_name, worktree_root_path, observed_at,
			normalized_remote, key_source, key
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(project, machine, root_path, git_remote) DO UPDATE SET
			git_remote_name = excluded.git_remote_name,
			worktree_name = excluded.worktree_name,
			worktree_root_path = excluded.worktree_root_path,
			observed_at = excluded.observed_at,
			normalized_remote = excluded.normalized_remote,
			key_source = excluded.key_source,
			key = excluded.key`,
		obs.Project, obs.Machine, obs.RootPath, obs.GitRemote,
		obs.GitRemoteName, obs.WorktreeName, obs.WorktreeRootPath,
		obs.ObservedAt.UTC().Format(time.RFC3339Nano),
		obs.NormalizedRemote, obs.KeySource, obs.Key,
	)
	if err != nil {
		return fmt.Errorf("upserting project identity observation: %w", err)
	}
	return nil
}

func normalizeProjectIdentityObservation(
	obs export.ProjectIdentityObservation,
) (export.ProjectIdentityObservation, error) {
	obs.Project = strings.TrimSpace(obs.Project)
	obs.Machine = strings.TrimSpace(obs.Machine)
	obs.RootPath = strings.TrimSpace(obs.RootPath)
	obs.GitRemote = export.SanitizeGitRemoteForStorage(obs.GitRemote)
	obs.GitRemoteName = strings.TrimSpace(obs.GitRemoteName)
	obs.WorktreeName = strings.TrimSpace(obs.WorktreeName)
	obs.WorktreeRootPath = strings.TrimSpace(obs.WorktreeRootPath)
	if obs.Project == "" {
		return obs, fmt.Errorf("project is required")
	}
	if obs.Machine == "" {
		return obs, fmt.Errorf("machine is required")
	}
	if obs.ObservedAt.IsZero() {
		obs.ObservedAt = time.Now().UTC()
	}
	identity := export.BuildProjectIdentity(
		export.ProjectIdentityInput{
			RootPath:         obs.RootPath,
			GitRemote:        obs.GitRemote,
			GitRemoteName:    obs.GitRemoteName,
			WorktreeName:     obs.WorktreeName,
			WorktreeRootPath: obs.WorktreeRootPath,
		},
	)
	obs.NormalizedRemote = identity.NormalizedRemote
	obs.KeySource = identity.KeySource
	obs.Key = identity.Key
	return obs, nil
}

func scrubProjectIdentityGitRemoteCredentialsTx(
	ctx context.Context, tx *sql.Tx,
) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT project, machine, root_path, git_remote, git_remote_name,
			worktree_name, worktree_root_path, observed_at,
			normalized_remote, key_source, key
		FROM project_identity_observations
		WHERE git_remote != ''`)
	if err != nil {
		return fmt.Errorf("listing project identity remotes for scrub: %w", err)
	}

	type pendingScrub struct {
		obs       export.ProjectIdentityObservation
		rawRemote string
	}
	var pending []pendingScrub
	for rows.Next() {
		var obs export.ProjectIdentityObservation
		var observedAt string
		if err := rows.Scan(
			&obs.Project,
			&obs.Machine,
			&obs.RootPath,
			&obs.GitRemote,
			&obs.GitRemoteName,
			&obs.WorktreeName,
			&obs.WorktreeRootPath,
			&observedAt,
			&obs.NormalizedRemote,
			&obs.KeySource,
			&obs.Key,
		); err != nil {
			return fmt.Errorf("scanning project identity remote for scrub: %w", err)
		}
		if t, err := time.Parse(time.RFC3339Nano, observedAt); err == nil {
			obs.ObservedAt = t
		}
		sanitized := export.SanitizeGitRemoteForStorage(obs.GitRemote)
		if sanitized == obs.GitRemote {
			continue
		}
		pending = append(pending, pendingScrub{obs: obs, rawRemote: obs.GitRemote})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating project identity remotes for scrub: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("closing project identity remotes scrub rows: %w", err)
	}

	for _, scrub := range pending {
		obs := scrub.obs
		obs = export.SanitizeStoredProjectIdentityObservation(obs)
		normalized, err := normalizeProjectIdentityObservation(obs)
		if err != nil {
			return fmt.Errorf("normalizing project identity remote scrub: %w", err)
		}
		if err := upsertProjectIdentityObservationExecExcludingRemote(
			ctx, tx,
			func(ctx context.Context, query string, args ...any) rowScanner {
				return tx.QueryRowContext(ctx, query, args...)
			},
			normalized, scrub.rawRemote,
		); err != nil {
			return fmt.Errorf("scrubbing project identity remote: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM project_identity_observations
			WHERE project = ? AND machine = ? AND root_path = ?
			  AND git_remote = ?`,
			scrub.obs.Project, scrub.obs.Machine, scrub.obs.RootPath,
			scrub.rawRemote,
		); err != nil {
			return fmt.Errorf("removing raw project identity remote: %w", err)
		}
	}
	return nil
}

func (db *DB) ListProjectIdentityObservations(
	ctx context.Context,
	labels []string,
) ([]export.ProjectIdentityObservation, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if labels != nil && len(labels) == 0 {
		return []export.ProjectIdentityObservation{}, nil
	}
	query := `SELECT project, machine, root_path, git_remote, git_remote_name,
		worktree_name, worktree_root_path, observed_at,
		normalized_remote, key_source, key
		FROM project_identity_observations`
	args := make([]any, 0, len(labels))
	if len(labels) > 0 {
		placeholders := make([]string, 0, len(labels))
		for _, label := range labels {
			placeholders = append(placeholders, "?")
			args = append(args, label)
		}
		query += " WHERE project IN (" + strings.Join(placeholders, ",") + ")"
	}
	query += " ORDER BY project, machine, root_path, git_remote"

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing project identity observations: %w", err)
	}
	defer rows.Close()

	var out []export.ProjectIdentityObservation
	for rows.Next() {
		var obs export.ProjectIdentityObservation
		var observedAt string
		if err := rows.Scan(
			&obs.Project,
			&obs.Machine,
			&obs.RootPath,
			&obs.GitRemote,
			&obs.GitRemoteName,
			&obs.WorktreeName,
			&obs.WorktreeRootPath,
			&observedAt,
			&obs.NormalizedRemote,
			&obs.KeySource,
			&obs.Key,
		); err != nil {
			return nil, fmt.Errorf("scanning project identity observation: %w", err)
		}
		if t, err := time.Parse(time.RFC3339Nano, observedAt); err == nil {
			obs.ObservedAt = t
		}
		out = append(out, obs)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating project identity observations: %w", err)
	}
	return out, nil
}

func (db *DB) BuildProjectIdentityMap(
	ctx context.Context,
	labels []string,
) (map[string]export.ProjectMapEntry, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if labels != nil && len(labels) == 0 {
		return map[string]export.ProjectMapEntry{}, nil
	}
	observations, err := db.ListProjectIdentityObservations(ctx, labels)
	if err != nil {
		return nil, err
	}
	result := export.BuildProjectsMap(labels, observations)

	legacy, err := db.legacyProjectIdentityCandidates(ctx, labels)
	if err != nil {
		return nil, err
	}
	for _, label := range labels {
		if result[label].Resolution != export.ProjectResolutionUnknown {
			continue
		}
		candidates := legacy[label]
		switch len(candidates) {
		case 0:
			result[label] = export.ProjectMapEntry{
				Resolution: export.ProjectResolutionUnknown,
			}
		case 1:
			for _, identity := range candidates {
				i := identity
				result[label] = export.ProjectMapEntry{
					Resolution: export.ProjectResolutionResolved,
					Identity:   &i,
				}
			}
		default:
			result[label] = export.ProjectMapEntry{
				Resolution: export.ProjectResolutionAmbiguous,
			}
		}
	}
	return result, nil
}

func (db *DB) legacyProjectIdentityCandidates(
	ctx context.Context,
	labels []string,
) (map[string]map[string]export.ProjectIdentity, error) {
	out := map[string]map[string]export.ProjectIdentity{}
	if len(labels) == 0 {
		return out, nil
	}
	query := `SELECT project, machine, cwd, COALESCE(file_path, '')
		FROM sessions`
	args := make([]any, 0, len(labels))
	placeholders := make([]string, 0, len(labels))
	for _, label := range labels {
		placeholders = append(placeholders, "?")
		args = append(args, label)
	}
	query += ` WHERE deleted_at IS NULL
		AND project IN (` + strings.Join(placeholders, ",") + `)
		ORDER BY project, machine, cwd, file_path`

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing legacy project identity sessions: %w", err)
	}
	defer rows.Close()

	type legacyProjectIdentitySession struct {
		project  string
		machine  string
		cwd      string
		filePath string
	}
	sessions := []legacyProjectIdentitySession{}
	for rows.Next() {
		var session legacyProjectIdentitySession
		if err := rows.Scan(
			&session.project,
			&session.machine,
			&session.cwd,
			&session.filePath,
		); err != nil {
			return nil, fmt.Errorf("scanning legacy project identity session: %w", err)
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating legacy project identity sessions: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("closing legacy project identity sessions: %w", err)
	}

	mappingsByMachine := map[string][]WorktreeProjectMapping{}
	for _, session := range sessions {
		identity, ok, err := db.legacyProjectIdentityForSession(
			ctx, mappingsByMachine,
			session.project,
			session.machine,
			session.cwd,
			session.filePath,
		)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if out[session.project] == nil {
			out[session.project] = map[string]export.ProjectIdentity{}
		}
		out[session.project][identity.Key] = identity
	}
	return out, nil
}

func (db *DB) legacyProjectIdentityForSession(
	ctx context.Context,
	mappingsByMachine map[string][]WorktreeProjectMapping,
	project, machine, cwd, filePath string,
) (export.ProjectIdentity, bool, error) {
	mappings, ok := mappingsByMachine[machine]
	if !ok {
		var err error
		mappings, err = db.ListActiveWorktreeProjectMappings(ctx, machine)
		if err != nil {
			return export.ProjectIdentity{}, false,
				fmt.Errorf("listing worktree mappings for project identity: %w", err)
		}
		mappingsByMachine[machine] = mappings
	}
	for _, root := range legacyProjectIdentityRoots(cwd, filePath) {
		for _, mapping := range mappings {
			if mapping.Project != project || !worktreePathMatches(mapping.PathPrefix, root) {
				continue
			}
			identity := export.BuildStoredProjectIdentity(
				export.ProjectIdentityInput{
					RootPath:         mapping.PathPrefix,
					WorktreeRootPath: mapping.PathPrefix,
				},
			)
			if identity.Key != "" {
				return identity, true, nil
			}
		}
	}
	for _, root := range legacyProjectIdentityRoots(cwd, filePath) {
		if !safeDBLocalAbsolutePath(root) {
			continue
		}
		if gitRoot, remotes := discoverLegacyLocalGitIdentity(root); gitRoot != "" {
			input := export.ProjectIdentityInput{RootPath: gitRoot}
			if _, raw, ok := export.SelectRemote(remotes); ok {
				input.GitRemote = raw
			} else {
				input.WorktreeRootPath = gitRoot
			}
			identity := export.BuildStoredProjectIdentity(input)
			if identity.Key != "" {
				return identity, true, nil
			}
		}
		identity := export.BuildStoredProjectIdentity(
			export.ProjectIdentityInput{RootPath: root},
		)
		if identity.Key != "" {
			return identity, true, nil
		}
	}
	return export.ProjectIdentity{}, false, nil
}

func legacyProjectIdentityRoots(cwd, filePath string) []string {
	var roots []string
	for _, candidate := range []string{cwd, filepath.Dir(filePath)} {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" || candidate == "." {
			continue
		}
		if !safeDBLocalAbsolutePath(candidate) {
			continue
		}
		if !slicesContainsString(roots, candidate) {
			roots = append(roots, candidate)
		}
	}
	sort.Strings(roots)
	return roots
}

func discoverLegacyLocalGitIdentity(root string) (string, map[string]string) {
	if !filepath.IsAbs(root) {
		return "", nil
	}
	// Skip macOS automounter namespaces: probing them wakes
	// automountd/opendirectoryd for paths that virtually never exist
	// locally (see export.IsAutomountNamespacePath).
	if export.IsAutomountNamespacePath(runtime.GOOS, filepath.Clean(root)) {
		return "", nil
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(root))
	if err != nil {
		return "", nil
	}
	gitRoot := findLegacyLocalGitRoot(resolved)
	if gitRoot == "" {
		return "", nil
	}
	config := legacyGitConfigPath(gitRoot)
	if config == "" {
		return gitRoot, nil
	}
	return gitRoot, readLegacyGitRemotes(config)
}

func findLegacyLocalGitRoot(start string) string {
	dir := filepath.Clean(start)
	for {
		if info, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			if info.IsDir() || info.Mode().IsRegular() {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func legacyGitConfigPath(root string) string {
	gitPath := filepath.Join(root, ".git")
	if info, err := os.Stat(gitPath); err == nil && info.IsDir() {
		return filepath.Join(gitPath, "config")
	}
	data, err := os.ReadFile(gitPath)
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(data))
	line = strings.TrimPrefix(line, "gitdir:")
	line = strings.TrimSpace(line)
	if line == "" {
		return ""
	}
	if !filepath.IsAbs(line) {
		line = filepath.Join(root, line)
	}
	commonDir := line
	if data, err := os.ReadFile(filepath.Join(line, "commondir")); err == nil {
		common := strings.TrimSpace(string(data))
		if filepath.IsAbs(common) {
			commonDir = common
		} else {
			commonDir = filepath.Clean(filepath.Join(line, common))
		}
	}
	return filepath.Join(commonDir, "config")
}

func readLegacyGitRemotes(configPath string) map[string]string {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil
	}
	remotes := map[string]string{}
	var current string
	for line := range strings.SplitSeq(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			current = legacyRemoteNameFromGitConfigSection(trimmed)
			continue
		}
		if current == "" || !strings.HasPrefix(trimmed, "url") {
			continue
		}
		key, value, ok := strings.Cut(trimmed, "=")
		if !ok || strings.TrimSpace(key) != "url" {
			continue
		}
		remotes[current] = strings.TrimSpace(value)
	}
	return remotes
}

func legacyRemoteNameFromGitConfigSection(section string) string {
	section = strings.Trim(section, "[]")
	if !strings.HasPrefix(section, `remote `) {
		return ""
	}
	name := strings.TrimSpace(strings.TrimPrefix(section, `remote `))
	return strings.Trim(name, `"`)
}

func safeDBLocalAbsolutePath(p string) bool {
	_, ok := export.NormalizeStoredRootPath(p)
	return ok
}

func slicesContainsString(values []string, needle string) bool {
	return slices.Contains(values, needle)
}

func newUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating database id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(b[:])
	return fmt.Sprintf(
		"%s-%s-%s-%s-%s",
		encoded[0:8], encoded[8:12], encoded[12:16],
		encoded[16:20], encoded[20:32],
	), nil
}
