package server

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/wesm/agentsview/internal/db"
)

// testSession returns a *db.Session with sensible defaults.
// Override fields after calling or via functional options.
func testSession(
	opts ...func(*db.Session),
) *db.Session {
	s := &db.Session{
		ID:           "test-id",
		Project:      "proj",
		Agent:        "claude",
		MessageCount: 0,
		StartedAt:    new("2025-01-15T10:00:00Z"),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// stubServer returns an httptest.Server that responds with
// the given status code and body. Caller must defer ts.Close().
func stubServer(
	t *testing.T, expectedMethod string, expectedToken string, status int, body string,
) *httptest.Server {
	t.Helper()
	return httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				if r.Method != expectedMethod {
					t.Errorf("expected method %q, got %q", expectedMethod, r.Method)
				}
				if r.Header.Get("User-Agent") != "agentsview" {
					t.Errorf("expected User-Agent %q, got %q", "agentsview", r.Header.Get("User-Agent"))
				}
				expectedAuth := "token " + expectedToken
				if auth := r.Header.Get("Authorization"); auth != expectedAuth {
					t.Errorf("expected Authorization header %q, got %q", expectedAuth, auth)
				}
				w.WriteHeader(status)
				if body != "" {
					w.Write([]byte(body))
				}
			},
		),
	)
}

// assertErrorContains checks that err is non-nil and contains want.
func assertErrorContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("expected error containing %q, got: %v", want, err)
	}
}

// assertContextCancelled checks that err is non-nil and
// wraps context.Canceled.
func assertContextCancelled(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
	if !errors.Is(err, context.Canceled) &&
		!strings.Contains(
			err.Error(), "context canceled",
		) {
		t.Errorf(
			"expected context.Canceled, got: %v", err,
		)
	}
}

func TestFormatTimestamp(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			"RFC3339",
			"2025-01-15T10:30:00Z",
			"2025-01-15 10:30:00",
		},
		{
			"RFC3339Nano",
			"2025-06-01T08:15:30.123456789Z",
			"2025-06-01 08:15:30",
		},
		{
			"RFC3339_WithOffset",
			"2025-03-20T14:00:00+05:00",
			"2025-03-20 14:00:00",
		},
		{
			"Empty",
			"",
			"",
		},
		{
			"Unparseable_ReturnsRaw",
			"not-a-timestamp",
			"not-a-timestamp",
		},
		{
			"Midnight",
			"2025-12-31T00:00:00Z",
			"2025-12-31 00:00:00",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := formatTimestamp(tt.in)
			if got != tt.want {
				t.Errorf(
					"formatTimestamp(%q) = %q, want %q",
					tt.in, got, tt.want,
				)
			}
		})
	}
}

func TestFormatDateShort(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   *string
		want string
	}{
		{"Nil", nil, "unknown"},
		{"Empty", new(""), "unknown"},
		{
			"Valid",
			new("2025-01-15T10:30:00Z"),
			"20250115",
		},
		{
			"Nano",
			new("2025-06-01T08:15:30.999Z"),
			"20250601",
		},
		{
			"Unparseable",
			new("garbage"),
			"unknown",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := formatDateShort(tt.in)
			if got != tt.want {
				t.Errorf(
					"formatDateShort(%v) = %q, want %q",
					tt.in, got, tt.want,
				)
			}
		})
	}
}

func TestParseTimestamp(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		in    string
		valid bool
	}{
		{"RFC3339", "2025-01-15T10:30:00Z", true},
		{"RFC3339Nano", "2025-01-15T10:30:00.123Z", true},
		{"WithOffset", "2025-01-15T10:30:00+02:00", true},
		{"Invalid", "January 15th", false},
		{"Empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, ok := parseTimestamp(tt.in)
			if ok != tt.valid {
				t.Errorf(
					"parseTimestamp(%q) ok=%v, want %v",
					tt.in, ok, tt.valid,
				)
			}
		})
	}
}

