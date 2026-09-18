package remotesync

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func TestMirrorRelativeChangePaths(t *testing.T) {
	mirrorRoot := filepath.Join(t.TempDir(), "mirror")

	remote, err := mirrorRelativeRemoteChangePath(
		mirrorRoot, "/srv/example/.codex/sessions/a.jsonl",
	)
	require.NoError(t, err)
	assert.Equal(t, "srv/example/.codex/sessions/a.jsonl", remote)

	local := filepath.Join(mirrorRoot, "srv", "example", "session.jsonl")
	rel, err := mirrorRelativeLocalChangePath(mirrorRoot, local)
	require.NoError(t, err)
	assert.Equal(t, "srv/example/session.jsonl", rel)

	_, err = mirrorRelativeLocalChangePath(
		mirrorRoot, filepath.Join(filepath.Dir(mirrorRoot), "escape.jsonl"),
	)
	require.Error(t, err)
	_, err = mirrorRelativeRemoteChangePath(mirrorRoot, "../../escape.jsonl")
	require.Error(t, err)
	_, err = mirrorRelativeLocalChangePath(mirrorRoot, mirrorRoot)
	require.Error(t, err)
}

func TestMirrorChangeJournalMergeSetUnionAndRearm(t *testing.T) {
	journal := MirrorChangeJournal{
		Version: mirrorJournalVersion,
		Entries: []MirrorChangeEntry{
			{Path: "a/session.jsonl"},
			{Path: "b/session.jsonl", InvalidateCache: true, ForceFullParse: true},
		},
	}

	merged, stats, err := mergeMirrorChanges(journal, []string{
		"a/session.jsonl",
		"a/session.jsonl",
		"c\\session.jsonl",
	})
	require.NoError(t, err)
	assert.Equal(t, JournalMergeStats{New: 1, Rearmed: 1, Replayed: 1}, stats)
	assert.Equal(t, []MirrorChangeEntry{
		{Path: "a/session.jsonl", InvalidateCache: true, ForceFullParse: true},
		{Path: "b/session.jsonl", InvalidateCache: true, ForceFullParse: true},
		{Path: "c/session.jsonl", InvalidateCache: true, ForceFullParse: true},
	}, merged.Entries)
}

func TestMirrorChangeJournalBoundsAndDisarm(t *testing.T) {
	entries := make([]string, mirrorJournalMaxEntries)
	for i := range entries {
		entries[i] = fmt.Sprintf("sessions/%08d.jsonl", i)
	}
	journal, _, err := mergeMirrorChanges(
		MirrorChangeJournal{Version: mirrorJournalVersion}, entries,
	)
	require.NoError(t, err)
	assert.Len(t, journal.Entries, mirrorJournalMaxEntries)

	overflow, _, err := mergeMirrorChanges(journal, []string{"sessions/overflow"})
	require.NoError(t, err)
	assert.Equal(t, MirrorChangeJournal{
		Version:           mirrorJournalVersion,
		FullImport:        true,
		FullImportReason:  FullImportJournalOverflow,
		InvalidateAll:     true,
		ForceFullParseAll: true,
	}, overflow)

	disarmed := disarmMirrorChanges(MirrorChangeJournal{
		Version:           mirrorJournalVersion,
		FullImport:        true,
		FullImportReason:  FullImportJournalRecovery,
		InvalidateAll:     true,
		ForceFullParseAll: true,
		Entries: []MirrorChangeEntry{{
			Path: "session.jsonl", InvalidateCache: true, ForceFullParse: true,
		}},
	})
	assert.Equal(t, MirrorChangeJournal{
		Version:           mirrorJournalVersion,
		FullImport:        true,
		FullImportReason:  FullImportJournalRecovery,
		ForceFullParseAll: true,
		Entries: []MirrorChangeEntry{{
			Path: "session.jsonl", ForceFullParse: true,
		}},
	}, disarmed)
}

