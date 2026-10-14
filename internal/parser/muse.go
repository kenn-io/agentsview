package parser

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// Muse Code (Meta's terminal coding agent) writes each session as an
// append-only event log at <root>/YYYY/MM/DD/<session-id>/session.jsonl.
// Subagent runs log beside their parent at
// <session-dir>/subagent/<child-id>/session.jsonl. Only one level of
// subagent has been observed in real logs; deeper nesting is not handled
// beyond discovery. The log records runtime events rather than chat turns, so this file
// folds the events back into the conversation the agent saw: user prompts,
// assistant replies grouped by model response, tool calls, tool results, and
// per-response token usage.

const (
	museSessionPrefix   = "muse:"
	museLogName         = "session.jsonl"
	museSubagentDirName = "subagent"
	museToolFailPrefix  = "tool failed: "
	// museWorkspaceScanLimit bounds the records read from a parent log
	// when a subagent log does not name its own workspace. The workspace
	// facts are written when the session opens, so they sit near the top.
	museWorkspaceScanLimit = 256
)

// museRecord is one event envelope from a Muse log. Envelopes may arrive
// directly on a line or inside a retained_frame's children.
type museRecord struct {
	value       gjson.Result
	streamKind  string
	streamID    string
	payloadType string
	recordedAt  time.Time
}

// museLog is the decoded content of one session.jsonl file.
type museLog struct {
	records        []museRecord
	malformedLines int
}

