// ABOUTME: Container-level freshness gate for OpenCode-family shared
// ABOUTME: SQLite databases, skipping per-session re-parse on idle syncs.
package sync

import (
	"maps"
	"path/filepath"
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
	return parser.ParseVirtualSourcePathForBase(file.Path, dbName)
}

// sqliteContainerPathForResultPath maps a processed result path back to its
// container. Result paths arrive without an agent, so every family DB name is
// tried.
func sqliteContainerPathForResultPath(path string) string {
	for _, agent := range openCodeFamilySQLiteAgents {
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
	addState := func(agent parser.AgentType, dbPath string) {
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
	if len(changedPaths) == 0 {
		for _, agent := range openCodeFamilySQLiteAgents {
			for _, dir := range e.agentDirs[agent] {
				if dir == "" || strings.HasPrefix(dir, "s3://") {
					continue
				}
				src := resolveOpenCodeFormatSource(agent, filepath.Clean(dir))
				addState(agent, src.DBPath)
			}
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
				addState(agent, openCodeContainerPathForEvent(agent, dir, path))
			}
		}
	}
	return states
}

func openCodeContainerPathForEvent(
	agent parser.AgentType,
	root string,
	path string,
) string {
	src := resolveOpenCodeFormatSource(agent, filepath.Clean(root))
	if src.DBPath == "" {
		return ""
	}
	path = filepath.Clean(path)
	if path == src.DBPath ||
		path == src.DBPath+"-wal" ||
		path == src.DBPath+"-shm" {
		return src.DBPath
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
	if e.forceParse || file.ForceParse {
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
