package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/terminaltext"
)

var (
	colorAccent = lipgloss.Color("63")
	colorMuted  = lipgloss.Color("244")
	colorError  = lipgloss.Color("196")
	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(colorAccent)
	mutedStyle  = lipgloss.NewStyle().Foreground(colorMuted)
	errorStyle  = lipgloss.NewStyle().Foreground(colorError)
	activeStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(colorAccent)
	borderStyle = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("238")).Padding(0, 1)
)

func (m *model) View() tea.View {
	m.syncStyles()
	width, height := max(m.width, 40), max(m.height, 12)
	content := m.render(width, height)
	view := tea.NewView(content)
	view.AltScreen = true
	view.WindowTitle = "AgentsView TUI"
	return view
}

func (m *model) syncStyles() {
	accent, muted, border := lipgloss.Color("63"), lipgloss.Color("244"), lipgloss.Color("238")
	if m.theme == "light" {
		accent, muted, border = lipgloss.Color("25"), lipgloss.Color("240"), lipgloss.Color("250")
	}
	if m.highContrast {
		accent, muted, border = lipgloss.Color("15"), lipgloss.Color("252"), lipgloss.Color("15")
	}
	colorAccent, colorMuted = accent, muted
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(accent)
	mutedStyle = lipgloss.NewStyle().Foreground(muted)
	activeStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(accent)
	borderStyle = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(border).Padding(0, 1)
}

func (m *model) render(width, height int) string {
	header := m.renderHeader(width)
	footer := m.renderFooter(width)
	bodyHeight := max(3, height-lipgloss.Height(header)-lipgloss.Height(footer))
	var body string
	if m.help {
		body = m.renderHelp(width, bodyHeight)
	} else if m.confirm != nil {
		body = m.renderConfirm(width, bodyHeight)
	} else {
		body = m.renderPage(width, bodyHeight)
	}
	return fitHeight(header+"\n"+body+"\n"+footer, height)
}

func (m *model) renderHeader(width int) string {
	name := titleStyle.Render(m.strings.App)
	page := m.strings.PageNames[m.page]
	filters := []string{}
	if m.filter.Project != "" {
		filters = append(filters, "project="+safe(m.filter.Project))
	}
	if m.filter.Agent != "" {
		filters = append(filters, "agent="+safe(m.filter.Agent))
	}
	if m.filter.Machine != "" {
		filters = append(filters, "machine="+safe(m.filter.Machine))
	}
	right := page
	if len(filters) > 0 {
		right += "  " + strings.Join(filters, " ")
	}
	if m.readOnly {
		right += "  [" + m.strings.ReadOnly + "]"
	}
	gap := max(1, width-lipgloss.Width(name)-lipgloss.Width(right))
	return truncateWidth(name+strings.Repeat(" ", gap)+mutedStyle.Render(right), width)
}

func (m *model) renderFooter(width int) string {
	if m.inputMode != "" {
		prefix := ":"
		if m.inputMode == "search" {
			prefix = "/"
		}
		m.input.SetWidth(max(1, width-3))
		return truncateWidth(activeStyle.Render(prefix)+" "+m.input.View(), width)
	}
	left := "tab panes  j/k move  enter select  / search  : command  ? help  q quit"
	right := safe(m.status)
	if m.loading {
		right = m.strings.Loading
	}
	if m.errText != "" {
		right = errorStyle.Render(m.strings.Error + ": " + safe(m.errText))
	}
	if right == "" {
		return truncateWidth(mutedStyle.Render(left), width)
	}
	available := max(0, width-lipgloss.Width(right)-2)
	return truncateWidth(mutedStyle.Render(truncateWidth(left, available))+"  "+right, width)
}

func (m *model) renderPage(width, height int) string {
	if m.page == PageSessions {
		return m.renderSessions(width, height)
	}
	navWidth := 21
	if width < 80 {
		if m.focus == 0 {
			return m.renderNavigation(width, height)
		}
		return m.renderReport(width, height)
	}
	nav := m.renderNavigation(navWidth, height)
	report := m.renderReport(width-navWidth-1, height)
	return lipgloss.JoinHorizontal(lipgloss.Top, nav, " ", report)
}

func (m *model) renderSessions(width, height int) string {
	if width >= 120 {
		navWidth, listWidth := 20, max(34, width/3)
		detailWidth := width - navWidth - listWidth - 2
		return lipgloss.JoinHorizontal(lipgloss.Top,
			m.renderNavigation(navWidth, height), " ",
			m.renderSessionList(listWidth, height), " ",
			m.renderTranscript(detailWidth, height))
	}
	if width >= 80 {
		listWidth := max(34, width*2/5)
		return lipgloss.JoinHorizontal(lipgloss.Top,
			m.renderSessionList(listWidth, height), " ",
			m.renderTranscript(width-listWidth-1, height))
	}
	if m.focus == 0 {
		return m.renderNavigation(width, height)
	}
	if m.detail != nil && len(m.messages) > 0 && m.focus == 2 {
		return m.renderTranscript(width, height)
	}
	return m.renderSessionList(width, height)
}

