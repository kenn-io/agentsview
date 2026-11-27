package parser

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

// CodeBuddyCatalogDBName is the IDE 5.x conversation catalog kept beside the
// CBC projects directory. The CLI alone does not write it. The IDE's remote
// server writes the same file with only the side-question tables.
const CodeBuddyCatalogDBName = "codebuddy.db"

// codebuddyCatalogEntry is one local conversation's catalog metadata. An
// entry read from a catalog without the conversations table has no
// conversationID, so the IDE cannot open it.
type codebuddyCatalogEntry struct {
	conversationID string
	// runtimeID is the first of runtimeIDs.
	runtimeID string
	// runtimeIDs are the runtime session ids that can name this
	// conversation's transcript, in the order a fork parent is looked up:
	// the explicit runtime_session_id, then the local runtime mappings in
	// sorted order, then the conversation id when no explicit id is set.
	// The catalog indexes the entry by each of them.
	runtimeIDs  []string
	title       string
	customTitle string
	// forkParentRuntimeID is the first of forkParentRuntimeIDs.
	forkParentRuntimeID string
	// forkParentRuntimeIDs are the runtime ids of the conversation this
	// one forked from, or its conversation id when the catalog has no local
	// row for it. Parsing maps the first one with a transcript to the
	// parent's archive id, the filename of that transcript.
	forkParentRuntimeIDs []string
	deleted              bool
	btw                  []codebuddyBtwEntry
	discussions          []codebuddyDiscussionThread
}

// codebuddyBtwEntry is one /btw side question.
type codebuddyBtwEntry struct {
	id         string
	parentID   string
	question   string
	answer     string
	status     string
	errText    string
	imageCount int
	createdAt  time.Time
	updatedAt  time.Time
}

// codebuddyDiscussionThread is one selection-anchored discussion.
type codebuddyDiscussionThread struct {
	id        string
	quote     string
	entries   []codebuddyBtwEntry
	createdAt time.Time
}

// codebuddyCatalog is one read of codebuddy.db, keyed by the runtime session
// id that names the transcript file.
type codebuddyCatalog struct {
	byRuntime map[string]*codebuddyCatalogEntry
	state     SQLiteContainerState
}

var codebuddyCatalogCache = struct {
	sync.Mutex
	byPath map[string]codebuddyCatalogSnapshot
	// baseline holds each catalog's per-session row fingerprints as of the
	// last catalog change event, so the next event can name exactly the
	// sessions whose rows changed.
	baseline map[string]map[string]string
}{
	byPath:   make(map[string]codebuddyCatalogSnapshot),
	baseline: make(map[string]map[string]string),
}

type codebuddyCatalogSnapshot struct {
	catalog *codebuddyCatalog
	readAt  time.Time
}

// loadCodeBuddyCatalog reads the catalog, reusing the last read while the
// SQLite container markers are unchanged. A missing catalog returns nil. A
// catalog that exists but cannot be read falls back to the last good read in
// this process; without one it returns an error so the caller keeps the
// previously stored metadata and retries.
func loadCodeBuddyCatalog(ctx context.Context, path string) (*codebuddyCatalog, error) {
	return loadCodeBuddyCatalogFresh(ctx, path, false)
}

func loadCodeBuddyCatalogFresh(
	ctx context.Context, path string, fresh bool,
) (*codebuddyCatalog, error) {
	if path == "" {
		return nil, nil
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stat CodeBuddy catalog: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, nil
	}
	state, ok := StatSQLiteContainerState(path)
	codebuddyCatalogCache.Lock()
	prior, cached := codebuddyCatalogCache.byPath[path]
	codebuddyCatalogCache.Unlock()
	if !fresh && ok && cached && prior.catalog.state == state &&
		time.Since(prior.readAt) < SQLiteContainerDigestVerifyInterval {
		return prior.catalog, nil
	}
	catalog, err := readCodeBuddyCatalog(ctx, path)
	if err != nil {
		if cached && ctx.Err() == nil {
			log.Printf("codebuddy: catalog %s unreadable, keeping its last good read: %v", path, err)
			return prior.catalog, nil
		}
		return nil, err
	}
	catalog.state = state
	codebuddyCatalogCache.Lock()
	if len(codebuddyCatalogCache.byPath) >= 16 {
		clear(codebuddyCatalogCache.byPath)
		clear(codebuddyCatalogCache.baseline)
	}
	if ok {
		codebuddyCatalogCache.byPath[path] = codebuddyCatalogSnapshot{
			catalog: catalog, readAt: time.Now(),
		}
	}
	if _, has := codebuddyCatalogCache.baseline[path]; !has {
		codebuddyCatalogCache.baseline[path] = catalog.rowFingerprints()
	}
	codebuddyCatalogCache.Unlock()
	return catalog, nil
}

