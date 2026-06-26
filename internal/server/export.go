package server

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// gistResponse represents the relevant fields from GitHub's
// Create Gist API response.
type gistResponse struct {
	ID      string `json:"id"`
	HTMLURL string `json:"html_url"`
	Owner   struct {
		Login string `json:"login"`
	} `json:"owner"`
}

var ghAuthTokenOutput = func(ctx context.Context) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", "auth", "token")
	cmd.Stderr = io.Discard
	return cmd.Output()
}

func githubHTTPClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = base.Clone()
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
	}
}

func createGist(
	ctx context.Context,
	token, filename, description, content string,
	public bool,
) (*gistResponse, error) {
	return createGistWithURL(
		ctx,
		"https://api.github.com/gists",
		token, filename, description, content, public,
	)
}

func createGistWithURL(
	ctx context.Context,
	apiURL, token, filename, description, content string,
	public bool,
) (*gistResponse, error) {
	payload, err := json.Marshal(map[string]any{
		"description": description,
		"public":      public,
		"files": map[string]any{
			filename: map[string]string{"content": content},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("marshaling gist payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		apiURL,
		strings.NewReader(string(payload)))
	if err != nil {
		return nil, fmt.Errorf("creating gist request: %w", err)
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "agentsview")

	client := githubHTTPClient(30 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, err := io.ReadAll(io.LimitReader(resp.Body, 512))
		if err != nil {
			return nil, fmt.Errorf("github API error: %d: reading body: %w", resp.StatusCode, err)
		}
		return nil, fmt.Errorf("github API error: %d: %s",
			resp.StatusCode, string(body))
	}

	var result gistResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("parsing github response: %w", err)
	}
	return &result, nil
}

func resolveGitHubToken(ctx context.Context, configured string) string {
	if token := strings.TrimSpace(configured); token != "" {
		return token
	}
	if !isLocalhostContext(ctx) {
		return ""
	}
	if token := strings.TrimSpace(os.Getenv("AGENTSVIEW_GITHUB_TOKEN")); token != "" {
		return token
	}

	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := ghAuthTokenOutput(cctx)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func validateGithubToken(ctx context.Context, token string) (string, error) {
	return validateGithubTokenWithURL(
		ctx, "https://api.github.com/user", token,
	)
}

func validateGithubTokenWithURL(
	ctx context.Context,
	apiURL, token string,
) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return "", fmt.Errorf("creating validation request: %w", err)
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "agentsview")

	client := githubHTTPClient(10 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("validating token: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 401 {
		return "", fmt.Errorf("invalid GitHub token")
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("GitHub API error: %d", resp.StatusCode)
	}

	var user struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return "", fmt.Errorf("parsing user response: %w", err)
	}
	return user.Login, nil
}

type exportData struct {
	Project      string
	Agent        string
	MessageCount int
	StartedAt    string
	Messages     []exportMessage
}

type exportMessage struct {
	Ordinal       int
	RoleClass     string
	ExtraClass    string
	Role          string
	Timestamp     string
	ContentHTML   template.HTML
	FocusedHidden bool
}

type insightExportData struct {
	Title       string
	Type        string
	Project     string
	DateRange   string
	Agent       string
	Model       string
	CreatedAt   string
	ContentHTML template.HTML
}

var exportTmpl = template.Must(
	template.New("export").Parse(exportTemplateStr))

var insightExportTmpl = template.Must(
	template.New("insight-export").Parse(insightExportTemplateStr))

const exportTemplateStr = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>{{.Project}} - Agent Session</title>
<style>
:root {
  --bg-primary: #f7f7fa;
  --bg-surface: #ffffff;
  --bg-inset: #edeef3;
  --border-default: #dfe1e8;
  --border-muted: #e8eaf0;
  --text-primary: #1a1d26;
  --text-secondary: #5a6070;
  --text-muted: #8b92a0;
  --accent-blue: #2563eb;
  --accent-rose: #e11d48;
  --accent-purple: #7c3aed;
  --accent-amber: #d97706;
  --accent-green: #059669;
  --accent-coral: #f34e3f;
  --accent-black: #2d2d2d;
  --accent-teal: #0d9488;
  --accent-red: #dc2626;
  --accent-indigo: #6366f1;
  --accent-lime: #65a30d;
  --user-bg: #eef2ff;
  --assistant-bg: #faf9ff;
  --thinking-bg: #f5f3ff;
  --tool-bg: #fffbf0;
  --code-bg: #1e1e2e;
  --code-text: #cdd6f4;
  --radius-sm: 4px;
  --radius-md: 6px;
  --font-sans: -apple-system, BlinkMacSystemFont, "Segoe UI",
    "Noto Sans", Helvetica, Arial, sans-serif;
  --font-mono: "JetBrains Mono", "SF Mono", "Fira Code",
    "Fira Mono", Menlo, Consolas, monospace;
  color-scheme: light;
}
:root.dark {
  --bg-primary: #0c0c10;
  --bg-surface: #15151b;
  --bg-inset: #101015;
  --border-default: #2a2a35;
  --border-muted: #222230;
  --text-primary: #e2e4e9;
  --text-secondary: #9ca3af;
  --text-muted: #6b7280;
  --accent-blue: #60a5fa;
  --accent-rose: #fb7185;
  --accent-purple: #a78bfa;
  --accent-amber: #fbbf24;
  --accent-green: #34d399;
  --accent-coral: #f34e3f;
  --accent-black: #b0b0b0;
  --accent-teal: #2dd4bf;
  --accent-red: #f87171;
  --accent-indigo: #818cf8;
  --accent-lime: #a3e635;
  --user-bg: #111827;
  --assistant-bg: #141220;
  --thinking-bg: #1a1530;
  --tool-bg: #1a1508;
  --code-bg: #0d0d14;
  --code-text: #cdd6f4;
  color-scheme: dark;
}
* { box-sizing: border-box; margin: 0; padding: 0; }
body {
  font-family: var(--font-sans);
  font-size: 14px;
  background: var(--bg-primary);
  color: var(--text-primary);
  line-height: 1.5;
  -webkit-font-smoothing: antialiased;
  -moz-osx-font-smoothing: grayscale;
}
header {
  background: var(--bg-surface);
  border-bottom: 1px solid var(--border-default);
  padding: 12px 24px;
  position: sticky; top: 0; z-index: 100;
}
.header-content {
  max-width: 900px; margin: 0 auto;
  display: flex; align-items: center;
  justify-content: space-between; gap: 12px;
}
h1 { font-size: 14px; font-weight: 600; }
.session-meta {
  font-size: 11px; color: var(--text-muted);
  display: flex; gap: 12px;
}
.controls { display: flex; gap: 8px; }
main { max-width: 900px; margin: 0 auto; padding: 16px; }
.messages {
  display: flex; flex-direction: column; gap: 8px;
}
.message {
  border-left: 4px solid;
  padding: 14px 20px;
  border-radius: 0 var(--radius-md) var(--radius-md) 0;
}
.message.user {
  background: var(--user-bg);
  border-left-color: var(--accent-blue);
}
.message.assistant {
  background: var(--assistant-bg);
  border-left-color: var(--accent-purple);
}
.message-header {
  display: flex; align-items: center; gap: 8px;
  margin-bottom: 10px;
}
.message-role {
  font-size: 13px; font-weight: 600;
  letter-spacing: 0.01em;
}
.message.user .message-role { color: var(--accent-blue); }
.message.assistant .message-role {
  color: var(--accent-purple);
}
.message-time {
  font-size: 12px; color: var(--text-muted);
}
.message-content {
  font-size: 14px; line-height: 1.7;
  color: var(--text-primary);
  white-space: pre-wrap; word-break: break-word;
}
.message-content pre {
  background: var(--code-bg);
  color: var(--code-text);
  border-radius: var(--radius-md);
  padding: 12px 16px; overflow-x: auto;
  margin: 0.5em 0;
}
.message-content code {
  font-family: var(--font-mono); font-size: 0.85em;
  background: var(--bg-inset);
  border: 1px solid var(--border-muted);
  border-radius: 4px; padding: 0.15em 0.4em;
}
.message-content pre code {
  background: none; border: none;
  padding: 0; font-size: 13px; color: inherit;
}
.thinking-block {
  border-left: 2px solid var(--accent-purple);
  background: var(--thinking-bg);
  border-radius: 0 var(--radius-sm) var(--radius-sm) 0;
  padding: 8px 14px 12px; margin: 4px 0;
  font-style: italic; color: var(--text-secondary);
  font-size: 13px; line-height: 1.65; display: none;
}
.thinking-label {
  font-size: 12px; font-weight: 600;
  color: var(--accent-purple);
  letter-spacing: 0.01em;
  margin-bottom: 4px; font-style: normal;
}
.message.thinking-only { display: none; }
#thinking-toggle:checked ~ main .thinking-block {
  display: block;
}
#thinking-toggle:checked ~ main .message.thinking-only {
  display: block;
}
#transcript-focused:checked ~ main .message.focused-hidden {
  display: none;
}
.tool-block {
  border-left: 2px solid var(--accent-amber);
  background: var(--tool-bg);
  border-radius: 0 var(--radius-sm) var(--radius-sm) 0;
  padding: 6px 10px; margin: 4px 0;
  font-family: var(--font-mono);
  font-size: 12px; color: var(--text-secondary);
}
#sort-toggle:checked ~ main .messages {
  flex-direction: column-reverse;
}
.toggle-input {
  position: absolute; opacity: 0; pointer-events: none;
}
.toggle-label {
  display: inline-flex; align-items: center; gap: 4px;
  padding: 4px 10px;
  background: var(--bg-inset);
  border: 1px solid var(--border-default);
  border-radius: var(--radius-sm);
  color: var(--text-primary);
  cursor: pointer; font-size: 11px;
}
#transcript-normal:checked ~ header label[for="transcript-normal"],
#transcript-focused:checked ~ header label[for="transcript-focused"],
#thinking-toggle:checked ~ header label[for="thinking-toggle"],
#sort-toggle:checked ~ header label[for="sort-toggle"] {
  background: var(--accent-blue); color: #fff;
  border-color: var(--accent-blue);
}
.theme-btn {
  padding: 4px 10px;
  background: var(--bg-inset);
  border: 1px solid var(--border-default);
  border-radius: var(--radius-sm);
  color: var(--text-primary);
  cursor: pointer; font-size: 11px;
  font-family: var(--font-sans);
}
.theme-btn:hover { background: var(--border-default); }
footer {
  max-width: 900px; margin: 40px auto; padding: 16px 24px;
  border-top: 1px solid var(--border-default);
  font-size: 11px; color: var(--text-muted);
  text-align: center;
}
footer a {
  color: var(--accent-blue); text-decoration: none;
}
footer a:hover { text-decoration: underline; }
</style>
</head>
<body>
<input type="radio" id="transcript-normal" name="transcript-mode" class="toggle-input" checked>
<input type="radio" id="transcript-focused" name="transcript-mode" class="toggle-input">
<input type="checkbox" id="thinking-toggle" class="toggle-input">
<input type="checkbox" id="sort-toggle" class="toggle-input">
<header>
<div class="header-content">
<div>
  <h1>{{.Project}}</h1>
  <div class="session-meta">
    <span>{{.Agent}}</span>
    <span>{{.MessageCount}} messages</span>
    <span>{{.StartedAt}}</span>
  </div>
