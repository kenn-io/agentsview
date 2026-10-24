package parser

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// AntigravitySummariesDBName is the per-root store where Antigravity keeps
// conversation titles, workspaces, and parents, apart from each
// conversation's transcript.
const AntigravitySummariesDBName = "conversation_summaries.db"

const antigravityTitleQuery = "SELECT conversation_id, title FROM conversation_summaries"

// AntigravitySummary is one conversation's row in AntigravitySummariesDBName.
type AntigravitySummary struct {
	Title string
	// TitlePresent distinguishes a stored (possibly blank) title from NULL.
	TitlePresent bool
	// Cwd is the first workspace folder; Project is derived from it.
	Cwd                  string
	Project              string
	ParentConversationID string
}

type antigravityTitleSnapshot struct {
	state     SQLiteContainerState
	summaries map[string]AntigravitySummary
	err       error
	readAt    time.Time
}

var antigravityTitleNow = time.Now

var antigravityTitleCache = struct {
	sync.Mutex
	stores map[string]antigravityTitleSnapshot
}{stores: make(map[string]antigravityTitleSnapshot)}

func InvalidateAntigravityTitles(path string) {
	antigravityTitleCache.Lock()
	defer antigravityTitleCache.Unlock()
	delete(antigravityTitleCache.stores, path)
}

// Matching markers can hide writes, so cached summaries expire.
func AntigravityStoreSummaries(ctx context.Context, path string) (map[string]AntigravitySummary, SQLiteContainerState, error) {
	state, ok := StatSQLiteContainerState(path)
	antigravityTitleCache.Lock()
	defer antigravityTitleCache.Unlock()
	now := antigravityTitleNow()
	if prior, cached := antigravityTitleCache.stores[path]; ok && cached && prior.state == state && now.Sub(prior.readAt) < SQLiteContainerDigestVerifyInterval {
		return prior.summaries, prior.state, prior.err
	}
	summaries, err := readAntigravitySummaries(ctx, path)
	if ctx.Err() != nil {
		return nil, state, ctx.Err()
	}
	if ok {
		// Remote imports extract stores into fresh temp roots; bound what stays cached.
		if _, cached := antigravityTitleCache.stores[path]; !cached && len(antigravityTitleCache.stores) >= 16 {
			clear(antigravityTitleCache.stores)
		}
		antigravityTitleCache.stores[path] = antigravityTitleSnapshot{state: state, summaries: summaries, err: err, readAt: now}
	} else {
		delete(antigravityTitleCache.stores, path)
	}
	return summaries, state, err
}

// AntigravityStoreTitles reads conversation titles through the shared native title cache.
func AntigravityStoreTitles(ctx context.Context, path string) (map[string]string, SQLiteContainerState, error) {
	return sqliteStoreTitles(ctx, path, "conversation_summaries", "conversation_id", antigravityTitleQuery)
}

