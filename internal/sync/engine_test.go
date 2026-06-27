// ABOUTME: Tests for sync engine helper functions.
// ABOUTME: Covers pairToolResults and related conversion logic.
package sync

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

func TestHasLegacyKiroCandidates(t *testing.T) {
	tests := []struct {
		name  string
		files []parser.DiscoveredFile
		want  bool
	}{
		{
			name: "empty",
			want: false,
		},
		{
			name: "non-kiro files",
			files: []parser.DiscoveredFile{
				{Agent: parser.AgentClaude, Path: "/tmp/claude/session.jsonl"},
			},
			want: false,
		},
		{
			name: "kiro sqlite database source",
			files: []parser.DiscoveredFile{
				{Agent: parser.AgentKiro, Path: "/tmp/kiro/data.sqlite3"},
			},
			want: false,
		},
		{
			name: "legacy kiro jsonl",
			files: []parser.DiscoveredFile{
				{Agent: parser.AgentKiro, Path: "/tmp/kiro/session.jsonl"},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, hasLegacyKiroCandidates(tt.files))
		})
	}
}

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

func TestToDBUsageEventsStampsFinalSessionID(t *testing.T) {
	tests := []struct {
		name      string
		sessionID string
		events    []parser.ParsedUsageEvent
		wantIDs   []string
	}{
		{
			name:      "empty event session id gets final id",
			sessionID: "antigravity:abc",
			events: []parser.ParsedUsageEvent{
				{Source: "generation", Model: "gemini"},
			},
			wantIDs: []string{"antigravity:abc"},
		},
		{
			name:      "parser-stamped id matching final id is kept",
			sessionID: "antigravity:abc",
			events: []parser.ParsedUsageEvent{
				{
					SessionID: "antigravity:abc",
					Source:    "generation",
					Model:     "gemini",
				},
			},
			wantIDs: []string{"antigravity:abc"},
		},
		{
			name:      "remote prefix overrides parser-stamped id",
			sessionID: "host~antigravity:abc",
			events: []parser.ParsedUsageEvent{
				{
					SessionID: "antigravity:abc",
					Source:    "generation",
					Model:     "gemini",
				},
				{
					SessionID: "antigravity:abc",
					Source:    "generation",
					Model:     "claude",
				},
			},
			wantIDs: []string{
				"host~antigravity:abc",
				"host~antigravity:abc",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := toDBUsageEvents(tt.sessionID, tt.events)
			require.Len(t, got, len(tt.wantIDs))
			for i, ev := range got {
				assert.Equal(t, tt.wantIDs[i], ev.SessionID)
			}
		})
	}
}

func TestToDBUsageEventsPreservesSessionSummaryTokenUpperBounds(t *testing.T) {
	rawInput := maxPlausibleTokens + 250_000
	rawOutput := maxPlausibleTokens + 500_000
	got, _ := toDBUsageEvents("hermes:summary", []parser.ParsedUsageEvent{
		{
			Source:                   "session",
			Model:                    "gpt-5.4",
			InputTokens:              rawInput,
			OutputTokens:             rawOutput,
			CacheCreationInputTokens: rawInput + 1,
			CacheReadInputTokens:     rawInput + 2,
			ReasoningTokens:          rawOutput + 3,
		},
		{
			Source:                   "session",
			Model:                    "gpt-5.4",
			InputTokens:              -1,
			OutputTokens:             -2,
			CacheCreationInputTokens: -3,
			CacheReadInputTokens:     -4,
			ReasoningTokens:          -5,
		},
	})

	require.Len(t, got, 2)
	assert.Equal(t, rawInput, got[0].InputTokens)
	assert.Equal(t, rawOutput, got[0].OutputTokens)
	assert.Equal(t, rawInput+1, got[0].CacheCreationInputTokens)
	assert.Equal(t, rawInput+2, got[0].CacheReadInputTokens)
	assert.Equal(t, rawOutput+3, got[0].ReasoningTokens)
	assert.Equal(t, 0, got[1].InputTokens)
	assert.Equal(t, 0, got[1].OutputTokens)
	assert.Equal(t, 0, got[1].CacheCreationInputTokens)
	assert.Equal(t, 0, got[1].CacheReadInputTokens)
	assert.Equal(t, 0, got[1].ReasoningTokens)
}

func TestWriteBatchRemoteIDPrefixUsageEvents(t *testing.T) {
	database := openTestDB(t)
	e := &Engine{db: database, idPrefix: "host~"}

	ts := time.Unix(1700000000, 0).UTC()
	pw := pendingWrite{
		sess: parser.ParsedSession{
			ID:           "antigravity:abc",
			Project:      "proj",
			Machine:      "host",
			Agent:        parser.AgentAntigravity,
			StartedAt:    ts,
			EndedAt:      ts,
			MessageCount: 1,
		},
		msgs: []parser.ParsedMessage{{
			Role:      parser.RoleUser,
			Content:   "hello",
			Timestamp: ts,
		}},
		usageEvents: []parser.ParsedUsageEvent{{
			// Parsers stamp the unprefixed session ID; the
			// write path must replace it with the final
			// remote-prefixed ID.
			SessionID:    "antigravity:abc",
			Source:       "generation",
			Model:        "gemini",
			InputTokens:  100,
			OutputTokens: 50,
			OccurredAt:   ts.Format(time.RFC3339Nano),
		}},
	}

	written, _, failed := e.writeBatch(
		[]pendingWrite{pw}, syncWriteDefault, false,
	)
	require.Equal(t, 0, failed, "no session writes may fail")
	require.Equal(t, 1, written)

	events, err := database.GetUsageEvents(
		context.Background(), "host~antigravity:abc",
	)
	require.NoError(t, err, "GetUsageEvents")
	require.Len(t, events, 1)
	assert.Equal(t, "host~antigravity:abc", events[0].SessionID)
	assert.Equal(t, "gemini", events[0].Model)
	assert.Equal(t, 100, events[0].InputTokens)
	assert.Equal(t, 50, events[0].OutputTokens)
}

// TestWriteBatchAntigravityReplacesMessages covers a live Antigravity
// IDE session synced before its gen_metadata rows exist: the next sync
// re-parses the same ordinals with model/token metadata attached, and
// that enrichment must reach the stored message rows rather than being
// dropped by the append-only write path.
func TestWriteBatchAntigravityReplacesMessages(t *testing.T) {
	database := openTestDB(t)
	e := &Engine{db: database}

	ts := time.Unix(1700000000, 0).UTC()
	mkWrite := func(withMeta bool) pendingWrite {
		msg := parser.ParsedMessage{
			Role:      parser.RoleAssistant,
			Content:   "assistant reply",
			Timestamp: ts,
		}
		if withMeta {
			msg.Model = "Test Gemini 3.5"
			msg.ContextTokens = 2400
			msg.OutputTokens = 210
			msg.HasContextTokens = true
			msg.HasOutputTokens = true
		}
		return pendingWrite{
			sess: parser.ParsedSession{
				ID:           "antigravity:meta",
				Project:      "proj",
				Machine:      "m",
				Agent:        parser.AgentAntigravity,
				StartedAt:    ts,
				EndedAt:      ts,
				MessageCount: 1,
			},
			msgs: []parser.ParsedMessage{msg},
		}
	}

	written, _, failed := e.writeBatch(
		[]pendingWrite{mkWrite(false)}, syncWriteDefault, false,
	)
	require.Equal(t, 0, failed)
	require.Equal(t, 1, written)

	written, _, failed = e.writeBatch(
		[]pendingWrite{mkWrite(true)}, syncWriteDefault, false,
	)
	require.Equal(t, 0, failed)
	require.Equal(t, 1, written)

	msgs, err := database.GetMessages(
		context.Background(), "antigravity:meta", 0, 10, true,
	)
	require.NoError(t, err, "GetMessages")
	require.Len(t, msgs, 1)
	assert.Equal(t, "Test Gemini 3.5", msgs[0].Model,
		"re-parsed model metadata must reach existing message rows")
}

// TestWriteBatchQwenPawReplacesMessages covers a QwenPaw session file
// being rewritten wholesale on every save. QwenPaw's
// _atomic_write_json rewrites the entire sessions/<name>.json on each
// save, and the parser assigns Ordinal by position in
// agent.memory.content. If that array is ever compacted, summarized,
// or reordered — common in agent-memory frameworks — ordinals shift,
// and the append-only writeMessages path would silently keep stale
// rows. The session must go through the replace path so a rewrite is
// applied as a delete+insert, not an ordinal-greater-than append.
func TestWriteBatchQwenPawReplacesMessages(t *testing.T) {
	database := openTestDB(t)
	e := &Engine{db: database}

	ts := time.Unix(1700000000, 0).UTC()
	mkWrite := func(content string) pendingWrite {
		msg := parser.ParsedMessage{
			Ordinal:   0,
			Role:      parser.RoleAssistant,
			Content:   content,
			Timestamp: ts,
		}
		return pendingWrite{
			sess: parser.ParsedSession{
				ID:           "qwenpaw:default:rewrite",
				Project:      "default",
				Machine:      "m",
				Agent:        parser.AgentQwenPaw,
				StartedAt:    ts,
				EndedAt:      ts,
				MessageCount: 1,
			},
			msgs: []parser.ParsedMessage{msg},
		}
	}

	written, _, failed := e.writeBatch(
		[]pendingWrite{mkWrite("old content")}, syncWriteDefault, false,
	)
	require.Equal(t, 0, failed)
	require.Equal(t, 1, written)

	written, _, failed = e.writeBatch(
		[]pendingWrite{mkWrite("new content")}, syncWriteDefault, false,
	)
	require.Equal(t, 0, failed)
	require.Equal(t, 1, written)

	msgs, err := database.GetMessages(
		context.Background(), "qwenpaw:default:rewrite", 0, 10, true,
	)
	require.NoError(t, err, "GetMessages")
	require.Len(t, msgs, 1, "rewrite must replace, not append")
	assert.Equal(t, "new content", msgs[0].Content,
		"rewritten content must reach existing message rows")
}

// TestSyncSingleSession_QwenPawPreservesWorkspaceFromDB covers the
// case where a QwenPaw session's stored DB file_path points outside
// any currently configured QWENPAW_DIR (e.g. the root was removed or
// the session was synced from a custom path). FindSourceFile still
// returns the stored path, and the provider must resolve the workspace
// from that path's implicit <root>/<workspace>/sessions/ layout rather
// than emitting a brand-new qwenpaw::<stem> session that orphans the
// requested qwenpaw:<workspace>:<stem> row.
func TestSyncSingleSession_QwenPawPreservesWorkspaceFromDB(t *testing.T) {
	database := openTestDB(t)

	// File at an arbitrary path NOT under any configured QWENPAW_DIR.
	root := t.TempDir()
	sessDir := filepath.Join(root, "my_ws", "sessions")
	require.NoError(t, os.MkdirAll(sessDir, 0o755))
	path := filepath.Join(sessDir, "default_1.json")
	require.NoError(t, os.WriteFile(path, []byte(
		`{"agent":{"memory":{"content":[[`+
			`{"id":"u1","name":"user","role":"user","content":[{"type":"text","text":"hi"}],"metadata":{},"timestamp":"2026-04-19 22:37:34.000"},[]`+
			`]]}}}`), 0o644))

	// Engine configured with QWENPAW_DIR pointing somewhere else
	// entirely, so the configured-root loop cannot match.
	otherDir := t.TempDir()
	e := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentQwenPaw: {otherDir},
		},
		Machine: "local",
	})

	// Seed the DB with the canonical session row. file_path is the
	// stored source of truth that FindSourceFile prefers.
	const sessionID = "qwenpaw:my_ws:default_1"
	fp := path
	require.NoError(t, database.UpsertSession(db.Session{
		ID:       sessionID,
		Project:  "my_ws",
		Machine:  "local",
		Agent:    "qwenpaw",
		FilePath: &fp,
	}))

	require.NoError(t, e.SyncSingleSession(sessionID))

	got, err := database.GetSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, got, "original session must still exist")
	assert.Equal(t, "my_ws", got.Project,
		"workspace must be preserved when the file is outside configured roots")

	// No empty-workspace orphan should have been written.
	orphan, err := database.GetSession(
		context.Background(), "qwenpaw::default_1",
	)
	require.NoError(t, err)
	assert.Nil(t, orphan,
		"no empty-workspace orphan session should be created")
}

