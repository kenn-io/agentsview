package parser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// geminiAppsCellHTML renders one My Activity cell in the layout Google Takeout
// writes: the product name in the header, then a body cell holding the
// activity line, the timestamp line, and the response, then an empty
// right-aligned cell and a caption cell.
func geminiAppsCellHTML(product, line, timestamp, response string) string {
	return `<div class="outer-cell mdl-cell mdl-cell--12-col mdl-shadow--2dp"><div class="mdl-grid">` +
		`<div class="header-cell mdl-cell mdl-cell--12-col"><p class="mdl-typography--title">` + product + `<br></p></div>` +
		`<div class="content-cell mdl-cell mdl-cell--6-col mdl-typography--body-1">` + line + `<br>` + timestamp + `<br>` + response + `</div>` +
		`<div class="content-cell mdl-cell mdl-cell--6-col mdl-typography--body-1 mdl-typography--text-right"></div>` +
		`<div class="content-cell mdl-cell mdl-cell--12-col mdl-typography--caption"><b>Products:</b><br>&emsp;` + product + `<br></div>` +
		`</div></div>`
}

func geminiAppsPromptedCellHTML(timestamp, prompt, response string) string {
	return geminiAppsCellHTML("Gemini Apps", "Prompted&nbsp;"+prompt, timestamp, response)
}

func geminiAppsDocumentHTML(htmlAttrs, title string, cells ...string) string {
	return `<html` + htmlAttrs + `><head><title>` + title + `</title></head><body><div class="mdl-grid">` +
		strings.Join(cells, "\n") + `</div></body></html>`
}

func geminiAppsPromptedDocument(cells ...string) string {
	return geminiAppsDocumentHTML("", "My Activity History", cells...)
}

type geminiAppsParse struct {
	results []ParseResult
	summary GeminiAppsParseSummary
	err     error
}

// parseGeminiAppsRoot parses root with host as the importing machine's zone,
// so results do not depend on the zone of the machine running the tests.
func parseGeminiAppsRoot(t *testing.T, root string, host *time.Location) geminiAppsParse {
	t.Helper()
	var got geminiAppsParse
	got.summary, got.err = parseGeminiAppsExport(root, host, func(result ParseResult) error {
		got.results = append(got.results, result)
		return nil
	})
	return got
}

func parseGeminiAppsHTML(t *testing.T, fixture string) geminiAppsParse {
	t.Helper()
	path := filepath.Join(t.TempDir(), "MyActivity.html")
	require.NoError(t, os.WriteFile(path, []byte(fixture), 0o644))
	return parseGeminiAppsRoot(t, path, time.UTC)
}

func geminiAppsIDsByPrompt(t *testing.T, fixture string) map[string]string {
	t.Helper()
	got := parseGeminiAppsHTML(t, fixture)
	require.NoError(t, got.err)
	ids := make(map[string]string)
	for _, result := range got.results {
		ids[result.Messages[0].Content] = result.Session.ID
	}
	return ids
}

func geminiAppsMessageContents(result ParseResult) []string {
	contents := make([]string, 0, len(result.Messages))
	for _, message := range result.Messages {
		contents = append(contents, message.Content)
	}
	return contents
}

