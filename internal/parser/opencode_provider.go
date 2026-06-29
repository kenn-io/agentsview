package parser

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

var _ Provider = (*openCodeFormatProvider)(nil)

type openCodeFormatProviderFactory struct {
	def  AgentDef
	spec openCodeProviderSpec
}

func newOpenCodeProviderFactory(def AgentDef) ProviderFactory {
	return openCodeFormatProviderFactory{
		def:  cloneAgentDef(def),
		spec: openCodeProviderSpecForAgent(AgentOpenCode),
	}
}

func newKiloProviderFactory(def AgentDef) ProviderFactory {
	return openCodeFormatProviderFactory{
		def:  cloneAgentDef(def),
		spec: openCodeProviderSpecForAgent(AgentKilo),
	}
}

func newMiMoCodeProviderFactory(def AgentDef) ProviderFactory {
	return openCodeFormatProviderFactory{
		def:  cloneAgentDef(def),
		spec: openCodeProviderSpecForAgent(AgentMiMoCode),
	}
}

func newIcodemateProviderFactory(def AgentDef) ProviderFactory {
	return openCodeFormatProviderFactory{
		def:  cloneAgentDef(def),
		spec: openCodeProviderSpecForAgent(AgentIcodemate),
	}
}

func (f openCodeFormatProviderFactory) Definition() AgentDef {
	return cloneAgentDef(f.def)
}

func (f openCodeFormatProviderFactory) Capabilities() Capabilities {
	return openCodeFormatProviderCapabilities()
}

func (f openCodeFormatProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	cfg = cfg.Clone()
	return &openCodeFormatProvider{
		ProviderBase: ProviderBase{
			Def:    cloneAgentDef(f.def),
			Caps:   openCodeFormatProviderCapabilities(),
			Config: cfg,
		},
		sources: newOpenCodeFormatSourceSet(cfg.Roots, f.spec),
	}
}

type openCodeFormatProvider struct {
	ProviderBase
	sources openCodeFormatSourceSet
}

func (p *openCodeFormatProvider) Discover(ctx context.Context) ([]SourceRef, error) {
	return p.sources.Discover(ctx)
}

func (p *openCodeFormatProvider) WatchPlan(ctx context.Context) (WatchPlan, error) {
	return p.sources.WatchPlan(ctx)
}

func (p *openCodeFormatProvider) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	return p.sources.SourcesForChangedPath(ctx, req)
}

func (p *openCodeFormatProvider) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	req = ProviderFindRequestWithRawSessionID(p.Def, req)
	return p.sources.FindSource(ctx, req)
}

func (p *openCodeFormatProvider) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	return p.sources.Fingerprint(ctx, source)
}

func (p *openCodeFormatProvider) Parse(
	ctx context.Context,
	req ParseRequest,
) (ParseOutcome, error) {
	if err := ctx.Err(); err != nil {
		return ParseOutcome{}, err
	}
	path, ok := p.sources.pathFromSource(req.Source)
	if !ok {
		return ParseOutcome{}, fmt.Errorf("%s source path unavailable", p.Def.Type)
	}

	machine := firstNonEmptyJSONLString(req.Machine, p.Config.Machine)
	var (
		sess *ParsedSession
		msgs []ParsedMessage
		err  error
	)
	if dbPath, sessionID, ok := p.sources.spec.parseVirtual(path); ok {
		sess, msgs, err = p.sources.spec.parseSQLite(dbPath, sessionID, machine)
	} else {
		sess, msgs, err = p.sources.spec.parseFile(path, machine)
	}
	if err != nil {
		return ParseOutcome{}, err
	}
	if sess == nil {
		return ParseOutcome{
			ResultSetComplete: true,
			SkipReason:        SkipNoSession,
		}, nil
	}
	if req.Fingerprint.Hash != "" {
		sess.File.Hash = req.Fingerprint.Hash
	}
	return ParseOutcome{
		Results: []ParseResultOutcome{{
			Result: ParseResult{
				Session:  *sess,
				Messages: msgs,
			},
			DataVersion: DataVersionCurrent,
		}},
		ResultSetComplete: true,
	}, nil
}

