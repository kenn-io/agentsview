package sync

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func (e *Engine) SnapshotAntigravityTitleStates() map[string]parser.SQLiteContainerState {
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	states := make(map[string]parser.SQLiteContainerState)
	for _, root := range e.sources().agentDirs[parser.AgentAntigravity] {
		path := filepath.Join(root, parser.AntigravitySummariesDBName)
		if applied, ok := e.appliedNativeTitles[path]; ok {
			states[path] = applied.state
		}
	}
	return states
}

// Mirror snapshots change only on extraction, so persisted states can seed a delta engine.
func (e *Engine) InjectAntigravityTitleStates(states map[string]parser.SQLiteContainerState) {
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	if e.appliedNativeTitles == nil {
		e.appliedNativeTitles = make(map[string]appliedNativeTitleStore)
	}
	now := time.Now()
	for _, root := range e.sources().agentDirs[parser.AgentAntigravity] {
		path := filepath.Join(root, parser.AntigravitySummariesDBName)
		delete(e.appliedNativeTitles, path)
		if state, ok := states[path]; ok {
			e.appliedNativeTitles[path] = appliedNativeTitleStore{state: state, readAt: now}
		}
	}
}

// antigravitySessionChange is what a store row changes on one stored session:
// its title, and a working directory and parent it did not have yet.
type antigravitySessionChange struct {
	retitle bool
	name    *string
	machine string
	cwd     string
	// project is the parser-derived project for cwd, before worktree mappings.
	project  string
	parentID string
}

func (e *Engine) applyAntigravityStoreSummaries(ctx context.Context, root string, summaries map[string]parser.AntigravitySummary) (updated int, err error) {
	if len(summaries) == 0 {
		return 0, nil
	}
	prefix := e.idPrefix + string(parser.AgentAntigravity) + ":"
	ids := make([]string, 0, len(summaries))
	for id := range maps.Keys(summaries) {
		ids = append(ids, prefix+id)
	}
	slices.Sort(ids)
	stored, err := e.db.ListSessionWriteIdentitiesByID(ctx, ids)
	if err != nil {
		return 0, err
	}
	var archived map[string]db.SessionWriteIdentity
	if reader, ok := e.archiveStore.(sessionWriteIdentityReader); ok && e.deferredAntigravityChanges != nil {
		// A rebuild copies archived sessions without sources only after this pass.
		if archived, err = reader.ListSessionWriteIdentitiesByID(ctx, ids); err != nil {
			return 0, err
		}
	}
	usageOnly := e.db.ArchiveContent().UsageOnly()
	for _, id := range ids {
		session, ok := stored[id]
		deferred := false
		if !ok {
			session, deferred = archived[id]
		}
		if !ok && !deferred {
			continue
		}
		summary := summaries[strings.TrimPrefix(id, prefix)]
		change := antigravitySessionChange{machine: session.Machine}
		title := db.SanitizeUTF8(summary.Title)
		sameTitle := session.SessionName != nil && *session.SessionName == title ||
			session.SessionName == nil && title == ""
		change.retitle = summary.TitlePresent && !usageOnly && !sameTitle
		if change.retitle && title != "" {
			change.name = &title
		}
		// The store only fills what the session does not have yet.
		cwd := session.Cwd
		if cwd == "" {
			cwd = summary.Cwd
			change.cwd, change.project = summary.Cwd, summary.Project
		}
		if session.ParserParentSessionID == "" && summary.ParentConversationID != "" {
			change.parentID = prefix + summary.ParentConversationID
		}
		if !change.retitle && change.cwd == "" && change.parentID == "" {
			continue
		}
		if !e.antigravityRootOwns(root, session.FilePath) || !e.cwdFilter.allows(cwd) {
			continue
		}
		if deferred {
			e.deferredAntigravityChanges[id] = change
			continue
		}
		// Trashed sessions take the change too, so restoring one needs no store change.
		if err := e.applyAntigravitySessionChange(ctx, e.db, id, change); err != nil {
			return updated, err
		}
		updated++
	}
	return updated, nil
}

// applyAntigravitySessionChange writes change into target. A filled working
// directory takes its project through the active worktree mappings and
// records a complete project identity, as a parsed write does.
func (e *Engine) applyAntigravitySessionChange(
	ctx context.Context, target *db.DB, id string, change antigravitySessionChange,
) error {
	if change.retitle {
		if err := target.RefreshNativeSessionName(ctx, id, change.name); err != nil {
			return err
		}
	}
	fill := db.NativeSessionMetadata{Cwd: change.cwd, ParentSessionID: change.parentID}
	if change.cwd != "" && change.project != "" {
		fill.Project = change.project
		mapped, ok, err := target.ResolveWorktreeProjectMapping(ctx, change.machine, change.cwd, change.project)
		if err != nil {
			return fmt.Errorf("resolve worktree project for %s: %w", id, err)
		}
		if ok {
			fill.Project = mapped
		}
		if obs, ok := e.projectIdentityObservation(db.Session{
			ID: id, Project: fill.Project, Machine: change.machine, Cwd: change.cwd,
		}); ok {
			fill.Identity = obs
			fill.IdentitySnapshotProject = change.project
		}
	}
	return target.FillNativeSessionMetadata(ctx, id, fill)
}

// antigravityRootOwns keeps a store from renaming a same-ID session that
// belongs to another configured root.
func (e *Engine) antigravityRootOwns(root, storedPath string) bool {
	if e.pathRewriter != nil {
		if e.storedPathResolver == nil {
			return false
		}
		resolved, ok := e.storedPathResolver(storedPath)
		if !ok {
			return false
		}
		storedPath = resolved
	}
	owner, ok := parser.AntigravitySessionRoot(storedPath)
	return ok && sameReconciliationSourcePath(owner, root)
}
