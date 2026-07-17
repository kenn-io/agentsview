package parser

import (
	"encoding/json"
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
					!IsValidSessionID(stem) {
					continue
				}
				files = append(files, DiscoveredFile{
					Path:    filepath.Join(projectDir, name),
					Project: project,
					Agent:   AgentQoder,
				})
				continue
			}
			if !isDirOrSymlink(entry, projectDir) || !IsValidSessionID(name) {
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
					!IsValidSessionID(stem) {
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

func FindQoderSourceFile(projectsDir, rawID string) string {
	if projectsDir == "" {
		return ""
	}
	rawID = strings.TrimPrefix(rawID, qoderIDPrefix)
	sessionID, subagentID, hasSubagent := strings.Cut(rawID, ":subagent:")
	if !IsValidSessionID(sessionID) {
		return ""
	}
	if hasSubagent &&
		(!strings.HasPrefix(subagentID, "agent-") || !IsValidSessionID(subagentID)) {
		return ""
	}

	projects, err := os.ReadDir(projectsDir)
	if err != nil {
		return ""
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
	for i := 0; i < len(parts)-1; i++ {
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
	metaPath := strings.TrimSuffix(path, ".jsonl") + "-session.json"
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return qoderSessionMeta{}
	}
	var meta qoderSessionMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return qoderSessionMeta{}
	}
	return meta
}

func qoderPathIDs(path, _ string) (parentID, subagentID string, isSubagent bool) {
	if filepath.Base(filepath.Dir(path)) != "subagents" {
		return "", "", false
	}
	parent := filepath.Base(filepath.Dir(filepath.Dir(path)))
	if !IsValidSessionID(parent) {
		return "", "", false
	}
	stem := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if !strings.HasPrefix(stem, "agent-") || !IsValidSessionID(stem) {
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
