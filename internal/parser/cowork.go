// ABOUTME: Parses Claude Desktop "cowork" (local agent mode) sessions.
// ABOUTME: Reuses the Claude Code JSONL parser on the nested transcript and
// enriches it with cowork session metadata (title, project, timestamps).
package parser

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// coworkIDPrefix is prepended to every cowork session ID so cowork
// conversations never collide with regular Claude Code sessions, which
// share the same on-disk transcript format.
const coworkIDPrefix = "cowork:"

// Cowork on-disk layout (Claude Desktop local agent mode):
//
//	<root>/<orgId>/<workspaceId>/local_<uuid>.json          session metadata
//	<root>/<orgId>/<workspaceId>/local_<uuid>/.claude/projects/<enc>/<cliSessionId>.jsonl
//	<root>/<orgId>/<workspaceId>/local_<uuid>/.claude/projects/<enc>/<cliSessionId>/subagents/**/agent-<id>.jsonl
//
// The metadata file (local_<uuid>.json) carries the human-facing title,
// model, and the cliSessionId that names the transcript. The transcript
// itself is a standard Claude Code JSONL file (same uuid/parentUuid DAG,
// message.usage token accounting, and event types), so the Claude parser
// handles it directly. The encoded project directory name varies between
// versions (".../-...-outputs" for host-loop sessions, "/sessions/<name>"
// for VM sessions), so the transcript is located by scanning the projects
// directory for "<cliSessionId>.jsonl" rather than reconstructing the name.
// Subagent transcripts live alongside it under subagents/, mirroring
// regular Claude Code, and are ingested the same way.

// coworkMeta holds the fields of a local_<uuid>.json metadata file that
// enrich the parsed session.
type coworkMeta struct {
	SessionID           string   `json:"sessionId"`
	CliSessionID        string   `json:"cliSessionId"`
	Title               string   `json:"title"`
	UserSelectedFolders []string `json:"userSelectedFolders"`
	CreatedAt           int64    `json:"createdAt"`      // epoch milliseconds
	LastActivityAt      int64    `json:"lastActivityAt"` // epoch milliseconds
}

// readCoworkMeta reads a local_<uuid>.json metadata file. Missing or
// malformed files yield a zero-value meta so the transcript can still be
// parsed on its own.
func readCoworkMeta(path string) coworkMeta {
	var meta coworkMeta
	if path == "" {
		return meta
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return meta
	}
	_ = json.Unmarshal(data, &meta)
	return meta
}

// coworkProjectName derives a project grouping for a cowork session.
// Cowork conversations are not tied to a code repository, so they group
// under "cowork" unless the user explicitly attached local folders.
func coworkProjectName(meta coworkMeta) string {
	for _, folder := range meta.UserSelectedFolders {
		if p := ExtractProjectFromCwd(folder); p != "" {
			return p
		}
	}
	return "cowork"
}

// isCoworkMetaFileName reports whether name is a cowork session metadata
// file (local_<uuid>.json), distinguishing it from sibling cache files
// such as cowork-clientdata-cache.json or cowork_settings.json.
func isCoworkMetaFileName(name string) bool {
	if !strings.HasPrefix(name, "local_") ||
		!strings.HasSuffix(name, ".json") {
		return false
	}
	return IsValidSessionID(strings.TrimSuffix(name, ".json"))
}

// resolveCoworkSession locates the Claude Code transcript for a cowork
// session given its working directory (the local_<uuid> dir) and the
// cliSessionId. It returns the transcript path and the encoded project
// directory that contains it (used to locate subagent transcripts), or
// ("", "") when no transcript exists yet. A symlinked project directory
// is followed only when its target stays inside the session directory,
// mirroring the containment guard used by Cursor discovery.
func resolveCoworkSession(sessionDir, cliSessionID string) (string, string) {
	if sessionDir == "" || !IsValidSessionID(cliSessionID) {
		return "", ""
	}
	projectsDir := filepath.Join(sessionDir, ".claude", "projects")
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return "", ""
	}
	resolvedRoot, err := filepath.EvalSymlinks(sessionDir)
	if err != nil {
		return "", ""
	}
	target := cliSessionID + ".jsonl"
	for _, entry := range entries {
		if !isDirOrSymlink(entry, projectsDir) {
			continue
		}
		encDir := filepath.Join(projectsDir, entry.Name())
		candidate := filepath.Join(encDir, target)
		if !IsRegularFile(candidate) {
			continue
		}
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil || !isContainedIn(resolved, resolvedRoot) {
			continue
		}
		return candidate, encDir
	}
	return "", ""
}

