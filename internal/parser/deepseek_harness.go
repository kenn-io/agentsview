package parser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"
)

type deepSeekHarnessTurnStep struct {
	Turn int64
	Step int64
}

type deepSeekHarnessUsage struct {
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	ReasoningTokens  int
}

type deepSeekHarnessBlockState struct {
	BlockType      string
	Text           strings.Builder
	Arguments      strings.Builder
	ToolCallID     string
	ToolName       string
	CompletedBlock json.RawMessage
}

type deepSeekHarnessResponse struct {
	Key            deepSeekHarnessTurnStep
	FirstChunkSeq  int64
	FirstChunkTime int64
	HasChunk       bool
	Blocks         map[int64]*deepSeekHarnessBlockState
	FinalSeen      bool
	FinalUsage     *deepSeekHarnessUsage
	ChunkUsage     *deepSeekHarnessUsage
	UsageTime      int64
	Model          string
	RequestModel   string
	StopReason     string
	CandidateID    int
}

type deepSeekHarnessUsageRecord struct {
	Seq   int64
	Time  int64
	Model string
	Usage deepSeekHarnessUsage
}

type deepSeekHarnessLifecycle struct {
	OpenTurn     int64
	OpenStep     int64
	NextTurn     int64
	NextStep     int64
	HasOpenTurn  bool
	HasOpenStep  bool
	PendingCalls map[string]struct{}
}

type deepSeekHarnessCandidate struct {
	ID      int
	Seq     int64
	Message ParsedMessage
}

