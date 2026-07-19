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

	_ "github.com/mattn/go-sqlite3"
)

const (
	windsurfStateDBName = "state.vscdb"

	// WindsurfStateDBName is the shared SQLite store used by Windsurf workspace chat.
	WindsurfStateDBName = windsurfStateDBName
)

var windsurfChatDataKeys = []string{
	"workbench.panel.aichat.view.aichat.chatdata",
	"aiChat.chatdata",
}

var _ Provider = (*windsurfProvider)(nil)

type windsurfProviderFactory struct {
	def AgentDef
}

func newWindsurfProviderFactory(def AgentDef) ProviderFactory {
	return windsurfProviderFactory{def: cloneAgentDef(def)}
}

func (f windsurfProviderFactory) Definition() AgentDef {
	return cloneAgentDef(f.def)
}

func (f windsurfProviderFactory) Capabilities() Capabilities {
	return windsurfProviderCapabilities()
}

func (f windsurfProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	cfg = cfg.Clone()
	return &windsurfProvider{
		ProviderBase: ProviderBase{
			Def:    cloneAgentDef(f.def),
			Caps:   windsurfProviderCapabilities(),
			Config: cfg,
		},
		sources: newWindsurfSourceSet(cfg.Roots),
	}
}

type windsurfProvider struct {
	ProviderBase
	sources windsurfSourceSet
}

func (p *windsurfProvider) Discover(ctx context.Context) ([]SourceRef, error) {
	return p.sources.Discover(ctx)
}

func (p *windsurfProvider) WatchPlan(ctx context.Context) (WatchPlan, error) {
	return p.sources.WatchPlan(ctx)
}

func (p *windsurfProvider) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	return p.sources.SourcesForChangedPath(ctx, req)
}

func (p *windsurfProvider) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	req = ProviderFindRequestWithRawSessionID(p.Def, req)
	return p.sources.FindSource(ctx, req)
}

func (p *windsurfProvider) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	return p.sources.Fingerprint(ctx, source)
}

func (p *windsurfProvider) Parse(
	ctx context.Context,
	req ParseRequest,
) (ParseOutcome, error) {
	if err := ctx.Err(); err != nil {
		return ParseOutcome{}, err
	}
	src, ok := p.sources.sourceFromRef(req.Source)
	if !ok {
		return ParseOutcome{}, fmt.Errorf("windsurf source path unavailable")
	}
	if _, err := os.Stat(src.DBPath); err != nil {
		if os.IsNotExist(err) {
			return ParseOutcome{
				ResultSetComplete: true,
				SkipReason:        SkipNoSession,
			}, nil
		}
		return ParseOutcome{}, fmt.Errorf("stat %s: %w", src.DBPath, err)
	}
	machine := firstNonEmptyJSONLString(req.Machine, p.Config.Machine)
	sess, msgs, err := parseWindsurfSession(
		src.DBPath, src.SessionID, src.Project, machine, src.VirtualPath,
	)
	if err == sql.ErrNoRows {
		return ParseOutcome{
			ResultSetComplete: true,
			ForceReplace:      true,
			SkipReason:        SkipNoSession,
		}, nil
	}
	if err != nil {
		return ParseOutcome{}, err
	}
	if sess == nil {
		return ParseOutcome{
			ResultSetComplete: true,
			ForceReplace:      true,
			SkipReason:        SkipNoSession,
		}, nil
	}
	if req.Fingerprint.Hash != "" {
		sess.File.Hash = req.Fingerprint.Hash
		sess.File.Size = req.Fingerprint.Size
		sess.File.Mtime = req.Fingerprint.MTimeNS
	}
	return ParseOutcome{
		Results: []ParseResultOutcome{{
			Result: ParseResult{
				Session:     *sess,
				Messages:    msgs,
				UsageEvents: sess.UsageEvents,
			},
			DataVersion: DataVersionCurrent,
		}},
		ResultSetComplete: true,
		ForceReplace:      true,
	}, nil
}

type windsurfSource struct {
	Root        string
	DBPath      string
	SessionID   string
	Project     string
	VirtualPath string
}

type windsurfSourceSet struct {
	roots []string
}

func newWindsurfSourceSet(roots []string) windsurfSourceSet {
	return windsurfSourceSet{roots: cleanJSONLRoots(roots)}
}