func TestFormatContentForExport_Escaping(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		input    string
		contains []string
		excludes []string
	}{
		{
			"HTMLEntitiesEscaped",
			`<script>alert("xss")</script>`,
			[]string{
				"&lt;script&gt;",
				"&lt;/script&gt;",
			},
			[]string{"<script>"},
		},
		{
			"AmpersandEscaped",
			"foo & bar < baz",
			[]string{"foo &amp; bar &lt; baz"},
			[]string{"foo & bar"},
		},
		{
			"CodeBlock",
			"```go\nfmt.Println(\"hello\")\n```",
			[]string{
				"<pre><code>",
				"</code></pre>",
			},
			nil,
		},
		{
			"InlineCode",
			"use `fmt.Println` here",
			[]string{"<code>fmt.Println</code>"},
			nil,
		},
		{
			"ThinkingBlock",
			"[Thinking]\nI need to consider this",
			[]string{
				`class="thinking-block"`,
				`class="thinking-label"`,
			},
			nil,
		},
		{
			"ToolBlock",
			"[Read file.go]\ncontent here",
			[]string{`class="tool-block"`},
			nil,
		},
		{
			"SkillBlock",
			"[Skill: planner]\nuse the plan\n[/Skill]",
			[]string{"[Skill: planner]"},
			[]string{`class="tool-block"`},
		},
		{
			"BashToolBlock",
			"[Bash ls -la]\noutput",
			[]string{`class="tool-block"`},
			nil,
		},
		{
			"TaskCreateToolBlock",
			"[TaskCreate: worker]\nrun task",
			[]string{`class="tool-block"`},
			nil,
		},
		{
			"LegacyCodexExecCommand",
			"[exec_command]\n$ ls -la",
			[]string{`class="tool-block"`},
			nil,
		},
		{
			"LegacyCodexParallel",
			"[parallel]\nrunning tasks",
			[]string{`class="tool-block"`},
			nil,
		},
		{
			"LegacyCodexViewImage",
			"[view_image]\nimage.png",
			[]string{`class="tool-block"`},
			nil,
		},
		{
			"LegacyCodexUpdatePlan",
			"[update_plan]\nnew plan",
			[]string{`class="tool-block"`},
			nil,
		},
		{
			"EmptyInput",
			"",
			[]string{""},
			nil,
		},
		{
			"NestedHTMLInCode",
			"```\n<div>not rendered</div>\n```",
			[]string{
				"&lt;div&gt;not rendered&lt;/div&gt;",
			},
			[]string{"<div>"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := formatContentForExport(tt.input)
			assertContainsAll(t, got, tt.contains)
			assertContainsNone(t, got, tt.excludes)
		})
	}
}

func TestIsThinkingOnly(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{
			"PureThinking",
			"[Thinking]\nDeep thoughts here",
			true,
		},
		{
			"ThinkingThenToolBlock",
			"[Thinking]\nthoughts\n[Read file.go]\ncontent",
			false,
		},
		{
			"NoThinking",
			"Just regular text",
			false,
		},
		{
			"Empty",
			"",
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := isThinkingOnly(tt.in)
			if got != tt.want {
				t.Errorf(
					"isThinkingOnly(%q) = %v, want %v",
					tt.in, got, tt.want,
				)
			}
		})
	}
}

func TestGenerateExportHTML_Structure(t *testing.T) {
	t.Parallel()
	session := testSession(func(s *db.Session) {
		s.Project = "my-project"
		s.MessageCount = 2
		s.FirstMessage = new("Hello")
	})
	msgs := []db.Message{
		{
			SessionID: "test-id", Ordinal: 0,
			Role: "user", Content: "Hello agent",
			Timestamp: "2025-01-15T10:00:00Z",
		},
		{
			SessionID: "test-id", Ordinal: 1,
			Role:      "assistant",
			Content:   "Hi! How can I help?",
			Timestamp: "2025-01-15T10:00:05Z",
		},
	}

	html := generateExportHTML(session, msgs)

	assertContainsAll(t, html, []string{
		"<!DOCTYPE html>",
		"my-project",
		"Claude",
		"2 messages",
		`class="message user"`,
		`class="message assistant"`,
		"Hello agent",
		"Hi! How can I help?",
		"2025-01-15 10:00:00",
		"2025-01-15 10:00:05",
	})
}

func TestGenerateExportHTML_ThinkingOnlyClass(t *testing.T) {
	t.Parallel()
	session := testSession(func(s *db.Session) {
		s.MessageCount = 1
	})
	msgs := []db.Message{
		{
			SessionID: "test-id", Ordinal: 0,
			Role:      "assistant",
			Content:   "[Thinking]\nJust internal thoughts",
			Timestamp: "2025-01-15T10:00:00Z",
		},
	}

	html := generateExportHTML(session, msgs)
	if !strings.Contains(html, "thinking-only") {
		t.Error("expected thinking-only class for" +
			" thinking-only message")
	}
}

