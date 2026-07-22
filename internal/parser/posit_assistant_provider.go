package parser

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// Posit Assistant (posit-dev/assistant) persists one directory per
// conversation under <root>/<workspaceId>/<conversationId>/, holding a
// conversation.json V3 message tree, an append-only lm-messages.jsonl
// transcript of Vercel AI SDK ModelMessages, and a ui-messages.jsonl render
// log. Subagent runs nest under <conversationDir>/subagents/<subagentId>/
// with the same layout. Workspace metadata (the project folder path) lives in
// <root>/<workspaceId>/workspace.json.
const (
	positAssistantConversationFile = "conversation.json"
	positAssistantLMMessagesFile   = "lm-messages.jsonl"
	positAssistantUIMessagesFile   = "ui-messages.jsonl"
	positAssistantWorkspaceFile    = "workspace.json"
	positAssistantSubagentsDir     = "subagents"
	positAssistantIDPrefix         = "posit-assistant:"
)

// positAssistantSummaryTagRe strips the assistant's inline bookkeeping tags
// (<MESSAGESUMMARY> and <CONVERSATIONSUMMARY>) from displayed message text so
// they do not pollute transcripts or full-text search.
var positAssistantSummaryTagRe = regexp.MustCompile(
	`(?s)<(MESSAGESUMMARY|CONVERSATIONSUMMARY)>.*?</(MESSAGESUMMARY|CONVERSATIONSUMMARY)>`,
)

var _ Provider = (*positAssistantProvider)(nil)

type positAssistantProviderFactory struct {
	def AgentDef
}

func newPositAssistantProviderFactory(def AgentDef) ProviderFactory {
	return positAssistantProviderFactory{def: cloneAgentDef(def)}
}

func (f positAssistantProviderFactory) Definition() AgentDef {
	return cloneAgentDef(f.def)
}

func (f positAssistantProviderFactory) Capabilities() Capabilities {
	return positAssistantProviderCapabilities()
}

func (f positAssistantProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	cfg = cfg.Clone()
	return &positAssistantProvider{
		ProviderBase: ProviderBase{
			Def:    cloneAgentDef(f.def),
			Caps:   positAssistantProviderCapabilities(),
			Config: cfg,
		},
		sources: newPositAssistantSourceSet(cfg.Roots),
	}
}

type positAssistantProvider struct {
	ProviderBase
	sources positAssistantSourceSet
}

func (p *positAssistantProvider) Discover(ctx context.Context) ([]SourceRef, error) {
	return p.sources.Discover(ctx)
}

func (p *positAssistantProvider) DiscoverEach(ctx context.Context, yield func(SourceRef) error) error {
	return p.sources.DiscoverEach(ctx, yield)
}

func (p *positAssistantProvider) WatchPlan(ctx context.Context) (WatchPlan, error) {
	return p.sources.WatchPlan(ctx)
}

func (p *positAssistantProvider) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	return p.sources.SourcesForChangedPath(ctx, req)
}

func (p *positAssistantProvider) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	req = ProviderFindRequestWithRawSessionID(p.Def, req)
	return p.sources.FindSource(ctx, req)
}

func (p *positAssistantProvider) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	return p.sources.Fingerprint(ctx, source)
}