// codebuddyCatalogRowFingerprint returns the fingerprint of one session's
// catalog row; "absent" when the catalog does not exist.
func codebuddyCatalogRowFingerprint(
	ctx context.Context, path, runtimeID string,
) (string, error) {
	catalog, err := loadCodeBuddyCatalog(ctx, path)
	if err != nil {
		return "", err
	}
	return catalog.fingerprint(runtimeID), nil
}

// codebuddyCatalogChangedRuntimes rereads the catalog and returns the runtime
// ids whose rows changed, appeared, or disappeared since the previous change
// event. known is false when this process has no earlier read to compare.
func codebuddyCatalogChangedRuntimes(
	ctx context.Context, path string,
) (changed []string, known bool, err error) {
	codebuddyCatalogCache.Lock()
	before, known := codebuddyCatalogCache.baseline[path]
	codebuddyCatalogCache.Unlock()
	catalog, err := loadCodeBuddyCatalogFresh(ctx, path, true)
	if err != nil {
		return nil, false, err
	}
	after := catalog.rowFingerprints()
	codebuddyCatalogCache.Lock()
	codebuddyCatalogCache.baseline[path] = after
	codebuddyCatalogCache.Unlock()
	if !known {
		return nil, false, nil
	}
	for id, fp := range after {
		if before[id] != fp {
			changed = append(changed, id)
		}
	}
	for id := range before {
		if _, ok := after[id]; !ok {
			changed = append(changed, id)
		}
	}
	return changed, true, nil
}

// rowFingerprints fingerprints every local session row by runtime id.
func (c *codebuddyCatalog) rowFingerprints() map[string]string {
	out := make(map[string]string)
	if c == nil {
		return out
	}
	for id := range c.byRuntime {
		out[id] = c.fingerprint(id)
	}
	return out
}

func readCodeBuddyCatalog(ctx context.Context, path string) (*codebuddyCatalog, error) {
	db, err := openSQLiteReadOnly(path, sqliteReadOptions{busyTimeoutMS: 3000})
	if err != nil {
		return nil, fmt.Errorf("open CodeBuddy catalog: %w", err)
	}
	defer db.Close()
	tables, err := codebuddyCatalogTables(ctx, db)
	if err != nil {
		return nil, err
	}
	catalog := &codebuddyCatalog{byRuntime: make(map[string]*codebuddyCatalogEntry)}
	// The IDE's remote server keeps side questions without the
	// conversations table, keyed by the runtime session id that names the
	// transcript. Those rows get entries of their own.
	byConversation := make(map[string]*codebuddyCatalogEntry)
	payloadKeysAreRuntimeIDs := !tables["conversations"]
	if !payloadKeysAreRuntimeIDs {
		byConversation, err = readCodeBuddyConversations(ctx, db, catalog)
		if err != nil {
			return nil, err
		}
		if tables["conversation_runtime_sources"] {
			if err := readCodeBuddyRuntimeSources(ctx, db, catalog, byConversation); err != nil {
				return nil, err
			}
		}
		for _, entry := range byConversation {
			// runtimeID holds the explicit id until here.
			if entry.runtimeID == "" {
				entry.runtimeIDs = appendCodeBuddyRuntimeID(entry.runtimeIDs, entry.conversationID)
			}
			entry.runtimeID = entry.runtimeIDs[0]
		}
		// fork_source_id names the parent conversation; its transcript
		// records one of the parent's runtime session ids.
		for _, entry := range byConversation {
			if parent := byConversation[entry.forkParentRuntimeID]; parent != nil {
				entry.forkParentRuntimeID = parent.runtimeID
				entry.forkParentRuntimeIDs = parent.runtimeIDs
			}
		}
	}
	entryFor := func(key string) *codebuddyCatalogEntry {
		if entry := byConversation[key]; entry != nil || !payloadKeysAreRuntimeIDs {
			return entry
		}
		entry := &codebuddyCatalogEntry{runtimeID: key, runtimeIDs: []string{key}}
		byConversation[key] = entry
		catalog.byRuntime[key] = entry
		return entry
	}
	for table, apply := range map[string]func(*codebuddyCatalogEntry, string){
		"btw_entries": func(e *codebuddyCatalogEntry, payload string) {
			e.btw = parseCodeBuddyBtwPayload(payload)
		},
		"discussion_threads": func(e *codebuddyCatalogEntry, payload string) {
			e.discussions = parseCodeBuddyDiscussionPayload(payload)
		},
	} {
		if !tables[table] {
			continue
		}
		if err := readCodeBuddyPayloads(ctx, db, table, entryFor, apply); err != nil {
			return nil, err
		}
	}
	return catalog, nil
}

