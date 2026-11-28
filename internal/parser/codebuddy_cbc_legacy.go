package parser

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

// An IDE 5.x conversation converted from 4.x keeps its 4.x id as the
// transcript stem, but the converter copies neither the per-request token
// totals nor the pasted images. Both stay in the 4.x conversation directory,
// history/<md5(workspace)>/<id>/, under one of the configured 4.x roots.

// codebuddyLegacyCompanion is what a converted transcript still needs from
// its 4.x conversation directory.
type codebuddyLegacyCompanion struct {
	// complete reports that the manifest read as a message list and that
	// every message file it lists as an assistant's or without a role, which
	// may hold request totals, was read.
	complete bool
	// assetsDir holds the images codebuddy-asset://assets/<name> names.
	assetsDir string
	// usage holds one row per 4.x request, keyed by the id of the 4.x
	// assistant message that recorded the request totals.
	usage map[string]ParsedMessage
	// order lists usage keys by timestamp.
	order []string
}

// codebuddyLegacyHistoryDepth bounds the search for history directories
// under a 4.x root: Data/<user>/CodeBuddyIDE/<user>/history is the deepest
// layout CodeBuddy writes.
const codebuddyLegacyHistoryDepth = 5

var codebuddyLegacyHistoryCache = struct {
	sync.Mutex
	byRoot map[string]codebuddyLegacyHistoryDirs
}{byRoot: make(map[string]codebuddyLegacyHistoryDirs)}

type codebuddyLegacyHistoryDirs struct {
	dirs   []string
	readAt time.Time
}

// codebuddyLegacyHistoryDirsFor lists the history directories under one 4.x
// root, reusing a recent listing. A root that is itself a history directory,
// as remote sync mirrors one from a CodeBuddy home, lists itself.
func codebuddyLegacyHistoryDirsFor(ctx context.Context, root string) []string {
	codebuddyLegacyHistoryCache.Lock()
	cached, ok := codebuddyLegacyHistoryCache.byRoot[root]
	codebuddyLegacyHistoryCache.Unlock()
	if ok && time.Since(cached.readAt) < SQLiteContainerDigestVerifyInterval {
		return cached.dirs
	}
	dirs := walkCodeBuddyLegacyHistoryDirs(ctx, root)
	if ctx.Err() != nil {
		return dirs
	}
	codebuddyLegacyHistoryCache.Lock()
	if len(codebuddyLegacyHistoryCache.byRoot) >= 16 {
		clear(codebuddyLegacyHistoryCache.byRoot)
	}
	codebuddyLegacyHistoryCache.byRoot[root] = codebuddyLegacyHistoryDirs{dirs: dirs, readAt: time.Now()}
	codebuddyLegacyHistoryCache.Unlock()
	return dirs
}

// walkCodeBuddyLegacyHistoryDirs lists the history directories under one 4.x
// root from disk. An unreadable branch only narrows the search.
func walkCodeBuddyLegacyHistoryDirs(ctx context.Context, root string) []string {
	var dirs []string
	base := strings.Count(filepath.Clean(root), string(filepath.Separator))
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // An unreadable branch only narrows the search.
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if !d.IsDir() {
			return nil
		}
		if d.Name() == "history" {
			dirs = append(dirs, path)
			return filepath.SkipDir
		}
		if strings.Count(path, string(filepath.Separator))-base >= codebuddyLegacyHistoryDepth {
			return filepath.SkipDir
		}
		return nil
	})
	return dirs
}

// CodeBuddyHasLegacyConversations reports whether a 4.x root holds at least
// one conversation the 4.x reader imports, history/<ws>/<id>/index.json, so
// remote sync sends a root whole only when it is a 4.x data directory. It
// reads the disk on every call; an unreadable branch only narrows the search,
// and a canceled ctx stops it early.
func CodeBuddyHasLegacyConversations(ctx context.Context, root string) bool {
	for _, history := range walkCodeBuddyLegacyHistoryDirs(ctx, root) {
		workspaces, err := os.ReadDir(history)
		if err != nil {
			continue
		}
		for _, ws := range workspaces {
			if ctx.Err() != nil {
				return false
			}
			if !ws.IsDir() || !IsValidSessionID(ws.Name()) {
				continue
			}
			conversations, err := os.ReadDir(filepath.Join(history, ws.Name()))
			if err != nil {
				continue
			}
			for _, conv := range conversations {
				if conv.IsDir() && IsValidSessionID(conv.Name()) &&
					IsRegularFile(filepath.Join(history, ws.Name(), conv.Name(), "index.json")) {
					return true
				}
			}
		}
	}
	return false
}

