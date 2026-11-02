package gittrailer

import (
	"context"
	"os"
	"strings"

	gitcmd "go.kenn.io/kit/git/cmd"
)

// Stamp adds session provenance only when Git has disabled the editor.
func Stamp(ctx context.Context, file string, session Session) error {
	if os.Getenv("GIT_EDITOR") != ":" || session.ID == "" {
		return nil
	}
	if err := session.validate(); err != nil {
		return err
	}
	message, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	// Keep the hook's repository environment and the user's Git configuration.
	runner := gitcmd.Runner{}
	cleanup, _, err := runner.Run(ctx, "", nil, "config", "--default", "default", "--get", "commit.cleanup")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(cleanup)) == "strip" {
		message, _, err = runner.Run(ctx, "", strings.NewReader(string(message)), "stripspace", "--strip-comments")
		if err != nil {
			return err
		}
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
