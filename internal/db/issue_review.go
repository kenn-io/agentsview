package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/secrets"
)

const (
	issueSnippetLimit = 1200

	IssueReviewInputLimit       = 2400
	IssueReviewResultEdgeLimit  = 1200
	IssueReviewMessageScanLimit = 4000
	issueReviewCacheTTL         = time.Hour
)

// IssueReviewQuery contains detector-specific scope and result controls.
type IssueReviewQuery struct {
	SessionID           string
	Folder              string
	Reason              string
	Tool                string
	Source              string
	Outcome             string
	Severity            string
	Confidence          string
	Status              string
	RecommendationType  string
	MinOccurrences      int
	MinSessions         int
	MinProjects         int
	MinWastedDurationMS int64
	Sort                string
	Refresh             bool
	Offset              int
	Limit               int
}

type IssueReviewResponse struct {
	GeneratedAt        string               `json:"generated_at"`
	ScannedSessions    int                  `json:"scanned_sessions"`
	ScannedMessages    int                  `json:"scanned_messages"`
	ScannedToolCalls   int                  `json:"scanned_tool_calls"`
	AnalyzedMessages   int                  `json:"analyzed_messages"`
	AnalyzedToolCalls  int                  `json:"analyzed_tool_calls"`
	DuplicateMessages  int                  `json:"duplicate_messages"`
	DuplicateToolCalls int                  `json:"duplicate_tool_calls"`
	ScannedTelemetry   int                  `json:"scanned_telemetry"`
	TelemetryStatus    string               `json:"telemetry_status"`
	TotalFindings      int                  `json:"total_findings"`
	Truncated          bool                 `json:"truncated"`
	Findings           []IssueReviewFinding `json:"findings" nullable:"false"`
	Facets             IssueReviewFacets    `json:"facets"`
}

type IssueFacet struct {
	Value string `json:"value"`
	Label string `json:"label,omitempty"`
	Count int    `json:"count"`
}

type IssueReviewFacets struct {
	Category           []IssueFacet `json:"category" nullable:"false"`
	Tool               []IssueFacet `json:"tool" nullable:"false"`
	Source             []IssueFacet `json:"source" nullable:"false"`
	Severity           []IssueFacet `json:"severity" nullable:"false"`
	Confidence         []IssueFacet `json:"confidence" nullable:"false"`
	Status             []IssueFacet `json:"status" nullable:"false"`
	RecommendationType []IssueFacet `json:"recommendation_type" nullable:"false"`
	Session            []IssueFacet `json:"session" nullable:"false"`
	Folder             []IssueFacet `json:"folder" nullable:"false"`
	Outcome            []IssueFacet `json:"outcome" nullable:"false"`
}

type IssueReviewFinding struct {
	ID                     string                `json:"id"`
	ReasonCode             string                `json:"reason_code"`
	Tool                   string                `json:"tool"`
	Signature              string                `json:"signature"`
	Severity               string                `json:"severity"`
	Confidence             string                `json:"confidence"`
	Status                 string                `json:"status"`
	RecommendationType     string                `json:"recommendation_type"`
	Recommendation         string                `json:"recommendation"`
	GitHubReference        string                `json:"github_reference,omitempty"`
	Sources                []string              `json:"sources" nullable:"false"`
	Occurrences            int                   `json:"occurrences"`
	SessionCount           int                   `json:"session_count"`
	ProjectCount           int                   `json:"project_count"`
	IncompleteSessionCount int                   `json:"incomplete_session_count"`
	TotalDurationMS        int64                 `json:"total_duration_ms"`
	WastedDurationMS       int64                 `json:"wasted_duration_ms"`
	P95DurationMS          *int64                `json:"p95_duration_ms,omitempty"`
	DurationCoverage       float64               `json:"duration_coverage"`
	DurationSource         string                `json:"duration_source,omitempty"`
	LastSeen               string                `json:"last_seen"`
	Evidence               []IssueReviewEvidence `json:"evidence" nullable:"false"`
	rank                   int
}

type IssueReviewEvidence struct {
	SessionID      string `json:"session_id"`
	Project        string `json:"project"`
	CWD            string `json:"cwd"`
	Agent          string `json:"agent"`
	Date           string `json:"date"`
	Outcome        string `json:"outcome"`
	Source         string `json:"source"`
	Tool           string `json:"tool"`
	Excerpt        string `json:"excerpt"`
	MessageOrdinal *int   `json:"message_ordinal,omitempty"`
	CallIndex      *int   `json:"call_index,omitempty"`
	EventStatus    string `json:"event_status,omitempty"`
	Recovered      bool   `json:"recovered"`
	DurationMS     *int64 `json:"duration_ms,omitempty"`
}

// IssueReviewSession, IssueReviewMessage, and IssueReviewToolCall are the
// narrow cross-store rows consumed by the shared detector.
type IssueReviewSession struct {
	ID, Name, Project, CWD, Agent, Date, Outcome string
	Incomplete                                   bool
}

type IssueReviewMessage struct {
	SessionID, Role, Content, Timestamp, SourceType, SourceSubtype, StableID string
	Ordinal                                                                  int
	IsSystem                                                                 bool
}

type IssueReviewToolCall struct {
	SessionID, Tool, Category, ToolUseID, Input, Result string
	EventStatus, EventSource, Timestamp, DurationSource string
	MessageOrdinal, CallIndex                           int
	DurationMS                                          *int64
}

type IssueReviewTelemetry struct {
	SessionID, Target, Level, Body, Timestamp string
	Tool, CallID                              string
	DurationMS                                *int64
}

type issueReviewCacheEntry struct {
	key       string
	expiresAt time.Time
	response  IssueReviewResponse
}

// IssueReviewCache shares the short-lived base-analysis cache across stores.
type IssueReviewCache struct {
	mu    sync.Mutex
	entry *issueReviewCacheEntry
}

