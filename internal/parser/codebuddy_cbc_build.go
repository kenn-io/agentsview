package parser

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// codebuddyCBCAssets resolves user images into durable asset references.
// Parsers receive it from the provider; nil keeps a readable placeholder.
type codebuddyCBCAssets interface {
	ImageReference(ref codebuddyImageRef) (string, bool)
}

// codebuddyPersistedOutputs resolves a persisted tool-result path recorded in
// a transcript to its readable text.
type codebuddyPersistedOutputs func(ctx context.Context, path string) (string, bool, error)

// codebuddyCBCBuildOptions carries the source-specific inputs a transcript
// conversion needs.
type codebuddyCBCBuildOptions struct {
	assets    codebuddyCBCAssets
	persisted codebuddyPersistedOutputs
	// legacyUsage holds a converted conversation's 4.x request totals keyed
	// by the 4.x assistant message id that recorded them.
	legacyUsage map[string]ParsedMessage
	// taskAssignment is the index of the item that delivered a team
	// subagent's task; hasTaskAssignment is false when there is none.
	taskAssignment    int
	hasTaskAssignment bool
}

// codebuddyCBCBuilt is the conversion of one replayed path.
type codebuddyCBCBuilt struct {
	messages      []ParsedMessage
	firstMessage  string
	userCount     int
	startedAt     time.Time
	endedAt       time.Time
	subagentCalls map[string]string
	// legacyClaimed lists the legacyUsage keys this path attached.
	legacyClaimed map[string]bool
	// legacyImageMissing reports a 4.x image, codebuddy-asset://, that
	// could not be read.
	legacyImageMissing bool
}

// buildCodeBuddyCBCMessages converts replayed transcript items into parsed
// messages. Items sharing a native response id merge into one assistant
// message so reasoning, text, and tool calls of one model response stay
// together; results pair to calls by call id.
func buildCodeBuddyCBCMessages(
	ctx context.Context, items []codebuddyCBCItem, indexes []int,
	opts codebuddyCBCBuildOptions,
) (codebuddyCBCBuilt, error) {
	b := &codebuddyCBCBuilder{
		opts: opts,
		built: codebuddyCBCBuilt{
			subagentCalls: map[string]string{}, legacyClaimed: map[string]bool{},
		},
		usageSeen: map[string]bool{},
	}
	for n, idx := range indexes {
		if err := contextErrEvery(ctx, n); err != nil {
			return codebuddyCBCBuilt{}, err
		}
		if err := b.add(ctx, items[idx]); err != nil {
			return codebuddyCBCBuilt{}, err
		}
	}
	b.flush()
	for i := range b.built.messages {
		b.built.messages[i].Ordinal = i
	}
	return b.built, nil
}

type codebuddyCBCBuilder struct {
	opts      codebuddyCBCBuildOptions
	built     codebuddyCBCBuilt
	current   *ParsedMessage
	groupKey  string
	texts     []string
	thoughts  []string
	usageSeen map[string]bool
	// splitParts holds the further parts of a 4.x request total that
	// current carries; flush places them right after current.
	splitParts []ParsedMessage
}

func (b *codebuddyCBCBuilder) observeTime(ts time.Time) {
	if ts.IsZero() {
		return
	}
	if b.built.startedAt.IsZero() || ts.Before(b.built.startedAt) {
		b.built.startedAt = ts
	}
	if ts.After(b.built.endedAt) {
		b.built.endedAt = ts
	}
}