// readMuseLog decodes every envelope in a Muse log. Malformed lines,
// including the partial last line of a session still being written, are
// counted and skipped. Retained markers stand in for omitted live-only
// records and carry no content.
func readMuseLog(ctx context.Context, path string, limit int) (museLog, error) {
	f, err := os.Open(path)
	if err != nil {
		return museLog{}, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	var log museLog
	lr := newLineReaderContext(ctx, f, maxLineSize)
	defer releaseLineReader(lr)
	for {
		line, ok := lr.next()
		if !ok {
			break
		}
		if limit > 0 && len(log.records) >= limit {
			break
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !gjson.Valid(line) {
			log.malformedLines++
			continue
		}
		value := gjson.Parse(line)
		switch {
		case value.Get("retained_marker").Exists():
			continue
		case value.Get("retained_frame").Exists():
			value.Get("children").ForEach(func(_, child gjson.Result) bool {
				raw := child.Get("record_json").Str
				if !gjson.Valid(raw) {
					log.malformedLines++
					return true
				}
				log.records = append(log.records, newMuseRecord(gjson.Parse(raw)))
				return true
			})
		default:
			log.records = append(log.records, newMuseRecord(value))
		}
	}
	if err := lr.Err(); err != nil {
		return museLog{}, fmt.Errorf("read %s: %w", path, err)
	}
	if lr.skippedOversized {
		log.malformedLines++
	}
	return log, ctx.Err()
}

func newMuseRecord(value gjson.Result) museRecord {
	record := museRecord{
		value:       value,
		streamKind:  value.Get("stream.kind").Str,
		streamID:    value.Get("stream.id").Str,
		payloadType: value.Get("payload_type").Str,
	}
	// recorded_at is microseconds since the Unix epoch.
	if us := value.Get("recorded_at"); us.Type == gjson.Number && us.Int() > 0 {
		record.recordedAt = time.UnixMicro(us.Int()).UTC()
	}
	return record
}

// museSessionIDFromLogPath returns the raw session ID for a log path: the
// name of the directory holding session.jsonl.
func museSessionIDFromLogPath(path string) string {
	if filepath.Base(path) != museLogName {
		return ""
	}
	return filepath.Base(filepath.Dir(path))
}

// museParentLogPath returns the parent session's log for a subagent log,
// or "" when path is a top-level session.
func museParentLogPath(path string) string {
	sessionDir := filepath.Dir(path)
	subagentDir := filepath.Dir(sessionDir)
	if filepath.Base(subagentDir) != museSubagentDirName {
		return ""
	}
	return filepath.Join(filepath.Dir(subagentDir), museLogName)
}

// museFacts holds the session-level facts collected from a log.
type museFacts struct {
	cwd         string
	gitBranch   string
	version     string
	sessionName string
	startedAt   time.Time
	endedAt     time.Time
}

func (f *museFacts) observe(record museRecord) {
	payload := record.value.Get("payload")
	switch record.payloadType {
	case "runtime.session.metadata":
		meta := payload.Get("record")
		if f.cwd == "" {
			f.cwd = meta.Get("workspace_root").Str
		}
		if f.cwd == "" {
			f.cwd = payload.Get("workspace_root").Str
		}
		if semver := meta.Get("build.semver").Str; semver != "" {
			f.version = semver
		}
	case "runtime.session.route_facts":
		if f.cwd == "" {
			f.cwd = payload.Get("record.cwd").Str
		}
	case "session.workspace_branch.observed":
		branch := payload.Get("record")
		if f.cwd == "" {
			f.cwd = branch.Get("workspace_root").Str
		}
		if branch.Get("reference.kind").Str == "branch" {
			f.gitBranch = branch.Get("reference.name").Str
		}
	case "session.name.changed":
		// Muse assigns every session a generated handle such as
		// "quiet-lyra". Only a name someone chose describes the work, so
		// automatic names are left for the first prompt to stand in for.
		if payload.Get("source").Str != "automatic" {
			if name := payload.Get("new_name").Str; name != "" {
				f.sessionName = name
			}
		}
	}
	if !record.recordedAt.IsZero() {
		if f.startedAt.IsZero() || record.recordedAt.Before(f.startedAt) {
			f.startedAt = record.recordedAt
		}
		if record.recordedAt.After(f.endedAt) {
			f.endedAt = record.recordedAt
		}
	}
}

// museUsage is one model_completed usage record.
type museUsage struct {
	model      string
	input      int
	output     int
	cacheRead  int
	cacheWrite int
	reasoning  int
	at         time.Time
	sequence   int64
}

func newMuseUsage(record museRecord, event gjson.Result) (museUsage, bool) {
	usage := event.Get("usage")
	if !usage.IsObject() {
		return museUsage{}, false
	}
	u := museUsage{
		model:      event.Get("model").Str,
		input:      int(usage.Get("input_tokens").Int()),
		output:     int(usage.Get("output_tokens").Int()),
		cacheWrite: int(usage.Get("cache_write_tokens").Int()),
		reasoning:  int(usage.Get("reasoning_tokens").Int()),
		at:         record.recordedAt,
		sequence:   record.value.Get("sequence").Int(),
	}
	if read := usage.Get("cache_read_tokens"); read.Exists() {
		u.cacheRead = int(read.Int())
	} else {
		u.cacheRead = int(usage.Get("cached_tokens").Int())
	}
	return u, true
}

// uncachedInput returns the input tokens billed at the full input rate.
// Muse reports cache reads and writes as subsets of input_tokens, so they
// are removed here and billed through their own buckets.
func (u museUsage) uncachedInput() int {
	return max(u.input-u.cacheRead-u.cacheWrite, 0)
}

func (u museUsage) applyTo(pm *ParsedMessage) {
	normalized := map[string]int{
		"input_tokens":                u.uncachedInput(),
		"output_tokens":               u.output,
		"cache_read_input_tokens":     u.cacheRead,
		"cache_creation_input_tokens": u.cacheWrite,
	}
	// output_tokens already includes reasoning: every real record seen
	// has reasoning_tokens <= output_tokens.
	if u.reasoning > 0 {
		normalized["reasoning_tokens"] = u.reasoning
	}
	encoded, err := json.Marshal(normalized, json.Deterministic(true))
	if err != nil {
		return
	}
	pm.TokenUsage = encoded
	pm.OutputTokens = u.output
	pm.HasOutputTokens = true
	pm.ContextTokens = u.input
	pm.HasContextTokens = true
	if u.model != "" {
		pm.Model = u.model
	}
}

// museAssistant collects the pieces of one model response before they
// are rendered into a ParsedMessage.
type museAssistant struct {
	thinking []string
	text     []string
	calls    []ParsedToolCall
	rendered []string
	usage    *museUsage
}

// museFold replays a session's run events into messages.
type museFold struct {
	messages []ParsedMessage
	// assistants maps an assistant message's index in messages to the
	// pieces gathered for it.
	assistants map[int]*museAssistant
	// byResponse maps a model response ID to its assistant message index.
	byResponse map[string]int
	// openResponse is the response a model_completed record belongs to.
	// Muse logs model_response_created before the response's output and
	// usage, and the usage record itself carries no response ID.
	openResponse string
	// pending holds usage for responses whose first visible output has not
	// been logged yet. A response that only calls tools logs its usage
	// before its tool calls, so a live log read between the two leaves the
	// usage here until a later parse sees the calls.
	pending map[string]museUsage
	// unbound holds usage that cannot be tied to one assistant message.
	// Usage with no open response, or a second usage record for one
	// response, has never appeared in a real Muse log: every
	// model_completed seen follows its model_response_created. This is a
	// fallback that keeps such usage priced, not a supported shape, so do
	// not add coverage for it.
	unbound []museUsage
	model   string
}

func newMuseFold() *museFold {
	return &museFold{
		assistants: make(map[int]*museAssistant),
		byResponse: make(map[string]int),
		pending:    make(map[string]museUsage),
	}
}

func (f *museFold) observe(record museRecord) {
	payload := record.value.Get("payload")
	switch record.payloadType {
	case "runtime.session.metadata":
		// The session's starting model, until a run configures its own.
		if model := payload.Get("record.model_id").Str; model != "" && f.model == "" {
			f.model = model
		}
		return
	case "run.model.configured":
		if model := payload.Get("record.model_id").Str; model != "" {
			f.model = model
		}
		return
	case "runtime.model_reconfigure.completed":
		if model := payload.Get("record.effective.model_id").Str; model != "" {
			f.model = model
		}
		return
	case "runtime.session":
	default:
		return
	}
	// Task events share the started kind with user turns but live under
	// payload.kind "task" and carry no prompt.
	if payload.Get("kind").Str != "run" {
		return
	}
	event := payload.Get("event")
	at := record.recordedAt
	switch event.Get("kind").Str {
	case "started":
		if prompt := event.Get("prompt").Str; prompt != "" {
			f.messages = append(f.messages, ParsedMessage{
				Ordinal:       len(f.messages),
				Role:          RoleUser,
				Content:       prompt,
				Timestamp:     at,
				ContentLength: len(prompt),
				SourceType:    "user",
				SourceUUID:    record.value.Get("id").Str,
			})
		}
	case "model_response_created":
		f.openResponse = event.Get("response_id").Str
	case "reasoning_summary_committed":
		if text := event.Get("text").Str; text != "" {
			a := f.assistant(event.Get("response_id").Str, at)
			a.thinking = append(a.thinking, text)
		}
	case "assistant_message_committed":
		if text := event.Get("text").Str; text != "" {
			a := f.assistant(event.Get("response_id").Str, at)
			a.text = append(a.text, text)
		}
	case "assistant_tool_calls_committed":
		calls := event.Get("tool_calls")
		if len(calls.Array()) == 0 {
			return
		}
		a := f.assistant(event.Get("response_id").Str, at)
		for _, call := range calls.Array() {
			parsed, rendered := museToolCall(call)
			a.calls = append(a.calls, parsed)
			a.rendered = append(a.rendered, rendered)
		}
	case "tool_result_batch_committed":
		f.appendToolResults(event, at)
	case "model_completed":
		usage, ok := newMuseUsage(record, event)
		if !ok {
			return
		}
		if usage.model == "" {
			usage.model = f.model
		}
		f.bindUsage(usage)
	}
}

// assistant returns the assistant message for responseID, creating it on
// first use. Pieces without a response ID each get their own message.
func (f *museFold) assistant(responseID string, at time.Time) *museAssistant {
	if responseID != "" {
		if index, ok := f.byResponse[responseID]; ok {
			if a := f.assistants[index]; a != nil {
				return a
			}
		}
	}
	index := len(f.messages)
	f.messages = append(f.messages, ParsedMessage{
		Ordinal:    index,
		Role:       RoleAssistant,
		Timestamp:  at,
		Model:      f.model,
		SourceType: "assistant",
		SourceUUID: responseID,
	})
	a := &museAssistant{}
	f.assistants[index] = a
	if responseID != "" {
		f.byResponse[responseID] = index
		if usage, ok := f.pending[responseID]; ok {
			delete(f.pending, responseID)
			a.usage = &usage
		}
	}
	return a
}

func (f *museFold) bindUsage(usage museUsage) {
	responseID := f.openResponse
	f.openResponse = ""
	if responseID == "" {
		f.unbound = append(f.unbound, usage)
		return
	}
	if index, ok := f.byResponse[responseID]; ok {
		if a := f.assistants[index]; a != nil && a.usage == nil {
			a.usage = &usage
			return
		}
		f.unbound = append(f.unbound, usage)
		return
	}
	if _, ok := f.pending[responseID]; ok {
		f.unbound = append(f.unbound, usage)
		return
	}
	f.pending[responseID] = usage
}

func (f *museFold) appendToolResults(event gjson.Result, at time.Time) {
	var results []ParsedToolResult
	for _, result := range event.Get("results").Array() {
		callID := result.Get("tool_call_id").Str
		if callID == "" {
			continue
		}
		text := result.Get("text").Str
		raw, err := json.Marshal(text)
		if err != nil {
			continue
		}
		status := ""
		if strings.HasPrefix(text, museToolFailPrefix) {
			status = "errored"
		}
		results = append(results, ParsedToolResult{
			ToolUseID:     callID,
			Source:        "tool_result",
			Status:        status,
			ContentLength: len(text),
			ContentRaw:    string(raw),
		})
	}
	if len(results) == 0 {
		return
	}
	length := 0
	for _, result := range results {
		length += result.ContentLength
	}
	f.messages = append(f.messages, ParsedMessage{
		Ordinal:       len(f.messages),
		Role:          RoleUser,
		Timestamp:     at,
		ContentLength: length,
		ToolResults:   results,
		SourceType:    "tool_result",
		SourceUUID:    event.Get("batch_id").Str,
	})
}

// finish renders the gathered assistant pieces and returns the messages
// plus usage that no assistant message could carry.
func (f *museFold) finish() ([]ParsedMessage, []museUsage) {
	for index, a := range f.assistants {
		pm := &f.messages[index]
		var parts []string
		for _, thinking := range a.thinking {
			parts = append(parts, "[Thinking]\n"+thinking+"\n[/Thinking]")
		}
		parts = append(parts, a.text...)
		parts = append(parts, a.rendered...)
		pm.Content = strings.Join(parts, "\n")
		pm.ContentLength = len(pm.Content)
		pm.ThinkingText = strings.Join(a.thinking, "\n\n")
		pm.HasThinking = len(a.thinking) > 0
		pm.ToolCalls = a.calls
		pm.HasToolUse = len(a.calls) > 0
		if a.usage != nil {
			a.usage.applyTo(pm)
		}
	}
	unbound := f.unbound
	for _, usage := range f.pending {
		unbound = append(unbound, usage)
	}
	slices.SortFunc(unbound, func(a, b museUsage) int {
		return cmp.Compare(a.sequence, b.sequence)
	})
	return f.messages, unbound
}

// museToolCall converts one committed tool call. Muse stores args as a
// JSON-encoded string; an object is accepted as well.
func museToolCall(call gjson.Result) (ParsedToolCall, string) {
	name := call.Get("name").Str
	args := call.Get("args")
	input := args.Raw
	if args.Type == gjson.String {
		if gjson.Valid(args.Str) {
			input = args.Str
		}
	}
	if input == "" {
		input = "{}"
	}
	block := gjson.Parse(`{"name":` + museJSONString(name) + `,"input":` + input + `}`)
	return ParsedToolCall{
		ToolUseID: call.Get("call_id").Str,
		ToolName:  name,
		Category:  museToolCategory(name),
		InputJSON: input,
	}, formatToolUse(block)
}

func museJSONString(s string) string {
	raw, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(raw)
}

// museToolCategory maps Muse tool names whose shared taxonomy entry
// belongs to another agent's tool of the same name.
func museToolCategory(name string) string {
	switch name {
	case "search":
		return "Grep"
	case "subagent_spawn":
		return "Task"
	}
	return NormalizeToolCategory(name)
}

// museWorkspaceFromLog reads the workspace a session opened in. Subagent
// logs omit it, so they inherit their parent's. Only the immediate parent
// is read: no real log has shown a subagent nested inside another, so
// deeper lookups are not supported.
func museWorkspaceFromLog(ctx context.Context, path string) string {
	log, err := readMuseLog(ctx, path, museWorkspaceScanLimit)
	if err != nil {
		return ""
	}
	var facts museFacts
	for _, record := range log.records {
		facts.observe(record)
		if facts.cwd != "" {
			return facts.cwd
		}
	}
	return ""
}

// parseMuseSession parses one Muse log. It returns nil when the log holds
// no records for its own session stream.
func parseMuseSession(
	ctx context.Context, path, machine string,
) (*ParseResult, error) {
	rawID := museSessionIDFromLogPath(path)
	if rawID == "" {
		return nil, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	log, err := readMuseLog(ctx, path, 0)
	if err != nil {
		return nil, err
	}

	var facts museFacts
	fold := newMuseFold()
	owned := 0
	for i, record := range log.records {
		if err := contextErrEvery(ctx, i); err != nil {
			return nil, err
		}
		// A parent log can mirror records from its children's streams.
		// Those belong to the child's own log.
		if record.streamKind != "session" || record.streamID != rawID {
			continue
		}
		owned++
		facts.observe(record)
		fold.observe(record)
	}
	if owned == 0 {
		return nil, nil
	}
	messages, unbound := fold.finish()

	fullID := museSessionPrefix + rawID
	session := ParsedSession{
		ID:              fullID,
		Machine:         machine,
		Agent:           AgentMuse,
		Cwd:             facts.cwd,
		GitBranch:       facts.gitBranch,
		SourceSessionID: rawID,
		SourceVersion:   facts.version,
		MalformedLines:  log.malformedLines,
		SessionName:     facts.sessionName,
		StartedAt:       facts.startedAt,
		EndedAt:         facts.endedAt,
		MessageCount:    len(messages),
		File: FileInfo{
			Path:  path,
			Size:  info.Size(),
			Mtime: info.ModTime().UnixNano(),
		},
	}
	if parentLog := museParentLogPath(path); parentLog != "" {
		session.ParentSessionID = museSessionPrefix +
			museSessionIDFromLogPath(parentLog)
		session.RelationshipType = RelSubagent
		if session.Cwd == "" {
			session.Cwd = museWorkspaceFromLog(ctx, parentLog)
		}
	}
	session.Project = ExtractProjectFromCwdWithBranchContext(
		ctx, session.Cwd, session.GitBranch,
	)
	session.FirstMessage, session.UserMessageCount = firstMessageAndUserCount(messages)
	// Session totals come from the assistant messages. Usage that no
	// message carries is still priced through its usage event; outside a
	// live tool-only response, real logs have not produced such usage.
	accumulateMessageTokenUsage(&session, messages)
	usageEvents := museUsageEvents(fullID, unbound)
	session.UsageEvents = usageEvents
	return &ParseResult{
		Session:     session,
		Messages:    messages,
		UsageEvents: usageEvents,
	}, nil
}

// museUsageEvents records usage that no assistant message carries, such
// as a model call whose response produced no visible output.
func museUsageEvents(sessionID string, usages []museUsage) []ParsedUsageEvent {
	if len(usages) == 0 {
		return nil
	}
	events := make([]ParsedUsageEvent, 0, len(usages))
	for _, u := range usages {
		occurredAt := ""
		if !u.at.IsZero() {
			occurredAt = u.at.Format(time.RFC3339Nano)
		}
		events = append(events, ParsedUsageEvent{
			SessionID:                sessionID,
			Source:                   "model_completed",
			Model:                    u.model,
			InputTokens:              u.uncachedInput(),
			OutputTokens:             u.output,
			CacheCreationInputTokens: u.cacheWrite,
			CacheReadInputTokens:     u.cacheRead,
			ReasoningTokens:          u.reasoning,
			OccurredAt:               occurredAt,
			DedupKey: fmt.Sprintf(
				"session:%s|sequence:%d", sessionID, u.sequence,
			),
		})
	}
	return events
}