func parseDeepSeekHarnessSession(
	ctx context.Context, path, machine string,
) (ParseResult, error) {
	info, err := os.Stat(path)
	if err != nil {
		return ParseResult{}, err
	}

	responses := make(map[deepSeekHarnessTurnStep]*deepSeekHarnessResponse)
	compactionUsage := make([]deepSeekHarnessUsageRecord, 0)
	candidates := make([]deepSeekHarnessCandidate, 0)
	nextCandidateID := 1
	var currentStep *deepSeekHarnessTurnStep
	latestRequestModel := ""
	latestTitle := ""
	latestAgentPreset := ""
	latestOwnedTime := int64(0)
	headerSeen := false
	lastTurnEndKind := ""
	lifecycle := deepSeekHarnessLifecycle{
		NextTurn: 1, NextStep: 1, PendingCalls: make(map[string]struct{}),
	}

	responseFor := func(key deepSeekHarnessTurnStep) *deepSeekHarnessResponse {
		response := responses[key]
		if response == nil {
			response = &deepSeekHarnessResponse{
				Key:         key,
				Blocks:      make(map[int64]*deepSeekHarnessBlockState),
				CandidateID: -1,
			}
			responses[key] = response
		}
		return response
	}
	appendCandidate := func(seq int64, message ParsedMessage) int {
		id := nextCandidateID
		nextCandidateID++
		candidates = append(candidates, deepSeekHarnessCandidate{
			ID: id, Seq: seq, Message: message,
		})
		return id
	}

	consumeEvent := func(
		header deepSeekHarnessHeader, event deepSeekHarnessEvent,
	) (consumeErr error) {
		if !headerSeen {
			latestOwnedTime = header.CreatedAt
			latestAgentPreset = header.AgentPreset
			headerSeen = true
		}
		if err := lifecycle.validate(event); err != nil {
			return eventError(event, err)
		}
		if event.Type == "session/title" {
			title, err := deepSeekHarnessTitle(event.Data)
			if err != nil {
				return eventError(event, err)
			}
			latestTitle = title
		}
		if event.Type == "agent-preset/selected" {
			preset, err := deepSeekHarnessAgentPreset(event.Data)
			if err != nil {
				return eventError(event, err)
			}
			latestAgentPreset = preset
		}
		if event.Type == "request/header" {
			model, err := deepSeekHarnessRequestModel(event.Data)
			if err != nil {
				return eventError(event, err)
			}
			latestRequestModel = model
		}
		ownedFrom := int64(0)
		if header.HasSeedLength {
			ownedFrom = header.SeedLength
		}
		if event.Seq < ownedFrom {
			if err := validateDeepSeekHarnessSemanticEvent(event); err != nil {
				return eventError(event, err)
			}
			return nil
		}
		defer func() {
			if consumeErr == nil && event.Time > latestOwnedTime {
				latestOwnedTime = event.Time
			}
		}()
		switch event.Type {
		case "turn/start":
			if _, err := deepSeekHarnessEventTurn(event.Data); err != nil {
				return eventError(event, err)
			}
			lastTurnEndKind = ""
		case "turn/end":
			kind, err := deepSeekHarnessTurnEndKind(event.Data)
			if err != nil {
				return eventError(event, err)
			}
			lastTurnEndKind = kind
			currentStep = nil
		case "step/start":
			key, err := deepSeekHarnessEventTurnStep(event.Data)
			if err != nil {
				return eventError(event, err)
			}
			currentStep = &key
			responseFor(key).RequestModel = latestRequestModel
		case "step/end":
			if _, err := deepSeekHarnessEventTurnStep(event.Data); err != nil {
				return eventError(event, err)
			}
			currentStep = nil
		case "request/header":
			if currentStep != nil {
				responseFor(*currentStep).RequestModel = latestRequestModel
			}
		case "user/message":
			message, err := deepSeekHarnessUserMessage(event)
			if err != nil {
				return eventError(event, err)
			}
			if !deepSeekHarnessSurfaceAppend(event.SurfaceOp) {
				return nil
			}
			appendCandidate(event.Seq, message)
		case "assistant/chunk":
			key, chunk, err := deepSeekHarnessChunkData(event.Data)
			if err != nil {
				return eventError(event, err)
			}
			response := responseFor(key)
			if response.RequestModel == "" {
				response.RequestModel = latestRequestModel
			}
			if err := applyDeepSeekHarnessChunk(response, chunk, event.Time); err != nil {
				return eventError(event, err)
			}
			if !response.HasChunk {
				response.FirstChunkSeq = event.Seq
				response.FirstChunkTime = event.Time
				response.HasChunk = true
			}
		case "assistant/message":
			key, rawMessage, usage, err := deepSeekHarnessAssistantData(event.Data)
			if err != nil {
				return eventError(event, err)
			}
			message, model, err := deepSeekHarnessAssistantMessage(rawMessage, event.Time)
			if err != nil {
				return eventError(event, err)
			}
			if !deepSeekHarnessSurfaceAppend(event.SurfaceOp) {
				return nil
			}
			response := responseFor(key)
			if response.RequestModel == "" {
				response.RequestModel = latestRequestModel
			}
			response.FinalSeen = true
			if usage != nil {
				response.FinalUsage = usage
				response.UsageTime = event.Time
			}
			if model != "" {
				response.Model = model
			}
			if !deepSeekHarnessMessageVisible(message) {
				return nil
			}
			message.StopReason = response.StopReason
			response.CandidateID = appendCandidate(event.Seq, message)
		case "tool/result":
			message, err := deepSeekHarnessToolResultMessage(event)
			if err != nil {
				return eventError(event, err)
			}
			if !deepSeekHarnessSurfaceAppend(event.SurfaceOp) {
				return nil
			}
			appendCandidate(event.Seq, message)
		case "compaction/summary":
			model, usage, err := deepSeekHarnessCompactionUsage(event.Data)
			if err != nil {
				return eventError(event, err)
			}
			if usage != nil {
				compactionUsage = append(compactionUsage, deepSeekHarnessUsageRecord{
					Seq: event.Seq, Time: event.Time, Model: model, Usage: *usage,
				})
			}
		}
		return nil
	}

	scan, err := scanDeepSeekHarnessLog(ctx, path, consumeEvent)
	if err != nil {
		return ParseResult{}, err
	}
	if !headerSeen {
		latestOwnedTime = scan.Header.CreatedAt
		latestAgentPreset = scan.Header.AgentPreset
	}
	if scan.Header.HasSeedLength && scan.Header.SeedLength > scan.EventCount {
		return ParseResult{}, fmt.Errorf(
			"DeepSeek Harness seedLength %d exceeds decoded event count %d",
			scan.Header.SeedLength, scan.EventCount,
		)
	}

	for _, response := range responses {
		if response.FinalSeen || !response.HasChunk {
			continue
		}
		message, err := buildDeepSeekHarnessPartialMessage(response)
		if err != nil {
			return ParseResult{}, err
		}
		if !deepSeekHarnessMessageVisible(message) {
			continue
		}
		response.CandidateID = appendCandidate(response.FirstChunkSeq, message)
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].Seq < candidates[j].Seq
	})
	messages := make([]ParsedMessage, 0, len(candidates))
	candidateOrdinals := make(map[int]int, len(candidates))
	for _, candidate := range candidates {
		message := candidate.Message
		message.Ordinal = len(messages)
		candidateOrdinals[candidate.ID] = message.Ordinal
		messages = append(messages, message)
	}

	fullSessionID := deepSeekHarnessCanonicalSessionID(scan.Header.ID)
	usageEvents := make([]ParsedUsageEvent, 0, len(responses)+len(compactionUsage))
	for _, response := range responses {
		if value, ok := candidateOrdinals[response.CandidateID]; ok {
			if messages[value].Model == "" {
				messages[value].Model = firstNonEmptyJSONLString(
					response.Model, response.RequestModel,
				)
			}
			if messages[value].StopReason == "" {
				messages[value].StopReason = response.StopReason
			}
		}
		usage := response.FinalUsage
		if usage == nil {
			usage = response.ChunkUsage
		}
		if usage == nil {
			continue
		}
		model := response.Model
		if model == "" {
			model = response.RequestModel
		}
		var ordinal *int
		if value, ok := candidateOrdinals[response.CandidateID]; ok {
			messages[value] = deepSeekHarnessApplyUsage(messages[value], usage, model)
			ordinalValue := value
			ordinal = &ordinalValue
		}
		occurredAt := time.UnixMilli(response.UsageTime).UTC()
		usageEvents = append(usageEvents, ParsedUsageEvent{
			SessionID:                fullSessionID,
			MessageOrdinal:           ordinal,
			Source:                   "deepseek-harness",
			Model:                    model,
			InputTokens:              usage.InputTokens,
			OutputTokens:             usage.OutputTokens,
			CacheCreationInputTokens: usage.CacheWriteTokens,
			CacheReadInputTokens:     usage.CacheReadTokens,
			ReasoningTokens:          usage.ReasoningTokens,
			OccurredAt:               occurredAt.Format(time.RFC3339Nano),
			DedupKey: fmt.Sprintf(
				"session:%s|response:%d:%d",
				fullSessionID, response.Key.Turn, response.Key.Step,
			),
		})
	}
	for _, record := range compactionUsage {
		usage := record.Usage
		usageEvents = append(usageEvents, ParsedUsageEvent{
			SessionID:                fullSessionID,
			Source:                   "deepseek-harness",
			Model:                    record.Model,
			InputTokens:              usage.InputTokens,
			OutputTokens:             usage.OutputTokens,
			CacheCreationInputTokens: usage.CacheWriteTokens,
			CacheReadInputTokens:     usage.CacheReadTokens,
			ReasoningTokens:          usage.ReasoningTokens,
			OccurredAt:               time.UnixMilli(record.Time).UTC().Format(time.RFC3339Nano),
			DedupKey: fmt.Sprintf(
				"session:%s|compaction:%d", fullSessionID, record.Seq,
			),
		})
	}
	sort.Slice(usageEvents, func(i, j int) bool {
		return usageEvents[i].DedupKey < usageEvents[j].DedupKey
	})

	firstMessage, userCount := firstMessageAndUserCount(messages)
	project := ExtractProjectFromCwd(scan.Header.Cwd)
	if project == "" {
		project = "deepseek_harness"
	}
	inode, device := sourceFileIdentity(info)
	session := ParsedSession{
		ID:                  fullSessionID,
		Project:             project,
		Machine:             machine,
		Agent:               AgentDeepSeekHarness,
		AgentLabel:          latestAgentPreset,
		Cwd:                 scan.Header.Cwd,
		SourceSessionID:     scan.Header.ID,
		SourceVersion:       "0",
		MalformedLines:      scan.MalformedLines,
		IsTruncated:         scan.Truncated,
		FirstMessage:        firstMessage,
		SessionName:         latestTitle,
		StartedAt:           time.UnixMilli(scan.Header.CreatedAt),
		EndedAt:             time.UnixMilli(latestOwnedTime),
		MessageCount:        len(messages),
		UserMessageCount:    userCount,
		CountsAuthoritative: true,
		File: FileInfo{
			Path: path, Size: info.Size(), Mtime: info.ModTime().UnixNano(),
			Inode: int64(inode), Device: int64(device),
		},
		UsageEvents: usageEvents,
	}
	if scan.Header.ParentSession != "" {
		session.ParentSessionID = deepSeekHarnessCanonicalSessionID(
			scan.Header.ParentSession,
		)
		if scan.Header.Origin == "subagent" {
			session.RelationshipType = RelSubagent
		} else {
			session.RelationshipType = RelFork
		}
	}
	switch {
	case scan.Truncated:
		session.TerminationStatus = TerminationTruncated
	case lifecycle.HasOpenTurn:
		if hasOrphanedToolCall(messages) {
			session.TerminationStatus = TerminationToolCallPending
		}
	case lastTurnEndKind == "completed":
		session.TerminationStatus = TerminationAwaitingUser
	case lastTurnEndKind != "":
		session.TerminationStatus = TerminationClean
	}
	accumulateMessageTokenUsage(&session, messages)
	applyUsageEventTokenTotals(&session, usageEvents)
	return ParseResult{
		Session: session, Messages: messages, UsageEvents: usageEvents,
	}, nil
}