func (b *codebuddyCBCBuilder) add(ctx context.Context, it codebuddyCBCItem) error {
	b.observeTime(it.timestamp)
	pd := it.providerData()
	switch it.itemType {
	case "message":
		switch it.role {
		case "user":
			b.flush()
			b.addUser(it)
		case "assistant":
			b.addAssistantText(it)
		case "system":
			b.flush()
			b.appendSystem(it, codebuddyCBCText(it.raw.Get("content")), "system", "")
		}
	case "reasoning":
		b.startGroup(it)
		if text := codebuddyCBCReasoning(it); text != "" {
			b.thoughts = append(b.thoughts, text)
		}
		b.addUsage(it)
		b.addLegacyUsage(it)
	case "function_call":
		b.startGroup(it)
		b.addCall(ctx, it)
		b.addUsage(it)
		b.addLegacyUsage(it)
	case "function_call_result":
		b.flush()
		return b.addResult(ctx, it)
	case "model-usage":
		// Usage-only items share a response id with nothing else; keep them
		// as hidden usage rows so the response still counts once.
		if b.current != nil && b.groupKey == codebuddyGroupKey(it) {
			b.addUsage(it)
			return nil
		}
		b.flush()
		msg := ParsedMessage{Role: RoleAssistant, Timestamp: it.timestamp,
			Model: codebuddyCBCModel(pd), SourceUUID: codebuddyUsageIdentity(pd)}
		if u, ok := codebuddyCBCUsageFrom(pd); ok && b.claimUsage(pd) {
			u.apply(&msg)
			var parts []ParsedMessage
			if u.requestTotal {
				parts = codebuddySplitRequestTotal(&msg)
			}
			row, ok := codebuddyHiddenUsageRow(msg, len(parts) > 0)
			if ok {
				b.built.messages = append(b.built.messages, row)
				b.built.messages = append(b.built.messages, parts...)
			}
		}
	}
	return nil
}

// codebuddyHiddenUsageRow returns msg, which has no visible content, as a
// hidden usage row. Beside the rows usageOnlyMessage keeps, it keeps the
// first part of a split total, whose counters sit at the bound, and any 4.x
// request total with reported counts, such as input alone, which has no
// known context size, or explicit zeros. The row carries the request's
// tokens, and its cb4: identity tells archive recovery the request is still
// recorded, so recovery does not restore an older total under it.
func codebuddyHiddenUsageRow(msg ParsedMessage, split bool) (ParsedMessage, bool) {
	if row, ok := usageOnlyMessage(msg); ok {
		return row, true
	}
	if split || (strings.HasPrefix(msg.SourceUUID, "cb4:") && len(msg.TokenUsage) > 0) {
		return hiddenUsageRow(msg), true
	}
	return ParsedMessage{}, false
}

func codebuddyGroupKey(it codebuddyCBCItem) string {
	pd := it.providerData()
	if id := pd.Get("messageId").Str; id != "" {
		return firstNonEmptyJSONLString(pd.Get("conversationRequestId").Str,
			pd.Get("rootRequestId").Str) + ":" + id
	}
	// Converted 4.x items name their tool calls <message>:call:<n>, so the
	// reasoning, text, and calls of one 4.x message share the prefix.
	base, _, _ := strings.Cut(it.id, ":")
	return "item:" + base
}

func (b *codebuddyCBCBuilder) startGroup(it codebuddyCBCItem) {
	key := codebuddyGroupKey(it)
	if b.current != nil && b.groupKey == key {
		return
	}
	b.flush()
	pd := it.providerData()
	b.current = &ParsedMessage{
		Role:      RoleAssistant,
		Timestamp: it.timestamp,
		Model:     codebuddyCBCModel(pd),
	}
	b.groupKey = key
}

func (b *codebuddyCBCBuilder) addAssistantText(it codebuddyCBCItem) {
	pd := it.providerData()
	text := codebuddyCBCText(it.raw.Get("content"))
	if pd.Get("hookStatus").Exists() || pd.Get("error").Exists() ||
		pd.Get("contentFilterNotice").Bool() {
		b.flush()
		subtype := "interrupted"
		switch {
		case pd.Get("hookStatus").Exists():
			subtype = "stop_hook"
		case pd.Get("contentFilterNotice").Bool():
			subtype = "content_filter"
		case pd.Get("error.message").Str != "" &&
			pd.Get("error.message").Str != "Interrupted by user":
			subtype = "api_error"
			text = firstNonEmptyJSONLString(text, pd.Get("error.message").Str)
		}
		b.appendSystem(it, text, "system", subtype)
		return
	}
	b.startGroup(it)
	if text != "" {
		b.texts = append(b.texts, text)
	}
	if it.raw.Get("status").Str == "incomplete" {
		b.current.StopReason = "incomplete"
	}
	b.addUsage(it)
	b.addLegacyUsage(it)
}

