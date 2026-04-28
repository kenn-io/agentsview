package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/wesm/agentsview/internal/db"
)

type sessionSpec struct {
	project           string
	suffix            string
	msgCount          int
	userMsgCount      int
	parentSessionID   string
	relationshipType  string
	terminationStatus string
}

var specs = []sessionSpec{
	{"project-alpha", "small-2", 2, 2, "", "", ""},
	{"project-alpha", "small-5", 5, 3, "", "", ""},
	// One unclean session for e2e termination tests.
	{"project-beta", "mixed-content-7", 7, 3, "", "",
		"tool_call_pending"},
	{"project-beta", "medium-8", 8, 4, "", "", ""},
	{"project-beta", "medium-100", 100, 50, "", "", ""},
	{"project-gamma", "large-200", 200, 100, "", "", ""},
	{"project-gamma", "large-1500", 1500, 750, "", "", ""},
	{"project-delta", "xlarge-5500", 5500, 2750, "", "", ""},

	// Sub-agent and fork sessions: must NOT appear in session
	// list, stats, or analytics summary counts.
	{"project-alpha", "subagent-1", 12, 6,
		"test-session-small-5", "subagent", ""},
	{"project-alpha", "subagent-2", 8, 4,
		"test-session-small-5", "subagent", ""},
	{"project-beta", "fork-1", 15, 7,
		"test-session-medium-8", "fork", ""},

	// Empty session (0 messages): must also be excluded.
	{"project-gamma", "empty-0", 0, 0, "", "", ""},
}

func main() {
	out := flag.String("out", "", "output database path")
	flag.Parse()
	if *out == "" {
		fmt.Fprintln(os.Stderr, "usage: testfixture -out <path>")
		os.Exit(1)
	}

	if err := os.Remove(*out); err != nil &&
		!errors.Is(err, os.ErrNotExist) {
		log.Fatalf("removing existing db: %v", err)
	}

	database, err := db.Open(*out)
	if err != nil {
		log.Fatalf("opening db: %v", err)
	}
	defer database.Close()

	// Seed model pricing for usage page e2e tests.
	if err := database.UpsertModelPricing([]db.ModelPricing{
		{
			ModelPattern:         "claude-sonnet-4-20250514",
			InputPerMTok:         3.0,
			OutputPerMTok:        15.0,
			CacheCreationPerMTok: 3.75,
			CacheReadPerMTok:     0.30,
		},
		{
			ModelPattern:         "claude-opus-4-20250514",
			InputPerMTok:         15.0,
			OutputPerMTok:        75.0,
			CacheCreationPerMTok: 18.75,
			CacheReadPerMTok:     1.50,
		},
	}); err != nil {
		log.Fatalf("seeding model pricing: %v", err)
	}

	// Use a recent base date so fixture data stays within the
	// default 1-year analytics window.
	base := time.Now().UTC().AddDate(0, 0, -30).
		Truncate(24 * time.Hour).Add(10 * time.Hour)

	for i, spec := range specs {
		if err := createSessionFixture(
			database, spec, i, base,
		); err != nil {
			log.Fatalf("creating fixture %s: %v", spec.suffix, err)
		}
		fmt.Printf(
			"  test-session-%s: %d messages\n",
			spec.suffix, spec.msgCount,
		)
	}

	if err := createDurationShowcaseFixture(
		database, base.Add(72*time.Hour),
	); err != nil {
		log.Fatalf("creating duration showcase: %v", err)
	}

	fmt.Printf("Fixture DB written to %s\n", *out)
}

func createSessionFixture(
	database *db.DB, spec sessionSpec,
	index int, base time.Time,
) error {
	sessionID := fmt.Sprintf("test-session-%s", spec.suffix)
	startedAt := base.Add(
		time.Duration(index) * 24 * time.Hour,
	)
	endedAt := startedAt.Add(
		time.Duration(spec.msgCount) * time.Minute,
	)

	sess := db.Session{
		ID:               sessionID,
		Project:          spec.project,
		Machine:          "test-machine",
		Agent:            "claude",
		StartedAt:        new(startedAt.Format(time.RFC3339Nano)),
		EndedAt:          new(endedAt.Format(time.RFC3339Nano)),
		MessageCount:     spec.msgCount,
		UserMessageCount: spec.userMsgCount,
		RelationshipType: spec.relationshipType,
	}
	if spec.parentSessionID != "" {
		sess.ParentSessionID = new(spec.parentSessionID)
	}
	if spec.terminationStatus != "" {
		sess.TerminationStatus = new(spec.terminationStatus)
	}
	if spec.msgCount > 0 {
		sess.FirstMessage = new(
			fmt.Sprintf("First message for %s", spec.project),
		)
	}
	if err := database.UpsertSession(sess); err != nil {
		return fmt.Errorf("upserting session: %w", err)
	}

	if spec.msgCount == 0 {
		return nil
	}

	model := "claude-sonnet-4-20250514"
	if index%3 == 1 {
		model = "claude-opus-4-20250514"
	}

	var msgs []db.Message
	if spec.suffix == "mixed-content-7" {
		msgs = generateMixedContentMessages(
			sessionID, startedAt, model,
		)
	} else {
		msgs = generateMessages(
			sessionID, spec.msgCount, startedAt, model,
		)
	}
	if err := database.InsertMessages(msgs); err != nil {
		return fmt.Errorf("inserting messages: %w", err)
	}
	return nil
}