func (p *positAssistantProvider) Parse(
	ctx context.Context,
	req ParseRequest,
) (ParseOutcome, error) {
	if err := ctx.Err(); err != nil {
		return ParseOutcome{}, err
	}
	src, ok := p.sources.sourceFromRef(req.Source)
	if !ok {
		return ParseOutcome{}, fmt.Errorf("posit-assistant source path unavailable")
	}
	if req.Source.ProjectHint != "" {
		src.Project = req.Source.ProjectHint
	}
	machine := firstNonEmptyJSONLString(req.Machine, p.Config.Machine)
	sess, msgs, err := parsePositAssistantConversation(src, machine)
	if err != nil {
		return ParseOutcome{}, err
	}
	if sess == nil {
		return ParseOutcome{
			ResultSetComplete: true,
			SkipReason:        SkipNoSession,
		}, nil
	}
	if req.Fingerprint.Size > 0 {
		sess.File.Size = req.Fingerprint.Size
	}
	if req.Fingerprint.MTimeNS > 0 {
		sess.File.Mtime = req.Fingerprint.MTimeNS
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

type positAssistantSource struct {
	Root    string
	Path    string // conversation.json path
	Project string
	Cwd     string
}

type positAssistantSourceSet struct {
	roots []string
}

func newPositAssistantSourceSet(roots []string) positAssistantSourceSet {
	return positAssistantSourceSet{roots: cleanJSONLRoots(roots)}
}

func (s positAssistantSourceSet) Discover(ctx context.Context) ([]SourceRef, error) {
	var sources []SourceRef
	seen := make(map[string]struct{})
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		workspaces, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, workspace := range workspaces {
			if !workspace.IsDir() {
				continue
			}
			wsDir := filepath.Join(root, workspace.Name())
			project, cwd := positAssistantWorkspaceProject(wsDir)
			entries, err := os.ReadDir(wsDir)
			if err != nil {
				continue
			}
			for _, entry := range entries {
				if !entry.IsDir() {
					continue
				}
				s.collectConversationSources(
					root,
					filepath.Join(wsDir, entry.Name()),
					project,
					cwd,
					&sources,
					seen,
				)
			}
		}
	}
	sortJSONLSources(sources)
	return sources, nil
}

func (s positAssistantSourceSet) DiscoverEach(
	ctx context.Context, yield func(SourceRef) error,
) error {
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := streamDirectoryEntries(ctx, root, func(workspace os.DirEntry) error {
			if !workspace.IsDir() {
				return nil
			}
			wsDir := filepath.Join(root, workspace.Name())
			project, cwd := positAssistantWorkspaceProject(wsDir)
			return streamDirectoryEntries(ctx, wsDir, func(entry os.DirEntry) error {
				if entry.IsDir() {
					return s.yieldConversationSources(ctx, root, filepath.Join(wsDir, entry.Name()), project, cwd, yield)
				}
				return nil
			})
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (s positAssistantSourceSet) yieldConversationSources(
	ctx context.Context, root, convDir, project, cwd string,
	yield func(SourceRef) error,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !IsValidSessionID(filepath.Base(convDir)) {
		return nil
	}
	convPath := filepath.Join(convDir, positAssistantConversationFile)
	if IsRegularFile(convPath) {
		if err := yield(s.newSourceRef(root, convPath, project, cwd)); err != nil {
			return err
		}
	}
	return streamDirectoryEntries(ctx, filepath.Join(convDir, positAssistantSubagentsDir), func(sub os.DirEntry) error {
		if sub.IsDir() {
			return s.yieldConversationSources(ctx, root, filepath.Join(convDir, positAssistantSubagentsDir, sub.Name()), project, cwd, yield)
		}
		return nil
	})
}

// collectConversationSources adds convDir as a source when it holds a
// conversation.json, then recurses into its subagents/ directory, so nested
// subagent conversations become their own sessions.
func (s positAssistantSourceSet) collectConversationSources(
	root, convDir, project, cwd string,
	sources *[]SourceRef,
	seen map[string]struct{},
) {
	if !IsValidSessionID(filepath.Base(convDir)) {
		return
	}
	convPath := filepath.Join(convDir, positAssistantConversationFile)
	if IsRegularFile(convPath) {
		addJSONLSource(s.newSourceRef(root, convPath, project, cwd), sources, seen)
	}
	subEntries, err := os.ReadDir(filepath.Join(convDir, positAssistantSubagentsDir))
	if err != nil {
		return
	}
	for _, sub := range subEntries {
		if !sub.IsDir() {
			continue
		}
		s.collectConversationSources(
			root,
			filepath.Join(convDir, positAssistantSubagentsDir, sub.Name()),
			project,
			cwd,
			sources,
			seen,
		)
	}
}

func (s positAssistantSourceSet) WatchPlan(context.Context) (WatchPlan, error) {
	roots := make([]WatchRoot, 0, len(s.roots))
	for _, root := range s.roots {
		roots = append(roots, WatchRoot{
			Path:         root,
			Recursive:    true,
			IncludeGlobs: []string{"*.json", "*.jsonl"},
			DebounceKey:  string(AgentPositAssistant) + ":workspaces:" + root,
		})
	}
	return WatchPlan{Roots: roots}, nil
}

func (s positAssistantSourceSet) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, root := range s.roots {
		sources := s.sourcesForWorkspaceManifest(root, req.Path)
		if len(sources) > 0 {
			return sources, nil
		}
		source, ok := s.sourceRefForChangedPath(root, req.Path)
		if ok {
			return []SourceRef{source}, nil
		}
	}
	return nil, nil
}

func (s positAssistantSourceSet) FindSource(
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
			if source, ok := s.sourceRef(root, path); ok {
				return source, true, nil
			}
		}
	}
	if req.RawSessionID == "" {
		return SourceRef{}, false, nil
	}
	for _, root := range s.roots {
		path := s.findSourceFile(root, req.RawSessionID)
		if path == "" {
			continue
		}
		if source, ok := s.sourceRef(root, path); ok {
			return source, true, nil
		}
	}
	return SourceRef{}, false, nil
}

// findSourceFile locates a conversation.json by conversation ID (prefix
// already stripped), checking direct workspace children first and then
// nested subagent directories.
func (s positAssistantSourceSet) findSourceFile(root, rawID string) string {
	if !IsValidSessionID(rawID) {
		return ""
	}
	workspaces, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	for _, workspace := range workspaces {
		if !workspace.IsDir() {
			continue
		}
		wsDir := filepath.Join(root, workspace.Name())
		direct := filepath.Join(wsDir, rawID, positAssistantConversationFile)
		if IsRegularFile(direct) {
			return direct
		}
		entries, err := os.ReadDir(wsDir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			path := positAssistantFindInSubagents(
				filepath.Join(wsDir, entry.Name()), rawID,
			)
			if path != "" {
				return path
			}
		}
	}
	return ""
}

func positAssistantFindInSubagents(convDir, rawID string) string {
	entries, err := os.ReadDir(filepath.Join(convDir, positAssistantSubagentsDir))
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		subDir := filepath.Join(convDir, positAssistantSubagentsDir, entry.Name())
		if entry.Name() == rawID {
			convPath := filepath.Join(subDir, positAssistantConversationFile)
			if IsRegularFile(convPath) {
				return convPath
			}
		}
		if path := positAssistantFindInSubagents(subDir, rawID); path != "" {
			return path
		}
	}
	return ""
}

