package parser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"go.kenn.io/agentsview/internal/money"
)

type junieSessionSummary struct {
	projectDir string
	taskName   string
	createdAt  time.Time
	updatedAt  time.Time
}

func (s junieIndexSummary) sessionSummary() junieSessionSummary {
	return junieSessionSummary{
		projectDir: s.ProjectDir,
		taskName:   s.TaskName,
		createdAt:  junieMillisTimestamp(s.CreatedAt),
		updatedAt:  junieMillisTimestamp(s.UpdatedAt),
	}
}

type junieTranscriptMessage struct {
	ParsedMessage
	active bool
}

type junieParserState struct {
	entries           []junieTranscriptMessage
	usageEvents       []ParsedUsageEvent
	userMessages      map[string]int
	assistantMessages map[string]int
	sourceSessionID   string
	startedAt         time.Time
	endedAt           time.Time
	sessionName       string
	sessionNameFound  bool
	malformedLines    int
}

type junieRootOpener func(string) (*os.Root, error)

func openValidatedJunieRoot(path string) (*os.Root, os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !info.IsDir() {
		return nil, nil, errors.New("junie root is not a directory")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, nil, err
	}
	openedInfo, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, nil, err
	}
	if !os.SameFile(info, openedInfo) {
		_ = root.Close()
		return nil, nil, errors.New("junie root changed while opening")
	}
	return root, openedInfo, nil
}

func openJunieEventStream(path string, openRoot junieRootOpener) (*os.File, error) {
	path = filepath.Clean(path)
	sessionDir := filepath.Dir(path)
	root, err := openRoot(filepath.Dir(sessionDir))
	if err != nil {
		return nil, err
	}
	defer root.Close()

	sessionName := filepath.Base(sessionDir)
	dirInfo, err := root.Lstat(sessionName)
	if err != nil {
		return nil, err
	}
	if !dirInfo.IsDir() {
		return nil, errors.New("session directory is not a directory")
	}

	relativePath := filepath.Join(sessionName, filepath.Base(path))
	info, err := root.Lstat(relativePath)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("session event stream is not a regular file")
	}

	f, err := openJuniePinnedFile(root, relativePath, info)
	if err != nil {
		return nil, fmt.Errorf("session event stream: %w", err)
	}
	return f, nil
}

func openJuniePinnedFile(root *os.Root, name string, expected os.FileInfo) (*os.File, error) {
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	openedInfo, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(expected, openedInfo) {
		_ = f.Close()
		return nil, errors.New("file changed while opening")
	}
	return f, nil
}

func parseJunieSessionWithSummary(
	ctx context.Context, path, machine string,
	summary junieSessionSummary, summaryPresent bool,
	openRoot junieRootOpener,
) (*ParsedSession, []ParsedMessage, bool, error) {
	f, err := openJunieEventStream(path, openRoot)
	if err != nil {
		return nil, nil, false, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, nil, false, fmt.Errorf("stat %s: %w", path, err)
	}

	state := junieParserState{
		userMessages:      make(map[string]int),
		assistantMessages: make(map[string]int),
		sourceSessionID:   filepath.Base(filepath.Dir(path)),
	}
	lr := newLineReaderContext(ctx, f, maxLineSize)
	defer releaseLineReader(lr)
	lastLineMalformed := false
	for lineNumber := 1; ; lineNumber++ {
		line, ok := lr.next()
		if !ok {
			break
		}
		if err := contextErrEvery(ctx, lineNumber); err != nil {
			return nil, nil, false, err
		}
		if !gjson.Valid(line) {
			state.malformedLines++
			lastLineMalformed = true
			continue
		}
		lastLineMalformed = false
		if err := state.consumeEvent(gjson.Parse(line), lineNumber); err != nil {
			return nil, nil, false, fmt.Errorf("parsing Junie session %s line %d: %w", path, lineNumber, err)
		}
	}
	if err := lr.Err(); err != nil {
		return nil, nil, false, fmt.Errorf("reading Junie session %s: %w", path, err)
	}
	sess, messages, err := state.session(
		ctx, path, machine, filepath.Base(filepath.Dir(path)),
		info.Size(), info.ModTime().UnixNano(), summary, summaryPresent,
	)
	return sess, messages, !lastLineMalformed, err
}