func TestParseGeminiAppsExportTakeoutFixture(t *testing.T) {
	provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
	require.True(t, ok)
	exporter, ok := provider.(GeminiAppsExportParser)
	require.True(t, ok)

	path := filepath.Join("testdata", "gemini-apps", "MyActivity.html")
	var results []ParseResult
	summary, err := exporter.ParseGeminiAppsExport(path, func(result ParseResult) error {
		results = append(results, result)
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 1, summary.Skipped)
	assert.Zero(t, summary.Errors)
	require.Len(t, results, 3)

	first := results[0]
	assert.Equal(t, AgentGeminiApps, first.Session.Agent)
	assert.Equal(t, "gemini.google.com", first.Session.Project)
	assert.Equal(t, "How do I list files sorted by size?", first.Session.FirstMessage)
	assert.Equal(t, "How do I list files sorted by size?", first.Session.SessionName)
	assert.Equal(t, 2, first.Session.MessageCount)
	assert.Equal(t, 1, first.Session.UserMessageCount)
	require.Len(t, first.Messages, 2)
	assert.Equal(t, RoleUser, first.Messages[0].Role)
	assert.Equal(t, "How do I list files sorted by size?", first.Messages[0].Content)
	assert.Equal(t, RoleAssistant, first.Messages[1].Role)
	assert.Equal(t, 1, first.Messages[1].Ordinal)
	assert.Equal(t,
		"Use ls with the size flag:\n\nls -lS\n\n\nAdd -r to reverse the order.\n\nAdd -h for readable sizes.",
		first.Messages[1].Content,
	)
	assert.Equal(t, first.Session.StartedAt, first.Messages[1].Timestamp)
	assert.True(t, strings.HasPrefix(first.Session.ID, "gemini-apps:"))

	second := results[1]
	assert.Equal(t,
		[]string{"Write a haiku about rain.\nKeep it short.", "Soft rain on the roof\nthe gutters hum their one song\nmorning smells of earth"},
		geminiAppsMessageContents(second),
	)

	third := results[2]
	assert.Equal(t, []string{"Thanks!"}, geminiAppsMessageContents(third))
	assert.Equal(t, 1, third.Session.MessageCount)

	var repeated []string
	_, err = exporter.ParseGeminiAppsExport(path, func(result ParseResult) error {
		repeated = append(repeated, result.Session.ID)
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, []string{first.Session.ID, second.Session.ID, third.Session.ID}, repeated)
}

func TestParseGeminiAppsFixtureTimestampsUseExportedZone(t *testing.T) {
	path := filepath.Join("testdata", "gemini-apps", "MyActivity.html")
	got := parseGeminiAppsRoot(t, path, time.UTC)
	require.NoError(t, got.err)
	require.Len(t, got.results, 3)
	assert.Equal(t, "2026-11-10T05:15:42Z", got.results[0].Session.StartedAt.UTC().Format(time.RFC3339))
	assert.Equal(t, "2026-11-09T00:03:17Z", got.results[1].Session.StartedAt.UTC().Format(time.RFC3339))
	assert.Equal(t, "2026-11-09T00:05:00Z", got.results[2].Session.StartedAt.UTC().Format(time.RFC3339))
}

func TestParseGeminiAppsTakeoutDirectoryLayout(t *testing.T) {
	root := t.TempDir()
	fixture, err := os.ReadFile(filepath.Join("testdata", "gemini-apps", "MyActivity.html"))
	require.NoError(t, err)
	activityDir := filepath.Join(root, "Takeout", "My Activity", "Gemini Apps")
	require.NoError(t, os.MkdirAll(activityDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(activityDir, "MyActivity.html"), fixture, 0o644))
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "Takeout", "archive_browser.html"),
		[]byte(`<html><head><title>Archive</title></head><body><h1>Your account, your data.</h1></body></html>`),
		0o644,
	))

	got := parseGeminiAppsRoot(t, root, time.UTC)
	require.NoError(t, got.err)
	assert.Len(t, got.results, 3)
	assert.Equal(t, 1, got.summary.Skipped)
}

func TestParseGeminiAppsIDsIgnoreUnrelatedRecordOrder(t *testing.T) {
	a := geminiAppsPromptedCellHTML("Jan 2, 2025, 3:04:05 PM EDT", "first", "")
	b := geminiAppsPromptedCellHTML("Jan 3, 2025, 3:04:05 PM EDT", "second", "")
	c := geminiAppsPromptedCellHTML("Jan 4, 2025, 3:04:05 PM EDT", "prepended", "")

	initial := geminiAppsIDsByPrompt(t, geminiAppsPromptedDocument(a, b))
	prepended := geminiAppsIDsByPrompt(t, geminiAppsPromptedDocument(c, a, b))
	reordered := geminiAppsIDsByPrompt(t, geminiAppsPromptedDocument(b, a))

	require.Len(t, initial, 2)
	require.Len(t, prepended, 3)
	require.Len(t, reordered, 2)
	require.NotEmpty(t, initial["first"])
	require.NotEmpty(t, initial["second"])
	assert.Equal(t, initial["first"], prepended["first"])
	assert.Equal(t, initial["second"], prepended["second"])
	assert.Equal(t, initial["first"], reordered["first"])
	assert.Equal(t, initial["second"], reordered["second"])
}

