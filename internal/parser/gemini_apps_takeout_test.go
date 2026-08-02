package parser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sanitizedGeminiAppsHTML = `<!doctype html>
<html><head><title>My Activity History</title></head><body>
<div class="outer-cell mdl-cell">
  <div class="header-cell mdl-cell"><h3>Gemini Apps</h3><p>Prompted<br></p><p>Jan 2, 2025, 3:04:05 PM EDT</p></div>
  <div class="content-cell mdl-cell"><p>first prompt<br></p><p><strong>first</strong> answer &amp; detail</p><script>secret script</script><style>secret style</style><template>secret template</template><noscript>secret noscript</noscript></div>
</div>
<div class="outer-cell mdl-cell">
  <div class="header-cell mdl-cell"><h3>Gemini Apps</h3><p>Canvas</p><p>Jan 3, 2025, 3:04:05 PM PST</p></div>
  <div class="content-cell mdl-cell"><p>canvas content</p></div>
</div>
<div class="outer-cell mdl-cell">
  <div class="header-cell mdl-cell"><h3>Gemini Apps</h3><p>Feedback</p><p>Jan 4, 2025, 3:04:05 PM GMT+05:30</p></div>
  <div class="content-cell mdl-cell"><p>feedback content</p></div>
</div>
<div class="outer-cell mdl-cell">
  <div class="header-cell mdl-cell"><h3>Gemini Apps</h3><p>Prompted<br></p><p>Jan 5, 2025, 3:04:05 PM PST</p></div>
  <div class="content-cell mdl-cell"><p>second prompt</p><p>second answer</p></div>
</div>
<div class="outer-cell mdl-cell">
  <div class="header-cell mdl-cell"><h3>Gemini Apps</h3><p>Unknown activity</p><p>Jan 6, 2025, 3:04:05 PM EDT</p></div>
  <div class="content-cell mdl-cell"><p>not a prompt</p></div>
</div>
</body></html>`

func geminiAppsSingleCellHTML(
	lang, title, label, timestamp, content string,
) string {
	return `<!doctype html><html` + lang + `><head><title>` + title + `</title></head><body>` +
		geminiAppsProductCellHTML("Gemini Apps", label, timestamp, content) +
		`</body></html>`
}

func geminiAppsProductCellHTML(
	product, label, timestamp, content string,
) string {
	return `<div class="outer-cell"><div class="header-cell"><h3>` + product +
		`</h3><p>` + label + `</p><p>` + timestamp +
		`</p></div><div class="content-cell">` + content +
		`</div></div>`
}

func parseGeminiAppsIDs(t *testing.T, fixture string) map[string]string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "activity.html")
	require.NoError(t, os.WriteFile(path, []byte(fixture), 0o644))

	provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
	require.True(t, ok)
	exporter := provider.(GeminiAppsExportParser)
	ids := make(map[string]string)
	_, err := exporter.ParseGeminiAppsExport(path, func(result ParseResult) error {
		ids[result.Messages[0].Content] = result.Session.ID
		return nil
	})
	require.NoError(t, err)
	return ids
}

func geminiAppsPromptedDocument(cells ...string) string {
	return `<!doctype html><html><head><title>My Activity History</title></head><body>` +
		strings.Join(cells, "") + `</body></html>`
}

