package parser

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Antigravity IDE sessions live under ~/.gemini/antigravity/:
//
//   conversations/<uuid>.db        SQLite, one per session
//   annotations/<uuid>.pbtxt       last_user_view_time + flags
//   brain/<uuid>/*.md(+.json)      plaintext task/plan artifacts
//   implicit/<uuid>.pb             encrypted (handled like CLI)
//
// We treat the .db as the canonical session file (like Gemini's
// per-session JSON). Each row of `steps` becomes one ParsedMessage.

const antigravityIDPrefix = "antigravity:"

var antigravityUUIDLikeRE = regexp.MustCompile(
	`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`,
)

// DiscoverAntigravitySessions returns one DiscoveredFile per
// conversations/<uuid>.db under the IDE root.
func DiscoverAntigravitySessions(root string) []DiscoveredFile {
	if root == "" {
		return nil
	}
	dir := filepath.Join(root, "conversations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var files []DiscoveredFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".db") {
			continue
		}
		id := strings.TrimSuffix(name, ".db")
		if !IsValidSessionID(id) {
			continue
		}
		files = append(files, DiscoveredFile{
			Path:  filepath.Join(dir, name),
			Agent: AgentAntigravity,
		})
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return files
}

// FindAntigravitySourceFile locates a session DB by id.
func FindAntigravitySourceFile(root, id string) string {
	if root == "" || !IsValidSessionID(id) {
		return ""
	}
	p := filepath.Join(root, "conversations", id+".db")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

// ParseAntigravitySession parses one IDE session DB.
func ParseAntigravitySession(
	path, project, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("stat %s: %w", path, err)
	}
	id := strings.TrimSuffix(filepath.Base(path), ".db")
	if !IsValidSessionID(id) {
		return nil, nil, fmt.Errorf(
			"invalid Antigravity IDE session filename: %s", path,
		)
	}
	root := filepath.Dir(filepath.Dir(path))

	// Open read-only; SQLite session files have WAL/SHM
	// sidecars that the driver expects in the same dir.
	dsn := "file:" + path + "?mode=ro&immutable=0"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"open antigravity db %s: %w", path, err,
		)
	}
	defer db.Close()

	messages, err := loadAntigravitySteps(db)
	if err != nil {
		return nil, nil, err
	}
	messages = append(messages,
		collectAntigravityBrainMessages(
			filepath.Join(root, "brain", id),
		)...,
	)

	sort.SliceStable(messages, func(i, j int) bool {
		return messages[i].Timestamp.Before(messages[j].Timestamp)
	})
	for i := range messages {
		messages[i].Ordinal = i
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
	if ann := readAntigravityAnnotation(
		filepath.Join(root, "annotations", id+".pbtxt"),
	); !ann.IsZero() && ann.After(endedAt) {
		endedAt = ann
	}
	if startedAt.IsZero() {
		startedAt = info.ModTime()
	}
	if endedAt.IsZero() {
		endedAt = info.ModTime()
	}

	sess := &ParsedSession{
		ID:               antigravityIDPrefix + id,
		Project:          project,
		Machine:          machine,
		Agent:            AgentAntigravity,
		FirstMessage:     firstMessage,
		StartedAt:        startedAt,
		EndedAt:          endedAt,
		MessageCount:     len(messages),
		UserMessageCount: userCount,
		File: FileInfo{
			Path:  path,
			Size:  info.Size(),
			Mtime: info.ModTime().UnixNano(),
		},
	}
	if len(messages) == 0 {
		return sess, nil, nil
	}
	return sess, messages, nil
}

func loadAntigravitySteps(db *sql.DB) ([]ParsedMessage, error) {
	result, err := loadAntigravityStepsWithRawCount(db)
	if err != nil {
		return nil, err
	}
	return result.messages, nil
}

type antigravityStepLoadResult struct {
	messages     []ParsedMessage
	rawStepCount int
}

func loadAntigravityStepsWithRawCount(
	db *sql.DB,
) (antigravityStepLoadResult, error) {
	rows, err := db.Query(
		`SELECT idx, step_type, step_payload FROM steps ` +
			`ORDER BY idx`,
	)
	if err != nil {
		return antigravityStepLoadResult{}, fmt.Errorf("query steps: %w", err)
	}
	defer rows.Close()
	var result antigravityStepLoadResult
	for rows.Next() {
		var (
			idx      int
			stepType int
			payload  []byte
		)
		if err := rows.Scan(&idx, &stepType, &payload); err != nil {
			return antigravityStepLoadResult{}, fmt.Errorf("scan step: %w", err)
		}
		result.rawStepCount++
		msg, ok := decodeAntigravityStep(idx, stepType, payload)
		if !ok {
			continue
		}
		result.messages = append(result.messages, msg)
	}
	if err := rows.Err(); err != nil {
		return antigravityStepLoadResult{}, fmt.Errorf("iterate steps: %w", err)
	}
	return result, nil
}

