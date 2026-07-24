package artifact

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
)

func decodeManifestWithLimits(data []byte, limits artifactLimits) (manifest, error) {
	var envelope struct {
		Version     int             `json:"v"`
		Origin      string          `json:"origin"`
		Segments    json.RawMessage `json:"segments"`
		UsageEvents json.RawMessage `json:"usage_events"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return manifest{}, err
	}
	// Future manifests are retained for forward compatibility. Reading only
	// their scalar header avoids allocating collections whose schema this
	// version does not understand.
	if envelope.Version > formatVersion {
		return manifest{Version: envelope.Version, Origin: envelope.Origin}, nil
	}
	if err := preflightManifestCollections(
		envelope.Segments, envelope.UsageEvents, limits,
	); err != nil {
		return manifest{}, err
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return manifest{}, err
	}
	return m, nil
}

func preflightManifestCollections(
	segments, usageEvents json.RawMessage,
	limits artifactLimits,
) error {
	if err := preflightSegmentReferences(segments, limits.manifestSegments); err != nil {
		return err
	}
	return preflightJSONArrayCount(
		usageEvents, "manifest usage event", limits.manifestUsageEvents,
	)
}

func preflightSegmentReferences(data json.RawMessage, limit int) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	token, err := dec.Token()
	if err != nil {
		return err
	}
	if token != json.Delim('[') {
		return errors.New("manifest segments must be an array")
	}
	seen := make(map[string]struct{}, min(limit, 16))
	count := 0
	for dec.More() {
		if count >= limit {
			return fmt.Errorf("manifest segment reference limit exceeded: limit %d", limit)
		}
		var hash string
		if err := dec.Decode(&hash); err != nil {
			return fmt.Errorf("decoding manifest segment reference: %w", err)
		}
		if _, ok := seen[hash]; ok {
			return fmt.Errorf("manifest has duplicate segment reference %s", hash)
		}
		seen[hash] = struct{}{}
		count++
	}
	_, err = dec.Token()
	return err
}

func preflightJSONArrayCount(data json.RawMessage, name string, limit int) error {
	_, err := countJSONArrayElements(data, name, limit)
	return err
}

func countJSONArrayElements(
	data json.RawMessage,
	name string,
	limit int,
) (int, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return 0, nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	token, err := dec.Token()
	if err != nil {
		return 0, err
	}
	if token != json.Delim('[') {
		return 0, fmt.Errorf("%ss must be an array", name)
	}
	count := 0
	for dec.More() {
		if count >= limit {
			return 0, fmt.Errorf("%s limit exceeded: limit %d", name, limit)
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return 0, fmt.Errorf("decoding %s: %w", name, err)
		}
		count++
	}
	if _, err := dec.Token(); err != nil {
		return 0, err
	}
	return count, nil
}

func canonicalMessages(msgs []db.Message) []db.Message {
	out := make([]db.Message, len(msgs))
	for i, msg := range msgs {
		msg.ID = 0
		msg.SessionID = ""
		if len(msg.ToolCalls) > 0 {
			calls := make([]db.ToolCall, len(msg.ToolCalls))
			copy(calls, msg.ToolCalls)
			for j := range calls {
				calls[j].MessageID = 0
				calls[j].SessionID = ""
			}
			msg.ToolCalls = calls
		}
		out[i] = msg
	}
	return out
}

func decodeSegment(data []byte) ([]db.Message, error) {
	return decodeSegmentWithLimits(data, productionArtifactLimits())
}

func decodeSegmentWithLimits(data []byte, limits artifactLimits) ([]db.Message, error) {
	preflight, err := preflightSegmentData(data, limits)
	if err != nil {
		return nil, err
	}
	return decodePreflightedSegment(preflight)
}

func decodePreflightedSegment(preflight segmentPreflight) ([]db.Message, error) {
	msgs := make([]db.Message, 0, len(preflight.records))
	for _, line := range preflight.records {
		var record segmentMessage
		if err := json.Unmarshal(line, &record); err != nil {
			return nil, fmt.Errorf("decoding message segment: %w", err)
		}
		msgs = append(msgs, record.dbMessage())
	}
	return msgs, nil
}

func segmentRecords(data []byte, limit int) ([][]byte, error) {
	capacity := min(max(limit, 0), 64)
	records := make([][]byte, 0, capacity)
	remaining := data
	lineNumber := 0
	for len(remaining) > 0 {
		lineNumber++
		newline := bytes.IndexByte(remaining, '\n')
		line := remaining
		if newline >= 0 {
			line = remaining[:newline]
			remaining = remaining[newline+1:]
		} else {
			remaining = nil
		}
		if len(records) >= limit {
			return nil, fmt.Errorf(
				"message record limit exceeded: limit %d per segment", limit,
			)
		}
		if len(bytes.TrimSpace(line)) == 0 {
			return nil, fmt.Errorf("blank message record at line %d", lineNumber)
		}
		records = append(records, line)
	}
	return records, nil
}

func preflightSegmentData(data []byte, limits artifactLimits) (segmentPreflight, error) {
	records, err := segmentRecords(data, limits.segmentMessages)
	if err != nil {
		return segmentPreflight{}, err
	}
	preflight := segmentPreflight{records: records}
	for _, line := range records {
		var header struct {
			Version int `json:"v"`
		}
		if err := json.Unmarshal(line, &header); err != nil {
			return segmentPreflight{}, fmt.Errorf("decoding message segment header: %w", err)
		}
		if header.Version > formatVersion {
			return segmentPreflight{}, fmt.Errorf(
				"%w: message segment has artifact version %d",
				errFutureArtifactVersion, header.Version,
			)
		}
		if header.Version != formatVersion {
			return segmentPreflight{}, fmt.Errorf(
				"message segment has unsupported artifact version %d",
				header.Version,
			)
		}
		messageNested, err := preflightMessageNestedCollections(line, limits)
		if err != nil {
			return segmentPreflight{}, err
		}
		if exceedsCollectionLimit(
			preflight.nested.toolCalls,
			messageNested.toolCalls,
			limits.segmentToolCalls,
		) {
			return segmentPreflight{}, fmt.Errorf(
				"segment tool call limit exceeded: limit %d", limits.segmentToolCalls,
			)
		}
		if exceedsCollectionLimit(
			preflight.nested.resultEvents,
			messageNested.resultEvents,
			limits.segmentResultEvents,
		) {
			return segmentPreflight{}, fmt.Errorf(
				"segment result event limit exceeded: limit %d",
				limits.segmentResultEvents,
			)
		}
		preflight.nested.toolCalls += messageNested.toolCalls
		preflight.nested.resultEvents += messageNested.resultEvents
	}
	return preflight, nil
}

func preflightMessageNestedCollections(
	line []byte,
	limits artifactLimits,
) (nestedCollectionCounts, error) {
	var envelope struct {
		Ordinal   int             `json:"ordinal"`
		ToolCalls json.RawMessage `json:"tool_calls"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		return nestedCollectionCounts{}, fmt.Errorf(
			"decoding message segment collections: %w", err,
		)
	}
	return preflightToolCallCollections(envelope.ToolCalls, envelope.Ordinal, limits)
}

