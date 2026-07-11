package sync

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// verifiedSourceSignature is the complete local filesystem identity trusted
// by the gate. Sidecar fields are zero for providers without an auxiliary
// source and carry the raw Codex index identity when that provider opts in.
type verifiedSourceSignature struct {
	size              int64
	mtime             int64
	inode             int64
	device            int64
	changeTime        int64
	sidecarSize       int64
	sidecarMtime      int64
	sidecarInode      int64
	sidecarDevice     int64
	sidecarChangeTime int64
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

// verifiedProviderSourceState captures the local stat/ctime signature before
// any content verification. Providers must opt in explicitly; forced,
// path-rewritten, non-regular, and unreliable-change-time sources bypass the
// gate and retain their existing verification path.
func (e *Engine) verifiedProviderSourceState(
	provider parser.Provider,
	source parser.SourceRef,
	file parser.DiscoveredFile,
) (verifiedSourceCapture, int64, bool, bool) {
	if e.forceParse || file.ForceParse || e.pathRewriter != nil ||
		provider.Capabilities().Source.VerifiedLocalStat !=
			parser.CapabilitySupported {
		return verifiedSourceCapture{}, 0, false, false
	}
	path := providerDiscoveredPath(source)
	if path == "" || strings.HasPrefix(path, "s3://") {
		return verifiedSourceCapture{}, 0, false, false
	}
	path = filepath.Clean(path)
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return verifiedSourceCapture{}, 0, false, false
	}
	changeTime, ok := fileChangeTime(path, info)
	if !ok {
		return verifiedSourceCapture{}, 0, false, false
	}
	inode, device := getFileIdentity(info)
	mtime := info.ModTime().UnixNano()
	sidecar := verifiedSourceSignature{}
	if provider.Definition().Type == parser.AgentCodex {
		indexPath := parser.CodexSessionIndexPath(path)
		if indexPath != "" {
			indexInfo, indexErr := os.Stat(indexPath)
			switch {
			case indexErr == nil:
				if !indexInfo.Mode().IsRegular() {
					return verifiedSourceCapture{}, 0, false, false
				}
				indexChangeTime, reliable := fileChangeTime(
					indexPath, indexInfo,
				)
				if !reliable {
					return verifiedSourceCapture{}, 0, false, false
				}
				indexInode, indexDevice := getFileIdentity(indexInfo)
				sidecar.sidecarSize = indexInfo.Size()
				sidecar.sidecarMtime = indexInfo.ModTime().UnixNano()
				sidecar.sidecarInode = indexInode
				sidecar.sidecarDevice = indexDevice
				sidecar.sidecarChangeTime = indexChangeTime
			case !errors.Is(indexErr, os.ErrNotExist):
				return verifiedSourceCapture{}, 0, false, false
			}
		}
	}
	capture, fresh := e.captureVerifiedSource(path, verifiedSourceSignature{
		size:              info.Size(),
		mtime:             mtime,
		inode:             inode,
		device:            device,
		changeTime:        changeTime,
		sidecarSize:       sidecar.sidecarSize,
		sidecarMtime:      sidecar.sidecarMtime,
		sidecarInode:      sidecar.sidecarInode,
		sidecarDevice:     sidecar.sidecarDevice,
		sidecarChangeTime: sidecar.sidecarChangeTime,
	})
	if sidecar.sidecarMtime > mtime {
		mtime = sidecar.sidecarMtime
	}
	return capture, mtime, fresh, true
}

// verifiedProviderSourceFreshInDB preserves the self-healing checks that run
// below the verified-source fast path. A matching filesystem signature cannot
// hide a missing active row, forced file-metadata reset, old parser data
// version, or project value that the current parser knows how to repair.
func (e *Engine) verifiedProviderSourceFreshInDB(
	source parser.SourceRef,
	wantSize, wantMtime int64,
) bool {
	path := providerDiscoveredPath(source)
	if path == "" {
		return false
	}
	project, dataVersion, storedSize, storedMtime, ok :=
		e.db.GetSourceRepairStateByPath(path)
	if !ok || parser.NeedsProjectReparse(project) {
		return false
	}
	return storedSize == wantSize &&
		storedMtime == wantMtime &&
		dataVersion >= db.CurrentDataVersion()
}

func (e *Engine) verifiedLocalStatSupported(agent parser.AgentType) bool {
	factory, ok := e.providerFactories[agent]
	return ok && factory != nil &&
		factory.Capabilities().Source.VerifiedLocalStat ==
			parser.CapabilitySupported
}

func (e *Engine) markVerifiedSourceSeen(path string) {
	if path == "" {
		return
	}
	path = filepath.Clean(path)
	e.verifiedSourceMu.Lock()
	defer e.verifiedSourceMu.Unlock()
	if e.verifiedSourceActivePass == 0 {
		return
	}
	record, ok := e.verifiedSources[path]
	if !ok {
		return
	}
	record.lastSeenPass = e.verifiedSourceActivePass
	e.verifiedSources[path] = record
}

// markVerifiedDiscoveredSources preserves trusted records for gateable sources
// present in the full pre-filter discovery set, including files omitted later
// by a quick-sync cutoff.
func (e *Engine) markVerifiedDiscoveredSources(files []parser.DiscoveredFile) {
	if e.pathRewriter != nil {
		return
	}
	for _, file := range files {
		if e.verifiedLocalStatSupported(file.Agent) {
			e.markVerifiedSourceSeen(file.Path)
		}
	}
}

// invalidateVerifiedDiscoveredSource makes a concrete changed-path signal win
// over a matching stat signature. Only opted-in local providers allocate
// invalidation records.
func (e *Engine) invalidateVerifiedDiscoveredSource(file parser.DiscoveredFile) {
	if e.pathRewriter != nil || !e.verifiedLocalStatSupported(file.Agent) {
		return
	}
	e.invalidateVerifiedSource(filepath.Clean(file.Path))
}
