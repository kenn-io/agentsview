// ABOUTME: Tests for sync engine helper functions.
// ABOUTME: Covers pairToolResults and related conversion logic.
package sync

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	gosync "sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(
		filepath.Join(t.TempDir(), "test.db"),
	)
	require.NoError(t, err)
	t.Cleanup(func() { d.Close() })
	return d
}

// fakeFileInfo implements os.FileInfo for test use.
type fakeFileInfo struct {
	size  int64
	mtime int64 // UnixNano
}

func (f fakeFileInfo) Name() string      { return "test" }
func (f fakeFileInfo) Size() int64       { return f.size }
func (f fakeFileInfo) Mode() os.FileMode { return 0 }
func (f fakeFileInfo) ModTime() time.Time {
	return time.Unix(0, f.mtime)
}
func (f fakeFileInfo) IsDir() bool { return false }
func (f fakeFileInfo) Sys() any    { return nil }

func TestFilterEmptyMessages(t *testing.T) {
	tests := []struct {
		name string
		msgs []db.Message
		want []db.Message
	}{
		{
			name: "removes empty-content user message after pairing",
			msgs: []db.Message{
				{
					Role:    "assistant",
					Content: "Let me read the file.",
					ToolCalls: []db.ToolCall{
						{ToolUseID: "t1", ToolName: "Read"},
					},
				},
				{
					Role:    "user",
					Content: "",
					ToolResults: []db.ToolResult{
						{ToolUseID: "t1", ContentLength: 500},
					},
				},
			},
			want: []db.Message{
				{
					Role:    "assistant",
					Content: "Let me read the file.",
					ToolCalls: []db.ToolCall{
						{ToolUseID: "t1", ToolName: "Read", ResultContentLength: 500},
					},
				},
			},
		},
		{
			name: "keeps user message with real content",
			msgs: []db.Message{
				{
					Role:    "assistant",
					Content: "Here is the result.",
					ToolCalls: []db.ToolCall{
						{ToolUseID: "t1", ToolName: "Bash"},
					},
				},
				{
					Role:    "user",
					Content: "",
					ToolResults: []db.ToolResult{
						{ToolUseID: "t1", ContentLength: 100},
					},
				},
				{
					Role:    "user",
					Content: "Thanks, now do something else.",
				},
			},
			want: []db.Message{
				{
					Role:    "assistant",
					Content: "Here is the result.",
					ToolCalls: []db.ToolCall{
						{ToolUseID: "t1", ToolName: "Bash", ResultContentLength: 100},
					},
				},
				{
					Role:    "user",
					Content: "Thanks, now do something else.",
				},
			},
		},
		{
			name: "whitespace-only content treated as empty",
			msgs: []db.Message{
				{
					Role:    "assistant",
					Content: "Reading...",
					ToolCalls: []db.ToolCall{
						{ToolUseID: "t1", ToolName: "Read"},
					},
				},
				{
					Role:    "user",
					Content: "   \n\t  ",
					ToolResults: []db.ToolResult{
						{ToolUseID: "t1", ContentLength: 300},
					},
				},
			},
			want: []db.Message{
				{
					Role:    "assistant",
					Content: "Reading...",
					ToolCalls: []db.ToolCall{
						{ToolUseID: "t1", ToolName: "Read", ResultContentLength: 300},
					},
				},
			},
		},
		{
			name: "preserves empty assistant message",
			msgs: []db.Message{
				{
					Role:    "assistant",
					Content: "",
				},
			},
			want: []db.Message{
				{
					Role:    "assistant",
					Content: "",
				},
			},
		},
		{
			name: "only removes user messages with tool results",
			msgs: []db.Message{
				{
					Role:    "assistant",
					Content: "",
				},
				{
					Role:    "user",
					Content: "",
				},
			},
			want: []db.Message{
				{
					Role:    "assistant",
					Content: "",
				},
				{
					Role:    "user",
					Content: "",
				},
			},
		},
		{
			name: "no messages returns empty",
			msgs: nil,
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pairAndFilter(tt.msgs, nil)
			diff := cmp.Diff(tt.want, got)
			assert.Empty(t, diff, "pairAndFilter() mismatch (-want +got):\n%s", diff)
		})
	}
}

func TestPostFilterCounts(t *testing.T) {
	type counts struct {
		Total int
		User  int
	}
	tests := []struct {
		name string
		msgs []db.Message
		want counts
	}{
		{
			name: "mixed roles",
			msgs: []db.Message{
				{Role: "user", Content: "hello"},
				{Role: "assistant", Content: "hi"},
				{Role: "user", Content: "thanks"},
			},
			want: counts{Total: 3, User: 2},
		},
		{
			name: "no user messages",
			msgs: []db.Message{
				{Role: "assistant", Content: "hi"},
			},
			want: counts{Total: 1, User: 0},
		},
		{
			name: "empty slice",
			msgs: nil,
			want: counts{Total: 0, User: 0},
		},
		{
			name: "all user messages",
			msgs: []db.Message{
				{Role: "user", Content: "a"},
				{Role: "user", Content: "b"},
			},
			want: counts{Total: 2, User: 2},
		},
		{
			name: "system messages excluded from user count",
			msgs: []db.Message{
				{Role: "user", Content: "hello", IsSystem: false},
				{Role: "user", Content: "system notice", IsSystem: true},
				{Role: "assistant", Content: "hi"},
				{Role: "user", Content: "[Turn finished: endTurn]", IsSystem: true},
			},
			want: counts{Total: 4, User: 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			total, user := postFilterCounts(tt.msgs)
			got := counts{Total: total, User: user}
			diff := cmp.Diff(tt.want, got)
			assert.Empty(t, diff, "postFilterCounts() mismatch (-want +got):\n%s", diff)
		})
	}
}

