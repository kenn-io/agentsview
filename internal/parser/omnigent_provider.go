// ABOUTME: Multi-session container provider for omnigent: one chat.db fanned
// ABOUTME: out into one session per conversation, with incremental sync.
package parser

import (
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type omnigentTrackedContainer struct {
	schema     omnigentSchema
	metas      map[string]omnigentMeta
	maxUpdated int64
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
// It is a multi-session container provider: discovery surfaces the database as
// a single source and Parse fans it out into one session per conversation,
// addressed by "<db>#<conversationID>" virtual paths.
func newOmnigentProviderFactory(def AgentDef) ProviderFactory {
	tracker := newOmnigentChangeTracker()
	return NewMultiSessionProviderFactory(
		def,
		omnigentProviderCapabilities(),
		func(cfg ProviderConfig) multiSessionContainerSourceSet {
			return NewMultiSessionContainerSourceSet(
				AgentOmnigent,
				cfg.Roots,
				WithContainerDiscovery(omnigentDiscoverContainers),
				WithWatchRoots(omnigentWatchRoots),
				WithChangedPathClassifier(omnigentClassifyPath),
				WithChangedPathMembers(tracker.changedMembers),
				WithMemberLookup(omnigentFindMember),
				WithFingerprint(omnigentFingerprintSource),
				WithContainerParse(tracker.parseContainer),
				WithMemberParse(tracker.parseMember),
				WithMemberPresence(omnigentMemberPresent),
				WithUnsupportedSourceError(omnigentSchemaUnsupported),
			)
		},
	)
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
		SELECT 0, c.id, COALESCE(c.updated_at, 0),
		       COUNT(ci.id), COALESCE(MAX(ci.position), -1)
		  FROM conversations c
		  LEFT JOIN conversation_items ci ON ci.conversation_id = c.id
		 WHERE c.id = ?
		 GROUP BY c.id`
	args := []any{member.rawID}
	if schema.splitMetadata {
		query = `
			SELECT c.workspace_id, c.id, COALESCE(c.updated_at, 0),
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
		&meta.workspaceID, &meta.rawID, &meta.updatedAt,
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
	if initialized {
		copied := make(map[string]omnigentMeta, len(tracked.metas))
		maps.Copy(copied, tracked.metas)
		tracked.metas = copied
	}
	t.mu.Unlock()
	if !initialized {
		metas, err := listOmnigentConversationMetas(conn, schema)
		if err != nil {
			return nil, err
		}
		return omnigentMatches(match.Container, schema, metas), nil
	}

	changed, err := listOmnigentConversationMetasSince(
		conn, schema, tracked.maxUpdated,
	)
	if err != nil {
		return nil, err
	}
	selected := make([]omnigentMeta, 0, len(changed))
	for _, meta := range changed {
		previous, exists := tracked.metas[meta.member().key(schema)]
		if !exists || previous.fingerprint() != meta.fingerprint() {
			selected = append(selected, meta)
		}
	}

	count, err := omnigentConversationCount(conn)
	if err != nil {
		return nil, err
	}
	if count < len(tracked.metas) {
		current, err := listOmnigentConversationMetas(conn, schema)
		if err != nil {
			return nil, err
		}
		present := make(map[string]struct{}, len(current))
		for _, meta := range current {
			present[meta.member().key(schema)] = struct{}{}
		}
		for key := range tracked.metas {
			if _, exists := present[key]; exists {
				continue
			}
			selected = append(selected, tracked.metas[key])
		}
	}
	return omnigentMatches(match.Container, schema, selected), nil
}

func omnigentConversationCount(conn *sql.DB) (int, error) {
	var count int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM conversations`).Scan(&count); err != nil {
		return 0, fmt.Errorf("counting omnigent conversations: %w", err)
	}
	return count, nil
}

func listOmnigentConversationMetasSince(
	conn *sql.DB, schema omnigentSchema, updatedAt int64,
) ([]omnigentMeta, error) {
	query := `
		SELECT 0, c.id, COALESCE(c.updated_at, 0),
		       COUNT(ci.id), COALESCE(MAX(ci.position), -1)
		  FROM conversations c
		  LEFT JOIN conversation_items ci ON ci.conversation_id = c.id
		 WHERE COALESCE(c.updated_at, 0) >= ?
		 GROUP BY c.id`
	if schema.splitMetadata {
		query = `
			SELECT c.workspace_id, c.id, COALESCE(c.updated_at, 0),
			       COUNT(ci.id), COALESCE(MAX(ci.position), -1)
			  FROM conversations c
			  LEFT JOIN conversation_items ci
			    ON ci.workspace_id = c.workspace_id AND ci.conversation_id = c.id
			 WHERE COALESCE(c.updated_at, 0) >= ?
			 GROUP BY c.workspace_id, c.id`
	}
	rows, err := conn.Query(query, updatedAt)
	if err != nil {
		return nil, fmt.Errorf("listing changed omnigent conversations: %w", err)
	}
	defer rows.Close()
	var metas []omnigentMeta
	for rows.Next() {
		var meta omnigentMeta
		if err := rows.Scan(&meta.workspaceID, &meta.rawID, &meta.updatedAt,
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
	result, err := omnigentParseMember(src, req)
	if err != nil {
		return nil, err
	}
	conn, openErr := openOmnigentDB(src.Container)
	if openErr != nil {
		return result, nil
	}
	defer conn.Close()
	schema, schemaErr := detectOmnigentSchema(conn)
	if schemaErr != nil {
		return result, nil
	}
	member, memberErr := omnigentMemberForSchema(schema, src.MemberID)
	if memberErr != nil {
		return result, nil
	}
	meta, exists, metaErr := loadOmnigentConversationMeta(conn, schema, member)
	if metaErr == nil {
		t.observe(src.Container, schema, member.key(schema), meta, exists)
	}
	return result, nil
}

func (t *omnigentChangeTracker) replace(
	container string, schema omnigentSchema, metas []omnigentMeta,
) {
	tracked := omnigentTrackedContainer{
		schema: schema, metas: make(map[string]omnigentMeta, len(metas)),
	}
	for _, meta := range metas {
		tracked.metas[meta.member().key(schema)] = meta
		if meta.updatedAt > tracked.maxUpdated {
			tracked.maxUpdated = meta.updatedAt
		}
	}
	t.mu.Lock()
	t.containers[container] = tracked
	t.mu.Unlock()
}

func (t *omnigentChangeTracker) observe(
	container string, schema omnigentSchema, key string, meta omnigentMeta, exists bool,
) {
	t.mu.Lock()
	defer t.mu.Unlock()
	tracked, ok := t.containers[container]
	if !ok || tracked.schema != schema {
		tracked = omnigentTrackedContainer{
			schema: schema, metas: make(map[string]omnigentMeta),
		}
	}
	if exists {
		tracked.metas[key] = meta
		if meta.updatedAt > tracked.maxUpdated {
			tracked.maxUpdated = meta.updatedAt
		}
	} else {
		delete(tracked.metas, key)
		tracked.maxUpdated = 0
		for _, current := range tracked.metas {
			if current.updatedAt > tracked.maxUpdated {
				tracked.maxUpdated = current.updatedAt
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
		conn, schema, src.Container, member, req.Machine, dbInfo, nil,
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
			req.Machine, dbInfo, &metas[i],
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