func (s positAssistantSourceSet) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	if err := ctx.Err(); err != nil {
		return SourceFingerprint{}, err
	}
	src, ok := s.sourceFromRef(source)
	if !ok {
		return SourceFingerprint{}, fmt.Errorf("posit-assistant source path unavailable")
	}
	info, err := os.Stat(src.Path)
	if err != nil {
		return SourceFingerprint{}, fmt.Errorf("stat %s: %w", src.Path, err)
	}
	if info.IsDir() {
		return SourceFingerprint{}, fmt.Errorf(
			"stat %s: source is a directory", src.Path,
		)
	}
	fingerprint := SourceFingerprint{
		Key:     firstNonEmptyJSONLString(source.FingerprintKey, source.Key, src.Path),
		Size:    info.Size(),
		MTimeNS: info.ModTime().UnixNano(),
	}
	lmPath := filepath.Join(filepath.Dir(src.Path), positAssistantLMMessagesFile)
	if lmInfo, err := os.Stat(lmPath); err == nil {
		fingerprint.Size += lmInfo.Size()
		if mtime := lmInfo.ModTime().UnixNano(); mtime > fingerprint.MTimeNS {
			fingerprint.MTimeNS = mtime
		}
	}
	wsPath := s.workspaceManifestForSource(src)
	if wsPath != "" {
		if wsInfo, err := os.Stat(wsPath); err == nil {
			fingerprint.Size += wsInfo.Size()
			if mtime := wsInfo.ModTime().UnixNano(); mtime > fingerprint.MTimeNS {
				fingerprint.MTimeNS = mtime
			}
		}
	}
	fingerprint.Hash, err = positAssistantSourceHash(src.Path, lmPath, wsPath)
	if err != nil {
		return SourceFingerprint{}, err
	}
	return fingerprint, nil
}

