package why

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestNamedReferences(t *testing.T) {
	exists := func(path string) (bool, error) { return path == "docs/guide.md" || path == "README.md", nil }
	refs, err := FindNamedRefs([]string{"this code test 2026 says (project#abcd), #1234 and #abcd. `docs/guide.md` [read](README.md). #1234"}, exists)
	require.NoError(t, err)
	assert.Equal(t, []NamedRef{{Key: "issue:project#abcd", Issue: "project#abcd"}, {Key: "issue:#1234", Issue: "#1234"}, {Key: "issue:#abcd", Issue: "#abcd"}, {Key: "path:docs/guide.md", Path: "docs/guide.md"}, {Key: "path:README.md", Path: "README.md"}}, refs)
	for _, text := range []string{"this code test 2026", "https://example.invalid/docs/guide.md", "/docs/guide.md", "../docs/guide.md", "a/../../docs/guide.md", "C:/docs/guide.md", "prefixproject#abcdef", "word#1234extra", "docs/missing.md"} {
		refs, err := FindNamedRefs([]string{text}, exists)
		require.NoError(t, err)
		assert.Empty(t, refs, text)
	}
}

func TestNamedRefsRecognizeColonBeforeIssueWithoutSplittingPaths(t *testing.T) {
	calls := 0
	refs, err := FindNamedRefs([]string{"Issue:#123", "README:docs/guide.md"}, func(path string) (bool, error) {
		calls++
		return path == "docs/guide.md", nil
	})
	require.NoError(t, err)
	assert.Equal(t, []NamedRef{{Key: "issue:#123", Issue: "#123"}}, refs)
	assert.Zero(t, calls)
}

func TestNamedReferenceBudgetsAndFailure(t *testing.T) {
	var text strings.Builder
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&text, "#%d docs/file%d.md ", i+1, i)
	}
	checks := 0
	refs, err := FindNamedRefs([]string{text.String()}, func(string) (bool, error) { checks++; return false, nil })
	require.NoError(t, err)
	assert.Len(t, refs, 5)
	assert.Equal(t, 64, checks)
	checks = 0
	refs, err = FindNamedRefs([]string{text.String()}, func(string) (bool, error) { checks++; return true, nil })
	require.NoError(t, err)
	assert.Len(t, refs, 13)
	assert.Equal(t, 8, checks)
	sentinel := errors.New("tree unavailable")
	_, err = FindNamedRefs([]string{"docs/file.md"}, func(string) (bool, error) { return false, sentinel })
	assert.ErrorIs(t, err, sentinel)
}
func TestNamedRefsOriginateInGeneratedText(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		text := hegel.Draw(ht, hegel.Text().MaxSize(1500))
		refs, err := FindNamedRefs([]string{text}, func(string) (bool, error) { return true, nil })
		require.NoError(ht, err)
		seen := map[string]bool{}
		issues, paths := 0, 0
		for _, ref := range refs {
			assert.False(ht, seen[ref.Key])
			seen[ref.Key] = true
			if ref.Issue != "" {
				issues++
				assert.Contains(ht, text, ref.Issue)
			} else {
				paths++
				assert.Contains(ht, text, ref.Path)
			}
		}
		assert.LessOrEqual(ht, issues, 5)
		assert.LessOrEqual(ht, paths, 8)
	}, hegel.WithTestCases(2000))
}
func TestNamedRefsRejectUnsafePrefixes(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		name := hegel.Draw(ht, hegel.Text().Alphabet("abcdefghijklmnopqrstuvwxyz0123456789_-.").MinSize(1).MaxSize(80))
		prefix := hegel.Draw(ht, hegel.SampledFrom([]string{"/", "../", "a/../../", "https://example.invalid/", "C:/"}))
		checks := 0
		refs, err := FindNamedRefs([]string{prefix + "docs/" + name + ".md"}, func(string) (bool, error) { checks++; return true, nil })
		require.NoError(ht, err)
		assert.Empty(ht, refs)
		assert.Zero(ht, checks)
	}, hegel.WithTestCases(2000))
}

func TestNamedRefsAcceptGeneratedMarkedReferences(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		id := hegel.Draw(ht, hegel.Text().Alphabet("abcdefghijklmnopqrstuvwxyz0123456789").MinSize(4).MaxSize(4))
		project := hegel.Draw(ht, hegel.Text().Alphabet("abcdefghijklmnopqrstuvwxyz").MinSize(1).MaxSize(40))
		issue := project + "#" + id
		refs, err := FindNamedRefs([]string{"Fix (" + issue + ")."}, func(string) (bool, error) { ht.Fatal("issue lookup must not read a document"); return false, nil })
		require.NoError(ht, err)
		assert.Equal(ht, []NamedRef{{Key: "issue:" + issue, Issue: issue}}, refs)
	}, hegel.WithTestCases(2000))
}

func TestNamedRefsDoNotSplitUnsafeWrappedPaths(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		prefix := hegel.Draw(ht, hegel.SampledFrom([]string{"/tmp/", "../", "a/../../", "https://host/", "C:/"}))
		name := hegel.Draw(ht, hegel.Text().Alphabet("abcdefghijklmnopqrstuvwxyz").MinSize(1).MaxSize(40)) + ".md"
		refs, err := FindNamedRefs([]string{prefix + "(" + name + ")"}, func(string) (bool, error) { return true, nil })
		require.NoError(ht, err)
		assert.Empty(ht, refs)
	}, hegel.WithTestCases(2000))
}
func TestNamedRefsAcceptMarkdownWrappers(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		name := hegel.Draw(ht, hegel.Text().Alphabet("abcdefghijklmnopqrstuvwxyz").MinSize(1).MaxSize(40)) + ".md"
		wrap := hegel.Draw(ht, hegel.SampledFrom([]string{"**", "*", "`", "\""}))
		refs, err := FindNamedRefs([]string{wrap + name + wrap}, func(string) (bool, error) { return true, nil })
		require.NoError(ht, err)
		assert.Equal(ht, []NamedRef{{Key: "path:" + name, Path: name}}, refs)
	}, hegel.WithTestCases(2000))
}
