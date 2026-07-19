package parser

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const traeStateDBName = "state.vscdb"
const traeStorageKey = "memento/icube-ai-agent-storage"

func newTraeProviderFactory(def AgentDef) ProviderFactory {
	memberships := newTraeMembershipCache()
	memberPresent := newTraeMemberPresence(memberships)
	return NewMultiSessionProviderFactory(def, traeProviderCapabilities(), func(cfg ProviderConfig) multiSessionContainerSourceSet {
		return NewMultiSessionContainerSourceSet(AgentTrae, cfg.Roots,
			WithContainerDiscovery(traeDiscoverContainers),
			WithWatchRoots(traeWatchRoots),
			WithChangedPathClassifier(traeClassifyPath),
			WithMemberLookup(func(root, rawID string) (multiSessionMatch, bool) {
				return traeFindMember(root, rawID, memberships)
			}),
			WithFingerprint(traeFingerprintSource),
			WithContainerParseOutcome(traeParseContainerOutcome),
			WithMemberParse(traeParseMember),
			WithMemberPresence(memberPresent),
		)
	})
}

type traeDB struct{ path, project string }

func traeDiscoverContainers(root string) []string {
	dbs := traeDBs(root)
	paths := make([]string, 0, len(dbs))
	for _, db := range dbs {
		paths = append(paths, db.path)
	}
	return paths
}

func traeDBs(root string) []traeDB {
	var dbs []traeDB
	workspace := filepath.Join(filepath.Clean(root), "workspaceStorage")
	if entries, err := os.ReadDir(workspace); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			path := filepath.Join(workspace, entry.Name(), traeStateDBName)
			if info, err := os.Stat(path); err == nil && !info.IsDir() {
				dbs = append(dbs, traeDB{path, traeWorkspaceProject(path)})
			}
		}
	}
	global := filepath.Join(filepath.Clean(root), "globalStorage", traeStateDBName)
	if info, err := os.Stat(global); err == nil && !info.IsDir() {
		dbs = append(dbs, traeDB{global, "unknown"})
	}
	sort.Slice(dbs, func(i, j int) bool { return dbs[i].path < dbs[j].path })
	return dbs
}

func traeWorkspaceProject(path string) string {
	project := readVSCodeWorkspaceManifest(filepath.Dir(path))
	if project == "" {
		return "unknown"
	}
	return project
}

func traeWatchRoots(configured []string) []WatchRoot {
	var roots []WatchRoot
	for _, root := range configured {
		for _, subdir := range []string{"workspaceStorage", "globalStorage"} {
			path := filepath.Join(filepath.Clean(root), subdir)
			roots = append(roots, WatchRoot{Path: path, Recursive: subdir == "workspaceStorage", IncludeGlobs: []string{traeStateDBName, traeStateDBName + "-wal", "workspace.json"}, DebounceKey: string(AgentTrae) + ":" + subdir + ":" + path})
		}
	}
	return roots
}

func traeClassifyPath(root, path string, allowMissing bool) (multiSessionMatch, bool) {
	path = filepath.Clean(path)
	if dbPath, id, ok := splitTraeVirtualPath(path); ok && traeDBBelongsToRoot(root, dbPath, !allowMissing) {
		return traeMatch(dbPath, id), true
	}
	if traeDBBelongsToRoot(root, path, !allowMissing) {
		return traeMatch(path, ""), true
	}
	if allowMissing {
		if dbPath, ok := traeDBPathForEvent(root, path); ok {
			return traeMatch(dbPath, ""), true
		}
	}
	return multiSessionMatch{}, false
}

func traeMatch(dbPath, id string) multiSessionMatch {
	project := "unknown"
	if filepath.Base(filepath.Dir(dbPath)) != "globalStorage" {
		project = traeWorkspaceProject(dbPath)
	}
	path := dbPath
	if id != "" {
		path = traeVirtualPath(dbPath, id)
	}
	return multiSessionMatch{Path: path, Container: dbPath, MemberID: id, ProjectHint: project}
}

func traeFindMember(
	root, rawID string,
	memberships *traeMembershipCache,
) (multiSessionMatch, bool) {
	for _, db := range traeDBs(root) {
		snapshot, err := memberships.load(db.path)
		if err != nil {
			continue
		}
		if _, ok := snapshot.ids[rawID]; ok {
			return traeMatch(db.path, rawID), true
		}
	}
	return multiSessionMatch{}, false
}

