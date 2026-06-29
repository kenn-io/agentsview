package parser

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Aider stores one Markdown chat log per repository at
// <repo>/.aider.chat.history.md. A single file accumulates many runs
// (one per aider process launch), each delimited by a
// "# aider chat started at <ts>" header (the only single-"#" line aider
// writes). The format is Markdown-derived, so roles are reconstructed
// from line prefixes (lower fidelity than the JSONL agents):
//
//   - "#### <text>"           -> a user prompt (Markdown h4).
//   - plain text after a turn -> assistant response.
//   - "> <text>"              -> aider tool/edit output (every aider
//     tool/warning/error line is blockquoted).
//
// aider records no per-message timestamps, so Message.Timestamp is the
// zero time for every message; a run's StartedAt is its own header
// (local naive time, assumed UTC, so it may be offset by the user's
// timezone) and there is no reliable EndedAt, so StartedAt is reused.
//
// Each run is indexed as its own session (mirroring the upstream
// sessiondex Rust adapter, which emits one session per run via a
// path#idx key). agentsview already supports multiple sessions per
// physical file via the virtual-path fan-out pattern used by Shelley and
// Zed, so aider reuses it: discoverAiderSessions returns the single
// physical file and the sync engine fans it out to one ParseResult per
// run. A run with no parseable turns (e.g. a header-only run) yields no
// session. Edited files are best-effort, taken from aider's own
// "Applied edit to" / "Creating empty file" lines. A leading "> "
// blockquote inside assistant prose is a known, rare misclassification,
// accepted and documented (mirrors the upstream adapter).

const (
	aiderIDPrefix     = "aider:"
	aiderHistoryFile  = ".aider.chat.history.md"
	aiderHeaderPrefix = "# aider chat started at "
	aiderTimeLayout   = "2006-01-02 15:04:05"
	aiderVPathSep     = "#"

	// aiderMaxWalkDepth bounds the rootless discovery walk. aider history
	// files sit at repository roots, rarely more than a few levels deep.
	aiderMaxWalkDepth = 4
	// aiderMaxFiles and aiderMaxDirs cap the walk so a pathological tree
	// cannot stall discovery.
	aiderMaxFiles = 5000
	aiderMaxDirs  = 50000
	// aiderWalkBudget bounds the wall-clock cost of the rootless discovery
	// walk, mirroring the upstream Rust adapter's WALK_BUDGET_SECS. A walk
	// that exceeds it stops early and returns whatever it found so far.
	aiderWalkBudget = 2 * time.Second
)

// aiderSkipDirs are directory names never descended into during
// discovery. Matched by exact name, never "all dotdirs", mirroring the
// upstream Rust adapter's skip-set.
var aiderSkipDirs = map[string]struct{}{
	".git":         {},
	"node_modules": {},
	"target":       {},
	".cache":       {},
	"Library":      {},
	"go":           {},
	".cargo":       {},
	".rustup":      {},
	".npm":         {},
	".pnpm-store":  {},
	".gradle":      {},
	".m2":          {},
	"vendor":       {},
	"dist":         {},
	"build":        {},
	".venv":        {},
	"venv":         {},
	"__pycache__":  {},
	".svn":         {},
	".hg":          {},
}

// aiderProtectedHomeDirs are first-level home folders that trigger macOS
// privacy prompts when a desktop app enumerates them. Aider discovery is
// opt-in, but a user-configured broad root such as $HOME still must not enter
// these folders unless the user explicitly configures one of them as the Aider
// root.
var aiderProtectedHomeDirs = map[string]struct{}{
	"Desktop":   {},
	"Documents": {},
	"Downloads": {},
	"Movies":    {},
	"Music":     {},
	"Photos":    {},
	"Pictures":  {},
}

