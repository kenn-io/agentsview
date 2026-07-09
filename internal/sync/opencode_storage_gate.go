// ABOUTME: Per-session freshness gate for OpenCode-family file-backed
// ABOUTME: storage sessions, skipping re-parse when the tree is unchanged.
package sync

import (
	"path/filepath"
	"strings"

	"go.kenn.io/agentsview/internal/parser"
)

// File-backed OpenCode-format sessions fan one session JSON plus its
// message and part files into a single source whose parse re-reads the
// whole tree. They are deliberately excluded from the mtime-keyed skip
// cache (see shouldCacheSkip): the cache keys on a composite max-mtime,
// which cannot see a child rewritten within one filesystem mtime granule
// or with a restored timestamp. The consequence was that every full sync
// and unwatched-root poll re-read and re-parsed every storage session of
// an idle archive, and every watcher event on a streaming session
// re-parsed its entire history.
//
// The gate restores a cheap answer for the unchanged case using
// parser.StatOpenCodeStorageSessionState, a per-file (name, size, mtimeNS)
// signature over exactly the files the parse would read. A session is
// skipped before fingerprinting only when its current signature equals one
// captured before a parse whose outcome the archive then absorbed: either
// every parsed result was dropped as already stored, or the results were
// confirmed fully written by the batch write path. The remaining blind
// spot — an in-place child rewrite preserving both size and mtime — is
// covered on the watcher path by invalidating trust for a session whenever
// a changed-path event resolves to it, so an event-signaled change always
// re-verifies by content. Trust is in-memory only: a restart re-verifies
// every session once, mirroring the shared-SQLite container gate.

// openCodeStorageSessionPath returns the discovered file's path when it is
// a file-backed OpenCode-format storage session JSON under a storage-mode
// root of its agent, or "" otherwise (SQLite containers and virtual rows
// included). The root-mode walk mirrors shouldCacheSkip, which excludes
// these same paths from the mtime skip cache.
func (e *Engine) openCodeStorageSessionPath(file parser.DiscoveredFile) string {
	if !isOpenCodeFormatStorageAgent(file.Agent) {
		return ""
	}
	if filepath.Base(file.Path) == openCodeFormatDBName(file.Agent) {
		return ""
	}
	if isOpenCodeFormatSQLiteVirtualPath(file.Agent, file.Path) {
		return ""
	}
	for _, dir := range e.agentDirs[file.Agent] {
		if dir == "" {
			continue
		}
		src := resolveOpenCodeFormatSource(file.Agent, dir)
		if src.Mode != parser.OpenCodeSourceStorage {
			continue
		}
		rel, ok := isUnder(dir, file.Path)
		if !ok {
			continue
		}
		rel = filepath.ToSlash(rel)
		sessionPrefix := "storage/" + filepath.Base(src.SessionRoot) + "/"
		if strings.HasPrefix(rel, sessionPrefix) {
			return file.Path
		}
		return ""
	}
	return ""
}

// storageTrustSnapshot marks a point in a session's invalidation history:
// the trust epoch and the session's invalidation generation at capture
// time. A promotion carries the snapshot taken before its parse; if either
// counter has moved by promotion time, an invalidation landed after the
// capture and the promotion is discarded rather than resurrecting trust
// the invalidation just revoked.
type storageTrustSnapshot struct {
	epoch uint64
	gen   uint64
}

// storageTrustSnapshotFor records the session's current invalidation
// coordinates. It must be taken before the stat signature is captured:
// an invalidation arriving after the snapshot then reliably blocks the
// promotion, even when the change it signals is invisible to the stat
// signature (a same-size, same-mtime child rewrite).
func (e *Engine) storageTrustSnapshotFor(path string) storageTrustSnapshot {
	e.storageTrustMu.Lock()
	defer e.storageTrustMu.Unlock()
	return storageTrustSnapshot{
		epoch: e.storageTrustEpoch,
		gen:   e.storageTrustGens[path],
	}
}

// openCodeStorageSessionGateState captures the session's current stat
// signature for the gate, along with the invalidation snapshot that any
// later promotion of this capture must present. It returns ok=false when
// the file is not a gateable storage session, this run force-parses, or
// the signature cannot be captured; such sessions never skip and never
// promote.
func (e *Engine) openCodeStorageSessionGateState(
	file parser.DiscoveredFile,
) (string, storageTrustSnapshot, bool) {
	if e.forceParse || file.ForceParse {
		return "", storageTrustSnapshot{}, false
	}
	sessionPath := e.openCodeStorageSessionPath(file)
	if sessionPath == "" {
		return "", storageTrustSnapshot{}, false
	}
	snap := e.storageTrustSnapshotFor(sessionPath)
	state, ok := parser.StatOpenCodeStorageSessionState(sessionPath)
	if !ok {
		return "", storageTrustSnapshot{}, false
	}
	return state, snap, true
}

