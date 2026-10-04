package insight

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

func toolEffectivenessCall(tool, id, input, result, status string) db.ToolCall {
	call := db.ToolCall{
		ToolName: tool, Category: tool, ToolUseID: id, InputJSON: input,
		ResultContent: result, ResultContentLength: len(result),
	}
	if status != "" {
		call.ResultEvents = []db.ToolResultEvent{{
			ToolUseID: id, Source: "tool_execution", Status: status,
			Content: result, ContentLength: len(result),
		}}
	}
	return call
}

func toolEffectivenessMessage(sessionID string, ordinal int, calls ...db.ToolCall) db.Message {
	return db.Message{
		SessionID: sessionID, Ordinal: ordinal, Role: "assistant", Content: "tool call",
		Timestamp: "2026-04-26T10:00:01Z", HasToolUse: true, ToolCalls: calls,
	}
}

func seedToolEffectivenessSession(t *testing.T, d *db.DB, sessionID, termination string, msgs ...db.Message) {
	t.Helper()
	dbtest.SeedSession(t, d, sessionID, "tool-effectiveness", func(s *db.Session) {
		s.Agent = "claude"
		s.MessageCount = len(msgs)
		s.StartedAt = new("2026-04-26T10:00:00Z")
		if termination != "" {
			s.TerminationStatus = new(termination)
		}
	})
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), sessionID, msgs))
}

func buildToolEffectiveness(t *testing.T, d *db.DB, sessionID string) (string, ToolEffectivenessEvidence) {
	t.Helper()
	prompt, ev, err := BuildToolEffectivenessPrompt(t.Context(), d, GenerateRequest{
		Type: ToolEffectivenessType, SessionID: sessionID,
	})
	require.NoError(t, err)
	return prompt, ev
}

func seedIssueExample(t *testing.T, d *db.DB) {
	t.Helper()
	seedToolEffectivenessSession(t, d, "issue", "awaiting_user",
		db.Message{SessionID: "issue", Ordinal: 0, Role: "user", Content: "Find where the retry limit is set"},
		toolEffectivenessMessage("issue", 1, toolEffectivenessCall("Grep", "g1", `{"pattern":"retryLimit"}`, "No matches found", "completed")),
		toolEffectivenessMessage("issue", 2, toolEffectivenessCall("Grep", "g2", `{"pattern":"retryLimit"}`, "No matches found", "completed")),
		toolEffectivenessMessage("issue", 3, toolEffectivenessCall("Glob", "gl1", `{"pattern":"**/retry*.go"}`, "internal/retry/policy.go", "completed")),
		db.Message{SessionID: "issue", Ordinal: 4, Role: "assistant", Content: "The limit lives in internal/retry/policy.go."},
	)
}

func TestBuildToolEffectivenessPrompt_IssueExample(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	seedIssueExample(t, d)
	prompt, ev := buildToolEffectiveness(t, d, "issue")

	assert.Contains(t, prompt, "assessing how the tool calls")
	assert.Contains(t, prompt, "An empty result can be useful evidence")
	assert.Contains(t, prompt, "does not prove the task succeeded")
	assert.Contains(t, prompt, "## Tool evidence")
	assert.Contains(t, prompt, "### Call msg 1 #0 Grep\n- Outcome: empty\n- Status: completed\n- Sequence: 1\n\nInput:\n```\n{\"pattern\":\"retryLimit\"}\n```\n\nResult:\n```\nNo matches found\n```\n")
	assert.Contains(t, prompt, "### Call msg 2 #0 Grep\n- Outcome: empty\n- Status: completed\n- Sequence: 1\n")
	assert.Contains(t, prompt, "### Call msg 3 #0 Glob\n- Outcome: content\n- Status: completed\n- Sequence: 1\n")
	assert.Contains(t, prompt, "internal/retry/policy.go")
	assert.Contains(t, prompt, "- Sequence 1: messages 1, 2, 3; tools Grep, Glob; ending recovered; identical repeat yes; near-identical repeat no; tool switch yes\n")
	assert.Contains(t, prompt, "## Omissions\n\nNone.\n")
	assert.Equal(t, 3, ev.CallCount)
	assert.Empty(t, ev.Omissions)
	assert.Equal(t, map[int]bool{0: true, 1: true, 2: true, 3: true, 4: true}, ev.sentOrdinals)
}

