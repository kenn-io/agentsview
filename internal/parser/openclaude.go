package parser

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

var _ SourceSet = (*openClaudeSourceSet)(nil)

type openClaudeSource struct {
	Root string
	Path string
}

type openClaudeSourceSet struct {
	roots []string
}

func newOpenClaudeProviderFactory(def AgentDef) ProviderFactory {
	return NewSourceSetFactory(
		def,
		openClaudeProviderCapabilities(),
		func(cfg ProviderConfig) SourceSet {
			return newOpenClaudeSourceSet(cfg.Roots)
		},
	)
}

func openClaudeProviderCapabilities() Capabilities {
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
			ForceReplaceOnParse:  CapabilitySupported,
		},
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			SessionName:          CapabilitySupported,
			Cwd:                  CapabilitySupported,
			GitBranch:            CapabilitySupported,
			Relationships:        CapabilitySupported,
			Thinking:             CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			PerMessageTokenUsage: CapabilitySupported,
			TerminationStatus:    CapabilitySupported,
			MalformedLineCount:   CapabilitySupported,
			Model:                CapabilitySupported,
			StopReason:           CapabilitySupported,
		},
	}
}

func newOpenClaudeSourceSet(roots []string) openClaudeSourceSet {
	return openClaudeSourceSet{roots: cleanJSONLRoots(roots)}
}

func (s openClaudeSourceSet) Discover(ctx context.Context) ([]SourceRef, error) {
	var sources []SourceRef
	seen := make(map[string]struct{})
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, file := range ClaudeProjectSessionFiles(root) {
			source, ok := s.discoveredSourceRef(root, file)
			if !ok {
				continue
			}
			addJSONLSource(source, &sources, seen)
		}
	}
	sortJSONLSources(sources)
	return sources, nil
}

func (s openClaudeSourceSet) discoveredSourceRef(
	root string, file DiscoveredFile,
) (SourceRef, bool) {
	return s.sourceRef(root, file.Path)
}

func (s openClaudeSourceSet) WatchPlan(context.Context) (WatchPlan, error) {
	roots := make([]WatchRoot, 0, len(s.roots))
	for _, root := range s.roots {
		roots = append(roots, WatchRoot{
			Path:         root,
			Recursive:    true,
			IncludeGlobs: []string{"*.jsonl"},
			DebounceKey:  string(AgentOpenClaude) + ":projects:" + root,
		})
	}
	return WatchPlan{Roots: roots}, nil
}

func (s openClaudeSourceSet) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	allowMissing := jsonlMissingPathFallbackAllowed(req) ||
		claudeChangedPathPresentButUnstatable(req.Path)
	if req.WatchRoot != "" {
		root := filepath.Clean(req.WatchRoot)
		if !s.hasRoot(root) {
			return nil, nil
		}
		source, ok := s.sourceForChangedPath(root, req.Path, allowMissing)
		if !ok {
			return nil, nil
		}
		return []SourceRef{source}, nil
	}
	for _, root := range s.roots {
		source, ok := s.sourceForChangedPath(root, req.Path, allowMissing)
		if ok {
			return []SourceRef{source}, nil
		}
	}
	return nil, nil
}

func (s openClaudeSourceSet) FindSource(
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
			if source, ok := s.sourceForPath(root, path); ok {
				return source, true, nil
			}
		}
	}
	if req.RawSessionID == "" {
		return SourceRef{}, false, nil
	}
	for _, root := range s.roots {
		path := claudeFindSourceFile(root, req.RawSessionID)
		if path == "" {
			continue
		}
		if source, ok := s.sourceRef(root, path); ok {
			return source, true, nil
		}
	}
	return SourceRef{}, false, nil
}

func (s openClaudeSourceSet) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	if err := ctx.Err(); err != nil {
		return SourceFingerprint{}, err
	}
	path, ok := s.pathFromSource(source)
	if !ok {
		return SourceFingerprint{}, fmt.Errorf("openclaude source path unavailable")
	}
	info, err := os.Stat(path)
	if err != nil {
		return SourceFingerprint{}, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.IsDir() {
		return SourceFingerprint{}, fmt.Errorf("stat %s: source is a directory", path)
	}
	hash, err := hashJSONLSourceFile(path)
	if err != nil {
		return SourceFingerprint{}, err
	}
	inode, device := sourceFileIdentity(info)
	return SourceFingerprint{
		Key:     firstNonEmptyJSONLString(source.FingerprintKey, source.Key, path),
		Size:    info.Size(),
		MTimeNS: info.ModTime().UnixNano(),
		Inode:   inode,
		Device:  device,
		Hash:    hash,
	}, nil
}