func (s *junieParserState) consumeEvent(event gjson.Result, lineNumber int) error {
	timestamp := junieTimestamp(event.Get("timestampMs"))
	if !timestamp.IsZero() {
		if s.startedAt.IsZero() {
			s.startedAt = timestamp
		}
		s.endedAt = timestamp
	}

	switch event.Get("kind").Str {
	case "UserPromptEvent":
		s.consumeUserPrompt(event, timestamp)
	case "UserResponseEvent":
		s.appendMessage(RoleUser, event.Get("prompt").Str, timestamp)
	case "UserAsyncResponseEvent":
		s.consumeAsyncResponse(event, timestamp)
	case "UserMessagesCommittedToHistory":
		s.setUserMessagesActive(event.Get("userMessageIds"), true)
	case "UserMessagesDroppedFromHistory", "UserMessagesFailedInHistory":
		s.setUserMessagesActive(event.Get("userMessageIds"), false)
	case "SessionTitleSetEvent":
		s.sessionName = event.Get("name").Str
		s.sessionNameFound = true
	case "SystemMessageEvent":
		s.consumeSystemMessage(event, timestamp)
	case "AgentTaskFailedEvent":
		s.appendMessage(RoleSystem, "Agent task failed", timestamp)
	case "SessionA2uxEvent":
		return s.consumeA2UXEvent(event, timestamp, lineNumber)
	}
	return nil
}

func (s *junieParserState) consumeUserPrompt(event gjson.Result, timestamp time.Time) {
	content := firstNonEmptyJSONLString(
		event.Get("presentablePrompt").Str,
		event.Get("prompt").Str,
	)
	requestID := event.Get("requestId").Str
	active := event.Get("delivery").Str != "Failed" && strings.TrimSpace(content) != ""
	if index, ok := s.userMessages[requestID]; requestID != "" && ok {
		s.entries[index].Content = content
		s.entries[index].ContentLength = len(content)
		s.entries[index].Timestamp = timestamp
		s.entries[index].active = active
		return
	}
	index := s.appendMessage(RoleUser, content, timestamp)
	s.entries[index].active = active
	if requestID != "" {
		s.userMessages[requestID] = index
	}
}

func (s *junieParserState) consumeAsyncResponse(event gjson.Result, timestamp time.Time) {
	var responses []string
	for _, entry := range event.Get("entries").Array() {
		response := strings.TrimSpace(strings.Join([]string{
			entry.Get("question").Str,
			entry.Get("answer").Str,
		}, "\n"))
		if response != "" {
			responses = append(responses, response)
		}
	}
	s.appendMessage(RoleUser, strings.Join(responses, "\n\n"), timestamp)
}

func (s *junieParserState) consumeSystemMessage(event gjson.Result, timestamp time.Time) {
	text := strings.TrimSpace(event.Get("text").Str)
	details := strings.TrimSpace(event.Get("details").Str)
	if details != "" && details != text {
		text = strings.TrimSpace(text + "\n\n" + details)
	}
	s.appendMessage(RoleSystem, text, timestamp)
}

func (s *junieParserState) consumeA2UXEvent(
	event gjson.Result, timestamp time.Time, lineNumber int,
) error {
	agentEvent := event.Get("event.agentEvent")
	var content string
	switch agentEvent.Get("kind").Str {
	case "LlmResponseMetadataEvent":
		return s.consumeModelUsage(agentEvent, timestamp, lineNumber)
	case "MarkdownBlockUpdatedEvent":
		content = strings.TrimSpace(agentEvent.Get("text").Str)
	case "ResultBlockUpdatedEvent":
		content = strings.TrimSpace(strings.TrimPrefix(
			agentEvent.Get("result").Str, "<!-- ANSWER -->",
		))
	default:
		return nil
	}
	stepID := agentEvent.Get("stepId").Str
	if index, ok := s.assistantMessages[stepID]; stepID != "" && ok {
		s.entries[index].Content = content
		s.entries[index].ContentLength = len(content)
		s.entries[index].Timestamp = timestamp
		s.entries[index].active = content != ""
		return nil
	}
	index := s.appendMessage(RoleAssistant, content, timestamp)
	if stepID != "" {
		s.assistantMessages[stepID] = index
	}
	return nil
}

func (s *junieParserState) consumeModelUsage(
	event gjson.Result, timestamp time.Time, lineNumber int,
) error {
	for index, usage := range event.Get("modelUsage").Array() {
		parsed := ParsedUsageEvent{
			SessionID:                "junie:" + s.sourceSessionID,
			Source:                   "llm-response",
			Model:                    strings.TrimSpace(usage.Get("model").Str),
			InputTokens:              max(int(usage.Get("inputTokens").Int()), 0),
			OutputTokens:             max(int(usage.Get("outputTokens").Int()), 0),
			CacheCreationInputTokens: max(int(usage.Get("cacheCreateTokens").Int()), 0),
			CacheReadInputTokens:     max(int(usage.Get("cacheInputTokens").Int()), 0),
			OccurredAt:               timeString(timestamp.UTC(), s.startedAt.UTC()),
			DedupKey: fmt.Sprintf(
				"junie:%s:llm-response:%d:%d", s.sourceSessionID, lineNumber, index,
			),
		}
		costValue := usage.Get("cost")
		if costValue.Exists() && costValue.Type != gjson.Null {
			if costValue.Type != gjson.Number || costValue.Num < 0 {
				return errors.New("invalid model usage cost")
			}
			cost, err := money.ParseDollars(costValue.Raw)
			if err != nil {
				return fmt.Errorf("parsing model usage cost: %w", err)
			}
			parsed.Cost = &cost
			parsed.CostStatus = "exact"
			parsed.CostSource = "junie-model-usage"
		}
		if parsed.InputTokens > 0 || parsed.OutputTokens > 0 ||
			parsed.CacheCreationInputTokens > 0 || parsed.CacheReadInputTokens > 0 ||
			parsed.Cost != nil {
			s.usageEvents = append(s.usageEvents, parsed)
		}
	}
	return nil
}

