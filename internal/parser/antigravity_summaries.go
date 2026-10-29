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
// whose store facts changed. A store write routes only the conversations
// whose archived values differ from what the store now says (see
// SummaryStoreExpectations).

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
// the store exists but cannot be read. missing is set when the store does not
// exist and was not read before in this process, so it may have been removed.
type antigravitySummaries struct {
	byID    map[string]antigravitySummary
	err     error
	missing bool
}

// cwdResolution describes what the store knows about a conversation's local
// working directory. A conversation the store does not list, or lists with no
// workspace folder, leaves the parsed value alone; an unreadable store keeps
// the stored one.
func (s antigravitySummaries) cwdResolution(id string) SourceCwdResolution {
	if s.err != nil || s.missing {
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

// writeFingerprint adds what a readable store says about id to a source
// fingerprint. A conversation the store does not list adds nothing, so
// roots without a store keep their fingerprints.
func (s antigravitySummaries) writeFingerprint(w io.Writer, id string) error {
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
// that was read before cannot be read or has disappeared, its last good read
// stands in, so an outage neither changes fingerprints nor clears stored
// workspaces and parents.
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
	if (summaries.err != nil || !exists) && cached && prior.summaries.err == nil {
		// A damaged store may not even report a container state, and a
		// removed one reads as empty, so the last good read stands in until
		// a read succeeds again.
		return prior.summaries
	}
	if !exists {
		summaries.missing = summaries.err == nil
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

// SummaryStoreExpectation is what a parse would store for one session whose
// conversation a summary store lists.
type SummaryStoreExpectation struct {
	Source    SourceRef
	SessionID string
	// CwdKnown reports whether the store decides the session's working
	// directory; Cwd is then the value a parse stores.
	Cwd      string
	CwdKnown bool
	// ParentKnown reports whether the store decides the session's parent;
	// ParentSessionID is then the parent a parse stores, or "" for none.
	ParentSessionID string
	ParentKnown     bool
}

// SummaryStoreExpectationLister lists, for a summary store write, every
// session its conversations feed, with the values a parse would store. The
// engine routes the sessions whose archived values differ, so a store write
// reparses only what it changed, whatever the process saw before.
type SummaryStoreExpectationLister interface {
	SummaryStoreExpectations(ctx context.Context, storePath string) ([]SummaryStoreExpectation, error)
}

// summaryExpectation describes source's session from the store's facts.
func summaryExpectation(source SourceRef, sessionID, parentID string, parentKnown bool) SummaryStoreExpectation {
	expectation := SummaryStoreExpectation{
		Source: source, SessionID: sessionID, ParentSessionID: parentID, ParentKnown: parentKnown,
	}
	switch source.CwdResolution.State {
	case SourceCwdResolved:
		expectation.Cwd, expectation.CwdKnown = source.CwdResolution.Path, true
	case SourceCwdAmbiguous, SourceCwdRemote:
		expectation.CwdKnown = true
	case SourceCwdUnspecified, SourceCwdNone, SourceCwdUnavailable:
		// The store does not decide this session's working directory.
	}
	return expectation
}

// AntigravitySummarySessionRoot returns the configured root that owns an
// archived session path of agent, so a store write in one root never routes a
// same-ID session archived from another.
func AntigravitySummarySessionRoot(agent AgentType, path string) (string, bool) {
	if agent == AgentAntigravity {
		return AntigravitySessionRoot(path)
	}
	if agent == AgentAntigravityCLI {
		root := filepath.Dir(filepath.Dir(filepath.Clean(path)))
		_, ok := antigravityCLISessionIDForPath(root, path)
		return root, ok
	}
	return "", false
}
