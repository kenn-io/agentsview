// ABOUTME: Multi-session container provider for omnigent: one chat.db fanned
// ABOUTME: out into one session per conversation, with incremental sync.
package parser

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
)

// omnigentChangeTracker remembers each container's schema plus bounded rowid
// high-water marks for the watcher path: newly inserted conversations and
// immutable items. Metadata-only edits with no new rows are deferred to the
// scheduled container pass, whose composite fingerprint gate observes every
// byte change. Cold and schema-changed containers retain a whole-container
// backstop.
type omnigentTrackedContainer struct {
	schema            omnigentSchema
	conversationRowID int64
	conversationTail  string
	itemRowID         int64
	itemTail          string
}

const omnigentChangePageSize = 128

type omnigentChangeTracker struct {
	mu         sync.Mutex
	containers map[string]omnigentTrackedContainer
}

type omnigentSourceSet struct {
	multiSessionContainerSourceSet
	tracker *omnigentChangeTracker
}

func newOmnigentChangeTracker() *omnigentChangeTracker {
	return &omnigentChangeTracker{
		containers: make(map[string]omnigentTrackedContainer),
	}
}

// Omnigent stores every conversation in one shared SQLite database (chat.db).
// It is a multi-session container provider: discovery surfaces the database as
// one source whose parse fans out into one session per conversation, addressed
// by "<db>#<conversationID>" virtual paths. Watcher events fan out only the
// members changed since the tracker's last floor; scheduled and full passes
// treat the container as one candidate gated by its composite fingerprint.
func newOmnigentProviderFactory(def AgentDef) ProviderFactory {
	tracker := newOmnigentChangeTracker()
	return NewSourceSetFactory(
		def,
		omnigentProviderCapabilities(),
		func(cfg ProviderConfig) SourceSet {
			return newOmnigentSourceSet(cfg, tracker)
		},
	)
}

// newOmnigentSourceSet fills the multi-session base config directly instead of
// going through NewMultiSessionContainerSourceSet: omnigent implements Parse
// and SourcesForChangedPath first-class below, so the option constructor's
// parse-hook validation does not apply.
func newOmnigentSourceSet(
	cfg ProviderConfig, tracker *omnigentChangeTracker,
) omnigentSourceSet {
	return omnigentSourceSet{
		agent: AgentOmnigent,
		roots: cleanJSONLRoots(cfg.Roots),
		cfg: multiSessionConfig{
			discoverContainers: omnigentDiscoverContainers,
			watchRoots:         omnigentWatchRoots,
			classifyPath:       omnigentClassifyPath,
			findMember:         omnigentFindMember,
			fingerprint:        omnigentFingerprintSource,
			memberPresent:      omnigentMemberPresent,
		},
		tracker: tracker,
	}
}

// SourcesForChangedPath resolves one filesystem event directly to the affected
// members via the change tracker, so a shared container fans out a bounded
// changed batch instead of reparsing every member per event.
func (s omnigentSourceSet) SourcesForChangedPath(
	ctx context.Context, req ChangedPathRequest,
) ([]SourceRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, root := range s.roots {
		matches, err := s.tracker.changedMembers(ctx, root, req)
		if err != nil {
			return nil, err
		}
		if len(matches) == 0 {
			continue
		}
		sources := make([]SourceRef, 0, len(matches))
		for _, match := range matches {
			sources = append(sources, s.sourceRef(root, match))
		}
		return sources, nil
	}
	return nil, nil
}