// workspaceManifestForSource returns the workspace.json path governing a
// conversation source, or "" when the workspace has no manifest.
func (s positAssistantSourceSet) workspaceManifestForSource(
	src positAssistantSource,
) string {
	rel, ok := relUnder(filepath.Clean(src.Root), filepath.Clean(src.Path))
	if !ok {
		return ""
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) < 3 {
		return ""
	}
	manifest := filepath.Join(src.Root, parts[0], positAssistantWorkspaceFile)
	if !IsRegularFile(manifest) {
		return ""
	}
	return manifest
}

// positAssistantSourceHash combines the conversation tree, transcript, and
// workspace manifest hashes. The manifest is included because it supplies the
// session's project and cwd, so editing or creating it must invalidate the
// skip cache and freshness checks, not just re-emit sources.
func positAssistantSourceHash(convPath, lmPath, wsPath string) (string, error) {
	hash, err := hashJSONLSourceFile(convPath)
	if err != nil {
		return "", err
	}
	combined := "conversation\x00" + hash
	composite := false
	if IsRegularFile(lmPath) {
		lmHash, err := hashJSONLSourceFile(lmPath)
		if err != nil {
			return "", err
		}
		combined += "\x00lm\x00" + lmHash
		composite = true
	}
	if wsPath != "" && IsRegularFile(wsPath) {
		wsHash, err := hashJSONLSourceFile(wsPath)
		if err != nil {
			return "", err
		}
		combined += "\x00workspace\x00" + wsHash
		composite = true
	}
	if !composite {
		return hash, nil
	}
	h := sha256.New()
	_, _ = h.Write([]byte(combined))
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func (s positAssistantSourceSet) sourceFromRef(
	source SourceRef,
) (positAssistantSource, bool) {
	switch src := source.Opaque.(type) {
	case positAssistantSource:
		if src.Path != "" {
			return src, true
		}
	case *positAssistantSource:
		if src != nil && src.Path != "" {
			return *src, true
		}
	}
	for _, candidate := range []string{
		source.DisplayPath, source.FingerprintKey, source.Key,
	} {
		if candidate == "" {
			continue
		}
		for _, root := range s.roots {
			if ref, ok := s.sourceRef(root, candidate); ok {
				return ref.Opaque.(positAssistantSource), true
			}
		}
	}
	return positAssistantSource{}, false
}

func (s positAssistantSourceSet) sourceRef(root, path string) (SourceRef, bool) {
	source, ok := s.validatedSourceRef(root, path)
	if !ok || !IsRegularFile(source.DisplayPath) {
		return SourceRef{}, false
	}
	return source, true
}

func (s positAssistantSourceSet) sourceRefForChangedPath(
	root, path string,
) (SourceRef, bool) {
	switch filepath.Base(filepath.Clean(path)) {
	case positAssistantConversationFile,
		positAssistantLMMessagesFile:
	default:
		return SourceRef{}, false
	}
	convPath := filepath.Join(
		filepath.Dir(filepath.Clean(path)), positAssistantConversationFile,
	)
	// Classify structurally, without requiring conversation.json to exist:
	// a removed lm-messages.jsonl must still map to its surviving conversation
	// source so the session reparses. When the conversation.json itself is
	// deleted, the engine drops the source and the stored session stays
	// archived, matching the persistent-archive policy shared by all
	// file-based providers.
	return s.validatedSourceRef(root, convPath)
}

// validatedSourceRef builds a SourceRef for a conversation.json path when the
// path is structurally inside a conversation directory under root:
// <workspaceId>/<conversationId>(/subagents/<id>)*/conversation.json. It does
// not require the file to exist.
func (s positAssistantSourceSet) validatedSourceRef(
	root, path string,
) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rel, ok := relUnder(root, path)
	if !ok {
		return SourceRef{}, false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) < 3 ||
		parts[len(parts)-1] != positAssistantConversationFile ||
		!positAssistantConversationDirParts(parts[:len(parts)-1]) {
		return SourceRef{}, false
	}
	project, cwd := positAssistantWorkspaceProject(filepath.Join(root, parts[0]))
	return s.newSourceRef(root, path, project, cwd), true
}

