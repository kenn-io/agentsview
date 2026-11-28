package parser

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"regexp"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"go.kenn.io/agentsview/internal/stringutil"
	"go.kenn.io/agentsview/internal/usagefacts"
)

var (
	codebuddySystemReminderRe = regexp.MustCompile(
		`(?s)<system-reminder(?:\s[^>]*)?>.*?</system-reminder>`,
	)
	codebuddyUserQueryTagRe   = regexp.MustCompile(`(?s)<user_query>(.*?)</user_query>`)
	codebuddyContextRefRe     = regexp.MustCompile(`(?s)^<context\s+ref="[^"]*"[^>]*>.*</context>$`)
	codebuddyBashInputRe      = regexp.MustCompile(`(?s)<bash-input>(.*?)</bash-input>`)
	codebuddyLocalCommandRe   = regexp.MustCompile(`(?s)^<local-command-(?:stdout|stderr)>.*</local-command-(?:stdout|stderr)>$`)
	codebuddyAskAnswerLineRe  = regexp.MustCompile(`^\s*·\s*(.*?)\s*→\s*(.*)$`)
	codebuddyCompactSummaryRe = regexp.MustCompile(`(?i)<cb_summary\b|<conversation_history_summary\b|data-role=["']compact-summary["']`)
	codebuddyTeammateTagRe    = regexp.MustCompile(`<teammate-message\b[^>]*>|</teammate-message\s*>`)
	codebuddyTeammateSummary  = regexp.MustCompile(`^<teammate-message\b[^>]*\bsummary="([^"]*)"`)
)

// codebuddyCBCUserText is the classified text of one user message.
type codebuddyCBCUserText struct {
	prompt  string
	system  string
	subtype string
	images  []codebuddyImageRef
}

// codebuddyImageRef points at one image a user message attached.
type codebuddyImageRef struct {
	blobID   string
	mime     string
	filename string
	dataURL  string
	assetURI string
}

// classifyCodeBuddyCBCUser splits one user message into prompt text and
// producer-injected content. Reminders, attached context, and image path
// markers leave the prompt; command envelopes render as the typed command.
func classifyCodeBuddyCBCUser(it codebuddyCBCItem) codebuddyCBCUserText {
	pd := it.providerData()
	var out codebuddyCBCUserText
	var promptParts, systemParts []string
	addText := func(text string, replacement gjson.Result) {
		if replacement.Type == gjson.String &&
			strings.TrimSpace(replacement.Str) != "" &&
			strings.Contains(text, "<command-") {
			// Skill expansions keep the typed command beside the expansion.
			promptParts = append(promptParts, strings.TrimSpace(replacement.Str))
			return
		}
		prompt, system := splitCodeBuddyCBCUserText(text)
		if prompt != "" {
			promptParts = append(promptParts, prompt)
		}
		if system != "" {
			systemParts = append(systemParts, system)
		}
	}
	content := it.raw.Get("content")
	if content.Type == gjson.String {
		addText(content.Str, gjson.Result{})
	}
	content.ForEach(func(_, block gjson.Result) bool {
		switch block.Get("type").Str {
		case "input_text", "text":
			addText(block.Get("text").Str, block.Get("providerData.content"))
		case "image_blob_ref":
			out.images = append(out.images, codebuddyImageRef{
				blobID:   block.Get("blob_id").Str,
				mime:     block.Get("mime").Str,
				filename: block.Get("original_filename").Str,
			})
		case "input_image", "image":
			image := block.Get("image")
			if image.Type != gjson.String {
				image = block.Get("image.url")
			}
			if image.Type != gjson.String {
				image = block.Get("image_url")
			}
			ref := codebuddyImageRef{}
			switch {
			case strings.HasPrefix(image.Str, "data:"):
				ref.dataURL = image.Str
			case strings.HasPrefix(image.Str, "codebuddy-asset://"):
				ref.assetURI = image.Str
			}
			out.images = append(out.images, ref)
		}
		return true
	})
	out.prompt = strings.TrimSpace(strings.Join(promptParts, "\n"))
	out.system = strings.TrimSpace(strings.Join(systemParts, "\n\n"))
	switch {
	case pd.Get("teammateMessage").Exists() ||
		strings.HasPrefix(out.prompt, "<teammate-message"):
		out.subtype = "peer_message"
	case pd.Get("backgroundTaskNotification").Bool() ||
		strings.HasPrefix(out.prompt, "<task-notification>"):
		out.subtype = "task_notification"
	case pd.Get("isCompacted").Bool() || pd.Get("isSummary").Bool() ||
		codebuddyCompactSummaryRe.MatchString(out.prompt):
		out.subtype = "compact_boundary"
	case pd.Get("turnAborted").Bool():
		out.subtype = "interrupted"
	case strings.HasPrefix(out.prompt, "<local-command-caveat>"),
		strings.Contains(out.system, `data-role="command-caveat"`) && out.prompt == "":
		out.subtype = "resume"
	case codebuddyLocalCommandRe.MatchString(out.prompt):
		out.subtype = "local_command_output"
	}
	return out
}

