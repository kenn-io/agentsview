// ABOUTME: Multi-session container provider for omnigent: one chat.db fanned
// ABOUTME: out into one session per conversation, with incremental sync.
package parser

import (
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
	omnigentProbeBatchSize     = 32
	omnigentWorkspaceBatchSize = 32
)

type omnigentTrackedContainer struct {
	schema             omnigentSchema
	metas              map[string]omnigentMeta
	checkedAt          int64
	count              int
	maxRowID           int64
	probeKeys          []string
	probeCursor        int
	probeRemaining     int
	compositeMTimeNS   int64
	workspaceIDs       []int64
	workspaceCursor    int
	workspaceCheckedAt map[int64]int64
	workspaceCounts    map[int64]int
}

type omnigentChangeTracker struct {
	mu         sync.Mutex
	containers map[string]omnigentTrackedContainer
}

func newOmnigentChangeTracker() *omnigentChangeTracker {
	return &omnigentChangeTracker{
		containers: make(map[string]omnigentTrackedContainer),
	}
}

// omnigent stores every conversation in one shared SQLite database (chat.db).
// It is a multi-session container provider: initial discovery surfaces the
// database as a single source, then scheduled discovery emits bounded changed
// member batches addressed by "<db>#<conversationID>" virtual paths.
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
			CapabilityUnsupported,
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
		t.mu.Lock()
		_, initialized := t.containers[container]
		t.mu.Unlock()
		if !initialized {
			t.seedContainer(container)
		}
		if forceFull || !initialized {
			matches = append(matches, whole)
			continue
		}
		changed, err := t.changedMembers(root, ChangedPathRequest{
			Path: container, EventKind: "poll",
		})
		if err != nil {
			// Fall back to the authoritative container source so operational
			// failures are surfaced by fingerprinting/parsing instead of turning
			// a failed discovery probe into a clean empty sync.
			matches = append(matches, whole)
			continue
		}
		matches = append(matches, changed...)
	}
	return matches
}