</div>
<div class="controls">
  <label for="transcript-normal" class="toggle-label">Normal</label>
  <label for="transcript-focused" class="toggle-label">Focused</label>
  <label for="thinking-toggle" class="toggle-label">Thinking</label>
  <label for="sort-toggle" class="toggle-label">Newest first</label>
  <button class="theme-btn" onclick="document.documentElement.classList.toggle('dark');this.textContent=document.documentElement.classList.contains('dark')?'Light':'Dark'">Dark</button>
</div>
</div>
</header>
<main><div class="messages">
{{- range .Messages}}
<div class="message {{.RoleClass}}{{.ExtraClass}}{{if .FocusedHidden}} focused-hidden{{end}}" data-ordinal="{{.Ordinal}}"><div class="message-header"><span class="message-role">{{.Role}}</span><span class="message-time">{{.Timestamp}}</span></div><div class="message-content">{{.ContentHTML}}</div></div>
{{- end}}
</div></main>
<footer>Exported from <a href="https://github.com/kenn-io/agentsview">agentsview</a></footer>
</body></html>`

const insightExportTemplateStr = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>{{.Title}}</title>
<style>
:root {
  --bg-primary: #f7f7fa;
  --bg-surface: #ffffff;
  --bg-inset: #edeef3;
  --border-default: #dfe1e8;
  --border-muted: #e8eaf0;
  --text-primary: #1a1d26;
  --text-secondary: #5a6070;
  --text-muted: #8b92a0;
  --accent-blue: #2563eb;
  --accent-purple: #7c3aed;
  --accent-amber: #d97706;
  --radius-sm: 4px;
  --radius-md: 6px;
  --font-sans: -apple-system, BlinkMacSystemFont, "Segoe UI",
    "Noto Sans", Helvetica, Arial, sans-serif;
  --font-mono: "JetBrains Mono", "SF Mono", "Fira Code",
    "Fira Mono", Menlo, Consolas, monospace;
  color-scheme: light;
}
* { box-sizing: border-box; margin: 0; padding: 0; }
body {
  font-family: var(--font-sans);
  font-size: 14px;
  background: var(--bg-primary);
  color: var(--text-primary);
  line-height: 1.6;
  -webkit-font-smoothing: antialiased;
  -moz-osx-font-smoothing: grayscale;
}
main {
  max-width: 860px;
  margin: 0 auto;
  padding: 32px 20px 48px;
}
header {
  background: var(--bg-surface);
  border: 1px solid var(--border-default);
  border-radius: var(--radius-md);
  padding: 20px 24px;
  margin-bottom: 20px;
}
h1 {
  font-size: 24px;
  font-weight: 700;
  margin-bottom: 10px;
}
.meta {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  font-size: 12px;
  color: var(--text-secondary);
}
.chip {
  padding: 3px 8px;
  border-radius: 999px;
  background: var(--bg-inset);
}
.content {
  background: var(--bg-surface);
  border: 1px solid var(--border-default);
  border-radius: var(--radius-md);
  padding: 24px;
  white-space: pre-wrap;
  word-break: break-word;
}
.content h1,
.content h2,
.content h3,
.content h4,
.content p,
.content ul,
.content ol,
.content blockquote,
.content pre {
  margin-bottom: 12px;
}
.content code {
  font-family: var(--font-mono);
  font-size: 0.9em;
  background: var(--bg-inset);
  border: 1px solid var(--border-muted);
  border-radius: var(--radius-sm);
  padding: 0.15em 0.4em;
}
.content pre {
  background: #1e1e2e;
  color: #cdd6f4;
  border-radius: var(--radius-md);
  padding: 12px 16px;
  overflow-x: auto;
}
.content pre code {
  background: none;
  border: none;
  color: inherit;
  padding: 0;
}
.thinking-block {
  border-left: 2px solid var(--accent-purple);
  background: #f5f3ff;
  border-radius: 0 var(--radius-sm) var(--radius-sm) 0;
  padding: 8px 14px 12px;
  margin: 8px 0;
  font-style: italic;
  color: var(--text-secondary);
}
.thinking-label {
  font-size: 12px;
  font-weight: 600;
  color: var(--accent-purple);
  margin-bottom: 4px;
  font-style: normal;
}
.tool-block {
  border-left: 2px solid var(--accent-amber);
  background: #fffbf0;
  border-radius: 0 var(--radius-sm) var(--radius-sm) 0;
  padding: 6px 10px;
  margin: 8px 0;
  font-family: var(--font-mono);
  font-size: 12px;
  color: var(--text-secondary);
}
footer {
  max-width: 860px;
  margin: 0 auto 32px;
  padding: 0 20px;
  font-size: 11px;
  color: var(--text-muted);
}
footer a {
  color: var(--accent-blue);
  text-decoration: none;
}
</style>
</head>
<body>
<main>
  <header>
    <h1>{{.Title}}</h1>
    <div class="meta">
      <span class="chip">{{.Type}}</span>
      <span class="chip">{{.Project}}</span>
      <span class="chip">{{.DateRange}}</span>
      <span class="chip">{{.Agent}}</span>
      {{if .Model}}<span class="chip">{{.Model}}</span>{{end}}
      {{if .CreatedAt}}<span class="chip">{{.CreatedAt}}</span>{{end}}
    </div>
  </header>
  <article class="content">{{.ContentHTML}}</article>
</main>
<footer>Exported from <a href="https://github.com/kenn-io/agentsview">agentsview</a></footer>
</body>
</html>`

