package rawarchive

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// A capture root can be named through a symlinked ancestor while the source
// installation stored the resolved spelling of the same transcript, or the
// reverse. After the originals are gone, reparse must still recognize the
// seeded session as the same source.
func TestSeededReparseAcceptsEquivalentSymlinkSpelling(t *testing.T) {
	const owner = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const id = "019eb791-cf7d-75c1-8439-9ed74c122e02"
	for _, tt := range []struct {
		name               string
		storeResolved      bool
		captureThroughLink bool
	}{
		{name: "stored resolved, captured through link", storeResolved: true, captureThroughLink: true},
		{name: "stored through link, captured resolved", captureThroughLink: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			target := filepath.Join(t.TempDir(), "real")
			link := filepath.Join(t.TempDir(), "link")
			require.NoError(t, os.MkdirAll(target, 0o700))
			require.NoError(t, os.Symlink(target, link))
			resolved, err := filepath.EvalSymlinks(target)
			require.NoError(t, err)
			rel := filepath.Join("home", "projects", "project-a", id+".jsonl")
			dbtest.WriteTestFile(t, filepath.Join(resolved, rel), []byte(testjsonl.NewSessionBuilder().
				AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", "seed history", id).
				AddClaudeAssistant("2026-01-01T00:00:01Z", "seed reply").String()))
			stored, captured := filepath.Join(link, rel), filepath.Join(resolved, "home")
			if tt.storeResolved {
				stored = filepath.Join(resolved, rel)
			}
			if tt.captureThroughLink {
				captured = filepath.Join(link, "home")
			}

			opts := newImportCapture(t, owner, "source", RootSpec{Provider: "claude", Path: captured})
			source, err := db.OpenIsolatedContext(ctx, filepath.Join(opts.DataDir, "sessions.db"))
			require.NoError(t, err)
			require.NoError(t, source.UpsertSession(ctx, db.Session{
				ID: id, Agent: "claude", Project: "project-a", Machine: owner, FilePath: &stored,
			}))
			require.NoError(t, source.Close())
			opts.Destination = filepath.Join(t.TempDir(), "capture")
			_, err = Capture(ctx, opts)
			require.NoError(t, err)
			seedPath := filepath.Join(t.TempDir(), "seed")
			_, err = Seed(ctx, filepath.Join(opts.Destination, "capture.json"), seedPath, nil)
			require.NoError(t, err)
			require.NoError(t, os.RemoveAll(target))

			seeded, err := db.OpenIsolatedContext(ctx, filepath.Join(seedPath, "sessions.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, seeded.Close()) })
			archive, err := Open(ctx, seeded, seedPath, nil)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, archive.Close()) })
			report, err := archive.Reparse(ctx, ReparseOptions{All: true, ScratchBytes: 1 << 20})
			require.NoError(t, err, "an equivalent spelling of the same transcript is the same source")
			assert.Equal(t, 1, report.Parsed)
			session, err := seeded.GetSessionFull(ctx, id)
			require.NoError(t, err)
			require.NotNil(t, session)
			require.NotNil(t, session.FilePath)
			assert.Equal(t, stored, *session.FilePath, "the seed keeps its stored spelling")
			messages, err := seeded.GetAllMessages(ctx, id)
			require.NoError(t, err)
			assert.Len(t, messages, 2)
		})
	}
}

