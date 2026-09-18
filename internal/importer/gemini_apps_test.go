package importer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

const sanitizedGeminiAppsImportHTML = `<!doctype html>
<html><head><title>My Activity History</title></head><body>
<div class="outer-cell"><div class="header-cell"><h3>Gemini Apps</h3><p>Prompted</p><p>Jan 2, 2025, 3:04:05 PM EDT</p></div><div class="content-cell"><p>first prompt</p><p>first answer</p></div></div>
<div class="outer-cell"><div class="header-cell"><h3>Gemini Apps</h3><p>Canvas</p><p>Jan 3, 2025, 3:04:05 PM PST</p></div><div class="content-cell"><p>canvas</p></div></div>
<div class="outer-cell"><div class="header-cell"><h3>Gemini Apps</h3><p>Feedback</p><p>Jan 4, 2025, 3:04:05 PM PST</p></div><div class="content-cell"><p>feedback</p></div></div>
<div class="outer-cell"><div class="header-cell"><h3>Gemini Apps</h3><p>Prompted</p><p>Jan 5, 2025, 3:04:05 PM PST</p></div><div class="content-cell"><p>second prompt</p><p>second answer</p></div></div>
<div class="outer-cell"><div class="header-cell"><h3>Gemini Apps</h3><p>Unknown</p><p>Jan 6, 2025, 3:04:05 PM PST</p></div><div class="content-cell"><p>unknown</p></div></div>
</body></html>`

func TestImportGeminiApps(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	path := filepath.Join(root, "activity.html")
	require.NoError(os.WriteFile(
		path, []byte(sanitizedGeminiAppsImportHTML), 0o644,
	))

	d := testDB(t)
	stats, err := ImportGeminiApps(
		t.Context(), d, root, nil, "test-machine",
	)
	require.NoError(err)
	assert.Equal(2, stats.Imported)
	assert.Equal(0, stats.Updated)
	assert.Equal(3, stats.Skipped)
	assert.Zero(stats.Errors)

	sessions, err := d.ListSessions(t.Context(), db.SessionFilter{Agent: "gemini-apps"})
	require.NoError(err)
	assert.Len(sessions.Sessions, 2)
	assert.Equal("test-machine", sessions.Sessions[0].Machine)
}

func TestImportGeminiAppsIgnoresOtherProductsThroughPublicPath(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	gemini := `<div class="outer-cell"><div class="header-cell"><h3>Gemini Apps</h3><p>Prompted</p><p>Jan 2, 2025, 3:04:05 PM EDT</p></div><div class="content-cell"><p>See <a href="https://example.invalid">source</a> for details</p></div></div>`
	other := `<div class="outer-cell"><div class="header-cell"><h3>YouTube</h3><p>Watched</p><p>Jan 2, 2025, 3:04:05 PM XYZ</p></div><div class="content-cell"><p>video title</p></div></div>`
	path := filepath.Join(root, "activity.html")
	require.NoError(os.WriteFile(
		path,
		[]byte(`<!doctype html><html><head><title>My Activity History</title></head><body>`+gemini+other+`</body></html>`),
		0o644,
	))

	d := testDB(t)
	stats, err := ImportGeminiApps(t.Context(), d, root, nil)
	require.NoError(err)
	assert.Equal(1, stats.Imported)
	assert.Zero(stats.Skipped)
	assert.Zero(stats.Errors)

	page, err := d.ListSessions(t.Context(), db.SessionFilter{Agent: "gemini-apps"})
	require.NoError(err)
	require.Len(page.Sessions, 1)
	messages, err := d.GetAllMessages(t.Context(), page.Sessions[0].ID)
	require.NoError(err)
	require.Len(messages, 1)
	assert.Equal("See source for details", messages[0].Content)
}

