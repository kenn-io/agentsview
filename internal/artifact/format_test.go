package artifact

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
)

func TestCanonicalCheckpointGolden(t *testing.T) {
	t.Parallel()

	cp := checkpoint{
		Version:  checkpointFormatVersion,
		Origin:   "laptop-a1b2c3",
		Sequence: 7,
		Sessions: map[string]string{
			"laptop-a1b2c3~sess-b": "b222",
			"laptop-a1b2c3~sess-a": "a111",
		},
	}

	data, err := canonicalJSON(cp)
	require.NoError(t, err)

	assert.Equal(t,
		"{\"origin\":\"laptop-a1b2c3\",\"seq\":7,\"sessions\":{\"laptop-a1b2c3~sess-a\":\"a111\",\"laptop-a1b2c3~sess-b\":\"b222\"},\"v\":1}\n",
		string(data),
	)
	assert.Equal(t, "56fd64d35ebd700bfa2a50d41a97857b871e033e5c1e1d02dea55c25c7df7655", hashHex(data))
}

func TestCanonicalManifestGolden(t *testing.T) {
	t.Parallel()

	ordinal := 2
	parent := "parent-1"
	name := "Fixture"
	raw := RawSourceRef{
		Hash:      "raw123",
		Size:      4096,
		MediaType: "application/jsonl",
		Path:      "claude/session.jsonl",
	}
	m := manifest{
		Version:         manifestFormatVersion,
		Origin:          "laptop-a1b2c3",
		NativeSessionID: "sess-1",
		Session: manifestSession{
			ID:                "sess-1",
			Project:           "alpha",
			Machine:           "laptop-a1b2c3",
			Agent:             "claude",
			FirstMessage:      new("hello"),
			StartedAt:         new("2026-06-14T01:02:03Z"),
			EndedAt:           new("2026-06-14T01:03:03Z"),
			MessageCount:      2,
			UserMessageCount:  1,
			ParentSessionID:   &parent,
			RelationshipType:  "subagent",
			TotalOutputTokens: 42,
			CreatedAt:         "2026-06-14T01:02:03Z",
		},
		SessionName: &name,
		Segments:    []string{"seg222", "seg111"},
		UsageEvents: []artifactUsageEvent{
			{
				MessageOrdinal: &ordinal,
				Source:         "fixture",
				Model:          "claude-test",
				InputTokens:    11,
				OutputTokens:   7,
				Cost:           &money.Money{Microdollars: 31_250},
				CostStatus:     "known",
				CostSource:     "fixture",
				OccurredAt:     "2026-06-14T01:02:04Z",
				DedupKey:       "usage-1",
			},
		},
		RawSource:             &raw,
		DataVersion:           99,
		Generation:            3,
		SessionHasToolCalls:   true,
		SessionHasContextData: true,
		SessionQualitySignals: &manifestQualitySignals{
			Version:                     3,
			ShortPromptCount:            2,
			UnstructuredStart:           true,
			MissingSuccessCriteriaCount: 4,
			MissingVerificationCount:    5,
			DuplicatePromptCount:        6,
			NoCodeContextCount:          7,
			RunawayToolLoopCount:        1,
		},
	}

	data, err := canonicalJSON(m)
	require.NoError(t, err)

	assert.Equal(t,
		"{\"data_version\":99,\"generation\":3,\"native_session_id\":\"sess-1\",\"origin\":\"laptop-a1b2c3\",\"raw_source\":{\"hash\":\"raw123\",\"media_type\":\"application/jsonl\",\"path\":\"claude/session.jsonl\",\"size\":4096},\"segments\":[\"seg222\",\"seg111\"],\"session\":{\"agent\":\"claude\",\"compaction_count\":0,\"consecutive_failure_max\":0,\"created_at\":\"2026-06-14T01:02:03Z\",\"edit_churn_count\":0,\"ended_at\":\"2026-06-14T01:03:03Z\",\"ended_with_role\":\"\",\"final_failure_streak\":0,\"first_message\":\"hello\",\"has_peak_context_tokens\":false,\"has_total_output_tokens\":false,\"id\":\"sess-1\",\"is_automated\":false,\"machine\":\"laptop-a1b2c3\",\"message_count\":2,\"mid_task_compaction_count\":0,\"outcome\":\"\",\"outcome_confidence\":\"\",\"parent_session_id\":\"parent-1\",\"peak_context_tokens\":0,\"project\":\"alpha\",\"relationship_type\":\"subagent\",\"secret_leak_count\":0,\"started_at\":\"2026-06-14T01:02:03Z\",\"tool_failure_signal_count\":0,\"tool_retry_count\":0,\"total_output_tokens\":42,\"user_message_count\":1},\"session_has_context_data\":true,\"session_has_tool_calls\":true,\"session_name\":\"Fixture\",\"session_quality_signals\":{\"duplicate_prompt_count\":6,\"missing_success_criteria_count\":4,\"missing_verification_count\":5,\"no_code_context_count\":7,\"runaway_tool_loop_count\":1,\"short_prompt_count\":2,\"unstructured_start\":true,\"version\":3},\"usage_events\":[{\"cost\":{\"microdollars\":31250},\"cost_source\":\"fixture\",\"cost_status\":\"known\",\"dedup_key\":\"usage-1\",\"input_tokens\":11,\"message_ordinal\":2,\"model\":\"claude-test\",\"occurred_at\":\"2026-06-14T01:02:04Z\",\"output_tokens\":7,\"source\":\"fixture\"}],\"v\":3}\n",
		string(data),
	)
	assert.Equal(t, "855d1a7ed086095582cc94136837bcb93d74f4ad0cf446b0a77177d1b31aba23", hashHex(data))
}

