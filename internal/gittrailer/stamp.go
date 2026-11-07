package gittrailer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
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
	// Git compares against the template file, which lacks the signoff -s adds
	// before this hook runs; a later hook may rewrite that signoff.
	if signoff, err := generatedSignoff(ctx); err == nil {
		switch trimmed := strings.TrimRight(message, "\n"); {
		case trimmed == signoff:
			message = ""
		case strings.HasSuffix(trimmed, "\n"+signoff):
			message = strings.TrimRight(strings.TrimSuffix(trimmed, signoff), "\n") + "\n"
		}
	}
	return os.WriteFile(marker, []byte(message), 0o600)
}

// generatedSignoff returns the line git commit -s adds for the committer.
func generatedSignoff(ctx context.Context) (string, error) {
	out, _, err := (gitcmd.Runner{}).Run(ctx, "", nil, "var", "GIT_COMMITTER_IDENT")
	if err != nil {
		return "", err
	}
	ident := string(out)
	end := strings.LastIndex(ident, ">")
	if end < 0 {
		return "", errors.New("unexpected committer identity")
	}
	return "Signed-off-by: " + ident[:end+1], nil
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
	if cut := scissorsOffset(message, commentStrings(ctx)); cut >= 0 {
		message = message[:cut]
	}
	// Keep the hook's repository environment and the user's Git configuration.
	out, _, err := (gitcmd.Runner{}).Run(ctx, "", strings.NewReader(message), "stripspace", "--strip-comments")
	return string(out), err
}

// scissorsOffset returns where Git cuts the message, or -1. Git cuts at the
// first scissors line for its comment string. With core.commentChar=auto, Git
// picks one of several characters and records nowhere which, so only a
// scissors line followed by a comment with the same prefix counts, and the
// last one wins: Git writes its own after all message text.
func scissorsOffset(message string, comments []string) int {
	lines := slices.Collect(strings.Lines(message))
	cut, offset := -1, 0
	for i, line := range lines {
		for _, comment := range comments {
			if strings.TrimRight(line, "\r\n") != comment+" "+cutLine {
				continue
			}
			if len(comments) == 1 {
				return offset
			}
			if i+1 < len(lines) && strings.HasPrefix(lines[i+1], comment) {
				cut = offset
			}
		}
		offset += len(line)
	}
	return cut
}

// commentStrings returns the strings Git may put before the scissors line. The
// last of core.commentChar and core.commentString wins, and "auto" lets Git
// pick one character from a fixed set.
func commentStrings(ctx context.Context) []string {
	out, _, err := (gitcmd.Runner{}).Run(ctx, "", nil, "config", "--get-regexp", `^core\.comment(char|string)$`)
	if err != nil {
		return []string{"#"}
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	_, value, _ := strings.Cut(lines[len(lines)-1], " ")
	if value == "auto" {
		return strings.Split("#;@!$%^&|:", "")
	}
	return []string{value}
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
