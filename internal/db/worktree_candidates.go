package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"go.kenn.io/agentsview/internal/export"
)

const worktreeCandidateExampleLimit = 10

// ArchiveWorktreeCandidateRequest selects a project with an optional Data
// date range, independently of the Activity page's filters.
type ArchiveWorktreeCandidateRequest struct {
	ProjectLabel string
	ProjectKey   string
	ProjectDateFilter
}

type WorktreeCandidateExample struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
}

type WorktreeReclassificationCandidate struct {
	ID                   string                     `json:"id"`
	Machine              string                     `json:"machine"`
	SuggestedPrefix      string                     `json:"suggested_prefix"`
	EvidenceKind         string                     `json:"evidence_kind"`
	EvidenceRoot         string                     `json:"evidence_root,omitempty"`
	ContributingSessions int                        `json:"contributing_sessions"`
	DistinctCwds         int                        `json:"distinct_cwds"`
	Available            bool                       `json:"available"`
	Examples             []WorktreeCandidateExample `json:"examples"`
}

// WorktreeCandidateSession is one session row feeding the shared candidate
// grouping pipeline (BuildWorktreeCandidates): the fields it needs to decide
// evidence kind and group membership, regardless of which backend loaded it.
type WorktreeCandidateSession struct {
	ID, Project, Machine, Cwd string
	Snapshot                  export.ProjectIdentityObservation
	HasSnapshot               bool
}

type worktreeCandidateGroupKey struct {
	machine, kind, root, fallbackCwd string
}

type worktreeCandidateGroup struct {
	key      worktreeCandidateGroupKey
	sessions []WorktreeCandidateSession
}

// ListArchiveWorktreeCandidates returns the machine/path groups for a
// project selected by (display label, project key) across every visible
// session in the archive that falls within the optional Data date range.
func (db *DB) ListArchiveWorktreeCandidates(
	ctx context.Context,
	request ArchiveWorktreeCandidateRequest,
) ([]WorktreeReclassificationCandidate, error) {
	if strings.TrimSpace(request.ProjectKey) == "" {
		return nil, fmt.Errorf("project_key is required")
	}
	sessions, err := db.archiveWorktreeCandidateSessions(ctx, request.ProjectDateFilter)
	if err != nil {
		return nil, err
	}
	labels := make(map[string]struct{})
	for _, session := range sessions {
		labels[session.project] = struct{}{}
	}
	projects, err := db.BuildProjectIdentityMap(ctx, sortedSetKeys(labels))
	if err != nil {
		return nil, err
	}
	selectedProjects := SelectWorktreeCandidateProjects(
		request, labels, projects,
	)
	if len(selectedProjects) == 0 {
		return []WorktreeReclassificationCandidate{}, nil
	}

	selectedIDs := make([]string, 0, len(sessions))
	for _, session := range sessions {
		if _, ok := selectedProjects[session.project]; !ok {
			continue
		}
		selectedIDs = append(selectedIDs, session.id)
	}
	return db.worktreeCandidatesFromSelection(ctx, selectedIDs, selectedProjects)
}

// SelectWorktreeCandidateProjects validates that the requested display label
// identifies the requested opaque project key, then selects every raw label
// represented by that exact inventory project. Distinct project keys can
// resolve to the same repository while still representing different inventory
// rows and session sets, so repository identity must not broaden the selection.
func SelectWorktreeCandidateProjects(
	request ArchiveWorktreeCandidateRequest,
	labels map[string]struct{},
	projects map[string]export.ProjectMapEntry,
) map[string]struct{} {
	labelMatches := false
	for label := range labels {
		entry := projects[label]
		if entry.ProjectKey != request.ProjectKey ||
			export.SafeProjectDisplayLabel(label) != request.ProjectLabel {
			continue
		}
		labelMatches = true
	}
	if !labelMatches {
		return map[string]struct{}{}
	}

	selected := make(map[string]struct{})
	for label := range labels {
		entry := projects[label]
		if entry.ProjectKey == request.ProjectKey {
			selected[label] = struct{}{}
		}
	}
	return selected
}

// archiveCandidateSessionRef is the minimal (id, project) pair the
// archive-wide selection query needs; the shared grouping pipeline only
// ever reads a session's ID and project label from the selection step.
type archiveCandidateSessionRef struct {
	id, project string
}

