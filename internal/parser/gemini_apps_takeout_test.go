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
	return `<!doctype html><html` + lang + `><head><title>` + title + `</title></head><body>
<div class="outer-cell"><div class="header-cell"><h3>Gemini Apps</h3><p>` + label + `</p><p>` + timestamp + `</p></div><div class="content-cell">` + content + `</div></div>
</body></html>`
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
	assert.Equal(t, "first prompt", results[0].Messages[0].Content)
	assert.Equal(t, RoleUser, results[0].Messages[0].Role)
	assert.Equal(t, RoleAssistant, results[0].Messages[1].Role)
	assert.Equal(t, "**first** answer & detail", results[0].Messages[1].Content)
	assert.NotContains(t, results[0].Messages[1].Content, "secret")
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
	require.Len(t, results[0].Messages, 2)
	assert.Equal(t, "line one\nline two", results[0].Messages[0].Content)
	assert.Equal(t, "answer", results[0].Messages[1].Content)
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
	assert.ErrorContains(t, err, "no admissible Prompted records")
	assert.Empty(t, results)
	assert.Equal(t, 1, summary.Errors)
}

func TestParseGeminiAppsPreservesTimestampTextInContentBlocks(t *testing.T) {
	tests := []struct {
		name         string
		content      string
		wantPrompt   string
		wantResponse string
	}{
		{
			name:         "prompt",
			content:      "<p>Meeting at Jan 2, 2025, 3:04:05 PM EDT is confirmed.</p><p>answer</p>",
			wantPrompt:   "Meeting at Jan 2, 2025, 3:04:05 PM EDT is confirmed.",
			wantResponse: "answer",
		},
		{
			name:         "response",
			content:      "<p>prompt</p><p>The old log says Jan 2, 2025, 3:04:05 PM EDT.</p>",
			wantPrompt:   "prompt",
			wantResponse: "The old log says Jan 2, 2025, 3:04:05 PM EDT.",
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
			require.Len(t, results[0].Messages, 2)
			assert.Equal(t, tt.wantPrompt, results[0].Messages[0].Content)
			assert.Equal(t, tt.wantResponse, results[0].Messages[1].Content)
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
	require.Len(t, results[0].Messages, 2)
	assert.Equal(t, "prompt", results[0].Messages[0].Content)
	assert.Equal(t, "answer", results[0].Messages[1].Content)
}

func TestParseGeminiAppsSoleTimestampMetadataIsError(t *testing.T) {
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

func TestParseGeminiAppsPreservesDirectCodeBoundaryBlocks(t *testing.T) {
	tests := []struct {
		name         string
		content      string
		wantPrompt   string
		wantResponse string
	}{
		{
			name:         "code prompt",
			content:      "<code>prompt</code><p>answer</p>",
			wantPrompt:   "`prompt`",
			wantResponse: "answer",
		},
		{
			name:         "code response",
			content:      "<p>prompt</p><code>answer</code>",
			wantPrompt:   "prompt",
			wantResponse: "`answer`",
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
			require.Len(t, results[0].Messages, 2)
			assert.Equal(t, tt.wantPrompt, results[0].Messages[0].Content)
			assert.Equal(t, tt.wantResponse, results[0].Messages[1].Content)
		})
	}
}

func TestParseGeminiAppsPreservesPreformattedWhitespace(t *testing.T) {
	content := "<p>prompt</p><pre><code>  line one\n\tline  two  \n</code></pre><p>inline <code>  x  </code></p>"
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
	require.Len(t, results[0].Messages, 2)
	assert.Equal(t, "prompt", results[0].Messages[0].Content)
	assert.Equal(t, "`  line one\n\tline  two  \n`\n\ninline `  x  `", results[0].Messages[1].Content)
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
	require.Len(t, results[0].Messages, 2)
	assert.Equal(t, "prompt with gaps", results[0].Messages[0].Content)
	assert.Equal(t, "answer", results[0].Messages[1].Content)
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

func TestParseGeminiAppsRejectsUnsupportedActivityVocabularyWithoutLang(t *testing.T) {
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
	assert.ErrorContains(t, err, "unsupported localized or changed Gemini Apps Takeout format")
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
	assert.ErrorContains(t, err, "unsupported localized or changed Gemini Apps Takeout format")
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
	assert.ErrorContains(t, err, "unsupported localized or changed Gemini Apps Takeout format")
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
	assert.ErrorContains(t, err, "unsupported localized or changed Gemini Apps Takeout format")
	assert.Zero(t, callbacks)
}

func TestParseGeminiAppsRejectsUnsupportedActivityLabelVocabulary(t *testing.T) {
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
			assert.ErrorContains(t, err, "unsupported localized or changed Gemini Apps Takeout format")
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
	assert.Equal(t, "second prompt", results[0].Messages[0].Content)
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
	assert.Equal(t, "**first** answer & detail", results[0].Messages[1].Content)
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