// TestProcessAntigravityWALOnlyUpdateNotSkipped covers a live IDE
// session whose gen_metadata commits land in the SQLite WAL: the main
// .db file's size/mtime are unchanged, so the skip check must consult
// the sidecar set or the session never reparses.
func TestProcessAntigravityWALOnlyUpdateNotSkipped(t *testing.T) {
	database := openTestDB(t)
	e := &Engine{db: database}
	ctx := context.Background()

	root := t.TempDir()
	convDir := filepath.Join(root, "conversations")
	require.NoError(t, os.MkdirAll(convDir, 0o755))
	dbPath := filepath.Join(
		convDir, "abcdabcd-1111-2222-3333-444455556666.db",
	)
	sqlDB, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = sqlDB.Exec(
		`CREATE TABLE steps (idx integer, step_type integer, ` +
			`step_payload blob, PRIMARY KEY (idx))`,
	)
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())

	file := parser.DiscoveredFile{
		Agent:   parser.AgentAntigravity,
		Path:    dbPath,
		Project: "proj",
	}

	res := e.processFile(ctx, file)
	require.NoError(t, res.err)
	require.False(t, res.skip)
	require.Len(t, res.results, 1)

	pw := pendingWrite{
		sess:        res.results[0].Session,
		msgs:        res.results[0].Messages,
		usageEvents: res.results[0].UsageEvents,
	}
	written, _, failed := e.writeBatch(
		[]pendingWrite{pw}, syncWriteDefault, false,
	)
	require.Equal(t, 0, failed)
	require.Equal(t, 1, written)

	res = e.processFile(ctx, file)
	require.True(t, res.skip, "unchanged session should skip")

	// WAL-only update: the main .db is untouched.
	walPath := dbPath + "-wal"
	require.NoError(t, os.WriteFile(walPath, []byte("wal bytes"), 0o644))
	info, err := os.Stat(dbPath)
	require.NoError(t, err)
	walTime := info.ModTime().Add(5 * time.Second)
	require.NoError(t, os.Chtimes(walPath, walTime, walTime))

	res = e.processFile(ctx, file)
	assert.False(t, res.skip, "WAL-only update must trigger a reparse")
}

func TestProcessVibeMetaOnlyUpdateNotSkipped(t *testing.T) {
	database := openTestDB(t)
	e := &Engine{db: database}
	ctx := context.Background()

	root := t.TempDir()
	sessionDir := filepath.Join(root, "session_20260616_083518_0107f266")
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))

	msgPath := filepath.Join(sessionDir, "messages.jsonl")
	require.NoError(t, os.WriteFile(
		msgPath,
		[]byte(`{"role":"user","content":"hi"}`+"\n"),
		0o644,
	))

	metaPath := filepath.Join(sessionDir, "meta.json")
	require.NoError(t, os.WriteFile(
		metaPath,
		[]byte(`{"session_id":"abc","title":"Original title"}`+"\n"),
		0o644,
	))

	file := parser.DiscoveredFile{
		Agent: parser.AgentVibe,
		Path:  msgPath,
	}

	res := e.processFile(ctx, file)
	require.NoError(t, res.err)
	require.False(t, res.skip)
	require.Len(t, res.results, 1)
	require.Equal(t, "Original title", res.results[0].Session.SessionName)

	pw := pendingWrite{
		sess: res.results[0].Session,
		msgs: res.results[0].Messages,
	}
	written, _, failed := e.writeBatch(
		[]pendingWrite{pw}, syncWriteDefault, false,
	)
	require.Equal(t, 0, failed)
	require.Equal(t, 1, written)

	res = e.processFile(ctx, file)
	require.True(t, res.skip, "unchanged session should skip")

	// meta.json-only update: messages.jsonl is untouched, but the title
	// (sourced from meta.json) changes.
	info, err := os.Stat(msgPath)
	require.NoError(t, err)
	metaTime := info.ModTime().Add(5 * time.Second)
	require.NoError(t, os.WriteFile(
		metaPath,
		[]byte(`{"session_id":"abc","title":"Renamed title"}`+"\n"),
		0o644,
	))
	require.NoError(t, os.Chtimes(metaPath, metaTime, metaTime))

	res = e.processFile(ctx, file)
	require.False(t, res.skip, "meta.json-only update must trigger a reparse")
	require.Len(t, res.results, 1)
	assert.Equal(t, "Renamed title", res.results[0].Session.SessionName)
}

func TestProcessAntigravityBrainOnlyUpdateNotSkipped(t *testing.T) {
	database := openTestDB(t)
	e := &Engine{db: database}
	ctx := context.Background()

	root := t.TempDir()
	convDir := filepath.Join(root, "conversations")
	require.NoError(t, os.MkdirAll(convDir, 0o755))
	id := "abcdabcd-1111-2222-3333-444455557777"
	dbPath := filepath.Join(convDir, id+".db")
	sqlDB, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = sqlDB.Exec(
		`CREATE TABLE steps (idx integer, step_type integer, ` +
			`step_payload blob, PRIMARY KEY (idx))`,
	)
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())

	file := parser.DiscoveredFile{
		Agent:   parser.AgentAntigravity,
		Path:    dbPath,
		Project: "proj",
	}

	res := e.processFile(ctx, file)
	require.NoError(t, res.err)
	require.False(t, res.skip)
	require.Len(t, res.results, 1)

	pw := pendingWrite{
		sess:        res.results[0].Session,
		msgs:        res.results[0].Messages,
		usageEvents: res.results[0].UsageEvents,
	}
	written, _, failed := e.writeBatch(
		[]pendingWrite{pw}, syncWriteDefault, false,
	)
	require.Equal(t, 0, failed)
	require.Equal(t, 1, written)

	res = e.processFile(ctx, file)
	require.True(t, res.skip, "unchanged session should skip")

	// Brain-only update: the conversation DB files are untouched.
	brainDir := filepath.Join(root, "brain", id)
	require.NoError(t, os.MkdirAll(brainDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(brainDir, "task.md"),
		[]byte("brain artifact body"), 0o644,
	))

	res = e.processFile(ctx, file)
	require.False(t, res.skip,
		"brain-only update must trigger a reparse")
	require.Len(t, res.results, 1)
	var found bool
	for _, m := range res.results[0].Messages {
		if strings.Contains(m.Content, "brain artifact body") {
			found = true
		}
	}
	assert.True(t, found,
		"reparse must pick up the brain artifact message")
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

func TestShouldSkipCodexReparsesStaleProject(t *testing.T) {
	database := openTestDB(t)
	path := filepath.Join(t.TempDir(), "rollout-2026-06-21T18-59-38-abc.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o600))
	info, err := os.Stat(path)
	require.NoError(t, err, "stat codex fixture")

	sess := db.Session{
		ID:        "host~codex:abc",
		Project:   "roborev_ci_28293_3831737461",
		Machine:   "host",
		Agent:     "codex",
		FilePath:  strPtr("host:" + path),
		FileSize:  int64Ptr(info.Size()),
		FileMtime: int64Ptr(info.ModTime().UnixNano()),
	}
	require.NoError(t, database.UpsertSession(sess))
	require.NoError(t, database.SetSessionDataVersion(
		sess.ID, db.CurrentDataVersion(),
	))

	e := &Engine{
		db:       database,
		idPrefix: "host~",
		pathRewriter: func(path string) string {
			return "host:" + path
		},
	}

	assert.False(t, e.shouldSkipCodex(path, info),
		"stale generated roborev CI projects must be reparsed")
}

func TestProcessFileSkipCacheReparsesStaleCodexProject(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	path := filepath.Join(root, "rollout-2026-06-21T18-59-38-abc.jsonl")
	content := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			"abc",
			"/home/roborev/.roborev/ci-worktrees/agentsview/roborev-ci-28293-3831737461",
			"user",
			"2024-01-01T10:00:00Z",
		),
		testjsonl.CodexMsgJSON("user", "review this", "2024-01-01T10:00:01Z"),
	)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	info, err := os.Stat(path)
	require.NoError(t, err, "stat codex fixture")

	sess := db.Session{
		ID:        "host~codex:abc",
		Project:   "roborev_ci_28293_3831737461",
		Machine:   "host",
		Agent:     "codex",
		FilePath:  strPtr("host:" + path),
		FileSize:  int64Ptr(info.Size()),
		FileMtime: int64Ptr(info.ModTime().UnixNano()),
	}
	require.NoError(t, database.UpsertSession(sess))
	require.NoError(t, database.SetSessionDataVersion(
		sess.ID, db.CurrentDataVersion(),
	))

	e := &Engine{
		db:        database,
		idPrefix:  "host~",
		skipCache: map[string]int64{path: info.ModTime().UnixNano()},
		pathRewriter: func(path string) string {
			return "host:" + path
		},
	}

	res := e.processFile(context.Background(), parser.DiscoveredFile{
		Agent: parser.AgentCodex,
		Path:  path,
	})
	require.NoError(t, res.err)
	require.False(t, res.skip,
		"remote skip cache must not hide stale generated roborev CI projects")
	require.Len(t, res.results, 1)
	assert.Equal(t, "agentsview", res.results[0].Session.Project)
}

func TestProcessCodexAppendedStaleProjectDoesFullReparse(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	path := filepath.Join(root, "rollout-2026-06-21T18-59-38-abc.jsonl")
	initial := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			"abc",
			"/home/roborev/.roborev/ci-worktrees/agentsview/roborev-ci-28293-3831737461",
			"user",
			"2024-01-01T10:00:00Z",
		),
		testjsonl.CodexMsgJSON("user", "review this", "2024-01-01T10:00:01Z"),
	)
	require.NoError(t, os.WriteFile(path, []byte(initial), 0o600))
	info, err := os.Stat(path)
	require.NoError(t, err, "stat initial codex fixture")

	sess := db.Session{
		ID:               "host~codex:abc",
		Project:          "roborev_ci_28293_3831737461",
		Machine:          "host",
		Agent:            "codex",
		FirstMessage:     strPtr("review this"),
		MessageCount:     1,
		UserMessageCount: 1,
		FilePath:         strPtr("host:" + path),
		FileSize:         int64Ptr(info.Size()),
		FileMtime:        int64Ptr(info.ModTime().UnixNano()),
		NextOrdinal:      1,
	}
	require.NoError(t, database.UpsertSession(sess))
	require.NoError(t, database.SetSessionDataVersion(
		sess.ID, db.CurrentDataVersion(),
	))
	require.NoError(t, database.InsertMessages([]db.Message{
		{
			SessionID: "host~codex:abc",
			Ordinal:   0,
			Role:      "user",
			Content:   "review this",
		},
	}))

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err, "open codex fixture for append")
	_, err = f.WriteString(testjsonl.CodexMsgJSON(
		"assistant", "done", "2024-01-01T10:00:02Z",
	) + "\n")
	require.NoError(t, err, "append codex fixture")
	require.NoError(t, f.Close(), "close codex fixture")
	info, err = os.Stat(path)
	require.NoError(t, err, "stat appended codex fixture")

	e := &Engine{
		db:       database,
		idPrefix: "host~",
		pathRewriter: func(path string) string {
			return "host:" + path
		},
	}

	res := e.processCodex(parser.DiscoveredFile{
		Agent: parser.AgentCodex,
		Path:  path,
	}, info)
	require.NoError(t, res.err)
	require.Nil(t, res.incremental,
		"stale project metadata must force full parse even when file appended")
	require.Len(t, res.results, 1)
	assert.Equal(t, "agentsview", res.results[0].Session.Project)
}

