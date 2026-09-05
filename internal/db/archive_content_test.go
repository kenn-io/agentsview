package db

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/parser"
)

func TestOpenUsageOnlyPreservesStoredAutomationClassification(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	database, err := OpenWithArchiveContent(path, config.ArchiveContentUsage)
	require.NoError(t, err)

	prompt := "You are a code reviewer. Review the code changes shown below."
	startedAt := "2026-08-31T10:00:00Z"
	require.NoError(t, database.UpsertSession(Session{
		ID: "automated", Project: "project", Agent: "claude", Machine: "local",
		FirstMessage: &prompt, StartedAt: &startedAt, UserMessageCount: 1,
	}))
	require.NoError(t, database.Close())

	reopened, err := OpenWithArchiveContent(path, config.ArchiveContentUsage)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	stored, err := reopened.GetSessionFull(context.Background(), "automated")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.True(t, stored.IsAutomated,
		"startup migrations cannot reclassify discarded transcript text")
	assert.Nil(t, stored.FirstMessage)
}

func TestUsageOnlyUpsertsPreserveAutomationWithoutPreview(t *testing.T) {
	for _, writeKind := range []string{"direct", "identity", "batch"} {
		t.Run(writeKind, func(t *testing.T) {
			for _, tc := range []struct {
				name      string
				policy    config.ArchiveContent
				preview   *string
				userCount int
				want      bool
			}{
				{name: "missing-preview", policy: config.ArchiveContentUsage, userCount: 1, want: true},
				{name: "second-user-turn", policy: config.ArchiveContentUsage, userCount: 2},
				{name: "interactive-preview", policy: config.ArchiveContentUsage, preview: new("explain this function"), userCount: 1},
				{name: "full-policy", policy: config.ArchiveContentFull, userCount: 1},
			} {
				t.Run(tc.name, func(t *testing.T) {
					database := testDB(t)
					database.SetArchiveContent(tc.policy)
					session := Session{ID: "automated", Project: "project", Agent: "claude", Machine: "local",
						FirstMessage: new("You are a code reviewer. Review the code changes shown below."), UserMessageCount: 1}
					require.NoError(t, database.UpsertSession(session))
					stored, err := database.GetSessionFull(context.Background(), session.ID)
					require.NoError(t, err)
					require.NotNil(t, stored)
					require.True(t, stored.IsAutomated)
					session.FirstMessage = tc.preview
					session.UserMessageCount = tc.userCount
					switch writeKind {
					case "direct":
						err = database.UpsertSession(session)
					case "identity":
						err = database.UpsertSessionWithProjectIdentity(session,
							export.ProjectIdentityObservation{SessionID: session.ID, Project: "project", Machine: "local"}, "project")
					case "batch":
						_, err = database.WriteSessionBatch([]SessionBatchWrite{{Session: session}})
					}
					require.NoError(t, err)
					stored, err = database.GetSessionFull(context.Background(), session.ID)
					require.NoError(t, err)
					require.NotNil(t, stored)
					assert.Equal(t, tc.want, stored.IsAutomated)
				})
			}
		})
	}
}

