package parser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// CodeBuddy CLI and IDE 5.x share the CBC JSONL transcript format under
// <home>/projects/<project>/<session>.jsonl. Each line is a history item:
// conversation items (message, reasoning, function_call,
// function_call_result) linked by parentId, plus producer-defined custom
// items that carry titles, usage snapshots, and replay markers.

// codebuddyCBCCustomTypes lists the producer's custom item types. They never
// enter the replayed conversation, mirroring the CLI's isCustomInputItem.
var codebuddyCBCCustomTypes = map[string]bool{
	"custom-title": true, "ai-title": true, "file-history-snapshot": true,
	"summary": true, "topic": true, "goal-result": true,
	"goal-progress": true, "goal-state": true, "turn-metrics": true,
	"resend-fork-notice": true, "acp-terminal-state": true,
	"request-recovery": true, "model-usage": true, "credit-usage": true,
	"context-usage": true, "checkpoint-restore": true,
}

// codebuddyCBCItem is one decoded transcript line.
type codebuddyCBCItem struct {
	index           int
	id              string
	parentID        string
	logicalParentID string
	itemType        string
	role            string
	timestamp       time.Time
	raw             gjson.Result
}

func (it codebuddyCBCItem) providerData() gjson.Result {
	return it.raw.Get("providerData")
}

func (it codebuddyCBCItem) custom() bool {
	return codebuddyCBCCustomTypes[it.itemType]
}

// codebuddyCBCConversationTypes are the item types that carry a turn of the
// conversation itself.
var codebuddyCBCConversationTypes = map[string]bool{
	"message": true, "reasoning": true, "function_call": true,
	"function_call_result": true,
}

// hasConversation reports whether the transcript holds any conversation
// item. A file that is empty, holds only session-meta or custom items, or
// cannot be parsed has none.
func (t *codebuddyCBCTranscript) hasConversation() bool {
	for _, it := range t.items {
		if codebuddyCBCConversationTypes[it.itemType] {
			return true
		}
	}
	return false
}

// codebuddyCBCHasConversation reports whether the transcript at path holds
// any conversation item, stopping at the first one.
func codebuddyCBCHasConversation(ctx context.Context, path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	lr := newLineReaderContext(ctx, f, maxLineSize)
	defer releaseLineReader(lr)
	for {
		line, ok := lr.next()
		if !ok {
			return false
		}
		if !gjson.Valid(line) {
			continue
		}
		if codebuddyCBCConversationTypes[gjson.Get(line, "type").Str] {
			return true
		}
	}
}

// codebuddyCBCHeader is what the start of a main transcript records about
// its conversation, read without parsing the rest of the file.
type codebuddyCBCHeader struct {
	// sessionID is the first recorded sessionId: the runtime session id
	// that keys the conversation's catalog row. It can differ from the
	// filename stem, which names the archived session.
	sessionID string
	// cwd is the working directory recorded up to that line; empty when
	// the header does not record one.
	cwd string
	// converted reports a conversation the IDE converted from 4.x. The
	// converter writes its session-meta, with the session id and working
	// directory, as the first line.
	converted bool
}

// The header read stops at the first line carrying a sessionId, which
// producers write on the first line, or at the first conversation item. The
// line bound keeps a transcript without either from being read in full. Each
// line may be as long as a full parse accepts, so a first item carrying a
// large pasted text still yields its session id.
const codebuddyCBCHeaderLines = 64

// readCodeBuddyCBCHeader reads the header of the main transcript at path. A
// subagent transcript, whose catalog key and archive identity both come from
// its path, or an unreadable file returns an empty header.
func readCodeBuddyCBCHeader(ctx context.Context, path string) codebuddyCBCHeader {
	var h codebuddyCBCHeader
	if codebuddyCBCIdentityForPath(path).subagent {
		return h
	}
	f, err := os.Open(path)
	if err != nil {
		return h
	}
	defer f.Close()
	lr := newLineReaderContext(ctx, f, maxLineSize)
	defer releaseLineReader(lr)
	for range codebuddyCBCHeaderLines {
		line, ok := lr.nextBytes()
		if !ok {
			break
		}
		if !gjson.ValidBytes(line) {
			continue
		}
		root := gjson.ParseBytes(line)
		if !root.IsObject() {
			continue
		}
		h.sessionID = root.Get("sessionId").Str
		h.cwd = firstNonEmptyJSONLString(h.cwd, root.Get("cwd").Str)
		itemType := root.Get("type").Str
		if itemType == "session-meta" && root.Get(`meta.codebuddy\.ai/converterVersion`).Str != "" {
			h.converted = true
		}
		if h.sessionID != "" || codebuddyCBCConversationTypes[itemType] {
			break
		}
	}
	return h
}

