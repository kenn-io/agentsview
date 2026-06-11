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
	"unicode"

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
	block, okUsage := extractTokenUsage(data)
	if okUsage {
		// gen_metadata field semantics (cross-validated against sidecar
		// generatorMetadata ground truth in 550/550 blocks):
		//   f2 = uncached input (inputTokens)
		//   f3 = total output including thinking (outputTokens)
		//   f5 = cache-read (cacheReadTokens, absent when no cache hits)
		//   f4 = always 0/absent, ignored
		// No per-field reasoning breakdown is available; f3 already
		// includes thinking tokens.
		context := block.UncachedInput + block.CacheRead
		eventModel := genModel
		var occurredAt string
		if decoded {
			if eventModel == "" {
				eventModel = msg.Model
			}
			if !msg.Timestamp.IsZero() {
				occurredAt = msg.Timestamp.Format(time.RFC3339Nano)
			}
			msg.ContextTokens = context
			msg.OutputTokens = block.TotalOutput
			msg.HasContextTokens = context > 0
			msg.HasOutputTokens = block.TotalOutput > 0

		}
		r.usageEvents = append(r.usageEvents, ParsedUsageEvent{
			Source:               "generation",
			Model:                eventModel,
			InputTokens:          block.UncachedInput,
			OutputTokens:         block.TotalOutput,
			CacheReadInputTokens: block.CacheRead,
			ReasoningTokens:      0, // not available in gen_metadata
			OccurredAt:           occurredAt,
		})
	}
	if decoded && genModel != "" {
		msg.Model = genModel
	}
	return msg
}

// agTokenBlock carries the decoded token usage extracted from one
// gen_metadata blob. Field semantics are cross-validated against sidecar
// ground truth (generatorMetadata[].chatModel.usage matches in 550/550
// blocks):
//
//	UncachedInput = f2 (inputTokens, tokens not served from cache)
//	TotalOutput   = f3 (outputTokens, includes thinking)
//	CacheRead     = f5 (cacheReadTokens, absent/zero for cache-miss sessions)
//
// No per-field reasoning breakdown is available in gen_metadata;
// TotalOutput already includes thinking tokens.
type agTokenBlock struct {
	UncachedInput int // f2: tokens not served from cache
	TotalOutput   int // f3: total output including thinking
	CacheRead     int // f5: cache-read tokens (0 when absent)
}

// maxPlausibleTokens caps the token values accepted by the heuristic.
// Other nested messages can coincidentally satisfy field1 ∈ [1000, 5000)
// while carrying large integers (e.g. a nanosecond latency).
// No real LLM generation involves more than a few million tokens,
// so blocks with values above this threshold are treated as false
// positives and skipped.
const maxPlausibleTokens = 2_000_000