func (s windsurfSourceSet) Discover(ctx context.Context) ([]SourceRef, error) {
	var sources []SourceRef
	seen := make(map[string]struct{})
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		dbs := s.workspaceDBs(root)
		for _, db := range dbs {
			records, err := listWindsurfSessionRecords(db.DBPath)
			if err != nil {
				return nil, err
			}
			for _, record := range records {
				ref := s.newSourceRef(root, db.DBPath, record.SessionID, db.Project)
				ref.DiscoveryMTimeNS = db.MTimeNS
				addJSONLSource(ref, &sources, seen)
			}
		}
	}
	sortJSONLSources(sources)
	return sources, nil
}

func (s windsurfSourceSet) WatchPlan(context.Context) (WatchPlan, error) {
	roots := make([]WatchRoot, 0, len(s.roots))
	for _, root := range s.roots {
		workspace := windsurfWorkspaceRoot(root)
		roots = append(roots, WatchRoot{
			Path:         workspace,
			Recursive:    true,
			IncludeGlobs: []string{windsurfStateDBName, windsurfStateDBName + "-wal", "workspace.json"},
			DebounceKey:  string(AgentWindsurf) + ":workspace:" + workspace,
		})
	}
	return WatchPlan{Roots: roots}, nil
}

func (s windsurfSourceSet) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, root := range s.roots {
		dbPath, ok := s.dbPathForEvent(root, req)
		if !ok {
			continue
		}
		if _, err := os.Stat(dbPath); err != nil {
			if os.IsNotExist(err) {
				return nil, nil
			}
			return nil, fmt.Errorf("stat %s: %w", dbPath, err)
		}
		sources, err := s.sourcesForDB(root, dbPath)
		if err != nil {
			return nil, err
		}
		for _, path := range req.StoredSourcePaths {
			ref, ok := s.sourceRef(root, path)
			if !ok {
				continue
			}
			src := ref.Opaque.(windsurfSource)
			if samePath(src.DBPath, dbPath) {
				sources = append(sources, ref)
			}
		}
		sortJSONLSources(sources)
		return sources, nil
	}
	return nil, nil
}

func (s windsurfSourceSet) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	if err := ctx.Err(); err != nil {
		return SourceRef{}, false, err
	}
	for _, path := range []string{req.StoredFilePath, req.FingerprintKey} {
		if path == "" {
			continue
		}
		for _, root := range s.roots {
			ref, ok := s.sourceRef(root, path)
			if !ok {
				continue
			}
			if !req.RequireFreshSource {
				return ref, true, nil
			}
			src := ref.Opaque.(windsurfSource)
			exists, err := windsurfDBHasSession(src.DBPath, src.SessionID)
			if err != nil {
				return SourceRef{}, false, err
			}
			if exists {
				return ref, true, nil
			}
		}
	}
	if req.RawSessionID == "" {
		return SourceRef{}, false, nil
	}
	for _, root := range s.roots {
		for _, db := range s.workspaceDBs(root) {
			records, err := listWindsurfSessionRecords(db.DBPath)
			if err != nil {
				return SourceRef{}, false, err
			}
			for _, record := range records {
				if record.SessionID == req.RawSessionID {
					return s.newSourceRef(root, db.DBPath, record.SessionID, db.Project), true, nil
				}
			}
		}
	}
	return SourceRef{}, false, nil
}

func (s windsurfSourceSet) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	if err := ctx.Err(); err != nil {
		return SourceFingerprint{}, err
	}
	src, ok := s.sourceFromRef(source)
	if !ok {
		return SourceFingerprint{}, fmt.Errorf("windsurf source path unavailable")
	}
	info, err := os.Stat(src.DBPath)
	if err != nil {
		if os.IsNotExist(err) {
			return SourceFingerprint{
				Key: firstNonEmptyJSONLString(
					source.FingerprintKey,
					source.Key,
					src.VirtualPath,
				),
			}, nil
		}
		return SourceFingerprint{}, fmt.Errorf("stat %s: %w", src.DBPath, err)
	}
	workspacePath := windsurfWorkspaceManifestPath(src.DBPath)
	combined := antigravityCLICombinedFileInfo(
		info,
		src.DBPath+"-wal",
		workspacePath,
	)
	hash, err := windsurfSourceHash(src.DBPath, workspacePath)
	if err != nil {
		return SourceFingerprint{}, err
	}
	return SourceFingerprint{
		Key:     firstNonEmptyJSONLString(source.FingerprintKey, source.Key, src.VirtualPath),
		Size:    combined.Size(),
		MTimeNS: combined.ModTime().UnixNano(),
		Hash:    hash,
	}, nil
}