// codebuddyLegacySessionDir finds the 4.x directory of conversation id. The
// workspace directory name is the MD5 of the workspace path; other
// workspaces are checked when the path changed spelling.
func codebuddyLegacySessionDir(ctx context.Context, roots []string, cwd, id string) string {
	if !IsValidSessionID(id) {
		return ""
	}
	hashed := ""
	if cwd != "" {
		sum := md5.Sum([]byte(cwd)) //nolint:gosec // CodeBuddy names workspace directories by this digest.
		hashed = hex.EncodeToString(sum[:])
	}
	for _, root := range roots {
		if strings.HasPrefix(root, "s3://") {
			continue
		}
		for _, history := range codebuddyLegacyHistoryDirsFor(ctx, root) {
			if hashed != "" && IsRegularFile(filepath.Join(history, hashed, id, "index.json")) {
				return filepath.Join(history, hashed, id)
			}
			workspaces, err := os.ReadDir(history)
			if err != nil {
				continue
			}
			for _, ws := range workspaces {
				if !ws.IsDir() {
					continue
				}
				dir := filepath.Join(history, ws.Name(), id)
				if IsRegularFile(filepath.Join(dir, "index.json")) {
					return dir
				}
			}
		}
	}
	return ""
}

// loadCodeBuddyLegacyCompanion reads the request totals and the asset
// directory of a converted conversation. A missing directory returns an
// empty companion, and a manifest that is not a message list or a listed
// assistant message that cannot be read returns one that is not complete. A
// manifest entry without a role takes the role its message file records, as
// the 4.x reader does.
func loadCodeBuddyLegacyCompanion(
	ctx context.Context, roots []string, cwd, id string,
) codebuddyLegacyCompanion {
	dir := codebuddyLegacySessionDir(ctx, roots, cwd, id)
	if dir == "" {
		return codebuddyLegacyCompanion{}
	}
	out := codebuddyLegacyCompanion{usage: map[string]ParsedMessage{}}
	if info, err := os.Stat(filepath.Join(dir, "assets")); err == nil && info.IsDir() {
		out.assetsDir = filepath.Join(dir, "assets")
	}
	index, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil || !gjson.ValidBytes(index) {
		return out
	}
	manifest := gjson.ParseBytes(index)
	if !manifest.IsObject() || !manifest.Get("messages").IsArray() {
		// The 4.x reader rejects such a manifest too.
		return out
	}
	out.complete = true
	type keyed struct {
		id string
		ts time.Time
	}
	// Each assistant message carrying a snapshot records its own totals.
	latest := map[string]keyed{}
	manifest.Get("messages").ForEach(func(_, ref gjson.Result) bool {
		if ctx.Err() != nil {
			// The caller's next context check reports the cancellation.
			return false
		}
		msgID := ref.Get("id").Str
		listedRole := ref.Get("role").Str
		if !IsValidSessionID(msgID) || (listedRole != "" && listedRole != "assistant") {
			return true
		}
		body, err := os.ReadFile(filepath.Join(dir, "messages", msgID+".json"))
		if err != nil || !gjson.ValidBytes(body) {
			// A message without a listed role may be an assistant's.
			out.complete = false
			return true
		}
		root := gjson.ParseBytes(body)
		if listedRole == "" && root.Get("role").Str != "assistant" {
			return true
		}
		extra := root.Get("extra")
		if extra.Type == gjson.String {
			extra = gjson.Parse(extra.Str)
		}
		msg, ok := codebuddyLegacyRequestUsage(extra)
		if !ok {
			return true
		}
		msg.Timestamp = parseCodeBuddyTimestamp(root.Get("createdAt").Str)
		msg.SourceUUID += ":" + msgID
		out.usage[msgID] = msg
		latest[msgID] = keyed{id: msgID, ts: msg.Timestamp}
		return true
	})
	keys := make([]keyed, 0, len(latest))
	for _, k := range latest {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b keyed) int {
		if c := a.ts.Compare(b.ts); c != 0 {
			return c
		}
		return strings.Compare(a.id, b.id)
	})
	for _, k := range keys {
		out.order = append(out.order, k.id)
	}
	return out
}