func TestImportGeminiAppsReimportUpdatesResponseWithStableID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	path := filepath.Join(root, "activity.html")
	initial := strings.ReplaceAll(
		sanitizedGeminiAppsImportHTML,
		`<div class="outer-cell"><div class="header-cell"><h3>Gemini Apps</h3><p>Canvas</p><p>Jan 3, 2025, 3:04:05 PM PST</p></div><div class="content-cell"><p>canvas</p></div></div>`,
		"",
	)
	initial = strings.ReplaceAll(initial,
		`<div class="outer-cell"><div class="header-cell"><h3>Gemini Apps</h3><p>Feedback</p><p>Jan 4, 2025, 3:04:05 PM PST</p></div><div class="content-cell"><p>feedback</p></div></div>`,
		"",
	)
	initial = strings.ReplaceAll(initial,
		`<div class="outer-cell"><div class="header-cell"><h3>Gemini Apps</h3><p>Prompted</p><p>Jan 5, 2025, 3:04:05 PM PST</p></div><div class="content-cell"><p>second prompt</p><p>second answer</p></div></div>`,
		"",
	)
	initial = strings.ReplaceAll(initial,
		`<div class="outer-cell"><div class="header-cell"><h3>Gemini Apps</h3><p>Unknown</p><p>Jan 6, 2025, 3:04:05 PM PST</p></div><div class="content-cell"><p>unknown</p></div></div>`,
		"",
	)
	require.NoError(os.WriteFile(path, []byte(initial), 0o644))

	d := testDB(t)
	first, err := ImportGeminiApps(t.Context(), d, root, nil)
	require.NoError(err)
	assert.Equal(1, first.Imported)
	initialPage, err := d.ListSessions(t.Context(), db.SessionFilter{Agent: "gemini-apps"})
	require.NoError(err)
	require.Len(initialPage.Sessions, 1)
	initialID := initialPage.Sessions[0].ID

	updated := strings.ReplaceAll(initial, "first answer", "updated answer")
	require.NoError(os.WriteFile(path, []byte(updated), 0o644))
	second, err := ImportGeminiApps(t.Context(), d, root, nil)
	require.NoError(err)
	assert.Equal(0, second.Imported)
	assert.Equal(1, second.Updated)
	assert.Zero(second.Skipped)

	page, err := d.ListSessions(t.Context(), db.SessionFilter{Agent: "gemini-apps"})
	require.NoError(err)
	require.Len(page.Sessions, 1)
	assert.Equal(initialID, page.Sessions[0].ID)
	messages, err := d.GetAllMessages(t.Context(), page.Sessions[0].ID)
	require.NoError(err)
	require.Len(messages, 1)
	assert.Equal("first prompt\n\nupdated answer", messages[0].Content)
}

func geminiAppsImportPromptedCell(timestamp, content string) string {
	return `<div class="outer-cell"><div class="header-cell"><h3>Gemini Apps</h3><p>Prompted</p><p>` + timestamp +
		`</p></div><div class="content-cell"><p>` + content + `</p></div></div>`
}

func geminiAppsImportDocument(cells ...string) string {
	return `<!doctype html><html><head><title>My Activity History</title></head><body>` +
		strings.Join(cells, "") + `</body></html>`
}

func geminiAppsSessionIDs(t *testing.T, d *db.DB) map[string]bool {
	t.Helper()
	page, err := d.ListSessions(t.Context(), db.SessionFilter{Agent: "gemini-apps"})
	require.NoError(t, err)
	ids := make(map[string]bool, len(page.Sessions))
	for _, session := range page.Sessions {
		ids[session.ID] = true
	}
	return ids
}