func TestParseGeminiAppsIDsDisambiguateSameTimestampOccurrences(t *testing.T) {
	timestamp := "Jan 2, 2025, 3:04:05 PM EDT"
	fixture := geminiAppsPromptedDocument(
		geminiAppsPromptedCellHTML(timestamp, "first", ""),
		geminiAppsPromptedCellHTML(timestamp, "second", ""),
	)
	ids := geminiAppsIDsByPrompt(t, fixture)
	repeated := geminiAppsIDsByPrompt(t, fixture)

	require.Len(t, ids, 2)
	assert.NotEqual(t, ids["first"], ids["second"])
	assert.Equal(t, ids, repeated)
}

func TestParseGeminiAppsEquivalentNumericZonesKeepStableIdentity(t *testing.T) {
	cell := func(zone string) string {
		return geminiAppsPromptedCellHTML("Jan 2, 2025, 3:04:05 PM "+zone, "prompt", "")
	}
	ids := geminiAppsIDsByPrompt(t, geminiAppsPromptedDocument(cell("GMT+8")))
	padded := geminiAppsIDsByPrompt(t, geminiAppsPromptedDocument(cell("GMT+08:00")))
	require.NotEmpty(t, ids["prompt"])
	assert.Equal(t, ids["prompt"], padded["prompt"])
}

func TestParseGeminiAppsSplitsPromptAndResponse(t *testing.T) {
	tests := []struct {
		name     string
		prompt   string
		response string
		want     []string
	}{
		{
			name: "plain", prompt: "hello", response: "<p>hi there</p>",
			want: []string{"hello", "hi there"},
		},
		{
			name: "multi-line prompt", prompt: "line one<br>line two", response: "<p>answer</p>",
			want: []string{"line one\nline two", "answer"},
		},
		{
			name: "prompt whitespace", prompt: "  prompt   with  gaps ", response: "<p> answer </p>",
			want: []string{"prompt with gaps", "answer"},
		},
		{
			name: "prompt keeps timestamp text", prompt: "Meeting at Jan 2, 2025, 3:04:05 PM EDT is confirmed.", response: "",
			want: []string{"Meeting at Jan 2, 2025, 3:04:05 PM EDT is confirmed."},
		},
		{
			name: "response keeps timestamp text", prompt: "prompt", response: "<p>The old log says Jan 2, 2025, 3:04:05 PM EDT.</p>",
			want: []string{"prompt", "The old log says Jan 2, 2025, 3:04:05 PM EDT."},
		},
		{
			name: "response keeps a standalone timestamp line", prompt: "prompt", response: "Jan 2, 2025, 3:04:05 PM EDT<br><p>answer</p>",
			want: []string{"prompt", "Jan 2, 2025, 3:04:05 PM EDT\n\nanswer"},
		},
		{
			name: "prompt line with a date and clock", prompt: "Summarize this log<br>12/03/2025 10:00:00 server restarted", response: "<p>It restarted.</p>",
			want: []string{"Summarize this log\n12/03/2025 10:00:00 server restarted", "It restarted."},
		},
		{
			name: "label word inside prompt", prompt: "Prompted by what?", response: "",
			want: []string{"Prompted by what?"},
		},
		{
			name: "no response", prompt: "prompt", response: "",
			want: []string{"prompt"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseGeminiAppsHTML(t, geminiAppsPromptedDocument(
				geminiAppsPromptedCellHTML("Jan 2, 2025, 3:04:05 PM EDT", tt.prompt, tt.response),
			))
			require.NoError(t, got.err)
			require.Len(t, got.results, 1)
			assert.Equal(t, tt.want, geminiAppsMessageContents(got.results[0]))
			assert.Equal(t, len(tt.want), got.results[0].Session.MessageCount)
		})
	}
}

