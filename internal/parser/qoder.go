package parser

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

const qoderIDPrefix = "qoder:"

type qoderSessionMeta struct {
	Title           string `json:"title"`
	ParentSessionID string `json:"parent_session_id"`
	ForkFrom        string `json:"fork_from"`
	WorkingDir      string `json:"working_dir"`
}

func DiscoverQoderSessions(projectsDir string) []DiscoveredFile {
	if projectsDir == "" {
		return nil
	}
	projects, err := os.ReadDir(projectsDir)
	if err != nil {
		return nil
	}

	var files []DiscoveredFile

	// SharedClientCache layout: files live directly under projectsDir with
	// no per-project subdirectory. Discover those flat files first and treat
	// the root itself as a single virtual project.
	files = append(files, qoderCollectFlatProject(projectsDir, projects)...)

	for _, projectEntry := range projects {
		if !isDirOrSymlink(projectEntry, projectsDir) {
			continue
		}
		projectDir := filepath.Join(projectsDir, projectEntry.Name())
		project := DecodeQoderProjectDir(projectEntry.Name())
		entries, err := os.ReadDir(projectDir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			if !entry.IsDir() && strings.HasSuffix(name, ".jsonl") {
				stem := strings.TrimSuffix(name, ".jsonl")
				if strings.HasPrefix(stem, "agent-") ||
					!IsValidQoderSessionID(stem) {
					continue
				}
				files = append(files, DiscoveredFile{
					Path:    filepath.Join(projectDir, name),
					Project: project,
					Agent:   AgentQoder,
				})
				continue
			}
			if !isDirOrSymlink(entry, projectDir) || !IsValidQoderSessionID(name) {
				continue
			}
			subagentsDir := filepath.Join(projectDir, name, "subagents")
			subagents, err := os.ReadDir(subagentsDir)
			if err != nil {
				continue
			}
			for _, sub := range subagents {
				if sub.IsDir() || !strings.HasSuffix(sub.Name(), ".jsonl") {
					continue
				}
				stem := strings.TrimSuffix(sub.Name(), ".jsonl")
				if !strings.HasPrefix(stem, "agent-") ||
					!IsValidQoderSessionID(stem) {
					continue
				}
				files = append(files, DiscoveredFile{
					Path:    filepath.Join(subagentsDir, sub.Name()),
					Project: project,
					Agent:   AgentQoder,
				})
			}
		}
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return files
}

// qoderCollectFlatProject treats the root directory itself as a single
// virtual project and discovers session .jsonl files written directly
// under it (the SharedClientCache layout). Files in subdirectories are
// left to the per-project loop in DiscoverQoderSessions. The project
// hint is the basename of the root's parent ("cli") when available,
// otherwise "SharedClientCache".
func qoderCollectFlatProject(
	root string, entries []os.DirEntry,
) []DiscoveredFile {
	var files []DiscoveredFile
	var value bool
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		stem := strings.TrimSuffix(entry.Name(), ".jsonl")
		if strings.HasPrefix(stem, "agent-") ||
			!IsValidQoderSessionID(stem) {
			continue
		}
		files = append(files, DiscoveredFile{
			Path:    filepath.Join(root, entry.Name()),
			Project: qoderFlatProjectHint(root),
			Agent:   AgentQoder,
		})
		value = true
	}
	if !value {
		return nil
	}
	return files
}

// qoderFlatProjectHint picks a stable project label for files discovered
// directly under a SharedClientCache projects root. Prefers the parent
// dir's basename (typically "cli") and falls back to "SharedClientCache"
// when the root has no parent (e.g. user-supplied QODER_PROJECTS_DIR).
func qoderFlatProjectHint(root string) string {
	root = filepath.Clean(root)
	parentDir := filepath.Dir(root)
	if parentDir == root || parentDir == "." {
		return "SharedClientCache"
	}
	return filepath.Base(parentDir)
}

