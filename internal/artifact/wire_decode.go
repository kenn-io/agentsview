package artifact

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
)

func decodeManifestWithLimits(data []byte, limits artifactLimits) (manifest, error) {
	var envelope struct {
		Version     int            `json:"v"`
		Origin      string         `json:"origin"`
		Segments    jsontext.Value `json:"segments"`
		UsageEvents jsontext.Value `json:"usage_events"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return manifest{}, err
	}
	// Future manifests are retained for forward compatibility. Reading only
	// their scalar header avoids allocating collections whose schema this
	// version does not understand.
	if envelope.Version > manifestFormatVersion {
		return manifest{}, &futureArtifactVersionError{
			Kind: KindManifests, Version: envelope.Version,
		}
	}
	if envelope.Version < manifestMinDecodeVersion {
		return manifest{}, fmt.Errorf(
			"manifest has unsupported artifact version %d", envelope.Version,
		)
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
	segments, usageEvents jsontext.Value,
	limits artifactLimits,
) error {
	if err := preflightSegmentReferences(segments, limits.manifestSegments); err != nil {
		return err
	}
	return preflightJSONArrayCount(
		usageEvents, "manifest usage event", limits.manifestUsageEvents,
	)
}

func preflightSegmentReferences(data jsontext.Value, limit int) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	dec := jsontext.NewDecoder(bytes.NewReader(trimmed))
	token, err := dec.ReadToken()
	if err != nil {
		return err
	}
	if token.Kind() != jsontext.KindBeginArray {
		return errors.New("manifest segments must be an array")
	}
	seen := make(map[string]struct{}, min(limit, 16))
	count := 0
	for dec.PeekKind() != jsontext.KindEndArray {
		if count >= limit {
			return fmt.Errorf("manifest segment reference limit exceeded: limit %d", limit)
		}
		var hash string
		if err := json.UnmarshalDecode(dec, &hash); err != nil {
			return fmt.Errorf("decoding manifest segment reference: %w", err)
		}
		if _, ok := seen[hash]; ok {
			return fmt.Errorf("manifest has duplicate segment reference %s", hash)
		}
		seen[hash] = struct{}{}
		count++
	}
	_, err = dec.ReadToken()
	return err
}

func preflightJSONArrayCount(data jsontext.Value, name string, limit int) error {
	_, err := countJSONArrayElements(data, name, limit)
	return err
}

func countJSONArrayElements(
	data jsontext.Value,
	name string,
	limit int,
) (int, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return 0, nil
	}
	dec := jsontext.NewDecoder(bytes.NewReader(trimmed))
	token, err := dec.ReadToken()
	if err != nil {
		return 0, err
	}
	if token.Kind() != jsontext.KindBeginArray {
		return 0, fmt.Errorf("%ss must be an array", name)
	}
	count := 0
	for dec.PeekKind() != jsontext.KindEndArray {
		if count >= limit {
			return 0, fmt.Errorf("%s limit exceeded: limit %d", name, limit)
		}
		var value jsontext.Value
		if err := json.UnmarshalDecode(dec, &value); err != nil {
			return 0, fmt.Errorf("decoding %s: %w", name, err)
		}
		count++
	}
	if _, err := dec.ReadToken(); err != nil {
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

func preflightSegmentData(data []byte, limits artifactLimits) (segmentPreflight, error) {
	preflight := segmentPreflight{
		records: make([][]byte, 0, min(max(limits.segmentMessages, 0), 64)),
	}
	remaining := data
	lineNumber := 0
	version := messageSegmentFormatVersion
	haveVersion := false
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
		if len(bytes.TrimSpace(line)) == 0 {
			return segmentPreflight{}, fmt.Errorf(
				"blank message record at line %d", lineNumber,
			)
		}
		if !haveVersion {
			var header struct {
				Version int `json:"v"`
			}
			if err := json.Unmarshal(line, &header); err != nil {
				return segmentPreflight{}, fmt.Errorf(
					"decoding message segment header at line %d: %w",
					lineNumber, err,
				)
			}
			version = header.Version
			haveVersion = true
			if version > messageSegmentFormatVersion {
				return segmentPreflight{}, &futureArtifactVersionError{
					Kind: KindSegments, Version: version,
				}
			}
			if version < messageSegmentMinDecodeVersion {
				return segmentPreflight{}, fmt.Errorf(
					"message segment has unsupported artifact version %d",
					version,
				)
			}
		}
		if len(preflight.records) >= limits.segmentMessages {
			return segmentPreflight{}, fmt.Errorf(
				"message record limit exceeded: limit %d per segment",
				limits.segmentMessages,
			)
		}
		recordVersion, messageNested, err :=
			preflightMessageNestedCollections(line, limits)
		if err != nil {
			return segmentPreflight{}, err
		}
		if recordVersion != version {
			return segmentPreflight{}, fmt.Errorf(
				"message segment mixes artifact versions %d and %d",
				version, recordVersion,
			)
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
		preflight.records = append(preflight.records, line)
	}
	return preflight, nil
}

func preflightMessageNestedCollections(
	line []byte,
	limits artifactLimits,
) (int, nestedCollectionCounts, error) {
	var envelope struct {
		Version   int            `json:"v"`
		Ordinal   int            `json:"ordinal"`
		ToolCalls jsontext.Value `json:"tool_calls"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		return 0, nestedCollectionCounts{}, fmt.Errorf(
			"decoding message segment collections: %w", err,
		)
	}
	nested, err := preflightToolCallCollections(
		envelope.ToolCalls, envelope.Ordinal, limits,
	)
	return envelope.Version, nested, err
}

func preflightToolCallCollections(
	data jsontext.Value,
	ordinal int,
	limits artifactLimits,
) (nestedCollectionCounts, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nestedCollectionCounts{}, nil
	}
	dec := jsontext.NewDecoder(bytes.NewReader(trimmed))
	token, err := dec.ReadToken()
	if err != nil {
		return nestedCollectionCounts{}, err
	}
	if token.Kind() != jsontext.KindBeginArray {
		return nestedCollectionCounts{}, errors.New("message tool_calls must be an array")
	}
	counts := nestedCollectionCounts{}
	for dec.PeekKind() != jsontext.KindEndArray {
		if counts.toolCalls >= limits.messageToolCalls {
			return nestedCollectionCounts{}, fmt.Errorf(
				"tool call limit exceeded for message ordinal %d: limit %d per message",
				ordinal, limits.messageToolCalls,
			)
		}
		var toolEnvelope struct {
			ResultEvents jsontext.Value `json:"result_events"`
		}
		if err := json.UnmarshalDecode(dec, &toolEnvelope); err != nil {
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
	if _, err := dec.ReadToken(); err != nil {
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
		ProviderID:        m.ProviderID,
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
		PromptSource:      m.PromptSource,
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