func TestProcessCodexAppendedStaleProjectCarriesForceReplace(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	path := filepath.Join(root, "rollout-2026-06-21T18-59-38-abc.jsonl")
	initial := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			"abc",
			"/home/roborev/.roborev/ci-worktrees/agentsview/roborev-ci-28293-3831737461",
			"user",
			"2024-01-01T10:00:00Z",
		),
		testjsonl.CodexMsgJSON("user", "run command", "2024-01-01T10:00:01Z"),
		testjsonl.CodexFunctionCallWithCallIDJSON(
			"exec_command",
			"call_cmd",
			map[string]any{"cmd": "go test"},
			"2024-01-01T10:00:02Z",
		),
	)
	require.NoError(t, os.WriteFile(path, []byte(initial), 0o600))
	info, err := os.Stat(path)
	require.NoError(t, err, "stat initial codex fixture")

	sess := db.Session{
		ID:               "host~codex:abc",
		Project:          "roborev_ci_28293_3831737461",
		Machine:          "host",
		Agent:            "codex",
		FirstMessage:     strPtr("run command"),
		MessageCount:     2,
		UserMessageCount: 1,
		FilePath:         strPtr("host:" + path),
		FileSize:         int64Ptr(info.Size()),
		FileMtime:        int64Ptr(info.ModTime().UnixNano()),
		NextOrdinal:      2,
	}
	require.NoError(t, database.UpsertSession(sess))
	require.NoError(t, database.SetSessionDataVersion(
		sess.ID, db.CurrentDataVersion(),
	))
	require.NoError(t, database.InsertMessages([]db.Message{
		{
			SessionID: "host~codex:abc",
			Ordinal:   0,
			Role:      "user",
			Content:   "run command",
		},
		{
			SessionID: "host~codex:abc",
			Ordinal:   1,
			Role:      "assistant",
			ToolCalls: []db.ToolCall{
				{
					ToolUseID: "call_cmd",
					ToolName:  "exec_command",
					InputJSON: `{"cmd":"go test"}`,
				},
			},
		},
	}))

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err, "open codex fixture for append")
	_, err = f.WriteString(testjsonl.CodexFunctionCallOutputJSON(
		"call_cmd", `{"status":"ok"}`, "2024-01-01T10:00:03Z",
	) + "\n")
	require.NoError(t, err, "append codex fixture")
	require.NoError(t, f.Close(), "close codex fixture")
	info, err = os.Stat(path)
	require.NoError(t, err, "stat appended codex fixture")

	e := &Engine{
		db:       database,
		idPrefix: "host~",
		pathRewriter: func(path string) string {
			return "host:" + path
		},
	}

	res := e.processCodex(parser.DiscoveredFile{
		Agent: parser.AgentCodex,
		Path:  path,
	}, info)
	require.NoError(t, res.err)
	require.Nil(t, res.incremental,
		"stale project metadata must force full parse even when file appended")
	require.Len(t, res.results, 1)
	assert.Equal(t, "agentsview", res.results[0].Session.Project)
	assert.True(t, res.forceReplace,
		"fallback-triggering appended data must replace existing messages")
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

// writeAiderHistory writes a two-content-run plus one header-only-run
// history file under a fresh repo dir and returns its path. The header-only
// trailing run produces no session, exercising the HasMessages path of
// aiderFileUnchanged.
func writeAiderHistory(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "myrepo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	path := filepath.Join(repo, parser.AiderHistoryFileName())
	content := "# aider chat started at 2026-06-09 14:01:00\n" +
		"#### first prompt\nanswer one\n" +
		"# aider chat started at 2026-06-09 15:30:00\n" +
		"#### second prompt\nanswer two\n" +
		"# aider chat started at 2026-06-09 16:45:00\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

// insertAiderRunRow stores a session row for one aider virtual run path at
// the given size, mtime, and data version, mirroring what a real fan-out write
// produces. data_version is stamped separately because UpsertSession does
// not persist it. The stored size must match the history file's reported size
// for aiderFileUnchanged to treat the run as current.
func insertAiderRunRow(
	t *testing.T, database *db.DB,
	virtualPath string, size, mtime int64, dataVersion int,
) {
	t.Helper()
	id := "aider:" + virtualPath
	require.NoError(t, database.UpsertSession(db.Session{
		ID:        id,
		Project:   "myrepo",
		Machine:   "local",
		Agent:     string(parser.AgentAider),
		FilePath:  strPtr(virtualPath),
		FileSize:  int64Ptr(size),
		FileMtime: int64Ptr(mtime),
	}))
	require.NoError(t, database.SetSessionDataVersion(id, dataVersion))
}

// TestAiderFileUnchangedRequiresAllRuns is the MEDIUM-2 regression test:
// aiderFileUnchanged must skip a file only when EVERY content-bearing run
// row is current. Skipping on the first matching row (the old behavior)
// would strand runs that a partial batch never wrote or that went stale
// after a data-version bump, so they would never be repaired.
func TestAiderFileUnchangedRequiresAllRuns(t *testing.T) {
	const mtime = int64(1_700_000_000_000_000_000)
	const size = int64(4096)
	cur := db.CurrentDataVersion()

	metasFor := func(t *testing.T, path string) []parser.AiderRunMeta {
		t.Helper()
		metas, err := parser.ListAiderRunMetas(path)
		require.NoError(t, err)
		// Two content-bearing runs plus one header-only run.
		require.Len(t, metas, 3)
		require.True(t, metas[0].HasMessages)
		require.True(t, metas[1].HasMessages)
		require.False(t, metas[2].HasMessages)
		return metas
	}

	t.Run("all runs current -> skip", func(t *testing.T) {
		database := openTestDB(t)
		path := writeAiderHistory(t)
		metas := metasFor(t, path)
		// Both content runs have a current row. The header-only run has none,
		// and must not block the skip.
		insertAiderRunRow(t, database, metas[0].VirtualPath, size, mtime, cur)
		insertAiderRunRow(t, database, metas[1].VirtualPath, size, mtime, cur)

		e := &Engine{db: database}
		got := e.aiderFileUnchanged(path, fakeFileInfo{size: size, mtime: mtime})
		assert.True(t, got, "file with all run rows current must be skipped")
	})

	t.Run("rewritten remote run rows current -> skip", func(t *testing.T) {
		database := openTestDB(t)
		path := writeAiderHistory(t)
		metas := metasFor(t, path)
		rewriter := func(p string) string {
			return "host:" + p
		}
		// Remote sync stores the rewritten virtual run path, not the temp
		// extraction path returned by ListAiderRunMetas.
		insertAiderRunRow(t, database,
			rewriter(metas[0].VirtualPath), size, mtime, cur)
		insertAiderRunRow(t, database,
			rewriter(metas[1].VirtualPath), size, mtime, cur)

		e := &Engine{db: database, pathRewriter: rewriter}
		got := e.aiderFileUnchanged(path, fakeFileInfo{size: size, mtime: mtime})
		assert.True(t, got,
			"remote file with all rewritten run rows current must be skipped")
	})

	t.Run("one run row missing -> do not skip", func(t *testing.T) {
		database := openTestDB(t)
		path := writeAiderHistory(t)
		metas := metasFor(t, path)
		// Only the FIRST content run was written (a partial batch). Under the
		// old any-match logic this stranded the second run forever.
		insertAiderRunRow(t, database, metas[0].VirtualPath, size, mtime, cur)

		e := &Engine{db: database}
		got := e.aiderFileUnchanged(path, fakeFileInfo{size: size, mtime: mtime})
		assert.False(t, got,
			"a missing run row must force a re-parse to repair it")
	})

	t.Run("one run row stale data version -> do not skip", func(t *testing.T) {
		database := openTestDB(t)
		path := writeAiderHistory(t)
		metas := metasFor(t, path)
		insertAiderRunRow(t, database, metas[0].VirtualPath, size, mtime, cur)
		// The second run was resynced under an OLDER data version while the
		// first is current. The file must still re-parse.
		insertAiderRunRow(t, database, metas[1].VirtualPath, size, mtime, cur-1)

		e := &Engine{db: database}
		got := e.aiderFileUnchanged(path, fakeFileInfo{size: size, mtime: mtime})
		assert.False(t, got,
			"a stale data-version run row must force a re-parse")
	})

	t.Run("one run row stale mtime -> do not skip", func(t *testing.T) {
		database := openTestDB(t)
		path := writeAiderHistory(t)
		metas := metasFor(t, path)
		insertAiderRunRow(t, database, metas[0].VirtualPath, size, mtime, cur)
		insertAiderRunRow(t, database, metas[1].VirtualPath, size, mtime-1, cur)

		e := &Engine{db: database}
		got := e.aiderFileUnchanged(path, fakeFileInfo{size: size, mtime: mtime})
		assert.False(t, got,
			"a run row with a different mtime must force a re-parse")
	})

	t.Run("one run row stale size -> do not skip", func(t *testing.T) {
		database := openTestDB(t)
		path := writeAiderHistory(t)
		metas := metasFor(t, path)
		insertAiderRunRow(t, database, metas[0].VirtualPath, size, mtime, cur)
		// The second run row has the SAME mtime but a different stored size,
		// modeling a same-mtime append/truncate. Ignoring size would wrongly
		// skip the file and strand the appended/removed runs.
		insertAiderRunRow(t, database, metas[1].VirtualPath, size-1, mtime, cur)

		e := &Engine{db: database}
		got := e.aiderFileUnchanged(path, fakeFileInfo{size: size, mtime: mtime})
		assert.False(t, got,
			"a run row with a different size must force a re-parse")
	})
}

// TestProcessAiderForceParseReparsesUnchangedFile is the forced-reparse
// regression test: under forceParse (parse-diff), processAider must NOT take
// the aiderFileUnchanged skip even when every run row is current, so a forced
// run re-reads already-synced aider files instead of stranding them.
func TestProcessAiderForceParseReparsesUnchangedFile(t *testing.T) {
	database := openTestDB(t)
	path := writeAiderHistory(t)
	info, err := os.Stat(path)
	require.NoError(t, err)
	// processAider stats the real file, so the stored rows must carry the
	// file's actual size and mtime for the unchanged-skip to fire.
	size := info.Size()
	mtime := info.ModTime().UnixNano()
	cur := db.CurrentDataVersion()

	metas, err := parser.ListAiderRunMetas(path)
	require.NoError(t, err)
	require.Len(t, metas, 3)
	// Mark every content-bearing run as current so the non-forced path skips.
	insertAiderRunRow(t, database, metas[0].VirtualPath, size, mtime, cur)
	insertAiderRunRow(t, database, metas[1].VirtualPath, size, mtime, cur)

	file := parser.DiscoveredFile{Path: path, Agent: parser.AgentAider}

	// Sanity: without forceParse the unchanged file is skipped.
	normal := &Engine{db: database, machine: "local"}
	skipRes := normal.processAider(file, info)
	require.True(t, skipRes.skip,
		"without forceParse an unchanged aider file must be skipped")
	require.Empty(t, skipRes.results)

	// With forceParse the file must be reparsed, not skipped.
	forced := &Engine{db: database, machine: "local", forceParse: true}
	forcedRes := forced.processAider(file, info)
	require.NoError(t, forcedRes.err)
	assert.False(t, forcedRes.skip,
		"forceParse must reparse an unchanged aider file, not skip it")
	assert.Len(t, forcedRes.results, 2,
		"forced reparse must fan out one result per content-bearing run")
}

// TestStripVirtualSourceSuffixAider verifies that an aider
// <history>#<runIdx> virtual path strips back to its physical history file,
// so parse-diff missing-run and parse-error reporting keys on the on-disk
// file rather than the run-scoped virtual path.
func TestStripVirtualSourceSuffixAider(t *testing.T) {
	historyPath := "/home/user/myrepo/" + parser.AiderHistoryFileName()
	virtual := parser.AiderVirtualPath(historyPath, 3)
	assert.Equal(t, historyPath, stripVirtualSourceSuffix(virtual),
		"the run-index suffix must strip to the physical history path")
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

	t.Run("stripped control bytes cannot pad parsed content", func(t *testing.T) {
		stored := []db.Message{
			{
				Ordinal:       1,
				Role:          "assistant",
				Content:       "complete archived content",
				ContentLength: len("complete archived content"),
			},
		}
		parsed := []db.Message{
			{
				Ordinal:       1,
				Role:          "assistant",
				Content:       "short" + strings.Repeat("\x00", 20),
				ContentLength: len("complete archived content"),
			},
		}

		require.True(t, openCodeLegacyArchiveLooksIncomplete(parsed, stored),
			"want sanitized parsed content to preserve complete archive")
	})
}

func TestVisualStudioCopilotArchiveDecisionMergesNewRowsWithArchiveOnlyRows(t *testing.T) {
	stored := []db.Message{
		{
			Ordinal:       0,
			Role:          "assistant",
			Content:       "Run command: dotnet build",
			ContentLength: len("Run command: dotnet build"),
			Timestamp:     "2026-06-12T19:46:40Z",
		},
		{
			Ordinal:       1,
			Role:          "user",
			Content:       "Archived prompt.",
			ContentLength: len("Archived prompt."),
			Timestamp:     "2026-06-12T19:47:00Z",
		},
	}
	parsed := []db.Message{
		{
			Ordinal:       0,
			Role:          "assistant",
			Content:       "Run command: dotnet build",
			ContentLength: len("Run command: dotnet build"),
			Timestamp:     "2026-06-12T19:46:40Z",
		},
		{
			Ordinal:       1,
			Role:          "user",
			Content:       "New follow-up.",
			ContentLength: len("New follow-up."),
			Timestamp:     "2026-06-12T19:47:20Z",
		},
	}

	decision := visualStudioCopilotArchiveDecision(parsed, stored)

	require.False(t, decision.preserve)
	require.Len(t, decision.merged, 3)
	assert.Equal(t, "Run command: dotnet build", decision.merged[0].Content)
	assert.Equal(t, "Archived prompt.", decision.merged[1].Content)
	assert.Equal(t, "New follow-up.", decision.merged[2].Content)
	for i, msg := range decision.merged {
		assert.Equal(t, i, msg.Ordinal)
	}
}

// TestPrepareSessionWriteReclampsMessageDerivedTokenTotals proves the full
// write path does not strand a corrupt per-message token value in the session
// aggregates. A message with an implausible OutputTokens/ContextTokens is
// clamped to maxPlausibleTokens in its row, so the message-derived session
// totals must be re-derived from the clamped rows -- while a legitimately
// large sum over many messages (above the per-message bound) is preserved.
func TestPrepareSessionWriteReclampsMessageDerivedTokenTotals(t *testing.T) {
	d := openTestDB(t)
	e := NewEngine(d, EngineConfig{Machine: "test-machine"})
	ts := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)

	msgs := []parser.ParsedMessage{
		{
			Ordinal: 0, Role: parser.RoleAssistant, Content: "a",
			ContentLength: 1, Timestamp: ts,
			OutputTokens: 1_000_000, HasOutputTokens: true,
			ContextTokens: 1_500_000, HasContextTokens: true,
		},
		{
			Ordinal: 1, Role: parser.RoleAssistant, Content: "b",
			ContentLength: 1, Timestamp: ts.Add(time.Second),
			OutputTokens: 1_500_000, HasOutputTokens: true,
			ContextTokens: 1_000_000, HasContextTokens: true,
		},
		{
			Ordinal: 2, Role: parser.RoleAssistant, Content: "c",
			ContentLength: 1, Timestamp: ts.Add(2 * time.Second),
			// Corrupt: both counts are far above maxPlausibleTokens and
			// will be clamped to it in the stored row.
			OutputTokens: 999_999_999, HasOutputTokens: true,
			ContextTokens: 999_999_999, HasContextTokens: true,
		},
	}

	newSess := func() parser.ParsedSession {
		return parser.ParsedSession{
			ID: "tok-session", Project: "proj", Machine: "test-machine",
			Agent: parser.AgentClaude, StartedAt: ts,
			EndedAt: ts.Add(time.Minute), MessageCount: len(msgs),
			File: parser.FileInfo{
				Path: "/tmp/tok.jsonl", Size: 10, Mtime: ts.UnixNano(),
			},
		}
	}

	// Message-derived totals (the parser accumulated them via
	// accumulateMessageTokenUsage): sum of output and peak context, raw.
	sess := newSess()
	sess.TotalOutputTokens = 1_000_000 + 1_500_000 + 999_999_999
	sess.HasTotalOutputTokens = true
	sess.PeakContextTokens = 999_999_999
	sess.HasPeakContextTokens = true

	prepared, dbMsgs, ok := e.prepareSessionWrite(
		pendingWrite{sess: sess, msgs: msgs}, nil,
	)
	require.True(t, ok)
	require.Len(t, dbMsgs, 3)

	// The corrupt message row is clamped to the per-message bound.
	assert.Equal(t, maxPlausibleTokens, dbMsgs[2].OutputTokens,
		"corrupt message OutputTokens clamped")
	assert.Equal(t, maxPlausibleTokens, dbMsgs[2].ContextTokens,
		"corrupt message ContextTokens clamped")
	// The session total is re-derived from the clamped rows: a legitimately
	// large sum (above maxPlausibleTokens) survives, the corrupt value does
	// not pollute it.
	assert.Equal(t, 1_000_000+1_500_000+maxPlausibleTokens,
		prepared.TotalOutputTokens,
		"message-derived total re-derived from clamped rows")
	assert.Equal(t, maxPlausibleTokens, prepared.PeakContextTokens,
		"message-derived peak re-derived from clamped rows")

	// Summary-derived totals (agents like Warp/Vibe set the session totals
	// directly, not from per-message rows) must survive the per-message
	// clamp untouched: they do not match the message-derived values.
	const summaryTotal = 4_242_424
	const summaryPeak = 3_333_333
	summarySess := newSess()
	summarySess.TotalOutputTokens = summaryTotal
	summarySess.HasTotalOutputTokens = true
	summarySess.PeakContextTokens = summaryPeak
	summarySess.HasPeakContextTokens = true

	preparedSummary, _, ok := e.prepareSessionWrite(
		pendingWrite{sess: summarySess, msgs: msgs}, nil,
	)
	require.True(t, ok)
	assert.Equal(t, summaryTotal, preparedSummary.TotalOutputTokens,
		"summary-derived total left untouched by per-message clamp")
	assert.Equal(t, summaryPeak, preparedSummary.PeakContextTokens,
		"summary-derived peak left untouched by per-message clamp")
}