func (m *model) renderNavigation(width, height int) string {
	lines := []string{titleStyle.Render("Views")}
	for i, page := range pages {
		label := "  " + m.strings.PageNames[page]
		if i == m.navIndex {
			label = activeStyle.Width(max(1, width-4)).Render("  " + m.strings.PageNames[page])
		}
		lines = append(lines, truncateWidth(label, width-2))
	}
	lines = append(lines, "", mutedStyle.Render("Filters"))
	lines = append(lines,
		truncateWidth("project  "+dash(m.filter.Project), width-2),
		truncateWidth("agent    "+dash(m.filter.Agent), width-2),
		truncateWidth("machine  "+dash(m.filter.Machine), width-2),
		fmt.Sprintf("one-shot %s", onOff(m.filter.IncludeOneShot)),
		fmt.Sprintf("automated %s", onOff(m.filter.IncludeAutomated)),
		fmt.Sprintf("children  %s", onOff(m.filter.IncludeChildren)),
	)
	return panel(strings.Join(lines, "\n"), width, height, m.focus == 0)
}

func (m *model) renderSessionList(width, height int) string {
	title := "Sessions"
	if m.query.Search != "" {
		title += " / " + safe(m.query.Search)
	}
	lines := []string{titleStyle.Render(truncateWidth(title, width-4))}
	if m.loading && m.itemCount() == 0 {
		lines = append(lines, "", m.strings.Loading)
	}
	if m.query.Search != "" {
		for i, result := range m.searchResults {
			name := result.Name
			if name == "" {
				name = result.SessionID
			}
			line := fmt.Sprintf("%s %s  %s", cursor(i == m.selected), name, result.Agent)
			lines = append(lines, selectedLine(truncateWidth(terminaltext.Sanitize(line), width-4), i == m.selected))
			if result.Snippet != "" {
				lines = append(lines, mutedStyle.Render(truncateWidth("   "+terminaltext.Sanitize(result.Snippet), width-4)))
			}
		}
	} else {
		for i, session := range m.sessions {
			name := sessionTitle(session)
			line := fmt.Sprintf("%s %s", cursor(i == m.selected), name)
			lines = append(lines, selectedLine(truncateWidth(line, width-4), i == m.selected))
			meta := fmt.Sprintf("   %s · %s · %d msgs", safe(session.Agent), safe(session.Project), session.MessageCount)
			lines = append(lines, mutedStyle.Render(truncateWidth(meta, width-4)))
		}
	}
	if m.itemCount() == 0 && !m.loading {
		lines = append(lines, "", m.strings.NoResults)
	}
	return panel(windowLines(lines, max(1, height-2), m.selected*2), width, height, m.focus == 1)
}

func (m *model) renderTranscript(width, height int) string {
	if m.detail == nil {
		return panel(m.strings.NoResults, width, height, m.focus == 2)
	}
	grade := "-"
	if m.detail.HealthGrade != nil {
		grade = *m.detail.HealthGrade
	}
	lines := []string{
		titleStyle.Render(truncateWidth(sessionTitle(m.detail.Session), width-4)),
		mutedStyle.Render(fmt.Sprintf("%s · %s · %d messages · health %s", safe(m.detail.Agent), safe(m.detail.Project), m.detail.MessageCount, safe(grade))),
	}
	if m.detail.Cwd != "" {
		lines = append(lines, mutedStyle.Render(truncateWidth(safe(m.detail.Cwd), width-4)))
	}
	lines = append(lines, m.sessionVitalLines(width)...)
	lines = append(lines, "")
	lineLimit := max(1, height*2)
	if m.transcriptLoading {
		lines = append(lines, m.strings.Loading)
	}
	start := max(0, m.messageSelected-2)
	for i := start; i < len(m.messages); i++ {
		message := m.messages[i]
		if !m.showTools && (message.Role == "tool" || message.Role == "system") {
			continue
		}
		prefix := fmt.Sprintf("%s #%d %s", cursor(i == m.messageSelected), message.Ordinal, strings.ToUpper(message.Role))
		if message.Model != "" {
			prefix += "  " + safe(message.Model)
		}
		lines = append(lines, selectedLine(truncateWidth(prefix, width-4), i == m.messageSelected))
		if len(lines) >= lineLimit {
			break
		}
		content := m.renderMessage(i, message, max(20, width-6))
		lines = appendTextLines(lines, content, lineLimit)
		if m.showTools {
			lines = append(lines, m.toolCallLines(message, width, lineLimit-len(lines))...)
		}
		if len(lines) < lineLimit && m.showThinking && message.HasThinking && message.ThinkingText != "" {
			lines = append(lines, mutedStyle.Render(truncateWidth(
				"thinking: "+terminaltext.Sanitize(firstLine(message.ThinkingText)), width-4,
			)))
		}
		if len(lines) < lineLimit && m.messageLayout != "compact" && m.messageLayout != "skim" {
			lines = append(lines, "")
		}
		if len(lines) >= lineLimit {
			break
		}
	}
	if len(lines) < lineLimit && m.nextMessageOrdinal != nil {
		lines = append(lines, mutedStyle.Render(fmt.Sprintf("n loads more messages · %d loaded", len(m.messages))))
	}
	return panel(windowLines(lines, max(1, height-2), 0), width, height, m.focus == 2)
}

func appendTextLines(lines []string, value string, limit int) []string {
	if len(lines) >= limit {
		return lines
	}
	for line := range strings.SplitSeq(strings.TrimSpace(value), "\n") {
		lines = append(lines, line)
		if len(lines) >= limit {
			break
		}
	}
	return lines
}