func traeFingerprintSource(src multiSessionSource) (SourceFingerprint, error) {
	info, err := os.Stat(src.Container)
	if err != nil {
		if os.IsNotExist(err) {
			return SourceFingerprint{}, nil
		}
		return SourceFingerprint{}, err
	}
	manifest := ""
	if filepath.Base(filepath.Dir(src.Container)) != "globalStorage" {
		manifest = windsurfWorkspaceManifestPath(src.Container)
	}
	combined := antigravityCLICombinedFileInfo(info, src.Container+"-wal", manifest)
	hash, err := traeSourceHash(src.Container, manifest)
	if err != nil {
		return SourceFingerprint{}, err
	}
	return SourceFingerprint{Size: combined.Size(), MTimeNS: combined.ModTime().UnixNano(), Hash: hash}, nil
}

func traeParseContainerOutcome(
	src multiSessionSource,
	req ParseRequest,
) (ParseOutcome, error) {
	snapshot, err := loadTraeSessionSnapshot(src.Container)
	if err != nil {
		return ParseOutcome{}, err
	}
	if !snapshot.authoritative {
		return ParseOutcome{
			ResultSetComplete: false,
			SkipReason:        SkipNoSession,
		}, nil
	}
	results := make([]ParseResultOutcome, 0, len(snapshot.records))
	for _, record := range snapshot.records {
		result, err := traeParseRecord(src, record, req)
		if err != nil {
			return ParseOutcome{}, err
		}
		if result != nil {
			results = append(results, ParseResultOutcome{
				Result:      *result,
				DataVersion: DataVersionCurrent,
			})
		}
	}
	if len(results) == 0 {
		return ParseOutcome{
			ResultSetComplete: snapshot.complete,
			ForceReplace:      snapshot.complete,
			SkipReason:        SkipNoSession,
		}, nil
	}
	return ParseOutcome{
		Results:           results,
		ResultSetComplete: snapshot.complete,
		ForceReplace:      true,
	}, nil
}

func traeParseMember(
	src multiSessionSource,
	req ParseRequest,
) (*ParseResult, error) {
	snapshot, err := loadTraeSessionSnapshot(src.Container)
	if err != nil {
		return nil, err
	}
	if !snapshot.authoritative {
		return nil, fmt.Errorf("trae storage in %s is not an explicit session list", src.Container)
	}
	record, ok := snapshot.record(strings.TrimPrefix(src.MemberID, "trae:"))
	if !ok && !snapshot.complete {
		return nil, fmt.Errorf("trae storage in %s contains malformed session entries", src.Container)
	}
	if !ok {
		return nil, nil
	}
	result, err := traeParseRecord(src, record, req)
	if err != nil || !ok {
		return nil, err
	}
	return result, nil
}

func traeParseRecord(src multiSessionSource, record traeSessionRecord, req ParseRequest) (*ParseResult, error) {
	sess, msgs := parseTraeSessionRecord(
		record.Session,
		req.Source.ProjectHint,
		req.Machine,
		traeVirtualPath(src.Container, record.SessionID),
	)
	if sess == nil {
		return nil, nil
	}
	sess.File.Hash = traeRecordHash(record.Hash, req.Source.ProjectHint)
	return &ParseResult{Session: *sess, Messages: msgs}, nil
}

func newTraeMemberPresence(memberships *traeMembershipCache) func(src multiSessionSource) bool {
	return func(src multiSessionSource) bool {
		if src.MemberID == "" {
			return IsRegularFile(src.Container)
		}
		if !IsRegularFile(src.Container) {
			memberships.delete(src.Container)
			return false
		}
		snapshot, err := memberships.load(src.Container)
		if err != nil {
			return true
		}
		if !snapshot.authoritative || !snapshot.complete {
			return true
		}
		_, ok := snapshot.ids[strings.TrimPrefix(src.MemberID, "trae:")]
		return ok
	}
}

type traeMembershipCacheEntry struct {
	state SQLiteContainerState
	data  traeSessionMembership
}

type traeMembershipCache struct {
	mu     sync.RWMutex
	byPath map[string]traeMembershipCacheEntry
}

func newTraeMembershipCache() *traeMembershipCache {
	return &traeMembershipCache{
		byPath: make(map[string]traeMembershipCacheEntry),
	}
}