func (s openClaudeSourceSet) Parse(
	ctx context.Context,
	req ParseRequest,
) (ParseOutcome, error) {
	if err := ctx.Err(); err != nil {
		return ParseOutcome{}, err
	}
	path, ok := s.pathFromSource(req.Source)
	if !ok {
		return ParseOutcome{}, fmt.Errorf("openclaude source path unavailable")
	}
	machine := firstNonEmptyJSONLString(req.Machine)
	project := GetProjectName(firstNonEmptyJSONLString(
		req.Source.ProjectHint,
		filepath.Base(filepath.Dir(path)),
	))
	sess, msgs, err := parseOpenClaudeSession(path, project, machine)
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
		ForceReplace:      true,
	}, nil
}

func (s openClaudeSourceSet) pathFromSource(source SourceRef) (string, bool) {
	switch src := source.Opaque.(type) {
	case openClaudeSource:
		return src.Path, src.Path != ""
	case *openClaudeSource:
		if src != nil && src.Path != "" {
			return src.Path, true
		}
	case MaterializedFileSource:
		return src.Path, src.Path != ""
	}
	for _, candidate := range []string{
		source.DisplayPath,
		source.FingerprintKey,
		source.Key,
	} {
		for _, root := range s.roots {
			if ref, ok := s.sourceForPath(root, candidate); ok {
				src := ref.Opaque.(openClaudeSource)
				return src.Path, true
			}
		}
	}
	return "", false
}

func (s openClaudeSourceSet) sourceForPath(root, path string) (SourceRef, bool) {
	return s.sourceForChangedPath(root, path, false)
}

func (s openClaudeSourceSet) sourceForChangedPath(
	root, path string, allowMissing bool,
) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if allowMissing {
		return s.sourceRefFromPath(root, path)
	}
	return s.sourceRef(root, path)
}

func (s openClaudeSourceSet) sourceRef(root, path string) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if !IsRegularFile(path) {
		return SourceRef{}, false
	}
	return s.sourceRefFromPath(root, path)
}

func (s openClaudeSourceSet) sourceRefFromPath(
	root, path string,
) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	project, ok := claudeProjectHintFromPath(root, path)
	if !ok {
		return SourceRef{}, false
	}
	return SourceRef{
		Provider:       AgentOpenClaude,
		Key:            path,
		DisplayPath:    path,
		FingerprintKey: path,
		ProjectHint:    project,
		Opaque: openClaudeSource{
			Root: root,
			Path: path,
		},
	}, true
}

func (s openClaudeSourceSet) hasRoot(root string) bool {
	for _, configured := range s.roots {
		if samePath(root, configured) {
			return true
		}
	}
	return false
}