func (m *model) renderMessage(index int, message db.Message, width int) string {
	raw := message.Content
	if m.messageLayout == "skim" {
		raw = firstLine(raw)
	}
	if cached, ok := m.renderedMessages[index]; ok &&
		cached.content == raw && cached.width == width && cached.theme == m.theme && cached.layout == m.messageLayout {
		return cached.rendered
	}
	rendered := renderMarkdown(raw, width, m.theme)
	if m.renderedMessages == nil {
		m.renderedMessages = make(map[int]renderedMessage)
	}
	m.renderedMessages[index] = renderedMessage{
		content: raw, rendered: rendered, width: width, theme: m.theme, layout: m.messageLayout,
	}
	return rendered
}

func (m *model) toolCallLines(message db.Message, width, limit int) []string {
	var lines []string
	for _, call := range message.ToolCalls {
		if len(lines) >= limit {
			break
		}
		header := "tool " + call.ToolName
		if call.Category != "" {
			header += " · " + call.Category
		}
		if call.SkillName != "" {
			header += " · skill " + call.SkillName
		}
		lines = append(lines, mutedStyle.Render(truncateWidth(terminaltext.Sanitize(header), width-4)))
		if call.InputJSON != "" {
			lines = append(lines, m.toolPayloadLines("input", call.InputJSON, width, limit-len(lines))...)
		}
		if call.ResultContent != "" {
			lines = append(lines, m.toolPayloadLines("result", call.ResultContent, width, limit-len(lines))...)
		}
		for _, event := range call.ResultEvents {
			if event.Content == "" || len(lines) >= limit {
				continue
			}
			label := "event"
			if event.Status != "" {
				label += " " + event.Status
			}
			lines = append(lines, m.toolPayloadLines(label, event.Content, width, limit-len(lines))...)
		}
	}
	return lines
}

func (m *model) toolPayloadLines(label, value string, width, limit int) []string {
	if limit <= 0 {
		return nil
	}
	if m.messageLayout == "compact" || m.messageLayout == "skim" {
		return []string{mutedStyle.Render(truncateWidth(
			"  "+label+": "+terminaltext.Sanitize(firstLine(value)), width-4,
		))}
	}
	lines := []string{mutedStyle.Render("  " + label + ":")}
	if len(lines) >= limit {
		return lines
	}
	for line := range strings.SplitSeq(value, "\n") {
		lines = append(lines, mutedStyle.Render(truncateWidth("    "+terminaltext.Sanitize(line), width-4)))
		if len(lines) >= limit {
			break
		}
	}
	return lines
}

func (m *model) sessionVitalLines(width int) []string {
	d := m.detail
	if d == nil {
		return nil
	}
	lines := []string{
		mutedStyle.Render(truncateWidth(fmt.Sprintf(
			"outcome %s (%s) · failures %d · retries %d · edit churn %d · compactions %d/%d",
			d.Outcome, d.OutcomeConfidence, d.ToolFailureSignalCount, d.ToolRetryCount,
			d.EditChurnCount, d.CompactionCount, d.MidTaskCompactionCount,
		), width-4)),
	}
	if d.HasTotalOutputTokens || d.HasPeakContextTokens {
		lines = append(lines, mutedStyle.Render(fmt.Sprintf(
			"output %d tokens · peak context %d", d.TotalOutputTokens, d.PeakContextTokens,
		)))
	}
	if d.ContextPressureMax != nil {
		lines = append(lines, mutedStyle.Render(fmt.Sprintf("context pressure %.0f%%", *d.ContextPressureMax*100)))
	}
	if usage := m.extras.Usage; usage != nil {
		cost := "unpriced"
		if usage.HasCost {
			cost = fmt.Sprintf("$%.4f", dollarsFloat(usage.Cost))
		}
		lines = append(lines, mutedStyle.Render(truncateWidth(
			fmt.Sprintf("usage %s · models %s", cost, safe(strings.Join(usage.Models, ", "))), width-4,
		)))
	}
	if timing := m.extras.Timing; timing != nil {
		lines = append(lines, mutedStyle.Render(fmt.Sprintf(
			"duration %s · tool time %s · %d turns · %d calls · %d subagents",
			formatMilliseconds(timing.TotalDurationMs), formatMilliseconds(timing.ToolDurationMs),
			timing.TurnCount, timing.ToolCallCount, timing.SubagentCount,
		)))
	}
	if activity := m.extras.Activity; activity != nil && len(activity.Buckets) > 0 {
		values := make([]int, len(activity.Buckets))
		for i, bucket := range activity.Buckets {
			values[i] = bucket.UserCount + bucket.AssistantCount
		}
		lines = append(lines, mutedStyle.Render("activity "+sparkline(values)))
	}
	return lines
}

func renderMarkdown(raw string, width int, theme string) string {
	safe := terminaltext.Sanitize(raw)
	style := "dark"
	if theme == "light" {
		style = "light"
	}
	renderer, err := glamour.NewTermRenderer(glamour.WithStandardStyle(style), glamour.WithWordWrap(width))
	if err != nil {
		return safe
	}
	rendered, err := renderer.Render(safe)
	if err != nil {
		return safe
	}
	return rendered
}

