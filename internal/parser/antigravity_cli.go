package parser

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// maxTrajectorySidecarBytes caps how much of a <uuid>.trajectory.json
// sidecar the parser will read. Real transcripts observed at the time
// of writing are well under 1 MB; the cap is a defense against a
// buggy or hostile sidecar writer dropping a multi-GB file in the
// session directory. See SECURITY.md ("Imports and new readers"):
// sidecars are treated as untrusted structured input.
const maxTrajectorySidecarBytes = 64 << 20

// Antigravity CLI sessions live under ~/.gemini/antigravity-cli/:
//
//   conversations/<uuid>.db      SQLite trajectory DB (newer CLI releases)
//   conversations/<uuid>.pb      AES-encrypted conversation stream
//   implicit/<uuid>.pb           AES-encrypted implicit conversation
//   brain/<uuid>/*.md(+.json)    plaintext task/plan/walkthrough docs
//   history.jsonl                user-prompt log (one row per turn)
//   cache/last_conversations.json   workspace -> conversationId
//
// Without the AES key (ANTIGRAVITY_KEY) we still produce a useful
// session from the brain/ artifacts and the matching history.jsonl
// rows. With the key we additionally append the decrypted transcript
// preview (raw extracted strings) as a synthetic assistant message.

const (
	antigravityCLIIDPrefix = "antigravity-cli:"

	// antigravityImplicitTag distinguishes implicit/<uuid>.pb from
	// conversations/<uuid>.pb in the storage ID. Both can exist for
	// the same UUID; without a tag they collide as
	// "antigravity-cli:<uuid>" and one record overwrites the other.
	// Hyphen keeps the tag inside the IsValidSessionID charset.
	antigravityImplicitTag = "implicit-"
)

// DiscoverAntigravityCLISessions enumerates conversations/*.db,
// conversations/*.pb, and implicit/*.pb under the CLI root and tags each with
// its workspace (resolved via history.jsonl).
func DiscoverAntigravityCLISessions(root string) []DiscoveredFile {
	if root == "" {
		return nil
	}
	projects := buildAntigravityProjectMap(
		filepath.Join(root, "history.jsonl"),
	)
	var files []DiscoveredFile
	for _, sub := range []string{"conversations", "implicit"} {
		dir := filepath.Join(root, sub)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		byID := make(map[string]string)
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			id, ext, ok := antigravityCLIPathID(name)
			if !ok || (sub == "implicit" && ext != ".pb") {
				continue
			}
			// Prefer the new SQLite source when both old and new files
			// exist for a conversation. They share a storage ID.
			if prev := byID[id]; prev == "" ||
				(strings.HasSuffix(prev, ".pb") && ext == ".db") {
				byID[id] = filepath.Join(dir, name)
			}
		}
		for id, path := range byID {
			files = append(files, DiscoveredFile{
				Path:    path,
				Project: projects[id],
				Agent:   AgentAntigravityCLI,
			})
		}
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return files
}