func codebuddyCatalogTables(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		return nil, fmt.Errorf("list CodeBuddy catalog tables: %w", err)
	}
	defer rows.Close()
	tables := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		tables[name] = true
	}
	return tables, rows.Err()
}

func codebuddyCatalogColumns(ctx context.Context, db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		cols[name] = true
	}
	return cols, rows.Err()
}

// readCodeBuddyConversations loads local conversations. Rows owned by a
// remote workspace describe sessions stored on another machine.
func readCodeBuddyConversations(
	ctx context.Context, db *sql.DB, catalog *codebuddyCatalog,
) (map[string]*codebuddyCatalogEntry, error) {
	cols, err := codebuddyCatalogColumns(ctx, db, "conversations")
	if err != nil {
		return nil, err
	}
	optional := func(name string) string {
		if cols[name] {
			return "COALESCE(" + name + ", '')"
		}
		return "''"
	}
	optionalNull := func(name string) string {
		if cols[name] {
			return name + " IS NOT NULL"
		}
		return "0"
	}
	local := "1"
	if cols["workspace_authority"] {
		local = "COALESCE(workspace_authority, '') = ''"
	}
	query := `SELECT id, ` + optional("runtime_session_id") + `, ` +
		optional("title") + `, ` + optional("custom_title") + `, ` +
		optional("fork_source_id") + `, ` + optionalNull("deleted_at") +
		` FROM conversations WHERE ` + local
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("read CodeBuddy conversations: %w", err)
	}
	defer rows.Close()
	byConversation := make(map[string]*codebuddyCatalogEntry)
	for rows.Next() {
		var e codebuddyCatalogEntry
		var forkSource string
		if err := rows.Scan(&e.conversationID, &e.runtimeID,
			&e.title, &e.customTitle, &forkSource, &e.deleted); err != nil {
			return nil, err
		}
		e.title = strings.TrimSpace(e.title)
		e.customTitle = strings.TrimSpace(e.customTitle)
		e.forkParentRuntimeID = strings.TrimSpace(forkSource)
		if e.forkParentRuntimeID != "" {
			e.forkParentRuntimeIDs = []string{e.forkParentRuntimeID}
		}
		key := e.runtimeID
		if key == "" {
			key = e.conversationID
		} else {
			e.runtimeIDs = []string{key}
		}
		entry := &e
		byConversation[e.conversationID] = entry
		catalog.byRuntime[key] = entry
	}
	return byConversation, rows.Err()
}