func parseOpenClaudeSession(
	path, project, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("stat %s: %w", path, err)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	lr := newLineReader(f, maxLineSize)
	lastLine := ""
	malformedLines := 0
	ordinal := 0
	var (
		messages       []ParsedMessage
		queuedCommands []claudeQueuedCommand
		startedAt      time.Time
		endedAt        time.Time
		firstUser      string
		userCount      int
		sessionID      = strings.TrimSuffix(filepath.Base(path), ".jsonl")
		sessionName    string
		customTitle    string
		aiTitle        string
		cwd            string
		gitBranch      string
	)

	for {
		line, ok := lr.next()
		if !ok {
			break
		}
		lastLine = line
		if !gjson.Valid(line) {
			malformedLines++
			continue
		}

		switch strings.TrimSpace(gjson.Get(line, "type").Str) {
		case "attachment":
			if qc, ok := extractOpenClaudeQueuedCommand(line); ok {
				queuedCommands = append(queuedCommands, qc)
			}
			continue
		case "custom-title":
			if v := strings.TrimSpace(gjson.Get(line, "customTitle").Str); v != "" {
				customTitle = v
			}
			continue
		case "ai-title":
			if v := strings.TrimSpace(gjson.Get(line, "aiTitle").Str); v != "" {
				aiTitle = v
			}
			continue
		}

		if ts := extractTimestamp(line); !ts.IsZero() {
			if startedAt.IsZero() || ts.Before(startedAt) {
				startedAt = ts
			}
			if ts.After(endedAt) {
				endedAt = ts
			}
		}

		if sessionName == "" {
			if v := strings.TrimSpace(gjson.Get(line, "sessionName").Str); v != "" {
				sessionName = v
			}
		}
		if cwd == "" {
			if v := strings.TrimSpace(gjson.Get(line, "cwd").Str); v != "" {
				cwd = v
			}
		}
		if gitBranch == "" {
			if v := strings.TrimSpace(gjson.Get(line, "gitBranch").Str); v != "" {
				gitBranch = v
			}
		}

		if isOpenClaudeCompactBoundary(line) {
			content, _, _, _, _, _ := ExtractTextContent(
				gjson.Get(line, "message.content"),
			)
			messages = append(messages, ParsedMessage{
				Ordinal:           ordinal,
				Role:              RoleAssistant,
				Content:           content,
				Timestamp:         extractTimestamp(line),
				IsSystem:          true,
				ContentLength:     len(content),
				SourceType:        "system",
				SourceSubtype:     "compact_boundary",
				SourceUUID:        gjson.Get(line, "uuid").Str,
				SourceParentUUID:  gjson.Get(line, "parentUuid").Str,
				IsCompactBoundary: true,
				IsSidechain:       gjson.Get(line, "isSidechain").Bool(),
			})
			ordinal++
			continue
		}

		role := strings.TrimSpace(gjson.Get(line, "message.role").Str)
		if role == "" {
			role = strings.TrimSpace(gjson.Get(line, "type").Str)
		}
		switch role {
		case "user", "assistant", "system":
		default:
			continue
		}
		if role == "user" && gjson.Get(line, "isMeta").Bool() {
			continue
		}

		content := gjson.Get(line, "message.content")
		text, thinkingText, hasThinking, hasToolUse, toolCalls, toolResults :=
			ExtractTextContent(content)
		if strings.TrimSpace(text) == "" && len(toolResults) == 0 &&
			len(toolCalls) == 0 && role != "system" {
			continue
		}

		if role == "system" {
			messages = append(messages, ParsedMessage{
				Ordinal:          ordinal,
				Role:             RoleSystem,
				Content:          text,
				ThinkingText:     thinkingText,
				Timestamp:        extractTimestamp(line),
				HasThinking:      hasThinking,
				HasToolUse:       hasToolUse,
				IsSystem:         true,
				ContentLength:    len(text),
				ToolCalls:        toolCalls,
				ToolResults:      toolResults,
				SourceType:       "system",
				SourceSubtype:    strings.TrimSpace(gjson.Get(line, "subtype").Str),
				SourceUUID:       gjson.Get(line, "uuid").Str,
				SourceParentUUID: gjson.Get(line, "parentUuid").Str,
				IsSidechain:      gjson.Get(line, "isSidechain").Bool(),
			})
			ordinal++
			continue
		}

		if role == "user" && strings.TrimSpace(text) == "" &&
			len(toolResults) == 0 {
			continue
		}

		msg := ParsedMessage{
			Ordinal:            ordinal,
			Role:               RoleType(role),
			Content:            text,
			ThinkingText:       thinkingText,
			Timestamp:          extractTimestamp(line),
			HasThinking:        hasThinking,
			HasToolUse:         hasToolUse,
			ContentLength:      len(text),
			ToolCalls:          toolCalls,
			ToolResults:        toolResults,
			SourceType:         role,
			SourceUUID:         gjson.Get(line, "uuid").Str,
			SourceParentUUID:   gjson.Get(line, "parentUuid").Str,
			IsSidechain:        gjson.Get(line, "isSidechain").Bool(),
			tokenPresenceKnown: role == "assistant",
		}
		if role == "assistant" {
			extractOpenClaudeTokenFields(&msg, line)
			msg.StopReason = gjson.Get(line, "message.stop_reason").Str
		}
		messages = append(messages, msg)
		ordinal++
		if role == "user" && strings.TrimSpace(text) != "" {
			userCount++
			if firstUser == "" {
				firstUser = truncate(strings.ReplaceAll(text, "\n", " "), 300)
			}
		}
	}

	if err := lr.Err(); err != nil {
		return nil, nil, fmt.Errorf("reading %s: %w", path, err)
	}

	isTruncated := lastLine != "" &&
		strings.TrimSpace(lastLine) != "" &&
		!gjson.Valid(lastLine) &&
		!fileEndsWithNewline(f, info.Size())

	if len(messages) == 0 && len(queuedCommands) == 0 {
		return nil, nil, nil
	}

	if len(queuedCommands) > 0 {
		messages = mergeQueuedCommands(messages, queuedCommands, 0)
		firstUser, userCount = firstMessageAndUserCount(messages)
		for _, qc := range queuedCommands {
			if qc.timestamp.After(endedAt) {
				endedAt = qc.timestamp
			}
			if !qc.timestamp.IsZero() &&
				(startedAt.IsZero() || qc.timestamp.Before(startedAt)) {
				startedAt = qc.timestamp
			}
		}
	}

	if customTitle != "" {
		sessionName = customTitle
	} else if aiTitle != "" {
		sessionName = aiTitle
	}

	project = firstNonEmptyJSONLString(project, GetProjectName(filepath.Base(filepath.Dir(path))))
	if project == "" {
		project = "unknown"
	}

	parentSessionID, relationshipType := openClaudeRelationship(path, sessionID)
	sess := &ParsedSession{
		ID:               openClaudeSessionID(sessionID),
		Project:          project,
		Machine:          machine,
		Agent:            AgentOpenClaude,
		ParentSessionID:  parentSessionID,
		RelationshipType: relationshipType,
		Cwd:              cwd,
		GitBranch:        gitBranch,
		FirstMessage:     firstUser,
		SessionName:      sessionName,
		StartedAt:        startedAt,
		EndedAt:          endedAt,
		MessageCount:     len(messages),
		UserMessageCount: userCount,
		MalformedLines:   malformedLines,
		IsTruncated:      isTruncated,
		File: FileInfo{
			Path:  path,
			Size:  info.Size(),
			Mtime: info.ModTime().UnixNano(),
		},
	}
	accumulateMessageTokenUsage(sess, messages)
	sess.TerminationStatus = Classify(
		openClaudeSemanticMessages(messages),
		lastAssistantStopReason(openClaudeSemanticMessages(messages)),
		isTruncated,
	)
	return sess, messages, nil
}