// FindAntigravityCLISourceFile locates the source file for a session
// id (without the agent prefix). An "implicit-" prefix routes to
// the implicit/ subdir; bare ids resolve under conversations/.
func FindAntigravityCLISourceFile(root, id string) string {
	if root == "" {
		return ""
	}
	if uuid, ok := strings.CutPrefix(id, antigravityImplicitTag); ok {
		if !IsValidSessionID(uuid) {
			return ""
		}
		for _, ext := range []string{".pb"} {
			p := filepath.Join(root, "implicit", uuid+ext)
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
		return ""
	}
	if !IsValidSessionID(id) {
		return ""
	}
	for _, ext := range []string{".db", ".pb"} {
		p := filepath.Join(root, "conversations", id+ext)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func antigravityCLIPathID(name string) (string, string, bool) {
	for _, ext := range []string{".db", ".pb"} {
		if !strings.HasSuffix(name, ext) {
			continue
		}
		id := strings.TrimSuffix(name, ext)
		if IsValidSessionID(id) {
			return id, ext, true
		}
	}
	return "", "", false
}

// ParseAntigravityCLISession parses one CLI session into the
// canonical ParsedSession + messages shape.
func ParseAntigravityCLISession(
	path, project, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	sess, msgs, _, err := ParseAntigravityCLISessionWithStatus(
		path, project, machine,
	)
	return sess, msgs, err
}

// AntigravityCLIParseStatus carries sync-relevant parser metadata
// that is not part of the canonical session/message shape.
type AntigravityCLIParseStatus struct {
	// NeedsRetry is true when a high-resolution source failed to
	// decode and the parser returned lower-resolution fallback
	// messages instead. Sync can store the fallback while leaving
	// data_version stale so the next pass retries.
	NeedsRetry bool
}

// ParseAntigravityCLISessionWithStatus parses one CLI session into
// the canonical ParsedSession + messages shape and reports whether
// the result should be retried on the next sync.
func ParseAntigravityCLISessionWithStatus(
	path, project, machine string,
) (*ParsedSession, []ParsedMessage, AntigravityCLIParseStatus, error) {
	var status AntigravityCLIParseStatus
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, status, fmt.Errorf("stat %s: %w", path, err)
	}
	id, ext, ok := antigravityCLIPathID(filepath.Base(path))
	if !ok {
		return nil, nil, status, fmt.Errorf(
			"invalid Antigravity CLI session filename: %s", path,
		)
	}
	// Root = two levels up from the source file
	// (conversations/<id>.db, conversations/<id>.pb, or implicit/<id>.pb).
	root := filepath.Dir(filepath.Dir(path))
	// Tag implicit/ sessions so they don't collide with the
	// conversations/ entry that may share the same UUID.
	storageID := id
	if filepath.Base(filepath.Dir(path)) == "implicit" {
		storageID = antigravityImplicitTag + id
	}

	var messages []ParsedMessage
	var hasTrajectory bool
	if ext == ".db" {
		if result, err := loadAntigravityCLIDBSteps(path); err == nil {
			if hasDisplayableAntigravityCLITrajectoryMessage(result.messages) {
				messages = mergeAntigravityDBHistoryMessages(
					result.messages,
					collectAntigravityHistoryMessages(
						filepath.Join(root, "history.jsonl"), id,
					),
				)
				hasTrajectory = true
			} else if result.rawStepCount > 0 {
				status.NeedsRetry = true
			}
		} else {
			status.NeedsRetry = true
		}
	} else {
		sidecarPath := strings.TrimSuffix(path, ".pb") + ".trajectory.json"
		if sidecarInfo, err := os.Stat(sidecarPath); err == nil &&
			!sidecarInfo.ModTime().Before(info.ModTime()) {
			if tMsgs, err := parseAntigravityCLITrajectory(sidecarPath); err == nil {
				if hasDisplayableAntigravityCLITrajectoryMessage(tMsgs) {
					messages = tMsgs
					hasTrajectory = true
				}
			}
		}
	}

	if !hasTrajectory {
		messages = collectAntigravityHistoryMessages(
			filepath.Join(root, "history.jsonl"), id,
		)
	}
	messages = append(messages,
		collectAntigravityBrainMessages(
			filepath.Join(root, "brain", id),
		)...,
	)

	if ext == ".pb" && !hasTrajectory && hasAntigravityKey() {
		if extra, ok := decryptAntigravityCLITranscript(path); ok {
			messages = append(messages, extra)
		}
	}

	sort.SliceStable(messages, func(i, j int) bool {
		return messages[i].Timestamp.Before(messages[j].Timestamp)
	})
	for i := range messages {
		messages[i].Ordinal = i
	}

	if project == "" {
		project = inferAntigravityProject(
			filepath.Join(root, "history.jsonl"), id,
		)
	}

	var firstMessage string
	var userCount int
	var startedAt, endedAt time.Time
	for _, m := range messages {
		if m.Role == RoleUser {
			userCount++
			if firstMessage == "" && m.Content != "" {
				firstMessage = truncate(
					strings.ReplaceAll(m.Content, "\n", " "),
					300,
				)
			}
		}
		if !m.Timestamp.IsZero() {
			if startedAt.IsZero() || m.Timestamp.Before(startedAt) {
				startedAt = m.Timestamp
			}
			if m.Timestamp.After(endedAt) {
				endedAt = m.Timestamp
			}
		}
	}
	if startedAt.IsZero() {
		startedAt = info.ModTime()
	}
	if endedAt.IsZero() {
		endedAt = info.ModTime()
	}

	var size int64
	var mtime int64
	effInfo, statErr := AntigravityCLIFileInfo(path)
	if statErr == nil {
		size = effInfo.Size()
		mtime = effInfo.ModTime().UnixNano()
	} else {
		size = info.Size()
		mtime = info.ModTime().UnixNano()
	}

	sess := &ParsedSession{
		ID:               antigravityCLIIDPrefix + storageID,
		Project:          project,
		Machine:          machine,
		Agent:            AgentAntigravityCLI,
		FirstMessage:     firstMessage,
		StartedAt:        startedAt,
		EndedAt:          endedAt,
		MessageCount:     len(messages),
		UserMessageCount: userCount,
		File: FileInfo{
			Path:  path,
			Size:  size,
			Mtime: mtime,
		},
	}
	if len(messages) == 0 {
		return sess, nil, status, nil
	}
	return sess, messages, status, nil
}

func loadAntigravityCLIDBSteps(
	path string,
) (antigravityStepLoadResult, error) {
	dsn := "file:" + path + "?mode=ro&immutable=0"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return antigravityStepLoadResult{}, fmt.Errorf(
			"open antigravity cli db %s: %w", path, err,
		)
	}
	defer db.Close()
	return loadAntigravityStepsWithRawCount(db)
}

func mergeAntigravityDBHistoryMessages(
	dbMessages []ParsedMessage, history []ParsedMessage,
) []ParsedMessage {
	if len(dbMessages) == 0 || len(history) == 0 {
		return dbMessages
	}
	history = nonEmptyAntigravityHistoryMessages(history)
	if len(history) == 0 {
		return dbMessages
	}
	userCount := 0
	for _, msg := range dbMessages {
		if msg.Role == RoleUser {
			userCount++
		}
	}
	if userCount != len(history) {
		return appendMissingAntigravityHistoryMessages(
			dbMessages, history,
		)
	}
	historyIdx := 0
	for i := range dbMessages {
		if dbMessages[i].Role != RoleUser {
			continue
		}
		if historyIdx >= len(history) {
			return appendMissingAntigravityHistoryMessages(
				dbMessages, history,
			)
		}
		dbMessages[i].Content = history[historyIdx].Content
		dbMessages[i].ContentLength = len(history[historyIdx].Content)
		if !history[historyIdx].Timestamp.IsZero() {
			dbMessages[i].Timestamp = history[historyIdx].Timestamp
		}
		historyIdx++
	}
	return dbMessages
}

func appendMissingAntigravityHistoryMessages(
	dbMessages []ParsedMessage, history []ParsedMessage,
) []ParsedMessage {
	seen := make(map[string]struct{})
	for _, msg := range dbMessages {
		if msg.Role != RoleUser {
			continue
		}
		if key := normalizedAntigravityPrompt(msg.Content); key != "" {
			seen[key] = struct{}{}
		}
	}
	out := dbMessages
	for _, msg := range history {
		key := normalizedAntigravityPrompt(msg.Content)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		out = append(out, msg)
		seen[key] = struct{}{}
	}
	return out
}

func normalizedAntigravityPrompt(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func nonEmptyAntigravityHistoryMessages(
	history []ParsedMessage,
) []ParsedMessage {
	var out []ParsedMessage
	for _, msg := range history {
		if strings.TrimSpace(msg.Content) == "" {
			continue
		}
		out = append(out, msg)
	}
	return out
}

func hasDisplayableAntigravityCLITrajectoryMessage(
	msgs []ParsedMessage,
) bool {
	for _, m := range msgs {
		if strings.TrimSpace(m.Content) != "" ||
			m.HasThinking ||
			len(m.ToolCalls) > 0 {
			return true
		}
	}
	return false
}

// buildAntigravityProjectMap reads history.jsonl and returns a
// map of conversationId -> workspace path.
func buildAntigravityProjectMap(path string) map[string]string {
	out := make(map[string]string)
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		id := gjson.GetBytes(line, "conversationId").Str
		ws := gjson.GetBytes(line, "workspace").Str
		if id != "" && ws != "" {
			out[id] = ws
		}
	}
	return out
}

func inferAntigravityProject(path, id string) string {
	m := buildAntigravityProjectMap(path)
	return m[id]
}

// collectAntigravityHistoryMessages returns one user ParsedMessage
// per history.jsonl row matching the conversationId.
func collectAntigravityHistoryMessages(
	path, id string,
) []ParsedMessage {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []ParsedMessage
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		cid := gjson.GetBytes(line, "conversationId").Str
		if cid != id {
			continue
		}
		display := gjson.GetBytes(line, "display").Str
		if display == "" {
			continue
		}
		tsMS := gjson.GetBytes(line, "timestamp").Int()
		out = append(out, ParsedMessage{
			Role:          RoleUser,
			Content:       display,
			ContentLength: len(display),
			Timestamp:     time.UnixMilli(tsMS),
		})
	}
	return out
}

