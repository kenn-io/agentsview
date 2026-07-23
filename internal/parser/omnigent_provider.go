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

// omnigentChangeTracker remembers each container's schema plus bounded change
// cursors: legacy databases use their indexed updated_at column, while split
// databases combine an indexed single-workspace updated_at cursor with rowid
// high-water marks for newly inserted conversations and immutable items.
// A capped recent-member replay covers separately committed metadata. Cold,
// schema-changed, and forced rebuilds retain a whole-container backstop;
// authoritative reconciliation streams every current member identity.
type omnigentTrackedContainer struct {
	schema            omnigentSchema
	checkedAt         int64
	singleWorkspace   bool
	workspaceID       int64
	conversationRowID int64
	conversationTail  string
	itemRowID         int64
	itemTail          string
	recentMembers     []omnigentRecentMember
}

type omnigentRecentMember struct {
	member     omnigentMemberID
	observedAt int64
}

const (
	omnigentChangePageSize    = 128
	omnigentRecentMemberTTL   = 15 * time.Minute
	omnigentRecentMemberLimit = omnigentChangePageSize
)

type omnigentChangeTracker struct {
	mu         sync.Mutex
	containers map[string]omnigentTrackedContainer
}

type omnigentSourceSet struct {
	multiSessionContainerSourceSet
	tracker            *omnigentChangeTracker
	forceFullDiscovery bool
}

func newOmnigentChangeTracker() *omnigentChangeTracker {
	return &omnigentChangeTracker{
		containers: make(map[string]omnigentTrackedContainer),
	}
}

// Omnigent stores every conversation in one shared SQLite database (chat.db).
// It is a multi-session container provider: discovery surfaces the database as
// one source whose parse fans out into one session per conversation, addressed
// by "<db>#<conversationID>" virtual paths, and watcher events fan out only
// the members changed since the tracker's last sweep.
func newOmnigentProviderFactory(def AgentDef) ProviderFactory {
	tracker := newOmnigentChangeTracker()
	return NewSourceSetFactory(
		def,
		omnigentProviderCapabilities(),
		func(cfg ProviderConfig) SourceSet {
			base := NewMultiSessionContainerSourceSet(
				AgentOmnigent,
				cfg.Roots,
				WithContainerDiscovery(omnigentDiscoverContainers),
				WithWatchRoots(omnigentWatchRoots),
				WithChangedPathClassifier(omnigentClassifyPath),
				WithChangedPathMembers(tracker.changedMembers),
				WithMemberLookup(omnigentFindMember),
				WithFingerprint(omnigentFingerprintSource),
				WithContainerParse(tracker.parseContainer),
				WithMemberParse(omnigentParseMember),
				WithMemberResultHashPreservation(),
				WithMemberPresence(omnigentMemberPresent),
				WithUnsupportedSourceError(omnigentSchemaUnsupported),
				WithExcludedSessionIDs(omnigentLegacySessionIDs),
			)
			return omnigentSourceSet{
				multiSessionContainerSourceSet: base,
				tracker:                        tracker,
				forceFullDiscovery:             cfg.ForceFullDiscovery,
			}
		},
	)
}

func (s omnigentSourceSet) Discover(
	ctx context.Context,
) ([]SourceRef, error) {
	var sources []SourceRef
	seen := make(map[string]struct{})
	for _, root := range s.roots {
		matches, err := s.tracker.discoveryMatches(
			ctx, root, s.forceFullDiscovery,
		)
		if err != nil {
			return nil, err
		}
		for _, match := range matches {
			addJSONLSource(s.sourceRef(root, match), &sources, seen)
		}
	}
	sortJSONLSources(sources)
	return sources, nil
}