// Parse parses one member source into one result, or fans a whole container
// out into every conversation while seeding the change tracker's floor. Member
// results keep the semantic per-conversation hash produced by the parser; the
// container's file hash is never stamped over it. A schema the parser
// deliberately does not support is a clean unsupported skip, not a failure.
func (s omnigentSourceSet) Parse(
	ctx context.Context, req ParseRequest,
) (ParseOutcome, error) {
	if err := ctx.Err(); err != nil {
		return ParseOutcome{}, err
	}
	src, ok := s.sourceFromRef(req.Source)
	if !ok {
		return ParseOutcome{}, fmt.Errorf("%s source path unavailable", s.agent)
	}
	if src.MemberID != "" {
		result, err := omnigentParseMember(ctx, src, req)
		if err != nil {
			if omnigentSchemaUnsupported(err) {
				return unsupportedMultiSessionOutcome(), nil
			}
			return ParseOutcome{}, err
		}
		if result == nil {
			return s.skipOutcome(src), nil
		}
		return ParseOutcome{
			Results: []ParseResultOutcome{{
				Result:      *result,
				DataVersion: DataVersionCurrent,
			}},
			ResultSetComplete: true,
			ForceReplace:      true,
		}, nil
	}
	results, err := s.tracker.parseContainer(ctx, src, req)
	if err != nil {
		if omnigentSchemaUnsupported(err) {
			return unsupportedMultiSessionOutcome(), nil
		}
		return ParseOutcome{}, err
	}
	if len(results) == 0 {
		return s.skipOutcome(src), nil
	}
	out := make([]ParseResultOutcome, 0, len(results))
	for i := range results {
		out = append(out, ParseResultOutcome{
			Result:      results[i],
			DataVersion: DataVersionCurrent,
		})
	}
	return ParseOutcome{
		Results:           out,
		ResultSetComplete: true,
		ForceReplace:      true,
	}, nil
}

func (s omnigentSourceSet) RestoreCachedSourceState(
	ctx context.Context, source SourceRef,
) (bool, error) {
	src, ok := source.Opaque.(multiSessionSource)
	if !ok || src.MemberID != "" || src.Container == "" {
		return false, nil
	}
	return s.tracker.restoreCachedContainer(ctx, src.Container)
}

// RestoreOmnigentCachedSourceState rebuilds omnigent's bounded change tracker
// when the sync engine validates a persisted whole-container cache entry
// without parsing, so the first watcher event after restart is not treated as
// a cold whole-container discovery. It reports true when tracker state was
// restored; the caller must then refingerprint the source to catch a commit
// racing restoration. The engine calls this only for AgentOmnigent sources.
// Provider decorators (test wrappers) are reached through the structural
// fallback and forward here with the provider they wrap.
func RestoreOmnigentCachedSourceState(
	ctx context.Context, provider Provider, source SourceRef,
) (bool, error) {
	if sp, ok := provider.(*SourceSetProvider); ok {
		set, ok := sp.sources.(omnigentSourceSet)
		if !ok {
			return false, nil
		}
		return set.RestoreCachedSourceState(ctx, source)
	}
	if restorer, ok := provider.(interface {
		RestoreCachedSourceState(context.Context, SourceRef) (bool, error)
	}); ok {
		return restorer.RestoreCachedSourceState(ctx, source)
	}
	return false, nil
}

// IsOmnigentContainerSource reports whether source addresses a whole omnigent
// chat.db container rather than one "<db>#<conversationID>" virtual member.
// The sync engine special-cases whole containers directly: they persist the
// archive when the physical database vanishes, and a complete container parse
// owns its member set.
func IsOmnigentContainerSource(source SourceRef) bool {
	if source.Provider != AgentOmnigent {
		return false
	}
	path := providerSourcePath(source)
	if path == "" {
		return false
	}
	_, _, virtual := parseOmnigentVirtualPath(path)
	return !virtual
}

// OmnigentMemberSessionID returns the raw archived session identity addressed
// by a virtual omnigent member source. The sync engine uses it to look up
// already archived descendants whose cwd and branch inherit from a changed
// root; it applies any remote ID prefix before querying the archive.
func OmnigentMemberSessionID(source SourceRef) (string, bool) {
	if source.Provider != AgentOmnigent {
		return "", false
	}
	path := providerSourcePath(source)
	if path == "" {
		return "", false
	}
	_, member, ok := parseOmnigentVirtualPath(path)
	if !ok {
		return "", false
	}
	return omnigentIDPrefix + member, true
}

func providerSourcePath(source SourceRef) string {
	for _, path := range []string{
		source.DisplayPath, source.FingerprintKey, source.Key,
	} {
		if path != "" {
			return path
		}
	}
	return ""
}