// catalogKey returns the id that keys the catalog row of the transcript at
// path: the session id it records, or its filename stem without one.
func (h codebuddyCBCHeader) catalogKey(path string) string {
	if h.sessionID != "" {
		return h.sessionID
	}
	return codebuddyCBCIdentityForPath(path).runtimeID
}

// codebuddyCBCTranscript is a whole transcript read once.
type codebuddyCBCTranscript struct {
	items      []codebuddyCBCItem
	malformed  int
	truncated  bool
	sessionID  string
	cwd        string
	firstTime  time.Time
	lastTime   time.Time
	titleFacts []codebuddyTitleFact
	entrypoint string
	converted  bool
	teamMember string
	// speaker is the first providerData.agent naming a run other than the
	// main CLI, IDE, or compaction agent; in a subagent transcript it is the
	// subagent's display name.
	speaker    string
	stopReason string
}

type codebuddyTitleFact struct {
	kind      string
	text      string
	timestamp int64
	index     int
}

func readCodeBuddyCBCTranscript(
	ctx context.Context, path string,
) (*codebuddyCBCTranscript, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	t := &codebuddyCBCTranscript{}
	lr := newLineReaderContext(ctx, f, maxLineSize)
	defer releaseLineReader(lr)
	lastLineValid := true
	lastLineHasData := false
	index := 0
	for {
		line, ok := lr.next()
		if !ok {
			break
		}
		lastLineHasData = strings.TrimSpace(line) != ""
		if !gjson.Valid(line) {
			lastLineValid = false
			t.malformed++
			continue
		}
		lastLineValid = true
		root := gjson.Parse(line)
		if !root.IsObject() {
			t.malformed++
			continue
		}
		t.observe(root)
		itemType := root.Get("type").Str
		if itemType == "session-meta" {
			t.observeMeta(root)
			continue
		}
		switch itemType {
		case "custom-title", "ai-title", "topic":
			t.addTitleFact(root, index)
		case "acp-terminal-state":
			if reason := root.Get("stopReason").Str; reason != "" {
				t.stopReason = reason
			}
		}
		t.items = append(t.items, codebuddyCBCItem{
			index:           index,
			id:              root.Get("id").Str,
			parentID:        root.Get("parentId").Str,
			logicalParentID: root.Get("logicalParentId").Str,
			itemType:        itemType,
			role:            root.Get("role").Str,
			timestamp:       codebuddyCBCTimestamp(root.Get("timestamp")),
			raw:             root,
		})
		index++
	}
	if err := lr.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	t.malformed += lr.SkippedLines()
	t.truncated = lastLineHasData && !lastLineValid &&
		!fileEndsWithNewline(f, info.Size())
	if t.truncated && t.malformed > 0 {
		// The cut-off tail is an in-progress write, not a corrupt record.
		t.malformed--
	}
	return t, nil
}

func (t *codebuddyCBCTranscript) observe(root gjson.Result) {
	if ts := codebuddyCBCTimestamp(root.Get("timestamp")); !ts.IsZero() {
		if t.firstTime.IsZero() || ts.Before(t.firstTime) {
			t.firstTime = ts
		}
		if ts.After(t.lastTime) {
			t.lastTime = ts
		}
	}
	if sid := root.Get("sessionId").Str; t.sessionID == "" && sid != "" {
		t.sessionID = sid
	}
	if cwd := root.Get("cwd").Str; t.cwd == "" && cwd != "" {
		t.cwd = cwd
	}
	agent := strings.TrimSpace(root.Get("providerData.agent").Str)
	if t.speaker == "" && agent != "" && !codebuddyRuntimeAgents[agent] {
		t.speaker = agent
	}
	if t.entrypoint == "" {
		switch agent {
		case "cli":
			t.entrypoint = "cli"
		case "craft":
			t.entrypoint = "ide"
		case "codebuddy-ide-4.x":
			t.entrypoint = "ide-4.x"
		}
	}
}