func TestImportGeminiAppsReimportAfterPrependKeepsExistingSessions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	path := filepath.Join(root, "activity.html")
	first := geminiAppsImportPromptedCell("Jan 2, 2025, 3:04:05 PM EDT", "first")
	second := geminiAppsImportPromptedCell("Jan 3, 2025, 3:04:05 PM EDT", "second")
	prepended := geminiAppsImportPromptedCell("Jan 1, 2025, 3:04:05 PM EDT", "prepended")
	require.NoError(os.WriteFile(path, []byte(geminiAppsImportDocument(first, second)), 0o644))

	d := testDB(t)
	initial, err := ImportGeminiApps(t.Context(), d, root, nil)
	require.NoError(err)
	assert.Equal(2, initial.Imported)
	initialIDs := geminiAppsSessionIDs(t, d)

	require.NoError(os.WriteFile(path, []byte(geminiAppsImportDocument(prepended, first, second)), 0o644))
	secondImport, err := ImportGeminiApps(t.Context(), d, root, nil)
	require.NoError(err)
	assert.Equal(1, secondImport.Imported)
	assert.Zero(secondImport.Updated)
	assert.Equal(2, secondImport.Skipped)
	assert.Zero(secondImport.Errors)

	page, err := d.ListSessions(t.Context(), db.SessionFilter{Agent: "gemini-apps"})
	require.NoError(err)
	assert.Len(page.Sessions, 3)
	finalIDs := geminiAppsSessionIDs(t, d)
	for id := range initialIDs {
		assert.True(finalIDs[id])
	}
}

func TestImportGeminiAppsReimportAfterReorderKeepsExistingSessions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	path := filepath.Join(root, "activity.html")
	first := geminiAppsImportPromptedCell("Jan 2, 2025, 3:04:05 PM EDT", "first")
	second := geminiAppsImportPromptedCell("Jan 3, 2025, 3:04:05 PM EDT", "second")
	require.NoError(os.WriteFile(path, []byte(geminiAppsImportDocument(first, second)), 0o644))

	d := testDB(t)
	initial, err := ImportGeminiApps(t.Context(), d, root, nil)
	require.NoError(err)
	assert.Equal(2, initial.Imported)
	initialIDs := geminiAppsSessionIDs(t, d)

	require.NoError(os.WriteFile(path, []byte(geminiAppsImportDocument(second, first)), 0o644))
	reordered, err := ImportGeminiApps(t.Context(), d, root, nil)
	require.NoError(err)
	assert.Zero(reordered.Imported)
	assert.Zero(reordered.Updated)
	assert.Equal(2, reordered.Skipped)
	assert.Zero(reordered.Errors)

	page, err := d.ListSessions(t.Context(), db.SessionFilter{Agent: "gemini-apps"})
	require.NoError(err)
	assert.Len(page.Sessions, 2)
	finalIDs := geminiAppsSessionIDs(t, d)
	for id := range initialIDs {
		assert.True(finalIDs[id])
	}
}

func TestImportGeminiAppsUnknownZoneDoesNotWriteSession(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	path := filepath.Join(root, "activity.html")
	html := strings.ReplaceAll(
		sanitizedGeminiAppsImportHTML,
		"EDT", "XYZ",
	)
	html = strings.ReplaceAll(html,
		`<div class="outer-cell"><div class="header-cell"><h3>Gemini Apps</h3><p>Canvas</p><p>Jan 3, 2025, 3:04:05 PM PST</p></div><div class="content-cell"><p>canvas</p></div></div>`,
		"",
	)
	html = strings.ReplaceAll(html,
		`<div class="outer-cell"><div class="header-cell"><h3>Gemini Apps</h3><p>Feedback</p><p>Jan 4, 2025, 3:04:05 PM PST</p></div><div class="content-cell"><p>feedback</p></div></div>`,
		"",
	)
	html = strings.ReplaceAll(html,
		`<div class="outer-cell"><div class="header-cell"><h3>Gemini Apps</h3><p>Prompted</p><p>Jan 5, 2025, 3:04:05 PM PST</p></div><div class="content-cell"><p>second prompt</p><p>second answer</p></div></div>`,
		"",
	)
	html = strings.ReplaceAll(html,
		`<div class="outer-cell"><div class="header-cell"><h3>Gemini Apps</h3><p>Unknown</p><p>Jan 6, 2025, 3:04:05 PM PST</p></div><div class="content-cell"><p>unknown</p></div></div>`,
		"",
	)
	require.NoError(os.WriteFile(path, []byte(html), 0o644))

	d := testDB(t)
	stats, err := ImportGeminiApps(t.Context(), d, root, nil)
	require.Error(err)
	assert.Zero(stats.Errors)
	page, listErr := d.ListSessions(t.Context(), db.SessionFilter{Agent: "gemini-apps"})
	require.NoError(listErr)
	assert.Empty(page.Sessions)
}