func (s windsurfSourceSet) sourceFromRef(source SourceRef) (windsurfSource, bool) {
	switch src := source.Opaque.(type) {
	case windsurfSource:
		return src, src.DBPath != "" && src.SessionID != ""
	case *windsurfSource:
		if src != nil && src.DBPath != "" && src.SessionID != "" {
			return *src, true
		}
	}
	for _, candidate := range []string{source.DisplayPath, source.FingerprintKey, source.Key} {
		for _, root := range s.roots {
			if ref, ok := s.sourceRef(root, candidate); ok {
				return ref.Opaque.(windsurfSource), true
			}
		}
	}
	return windsurfSource{}, false
}

func (s windsurfSourceSet) sourceRef(root, virtualPath string) (SourceRef, bool) {
	dbPath, sessionID, ok := splitWindsurfVirtualPath(virtualPath)
	if !ok || sessionID == "" {
		return SourceRef{}, false
	}
	if !s.dbBelongsToRoot(root, dbPath) {
		return SourceRef{}, false
	}
	return s.newSourceRef(
		root,
		dbPath,
		sessionID,
		windsurfWorkspaceProject(dbPath),
	), true
}

func (s windsurfSourceSet) newSourceRef(
	root, dbPath, sessionID, project string,
) SourceRef {
	virtualPath := windsurfVirtualPath(dbPath, sessionID)
	return SourceRef{
		Provider:       AgentWindsurf,
		Key:            virtualPath,
		DisplayPath:    virtualPath,
		FingerprintKey: virtualPath,
		ProjectHint:    project,
		Opaque: windsurfSource{
			Root:        root,
			DBPath:      dbPath,
			SessionID:   sessionID,
			Project:     project,
			VirtualPath: virtualPath,
		},
	}
}

func (s windsurfSourceSet) sourcesForDB(root, dbPath string) ([]SourceRef, error) {
	records, err := listWindsurfSessionRecords(dbPath)
	if err != nil {
		return nil, err
	}
	project := windsurfWorkspaceProject(dbPath)
	sources := make([]SourceRef, 0, len(records))
	seen := make(map[string]struct{}, len(records))
	for _, record := range records {
		addJSONLSource(
			s.newSourceRef(root, dbPath, record.SessionID, project),
			&sources,
			seen,
		)
	}
	return sources, nil
}

func (s windsurfSourceSet) dbPathForEvent(
	root string,
	req ChangedPathRequest,
) (string, bool) {
	if req.WatchRoot != "" {
		want := windsurfWorkspaceRoot(root)
		if !samePath(req.WatchRoot, want) {
			return "", false
		}
	}
	workspaceRoot := windsurfWorkspaceRoot(root)
	path := filepath.Clean(req.Path)
	rel, ok := relUnder(workspaceRoot, path)
	if !ok {
		return "", false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 2 {
		return "", false
	}
	switch parts[1] {
	case windsurfStateDBName, windsurfStateDBName + "-wal", "workspace.json":
		return filepath.Join(workspaceRoot, parts[0], windsurfStateDBName), true
	default:
		return "", false
	}
}

func (s windsurfSourceSet) dbBelongsToRoot(root, dbPath string) bool {
	workspaceRoot := windsurfWorkspaceRoot(root)
	rel, ok := relUnder(workspaceRoot, filepath.Clean(dbPath))
	if !ok {
		return false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	return len(parts) == 2 && parts[1] == windsurfStateDBName
}

type windsurfWorkspaceDB struct {
	DBPath  string
	Project string
	MTimeNS int64
}

func (s windsurfSourceSet) workspaceDBs(root string) []windsurfWorkspaceDB {
	workspaceRoot := windsurfWorkspaceRoot(root)
	entries, err := os.ReadDir(workspaceRoot)
	if err != nil {
		return nil
	}
	dbs := make([]windsurfWorkspaceDB, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dbPath := filepath.Join(workspaceRoot, entry.Name(), windsurfStateDBName)
		info, err := os.Stat(dbPath)
		if err != nil || info.IsDir() {
			continue
		}
		dbs = append(dbs, windsurfWorkspaceDB{
			DBPath:  dbPath,
			Project: windsurfWorkspaceProject(dbPath),
			MTimeNS: info.ModTime().UnixNano(),
		})
	}
	sort.Slice(dbs, func(i, j int) bool {
		return dbs[i].DBPath < dbs[j].DBPath
	})
	return dbs
}

func windsurfWorkspaceRoot(root string) string {
	clean := filepath.Clean(root)
	if filepath.Base(clean) == "workspaceStorage" {
		return clean
	}
	return filepath.Join(clean, "workspaceStorage")
}

type windsurfSessionRecord struct {
	SessionID string
	Data      []byte
}

type windsurfChatValue struct {
	Key   string
	Value string
}

func listWindsurfSessionRecords(dbPath string) ([]windsurfSessionRecord, error) {
	values, err := readWindsurfChatValues(dbPath)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{})
	var records []windsurfSessionRecord
	for _, value := range values {
		next, err := windsurfRecordsFromValue(
			[]byte(value.Value),
			windsurfFallbackSessionID(dbPath),
		)
		if err != nil {
			return nil, err
		}
		for _, record := range next {
			if _, ok := seen[record.SessionID]; ok {
				continue
			}
			seen[record.SessionID] = struct{}{}
			records = append(records, record)
		}
	}
	sort.Slice(records, func(i, j int) bool {
		return records[i].SessionID < records[j].SessionID
	})
	return records, nil
}

func readWindsurfChatValues(dbPath string) ([]windsurfChatValue, error) {
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}
	db, err := openWindsurfDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	values := make([]windsurfChatValue, 0, len(windsurfChatDataKeys))
	for _, key := range windsurfChatDataKeys {
		var value string
		err := db.QueryRow(
			`SELECT value FROM ItemTable WHERE key = ?`,
			key,
		).Scan(&value)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read windsurf chat data: %w", err)
		}
		values = append(values, windsurfChatValue{
			Key:   key,
			Value: value,
		})
	}
	return values, nil
}