func (c *IssueReviewCache) Get(key string, q IssueReviewQuery) (IssueReviewResponse, bool) {
	if q.Refresh {
		return IssueReviewResponse{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entry == nil || c.entry.key != key || !time.Now().Before(c.entry.expiresAt) {
		return IssueReviewResponse{}, false
	}
	return filterIssueReviewResponse(c.entry.response, q), true
}

func (c *IssueReviewCache) Put(key string, response IssueReviewResponse) {
	c.mu.Lock()
	c.entry = &issueReviewCacheEntry{key: key, expiresAt: time.Now().Add(issueReviewCacheTTL), response: response}
	c.mu.Unlock()
}

var (
	spaceRE            = regexp.MustCompile(`\s+`)
	windowsPathRE      = regexp.MustCompile(`(?i)[A-Z]:[\\/][^\r\n"']+`)
	unixPathRE         = regexp.MustCompile(`(?:^|\s)/(?:[^\s"']+/)+[^\s"']*`)
	searchCommandRE    = regexp.MustCompile(`(?i)^\s*(?:&\s*)?(?:"[^"]*[\\/])?(?:rg|grep)(?:\.exe)?(?:"?\s|$)`)
	errorWordRE        = regexp.MustCompile(`(?i)\b(error|failed|failure|fatal|exception|denied|timeout|not found|cannot|could not|crash|panic)\b`)
	blockerPredicateRE = regexp.MustCompile(`(?i)\b(error|failed|failure|bug|issue|blocked|broken|crash|denied|timeout|cannot|stopped|unavailable|missing|requires|incomplete)\b|\bcould not\b|\bdid not\b`)
	failureSummaryRE   = regexp.MustCompile(`(?i)\b[1-9]\d*\s+(?:tests?\s+)?failed\b|\btests? failed\b|(?:^|\n)\s*(?:npm err!|fatal:|panic:|traceback \(most recent call last\):)`)
	httpFailureRE      = regexp.MustCompile(`(?i)\b(?:http(?: status)?|status)\s*[:=]?\s*[45]\d\d\b`)
	githubIssueURLRE   = regexp.MustCompile(`(?i)https?://github\.com/([a-z0-9_.-]+)/([a-z0-9_.-]+)/issues/([1-9]\d*)`)
	githubIssueShortRE = regexp.MustCompile(`(?i)\b([a-z0-9_.-]+)/([a-z0-9_.-]+)#([1-9]\d*)\b`)
	nestedToolRE       = regexp.MustCompile("\\btools\\.([A-Za-z0-9_]+)\\s*\\(")
	logFieldRE         = regexp.MustCompile(`([a-z_]+)=(?:"([^"]*)"|([^\s]+))`)
	credentialFieldRE  = regexp.MustCompile(`(?i)["']?\b(api[_-]?key|key|token|secret|password|credential|authorization|cookie|session[_-]?key)\b["']?\s*[:=]\s*(?:"[^"]*"|'[^']*'|[^\s,;]+)`)
	pathFieldRE        = regexp.MustCompile(`(?i)\b(path|cwd|workdir|file|filename|directory|repo|repository|socket)\b\s*[:=]\s*(?:"[^"]*"|'[^']*'|[^\s,;]+)`)
	bearerRE           = regexp.MustCompile(`(?i)\bbearer\s+[^\s,;]+`)
)

var correctionTerms = []string{"nah", "no, that's not", "no, that is not", "i don't want", "i do not want", "wrong", "something is off", "where exactly", "you missed", "that's not what", "that is not what"}

var blockerTerms = []string{"root cause", "failed because", "blocked by", "hit a ", "exposed a ", "exposed an ", "stopped before", "currently broken", "crashed", "failure is", "failures compare"}

var failureMarkerTerms = []string{
	"script failed", "script error", "parsererror", "parameterbinding", "invalid context",
	"npm err!", "fatal:", "panic:", "traceback (most recent call last):",
	"permission denied", "access is denied", "deadline exceeded", "timed out",
	"unhandled exception", "segmentation fault", `"iserror":true`, "iserror=true",
}

// IssueReviewMessagePredicate narrows storage reads to text that the shared
// message detector can classify. All terms are fixed internal constants.
func IssueReviewMessagePredicate(roleColumn, contentColumn string) string {
	likes := func(terms []string) string {
		parts := make([]string, len(terms))
		for i, term := range terms {
			term = strings.ReplaceAll(term, "'", "''")
			parts[i] = "LOWER(" + contentColumn + ") LIKE '%" + term + "%'"
		}
		return "(" + strings.Join(parts, " OR ") + ")"
	}
	return "((" + roleColumn + " = 'user' AND (LENGTH(TRIM(" + contentColumn + ")) >= 32 OR " + likes(correctionTerms) + ")) OR (" + roleColumn + " = 'assistant' AND " + likes(blockerTerms) + ") OR LOWER(" + contentColumn + ") LIKE '%github.com/%/issues/%')"
}

// IssueReviewTailPredicate limits expensive result-tail reads to calls whose
// structured status or bounded head already proves a failure.
func IssueReviewTailPredicate(statusColumn, resultColumn string) string {
	head := "LOWER(SUBSTR(" + resultColumn + ",1," + strconv.Itoa(IssueReviewResultEdgeLimit) + "))"
	likes := make([]string, len(failureMarkerTerms))
	for i, term := range failureMarkerTerms {
		likes[i] = head + " LIKE '%" + strings.ReplaceAll(term, "'", "''") + "%'"
	}
	return "(LOWER(COALESCE(" + statusColumn + ",'')) IN ('errored','error','cancelled','canceled') OR " + strings.Join(likes, " OR ") + ")"
}

type issuePattern struct {
	reason string
	terms  []string
}

var issueFailurePatterns = []issuePattern{
	{"windows_shell", []string{"parsererror", "parameterbinding", "a parameter cannot be found", "is not recognized as the name", "the term '"}},
	{"line_endings", []string{"crlf", "line ending", "newline-portable", "contains \\r\\n", "carriage return"}},
	{"missing_file", []string{"no such file or directory", "cannot find path", "path does not exist", "file not found", "could not find file", "index is incomplete"}},
	{"missing_dependency", []string{"command not found", "module not found", "cannot find module", "no module named", "missing dependency", "package not installed"}},
	{"permission_auth", []string{"permission denied", "access is denied", "unauthorized", "forbidden", "status 401", " 401 ", "requires root", "requires sudo", "authentication failed", "credential"}},
	{"rate_limit", []string{"rate limit", "too many requests", "status 429", " 429 ", "quota exceeded"}},
	{"shell_syntax", []string{"unexpected eof while looking for matching", "unexpected token", "syntax error near unexpected token", "unterminated quoted string"}},
	{"network", []string{"connection refused", "connection reset", "network is unreachable", "dns", "tls handshake", "websocket", "stream disconnect", "unexpected eof"}},
	{"timeout", []string{"timed out", "timeout", "deadline exceeded", "60m limit"}},
	{"git_github_ci", []string{"github", "gh api", "git push", "git pull", "merge conflict", "non-fast-forward", "workflow failed", "actions failed", "ci failed", "fatal: not a git"}},
	{"failed_edit", []string{"apply_patch", "patch failed", "invalid context", "failed to apply", "edit failed", "old_string was not found", "did not match"}},
	{"build_test", []string{"compilation failed", "compiler error", "build failed", "test failed", "tests failed", "assertion failed", "schema existed before restore", "psql", "migration failed", "npm err", "typecheck failed"}},
	{"tool_crash", []string{"panicked", "panic:", "segmentation fault", "stack trace", "crashed", "access violation", "unhandled exception"}},
}

// ClassifyIssueFailure classifies a tool result conservatively. It returns
// false for explicit success and search no-match exits.
func ClassifyIssueFailure(tool, status, input, result string) (string, bool) {
	command := input
	if isShellTool(tool) {
		command = issueCommandInput(input)
	}
	return classifyIssueFailure(tool, status, command, result)
}

func classifyIssueFailure(tool, status, command, result string) (string, bool) {
	status = strings.ToLower(strings.TrimSpace(status))
	failedStatus := status == "errored" || status == "error" || status == "cancelled" || status == "canceled"
	resultLower := strings.ToLower(result)
	hasZeroExit, hasOneExit, hasNonZeroExit := issueExitCodes(resultLower)
	if isSearchInvocation(tool, command) && hasOneExit && !hasSpecificSearchFailure(resultLower) {
		return "", false
	}
	logicalFailure := hasLogicalFailure(command, resultLower)
	markerFailure := explicitFailureMarker(result)
	if !failedStatus && !hasNonZeroExit && isReadInvocation(tool, command) {
		return "", false
	}
	if hasZeroExit && !hasNonZeroExit && !logicalFailure && !failedStatus {
		return "", false
	}
	if !failedStatus && !hasNonZeroExit && !logicalFailure && !markerFailure {
		return "", false
	}
	if reason, ok := classifyIssueReason(resultLower); ok {
		return reason, true
	}
	if isEditInvocation(tool, command) {
		return "failed_edit", true
	}
	if isGitHubInvocation(tool, command) {
		return "git_github_ci", true
	}
	if isBuildTestInvocation(tool, command) {
		return "build_test", true
	}
	if isShellTool(tool) {
		return "command_failure", true
	}
	if failedStatus || hasNonZeroExit || logicalFailure || markerFailure {
		return "generic_tool_failure", true
	}
	return "", false
}

func classifyIssueReason(content string) (string, bool) {
	content = strings.ToLower(content)
	for _, pattern := range issueFailurePatterns {
		for _, term := range pattern.terms {
			if strings.Contains(content, term) {
				return pattern.reason, true
			}
		}
	}
	return "", false
}

func issueFailureConfidence(status, input, result string) string {
	status = strings.ToLower(strings.TrimSpace(status))
	if status == "errored" || status == "error" || status == "cancelled" || status == "canceled" {
		return "high"
	}
	_, _, hasNonZeroExit := issueExitCodes(strings.ToLower(result))
	if hasNonZeroExit {
		return "high"
	}
	return "medium"
}

func explicitFailureMarker(result string) bool {
	result = strings.ToLower(result)
	for _, marker := range failureMarkerTerms {
		if strings.Contains(result, marker) {
			return true
		}
	}
	for _, line := range strings.Split(result, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "error:") || strings.HasPrefix(line, "exception:") {
			return true
		}
	}
	return false
}

func hasLogicalFailure(input, resultLower string) bool {
	if strings.Contains(resultLower, "parsererror") || strings.Contains(resultLower, "parameterbinding") || strings.Contains(resultLower, "invalid context") {
		return true
	}
	if (strings.Contains(resultLower, "failed") || strings.Contains(resultLower, "npm err!") || strings.Contains(resultLower, "fatal:") || strings.Contains(resultLower, "panic:") || strings.Contains(resultLower, "traceback (most recent call last):")) && failureSummaryRE.MatchString(resultLower) {
		return true
	}
	lowerInput := strings.ToLower(input)
	if (!strings.Contains(resultLower, "http") && !strings.Contains(resultLower, "status")) || !httpFailureRE.MatchString(resultLower) {
		return false
	}
	for _, term := range []string{"gh ", "github", "curl", "invoke-webrequest", "api", "http"} {
		if strings.Contains(lowerInput, term) {
			return true
		}
	}
	return false
}

func issueExitCodes(resultLower string) (hasZero, hasOne, hasNonZero bool) {
	for offset := 0; ; {
		relative := strings.Index(resultLower[offset:], "exit")
		if relative < 0 {
			return hasZero, hasOne, hasNonZero
		}
		start := offset + relative + len("exit")
		if strings.HasPrefix(resultLower[start:], "ed") {
			start += len("ed")
		}
		for start < len(resultLower) && resultLower[start] == ' ' {
			start++
		}
		if strings.HasPrefix(resultLower[start:], "with") {
			start += len("with")
			for start < len(resultLower) && resultLower[start] == ' ' {
				start++
			}
		}
		if strings.HasPrefix(resultLower[start:], "code") {
			start += len("code")
		}
		for start < len(resultLower) && (resultLower[start] == ' ' || resultLower[start] == ':' || resultLower[start] == '=') {
			start++
		}
		if start < len(resultLower) && resultLower[start] >= '0' && resultLower[start] <= '9' {
			value := 0
			for start < len(resultLower) && resultLower[start] >= '0' && resultLower[start] <= '9' {
				value = value*10 + int(resultLower[start]-'0')
				start++
			}
			hasZero = hasZero || value == 0
			hasOne = hasOne || value == 1
			hasNonZero = hasNonZero || value > 0
		}
		offset += relative + len("exit")
	}
}

func hasSpecificSearchFailure(resultLower string) bool {
	withoutWrapper := strings.ReplaceAll(resultLower, "script failed", "")
	for _, term := range []string{"error", "fatal", "exception", "denied", "timeout", "not found", "cannot", "could not", "crash", "panic", "parsererror", "parameterbinding", "invalid context", "no such file"} {
		if strings.Contains(withoutWrapper, term) {
			return true
		}
	}
	return false
}

func isSearchInvocation(tool, command string) bool {
	if isSearchTool(tool) {
		return true
	}
	if !isShellTool(tool) {
		return false
	}
	return searchCommandRE.MatchString(command)
}

func issueCommandInput(input string) string {
	var payload struct {
		Command string `json:"command"`
	}
	if json.Unmarshal([]byte(input), &payload) == nil && payload.Command != "" {
		return payload.Command
	}
	return input
}

func isEditInvocation(tool, command string) bool {
	lowerTool := normalizeTool(tool)
	if strings.Contains(lowerTool, "apply_patch") || strings.Contains(lowerTool, "edit") {
		return true
	}
	lower := strings.ToLower(command)
	return strings.HasPrefix(strings.TrimSpace(lower), "apply_patch")
}

func isGitHubInvocation(tool, command string) bool {
	if !isShellTool(tool) && !strings.Contains(normalizeTool(tool), "github") {
		return false
	}
	lower := strings.ToLower(command)
	return strings.Contains(lower, "gh ") || strings.Contains(lower, "github.com") ||
		strings.Contains(lower, "git push") || strings.Contains(lower, "git pull") ||
		strings.Contains(lower, "git fetch") || strings.Contains(lower, "git clone")
}

func isBuildTestInvocation(tool, command string) bool {
	if !isShellTool(tool) {
		return false
	}
	lower := strings.ToLower(command)
	for _, term := range []string{" go test", "npm test", "npm run build", "npm run check", "pytest", "cargo test", "dotnet test", "psql", "migration"} {
		if strings.Contains(" "+lower, term) {
			return true
		}
	}
	return false
}

func canonicalGitHubReference(value string) string {
	return canonicalGitHubReferenceParts(value, "")
}

func canonicalGitHubReferenceParts(first, second string) string {
	values := [...]string{first, second}
	for _, value := range values {
		if strings.Contains(value, "://") {
			match := githubIssueURLRE.FindStringSubmatch(value)
			if len(match) == 4 {
				return strings.ToLower(match[1]+"/"+match[2]) + "#" + match[3]
			}
		}
	}
	for _, value := range values {
		if strings.Contains(value, "#") {
			match := githubIssueShortRE.FindStringSubmatch(value)
			if len(match) == 4 {
				return strings.ToLower(match[1]+"/"+match[2]) + "#" + match[3]
			}
		}
	}
	return ""
}

func isShellTool(tool string) bool {
	switch normalizeTool(tool) {
	case "bash", "shell", "powershell", "exec", "exec_command", "shell_command", "functions.exec":
		return true
	default:
		return false
	}
}

func isSearchTool(tool string) bool {
	t := strings.ToLower(tool)
	return t == "rg" || t == "grep" || strings.Contains(t, "search")
}

func normalizeTool(tool string) string {
	t := strings.ToLower(strings.TrimSpace(tool))
	switch t {
	case "bash", "shell", "powershell", "exec", "exec_command", "shell_command", "functions.exec":
		return t
	default:
		return t
	}
}

func effectiveIssueTool(tool, input string) string {
	outer := normalizeTool(tool)
	if outer != "exec" && outer != "functions.exec" {
		return outer
	}
	var nested string
	for _, match := range nestedToolRE.FindAllStringSubmatch(input, -1) {
		candidate := normalizeTool(match[1])
		if nested == "" {
			nested = candidate
		} else if nested != candidate {
			return outer
		}
	}
	if nested != "" {
		return nested
	}
	return outer
}

func normalizeIssueText(value string) string {
	value = strings.TrimSpace(value)
	var out strings.Builder
	out.Grow(len(value))
	pendingSpace := false
	for i := 0; i < len(value); {
		if isIssueSpace(value[i]) {
			pendingSpace = out.Len() > 0
			i++
			continue
		}
		if pendingSpace {
			out.WriteByte(' ')
			pendingSpace = false
		}
		if isWindowsPathAt(value, i) {
			out.WriteString("<path>")
			i += 3
			for i < len(value) && !strings.ContainsRune("\r\n\"'", rune(value[i])) {
				i++
			}
			continue
		}
		if isUnixPathAt(value, i) {
			out.WriteString("<path>")
			i++
			for i < len(value) && !isIssueSpace(value[i]) && value[i] != '"' && value[i] != '\'' {
				i++
			}
			continue
		}
		if end, ok := volatileIssueToken(value, i); ok {
			out.WriteByte('#')
			i = end
			continue
		}
		c := value[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out.WriteByte(c)
		i++
	}
	return out.String()
}

func isIssueSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

func isIssueWord(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}

func isWindowsPathAt(value string, i int) bool {
	return i+2 < len(value) && (value[i] >= 'a' && value[i] <= 'z' || value[i] >= 'A' && value[i] <= 'Z') && value[i+1] == ':' && value[i+2] == '\\' && (i == 0 || !isIssueWord(value[i-1]))
}

func isUnixPathAt(value string, i int) bool {
	if value[i] != '/' || i > 0 && !isIssueSpace(value[i-1]) && value[i-1] != '"' && value[i-1] != '\'' {
		return false
	}
	end := i + 1
	for end < len(value) && !isIssueSpace(value[end]) && value[end] != '"' && value[end] != '\'' {
		end++
	}
	return strings.Contains(value[i+1:end], "/")
}

func volatileIssueToken(value string, i int) (int, bool) {
	if i > 0 && isIssueWord(value[i-1]) {
		return i, false
	}
	end := i
	hasHexLetter := false
	for end < len(value) {
		c := value[end]
		if c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' || c == '-' {
			hasHexLetter = hasHexLetter || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
			end++
			continue
		}
		break
	}
	if end-i >= 7 && hasHexLetter && (end == len(value) || !isIssueWord(value[end])) {
		return end, true
	}
	end = i
	if value[i] >= '0' && value[i] <= '9' {
		for end < len(value) && (value[end] >= '0' && value[end] <= '9' || value[end] == '.') {
			end++
		}
		return end, end == len(value) || !isIssueWord(value[end])
	}
	return i, false
}

func displayIssueText(value string) string {
	value = redactIssueText(value)
	if len(value) > 240 {
		value = value[:240] + "…"
	}
	return value
}

func issueExcerpt(value string) string {
	value = redactIssueText(value)
	if len(value) > issueSnippetLimit {
		value = value[:issueSnippetLimit] + "…"
	}
	return value
}

// JoinIssueReviewResult keeps bounded failure context from both ends of a
// potentially large tool result.
func JoinIssueReviewResult(head, tail string) string {
	if tail == "" || head == tail {
		return head
	}
	return head + "\n...[truncated]...\n" + tail
}

func redactIssueText(value string) string {
	value = strings.ReplaceAll(value, `\"`, `"`)
	value = credentialFieldRE.ReplaceAllString(value, "$1=<redacted>")
	value = pathFieldRE.ReplaceAllString(value, "$1=<path>")
	value = bearerRE.ReplaceAllString(value, "Bearer <redacted>")
	value = windowsPathRE.ReplaceAllString(value, "<path>")
	value = unixPathRE.ReplaceAllString(value, " <path>")
	return secrets.Redact(strings.TrimSpace(spaceRE.ReplaceAllString(value, " ")))
}

func findingID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:8])
}