// collectAntigravityBrainMessages reads brain/<id>/*.md plus
// sibling .metadata.json files and emits one assistant message
// per artifact.
func collectAntigravityBrainMessages(dir string) []ParsedMessage {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []ParsedMessage
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		ts, summary, artifactType := readAntigravityArtifactMeta(
			filepath.Join(dir, name+".metadata.json"),
		)
		header := "[" + name + "]"
		if artifactType != "" {
			header = "[" + artifactType + " — " + name + "]"
		}
		var content string
		if summary != "" {
			content = header + "\n" + summary + "\n\n" +
				string(body)
		} else {
			content = header + "\n" + string(body)
		}
		out = append(out, ParsedMessage{
			Role:          RoleAssistant,
			Content:       content,
			ContentLength: len(content),
			Timestamp:     ts,
		})
	}
	return out
}

func readAntigravityArtifactMeta(
	path string,
) (ts time.Time, summary, artifactType string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, "", ""
	}
	summary = gjson.GetBytes(data, "summary").Str
	artifactType = gjson.GetBytes(data, "artifactType").Str
	if s := gjson.GetBytes(data, "updatedAt").Str; s != "" {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			ts = t
		}
	}
	return ts, summary, artifactType
}

// decryptAntigravityCLITranscript attempts to decrypt a .pb file
// and returns a single assistant ParsedMessage holding the raw
// strings extracted from the plaintext. Returns ok=false when
// decryption fails or yields no usable text.
func decryptAntigravityCLITranscript(
	path string,
) (ParsedMessage, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ParsedMessage{}, false
	}
	plain, err := decryptAntigravity(data)
	if err != nil || plain == nil {
		return ParsedMessage{}, false
	}
	fields, err := agProtoParse(plain)
	if err != nil || len(fields) == 0 {
		return ParsedMessage{}, false
	}
	strs := agProtoCollectStrings(fields, 40)
	if len(strs) == 0 {
		return ParsedMessage{}, false
	}
	body := strings.Join(strs, "\n\n---\n\n")
	header := "[Antigravity CLI: decrypted transcript preview]\n" +
		"(field schema unknown; strings extracted by wire walker)\n\n"
	content := header + body
	info, statErr := os.Stat(path)
	var ts time.Time
	if statErr == nil {
		ts = info.ModTime()
	}
	return ParsedMessage{
		Role:          RoleAssistant,
		Content:       content,
		ContentLength: len(content),
		Timestamp:     ts,
	}, true
}