func (t *omnigentChangeTracker) seedContainer(container string) {
	conn, err := openOmnigentDB(container)
	if err != nil {
		return
	}
	defer conn.Close()
	schema, err := detectOmnigentSchema(conn)
	if err != nil {
		return
	}
	metas, err := listOmnigentConversationMetas(conn, schema)
	if err != nil {
		return
	}
	t.replace(container, schema, metas)
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
		if compositeMtime, err := sqliteDBCompositeMtime(src.Container); err == nil {
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
	root string, req ChangedPathRequest,
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
		return nil, err
	}

	t.mu.Lock()
	tracked, initialized := t.containers[match.Container]
	if !initialized {
		t.mu.Unlock()
		metas, err := listOmnigentConversationMetas(conn, schema)
		if err != nil {
			return nil, err
		}
		return omnigentMatches(match.Container, schema, metas), nil
	}
	defer t.mu.Unlock()
	if tracked.schema != schema {
		// Member identities and joins change across schema generations. Reparse
		// the whole container so every new identity is emitted and legacy IDs are
		// retired even when the migrated rows reuse rowids and old timestamps.
		return []multiSessionMatch{match}, nil
	}
	compositeMTimeNS, err := sqliteDBCompositeMtime(match.Container)
	if err != nil {
		return nil, err
	}
	if compositeMTimeNS != tracked.compositeMTimeNS {
		tracked.compositeMTimeNS = compositeMTimeNS
		tracked.probeRemaining = len(tracked.probeKeys)
	}

	checkedAt := time.Now().Unix()
	// Omnigent's store advances updated_at on supported item appends and title
	// changes. Use the last successful wall-clock observation as the cursor,
	// rather than the greatest source timestamp: a future-dated row must not
	// suppress every normally dated conversation. The one-second overlap covers
	// multiple writes within the column's seconds precision.
	var changed []omnigentMeta
	workspaceBatch := omnigentWorkspaceBatch(tracked, schema)
	if schema.splitMetadata {
		for _, workspaceID := range workspaceBatch {
			cutoff := max(tracked.workspaceCheckedAt[workspaceID]-1, 0)
			workspaceMetas, err := listOmnigentConversationMetasSince(
				conn, schema, []int64{workspaceID}, cutoff, checkedAt+1,
			)
			if err != nil {
				return nil, err
			}
			changed = append(changed, workspaceMetas...)
		}
	} else {
		cutoff := max(tracked.checkedAt-1, 0)
		changed, err = listOmnigentConversationMetasSince(
			conn, schema, nil, cutoff, checkedAt+1,
		)
		if err != nil {
			return nil, err
		}
	}
	newRows, err := listOmnigentConversationMetasAfterRowID(
		conn, schema, tracked.maxRowID,
	)
	if err != nil {
		return nil, err
	}
	selected := make(map[string]omnigentMeta, len(changed)+len(newRows))
	selectChanged := func(meta omnigentMeta) {
		key := meta.member().key(schema)
		previous, exists := tracked.metas[meta.member().key(schema)]
		if !exists || previous.fingerprint() != meta.fingerprint() {
			selected[key] = meta
		}
	}
	for _, meta := range changed {
		selectChanged(meta)
	}
	for _, meta := range newRows {
		selectChanged(meta)
	}

	// A bounded rotating probe catches direct/imported transcript edits that
	// bypass every conversation metadata invariant. A physical container change
	// starts one complete probe cycle, spread across scheduled passes, so work per
	// pass stays constant while every tracked member is eventually revisited.
	probeCount := min(
		omnigentProbeBatchSize,
		len(tracked.probeKeys),
		tracked.probeRemaining,
	)
	probeMissing := false
	for i := range probeCount {
		key := tracked.probeKeys[(tracked.probeCursor+i)%len(tracked.probeKeys)]
		previous, stillTracked := tracked.metas[key]
		if !stillTracked {
			continue
		}
		meta, exists, err := loadOmnigentConversationMeta(
			conn, schema, previous.member(),
		)
		if err != nil {
			return nil, err
		}
		if !exists {
			probeMissing = true
			selected[key] = previous
			continue
		}
		selected[key] = meta
	}
	tracked.probeRemaining -= probeCount

	maxRowID, err := omnigentMaxConversationRowID(conn)
	if err != nil {
		return nil, err
	}
	newMemberCount := 0
	for key := range selected {
		if _, exists := tracked.metas[key]; !exists {
			newMemberCount++
		}
	}
	membershipLoss := probeMissing || maxRowID < tracked.maxRowID
	if newMemberCount > 0 {
		count, err := omnigentConversationCount(conn)
		if err != nil {
			return nil, err
		}
		membershipLoss = membershipLoss ||
			newMemberCount > max(0, count-tracked.count)
	}
	if membershipLoss {
		// A net-neutral delete+insert is the only membership transition whose
		// deleted identity cannot be recovered from source timestamps or rowids.
		// Reconcile membership only for that exceptional transition; ordinary
		// transcript events remain bounded by the changed batch and probe size.
		current, err := listOmnigentConversationMetas(conn, schema)
		if err != nil {
			return nil, err
		}
		present := make(map[string]struct{}, len(current))
		for _, meta := range current {
			key := meta.member().key(schema)
			present[key] = struct{}{}
			if _, exists := tracked.metas[key]; !exists {
				selected[key] = meta
			}
		}
		for key := range tracked.metas {
			if _, exists := present[key]; exists {
				continue
			}
			selected[key] = tracked.metas[key]
		}
	}
	metas := make([]omnigentMeta, 0, len(selected))
	for _, meta := range selected {
		metas = append(metas, meta)
	}
	slices.SortFunc(metas, func(a, b omnigentMeta) int {
		return strings.Compare(a.member().key(schema), b.member().key(schema))
	})
	selectedWorkspaces := make(map[int64]struct{}, len(metas))
	for _, meta := range metas {
		selectedWorkspaces[meta.workspaceID] = struct{}{}
	}
	advanceOmnigentClassification(&tracked, checkedAt, probeCount,
		workspaceBatch, selectedWorkspaces, len(metas) == 0)
	t.containers[match.Container] = tracked
	return omnigentMatches(match.Container, schema, metas), nil
}

func omnigentMaxConversationRowID(conn *sql.DB) (int64, error) {
	var maxRowID int64
	if err := conn.QueryRow(
		`SELECT COALESCE(MAX(rowid), 0) FROM conversations`,
	).Scan(&maxRowID); err != nil {
		return 0, fmt.Errorf("reading omnigent conversation cursor: %w", err)
	}
	return maxRowID, nil
}