func windsurfDBHasSession(dbPath, sessionID string) (bool, error) {
	records, err := listWindsurfSessionRecords(dbPath)
	if err != nil {
		return false, err
	}
	for _, record := range records {
		if record.SessionID == sessionID {
			return true, nil
		}
	}
	return false, nil
}

func parseWindsurfSession(
	dbPath, sessionID, project, machine, virtualPath string,
) (*ParsedSession, []ParsedMessage, error) {
	record, err := loadWindsurfSessionRecord(dbPath, sessionID)
	if err != nil {
		return nil, nil, err
	}
	sess, msgs, err := parseVSCodeCopilotData(
		record.Data,
		virtualPath,
		project,
		machine,
	)
	if err != nil {
		return nil, nil, err
	}
	if sess == nil {
		return nil, nil, nil
	}
	info, err := os.Stat(dbPath)
	if err != nil {
		return nil, nil, fmt.Errorf("stat %s: %w", dbPath, err)
	}
	combined := antigravityCLICombinedFileInfo(
		info,
		dbPath+"-wal",
		windsurfWorkspaceManifestPath(dbPath),
	)
	sess.Agent = AgentWindsurf
	sess.ID = "windsurf:" + strings.TrimPrefix(sess.ID, "windsurf:")
	sess.File = FileInfo{
		Path:  virtualPath,
		Size:  combined.Size(),
		Mtime: combined.ModTime().UnixNano(),
	}
	for i := range sess.UsageEvents {
		sess.UsageEvents[i].SessionID = sess.ID
		sess.UsageEvents[i].Source = string(AgentWindsurf)
	}
	return sess, msgs, nil
}

func loadWindsurfSessionRecord(
	dbPath, sessionID string,
) (windsurfSessionRecord, error) {
	records, err := listWindsurfSessionRecords(dbPath)
	if err != nil {
		return windsurfSessionRecord{}, err
	}
	for _, record := range records {
		if record.SessionID == sessionID {
			return record, nil
		}
	}
	return windsurfSessionRecord{}, sql.ErrNoRows
}

func openWindsurfDB(dbPath string) (*sql.DB, error) {
	if windsurfDBOpenHook != nil {
		windsurfDBOpenHook(dbPath)
	}
	dsn := "file:" + sqliteURIPath(dbPath) + "?mode=ro&immutable=0&_busy_timeout=3000"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open windsurf db %s: %w", dbPath, err)
	}
	return db, nil
}

var windsurfDBOpenHook func(string)

func SetWindsurfDBOpenHookForTest(fn func(string)) func() {
	prev := windsurfDBOpenHook
	windsurfDBOpenHook = fn
	return func() { windsurfDBOpenHook = prev }
}

