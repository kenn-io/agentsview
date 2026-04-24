// ABOUTME: Parses ChatGPT export archives (conversations-*.json)
// ABOUTME: into structured session data with DAG linearization
// ABOUTME: and content assembly for text, code, thinking, and tools.
package parser

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// AssetResolver resolves and copies image assets from exports.
type AssetResolver interface {
	Resolve(pointer string) (path string, ok bool)
	Copy(srcPath string) (assetRef string, err error)
}

type chatGPTConversation struct {
	ID          string                 `json:"conversation_id"`
	RawID       string                 `json:"id"`
	Title       string                 `json:"title"`
	CreateTime  *float64               `json:"create_time"`
	UpdateTime  *float64               `json:"update_time"`
	CurrentNode string                 `json:"current_node"`
	Mapping     map[string]chatGPTNode `json:"mapping"`
}

type chatGPTNode struct {
	ID       string          `json:"id"`
	Parent   *string         `json:"parent"`
	Children []string        `json:"children"`
	Message  *chatGPTMessage `json:"message"`
}

type chatGPTMessage struct {
	ID         string         `json:"id"`
	CreateTime *float64       `json:"create_time"`
	Author     chatGPTAuthor  `json:"author"`
	Content    chatGPTContent `json:"content"`
	Status     string         `json:"status"`
	Metadata   chatGPTMeta    `json:"metadata"`
}

type chatGPTAuthor struct {
	Role string  `json:"role"`
	Name *string `json:"name"`
}

type chatGPTContent struct {
	ContentType string            `json:"content_type"`
	Parts       []json.RawMessage `json:"parts"`
	Text        string            `json:"text"`
	Language    string            `json:"language"`
	Thoughts    []chatGPTThought  `json:"thoughts"`
	Title       string            `json:"title"`
	URL         string            `json:"url"`
}

type chatGPTThought struct {
	Content string `json:"content"`
}

type chatGPTMeta struct {
	ModelSlug string `json:"model_slug"`
}

// ParseChatGPTExport reads all conversations-*.json files from dir
// and calls onConversation for each non-empty conversation.
func ParseChatGPTExport(
	dir string,
	assets AssetResolver,
	onConversation func(ParseResult) error,
) error {
	// Support both numbered shards (conversations-000.json)
	// and a single conversations.json file.
	matches, err := filepath.Glob(
		filepath.Join(dir, "conversations-*.json"),
	)
	if err != nil {
		return fmt.Errorf("globbing conversations: %w", err)
	}

	single := filepath.Join(dir, "conversations.json")
	if len(matches) == 0 {
		if _, err := os.Stat(single); err == nil {
			matches = []string{single}
		}
	}

	if len(matches) == 0 {
		return fmt.Errorf(
			"no conversation files found in %s", dir,
		)
	}

	sort.Strings(matches)

	for _, path := range matches {
		if err := parseChatGPTFile(
			path, assets, onConversation,
		); err != nil {
			return fmt.Errorf("parsing %s: %w", path, err)
		}
	}

	return nil
}

func parseChatGPTFile(
	path string,
	assets AssetResolver,
	onConversation func(ParseResult) error,
) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading file: %w", err)
	}

	var convs []chatGPTConversation
	if err := json.Unmarshal(data, &convs); err != nil {
		return fmt.Errorf("decoding JSON: %w", err)
	}

	for _, conv := range convs {
		if len(conv.Mapping) == 0 {
			continue
		}

		result, ok := convertChatGPTConversation(conv, assets)
		if !ok {
			continue
		}

		if err := onConversation(result); err != nil {
			return err
		}
	}

	return nil
}

func convertChatGPTConversation(
	conv chatGPTConversation,
	assets AssetResolver,
) (ParseResult, bool) {
	// Fall back to id when conversation_id is empty.
	convID := conv.ID
	if convID == "" {
		convID = conv.RawID
	}
	if convID == "" {
		return ParseResult{}, false
	}

	nodes := linearizeDAG(conv.Mapping, conv.CurrentNode)

	msgs, model := buildChatGPTMessages(nodes, assets)
	if len(msgs) == 0 {
		return ParseResult{}, false
	}

	var (
		userCount int
		firstMsg  string
	)
	for _, m := range msgs {
		if m.Role == RoleUser && !m.IsSystem {
			userCount++
			if firstMsg == "" {
				firstMsg = m.Content
			}
		}
	}

	// Backfill model onto assistant messages that lack one.
	if model != "" {
		for i := range msgs {
			if msgs[i].Role == RoleAssistant &&
				msgs[i].Model == "" {
				msgs[i].Model = model
			}
		}
	}

	sess := ParsedSession{
		ID:               "chatgpt:" + convID,
		Project:          "chatgpt.com",
		Machine:          "local",
		Agent:            AgentChatGPT,
		FirstMessage:     firstMsg,
		DisplayName:      conv.Title,
		StartedAt:        unixFloatToTime(conv.CreateTime),
		EndedAt:          unixFloatToTime(conv.UpdateTime),
		MessageCount:     len(msgs),
		UserMessageCount: userCount,
	}

	return ParseResult{Session: sess, Messages: msgs}, true
}

