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
	assert.ErrorContains(t, err, "no admissible Prompted records")
	assert.Equal(t, 1, stats.Errors)
	page, listErr := d.ListSessions(context.Background(), db.SessionFilter{Agent: "gemini-apps"})
	require.NoError(t, listErr)
	assert.Empty(t, page.Sessions)
}
