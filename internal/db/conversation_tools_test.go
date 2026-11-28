package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
)

// toolSummaryChanges walks to the checkpoint in pages of limit and returns the
// tool summary changes by session, failing on a session listed twice.
func toolSummaryChanges(t *testing.T, d *DB, opts ConversationExportOptions) (map[string]ConversationChange, []ConversationChange, string) {
	t.Helper()
	summaries := map[string]ConversationChange{}
	var others []ConversationChange
	for {
		page, err := d.ExportConversationChanges(t.Context(), opts)
		require.NoError(t, err)
		if opts.Limit > 0 {
			require.LessOrEqual(t, len(page.Changes), opts.Limit, "summaries count toward the page limit")
		}
		for _, change := range page.Changes {
			if change.Type != "tool_summary" {
				others = append(others, change)
				continue
			}
			require.NotContains(t, summaries, change.SessionID, "one walk lists a session's summary once")
			summaries[change.SessionID] = change
		}
		if page.Checkpoint != "" {
			return summaries, others, page.Checkpoint
		}
		require.NotEmpty(t, page.NextCursor)
		opts.Checkpoint, opts.Cursor = "", page.NextCursor
	}
}

func toolSession(t *testing.T, d *DB, id string, calls ...ToolCall) {
	t.Helper()
	started, ended := "2026-03-01T10:00:00Z", "2026-03-01T11:00:00Z"
	require.NoError(t, d.UpsertSession(t.Context(), Session{
		ID: id, Project: "sample", Machine: "local", Agent: "claude", StartedAt: &started, EndedAt: &ended,
	}))
	require.NoError(t, d.InsertMessages(t.Context(), []Message{
		{SessionID: id, Ordinal: 0, Role: "user", Content: "Run the checks.", SourceUUID: id + "-ask", Timestamp: started},
		{
			SessionID: id, Ordinal: 1, Role: "assistant", Content: "Running them.", SourceUUID: id + "-reply",
			Timestamp: "2026-03-01T10:00:05Z", HasToolUse: true, ToolCalls: calls,
		},
	}))
}

func timedCall(category, toolUseID, started, completed string) ToolCall {
	return ToolCall{ToolName: category, Category: category, ToolUseID: toolUseID, ResultEvents: []ToolResultEvent{
		{ToolUseID: toolUseID, Source: "tool_execution", Status: "started", Timestamp: started, EventIndex: 0},
		{ToolUseID: toolUseID, Source: "tool_execution", Status: "completed", Timestamp: completed, EventIndex: 1},
	}}
}

