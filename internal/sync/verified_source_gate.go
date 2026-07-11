package sync

// verifiedSourceSignature is the complete local filesystem identity trusted
// by the gate. sidecarMtime is zero for providers without an auxiliary source
// and carries Codex's effective index watermark when that provider opts in.
type verifiedSourceSignature struct {
	size         int64
	mtime        int64
	inode        int64
	device       int64
	changeTime   int64
	sidecarMtime int64
}

// verifiedSourceRecord deliberately combines trust, invalidation, and pass
// bookkeeping in one map value. Unknown-path invalidations also live here so a
// concurrent stale promotion cannot recreate trust after the event.
type verifiedSourceRecord struct {
	signature       verifiedSourceSignature
	invalidationGen uint64
	lastSeenPass    uint64
	trusted         bool
}

// verifiedSourceCapture binds a pre-verification signature to the path's
// invalidation coordinates. Promotion succeeds only if neither coordinate
// changed while content verification was in flight.
type verifiedSourceCapture struct {
	path         string
	signature    verifiedSourceSignature
	epoch        uint64
	invalidation uint64
}

// captureVerifiedSource records that a full pass saw path, snapshots its
// invalidation coordinates before verification, and reports whether the same
// signature was previously promoted.
func (e *Engine) captureVerifiedSource(
	path string,
	signature verifiedSourceSignature,
) (verifiedSourceCapture, bool) {
	if path == "" {
		return verifiedSourceCapture{}, false
	}
	e.verifiedSourceMu.Lock()
	defer e.verifiedSourceMu.Unlock()
	if e.verifiedSources == nil {
		e.verifiedSources = make(map[string]verifiedSourceRecord)
	}
	record := e.verifiedSources[path]
	if e.verifiedSourceActivePass != 0 {
		record.lastSeenPass = e.verifiedSourceActivePass
	}
	e.verifiedSources[path] = record
	return verifiedSourceCapture{
		path:         path,
		signature:    signature,
		epoch:        e.verifiedSourceEpoch,
		invalidation: record.invalidationGen,
	}, record.trusted && record.signature == signature
}

// promoteVerifiedSource trusts a capture only when no path invalidation,
// global clear, or completed-pass pruning landed after capture.
func (e *Engine) promoteVerifiedSource(capture verifiedSourceCapture) {
	if capture.path == "" {
		return
	}
	e.verifiedSourceMu.Lock()
	defer e.verifiedSourceMu.Unlock()
	record, ok := e.verifiedSources[capture.path]
	if !ok ||
		capture.epoch != e.verifiedSourceEpoch ||
		capture.invalidation != record.invalidationGen {
		return
	}
	record.signature = capture.signature
	record.trusted = true
	e.verifiedSources[capture.path] = record
}

// invalidateVerifiedSource drops one path's trust and advances its generation.
// During an active full pass the invalidation record is marked seen so pruning
// cannot erase the generation before a stale in-flight promotion observes it.
func (e *Engine) invalidateVerifiedSource(path string) {
	if path == "" {
		return
	}
	e.verifiedSourceMu.Lock()
	defer e.verifiedSourceMu.Unlock()
	if e.verifiedSources == nil {
		e.verifiedSources = make(map[string]verifiedSourceRecord)
	}
	record := e.verifiedSources[path]
	record.trusted = false
	record.invalidationGen++
	if e.verifiedSourceActivePass != 0 {
		record.lastSeenPass = e.verifiedSourceActivePass
	}
	e.verifiedSources[path] = record
}

// clearVerifiedSources invalidates every trusted source and vetoes all
// captures taken before the clear without retaining per-path tombstones.
func (e *Engine) clearVerifiedSources() {
	e.verifiedSourceMu.Lock()
	defer e.verifiedSourceMu.Unlock()
	e.verifiedSources = nil
	e.verifiedSourceEpoch++
}

// beginVerifiedSourcePass starts the seen-generation used by one full source
// discovery. Changed-path and scoped passes do not call it and never prune.
func (e *Engine) beginVerifiedSourcePass() uint64 {
	e.verifiedSourceMu.Lock()
	defer e.verifiedSourceMu.Unlock()
	e.verifiedSourcePass++
	if e.verifiedSourcePass == 0 {
		e.verifiedSourcePass++
	}
	e.verifiedSourceActivePass = e.verifiedSourcePass
	return e.verifiedSourceActivePass
}

// finishVerifiedSourcePass prunes records absent from a completed full
// discovery. Incomplete or superseded passes leave existing trust untouched.
func (e *Engine) finishVerifiedSourcePass(pass uint64, complete bool) {
	if pass == 0 {
		return
	}
	e.verifiedSourceMu.Lock()
	defer e.verifiedSourceMu.Unlock()
	if e.verifiedSourceActivePass != pass {
		return
	}
	if complete {
		for path, record := range e.verifiedSources {
			if record.lastSeenPass != pass {
				delete(e.verifiedSources, path)
			}
		}
	}
	e.verifiedSourceActivePass = 0
}