func (m *model) renderReport(width, height int) string {
	lines := m.reportLines()
	if m.loading && len(lines) == 0 {
		lines = []string{m.strings.Loading}
	}
	if len(lines) == 0 {
		lines = []string{m.strings.NoResults}
	}
	return panel(windowLines(lines, max(1, height-2), m.scroll), width, height, m.focus == 1)
}

func (m *model) analyticsLines() []string {
	a := m.pageData.Analytics
	if a == nil {
		return nil
	}
	lines := []string{titleStyle.Render("Analytics dashboard"), "",
		metric("Sessions", a.TotalSessions), metric("Messages", a.TotalMessages), metric("Output tokens", a.TotalOutputTokens),
		fmt.Sprintf("Active projects  %d", a.ActiveProjects), fmt.Sprintf("Active days      %d", a.ActiveDays),
		fmt.Sprintf("Messages/session avg %.1f  median %d  p90 %d", a.AvgMessages, a.MedianMessages, a.P90Messages),
		fmt.Sprintf("Most active      %s", dash(a.MostActive)), "", titleStyle.Render("Agents"),
	}
	keys := sortedKeys(a.Agents)
	for _, key := range keys {
		row := a.Agents[key]
		if row == nil {
			continue
		}
		lines = append(lines, fmt.Sprintf("%-18s %6d sessions  %8d messages", safe(key), row.Sessions, row.Messages))
	}
	if series := m.pageData.AnalyticsSeries; series != nil {
		values := make([]int, len(series.Series))
		for i, point := range series.Series {
			values[i] = point.Messages
		}
		lines = append(lines, "", titleStyle.Render("Activity · "+safe(series.Granularity)), sparkline(values))
	}
	if heatmap := m.pageData.Heatmap; heatmap != nil {
		values := make([]int, len(heatmap.Entries))
		for i, entry := range heatmap.Entries {
			values[i] = entry.Value
		}
		lines = append(lines, "", titleStyle.Render("Daily "+safe(heatmap.Metric)), sparkline(values))
	}
	if projects := m.pageData.Projects; projects != nil {
		lines = append(lines, "", titleStyle.Render("Projects"))
		for _, project := range projects.Projects {
			lines = append(lines, fmt.Sprintf("%-28s %6d sessions  %8d messages  trend %+.1f", truncateWidth(safe(project.Name), 28), project.Sessions, project.Messages, project.DailyTrend))
		}
	}
	if hours := m.pageData.HourOfWeek; hours != nil {
		dayTotals := make([]int, 7)
		for _, cell := range hours.Cells {
			if cell.DayOfWeek >= 0 && cell.DayOfWeek < len(dayTotals) {
				dayTotals[cell.DayOfWeek] += cell.Messages
			}
		}
		lines = append(lines, "", titleStyle.Render("Hour-of-week totals · Mon → Sun"), sparkline(dayTotals))
	}
	if shape := m.pageData.SessionShape; shape != nil {
		lines = append(lines, "", titleStyle.Render(fmt.Sprintf("Session shape · %d sessions", shape.Count)))
		lines = append(lines, "length    "+distributionLine(shape.LengthDistribution))
		lines = append(lines, "duration  "+distributionLine(shape.DurationDistribution))
		lines = append(lines, "autonomy  "+distributionLine(shape.AutonomyDistribution))
	}
	if velocity := m.pageData.Velocity; velocity != nil {
		lines = append(lines, "", titleStyle.Render("Velocity"),
			fmt.Sprintf("turn cycle p50 %.1fs / p90 %.1fs", velocity.Overall.TurnCycleSec.P50, velocity.Overall.TurnCycleSec.P90),
			fmt.Sprintf("first response p50 %.1fs / p90 %.1fs", velocity.Overall.FirstResponseSec.P50, velocity.Overall.FirstResponseSec.P90),
			fmt.Sprintf("%.2f messages/min · %.2f tool calls/min", velocity.Overall.MsgsPerActiveMin, velocity.Overall.ToolCallsPerActiveMin))
	}
	if tools := m.pageData.Tools; tools != nil {
		lines = append(lines, "", titleStyle.Render(fmt.Sprintf("Tools · %d calls", tools.TotalCalls)))
		for i, tool := range tools.ByTool {
			if i == 10 {
				break
			}
			lines = append(lines, fmt.Sprintf("%-24s %7d calls  %5.1f%%", truncateWidth(safe(tool.ToolName), 24), tool.CallCount, tool.Pct))
		}
	}
	if skills := m.pageData.Skills; skills != nil {
		lines = append(lines, "", titleStyle.Render(fmt.Sprintf("Skills · %d calls / %d distinct", skills.TotalSkillCalls, skills.DistinctSkills)))
		for i, skill := range skills.BySkill {
			if i == 10 {
				break
			}
			lines = append(lines, fmt.Sprintf("%-24s %7d calls  %5.1f%%", truncateWidth(safe(skill.SkillName), 24), skill.CallCount, skill.Pct))
		}
	}
	if signals := m.pageData.Signals; signals != nil {
		avg := "-"
		if signals.AvgHealthScore != nil {
			avg = fmt.Sprintf("%.1f", *signals.AvgHealthScore)
		}
		lines = append(lines, "", titleStyle.Render("Session health"),
			fmt.Sprintf("scored %d · unscored %d · average %s", signals.ScoredSessions, signals.UnscoredSessions, avg),
			fmt.Sprintf("failures %d · retries %d · edit churn %d · failure rate %.1f%%", signals.ToolHealth.TotalFailureSignals, signals.ToolHealth.TotalRetries, signals.ToolHealth.TotalEditChurn, signals.ToolHealth.FailureRate*100),
			fmt.Sprintf("compacted %d · mid-task %d · high pressure %d", signals.ContextHealth.SessionsWithCompaction, signals.ContextHealth.MidTaskCompactionCount, signals.ContextHealth.HighPressureSessions))
	}
	if top := m.pageData.TopSessions; top != nil {
		lines = append(lines, "", titleStyle.Render("Top sessions · "+safe(top.Metric)))
		for _, session := range top.Sessions {
			lines = append(lines, fmt.Sprintf("%-28s %6d messages  %8d output  %6.1fm", truncateWidth(sessionTitle(db.Session{ID: session.ID, Project: session.Project, DisplayName: session.DisplayName, FirstMessage: session.FirstMessage}), 28), session.MessageCount, session.OutputTokens, session.DurationMin))
		}
	}
	return lines
}