func TestImportGeminiAppsWholeHourZonePersistsCorrectTimestamp(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	path := filepath.Join(root, "activity.html")
	valid := geminiAppsImportPromptedCell(
		"Jan 2, 2025, 3:04:05 PM GMT+8", "prompt",
	)
	malformed := geminiAppsImportPromptedCell(
		"Jan 3, 2025, 3:04:05 PM GMT+8junk", "malformed",
	)
	require.NoError(os.WriteFile(path, []byte(geminiAppsImportDocument(valid)), 0o644))

	d := testDB(t)
	stats, err := ImportGeminiApps(t.Context(), d, root, nil)
	require.NoError(err)
	assert.Equal(1, stats.Imported)

	page, err := d.ListSessions(t.Context(), db.SessionFilter{Agent: "gemini-apps"})
	require.NoError(err)
	require.Len(page.Sessions, 1)
	stored, err := d.GetSessionFull(t.Context(), page.Sessions[0].ID)
	require.NoError(err)
	require.NotNil(stored.StartedAt)
	require.NotNil(stored.EndedAt)
	assert.Equal("2025-01-02T07:04:05Z", *stored.StartedAt)
	assert.Equal(*stored.StartedAt, *stored.EndedAt)
	messages, err := d.GetAllMessages(t.Context(), page.Sessions[0].ID)
	require.NoError(err)
	require.Len(messages, 1)
	assert.Equal(*stored.StartedAt, messages[0].Timestamp)

	require.NoError(os.WriteFile(path, []byte(geminiAppsImportDocument(malformed)), 0o644))
	stats, err = ImportGeminiApps(t.Context(), d, root, nil)
	require.Error(err)
	assert.Zero(stats.Imported)
	assert.Zero(stats.Updated)
	assert.Zero(stats.Errors)
	page, err = d.ListSessions(t.Context(), db.SessionFilter{Agent: "gemini-apps"})
	require.NoError(err)
	assert.Len(page.Sessions, 1)
}