func omnigentProviderCapabilities() Capabilities {
	source := multiSessionContainerSourceCapabilities(
		CapabilitySupported,
		CapabilityUnsupported,
	)
	source.PersistentArchive = CapabilitySupported
	return Capabilities{
		Source: source,
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			SessionName:          CapabilitySupported,
			Cwd:                  CapabilitySupported,
			GitBranch:            CapabilitySupported,
			Relationships:        CapabilitySupported,
			Subagents:            CapabilitySupported,
			Thinking:             CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			AggregateUsageEvents: CapabilitySupported,
		},
		Sync: ProviderSyncSemantics{
			FingerprintHashInCacheKey:           true,
			FingerprintHashRequiredForFreshness: true,
			UnchangedResults:                    UnchangedResultMTimeAndHash,
		},
	}
}

func omnigentDiscoverContainers(root string) []string {
	if dbPath := omnigentDBPath(root); dbPath != "" {
		return []string{dbPath}
	}
	return nil
}

func omnigentWatchRoots(roots []string) []WatchRoot {
	out := make([]WatchRoot, 0, len(roots))
	for _, root := range roots {
		out = append(out, WatchRoot{
			Path:         root,
			Recursive:    false,
			IncludeGlobs: []string{omnigentDBName, omnigentDBName + "-*"},
			DebounceKey:  string(AgentOmnigent) + ":db:" + root,
		})
	}
	return out
}

// omnigentClassifyPath maps a stored or changed path to its database container
// and conversation. allowMissing relaxes the regular-file requirement so a
// database delete (or its WAL/SHM sibling) still classifies for tombstones.
// Unlike Zed and Shelley, Omnigent rejects a bare "-shm" sibling event: the
// provider's own read connections update that file's mtime, so treating it as
// a source change would make every scan trigger the next one.
func omnigentClassifyPath(
	root, path string, allowMissing bool,
) (multiSessionMatch, bool) {
	return classifySQLiteContainerPath(
		root, path, omnigentDBName, allowMissing, true, parseOmnigentVirtualPath,
	)
}

func omnigentFindMember(root, rawID string) (multiSessionMatch, bool) {
	if root == "" {
		return multiSessionMatch{}, false
	}
	dbPath := omnigentDBPath(root)
	if dbPath == "" || !omnigentConversationExists(dbPath, rawID) {
		return multiSessionMatch{}, false
	}
	return multiSessionMatch{
		Path:      VirtualSourcePath(dbPath, rawID),
		Container: dbPath,
		MemberID:  rawID,
	}, true
}

func omnigentFingerprintSource(src multiSessionSource) (SourceFingerprint, error) {
	info, err := os.Stat(src.Container)
	if err != nil {
		if os.IsNotExist(err) {
			return SourceFingerprint{}, nil
		}
		return SourceFingerprint{}, fmt.Errorf("stat %s: %w", src.Container, err)
	}
	fingerprint := SourceFingerprint{
		Size:    info.Size(),
		MTimeNS: info.ModTime().UnixNano(),
	}
	if src.MemberID == "" {
		if compositeMtime, err := sqliteDBCompositeMtime(
			src.Container, omnigentDBMtimeSuffixes,
		); err == nil {
			fingerprint.MTimeNS = compositeMtime
		}
		fingerprint.Hash, err = hashJSONLSourceFile(src.Container)
		if err != nil {
			return SourceFingerprint{}, err
		}
		return fingerprint, nil
	}

	conn, err := openOmnigentDB(src.Container)
	if err != nil {
		return SourceFingerprint{}, err
	}
	defer conn.Close()
	schema, err := detectOmnigentSchema(conn)
	if err != nil {
		return SourceFingerprint{}, err
	}
	// A member ID that no longer parses under the detected schema identifies
	// a retired legacy member (pre-schema-change identity), not a failure.
	member, err := omnigentMemberForSchema(src.MemberID)
	if err != nil {
		return SourceFingerprint{}, nil
	}
	meta, ok, err := loadOmnigentConversationMeta(conn, schema, member)
	if err != nil {
		return SourceFingerprint{}, err
	}
	if ok {
		fingerprint.MTimeNS = meta.updatedAt * int64(1_000_000_000)
		fingerprint.Hash = meta.fingerprint()
		return fingerprint, nil
	}
	// Conversation row is gone but the DB file remains: return a keyed-empty
	// fingerprint without error so the engine proceeds to Parse, which
	// force-replaces the deleted session out of the archive.
	return SourceFingerprint{}, nil
}