func (s omnigentSourceSet) DiscoverEach(
	ctx context.Context, yield func(SourceRef) error,
) error {
	for _, root := range s.roots {
		if s.forceFullDiscovery {
			if err := streamOmnigentMemberMatches(
				ctx, root,
				func(match multiSessionMatch) error {
					return yield(s.sourceRef(root, match))
				},
			); err != nil {
				return err
			}
			continue
		}
		matches, err := s.tracker.discoveryMatches(
			ctx, root, false,
		)
		if err != nil {
			return err
		}
		for _, match := range matches {
			if err := yield(s.sourceRef(root, match)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s omnigentSourceSet) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	if !s.forceFullDiscovery {
		return s.multiSessionContainerSourceSet.SourcesForChangedPath(ctx, req)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var sources []SourceRef
	seen := make(map[string]struct{})
	for _, root := range s.roots {
		match, ok := omnigentClassifyPath(root, req.Path, true)
		if !ok {
			continue
		}
		match.Path = match.Container
		match.MemberID = ""
		addJSONLSource(s.sourceRef(root, match), &sources, seen)
	}
	sortJSONLSources(sources)
	return sources, nil
}

func streamOmnigentMemberMatches(
	ctx context.Context, root string, yield func(multiSessionMatch) error,
) error {
	dbPath := omnigentDBPath(root)
	if dbPath == "" {
		return nil
	}
	conn, err := openOmnigentDB(dbPath)
	if err != nil {
		return err
	}
	defer conn.Close()
	schema, err := detectOmnigentSchema(conn)
	if err != nil {
		return err
	}
	idExpr := omnigentIDExpr(schema, "id")
	query := `SELECT 0, ` + idExpr + ` FROM conversations ORDER BY id`
	if schema.splitMetadata {
		query = `SELECT workspace_id, ` + idExpr +
			` FROM conversations ORDER BY workspace_id, id`
	}
	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("streaming omnigent member keys: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var member omnigentMemberID
		if err := rows.Scan(&member.workspaceID, &member.rawID); err != nil {
			return fmt.Errorf("scanning omnigent member key: %w", err)
		}
		key := member.key(schema)
		if err := yield(multiSessionMatch{
			Path:      VirtualSourcePath(dbPath, key),
			Container: dbPath,
			MemberID:  key,
		}); err != nil {
			return err
		}
	}
	return rows.Err()
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
	// A member ID that no longer parses under the detected schema identifies
	// a retired legacy member (pre-schema-change identity), not a failure.
	member, err := omnigentMemberForSchema(schema, src.MemberID)
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
	idExpr := omnigentIDExpr(schema, "c.id")
	query := `
		SELECT c.rowid, 0, ` + idExpr + `, COALESCE(c.updated_at, 0),
		       COUNT(ci.id), COALESCE(MAX(ci.position), -1)
		  FROM conversations c
		  LEFT JOIN conversation_items ci ON ci.conversation_id = c.id
		 WHERE c.id = ?
		 GROUP BY c.id`
	args := []any{omnigentIDArg(schema, member.rawID)}
	if schema.splitMetadata {
		query = `
			SELECT c.rowid, c.workspace_id, ` + idExpr + `, COALESCE(c.updated_at, 0),
			       COUNT(ci.id), COALESCE(MAX(ci.position), -1)
			  FROM conversations c
			  LEFT JOIN conversation_items ci
			    ON ci.workspace_id = c.workspace_id AND ci.conversation_id = c.id
			 WHERE c.workspace_id = ? AND c.id = ?
			 GROUP BY c.workspace_id, c.id`
		args = []any{member.workspaceID, omnigentIDArg(schema, member.rawID)}
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
	return t.matchesSince(ctx, conn, schema, match, false)
}

func (t *omnigentChangeTracker) discoveryMatches(
	ctx context.Context, root string, forceFullDiscovery bool,
) ([]multiSessionMatch, error) {
	dbPath := omnigentDBPath(root)
	if dbPath == "" {
		return nil, nil
	}
	match, ok := omnigentClassifyPath(root, dbPath, false)
	if !ok {
		return nil, nil
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
	return t.matchesSince(
		ctx, conn, schema, match, forceFullDiscovery,
	)
}

func (t *omnigentChangeTracker) matchesSince(
	ctx context.Context, conn *sql.DB, schema omnigentSchema,
	match multiSessionMatch, forceFullDiscovery bool,
) ([]multiSessionMatch, error) {
	t.mu.Lock()
	tracked, warm := t.containers[match.Container]
	t.mu.Unlock()
	if forceFullDiscovery || !warm || tracked.schema != schema {
		// A cold or schema-changed container parses whole: the complete
		// result set reconciles archived membership and seeds the floor.
		return []multiSessionMatch{match}, nil
	}
	if schema.splitMetadata {
		return t.splitSchemaMatchesSince(
			ctx, conn, schema, match, tracked,
		)
	}
	// Capture the new floor before querying so a commit that lands during
	// the sweep is re-observed by the next event instead of skipped. The
	// window's upper bound keeps a clock-skewed future updated_at from
	// re-surfacing on every sweep; such rows wait for the next full parse.
	checkedAt := time.Now().Unix()
	changed, err := listOmnigentConversationMetasSince(
		ctx, conn, schema, max(tracked.checkedAt-1, 0), checkedAt+1,
	)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	if current, ok := t.containers[match.Container]; ok && current.schema == schema {
		current.checkedAt = checkedAt
		t.containers[match.Container] = current
	}
	t.mu.Unlock()
	return omnigentMatches(match.Container, schema, changed), nil
}

func (t *omnigentChangeTracker) splitSchemaMatchesSince(
	ctx context.Context, conn *sql.DB, schema omnigentSchema,
	match multiSessionMatch, tracked omnigentTrackedContainer,
) ([]multiSessionMatch, error) {
	checkedAt := time.Now().Unix()
	conversationCursor, conversationTail, reconcile, err :=
		normalizeOmnigentRowIDCursor(
			ctx, conn, tracked.conversationRowID, tracked.conversationTail,
			func(rowID int64) (string, bool, error) {
				return omnigentConversationRowIdentity(ctx, conn, schema, rowID)
			},
			func() (int64, string, error) {
				return omnigentLatestConversationRow(ctx, conn, schema)
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
	changed := newConversations
	if tracked.singleWorkspace {
		updated, updateErr := listOmnigentSplitConversationMetasSince(
			ctx, conn, schema, tracked.workspaceID,
			max(tracked.checkedAt-1, 0), checkedAt+1,
		)
		if updateErr != nil {
			return nil, updateErr
		}
		changed = append(changed, updated...)
	}
	itemCursor, itemTail, reconcile, err := normalizeOmnigentRowIDCursor(
		ctx, conn, tracked.itemRowID, tracked.itemTail,
		func(rowID int64) (string, bool, error) {
			return omnigentItemRowIdentity(ctx, conn, schema, rowID)
		},
		func() (int64, string, error) {
			return omnigentLatestItemRow(ctx, conn, schema)
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
	metaByKey := make(map[string]omnigentMeta, len(changed))
	candidates := make([]omnigentMemberID, 0, len(changed)+len(members)+len(tracked.recentMembers))
	observedAt := make(map[string]int64, cap(candidates))
	addCandidate := func(member omnigentMemberID, observed int64) {
		key := member.key(schema)
		if prior, ok := observedAt[key]; ok {
			if observed > prior {
				observedAt[key] = observed
			}
			return
		}
		observedAt[key] = observed
		candidates = append(candidates, member)
	}
	for _, meta := range changed {
		member := meta.member()
		metaByKey[member.key(schema)] = meta
		addCandidate(member, checkedAt)
	}
	for _, member := range members {
		addCandidate(member, checkedAt)
	}
	recentCutoff := checkedAt - int64(omnigentRecentMemberTTL/time.Second)
	for _, recent := range tracked.recentMembers {
		if recent.observedAt >= recentCutoff {
			addCandidate(recent.member, recent.observedAt)
		}
	}

	changed = changed[:0]
	recentMembers := make([]omnigentRecentMember, 0, min(
		len(candidates), omnigentRecentMemberLimit,
	))
	for _, member := range candidates {
		key := member.key(schema)
		meta, present := metaByKey[key]
		if !present {
			var loadErr error
			meta, present, loadErr = loadOmnigentConversationMeta(
				conn, schema, member,
			)
			if loadErr != nil {
				return nil, loadErr
			}
		}
		if present {
			changed = append(changed, meta)
			if len(recentMembers) < omnigentRecentMemberLimit {
				recentMembers = append(recentMembers, omnigentRecentMember{
					member: member, observedAt: observedAt[key],
				})
			}
		}
	}

	t.mu.Lock()
	if current, ok := t.containers[match.Container]; ok &&
		current.schema == schema {
		current.checkedAt = checkedAt
		current.conversationRowID = conversationRowID
		current.conversationTail = conversationTail
		current.itemRowID = itemRowID
		current.itemTail = itemTail
		current.recentMembers = recentMembers
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

func omnigentConversationRowIdentity(
	ctx context.Context, conn *sql.DB, schema omnigentSchema, rowID int64,
) (string, bool, error) {
	if rowID == 0 {
		return "", false, nil
	}
	idExpr := omnigentIDExpr(schema, "id")
	var member omnigentMemberID
	err := conn.QueryRowContext(
		ctx,
		`SELECT workspace_id, `+idExpr+`
		   FROM conversations
		  WHERE rowid = ?`,
		rowID,
	).Scan(&member.workspaceID, &member.rawID)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf(
			"reading omnigent conversation row identity: %w", err,
		)
	}
	return member.key(schema), true, nil
}

func omnigentLatestConversationRow(
	ctx context.Context, conn *sql.DB, schema omnigentSchema,
) (int64, string, error) {
	idExpr := omnigentIDExpr(schema, "id")
	var rowID int64
	var member omnigentMemberID
	err := conn.QueryRowContext(
		ctx,
		`SELECT rowid, workspace_id, `+idExpr+`
		   FROM conversations
		  ORDER BY rowid DESC
		  LIMIT 1`,
	).Scan(&rowID, &member.workspaceID, &member.rawID)
	if err == sql.ErrNoRows {
		return 0, "", nil
	}
	if err != nil {
		return 0, "", fmt.Errorf(
			"reading latest omnigent conversation row: %w", err,
		)
	}
	return rowID, member.key(schema), nil
}

func omnigentItemRowIdentity(
	ctx context.Context, conn *sql.DB, schema omnigentSchema, rowID int64,
) (string, bool, error) {
	if rowID == 0 {
		return "", false, nil
	}
	conversationExpr := omnigentIDExpr(schema, "conversation_id")
	itemExpr := omnigentIDExpr(schema, "id")
	workspaceExpr := "0"
	if schema.splitMetadata {
		workspaceExpr = "workspace_id"
	}
	var member omnigentMemberID
	var itemID string
	err := conn.QueryRowContext(
		ctx,
		`SELECT `+workspaceExpr+`, `+conversationExpr+`, `+itemExpr+`
		   FROM conversation_items
		  WHERE rowid = ?`,
		rowID,
	).Scan(&member.workspaceID, &member.rawID, &itemID)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf(
			"reading omnigent item row identity: %w", err,
		)
	}
	return member.key(schema) + ":" + itemID, true, nil
}

func omnigentLatestItemRow(
	ctx context.Context, conn *sql.DB, schema omnigentSchema,
) (int64, string, error) {
	conversationExpr := omnigentIDExpr(schema, "conversation_id")
	itemExpr := omnigentIDExpr(schema, "id")
	workspaceExpr := "0"
	if schema.splitMetadata {
		workspaceExpr = "workspace_id"
	}
	var rowID int64
	var member omnigentMemberID
	var itemID string
	err := conn.QueryRowContext(
		ctx,
		`SELECT rowid, `+workspaceExpr+`, `+conversationExpr+`, `+itemExpr+`
		   FROM conversation_items
		  ORDER BY rowid DESC
		  LIMIT 1`,
	).Scan(&rowID, &member.workspaceID, &member.rawID, &itemID)
	if err == sql.ErrNoRows {
		return 0, "", nil
	}
	if err != nil {
		return 0, "", fmt.Errorf(
			"reading latest omnigent item row: %w", err,
		)
	}
	return rowID, member.key(schema) + ":" + itemID, nil
}

func listOmnigentConversationMetasSince(
	ctx context.Context, conn *sql.DB, schema omnigentSchema,
	updatedAfter, updatedThrough int64,
) ([]omnigentMeta, error) {
	return queryOmnigentConversationMetas(
		ctx, conn, omnigentChangedMetaQuery(schema),
		updatedAfter, updatedThrough,
	)
}

func omnigentChangedMetaQuery(schema omnigentSchema) string {
	idExpr := omnigentIDExpr(schema, "c.id")
	query := `
		WITH selected AS MATERIALIZED (
			SELECT rowid, id, COALESCE(updated_at, 0) AS updated_at
			  FROM conversations
			 WHERE updated_at >= ?
			   AND updated_at <= ?
		)
		SELECT c.rowid, 0, ` + idExpr + `, c.updated_at,
		       COUNT(ci.id), COALESCE(MAX(ci.position), -1)
		  FROM selected c
		  LEFT JOIN conversation_items ci ON ci.conversation_id = c.id
		 GROUP BY c.id
		 ORDER BY c.updated_at, c.rowid`
	return query
}

func listOmnigentSplitConversationMetasSince(
	ctx context.Context, conn *sql.DB, schema omnigentSchema,
	workspaceID, updatedAfter, updatedThrough int64,
) ([]omnigentMeta, error) {
	args := []any{workspaceID, updatedAfter, updatedThrough}
	if schema.changeIndexArchived {
		args = append(args, workspaceID, updatedAfter, updatedThrough)
	}
	return queryOmnigentConversationMetas(
		ctx, conn, omnigentSplitChangedMetaQuery(schema), args...,
	)
}

func omnigentSplitChangedMetaQuery(schema omnigentSchema) string {
	idExpr := omnigentIDExpr(schema, "c.id")
	indexName := fmt.Sprintf("%q", schema.changeIndexName)
	selected := `
			SELECT rowid, workspace_id, id,
			       COALESCE(updated_at, 0) AS updated_at
			  FROM conversations INDEXED BY ` + indexName + `
			 WHERE workspace_id = ?
			   AND updated_at >= ?
			   AND updated_at <= ?`
	if schema.changeIndexArchived {
		selected = `
			SELECT rowid, workspace_id, id,
			       COALESCE(updated_at, 0) AS updated_at
			  FROM conversations INDEXED BY ` + indexName + `
			 WHERE workspace_id = ?
			   AND archived = 0
			   AND updated_at >= ?
			   AND updated_at <= ?
			UNION ALL
			SELECT rowid, workspace_id, id,
			       COALESCE(updated_at, 0) AS updated_at
			  FROM conversations INDEXED BY ` + indexName + `
			 WHERE workspace_id = ?
			   AND archived = 1
			   AND updated_at >= ?
			   AND updated_at <= ?`
	}
	return `
		WITH selected AS MATERIALIZED (` + selected + `
		)
		SELECT c.rowid, c.workspace_id, ` + idExpr + `, c.updated_at,
		       COUNT(ci.id), COALESCE(MAX(ci.position), -1)
		  FROM selected c
		  LEFT JOIN conversation_items ci
		    ON ci.workspace_id = c.workspace_id AND ci.conversation_id = c.id
		 GROUP BY c.workspace_id, c.id
		 ORDER BY c.updated_at, c.rowid`
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
		tailIdentity = page[len(page)-1].member().key(schema)
		if len(page) < omnigentChangePageSize {
			return out, cursor, tailIdentity, nil
		}
	}
}

func omnigentNewConversationQuery(schema omnigentSchema) string {
	idExpr := omnigentIDExpr(schema, "c.id")
	return `
		WITH selected AS MATERIALIZED (
			SELECT rowid, workspace_id, id,
			       COALESCE(updated_at, 0) AS updated_at
			  FROM conversations
			 WHERE rowid > ?
			 ORDER BY rowid
			 LIMIT ?
		)
		SELECT c.rowid, c.workspace_id, ` + idExpr + `, c.updated_at,
		       COUNT(ci.id), COALESCE(MAX(ci.position), -1)
		  FROM selected c
		  LEFT JOIN conversation_items ci
		    ON ci.workspace_id = c.workspace_id AND ci.conversation_id = c.id
		 GROUP BY c.workspace_id, c.id
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
			tailIdentity = member.key(schema) + ":" + itemID
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

func omnigentSingleWorkspace(metas []omnigentMeta) (int64, bool) {
	if len(metas) == 0 {
		return 0, false
	}
	workspaceID := metas[0].workspaceID
	for _, meta := range metas[1:] {
		if meta.workspaceID != workspaceID {
			return 0, false
		}
	}
	return workspaceID, true
}

func omnigentMostRecentMembers(
	metas []omnigentMeta, observedAt int64,
) []omnigentRecentMember {
	selected := make([]omnigentMeta, 0, min(
		len(metas), omnigentRecentMemberLimit,
	))
	for _, meta := range metas {
		insertAt := len(selected)
		for i, current := range selected {
			if meta.updatedAt > current.updatedAt {
				insertAt = i
				break
			}
		}
		if insertAt >= omnigentRecentMemberLimit {
			continue
		}
		selected = append(selected, omnigentMeta{})
		copy(selected[insertAt+1:], selected[insertAt:])
		selected[insertAt] = meta
		if len(selected) > omnigentRecentMemberLimit {
			selected = selected[:omnigentRecentMemberLimit]
		}
	}
	recent := make([]omnigentRecentMember, 0, len(selected))
	for _, meta := range selected {
		recent = append(recent, omnigentRecentMember{
			member: meta.member(), observedAt: observedAt,
		})
	}
	return recent
}

func (t *omnigentChangeTracker) parseContainer(
	src multiSessionSource, req ParseRequest,
) ([]ParseResult, error) {
	// Capture the floor before reading so a commit that lands during the
	// parse is re-observed by the next changed-member sweep.
	checkedAt := time.Now().Unix()
	results, schema, metas, itemRowID, itemTail, err := omnigentParseContainerData(
		src, req,
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
				conversationTail = meta.member().key(schema)
			}
		}
		workspaceID, singleWorkspace := omnigentSingleWorkspace(metas)
		t.mu.Lock()
		t.containers[src.Container] = omnigentTrackedContainer{
			schema:            schema,
			checkedAt:         checkedAt,
			singleWorkspace:   singleWorkspace,
			workspaceID:       workspaceID,
			conversationRowID: conversationRowID,
			conversationTail:  conversationTail,
			itemRowID:         itemRowID,
			itemTail:          itemTail,
			recentMembers:     omnigentMostRecentMembers(metas, checkedAt),
		}
		t.mu.Unlock()
	}
	return results, nil
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
	// A member ID that no longer parses under the detected schema is a retired
	// legacy identity; a nil result retires its archived session.
	member, err := omnigentMemberForSchema(schema, src.MemberID)
	if err != nil {
		return nil, nil
	}
	return parseOmnigentConversationFromDB(
		conn, schema, src.Container, member, req.Machine, dbInfo,
	)
}

func omnigentParseContainerData(
	src multiSessionSource, req ParseRequest,
) ([]ParseResult, omnigentSchema, []omnigentMeta, int64, string, error) {
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
	metas, err := listOmnigentConversationMetas(conn, schema)
	if err != nil {
		return nil, omnigentSchema{}, nil, 0, "", err
	}
	itemRowID, itemTail, err := omnigentLatestItemRow(
		context.Background(), conn, schema,
	)
	if err != nil {
		return nil, omnigentSchema{}, nil, 0, "", err
	}
	results := make([]ParseResult, 0, len(metas))
	for i := range metas {
		result, err := parseOmnigentConversationFromDB(
			conn, schema, src.Container, metas[i].member(),
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
	// The provider's own read connections update the WAL shared-memory
	// file's mtime, so treating -shm events as source changes would make
	// every sweep trigger the next one, a permanent watcher loop. Real
	// commits always touch the database file or -wal as well.
	if strings.HasSuffix(path, "-shm") {
		return "", false
	}
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