// TestPrepareSessionWriteReclampsEventDerivedTokenTotals covers the
// usage-event-derived case (VS Code Copilot accumulates session totals from
// per-turn usage events, not per-message rows). A corrupt usage event is
// clamped in its usage_events row, so the event-derived session aggregates
// must be re-derived from the clamped events rather than left at the raw
// inflated value.
func TestPrepareSessionWriteReclampsEventDerivedTokenTotals(t *testing.T) {
	d := openTestDB(t)
	e := NewEngine(d, EngineConfig{Machine: "test-machine"})
	ts := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)

	// Per-message rows carry no tokens; the tokens live in usage events.
	msgs := []parser.ParsedMessage{
		{
			Ordinal: 0, Role: parser.RoleUser, Content: "q",
			ContentLength: 1, Timestamp: ts,
		},
		{
			Ordinal: 1, Role: parser.RoleAssistant, Content: "a",
			ContentLength: 1, Timestamp: ts.Add(time.Second),
		},
	}
	events := []parser.ParsedUsageEvent{
		{OutputTokens: 1_000_000, InputTokens: 1_500_000},
		{OutputTokens: 1_500_000, InputTokens: 1_000_000},
		// Corrupt event: both counts are clamped in the usage_events row.
		{OutputTokens: 999_999_999, InputTokens: 999_999_999},
	}
	sess := parser.ParsedSession{
		ID: "evt-session", Project: "proj", Machine: "test-machine",
		Agent: parser.AgentVSCodeCopilot, StartedAt: ts,
		EndedAt: ts.Add(time.Minute), MessageCount: len(msgs),
		File: parser.FileInfo{
			Path: "/tmp/evt.json", Size: 10, Mtime: ts.UnixNano(),
		},
		// Event-derived aggregates, raw (as the parser accumulates them):
		// sum of event output tokens, peak of event input tokens.
		TotalOutputTokens:    1_000_000 + 1_500_000 + 999_999_999,
		HasTotalOutputTokens: true,
		PeakContextTokens:    999_999_999,
		HasPeakContextTokens: true,
	}

	prepared, _, ok := e.prepareSessionWrite(
		pendingWrite{sess: sess, msgs: msgs, usageEvents: events}, nil,
	)
	require.True(t, ok)
	assert.Equal(t, 1_000_000+1_500_000+maxPlausibleTokens,
		prepared.TotalOutputTokens,
		"event-derived total re-derived from clamped usage events")
	assert.Equal(t, maxPlausibleTokens, prepared.PeakContextTokens,
		"event-derived peak re-derived from clamped usage events")
}

func TestPrepareSessionWritePreservesSummaryUsageEventTokenTotals(t *testing.T) {
	d := openTestDB(t)
	e := NewEngine(d, EngineConfig{Machine: "test-machine"})
	ts := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)

	rawTotal := maxPlausibleTokens + 500_000
	rawPeak := maxPlausibleTokens + 250_000
	msgs := []parser.ParsedMessage{
		{
			Ordinal: 0, Role: parser.RoleUser, Content: "q",
			ContentLength: 1, Timestamp: ts,
		},
		{
			Ordinal: 1, Role: parser.RoleAssistant, Content: "a",
			ContentLength: 1, Timestamp: ts.Add(time.Second),
		},
	}
	events := []parser.ParsedUsageEvent{{
		Source:       "session",
		Model:        "claude-sonnet-4",
		InputTokens:  rawPeak,
		OutputTokens: rawTotal,
	}}
	sess := parser.ParsedSession{
		ID: "summary-event-session", Project: "proj", Machine: "test-machine",
		Agent: parser.AgentHermes, StartedAt: ts,
		EndedAt: ts.Add(time.Minute), MessageCount: len(msgs),
		File: parser.FileInfo{
			Path: "/tmp/summary.json", Size: 10, Mtime: ts.UnixNano(),
		},
		TotalOutputTokens:    rawTotal,
		HasTotalOutputTokens: true,
		PeakContextTokens:    rawPeak,
		HasPeakContextTokens: true,
	}

	prepared, _, ok := e.prepareSessionWrite(
		pendingWrite{sess: sess, msgs: msgs, usageEvents: events}, nil,
	)
	require.True(t, ok)
	assert.Equal(t, rawTotal, prepared.TotalOutputTokens,
		"session-summary usage event must not make the session aggregate event-derived")
	assert.Equal(t, rawPeak, prepared.PeakContextTokens,
		"summary-derived peak context must survive the per-row event clamp")
}