// churningStore reports a new transcript revision, or termination status, on each session read
// until its budget of changes runs out.
type churningStore struct {
	db.Store
	changes     int
	reads       int
	termination bool
}

func (s *churningStore) GetSession(ctx context.Context, id string) (*db.Session, error) {
	sess, err := s.Store.GetSession(ctx, id)
	if err != nil || sess == nil {
		return sess, err
	}
	s.reads++
	if s.changes > 0 {
		s.changes--
		if s.termination {
			sess.TerminationStatus = new(fmt.Sprintf("status-%d", s.reads))
		} else {
			sess.TranscriptRevision = new(fmt.Sprintf("rev-%d", s.reads))
		}
	}
	return sess, nil
}

func TestBuildToolEffectivenessPrompt_RereadsWhileTheSessionChanges(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	seedIssueExample(t, d)
	req := GenerateRequest{Type: ToolEffectivenessType, SessionID: "issue"}

	settles := &churningStore{Store: d, changes: 2}
	_, ev, err := BuildToolEffectivenessPrompt(t.Context(), settles, req)
	require.NoError(t, err)
	assert.Equal(t, 3, ev.CallCount)
	stored, err := d.GetSession(t.Context(), "issue")
	require.NoError(t, err)
	assert.Equal(t, revisionOf(stored), ev.TranscriptRevision)
	assert.Equal(t, "awaiting_user", ev.TerminationStatus)

	_, _, err = BuildToolEffectivenessPrompt(t.Context(), &churningStore{Store: d, changes: 100}, req)
	require.ErrorIs(t, err, db.ErrSessionChanged)

	_, _, err = BuildToolEffectivenessPrompt(t.Context(), &churningStore{Store: d, changes: 100, termination: true}, req)
	require.ErrorIs(t, err, db.ErrSessionChanged)
}

// rebindingStore reports a different hosted source binding on each check
// until its budget of changes runs out.
type rebindingStore struct {
	db.Store
	changes int
	checks  int
}

func (s *rebindingStore) SessionSourceBinding(context.Context, string) (string, error) {
	s.checks++
	if s.changes > 0 {
		s.changes--
		return fmt.Sprintf("binding-%d", s.checks), nil
	}
	return "binding", nil
}

func (s *rebindingStore) SessionSourceChanged(error) bool { return false }

// revisionlessStore stands in for a backend that records no transcript revision.
type revisionlessStore struct{ db.Store }

func (s *revisionlessStore) GetSession(ctx context.Context, id string) (*db.Session, error) {
	sess, err := s.Store.GetSession(ctx, id)
	if sess != nil {
		sess.TranscriptRevision = nil
	}
	return sess, err
}

func TestBuildToolEffectivenessPrompt_RefusesSessionsWithoutRevision(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	seedIssueExample(t, d)
	req := GenerateRequest{Type: ToolEffectivenessType, SessionID: "issue"}
	_, _, err := BuildToolEffectivenessPrompt(t.Context(), &revisionlessStore{Store: d}, req)
	require.ErrorIs(t, err, db.ErrSessionRevisionUnavailable)
}

func TestBuildToolEffectivenessPrompt_RereadsWhileTheSourceMoves(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	seedIssueExample(t, d)
	req := GenerateRequest{Type: ToolEffectivenessType, SessionID: "issue"}

	settles := &rebindingStore{Store: d, changes: 2}
	_, _, err := BuildToolEffectivenessPrompt(t.Context(), settles, req)
	require.NoError(t, err)
	assert.Equal(t, 4, settles.checks)

	_, _, err = BuildToolEffectivenessPrompt(t.Context(), &rebindingStore{Store: d, changes: 100}, req)
	require.ErrorIs(t, err, db.ErrSessionChanged)
}