func TestParseGeminiAppsExportRealOuterCellHeaderShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "activity.html")
	require.NoError(t, os.WriteFile(path, []byte(sanitizedGeminiAppsHTML), 0o644))

	provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
	require.True(t, ok)
	exporter, ok := provider.(GeminiAppsExportParser)
	require.True(t, ok)

	var results []ParseResult
	summary, err := exporter.ParseGeminiAppsExport(path, func(result ParseResult) error {
		results = append(results, result)
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 2, len(results))
	assert.Equal(t, 3, summary.Skipped)
	assert.Zero(t, summary.Errors)

	assert.Equal(t, "gemini.google.com", results[0].Session.Project)
	assert.Equal(t, AgentGeminiApps, results[0].Session.Agent)
	assert.Equal(t, "first prompt\n\nfirst answer & detail", results[0].Messages[0].Content)
	assert.Equal(t, RoleUser, results[0].Messages[0].Role)
	assert.Len(t, results[0].Messages, 1)
	assert.NotContains(t, results[0].Messages[0].Content, "secret")
	assert.Equal(t, "2025-01-02T19:04:05Z", results[0].Session.StartedAt.UTC().Format("2006-01-02T15:04:05Z"))

	firstID := results[0].Session.ID
	var repeated []ParseResult
	_, err = exporter.ParseGeminiAppsExport(path, func(result ParseResult) error {
		repeated = append(repeated, result)
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, firstID, repeated[0].Session.ID)
}

func TestParseGeminiAppsIDsIgnoreUnrelatedRecordOrder(t *testing.T) {
	cell := func(label, timestamp, content string) string {
		return geminiAppsProductCellHTML("Gemini Apps", label, timestamp, "<p>"+content+"</p>")
	}
	a := cell("Prompted", "Jan 2, 2025, 3:04:05 PM EDT", "first")
	b := cell("Prompted", "Jan 3, 2025, 3:04:05 PM EDT", "second")
	c := cell("Prompted", "Jan 4, 2025, 3:04:05 PM EDT", "prepended")

	initial := parseGeminiAppsIDs(t, geminiAppsPromptedDocument(a, b))
	prepended := parseGeminiAppsIDs(t, geminiAppsPromptedDocument(c, a, b))
	reordered := parseGeminiAppsIDs(t, geminiAppsPromptedDocument(b, a))

	require.Len(t, initial, 2)
	require.Len(t, prepended, 3)
	require.Len(t, reordered, 2)
	require.Contains(t, initial, "first")
	require.Contains(t, initial, "second")
	require.Contains(t, prepended, "first")
	require.Contains(t, prepended, "second")
	require.Contains(t, reordered, "first")
	require.Contains(t, reordered, "second")
	assert.Equal(t, initial["first"], prepended["first"])
	assert.Equal(t, initial["second"], prepended["second"])
	assert.Equal(t, initial["first"], reordered["first"])
	assert.Equal(t, initial["second"], reordered["second"])
}

func TestParseGeminiAppsIDsDisambiguateSameTimestampOccurrences(t *testing.T) {
	timestamp := "Jan 2, 2025, 3:04:05 PM EDT"
	fixture := geminiAppsPromptedDocument(
		geminiAppsProductCellHTML("Gemini Apps", "Prompted", timestamp, "<p>first</p>"),
		geminiAppsProductCellHTML("Gemini Apps", "Prompted", timestamp, "<p>second</p>"),
	)
	ids := parseGeminiAppsIDs(t, fixture)
	repeated := parseGeminiAppsIDs(t, fixture)

	require.Len(t, ids, 2)
	assert.NotEqual(t, ids["first"], ids["second"])
	assert.Equal(t, ids, repeated)
}

func TestParseGeminiAppsRejectsNotPromptedActivityLabel(t *testing.T) {
	fixture := strings.ReplaceAll(
		sanitizedGeminiAppsHTML,
		"<p>Prompted<br></p>",
		"<p>Not Prompted</p>",
	)
	path := filepath.Join(t.TempDir(), "not-prompted.html")
	require.NoError(t, os.WriteFile(path, []byte(fixture), 0o644))

	provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
	require.True(t, ok)
	exporter := provider.(GeminiAppsExportParser)
	var results []ParseResult
	summary, err := exporter.ParseGeminiAppsExport(path, func(result ParseResult) error {
		results = append(results, result)
		return nil
	})
	assert.Empty(t, results)
	assert.Equal(t, 5, summary.Skipped)
	assert.ErrorContains(t, err, "no admissible Prompted records")
}

func TestParseGeminiAppsPreservesOrdinaryResponseAndAnswerText(t *testing.T) {
	fixture := strings.Replace(
		sanitizedGeminiAppsHTML,
		`<div class="content-cell mdl-cell"><p>first prompt<br></p><p><strong>first</strong> answer &amp; detail</p><script>secret script</script><style>secret style</style><template>secret template</template><noscript>secret noscript</noscript></div>`,
		`<div class="content-cell mdl-cell"><p>ordinary Response: and Answer: text</p></div>`,
		1,
	)
	path := filepath.Join(t.TempDir(), "ordinary-markers.html")
	require.NoError(t, os.WriteFile(path, []byte(fixture), 0o644))

	provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
	require.True(t, ok)
	exporter := provider.(GeminiAppsExportParser)
	var results []ParseResult
	_, err := exporter.ParseGeminiAppsExport(path, func(result ParseResult) error {
		results = append(results, result)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, results, 2)
	require.Len(t, results[0].Messages, 1)
	assert.Equal(t, "ordinary Response: and Answer: text", results[0].Messages[0].Content)
}

func TestParseGeminiAppsIgnoresOtherProductCells(t *testing.T) {
	gemini := geminiAppsProductCellHTML(
		"Gemini Apps", "Prompted", "Jan 2, 2025, 3:04:05 PM EDT",
		"<p>prompt</p>",
	)
	youtube := geminiAppsProductCellHTML(
		"YouTube", "Watched", "Jan 2, 2025, 3:04:05 PM XYZ",
		"<p>video title</p>",
	)
	tests := []struct {
		name  string
		input string
	}{
		{
			name:  "mixed file",
			input: `<!doctype html><html><head><title>My Activity History</title></head><body>` + gemini + youtube + `</body></html>`,
		},
		{
			name:  "mixed directory",
			input: "directory",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if tt.input == "directory" {
				require.NoError(t, os.WriteFile(
					filepath.Join(root, "01-gemini.html"),
					[]byte(`<!doctype html><html><head><title>My Activity History</title></head><body>`+gemini+`</body></html>`),
					0o644,
				))
				require.NoError(t, os.WriteFile(
					filepath.Join(root, "02-youtube.html"),
					[]byte(`<!doctype html><html><head><title>My Activity History</title></head><body>`+youtube+`</body></html>`),
					0o644,
				))
			} else {
				root = filepath.Join(root, "mixed.html")
				require.NoError(t, os.WriteFile(root, []byte(tt.input), 0o644))
			}

			provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
			require.True(t, ok)
			exporter := provider.(GeminiAppsExportParser)
			var results []ParseResult
			summary, err := exporter.ParseGeminiAppsExport(root, func(result ParseResult) error {
				results = append(results, result)
				return nil
			})
			require.NoError(t, err)
			require.Len(t, results, 1)
			assert.Equal(t, "prompt", results[0].Messages[0].Content)
			assert.Zero(t, summary.Skipped)
			assert.Zero(t, summary.Errors)
		})
	}
}

func TestParseGeminiAppsOnlyOtherProductIsNotGeminiDocument(t *testing.T) {
	fixture := `<!doctype html><html><head><title>My Activity History</title></head><body>` +
		geminiAppsProductCellHTML(
			"YouTube", "Watched", "Jan 2, 2025, 3:04:05 PM XYZ", "<p>video</p>",
		) + `</body></html>`
	path := filepath.Join(t.TempDir(), "youtube.html")
	require.NoError(t, os.WriteFile(path, []byte(fixture), 0o644))

	provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
	require.True(t, ok)
	exporter := provider.(GeminiAppsExportParser)
	callbacks := 0
	_, err := exporter.ParseGeminiAppsExport(path, func(ParseResult) error {
		callbacks++
		return nil
	})
	assert.ErrorContains(t, err, "does not contain a Gemini Apps")
	assert.Zero(t, callbacks)
}

func TestParseGeminiAppsIgnoresUnrelatedLocalizedHTML(t *testing.T) {
	root := t.TempDir()
	valid := geminiAppsSingleCellHTML(
		"", "My Activity History", "Prompted",
		"Jan 2, 2025, 3:04:05 PM EDT", "<p>prompt</p>",
	)
	unrelated := `<!doctype html><html lang="de"><head><title>Meine Aktivität</title></head><body><p>unrelated</p></body></html>`
	require.NoError(t, os.WriteFile(filepath.Join(root, "01-gemini.html"), []byte(valid), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "02-unrelated.html"), []byte(unrelated), 0o644))

	provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
	require.True(t, ok)
	exporter := provider.(GeminiAppsExportParser)
	var results []ParseResult
	_, err := exporter.ParseGeminiAppsExport(root, func(result ParseResult) error {
		results = append(results, result)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "prompt", results[0].Messages[0].Content)
}

func TestParseGeminiAppsSkipsNonPromptedRecordsBeforeValidation(t *testing.T) {
	valid := geminiAppsProductCellHTML(
		"Gemini Apps", "Prompted", "Jan 2, 2025, 3:04:05 PM EDT", "<p>prompt</p>",
	)
	malformedCanvas := `<div class="outer-cell"><div class="header-cell"><h3>Gemini Apps</h3><p>Canvas</p><p>not a timestamp</p></div></div>`
	malformedFeedback := `<div class="outer-cell"><div class="header-cell"><h3>Gemini Apps</h3><p>Feedback</p></div></div>`
	malformedUnknown := `<div class="outer-cell"><div class="header-cell"><h3>Gemini Apps</h3><p>Unknown activity</p><p>not a timestamp</p></div></div>`
	fixture := `<!doctype html><html><head><title>My Activity History</title></head><body>` + valid + malformedCanvas + malformedFeedback + malformedUnknown + `</body></html>`
	path := filepath.Join(t.TempDir(), "non-prompted-validation.html")
	require.NoError(t, os.WriteFile(path, []byte(fixture), 0o644))

	provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
	require.True(t, ok)
	exporter := provider.(GeminiAppsExportParser)
	var results []ParseResult
	summary, err := exporter.ParseGeminiAppsExport(path, func(result ParseResult) error {
		results = append(results, result)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "prompt", results[0].Messages[0].Content)
	assert.Equal(t, 3, summary.Skipped)
	assert.Zero(t, summary.Errors)
}

func TestParseGeminiAppsIgnoresLeadingContentWhitespaceAndComments(t *testing.T) {
	content := "\n<!-- generated marker -->\n<script>ignored</script><style>ignored</style>\n<p>prompt</p><p>answer</p>\n"
	fixture := geminiAppsSingleCellHTML(
		"", "My Activity History", "Prompted",
		"Jan 2, 2025, 3:04:05 PM EDT", content,
	)
	path := filepath.Join(t.TempDir(), "content-prefix.html")
	require.NoError(t, os.WriteFile(path, []byte(fixture), 0o644))

	provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
	require.True(t, ok)
	exporter := provider.(GeminiAppsExportParser)
	var results []ParseResult
	_, err := exporter.ParseGeminiAppsExport(path, func(result ParseResult) error {
		results = append(results, result)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Len(t, results[0].Messages, 1)
	assert.Equal(t, "prompt\n\nanswer", results[0].Messages[0].Content)
}

func TestParseGeminiAppsEquivalentPresentationHasOneOwner(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{name: "separator", content: `<span>left</span> <strong>right</strong>`, want: "left right"},
		{name: "duplicate boundary", content: `<span>left </span> <strong>right</strong>`, want: "left right"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := geminiAppsSingleCellHTML(
				"", "My Activity History", "Prompted",
				"Jan 2, 2025, 3:04:05 PM EDT", tt.content,
			)
			path := filepath.Join(t.TempDir(), "inline-whitespace.html")
			require.NoError(t, os.WriteFile(path, []byte(fixture), 0o644))

			provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
			require.True(t, ok)
			exporter := provider.(GeminiAppsExportParser)
			var results []ParseResult
			_, err := exporter.ParseGeminiAppsExport(path, func(result ParseResult) error {
				results = append(results, result)
				return nil
			})
			require.NoError(t, err)
			require.Len(t, results, 1)
			require.Len(t, results[0].Messages, 1)
			assert.Equal(t, tt.want, results[0].Messages[0].Content)
		})
	}
}

func TestParseGeminiAppsPreservesNestedPreformattedWhitespace(t *testing.T) {
	fixture := geminiAppsSingleCellHTML(
		"", "My Activity History", "Prompted",
		"Jan 2, 2025, 3:04:05 PM EDT", "<span><code>  x  y  </code></span>",
	)
	path := filepath.Join(t.TempDir(), "nested-code-whitespace.html")
	require.NoError(t, os.WriteFile(path, []byte(fixture), 0o644))

	provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
	require.True(t, ok)
	exporter := provider.(GeminiAppsExportParser)
	var results []ParseResult
	_, err := exporter.ParseGeminiAppsExport(path, func(result ParseResult) error {
		results = append(results, result)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Len(t, results[0].Messages, 1)
	assert.Equal(t, "x y", results[0].Messages[0].Content)
}

func TestParseGeminiAppsEmptyFormattingNodeAlongsideTextIsError(t *testing.T) {
	fixture := geminiAppsSingleCellHTML(
		"", "My Activity History", "Prompted",
		"Jan 2, 2025, 3:04:05 PM EDT", "<p>prompt</p><strong></strong>tail",
	)
	path := filepath.Join(t.TempDir(), "empty-formatting-node.html")
	require.NoError(t, os.WriteFile(path, []byte(fixture), 0o644))

	provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
	require.True(t, ok)
	exporter := provider.(GeminiAppsExportParser)
	callbacks := 0
	_, err := exporter.ParseGeminiAppsExport(path, func(ParseResult) error {
		callbacks++
		return nil
	})
	assert.NoError(t, err)
	assert.Equal(t, 1, callbacks)
}

func TestParseGeminiAppsEmptyFormattingRunBetweenBlocksIsError(t *testing.T) {
	fixture := geminiAppsSingleCellHTML(
		"", "My Activity History", "Prompted",
		"Jan 2, 2025, 3:04:05 PM EDT", "<p>prompt</p><span></span><p>answer</p>",
	)
	path := filepath.Join(t.TempDir(), "empty-formatting-run.html")
	require.NoError(t, os.WriteFile(path, []byte(fixture), 0o644))

	provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
	require.True(t, ok)
	exporter := provider.(GeminiAppsExportParser)
	callbacks := 0
	_, err := exporter.ParseGeminiAppsExport(path, func(ParseResult) error {
		callbacks++
		return nil
	})
	assert.NoError(t, err)
	assert.Equal(t, 1, callbacks)
}

func TestParseGeminiAppsIgnoresHiddenActivityLabels(t *testing.T) {
	cell := `<div class="outer-cell"><div class="header-cell"><h3>Gemini Apps</h3><template><p>Prompted</p></template><p>Canvas</p><p>not a timestamp</p></div></div>`
	fixture := `<!doctype html><html><head><title>My Activity History</title></head><body>` + cell + `</body></html>`
	path := filepath.Join(t.TempDir(), "hidden-label.html")
	require.NoError(t, os.WriteFile(path, []byte(fixture), 0o644))

	provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
	require.True(t, ok)
	exporter := provider.(GeminiAppsExportParser)
	callbacks := 0
	summary, err := exporter.ParseGeminiAppsExport(path, func(ParseResult) error {
		callbacks++
		return nil
	})
	assert.ErrorContains(t, err, "no admissible Prompted records")
	assert.Zero(t, callbacks)
	assert.Equal(t, 1, summary.Skipped)
	assert.Zero(t, summary.Errors)
}

func TestParseGeminiAppsIgnoresHiddenEmptySemanticBlocks(t *testing.T) {
	fixture := geminiAppsSingleCellHTML(
		"", "My Activity History", "Prompted",
		"Jan 2, 2025, 3:04:05 PM EDT", "<p>prompt</p><template><p></p></template><p>answer</p>",
	)
	path := filepath.Join(t.TempDir(), "hidden-empty-block.html")
	require.NoError(t, os.WriteFile(path, []byte(fixture), 0o644))

	provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
	require.True(t, ok)
	exporter := provider.(GeminiAppsExportParser)
	var results []ParseResult
	_, err := exporter.ParseGeminiAppsExport(path, func(result ParseResult) error {
		results = append(results, result)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Len(t, results[0].Messages, 1)
	assert.Equal(t, "prompt\n\nanswer", results[0].Messages[0].Content)
	assert.Contains(t, results[0].Messages[0].Content, "answer")
}

func TestParseGeminiAppsIgnoresHiddenPreformattedAncestry(t *testing.T) {
	fixture := geminiAppsSingleCellHTML(
		"", "My Activity History", "Prompted",
		"Jan 2, 2025, 3:04:05 PM EDT", `<template><code>hidden</code></template><span>left </span> <strong>right</strong>`,
	)
	path := filepath.Join(t.TempDir(), "hidden-preformatted.html")
	require.NoError(t, os.WriteFile(path, []byte(fixture), 0o644))

	provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
	require.True(t, ok)
	exporter := provider.(GeminiAppsExportParser)
	var results []ParseResult
	_, err := exporter.ParseGeminiAppsExport(path, func(result ParseResult) error {
		results = append(results, result)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Len(t, results[0].Messages, 1)
	assert.Equal(t, "left right", results[0].Messages[0].Content)
}

func TestParseGeminiAppsPresentationDoesNotInferTurns(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "link",
			content: `<p>See <a href="https://example.invalid">source</a> for details</p>`,
			want:    "See source for details",
		},
		{
			name:    "span",
			content: `<p>Before <span>middle</span> after</p>`,
			want:    "Before middle after",
		},
		{
			name:    "nested marker",
			content: `<p>Read <span><strong>this</strong></span> now</p>`,
			want:    "Read this now",
		},
		{
			name:    "list markers",
			content: `<div><ul><li>first <a href="https://example.invalid">link</a></li><li><span>second</span> item</li></ul></div>`,
			want:    "first link\n\nsecond item",
		},
		{
			name:    "heading quote table",
			content: `<h2>Heading</h2><blockquote>quote</blockquote><table><tr><th>key</th><td>value</td></tr></table>`,
			want:    "Heading\n\nquote\n\nkey\n\nvalue",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := geminiAppsSingleCellHTML(
				"", "My Activity History", "Prompted",
				"Jan 2, 2025, 3:04:05 PM EDT", tt.content,
			)
			path := filepath.Join(t.TempDir(), "inline-spacing.html")
			require.NoError(t, os.WriteFile(path, []byte(fixture), 0o644))

			provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
			require.True(t, ok)
			exporter := provider.(GeminiAppsExportParser)
			var results []ParseResult
			_, err := exporter.ParseGeminiAppsExport(path, func(result ParseResult) error {
				results = append(results, result)
				return nil
			})
			require.NoError(t, err)
			require.Len(t, results, 1)
			require.Len(t, results[0].Messages, 1)
			assert.Equal(t, tt.want, results[0].Messages[0].Content)
		})
	}
}

func TestParseGeminiAppsBrStaysInsidePromptBlock(t *testing.T) {
	fixture := strings.Replace(
		sanitizedGeminiAppsHTML,
		`<div class="content-cell mdl-cell"><p>first prompt<br></p><p><strong>first</strong> answer &amp; detail</p><script>secret script</script><style>secret style</style><template>secret template</template><noscript>secret noscript</noscript></div>`,
		`<div class="content-cell mdl-cell"><p>line one<br>line two</p><p>answer</p></div>`,
		1,
	)
	path := filepath.Join(t.TempDir(), "br-inline.html")
	require.NoError(t, os.WriteFile(path, []byte(fixture), 0o644))

	provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
	require.True(t, ok)
	exporter := provider.(GeminiAppsExportParser)
	var results []ParseResult
	_, err := exporter.ParseGeminiAppsExport(path, func(result ParseResult) error {
		results = append(results, result)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, results, 2)
	require.Len(t, results[0].Messages, 1)
	assert.Equal(t, "line one\nline two\n\nanswer", results[0].Messages[0].Content)
	assert.Contains(t, results[0].Messages[0].Content, "answer")
}

func TestParseGeminiAppsEmptyFirstContentBlockIsError(t *testing.T) {
	fixture := geminiAppsSingleCellHTML(
		"", "My Activity History", "Prompted",
		"Jan 2, 2025, 3:04:05 PM EDT", "<p></p><p>answer</p>",
	)
	path := filepath.Join(t.TempDir(), "empty-first-block.html")
	require.NoError(t, os.WriteFile(path, []byte(fixture), 0o644))

	provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
	require.True(t, ok)
	exporter := provider.(GeminiAppsExportParser)
	var results []ParseResult
	summary, err := exporter.ParseGeminiAppsExport(path, func(result ParseResult) error {
		results = append(results, result)
		return nil
	})
	assert.NoError(t, err)
	assert.Len(t, results, 1)
	assert.Equal(t, 0, summary.Errors)
}

func TestParseGeminiAppsEmptySemanticBlockAfterPromptIsError(t *testing.T) {
	fixture := geminiAppsSingleCellHTML(
		"", "My Activity History", "Prompted",
		"Jan 2, 2025, 3:04:05 PM EDT", "<p>prompt</p><p></p>",
	)
	path := filepath.Join(t.TempDir(), "empty-response-block.html")
	require.NoError(t, os.WriteFile(path, []byte(fixture), 0o644))

	provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
	require.True(t, ok)
	exporter := provider.(GeminiAppsExportParser)
	callbacks := 0
	_, err := exporter.ParseGeminiAppsExport(path, func(ParseResult) error {
		callbacks++
		return nil
	})
	assert.NoError(t, err)
	assert.Equal(t, 1, callbacks)
}

func TestParseGeminiAppsEmptyCodeBlockAfterPromptIsError(t *testing.T) {
	fixture := geminiAppsSingleCellHTML(
		"", "My Activity History", "Prompted",
		"Jan 2, 2025, 3:04:05 PM EDT", "<p>prompt</p><code></code>",
	)
	path := filepath.Join(t.TempDir(), "empty-code-block.html")
	require.NoError(t, os.WriteFile(path, []byte(fixture), 0o644))

	provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
	require.True(t, ok)
	exporter := provider.(GeminiAppsExportParser)
	callbacks := 0
	_, err := exporter.ParseGeminiAppsExport(path, func(ParseResult) error {
		callbacks++
		return nil
	})
	assert.NoError(t, err)
	assert.Equal(t, 1, callbacks)
}

func TestParseGeminiAppsPreservesTimestampTextInContentBlocks(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "prompt",
			content: "<p>Meeting at Jan 2, 2025, 3:04:05 PM EDT is confirmed.</p><p>answer</p>",
			want:    "Meeting at Jan 2, 2025, 3:04:05 PM EDT is confirmed.\n\nanswer",
		},
		{
			name:    "response",
			content: "<p>prompt</p><p>The old log says Jan 2, 2025, 3:04:05 PM EDT.</p>",
			want:    "prompt\n\nThe old log says Jan 2, 2025, 3:04:05 PM EDT.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "timestamp-text.html")
			fixture := geminiAppsSingleCellHTML(
				"", "My Activity History", "Prompted",
				"Jan 2, 2025, 3:04:05 PM EDT", tt.content,
			)
			require.NoError(t, os.WriteFile(path, []byte(fixture), 0o644))

			provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
			require.True(t, ok)
			exporter := provider.(GeminiAppsExportParser)
			var results []ParseResult
			_, err := exporter.ParseGeminiAppsExport(path, func(result ParseResult) error {
				results = append(results, result)
				return nil
			})
			require.NoError(t, err)
			require.Len(t, results, 1)
			require.Len(t, results[0].Messages, 1)
			assert.Equal(t, tt.want, results[0].Messages[0].Content)
		})
	}
}

func TestParseGeminiAppsExcludesExactContentTimestampMetadata(t *testing.T) {
	fixture := geminiAppsSingleCellHTML(
		"", "My Activity History", "Prompted",
		"Jan 2, 2025, 3:04:05 PM EDT",
		"<p>prompt</p><p>Jan 2, 2025, 3:04:05 PM EDT</p><p>answer</p>",
	)
	path := filepath.Join(t.TempDir(), "timestamp-metadata.html")
	require.NoError(t, os.WriteFile(path, []byte(fixture), 0o644))

	provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
	require.True(t, ok)
	exporter := provider.(GeminiAppsExportParser)
	var results []ParseResult
	_, err := exporter.ParseGeminiAppsExport(path, func(result ParseResult) error {
		results = append(results, result)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Len(t, results[0].Messages, 1)
	assert.Equal(t, "prompt\n\nanswer", results[0].Messages[0].Content)
}

func TestParseGeminiAppsEmptyRecordPayloadIsError(t *testing.T) {
	fixture := geminiAppsSingleCellHTML(
		"", "My Activity History", "Prompted",
		"Jan 2, 2025, 3:04:05 PM EDT",
		"<p>Jan 2, 2025, 3:04:05 PM EDT</p>",
	)
	path := filepath.Join(t.TempDir(), "sole-timestamp-metadata.html")
	require.NoError(t, os.WriteFile(path, []byte(fixture), 0o644))

	provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
	require.True(t, ok)
	exporter := provider.(GeminiAppsExportParser)
	var results []ParseResult
	summary, err := exporter.ParseGeminiAppsExport(path, func(result ParseResult) error {
		results = append(results, result)
		return nil
	})
	assert.ErrorContains(t, err, "no admissible Prompted records")
	assert.Empty(t, results)
	assert.Equal(t, 1, summary.Errors)
}

func TestParseGeminiAppsExcludesStandaloneTimestampMetadataAcrossDirectNodes(t *testing.T) {
	timestamp := "Jan 2, 2025, 3:04:05 PM EDT"
	for _, content := range []string{
		timestamp + "<p>prompt</p>",
		"<span>" + timestamp + "</span><p>prompt</p>",
		"<code>" + timestamp + "</code><p>prompt</p>",
	} {
		t.Run(content, func(t *testing.T) {
			fixture := geminiAppsSingleCellHTML("", "My Activity History", "Prompted", timestamp, content)
			path := filepath.Join(t.TempDir(), "direct-timestamp-metadata.html")
			require.NoError(t, os.WriteFile(path, []byte(fixture), 0o644))

			provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
			require.True(t, ok)
			exporter := provider.(GeminiAppsExportParser)
			var results []ParseResult
			_, err := exporter.ParseGeminiAppsExport(path, func(result ParseResult) error {
				results = append(results, result)
				return nil
			})
			require.NoError(t, err)
			require.Len(t, results, 1)
			assert.Equal(t, "prompt", results[0].Messages[0].Content)
		})
	}
}

func TestParseGeminiAppsInlineCodeRemainsOneRecordMessage(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name: "code prompt", content: "<code>prompt</code><p>answer</p>",
			want: "prompt\nanswer",
		},
		{
			name: "code response", content: "<p>prompt</p><code>answer</code>",
			want: "prompt\nanswer",
		},
		{
			name: "inline code", content: "<p>Use <code>go test</code> now</p>",
			want: "Use go test now",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := geminiAppsSingleCellHTML(
				"", "My Activity History", "Prompted",
				"Jan 2, 2025, 3:04:05 PM EDT", tt.content,
			)
			path := filepath.Join(t.TempDir(), "direct-code.html")
			require.NoError(t, os.WriteFile(path, []byte(fixture), 0o644))

			provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
			require.True(t, ok)
			exporter := provider.(GeminiAppsExportParser)
			var results []ParseResult
			_, err := exporter.ParseGeminiAppsExport(path, func(result ParseResult) error {
				results = append(results, result)
				return nil
			})
			require.NoError(t, err)
			require.Len(t, results, 1)
			require.Len(t, results[0].Messages, 1)
			assert.Equal(t, tt.want, results[0].Messages[0].Content)
		})
	}
}

func TestParseGeminiAppsPreformattedCodeIsPlainText(t *testing.T) {
	content := "<p>prompt</p><pre><code>  line one\n\tline  two  \n`one` and ```three```  \n</code></pre><p>inline <code>  x  </code></p>"
	fixture := geminiAppsSingleCellHTML(
		"", "My Activity History", "Prompted",
		"Jan 2, 2025, 3:04:05 PM EDT", content,
	)
	path := filepath.Join(t.TempDir(), "preformatted.html")
	require.NoError(t, os.WriteFile(path, []byte(fixture), 0o644))

	provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
	require.True(t, ok)
	exporter := provider.(GeminiAppsExportParser)
	var results []ParseResult
	_, err := exporter.ParseGeminiAppsExport(path, func(result ParseResult) error {
		results = append(results, result)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Len(t, results[0].Messages, 1)
	assert.Equal(t, "prompt\n\n  line one\n\tline  two  \n`one` and ```three```  \n\n\ninline x", results[0].Messages[0].Content)
}

func TestParseGeminiAppsPreservesBoundaryPreformattedWhitespace(t *testing.T) {
	content := "<pre><code>  leading\n\tbody\n  </code></pre>"
	fixture := geminiAppsSingleCellHTML("", "My Activity History", "Prompted", "Jan 2, 2025, 3:04:05 PM EDT", content)
	path := filepath.Join(t.TempDir(), "boundary-preformatted.html")
	require.NoError(t, os.WriteFile(path, []byte(fixture), 0o644))

	provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
	require.True(t, ok)
	exporter := provider.(GeminiAppsExportParser)
	var results []ParseResult
	_, err := exporter.ParseGeminiAppsExport(path, func(result ParseResult) error {
		results = append(results, result)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "  leading\n\tbody\n  ", results[0].Messages[0].Content)
}

func TestParseGeminiAppsNormalizesOrdinaryWhitespace(t *testing.T) {
	fixture := geminiAppsSingleCellHTML(
		"", "My Activity History", "Prompted",
		"Jan 2, 2025, 3:04:05 PM EDT", "<p>  prompt   with  gaps </p><p> answer </p>",
	)
	path := filepath.Join(t.TempDir(), "ordinary-whitespace.html")
	require.NoError(t, os.WriteFile(path, []byte(fixture), 0o644))

	provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
	require.True(t, ok)
	exporter := provider.(GeminiAppsExportParser)
	var results []ParseResult
	_, err := exporter.ParseGeminiAppsExport(path, func(result ParseResult) error {
		results = append(results, result)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Len(t, results[0].Messages, 1)
	assert.Equal(t, "prompt with gaps\n\nanswer", results[0].Messages[0].Content)
}

func TestParseGeminiAppsRejectsDeclaredNonEnglishLocale(t *testing.T) {
	fixture := geminiAppsSingleCellHTML(
		` lang="de"`, "My Activity History", "Prompted",
		"Jan 2, 2025, 3:04:05 PM EDT", "<p>prompt</p>",
	)
	path := filepath.Join(t.TempDir(), "localized.html")
	require.NoError(t, os.WriteFile(path, []byte(fixture), 0o644))

	provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
	require.True(t, ok)
	exporter := provider.(GeminiAppsExportParser)
	callbacks := 0
	_, err := exporter.ParseGeminiAppsExport(path, func(ParseResult) error {
		callbacks++
		return nil
	})
	assert.ErrorContains(t, err, "unsupported Gemini Apps Takeout locale")
	assert.Zero(t, callbacks)
}

func TestParseGeminiAppsRejectsUnsupportedVocabularyWithoutLang(t *testing.T) {
	fixture := geminiAppsSingleCellHTML(
		"", "Meine Aktivität", "Prompted",
		"Jan 2, 2025, 3:04:05 PM EDT", "<p>prompt</p>",
	)
	path := filepath.Join(t.TempDir(), "unsupported-vocabulary.html")
	require.NoError(t, os.WriteFile(path, []byte(fixture), 0o644))

	provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
	require.True(t, ok)
	exporter := provider.(GeminiAppsExportParser)
	callbacks := 0
	_, err := exporter.ParseGeminiAppsExport(path, func(ParseResult) error {
		callbacks++
		return nil
	})
	assert.ErrorContains(t, err, "unsupported localized or changed Gemini Apps Takeout format")
	assert.Zero(t, callbacks)
}

func TestParseGeminiAppsSkipsUnknownCompatibleActivityWithoutLang(t *testing.T) {
	fixture := geminiAppsSingleCellHTML(
		"", "My Activity History", "Angefragt",
		"Jan 2, 2025, 3:04:05 PM EDT", "<p>prompt</p>",
	)
	path := filepath.Join(t.TempDir(), "unsupported-label.html")
	require.NoError(t, os.WriteFile(path, []byte(fixture), 0o644))

	provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
	require.True(t, ok)
	exporter := provider.(GeminiAppsExportParser)
	callbacks := 0
	_, err := exporter.ParseGeminiAppsExport(path, func(ParseResult) error {
		callbacks++
		return nil
	})
	assert.ErrorContains(t, err, "no admissible Prompted records")
	assert.Zero(t, callbacks)
}

func TestParseGeminiAppsPreflightsUnsupportedCandidateBeforeCallback(t *testing.T) {
	root := t.TempDir()
	supported := geminiAppsSingleCellHTML(
		"", "My Activity History", "Prompted",
		"Jan 2, 2025, 3:04:05 PM EDT", "<p>prompt</p>",
	)
	unsupported := geminiAppsSingleCellHTML(
		` lang="de"`, "My Activity History", "Prompted",
		"Jan 2, 2025, 3:04:05 PM EDT", "<p>prompt</p>",
	)
	require.NoError(t, os.WriteFile(filepath.Join(root, "01-supported.html"), []byte(supported), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "02-unsupported.html"), []byte(unsupported), 0o644))

	provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
	require.True(t, ok)
	exporter := provider.(GeminiAppsExportParser)
	callbacks := 0
	_, err := exporter.ParseGeminiAppsExport(root, func(ParseResult) error {
		callbacks++
		return nil
	})
	assert.ErrorContains(t, err, "unsupported Gemini Apps Takeout locale")
	assert.Zero(t, callbacks)
}

func TestParseGeminiAppsPreflightsUnsupportedCellBeforeCallback(t *testing.T) {
	supported := geminiAppsSingleCellHTML(
		"", "My Activity History", "Prompted",
		"Jan 2, 2025, 3:04:05 PM EDT", "<p>prompt</p>",
	)
	unsupportedCell := `<div class="outer-cell"><div class="header-cell"><h3>Gemini Apps</h3><p>Prompted</p><p>2. Januar 2025, 3:04:05 PM MEZ</p></div><div class="content-cell"><p>prompt</p></div></div>`
	fixture := strings.Replace(supported, "</body>", unsupportedCell+"</body>", 1)
	path := filepath.Join(t.TempDir(), "mixed.html")
	require.NoError(t, os.WriteFile(path, []byte(fixture), 0o644))

	provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
	require.True(t, ok)
	exporter := provider.(GeminiAppsExportParser)
	callbacks := 0
	_, err := exporter.ParseGeminiAppsExport(path, func(ParseResult) error {
		callbacks++
		return nil
	})
	assert.Error(t, err)
	assert.Zero(t, callbacks)
}

func TestParseGeminiAppsPreflightsUnknownZoneBeforeCallback(t *testing.T) {
	supported := geminiAppsSingleCellHTML(
		"", "My Activity History", "Prompted",
		"Jan 2, 2025, 3:04:05 PM EDT", "<p>prompt</p>",
	)
	unsupportedCell := `<div class="outer-cell"><div class="header-cell"><h3>Gemini Apps</h3><p>Prompted</p><p>Jan 3, 2025, 3:04:05 PM XYZ</p></div><div class="content-cell"><p>prompt</p></div></div>`
	fixture := strings.Replace(supported, "</body>", unsupportedCell+"</body>", 1)
	path := filepath.Join(t.TempDir(), "unknown-zone.html")
	require.NoError(t, os.WriteFile(path, []byte(fixture), 0o644))

	provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
	require.True(t, ok)
	exporter := provider.(GeminiAppsExportParser)
	callbacks := 0
	_, err := exporter.ParseGeminiAppsExport(path, func(ParseResult) error {
		callbacks++
		return nil
	})
	assert.Error(t, err)
	assert.Zero(t, callbacks)
}

func TestParseGeminiAppsPreflightsUnknownZoneFileBeforeCallback(t *testing.T) {
	root := t.TempDir()
	supported := geminiAppsSingleCellHTML(
		"", "My Activity History", "Prompted",
		"Jan 2, 2025, 3:04:05 PM EDT", "<p>prompt</p>",
	)
	unsupported := geminiAppsSingleCellHTML(
		"", "My Activity History", "Prompted",
		"Jan 3, 2025, 3:04:05 PM XYZ", "<p>prompt</p>",
	)
	require.NoError(t, os.WriteFile(filepath.Join(root, "01-supported.html"), []byte(supported), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "02-unsupported.html"), []byte(unsupported), 0o644))

	provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
	require.True(t, ok)
	exporter := provider.(GeminiAppsExportParser)
	callbacks := 0
	_, err := exporter.ParseGeminiAppsExport(root, func(ParseResult) error {
		callbacks++
		return nil
	})
	assert.Error(t, err)
	assert.Zero(t, callbacks)
}

func TestParseGeminiAppsSkipsUnknownCompatibleActivityLabels(t *testing.T) {
	for _, label := range []string{"Nicht Prompted", "Unknown Ereignis", "Prompted extra"} {
		t.Run(label, func(t *testing.T) {
			fixture := geminiAppsSingleCellHTML(
				"", "My Activity History", label,
				"Jan 2, 2025, 3:04:05 PM EDT", "<p>prompt</p>",
			)
			path := filepath.Join(t.TempDir(), "unsupported-label.html")
			require.NoError(t, os.WriteFile(path, []byte(fixture), 0o644))

			provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
			require.True(t, ok)
			exporter := provider.(GeminiAppsExportParser)
			callbacks := 0
			_, err := exporter.ParseGeminiAppsExport(path, func(ParseResult) error {
				callbacks++
				return nil
			})
			assert.ErrorContains(t, err, "no admissible Prompted records")
			assert.Zero(t, callbacks)
		})
	}
}

func TestParseGeminiAppsTimestampMustBeInHeader(t *testing.T) {
	fixture := strings.Replace(
		sanitizedGeminiAppsHTML,
		"<p>Jan 2, 2025, 3:04:05 PM EDT</p>",
		"<p>header timestamp missing</p>",
		1,
	)
	fixture = strings.Replace(
		fixture,
		"<p>first prompt<br></p>",
		"<p>Jan 2, 2025, 3:04:05 PM EDT</p><p>first prompt</p>",
		1,
	)
	path := filepath.Join(t.TempDir(), "content-date.html")
	require.NoError(t, os.WriteFile(path, []byte(fixture), 0o644))

	provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
	require.True(t, ok)
	exporter := provider.(GeminiAppsExportParser)
	var results []ParseResult
	summary, err := exporter.ParseGeminiAppsExport(path, func(result ParseResult) error {
		results = append(results, result)
		return nil
	})
	require.NoError(t, err)
	assert.Len(t, results, 1)
	assert.Equal(t, 1, summary.Errors)
	assert.Equal(t, "second prompt\n\nsecond answer", results[0].Messages[0].Content)
}

func TestParseGeminiAppsExportAdmitsDirectoryAndRejectsOtherHTML(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "other.html"),
		[]byte("<html><head><title>Other activity</title></head></html>"),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "activity.html"),
		[]byte(sanitizedGeminiAppsHTML),
		0o644,
	))

	provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
	require.True(t, ok)
	exporter := provider.(GeminiAppsExportParser)
	var count int
	_, err := exporter.ParseGeminiAppsExport(root, func(ParseResult) error {
		count++
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 2, count)

	other := filepath.Join(t.TempDir(), "other.html")
	require.NoError(t, os.WriteFile(
		other,
		[]byte("<html><head><title>Other activity</title></head></html>"),
		0o644,
	))
	_, err = exporter.ParseGeminiAppsExport(other, func(ParseResult) error { return nil })
	assert.ErrorContains(t, err, "does not contain a Gemini Apps")
}

func TestParseGeminiAppsTimestampUsesExplicitZones(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{"edt", "Jan 2, 2025, 3:04:05 PM EDT", "2025-01-02T19:04:05Z"},
		{"pst", "Jan 2, 2025, 3:04:05 PM PST", "2025-01-02T23:04:05Z"},
		{"numeric", "Jan 2, 2025, 3:04:05 PM GMT+05:30", "2025-01-02T09:34:05Z"},
		{"negative-zero", "Jan 2, 2025, 3:04:05 PM GMT-00:30", "2025-01-02T15:34:05Z"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			match := geminiAppsTimestampRE.FindStringSubmatch(tt.text)
			require.Len(t, match, 2)
			parsed, err := parseGeminiAppsTimestamp(match[0], match[1])
			require.NoError(t, err)
			assert.Equal(t, tt.want, parsed.UTC().Format("2006-01-02T15:04:05Z"))
		})
	}

	match := geminiAppsTimestampRE.FindStringSubmatch(
		"Jan 2, 2025, 3:04:05 PM XYZ",
	)
	require.Len(t, match, 2)
	_, err := parseGeminiAppsTimestamp(match[0], match[1])
	assert.ErrorContains(t, err, "unsupported")
}

