// ABOUTME: Container-level freshness gate for OpenCode-family shared
// ABOUTME: SQLite databases, skipping per-session re-parse on idle syncs.
package sync

import (
	"context"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// The OpenCode-family providers fan one shared SQLite database into one
// virtual source per session row. Per-session freshness cannot be decided
// before parsing (a message or part row can change without bumping the
// session's time_updated, which is why dropUnchangedSharedSQLiteResults
// compares content fingerprints after the parse), so a periodic sync of an
// untouched archive used to re-open and re-parse every session on every
// pass. The gate restores an O(1) answer for the common idle case: when the
// container file provably has not changed since a pass that verified every
// one of its sessions — as decided by parser.SQLiteContainerState, which
// rests on SQLite's own write markers rather than timestamp precision —
// none of its sessions can have changed either, and they all skip before
// fingerprinting.

// openCodeFamilySQLiteAgents lists the agents whose sessions live in a
// shared OpenCode-format SQLite container.
var openCodeFamilySQLiteAgents = []parser.AgentType{
	parser.AgentOpenCode,
	parser.AgentKilo,
	parser.AgentMiMoCode,
	parser.AgentIcodemate,
}

var statSQLiteContainerState = parser.StatSQLiteContainerState

var (
	openCodeContainerDigestVerifyInterval = 5 * time.Minute
	openCodeContainerDigestVerifyNow      = time.Now
)

// sqliteContainerSourceForFile maps a discovered file to its shared SQLite
// container path and session ID, or ok=false when the file is not one of the
// shared-SQLite sources that can gate-skip before fingerprinting.
func sqliteContainerSourceForFile(
	file parser.DiscoveredFile,
) (dbPath, sessionID string, ok bool) {
	dbName := openCodeFormatDBName(file.Agent)
	if dbName == "" {
		return "", "", false
	}
	if file.Agent == parser.AgentOpenCode {
		return parser.ParseOpenCodeSQLiteVirtualPath(file.Path)
	}
	return parser.ParseVirtualSourcePathForBase(file.Path, dbName)
}

// sqliteContainerPathForResultPath maps a processed result path back to its
// container. Result paths arrive without an agent, so every family DB name is
// tried.
func sqliteContainerPathForResultPath(path string) string {
	if dbPath, _, ok := parser.ParseOpenCodeSQLiteVirtualPath(path); ok {
		return dbPath
	}
	for _, agent := range openCodeFamilySQLiteAgents {
		if agent == parser.AgentOpenCode {
			continue
		}
		dbPath, _, ok := parser.ParseVirtualSourcePathForBase(
			path, openCodeFormatDBName(agent),
		)
		if ok {
			return dbPath
		}
	}
	return ""
}

// trustedSQLiteContainer is a container's state at the end of the last pass
// that verified every one of its discovered sessions. Per-session membership
// is checked against the persistent archive's canonical source path instead
// of retaining an archive-sized Go set: a newly unshadowed SQLite row still
// has its storage JSON path in the archive and therefore cannot gate-skip.
type trustedSQLiteContainer struct {
	state parser.SQLiteContainerState
}

// sqliteContainerPass tracks one sync pass's view of every OpenCode-family
// SQLite container it discovered. captured and sessions are written once
// before workers start and are read-only afterwards; completed and failed
// are touched only by the single collectAndBatch goroutine, so no locking
// is needed during the pass.
type sqliteContainerPass struct {
	captured         map[string]parser.SQLiteContainerState
	discovered       map[string]int
	completed        map[string]int
	failed           map[string]bool
	fullDigestListed map[string]bool
	poisoned         bool
}

// captureSQLiteContainerStates snapshots every configured OpenCode-family
// SQLite container's state. It must run BEFORE discovery lists any session
// rows: promotion may only trust a state that is at least as old as the
// discovered session set, otherwise a session written between the listing
// and a later capture would be promoted away and gate-skipped without ever
// being parsed. Containers whose state cannot be read are simply absent
// from the map and never promoted.
func (e *Engine) captureSQLiteContainerStates(
	changedPaths []string,
) map[string]parser.SQLiteContainerState {
	if e.forceParse {
		return nil
	}
	states := make(map[string]parser.SQLiteContainerState)
	if len(changedPaths) == 0 {
		for _, agent := range openCodeFamilySQLiteAgents {
			e.captureAgentSQLiteContainerStates(agent, nil, states)
		}
		return states
	}
	for _, rawPath := range changedPaths {
		path := filepath.Clean(rawPath)
		for _, agent := range openCodeFamilySQLiteAgents {
			for _, dir := range e.agentDirs[agent] {
				if dir == "" || strings.HasPrefix(dir, "s3://") {
					continue
				}
				addSQLiteContainerState(
					states, openCodeContainerPathForEvent(agent, dir, path),
				)
			}
		}
	}
	return states
}

// capturePlannedSQLiteContainerStates scopes the pre-discovery capture to
// what the resolved reconciliation plans can discover. A pass whose plans
// name no OpenCode-family provider streams no shared containers, so probing
// every configured container there would repeat once per provider group in a
// grouped poll; an in-family plan probes only its own containers that
// overlap its scopes' traversal roots, keeping capture work bounded by the
// batch rather than the agent's full configuration. A full-coverage pass
// still captures every configured container.
func (e *Engine) capturePlannedSQLiteContainerStates(
	plans []providerReconciliationPlan, fullCoverage bool,
) map[string]parser.SQLiteContainerState {
	if fullCoverage {
		return e.captureSQLiteContainerStates(nil)
	}
	if e.forceParse {
		return nil
	}
	states := make(map[string]parser.SQLiteContainerState)
	for _, plan := range plans {
		if plan.err != nil ||
			!slices.Contains(openCodeFamilySQLiteAgents, plan.agent) {
			continue
		}
		var roots []string
		for _, scope := range plan.plan.Scopes {
			roots = append(roots, scope.TraversalRoots...)
		}
		if len(roots) == 0 {
			continue
		}
		e.captureAgentSQLiteContainerStates(plan.agent, roots, states)
	}
	return states
}

// captureAgentSQLiteContainerStates captures one agent's containers. Non-nil
// roots restrict the capture to configured dirs overlapping them; nil roots
// capture every configured dir (full and changed-path passes).
func (e *Engine) captureAgentSQLiteContainerStates(
	agent parser.AgentType,
	roots []string,
	states map[string]parser.SQLiteContainerState,
) {
	for _, dir := range e.agentDirs[agent] {
		if dir == "" || strings.HasPrefix(dir, "s3://") {
			continue
		}
		if !containerDirOverlapsRoots(dir, roots) {
			continue
		}
		src := resolveOpenCodeFormatSource(agent, filepath.Clean(dir))
		for _, dbPath := range src.DBPaths {
			addSQLiteContainerState(states, dbPath)
		}
	}
}

// containerDirOverlapsRoots mirrors logicalRootsForAgentWatchRoots's
// bidirectional overlap so the capture covers exactly the configured dirs a
// scoped pass can expand into: a dir is capturable when it is the same path
// as, an ancestor of, or a descendant of any reconciliation root. Empty
// roots match everything.
func containerDirOverlapsRoots(dir string, roots []string) bool {
	if len(roots) == 0 {
		return true
	}
	cleanedDir := cleanRootPath(dir)
	return slices.ContainsFunc(roots, func(root string) bool {
		cleanedRoot := cleanRootPath(root)
		return samePathOrDescendant(cleanedRoot, cleanedDir) ||
			samePathOrDescendant(cleanedDir, cleanedRoot)
	})
}

func addSQLiteContainerState(
	states map[string]parser.SQLiteContainerState, dbPath string,
) {
	if dbPath == "" {
		return
	}
	if _, seen := states[dbPath]; seen {
		return
	}
	state, ok := statSQLiteContainerState(dbPath)
	if !ok {
		return
	}
	states[dbPath] = state
}

// openCodeContainerPathForChangedPathEvent maps a changed-path event to the
// shared SQLite container it names for one OpenCode-family agent, or ""
// when the agent has no container or the event is not a container write.
func openCodeContainerPathForChangedPathEvent(
	agent parser.AgentType,
	roots []string,
	path string,
) string {
	if openCodeFormatDBName(agent) == "" {
		return ""
	}
	for _, dir := range roots {
		if dir == "" || strings.HasPrefix(dir, "s3://") {
			continue
		}
		if container := openCodeContainerPathForEvent(agent, dir, path); container != "" {
			return container
		}
	}
	return ""
}

// storedMemberFreshnessPager pages stored freshness for one shared container
// in ascending virtual-path order, translating each folded row into the
// coverage authority the provider's changed-path merge consumes: a listed
// member whose carried session-row watermark is at or below the row's
// covered-through watermark is provably unchanged and is omitted from the
// listing, so a one-session write flows one candidate into the sync pipeline
// while peak memory stays one page — never the container's full membership.
//
// The covered-through watermark is the session/project metadata watermark
// recovered from the stored child digest (storedSessionRowWatermarkNS),
// keeping the comparison per-session and like-for-like: a session or project
// row that advances past its own stored metadata watermark is always kept,
// wherever other sessions' watermarks or its own child timestamps sit. Rows
// behind the current data version are not emitted at all — a version rewrite
// must keep the source — and sessions with no stored row are kept by the
// merge's absent-row rule.
//
// Known, deliberate deferral (not a detection gap to "fix" here): a
// child-only write that leaves the session and project rows untouched is
// invisible to the session-row watermark wherever its timestamps land —
// above or below the stored composite alike. Detecting it per event would
// require reading child rows, which is exactly the archive-sized work this
// path exists to avoid. Such writes reconcile on the next full-discovery
// pass, whose digest still catches them (the write itself broke container
// trust, so that pass carries the full digest); actively watched sessions
// bypass this path entirely via the per-session composite poll. The
// contract is documented in docs/internal/session-format-sources.md and
// pinned by TestOpenCodeWatcherPassDefersChildOnlyEditToFullDiscovery.
func (e *Engine) storedMemberFreshnessPager(
	container string,
) parser.StoredMemberFreshnessPager {
	current := db.CurrentDataVersion()
	return func(
		ctx context.Context, afterPath string, limit int,
	) ([]parser.StoredMemberFreshness, bool, error) {
		var rows []parser.StoredMemberFreshness
		// Withheld rows shrink the emitted page below the raw page, so keep
		// reading raw pages — advancing by the raw cursor, not the emitted
		// one — until something is vouchable or the container is exhausted.
		// Returning an empty page with done=false instead would read as
		// exhaustion to the merge cursor, silently un-covering every stored
		// member past the first all-stale page.
		for {
			page, done, err := e.db.ListVirtualContainerMemberFreshnessPage(
				ctx, container, afterPath, limit,
			)
			if err != nil {
				return nil, false, err
			}
			for _, row := range page {
				if row.DataVersion < current {
					continue
				}
				rows = append(rows, parser.StoredMemberFreshness{
					Path: row.Path,
					CoveredThroughNS: storedSessionRowWatermarkNS(
						row.VirtualContainerMemberFreshness,
					),
				})
			}
			if done || len(rows) > 0 {
				return rows, done, nil
			}
			if len(page) == 0 {
				// The page contract returns done for an empty page; treat a
				// violation as exhaustion rather than spinning on it.
				return rows, true, nil
			}
			afterPath = page[len(page)-1].Path
		}
	}
}

// storedSessionRowWatermarkNS resolves the stored value a carried session-row
// watermark is compared against, like-for-like: the session/project metadata
// watermark recovered from the stored child digest. Comparing against the
// stored composite MTimeNS instead would over-skip — a composite dominated by
// a newer child timestamp would hide a metadata update (title, directory,
// worktree rename) whose stamp lands below it. Rows without a parseable
// digest (pre-digest fingerprints, future digest versions) fall back to the
// composite, the conservative pre-digest behavior that self-heals on the
// row's next reparse.
func storedSessionRowWatermarkNS(
	member db.VirtualContainerMemberFreshness,
) int64 {
	if metadata, ok := parser.OpenCodeChildDigestMetadataWatermarkNS(
		member.Hash,
	); ok {
		return metadata
	}
	return member.MTimeNS
}

// sqliteContainerListsWatermarkOnly returns discovery's bounded listing policy.
// A recently digest-verified container may use its complete-membership
// watermark listing after a write; due verification, missing state, and
// replacement containers fall back to the full composite listing.
func (e *Engine) sqliteContainerListsWatermarkOnly(
	preStates map[string]parser.SQLiteContainerState,
) func(string) bool {
	if len(preStates) == 0 || e.forceParse || e.forceFullParse {
		return nil
	}
	return func(dbPath string) bool {
		dbPath = filepath.Clean(dbPath)
		state, ok := preStates[dbPath]
		if !ok {
			return false
		}
		e.containerMu.Lock()
		trusted, ok := e.trustedSQLiteContainers[dbPath]
		verifiedAt, verified := e.digestVerifiedAt[dbPath]
		e.containerMu.Unlock()
		if !ok || sqliteContainerStateReplaced(trusted.state, state) {
			return false
		}
		return verified && openCodeContainerDigestVerificationCurrent(
			verifiedAt, openCodeContainerDigestVerifyNow(),
		)
	}
}

func openCodeContainerDigestVerificationCurrent(verifiedAt, now time.Time) bool {
	return !verifiedAt.IsZero() && now.Sub(verifiedAt) >= 0 &&
		now.Sub(verifiedAt) < openCodeContainerDigestVerifyInterval
}

// filterQuickSyncFiles applies the quick-sync mtime cutoff, exempting the
// sessions of containers whose digest verification has lapsed. A lapsed
// container's due pass already paid the full digest listing, and the stamp
// can only refresh when every discovered session completes; filtering its
// old-composite sessions would discard the listing's findings and leave the
// container due again on every subsequent quick sync. The exemption costs at
// most one full-gated pass over the container per verification interval.
func (e *Engine) filterQuickSyncFiles(
	ctx context.Context,
	files []parser.DiscoveredFile,
	cutoff time.Time,
) []parser.DiscoveredFile {
	due := e.lapsedDigestVerificationContainers(files)
	if len(due) == 0 {
		return e.filterFilesByMtime(ctx, files, cutoff)
	}
	kept := make([]parser.DiscoveredFile, 0, len(files))
	filterable := make([]parser.DiscoveredFile, 0, len(files))
	for _, f := range files {
		if dbPath, _, ok := sqliteContainerSourceForFile(f); ok && due[dbPath] {
			kept = append(kept, f)
			continue
		}
		filterable = append(filterable, f)
	}
	return append(kept, e.filterFilesByMtime(ctx, filterable, cutoff)...)
}

// lapsedDigestVerificationContainers returns the containers among the
// discovered files whose verification stamp exists but has aged past the
// interval, and whose current pass carried the full digest listing. The
// listing requirement keeps the exemption consistent with what discovery
// produced: a pass that crossed the interval boundary after listing
// watermark-only has no digest findings to process and could not refresh
// the stamp, so its sessions stay behind the cutoff until the next pass
// lists the digest form. A container with no stamp stays subject to the
// cutoff: a fresh engine has no verification window to restore, and
// exempting it would turn every quick sync into container-sized work.
func (e *Engine) lapsedDigestVerificationContainers(
	files []parser.DiscoveredFile,
) map[string]bool {
	var due map[string]bool
	now := openCodeContainerDigestVerifyNow()
	e.containerMu.Lock()
	defer e.containerMu.Unlock()
	pass := e.containerPass
	if pass == nil {
		return nil
	}
	for _, f := range files {
		dbPath, _, ok := sqliteContainerSourceForFile(f)
		if !ok {
			continue
		}
		if !pass.fullDigestListed[dbPath] {
			continue
		}
		verifiedAt, stamped := e.digestVerifiedAt[dbPath]
		if !stamped ||
			openCodeContainerDigestVerificationCurrent(verifiedAt, now) {
			continue
		}
		if due == nil {
			due = make(map[string]bool)
		}
		due[dbPath] = true
	}
	return due
}

func sqliteContainerStateReplaced(
	previous, current parser.SQLiteContainerState,
) bool {
	// Without both identity components, a replacement cannot be
	// distinguished from an in-place transaction. Fail closed rather than
	// allowing a stale verification timestamp to authorize watermark-only
	// discovery on platforms whose path stat has no stable identity.
	if previous.DBInode == 0 || previous.DBDevice == 0 ||
		current.DBInode == 0 || current.DBDevice == 0 {
		return true
	}
	if previous.DBInode != current.DBInode ||
		previous.DBDevice != current.DBDevice {
		return true
	}
	// A rollback of SQLite's transaction counter is evidence of an in-place
	// restore. Normal committed writes advance it, so retain the fast path for
	// ordinary in-place changes while rejecting stale verification after a
	// restore that preserved the file identity.
	return current.DBChangeCounter < previous.DBChangeCounter
}

func openCodeContainerPathForEvent(
	agent parser.AgentType,
	root string,
	path string,
) string {
	path = filepath.Clean(path)
	for _, dbPath := range resolveOpenCodeFormatSource(agent, filepath.Clean(root)).DBPaths {
		if path == dbPath || path == dbPath+"-wal" || path == dbPath+"-shm" {
			return dbPath
		}
	}
	return ""
}

// beginSQLiteContainerPass starts a pass's gate bookkeeping from the
// discovered files and the pre-discovery container captures. files must be
// the pre-filter discovery set: promotion requires seeing a completion for
// every discovered session, so an mtime-cutoff or scope filter that drops
// sessions from processing keeps the container untrusted. A discovered
// container with no pre-discovery capture is marked failed and can neither
// gate-skip nor be promoted this pass.
//
// It runs AFTER discovery, so each captured container is re-stat'ed here
// and compared against its pre-discovery capture. A mismatch means the
// container changed inside the capture-discovery window: the discovered
// session set may already include that change, so gating against the
// pre-discovery state would skip it while it still matches the trusted
// state. Such containers are failed for the pass — no skips, no promotion
// — and the next pass re-verifies them by content.
func (e *Engine) beginSQLiteContainerPass(
	files []parser.DiscoveredFile,
	preStates map[string]parser.SQLiteContainerState,
) {
	if e.forceParse {
		e.containerMu.Lock()
		e.containerPass = nil
		e.containerMu.Unlock()
		return
	}
	e.beginStreamingSQLiteContainerPass(preStates)
	for _, file := range files {
		e.noteSQLiteContainerDiscovery(file)
	}
	e.finishStreamingSQLiteContainerDiscovery()
}

func (e *Engine) beginStreamingSQLiteContainerPass(
	preStates map[string]parser.SQLiteContainerState,
) {
	if e.forceParse {
		e.containerMu.Lock()
		e.containerPass = nil
		e.containerMu.Unlock()
		return
	}
	pass := &sqliteContainerPass{
		captured:         make(map[string]parser.SQLiteContainerState, len(preStates)),
		discovered:       make(map[string]int),
		completed:        make(map[string]int),
		failed:           make(map[string]bool),
		fullDigestListed: make(map[string]bool),
	}
	maps.Copy(pass.captured, preStates)
	e.containerMu.Lock()
	e.containerPass = pass
	e.containerMu.Unlock()
}

func (e *Engine) noteSQLiteContainerDiscovery(file parser.DiscoveredFile) {
	dbPath, _, ok := sqliteContainerSourceForFile(file)
	if !ok {
		return
	}
	e.containerMu.Lock()
	defer e.containerMu.Unlock()
	pass := e.containerPass
	if pass == nil {
		return
	}
	pass.discovered[dbPath]++
	if file.ProviderSource != nil &&
		parser.SourceUsesOpenCodeCompositeMTime(*file.ProviderSource) {
		pass.fullDigestListed[dbPath] = true
	}
	if _, captured := pass.captured[dbPath]; !captured {
		pass.failed[dbPath] = true
	}
}

func (e *Engine) unNoteSQLiteContainerDiscovery(file parser.DiscoveredFile) {
	dbPath, _, ok := sqliteContainerSourceForFile(file)
	if !ok {
		return
	}
	e.containerMu.Lock()
	defer e.containerMu.Unlock()
	pass := e.containerPass
	if pass == nil || pass.discovered[dbPath] == 0 {
		return
	}
	pass.discovered[dbPath]--
}

func (e *Engine) finishStreamingSQLiteContainerDiscovery() {
	e.containerMu.Lock()
	defer e.containerMu.Unlock()
	pass := e.containerPass
	if pass != nil {
		for dbPath, pre := range pass.captured {
			if post, ok := statSQLiteContainerState(dbPath); ok &&
				post == pre {
				continue
			}
			pass.failed[dbPath] = true
		}
	}
}

// sqliteContainerSourceFresh reports whether a discovered file belongs to a
// container whose current state matches the last fully verified state AND
// whose session ID was part of that verified pass, in which case the
// session is unchanged and skips before fingerprinting. The membership
// check covers hybrid roots, where the discoverable row set can grow (a
// removed storage JSON stops shadowing its same-ID row) without the
// container state changing; such a row was never verified and must parse.
func (e *Engine) sqliteContainerSourceFresh(file parser.DiscoveredFile) bool {
	if e.forceParseRequested(file) {
		return false
	}
	dbPath, sessionID, ok := sqliteContainerSourceForFile(file)
	if !ok {
		return false
	}
	e.containerMu.Lock()
	pass := e.containerPass
	if pass == nil {
		e.containerMu.Unlock()
		return false
	}
	current, ok := pass.captured[dbPath]
	if !ok || pass.failed[dbPath] {
		e.containerMu.Unlock()
		return false
	}
	// A full digest listing is the authoritative verification for this pass.
	// It must reach the per-session fingerprint path even when a prior
	// watermark pass promoted the same container state to trusted; otherwise
	// child-only edits remain hidden behind the container-level skip.
	if pass.fullDigestListed[dbPath] {
		e.containerMu.Unlock()
		return false
	}
	trusted, ok := e.trustedSQLiteContainers[dbPath]
	e.containerMu.Unlock()
	if !ok || current != trusted.state {
		return false
	}
	fullID := applyIDPrefixToID(e.idPrefix, string(file.Agent)+":"+sessionID)
	return e.db.GetSessionDataVersion(fullID) >= db.CurrentDataVersion() &&
		e.db.GetSessionFilePath(fullID) == e.effectiveSourcePath(file.Path)
}

// watermarkOnlySQLiteSourceFresh reports whether a shared-container session
// whose source carries only the session-row watermark is already covered by
// its stored session/project metadata watermark, compared like-for-like:
// the stored value is recovered from the persisted child digest, falling
// back to the stored composite MTimeNS for rows without a parseable digest.
// A session-row watermark at or below the stored metadata watermark proves
// the session and project rows did not advance, so the parse is skipped
// without resolving the child digest. What the watermark cannot see — any
// child-only write that leaves the session and project rows untouched — is
// deliberately deferred to the next full-discovery pass, whose carried
// digest still catches it (see storedMemberFreshnessPager for the full
// contract). That keeps per-event work bounded by the changed batch instead
// of the archive.
func (e *Engine) watermarkOnlySQLiteSourceFresh(
	source parser.SourceRef,
	file parser.DiscoveredFile,
) (int64, bool) {
	if e.forceParseRequested(file) {
		return 0, false
	}
	watermark, ok := parser.SourceWatermarkOnlyMTimeNS(source)
	if !ok {
		return 0, false
	}
	// The skip is only sound while the pass's container capture is valid. A
	// trusted full discovery lists watermark-only sources; if the container
	// changes between that listing and the pass's recapture check, the
	// capture is invalidated and a concurrent child-only write may hide
	// beneath an unchanged metadata watermark — those sources must fall
	// through to Fingerprint and resolve the full digest instead.
	if dbPath, _, ok := sqliteContainerSourceForFile(file); !ok ||
		!e.sqliteContainerPassCaptureValid(dbPath) {
		return 0, false
	}
	lookupPath := providerDiscoveredPath(source)
	if lookupPath == "" {
		return 0, false
	}
	if e.pathRewriter != nil {
		lookupPath = e.pathRewriter(lookupPath)
	}
	_, storedMtime, found := e.db.GetFileInfoByPath(lookupPath)
	if !found {
		return 0, false
	}
	limit := storedMtime
	if hash, ok := e.db.GetFileHashByPath(lookupPath); ok {
		if metadata, parsed := parser.OpenCodeChildDigestMetadataWatermarkNS(
			hash,
		); parsed {
			limit = metadata
		}
	}
	if limit < watermark {
		return 0, false
	}
	if e.db.GetDataVersionByPath(lookupPath) < db.CurrentDataVersion() {
		return 0, false
	}
	return storedMtime, true
}

// sqliteContainerPassCaptureValid reports whether the current pass still
// holds a live capture for the container: one was taken before discovery,
// the post-discovery recapture matched it, and no processing failure has
// poisoned the container since. Watermark-only skips require this — an
// invalidated capture means the container changed while the pass was
// listing it, and the watermark cannot see what that change touched.
func (e *Engine) sqliteContainerPassCaptureValid(dbPath string) bool {
	e.containerMu.Lock()
	defer e.containerMu.Unlock()
	pass := e.containerPass
	if pass == nil || pass.failed[dbPath] {
		return false
	}
	_, ok := pass.captured[dbPath]
	return ok
}

// sqliteContainerPassCaptureStillCurrent rechecks the pre-discovery capture at
// a reconciliation source-state boundary. Page refresh happens before source
// resolution, so this second check closes the window in which a container can
// change after refresh but before its carried digest is applied.
func (e *Engine) sqliteContainerPassCaptureStillCurrent(dbPath string) bool {
	e.containerMu.Lock()
	pass := e.containerPass
	if pass == nil || pass.failed[dbPath] {
		e.containerMu.Unlock()
		return false
	}
	before, ok := pass.captured[dbPath]
	e.containerMu.Unlock()
	if !ok {
		return false
	}
	after, ok := statSQLiteContainerState(dbPath)
	if ok && after == before {
		return true
	}
	e.containerMu.Lock()
	if e.containerPass == pass {
		pass.failed[dbPath] = true
	}
	e.containerMu.Unlock()
	return false
}

// noteSQLiteContainerResult records a processed file's outcome for
// promotion bookkeeping. Skips count as completions: a skipped session was
// either gate-skipped against an already-trusted state or individually
// verified fresh.
func (e *Engine) noteSQLiteContainerResult(path string, ok bool) {
	e.containerMu.Lock()
	defer e.containerMu.Unlock()
	pass := e.containerPass
	if pass == nil {
		return
	}
	dbPath := sqliteContainerPathForResultPath(path)
	if dbPath == "" {
		return
	}
	if ok {
		pass.completed[dbPath]++
	} else {
		pass.failed[dbPath] = true
	}
}

// poisonSQLiteContainerPass blocks every promotion for the current pass.
// Used when a batched DB write fails, because batch failures cannot be
// attributed to individual sessions.
// poisonSQLiteContainerPass marks the active pass invalidated by a failure
// that cannot be attributed to one container; finalization then clears
// every captured verification instead of promoting.
func (e *Engine) poisonSQLiteContainerPass() {
	e.containerMu.Lock()
	defer e.containerMu.Unlock()
	if e.containerPass != nil {
		e.containerPass.poisoned = true
	}
}

// finishSQLiteContainerPass promotes the pass's captured container states
// to trusted for every container whose discovered sessions all completed
// without errors, retries, or write failures. Promotion requires at least
// one discovered session: scoped passes capture every configured container
// (captureSQLiteContainerStates(nil)) but discover only in-scope sources,
// so an out-of-scope container ends the pass at completed == discovered ==
// 0 having verified nothing — trusting its freshly captured state would
// gate-skip changes that were never parsed. incomplete marks passes that
// must never promote (changed-path subsets, whose discovery covers only
// the changed sessions).
//
// digestVerifiedAt clears only on evidence against a container: an entry
// in pass.failed (a failed session, or a capture that changed under the
// pass), or a poisoned pass, whose failure cannot be attributed to a
// container and so invalidates every captured verification. A clean pass
// that merely did not cover a container keeps its verification age: the
// timestamp is written only by the full-digest promotion below, so a
// preserved timestamp can never extend child-only-edit staleness past the
// bounded verification window, while clearing it forces a full composite
// child scan the next discovery would otherwise skip.
//
// fullDiscovery marks passes whose discovery covered every configured
// root (full syncs, as opposed to changed-path or scoped-root passes).
// Such a pass is authoritative for which rows are discoverable, so a
// trusted container it discovered no sources for — fully shadowed by
// storage JSONs, or gone — loses its trusted entry. Per-session archive-path
// checks protect newly re-exposed rows; removing the unused container trust
// here also keeps the compact state map aligned with current discovery.
func (e *Engine) finishSQLiteContainerPass(incomplete, fullDiscovery bool) {
	e.containerMu.Lock()
	pass := e.containerPass
	if incomplete || pass == nil || pass.poisoned {
		e.containerPass = nil
		if pass != nil {
			if pass.poisoned {
				e.clearDigestVerificationForPass(pass)
			} else {
				for dbPath := range pass.failed {
					delete(e.digestVerifiedAt, dbPath)
				}
			}
		}
		e.containerMu.Unlock()
		return
	}
	e.containerMu.Unlock()

	digestFailures := e.sqliteContainerDigestRevalidationFailures(pass)

	e.containerMu.Lock()
	defer e.containerMu.Unlock()
	if e.containerPass != pass {
		return
	}
	e.containerPass = nil
	if pass.poisoned {
		e.clearDigestVerificationForPass(pass)
		return
	}
	if fullDiscovery {
		for dbPath := range e.trustedSQLiteContainers {
			if pass.discovered[dbPath] == 0 {
				delete(e.trustedSQLiteContainers, dbPath)
				delete(e.digestVerifiedAt, dbPath)
			}
		}
	}
	for dbPath := range digestFailures {
		pass.failed[dbPath] = true
	}
	for dbPath := range pass.failed {
		delete(e.digestVerifiedAt, dbPath)
	}
	for dbPath, state := range pass.captured {
		if pass.failed[dbPath] {
			continue
		}
		if pass.discovered[dbPath] == 0 ||
			pass.completed[dbPath] != pass.discovered[dbPath] {
			// Out of scope, cutoff-filtered, or deferred: promotion is
			// unearned, but nothing observed the container changing.
			continue
		}
		if e.trustedSQLiteContainers == nil {
			e.trustedSQLiteContainers =
				make(map[string]trustedSQLiteContainer)
		}
		e.trustedSQLiteContainers[dbPath] = trustedSQLiteContainer{
			state: state,
		}
		if pass.fullDigestListed[dbPath] {
			if e.digestVerifiedAt == nil {
				e.digestVerifiedAt = make(map[string]time.Time)
			}
			e.digestVerifiedAt[dbPath] = openCodeContainerDigestVerifyNow()
		}
	}
}

// sqliteContainerDigestRevalidationFailures rechecks full-digest containers
// without holding containerMu during filesystem I/O.
func (e *Engine) sqliteContainerDigestRevalidationFailures(
	pass *sqliteContainerPass,
) map[string]struct{} {
	e.containerMu.Lock()
	captures := make(map[string]parser.SQLiteContainerState,
		len(pass.fullDigestListed))
	failures := make(map[string]struct{})
	for dbPath := range pass.fullDigestListed {
		state, ok := pass.captured[dbPath]
		if !ok {
			failures[dbPath] = struct{}{}
			continue
		}
		captures[dbPath] = state
	}
	e.containerMu.Unlock()

	for dbPath, captured := range captures {
		current, ok := statSQLiteContainerState(dbPath)
		if !ok || current != captured {
			failures[dbPath] = struct{}{}
		}
	}
	return failures
}

// clearDigestVerificationForPass clears verification age for every container
// whose capture or discovery was invalid during the pass. Failed discoveries
// may have no captured entry, so both maps are part of the invalidation set.
func (e *Engine) clearDigestVerificationForPass(pass *sqliteContainerPass) {
	for dbPath := range pass.captured {
		delete(e.digestVerifiedAt, dbPath)
	}
	for dbPath := range pass.failed {
		delete(e.digestVerifiedAt, dbPath)
	}
}

// clearTrustedSQLiteContainers drops every trusted container state. Called
// by resync, which rebuilds the archive from scratch and must re-verify
// every session against it.
func (e *Engine) clearTrustedSQLiteContainers() {
	e.containerMu.Lock()
	defer e.containerMu.Unlock()
	e.trustedSQLiteContainers = nil
	e.digestVerifiedAt = nil
	e.containerPass = nil
}