func TestParseGeminiAppsResponseRenderingIsPlainText(t *testing.T) {
	tests := []struct {
		name     string
		response string
		want     string
	}{
		{name: "inline separator", response: `<span>left</span> <strong>right</strong>`, want: "left right"},
		{name: "duplicate boundary space", response: `<span>left </span> <strong>right</strong>`, want: "left right"},
		{name: "link", response: `<p>See <a href="https://example.invalid">source</a> for details</p>`, want: "See source for details"},
		{name: "nested inline", response: `<p>Read <span><strong>this</strong></span> now</p>`, want: "Read this now"},
		{name: "list", response: `<ul><li>first <a href="https://example.invalid">link</a></li><li><span>second</span> item</li></ul>`, want: "first link\n\nsecond item"},
		{name: "heading quote table", response: `<h2>Heading</h2><blockquote>quote</blockquote><table><tr><th>key</th><td>value</td></tr></table>`, want: "Heading\n\nquote\n\nkey\n\nvalue"},
		{name: "br inside paragraph", response: `<p>line one<br>line two</p><p>next</p>`, want: "line one\nline two\n\nnext"},
		{name: "inline code", response: `<p>Use <code>go test</code> now</p>`, want: "Use go test now"},
		{name: "nested code collapses spaces", response: `<span><code>  x  y  </code></span>`, want: "x y"},
		{
			name:     "preformatted code keeps authored whitespace",
			response: "<p>intro</p><pre><code>  line one\n\tline  two  \n`one` and ```three```  \n</code></pre><p>inline <code>  x  </code></p>",
			want:     "intro\n\n  line one\n\tline  two  \n`one` and ```three```  \n\n\ninline x",
		},
		{name: "boundary preformatted whitespace", response: "<pre><code>  leading\n\tbody\n  </code></pre>", want: "  leading\n\tbody\n  "},
		{
			name:     "hidden elements and comments",
			response: "\n<!-- generated marker -->\n<script>secret script</script><style>secret style</style><template><p>secret template</p></template><noscript>secret noscript</noscript>\n<p>answer</p>\n",
			want:     "answer",
		},
		{name: "empty formatting nodes", response: `<p>one</p><strong></strong><span></span><p></p><code></code><p>two</p>`, want: "one\n\ntwo"},
		{name: "entities", response: `<p><strong>first</strong> answer &amp; detail</p>`, want: "first answer & detail"},
		{name: "control characters", response: "<p>first\x7f\u0085\u009b answer</p>", want: "first answer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseGeminiAppsHTML(t, geminiAppsPromptedDocument(
				geminiAppsPromptedCellHTML("Jan 2, 2025, 3:04:05 PM EDT", "prompt", tt.response),
			))
			require.NoError(t, got.err)
			require.Len(t, got.results, 1)
			require.Len(t, got.results[0].Messages, 2)
			assert.Equal(t, tt.want, got.results[0].Messages[1].Content)
			assert.NotContains(t, got.results[0].Messages[1].Content, "secret")
		})
	}
}

func TestParseGeminiAppsSkipsActivityThatIsNotAPrompt(t *testing.T) {
	valid := geminiAppsPromptedCellHTML("Jan 2, 2025, 3:04:05 PM EDT", "prompt", "<p>answer</p>")
	fixture := geminiAppsPromptedDocument(
		valid,
		geminiAppsCellHTML("Gemini Apps", "Gave feedback: Good response", "Jan 3, 2025, 3:04:05 PM PST", ""),
		geminiAppsCellHTML("Gemini Apps", "Not Prompted&nbsp;thing", "Jan 3, 2025, 3:04:05 PM PST", ""),
		geminiAppsCellHTML("Gemini Apps", "Prompted:&nbsp;thing", "Jan 3, 2025, 3:04:05 PM PST", ""),
		// Skipped records are not validated, so a malformed timestamp is fine.
		geminiAppsCellHTML("Gemini Apps", "Created a Gem", "not a timestamp", ""),
	)
	got := parseGeminiAppsHTML(t, fixture)
	require.NoError(t, got.err)
	require.Len(t, got.results, 1)
	assert.Equal(t, "prompt", got.results[0].Messages[0].Content)
	assert.Equal(t, 4, got.summary.Skipped)
	assert.Zero(t, got.summary.Errors)
}

func TestParseGeminiAppsNoPromptsIsError(t *testing.T) {
	for _, label := range []string{"Angefragt&nbsp;prompt", "Gave feedback"} {
		t.Run(label, func(t *testing.T) {
			got := parseGeminiAppsHTML(t, geminiAppsPromptedDocument(
				geminiAppsCellHTML("Gemini Apps", label, "Jan 2, 2025, 3:04:05 PM EDT", ""),
			))
			require.ErrorContains(t, got.err, "no admissible Prompted records")
			assert.Empty(t, got.results)
			assert.Equal(t, 1, got.summary.Skipped)
		})
	}
}