// coworkSubagentTranscripts returns the subagent transcript files
// (agent-*.jsonl) for a cowork session, found under
// <encDir>/<cliSessionId>/subagents/**, mirroring regular Claude Code.
func coworkSubagentTranscripts(encDir, cliSessionID string) []string {
	if encDir == "" {
		return nil
	}
	subagentsDir := filepath.Join(encDir, cliSessionID, "subagents")
	var out []string
	_ = filepath.WalkDir(
		subagentsDir,
		func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			name := d.Name()
			if !strings.HasPrefix(name, "agent-") ||
				!strings.HasSuffix(name, ".jsonl") {
				return nil
			}
			if !IsRegularFile(path) {
				return nil
			}
			out = append(out, path)
			return nil
		},
	)
	sort.Strings(out)
	return out
}

// coworkMetaPathForTranscript derives the local_<uuid>.json metadata path
// from a transcript path. The transcript lives under
// <sessionDir>/.claude/projects/..., and the metadata file is the sibling
// <sessionDir>.json. Returns "" when the path is not a cowork transcript.
func coworkMetaPathForTranscript(transcriptPath string) string {
	sep := string(filepath.Separator)
	marker := sep + ".claude" + sep + "projects" + sep
	sessionDir, _, ok := strings.Cut(transcriptPath, marker)
	if !ok {
		return ""
	}
	return sessionDir + ".json"
}

// CoworkSessionMtime returns the larger of a cowork transcript's mtime and
// its metadata file's mtime (nanoseconds). The human-facing title lives in
// the metadata file, so a rename changes only the metadata; folding its
// mtime into the skip key ensures renames are re-parsed instead of skipped
// as "unchanged". Returns transcriptMtime when the metadata file is absent
// or unreadable.
func CoworkSessionMtime(transcriptPath string, transcriptMtime int64) int64 {
	metaPath := coworkMetaPathForTranscript(transcriptPath)
	if metaPath == "" {
		return transcriptMtime
	}
	info, err := os.Stat(metaPath)
	if err != nil {
		return transcriptMtime
	}
	if m := info.ModTime().UnixNano(); m > transcriptMtime {
		return m
	}
	return transcriptMtime
}

// walkCoworkSessions traverses a cowork root and invokes fn for every
// session transcript on disk (the main conversation and each subagent).
// The walk is shallow: it descends only the org/workspace directory
// structure and never enters the session working directories
// (local_<uuid>/, which hold large .claude/outputs subtrees) or the
// sibling skills-plugin mirror.
func walkCoworkSessions(root string, fn func(transcriptPath string)) {
	if root == "" {
		return
	}
	_ = filepath.WalkDir(
		root,
		func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if path == root {
					return nil
				}
				name := d.Name()
				if strings.HasPrefix(name, "local_") ||
					name == "skills-plugin" ||
					name == "node_modules" ||
					name == ".git" {
					return filepath.SkipDir
				}
				return nil
			}
			if !isCoworkMetaFileName(d.Name()) {
				return nil
			}
			meta := readCoworkMeta(path)
			if meta.CliSessionID == "" {
				return nil
			}
			sessionDir := strings.TrimSuffix(path, ".json")
			main, encDir := resolveCoworkSession(
				sessionDir, meta.CliSessionID,
			)
			if main == "" {
				return nil
			}
			fn(main)
			for _, sub := range coworkSubagentTranscripts(
				encDir, meta.CliSessionID,
			) {
				fn(sub)
			}
			return nil
		},
	)
}

