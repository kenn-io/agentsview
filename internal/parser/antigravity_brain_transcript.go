package parser

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// Antigravity's agent brain writes a plaintext transcript of a conversation to
//
//	brain/<uuid>/.system_generated/logs/transcript.jsonl
//
// one JSON object per line, one line per step:
//
//	step_index  number  step order, with gaps where a step logged nothing
//	source      string  USER_EXPLICIT, MODEL, or SYSTEM
//	type        string  USER_INPUT, PLANNER_RESPONSE, VIEW_FILE, ...
//	status      string  DONE, ...
//	created_at  string  RFC3339
//	content     string  the step's text; absent on some steps
//	thinking    string  the model's reasoning, when it logged any
//	tool_calls  array   [{"name": ..., "args": {...}}]
//
// This matters because a conversation's own stream, conversations/<uuid>.pb,
// is AES-encrypted: without the key it holds nothing this parser can read,
// while the brain transcript beside it is plaintext and carries the user's
// turns, the model's turns, its reasoning and its tool calls. So the
// transcript is a session source of its own whenever there is no
// conversations/<uuid>.db for the same conversation, and a companion of that
// database's session when there is one.
//
// Trust posture (SECURITY.md, "Imports and new readers"): the transcript is
// untrusted structured input. The read is size-capped, unknown sources and
// malformed lines are skipped, and nothing read here is executed or echoed to
// any outbound channel.

const (
	// antigravityBrainTranscriptName is the transcript's file name; the two
	// directories above it are fixed by the brain's own layout.
	antigravityBrainTranscriptName = "transcript.jsonl"
	antigravityBrainGeneratedDir   = ".system_generated"
	antigravityBrainLogsDir        = "logs"

	// antigravityBrainTag marks a session whose source is a brain
	// transcript rather than a conversations/<uuid>.db, in the same spirit as
	// antigravityImplicitTag on the CLI side.
	antigravityBrainTag = "brain-"

	// antigravityBrainRootTagLen is how much of the root's digest goes into a
	// brain session's storage ID. The digest is what keeps two configured
	// roots holding the same conversation from collapsing onto one session
	// (see antigravityBrainRootTag).
	antigravityBrainRootTagLen = 8

	// maxAntigravityBrainTranscriptBytes caps the transcript read, matching
	// the cap on the trajectory sidecar next door.
	maxAntigravityBrainTranscriptBytes = maxTrajectorySidecarBytes
)

// antigravityBrainTranscriptPath returns where conversation id's brain
// transcript lives under root.
func antigravityBrainTranscriptPath(root, id string) string {
	return filepath.Join(
		root, "brain", id,
		antigravityBrainGeneratedDir,
		antigravityBrainLogsDir,
		antigravityBrainTranscriptName,
	)
}

// antigravityBrainTranscriptConversation reports whether path is a brain
// transcript and, if so, returns the root it sits under and the conversation
// id it belongs to. The shape is checked segment by segment so a file that
// merely shares the name is not mistaken for one.
func antigravityBrainTranscriptConversation(path string) (string, string, bool) {
	clean := filepath.Clean(path)
	if filepath.Base(clean) != antigravityBrainTranscriptName {
		return "", "", false
	}
	logs := filepath.Dir(clean)
	if filepath.Base(logs) != antigravityBrainLogsDir {
		return "", "", false
	}
	generated := filepath.Dir(logs)
	if filepath.Base(generated) != antigravityBrainGeneratedDir {
		return "", "", false
	}
	conversation := filepath.Dir(generated)
	id := filepath.Base(conversation)
	if !IsValidSessionID(id) {
		return "", "", false
	}
	brain := filepath.Dir(conversation)
	if filepath.Base(brain) != "brain" {
		return "", "", false
	}
	return filepath.Dir(brain), id, true
}