// positAssistantConversationDirParts reports whether parts form a
// conversation directory path relative to a configured root:
// <workspaceId>/<conversationId> followed by zero or more
// subagents/<subagentId> pairs.
func positAssistantConversationDirParts(parts []string) bool {
	if len(parts) < 2 ||
		!IsValidSessionID(parts[0]) ||
		!IsValidSessionID(parts[1]) {
		return false
	}
	for i := 2; i < len(parts); i += 2 {
		if parts[i] != positAssistantSubagentsDir ||
			i+1 >= len(parts) ||
			!IsValidSessionID(parts[i+1]) {
			return false
		}
	}
	return true
}

func (s positAssistantSourceSet) sourcesForWorkspaceManifest(
	root, path string,
) []SourceRef {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rel, ok := relUnder(root, path)
	if !ok {
		return nil
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 2 || parts[1] != positAssistantWorkspaceFile {
		return nil
	}
	wsDir := filepath.Join(root, parts[0])
	project, cwd := positAssistantWorkspaceProject(wsDir)
	entries, err := os.ReadDir(wsDir)
	if err != nil {
		return nil
	}
	var sources []SourceRef
	seen := make(map[string]struct{})
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		s.collectConversationSources(
			root, filepath.Join(wsDir, entry.Name()), project, cwd, &sources, seen,
		)
	}
	sortJSONLSources(sources)
	return sources
}

func (s positAssistantSourceSet) newSourceRef(
	root, path, project, cwd string,
) SourceRef {
	return SourceRef{
		Provider:       AgentPositAssistant,
		Key:            path,
		DisplayPath:    path,
		FingerprintKey: path,
		ProjectHint:    project,
		Opaque: positAssistantSource{
			Root:    root,
			Path:    path,
			Project: project,
			Cwd:     cwd,
		},
	}
}

// positAssistantWorkspaceProject reads a workspace.json manifest and returns
// the project display name (folder basename) and the workspace folder path.
func positAssistantWorkspaceProject(wsDir string) (string, string) {
	data, err := os.ReadFile(filepath.Join(wsDir, positAssistantWorkspaceFile))
	if err != nil {
		return "unknown", ""
	}
	var manifest struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil || manifest.Path == "" {
		return "unknown", ""
	}
	return filepath.Base(manifest.Path), manifest.Path
}

// positAssistantConversation mirrors the V3 conversation.json tree written by
// Posit Assistant's ConversationManagerDisk.
type positAssistantConversation struct {
	SchemaVersion string `json:"schemaVersion"`
	Root          struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		Summary   string `json:"summary"`
		Timestamp int64  `json:"timestamp"`
		Metadata  struct {
			Kind         string `json:"kind"`
			ParentTreeID string `json:"parentTreeId"`
			Task         string `json:"task"`
			// GitBranch is recorded by Posit Assistant releases that persist
			// the workspace's checked-out branch; absent in older files.
			GitBranch string `json:"gitBranch"`
		} `json:"metadata"`
	} `json:"root"`
	Messages []positAssistantTreeNode `json:"messages"`
}