func validateDeepSeekHarnessSemanticEvent(event deepSeekHarnessEvent) error {
	switch event.Type {
	case "agent-preset/selected":
		_, err := deepSeekHarnessAgentPreset(event.Data)
		return err
	case "turn/start":
		_, err := deepSeekHarnessEventTurn(event.Data)
		return err
	case "turn/end":
		_, err := deepSeekHarnessTurnEndKind(event.Data)
		return err
	case "step/start", "step/end":
		_, err := deepSeekHarnessEventTurnStep(event.Data)
		return err
	case "request/header":
		_, err := deepSeekHarnessRequestModel(event.Data)
		return err
	case "session/title":
		_, err := deepSeekHarnessTitle(event.Data)
		return err
	case "user/message":
		_, err := deepSeekHarnessUserMessage(event)
		return err
	case "assistant/chunk":
		_, chunk, err := deepSeekHarnessChunkData(event.Data)
		if err != nil {
			return err
		}
		response := &deepSeekHarnessResponse{
			Blocks: make(map[int64]*deepSeekHarnessBlockState),
		}
		return applyDeepSeekHarnessChunk(response, chunk, event.Time)
	case "assistant/message":
		_, message, _, err := deepSeekHarnessAssistantData(event.Data)
		if err != nil {
			return err
		}
		_, _, err = deepSeekHarnessAssistantMessage(message, event.Time)
		return err
	case "tool/call":
		_, _, err := deepSeekHarnessToolCallData(event.Data)
		return err
	case "tool/result":
		_, err := deepSeekHarnessToolResultMessage(event)
		return err
	case "compaction/summary":
		_, _, err := deepSeekHarnessCompactionUsage(event.Data)
		return err
	default:
		return nil
	}
}

func (state *deepSeekHarnessLifecycle) validate(event deepSeekHarnessEvent) error {
	requireOpenStep := func(key deepSeekHarnessTurnStep) error {
		if !state.HasOpenTurn || !state.HasOpenStep ||
			state.OpenTurn != key.Turn || state.OpenStep != key.Step {
			return fmt.Errorf(
				"%s names turn %d/step %d but open is turn %s/step %s",
				event.Type, key.Turn, key.Step,
				deepSeekHarnessOpenNumber(state.OpenTurn, state.HasOpenTurn),
				deepSeekHarnessOpenNumber(state.OpenStep, state.HasOpenStep),
			)
		}
		return nil
	}

	switch event.Type {
	case "turn/start":
		turn, err := deepSeekHarnessEventTurn(event.Data)
		if err != nil {
			return err
		}
		if state.HasOpenTurn {
			return fmt.Errorf("turn/start %d while turn %d is still open", turn, state.OpenTurn)
		}
		if turn != state.NextTurn {
			return fmt.Errorf("turn/start expected turn %d, got %d", state.NextTurn, turn)
		}
		state.OpenTurn = turn
		state.HasOpenTurn = true
		state.NextStep = 1
	case "turn/end":
		fields, err := decodeDeepSeekHarnessObject(event.Data)
		if err != nil {
			return errors.New("turn end data is not an object")
		}
		turn, err := deepSeekHarnessRequiredSafeInt(fields, "turn", true)
		if err != nil {
			return err
		}
		if _, err := deepSeekHarnessTurnEndKind(event.Data); err != nil {
			return err
		}
		if !state.HasOpenTurn || state.OpenTurn != turn {
			return fmt.Errorf(
				"turn/end %d does not match open turn %s",
				turn, deepSeekHarnessOpenNumber(state.OpenTurn, state.HasOpenTurn),
			)
		}
		if state.HasOpenStep {
			return fmt.Errorf("turn/end %d while step %d is still open", turn, state.OpenStep)
		}
		state.HasOpenTurn = false
		state.NextTurn++
	case "step/start":
		key, err := deepSeekHarnessEventTurnStep(event.Data)
		if err != nil {
			return err
		}
		if !state.HasOpenTurn || state.OpenTurn != key.Turn {
			return fmt.Errorf(
				"step/start in turn %d but open turn is %s",
				key.Turn, deepSeekHarnessOpenNumber(state.OpenTurn, state.HasOpenTurn),
			)
		}
		if state.HasOpenStep {
			return fmt.Errorf("step/start %d while step %d is still open", key.Step, state.OpenStep)
		}
		if key.Step != state.NextStep {
			return fmt.Errorf(
				"step/start expected step %d in turn %d, got %d",
				state.NextStep, key.Turn, key.Step,
			)
		}
		state.OpenStep = key.Step
		state.HasOpenStep = true
	case "step/end":
		key, err := deepSeekHarnessEventTurnStep(event.Data)
		if err != nil {
			return err
		}
		if err := requireOpenStep(key); err != nil {
			return err
		}
		clear(state.PendingCalls)
		state.HasOpenStep = false
		state.NextStep++
	case "assistant/chunk", "assistant/message":
		key, err := deepSeekHarnessEventTurnStep(event.Data)
		if err != nil {
			return err
		}
		return requireOpenStep(key)
	case "tool/call":
		key, callID, err := deepSeekHarnessToolCallData(event.Data)
		if err != nil {
			return err
		}
		if err := requireOpenStep(key); err != nil {
			return err
		}
		state.PendingCalls[callID] = struct{}{}
	case "tool/result":
		key, message, isError, errorCode, err := deepSeekHarnessToolResultData(event)
		if err != nil {
			return err
		}
		if deepSeekHarnessSurfaceAppend(event.SurfaceOp) {
			if err := requireOpenStep(key); err != nil {
				return err
			}
			callID := message.ToolResults[0].ToolUseID
			_, pending := state.PendingCalls[callID]
			if !pending && (!isError || errorCode != "TOOL_NOT_STARTED") {
				return fmt.Errorf("tool/result for %s with no prior tool/call in this step", callID)
			}
			delete(state.PendingCalls, callID)
			return nil
		}
		if !state.HasOpenTurn {
			return errors.New("tool/result surface replacement appended outside any open turn")
		}
	case "request/header", "request/context", "todo/write":
		if !state.HasOpenTurn {
			return fmt.Errorf("%s appended outside any open turn", event.Type)
		}
	}
	return nil
}