func TestParseGeminiAppsMalformedPromptRecordsAreCountedErrors(t *testing.T) {
	valid := geminiAppsPromptedCellHTML("Jan 2, 2025, 3:04:05 PM EDT", "prompt", "")
	tests := []struct {
		name string
		cell string
	}{
		{
			name: "no timestamp line",
			cell: geminiAppsCellHTML("Gemini Apps", "Prompted&nbsp;lost", "no time here", ""),
		},
		{
			name: "empty prompt",
			cell: geminiAppsCellHTML("Gemini Apps", "Prompted&nbsp;", "Jan 3, 2025, 3:04:05 PM EDT", "<p>answer</p>"),
		},
		{
			name: "no content cell",
			cell: `<div class="outer-cell mdl-cell"><div class="mdl-grid"><div class="header-cell mdl-cell"><p class="mdl-typography--title">Gemini Apps<br></p></div></div></div>`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseGeminiAppsHTML(t, geminiAppsPromptedDocument(valid, tt.cell))
			require.NoError(t, got.err)
			require.Len(t, got.results, 1)
			assert.Equal(t, "prompt", got.results[0].Messages[0].Content)
			assert.Equal(t, 1, got.summary.Errors)
		})
	}
}

func TestParseGeminiAppsIgnoresOtherProductCells(t *testing.T) {
	gemini := geminiAppsPromptedCellHTML("Jan 2, 2025, 3:04:05 PM EDT", "prompt", "")
	search := geminiAppsCellHTML("Search", `Visited&nbsp;<a href="https://example.invalid/">https://example.invalid/</a>`, "Jan 2, 2025, 3:04:05 PM XYZ", "")

	t.Run("mixed file", func(t *testing.T) {
		got := parseGeminiAppsHTML(t, geminiAppsPromptedDocument(gemini, search))
		require.NoError(t, got.err)
		require.Len(t, got.results, 1)
		assert.Equal(t, "prompt", got.results[0].Messages[0].Content)
		assert.Zero(t, got.summary.Skipped)
		assert.Zero(t, got.summary.Errors)
	})

	t.Run("mixed directory", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(root, "01-gemini.html"), []byte(geminiAppsPromptedDocument(gemini)), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(root, "02-search.html"), []byte(geminiAppsPromptedDocument(search)), 0o644))
		got := parseGeminiAppsRoot(t, root, time.UTC)
		require.NoError(t, got.err)
		require.Len(t, got.results, 1)
		assert.Zero(t, got.summary.Skipped)
	})

	t.Run("only other products", func(t *testing.T) {
		got := parseGeminiAppsHTML(t, geminiAppsPromptedDocument(search))
		require.ErrorContains(t, got.err, "does not contain a Gemini Apps")
		assert.Empty(t, got.results)
	})
}

func TestParseGeminiAppsNonActivityHTML(t *testing.T) {
	valid := geminiAppsPromptedDocument(geminiAppsPromptedCellHTML("Jan 2, 2025, 3:04:05 PM EDT", "prompt", ""))
	other := `<html lang="de"><head><title>Meine Aktivität</title></head><body><p>unrelated</p></body></html>`

	t.Run("ignored beside a Gemini file", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(root, "01-gemini.html"), []byte(valid), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(root, "02-other.html"), []byte(other), 0o644))
		got := parseGeminiAppsRoot(t, root, time.UTC)
		require.NoError(t, got.err)
		require.Len(t, got.results, 1)
	})

	t.Run("alone is rejected", func(t *testing.T) {
		got := parseGeminiAppsHTML(t, other)
		require.ErrorContains(t, got.err, "does not contain a Gemini Apps")
	})

	t.Run("empty activity page is rejected", func(t *testing.T) {
		got := parseGeminiAppsHTML(t, geminiAppsPromptedDocument())
		require.ErrorContains(t, got.err, "does not contain a Gemini Apps")
	})

	t.Run("no HTML files", func(t *testing.T) {
		got := parseGeminiAppsRoot(t, t.TempDir(), time.UTC)
		require.ErrorContains(t, got.err, "no HTML files")
	})
}