func TestBuildToolEffectivenessPrompt_FitsAgentArgumentLimit(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	big := strings.Repeat("x", 40<<10)
	seedToolEffectivenessSession(t, d, "big", "clean",
		db.Message{SessionID: "big", Ordinal: 0, Role: "user", Content: "go"},
		toolEffectivenessMessage("big", 1, toolEffectivenessCall("Read", "r1", `{}`, big, "completed")),
		toolEffectivenessMessage("big", 2, toolEffectivenessCall("Read", "r2", `{}`, big, "completed")),
	)
	req := GenerateRequest{Type: ToolEffectivenessType, SessionID: "big"}
	full, _, err := BuildToolEffectivenessPrompt(t.Context(), d, req)
	require.NoError(t, err)
	require.Greater(t, len(full), 80<<10)

	req.MaxPromptBytes = 30 << 10
	prompt, ev, err := BuildToolEffectivenessPrompt(t.Context(), d, req)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(prompt), req.MaxPromptBytes)
	require.NotEmpty(t, ev.Omissions)
	assert.Equal(t, OmissionBudget, ev.Omissions[0].Reason)

	req.MaxPromptBytes = 100
	_, _, err = BuildToolEffectivenessPrompt(t.Context(), d, req)
	require.ErrorIs(t, err, ErrPromptTooLarge)
}

func TestBuildToolEffectivenessPrompt_FitsEscapedWindowsArgument(t *testing.T) {
	saved := promptArgSize
	promptArgSize = windowsArgLength
	t.Cleanup(func() { promptArgSize = saved })
	d := dbtest.OpenTestDB(t)
	quoted := strings.Repeat(`a "b" `, 8<<10)
	seedToolEffectivenessSession(t, d, "quoted", "clean",
		db.Message{SessionID: "quoted", Ordinal: 0, Role: "user", Content: "go"},
		toolEffectivenessMessage("quoted", 1, toolEffectivenessCall("Read", "r1", `{}`, quoted, "completed")),
		toolEffectivenessMessage("quoted", 2, toolEffectivenessCall("Read", "r2", `{}`, quoted, "completed")),
	)
	req := GenerateRequest{Type: ToolEffectivenessType, SessionID: "quoted", MaxPromptBytes: 30 << 10}
	prompt, _, err := BuildToolEffectivenessPrompt(t.Context(), d, req)
	require.NoError(t, err)
	assert.LessOrEqual(t, windowsArgLength(prompt), req.MaxPromptBytes)
	assert.Greater(t, windowsArgLength(prompt), len(prompt))
}

func TestBuildToolEffectivenessPrompt_BudgetTrimsLargestFirst(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	big := strings.Repeat("a", 300<<10)
	medium := strings.Repeat("b", 200<<10)
	input := `{"q":"` + strings.Repeat("c", 10<<10-8) + `"}`
	small := strings.Repeat("d", 2<<10)
	seedToolEffectivenessSession(t, d, "budget", "clean",
		db.Message{SessionID: "budget", Ordinal: 0, Role: "user", Content: "go"},
		toolEffectivenessMessage("budget", 1, toolEffectivenessCall("Bash", "b1", `{}`, big, "completed")),
		toolEffectivenessMessage("budget", 2, toolEffectivenessCall("Bash", "b2", input, medium, "completed")),
		toolEffectivenessMessage("budget", 3, toolEffectivenessCall("Read", "r1", `{}`, small, "completed")),
	)
	prompt, ev := buildToolEffectiveness(t, d, "budget")

	fixed := 2 + 2 + len(input) + len(small)
	limit := (toolEvidenceBudgetBytes - fixed) / 2
	require.Len(t, ev.Omissions, 2)
	assert.Equal(t, ToolEvidenceOmission{
		Reason: OmissionBudget, Ordinal: new(1), CallIndex: new(0), ToolName: "Bash",
		Field: "result", KeptBytes: new(limit), OriginalBytes: new(len(big)),
	}, ev.Omissions[0])
	assert.Equal(t, ToolEvidenceOmission{
		Reason: OmissionBudget, Ordinal: new(2), CallIndex: new(0), ToolName: "Bash",
		Field: "result", KeptBytes: new(limit), OriginalBytes: new(len(medium)),
	}, ev.Omissions[1])
	assert.LessOrEqual(t, fixed+2*limit, toolEvidenceBudgetBytes)
	assert.Contains(t, prompt, input)
	assert.Contains(t, prompt, small)
	assert.NotContains(t, prompt, strings.Repeat("a", limit+1))
	assert.Contains(t, prompt, "msg 1 #0 Bash result: cut to")

	again, _ := buildToolEffectiveness(t, d, "budget")
	assert.Equal(t, prompt, again)
}

