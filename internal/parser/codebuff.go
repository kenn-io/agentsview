// ABOUTME: Parses Codebuff/Freebuff chat-messages.json session files into
// ABOUTME: structured session data. Both agents share the same on-disk layout
// ABOUTME: under ~/.config/manicode/projects/<project>/chats/<timestamp>/.
// ABOUTME: The agent type (codebuff vs freebuff) is determined from the
// ABOUTME: agentType field in run-state.json, and agentType is also
// ABOUTME: surfaced as the session's UsageEvent.Model so the daily usage
// ABOUTME: report can bucket similar sessions by template while leaving the literal
// LLM (server-selected and not persisted on disk) unknown.
package parser

import (
	"context"
	"fmt"
	"math"
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
// and returns the parsed session with messages.
func parseCodebuffSession(
	dir string,
	projectHint string,
	machine string,
) (*ParsedSession, []ParsedMessage, error) {
	chatMessagesPath := filepath.Join(dir, codebuffPrimaryTranscriptName)
	runStatePath := filepath.Join(dir, codebuffRunStateName)
	chatMetaPath := filepath.Join(dir, codebuffChatMetaName)

	// Read run-state.json for model, token, agent-type, and skills data.
	rs, err := readCodebuffRunState(runStatePath)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, fmt.Errorf("read run-state %s: %w", runStatePath, err)
	}

	// Session ID is the timestamp directory name (ISO 8601).
	sessionID := filepath.Base(dir)
	sessionDate := parseCodebuffSessionDate(sessionID)

	// Read and parse the chat messages. The transcript is streamed element
	// by element: each AI message embeds the full project context in its
	// metadata.runState, so buffering the whole file (quadratic in
	// conversation length) held megabytes the parser never reads. A file cut
	// off mid-write yields the messages that survived plus a truncation
	// marker instead of dropping the session.
	//
	// context.Background() here is a known gap, not a guarantee: the
	// per-file parse callback has no ctx (WithFileParse would need widening,
	// out of scope for plan 021). decodeCodebuffMessages' cancellation rule
	// keeps that gap safe if a real context is threaded later.
	f, err := os.Open(chatMessagesPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read chat-messages %s: %w", chatMessagesPath, err)
	}
	transcript, err := decodeCodebuffMessages(context.Background(), f, sessionDate)
	closeErr := f.Close()
	if err != nil {
		return nil, nil, fmt.Errorf("parse chat-messages %s: %w", chatMessagesPath, err)
	}
	if closeErr != nil {
		return nil, nil, fmt.Errorf("close chat-messages %s: %w", chatMessagesPath, closeErr)
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

	// The sidecar is rewritten after the transcript upstream, so a crash
	// between the two writes (or an older CLI that skips the refresh)
	// legitimately leaves it stale. The transcript has been fully read and
	// stat'ed by now; validate the sidecar against that stat before any
	// consumer trusts it.
	meta := readCodebuffChatMeta(chatMetaPath, chatMessagesPath)

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

	// Use projectHint (the storage directory name) for the session ID
	// to ensure stability. The cwd-derived project name can change if
	// the git root changes, which would break source lookup and cause
	// session ID instability.
	projectID := projectHint
	if projectID == "" {
		projectID = "unknown"
	}
	fullID := string(agent) + ":" + projectID + ":" + sessionID

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
	runStateRaw := gjson.Parse("")
	if runStateData, err := os.ReadFile(filepath.Join(dir, codebuffRunStateName)); err == nil {
		runStateRaw = gjson.ParseBytes(runStateData)
	}
	sess.UsageEvents = codebuffUsageEvents(
		fullID, turnFacts, rs, runStateRaw, occurredAt,
	)

	// Termination classification uses the shared classifier with an empty
	// stop reason: the format carries no assistant stop-reason signal, so
	// the classifier can only report an unresolved tool call (the last
	// assistant turn left one dangling) or clean. It deliberately never
	// reports awaiting_user -- "parked waiting for you" is
	// indistinguishable from "finished" in this format, and claiming
	// otherwise would put a false waiting indicator on every session.
	// A transcript the decoder found cut off mid-write overrides both:
	// the truncation flag takes precedence in the classifier, and the
	// partial buffer committed by ForceReplace is exactly what the flag
	// describes.
	fileTruncated := transcript.Truncated
	sess.IsTruncated = fileTruncated
	sess.TerminationStatus = Classify(msgs, "", fileTruncated)

	return sess, msgs, nil
}

// codebuffRunState holds extracted fields from run-state.json.
type codebuffRunState struct {
	AgentType         string
	ContextTokenCount int
	CreditsUsed       float64
	Cwd               string
	GitBranch         string
	Skills            []codebuffSkill
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
// MessagesSize and MessagesMtimeMs are binding fields: they must match the
// transcript's current stat or the whole sidecar is treated as absent.
// MessagesSize also feeds the sidecar's own presence check, so it is kept
// alongside the derived values.
type codebuffChatMeta struct {
	MessageCount     int
	FirstPrompt      string
	MessagesSize     int64
	MessagesMtimeMs  float64
	hasMessageCount  bool
	hasFirstPrompt   bool
	hasMessagesSize  bool
	hasMessagesMtime bool
}

// trusted reports whether the sidecar may influence session naming or
// counts. It mirrors upstream readChatMeta (cli/src/utils/chat-meta.ts):
// the sidecar is usable only when it parses, carries all four required
// fields, and its recorded messagesSize and messagesMtimeMs equal the
// transcript file's current size and mtime in milliseconds. A stale
// sidecar is treated exactly like a missing one -- upstream's own comment
// explains the rule as binding the sidecar "to the exact messages file it
// summarizes" so callers fall back to the full parse instead of showing
// outdated data or hiding corruption. There is no retry or healing here:
// the sidecar carries no durable value the transcript cannot regenerate.
//
// The recorded mtime is compared at whole-millisecond precision: upstream
// writes Node's statSync().mtimeMs, which carries sub-millisecond
// fractions on APFS and ext4, while Go's UnixMilli() truncates. Flooring
// the stored float matches the two writers' common precision, so real
// sidecars still validate on those filesystems while a transcript
// rewritten in a later millisecond still fails the compare.
func (m codebuffChatMeta) trusted(info os.FileInfo) bool {
	if !m.hasMessageCount || !m.hasFirstPrompt ||
		!m.hasMessagesSize || !m.hasMessagesMtime {
		return false
	}
	return m.MessagesSize == info.Size() &&
		math.Floor(m.MessagesMtimeMs) == float64(info.ModTime().UnixMilli())
}

// readCodebuffChatMeta reads chat-meta.json beside the transcript at
// chatPath and returns it only when the trust rule passes. When the
// sidecar is missing, unparsable, incomplete, or stale, the zero value is
// returned and callers must treat the session as if no sidecar existed --
// notably, an empty transcript plus an untrusted sidecar yields no counts
// rather than counts invented from a summary that no longer describes the
// transcript.
func readCodebuffChatMeta(
	path string, chatPath string,
) codebuffChatMeta {
	data, err := os.ReadFile(path)
	if err != nil {
		return codebuffChatMeta{}
	}
	if !gjson.ValidBytes(data) {
		return codebuffChatMeta{}
	}
	var m codebuffChatMeta
	if v := gjson.GetBytes(data, "messageCount"); v.Exists() {
		m.MessageCount = int(v.Int())
		m.hasMessageCount = true
	}
	if v := gjson.GetBytes(data, "firstPrompt"); v.Exists() {
		m.FirstPrompt = v.Str
		m.hasFirstPrompt = true
	}
	if v := gjson.GetBytes(data, "messagesSize"); v.Exists() {
		m.MessagesSize = v.Int()
		m.hasMessagesSize = true
	}
	if v := gjson.GetBytes(data, "messagesMtimeMs"); v.Exists() {
		m.MessagesMtimeMs = v.Float()
		m.hasMessagesMtime = true
	}
	info, err := os.Stat(chatPath)
	if err != nil || !m.trusted(info) {
		return codebuffChatMeta{}
	}
	return m
}

// codebuffTurnModel resolves the model one billed turn ran, in this order:
//
//  1. metadata.runState.inference.model when inference.source == "byok" --
//     the only place a concrete model string is recorded (BYOK runs pin it
//     for resumed conversations; hosted runs carry {source:'codebuff'} and
//     no model);
//  2. the message runState's agentTemplates[agentType].model -- the
//     template definition the server executed for that turn;
//  3. the same two lookups against the standalone run-state.json, which
//     feeds the legacy fallback path;
//  4. the run-state agentType itself (a template id, what AgentsView has
//     always stored for Codebuff).
//
// contextTokenBaseline.model and contextTokenCount are deliberately not
// consulted: they anchor context occupancy, not the model the turn billed.
// An empty result means unresolvable, and callers must emit no event for
// that turn -- the usage report filters on a non-empty model, so an
// empty-model row would silently drop the cost everywhere.
func codebuffTurnModel(
	msgRunState gjson.Result, fileRunState gjson.Result, agentType string,
) string {
	for _, rs := range []gjson.Result{msgRunState, fileRunState} {
		if !rs.Exists() {
			continue
		}
		inference := rs.Get(
			"sessionState.mainAgentState.inference")
		if !inference.Exists() {
			inference = rs.Get("inference")
		}
		if inference.Get("source").Str == "byok" &&
			inference.Get("model").Str != "" {
			return inference.Get("model").Str
		}
		agentTypeHere := rs.Get(
			"sessionState.mainAgentState.agentType").Str
		if agentTypeHere == "" {
			agentTypeHere = rs.Get("agentType").Str
		}
		if model := rs.Get(
			"sessionState.fileContext.agentTemplates." + agentTypeHere + ".model",
		).Str; model != "" {
			return model
		}
		if model := rs.Get(
			"fileContext.agentTemplates." + agentTypeHere + ".model",
		).Str; model != "" {
			return model
		}
		if agentTypeHere != "" {
			return agentTypeHere
		}
	}
	return agentType
}

// codebuffTurnCost converts one message's credits number into a reported
// cost. Upstream writes a JSON number of credits; using the raw decimal
// text keeps the value's original representation out of float rounding.
// One credit is one cent ($0.01 = 10_000 microdollars), matching the
// money.ParseScaledDecimal shape the Grok parser uses for its own
// upstream-supplied cost. A negative value fails closed (ParseScaledDecimal
// accepts a sign), and any parse error means no event rather than a $0.00
// row. A nil result means "emit nothing for this turn".
func codebuffTurnCost(creditsRaw string) *money.Money {
	if creditsRaw == "" || strings.HasPrefix(creditsRaw, "-") {
		return nil
	}
	microdollars, err := money.ParseScaledDecimal(creditsRaw+"e-2", 6)
	if err != nil {
		return nil
	}
	cost := money.Money{Microdollars: microdollars}
	return &cost
}

// codebuffUsageEvents builds the session's reported-cost rows.
//
// Per-turn path: every AI message carrying a credits field greater than
// zero emits one event bound to that message's ordinal, so a multi-prompt
// session attributes each prompt's spend to the prompt that incurred it
// under the model that turn ran.
//
// Zero-credit turn: a present credits of exactly zero is a real, unbilled
// turn. It emits nothing but still counts as "the transcript carries
// credits", which is what suppresses the legacy fallback below.
//
// Partial final turn: if any message carried credits but the last AI
// message did not, no residual event is added from run-state's creditsUsed.
// Upstream resets that counter to zero at each prompt and writes it back
// only on completion, so an in-flight prompt leaves a partial total there;
// adding it as a separate row would double count spend the transcript
// already recorded. Deliberately dropping a present number is the one
// non-obvious call here: the alternative double counts.
//
// Legacy path: only when no message in the transcript carried a credits
// field at all, one event is emitted from run-state creditsUsed with the
// old dedup key, keeping pre-credits archives on their existing row count.
// When the model is unresolvable on either path, no event is emitted: the
// usage report filters on a non-empty model (ue.model != ” in every read
// path), so an empty-model row would lose the cost silently instead of
// surfacing it.
func codebuffUsageEvents(
	sessionID string,
	turns []codebuffTurnFact,
	rs codebuffRunState,
	fileRunState gjson.Result,
	fallbackOccurredAt time.Time,
) []ParsedUsageEvent {
	var events []ParsedUsageEvent

	carryCredits := false
	for _, turn := range turns {
		carryCredits = true
		if turn.Credits <= 0 {
			// Present zero: real unbilled turn, no row.
			continue
		}
		model := codebuffTurnModel(turn.RunState, fileRunState, rs.AgentType)
		if model == "" {
			continue
		}
		cost := codebuffTurnCost(turn.CreditsRaw)
		if cost == nil {
			continue
		}
		ordinal := turn.Ordinal
		events = append(events, ParsedUsageEvent{
			SessionID:      sessionID,
			MessageOrdinal: &ordinal,
			Source:         "session",
			Model:          model,
			OccurredAt:     turn.Timestamp.Format(time.RFC3339Nano),
			Cost:           cost,
			CostStatus:     "reported",
			CostSource:     "session",
			DedupKey:       "turn:" + sessionID + ":" + codebuffTurnKey(turn),
		})
	}

	if len(events) > 0 {
		sort.Slice(events, func(i, j int) bool {
			if events[i].OccurredAt != events[j].OccurredAt {
				return events[i].OccurredAt < events[j].OccurredAt
			}
			return events[i].DedupKey < events[j].DedupKey
		})
		return events
	}

	// Legacy fallback: no message carried a credits field (or none parsed),
	// so the run-state total is the only accounting signal. creditsUsed is
	// the last prompt's spend, but for pre-credits archives it is the only
	// number available and matches today's single-row behavior.
	if !carryCredits && rs.CreditsUsed > 0 {
		if model := codebuffTurnModel(gjson.Parse(""), fileRunState, rs.AgentType); model != "" {
			creditsRaw := strconv.FormatFloat(
				rs.CreditsUsed, 'f', -1, 64,
			)
			if cost := codebuffTurnCost(creditsRaw); cost != nil {
				events = append(events, ParsedUsageEvent{
					SessionID:  sessionID,
					Source:     "session",
					Model:      model,
					OccurredAt: fallbackOccurredAt.Format(time.RFC3339Nano),
					Cost:       cost,
					CostStatus: "reported",
					CostSource: "session",
					DedupKey:   "session:" + sessionID,
				})
			}
		}
	}
	return events
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

// codebuffTurnFact carries one AI message's billing facts out of the
// transcript walk. Upstream resets creditsUsed at every user prompt and
// stamps each completed AI message with that prompt's credits
// (cli/src/hooks/helpers/send-message.ts) plus its runState
// (metadata.runState, "RunState stored after completion"), so a completed
// AI message is the unit of billing. Absent, zero, and positive credits
// are three distinct states: gjson Exists() separates absent from 0, and
// zero is a real unbilled turn that must still suppress the legacy
// run-state fallback. RunState keeps the raw gjson result so the model
// resolution in parseCodebuffSession can dig inference/agentTemplates
// without the walk knowing their shapes.
type codebuffTurnFact struct {
	MessageID      string
	Ordinal        int
	Timestamp      time.Time
	CreditsPresent bool
	Credits        float64
	CreditsRaw     string
	RunState       gjson.Result
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