func (b *codebuddyCBCBuilder) addCall(ctx context.Context, it codebuddyCBCItem) {
	name := it.raw.Get("name").Str
	callID := it.raw.Get("callId").Str
	if name == "" {
		return
	}
	args := it.raw.Get("arguments")
	input := args.Raw
	if args.Type == gjson.String {
		input = args.Str
	}
	if strings.TrimSpace(input) == "" {
		input = "{}"
	}
	call := ParsedToolCall{
		ToolUseID: callID,
		ToolName:  name,
		Category:  NormalizeToolCategory(name),
		InputJSON: input,
		Rendering: ToolUseRendering(name, input),
	}
	switch name {
	case "Skill", "skill":
		call.SkillName = gjson.Get(input, "skill").Str
	default:
		call.SkillName = inferToolSkillName(ctx, name, input)
	}
	if call.Rendering == "" {
		call.Rendering = "[Tool: " + name + "]"
	}
	b.current.ToolCalls = append(b.current.ToolCalls, call)
	b.current.HasToolUse = true
}

func (b *codebuddyCBCBuilder) addResult(ctx context.Context, it codebuddyCBCItem) error {
	callID := it.raw.Get("callId").Str
	if callID == "" {
		return nil
	}
	pd := it.providerData()
	output := codebuddyCBCToolOutput(it)
	unresolved := false
	if path := codebuddyPersistedPath(output, pd); path != "" && b.opts.persisted != nil {
		full, ok, err := b.opts.persisted(ctx, path)
		if err != nil {
			return err
		}
		if ok {
			output = full
		} else {
			unresolved = true
		}
	}
	if sub := pd.Get("toolResult.subAgent.sessionId").Str; sub != "" {
		b.built.subagentCalls[callID] = sub
	}
	status := ""
	if it.raw.Get("status").Str == "incomplete" || pd.Get("toolResult.error").Exists() {
		status = "errored"
	}
	// Persisted outputs are raw file bytes; keep them even when they are not
	// valid UTF-8 instead of storing an empty result.
	quoted, _ := json.Marshal(output, jsontext.AllowInvalidUTF8(true))
	result := ParsedToolResult{
		ToolUseID:                 callID,
		Source:                    "tool_result",
		Status:                    status,
		ContentLength:             len(output),
		ContentRaw:                string(quoted),
		PersistedOutputUnresolved: unresolved,
	}
	if it.raw.Get("name").Str == "AskUserQuestion" {
		if answers := codebuddyAskUserAnswers(output); len(answers) > 0 {
			// Human answers are dialogue, so they become a searchable prompt.
			content := strings.Join(answers, "\n")
			b.built.messages = append(b.built.messages, ParsedMessage{
				Role: RoleUser, Content: content, DialogueText: new(content),
				Timestamp: it.timestamp, ContentLength: len(content),
				ToolResults: []ParsedToolResult{result},
				SourceUUID:  it.id, SourceParentUUID: it.parentID,
				SourceType: "user", SourceSubtype: "ask_user_answer",
			})
			b.built.userCount++
			return nil
		}
	}
	b.built.messages = append(b.built.messages, ParsedMessage{
		Role: RoleUser, Timestamp: it.timestamp, ContentLength: len(output),
		ToolResults:   []ParsedToolResult{result},
		SourceUUID:    it.id,
		SourceType:    "user",
		SourceSubtype: SourceSubtypeToolResult,
	})
	return nil
}

// claimUsage reports whether this transcript has not yet counted the
// response's usage. Repeats within one file share one figure.
func (b *codebuddyCBCBuilder) claimUsage(pd gjson.Result) bool {
	key := codebuddyUsageIdentity(pd)
	if key == "" {
		return true
	}
	if b.usageSeen[key] {
		return false
	}
	b.usageSeen[key] = true
	return true
}

