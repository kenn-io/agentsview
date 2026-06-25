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
		stat := statS3Object
		switch file.Agent {
		case parser.AgentClaude:
			stat = statClaudeS3Session
		case parser.AgentCodex:
			stat = statCodexS3Session
		}
		obj, err := stat(file.Path)
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
	stat := statS3Object
	switch file.Agent {
	case parser.AgentClaude:
		stat = statClaudeS3Session
	case parser.AgentCodex:
		stat = statCodexS3Session
	}
	obj, err := stat(file.Path)
	if err != nil {
		return ""
	}
	return obj.Fingerprint
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

func (e *Engine) s3SourceFingerprintChanged(file parser.DiscoveredFile) bool {
	if file.SourceFingerprint == "" {
		return false
	}
	sessionID := s3DiscoveredSessionID(file)
	if sessionID == "" || e.db.GetSessionFilePath(sessionID) != file.Path {
		return false
	}
	storedHash, ok := e.db.GetSessionFileHash(sessionID)
	return !ok || storedHash != file.SourceFingerprint
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
			file.Machine = s3MachineFromRoot(root)
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