func TestParseGeminiAppsMissingContentCellIsCountedError(t *testing.T) {
	provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
	require.True(t, ok)
	exporter := provider.(GeminiAppsExportParser)
	withoutContent := strings.Replace(
		sanitizedGeminiAppsHTML,
		`  <div class="content-cell mdl-cell"><p>first prompt<br></p><p><strong>first</strong> answer &amp; detail</p><script>secret script</script><style>secret style</style><template>secret template</template><noscript>secret noscript</noscript></div>
`,
		"",
		1,
	)
	path := filepath.Join(t.TempDir(), "missing-content.html")
	require.NoError(t, os.WriteFile(path, []byte(withoutContent), 0o644))
	var results []ParseResult
	summary, err := exporter.ParseGeminiAppsExport(path, func(result ParseResult) error {
		results = append(results, result)
		return nil
	})
	require.NoError(t, err)
	assert.Len(t, results, 1)
	assert.Equal(t, 3, summary.Skipped)
	assert.Equal(t, 1, summary.Errors)
}

func TestParseGeminiAppsTextDropsC0DELAndC1Controls(t *testing.T) {
	provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
	require.True(t, ok)
	exporter := provider.(GeminiAppsExportParser)
	fixture := strings.Replace(
		sanitizedGeminiAppsHTML,
		"first answer",
		"first\x7f\u0085\u009b answer",
		1,
	)
	path := filepath.Join(t.TempDir(), "controls.html")
	require.NoError(t, os.WriteFile(path, []byte(fixture), 0o644))
	var results []ParseResult
	_, err := exporter.ParseGeminiAppsExport(path, func(result ParseResult) error {
		results = append(results, result)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, "first prompt\n\nfirst answer & detail", results[0].Messages[0].Content)
}

func TestParseGeminiAppsZeroRecordsAndUnknownLabel(t *testing.T) {
	provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
	require.True(t, ok)
	exporter := provider.(GeminiAppsExportParser)

	path := filepath.Join(t.TempDir(), "empty.html")
	require.NoError(t, os.WriteFile(path, []byte(
		`<html><head><title>My Activity History</title></head><body></body></html>`,
	), 0o644))
	_, err := exporter.ParseGeminiAppsExport(path, func(ParseResult) error { return nil })
	assert.ErrorContains(t, err, "does not contain a Gemini Apps")

	unknown := strings.ReplaceAll(
		sanitizedGeminiAppsHTML,
		"<p>Unknown activity</p>",
		"<p>Unrecognized activity</p>",
	)
	path = filepath.Join(t.TempDir(), "unknown.html")
	require.NoError(t, os.WriteFile(path, []byte(unknown), 0o644))
	var count int
	summary, err := exporter.ParseGeminiAppsExport(path, func(ParseResult) error {
		count++
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 2, count)
	assert.Equal(t, 3, summary.Skipped)
}