func (c *traeMembershipCache) load(path string) (traeSessionMembership, error) {
	state, stateOK := StatSQLiteContainerState(path)
	if stateOK {
		c.mu.RLock()
		entry, ok := c.byPath[path]
		c.mu.RUnlock()
		if ok && entry.state == state {
			return entry.data, nil
		}
	}

	snapshot, err := loadTraeSessionMembership(path)
	if err != nil {
		c.delete(path)
		return traeSessionMembership{}, err
	}
	if !stateOK {
		c.delete(path)
		return snapshot, nil
	}
	c.mu.Lock()
	c.byPath[path] = traeMembershipCacheEntry{state: state, data: snapshot}
	c.mu.Unlock()
	return snapshot, nil
}

func (c *traeMembershipCache) delete(path string) {
	c.mu.Lock()
	delete(c.byPath, path)
	c.mu.Unlock()
}

func traeDBPathForEvent(root, path string) (string, bool) {
	for _, subdir := range []string{"workspaceStorage", "globalStorage"} {
		watch := filepath.Join(filepath.Clean(root), subdir)
		rel, ok := relUnder(watch, filepath.Clean(path))
		if !ok {
			continue
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if subdir == "workspaceStorage" && len(parts) == 2 && (parts[1] == traeStateDBName || parts[1] == traeStateDBName+"-wal" || parts[1] == "workspace.json") {
			return filepath.Join(watch, parts[0], traeStateDBName), true
		}
		if subdir == "globalStorage" && len(parts) == 1 && (parts[0] == traeStateDBName || parts[0] == traeStateDBName+"-wal") {
			return filepath.Join(watch, traeStateDBName), true
		}
	}
	return "", false
}

func traeDBBelongsToRoot(root, path string, requireRegular bool) bool {
	for _, subdir := range []string{"workspaceStorage", "globalStorage"} {
		rel, ok := relUnder(filepath.Join(filepath.Clean(root), subdir), filepath.Clean(path))
		if !ok {
			continue
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		valid := (subdir == "globalStorage" && len(parts) == 1 && parts[0] == traeStateDBName) || (subdir == "workspaceStorage" && len(parts) == 2 && parts[1] == traeStateDBName)
		if valid {
			return !requireRegular || IsRegularFile(path)
		}
	}
	return false
}

type traeSessionRecord struct {
	SessionID string
	Session   traeSession
	Hash      string
}

type traeSessionSnapshot struct {
	records       []traeSessionRecord
	ids           map[string]struct{}
	authoritative bool
	complete      bool
}

type traeSessionMembership struct {
	ids           map[string]struct{}
	authoritative bool
	complete      bool
}

func (s traeSessionSnapshot) record(id string) (traeSessionRecord, bool) {
	for _, record := range s.records {
		if record.SessionID == id {
			return record, true
		}
	}
	return traeSessionRecord{}, false
}

func loadTraeSessionSnapshot(path string) (traeSessionSnapshot, error) {
	value, err := readTraeValue(path)
	if err != nil {
		return traeSessionSnapshot{}, err
	}
	return decodeTraeSessionSnapshot(value)
}

func loadTraeSessionMembership(path string) (traeSessionMembership, error) {
	snapshot, err := loadTraeSessionSnapshot(path)
	if err != nil {
		return traeSessionMembership{}, err
	}
	return snapshot.membership(), nil
}

func (s traeSessionSnapshot) membership() traeSessionMembership {
	ids := make(map[string]struct{}, len(s.ids))
	for id := range s.ids {
		ids[id] = struct{}{}
	}
	return traeSessionMembership{
		ids:           ids,
		authoritative: s.authoritative,
		complete:      s.complete,
	}
}

func decodeTraeSessionSnapshot(value string) (traeSessionSnapshot, error) {
	snapshot := traeSessionSnapshot{
		ids:      map[string]struct{}{},
		complete: true,
	}
	if value == "" {
		snapshot.complete = false
		return snapshot, nil
	}
	var store map[string]json.RawMessage
	if err := json.Unmarshal([]byte(value), &store); err != nil {
		return traeSessionSnapshot{}, err
	}
	rawList, ok := store["list"]
	if !ok || !traeExplicitList(rawList) {
		snapshot.complete = false
		return snapshot, nil
	}
	snapshot.authoritative = true
	var list []json.RawMessage
	if err := json.Unmarshal(rawList, &list); err != nil {
		return traeSessionSnapshot{}, err
	}
	snapshot.records = make([]traeSessionRecord, 0, len(list))
	seen := map[string]struct{}{}
	for _, raw := range list {
		var session traeSession
		if err := json.Unmarshal(raw, &session); err != nil {
			snapshot.complete = false
			continue
		}
		id := strings.TrimSpace(session.SessionID)
		if id == "" || len(session.Messages) == 0 {
			snapshot.complete = false
			continue
		}
		if !traeSessionProducesMessages(session) {
			snapshot.complete = false
			continue
		}
		if _, ok := seen[id]; ok {
			snapshot.complete = false
			continue
		}
		seen[id] = struct{}{}
		snapshot.ids[id] = struct{}{}
		snapshot.records = append(snapshot.records, traeSessionRecord{
			SessionID: id,
			Session:   session,
			Hash:      traeRecordHash(string(raw), ""),
		})
	}
	sort.Slice(snapshot.records, func(i, j int) bool { return snapshot.records[i].SessionID < snapshot.records[j].SessionID })
	return snapshot, nil
}

func traeExplicitList(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '['
}

func traeSessionIDHint(raw json.RawMessage) string {
	var hint struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(raw, &hint); err != nil {
		return ""
	}
	return strings.TrimSpace(hint.SessionID)
}

func traeSessionProducesMessages(session traeSession) bool {
	for _, msg := range session.Messages {
		role := RoleType(strings.ToLower(strings.TrimSpace(msg.Role)))
		if role != RoleUser && role != RoleAssistant {
			continue
		}
		content := strings.TrimSpace(msg.Content)
		if content == "" && role == RoleAssistant {
			content = traeAssistantFallback(msg.AgentTaskContent)
		}
		if content != "" {
			return true
		}
	}
	return false
}

func traeSelectRawRecord(path, id string) (json.RawMessage, bool, error) {
	value, err := readTraeValue(path)
	if err != nil {
		return nil, false, err
	}
	var store map[string]json.RawMessage
	if err := json.Unmarshal([]byte(value), &store); err != nil {
		return nil, false, err
	}
	rawList, ok := store["list"]
	if !ok || !traeExplicitList(rawList) {
		return nil, false, nil
	}
	var list []json.RawMessage
	if err := json.Unmarshal(rawList, &list); err != nil {
		return nil, false, err
	}
	for _, raw := range list {
		if traeSessionIDHint(raw) == strings.TrimPrefix(id, "trae:") {
			return raw, true, nil
		}
	}
	return nil, false, nil
}

func readTraeValue(path string) (string, error) {
	db, err := openWindsurfDB(path)
	if err != nil {
		return "", err
	}
	defer db.Close()
	var value string
	err = db.QueryRow(`SELECT value FROM ItemTable WHERE key = ?`, traeStorageKey).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read trae chat data: %w", err)
	}
	return value, nil
}

type traeSession struct {
	SessionID string        `json:"sessionId"`
	CreatedAt traeTime      `json:"createdAt"`
	UpdatedAt traeTime      `json:"updatedAt"`
	Model     string        `json:"model,omitempty"`
	Messages  []traeMessage `json:"messages"`
}
type traeMessage struct {
	Role             string          `json:"role"`
	Content          string          `json:"content"`
	AgentTaskContent json.RawMessage `json:"agentTaskContent,omitempty"`
	Timestamp        traeTime        `json:"timestamp"`
	Model            string          `json:"model,omitempty"`
	TurnIndex        int             `json:"turnIndex"`
}
type traeTime struct{ time.Time }

func (t *traeTime) UnmarshalJSON(data []byte) error {
	var n float64
	if json.Unmarshal(data, &n) == nil && n != 0 {
		if n < 1e11 {
			n *= 1000
		}
		t.Time = time.UnixMilli(int64(n))
		return nil
	}
	var s string
	if json.Unmarshal(data, &s) == nil && s != "" {
		v, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return err
		}
		t.Time = v
	}
	return nil
}

