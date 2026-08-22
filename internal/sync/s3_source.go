package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

var (
	fetchS3Object       = parser.FetchS3Object
	statS3Object        = parser.StatS3Object
	statClaudeS3Session = parser.StatClaudeS3Session
	statCodexS3Session  = parser.StatCodexS3Session
	lookupS3Provider    = parser.S3ProviderFor
)

func s3ProviderFor(agent parser.AgentType) (parser.S3Provider, bool) {
	return lookupS3Provider(agent)
}

func s3SourceFileInfo(file parser.DiscoveredFile) (os.FileInfo, error) {
	size := file.SourceSize
	mtime := file.SourceMtime
	if mtime == 0 {
		obj, err := statS3SourceObject(file)
		if err != nil {
			return nil, err
		}
		size = obj.Size
		mtime = obj.LastModified.UnixNano()
	}
	return fakeSnapshotInfo{
		fName:  path.Base(file.Path),
		fSize:  size,
		fMtime: mtime,
	}, nil
}

func s3SourceFingerprint(file parser.DiscoveredFile) string {
	if file.SourceFingerprint != "" {
		return file.SourceFingerprint
	}
	if file.SourceMtime != 0 {
		return ""
	}
	obj, err := statS3SourceObject(file)
	if err != nil {
		return ""
	}
	return obj.Fingerprint
}

func statS3SourceObject(file parser.DiscoveredFile) (parser.S3Object, error) {
	// Keep Claude/Codex package-level hooks so existing tests can stub
	// sidecar-aware stats without replacing the provider.
	switch file.Agent {
	case parser.AgentClaude:
		return statClaudeS3Session(file.Path)
	case parser.AgentCodex:
		return statCodexS3Session(file.Path)
	}
	p, ok := s3ProviderFor(file.Agent)
	if !ok {
		return parser.S3Object{}, fmt.Errorf("unsupported s3 agent type: %s", file.Agent)
	}
	return p.S3StatSession(file.Path)
}

func s3DiscoveredSessionID(file parser.DiscoveredFile) string {
	p, ok := s3ProviderFor(file.Agent)
	if !ok {
		return ""
	}
	id := p.S3SessionID(file.Path)
	if id == "" {
		return ""
	}
	return applyIDPrefixToID(s3SessionIDPrefix(file.Machine), id)
}

func (e *Engine) s3SourceMetadataChanged(file parser.DiscoveredFile) bool {
	if file.SourceMtime == 0 {
		return false
	}
	return e.s3SourceMetadataChangedFromInfo(
		file, file.SourceSize, file.SourceMtime, file.SourceFingerprint,
	)
}

func (e *Engine) s3SourceMetadataChangedFromInfo(
	file parser.DiscoveredFile, size, mtime int64, sourceFingerprint string,
) bool {
	sessionID := s3DiscoveredSessionID(file)
	if sessionID == "" {
		return false
	}
	storedPath := e.db.GetSessionFilePath(sessionID)
	if storedPath == "" || storedPath != file.Path {
		return true
	}
	storedSize, storedMtime, ok := e.db.GetSessionFileInfo(sessionID)
	if !ok {
		return true
	}
	if storedSize != size || storedMtime != mtime {
		return true
	}
	if sourceFingerprint != "" {
		storedHash, ok := e.db.GetSessionFileHash(sessionID)
		if !ok || storedHash != sourceFingerprint {
			return true
		}
	}
	return false
}

type s3CodexIndexSnapshot struct {
	mtime        int64
	statOK       bool
	missing      bool
	err          error
	titles       map[string]string
	titlesLoaded bool
}

func (e *Engine) resetS3CodexIndexCache() {
	e.s3CodexIndexMu.Lock()
	e.s3CodexIndexCache = make(map[string]s3CodexIndexSnapshot)
	e.s3CodexIndexMu.Unlock()
}