type positAssistantTreeNode struct {
	ID           string `json:"id"`
	ParentID     string `json:"parentId"`
	IsActive     bool   `json:"isActive"`
	LMMessageIDs []int  `json:"lmMessageIds"`
	Timestamp    int64  `json:"timestamp"`
}

func parsePositAssistantConversation(
	src positAssistantSource,
	machine string,
) (*ParsedSession, []ParsedMessage, error) {
	info, err := os.Stat(src.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("stat %s: %w", src.Path, err)
	}
	data, err := os.ReadFile(src.Path)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", src.Path, err)
	}
	if len(data) == 0 {
		return nil, nil, nil
	}
	var conv positAssistantConversation
	if err := json.Unmarshal(data, &conv); err != nil {
		return nil, nil, fmt.Errorf("unmarshal %s: %w", src.Path, err)
	}

	convDir := filepath.Dir(src.Path)
	lmMessages, malformed, err := readPositAssistantLMMessages(
		filepath.Join(convDir, positAssistantLMMessagesFile),
	)
	if err != nil {
		return nil, nil, err
	}

	var messages []ParsedMessage
	firstMessage := ""
	for _, node := range conv.Messages {
		if !node.IsActive {
			continue
		}
		nodeTime := positAssistantTime(node.Timestamp)
		for _, lmID := range node.LMMessageIDs {
			lm, ok := lmMessages[lmID]
			if !ok {
				malformed++
				continue
			}
			msg, ok := positAssistantMessageFromLM(lm, nodeTime, len(messages))
			if !ok {
				continue
			}
			if firstMessage == "" && msg.Role == RoleUser && msg.Content != "" {
				firstMessage = truncate(
					strings.ReplaceAll(msg.Content, "\n", " "), 300,
				)
			}
			messages = append(messages, msg)
		}
	}
	if len(messages) == 0 {
		return nil, nil, nil
	}

	if firstMessage == "" {
		firstMessage = truncate(strings.ReplaceAll(
			firstNonEmptyJSONLString(
				conv.Root.Metadata.Task, conv.Root.Summary,
			), "\n", " ",
		), 300)
	}

	startedAt := positAssistantTime(conv.Root.Timestamp)
	endedAt := startedAt
	userCount := 0
	for _, msg := range messages {
		if startedAt.IsZero() ||
			(!msg.Timestamp.IsZero() && msg.Timestamp.Before(startedAt)) {
			startedAt = msg.Timestamp
		}
		if msg.Timestamp.After(endedAt) {
			endedAt = msg.Timestamp
		}
		if msg.Role == RoleUser && msg.Content != "" {
			userCount++
		}
	}

	sessionID := filepath.Base(convDir)
	sess := &ParsedSession{
		ID:               positAssistantIDPrefix + sessionID,
		Project:          src.Project,
		Machine:          machine,
		Agent:            AgentPositAssistant,
		Cwd:              src.Cwd,
		GitBranch:        conv.Root.Metadata.GitBranch,
		SessionName:      conv.Root.Title,
		FirstMessage:     firstMessage,
		StartedAt:        startedAt,
		EndedAt:          endedAt,
		MessageCount:     len(messages),
		UserMessageCount: userCount,
		MalformedLines:   malformed,
		File: FileInfo{
			Path:  src.Path,
			Size:  info.Size(),
			Mtime: info.ModTime().UnixNano(),
		},
	}
	if conv.Root.Metadata.Kind == "subagent" && conv.Root.Metadata.ParentTreeID != "" {
		sess.ParentSessionID = positAssistantIDPrefix + conv.Root.Metadata.ParentTreeID
		sess.RelationshipType = RelSubagent
	}
	accumulateMessageTokenUsage(sess, messages)
	return sess, messages, nil
}

