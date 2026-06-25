package parser

import "strings"

// MiMoCode uses OpenCode's storage format, but stores sessions under
// storage/session_diff and is exposed as a distinct agent with the
// mimocode: ID prefix. The OpenCode-format provider owns parsing and
// relabels results through relabelOpenCodeSessionAsMiMoCode.

func ListMiMoCodeSessionMeta(dbPath string) ([]OpenCodeSessionMeta, error) {
	metas, err := ListOpenCodeSessionMeta(dbPath)
	if err != nil {
		return nil, err
	}
	for i := range metas {
		metas[i].VirtualPath = MiMoCodeSQLiteVirtualPath(
			dbPath, metas[i].SessionID,
		)
	}
	return metas, nil
}

func MiMoCodeSourceMtime(sourcePath string) (int64, error) {
	if sourcePath == "" {
		return 0, nil
	}
	if dbPath, sessionID, ok := parseOpenCodeFormatVirtualPath(
		mimoFmt.dbName, sourcePath,
	); ok {
		return openCodeSQLiteSessionMtime(dbPath, sessionID)
	}
	return openCodeStorageSessionMtime(sourcePath)
}

func relabelOpenCodeSessionAsMiMoCode(sess *ParsedSession) {
	sess.ID = strings.Replace(sess.ID, "opencode:", "mimocode:", 1)
	if sess.ParentSessionID != "" {
		sess.ParentSessionID = strings.Replace(
			sess.ParentSessionID, "opencode:", "mimocode:", 1,
		)
	}
	if sess.SourceSessionID != "" {
		sess.SourceSessionID = strings.Replace(
			sess.SourceSessionID, "opencode:", "mimocode:", 1,
		)
	}
	sess.Agent = AgentMiMoCode
}
