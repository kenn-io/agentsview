package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOutputFormat_Resolves(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"default is human", nil, "human"},
		{"json alias", []string{"--json"}, "json"},
		{"format json", []string{"--format", "json"}, "json"},
		{"format human", []string{"--format", "human"}, "human"},
		{"json wins over format human", []string{"--json", "--format", "human"}, "json"},
		{"explicit json false defers to format", []string{"--json=false", "--format", "json"}, "json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := &cobra.Command{Use: "x"}
			registerFormatFlags(cmd.Flags())
			require.NoError(t, cmd.ParseFlags(tt.args))
			assert.Equal(t, tt.want, outputFormat(cmd))
		})
	}
}

func TestOutputFormat_RejectsInvalid(t *testing.T) {
	t.Parallel()
	cmd := &cobra.Command{Use: "x"}
	registerFormatFlags(cmd.Flags())
	err := cmd.ParseFlags([]string{"--format", "yaml"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be human or json")
}

// machineOutputCommandPaths are the commands that must accept both
// --format and the --json alias. token-use (deprecated, JSON-only) and
// openapi (spec-only) are deliberately excluded.
var machineOutputCommandPaths = [][]string{
	{"projects"},
	{"health"},
	{"usage", "daily"},
	{"activity", "report"},
	{"stats"},
	{"secrets", "list"},
	{"secrets", "scan"},
	{"export", "sessions"},
	{"parse-diff"},
	{"session", "list"},
	{"session", "get"},
	{"session", "messages"},
	{"session", "tool-calls"},
	{"session", "search"},
	{"session", "usage"},
	{"session", "sync"},
}

func TestMachineOutputCommands_AcceptFormatAndJSON(t *testing.T) {
	t.Parallel()
	for _, path := range machineOutputCommandPaths {
		t.Run(strings.Join(path, " "), func(t *testing.T) {
			t.Parallel()
			for _, args := range [][]string{{"--json"}, {"--format", "json"}} {
				root := newRootCommand()
				cmd, _, err := root.Find(path)
				require.NoError(t, err, "command %q should exist", path)
				require.NoError(t, cmd.ParseFlags(args),
					"command %q should accept %v", path, args)
				assert.Equal(t, "json", outputFormat(cmd),
					"command %q with %v should resolve json", path, args)
			}
		})
	}
}

// TestFormatAndJSONFlagsArePaired enforces that --format and --json are
// always registered together, so a reintroduced bare --json fails the
// build. It does not prove a JSON command opted in at all;
// machineOutputCommandPaths covers that.
func TestFormatAndJSONFlagsArePaired(t *testing.T) {
	t.Parallel()
	var walk func(cmd *cobra.Command)
	walk = func(cmd *cobra.Command) {
		// cmd.Flag resolves local and inherited persistent flags.
		hasFormat := cmd.Flag("format") != nil
		hasJSON := cmd.Flag("json") != nil
		assert.Equalf(t, hasFormat, hasJSON,
			"command %q must register --format and --json together "+
				"(has --format=%v, has --json=%v)",
			cmd.CommandPath(), hasFormat, hasJSON)
		for _, sub := range cmd.Commands() {
			walk(sub)
		}
	}
	walk(newRootCommand())
}