func TestPairToolResults(t *testing.T) {
	tests := []struct {
		name string
		msgs []db.Message
		want []db.Message
	}{
		{
			name: "basic pairing across messages",
			msgs: []db.Message{
				{ToolCalls: []db.ToolCall{
					{ToolUseID: "t1", ToolName: "Read"},
					{ToolUseID: "t2", ToolName: "Grep"},
				}},
				{ToolResults: []db.ToolResult{
					{ToolUseID: "t1", ContentLength: 100},
					{ToolUseID: "t2", ContentLength: 200},
				}},
			},
			want: []db.Message{
				{ToolCalls: []db.ToolCall{
					{ToolUseID: "t1", ToolName: "Read", ResultContentLength: 100},
					{ToolUseID: "t2", ToolName: "Grep", ResultContentLength: 200},
				}},
				{ToolResults: []db.ToolResult{
					{ToolUseID: "t1", ContentLength: 100},
					{ToolUseID: "t2", ContentLength: 200},
				}},
			},
		},
		{
			name: "unmatched tool_result ignored",
			msgs: []db.Message{
				{ToolCalls: []db.ToolCall{
					{ToolUseID: "t1", ToolName: "Read"},
				}},
				{ToolResults: []db.ToolResult{
					{ToolUseID: "t1", ContentLength: 50},
					{ToolUseID: "t_unknown", ContentLength: 999},
				}},
			},
			want: []db.Message{
				{ToolCalls: []db.ToolCall{
					{ToolUseID: "t1", ToolName: "Read", ResultContentLength: 50},
				}},
				{ToolResults: []db.ToolResult{
					{ToolUseID: "t1", ContentLength: 50},
					{ToolUseID: "t_unknown", ContentLength: 999},
				}},
			},
		},
		{
			name: "unmatched tool_call keeps zero",
			msgs: []db.Message{
				{ToolCalls: []db.ToolCall{
					{ToolUseID: "t1", ToolName: "Read"},
					{ToolUseID: "t2", ToolName: "Bash"},
				}},
				{ToolResults: []db.ToolResult{
					{ToolUseID: "t1", ContentLength: 42},
				}},
			},
			want: []db.Message{
				{ToolCalls: []db.ToolCall{
					{ToolUseID: "t1", ToolName: "Read", ResultContentLength: 42},
					{ToolUseID: "t2", ToolName: "Bash", ResultContentLength: 0},
				}},
				{ToolResults: []db.ToolResult{
					{ToolUseID: "t1", ContentLength: 42},
				}},
			},
		},
		{
			name: "empty messages",
			msgs: nil,
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pairToolResults(tt.msgs, nil)
			diff := cmp.Diff(tt.want, tt.msgs)
			assert.Empty(t, diff, "pairToolResults() mismatch (-want +got):\n%s", diff)
		})
	}
}

