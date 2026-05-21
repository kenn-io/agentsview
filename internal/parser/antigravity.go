package parser

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
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
	rows, err := db.Query(
		`SELECT idx, step_type, step_payload FROM steps ` +
			`ORDER BY idx`,
	)
	if err != nil {
		return nil, fmt.Errorf("query steps: %w", err)
	}
	defer rows.Close()
	var out []ParsedMessage
	for rows.Next() {
		var (
			idx      int
			stepType int
			payload  []byte
		)
		if err := rows.Scan(&idx, &stepType, &payload); err != nil {
			return nil, fmt.Errorf("scan step: %w", err)
		}
		msg, ok := decodeAntigravityStep(idx, stepType, payload)
		if !ok {
			continue
		}
		out = append(out, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate steps: %w", err)
	}
	return out, nil
}

// decodeAntigravityStep extracts a ParsedMessage from one step's
// protobuf payload. Without an upstream .proto we use heuristics:
//   - role: step_type 14 has been observed to carry user prompts.
//     Every other type is rendered as assistant. (TODO: refine
//     when more sample data is available.)
//   - content: concatenation of every UTF-8 string >= 20 chars
//     found anywhere in the payload tree, deduped.
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
	strs := dedupeStrings(agProtoCollectStrings(fields, 20))
	ts := earliestAntigravityTimestamp(fields)
	if len(strs) == 0 {
		return ParsedMessage{}, false
	}
	role := RoleAssistant
	if stepType == 14 {
		role = RoleUser
	}
	header := fmt.Sprintf(
		"[step %d · type %d]", idx, stepType,
	)
	content := header + "\n" + strings.Join(strs, "\n\n")
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
