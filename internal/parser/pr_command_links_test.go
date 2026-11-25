package parser

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCommandPRLinks(t *testing.T) {
	for _, tc := range []struct {
		command, want string
	}{
		{`bash -c 'export GH_HOST=git.example'; gh pr view 42 -R example/repo`, ""},
		{`export GH_HOST=$HOST; gh pr view 42 -R example/repo`, ""},
		{`setHost() { export GH_HOST=git.example; }; setHost; gh pr view 42 -R example/repo`, ""},
		{`command gh pr view 41 -R example/repo`, "https://github.com/example/repo/pull/41"},
		{`gh -R example/repo pr view 42`, "https://github.com/example/repo/pull/42"},
		{`bash -lc 'gh pr view 44 -R example/repo'`, "https://github.com/example/repo/pull/44"},
		{`timeout 60 gh pr checks 45 -R example/repo --watch`, "https://github.com/example/repo/pull/45"},
		{`env GH_HOST=other.example gh pr view 42 -R example/repo`, ""},
		{`env GH_HOST=git.example gh pr view https://github.com/example/repo/pull/42`, "https://github.com/example/repo/pull/42"},
		{`env GH_HOST=git.example bash -c 'gh pr view 42 -R example/repo'`, ""},
		{`bash -n -c 'gh pr view 42 -R example/repo'`, ""},
		{`command -v gh pr view 42 -R example/repo`, ""},
		{`GH_HOST=git.example command gh pr view 42 -R example/repo`, ""},
		{`bash -c 'export GH_HOST=git.example; gh pr view 42 -R example/repo'`, ""},
		{`gh pr merge 12 -R example/repo --squash`, "https://github.com/example/repo/pull/12"},
		{`gh pr comment https://github.com/example/repo/pull/42 --body 'hello'`, "https://github.com/example/repo/pull/42"},
		{`gh pr comment --body "$BODY" 42 -R example/repo`, "https://github.com/example/repo/pull/42"},
		{`gh pr comment 42 -R example/repo --body "$BODY"`, "https://github.com/example/repo/pull/42"},
		{`gh pr comment --body="$BODY" 42 -R example/repo`, "https://github.com/example/repo/pull/42"},
		{`gh pr view 42 -R example/repo --future value`, "https://github.com/example/repo/pull/42"},
		{`gh pr view 42 --future -R example/repo`, ""},
		{`gh pr comment --body 42 -R example/repo`, ""},
		{`gh pr comment --body -R example/repo 42`, ""},
		{`gh pr view --json=x 42 -R example/repo`, "https://github.com/example/repo/pull/42"},
		{`gh pr view --unknown 42 -R example/repo`, ""},
		{`gh pr view 42 -R example/repo $OPTIONS`, ""},
		{`gh pr comment 42 -R example/repo --body=$OPTIONS`, ""},
		{`gh pr comment 42 -R example/repo -b$OPTIONS`, ""},
		{`gh pr comment 42 -R example/repo --body $OPTIONS`, ""},
		{`gh pr comment 42 -R example/repo --body "$@"`, ""},
		{`gh pr comment 42 -R example/repo --body "${OPTIONS[@]}"`, ""},
		{`gh pr view 42 -R example/repo "--repo=$OTHER"`, ""},
		{`gh pr view 42 -R example/repo '-R'"$OTHER"`, ""},
		{`gh pr view -R example/repo --repo="$OTHER" 42`, ""},
		{`gh pr co 42 -R example/repo`, "https://github.com/example/repo/pull/42"},
		{`gh pr view '#42' -R example/repo`, "https://github.com/example/repo/pull/42"},
		{`gh pr update-branch 42 -R example/repo`, "https://github.com/example/repo/pull/42"},
		{`gh pr lock 42 -R example/repo`, "https://github.com/example/repo/pull/42"},
		{`gh pr unlock 42 -R example/repo`, "https://github.com/example/repo/pull/42"},
		{`gh pr revert 42 -R example/repo`, "https://github.com/example/repo/pull/42"},
		{`review() { gh pr view 42 -R github.com/example/repo; }; review`, ""},
		{`review() { echo ready; }; review; gh pr view 42 -R github.com/example/repo`, "https://github.com/example/repo/pull/42"},
		{`echo hi | gh pr diff https://github.com/example/repo/pull/42`, "https://github.com/example/repo/pull/42"},
		{`gh pr view $(echo 42) -R example/repo`, ""},
		{`gh pr create 42 -R example/repo`, ""},
		{`gh pr view branch -R example/repo`, ""},
		{`cd /work/other; gh pr view 45 -R example/repo`, "https://github.com/example/repo/pull/45"},
		{`gh pr view 42 -R=github.com/example/repo`, "https://github.com/example/repo/pull/42"},
		{`gh pr view 42 -R git.example/example/repo`, "https://git.example/example/repo/pull/42"},
		{`GH_HOST=$HOST gh pr view 42 -R github.com/example/repo`, "https://github.com/example/repo/pull/42"},
		{`gh pr view -R example/repo 42`, "https://github.com/example/repo/pull/42"},
		{`gh pr view 42 -R example/repo; export GH_HOST=git.example`, ""},
		{`GH_HOST=git.example gh pr view 42 -R example/repo`, ""},
		{`GH_HOST=git.example gh pr view 41 -R example/repo; gh pr view 42 -R example/repo`, ""},
		{`env GH_PAGER=cat gh pr view 42 -R example/repo`, "https://github.com/example/repo/pull/42"},
	} {
		t.Run(tc.command, func(t *testing.T) {
			links, err := AnalyzePRCommand(t.Context(), tc.command)
			require.NoError(t, err)
			if tc.want == "" {
				assert.Empty(t, links)
				return
			}
			require.Len(t, links, 1)
			assert.Equal(t, tc.want, links[0].URL)
		})
	}
	t.Run("branches", func(t *testing.T) {
		branches, err := AnalyzePRCommand(t.Context(), `if true; then gh pr view 45 -R example/repo; else gh pr view 46 -R example/repo; fi`)
		require.NoError(t, err)
		require.Len(t, branches, 2)
		assert.Equal(t, 45, branches[0].Number)
		assert.Equal(t, 46, branches[1].Number)
	})
	t.Run("canceled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := AnalyzePRCommand(ctx, "gh pr view 42")
		assert.ErrorIs(t, err, context.Canceled)
	})
}
