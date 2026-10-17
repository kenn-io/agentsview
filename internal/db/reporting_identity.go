package db

import (
	"context"
	"database/sql"
	"slices"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/export"
)

// Resolve once per export snapshot, in bounded queries, not once per hour.
// Aggregate label catalogs are not evidence for individual contributions.
func (db *DB) reportingSessionReferences(ctx context.Context, tx *sql.Tx, sessions []activity.SessionMeta) (map[string]export.ProjectReference, error) {
	archiveID, err := sessionExportMetadataValue(ctx, tx, archiveMetadataArchiveIDKey, ErrArchiveIDMissing, "archive id")
	if err != nil {
		return nil, err
	}
	salt, err := sessionExportMetadataValue(ctx, tx, archiveMetadataArchiveSaltKey, ErrArchiveSaltMissing, "archive salt")
	if err != nil {
		return nil, err
	}
	scope := export.IdentityScope{ArchiveID: archiveID, ArchiveSalt: salt}
	ids := make([]string, len(sessions))
	for i, session := range sessions {
		ids[i] = session.SessionID
	}
	references := make(map[string]export.ProjectReference, len(ids))
	err = queryChunked(ids, func(chunk []string) error {
		snapshots, err := db.listSessionProjectIdentitySnapshotsFrom(ctx, tx, chunk)
		if err != nil {
			return err
		}
		for id, observation := range snapshots {
			references[id] = export.ResolveProjectReferenceFromObservation(observation, scope)
		}
		return nil
	})
	return references, err
}

// Partition before aggregation so missing evidence cannot erase a known
// contribution or borrow its identity. Only export-local session copies change.
// The returned labels map each group to its session project. DisplayLabel
// cannot stand in for it: it also hides paths.
func partitionReportingProjects(sessions map[string]activity.SessionMeta, projects map[string]export.ProjectMapEntry, references map[string]export.ProjectReference) (map[string]export.ProjectMapEntry, map[string]export.ProjectReference, map[string]string) {
	groups := make(map[string]export.ProjectMapEntry)
	groupReferences := make(map[string]export.ProjectReference, len(sessions))
	groupLabels := make(map[string]string)
	for id, session := range sessions {
		labelKey := export.ProjectKeyForEntry(projects[session.Project])
		reference := references[id]
		if reference.ProjectKey != labelKey || reference.Resolution == "" ||
			reference.Resolution == export.ProjectResolutionResolved && reference.Identity == nil {
			reference = export.ProjectReference{Resolution: export.ProjectResolutionUnknown}
		}
		key := labelKey + "/" + string(reference.Resolution)
		if reference.Resolution == export.ProjectResolutionResolved {
			key += "/" + reference.Identity.Key
		}
		groups[key] = export.ProjectMapEntry{ProjectKey: key, DisplayLabel: export.SafeProjectDisplayLabel(session.Project)}
		groupLabels[key] = session.Project
		reference.ProjectKey = key
		groupReferences[id] = reference
		session.Project, session.ProjectKey = key, key
		sessions[id] = session
	}
	return groups, groupReferences, groupLabels
}

// Restore labels after the ordinary aggregator has used the opaque groups.
// A nonempty selector does not turn a projectless session into a project, so
// its groups stay out of the project breakdowns.
func restoreReportingProjectLabels(hour *export.ReportingHour, projects map[string]export.ProjectMapEntry, groupLabels map[string]string) {
	hour.Activity.ByProject = slices.DeleteFunc(hour.Activity.ByProject, func(row export.ReportingActivityProjectBreakdown) bool {
		return groupLabels[row.ProjectKey] == ""
	})
	hour.Usage.ByProject = slices.DeleteFunc(hour.Usage.ByProject, func(row export.ReportingUsageProjectBreakdown) bool {
		return groupLabels[row.ProjectKey] == ""
	})
	for i := range hour.Activity.ByProject {
		hour.Activity.ByProject[i].Project = projects[hour.Activity.ByProject[i].ProjectKey].DisplayLabel
	}
	for i := range hour.Usage.ByProject {
		hour.Usage.ByProject[i].Project = projects[hour.Usage.ByProject[i].ProjectKey].DisplayLabel
	}
	for i := range hour.Joint.Cells {
		hour.Joint.Cells[i].Project = projects[hour.Joint.Cells[i].ProjectKey].DisplayLabel
	}
	for key, entry := range hour.Joint.Projects {
		entry.DisplayLabel = projects[key].DisplayLabel
		hour.Joint.Projects[key] = entry
	}
}

func reportingProjectEvidence(contributors map[string]bool, sessions map[string]activity.SessionMeta, projects map[string]export.ProjectMapEntry, references map[string]export.ProjectReference) map[string]export.ProjectMapEntry {
	type evidence struct {
		entry                 export.ProjectMapEntry
		incomplete, ambiguous bool
	}
	states := make(map[string]*evidence)
	// The first contributor fixes the display label, which feeds the
	// digests, so visit sessions in a stable order.
	for _, id := range SortedKeys(contributors) {
		session := sessions[id]
		key := export.ProjectKeyForEntry(projects[session.Project])
		if key == "" {
			continue
		}
		state := states[key]
		if state == nil {
			state = &evidence{entry: export.ProjectMapEntry{DisplayLabel: export.SafeProjectDisplayLabel(session.Project)}}
			states[key] = state
		}
		reference := references[id]
		if reference.ProjectKey != key || reference.Resolution == export.ProjectResolutionUnknown || reference.Resolution == "" {
			state.incomplete = true
		} else if reference.Resolution == export.ProjectResolutionAmbiguous {
			state.ambiguous = true
		} else if reference.Identity == nil {
			state.incomplete = true
		} else if state.entry.Identity == nil {
			state.entry.Identity = export.ProjectCatalogIdentity(reference.Identity)
		} else if state.entry.Identity.Key != reference.Identity.Key {
			state.ambiguous = true
		} else if state.entry.Identity.RootKey != reference.Identity.RootKey {
			// A repository can span multiple worktree roots. Retain only a
			// root fact shared by every contributor, not an arbitrary session.
			state.entry.Identity.RootKey = ""
		}
	}
	out := make(map[string]export.ProjectMapEntry, len(states))
	for key, state := range states {
		switch {
		case state.ambiguous:
			state.entry.Resolution, state.entry.Identity = export.ProjectResolutionAmbiguous, nil
		case state.incomplete:
			state.entry.Resolution, state.entry.Identity = export.ProjectResolutionUnknown, nil
		default:
			state.entry.Resolution = export.ProjectResolutionResolved
		}
		out[key] = state.entry
	}
	return out
}