// TestPrepareSessionWriteReclampsEventDerivedCacheContext covers an
// event-derived peak context that, like the parser-side rollup, sums input and
// cache tokens. A corrupt cache value is clamped per-component in its
// usage_events row, so the event-derived peak must be re-derived from the
// clamped components rather than left at the raw inflated value.
func TestPrepareSessionWriteReclampsEventDerivedCacheContext(t *testing.T) {
	d := openTestDB(t)
	e := NewEngine(d, EngineConfig{Machine: "test-machine"})
	ts := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)

	msgs := []parser.ParsedMessage{
		{
			Ordinal: 0, Role: parser.RoleUser, Content: "q",
			ContentLength: 1, Timestamp: ts,
		},
		{
			Ordinal: 1, Role: parser.RoleAssistant, Content: "a",
			ContentLength: 1, Timestamp: ts.Add(time.Second),
		},
	}
	// Per-event context = input + cache-creation + cache-read.
	events := []parser.ParsedUsageEvent{
		{OutputTokens: 100_000, InputTokens: 1_000_000, CacheReadInputTokens: 500_000},
		{OutputTokens: 100_000, InputTokens: 800_000, CacheCreationInputTokens: 200_000},
		// Corrupt event: each component is clamped to maxPlausibleTokens.
		{
			OutputTokens: 999_999_999, InputTokens: 999_999_999,
			CacheReadInputTokens: 999_999_999,
		},
	}
	rawTotal := 100_000 + 100_000 + 999_999_999
	rawPeak := 999_999_999 + 999_999_999 // the corrupt event's input+cache
	sess := parser.ParsedSession{
		ID: "evt-cache-session", Project: "proj", Machine: "test-machine",
		Agent: parser.AgentVSCodeCopilot, StartedAt: ts,
		EndedAt: ts.Add(time.Minute), MessageCount: len(msgs),
		File: parser.FileInfo{
			Path: "/tmp/evtcache.json", Size: 10, Mtime: ts.UnixNano(),
		},
		TotalOutputTokens:    rawTotal,
		HasTotalOutputTokens: true,
		PeakContextTokens:    rawPeak,
		HasPeakContextTokens: true,
	}

	prepared, _, ok := e.prepareSessionWrite(
		pendingWrite{sess: sess, msgs: msgs, usageEvents: events}, nil,
	)
	require.True(t, ok)
	assert.Equal(t, 100_000+100_000+maxPlausibleTokens,
		prepared.TotalOutputTokens,
		"event-derived total re-derived from clamped output tokens")
	// Peak = the corrupt event's input + cache-read, each clamped to the
	// per-row bound: 2M + 2M. The sum is not clamped to the per-row bound.
	assert.Equal(t, maxPlausibleTokens+maxPlausibleTokens,
		prepared.PeakContextTokens,
		"event-derived peak re-derived from clamped input+cache components")
}

// TestPrepareSessionWriteReclampsEventDerivedMixedSignTokens covers the
// parser rollup semantics shared with parser.UsageEventTokenAggregate: only
// positive output is summed and only positive context contributes to the peak.
// A mix of a negative (corrupt) event and an over-bound event must still be
// recognized as event-derived and re-derived from the clamped rows.
func TestPrepareSessionWriteReclampsEventDerivedMixedSignTokens(t *testing.T) {
	d := openTestDB(t)
	e := NewEngine(d, EngineConfig{Machine: "test-machine"})
	ts := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)

	msgs := []parser.ParsedMessage{
		{
			Ordinal: 0, Role: parser.RoleUser, Content: "q",
			ContentLength: 1, Timestamp: ts,
		},
		{
			Ordinal: 1, Role: parser.RoleAssistant, Content: "a",
			ContentLength: 1, Timestamp: ts.Add(time.Second),
		},
	}
	events := []parser.ParsedUsageEvent{
		{OutputTokens: 1_000_000, InputTokens: 1_500_000},
		// Corrupt negative event: excluded from the positive-only rollup.
		{OutputTokens: -5, InputTokens: -10},
		// Over-bound event: clamped to the per-row cap.
		{OutputTokens: 999_999_999, InputTokens: 999_999_999},
	}
	// Raw rollup (positive-only): output 1M + 999,999,999; peak context
	// max(1.5M, 999,999,999).
	rawTotal := 1_000_000 + 999_999_999
	rawPeak := 999_999_999
	sess := parser.ParsedSession{
		ID: "evt-mixed-session", Project: "proj", Machine: "test-machine",
		Agent: parser.AgentVSCodeCopilot, StartedAt: ts,
		EndedAt: ts.Add(time.Minute), MessageCount: len(msgs),
		File: parser.FileInfo{
			Path: "/tmp/evtmixed.json", Size: 10, Mtime: ts.UnixNano(),
		},
		TotalOutputTokens:    rawTotal,
		HasTotalOutputTokens: true,
		PeakContextTokens:    rawPeak,
		HasPeakContextTokens: true,
	}

	prepared, _, ok := e.prepareSessionWrite(
		pendingWrite{sess: sess, msgs: msgs, usageEvents: events}, nil,
	)
	require.True(t, ok)
	// Negative output floors to 0 (dropped); over-bound output clamps to 2M.
	assert.Equal(t, 1_000_000+maxPlausibleTokens, prepared.TotalOutputTokens,
		"negative event excluded, over-bound event clamped in event total")
	assert.Equal(t, maxPlausibleTokens, prepared.PeakContextTokens,
		"event peak re-derived from clamped positive context")
}

// TestVisualStudioCopilotArchiveCompareUsesSanitizedParsed guards against a
// truncated reparse padded with control bytes bypassing archive preservation.
// Stored rows are sanitized and length-adjusted on write, so the reconcile must
// measure the parsed side the same way rather than against its raw length.
func TestVisualStudioCopilotArchiveCompareUsesSanitizedParsed(t *testing.T) {
	stored := db.Message{
		Role:          "assistant",
		Content:       "complete answer",
		ContentLength: len("complete answer"),
	}
	// Genuinely truncated content ("trunc"), padded with BEL control bytes so
	// the RAW length (25) exceeds the stored length (15); sanitized it is 5.
	raw := "trunc" + strings.Repeat("\x07", 20)
	require.Greater(t, len(raw), stored.ContentLength,
		"raw length must look long enough to expose the bug")
	truncated := db.Message{Role: "assistant", Content: raw, ContentLength: len(raw)}

	assert.True(t,
		visualStudioCopilotMessageLooksIncomplete(truncated, stored),
		"truncated reparse padded with control bytes must be incomplete")

	// A reparse that differs only by stripped control bytes is neither
	// incomplete nor an archive update: "complete\x07 answer" sanitizes to the
	// stored "complete answer".
	withControl := db.Message{
		Role:          "assistant",
		Content:       "complete\x07 answer",
		ContentLength: len("complete\x07 answer"),
	}
	assert.False(t,
		visualStudioCopilotMessageLooksIncomplete(withControl, stored),
		"stripped-control reparse of equal text is not incomplete")
	assert.False(t,
		visualStudioCopilotMessageHasArchiveUpdate(withControl, stored),
		"stripped-control reparse of equal text is not an archive update")
}

func TestVisualStudioCopilotArchiveDecisionMatchesTimestampShiftedToolCall(t *testing.T) {
	stored := []db.Message{
		{
			Ordinal:       0,
			Role:          "assistant",
			Content:       "Run command: dotnet build",
			ContentLength: len("Run command: dotnet build"),
			Timestamp:     "2026-06-12T19:46:40Z",
			ToolCalls: []db.ToolCall{{
				ToolName:  "run_command_in_terminal",
				ToolUseID: "call_build",
			}},
		},
		{
			Ordinal:       1,
			Role:          "user",
			Content:       "Archived prompt.",
			ContentLength: len("Archived prompt."),
			Timestamp:     "2026-06-12T19:47:00Z",
		},
	}
	parsed := []db.Message{{
		Ordinal:       0,
		Role:          "assistant",
		Content:       "Run command: dotnet build",
		ContentLength: len("Run command: dotnet build"),
		Timestamp:     "2026-06-12T19:47:40Z",
		ToolCalls: []db.ToolCall{{
			ToolName:  "run_command_in_terminal",
			ToolUseID: "call_build",
			ResultEvents: []db.ToolResultEvent{{
				ToolUseID:     "call_build",
				Source:        "visualstudio-copilot",
				Status:        "completed",
				Content:       "Build succeeded.",
				ContentLength: len("Build succeeded."),
			}},
		}},
	}}

	decision := visualStudioCopilotArchiveDecision(parsed, stored)

	require.False(t, decision.preserve)
	require.Len(t, decision.merged, 2)
	assert.Equal(t, "Run command: dotnet build", decision.merged[0].Content)
	assert.Equal(t, "2026-06-12T19:46:40Z", decision.merged[0].Timestamp,
		"fallback merge should preserve the archived transcript anchor")
	require.Len(t, decision.merged[0].ToolCalls, 1)
	require.Len(t, decision.merged[0].ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, "Build succeeded.",
		decision.merged[0].ToolCalls[0].ResultEvents[0].Content)
	assert.Equal(t, "Archived prompt.", decision.merged[1].Content)
}

func TestVisualStudioCopilotArchiveDecisionMergesOnlyTimestampShiftedToolCall(t *testing.T) {
	stored := []db.Message{{
		Ordinal:       0,
		Role:          "assistant",
		Content:       "Run command: dotnet build",
		ContentLength: len("Run command: dotnet build"),
		Timestamp:     "2026-06-12T19:46:40Z",
		ToolCalls: []db.ToolCall{{
			ToolName:  "run_command_in_terminal",
			ToolUseID: "call_build",
		}},
	}}
	parsed := []db.Message{{
		Ordinal:       0,
		Role:          "assistant",
		Content:       "Run command: dotnet build",
		ContentLength: len("Run command: dotnet build"),
		Timestamp:     "2026-06-12T19:47:40Z",
		ToolCalls: []db.ToolCall{{
			ToolName:  "run_command_in_terminal",
			ToolUseID: "call_build",
		}},
	}}

	decision := visualStudioCopilotArchiveDecision(parsed, stored)

	require.False(t, decision.preserve)
	require.Len(t, decision.merged, 1)
	assert.Equal(t, "2026-06-12T19:46:40Z", decision.merged[0].Timestamp)
}

func TestVisualStudioCopilotArchiveDecisionMatchesTimestampShiftedUserPrompt(t *testing.T) {
	stored := []db.Message{
		{
			Ordinal:       0,
			Role:          "user",
			Content:       "Archived prompt.",
			ContentLength: len("Archived prompt."),
			Timestamp:     "2026-06-12T19:46:40Z",
		},
		{
			Ordinal:       1,
			Role:          "assistant",
			Content:       "Archived answer.",
			ContentLength: len("Archived answer."),
			Timestamp:     "2026-06-12T19:47:00Z",
		},
	}
	parsed := []db.Message{{
		Ordinal:       0,
		Role:          "user",
		Content:       "Archived prompt.",
		ContentLength: len("Archived prompt."),
		Timestamp:     "2026-06-12T19:47:40Z",
	}}

	decision := visualStudioCopilotArchiveDecision(parsed, stored)

	require.False(t, decision.preserve)
	require.Len(t, decision.merged, 2)
	assert.Equal(t, "Archived prompt.", decision.merged[0].Content)
	assert.Equal(t, "2026-06-12T19:46:40Z", decision.merged[0].Timestamp)
	assert.Equal(t, "Archived answer.", decision.merged[1].Content)
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

// TestEngine_ClassifyPathsOpenCodeSQLiteWALFile covers a WAL-file change on
// a pure-SQLite OpenCode root. OpenCode is provider-authoritative, so the
// provider facade classifies the change into the per-session SQLite virtual
// paths it would re-parse rather than the raw opencode.db path.
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
	seedOpenCodeSQLiteSession(t, dbPath, "ses_wal")
	walPath := filepath.Join(opencodeDir, "opencode.db-wal")
	require.NoError(t, os.WriteFile(walPath, []byte("wal"), 0o644), "WriteFile(%q)", walPath)

	files := engine.classifyPaths([]string{walPath})
	require.Len(t, files, 1)
	assert.Equal(t,
		parser.OpenCodeSQLiteVirtualPath(dbPath, "ses_wal"),
		files[0].Path,
	)
	assert.Equal(t, parser.AgentOpenCode, files[0].Agent)
}

