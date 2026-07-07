package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/service"
)

// parseMessagesFlags builds a `session messages` command, parses args
// against it, and returns the resulting filter/error from
// messagesFilterFromFlags without running the command's RunE (which would
// require a live service).
func parseMessagesFlags(t *testing.T, args []string) (service.MessageFilter, error) {
	t.Helper()
	cmd := newSessionMessagesCommand()
	require.NoError(t, cmd.ParseFlags(args))
	return messagesFilterFromFlags(cmd)
}

func TestSessionMessagesFlags_InvalidDirection(t *testing.T) {
	_, err := parseMessagesFlags(t, []string{"--direction", "backwards"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid --direction")
}

func TestSessionMessagesFlags_BeforeAfterRequireAround(t *testing.T) {
	_, err := parseMessagesFlags(t, []string{"--before", "2"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--around")

	_, err = parseMessagesFlags(t, []string{"--after", "2"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--around")
}

// TestSessionMessagesFlags_AroundOnlyOmitsDirectionAndFrom is the CRITICAL
// flag-default-gotcha regression test: --direction defaults to "asc" and
// --from defaults to 0, so a naive implementation would always forward
// them, tripping the service's around-vs-direction/from mutual-exclusion
// check on every plain `--around N` call. With only --around set, the
// built filter must leave Direction empty and From nil.
func TestSessionMessagesFlags_AroundOnlyOmitsDirectionAndFrom(t *testing.T) {
	filter, err := parseMessagesFlags(t, []string{"--around", "5"})
	require.NoError(t, err)
	require.NotNil(t, filter.Around)
	assert.Equal(t, 5, *filter.Around)
	assert.Empty(t, filter.Direction,
		"Direction must stay empty when --direction was never set, "+
			"even though the flag default is asc")
	assert.Nil(t, filter.From,
		"From must stay nil when --from was never set")
	assert.Nil(t, filter.Before, "Before must stay nil when --before was never set")
	assert.Nil(t, filter.After, "After must stay nil when --after was never set")
}

// TestSessionMessagesFlags_AroundWithExplicitFromForwardsIt verifies that
// an explicit --from alongside --around is still forwarded to the
// service (whose mutual-exclusion error then surfaces), rather than
// silently dropped.
func TestSessionMessagesFlags_AroundWithExplicitFromForwardsIt(t *testing.T) {
	filter, err := parseMessagesFlags(t, []string{"--around", "5", "--from", "1"})
	require.NoError(t, err)
	require.NotNil(t, filter.From)
	assert.Equal(t, 1, *filter.From)
}

// TestSessionMessagesFlags_AroundWithExplicitDirectionForwardsIt mirrors
// the From case for --direction.
func TestSessionMessagesFlags_AroundWithExplicitDirectionForwardsIt(t *testing.T) {
	filter, err := parseMessagesFlags(t, []string{"--around", "5", "--direction", "desc"})
	require.NoError(t, err)
	assert.Equal(t, "desc", filter.Direction)
}

func TestSessionMessagesFlags_AroundWithBeforeAfter(t *testing.T) {
	filter, err := parseMessagesFlags(t, []string{
		"--around", "5", "--before", "2", "--after", "3",
	})
	require.NoError(t, err)
	require.NotNil(t, filter.Before)
	require.NotNil(t, filter.After)
	assert.Equal(t, 2, *filter.Before)
	assert.Equal(t, 3, *filter.After)
}

func TestSessionMessagesFlags_RoleSplitsOnComma(t *testing.T) {
	filter, err := parseMessagesFlags(t, []string{"--role", "user,assistant"})
	require.NoError(t, err)
	assert.Equal(t, []string{"user", "assistant"}, filter.Roles)
}

// TestSessionMessagesFlags_RoleTrimsSpacesAndDropsEmpty covers a trailing
// comma or stray whitespace (e.g. "user, " or "user,") which must not
// narrow the filter with a spurious "" role that matches nothing.
func TestSessionMessagesFlags_RoleTrimsSpacesAndDropsEmpty(t *testing.T) {
	filter, err := parseMessagesFlags(t, []string{"--role", "user, assistant, "})
	require.NoError(t, err)
	assert.Equal(t, []string{"user", "assistant"}, filter.Roles)
}

// TestSessionMessagesAroundNoOtherFlagsSucceeds is the brief-mandated
// end-to-end check: `session messages <id> --around 5` with no other flags
// must actually succeed when run as a full CLI command (through
// resolveService's normal local-SQLite discovery), not just build a filter
// that looks right in isolation. This is the regression test for the
// CRITICAL flag-default gotcha: the command previously always forwarded
// Direction (flag default "asc"), which would have tripped the
// around-vs-direction validation on every default `--around` call.
func TestSessionMessagesAroundNoOtherFlagsSucceeds(t *testing.T) {
	dataDir := newAgentDataDir(t)
	seedSession(t, dataDir, "s-around", "proj")
	seedMessages(t, dataDir, "s-around", 12) // ordinals 1..12

	out, err := executeCommand(newRootCommand(),
		"session", "messages", "s-around", "--around", "5", "--format", "json")
	require.NoError(t, err, "--around 5 with no other flags must succeed")

	got := decodeCLIJSON[cliMessageList](t, out)
	// Ordinals start at 1 (seedMessages convention): only 4 messages exist
	// below ordinal 5, so before-window is capped at 4 even though the
	// default asks for 5; after-window gets the full 5 (6..10).
	assert.Equal(t, 10, got.Count,
		"before is capped at 4 available messages; after takes the full 5")
	assert.Equal(t, float64(1), got.Messages[0]["ordinal"])
	assert.Equal(t, float64(10), got.Messages[len(got.Messages)-1]["ordinal"])
}