// codebuddyInternalUser reports whether a user item is a producer-internal
// prompt rather than a turn anyone sent.
func codebuddyInternalUser(it codebuddyCBCItem) bool {
	pd := it.providerData()
	return pd.Get("skipRun").Bool() || pd.Get("isCompactInternal").Bool() ||
		pd.Get("agent").Str == "compact" || pd.Get("isMeta").Bool()
}

// codebuddyTeamTaskAssignment finds the item that gave a team subagent its
// task. The lead starts a teammate by sending the task as a teammate message
// (CLI formatTeammateMessage with summary "Initial task assignment for
// <member>"), so a subagent transcript whose first prompt is a teammate
// message opens with its assignment rather than a peer reply.
func codebuddyTeamTaskAssignment(items []codebuddyCBCItem) (int, bool) {
	for _, it := range items {
		if it.itemType != "message" || it.role != "user" || codebuddyInternalUser(it) {
			continue
		}
		user := classifyCodeBuddyCBCUser(it)
		if user.prompt == "" && len(user.images) == 0 {
			continue
		}
		if user.subtype == "peer_message" &&
			strings.HasPrefix(user.prompt, "<teammate-message") {
			return it.index, true
		}
		return 0, false
	}
	return 0, false
}

// codebuddyTeammateText returns a teammate message without its wrapper,
// falling back to the summary attribute when the body is empty.
func codebuddyTeammateText(prompt string) string {
	if body := strings.TrimSpace(codebuddyTeammateTagRe.ReplaceAllString(prompt, "")); body != "" {
		return body
	}
	if m := codebuddyTeammateSummary.FindStringSubmatch(prompt); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// splitCodeBuddyCBCUserText separates one text block into prompt text and
// producer-injected content.
func splitCodeBuddyCBCUserText(text string) (prompt, system string) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return "", ""
	}
	if codebuddyContextRefRe.MatchString(trimmed) {
		return "", trimmed
	}
	var injected []string
	stripped := codebuddySystemReminderRe.ReplaceAllStringFunc(trimmed, func(block string) string {
		injected = append(injected, block)
		return ""
	})
	if open := strings.Index(stripped, "<system-reminder"); open >= 0 {
		// An unterminated reminder swallows the rest of the block.
		injected = append(injected, stripped[open:])
		stripped = stripped[:open]
	}
	if end := strings.Index(stripped, "</system-reminder>"); end >= 0 {
		// A reminder split across blocks closes here.
		injected = append(injected, stripped[:end+len("</system-reminder>")])
		stripped = stripped[end+len("</system-reminder>"):]
	}
	stripped = codebuddyStripImagePaths(stripped)
	if strings.HasPrefix(strings.TrimSpace(stripped), "<user_query>") {
		// Only a whole-block query wrapper is the typed prompt; a summary
		// that quotes earlier queries keeps its full text.
		if m := codebuddyUserQueryTagRe.FindStringSubmatch(stripped); m != nil {
			stripped = strings.TrimSpace(m[1])
		}
	}
	if cmd, ok := extractCommandText(stripped); ok {
		stripped = cmd
	} else if m := codebuddyBashInputRe.FindStringSubmatch(stripped); m != nil {
		stripped = "! " + strings.TrimSpace(m[1])
	}
	return strings.TrimSpace(stripped), strings.TrimSpace(strings.Join(injected, "\n"))
}