func FindQoderSourceFile(projectsDir, rawID string) string {
	if projectsDir == "" {
		return ""
	}
	rawID = strings.TrimPrefix(rawID, qoderIDPrefix)
	sessionID, subagentID, hasSubagent := strings.Cut(rawID, ":subagent:")
	if !IsValidQoderSessionID(sessionID) {
		return ""
	}
	if hasSubagent &&
		(!strings.HasPrefix(subagentID, "agent-") || !IsValidQoderSessionID(subagentID)) {
		return ""
	}

	projects, err := os.ReadDir(projectsDir)
	if err != nil {
		return ""
	}
	// SharedClientCache flat layout: files live directly under projectsDir.
	// Check the root itself before descending into per-project subdirs.
	if !hasSubagent {
		candidate := filepath.Join(projectsDir, sessionID+".jsonl")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	for _, projectEntry := range projects {
		if !isDirOrSymlink(projectEntry, projectsDir) {
			continue
		}
		projectDir := filepath.Join(projectsDir, projectEntry.Name())
		candidate := filepath.Join(projectDir, sessionID+".jsonl")
		if hasSubagent {
			candidate = filepath.Join(projectDir, sessionID, "subagents", subagentID+".jsonl")
		}
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

func ParseQoderSession(path, project, machine string) ([]ParseResult, error) {
	results, _, err := ParseQoderSessionWithExclusions(path, project, machine)
	return results, err
}

func ParseQoderSessionWithExclusions(
	path, project, machine string,
) ([]ParseResult, []string, error) {
	results, excluded, err := claudeParseWithExclusions(
		path, project, machine,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("qoder parse %s: %w", path, err)
	}

	meta := readQoderSessionMeta(path)
	parentID, subagentID, isSubagent := qoderPathIDs(path, project)
	fileStem := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	for i := range results {
		retagQoderResult(&results[i], fileStem, parentID, subagentID, isSubagent)
		if !isSubagent {
			applyQoderMeta(&results[i].Session, meta)
		}
	}
	for i := range excluded {
		excluded[i] = qoderExcludedID(
			excluded[i], fileStem, parentID, subagentID, isSubagent,
		)
	}
	InferRelationshipTypes(results)
	return results, excluded, nil
}

func DecodeQoderProjectDir(encoded string) string {
	if !strings.HasPrefix(encoded, "-") {
		return NormalizeName(encoded)
	}
	parts := strings.Split(encoded, "-")
	for i := range len(parts) - 1 {
		if isQoderProjectParentDir(parts[i]) {
			project := strings.Join(parts[i+1:], "-")
			if project != "" {
				return NormalizeName(project)
			}
		}
	}
	for _, v := range slices.Backward(parts) {
		if v != "" {
			return NormalizeName(v)
		}
	}
	return NormalizeName(encoded)
}

func isQoderProjectParentDir(part string) bool {
	switch strings.ToLower(part) {
	case "code", "coding", "dev", "development", "projects", "repos", "src", "work", "workspace":
		return true
	default:
		return false
	}
}

func retagQoderResult(
	result *ParseResult,
	fileStem, parentID, subagentID string,
	isSubagent bool,
) {
	rawID := strings.TrimPrefix(result.Session.ID, qoderIDPrefix)
	if isSubagent {
		suffix := strings.TrimPrefix(rawID, fileStem)
		result.Session.ID = qoderSubagentID(parentID, subagentID+suffix)
		result.Session.ParentSessionID = qoderPrefixID(parentID)
		result.Session.RelationshipType = RelSubagent
	} else {
		result.Session.ID = qoderPrefixID(rawID)
		result.Session.ParentSessionID = qoderPrefixMaybe(result.Session.ParentSessionID)
	}
	result.Session.Agent = AgentQoder
	result.Session.AgentLabel = ""
	result.Session.Entrypoint = ""
	result.Session.SessionKind = ""
	toolCallParentID := result.Session.ID
	if !isSubagent {
		toolCallParentID = qoderPrefixID(fileStem)
	}
	retagQoderToolCalls(result.Messages, toolCallParentID)
}

func retagQoderToolCalls(messages []ParsedMessage, sessionID string) {
	for i := range messages {
		for j := range messages[i].ToolCalls {
			subagentID := messages[i].ToolCalls[j].SubagentSessionID
			if strings.HasPrefix(subagentID, "agent-") {
				messages[i].ToolCalls[j].SubagentSessionID =
					sessionID + ":subagent:" + subagentID
			}
		}
	}
}

func applyQoderMeta(sess *ParsedSession, meta qoderSessionMeta) {
	if meta.Title != "" {
		sess.SessionName = meta.Title
		// The sibling JSON is the high-priority title source, so a non-empty
		// value is an explicitly present title that must not be overwritten
		// by the shared database.
		sess.SessionNamePresent = true
	}
	if sess.Cwd == "" && meta.WorkingDir != "" {
		sess.Cwd = meta.WorkingDir
	}
	if meta.ForkFrom != "" && !hasParserDiscoveredForkParent(sess) {
		sess.ParentSessionID = qoderPrefixID(meta.ForkFrom)
		sess.RelationshipType = RelFork
	} else if sess.ParentSessionID == "" && meta.ParentSessionID != "" {
		sess.ParentSessionID = qoderPrefixID(meta.ParentSessionID)
	}
}

func hasParserDiscoveredForkParent(sess *ParsedSession) bool {
	return sess.RelationshipType == RelFork && sess.ParentSessionID != ""
}

func readQoderSessionMeta(path string) qoderSessionMeta {
	meta, _ := readQoderSessionMetaFile(path)
	return meta
}

// readQoderSessionMetaFile reads the sibling "<uuid>-session.json". A missing
// file is not an error: it is the expected low-priority case that falls
// through to the IDE database. A present but unreadable or malformed file is
// returned as an error so the caller can keep the stored title and retry
// instead of pretending the JSON held no title.
func readQoderSessionMetaFile(path string) (qoderSessionMeta, error) {
	metaPath := strings.TrimSuffix(path, ".jsonl") + "-session.json"
	data, err := os.ReadFile(metaPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return qoderSessionMeta{}, nil
		}
		return qoderSessionMeta{}, err
	}
	var meta qoderSessionMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return qoderSessionMeta{}, err
	}
	return meta, nil
}

// qoderAppDatabasePathOverride is a test hook that replaces the Application
// Support lookup. Production code leaves it nil; tests point it at a fixture
// database so the CN/international split can be exercised without the real
// clients installed.
var qoderAppDatabasePathOverride func(isCN bool) string

// resolveQoderAppSQLitePath binds a session file to the Qoder application
// database that owns its title. Binding is by configured root, never by a
// substring guess: a root that is not under one of the two recognized client
// directories (a custom root, a remote mirror) has no title database on this
// machine, so the caller must treat it as no signal rather than reading this
// machine's application database for it.
func resolveQoderAppSQLitePath(sessionPath string, roots []string) string {
	cleanPath := filepath.Clean(sessionPath)
	sep := string(filepath.Separator)
	matchedRoot := ""
	isCN := false
	for _, root := range roots {
		cleanRoot := filepath.Clean(root)
		if cleanPath != cleanRoot &&
			!strings.HasPrefix(cleanPath+sep, cleanRoot+sep) {
			continue
		}
		matchedRoot = cleanRoot
		// The client directory is an ancestor of the root, not the root's last
		// segment: default roots are <client>/projects.
		isCN = QoderClientAppForPath(cleanRoot) == qoderCNClientApp
		break
	}
	if qoderAppDatabasePathOverride != nil {
		return qoderAppDatabasePathOverride(isCN)
	}
	if matchedRoot == "" {
		return ""
	}
	app := qoderAppForRoot(matchedRoot)
	if app == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "Application Support", app, qoderAppDatabaseName)
}

