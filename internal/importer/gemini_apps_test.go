package importer

import (
	"context"
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
	root := t.TempDir()
	path := filepath.Join(root, "activity.html")
	require.NoError(t, os.WriteFile(
		path, []byte(sanitizedGeminiAppsImportHTML), 0o644,
	))

	d := testDB(t)
	stats, err := ImportGeminiApps(
		context.Background(), d, root, nil, "test-machine",
	)
	require.NoError(t, err)
	assert.Equal(t, 2, stats.Imported)
	assert.Equal(t, 0, stats.Updated)
	assert.Equal(t, 3, stats.Skipped)
	assert.Zero(t, stats.Errors)

	sessions, err := d.ListSessions(context.Background(), db.SessionFilter{Agent: "gemini-apps"})
	require.NoError(t, err)
	assert.Len(t, sessions.Sessions, 2)
	assert.Equal(t, "test-machine", sessions.Sessions[0].Machine)
}

func TestImportGeminiAppsIgnoresOtherProductsThroughPublicPath(t *testing.T) {
	root := t.TempDir()
	gemini := `<div class="outer-cell"><div class="header-cell"><h3>Gemini Apps</h3><p>Prompted</p><p>Jan 2, 2025, 3:04:05 PM EDT</p></div><div class="content-cell"><p>See <a href="https://example.invalid">source</a> for details</p></div></div>`
	other := `<div class="outer-cell"><div class="header-cell"><h3>YouTube</h3><p>Watched</p><p>Jan 2, 2025, 3:04:05 PM XYZ</p></div><div class="content-cell"><p>video title</p></div></div>`
	path := filepath.Join(root, "activity.html")
	require.NoError(t, os.WriteFile(
		path,
		[]byte(`<!doctype html><html><head><title>My Activity History</title></head><body>`+gemini+other+`</body></html>`),
		0o644,
	))

	d := testDB(t)
	stats, err := ImportGeminiApps(context.Background(), d, root, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Imported)
	assert.Zero(t, stats.Skipped)
	assert.Zero(t, stats.Errors)

	page, err := d.ListSessions(context.Background(), db.SessionFilter{Agent: "gemini-apps"})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	messages, err := d.GetAllMessages(context.Background(), page.Sessions[0].ID)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "See source for details", messages[0].Content)
}

func TestImportGeminiAppsReimportUpdatesResponseWithStableID(t *testing.T) {
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
	require.NoError(t, os.WriteFile(path, []byte(initial), 0o644))

	d := testDB(t)
	first, err := ImportGeminiApps(context.Background(), d, root, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, first.Imported)

	updated := strings.ReplaceAll(initial, "first answer", "updated answer")
	require.NoError(t, os.WriteFile(path, []byte(updated), 0o644))
	second, err := ImportGeminiApps(context.Background(), d, root, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, second.Imported)
	assert.Equal(t, 1, second.Updated)
	assert.Zero(t, second.Skipped)

	page, err := d.ListSessions(context.Background(), db.SessionFilter{Agent: "gemini-apps"})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	messages, err := d.GetAllMessages(context.Background(), page.Sessions[0].ID)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, "updated answer", messages[1].Content)
}

func TestImportGeminiAppsUnknownZoneDoesNotWriteSession(t *testing.T) {
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
	require.NoError(t, os.WriteFile(path, []byte(html), 0o644))

	d := testDB(t)
	stats, err := ImportGeminiApps(context.Background(), d, root, nil)
	assert.Error(t, err)
	assert.Zero(t, stats.Errors)
	page, listErr := d.ListSessions(context.Background(), db.SessionFilter{Agent: "gemini-apps"})
	require.NoError(t, listErr)
	assert.Empty(t, page.Sessions)
}

func TestImportGeminiAppsPersistenceGuards(t *testing.T) {
	t.Run("unchanged and superset preserve revision", func(t *testing.T) {
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
		require.NoError(t, os.WriteFile(path, []byte(initial), 0o644))
		d := testDB(t)
		require.True(t, d.HasFTS())
		indexing := 0
		first, err := ImportGeminiApps(context.Background(), d, root, &ImportCallbacks{
			OnIndexing: func() { indexing++ },
		})
		require.NoError(t, err)
		assert.Equal(t, 1, first.Imported)
		assert.Equal(t, 1, indexing)

		page, err := d.ListSessions(context.Background(), db.SessionFilter{Agent: "gemini-apps"})
		require.NoError(t, err)
		require.Len(t, page.Sessions, 1)
		id := page.Sessions[0].ID
		stored, err := d.GetSessionFull(context.Background(), id)
		require.NoError(t, err)
		require.NotNil(t, stored.TranscriptRevision)
		revision := *stored.TranscriptRevision

		indexing = 0
		unchanged, err := ImportGeminiApps(context.Background(), d, root, &ImportCallbacks{
			OnIndexing: func() { indexing++ },
		})
		require.NoError(t, err)
		assert.Equal(t, 1, unchanged.Skipped)
		assert.Zero(t, indexing)
		stored, err = d.GetSessionFull(context.Background(), id)
		require.NoError(t, err)
		require.NotNil(t, stored.TranscriptRevision)
		assert.Equal(t, revision, *stored.TranscriptRevision)

		require.NoError(t, os.WriteFile(path, []byte(sanitizedGeminiAppsImportHTML), 0o644))
		indexing = 0
		superset, err := ImportGeminiApps(context.Background(), d, root, &ImportCallbacks{
			OnIndexing: func() { indexing++ },
		})
		require.NoError(t, err)
		assert.Equal(t, 1, superset.Imported)
		assert.Equal(t, 4, superset.Skipped)
		assert.Equal(t, 1, indexing)
		stored, err = d.GetSessionFull(context.Background(), id)
		require.NoError(t, err)
		require.NotNil(t, stored.TranscriptRevision)
		assert.Equal(t, revision, *stored.TranscriptRevision)
	})

	t.Run("excluded sessions stay excluded", func(t *testing.T) {
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
		require.NoError(t, os.WriteFile(path, []byte(fixture), 0o644))
		d := testDB(t)
		_, err := ImportGeminiApps(context.Background(), d, root, nil)
		require.NoError(t, err)
		page, err := d.ListSessions(context.Background(), db.SessionFilter{Agent: "gemini-apps"})
		require.NoError(t, err)
		require.Len(t, page.Sessions, 1)
		id := page.Sessions[0].ID
		require.NoError(t, d.DeleteSession(id))
		assert.True(t, d.IsSessionExcluded(id))

		indexing := 0
		stats, err := ImportGeminiApps(context.Background(), d, root, &ImportCallbacks{
			OnIndexing: func() { indexing++ },
		})
		require.NoError(t, err)
		assert.Equal(t, 1, stats.Skipped)
		assert.Zero(t, stats.Errors)
		assert.Zero(t, indexing)
		page, err = d.ListSessions(context.Background(), db.SessionFilter{Agent: "gemini-apps"})
		require.NoError(t, err)
		assert.Empty(t, page.Sessions)
	})
}