func TestPairToolResultsContent(t *testing.T) {
	ampToolResultText := "line 1\nline \"2\" output"
	ampToolResultRaw := "\"line 1\\nline \\\"2\\\" output\""

	tests := []struct {
		name    string
		msgs    []db.Message
		blocked map[string]bool
		want    []db.Message
	}{
		{
			name: "content stored for non-blocked category",
			msgs: []db.Message{
				{ToolCalls: []db.ToolCall{
					{ToolUseID: "t1", ToolName: "Bash", Category: "Bash"},
				}},
				{ToolResults: []db.ToolResult{
					{ToolUseID: "t1", ContentLength: 42, ContentRaw: `"output text"`},
				}},
			},
			blocked: map[string]bool{"Read": true, "Glob": true},
			want: []db.Message{
				{ToolCalls: []db.ToolCall{
					{ToolUseID: "t1", ToolName: "Bash", Category: "Bash",
						ResultContentLength: 42, ResultContent: "output text"},
				}},
				{ToolResults: []db.ToolResult{
					{ToolUseID: "t1", ContentLength: 42, ContentRaw: `"output text"`},
				}},
			},
		},
		{
			name: "content blocked for Read category",
			msgs: []db.Message{
				{ToolCalls: []db.ToolCall{
					{ToolUseID: "t1", ToolName: "Read", Category: "Read"},
				}},
				{ToolResults: []db.ToolResult{
					{ToolUseID: "t1", ContentLength: 5000, ContentRaw: `"file data"`},
				}},
			},
			blocked: map[string]bool{"Read": true, "Glob": true},
			want: []db.Message{
				{ToolCalls: []db.ToolCall{
					{ToolUseID: "t1", ToolName: "Read", Category: "Read",
						ResultContentLength: 5000, ResultContent: ""},
				}},
				{ToolResults: []db.ToolResult{
					{ToolUseID: "t1", ContentLength: 5000, ContentRaw: `"file data"`},
				}},
			},
		},
		{
			name: "nil blocked map stores all content",
			msgs: []db.Message{
				{ToolCalls: []db.ToolCall{
					{ToolUseID: "t1", ToolName: "Read", Category: "Read"},
				}},
				{ToolResults: []db.ToolResult{
					{ToolUseID: "t1", ContentLength: 100, ContentRaw: `"file content"`},
				}},
			},
			blocked: nil,
			want: []db.Message{
				{ToolCalls: []db.ToolCall{
					{ToolUseID: "t1", ToolName: "Read", Category: "Read",
						ResultContentLength: 100, ResultContent: "file content"},
				}},
				{ToolResults: []db.ToolResult{
					{ToolUseID: "t1", ContentLength: 100, ContentRaw: `"file content"`},
				}},
			},
		},
		{
			// Mirrors ContentRaw produced by parser.extractAmpToolResults
			// (JSON-marshaled plain-text output).
			name: "amp: marshaled tool result text decodes into ResultContent",
			msgs: []db.Message{
				{ToolCalls: []db.ToolCall{
					{ToolUseID: "t1", ToolName: "Bash", Category: "Bash"},
				}},
				{ToolResults: []db.ToolResult{
					{
						ToolUseID:     "t1",
						ContentLength: len(ampToolResultText),
						ContentRaw:    ampToolResultRaw,
					},
				}},
			},
			blocked: nil,
			want: []db.Message{
				{ToolCalls: []db.ToolCall{
					{
						ToolUseID: "t1", ToolName: "Bash", Category: "Bash",
						ResultContentLength: len(ampToolResultText),
						ResultContent:       ampToolResultText,
					},
				}},
				{ToolResults: []db.ToolResult{
					{
						ToolUseID:     "t1",
						ContentLength: len(ampToolResultText),
						ContentRaw:    ampToolResultRaw,
					},
				}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pairToolResults(tt.msgs, tt.blocked)
			diff := cmp.Diff(tt.want, tt.msgs)
			assert.Empty(t, diff, "pairToolResults() mismatch (-want +got):\n%s", diff)
		})
	}
}

func TestPairToolResultEventSummaries(t *testing.T) {
	tests := []struct {
		name    string
		msgs    []db.Message
		blocked map[string]bool
		want    []db.Message
	}{
		{
			name: "single event becomes summary",
			msgs: []db.Message{{
				ToolCalls: []db.ToolCall{{
					ToolUseID: "call_wait",
					ToolName:  "wait",
					Category:  "Other",
					ResultEvents: []db.ToolResultEvent{{
						ToolUseID:     "call_wait",
						AgentID:       "agent-1",
						Source:        "wait_output",
						Status:        "completed",
						Content:       "Finished successfully",
						ContentLength: len("Finished successfully"),
					}},
				}},
			}},
			want: []db.Message{{
				ToolCalls: []db.ToolCall{{
					ToolUseID:           "call_wait",
					ToolName:            "wait",
					Category:            "Other",
					ResultContentLength: len("Finished successfully"),
					ResultContent:       "Finished successfully",
					ResultEvents: []db.ToolResultEvent{{
						ToolUseID:     "call_wait",
						AgentID:       "agent-1",
						Source:        "wait_output",
						Status:        "completed",
						Content:       "Finished successfully",
						ContentLength: len("Finished successfully"),
					}},
				}},
			}},
		},
		{
			name: "multi-agent latest summary keeps one line per agent",
			msgs: []db.Message{{
				ToolCalls: []db.ToolCall{{
					ToolUseID: "call_wait",
					ToolName:  "wait",
					Category:  "Other",
					ResultEvents: []db.ToolResultEvent{
						{
							ToolUseID:     "call_wait",
							AgentID:       "agent-a",
							Source:        "wait_output",
							Status:        "completed",
							Content:       "First finished",
							ContentLength: len("First finished"),
						},
						{
							ToolUseID:     "call_wait",
							AgentID:       "agent-b",
							Source:        "subagent_notification",
							Status:        "completed",
							Content:       "Second finished",
							ContentLength: len("Second finished"),
						},
					},
				}},
			}},
			want: []db.Message{{
				ToolCalls: []db.ToolCall{{
					ToolUseID:           "call_wait",
					ToolName:            "wait",
					Category:            "Other",
					ResultContentLength: len("agent-a:\nFirst finished\n\nagent-b:\nSecond finished"),
					ResultContent:       "agent-a:\nFirst finished\n\nagent-b:\nSecond finished",
					ResultEvents: []db.ToolResultEvent{
						{
							ToolUseID:     "call_wait",
							AgentID:       "agent-a",
							Source:        "wait_output",
							Status:        "completed",
							Content:       "First finished",
							ContentLength: len("First finished"),
						},
						{
							ToolUseID:     "call_wait",
							AgentID:       "agent-b",
							Source:        "subagent_notification",
							Status:        "completed",
							Content:       "Second finished",
							ContentLength: len("Second finished"),
						},
					},
				}},
			}},
		},
		{
			name: "blocked category keeps length but drops summary content",
			msgs: []db.Message{{
				ToolCalls: []db.ToolCall{{
					ToolUseID: "call_read",
					ToolName:  "Read",
					Category:  "Read",
					ResultEvents: []db.ToolResultEvent{{
						ToolUseID:     "call_read",
						Source:        "wait_output",
						Status:        "completed",
						Content:       "secret file body",
						ContentLength: len("secret file body"),
					}},
				}},
			}},
			blocked: map[string]bool{"Read": true},
			want: []db.Message{{
				ToolCalls: []db.ToolCall{{
					ToolUseID:           "call_read",
					ToolName:            "Read",
					Category:            "Read",
					ResultContentLength: len("secret file body"),
					ResultContent:       "",
					ResultEvents:        nil,
				}},
			}},
		},
		{
			name: "mixed anonymous and multi-agent content keeps both",
			msgs: []db.Message{{
				ToolCalls: []db.ToolCall{{
					ToolUseID: "call_wait",
					ToolName:  "wait",
					Category:  "Other",
					ResultEvents: []db.ToolResultEvent{
						{
							ToolUseID:     "call_wait",
							AgentID:       "agent-a",
							Source:        "wait_output",
							Status:        "completed",
							Content:       "First finished",
							ContentLength: len("First finished"),
						},
						{
							ToolUseID:     "call_wait",
							AgentID:       "agent-b",
							Source:        "wait_output",
							Status:        "completed",
							Content:       "Second finished",
							ContentLength: len("Second finished"),
						},
						{
							ToolUseID:     "call_wait",
							Source:        "subagent_notification",
							Status:        "completed",
							Content:       "Detached note",
							ContentLength: len("Detached note"),
						},
					},
				}},
			}},
			want: []db.Message{{
				ToolCalls: []db.ToolCall{{
					ToolUseID:           "call_wait",
					ToolName:            "wait",
					Category:            "Other",
					ResultContentLength: len("agent-a:\nFirst finished\n\nagent-b:\nSecond finished\n\nDetached note"),
					ResultContent:       "agent-a:\nFirst finished\n\nagent-b:\nSecond finished\n\nDetached note",
					ResultEvents: []db.ToolResultEvent{
						{
							ToolUseID:     "call_wait",
							AgentID:       "agent-a",
							Source:        "wait_output",
							Status:        "completed",
							Content:       "First finished",
							ContentLength: len("First finished"),
						},
						{
							ToolUseID:     "call_wait",
							AgentID:       "agent-b",
							Source:        "wait_output",
							Status:        "completed",
							Content:       "Second finished",
							ContentLength: len("Second finished"),
						},
						{
							ToolUseID:     "call_wait",
							Source:        "subagent_notification",
							Status:        "completed",
							Content:       "Detached note",
							ContentLength: len("Detached note"),
						},
					},
				}},
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pairToolResultEventSummaries(tt.msgs, tt.blocked)
			diff := cmp.Diff(tt.want, tt.msgs)
			require.Empty(t, diff, "pairToolResultEventSummaries() mismatch (-want +got):\n%s", diff)
		})
	}
}

func TestApplyRemoteRewrites(t *testing.T) {
	tests := []struct {
		name         string
		prefix       string
		rewriter     func(string) string
		sess         db.Session
		msgs         []db.Message
		wantSessID   string
		wantParent   *string
		wantFilePath *string
		wantMsgSess  string // expected SessionID on messages
		wantSubs     []string
		wantEvSubs   []string
	}{
		{
			name:   "no prefix is no-op",
			prefix: "",
			sess: db.Session{
				ID: "abc",
			},
			msgs: []db.Message{
				{SessionID: "abc"},
			},
			wantSessID:  "abc",
			wantMsgSess: "abc",
		},
		{
			name:   "all fields prefixed",
			prefix: "host~",
			sess: db.Session{
				ID:              "abc",
				ParentSessionID: strPtr("parent-1"),
				FilePath:        strPtr("/tmp/file"),
			},
			msgs: []db.Message{
				{
					SessionID: "abc",
					ToolCalls: []db.ToolCall{
						{
							SessionID:         "abc",
							SubagentSessionID: "sub-1",
							ResultEvents: []db.ToolResultEvent{
								{SubagentSessionID: "ev-1"},
								{SubagentSessionID: ""},
							},
						},
						{SessionID: "abc"},
					},
				},
			},
			wantSessID:   "host~abc",
			wantParent:   strPtr("host~parent-1"),
			wantFilePath: strPtr("/tmp/file"),
			wantMsgSess:  "host~abc",
			wantSubs:     []string{"host~sub-1", ""},
			wantEvSubs:   []string{"host~ev-1", ""},
		},
		{
			name:   "path rewriter applied",
			prefix: "box~",
			rewriter: func(p string) string {
				return "box:" + p
			},
			sess: db.Session{
				ID:       "x",
				FilePath: strPtr("/remote/path"),
			},
			msgs:         nil,
			wantSessID:   "box~x",
			wantFilePath: strPtr("box:/remote/path"),
		},
		{
			name:   "nil parent stays nil",
			prefix: "h~",
			sess: db.Session{
				ID: "z",
			},
			wantSessID: "h~z",
			wantParent: nil,
		},
		{
			name:   "empty parent stays empty",
			prefix: "h~",
			sess: db.Session{
				ID:              "z",
				ParentSessionID: strPtr(""),
			},
			wantSessID: "h~z",
			wantParent: strPtr(""),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &Engine{
				idPrefix:     tt.prefix,
				pathRewriter: tt.rewriter,
			}
			e.applyRemoteRewrites(&tt.sess, tt.msgs)

			assert.Equal(t, tt.wantSessID, tt.sess.ID)
			diff := cmp.Diff(tt.wantParent, tt.sess.ParentSessionID)
			assert.Empty(t, diff, "ParentSessionID %s", diff)
			if tt.wantFilePath != nil {
				diff := cmp.Diff(tt.wantFilePath, tt.sess.FilePath)
				assert.Empty(t, diff, "FilePath %s", diff)
			}
			for _, m := range tt.msgs {
				assert.Equal(t, tt.wantMsgSess, m.SessionID)
			}
			var gotSubs, gotEvSubs []string
			for _, m := range tt.msgs {
				for _, tc := range m.ToolCalls {
					gotSubs = append(
						gotSubs, tc.SubagentSessionID,
					)
					for _, ev := range tc.ResultEvents {
						gotEvSubs = append(
							gotEvSubs,
							ev.SubagentSessionID,
						)
					}
				}
			}
			diff = cmp.Diff(tt.wantSubs, gotSubs)
			assert.Empty(t, diff, "SubagentSessionIDs %s", diff)
			diff = cmp.Diff(tt.wantEvSubs, gotEvSubs)
			assert.Empty(t, diff, "ResultEvent SubagentSessionIDs %s", diff)
		})
	}
}

func TestShouldSkipFileWithIDPrefix(t *testing.T) {
	database := openTestDB(t)

	// Store a session with prefixed ID and file metadata.
	sess := db.Session{
		ID:       "host~abc-123",
		Project:  "test",
		Machine:  "host",
		Agent:    "claude",
		FilePath: strPtr("host:/remote/session.jsonl"),
		FileSize: int64Ptr(1024),
		FileMtime: int64Ptr(
			int64(1700000000000000000),
		),
	}
	require.NoError(t, database.UpsertSession(sess))
	// data_version is no longer persisted by UpsertSession;
	// stamp it explicitly so the skip check sees a current
	// row.
	require.NoError(t, database.SetSessionDataVersion(
		sess.ID, db.CurrentDataVersion(),
	))

	// Engine with IDPrefix should find the session.
	e := &Engine{
		db:       database,
		idPrefix: "host~",
	}
	got := e.shouldSkipFile(
		"abc-123",
		fakeFileInfo{size: 1024, mtime: 1700000000000000000},
	)
	assert.True(t, got, "shouldSkipFile should return true")

	// Engine WITHOUT IDPrefix should NOT find it.
	e2 := &Engine{db: database}
	got2 := e2.shouldSkipFile(
		"abc-123",
		fakeFileInfo{size: 1024, mtime: 1700000000000000000},
	)
	assert.False(t, got2, "shouldSkipFile without prefix should return false")
}

func TestCollectAndBatchPrefixesParserExcludedIDs(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()

	raw := db.Session{
		ID:      "probe",
		Project: "local",
		Machine: "local",
		Agent:   "claude",
	}
	prefixed := db.Session{
		ID:      "host~probe",
		Project: "remote",
		Machine: "host",
		Agent:   "claude",
	}
	require.NoError(t, database.UpsertSession(raw))
	require.NoError(t, database.UpsertSession(prefixed))

	results := make(chan syncJob, 1)
	results <- syncJob{
		processResult: processResult{
			excludedSessionIDs: []string{"probe"},
		},
		path: "/remote/probe.jsonl",
	}
	close(results)

	e := &Engine{db: database, idPrefix: "host~"}
	stats := e.collectAndBatch(
		ctx, results, 1, 1, nil, syncWriteDefault,
	)

	assert.Equal(t, []string{"host~probe"}, stats.parserExcludedIDs)
	gotRaw, err := database.GetSession(ctx, "probe")
	require.NoError(t, err, "raw local session lookup")
	assert.NotNil(t, gotRaw, "raw local session must not be deleted")
	gotPrefixed, err := database.GetSession(ctx, "host~probe")
	require.NoError(t, err, "prefixed remote session lookup")
	assert.Nil(t, gotPrefixed, "prefixed remote session should be deleted")
}

func TestShouldSkipByPathWithRewriter(t *testing.T) {
	database := openTestDB(t)

	// Store a session with rewritten file path.
	sess := db.Session{
		ID:       "host~codex:abc",
		Project:  "test",
		Machine:  "host",
		Agent:    "codex",
		FilePath: strPtr("host:/remote/codex/abc.jsonl"),
		FileSize: int64Ptr(2048),
		FileMtime: int64Ptr(
			int64(1700000000000000000),
		),
	}
	require.NoError(t, database.UpsertSession(sess))
	require.NoError(t, database.SetSessionDataVersion(
		sess.ID, db.CurrentDataVersion(),
	))

	rewriter := func(p string) string {
		return "host:" + p
	}

	// Engine with PathRewriter should find the session.
	e := &Engine{
		db:           database,
		pathRewriter: rewriter,
	}
	got := e.shouldSkipByPath(
		"/remote/codex/abc.jsonl",
		fakeFileInfo{size: 2048, mtime: 1700000000000000000},
	)
	assert.True(t, got, "shouldSkipByPath should return true")

	// Without rewriter, lookup misses.
	e2 := &Engine{db: database}
	got2 := e2.shouldSkipByPath(
		"/remote/codex/abc.jsonl",
		fakeFileInfo{size: 2048, mtime: 1700000000000000000},
	)
	assert.False(t, got2, "shouldSkipByPath without rewriter should return false")
}

func TestToDBSessionStoresSessionName(t *testing.T) {
	pw := pendingWrite{sess: parser.ParsedSession{
		ID:           "commandcode:test",
		Project:      "sample_project",
		Machine:      "local",
		Agent:        parser.AgentCommandCode,
		SessionName:  "Startup investigation",
		FirstMessage: "Inspect server logs",
	}}

	got := toDBSession(pw)
	require.NotNil(t, got.SessionName)
	assert.Equal(t, "Startup investigation", *got.SessionName)
	require.NotNil(t, got.FirstMessage)
	assert.Equal(t, "Inspect server logs", *got.FirstMessage)
}

func TestBlockedCategorySet(t *testing.T) {
	tests := []struct {
		name  string
		input []string
		check string
		want  bool
	}{
		{"exact match", []string{"Read"}, "Read", true},
		{"lowercase normalized", []string{"read"}, "Read", true},
		{"uppercase normalized", []string{"GLOB"}, "Glob", true},
		{"trimmed", []string{" Read "}, "Read", true},
		{"empty entry skipped", []string{""}, "Read", false},
		{"nil input", nil, "Read", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := blockedCategorySet(tt.input)
			got := m[tt.check]
			assert.Equal(t, tt.want, got,
				"blockedCategorySet(%v)[%q]", tt.input, tt.check)
		})
	}
}