func TestDecodeManifestRejectsUnsupportedOlderVersion(t *testing.T) {
	t.Parallel()

	data := []byte(`{
		"v": 1,
		"origin": "laptop-a1b2c3",
		"native_session_id": "sess-1",
		"segments": [],
		"usage_events": [{
			"source": "fixture",
			"model": "claude-test",
			"cost_usd": 0.03125
		}]
	}`)

	decoded, err := decodeManifestWithLimits(data, productionArtifactLimits())
	require.Error(t, err)
	assert.Empty(t, decoded)
	assert.Contains(t, err.Error(), "manifest has unsupported artifact version 1")
}

func TestDecodeManifestAcceptsPriorVersionWithoutSessionKind(t *testing.T) {
	t.Parallel()

	data := []byte(`{
		"v": 2,
		"origin": "laptop-a1b2c3",
		"native_session_id": "sess-1",
		"segments": [],
		"session": {
			"id": "sess-1",
			"machine": "laptop-a1b2c3",
			"agent": "claude",
			"created_at": "2026-06-14T01:02:03Z"
		}
	}`)

	decoded, err := decodeManifestWithLimits(data, productionArtifactLimits())
	require.NoError(t, err)
	assert.Equal(t, 2, decoded.Version)
	assert.Empty(t, decoded.Session.SessionKind,
		"v2 manifests predate session_kind and must decode with it empty")
}

func TestDecodeSegmentAcceptsPriorVersionWithoutPromptSource(t *testing.T) {
	t.Parallel()

	data := []byte("{\"content\":\"hello\",\"ordinal\":0,\"role\":\"user\",\"v\":1}\n")

	msgs, err := decodeSegmentWithLimits(data, productionArtifactLimits())
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Empty(t, msgs[0].PromptSource,
		"v1 segments predate prompt_source and must decode with it empty")
}