func generateMessages(
	sessionID string, count int,
	start time.Time, model string,
) []db.Message {
	msgs := make([]db.Message, 0, count)
	for i := range count {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}

		ts := start.Add(time.Duration(i) * time.Minute)
		content := generateContent(role, i, count)

		msg := db.Message{
			SessionID:     sessionID,
			Ordinal:       i,
			Role:          role,
			Content:       content,
			Timestamp:     ts.Format(time.RFC3339Nano),
			HasThinking:   role == "assistant" && i%5 == 0,
			HasToolUse:    role == "assistant" && i%3 == 0,
			ContentLength: len(content),
		}

		if role == "assistant" && model != "" {
			msg.Model = model
			inputTok := 500 + (i*137)%2000
			outputTok := 200 + (i*89)%800
			cacheCr := 50 + (i*31)%200
			cacheRd := 1000 + (i*53)%4000
			msg.TokenUsage = json.RawMessage(
				fmt.Sprintf(
					`{"input_tokens":%d,`+
						`"output_tokens":%d,`+
						`"cache_creation_input_tokens":%d,`+
						`"cache_read_input_tokens":%d}`,
					inputTok, outputTok,
					cacheCr, cacheRd,
				),
			)
		}

		msgs = append(msgs, msg)
	}
	return msgs
}

func generateMixedContentMessages(
	sessionID string, start time.Time, model string,
) []db.Message {
	type spec struct {
		role        string
		content     string
		hasThinking bool
		hasToolUse  bool
	}

	specs := []spec{
		{
			role:    "user",
			content: "Help me read a file",
		},
		{
			role: "assistant",
			content: "[Thinking]\nLet me analyze..." +
				"\n\nHere is my analysis.",
			hasThinking: true,
		},
		{
			role:    "user",
			content: "Now check the directory",
		},
		{
			role:       "assistant",
			content:    "[Read /src/main.ts]\nconst app = express();",
			hasToolUse: true,
		},
		{
			role:       "assistant",
			content:    "[Bash]\nls -la /src",
			hasToolUse: true,
		},
		{
			role: "assistant",
			content: "[Thinking]\nGemini-style reasoning\n" +
				"[/Thinking]\n\n" +
				"This is the visible response after thinking.",
			hasThinking: true,
		},
		{
			role:    "user",
			content: "Thanks",
		},
	}

	msgs := make([]db.Message, 0, len(specs))
	for i, s := range specs {
		ts := start.Add(time.Duration(i) * time.Minute)
		msg := db.Message{
			SessionID:     sessionID,
			Ordinal:       i,
			Role:          s.role,
			Content:       s.content,
			Timestamp:     ts.Format(time.RFC3339Nano),
			HasThinking:   s.hasThinking,
			HasToolUse:    s.hasToolUse,
			ContentLength: len(s.content),
		}
		if s.role == "assistant" && model != "" {
			msg.Model = model
			inputTok := 400 + (i*113)%1500
			outputTok := 150 + (i*67)%600
			cacheCr := 30 + (i*23)%150
			cacheRd := 800 + (i*41)%3000
			msg.TokenUsage = json.RawMessage(
				fmt.Sprintf(
					`{"input_tokens":%d,`+
						`"output_tokens":%d,`+
						`"cache_creation_input_tokens":%d,`+
						`"cache_read_input_tokens":%d}`,
					inputTok, outputTok,
					cacheCr, cacheRd,
				),
			)
		}
		msgs = append(msgs, msg)
	}
	return msgs
}

