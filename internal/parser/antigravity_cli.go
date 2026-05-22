package parser

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// Antigravity CLI sessions live under ~/.gemini/antigravity-cli/:
//
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

// DiscoverAntigravityCLISessions enumerates conversations/*.pb and
// implicit/*.pb under the CLI root and tags each with its workspace
// (resolved via history.jsonl).
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
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if !strings.HasSuffix(name, ".pb") {
				continue
			}
			id := strings.TrimSuffix(name, ".pb")
			if !IsValidSessionID(id) {
				continue
			}
			files = append(files, DiscoveredFile{
				Path:    filepath.Join(dir, name),
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

// FindAntigravityCLISourceFile locates the .pb file for a session
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
		p := filepath.Join(root, "implicit", uuid+".pb")
		if _, err := os.Stat(p); err == nil {
			return p
		}
		return ""
	}
	if !IsValidSessionID(id) {
		return ""
	}
	p := filepath.Join(root, "conversations", id+".pb")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

// ParseAntigravityCLISession parses one CLI session into the
// canonical ParsedSession + messages shape.
func ParseAntigravityCLISession(
	path, project, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("stat %s: %w", path, err)
	}
	id := strings.TrimSuffix(filepath.Base(path), ".pb")
	if !IsValidSessionID(id) {
		return nil, nil, fmt.Errorf(
			"invalid Antigravity CLI session filename: %s", path,
		)
	}
	// Root = two levels up from the .pb file
	// (conversations/<id>.pb or implicit/<id>.pb).
	root := filepath.Dir(filepath.Dir(path))
	// Tag implicit/ sessions so they don't collide with the
	// conversations/ entry that may share the same UUID.
	storageID := id
	if filepath.Base(filepath.Dir(path)) == "implicit" {
		storageID = antigravityImplicitTag + id
	}

	messages := collectAntigravityHistoryMessages(
		filepath.Join(root, "history.jsonl"), id,
	)
	messages = append(messages,
		collectAntigravityBrainMessages(
			filepath.Join(root, "brain", id),
		)...,
	)

	if hasAntigravityKey() {
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
			Size:  info.Size(),
			Mtime: info.ModTime().UnixNano(),
		},
	}
	if len(messages) == 0 {
		return sess, nil, nil
	}
	return sess, messages, nil
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
