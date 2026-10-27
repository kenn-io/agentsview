package parser

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

// Besides titles, a root's conversation_summaries.db records two facts that
// the transcript does not: each conversation's workspace folders, and the
// conversation that started a sub-agent conversation. Both providers read
// them through loadAntigravitySummaries. Discovery turns the workspace into
// source CWD authority and parsing applies the parent. Each source's
// fingerprint covers both, so a full sync reparses exactly the conversations
// whose store facts changed, and a store write routes only those
// conversations (see changedAntigravitySummaryIDs).

// antigravitySummary is what one conversation's summary row says outside its
// transcript.
type antigravitySummary struct {
	// workspace is the first workspace folder as a local path, or "" when
	// that folder is not local. hasWorkspace reports whether any folder is
	// named.
	workspace    string
	hasWorkspace bool
	// parentID is the canonical ID of the conversation that started this
	// one, or "".
	parentID string
}

// antigravitySummaries is one read of a root's summary store. err is set when
// the store exists but cannot be read.
type antigravitySummaries struct {
	byID map[string]antigravitySummary
	err  error
}

// cwdResolution describes what the store knows about a conversation's local
// working directory. A conversation the store does not list, or lists with no
// workspace folder, leaves the parsed value alone; an unreadable store keeps
// the stored one.
func (s antigravitySummaries) cwdResolution(id string) SourceCwdResolution {
	if s.err != nil {
		return SourceCwdResolution{State: SourceCwdUnavailable}
	}
	summary := s.byID[id]
	switch {
	case !summary.hasWorkspace:
		return SourceCwdResolution{}
	case summary.workspace == "":
		return SourceCwdResolution{State: SourceCwdAmbiguous}
	}
	return SourceCwdResolution{State: SourceCwdResolved, Path: summary.workspace}
}

// parent returns the conversation that started id, or "".
func (s antigravitySummaries) parent(id string) string {
	parent := s.byID[id].parentID
	if strings.EqualFold(parent, id) {
		return ""
	}
	return parent
}

// writeFingerprint adds what the store says about id to a source
// fingerprint. A conversation the store does not list adds nothing, so
// roots without a store keep their fingerprints.
func (s antigravitySummaries) writeFingerprint(w io.Writer, id string) error {
	if s.err != nil {
		_, err := io.WriteString(w, "summary\x00unreadable\x00")
		return err
	}
	summary, ok := s.byID[id]
	if !ok {
		return nil
	}
	_, err := fmt.Fprintf(w, "summary\x00%t\x00%d\x00%s\x00%s\x00",
		summary.hasWorkspace, len(summary.workspace), summary.workspace, s.parent(id))
	return err
}

type antigravitySummariesSnapshot struct {
	state     SQLiteContainerState
	summaries antigravitySummaries
	readAt    time.Time
}

var antigravitySummariesNow = time.Now

// Discovery, fingerprints, change routing, and parsing all ask for the same
// store, so one read serves them while the store's container state holds.
// Matching markers can hide a write, so a cached read expires.
var antigravitySummariesCache = struct {
	sync.Mutex
	stores map[string]antigravitySummariesSnapshot
}{stores: make(map[string]antigravitySummariesSnapshot)}

// loadAntigravitySummaries reads the summary store under root. While a store
// that was read before cannot be read, its last good read stands in, so an
// outage neither changes fingerprints nor clears stored workspaces and
// parents.
func loadAntigravitySummaries(ctx context.Context, root string) antigravitySummaries {
	path := filepath.Join(root, AntigravitySummariesDBName)
	state, exists := StatSQLiteContainerState(path)
	antigravitySummariesCache.Lock()
	defer antigravitySummariesCache.Unlock()
	now := antigravitySummariesNow()
	prior, cached := antigravitySummariesCache.stores[path]
	if cached && exists && prior.state == state &&
		now.Sub(prior.readAt) < SQLiteContainerDigestVerifyInterval {
		return prior.summaries
	}
	summaries := readAntigravitySummaries(ctx, path)
	if ctx.Err() != nil {
		return antigravitySummaries{err: ctx.Err()}
	}
	if summaries.err != nil && cached && prior.summaries.err == nil {
		// A damaged store may not even report a container state, so the
		// last good read stands in until a read succeeds again.
		return prior.summaries
	}
	if !exists {
		delete(antigravitySummariesCache.stores, path)
		return summaries
	}
	// Remote imports extract stores into fresh temp roots; bound what stays cached.
	if _, ok := antigravitySummariesCache.stores[path]; !ok && len(antigravitySummariesCache.stores) >= 16 {
		clear(antigravitySummariesCache.stores)
	}
	antigravitySummariesCache.stores[path] = antigravitySummariesSnapshot{
		state: state, summaries: summaries, readAt: now,
	}
	return summaries
}