func parseTraeSessionRecord(selected traeSession, project, machine, virtualPath string) (*ParsedSession, []ParsedMessage) {
	var messages []ParsedMessage
	first := ""
	started := selected.CreatedAt.Time
	if started.IsZero() {
		started = selected.UpdatedAt.Time
	}
	for _, msg := range selected.Messages {
		role := RoleType(strings.ToLower(strings.TrimSpace(msg.Role)))
		if role != RoleUser && role != RoleAssistant {
			continue
		}
		content := strings.TrimSpace(msg.Content)
		if content == "" && role == RoleAssistant {
			content = traeAssistantFallback(msg.AgentTaskContent)
		}
		if content == "" {
			continue
		}
		stamp := msg.Timestamp.Time
		if stamp.IsZero() {
			stamp = started
		}
		if first == "" && role == RoleUser {
			first = truncate(strings.ReplaceAll(content, "\n", " "), 300)
		}
		model := msg.Model
		if model == "" {
			model = selected.Model
		}
		messages = append(messages, ParsedMessage{Ordinal: len(messages), Role: role, Content: content, Timestamp: stamp, ContentLength: len(content), Model: model})
	}
	if len(messages) == 0 {
		return nil, nil
	}
	ended := selected.UpdatedAt.Time
	if ended.IsZero() {
		ended = messages[len(messages)-1].Timestamp
	}
	sess := &ParsedSession{ID: "trae:" + strings.TrimPrefix(selected.SessionID, "trae:"), Project: project, Machine: machine, Agent: AgentTrae, SourceSessionID: selected.SessionID, FirstMessage: first, StartedAt: started, EndedAt: ended, MessageCount: len(messages), File: FileInfo{Path: virtualPath}}
	if started.IsZero() {
		sess.StartedAt = messages[0].Timestamp
	}
	return sess, messages
}

