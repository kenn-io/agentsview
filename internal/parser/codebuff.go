// ABOUTME: Parses Codebuff/Freebuff chat-messages.json session files into
// ABOUTME: structured session data. Both agents share the same on-disk layout
// ABOUTME: under ~/.config/manicode/projects/<project>/chats/<timestamp>/.
// ABOUTME: The agent type (codebuff vs freebuff) is determined from the
// ABOUTME: agentType field in run-state.json. Usage events carry the model a
// ABOUTME: turn ran when the run state records one, else the agent template.
package parser

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"go.kenn.io/agentsview/internal/money"
)

// codebuffSessionDir contains the session timestamp directory path and
// the project hint derived from the parent directory name.
type codebuffSessionDir struct {
	Path        string
	ProjectHint string
}

// parseCodebuffSession parses a single codebuff/freebuff session directory
// and returns the parsed session with messages, plus one linked child result
// per nested subagent (see codebuffSubagentResults).
func parseCodebuffSession(
	dir string,
	projectHint string,
	machine string,
) (*ParsedSession, []ParsedMessage, []ParseResult, error) {
	chatMessagesPath := filepath.Join(dir, codebuffPrimaryTranscriptName)
	runStatePath := filepath.Join(dir, codebuffRunStateName)
	chatMetaPath := filepath.Join(dir, codebuffChatMetaName)

	// Read run-state.json for model, token, agent-type, and skills data.
	rs, err := readCodebuffRunState(runStatePath)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, nil, fmt.Errorf("read run-state %s: %w", runStatePath, err)
	}

	// Session ID is the timestamp directory name (ISO 8601).
	sessionID := filepath.Base(dir)
	sessionDate := parseCodebuffSessionDate(sessionID)

	// Determine agent type from run-state agentType field.
	// Sessions with "free" in the agentType are Freebuff, others are Codebuff.
	// Both share the same on-disk layout; the parser splits them by type
	// so the UI can filter each agent independently.
	agent := AgentCodebuff
	agentLabel := "Codebuff"
	if strings.Contains(strings.ToLower(rs.AgentType), "free") {
		agent = AgentFreebuff
		agentLabel = "Freebuff"
	}

	// Use projectHint (the storage directory name) for the session ID
	// to ensure stability. The cwd-derived project name can change if
	// the git root changes, which would break source lookup and cause
	// session ID instability.
	projectID := projectHint
	if projectID == "" {
		projectID = "unknown"
	}
	fullID := string(agent) + ":" + projectID + ":" + sessionID

	// Stream the transcript: each AI message embeds the full project context
	// in metadata.runState, so the file grows quadratically with the
	// conversation.
	f, err := os.Open(chatMessagesPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read chat-messages %s: %w", chatMessagesPath, err)
	}
	transcript, err := decodeCodebuffMessages(f, sessionDate, fullID)
	closeErr := f.Close()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("parse chat-messages %s: %w", chatMessagesPath, err)
	}
	if closeErr != nil {
		return nil, nil, nil, fmt.Errorf("close chat-messages %s: %w", chatMessagesPath, closeErr)
	}
	msgs := transcript.Messages
	turnFacts := transcript.TurnFacts
	startedAt := transcript.StartedAt
	endedAt := transcript.EndedAt

	// Enrich tool calls with skill names by matching against the skills
	// catalog available to this session (run-state.json.fileContext.skills).
	// Codebuff/Freebuff invoke skills through generic tool calls (e.g.
	// run_terminal_command) rather than a dedicated Skill tool, so a tool
	// call is attributed to a skill when its name or input references a
	// known skill from the catalog.
	codebuffAttachSkillNames(msgs, rs.Skills)

	// Read chat-meta.json for session name and timing hints.
	meta := readCodebuffChatMeta(chatMetaPath)

	// Build session name from first user prompt.
	firstMsg := ""
	for _, msg := range msgs {
		if msg.Role == RoleUser && !msg.IsSystem &&
			strings.TrimSpace(msg.Content) != "" {
			firstMsg = truncate(
				strings.ReplaceAll(msg.Content, "\n", " "),
				300,
			)
			break
		}
	}
	if firstMsg == "" && meta.FirstPrompt != "" {
		firstMsg = truncate(
			strings.ReplaceAll(meta.FirstPrompt, "\n", " "),
			300,
		)
	}

	// Session name from first prompt (better than directory name).
	sessionName := firstMsg
	if len(sessionName) > 80 {
		sessionName = truncate(sessionName, 77)
	}
	if sessionName == "" {
		if rs.Cwd != "" {
			sessionName = filepath.Base(rs.Cwd)
		} else {
			sessionName = projectHint
		}
	}

	// Count user messages.
	userMsgCount := 0
	for _, msg := range msgs {
		if msg.Role == RoleUser && !msg.IsSystem &&
			strings.TrimSpace(msg.Content) != "" {
			userMsgCount++
		}
	}
	messageCount := len(msgs)

	// If no messages from the transcript, use meta counts.
	if messageCount == 0 {
		messageCount = meta.MessageCount
		if meta.MessageCount > 0 {
			userMsgCount = 1 // at least one user prompt
		}
	}

	// Mark meta-derived counts authoritative. When the on-disk transcript is
	// empty but chat-meta.json reports a count, the meta totals are the
	// parser's only source for the session's user-visible counts. Without
	// this flag the sync engine's applySessionTokenTotalsFromMessages pass
	// would recompute counts from the empty parsed-message slice and
	// overwrite the meta totals with zero, hiding the session from any UI
	// that filters on nonzero counts. Set the flag only in the fallback case
	// (not when the transcript already provided counts) so the sync engine
	// keeps reconciling message-derived counts for sessions with real
	// transcripts.
	countsAuthoritative := len(msgs) == 0 && meta.MessageCount > 0

	// Source file identity: use chat-messages.json as the primary source.
	info, err := os.Stat(chatMessagesPath)
	fileInfo := FileInfo{
		Path: chatMessagesPath,
	}
	if err == nil {
		fileInfo.Size = info.Size()
		fileInfo.Mtime = info.ModTime().UnixNano()
	}

	// Derive display project from run-state cwd for UI display.
	// Use ExtractProjectFromCwd (git-root aware) rather than
	// GetProjectName because rs.Cwd is a full absolute path, not
	// a Claude-style encoded project name.
	project := projectHint
	if rs.Cwd != "" {
		if p := ExtractProjectFromCwd(rs.Cwd); p != "" {
			project = p
		}
	}

	// Fall back StartedAt/EndedAt for sessions whose transcript carries
	// no parseable message timestamps (empty chat-messages.json, or
	// messages without timestamp fields). Analytics and sorting need a
	// real timestamp; fall back to the session directory date, then the
	// source mtime as a last resort.
	if startedAt.IsZero() && !sessionDate.IsZero() {
		startedAt = sessionDate
	}
	if startedAt.IsZero() && fileInfo.Mtime > 0 {
		startedAt = time.Unix(0, fileInfo.Mtime)
	}
	if endedAt.IsZero() && !startedAt.IsZero() {
		endedAt = startedAt
	}

	sess := &ParsedSession{
		ID:                  fullID,
		Project:             project,
		Machine:             machine,
		Agent:               agent,
		AgentLabel:          agentLabel,
		Cwd:                 rs.Cwd,
		GitBranch:           rs.GitBranch,
		FirstMessage:        firstMsg,
		SessionName:         sessionName,
		StartedAt:           startedAt,
		EndedAt:             endedAt,
		MessageCount:        messageCount,
		UserMessageCount:    userMsgCount,
		CountsAuthoritative: countsAuthoritative,
		SourceSessionID:     sessionID,
		SourceVersion:       "codebuff-chat-v1",
		File:                fileInfo,
	}

	// contextTokenCount from run-state.json is the final per-step context
	// count, not the peak. Compaction can make the final value lower than
	// the true peak, so we cannot reliably derive PeakContextTokens from
	// this value. Leave peak context unavailable.

	// Determine occurred_at: prefer message timestamps, then fall
	// back to the session directory timestamp, then source mtime.
	occurredAt := startedAt
	if !endedAt.IsZero() {
		occurredAt = endedAt
	}
	if occurredAt.IsZero() && !sessionDate.IsZero() {
		occurredAt = sessionDate
	}
	if occurredAt.IsZero() && fileInfo.Mtime > 0 {
		occurredAt = time.Unix(0, fileInfo.Mtime)
	}
	sess.UsageEvents = codebuffUsageEvents(fullID, turnFacts, rs, occurredAt)

	// The format records no stop reason, so the classifier can only report
	// an unresolved final tool call or a clean ending.
	sess.TerminationStatus = Classify(msgs, "", false)

	children := codebuffSubagentResults(sess, transcript.Subagents, rs.Skills)
	return sess, msgs, children, nil
}

