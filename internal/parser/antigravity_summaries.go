package parser

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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
// fingerprint covers both, so a store-only change reparses the conversation
// on the next full sync. Store writes are not routed on their own: a missed
// or failed read is corrected by a later sync.

// antigravitySummary is what one conversation's summary row says outside its
// transcript.
type antigravitySummary struct {
	// workspace is the first workspace folder as a local path, or "" when
	// that folder is not local. hasWorkspace reports whether any folder is
	// named.
	workspace    string
	hasWorkspace bool
	// workspaceRecorded reports whether the store records workspaces at all.
	workspaceRecorded bool
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
// workspace folder, leaves the parsed value. An unreadable store keeps the
// stored value until a later read succeeds.
func (s antigravitySummaries) cwdResolution(id string) SourceCwdResolution {
	if s.err != nil {
		return SourceCwdResolution{State: SourceCwdUnavailable}
	}
	summary := s.byID[id]
	switch {
	case !summary.workspaceRecorded:
		return SourceCwdResolution{}
	case !summary.hasWorkspace:
		// The row names no folder, so the conversation has none. This
		// clears a stored cwd even when a cwd allow-list filters the parse.
		return SourceCwdResolution{State: SourceCwdNone}
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

// fingerprintDigest condenses what the store says about id. A source
// fingerprint ends with it, so a parse can check that it read the same values
// the fingerprint covered.
// A conversation the store does not list adds nothing, so roots without a
// store keep their fingerprints. An unreadable store lists nothing, so the
// conversation reparses again once the store reads.
func (s antigravitySummaries) fingerprintDigest(id string) string {
	summary, ok := s.byID[id]
	if !ok {
		return ""
	}
	h := sha256.New()
	fmt.Fprintf(h, "%t\x00%t\x00%d\x00%s\x00%s\x00", summary.workspaceRecorded,
		summary.hasWorkspace, len(summary.workspace), summary.workspace, s.parent(id))
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// antigravityHashWithSummary appends a summary digest to a source hash.
func antigravityHashWithSummary(hash, digest string) string {
	return hash + "." + digest
}

// antigravityHashCoversSummary reports whether a source hash was taken with
// the summary values a parse read. When it was not, for example because the
// store became unreadable, disappeared, or changed between fingerprint and
// parse, the parse leaves its source hash unset and the next sync reparses.
// A store unreadable at both points matches, so it does not reparse every
// sync.
func antigravityHashCoversSummary(hash, digest string) bool {
	return hash != "" && strings.HasSuffix(hash, "."+digest)
}

type antigravitySummariesSnapshot struct {
	state     SQLiteContainerState
	summaries antigravitySummaries
	readAt    time.Time
}

var antigravitySummariesNow = time.Now

// antigravitySummariesFailureRetry is how long a failed read stands for the
// same store. It spans the reads of one sync pass, so a locked store is not
// waited on once per conversation, and is short enough that the next pass
// reads again.
const antigravitySummariesFailureRetry = 15 * time.Second

// Discovery, fingerprints, and parsing all ask for the same store, so one
// read serves them while the store's container state holds. Matching markers
// can hide a write, so a cached read expires, and a failed read expires
// sooner.
var antigravitySummariesCache = struct {
	sync.Mutex
	stores map[string]antigravitySummariesSnapshot
}{stores: make(map[string]antigravitySummariesSnapshot)}

// loadAntigravitySummaries reads the summary store under root.
func loadAntigravitySummaries(ctx context.Context, root string) antigravitySummaries {
	path := filepath.Join(root, AntigravitySummariesDBName)
	state, exists := StatSQLiteContainerState(path)
	antigravitySummariesCache.Lock()
	defer antigravitySummariesCache.Unlock()
	now := antigravitySummariesNow()
	if prior, ok := antigravitySummariesCache.stores[path]; ok && prior.state == state {
		ttl := SQLiteContainerDigestVerifyInterval
		if prior.summaries.err != nil {
			ttl = antigravitySummariesFailureRetry
		}
		if now.Sub(prior.readAt) < ttl {
			return prior.summaries
		}
	}
	summaries := readAntigravitySummaries(ctx, path)
	if ctx.Err() != nil {
		return summaries
	}
	if !exists && summaries.err == nil {
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
			parentID:          canonicalAgyCascadeID(strings.TrimSpace(parentID)),
			workspaceRecorded: hasWorkspaces,
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