func preflightToolCallCollections(
	data json.RawMessage,
	ordinal int,
	limits artifactLimits,
) (nestedCollectionCounts, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nestedCollectionCounts{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	token, err := dec.Token()
	if err != nil {
		return nestedCollectionCounts{}, err
	}
	if token != json.Delim('[') {
		return nestedCollectionCounts{}, errors.New("message tool_calls must be an array")
	}
	counts := nestedCollectionCounts{}
	for dec.More() {
		if counts.toolCalls >= limits.messageToolCalls {
			return nestedCollectionCounts{}, fmt.Errorf(
				"tool call limit exceeded for message ordinal %d: limit %d per message",
				ordinal, limits.messageToolCalls,
			)
		}
		var toolEnvelope struct {
			ResultEvents json.RawMessage `json:"result_events"`
		}
		if err := dec.Decode(&toolEnvelope); err != nil {
			return nestedCollectionCounts{}, fmt.Errorf(
				"decoding tool call %d in message ordinal %d: %w",
				counts.toolCalls, ordinal, err,
			)
		}
		resultEvents, err := countJSONArrayElements(
			toolEnvelope.ResultEvents, "result event", limits.toolResultEvents,
		)
		if err != nil {
			return nestedCollectionCounts{}, fmt.Errorf(
				"preflighting tool call %d in message ordinal %d: %w",
				counts.toolCalls, ordinal, err,
			)
		}
		counts.toolCalls++
		counts.resultEvents += resultEvents
	}
	if _, err := dec.Token(); err != nil {
		return nestedCollectionCounts{}, err
	}
	return counts, nil
}

func (m segmentMessage) dbMessage() db.Message {
	msg := db.Message{
		Ordinal:           m.Ordinal,
		Role:              m.Role,
		Content:           m.Content,
		ThinkingText:      m.ThinkingText,
		Timestamp:         m.Timestamp,
		HasThinking:       m.HasThinking,
		HasToolUse:        m.HasToolUse,
		ContentLength:     m.ContentLength,
		Model:             m.Model,
		TokenUsage:        m.TokenUsage,
		ContextTokens:     m.ContextTokens,
		OutputTokens:      m.OutputTokens,
		HasContextTokens:  m.HasContextTokens,
		HasOutputTokens:   m.HasOutputTokens,
		ClaudeMessageID:   m.ClaudeMessageID,
		ClaudeRequestID:   m.ClaudeRequestID,
		IsSystem:          m.IsSystem,
		SourceType:        m.SourceType,
		SourceSubtype:     m.SourceSubtype,
		SourceUUID:        m.SourceUUID,
		SourceParentUUID:  m.SourceParentUUID,
		IsSidechain:       m.IsSidechain,
		IsCompactBoundary: m.IsCompactBoundary,
	}
	if len(m.ToolCalls) > 0 {
		msg.ToolCalls = make([]db.ToolCall, len(m.ToolCalls))
		for i, call := range m.ToolCalls {
			msg.ToolCalls[i] = db.ToolCall{
				ToolName:            call.ToolName,
				Category:            call.Category,
				ToolUseID:           call.ToolUseID,
				InputJSON:           call.InputJSON,
				FilePath:            call.FilePath,
				SkillName:           call.SkillName,
				ResultContentLength: call.ResultContentLength,
				ResultContent:       call.ResultContent,
				SubagentSessionID:   call.SubagentSessionID,
			}
			if len(call.ResultEvents) > 0 {
				msg.ToolCalls[i].ResultEvents = make([]db.ToolResultEvent, len(call.ResultEvents))
				for j, ev := range call.ResultEvents {
					msg.ToolCalls[i].ResultEvents[j] = db.ToolResultEvent{
						ToolUseID:         ev.ToolUseID,
						AgentID:           ev.AgentID,
						SubagentSessionID: ev.SubagentSessionID,
						Source:            ev.Source,
						Status:            ev.Status,
						Content:           ev.Content,
						ContentLength:     ev.ContentLength,
						Timestamp:         ev.Timestamp,
						EventIndex:        ev.EventIndex,
					}
				}
			}
		}
	}
	return msg
}
