package parser

import "strings"

// Kilo uses OpenCode's storage format, but sessions are exposed as a
// distinct agent with the kilo: ID prefix. The OpenCode-format provider
// owns parsing and relabels results through relabelOpenCodeSessionAsKilo.

func ListKiloSessionMeta(dbPath string) ([]OpenCodeSessionMeta, error) {
	metas, err := ListOpenCodeSessionMeta(dbPath)
	if err != nil {
		return nil, err
	}
	for i := range metas {
		metas[i].VirtualPath = KiloSQLiteVirtualPath(
			dbPath, metas[i].SessionID,
		)
	}
	return metas, nil
}

func KiloSourceMtime(sourcePath string) (int64, error) {
	if sourcePath == "" {
		return 0, nil
	}
	if dbPath, sessionID, ok := parseOpenCodeFormatVirtualPath(
		kiloFmt.dbName, sourcePath,
	); ok {
		return openCodeSQLiteSessionMtime(dbPath, sessionID)
	}
	return openCodeStorageSessionMtime(sourcePath)
}

func relabelOpenCodeSessionAsKilo(sess *ParsedSession) {
	sess.ID = strings.Replace(sess.ID, "opencode:", "kilo:", 1)
	if sess.ParentSessionID != "" {
		sess.ParentSessionID = strings.Replace(
			sess.ParentSessionID, "opencode:", "kilo:", 1,
		)
	}
	if sess.SourceSessionID != "" {
		sess.SourceSessionID = strings.Replace(
			sess.SourceSessionID, "opencode:", "kilo:", 1,
		)
	}
	sess.Agent = AgentKilo
}