func openClaudeRelationship(
	path, sessionID string,
) (string, RelationshipType) {
	parent := claudeCompanionParentSessionID(path, sessionID)
	if parent == "" {
		return "", RelNone
	}
	return openClaudeSessionID(parent), RelSubagent
}

func openClaudeSemanticMessages(messages []ParsedMessage) []ParsedMessage {
	filtered := make([]ParsedMessage, 0, len(messages))
	for _, msg := range messages {
		if msg.IsSystem {
			continue
		}
		filtered = append(filtered, msg)
	}
	return filtered
}

func extractOpenClaudeQueuedCommand(line string) (claudeQueuedCommand, bool) {
	attachment := gjson.Get(line, "attachment")
	if attachment.Get("type").Str != "queued_command" {
		return claudeQueuedCommand{}, false
	}
	if attachment.Get("commandMode").Str != "prompt" {
		return claudeQueuedCommand{}, false
	}
	if attachment.Get("isMeta").Bool() || attachment.Get("origin").Exists() {
		return claudeQueuedCommand{}, false
	}

	prompt, _, _, _, _, _ := ExtractTextContent(attachment.Get("prompt"))
	if strings.TrimSpace(prompt) == "" {
		return claudeQueuedCommand{}, false
	}

	return claudeQueuedCommand{
		prompt:    prompt,
		timestamp: extractTimestamp(line),
	}, true
}

func openClaudeSessionID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" || strings.HasPrefix(id, "openclaude:") {
		return id
	}
	return "openclaude:" + id
}

func isOpenClaudeCompactBoundary(line string) bool {
	if gjson.Get(line, "isCompactSummary").Bool() {
		return true
	}
	if gjson.Get(line, "compact_boundary").Bool() {
		return true
	}
	switch strings.TrimSpace(gjson.Get(line, "subtype").Str) {
	case "compact_boundary", "compact-summary", "compact_summary":
		return true
	}
	return strings.TrimSpace(gjson.Get(line, "type").Str) == "compact_boundary"
}

func extractOpenClaudeTokenFields(msg *ParsedMessage, line string) {
	msg.Model = gjson.Get(line, "message.model").String()

	usageResult := gjson.Get(line, "message.usage")
	if usageResult.Exists() {
		msg.TokenUsage = json.RawMessage(usageResult.Raw)
		msg.HasOutputTokens = usageResult.Get("output_tokens").Exists()
		msg.HasContextTokens = usageResult.Get("input_tokens").Exists() ||
			usageResult.Get("cache_creation_input_tokens").Exists() ||
			usageResult.Get("cache_read_input_tokens").Exists()

		input := int(usageResult.Get("input_tokens").Int())
		cacheCreation := int(usageResult.Get("cache_creation_input_tokens").Int())
		cacheRead := int(usageResult.Get("cache_read_input_tokens").Int())
		msg.OutputTokens = int(usageResult.Get("output_tokens").Int())
		msg.ContextTokens = input + cacheCreation + cacheRead
	}
}