// AntigravityCLIFileInfo returns a fake os.FileInfo whose size and mtime are
// computed by looking at both <uuid>.pb and <uuid>.trajectory.json.
// If the trajectory file exists, its mtime and size are factored in.
func AntigravityCLIFileInfo(path string) (os.FileInfo, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(path, ".db") {
		return antigravityCLICombinedFileInfo(
			info, path+"-wal", path+"-shm",
		), nil
	}
	size := info.Size()
	mtime := info.ModTime().UnixNano()

	sidecar := strings.TrimSuffix(path, ".pb") + ".trajectory.json"
	if sidecarInfo, err := os.Stat(sidecar); err == nil {
		size += sidecarInfo.Size()
		if sidecarInfo.ModTime().UnixNano() > mtime {
			mtime = sidecarInfo.ModTime().UnixNano()
		}
	}

	return fakeFileInfo{
		name:  info.Name(),
		size:  size,
		mtime: mtime,
	}, nil
}

func antigravityCLICombinedFileInfo(
	base os.FileInfo, companions ...string,
) os.FileInfo {
	size := base.Size()
	mtime := base.ModTime().UnixNano()
	for _, p := range companions {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		size += info.Size()
		if info.ModTime().UnixNano() > mtime {
			mtime = info.ModTime().UnixNano()
		}
	}
	return fakeFileInfo{
		name:  base.Name(),
		size:  size,
		mtime: mtime,
	}
}