// archiveWorktreeCandidateSessions returns every archive-wide visible
// session (deleted_at IS NULL) within the optional Data date range, without
// excluding child or empty sessions.
// Data inventory counts these same rows, including zero-message sessions,
// so the selected project's session count and its folder groups stay
// reconcilable.
func (db *DB) archiveWorktreeCandidateSessions(
	ctx context.Context,
	filter ProjectDateFilter,
) ([]archiveCandidateSessionRef, error) {
	where, args := BuildSessionBaseFilterSQL(filter.SessionFilter(), SQLiteQueryDialect())
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT id, project
		FROM sessions
		WHERE `+where+`
		ORDER BY id`, args...)
	if err != nil {
		return nil, fmt.Errorf("querying archive worktree candidate sessions: %w", err)
	}
	defer rows.Close()
	var sessions []archiveCandidateSessionRef
	for rows.Next() {
		var session archiveCandidateSessionRef
		if err := rows.Scan(&session.id, &session.project); err != nil {
			return nil, fmt.Errorf("scanning archive worktree candidate session: %w", err)
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating archive worktree candidate sessions: %w", err)
	}
	return sessions, nil
}

// worktreeCandidatesFromSelection runs the shared grouping pipeline
// (snapshot/aggregate/fallback evidence, then deterministic ordering)
// over an already-selected set of session IDs.
// ListArchiveWorktreeCandidates calls it after selecting the archive-wide
// session set.
func (db *DB) worktreeCandidatesFromSelection(
	ctx context.Context,
	selectedIDs []string,
	selectedProjects map[string]struct{},
) ([]WorktreeReclassificationCandidate, error) {
	details, err := db.loadWorktreeCandidateSessions(ctx, selectedIDs)
	if err != nil {
		return nil, err
	}
	observations, err := db.ListProjectIdentityObservations(
		ctx, sortedSetKeys(selectedProjects))
	if err != nil {
		return nil, err
	}
	return BuildWorktreeCandidates(details, observations), nil
}

// BuildWorktreeCandidates groups an already-selected set of sessions into
// machine/path worktree reclassification candidates, using snapshot
// evidence first, then compatible aggregate observation evidence, then an
// exact-cwd fallback. It is the single implementation of the grouping
// pipeline shared by every backend (SQLite, PostgreSQL, DuckDB): each
// backend loads its own WorktreeCandidateSession rows and
// export.ProjectIdentityObservation list, then calls this function.
func BuildWorktreeCandidates(
	sessions []WorktreeCandidateSession,
	observations []export.ProjectIdentityObservation,
) []WorktreeReclassificationCandidate {
	groups := make(map[worktreeCandidateGroupKey]*worktreeCandidateGroup)
	for _, session := range sessions {
		key := worktreeCandidateGroupKey{machine: session.Machine}
		if prefix := observedWorktreePrefix(session.Cwd); prefix != "" {
			key.kind, key.root = "worktree", prefix
		} else if root := candidateSnapshotRoot(session); root != "" &&
			worktreePathMatches(root, normalizedMappingPath(session.Cwd)) {
			key.kind, key.root = "snapshot", root
		} else if root := compatibleAggregateRoot(session, observations); root != "" {
			key.kind, key.root = "aggregate", root
		} else if cwd := normalizedMappingPath(session.Cwd); cwd != "" {
			key.kind, key.fallbackCwd = "fallback", cwd
		} else {
			key.kind = "unavailable"
		}
		group := groups[key]
		if group == nil {
			group = &worktreeCandidateGroup{key: key}
			groups[key] = group
		}
		group.sessions = append(group.sessions, session)
	}
	groups = collapseObservedParents(groups)

	result := make([]WorktreeReclassificationCandidate, 0, len(groups))
	for _, group := range groups {
		result = append(result, candidateFromGroup(group))
	}
	sort.Slice(result, func(i, j int) bool {
		left, right := result[i], result[j]
		if left.Machine != right.Machine {
			return left.Machine < right.Machine
		}
		if left.Available != right.Available {
			return left.Available
		}
		if left.ContributingSessions != right.ContributingSessions {
			return left.ContributingSessions > right.ContributingSessions
		}
		if candidateKindOrder(left.EvidenceKind) != candidateKindOrder(right.EvidenceKind) {
			return candidateKindOrder(left.EvidenceKind) < candidateKindOrder(right.EvidenceKind)
		}
		if left.SuggestedPrefix != right.SuggestedPrefix {
			return left.SuggestedPrefix < right.SuggestedPrefix
		}
		return left.ID < right.ID
	})
	return result
}

// observedWorktreePrefix only truncates a recorded cwd. It never constructs
// a sibling checkout or guesses a repository from a similar folder name.
func observedWorktreePrefix(cwd string) string {
	cwd = normalizedMappingPath(cwd)
	for _, marker := range []string{"/.claude/worktrees/", "/.worktrees/"} {
		if before, _, ok := strings.Cut(cwd, marker); ok {
			return before + strings.TrimSuffix(marker, "/")
		}
	}
	for _, marker := range []string{"/.t3/worktrees/", "/.superset/worktrees/", "/conductor/workspaces/", "/.roborev/ci-worktrees/"} {
		if before, after, ok := strings.Cut(cwd, marker); ok {
			repo, _, _ := strings.Cut(after, "/")
			return before + marker + repo
		}
	}
	parts := strings.Split(cwd, "/")
	for i, part := range parts {
		if strings.HasSuffix(part, ".worktrees") && part != ".worktrees" {
			return strings.Join(parts[:i+1], "/")
		}
	}
	return ""
}

// Collapse sibling observations once, at their nearest shared parent. Do not
// repeatedly climb toward a home directory or drive root, and do not broaden
// a known worktree container. Each group retains its actual session examples.
func collapseObservedParents(
	groups map[worktreeCandidateGroupKey]*worktreeCandidateGroup,
) map[worktreeCandidateGroupKey]*worktreeCandidateGroup {
	parents := make(map[worktreeCandidateGroupKey][]*worktreeCandidateGroup)
	for _, group := range groups {
		if group.key.kind == "worktree" || group.key.kind == "unavailable" {
			continue
		}
		candidate := candidateFromGroup(group)
		if !candidate.Available {
			continue
		}
		parent := path.Dir(candidate.SuggestedPrefix)
		if strings.HasPrefix(candidate.SuggestedPrefix, "//") {
			parent = "/" + parent // path.Dir cleans the UNC double slash.
		}
		if parent == "." || isFilesystemRootMappingPath(parent) ||
			(len(parent) == 2 && parent[1] == ':') {
			continue
		}
		key := worktreeCandidateGroupKey{machine: group.key.machine, kind: "parent", root: parent}
		parents[key] = append(parents[key], group)
	}
	for key, children := range parents {
		paths := make(map[string]struct{})
		for _, child := range children {
			paths[candidateFromGroup(child).SuggestedPrefix] = struct{}{}
		}
		if len(paths) < 2 {
			continue
		}
		merged := &worktreeCandidateGroup{key: key}
		for _, child := range children {
			merged.sessions = append(merged.sessions, child.sessions...)
			delete(groups, child.key)
		}
		groups[key] = merged
	}
	return groups
}

func (db *DB) loadWorktreeCandidateSessions(
	ctx context.Context,
	ids []string,
) ([]WorktreeCandidateSession, error) {
	byID := make(map[string]WorktreeCandidateSession, len(ids))
	err := queryChunked(ids, func(chunk []string) error {
		placeholders, args := inPlaceholders(chunk)
		rows, err := db.getReader().QueryContext(ctx, `
			SELECT s.id, s.project, s.machine, s.cwd,
				COALESCE(snap.session_id, ''), COALESCE(snap.project, ''),
				COALESCE(snap.machine, ''), COALESCE(snap.root_path, ''),
				COALESCE(snap.worktree_root_path, ''), COALESCE(snap.key_source, '')
			FROM sessions s
			LEFT JOIN session_project_identity_snapshots snap
			  ON snap.session_id = s.id
			WHERE s.id IN `+placeholders+` AND s.deleted_at IS NULL
			ORDER BY s.id`, args...)
		if err != nil {
			return fmt.Errorf("querying worktree candidate sessions: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var session WorktreeCandidateSession
			var snapshotSessionID string
			if err := rows.Scan(
				&session.ID, &session.Project, &session.Machine, &session.Cwd,
				&snapshotSessionID, &session.Snapshot.Project,
				&session.Snapshot.Machine, &session.Snapshot.RootPath,
				&session.Snapshot.WorktreeRootPath, &session.Snapshot.KeySource,
			); err != nil {
				return fmt.Errorf("scanning worktree candidate session: %w", err)
			}
			session.HasSnapshot = snapshotSessionID != ""
			byID[session.ID] = session
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterating worktree candidate sessions: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	result := make([]WorktreeCandidateSession, 0, len(byID))
	for _, id := range ids {
		if session, ok := byID[id]; ok {
			result = append(result, session)
		}
	}
	return result, nil
}

func candidateSnapshotRoot(session WorktreeCandidateSession) string {
	if !session.HasSnapshot || session.Snapshot.Project != session.Project ||
		session.Snapshot.Machine != session.Machine {
		return ""
	}
	if root := normalizedMappingPath(session.Snapshot.WorktreeRootPath); root != "" {
		return root
	}
	// The schema's insert trigger records cwd as a placeholder root before
	// repository inspection. Treat root_path as identity evidence only after
	// inspection supplied a key source; otherwise compatible aggregate
	// evidence and the explicit exact-cwd fallback must remain reachable.
	if strings.TrimSpace(session.Snapshot.KeySource) == "" {
		return ""
	}
	return normalizedMappingPath(session.Snapshot.RootPath)
}

func compatibleAggregateRoot(
	session WorktreeCandidateSession,
	observations []export.ProjectIdentityObservation,
) string {
	cwd := normalizedMappingPath(session.Cwd)
	if cwd == "" {
		return ""
	}
	best := ""
	for _, observation := range observations {
		if observation.Project != session.Project || observation.Machine != session.Machine {
			continue
		}
		root := normalizedMappingPath(observation.WorktreeRootPath)
		if root == "" {
			root = normalizedMappingPath(observation.RootPath)
		}
		if root != "" && worktreePathMatches(root, cwd) && len(root) > len(best) {
			best = root
		}
	}
	return best
}

func candidateFromGroup(group *worktreeCandidateGroup) WorktreeReclassificationCandidate {
	sort.Slice(group.sessions, func(i, j int) bool {
		return group.sessions[i].ID < group.sessions[j].ID
	})
	cwds := make(map[string]struct{})
	paths := make([]string, 0, len(group.sessions))
	for _, session := range group.sessions {
		cwd := normalizedMappingPath(session.Cwd)
		if cwd == "" {
			continue
		}
		if _, exists := cwds[cwd]; !exists {
			cwds[cwd] = struct{}{}
			paths = append(paths, cwd)
		}
	}
	suggestedPrefix := ""
	if len(paths) > 0 {
		switch group.key.kind {
		case "worktree", "parent":
			suggestedPrefix = group.key.root
		case "fallback":
			suggestedPrefix = group.key.fallbackCwd
		default:
			suggestedPrefix = longestCommonDirectoryPrefix(paths)
		}
	}
	exampleLimit := min(len(group.sessions), worktreeCandidateExampleLimit)
	examples := make([]WorktreeCandidateExample, 0, exampleLimit)
	for _, session := range group.sessions[:exampleLimit] {
		examples = append(examples, WorktreeCandidateExample{
			SessionID: session.ID, Cwd: normalizedMappingPath(session.Cwd),
		})
	}
	available := suggestedPrefix != "" &&
		!isFilesystemRootMappingPath(suggestedPrefix)
	return WorktreeReclassificationCandidate{
		ID: candidateGroupID(group.key), Machine: group.key.machine,
		SuggestedPrefix: suggestedPrefix, EvidenceKind: group.key.kind,
		EvidenceRoot: group.key.root, ContributingSessions: len(group.sessions),
		DistinctCwds: len(cwds), Available: available, Examples: examples,
	}
}

// isFilesystemRootMappingPath identifies prefixes that are technically valid
// mapping rules but far too broad to suggest automatically. Users can still
// create a deliberate root rule in Rules; Data's observed-folder workflow
// must not turn an unclassified cwd such as "/" into a machine-wide mapping.
func isFilesystemRootMappingPath(value string) bool {
	value = normalizedMappingPath(value)
	if value == "/" {
		return true
	}
	if len(value) == 3 && value[1] == ':' && value[2] == '/' {
		return true
	}
	if !strings.HasPrefix(value, "//") {
		return false
	}
	parts := strings.Split(strings.Trim(value, "/"), "/")
	return len(parts) == 2 && parts[0] != "" && parts[1] != ""
}

func longestCommonDirectoryPrefix(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	prefix := normalizedMappingPath(paths[0])
	for _, value := range paths[1:] {
		value = normalizedMappingPath(value)
		for prefix != "" && !worktreePathMatches(prefix, value) {
			index := strings.LastIndex(prefix, "/")
			if index == 0 {
				prefix = "/"
				if worktreePathMatches(prefix, value) {
					break
				}
				prefix = ""
				break
			}
			if index < 0 {
				prefix = ""
				break
			}
			prefix = prefix[:index]
		}
	}
	return prefix
}

func candidateGroupID(key worktreeCandidateGroupKey) string {
	hash := sha256.New()
	for _, field := range []string{key.machine, key.kind, key.root, key.fallbackCwd} {
		_, _ = hash.Write([]byte(strconv.Itoa(len(field))))
		_, _ = hash.Write([]byte{':'})
		_, _ = hash.Write([]byte(field))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func candidateKindOrder(kind string) int {
	switch kind {
	case "snapshot":
		return 0
	case "aggregate":
		return 1
	case "fallback":
		return 2
	default:
		return 3
	}
}