// seedOpenCodeSQLiteSession creates a minimal OpenCode-shaped SQLite database
// with a single session row so changed-path classification can enumerate it.
func seedOpenCodeSQLiteSession(t *testing.T, dbPath, sessionID string) {
	t.Helper()
	d, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err, "open opencode db")
	t.Cleanup(func() { d.Close() })
	_, err = d.Exec(`
		CREATE TABLE project (id TEXT PRIMARY KEY, worktree TEXT NOT NULL);
		CREATE TABLE session (
			id TEXT PRIMARY KEY,
			project_id TEXT NOT NULL,
			parent_id TEXT,
			title TEXT,
			time_created INTEGER NOT NULL,
			time_updated INTEGER NOT NULL
		);
		CREATE TABLE message (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			data TEXT NOT NULL,
			time_created INTEGER NOT NULL
		);
		CREATE TABLE part (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			message_id TEXT NOT NULL,
			data TEXT NOT NULL,
			time_created INTEGER NOT NULL
		);
	`)
	require.NoError(t, err, "create opencode schema")
	_, err = d.Exec(
		"INSERT INTO project (id, worktree) VALUES ('prj_1', '/home/user/code/app')",
	)
	require.NoError(t, err, "insert project")
	_, err = d.Exec(
		`INSERT INTO session (id, project_id, time_created, time_updated)
		 VALUES (?, 'prj_1', 1, 2)`,
		sessionID,
	)
	require.NoError(t, err, "insert session")
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

// TestEngine_ClassifyPathsOpenCodeFamilyRemovedSessionFile covers a removed
// storage session file for the provider-authoritative OpenCode-format agents.
// A delete event yields no reparse classification: there is no source to
// re-read, and the deletion is reconciled by the presence sweep rather than
// changed-path classification.
func TestEngine_ClassifyPathsOpenCodeFamilyRemovedSessionFile(
	t *testing.T,
) {
	for _, tc := range []struct {
		name          string
		agent         parser.AgentType
		sessionSubdir string
	}{
		{name: "opencode", agent: parser.AgentOpenCode, sessionSubdir: "session"},
		{name: "kilo", agent: parser.AgentKilo, sessionSubdir: "session"},
		{name: "mimocode", agent: parser.AgentMiMoCode, sessionSubdir: "session_diff"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)
			root := t.TempDir()
			engine := NewEngine(db, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{
					tc.agent: {root},
				},
				Machine: "local",
			})

			sessionPath := filepath.Join(
				root, "storage", tc.sessionSubdir, "global",
				"ses_removed.json",
			)
			require.NoError(
				t, os.MkdirAll(filepath.Dir(sessionPath), 0o755),
				"MkdirAll(%q)", sessionPath,
			)
			require.NoError(
				t,
				os.WriteFile(
					sessionPath,
					[]byte(`{"id":"ses_removed","directory":"/tmp/proj","time":{"created":1,"updated":2}}`),
					0o644,
				),
				"WriteFile(%q)", sessionPath,
			)
			require.NoError(t, os.Remove(sessionPath), "Remove(%q)", sessionPath)

			files := engine.classifyPaths([]string{sessionPath})
			assert.Empty(t, files)
		})
	}
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
func TestEngine_ClassifyPathsQwenPawRejectsColon(t *testing.T) {
	if runtime.GOOS == "windows" {
		// ":" is invalid in Windows filenames, so colon-bearing
		// workspace/subdir/stem paths cannot be created there.
		t.Skip("':' is invalid in Windows filenames")
	}
	db := openTestDB(t)
	qwenpawDir := t.TempDir()
	engine := NewEngine(db, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentQwenPaw: {qwenpawDir},
		},
		Machine: "local",
	})

	write := func(parts ...string) string {
		p := filepath.Join(append([]string{qwenpawDir}, parts...)...)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte("{}"), 0o644))
		return p
	}

	rootPath := write("default", "sessions", "ok.json")
	subPath := write("default", "sessions", "console", "ok.json")
	// ":" in the workspace, subdir, or stem makes the joined ID
	// ambiguous, so these must not classify.
	colonWorkspace := write("ws:bad", "sessions", "ok.json")
	colonSubdir := write("default", "sessions", "sub:bad", "ok.json")
	colonStem := write("default", "sessions", "foo:bar.json")

	files := engine.classifyPaths([]string{rootPath, subPath})
	require.Len(t, files, 2)
	for _, f := range files {
		assert.Equal(t, parser.AgentQwenPaw, f.Agent)
		assert.Equal(t, "default", f.Project)
	}

	got := engine.classifyPaths([]string{
		colonWorkspace, colonSubdir, colonStem,
	})
	assert.Empty(t, got,
		"colon-containing ID parts must not classify: %v", got)
}

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

func TestEngine_ClassifyPathsDeepSeekTUISession(t *testing.T) {
	db := openTestDB(t)
	deepSeekDir := t.TempDir()
	engine := NewEngine(db, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentDeepSeekTUI: {deepSeekDir},
		},
		Machine: "local",
	})

	sessionID := "adc026b4-c620-43e4-8cc4-295593889d18"
	sessionPath := filepath.Join(deepSeekDir, sessionID+".json")
	dbtest.WriteTestFile(t, sessionPath, []byte("{}"))

	files := engine.classifyPaths([]string{sessionPath})
	require.Len(t, files, 1, "len(files) = %d, want 1 (%v)", len(files), files)
	assert.Equal(t, sessionPath, files[0].Path)
	assert.Equal(t, parser.AgentDeepSeekTUI, files[0].Agent)
	assert.True(t, files[0].ProviderProcess)
	require.NotNil(t, files[0].ProviderSource)
	assert.Equal(t, sessionPath, files[0].ProviderSource.DisplayPath)

	bogus := []string{
		filepath.Join(deepSeekDir, "stray.jsonl"),
		filepath.Join(deepSeekDir, "latest.json"),
		filepath.Join(deepSeekDir, "offline_queue.json"),
		filepath.Join(deepSeekDir, "nested", sessionID+".json"),
		filepath.Join(deepSeekDir, "..bad.json"),
	}
	for _, p := range bogus {
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755), "MkdirAll(%q)", p)
		dbtest.WriteTestFile(t, p, []byte("{}"))
	}
	got := engine.classifyPaths(bogus)
	assert.Empty(t, got, "expected no DeepSeek TUI classifications for %v, got %v", bogus, got)
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
	// Command Code is provider-authoritative: classification attaches a
	// provider source and recomputes the project during parse, so the
	// classification carries no informational project hint.
	assert.Empty(t, files[0].Project)
	require.NotNil(t, files[0].ProviderSource)

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

func TestEngine_ClassifyOnePathReasonixProjectBareMeta(t *testing.T) {
	db := openTestDB(t)
	reasonixDir := t.TempDir()
	engine := NewEngine(db, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentReasonix: {reasonixDir},
		},
		Machine: "local",
	})

	sessionPath := filepath.Join(
		reasonixDir, "projects", "proj", "sessions", "session-123.jsonl",
	)
	metaPath := sessionPath + ".meta"
	dbtest.WriteTestFile(t, sessionPath, []byte(`{"role":"user","content":"hi"}`))
	dbtest.WriteTestFile(t, metaPath, []byte(`{"model":"claude"}`))

	got, ok := engine.classifyOnePath(metaPath, nil)
	require.True(t, ok, "expected Reasonix sidecar to classify")
	assert.Equal(t, sessionPath, got.Path)
	assert.Equal(t, "proj", got.Project)
	assert.Equal(t, parser.AgentReasonix, got.Agent)
}

func TestEngine_ClassifyOnePathReasonixDeletedMeta(t *testing.T) {
	db := openTestDB(t)
	reasonixDir := t.TempDir()
	engine := NewEngine(db, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentReasonix: {reasonixDir},
		},
		Machine: "local",
	})

	sessionPath := filepath.Join(
		reasonixDir, "projects", "proj", "sessions", "session-123.jsonl",
	)
	metaPath := sessionPath + ".meta"
	dbtest.WriteTestFile(t, sessionPath, []byte(`{"role":"user","content":"hi"}`))

	got, ok := engine.classifyOnePath(metaPath, nil)
	require.True(t, ok, "expected deleted Reasonix sidecar to classify")
	assert.Equal(t, sessionPath, got.Path)
	assert.Equal(t, "proj", got.Project)
	assert.Equal(t, parser.AgentReasonix, got.Agent)
}

func TestEngine_ClassifyOnePathReasonixDeletedTranscriptIgnored(t *testing.T) {
	db := openTestDB(t)
	reasonixDir := t.TempDir()
	engine := NewEngine(db, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentReasonix: {reasonixDir},
		},
		Machine: "local",
	})

	sessionPath := filepath.Join(
		reasonixDir, "projects", "proj", "sessions", "session-123.jsonl",
	)

	_, ok := engine.classifyOnePath(sessionPath, nil)
	assert.False(t, ok, "expected deleted Reasonix transcript to be ignored")
}

func TestEngine_SyncPathsReasonixMetadataOnlySessionFieldUpdate(t *testing.T) {
	db := openTestDB(t)
	reasonixDir := t.TempDir()
	engine := NewEngine(db, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentReasonix: {reasonixDir},
		},
		Machine: "local",
	})

	sessionPath := filepath.Join(reasonixDir, "sessions", "session-123.jsonl")
	metaPath := sessionPath + ".meta"
	require.NoError(t, os.MkdirAll(filepath.Dir(sessionPath), 0o755))
	require.NoError(t, os.WriteFile(sessionPath, []byte(
		"{\"role\":\"user\",\"content\":\"hi\"}\n{\"role\":\"assistant\",\"content\":\"hello\"}\n",
	), 0o644))

	initialRoot := filepath.Join("workspace", "my-app")
	initialMeta, err := json.Marshal(map[string]string{
		"created_at":     "2026-06-12T10:42:35.2672024Z",
		"updated_at":     "2026-06-12T10:58:03.6456434Z",
		"topic_title":    "Initial title",
		"workspace_root": initialRoot,
		"model":          "claude-opus-4",
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(metaPath, initialMeta, 0o644))

	engine.SyncPaths([]string{sessionPath})

	got, err := db.GetSessionFull(context.Background(), "reasonix:session-123")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, got.DisplayName)
	require.NotNil(t, got.SessionName)
	assert.Equal(t, "Initial title", *got.DisplayName)
	assert.Equal(t, "Initial title", *got.SessionName)
	assert.Equal(t, initialRoot, got.Cwd)
	assert.Equal(t, "my_app", got.Project)

	updatedRoot := filepath.Join("workspace", "renamed-app")
	updatedMeta, err := json.Marshal(map[string]string{
		"created_at":     "2026-06-12T10:42:35.2672024Z",
		"updated_at":     "2026-06-12T10:58:03.6456434Z",
		"topic_title":    "Updated title",
		"workspace_root": updatedRoot,
		"model":          "claude-opus-4",
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(metaPath, updatedMeta, 0o644))
	future := time.Date(2026, time.June, 19, 2, 55, 0, 0, time.UTC)
	require.NoError(t, os.Chtimes(metaPath, future, future))

	engine.SyncPaths([]string{metaPath})

	got, err = db.GetSessionFull(context.Background(), "reasonix:session-123")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, got.DisplayName)
	require.NotNil(t, got.SessionName)
	assert.Equal(t, "Updated title", *got.DisplayName)
	assert.Equal(t, "Updated title", *got.SessionName)
	assert.Equal(t, updatedRoot, got.Cwd)
	assert.Equal(t, "renamed_app", got.Project)
}

func TestEngine_SyncPathsReasonixDeletedMetadataClearsSessionFields(t *testing.T) {
	db := openTestDB(t)
	reasonixDir := t.TempDir()
	engine := NewEngine(db, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentReasonix: {reasonixDir},
		},
		Machine: "local",
	})

	sessionPath := filepath.Join(reasonixDir, "sessions", "session-123.jsonl")
	metaPath := sessionPath + ".meta"
	require.NoError(t, os.MkdirAll(filepath.Dir(sessionPath), 0o755))
	require.NoError(t, os.WriteFile(sessionPath, []byte(
		"{\"role\":\"user\",\"content\":\"hi\"}\n"+
			"{\"role\":\"assistant\",\"content\":\"hello\"}\n",
	), 0o644))

	initialRoot := filepath.Join("workspace", "my-app")
	initialMeta, err := json.Marshal(map[string]string{
		"created_at":     "2026-06-12T10:42:35.2672024Z",
		"updated_at":     "2026-06-12T10:58:03.6456434Z",
		"topic_title":    "Initial title",
		"workspace_root": initialRoot,
		"model":          "claude-opus-4",
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(metaPath, initialMeta, 0o644))

	engine.SyncPaths([]string{sessionPath})

	require.NoError(t, os.Remove(metaPath))
	engine.SyncPaths([]string{metaPath})

	got, err := db.GetSessionFull(context.Background(), "reasonix:session-123")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Nil(t, got.DisplayName)
	assert.Nil(t, got.SessionName)
	assert.Equal(t, "", got.Cwd)
	assert.Equal(t, "", got.Project)
}