func TestBuildToolEffectivenessPrompt_FencesAndUTF8(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	fenced := "before\n`````\ninside\n`````\nafter"
	cjk := strings.Repeat("界", 120<<10)
	seedToolEffectivenessSession(t, d, "fence", "clean",
		db.Message{SessionID: "fence", Ordinal: 0, Role: "user", Content: "go"},
		toolEffectivenessMessage("fence", 1, toolEffectivenessCall("Read", "r1", `{}`, fenced, "completed")),
		toolEffectivenessMessage("fence", 2, toolEffectivenessCall("Read", "r2", `{}`, cjk, "completed")),
	)
	prompt, ev := buildToolEffectiveness(t, d, "fence")

	assert.Contains(t, prompt, "``````\n"+fenced+"\n``````\n")
	assert.True(t, utf8.ValidString(prompt))
	require.Len(t, ev.Omissions, 1)
	assert.Equal(t, 0, *ev.Omissions[0].KeptBytes%3)
}

func TestBuildToolEffectivenessPrompt_UnretainedResults(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	staged := toolEffectivenessCall("Read", "staged", `{}`, "[image]", "completed")
	withheld := db.ToolCall{ToolName: "Read", Category: "Read", ToolUseID: "withheld", InputJSON: `{}`, ResultContentLength: 17}
	missing := db.ToolCall{ToolName: "Bash", Category: "Bash", ToolUseID: "missing", InputJSON: `{}`}
	seedToolEffectivenessSession(t, d, "unretained", "clean",
		db.Message{SessionID: "unretained", Ordinal: 0, Role: "user", Content: "go"},
		toolEffectivenessMessage("unretained", 1, staged),
		toolEffectivenessMessage("unretained", 2, withheld),
		toolEffectivenessMessage("unretained", 3, missing),
	)
	prompt, ev := buildToolEffectiveness(t, d, "unretained")

	assert.Equal(t, 3, strings.Count(prompt, "(result not retained)"))
	require.Len(t, ev.Omissions, 3)
	for _, o := range ev.Omissions {
		assert.Equal(t, OmissionUnretained, o.Reason)
	}
	assert.Equal(t, new(17), ev.Omissions[1].OriginalBytes)
	assert.Nil(t, ev.Omissions[2].OriginalBytes)
	assert.Contains(t, prompt, "msg 2 #0 Read result: not retained (17 bytes originally)")
	assert.True(t, ev.allResultsUnknown)
}

func TestBuildToolEffectivenessPrompt_NoCitableMessages(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	req := GenerateRequest{Type: ToolEffectivenessType, SessionID: "empty"}
	seedToolEffectivenessSession(t, d, "empty", "clean")
	_, _, err := BuildToolEffectivenessPrompt(t.Context(), d, req)
	require.ErrorIs(t, err, ErrNoCitableMessages)

	req.SessionID = "system"
	seedToolEffectivenessSession(t, d, "system", "clean",
		db.Message{SessionID: "system", Ordinal: 0, Role: "user", Content: "setup", IsSystem: true},
	)
	_, _, err = BuildToolEffectivenessPrompt(t.Context(), d, req)
	require.ErrorIs(t, err, ErrNoCitableMessages)
}

