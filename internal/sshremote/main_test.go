package sshremote

import (
	"fmt"
	"os"
	"testing"
)

// testChildEnv makes the test binary act as the remote agentsview, so tests
// exercise the real process code without building cmd/agentsview.
const testChildEnv = "AGENTSVIEW_SSHREMOTE_TEST_CHILD"

const testChildVersion = "test-child"

func TestMain(m *testing.M) {
	if os.Getenv(testChildEnv) == "1" {
		os.Exit(childMain(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func childMain(args []string) int {
	switch {
	case len(args) == 2 && args[0] == "remote-sync" && args[1] == "probe":
		if err := WriteProbe(os.Stdout, testChildVersion); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	case len(args) == 4 && args[0] == "remote-sync" && args[1] == "serve" && args[2] == "--op":
		op, err := ParseOp(args[3])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return RunServeProcess(op, os.Stdin, os.Stderr)
	default:
		fmt.Fprintf(os.Stderr, "unexpected child args %q\n", args)
		return 1
	}
}