func TestGenerateExportHTML_EscapesHostileInput(t *testing.T) {
	t.Parallel()
	session := testSession(func(s *db.Session) {
		s.Project = `<img src=x onerror=alert(1)>`
		s.MessageCount = 1
	})
	msgs := []db.Message{
		{
			SessionID: "test-id", Ordinal: 0,
			Role:      "user",
			Content:   `<script>alert("xss")</script>`,
			Timestamp: "2025-01-15T10:00:00Z",
		},
	}

	out := generateExportHTML(session, msgs)

	// Template auto-escapes the <img> tag in project name
	if strings.Contains(out, "<img src=x") {
		t.Error("project name XSS: raw <img> tag not escaped")
	}
	// Content is escaped by formatContentForExport
	if strings.Contains(out, "<script>alert") {
		t.Error("message content XSS not escaped")
	}
}

func TestGenerateExportHTML_CodexAgent(t *testing.T) {
	t.Parallel()
	session := testSession(func(s *db.Session) {
		s.Agent = "codex"
	})

	html := generateExportHTML(session, nil)
	if !strings.Contains(html, "Codex") {
		t.Error("expected Codex display name for codex agent")
	}
}

func TestGenerateExportHTML_NilStartedAt(t *testing.T) {
	t.Parallel()
	session := testSession(func(s *db.Session) {
		s.StartedAt = nil
	})

	html := generateExportHTML(session, nil)
	if !strings.Contains(html, "<!DOCTYPE html>") {
		t.Error("expected valid HTML even with nil StartedAt")
	}
}

func TestGenerateExportHTML_TranscriptModeControls(t *testing.T) {
	t.Parallel()
	session := testSession()
	msgs := []db.Message{
		{
			SessionID: "test-id", Ordinal: 0,
			Role: "user", Content: "Please inspect",
			Timestamp: "2025-01-15T10:00:00Z",
		},
		{
			SessionID: "test-id", Ordinal: 1,
			Role: "assistant", Content: "I'll check that",
			Timestamp: "2025-01-15T10:00:01Z",
		},
		{
			SessionID: "test-id", Ordinal: 2,
			Role: "assistant", Content: "[Bash]\nls",
			Timestamp:  "2025-01-15T10:00:02Z",
			HasToolUse: true,
		},
		{
			SessionID: "test-id", Ordinal: 3,
			Role: "assistant", Content: "The answer",
			Timestamp: "2025-01-15T10:00:03Z",
		},
	}

	html := generateExportHTML(session, msgs)

	assertContainsAll(t, html, []string{
		`id="transcript-normal" name="transcript-mode" class="toggle-input" checked`,
		`id="transcript-focused" name="transcript-mode" class="toggle-input"`,
		`<label for="transcript-normal" class="toggle-label">Normal</label>`,
		`<label for="transcript-focused" class="toggle-label">Focused</label>`,
		`#transcript-focused:checked ~ main .message.focused-hidden`,
		`class="message assistant focused-hidden" data-ordinal="1"`,
		`class="message assistant focused-hidden" data-ordinal="2"`,
		`class="message assistant" data-ordinal="3"`,
	})
	if strings.Index(
		html,
		`#transcript-focused:checked ~ main .message.focused-hidden`,
	) < strings.Index(
		html,
		`#thinking-toggle:checked ~ main .message.thinking-only`,
	) {
		t.Error("focused hide rule must follow thinking display rule")
	}
	assertContainsNone(t, html, []string{
		`class="message user focused-hidden" data-ordinal="0"`,
		`class="message assistant focused-hidden" data-ordinal="3"`,
	})
}