// codebuddyCBCUsage is one response's normalized token accounting.
type codebuddyCBCUsage struct {
	model      string
	input      int
	output     int
	cacheRead  int
	cacheWrite int
	reasoning  int
	hasInput   bool
	hasOutput  bool
	hasRead    bool
	hasWrite   bool
	hasReason  bool
	// context replaces the prompt-derived context size when the record
	// reports the window separately.
	context    int
	hasContext bool
	// requestTotal marks counters that sum every model step of one 4.x
	// request, which codebuddySplitRequestTotal splits above the
	// per-request bound.
	requestTotal bool
}

// codebuddyCBCUsageFrom interprets rawUsage first and the normalized usage
// object second, following the CLI's UsageUtils: prompt_tokens includes
// cached tokens, completion_tokens includes reasoning tokens, and
// prompt_cache_miss_tokens is uncached input, not a cache write.
func codebuddyCBCUsageFrom(pd gjson.Result) (codebuddyCBCUsage, bool) {
	raw := pd.Get("rawUsage")
	norm := pd.Get("usage")
	if !raw.IsObject() && !norm.IsObject() {
		return codebuddyCBCUsage{}, false
	}
	u := codebuddyCBCUsage{model: codebuddyCBCModel(pd)}
	u.requestTotal = pd.Get("agent").Str == "codebuddy-ide-4.x"
	inputField := codebuddyFirstNumber(raw, "prompt_tokens", "input_tokens", "inputTokens")
	if !inputField.Exists() {
		inputField = codebuddyFirstNumber(norm, "inputTokens", "input_tokens", "prompt_tokens")
	}
	outputField := codebuddyFirstNumber(raw, "completion_tokens", "output_tokens", "outputTokens")
	if !outputField.Exists() {
		outputField = codebuddyFirstNumber(norm, "outputTokens", "output_tokens", "completion_tokens")
	}
	read, hasRead := codebuddyCounter([]gjson.Result{
		raw.Get("cache_read_input_tokens"), raw.Get("cacheReadInputTokens"),
		codebuddyDetailSum(raw.Get("inputTokensDetails"), "cached_tokens"),
		raw.Get("prompt_tokens_details.cached_tokens"),
		raw.Get("prompt_cache_hit_tokens"),
	}, []gjson.Result{
		norm.Get("cache_read_input_tokens"), norm.Get("cacheReadInputTokens"),
		codebuddyDetailSum(norm.Get("inputTokensDetails"), "cached_tokens"),
	})
	write, hasWrite := codebuddyCounter([]gjson.Result{
		raw.Get("cache_creation_input_tokens"), raw.Get("cacheCreationInputTokens"),
		raw.Get("prompt_cache_write_tokens"),
	}, []gjson.Result{
		norm.Get("cache_creation_input_tokens"), norm.Get("cacheCreationInputTokens"),
	})
	reason, hasReason := codebuddyCounter([]gjson.Result{
		raw.Get("completion_thinking_tokens"),
		raw.Get("completion_tokens_details.reasoning_tokens"),
	}, []gjson.Result{
		codebuddyDetailSum(norm.Get("outputTokensDetails"), "reasoning_tokens"),
	})
	u.hasInput = inputField.Exists()
	u.hasOutput = outputField.Exists()
	u.hasRead, u.hasWrite, u.hasReason = hasRead, hasWrite, hasReason
	prompt := max(int(inputField.Int()), 0)
	u.cacheRead = read
	u.cacheWrite = write
	u.input = max(prompt-read-write, 0)
	u.output = max(int(outputField.Int()), 0)
	u.reasoning = min(reason, u.output)
	// A 4.x request carried into a converted transcript sums every model
	// step, so its prompt total is not a context window; the last step's
	// prompt is.
	if last := pd.Get("lastStepInputTokens"); last.Type == gjson.Number && last.Int() >= 0 {
		u.context = int(last.Int())
		u.hasContext = true
	}
	return u, u.hasInput || u.hasOutput || u.hasRead || u.hasWrite
}