func TestOpenCodeLegacyArchiveLooksIncomplete(t *testing.T) {
	stored := []db.Message{
		{
			Ordinal:          1,
			Role:             "assistant",
			ContentLength:    100,
			HasOutputTokens:  true,
			OutputTokens:     200,
			HasContextTokens: true,
			ContextTokens:    400,
			ToolCalls:        []db.ToolCall{{ToolName: "Read"}},
			HasThinking:      true,
		},
	}

	t.Run("extra parsed messages still preserve incomplete prefix", func(t *testing.T) {
		parsed := []db.Message{
			{
				Ordinal:          1,
				Role:             "assistant",
				ContentLength:    50,
				HasOutputTokens:  false,
				HasContextTokens: false,
				ToolCalls:        nil,
				HasThinking:      false,
			},
			{
				Ordinal:       2,
				Role:          "assistant",
				ContentLength: 25,
			},
		}

		require.True(t, openCodeLegacyArchiveLooksIncomplete(parsed, stored),
			"want incomplete archive detection")
	})

	t.Run("extra parsed messages with complete prefix do not preserve", func(t *testing.T) {
		parsed := []db.Message{
			{
				Ordinal:          1,
				Role:             "assistant",
				ContentLength:    100,
				HasOutputTokens:  true,
				OutputTokens:     200,
				HasContextTokens: true,
				ContextTokens:    400,
				ToolCalls:        []db.ToolCall{{ToolName: "Read"}},
				HasThinking:      true,
			},
			{
				Ordinal:       2,
				Role:          "assistant",
				ContentLength: 25,
			},
		}

		require.False(t, openCodeLegacyArchiveLooksIncomplete(parsed, stored),
			"got incomplete archive detection, want false")
	})
}