// linearizeDAG walks parent pointers from currentNode to root,
// then reverses to get chronological order.
func linearizeDAG(
	mapping map[string]chatGPTNode,
	currentNode string,
) []chatGPTNode {
	if currentNode == "" {
		return nil
	}

	chain := make([]chatGPTNode, 0)
	nodeID := currentNode
	for {
		node, ok := mapping[nodeID]
		if !ok {
			break
		}
		chain = append(chain, node)
		if node.Parent == nil || *node.Parent == "" {
			break
		}
		nodeID = *node.Parent
	}

	// Reverse to chronological order.
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}

	return chain
}

// buildChatGPTMessages walks linearized nodes and produces
// ParsedMessages. Tool nodes are attached to the preceding
// assistant message. Returns messages and the last model seen.
func buildChatGPTMessages(
	nodes []chatGPTNode,
	assets AssetResolver,
) ([]ParsedMessage, string) {
	var (
		msgs       []ParsedMessage
		lastModel  string
		lastAsstID = -1
		ordinal    int
	)

	for _, node := range nodes {
		if node.Message == nil {
			continue
		}
		msg := node.Message
		role := msg.Author.Role
		content := assembleContent(msg.Content, assets)

		if msg.Metadata.ModelSlug != "" {
			lastModel = msg.Metadata.ModelSlug
		}

		switch role {
		case "user":
			pm := ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleUser,
				Content:       content,
				Timestamp:     unixFloatToTime(msg.CreateTime),
				ContentLength: len(content),
			}
			msgs = append(msgs, pm)
			lastAsstID = -1
			ordinal++

		case "assistant":
			hasThinking := msg.Content.ContentType == "thoughts"
			pm := ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleAssistant,
				Content:       content,
				Timestamp:     unixFloatToTime(msg.CreateTime),
				HasThinking:   hasThinking,
				ContentLength: len(content),
				Model:         msg.Metadata.ModelSlug,
			}
			msgs = append(msgs, pm)
			lastAsstID = len(msgs) - 1
			ordinal++

		case "system":
			pm := ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleAssistant,
				Content:       content,
				Timestamp:     unixFloatToTime(msg.CreateTime),
				IsSystem:      true,
				ContentLength: len(content),
			}
			msgs = append(msgs, pm)
			lastAsstID = -1
			ordinal++

		case "tool":
			attachToolToAssistant(
				&msgs, lastAsstID, msg, content,
			)
		}
	}

	return msgs, lastModel
}

// attachToolToAssistant attaches a tool node's content to the
// preceding assistant message as a ParsedToolCall.
func attachToolToAssistant(
	msgs *[]ParsedMessage,
	asstIdx int,
	msg *chatGPTMessage,
	content string,
) {
	if asstIdx < 0 || asstIdx >= len(*msgs) {
		return
	}

	ct := msg.Content.ContentType
	asst := &(*msgs)[asstIdx]
	asst.HasToolUse = true

	switch ct {
	case "code":
		asst.ToolCalls = append(asst.ToolCalls, ParsedToolCall{
			ToolName: "code_interpreter",
			Category: NormalizeToolCategory("code_interpreter"),
		})

	case "execution_output":
		// Pair with the last code_interpreter tool call.
		if n := len(asst.ToolCalls); n > 0 {
			tc := &asst.ToolCalls[n-1]
			tc.ResultEvents = append(
				tc.ResultEvents,
				ParsedToolResultEvent{Content: content},
			)
		}

	case "tether_quote", "tether_browsing_display":
		asst.ToolCalls = append(asst.ToolCalls, ParsedToolCall{
			ToolName: "web_search",
			Category: NormalizeToolCategory("web_search"),
		})

	case "multimodal_text":
		// DALL-E tool responses contain generated images as
		// multimodal_text. Append the resolved image content
		// to the assistant message.
		if content != "" {
			if asst.Content != "" {
				asst.Content += "\n\n"
			}
			asst.Content += content
			asst.ContentLength = len(asst.Content)
		}
	}
}