func TestMirrorChangeJournalCombinedOwnershipEntryBound(t *testing.T) {
	ownership := map[parser.AgentType][]string{
		parser.AgentWindsurf: {"/snapshot"},
	}
	paths := make([]string, mirrorJournalMaxEntries-1)
	for i := range paths {
		paths[i] = fmt.Sprintf("sessions/%08d.jsonl", i)
	}
	exact, _, err := mergeMirrorChanges(MirrorChangeJournal{
		Version:        mirrorJournalVersion,
		FileScopedDirs: ownership,
	}, paths)
	require.NoError(t, err)
	assert.False(t, exact.FullImport)
	require.NoError(t, validateMirrorChangeJournal(exact))

	overflow, _, err := mergeMirrorChanges(exact, []string{"sessions/overflow"})
	require.NoError(t, err)
	assert.Equal(t, FullImportJournalOverflow, overflow.FullImportReason)
	assert.Equal(t, ownership, overflow.FileScopedDirs)
	require.NoError(t, validateMirrorChangeJournal(overflow))
}

func TestFullImportReasonClosedSet(t *testing.T) {
	for _, reason := range []FullImportReason{
		FullImportLegacy,
		FullImportBootstrap,
		FullImportExplicit,
		FullImportDataRebuild,
		FullImportJournalOverflow,
		FullImportJournalRecovery,
	} {
		assert.True(t, reason.Valid(), "reason %q must be reportable", reason)
	}
	assert.False(t, FullImportReason("transfer-heuristic").Valid(),
		"transfer mode is not a full-import reason")
}

func TestMirrorChangeJournalPathByteBound(t *testing.T) {
	exact := strings.Repeat("a", mirrorJournalMaxPathBytes)
	journal, _, err := mergeMirrorChanges(
		MirrorChangeJournal{Version: mirrorJournalVersion}, []string{exact},
	)
	require.NoError(t, err)
	assert.False(t, journal.FullImport)

	overflow, _, err := mergeMirrorChanges(
		MirrorChangeJournal{Version: mirrorJournalVersion},
		[]string{strings.Repeat("a", mirrorJournalMaxPathBytes+1)},
	)
	require.NoError(t, err)
	assert.Equal(t, FullImportJournalOverflow, overflow.FullImportReason)
}

func TestMirrorChangeJournalCombinedOwnershipPathByteBound(t *testing.T) {
	const root = "/snapshot"
	ownership := map[parser.AgentType][]string{
		parser.AgentWindsurf: {root},
	}
	exactPath := strings.Repeat("a", mirrorJournalMaxPathBytes-len(root))
	exact, _, err := mergeMirrorChanges(MirrorChangeJournal{
		Version:        mirrorJournalVersion,
		FileScopedDirs: ownership,
	}, []string{exactPath})
	require.NoError(t, err)
	assert.False(t, exact.FullImport)
	require.NoError(t, validateMirrorChangeJournal(exact))

	overflow, _, err := mergeMirrorChanges(exact, []string{"b"})
	require.NoError(t, err)
	assert.Equal(t, FullImportJournalOverflow, overflow.FullImportReason)
	assert.Equal(t, ownership, overflow.FileScopedDirs)
	require.NoError(t, validateMirrorChangeJournal(overflow))
}

func TestMirrorChangeJournalRoundTripAndAbsent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mirror")
	path := mirrorJournalPath(root)

	absent, err := loadMirrorChangeJournal(path)
	require.NoError(t, err)
	assert.Equal(t, MirrorChangeJournal{Version: mirrorJournalVersion}, absent)

	want := MirrorChangeJournal{
		Version: mirrorJournalVersion,
		Entries: []MirrorChangeEntry{{
			Path: "sessions/a.jsonl", InvalidateCache: true,
		}},
	}
	require.NoError(t, replaceMirrorChangeJournal(path, want))
	got, err := loadMirrorChangeJournal(path)
	require.NoError(t, err)
	assert.Equal(t, want, got)

	require.NoError(t, retireMirrorChangeJournal(path))
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.NoError(t, retireMirrorChangeJournal(path))
}