func (b *codebuddyCBCBuilder) addUsage(it codebuddyCBCItem) {
	pd := it.providerData()
	u, ok := codebuddyCBCUsageFrom(pd)
	if !ok || b.current == nil || len(b.current.TokenUsage) > 0 {
		return
	}
	if !b.claimUsage(pd) {
		return
	}
	u.apply(b.current)
	b.current.SourceUUID = codebuddyUsageIdentity(pd)
	if u.requestTotal {
		b.splitParts = codebuddySplitRequestTotal(b.current)
	}
}

// addLegacyUsage attaches a converted conversation's 4.x request totals to
// the assistant message the 4.x client recorded them on.
func (b *codebuddyCBCBuilder) addLegacyUsage(it codebuddyCBCItem) {
	if len(b.opts.legacyUsage) == 0 || b.current == nil || len(b.current.TokenUsage) > 0 {
		return
	}
	base, _, _ := strings.Cut(it.id, ":")
	usage, ok := b.opts.legacyUsage[base]
	if !ok || b.built.legacyClaimed[base] {
		return
	}
	b.built.legacyClaimed[base] = true
	b.splitParts = codebuddyCopyUsage(b.current, usage)
}

// codebuddyCopyUsage copies the token accounting of the 4.x request total
// from onto msg. It returns the further parts of a total above the
// per-request bound, which belong right after msg.
func codebuddyCopyUsage(msg *ParsedMessage, from ParsedMessage) []ParsedMessage {
	msg.TokenUsage = from.TokenUsage
	msg.OutputTokens = from.OutputTokens
	msg.HasOutputTokens = from.HasOutputTokens
	msg.ContextTokens = from.ContextTokens
	msg.HasContextTokens = from.HasContextTokens
	msg.tokenPresenceKnown = from.tokenPresenceKnown
	msg.SourceUUID = from.SourceUUID
	if msg.Model == "" {
		msg.Model = from.Model
	}
	return codebuddySplitRequestTotal(msg)
}

func (b *codebuddyCBCBuilder) flush() {
	if b.current == nil {
		return
	}
	msg := *b.current
	split := b.splitParts
	b.current = nil
	b.splitParts = nil
	thinking := strings.TrimSpace(strings.Join(b.thoughts, "\n\n"))
	dialogue := strings.TrimSpace(strings.Join(b.texts, "\n\n"))
	b.texts, b.thoughts = nil, nil
	var parts []string
	if thinking != "" {
		parts = append(parts, "[Thinking]\n"+thinking+"\n[/Thinking]")
		msg.ThinkingText = thinking
		msg.HasThinking = true
	}
	if dialogue != "" {
		parts = append(parts, dialogue)
	}
	for _, call := range msg.ToolCalls {
		parts = append(parts, call.Rendering)
	}
	msg.Content = strings.Join(parts, "\n\n")
	msg.DialogueText = new(dialogue)
	msg.ContentLength = len(msg.Content)
	if msg.Content == "" {
		row, ok := codebuddyHiddenUsageRow(msg, len(split) > 0)
		if !ok {
			return
		}
		msg = row
	}
	if msg.StopReason == "" && len(msg.ToolCalls) == 0 && dialogue != "" {
		msg.StopReason = "end_turn"
	}
	b.built.messages = append(b.built.messages, msg)
	b.built.messages = append(b.built.messages, split...)
}

func (b *codebuddyCBCBuilder) addUser(it codebuddyCBCItem) {
	user := classifyCodeBuddyCBCUser(it)
	assignment := b.opts.hasTaskAssignment && it.index == b.opts.taskAssignment
	if assignment {
		// The lead's task assignment is the subagent's first prompt, as a
		// typed prompt is for a main session.
		user.subtype = ""
	}
	images := b.imageMarkdown(user.images)
	prompt := user.prompt
	if images != "" {
		prompt = strings.TrimSpace(prompt + "\n\n" + images)
	}
	if user.system != "" && user.subtype == "" {
		b.appendSystem(it, user.system, "system", "system_reminder")
	}
	if prompt == "" {
		return
	}
	if user.subtype != "" || codebuddyInternalUser(it) {
		subtype := user.subtype
		if subtype == "" {
			subtype = "system_reminder"
		}
		msg := ParsedMessage{
			Role: RoleUser, Content: prompt, Timestamp: it.timestamp,
			IsSystem: true, ContentLength: len(prompt),
			SourceType: "system", SourceSubtype: subtype,
			SourceUUID: it.id, SourceParentUUID: it.parentID,
		}
		if subtype == "compact_boundary" {
			msg.Role = RoleAssistant
			msg.IsCompactBoundary = true
		}
		b.built.messages = append(b.built.messages, msg)
		return
	}
	if b.built.firstMessage == "" {
		first := user.prompt
		if assignment {
			first = codebuddyTeammateText(first)
		}
		if first == "" {
			first = "[Image]"
		}
		b.built.firstMessage = truncate(strings.ReplaceAll(first, "\n", " "), 300)
	}
	b.built.userCount++
	b.built.messages = append(b.built.messages, ParsedMessage{
		Role: RoleUser, Content: prompt, DialogueText: new(prompt),
		Timestamp: it.timestamp, ContentLength: len(prompt),
		SourceType: "user", SourceUUID: it.id, SourceParentUUID: it.parentID,
	})
}