func (m *model) usageLines() []string {
	u := m.pageData.Usage
	if u == nil {
		if len(m.pageData.UsageTopSessions) == 0 {
			return nil
		}
		return append([]string{titleStyle.Render("Usage"), "", titleStyle.Render("Top sessions by cost")}, m.usageTopSessionLines()...)
	}
	lines := []string{titleStyle.Render("Usage"), mutedStyle.Render(u.From + " → " + u.To), "",
		fmt.Sprintf("Total cost          %s", formatMoney(u.Totals.TotalCost)), metric("Input tokens", u.Totals.InputTokens), metric("Output tokens", u.Totals.OutputTokens),
		metric("Cache read tokens", u.Totals.CacheReadTokens), fmt.Sprintf("Cache savings       %s", formatMoney(u.Totals.CacheSavings)),
		fmt.Sprintf("Cache hit rate      %.1f%%", u.CacheStats.HitRate*100), "", titleStyle.Render("Daily cost"),
	}
	dailyCosts := make([]float64, len(u.Daily))
	for i, day := range u.Daily {
		dailyCosts[i] = dollarsFloat(day.TotalCost)
	}
	lines = append(lines, sparklineFloat(dailyCosts))
	if comparison := m.pageData.UsageComparison; comparison != nil {
		lines = append(lines, fmt.Sprintf("prior %s → %s  %s  delta %+.1f%%", comparison.PriorFrom, comparison.PriorTo, formatMoney(comparison.PriorTotalCost), comparison.DeltaPct*100))
	}
	lines = append(lines, "", titleStyle.Render("Models"))
	for _, row := range u.ModelTotals {
		lines = append(lines, fmt.Sprintf("%-28s %9s  %10d out", truncateWidth(safe(row.Model), 28), formatMoney(row.Cost), row.OutputTokens))
	}
	lines = append(lines, "", titleStyle.Render("Projects"))
	for _, row := range u.ProjectTotals {
		lines = append(lines, fmt.Sprintf("%-28s %9s  %10d out", truncateWidth(safe(row.Project), 28), formatMoney(row.Cost), row.OutputTokens))
	}
	lines = append(lines, "", titleStyle.Render("Agents"))
	for _, row := range u.AgentTotals {
		lines = append(lines, fmt.Sprintf("%-28s %9s  %10d out", truncateWidth(safe(row.Agent), 28), formatMoney(row.Cost), row.OutputTokens))
	}
	if pair := m.pageData.UsagePairwise; pair != nil {
		lines = append(lines, "", titleStyle.Render("Pairwise comparison"),
			fmt.Sprintf("left  %s · %d tokens · %d sessions", formatMoney(pair.Left.TotalCost), pair.Left.TotalTokens, pair.Left.SessionCount),
			fmt.Sprintf("right %s · %d tokens · %d sessions", formatMoney(pair.Right.TotalCost), pair.Right.TotalTokens, pair.Right.SessionCount),
			fmt.Sprintf("delta %s · %d tokens · %d sessions", formatMoney(pair.Deltas.TotalCostDelta), pair.Deltas.TotalTokensDelta, pair.Deltas.SessionCountDelta))
	}
	lines = append(lines, "", titleStyle.Render("Top sessions by cost"))
	lines = append(lines, m.usageTopSessionLines()...)
	lines = append(lines, "", mutedStyle.Render(":compare model|LEFT|RIGHT or :compare project|LEFT|RIGHT"))
	return lines
}

func (m *model) usageTopSessionLines() []string {
	lines := make([]string, 0, len(m.pageData.UsageTopSessions))
	for _, session := range m.pageData.UsageTopSessions {
		lines = append(lines, fmt.Sprintf("%-28s %-10s %8s  %10d tokens", truncateWidth(safe(session.DisplayName), 28), safe(session.Agent), formatMoney(session.Cost), session.TotalTokens))
	}
	return lines
}