func TestEngine_SyncSingleSessionReasonixDeletedMetadataClearsProject(t *testing.T) {
	db := openTestDB(t)
	reasonixDir := t.TempDir()
	engine := NewEngine(db, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentReasonix: {reasonixDir},
		},
		Machine: "local",
	})

	sessionPath := filepath.Join(reasonixDir, "sessions", "session-123.jsonl")
	metaPath := sessionPath + ".meta"
	require.NoError(t, os.MkdirAll(filepath.Dir(sessionPath), 0o755))
	require.NoError(t, os.WriteFile(sessionPath, []byte(
		"{\"role\":\"user\",\"content\":\"hi\"}\n"+
			"{\"role\":\"assistant\",\"content\":\"hello\"}\n",
	), 0o644))

	initialRoot := filepath.Join("workspace", "my-app")
	initialMeta, err := json.Marshal(map[string]string{
		"created_at":     "2026-06-12T10:42:35.2672024Z",
		"updated_at":     "2026-06-12T10:58:03.6456434Z",
		"topic_title":    "Initial title",
		"workspace_root": initialRoot,
		"model":          "claude-opus-4",
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(metaPath, initialMeta, 0o644))

	engine.SyncPaths([]string{sessionPath})

	got, err := db.GetSessionFull(context.Background(), "reasonix:session-123")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "my_app", got.Project)

	require.NoError(t, os.Remove(metaPath))
	require.NoError(t, db.Update(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			"UPDATE sessions SET file_mtime = NULL WHERE id = ?",
			"reasonix:session-123",
		)
		return err
	}))

	require.NoError(t, engine.SyncSingleSession("reasonix:session-123"))

	got, err = db.GetSessionFull(context.Background(), "reasonix:session-123")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "", got.Project)
}

func TestEngine_SyncPathsReasonixMalformedMetadataPreservesSessionFields(t *testing.T) {
	db := openTestDB(t)
	reasonixDir := t.TempDir()
	engine := NewEngine(db, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentReasonix: {reasonixDir},
		},
		Machine: "local",
	})

	sessionPath := filepath.Join(reasonixDir, "sessions", "session-123.jsonl")
	metaPath := sessionPath + ".meta"
	require.NoError(t, os.MkdirAll(filepath.Dir(sessionPath), 0o755))
	require.NoError(t, os.WriteFile(sessionPath, []byte(
		"{\"role\":\"user\",\"content\":\"hi\"}\n"+
			"{\"role\":\"assistant\",\"content\":\"hello\"}\n",
	), 0o644))

	initialRoot := filepath.Join("workspace", "my-app")
	initialMeta, err := json.Marshal(map[string]string{
		"created_at":     "2026-06-12T10:42:35.2672024Z",
		"updated_at":     "2026-06-12T10:58:03.6456434Z",
		"topic_title":    "Initial title",
		"workspace_root": initialRoot,
		"model":          "claude-opus-4",
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(metaPath, initialMeta, 0o644))

	engine.SyncPaths([]string{sessionPath})

	require.NoError(t, os.WriteFile(metaPath, []byte(`{"topic_title":`), 0o644))
	future := time.Date(2026, time.June, 19, 4, 15, 0, 0, time.UTC)
	require.NoError(t, os.Chtimes(metaPath, future, future))

	engine.SyncPaths([]string{metaPath})

	got, err := db.GetSessionFull(context.Background(), "reasonix:session-123")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, got.DisplayName)
	require.NotNil(t, got.SessionName)
	assert.Equal(t, "Initial title", *got.DisplayName)
	assert.Equal(t, "Initial title", *got.SessionName)
	assert.Equal(t, initialRoot, got.Cwd)
	assert.Equal(t, "my_app", got.Project)
}

func TestEngine_SyncPathsReasonixMalformedMetadataRecoveryUpdatesSession(t *testing.T) {
	db := openTestDB(t)
	reasonixDir := t.TempDir()
	engine := NewEngine(db, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentReasonix: {reasonixDir},
		},
		Machine: "local",
	})

	sessionPath := filepath.Join(reasonixDir, "sessions", "session-123.jsonl")
	metaPath := sessionPath + ".meta"
	require.NoError(t, os.MkdirAll(filepath.Dir(sessionPath), 0o755))
	require.NoError(t, os.WriteFile(sessionPath, []byte(
		"{\"role\":\"user\",\"content\":\"hi\"}\n"+
			"{\"role\":\"assistant\",\"content\":\"hello\"}\n",
	), 0o644))

	initialRoot := filepath.Join("workspace", "my-app")
	initialMeta, err := json.Marshal(map[string]string{
		"created_at":     "2026-06-12T10:42:35.2672024Z",
		"updated_at":     "2026-06-12T10:58:03.6456434Z",
		"topic_title":    "Initial title",
		"workspace_root": initialRoot,
		"model":          "claude-opus-4",
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(metaPath, initialMeta, 0o644))

	engine.SyncPaths([]string{sessionPath})

	transcriptInfo, err := os.Stat(sessionPath)
	require.NoError(t, err)
	badMtime := transcriptInfo.ModTime().Add(time.Minute)
	require.NoError(t, os.WriteFile(metaPath, []byte(`{"topic_title":`), 0o644))
	require.NoError(t, os.Chtimes(metaPath, badMtime, badMtime))
	engine.SyncPaths([]string{metaPath})

	updatedRoot := filepath.Join("workspace", "renamed-app")
	updatedMeta, err := json.Marshal(map[string]string{
		"created_at":     "2026-06-12T10:42:35.2672024Z",
		"updated_at":     "2026-06-12T10:58:03.6456434Z",
		"topic_title":    "Recovered title",
		"workspace_root": updatedRoot,
		"model":          "claude-opus-4",
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(metaPath, updatedMeta, 0o644))
	recoveredMtime := badMtime.Add(time.Minute)
	require.NoError(t, os.Chtimes(metaPath, recoveredMtime, recoveredMtime))

	engine.SyncPaths([]string{metaPath})

	got, err := db.GetSessionFull(context.Background(), "reasonix:session-123")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, got.DisplayName)
	require.NotNil(t, got.SessionName)
	assert.Equal(t, "Recovered title", *got.DisplayName)
	assert.Equal(t, "Recovered title", *got.SessionName)
	assert.Equal(t, updatedRoot, got.Cwd)
	assert.Equal(t, "renamed_app", got.Project)
}

func TestEngine_SyncPathsReasonixProjectLayoutMetadataProjectUpdate(t *testing.T) {
	db := openTestDB(t)
	reasonixDir := t.TempDir()
	engine := NewEngine(db, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentReasonix: {reasonixDir},
		},
		Machine: "local",
	})

	sessionPath := filepath.Join(
		reasonixDir, "projects", "layout-name", "sessions", "session-123", "session-123.jsonl",
	)
	metaPath := sessionPath + ".meta"
	require.NoError(t, os.MkdirAll(filepath.Dir(sessionPath), 0o755))
	require.NoError(t, os.WriteFile(sessionPath, []byte(
		"{\"role\":\"user\",\"content\":\"hi\"}\n{\"role\":\"assistant\",\"content\":\"hello\"}\n",
	), 0o644))

	initialMeta, err := json.Marshal(map[string]string{
		"created_at":     "2026-06-12T10:42:35.2672024Z",
		"updated_at":     "2026-06-12T10:58:03.6456434Z",
		"topic_title":    "Initial title",
		"workspace_root": filepath.Join("workspace", "my-app"),
		"model":          "claude-opus-4",
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(metaPath, initialMeta, 0o644))

	engine.SyncPaths([]string{sessionPath})

	got, err := db.GetSessionFull(context.Background(), "reasonix:session-123")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "my_app", got.Project)

	updatedMeta, err := json.Marshal(map[string]string{
		"created_at":     "2026-06-12T10:42:35.2672024Z",
		"updated_at":     "2026-06-12T10:58:03.6456434Z",
		"topic_title":    "Updated title",
		"workspace_root": filepath.Join("workspace", "renamed-app"),
		"model":          "claude-opus-4",
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(metaPath, updatedMeta, 0o644))
	future := time.Date(2026, time.June, 19, 3, 30, 0, 0, time.UTC)
	require.NoError(t, os.Chtimes(metaPath, future, future))

	engine.SyncPaths([]string{metaPath})

	got, err = db.GetSessionFull(context.Background(), "reasonix:session-123")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "renamed_app", got.Project)
}

func TestEngine_SyncSingleSessionReasonixProjectLayoutPreservesProject(t *testing.T) {
	db := openTestDB(t)
	reasonixDir := t.TempDir()
	engine := NewEngine(db, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentReasonix: {reasonixDir},
		},
		Machine: "local",
	})

	sessionPath := filepath.Join(
		reasonixDir, "projects", "layout-name", "sessions",
		"session-123", "session-123.jsonl",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(sessionPath), 0o755))
	require.NoError(t, os.WriteFile(sessionPath, []byte(
		"{\"role\":\"user\",\"content\":\"hi\"}\n"+
			"{\"role\":\"assistant\",\"content\":\"hello\"}\n",
	), 0o644))

	engine.SyncPaths([]string{sessionPath})

	got, err := db.GetSessionFull(context.Background(), "reasonix:session-123")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "layout-name", got.Project)

	require.NoError(t, db.Update(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			"UPDATE sessions SET file_mtime = NULL WHERE id = ?",
			"reasonix:session-123",
		)
		return err
	}))

	require.NoError(t, engine.SyncSingleSession("reasonix:session-123"))

	got, err = db.GetSessionFull(context.Background(), "reasonix:session-123")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "layout-name", got.Project)
}

func TestEngine_SyncPathsReasonixPersistsToolResultContent(t *testing.T) {
	database := openTestDB(t)
	reasonixDir := t.TempDir()
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentReasonix: {reasonixDir},
		},
		Machine: "local",
	})

	sessionPath := filepath.Join(reasonixDir, "sessions", "tool-result.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(sessionPath), 0o755))
	require.NoError(t, os.WriteFile(sessionPath, []byte(
		"{\"role\":\"user\",\"content\":\"Read the file\"}\n"+
			"{\"role\":\"assistant\",\"content\":\"I'll read it\","+
			"\"tool_calls\":[{\"id\":\"call_1\",\"name\":\"read_file\","+
			"\"arguments\":\"{\\\"path\\\":\\\"config.json\\\"}\"}]}\n"+
			"{\"role\":\"tool\",\"content\":\"file contents here\","+
			"\"tool_call_id\":\"call_1\"}\n",
	), 0o644))

	engine.SyncPaths([]string{sessionPath})

	msgs, err := database.GetAllMessages(context.Background(), "reasonix:tool-result")
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "file contents here", msgs[1].ToolCalls[0].ResultContent)
	assert.Equal(t, len("file contents here"), msgs[1].ToolCalls[0].ResultContentLength)
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