func generateExportHTML(
	session *db.Session, msgs []db.Message,
) string {
	startedAt := ""
	if session.StartedAt != nil {
		startedAt = formatTimestamp(*session.StartedAt)
	}

	data := exportData{
		Project:      session.Project,
		Agent:        agentDisplayName(session.Agent),
		MessageCount: session.MessageCount,
		StartedAt:    startedAt,
		Messages:     make([]exportMessage, len(msgs)),
	}

	focusedVisible := focusedExportOrdinals(msgs)
	for i, m := range msgs {
		roleClass := "unknown"
		if m.Role == "user" || m.Role == "assistant" {
			roleClass = m.Role
		}
		extraClass := ""
		if m.Role == "assistant" && isThinkingOnly(m.Content) {
			extraClass = " thinking-only"
		}

		data.Messages[i] = exportMessage{
			Ordinal:       m.Ordinal,
			RoleClass:     roleClass,
			ExtraClass:    extraClass,
			Role:          m.Role,
			Timestamp:     formatTimestamp(m.Timestamp),
			ContentHTML:   template.HTML(formatContentForExport(m.Content)),
			FocusedHidden: !focusedVisible[m.Ordinal],
		}
	}

	var b strings.Builder
	if err := exportTmpl.Execute(&b, data); err != nil {
		return fmt.Sprintf("template error: %s", err)
	}
	return b.String()
}

