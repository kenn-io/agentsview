package main

import (
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPGRawJobsRetryRequiresOneFilter(t *testing.T) {
	for _, args := range [][]string{{"retry"}, {"retry", "--all", "--error-class", "parse"}, {"retry", "--manifest", "abc", "--all"}} {
		t.Run(args[len(args)-1], func(t *testing.T) {
			cmd := newPGRawJobsCommand()
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SetArgs(args)
			require.ErrorContains(t, cmd.Execute(), "flags")
		})
	}
}
