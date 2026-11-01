package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/gittrailer"
)

func newGitTrailerCommand() *cobra.Command {
	return &cobra.Command{
		Use: "git-trailer <message-file> [<source>]", Short: "Add optional agent session provenance to a Git commit message", GroupID: groupMeta,
		DisableFlagParsing: true,
		Run:                func(cmd *cobra.Command, args []string) { runGitTrailer(args, cmd.ErrOrStderr()) },
	}
}

// dispatchGitTrailer bypasses startup and legacy flag rewriting, including for
// flag-looking message paths and malformed hook invocations. Handled calls
// always return normally so an optional trailer cannot fail the Git commit.
func dispatchGitTrailer(args []string, stderr io.Writer) bool {
	if len(args) == 0 || args[0] != "git-trailer" {
		return false
	}
	runGitTrailer(args[1:], stderr)
	return true
}

func runGitTrailer(args []string, stderr io.Writer) {
	warn := func(err error) {
		if err != nil {
			fmt.Fprintln(stderr, "git-trailer: "+strings.Join(strings.Fields(err.Error()), " "))
		}
	}
	if len(args) < 1 || len(args) > 3 || args[0] == "" {
		warn(errors.New("expected <message-file> [<source>]; skipping trailer"))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), gittrailer.HookTimeout)
	defer cancel()
	cfg, err := config.LoadGitTrailer()
	if err != nil {
		warn(err)
		return
	}
	if !cfg.Enabled {
		return
	}
	session, err := gittrailer.Discover(os.Getenv, cfg.SessionIDEnv)
	if err != nil {
		warn(err)
		return
	}
	if session.ID == "" {
		return
	}
	if len(args) == 3 {
		warn(gittrailer.StampHook(ctx, args[0], args[1], session, args[2]))
		return
	}
	var source *string
	if len(args) == 2 {
		source = &args[1]
	}
	warn(gittrailer.Stamp(ctx, args[0], source, session))
}