// fakeEmitter records scopes passed to Emit. Thread-safe so it
// can be called from engine goroutines under test.
type fakeEmitter struct {
	mu     gosync.Mutex
	scopes []string
}

func (f *fakeEmitter) Emit(scope string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scopes = append(f.scopes, scope)
}

func (f *fakeEmitter) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.scopes))
	copy(out, f.scopes)
	return out
}

// engineFixture bundles a *db.DB, a Claude directory, and an
// *Engine for emitter tests. The engine is rebuilt by
// engineWithEmitter so tests can swap emitters in.
type engineFixture struct {
	db        *db.DB
	claudeDir string
	engine    *Engine
}

func newEngineFixture(t *testing.T) *engineFixture {
	t.Helper()
	fx := &engineFixture{
		db:        openTestDB(t),
		claudeDir: t.TempDir(),
	}
	fx.engineWithEmitter(nil)
	return fx
}

// engineWithEmitter builds a new *Engine wired to the fixture's
// db and claude dir, using em as the Emitter (nil for no
// emitter).
func (fx *engineFixture) engineWithEmitter(em Emitter) {
	fx.engine = NewEngine(fx.db, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {fx.claudeDir},
		},
		Machine: "local",
		Emitter: em,
	})
}

// writeClaudeSession writes a minimal single-user-message
// Claude JSONL file under <claudeDir>/<proj>/<filename> and
// returns the full path. The session ID derived by the parser
// is the filename with .jsonl stripped.
func (fx *engineFixture) writeClaudeSession(
	t *testing.T, proj, filename, firstMessage string,
) string {
	t.Helper()
	content := testjsonl.NewSessionBuilder().
		AddClaudeUser("2024-01-01T00:00:00Z", firstMessage).
		String()
	path := filepath.Join(fx.claudeDir, proj, filename)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

// appendClaudeMessage appends a single user message to the
// existing JSONL file so that SyncSingleSession has new data
// to ingest.
func (fx *engineFixture) appendClaudeMessage(
	t *testing.T, path, message string,
) {
	t.Helper()
	line := testjsonl.NewSessionBuilder().
		AddClaudeUser("2024-01-01T00:00:05Z", message).
		String()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err, "OpenFile")
	defer f.Close()
	_, err = f.WriteString(line)
	require.NoError(t, err, "WriteString")
}

// sessionIDFor returns the session ID the engine uses for the
// given Claude JSONL file. For Claude sessions the ID is the
// filename stem (no .jsonl suffix).
func (fx *engineFixture) sessionIDFor(
	t *testing.T, path string,
) string {
	t.Helper()
	return filepath.Base(path[:len(path)-len(".jsonl")])
}

func TestEngine_SyncAllEmitsWhenSessionsChange(t *testing.T) {
	fx := newEngineFixture(t)
	em := &fakeEmitter{}
	fx.engineWithEmitter(em)

	fx.writeClaudeSession(t, "proj", "s1.jsonl", "hello")
	stats := fx.engine.SyncAll(context.Background(), nil)
	require.NotZero(t, stats.Synced, "expected Synced > 0")
	got := em.got()
	require.Len(t, got, 1, "expected 1 emission, got %v", got)
	assert.Equal(t, "sessions", got[0], "SyncAll scope")
}

func TestEngine_SyncAllDoesNotEmitOnEmptyRun(t *testing.T) {
	fx := newEngineFixture(t)
	em := &fakeEmitter{}
	fx.engineWithEmitter(em)

	// No session files — sync finds nothing.
	stats := fx.engine.SyncAll(context.Background(), nil)
	require.Zero(t, stats.Synced)
	assert.Empty(t, em.got(), "expected no emissions")
}

func TestEngine_SyncPathsEmitsWhenSessionsChange(t *testing.T) {
	fx := newEngineFixture(t)
	em := &fakeEmitter{}
	fx.engineWithEmitter(em)

	path := fx.writeClaudeSession(t, "proj", "s1.jsonl", "hello")
	fx.engine.SyncPaths([]string{path})

	got := em.got()
	require.Len(t, got, 1, "expected 1 emission, got %v", got)
	assert.Equal(t, "sessions", got[0], "SyncPaths scope")
}

// emitterFunc adapts a plain function to the Emitter interface so
// tests can inline probing behavior without declaring a new type.
type emitterFunc func(scope string)