func deepSeekHarnessOpenNumber(value int64, present bool) string {
	if !present {
		return "none"
	}
	return fmt.Sprintf("%d", value)
}

func eventError(event deepSeekHarnessEvent, err error) error {
	return fmt.Errorf("DeepSeek Harness event %d (%s): %w", event.Seq, event.Type, err)
}

func deepSeekHarnessSurfaceAppend(raw json.RawMessage) bool {
	return string(raw) == `"append"`
}

func deepSeekHarnessTitle(raw json.RawMessage) (string, error) {
	fields, err := decodeDeepSeekHarnessObject(raw)
	if err != nil {
		return "", errors.New("session title data is not an object")
	}
	title, err := deepSeekHarnessRequiredString(fields, "title")
	if err != nil || strings.TrimSpace(title) == "" {
		return "", errors.New("session title is invalid")
	}
	return title, nil
}

func deepSeekHarnessAgentPreset(raw json.RawMessage) (string, error) {
	fields, err := decodeDeepSeekHarnessObject(raw)
	if err != nil {
		return "", errors.New("agent preset selection data is not an object")
	}
	preset, err := deepSeekHarnessRequiredString(fields, "agentPreset")
	if err != nil {
		return "", errors.New("agent preset selection is invalid")
	}
	return preset, nil
}

func deepSeekHarnessEventTurn(raw json.RawMessage) (int64, error) {
	fields, err := decodeDeepSeekHarnessObject(raw)
	if err != nil {
		return 0, errors.New("turn data is not an object")
	}
	return deepSeekHarnessRequiredSafeInt(fields, "turn", true)
}

func deepSeekHarnessEventTurnStep(raw json.RawMessage) (deepSeekHarnessTurnStep, error) {
	fields, err := decodeDeepSeekHarnessObject(raw)
	if err != nil {
		return deepSeekHarnessTurnStep{}, errors.New("step data is not an object")
	}
	turn, err := deepSeekHarnessRequiredSafeInt(fields, "turn", true)
	if err != nil {
		return deepSeekHarnessTurnStep{}, err
	}
	step, err := deepSeekHarnessRequiredSafeInt(fields, "step", true)
	if err != nil {
		return deepSeekHarnessTurnStep{}, err
	}
	return deepSeekHarnessTurnStep{Turn: turn, Step: step}, nil
}

func deepSeekHarnessToolCallData(
	raw json.RawMessage,
) (deepSeekHarnessTurnStep, string, error) {
	fields, err := decodeDeepSeekHarnessObject(raw)
	if err != nil {
		return deepSeekHarnessTurnStep{}, "", errors.New("tool call data is not an object")
	}
	key, err := deepSeekHarnessEventTurnStep(raw)
	if err != nil {
		return deepSeekHarnessTurnStep{}, "", err
	}
	callID, callIDErr := deepSeekHarnessRequiredString(fields, "callId")
	name, nameErr := deepSeekHarnessRequiredString(fields, "name")
	_, argumentsErr := deepSeekHarnessRequiredString(fields, "arguments")
	if callIDErr != nil || nameErr != nil || argumentsErr != nil || callID == "" || name == "" {
		return deepSeekHarnessTurnStep{}, "", errors.New("tool call data is invalid")
	}
	return key, callID, nil
}

func deepSeekHarnessTurnEndKind(raw json.RawMessage) (string, error) {
	fields, err := decodeDeepSeekHarnessObject(raw)
	if err != nil {
		return "", errors.New("turn end data is not an object")
	}
	if _, err := deepSeekHarnessRequiredSafeInt(fields, "turn", true); err != nil {
		return "", err
	}
	reason, err := decodeDeepSeekHarnessObject(fields["reason"])
	if err != nil {
		return "", errors.New("turn end reason is not an object")
	}
	kind, err := deepSeekHarnessRequiredString(reason, "kind")
	if err != nil {
		return "", err
	}
	if kind == "" {
		return "", errors.New("turn end reason has empty kind")
	}
	return kind, nil
}