// writeCodeBuddyLegacyCompanionState writes the state of what a converted
// conversation reads from its 4.x directory: where the directory is, its
// manifest, its messages directory and each message file, and its assets
// directory and each image in it. The manifest, message files, and images
// count by content, because an edit or a repair can keep a file's size and
// modification time. A change to any of them, or the directory appearing,
// changes the written state. A missing directory writes only its absence.
// The directory is named by its workspace and conversation, the part of the
// path CodeBuddy chose, so the same history mirrored under another root,
// such as a remote sync's temporary extraction, writes the same state.
func writeCodeBuddyLegacyCompanionState(
	ctx context.Context, w io.Writer, roots []string, cwd, id string,
) error {
	dir := codebuddyLegacySessionDir(ctx, roots, cwd, id)
	if dir == "" {
		_, _ = io.WriteString(w, "legacy\x00absent\x00")
		return ctx.Err()
	}
	_, _ = fmt.Fprintf(w, "legacy\x00%s\x00%s\x00",
		filepath.Base(filepath.Dir(dir)), filepath.Base(dir))
	messages := filepath.Join(dir, "messages")
	assetsDir := filepath.Join(dir, "assets")
	paths := []string{filepath.Join(dir, "index.json"), messages, assetsDir}
	entries, _ := os.ReadDir(messages)
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".json") {
			paths = append(paths, filepath.Join(messages, entry.Name()))
		}
	}
	// Images are read by plain name from the top of the assets directory.
	images, _ := os.ReadDir(assetsDir)
	for _, entry := range images {
		if entry.Type().IsRegular() {
			paths = append(paths, filepath.Join(assetsDir, entry.Name()))
		}
	}
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		info, err := os.Stat(path)
		if err != nil {
			_, _ = fmt.Fprintf(w, "missing\x00%s\x00", filepath.ToSlash(rel))
			continue
		}
		if info.IsDir() {
			// A directory counts by its presence; its entries are listed
			// above by name and its files by content. Its size and times
			// change with every extraction, as in each remote sync, and
			// say nothing the entries do not.
			_, _ = fmt.Fprintf(w, "dir\x00%s\x00", filepath.ToSlash(rel))
			continue
		}
		content := ""
		// An image larger than the reader accepts stays a placeholder
		// whatever it holds, so its size alone decides.
		oversized := filepath.Dir(path) == assetsDir && info.Size() > maxCodeBuddyImageBytes
		if info.Mode().IsRegular() && !oversized {
			content, err = codebuddySidecarContentHash(ctx, path, info)
			if err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				// The file becoming readable changes this entry.
				content = "unreadable"
			}
		}
		_, _ = fmt.Fprintf(w, "file\x00%s\x00%d\x00%d\x00%s\x00",
			filepath.ToSlash(rel), info.Size(), info.ModTime().UnixNano(), content)
	}
	return nil
}