func omnigentConversationCount(conn *sql.DB) (int, error) {
	var count int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM conversations`).Scan(&count); err != nil {
		return 0, fmt.Errorf("counting omnigent conversations: %w", err)
	}
	return count, nil
}

func listOmnigentConversationMetasSince(
	conn *sql.DB, schema omnigentSchema, workspaceIDs []int64,
	updatedAfter, updatedThrough int64,
) ([]omnigentMeta, error) {
	query := `
		SELECT c.rowid, 0, c.id, COALESCE(c.updated_at, 0),
		       COUNT(ci.id), COALESCE(MAX(ci.position), -1)
		  FROM conversations c
		  LEFT JOIN conversation_items ci ON ci.conversation_id = c.id
		 WHERE c.updated_at >= ?
		   AND c.updated_at <= ?
		 GROUP BY c.id`
	if !schema.splitMetadata {
		return queryOmnigentConversationMetas(
			conn, query, updatedAfter, updatedThrough,
		)
	}
	query = `
			SELECT c.rowid, c.workspace_id, c.id, COALESCE(c.updated_at, 0),
			       COUNT(ci.id), COALESCE(MAX(ci.position), -1)
			  FROM conversations c
			  LEFT JOIN conversation_items ci
			    ON ci.workspace_id = c.workspace_id AND ci.conversation_id = c.id
			 WHERE c.workspace_id = ?
			   AND c.updated_at >= ?
			   AND c.updated_at <= ?
			 GROUP BY c.workspace_id, c.id`
	var metas []omnigentMeta
	for _, workspaceID := range workspaceIDs {
		workspaceMetas, err := queryOmnigentConversationMetas(
			conn, query, workspaceID, updatedAfter, updatedThrough,
		)
		if err != nil {
			return nil, err
		}
		metas = append(metas, workspaceMetas...)
	}
	return metas, nil
}

func queryOmnigentConversationMetas(
	conn *sql.DB, query string, args ...any,
) ([]omnigentMeta, error) {
	rows, err := conn.Query(query, args...)
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

func omnigentWorkspaceBatch(
	tracked omnigentTrackedContainer, schema omnigentSchema,
) []int64 {
	if !schema.splitMetadata || len(tracked.workspaceIDs) == 0 {
		return nil
	}
	start := min(tracked.workspaceCursor, len(tracked.workspaceIDs)-1)
	end := min(start+omnigentWorkspaceBatchSize, len(tracked.workspaceIDs))
	return tracked.workspaceIDs[start:end]
}

func listOmnigentConversationMetasAfterRowID(
	conn *sql.DB, schema omnigentSchema, rowID int64,
) ([]omnigentMeta, error) {
	query := `
		SELECT c.rowid, 0, c.id, COALESCE(c.updated_at, 0),
		       COUNT(ci.id), COALESCE(MAX(ci.position), -1)
		  FROM conversations c
		  LEFT JOIN conversation_items ci ON ci.conversation_id = c.id
		 WHERE c.rowid > ?
		 GROUP BY c.id`
	if schema.splitMetadata {
		query = `
			SELECT c.rowid, c.workspace_id, c.id, COALESCE(c.updated_at, 0),
			       COUNT(ci.id), COALESCE(MAX(ci.position), -1)
			  FROM conversations c
			  LEFT JOIN conversation_items ci
			    ON ci.workspace_id = c.workspace_id AND ci.conversation_id = c.id
			 WHERE c.rowid > ?
			 GROUP BY c.workspace_id, c.id`
	}
	rows, err := conn.Query(query, rowID)
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

func advanceOmnigentClassification(
	tracked *omnigentTrackedContainer, checkedAt int64, probeCount int,
	workspaceBatch []int64, selectedWorkspaces map[int64]struct{}, noChanges bool,
) {
	if len(tracked.probeKeys) > 0 {
		tracked.probeCursor = (tracked.probeCursor + probeCount) % len(tracked.probeKeys)
	}
	if tracked.schema.splitMetadata {
		for _, workspaceID := range workspaceBatch {
			if _, selected := selectedWorkspaces[workspaceID]; !selected {
				tracked.workspaceCheckedAt[workspaceID] = checkedAt
			}
		}
		if len(tracked.workspaceIDs) > 0 {
			tracked.workspaceCursor =
				(tracked.workspaceCursor + len(workspaceBatch)) % len(tracked.workspaceIDs)
		}
	} else if noChanges {
		tracked.checkedAt = checkedAt
	}
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
	compositeMTimeNS, _ := sqliteDBCompositeMtime(container)
	tracked := omnigentTrackedContainer{
		schema: schema, metas: make(map[string]omnigentMeta, len(metas)),
		checkedAt: time.Now().Unix(), count: len(metas),
		compositeMTimeNS:   compositeMTimeNS,
		workspaceCheckedAt: make(map[int64]int64),
		workspaceCounts:    make(map[int64]int),
	}
	for _, meta := range metas {
		key := meta.member().key(schema)
		tracked.metas[key] = meta
		tracked.probeKeys = append(tracked.probeKeys, key)
		if meta.rowID > tracked.maxRowID {
			tracked.maxRowID = meta.rowID
		}
		if schema.splitMetadata {
			tracked.workspaceCounts[meta.workspaceID]++
		}
	}
	slices.Sort(tracked.probeKeys)
	for workspaceID := range tracked.workspaceCounts {
		tracked.workspaceIDs = append(tracked.workspaceIDs, workspaceID)
		tracked.workspaceCheckedAt[workspaceID] = tracked.checkedAt
	}
	slices.Sort(tracked.workspaceIDs)
	t.mu.Lock()
	t.containers[container] = tracked
	t.mu.Unlock()
}

func (t *omnigentChangeTracker) observe(
	container string, schema omnigentSchema, member omnigentMemberID,
	meta omnigentMeta, exists bool, observedAt int64,
) {
	t.mu.Lock()
	defer t.mu.Unlock()
	tracked, ok := t.containers[container]
	if !ok || tracked.schema != schema {
		tracked = omnigentTrackedContainer{
			schema: schema, metas: make(map[string]omnigentMeta),
			workspaceCheckedAt: make(map[int64]int64),
			workspaceCounts:    make(map[int64]int),
		}
	}
	if schema.splitMetadata {
		tracked.workspaceCheckedAt[member.workspaceID] = observedAt
	} else {
		tracked.checkedAt = observedAt
	}
	key := member.key(schema)
	_, wasPresent := tracked.metas[key]
	if exists {
		tracked.metas[key] = meta
		if !wasPresent {
			tracked.count++
			tracked.probeKeys = append(tracked.probeKeys, key)
			slices.Sort(tracked.probeKeys)
			if schema.splitMetadata {
				if tracked.workspaceCounts[meta.workspaceID] == 0 {
					tracked.workspaceIDs = append(tracked.workspaceIDs, meta.workspaceID)
					slices.Sort(tracked.workspaceIDs)
				}
				tracked.workspaceCounts[meta.workspaceID]++
			}
		}
		if meta.rowID > tracked.maxRowID {
			tracked.maxRowID = meta.rowID
		}
	} else {
		previous := tracked.metas[key]
		delete(tracked.metas, key)
		if wasPresent && tracked.count > 0 {
			tracked.count--
		}
		if wasPresent && schema.splitMetadata {
			workspaceID := previous.workspaceID
			tracked.workspaceCounts[workspaceID]--
			if tracked.workspaceCounts[workspaceID] == 0 {
				delete(tracked.workspaceCounts, workspaceID)
				delete(tracked.workspaceCheckedAt, workspaceID)
				tracked.workspaceIDs = slices.DeleteFunc(
					tracked.workspaceIDs,
					func(candidate int64) bool { return candidate == workspaceID },
				)
				if len(tracked.workspaceIDs) == 0 {
					tracked.workspaceCursor = 0
				} else {
					tracked.workspaceCursor %= len(tracked.workspaceIDs)
				}
			}
		}
		tracked.probeKeys = slices.DeleteFunc(
			tracked.probeKeys, func(candidate string) bool { return candidate == key },
		)
		tracked.maxRowID = 0
		for _, current := range tracked.metas {
			if current.rowID > tracked.maxRowID {
				tracked.maxRowID = current.rowID
			}
		}
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
	return ParseVirtualSourcePathForBase(path, omnigentDBName)
}