func TestCanonicalMessageSegmentGolden(t *testing.T) {
	t.Parallel()

	msgs := []db.Message{
		{
			ID:               99,
			SessionID:        "sess-1",
			Ordinal:          2,
			Role:             "assistant",
			Content:          "world",
			ContentLength:    5,
			Timestamp:        "2026-06-14T01:02:05Z",
			HasToolUse:       true,
			Model:            "claude-test",
			TokenUsage:       jsontext.Value(`{"output":2,"input":1}`),
			OutputTokens:     2,
			HasOutputTokens:  true,
			ClaudeMessageID:  "msg-1",
			ClaudeRequestID:  "req-1",
			SourceType:       "jsonl",
			SourceSubtype:    "assistant",
			SourceUUID:       "uuid-msg-1",
			SourceParentUUID: "uuid-parent",
			ToolCalls: []db.ToolCall{
				{
					MessageID:           99,
					SessionID:           "sess-1",
					ToolName:            "Read",
					Category:            "file",
					ToolUseID:           "tool-1",
					InputJSON:           "{\"file_path\":\"README.md\"}",
					FilePath:            "README.md",
					ResultContentLength: 12,
					ResultContent:       "file content",
					SubagentSessionID:   "child-1",
					ResultEvents: []db.ToolResultEvent{
						{
							ToolUseID:         "tool-1",
							AgentID:           "agent-1",
							SubagentSessionID: "child-1",
							Source:            "tool_result",
							Status:            "success",
							Content:           "done",
							ContentLength:     4,
							Timestamp:         "2026-06-14T01:02:06Z",
							EventIndex:        0,
						},
					},
				},
			},
		},
	}

	data, err := encodeSegment(msgs)
	require.NoError(t, err)

	assert.Equal(t,
		"{\"claude_message_id\":\"msg-1\",\"claude_request_id\":\"req-1\",\"content\":\"world\",\"content_length\":5,\"has_output_tokens\":true,\"has_tool_use\":true,\"model\":\"claude-test\",\"ordinal\":2,\"output_tokens\":2,\"role\":\"assistant\",\"source_parent_uuid\":\"uuid-parent\",\"source_subtype\":\"assistant\",\"source_type\":\"jsonl\",\"source_uuid\":\"uuid-msg-1\",\"timestamp\":\"2026-06-14T01:02:05Z\",\"token_usage\":{\"input\":1,\"output\":2},\"tool_calls\":[{\"call_index\":0,\"category\":\"file\",\"file_path\":\"README.md\",\"input_json\":\"{\\\"file_path\\\":\\\"README.md\\\"}\",\"result_content\":\"file content\",\"result_content_length\":12,\"result_events\":[{\"agent_id\":\"agent-1\",\"content\":\"done\",\"content_length\":4,\"event_index\":0,\"source\":\"tool_result\",\"status\":\"success\",\"subagent_session_id\":\"child-1\",\"timestamp\":\"2026-06-14T01:02:06Z\",\"tool_use_id\":\"tool-1\"}],\"subagent_session_id\":\"child-1\",\"tool_name\":\"Read\",\"tool_use_id\":\"tool-1\"}],\"v\":2}\n",
		string(data),
	)
	assert.NotContains(t, string(data), `"id"`)
	assert.NotContains(t, string(data), `"session_id"`)
	assert.NotContains(t, string(data), `"message_id"`)
	assert.Equal(t, "e45219604ab58bb9685ac3dfb0f5cc5e865705e29dc8a985ccebe1b748a6d396", hashHex(data))
}

func TestEncodeSegmentPreservesPromptSource(t *testing.T) {
	t.Parallel()

	msgs := []db.Message{
		{
			ID:            1,
			SessionID:     "sess-1",
			Ordinal:       1,
			Role:          "user",
			Content:       "hello",
			SourceType:    "jsonl",
			SourceSubtype: "user",
			PromptSource:  "typed",
		},
	}

	data, err := encodeSegment(msgs)
	require.NoError(t, err)

	var decoded segmentMessage
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.Equal(t, "typed", decoded.PromptSource)

	restored := decoded.dbMessage()
	assert.Equal(t, "typed", restored.PromptSource,
		"import must carry prompt_source back into the db message")
}

func TestCanonicalMetadataEventGolden(t *testing.T) {
	t.Parallel()

	value := jsontext.Value(`{"display_name":"Renamed session"}`)
	note := "remember this"
	event := metadataEvent{
		Version:    metadataEventFormatVersion,
		HLC:        "2026-06-14T010203.000000001Z-laptop-a1b2c3",
		Origin:     "laptop-a1b2c3",
		SessionGID: "desktop-d4e5f6~sess-1",
		Op:         "rename",
		Value:      value,
		Pin: &MetadataPin{
			SourceUUID: "uuid-msg-1",
			Ordinal:    2,
			Note:       &note,
		},
	}

	data, err := canonicalJSON(event)
	require.NoError(t, err)

	assert.Equal(t,
		"{\"hlc\":\"2026-06-14T010203.000000001Z-laptop-a1b2c3\",\"op\":\"rename\",\"origin\":\"laptop-a1b2c3\",\"pin\":{\"note\":\"remember this\",\"ordinal\":2,\"source_uuid\":\"uuid-msg-1\"},\"session_gid\":\"desktop-d4e5f6~sess-1\",\"v\":1,\"value\":{\"display_name\":\"Renamed session\"}}\n",
		string(data),
	)
	assert.Equal(t, "fcb36d602e56fe1616ba6e2f86e973adde4ef547e0ecf280b37eb534b60e4b71", hashHex(data))
}