func TestParseGeminiAppsRejectsUnsupportedRenderingsBeforeCallbacks(t *testing.T) {
	supported := geminiAppsPromptedCellHTML("Jan 2, 2025, 3:04:05 PM EDT", "prompt", "")
	tests := []struct {
		name    string
		files   []string
		wantErr string
	}{
		{
			name:    "declared non-English locale",
			files:   []string{geminiAppsPromptedDocument(supported), geminiAppsDocumentHTML(` lang="de"`, "My Activity History", supported)},
			wantErr: "unsupported Gemini Apps Takeout locale",
		},
		{
			name:    "localized title",
			files:   []string{geminiAppsDocumentHTML("", "Meine Aktivität", supported)},
			wantErr: "unsupported localized or changed Gemini Apps Takeout format",
		},
		{
			name: "localized timestamp",
			files: []string{geminiAppsPromptedDocument(supported,
				geminiAppsPromptedCellHTML("2. Januar 2025, 15:04:05 MEZ", "prompt", ""))},
			wantErr: "unsupported Gemini Apps timestamp",
		},
		{
			name: "unknown zone",
			files: []string{geminiAppsPromptedDocument(supported,
				geminiAppsPromptedCellHTML("Jan 3, 2025, 3:04:05 PM XYZ", "prompt", ""))},
			wantErr: `unsupported Gemini Apps timestamp zone "XYZ"`,
		},
		{
			name: "unknown zone in a later file",
			files: []string{geminiAppsPromptedDocument(supported), geminiAppsPromptedDocument(
				geminiAppsPromptedCellHTML("Jan 3, 2025, 3:04:05 PM XYZ", "prompt", ""))},
			wantErr: "unsupported Gemini Apps timestamp zone",
		},
	}
	for _, zone := range []string{"GMT+8:3", "GMT+8junk", "GMT+8:30junk", "GMT+24", "GMT+8:60"} {
		tests = append(tests, struct {
			name    string
			files   []string
			wantErr string
		}{
			name: "malformed zone " + zone,
			files: []string{geminiAppsPromptedDocument(supported,
				geminiAppsPromptedCellHTML("Jan 3, 2025, 3:04:05 PM "+zone, "prompt", ""))},
			wantErr: strings.ToUpper(zone),
		})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			for i, file := range tt.files {
				name := filepath.Join(root, string(rune('a'+i))+".html")
				require.NoError(t, os.WriteFile(name, []byte(file), 0o644))
			}
			got := parseGeminiAppsRoot(t, root, time.UTC)
			require.ErrorContains(t, got.err, tt.wantErr)
			assert.Empty(t, got.results)
		})
	}
}

func TestParseGeminiAppsTimestampUsesExplicitZones(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{"edt", "Jan 2, 2025, 3:04:05 PM EDT", "2025-01-02T19:04:05Z"},
		{"pst", "Jan 2, 2025, 3:04:05 PM PST", "2025-01-02T23:04:05Z"},
		{"utc", "Jan 2, 2025, 3:04:05 PM UTC", "2025-01-02T15:04:05Z"},
		{"gmt", "Jan 2, 2025, 3:04:05 PM GMT", "2025-01-02T15:04:05Z"},
		{"est", "Jan 2, 2025, 3:04:05 PM EST", "2025-01-02T20:04:05Z"},
		{"cst", "Jan 2, 2025, 3:04:05 PM CST", "2025-01-02T21:04:05Z"},
		{"cdt", "Jan 2, 2025, 3:04:05 PM CDT", "2025-01-02T20:04:05Z"},
		{"mst", "Jan 2, 2025, 3:04:05 PM MST", "2025-01-02T22:04:05Z"},
		{"mdt", "Jan 2, 2025, 3:04:05 PM MDT", "2025-01-02T21:04:05Z"},
		{"pdt", "Jan 2, 2025, 3:04:05 PM PDT", "2025-01-02T22:04:05Z"},
		{"narrow no-break space", "Jan 2, 2025, 3:04:05\u202fPM PST", "2025-01-02T23:04:05Z"},
		{"whole-hour", "Jan 2, 2025, 3:04:05 PM GMT+8", "2025-01-02T07:04:05Z"},
		{"whole-hour-padded", "Jan 2, 2025, 3:04:05 PM GMT+08:00", "2025-01-02T07:04:05Z"},
		{"whole-hour-two-digit", "Jan 2, 2025, 3:04:05 PM GMT+08", "2025-01-02T07:04:05Z"},
		{"whole-hour-lowercase", "Jan 2, 2025, 3:04:05 PM gmt+8", "2025-01-02T07:04:05Z"},
		{"whole-hour-mixed-case", "Jan 2, 2025, 3:04:05 PM GmT+8", "2025-01-02T07:04:05Z"},
		{"whole-hour-negative", "Jan 2, 2025, 3:04:05 PM GMT-8", "2025-01-02T23:04:05Z"},
		{"numeric", "Jan 2, 2025, 3:04:05 PM GMT+05:30", "2025-01-02T09:34:05Z"},
		{"numeric-max", "Jan 2, 2025, 3:04:05 PM GMT+23:59", "2025-01-01T15:05:05Z"},
		{"negative-zero", "Jan 2, 2025, 3:04:05 PM GMT-00:30", "2025-01-02T15:34:05Z"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			match := geminiAppsTimestampLineRE.FindStringSubmatch(normalizeMetadata(tt.text))
			require.Len(t, match, 2)
			parsed, err := parseGeminiAppsTimestamp(match[0], match[1], time.UTC)
			require.NoError(t, err)
			assert.Equal(t, tt.want, parsed.UTC().Format(time.RFC3339))
		})
	}

	for _, zone := range []string{
		"GMT+8:3", "GMT+8junk", "GMT+8:30junk", "GMT+24", "GMT+8:60", "GMT+8,", "XYZ",
	} {
		t.Run("reject-"+zone, func(t *testing.T) {
			match := geminiAppsTimestampLineRE.FindStringSubmatch("Jan 2, 2025, 3:04:05 PM " + zone)
			require.Len(t, match, 2)
			_, err := parseGeminiAppsTimestamp(match[0], match[1], time.UTC)
			require.ErrorContains(t, err, "unsupported")
			assert.ErrorContains(t, err, strings.ToUpper(zone))
			assert.ErrorContains(t, err, "TZ")
		})
	}
}