// openCodeStorageSessionFresh reports whether the session's captured stat
// signature matches the last verified one, in which case its parse inputs
// are unchanged and it skips before fingerprinting.
func (e *Engine) openCodeStorageSessionFresh(path, state string) bool {
	if state == "" {
		return false
	}
	e.storageTrustMu.Lock()
	defer e.storageTrustMu.Unlock()
	trusted, ok := e.trustedStorageSessions[path]
	return ok && trusted == state
}

// promoteOpenCodeStorageSession records a stat signature that was captured
// before a parse whose outcome the archive has since absorbed (results
// dropped as already stored, or confirmed written). The
// capture-before-parse ordering makes overlapping writes safe: a write
// landing between capture and parse leaves the parsed content newer than
// the signature, so the next capture mismatches and re-verifies. The
// snapshot check covers the writes that ordering cannot: an invalidation
// landing between capture and promotion (a watcher event classified while
// a full sync was mid-pass) bumps the session's generation, so the stale
// promotion is dropped and the next pass re-verifies by content.
func (e *Engine) promoteOpenCodeStorageSession(
	path, state string, snap storageTrustSnapshot,
) {
	if path == "" || state == "" {
		return
	}
	e.storageTrustMu.Lock()
	defer e.storageTrustMu.Unlock()
	if snap.epoch != e.storageTrustEpoch || snap.gen != e.storageTrustGens[path] {
		return
	}
	if e.trustedStorageSessions == nil {
		e.trustedStorageSessions = make(map[string]string)
	}
	e.trustedStorageSessions[path] = state
}

// stageOpenCodeStorageTrust decides what a finished parse means for the
// session's gate trust. A complete result set with no exclusions or
// deferred retries either re-verified the session (every result dropped as
// already stored — promote immediately) or produced content headed for the
// write batch (stage the state on the result; the write path promotes it
// once the batch is confirmed fully written). Anything else — retries,
// exclusions, policy-suppressed results — invalidates so the next pass
// re-verifies by content.
func (e *Engine) stageOpenCodeStorageTrust(
	res *processResult,
	path, state string,
	snap storageTrustSnapshot,
	parsedCount int,
	resultSetComplete bool,
) {
	clean := resultSetComplete &&
		parsedCount > 0 &&
		len(res.excludedSessionIDs) == 0 &&
		res.retrySessionIDs == nil
	if clean && len(res.results) == 0 {
		e.promoteOpenCodeStorageSession(path, state, snap)
		return
	}
	e.dropOpenCodeStorageTrust(path)
	if clean {
		res.storageTrustPath = path
		res.storageTrustState = state
		res.storageTrustSnap = snap
	}
}

// promoteOpenCodeStorageTrustAfterWrite promotes the staged trust of every
// session in a written batch, but only when the batch verifiably persisted
// all of it: any failed, cwd-vetoed, or otherwise unwritten session leaves
// the whole batch unpromoted (write failures are not attributable to
// individual sessions), so those sessions re-verify on the next pass.
func (e *Engine) promoteOpenCodeStorageTrustAfterWrite(
	batch []pendingWrite,
	writtenSessions, failedWrites, cwdFiltered int,
) {
	if failedWrites > 0 || cwdFiltered > 0 ||
		writtenSessions != len(batch) {
		return
	}
	for _, pw := range batch {
		e.promoteOpenCodeStorageSession(
			pw.storageTrustPath, pw.storageTrustState, pw.storageTrustSnap,
		)
	}
}

// dropOpenCodeStorageTrust removes the session's trusted signature without
// bumping its invalidation generation. Used by the pass that owns the
// session's staged promotion (results in flight to the write batch, or an
// unclean parse), so the drop does not veto that same pass's promotion.
func (e *Engine) dropOpenCodeStorageTrust(path string) {
	if path == "" {
		return
	}
	e.storageTrustMu.Lock()
	defer e.storageTrustMu.Unlock()
	delete(e.trustedStorageSessions, path)
}

// invalidateOpenCodeStorageSession drops trust for one session and bumps
// its invalidation generation. Called when a watcher changed-path event
// resolves to the session: the event says something changed even if the
// stat signature cannot see it, so the next pass must re-verify by
// content. The generation bump extends that guarantee across a
// concurrently running pass — a promotion captured before the event
// presents a stale snapshot and is discarded, so it cannot restore the
// trust this invalidation revoked.
func (e *Engine) invalidateOpenCodeStorageSession(path string) {
	if path == "" {
		return
	}
	e.storageTrustMu.Lock()
	defer e.storageTrustMu.Unlock()
	delete(e.trustedStorageSessions, path)
	if e.storageTrustGens == nil {
		e.storageTrustGens = make(map[string]uint64)
	}
	e.storageTrustGens[path]++
}

// clearTrustedOpenCodeStorageSessions drops every trusted session state
// and starts a new trust epoch, so promotions staged before the clear are
// discarded. Called by resync, which rebuilds the archive from scratch and
// must re-verify every session against it.
func (e *Engine) clearTrustedOpenCodeStorageSessions() {
	e.storageTrustMu.Lock()
	defer e.storageTrustMu.Unlock()
	e.trustedStorageSessions = nil
	e.storageTrustGens = nil
	e.storageTrustEpoch++
}
