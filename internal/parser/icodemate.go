package parser

import "strings"

// Icodemate uses OpenCode's storage format, and is exposed as a distinct
// agent with the icodemate: ID prefix.
func ParseIcodemateFile(
	sessionPath, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	sess, msgs, err := ParseOpenCodeFile(sessionPath, machine)
	if err != nil || sess == nil {
		return sess, msgs, err
	}
	relabelOpenCodeSessionAsIcodemate(sess)
	return sess, msgs, nil
}

func ParseIcodemateSession(
	dbPath, sessionID, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	sess, msgs, err := ParseOpenCodeSession(dbPath, sessionID, machine)
	if err != nil || sess == nil {
		return sess, msgs, err
	}
	relabelOpenCodeSessionAsIcodemate(sess)
	return sess, msgs, nil
}

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