// assembleContent converts a chatGPTContent into a plain-text
// string suitable for display. Unicode normalization is applied
// only to prose content (text, thoughts, quotes, errors), not
// to code blocks, execution output, or URLs.
func assembleContent(
	c chatGPTContent,
	assets AssetResolver,
) string {
	switch c.ContentType {
	case "text", "multimodal_text":
		return assembleTextParts(c.Parts, assets)

	case "code":
		return "```" + c.Language + "\n" + c.Text + "\n```"

	case "execution_output":
		return "```\n" + c.Text + "\n```"

	case "thoughts":
		var parts []string
		for _, t := range c.Thoughts {
			if t.Content != "" {
				parts = append(parts, normalizeUnicode(t.Content))
			}
		}
		if len(parts) == 0 {
			return ""
		}
		return "[Thinking]\n" +
			strings.Join(parts, "\n") +
			"\n[/Thinking]"

	case "tether_quote":
		line := "> " + normalizeUnicode(c.Text)
		if c.Title != "" || c.URL != "" {
			line += "\n> -- [" +
				normalizeUnicode(c.Title) + "](" + c.URL + ")"
		}
		return line

	case "reasoning_recap", "tether_browsing_display":
		return ""

	case "system_error":
		return normalizeUnicode(c.Text)

	default:
		return assembleFallbackParts(c.Parts)
	}
}

// assembleTextParts joins Parts from text/multimodal_text
// content. Each part is either a JSON string or an object.
func assembleTextParts(
	parts []json.RawMessage,
	assets AssetResolver,
) string {
	var out []string

	for _, raw := range parts {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			if s != "" {
				out = append(out, normalizeUnicode(s))
			}
			continue
		}

		// Try as an object with content_type field.
		var obj struct {
			ContentType  string `json:"content_type"`
			AssetPointer string `json:"asset_pointer"`
		}
		if err := json.Unmarshal(raw, &obj); err != nil {
			continue
		}

		if obj.ContentType == "image_asset_pointer" {
			out = append(out, resolveImageAsset(
				obj.AssetPointer, assets,
			))
		}
	}

	return strings.Join(out, "\n")
}

// assembleFallbackParts tries to join parts as strings for
// unknown content types.
func assembleFallbackParts(parts []json.RawMessage) string {
	var out []string
	for _, raw := range parts {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			if s != "" {
				out = append(out, s)
			}
		}
	}
	return strings.Join(out, "\n")
}

// resolveImageAsset attempts to resolve and copy an image asset.
func resolveImageAsset(
	pointer string,
	assets AssetResolver,
) string {
	if assets == nil {
		return "[image unavailable]"
	}

	srcPath, ok := assets.Resolve(pointer)
	if !ok {
		return "[image unavailable]"
	}

	ref, err := assets.Copy(srcPath)
	if err != nil {
		return "[image unavailable]"
	}

	return "![image](" + ref + ")"
}

// normalizeUnicode replaces decorative Unicode characters with
// their plain ASCII equivalents.
var unicodeReplacer = strings.NewReplacer(
	"\u275D", `"`, // HEAVY DOUBLE TURNED COMMA QUOTATION MARK ORNAMENT
	"\u275E", `"`, // HEAVY DOUBLE COMMA QUOTATION MARK ORNAMENT
	"\u275B", "'", // HEAVY SINGLE TURNED COMMA QUOTATION MARK ORNAMENT
	"\u275C", "'", // HEAVY SINGLE COMMA QUOTATION MARK ORNAMENT
	"\u201C", `"`, // LEFT DOUBLE QUOTATION MARK
	"\u201D", `"`, // RIGHT DOUBLE QUOTATION MARK
	"\u2018", "'", // LEFT SINGLE QUOTATION MARK
	"\u2019", "'", // RIGHT SINGLE QUOTATION MARK
	"\u2013", "-", // EN DASH
	"\u2014", "--", // EM DASH
	"\u2026", "...", // HORIZONTAL ELLIPSIS
)

func normalizeUnicode(s string) string {
	return unicodeReplacer.Replace(s)
}

func unixFloatToTime(f *float64) time.Time {
	if f == nil || *f == 0 {
		return time.Time{}
	}
	sec := int64(*f)
	nsec := int64((*f - float64(sec)) * 1e9)
	return time.Unix(sec, nsec)
}
