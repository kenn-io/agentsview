package why

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

const firstSHA = "1111111111111111111111111111111111111111"
const secondSHA = "2222222222222222222222222222222222222222"

func TestParseRequest(t *testing.T) {
	for _, tc := range []struct {
		path, commit, rev string
		literal           bool
		start, end        int
		wantPath          string
		bad               bool
	}{
		{path: "x.go:41-71", start: 41, end: 71, wantPath: "x.go"},
		{path: "x.go:7", start: 7, end: 7, wantPath: "x.go"},
		{path: "docs/name:part.md", wantPath: "docs/name:part.md"},
		{path: "notes:12", literal: true, wantPath: "notes:12"},
		{path: "-file.go", wantPath: "-file.go"},
		{commit: "ABCDEF1"},
		{bad: true}, {path: "x", commit: "abcdef1", bad: true},
		{commit: "abcdef1", rev: "HEAD", bad: true}, {commit: "HEAD", bad: true},
		{path: "x:0", bad: true}, {path: "x:3-2", bad: true}, {path: "x:1-401", bad: true},
		{path: "x:1-", bad: true}, {path: "x:999999999999999999999", bad: true},
		{path: "x\x00.go", bad: true}, {path: "x\xff.go", bad: true},
	} {
		t.Run(fmt.Sprintf("%s/%s/%t", tc.path, tc.commit, tc.literal), func(t *testing.T) {
			r, err := ParseRequest(tc.path, tc.commit, tc.rev, tc.literal)
			if tc.bad {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantPath, r.Path)
			assert.Equal(t, tc.start, r.Start)
			assert.Equal(t, tc.end, r.End)
		})
	}
}

func TestParseBlame(t *testing.T) {
	raw := firstSHA + " 1 41 1\nauthor Example\nauthor-time 0\ncommitter-time 10\nboundary\nfilename x.go\n\tfirst\n" +
		strings.Repeat("0", 40) + " 2 42 1\nauthor Not Committed Yet\nauthor-time 99\ncommitter-time 99\nfilename x.go\n\tdirty\n"
	lines, err := parseBlame([]byte(raw))
	require.NoError(t, err)
	require.Len(t, lines, 2)
	require.NotNil(t, lines[0].SHA)
	assert.Equal(t, firstSHA, *lines[0].SHA)
	assert.Equal(t, 41, lines[0].N)
	assert.Equal(t, "first", lines[0].Text)
	assert.Equal(t, "1970-01-01T00:00:00Z", lines[0].At)
	assert.Equal(t, int64(10), lines[0].commitTime)
	assert.Nil(t, lines[1].SHA)
	assert.Empty(t, lines[1].At)
	assert.Empty(t, lines[1].Author)
	for _, raw := range []string{"garbage\n", firstSHA + " 1 1\n\ttext\n", firstSHA + " 1 1\nauthor A\nauthor-time bad\ncommitter-time 1\n\ttext\n", firstSHA + " 1 1\nauthor A\nauthor-time 1\ncommitter-time 1\n", firstSHA + " 1 0\n"} {
		_, err := parseBlame([]byte(raw))
		assert.Error(t, err, raw)
	}
}

func TestBlamePreservesAnnotatedLines(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		texts := hegel.Draw(ht, hegel.Lists(hegel.Text().ExcludeCharacters("\n\r").MaxSize(120)).MaxSize(400))
		dirty := hegel.Draw(ht, hegel.Booleans())
		sha := firstSHA
		if dirty {
			sha = strings.Repeat("0", 40)
		}
		var raw strings.Builder
		for i, text := range texts {
			fmt.Fprintf(&raw, "%s %d %d 1\nauthor Example\nauthor-time 0\ncommitter-time 10\nfilename x.go\n\t%s\n", sha, i+1, i+1, text)
		}
		lines, err := parseBlame([]byte(raw.String()))
		require.NoError(ht, err)
		require.Len(ht, lines, len(texts))
		for i, line := range lines {
			assert.Equal(ht, i+1, line.N)
			assert.Equal(ht, texts[i], line.Text)
			assert.Equal(ht, dirty, line.SHA == nil)
		}
	}, hegel.WithTestCases(2000))
}

func TestSelectionConservesCoverageAndUsesCommitterTime(t *testing.T) {
	var lines []Line
	for i := 0; i < 7; i++ {
		sha := fmt.Sprintf("%040x", i+1)
		lines = append(lines, Line{N: i + 1, SHA: &sha, At: "2099-01-01T00:00:00Z", commitTime: int64(i)})
	}
	lines = append(lines, Line{N: 8})
	commits, older := selectCommits(lines)
	require.Len(t, commits, 6)
	assert.Equal(t, 1, older)
	assert.Equal(t, fmt.Sprintf("%040x", 7), commits[0].SHA)
	assert.Equal(t, fmt.Sprintf("%040x", 2), commits[5].SHA)
}

func TestSelectionConservesGeneratedCoverage(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		groups := hegel.Draw(ht, hegel.Lists(hegel.Integers(0, 10)).MaxSize(400))
		lines := make([]Line, 0, len(groups))
		committed := 0
		for i, group := range groups {
			line := Line{N: i + 1}
			if group != 0 {
				sha := fmt.Sprintf("%040x", group)
				line.SHA = &sha
				committed++
			}
			lines = append(lines, line)
		}
		commits, older := selectCommits(lines)
		covered := older
		for _, c := range commits {
			covered += c.Covered
		}
		assert.Equal(ht, committed, covered)
		assert.LessOrEqual(ht, len(commits), 6)
	}, hegel.WithTestCases(2000))
}

func TestBlamePreservesCarriageReturns(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		text := hegel.Draw(ht, hegel.Text().ExcludeCharacters("\n").MaxSize(200)) + "\r"
		raw := firstSHA + " 1 1 1\nauthor Example\nauthor-time 0\ncommitter-time 0\n\t" + text + "\n"
		lines, err := parseBlame([]byte(raw))
		require.NoError(ht, err)
		require.Len(ht, lines, 1)
		assert.Equal(ht, text, lines[0].Text)
	}, hegel.WithTestCases(2000))
}
func TestBlameRejectsMalformedGroupCounts(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		count := hegel.Draw(ht, hegel.Text().Alphabet("abcdefghijklmnopqrstuvwxyz").MinSize(1).MaxSize(100))
		if hegel.Draw(ht, hegel.Booleans()) {
			count = fmt.Sprint(hegel.Draw(ht, hegel.Integers(math.MinInt, 0)))
		}
		_, err := parseBlame([]byte(firstSHA + " 1 1 " + count + "\nauthor Example\nauthor-time 0\ncommitter-time 0\n\ttext\n"))
		require.Error(ht, err)
	}, hegel.WithTestCases(2000))
}
