package parser

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ParsePositronSession parses a Positron Assistant chat session
// file. The format is identical to VSCode Copilot sessions.
// Returns (nil, nil, nil) if the file is empty or contains no
// meaningful content.
func ParsePositronSession(
	path, project, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("stat %s: %w", path, err)
	}

	var data []byte
	if strings.HasSuffix(path, ".jsonl") {
		data, err = reconstructJSONL(path)
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) == 0 {
		return nil, nil, nil
	}

	// Reuse VSCode Copilot parsing logic since formats are identical
	sess, msgs, err := parseVSCodeCopilotData(
		data, path, project, machine,
	)
	if err != nil {
		return nil, nil, err
	}
	if sess == nil {
		return nil, nil, nil
	}

	// Override agent type and ID prefix for Positron
	sess.Agent = AgentPositron
	sess.ID = "positron:" + sess.ID

	sess.File = FileInfo{
		Path:  path,
		Size:  info.Size(),
		Mtime: info.ModTime().UnixNano(),
	}

	return sess, msgs, nil
}

// DiscoverPositronSessions finds all chat session files under the
// Positron User directory. The structure mirrors VSCode:
// <userDir>/workspaceStorage/<hash>/chatSessions/<uuid>.json
func DiscoverPositronSessions(userDir string) []DiscoveredFile {
	if userDir == "" {
		return nil
	}

	var files []DiscoveredFile

	// Scan workspaceStorage/<hash>/chatSessions/*.{json,jsonl}
	wsDir := filepath.Join(userDir, "workspaceStorage")
	hashDirs, err := os.ReadDir(wsDir)
	if err != nil {
		return nil
	}

	for _, entry := range hashDirs {
		if !entry.IsDir() {
			continue
		}

		hashPath := filepath.Join(wsDir, entry.Name())
		chatDir := filepath.Join(hashPath, "chatSessions")
		sessionFiles, err := os.ReadDir(chatDir)
		if err != nil {
			continue
		}

		// Read workspace.json to get project name
		project := ReadVSCodeWorkspaceManifest(hashPath)
		if project == "" {
			project = "unknown"
		}

		for _, f := range sessionFiles {
			if f.IsDir() {
				continue
			}
			name := f.Name()
			if !strings.HasSuffix(name, ".json") &&
				!strings.HasSuffix(name, ".jsonl") {
				continue
			}
			files = append(files, DiscoveredFile{
				Path:    filepath.Join(chatDir, name),
				Project: project,
				Agent:   AgentPositron,
			})
		}
	}

	return files
}

// FindPositronSourceFile locates a Positron session file by its
// raw ID (prefix already stripped).
func FindPositronSourceFile(userDir, rawID string) string {
	if userDir == "" || !IsValidSessionID(rawID) {
		return ""
	}

	// Search through workspaceStorage
	wsDir := filepath.Join(userDir, "workspaceStorage")
	hashDirs, err := os.ReadDir(wsDir)
	if err != nil {
		return ""
	}

	for _, entry := range hashDirs {
		if !entry.IsDir() {
			continue
		}
		base := filepath.Join(
			wsDir, entry.Name(), "chatSessions",
		)
		// Prefer .jsonl over .json
		for _, ext := range []string{".jsonl", ".json"} {
			candidate := filepath.Join(base, rawID+ext)
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
	}

	return ""
}