func (f emitterFunc) Emit(scope string) { f(scope) }

// TestEngine_SyncPathsEmitsAfterSyncMuReleased asserts that SyncPaths
// releases syncMu BEFORE invoking Emitter.Emit. The probe uses
// sync.Mutex.TryLock() synchronously: if the emit caller still holds
// the lock, TryLock returns false immediately; if the lock is already
// released, TryLock returns true. No goroutines, no wall-clock
// timeouts — deterministic under load.
func TestEngine_SyncPathsEmitsAfterSyncMuReleased(t *testing.T) {
	fx := newEngineFixture(t)

	var acquired atomic.Bool
	em := emitterFunc(func(scope string) {
		if fx.engine.syncMu.TryLock() {
			fx.engine.syncMu.Unlock()
			acquired.Store(true)
		}
	})
	fx.engineWithEmitter(em)

	path := fx.writeClaudeSession(t, "proj", "s1.jsonl", "hello")
	fx.engine.SyncPaths([]string{path})

	assert.True(t, acquired.Load(),
		"syncMu was still held when SyncPaths emitted — defer-order regression")
}

func TestEngine_SyncPathsDoesNotEmitOnNoMatches(t *testing.T) {
	fx := newEngineFixture(t)
	em := &fakeEmitter{}
	fx.engineWithEmitter(em)

	// Path doesn't match any known session pattern — classifyPaths
	// returns zero files and SyncPaths returns early.
	fx.engine.SyncPaths([]string{"/nonexistent/bogus.txt"})

	assert.Empty(t, em.got(), "expected no emissions")
}

func TestEngine_ClassifyOnePathClaudeStatPermissionErrorStillClassifies(
	t *testing.T,
) {
	if runtime.GOOS == "windows" {
		t.Skip("permission semantics differ on Windows")
	}

	db := openTestDB(t)
	claudeDir := t.TempDir()
	engine := NewEngine(db, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {claudeDir},
		},
		Machine: "local",
	})

	projectDir := filepath.Join(claudeDir, "proj")
	path := filepath.Join(projectDir, "session.jsonl")
	require.NoError(t, os.MkdirAll(projectDir, 0o755), "MkdirAll(%q)", projectDir)
	require.NoError(t, os.WriteFile(path, []byte("[]"), 0o644), "WriteFile(%q)", path)
	require.NoError(t, os.Chmod(projectDir, 0o000), "Chmod(%q)", projectDir)
	defer func() {
		_ = os.Chmod(projectDir, 0o755)
	}()

	got, ok := engine.classifyOnePath(path, nil)
	require.True(t, ok, "expected path to classify despite stat permission error")
	assert.Equal(t, path, got.Path)
	assert.Equal(t, parser.AgentClaude, got.Agent)
}

func TestEngine_ClassifyPathsDedupesOpenCodeChildPaths(t *testing.T) {
	db := openTestDB(t)
	opencodeDir := t.TempDir()
	engine := NewEngine(db, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOpenCode: {opencodeDir},
		},
		Machine: "local",
	})

	sessionPath := filepath.Join(
		opencodeDir, "storage", "session", "global",
		"ses_123.json",
	)
	messagePath := filepath.Join(
		opencodeDir, "storage", "message", "ses_123",
		"msg_1.json",
	)
	partPath := filepath.Join(
		opencodeDir, "storage", "part", "msg_1",
		"part_1.json",
	)
	for path, content := range map[string]string{
		sessionPath: `{"id":"ses_123","directory":"/tmp/proj","time":{"created":1,"updated":2}}`,
		messagePath: `{"id":"msg_1","sessionID":"ses_123","role":"user","time":{"created":1}}`,
		partPath:    `{"id":"part_1","sessionID":"ses_123","messageID":"msg_1","type":"text","text":"hi","time":{"created":1}}`,
	} {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755), "MkdirAll(%q)", path)
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644), "WriteFile(%q)", path)
	}

	files := engine.classifyPaths([]string{
		messagePath,
		partPath,
	})
	require.Len(t, files, 1)
	assert.Equal(t, sessionPath, files[0].Path)
}

func TestEngine_ClassifyPathsOpenCodeRemovedMessageDir(
	t *testing.T,
) {
	db := openTestDB(t)
	opencodeDir := t.TempDir()
	engine := NewEngine(db, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOpenCode: {opencodeDir},
		},
		Machine: "local",
	})

	sessionPath := filepath.Join(
		opencodeDir, "storage", "session", "global",
		"ses_123.json",
	)
	messagePath := filepath.Join(
		opencodeDir, "storage", "message", "ses_123",
		"msg_1.json",
	)
	for path, content := range map[string]string{
		sessionPath: `{"id":"ses_123","directory":"/tmp/proj","time":{"created":1,"updated":2}}`,
		messagePath: `{"id":"msg_1","sessionID":"ses_123","role":"user","time":{"created":1}}`,
	} {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755), "MkdirAll(%q)", path)
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644), "WriteFile(%q)", path)
	}

	messageDir := filepath.Dir(messagePath)
	require.NoError(t, os.RemoveAll(messageDir), "RemoveAll(%q)", messageDir)

	files := engine.classifyPaths([]string{messageDir})
	require.Len(t, files, 1)
	assert.Equal(t, sessionPath, files[0].Path)
}

func TestEngine_ClassifyPathsOpenCodeSQLiteWALFile(
	t *testing.T,
) {
	db := openTestDB(t)
	opencodeDir := t.TempDir()
	engine := NewEngine(db, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOpenCode: {opencodeDir},
		},
		Machine: "local",
	})

	dbPath := filepath.Join(opencodeDir, "opencode.db")
	require.NoError(t, os.WriteFile(dbPath, []byte("db"), 0o644), "WriteFile(%q)", dbPath)
	walPath := filepath.Join(opencodeDir, "opencode.db-wal")
	require.NoError(t, os.WriteFile(walPath, []byte("wal"), 0o644), "WriteFile(%q)", walPath)

	files := engine.classifyPaths([]string{walPath})
	require.Len(t, files, 1)
	assert.Equal(t, dbPath, files[0].Path)
}

func TestEngine_ClassifyPathsOpenCodeRemovedMessageFile(
	t *testing.T,
) {
	db := openTestDB(t)
	opencodeDir := t.TempDir()
	engine := NewEngine(db, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOpenCode: {opencodeDir},
		},
		Machine: "local",
	})

	sessionPath := filepath.Join(
		opencodeDir, "storage", "session", "global",
		"ses_123.json",
	)
	messagePath := filepath.Join(
		opencodeDir, "storage", "message", "ses_123",
		"msg_1.json",
	)
	for path, content := range map[string]string{
		sessionPath: `{"id":"ses_123","directory":"/tmp/proj","time":{"created":1,"updated":2}}`,
		messagePath: `{"id":"msg_1","sessionID":"ses_123","role":"user","time":{"created":1}}`,
	} {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755), "MkdirAll(%q)", path)
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644), "WriteFile(%q)", path)
	}

	require.NoError(t, os.Remove(messagePath), "Remove(%q)", messagePath)

	files := engine.classifyPaths([]string{messagePath})
	require.Len(t, files, 1)
	assert.Equal(t, sessionPath, files[0].Path)
}

