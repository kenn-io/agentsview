package gittrailer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	gitcmd "go.kenn.io/kit/git/cmd"
)

const (
	templateMarkerSuffix = ".agentsview-template-origin"
	// cutLine follows the comment string on the line above a verbose commit's diff.
	cutLine = "------------------------ >8 ------------------------"
)

// PrepareTemplate records the cleaned message Git prepared before editing. Git
// passes "template" as the source only when it loaded a commit template.
func PrepareTemplate(ctx context.Context, file, source string) error {
	marker := file + templateMarkerSuffix
	if source != "template" {
		if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	message, err := cleanMessage(ctx, file)
	if err != nil {
		return err
	}
	return os.WriteFile(marker, []byte(message), 0o600)
}

// Stamp adds session provenance only when Git has disabled the editor.
func Stamp(ctx context.Context, file string, session Session) error {
	template, err := consumeTemplateMarker(file)
	if err != nil {
		return err
	}
	if os.Getenv("GIT_EDITOR") != ":" || session.ID == "" {
		return nil
	}
	message, err := cleanMessage(ctx, file)
	if err != nil {
		return err
	}
	// Git rejects an untouched template the same way it rejects an empty
	// message, so a trailer must not make either look edited.
	if !hasContent(strings.TrimPrefix(message, template)) {
		return nil
	}
	_, _, err = (gitcmd.Runner{}).Run(ctx, "", nil, "interpret-trailers", "--in-place", "--no-divider", "--if-exists", "addIfDifferent", "--if-missing", "add", "--trailer", "Agent-Session: "+session.Agent+":"+session.ID, file)
	return err
}

// cleanMessage approximates the text Git judges after its commit cleanup: it
// drops a verbose commit's diff below the scissors line, then comments and
// surplus whitespace.
func cleanMessage(ctx context.Context, file string) (string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	message := string(data)
	offset := 0
	for line := range strings.Lines(message) {
		prefix, found := strings.CutSuffix(strings.TrimRight(line, "\r\n"), " "+cutLine)
		if found && prefix != "" && !strings.ContainsAny(prefix, " \t") {
			message = message[:offset]
			break
		}
		offset += len(line)
	}
	// Keep the hook's repository environment and the user's Git configuration.
	out, _, err := (gitcmd.Runner{}).Run(ctx, "", strings.NewReader(message), "stripspace", "--strip-comments")
	return string(out), err
}

// hasContent mirrors Git, which ignores blank lines and signoffs here.
func hasContent(message string) bool {
	for line := range strings.Lines(message) {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "Signed-off-by:") {
			return true
		}
	}
	return false
}

func consumeTemplateMarker(file string) (string, error) {
	marker := file + templateMarkerSuffix
	template, err := os.ReadFile(marker)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err == nil {
		err = os.Remove(marker)
	}
	if err != nil {
		return "", fmt.Errorf("cannot check commit template: %w", err)
	}
	return string(template), nil
}