func TestFocusedExportOrdinals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		msgs []db.Message
		want []int
	}{
		{
			name: "keeps final assistant before next user",
			msgs: []db.Message{
				exportUserMsg(0),
				exportAssistantMsg(1, "working"),
				exportToolMsg(2, "[Read]\nfile"),
				exportAssistantMsg(3, "final"),
				exportUserMsg(4),
			},
			want: []int{0, 3, 4},
		},
		{
			name: "drops terminal tool-only stretch",
			msgs: []db.Message{
				exportUserMsg(0),
				exportToolMsg(1, "[Bash]\nmake test"),
			},
			want: []int{0},
		},
		{
			name: "drops consecutive tool blocks in one message",
			msgs: []db.Message{
				exportUserMsg(0),
				exportToolMsg(1, "[Read]\nold\n[Write]\nnew"),
			},
			want: []int{0},
		},
		{
			name: "keeps terminal final assistant",
			msgs: []db.Message{
				exportUserMsg(0),
				exportToolMsg(1, "[Bash]\nmake test"),
				exportAssistantMsg(2, "done"),
			},
			want: []int{0, 2},
		},
		{
			name: "keeps only last assistant in consecutive assistant run",
			msgs: []db.Message{
				exportUserMsg(0),
				exportAssistantMsg(1, "first"),
				exportAssistantMsg(2, "second"),
				exportUserMsg(3),
			},
			want: []int{0, 2, 3},
		},
		{
			name: "ignores thinking-only tail after final assistant",
			msgs: []db.Message{
				exportUserMsg(0),
				exportAssistantMsg(1, "answer"),
				exportAssistantMsg(2, "[Thinking]\nfollow-up notes"),
			},
			want: []int{0, 1},
		},
		{
			name: "ignores system messages",
			msgs: []db.Message{
				exportUserMsg(0),
				exportAssistantMsg(1, "answer"),
				{
					SessionID: "test-id",
					Ordinal:   2,
					Role:      "assistant",
					Content:   "system progress",
					IsSystem:  true,
				},
				{
					SessionID: "test-id",
					Ordinal:   3,
					Role:      "user",
					Content:   "system user event",
					IsSystem:  true,
				},
				exportUserMsg(4),
			},
			want: []int{0, 1, 4},
		},
		{
			name: "keeps answer before compact boundary",
			msgs: []db.Message{
				exportUserMsg(0),
				exportAssistantMsg(1, "answer"),
				{
					SessionID:         "test-id",
					Ordinal:           2,
					Role:              "assistant",
					Content:           "[compact summary]",
					IsSystem:          true,
					IsCompactBoundary: true,
				},
				exportUserMsg(3),
			},
			want: []int{0, 1, 2, 3},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			visible := focusedExportOrdinals(tt.msgs)
			got := make([]int, 0, len(tt.msgs))
			for _, msg := range tt.msgs {
				if visible[msg.Ordinal] {
					got = append(got, msg.Ordinal)
				}
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("visible ordinals = %v, want %v", got, tt.want)
			}
		})
	}
}

func exportUserMsg(ordinal int) db.Message {
	return db.Message{
		SessionID: "test-id",
		Ordinal:   ordinal,
		Role:      "user",
		Content:   "user",
	}
}

func exportAssistantMsg(ordinal int, content string) db.Message {
	return db.Message{
		SessionID: "test-id",
		Ordinal:   ordinal,
		Role:      "assistant",
		Content:   content,
	}
}

func exportToolMsg(ordinal int, content string) db.Message {
	return db.Message{
		SessionID:   "test-id",
		Ordinal:     ordinal,
		Role:        "assistant",
		Content:     content,
		HasToolUse:  true,
		HasThinking: strings.Contains(content, "[Thinking]"),
	}
}

func TestGenerateExportMarkdown_Structure(t *testing.T) {
	t.Parallel()
	session := testSession(func(s *db.Session) {
		s.Project = "my-project"
		s.MessageCount = 2
	})
	msgs := []db.Message{
		{
			SessionID: "test-id",
			Ordinal:   0,
			Role:      "user",
			Content:   "Hello <agent>",
			Timestamp: "2025-01-15T10:00:00Z",
		},
		{
			SessionID:   "test-id",
			Ordinal:     1,
			Role:        "assistant",
			Content:     "[Thinking]\nNeed inspect.\n\n[Task]\nworking",
			Timestamp:   "2025-01-15T10:00:05Z",
			HasThinking: true,
			HasToolUse:  true,
			ToolCalls: []db.ToolCall{{
				ToolName:      "Task",
				Category:      "Task",
				ToolUseID:     "toolu_1",
				InputJSON:     `{"prompt":"inspect repo"}`,
				ResultContent: "done",
			}},
		},
	}

	out := generateExportMarkdown(session, msgs, exportMarkdownOptions{})
	assertContainsAll(t, out, []string{
		"# Session: my-project",
		`<session id="test-id" project="my-project" agent="Claude"`,
		`<message role="user" ordinal="0"`,
		"Hello &lt;agent&gt;",
		`<thinking><![CDATA[` + "\nNeed inspect.\n" + `]]></thinking>`,
		`<tool_call id="toolu_1" name="Task" category="Task">`,
		`<arguments><![CDATA[` + "\n{\"prompt\":\"inspect repo\"}\n" + `]]></arguments>`,
		`<tool_body><![CDATA[` + "\ninspect repo\n" + `]]></tool_body>`,
		`<tool_result><![CDATA[` + "\ndone\n" + `]]></tool_result>`,
	})
}