// readCodeBuddyRuntimeSources adds each local runtime mapping to its
// conversation's runtime ids, in sorted order so the lookup order does not
// depend on how the rows were written.
func readCodeBuddyRuntimeSources(
	ctx context.Context, db *sql.DB, catalog *codebuddyCatalog,
	byConversation map[string]*codebuddyCatalogEntry,
) error {
	rows, err := db.QueryContext(ctx, `SELECT runtime_session_id, conversation_id
		FROM conversation_runtime_sources WHERE COALESCE(authority, '') = ''
		ORDER BY runtime_session_id, conversation_id`)
	if err != nil {
		return fmt.Errorf("read CodeBuddy runtime sources: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var runtime, conversation string
		if err := rows.Scan(&runtime, &conversation); err != nil {
			return err
		}
		if entry := byConversation[conversation]; entry != nil && runtime != "" {
			entry.runtimeIDs = appendCodeBuddyRuntimeID(entry.runtimeIDs, runtime)
			catalog.byRuntime[runtime] = entry
		}
	}
	return rows.Err()
}

func appendCodeBuddyRuntimeID(ids []string, id string) []string {
	if slices.Contains(ids, id) {
		return ids
	}
	return append(ids, id)
}

func readCodeBuddyPayloads(
	ctx context.Context, db *sql.DB, table string,
	entryFor func(string) *codebuddyCatalogEntry,
	apply func(*codebuddyCatalogEntry, string),
) error {
	rows, err := db.QueryContext(ctx,
		`SELECT conversation_id, payload FROM `+table)
	if err != nil {
		return fmt.Errorf("read CodeBuddy %s: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var conversation, payload string
		if err := rows.Scan(&conversation, &payload); err != nil {
			return err
		}
		if conversation == "" {
			continue
		}
		if entry := entryFor(conversation); entry != nil {
			apply(entry, payload)
		}
	}
	return rows.Err()
}

func parseCodeBuddyBtwPayload(payload string) []codebuddyBtwEntry {
	var out []codebuddyBtwEntry
	gjson.Parse(payload).ForEach(func(_, e gjson.Result) bool {
		if entry, ok := codebuddyBtwEntryFrom(e); ok {
			out = append(out, entry)
		}
		return true
	})
	return out
}

func codebuddyBtwEntryFrom(e gjson.Result) (codebuddyBtwEntry, bool) {
	id := e.Get("id").Str
	question := strings.TrimSpace(e.Get("question").Str)
	if id == "" || question == "" {
		return codebuddyBtwEntry{}, false
	}
	return codebuddyBtwEntry{
		id: id, parentID: e.Get("parentId").Str, question: question,
		answer: strings.TrimSpace(e.Get("answer").Str),
		status: e.Get("status").Str, errText: strings.TrimSpace(e.Get("error").Str),
		imageCount: int(e.Get("imageCount").Int()),
		createdAt:  codebuddyCBCTimestamp(e.Get("createdAt")),
		updatedAt:  codebuddyCBCTimestamp(e.Get("updatedAt")),
	}, true
}

func parseCodeBuddyDiscussionPayload(payload string) []codebuddyDiscussionThread {
	var out []codebuddyDiscussionThread
	gjson.Parse(payload).ForEach(func(_, t gjson.Result) bool {
		thread := codebuddyDiscussionThread{
			id: t.Get("id").Str, quote: t.Get("anchor.quote").Str,
			createdAt: codebuddyCBCTimestamp(t.Get("createdAt")),
		}
		t.Get("entries").ForEach(func(_, e gjson.Result) bool {
			if entry, ok := codebuddyBtwEntryFrom(e); ok {
				thread.entries = append(thread.entries, entry)
			}
			return true
		})
		if thread.id != "" && len(thread.entries) > 0 {
			out = append(out, thread)
		}
		return true
	})
	return out
}

// codebuddyCatalogPathForProjects returns the catalog beside a CBC projects
// root, or "" for a root that does not look like a CodeBuddy home layout.
func codebuddyCatalogPathForProjects(projectsRoot string) string {
	if filepath.Base(filepath.Clean(projectsRoot)) != "projects" {
		return ""
	}
	return filepath.Join(filepath.Dir(filepath.Clean(projectsRoot)), CodeBuddyCatalogDBName)
}

// CodeBuddyCBCProjectsRoots returns the CLI and IDE 5.x projects directories
// one configured CodeBuddy root contributes, so remote sync can carry their
// catalog and images beside them.
func CodeBuddyCBCProjectsRoots(root string) []string {
	_, cbc := splitCodeBuddyRoots([]string{root})
	return cbc
}

// CodeBuddyCatalogForProjects returns the IDE catalog path beside a CBC
// projects directory.
func CodeBuddyCatalogForProjects(projectsRoot string) string {
	return codebuddyCatalogPathForProjects(projectsRoot)
}

// CodeBuddyIDEConversationID returns the IDE conversation id that owns a CLI
// runtime session, read from the catalog beside the transcript. ok is false
// when the transcript has no catalog or the catalog has no local row for the
// session, so the IDE cannot open it.
func CodeBuddyIDEConversationID(
	ctx context.Context, transcriptPath, runtimeID string,
) (string, bool, error) {
	root, ok := codebuddyCBCRootFromPath(transcriptPath)
	if !ok || runtimeID == "" {
		return "", false, nil
	}
	catalog, err := loadCodeBuddyCatalog(ctx, codebuddyCatalogPathForProjects(root))
	if err != nil || catalog == nil {
		return "", false, err
	}
	entry := catalog.byRuntime[runtimeID]
	if entry == nil || entry.deleted || entry.conversationID == "" {
		return "", false, nil
	}
	return entry.conversationID, true, nil
}