// codebuffSubagentResults turns the decoder's nested subagents into linked
// child sessions. Each child inherits the parent's project, machine, agent,
// working directory, branch, and source file (one chat-messages.json holds
// the whole tree, as a Claude transcript holds its forks); ParentSessionID
// names the session whose transcript held the agent block. Usage events stay
// on the parent: upstream bills credits per top-level AI message, not per
// subagent.
func codebuffSubagentResults(
	parent *ParsedSession, subs []codebuffSubagent, skills []codebuffSkill,
) []ParseResult {
	if len(subs) == 0 {
		return nil
	}
	out := make([]ParseResult, 0, len(subs))
	for _, sub := range subs {
		msgs := sub.Messages
		codebuffAttachSkillNames(msgs, skills)

		firstMsg := ""
		userCount := 0
		for _, msg := range msgs {
			if msg.Role == RoleUser && !msg.IsSystem &&
				strings.TrimSpace(msg.Content) != "" {
				userCount++
				if firstMsg == "" {
					firstMsg = truncate(
						strings.ReplaceAll(msg.Content, "\n", " "), 300,
					)
				}
			}
		}
		name := sub.AgentName
		if name == "" {
			name = sub.AgentType
		}
		if name == "" {
			name = "subagent"
		}
		startedAt := sub.Timestamp
		if startedAt.IsZero() {
			startedAt = parent.StartedAt
		}
		out = append(out, ParseResult{
			Session: ParsedSession{
				ID:                sub.ID,
				Project:           parent.Project,
				Machine:           parent.Machine,
				Agent:             parent.Agent,
				AgentLabel:        parent.AgentLabel,
				Cwd:               parent.Cwd,
				GitBranch:         parent.GitBranch,
				ParentSessionID:   sub.ParentID,
				RelationshipType:  RelSubagent,
				FirstMessage:      firstMsg,
				SessionName:       "Subagent: " + name,
				StartedAt:         startedAt,
				EndedAt:           startedAt,
				MessageCount:      len(msgs),
				UserMessageCount:  userCount,
				SourceSessionID:   sub.AgentID,
				SourceVersion:     parent.SourceVersion,
				File:              parent.File,
				TerminationStatus: Classify(msgs, "", false),
			},
			Messages: msgs,
		})
	}
	return out
}

