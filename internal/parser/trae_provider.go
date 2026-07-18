package parser

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const traeStateDBName = "state.vscdb"

const traeStorageKey = "memento/icube-ai-agent-storage"

var _ Provider = (*traeProvider)(nil)

type traeProviderFactory struct{ def AgentDef }

func newTraeProviderFactory(def AgentDef) ProviderFactory {
	return traeProviderFactory{def: cloneAgentDef(def)}
}

func (f traeProviderFactory) Definition() AgentDef { return cloneAgentDef(f.def) }

func (f traeProviderFactory) Capabilities() Capabilities { return traeProviderCapabilities() }

func (f traeProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	cfg = cfg.Clone()
	return &traeProvider{
		ProviderBase: ProviderBase{Def: cloneAgentDef(f.def), Caps: traeProviderCapabilities(), Config: cfg},
		sources:      newTraeSourceSet(cfg.Roots),
	}
}

type traeProvider struct {
	ProviderBase
	sources traeSourceSet
}

func (p *traeProvider) Discover(ctx context.Context) ([]SourceRef, error) {
	return p.sources.Discover(ctx)
}

func (p *traeProvider) WatchPlan(ctx context.Context) (WatchPlan, error) {
	return p.sources.WatchPlan(ctx)
}

func (p *traeProvider) SourcesForChangedPath(ctx context.Context, req ChangedPathRequest) ([]SourceRef, error) {
	return p.sources.SourcesForChangedPath(ctx, req)
}

func (p *traeProvider) FindSource(ctx context.Context, req FindSourceRequest) (SourceRef, bool, error) {
	return p.sources.FindSource(ctx, ProviderFindRequestWithRawSessionID(p.Def, req))
}

func (p *traeProvider) Fingerprint(ctx context.Context, source SourceRef) (SourceFingerprint, error) {
	return p.sources.Fingerprint(ctx, source)
}

func (p *traeProvider) Parse(ctx context.Context, req ParseRequest) (ParseOutcome, error) {
	if err := ctx.Err(); err != nil {
		return ParseOutcome{}, err
	}
	src, ok := p.sources.sourceFromRef(req.Source)
	if !ok {
		return ParseOutcome{}, fmt.Errorf("trae source path unavailable")
	}
	if _, err := os.Stat(src.DBPath); err != nil {
		if os.IsNotExist(err) {
			return ParseOutcome{ResultSetComplete: true, SkipReason: SkipNoSession}, nil
		}
		return ParseOutcome{}, fmt.Errorf("stat %s: %w", src.DBPath, err)
	}
	sess, msgs, err := parseTraeSession(src.DBPath, src.SessionID, src.Project, firstNonEmptyJSONLString(req.Machine, p.Config.Machine), src.VirtualPath)
	if err == sql.ErrNoRows || sess == nil {
		return ParseOutcome{ResultSetComplete: true, ForceReplace: true, SkipReason: SkipNoSession}, nil
	}
	if err != nil {
		return ParseOutcome{}, err
	}
	if req.Fingerprint.Hash != "" {
		sess.File.Hash, sess.File.Size, sess.File.Mtime = req.Fingerprint.Hash, req.Fingerprint.Size, req.Fingerprint.MTimeNS
	}
	return ParseOutcome{Results: []ParseResultOutcome{{Result: ParseResult{Session: *sess, Messages: msgs}, DataVersion: DataVersionCurrent}}, ResultSetComplete: true, ForceReplace: true}, nil
}

type traeSource struct{ Root, DBPath, SessionID, Project, VirtualPath string }

type traeSourceSet struct{ roots []string }

func newTraeSourceSet(roots []string) traeSourceSet {
	return traeSourceSet{roots: cleanJSONLRoots(roots)}
}

type traeDB struct {
	path, project string
	mtime         int64
}

func (s traeSourceSet) dbs(root string) []traeDB {
	var dbs []traeDB
	workspace := filepath.Join(filepath.Clean(root), "workspaceStorage")
	if entries, err := os.ReadDir(workspace); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			path := filepath.Join(workspace, entry.Name(), traeStateDBName)
			if info, err := os.Stat(path); err == nil && !info.IsDir() {
				dbs = append(dbs, traeDB{path, traeWorkspaceProject(path), info.ModTime().UnixNano()})
			}
		}
	}
	global := filepath.Join(filepath.Clean(root), "globalStorage", traeStateDBName)
	if info, err := os.Stat(global); err == nil && !info.IsDir() {
		dbs = append(dbs, traeDB{global, "unknown", info.ModTime().UnixNano()})
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

func (s traeSourceSet) Discover(ctx context.Context) ([]SourceRef, error) {
	var sources []SourceRef
	seen := map[string]struct{}{}
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, db := range s.dbs(root) {
			records, err := listTraeSessionRecords(db.path)
			if err != nil {
				return nil, err
			}
			for _, record := range records {
				ref := s.newSourceRef(root, db.path, record.SessionID, db.project)
				ref.DiscoveryMTimeNS = db.mtime
				addJSONLSource(ref, &sources, seen)
			}
		}
	}
	sortJSONLSources(sources)
	return sources, nil
}