// relUnder returns the path of child relative to dir when child is
// strictly contained within dir, mirroring the engine's isUnder helper so
// the parser can classify paths without importing sync internals.
func relUnder(dir, child string) (string, bool) {
	if dir == "" {
		return "", false
	}
	rel, err := filepath.Rel(dir, child)
	if err != nil {
		return "", false
	}
	if rel == "." || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// extractCoworkAITitle scans a cowork transcript for the most recent
// "ai-title" event, which carries the auto-generated conversation title.
// Returns "" when no title event is present.
func extractCoworkAITitle(transcriptPath string) string {
	f, err := os.Open(transcriptPath)
	if err != nil {
		return ""
	}
	defer f.Close()

	lr := newLineReader(f, maxLineSize)
	title := ""
	for {
		line, ok := lr.next()
		if !ok {
			break
		}
		if !gjson.Valid(line) {
			continue
		}
		if gjson.Get(line, "type").Str != "ai-title" {
			continue
		}
		if t := gjson.Get(line, "aiTitle").Str; t != "" {
			title = t
		}
	}
	return title
}

// parseSession parses a cowork session transcript. It reuses the Claude
// Code parser on the transcript and then rewrites the results into the
// cowork namespace: agent type, "cowork:"-prefixed IDs, the session title,
// and metadata-derived timestamps for transcripts that carry none. Returns
// parsed results plus session IDs the parser intentionally excluded
// (prefixed), matching ParseClaudeSessionWithExclusions.
func parseCoworkSession(
	transcriptPath, machine string,
) ([]ParseResult, []string, error) {
	metaPath := coworkMetaPathForTranscript(transcriptPath)
	meta := readCoworkMeta(metaPath)
	project := coworkProjectName(meta)

	results, excluded, err := claudeParseWithExclusions(
		transcriptPath, project, machine,
	)
	if err != nil {
		return nil, nil, err
	}

	title := strings.TrimSpace(meta.Title)
	if title == "" {
		title = strings.TrimSpace(extractCoworkAITitle(transcriptPath))
	}

	// Infer relationship types on the raw (unprefixed) IDs before
	// rewriting them into the cowork namespace, so the "agent-" subagent
	// heuristic still applies.
	InferRelationshipTypes(results)
	applyCoworkIdentity(results, title, meta)

	// Fold the metadata file's mtime into the stored session mtime so a
	// later title rename (which touches only the metadata) is detected as
	// a change and re-parsed rather than skipped as unchanged.
	if len(results) > 0 {
		composite := CoworkSessionMtime(
			transcriptPath, results[0].Session.File.Mtime,
		)
		for i := range results {
			results[i].Session.File.Mtime = composite
		}
	}

	for i := range excluded {
		excluded[i] = coworkIDPrefix + excluded[i]
	}
	return results, excluded, nil
}

// applyCoworkIdentity rewrites Claude parse results into the cowork
// namespace. Every session ID, parent ID, and subagent link is prefixed
// uniformly so relationships are preserved.
func applyCoworkIdentity(
	results []ParseResult, title string, meta coworkMeta,
) {
	for i := range results {
		sess := &results[i].Session
		sess.Agent = AgentCowork
		sess.ID = coworkIDPrefix + sess.ID
		if sess.ParentSessionID != "" {
			sess.ParentSessionID = coworkIDPrefix + sess.ParentSessionID
		}
		if title != "" {
			sess.SessionName = title
		}
		// Fall back to metadata timestamps for transcripts with no
		// message timestamps (e.g. a session created but never run).
		if sess.StartedAt.IsZero() && meta.CreatedAt > 0 {
			sess.StartedAt = time.UnixMilli(meta.CreatedAt).UTC()
		}
		if sess.EndedAt.IsZero() && meta.LastActivityAt > 0 {
			sess.EndedAt = time.UnixMilli(meta.LastActivityAt).UTC()
		}

		for m := range results[i].Messages {
			msg := &results[i].Messages[m]
			for t := range msg.ToolCalls {
				tc := &msg.ToolCalls[t]
				if tc.SubagentSessionID != "" {
					tc.SubagentSessionID = coworkIDPrefix + tc.SubagentSessionID
				}
				for r := range tc.ResultEvents {
					if tc.ResultEvents[r].SubagentSessionID != "" {
						tc.ResultEvents[r].SubagentSessionID =
							coworkIDPrefix + tc.ResultEvents[r].SubagentSessionID
					}
				}
			}
		}
	}
}