func loadOmnigentConversationMeta(
	conn *sql.DB, schema omnigentSchema, member omnigentMemberID,
) (omnigentMeta, bool, error) {
	query := omnigentConversationAggregateQuery(
		schema, "conversations", "WHERE c.workspace_id = ? AND c.id = ?",
	)
	args := []any{member.workspaceID, omnigentIDArg(schema, member.rawID)}
	var meta omnigentMeta
	err := conn.QueryRow(query, args...).Scan(
		&meta.rowID, &meta.workspaceID, &meta.rawID, &meta.updatedAt,
		&meta.itemCount, &meta.maxPosition,
	)
	if err == sql.ErrNoRows {
		return omnigentMeta{}, false, nil
	}
	if err != nil {
		return omnigentMeta{}, false, fmt.Errorf("loading omnigent conversation meta: %w", err)
	}
	return meta, true, nil
}

func (t *omnigentChangeTracker) changedMembers(
	ctx context.Context, root string, req ChangedPathRequest,
) ([]multiSessionMatch, error) {
	match, ok := omnigentClassifyPath(root, req.Path, true)
	if !ok {
		return nil, nil
	}
	if match.MemberID != "" || !IsRegularFile(match.Container) {
		return []multiSessionMatch{match}, nil
	}
	conn, err := openOmnigentDB(match.Container)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	schema, err := detectOmnigentSchema(conn)
	if err != nil {
		if omnigentSchemaUnsupported(err) {
			return []multiSessionMatch{match}, nil
		}
		return nil, err
	}
	return t.matchesSince(ctx, conn, schema, match)
}

func (t *omnigentChangeTracker) matchesSince(
	ctx context.Context, conn *sql.DB, schema omnigentSchema,
	match multiSessionMatch,
) ([]multiSessionMatch, error) {
	t.mu.Lock()
	tracked, warm := t.containers[match.Container]
	t.mu.Unlock()
	if !warm || tracked.schema != schema {
		// A cold or schema-changed container parses whole: the complete
		// result set reconciles archived membership and seeds the floor.
		return []multiSessionMatch{match}, nil
	}
	return t.splitSchemaMatchesSince(ctx, conn, schema, match, tracked)
}

// splitSchemaMatchesSince fans out the members changed since the tracked rowid
// high-water marks: newly inserted conversations plus the conversations owning
// newly inserted items. Metadata-only edits insert no rows and are invisible
// here; they are deferred to the scheduled container pass, whose composite
// fingerprint observes every byte change.
func (t *omnigentChangeTracker) splitSchemaMatchesSince(
	ctx context.Context, conn *sql.DB, schema omnigentSchema,
	match multiSessionMatch, tracked omnigentTrackedContainer,
) ([]multiSessionMatch, error) {
	conversationIDExprs := omnigentConversationIDExprs(schema)
	conversationCursor, conversationTail, reconcile, err :=
		normalizeOmnigentRowIDCursor(
			ctx, conn, tracked.conversationRowID, tracked.conversationTail,
			func(rowID int64) (string, bool, error) {
				return omnigentRowIdentityAt(
					ctx, conn, omnigentConversationsTable, conversationIDExprs, rowID,
				)
			},
			func() (int64, string, error) {
				return omnigentLatestRowIdentity(
					ctx, conn, omnigentConversationsTable, conversationIDExprs,
				)
			},
		)
	if err != nil {
		return nil, err
	}
	if reconcile {
		return []multiSessionMatch{match}, nil
	}
	newConversations, conversationRowID, conversationTail, err :=
		listOmnigentNewConversationMetas(
			ctx, conn, schema, conversationCursor, conversationTail,
		)
	if err != nil {
		return nil, err
	}
	itemIDExprs := omnigentItemIDExprs(schema)
	itemCursor, itemTail, reconcile, err := normalizeOmnigentRowIDCursor(
		ctx, conn, tracked.itemRowID, tracked.itemTail,
		func(rowID int64) (string, bool, error) {
			return omnigentRowIdentityAt(
				ctx, conn, omnigentItemsTable, itemIDExprs, rowID,
			)
		},
		func() (int64, string, error) {
			return omnigentLatestRowIdentity(ctx, conn, omnigentItemsTable, itemIDExprs)
		},
	)
	if err != nil {
		return nil, err
	}
	if reconcile {
		return []multiSessionMatch{match}, nil
	}
	members, itemRowID, itemTail, err := listOmnigentNewItemMembers(
		ctx, conn, schema, itemCursor, itemTail,
	)
	if err != nil {
		return nil, err
	}
	changed := newConversations
	seen := make(map[string]struct{}, len(changed)+len(members))
	for _, meta := range changed {
		seen[meta.member().key()] = struct{}{}
	}
	for _, member := range members {
		key := member.key()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		meta, present, loadErr := loadOmnigentConversationMeta(
			conn, schema, member,
		)
		if loadErr != nil {
			return nil, loadErr
		}
		if present {
			changed = append(changed, meta)
		}
	}

	t.mu.Lock()
	if current, ok := t.containers[match.Container]; ok &&
		current.schema == schema {
		current.conversationRowID = conversationRowID
		current.conversationTail = conversationTail
		current.itemRowID = itemRowID
		current.itemTail = itemTail
		t.containers[match.Container] = current
	}
	t.mu.Unlock()
	return omnigentMatches(match.Container, schema, changed), nil
}