func TestGenerateExportMarkdown_SerializesCodeSkillAndCDATAFallback(t *testing.T) {
	t.Parallel()
	session := testSession()
	msgs := []db.Message{{
		SessionID: "test-id",
		Ordinal:   0,
		Role:      "assistant",
		Content: "```go\nfmt.Println(\"hi\")\n```\n\n" +
			"[Skill: planner]\nuse ]]> carefully\n[/Skill]",
		Timestamp: "2025-01-15T10:00:00Z",
	}}

	out := generateExportMarkdown(session, msgs, exportMarkdownOptions{})
	assertContainsAll(t, out, []string{
		`<code_block language="go"><![CDATA[` + "\nfmt.Println(\"hi\")\n" + `]]></code_block>`,
		`<skill name="planner">use ]]&gt; carefully</skill>`,
	})
}

func TestGenerateExportMarkdown_OmitsEmptyOptionalAttributes(t *testing.T) {
	t.Parallel()
	session := testSession(func(s *db.Session) {
		s.StartedAt = nil
	})
	childStarted := "2025-01-15T10:05:00Z"
	out := generateExportMarkdownTree(&exportSessionTree{
		Session: session,
		Messages: []db.Message{{
			SessionID:  "test-id",
			Ordinal:    0,
			Role:       "assistant",
			Content:    "[Read file.go]\nbody",
			HasToolUse: true,
			ToolCalls: []db.ToolCall{{
				ToolName:  "Read",
				Category:  "Read",
				ToolUseID: "toolu_1",
			}},
		}},
		AppendedChildren: []*exportSessionTree{{
			Session: &db.Session{
				ID:              "child-1",
				Project:         "proj",
				Agent:           "claude",
				ParentSessionID: new("test-id"),
				StartedAt:       &childStarted,
			},
		}},
	}, exportMarkdownOptions{})
	assertContainsAll(t, out, []string{
		`<tool_call id="toolu_1" name="Read" category="Read">`,
		`<child_session id="child-1" parent_session_id="test-id" project="proj" agent="Claude" started_at="2025-01-15T10:05:00Z">`,
	})
	assertContainsNone(t, out, []string{
		`relationship=""`,
		`started_at=""`,
	})
}

func TestGenerateExportMarkdown_PreservesMultiWordToolNamesAndResultEvents(t *testing.T) {
	t.Parallel()
	session := testSession()
	msgs := []db.Message{{
		SessionID:   "test-id",
		Ordinal:     0,
		Role:        "assistant",
		Content:     "[Todo List]\nplan work",
		HasToolUse:  true,
		Timestamp:   "2025-01-15T10:00:00Z",
		ToolCalls:   nil,
		HasThinking: false,
	}}
	out := generateExportMarkdown(session, msgs, exportMarkdownOptions{})
	assertContainsAll(t, out, []string{
		`<tool_call name="Todo List" category="Todo List">`,
		`<tool_body><![CDATA[` + "\nplan work\n" + `]]></tool_body>`,
	})

	msgs[0].Content = "[Bash]\n$ echo hi"
	msgs[0].ToolCalls = []db.ToolCall{{
		ToolName:  "Bash",
		Category:  "Bash",
		ToolUseID: "toolu_bash",
		ResultEvents: []db.ToolResultEvent{{
			ToolUseID: "toolu_bash",
			Source:    "subagent_notification",
			Status:    "running",
			Content:   "still working",
		}},
	}}
	out = generateExportMarkdown(session, msgs, exportMarkdownOptions{})
	assertContainsAll(t, out, []string{
		`<tool_result tool_call_id="toolu_bash" source="subagent_notification" status="running"><![CDATA[` + "\nstill working\n" + `]]></tool_result>`,
	})
}