func windsurfRecordsFromValue(
	data []byte,
	fallbackSessionID string,
) ([]windsurfSessionRecord, error) {
	var session vscodeCopilotSession
	if err := json.Unmarshal(data, &session); err == nil &&
		len(session.Requests) > 0 {
		id := session.SessionID
		if id == "" {
			id = fallbackSessionID
			session.SessionID = id
			payload, err := json.Marshal(session)
			if err != nil {
				return nil, err
			}
			data = payload
		}
		return []windsurfSessionRecord{{
			SessionID: id,
			Data:      append([]byte(nil), data...),
		}}, nil
	}

	var chatData windsurfChatData
	if err := json.Unmarshal(data, &chatData); err != nil {
		return nil, fmt.Errorf("parse windsurf chatdata: %w", err)
	}
	records := make([]windsurfSessionRecord, 0, len(chatData.Tabs))
	for _, tab := range chatData.Tabs {
		session, ok := tab.toVSCodeSession()
		if !ok {
			continue
		}
		payload, err := json.Marshal(session)
		if err != nil {
			return nil, err
		}
		records = append(records, windsurfSessionRecord{
			SessionID: session.SessionID,
			Data:      payload,
		})
	}
	return records, nil
}

func windsurfFallbackSessionID(dbPath string) string {
	workspaceID := strings.TrimSpace(filepath.Base(filepath.Dir(dbPath)))
	if workspaceID == "" || workspaceID == "." || workspaceID == string(filepath.Separator) {
		hash := sha256.Sum256([]byte(filepath.ToSlash(dbPath)))
		return fmt.Sprintf("workspace-%x", hash[:8])
	}
	return "workspace-" + workspaceID
}

type windsurfChatData struct {
	Tabs []windsurfChatTab `json:"tabs"`
}

type windsurfChatTab struct {
	TabID     string               `json:"tabId"`
	ChatTitle string               `json:"chatTitle"`
	Bubbles   []windsurfChatBubble `json:"bubbles"`
}

type windsurfChatBubble struct {
	Type     windsurfBubbleType `json:"type"`
	Text     string             `json:"text"`
	RawText  string             `json:"rawText"`
	InitText string             `json:"initText"`
}

type windsurfBubbleType string

func (t *windsurfBubbleType) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err == nil {
		*t = windsurfBubbleType(value)
		return nil
	}
	var number json.Number
	if err := json.Unmarshal(data, &number); err == nil {
		*t = windsurfBubbleType(number.String())
		return nil
	}
	*t = ""
	return nil
}

func (t windsurfChatTab) toVSCodeSession() (vscodeCopilotSession, bool) {
	id := strings.TrimSpace(t.TabID)
	if id == "" || len(t.Bubbles) == 0 {
		return vscodeCopilotSession{}, false
	}
	session := vscodeCopilotSession{
		Version:     1,
		SessionID:   id,
		CustomTitle: t.ChatTitle,
	}
	var current *vscodeCopilotRequest
	flush := func() {
		if current == nil {
			return
		}
		session.Requests = append(session.Requests, *current)
		current = nil
	}
	for i, bubble := range t.Bubbles {
		content := strings.TrimSpace(bubble.content())
		if content == "" {
			continue
		}
		if bubble.isAssistant() {
			if current == nil {
				current = &vscodeCopilotRequest{
					RequestID: fmt.Sprintf("%s-%d", id, i),
				}
			}
			current.Response = append(current.Response, windsurfResponseItem(content))
			continue
		}
		flush()
		current = &vscodeCopilotRequest{
			RequestID: fmt.Sprintf("%s-%d", id, i),
			Message: vscodeCopilotMessage{
				Text: content,
			},
		}
	}
	flush()
	return session, len(session.Requests) > 0
}

