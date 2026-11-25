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
		{`codex exec "$PROMPT"`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}, Reach: Reached}}},
		{`claude -p "$PROMPT"`, []LaunchCommand{{Agent: "claude", Dirs: []string{"/repo"}, Reach: Reached}}},
		{`codex "$MODE"`, []LaunchCommand{{Agent: "codex", Unknown: true, Reach: Possible}}},
		{`sudo "$BIN" exec`, []LaunchCommand{{Agent: "codex", Unknown: true, Reach: Possible}, {Agent: "claude", Unknown: true, Reach: Possible}}},
		{`env "$BIN" -p`, []LaunchCommand{{Agent: "codex", Unknown: true, Reach: Possible}, {Agent: "claude", Unknown: true, Reach: Possible}}},
		{`cd /other && codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/other"}, Reach: Possible, CDGuarded: true}}},
		{`false && codex exec hi`, nil},
		{`true || codex -C /repo exec hi`, nil},
		{`false || codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}, Reach: Reached}}},
		{`true && codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}, Reach: Reached}}},
		{`cd /other; codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo", "/other"}, Reach: Reached}}},
		{`cd /other & codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}, Reach: Reached}}},
		{`pushd /other; codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}, Unknown: true, Reach: Reached}}},
		{`popd; codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}, Unknown: true, Reach: Reached}}},
		{`eval 'cd /other'; codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}, Unknown: true, Reach: Possible}}},
		{`source setup.sh; codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}, Unknown: true, Reach: Possible}}},
		{`. setup.sh; codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}, Unknown: true, Reach: Possible}}},
		{`eval 'exit'; codex -C /repo exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}, Reach: Possible}}},
		{`source setup.sh; codex -C /repo exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}, Reach: Possible}}},
		{`. setup.sh; codex -C /repo exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}, Reach: Possible}}},
		{`exit; codex exec hi`, nil},
		{`while test -d x; do exit; done; codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}, Reach: Possible}}},
		{`while test -d x; do cd /other; done; codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}, Unknown: true, Reach: Reached}}},
		{`{ cd /other; }; codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}, Unknown: true, Reach: Reached}}},
		{`(cd /other); codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}, Reach: Reached}}},
		{`exec true; codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}, Reach: Possible}}},
		{`(exit); codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}, Reach: Reached}}},
		{`echo $(exit); codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}, Reach: Reached}}},
		{`if false; then codex exec hi; fi`, nil},
		{`if ! true; then codex exec hi; fi`, nil},
		{`if ! false; then codex exec hi; fi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}, Reach: Reached}}},
		{`if true; then codex exec hi; fi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}, Reach: Reached}}},
		{`if test -d /x; then echo hi; fi; codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}, Reach: Reached}}},
		{`if test -d /x; then if test -d /y; then exit; fi; fi; codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}, Reach: Possible}}},
		{`test -d x && codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}, Reach: Possible}}},
		{`echo 'codex exec'`, nil},
		{`git commit -m 'codex notes'`, nil},
		{`codex resume "$ID"`, nil},
		{`bash -lc 'codex exec hi'`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}, Reach: Reached}}},
		{`echo $(codex exec hi)`, []LaunchCommand{{Agent: "codex", Unknown: true, Reach: Possible}}},
		{`env echo "$TEXT"`, nil},
		{`bash -c "$COMMAND"`, []LaunchCommand{{Agent: "codex", Unknown: true, Reach: Possible}, {Agent: "claude", Unknown: true, Reach: Possible}}},
		{`grep codex exec`, nil},
	} {
		t.Run(tc.command, func(t *testing.T) { assert.Equal(t, tc.want, LaunchesFromCommand(tc.command, "/repo")) })
	}
}