func TestMirrorChangeJournalLoadErrorsAreTyped(t *testing.T) {
	dir := t.TempDir()

	unsupported := filepath.Join(dir, "unsupported.json")
	require.NoError(t, os.WriteFile(unsupported, fmt.Appendf(nil,
		`{"version":%d}`, mirrorJournalVersion+1,
	), 0o600))
	_, err := loadMirrorChangeJournal(unsupported)
	require.ErrorIs(t, err, ErrUnsupportedMirrorJournal)

	malformed := filepath.Join(dir, "malformed.json")
	require.NoError(t, os.WriteFile(malformed, []byte(`{"version":1,`), 0o600))
	_, err = loadMirrorChangeJournal(malformed)
	require.ErrorIs(t, err, ErrMalformedMirrorJournal)

	invalid := filepath.Join(dir, "invalid.json")
	require.NoError(t, os.WriteFile(invalid, []byte(`{"version":1,"entries":[{"path":"../escape"}]}`), 0o600))
	_, err = loadMirrorChangeJournal(invalid)
	require.ErrorIs(t, err, ErrMalformedMirrorJournal)

	invalidOwnership := filepath.Join(dir, "invalid-ownership.json")
	require.NoError(t, os.WriteFile(invalidOwnership, fmt.Appendf(nil,
		`{"version":%d,"file_scoped_dirs":{"windsurf":["../escape"]}}`,
		mirrorJournalVersion,
	), 0o600))
	_, err = loadMirrorChangeJournal(invalidOwnership)
	require.ErrorIs(t, err, ErrMalformedMirrorJournal)

	unreadable := filepath.Join(dir, "directory")
	require.NoError(t, os.Mkdir(unreadable, 0o700))
	_, err = loadMirrorChangeJournal(unreadable)
	assert.ErrorIs(t, err, ErrUnreadableMirrorJournal)
}

func TestMirrorChangeJournalFailedRenamePreservesPreviousFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.json")
	previous := MirrorChangeJournal{
		Version: mirrorJournalVersion,
		Entries: []MirrorChangeEntry{{Path: "old.jsonl", InvalidateCache: true}},
	}
	require.NoError(t, replaceMirrorChangeJournal(path, previous))
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	store := newMirrorJournalStore()
	store.rename = func(string, string) error { return errors.New("rename failed") }
	err = store.replace(path, MirrorChangeJournal{
		Version: mirrorJournalVersion,
		Entries: []MirrorChangeEntry{{Path: "new.jsonl", InvalidateCache: true}},
	})
	require.EqualError(t, err, "replace mirror journal: rename failed")
	after, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, before, after)
}

func TestMirrorChangeJournalDirectorySyncFailureMayLeaveReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.json")
	previous := MirrorChangeJournal{
		Version: mirrorJournalVersion,
		Entries: []MirrorChangeEntry{{Path: "old.jsonl", InvalidateCache: true}},
	}
	replacement := MirrorChangeJournal{
		Version: mirrorJournalVersion,
		Entries: []MirrorChangeEntry{{Path: "new.jsonl", InvalidateCache: true}},
	}
	require.NoError(t, replaceMirrorChangeJournal(path, previous))

	closed, err := os.Open(dir)
	require.NoError(t, err)
	require.NoError(t, closed.Close())
	store := newMirrorJournalStore()
	store.open = func(string) (*os.File, error) { return closed, nil }
	require.Error(t, store.replace(path, replacement))

	got, err := loadMirrorChangeJournal(path)
	require.NoError(t, err)
	assert.Equal(t, replacement, got,
		"rename can succeed before parent-directory durability fails")
}

func TestMirrorChangeJournalRetirementRetrySyncsAbsentParentEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.json")
	require.NoError(t, replaceMirrorChangeJournal(
		path, MirrorChangeJournal{Version: mirrorJournalVersion},
	))
	closed, err := os.Open(dir)
	require.NoError(t, err)
	require.NoError(t, closed.Close())
	store := newMirrorJournalStore()
	store.open = func(string) (*os.File, error) { return closed, nil }
	require.Error(t, store.retire(path))
	assert.NoFileExists(t, path)

	store = newMirrorJournalStore()
	require.NoError(t, store.retire(path),
		"an absent journal still requires a successful parent-directory sync")
}

func TestMirrorJournalPathIsAdjacentAndSurvivesMirrorDeletion(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "mirror")
	require.NoError(t, os.MkdirAll(root, 0o700))
	stale := filepath.Join(root, "stale.jsonl")
	require.NoError(t, os.WriteFile(stale, []byte("stale"), 0o600))

	journalPath := mirrorJournalPath(root)
	require.NoError(t, replaceMirrorChangeJournal(
		journalPath, MirrorChangeJournal{Version: mirrorJournalVersion},
	))
	require.NoError(t, ApplyMirrorDeletions(root, []string{stale}))

	assert.Equal(t, parent, filepath.Dir(journalPath))
	_, err := os.Stat(journalPath)
	assert.NoError(t, err)
}
