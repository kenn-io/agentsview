package parser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// Icodemate exposes two storage families under the single icodemate agent:
// the VSCode-extension plugin path (OpenCode storage on
// ~/.local/share/icodemate, owned by the OpenCode-format source set) and the
// terminal CLI path (~/.icodemate/cli/projects), whose per-session JSONL
// transcripts match Claude Code's project layout
// (<root>/<project>/<session>.jsonl). The CLI source set below enumerates and
// parses that Claude-format layout independently, then relabels every parsed
// session onto Icodemate's own agent and icodemate: ID prefix, mirroring how
// the OpenCode-format path relabels its sessions. Discovery, watch, changed
// path, find, fingerprint, and parsing all mirror claudeSourceSet, with the
// Icodemate agent label substituted throughout so the two families collide
// only where the format genuinely differs.

var _ SourceSet = (*icodemateCLISourceSet)(nil)

type icodemateCLISource struct {
	Root string
	Path string
}

type icodemateCLISourceSet struct {
	roots []string
}

func newIcodemateCLISourceSet(roots []string) *icodemateCLISourceSet {
	return &icodemateCLISourceSet{roots: cleanJSONLRoots(roots)}
}

func (s *icodemateCLISourceSet) Discover(ctx context.Context) ([]SourceRef, error) {
	var sources []SourceRef
	seen := make(map[string]struct{})
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, file := range IcodemateCLIProjectSessionFiles(root) {
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

// DiscoverEach implements StreamingDiscoverer.
func (s *icodemateCLISourceSet) DiscoverEach(
	ctx context.Context, yield func(SourceRef) error,
) error {
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return err
		}
		if strings.HasPrefix(root, "s3://") {
			for _, file := range IcodemateCLIProjectSessionFiles(root) {
				source, ok := s.discoveredSourceRef(root, file)
				if ok {
					if err := yield(source); err != nil {
						return err
					}
				}
			}
			continue
		}
		if err := s.streamLocalRoot(ctx, root, yield); err != nil {
			return err
		}
	}
	return nil
}

// streamLocalRoot enumerates one local IcodeMate CLI projects root, following
// symlinked project directories exactly as Discover's walk does so a symlink
// whose target cannot be resolved surfaces as an incomplete discovery instead
// of reading as absent (reconciliation would otherwise tombstone every session
// beneath the symlink).
func (s *icodemateCLISourceSet) streamLocalRoot(
	ctx context.Context, root string, yield func(SourceRef) error,
) error {
	var incomplete error
	err := streamDirectoryEntries(ctx, root, func(project os.DirEntry) error {
		isProjectDir, dirErr := streamingDirCandidateOrIncomplete(
			AgentIcodemate, "IcodeMate CLI project directory", project, root,
		)
		if dirErr != nil {
			incomplete = errors.Join(incomplete, dirErr)
			return nil
		}
		if !isProjectDir {
			return nil
		}
		projectRoot := filepath.Join(root, project.Name())
		err := streamDirectoryTreeRecursive(ctx, projectRoot, func(
			path string, entry os.DirEntry,
		) error {
			if !strings.HasSuffix(entry.Name(), ".jsonl") {
				return nil
			}
			source, ok := s.sourceRef(root, path)
			if !ok {
				return nil
			}
			return yield(source)
		})
		if err == nil {
			return nil
		}
		if _, ok := discoveryYieldCause(err); ok {
			return err
		}
		if ctx.Err() != nil {
			return err
		}
		incomplete = errors.Join(incomplete, err)
		return nil
	})
	if cause, ok := discoveryYieldCause(err); ok {
		return cause
	}
	return errors.Join(incomplete, err)
}

// discoveredSourceRef builds the SourceRef for one enumerated session file.
// Local files resolve through the regular file-backed source ref; s3://
// objects (enumerated by ClaudeProjectSessionFiles via discoverClaudeS3) carry
// their durable object metadata in the Opaque payload, because the
// IsRegularFile gate sourceRef applies to a local path would otherwise drop
// every remote object.
func (s *icodemateCLISourceSet) discoveredSourceRef(
	root string, file DiscoveredFile,
) (SourceRef, bool) {
	if strings.HasPrefix(file.Path, "s3://") {
		return s3SourceRefFromDiscoveredFile(root, file), true
	}
	return s.sourceRef(root, file.Path)
}