// codebuffRunState holds extracted fields from run-state.json.
type codebuffRunState struct {
	AgentType         string
	ContextTokenCount int
	CreditsUsed       float64
	Cwd               string
	GitBranch         string
	Skills            []codebuffSkill
	// Wire is the typed view used for model resolution; nil when the file
	// is absent or does not decode.
	Wire *codebuffWireRunState
}

// codebuffSkill is a single skill entry from the session's skill catalog
// (run-state.json sessionState.fileContext.skills). The catalog lists the
// skills available to the agent during the session.
type codebuffSkill struct {
	Name        string
	Description string
	FilePath    string
	Content     string
}

func readCodebuffRunState(path string) (codebuffRunState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return codebuffRunState{}, err
	}
	if !gjson.ValidBytes(data) {
		return codebuffRunState{}, fmt.Errorf("invalid json in %s", path)
	}

	mas := gjson.GetBytes(data, "sessionState.mainAgentState")
	rs := codebuffRunState{
		AgentType:         mas.Get("agentType").Str,
		ContextTokenCount: int(mas.Get("contextTokenCount").Int()),
		CreditsUsed:       mas.Get("creditsUsed").Float(),
		Cwd: gjson.GetBytes(data,
			"sessionState.fileContext.cwd").Str,
		// branch is optional upstream (ProjectFileContext.gitChanges) and
		// absent when git is unavailable or the project is not a
		// repository; leave it empty there rather than substituting the
		// project name.
		GitBranch: gjson.GetBytes(data,
			"sessionState.fileContext.gitChanges.branch").Str,
	}
	rs.Skills = parseCodebuffSkills(data)
	var wire codebuffWireRunState
	if err := json.Unmarshal(data, &wire); err == nil {
		rs.Wire = &wire
	}
	return rs, nil
}

