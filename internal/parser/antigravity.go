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

// AntigravityFileInfo returns the effective file info for an IDE
// session .db, combining the main file with its -wal/-shm sidecars,
// the annotations/<id>.pbtxt sidecar, and the brain/<id> artifacts
// the parse renders as messages. WAL-only commits and annotation or
// brain updates do not touch the main file, so skip checks and
// persisted file metadata must use this composite or live sessions
// never reparse.
func AntigravityFileInfo(path string) (os.FileInfo, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	id := strings.TrimSuffix(filepath.Base(path), ".db")
	root := filepath.Dir(filepath.Dir(path))
	companions := []string{
		path + "-wal",
		path + "-shm",
		filepath.Join(root, "annotations", id+".pbtxt"),
	}
	companions = append(companions, antigravityBrainCompanions(
		filepath.Join(root, "brain", id),
	)...)
	return antigravityCLICombinedFileInfo(info, companions...), nil
}

// ParseAntigravitySession parses one IDE session DB.
func ParseAntigravitySession(
	path, project, machine string,
) (*ParsedSession, []ParsedMessage, []ParsedUsageEvent, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("stat %s: %w", path, err)
	}
	id := strings.TrimSuffix(filepath.Base(path), ".db")
	if !IsValidSessionID(id) {
		return nil, nil, nil, fmt.Errorf(
			"invalid Antigravity IDE session filename: %s", path,
		)
	}
	root := filepath.Dir(filepath.Dir(path))

	// Open read-only; SQLite session files have WAL/SHM
	// sidecars that the driver expects in the same dir.
	dsn := "file:" + path + "?mode=ro&immutable=0"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, nil, nil, fmt.Errorf(
			"open antigravity db %s: %w", path, err,
		)
	}
	defer db.Close()

	messages, usageEvents, err := loadAntigravitySteps(db)
	if err != nil {
		return nil, nil, nil, err
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

	var size int64
	var mtime int64
	if effInfo, statErr := AntigravityFileInfo(path); statErr == nil {
		size = effInfo.Size()
		mtime = effInfo.ModTime().UnixNano()
	} else {
		size = info.Size()
		mtime = info.ModTime().UnixNano()
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
			Size:  size,
			Mtime: mtime,
		},
	}
	accumulateMessageTokenUsage(sess, messages)
	applyUsageEventTokenTotals(sess, usageEvents)
	for i := range usageEvents {
		usageEvents[i].SessionID = sess.ID
	}
	if len(messages) == 0 {
		// Usage events still flow for message-less parses (e.g. an
		// undecodable DB with gen_metadata) so daily usage analytics
		// match the event-derived session totals stamped above.
		return sess, nil, usageEvents, nil
	}
	return sess, messages, usageEvents, nil
}

func loadAntigravitySteps(db *sql.DB) ([]ParsedMessage, []ParsedUsageEvent, error) {
	result, err := loadAntigravityStepsWithRawCount(db)
	if err != nil {
		return nil, nil, err
	}
	return result.messages, result.usageEvents, nil
}

type antigravityStepLoadResult struct {
	messages     []ParsedMessage
	usageEvents  []ParsedUsageEvent
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

	// Gracefully query gen_metadata if the table exists
	var genMeta map[int][]byte
	if genRows, err := db.Query("SELECT idx, data FROM gen_metadata"); err == nil {
		defer genRows.Close()
		genMeta = make(map[int][]byte)
		for genRows.Next() {
			var idx int
			var data []byte
			if err := genRows.Scan(&idx, &data); err == nil {
				genMeta[idx] = data
			}
		}
	}

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
		msg, decoded := decodeAntigravityStep(idx, stepType, payload)
		if data, ok := genMeta[idx]; ok {
			msg = result.appendGenMetadataUsage(data, msg, decoded)
		}
		if !decoded {
			continue
		}
		result.messages = append(result.messages, msg)
	}
	if err := rows.Err(); err != nil {
		return antigravityStepLoadResult{}, fmt.Errorf("iterate steps: %w", err)
	}
	return result, nil
}