type fakeFileInfo struct {
	name  string
	size  int64
	mtime int64
}

func (f fakeFileInfo) Name() string      { return f.name }
func (f fakeFileInfo) Size() int64       { return f.size }
func (f fakeFileInfo) Mode() os.FileMode { return 0 }
func (f fakeFileInfo) ModTime() time.Time {
	return time.Unix(0, f.mtime)
}
func (f fakeFileInfo) IsDir() bool { return false }
func (f fakeFileInfo) Sys() any    { return nil }

// The following agy* structs represent the decrypted trajectory schema generated by agy-reader.
// Note: These structs use the "agy" prefix to name them after the consumer tool (agy-reader),
// not the underlying Antigravity protocol formats.
//
// SCHEMA STABILITY & POLICY:
// The schema is daemon-defined and has historically drifted (e.g., CombinedOutput/ActionResult shapes).
// We parse this trajectory JSON on a best-effort basis. If the JSON contains unknown step types,
// they are silently ignored/skipped in the switch statements of the parser.
type agyTrajectory struct {
	TrajectoryID string    `json:"trajectoryId"`
	CascadeID    string    `json:"cascadeId"`
	Steps        []agyStep `json:"steps"`
}

type agyStep struct {
	Type     string          `json:"type"`
	Status   string          `json:"status"`
	Metadata agyStepMetadata `json:"metadata"`

	UserInput       *agyUserInput       `json:"userInput"`
	PlannerResponse *agyPlannerResponse `json:"plannerResponse"`
	RunCommand      *agyRunCommand      `json:"runCommand"`
	ViewFile        *agyViewFile        `json:"viewFile"`
	CodeAction      *agyCodeAction      `json:"codeAction"`
	GrepSearch      *agyGrepSearch      `json:"grepSearch"`
	ErrorMessage    *agyErrorMessage    `json:"errorMessage"`
	SystemMessage   *agySystemMessage   `json:"systemMessage"`
	Checkpoint      *agyCheckpoint      `json:"checkpoint"`
	ListDirectory   *agyListDirectory   `json:"listDirectory"`
}

type agyStepMetadata struct {
	CreatedAt   string `json:"createdAt"`
	ExecutionID string `json:"executionId"`
}

type agyUserInput struct {
	UserResponse string `json:"userResponse"`
}

type agyPlannerResponse struct {
	Thinking  string        `json:"thinking"`
	Response  string        `json:"response"`
	ToolCalls []agyToolCall `json:"toolCalls"`
}

type agyToolCall struct {
	Name          string `json:"name"`
	ArgumentsJSON string `json:"argumentsJson"`
	ID            string `json:"id"`
}

type agyRunCommand struct {
	CommandLine         string          `json:"commandLine"`
	ProposedCommandLine string          `json:"proposedCommandLine"`
	Cwd                 string          `json:"cwd"`
	ExitCode            *int            `json:"exitCode"`
	CombinedOutput      json.RawMessage `json:"combinedOutput"`
}

