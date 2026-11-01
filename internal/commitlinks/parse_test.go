package commitlinks

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSummaries(t *testing.T) {
	got := ParseSummaries("noise\r\n[main (root-commit) abcdef1] Initial commit\r\n[detached HEAD 1234567] Unicode λ [ok]\n[feat/foo deadbee] Fix parser\n")
	require.Len(t, got, 3)
	assert.Equal(t, Summary{Branch: "main", SHA: "abcdef1", Subject: "Initial commit"}, got[0])
	assert.Equal(t, Summary{Branch: "detached HEAD", SHA: "1234567", Subject: "Unicode λ [ok]"}, got[1])
	assert.Equal(t, Summary{Branch: "feat/foo", SHA: "deadbee", Subject: "Fix parser"}, got[2])
	assert.Empty(t, ParseSummaries("echo [main abcdef1] fake\n[main abcde] too short\n[main zzzzzzz] not hex\n"))
}

func TestCommitCommand(t *testing.T) {
	for _, tc := range []struct {
		command string
		want    bool
		repo    string
	}{
		{`git commit -m 'Fix git commit detection'`, true, "/repo"},
		{`git commit -m '--dry-run'`, true, "/repo"},
		{`git() { echo fake; }; git commit`, false, ""},
		{`git add x && git -c core.quotePath=false commit --amend --no-edit`, true, "/repo"},
		{`git -C child -C nested commit -m fix`, true, "/repo/child/nested"},
		{`for f in a b; do git commit -am "$f"; done`, true, "/repo"},
		{`echo git commit`, false, ""},
		{`printf '%s' 'git commit'`, false, ""},
		{`git commit-tree abcdef1`, false, ""},
		{`git commit --dry-run; cat old.log`, false, ""},
		{"cat <<'EOF'\ngit commit -m fake\nEOF", false, ""},
		{`# git commit -m fake`, false, ""},
		{`git -C "$target" commit -m fix`, true, ""},
		{`cd other && git commit -m fix`, true, ""},
		{`git --git-dir=.other commit -m fix`, true, ""},
		{`git -C one commit -m one; git -C two commit -m two`, true, ""},
	} {
		t.Run(tc.command, func(t *testing.T) {
			got, ok := CommitCommand(tc.command, "/repo")
			assert.Equal(t, tc.want, ok)
			assert.Equal(t, tc.repo, got.Repository)
		})
	}
}

func TestParseTrailers(t *testing.T) {
	message := "Subject\r\nAgent-Session: claude:01234567-89ab-4cde-8fab-0123456789ab\r\nbody\nAgent-Session:\tcodex:12345678-89ab-4cde-8fab-0123456789ab \nAgent-Session: claude:01234567-89ab-4cde-8fab-0123456789ab\nAgent-Session: custom-agent:session_1\nAgent-Session: codex:not-a-uuid\nAgent-Session: Bad:id\n"
	assert.Equal(t, []string{"01234567-89ab-4cde-8fab-0123456789ab", "codex:12345678-89ab-4cde-8fab-0123456789ab", "custom-agent:session_1"}, ParseTrailers(message))
	assert.Empty(t, ParseTrailers("prefix Agent-Session: custom:id\nAgent-Session: custom:../path\n"))
}