func deepSeekHarnessRequestModel(raw json.RawMessage) (string, error) {
	fields, err := decodeDeepSeekHarnessObject(raw)
	if err != nil {
		return "", errors.New("request header data is not an object")
	}
	header, err := decodeDeepSeekHarnessObject(fields["header"])
	if err != nil {
		return "", errors.New("request header snapshot is not an object")
	}
	config, err := decodeDeepSeekHarnessObject(header["config"])
	if err != nil {
		return "", errors.New("request config is not an object")
	}
	model, err := deepSeekHarnessRequiredString(config, "model")
	if err != nil || model == "" {
		return "", errors.New("request config has invalid model")
	}
	if provider, err := deepSeekHarnessRequiredString(config, "provider"); err != nil || provider == "" {
		return "", errors.New("request config has invalid provider")
	}
	return model, nil
}

func deepSeekHarnessChunkData(raw json.RawMessage) (
	deepSeekHarnessTurnStep, json.RawMessage, error,
) {
	fields, err := decodeDeepSeekHarnessObject(raw)
	if err != nil {
		return deepSeekHarnessTurnStep{}, nil, errors.New("chunk data is not an object")
	}
	key, err := deepSeekHarnessEventTurnStep(raw)
	if err != nil {
		return deepSeekHarnessTurnStep{}, nil, err
	}
	chunk, ok := fields["chunk"]
	if !ok {
		return deepSeekHarnessTurnStep{}, nil, errors.New("chunk data has no chunk")
	}
	return key, chunk, nil
}

func deepSeekHarnessAssistantData(raw json.RawMessage) (
	deepSeekHarnessTurnStep, json.RawMessage, *deepSeekHarnessUsage, error,
) {
	fields, err := decodeDeepSeekHarnessObject(raw)
	if err != nil {
		return deepSeekHarnessTurnStep{}, nil, nil, errors.New("assistant data is not an object")
	}
	key, err := deepSeekHarnessEventTurnStep(raw)
	if err != nil {
		return deepSeekHarnessTurnStep{}, nil, nil, err
	}
	message, ok := fields["message"]
	if !ok {
		return deepSeekHarnessTurnStep{}, nil, nil, errors.New("assistant data has no message")
	}
	var usage *deepSeekHarnessUsage
	if rawUsage, ok := fields["usage"]; ok {
		parsed, err := parseDeepSeekHarnessUsage(rawUsage)
		if err != nil {
			return deepSeekHarnessTurnStep{}, nil, nil, err
		}
		usage = &parsed
	}
	return key, message, usage, nil
}

func deepSeekHarnessUserMessage(event deepSeekHarnessEvent) (ParsedMessage, error) {
	message, source, _, err := deepSeekHarnessMessageEnvelope(event.Data, "user")
	if err != nil {
		return ParsedMessage{}, err
	}
	parsed, err := parseDeepSeekHarnessContent(message, event.Time)
	if err != nil {
		return ParsedMessage{}, err
	}
	parsed.Role = RoleUser
	parsed.IsSystem = source != "user"
	parsed.SourceType = source
	return parsed, nil
}

func deepSeekHarnessAssistantMessage(
	raw json.RawMessage, eventTime int64,
) (ParsedMessage, string, error) {
	message, source, model, err := deepSeekHarnessMessageEnvelope(raw, "assistant")
	if err != nil {
		return ParsedMessage{}, "", err
	}
	if source != "model" {
		return ParsedMessage{}, "", errors.New("assistant source is not model")
	}
	parsed, err := parseDeepSeekHarnessContent(message, eventTime)
	if err != nil {
		return ParsedMessage{}, "", err
	}
	parsed.Role = RoleAssistant
	parsed.Model = model
	return parsed, model, nil
}

func deepSeekHarnessMessageEnvelope(
	raw json.RawMessage, wantRole string,
) (content json.RawMessage, sourceKind, model string, err error) {
	fields, err := decodeDeepSeekHarnessObject(raw)
	if err != nil {
		return nil, "", "", errors.New("message is not an object")
	}
	id, err := deepSeekHarnessRequiredString(fields, "id")
	if err != nil || id == "" {
		return nil, "", "", errors.New("message has invalid id")
	}
	role, err := deepSeekHarnessRequiredString(fields, "role")
	if err != nil || role != wantRole {
		return nil, "", "", fmt.Errorf("message role is not %q", wantRole)
	}
	content, ok := fields["content"]
	if !ok {
		return nil, "", "", errors.New("message has no content")
	}
	source, err := decodeDeepSeekHarnessObject(fields["source"])
	if err != nil {
		return nil, "", "", errors.New("message source is not an object")
	}
	sourceKind, err = deepSeekHarnessRequiredString(source, "kind")
	if err != nil || sourceKind == "" {
		return nil, "", "", errors.New("message source has invalid kind")
	}
	if sourceKind == "model" {
		model, err = deepSeekHarnessRequiredString(source, "model")
		if err != nil || model == "" {
			return nil, "", "", errors.New("model message source has invalid model")
		}
		if provider, err := deepSeekHarnessRequiredString(source, "provider"); err != nil || provider == "" {
			return nil, "", "", errors.New("model message source has invalid provider")
		}
	}
	return content, sourceKind, model, nil
}