// parseCodebuffSkills extracts the skill catalog from run-state.json
// (sessionState.fileContext.skills). The field is a JSON object keyed by
// skill name; each value carries name, description, optional content, and
// filePath. Returns an empty slice when no skills are present.
func parseCodebuffSkills(data []byte) []codebuffSkill {
	skills := gjson.GetBytes(data, "sessionState.fileContext.skills")
	if !skills.Exists() || !skills.IsObject() {
		return nil
	}
	var out []codebuffSkill
	skills.ForEach(func(key, val gjson.Result) bool {
		name := val.Get("name").Str
		if name == "" {
			name = key.Str
		}
		out = append(out, codebuffSkill{
			Name:        name,
			Description: val.Get("description").Str,
			FilePath:    val.Get("filePath").Str,
			Content:     val.Get("content").Str,
		})
		return true
	})
	return out
}

// codebuffAttachSkillNames attributes tool calls to skills from the
// session's skill catalog. Codebuff/Freebuff do not emit a dedicated Skill
// tool; skills are invoked through generic tools (e.g. run_terminal_command)
// whose input names the skill, or through a tool literally named "Skill".
// A tool call is attributed when its tool name matches a skill, or its input
// JSON references a known skill name.
func codebuffAttachSkillNames(msgs []ParsedMessage, skills []codebuffSkill) {
	if len(skills) == 0 {
		return
	}
	// byName maps the lowercased skill name to the catalog's canonical
	// casing so attribution can match case-insensitively while always
	// reporting the catalog spelling.
	byName := make(map[string]string, len(skills))
	for _, s := range skills {
		byName[strings.ToLower(s.Name)] = s.Name
	}
	for i := range msgs {
		for j := range msgs[i].ToolCalls {
			tc := &msgs[i].ToolCalls[j]
			if tc.SkillName != "" {
				continue
			}
			// Explicit Skill tool.
			if strings.EqualFold(tc.ToolName, "Skill") ||
				strings.EqualFold(tc.ToolName, "skill") {
				tc.SkillName = gjson.Get(tc.InputJSON, "skill").Str
				if tc.SkillName == "" {
					tc.SkillName = gjson.Get(tc.InputJSON, "name").Str
				}
				if tc.SkillName == "" {
					tc.SkillName = tc.ToolName
				}
				continue
			}
			// Tool name itself is a skill name.
			if _, ok := byName[strings.ToLower(tc.ToolName)]; ok {
				tc.SkillName = tc.ToolName
				continue
			}
			// Input JSON references a known skill name.
			if name := codebuffSkillNameFromInput(tc.InputJSON, byName); name != "" {
				tc.SkillName = name
			}
		}
	}
}