type findingAccumulator struct {
	finding        IssueReviewFinding
	sessions       map[string]bool
	projects       map[string]bool
	incomplete     map[string]bool
	sources        map[string]bool
	durations      []int64
	statsDurations []int64
	measuredCalls  int
	toolCalls      int
	totalCalls     int
	recovered      int
	unrecovered    int
}

type issueAnalyzer struct {
	sessions map[string]IssueReviewSession
	clusters map[string]*findingAccumulator
}

func newIssueAnalyzer(sessions []IssueReviewSession) *issueAnalyzer {
	byID := make(map[string]IssueReviewSession, len(sessions))
	for _, session := range sessions {
		byID[session.ID] = session
	}
	return &issueAnalyzer{sessions: byID, clusters: map[string]*findingAccumulator{}}
}

func (a *issueAnalyzer) evidence(sessionID, source, tool, excerpt, status string, ordinal, callIndex *int, duration *int64) IssueReviewEvidence {
	s := a.sessions[sessionID]
	return IssueReviewEvidence{SessionID: sessionID, Project: s.Project, CWD: s.CWD, Agent: s.Agent, Date: s.Date, Outcome: s.Outcome, Source: source, Tool: tool, Excerpt: issueExcerpt(excerpt), MessageOrdinal: ordinal, CallIndex: callIndex, EventStatus: status, DurationMS: duration}
}