func codebuddyFirstNumber(obj gjson.Result, keys ...string) gjson.Result {
	for _, key := range keys {
		if v := obj.Get(key); v.Type == gjson.Number && v.Int() >= 0 {
			return v
		}
	}
	return gjson.Result{}
}

// codebuddyCounter reads one token counter, preferring the original
// rawUsage fields to the normalized usage object. Within one source the
// first positive field wins, since providers fill different aliases, and a
// source whose fields report only zero gives an explicit zero. The
// normalized fields count only when no original field holds a valid number.
func codebuddyCounter(raw, norm []gjson.Result) (int, bool) {
	if v, ok := codebuddyFirstValid(raw); ok {
		return v, true
	}
	return codebuddyFirstValid(norm)
}

// codebuddyFirstValid returns the first positive value in values, or zero
// when the only valid values are zero. Negative and non-numeric values are
// not valid.
func codebuddyFirstValid(values []gjson.Result) (int, bool) {
	found := false
	for _, v := range values {
		if v.Type != gjson.Number || v.Int() < 0 {
			continue
		}
		if v.Int() > 0 {
			return int(v.Int()), true
		}
		found = true
	}
	return 0, found
}

func codebuddyDetailSum(details gjson.Result, key string) gjson.Result {
	if details.IsObject() {
		return details.Get(key)
	}
	if !details.IsArray() {
		return gjson.Result{}
	}
	total := int64(0)
	seen := false
	details.ForEach(func(_, d gjson.Result) bool {
		if v := d.Get(key); v.Type == gjson.Number {
			total += v.Int()
			seen = true
		}
		return true
	})
	if !seen {
		return gjson.Result{}
	}
	return gjson.Parse(strconv.FormatInt(total, 10))
}

func (u codebuddyCBCUsage) apply(msg *ParsedMessage) {
	normalized := map[string]int{}
	if u.hasInput {
		normalized["input_tokens"] = u.input
	}
	if u.hasOutput {
		normalized["output_tokens"] = u.output
	}
	if u.hasRead {
		normalized["cache_read_input_tokens"] = u.cacheRead
	}
	if u.hasWrite {
		normalized["cache_creation_input_tokens"] = u.cacheWrite
	}
	if u.hasReason {
		normalized["reasoning_tokens"] = u.reasoning
	}
	data, err := json.Marshal(normalized, json.Deterministic(true))
	if err != nil {
		return
	}
	msg.TokenUsage = data
	if u.model != "" {
		msg.Model = u.model
	}
	msg.OutputTokens = u.output
	msg.HasOutputTokens = u.hasOutput
	msg.ContextTokens = u.input + u.cacheRead + u.cacheWrite
	msg.HasContextTokens = u.hasInput || u.hasRead || u.hasWrite
	switch {
	case u.hasContext:
		msg.ContextTokens = u.context
		msg.HasContextTokens = true
	case u.requestTotal:
		// A 4.x request total sums every step's prompt, so it is no
		// context window; without the last step's prompt the window is
		// unknown.
		msg.ContextTokens = 0
		msg.HasContextTokens = false
	}
	msg.tokenPresenceKnown = true
}

// codebuddyMaxRequestTotalParts bounds how many rows one 4.x request total
// splits into: 64 parts hold 128 million tokens per counter, far above what
// one request records.
const codebuddyMaxRequestTotalParts = 64