// QoderSiblingJSONTitle reports the title carried by a Qoder transcript's
// sibling "<uuid>-session.json". That file outranks the shared application
// database, so a refresh that cannot rule it out must not write.
//
// present is true only for a non-empty JSON title. A missing file reports
// present=false with a nil error. An unreadable or malformed file returns an
// error: the caller cannot tell whether a high-priority title is being
// overwritten, so it must keep the stored name and retry.
func QoderSiblingJSONTitle(path string) (title string, present bool, err error) {
	meta, err := readQoderSessionMetaFile(path)
	if err != nil {
		return "", false, err
	}
	if meta.Title == "" {
		return "", false, nil
	}
	return meta.Title, true, nil
}

// applyQoderTitles resolves the title for a parsed Qoder session using the
// shared two-tier contract, and returns a non-empty retry reason when a
// readable title source failed. The sibling JSON wins whenever it carries a
// non-empty title (already applied by applyQoderMeta); otherwise the bound
// IDE database supplies the title, and an explicitly empty TEXT is allowed to
// clear the stored name. Missing files, missing rows, and SQL NULL are all
// no-signal and leave the stored name alone.
func applyQoderTitles(
	ctx context.Context, path string, roots []string, results []ParseResult,
) string {
	meta, jsonErr := readQoderSessionMetaFile(path)
	if jsonErr != nil {
		// A malformed or unreadable JSON could itself be a high-priority
		// title source, so falling through to the database would silently
		// replace it with a lower-priority value.
		return fmt.Sprintf("qoder session metadata read failed: %v", jsonErr)
	}
	if meta.Title != "" {
		return ""
	}
	databasePath := resolveQoderAppSQLitePath(path, roots)
	if databasePath == "" {
		markQoderTranscriptTitle(results)
		return ""
	}
	sessionID := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	title, present, err := readTitleFromDatabase(ctx, SharedTitleDatabase{
		Agent: AgentQoder, DBPath: databasePath,
	}, sessionID)
	if err != nil {
		return fmt.Sprintf("qoder title read failed: %v", err)
	}
	if !present {
		// Neither JSON nor the application database supplied a title. A
		// custom-title or /rename already parsed into the transcript is then
		// authoritative, including an empty /rename that clears the name.
		// A read failure returns before this point so a transcript title
		// cannot hide an unknown higher-priority source.
		markQoderTranscriptTitle(results)
		return ""
	}
	for i := range results {
		results[i].Session.SessionName = title
		results[i].Session.SessionNamePresent = true
	}
	return ""
}