// antigravityBrainRootTag returns the root-scoped part of a brain session's
// storage ID. It is derived from the root path alone, so it does not depend on
// which root the walk reaches first, on how many roots are configured, or on
// the order configuration lists them: the same root always produces the same
// tag, and two different roots holding one conversation produce two sessions
// instead of one overwriting the other.
//
// Sessions parsed from a conversations/<uuid>.db keep their untagged
// "antigravity:<uuid>" ID; only brain-transcript sessions, which are new, are
// keyed this way.
func antigravityBrainRootTag(root string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(root)))
	return hex.EncodeToString(sum[:])[:antigravityBrainRootTagLen]
}

// antigravityBrainRawID builds the storage ID (without the agent prefix) for
// conversation id's brain transcript under root.
func antigravityBrainRawID(root, id string) string {
	return antigravityBrainTag + antigravityBrainRootTag(root) + "-" + id
}

// antigravityBrainConversationForRawID reverses antigravityBrainRawID for one
// root: it returns the conversation id when rawID names a brain transcript
// under that root, so a stored session resolves back to its own file.
func antigravityBrainConversationForRawID(root, rawID string) (string, bool) {
	prefix := antigravityBrainTag + antigravityBrainRootTag(root) + "-"
	id, ok := strings.CutPrefix(rawID, prefix)
	if !ok || !IsValidSessionID(id) {
		return "", false
	}
	return id, true
}

// antigravityBrainTranscriptSources lists the brain transcripts under root
// that are sessions in their own right: those whose conversation has no
// conversations/<uuid>.db. A transcript whose conversation does have one is
// left out here and folded into that database's session instead, so one
// conversation never becomes two sessions.
func antigravityBrainTranscriptSources(root string) []string {
	if root == "" {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(root, "brain"))
	if err != nil {
		return nil
	}
	var paths []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id := entry.Name()
		if !IsValidSessionID(id) {
			continue
		}
		path := antigravityBrainTranscriptPath(root, id)
		if !IsRegularFile(path) {
			continue
		}
		if IsRegularFile(filepath.Join(root, "conversations", id+".db")) {
			continue
		}
		paths = append(paths, path)
	}
	slices.Sort(paths)
	return paths
}

// antigravityBrainTranscriptEntry is one logged step.
type antigravityBrainTranscriptEntry struct {
	stepIndex int64
	source    string
	stepType  string
	status    string
	createdAt time.Time
	content   string
	thinking  string
	toolCalls []ParsedToolCall
}

// readAntigravityBrainTranscript parses the transcript at path into entries
// ordered by step_index. A line that is not an object, or that carries no
// step fields at all, is skipped; the rest of the file still reads.
func readAntigravityBrainTranscript(
	path string,
) ([]antigravityBrainTranscriptEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(
		io.LimitReader(f, maxAntigravityBrainTranscriptBytes+1),
	)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxAntigravityBrainTranscriptBytes {
		return nil, fmt.Errorf(
			"antigravity brain transcript %s exceeds %d-byte cap",
			path, int64(maxAntigravityBrainTranscriptBytes),
		)
	}

	var entries []antigravityBrainTranscriptEntry
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !gjson.Valid(line) {
			continue
		}
		parsed := gjson.Parse(line)
		if !parsed.IsObject() {
			continue
		}
		entry := antigravityBrainTranscriptEntry{
			stepIndex: parsed.Get("step_index").Int(),
			source:    parsed.Get("source").Str,
			stepType:  parsed.Get("type").Str,
			status:    parsed.Get("status").Str,
			content:   parsed.Get("content").Str,
			thinking:  parsed.Get("thinking").Str,
			toolCalls: antigravityBrainToolCalls(parsed.Get("tool_calls")),
		}
		if entry.source == "" && entry.stepType == "" {
			continue
		}
		if stamp := parsed.Get("created_at").Str; stamp != "" {
			if ts, err := time.Parse(time.RFC3339Nano, stamp); err == nil {
				entry.createdAt = ts.UTC()
			}
		}
		entries = append(entries, entry)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].stepIndex < entries[j].stepIndex
	})
	return entries, nil
}