// openCodeProviderSpec parameterizes the one shared OpenCode-format
// provider implementation for OpenCode and its Kilo and MiMoCode forks.
// All three reuse the same discovery, source-lookup, fingerprinting,
// and parsing code; they differ only in the per-agent SQLite filename,
// the storage/<sessionSubdir> that holds session JSON, and the agent
// label/ID prefix applied via relabel. Kilo and MiMoCode parse through
// the OpenCode storage and SQLite readers, then relabel the result onto
// their own agent and ID prefix.
type openCodeProviderSpec struct {
	agent       AgentType
	format      openCodeFormat
	dbName      string
	listSQLite  func(string) ([]OpenCodeSessionMeta, error)
	sourceMtime func(string) (int64, error)
	relabel     func(*ParsedSession)
}

func openCodeProviderSpecForAgent(agent AgentType) openCodeProviderSpec {
	switch agent {
	case AgentOpenCode:
		return openCodeProviderSpec{
			agent:       AgentOpenCode,
			format:      openCodeFmt,
			dbName:      openCodeFmt.dbName,
			listSQLite:  ListOpenCodeSessionMeta,
			sourceMtime: OpenCodeSourceMtime,
		}
	case AgentKilo:
		return openCodeProviderSpec{
			agent:       AgentKilo,
			format:      kiloFmt,
			dbName:      kiloFmt.dbName,
			listSQLite:  ListKiloSessionMeta,
			sourceMtime: KiloSourceMtime,
			relabel:     relabelOpenCodeSessionAsKilo,
		}
	case AgentMiMoCode:
		return openCodeProviderSpec{
			agent:       AgentMiMoCode,
			format:      mimoFmt,
			dbName:      mimoFmt.dbName,
			listSQLite:  ListMiMoCodeSessionMeta,
			sourceMtime: MiMoCodeSourceMtime,
			relabel:     relabelOpenCodeSessionAsMiMoCode,
		}
	case AgentIcodemate:
		return openCodeProviderSpec{
			agent:       AgentIcodemate,
			format:      icodemateFmt,
			dbName:      icodemateFmt.dbName,
			listSQLite:  ListIcodemateSessionMeta,
			sourceMtime: IcodemateSourceMtime,
			relabel:     relabelOpenCodeSessionAsIcodemate,
		}
	default:
		return openCodeProviderSpec{}
	}
}

// resolve detects the OpenCode storage backend for a root.
func (spec openCodeProviderSpec) resolve(root string) OpenCodeSource {
	return resolveOpenCodeFormatSource(spec.format, root)
}

// discover lists file-backed storage session JSON files under a root.
func (spec openCodeProviderSpec) discover(root string) []DiscoveredFile {
	return discoverOpenCodeFormatSessions(spec.format, root)
}

// find locates a session source path (storage JSON or SQLite virtual
// path) by raw session ID under a root.
func (spec openCodeProviderSpec) find(root, sessionID string) string {
	return findOpenCodeFormatSourceFile(spec.format, root, sessionID)
}

// watchRoots returns the directories that should be watched for live
// updates under a configured root.
func (spec openCodeProviderSpec) watchRoots(root string) []string {
	return resolveOpenCodeFormatWatchRoots(spec.format, root)
}

// storageIDs returns the set of session IDs present as storage JSON
// under a root, used to skip duplicate SQLite metas in hybrid roots.
func (spec openCodeProviderSpec) storageIDs(root string) map[string]struct{} {
	return openCodeFormatStorageSessionIDs(spec.format, root)
}

// parseVirtual splits an opencode-format SQLite virtual path
// (<dbPath>#<sessionID>) when the DB base name matches this agent.
func (spec openCodeProviderSpec) parseVirtual(
	sourcePath string,
) (dbPath, sessionID string, ok bool) {
	return parseOpenCodeFormatVirtualPath(spec.dbName, sourcePath)
}

