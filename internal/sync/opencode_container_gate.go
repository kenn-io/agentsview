// ABOUTME: Container-level freshness gate for OpenCode-family shared
// ABOUTME: SQLite databases, skipping per-session re-parse on idle syncs.
package sync

import (
	"context"
	"maps"
	"path/filepath"
	"slices"
	"strings"

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
	captured   map[string]parser.SQLiteContainerState
	discovered map[string]int
	completed  map[string]int
	failed     map[string]bool
	poisoned   bool
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

// sqliteContainerTrustedForDiscovery returns discovery's trust probe: it
// reports containers whose pre-discovery capture matches the last fully
// verified state, meaning every member will gate-skip before fingerprinting
// and the full child digest would be computed for nothing. The probe is
// keyed to the pass's own pre-discovery captures so a container that
// changes between capture and listing can never look trusted with a newer
// session set (the gate separately fails such containers for the pass).
// Nil when nothing was captured or every parse is forced.
func (e *Engine) sqliteContainerTrustedForDiscovery(
	preStates map[string]parser.SQLiteContainerState,
) func(string) bool {
	if len(preStates) == 0 || e.forceParse {
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
		e.containerMu.Unlock()
		return ok && trusted.state == state
	}
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
		captured:   make(map[string]parser.SQLiteContainerState, len(preStates)),
		discovered: make(map[string]int),
		completed:  make(map[string]int),
		failed:     make(map[string]bool),
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
	if _, captured := pass.captured[dbPath]; !captured {
		pass.failed[dbPath] = true
	}
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
			delete(pass.captured, dbPath)
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
	if e.containerPass == nil {
		e.containerMu.Unlock()
		return false
	}
	current, ok := e.containerPass.captured[dbPath]
	if !ok {
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
// must never promote (aborted, cancelled, or discovery failures whose
// provider cannot be attributed).
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
	defer e.containerMu.Unlock()
	pass := e.containerPass
	e.containerPass = nil
	if incomplete {
		return
	}
	if fullDiscovery {
		for dbPath := range e.trustedSQLiteContainers {
			if pass == nil || pass.discovered[dbPath] == 0 {
				delete(e.trustedSQLiteContainers, dbPath)
			}
		}
	}
	if pass == nil || pass.poisoned {
		return
	}
	for dbPath, state := range pass.captured {
		if pass.failed[dbPath] {
			continue
		}
		if pass.discovered[dbPath] == 0 ||
			pass.completed[dbPath] != pass.discovered[dbPath] {
			continue
		}
		if e.trustedSQLiteContainers == nil {
			e.trustedSQLiteContainers =
				make(map[string]trustedSQLiteContainer)
		}
		e.trustedSQLiteContainers[dbPath] = trustedSQLiteContainer{
			state: state,
		}
	}
}

// clearTrustedSQLiteContainers drops every trusted container state. Called
// by resync, which rebuilds the archive from scratch and must re-verify
// every session against it.
func (e *Engine) clearTrustedSQLiteContainers() {
	e.containerMu.Lock()
	defer e.containerMu.Unlock()
	e.trustedSQLiteContainers = nil
	e.containerPass = nil
}