func normalizeOmnigentRowIDCursor(
	ctx context.Context,
	conn *sql.DB,
	trackedRowID int64,
	trackedIdentity string,
	identityAt func(int64) (string, bool, error),
	latest func() (int64, string, error),
) (int64, string, bool, error) {
	latestRowID, latestIdentity, err := latest()
	if err != nil {
		return 0, "", false, err
	}
	if trackedRowID == 0 {
		return 0, "", false, nil
	}
	if latestRowID < trackedRowID {
		// Tail deletion, VACUUM, and table rebuilds can lower implicit rowids.
		// No bounded cursor can distinguish pure deletion from a
		// delete-many/insert-fewer rewrite that reused several lower rowids.
		// Request one authoritative container rebuild rather than silently
		// adopting an incomplete boundary.
		return 0, "", true, nil
	}
	var currentIdentity string
	var present bool
	if latestRowID == trackedRowID {
		currentIdentity, present = latestIdentity, true
	} else {
		currentIdentity, present, err = identityAt(trackedRowID)
		if err != nil {
			return 0, "", false, err
		}
	}
	if !present || currentIdentity != trackedIdentity {
		// SQLite may reuse a deleted maximum rowid. Replay that one boundary
		// row plus any following inserts; this stays proportional to the changed
		// tail rather than rescanning the table.
		return max(trackedRowID-1, 0), "", false, nil
	}
	return trackedRowID, trackedIdentity, false, nil
}

// omnigentConversationsTable and omnigentItemsTable name the two tables the
// bounded change tracker reads row identities from.
const (
	omnigentConversationsTable = "conversations"
	omnigentItemsTable         = "conversation_items"
)

// omnigentConversationIDExprs and omnigentItemIDExprs give
// omnigentRowIdentityAt/omnigentLatestRowIdentity the schema-resolved id
// column list for each table: one column (id) for conversations, two
// (conversation_id, id) for conversation_items.
func omnigentConversationIDExprs(schema omnigentSchema) []string {
	return []string{omnigentIDExpr(schema, "id")}
}

func omnigentItemIDExprs(schema omnigentSchema) []string {
	return []string{
		omnigentIDExpr(schema, "conversation_id"),
		omnigentIDExpr(schema, "id"),
	}
}

// omnigentRowIdentityAt resolves the bounded change-tracker identity string
// ("workspaceID:id[:itemID]") for one row addressed by its SQLite rowid, in
// either conversations or conversation_items. It reports false when the row
// no longer exists (deleted, or not yet reused by a later insert).
func omnigentRowIdentityAt(
	ctx context.Context, conn *sql.DB, table string, idExprs []string, rowID int64,
) (string, bool, error) {
	if rowID == 0 {
		return "", false, nil
	}
	query := `SELECT workspace_id, ` + strings.Join(idExprs, ", ") +
		` FROM ` + table + ` WHERE rowid = ?`
	var workspaceID int64
	ids := make([]string, len(idExprs))
	dest := append([]any{&workspaceID}, omnigentStringDests(ids)...)
	err := conn.QueryRowContext(ctx, query, rowID).Scan(dest...)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf(
			"reading omnigent %s row identity: %w", table, err,
		)
	}
	return omnigentRowIdentityKey(workspaceID, ids), true, nil
}

