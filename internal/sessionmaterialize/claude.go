package sessionmaterialize

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"go.kenn.io/agentsview/internal/capture"
	"golang.org/x/text/unicode/norm"
)

var (
	claudeProjectOverride   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	reservedProjectOverride = regexp.MustCompile(`(?i)^(con|prn|aux|nul|com[0-9]|lpt[0-9])$`)
)

func claudeDestination(cwd, sessionID string) (string, string, string, error) {
	home := os.Getenv("CLAUDE_CONFIG_DIR")
	explicitHome := home != ""
	rawHome := home
	if explicitHome && !filepath.IsAbs(home) {
		return "", "", "", errors.New("CLAUDE_CONFIG_DIR must be absolute for materialize; use the absolute path when starting Claude")
	}
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", "", "", err
		}
		home = filepath.Join(userHome, ".claude")
	}
	home, err := filepath.Abs(norm.NFC.String(home))
	if err != nil {
		return "", "", "", err
	}
	project := capture.ClaudeProjectDir(cwd)
	var selectedOverride string
	if override := os.Getenv("CLAUDE_CODE_PROJECT_DIR_NAME"); explicitHome && claudeProjectOverride.MatchString(override) && !reservedProjectOverride.MatchString(override) {
		project = override
		selectedOverride = override
	}
	return filepath.Join(home, "projects", project, sessionID+".jsonl"), rawHome, selectedOverride, nil
}

// clearClaudeWorktree preserves the transcript while preventing Claude from
// restoring a worktree belonging to the machine that recorded it.
func clearClaudeWorktree(data []byte, sessionID, sourceSessionID string) ([]byte, error) {
	var bound bool
	var found bool
	for line := range bytes.SplitSeq(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var entry map[string]jsontext.Value
		if err := json.Unmarshal(line, &entry); err != nil {
			return nil, fmt.Errorf("invalid transcript record: %w", err)
		}
		var kind string
		if err := json.Unmarshal(entry["type"], &kind); err != nil {
			continue
		}
		if kind == "user" || kind == "assistant" {
			var id string
			var message map[string]jsontext.Value
			messageErr := json.Unmarshal(entry["message"], &message)
			content, ok := message["content"]
			if json.Unmarshal(entry["sessionId"], &id) == nil && (id == sessionID || (sourceSessionID != "" && id == sourceSessionID)) && messageErr == nil && ok && !bytes.Equal(bytes.TrimSpace(content), []byte("null")) {
				found = true
			}
		}
		if kind != "worktree-state" {
			continue
		}
		var id string
		if err := json.Unmarshal(entry["sessionId"], &id); err != nil || !capture.ValidUUID(id) {
			return nil, errors.New("invalid worktree session ID")
		}
		if id == sessionID {
			value, ok := entry["worktreeSession"]
			bound = ok && !bytes.Equal(bytes.TrimSpace(value), []byte("null"))
		}
	}
	if !found {
		return nil, errors.New("transcript has no messages for this session")
	}
	if !bound {
		return data, nil
	}
	out := data
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	out = append(out, []byte(fmt.Sprintf("{\"type\":\"worktree-state\",\"worktreeSession\":null,\"sessionId\":%q}\n", sessionID))...)
	return out, nil
}