// readPositAssistantLMMessages loads lm-messages.jsonl into a map keyed by
// the numeric message ID that conversation.json tree nodes reference. A
// missing transcript yields an empty map so the tree still parses.
func readPositAssistantLMMessages(
	path string,
) (map[int]gjson.Result, int, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[int]gjson.Result{}, 0, nil
		}
		return nil, 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	lines := make(map[int]gjson.Result)
	malformed := 0
	lr := newLineReader(f, maxLineSize)
	defer releaseLineReader(lr)
	for {
		line, ok := lr.next()
		if !ok {
			break
		}
		if !gjson.Valid(line) {
			malformed++
			continue
		}
		parsed := gjson.Parse(line)
		id := parsed.Get("id")
		message := parsed.Get("message")
		if !id.Exists() || !message.Exists() {
			malformed++
			continue
		}
		lines[int(id.Int())] = message
	}
	if err := lr.Err(); err != nil {
		return nil, 0, fmt.Errorf("read %s: %w", path, err)
	}
	return lines, malformed, nil
}

// positAssistantMessageFromLM converts one Vercel AI SDK ModelMessage into a
// ParsedMessage. Returns false for roles that carry no transcript content.
func positAssistantMessageFromLM(
	message gjson.Result,
	nodeTime time.Time,
	ordinal int,
) (ParsedMessage, bool) {
	role := message.Get("role").Str
	content := message.Get("content")
	positai := message.Get("providerOptions.providerMetadata.positai")

	timestamp := nodeTime
	if ms := positai.Get("timestamp").Int(); ms > 0 {
		timestamp = positAssistantTime(ms)
	}

	msg := ParsedMessage{
		Ordinal:   ordinal,
		Timestamp: timestamp,
	}
	switch role {
	case "user":
		msg.Role = RoleUser
		msg.Content = positAssistantTextContent(content)
	case "system":
		msg.Role = RoleSystem
		msg.IsSystem = true
		msg.Content = positAssistantTextContent(content)
	case "assistant":
		msg.Role = RoleAssistant
		positAssistantFillAssistant(&msg, content, positai)
	case "tool":
		msg.Role = RoleTool
		msg.ToolResults = positAssistantToolResults(content)
		if len(msg.ToolResults) == 0 {
			return ParsedMessage{}, false
		}
	default:
		return ParsedMessage{}, false
	}
	msg.ContentLength = len(msg.Content)
	return msg, true
}

