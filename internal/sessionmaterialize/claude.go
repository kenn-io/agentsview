package sessionmaterialize

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"go.kenn.io/agentsview/internal/capture"
)

var claudeProjectOverride = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
var reservedProjectOverride = regexp.MustCompile(`(?i)^(con|prn|aux|nul|com[0-9]|lpt[0-9])$`)

func claudeDestination(cwd, sessionID string) (string, error) {
	if !capture.ValidUUID(sessionID) {
		return "", errors.New("invalid Claude session ID")
	}
	home := os.Getenv("CLAUDE_CONFIG_DIR")
	explicitHome := home != ""
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		home = filepath.Join(userHome, ".claude")
	}
	project := capture.ClaudeProjectDir(cwd)
	if override := os.Getenv("CLAUDE_CODE_PROJECT_DIR_NAME"); explicitHome && claudeProjectOverride.MatchString(override) && !reservedProjectOverride.MatchString(override) {
		project = override
	}
	return filepath.Join(home, "projects", project, sessionID+".jsonl"), nil
}

// clearClaudeWorktree preserves the transcript while preventing Claude from
// restoring a worktree belonging to the machine that recorded it.
func clearClaudeWorktree(data []byte, sessionID string) ([]byte, error) {
	var bound bool
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var entry map[string]json.RawMessage
		if err := json.Unmarshal(line, &entry); err != nil {
			return nil, fmt.Errorf("invalid transcript record: %w", err)
		}
		var kind string
		if err := json.Unmarshal(entry["type"], &kind); err != nil || kind != "worktree-state" {
			continue
		}
		var id string
		if err := json.Unmarshal(entry["sessionId"], &id); err != nil || !capture.ValidUUID(id) {
			return nil, errors.New("worktree-state record has no valid sessionId")
		}
		if id == sessionID {
			value, ok := entry["worktreeSession"]
			bound = ok && !bytes.Equal(bytes.TrimSpace(value), []byte("null"))
		}
	}
	if !bound {
		return data, nil
	}
	out := append([]byte(nil), data...)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	out = append(out, []byte(fmt.Sprintf("{\"type\":\"worktree-state\",\"worktreeSession\":null,\"sessionId\":%q}\n", sessionID))...)
	return out, nil
}
