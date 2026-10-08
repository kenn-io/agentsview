package db

import (
	"encoding/json/v2"
	"slices"
	"sort"
	"strings"

	"go.kenn.io/agentsview/internal/export"
)

// SortProjectIdentityObservations orders observations by archive, project,
// machine, root path and git remote.
func SortProjectIdentityObservations(obs []export.ProjectIdentityObservation) {
	sort.SliceStable(obs, func(i, j int) bool {
		a, b := obs[i], obs[j]
		if a.SourceArchiveID != b.SourceArchiveID {
			return a.SourceArchiveID < b.SourceArchiveID
		}
		if a.Project != b.Project {
			return a.Project < b.Project
		}
		if a.Machine != b.Machine {
			return a.Machine < b.Machine
		}
		if a.RootPath != b.RootPath {
			return a.RootPath < b.RootPath
		}
		return a.GitRemote < b.GitRemote
	})
}

// ObservationIdentityScope returns the archive identity shared by the
// observations, an aggregate scope when they span archives, or the legacy
// shared-store scope when none carry an archive ID and salt.
func ObservationIdentityScope(
	observations []export.ProjectIdentityObservation,
) export.IdentityScope {
	unique := make(map[string]export.IdentityScope)
	for _, obs := range observations {
		scope := export.IdentityScope{
			ArchiveID:   strings.TrimSpace(obs.SourceArchiveID),
			ArchiveSalt: strings.TrimSpace(obs.SourceArchiveSalt),
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

// MergeProjectIdentitySnapshots overlays refresh on base by session ID and
// returns the result sorted by session ID.
func MergeProjectIdentitySnapshots(
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

// FilterIdentityScope limits full publications to the push scope, reusing
// items' backing array. LoadProjectIdentityPublicationDelta applies the scope
// in SQL instead.
func FilterIdentityScope(
	items []export.ProjectIdentityObservation, projects, excludeProjects []string,
) []export.ProjectIdentityObservation {
	if len(projects) == 0 && len(excludeProjects) == 0 {
		return items
	}
	out := items[:0]
	for _, item := range items {
		if ProjectMatchesPushScope(item.Project, projects, excludeProjects) {
			out = append(out, item)
		}
	}
	return out
}

// ProjectMatchesPushScope reports whether project passes the push's include
// and exclude lists.
func ProjectMatchesPushScope(project string, projects, excludeProjects []string) bool {
	if len(projects) > 0 && !slices.Contains(projects, project) {
		return false
	}
	return !slices.Contains(excludeProjects, project)
}

// CanonicalPushScope renders the push's project filters as a deterministic
// string for comparing scopes across pushes. An unfiltered scope returns "",
// keeping the common case free of JSON.
func CanonicalPushScope(projects, excludeProjects []string) string {
	if len(projects) == 0 && len(excludeProjects) == 0 {
		return ""
	}
	scope := struct {
		Projects []string `json:"projects,omitempty"`
		Exclude  []string `json:"exclude,omitempty"`
	}{
		Projects: SortedCopy(projects),
		Exclude:  SortedCopy(excludeProjects),
	}
	data, err := json.Marshal(scope)
	if err != nil {
		return ""
	}
	return string(data)
}

// ProjectIdentityRootKey identifies the root a fallback (empty git_remote)
// observation competes with real-remote observations over.
type ProjectIdentityRootKey struct {
	ArchiveID string
	Project   string
	Machine   string
	RootPath  string
}

// ProjectIdentityObservationPlan is the reduced form of an observation batch.
type ProjectIdentityObservationPlan struct {
	// RealRemote holds deduped observations with a git remote.
	RealRemote []export.ProjectIdentityObservation
	// Ambiguous holds deduped empty-remote observations that must coexist
	// with real-remote evidence for the same root.
	Ambiguous []export.ProjectIdentityObservation
	// Fallbacks holds deduped empty-remote observations whose root has no
	// real-remote observation in the batch. Whether each survives still
	// depends on the rows already in the store.
	Fallbacks []export.ProjectIdentityObservation
	// RealRoots lists the roots of RealRemote in first-seen order; stale
	// fallback rows for these roots must be deleted.
	RealRoots []ProjectIdentityRootKey
}

// ObservationRootKey returns the root that obs competes for.
func ObservationRootKey(
	obs export.ProjectIdentityObservation,
) ProjectIdentityRootKey {
	return ProjectIdentityRootKey{
		ArchiveID: obs.SourceArchiveID,
		Project:   obs.Project,
		Machine:   obs.Machine,
		RootPath:  obs.RootPath,
	}
}

// PlanProjectIdentityObservationSync reduces a batch to the final state of
// upserting each observation in order: the last observation per conflict key
// wins. Ordinary empty-remote fallbacks never survive alongside real-remote
// evidence for the same root, while ambiguous observations always survive
// because they are conflicting evidence rather than root-derived fallbacks.
func PlanProjectIdentityObservationSync(
	observations []export.ProjectIdentityObservation,
) ProjectIdentityObservationPlan {
	type conflictKey struct {
		root      ProjectIdentityRootKey
		gitRemote string
	}
	keyOrder := make([]conflictKey, 0, len(observations))
	latest := make(map[conflictKey]export.ProjectIdentityObservation,
		len(observations))
	realRootSet := make(map[ProjectIdentityRootKey]bool)

	var plan ProjectIdentityObservationPlan
	for _, obs := range observations {
		key := conflictKey{
			root: ObservationRootKey(obs), gitRemote: obs.GitRemote,
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
			plan.RealRoots = append(plan.RealRoots, key.root)
		}
	}
	for _, key := range keyOrder {
		obs := latest[key]
		if obs.GitRemote != "" {
			plan.RealRemote = append(plan.RealRemote, obs)
			continue
		}
		if obs.RemoteResolution == export.ProjectResolutionAmbiguous {
			plan.Ambiguous = append(plan.Ambiguous, obs)
			continue
		}
		if !realRootSet[key.root] {
			plan.Fallbacks = append(plan.Fallbacks, obs)
		}
	}
	return plan
}
