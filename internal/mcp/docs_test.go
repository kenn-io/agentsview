package mcp

import (
	"testing"
	"testing/fstest"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSplitDocSections_FrontMatterHeadingsAndFences(t *testing.T) {
	t.Parallel()
	text := "---\r\ntitle: \"Guide Title\"\r\ndescription: x\r\n---\r\n" +
		"Intro text.\r\n\r\n## Setup\r\nRun it.\r\n```bash\r\n# not a heading\r\n```\r\n#hashtag stays\r\n### Limits\r\nNone.\r\n"
	sections := splitDocSections("guide", text)
	require.Len(t, sections, 3)
	assert.Equal(t, "Guide Title", sections[0].title)
	assert.Equal(t, "Guide Title", sections[0].heading)
	assert.Equal(t, "Intro text.", sections[0].body)
	assert.Equal(t, "Setup", sections[1].heading)
	assert.Contains(t, sections[1].body, "# not a heading")
	assert.Contains(t, sections[1].body, "#hashtag stays")
	assert.Equal(t, "Limits", sections[2].heading)
}

func TestFindDocs_RanksTruncatesAndLinks(t *testing.T) {
	t.Parallel()
	sections, err := loadDocSections(fstest.MapFS{
		"index.md": {Data: []byte("---\ntitle: Docs Home\n---\nStart with the token report.\n")},
		"usage.md": {Data: []byte("---\ntitle: Usage\n---\nOverview mentions token once.\n## Token report\nThe token report lists cost per model.\n")},
	})
	require.NoError(t, err)

	results, err := findDocs(sections, searchDocsIn{Query: "Token REPORT"})
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, "Token report", results[0].Section, "heading match ranks first")
	assert.Equal(t, "https://agentsview.io/docs/usage/", results[0].URL)
	assert.Equal(t, "https://agentsview.io/docs/", results[1].URL)

	short, err := findDocs(sections, searchDocsIn{Query: "cost", MaxChars: 10})
	require.NoError(t, err)
	require.Len(t, short, 1)
	assert.True(t, short[0].Truncated)
	assert.Equal(t, 10, utf8.RuneCountInString(short[0].Text))

	_, err = findDocs(sections, searchDocsIn{Query: "token", Topic: "README"})
	require.ErrorContains(t, err, "unknown documentation topic")
	_, err = findDocs(sections, searchDocsIn{Query: "   "})
	require.Error(t, err)
}

func TestFindDocs_SearchesEmbeddedGuides(t *testing.T) {
	t.Parallel()
	results, err := findDocs(docSections, searchDocsIn{Query: "StreamableHTTP", Topic: "mcp", Limit: 1})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "MCP Server", results[0].Title)
	assert.Equal(t, "https://agentsview.io/docs/mcp/", results[0].URL)
	for _, s := range docSections {
		assert.NotEqual(t, "README", s.topic, "maintainer guide must not be searchable")
		assert.NotEqual(t, "changelog", s.topic)
	}
}