func generateContent(role string, idx, total int) string {
	if role == "user" {
		return fmt.Sprintf(
			"User message %d of %d. "+
				"Please help me with this task. "+
				"I need to understand how the code works.",
			idx, total,
		)
	}
	return fmt.Sprintf(
		"Assistant response %d of %d. "+
			"Here is my analysis of the code. "+
			"The implementation follows standard patterns "+
			"and uses well-known libraries. "+
			"Let me explain the key components.",
		idx, total,
	)
}

// createDurationShowcaseFixture builds a parent session that
// exercises every shape the Session Vital Signs UX renders:
// solo tool call, parallel turn with a sub-agent, and a slow
// solo Bash. Together with the linked sub-agent session it
// gives the right-panel timing query stable data to display.
//
// Timeline (relative to start):
//
//	T+0:00  msg 0  user      "investigate auth"
//	T+0:02  msg 1  assistant solo Read (tool_use)
//	T+0:04  msg 2  user      tool_result for Read
//	T+0:14  msg 3  assistant parallel: 2 Reads + 1 Task
//	T+2:14  msg 4  user      tool_results for all 3
//	T+2:24  msg 5  assistant solo Bash
//	T+2:52  msg 6  user      tool_result for Bash
//	T+2:55  session ends
//
// Per the timing spec, turn durations come from the gap to
// the next message; sub-agent calls take their duration from
// the linked child session's started_at/ended_at.
func createDurationShowcaseFixture(
	database *db.DB, start time.Time,
) error {
	const (
		parentID   = "test-session-duration-showcase"
		subagentID = "test-session-duration-subagent-1"
		project    = "project-duration"
		model      = "claude-sonnet-4-20250514"
	)

	// Per-message timestamps anchored on `start`.
	t0 := start
	t1 := start.Add(2 * time.Second)
	t2 := start.Add(4 * time.Second)
	t3 := start.Add(14 * time.Second)
	t4 := start.Add(2*time.Minute + 14*time.Second)
	t5 := start.Add(2*time.Minute + 24*time.Second)
	t6 := start.Add(2*time.Minute + 52*time.Second)
	endParent := start.Add(2*time.Minute + 55*time.Second)

	// Sub-agent runs alongside the parallel turn so its
	// duration covers the full ~2 minutes of that turn.
	subStart := t3
	subEnd := t4
	subAgentMessages := buildDurationSubagentMessages(
		subagentID, subStart,
	)

	subSess := db.Session{
		ID:               subagentID,
		Project:          project,
		Machine:          "test-machine",
		Agent:            "claude",
		StartedAt:        new(subStart.Format(time.RFC3339Nano)),
		EndedAt:          new(subEnd.Format(time.RFC3339Nano)),
		MessageCount:     len(subAgentMessages),
		UserMessageCount: countUserMessages(subAgentMessages),
		ParentSessionID:  new(parentID),
		RelationshipType: "subagent",
		FirstMessage: new(
			"Inspect middleware request flow",
		),
	}
	if err := database.UpsertSession(subSess); err != nil {
		return fmt.Errorf(
			"upserting subagent session: %w", err,
		)
	}
	if err := database.InsertMessages(
		subAgentMessages,
	); err != nil {
		return fmt.Errorf(
			"inserting subagent messages: %w", err,
		)
	}
	fmt.Printf(
		"  %s: %d messages (subagent)\n",
		subagentID, len(subAgentMessages),
	)

	parentMessages := buildDurationShowcaseMessages(
		parentID, subagentID, model,
		t0, t1, t2, t3, t4, t5, t6,
	)

	parentSess := db.Session{
		ID:               parentID,
		Project:          project,
		Machine:          "test-machine",
		Agent:            "claude",
		StartedAt:        new(t0.Format(time.RFC3339Nano)),
		EndedAt:          new(endParent.Format(time.RFC3339Nano)),
		MessageCount:     len(parentMessages),
		UserMessageCount: countUserMessages(parentMessages),
		FirstMessage: new(
			"Investigate auth middleware performance",
		),
	}
	if err := database.UpsertSession(parentSess); err != nil {
		return fmt.Errorf(
			"upserting showcase session: %w", err,
		)
	}
	if err := database.InsertMessages(
		parentMessages,
	); err != nil {
		return fmt.Errorf(
			"inserting showcase messages: %w", err,
		)
	}
	fmt.Printf(
		"  %s: %d messages (duration showcase)\n",
		parentID, len(parentMessages),
	)
	return nil
}

func countUserMessages(msgs []db.Message) int {
	n := 0
	for _, m := range msgs {
		if m.Role == "user" {
			n++
		}
	}
	return n
}

