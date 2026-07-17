// ABOUTME: Multi-session container provider for omnigent: one chat.db fanned
// ABOUTME: out into one session per conversation, with incremental sync.
package parser

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	omnigentChangedBatchSize     = 32
	omnigentFastChangedBatchSize = 8
	omnigentProbeBatchSize       = omnigentChangedBatchSize
)

type omnigentTrackedContainer struct {
	schema           omnigentSchema
	initializing     bool
	recovering       bool
	recoveryBoundary bool
	initialScanRowID int64
	checkedAt        int64
	compositeMTimeNS int64
	probeRowID       int64
	probeActive      bool
	probeRepeat      bool
}

type omnigentChangeTracker struct {
	mu              sync.Mutex
	containers      map[string]omnigentTrackedContainer
	recoveryPending map[string]struct{}
}

func newOmnigentChangeTracker() *omnigentChangeTracker {
	return &omnigentChangeTracker{
		containers:      make(map[string]omnigentTrackedContainer),
		recoveryPending: make(map[string]struct{}),
	}
}

// Omnigent stores every conversation in one shared SQLite database (chat.db).
// The provider emits bounded member batches addressed by
// "<db>#<conversationID>" virtual paths. A complete database of at most one
// batch may use one whole-container source for authoritative reconciliation.
func newOmnigentProviderFactory(def AgentDef) ProviderFactory {
	tracker := newOmnigentChangeTracker()
	return NewMultiSessionProviderFactory(
		def,
		omnigentProviderCapabilities(),
		func(cfg ProviderConfig) multiSessionContainerSourceSet {
			return NewMultiSessionContainerSourceSet(
				AgentOmnigent,
				cfg.Roots,
				WithSourceDiscovery(func(root string) []multiSessionMatch {
					return tracker.discoverSources(root, cfg.ForceFullDiscovery)
				}),
				WithWatchRoots(omnigentWatchRoots),
				WithChangedPathClassifier(omnigentClassifyPath),
				WithChangedPathMembers(tracker.changedMembers),
				WithMemberLookup(omnigentFindMember),
				WithFingerprint(omnigentFingerprintSource),
				WithContainerParse(tracker.parseContainer),
				WithMemberParse(tracker.parseMember),
				WithMemberResultHashPreservation(),
				WithMemberPresence(omnigentMemberPresent),
				WithUnsupportedSourceError(omnigentSchemaUnsupported),
				WithExcludedSessionIDs(omnigentLegacySessionIDs),
			)
		},
	)
}

// IsOmnigentContainerSource reports whether source addresses the whole
// physical chat database rather than one virtual conversation member.
func IsOmnigentContainerSource(source SourceRef) bool {
	if source.Provider != AgentOmnigent {
		return false
	}
	src, ok := source.Opaque.(multiSessionSource)
	return ok && src.Container != "" && src.MemberID == ""
}