// AiderDiscoverySkipDirNames returns the directory basenames pruned by Aider
// discovery. Remote SSH discovery uses this to mirror local discovery semantics.
func AiderDiscoverySkipDirNames() []string {
	names := make([]string, 0, len(aiderSkipDirs))
	for name := range aiderSkipDirs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// AiderDiscoveryMaxWalkDepth returns the maximum directory depth local Aider
// discovery descends below the configured root.
func AiderDiscoveryMaxWalkDepth() int { return aiderMaxWalkDepth }

// AiderDiscoveryMaxFiles returns the maximum number of Aider history files
// local discovery returns from one configured root.
func AiderDiscoveryMaxFiles() int { return aiderMaxFiles }

// AiderDiscoveryMaxDirs returns the maximum number of directories local Aider
// discovery visits below one configured root.
func AiderDiscoveryMaxDirs() int { return aiderMaxDirs }

// AiderHistoryFileName returns the fixed Markdown filename aider writes
// per repo (".aider.chat.history.md"). The sync engine uses it to match
// watched files back to the aider agent.
func AiderHistoryFileName() string { return aiderHistoryFile }

// aiderRun is one aider launch within a history file: a run header
// timestamp plus the raw body lines that follow it (until the next
// header). rawHeader is the verbatim timestamp string from the header,
// retained so the session ID can hash the header even when it is
// unparseable.
type aiderRun struct {
	started   time.Time
	hasTime   bool
	rawHeader string
	body      string
}

// parseAiderTimestamp parses aider's header timestamp
// "%Y-%m-%d %H:%M:%S" (local naive, no timezone) and assumes UTC. The
// shared parseTimestamp is RFC3339-oriented and would reject these, so
// aider gets its own narrow parser. ok is false for empty or unparseable
// input.
func parseAiderTimestamp(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(aiderTimeLayout, s)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// splitAiderRuns splits a history file into runs on the
// "# aider chat started at " header (the only single-"#" line aider
// writes). Bytes before the first header belong to no run and are
// dropped. A header with no body keeps its slot, so positional run order
// never shifts when new runs are appended. CRLF endings are tolerated.
func splitAiderRuns(content string) []aiderRun {
	var runs []aiderRun
	var cur *aiderRun
	// Accumulate the current run's body in a Builder rather than repeatedly
	// reallocating cur.body, so a large history file stays linear, not
	// quadratic, in copy cost.
	var body strings.Builder
	flush := func() {
		if cur != nil {
			cur.body = body.String()
			runs = append(runs, *cur)
		}
	}
	for raw := range strings.SplitSeq(content, "\n") {
		line := strings.TrimSuffix(raw, "\r")
		if ts, ok := strings.CutPrefix(line, aiderHeaderPrefix); ok {
			flush()
			body.Reset()
			started, hasTime := parseAiderTimestamp(ts)
			cur = &aiderRun{
				started:   started,
				hasTime:   hasTime,
				rawHeader: strings.TrimSpace(ts),
			}
			continue
		}
		if cur != nil {
			body.WriteString(line)
			body.WriteByte('\n')
		}
		// lines before the first header (cur == nil) are dropped.
	}
	flush()
	return runs
}

func aiderRunSourceRange(content string, idx int) (int, int, bool) {
	if idx < 0 {
		return 0, 0, false
	}
	runIdx := -1
	start := -1
	lineStart := 0
	for lineStart <= len(content) {
		lineEnd := len(content)
		if rel := strings.IndexByte(content[lineStart:], '\n'); rel >= 0 {
			lineEnd = lineStart + rel
		}
		if strings.HasPrefix(content[lineStart:lineEnd], aiderHeaderPrefix) {
			runIdx++
			if runIdx == idx {
				start = lineStart
			} else if runIdx == idx+1 && start >= 0 {
				return start, lineStart, true
			}
		}
		if lineEnd == len(content) {
			break
		}
		lineStart = lineEnd + 1
	}
	if start >= 0 {
		return start, len(content), true
	}
	return 0, 0, false
}

// WriteAiderRunMarkdown streams the raw Markdown source for one run in a shared
// aider history file. It preserves the selected run's header and body, drops any
// preamble before the first run, and never emits sibling runs from the same
// repository history.
func WriteAiderRunMarkdown(w io.Writer, historyPath string, idx int) error {
	data, err := os.ReadFile(historyPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", historyPath, err)
	}
	start, end, ok := aiderRunSourceRange(string(data), idx)
	if !ok {
		return fmt.Errorf(
			"aider run %d not found in %s: %w",
			idx, historyPath, os.ErrNotExist,
		)
	}
	_, err = w.Write(data[start:end])
	return err
}

// pushUniqueAider appends the trimmed path to v if it is non-empty and
// not already present (preserves first-seen order, de-duplicates).
func pushUniqueAider(v []string, s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return v
	}
	if slices.Contains(v, s) {
		return v
	}
	return append(v, s)
}

// parseAiderTurns reconstructs turns from a single run body. It is an
// anchored state machine, not per-line role guessing:
//
//   - "#### " (or a bare "####")    -> user channel.
//   - "> " (or a bare ">")          -> tool channel.
//   - blank lines                   -> continue the current channel.
//   - everything else               -> assistant channel (the default).
//
// A message is emitted whenever the channel switches. Tool output is
// surfaced as assistant transcript content because agentsview has no
// dedicated tool role. Edited files come from "Applied edit to" and
// "Creating empty file" tool lines (relative paths), de-duplicated;
// "Did not apply edit ... (--dry-run)" and "Skipping edits to ..."
// contribute nothing.
func parseAiderTurns(body string) ([]ParsedMessage, []string) {
	const (
		chanNone = iota
		chanUser
		chanAssistant
		chanTool
	)

	var (
		messages []ParsedMessage
		touched  []string
		curChan  = chanNone
		buf      []string
		ordinal  int
	)

	flush := func() {
		if curChan == chanNone {
			buf = buf[:0]
			return
		}
		text := strings.TrimSpace(strings.Join(buf, "\n"))
		buf = buf[:0]
		// Keep empty user turns (aider writes "#### " for empty input);
		// drop empty assistant/tool noise.
		if text == "" && curChan != chanUser {
			return
		}
		role := RoleAssistant
		if curChan == chanUser {
			role = RoleUser
		}
		messages = append(messages, ParsedMessage{
			Ordinal:       ordinal,
			Role:          role,
			Content:       text,
			ContentLength: len(text),
		})
		ordinal++
	}

	for raw := range strings.SplitSeq(body, "\n") {
		line := strings.TrimSuffix(raw, "\r")

		var lineChan int
		var content string
		switch {
		case strings.HasPrefix(line, "#### "):
			lineChan = chanUser
			content = strings.TrimRight(line[len("#### "):], " \t")
		case line == "####":
			lineChan = chanUser
		case strings.HasPrefix(line, "> "):
			lineChan = chanTool
			content = line[len("> "):]
			if p, ok := strings.CutPrefix(content, "Applied edit to "); ok {
				touched = pushUniqueAider(touched, p)
			} else if p, ok := strings.CutPrefix(content, "Creating empty file "); ok {
				touched = pushUniqueAider(touched, p)
			}
		case line == ">":
			lineChan = chanTool
		case strings.TrimSpace(line) == "":
			// blank: continue the current channel.
			if curChan != chanNone {
				buf = append(buf, "")
			}
			continue
		default:
			lineChan = chanAssistant
			content = line
		}

		if lineChan != curChan {
			flush()
			curChan = lineChan
		}
		buf = append(buf, content)
	}
	flush()
	return messages, touched
}

// AiderVirtualPath gives a single run within a history file a stable
// source identity for the AgentsView archive: "<historyPath>#<idx>",
// where idx is the run's positional index in the file. Mirrors
// ShelleyVirtualPath.
func AiderVirtualPath(path string, idx int) string {
	return path + aiderVPathSep + strconv.Itoa(idx)
}

// ParseAiderVirtualPath splits a virtual aider source path back into its
// physical history-file path and run index. It validates that the base
// name is the aider history file and the index is a non-negative
// integer, so a real path (no "#") or any other "#"-bearing path is
// rejected. Mirrors ParseShelleyVirtualPath.
func ParseAiderVirtualPath(path string) (string, int, bool) {
	sep := strings.LastIndex(path, aiderVPathSep)
	if sep < 0 {
		return "", 0, false
	}
	physical, idxStr := path[:sep], path[sep+1:]
	if filepath.Base(physical) != aiderHistoryFile || idxStr == "" {
		return "", 0, false
	}
	idx, err := strconv.Atoi(idxStr)
	if err != nil || idx < 0 {
		return "", 0, false
	}
	return physical, idx, true
}

// aiderAbsPath returns the absolute form of path, falling back to the
// input when it cannot be resolved. Used so the session ID hashes a
// stable absolute identity.
func aiderAbsPath(path string) string {
	if a, err := filepath.Abs(path); err == nil {
		return a
	}
	return path
}

// aiderRawID derives a stable session raw ID for one run. aider has no
// session id of its own, so the identity is the absolute history-file
// path plus the run's header-timestamp string plus an ordinal among the
// runs in that file that share the same header string. Hashing the header
// rather than the bare run index keeps IDs stable across the common
// mutations: appending a run, or removing/inserting a run with a DIFFERENT
// header, never re-keys other runs. The ordinal (not the absolute run
// index) is what is hashed, so a fresh-timestamp append leaves every
// earlier run's ID untouched.
//
// Residual limitation: when two or more runs in one file share a
// byte-identical header timestamp (aider headers have 1-second
// resolution, so this requires same-second runs in the same repo -- e.g.
// scripted/parallel invocations or a restart within the same second), the
// ordinal disambiguates them by position. Removing or inserting-before an
// earlier same-header run therefore re-keys its later same-header
// siblings. This is an accepted residual given its rarity; it is pinned by
// TestAiderSameHeaderEarlyRemovalRekeysSiblings. The "aider:" prefix is
// added by the session ID.
func aiderRawID(absPath, rawHeader string, equalHeaderOrdinal int) string {
	var b strings.Builder
	b.WriteString(absPath)
	b.WriteByte(0)
	b.WriteString(rawHeader)
	b.WriteByte(0)
	b.WriteString(strconv.Itoa(equalHeaderOrdinal))
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// aiderEqualHeaderOrdinals returns, for each run, its ordinal among the
// runs that share its exact header string. The first run with a given
// header gets 0, the second 1, and so on. This is what keeps IDs stable
// across appends: only equal-header runs disambiguate by position, so a
// new run with a fresh timestamp leaves every earlier run's ordinal
// untouched.
func aiderEqualHeaderOrdinals(runs []aiderRun) []int {
	ordinals := make([]int, len(runs))
	seen := make(map[string]int, len(runs))
	for i, run := range runs {
		ordinals[i] = seen[run.rawHeader]
		seen[run.rawHeader]++
	}
	return ordinals
}

// AiderRawIDAt recomputes the raw session ID (the per-run hash, without the
// "aider:" prefix) of the run at positional index idx in a history file. It
// returns ("", false) when the file is unreadable or idx is out of range.
// Callers use it to validate that a stored "<historyPath>#<idx>" virtual
// path still points at the run they expect: the index is positional, so an
// inserted or removed earlier run can shift it onto a different session.
func AiderRawIDAt(historyPath string, idx int) (string, bool) {
	data, err := os.ReadFile(historyPath)
	if err != nil {
		return "", false
	}
	runs := splitAiderRuns(string(data))
	if idx < 0 || idx >= len(runs) {
		return "", false
	}
	ordinals := aiderEqualHeaderOrdinals(runs)
	return aiderRawID(aiderAbsPath(historyPath), runs[idx].rawHeader, ordinals[idx]), true
}

// AiderVirtualPathForRawID resolves rawID to its current positional virtual
// path within one physical history file. This is used when a previously stored
// "<history>#<idx>" path has gone stale because an earlier run was inserted or
// removed after the last sync.
func AiderVirtualPathForRawID(historyPath, rawID string) (string, bool) {
	if historyPath == "" || rawID == "" {
		return "", false
	}
	data, err := os.ReadFile(historyPath)
	if err != nil {
		return "", false
	}
	runs := splitAiderRuns(string(data))
	if len(runs) == 0 {
		return "", false
	}
	absPath := aiderAbsPath(historyPath)
	ordinals := aiderEqualHeaderOrdinals(runs)
	for idx, run := range runs {
		if aiderRawID(absPath, run.rawHeader, ordinals[idx]) == rawID {
			return AiderVirtualPath(historyPath, idx), true
		}
	}
	return "", false
}

// AiderRunMeta describes one run within a history file: its virtual
// source path, positional index, and parsed start time. The sync engine
// fans a physical file out into one session per meta. HasMessages reports
// whether the run has parseable turns and so produces a session row;
// header-only runs keep a meta slot (to hold their positional index) but
// HasMessages is false, so the engine's unchanged-check does not expect a
// stored row for them.
type AiderRunMeta struct {
	VirtualPath string
	Idx         int
	Started     time.Time
	HasMessages bool
}

// ListAiderRunMetas reads a history file once and returns one meta per
// run it contains, in file order. It mirrors the per-conversation meta
// listers (e.g. ListShelleyConversationMetas) so the engine can fan a
// single physical file out into per-run sessions. Runs with no parseable
// header still get a meta slot so their positional index stays stable;
// the per-run parse drops runs with no messages (flagged via HasMessages).
// Returns nil for an unreadable or run-less file.
func ListAiderRunMetas(path string) ([]AiderRunMeta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	runs := splitAiderRuns(string(data))
	if len(runs) == 0 {
		return nil, nil
	}
	metas := make([]AiderRunMeta, 0, len(runs))
	for idx, run := range runs {
		msgs, _ := parseAiderTurns(run.body)
		metas = append(metas, AiderRunMeta{
			VirtualPath: AiderVirtualPath(path, idx),
			Idx:         idx,
			Started:     run.started,
			HasMessages: len(msgs) > 0,
		})
	}
	return metas, nil
}

// aiderIdentityPath returns the path whose absolute form seeds the run's
// session ID hash. When idPath is non-empty it is used verbatim (it is
// already a canonical identity, e.g. the remote physical history path), so
// the ID stays stable across syncs that read the file from a different
// location. When idPath is empty the on-disk path's absolute form is used,
// preserving the original local behavior exactly.
func aiderIdentityPath(path, idPath string) string {
	if idPath != "" {
		return idPath
	}
	return aiderAbsPath(path)
}

// buildAiderRunSession builds a single per-run session from one run's
// body and metadata. Returns (nil, nil) when the run has no parseable
// turns so the caller skips it cleanly. idPath, when non-empty, is the
// canonical identity path used to derive the stable session ID (see
// aiderIdentityPath); the on-disk path is still used for the stored virtual
// File.Path and for reading the file.
func buildAiderRunSession(
	path, idPath, machine string,
	info os.FileInfo,
	run aiderRun,
	idx, equalHeaderOrdinal int,
) (*ParsedSession, []ParsedMessage) {
	messages, _ := parseAiderTurns(run.body)
	// Edited files (parseAiderTurns' second return) are not surfaced as a
	// structured session field: agentsview has no per-session edited-files
	// column, and aider's own "Applied edit to ..." lines are preserved
	// verbatim in the assistant transcript content. The extraction is kept
	// and unit-tested at the parseAiderTurns level for parity with the
	// upstream adapter.
	if len(messages) == 0 {
		return nil, nil
	}

	var (
		firstMsg  string
		userCount int
	)
	for _, m := range messages {
		if m.Role != RoleUser || m.Content == "" {
			continue
		}
		userCount++
		if firstMsg == "" {
			firstMsg = truncate(strings.ReplaceAll(m.Content, "\n", " "), 300)
		}
	}

	project := GetProjectName(filepath.Base(filepath.Dir(path)))
	if project == "" {
		project = "unknown"
	}

	identity := aiderIdentityPath(path, idPath)
	// A run has no reliable end time (aider writes no per-message
	// timestamps and no run-end marker), so EndedAt mirrors StartedAt.
	sess := &ParsedSession{
		ID: aiderIDPrefix +
			aiderRawID(identity, run.rawHeader, equalHeaderOrdinal),
		Project:          project,
		Machine:          machine,
		Agent:            AgentAider,
		FirstMessage:     firstMsg,
		StartedAt:        run.started,
		EndedAt:          run.started,
		MessageCount:     len(messages),
		UserMessageCount: userCount,
		File: FileInfo{
			Path:  AiderVirtualPath(path, idx),
			Size:  info.Size(),
			Mtime: info.ModTime().UnixNano(),
		},
	}
	accumulateMessageTokenUsage(sess, messages)
	return sess, messages
}

// parseAiderRun parses a single run (by positional index) out of a
// history file into one session. The physical file is read and split on
// every call; callers parsing every run of a file should prefer
// parseAiderRuns, which reads the file once. Returns (nil, nil, nil)
// when the run does not exist or has no parseable turns.
func parseAiderRun(
	path string, idx int, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	return parseAiderRunWithID(path, "", idx, machine)
}

// parseAiderRunWithID is parseAiderRun with an explicit canonical identity
// path used to derive the stable session ID. idPath should be the run's
// canonical physical history path (e.g. the remote path during SSH sync);
// pass "" to fall back to the on-disk path, which is the local behavior.
// The file is always read from path; only the ID hash uses idPath.
func parseAiderRunWithID(
	path, idPath string, idx int, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("stat %s: %w", path, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	runs := splitAiderRuns(string(data))
	if idx < 0 || idx >= len(runs) {
		return nil, nil, nil
	}
	ordinals := aiderEqualHeaderOrdinals(runs)
	sess, msgs := buildAiderRunSession(
		path, idPath, machine, info, runs[idx], idx, ordinals[idx],
	)
	if sess == nil {
		return nil, nil, nil
	}
	return sess, msgs, nil
}

// parseAiderRuns reads a history file once and parses every run into its
// own ParseResult, in file order. Runs with no parseable turns are
// dropped. Returns nil for an unreadable or run-less file. This is the
// fan-out entry point used by the sync engine; parseAiderRun is the
// single-run lookup used when resolving one virtual path.
func parseAiderRuns(path, machine string) ([]ParseResult, error) {
	return parseAiderRunsWithID(path, "", machine)
}

// parseAiderRunsWithID is parseAiderRuns with an explicit canonical
// identity path used to derive stable session IDs for every run. idPath
// should be the file's canonical physical history path (e.g. the remote
// path during SSH sync, where path is a random temp extraction dir); pass
// "" to fall back to the on-disk path, which is the local behavior. The
// file is always read from path; only the per-run ID hash uses idPath, so
// the IDs stay stable across syncs that extract the file to a different
// temp location.
func parseAiderRunsWithID(path, idPath, machine string) ([]ParseResult, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	runs := splitAiderRuns(string(data))
	if len(runs) == 0 {
		return nil, nil
	}
	ordinals := aiderEqualHeaderOrdinals(runs)
	var results []ParseResult
	for idx, run := range runs {
		sess, msgs := buildAiderRunSession(
			path, idPath, machine, info, run, idx, ordinals[idx],
		)
		if sess == nil {
			continue
		}
		results = append(results, ParseResult{Session: *sess, Messages: msgs})
	}
	return results, nil
}

// discoverAiderSessions walks root looking for .aider.chat.history.md
// files. aider is rootless (no central store), so this is a bounded,
// depth-capped, symlink-safe walk: it descends at most aiderMaxWalkDepth
// levels, never follows symlinks, skips a fixed set of large vendor /
// build / VCS directories by exact name, and opens only the exact
// filename. A wall-clock budget (aiderWalkBudget) stops the walk early
// on a pathological tree. The walk is best-effort and silent: permission
// errors, over-cap trees, and a blown budget are skipped rather than
// surfaced, so a partial scan still indexes whatever it found. Each
// discovered physical file is fanned out into one session per run by the
// sync engine.
func discoverAiderSessions(root string) []DiscoveredFile {
	if root == "" {
		return nil
	}
	home := aiderHomeDir()
	skipProtectedHomeDirs := aiderShouldSkipProtectedHomeDirs(root, home, runtime.GOOS)
	rootDepth := strings.Count(filepath.Clean(root), string(os.PathSeparator))
	walkRoots := aiderDiscoveryWalkRoots(root, home, runtime.GOOS)

	var files []DiscoveredFile
	if skipProtectedHomeDirs {
		files = appendAiderHistoryFile(files, filepath.Join(root, aiderHistoryFile))
	}
	dirCount := 0
	start := time.Now()
	stopAll := false
	for _, walkRoot := range walkRoots {
		if stopAll {
			break
		}
		_ = filepath.WalkDir(walkRoot, func(
			path string, d os.DirEntry, err error,
		) error {
			// Wall-clock budget: stop the whole walk once it is exceeded,
			// returning whatever was found so far. Mirrors the upstream Rust
			// adapter's WALK_BUDGET_SECS.
			if time.Since(start) >= aiderWalkBudget {
				stopAll = true
				return filepath.SkipAll
			}
			if err != nil {
				// Unreadable entry: skip it (and its subtree if a dir) but
				// keep walking the rest of the tree.
				if d != nil && d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				// Never follow symlinked directories.
				if d.Type()&os.ModeSymlink != 0 {
					return filepath.SkipDir
				}
				depth := strings.Count(
					filepath.Clean(path), string(os.PathSeparator),
				) - rootDepth
				if skipProtectedHomeDirs && depth == 1 {
					if _, skip := aiderProtectedHomeDirs[d.Name()]; skip {
						return filepath.SkipDir
					}
				}
				if path != root {
					if _, skip := aiderSkipDirs[d.Name()]; skip {
						return filepath.SkipDir
					}
				}
				// Skip descent BELOW the cap, but still visit files in a
				// directory AT the cap, so a history file exactly
				// aiderMaxWalkDepth levels under the root is discovered (the
				// documented N-level scan). A `>=` test would skip the
				// max-depth directory before its files were seen.
				if depth > aiderMaxWalkDepth {
					return filepath.SkipDir
				}
				dirCount++
				if dirCount > aiderMaxDirs {
					return filepath.SkipDir
				}
				return nil
			}
			if d.Name() != aiderHistoryFile {
				return nil
			}
			// Skip symlinked files; only index real history files.
			if d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			files = append(files, DiscoveredFile{
				Path:  path,
				Agent: AgentAider,
			})
			if len(files) >= aiderMaxFiles {
				stopAll = true
				return filepath.SkipAll
			}
			return nil
		})
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return files
}

func aiderDiscoveryWalkRoots(root, home, goos string) []string {
	if !aiderShouldSkipProtectedHomeDirs(root, home, goos) {
		return []string{root}
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	roots := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if _, skip := aiderProtectedHomeDirs[name]; skip {
			continue
		}
		if _, skip := aiderSkipDirs[name]; skip {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
			continue
		}
		roots = append(roots, filepath.Join(root, name))
	}
	return roots
}

func appendAiderHistoryFile(files []DiscoveredFile, path string) []DiscoveredFile {
	info, err := os.Lstat(path)
	if err != nil || info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return files
	}
	return append(files, DiscoveredFile{
		Path:  path,
		Agent: AgentAider,
	})
}

func aiderHomeDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return home
}

func aiderShouldSkipProtectedHomeDirs(root, home, goos string) bool {
	if goos != "darwin" || root == "" || home == "" {
		return false
	}
	return filepath.Clean(root) == filepath.Clean(home)
}

// findAiderSourceFile resolves a single aider run's virtual source path
// ("<historyPath>#<idx>") from a root directory and a raw session ID (the
// per-run hash). It re-runs the bounded discovery walk to find candidate
// history files, then, for each, reads and splits it once to recompute
// the per-run IDs and match rawID. It returns the matching virtual path,
// or "" when nothing under root produces rawID. The physical file is
// stat-ed via os.Stat (not re-walked) for the per-run parse downstream.
func findAiderSourceFile(root, rawID string) string {
	if root == "" || rawID == "" {
		return ""
	}
	for _, f := range discoverAiderSessions(root) {
		if path, ok := AiderVirtualPathForRawID(f.Path, rawID); ok {
			return path
		}
	}
	return ""
}