// buildDurationShowcaseMessages assembles the parent session's
// messages and tool calls. ToolUseIDs are stable strings so the
// IDs surface intact in the tool_calls table; matching results
// would arrive in the next user message in a real transcript,
// but the result_content_length lives on the originating call
// row in this DB schema, so it's set there directly.
func buildDurationShowcaseMessages(
	sessionID, subagentID, model string,
	t0, t1, t2, t3, t4, t5, t6 time.Time,
) []db.Message {
	const (
		readSoloID = "tu_read_solo"
		readPar1ID = "tu_read_par1"
		readPar2ID = "tu_read_par2"
		taskID     = "tu_task_subagent"
		bashSlowID = "tu_bash_slow"
	)

	tokenUsage := func(seed int) json.RawMessage {
		input := 600 + seed*150
		output := 220 + seed*80
		cacheCr := 60 + seed*15
		cacheRd := 1100 + seed*40
		return json.RawMessage(fmt.Sprintf(
			`{"input_tokens":%d,`+
				`"output_tokens":%d,`+
				`"cache_creation_input_tokens":%d,`+
				`"cache_read_input_tokens":%d}`,
			input, output, cacheCr, cacheRd,
		))
	}

	msg0Content := "Take a look at the auth middleware " +
		"and figure out where time is going."
	msg1Content := "Reading the middleware so I can map " +
		"the request flow."
	msg2Content := "[tool_result]"
	msg3Content := "Fanning out: two reads plus a sub-agent " +
		"to dig into the session helpers."
	msg4Content := "[tool_results]"
	msg5Content := "Running the auth test suite to confirm " +
		"the slow path matches what I read."
	msg6Content := "[tool_result]"

	return []db.Message{
		{
			SessionID:     sessionID,
			Ordinal:       0,
			Role:          "user",
			Content:       msg0Content,
			Timestamp:     t0.Format(time.RFC3339Nano),
			ContentLength: len(msg0Content),
		},
		{
			SessionID:     sessionID,
			Ordinal:       1,
			Role:          "assistant",
			Content:       msg1Content,
			Timestamp:     t1.Format(time.RFC3339Nano),
			HasToolUse:    true,
			ContentLength: len(msg1Content),
			Model:         model,
			TokenUsage:    tokenUsage(1),
			ToolCalls: []db.ToolCall{
				{
					ToolName:  "Read",
					Category:  "Read",
					ToolUseID: readSoloID,
					InputJSON: `{"file_path":` +
						`"/src/auth/middleware.go"}`,
					ResultContentLength: 412,
				},
			},
		},
		{
			SessionID:     sessionID,
			Ordinal:       2,
			Role:          "user",
			Content:       msg2Content,
			Timestamp:     t2.Format(time.RFC3339Nano),
			ContentLength: len(msg2Content),
		},
		{
			SessionID:     sessionID,
			Ordinal:       3,
			Role:          "assistant",
			Content:       msg3Content,
			Timestamp:     t3.Format(time.RFC3339Nano),
			HasToolUse:    true,
			ContentLength: len(msg3Content),
			Model:         model,
			TokenUsage:    tokenUsage(2),
			ToolCalls: []db.ToolCall{
				{
					ToolName:  "Read",
					Category:  "Read",
					ToolUseID: readPar1ID,
					InputJSON: `{"file_path":` +
						`"/src/auth/session.go"}`,
					ResultContentLength: 510,
				},
				{
					ToolName:  "Read",
					Category:  "Read",
					ToolUseID: readPar2ID,
					InputJSON: `{"file_path":` +
						`"/src/auth/tokens.go"}`,
					ResultContentLength: 388,
				},
				{
					ToolName:          "Task",
					Category:          "Task",
					ToolUseID:         taskID,
					SubagentSessionID: subagentID,
					InputJSON: `{"description":` +
						`"audit session helpers",` +
						`"prompt":"Walk through ` +
						`session helpers and report ` +
						`anything that touches the DB ` +
						`on the hot path."}`,
					ResultContentLength: 1280,
				},
			},
		},
		{
			SessionID:     sessionID,
			Ordinal:       4,
			Role:          "user",
			Content:       msg4Content,
			Timestamp:     t4.Format(time.RFC3339Nano),
			ContentLength: len(msg4Content),
		},
		{
			SessionID:     sessionID,
			Ordinal:       5,
			Role:          "assistant",
			Content:       msg5Content,
			Timestamp:     t5.Format(time.RFC3339Nano),
			HasToolUse:    true,
			ContentLength: len(msg5Content),
			Model:         model,
			TokenUsage:    tokenUsage(3),
			ToolCalls: []db.ToolCall{
				{
					ToolName:  "Bash",
					Category:  "Bash",
					ToolUseID: bashSlowID,
					InputJSON: `{"command":` +
						`"go test ./... -count=10",` +
						`"description":"rerun ` +
						`auth tests"}`,
					ResultContentLength: 940,
				},
			},
		},
		{
			SessionID:     sessionID,
			Ordinal:       6,
			Role:          "user",
			Content:       msg6Content,
			Timestamp:     t6.Format(time.RFC3339Nano),
			ContentLength: len(msg6Content),
		},
	}
}