// codebuffSkillNameFromInput scans raw tool input JSON for a reference to a
// known skill name. It matches the skill name as a quoted JSON string value
// or as a standalone token (e.g. inside a shell command). byName maps the
// lowercased skill name to its canonical catalog casing; matches always
// return the canonical casing. When multiple catalog skills appear as
// tokens, the winner is deterministic: the first in lowercase-sorted
// order. Returns "" when no known skill is referenced.
func codebuffSkillNameFromInput(inputJSON string, byName map[string]string) string {
	if inputJSON == "" {
		return ""
	}
	// Direct JSON string/object match on common skill-carrying keys.
	for _, key := range []string{"skill", "name", "skill_name", "command", "prompt"} {
		v := gjson.Get(inputJSON, key).Str
		if v != "" {
			if canonical, ok := byName[strings.ToLower(v)]; ok {
				return canonical
			}
		}
	}
	// Fall back to scanning for any known skill name as a whole token.
	// Sort the candidate names so the winner is deterministic when two
	// catalog skills both appear in the input.
	lower := strings.ToLower(inputJSON)
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if containsSkillToken(lower, name) {
			return byName[name]
		}
	}
	return ""
}

// containsSkillToken reports whether lower contains name as a whole
// alphanumeric token (word-boundary match). It splits lower on
// non-alphanumeric runes and compares each token against name.
// This avoids false positives from substring matching (e.g. "go"
// matching "going" or "cargo").
func containsSkillToken(lower, name string) bool {
	if name == "" {
		return false
	}
	var buf []byte
	for i := range len(lower) {
		c := lower[i]
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			buf = append(buf, c)
		} else {
			if string(buf) == name {
				return true
			}
			buf = buf[:0]
		}
	}
	return string(buf) == name
}

// codebuffChatMeta holds extracted fields from chat-meta.json.
type codebuffChatMeta struct {
	MessageCount int
	FirstPrompt  string
	MessagesSize int64
}

func readCodebuffChatMeta(path string) codebuffChatMeta {
	data, err := os.ReadFile(path)
	if err != nil {
		return codebuffChatMeta{}
	}
	if !gjson.ValidBytes(data) {
		return codebuffChatMeta{}
	}
	return codebuffChatMeta{
		MessageCount: int(gjson.GetBytes(data, "messageCount").Int()),
		FirstPrompt:  gjson.GetBytes(data, "firstPrompt").Str,
		MessagesSize: gjson.GetBytes(data, "messagesSize").Int(),
	}
}

// codebuffTurnModel resolves the model a billed turn ran. Each run state is
// checked in turn (the message's metadata.runState, then run-state.json) for
// a BYOK inference.model, then agentTemplates[agentType].model. When neither
// names a model, the first agentType seen (a template id) is used.
func codebuffTurnModel(runStates ...*codebuffWireRunState) string {
	fallback := ""
	for _, rs := range runStates {
		if rs == nil {
			continue
		}
		if inf := rs.Inference; inf != nil && inf.Source == "byok" && inf.Model != "" {
			return inf.Model
		}
		ss := rs.SessionState
		if ss == nil || ss.MainAgentState == nil || ss.MainAgentState.AgentType == "" {
			continue
		}
		agentType := ss.MainAgentState.AgentType
		if ss.FileContext != nil {
			if model := ss.FileContext.AgentTemplates[agentType].Model; model != "" {
				return model
			}
		}
		if fallback == "" {
			fallback = agentType
		}
	}
	return fallback
}

// codebuffTurnCost converts a raw JSON credits number into a reported cost
// (one credit is $0.01). It returns nil for anything that is not a positive
// number, including JSON strings and null.
func codebuffTurnCost(creditsRaw string) *money.Money {
	if creditsRaw == "" || strings.HasPrefix(creditsRaw, "-") {
		return nil
	}
	microdollars, err := money.ParseScaledDecimal(creditsRaw+"e-2", 6)
	if err != nil || microdollars <= 0 {
		return nil
	}
	cost := money.Money{Microdollars: microdollars}
	return &cost
}