func generateInsightExportHTML(insight *db.Insight) string {
	data := insightExportData{
		Title:       insightExportTitle(insight),
		Type:        insightTypeLabel(insight.Type),
		Project:     insightProjectLabel(insight.Project),
		DateRange:   insightDateRangeLabel(insight.DateFrom, insight.DateTo),
		Agent:       agentDisplayName(insight.Agent),
		Model:       strings.TrimSpace(derefString(insight.Model)),
		CreatedAt:   formatTimestamp(insight.CreatedAt),
		ContentHTML: template.HTML(formatContentForExport(insight.Content)),
	}

	var b strings.Builder
	if err := insightExportTmpl.Execute(&b, data); err != nil {
		return fmt.Sprintf("template error: %s", err)
	}
	return b.String()
}

func agentDisplayName(agent string) string {
	if def, ok := parser.AgentByType(parser.AgentType(agent)); ok {
		return def.DisplayName
	}
	return agent
}

var (
	codeBlockRe      = regexp.MustCompile("(?s)```(\\w*)\\n(.*?)```")
	inlineCodeRe     = regexp.MustCompile("`([^`]+)`")
	thinkingMarkedRe = regexp.MustCompile(
		`(?s)\[Thinking\]\n?(.*?)\n?\[/Thinking\]`)
	thinkingLegacyRe = regexp.MustCompile(
		`(?s)\[Thinking\]\n?(.*?)(?:\n\[|\n\n|$)`)
	toolBlockRe = regexp.MustCompile(
		`(?s)\[(` + exportToolNames +
			`)([^\]]*)\](.*?)(?:\n\[|\n\n|$)`)
)