func (s *junieParserState) appendMessage(
	role RoleType, content string, timestamp time.Time,
) int {
	s.entries = append(s.entries, junieTranscriptMessage{
		Role:          role,
		Content:       content,
		Timestamp:     timestamp,
		IsSystem:      role == RoleSystem,
		ContentLength: len(content),
		active:        strings.TrimSpace(content) != "",
	})
	return len(s.entries) - 1
}

func (s *junieParserState) setUserMessagesActive(ids gjson.Result, active bool) {
	for _, id := range ids.Array() {
		if index, ok := s.userMessages[id.Str]; ok {
			s.entries[index].active = active
		}
	}
}

func (s *junieParserState) session(
	ctx context.Context, path, machine, sourceSessionID string,
	fileSize, mtime int64,
	summary junieSessionSummary, summaryPresent bool,
) (*ParsedSession, []ParsedMessage, error) {
	messages := make([]ParsedMessage, 0, len(s.entries))
	firstMessage := ""
	userCount := 0
	for _, entry := range s.entries {
		if !entry.active || strings.TrimSpace(entry.Content) == "" {
			continue
		}
		entry.Ordinal = len(messages)
		messages = append(messages, entry.ParsedMessage)
		if entry.Role == RoleUser {
			userCount++
			if firstMessage == "" {
				firstMessage = truncate(strings.ReplaceAll(entry.Content, "\n", " "), 300)
			}
		}
	}
	// Preserve recognized empty projections so force replacement can clear stale messages.
	// A wholly empty or unrecognized stream may be a partial rewrite and stays skipped.
	if len(messages) == 0 && len(s.entries) == 0 && len(s.usageEvents) == 0 && !s.sessionNameFound {
		return nil, nil, nil
	}

	if !summary.createdAt.IsZero() &&
		(s.startedAt.IsZero() || summary.createdAt.Before(s.startedAt)) {
		s.startedAt = summary.createdAt
	}
	if summary.updatedAt.After(s.endedAt) {
		s.endedAt = summary.updatedAt
	}
	if !s.sessionNameFound && summaryPresent {
		s.sessionName = summary.taskName
	}
	if firstMessage == "" && strings.TrimSpace(s.sessionName) != "" {
		firstMessage = s.sessionName
	}

	project := ExtractProjectFromCwdWithBranchContext(
		WithoutFilesystemProjectDiscovery(ctx), summary.projectDir, "",
	)
	if project == "" {
		project = "junie"
	}
	projectDir := strings.TrimSpace(summary.projectDir)
	cwd := projectDir
	if filepath.IsAbs(projectDir) {
		cwd = filepath.Clean(projectDir)
	} else if !strings.HasPrefix(projectDir, "/") && !looksLikeWindowsPath(projectDir) {
		cwd = ""
	}

	session := &ParsedSession{
		ID:                 "junie:" + sourceSessionID,
		SourceSessionID:    sourceSessionID,
		Project:            project,
		Machine:            machine,
		Agent:              AgentJunie,
		Cwd:                cwd,
		FirstMessage:       firstMessage,
		SessionName:        s.sessionName,
		SessionNamePresent: s.sessionNameFound || summaryPresent,
		StartedAt:          s.startedAt,
		EndedAt:            s.endedAt,
		MessageCount:       len(messages),
		UserMessageCount:   userCount,
		MalformedLines:     s.malformedLines,
		File: FileInfo{
			Path:  path,
			Size:  fileSize,
			Mtime: mtime,
		},
	}
	applyUsageEventTokenTotals(session, s.usageEvents)
	session.UsageEvents = s.usageEvents
	return session, messages, nil
}

func parseJunieSessionSummary(line string) junieSessionSummary {
	return junieSessionSummary{
		projectDir: gjson.Get(line, "projectDir").Str,
		taskName:   gjson.Get(line, "taskName").Str,
		createdAt:  junieTimestamp(gjson.Get(line, "createdAt")),
		updatedAt:  junieTimestamp(gjson.Get(line, "updatedAt")),
	}
}

func junieTimestamp(value gjson.Result) time.Time {
	if value.Type != gjson.Number || value.Int() <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(value.Int())
}

func junieMillisTimestamp(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}
