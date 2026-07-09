// ABOUTME: Container-level freshness gate for OpenCode-family shared
// ABOUTME: SQLite databases, skipping per-session re-parse on idle syncs.
package sync

import (
	"path/filepath"
	"strings"

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

// sqliteContainerPathForFile maps a discovered file to its shared SQLite
// container path, or "" when the file is not an OpenCode-family SQLite
// virtual source (storage-mode JSON sessions included).
func sqliteContainerPathForFile(file parser.DiscoveredFile) string {
	dbName := openCodeFormatDBName(file.Agent)
	if dbName == "" {
		return ""
	}
	dbPath, _, ok := parser.ParseVirtualSourcePathForBase(file.Path, dbName)
	if !ok {
		return ""
	}
	return dbPath
}

// sqliteContainerPathForResultPath maps a processed result path back to its
// container. Result paths arrive without an agent, so every family DB name
// is tried.
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

// sqliteContainerPass tracks one sync pass's view of every OpenCode-family
// SQLite container it discovered. captured is written once before workers
// start and is read-only afterwards; completed and failed are touched only
// by the single collectAndBatch goroutine, so no locking is needed during
// the pass.
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
func (e *Engine) captureSQLiteContainerStates() map[string]parser.SQLiteContainerState {
	if e.forceParse {
		return nil
	}
	states := make(map[string]parser.SQLiteContainerState)
	for _, agent := range openCodeFamilySQLiteAgents {
		for _, dir := range e.agentDirs[agent] {
			if dir == "" || strings.HasPrefix(dir, "s3://") {
				continue
			}
			src := resolveOpenCodeFormatSource(agent, filepath.Clean(dir))
			if src.DBPath == "" {
				continue
			}
			if state, ok := parser.StatSQLiteContainerState(src.DBPath); ok {
				states[src.DBPath] = state
			}
		}
	}
	return states
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
	var pass *sqliteContainerPass
	for _, file := range files {
		dbPath := sqliteContainerPathForFile(file)
		if dbPath == "" {
			continue
		}
		if pass == nil {
			pass = &sqliteContainerPass{
				captured:   make(map[string]parser.SQLiteContainerState),
				discovered: make(map[string]int),
				completed:  make(map[string]int),
				failed:     make(map[string]bool),
			}
		}
		pass.discovered[dbPath]++
		if _, seen := pass.captured[dbPath]; seen || pass.failed[dbPath] {
			continue
		}
		if state, ok := preStates[dbPath]; ok {
			pass.captured[dbPath] = state
		} else {
			pass.failed[dbPath] = true
		}
	}
	if pass != nil {
		for dbPath, pre := range pass.captured {
			if post, ok := parser.StatSQLiteContainerState(dbPath); ok &&
				post == pre {
				continue
			}
			delete(pass.captured, dbPath)
			pass.failed[dbPath] = true
		}
	}
	e.containerMu.Lock()
	e.containerPass = pass
	e.containerMu.Unlock()
}

// sqliteContainerSourceFresh reports whether a discovered file belongs to a
// container whose current state matches the last fully verified state, in
// which case the session is unchanged and skips before fingerprinting.
func (e *Engine) sqliteContainerSourceFresh(file parser.DiscoveredFile) bool {
	if e.forceParse || file.ForceParse {
		return false
	}
	dbPath := sqliteContainerPathForFile(file)
	if dbPath == "" {
		return false
	}
	e.containerMu.Lock()
	defer e.containerMu.Unlock()
	if e.containerPass == nil {
		return false
	}
	current, ok := e.containerPass.captured[dbPath]
	if !ok {
		return false
	}
	trusted, ok := e.trustedSQLiteContainers[dbPath]
	return ok && current == trusted
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
// without errors, retries, or write failures. incomplete marks passes that
// must never promote (aborted, cancelled, or discovery failures whose
// provider cannot be attributed).
func (e *Engine) finishSQLiteContainerPass(incomplete bool) {
	e.containerMu.Lock()
	defer e.containerMu.Unlock()
	pass := e.containerPass
	e.containerPass = nil
	if pass == nil || pass.poisoned || incomplete {
		return
	}
	for dbPath, state := range pass.captured {
		if pass.failed[dbPath] {
			continue
		}
		if pass.completed[dbPath] != pass.discovered[dbPath] {
			continue
		}
		if e.trustedSQLiteContainers == nil {
			e.trustedSQLiteContainers =
				make(map[string]parser.SQLiteContainerState)
		}
		e.trustedSQLiteContainers[dbPath] = state
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