func TestEngine_ClassifyPathsOpenCodeRemovedPartDir(
	t *testing.T,
) {
	db := openTestDB(t)
	opencodeDir := t.TempDir()
	engine := NewEngine(db, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOpenCode: {opencodeDir},
		},
		Machine: "local",
	})

	sessionPath := filepath.Join(
		opencodeDir, "storage", "session", "global",
		"ses_123.json",
	)
	messagePath := filepath.Join(
		opencodeDir, "storage", "message", "ses_123",
		"msg_1.json",
	)
	partPath := filepath.Join(
		opencodeDir, "storage", "part", "msg_1",
		"part_1.json",
	)
	for path, content := range map[string]string{
		sessionPath: `{"id":"ses_123","directory":"/tmp/proj","time":{"created":1,"updated":2}}`,
		messagePath: `{"id":"msg_1","sessionID":"ses_123","role":"user","time":{"created":1}}`,
		partPath:    `{"id":"part_1","messageID":"msg_1","type":"text","text":"hi","time":{"created":1}}`,
	} {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755), "MkdirAll(%q)", path)
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644), "WriteFile(%q)", path)
	}

	partDir := filepath.Dir(partPath)
	require.NoError(t, os.RemoveAll(partDir), "RemoveAll(%q)", partDir)

	files := engine.classifyPaths([]string{partDir})
	require.Len(t, files, 1)
	assert.Equal(t, sessionPath, files[0].Path)
}

func TestEngine_ClassifyPathsOpenCodeRemovedPartFile(
	t *testing.T,
) {
	db := openTestDB(t)
	opencodeDir := t.TempDir()
	engine := NewEngine(db, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOpenCode: {opencodeDir},
		},
		Machine: "local",
	})

	sessionPath := filepath.Join(
		opencodeDir, "storage", "session", "global",
		"ses_123.json",
	)
	messagePath := filepath.Join(
		opencodeDir, "storage", "message", "ses_123",
		"msg_1.json",
	)
	partPath := filepath.Join(
		opencodeDir, "storage", "part", "msg_1",
		"part_1.json",
	)
	for path, content := range map[string]string{
		sessionPath: `{"id":"ses_123","directory":"/tmp/proj","time":{"created":1,"updated":2}}`,
		messagePath: `{"id":"msg_1","sessionID":"ses_123","role":"user","time":{"created":1}}`,
		partPath:    `{"id":"part_1","messageID":"msg_1","type":"text","text":"hi","time":{"created":1}}`,
	} {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755), "MkdirAll(%q)", path)
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644), "WriteFile(%q)", path)
	}

	require.NoError(t, os.Remove(partPath), "Remove(%q)", partPath)

	files := engine.classifyPaths([]string{partPath})
	require.Len(t, files, 1)
	assert.Equal(t, sessionPath, files[0].Path)
}

// TestEngine_ClassifyPathsQwenSession verifies fsnotify events for
// Qwen session files (which live two levels deep under the projects
// root, at <projectsDir>/<encoded-project>/chats/<session>.jsonl) are
// classified as AgentQwen — the original WatchSubdirs="chats" wiring
// pointed the watcher at the wrong path, leaving live sync broken
// even after the classifier branch is reachable.
func TestEngine_ClassifyPathsQwenSession(t *testing.T) {
	db := openTestDB(t)
	qwenDir := t.TempDir()
	engine := NewEngine(db, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentQwen: {qwenDir},
		},
		Machine: "local",
	})

	sessionID := "adc026b4-c620-43e4-8cc4-295593889d18"
	encodedProject := "-Users-alice-code-sample-project"
	chatsDir := filepath.Join(qwenDir, encodedProject, "chats")
	require.NoError(t, os.MkdirAll(chatsDir, 0o755), "MkdirAll(%q)", chatsDir)
	sessionPath := filepath.Join(chatsDir, sessionID+".jsonl")
	require.NoError(t, os.WriteFile(sessionPath, []byte("{}\n"), 0o644), "WriteFile(%q)", sessionPath)

	files := engine.classifyPaths([]string{sessionPath})
	require.Len(t, files, 1, "len(files) = %d, want 1 (%v)", len(files), files)
	assert.Equal(t, sessionPath, files[0].Path)
	assert.Equal(t, parser.AgentQwen, files[0].Agent)
	assert.Equal(t, "sample_project", files[0].Project)

	// Non-Qwen siblings (a stray file directly under projectsDir, a
	// file under <project>/<not-chats>/, a non-jsonl in chats/, and a
	// path outside the canonical <encoded-project>/chats/ shape) must
	// not classify as Qwen.
	bogus := []string{
		filepath.Join(qwenDir, "stray.jsonl"),
		filepath.Join(qwenDir, "proj", "notes", "a.jsonl"),
		filepath.Join(chatsDir, "notes.txt"),
		filepath.Join(qwenDir, "chats", sessionID+".jsonl"),
	}
	for _, p := range bogus {
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755), "MkdirAll(%q)", p)
		require.NoError(t, os.WriteFile(p, []byte("{}"), 0o644), "WriteFile(%q)", p)
	}
	got := engine.classifyPaths(bogus)
	assert.Empty(t, got, "expected no Qwen classifications for %v, got %v", bogus, got)
}

