package db

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"

	"go.kenn.io/agentsview/internal/export"
)

const worktreeCandidateExampleLimit = 10

// ArchiveWorktreeCandidateRequest selects a project across the whole
// archive, with no Activity date range or filter scoping.
type ArchiveWorktreeCandidateRequest struct {
	ProjectLabel string
	ProjectKey   string
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

// SelectWorktreeCandidateProjects validates that the requested display label
// identifies the requested opaque project key, then expands the selection to
// raw labels with the same resolved project identity. The display label
// disambiguates a clicked inventory row; it must not exclude historical
// aliases that display differently but resolve to the same repository.
func SelectWorktreeCandidateProjects(
	request ArchiveWorktreeCandidateRequest,
	labels map[string]struct{},
	projects map[string]export.ProjectMapEntry,
) map[string]struct{} {
	identityKeys := make(map[string]struct{})
	labelMatches := false
	for label := range labels {
		entry := projects[label]
		if entry.ProjectKey != request.ProjectKey ||
			export.SafeProjectDisplayLabel(label) != request.ProjectLabel {
			continue
		}
		labelMatches = true
		if entry.Resolution == export.ProjectResolutionResolved &&
			entry.Identity != nil &&
			strings.TrimSpace(entry.Identity.Key) != "" {
			identityKeys[entry.Identity.Key] = struct{}{}
		}
	}
	if !labelMatches {
		return map[string]struct{}{}
	}

	selected := make(map[string]struct{})
	for label := range labels {
		entry := projects[label]
		if entry.ProjectKey == request.ProjectKey {
			selected[label] = struct{}{}
			continue
		}
		if entry.Resolution != export.ProjectResolutionResolved ||
			entry.Identity == nil {
			continue
		}
		if _, ok := identityKeys[entry.Identity.Key]; ok {
			selected[label] = struct{}{}
		}
	}
	return selected
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
		if root := candidateSnapshotRoot(session); root != "" {
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

	result := make([]WorktreeReclassificationCandidate, 0, len(groups))
	for _, group := range groups {
		result = append(result, candidateFromGroup(group))
	}
	sort.Slice(result, func(i, j int) bool {
		left, right := result[i], result[j]
		if left.Machine != right.Machine {
			return left.Machine < right.Machine
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
		if group.key.kind == "fallback" {
			suggestedPrefix = group.key.fallbackCwd
		} else {
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