func (a *issueAnalyzer) add(key, reason, tool, signature, severity, confidence, recommendation string, evidence IssueReviewEvidence, duration, waste *int64, recovered bool) {
	acc := a.clusters[key]
	if acc == nil {
		acc = &findingAccumulator{finding: IssueReviewFinding{ID: findingID(key), ReasonCode: reason, Tool: tool, Signature: displayIssueText(signature), Severity: severity, Confidence: confidence, RecommendationType: recommendation, GitHubReference: canonicalGitHubReference(signature), Evidence: []IssueReviewEvidence{}, Sources: []string{}}, sessions: map[string]bool{}, projects: map[string]bool{}, incomplete: map[string]bool{}, sources: map[string]bool{}}
		a.clusters[key] = acc
	}
	if confidence == "high" || confidence == "medium" && acc.finding.Confidence == "low" {
		acc.finding.Confidence = confidence
	}
	if acc.finding.GitHubReference == "" {
		acc.finding.GitHubReference = canonicalGitHubReference(signature + "\n" + evidence.Excerpt)
	}
	acc.finding.Occurrences++
	acc.sessions[evidence.SessionID] = true
	if evidence.Project != "" {
		acc.projects[evidence.Project] = true
	}
	if a.sessions[evidence.SessionID].Incomplete {
		acc.incomplete[evidence.SessionID] = true
	}
	if evidence.Source != "" {
		acc.sources[evidence.Source] = true
	}
	if evidence.Date > acc.finding.LastSeen {
		acc.finding.LastSeen = evidence.Date
	}
	if duration != nil {
		acc.durations = append(acc.durations, *duration)
		acc.finding.TotalDurationMS += *duration
	}
	if waste != nil {
		acc.finding.WastedDurationMS += *waste
	}
	if recovered {
		acc.recovered++
	} else {
		acc.unrecovered++
	}
	if len(acc.finding.Evidence) < 5 {
		evidence.Recovered = recovered
		acc.finding.Evidence = append(acc.finding.Evidence, evidence)
	}
}

func (a *issueAnalyzer) finish(totalCalls int, durationCounts map[string]int) []IssueReviewFinding {
	out := make([]IssueReviewFinding, 0, len(a.clusters))
	for _, acc := range a.clusters {
		f := acc.finding
		f.SessionCount = len(acc.sessions)
		f.ProjectCount = len(acc.projects)
		f.IncompleteSessionCount = len(acc.incomplete)
		for source := range acc.sources {
			f.Sources = append(f.Sources, source)
		}
		sort.Strings(f.Sources)
		f.RecommendationType = recommendationFor(f.ReasonCode, f.SessionCount, f.ProjectCount)
		f.Recommendation = concreteRecommendation(f)
		if f.SessionCount >= 2 {
			f.Status = "recurring"
		} else if acc.recovered > 0 && acc.unrecovered == 0 {
			f.Status = "recovered"
		} else if acc.unrecovered > 0 && f.IncompleteSessionCount > 0 {
			f.Status = "open"
		} else {
			f.Status = "observed"
		}
		statsDurations := acc.statsDurations
		if len(statsDurations) == 0 {
			statsDurations = acc.durations
		}
		if len(statsDurations) > 0 {
			sort.Slice(statsDurations, func(i, j int) bool { return statsDurations[i] < statsDurations[j] })
			p := int(math.Ceil(float64(len(statsDurations))*0.95)) - 1
			v := statsDurations[max(0, p)]
			f.P95DurationMS = &v
			numerator := len(acc.durations)
			denominator := durationCounts[f.Tool]
			if acc.measuredCalls > 0 {
				numerator = acc.measuredCalls
			}
			if acc.toolCalls > 0 {
				denominator = acc.toolCalls
			}
			if denominator == 0 {
				denominator = totalCalls
			}
			if denominator > 0 {
				f.DurationCoverage = float64(numerator) / float64(denominator)
			}
		}
		f.rank = f.Occurrences*10 + f.SessionCount*30 + f.IncompleteSessionCount*20 + int(f.WastedDurationMS/30000)
		if f.Severity == "high" {
			f.rank += 40
		} else if f.Severity == "medium" {
			f.rank += 20
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].rank != out[j].rank {
			return out[i].rank > out[j].rank
		}
		if out[i].Occurrences != out[j].Occurrences {
			return out[i].Occurrences > out[j].Occurrences
		}
		return out[i].ID < out[j].ID
	})
	return out
}

type analyzedCall struct {
	row        IssueReviewToolCall
	tool       string
	command    string
	normalized string
	reason     string
	failed     bool
	recovered  bool
}

type workflowAccumulator struct {
	rows         []analyzedCall
	firstSession string
	firstProject string
	multiSession bool
	multiProject bool
}

func dedupeIssueMessages(rows []IssueReviewMessage) ([]IssueReviewMessage, int) {
	seen := make(map[string]bool)
	out := make([]IssueReviewMessage, 0, len(rows))
	duplicates := 0
	for _, row := range rows {
		if row.StableID == "" {
			out = append(out, row)
			continue
		}
		key := row.Role + "|" + row.StableID + "|" + row.Content
		if seen[key] {
			duplicates++
			continue
		}
		seen[key] = true
		out = append(out, row)
	}
	return out, duplicates
}

func dedupeIssueCalls(rows []IssueReviewToolCall) ([]IssueReviewToolCall, int) {
	indices := make(map[string]int)
	out := make([]IssueReviewToolCall, 0, len(rows))
	duplicates := 0
	for _, row := range rows {
		if row.ToolUseID == "" {
			out = append(out, row)
			continue
		}
		key := normalizeTool(row.Tool) + "|" + row.ToolUseID
		index, ok := indices[key]
		if !ok {
			indices[key] = len(out)
			out = append(out, row)
			continue
		}
		duplicates++
		if len(row.Result) > len(out[index].Result) {
			out[index] = row
		} else if out[index].DurationMS == nil && row.DurationMS != nil {
			out[index].DurationMS = row.DurationMS
			out[index].DurationSource = row.DurationSource
		}
	}
	return out, duplicates
}

func AnalyzeIssueReview(sessions []IssueReviewSession, messages []IssueReviewMessage, calls []IssueReviewToolCall, telemetry []IssueReviewTelemetry, q IssueReviewQuery) IssueReviewResponse {
	return filterIssueReviewResponse(AnalyzeIssueReviewBase(sessions, messages, calls, telemetry), q)
}

// FilterIssueReview applies cheap result filters and pagination to a base analysis.
func FilterIssueReview(response IssueReviewResponse, q IssueReviewQuery) IssueReviewResponse {
	return filterIssueReviewResponse(response, q)
}

