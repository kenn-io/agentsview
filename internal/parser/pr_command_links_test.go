package parser

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCommandPRLinks(t *testing.T) {
	remote := "https://github.com/example/repo.git"
	for _, tc := range []struct{ command, want string }{
		{`gh pr view 42`, "https://github.com/example/repo/pull/42"},
		{`cd repo && gh pr view 42`, ""},
		{`echo hi; gh pr view 42 -R example/other`, "https://github.com/example/other/pull/42"},
		{`echo hi | gh pr diff https://github.com/example/repo/pull/42`, "https://github.com/example/repo/pull/42"},
		{`gh pr view 42 --body 'a;b'`, "https://github.com/example/repo/pull/42"},
		{`gh pr view 42 --body "$VAR"`, ""},
		{`export GH_REPO=example/other; gh pr view 42`, ""},
		{`gh pr view $(echo 42) -R example/repo`, ""},
		{`gh pr create 42 -R example/repo`, ""},
		{`gh pr view branch -R example/repo`, ""},
	} {
		t.Run(tc.command, func(t *testing.T) {
			links, err := CommandPRLinks(t.Context(), tc.command, remote)
			require.NoError(t, err)
			if tc.want == "" {
				assert.Empty(t, links)
				return
			}
			require.Len(t, links, 1)
			assert.Equal(t, tc.want, links[0].URL)
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := CommandPRLinks(ctx, "gh pr view 42", remote)
	assert.ErrorIs(t, err, context.Canceled)
}