func TestBuildToolEffectivenessPrompt_CompletedEmptyResult(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	seedToolEffectivenessSession(t, d, "quiet", "clean",
		db.Message{SessionID: "quiet", Ordinal: 0, Role: "user", Content: "go"},
		toolEffectivenessMessage("quiet", 1, toolEffectivenessCall("Bash", "b1", `{"command":"true"}`, "", "completed")),
	)
	prompt, ev := buildToolEffectiveness(t, d, "quiet")

	assert.NotContains(t, prompt, "(result not retained)")
	assert.Empty(t, ev.Omissions)
	detail := ev.callDetails[[2]int{1, 0}]
	assert.Equal(t, "unknown", detail.Outcome)
	assert.Equal(t, new(0), detail.ResultBytes)
}

func TestBuildToolEffectivenessPrompt_WithheldErrorResult(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	failed := db.ToolCall{
		ToolName: "Read", Category: "Read", ToolUseID: "failed", InputJSON: `{}`, ResultContentLength: 17,
		ResultEvents: []db.ToolResultEvent{{ToolUseID: "failed", Source: "tool_execution", Status: "errored", ContentLength: 17}},
	}
	seedToolEffectivenessSession(t, d, "withheld-error", "clean",
		db.Message{SessionID: "withheld-error", Ordinal: 0, Role: "user", Content: "go"},
		toolEffectivenessMessage("withheld-error", 1, failed),
	)
	prompt, ev := buildToolEffectiveness(t, d, "withheld-error")

	assert.Contains(t, prompt, "msg 1 #0 Read result: not retained (17 bytes originally)")
	require.Len(t, ev.Omissions, 1)
	assert.Equal(t, OmissionUnretained, ev.Omissions[0].Reason)
	assert.Equal(t, new(17), ev.Omissions[0].OriginalBytes)
	assert.Equal(t, "errored", ev.callDetails[[2]int{1, 0}].Outcome)
}

func TestBuildToolEffectivenessPrompt_PreviewOmission(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	seedToolEffectivenessSession(t, d, "preview", "clean",
		db.Message{SessionID: "preview", Ordinal: 0, Role: "user", Content: "go"},
		db.Message{SessionID: "preview", Ordinal: 1, Role: "assistant", Content: strings.Repeat("x", 900)},
	)
	prompt, ev := buildToolEffectiveness(t, d, "preview")

	assert.Equal(t, []ToolEvidenceOmission{{Reason: OmissionPreviews, Count: 1}}, ev.Omissions)
	assert.Contains(t, prompt, "- 1 messages shown as 800-character previews")
}

func TestBuildToolEffectivenessPrompt_TranscriptCompletion(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	for _, tc := range []struct{ termination, ending string }{
		{"awaiting_user", "abandoned"},
		{"tool_call_pending", "open"},
	} {
		id := "completion-" + tc.termination
		seedToolEffectivenessSession(t, d, id, tc.termination,
			db.Message{SessionID: id, Ordinal: 0, Role: "user", Content: "go"},
			toolEffectivenessMessage(id, 1, toolEffectivenessCall("Bash", "b1", `{}`, "command failed", "errored")),
		)
		prompt, _ := buildToolEffectiveness(t, d, id)
		assert.Contains(t, prompt, "ending "+tc.ending+";", tc.termination)
	}
}

func issueEvidence(t *testing.T) ToolEffectivenessEvidence {
	t.Helper()
	d := dbtest.OpenTestDB(t)
	seedIssueExample(t, d)
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "issue", []db.Message{
		{SessionID: "issue", Ordinal: 0, Role: "user", Content: "Find where the retry limit is set"},
		{SessionID: "issue", Ordinal: 1, Role: "user", Content: "system note", IsSystem: true},
		toolEffectivenessMessage("issue", 2, toolEffectivenessCall("Grep", "g1", `{"pattern":"retryLimit"}`, "", "completed")),
		toolEffectivenessMessage("issue", 3, toolEffectivenessCall("Glob", "gl1", `{}`, "a.go", "completed")),
	}))
	_, ev := buildToolEffectiveness(t, d, "issue")
	return ev
}