func TestGenerateExportMarkdown_EmitsEmptyMessages(t *testing.T) {
	t.Parallel()
	session := testSession()
	msgs := []db.Message{{
		SessionID: "test-id",
		Ordinal:   0,
		Role:      "assistant",
		Content:   "",
	}}
	out := generateExportMarkdown(session, msgs, exportMarkdownOptions{})
	assertContainsAll(t, out, []string{
		`<message role="assistant" ordinal="0"></message>`,
	})
}

func TestGenerateExportMarkdown_SanitizesHeadingAndAvoidsDuplicateAnchors(t *testing.T) {
	t.Parallel()
	session := testSession(func(s *db.Session) {
		s.Project = "proj\n<script>alert(1)</script>"
	})
	child := &exportSessionTree{
		Session: &db.Session{
			ID:               "child-a",
			Project:          "proj",
			Agent:            "claude",
			ParentSessionID:  new("test-id"),
			RelationshipType: "subagent",
		},
		Messages: []db.Message{{
			SessionID: "child-a",
			Ordinal:   0,
			Role:      "assistant",
			Content:   "child message",
		}},
	}
	msgs := []db.Message{{
		SessionID:  "test-id",
		Ordinal:    0,
		Role:       "assistant",
		Content:    "[Task]\none\n\n[Task]\ntwo",
		HasToolUse: true,
		ToolCalls: []db.ToolCall{
			{ToolName: "Task", Category: "Task", ToolUseID: "toolu_1", SubagentSessionID: "child-a"},
			{ToolName: "Task", Category: "Task", ToolUseID: "toolu_2", SubagentSessionID: "child-a"},
		},
	}}
	out := generateExportMarkdownTree(&exportSessionTree{
		Session:          session,
		Messages:         msgs,
		AnchoredChildren: map[string]*exportSessionTree{"child-a": child},
	}, exportMarkdownOptions{Depth: "all"})
	assertContainsNone(t, out, []string{"# Session: proj\n<script>alert(1)</script>"})
	if strings.Count(out, `<subagent_session id="child-a"`) != 1 {
		t.Fatalf("expected child session once, got:\n%s", out)
	}
}

func TestGenerateExportMarkdown_DoesNotParseToolMarkersInsideCodeBlocks(t *testing.T) {
	t.Parallel()
	session := testSession(func(s *db.Session) {
		s.Project = "proj\\[link]\x00"
	})
	msgs := []db.Message{{
		SessionID: "test-id",
		Ordinal:   0,
		Role:      "assistant",
		Content:   "```txt\n[Task]\nnot tool\n```",
	}}
	out := generateExportMarkdown(session, msgs, exportMarkdownOptions{})
	assertContainsAll(t, out, []string{
		`<code_block language="txt"><![CDATA[` + "\n[Task]\nnot tool\n" + `]]></code_block>`,
	})
	assertContainsNone(t, out, []string{`<tool_call`, "\x00", "# Session: proj\\[link]"})
}

func TestGenerateExportMarkdown_StripsXMLInvalidControlChars(t *testing.T) {
	t.Parallel()
	session := testSession()
	msgs := []db.Message{{
		SessionID:  "test-id",
		Ordinal:    0,
		Role:       "assistant",
		Content:    "[Bash]\n$ printf hi",
		HasToolUse: true,
		ToolCalls: []db.ToolCall{{
			ToolName:      "Bash",
			Category:      "Bash",
			ResultContent: "ok\x1b[31mred\x00",
		}},
	}}
	out := generateExportMarkdown(session, msgs, exportMarkdownOptions{})
	assertContainsNone(t, out, []string{"\x1b", "\x00"})
}

func TestGenerateExportMarkdown_SeparatesAdjacentLegacyBlocks(t *testing.T) {
	t.Parallel()
	session := testSession()
	msgs := []db.Message{{
		SessionID:  "test-id",
		Ordinal:    0,
		Role:       "assistant",
		Content:    "[Read file-a.go]\nbody one\n[Grep TODO]\nbody two\n```txt\n[Task]\ncode only\n```",
		HasToolUse: true,
	}}
	out := generateExportMarkdown(session, msgs, exportMarkdownOptions{})
	assertContainsAll(t, out, []string{
		`<tool_call name="Read" category="Read">`,
		`<tool_body><![CDATA[` + "\nbody one\n" + `]]></tool_body>`,
		`<tool_call name="Grep" category="Grep">`,
		`<tool_body><![CDATA[` + "\nbody two\n" + `]]></tool_body>`,
		`<code_block language="txt"><![CDATA[` + "\n[Task]\ncode only\n" + `]]></code_block>`,
	})
}

