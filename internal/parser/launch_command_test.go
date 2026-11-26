package parser

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLaunchCommands(t *testing.T) {
	for _, tc := range []struct {
		command string
		want    []LaunchCommand
	}{
		{`codex exec "$PROMPT"`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}}}},
		{`codex -m "$MODEL" exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}}}},
		{`codex --model="$MODEL" exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}}}},
		{`claude --model "$MODEL" -p hi`, []LaunchCommand{{Agent: "claude", Dirs: []string{"/repo"}}}},
		{`codex -C "$DIR" exec hi`, []LaunchCommand{{Agent: "codex", Unknown: true}}},
		{`codex -C /Users/example/project exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/Users/example/project"}}}},
		{`codex "$MODE"`, []LaunchCommand{{Agent: "codex", Unknown: true}}},
		{`sudo "$BIN" exec`, []LaunchCommand{{Agent: "codex", Unknown: true}, {Agent: "claude", Unknown: true}}},
		{`env "$BIN" -p`, []LaunchCommand{{Agent: "codex", Unknown: true}, {Agent: "claude", Unknown: true}}},
		{`cd /other && codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/other"}}}},
		{`cd /other; codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo", "/other"}}}},
		{`cd /other & codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}}}},
		{`pushd /other; codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}, Unknown: true}}},
		{`popd; codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}, Unknown: true}}},
		{`eval 'cd /other'; codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}, Unknown: true}}},
		{`source setup.sh; codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}, Unknown: true}}},
		{`. setup.sh; codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}, Unknown: true}}},
		{`eval 'exit'; codex -C /repo exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}}}},
		{`source setup.sh; codex -C /repo exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}}}},
		{`while test -d x; do exit; done; codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}}}},
		{`while test -d x; do cd /other; done; codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}, Unknown: true}}},
		{`{ cd /other; }; codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}, Unknown: true}}},
		{`(cd /other); codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}}}},
		{`exec true; codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}}}},
		{`(exit); codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}}}},
		{`echo $(exit); codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}}}},
		{`if test -d /x; then echo hi; fi; codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}}}},
		{`if test -d /x; then if test -d /y; then exit; fi; fi; codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}}}},
		{`test -d x && codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}, Possible: true}}},
		{`false && codex exec hi`, nil},
		{`exit; codex exec hi`, nil},
		{`true && exit; codex exec hi`, nil},
		{`if true; then exit; fi; codex exec hi`, nil},
		{`exit && codex exec hi`, nil},
		{`if exit; then codex exec hi; fi`, nil},
		{`if test -d x; then test -d y && exit; fi; codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}, Possible: true}}},
		{`cd "$DIR" && codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}, Unknown: true, Possible: true}}},
		{`codex --cd="$DIR" exec hi`, []LaunchCommand{{Agent: "codex", Unknown: true}}},
		{`cd /other || codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo", "/other"}, Possible: true}}},
		{`cd; codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}, Unknown: true}}},
		{`cd -; codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}, Unknown: true}}},
		{`cd ~/repo; codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}, Unknown: true}}},
		{`test -d /repo && cd /repo && codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}, Possible: true}}},
		{`cd /repo && codex -C /other exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/other"}, Possible: true}}},
		{`test -d x && echo ok && codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}, Possible: true}}},
		{`test -d x && echo ok; codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}}}},
		{`echo 'codex exec'`, nil},
		{`codex resume "$ID"`, nil},
		{`bash -lc 'codex exec hi'`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}}}},
		{`powershell.exe -Command 'codex exec hi'`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}}}},
		{`powershell -Command '& codex exec hi'`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}}}},
		{`pwsh -NoProfile -Command 'claude -p hi'`, []LaunchCommand{{Agent: "claude", Dirs: []string{"/repo"}}}},
		{`cmd.exe /c codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}}}},
		{`cmd.exe /c "codex exec hi"`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}}}},
		{`echo $(codex exec hi)`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}}}},
		{`env echo "$TEXT"`, nil},
		{`bash -c "$COMMAND"`, []LaunchCommand{{Agent: "codex", Unknown: true}, {Agent: "claude", Unknown: true}}},
		{`cd /other && cd nested && codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/other/nested"}}}},
		{`timeout 600 codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}}}},
		{`nohup codex exec hi > log &`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}}}},
		{`out=$(cd /other && codex exec hi); codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/other"}}, {Agent: "codex", Dirs: []string{"/repo"}}}},
		{`codex exec hi 2>&1 | tail -20`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}}}},
		{`cat p.md | codex exec -`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}}}},
		{`export X=1 && codex exec hi`, []LaunchCommand{{Agent: "codex", Dirs: []string{"/repo"}}}},
		{`grep codex exec`, nil},
	} {
		t.Run(tc.command, func(t *testing.T) { assert.Equal(t, tc.want, LaunchesFromCommand(tc.command, "/repo")) })
	}
}
