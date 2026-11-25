package parser

import (
	"github.com/stretchr/testify/assert"
	"testing"
)

func TestLaunchCommands(t *testing.T) {
	for _, tc := range []struct {
		command string
		want    []LaunchCommand
	}{
		{`codex exec "$PROMPT"`, []LaunchCommand{{Agent: "codex", Dir: "/repo", Certain: true}}},
		{`claude -p "$PROMPT"`, []LaunchCommand{{Agent: "claude", Dir: "/repo", Certain: true}}},
		{`codex "$MODE"`, []LaunchCommand{{Agent: "codex"}}},
		{`sudo "$BIN" exec`, []LaunchCommand{{Agent: "codex"}, {Agent: "claude"}}},
		{`env "$BIN" -p`, []LaunchCommand{{Agent: "codex"}, {Agent: "claude"}}},
		{`cd /other && codex exec hi`, []LaunchCommand{{Agent: "codex", Dir: "/other", Certain: true}}},
		{`false && codex exec hi`, []LaunchCommand{{Agent: "codex", Dir: "/repo"}}},
		{`cd /other; codex exec hi`, []LaunchCommand{{Agent: "codex", Dir: "", Certain: true}}},
		{`cd /other || codex exec hi`, []LaunchCommand{{Agent: "codex", Dir: "", Certain: true}}},
		{`cd /other & codex exec hi`, []LaunchCommand{{Agent: "codex", Dir: "/repo", Certain: true}}},
		{`echo 'codex exec'`, nil},
		{`git commit -m 'codex notes'`, nil},
		{`codex resume foo`, nil},
		{`codex resume "$ID"`, nil},
		{`codex -m "$MODEL" exec hi`, []LaunchCommand{{Agent: "codex", Dir: "/repo"}}},
		{`"$CMD" "$MODE"`, []LaunchCommand{{Agent: "codex"}, {Agent: "claude"}}},
		{`while true; do codex exec hi; done`, []LaunchCommand{{Agent: "codex"}}},
		{`echo 'codex exec' # claude -p`, nil},
		{`codex -m x exec hi`, []LaunchCommand{{Agent: "codex", Dir: "/repo", Certain: true}}},
		{`codex -C "$DIR" exec hi`, []LaunchCommand{{Agent: "codex", Certain: true}}},
		{`codex --cd="$DIR" exec hi`, []LaunchCommand{{Agent: "codex", Certain: true}}},
		{`bash -lc 'codex exec hi'`, []LaunchCommand{{Agent: "codex", Dir: "/repo", Certain: true}}},
		{`if true; then codex exec hi; fi`, []LaunchCommand{{Agent: "codex"}}},
		{`if true; then cd /repo && codex exec hi; fi`, []LaunchCommand{{Agent: "codex"}}},
		{`echo $(codex exec hi)`, []LaunchCommand{{Agent: "codex"}}},
		{`sudo codex exec hi`, []LaunchCommand{{Agent: "codex"}}},
		{`env echo "$TEXT"`, nil},
		{`bash -c "$COMMAND"`, []LaunchCommand{{Agent: "codex"}, {Agent: "claude"}}},
		{`eval "$COMMAND"`, []LaunchCommand{{Agent: "codex"}, {Agent: "claude"}}},
		{`eval 'codex exec hi'`, []LaunchCommand{{Agent: "codex"}}},
		{`grep codex exec`, nil},
	} {
		t.Run(tc.command, func(t *testing.T) { assert.Equal(t, tc.want, LaunchesFromCommand(tc.command, "/repo")) })
	}
}