// AnalyzeIssueReviewBase performs the expensive shared analysis before result filters.
func AnalyzeIssueReviewBase(sessions []IssueReviewSession, messages []IssueReviewMessage, calls []IssueReviewToolCall, telemetry []IssueReviewTelemetry) IssueReviewResponse {
	rawMessages, rawCalls := len(messages), len(calls)
	messages, duplicateMessages := dedupeIssueMessages(messages)
	calls, duplicateCalls := dedupeIssueCalls(calls)
	a := newIssueAnalyzer(sessions)
	bySession := make(map[string][]analyzedCall)
	normalizedInputs := make(map[string]string)
	durationCounts := map[string]int{}
	toolCounts := map[string]int{}
	for _, row := range calls {
		tool := effectiveIssueTool(row.Tool, row.Input)
		command := row.Input
		if isShellTool(tool) {
			command = issueCommandInput(row.Input)
		}
		toolCounts[tool]++
		if row.DurationMS != nil && *row.DurationMS < 0 {
			row.DurationMS = nil
			row.DurationSource = ""
		}
		normalized := strings.TrimSpace(command)
		reason, failed := classifyIssueFailure(tool, row.EventStatus, command, row.Result)
		bySession[row.SessionID] = append(bySession[row.SessionID], analyzedCall{row: row, tool: tool, command: command, normalized: normalized, reason: reason, failed: failed})
		if row.DurationMS != nil && *row.DurationMS >= 0 {
			durationCounts[tool]++
		}
	}
	workflows := map[string]*workflowAccumulator{}
	for sessionID, rows := range bySession {
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].row.MessageOrdinal != rows[j].row.MessageOrdinal {
				return rows[i].row.MessageOrdinal < rows[j].row.MessageOrdinal
			}
			return rows[i].row.CallIndex < rows[j].row.CallIndex
		})
		for i := range rows {
			if !rows[i].failed {
				continue
			}
			intervening := 0
			for j := i + 1; j < len(rows); j++ {
				next := rows[j]
				if next.failed {
					break
				}
				if next.tool == rows[i].tool && next.normalized == rows[i].normalized {
					rows[i].recovered = true
					break
				}
				if intervening == 3 || !isRecoveryDiagnostic(next.tool, next.command) {
					break
				}
				intervening++
			}
		}
		bySession[sessionID] = rows
		for i, call := range rows {
			tool := call.tool
			ord, idx := call.row.MessageOrdinal, call.row.CallIndex
			githubRef := canonicalGitHubReferenceParts(call.row.Input, call.row.Result)
			if githubRef != "" && (call.failed || isGitHubInvocation(tool, call.command)) {
				severity := "low"
				if call.failed {
					severity = "medium"
				}
				e := a.evidence(sessionID, firstNonEmptyString(call.row.EventSource, "tool_call"), tool, githubRef, call.row.EventStatus, &ord, &idx, call.row.DurationMS)
				a.add("github-issue|"+githubRef, "github_issue_reference", tool, githubRef, severity, "high", "rule", e, call.row.DurationMS, nil, false)
			}
			if call.failed {
				sig := firstIssueLine(call.row.Result, call.row.Input)
				key := "failure|" + call.reason + "|" + tool + "|" + githubRef + "|" + normalizeIssueText(sig)
				e := a.evidence(sessionID, firstNonEmptyString(call.row.EventSource, "tool_result"), tool, sig, call.row.EventStatus, &ord, &idx, call.row.DurationMS)
				a.add(key, call.reason, tool, sig, failureSeverity(call.reason), issueFailureConfidence(call.row.EventStatus, call.row.Input, call.row.Result), recommendationFor(call.reason, 1, 1), e, call.row.DurationMS, call.row.DurationMS, call.recovered)
				if i+1 < len(rows) && rows[i+1].tool == tool && rows[i+1].normalized == call.normalized {
					next := rows[i+1]
					nOrd, nIdx := next.row.MessageOrdinal, next.row.CallIndex
					e = a.evidence(sessionID, "tool_call", tool, next.row.Input, next.row.EventStatus, &nOrd, &nIdx, next.row.DurationMS)
					a.add("retry|"+tool+"|"+call.normalized, "retry_after_failure", tool, next.row.Input, "medium", "high", "script", e, next.row.DurationMS, next.row.DurationMS, !next.failed)
				}
			}
			if eligibleWorkflow(tool, call.row.Input, call.command) {
				normalized, ok := normalizedInputs[call.row.Input]
				if !ok {
					normalized = normalizeIssueText(call.row.Input)
					normalizedInputs[call.row.Input] = normalized
				}
				key := tool + "|" + normalized
				acc := workflows[key]
				if acc == nil {
					acc = &workflowAccumulator{firstSession: call.row.SessionID, firstProject: a.sessions[call.row.SessionID].Project}
					workflows[key] = acc
				} else {
					acc.multiSession = acc.multiSession || call.row.SessionID != acc.firstSession
					acc.multiProject = acc.multiProject || a.sessions[call.row.SessionID].Project != acc.firstProject
				}
				acc.rows = append(acc.rows, call)
			}
		}
		for start := 0; start < len(rows); {
			end := start + 1
			for end < len(rows) && !rows[end].failed && !rows[start].failed && rows[end].tool == rows[start].tool && rows[end].normalized == rows[start].normalized {
				end++
			}
			threshold := 3
			if isWaitTool(rows[start].tool) {
				threshold = 4
			}
			if end-start >= threshold {
				call := rows[start]
				tool := call.tool
				reason := "repeated_polling"
				if isReadInvocation(tool, call.command) {
					reason = "repeated_read"
				}
				ord, idx := call.row.MessageOrdinal, call.row.CallIndex
				e := a.evidence(sessionID, "tool_call", tool, call.row.Input, call.row.EventStatus, &ord, &idx, call.row.DurationMS)
				for n := 0; n < end-start; n++ {
					a.add(reason+"|"+tool+"|"+call.normalized, reason, tool, call.row.Input, "low", "high", "script", e, call.row.DurationMS, call.row.DurationMS, false)
				}
			}
			start = end
		}
	}
	for key, workflow := range workflows {
		if !workflow.multiSession {
			continue
		}
		recommendation := "script"
		if workflow.multiProject {
			recommendation = "skill"
		}
		for _, row := range workflow.rows {
			ord, idx := row.row.MessageOrdinal, row.row.CallIndex
			e := a.evidence(row.row.SessionID, "tool_call", row.tool, row.row.Input, row.row.EventStatus, &ord, &idx, row.row.DurationMS)
			a.add("workflow|"+key, "repeated_workflow", row.tool, row.row.Input, "medium", "high", recommendation, e, row.row.DurationMS, row.row.DurationMS, false)
		}
	}
	addSlowToolFindings(a, calls, toolCounts)
	addMessageFindings(a, messages)
	addTelemetryFindings(a, telemetry)
	findings := a.finish(len(calls), durationCounts)
	facets := issueFacets(findings, sessions)
	return IssueReviewResponse{GeneratedAt: time.Now().UTC().Format(time.RFC3339), ScannedSessions: len(sessions), ScannedMessages: rawMessages, ScannedToolCalls: rawCalls, AnalyzedMessages: len(messages), AnalyzedToolCalls: len(calls), DuplicateMessages: duplicateMessages, DuplicateToolCalls: duplicateCalls, ScannedTelemetry: len(telemetry), TotalFindings: len(findings), Findings: findings, Facets: facets}
}

func filterIssueReviewResponse(response IssueReviewResponse, q IssueReviewQuery) IssueReviewResponse {
	filtered := make([]IssueReviewFinding, 0, len(response.Findings))
	for _, finding := range response.Findings {
		if q.Reason != "" && finding.ReasonCode != q.Reason || q.Tool != "" && finding.Tool != q.Tool || q.Source != "" && !containsIssueString(finding.Sources, q.Source) || q.Severity != "" && finding.Severity != q.Severity || q.Confidence != "" && finding.Confidence != q.Confidence || q.Status != "" && finding.Status != q.Status || q.RecommendationType != "" && finding.RecommendationType != q.RecommendationType || finding.Occurrences < max(1, q.MinOccurrences) || finding.SessionCount < max(1, q.MinSessions) || finding.ProjectCount < q.MinProjects || finding.WastedDurationMS < q.MinWastedDurationMS {
			continue
		}
		filtered = append(filtered, finding)
	}
	sortIssueFindings(filtered, q.Sort)
	totalFindings := len(filtered)
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	offset := min(max(0, q.Offset), totalFindings)
	end := min(offset+limit, totalFindings)
	truncated := end < totalFindings
	filtered = filtered[offset:end]
	response.TotalFindings = totalFindings
	response.Truncated = truncated
	response.Findings = filtered
	return response
}

