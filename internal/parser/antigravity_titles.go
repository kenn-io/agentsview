package parser

import (
	"context"
	"path/filepath"
)

// AntigravitySummariesDBName is the per-root store where Antigravity keeps
// conversation titles, apart from each conversation's transcript.
const AntigravitySummariesDBName = "conversation_summaries.db"

const antigravityTitleQuery = "SELECT conversation_id, title FROM conversation_summaries"

// Matching markers can hide writes, so cached titles expire.
func AntigravityStoreTitles(ctx context.Context, path string) (map[string]string, SQLiteContainerState, error) {
	return sqliteStoreTitles(ctx, path, "conversation_summaries", "conversation_id", antigravityTitleQuery)
}

// AntigravitySessionRoot returns the Antigravity root that owns a stored
// conversation database or brain transcript path, even after it was deleted.
func AntigravitySessionRoot(path string) (string, bool) {
	path = filepath.Clean(path)
	if root, _, ok := antigravityBrainTranscriptConversation(path); ok {
		return root, true
	}
	root := filepath.Dir(filepath.Dir(path))
	if _, _, ok := antigravityConversationDBForPath(root, path); ok {
		return root, true
	}
	return "", false
}

// AntigravityCLISessionRoot returns the Antigravity CLI root that owns a
// stored conversation path, even after it was deleted.
func AntigravityCLISessionRoot(path string) (string, bool) {
	path = filepath.Clean(path)
	root := filepath.Dir(filepath.Dir(path))
	if _, ok := antigravityCLISessionIDForPath(root, path); ok {
		return root, true
	}
	return "", false
}

// AntigravityCLITitleStoreOwner reports whether store is the
// conversation_summaries.db of root or of an instance beneath root.
func AntigravityCLITitleStoreOwner(root, store string) bool {
	cleanRoot := filepath.Clean(root)
	cleanStore := filepath.Clean(store)
	if filepath.Base(cleanStore) != AntigravitySummariesDBName {
		return false
	}
	if samePath(filepath.Dir(cleanStore), cleanRoot) {
		return true
	}
	instanceRoot, ok := antigravityCLIContainerSpec.InstanceRootForPath(cleanRoot, cleanStore)
	return ok && samePath(filepath.Dir(cleanStore), instanceRoot)
}

// applyAntigravityTitle sets the session's title from its root's store. An
// unreadable store or missing entry leaves no title signal, so the stored
// name survives the write.
func applyAntigravityTitle(ctx context.Context, sess *ParsedSession, root, id string) {
	titles, _, err := AntigravityStoreTitles(ctx, filepath.Join(root, AntigravitySummariesDBName))
	if err != nil {
		return
	}
	if title, ok := titles[id]; ok {
		sess.SessionName = title
		sess.SessionNamePresent = true
	}
}

// PreservesStoredSessionName reports whether a parse carries no title signal
// for an agent whose titles live outside the transcript.
func PreservesStoredSessionName(sess ParsedSession) bool {
	return (sess.Agent == AgentCodex ||
		sess.Agent == AgentAntigravity ||
		sess.Agent == AgentAntigravityCLI ||
		sess.Agent == AgentQoder) && !sess.SessionNamePresent
}