func traeAssistantFallback(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		return ""
	}
	for _, key := range []string{"content", "text", "proposal"} {
		if value, ok := obj[key].(string); ok && strings.TrimSpace(value) != "" {
			return value
		}
	}
	if guideline, ok := obj["guideline"].(map[string]any); ok {
		if items, ok := guideline["planItems"].([]any); ok {
			var parts []string
			for _, item := range items {
				if m, ok := item.(map[string]any); ok {
					if v, ok := m["content"].(string); ok {
						parts = append(parts, v)
					}
				}
			}
			return strings.Join(parts, "\n")
		}
	}
	return ""
}

func traeVirtualPath(path, id string) string                  { return path + "#" + id }
func SplitTraeVirtualPath(path string) (string, string, bool) { return splitTraeVirtualPath(path) }
func TraeDBPathForEvent(root, path string) (string, bool)     { return traeDBPathForEvent(root, path) }
func splitTraeVirtualPath(path string) (string, string, bool) {
	return ParseVirtualSourcePathForBase(path, traeStateDBName)
}

func WriteTraeSessionJSON(w io.Writer, path, id string) error {
	record, ok, err := traeSelectRawRecord(path, id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("trae session %s not found in %s: %w", id, path, os.ErrNotExist)
	}
	_, err = w.Write(record)
	return err
}

func traeSourceHash(path, manifest string) (string, error) {
	h := sha256.New()
	value, err := readTraeValue(path)
	if err != nil {
		return "", err
	}
	_, _ = h.Write([]byte(traeStorageKey))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(value))
	_, _ = h.Write([]byte{0})
	if manifest != "" && IsRegularFile(manifest) {
		hash, err := hashJSONLSourceFile(manifest)
		if err != nil {
			return "", err
		}
		_, _ = h.Write([]byte(hash))
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func traeRecordHash(raw, projectHint string) string {
	sum := sha256.New()
	_, _ = sum.Write([]byte(raw))
	if projectHint != "" {
		_, _ = sum.Write([]byte{0})
		_, _ = sum.Write([]byte(projectHint))
	}
	return fmt.Sprintf("%x", sum.Sum(nil))
}

func traeProviderCapabilities() Capabilities {
	caps := windsurfProviderCapabilities()
	caps.Source = multiSessionContainerSourceCapabilities(CapabilitySupported, CapabilitySupported)
	caps.Source.CompositeFingerprint = CapabilitySupported
	caps.Content.AggregateUsageEvents = CapabilityUnsupported
	caps.Content.ToolCalls = CapabilityUnsupported
	caps.Content.ToolResults = CapabilityUnsupported
	caps.Content.Thinking = CapabilityUnsupported
	return caps
}
