// ABOUTME: Container-level freshness gate for OpenCode-family shared
// ABOUTME: SQLite databases, skipping per-session re-parse on idle syncs.
package sync

import (
	"os"
	"path/filepath"
	"slices"
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

const sqliteContainerWholeSourceID = "\x00container"

var statSQLiteContainerState = parser.StatSQLiteContainerState

// sqliteContainerSourceForFile maps a discovered file to its shared SQLite
// container path and session ID, or ok=false when the file is not one of the
// shared-SQLite sources that can gate-skip before fingerprinting.
func sqliteContainerSourceForFile(
	file parser.DiscoveredFile,
) (dbPath, sessionID string, ok bool) {
	if file.Agent == parser.AgentTrae {
		if dbPath, sessionID, ok := parser.SplitTraeVirtualPath(file.Path); ok {
			return dbPath, sessionID, true
		}
		if filepath.Base(file.Path) == parser.WindsurfStateDBName {
			return file.Path, sqliteContainerWholeSourceID, true
		}
		return "", "", false
	}
	dbName := openCodeFormatDBName(file.Agent)
	if dbName == "" {
		return "", "", false
	}
	return parser.ParseVirtualSourcePathForBase(file.Path, dbName)
}

// sqliteContainerPathForResultPath maps a processed result path back to its
// container. Result paths arrive without an agent, so every family DB name is
// tried before the Trae forms.
func sqliteContainerPathForResultPath(path string) string {
	for _, agent := range openCodeFamilySQLiteAgents {
		dbPath, _, ok := parser.ParseVirtualSourcePathForBase(
			path, openCodeFormatDBName(agent),
		)
		if ok {
			return dbPath
		}
	}
	if dbPath, _, ok := parser.SplitTraeVirtualPath(path); ok {
		return dbPath
	}
	if filepath.Base(path) == parser.WindsurfStateDBName {
		return path
	}
	return ""
}

// trustedSQLiteContainer is a container's state at the end of the last pass
// that verified every one of its discovered sessions, together with exactly
// which session IDs that pass discovered. The set matters in hybrid roots:
// discovery drops SQLite rows shadowed by a same-ID storage JSON, so the
// discoverable row set can grow — a storage JSON removed while the DB is
// untouched exposes its row — without the container state changing. A
// source may therefore gate-skip only when its session ID was part of the
// verified set; a newly exposed row misses the set and parses.
type trustedSQLiteContainer struct {
	state    sqliteContainerGateState
	sessions map[string]struct{}
}

type sqliteContainerGateState struct {
	sqlite           parser.SQLiteContainerState
	manifestSize     int64
	manifestMtimeSec int64
}

// sqliteContainerPass tracks one sync pass's view of every OpenCode-family
// SQLite container it discovered. captured and sessions are written once
// before workers start and are read-only afterwards; completed and failed
// are touched only by the single collectAndBatch goroutine, so no locking
// is needed during the pass.
type sqliteContainerPass struct {
	captured   map[string]sqliteContainerGateState
	discovered map[string]int
	sessions   map[string]map[string]struct{}
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
) map[string]sqliteContainerGateState {
	if e.forceParse {
		return nil
	}
	states := make(map[string]sqliteContainerGateState)
	addState := func(agent parser.AgentType, dbPath string) {
		if dbPath == "" {
			return
		}
		if _, seen := states[dbPath]; seen {
			return
		}
		state, ok := sqliteContainerGateStateForAgent(agent, dbPath)
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
		for _, dir := range e.agentDirs[parser.AgentTrae] {
			if dir == "" || strings.HasPrefix(dir, "s3://") {
				continue
			}
			for _, dbPath := range traeContainerPaths(filepath.Clean(dir)) {
				addState(parser.AgentTrae, dbPath)
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
		for _, dir := range e.agentDirs[parser.AgentTrae] {
			if dir == "" || strings.HasPrefix(dir, "s3://") {
				continue
			}
			if dbPath, ok := parser.TraeDBPathForEvent(
				filepath.Clean(dir), path,
			); ok {
				addState(parser.AgentTrae, dbPath)
			}
		}
	}
	return states
}

func sqliteContainerGateStateForAgent(
	agent parser.AgentType,
	dbPath string,
) (sqliteContainerGateState, bool) {
	sqliteState, ok := statSQLiteContainerState(dbPath)
	if !ok {
		return sqliteContainerGateState{}, false
	}
	state := sqliteContainerGateState{sqlite: sqliteState}
	if agent != parser.AgentTrae {
		return state, true
	}
	manifestPath := filepath.Join(filepath.Dir(dbPath), "workspace.json")
	info, err := os.Stat(manifestPath)
	if err == nil && !info.IsDir() {
		state.manifestSize = info.Size()
		state.manifestMtimeSec = info.ModTime().Unix()
	}
	return state, true
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

func providerChangedPathStoredHintRoots(
	agent parser.AgentType,
	watchRoot string,
	path string,
) []string {
	watchRoot = filepath.Clean(watchRoot)
	if agent != parser.AgentTrae {
		return []string{watchRoot}
	}
	root := filepath.Dir(watchRoot)
	dbPath, ok := parser.TraeDBPathForEvent(root, path)
	if !ok {
		return []string{watchRoot}
	}
	return []string{dbPath}
}

func uniqueContainerPaths(paths []string) []string {
	if len(paths) < 2 {
		return paths
	}
	seen := make(map[string]struct{}, len(paths))
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		out = append(out, path)
	}
	slices.Sort(out)
	return out
}

func traeContainerPaths(root string) []string {
	var paths []string
	global := filepath.Join(root, "globalStorage", parser.WindsurfStateDBName)
	if info, err := os.Stat(global); err == nil && !info.IsDir() {
		paths = append(paths, global)
	}
	workspace := filepath.Join(root, "workspaceStorage")
	if entries, err := os.ReadDir(workspace); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			path := filepath.Join(
				workspace, entry.Name(), parser.WindsurfStateDBName,
			)
			if info, err := os.Stat(path); err == nil && !info.IsDir() {
				paths = append(paths, path)
			}
		}
	}
	return uniqueContainerPaths(paths)
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
	preStates map[string]sqliteContainerGateState,
) {
	if e.forceParse {
		e.containerMu.Lock()
		e.containerPass = nil
		e.containerMu.Unlock()
		return
	}
	var pass *sqliteContainerPass
	for _, file := range files {
		dbPath, sessionID, ok := sqliteContainerSourceForFile(file)
		if !ok {
			continue
		}
		if pass == nil {
			pass = &sqliteContainerPass{
				captured:   make(map[string]sqliteContainerGateState),
				discovered: make(map[string]int),
				sessions:   make(map[string]map[string]struct{}),
				completed:  make(map[string]int),
				failed:     make(map[string]bool),
			}
		}
		pass.discovered[dbPath]++
		if pass.sessions[dbPath] == nil {
			pass.sessions[dbPath] = make(map[string]struct{})
		}
		pass.sessions[dbPath][sessionID] = struct{}{}
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
			if post, ok := sqliteContainerGateStateForAgent(
				sqliteContainerAgentForPath(dbPath), dbPath,
			); ok &&
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
	defer e.containerMu.Unlock()
	if e.containerPass == nil {
		return false
	}
	current, ok := e.containerPass.captured[dbPath]
	if !ok {
		return false
	}
	trusted, ok := e.trustedSQLiteContainers[dbPath]
	if !ok || current != trusted.state {
		return false
	}
	_, verified := trusted.sessions[sessionID]
	return verified
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

func sqliteContainerAgentForPath(dbPath string) parser.AgentType {
	if filepath.Base(dbPath) == parser.WindsurfStateDBName {
		return parser.AgentTrae
	}
	for _, agent := range openCodeFamilySQLiteAgents {
		if filepath.Base(dbPath) == openCodeFormatDBName(agent) {
			return agent
		}
	}
	return ""
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
//
// fullDiscovery marks passes whose discovery covered every configured
// root (full syncs, as opposed to changed-path or scoped-root passes).
// Such a pass is authoritative for which rows are discoverable, so a
// trusted container it discovered no sources for — fully shadowed by
// storage JSONs, or gone — loses its trusted entry: the entry's session
// set is no longer being maintained, and stale membership would otherwise
// gate-skip a row re-exposed later by a storage removal that leaves the
// DB untouched.
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
		if pass.completed[dbPath] != pass.discovered[dbPath] {
			continue
		}
		if e.trustedSQLiteContainers == nil {
			e.trustedSQLiteContainers =
				make(map[string]trustedSQLiteContainer)
		}
		e.trustedSQLiteContainers[dbPath] = trustedSQLiteContainer{
			state:    state,
			sessions: pass.sessions[dbPath],
		}
	}
}

// dropTrustedSQLiteContainerSessionForStorage removes a processed storage
// session's ID from its root's trusted container membership. From the
// moment a file-backed storage session is processed, the archive's
// canonical copy for that ID is the storage one, so a same-ID SQLite row —
// even under an unchanged container state — no longer matches what its
// membership verified. Without this, a storage JSON that arrives and
// disappears entirely between full passes (both legs via watcher
// changed-path syncs, which never re-promote containers) would leave the
// stale membership in place, and the re-exposed row would gate-skip
// forever while the archive kept the interim storage copy. The next fully
// verified pass re-adds the ID once the row is actually re-verified.
func (e *Engine) dropTrustedSQLiteContainerSessionForStorage(
	agent parser.AgentType, sessionPath string,
) {
	// sessionPath is root/storage/<sessionSubdir>/<project>/<id>.json,
	// mirroring the root derivation in StatOpenCodeStorageSessionState.
	sessionID := strings.TrimSuffix(filepath.Base(sessionPath), ".json")
	if sessionID == "" {
		return
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(sessionPath))))
	src := resolveOpenCodeFormatSource(agent, root)
	if src.DBPath == "" {
		return
	}
	e.containerMu.Lock()
	defer e.containerMu.Unlock()
	if trusted, ok := e.trustedSQLiteContainers[src.DBPath]; ok {
		delete(trusted.sessions, sessionID)
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
