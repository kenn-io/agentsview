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
<div class="outer-cell mdl-cell mdl-cell--12-col mdl-shadow--2dp"><div class="mdl-grid"><div class="header-cell mdl-cell mdl-cell--12-col"><p class="mdl-typography--title">Gemini Apps<br></p></div><div class="content-cell mdl-cell mdl-cell--6-col mdl-typography--body-1">Prompted&nbsp;first prompt<br>Jan 2, 2025, 3:04:05 PM EDT<br><p>first answer</p></div><div class="content-cell mdl-cell mdl-cell--12-col mdl-typography--caption"><b>Products:</b><br>&emsp;Gemini Apps<br></div></div></div>
<div class="outer-cell mdl-cell mdl-cell--12-col mdl-shadow--2dp"><div class="mdl-grid"><div class="header-cell mdl-cell mdl-cell--12-col"><p class="mdl-typography--title">Gemini Apps<br></p></div><div class="content-cell mdl-cell mdl-cell--6-col mdl-typography--body-1">Created a canvas<br>Jan 3, 2025, 3:04:05 PM PST<br><p>canvas</p></div><div class="content-cell mdl-cell mdl-cell--12-col mdl-typography--caption"><b>Products:</b><br>&emsp;Gemini Apps<br></div></div></div>
<div class="outer-cell mdl-cell mdl-cell--12-col mdl-shadow--2dp"><div class="mdl-grid"><div class="header-cell mdl-cell mdl-cell--12-col"><p class="mdl-typography--title">Gemini Apps<br></p></div><div class="content-cell mdl-cell mdl-cell--6-col mdl-typography--body-1">Gave feedback: Good response<br>Jan 4, 2025, 3:04:05 PM PST<br></div><div class="content-cell mdl-cell mdl-cell--12-col mdl-typography--caption"><b>Products:</b><br>&emsp;Gemini Apps<br></div></div></div>
<div class="outer-cell mdl-cell mdl-cell--12-col mdl-shadow--2dp"><div class="mdl-grid"><div class="header-cell mdl-cell mdl-cell--12-col"><p class="mdl-typography--title">Gemini Apps<br></p></div><div class="content-cell mdl-cell mdl-cell--6-col mdl-typography--body-1">Prompted&nbsp;second prompt<br>Jan 5, 2025, 3:04:05 PM PST<br><p>second answer</p></div><div class="content-cell mdl-cell mdl-cell--12-col mdl-typography--caption"><b>Products:</b><br>&emsp;Gemini Apps<br></div></div></div>
<div class="outer-cell mdl-cell mdl-cell--12-col mdl-shadow--2dp"><div class="mdl-grid"><div class="header-cell mdl-cell mdl-cell--12-col"><p class="mdl-typography--title">Gemini Apps<br></p></div><div class="content-cell mdl-cell mdl-cell--6-col mdl-typography--body-1">Unknown activity<br>Jan 6, 2025, 3:04:05 PM PST<br><p>unknown</p></div><div class="content-cell mdl-cell mdl-cell--12-col mdl-typography--caption"><b>Products:</b><br>&emsp;Gemini Apps<br></div></div></div>
</body></html>`

func TestImportGeminiApps(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "activity.html")
	require.NoError(t, os.WriteFile(
		path, []byte(sanitizedGeminiAppsImportHTML), 0o644,
	))

	d := testDB(t)
	stats, err := ImportGeminiApps(
		t.Context(), d, root, nil, "test-machine",
	)
	require.NoError(t, err)
	assert.Equal(t, 2, stats.Imported)
	assert.Equal(t, 0, stats.Updated)
	assert.Equal(t, 3, stats.Skipped)
	assert.Zero(t, stats.Errors)

	sessions, err := d.ListSessions(t.Context(), db.SessionFilter{Agent: "gemini-apps"})
	require.NoError(t, err)
	assert.Len(t, sessions.Sessions, 2)
	assert.Equal(t, "test-machine", sessions.Sessions[0].Machine)
}

func TestImportGeminiAppsIgnoresOtherProductsThroughPublicPath(t *testing.T) {
	root := t.TempDir()
	gemini := geminiAppsImportPromptedCell("Jan 2, 2025, 3:04:05 PM EDT", `See <a href="https://example.invalid">source</a> for details`)
	other := strings.ReplaceAll(
		geminiAppsImportPromptedCell("Jan 2, 2025, 3:04:05 PM XYZ", "video title"),
		"Gemini Apps", "YouTube",
	)
	path := filepath.Join(root, "activity.html")
	require.NoError(t, os.WriteFile(
		path,
		[]byte(`<!doctype html><html><head><title>My Activity History</title></head><body>`+gemini+other+`</body></html>`),
		0o644,
	))

	d := testDB(t)
	stats, err := ImportGeminiApps(t.Context(), d, root, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Imported)
	assert.Zero(t, stats.Skipped)
	assert.Zero(t, stats.Errors)

	page, err := d.ListSessions(t.Context(), db.SessionFilter{Agent: "gemini-apps"})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	messages, err := d.GetAllMessages(t.Context(), page.Sessions[0].ID)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "See source for details", messages[0].Content)
}

func TestImportGeminiAppsReimportUpdatesResponseWithStableID(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "activity.html")
	initial := strings.ReplaceAll(
		sanitizedGeminiAppsImportHTML,
		`<div class="outer-cell mdl-cell mdl-cell--12-col mdl-shadow--2dp"><div class="mdl-grid"><div class="header-cell mdl-cell mdl-cell--12-col"><p class="mdl-typography--title">Gemini Apps<br></p></div><div class="content-cell mdl-cell mdl-cell--6-col mdl-typography--body-1">Created a canvas<br>Jan 3, 2025, 3:04:05 PM PST<br><p>canvas</p></div><div class="content-cell mdl-cell mdl-cell--12-col mdl-typography--caption"><b>Products:</b><br>&emsp;Gemini Apps<br></div></div></div>`,
		"",
	)
	initial = strings.ReplaceAll(initial,
		`<div class="outer-cell mdl-cell mdl-cell--12-col mdl-shadow--2dp"><div class="mdl-grid"><div class="header-cell mdl-cell mdl-cell--12-col"><p class="mdl-typography--title">Gemini Apps<br></p></div><div class="content-cell mdl-cell mdl-cell--6-col mdl-typography--body-1">Gave feedback: Good response<br>Jan 4, 2025, 3:04:05 PM PST<br></div><div class="content-cell mdl-cell mdl-cell--12-col mdl-typography--caption"><b>Products:</b><br>&emsp;Gemini Apps<br></div></div></div>`,
		"",
	)
	initial = strings.ReplaceAll(initial,
		`<div class="outer-cell mdl-cell mdl-cell--12-col mdl-shadow--2dp"><div class="mdl-grid"><div class="header-cell mdl-cell mdl-cell--12-col"><p class="mdl-typography--title">Gemini Apps<br></p></div><div class="content-cell mdl-cell mdl-cell--6-col mdl-typography--body-1">Prompted&nbsp;second prompt<br>Jan 5, 2025, 3:04:05 PM PST<br><p>second answer</p></div><div class="content-cell mdl-cell mdl-cell--12-col mdl-typography--caption"><b>Products:</b><br>&emsp;Gemini Apps<br></div></div></div>`,
		"",
	)
	initial = strings.ReplaceAll(initial,
		`<div class="outer-cell mdl-cell mdl-cell--12-col mdl-shadow--2dp"><div class="mdl-grid"><div class="header-cell mdl-cell mdl-cell--12-col"><p class="mdl-typography--title">Gemini Apps<br></p></div><div class="content-cell mdl-cell mdl-cell--6-col mdl-typography--body-1">Unknown activity<br>Jan 6, 2025, 3:04:05 PM PST<br><p>unknown</p></div><div class="content-cell mdl-cell mdl-cell--12-col mdl-typography--caption"><b>Products:</b><br>&emsp;Gemini Apps<br></div></div></div>`,
		"",
	)
	require.NoError(t, os.WriteFile(path, []byte(initial), 0o644))

	d := testDB(t)
	first, err := ImportGeminiApps(t.Context(), d, root, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, first.Imported)
	initialPage, err := d.ListSessions(t.Context(), db.SessionFilter{Agent: "gemini-apps"})
	require.NoError(t, err)
	require.Len(t, initialPage.Sessions, 1)
	initialID := initialPage.Sessions[0].ID

	updated := strings.ReplaceAll(initial, "first answer", "updated answer")
	require.NoError(t, os.WriteFile(path, []byte(updated), 0o644))
	second, err := ImportGeminiApps(t.Context(), d, root, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, second.Imported)
	assert.Equal(t, 1, second.Updated)
	assert.Zero(t, second.Skipped)

	page, err := d.ListSessions(t.Context(), db.SessionFilter{Agent: "gemini-apps"})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	assert.Equal(t, initialID, page.Sessions[0].ID)
	messages, err := d.GetAllMessages(t.Context(), page.Sessions[0].ID)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, "first prompt", messages[0].Content)
	assert.Equal(t, "user", messages[0].Role)
	assert.Equal(t, "updated answer", messages[1].Content)
	assert.Equal(t, "assistant", messages[1].Role)
}

// geminiAppsImportPromptedCell renders one Prompted record in the cell layout
// Google Takeout writes.
func geminiAppsImportPromptedCell(timestamp, prompt string) string {
	return `<div class="outer-cell mdl-cell mdl-cell--12-col mdl-shadow--2dp"><div class="mdl-grid">` +
		`<div class="header-cell mdl-cell mdl-cell--12-col"><p class="mdl-typography--title">Gemini Apps<br></p></div>` +
		`<div class="content-cell mdl-cell mdl-cell--6-col mdl-typography--body-1">Prompted&nbsp;` + prompt + `<br>` + timestamp + `<br></div>` +
		`<div class="content-cell mdl-cell mdl-cell--12-col mdl-typography--caption"><b>Products:</b><br>&emsp;Gemini Apps<br></div>` +
		`</div></div>`
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
	root := t.TempDir()
	path := filepath.Join(root, "activity.html")
	first := geminiAppsImportPromptedCell("Jan 2, 2025, 3:04:05 PM EDT", "first")
	second := geminiAppsImportPromptedCell("Jan 3, 2025, 3:04:05 PM EDT", "second")
	prepended := geminiAppsImportPromptedCell("Jan 1, 2025, 3:04:05 PM EDT", "prepended")
	require.NoError(t, os.WriteFile(path, []byte(geminiAppsImportDocument(first, second)), 0o644))

	d := testDB(t)
	initial, err := ImportGeminiApps(t.Context(), d, root, nil)
	require.NoError(t, err)
	assert.Equal(t, 2, initial.Imported)
	initialIDs := geminiAppsSessionIDs(t, d)

	require.NoError(t, os.WriteFile(path, []byte(geminiAppsImportDocument(prepended, first, second)), 0o644))
	secondImport, err := ImportGeminiApps(t.Context(), d, root, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, secondImport.Imported)
	assert.Zero(t, secondImport.Updated)
	assert.Equal(t, 2, secondImport.Skipped)
	assert.Zero(t, secondImport.Errors)

	page, err := d.ListSessions(t.Context(), db.SessionFilter{Agent: "gemini-apps"})
	require.NoError(t, err)
	assert.Len(t, page.Sessions, 3)
	finalIDs := geminiAppsSessionIDs(t, d)
	for id := range initialIDs {
		assert.True(t, finalIDs[id])
	}
}

func TestImportGeminiAppsReimportAfterReorderKeepsExistingSessions(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "activity.html")
	first := geminiAppsImportPromptedCell("Jan 2, 2025, 3:04:05 PM EDT", "first")
	second := geminiAppsImportPromptedCell("Jan 3, 2025, 3:04:05 PM EDT", "second")
	require.NoError(t, os.WriteFile(path, []byte(geminiAppsImportDocument(first, second)), 0o644))

	d := testDB(t)
	initial, err := ImportGeminiApps(t.Context(), d, root, nil)
	require.NoError(t, err)
	assert.Equal(t, 2, initial.Imported)
	initialIDs := geminiAppsSessionIDs(t, d)

	require.NoError(t, os.WriteFile(path, []byte(geminiAppsImportDocument(second, first)), 0o644))
	reordered, err := ImportGeminiApps(t.Context(), d, root, nil)
	require.NoError(t, err)
	assert.Zero(t, reordered.Imported)
	assert.Zero(t, reordered.Updated)
	assert.Equal(t, 2, reordered.Skipped)
	assert.Zero(t, reordered.Errors)

	page, err := d.ListSessions(t.Context(), db.SessionFilter{Agent: "gemini-apps"})
	require.NoError(t, err)
	assert.Len(t, page.Sessions, 2)
	finalIDs := geminiAppsSessionIDs(t, d)
	for id := range initialIDs {
		assert.True(t, finalIDs[id])
	}
}

func TestImportGeminiAppsUnknownZoneDoesNotWriteSession(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "activity.html")
	html := strings.ReplaceAll(
		sanitizedGeminiAppsImportHTML,
		"EDT", "XYZ",
	)
	html = strings.ReplaceAll(html,
		`<div class="outer-cell mdl-cell mdl-cell--12-col mdl-shadow--2dp"><div class="mdl-grid"><div class="header-cell mdl-cell mdl-cell--12-col"><p class="mdl-typography--title">Gemini Apps<br></p></div><div class="content-cell mdl-cell mdl-cell--6-col mdl-typography--body-1">Created a canvas<br>Jan 3, 2025, 3:04:05 PM PST<br><p>canvas</p></div><div class="content-cell mdl-cell mdl-cell--12-col mdl-typography--caption"><b>Products:</b><br>&emsp;Gemini Apps<br></div></div></div>`,
		"",
	)
	html = strings.ReplaceAll(html,
		`<div class="outer-cell mdl-cell mdl-cell--12-col mdl-shadow--2dp"><div class="mdl-grid"><div class="header-cell mdl-cell mdl-cell--12-col"><p class="mdl-typography--title">Gemini Apps<br></p></div><div class="content-cell mdl-cell mdl-cell--6-col mdl-typography--body-1">Gave feedback: Good response<br>Jan 4, 2025, 3:04:05 PM PST<br></div><div class="content-cell mdl-cell mdl-cell--12-col mdl-typography--caption"><b>Products:</b><br>&emsp;Gemini Apps<br></div></div></div>`,
		"",
	)
	html = strings.ReplaceAll(html,
		`<div class="outer-cell mdl-cell mdl-cell--12-col mdl-shadow--2dp"><div class="mdl-grid"><div class="header-cell mdl-cell mdl-cell--12-col"><p class="mdl-typography--title">Gemini Apps<br></p></div><div class="content-cell mdl-cell mdl-cell--6-col mdl-typography--body-1">Prompted&nbsp;second prompt<br>Jan 5, 2025, 3:04:05 PM PST<br><p>second answer</p></div><div class="content-cell mdl-cell mdl-cell--12-col mdl-typography--caption"><b>Products:</b><br>&emsp;Gemini Apps<br></div></div></div>`,
		"",
	)
	html = strings.ReplaceAll(html,
		`<div class="outer-cell mdl-cell mdl-cell--12-col mdl-shadow--2dp"><div class="mdl-grid"><div class="header-cell mdl-cell mdl-cell--12-col"><p class="mdl-typography--title">Gemini Apps<br></p></div><div class="content-cell mdl-cell mdl-cell--6-col mdl-typography--body-1">Unknown activity<br>Jan 6, 2025, 3:04:05 PM PST<br><p>unknown</p></div><div class="content-cell mdl-cell mdl-cell--12-col mdl-typography--caption"><b>Products:</b><br>&emsp;Gemini Apps<br></div></div></div>`,
		"",
	)
	require.NoError(t, os.WriteFile(path, []byte(html), 0o644))

	d := testDB(t)
	stats, err := ImportGeminiApps(t.Context(), d, root, nil)
	require.Error(t, err)
	assert.Zero(t, stats.Errors)
	page, listErr := d.ListSessions(t.Context(), db.SessionFilter{Agent: "gemini-apps"})
	require.NoError(t, listErr)
	assert.Empty(t, page.Sessions)
}

func TestImportGeminiAppsWholeHourZonePersistsCorrectTimestamp(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "activity.html")
	valid := geminiAppsImportPromptedCell(
		"Jan 2, 2025, 3:04:05 PM GMT+8", "prompt",
	)
	malformed := geminiAppsImportPromptedCell(
		"Jan 3, 2025, 3:04:05 PM GMT+8junk", "malformed",
	)
	require.NoError(t, os.WriteFile(path, []byte(geminiAppsImportDocument(valid)), 0o644))

	d := testDB(t)
	stats, err := ImportGeminiApps(t.Context(), d, root, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Imported)

	page, err := d.ListSessions(t.Context(), db.SessionFilter{Agent: "gemini-apps"})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	stored, err := d.GetSessionFull(t.Context(), page.Sessions[0].ID)
	require.NoError(t, err)
	require.NotNil(t, stored.StartedAt)
	require.NotNil(t, stored.EndedAt)
	assert.Equal(t, "2025-01-02T07:04:05Z", *stored.StartedAt)
	assert.Equal(t, *stored.StartedAt, *stored.EndedAt)
	messages, err := d.GetAllMessages(t.Context(), page.Sessions[0].ID)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, *stored.StartedAt, messages[0].Timestamp)

	require.NoError(t, os.WriteFile(path, []byte(geminiAppsImportDocument(malformed)), 0o644))
	stats, err = ImportGeminiApps(t.Context(), d, root, nil)
	require.Error(t, err)
	assert.Zero(t, stats.Imported)
	assert.Zero(t, stats.Updated)
	assert.Zero(t, stats.Errors)
	page, err = d.ListSessions(t.Context(), db.SessionFilter{Agent: "gemini-apps"})
	require.NoError(t, err)
	assert.Len(t, page.Sessions, 1)
}

func TestImportGeminiAppsPersistenceGuards(t *testing.T) {
	t.Run("unchanged and superset preserve revision", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "activity.html")
		initial := strings.ReplaceAll(
			sanitizedGeminiAppsImportHTML,
			`<div class="outer-cell mdl-cell mdl-cell--12-col mdl-shadow--2dp"><div class="mdl-grid"><div class="header-cell mdl-cell mdl-cell--12-col"><p class="mdl-typography--title">Gemini Apps<br></p></div><div class="content-cell mdl-cell mdl-cell--6-col mdl-typography--body-1">Created a canvas<br>Jan 3, 2025, 3:04:05 PM PST<br><p>canvas</p></div><div class="content-cell mdl-cell mdl-cell--12-col mdl-typography--caption"><b>Products:</b><br>&emsp;Gemini Apps<br></div></div></div>`,
			"",
		)
		initial = strings.ReplaceAll(initial,
			`<div class="outer-cell mdl-cell mdl-cell--12-col mdl-shadow--2dp"><div class="mdl-grid"><div class="header-cell mdl-cell mdl-cell--12-col"><p class="mdl-typography--title">Gemini Apps<br></p></div><div class="content-cell mdl-cell mdl-cell--6-col mdl-typography--body-1">Gave feedback: Good response<br>Jan 4, 2025, 3:04:05 PM PST<br></div><div class="content-cell mdl-cell mdl-cell--12-col mdl-typography--caption"><b>Products:</b><br>&emsp;Gemini Apps<br></div></div></div>`,
			"",
		)
		initial = strings.ReplaceAll(initial,
			`<div class="outer-cell mdl-cell mdl-cell--12-col mdl-shadow--2dp"><div class="mdl-grid"><div class="header-cell mdl-cell mdl-cell--12-col"><p class="mdl-typography--title">Gemini Apps<br></p></div><div class="content-cell mdl-cell mdl-cell--6-col mdl-typography--body-1">Prompted&nbsp;second prompt<br>Jan 5, 2025, 3:04:05 PM PST<br><p>second answer</p></div><div class="content-cell mdl-cell mdl-cell--12-col mdl-typography--caption"><b>Products:</b><br>&emsp;Gemini Apps<br></div></div></div>`,
			"",
		)
		initial = strings.ReplaceAll(initial,
			`<div class="outer-cell mdl-cell mdl-cell--12-col mdl-shadow--2dp"><div class="mdl-grid"><div class="header-cell mdl-cell mdl-cell--12-col"><p class="mdl-typography--title">Gemini Apps<br></p></div><div class="content-cell mdl-cell mdl-cell--6-col mdl-typography--body-1">Unknown activity<br>Jan 6, 2025, 3:04:05 PM PST<br><p>unknown</p></div><div class="content-cell mdl-cell mdl-cell--12-col mdl-typography--caption"><b>Products:</b><br>&emsp;Gemini Apps<br></div></div></div>`,
			"",
		)
		require.NoError(t, os.WriteFile(path, []byte(initial), 0o644))
		d := testDB(t)
		require.True(t, d.HasFTS(t.Context()))
		indexing := 0
		first, err := ImportGeminiApps(t.Context(), d, root, &ImportCallbacks{
			OnIndexing: func() { indexing++ },
		})
		require.NoError(t, err)
		assert.Equal(t, 1, first.Imported)
		assert.Equal(t, 1, indexing)

		page, err := d.ListSessions(t.Context(), db.SessionFilter{Agent: "gemini-apps"})
		require.NoError(t, err)
		require.Len(t, page.Sessions, 1)
		id := page.Sessions[0].ID
		stored, err := d.GetSessionFull(t.Context(), id)
		require.NoError(t, err)
		require.NotNil(t, stored.TranscriptRevision)
		revision := *stored.TranscriptRevision

		indexing = 0
		unchanged, err := ImportGeminiApps(t.Context(), d, root, &ImportCallbacks{
			OnIndexing: func() { indexing++ },
		})
		require.NoError(t, err)
		assert.Equal(t, 1, unchanged.Skipped)
		assert.Zero(t, indexing)
		stored, err = d.GetSessionFull(t.Context(), id)
		require.NoError(t, err)
		require.NotNil(t, stored.TranscriptRevision)
		assert.Equal(t, revision, *stored.TranscriptRevision)

		require.NoError(t, os.WriteFile(path, []byte(sanitizedGeminiAppsImportHTML), 0o644))
		indexing = 0
		superset, err := ImportGeminiApps(t.Context(), d, root, &ImportCallbacks{
			OnIndexing: func() { indexing++ },
		})
		require.NoError(t, err)
		assert.Equal(t, 1, superset.Imported)
		assert.Equal(t, 4, superset.Skipped)
		assert.Equal(t, 1, indexing)
		stored, err = d.GetSessionFull(t.Context(), id)
		require.NoError(t, err)
		require.NotNil(t, stored.TranscriptRevision)
		assert.Equal(t, revision, *stored.TranscriptRevision)
	})

	t.Run("excluded sessions stay excluded", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "activity.html")
		fixture := geminiAppsSinglePromptHTML()
		require.NoError(t, os.WriteFile(path, []byte(fixture), 0o644))
		d := testDB(t)
		_, err := ImportGeminiApps(t.Context(), d, root, nil)
		require.NoError(t, err)
		page, err := d.ListSessions(t.Context(), db.SessionFilter{Agent: "gemini-apps"})
		require.NoError(t, err)
		require.Len(t, page.Sessions, 1)
		id := page.Sessions[0].ID
		require.NoError(t, d.DeleteSession(t.Context(), id))
		assert.True(t, d.IsSessionExcluded(t.Context(), id))

		indexing := 0
		stats, err := ImportGeminiApps(t.Context(), d, root, &ImportCallbacks{
			OnIndexing: func() { indexing++ },
		})
		require.NoError(t, err)
		assert.Equal(t, 1, stats.Skipped)
		assert.Zero(t, stats.Errors)
		assert.Empty(t, stats.Refusals)
		assert.Zero(t, indexing)
		page, err = d.ListSessions(t.Context(), db.SessionFilter{Agent: "gemini-apps"})
		require.NoError(t, err)
		assert.Empty(t, page.Sessions)
	})
}

// geminiAppsSinglePromptHTML keeps only the first Prompted record of the sanitized fixture.
func geminiAppsSinglePromptHTML() string {
	fixture := strings.ReplaceAll(
		sanitizedGeminiAppsImportHTML,
		`<div class="outer-cell mdl-cell mdl-cell--12-col mdl-shadow--2dp"><div class="mdl-grid"><div class="header-cell mdl-cell mdl-cell--12-col"><p class="mdl-typography--title">Gemini Apps<br></p></div><div class="content-cell mdl-cell mdl-cell--6-col mdl-typography--body-1">Created a canvas<br>Jan 3, 2025, 3:04:05 PM PST<br><p>canvas</p></div><div class="content-cell mdl-cell mdl-cell--12-col mdl-typography--caption"><b>Products:</b><br>&emsp;Gemini Apps<br></div></div></div>`,
		"",
	)
	fixture = strings.ReplaceAll(fixture,
		`<div class="outer-cell mdl-cell mdl-cell--12-col mdl-shadow--2dp"><div class="mdl-grid"><div class="header-cell mdl-cell mdl-cell--12-col"><p class="mdl-typography--title">Gemini Apps<br></p></div><div class="content-cell mdl-cell mdl-cell--6-col mdl-typography--body-1">Gave feedback: Good response<br>Jan 4, 2025, 3:04:05 PM PST<br></div><div class="content-cell mdl-cell mdl-cell--12-col mdl-typography--caption"><b>Products:</b><br>&emsp;Gemini Apps<br></div></div></div>`,
		"",
	)
	fixture = strings.ReplaceAll(fixture,
		`<div class="outer-cell mdl-cell mdl-cell--12-col mdl-shadow--2dp"><div class="mdl-grid"><div class="header-cell mdl-cell mdl-cell--12-col"><p class="mdl-typography--title">Gemini Apps<br></p></div><div class="content-cell mdl-cell mdl-cell--6-col mdl-typography--body-1">Prompted&nbsp;second prompt<br>Jan 5, 2025, 3:04:05 PM PST<br><p>second answer</p></div><div class="content-cell mdl-cell mdl-cell--12-col mdl-typography--caption"><b>Products:</b><br>&emsp;Gemini Apps<br></div></div></div>`,
		"",
	)
	fixture = strings.ReplaceAll(fixture,
		`<div class="outer-cell mdl-cell mdl-cell--12-col mdl-shadow--2dp"><div class="mdl-grid"><div class="header-cell mdl-cell mdl-cell--12-col"><p class="mdl-typography--title">Gemini Apps<br></p></div><div class="content-cell mdl-cell mdl-cell--6-col mdl-typography--body-1">Unknown activity<br>Jan 6, 2025, 3:04:05 PM PST<br><p>unknown</p></div><div class="content-cell mdl-cell mdl-cell--12-col mdl-typography--caption"><b>Products:</b><br>&emsp;Gemini Apps<br></div></div></div>`,
		"",
	)
	return fixture
}

func TestImportGeminiAppsReportsTrashedSession(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "activity.html"), []byte(geminiAppsSinglePromptHTML()), 0o644,
	))
	d := testDB(t)
	_, err := ImportGeminiApps(t.Context(), d, root, nil)
	require.NoError(t, err)
	page, err := d.ListSessions(t.Context(), db.SessionFilter{Agent: "gemini-apps"})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	id := page.Sessions[0].ID
	require.NoError(t, d.SoftDeleteSession(t.Context(), id))

	stats, err := ImportGeminiApps(t.Context(), d, root, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Errors)
	assert.Equal(t, []ImportRefusal{{SessionID: id, Reason: RefusalTrashed}}, stats.Refusals)
}

func TestImportGeminiAppsTakeoutFixture(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("..", "parser", "testdata", "gemini-apps", "MyActivity.html"))
	require.NoError(t, err)
	root := t.TempDir()
	activityDir := filepath.Join(root, "Takeout", "My Activity", "Gemini Apps")
	require.NoError(t, os.MkdirAll(activityDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(activityDir, "MyActivity.html"), fixture, 0o644))

	d := testDB(t)
	stats, err := ImportGeminiApps(t.Context(), d, root, nil)
	require.NoError(t, err)
	assert.Equal(t, 3, stats.Imported)
	assert.Equal(t, 1, stats.Skipped)
	assert.Zero(t, stats.Errors)

	page, err := d.ListSessions(t.Context(), db.SessionFilter{Agent: "gemini-apps"})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 3)
	var haiku *db.Session
	for i := range page.Sessions {
		if page.Sessions[i].FirstMessage != nil && *page.Sessions[i].FirstMessage == "Write a haiku about rain.\nKeep it short." {
			haiku = &page.Sessions[i]
		}
	}
	require.NotNil(t, haiku)
	messages, err := d.GetAllMessages(t.Context(), haiku.ID)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, "user", messages[0].Role)
	assert.Equal(t, "assistant", messages[1].Role)
	assert.Equal(t, "Soft rain on the roof\nthe gutters hum their one song\nmorning smells of earth", messages[1].Content)
}