func TestUsageOnlyStoragePolicyOwnsDirectAndBatchWrites(t *testing.T) {
	database := testDB(t)
	database.SetArchiveContent(config.ArchiveContentUsage)
	require.Equal(t, config.ArchiveContentUsage, database.ArchiveContent())

	privateTitle := "private conversation title"
	privatePrompt := "You are a code reviewer. Review the code changes shown below."
	startedAt := "2026-08-31T10:00:00Z"
	session := Session{
		ID: "direct", Project: "project", Agent: "claude", Machine: "local",
		FirstMessage: &privatePrompt, DisplayName: &privateTitle,
		SessionName: &privateTitle, PreserveSessionName: true,
		StartedAt:    &startedAt,
		MessageCount: 4, UserMessageCount: 1,
		SecretLeakCount: 2, SecretsRulesVersion: "private-rules",
		ToolFailureSignalCount: 3, Outcome: "failure",
		QualitySignalVersion: CurrentQualitySignalVersion,
	}
	require.NoError(t, database.UpsertSession(session))
	require.NoError(t, database.ReplaceSessionMessages(session.ID, []Message{
		{SessionID: session.ID, Ordinal: 0, Role: "user", Content: privatePrompt},
		{SessionID: session.ID, Ordinal: 1, Role: "tool", Content: "private tool output"},
		{SessionID: session.ID, Ordinal: 2, Role: "assistant", Model: "model-a", Content: "private response"},
		{SessionID: session.ID, Ordinal: 3, Role: "assistant", Model: "model-a", Content: "private billed response", TokenUsage: []byte(`{"input_tokens":10,"output_tokens":2}`)},
	}))
	require.NoError(t, database.UpdateSessionSignals(
		session.ID, SessionSignalUpdate{
			ToolFailureSignalCount: 4,
			Outcome:                "failure",
			QualitySignals: QualitySignals{
				ShortPromptCount: 3,
			},
		},
	))
	require.NoError(t, database.ReplaceSessionSecretFindings(
		session.ID,
		[]SecretFinding{{
			SessionID: session.ID,
			RuleName:  "private-secret-rule",
		}},
		1,
		"private-rules",
	))

	assertUsageOnlyStoredSession(t, database, session.ID, []int{2, 3})
	replacementTitle := "title added after the initial import"
	require.NoError(t, database.RefreshSessionName(session.ID, &replacementTitle))
	require.NoError(t, database.RenameSession(session.ID, &replacementTitle))
	stored, err := database.GetSessionFull(context.Background(), session.ID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.True(t, stored.IsAutomated,
		"classification must be derived before private prompt text is discarded")
	assert.Nil(t, stored.SessionName)
	assert.Nil(t, stored.DisplayName)

	batchSession := session
	batchSession.ID = "batch"
	batchSession.IsAutomated = true
	result, err := database.WriteSessionBatch([]SessionBatchWrite{{
		Session: batchSession,
		Messages: []Message{
			{SessionID: batchSession.ID, Ordinal: 0, Role: "user", Content: "private batch prompt"},
			{SessionID: batchSession.ID, Ordinal: 1, Role: "assistant", Model: "model-b", Content: "private batch response", TokenUsage: []byte(`{"input_tokens":20,"output_tokens":4}`)},
		},
		ReplaceMessages: true,
	}})
	require.NoError(t, err)
	require.Equal(t, 1, result.WrittenSessions)
	assertUsageOnlyStoredSession(t, database, batchSession.ID, []int{1})

	incrementalSession := session
	incrementalSession.ID = "incremental"
	incrementalSession.FirstMessage = nil
	incrementalSession.IsAutomated = false
	incrementalSession.MessageCount = 0
	incrementalSession.UserMessageCount = 0
	require.NoError(t, database.UpsertSession(incrementalSession))
	require.NoError(t, database.WriteSessionIncremental(
		incrementalSession.ID,
		[]Message{{
			SessionID: incrementalSession.ID, Ordinal: 0,
			Role: "user", Content: privatePrompt,
		}},
		IncrementalSessionUpdate{MsgCount: 1, UserMsgCount: 1},
	))
	assertUsageOnlyStoredSession(t, database, incrementalSession.ID, []int{})
	incrementalStored, err := database.GetSessionFull(
		context.Background(), incrementalSession.ID,
	)
	require.NoError(t, err)
	require.NotNil(t, incrementalStored)
	assert.True(t, incrementalStored.IsAutomated,
		"incremental classification must use text before it is discarded")

	require.NoError(t, database.WriteSessionIncremental(
		incrementalSession.ID,
		[]Message{{
			SessionID: incrementalSession.ID, Ordinal: 1,
			Role: "user", Content: "a second interactive turn",
		}},
		IncrementalSessionUpdate{MsgCount: 2, UserMsgCount: 2},
	))
	incrementalStored, err = database.GetSessionFull(
		context.Background(), incrementalSession.ID,
	)
	require.NoError(t, err)
	require.NotNil(t, incrementalStored)
	assert.False(t, incrementalStored.IsAutomated,
		"a second user turn conclusively demotes text-derived automation")
}

func TestUsageOnlyStoragePreservesContentFreeIncrementalSubagentEdge(
	t *testing.T,
) {
	database := testDB(t)
	database.SetArchiveContent(config.ArchiveContentUsage)

	startedAt := "2026-08-31T10:00:00Z"
	require.NoError(t, database.UpsertSession(Session{
		ID: "parent", Project: "project", Agent: "claude", Machine: "local",
		StartedAt: &startedAt,
	}))
	require.NoError(t, database.UpsertSession(Session{
		ID: "child", Project: "project", Agent: "claude", Machine: "local",
		StartedAt: &startedAt,
	}))

	require.NoError(t, database.WriteSessionIncremental(
		"parent",
		[]Message{{
			SessionID: "parent", Ordinal: 0, Role: "assistant",
			Model: "model-a", Content: "private delegated work",
			HasToolUse: true,
			ToolCalls: []ToolCall{{
				ToolName: "Agent", Category: "Task", ToolUseID: "tool-use-1",
				InputJSON: `{"prompt":"private subagent prompt"}`,
			}},
		}},
		IncrementalSessionUpdate{
			MsgCount: 1,
			SubagentLinks: []ToolCallSubagentLink{{
				ToolUseID: "tool-use-1", SubagentSessionID: "child",
				ResultContent: "private subagent result", ResultContentLen: 23,
				HasResult: true,
			}},
		},
	))
	require.NoError(t, database.LinkSubagentSessions())

	child, err := database.GetSession(context.Background(), "child")
	require.NoError(t, err)
	require.NotNil(t, child)
	require.NotNil(t, child.ParentSessionID)
	assert.Equal(t, "parent", *child.ParentSessionID)
	assert.Equal(t, "subagent", child.RelationshipType)

	messages, err := database.GetAllMessages(context.Background(), "parent")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	require.Len(t, messages[0].ToolCalls, 1)
	call := messages[0].ToolCalls[0]
	assert.Equal(t, "tool-use-1", call.ToolUseID)
	assert.Equal(t, "child", call.SubagentSessionID)
	assert.Equal(t, "subagent", call.ToolName)
	assert.Equal(t, "Task", call.Category)
	assert.Empty(t, call.InputJSON)
	assert.Empty(t, call.SkillName)
	assert.Empty(t, call.ResultContent)
	assert.Zero(t, call.ResultContentLength)
	assert.Empty(t, call.ResultEvents)
}

func assertUsageOnlyStoredSession(
	t *testing.T, database *DB, sessionID string, wantOrdinals []int,
) {
	t.Helper()
	session, err := database.GetSessionFull(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Nil(t, session.FirstMessage)
	assert.Nil(t, session.DisplayName)
	assert.Nil(t, session.SessionName)
	assert.Zero(t, session.SecretLeakCount)
	assert.Empty(t, session.SecretsRulesVersion)
	assert.Zero(t, session.ToolFailureSignalCount)
	assert.Empty(t, session.Outcome)
	assert.Equal(t, CurrentQualitySignalVersion, session.QualitySignalVersion)
	findings, err := database.SessionSecretFindings(
		context.Background(), sessionID,
	)
	require.NoError(t, err)
	assert.Empty(t, findings)

	messages, err := database.GetAllMessages(context.Background(), sessionID)
	require.NoError(t, err)
	ordinals := make([]int, len(messages))
	for index, message := range messages {
		ordinals[index] = message.Ordinal
		assert.Empty(t, message.Content)
		assert.Empty(t, message.ThinkingText)
		assert.Empty(t, message.ToolCalls)
		assert.Empty(t, message.ToolResults)
	}
	assert.Equal(t, wantOrdinals, ordinals)
}

func TestTranscriptsArchiveContentKeepsTextAndDropsToolPayloads(t *testing.T) {
	database := testDB(t)
	database.SetArchiveContent(config.ArchiveContentTranscripts)

	title := "conversation title"
	prompt := "please run the build"
	startedAt := "2026-08-31T10:00:00Z"
	require.NoError(t, database.UpsertSession(Session{
		ID: "transcripts", Project: "project", Agent: "claude",
		Machine: "local", FirstMessage: &prompt, SessionName: &title,
		StartedAt: &startedAt, MessageCount: 2, UserMessageCount: 1,
	}))
	require.NoError(t, database.ReplaceSessionMessages("transcripts", []Message{
		{SessionID: "transcripts", Ordinal: 0, Role: "user", Content: prompt},
		{
			SessionID: "transcripts", Ordinal: 1, Role: "assistant",
			Model: "model-a", HasToolUse: true,
			Content:      "running it now\n[Bash: build it]\n$ make build TOKEN=abc",
			ThinkingText: "the build script is make",
			ToolCalls: []ToolCall{{
				ToolName: "Bash", Category: "Bash", ToolUseID: "tool-use-1",
				InputJSON:           `{"command":"make build TOKEN=abc","description":"build it"}`,
				ResultContent:       "build output line",
				ResultContentLength: 17,
				ResultEvents: []ToolResultEvent{{
					Source: "tool_result", Status: "ok",
					Content: "build output line", ContentLength: 17,
				}},
			}},
		},
	}))

	stored, err := database.GetSessionFull(context.Background(), "transcripts")
	require.NoError(t, err)
	require.NotNil(t, stored)
	require.NotNil(t, stored.FirstMessage)
	assert.Equal(t, prompt, *stored.FirstMessage)
	require.NotNil(t, stored.SessionName)
	assert.Equal(t, title, *stored.SessionName)

	require.NoError(t, database.InsertMessages([]Message{
		{
			SessionID: "transcripts", Ordinal: 2, Role: "tool",
			Content: "standalone tool output", ContentLength: 22,
		},
		{
			SessionID: "transcripts", Ordinal: 3, Role: "user", IsSystem: true,
			SourceSubtype: parser.SourceSubtypeToolResult,
			Content:       "unpaired command output", ContentLength: 23,
		},
		{
			SessionID: "transcripts", Ordinal: 4, Role: "user", IsSystem: true,
			Content: "unmarked command output", ContentLength: 23,
			ToolResults: []ToolResult{{
				ContentRaw: "unmarked command output", ContentLength: 23,
			}},
		},
		{
			SessionID: "transcripts", Ordinal: 5, Role: "user",
			Content: "please also run lint", ContentLength: 20,
			ToolResults: []ToolResult{{
				ToolUseID: "tool-use-1", ContentRaw: "lint output",
				ContentLength: 11,
			}},
		},
		{
			SessionID: "transcripts", Ordinal: 6, Role: "assistant",
			Model: "model-a", HasToolUse: true,
			Content: "checking\n[Bash: list secrets]\n$ ls ~/.ssh",
			ToolCalls: []ToolCall{{
				ToolName: "shell", Category: "Bash", ToolUseID: "tool-use-3",
				InputJSON: `{"command":["ls","~/.ssh"]}`,
				Rendering: "[Bash: list secrets]\n$ ls ~/.ssh",
			}},
		},
	}))

	messages, err := database.GetAllMessages(context.Background(), "transcripts")
	require.NoError(t, err)
	require.Len(t, messages, 7)
	assert.Equal(t, prompt, messages[0].Content)
	assert.Equal(t, "running it now\n[Bash]", messages[1].Content,
		"the inline tool summary keeps its label and loses the command")
	assert.Equal(t, len("running it now\n[Bash]"), messages[1].ContentLength)
	assert.Equal(t, "tool", messages[2].Role)
	assert.Empty(t, messages[2].Content,
		"standalone tool-role rows carry tool output")
	assert.Zero(t, messages[2].ContentLength)
	assert.Equal(t, "user", messages[3].Role)
	assert.Empty(t, messages[3].Content,
		"provider-marked fallback rows carry tool output")
	assert.Zero(t, messages[3].ContentLength)
	assert.Empty(t, messages[4].Content,
		"text identical to a carried tool result is tool output")
	assert.Zero(t, messages[4].ContentLength)
	assert.Equal(t, "please also run lint", messages[5].Content,
		"a user turn that also carries tool results keeps its own text")
	assert.Equal(t, "checking\n[Bash]", messages[6].Content,
		"a parser-provided rendering is replaced even when the input cannot rebuild it")

	require.NoError(t, database.InsertMessages([]Message{{
		SessionID: "transcripts", Ordinal: 8, Role: "assistant",
		Model: "model-a", HasToolUse: true,
		Content: "[Bash]\n$ cat ~/.aws/credentials\nagain\n[Bash]\n$ cat ~/.aws/credentials",
		ToolCalls: []ToolCall{{
			ToolName: "Bash", Category: "Bash", ToolUseID: "tool-use-4",
			InputJSON: `{"command":"cat ~/.aws/credentials"}`,
			Rendering: "[Bash]\n$ cat ~/.aws/credentials",
		}},
	}}))
	messages, err = database.GetAllMessages(context.Background(), "transcripts")
	require.NoError(t, err)
	assert.Equal(t, "[Bash]\nagain\n[Bash]", messages[len(messages)-1].Content,
		"every occurrence of a repeated rendering is replaced")

	// Sanitization strips control bytes from content before storage; the
	// recorded rendering must still match afterwards.
	require.NoError(t, database.InsertMessages([]Message{{
		SessionID: "transcripts", Ordinal: 9, Role: "assistant",
		Model: "model-a", HasToolUse: true,
		Content: "[Bash]\n$ cat \x00~/.netrc",
		ToolCalls: []ToolCall{{
			ToolName: "Bash", Category: "Bash", ToolUseID: "tool-use-5",
			InputJSON: "{\"command\":\"cat \x00~/.netrc\"}",
			Rendering: "[Bash]\n$ cat \x00~/.netrc",
		}},
	}}))
	messages, err = database.GetAllMessages(context.Background(), "transcripts")
	require.NoError(t, err)
	assert.Equal(t, "[Bash]", messages[len(messages)-1].Content,
		"a rendering sanitized alongside its content is still replaced")
	assert.Equal(t, "the build script is make", messages[1].ThinkingText)
	require.Len(t, messages[1].ToolCalls, 1)
	call := messages[1].ToolCalls[0]
	assert.Equal(t, "Bash", call.ToolName)
	assert.Equal(t, "tool-use-1", call.ToolUseID)
	assert.Empty(t, call.InputJSON)
	assert.Empty(t, call.ResultContent)
	assert.Equal(t, 17, call.ResultContentLength)
	require.Len(t, call.ResultEvents, 1)
	assert.Equal(t, "ok", call.ResultEvents[0].Status)
	assert.Empty(t, call.ResultEvents[0].Content)
	assert.Equal(t, 17, call.ResultEvents[0].ContentLength)

	require.NoError(t, database.WriteSessionIncremental(
		"transcripts",
		[]Message{{
			SessionID: "transcripts", Ordinal: 7, Role: "assistant",
			Model: "model-a", Content: "delegating", HasToolUse: true,
			ToolCalls: []ToolCall{{
				ToolName: "Agent", Category: "Task", ToolUseID: "tool-use-2",
				InputJSON: `{"prompt":"private subagent prompt"}`,
			}},
		}},
		IncrementalSessionUpdate{
			MsgCount: 8,
			SubagentLinks: []ToolCallSubagentLink{{
				ToolUseID: "tool-use-2", SubagentSessionID: "child",
				ResultContent: "subagent result", ResultContentLen: 15,
				HasResult: true,
			}},
		},
	))
	messages, err = database.GetAllMessages(context.Background(), "transcripts")
	require.NoError(t, err)
	require.Len(t, messages, 10)
	assert.Equal(t, "delegating", messages[7].Content)
	require.Len(t, messages[7].ToolCalls, 1)
	link := messages[7].ToolCalls[0]
	assert.Equal(t, "child", link.SubagentSessionID)
	assert.Empty(t, link.InputJSON)
	assert.Empty(t, link.ResultContent)
	assert.Equal(t, 15, link.ResultContentLength)
}

// These are persisted row shapes from the provider parsers. Rendering is
// deliberately absent: the archive does not store ParsedToolCall.Rendering.
func TestCopiedTranscriptsDropUnrecoverableToolText(t *testing.T) {
	cases := []struct {
		name, agent string
		version     int
		message     Message
		want        string
	}{
		{
			name: "openhands-terminal-summary", agent: "openhands", version: 100,
			message: Message{Role: "assistant", HasToolUse: true,
				Content:   "Inspecting files.\n\n[Bash: inspect ]\nprivate summary]\n$ cat credentials.txt\n[Thinking]\nafter action\n[/Thinking]",
				ToolCalls: []ToolCall{{ToolName: "terminal", Category: "Bash", InputJSON: `{"command":"cat credentials.txt"}`}},
			},
			want: "Inspecting files.\n\n[Bash]",
		},
		{
			name: "openhands-custom-summary", agent: "openhands", version: 100,
			message: Message{Role: "assistant", HasToolUse: true,
				Content:   "Looking up details.\n\n[lookup: private summary]",
				ToolCalls: []ToolCall{{ToolName: "lookup", Category: "Tool", InputJSON: `{}`}},
			},
			want: "Looking up details.\n\n[lookup]",
		},
		{
			name: "openhands-without-summary", agent: "openhands", version: 100,
			message: Message{Role: "assistant", HasToolUse: true,
				Content:   "Inspecting files.\n\n[Bash]\n$ cat credentials.txt",
				ToolCalls: []ToolCall{{ToolName: "terminal", Category: "Bash", InputJSON: `{"command":"cat credentials.txt"}`}},
			},
			want: "Inspecting files.\n\n[Bash]",
		},
		{
			name: "kimi-glob", agent: "kimi", version: 100,
			message: Message{Role: "assistant", HasToolUse: true,
				Content:   "Finding files.\n[Glob: confidential-pattern]",
				ToolCalls: []ToolCall{{ToolName: "Glob", Category: "Glob", InputJSON: `{"pattern":"confidential-pattern"}`}},
			},
			want: "Finding files.\n[Glob]",
		},
		{
			name: "kimi-work-grep", agent: "kimi-work", version: 100,
			message: Message{Role: "assistant", HasToolUse: true,
				Content:   "Searching text.\n[Grep: confidential-pattern]",
				ToolCalls: []ToolCall{{ToolName: "Grep", Category: "Grep", InputJSON: `{"pattern":"confidential-pattern"}`}},
			},
			want: "Searching text.\n[Grep]",
		},
		{
			name: "kimi-bash", agent: "kimi", version: 100,
			message: Message{Role: "assistant", HasToolUse: true,
				Content:   "Inspecting files.\n[Bash: inspect credentials]\n$ cat credentials.txt",
				ToolCalls: []ToolCall{{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"cat credentials.txt","description":"inspect credentials"}`}},
			},
			want: "Inspecting files.\n[Bash]",
		},
		{
			name: "legacy-codex", agent: "codex", version: 99,
			message: Message{Role: "user", Model: "model-a", Content: "unpaired agent result"},
		},
		{
			name: "legacy-traex", agent: "traex", version: 99,
			message: Message{Role: "user", Model: "model-a", Content: "unpaired agent result"},
		},
		{
			name: "legacy-zencoder", agent: "zencoder", version: 99,
			message: Message{Role: "user", IsSystem: true, Content: "system text from a tool result"},
		},
		{
			name: "legacy-codex-reply", agent: "codex", version: 99,
			message: Message{Role: "assistant", Content: "ordinary reply"}, want: "ordinary reply",
		},
		{
			name: "legacy-traex-reply", agent: "traex", version: 99,
			message: Message{Role: "assistant", Content: "ordinary reply"}, want: "ordinary reply",
		},
		{
			name: "legacy-zencoder-prompt", agent: "zencoder", version: 99,
			message: Message{Role: "user", Content: "ordinary prompt"}, want: "ordinary prompt",
		},
		{
			name: "marked-codex", agent: "codex", version: 100,
			message: Message{Role: "user", Content: "ordinary prompt"}, want: "ordinary prompt",
		},
		{
			name: "marked-traex", agent: "traex", version: 100,
			message: Message{Role: "user", Content: "ordinary prompt"}, want: "ordinary prompt",
		},
		{
			name: "marked-zencoder", agent: "zencoder", version: 100,
			message: Message{Role: "user", IsSystem: true, Content: "ordinary notice"}, want: "ordinary notice",
		},
	}
	for _, trashed := range []bool{false, true} {
		name := "orphaned"
		if trashed {
			name = "trashed"
		}
		t.Run(name, func(t *testing.T) {
			source := testDB(t)
			for _, tc := range cases {
				require.NoError(t, source.UpsertSession(Session{
					ID: tc.name, Project: "project", Agent: tc.agent, Machine: "local",
				}))
				message := tc.message
				message.SessionID = tc.name
				message.ContentLength = len(message.Content)
				require.NoError(t, source.InsertMessages([]Message{message}))
				require.NoError(t, source.SetSessionDataVersion(tc.name, tc.version))
				if trashed {
					require.NoError(t, source.SoftDeleteSession(tc.name))
				}
			}
			destination := testDB(t)
			destination.SetArchiveContent(config.ArchiveContentTranscripts)
			copyData := destination.CopyOrphanedDataFrom
			if trashed {
				copyData = destination.CopyTrashedDataFrom
			}
			copied, err := copyData(source.Path())
			require.NoError(t, err)
			require.Equal(t, len(cases), copied)
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					messages, err := destination.GetAllMessages(context.Background(), tc.name)
					require.NoError(t, err)
					require.Len(t, messages, 1)
					assert.Equal(t, tc.want, messages[0].Content)
					assert.Equal(t, len(tc.want), messages[0].ContentLength)
					for _, call := range messages[0].ToolCalls {
						assert.Empty(t, call.InputJSON)
					}
				})
			}
		})
	}
}

// TestCopyOrphanedDataProjectsArchiveContent covers the resync copy path,
// which reads a full archive with ATTACH and therefore bypasses the
// write-time projection.
func TestCopyOrphanedDataProjectsArchiveContent(t *testing.T) {
	prompt := "please inspect the repository"
	startedAt := "2026-08-31T10:00:00Z"
	seedSource := func(t *testing.T, sourcePath string) {
		t.Helper()
		source := testDBAtPath(t, sourcePath, "source")
		require.NoError(t, source.UpsertSession(Session{
			ID: "archived", Project: "project", Agent: "claude",
			Machine: "local", FirstMessage: &prompt, StartedAt: &startedAt,
			MessageCount: 3, UserMessageCount: 1,
		}))
		require.NoError(t, source.InsertMessages([]Message{
			{SessionID: "archived", Ordinal: 0, Role: "user", Content: prompt},
			{
				SessionID: "archived", Ordinal: 1, Role: "assistant",
				Model: "model-a", HasToolUse: true,
				Content:    "listing files\n[Bash]\n$ ls /private",
				TokenUsage: []byte(`{"input_tokens":10,"output_tokens":2}`),
				ToolCalls: []ToolCall{{
					ToolName: "Bash", Category: "Bash", ToolUseID: "tool-use-1",
					InputJSON: `{"command":"ls /private"}`, ResultContent: "README.md",
					ResultContentLength: 9,
					ResultEvents: []ToolResultEvent{{
						Source: "tool_result", Status: "ok",
						Content: "README.md", ContentLength: 9,
					}},
				}},
			},
			{
				SessionID: "archived", Ordinal: 2, Role: "assistant",
				Model: "model-a", Content: "delegating", HasToolUse: true,
				ToolCalls: []ToolCall{{
					ToolName: "Agent", Category: "Task", ToolUseID: "tool-use-2",
					InputJSON:         `{"prompt":"private delegated prompt"}`,
					SubagentSessionID: "child",
				}},
			},
			{
				SessionID: "archived", Ordinal: 3, Role: "tool",
				Content: "standalone tool output", ContentLength: 22,
			},
			{
				SessionID: "archived", Ordinal: 4, Role: "system", IsSystem: true,
				SourceSubtype: parser.SourceSubtypeToolResult,
				Content:       "unpaired MCP response", ContentLength: 21,
			},
		}))
		require.NoError(t, source.UpdateSessionSignals("archived", SessionSignalUpdate{
			ToolFailureSignalCount: 2,
			QualitySignals:         QualitySignals{Version: CurrentQualitySignalVersion},
		}))
		// Sessions parsed before the tool-output marker existed keep the
		// row shapes their parsers used for tool output. data_version stays
		// 0 for these direct upserts, which is below the marker version.
		for _, legacy := range []struct {
			id, agent string
			messages  []Message
		}{
			{id: "legacy-roo", agent: "roocode", messages: []Message{
				{Ordinal: 0, Role: "user", Content: "roo prompt"},
				{Ordinal: 1, Role: "user", IsSystem: true, Content: "roo command output"},
			}},
			{id: "legacy-gptme", agent: "gptme", messages: []Message{
				{Ordinal: 0, Role: "assistant", Model: "model-a", Content: "gptme reply"},
				{Ordinal: 1, Role: "assistant", Content: "gptme tool output"},
			}},
			{id: "legacy-openhands", agent: "openhands", messages: []Message{
				{Ordinal: 0, Role: "user", Content: "openhands prompt"},
				{Ordinal: 1, Role: "assistant", Content: "openhands reply"},
			}},
			{id: "legacy-aider", agent: "aider", messages: []Message{
				{Ordinal: 0, Role: "user", Content: "aider prompt"},
				{Ordinal: 1, Role: "assistant", Content: "aider reply or tool block"},
			}},
			{id: "marked-roo", agent: "roocode", messages: []Message{
				{Ordinal: 0, Role: "system", IsSystem: true, Content: "roo notice"},
			}},
		} {
			require.NoError(t, source.UpsertSession(Session{
				ID: legacy.id, Project: "project", Agent: legacy.agent,
				Machine: "local", StartedAt: &startedAt,
				MessageCount: len(legacy.messages),
			}))
			for i := range legacy.messages {
				legacy.messages[i].SessionID = legacy.id
			}
			require.NoError(t, source.InsertMessages(legacy.messages))
		}
		_, err := source.getWriter().Exec(
			"UPDATE sessions SET data_version = ? WHERE id = 'marked-roo'",
			toolOutputMarkerDataVersion,
		)
		require.NoError(t, err)
		require.NoError(t, source.ReplaceSessionSecretFindings(
			"archived",
			[]SecretFinding{{SessionID: "archived", RuleName: "aws-access-key"}},
			1, "rules-v1",
		))
		archivedRows, err := source.GetAllMessages(context.Background(), "archived")
		require.NoError(t, err)
		pinNote := "quoted the secret here"
		_, err = source.PinMessage("archived", archivedRows[1].ID, &pinNote)
		require.NoError(t, err)
		require.NoError(t, source.Close())
	}

	t.Run("transcripts", func(t *testing.T) {
		dir := t.TempDir()
		sourcePath := filepath.Join(dir, "source.db")
		seedSource(t, sourcePath)
		destination := testDBAtPath(t, filepath.Join(dir, "dest.db"), "dest")
		t.Cleanup(func() { require.NoError(t, destination.Close()) })
		destination.SetArchiveContent(config.ArchiveContentTranscripts)

		copied, err := destination.CopyOrphanedDataFrom(sourcePath)
		require.NoError(t, err)
		require.Equal(t, 6, copied)

		stored, err := destination.GetSessionFull(context.Background(), "archived")
		require.NoError(t, err)
		require.NotNil(t, stored)
		require.NotNil(t, stored.FirstMessage)
		assert.Equal(t, prompt, *stored.FirstMessage)
		// Signals and findings from the source were computed over payloads
		// this archive does not keep, so the copy clears them and leaves the
		// startup backfill to recompute from the projected rows.
		assert.Zero(t, stored.ToolFailureSignalCount)
		assert.Zero(t, stored.SecretLeakCount)
		assert.Empty(t, stored.SecretsRulesVersion)
		assert.Zero(t, stored.QualitySignalVersion)
		findings, err := destination.SessionSecretFindings(
			context.Background(), "archived",
		)
		require.NoError(t, err)
		assert.Empty(t, findings)

		messages, err := destination.GetAllMessages(context.Background(), "archived")
		require.NoError(t, err)
		require.Len(t, messages, 5)
		assert.Equal(t, prompt, messages[0].Content)
		assert.Equal(t, "listing files\n[Bash]", messages[1].Content,
			"copied inline tool summaries lose their arguments too")
		assert.Equal(t, "tool", messages[3].Role)
		assert.Empty(t, messages[3].Content)
		assert.Zero(t, messages[3].ContentLength)
		assert.Equal(t, "system", messages[4].Role)
		assert.Empty(t, messages[4].Content)
		assert.Zero(t, messages[4].ContentLength)
		require.Len(t, messages[1].ToolCalls, 1)
		call := messages[1].ToolCalls[0]
		assert.Equal(t, "Bash", call.ToolName)
		assert.Empty(t, call.InputJSON)
		assert.Empty(t, call.ResultContent)
		assert.Equal(t, 9, call.ResultContentLength)
		require.Len(t, call.ResultEvents, 1)
		assert.Empty(t, call.ResultEvents[0].Content)
		assert.Equal(t, "ok", call.ResultEvents[0].Status)
		require.Len(t, messages[2].ToolCalls, 1)
		assert.Equal(t, "child", messages[2].ToolCalls[0].SubagentSessionID)
		assert.Empty(t, messages[2].ToolCalls[0].InputJSON)

		contentsOf := func(id string) []string {
			rows, err := destination.GetAllMessages(context.Background(), id)
			require.NoError(t, err)
			contents := make([]string, len(rows))
			for i, row := range rows {
				contents[i] = row.Content
			}
			return contents
		}
		assert.Equal(t, []string{"roo prompt", ""}, contentsOf("legacy-roo"),
			"an unmarked RooCode system-flagged row is tool output")
		assert.Equal(t, []string{"gptme reply", ""}, contentsOf("legacy-gptme"),
			"an unmarked gptme assistant row without a model is tool output")
		assert.Equal(t, []string{"", "openhands reply"}, contentsOf("legacy-openhands"),
			"unmarked OpenHands user rows cannot be told from observations")
		assert.Equal(t, []string{"aider prompt", ""}, contentsOf("legacy-aider"),
			"unmarked Aider assistant rows cannot be told from tool blocks")
		assert.Equal(t, []string{"roo notice"}, contentsOf("marked-roo"),
			"sessions parsed with the marker keep their unmarked notices")
	})

	t.Run("usage", func(t *testing.T) {
		dir := t.TempDir()
		sourcePath := filepath.Join(dir, "source.db")
		seedSource(t, sourcePath)
		destination := testDBAtPath(t, filepath.Join(dir, "dest.db"), "dest")
		t.Cleanup(func() { require.NoError(t, destination.Close()) })
		destination.SetArchiveContent(config.ArchiveContentUsage)

		copied, err := destination.CopyOrphanedDataFrom(sourcePath)
		require.NoError(t, err)
		require.Equal(t, 6, copied)

		stored, err := destination.GetSessionFull(context.Background(), "archived")
		require.NoError(t, err)
		require.NotNil(t, stored)
		assert.Nil(t, stored.FirstMessage)
		assert.Zero(t, stored.SecretLeakCount)
		assert.Empty(t, stored.SecretsRulesVersion)
		assert.Equal(t, CurrentQualitySignalVersion, stored.QualitySignalVersion)
		findings, err := destination.SessionSecretFindings(
			context.Background(), "archived",
		)
		require.NoError(t, err)
		assert.Empty(t, findings)

		messages, err := destination.GetAllMessages(context.Background(), "archived")
		require.NoError(t, err)
		require.Len(t, messages, 2)
		assert.Equal(t, []int{1, 2}, []int{messages[0].Ordinal, messages[1].Ordinal})
		assert.Empty(t, messages[0].Content)
		assert.Empty(t, messages[1].Content)
		pins, err := destination.ListPinnedMessages(context.Background(), "archived", "")
		require.NoError(t, err)
		require.Len(t, pins, 1)
		assert.Nil(t, pins[0].Note, "copied pins lose their notes on a usage archive")
		assert.JSONEq(t, `{"input_tokens":10,"output_tokens":2}`,
			string(messages[0].TokenUsage))
		assert.False(t, messages[0].HasToolUse)
		assert.True(t, messages[1].HasToolUse)
		require.Len(t, messages[1].ToolCalls, 1)
		call := messages[1].ToolCalls[0]
		assert.Equal(t, "subagent", call.ToolName)
		assert.Equal(t, "Task", call.Category)
		assert.Equal(t, "tool-use-2", call.ToolUseID)
		assert.Equal(t, "child", call.SubagentSessionID)
		assert.Empty(t, call.InputJSON)
		assert.Empty(t, call.ResultEvents)
	})
}

func TestUsageArchiveRefusesDerivedText(t *testing.T) {
	database := testDB(t)
	database.SetArchiveContent(config.ArchiveContentUsage)

	_, err := database.InsertInsight(Insight{
		Type: "daily", DateFrom: "2026-08-31", DateTo: "2026-08-31",
		Agent: "claude", Content: "summary quoting transcript text",
	})
	require.ErrorIs(t, err, ErrArchiveContentExcluded)

	entry := RecallEntry{
		ID: "entry-1", Type: "fact", Scope: "project", Status: "accepted",
		Title: "build tool", Body: "the build tool is missing",
		Project: "project", Agent: "claude", SourceSessionID: "session",
	}
	_, err = database.InsertRecallEntry(entry)
	require.ErrorIs(t, err, ErrArchiveContentExcluded)
	_, err = database.InsertExtractedRecallEntries(
		context.Background(), []RecallEntry{entry},
	)
	require.ErrorIs(t, err, ErrArchiveContentExcluded)
	_, err = database.ImportAcceptedRecallEntriesJSONL(
		context.Background(), strings.NewReader(""),
	)
	require.ErrorIs(t, err, ErrArchiveContentExcluded)

	_, err = database.CommitExtractedUnit(
		context.Background(), ExtractUnitCommit{},
	)
	require.ErrorIs(t, err, ErrArchiveContentExcluded)
	_, err = database.IngestEvalTrajectory(
		context.Background(), EvalTrajectoryIngest{},
	)
	require.ErrorIs(t, err, ErrArchiveContentExcluded)
	_, err = database.RecordRecallQueryEvent(
		context.Background(), RecallQueryEvent{Query: "private query"},
	)
	require.ErrorIs(t, err, ErrArchiveContentExcluded)
	sourcePath := filepath.Join(t.TempDir(), "source.db")
	require.NoError(t, testDBAtPath(t, sourcePath, "source").Close())
	require.ErrorIs(t, database.CopyInsightsFrom(sourcePath),
		ErrArchiveContentExcluded)
	require.ErrorIs(t, database.CopyRecallEntriesFrom(sourcePath),
		ErrArchiveContentExcluded)

	insights, err := database.ListInsights(context.Background(), InsightFilter{})
	require.NoError(t, err)
	assert.Empty(t, insights)

	startedAt := "2026-08-31T10:00:00Z"
	require.NoError(t, database.UpsertSession(Session{
		ID: "pinned", Project: "project", Agent: "claude", Machine: "local",
		StartedAt: &startedAt, MessageCount: 1,
	}))
	require.NoError(t, database.InsertMessages([]Message{{
		SessionID: "pinned", Ordinal: 0, Role: "assistant", Model: "model-a",
	}}))
	pinnable, err := database.GetAllMessages(context.Background(), "pinned")
	require.NoError(t, err)
	require.Len(t, pinnable, 1)
	note := "the token was sk-live-123"
	_, err = database.PinMessage("pinned", pinnable[0].ID, &note)
	require.NoError(t, err)
	pins, err := database.ListPinnedMessages(context.Background(), "pinned", "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	assert.Nil(t, pins[0].Note, "a usage archive keeps the pin but not its text")

	transcripts := testDB(t)
	transcripts.SetArchiveContent(config.ArchiveContentTranscripts)
	_, err = transcripts.InsertInsight(Insight{
		Type: "daily", DateFrom: "2026-08-31", DateTo: "2026-08-31",
		Agent: "claude", Content: "summary",
	})
	require.NoError(t, err, "the transcripts policy keeps insights")
}

// TestUsageArchiveClearsTextLeftByAnEarlierPolicy covers an archive that held
// full content before archive_content = "usage" was set and has not been
// rebuilt yet: every write path must drop the titles and pin notes the rows
// still carry, not only the fields the write itself supplies.
func TestUsageArchiveClearsTextLeftByAnEarlierPolicy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.db")
	full := testDBAtPath(t, path, "full")
	title := "private title"
	prompt := "private prompt"
	startedAt := "2026-08-31T10:00:00Z"
	seed := func(id string) {
		require.NoError(t, full.UpsertSession(Session{
			ID: id, Project: "project", Agent: "claude", Machine: "local",
			FirstMessage: &prompt, DisplayName: &title, SessionName: &title,
			StartedAt: &startedAt, MessageCount: 1,
		}))
		require.NoError(t, full.InsertMessages([]Message{{
			SessionID: id, Ordinal: 0, Role: "assistant", Model: "model-a",
			Content: "reply",
		}}))
		rows, err := full.GetAllMessages(context.Background(), id)
		require.NoError(t, err)
		note := "quoted secret"
		_, err = full.PinMessage(id, rows[0].ID, &note)
		require.NoError(t, err)
	}
	seed("upsert")
	seed("replace")
	seed("incremental")
	seed("batch")
	seed("identity")
	seed("rename")
	require.NoError(t, full.UpdateSessionSignals("upsert", SessionSignalUpdate{
		ToolFailureSignalCount: 3, Outcome: "failure",
		QualitySignals: QualitySignals{Version: CurrentQualitySignalVersion},
	}))
	require.NoError(t, full.ReplaceSessionSecretFindings("upsert",
		[]SecretFinding{{SessionID: "upsert", RuleName: "aws-access-key"}},
		1, "rules-v1",
	))
	require.NoError(t, full.Close())

	database, err := OpenWithArchiveContent(path, config.ArchiveContentUsage)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })

	require.NoError(t, database.UpsertSession(Session{
		ID: "upsert", Project: "project", Agent: "claude", Machine: "local",
		StartedAt: &startedAt, MessageCount: 1,
	}))
	require.NoError(t, database.ReplaceSessionMessages("replace", []Message{{
		SessionID: "replace", Ordinal: 0, Role: "assistant", Model: "model-a",
		Content: "reply",
	}}))
	require.NoError(t, database.WriteSessionIncremental("incremental",
		[]Message{{
			SessionID: "incremental", Ordinal: 1, Role: "assistant",
			Model: "model-a", Content: "more",
		}},
		IncrementalSessionUpdate{MsgCount: 2},
	))
	_, err = database.WriteSessionBatch([]SessionBatchWrite{{
		Session: Session{
			ID: "batch", Project: "project", Agent: "claude", Machine: "local",
			StartedAt: &startedAt, MessageCount: 1,
		},
		Messages: []Message{{
			SessionID: "batch", Ordinal: 0, Role: "assistant", Model: "model-a",
			Content: "reply",
		}},
		ReplaceMessages: true,
	}})
	require.NoError(t, err)
	require.NoError(t, database.UpsertSessionWithProjectIdentity(
		Session{
			ID: "identity", Project: "project", Agent: "claude",
			Machine: "local", StartedAt: &startedAt, MessageCount: 1,
		},
		export.ProjectIdentityObservation{
			SessionID: "identity", Project: "project", Machine: "local",
		},
		"project",
	))
	require.NoError(t, database.RefreshSessionName("rename", &title))

	upserted, err := database.GetSessionFull(context.Background(), "upsert")
	require.NoError(t, err)
	require.NotNil(t, upserted)
	assert.Zero(t, upserted.ToolFailureSignalCount)
	assert.Empty(t, upserted.Outcome)
	assert.Zero(t, upserted.SecretLeakCount)
	findings, err := database.SessionSecretFindings(context.Background(), "upsert")
	require.NoError(t, err)
	assert.Empty(t, findings, "an upsert settles signals the row carried")

	for _, id := range []string{
		"upsert", "replace", "incremental", "batch", "identity", "rename",
	} {
		stored, err := database.GetSessionFull(context.Background(), id)
		require.NoError(t, err)
		require.NotNil(t, stored, id)
		assert.Nil(t, stored.FirstMessage, id)
		assert.Nil(t, stored.DisplayName, id)
		assert.Nil(t, stored.SessionName, id)
		pins, err := database.ListPinnedMessages(context.Background(), id, "")
		require.NoError(t, err)
		for _, pin := range pins {
			assert.Nil(t, pin.Note, id)
		}
	}
}