func extractTokenUsage(data []byte) (agTokenBlock, bool) {
	fields, err := agProtoParse(data)
	if err != nil {
		return agTokenBlock{}, false
	}
	var found bool
	var block agTokenBlock
	var walk func([]agProtoField)
	walk = func(fs []agProtoField) {
		if found {
			return
		}
		if b, ok := tokenBlockFrom(fs); ok {
			block = b
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
	return block, found
}

// tokenBlockFrom reports whether fs is a plausible token usage block.
//
// Field semantics are cross-validated against sidecar ground truth
// (generatorMetadata[].chatModel.usage matches in 550/550 blocks):
//
//	f1 = model-kind varint in [1000, 5000)
//	f2 = uncached input (inputTokens)
//	f3 = total output including thinking (outputTokens)
//	f4 = always 0/absent, ignored
//	f5 = cache-read (cacheReadTokens, absent when no cache hits)
//
// No per-field reasoning breakdown is available in gen_metadata;
// the reasoning return value is always 0.
//
// f2 and f3 are required. f5 is optional: proto3 omits zero-valued
// fields, and a fresh session with no cache hits omits f5 entirely.
// Requiring f5 (the previous heuristic) caused the parser to miss
// token blocks in such sessions, which is why the single-generation
// June-11 archives all have no extracted block under the old mapping.
func tokenBlockFrom(fs []agProtoField) (agTokenBlock, bool) {
	f1, ok1 := agProtoFind(fs, 1)
	f2, ok2 := agProtoFind(fs, 2)
	f3, ok3 := agProtoFind(fs, 3)
	// f5 (cache-read) is optional: proto3 omits zero-valued fields, and
	// cache-read is absent when a session has no cache hits.
	f5, hasF5 := agProtoFind(fs, 5)

	if !ok1 || !ok2 || !ok3 ||
		f1.Wire != pbWireVarint || f2.Wire != pbWireVarint ||
		f3.Wire != pbWireVarint {
		return agTokenBlock{}, false
	}
	if f1.Varint < 1000 || f1.Varint >= 5000 {
		return agTokenBlock{}, false
	}
	if f2.Varint > maxPlausibleTokens || f3.Varint > maxPlausibleTokens {
		return agTokenBlock{}, false
	}
	// f2 (input) and f3 (output) are independent quantities, but an
	// implausibly large combined footprint (input + output > cap)
	// signals a decoy block where both values individually pass the
	// per-field cap but are collectively implausible for a single
	// generation.
	if f2.Varint+f3.Varint > maxPlausibleTokens {
		return agTokenBlock{}, false
	}
	// f4 is consistently absent/zero in real blocks and carries no
	// semantics. Tolerate its presence but ignore the value.
	if f4, hasF4 := agProtoFind(fs, 4); hasF4 {
		if f4.Wire != pbWireVarint || f4.Varint > maxPlausibleTokens {
			return agTokenBlock{}, false
		}
	}
	if hasF5 {
		if f5.Wire != pbWireVarint || f5.Varint > maxPlausibleTokens {
			return agTokenBlock{}, false
		}
	}

	block := agTokenBlock{
		UncachedInput: int(f2.Varint),
		TotalOutput:   int(f3.Varint),
	}
	if hasF5 {
		block.CacheRead = int(f5.Varint)
	}
	return block, true
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
			if s, ok := agProtoString(f21); ok &&
				isPlausibleModelName(s) {
				model = s
				return
			}
		}
		if f19, ok := agProtoFind(fs, 19); ok {
			if s, ok := agProtoString(f19); ok &&
				isPlausibleModelName(s) {
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

// isPlausibleModelName reports whether s looks like a human-readable
// model identifier. Field 21/19 sometimes carries a nested protobuf
// message whose low bytes (tags, varints, NULs) are valid UTF-8 --
// agProtoString cannot tell those apart from text, and the raw bytes
// previously leaked into messages.model (and broke `pg push`, which
// rejects NUL bytes). Require every rune to be printable and at least
// one letter to be present.
func isPlausibleModelName(s string) bool {
	if s == "" {
		return false
	}
	hasLetter := false
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
		if unicode.IsLetter(r) {
			hasLetter = true
		}
	}
	return hasLetter
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
//   - tool calls: assistant steps whose payloads contain known tool
//     name strings emit structured ParsedToolCall entries so that
//     the timing panel can compute turns, categories, and counts.
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
	ts := earliestAntigravityTimestamp(fields)

	role := RoleAssistant
	if stepType == 14 {
		role = RoleUser
	}

	// Extract tool calls for assistant steps before the content guard
	// so that tool-only steps (no displayable text) are not silently
	// dropped.
	var calls []ParsedToolCall
	if role == RoleAssistant {
		calls = extractAntigravityToolCalls(idx, fields)
	}

	strs := cleanAntigravityStepStrings(
		dedupeStrings(agProtoCollectStrings(fields, 20)), stepType,
	)

	// Emit the message if it has displayable content OR tool calls.
	// Tool-only assistant steps (empty prose) are valid.
	if len(strs) == 0 && len(calls) == 0 {
		return ParsedMessage{}, false
	}

	content := strings.Join(strs, "\n\n")
	msg := ParsedMessage{
		Role:          role,
		Content:       content,
		ContentLength: len(content),
		Timestamp:     ts,
	}
	if len(calls) > 0 {
		msg.ToolCalls = calls
		msg.HasToolUse = true
	}
	return msg, true
}

// isLikelyToolName returns true if s is a reasonable candidate for a tool name:
// - length between 1 and 64 characters
// - contains only ASCII letters, digits, underscores, hyphens, and colons.
func isLikelyToolName(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		r := s[i]
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' && r != ':' {
			return false
		}
	}
	return true
}

// knownAntigravityToolNames is the set of tool names that Antigravity
// actually uses. Only strings present in this set are accepted as tool
// calls; generic taxonomy matches without a known Antigravity name
// are rejected. This prevents generic strings like "read", "write",
// "message", or "process" from being falsely matched.
var knownAntigravityToolNames = map[string]bool{
	// Antigravity-specific tools
	"view_file":                  true,
	"read_url_content":           true,
	"replace_file_content":       true,
	"multi_replace_file_content": true,
	"write_to_file":              true,
	"define_subagent":            true,
	"invoke_subagent":            true,
	"manage_subagents":           true,
	"send_message":               true,
	"manage_task":                true,
	"ask_permission":             true,
	"ask_question":               true,
	"schedule":                   true,
	"search_web":                 true,
	"generate_image":             true,
	// Gemini/Antigravity shared tools (also appear in CLI variant)
	"run_command":       true,
	"execute_command":   true,
	"run_shell_command": true,
	"grep_search":       true,
	"search_files":      true,
	"list_directory":    true,
	// Known CLI JSON structure tool names
	"edit_file":  true,
	"read_file":  true,
	"write_file": true,
}

// isAntigravityToolName reports whether s is a known Antigravity tool
// name. Only strings present in knownAntigravityToolNames are accepted;
// generic taxonomy matches are rejected.
func isAntigravityToolName(s string) bool {
	return knownAntigravityToolNames[s]
}

// extractAntigravityToolCalls walks the decoded protobuf field tree
// and returns one ParsedToolCall per tool invocation found. Uses the
// same heuristic-walker approach as extractTokenUsage / extractModelName:
// we identify strings that exactly match known tool names, collect any
// adjacent UUID-like string as the ToolUseID, and any adjacent JSON
// object string as the InputJSON.
//
// When no UUID-like ID is found, a synthetic deterministic ID is
// generated so the timing pipeline still has a stable key per call.
//
// Only strings matching Antigravity-known tool names are accepted.
func extractAntigravityToolCalls(
	stepIdx int, fields []agProtoField,
) []ParsedToolCall {
	// Collect all string values reachable from this step's field tree.
	// minLen=1 so we catch even short tool names like "Bash" or "Read".
	all := agProtoCollectStrings(fields, 1)

	var calls []ParsedToolCall
	seen := map[string]bool{}
	for i, s := range all {
		if !isLikelyToolName(s) {
			continue
		}
		cat := NormalizeToolCategory(s)
		if cat == "Other" {
			continue
		}
		// Reject generic taxonomy matches that are not known Antigravity tools.
		if !isAntigravityToolName(s) {
			continue
		}
		// Look for an adjacent UUID-like string to use as ToolUseID.
		// We scan the neighbouring strings (within a small window on
		// either side) since the proto walker returns siblings in
		// encounter order. Prefer following siblings so a flat sequence
		// of tools doesn't mistakenly pick up previous IDs.
		toolUseID := ""
		for _, offset := range []int{1, 2, -1, -2} {
			j := i + offset
			if j < 0 || j >= len(all) {
				continue
			}
			// Check for intervening tool names to avoid stealing UUID of another tool call
			interveningTool := false
			if offset > 0 {
				for k := i + 1; k < j; k++ {
					if isAntigravityToolName(all[k]) {
						interveningTool = true
						break
					}
				}
			} else {
				for k := j + 1; k < i; k++ {
					if isAntigravityToolName(all[k]) {
						interveningTool = true
						break
					}
				}
			}
			if interveningTool {
				continue
			}

			if antigravityUUIDLikeRE.MatchString(all[j]) {
				toolUseID = all[j]
				break
			}
		}

		// Look for an adjacent JSON-object string to use as InputJSON.
		inputJSON := ""
		for _, offset := range []int{1, 2, -1} {
			j := i + offset
			if j < 0 || j >= len(all) {
				continue
			}
			// Check for intervening tool names to avoid stealing InputJSON of another tool call
			interveningTool := false
			if offset > 0 {
				for k := i + 1; k < j; k++ {
					if isAntigravityToolName(all[k]) {
						interveningTool = true
						break
					}
				}
			} else {
				for k := j + 1; k < i; k++ {
					if isAntigravityToolName(all[k]) {
						interveningTool = true
						break
					}
				}
			}
			if interveningTool {
				continue
			}

			if strings.HasPrefix(strings.TrimSpace(all[j]), "{") {
				inputJSON = all[j]
				break
			}
		}

		// Assign a synthetic ID when no UUID was found in the payload,
		// using the string index to make each invocation unique.
		if toolUseID == "" {
			toolUseID = fmt.Sprintf("ag-step-%d-%d", stepIdx, i)
		}

		// Avoid emitting duplicate tool hits from the same payload
		// (the walker may surface the same string via multiple paths).
		// We deduplicate by tool name + ID + Input JSON to avoid collapsing
		// multiple distinct invocations of the same tool in one step.
		// This runs after synthetic-ID assignment so that calls without
		// adjacent UUIDs still get position-unique keys.
		dedupKey := s + ":" + toolUseID + ":" + inputJSON
		if seen[dedupKey] {
			continue
		}
		seen[dedupKey] = true

		calls = append(calls, ParsedToolCall{
			ToolUseID: toolUseID,
			ToolName:  s,
			Category:  cat,
			InputJSON: inputJSON,
		})
	}
	return calls
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
