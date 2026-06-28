package parser

import "strings"

// Icodemate uses OpenCode's storage format and is exposed as a distinct
// agent with the icodemate: ID prefix. Discovery and parsing run through
// the shared OpenCode-format provider (openCodeProviderSpecForAgent); the
// helpers below adapt the shared SQLite metadata reader, source mtime, and
// session relabeling to Icodemate.
func ListIcodemateSessionMeta(dbPath string) ([]OpenCodeSessionMeta, error) {
	metas, err := ListOpenCodeSessionMeta(dbPath)
	if err != nil {
		return nil, err
	}
	for i := range metas {
		metas[i].VirtualPath = IcodemateSQLiteVirtualPath(
			dbPath, metas[i].SessionID,
		)
	}
	return metas, nil
}

func IcodemateSourceMtime(sourcePath string) (int64, error) {
	if sourcePath == "" {
		return 0, nil
	}
	if dbPath, sessionID, ok := ParseIcodemateSQLiteVirtualPath(sourcePath); ok {
		return openCodeSQLiteSessionMtime(dbPath, sessionID)
	}
	return openCodeStorageSessionMtime(sourcePath)
}

func relabelOpenCodeSessionAsIcodemate(sess *ParsedSession) {
	sess.ID = strings.Replace(sess.ID, "opencode:", "icodemate:", 1)
	if sess.ParentSessionID != "" {
		sess.ParentSessionID = strings.Replace(
			sess.ParentSessionID, "opencode:", "icodemate:", 1,
		)
	}
	if sess.SourceSessionID != "" {
		sess.SourceSessionID = strings.Replace(
			sess.SourceSessionID, "opencode:", "icodemate:", 1,
		)
	}
	sess.Agent = AgentIcodemate
}