// positAssistantTextContent extracts display text from a ModelMessage content
// value, which is either a plain string or an array of typed parts.
func positAssistantTextContent(content gjson.Result) string {
	if content.Type == gjson.String {
		return content.Str
	}
	var parts []string
	for _, part := range content.Array() {
		if part.Get("type").Str == "text" {
			if text := part.Get("text").Str; text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func positAssistantFillAssistant(
	msg *ParsedMessage,
	content, positai gjson.Result,
) {
	var textParts, thinkingParts []string
	if content.Type == gjson.String {
		textParts = append(textParts, content.Str)
	}
	for _, part := range content.Array() {
		switch part.Get("type").Str {
		case "text":
			if text := part.Get("text").Str; text != "" {
				textParts = append(textParts, text)
			}
		case "reasoning":
			if text := part.Get("text").Str; text != "" {
				thinkingParts = append(thinkingParts, text)
			}
		case "tool-call":
			if tc, ok := positAssistantToolCall(part); ok {
				msg.ToolCalls = append(msg.ToolCalls, tc)
			}
		}
	}
	msg.Content = strings.TrimSpace(
		positAssistantSummaryTagRe.ReplaceAllString(
			strings.Join(textParts, "\n"), "",
		),
	)
	msg.ThinkingText = strings.Join(thinkingParts, "\n\n")
	msg.HasThinking = len(thinkingParts) > 0
	msg.HasToolUse = len(msg.ToolCalls) > 0
	msg.Model = positai.Get("modelId").Str
	positAssistantFillTokenUsage(msg, positai.Get("usage"))
}

func positAssistantToolCall(part gjson.Result) (ParsedToolCall, bool) {
	name := part.Get("toolName").Str
	if name == "" {
		return ParsedToolCall{}, false
	}
	input := part.Get("input")
	tc := ParsedToolCall{
		ToolUseID: part.Get("toolCallId").Str,
		ToolName:  name,
		Category:  NormalizeToolCategory(name),
		InputJSON: input.Raw,
	}
	switch tc.Category {
	case "Read", "Edit", "Write":
		tc.FilePath = ResolveFilePathFromJSON(input.Raw)
	}
	if name == "skill" {
		tc.SkillName = firstNonEmptyJSONLString(
			input.Get("skill").Str, input.Get("name").Str,
		)
	}
	return tc, true
}

func positAssistantToolResults(content gjson.Result) []ParsedToolResult {
	var results []ParsedToolResult
	for _, part := range content.Array() {
		if part.Get("type").Str != "tool-result" {
			continue
		}
		output := part.Get("output")
		results = append(results, ParsedToolResult{
			ToolUseID:     part.Get("toolCallId").Str,
			ContentLength: len(output.Get("value").Str),
			ContentRaw:    output.Raw,
		})
	}
	return results
}

// positAssistantFillTokenUsage maps positai usage metadata onto the message.
// ContextTokens counts the full billed context (fresh input plus cache reads
// and writes), matching the Claude parser's attribution. TokenUsage is
// re-emitted with Claude-style snake_case keys so downstream token-presence
// and cost consumers read it uniformly.
func positAssistantFillTokenUsage(msg *ParsedMessage, usage gjson.Result) {
	if !usage.Exists() {
		return
	}
	tokenUsage := make(map[string]int, 4)
	input, output, cacheRead, cacheWrite := 0, 0, 0, 0

	inputField := usage.Get("inputTokens")
	if inputField.Exists() {
		input = int(inputField.Int())
		tokenUsage["input_tokens"] = input
	}
	outputField := usage.Get("outputTokens")
	if outputField.Exists() {
		output = int(outputField.Int())
		tokenUsage["output_tokens"] = output
	}
	cacheReadField := usage.Get("cacheReadTokens")
	if cacheReadField.Exists() {
		cacheRead = int(cacheReadField.Int())
		tokenUsage["cache_read_input_tokens"] = cacheRead
	}
	cacheWriteField := usage.Get("cacheWriteTokens")
	if cacheWriteField.Exists() {
		cacheWrite = int(cacheWriteField.Int())
		tokenUsage["cache_creation_input_tokens"] = cacheWrite
	}
	if len(tokenUsage) == 0 {
		return
	}

	msg.HasContextTokens = inputField.Exists() ||
		cacheReadField.Exists() ||
		cacheWriteField.Exists()
	msg.HasOutputTokens = outputField.Exists()
	msg.tokenPresenceKnown = true
	msg.ContextTokens = input + cacheRead + cacheWrite
	msg.OutputTokens = output

	tokenUsageJSON, err := json.Marshal(tokenUsage)
	if err == nil {
		msg.TokenUsage = tokenUsageJSON
	}
}

func positAssistantTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

func positAssistantProviderCapabilities() Capabilities {
	return Capabilities{
		Source: SourceCapabilities{
			DiscoverSources:      CapabilitySupported,
			StreamingDiscovery:   CapabilitySupported,
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
			SessionName:          CapabilitySupported,
			Cwd:                  CapabilitySupported,
			GitBranch:            CapabilitySupported,
			Relationships:        CapabilitySupported,
			Subagents:            CapabilitySupported,
			Thinking:             CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			PerMessageTokenUsage: CapabilitySupported,
			MalformedLineCount:   CapabilitySupported,
			Model:                CapabilitySupported,
		},
	}
}