// TestDiscoveredFileMtimeVisualStudioCopilotResolvesVirtualPath verifies that
// the mtime helper resolves a <traceFile>#<conversationID> virtual path to its
// physical trace before stat. Without resolution os.Stat fails on the virtual
// path, so SyncAllSince's mtime filter cannot drop unchanged Visual Studio
// conversations and re-syncs every one of them on each poll.
func TestDiscoveredFileMtimeVisualStudioCopilotResolvesVirtualPath(t *testing.T) {
	dir := t.TempDir()
	tracePath := filepath.Join(
		dir, "20260612T194439_257709a3_VSGitHubCopilot_traces.jsonl",
	)
	require.NoError(t, os.WriteFile(tracePath, []byte("{}\n"), 0o644))
	info, err := os.Stat(tracePath)
	require.NoError(t, err)

	virtual := parser.VisualStudioCopilotVirtualPath(
		tracePath, "4a8f63f6-7626-4416-a874-fc7bd2c3f005",
	)
	mtime, err := discoveredFileMtime(parser.DiscoveredFile{
		Path:  virtual,
		Agent: parser.AgentVSCopilot,
	})
	require.NoError(t, err,
		"virtual path must resolve to the physical trace for stat")
	assert.Equal(t, info.ModTime().UnixNano(), mtime)
}

// TestWriteIncrementalBlanksImplausibleEndedAt verifies that the
// incremental sync path runs the appended ended_at through the same
// timestamp plausibility check the full path applies in sanitizeSession.
// An out-of-window ended_at must not persist via incremental sync while a
// full sync of the same file would blank it (an incremental-vs-full
// parity divergence).
func TestWriteIncrementalBlanksImplausibleEndedAt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		endedAt time.Time
	}{
		{name: "far past", endedAt: time.Date(1850, 1, 1, 0, 0, 0, 0, time.UTC)},
		{name: "far future", endedAt: time.Date(2999, 1, 1, 0, 0, 0, 0, time.UTC)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := openTestDB(t)
			e := &Engine{db: database}

			plausibleEnd := time.Date(2026, 6, 20, 10, 0, 0, 0, time.UTC)
			start := plausibleEnd.Add(-time.Hour)
			pw := pendingWrite{
				sess: parser.ParsedSession{
					ID:           "inc-ts",
					Project:      "proj",
					Machine:      "host",
					Agent:        parser.AgentClaude,
					StartedAt:    start,
					EndedAt:      plausibleEnd,
					MessageCount: 1,
				},
				msgs: []parser.ParsedMessage{{
					Role:      parser.RoleUser,
					Content:   "hello",
					Timestamp: start,
				}},
			}
			_, _, failed := e.writeBatch(
				[]pendingWrite{pw}, syncWriteDefault, false,
			)
			require.Equal(t, 0, failed, "initial session write must not fail")

			before, err := database.GetSessionFull(context.Background(), "inc-ts")
			require.NoError(t, err)
			require.NotNil(t, before)
			require.NotNil(t, before.EndedAt, "baseline ended_at must be set")
			wantEnd := *before.EndedAt

			err = e.writeIncremental(&incrementalUpdate{
				sessionID: "inc-ts",
				msgs: []parser.ParsedMessage{{
					Role:      parser.RoleAssistant,
					Content:   "world",
					Timestamp: plausibleEnd,
					Ordinal:   1,
				}},
				endedAt:      tc.endedAt,
				msgCount:     2,
				userMsgCount: 1,
				fileSize:     100,
				fileMtime:    plausibleEnd.UnixNano(),
			})
			require.NoError(t, err, "writeIncremental")

			after, err := database.GetSessionFull(context.Background(), "inc-ts")
			require.NoError(t, err)
			require.NotNil(t, after)
			require.NotNil(t, after.EndedAt,
				"implausible ended_at must be blanked, leaving the prior value via COALESCE")
			// The implausible appended timestamp must not have been
			// stored. Because it is blanked to nil, COALESCE keeps the
			// prior plausible value.
			assert.Equal(t, wantEnd, *after.EndedAt,
				"implausible ended_at must not overwrite the plausible value")
			assert.NotContains(t, *after.EndedAt, "1850",
				"far-past ended_at must not persist")
			assert.NotContains(t, *after.EndedAt, "2999",
				"far-future ended_at must not persist")
		})
	}
}

// TestWriteIncrementalKeepsPlausibleEndedAt is the positive control for
// TestWriteIncrementalBlanksImplausibleEndedAt: a plausible appended
// ended_at must still update the column.
func TestWriteIncrementalKeepsPlausibleEndedAt(t *testing.T) {
	database := openTestDB(t)
	e := &Engine{db: database}

	start := time.Date(2026, 6, 20, 10, 0, 0, 0, time.UTC)
	firstEnd := start.Add(time.Hour)
	pw := pendingWrite{
		sess: parser.ParsedSession{
			ID:           "inc-ts-ok",
			Project:      "proj",
			Machine:      "host",
			Agent:        parser.AgentClaude,
			StartedAt:    start,
			EndedAt:      firstEnd,
			MessageCount: 1,
		},
		msgs: []parser.ParsedMessage{{
			Role:      parser.RoleUser,
			Content:   "hello",
			Timestamp: start,
		}},
	}
	_, _, failed := e.writeBatch(
		[]pendingWrite{pw}, syncWriteDefault, false,
	)
	require.Equal(t, 0, failed, "initial session write must not fail")

	newEnd := start.Add(2 * time.Hour)
	err := e.writeIncremental(&incrementalUpdate{
		sessionID: "inc-ts-ok",
		msgs: []parser.ParsedMessage{{
			Role:      parser.RoleAssistant,
			Content:   "world",
			Timestamp: newEnd,
			Ordinal:   1,
		}},
		endedAt:      newEnd,
		msgCount:     2,
		userMsgCount: 1,
		fileSize:     100,
		fileMtime:    newEnd.UnixNano(),
	})
	require.NoError(t, err, "writeIncremental")

	after, err := database.GetSessionFull(context.Background(), "inc-ts-ok")
	require.NoError(t, err)
	require.NotNil(t, after)
	require.NotNil(t, after.EndedAt)
	gotEnd, ok := parseStoredTimestamp(*after.EndedAt)
	require.True(t, ok, "stored ended_at must parse")
	assert.True(t, gotEnd.Equal(newEnd),
		"plausible appended ended_at must update the column: got %q want %s",
		*after.EndedAt, newEnd.Format(time.RFC3339Nano))
}

func TestConvertToolCallsFilePathAndCallIndex(t *testing.T) {
	parsed := []parser.ParsedToolCall{
		{ToolName: "Edit", Category: "Edit", ToolUseID: "a",
			InputJSON: `{"file_path":"/x.go"}`}, // resolved from JSON
		{ToolName: "Write", Category: "Write", ToolUseID: "b",
			InputJSON: "raw diff not json", FilePath: "/native.go"}, // native wins
		{ToolName: "Bash", Category: "Bash", ToolUseID: "c",
			InputJSON: `{"command":"ls"}`}, // no path
	}
	got := convertToolCalls("sess-1", parsed)
	require.Len(t, got, 3)
	assert.Equal(t, "/x.go", got[0].FilePath)
	assert.Equal(t, 0, got[0].CallIndex)
	assert.Equal(t, "/native.go", got[1].FilePath)
	assert.Equal(t, 1, got[1].CallIndex)
	assert.Equal(t, "", got[2].FilePath)
	assert.Equal(t, 2, got[2].CallIndex)
}

// codexRenameFixture is a seeded Codex session whose stored file_mtime is the
// folded index-mtime watermark, used to exercise title-rename detection in
// shouldSkipCodex.
type codexRenameFixture struct {
	e              *Engine
	path           string
	info           os.FileInfo
	effectiveMtime int64
	root           string
	uuid           string
}

// writeCodexIndexForTest writes the session_index.jsonl mapping uuid -> title
// at indexMtime, the file shouldSkipCodex's title check reads.
func writeCodexIndexForTest(
	t *testing.T, root, uuid, title string, indexMtime time.Time,
) string {
	t.Helper()
	idxPath := filepath.Join(root, parser.CodexSessionIndexFilename)
	line := `{"id":"` + uuid + `","thread_name":"` + title + `"}` + "\n"
	require.NoError(t, os.WriteFile(idxPath, []byte(line), 0o600))
	require.NoError(t, os.Chtimes(idxPath, indexMtime, indexMtime))
	return idxPath
}

// seedCodexRenameCase stores a Codex session whose file_mtime watermark is the
// folded index mtime (the index is newer than the transcript). That is the
// exact shape where a later title-only rename whose index mtime lands at or
// below the watermark is invisible to an mtime comparison.
func seedCodexRenameCase(t *testing.T, database *db.DB) codexRenameFixture {
	t.Helper()
	root := t.TempDir()
	const uuid = "11111111-2222-3333-4444-555555555555"
	sessDir := filepath.Join(root, "sessions", "2026", "06", "21")
	require.NoError(t, os.MkdirAll(sessDir, 0o755))
	path := filepath.Join(sessDir, "rollout-2026-06-21T18-59-38-"+uuid+".jsonl")

	content := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/home/user/code/api", "user", "2026-06-21T18:59:38Z",
		),
		testjsonl.CodexMsgJSON("user", "review this", "2026-06-21T18:59:39Z"),
	)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	transcriptMtime := time.Unix(1_700_000_000, 0)
	require.NoError(t, os.Chtimes(path, transcriptMtime, transcriptMtime))
	origIndexMtime := transcriptMtime.Add(time.Hour)
	writeCodexIndexForTest(t, root, uuid, "Original Title", origIndexMtime)

	info, err := os.Stat(path)
	require.NoError(t, err, "stat codex fixture")
	effectiveMtime := parser.CodexEffectiveMtime(path, info.ModTime().UnixNano())
	require.Equal(t, origIndexMtime.UnixNano(), effectiveMtime,
		"folded watermark should be the index mtime")

	sess := db.Session{
		ID:          "host~codex:" + uuid,
		Project:     "api",
		Machine:     "host",
		Agent:       "codex",
		SessionName: strPtr("Original Title"),
		FilePath:    strPtr("host:" + path),
		FileSize:    int64Ptr(info.Size()),
		FileMtime:   int64Ptr(effectiveMtime),
	}
	require.NoError(t, database.UpsertSession(sess))
	require.NoError(t, database.SetSessionDataVersion(
		sess.ID, db.CurrentDataVersion(),
	))

	e := &Engine{
		db:       database,
		idPrefix: "host~",
		pathRewriter: func(p string) string {
			return "host:" + p
		},
	}
	return codexRenameFixture{
		e: e, path: path, info: info,
		effectiveMtime: effectiveMtime, root: root, uuid: uuid,
	}
}

// TestShouldSkipCodexTitleRenameBelowStoredMtimeDoesNotSkip pins the masking
// fix: a title-only rename whose folded index mtime is at or below the stored
// watermark used to be skipped by shouldSkipCodex's storedMtime==effectiveMtime
// fast path, which never consulted the title. The direct title check must now
// force a reparse while an unchanged session still hits the skip path.
func TestShouldSkipCodexTitleRenameBelowStoredMtimeDoesNotSkip(t *testing.T) {
	database := openTestDB(t)
	f := seedCodexRenameCase(t, database)

	// Control: nothing changed -> hot path still skips.
	assert.True(t, f.e.shouldSkipCodex(f.path, f.info),
		"unchanged transcript and title must still skip")

	// Title-only rename whose index mtime lands at or below the stored
	// watermark. The transcript bytes are untouched, so the old mtime gate
	// would skip; the mtime-independent title check must catch it.
	writeCodexIndexForTest(t, f.root, f.uuid, "Renamed Title",
		time.Unix(0, f.effectiveMtime))
	renamedEff := parser.CodexEffectiveMtime(f.path, f.info.ModTime().UnixNano())
	require.LessOrEqual(t, renamedEff, f.effectiveMtime,
		"renamed index mtime must be at or below the stored watermark")
	require.Equal(t, "Renamed Title",
		parser.LookupCodexThreadName(f.path, f.uuid),
		"live index must report the renamed title")

	assert.False(t, f.e.shouldSkipCodex(f.path, f.info),
		"title-only rename at or below stored watermark must not skip")
}