// omnigentLatestRowIdentity resolves the SQLite rowid and identity of the
// most recently inserted row in either conversations or conversation_items.
func omnigentLatestRowIdentity(
	ctx context.Context, conn *sql.DB, table string, idExprs []string,
) (int64, string, error) {
	query := `SELECT rowid, workspace_id, ` + strings.Join(idExprs, ", ") +
		` FROM ` + table + ` ORDER BY rowid DESC LIMIT 1`
	row := conn.QueryRowContext(ctx, query)
	var rowID int64
	var workspaceID int64
	ids := make([]string, len(idExprs))
	dest := append([]any{&rowID, &workspaceID}, omnigentStringDests(ids)...)
	err := row.Scan(dest...)
	if err == sql.ErrNoRows {
		return 0, "", nil
	}
	if err != nil {
		return 0, "", fmt.Errorf("reading latest omnigent %s row: %w", table, err)
	}
	return rowID, omnigentRowIdentityKey(workspaceID, ids), nil
}

// omnigentStringDests returns a pointer to each element of ids, in order, for
// use as variadic sql.Row/sql.Rows Scan destinations.
func omnigentStringDests(ids []string) []any {
	dest := make([]any, len(ids))
	for i := range ids {
		dest[i] = &ids[i]
	}
	return dest
}

// omnigentRowIdentityKey joins a workspace ID with one or more id columns
// into the tracker's colon-separated identity string.
func omnigentRowIdentityKey(workspaceID int64, ids []string) string {
	parts := make([]string, 0, len(ids)+1)
	parts = append(parts, strconv.FormatInt(workspaceID, 10))
	parts = append(parts, ids...)
	return strings.Join(parts, ":")
}

func listOmnigentNewConversationMetas(
	ctx context.Context, conn *sql.DB, schema omnigentSchema,
	afterRowID int64, afterIdentity string,
) ([]omnigentMeta, int64, string, error) {
	var out []omnigentMeta
	cursor := afterRowID
	tailIdentity := afterIdentity
	for {
		page, err := queryOmnigentConversationMetas(
			ctx, conn, omnigentNewConversationQuery(schema),
			cursor, omnigentChangePageSize,
		)
		if err != nil {
			return nil, afterRowID, afterIdentity, err
		}
		out = append(out, page...)
		if len(page) == 0 {
			return out, cursor, tailIdentity, nil
		}
		cursor = page[len(page)-1].rowID
		tailIdentity = page[len(page)-1].member().key()
		if len(page) < omnigentChangePageSize {
			return out, cursor, tailIdentity, nil
		}
	}
}

// omnigentNewConversationQuery paginates newly inserted conversation rows by
// rowid into a MATERIALIZED CTE before joining conversation_items for the
// aggregate rollup: without the hint, SQLite's planner can flatten the CTE
// into the join and fall back to scanning the whole item table for each
// page. TestOmnigentIncrementalQueriesAvoidFullTableScans guards against
// that with EXPLAIN QUERY PLAN, restricted to SQLite's long-stable
// "SCAN <table>" vocabulary rather than index names or the version-sensitive
// "AUTOMATIC" marker. TestOmnigentChangedPathParsingIsBounded and
// TestOmnigentIncrementalRowQueriesReturnOnlyRowsPastCursor separately
// exercise this query at growing archive cardinality and guard its output
// correctness.
func omnigentNewConversationQuery(schema omnigentSchema) string {
	prefix := `
		WITH selected AS MATERIALIZED (
			SELECT rowid, workspace_id, id, updated_at
			  FROM conversations
			 WHERE rowid > ?
			 ORDER BY rowid
			 LIMIT ?
		)`
	// The shared builder applies COALESCE(c.updated_at, 0) in the outer
	// SELECT rather than inside the CTE; algebraically equivalent since the
	// CTE's updated_at is scanned straight through either way.
	return prefix + omnigentConversationAggregateQuery(schema, "selected", "") + `
		 ORDER BY c.rowid`
}