func (b windsurfChatBubble) content() string {
	for _, value := range []string{b.Text, b.RawText, b.InitText} {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func (b windsurfChatBubble) isAssistant() bool {
	switch strings.ToLower(strings.TrimSpace(string(b.Type))) {
	case "2", "ai", "assistant":
		return true
	default:
		return false
	}
}

func windsurfResponseItem(content string) json.RawMessage {
	data, _ := json.Marshal(map[string]string{"value": content})
	return data
}

func windsurfVirtualPath(dbPath, sessionID string) string {
	return dbPath + "#" + sessionID
}

func SplitWindsurfVirtualPath(path string) (string, string, bool) {
	return splitWindsurfVirtualPath(path)
}

func splitWindsurfVirtualPath(path string) (string, string, bool) {
	return ParseVirtualSourcePathForBase(path, windsurfStateDBName)
}

func WriteWindsurfSessionJSON(w io.Writer, dbPath, sessionID string) error {
	record, err := loadWindsurfSessionRecord(dbPath, sessionID)
	if err == sql.ErrNoRows {
		return fmt.Errorf(
			"windsurf session %s not found in %s: %w",
			sessionID, dbPath, os.ErrNotExist,
		)
	}
	if err != nil {
		return err
	}
	_, err = w.Write(record.Data)
	return err
}

func WriteSanitizedWindsurfStateDB(dstPath, dbPath string) error {
	values, err := readWindsurfChatValues(dbPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return fmt.Errorf("create windsurf export dir: %w", err)
	}
	if err := os.Remove(dstPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("replace windsurf export db %s: %w", dstPath, err)
	}

	dst, err := sql.Open(
		"sqlite3",
		"file:"+sqliteURIPath(dstPath)+"?mode=rwc&_busy_timeout=3000",
	)
	if err != nil {
		return fmt.Errorf("open sanitized windsurf db %s: %w", dstPath, err)
	}
	complete := false
	defer func() {
		_ = dst.Close()
		if !complete {
			_ = os.Remove(dstPath)
		}
	}()

	if _, err := dst.Exec(`PRAGMA journal_mode=DELETE`); err != nil {
		return fmt.Errorf("configure sanitized windsurf db: %w", err)
	}
	if _, err := dst.Exec(`CREATE TABLE ItemTable (key TEXT PRIMARY KEY, value TEXT)`); err != nil {
		return fmt.Errorf("create sanitized windsurf ItemTable: %w", err)
	}
	tx, err := dst.Begin()
	if err != nil {
		return fmt.Errorf("begin sanitized windsurf export: %w", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO ItemTable (key, value) VALUES (?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("prepare sanitized windsurf export: %w", err)
	}
	for _, value := range values {
		if _, err := stmt.Exec(value.Key, value.Value); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			return fmt.Errorf("write sanitized windsurf chat key: %w", err)
		}
	}
	if err := stmt.Close(); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("close sanitized windsurf export: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit sanitized windsurf export: %w", err)
	}
	if err := dst.Close(); err != nil {
		return fmt.Errorf("close sanitized windsurf db: %w", err)
	}
	complete = true
	return nil
}

func windsurfWorkspaceManifestPath(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), "workspace.json")
}

func windsurfWorkspaceProject(dbPath string) string {
	project := readVSCodeWorkspaceManifest(filepath.Dir(dbPath))
	if project == "" {
		return "unknown"
	}
	return project
}

func windsurfSourceHash(dbPath, workspacePath string) (string, error) {
	h := sha256.New()
	if IsRegularFile(dbPath) {
		values, err := readWindsurfChatValues(dbPath)
		if err != nil {
			return "", err
		}
		for _, value := range values {
			_, _ = h.Write([]byte("chat"))
			_, _ = h.Write([]byte{0})
			_, _ = h.Write([]byte(value.Key))
			_, _ = h.Write([]byte{0})
			_, _ = h.Write([]byte(value.Value))
			_, _ = h.Write([]byte{0})
		}
	}
	if workspacePath != "" && IsRegularFile(workspacePath) {
		hash, err := hashJSONLSourceFile(workspacePath)
		if err != nil {
			return "", err
		}
		_, _ = h.Write([]byte("workspace"))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(hash))
		_, _ = h.Write([]byte{0})
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func windsurfProviderCapabilities() Capabilities {
	return Capabilities{
		Source: SourceCapabilities{
			DiscoverSources:      CapabilitySupported,
			WatchSources:         CapabilitySupported,
			ClassifyChangedPath:  CapabilitySupported,
			StoredSourceHints:    CapabilitySupported,
			FindSource:           CapabilitySupported,
			CompositeFingerprint: CapabilitySupported,
			IncrementalAppend:    CapabilityNotApplicable,
			MultiSessionSource:   CapabilityNotApplicable,
			PerSessionErrors:     CapabilityNotApplicable,
			ExcludedSessions:     CapabilityNotApplicable,
			ForceReplaceOnParse:  CapabilitySupported,
		},
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			Thinking:             CapabilitySupported,
			AggregateUsageEvents: CapabilitySupported,
			Model:                CapabilitySupported,
		},
	}
}