// A stored path that does not resolve into the captured root is a different
// source, even when it holds a copy of the same transcript.
func TestSeededReparseRejectsUnrelatedSpelling(t *testing.T) {
	ctx := t.Context()
	const owner = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const id = "019eb791-cf7d-75c1-8439-9ed74c122e02"
	home := t.TempDir()
	other := t.TempDir()
	rel := filepath.Join("projects", "project-a", id+".jsonl")
	content := []byte(testjsonl.NewSessionBuilder().
		AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", "seed history", id).String())
	dbtest.WriteTestFile(t, filepath.Join(home, rel), content)
	dbtest.WriteTestFile(t, filepath.Join(other, rel), content)
	stored := filepath.Join(other, rel)
	opts := newImportCapture(t, owner, "source", RootSpec{Provider: "claude", Path: home})
	source, err := db.OpenIsolatedContext(ctx, filepath.Join(opts.DataDir, "sessions.db"))
	require.NoError(t, err)
	require.NoError(t, source.UpsertSession(ctx, db.Session{
		ID: id, Agent: "claude", Project: "project-a", Machine: owner, FilePath: &stored,
	}))
	require.NoError(t, source.Close())
	opts.Destination = filepath.Join(t.TempDir(), "capture")
	captured, err := Capture(ctx, opts)
	require.NoError(t, err)
	for _, root := range captured.Source.Roots {
		assert.Empty(t, root.Aliases, "an unrelated directory is not an alias")
	}
	seedPath := filepath.Join(t.TempDir(), "seed")
	_, err = Seed(ctx, filepath.Join(opts.Destination, "capture.json"), seedPath, nil)
	require.NoError(t, err)
	seeded, err := db.OpenIsolatedContext(ctx, filepath.Join(seedPath, "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, seeded.Close()) })
	archive, err := Open(ctx, seeded, seedPath, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, archive.Close()) })
	_, err = archive.Reparse(ctx, ReparseOptions{All: true, ScratchBytes: 1 << 20})
	require.Error(t, err, "a different source must not be merged into the seeded session")
}

// One transcript linked into the captured root does not make its parent
// directory an alias of the root. Another transcript under that directory is
// still a different source.
func TestCaptureRejectsAliasInferredFromDescendantLink(t *testing.T) {
	ctx := t.Context()
	const owner = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const linked = "019eb791-cf7d-75c1-8439-9ed74c122e02"
	const unrelated = "019eb791-cf7d-75c1-8439-9ed74c122e03"
	home := t.TempDir()
	other := t.TempDir()
	write := func(dir, id, text string) string {
		path := filepath.Join(dir, "projects", "project-a", id+".jsonl")
		dbtest.WriteTestFile(t, path, []byte(testjsonl.NewSessionBuilder().
			AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", text, id).String()))
		return path
	}
	captured := write(home, linked, "captured history")
	write(home, unrelated, "captured history")
	linkedPath := filepath.Join(other, "projects", "project-a", linked+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(linkedPath), 0o700))
	require.NoError(t, os.Symlink(captured, linkedPath))
	unrelatedPath := write(other, unrelated, "different history")

	opts := newImportCapture(t, owner, "source", RootSpec{Provider: "claude", Path: home})
	source, err := db.OpenIsolatedContext(ctx, filepath.Join(opts.DataDir, "sessions.db"))
	require.NoError(t, err)
	for id, path := range map[string]string{linked: linkedPath, unrelated: unrelatedPath} {
		require.NoError(t, source.UpsertSession(ctx, db.Session{
			ID: id, Agent: "claude", Project: "project-a", Machine: owner, FilePath: new(path),
		}))
	}
	require.NoError(t, source.Close())
	opts.Destination = filepath.Join(t.TempDir(), "capture")
	descriptor, err := Capture(ctx, opts)
	require.NoError(t, err)
	for _, root := range descriptor.Source.Roots {
		assert.NotContains(t, root.Aliases, other, "a directory that is not the root is not an alias")
	}
}

func TestRepeatedCapturePreservesRootOriginalPath(t *testing.T) {
	ctx := t.Context()
	const owner = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const foreign = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const id = "019eb791-cf7d-75c1-8439-9ed74c122e02"
	base := t.TempDir()
	real := filepath.Join(base, "real")
	link := filepath.Join(base, "link")
	require.NoError(t, os.MkdirAll(real, 0o700))
	require.NoError(t, os.Symlink(real, link))
	dbtest.WriteTestFile(t, filepath.Join(real, "home", "projects", "project-a", id+".jsonl"), []byte(testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", "captured history", id).String()))
	opts := newImportCapture(t, foreign, "source", RootSpec{Provider: "claude", Path: filepath.Join(link, "home")})
	first := loadTestCapture(t, &opts)
	require.NoError(t, os.Remove(link))
	opts.Roots[0].Path = filepath.Join(real, "home")
	second := loadTestCapture(t, &opts)
	assert.Equal(t, first.Roots[0].ID, second.Roots[0].ID)
	assert.Equal(t, filepath.Join(link, "home"), second.Roots[0].OriginalPath)
	assert.Contains(t, second.Roots[0].Aliases, filepath.Join(real, "home"))
	data := t.TempDir()
	dbtest.WriteTestFile(t, filepath.Join(data, "telemetry-install-id"), []byte(owner))
	database := dbtest.OpenTestDB(t)
	require.NoError(t, database.EnableArchiveOnly(ctx))
	archive, err := Open(ctx, database, data, nil)
	require.NoError(t, err)
	defer archive.Close()
	for _, spec := range []ImportSpec{first, second} {
		report, err := archive.Import(ctx, spec)
		require.NoError(t, err)
		assert.Empty(t, report.Gaps)
		assert.Equal(t, 1, report.Sources)
	}
	_, err = archive.Reparse(ctx, ReparseOptions{All: true, ScratchBytes: 1 << 20})
	require.NoError(t, err)
	messages, err := database.GetAllMessages(ctx, foreign+"~"+id)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "captured history", messages[0].Content)
}