func TestValidateToolEffectivenessReport(t *testing.T) {
	ev := issueEvidence(t)
	valid := ToolEffectivenessConclusion{Assessment: AssessmentHelped, Text: "Glob found it", Ordinals: []int{0, 3}, Calls: []ToolEffectivenessCallRef{{Ordinal: 3, CallIndex: 0}}}
	require.NoError(t, ValidateToolEffectivenessReport(ToolEffectivenessReport{Conclusions: []ToolEffectivenessConclusion{valid}}, ev))

	for name, mutate := range map[string]func(*ToolEffectivenessConclusion){
		"unknown ordinal":  func(c *ToolEffectivenessConclusion) { c.Ordinals = []int{999} },
		"system ordinal":   func(c *ToolEffectivenessConclusion) { c.Ordinals = []int{1} },
		"negative ordinal": func(c *ToolEffectivenessConclusion) { c.Ordinals = []int{-1} },
		"empty ordinals":   func(c *ToolEffectivenessConclusion) { c.Ordinals = nil },
		"unsent call":      func(c *ToolEffectivenessConclusion) { c.Calls = []ToolEffectivenessCallRef{{Ordinal: 3, CallIndex: 1}} },
		"bad assessment":   func(c *ToolEffectivenessConclusion) { c.Assessment = "great" },
		"empty text":       func(c *ToolEffectivenessConclusion) { c.Text = "  " },
	} {
		c := valid
		mutate(&c)
		require.Error(t, ValidateToolEffectivenessReport(ToolEffectivenessReport{Conclusions: []ToolEffectivenessConclusion{c}}, ev), name)
	}
	assert.Error(t, ValidateToolEffectivenessReport(ToolEffectivenessReport{}, ev))
}

func TestValidateToolEffectivenessReport_UnknownOnly(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	seedToolEffectivenessSession(t, d, "placeholders", "clean",
		db.Message{SessionID: "placeholders", Ordinal: 0, Role: "user", Content: "go"},
		toolEffectivenessMessage("placeholders", 1, db.ToolCall{ToolName: "Read", Category: "Read", ToolUseID: "w", ResultContentLength: 40}),
	)
	_, ev := buildToolEffectiveness(t, d, "placeholders")
	helped := ToolEffectivenessReport{Conclusions: []ToolEffectivenessConclusion{{Assessment: AssessmentHelped, Text: "x", Ordinals: []int{1}}}}
	require.Error(t, ValidateToolEffectivenessReport(helped, ev))
	unknown := ToolEffectivenessReport{Conclusions: []ToolEffectivenessConclusion{{Assessment: AssessmentUnknown, Text: "x", Ordinals: []int{1}}}}
	require.NoError(t, ValidateToolEffectivenessReport(unknown, ev))

	seedToolEffectivenessSession(t, d, "no-calls", "clean",
		db.Message{SessionID: "no-calls", Ordinal: 0, Role: "user", Content: "hi"},
		db.Message{SessionID: "no-calls", Ordinal: 1, Role: "assistant", Content: "hello"},
	)
	_, noCalls := buildToolEffectiveness(t, d, "no-calls")
	require.Error(t, ValidateToolEffectivenessReport(ToolEffectivenessReport{Conclusions: []ToolEffectivenessConclusion{{Assessment: AssessmentDidNotHelp, Text: "x", Ordinals: []int{1}}}}, noCalls))
	require.NoError(t, ValidateToolEffectivenessReport(ToolEffectivenessReport{Conclusions: []ToolEffectivenessConclusion{{Assessment: AssessmentUnknown, Text: "x", Ordinals: []int{1}}}}, noCalls))

	seedToolEffectivenessSession(t, d, "withheld-finished", "clean",
		db.Message{SessionID: "withheld-finished", Ordinal: 0, Role: "user", Content: "go"},
		toolEffectivenessMessage("withheld-finished", 1, db.ToolCall{
			ToolName: "Bash", Category: "Bash", ToolUseID: "w", ResultContentLength: 17,
			ResultEvents: []db.ToolResultEvent{{ToolUseID: "w", Source: "tool_execution", Status: "completed", ContentLength: 17}},
		}),
	)
	_, withheld := buildToolEffectiveness(t, d, "withheld-finished")
	require.Error(t, ValidateToolEffectivenessReport(helped, withheld), "a finished call whose output was withheld is still unknown")

	emptyHelped := ToolEffectivenessReport{Conclusions: []ToolEffectivenessConclusion{{Assessment: AssessmentHelped, Text: "ruled out", Ordinals: []int{2}}}}
	assert.NoError(t, ValidateToolEffectivenessReport(emptyHelped, issueEvidence(t)))
}