func (s traeSourceSet) WatchPlan(context.Context) (WatchPlan, error) {
	var roots []WatchRoot
	for _, root := range s.roots {
		for _, subdir := range []string{"workspaceStorage", "globalStorage"} {
			path := filepath.Join(filepath.Clean(root), subdir)
			roots = append(roots, WatchRoot{Path: path, Recursive: subdir == "workspaceStorage", IncludeGlobs: []string{traeStateDBName, traeStateDBName + "-wal", "workspace.json"}, DebounceKey: string(AgentTrae) + ":" + subdir + ":" + path})
		}
	}
	return WatchPlan{Roots: roots}, nil
}

func (s traeSourceSet) SourcesForChangedPath(ctx context.Context, req ChangedPathRequest) ([]SourceRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, root := range s.roots {
		path, ok := s.dbPathForEvent(root, req)
		if !ok {
			continue
		}
		if _, err := os.Stat(path); err != nil {
			if os.IsNotExist(err) {
				return nil, nil
			}
			return nil, err
		}
		return s.sourcesForDB(root, path)
	}
	return nil, nil
}

func (s traeSourceSet) FindSource(ctx context.Context, req FindSourceRequest) (SourceRef, bool, error) {
	if err := ctx.Err(); err != nil {
		return SourceRef{}, false, err
	}
	for _, candidate := range []string{req.StoredFilePath, req.FingerprintKey} {
		for _, root := range s.roots {
			if ref, ok := s.sourceRef(root, candidate); ok {
				return ref, true, nil
			}
		}
	}
	if req.RawSessionID == "" {
		return SourceRef{}, false, nil
	}
	for _, root := range s.roots {
		for _, db := range s.dbs(root) {
			records, err := listTraeSessionRecords(db.path)
			if err != nil {
				return SourceRef{}, false, err
			}
			for _, record := range records {
				if record.SessionID == req.RawSessionID {
					return s.newSourceRef(root, db.path, record.SessionID, db.project), true, nil
				}
			}
		}
	}
	return SourceRef{}, false, nil
}

func (s traeSourceSet) Fingerprint(ctx context.Context, source SourceRef) (SourceFingerprint, error) {
	if err := ctx.Err(); err != nil {
		return SourceFingerprint{}, err
	}
	src, ok := s.sourceFromRef(source)
	if !ok {
		return SourceFingerprint{}, fmt.Errorf("trae source path unavailable")
	}
	info, err := os.Stat(src.DBPath)
	if err != nil {
		if os.IsNotExist(err) {
			return SourceFingerprint{Key: src.VirtualPath}, nil
		}
		return SourceFingerprint{}, err
	}
	manifest := ""
	if filepath.Base(filepath.Dir(src.DBPath)) != "globalStorage" {
		manifest = windsurfWorkspaceManifestPath(src.DBPath)
	}
	combined := antigravityCLICombinedFileInfo(info, src.DBPath+"-wal", manifest)
	hash, err := traeSourceHash(src.DBPath, manifest)
	if err != nil {
		return SourceFingerprint{}, err
	}
	return SourceFingerprint{Key: src.VirtualPath, Size: combined.Size(), MTimeNS: combined.ModTime().UnixNano(), Hash: hash}, nil
}

func (s traeSourceSet) sourceFromRef(source SourceRef) (traeSource, bool) {
	if src, ok := source.Opaque.(traeSource); ok && src.DBPath != "" && src.SessionID != "" {
		return src, true
	}
	for _, candidate := range []string{source.DisplayPath, source.FingerprintKey, source.Key} {
		for _, root := range s.roots {
			if ref, ok := s.sourceRef(root, candidate); ok {
				return ref.Opaque.(traeSource), true
			}
		}
	}
	return traeSource{}, false
}

func (s traeSourceSet) sourceRef(root, virtualPath string) (SourceRef, bool) {
	dbPath, sessionID, ok := splitTraeVirtualPath(virtualPath)
	if !ok || sessionID == "" || !s.dbBelongsToRoot(root, dbPath) {
		return SourceRef{}, false
	}
	project := "unknown"
	if filepath.Base(filepath.Dir(dbPath)) != "globalStorage" {
		project = traeWorkspaceProject(dbPath)
	}
	return s.newSourceRef(root, dbPath, sessionID, project), true
}

func (s traeSourceSet) newSourceRef(root, dbPath, sessionID, project string) SourceRef {
	path := traeVirtualPath(dbPath, sessionID)
	return SourceRef{Provider: AgentTrae, Key: path, DisplayPath: path, FingerprintKey: path, ProjectHint: project, Opaque: traeSource{root, dbPath, sessionID, project, path}}
}