// antigravityBrainToolCalls reads the tool_calls array of one step. Arguments
// are kept as the raw JSON object the transcript carried, which is what the
// rest of the pipeline stores for a tool call's input.
func antigravityBrainToolCalls(calls gjson.Result) []ParsedToolCall {
	if !calls.IsArray() {
		return nil
	}
	var out []ParsedToolCall
	for _, call := range calls.Array() {
		name := call.Get("name").Str
		if name == "" {
			continue
		}
		out = append(out, ParsedToolCall{
			ToolName:  name,
			Category:  NormalizeToolCategory(name),
			InputJSON: call.Get("args").Raw,
		})
	}
	return out
}

// roleForAntigravityBrainSource maps a step's producer onto a message role. An
// unrecognized producer is treated as a system row rather than dropped: the
// step's text is still part of the conversation, and claiming it for the user
// or the model would be a guess.
func roleForAntigravityBrainSource(source string) (RoleType, bool) {
	switch source {
	case "USER_EXPLICIT":
		return RoleUser, false
	case "MODEL":
		return RoleAssistant, false
	default:
		return RoleSystem, true
	}
}

// collectAntigravityBrainTranscriptMessages renders one message per logged
// step, in step order. Ordinals are assigned by the caller's own ordering
// pass when the messages join a database session.
func collectAntigravityBrainTranscriptMessages(path string) []ParsedMessage {
	entries, err := readAntigravityBrainTranscript(path)
	if err != nil {
		return nil
	}
	messages := make([]ParsedMessage, 0, len(entries))
	for _, entry := range entries {
		role, isSystem := roleForAntigravityBrainSource(entry.source)
		content := entry.content
		toolCalls := entry.toolCalls
		if content == "" && len(toolCalls) > 0 {
			// A step that only called tools has no prose of its own, so the
			// call headers are the message text, exactly as the trajectory
			// reader renders them.
			headers := make([]string, 0, len(toolCalls))
			for i := range toolCalls {
				header := formatToolHeader(
					toolCalls[i].Category,
					agyToolDetail(
						toolCalls[i].ToolName, toolCalls[i].InputJSON,
					),
				)
				toolCalls[i].Rendering = header
				headers = append(headers, header)
			}
			content = strings.Join(headers, "\n")
		}
		messages = append(messages, ParsedMessage{
			Role:          role,
			IsSystem:      isSystem,
			Content:       content,
			ContentLength: len(content),
			ThinkingText:  entry.thinking,
			HasThinking:   entry.thinking != "",
			ToolCalls:     toolCalls,
			HasToolUse:    len(toolCalls) > 0,
			Timestamp:     entry.createdAt,
			SourceType:    entry.stepType,
			SourceSubtype: entry.status,
		})
	}
	return messages
}

// parseAntigravityBrainTranscriptSession parses a brain transcript that is a
// session in its own right, which is the case for every conversation whose
// own stream is encrypted and has no readable database beside it.
func parseAntigravityBrainTranscriptSession(
	path, root, id, project, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("stat %s: %w", path, err)
	}
	messages := collectAntigravityBrainTranscriptMessages(path)
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
					strings.ReplaceAll(m.Content, "\n", " "), 300,
				)
			}
		}
		if m.Timestamp.IsZero() {
			continue
		}
		if startedAt.IsZero() || m.Timestamp.Before(startedAt) {
			startedAt = m.Timestamp
		}
		if m.Timestamp.After(endedAt) {
			endedAt = m.Timestamp
		}
	}
	if startedAt.IsZero() {
		startedAt = info.ModTime()
	}
	if endedAt.IsZero() {
		endedAt = info.ModTime()
	}

	sess := &ParsedSession{
		ID:               antigravityIDPrefix + antigravityBrainRawID(root, id),
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
	accumulateMessageTokenUsage(sess, messages)
	return sess, messages, nil
}
