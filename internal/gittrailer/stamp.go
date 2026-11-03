package gittrailer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"strings"

	gitcmd "go.kenn.io/kit/git/cmd"
)

const templateMarkerSuffix = ".agentsview-template-origin"

// PrepareTemplate records the message Git prepared before editing. Git passes
// "template" as the source only when it loaded a commit template.
func PrepareTemplate(file, source string) error {
	marker := file + templateMarkerSuffix
	if source != "template" {
		if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	message, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(message)
	return os.WriteFile(marker, digest[:], 0o600)
}

// Stamp adds session provenance only when Git has disabled the editor.
func Stamp(ctx context.Context, file string, session Session) error {
	unchangedTemplate, err := consumeTemplateMarker(file)
	if err != nil {
		return err
	}
	if unchangedTemplate {
		return nil
	}
	if os.Getenv("GIT_EDITOR") != ":" || session.ID == "" {
		return nil
	}
	message, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	// Keep the hook's repository environment and the user's Git configuration.
	runner := gitcmd.Runner{}
	message, _, err = runner.Run(ctx, "", strings.NewReader(string(message)), "stripspace", "--strip-comments")
	if err != nil {
		return err
	}
	empty := true
	for line := range strings.SplitSeq(string(message), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "Signed-off-by:") {
			empty = false
			break
		}
	}
	if empty {
		return nil
	}
	_, _, err = runner.Run(ctx, "", nil, "interpret-trailers", "--in-place", "--no-divider", "--if-exists", "addIfDifferent", "--if-missing", "add", "--trailer", "Agent-Session: "+session.Agent+":"+session.ID, file)
	return err
}

func consumeTemplateMarker(file string) (bool, error) {
	marker := file + templateMarkerSuffix
	digest, err := os.ReadFile(marker)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	if err := os.Remove(marker); err != nil {
		return true, err
	}
	if len(digest) != sha256.Size {
		return true, errors.New("invalid commit template marker")
	}
	message, err := os.ReadFile(file)
	if err != nil {
		return true, err
	}
	current := sha256.Sum256(message)
	return bytes.Equal(digest, current[:]), nil
}