// codebuddySplitRequestTotal splits the 4.x request total msg carries when a
// counter passes usagefacts.MaxPlausibleTokens, the bound every backend
// applies to one message's counters. The total sums every model step of the
// request, so it can pass that bound. msg keeps the first part, its identity,
// and its context size, the last step's prompt. Each further part is returned
// as a hidden usage row named <identity>#<n>, n from 2, with msg's model and
// timestamp and no context size, for the caller to place right after msg.
// Every part takes up to the bound from each counter, so the parts sum to the
// total, and since reasoning never exceeds output, it stays within output in
// each part. A total within the bound returns no rows and leaves msg as it is.
// A total that would need more than codebuddyMaxRequestTotalParts parts is
// not a plausible request and is left whole, so backends cap it as before
// and a corrupt counter cannot allocate millions of rows.
func codebuddySplitRequestTotal(msg *ParsedMessage) []ParsedMessage {
	const bound = usagefacts.MaxPlausibleTokens
	var remaining map[string]int
	if len(msg.TokenUsage) == 0 || json.Unmarshal(msg.TokenUsage, &remaining) != nil {
		return nil
	}
	parts := 1
	for _, v := range remaining {
		if v > codebuddyMaxRequestTotalParts*bound {
			return nil
		}
		parts = max(parts, (v+bound-1)/bound)
	}
	if parts == 1 {
		return nil
	}
	take := func() (jsontext.Value, int, bool) {
		part := make(map[string]int, len(remaining))
		for key, v := range remaining {
			part[key] = min(v, bound)
			remaining[key] = v - part[key]
		}
		data, err := json.Marshal(part, json.Deterministic(true))
		return data, part["output_tokens"], err == nil
	}
	first, output, ok := take()
	if !ok {
		return nil
	}
	rows := make([]ParsedMessage, 0, parts-1)
	for n := 2; n <= parts; n++ {
		data, partOutput, ok := take()
		if !ok {
			return nil
		}
		row := hiddenUsageRow(ParsedMessage{
			Timestamp: msg.Timestamp, Model: msg.Model,
			TokenUsage: data, OutputTokens: partOutput,
			HasOutputTokens: msg.HasOutputTokens, tokenPresenceKnown: true,
		})
		// A part without an identity would join other requests' parts.
		if msg.SourceUUID != "" {
			row.SourceUUID = msg.SourceUUID + "#" + strconv.Itoa(n)
		}
		rows = append(rows, row)
	}
	msg.TokenUsage = first
	msg.OutputTokens = output
	return rows
}

// codebuddyUsageIdentity is one response's billing identity. A response keeps
// one usage figure however many items or forked files repeat it.
func codebuddyUsageIdentity(pd gjson.Result) string {
	request := firstNonEmptyJSONLString(
		pd.Get("conversationRequestId").Str, pd.Get("rootRequestId").Str,
	)
	message := pd.Get("messageId").Str
	if request == "" || message == "" {
		return ""
	}
	if pd.Get("agent").Str == "codebuddy-ide-4.x" {
		// A converted 4.x record carries the totals the 4.x message with
		// this id recorded; the 4.x copy names that total the same way.
		return "cb4:" + request + ":" + message
	}
	key := "cb:" + request + ":" + message
	if step := pd.Get("stepSeq"); step.Type == gjson.Number {
		key += ":" + strconv.FormatInt(step.Int(), 10)
	}
	return key
}

func codebuddyCBCModel(pd gjson.Result) string {
	return firstNonEmptyJSONLString(
		pd.Get("model").Str, pd.Get("requestModelName").Str,
		pd.Get("requestModelId").Str,
	)
}

// codebuddyCBCToolOutput extracts a function_call_result's displayed text.
func codebuddyCBCToolOutput(it codebuddyCBCItem) string {
	output := it.raw.Get("output")
	switch {
	case output.Type == gjson.String:
		return output.Str
	case output.IsObject():
		if text := output.Get("text"); text.Type == gjson.String {
			return text.Str
		}
		return output.Raw
	case output.IsArray():
		var parts []string
		output.ForEach(func(_, block gjson.Result) bool {
			if text := block.Get("text").Str; text != "" {
				parts = append(parts, text)
			}
			return true
		})
		return strings.Join(parts, "\n")
	}
	if content := it.providerData().Get("toolResult.content"); content.Type == gjson.String {
		return content.Str
	}
	return ""
}

// codebuddyAskUserAnswers parses the CLI's AskUserQuestion result, a list of
// " · question → answer" lines, into searchable answer lines.
func codebuddyAskUserAnswers(output string) []string {
	var answers []string
	for line := range strings.SplitSeq(output, "\n") {
		m := codebuddyAskAnswerLineRe.FindStringSubmatch(line)
		if m == nil || strings.TrimSpace(m[2]) == "" {
			continue
		}
		answer := strings.TrimSpace(m[2])
		label := stringutil.SafeTruncate(strings.TrimSpace(m[1]), 120)
		if label == "" {
			answers = append(answers, "[Answer] "+answer)
			continue
		}
		answers = append(answers, "[Answer: "+label+"] "+answer)
	}
	return answers
}