func omnigentLegacySessionIDs(
	src multiSessionSource, results []ParseResult,
) []string {
	legacy := make(map[string]struct{})
	add := func(memberKey string) {
		_, rawID, ok := strings.Cut(memberKey, ":")
		if ok && IsValidSessionID(rawID) {
			legacy[omnigentIDPrefix+rawID] = struct{}{}
		}
	}
	if src.MemberID != "" {
		add(src.MemberID)
	}
	for _, result := range results {
		add(strings.TrimPrefix(result.Session.ID, omnigentIDPrefix))
	}
	ids := make([]string, 0, len(legacy))
	for id := range legacy {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func omnigentProviderCapabilities() Capabilities {
	return Capabilities{
		Source: multiSessionContainerSourceCapabilities(
			CapabilitySupported,
			CapabilitySupported,
		),
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
	}
}

func omnigentDiscoverContainers(root string) []string {
	if dbPath := omnigentDBPath(root); dbPath != "" {
		return []string{dbPath}
	}
	return nil
}

func (t *omnigentChangeTracker) discoverSources(
	root string, forceFull bool,
) []multiSessionMatch {
	containers := omnigentDiscoverContainers(root)
	matches := make([]multiSessionMatch, 0, len(containers))
	for _, container := range containers {
		whole := multiSessionMatch{Path: container, Container: container}
		if forceFull {
			matches = append(matches, whole)
			continue
		}
		t.mu.Lock()
		_, recoveryPending := t.recoveryPending[container]
		t.mu.Unlock()
		if recoveryPending {
			continue
		}
		changed, err := t.changedMembers(context.Background(), root, ChangedPathRequest{
			Path: container, EventKind: "poll",
		})
		if err != nil {
			continue
		}
		matches = append(matches, changed...)
	}
	return matches
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
func omnigentClassifyPath(
	root, path string, allowMissing bool,
) (multiSessionMatch, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	requireRegular := !allowMissing
	if dbPath, conversationID, ok := parseOmnigentVirtualPath(path); ok {
		if !omnigentDBUnderRoot(root, dbPath, requireRegular) {
			return multiSessionMatch{}, false
		}
		return multiSessionMatch{
			Path:      path,
			Container: dbPath,
			MemberID:  conversationID,
		}, true
	}
	if omnigentDBUnderRoot(root, path, requireRegular) {
		return multiSessionMatch{Path: path, Container: path}, true
	}
	if allowMissing {
		if dbPath, ok := omnigentDBPathForEvent(root, path); ok {
			return multiSessionMatch{Path: dbPath, Container: dbPath}, true
		}
	}
	return multiSessionMatch{}, false
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
		if compositeMtime, err := omnigentDBCompositeMtime(src.Container); err == nil {
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
	member, err := omnigentMemberForSchema(schema, src.MemberID)
	if err != nil {
		return SourceFingerprint{}, err
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
	query := `
		SELECT c.rowid, 0, c.id, COALESCE(c.updated_at, 0),
		       COUNT(ci.id), COALESCE(MAX(ci.position), -1)
		  FROM conversations c
		  LEFT JOIN conversation_items ci ON ci.conversation_id = c.id
		 WHERE c.id = ?
		 GROUP BY c.id`
	args := []any{member.rawID}
	if schema.splitMetadata {
		query = `
			SELECT c.rowid, c.workspace_id, c.id, COALESCE(c.updated_at, 0),
			       COUNT(ci.id), COALESCE(MAX(ci.position), -1)
			  FROM conversations c
			  LEFT JOIN conversation_items ci
			    ON ci.workspace_id = c.workspace_id AND ci.conversation_id = c.id
			 WHERE c.workspace_id = ? AND c.id = ?
			 GROUP BY c.workspace_id, c.id`
		args = []any{member.workspaceID, member.rawID}
	}
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
	t.mu.Lock()
	tracked, initialized := t.containers[match.Container]
	_, recoveryPending := t.recoveryPending[match.Container]
	if req.EventKind == ChangedPathEventRecovery {
		if tracked.recoveryBoundary {
			tracked.recoveryBoundary = false
			t.containers[match.Container] = tracked
			t.mu.Unlock()
			return nil, nil
		}
		if !tracked.recovering {
			t.recoveryPending[match.Container] = struct{}{}
			t.mu.Unlock()
			return t.initializeRecoveryContainer(
				ctx, root, match, req.StoredSourcePaths,
			)
		}
	} else if recoveryPending || tracked.recovering || tracked.recoveryBoundary {
		t.mu.Unlock()
		return nil, nil
	}
	t.mu.Unlock()
	if !initialized {
		return t.initializeColdContainer(
			ctx, root, match, req.StoredSourcePaths,
		)
	}
	conn, err := openOmnigentDB(match.Container)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	schema, err := detectOmnigentSchema(conn)
	if err != nil {
		return nil, err
	}
	storedMatches, err := omnigentStoredSourceMatches(
		ctx, root, match.Container, schema, req.StoredSourcePaths,
	)
	if err != nil {
		return nil, err
	}
	if req.EventKind == ChangedPathEventReconcile {
		return storedMatches, nil
	}

	t.mu.Lock()
	tracked = t.containers[match.Container]
	if tracked.schema != schema {
		if req.EventKind == ChangedPathEventRecovery {
			t.recoveryPending[match.Container] = struct{}{}
			t.mu.Unlock()
			return t.initializeRecoveryContainer(
				ctx, root, match, req.StoredSourcePaths,
			)
		}
		t.mu.Unlock()
		return t.initializeColdContainer(
			ctx, root, match, req.StoredSourcePaths,
		)
	}
	defer t.mu.Unlock()
	if tracked.initializing {
		capacity := max(omnigentChangedBatchSize-len(storedMatches), 0)
		if capacity == 0 {
			return storedMatches, nil
		}
		page, err := listOmnigentConversationMetasAfterRowID(
			ctx, conn, schema, tracked.initialScanRowID,
			capacity+1,
		)
		if err != nil {
			return nil, err
		}
		batch := page
		if len(batch) > capacity {
			batch = batch[:capacity]
		}
		if len(batch) > 0 {
			tracked.initialScanRowID = batch[len(batch)-1].rowID
		}
		more := len(page) > capacity
		tracked.initializing = more
		if tracked.recovering {
			tracked.recovering = more
			tracked.recoveryBoundary = !more && len(batch) == capacity
		}
		t.containers[match.Container] = tracked
		out := appendOmnigentMatches(
			omnigentMatches(match.Container, schema, batch), storedMatches,
		)
		if !more && len(req.StoredSourcePaths) == 0 {
			if len(out) == 0 {
				match.ReconcileStoredHints = true
				match.ReconcileOnly = true
				return []multiSessionMatch{match}, nil
			}
			for i := range out {
				out[i].ReconcileStoredHints = true
			}
		}
		return out, nil
	}
	compositeMTimeNS, err := omnigentDBCompositeMtime(match.Container)
	if err != nil {
		return nil, err
	}
	changedEvent := req.EventKind != "" && req.EventKind != "poll"
	containerChanged := compositeMTimeNS != tracked.compositeMTimeNS || changedEvent
	if containerChanged {
		tracked.compositeMTimeNS = compositeMTimeNS
		if tracked.probeActive {
			tracked.probeRepeat = true
		} else {
			tracked.probeActive = true
			tracked.probeRowID = 0
		}
	}
	checkedAt := time.Now().Unix()
	fastChanged, err := listOmnigentConversationMetasSince(
		ctx, conn, schema, nil, max(tracked.checkedAt-1, 0), checkedAt+1,
		omnigentFastChangedBatchSize,
	)
	if err != nil {
		return nil, err
	}
	if len(fastChanged) < omnigentFastChangedBatchSize {
		tracked.checkedAt = checkedAt
	}
	baseMatches := appendOmnigentMatches(
		omnigentMatches(match.Container, schema, fastChanged), storedMatches,
	)
	if len(baseMatches) > omnigentChangedBatchSize {
		baseMatches = baseMatches[:omnigentChangedBatchSize]
	}
	capacity := max(omnigentChangedBatchSize-len(baseMatches), 0)
	var metas []omnigentMeta
	if tracked.probeActive && capacity > 0 {
		page, err := listOmnigentConversationMetasAfterRowID(
			ctx, conn, schema, tracked.probeRowID, capacity+1,
		)
		if err != nil {
			return nil, err
		}
		metas = page
		if len(metas) > capacity {
			metas = metas[:capacity]
		}
		if len(metas) > 0 {
			tracked.probeRowID = metas[len(metas)-1].rowID
		}
		if len(page) <= capacity {
			if tracked.probeRepeat {
				tracked.probeRowID = 0
				tracked.probeRepeat = false
			} else {
				tracked.probeActive = false
				tracked.probeRowID = 0
			}
		}
	}
	t.containers[match.Container] = tracked
	out := appendOmnigentMatches(
		omnigentMatches(match.Container, schema, metas), baseMatches,
	)
	if containerChanged && len(req.StoredSourcePaths) == 0 {
		if len(out) == 0 {
			match.ReconcileStoredHints = true
			match.ReconcileOnly = true
			return []multiSessionMatch{match}, nil
		}
		for i := range out {
			out[i].ReconcileStoredHints = true
		}
	}
	return out, nil
}

// initializeColdContainer establishes a bounded tracker foothold without
// fanning the physical database out through one archive-sized parse. Member
// classification advances an initialization cursor independently of parse and
// archive freshness skips, so later events continue from the next batch. A
// first page that contains the complete container remains a whole source: its
// bounded complete outcome authoritatively reconciles archived membership.
func (t *omnigentChangeTracker) initializeColdContainer(
	ctx context.Context, root string, match multiSessionMatch,
	storedSourcePaths []string,
) ([]multiSessionMatch, error) {
	conn, err := openOmnigentDB(match.Container)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	schema, err := detectOmnigentSchema(conn)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	storedMatches, err := omnigentStoredSourceMatches(
		ctx, root, match.Container, schema, storedSourcePaths,
	)
	if err != nil {
		return nil, err
	}
	capacity := max(omnigentChangedBatchSize-len(storedMatches), 0)
	page, err := listOmnigentConversationMetasAfterRowID(
		ctx, conn, schema, 0, capacity+1,
	)
	if err != nil {
		return nil, err
	}
	if len(storedMatches) == 0 && len(page) <= omnigentChangedBatchSize {
		t.replace(match.Container, schema, page)
		return []multiSessionMatch{match}, nil
	}
	batch := page
	if len(batch) > capacity {
		batch = batch[:capacity]
	}
	t.replace(match.Container, schema, batch)
	t.mu.Lock()
	tracked := t.containers[match.Container]
	tracked.initializing = len(page) > capacity || len(storedMatches) > 0
	if len(batch) > 0 {
		tracked.initialScanRowID = batch[len(batch)-1].rowID
	}
	t.containers[match.Container] = tracked
	t.mu.Unlock()
	return appendOmnigentMatches(
		omnigentMatches(match.Container, schema, batch), storedMatches,
	), nil
}

func (t *omnigentChangeTracker) initializeRecoveryContainer(
	ctx context.Context, root string, match multiSessionMatch,
	storedSourcePaths []string,
) ([]multiSessionMatch, error) {
	conn, err := openOmnigentDB(match.Container)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	schema, err := detectOmnigentSchema(conn)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	storedMatches, err := omnigentStoredSourceMatches(
		ctx, root, match.Container, schema, storedSourcePaths,
	)
	if err != nil {
		return nil, err
	}
	capacity := max(omnigentChangedBatchSize-len(storedMatches), 0)
	page, err := listOmnigentConversationMetasAfterRowID(
		ctx, conn, schema, 0, capacity+1,
	)
	if err != nil {
		return nil, err
	}
	batch := page
	if len(batch) > capacity {
		batch = batch[:capacity]
	}
	tracked := newOmnigentTrackedContainer(match.Container, schema, batch)
	more := len(page) > capacity || len(storedMatches) > 0
	tracked.initializing = more
	tracked.recovering = more
	tracked.recoveryBoundary = !more && len(batch) == capacity
	if len(batch) > 0 {
		tracked.initialScanRowID = batch[len(batch)-1].rowID
	}
	t.mu.Lock()
	t.containers[match.Container] = tracked
	delete(t.recoveryPending, match.Container)
	t.mu.Unlock()
	return appendOmnigentMatches(
		omnigentMatches(match.Container, schema, batch), storedMatches,
	), nil
}

func listOmnigentConversationMetasSince(
	ctx context.Context, conn *sql.DB, schema omnigentSchema,
	workspaceIDs []int64, updatedAfter, updatedThrough int64, limit int,
) ([]omnigentMeta, error) {
	query := `
		WITH selected AS (
			SELECT rowid, id, COALESCE(updated_at, 0) AS updated_at
			  FROM conversations
			 WHERE updated_at >= ?
			   AND updated_at <= ?
			 ORDER BY updated_at, rowid
			 LIMIT ?
		)
		SELECT c.rowid, 0, c.id, c.updated_at,
		       COUNT(ci.id), COALESCE(MAX(ci.position), -1)
		  FROM selected c
		  LEFT JOIN conversation_items ci ON ci.conversation_id = c.id
		 GROUP BY c.id
		 ORDER BY c.updated_at, c.rowid`
	if !schema.splitMetadata {
		return queryOmnigentConversationMetas(
			ctx, conn, query, updatedAfter, updatedThrough, limit,
		)
	}
	if len(workspaceIDs) == 0 {
		query = `
			WITH selected AS (
				SELECT rowid, workspace_id, id,
				       COALESCE(updated_at, 0) AS updated_at
				  FROM conversations
				 WHERE updated_at >= ?
				   AND updated_at <= ?
				 ORDER BY updated_at, rowid
				 LIMIT ?
			)
			SELECT c.rowid, c.workspace_id, c.id, c.updated_at,
			       COUNT(ci.id), COALESCE(MAX(ci.position), -1)
			  FROM selected c
			  LEFT JOIN conversation_items ci
			    ON ci.workspace_id = c.workspace_id AND ci.conversation_id = c.id
			 GROUP BY c.workspace_id, c.id
			 ORDER BY c.updated_at, c.rowid`
		return queryOmnigentConversationMetas(
			ctx, conn, query, updatedAfter, updatedThrough, limit,
		)
	}
	query = `
			WITH selected AS (
				SELECT rowid, workspace_id, id,
				       COALESCE(updated_at, 0) AS updated_at
				  FROM conversations
				 WHERE workspace_id = ?
				   AND updated_at >= ?
				   AND updated_at <= ?
				 ORDER BY updated_at, rowid
				 LIMIT ?
			)
			SELECT c.rowid, c.workspace_id, c.id, c.updated_at,
			       COUNT(ci.id), COALESCE(MAX(ci.position), -1)
			  FROM selected c
			  LEFT JOIN conversation_items ci
			    ON ci.workspace_id = c.workspace_id AND ci.conversation_id = c.id
			 GROUP BY c.workspace_id, c.id
			 ORDER BY c.updated_at, c.rowid`
	var metas []omnigentMeta
	for _, workspaceID := range workspaceIDs {
		remaining := limit - len(metas)
		if remaining <= 0 {
			break
		}
		workspaceMetas, err := queryOmnigentConversationMetas(
			ctx, conn, query, workspaceID, updatedAfter, updatedThrough, remaining,
		)
		if err != nil {
			return nil, err
		}
		metas = append(metas, workspaceMetas...)
	}
	return metas, nil
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

func listOmnigentConversationMetasAfterRowID(
	ctx context.Context, conn *sql.DB, schema omnigentSchema, rowID int64, limit int,
) ([]omnigentMeta, error) {
	query := `
		SELECT c.rowid, 0, c.id, COALESCE(c.updated_at, 0), 0, -1
		  FROM conversations c
		 WHERE c.rowid > ?
		 ORDER BY c.rowid
		 LIMIT ?`
	if schema.splitMetadata {
		query = `
			SELECT c.rowid, c.workspace_id, c.id,
			       COALESCE(c.updated_at, 0), 0, -1
			  FROM conversations c
			 WHERE c.rowid > ?
			 ORDER BY c.rowid
			 LIMIT ?`
	}
	rows, err := conn.QueryContext(ctx, query, rowID, limit)
	if err != nil {
		return nil, fmt.Errorf("listing new omnigent conversations: %w", err)
	}
	defer rows.Close()
	var metas []omnigentMeta
	for rows.Next() {
		var meta omnigentMeta
		if err := rows.Scan(&meta.rowID, &meta.workspaceID, &meta.rawID,
			&meta.updatedAt, &meta.itemCount, &meta.maxPosition); err != nil {
			return nil, fmt.Errorf("scanning new omnigent conversation: %w", err)
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
		key := meta.member().key(schema)
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

func appendOmnigentMatches(
	primary, extra []multiSessionMatch,
) []multiSessionMatch {
	seen := make(map[string]struct{}, len(primary)+len(extra))
	out := make([]multiSessionMatch, 0, len(primary)+len(extra))
	for _, matches := range [][]multiSessionMatch{primary, extra} {
		for _, match := range matches {
			if _, exists := seen[match.Path]; exists {
				continue
			}
			seen[match.Path] = struct{}{}
			out = append(out, match)
		}
	}
	return out
}

func omnigentStoredSourceMatches(
	ctx context.Context, root, container string, schema omnigentSchema,
	storedSourcePaths []string,
) ([]multiSessionMatch, error) {
	if len(storedSourcePaths) == 0 {
		return nil, nil
	}
	var matches []multiSessionMatch
	for _, storedPath := range storedSourcePaths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		match, ok := omnigentClassifyPath(root, storedPath, true)
		if !ok || match.MemberID == "" || !samePath(match.Container, container) {
			continue
		}
		if _, err := omnigentMemberForSchema(schema, match.MemberID); err != nil {
			continue
		}
		matches = append(matches, match)
	}
	return matches, nil
}

func (t *omnigentChangeTracker) parseContainer(
	src multiSessionSource, req ParseRequest,
) ([]ParseResult, error) {
	results, schema, metas, err := omnigentParseContainerData(src, req)
	if err != nil {
		return nil, err
	}
	t.replace(src.Container, schema, metas)
	return results, nil
}

func (t *omnigentChangeTracker) parseMember(
	src multiSessionSource, req ParseRequest,
) (*ParseResult, error) {
	return t.parseMemberWith(src, req, omnigentParseMember)
}

func (t *omnigentChangeTracker) parseMemberWith(
	src multiSessionSource, req ParseRequest,
	parse func(multiSessionSource, ParseRequest) (*ParseResult, error),
) (*ParseResult, error) {
	// Capture the classification metadata before reading the transcript. If a
	// commit lands between these operations, observing the older metadata causes
	// one safe extra parse on the next event; observing newer metadata for an
	// older transcript would suppress that event and leave the archive stale.
	var (
		schema     omnigentSchema
		member     omnigentMemberID
		meta       omnigentMeta
		exists     bool
		track      bool
		observedAt int64
	)
	observedAt = time.Now().Unix()
	conn, openErr := openOmnigentDB(src.Container)
	if openErr == nil {
		schema, openErr = detectOmnigentSchema(conn)
		if openErr == nil {
			member, openErr = omnigentMemberForSchema(schema, src.MemberID)
		}
		if openErr == nil {
			meta, exists, openErr = loadOmnigentConversationMeta(conn, schema, member)
		}
		track = openErr == nil
		_ = conn.Close()
	}

	result, err := parse(src, req)
	if err != nil {
		return nil, err
	}
	if track {
		t.observe(src.Container, schema, member, meta, exists, observedAt)
	}
	return result, nil
}

func (t *omnigentChangeTracker) replace(
	container string, schema omnigentSchema, metas []omnigentMeta,
) {
	tracked := newOmnigentTrackedContainer(container, schema, metas)
	t.mu.Lock()
	t.containers[container] = tracked
	delete(t.recoveryPending, container)
	t.mu.Unlock()
}

func newOmnigentTrackedContainer(
	container string, schema omnigentSchema, metas []omnigentMeta,
) omnigentTrackedContainer {
	compositeMTimeNS, _ := omnigentDBCompositeMtime(container)
	tracked := omnigentTrackedContainer{
		schema: schema, compositeMTimeNS: compositeMTimeNS,
		checkedAt: time.Now().Unix(),
	}
	for _, meta := range metas {
		if meta.rowID > tracked.initialScanRowID {
			tracked.initialScanRowID = meta.rowID
		}
	}
	return tracked
}

// omnigentDBCompositeMtime tracks content-bearing SQLite files only. Opening a
// read connection can update the shared-memory file, so including -shm would
// turn the provider's own probes into apparent source changes and keep recovery
// sweeps running forever.
func omnigentDBCompositeMtime(dbPath string) (int64, error) {
	var maxMtime int64
	for _, suffix := range []string{"", "-wal"} {
		info, err := os.Stat(dbPath + suffix)
		if err != nil {
			continue
		}
		maxMtime = max(maxMtime, info.ModTime().UnixNano())
	}
	if maxMtime == 0 {
		return 0, &os.PathError{Op: "stat", Path: dbPath, Err: os.ErrNotExist}
	}
	return maxMtime, nil
}

func (t *omnigentChangeTracker) observe(
	container string, schema omnigentSchema, member omnigentMemberID,
	meta omnigentMeta, exists bool, observedAt int64,
) {
	_ = member
	_ = meta
	_ = exists
	_ = observedAt
	t.mu.Lock()
	defer t.mu.Unlock()
	tracked, ok := t.containers[container]
	if !ok || tracked.schema != schema {
		tracked = newOmnigentTrackedContainer(container, schema, nil)
	}
	t.containers[container] = tracked
}

func omnigentMemberPresent(src multiSessionSource) bool {
	if src.MemberID == "" {
		return IsRegularFile(src.Container)
	}
	return omnigentConversationExists(src.Container, src.MemberID)
}

func omnigentParseMember(
	src multiSessionSource, req ParseRequest,
) (*ParseResult, error) {
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
	member, err := omnigentMemberForSchema(schema, src.MemberID)
	if err != nil {
		return nil, err
	}
	return parseOmnigentConversationFromDB(
		conn, schema, src.Container, member, req.Machine, dbInfo,
	)
}

func omnigentParseContainerData(
	src multiSessionSource, req ParseRequest,
) ([]ParseResult, omnigentSchema, []omnigentMeta, error) {
	dbInfo, err := os.Stat(src.Container)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, omnigentSchema{}, nil, nil
		}
		return nil, omnigentSchema{}, nil,
			fmt.Errorf("stat %s: %w", src.Container, err)
	}
	conn, err := openOmnigentDB(src.Container)
	if err != nil {
		return nil, omnigentSchema{}, nil, err
	}
	defer conn.Close()

	schema, err := detectOmnigentSchema(conn)
	if err != nil {
		return nil, omnigentSchema{}, nil, err
	}
	metas, err := listOmnigentConversationMetas(conn, schema)
	if err != nil {
		return nil, omnigentSchema{}, nil, err
	}
	results := make([]ParseResult, 0, len(metas))
	for i := range metas {
		result, err := parseOmnigentConversationFromDB(
			conn, schema, src.Container, metas[i].member(),
			req.Machine, dbInfo,
		)
		if err != nil {
			return nil, omnigentSchema{}, nil, err
		}
		if result == nil {
			continue
		}
		results = append(results, *result)
	}
	return results, schema, metas, nil
}

func omnigentSchemaUnsupported(err error) bool {
	var unsupported ErrOmnigentUnsupportedSchema
	return errors.As(err, &unsupported)
}

func omnigentDBUnderRoot(root, dbPath string, requireRegular bool) bool {
	root = filepath.Clean(root)
	dbPath = filepath.Clean(dbPath)
	rel, ok := relUnder(root, dbPath)
	if !ok || filepath.ToSlash(rel) != omnigentDBName {
		return false
	}
	return !requireRegular || IsRegularFile(dbPath)
}

func omnigentDBPathForEvent(root, path string) (string, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rel, ok := relUnder(root, path)
	if !ok {
		return "", false
	}
	if filepath.ToSlash(rel) == omnigentDBName ||
		(filepath.Dir(rel) == "." &&
			strings.HasPrefix(filepath.Base(rel), omnigentDBName+"-")) {
		return filepath.Join(root, omnigentDBName), true
	}
	return "", false
}

func parseOmnigentVirtualPath(path string) (string, string, bool) {
	container, member, ok := ParseVirtualSourcePathForBase(path, omnigentDBName)
	if !ok || strings.ContainsAny(member, `/\`) {
		return "", "", false
	}
	return container, member, true
}

// ParseOmnigentVirtualSourcePath recognizes only virtual member paths whose
// separator follows the physical chat.db basename. Directory names containing
// '#' therefore remain valid physical container paths.
func ParseOmnigentVirtualSourcePath(path string) (string, string, bool) {
	return parseOmnigentVirtualPath(path)
}