// decodeAntigravityStep extracts a ParsedMessage from one step's
// protobuf payload. Without an upstream .proto we use heuristics:
//   - role: step_type 14 has been observed to carry user prompts.
//     Every other type is rendered as assistant. (TODO: refine
//     when more sample data is available.)
//   - content: best-effort human-facing strings found in the
//     payload tree. Internal ids, local Antigravity config paths,
//     model placeholders, and duplicate payload echoes are filtered
//     out. User-input steps prefer a single prompt-like string.
//   - timestamp: earliest google.protobuf.Timestamp-shaped field.
func decodeAntigravityStep(
	idx, stepType int, payload []byte,
) (ParsedMessage, bool) {
	if len(payload) == 0 {
		return ParsedMessage{}, false
	}
	fields, err := agProtoParse(payload)
	if err != nil || len(fields) == 0 {
		return ParsedMessage{}, false
	}
	strs := cleanAntigravityStepStrings(
		dedupeStrings(agProtoCollectStrings(fields, 20)), stepType,
	)
	ts := earliestAntigravityTimestamp(fields)
	if len(strs) == 0 {
		return ParsedMessage{}, false
	}
	role := RoleAssistant
	if stepType == 14 {
		role = RoleUser
	}
	content := strings.Join(strs, "\n\n")
	return ParsedMessage{
		Role:          role,
		Content:       content,
		ContentLength: len(content),
		Timestamp:     ts,
	}, true
}

func dedupeStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func cleanAntigravityStepStrings(
	strs []string, stepType int,
) []string {
	var cleaned []string
	for _, s := range strs {
		s = strings.TrimSpace(s)
		if isNoisyAntigravityStepString(s) {
			continue
		}
		cleaned = append(cleaned, s)
	}
	cleaned = dedupeStrings(cleaned)
	if stepType == 14 {
		if prompt := bestAntigravityUserPrompt(cleaned); prompt != "" {
			return []string{prompt}
		}
	}
	return cleaned
}

func isNoisyAntigravityStepString(s string) bool {
	if s == "" {
		return true
	}
	if antigravityUUIDLikeRE.MatchString(s) {
		return true
	}
	if strings.HasPrefix(s, "MODEL_PLACEHOLDER_") {
		return true
	}
	if strings.HasPrefix(s, "{") &&
		(strings.Contains(s, `"toolAction"`) ||
			strings.Contains(s, `"toolSummary"`) ||
			strings.Contains(s, `"DirectoryPath"`)) {
		return true
	}
	if looksLikeAntigravityOpaqueID(s) {
		return true
	}
	if strings.HasPrefix(s, "file:///home/") {
		return true
	}
	if strings.HasPrefix(s, "/home/") &&
		strings.Contains(s, "/.gemini/") {
		return true
	}
	if strings.HasPrefix(s, "/Users/") &&
		strings.Contains(s, "/.gemini/") {
		return true
	}
	if strings.HasPrefix(s, `C:\Users\`) &&
		strings.Contains(s, `\.gemini\`) {
		return true
	}
	if strings.HasPrefix(s, "command(") ||
		strings.HasPrefix(s, "execute_url(") ||
		strings.HasPrefix(s, "read_url(") ||
		strings.HasPrefix(s, "mcp(") {
		return true
	}
	return false
}

func looksLikeAntigravityOpaqueID(s string) bool {
	if strings.ContainsAny(s, " \n\t") {
		return false
	}
	if len(s) < 16 || len(s) > 128 {
		return false
	}
	var alpha, digit, symbol int
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			alpha++
		case r >= '0' && r <= '9':
			digit++
		case r == '_' || r == '-' || r == '.':
			symbol++
		default:
			return false
		}
	}
	if alpha+digit+symbol != len(s) {
		return false
	}
	if digit == len(s) || digit+symbol == len(s) {
		return true
	}
	return alpha > 0 && digit > 0
}

func bestAntigravityUserPrompt(strs []string) string {
	var best string
	bestScore := -1
	for _, s := range strs {
		score := antigravityPromptScore(s)
		if score > bestScore {
			best = s
			bestScore = score
		}
	}
	if bestScore <= 0 {
		return ""
	}
	return best
}

func antigravityPromptScore(s string) int {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" || isNoisyAntigravityStepString(trimmed) {
		return -1
	}
	score := len(trimmed)
	if strings.ContainsAny(trimmed, " \n\t") {
		score += 50
	}
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		score -= 100
	}
	if strings.HasPrefix(trimmed, "/") || strings.HasPrefix(trimmed, "file://") {
		score -= 100
	}
	if !strings.ContainsAny(trimmed, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ") {
		score -= 100
	}
	return score
}

// earliestAntigravityTimestamp walks the field tree and returns
// the earliest plausible google.protobuf.Timestamp value.
// Plausible = seconds field in the year 2000..2100 range.
func earliestAntigravityTimestamp(
	fields []agProtoField,
) time.Time {
	var best time.Time
	var walk func([]agProtoField)
	walk = func(fs []agProtoField) {
		for _, f := range fs {
			if f.Nested != nil {
				if sec, nanos, ok := agProtoTimestamp(f.Nested); ok {
					if sec > 946_684_800 && sec < 4_102_444_800 {
						t := time.Unix(sec, int64(nanos))
						if best.IsZero() || t.Before(best) {
							best = t
						}
					}
				}
				walk(f.Nested)
			}
		}
	}
	walk(fields)
	return best
}

// readAntigravityAnnotation parses last_user_view_time from a
// pbtxt annotation file. Returns zero time on any failure.
func readAntigravityAnnotation(path string) time.Time {
	data, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}
	}
	// last_user_view_time:{seconds:1779326586 nanos:959000000}
	i := strings.Index(string(data), "last_user_view_time")
	if i < 0 {
		return time.Time{}
	}
	rest := string(data[i:])
	j := strings.Index(rest, "seconds:")
	if j < 0 {
		return time.Time{}
	}
	rest = rest[j+len("seconds:"):]
	end := strings.IndexAny(rest, " \n\t}")
	if end < 0 {
		return time.Time{}
	}
	var sec int64
	if _, err := fmt.Sscanf(rest[:end], "%d", &sec); err != nil {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}