func containsIssueString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func sortIssueFindings(findings []IssueReviewFinding, mode string) {
	sort.SliceStable(findings, func(i, j int) bool {
		left, right := findings[i], findings[j]
		switch mode {
		case "frequency":
			if left.Occurrences != right.Occurrences {
				return left.Occurrences > right.Occurrences
			}
		case "recent":
			if left.LastSeen != right.LastSeen {
				return left.LastSeen > right.LastSeen
			}
		case "waste":
			if left.WastedDurationMS != right.WastedDurationMS {
				return left.WastedDurationMS > right.WastedDurationMS
			}
		case "duration":
			if left.TotalDurationMS != right.TotalDurationMS {
				return left.TotalDurationMS > right.TotalDurationMS
			}
		default:
			if left.rank != right.rank {
				return left.rank > right.rank
			}
		}
		return left.ID < right.ID
	})
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func firstIssueLine(result, input string) string {
	for _, source := range []string{result, issueCommandInput(input)} {
		contentBlocks := strings.Contains(source, `"type"`) && strings.Contains(source, `"text"`)
		if decoded := parser.DecodeContent(source); json.Valid([]byte(source)) && decoded != "" {
			source = decoded
			contentBlocks = false
		}
		source = strings.ReplaceAll(source, `\r\n`, "\n")
		source = strings.ReplaceAll(source, `\r`, "\n")
		source = strings.ReplaceAll(source, `\n`, "\n")
		candidate := ""
		errorLine := ""
		for _, line := range strings.Split(source, "\n") {
			line = strings.TrimSpace(line)
			if contentBlocks {
				line = stripIssueContentBlock(line)
			}
			if line == "" || isIssueWrapperLine(line) {
				continue
			}
			if candidate == "" {
				candidate = line
			}
			if errorLine == "" && errorWordRE.MatchString(line) {
				errorLine = line
			}
		}
		if errorLine != "" {
			return errorLine
		}
		if candidate != "" {
			return candidate
		}
	}
	return "Tool failure"
}

func stripIssueContentBlock(line string) string {
	if start := strings.Index(line, `{"type"`); start >= 0 && start < 32 {
		if text := strings.Index(line[start:], `"text":"`); text >= 0 && text < 96 {
			line = line[start+text+len(`"text":"`):]
		}
	}
	for _, suffix := range []string{`"}]`, `"},`, `"}`} {
		line = strings.TrimSuffix(line, suffix)
	}
	return strings.TrimSpace(line)
}

func isIssueWrapperLine(line string) bool {
	lower := strings.ToLower(strings.TrimSpace(line))
	if lower == "script failed" || lower == "script error" || lower == "script error:" || lower == "script completed" || lower == "output:" || lower == "final output:" {
		return true
	}
	for _, prefix := range []string{"wall time:", "wall time ", "process exited with code", "exit code:", "exit code ", "warning: truncated output", "total output lines:"} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

func failureSeverity(reason string) string {
	switch reason {
	case "permission_auth", "tool_crash", "git_github_ci", "build_test":
		return "high"
	case "generic_tool_failure", "github_issue_reference", "repeated_read", "repeated_polling":
		return "low"
	default:
		return "medium"
	}
}

func recommendationFor(reason string, sessions, projects int) string {
	switch reason {
	case "tool_crash", "network", "rate_limit", "timeout", "app_session_error", "tool_router_error":
		return "tool_fix"
	case "windows_shell", "shell_syntax", "line_endings", "permission_auth", "user_correction", "github_issue_reference":
		return "rule"
	case "repeated_polling", "repeated_read", "retry_after_failure", "slow_tool", "command_failure":
		return "script"
	case "repeated_workflow":
		if projects > 1 {
			return "skill"
		}
		return "script"
	case "repeated_question":
		if projects > 1 {
			return "skill"
		}
		return "rule"
	default:
		if sessions > 1 {
			return "skill"
		}
		return "script"
	}
}

func concreteRecommendation(f IssueReviewFinding) string {
	tool := f.Tool
	if tool == "" {
		tool = "this workflow"
	}
	switch f.ReasonCode {
	case "missing_file":
		return "Add a path-existence preflight and resolve the exact working directory before rerunning " + tool + "."
	case "missing_dependency":
		return "Add a dependency preflight for " + tool + " and print one exact install or fallback command when it is missing."
	case "permission_auth":
		return "Check authorization and required privileges before " + tool + "; stop before any protected write when the check fails."
	case "rate_limit":
		return "Honor Retry-After, add bounded exponential backoff, and cache repeated read-only requests made through " + tool + "."
	case "network":
		return "Add a connectivity preflight and bounded retry with the endpoint and final network error preserved for " + tool + "."
	case "timeout":
		return "Profile " + tool + ", split oversized work, and replace fixed polling with completion events or a measured timeout."
	case "windows_shell", "shell_syntax":
		return "Move complex shell logic into a checked script, validate arguments and paths, and propagate the first failing exit code."
	case "line_endings":
		return "Normalize line endings at the comparison boundary and keep tests portable across Windows and CI."
	case "git_github_ci":
		return "Run read-only git and GitHub preflights first, preserve the exact failing command, and retry only after repository state changes."
	case "github_issue_reference":
		return "Open " + f.GitHubReference + ", record whether it blocks the task, and link the chosen workaround or follow-up rule."
	case "failed_edit":
		return "Re-read the exact target range, apply one smaller context patch, and do not repeat the same edit after an unchanged failure."
	case "build_test":
		return "Run the narrow failing check first, fix its first stable failure, then rerun the full suite once."
	case "tool_crash":
		return "Capture the tool version, crash signature, and minimal safe input, then isolate a reproducible tool-level fix."
	case "command_failure":
		return "Preserve the first failing command and exit code, split compound shell logic, and retry only the failed step after a material change."
	case "retry_after_failure":
		return "Require a changed input or external state before retrying " + tool + ", then verify the intended outcome explicitly."
	case "repeated_read":
		return "Cache this stable read or request a narrower range; read it again only after the source changes."
	case "repeated_polling":
		return "Replace fixed polling with an event-driven wait or bounded backoff and an explicit stop condition."
	case "slow_tool":
		return "Profile " + tool + " at p95, then batch, cache, or parallelize only the measured slow stage."
	case "repeated_workflow":
		if f.ProjectCount > 1 {
			return "Package this repeated " + tool + " workflow as a reusable skill with preflight, stop conditions, and one verification command."
		}
		return "Extract this repeated " + tool + " workflow into a project script with idempotent inputs and one verification command."
	case "repeated_question":
		if f.ProjectCount > 1 {
			return "Turn this recurring request into a reusable skill with explicit inputs, scope, and one verification step."
		}
		return "Add a project rule or request template that fixes the expected scope, output, and verification step."
	case "user_correction":
		return "Add a rule that confirms scope, expected output, and exclusions before taking the corrected action."
	case "reported_blocker":
		return "Add a preflight for this blocker and document the smallest safe recovery path before repeating the workflow."
	case "response_retry":
		return "Measure response retries by cause, cap them, and surface the final provider error instead of silently looping."
	case "tool_router_error":
		return "Validate the tool name and arguments before routing, and preserve the rejected call shape for diagnosis."
	case "hook_failure":
		return "Run the hook in isolation, validate its runtime and exit code, and disable repeated unchanged retries."
	case "app_session_error":
		return "Capture the app session error with its task ID and lifecycle state, then verify recovery in a fresh session."
	case "shell_snapshot_failure":
		return "Rebuild the shell snapshot once after validating the shell path and startup profile."
	default:
		return "Preserve the first error from " + tool + ", change one material input before retrying, and verify the intended outcome."
	}
}

func eligibleWorkflow(tool, input, command string) bool {
	if isWaitTool(tool) || isReadInvocation(tool, command) || len(strings.TrimSpace(input)) < 80 {
		return false
	}
	lower := strings.ToLower(strings.TrimSpace(input))
	for _, prefix := range []string{"git status", "pwd", "get-location", "ls", "dir", "rg ", "grep ", "find ", "get-childitem", "get-content"} {
		if strings.HasPrefix(lower, prefix) {
			return false
		}
	}
	return strings.ContainsAny(input, "\n;|") || len(input) >= 180
}

func isWaitTool(tool string) bool {
	t := normalizeTool(tool)
	return strings.Contains(t, "wait") || t == "sleep" || t == "await" || t == "awaitshell"
}

func isReadInvocation(tool, command string) bool {
	t := normalizeTool(tool)
	for _, term := range []string{"read", "view_file", "get_file", "read_mcp_resource"} {
		if t == term || strings.Contains(t, "read_file") {
			return true
		}
	}
	lower := strings.ToLower(strings.TrimSpace(command))
	for _, prefix := range []string{"get-content ", "cat ", "type ", "head ", "tail ", "sed -n "} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

func isRecoveryDiagnostic(tool, command string) bool {
	t := normalizeTool(tool)
	if isShellTool(t) {
		command = strings.ToLower(strings.TrimSpace(command))
		if strings.ContainsAny(command, "\r\n;|&") {
			return false
		}
	}
	if isWaitTool(tool) || isReadInvocation(tool, command) || isSearchInvocation(tool, command) {
		return true
	}
	if t == "status" || t == "location" || t == "list" || strings.HasPrefix(t, "get_status") || strings.HasPrefix(t, "get_location") || strings.HasPrefix(t, "list_") {
		return true
	}
	if !isShellTool(t) {
		return false
	}
	for _, diagnostic := range []string{"git status", "pwd", "get-location", "ls", "dir", "get-childitem"} {
		if command == diagnostic || strings.HasPrefix(command, diagnostic+" ") {
			return true
		}
	}
	return false
}

func addSlowToolFindings(a *issueAnalyzer, calls []IssueReviewToolCall, toolCounts map[string]int) {
	byTool := map[string][]IssueReviewToolCall{}
	for _, call := range calls {
		tool := effectiveIssueTool(call.Tool, call.Input)
		if call.DurationMS != nil && *call.DurationMS >= 0 && !isWaitTool(tool) {
			byTool[tool] = append(byTool[tool], call)
		}
	}
	for tool, rows := range byTool {
		durations := make([]int64, len(rows))
		var maxDuration int64
		for i, row := range rows {
			durations[i] = *row.DurationMS
			if durations[i] > maxDuration {
				maxDuration = durations[i]
			}
		}
		sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
		p95 := durations[int(math.Ceil(float64(len(durations))*0.95))-1]
		if !(len(rows) >= 3 && p95 >= 30000) && maxDuration < 120000 {
			continue
		}
		severity := "medium"
		if maxDuration >= 120000 {
			severity = "high"
		}
		for _, row := range rows {
			if *row.DurationMS < 30000 && *row.DurationMS < 120000 {
				continue
			}
			waste := *row.DurationMS - 30000
			if waste < 0 {
				waste = 0
			}
			ord, idx := row.MessageOrdinal, row.CallIndex
			e := a.evidence(row.SessionID, firstNonEmptyString(row.DurationSource, "tool_execution"), tool, row.Input, row.EventStatus, &ord, &idx, row.DurationMS)
			a.add("slow|"+tool, "slow_tool", tool, tool, severity, "high", "tool_fix", e, row.DurationMS, &waste, false)
		}
		acc := a.clusters["slow|"+tool]
		acc.finding.DurationSource = firstNonEmptyString(rows[0].DurationSource, "tool_execution")
		acc.statsDurations = durations
		acc.measuredCalls = len(rows)
		acc.toolCalls = toolCounts[tool]
	}
}

func addMessageFindings(a *issueAnalyzer, messages []IssueReviewMessage) {
	repeatedQuestions := map[string][]IssueReviewMessage{}
	for _, message := range messages {
		if message.IsSystem {
			continue
		}
		content := strings.TrimSpace(message.Content)
		if isHarnessEnvelope(content) {
			continue
		}
		if ref := canonicalGitHubReference(content); ref != "" {
			ord := message.Ordinal
			e := a.evidence(message.SessionID, firstNonEmptyString(message.SourceType, "message"), "", ref, "", &ord, nil, nil)
			a.add("github-issue|"+ref, "github_issue_reference", "", ref, "low", "high", "rule", e, nil, nil, false)
		}
		if message.Role == "user" {
			if key, ok := repeatedQuestionKey(content); ok {
				repeatedQuestions[key] = append(repeatedQuestions[key], message)
			}
			if !isStrongCorrection(content) {
				continue
			}
			ord := message.Ordinal
			e := a.evidence(message.SessionID, "user_message", "", content, "", &ord, nil, nil)
			a.add("correction|"+normalizeIssueText(content), "user_correction", "", content, "medium", "medium", "rule", e, nil, nil, false)
			continue
		}
		if message.Role != "assistant" || len(content) < 40 || !isAssistantBlocker(message, content) {
			continue
		}
		reason, ok := classifyIssueReason(content)
		if !ok {
			reason = "reported_blocker"
		}
		ord := message.Ordinal
		e := a.evidence(message.SessionID, "assistant_commentary", "", content, "", &ord, nil, nil)
		a.add("blocker|"+reason+"|"+normalizeIssueText(content), reason, "", content, failureSeverity(reason), "medium", recommendationFor(reason, 1, 1), e, nil, nil, false)
	}
	for key, rows := range repeatedQuestions {
		if len(rows) < 2 {
			continue
		}
		for _, message := range rows {
			ord := message.Ordinal
			e := a.evidence(message.SessionID, "user_message", "", message.Content, "", &ord, nil, nil)
			a.add("question|"+key, "repeated_question", "", message.Content, "low", "high", "rule", e, nil, nil, false)
		}
	}
}

func repeatedQuestionKey(content string) (string, bool) {
	if isHarnessEnvelope(content) {
		return "", false
	}
	key := normalizeIssueText(content)
	return key, len(key) >= 32 && len(strings.Fields(key)) >= 6
}

func isHarnessEnvelope(content string) bool {
	lower := strings.ToLower(content)
	for _, marker := range []string{
		"<instructions>", "<environment_context>", "<recommended_plugins>", "<skills_instructions>", "<app-context>",
		"<task-notification>", "<subagent_notification>", "message type: new_task", "# agents.md instructions",
		"perform any necessary follow-up actions in response to the subagent completion above",
		"briefly inform the user about the task result",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func isStrongCorrection(content string) bool {
	lower := strings.ToLower(strings.TrimSpace(content))
	for _, term := range correctionTerms {
		if strings.HasPrefix(lower, term) || strings.Contains(lower, " "+term) {
			return true
		}
	}
	return false
}

func isAssistantBlocker(message IssueReviewMessage, content string) bool {
	lower := strings.ToLower(content)
	search := lower
	if message.SourceType != "event_msg" || message.SourceSubtype != "commentary" {
		if len(search) > 500 {
			search = search[:500]
		}
	}
	strong, broad := false, false
	for _, term := range blockerTerms {
		if strings.Contains(search, term) {
			if term == "hit a " || term == "exposed a " || term == "exposed an " {
				broad = true
			} else {
				strong = true
			}
		}
	}
	if !strong && (!broad || !blockerPredicateRE.MatchString(search)) {
		return false
	}
	if message.SourceType == "event_msg" && message.SourceSubtype == "commentary" {
		return true
	}
	return message.SourceType == "" && len(content) <= 500 && (strings.HasPrefix(search, "root cause") || strings.HasPrefix(search, "the ") || strings.HasPrefix(search, "git"))
}

func addTelemetryFindings(a *issueAnalyzer, telemetry []IssueReviewTelemetry) {
	for _, row := range telemetry {
		if row.DurationMS != nil {
			continue
		}
		reason := telemetryReason(row.Target)
		if reason == "" {
			continue
		}
		confidence := "medium"
		severity := "medium"
		if strings.EqualFold(row.Level, "ERROR") {
			confidence, severity = "high", "high"
		}
		tail := sanitizeTelemetryTail(row.Body)
		if tail == "" {
			continue
		}
		e := a.evidence(row.SessionID, "codex_log", "", tail, row.Level, nil, nil, nil)
		a.add("log|"+reason+"|"+normalizeIssueText(tail), reason, "", tail, severity, confidence, recommendationFor(reason, 1, 1), e, nil, nil, false)
	}
}

func telemetryReason(target string) string {
	switch target {
	case "codex_core::responses_retry":
		return "response_retry"
	case "codex_core::tools::router":
		return "tool_router_error"
	case "codex_core::hook_runtime":
		return "hook_failure"
	case "codex_core::session::turn":
		return "app_session_error"
	case "codex_core::shell_snapshot":
		return "shell_snapshot_failure"
	default:
		return ""
	}
}

func logTail(body string) string {
	if i := strings.LastIndex(body, ": "); i >= 0 && i+2 < len(body) {
		return body[i+2:]
	}
	return body
}

func sanitizeTelemetryTail(body string) string {
	tail := strings.TrimSpace(logTail(body))
	if tail == "" {
		return ""
	}
	tail = credentialFieldRE.ReplaceAllString(tail, "$1=<redacted>")
	tail = pathFieldRE.ReplaceAllString(tail, "$1=<path>")
	tail = bearerRE.ReplaceAllString(tail, "Bearer <redacted>")
	tail = windowsPathRE.ReplaceAllString(tail, "<path>")
	tail = unixPathRE.ReplaceAllString(tail, " <path>")
	return issueExcerpt(secrets.Redact(tail))
}

func issueFacets(findings []IssueReviewFinding, sessions []IssueReviewSession) IssueReviewFacets {
	maps := map[string]map[string]int{"category": {}, "tool": {}, "source": {}, "severity": {}, "confidence": {}, "status": {}, "recommendation_type": {}, "session": {}, "folder": {}, "outcome": {}}
	labels := map[string]string{}
	for _, finding := range findings {
		for key, value := range map[string]string{"category": finding.ReasonCode, "tool": finding.Tool, "severity": finding.Severity, "confidence": finding.Confidence, "status": finding.Status, "recommendation_type": finding.RecommendationType} {
			if value != "" {
				maps[key][value]++
			}
		}
		for _, source := range finding.Sources {
			maps["source"][source]++
		}
	}
	for _, session := range sessions {
		maps["session"][session.ID]++
		label := firstNonEmptyString(strings.Join(strings.Fields(session.Name), " "), session.Project, session.ID)
		if session.Date != "" {
			label += " · " + session.Date
		}
		labels[session.ID] = label
		if session.CWD != "" {
			maps["folder"][session.CWD]++
		}
		if session.Outcome != "" {
			maps["outcome"][session.Outcome]++
		}
	}
	out := make(map[string][]IssueFacet, len(maps))
	for key, counts := range maps {
		for value, count := range counts {
			facet := IssueFacet{Value: value, Count: count}
			if key == "session" {
				facet.Label = labels[value]
			}
			out[key] = append(out[key], facet)
		}
		sort.Slice(out[key], func(i, j int) bool {
			if out[key][i].Count != out[key][j].Count {
				return out[key][i].Count > out[key][j].Count
			}
			left, right := out[key][i].Value, out[key][j].Value
			if key == "session" {
				left, right = out[key][i].Label, out[key][j].Label
			}
			return left < right
		})
	}
	return IssueReviewFacets{
		Category: out["category"], Tool: out["tool"], Source: out["source"], Severity: out["severity"],
		Confidence: out["confidence"], Status: out["status"],
		RecommendationType: out["recommendation_type"], Session: out["session"], Folder: out["folder"],
		Outcome: out["outcome"],
	}
}

// GetAnalyticsIssueReview implements the local archive query and optional
// read-only Codex telemetry supplement.
func (db *DB) GetAnalyticsIssueReview(ctx context.Context, f AnalyticsFilter, q IssueReviewQuery) (IssueReviewResponse, error) {
	key := IssueReviewCacheKey(f, q)
	if cached, ok := db.issueReviewCache.Get(key, q); ok {
		return cached, nil
	}
	sessions, err := db.issueReviewSessions(ctx, f, q)
	if err != nil {
		return IssueReviewResponse{}, err
	}
	messages, calls, err := db.issueReviewRows(ctx, sessions)
	if err != nil {
		return IssueReviewResponse{}, err
	}
	telemetry, telemetryStatus := db.issueReviewTelemetry(ctx, sessions, calls)
	response := AnalyzeIssueReviewBase(sessions, messages, calls, telemetry)
	response.TelemetryStatus = telemetryStatus
	db.issueReviewCache.Put(key, response)
	return filterIssueReviewResponse(response, q), nil
}

// IssueReviewCacheKey identifies the expensive base-analysis scope.
func IssueReviewCacheKey(f AnalyticsFilter, q IssueReviewQuery) string {
	value, _ := json.Marshal(struct {
		Filter    AnalyticsFilter
		SessionID string
		Folder    string
		Outcome   string
	}{Filter: f, SessionID: q.SessionID, Folder: q.Folder, Outcome: q.Outcome})
	return string(value)
}

func (db *DB) issueReviewSessions(ctx context.Context, f AnalyticsFilter, q IssueReviewQuery) ([]IssueReviewSession, error) {
	dateCol := "COALESCE(NULLIF(started_at, ''), created_at)"
	where, args := f.buildWhere(dateCol)
	if q.SessionID != "" {
		where += " AND id = ?"
		args = append(args, q.SessionID)
	}
	rows, err := db.getReader().QueryContext(ctx, `SELECT id, SUBSTR(COALESCE(NULLIF(display_name,''),NULLIF(session_name,''),NULLIF(first_message,''),NULLIF(project,''),id),1,160), project, cwd, agent, `+dateCol+`, outcome FROM sessions WHERE `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("querying issue review sessions: %w", err)
	}
	defer rows.Close()
	loc := f.location()
	var out []IssueReviewSession
	for rows.Next() {
		var row IssueReviewSession
		var ts string
		if err := rows.Scan(&row.ID, &row.Name, &row.Project, &row.CWD, &row.Agent, &ts, &row.Outcome); err != nil {
			return nil, fmt.Errorf("scanning issue review session: %w", err)
		}
		row.Date = localDate(ts, loc)
		row.Incomplete = row.Outcome == "errored" || row.Outcome == "abandoned"
		if q.Folder != "" && row.CWD != q.Folder || q.Outcome != "" && row.Outcome != q.Outcome {
			continue
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (db *DB) issueReviewRows(ctx context.Context, sessions []IssueReviewSession) ([]IssueReviewMessage, []IssueReviewToolCall, error) {
	ids := make([]string, len(sessions))
	for i, s := range sessions {
		ids[i] = s.ID
	}
	var messages []IssueReviewMessage
	var calls []IssueReviewToolCall
	err := queryChunkedSize(ids, 400, func(chunk []string) error {
		ph, args := inPlaceholders(chunk)
		rows, err := db.getReader().QueryContext(ctx, `SELECT session_id, ordinal, role, substr(content,1,?), COALESCE(timestamp,''), is_system, source_type, source_subtype, COALESCE(NULLIF(source_uuid,''),NULLIF(claude_message_id,''),'') FROM messages WHERE session_id IN `+ph+` AND NOT is_system AND `+IssueReviewMessagePredicate("role", "content")+` ORDER BY session_id,ordinal`, append([]any{IssueReviewMessageScanLimit}, args...)...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var r IssueReviewMessage
			if err := rows.Scan(&r.SessionID, &r.Ordinal, &r.Role, &r.Content, &r.Timestamp, &r.IsSystem, &r.SourceType, &r.SourceSubtype, &r.StableID); err != nil {
				rows.Close()
				return err
			}
			messages = append(messages, r)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		queryArgs := append([]any{}, args...)
		queryArgs = append(queryArgs, IssueReviewInputLimit, IssueReviewResultEdgeLimit)
		queryArgs = append(queryArgs, args...)
		result := "COALESCE(es.content,tc.result_content,'')"
		rows, err = db.getReader().QueryContext(ctx, `WITH events AS (
			SELECT tre.*,
				ROW_NUMBER() OVER (PARTITION BY tre.session_id,tre.tool_call_message_ordinal,tre.call_index ORDER BY tre.event_index DESC,tre.id DESC) AS latest_rank,
				MIN(CASE WHEN tre.source='tool_execution' AND tre.status='started' THEN tre.timestamp END) OVER (PARTITION BY tre.session_id,tre.tool_call_message_ordinal,tre.call_index) AS started,
				MAX(CASE WHEN tre.source='tool_execution' AND tre.status IN ('completed','errored') THEN tre.timestamp END) OVER (PARTITION BY tre.session_id,tre.tool_call_message_ordinal,tre.call_index) AS ended
			FROM tool_result_events tre WHERE tre.session_id IN `+ph+`
		), event_summary AS (
			SELECT session_id,tool_call_message_ordinal,call_index,content,status,source,started,ended FROM events WHERE latest_rank=1
		)
		SELECT tc.session_id,m.ordinal,COALESCE(tc.call_index,0),tc.tool_name,tc.category,COALESCE(tc.tool_use_id,''),substr(COALESCE(tc.input_json,''),1,?),substr(`+result+`,1,?),CASE WHEN `+IssueReviewTailPredicate("es.status", result)+` THEN substr(`+result+`,-`+strconv.Itoa(IssueReviewResultEdgeLimit)+`) ELSE '' END,COALESCE(es.status,''),COALESCE(es.source,''),COALESCE(m.timestamp,''),es.started,es.ended
		FROM tool_calls tc JOIN messages m ON m.id=tc.message_id
		LEFT JOIN event_summary es ON es.session_id=tc.session_id AND es.tool_call_message_ordinal=m.ordinal AND es.call_index=COALESCE(tc.call_index,0)
		WHERE tc.session_id IN `+ph+` ORDER BY tc.session_id,m.ordinal,tc.call_index`, queryArgs...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var r IssueReviewToolCall
			var resultHead, resultTail string
			var started, ended sql.NullString
			if err := rows.Scan(&r.SessionID, &r.MessageOrdinal, &r.CallIndex, &r.Tool, &r.Category, &r.ToolUseID, &r.Input, &resultHead, &resultTail, &r.EventStatus, &r.EventSource, &r.Timestamp, &started, &ended); err != nil {
				rows.Close()
				return err
			}
			r.Result = JoinIssueReviewResult(resultHead, resultTail)
			r.DurationMS = IssueDuration(started.String, ended.String)
			if r.DurationMS != nil {
				r.DurationSource = "tool_execution"
			}
			calls = append(calls, r)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		return rows.Close()
	})
	return messages, calls, err
}

// IssueDuration returns a measured event duration when both timestamps exist.
func IssueDuration(started, ended string) *int64 {
	if started == "" || ended == "" {
		return nil
	}
	a, err := time.Parse(time.RFC3339Nano, started)
	if err != nil {
		return nil
	}
	b, err := time.Parse(time.RFC3339Nano, ended)
	if err != nil || b.Before(a) {
		return nil
	}
	v := b.Sub(a).Milliseconds()
	return &v
}

func (db *DB) issueReviewTelemetry(ctx context.Context, sessions []IssueReviewSession, calls []IssueReviewToolCall) ([]IssueReviewTelemetry, string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, "unavailable"
	}
	return readIssueReviewTelemetry(ctx, filepath.Join(home, ".codex", "logs_2.sqlite"), sessions, calls)
}

func readIssueReviewTelemetry(ctx context.Context, path string, sessions []IssueReviewSession, calls []IssueReviewToolCall) ([]IssueReviewTelemetry, string) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, "missing"
		}
		return nil, "unavailable"
	}
	conn, err := sql.Open("sqlite3", makeDSN(path, true))
	if err != nil {
		return nil, "unavailable"
	}
	defer conn.Close()
	ids := make([]string, len(sessions))
	allowed := make(map[string]bool, len(sessions))
	for i, s := range sessions {
		ids[i] = s.ID
		allowed[s.ID] = true
	}
	callByID := map[string]*IssueReviewToolCall{}
	for i := range calls {
		if calls[i].ToolUseID != "" {
			callByID[calls[i].SessionID+"|"+calls[i].ToolUseID] = &calls[i]
		}
	}
	var out []IssueReviewTelemetry
	err = queryChunkedSize(ids, 400, func(chunk []string) error {
		ph, args := inPlaceholders(chunk)
		rows, err := conn.QueryContext(ctx, `SELECT COALESCE(thread_id,''),target,level,COALESCE(feedback_log_body,''),ts FROM logs WHERE thread_id IN `+ph+` AND (target='codex_core::tools::parallel' OR target IN ('codex_core::responses_retry','codex_core::tools::router','codex_core::hook_runtime','codex_core::session::turn','codex_core::shell_snapshot')) AND (target='codex_core::tools::parallel' OR level IN ('WARN','ERROR')) ORDER BY ts,ts_nanos,id`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r IssueReviewTelemetry
			var unix int64
			if err := rows.Scan(&r.SessionID, &r.Target, &r.Level, &r.Body, &unix); err != nil {
				return err
			}
			if !allowed[r.SessionID] {
				continue
			}
			r.Timestamp = time.Unix(unix, 0).UTC().Format(time.RFC3339)
			if r.Target == "codex_core::tools::parallel" {
				fields := parseLogFields(r.Body)
				if !strings.Contains(r.Body, "tool call completed") {
					continue
				}
				r.Tool, r.CallID = fields["tool_name"], fields["call_id"]
				ms, err := strconv.ParseInt(fields["total_duration_ms"], 10, 64)
				if err != nil || ms < 0 {
					continue
				}
				r.DurationMS = &ms
				call := callByID[r.SessionID+"|"+r.CallID]
				if call == nil {
					continue
				}
				call.DurationMS = &ms
				call.DurationSource = "codex_log"
				continue
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, "unavailable"
	}
	return out, "available"
}

func parseLogFields(body string) map[string]string {
	out := map[string]string{}
	for _, m := range logFieldRE.FindAllStringSubmatch(body, -1) {
		value := m[2]
		if value == "" {
			value = m[3]
		}
		out[m[1]] = value
	}
	return out
}