func parseDeepSeekHarnessContent(
	raw json.RawMessage, eventTime int64,
) (ParsedMessage, error) {
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ParsedMessage{}, errors.New("message content is not an array")
	}
	parsed := ParsedMessage{Timestamp: time.UnixMilli(eventTime)}
	visible := make([]string, 0)
	thinking := make([]string, 0)
	for _, rawBlock := range blocks {
		fields, err := decodeDeepSeekHarnessObject(rawBlock)
		if err != nil {
			return ParsedMessage{}, errors.New("content block is not an object")
		}
		blockType, err := deepSeekHarnessRequiredString(fields, "type")
		if err != nil {
			return ParsedMessage{}, err
		}
		switch blockType {
		case "text":
			text, err := deepSeekHarnessRequiredString(fields, "text")
			if err != nil {
				return ParsedMessage{}, err
			}
			visible = append(visible, text)
		case "reasoning":
			text, err := deepSeekHarnessRequiredString(fields, "text")
			if err != nil {
				return ParsedMessage{}, err
			}
			thinking = append(thinking, text)
		case "image":
			if _, ok := fields["attachment"]; !ok {
				return ParsedMessage{}, errors.New("image block has no attachment")
			}
			visible = append(visible, "[image]")
		case "tool-call":
			id, idErr := deepSeekHarnessRequiredString(fields, "id")
			name, nameErr := deepSeekHarnessRequiredString(fields, "name")
			arguments, argumentsErr := deepSeekHarnessRequiredString(fields, "arguments")
			if idErr != nil || nameErr != nil || argumentsErr != nil || id == "" || name == "" {
				return ParsedMessage{}, errors.New("tool-call block is invalid")
			}
			parsed.ToolCalls = append(parsed.ToolCalls, ParsedToolCall{
				ToolUseID: id, ToolName: name,
				Category: NormalizeToolCategory(name), InputJSON: arguments,
			})
		case "tool-result":
			id, err := deepSeekHarnessRequiredString(fields, "toolCallId")
			if err != nil || id == "" {
				return ParsedMessage{}, errors.New("tool-result block has invalid toolCallId")
			}
			content, ok := fields["content"]
			if !ok {
				return ParsedMessage{}, errors.New("tool-result block has no content")
			}
			normalizedContent, err := normalizeDeepSeekHarnessContentImages(content)
			if err != nil {
				return ParsedMessage{}, err
			}
			text, err := deepSeekHarnessContentText(normalizedContent)
			if err != nil {
				return ParsedMessage{}, err
			}
			parsed.ToolResults = append(parsed.ToolResults, ParsedToolResult{
				ToolUseID: id, ContentLength: len(text), ContentRaw: string(normalizedContent),
			})
		default:
			return ParsedMessage{}, deepSeekHarnessUnsupportedError{message: fmt.Sprintf(
				"unsupported content block type %q", blockType,
			)}
		}
	}
	parsed.Content = strings.Join(visible, "\n")
	parsed.ThinkingText = strings.Join(thinking, "\n")
	parsed.HasThinking = parsed.ThinkingText != ""
	parsed.HasToolUse = len(parsed.ToolCalls) > 0
	parsed.ContentLength = len(parsed.Content)
	return parsed, nil
}

func deepSeekHarnessContentText(raw json.RawMessage) (string, error) {
	parsed, err := parseDeepSeekHarnessContent(raw, 0)
	if err != nil {
		return "", err
	}
	parts := []string{parsed.Content, parsed.ThinkingText}
	return strings.Trim(strings.Join(parts, "\n"), "\n"), nil
}

func normalizeDeepSeekHarnessContentImages(
	raw json.RawMessage,
) (json.RawMessage, error) {
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, errors.New("message content is not an array")
	}
	normalized := make([]json.RawMessage, 0, len(blocks))
	for _, block := range blocks {
		fields, err := decodeDeepSeekHarnessObject(block)
		if err != nil {
			return nil, errors.New("content block is not an object")
		}
		blockType, err := deepSeekHarnessRequiredString(fields, "type")
		if err != nil {
			return nil, err
		}
		switch blockType {
		case "image":
			if _, ok := fields["attachment"]; !ok {
				return nil, errors.New("image block has no attachment")
			}
			block, _ = json.Marshal(map[string]any{
				"type": "text", "text": "[image]",
			})
		case "tool-result":
			content, ok := fields["content"]
			if !ok {
				return nil, errors.New("tool-result block has no content")
			}
			nested, err := normalizeDeepSeekHarnessContentImages(content)
			if err != nil {
				return nil, err
			}
			fields["content"] = nested
			block, _ = json.Marshal(fields)
		}
		normalized = append(normalized, block)
	}
	return json.Marshal(normalized)
}

func deepSeekHarnessToolResultMessage(event deepSeekHarnessEvent) (ParsedMessage, error) {
	_, message, _, _, err := deepSeekHarnessToolResultData(event)
	return message, err
}

func deepSeekHarnessToolResultData(
	event deepSeekHarnessEvent,
) (deepSeekHarnessTurnStep, ParsedMessage, bool, string, error) {
	fields, err := decodeDeepSeekHarnessObject(event.Data)
	if err != nil {
		return deepSeekHarnessTurnStep{}, ParsedMessage{}, false, "",
			errors.New("tool result data is not an object")
	}
	key, err := deepSeekHarnessEventTurnStep(event.Data)
	if err != nil {
		return deepSeekHarnessTurnStep{}, ParsedMessage{}, false, "", err
	}
	rawMessage, ok := fields["message"]
	if !ok {
		return deepSeekHarnessTurnStep{}, ParsedMessage{}, false, "",
			errors.New("tool result data has no message")
	}
	content, source, _, err := deepSeekHarnessMessageEnvelope(rawMessage, "user")
	if err != nil {
		return deepSeekHarnessTurnStep{}, ParsedMessage{}, false, "", err
	}
	if source != "tool" {
		return deepSeekHarnessTurnStep{}, ParsedMessage{}, false, "",
			errors.New("tool result message source is not tool")
	}
	messageFields, _ := decodeDeepSeekHarnessObject(rawMessage)
	sourceFields, err := decodeDeepSeekHarnessObject(messageFields["source"])
	if err != nil {
		return deepSeekHarnessTurnStep{}, ParsedMessage{}, false, "",
			errors.New("tool result source is not an object")
	}
	sourceCallID, err := deepSeekHarnessRequiredString(sourceFields, "callId")
	if err != nil || sourceCallID == "" {
		return deepSeekHarnessTurnStep{}, ParsedMessage{}, false, "",
			errors.New("tool result source has invalid callId")
	}
	var rawBlocks []json.RawMessage
	if err := json.Unmarshal(content, &rawBlocks); err != nil || len(rawBlocks) != 1 {
		return deepSeekHarnessTurnStep{}, ParsedMessage{}, false, "",
			errors.New("tool result content must contain exactly one block")
	}
	blockFields, err := decodeDeepSeekHarnessObject(rawBlocks[0])
	if err != nil {
		return deepSeekHarnessTurnStep{}, ParsedMessage{}, false, "",
			errors.New("tool result content block is not an object")
	}
	isError := false
	if rawIsError, ok := blockFields["isError"]; ok {
		if err := json.Unmarshal(rawIsError, &isError); err != nil {
			return deepSeekHarnessTurnStep{}, ParsedMessage{}, false, "",
				errors.New("tool result isError is not a boolean")
		}
	}
	errorCode := ""
	if rawError, ok := fields["error"]; ok {
		errorFields, err := decodeDeepSeekHarnessObject(rawError)
		if err != nil {
			return deepSeekHarnessTurnStep{}, ParsedMessage{}, false, "",
				errors.New("tool result error is not an object")
		}
		name, nameErr := deepSeekHarnessRequiredString(errorFields, "name")
		code, codeErr := deepSeekHarnessRequiredString(errorFields, "code")
		if nameErr != nil || codeErr != nil || name == "" || code == "" {
			return deepSeekHarnessTurnStep{}, ParsedMessage{}, false, "",
				errors.New("tool result error is invalid")
		}
		errorCode = code
	}
	parsed, err := parseDeepSeekHarnessContent(content, event.Time)
	if err != nil {
		return deepSeekHarnessTurnStep{}, ParsedMessage{}, false, "", err
	}
	if len(parsed.ToolResults) != 1 || parsed.ToolResults[0].ToolUseID != sourceCallID {
		return deepSeekHarnessTurnStep{}, ParsedMessage{}, false, "",
			errors.New("tool result call id does not match its source")
	}
	parsed.Role = RoleUser
	parsed.IsSystem = true
	parsed.Content = ""
	parsed.ContentLength = 0
	parsed.SourceType = "tool"
	return key, parsed, isError, errorCode, nil
}

