package commitlinks

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestSummaryPreservesGeneratedFields(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		branch := hegel.Draw(ht, hegel.Text().MinSize(1).ExcludeCharacters("\r\n"))
		sha := hegel.Draw(ht, hegel.Text().Alphabet("0123456789abcdef").MinSize(7).MaxSize(40))
		subject := hegel.Draw(ht, hegel.Text().ExcludeCharacters("\r\n"))
		got := ParseSummaries(fmt.Sprintf("[%s %s] %s\n", branch, sha, subject))
		require.Len(ht, got, 1)
		assert.Equal(ht, Summary{Branch: branch, SHA: sha, Subject: subject}, got[0])
	}, hegel.WithTestCases(2000))
}

func TestSummaryRejectsUnbracketedLines(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		line := hegel.Draw(ht, hegel.Text().ExcludeCharacters("\r\n["))
		assert.Empty(ht, ParseSummaries(line))
	}, hegel.WithTestCases(2000))
}

func TestTrailerDedupAndLineEndings(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		id := hegel.Draw(ht, hegel.UUIDs()).String()
		agent := hegel.Draw(ht, hegel.SampledFrom([]string{"claude", "codex", "custom-agent"}))
		ending := hegel.Draw(ht, hegel.SampledFrom([]string{"\n", "\r\n"}))
		line := "Agent-Session: " + agent + ":" + id
		want := id
		if agent != "claude" {
			want = agent + ":" + id
		}
		assert.Equal(ht, []string{want}, ParseTrailers(strings.Join([]string{"body", line, "more", line, ""}, ending)))
	}, hegel.WithTestCases(2000))
}

func TestCommandWordsInsideQuotesDoNotCreateCommits(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		text := hegel.Draw(ht, hegel.Text().ExcludeCharacters("'"))
		_, ok := CommitCommand("printf '%s' '"+text+" git commit -m fake'", "/repo")
		assert.False(ht, ok)
	}, hegel.WithTestCases(2000))
}

func TestExplicitGitTargetRemainsUnknown(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		repo := hegel.Draw(ht, hegel.Text().ExcludeCharacters("\x00").MinSize(1))
		quoted := "'" + strings.ReplaceAll("/"+repo, "'", "'\"'\"'") + "'"
		invalidator := hegel.Draw(ht, hegel.SampledFrom([]string{"GIT_DIR=/elsewhere ", "GIT_WORK_TREE=/elsewhere ", "export GIT_DIR=/elsewhere; ", "git-dir", "work-tree", "bare"}))
		command := "git -C " + quoted + " -C child commit -m fixture"
		if strings.HasSuffix(invalidator, " ") {
			command = invalidator + command
		} else {
			command = "git --" + invalidator
			if invalidator != "bare" {
				command += "=/elsewhere"
			}
			command += " -C " + quoted + " commit -m fixture"
		}
		got, ok := CommitCommand(command, "/initial")
		require.True(ht, ok)
		assert.Empty(ht, got.Repository)
	}, hegel.WithTestCases(2000))
}