// codebuddyRuntimeAgents are providerData.agent values that name the main
// run or an internal step rather than a subagent. The producer otherwise
// stamps each subagent item with the team member name, or with the subagent
// preset name when the run is not a team member.
var codebuddyRuntimeAgents = map[string]bool{
	"cli": true, "craft": true, "codebuddy-ide-4.x": true, "compact": true,
}

func (t *codebuddyCBCTranscript) observeMeta(root gjson.Result) {
	meta := root.Get("meta")
	if meta.Get(`codebuddy\.ai/converterVersion`).Str != "" {
		t.converted = true
	}
	if member := meta.Get("teamContext.memberName").Str; member != "" {
		t.teamMember = member
	}
	meta.Get(`codebuddy\.ai/sessionTitle`).ForEach(func(_, fact gjson.Result) bool {
		t.addTitleFact(fact, -1)
		return true
	})
}

func (t *codebuddyCBCTranscript) addTitleFact(fact gjson.Result, index int) {
	kind := fact.Get("type").Str
	field := map[string]string{
		"custom-title": "customTitle", "ai-title": "aiTitle", "topic": "topic",
	}[kind]
	if field == "" {
		return
	}
	text := strings.TrimSpace(fact.Get(field).Str)
	if text == "" {
		return
	}
	t.titleFacts = append(t.titleFacts, codebuddyTitleFact{
		kind: kind, text: text, timestamp: fact.Get("timestamp").Int(),
		index: index,
	})
}

func codebuddyCBCTimestamp(v gjson.Result) time.Time {
	switch v.Type {
	case gjson.Number:
		if ms := v.Int(); ms > 0 {
			return time.UnixMilli(ms)
		}
	case gjson.String:
		if ts, err := time.Parse(time.RFC3339Nano, v.Str); err == nil {
			return ts
		}
	}
	return time.Time{}
}

var (
	codebuddyImagePathRe    = regexp.MustCompile(`(?s)\s*<image_local_path>.*?</image_local_path>\s*`)
	codebuddyImageMentionRe = regexp.MustCompile(`(^|\s)@image#\d+:(?:"(?:\\.|[^"\\])*"|\S+)`)
)

// codebuddyCBCTitles resolves the producer's title facts. custom is the
// newest user-set title; generated is the newest non-placeholder AI title,
// falling back to the newest topic.
func codebuddyCBCTitles(facts []codebuddyTitleFact) (custom, generated string) {
	ordered := slices.Clone(facts)
	slices.SortStableFunc(ordered, func(a, b codebuddyTitleFact) int {
		switch {
		case a.timestamp < b.timestamp:
			return -1
		case a.timestamp > b.timestamp:
			return 1
		}
		return a.index - b.index
	})
	pick := func(kind string) string {
		for i := len(ordered) - 1; i >= 0; i-- {
			fact := ordered[i]
			if fact.kind != kind {
				continue
			}
			if kind == "custom-title" {
				if text := codebuddyStripCustomTitleImages(fact.text); text != "" {
					return text
				}
				continue
			}
			if !codebuddyPlaceholderTitle(fact.text) {
				return fact.text
			}
		}
		return ""
	}
	custom = pick("custom-title")
	generated = pick("ai-title")
	if generated == "" {
		generated = pick("topic")
	}
	return custom, generated
}

func codebuddyStripImagePaths(text string) string {
	text = codebuddyImagePathRe.ReplaceAllString(text, " ")
	if i := strings.Index(text, "<image_local_path>"); i >= 0 {
		text = text[:i]
	}
	return strings.TrimSpace(text)
}

func codebuddyStripCustomTitleImages(text string) string {
	text = codebuddyStripImagePaths(text)
	text = codebuddyImageMentionRe.ReplaceAllString(text, "$1")
	return strings.Join(strings.Fields(text), " ")
}

func codebuddyPlaceholderTitle(text string) bool {
	text = strings.TrimSpace(text)
	return text == "" || text == "(No content)" || text == "/compact" ||
		codebuddyStripImagePaths(text) == ""
}

// codebuddyCBCPathParts reports the project directory and transcript stems
// for a main or subagent CBC transcript path.
func codebuddyCBCPathParts(path string) (projectDir, sessionStem, agentStem string) {
	path = filepath.Clean(path)
	base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	dir := filepath.Dir(path)
	if filepath.Base(dir) == "subagents" {
		sessionDir := filepath.Dir(dir)
		return filepath.Dir(sessionDir), filepath.Base(sessionDir), base
	}
	return dir, base, ""
}