func (m *model) activityLines() []string {
	a := m.pageData.Activity
	if a == nil {
		return nil
	}
	lines := []string{titleStyle.Render("Activity"), mutedStyle.Render(a.RangeStart + " → " + a.RangeEnd), "",
		fmt.Sprintf("Sessions       %d (%d automated / %d interactive)", a.Totals.Sessions, a.Totals.AutomatedSessions, a.Totals.InteractiveSessions),
		fmt.Sprintf("Active minutes %.1f", a.Totals.ActiveMinutes), fmt.Sprintf("Agent minutes  %.1f", a.Totals.AgentMinutes),
		fmt.Sprintf("Peak agents    %d", a.Peak.Agents), fmt.Sprintf("Output tokens  %d", a.Totals.OutputTokens), fmt.Sprintf("Cost           %s", formatMoney(a.Totals.Cost)),
		"", titleStyle.Render("Timeline"), sparklineActivity(a.Buckets), "", titleStyle.Render("Projects"),
	}
	for _, row := range a.ByProject {
		lines = append(lines, fmt.Sprintf("%-28s %8.1f min  %8s", truncateWidth(safe(row.Key), 28), row.AgentMinutes, formatMoney(row.Cost)))
	}
	lines = append(lines, "", titleStyle.Render("Models"))
	for _, row := range a.ByModel {
		lines = append(lines, fmt.Sprintf("%-28s %8.1f min  %8s", truncateWidth(safe(row.Key), 28), row.AgentMinutes, formatMoney(row.Cost)))
	}
	lines = append(lines, "", titleStyle.Render("Agents"))
	for _, row := range a.ByAgent {
		lines = append(lines, fmt.Sprintf("%-28s %8.1f min  %8s", truncateWidth(safe(row.Key), 28), row.AgentMinutes, formatMoney(row.Cost)))
	}
	lines = append(lines, "", titleStyle.Render("Sessions"))
	for _, row := range a.BySession {
		minutes := "untimed"
		if row.AgentMinutes != nil {
			minutes = fmt.Sprintf("%.1f min", *row.AgentMinutes)
		}
		lines = append(lines, fmt.Sprintf("%-28s %-12s %8s  %8d out", truncateWidth(safe(row.Title), 28), minutes, formatMoney(row.Cost), row.OutputTokens))
	}
	return lines
}

func formatMoney(value money.Money) string {
	return money.FormatUSD(value, money.DisplayCents)
}

func dollarsFloat(value money.Money) float64 {
	return float64(value.Microdollars) / 1_000_000
}

func (m *model) trendsLines() []string {
	t := m.pageData.Trends
	if t == nil {
		return []string{titleStyle.Render("Trends"), mutedStyle.Render("Press / to enter comma-separated terms.")}
	}
	lines := []string{titleStyle.Render("Trends"), mutedStyle.Render(safe(t.From) + " → " + safe(t.To) + " · " + safe(t.Granularity)), fmt.Sprintf("Messages %d", t.MessageCount), ""}
	for _, series := range t.Series {
		lines = append(lines, fmt.Sprintf("%-24s %7d  %s", safe(series.Term), series.Total, sparklineTrend(series.Points)))
	}
	if len(t.Series) == 0 {
		lines = append(lines, "Press / to enter comma-separated terms.")
	}
	return lines
}

func (m *model) insightLines() []string {
	lines := []string{titleStyle.Render("Insights"), mutedStyle.Render(":publish-insight [ID]  :publish-insight-secret [ID]  :delete-insight [ID]"), mutedStyle.Render(":export-insight-html ID[|PATH]  :export-insight-markdown ID[|PATH]"), ""}
	for i, item := range m.pageData.Insights {
		project := "all projects"
		if item.Project != nil {
			project = *item.Project
		}
		header := fmt.Sprintf("%s #%d  %s  %s → %s  %s", cursor(i == m.selected), item.ID, safe(item.Type), safe(item.DateFrom), safe(item.DateTo), safe(project))
		lines = append(lines, selectedLine(header, i == m.selected))
		if i == m.selected {
			lines = append(lines, strings.Split(strings.TrimSpace(renderMarkdown(item.Content, 100, m.theme)), "\n")...)
		} else {
			lines = append(lines, mutedStyle.Render(truncateWidth(terminaltext.Sanitize(firstLine(item.Content)), 100)))
		}
		lines = append(lines, "")
	}
	return lines
}

func (m *model) pinLines() []string {
	lines := []string{titleStyle.Render("Pinned messages"), ""}
	for i, pin := range m.pageData.Pins {
		role := ""
		if pin.Role != nil {
			role = *pin.Role
		}
		note := ""
		if pin.Note != nil {
			note = " · " + safe(*pin.Note)
		}
		header := fmt.Sprintf("%s %s #%d %s%s", cursor(i == m.selected), safe(pin.SessionID), pin.Ordinal, safe(role), note)
		lines = append(lines, selectedLine(header, i == m.selected))
		if pin.Content != nil {
			if i == m.selected {
				lines = append(lines, strings.Split(strings.TrimSpace(renderMarkdown(*pin.Content, 100, m.theme)), "\n")...)
			} else {
				lines = append(lines, mutedStyle.Render(truncateWidth(terminaltext.Sanitize(firstLine(*pin.Content)), 100)))
			}
		}
	}
	return lines
}

func (m *model) trashLines() []string {
	lines := []string{titleStyle.Render("Trash"), mutedStyle.Render(":restore  :delete-permanent  :empty-trash"), ""}
	for i, session := range m.pageData.Trash {
		lines = append(lines, selectedLine(fmt.Sprintf("%s %s  %s", cursor(i == m.selected), sessionTitle(session), safe(session.Agent)), i == m.selected))
	}
	return lines
}