// parseFile parses a file-backed storage session and relabels it onto
// this agent's ID prefix when the agent is a fork of OpenCode.
func (spec openCodeProviderSpec) parseFile(
	sessionPath, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	sess, msgs, err := parseOpenCodeStorageFile(sessionPath, machine)
	if err != nil || sess == nil {
		return sess, msgs, err
	}
	if spec.relabel != nil {
		spec.relabel(sess)
	}
	return sess, msgs, nil
}

// parseSQLite parses a single SQLite-backed session and relabels it
// onto this agent's ID prefix when the agent is a fork of OpenCode.
func (spec openCodeProviderSpec) parseSQLite(
	dbPath, sessionID, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	sess, msgs, err := parseOpenCodeDBSession(dbPath, sessionID, machine)
	if err != nil || sess == nil {
		return sess, msgs, err
	}
	if spec.relabel != nil {
		spec.relabel(sess)
	}
	return sess, msgs, nil
}

type openCodeFormatSource struct {
	Root string
	Path string
}

type openCodeFormatSourceSet struct {
	roots []string
	spec  openCodeProviderSpec
}

func newOpenCodeFormatSourceSet(
	roots []string,
	spec openCodeProviderSpec,
) openCodeFormatSourceSet {
	return openCodeFormatSourceSet{
		roots: cleanJSONLRoots(roots),
		spec:  spec,
	}
}

func (s openCodeFormatSourceSet) Discover(ctx context.Context) ([]SourceRef, error) {
	var sources []SourceRef
	seen := make(map[string]struct{})
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		src := s.spec.resolve(root)
		storageIDs := map[string]struct{}{}
		if src.Mode == OpenCodeSourceStorage {
			for _, file := range s.spec.discover(root) {
				source, ok := s.sourceRef(root, file.Path, false)
				if !ok {
					continue
				}
				source.ProjectHint = file.Project
				addJSONLSource(source, &sources, seen)
			}
			storageIDs = s.spec.storageIDs(root)
		}
		if src.DBPath == "" || !IsRegularFile(src.DBPath) {
			continue
		}
		dbSources, err := s.sqliteSources(ctx, root, src.DBPath, storageIDs)
		if err != nil {
			if ctx.Err() != nil {
				return nil, err
			}
			// The SQLite DB is optional alongside filesystem storage. A
			// corrupt or unreadable DB must not abort discovery of the valid
			// storage-backed sessions in this root; scope the failure to the DB
			// portion, matching the legacy independent discovery paths. A
			// SQLite-only root has nothing to fall back to, so keep failing.
			if src.Mode == OpenCodeSourceStorage {
				log.Printf("sync %s: skipping unreadable %s: %v",
					s.spec.agent, src.DBPath, err)
				continue
			}
			return nil, err
		}
		for _, source := range dbSources {
			addJSONLSource(source, &sources, seen)
		}
	}
	sortJSONLSources(sources)
	return sources, nil
}

func (s openCodeFormatSourceSet) WatchPlan(context.Context) (WatchPlan, error) {
	roots := make([]WatchRoot, 0, len(s.roots))
	for _, root := range s.roots {
		for _, watchRoot := range s.spec.watchRoots(root) {
			roots = append(roots, WatchRoot{
				Path:      watchRoot,
				Recursive: true,
				IncludeGlobs: []string{
					"*.json",
					s.spec.dbName,
					s.spec.dbName + "-*",
				},
				DebounceKey: string(s.spec.agent) + ":opencode:" + watchRoot,
			})
		}
	}
	return WatchPlan{Roots: roots}, nil
}

func (s openCodeFormatSourceSet) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pathExists := true
	if _, err := os.Stat(req.Path); err != nil {
		if !os.IsNotExist(err) {
			return nil, nil
		}
		pathExists = false
	}
	for _, root := range s.roots {
		sources, ok, err := s.sourcesForChangedPathInRoot(
			ctx, root, req.Path, pathExists,
		)
		if err != nil || ok {
			return sources, err
		}
	}
	return nil, nil
}