func (b *codebuddyCBCBuilder) appendSystem(
	it codebuddyCBCItem, text, sourceType, subtype string,
) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	b.built.messages = append(b.built.messages, ParsedMessage{
		Role: RoleUser, Content: text, Timestamp: it.timestamp,
		IsSystem: true, ContentLength: len(text),
		SourceType: sourceType, SourceSubtype: subtype,
		SourceUUID:       codebuddySystemSourceUUID(it.id, subtype),
		SourceParentUUID: it.parentID,
	})
}

// codebuddySystemSourceUUID keeps a split-off reminder distinct from the
// prompt that shares its source item.
func codebuddySystemSourceUUID(id, subtype string) string {
	if id == "" || subtype != "system_reminder" {
		return id
	}
	return id + ":context"
}

func (b *codebuddyCBCBuilder) imageMarkdown(refs []codebuddyImageRef) string {
	var parts []string
	for _, ref := range refs {
		if b.opts.assets != nil {
			if asset, ok := b.opts.assets.ImageReference(ref); ok {
				parts = append(parts, "![image]("+asset+")")
				continue
			}
		}
		if strings.HasPrefix(ref.assetURI, "codebuddy-asset://") {
			b.built.legacyImageMissing = true
		}
		label := firstNonEmptyJSONLString(ref.filename, "image")
		parts = append(parts, "[image unavailable: "+label+"]")
	}
	return strings.Join(parts, "\n")
}

// codebuddyCBCText joins a message's text blocks.
func codebuddyCBCText(content gjson.Result) string {
	if content.Type == gjson.String {
		return strings.TrimSpace(content.Str)
	}
	var parts []string
	content.ForEach(func(_, block gjson.Result) bool {
		switch block.Get("type").Str {
		case "output_text", "input_text", "text", "refusal":
			if text := block.Get("text").Str; strings.TrimSpace(text) != "" {
				parts = append(parts, text)
			}
		}
		return true
	})
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func codebuddyCBCReasoning(it codebuddyCBCItem) string {
	var parts []string
	for _, field := range []string{"rawContent", "content", "summary"} {
		it.raw.Get(field).ForEach(func(_, block gjson.Result) bool {
			if text := strings.TrimSpace(block.Get("text").Str); text != "" {
				parts = append(parts, text)
			}
			return true
		})
		if len(parts) > 0 {
			break
		}
	}
	if len(parts) == 0 {
		if text := strings.TrimSpace(it.providerData().Get("reasoning").Str); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// codebuddyPersistedPath returns the persisted output path a tool result
// names. Only an output that is itself the producer's placeholder counts:
// a Read or Bash output that merely quotes another transcript's placeholder
// is ordinary text.
func codebuddyPersistedPath(output string, pd gjson.Result) string {
	if !strings.HasPrefix(strings.TrimSpace(output), "<persisted-output>") {
		return ""
	}
	if path := persistedOutputPathFromContent(output); path != "" {
		return path
	}
	envelope := pd.Get("toolResult.content").Str
	if !strings.HasPrefix(strings.TrimSpace(envelope), "<persisted-output>") {
		return ""
	}
	return persistedOutputPathFromContent(envelope)
}