const thinkingHTML = `<div class="thinking-block">` +
	`<div class="thinking-label">Thinking</div>$1</div>`

func formatContentForExport(text string) string {
	s := html.EscapeString(text)
	s = codeBlockRe.ReplaceAllString(s, "<pre><code>$2</code></pre>")
	s = inlineCodeRe.ReplaceAllString(s, "<code>$1</code>")
	s = thinkingMarkedRe.ReplaceAllString(s, thinkingHTML)
	s = thinkingLegacyRe.ReplaceAllString(s, thinkingHTML)
	s = toolBlockRe.ReplaceAllString(s,
		`<div class="tool-block">[$1$2]$3</div>`)
	return s
}

func isThinkingOnly(content string) bool {
	if content == "" {
		return false
	}
	without := thinkingMarkedRe.ReplaceAllString(content, "")
	without = thinkingLegacyRe.ReplaceAllString(without, "")
	return strings.TrimSpace(without) == ""
}

func focusedExportOrdinals(msgs []db.Message) map[int]bool {
	visible := make(map[int]bool, len(msgs))
	pendingOrdinal := 0
	hasPendingAssistant := false
	toolAfterPendingAssistant := false

	flushPending := func() {
		if hasPendingAssistant && !toolAfterPendingAssistant {
			visible[pendingOrdinal] = true
		}
		hasPendingAssistant = false
		toolAfterPendingAssistant = false
	}

	for _, m := range msgs {
		if m.IsCompactBoundary {
			flushPending()
			visible[m.Ordinal] = true
			continue
		}

		if m.IsSystem || isThinkingOnly(m.Content) {
			continue
		}

		if isExportToolOnly(m) {
			if hasPendingAssistant {
				toolAfterPendingAssistant = true
			}
			continue
		}

		if m.Role == "user" {
			flushPending()
			visible[m.Ordinal] = true
			continue
		}

		// Match the app's focused transcript mode: consecutive
		// assistant-like messages collapse to the last visible answer.
		pendingOrdinal = m.Ordinal
		hasPendingAssistant = true
		toolAfterPendingAssistant = false
	}

	flushPending()
	return visible
}