func TestParseGeminiAppsTimestampResolvesHostZoneAbbreviations(t *testing.T) {
	load := func(name string) *time.Location {
		loc, err := time.LoadLocation(name)
		require.NoError(t, err)
		return loc
	}
	tests := []struct {
		name string
		host *time.Location
		text string
		want string
	}{
		{"india", load("Asia/Kolkata"), "Nov 10, 2026, 3:33:10 AM IST", "2026-11-09T22:03:10Z"},
		{"ireland summer abbreviation", load("Europe/Dublin"), "Jul 10, 2026, 3:33:10 AM IST", "2026-07-10T02:33:10Z"},
		{"china overrides the table", load("Asia/Shanghai"), "Jan 2, 2025, 3:04:05 PM CST", "2025-01-02T07:04:05Z"},
		{"export-time summer zone on a winter record", load("Europe/London"), "Jan 15, 2021, 6:54:12 PM BST", "2021-01-15T17:54:12Z"},
		{"winter zone on a summer record", load("Europe/London"), "Jul 15, 2021, 6:54:12 PM GMT", "2021-07-15T18:54:12Z"},
		{"table still applies when the host differs", load("Asia/Kolkata"), "Jan 2, 2025, 3:04:05 PM EDT", "2025-01-02T19:04:05Z"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			match := geminiAppsTimestampLineRE.FindStringSubmatch(tt.text)
			require.Len(t, match, 2)
			parsed, err := parseGeminiAppsTimestamp(match[0], match[1], tt.host)
			require.NoError(t, err)
			assert.Equal(t, tt.want, parsed.UTC().Format(time.RFC3339))
		})
	}

	t.Run("abbreviation the host does not use", func(t *testing.T) {
		match := geminiAppsTimestampLineRE.FindStringSubmatch("Nov 10, 2026, 3:33:10 AM IST")
		require.Len(t, match, 2)
		_, err := parseGeminiAppsTimestamp(match[0], match[1], load("America/New_York"))
		require.ErrorContains(t, err, `unsupported Gemini Apps timestamp zone "IST"`)
	})
}

func TestParseGeminiAppsExportResolvesHostZoneEndToEnd(t *testing.T) {
	kolkata, err := time.LoadLocation("Asia/Kolkata")
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "MyActivity.html")
	require.NoError(t, os.WriteFile(path, []byte(geminiAppsPromptedDocument(
		geminiAppsPromptedCellHTML("Nov 10, 2026, 3:33:10 AM IST", "prompt", "<p>answer</p>"),
	)), 0o644))

	got := parseGeminiAppsRoot(t, path, kolkata)
	require.NoError(t, got.err)
	require.Len(t, got.results, 1)
	assert.Equal(t, "2026-11-09T22:03:10Z", got.results[0].Session.StartedAt.UTC().Format(time.RFC3339))

	got = parseGeminiAppsRoot(t, path, time.UTC)
	require.ErrorContains(t, got.err, `unsupported Gemini Apps timestamp zone "IST"`)
	assert.Empty(t, got.results)
}