func (e *Engine) s3CodexIndexSnapshot(
	indexURI string, needTitles bool,
) s3CodexIndexSnapshot {
	e.s3CodexIndexMu.Lock()
	defer e.s3CodexIndexMu.Unlock()
	if e.s3CodexIndexCache == nil {
		e.s3CodexIndexCache = make(map[string]s3CodexIndexSnapshot)
	}

	snapshot, ok := e.s3CodexIndexCache[indexURI]
	if !ok {
		obj, err := statS3Object(indexURI)
		if err != nil {
			if isMissingS3Object(err) {
				snapshot = s3CodexIndexSnapshot{
					statOK:       true,
					missing:      true,
					titlesLoaded: true,
				}
			} else {
				snapshot.err = err
			}
			e.s3CodexIndexCache[indexURI] = snapshot
			return snapshot
		}
		snapshot = s3CodexIndexSnapshot{
			mtime:  obj.LastModified.UnixNano(),
			statOK: true,
		}
		e.s3CodexIndexCache[indexURI] = snapshot
	}

	if needTitles && snapshot.statOK && !snapshot.missing &&
		!snapshot.titlesLoaded && snapshot.err == nil {
		titles, err := fetchS3CodexSessionIndexTitles(indexURI)
		snapshot.titlesLoaded = err == nil
		if err == nil {
			snapshot.titles = titles
		} else if isMissingS3Object(err) {
			snapshot.missing = true
			snapshot.titlesLoaded = true
			snapshot.titles = nil
		} else {
			snapshot.err = err
		}
		e.s3CodexIndexCache[indexURI] = snapshot
	}

	return snapshot
}

func (e *Engine) s3CodexIndexNeedsRefreshSince(
	file parser.DiscoveredFile,
	cutoffNs int64,
) bool {
	uuid := parser.CodexSessionUUIDFromFilename(path.Base(file.Path))
	if uuid == "" {
		return false
	}
	indexURI, ok := parser.CodexS3SessionIndexURI(file.Path)
	if !ok {
		return false
	}
	snapshot := e.s3CodexIndexSnapshot(indexURI, false)
	if snapshot.err != nil {
		return true
	}
	if !snapshot.statOK {
		return false
	}
	if snapshot.missing {
		return false
	}
	if snapshot.mtime < cutoffNs {
		return false
	}

	snapshot = e.s3CodexIndexSnapshot(indexURI, true)
	if snapshot.err != nil {
		return true
	}
	if snapshot.missing {
		return false
	}
	if !snapshot.titlesLoaded {
		return false
	}

	title, ok := snapshot.titles[uuid]
	if !ok {
		return false
	}
	return e.s3CodexStoredNameDiffers(file, uuid, title)
}

func (e *Engine) s3CodexIndexSessionNameChanged(
	file parser.DiscoveredFile, uuid string,
) (bool, error) {
	indexURI, ok := parser.CodexS3SessionIndexURI(file.Path)
	if !ok {
		return false, nil
	}
	snapshot := e.s3CodexIndexSnapshot(indexURI, true)
	if snapshot.err != nil {
		return false, snapshot.err
	}
	if snapshot.missing {
		return false, nil
	}
	if !snapshot.statOK || !snapshot.titlesLoaded {
		return false, nil
	}
	title, ok := snapshot.titles[uuid]
	if !ok {
		return false, nil
	}
	return e.s3CodexStoredNameDiffers(file, uuid, title), nil
}

func (e *Engine) s3CodexStoredNameDiffers(
	file parser.DiscoveredFile, uuid, indexTitle string,
) bool {
	sessionID := applyIDPrefixToID(
		s3SessionIDPrefix(file.Machine), "codex:"+uuid,
	)
	return e.codexStoredNameDiffersBySessionID(
		sessionID, indexTitle, false,
	)
}

func fetchS3CodexSessionIndexTitles(
	indexURI string,
) (map[string]string, error) {
	rc, err := fetchS3Object(indexURI)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return parser.ParseCodexSessionIndexTitles(rc)
}

func isS3SourcePath(path string) bool {
	return strings.HasPrefix(path, "s3://")
}

func (e *Engine) shouldSkipFileWithPrefix(
	prefix, sessionID string, info os.FileInfo, sourceFingerprint ...string,
) bool {
	if e.forceParse {
		return false
	}
	fullID := applyIDPrefixToID(prefix, sessionID)
	storedSize, storedMtime, ok := e.db.GetSessionFileInfo(
		fullID,
	)
	if !ok {
		return false
	}
	if storedSize != info.Size() ||
		storedMtime != info.ModTime().UnixNano() {
		return false
	}
	if len(sourceFingerprint) > 0 && sourceFingerprint[0] != "" {
		storedHash, ok := e.db.GetSessionFileHash(fullID)
		if !ok || storedHash != sourceFingerprint[0] {
			return false
		}
	}
	if e.db.GetSessionDataVersion(fullID) <
		db.CurrentDataVersion() {
		return false
	}
	return true
}

func s3MachineFromRoot(root string) string {
	segs := strings.Split(strings.TrimPrefix(root, "s3://"), "/")
	for i := len(segs) - 2; i > 1; i-- {
		if segs[i] == "raw" && isS3AgentRootSegment(segs[i+1]) {
			return segs[i-1]
		}
	}
	return ""
}