func deepSeekHarnessMessageVisible(message ParsedMessage) bool {
	return message.Content != "" || message.ThinkingText != "" ||
		len(message.ToolCalls) > 0 || len(message.ToolResults) > 0
}

func parseDeepSeekHarnessUsage(raw json.RawMessage) (deepSeekHarnessUsage, error) {
	fields, err := decodeDeepSeekHarnessObject(raw)
	if err != nil {
		return deepSeekHarnessUsage{}, errors.New("token usage is not an object")
	}
	input, err := deepSeekHarnessRequiredSafeInt(fields, "inputTokens", true)
	if err != nil {
		return deepSeekHarnessUsage{}, err
	}
	output, err := deepSeekHarnessRequiredSafeInt(fields, "outputTokens", true)
	if err != nil {
		return deepSeekHarnessUsage{}, err
	}
	usage := deepSeekHarnessUsage{
		InputTokens: int(input), OutputTokens: int(output),
	}
	optional := []struct {
		key    string
		target *int
	}{
		{"cacheReadTokens", &usage.CacheReadTokens},
		{"cacheWriteTokens", &usage.CacheWriteTokens},
		{"reasoningTokens", &usage.ReasoningTokens},
	}
	for _, field := range optional {
		if _, ok := fields[field.key]; !ok {
			continue
		}
		value, err := deepSeekHarnessRequiredSafeInt(fields, field.key, true)
		if err != nil {
			return deepSeekHarnessUsage{}, err
		}
		*field.target = int(value)
	}
	return usage, nil
}

func deepSeekHarnessCompactionUsage(
	raw json.RawMessage,
) (string, *deepSeekHarnessUsage, error) {
	fields, err := decodeDeepSeekHarnessObject(raw)
	if err != nil {
		return "", nil, errors.New("compaction summary data is not an object")
	}
	compactionID, err := deepSeekHarnessRequiredString(fields, "compactionId")
	if err != nil || compactionID == "" {
		return "", nil, errors.New("compaction summary has invalid compactionId")
	}
	provider, err := deepSeekHarnessRequiredString(fields, "provider")
	if err != nil || provider == "" {
		return "", nil, errors.New("compaction summary has invalid provider")
	}
	model, err := deepSeekHarnessRequiredString(fields, "model")
	if err != nil || model == "" {
		return "", nil, errors.New("compaction summary has invalid model")
	}
	if summary, ok := fields["summary"]; !ok {
		return "", nil, errors.New("compaction summary has no summary content")
	} else if _, err := parseDeepSeekHarnessContent(summary, 0); err != nil {
		return "", nil, fmt.Errorf("compaction summary content: %w", err)
	}
	rangeFields, err := decodeDeepSeekHarnessObject(fields["shadowedRange"])
	if err != nil {
		return "", nil, errors.New("compaction summary has invalid shadowedRange")
	}
	start, err := deepSeekHarnessRequiredSafeInt(rangeFields, "start", true)
	if err != nil {
		return "", nil, fmt.Errorf("compaction summary shadowedRange: %w", err)
	}
	end, err := deepSeekHarnessRequiredSafeInt(rangeFields, "end", true)
	if err != nil {
		return "", nil, errors.New("compaction summary has invalid shadowedRange end")
	}
	shadowedSeqs, err := deepSeekHarnessSafeIntArray(fields["shadowedSeqs"], true)
	if err != nil || len(shadowedSeqs) == 0 ||
		shadowedSeqs[0] != start || shadowedSeqs[len(shadowedSeqs)-1] != end {
		return "", nil, errors.New("compaction summary has invalid shadowedSeqs")
	}
	if _, err := deepSeekHarnessRequiredSafeInt(fields, "shadowedTokenCount", true); err != nil {
		return "", nil, fmt.Errorf("compaction summary: %w", err)
	}
	rawOutput, hasRawOutput := fields["rawOutput"]
	if hasRawOutput {
		if _, err := parseDeepSeekHarnessContent(rawOutput, 0); err != nil {
			return "", nil, fmt.Errorf("compaction summary raw output: %w", err)
		}
	}
	if rawCall, ok := fields["llmStreamCall"]; ok {
		if string(rawCall) != "true" {
			return "", nil, errors.New("compaction summary has invalid llmStreamCall")
		}
		if !hasRawOutput {
			return "", nil, errors.New("compaction summary llmStreamCall has no rawOutput")
		}
	}
	if rawMaxTokens, ok := fields["maxTokens"]; ok {
		if _, err := deepSeekHarnessRequiredSafeInt(
			map[string]json.RawMessage{"maxTokens": rawMaxTokens}, "maxTokens", true,
		); err != nil {
			return "", nil, fmt.Errorf("compaction summary: %w", err)
		}
	}
	rawUsage, ok := fields["usage"]
	if !ok {
		return model, nil, nil
	}
	usage, err := parseDeepSeekHarnessUsage(rawUsage)
	if err != nil {
		return "", nil, fmt.Errorf("compaction summary usage: %w", err)
	}
	return model, &usage, nil
}