// readAntigravitySummaries reads every conversation row. A missing store or
// table yields no rows, and a missing workspace or parent column yields rows
// without that value: builds that predate the columns record neither.
func readAntigravitySummaries(ctx context.Context, path string) antigravitySummaries {
	summaries := antigravitySummaries{byID: make(map[string]antigravitySummary)}
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return summaries
	}
	if err != nil {
		return antigravitySummaries{err: err}
	}
	if !info.Mode().IsRegular() {
		return summaries
	}
	observeSharedContainerScan(ctx)
	db, err := openSQLiteReadOnly(path, sqliteReadOptions{busyTimeoutMS: 3000})
	if err != nil {
		return antigravitySummaries{err: err}
	}
	defer db.Close()
	var hasID, hasWorkspaces, hasParent bool
	if err := db.QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM pragma_table_info('conversation_summaries') WHERE name = 'conversation_id'),
		EXISTS(SELECT 1 FROM pragma_table_info('conversation_summaries') WHERE name = 'workspace_uris'),
		EXISTS(SELECT 1 FROM pragma_table_info('conversation_summaries') WHERE name = 'parent_conversation_id')`,
	).Scan(&hasID, &hasWorkspaces, &hasParent); err != nil {
		return antigravitySummaries{err: err}
	}
	if !hasID || !hasWorkspaces && !hasParent {
		return summaries
	}
	workspaceColumn, parentColumn := "''", "''"
	if hasWorkspaces {
		workspaceColumn = "workspace_uris"
	}
	if hasParent {
		parentColumn = "parent_conversation_id"
	}
	rows, err := db.QueryContext(ctx, "SELECT conversation_id, COALESCE("+workspaceColumn+
		", ''), COALESCE("+parentColumn+", '') FROM conversation_summaries")
	if err != nil {
		return antigravitySummaries{err: err}
	}
	defer rows.Close()
	for rows.Next() {
		var id, uris, parentID string
		if err := rows.Scan(&id, &uris, &parentID); err != nil {
			return antigravitySummaries{err: err}
		}
		if id == "" {
			continue
		}
		summary := antigravitySummary{
			parentID: canonicalAgyCascadeID(strings.TrimSpace(parentID)),
		}
		if first := gjson.Get(uris, "0"); first.Type == gjson.String {
			summary.workspace = antigravityWorkspacePath(first.Str)
			summary.hasWorkspace = true
		}
		summaries.byID[id] = summary
	}
	if err := rows.Err(); err != nil {
		return antigravitySummaries{err: err}
	}
	return summaries
}

// antigravityWorkspacePath converts a recorded workspace URI to a local
// folder path, or returns "" when the URI does not name a local folder.
// Windows drive folders come back with backslashes, as Windows tools report
// them; a URI with a host names a UNC share.
func antigravityWorkspacePath(uri string) string {
	u, err := url.Parse(strings.TrimSpace(uri))
	if err != nil || !strings.EqualFold(u.Scheme, "file") || u.Path == "" {
		return ""
	}
	path := u.Path
	if len(path) > 1 {
		path = strings.TrimRight(path, "/")
	}
	if host := u.Host; host != "" && !strings.EqualFold(host, "localhost") {
		return `\\` + host + strings.ReplaceAll(path, "/", `\`)
	}
	if len(path) >= 3 && path[0] == '/' && path[2] == ':' && isASCIILetter(path[1]) {
		drive := path[1:]
		if len(drive) == 2 {
			drive += "/"
		}
		return strings.ReplaceAll(drive, "/", `\`)
	}
	if !strings.HasPrefix(path, "/") {
		return ""
	}
	return path
}

func isASCIILetter(b byte) bool {
	return b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z'
}

// antigravitySummaryRoutes holds, per store, the rows that change routing
// last saw, so a store write reparses only conversations whose workspace or
// parent changed. A title-only write routes nothing.
var antigravitySummaryRoutes = struct {
	sync.Mutex
	seen map[string]map[string]antigravitySummary
}{seen: make(map[string]map[string]antigravitySummary)}

// seedAntigravitySummaryRoutes records what discovery read as the routing
// baseline for a store that has none, so the first write after startup
// routes the conversations it changed. An existing baseline stays: a full
// sync reparses changed rows through their fingerprints, and moving the
// baseline here could hide a change from routing.
func seedAntigravitySummaryRoutes(root string, summaries antigravitySummaries) {
	if summaries.err != nil {
		return
	}
	path := filepath.Join(root, AntigravitySummariesDBName)
	antigravitySummaryRoutes.Lock()
	defer antigravitySummaryRoutes.Unlock()
	if _, ok := antigravitySummaryRoutes.seen[path]; !ok {
		antigravitySummaryRoutes.seen[path] = summaries.byID
	}
}

// changedAntigravitySummaryIDs returns, sorted, the conversations whose
// workspace or parent changed since routing last saw root's store, and makes
// the current rows the new baseline. A store with no baseline yet only gains
// one; the next full sync covers anything it changed.
func changedAntigravitySummaryIDs(ctx context.Context, root string) []string {
	summaries := loadAntigravitySummaries(ctx, root)
	if summaries.err != nil {
		return nil
	}
	path := filepath.Join(root, AntigravitySummariesDBName)
	antigravitySummaryRoutes.Lock()
	prior, ok := antigravitySummaryRoutes.seen[path]
	antigravitySummaryRoutes.seen[path] = summaries.byID
	antigravitySummaryRoutes.Unlock()
	if !ok {
		return nil
	}
	var ids []string
	for id, summary := range summaries.byID {
		if before, ok := prior[id]; !ok || before != summary {
			ids = append(ids, id)
		}
	}
	for id := range prior {
		if _, ok := summaries.byID[id]; !ok {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// IsAntigravitySummariesStore reports whether path is an Antigravity summary
// store or its WAL.
func IsAntigravitySummariesStore(path string) bool {
	return strings.TrimSuffix(filepath.Base(path), "-wal") == AntigravitySummariesDBName
}

// antigravitySummariesPath reports whether path is root's summary store or
// its WAL. The -shm index is never data; see ChangedPathRelevance.
func antigravitySummariesPath(root, path string) bool {
	rel, ok := relUnder(filepath.Clean(root), filepath.Clean(path))
	return ok && IsAntigravitySummariesStore(rel) && filepath.Dir(rel) == "."
}