func (m *model) recentEditLines() []string {
	r := m.pageData.RecentEdits
	lines := []string{titleStyle.Render("Recent edits"), mutedStyle.Render("Press / to filter file paths."), ""}
	if r == nil {
		return lines
	}
	for i, file := range r.Files {
		header := fmt.Sprintf("%s %s  %s", cursor(i == m.selected), safe(file.Project), safe(file.FilePath))
		lines = append(lines, selectedLine(header, i == m.selected), mutedStyle.Render(fmt.Sprintf("  %d edits · %s · %s", file.EditCount, safe(file.LastSessionID), safe(shortTime(file.LastEditedAt)))))
		if i == m.selected {
			for _, edit := range file.Edits {
				lines = append(lines, mutedStyle.Render(fmt.Sprintf("    #%d %-8s %s", edit.Ordinal, safe(edit.ToolName), safe(shortTime(edit.Timestamp)))))
			}
		}
	}
	return lines
}

func (m *model) settingsLines() []string {
	s := m.pageData.Settings
	if s == nil {
		return nil
	}
	terminal := s.Terminal.Mode
	if s.Terminal.Mode == "custom" {
		terminal += " · " + safe(s.Terminal.CustomBin) + " " + safe(s.Terminal.CustomArgs)
	}
	lines := []string{
		titleStyle.Render("Settings"), "",
		fmt.Sprintf("Daemon          %s:%d", safe(s.Host), s.Port),
		fmt.Sprintf("Authentication  %s", onOff(s.RequireAuth)),
		fmt.Sprintf("GitHub          %s", configured(s.GithubConfigured)),
		fmt.Sprintf("Storage         %s", map[bool]string{true: "read-only", false: "read-write"}[s.ReadOnly]),
		fmt.Sprintf("Terminal        %s", safe(terminal)),
		fmt.Sprintf("Theme           %s", m.theme),
		fmt.Sprintf("High contrast   %s", onOff(m.highContrast)),
		fmt.Sprintf("Message layout  %s", m.messageLayout),
		fmt.Sprintf("Thinking blocks %s", onOff(m.showThinking)),
		fmt.Sprintf("Tool blocks     %s", onOff(m.showTools)),
		"", titleStyle.Render("Agent directories"),
	}
	for _, agent := range sortedKeys(s.AgentDirs) {
		dirs := s.AgentDirs[agent]
		lines = append(lines, fmt.Sprintf("%-18s %s", safe(agent), safe(strings.Join(dirs, ", "))))
	}
	if m.pageData.Mappings != nil {
		lines = append(lines, "", titleStyle.Render("Worktree mappings · "+safe(m.pageData.Mappings.Machine)))
		for _, mapping := range m.pageData.Mappings.Mappings {
			lines = append(lines, fmt.Sprintf("#%d  %-10s %-3s %s → %s", mapping.ID, safe(mapping.Layout), onOff(mapping.Enabled), safe(mapping.PathPrefix), safe(mapping.Project)))
		}
	}
	if m.pageData.Embeddings != nil {
		lines = append(lines, "", titleStyle.Render("Embedding generations"))
		for _, generation := range m.pageData.Embeddings.Generations {
			lines = append(lines, fmt.Sprintf("#%d  %-10s %s", generation.ID, safe(generation.State), safe(generation.Model)))
		}
	}
	lines = append(lines, "", mutedStyle.Render(":theme  :contrast  :layout  :thinking  :tools"), mutedStyle.Render(":sync  :resync  :sync-remote HOST [force]"), mutedStyle.Render(":terminal auto|clipboard or :terminal custom|BIN|ARGS"), mutedStyle.Render(":github-token  :require-auth on|off"), mutedStyle.Render(":worktree-add layout|path|project[|enabled]"), mutedStyle.Render(":worktree-update id|layout|path|project|enabled  :worktree-delete ID"), mutedStyle.Render(":embeddings-build  :embeddings-activate ID  :embeddings-retire ID [force]"))
	return lines
}

func (m *model) renderHelp(width, height int) string {
	text := []string{titleStyle.Render(m.strings.Help), "", "Navigation", "  tab / shift+tab     change pane", "  j/k or arrows       move selection", "  enter               open selection", "  n/p                 next/previous page; n loads more transcript rows", "  [ / ]               previous/next transcript search match", "  r                   reload", "", "Sessions", "  /query              search sessions", "  :find QUERY         search inside the selected transcript", "  s                   star session", "  d                   move session to trash", "  :rename NAME        rename session", "  :open-session       open its directory", "  :resume-session     resume in configured terminal", "  :pin NOTE           pin selected message", "  :publish-session    publish an HTML gist", "  :export-html PATH   export; omit path to open a temporary file", "  :export-markdown PATH", "  :open-link URL      hand an image, Mermaid URL, or link to the OS", "", "Filters and reports", "  :project / :exclude-project / :agent / :exclude-agent / :machine", "  :model / :exclude-model / :branch", "  :date / :from / :to / :active-since", "  :min-messages / :max-messages / :min-user-messages / :min-failures", "  :outcome / :health / :termination / :sort", "  :has-secret on|off  :starred on|off", "  :one-shot on|off    :automated on|off    :children on|off", "  /terms              set Trends terms on Trends", "  :compare model|LEFT|RIGHT or project|LEFT|RIGHT", "  :generate-insight type|from|to|project", "  :publish-insight [ID] / :publish-insight-secret [ID]", "  :delete-insight [ID]", "  :export-insight-html ID[|PATH] / :export-insight-markdown ID[|PATH]", "", "Appearance", "  :theme auto|dark|light", "  :contrast on|off", "  :layout default|compact|stream|skim", "  :thinking on|off    :tools on|off", "  Text size is controlled by the terminal.", "", "Global actions", "  :sync / :resync / :sync-remote HOST [force]", "  :import-claude PATH / :import-chatgpt PATH", "  :terminal auto|clipboard or :terminal custom|BIN|ARGS", "  :github-token / :require-auth on|off", "  :embeddings-build / :embeddings-activate ID / :embeddings-retire ID", "  :worktree-add layout|path|project[|enabled]", "  :worktree-update id|layout|path|project|enabled / :worktree-delete ID", "  :worktrees-apply", "", "Press ? or Esc to close."}
	return panel(windowLines(text, max(1, height-2), 0), width, height, true)
}