func isExportToolOnly(m db.Message) bool {
	if m.Role != "assistant" || !m.HasToolUse {
		return false
	}
	for _, segment := range parseMarkdownSegments(m) {
		switch segment.Type {
		case markdownSegmentThinking, markdownSegmentTool:
			continue
		case markdownSegmentText:
			if strings.TrimSpace(segment.Content) == "" {
				continue
			}
			return false
		default:
			return false
		}
	}
	return true
}

// parseTimestamp tries RFC3339Nano then RFC3339.
func parseTimestamp(ts string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t, err = time.Parse(time.RFC3339, ts)
	}
	return t, err == nil
}

func formatTimestamp(ts string) string {
	if ts == "" {
		return ""
	}
	t, ok := parseTimestamp(ts)
	if !ok {
		return ts
	}
	return t.Format("2006-01-02 15:04:05")
}

func formatDateShort(ts *string) string {
	if ts == nil || *ts == "" {
		return "unknown"
	}
	t, ok := parseTimestamp(*ts)
	if !ok {
		return "unknown"
	}
	return t.Format("20060102")
}

func sanitizeFilename(name string) string {
	re := regexp.MustCompile(`[^\w.\-]`)
	return re.ReplaceAllString(name, "_")
}

func insightExportStem(insight *db.Insight) string {
	dateRange := strings.ReplaceAll(insight.DateFrom, "-", "")
	if insight.DateTo != "" && insight.DateTo != insight.DateFrom {
		dateRange += "-" + strings.ReplaceAll(insight.DateTo, "-", "")
	}
	return sanitizeFilename(fmt.Sprintf(
		"insight-%s-%s-%s",
		insight.Type,
		insightProjectLabel(insight.Project),
		dateRange,
	))
}

func insightExportHTMLFilename(insight *db.Insight) string {
	return insightExportStem(insight) + ".html"
}

func insightExportMarkdownFilename(insight *db.Insight) string {
	return insightExportStem(insight) + ".md"
}

func insightExportTitle(insight *db.Insight) string {
	return fmt.Sprintf(
		"%s Insight",
		insightTypeLabel(insight.Type),
	)
}

func insightPublishDescription(insight *db.Insight) string {
	return fmt.Sprintf(
		"Insight: %s - %s - %s",
		insightTypeLabel(insight.Type),
		insightProjectLabel(insight.Project),
		insightDateRangeLabel(insight.DateFrom, insight.DateTo),
	)
}

func insightTypeLabel(insightType string) string {
	switch insightType {
	case "daily_activity":
		return "Daily Activity"
	case "agent_analysis":
		return "Agent Analysis"
	default:
		return strings.ReplaceAll(insightType, "_", " ")
	}
}

func insightProjectLabel(project *string) string {
	value := strings.TrimSpace(derefString(project))
	if value == "" {
		return "global"
	}
	return value
}

func insightDateRangeLabel(dateFrom, dateTo string) string {
	if dateTo == "" || dateTo == dateFrom {
		return dateFrom
	}
	return dateFrom + " to " + dateTo
}

func publishExportHTML(
	ctx context.Context,
	token, filename, description, htmlContent string,
	public bool,
) (*publishResponse, error) {
	gist, err := createGist(ctx, token, filename, description, htmlContent, public)
	if err != nil {
		return nil, err
	}
	if gist.ID == "" || gist.HTMLURL == "" {
		return nil, fmt.Errorf("GitHub API returned incomplete gist data")
	}
	encoded := urlPathEscape(filename)
	rawURL := fmt.Sprintf(
		"https://gist.githubusercontent.com/%s/%s/raw/%s",
		gist.Owner.Login, gist.ID, encoded,
	)
	return &publishResponse{
		GistID:  gist.ID,
		GistURL: gist.HTMLURL,
		ViewURL: "https://htmlpreview.github.io/?" + rawURL,
		RawURL:  rawURL,
	}, nil
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	// Truncate at a valid rune boundary to avoid producing
	// invalid UTF-8.
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "..."
}