func readAntigravitySummaries(ctx context.Context, path string) (map[string]AntigravitySummary, error) {
	if !IsRegularFile(path) {
		return nil, nil
	}
	observeSharedContainerScan(ctx)
	store, err := openSQLiteReadOnly(path, sqliteReadOptions{busyTimeoutMS: 3000})
	if err != nil {
		return nil, err
	}
	defer store.Close()
	var tableExists bool
	if err := store.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'conversation_summaries')`).Scan(&tableExists); err != nil {
		return nil, err
	}
	if !tableExists {
		// Same as the shared title reader: a database without the summaries
		// table is empty metadata, not a failed read.
		return nil, nil
	}
	// The workspace and parent columns are undocumented, so stores without
	// them still yield titles.
	workspaceColumn, parentColumn := "NULL", "NULL"
	columns, err := store.QueryContext(ctx, "SELECT name FROM pragma_table_info('conversation_summaries')")
	if err != nil {
		return nil, err
	}
	defer columns.Close()
	for columns.Next() {
		var name string
		if err := columns.Scan(&name); err != nil {
			return nil, err
		}
		switch name {
		case "workspace_uris":
			workspaceColumn = name
		case "parent_conversation_id":
			parentColumn = name
		}
	}
	if err := columns.Err(); err != nil {
		return nil, err
	}
	rows, err := store.QueryContext(ctx, "SELECT conversation_id, title, "+
		workspaceColumn+", "+parentColumn+" FROM conversation_summaries")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	summaries := make(map[string]AntigravitySummary)
	for rows.Next() {
		var id string
		var title, workspaceURIs, parentID sql.NullString
		if err := rows.Scan(&id, &title, &workspaceURIs, &parentID); err != nil {
			return nil, err
		}
		summary := AntigravitySummary{
			Title:                strings.TrimSpace(title.String),
			TitlePresent:         title.Valid,
			Cwd:                  antigravityWorkspace(workspaceURIs.String),
			ParentConversationID: strings.TrimSpace(parentID.String),
		}
		if summary.Cwd != "" {
			summary.Project = ExtractProjectFromCwdWithBranchContext(ctx, summary.Cwd, "")
		}
		summaries[id] = summary
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return summaries, nil
}

// antigravityWorkspace returns the first workspace in a workspace_uris value:
// a JSON array of URIs, a JSON string, or (when not JSON) a bare URI or path.
func antigravityWorkspace(value string) string {
	var uris []string
	if err := json.Unmarshal([]byte(value), &uris); err != nil {
		var uri string
		if err := json.Unmarshal([]byte(value), &uri); err != nil {
			uri = value
		}
		uris = []string{uri}
	}
	for _, uri := range uris {
		if path := antigravityWorkspacePath(uri); path != "" {
			return path
		}
	}
	return ""
}

// antigravityWorkspacePath turns a file:// URI (percent-encoded, with an
// optional localhost or UNC host and Windows drive letter) into a path.
func antigravityWorkspacePath(uri string) string {
	uri = strings.TrimSpace(uri)
	if !strings.HasPrefix(uri, "file://") {
		if uri == "" {
			return ""
		}
		return filepath.Clean(filepath.FromSlash(uri))
	}
	parsed, err := url.Parse(uri)
	if err != nil {
		return ""
	}
	path := parsed.Path
	if parsed.Host != "" && parsed.Host != "localhost" {
		path = "//" + parsed.Host + path
	} else if len(path) >= 3 && path[0] == '/' && path[2] == ':' {
		path = path[1:]
	}
	return filepath.Clean(filepath.FromSlash(path))
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

// applyAntigravityTitle sets the session's title, and fills its working
// directory, project, and parent, from its root's store. An unreadable store
// or missing entry leaves no title signal, so the stored name survives the
// write. The workspace and parent come from outside the transcript, so sync
// keeps any stored values instead of these.
func applyAntigravityTitle(ctx context.Context, sess *ParsedSession, src antigravitySource) {
	sess.KeepStoredWorkspace = sess.Cwd == ""
	sess.KeepStoredParent = sess.ParentSessionID == ""
	summaries, _, err := AntigravityStoreSummaries(ctx, filepath.Join(src.Root, AntigravitySummariesDBName))
	if err != nil {
		return
	}
	summary, ok := summaries[src.ID]
	if !ok {
		return
	}
	if summary.TitlePresent {
		sess.SessionName = summary.Title
		sess.SessionNamePresent = true
	}
	if sess.Cwd == "" {
		sess.Cwd = summary.Cwd
		if sess.Project == "" {
			sess.Project = summary.Project
		}
	}
	if sess.ParentSessionID == "" && summary.ParentConversationID != "" {
		sess.ParentSessionID = antigravityIDPrefix + summary.ParentConversationID
		if sess.RelationshipType == "" {
			sess.RelationshipType = RelSubagent
		}
	}
}

// PreservesStoredSessionName reports whether a parse carries no title signal
// for an agent whose titles live outside the transcript.
func PreservesStoredSessionName(sess ParsedSession) bool {
	return (sess.Agent == AgentCodex ||
		sess.Agent == AgentAntigravity ||
		sess.Agent == AgentQoder) && !sess.SessionNamePresent
}