func (m *model) renderConfirm(width, height int) string {
	return panel("Confirm "+m.confirm.Kind+"?\n\nPress y to continue or n to cancel.", width, height, true)
}

func panel(content string, width, height int, focused bool) string {
	style := borderStyle.Width(max(1, width-2)).Height(max(1, height-2))
	if focused {
		style = style.BorderForeground(colorAccent)
	}
	return style.Render(fitHeight(content, max(1, height-2)))
}

func fitHeight(content string, height int) string {
	lines := strings.Split(content, "\n")
	if len(lines) > height {
		lines = lines[:height]
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

func windowLines(lines []string, height, anchor int) string {
	if len(lines) <= height {
		return strings.Join(lines, "\n")
	}
	start := clamp(anchor-height/3, 0, len(lines)-height)
	return strings.Join(lines[start:start+height], "\n")
}

func truncateWidth(value string, width int) string {
	if width <= 0 {
		return ""
	}
	return ansi.Truncate(value, width, "…")
}

func cursor(selected bool) string {
	if selected {
		return ">"
	}
	return " "
}
func selectedLine(line string, selected bool) string {
	if selected {
		return activeStyle.Render(line)
	}
	return line
}
func dash(value string) string {
	if value == "" {
		return "-"
	}
	return terminaltext.Sanitize(value)
}

func safe(value string) string { return terminaltext.Sanitize(value) }
func onOff(value bool) string {
	if value {
		return "on"
	}
	return "off"
}
func configured(value bool) string {
	if value {
		return "configured"
	}
	return "not configured"
}
func metric(label string, value int) string { return fmt.Sprintf("%-18s %12d", label, value) }

func formatMilliseconds(value int64) string {
	duration := time.Duration(value) * time.Millisecond
	if duration < time.Second {
		return duration.String()
	}
	return duration.Round(time.Second).String()
}

func distributionLine(buckets []db.DistributionBucket) string {
	parts := make([]string, 0, len(buckets))
	for _, bucket := range buckets {
		parts = append(parts, fmt.Sprintf("%s:%d", safe(bucket.Label), bucket.Count))
	}
	return strings.Join(parts, "  ")
}

func sessionTitle(session db.Session) string {
	if session.DisplayName != nil && strings.TrimSpace(*session.DisplayName) != "" {
		return terminaltext.Sanitize(*session.DisplayName)
	}
	if session.FirstMessage != nil && strings.TrimSpace(*session.FirstMessage) != "" {
		return terminaltext.Sanitize(firstLine(*session.FirstMessage))
	}
	return session.ID
}

func firstLine(value string) string {
	if line, _, ok := strings.Cut(value, "\n"); ok {
		return line
	}
	return value
}
func shortTime(value string) string {
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t.Format("2006-01-02 15:04")
	}
	return value
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sparklineActivity(points []activity.Bucket) string {
	values := make([]int, len(points))
	for i, point := range points {
		values[i] = point.MaxAgents
	}
	return sparkline(values)
}

func sparklineTrend(points []db.TrendPoint) string {
	values := make([]int, len(points))
	for i, point := range points {
		values[i] = point.Count
	}
	return sparkline(values)
}

func sparkline(values []int) string {
	const bars = "▁▂▃▄▅▆▇█"
	if len(values) == 0 {
		return "-"
	}
	maximum := 0
	for _, value := range values {
		maximum = max(maximum, value)
	}
	if maximum == 0 {
		return strings.Repeat("▁", len(values))
	}
	var b strings.Builder
	for _, value := range values {
		index := value * (len([]rune(bars)) - 1) / maximum
		b.WriteRune([]rune(bars)[index])
	}
	return b.String()
}

func sparklineFloat(values []float64) string {
	const bars = "▁▂▃▄▅▆▇█"
	if len(values) == 0 {
		return "-"
	}
	maximum := 0.0
	for _, value := range values {
		maximum = max(maximum, value)
	}
	if maximum == 0 {
		return strings.Repeat("▁", len(values))
	}
	barRunes := []rune(bars)
	var b strings.Builder
	for _, value := range values {
		index := int(value * float64(len(barRunes)-1) / maximum)
		b.WriteRune(barRunes[index])
	}
	return b.String()
}