func (s openCodeFormatSourceSet) FindSource(
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
			if source, ok := s.sourceRef(root, path, true); ok {
				return source, true, nil
			}
		}
	}
	if req.RawSessionID == "" {
		return SourceRef{}, false, nil
	}
	for _, root := range s.roots {
		path := s.spec.find(root, req.RawSessionID)
		if path == "" {
			continue
		}
		if source, ok := s.sourceRef(root, path, false); ok {
			return source, true, nil
		}
	}
	return SourceRef{}, false, nil
}

func (s openCodeFormatSourceSet) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	if err := ctx.Err(); err != nil {
		return SourceFingerprint{}, err
	}
	path, ok := s.pathFromSource(source)
	if !ok {
		return SourceFingerprint{}, fmt.Errorf("%s source path unavailable", s.spec.agent)
	}
	mtime, err := s.spec.sourceMtime(path)
	if err != nil {
		return SourceFingerprint{}, err
	}
	fingerprint := SourceFingerprint{
		Key:     firstNonEmptyJSONLString(source.FingerprintKey, source.Key, path),
		MTimeNS: mtime,
	}
	if dbPath, _, ok := s.spec.parseVirtual(path); ok {
		info, err := os.Stat(dbPath)
		if err != nil {
			return SourceFingerprint{}, fmt.Errorf("stat %s: %w", dbPath, err)
		}
		fingerprint.Size = info.Size()
		return fingerprint, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return SourceFingerprint{}, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.IsDir() {
		return SourceFingerprint{}, fmt.Errorf("stat %s: source is a directory", path)
	}
	fingerprint.Size = info.Size()
	fingerprint.Hash, err = openCodeProviderStorageFingerprint(path)
	if err != nil {
		return SourceFingerprint{}, err
	}
	return fingerprint, nil
}

func (s openCodeFormatSourceSet) pathFromSource(source SourceRef) (string, bool) {
	switch src := source.Opaque.(type) {
	case openCodeFormatSource:
		return src.Path, src.Path != ""
	case *openCodeFormatSource:
		if src != nil && src.Path != "" {
			return src.Path, true
		}
	}
	for _, candidate := range []string{
		source.DisplayPath,
		source.FingerprintKey,
		source.Key,
	} {
		for _, root := range s.roots {
			if ref, ok := s.sourceRef(root, candidate, false); ok {
				src := ref.Opaque.(openCodeFormatSource)
				return src.Path, true
			}
		}
	}
	return "", false
}

func (s openCodeFormatSourceSet) sqliteSources(
	ctx context.Context,
	root string,
	dbPath string,
	storageIDs map[string]struct{},
) ([]SourceRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	metas, err := s.spec.listSQLite(dbPath)
	if err != nil {
		return nil, err
	}
	sources := make([]SourceRef, 0, len(metas))
	for _, meta := range metas {
		if _, exists := storageIDs[meta.SessionID]; exists {
			continue
		}
		// meta was just read from this DB, so the session row is known to
		// exist. Build the SourceRef directly instead of routing through
		// sourceRef, which reopens the same SQLite DB once per row via
		// OpenCodeSQLiteSessionExists (O(n) opens for n sessions, and it would
		// silently drop a row whose redundant probe failed).
		source, ok := s.sqliteSourceRefFromMeta(root, meta.VirtualPath)
		if !ok {
			continue
		}
		sources = append(sources, source)
	}
	return sources, nil
}

// sqliteSourceRefFromMeta builds a SourceRef for a session row already listed
// from the SQLite DB at root. It validates the virtual path parses and that its
// DB lives under root, but unlike sourceRef it skips the per-row
// OpenCodeSQLiteSessionExists probe because the caller read the row from that
// same DB moments earlier.
func (s openCodeFormatSourceSet) sqliteSourceRefFromMeta(
	root string,
	virtualPath string,
) (SourceRef, bool) {
	root = filepath.Clean(root)
	path := filepath.Clean(virtualPath)
	dbPath, _, ok := s.spec.parseVirtual(path)
	if !ok {
		return SourceRef{}, false
	}
	if _, under := relUnder(root, dbPath); !under {
		return SourceRef{}, false
	}
	return s.newSourceRef(root, path, ""), true
}