func TestValidateToolEffectivenessReport_MultiCallMessage(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	seedToolEffectivenessSession(t, d, "multi", "clean",
		db.Message{SessionID: "multi", Ordinal: 0, Role: "user", Content: "go"},
		toolEffectivenessMessage("multi", 1,
			db.ToolCall{ToolName: "Grep", Category: "Grep", ToolUseID: "a", ResultContent: "No matches found", ResultContentLength: 16},
			db.ToolCall{ToolName: "Read", Category: "Read", ToolUseID: "b", ResultContent: "{}", ResultContentLength: 2},
		),
	)
	_, ev := buildToolEffectiveness(t, d, "multi")
	bare := ToolEffectivenessConclusion{Assessment: AssessmentHelped, Text: "Read found it", Ordinals: []int{1}}
	require.ErrorContains(t, ValidateToolEffectivenessReport(ToolEffectivenessReport{Conclusions: []ToolEffectivenessConclusion{bare}}, ev), "must name one")
	named := bare
	named.Calls = []ToolEffectivenessCallRef{{Ordinal: 1, CallIndex: 1}}
	assert.NoError(t, ValidateToolEffectivenessReport(ToolEffectivenessReport{Conclusions: []ToolEffectivenessConclusion{named}}, ev))
	assert.NoError(t, ValidateToolEffectivenessReport(ToolEffectivenessReport{Conclusions: []ToolEffectivenessConclusion{{Assessment: AssessmentUnknown, Text: "context", Ordinals: []int{0}}}}, ev))
}

func TestParseToolEffectivenessReport(t *testing.T) {
	r, err := ParseToolEffectivenessReport("```json\n{\"conclusions\":[{\"assessment\":\"unknown\",\"text\":\"t\",\"ordinals\":[0]}]}\n```")
	require.NoError(t, err)
	require.Len(t, r.Conclusions, 1)
	assert.Equal(t, []int{0}, r.Conclusions[0].Ordinals)

	r, err = ParseToolEffectivenessReport("Here is the report:\n```json\n{\"conclusions\":[{\"assessment\":\"helped\",\"text\":\"t\",\"ordinals\":[1]}]}\n```\nDone.")
	require.NoError(t, err)
	assert.Equal(t, AssessmentHelped, r.Conclusions[0].Assessment)

	r, err = ParseToolEffectivenessReport(`{"conclusions":[{"assessment":"unknown","text":"t","ordinals":[2]}]}` + "\nThat is all.")
	require.NoError(t, err)
	assert.Equal(t, []int{2}, r.Conclusions[0].Ordinals)

	_, err = ParseToolEffectivenessReport(`{"conclusions":[{"assessment":"helped","text":"t","ordinals":[2],"calls":[{"ordinal":2}]}]}`)
	require.ErrorContains(t, err, "call_index")
	_, err = ParseToolEffectivenessReport(`{"conclusions":[{"assessment":"helped","text":"t","ordinals":[2],"calls":[{"ordinal":2,"call_index":1,"x":1}]}]}`)
	require.Error(t, err)

	_, err = ParseToolEffectivenessReport(`{"conclusions":[],"extra":1}`)
	require.Error(t, err)
	_, err = ParseToolEffectivenessReport("The calls helped.")
	assert.Error(t, err)
}