// codebuffUsageEvents builds the session's reported-cost rows. Each AI
// message with positive credits emits one row bound to that message. Only
// when no AI message carries a credits field at all (older CLIs) is one row
// emitted from run-state.json's creditsUsed. That field holds only the last
// prompt's spend, so it is never added on top of per-message rows. A turn
// whose model cannot be resolved emits nothing, since usage reports drop
// empty-model rows.
func codebuffUsageEvents(
	sessionID string,
	turns []codebuffTurnFact,
	rs codebuffRunState,
	fallbackOccurredAt time.Time,
) []ParsedUsageEvent {
	var events []ParsedUsageEvent
	for _, turn := range turns {
		cost := codebuffTurnCost(turn.CreditsRaw)
		if cost == nil {
			continue
		}
		model := codebuffTurnModel(turn.RunState, rs.Wire)
		if model == "" {
			continue
		}
		occurredAt := turn.Timestamp
		if occurredAt.IsZero() {
			occurredAt = fallbackOccurredAt
		}
		ordinal := turn.Ordinal
		events = append(events, ParsedUsageEvent{
			SessionID:      sessionID,
			MessageOrdinal: &ordinal,
			Source:         "session",
			Model:          model,
			OccurredAt:     occurredAt.Format(time.RFC3339Nano),
			Cost:           cost,
			CostStatus:     "reported",
			CostSource:     "session",
			DedupKey:       "turn:" + sessionID + ":" + codebuffTurnKey(turn),
		})
	}
	if len(turns) > 0 || rs.CreditsUsed <= 0 {
		return events
	}

	model := codebuffTurnModel(rs.Wire)
	cost := codebuffTurnCost(strconv.FormatFloat(rs.CreditsUsed, 'f', -1, 64))
	if model == "" || cost == nil {
		return nil
	}
	return []ParsedUsageEvent{{
		SessionID:  sessionID,
		Source:     "session",
		Model:      model,
		OccurredAt: fallbackOccurredAt.Format(time.RFC3339Nano),
		Cost:       cost,
		CostStatus: "reported",
		CostSource: "session",
		DedupKey:   "session:" + sessionID,
	}}
}

// codebuffTurnKey derives the stable per-turn identity for the dedup key.
// The message's own id is preferred because it is stable across reparses;
// the ordinal is the fallback for messages that never carried an id.
func codebuffTurnKey(turn codebuffTurnFact) string {
	if turn.MessageID != "" {
		return turn.MessageID
	}
	return strconv.Itoa(turn.Ordinal)
}

// IsCodebuffTimestamp reports whether s matches one of the
// on-disk session-directory timestamp shapes that parseCodebuffSession
// treats as the bare session ID suffix of the canonical
// "agent:<project>:<ts>" id. Used by the session-get resolver to
// distinguish a Codebuff/Freebuff timestamp from a bare UUID
// (Codex, Copilot, Gemini, ...) that the generic prefix resolver
// handles. Returns true when s parses as any of the four ISO-8601
// forms accepted by parseCodebuffSessionDate:
//
//   - "2026-07-16T00-09-00.236Z"   (full ISO with millis and Z)
//   - "2026-07-16T00-09-00Z"       (full ISO without millis)
//   - "2026-07-16T00-09-00.123"    (full ISO without Z)
//   - "2026-07-16"                 (basic ISO date only)
//
// Everything else (UUIDs, numeric Unix epochs, free-form strings)
// returns false so the generic resolver path stays open. The
// predicate is purely syntactic — no FS walk — so it is cheap on
// --server and --pg transports where a FS scan is wasted work.
func IsCodebuffTimestamp(s string) bool {
	return !parseCodebuffSessionDate(s).IsZero()
}

// parseCodebuffSessionDate parses the session directory name as an ISO 8601
// timestamp. The directory name format is "2026-07-16T00-09-00.236Z".
// The returned time is always in the local timezone so that time-only
// message timestamps (HH:MM PM) combine correctly with the date.
func parseCodebuffSessionDate(sessionID string) time.Time {
	// Try full ISO format with milliseconds and Z suffix.
	if ts, err := time.Parse("2006-01-02T15-04-05.999Z", sessionID); err == nil {
		return ts.In(time.Local) //nolint:forbidigo // Codebuff combines local wall-clock message times with the session date.
	}
	// Try without milliseconds.
	if ts, err := time.Parse("2006-01-02T15-04-05Z", sessionID); err == nil {
		return ts.In(time.Local) //nolint:forbidigo // Codebuff combines local wall-clock message times with the session date.
	}
	// Try with milliseconds, no Z. Interpret as local time since
	// codebuff records wall-clock timestamps without a UTC offset.
	if ts, err := time.ParseInLocation("2006-01-02T15-04-05.999", sessionID, time.Local); err == nil { //nolint:forbidigo // Codebuff combines local wall-clock message times with the session date.
		return ts
	}
	// Try basic ISO date only.
	if ts, err := time.ParseInLocation("2006-01-02", sessionID, time.Local); err == nil { //nolint:forbidigo // Codebuff combines local wall-clock message times with the session date.
		return ts
	}
	return time.Time{}
}

