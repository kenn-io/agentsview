package sync

import (
	"context"
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
)

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
	stat := statS3Object
	switch file.Agent {
	case parser.AgentClaude:
		stat = statClaudeS3Session
	case parser.AgentCodex:
		stat = statCodexS3Session
	}
	return stat(file.Path)
}

func s3DiscoveredSessionID(file parser.DiscoveredFile) string {
	switch file.Agent {
	case parser.AgentClaude:
		id := claudeSessionIDFromPath(file.Path)
		if id == "" {
			return ""
		}
		return applyIDPrefixToID(s3SessionIDPrefix(file.Machine), id)
	case parser.AgentCodex:
		uuid := parser.CodexSessionUUIDFromFilename(path.Base(file.Path))
		if uuid == "" {
			return ""
		}
		return applyIDPrefixToID(
			s3SessionIDPrefix(file.Machine), "codex:"+uuid,
		)
	default:
		return ""
	}
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
		return e.s3CodexStoredNameDiffers(file, uuid, "")
	}
	if snapshot.mtime < cutoffNs {
		return false
	}

	snapshot = e.s3CodexIndexSnapshot(indexURI, true)
	if snapshot.err != nil {
		return true
	}
	if snapshot.missing {
		return e.s3CodexStoredNameDiffers(file, uuid, "")
	}
	if !snapshot.titlesLoaded {
		return false
	}

	title := snapshot.titles[uuid]
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
		return e.s3CodexStoredNameDiffers(file, uuid, ""), nil
	}
	if !snapshot.statOK || !snapshot.titlesLoaded {
		return false, nil
	}
	title := snapshot.titles[uuid]
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
	return seg == "claude" || seg == "codex"
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
			file.Machine = e.s3MachineForSource(
				file.Agent, root, s3MachineFromRoot(root),
			)
		}
		if file.Project == "" && file.Agent == parser.AgentClaude {
			if first, _, ok := strings.Cut(rel, "/"); ok {
				file.Project = first
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
		stat := statS3Object
		switch file.Agent {
		case parser.AgentClaude:
			stat = statClaudeS3Session
		case parser.AgentCodex:
			stat = statCodexS3Session
		}
		if obj, err := stat(file.Path); err == nil {
			file.SourceSize = obj.Size
			file.SourceMtime = obj.LastModified.UnixNano()
			file.SourceFingerprint = obj.Fingerprint
		}
	}
}