func listOmnigentNewItemMembers(
	ctx context.Context, conn *sql.DB, schema omnigentSchema,
	afterRowID int64, afterIdentity string,
) ([]omnigentMemberID, int64, string, error) {
	query := omnigentNewItemQuery(schema)
	cursor := afterRowID
	tailIdentity := afterIdentity
	var members []omnigentMemberID
	for {
		rows, err := conn.QueryContext(
			ctx, query, cursor, omnigentChangePageSize,
		)
		if err != nil {
			return nil, afterRowID, afterIdentity,
				fmt.Errorf("listing new omnigent items: %w", err)
		}
		pageSize := 0
		for rows.Next() {
			var rowID int64
			var member omnigentMemberID
			var itemID string
			if err := rows.Scan(
				&rowID, &member.workspaceID, &member.rawID, &itemID,
			); err != nil {
				_ = rows.Close()
				return nil, afterRowID, afterIdentity,
					fmt.Errorf("scanning new omnigent item: %w", err)
			}
			cursor = rowID
			tailIdentity = member.key() + ":" + itemID
			members = append(members, member)
			pageSize++
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, afterRowID, afterIdentity, err
		}
		if err := rows.Close(); err != nil {
			return nil, afterRowID, afterIdentity, err
		}
		if pageSize < omnigentChangePageSize {
			return members, cursor, tailIdentity, nil
		}
	}
}

func omnigentNewItemQuery(schema omnigentSchema) string {
	idExpr := omnigentIDExpr(schema, "conversation_id")
	itemExpr := omnigentIDExpr(schema, "id")
	return `
		SELECT rowid, workspace_id, ` + idExpr + `, ` + itemExpr + `
		  FROM conversation_items
		 WHERE rowid > ?
		 ORDER BY rowid
		 LIMIT ?`
}

func queryOmnigentConversationMetas(
	ctx context.Context, conn *sql.DB, query string, args ...any,
) ([]omnigentMeta, error) {
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing changed omnigent conversations: %w", err)
	}
	defer rows.Close()
	var metas []omnigentMeta
	for rows.Next() {
		var meta omnigentMeta
		if err := rows.Scan(&meta.rowID, &meta.workspaceID, &meta.rawID, &meta.updatedAt,
			&meta.itemCount, &meta.maxPosition); err != nil {
			return nil, fmt.Errorf("scanning changed omnigent conversation: %w", err)
		}
		metas = append(metas, meta)
	}
	return metas, rows.Err()
}

func omnigentMatches(
	container string, schema omnigentSchema, metas []omnigentMeta,
) []multiSessionMatch {
	matches := make([]multiSessionMatch, 0, len(metas))
	seen := make(map[string]struct{}, len(metas))
	for _, meta := range metas {
		key := meta.member().key()
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		matches = append(matches, multiSessionMatch{
			Path: VirtualSourcePath(container, key), Container: container, MemberID: key,
		})
	}
	return matches
}

func (t *omnigentChangeTracker) restoreCachedContainer(
	ctx context.Context, container string,
) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	t.mu.Lock()
	_, warm := t.containers[container]
	t.mu.Unlock()
	if warm {
		return false, nil
	}
	conn, err := openOmnigentDB(container)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	schema, err := detectOmnigentSchema(conn)
	if err != nil {
		if omnigentSchemaUnsupported(err) {
			// Unsupported parse outcomes are intentionally skip-cached. A
			// restart may validate that cache entry with a cold tracker; the
			// known unsupported state still proves the cached skip, but cannot
			// seed supported-schema change cursors.
			return false, nil
		}
		return false, err
	}
	conversationRowID, conversationTail, err := omnigentLatestRowIdentity(
		ctx, conn, omnigentConversationsTable, omnigentConversationIDExprs(schema),
	)
	if err != nil {
		return false, err
	}
	itemRowID, itemTail, err := omnigentLatestRowIdentity(
		ctx, conn, omnigentItemsTable, omnigentItemIDExprs(schema),
	)
	if err != nil {
		return false, err
	}
	t.mu.Lock()
	t.containers[container] = omnigentTrackedContainer{
		schema:            schema,
		conversationRowID: conversationRowID,
		conversationTail:  conversationTail,
		itemRowID:         itemRowID,
		itemTail:          itemTail,
	}
	t.mu.Unlock()
	return true, nil
}