// codebuddyLegacyConversationID returns the conversation id of a path in a
// 4.x conversation directory, history/<ws>/<id>/..., under root. root may be
// the history directory itself.
func codebuddyLegacyConversationID(root, path string) (string, bool) {
	root = filepath.Clean(root)
	rel, ok := relUnder(root, filepath.Clean(path))
	if !ok {
		return "", false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if filepath.Base(root) == "history" {
		parts = append([]string{"history"}, parts...)
	}
	for i := 0; i+2 < len(parts); i++ {
		if parts[i] == "history" && IsValidSessionID(parts[i+1]) && IsValidSessionID(parts[i+2]) {
			return parts[i+2], true
		}
	}
	return "", false
}

// codebuddyInsertLegacyUsage adds a hidden usage row for each 4.x request
// whose recording message the converted transcript no longer contains, at
// its place in time, so the request still counts once. A total above the
// per-request bound adds its further parts right after that row. A request
// recorded with zero tokens keeps its row too, so the parse still carries its
// cb4: identity and recovery of an incomplete 4.x folder cannot restore an
// older archived total in its place.
func codebuddyInsertLegacyUsage(
	msgs []ParsedMessage, usage map[string]ParsedMessage, order []string,
	claimed map[string]bool,
) []ParsedMessage {
	inserted := false
	for _, id := range order {
		if claimed[id] {
			continue
		}
		total := usage[id]
		if len(total.TokenUsage) == 0 &&
			!total.HasContextTokens && !total.HasOutputTokens {
			continue
		}
		row := hiddenUsageRow(total)
		parts := codebuddySplitRequestTotal(&row)
		at := len(msgs)
		if !row.Timestamp.IsZero() {
			at = slices.IndexFunc(msgs, func(m ParsedMessage) bool {
				return m.Timestamp.After(row.Timestamp)
			})
			if at < 0 {
				at = len(msgs)
			}
		}
		msgs = slices.Insert(msgs, at, append([]ParsedMessage{row}, parts...)...)
		inserted = true
	}
	if inserted {
		for i := range msgs {
			msgs[i].Ordinal = i
		}
	}
	return msgs
}

// codebuddyLegacyUsageMissingFrom keeps the 4.x request totals a converted
// transcript does not already carry. Newer converters copy each request's
// totals into the transcript; older ones drop them.
func codebuddyLegacyUsageMissingFrom(
	usage map[string]ParsedMessage, items []codebuddyCBCItem,
) map[string]ParsedMessage {
	if len(usage) == 0 {
		return usage
	}
	carried := make(map[string]bool)
	for _, it := range items {
		pd := it.providerData()
		// Only a usage block the parser reads counts from replaces the 4.x
		// total; an empty one, such as rawUsage: {}, carries nothing.
		if _, ok := codebuddyCBCUsageFrom(pd); ok {
			if id := codebuddyUsageIdentity(pd); id != "" {
				carried[id] = true
			}
		}
	}
	out := make(map[string]ParsedMessage, len(usage))
	for key, msg := range usage {
		if !carried[msg.SourceUUID] {
			out[key] = msg
		}
	}
	return out
}

// codebuddyFillUsageModels names the model of usage rows recorded without
// one after the nearest earlier model in the conversation, so usage reports,
// which count only priced rows, still include them. inherited is the model
// in effect before msgs begin, such as the shared turns a rewound branch
// leaves out; it names rows that come before msgs name a model.
func codebuddyFillUsageModels(msgs []ParsedMessage, inherited string) {
	last := inherited
	for i := range msgs {
		if msgs[i].Model != "" {
			last = msgs[i].Model
			continue
		}
		if len(msgs[i].TokenUsage) > 0 && last != "" {
			msgs[i].Model = last
		}
	}
	if last == "" {
		return
	}
	first := ""
	for i := range msgs {
		if msgs[i].Model != "" {
			first = msgs[i].Model
			break
		}
	}
	for i := range msgs {
		if msgs[i].Model != "" {
			break
		}
		if len(msgs[i].TokenUsage) > 0 {
			msgs[i].Model = first
		}
	}
}

// codebuddyLegacyRequestUsage turns a 4.x request's statsSnapshot into one
// usage row. The snapshot sums every model step of the request: inputTokens
// is cache reads plus cache writes plus uncached input, and thinkingTokens is
// part of outputTokens. Context is the last step's prompt, the window the
// request ended with.
func codebuddyLegacyRequestUsage(extra gjson.Result) (ParsedMessage, bool) {
	stats := extra.Get("statsSnapshot")
	if stats.Type == gjson.String {
		stats = gjson.Parse(stats.Str)
	}
	request := extra.Get("requestId").Str
	if !stats.IsObject() || request == "" {
		return ParsedMessage{}, false
	}
	number := func(r gjson.Result) (int, bool) {
		if r.Type != gjson.Number || r.Int() < 0 {
			return 0, false
		}
		return int(r.Int()), true
	}
	prompt, hasPrompt := number(stats.Get("inputTokens"))
	output, hasOutput := number(stats.Get("outputTokens"))
	if !hasPrompt && !hasOutput {
		return ParsedMessage{}, false
	}
	read, hasRead := number(stats.Get("cachedInputTokens"))
	write, hasWrite := number(stats.Get("cacheWriteTokens"))
	reasoning, hasReason := number(stats.Get("thinkingTokens"))
	u := codebuddyCBCUsage{
		model:  firstNonEmptyJSONLString(extra.Get("modelId").Str, extra.Get("modelName").Str),
		output: output, cacheRead: read, cacheWrite: write,
		reasoning: min(reasoning, output),
		input:     max(prompt-read-write, 0),
		hasInput:  hasPrompt, hasOutput: hasOutput, hasRead: hasRead,
		hasWrite: hasWrite, hasReason: hasReason,
		requestTotal: true,
	}
	if last, ok := number(extra.Get("lastStepInputTokens")); ok {
		u.context, u.hasContext = last, true
	}
	msg := ParsedMessage{Role: RoleAssistant}
	u.apply(&msg)
	msg.SourceUUID = "cb4:" + request
	return msg, true
}