func (s openCodeFormatSourceSet) sourcesForChangedPathInRoot(
	ctx context.Context,
	root string,
	path string,
	pathExists bool,
) ([]SourceRef, bool, error) {
	rel, ok := relUnder(root, path)
	if !ok {
		return nil, false, nil
	}
	base := filepath.Base(rel)
	if rel == s.spec.dbName || strings.HasPrefix(base, s.spec.dbName+"-") {
		dbPath := filepath.Join(root, s.spec.dbName)
		if !IsRegularFile(dbPath) {
			return nil, true, nil
		}
		storageIDs := map[string]struct{}{}
		if s.spec.resolve(root).Mode == OpenCodeSourceStorage {
			storageIDs = s.spec.storageIDs(root)
		}
		sources, err := s.sqliteSources(ctx, root, dbPath, storageIDs)
		return sources, true, err
	}

	src := s.spec.resolve(root)
	if src.Mode != OpenCodeSourceStorage {
		return nil, false, nil
	}
	parts := strings.Split(rel, string(filepath.Separator))
	sessionSubdir := filepath.Base(src.SessionRoot)
	switch {
	case pathExists &&
		len(parts) == 4 &&
		parts[0] == "storage" &&
		parts[1] == sessionSubdir &&
		strings.HasSuffix(parts[3], ".json"):
		source, ok := s.sourceRef(root, path, false)
		if !ok {
			return nil, true, nil
		}
		return []SourceRef{source}, true, nil
	case !pathExists &&
		len(parts) == 4 &&
		parts[0] == "storage" &&
		parts[1] == sessionSubdir &&
		strings.HasSuffix(parts[3], ".json"):
		source, ok := s.sourceRefFromStoragePath(root, path)
		if !ok {
			return nil, true, nil
		}
		return []SourceRef{source}, true, nil
	case len(parts) == 4 &&
		parts[0] == "storage" &&
		parts[1] == "message" &&
		strings.HasSuffix(parts[3], ".json"):
		source, ok := s.sourceForRawID(root, parts[2])
		if !ok {
			return nil, false, nil
		}
		return []SourceRef{source}, true, nil
	case len(parts) == 4 &&
		parts[0] == "storage" &&
		parts[1] == "part" &&
		strings.HasSuffix(parts[3], ".json"):
		sessionID := ""
		if pathExists {
			sessionID = readOpenCodeProviderStorageSessionID(path)
		}
		if sessionID == "" {
			sessionID = findOpenCodeProviderStorageSessionIDByMessageID(root, parts[2])
		}
		if sessionID == "" {
			return nil, false, nil
		}
		source, ok := s.sourceForRawID(root, sessionID)
		if !ok {
			return nil, false, nil
		}
		return []SourceRef{source}, true, nil
	case !pathExists &&
		len(parts) == 3 &&
		parts[0] == "storage" &&
		parts[1] == "message":
		source, ok := s.sourceForRawID(root, parts[2])
		if !ok {
			return nil, false, nil
		}
		return []SourceRef{source}, true, nil
	case !pathExists &&
		len(parts) == 3 &&
		parts[0] == "storage" &&
		parts[1] == "part":
		sessionID := findOpenCodeProviderStorageSessionIDByMessageID(root, parts[2])
		if sessionID == "" {
			return nil, false, nil
		}
		source, ok := s.sourceForRawID(root, sessionID)
		if !ok {
			return nil, false, nil
		}
		return []SourceRef{source}, true, nil
	}
	return nil, false, nil
}

func (s openCodeFormatSourceSet) sourceForRawID(root, sessionID string) (SourceRef, bool) {
	path := s.spec.find(root, sessionID)
	if path == "" {
		return SourceRef{}, false
	}
	return s.sourceRef(root, path, false)
}