// codebuffTurnFact is one AI message that carried a credits field. Upstream
// stamps each completed AI message with its prompt's credits and a
// metadata.runState snapshot (cli/src/hooks/helpers/send-message.ts).
type codebuffTurnFact struct {
	MessageID  string
	Ordinal    int
	Timestamp  time.Time
	CreditsRaw string
	RunState   *codebuffWireRunState
}

// parseCodebuffTimestamp parses a timestamp string. Codebuff/freebuff
// messages use "HH:MM PM" format with the date provided by the session
// directory name. The sessionDate carries the date context.
func parseCodebuffTimestamp(s string, sessionDate time.Time) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}

	// ISO format (used by newer builds or subagent messages).
	if ts, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return ts
	}
	if ts, err := time.Parse("2006-01-02T15:04:05.999Z07:00", s); err == nil {
		return ts
	}

	// "HH:MM PM" format: combine with the session date.
	if ts, err := time.Parse("03:04 PM", s); err == nil {
		if sessionDate.IsZero() {
			return time.Time{}
		}
		// Combine date from session with time from message.
		return time.Date(
			sessionDate.Year(),
			sessionDate.Month(),
			sessionDate.Day(),
			ts.Hour(),
			ts.Minute(),
			0, 0,
			sessionDate.Location(),
		)
	}

	return time.Time{}
}

// discoverCodebuffSessions finds all session directories under a root.
// root is the parent projects directory (~/.config/manicode/projects).
// Sessions live under <root>/<project>/chats/<timestamp>/.
func discoverCodebuffSessions(root string) []codebuffSessionDir {
	var dirs []codebuffSessionDir
	_ = codebuffDiscoverEach(context.Background(), root, func(match singleFileMatch) error {
		dirs = append(dirs, codebuffSessionDir{
			Path:        filepath.Dir(match.Path),
			ProjectHint: match.ProjectHint,
		})
		return nil
	})
	return dirs
}

// codebuffDiscoverEach streams session discoveries, yielding each match
// as it is found. This avoids materializing the entire archive in memory.
func codebuffDiscoverEach(
	ctx context.Context, root string, yield func(singleFileMatch) error,
) error {
	// Stream project directories.
	return streamDirectoryEntries(ctx, root, func(projectEntry os.DirEntry) error {
		if !projectEntry.IsDir() {
			return nil
		}
		projectName := projectEntry.Name()
		chatsDir := filepath.Join(root, projectName, "chats")
		// Stream session directories within each project.
		return streamDirectoryEntries(ctx, chatsDir, func(sessionEntry os.DirEntry) error {
			if !sessionEntry.IsDir() {
				return nil
			}
			dir := filepath.Join(chatsDir, sessionEntry.Name())
			chatPath := filepath.Join(dir, codebuffPrimaryTranscriptName)
			if !IsRegularFile(chatPath) {
				return nil
			}
			return yield(singleFileMatch{
				Path:        chatPath,
				ProjectHint: projectName,
			})
		})
	})
}

// codebuffProjectFromPath extracts the project name from a session
// file path. The path is rooted under ~/.config/manicode/projects/.
func codebuffProjectFromPath(path string) string {
	// Path is: <root>/<project>/chats/<timestamp>/chat-messages.json
	// We want the <project> component.
	// Walk up from chat-messages.json: dir=/timestamp, parent=chats, grandparent=<project>
	dir := filepath.Dir(path)            // <timestamp>
	chatsDir := filepath.Dir(dir)        // chats
	projectDir := filepath.Dir(chatsDir) // <project>
	return filepath.Base(projectDir)
}
