package server

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/wesm/agentsview/internal/db"
	"github.com/wesm/agentsview/internal/parser"
)

type exportMarkdownOptions struct {
	Depth string
}

type exportSessionTree struct {
	Session          *db.Session
	Messages         []db.Message
	AnchoredChildren map[string]*exportSessionTree
	AppendedChildren []*exportSessionTree
}

type markdownSegmentType string

const (
	markdownSegmentText     markdownSegmentType = "text"
	markdownSegmentThinking markdownSegmentType = "thinking"
	markdownSegmentTool     markdownSegmentType = "tool"
	markdownSegmentCode     markdownSegmentType = "code"
	markdownSegmentSkill    markdownSegmentType = "skill"
)

type markdownSegment struct {
	Type     markdownSegmentType
	Content  string
	Label    string
	ToolName string
	ToolCall *db.ToolCall
}

type markdownMatch struct {
	Start   int
	End     int
	Segment markdownSegment
}

func (s *Server) handleMarkdownSession(
	w http.ResponseWriter, r *http.Request,
) {
	depth := strings.TrimSpace(r.URL.Query().Get("depth"))
	if depth != "" && depth != "1" && depth != "all" {
		writeError(w, http.StatusBadRequest, "invalid depth")
		return
	}
	tree, err := s.loadExportSessionTree(r.Context(), r.PathValue("id"), depth, map[string]bool{}, 0)
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if tree == nil || tree.Session == nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	md := generateExportMarkdownTree(tree, exportMarkdownOptions{Depth: depth})
	filename := sanitizeFilename(
		tree.Session.Project + "-" + formatDateShort(tree.Session.StartedAt) + ".md",
	)
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set(
		"Content-Disposition",
		fmt.Sprintf(`inline; filename="%s"`, filename),
	)
	_, _ = io.WriteString(w, md)
}