func deepSeekHarnessApplyUsage(
	message ParsedMessage, usage *deepSeekHarnessUsage, model string,
) ParsedMessage {
	message.Model = firstNonEmptyJSONLString(message.Model, model)
	message.ContextTokens = usage.InputTokens + usage.CacheReadTokens + usage.CacheWriteTokens
	message.OutputTokens = usage.OutputTokens
	message.HasContextTokens = true
	message.HasOutputTokens = true
	message.tokenPresenceKnown = true
	return message
}

func applyDeepSeekHarnessChunk(
	response *deepSeekHarnessResponse, raw json.RawMessage, eventTime int64,
) error {
	fields, err := decodeDeepSeekHarnessObject(raw)
	if err != nil {
		return errors.New("stream chunk is not an object")
	}
	chunkType, err := deepSeekHarnessRequiredString(fields, "type")
	if err != nil {
		return err
	}
	blockFor := func(index int64) *deepSeekHarnessBlockState {
		block := response.Blocks[index]
		if block == nil {
			block = &deepSeekHarnessBlockState{}
			response.Blocks[index] = block
		}
		return block
	}
	switch chunkType {
	case "block-start":
		index, err := deepSeekHarnessRequiredSafeInt(fields, "index", true)
		if err != nil {
			return err
		}
		blockType, err := deepSeekHarnessRequiredString(fields, "blockType")
		if err != nil {
			return err
		}
		switch blockType {
		case "text", "reasoning", "image", "tool-call", "tool-result":
		default:
			return deepSeekHarnessUnsupportedError{message: fmt.Sprintf(
				"unsupported content block type %q", blockType,
			)}
		}
		blockFor(index).BlockType = blockType
	case "text-delta", "reasoning-delta":
		index, err := deepSeekHarnessRequiredSafeInt(fields, "index", true)
		if err != nil {
			return err
		}
		text, err := deepSeekHarnessRequiredString(fields, "text")
		if err != nil {
			return err
		}
		block := blockFor(index)
		if chunkType == "text-delta" {
			block.BlockType = "text"
		} else {
			block.BlockType = "reasoning"
		}
		block.Text.WriteString(text)
	case "tool-call-delta":
		index, err := deepSeekHarnessRequiredSafeInt(fields, "index", true)
		if err != nil {
			return err
		}
		id, err := deepSeekHarnessRequiredString(fields, "id")
		if err != nil || id == "" {
			return errors.New("tool-call delta has invalid id")
		}
		arguments, err := deepSeekHarnessRequiredString(fields, "argumentsDelta")
		if err != nil {
			return err
		}
		block := blockFor(index)
		block.BlockType, block.ToolCallID = "tool-call", id
		if rawName, ok := fields["name"]; ok {
			block.ToolName, err = deepSeekHarnessString(rawName)
			if err != nil {
				return errors.New("tool-call delta has invalid name")
			}
		}
		block.Arguments.WriteString(arguments)
	case "block-end":
		index, err := deepSeekHarnessRequiredSafeInt(fields, "index", true)
		if err != nil {
			return err
		}
		completed, ok := fields["block"]
		if !ok {
			return errors.New("block-end has no block")
		}
		if _, err := parseDeepSeekHarnessContent(json.RawMessage("["+string(completed)+"]"), eventTime); err != nil {
			return err
		}
		blockFor(index).CompletedBlock = append(json.RawMessage(nil), completed...)
	case "usage":
		rawUsage, ok := fields["usage"]
		if !ok {
			return errors.New("usage chunk has no usage")
		}
		usage, err := parseDeepSeekHarnessUsage(rawUsage)
		if err != nil {
			return err
		}
		response.ChunkUsage = &usage
		response.UsageTime = eventTime
	case "finish":
		reason, err := decodeDeepSeekHarnessObject(fields["reason"])
		if err != nil {
			return errors.New("finish chunk reason is not an object")
		}
		kind, err := deepSeekHarnessRequiredString(reason, "kind")
		if err != nil {
			return err
		}
		if kind == "" {
			return errors.New("finish chunk reason has empty kind")
		}
		response.StopReason = kind
	default:
		return deepSeekHarnessUnsupportedError{message: fmt.Sprintf(
			"unsupported stream chunk type %q", chunkType,
		)}
	}
	return nil
}

func buildDeepSeekHarnessPartialMessage(
	response *deepSeekHarnessResponse,
) (ParsedMessage, error) {
	indexes := make([]int64, 0, len(response.Blocks))
	for index := range response.Blocks {
		indexes = append(indexes, index)
	}
	slices.Sort(indexes)
	blocks := make([]json.RawMessage, 0, len(indexes))
	for _, index := range indexes {
		block := response.Blocks[index]
		if block == nil {
			continue
		}
		if len(block.CompletedBlock) > 0 {
			blocks = append(blocks, block.CompletedBlock)
			continue
		}
		var value any
		switch block.BlockType {
		case "text":
			value = map[string]any{"type": "text", "text": block.Text.String()}
		case "reasoning":
			value = map[string]any{"type": "reasoning", "text": block.Text.String()}
		case "tool-call":
			if block.ToolCallID == "" || block.ToolName == "" {
				continue
			}
			value = map[string]any{
				"type": "tool-call", "id": block.ToolCallID,
				"name": block.ToolName, "arguments": block.Arguments.String(),
			}
		default:
			continue
		}
		encoded, _ := json.Marshal(value)
		blocks = append(blocks, encoded)
	}
	encoded, _ := json.Marshal(blocks)
	message, err := parseDeepSeekHarnessContent(encoded, response.FirstChunkTime)
	if err != nil {
		return ParsedMessage{}, err
	}
	message.Role = RoleAssistant
	message.Model = response.RequestModel
	message.StopReason = response.StopReason
	return message, nil
}