func TestGenerateExportMarkdown_PreservesFollowingTextAfterMultilineBash(t *testing.T) {
	t.Parallel()
	session := testSession()
	cmd := "for x in a; do\n  echo done\ndone"
	msgs := []db.Message{{
		SessionID:  "test-id",
		Ordinal:    0,
		Role:       "assistant",
		Content:    "[Bash]\n$ " + cmd + "\n\ndone",
		HasToolUse: true,
		ToolCalls: []db.ToolCall{{
			ToolName:  "Bash",
			Category:  "Bash",
			InputJSON: `{"command":"` + "for x in a; do\\n  echo done\\ndone" + `"}`,
		}},
	}}
	out := generateExportMarkdown(session, msgs, exportMarkdownOptions{})
	assertContainsAll(t, out, []string{
		`<tool_body><![CDATA[` + "\n$ " + cmd + "\n" + `]]></tool_body>`,
		"\ndone\n</message>",
	})
}

func TestGenerateExportMarkdown_SeparatesEmptyLegacyBlocks(t *testing.T) {
	t.Parallel()
	session := testSession()
	msgs := []db.Message{{
		SessionID:  "test-id",
		Ordinal:    0,
		Role:       "assistant",
		Content:    "[Task]\n[Grep TODO]\nbody two\n[Thinking]\n```txt\ncode only\n```",
		HasToolUse: true,
	}}
	out := generateExportMarkdown(session, msgs, exportMarkdownOptions{})
	assertContainsAll(t, out, []string{
		`<tool_call name="Task" category="Task">`,
		`<tool_call name="Grep" category="Grep">`,
		`<tool_body><![CDATA[` + "\nbody two\n" + `]]></tool_body>`,
		`<thinking><![CDATA[` + "\n\n" + `]]></thinking>`,
		`<code_block language="txt"><![CDATA[` + "\ncode only\n" + `]]></code_block>`,
	})
}

func TestGenerateExportMarkdown_PreservesInlineBracketsInLegacyBodies(t *testing.T) {
	t.Parallel()
	session := testSession()
	msgs := []db.Message{{
		SessionID:  "test-id",
		Ordinal:    0,
		Role:       "assistant",
		Content:    "[Bash]\n$ test [ -f foo ] && echo ```not fence```",
		HasToolUse: true,
	}}
	out := generateExportMarkdown(session, msgs, exportMarkdownOptions{})
	assertContainsAll(t, out, []string{
		`<tool_call name="Bash" category="Bash">`,
		`<tool_body><![CDATA[` + "\n$ test [ -f foo ] && echo ```not fence```\n" + `]]></tool_body>`,
	})
}

func TestSanitizeFilename(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"Clean", "foo-bar.html", "foo-bar.html"},
		{"Spaces", "my file.html", "my_file.html"},
		{
			"SpecialChars",
			"a/b:c*d?.html",
			"a_b_c_d_.html",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := sanitizeFilename(tt.in)
			if got != tt.want {
				t.Errorf(
					"sanitizeFilename(%q) = %q, want %q",
					tt.in, got, tt.want,
				)
			}
		})
	}
}

func TestTruncateStr(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"Short", "hi", 10, "hi"},
		{"Exact", "hello", 5, "hello"},
		{"Long", "hello world", 5, "hello..."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := truncateStr(tt.in, tt.max)
			if got != tt.want {
				t.Errorf(
					"truncateStr(%q, %d) = %q, want %q",
					tt.in, tt.max, got, tt.want,
				)
			}
		})
	}
}

// TestExportTemplateValid ensures the template parses and
// renders without error for a minimal input.
func TestExportTemplateValid(t *testing.T) {
	t.Parallel()
	data := exportData{
		Project:      "test",
		Agent:        "Claude",
		MessageCount: 1,
		StartedAt:    "2025-01-15 10:00:00",
		Messages: []exportMessage{
			{
				RoleClass:   "user",
				Role:        "user",
				Timestamp:   "2025-01-15 10:00:00",
				ContentHTML: template.HTML("hello"),
			},
		},
	}
	var b strings.Builder
	if err := exportTmpl.Execute(&b, data); err != nil {
		t.Fatalf("template execution failed: %v", err)
	}
	if !strings.Contains(b.String(), "<!DOCTYPE html>") {
		t.Error("expected valid HTML doctype")
	}
}