// buildDurationSubagentMessages builds a small but realistic
// sub-agent transcript: a Read followed by a Grep, then a
// final report. The exact gaps don't drive the parent timing
// UI (that uses the child session's start/end window), so we
// keep the messages evenly spaced for readability.
func buildDurationSubagentMessages(
	sessionID string, start time.Time,
) []db.Message {
	const model = "claude-sonnet-4-20250514"

	tokenUsage := func(seed int) json.RawMessage {
		input := 350 + seed*90
		output := 180 + seed*55
		cacheCr := 40 + seed*12
		cacheRd := 700 + seed*30
		return json.RawMessage(fmt.Sprintf(
			`{"input_tokens":%d,`+
				`"output_tokens":%d,`+
				`"cache_creation_input_tokens":%d,`+
				`"cache_read_input_tokens":%d}`,
			input, output, cacheCr, cacheRd,
		))
	}

	t0 := start
	t1 := start.Add(20 * time.Second)
	t2 := start.Add(40 * time.Second)
	t3 := start.Add(70 * time.Second)
	t4 := start.Add(95 * time.Second)
	t5 := start.Add(115 * time.Second)

	msg0 := "Audit the session helpers and report any " +
		"hot-path DB calls."
	msg1 := "Reading the helper module first."
	msg2 := "[tool_result]"
	msg3 := "Now scanning the cache layer for sync calls."
	msg4 := "[tool_result]"
	msg5 := "Two helpers issue a synchronous DB read on " +
		"every request. Report attached."

	return []db.Message{
		{
			SessionID:     sessionID,
			Ordinal:       0,
			Role:          "user",
			Content:       msg0,
			Timestamp:     t0.Format(time.RFC3339Nano),
			ContentLength: len(msg0),
		},
		{
			SessionID:     sessionID,
			Ordinal:       1,
			Role:          "assistant",
			Content:       msg1,
			Timestamp:     t1.Format(time.RFC3339Nano),
			HasToolUse:    true,
			ContentLength: len(msg1),
			Model:         model,
			TokenUsage:    tokenUsage(1),
			ToolCalls: []db.ToolCall{
				{
					ToolName:  "Read",
					Category:  "Read",
					ToolUseID: "tu_sub_read1",
					InputJSON: `{"file_path":` +
						`"/src/auth/helpers.go"}`,
					ResultContentLength: 320,
				},
			},
		},
		{
			SessionID:     sessionID,
			Ordinal:       2,
			Role:          "user",
			Content:       msg2,
			Timestamp:     t2.Format(time.RFC3339Nano),
			ContentLength: len(msg2),
		},
		{
			SessionID:     sessionID,
			Ordinal:       3,
			Role:          "assistant",
			Content:       msg3,
			Timestamp:     t3.Format(time.RFC3339Nano),
			HasToolUse:    true,
			ContentLength: len(msg3),
			Model:         model,
			TokenUsage:    tokenUsage(2),
			ToolCalls: []db.ToolCall{
				{
					ToolName:  "Grep",
					Category:  "Grep",
					ToolUseID: "tu_sub_grep1",
					InputJSON: `{"pattern":` +
						`"db.Query","path":` +
						`"/src/auth"}`,
					ResultContentLength: 210,
				},
			},
		},
		{
			SessionID:     sessionID,
			Ordinal:       4,
			Role:          "user",
			Content:       msg4,
			Timestamp:     t4.Format(time.RFC3339Nano),
			ContentLength: len(msg4),
		},
		{
			SessionID:     sessionID,
			Ordinal:       5,
			Role:          "assistant",
			Content:       msg5,
			Timestamp:     t5.Format(time.RFC3339Nano),
			ContentLength: len(msg5),
			Model:         model,
			TokenUsage:    tokenUsage(3),
		},
	}
}