func (s traeSourceSet) sourcesForDB(root, path string) ([]SourceRef, error) {
	records, err := listTraeSessionRecords(path)
	if err != nil {
		return nil, err
	}
	project := "unknown"
	if filepath.Base(filepath.Dir(path)) != "globalStorage" {
		project = traeWorkspaceProject(path)
	}
	sources := make([]SourceRef, 0, len(records))
	seen := map[string]struct{}{}
	for _, record := range records {
		addJSONLSource(s.newSourceRef(root, path, record.SessionID, project), &sources, seen)
	}
	sortJSONLSources(sources)
	return sources, nil
}

func (s traeSourceSet) dbPathForEvent(root string, req ChangedPathRequest) (string, bool) {
	for _, subdir := range []string{"workspaceStorage", "globalStorage"} {
		watch := filepath.Join(filepath.Clean(root), subdir)
		if req.WatchRoot != "" && !samePath(req.WatchRoot, watch) {
			continue
		}
		rel, ok := relUnder(watch, filepath.Clean(req.Path))
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

func (s traeSourceSet) dbBelongsToRoot(root, path string) bool {
	for _, subdir := range []string{"workspaceStorage", "globalStorage"} {
		rel, ok := relUnder(filepath.Join(filepath.Clean(root), subdir), filepath.Clean(path))
		if !ok {
			continue
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if (subdir == "globalStorage" && len(parts) == 1 && parts[0] == traeStateDBName) || (subdir == "workspaceStorage" && len(parts) == 2 && parts[1] == traeStateDBName) {
			return true
		}
	}
	return false
}

type traeSessionRecord struct {
	SessionID string
	Data      []byte
}

func listTraeSessionRecords(path string) ([]traeSessionRecord, error) {
	value, err := readTraeValue(path)
	if err != nil {
		return nil, err
	}
	if value == "" {
		return nil, nil
	}
	var store traeStore
	if err := json.Unmarshal([]byte(value), &store); err != nil {
		return nil, nil
	}
	records := make([]traeSessionRecord, 0, len(store.List))
	seen := map[string]struct{}{}
	for _, rawSession := range store.List {
		var session traeSession
		if err := json.Unmarshal(rawSession, &session); err != nil {
			continue
		}
		id := strings.TrimSpace(session.SessionID)
		if id == "" || len(session.Messages) == 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		records = append(records, traeSessionRecord{id, append([]byte(nil), rawSession...)})
	}
	sort.Slice(records, func(i, j int) bool { return records[i].SessionID < records[j].SessionID })
	return records, nil
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

type traeStore struct {
	List []json.RawMessage `json:"list"`
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

func parseTraeSession(path, id, project, machine, virtualPath string) (*ParsedSession, []ParsedMessage, error) {
	records, err := listTraeSessionRecords(path)
	if err != nil {
		return nil, nil, err
	}
	var selected traeSession
	found := false
	for _, record := range records {
		if record.SessionID == id {
			if err := json.Unmarshal(record.Data, &selected); err != nil {
				return nil, nil, err
			}
			found = true
			break
		}
	}
	if !found {
		return nil, nil, sql.ErrNoRows
	}
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
		return nil, nil, nil
	}
	ended := selected.UpdatedAt.Time
	if ended.IsZero() {
		ended = messages[len(messages)-1].Timestamp
	}
	sess := &ParsedSession{ID: "trae:" + strings.TrimPrefix(id, "trae:"), Project: project, Machine: machine, Agent: AgentTrae, SourceSessionID: id, FirstMessage: first, StartedAt: started, EndedAt: ended, MessageCount: len(messages), File: FileInfo{Path: virtualPath}}
	if started.IsZero() {
		sess.StartedAt = messages[0].Timestamp
	}
	return sess, messages, nil
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
func splitTraeVirtualPath(path string) (string, string, bool) {
	return ParseVirtualSourcePathForBase(path, traeStateDBName)
}

func WriteTraeSessionJSON(w io.Writer, path, id string) error {
	records, err := listTraeSessionRecords(path)
	if err != nil {
		return err
	}
	for _, record := range records {
		if record.SessionID == id {
			_, err = w.Write(record.Data)
			return err
		}
	}
	return fmt.Errorf("trae session %s not found in %s: %w", id, path, os.ErrNotExist)
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

func traeProviderCapabilities() Capabilities {
	caps := windsurfProviderCapabilities()
	caps.Content.AggregateUsageEvents = CapabilityUnsupported
	caps.Content.ToolCalls = CapabilityUnsupported
	caps.Content.ToolResults = CapabilityUnsupported
	caps.Content.Thinking = CapabilityUnsupported
	return caps
}