func TestExportTemplateAccentColors(t *testing.T) {
	t.Parallel()
	// Every accent color used by the frontend must be defined in
	// the export template so exported HTML renders agent colors.
	required := []string{
		"--accent-blue",
		"--accent-rose",
		"--accent-purple",
		"--accent-amber",
		"--accent-green",
		"--accent-coral",
		"--accent-black",
		"--accent-teal",
		"--accent-red",
		"--accent-indigo",
	}
	for _, v := range required {
		if !strings.Contains(exportTemplateStr, v) {
			t.Errorf("export template missing CSS variable %s", v)
		}
	}
}

// --- GitHub API mock tests ---

func TestCreateGist(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		respStatus int
		respBody   string
		cancelCtx  bool
		wantErr    string
		wantID     string
		wantURL    string
		wantLogin  string
	}{
		{
			name:       "Success",
			respStatus: http.StatusCreated,
			respBody:   `{"id":"abc123","html_url":"https://gist.github.com/abc123","owner":{"login":"testuser"}}`,
			wantID:     "abc123",
			wantURL:    "https://gist.github.com/abc123",
			wantLogin:  "testuser",
		},
		{
			name:       "APIError",
			respStatus: http.StatusUnprocessableEntity,
			respBody:   `{"message":"Validation Failed"}`,
			wantErr:    "422",
		},
		{
			name:       "MalformedJSON",
			respStatus: http.StatusOK,
			respBody:   "not json",
			wantErr:    "parsing",
		},
		{
			name:       "MissingFields",
			respStatus: http.StatusCreated,
			respBody:   `{}`,
		},
		{
			name:       "ContextCancelled",
			respStatus: http.StatusOK,
			respBody:   "",
			cancelCtx:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ts := stubServer(t, http.MethodPost, "tok", tt.respStatus, tt.respBody)
			defer ts.Close()

			ctx := context.Background()
			if tt.cancelCtx {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}

			got, err := createGistWithURL(
				ctx, ts.URL, "tok", "f.html", "desc", "content",
			)

			if tt.cancelCtx {
				assertContextCancelled(t, err)
				return
			}

			if tt.wantErr != "" {
				assertErrorContains(t, err, tt.wantErr)
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if got.ID != tt.wantID {
				t.Errorf("expected ID %q, got %q", tt.wantID, got.ID)
			}
			if got.HTMLURL != tt.wantURL {
				t.Errorf("expected HTMLURL %q, got %q", tt.wantURL, got.HTMLURL)
			}
			if got.Owner.Login != tt.wantLogin {
				t.Errorf("expected Owner.Login %q, got %q", tt.wantLogin, got.Owner.Login)
			}
		})
	}
}

func TestValidateGithubToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		respStatus int
		respBody   string
		cancelCtx  bool
		wantErr    string
		wantLogin  string
	}{
		{
			name:       "Success",
			respStatus: http.StatusOK,
			respBody:   `{"login":"octocat"}`,
			wantLogin:  "octocat",
		},
		{
			name:       "Unauthorized",
			respStatus: http.StatusUnauthorized,
			respBody:   "",
			wantErr:    "invalid",
		},
		{
			name:       "ServerError",
			respStatus: http.StatusInternalServerError,
			respBody:   "",
			wantErr:    "500",
		},
		{
			name:       "MalformedJSON",
			respStatus: http.StatusOK,
			respBody:   "{broken",
			wantErr:    "parsing",
		},
		{
			name:       "ContextCancelled",
			respStatus: http.StatusOK,
			respBody:   "",
			cancelCtx:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ts := stubServer(t, http.MethodGet, "tok", tt.respStatus, tt.respBody)
			defer ts.Close()

			ctx := context.Background()
			if tt.cancelCtx {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}

			login, err := validateGithubTokenWithURL(
				ctx, ts.URL, "tok",
			)

			if tt.cancelCtx {
				assertContextCancelled(t, err)
				return
			}

			if tt.wantErr != "" {
				assertErrorContains(t, err, tt.wantErr)
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if login != tt.wantLogin {
				t.Errorf("expected login %q, got %q", tt.wantLogin, login)
			}
		})
	}
}