func TestEngine_ClassifyPathsCommandCodeSession(t *testing.T) {
	db := openTestDB(t)
	commandCodeDir := t.TempDir()
	engine := NewEngine(db, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCommandCode: {commandCodeDir},
		},
		Machine: "local",
	})

	sessionID := "adc026b4-c620-43e4-8cc4-295593889d18"
	projectDir := filepath.Join(commandCodeDir, "users-alice-code-sample-project")
	require.NoError(t, os.MkdirAll(projectDir, 0o755), "MkdirAll(%q)", projectDir)
	sessionPath := filepath.Join(projectDir, sessionID+".jsonl")
	dbtest.WriteTestFile(t, sessionPath, []byte("{}\n"))

	files := engine.classifyPaths([]string{sessionPath})
	require.Len(t, files, 1, "len(files) = %d, want 1 (%v)", len(files), files)
	assert.Equal(t, sessionPath, files[0].Path)
	assert.Equal(t, parser.AgentCommandCode, files[0].Agent)
	assert.Equal(t, "users_alice_code_sample_project", files[0].Project)

	bogus := []string{
		filepath.Join(commandCodeDir, "stray.jsonl"),
		filepath.Join(projectDir, "notes.txt"),
		filepath.Join(projectDir, sessionID+".checkpoints.jsonl"),
		filepath.Join(projectDir, sessionID+".prompts.jsonl"),
		filepath.Join(projectDir, "nested", sessionID+".jsonl"),
	}
	for _, p := range bogus {
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755), "MkdirAll(%q)", p)
		dbtest.WriteTestFile(t, p, []byte("{}"))
	}
	got := engine.classifyPaths(bogus)
	assert.Empty(t, got, "expected no Command Code classifications for %v, got %v", bogus, got)

	metaPath := filepath.Join(projectDir, sessionID+".meta.json")
	dbtest.WriteTestFile(t, metaPath, []byte("{}"))
	files = engine.classifyPaths([]string{metaPath})
	require.Len(t, files, 1, "len(files) = %d, want 1 (%v)", len(files), files)
	assert.Equal(t, sessionPath, files[0].Path)
	assert.Equal(t, parser.AgentCommandCode, files[0].Agent)
}

func TestEngine_ClassifyPathsQClawSession(t *testing.T) {
	db := openTestDB(t)
	qclawDir := t.TempDir()
	engine := NewEngine(db, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentQClaw: {qclawDir},
		},
		Machine: "local",
	})

	agentID := "main"
	sessionID := "adc026b4-c620-43e4-8cc4-295593889d18"
	sessionsDir := filepath.Join(qclawDir, agentID, "sessions")
	sessionPath := filepath.Join(sessionsDir, sessionID+".jsonl")
	dbtest.WriteTestFile(t, sessionPath, []byte("{}\n"))

	files := engine.classifyPaths([]string{sessionPath})
	require.Len(t, files, 1, "len(files) = %d, want 1 (%v)", len(files), files)
	assert.Equal(t, sessionPath, files[0].Path)
	assert.Equal(t, parser.AgentQClaw, files[0].Agent)

	bogus := []string{
		filepath.Join(qclawDir, "stray.jsonl"),
		filepath.Join(qclawDir, agentID, "notes", sessionID+".jsonl"),
		filepath.Join(sessionsDir, "notes.txt"),
		filepath.Join(qclawDir, "not a session id", "sessions", sessionID+".jsonl"),
	}
	for _, p := range bogus {
		dbtest.WriteTestFile(t, p, []byte("{}"))
	}
	got := engine.classifyPaths(bogus)
	assert.Empty(t, got, "expected no QClaw classifications for %v, got %v", bogus, got)
}

func TestEngine_ClassifyPathsQClawArchivedSession(t *testing.T) {
	db := openTestDB(t)
	qclawDir := t.TempDir()
	engine := NewEngine(db, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentQClaw: {qclawDir},
		},
		Machine: "local",
	})

	agentID := "main"
	sessionID := "adc026b4-c620-43e4-8cc4-295593889d18"
	sessionsDir := filepath.Join(qclawDir, agentID, "sessions")

	active := filepath.Join(sessionsDir, sessionID+".jsonl")
	archived := filepath.Join(
		sessionsDir,
		sessionID+".jsonl.deleted.2026-02-19T08-59-24.951Z",
	)
	dbtest.WriteTestFile(t, active, []byte("{}\n"))
	dbtest.WriteTestFile(t, archived, []byte("{}\n"))

	got := engine.classifyPaths([]string{archived})
	require.Empty(t, got, "expected archived file shadowed by active to be ignored, got %v", got)

	require.NoError(t, os.Remove(active), "Remove(%q)", active)
	files := engine.classifyPaths([]string{archived})
	require.Len(t, files, 1, "len(files) = %d, want 1 (%v)", len(files), files)
	assert.Equal(t, archived, files[0].Path)
	assert.Equal(t, parser.AgentQClaw, files[0].Agent)
}

func TestEngine_SyncSingleSessionEmitsOnSuccess(t *testing.T) {
	fx := newEngineFixture(t)
	em := &fakeEmitter{}
	fx.engineWithEmitter(em)

	path := fx.writeClaudeSession(t, "proj", "s1.jsonl", "hello")
	// Seed DB first so SyncSingleSession has something to find.
	fx.engine.SyncPaths([]string{path})

	// Clear emissions from the seed, then append + SyncSingleSession.
	em.mu.Lock()
	em.scopes = em.scopes[:0]
	em.mu.Unlock()

	fx.appendClaudeMessage(t, path, "world")
	sessionID := fx.sessionIDFor(t, path)
	require.NoError(t, fx.engine.SyncSingleSession(sessionID), "SyncSingleSession")
	got := em.got()
	require.Len(t, got, 1, "expected 1 emission, got %v", got)
	assert.Equal(t, "messages", got[0], "SyncSingleSession scope")
}

func TestToDBSessionTerminationStatus(t *testing.T) {
	tests := []struct {
		name string
		in   parser.TerminationStatus
		want *string
	}{
		{name: "empty maps to nil", in: "", want: nil},
		{name: "clean maps to pointer", in: parser.TerminationClean, want: new("clean")},
		{name: "tool_call_pending maps to pointer", in: parser.TerminationToolCallPending, want: new("tool_call_pending")},
		{name: "truncated maps to pointer", in: parser.TerminationTruncated, want: new("truncated")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pw := pendingWrite{
				sess: parser.ParsedSession{
					ID:                "s1",
					Project:           "p",
					Machine:           "m",
					Agent:             parser.AgentClaude,
					StartedAt:         time.Now(),
					EndedAt:           time.Now(),
					MessageCount:      1,
					UserMessageCount:  1,
					TerminationStatus: tc.in,
				},
			}
			got := toDBSession(pw)

			if tc.want == nil {
				assert.Nil(t, got.TerminationStatus)
			} else {
				require.NotNil(t, got.TerminationStatus)
				assert.Equal(t, *tc.want, *got.TerminationStatus)
			}
		})
	}
}

func TestToDBSessionCarriesSessionName(t *testing.T) {
	pw := pendingWrite{sess: parser.ParsedSession{
		ID:          "s1",
		Project:     "p",
		Agent:       parser.AgentClaude,
		SessionName: "agent-name",
	}}
	s := toDBSession(pw)
	require.NotNil(t, s.SessionName)
	assert.Equal(t, "agent-name", *s.SessionName)
	// converter must not touch display_name — only RenameSession may write it.
	assert.Nil(t, s.DisplayName)

	s2 := toDBSession(pendingWrite{sess: parser.ParsedSession{
		ID:      "s2",
		Project: "p",
		Agent:   parser.AgentClaude,
	}})
	assert.Nil(t, s2.SessionName)
	assert.Nil(t, s2.DisplayName)
}