// markQoderTranscriptTitle promotes an explicit transcript rename to a present
// title. claudeRenameSeen is set only for a non-empty custom-title or a
// /rename command; an empty custom-title is ignored by the parser and must
// not become a clear.
func markQoderTranscriptTitle(results []ParseResult) {
	for i := range results {
		if results[i].Session.claudeRenameSeen {
			results[i].Session.SessionNamePresent = true
		}
	}
}

func qoderPathIDs(path, _ string) (parentID, subagentID string, isSubagent bool) {
	if filepath.Base(filepath.Dir(path)) != "subagents" {
		return "", "", false
	}
	parent := filepath.Base(filepath.Dir(filepath.Dir(path)))
	if !IsValidQoderSessionID(parent) {
		return "", "", false
	}
	stem := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if !strings.HasPrefix(stem, "agent-") || !IsValidQoderSessionID(stem) {
		return "", "", false
	}
	return parent, stem, true
}

func qoderPrefixID(id string) string {
	id = strings.TrimPrefix(id, qoderIDPrefix)
	return qoderIDPrefix + id
}

func qoderPrefixMaybe(id string) string {
	if id == "" {
		return ""
	}
	return qoderPrefixID(id)
}

func qoderSubagentID(parentID, subagentID string) string {
	return qoderPrefixID(parentID) + ":subagent:" + subagentID
}

func qoderExcludedID(
	id, fileStem, parentID, subagentID string,
	isSubagent bool,
) string {
	rawID := strings.TrimPrefix(id, qoderIDPrefix)
	if isSubagent {
		suffix := strings.TrimPrefix(rawID, fileStem)
		return qoderSubagentID(parentID, subagentID+suffix)
	}
	return qoderPrefixID(rawID)
}