// appendGenMetadataUsage records a usage event from one gen_metadata
// payload and, when the step decoded into a message, attaches token
// counts and the model name to the returned copy. Usage extraction is
// deliberately independent of message decoding: a step the heuristic
// cannot render can still be rescued by the CLI trajectory sidecar
// transcript, and its usage must not be dropped.
func (r *antigravityStepLoadResult) appendGenMetadataUsage(
	data []byte, msg ParsedMessage, decoded bool,
) ParsedMessage {
	genModel := extractModelName(data)
	input, output, reasoning, okUsage := extractTokenUsage(data)
	if okUsage {
		// gen_metadata splits candidates (field 2) and thoughts
		// (field 3) Gemini-style, but cost paths price OutputTokens
		// only. Fold reasoning into the billable output — matching
		// the Gemini parser — and keep ReasoningTokens as a
		// breakdown.
		billableOutput := output + reasoning
		eventModel := genModel
		var occurredAt string
		if decoded {
			if eventModel == "" {
				eventModel = msg.Model
			}
			if !msg.Timestamp.IsZero() {
				occurredAt = msg.Timestamp.Format(time.RFC3339Nano)
			}
			msg.ContextTokens = input
			msg.OutputTokens = billableOutput
			msg.HasContextTokens = input > 0
			msg.HasOutputTokens = billableOutput > 0
		}
		r.usageEvents = append(r.usageEvents, ParsedUsageEvent{
			Source:          "generation",
			Model:           eventModel,
			InputTokens:     input,
			OutputTokens:    billableOutput,
			ReasoningTokens: reasoning,
			OccurredAt:      occurredAt,
		})
	}
	if decoded && genModel != "" {
		msg.Model = genModel
	}
	return msg
}

// extractTokenUsage walks the decoded protobuf fields recursively to find
// the token usage block containing Field 1 = 1020, Field 2 = output,
// Field 3 = reasoning, and Field 5 = input.
//
// maxPlausibleTokens caps the token values accepted by the heuristic.
// Other nested messages can coincidentally satisfy field1 ∈ [1000, 5000)
// while carrying large integers (e.g. a nanosecond latency).
// No real LLM generation involves more than a few million tokens,
// so blocks with values above this threshold are treated as false
// positives and skipped.
const maxPlausibleTokens = 2_000_000

func extractTokenUsage(data []byte) (input, output, reasoning int, ok bool) {
	fields, err := agProtoParse(data)
	if err != nil {
		return 0, 0, 0, false
	}
	var found bool
	var walk func([]agProtoField)
	walk = func(fs []agProtoField) {
		if found {
			return
		}
		if in, out, reas, blockOK := tokenBlockFrom(fs); blockOK {
			input, output, reasoning = in, out, reas
			found = true
			return
		}
		for _, f := range fs {
			if f.Nested != nil {
				walk(f.Nested)
			}
		}
	}
	walk(fields)
	return input, output, reasoning, found
}

// tokenBlockFrom reports whether fs is a plausible token usage block:
// field 1 holds a model-kind varint in [1000, 5000), fields 2 (output)
// and 5 (input) are varints within maxPlausibleTokens, and field 3
// (reasoning), when present, is a varint within the cap too. Field 5
// is required: a real generation always consumes input context (proto3
// omits only zero values), while observed false-positive blocks (e.g.
// latency counters) lack it. Field 3 stays optional because zero
// reasoning is legitimate and omitted from the wire, but a present
// field 3 with a non-varint wire type marks the block as a decoy.
func tokenBlockFrom(fs []agProtoField) (input, output, reasoning int, ok bool) {
	f1, ok1 := agProtoFind(fs, 1)
	f2, ok2 := agProtoFind(fs, 2)
	f5, ok5 := agProtoFind(fs, 5)
	if !ok1 || !ok2 || !ok5 ||
		f1.Wire != pbWireVarint || f2.Wire != pbWireVarint ||
		f5.Wire != pbWireVarint {
		return 0, 0, 0, false
	}
	if f1.Varint < 1000 || f1.Varint >= 5000 {
		return 0, 0, 0, false
	}
	if f2.Varint > maxPlausibleTokens || f5.Varint > maxPlausibleTokens {
		return 0, 0, 0, false
	}
	if f3, hasF3 := agProtoFind(fs, 3); hasF3 {
		if f3.Wire != pbWireVarint || f3.Varint > maxPlausibleTokens {
			return 0, 0, 0, false
		}
		reasoning = int(f3.Varint)
	}
	return int(f5.Varint), int(f2.Varint), reasoning, true
}

// extractModelName recursively walks fields to extract the model name from Field 21 or Field 19.
func extractModelName(data []byte) string {
	fields, err := agProtoParse(data)
	if err != nil {
		return ""
	}
	var model string
	var walk func([]agProtoField)
	walk = func(fs []agProtoField) {
		if model != "" {
			return
		}
		if f21, ok := agProtoFind(fs, 21); ok {
			if s, ok := agProtoString(f21); ok && s != "" {
				model = s
				return
			}
		}
		if f19, ok := agProtoFind(fs, 19); ok {
			if s, ok := agProtoString(f19); ok && s != "" {
				model = s
				return
			}
		}
		for _, f := range fs {
			if f.Nested != nil {
				walk(f.Nested)
			}
		}
	}
	walk(fields)
	return model
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