func isS3AgentRootSegment(seg string) bool {
	return parser.AgentSupportsS3Discovery(parser.AgentType(seg))
}

func s3RelFromRoot(root, uri string) (string, bool) {
	prefix := strings.TrimSuffix(root, "/")
	if !strings.HasPrefix(uri, prefix+"/") {
		return "", false
	}
	return strings.TrimPrefix(uri, prefix+"/"), true
}

func (e *Engine) hydrateS3DiscoveredFile(
	ctx context.Context, sessionID string, file *parser.DiscoveredFile,
) {
	if !isS3SourcePath(file.Path) {
		return
	}
	if sess, _ := e.db.GetSession(ctx, sessionID); sess != nil {
		if sess.Project != "" &&
			!parser.NeedsProjectReparse(sess.Project) {
			file.Project = sess.Project
		}
	}
	for _, root := range e.agentDirs[file.Agent] {
		if !isS3SourcePath(root) {
			continue
		}
		rel, ok := s3RelFromRoot(root, file.Path)
		if !ok {
			continue
		}
		if file.Machine == "" {
			file.Machine = s3MachineFromRoot(root)
		}
		if file.Project == "" {
			if p, ok := s3ProviderFor(file.Agent); ok {
				scan := p.S3Scanner()
				if scan.Project != nil && strings.Contains(rel, "/") {
					segs := strings.Split(rel, "/")
					file.Project = scan.Project(rel, segs)
				}
			}
		}
		break
	}
	if file.Machine == "" {
		if host, _ := parser.StripHostPrefix(sessionID); host != "" {
			file.Machine = host
		}
	}
	if file.SourceMtime == 0 {
		obj, err := statS3SourceObject(*file)
		if err == nil {
			file.SourceSize = obj.Size
			file.SourceMtime = obj.LastModified.UnixNano()
			file.SourceFingerprint = obj.Fingerprint
		}
	}
}

// SyncClaudeS3SubagentTranscriptsContext ingests the given s3:// Claude
// subagent transcript objects. The changed-path pipeline
// (SyncPathsContext) classifies by statting local files, so it cannot
// route s3:// objects; the on-demand subagent refresh behind `session
// usage` syncs them here instead, one process-and-write per object with
// the project, machine, and object-metadata hydration the s3 discovery
// path would apply. Work is bounded by the given objects, never by
// archive size. Objects that fail to sync are reported joined; the
// remaining objects still sync. parentSessionID preserves the stored
// parent's machine namespace when its original S3 root is no longer
// configured.
func (e *Engine) SyncClaudeS3SubagentTranscriptsContext(
	ctx context.Context, parentSessionID string, paths []string,
) error {
	if e.refuseWriteInForceParse("SyncClaudeS3SubagentTranscripts") {
		return nil
	}
	e.syncMu.Lock()
	synced := false
	sessionsChanged := false
	// Defers run LIFO: emit runs after syncMu.Unlock, matching the
	// other sync entry points.
	defer func() {
		if synced {
			e.emit("messages")
		}
		if sessionsChanged {
			e.emit("sessions")
		}
	}()
	defer e.syncMu.Unlock()

	parentMachine, _ := parser.StripHostPrefix(parentSessionID)
	parentProject := ""
	if parent, _ := e.db.GetSession(ctx, parentSessionID); parent != nil &&
		parent.Project != "" &&
		!parser.NeedsProjectReparse(parent.Project) {
		parentProject = parent.Project
	}
	var errs error
	for _, p := range paths {
		if err := ctx.Err(); err != nil {
			return errors.Join(errs, err)
		}
		if !isS3SourcePath(p) {
			continue
		}
		file := parser.DiscoveredFile{
			Path: p, Agent: parser.AgentClaude, Machine: parentMachine,
		}
		sessionID := strings.TrimSuffix(path.Base(p), ".jsonl")
		childID := applyIDPrefixToID(
			s3SessionIDPrefix(parentMachine), sessionID)
		e.hydrateS3DiscoveredFile(ctx, childID, &file)
		if file.Project == "" {
			file.Project = parentProject
		}
		preserved, sourceSessionsChanged, err := e.processAndWriteSessionFile(
			ctx, file, childID,
		)
		sessionsChanged = sessionsChanged || sourceSessionsChanged
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf(
				"sync subagent transcript %s: %w", p, err))
			continue
		}
		if !preserved {
			synced = true
		}
	}
	return errs
}