func (s openCodeFormatSourceSet) sourceRef(
	root string,
	path string,
	promoteVirtual bool,
) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if dbPath, sessionID, ok := s.spec.parseVirtual(path); ok {
		if _, under := relUnder(root, dbPath); !under {
			return SourceRef{}, false
		}
		if promoteVirtual {
			if selected := s.spec.find(root, sessionID); selected != "" &&
				selected != path {
				return s.sourceRef(root, selected, false)
			}
		}
		if !OpenCodeSQLiteSessionExists(dbPath, sessionID) {
			return SourceRef{}, false
		}
		return s.newSourceRef(root, path, ""), true
	}
	if !s.isStorageSessionPath(root, path, true) {
		return SourceRef{}, false
	}
	return s.sourceRefFromStoragePath(root, path)
}

func (s openCodeFormatSourceSet) sourceRefFromStoragePath(
	root string,
	path string,
) (SourceRef, bool) {
	if !s.isStorageSessionPath(root, path, false) {
		return SourceRef{}, false
	}
	return s.newSourceRef(root, path, openCodeSessionProject(path)), true
}

func (s openCodeFormatSourceSet) newSourceRef(
	root string,
	path string,
	project string,
) SourceRef {
	return SourceRef{
		Provider:       s.spec.agent,
		Key:            path,
		DisplayPath:    path,
		FingerprintKey: path,
		ProjectHint:    project,
		Opaque: openCodeFormatSource{
			Root: root,
			Path: path,
		},
	}
}

func (s openCodeFormatSourceSet) isStorageSessionPath(
	root string,
	path string,
	requireExisting bool,
) bool {
	rel, ok := relUnder(root, path)
	if !ok {
		return false
	}
	src := s.spec.resolve(root)
	if src.Mode != OpenCodeSourceStorage {
		return false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	return len(parts) == 4 &&
		parts[0] == "storage" &&
		parts[1] == filepath.Base(src.SessionRoot) &&
		strings.HasSuffix(parts[3], ".json") &&
		(!requireExisting || IsRegularFile(path))
}

func openCodeProviderStorageFingerprint(sessionPath string) (string, error) {
	root := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(sessionPath))))
	sessionID := strings.TrimSuffix(filepath.Base(sessionPath), filepath.Ext(sessionPath))
	msgs, err := loadOpenCodeStorageMessages(root, sessionID)
	if err != nil {
		return "", err
	}
	parts, err := loadOpenCodeStorageParts(root, msgs)
	if err != nil {
		return "", err
	}
	return buildOpenCodeStorageFingerprint(msgs, parts), nil
}

func readOpenCodeProviderStorageSessionID(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var data struct {
		SessionID string `json:"sessionID"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return ""
	}
	return data.SessionID
}

func findOpenCodeProviderStorageSessionIDByMessageID(
	openCodeDir, messageID string,
) string {
	messageRoot := filepath.Join(openCodeDir, "storage", "message")
	entries, err := os.ReadDir(messageRoot)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(messageRoot, entry.Name(), messageID+".json")
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return entry.Name()
		}
	}
	return ""
}

func openCodeFormatProviderCapabilities() Capabilities {
	return Capabilities{
		Source: SourceCapabilities{
			DiscoverSources:      CapabilitySupported,
			WatchSources:         CapabilitySupported,
			ClassifyChangedPath:  CapabilitySupported,
			FindSource:           CapabilitySupported,
			CompositeFingerprint: CapabilitySupported,
			IncrementalAppend:    CapabilityNotApplicable,
			MultiSessionSource:   CapabilityNotApplicable,
			PerSessionErrors:     CapabilityNotApplicable,
			ExcludedSessions:     CapabilityNotApplicable,
			ForceReplaceOnParse:  CapabilityNotApplicable,
		},
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			Cwd:                  CapabilitySupported,
			Relationships:        CapabilitySupported,
			Thinking:             CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			PerMessageTokenUsage: CapabilitySupported,
			Model:                CapabilitySupported,
		},
	}
}