func (s *icodemateCLISourceSet) Parse(
	ctx context.Context,
	req ParseRequest,
) (ParseOutcome, error) {
	if err := ctx.Err(); err != nil {
		return ParseOutcome{}, err
	}
	path, ok := s.pathFromSource(req.Source)
	if !ok {
		return ParseOutcome{}, fmt.Errorf("icodemate cli source path unavailable")
	}
	machine := firstNonEmptyJSONLString(req.Machine)
	project := GetProjectName(firstNonEmptyJSONLString(
		req.Source.ProjectHint,
		filepath.Base(filepath.Dir(path)),
	))
	sess, msgs, err := parseIcodemateCLISession(path, project, machine)
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

func (s *icodemateCLISourceSet) WatchPlan(context.Context) (WatchPlan, error) {
	roots := make([]WatchRoot, 0, len(s.roots))
	for _, root := range s.roots {
		roots = append(roots, WatchRoot{
			Path:         root,
			Recursive:    true,
			IncludeGlobs: []string{"*.jsonl"},
			DebounceKey:  string(AgentIcodemate) + ":cli-projects:" + root,
		})
	}
	return WatchPlan{Roots: roots}, nil
}

func (s *icodemateCLISourceSet) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Mirror the legacy classifier: treat a stat failure as absent only when
	// the path is known missing; otherwise fall back to path-shape
	// classification so a transient unreadable parent does not drop the change.
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

func (s *icodemateCLISourceSet) FindSource(
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

func (s *icodemateCLISourceSet) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	if err := ctx.Err(); err != nil {
		return SourceFingerprint{}, err
	}
	path, ok := s.pathFromSource(source)
	if !ok {
		return SourceFingerprint{}, fmt.Errorf("icodemate cli source path unavailable")
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

func (s *icodemateCLISourceSet) pathFromSource(source SourceRef) (string, bool) {
	switch src := source.Opaque.(type) {
	case icodemateCLISource:
		return src.Path, src.Path != ""
	case *icodemateCLISource:
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
				src := ref.Opaque.(icodemateCLISource)
				return src.Path, true
			}
		}
	}
	return "", false
}

func (s *icodemateCLISourceSet) sourceForPath(root, path string) (SourceRef, bool) {
	return s.sourceForChangedPath(root, path, false)
}

func (s *icodemateCLISourceSet) sourceForChangedPath(
	root, path string, allowMissing bool,
) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if allowMissing {
		return s.sourceRefFromPath(root, path)
	}
	return s.sourceRef(root, path)
}

func (s *icodemateCLISourceSet) sourceRef(root, path string) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if !IsRegularFile(path) {
		return SourceRef{}, false
	}
	return s.sourceRefFromPath(root, path)
}

func (s *icodemateCLISourceSet) sourceRefFromPath(
	root, path string,
) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	project, ok := claudeProjectHintFromPath(root, path)
	if !ok {
		return SourceRef{}, false
	}
	return SourceRef{
		Provider:       AgentIcodemate,
		Key:            path,
		DisplayPath:    path,
		FingerprintKey: path,
		ProjectHint:    project,
		Opaque: icodemateCLISource{
			Root: root,
			Path: path,
		},
	}, true
}

func (s *icodemateCLISourceSet) hasRoot(root string) bool {
	for _, configured := range s.roots {
		if samePath(root, configured) {
			return true
		}
	}
	return false
}

// parseIcodemateCLISession parses one IcodeMate CLI JSONL transcript
// (<root>/<project>/<session>.jsonl). The transcript schema matches Claude
// Code's, so the reader reuses the shared Claude JSONL helpers; the comment
// above and the relabels below keep it an Icodemate-owned parser rather than
// the Claude-source-set one, so the two families stay independent.
func parseIcodemateCLISession(
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
	defer releaseLineReader(lr)
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
			if qc, ok := extractIcodemateCLIQueuedCommand(line); ok {
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

		if isIcodemateCLICompactBoundary(line) {
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
			extractIcodemateCLITokenFields(&msg, line)
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
		messages = mergeQueuedCommands(
			messages, queuedCommands, 0, icodemateCLIQueuedCommandMessage,
		)
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

	project = firstNonEmptyJSONLString(
		project, GetProjectName(filepath.Base(filepath.Dir(path))),
	)
	if project == "" {
		project = "unknown"
	}

	parentSessionID, relationshipType := icodemateCLIRelationship(path, sessionID)
	sess := &ParsedSession{
		ID:               icodemateCLISessionID(sessionID),
		Project:          project,
		Machine:          machine,
		Agent:            AgentIcodemate,
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
		messages,
		lastAssistantStopReason(icodemateCLISemanticMessages(messages)),
		isTruncated,
	)
	return sess, messages, nil
}

func icodemateCLIRelationship(
	path, sessionID string,
) (string, RelationshipType) {
	parent := claudeCompanionParentSessionID(path, sessionID)
	if parent == "" {
		return "", RelNone
	}
	return icodemateCLISessionID(parent), RelSubagent
}

func icodemateCLISemanticMessages(messages []ParsedMessage) []ParsedMessage {
	filtered := make([]ParsedMessage, 0, len(messages))
	for _, msg := range messages {
		if msg.IsSystem {
			continue
		}
		filtered = append(filtered, msg)
	}
	return filtered
}

func extractIcodemateCLIQueuedCommand(line string) (claudeQueuedCommand, bool) {
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

func icodemateCLIQueuedCommandMessage(q claudeQueuedCommand) ParsedMessage {
	return ParsedMessage{
		Role:          RoleUser,
		Content:       q.prompt,
		Timestamp:     q.timestamp,
		ContentLength: len(q.prompt),
		SourceType:    "user",
		SourceSubtype: "queued_command",
	}
}

func icodemateCLISessionID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" || strings.HasPrefix(id, "icodemate:") {
		return id
	}
	return "icodemate:" + id
}

func isIcodemateCLICompactBoundary(line string) bool {
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

func extractIcodemateCLITokenFields(msg *ParsedMessage, line string) {
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