func (rc *agyRunCommand) CombinedOutputString() string {
	if rc == nil || len(rc.CombinedOutput) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(rc.CombinedOutput, &s); err == nil {
		return s
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(rc.CombinedOutput, &obj); err == nil {
		parts := make([]string, 0, 4)
		for _, key := range []string{"stdout", "stderr", "output", "text", "full"} {
			if raw, ok := obj[key]; ok {
				var v string
				if json.Unmarshal(raw, &v) == nil && v != "" {
					parts = append(parts, v)
				}
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	return string(rc.CombinedOutput)
}

type agyViewFile struct {
	AbsolutePathURI string `json:"absolutePathUri"`
	StartLine       int    `json:"startLine"`
	EndLine         int    `json:"endLine"`
	Content         string `json:"content"`
}

type agyCodeAction struct {
	Description  string          `json:"description"`
	ActionSpec   json.RawMessage `json:"actionSpec"`
	ActionResult json.RawMessage `json:"actionResult"`
}

type agyCodeActionResult struct {
	Edit *agyCodeActionEdit `json:"edit"`
}

type agyCodeActionEdit struct {
	Diff *agyCodeActionDiff `json:"diff"`
}

type agyCodeActionDiff struct {
	UnifiedDiff *agyCodeActionUD `json:"unifiedDiff"`
}

type agyCodeActionUD struct {
	Lines []agyCodeActionDiffLine `json:"lines"`
}

type agyCodeActionDiffLine struct {
	Text string `json:"text"`
	Type string `json:"type"`
}

func (ca *agyCodeAction) FormattedDiff() string {
	if ca == nil || len(ca.ActionResult) == 0 {
		return ""
	}
	var res agyCodeActionResult
	if err := json.Unmarshal(ca.ActionResult, &res); err != nil {
		return ""
	}
	if res.Edit == nil || res.Edit.Diff == nil || res.Edit.Diff.UnifiedDiff == nil {
		return ""
	}
	lines := res.Edit.Diff.UnifiedDiff.Lines
	if len(lines) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("```diff\n")
	for _, line := range lines {
		switch line.Type {
		case "UNIFIED_DIFF_LINE_TYPE_INSERT":
			sb.WriteString("+" + line.Text + "\n")
		case "UNIFIED_DIFF_LINE_TYPE_DELETE":
			sb.WriteString("-" + line.Text + "\n")
		default:
			sb.WriteString(" " + line.Text + "\n")
		}
	}
	sb.WriteString("```")
	return sb.String()
}

type agyGrepSearch struct {
	SearchPathURI string `json:"searchPathUri"`
	Query         string `json:"query"`
}

type agyErrorMessage struct {
	Error agyErrorMessageError `json:"error"`
}

type agyErrorMessageError struct {
	UserErrorMessage  string `json:"userErrorMessage"`
	ModelErrorMessage string `json:"modelErrorMessage"`
}

type agySystemMessage struct {
	Message string `json:"message"`
}

type agyCheckpoint struct {
	UserRequests   []string `json:"userRequests"`
	SessionSummary string   `json:"sessionSummary"`
}

type agyListDirectory struct {
	DirectoryPathURI string `json:"directoryPathUri"`
}

func parseTrajectoryTime(ts string) time.Time {
	if ts == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		return t
	}
	return time.Time{}
}

func agyToolDetail(name, inputJSON string) string {
	if !strings.HasPrefix(strings.TrimSpace(inputJSON), "{") {
		return name
	}
	var input map[string]json.RawMessage
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return name
	}
	getString := func(keys ...string) string {
		for _, k := range keys {
			v, ok := input[k]
			if !ok {
				continue
			}
			var s string
			if err := json.Unmarshal(v, &s); err == nil {
				if t := strings.TrimSpace(s); t != "" {
					return t
				}
			}
		}
		return ""
	}
	switch name {
	case "view_file":
		if p := getString("AbsolutePathURI", "AbsolutePath", "path"); p != "" {
			p = strings.TrimPrefix(p, "file://")
			return filepath.Base(p)
		}
	case "write_to_file", "replace_file_content", "multi_replace_file_content":
		if p := getString("TargetFile", "file_path", "path"); p != "" {
			return filepath.Base(p)
		}
	case "invoke_subagent", "define_subagent":
		if p := getString("name", "TypeName"); p != "" {
			return p
		}
	case "send_message":
		if p := getString("Recipient"); p != "" {
			return "to " + p
		}
	case "manage_task":
		if action := getString("Action"); action != "" {
			if id := getString("TaskId"); id != "" {
				return action + " " + id
			}
			return action
		}
	case "search_web":
		if q := getString("query"); q != "" {
			return q
		}
	case "read_url_content":
		if u := getString("Url"); u != "" {
			return u
		}
	case "generate_image":
		if n := getString("ImageName"); n != "" {
			return n
		}
	case "ask_question":
		if s := getString("toolSummary"); s != "" {
			return s
		}
	case "schedule":
		if p := getString("Prompt"); p != "" {
			return p
		}
	}
	return name
}

// parseAntigravityCLITrajectory reads a <uuid>.trajectory.json sidecar
// produced out-of-process by agy-reader and returns the decoded
// transcript as ParsedMessages.
//
// Trust posture (see SECURITY.md, "Imports and new readers" row of the
// Trust boundaries table): the sidecar is treated as untrusted
// structured input — same posture as any other agent session file.
// The read is size-capped (maxTrajectorySidecarBytes) and unknown step
// types are silently skipped further down. No content from the sidecar
// is executed or echoed back over any outbound channel.
func parseAntigravityCLITrajectory(
	trajectoryPath string,
) ([]ParsedMessage, error) {
	f, err := os.Open(trajectoryPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxTrajectorySidecarBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxTrajectorySidecarBytes {
		return nil, fmt.Errorf(
			"trajectory sidecar %s exceeds %d-byte cap",
			trajectoryPath, maxTrajectorySidecarBytes,
		)
	}
	var traj agyTrajectory
	if err := json.Unmarshal(data, &traj); err != nil {
		return nil, err
	}

	var msgs []ParsedMessage

	var pendingResults []ParsedToolResult
	var pendingResultsTime time.Time

	flushPendingResults := func() {
		if len(pendingResults) == 0 {
			return
		}
		// NOTE: We emit a synthetic User message with empty content containing these tool results.
		// This relies on the sync engine's internal contract (specifically pairToolResults and
		// pairAndFilter in engine.go) which matches tool results to tool calls by ToolUseID,
		// and then filters out empty-content synthetic User messages from final display.
		// Future maintainers: do not "clean up" this empty message behavior as it is critical
		// for correct UI rendering.
		msgs = append(msgs, ParsedMessage{
			Role:        RoleUser,
			Content:     "",
			Timestamp:   pendingResultsTime,
			ToolResults: pendingResults,
		})
		pendingResults = nil
	}

	for _, step := range traj.Steps {
		stepTime := parseTrajectoryTime(step.Metadata.CreatedAt)

		switch step.Type {
		case "CORTEX_STEP_TYPE_USER_INPUT":
			flushPendingResults()
			if step.UserInput == nil {
				continue
			}
			msgs = append(msgs, ParsedMessage{
				Role:          RoleUser,
				Content:       step.UserInput.UserResponse,
				ContentLength: len(step.UserInput.UserResponse),
				Timestamp:     stepTime,
			})

		case "CORTEX_STEP_TYPE_PLANNER_RESPONSE":
			flushPendingResults()
			if step.PlannerResponse == nil {
				continue
			}
			pr := step.PlannerResponse
			var toolCalls []ParsedToolCall
			var toolHeaders []string

			for _, tc := range pr.ToolCalls {
				cat := NormalizeToolCategory(tc.Name)
				detail := agyToolDetail(tc.Name, tc.ArgumentsJSON)
				header := formatToolHeader(cat, detail)
				toolHeaders = append(toolHeaders, header)

				toolCalls = append(toolCalls, ParsedToolCall{
					ToolUseID: tc.ID,
					ToolName:  tc.Name,
					Category:  cat,
					InputJSON: tc.ArgumentsJSON,
				})
			}

			content := pr.Response
			if content == "" && len(toolHeaders) > 0 {
				content = strings.Join(toolHeaders, "\n")
			}

			msg := ParsedMessage{
				Role:          RoleAssistant,
				Content:       content,
				ContentLength: len(content),
				Timestamp:     stepTime,
				ToolCalls:     toolCalls,
				HasToolUse:    len(toolCalls) > 0,
			}
			if pr.Thinking != "" {
				msg.ThinkingText = pr.Thinking
				msg.HasThinking = true
			}
			msgs = append(msgs, msg)

		case "CORTEX_STEP_TYPE_RUN_COMMAND",
			"CORTEX_STEP_TYPE_VIEW_FILE",
			"CORTEX_STEP_TYPE_CODE_ACTION",
			"CORTEX_STEP_TYPE_GREP_SEARCH",
			"CORTEX_STEP_TYPE_LIST_DIRECTORY",
			"CORTEX_STEP_TYPE_ERROR_MESSAGE":

			tuid := step.Metadata.ExecutionID
			if tuid == "" {
				continue
			}

			var resultText string
			switch step.Type {
			case "CORTEX_STEP_TYPE_RUN_COMMAND":
				if step.RunCommand != nil {
					resultText = step.RunCommand.CombinedOutputString()
				}
			case "CORTEX_STEP_TYPE_VIEW_FILE":
				if step.ViewFile != nil {
					resultText = step.ViewFile.Content
				}
			case "CORTEX_STEP_TYPE_CODE_ACTION":
				if step.CodeAction != nil {
					diff := step.CodeAction.FormattedDiff()
					if diff != "" {
						resultText = diff
					} else {
						var actRes string
						if len(step.CodeAction.ActionResult) > 0 {
							if err := json.Unmarshal(step.CodeAction.ActionResult, &actRes); err == nil {
								resultText = actRes
							} else {
								resultText = string(step.CodeAction.ActionResult)
							}
						}
					}
				}
			case "CORTEX_STEP_TYPE_GREP_SEARCH":
				if step.GrepSearch != nil {
					resultText = fmt.Sprintf("Search for query %q in path %s", step.GrepSearch.Query, step.GrepSearch.SearchPathURI)
				}
			case "CORTEX_STEP_TYPE_LIST_DIRECTORY":
				if step.ListDirectory != nil {
					resultText = fmt.Sprintf("List directory: %s", step.ListDirectory.DirectoryPathURI)
				}
			case "CORTEX_STEP_TYPE_ERROR_MESSAGE":
				if step.ErrorMessage != nil {
					em := step.ErrorMessage
					if em.Error.UserErrorMessage != "" {
						resultText = em.Error.UserErrorMessage
					}
					if em.Error.ModelErrorMessage != "" {
						if resultText != "" {
							resultText += "\n"
						}
						resultText += "Model Error: " + em.Error.ModelErrorMessage
					}
				}
			}

			resJSON, _ := json.Marshal(resultText)
			pendingResults = append(pendingResults, ParsedToolResult{
				ToolUseID:     tuid,
				ContentRaw:    string(resJSON),
				ContentLength: len(resultText),
			})
			if pendingResultsTime.IsZero() || stepTime.After(pendingResultsTime) {
				pendingResultsTime = stepTime
			}

		case "CORTEX_STEP_TYPE_SYSTEM_MESSAGE":
			flushPendingResults()
			if step.SystemMessage == nil {
				continue
			}
			msgs = append(msgs, ParsedMessage{
				Role:          RoleUser,
				IsSystem:      true,
				Content:       step.SystemMessage.Message,
				ContentLength: len(step.SystemMessage.Message),
				Timestamp:     stepTime,
			})

		case "CORTEX_STEP_TYPE_CHECKPOINT":
			flushPendingResults()
			if step.Checkpoint == nil {
				continue
			}
			cp := step.Checkpoint
			var parts []string
			if len(cp.UserRequests) > 0 {
				parts = append(parts, fmt.Sprintf("User Requests: %s", strings.Join(cp.UserRequests, ", ")))
			}
			if cp.SessionSummary != "" {
				parts = append(parts, cp.SessionSummary)
			}
			if len(parts) > 0 {
				content := "[Checkpoint]\n" + strings.Join(parts, "\n")
				msgs = append(msgs, ParsedMessage{
					Role:          RoleUser,
					IsSystem:      true,
					Content:       content,
					ContentLength: len(content),
					Timestamp:     stepTime,
				})
			}
		}
	}

	flushPendingResults()
	return msgs, nil
}