func TestImportGeminiAppsPersistenceGuards(t *testing.T) {
	t.Run("unchanged and superset preserve revision", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		root := t.TempDir()
		path := filepath.Join(root, "activity.html")
		initial := strings.ReplaceAll(
			sanitizedGeminiAppsImportHTML,
			`<div class="outer-cell"><div class="header-cell"><h3>Gemini Apps</h3><p>Canvas</p><p>Jan 3, 2025, 3:04:05 PM PST</p></div><div class="content-cell"><p>canvas</p></div></div>`,
			"",
		)
		initial = strings.ReplaceAll(initial,
			`<div class="outer-cell"><div class="header-cell"><h3>Gemini Apps</h3><p>Feedback</p><p>Jan 4, 2025, 3:04:05 PM PST</p></div><div class="content-cell"><p>feedback</p></div></div>`,
			"",
		)
		initial = strings.ReplaceAll(initial,
			`<div class="outer-cell"><div class="header-cell"><h3>Gemini Apps</h3><p>Prompted</p><p>Jan 5, 2025, 3:04:05 PM PST</p></div><div class="content-cell"><p>second prompt</p><p>second answer</p></div></div>`,
			"",
		)
		initial = strings.ReplaceAll(initial,
			`<div class="outer-cell"><div class="header-cell"><h3>Gemini Apps</h3><p>Unknown</p><p>Jan 6, 2025, 3:04:05 PM PST</p></div><div class="content-cell"><p>unknown</p></div></div>`,
			"",
		)
		require.NoError(os.WriteFile(path, []byte(initial), 0o644))
		d := testDB(t)
		require.True(d.HasFTS())
		indexing := 0
		first, err := ImportGeminiApps(t.Context(), d, root, &ImportCallbacks{
			OnIndexing: func() { indexing++ },
		})
		require.NoError(err)
		assert.Equal(1, first.Imported)
		assert.Equal(1, indexing)

		page, err := d.ListSessions(t.Context(), db.SessionFilter{Agent: "gemini-apps"})
		require.NoError(err)
		require.Len(page.Sessions, 1)
		id := page.Sessions[0].ID
		stored, err := d.GetSessionFull(t.Context(), id)
		require.NoError(err)
		require.NotNil(stored.TranscriptRevision)
		revision := *stored.TranscriptRevision

		indexing = 0
		unchanged, err := ImportGeminiApps(t.Context(), d, root, &ImportCallbacks{
			OnIndexing: func() { indexing++ },
		})
		require.NoError(err)
		assert.Equal(1, unchanged.Skipped)
		assert.Zero(indexing)
		stored, err = d.GetSessionFull(t.Context(), id)
		require.NoError(err)
		require.NotNil(stored.TranscriptRevision)
		assert.Equal(revision, *stored.TranscriptRevision)

		require.NoError(os.WriteFile(path, []byte(sanitizedGeminiAppsImportHTML), 0o644))
		indexing = 0
		superset, err := ImportGeminiApps(t.Context(), d, root, &ImportCallbacks{
			OnIndexing: func() { indexing++ },
		})
		require.NoError(err)
		assert.Equal(1, superset.Imported)
		assert.Equal(4, superset.Skipped)
		assert.Equal(1, indexing)
		stored, err = d.GetSessionFull(t.Context(), id)
		require.NoError(err)
		require.NotNil(stored.TranscriptRevision)
		assert.Equal(revision, *stored.TranscriptRevision)
	})

	t.Run("excluded sessions stay excluded", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		root := t.TempDir()
		path := filepath.Join(root, "activity.html")
		fixture := strings.ReplaceAll(
			sanitizedGeminiAppsImportHTML,
			`<div class="outer-cell"><div class="header-cell"><h3>Gemini Apps</h3><p>Canvas</p><p>Jan 3, 2025, 3:04:05 PM PST</p></div><div class="content-cell"><p>canvas</p></div></div>`,
			"",
		)
		fixture = strings.ReplaceAll(fixture,
			`<div class="outer-cell"><div class="header-cell"><h3>Gemini Apps</h3><p>Feedback</p><p>Jan 4, 2025, 3:04:05 PM PST</p></div><div class="content-cell"><p>feedback</p></div></div>`,
			"",
		)
		fixture = strings.ReplaceAll(fixture,
			`<div class="outer-cell"><div class="header-cell"><h3>Gemini Apps</h3><p>Prompted</p><p>Jan 5, 2025, 3:04:05 PM PST</p></div><div class="content-cell"><p>second prompt</p><p>second answer</p></div></div>`,
			"",
		)
		fixture = strings.ReplaceAll(fixture,
			`<div class="outer-cell"><div class="header-cell"><h3>Gemini Apps</h3><p>Unknown</p><p>Jan 6, 2025, 3:04:05 PM PST</p></div><div class="content-cell"><p>unknown</p></div></div>`,
			"",
		)
		require.NoError(os.WriteFile(path, []byte(fixture), 0o644))
		d := testDB(t)
		_, err := ImportGeminiApps(t.Context(), d, root, nil)
		require.NoError(err)
		page, err := d.ListSessions(t.Context(), db.SessionFilter{Agent: "gemini-apps"})
		require.NoError(err)
		require.Len(page.Sessions, 1)
		id := page.Sessions[0].ID
		require.NoError(d.DeleteSession(id))
		assert.True(d.IsSessionExcluded(id))

		indexing := 0
		stats, err := ImportGeminiApps(t.Context(), d, root, &ImportCallbacks{
			OnIndexing: func() { indexing++ },
		})
		require.NoError(err)
		assert.Equal(1, stats.Skipped)
		assert.Zero(stats.Errors)
		assert.Zero(indexing)
		page, err = d.ListSessions(t.Context(), db.SessionFilter{Agent: "gemini-apps"})
		require.NoError(err)
		assert.Empty(page.Sessions)
	})
}