func TestConversationExportToolSummaryTotalsStoredCalls(t *testing.T) {
	d := testDB(t)
	toolSession(t, d, "chat",
		// Two overlapping commands: 10:01:00-10:01:40 and 10:01:20-10:02:00.
		timedCall("Bash", "run-1", "2026-03-01T10:01:00Z", "2026-03-01T10:01:40Z"),
		timedCall("Bash", "run-2", "2026-03-01T10:01:20Z", "2026-03-01T10:02:00Z"),
		ToolCall{ToolName: "Bash", Category: "Bash", ToolUseID: "run-3"},
		timedCall("Read", "read-1", "2026-03-01T10:01:50Z", "2026-03-01T10:02:10Z"),
		ToolCall{ToolName: "Skill", Category: "Skill", ToolUseID: "skill-1", SkillName: " review-change "},
		ToolCall{ToolName: "Skill", Category: "Skill", ToolUseID: "skill-2", SkillName: "review-change"},
		ToolCall{ToolName: "Skill", Category: "Skill", ToolUseID: "skill-3", SkillName: "commit"},
		ToolCall{ToolName: "Skill", Category: "Skill", ToolUseID: "skill-4", SkillName: "  "},
	)
	// Sync pays nothing for the journal until the archive's first export.
	var journaled int
	require.NoError(t, d.getReader().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM conversation_tool_changes`).Scan(&journaled))
	require.Zero(t, journaled)
	require.NoError(t, d.UpdateSessionSignals(t.Context(), "chat", SessionSignalUpdate{ToolObservations: []ToolObservation{
		{MessageOrdinal: 1, CallIndex: 0, Outcome: "errored", Repeat: "none"},
		{MessageOrdinal: 1, CallIndex: 1, Outcome: "content", Repeat: "none"},
		{MessageOrdinal: 1, CallIndex: 2, Outcome: "unknown", Repeat: "none"},
		{MessageOrdinal: 1, CallIndex: 3, Outcome: "empty", Repeat: "none"},
	}}))

	summaries, others, _ := toolSummaryChanges(t, d, ConversationExportOptions{ToolSummaries: true})
	require.Contains(t, summaries, "chat")
	change := summaries["chat"]
	assert.False(t, change.Deleted)
	assert.Empty(t, change.Gap)
	assert.Equal(t, "sample", change.Project.DisplayLabel)
	require.NotNil(t, change.ToolSummary)
	assert.Equal(t, ConversationToolSummary{
		// 10:01:00-10:02:10 once the three timed calls are merged.
		DurationMs: 70_000,
		Categories: []ConversationToolCategory{
			{Category: "Bash", Calls: 3, Observed: 2, Errored: 1, Timed: 2, DurationMs: 60_000},
			{Category: "Read", Calls: 1, Observed: 1, Errored: 0, Timed: 1, DurationMs: 20_000},
			{Category: "Skill", Calls: 4, Observed: 0, Errored: 0, Timed: 0, DurationMs: 0},
		},
		Skills: []ConversationSkillUse{{Name: "commit", Calls: 1}, {Name: "review-change", Calls: 2}},
	}, *change.ToolSummary)
	assert.Len(t, others, 2, "the two messages are still listed")

	// Summaries have their own revisions, so asking for them changes nothing
	// about the message and session changes or their revisions.
	withSummaries, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{ToolSummaries: true})
	require.NoError(t, err)

	plain, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{})
	require.NoError(t, err)
	require.Len(t, plain.Changes, 2)
	assert.Equal(t, plain.Changes, withSummaries.Changes[:2], "summaries follow the unchanged listing and appear only on request")
}

func TestConversationExportToolSummaryCatchesUpAfterWalkWithoutIt(t *testing.T) {
	d := testDB(t)
	toolSession(t, d, "first", ToolCall{ToolName: "Bash", Category: "Bash", ToolUseID: "a"})
	toolSession(t, d, "second", ToolCall{ToolName: "Read", Category: "Read", ToolUseID: "b"})

	// A consumer that never asked has walked past every summary.
	_, messages, withoutSummaries := toolSummaryChanges(t, d, ConversationExportOptions{})
	require.Len(t, messages, 4)

	// One new message arrives before it starts asking.
	require.NoError(t, d.InsertMessages(t.Context(), []Message{
		{SessionID: "second", Ordinal: 2, Role: "user", Content: "And again.", SourceUUID: "second-again"},
	}))

	summaries, others, withSummaries := toolSummaryChanges(t, d,
		ConversationExportOptions{ToolSummaries: true, Checkpoint: withoutSummaries, Limit: 1})
	assert.Len(t, summaries, 2, "every session's summary is listed, including the unchanged one")
	require.Contains(t, summaries, "first")
	require.NotNil(t, summaries["first"].ToolSummary)
	assert.Equal(t, []ConversationToolCategory{{Category: "Bash", Calls: 1}}, summaries["first"].ToolSummary.Categories)
	require.Len(t, others, 1, "messages already walked are not listed again")
	assert.Equal(t, "message", others[0].Type)
	assert.Equal(t, 2, others[0].Ordinal)

	summaries, others, _ = toolSummaryChanges(t, d, ConversationExportOptions{ToolSummaries: true, Checkpoint: withSummaries})
	assert.Empty(t, summaries, "a caught-up walk lists nothing until a session changes")
	assert.Empty(t, others)

	// Changes that touch no tool call still page to the end of the walk.
	started, ended := "2026-03-01T10:00:00Z", "2026-03-01T11:00:00Z"
	for _, id := range []string{"first", "second"} {
		require.NoError(t, d.UpsertSession(t.Context(), Session{
			ID: id, Project: "renamed", Machine: "local", Agent: "claude", StartedAt: &started, EndedAt: &ended,
		}))
	}
	summaries, others, withSummaries = toolSummaryChanges(t, d,
		ConversationExportOptions{ToolSummaries: true, Checkpoint: withSummaries, Limit: 1})
	assert.Empty(t, summaries)
	require.Len(t, others, 2)
	assert.Equal(t, []string{"session", "session"}, []string{others[0].Type, others[1].Type})

	// Walking without summaries again gives up the place in them.
	_, _, withoutSummaries = toolSummaryChanges(t, d, ConversationExportOptions{Checkpoint: withSummaries})
	summaries, _, _ = toolSummaryChanges(t, d, ConversationExportOptions{ToolSummaries: true, Checkpoint: withoutSummaries})
	assert.Len(t, summaries, 2)
}

func TestConversationExportToolSummaryFollowsToolCallChanges(t *testing.T) {
	bash := func(t *testing.T, d *DB, checkpoint string) (ConversationToolCategory, string) {
		t.Helper()
		summaries, _, next := toolSummaryChanges(t, d, ConversationExportOptions{ToolSummaries: true, Checkpoint: checkpoint})
		require.Contains(t, summaries, "chat", "the changed session is listed again")
		require.NotContains(t, summaries, "idle", "an unchanged session is not")
		require.NotNil(t, summaries["chat"].ToolSummary)
		require.Len(t, summaries["chat"].ToolSummary.Categories, 1)
		return summaries["chat"].ToolSummary.Categories[0], next
	}
	tests := []struct {
		name   string
		change func(t *testing.T, d *DB)
		want   ConversationToolCategory
	}{
		{"a later message adds a call", func(t *testing.T, d *DB) {
			t.Helper()
			require.NoError(t, d.InsertMessages(t.Context(), []Message{{
				SessionID: "chat", Ordinal: 2, Role: "assistant", Content: "Once more.", SourceUUID: "chat-more", HasToolUse: true,
				ToolCalls: []ToolCall{{ToolName: "Bash", Category: "Bash", ToolUseID: "c"}},
			}}))
		}, ConversationToolCategory{Category: "Bash", Calls: 3}},
		{"a call's result is classified after the write", func(t *testing.T, d *DB) {
			t.Helper()
			require.NoError(t, d.UpdateSessionSignals(t.Context(), "chat", SessionSignalUpdate{ToolObservations: []ToolObservation{
				{MessageOrdinal: 1, CallIndex: 0, Outcome: "errored", Repeat: "none"},
			}}))
		}, ConversationToolCategory{Category: "Bash", Calls: 2, Observed: 1, Errored: 1}},
		{"a rewrite drops a call", func(t *testing.T, d *DB) {
			t.Helper()
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), "chat", []Message{
				{SessionID: "chat", Ordinal: 0, Role: "user", Content: "Run the checks.", SourceUUID: "chat-ask"},
				{
					SessionID: "chat", Ordinal: 1, Role: "assistant", Content: "Running them.", SourceUUID: "chat-reply", HasToolUse: true,
					ToolCalls: []ToolCall{{ToolName: "Bash", Category: "Bash", ToolUseID: "a"}},
				},
			}))
		}, ConversationToolCategory{Category: "Bash", Calls: 1}},
		// Every row gains parser metadata, so the rows are stored again with
		// their classification although the transcript is the same.
		{"a rewrite of the same transcript classifies a result", func(t *testing.T, d *DB) {
			t.Helper()
			require.NoError(t, d.ReplaceSessionContent(t.Context(), "chat", []Message{
				{
					SessionID: "chat", Ordinal: 0, Role: "user", Content: "Run the checks.", SourceUUID: "chat-ask",
					Timestamp: "2026-03-01T10:00:00Z", SourceParentUUID: "root",
				},
				{
					SessionID: "chat", Ordinal: 1, Role: "assistant", Content: "Running them.", SourceUUID: "chat-reply",
					Timestamp: "2026-03-01T10:00:05Z", SourceParentUUID: "chat-ask", HasToolUse: true,
					ToolCalls: []ToolCall{
						{ToolName: "Bash", Category: "Bash", ToolUseID: "a"},
						{ToolName: "Bash", Category: "Bash", ToolUseID: "b"},
					},
				},
			}, SessionSignalUpdate{ToolObservations: []ToolObservation{
				{MessageOrdinal: 1, CallIndex: 1, Outcome: "errored", Repeat: "none"},
			}}, nil))
			var revision string
			require.NoError(t, d.getReader().QueryRowContext(t.Context(), `SELECT transcript_revision FROM sessions WHERE id='chat'`).Scan(&revision))
			require.Equal(t, "1", revision, "the rewrite left the transcript revision alone")
		}, ConversationToolCategory{Category: "Bash", Calls: 2, Observed: 1, Errored: 1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			d := testDB(t)
			toolSession(t, d, "chat",
				ToolCall{ToolName: "Bash", Category: "Bash", ToolUseID: "a"},
				ToolCall{ToolName: "Bash", Category: "Bash", ToolUseID: "b"})
			toolSession(t, d, "idle", ToolCall{ToolName: "Read", Category: "Read", ToolUseID: "r"})
			_, _, checkpoint := toolSummaryChanges(t, d, ConversationExportOptions{ToolSummaries: true})

			test.change(t, d)
			got, _ := bash(t, d, checkpoint)
			assert.Equal(t, test.want, got)
		})
	}
}

// A rewrite that stores the same transcript again rebuilds the rows of the
// tool calls it touches from the parsed messages, which carry no
// classification unless the write recomputes it. The summary must follow.
func TestConversationExportToolSummaryFollowsClassificationDroppedByRewrite(t *testing.T) {
	// Changing parser metadata on one of two rows updates that row in place;
	// changing it on both replaces every row.
	rewrite := func(both bool) []Message {
		askParent := ""
		if both {
			askParent = "root"
		}
		return []Message{
			{
				SessionID: "chat", Ordinal: 0, Role: "user", Content: "Run the checks.", SourceUUID: "chat-ask",
				Timestamp: "2026-03-01T10:00:00Z", SourceParentUUID: askParent,
			},
			{
				SessionID: "chat", Ordinal: 1, Role: "assistant", Content: "Running them.", SourceUUID: "chat-reply",
				Timestamp: "2026-03-01T10:00:05Z", SourceParentUUID: "chat-ask", HasToolUse: true,
				ToolCalls: []ToolCall{
					{ToolName: "Bash", Category: "Bash", ToolUseID: "a"},
					{ToolName: "Bash", Category: "Bash", ToolUseID: "b"},
				},
			},
		}
	}
	tests := []struct {
		name    string
		rewrite func(t *testing.T, d *DB)
	}{
		{"messages only, one row updated in place", func(t *testing.T, d *DB) {
			t.Helper()
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), "chat", rewrite(false)))
		}},
		{"messages only, every row replaced", func(t *testing.T, d *DB) {
			t.Helper()
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), "chat", rewrite(true)))
		}},
		{"content without recomputed outcomes, one row updated in place", func(t *testing.T, d *DB) {
			t.Helper()
			require.NoError(t, d.ReplaceSessionContent(t.Context(), "chat", rewrite(false), SessionSignalUpdate{}, nil))
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			d := testDB(t)
			toolSession(t, d, "chat",
				ToolCall{ToolName: "Bash", Category: "Bash", ToolUseID: "a"},
				ToolCall{ToolName: "Bash", Category: "Bash", ToolUseID: "b"})
			toolSession(t, d, "idle", ToolCall{ToolName: "Read", Category: "Read", ToolUseID: "r"})
			require.NoError(t, d.UpdateSessionSignals(t.Context(), "chat", SessionSignalUpdate{ToolObservations: []ToolObservation{
				{MessageOrdinal: 1, CallIndex: 0, Outcome: "errored", Repeat: "none"},
			}}))
			summaries, _, checkpoint := toolSummaryChanges(t, d, ConversationExportOptions{ToolSummaries: true})
			require.Equal(t, []ConversationToolCategory{{Category: "Bash", Calls: 2, Observed: 1, Errored: 1}},
				summaries["chat"].ToolSummary.Categories)

			test.rewrite(t, d)
			var revision string
			require.NoError(t, d.getReader().QueryRowContext(t.Context(), `SELECT transcript_revision FROM sessions WHERE id='chat'`).Scan(&revision))
			require.Equal(t, "1", revision, "the rewrite left the transcript revision alone")

			summaries, _, _ = toolSummaryChanges(t, d, ConversationExportOptions{ToolSummaries: true, Checkpoint: checkpoint})
			require.Contains(t, summaries, "chat", "the rewritten session is listed again")
			assert.NotContains(t, summaries, "idle")
			assert.Equal(t, []ConversationToolCategory{{Category: "Bash", Calls: 2}}, summaries["chat"].ToolSummary.Categories)
		})
	}
}

func TestConversationExportToolSummaryIsUnavailableUnderUsagePolicy(t *testing.T) {
	metadata := func(t *testing.T, d *DB) {
		t.Helper()
		started, ended := "2026-03-01T10:00:00Z", "2026-03-01T11:00:00Z"
		require.NoError(t, d.UpsertSession(t.Context(), Session{
			ID: "chat", Project: "sample", Machine: "local", Agent: "claude", StartedAt: &started, EndedAt: &ended,
		}))
	}
	tests := []struct {
		name string
		// restored gives the session a change row before the policy applies.
		restored bool
		write    func(t *testing.T, d *DB)
	}{
		{"the session's messages are stored again", false, func(t *testing.T, d *DB) {
			t.Helper()
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), "chat", []Message{
				{SessionID: "chat", Ordinal: 0, Role: "user", Content: "Run the checks.", SourceUUID: "chat-ask"},
				{
					SessionID: "chat", Ordinal: 1, Role: "assistant", Content: "Running them.", SourceUUID: "chat-reply", HasToolUse: true,
					ToolCalls: []ToolCall{{ToolName: "Bash", Category: "Bash", ToolUseID: "a"}},
				},
			}))
		}},
		{"only the session's metadata is stored again", false, metadata},
		{"only the metadata of a once trashed session is stored again", true, metadata},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			d := testDB(t)
			toolSession(t, d, "chat", ToolCall{ToolName: "Bash", Category: "Bash", ToolUseID: "a"})
			if test.restored {
				require.NoError(t, d.SoftDeleteSession(t.Context(), "chat"))
				_, err := d.RestoreSession(t.Context(), "chat")
				require.NoError(t, err)
			}
			summaries, _, checkpoint := toolSummaryChanges(t, d, ConversationExportOptions{ToolSummaries: true})
			require.Contains(t, summaries, "chat")
			require.NotNil(t, summaries["chat"].ToolSummary)

			d.SetArchiveContent(config.ArchiveContentUsage)
			test.write(t, d)

			summaries, _, _ = toolSummaryChanges(t, d, ConversationExportOptions{ToolSummaries: true, Checkpoint: checkpoint})
			require.Contains(t, summaries, "chat")
			assert.Equal(t, "archive_content_excluded", summaries["chat"].Gap)
			assert.Nil(t, summaries["chat"].ToolSummary, "calls the policy excludes are not a count of zero")
		})
	}
}

func TestConversationExportToolSummaryFollowsMeasuredTime(t *testing.T) {
	d := testDB(t)
	toolSession(t, d, "chat",
		timedCall("Bash", "a", "2026-03-01T10:00:10Z", "2026-03-01T10:01:10Z"),
		ToolCall{ToolName: "Task", Category: "Task", ToolUseID: "t", SubagentSessionID: "helper"})
	// A second session delegates to the same subagent session.
	toolSession(t, d, "other", ToolCall{ToolName: "Task", Category: "Task", ToolUseID: "o", SubagentSessionID: "helper"})
	summaries, _, checkpoint := toolSummaryChanges(t, d, ConversationExportOptions{ToolSummaries: true})
	require.Contains(t, summaries, "chat")
	require.Equal(t, []ConversationToolCategory{
		{Category: "Bash", Calls: 1, Timed: 1, DurationMs: 60_000},
		{Category: "Task", Calls: 1},
	}, summaries["chat"].ToolSummary.Categories)

	session := func(id, started, ended string) {
		require.NoError(t, d.UpsertSession(t.Context(), Session{
			ID: id, Project: "sample", Machine: "local", Agent: "claude", StartedAt: &started, EndedAt: &ended,
		}))
	}
	// None of these steps writes a message or a tool call of the session.
	steps := []struct {
		name     string
		subagent bool
		change   func()
		want     []ConversationToolCategory
	}{
		{"the subagent session it delegated to arrives", true, func() {
			session("helper", "2026-03-01T10:02:00Z", "2026-03-01T10:07:00Z")
		}, []ConversationToolCategory{
			{Category: "Bash", Calls: 1, Timed: 1, DurationMs: 60_000},
			{Category: "Task", Calls: 1, Timed: 1, DurationMs: 300_000},
		}},
		{"the subagent session runs longer", true, func() {
			session("helper", "2026-03-01T10:02:00Z", "2026-03-01T10:12:00Z")
		}, []ConversationToolCategory{
			{Category: "Bash", Calls: 1, Timed: 1, DurationMs: 60_000},
			{Category: "Task", Calls: 1, Timed: 1, DurationMs: 600_000},
		}},
		{"the subagent session is deleted", true, func() {
			require.NoError(t, d.SoftDeleteSession(t.Context(), "helper"))
			require.NoError(t, d.DeleteSession(t.Context(), "helper"))
		}, []ConversationToolCategory{
			{Category: "Bash", Calls: 1, Timed: 1, DurationMs: 60_000},
			{Category: "Task", Calls: 1},
		}},
		{"the session's own end moves into a measured call", false, func() {
			session("chat", "2026-03-01T10:00:00Z", "2026-03-01T10:00:40Z")
		}, []ConversationToolCategory{
			{Category: "Bash", Calls: 1, Timed: 1, DurationMs: 30_000},
			{Category: "Task", Calls: 1},
		}},
	}
	for _, step := range steps {
		step.change()
		summaries, _, checkpoint = toolSummaryChanges(t, d, ConversationExportOptions{ToolSummaries: true, Checkpoint: checkpoint, Limit: 1})
		require.Contains(t, summaries, "chat", step.name)
		require.NotNil(t, summaries["chat"].ToolSummary, step.name)
		assert.Equal(t, step.want, summaries["chat"].ToolSummary.Categories, step.name)
		assert.Equal(t, step.subagent, summaries["other"].ToolSummary != nil, "%s: every delegating session is listed", step.name)
	}
}

func TestConversationExportToolSummaryAfterRebuildListsEverySession(t *testing.T) {
	source := testDB(t)
	toolSession(t, source, "one", ToolCall{ToolName: "Bash", Category: "Bash", ToolUseID: "a"})
	_, _, _ = toolSummaryChanges(t, source, ConversationExportOptions{ToolSummaries: true})

	// The rebuilt archive stores more sessions than the one it replaces, then
	// takes over that archive's metadata.
	rebuilt := testDB(t)
	require.NoError(t, rebuilt.CopyArchiveIdentityFrom(t.Context(), source.Path()))
	for _, id := range []string{"one", "two", "three"} {
		toolSession(t, rebuilt, id, ToolCall{ToolName: "Bash", Category: "Bash", ToolUseID: "a"})
	}
	require.NoError(t, rebuilt.CopySessionMetadataFrom(t.Context(), source.Path()))

	summaries, _, _ := toolSummaryChanges(t, rebuilt, ConversationExportOptions{ToolSummaries: true})
	assert.Len(t, summaries, 3)
}

func TestConversationExportToolSummaryOfCopiedSession(t *testing.T) {
	source := testDB(t)
	toolSession(t, source, "copied", ToolCall{ToolName: "Bash", Category: "Bash", ToolUseID: "a"})
	_, err := source.ExportConversationChanges(t.Context(), ConversationExportOptions{})
	require.NoError(t, err)

	// A rebuilt archive is exporting from the start and copies sessions whose
	// source files are gone.
	destination := testDB(t)
	require.NoError(t, destination.CopyArchiveIdentityFrom(t.Context(), source.Path()))
	_, err = destination.CopyOrphanedDataFrom(t.Context(), source.Path())
	require.NoError(t, err)

	summaries, _, _ := toolSummaryChanges(t, destination, ConversationExportOptions{ToolSummaries: true})
	require.Contains(t, summaries, "copied")
	require.NotNil(t, summaries["copied"].ToolSummary)
	assert.Equal(t, []ConversationToolCategory{{Category: "Bash", Calls: 1}}, summaries["copied"].ToolSummary.Categories)
}

func TestConversationExportToolSummaryOnArchiveFromOlderVersion(t *testing.T) {
	d := testDB(t)
	toolSession(t, d, "chat", ToolCall{ToolName: "Bash", Category: "Bash", ToolUseID: "a"})
	_, _, checkpoint := toolSummaryChanges(t, d, ConversationExportOptions{})
	// Model an exporting archive last written before the tool journal existed.
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(), `SELECT name FROM sqlite_master WHERE type='trigger' AND name LIKE 'conversation_tool_%'`)
		if err != nil {
			return err
		}
		names, err := scanStrings(rows)
		if err != nil {
			return err
		}
		for _, name := range names {
			if _, err := tx.ExecContext(t.Context(), `DROP TRIGGER "`+name+`"`); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(t.Context(), `DROP TABLE conversation_tool_changes`)
		return err
	}))
	path := d.Path()
	require.NoError(t, d.Close())

	// A read-only open does not upgrade the archive.
	readOnly, err := OpenReadOnly(t.Context(), path)
	require.NoError(t, err)
	_, err = readOnly.ExportConversationChanges(t.Context(), ConversationExportOptions{ToolSummaries: true, Checkpoint: checkpoint})
	var upgrade *SchemaUpgradeRequiredError
	require.ErrorAs(t, err, &upgrade)
	plain, err := readOnly.ExportConversationChanges(t.Context(), ConversationExportOptions{Checkpoint: checkpoint})
	require.NoError(t, err, "an export without summaries still reads the older archive")
	assert.Empty(t, plain.Changes)
	require.NoError(t, readOnly.Close())

	upgraded, err := OpenIsolated(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, upgraded.Close()) })
	summaries, others, _ := toolSummaryChanges(t, upgraded, ConversationExportOptions{ToolSummaries: true, Checkpoint: checkpoint})
	assert.Empty(t, others)
	require.Contains(t, summaries, "chat")
	require.NotNil(t, summaries["chat"].ToolSummary)
	assert.Equal(t, []ConversationToolCategory{{Category: "Bash", Calls: 1}}, summaries["chat"].ToolSummary.Categories)
}

func TestConversationExportToolSummaryOfRemovedSessions(t *testing.T) {
	d := testDB(t)
	toolSession(t, d, "trashed", ToolCall{ToolName: "Bash", Category: "Bash", ToolUseID: "a"})
	toolSession(t, d, "deleted", ToolCall{ToolName: "Bash", Category: "Bash", ToolUseID: "b"})
	_, _, checkpoint := toolSummaryChanges(t, d, ConversationExportOptions{ToolSummaries: true})

	require.NoError(t, d.SoftDeleteSession(t.Context(), "trashed"))
	require.NoError(t, d.SoftDeleteSession(t.Context(), "deleted"))
	require.NoError(t, d.DeleteSession(t.Context(), "deleted"))

	// A trashed session's summary is withdrawn like its messages. A deleted
	// session has no summary left to list; its session change reports it.
	summaries, others, checkpoint := toolSummaryChanges(t, d, ConversationExportOptions{ToolSummaries: true, Checkpoint: checkpoint})
	assert.NotContains(t, summaries, "deleted")
	require.Contains(t, summaries, "trashed")
	assert.True(t, summaries["trashed"].Deleted)
	assert.Nil(t, summaries["trashed"].ToolSummary)
	removed := map[string]bool{}
	for _, change := range others {
		if change.Type == "session" && change.Deleted {
			removed[change.SessionID] = true
		}
	}
	assert.Equal(t, map[string]bool{"trashed": true, "deleted": true}, removed)

	_, err := d.RestoreSession(t.Context(), "trashed")
	require.NoError(t, err)
	summaries, _, _ = toolSummaryChanges(t, d, ConversationExportOptions{ToolSummaries: true, Checkpoint: checkpoint})
	require.Contains(t, summaries, "trashed")
	assert.False(t, summaries["trashed"].Deleted)
	require.NotNil(t, summaries["trashed"].ToolSummary, "restoring a session lists its summary again")
	assert.Equal(t, []ConversationToolCategory{{Category: "Bash", Calls: 1}}, summaries["trashed"].ToolSummary.Categories)

	summaries, _, _ = toolSummaryChanges(t, d, ConversationExportOptions{ToolSummaries: true})
	assert.NotContains(t, summaries, "deleted", "a new walk does not list a deleted session's summary")
	var journaled int
	require.NoError(t, d.getReader().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM conversation_tool_changes WHERE session_id='deleted'`).Scan(&journaled))
	assert.Zero(t, journaled, "a deleted session leaves no journal row behind")
}