func TestToolEffectivenessStructuredAndMarkdown(t *testing.T) {
	ev := ToolEffectivenessEvidence{SessionID: "s", CallCount: 2, Omissions: []ToolEvidenceOmission{{
		Reason: OmissionUnretained, Ordinal: new(2), CallIndex: new(0), ToolName: "Read", Field: "result",
	}}}
	r := ToolEffectivenessReport{Conclusions: []ToolEffectivenessConclusion{{
		Assessment: AssessmentDidNotHelp, Text: "Repeated the same search", Ordinals: []int{1, 2},
		Calls: []ToolEffectivenessCallRef{{Ordinal: 2, CallIndex: 0}},
	}}}
	raw, err := ToolEffectivenessStructuredJSON(r, ev)
	require.NoError(t, err)
	var saved map[string]any
	require.NoError(t, json.Unmarshal(raw, &saved))
	assert.Equal(t, "s", saved["session_id"])
	assert.EqualValues(t, 2, saved["call_count"])
	assert.Len(t, saved["conclusions"], 1)
	assert.Len(t, saved["omissions"], 1)

	md := RenderToolEffectivenessMarkdown(r, ev)
	assert.Contains(t, md, "## Model assessment\n\n- **Did not help**: Repeated the same search (msg 1, msg 2, msg 2 #0)\n")
	assert.Contains(t, md, "## Observed tool sequences\n\nNone.\n")
	assert.Contains(t, md, "## Omissions\n\n- msg 2 #0 Read result: not retained\n")

	raw, err = ToolEffectivenessStructuredJSON(r, ToolEffectivenessEvidence{SessionID: "s"})
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"omissions":[]`)
}

func TestToolEffectivenessStructured_CitedCalls(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	seedIssueExample(t, d)
	_, ev := buildToolEffectiveness(t, d, "issue")
	r := ToolEffectivenessReport{Conclusions: []ToolEffectivenessConclusion{
		{Assessment: AssessmentDidNotHelp, Text: "Repeated search", Ordinals: []int{2, 1}, Calls: []ToolEffectivenessCallRef{{Ordinal: 2, CallIndex: 0}}},
		{Assessment: AssessmentHelped, Text: "Answer", Ordinals: []int{4}},
	}}
	raw, err := ToolEffectivenessStructuredJSON(r, ev)
	require.NoError(t, err)
	var saved ToolEffectivenessStructured
	require.NoError(t, json.Unmarshal(raw, &saved))
	assert.Equal(t, []ToolEffectivenessCitedCall{
		{Ordinal: 1, CallIndex: 0, ToolName: "Grep", InputPreview: `{"pattern":"retryLimit"}`, Outcome: "empty", ResultBytes: new(16), MessageCalls: 1},
		{Ordinal: 2, CallIndex: 0, ToolName: "Grep", InputPreview: `{"pattern":"retryLimit"}`, Outcome: "empty", ResultBytes: new(16), MessageCalls: 1},
	}, saved.CitedCalls)

	md := RenderToolEffectivenessMarkdown(r, ev)
	assert.Contains(t, md, "## Observed tool sequences\n\n- Sequence 1: messages 1, 2")
}

func TestParseToolEffectivenessReport_DropsRepeatedCitations(t *testing.T) {
	r, err := ParseToolEffectivenessReport(`{"conclusions":[{"assessment":"helped","text":"x","ordinals":[3,2,3],"calls":[{"ordinal":3,"call_index":1},{"ordinal":3,"call_index":0},{"ordinal":3,"call_index":1}]}]}`)
	require.NoError(t, err)
	require.Len(t, r.Conclusions, 1)
	assert.Equal(t, []int{2, 3}, r.Conclusions[0].Ordinals)
	assert.Equal(t, []ToolEffectivenessCallRef{{Ordinal: 3, CallIndex: 1}, {Ordinal: 3, CallIndex: 0}}, r.Conclusions[0].Calls)
}

func TestToolEffectivenessCorrectionPrompt(t *testing.T) {
	got := ToolEffectivenessCorrectionPrompt("prompt", errors.New("conclusion 0: msg 2 holds 2 calls, so the conclusion must name one"))
	assert.True(t, strings.HasPrefix(got, "prompt\n## Correction\n"))
	assert.Contains(t, got, "msg 2 holds 2 calls")
}