func (s *Server) loadExportSessionTree(
	ctx context.Context,
	sessionID string,
	depth string,
	visited map[string]bool,
	level int,
) (*exportSessionTree, error) {
	if visited[sessionID] {
		return nil, nil
	}
	visited[sessionID] = true

	sess, err := s.db.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if sess == nil {
		return nil, nil
	}
	msgs, err := s.db.GetAllMessages(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	tree := &exportSessionTree{
		Session:          sess,
		Messages:         msgs,
		AnchoredChildren: map[string]*exportSessionTree{},
	}
	if !shouldLoadMarkdownChildren(depth, level) {
		return tree, nil
	}
	children, err := s.db.GetChildSessions(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	anchored := markdownAnchoredChildIDs(msgs)
	for _, child := range children {
		childTree, err := s.loadExportSessionTree(ctx, child.ID, depth, visited, level+1)
		if err != nil {
			return nil, err
		}
		if childTree == nil || childTree.Session == nil {
			continue
		}
		if anchored[child.ID] {
			tree.AnchoredChildren[child.ID] = childTree
			continue
		}
		tree.AppendedChildren = append(tree.AppendedChildren, childTree)
	}
	return tree, nil
}

func shouldLoadMarkdownChildren(depth string, level int) bool {
	switch depth {
	case "1":
		return level == 0
	case "all":
		return true
	default:
		return false
	}
}

func markdownAnchoredChildIDs(msgs []db.Message) map[string]bool {
	ids := map[string]bool{}
	for _, msg := range msgs {
		for _, tc := range msg.ToolCalls {
			if tc.SubagentSessionID != "" {
				ids[tc.SubagentSessionID] = true
			}
		}
	}
	return ids
}

var (
	mdThinkingMarkedRe = regexp.MustCompile(`(?s)\[Thinking\]\n?(.*?)\n?\[/Thinking\]`)
	mdSkillRe          = regexp.MustCompile(`(?s)\[Skill: (.+?)\]\n?(.*?)\n?\[/Skill\]`)
	mdCodeBlockRe      = regexp.MustCompile("(?s)```(\\w*)\\n(.*?)```")
)

const exportToolNames = "Tool|Read|Write|Edit|Bash|Glob|Grep|Other|TaskCreate|TaskUpdate|TaskGet|TaskList|Task|Agent|" +
	"SendMessage|Question|Todo List|Entering Plan Mode|" +
	"Exiting Plan Mode|exec_command|shell_command|" +
	"write_stdin|apply_patch|shell|parallel|view_image|" +
	"request_user_input|update_plan"

const markdownToolNames = exportToolNames + "|Skill"

var (
	mdToolAliases = map[string]string{
		"Agent":         "Task",
		"exec_command":  "Bash",
		"shell_command": "Bash",
		"write_stdin":   "Bash",
		"shell":         "Bash",
		"apply_patch":   "Edit",
		"str_replace":   "Edit",
		"run_command":   "Bash",
		"create_file":   "Write",
		"read_file":     "Read",
		"bash":          "Bash",
		"read":          "Read",
		"write":         "Write",
		"edit":          "Edit",
		"grep":          "Grep",
		"glob":          "Glob",
		"find":          "Read",
	}
	mdToolStartRe = regexp.MustCompile(`^\[(` + markdownToolNames + `)([^\]]*)\]`)
)

func generateExportMarkdown(
	session *db.Session,
	msgs []db.Message,
	opts exportMarkdownOptions,
) string {
	return generateExportMarkdownTree(&exportSessionTree{
		Session:          session,
		Messages:         msgs,
		AnchoredChildren: map[string]*exportSessionTree{},
	}, opts)
}

func generateExportMarkdownTree(
	tree *exportSessionTree,
	opts exportMarkdownOptions,
) string {
	if tree == nil || tree.Session == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Session: ")
	b.WriteString(markdownHeadingText(tree.Session.Project))
	b.WriteString("\n\n")
	renderMarkdownSession(&b, tree, opts, true)
	return b.String()
}

func renderMarkdownSession(
	b *strings.Builder,
	tree *exportSessionTree,
	opts exportMarkdownOptions,
	root bool,
) {
	if tree == nil || tree.Session == nil {
		return
	}
	tag := "session"
	if !root {
		tag = "child_session"
	}
	attrs := markdownSessionAttrs(tree.Session, root)
	b.WriteString(openTag(tag, attrs))
	b.WriteString("\n")

	renderedAnchors := map[string]bool{}
	for i := range tree.Messages {
		renderMarkdownMessage(b, tree, &tree.Messages[i], opts, renderedAnchors)
	}

	children := sortedExportChildren(tree.AppendedChildren)
	for _, child := range children {
		if child == nil || child.Session == nil {
			continue
		}
		if renderedAnchors[child.Session.ID] {
			continue
		}
		renderMarkdownChildSession(b, child, opts, false)
	}

	b.WriteString(closeTag(tag))
	b.WriteString("\n")
}

func renderMarkdownChildSession(
	b *strings.Builder,
	child *exportSessionTree,
	opts exportMarkdownOptions,
	anchored bool,
) {
	if child == nil || child.Session == nil {
		return
	}
	tag := "child_session"
	if anchored {
		tag = "subagent_session"
	}
	b.WriteString(openTag(tag, markdownSessionAttrs(child.Session, false)))
	b.WriteString("\n")
	renderedAnchors := map[string]bool{}
	for i := range child.Messages {
		renderMarkdownMessage(b, child, &child.Messages[i], opts, renderedAnchors)
	}
	for _, appended := range sortedExportChildren(child.AppendedChildren) {
		if appended == nil || appended.Session == nil {
			continue
		}
		if renderedAnchors[appended.Session.ID] {
			continue
		}
		renderMarkdownChildSession(b, appended, opts, false)
	}
	b.WriteString(closeTag(tag))
	b.WriteString("\n")
}

func renderMarkdownMessage(
	b *strings.Builder,
	tree *exportSessionTree,
	msg *db.Message,
	opts exportMarkdownOptions,
	renderedAnchors map[string]bool,
) {
	attrs := map[string]string{
		"role":    msg.Role,
		"ordinal": fmt.Sprintf("%d", msg.Ordinal),
	}
	if msg.Timestamp != "" {
		attrs["timestamp"] = msg.Timestamp
	}
	if msg.IsSystem {
		attrs["is_system"] = "true"
	}
	if msg.HasThinking {
		attrs["has_thinking"] = "true"
	}
	if msg.HasToolUse {
		attrs["has_tool_use"] = "true"
	}
	b.WriteString(openTag("message", attrs))

	segments := parseMarkdownSegments(*msg)
	if len(segments) == 0 {
		b.WriteString(closeTag("message"))
		b.WriteString("\n")
		return
	}
	b.WriteString("\n")
	for _, seg := range segments {
		switch seg.Type {
		case markdownSegmentText:
			b.WriteString(escapeXMLText(seg.Content))
			b.WriteString("\n")
		case markdownSegmentThinking:
			b.WriteString(renderXMLBodyTag("thinking", nil, seg.Content))
			b.WriteString("\n")
		case markdownSegmentCode:
			codeAttrs := map[string]string{}
			if seg.Label != "" {
				codeAttrs["language"] = seg.Label
			}
			b.WriteString(renderXMLBodyTag("code_block", codeAttrs, seg.Content))
			b.WriteString("\n")
		case markdownSegmentSkill:
			skillAttrs := map[string]string{}
			if seg.Label != "" {
				skillAttrs["name"] = seg.Label
			}
			b.WriteString(renderXMLBodyTag("skill", skillAttrs, seg.Content))
			b.WriteString("\n")
		case markdownSegmentTool:
			renderMarkdownToolSegment(b, tree, seg, opts, renderedAnchors)
		}
	}
	b.WriteString(closeTag("message"))
	b.WriteString("\n")
}

func renderMarkdownToolSegment(
	b *strings.Builder,
	tree *exportSessionTree,
	seg markdownSegment,
	opts exportMarkdownOptions,
	renderedAnchors map[string]bool,
) {
	name, category := markdownToolIdentity(seg)
	attrs := map[string]string{}
	if name != "" {
		attrs["name"] = name
	}
	if category != "" {
		attrs["category"] = category
	}
	if seg.ToolCall != nil {
		if seg.ToolCall.ToolUseID != "" {
			attrs["id"] = seg.ToolCall.ToolUseID
		}
		if seg.ToolCall.SubagentSessionID != "" {
			attrs["subagent_session_id"] = seg.ToolCall.SubagentSessionID
		}
	}
	b.WriteString(openTag("tool_call", attrs))
	b.WriteString("\n")
	if seg.ToolCall != nil && seg.ToolCall.InputJSON != "" {
		b.WriteString(renderXMLBodyTag("arguments", nil, seg.ToolCall.InputJSON))
		b.WriteString("\n")
	}
	b.WriteString(closeTag("tool_call"))
	b.WriteString("\n")

	if body := markdownToolBody(seg); body != "" {
		b.WriteString(renderXMLBodyTag("tool_body", nil, body))
		b.WriteString("\n")
	}
	if seg.ToolCall != nil && seg.ToolCall.ResultContent != "" {
		b.WriteString(renderXMLBodyTag("tool_result", nil, seg.ToolCall.ResultContent))
		b.WriteString("\n")
	}
	if seg.ToolCall != nil {
		for _, ev := range seg.ToolCall.ResultEvents {
			evAttrs := map[string]string{}
			if ev.ToolUseID != "" {
				evAttrs["tool_call_id"] = ev.ToolUseID
			}
			if ev.Source != "" {
				evAttrs["source"] = ev.Source
			}
			if ev.Status != "" {
				evAttrs["status"] = ev.Status
			}
			if ev.AgentID != "" {
				evAttrs["agent_id"] = ev.AgentID
			}
			if ev.SubagentSessionID != "" {
				evAttrs["subagent_session_id"] = ev.SubagentSessionID
			}
			if ev.Timestamp != "" {
				evAttrs["timestamp"] = ev.Timestamp
			}
			b.WriteString(renderXMLBodyTag("tool_result", evAttrs, ev.Content))
			b.WriteString("\n")
		}
	}

	if seg.ToolCall != nil && seg.ToolCall.SubagentSessionID != "" && opts.Depth != "" {
		if child := tree.AnchoredChildren[seg.ToolCall.SubagentSessionID]; child != nil {
			if renderedAnchors[child.Session.ID] {
				return
			}
			renderedAnchors[child.Session.ID] = true
			anchorAttrs := map[string]string{
				"session_id": child.Session.ID,
			}
			if seg.ToolCall.ToolUseID != "" {
				anchorAttrs["tool_call_id"] = seg.ToolCall.ToolUseID
			}
			if opts.Depth != "" {
				anchorAttrs["depth"] = opts.Depth
			}
			b.WriteString(openTag("subagent_anchor", anchorAttrs))
			b.WriteString("\n")
			renderMarkdownChildSession(b, child, opts, true)
			b.WriteString(closeTag("subagent_anchor"))
			b.WriteString("\n")
		}
	}
}

func markdownToolIdentity(seg markdownSegment) (string, string) {
	if seg.ToolCall != nil {
		name := seg.ToolCall.ToolName
		if name == "" {
			name = seg.Label
		}
		category := seg.ToolCall.Category
		if category == "" {
			category = normalizeMarkdownToolName(name)
		}
		return name, category
	}
	name := strings.TrimSpace(seg.ToolName)
	if name == "" {
		name = strings.TrimSpace(seg.Label)
	}
	if name == "" {
		return "Tool", "Tool"
	}
	return name, normalizeMarkdownToolName(name)
}

func markdownToolBody(seg markdownSegment) string {
	if seg.ToolCall != nil {
		if prompt := markdownToolPrompt(seg.ToolCall); prompt != "" {
			return prompt
		}
	}
	if strings.TrimSpace(seg.Content) != "" {
		return seg.Content
	}
	if seg.ToolCall != nil {
		return markdownToolFallback(seg.ToolCall)
	}
	return ""
}

func markdownToolPrompt(tc *db.ToolCall) string {
	if tc == nil || tc.InputJSON == "" {
		return ""
	}
	var params map[string]any
	if json.Unmarshal([]byte(tc.InputJSON), &params) != nil {
		return ""
	}
	if tc.ToolName == "Task" || tc.ToolName == "Agent" || tc.Category == "Task" {
		if prompt, ok := params["prompt"].(string); ok {
			return prompt
		}
	}
	return ""
}

func markdownToolFallback(tc *db.ToolCall) string {
	if tc == nil || tc.InputJSON == "" {
		return ""
	}
	var params map[string]any
	if json.Unmarshal([]byte(tc.InputJSON), &params) != nil {
		return ""
	}
	toolName := normalizeMarkdownToolName(tc.ToolName)
	if toolName == "Task" {
		return ""
	}
	isEdit := toolName == "Edit" || stringValue(params["command"]) == "strReplace"
	if isEdit {
		if diff := stringValue(params["diff"]); diff != "" {
			return capLines(diff, 200)
		}
		oldText := firstString(params, "old_string", "old_str", "oldString", "oldStr")
		newText := firstString(params, "new_string", "new_str", "newString", "newStr")
		if oldText != "" || newText != "" {
			oldLines := strings.Split(oldText, "\n")
			newLines := strings.Split(newText, "\n")
			lines := []string{fmt.Sprintf("@@ -1,%d +1,%d @@", len(oldLines), len(newLines))}
			for _, line := range oldLines {
				lines = append(lines, "-"+line)
			}
			for _, line := range newLines {
				lines = append(lines, "+"+line)
			}
			return capLines(strings.Join(lines, "\n"), 200)
		}
	}
	if toolName == "Write" {
		if text := stringValue(params["content"]); text != "" {
			allLines := strings.Split(text, "\n")
			show := allLines
			if len(show) > 200 {
				show = show[:200]
			}
			body := make([]string, 0, len(show)+1)
			body = append(body, fmt.Sprintf("@@ -0,0 +1,%d @@", len(allLines)))
			for _, line := range show {
				body = append(body, "+"+line)
			}
			if len(show) != len(allLines) {
				body = append(body, fmt.Sprintf("... (%d lines total)", len(allLines)))
			}
			return strings.Join(body, "\n")
		}
	}
	if toolName == "Read" {
		if path := firstString(params, "path", "file_path"); path != "" {
			return path
		}
	}
	if toolName == "Bash" {
		if cmd := firstString(params, "command", "cmd"); cmd != "" {
			return "$ " + cmd
		}
	}
	lines := []string{}
	for _, key := range sortedJSONKeys(params) {
		if key == "agent__intent" || key == "_i" {
			continue
		}
		value := params[key]
		if value == nil {
			continue
		}
		str := stringValue(value)
		if str == "" {
			raw, _ := json.Marshal(value)
			str = string(raw)
		}
		if str == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s: %s", key, truncateMarkdownFallback(str, 200)))
	}
	return strings.Join(lines, "\n")
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

func firstString(params map[string]any, keys ...string) string {
	for _, key := range keys {
		if s := stringValue(params[key]); s != "" {
			return s
		}
	}
	return ""
}

func truncateMarkdownFallback(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

func capLines(text string, max int) string {
	lines := strings.Split(text, "\n")
	if len(lines) <= max {
		return text
	}
	return strings.Join(lines[:max], "\n") + fmt.Sprintf("\n... (%d lines total)", len(lines))
}

func sortedJSONKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func markdownSessionAttrs(s *db.Session, root bool) map[string]string {
	attrs := map[string]string{
		"id":      s.ID,
		"project": s.Project,
		"agent":   markdownAgentDisplay(s.Agent),
	}
	if !root && s.ParentSessionID != nil {
		attrs["parent_session_id"] = *s.ParentSessionID
	}
	if s.RelationshipType != "" && !root {
		attrs["relationship"] = s.RelationshipType
	}
	if s.StartedAt != nil && *s.StartedAt != "" {
		attrs["started_at"] = *s.StartedAt
	}
	if s.EndedAt != nil && *s.EndedAt != "" {
		attrs["ended_at"] = *s.EndedAt
	}
	if s.MessageCount > 0 {
		attrs["message_count"] = fmt.Sprintf("%d", s.MessageCount)
	}
	return attrs
}

func markdownHeadingText(s string) string {
	replacer := strings.NewReplacer("\r", " ", "\n", " ")
	escaped := html.EscapeString(sanitizeXMLText(strings.TrimSpace(replacer.Replace(s))))
	mdEscaper := strings.NewReplacer(
		"\\", "\\\\",
		"`", "\\`",
		"*", "\\*",
		"_", "\\_",
		"[", "\\[",
		"]", "\\]",
		"(", "\\(",
		")", "\\)",
	)
	return mdEscaper.Replace(escaped)
}

func markdownAgentDisplay(agent string) string {
	if def, ok := parser.AgentByType(parser.AgentType(agent)); ok {
		if def.Type == parser.AgentClaude {
			return "Claude"
		}
		return def.DisplayName
	}
	return agent
}

func parseMarkdownSegments(msg db.Message) []markdownSegment {
	matches := extractMarkdownMatches(msg.Content, msg.HasToolUse)
	segments := buildMarkdownSegments(msg.Content, matches)
	segments = mergeThinkingSegments(segments)
	return enrichMarkdownSegments(segments, msg.ToolCalls)
}

func extractMarkdownMatches(text string, parseTools bool) []markdownMatch {
	spans := scanInlineCodeSpans(text)
	codeBlockSpans := scanCodeBlockSpans(text)
	matches := []markdownMatch{}

	for _, m := range mdThinkingMarkedRe.FindAllStringSubmatchIndex(text, -1) {
		start, end := m[0], m[1]
		if insideInlineCodeSpan(start, spans) || insideCodeBlockSpan(start, codeBlockSpans) {
			continue
		}
		matches = append(matches, markdownMatch{Start: start, End: end, Segment: markdownSegment{Type: markdownSegmentThinking, Content: strings.TrimSpace(text[m[2]:m[3]])}})
	}
	matches = append(matches, extractLegacyThinkingMatches(text, spans, codeBlockSpans, matches)...)
	for _, m := range mdSkillRe.FindAllStringSubmatchIndex(text, -1) {
		start, end := m[0], m[1]
		if insideInlineCodeSpan(start, spans) || insideCodeBlockSpan(start, codeBlockSpans) || overlapsMarkdownMatch(start, end, matches) {
			continue
		}
		matches = append(matches, markdownMatch{Start: start, End: end, Segment: markdownSegment{Type: markdownSegmentSkill, Label: text[m[2]:m[3]], Content: strings.TrimSpace(text[m[4]:m[5]])}})
	}
	if parseTools {
		matches = append(matches, extractLegacyToolMatches(text, spans, codeBlockSpans, matches)...)
	}
	for _, m := range mdCodeBlockRe.FindAllStringSubmatchIndex(text, -1) {
		start, end := m[0], m[1]
		if overlapsMarkdownMatch(start, end, matches) {
			continue
		}
		label := ""
		if m[2] >= 0 {
			label = text[m[2]:m[3]]
		}
		content := strings.TrimSuffix(text[m[4]:m[5]], "\n")
		matches = append(matches, markdownMatch{Start: start, End: end, Segment: markdownSegment{Type: markdownSegmentCode, Label: label, Content: content}})
	}
	return resolveMarkdownMatches(matches)
}

func extractLegacyThinkingMatches(
	text string,
	inlineSpans [][2]int,
	codeBlockSpans [][2]int,
	existing []markdownMatch,
) []markdownMatch {
	matches := []markdownMatch{}
	for search := 0; search < len(text); {
		idx := strings.Index(text[search:], "[Thinking]")
		if idx < 0 {
			break
		}
		start := search + idx
		if insideInlineCodeSpan(start, inlineSpans) || insideCodeBlockSpan(start, codeBlockSpans) {
			search = start + len("[Thinking]")
			continue
		}
		contentStart := start + len("[Thinking]")
		if contentStart < len(text) && text[contentStart] == '\n' {
			contentStart++
		}
		end := findLegacyBlockEnd(text, contentStart)
		if overlapsMarkdownMatch(start, end, existing) || overlapsMarkdownMatch(start, end, matches) {
			search = start + len("[Thinking]")
			continue
		}
		matches = append(matches, markdownMatch{
			Start: start,
			End:   end,
			Segment: markdownSegment{
				Type:    markdownSegmentThinking,
				Content: strings.TrimSpace(text[contentStart:end]),
			},
		})
		search = end
	}
	return matches
}

func extractLegacyToolMatches(
	text string,
	inlineSpans [][2]int,
	codeBlockSpans [][2]int,
	existing []markdownMatch,
) []markdownMatch {
	matches := []markdownMatch{}
	for search := 0; search < len(text); {
		next := strings.IndexByte(text[search:], '[')
		if next < 0 {
			break
		}
		start := search + next
		if insideInlineCodeSpan(start, inlineSpans) || insideCodeBlockSpan(start, codeBlockSpans) {
			search = start + 1
			continue
		}
		loc := mdToolStartRe.FindStringSubmatchIndex(text[start:])
		if loc == nil || loc[0] != 0 {
			search = start + 1
			continue
		}
		name := text[start+loc[2] : start+loc[3]]
		args := strings.TrimSpace(text[start+loc[4] : start+loc[5]])
		contentStart := start + loc[1]
		if contentStart < len(text) && text[contentStart] == '\n' {
			contentStart++
		}
		end := findLegacyBlockEnd(text, contentStart)
		if overlapsMarkdownMatch(start, end, existing) || overlapsMarkdownMatch(start, end, matches) {
			search = start + 1
			continue
		}
		normalized := normalizeMarkdownToolName(name)
		label := normalized
		if args != "" {
			label += " " + args
		}
		matches = append(matches, markdownMatch{
			Start: start,
			End:   end,
			Segment: markdownSegment{
				Type:     markdownSegmentTool,
				Label:    label,
				ToolName: normalized,
				Content:  strings.TrimSpace(text[contentStart:end]),
			},
		})
		search = end
	}
	return matches
}

func findLegacyBlockEnd(text string, contentStart int) int {
	if contentStart >= len(text) {
		return contentStart
	}
	if strings.HasPrefix(text[contentStart:], "```") ||
		strings.HasPrefix(text[contentStart:], "[") {
		return contentStart
	}
	end := len(text)
	for _, marker := range []string{"\n```", "\n[", "\n\n"} {
		if idx := strings.Index(text[contentStart:], marker); idx >= 0 {
			cand := contentStart + idx
			if cand < end {
				end = cand
			}
		}
	}
	return end
}

func overlapsMarkdownMatch(start, end int, matches []markdownMatch) bool {
	for _, m := range matches {
		if start < m.End && end > m.Start {
			return true
		}
	}
	return false
}

func resolveMarkdownMatches(matches []markdownMatch) []markdownMatch {
	sort.Slice(matches, func(i, j int) bool { return matches[i].Start < matches[j].Start })
	out := make([]markdownMatch, 0, len(matches))
	lastEnd := 0
	for _, m := range matches {
		if m.Start < lastEnd {
			continue
		}
		out = append(out, m)
		lastEnd = m.End
	}
	return out
}

func buildMarkdownSegments(text string, matches []markdownMatch) []markdownSegment {
	if len(matches) == 0 {
		trimmed := strings.TrimRight(text, "\n")
		if trimmed == "" {
			return nil
		}
		return []markdownSegment{{Type: markdownSegmentText, Content: trimmed}}
	}
	segments := []markdownSegment{}
	pos := 0
	for _, m := range matches {
		if m.Start > pos {
			gap := strings.TrimRight(strings.TrimLeft(text[pos:m.Start], "\n"), "\n")
			if gap != "" {
				segments = append(segments, markdownSegment{Type: markdownSegmentText, Content: gap})
			}
		}
		segments = append(segments, m.Segment)
		pos = m.End
	}
	if pos < len(text) {
		tail := strings.TrimRight(strings.TrimLeft(text[pos:], "\n"), "\n")
		if tail != "" {
			segments = append(segments, markdownSegment{Type: markdownSegmentText, Content: tail})
		}
	}
	return segments
}

func mergeThinkingSegments(segments []markdownSegment) []markdownSegment {
	out := []markdownSegment{}
	for _, seg := range segments {
		if len(out) > 0 && seg.Type == markdownSegmentThinking && out[len(out)-1].Type == markdownSegmentThinking {
			out[len(out)-1].Content += "\n\n" + seg.Content
			continue
		}
		out = append(out, seg)
	}
	return out
}

func enrichMarkdownSegments(segments []markdownSegment, calls []db.ToolCall) []markdownSegment {
	if len(calls) == 0 {
		return segments
	}
	result := make([]markdownSegment, 0, len(segments)+len(calls))
	tcIdx := 0
	for i := 0; i < len(segments); i++ {
		seg := segments[i]
		if seg.Type == markdownSegmentTool && tcIdx < len(calls) {
			tc := calls[tcIdx]
			tcIdx++
			seg.ToolCall = &tc
			if tc.Category == "Bash" && tc.InputJSON != "" {
				var input map[string]any
				if json.Unmarshal([]byte(tc.InputJSON), &input) == nil {
					if cmd, ok := input["command"].(string); ok && strings.Contains(cmd, "\n") {
						rendered := "$ " + cmd
						origContent := strings.TrimSpace(seg.Content)
						prefix := origContent
						for i+1 < len(segments) && segments[i+1].Type == markdownSegmentText {
							next := strings.TrimSpace(segments[i+1].Content)
							if shouldAbsorbBashText(rendered, prefix, next) {
								if next != "" && prefix != "" {
									prefix += "\n\n" + next
								}
								i++
								continue
							}
							break
						}
						seg.Content = rendered
					}
					if cmd, ok := input["cmd"].(string); ok && strings.Contains(cmd, "\n") {
						rendered := "$ " + cmd
						origContent := strings.TrimSpace(seg.Content)
						prefix := origContent
						for i+1 < len(segments) && segments[i+1].Type == markdownSegmentText {
							next := strings.TrimSpace(segments[i+1].Content)
							if shouldAbsorbBashText(rendered, prefix, next) {
								if next != "" && prefix != "" {
									prefix += "\n\n" + next
								}
								i++
								continue
							}
							break
						}
						seg.Content = rendered
					}
				}
			}
			result = append(result, seg)
			continue
		}
		result = append(result, seg)
	}
	for tcIdx < len(calls) {
		tc := calls[tcIdx]
		tcIdx++
		normalized := normalizeMarkdownToolName(tc.ToolName)
		result = append(result, markdownSegment{
			Type:     markdownSegmentTool,
			Label:    normalized,
			ToolName: normalized,
			Content:  markdownToolFallback(&tc),
			ToolCall: &tc,
		})
	}
	return result
}

func shouldAbsorbBashText(rendered, prefix, next string) bool {
	if next == "" {
		return true
	}
	trimmedRendered := strings.TrimSpace(rendered)
	if next == trimmedRendered {
		return true
	}
	if prefix == "" {
		return false
	}
	candidate := prefix + "\n\n" + next
	return strings.HasPrefix(trimmedRendered, candidate)
}

func normalizeMarkdownToolName(name string) string {
	if alias, ok := mdToolAliases[name]; ok {
		return alias
	}
	return name
}

func sortedExportChildren(children []*exportSessionTree) []*exportSessionTree {
	out := append([]*exportSessionTree(nil), children...)
	sort.SliceStable(out, func(i, j int) bool {
		ai, aj := out[i], out[j]
		if ai == nil || ai.Session == nil {
			return false
		}
		if aj == nil || aj.Session == nil {
			return true
		}
		asi, asj := "", ""
		if ai.Session.StartedAt != nil {
			asi = *ai.Session.StartedAt
		}
		if aj.Session.StartedAt != nil {
			asj = *aj.Session.StartedAt
		}
		if asi != asj {
			return asi < asj
		}
		return ai.Session.ID < aj.Session.ID
	})
	return out
}

func renderXMLBodyTag(tag string, attrs map[string]string, body string) string {
	body = sanitizeXMLText(body)
	if strings.Contains(body, "]]>") {
		return openTag(tag, attrs) + escapeXMLText(body) + closeTag(tag)
	}
	return openTag(tag, attrs) + "<![CDATA[\n" + body + "\n]]>" + closeTag(tag)
}

func openTag(tag string, attrs map[string]string) string {
	preferred := preferredTagAttrOrder(tag)
	seen := map[string]bool{}
	keys := make([]string, 0, len(attrs))
	for _, k := range preferred {
		if v := attrs[k]; v != "" {
			keys = append(keys, k)
			seen[k] = true
		}
	}
	for k, v := range attrs {
		if v == "" || seen[k] {
			continue
		}
		keys = append(keys, k)
	}
	var b strings.Builder
	b.WriteString("<")
	b.WriteString(tag)
	for _, k := range keys {
		b.WriteString(" ")
		b.WriteString(k)
		b.WriteString(`="`)
		b.WriteString(escapeXMLAttr(attrs[k]))
		b.WriteString(`"`)
	}
	b.WriteString(">")
	return b.String()
}

func preferredTagAttrOrder(tag string) []string {
	switch tag {
	case "session":
		return []string{"id", "project", "agent", "started_at", "ended_at", "message_count"}
	case "message":
		return []string{"role", "ordinal", "timestamp", "is_system", "has_thinking", "has_tool_use"}
	case "tool_call":
		return []string{"id", "name", "category", "subagent_session_id"}
	case "subagent_anchor":
		return []string{"session_id", "tool_call_id", "depth"}
	case "subagent_session", "child_session":
		return []string{"id", "parent_session_id", "relationship", "project", "agent", "started_at", "ended_at", "message_count"}
	case "tool_result":
		return []string{"tool_call_id", "source", "status", "agent_id", "subagent_session_id", "timestamp"}
	case "skill":
		return []string{"name"}
	case "code_block":
		return []string{"language"}
	default:
		return nil
	}
}

func closeTag(tag string) string {
	return "</" + tag + ">"
}

func sanitizeXMLText(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return r
		}
		if r < 0x20 {
			return -1
		}
		return r
	}, s)
}

func escapeXMLText(s string) string {
	return html.EscapeString(sanitizeXMLText(s))
}

func escapeXMLAttr(s string) string {
	return html.EscapeString(sanitizeXMLText(s))
}

func scanCodeBlockSpans(text string) [][2]int {
	spans := make([][2]int, 0)
	for _, m := range mdCodeBlockRe.FindAllStringSubmatchIndex(text, -1) {
		spans = append(spans, [2]int{m[0], m[1]})
	}
	return spans
}

func scanInlineCodeSpans(text string) [][2]int {
	spans := [][2]int{}
	for i := 0; i < len(text); {
		if text[i] != '`' {
			i++
			continue
		}
		open := i
		for i < len(text) && text[i] == '`' {
			i++
		}
		runLen := i - open
		found := false
		for j := i; j < len(text); j++ {
			if text[j] != '`' {
				continue
			}
			closeStart := j
			for j < len(text) && text[j] == '`' {
				j++
			}
			if j-closeStart == runLen {
				spans = append(spans, [2]int{open, j})
				i = j
				found = true
				break
			}
		}
		if !found {
			continue
		}
	}
	return spans
}

func insideCodeBlockSpan(pos int, spans [][2]int) bool {
	for _, span := range spans {
		if pos >= span[0] && pos < span[1] {
			return true
		}
	}
	return false
}

func insideInlineCodeSpan(pos int, spans [][2]int) bool {
	for _, span := range spans {
		if pos > span[0] && pos < span[1] {
			return true
		}
	}
	return false
}