func (t *omnigentChangeTracker) parseContainer(
	ctx context.Context, src multiSessionSource, req ParseRequest,
) ([]ParseResult, error) {
	results, schema, metas, itemRowID, itemTail, err := omnigentParseContainerData(
		ctx, src, req,
	)
	if err != nil {
		return nil, err
	}
	if IsRegularFile(src.Container) {
		var conversationRowID int64
		var conversationTail string
		for _, meta := range metas {
			if meta.rowID > conversationRowID {
				conversationRowID = meta.rowID
				conversationTail = meta.member().key()
			}
		}
		t.mu.Lock()
		t.containers[src.Container] = omnigentTrackedContainer{
			schema:            schema,
			conversationRowID: conversationRowID,
			conversationTail:  conversationTail,
			itemRowID:         itemRowID,
			itemTail:          itemTail,
		}
		t.mu.Unlock()
	}
	return results, nil
}

// omnigentDBMtimeSuffixes tracks content-bearing SQLite files only, omitting
// "-shm": opening a read connection can update the shared-memory file, so
// including it would turn the provider's own reads into apparent source
// changes and keep the scheduled fingerprint pass reparsing forever.
var omnigentDBMtimeSuffixes = []string{"", "-wal"}

func omnigentMemberPresent(src multiSessionSource) bool {
	if src.MemberID == "" {
		return IsRegularFile(src.Container)
	}
	return omnigentConversationExists(src.Container, src.MemberID)
}

func omnigentParseMember(
	ctx context.Context, src multiSessionSource, req ParseRequest,
) (*ParseResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dbInfo, err := os.Stat(src.Container)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat %s: %w", src.Container, err)
	}
	conn, err := openOmnigentDB(src.Container)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	schema, err := detectOmnigentSchema(conn)
	if err != nil {
		return nil, err
	}
	// A member ID that no longer parses under the detected schema is a retired
	// legacy identity; a nil result retires its archived session.
	member, err := omnigentMemberForSchema(src.MemberID)
	if err != nil {
		return nil, nil
	}
	return parseOmnigentConversationFromDB(
		ctx, conn, schema, src.Container, member, req.Machine, dbInfo,
	)
}

func omnigentParseContainerData(
	ctx context.Context, src multiSessionSource, req ParseRequest,
) ([]ParseResult, omnigentSchema, []omnigentMeta, int64, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, omnigentSchema{}, nil, 0, "", err
	}
	dbInfo, err := os.Stat(src.Container)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, omnigentSchema{}, nil, 0, "", nil
		}
		return nil, omnigentSchema{}, nil, 0, "",
			fmt.Errorf("stat %s: %w", src.Container, err)
	}
	conn, err := openOmnigentDB(src.Container)
	if err != nil {
		return nil, omnigentSchema{}, nil, 0, "", err
	}
	defer conn.Close()

	schema, err := detectOmnigentSchema(conn)
	if err != nil {
		return nil, omnigentSchema{}, nil, 0, "", err
	}
	metas, err := listOmnigentConversationMetas(ctx, conn, schema)
	if err != nil {
		return nil, omnigentSchema{}, nil, 0, "", err
	}
	itemRowID, itemTail, err := omnigentLatestRowIdentity(
		ctx, conn, omnigentItemsTable, omnigentItemIDExprs(schema),
	)
	if err != nil {
		return nil, omnigentSchema{}, nil, 0, "", err
	}
	results := make([]ParseResult, 0, len(metas))
	for i := range metas {
		if err := ctx.Err(); err != nil {
			return nil, omnigentSchema{}, nil, 0, "", err
		}
		result, err := parseOmnigentConversationFromDB(
			ctx, conn, schema, src.Container, metas[i].member(),
			req.Machine, dbInfo,
		)
		if err != nil {
			return nil, omnigentSchema{}, nil, 0, "", err
		}
		if result == nil {
			continue
		}
		results = append(results, *result)
	}
	return results, schema, metas, itemRowID, itemTail, nil
}

func omnigentSchemaUnsupported(err error) bool {
	var unsupported ErrOmnigentUnsupportedSchema
	return errors.As(err, &unsupported)
}

func parseOmnigentVirtualPath(path string) (string, string, bool) {
	container, member, ok := ParseVirtualSourcePathForBase(path, omnigentDBName)
	if !ok || strings.ContainsAny(member, `/\`) {
		return "", "", false
	}
	return container, member, true
}
