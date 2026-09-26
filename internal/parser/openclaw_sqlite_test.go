package parser

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenClawProviderCapabilities(t *testing.T) {
	factory, ok := ProviderFactoryByType(AgentOpenClaw)
	require.True(t, ok)
	caps := factory.Capabilities()
	assert.Equal(t, CapabilitySupported, caps.Source.StreamingDiscovery)
	assert.Equal(t, CapabilitySupported, caps.Source.MultiSessionSource)
	assert.Equal(t, CapabilitySupported, caps.Source.SharedContainerSource)
	assert.Equal(t, CapabilitySupported, caps.Source.PerSessionErrors)
	assert.Equal(t, CapabilitySupported, caps.Source.ForceReplaceOnParse)
	assert.Equal(t, CapabilitySupported, caps.Source.PersistentArchive)
	assert.Equal(t, CapabilitySupported, caps.Source.StoredSourceHints)
	assert.True(t, caps.Sync.FingerprintHashInCacheKey)
	assert.True(t, caps.Sync.FingerprintHashRequiredForFreshness)
	provider, ok := factory.NewProvider(ProviderConfig{Roots: []string{t.TempDir()}}).(*openClawProvider)
	require.True(t, ok)
	_, ok = any(provider).(StreamingDiscoverer)
	assert.True(t, ok)
	_, ok = any(provider).(ReconciliationSourceResolver)
	assert.True(t, ok)
	_, ok = any(provider).(PersistentArchiveSourceResolver)
	assert.True(t, ok)
}

func TestOpenClawSQLiteStoreDiscoverAndParse(t *testing.T) {
	root := t.TempDir()
	dbPath := createOpenClawSQLiteFixture(t, root, "main", map[string][]string{
		"sqlite-session": openClawSQLiteFixtureEvents("sqlite-session"),
	})
	require.NoError(t, os.MkdirAll(filepath.Join(root, "main", "sessions"), 0o755))

	provider, ok := NewProvider(AgentOpenClaw, ProviderConfig{
		Roots:   []string{root},
		Machine: "test-machine",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	virtualPath := VirtualSourcePath(dbPath, "main:sqlite-session")
	assert.Equal(t, virtualPath, discovered[0].DisplayPath)
	assert.Equal(t, "main", discovered[0].ProjectHint)

	fingerprint, err := provider.Fingerprint(t.Context(), discovered[0])
	require.NoError(t, err)
	assert.Equal(t, virtualPath, fingerprint.Key)
	assert.NotEmpty(t, fingerprint.Hash)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:      discovered[0],
		Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.True(t, outcome.ResultSetComplete)
	require.Len(t, outcome.Results, 1)
	result := outcome.Results[0].Result
	assert.Equal(t, "openclaw:main:sqlite-session", result.Session.ID)
	assert.Equal(t, "project_a", result.Session.Project)
	assert.Equal(t, "test-machine", result.Session.Machine)
	assert.Equal(t, virtualPath, result.Session.File.Path)
	assert.Equal(t, fingerprint.Hash, result.Session.File.Hash)
	require.Len(t, result.Messages, 4)
	assert.Equal(t, RoleUser, result.Messages[0].Role)
	assert.Equal(t, "hello from sqlite", result.Messages[0].Content)
	assert.Equal(t, RoleAssistant, result.Messages[1].Role)
	assert.True(t, result.Messages[1].HasToolUse)
	assert.Equal(t, RoleUser, result.Messages[2].Role)
	assert.Equal(t, "tool-1", result.Messages[2].ToolResults[0].ToolUseID)
	assert.Equal(t, RoleAssistant, result.Messages[3].Role)
}

func TestOpenClawSQLiteDiscoveryCarriesPerSessionRecency(t *testing.T) {
	root := t.TempDir()
	dbPath := createOpenClawSQLiteFixture(t, root, "main", map[string][]string{
		"a-older": openClawSQLiteFixtureEvents("a-older"),
		"z-newer": openClawSQLiteFixtureEvents("z-newer"),
	})
	database, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = database.ExecContext(t.Context(), `
		UPDATE transcript_events SET created_at = ? WHERE session_id = ?
	`, int64(1_700_000_000_100), "a-older")
	require.NoError(t, err)
	_, err = database.ExecContext(t.Context(), `
		UPDATE transcript_events SET created_at = ? WHERE session_id = ? AND seq = ?
	`, int64(1_700_000_000_200), "z-newer", 0)
	require.NoError(t, err)
	_, err = database.ExecContext(t.Context(), `
		UPDATE transcript_events SET created_at = ? WHERE session_id = ? AND seq = ?
	`, int64(1_700_000_000_300), "z-newer", 1)
	require.NoError(t, err)
	require.NoError(t, database.Close())

	provider, ok := NewProvider(AgentOpenClaw, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)

	want := map[string]int64{
		VirtualSourcePath(dbPath, "main:a-older"): time.UnixMilli(1_700_000_000_100).UnixNano(),
		VirtualSourcePath(dbPath, "main:z-newer"): time.UnixMilli(1_700_000_000_300).UnixNano(),
	}
	got := make(map[string]int64, len(discovered))
	for _, source := range discovered {
		got[source.DisplayPath] = source.DiscoveryMTimeNS
	}
	assert.Equal(t, want, got)
}

func TestOpenClawSQLiteStoreLayouts(t *testing.T) {
	root := t.TempDir()
	mainDB := createOpenClawSQLiteFixture(t, root, "main", map[string][]string{
		"same-session": openClawSQLiteFixtureEvents("same-session"),
	})
	otherDB := createOpenClawSQLiteFixture(t, root, "other", map[string][]string{
		"same-session": openClawSQLiteFixtureEvents("same-session"),
	})
	legacyPath := filepath.Join(root, "main", "sessions", "same-session.jsonl")
	legacyArchive := legacyPath + ".full.bak"
	writeSourceFile(t, legacyPath, clawProviderFixture("same-session", "legacy"))
	writeSourceFile(t, legacyArchive, clawProviderFixture("same-session", "archived legacy"))

	provider, ok := NewProvider(AgentOpenClaw, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, discovered, 2)
	assert.Equal(t, VirtualSourcePath(mainDB, "main:same-session"), discovered[0].DisplayPath)
	assert.Equal(t, VirtualSourcePath(otherDB, "other:same-session"), discovered[1].DisplayPath)
	for _, source := range discovered {
		assert.NotEqual(t, legacyPath, source.DisplayPath)
	}

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "main:same-session",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, VirtualSourcePath(mainDB, "main:same-session"), found.DisplayPath)

	found, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID:       "main:same-session",
		StoredFilePath:     legacyArchive,
		RequireFreshSource: true,
		PreferStoredSource: true,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, VirtualSourcePath(mainDB, "main:same-session"), found.DisplayPath)

	require.NoError(t, os.Remove(legacyArchive))
	found, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID:       "main:same-session",
		StoredFilePath:     legacyArchive,
		RequireFreshSource: true,
		PreferStoredSource: true,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, VirtualSourcePath(mainDB, "main:same-session"), found.DisplayPath)

	require.NoError(t, os.Remove(mainDB))
	found, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "main:same-session",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, legacyPath, found.DisplayPath)
}

func TestOpenClawSQLiteConfiguredRootWinner(t *testing.T) {
	firstRoot := t.TempDir()
	firstDB := createOpenClawSQLiteFixture(t, firstRoot, "main", map[string][]string{
		"duplicate": openClawSQLiteFixtureEvents("first"),
	})
	secondRoot := t.TempDir()
	secondDB := createOpenClawSQLiteFixture(t, secondRoot, "main", map[string][]string{
		"duplicate":  openClawSQLiteFixtureEvents("second"),
		"later-only": openClawSQLiteFixtureEvents("later-only"),
	})
	updateOpenClawSQLiteSessionCreatedAt(t, firstDB, "duplicate", 1_700_000_000_100)
	updateOpenClawSQLiteSessionCreatedAt(t, secondDB, "duplicate", 1_700_000_000_900)
	updateOpenClawSQLiteSessionCreatedAt(t, secondDB, "later-only", 1_700_000_001_100)
	firstMTime := time.UnixMilli(1_700_000_000_100).UnixNano()
	secondDuplicateMTime := time.UnixMilli(1_700_000_000_900).UnixNano()
	laterOnlyMTime := time.UnixMilli(1_700_000_001_100).UnixNano()
	provider, ok := NewProvider(AgentOpenClaw, ProviderConfig{
		Roots: []string{firstRoot, secondRoot},
	})
	require.True(t, ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, discovered, 2)
	assert.ElementsMatch(t, []string{
		VirtualSourcePath(firstDB, "main:duplicate"),
		VirtualSourcePath(secondDB, "main:later-only"),
	}, []string{discovered[0].DisplayPath, discovered[1].DisplayPath})
	gotMTime := make(map[string]int64, len(discovered))
	for _, source := range discovered {
		gotMTime[source.DisplayPath] = source.DiscoveryMTimeNS
	}
	assert.Equal(t, map[string]int64{
		VirtualSourcePath(firstDB, "main:duplicate"):   firstMTime,
		VirtualSourcePath(secondDB, "main:later-only"): laterOnlyMTime,
	}, gotMTime)

	discoverer, ok := provider.(StreamingDiscoverer)
	require.True(t, ok)
	var streamed []SourceRef
	err = discoverer.DiscoverEach(t.Context(), func(source SourceRef) error {
		streamed = append(streamed, source)
		return nil
	})
	require.NoError(t, err)
	streamedMTime := make(map[string]int64, len(streamed))
	for _, source := range streamed {
		streamedMTime[source.DisplayPath] = source.DiscoveryMTimeNS
	}
	assert.Equal(t, gotMTime, streamedMTime)

	for _, tc := range []struct {
		name string
		make func(string) FindSourceRequest
	}{
		{
			name: "raw ID",
			make: func(string) FindSourceRequest {
				return FindSourceRequest{RawSessionID: "main:duplicate"}
			},
		},
		{
			name: "stored path",
			make: func(path string) FindSourceRequest {
				return FindSourceRequest{StoredFilePath: path}
			},
		},
		{
			name: "fingerprint key",
			make: func(path string) FindSourceRequest {
				return FindSourceRequest{FingerprintKey: path}
			},
		},
		{
			name: "preferred stored path",
			make: func(path string) FindSourceRequest {
				return FindSourceRequest{
					StoredFilePath:     path,
					PreferStoredSource: true,
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			found, ok, err := provider.FindSource(
				t.Context(), tc.make(VirtualSourcePath(secondDB, "main:duplicate")),
			)
			require.NoError(t, err)
			require.True(t, ok)
			assert.Equal(t, VirtualSourcePath(firstDB, "main:duplicate"), found.DisplayPath)
			assert.Equal(t, firstMTime, found.DiscoveryMTimeNS)
		})
	}

	laterOnly, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "main:later-only",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, VirtualSourcePath(secondDB, "main:later-only"), laterOnly.DisplayPath)
	assert.Equal(t, laterOnlyMTime, laterOnly.DiscoveryMTimeNS)

	resolver, ok := provider.(ReconciliationSourceResolver)
	require.True(t, ok)
	found, ok, err := resolver.SourceForReconciliation(
		t.Context(), VirtualSourcePath(secondDB, "main:duplicate"), "main",
	)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, VirtualSourcePath(firstDB, "main:duplicate"), found.DisplayPath)
	assert.Equal(t, firstMTime, found.DiscoveryMTimeNS)

	reversed, ok := NewProvider(AgentOpenClaw, ProviderConfig{
		Roots: []string{secondRoot, firstRoot},
	})
	require.True(t, ok)
	found, ok, err = reversed.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "main:duplicate",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, VirtualSourcePath(secondDB, "main:duplicate"), found.DisplayPath)
	assert.Equal(t, secondDuplicateMTime, found.DiscoveryMTimeNS)
}

func TestOpenClawSQLitePublicLookupContinuesPastRootError(t *testing.T) {
	brokenRoot := t.TempDir()
	writeSourceFile(t, openClawSQLiteDBPath(brokenRoot, "main"), "not sqlite")
	healthyRoot := t.TempDir()
	healthyDB := createOpenClawSQLiteFixture(t, healthyRoot, "main", map[string][]string{
		"later": openClawSQLiteFixtureEvents("later"),
	})
	updateOpenClawSQLiteSessionCreatedAt(t, healthyDB, "later", 1_700_000_002_100)
	laterMTime := time.UnixMilli(1_700_000_002_100).UnixNano()
	provider, ok := NewProvider(AgentOpenClaw, ProviderConfig{
		Roots: []string{brokenRoot, healthyRoot},
	})
	require.True(t, ok)
	wantPath := VirtualSourcePath(healthyDB, "main:later")

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "main:later",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, wantPath, found.DisplayPath)
	assert.Equal(t, laterMTime, found.DiscoveryMTimeNS)

	resolver, ok := provider.(ReconciliationSourceResolver)
	require.True(t, ok)
	found, ok, err = resolver.SourceForReconciliation(t.Context(), wantPath, "main")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, wantPath, found.DisplayPath)
	assert.Equal(t, laterMTime, found.DiscoveryMTimeNS)

	missingRoot := t.TempDir()
	provider, ok = NewProvider(AgentOpenClaw, ProviderConfig{
		Roots: []string{brokenRoot, missingRoot},
	})
	require.True(t, ok)
	found, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "main:missing",
	})
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Empty(t, found.DisplayPath)
	resolver, ok = provider.(ReconciliationSourceResolver)
	require.True(t, ok)
	found, ok, err = resolver.SourceForReconciliation(
		t.Context(), VirtualSourcePath(openClawSQLiteDBPath(brokenRoot, "main"), "main:missing"), "main",
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "file is not a database")
	assert.False(t, ok)
	assert.Empty(t, found.DisplayPath)
}

func TestOpenClawSQLiteChangedPathExpandsAndRemapsRoots(t *testing.T) {
	firstRoot := t.TempDir()
	firstDB := createOpenClawSQLiteFixture(t, firstRoot, "main", map[string][]string{
		"duplicate": openClawSQLiteFixtureEvents("first"),
	})
	secondRoot := t.TempDir()
	secondDB := createOpenClawSQLiteFixture(t, secondRoot, "main", map[string][]string{
		"duplicate":  openClawSQLiteFixtureEvents("second"),
		"later-only": openClawSQLiteFixtureEvents("later-only"),
	})
	updateOpenClawSQLiteSessionCreatedAt(t, firstDB, "duplicate", 1_700_000_000_100)
	updateOpenClawSQLiteSessionCreatedAt(t, secondDB, "duplicate", 1_700_000_000_900)
	updateOpenClawSQLiteSessionCreatedAt(t, secondDB, "later-only", 1_700_000_001_100)
	firstMTime := time.UnixMilli(1_700_000_000_100).UnixNano()
	laterOnlyMTime := time.UnixMilli(1_700_000_001_100).UnixNano()
	provider, ok := NewProvider(AgentOpenClaw, ProviderConfig{
		Roots: []string{firstRoot, secondRoot},
	})
	require.True(t, ok)
	want := []string{
		VirtualSourcePath(firstDB, "main:duplicate"),
		VirtualSourcePath(secondDB, "main:later-only"),
	}
	for _, suffix := range []string{"", "-wal", "-journal"} {
		t.Run("event "+suffix, func(t *testing.T) {
			changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
				Path:      secondDB + suffix,
				EventKind: "write",
				WatchRoot: secondRoot,
			})
			require.NoError(t, err)
			require.Len(t, changed, 2)
			assert.ElementsMatch(t, want, []string{
				changed[0].DisplayPath, changed[1].DisplayPath,
			})
			for _, source := range changed {
				if source.DisplayPath == VirtualSourcePath(firstDB, "main:duplicate") {
					assert.Equal(t, firstMTime, source.DiscoveryMTimeNS)
				}
				if source.DisplayPath == VirtualSourcePath(secondDB, "main:later-only") {
					assert.Equal(t, laterOnlyMTime, source.DiscoveryMTimeNS)
				}
			}
		})
	}

	duplicatePath := VirtualSourcePath(secondDB, "main:duplicate")
	deleteOpenClawSQLiteSession(t, secondDB, "duplicate")
	changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path: secondDB, EventKind: "write", WatchRoot: secondRoot,
		StoredSourcePaths: []string{duplicatePath},
	})
	require.NoError(t, err)
	require.Len(t, changed, 2)
	assert.ElementsMatch(t, []string{
		VirtualSourcePath(firstDB, "main:duplicate"),
		VirtualSourcePath(secondDB, "main:later-only"),
	}, []string{changed[0].DisplayPath, changed[1].DisplayPath})
	for _, source := range changed {
		if source.DisplayPath == VirtualSourcePath(firstDB, "main:duplicate") {
			assert.Equal(t, firstMTime, source.DiscoveryMTimeNS)
		}
	}

	deleteOpenClawSQLiteSession(t, firstDB, "duplicate")
	changed, err = provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path: secondDB, EventKind: "write", WatchRoot: secondRoot,
		StoredSourcePaths: []string{duplicatePath},
	})
	require.NoError(t, err)
	require.Len(t, changed, 2)
	assert.ElementsMatch(t, []string{
		duplicatePath,
		VirtualSourcePath(secondDB, "main:later-only"),
	}, []string{changed[0].DisplayPath, changed[1].DisplayPath})
}

func TestOpenClawSQLitePartialDiscoveryReturnsIncomplete(t *testing.T) {
	root := t.TempDir()
	dbPath := openClawSQLiteDBPath(root, "main")
	writeSourceFile(t, dbPath, "placeholder")
	sources := newOpenClawSourceSet([]string{root})
	sentinel := errors.New("partial session listing")
	var got []SourceRef
	err := openClawSQLiteDiscoverEachWithEnumerator(
		t.Context(), root, func(match multiSessionMatch) error {
			got = append(got, sources.sqlite.sourceRef(root, match))
			return nil
		}, func(
			ctx context.Context, path string, yield func(string, int64) error,
		) error {
			if err := yield("partial", 0); err != nil {
				return err
			}
			return sentinel
		},
	)
	var incomplete DiscoveryIncompleteError
	require.ErrorAs(t, err, &incomplete)
	require.ErrorIs(t, err, sentinel)
	require.Len(t, got, 1)
	assert.Equal(t, "main:partial", got[0].Opaque.(multiSessionSource).MemberID)
}

func TestOpenClawSQLiteChangedContainerEnumerationBoundaries(t *testing.T) {
	root := t.TempDir()
	dbPath := createOpenClawSQLiteFixture(t, root, "main", map[string][]string{
		"member": openClawSQLiteFixtureEvents("member"),
	})
	sources := newOpenClawSourceSet([]string{root})
	container := sources.sqlite.sourceRef(root, multiSessionMatch{
		Path: dbPath, Container: dbPath,
	})
	beforeErr := errors.New("listing failed before first member")
	got, err := sources.expandSQLiteChangedContainer(
		t.Context(), container,
		func(context.Context, string, func(string, int64) error) error {
			return beforeErr
		},
	)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, dbPath, got[0].DisplayPath)

	afterErr := errors.New("listing failed after first member")
	got, err = sources.expandSQLiteChangedContainer(
		t.Context(), container,
		func(
			_ context.Context, _ string, yield func(string, int64) error,
		) error {
			if err := yield("member", 0); err != nil {
				return err
			}
			return afterErr
		},
	)
	var incomplete DiscoveryIncompleteError
	require.ErrorAs(t, err, &incomplete)
	require.ErrorIs(t, err, afterErr)
	require.Len(t, got, 1)
	assert.Equal(t, VirtualSourcePath(dbPath, "main:member"), got[0].DisplayPath)
}

func TestOpenClawSQLiteChangedPathRootErrors(t *testing.T) {
	brokenRoot := t.TempDir()
	writeSourceFile(t, openClawSQLiteDBPath(brokenRoot, "main"), "not sqlite")
	healthyRoot := t.TempDir()
	healthyDB := createOpenClawSQLiteFixture(t, healthyRoot, "main", map[string][]string{
		"later": openClawSQLiteFixtureEvents("later"),
	})
	provider, ok := NewProvider(AgentOpenClaw, ProviderConfig{
		Roots: []string{brokenRoot, healthyRoot},
	})
	require.True(t, ok)
	changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path: healthyDB, EventKind: "write", WatchRoot: healthyRoot,
	})
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, VirtualSourcePath(healthyDB, "main:later"), changed[0].DisplayPath)

	openClaw, ok := provider.(*openClawProvider)
	require.True(t, ok)
	container := openClaw.sources.sqlite.sourceRef(healthyRoot, multiSessionMatch{
		Path: healthyDB, Container: healthyDB,
	})
	_, err = openClaw.sources.expandSQLiteChangedContainer(
		t.Context(), container,
		func(
			_ context.Context, _ string, yield func(string, int64) error,
		) error {
			return yield("missing", 0)
		},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "file is not a database")
}

func TestOpenClawSQLiteChangedContainerStopsOnDeadlineAfterMember(t *testing.T) {
	root := t.TempDir()
	dbPath := createOpenClawSQLiteFixture(t, root, "main", map[string][]string{
		"member": openClawSQLiteFixtureEvents("member"),
	})
	sources := newOpenClawSourceSet([]string{root})
	container := sources.sqlite.sourceRef(root, multiSessionMatch{
		Path: dbPath, Container: dbPath,
	})
	got, err := sources.expandSQLiteChangedContainer(
		t.Context(), container,
		func(
			_ context.Context, _ string, yield func(string, int64) error,
		) error {
			if err := yield("member", 0); err != nil {
				return err
			}
			return context.DeadlineExceeded
		},
	)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Len(t, got, 1)
	assert.Equal(t, VirtualSourcePath(dbPath, "main:member"), got[0].DisplayPath)
}

func TestOpenClawSQLitePartialDatabaseKeepsHealthySibling(t *testing.T) {
	root := t.TempDir()
	partialDB := openClawSQLiteDBPath(root, "aaa")
	healthyDB := openClawSQLiteDBPath(root, "main")
	writeSourceFile(t, partialDB, "placeholder")
	writeSourceFile(t, healthyDB, "placeholder")
	sentinel := errors.New("partial database")
	var got []multiSessionMatch
	err := openClawSQLiteDiscoverEachWithEnumerator(
		t.Context(), root, func(match multiSessionMatch) error {
			got = append(got, match)
			return nil
		}, func(
			_ context.Context, path string, yield func(string, int64) error,
		) error {
			if path == partialDB {
				if err := yield("partial", 0); err != nil {
					return err
				}
				return sentinel
			}
			return yield("healthy", 0)
		},
	)
	var incomplete DiscoveryIncompleteError
	require.ErrorAs(t, err, &incomplete)
	require.ErrorIs(t, err, sentinel)
	require.Len(t, got, 2)
	assert.ElementsMatch(t, []string{
		"aaa:partial", "main:healthy",
	}, []string{got[0].MemberID, got[1].MemberID})
}

func TestOpenClawSQLiteDiscoveryAggregatesRootIncomplete(t *testing.T) {
	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	legacyPath := filepath.Join(secondRoot, "legacy", "sessions", "legacy.jsonl")
	writeSourceFile(t, legacyPath, clawProviderFixture("legacy", "legacy"))
	sources := newOpenClawSourceSet([]string{firstRoot, secondRoot})
	sentinel := errors.New("incomplete root")
	sources.sqlite.cfg.discoverEach = func(
		_ context.Context, root string, yield func(multiSessionMatch) error,
	) error {
		if root == firstRoot {
			if err := yield(multiSessionMatch{
				Path:      VirtualSourcePath(openClawSQLiteDBPath(root, "main"), "main:partial"),
				Container: openClawSQLiteDBPath(root, "main"),
				MemberID:  "main:partial",
			}); err != nil {
				return err
			}
			return incompleteDiscoveryError(AgentOpenClaw, "test root", sentinel)
		}
		return yield(multiSessionMatch{
			Path:      VirtualSourcePath(openClawSQLiteDBPath(root, "main"), "main:later"),
			Container: openClawSQLiteDBPath(root, "main"),
			MemberID:  "main:later",
		})
	}
	var got []SourceRef
	err := sources.DiscoverEach(t.Context(), func(source SourceRef) error {
		got = append(got, source)
		return nil
	})
	var incomplete DiscoveryIncompleteError
	require.ErrorAs(t, err, &incomplete)
	require.ErrorIs(t, err, sentinel)
	assert.ElementsMatch(t, []string{
		VirtualSourcePath(openClawSQLiteDBPath(firstRoot, "main"), "main:partial"),
		VirtualSourcePath(openClawSQLiteDBPath(secondRoot, "main"), "main:later"),
		legacyPath,
	}, []string{got[0].DisplayPath, got[1].DisplayPath, got[2].DisplayPath})
}

func TestOpenClawSQLiteDiscoveryReturnsConsumerIncomplete(t *testing.T) {
	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	legacyPath := filepath.Join(firstRoot, "other", "sessions", "legacy.jsonl")
	writeSourceFile(t, legacyPath, clawProviderFixture("legacy", "legacy"))
	sources := newOpenClawSourceSet([]string{firstRoot, secondRoot})
	consumerErr := DiscoveryIncompleteError{
		Provider: AgentOpenClaw,
		Reason:   "consumer stopped discovery",
	}
	var rootsVisited []string
	sources.sqlite.cfg.discoverEach = func(
		_ context.Context, root string, yield func(multiSessionMatch) error,
	) error {
		rootsVisited = append(rootsVisited, root)
		return yield(multiSessionMatch{
			Path:      VirtualSourcePath(openClawSQLiteDBPath(root, "main"), "main:consumer"),
			Container: openClawSQLiteDBPath(root, "main"),
			MemberID:  "main:consumer",
		})
	}
	var yielded int
	err := sources.DiscoverEach(t.Context(), func(SourceRef) error {
		yielded++
		return consumerErr
	})
	require.Equal(t, consumerErr, err)
	assert.Equal(t, 1, yielded)
	assert.Equal(t, []string{firstRoot}, rootsVisited)
}

func TestOpenClawSQLiteDiscoveryStopsOnCancellationAfterMember(t *testing.T) {
	root := t.TempDir()
	createOpenClawSQLiteFixture(t, root, "main", map[string][]string{
		"one": openClawSQLiteFixtureEvents("one"),
		"two": openClawSQLiteFixtureEvents("two"),
	})
	writeSourceFile(t, filepath.Join(root, "other", "sessions", "legacy.jsonl"),
		clawProviderFixture("legacy", "legacy"))
	provider, ok := NewProvider(AgentOpenClaw, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	discoverer, ok := provider.(StreamingDiscoverer)
	require.True(t, ok)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	yields := 0
	err := discoverer.DiscoverEach(ctx, func(SourceRef) error {
		yields++
		cancel()
		return ctx.Err()
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, yields)
}

func TestOpenClawSQLiteFindSourceStoredHints(t *testing.T) {
	hintFields := []struct {
		name string
		set  func(*FindSourceRequest, string)
	}{
		{
			name: "StoredFilePath",
			set: func(req *FindSourceRequest, path string) {
				req.StoredFilePath = path
			},
		},
		{
			name: "FingerprintKey",
			set: func(req *FindSourceRequest, path string) {
				req.FingerprintKey = path
			},
		},
	}
	cases := []struct {
		name  string
		setup func(t *testing.T) (roots []string, hint, legacyPath string)
	}{
		{
			name: "mismatched live member",
			setup: func(t *testing.T) ([]string, string, string) {
				t.Helper()
				root := t.TempDir()
				dbPath := createOpenClawSQLiteFixture(t, root, "main", map[string][]string{
					"other": openClawSQLiteFixtureEvents("other"),
				})
				legacyPath := filepath.Join(root, "main", "sessions", "wanted.jsonl")
				writeSourceFile(t, legacyPath, clawProviderFixture("wanted", "legacy wanted"))
				return []string{root}, VirtualSourcePath(dbPath, "main:other"), legacyPath
			},
		},
		{
			name: "stat error",
			setup: func(t *testing.T) ([]string, string, string) {
				t.Helper()
				brokenRoot := t.TempDir()
				writeSourceFile(t, filepath.Join(brokenRoot, "main"), "not a directory")
				goodRoot := t.TempDir()
				legacyPath := filepath.Join(goodRoot, "main", "sessions", "wanted.jsonl")
				writeSourceFile(t, legacyPath, clawProviderFixture("wanted", "legacy wanted"))
				hint := VirtualSourcePath(
					filepath.Join(brokenRoot, "main", "agent", openClawSQLiteDBName),
					"main:wanted",
				)
				return []string{brokenRoot, goodRoot}, hint, legacyPath
			},
		},
		{
			name: "SQLite schema error",
			setup: func(t *testing.T) ([]string, string, string) {
				t.Helper()
				root := t.TempDir()
				dbPath := openClawSQLiteDBPath(root, "main")
				createOpenClawSQLiteSchema(t, dbPath, `
					CREATE TABLE transcript_events (session_id TEXT NOT NULL);
				`)
				legacyPath := filepath.Join(root, "main", "sessions", "wanted.jsonl")
				writeSourceFile(t, legacyPath, clawProviderFixture("wanted", "legacy wanted"))
				return []string{root}, VirtualSourcePath(dbPath, "main:wanted"), legacyPath
			},
		},
	}

	for _, field := range hintFields {
		t.Run(field.name, func(t *testing.T) {
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					roots, hint, legacyPath := tc.setup(t)
					provider, ok := NewProvider(AgentOpenClaw, ProviderConfig{Roots: roots})
					require.True(t, ok)

					req := FindSourceRequest{RawSessionID: "main:wanted"}
					field.set(&req, hint)
					found, foundOK, err := provider.FindSource(t.Context(), req)
					require.NoError(t, err)
					require.True(t, foundOK)
					assert.Equal(t, legacyPath, found.DisplayPath)
					logicalID, valid := openClawLogicalSourceID(found)
					require.True(t, valid)
					assert.Equal(t, "main:wanted", logicalID)
				})
			}
		})
	}

	for _, tc := range []struct {
		name string
		ctx  func(t *testing.T) context.Context
		want error
	}{
		{
			name: "already canceled",
			ctx: func(t *testing.T) context.Context {
				t.Helper()
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				return ctx
			},
			want: context.Canceled,
		},
		{
			name: "expired deadline",
			ctx: func(t *testing.T) context.Context {
				t.Helper()
				ctx, cancel := context.WithDeadline(
					t.Context(), time.Now().Add(-time.Second),
				)
				t.Cleanup(cancel)
				return ctx
			},
			want: context.DeadlineExceeded,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			legacyPath := filepath.Join(root, "main", "sessions", "preferred.jsonl")
			writeSourceFile(t, legacyPath, clawProviderFixture("preferred", "preferred legacy"))
			provider, ok := NewProvider(AgentOpenClaw, ProviderConfig{Roots: []string{root}})
			require.True(t, ok)

			found, foundOK, err := provider.FindSource(tc.ctx(t), FindSourceRequest{
				RawSessionID:       "main:preferred",
				StoredFilePath:     legacyPath,
				PreferStoredSource: true,
			})
			require.ErrorIs(t, err, tc.want)
			assert.False(t, foundOK)
			assert.Empty(t, found.DisplayPath)
		})
	}
}

func TestOpenClawSQLiteStoreSupersedesLegacyJSONL(t *testing.T) {
	root := t.TempDir()
	dbPath := createOpenClawSQLiteFixture(t, root, "main", map[string][]string{
		"duplicate":   openClawSQLiteFixtureEvents("duplicate"),
		"sqlite-only": openClawSQLiteFixtureEvents("sqlite-only"),
	})
	legacyDuplicate := filepath.Join(root, "main", "sessions", "duplicate.jsonl")
	legacyOnly := filepath.Join(root, "main", "sessions", "legacy-only.jsonl")
	writeSourceFile(t, legacyDuplicate, clawProviderFixture("duplicate", "legacy"))
	writeSourceFile(t, legacyOnly, clawProviderFixture("legacy-only", "legacy only"))

	provider, ok := NewProvider(AgentOpenClaw, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, discovered, 3)
	for _, source := range discovered {
		assert.NotEqual(t, legacyDuplicate, source.DisplayPath)
	}

	virtual := VirtualSourcePath(dbPath, "main:duplicate")
	changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path: dbPath, EventKind: "write", WatchRoot: root,
		StoredSourcePaths: []string{virtual},
	})
	require.NoError(t, err)
	require.Len(t, changed, 2)
	assert.ElementsMatch(t, []string{
		VirtualSourcePath(dbPath, "main:duplicate"),
		VirtualSourcePath(dbPath, "main:sqlite-only"),
	}, []string{changed[0].DisplayPath, changed[1].DisplayPath})
	changed, err = provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path: legacyDuplicate, EventKind: "write", WatchRoot: root,
	})
	require.NoError(t, err)
	assert.Empty(t, changed)
	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath: legacyDuplicate,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, virtual, found.DisplayPath)

	deleteOpenClawSQLiteSession(t, dbPath, "duplicate")
	changed, err = provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path: dbPath, EventKind: "write", WatchRoot: root,
		StoredSourcePaths: []string{virtual},
	})
	require.NoError(t, err)
	require.Len(t, changed, 2)
	for _, source := range changed {
		assert.NotEqual(t, legacyDuplicate, source.DisplayPath)
	}
	changed, err = provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path: legacyDuplicate, EventKind: "write", WatchRoot: root,
	})
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, legacyDuplicate, changed[0].DisplayPath)
	fingerprint, err := provider.Fingerprint(t.Context(), changed[0])
	require.NoError(t, err)
	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source: changed[0], Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	assert.True(t, outcome.ForceReplace)
	assert.Equal(t, "legacy", outcome.Results[0].Result.Messages[0].Content)

	found, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "main:duplicate",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, legacyDuplicate, found.DisplayPath)
}

func TestOpenClawSQLiteStoreDeletionSafety(t *testing.T) {
	root := t.TempDir()
	dbPath := createOpenClawSQLiteFixture(t, root, "main", map[string][]string{
		"keep": openClawSQLiteFixtureEvents("keep"),
		"drop": openClawSQLiteFixtureEvents("drop"),
	})
	provider, ok := NewProvider(AgentOpenClaw, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	dropPath := VirtualSourcePath(dbPath, "main:drop")
	deleteOpenClawSQLiteSession(t, dbPath, "drop")
	changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path: dbPath, EventKind: "write", WatchRoot: root,
		StoredSourcePaths: []string{dropPath},
	})
	require.NoError(t, err)
	require.Len(t, changed, 2)
	var keep, tombstone SourceRef
	for _, source := range changed {
		if source.DisplayPath == VirtualSourcePath(dbPath, "main:keep") {
			keep = source
		}
		if source.DisplayPath == dropPath {
			tombstone = source
		}
	}
	require.NotEmpty(t, keep.DisplayPath)
	require.NotEmpty(t, tombstone.DisplayPath)
	keepOutcome, err := provider.Parse(t.Context(), ParseRequest{Source: keep})
	require.NoError(t, err)
	require.Len(t, keepOutcome.Results, 1)
	assert.Equal(t, "openclaw:main:keep", keepOutcome.Results[0].Result.Session.ID)
	tombstoneOutcome, err := provider.Parse(t.Context(), ParseRequest{Source: tombstone})
	require.NoError(t, err)
	assert.Empty(t, tombstoneOutcome.Results)
	assert.True(t, tombstoneOutcome.ForceReplace)

	missingRoot := t.TempDir()
	missingDB := createOpenClawSQLiteFixture(t, missingRoot, "main", map[string][]string{
		"missing": openClawSQLiteFixtureEvents("missing"),
	})
	missingProvider, ok := NewProvider(AgentOpenClaw, ProviderConfig{
		Roots: []string{missingRoot},
	})
	require.True(t, ok)
	missingPath := VirtualSourcePath(missingDB, "main:missing")
	require.NoError(t, os.Remove(missingDB))
	changed, err = missingProvider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path: missingDB, EventKind: "remove", WatchRoot: missingRoot,
		StoredSourcePaths: []string{missingPath},
	})
	require.NoError(t, err)
	require.Len(t, changed, 1)
	missingOutcome, err := missingProvider.Parse(t.Context(), ParseRequest{
		Source: changed[0],
	})
	require.NoError(t, err)
	assert.True(t, missingOutcome.ResultSetComplete)
	assert.False(t, missingOutcome.ForceReplace)
	assert.Equal(t, SkipNoSession, missingOutcome.SkipReason)
}

func TestOpenClawSQLiteStoreDeletionKeepsSQLiteArchiveSource(t *testing.T) {
	root := t.TempDir()
	dbPath := createOpenClawSQLiteFixture(t, root, "main", map[string][]string{
		"fallback": openClawSQLiteFixtureEvents("fallback"),
	})
	legacyPath := filepath.Join(root, "main", "sessions", "fallback.jsonl")
	writeSourceFile(t, legacyPath, clawProviderFixture("fallback", "legacy fallback"))
	provider, ok := NewProvider(AgentOpenClaw, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	storedPath := VirtualSourcePath(dbPath, "main:fallback")
	require.NoError(t, os.Remove(dbPath))

	changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path: dbPath, EventKind: "remove", WatchRoot: root,
		StoredSourcePaths: []string{storedPath},
	})
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, dbPath, changed[0].DisplayPath)
	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source: changed[0],
	})
	require.NoError(t, err)
	assert.Empty(t, outcome.Results)
	assert.False(t, outcome.ForceReplace)
	assert.Equal(t, SkipNoSession, outcome.SkipReason)
	assert.NotEqual(t, legacyPath, changed[0].DisplayPath)
}

func TestOpenClawSQLiteSourceForReconciliationMissingDatabase(t *testing.T) {
	root := t.TempDir()
	dbPath := createOpenClawSQLiteFixture(t, root, "main", map[string][]string{
		"archived": openClawSQLiteFixtureEvents("archived"),
	})
	storedPath := VirtualSourcePath(dbPath, "main:archived")
	provider, ok := NewProvider(AgentOpenClaw, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	require.NoError(t, os.Remove(dbPath))

	resolver, ok := provider.(ReconciliationSourceResolver)
	require.True(t, ok)
	source, found, err := resolver.SourceForReconciliation(
		t.Context(), storedPath, "main",
	)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, storedPath, source.DisplayPath)
	assert.Equal(t, "main:archived", source.ReconciliationIdentity)

	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	assert.Empty(t, discovered)
	_, found, err = provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "main:archived",
	})
	require.NoError(t, err)
	assert.False(t, found)
}

func TestOpenClawSQLiteFindMemberContinuesPastRootError(t *testing.T) {
	brokenRoot := t.TempDir()
	writeSourceFile(t, openClawSQLiteDBPath(brokenRoot, "main"), "not a sqlite database")
	healthyRoot := t.TempDir()
	dbPath := createOpenClawSQLiteFixture(t, healthyRoot, "main", map[string][]string{
		"later": openClawSQLiteFixtureEvents("later"),
	})
	provider, ok := NewProvider(AgentOpenClaw, ProviderConfig{
		Roots: []string{brokenRoot, healthyRoot},
	})
	require.True(t, ok)

	openClaw, ok := provider.(*openClawProvider)
	require.True(t, ok)
	source, found, err := openClaw.sources.findSQLiteMember(
		t.Context(), "main:later",
	)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, VirtualSourcePath(dbPath, "main:later"), source.DisplayPath)
}

func TestOpenClawSQLiteFindMemberReturnsRememberedRootError(t *testing.T) {
	brokenRoot := t.TempDir()
	writeSourceFile(t, openClawSQLiteDBPath(brokenRoot, "main"), "not a sqlite database")
	emptyRoot := t.TempDir()
	provider, ok := NewProvider(AgentOpenClaw, ProviderConfig{
		Roots: []string{brokenRoot, emptyRoot},
	})
	require.True(t, ok)
	openClaw, ok := provider.(*openClawProvider)
	require.True(t, ok)

	source, found, err := openClaw.sources.findSQLiteMember(
		t.Context(), "main:missing",
	)
	require.Error(t, err)
	assert.False(t, found)
	assert.Empty(t, source.DisplayPath)
	assert.Contains(t, err.Error(), "file is not a database")

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, found, err = openClaw.sources.findSQLiteMember(canceled, "main:missing")
	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, found)

	deadline, cancel := context.WithDeadline(
		t.Context(), time.Now().Add(-time.Second),
	)
	t.Cleanup(cancel)
	_, found, err = openClaw.sources.findSQLiteMember(deadline, "main:missing")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.False(t, found)
}

func TestOpenClawSQLiteStoreDiscoveryAndLookupErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func(t *testing.T, path string)
	}{
		{
			name: "SQL error",
			make: func(t *testing.T, path string) {
				t.Helper()
				writeSourceFile(t, path, "not a sqlite database")
			},
		},
		{
			name: "unsupported schema",
			make: func(t *testing.T, path string) {
				t.Helper()
				createOpenClawSQLiteSchema(t, path, `
					CREATE TABLE transcript_events (session_id TEXT NOT NULL);
				`)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dbPath := filepath.Join(root, "main", "agent", openClawSQLiteDBName)
			tc.make(t, dbPath)
			provider, ok := NewProvider(AgentOpenClaw, ProviderConfig{
				Roots: []string{root},
			})
			require.True(t, ok)

			discovered, err := provider.Discover(t.Context())
			require.NoError(t, err)
			require.Len(t, discovered, 1)
			assert.Equal(t, dbPath, discovered[0].DisplayPath)

			discoverer, ok := provider.(StreamingDiscoverer)
			require.True(t, ok)
			err = discoverer.DiscoverEach(t.Context(), func(SourceRef) error {
				return nil
			})
			require.NoError(t, err)

			_, found, err := provider.FindSource(t.Context(), FindSourceRequest{
				RawSessionID: "main:missing",
			})
			require.NoError(t, err)
			assert.False(t, found)

			changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
				Path: dbPath, EventKind: "write", WatchRoot: root,
			})
			require.NoError(t, err)
			require.Len(t, changed, 1)
			_, err = provider.Parse(t.Context(), ParseRequest{Source: changed[0]})
			require.Error(t, err)
		})
	}
}

func TestOpenClawSQLiteDiscoveryContinuesPastUnreadableDatabase(t *testing.T) {
	root := t.TempDir()
	badDB := openClawSQLiteDBPath(root, "aaa")
	writeSourceFile(t, badDB, "not a sqlite database")
	goodDB := createOpenClawSQLiteFixture(t, root, "main", map[string][]string{
		"sqlite": openClawSQLiteFixtureEvents("sqlite"),
	})
	legacyPath := filepath.Join(root, "other", "sessions", "legacy.jsonl")
	writeSourceFile(t, legacyPath, clawProviderFixture("legacy", "legacy question"))
	provider, ok := NewProvider(AgentOpenClaw, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, discovered, 3)
	paths := []string{
		discovered[0].DisplayPath,
		discovered[1].DisplayPath,
		discovered[2].DisplayPath,
	}
	assert.Contains(t, paths, badDB)
	assert.Contains(t, paths, VirtualSourcePath(goodDB, "main:sqlite"))
	assert.Contains(t, paths, legacyPath)
}

func TestOpenClawSQLiteDiscoveryContinuesPastDatabaseStatError(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permissions do not yield EACCES in this environment")
	}
	root := t.TempDir()
	blocked := filepath.Join(root, "aaa", "agent")
	require.NoError(t, os.MkdirAll(blocked, 0o755))
	require.NoError(t, os.Chmod(blocked, 0o000))
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })
	goodDB := createOpenClawSQLiteFixture(t, root, "main", map[string][]string{
		"sqlite": openClawSQLiteFixtureEvents("sqlite"),
	})
	legacyPath := filepath.Join(root, "other", "sessions", "legacy.jsonl")
	writeSourceFile(t, legacyPath, clawProviderFixture("legacy", "legacy question"))
	provider, ok := NewProvider(AgentOpenClaw, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, discovered, 3)
	paths := []string{
		discovered[0].DisplayPath,
		discovered[1].DisplayPath,
		discovered[2].DisplayPath,
	}
	assert.Contains(t, paths, openClawSQLiteDBPath(root, "aaa"))
	assert.Contains(t, paths, VirtualSourcePath(goodDB, "main:sqlite"))
	assert.Contains(t, paths, legacyPath)
}

func TestOpenClawSQLiteDiscoveryStopsOnYieldError(t *testing.T) {
	root := t.TempDir()
	createOpenClawSQLiteFixture(t, root, "main", map[string][]string{
		"sqlite": openClawSQLiteFixtureEvents("sqlite"),
	})
	writeSourceFile(t, filepath.Join(root, "other", "sessions", "legacy.jsonl"),
		clawProviderFixture("legacy", "legacy question"))
	provider, ok := NewProvider(AgentOpenClaw, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	discoverer := provider.(StreamingDiscoverer)
	wantErr := errors.New("stop")
	yields := 0

	err := discoverer.DiscoverEach(t.Context(), func(SourceRef) error {
		yields++
		return wantErr
	})

	require.ErrorIs(t, err, wantErr)
	assert.Equal(t, 1, yields)
}

func TestOpenClawSQLiteSessionIDStreamingStopsOnYieldError(t *testing.T) {
	root := t.TempDir()
	dbPath := createOpenClawSQLiteFixture(t, root, "main", map[string][]string{
		"one": openClawSQLiteFixtureEvents("one"),
		"two": openClawSQLiteFixtureEvents("two"),
	})
	wantErr := errors.New("stop")
	yields := 0

	err := openClawSQLiteSessionIDsEach(t.Context(), dbPath, func(string, int64) error {
		yields++
		return wantErr
	})

	require.ErrorIs(t, err, wantErr)
	assert.Equal(t, 1, yields)
}

func TestOpenClawSQLiteDecoderParity(t *testing.T) {
	events := []string{
		`{"type":"session","version":3,"id":"parity","timestamp":"2026-09-22T10:00:00Z","cwd":"/workspace/project-a"}`,
		`{"type":"message","id":"m1","timestamp":"2026-09-22T10:00:01Z","message":{"role":"user","content":[{"type":"text","text":"hello"}],"timestamp":"2026-09-22T10:00:01Z"}}`,
		`{"type":"message","id":"m2","timestamp":"2026-09-22T10:00:02Z","message":{"role":"assistant","model":"gpt-test","usage":{"input":10,"output":4,"cacheRead":2,"cacheWrite":1},"content":[{"type":"thinking","thinking":"plan"},{"type":"toolCall","id":"tool-1","name":"read","input":{"path":"/tmp/file"}}],"timestamp":"2026-09-22T10:00:02Z"}}`,
		`{"type":"message","id":"m3","timestamp":"2026-09-22T10:00:03Z","message":{"role":"toolResult","toolCallId":"tool-1","content":[{"type":"toolResult","text":"contents"}],"timestamp":"2026-09-22T10:00:03Z"}}`,
		`{"type":"message","id":"m4","timestamp":"2026-09-22T10:00:04Z","message":{"role":"assistant","content":[{"type":"text","text":"done"}],"timestamp":"2026-09-22T10:00:04Z"}}`,
	}
	jsonRoot := t.TempDir()
	jsonPath := filepath.Join(jsonRoot, "main", "sessions", "parity.jsonl")
	writeSourceFile(t, jsonPath, strings.Join(events, "\n")+"\n")
	jsonProvider, ok := NewProvider(AgentOpenClaw, ProviderConfig{Roots: []string{jsonRoot}})
	require.True(t, ok)
	jsonSource, ok, err := jsonProvider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "main:parity",
	})
	require.NoError(t, err)
	require.True(t, ok)
	jsonFingerprint, err := jsonProvider.Fingerprint(t.Context(), jsonSource)
	require.NoError(t, err)
	jsonOutcome, err := jsonProvider.Parse(t.Context(), ParseRequest{
		Source: jsonSource, Fingerprint: jsonFingerprint,
	})
	require.NoError(t, err)
	require.Len(t, jsonOutcome.Results, 1)

	sqliteRoot := t.TempDir()
	dbPath := createOpenClawSQLiteFixture(t, sqliteRoot, "main", map[string][]string{
		"parity": events,
	})
	sqliteProvider, ok := NewProvider(AgentOpenClaw, ProviderConfig{
		Roots: []string{sqliteRoot},
	})
	require.True(t, ok)
	sources, err := sqliteProvider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	sqliteFingerprint, err := sqliteProvider.Fingerprint(t.Context(), sources[0])
	require.NoError(t, err)
	sqliteOutcome, err := sqliteProvider.Parse(t.Context(), ParseRequest{
		Source: sources[0], Fingerprint: sqliteFingerprint,
	})
	require.NoError(t, err)
	require.Len(t, sqliteOutcome.Results, 1)

	jsonResult := jsonOutcome.Results[0].Result
	sqliteResult := sqliteOutcome.Results[0].Result
	assert.Equal(t, jsonResult.Session.ID, sqliteResult.Session.ID)
	assert.Equal(t, jsonResult.Session.Project, sqliteResult.Session.Project)
	assert.Equal(t, jsonResult.Session.FirstMessage, sqliteResult.Session.FirstMessage)
	assert.Equal(t, jsonResult.Session.StartedAt, sqliteResult.Session.StartedAt)
	assert.Equal(t, jsonResult.Session.EndedAt, sqliteResult.Session.EndedAt)
	assert.Equal(t, jsonResult.Session.MessageCount, sqliteResult.Session.MessageCount)
	assert.Equal(t, jsonResult.Session.UserMessageCount, sqliteResult.Session.UserMessageCount)
	assert.Equal(t, jsonResult.Messages, sqliteResult.Messages)
	assert.Equal(t, dbPath+"#main:parity", sqliteResult.Session.File.Path)
}

func TestOpenClawSQLiteStoreChangedPathRouting(t *testing.T) {
	root := t.TempDir()
	dbPath := createOpenClawSQLiteFixture(t, root, "main", map[string][]string{
		"routing": openClawSQLiteFixtureEvents("routing"),
	})
	provider, ok := NewProvider(AgentOpenClaw, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	for _, suffix := range []string{"", "-wal", "-journal"} {
		t.Run("routes "+suffix, func(t *testing.T) {
			changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
				Path:      dbPath + suffix,
				EventKind: "write",
				WatchRoot: root,
			})
			require.NoError(t, err)
			require.Len(t, changed, 1)
			assert.Equal(t, VirtualSourcePath(dbPath, "main:routing"), changed[0].DisplayPath)
		})
	}
	for _, path := range []string{
		dbPath + "-shm",
		filepath.Join(root, "wrong", "openclaw-agent.sqlite"),
		filepath.Join(t.TempDir(), "main", "agent", openClawSQLiteDBName),
	} {
		t.Run("rejects "+filepath.Base(path), func(t *testing.T) {
			changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
				Path:      path,
				EventKind: "write",
				WatchRoot: root,
			})
			require.NoError(t, err)
			assert.Empty(t, changed)
		})
	}
}

func TestOpenClawSQLiteStoreFingerprints(t *testing.T) {
	root := t.TempDir()
	dbPath := createOpenClawSQLiteFixture(t, root, "main", map[string][]string{
		"one": openClawSQLiteFixtureEvents("one"),
		"two": openClawSQLiteFixtureEvents("two"),
	})
	provider, ok := NewProvider(AgentOpenClaw, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 2)
	first, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(t, err)
	second, err := provider.Fingerprint(t.Context(), sources[1])
	require.NoError(t, err)
	require.NotEqual(t, first.Hash, second.Hash)

	info, err := os.Stat(dbPath)
	require.NoError(t, err)
	updated := strings.Replace(
		openClawSQLiteFixtureEvents("one")[1],
		"hello from sqlite", "hello from sqlitE", 1,
	)
	updateOpenClawSQLiteEvent(t, dbPath, "one", 1, updated)
	require.NoError(t, os.Chtimes(dbPath, info.ModTime(), info.ModTime()))

	updatedFingerprint, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(t, err)
	assert.NotEqual(t, first.Hash, updatedFingerprint.Hash)
	unchanged, err := provider.Fingerprint(t.Context(), sources[1])
	require.NoError(t, err)
	assert.Equal(t, second.Hash, unchanged.Hash)

	walDB, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = walDB.Close() })
	_, err = walDB.ExecContext(t.Context(), `PRAGMA journal_mode = WAL`)
	require.NoError(t, err)
	_, err = walDB.ExecContext(t.Context(), `
		INSERT INTO transcript_events(session_id, seq, event_json, created_at)
		VALUES (?, ?, ?, ?)
	`, "one", 99,
		`{"type":"message","timestamp":"2026-09-22T10:00:09Z","message":{"role":"assistant","content":[{"type":"text","text":"wal append"}]}}`,
		1_700_000_000_099,
	)
	require.NoError(t, err)
	walFingerprint, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(t, err)
	walOutcome, err := provider.Parse(t.Context(), ParseRequest{
		Source: sources[0], Fingerprint: walFingerprint,
	})
	require.NoError(t, err)
	require.Len(t, walOutcome.Results, 1)
	assert.Equal(t, walFingerprint.MTimeNS,
		walOutcome.Results[0].Result.Session.File.Mtime)
	assert.Equal(t, "wal append", walOutcome.Results[0].Result.Messages[len(
		walOutcome.Results[0].Result.Messages)-1].Content)
}

func TestOpenClawSQLiteStoreMalformedMemberIsolation(t *testing.T) {
	root := t.TempDir()
	dbPath := createOpenClawSQLiteFixture(t, root, "main", map[string][]string{
		"good": openClawSQLiteFixtureEvents("good"),
		"bad": {
			`{"type":"session","id":"wrong-header","timestamp":"2026-09-22T10:00:00Z"}`,
			`{"type":"message","message":{"role":"user","content":"bad"}}`,
		},
		"bad-json": {`{"type":"session"`},
	})
	provider, ok := NewProvider(AgentOpenClaw, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	openClaw, ok := provider.(*openClawProvider)
	require.True(t, ok)
	container := openClaw.sources.sqlite.sourceRef(root, multiSessionMatch{
		Path: dbPath, Container: dbPath,
	})
	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: container})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, "openclaw:main:good", outcome.Results[0].Result.Session.ID)
	require.Len(t, outcome.SourceErrors, 2)
	assert.ElementsMatch(t, []string{"openclaw:main:bad", "openclaw:main:bad-json"},
		[]string{outcome.SourceErrors[0].SessionID, outcome.SourceErrors[1].SessionID})
	assert.False(t, outcome.ResultSetComplete)
	assert.True(t, outcome.ForceReplace)
	assert.Error(t, outcome.SourceErrors[0].Err)
}

func TestOpenClawSQLiteInvalidSessionIDDoesNotBlockValidMember(t *testing.T) {
	root := t.TempDir()
	createOpenClawSQLiteFixture(t, root, "main", map[string][]string{
		"valid":      openClawSQLiteFixtureEvents("valid"),
		"invalid:id": openClawSQLiteFixtureEvents("invalid:id"),
	})
	provider, ok := NewProvider(AgentOpenClaw, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Contains(t, discovered[0].DisplayPath, "#main:valid")
}

func TestQClawIgnoresOpenClawSQLiteStore(t *testing.T) {
	root := t.TempDir()
	createOpenClawSQLiteFixture(t, root, "main", map[string][]string{
		"qclaw-negative": openClawSQLiteFixtureEvents("qclaw-negative"),
	})
	provider, ok := NewProvider(AgentQClaw, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	assert.Empty(t, discovered)
	plan, err := provider.WatchPlan(t.Context())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, []string{"*.jsonl", "*.jsonl.*"}, plan.Roots[0].IncludeGlobs)
}

func createOpenClawSQLiteFixture(
	t *testing.T, root, agentID string, sessions map[string][]string,
) string {
	t.Helper()
	dbPath := openClawSQLiteDBPath(root, agentID)
	require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), 0o755))
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(t.Context(), `
		CREATE TABLE transcript_events (
			session_id TEXT NOT NULL,
			seq INTEGER NOT NULL,
			event_json TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			PRIMARY KEY (session_id, seq)
		) STRICT
	`)
	require.NoError(t, err)
	for sessionID, events := range sessions {
		for seq, event := range events {
			_, err = db.ExecContext(t.Context(),
				`INSERT INTO transcript_events(session_id, seq, event_json, created_at) VALUES (?, ?, ?, ?)`,
				sessionID, seq, event, 1_700_000_000_000+seq,
			)
			require.NoError(t, err)
		}
	}
	require.NoError(t, db.Close())
	return dbPath
}

func updateOpenClawSQLiteEvent(
	t *testing.T, dbPath, sessionID string, seq int, event string,
) {
	t.Helper()
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer db.Close()
	_, err = db.ExecContext(t.Context(),
		`UPDATE transcript_events SET event_json = ? WHERE session_id = ? AND seq = ?`,
		event, sessionID, seq,
	)
	require.NoError(t, err)
}

func updateOpenClawSQLiteSessionCreatedAt(
	t *testing.T, dbPath, sessionID string, createdAt int64,
) {
	t.Helper()
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer db.Close()
	_, err = db.ExecContext(t.Context(), `
		UPDATE transcript_events SET created_at = ? WHERE session_id = ?
	`, createdAt, sessionID)
	require.NoError(t, err)
}

func deleteOpenClawSQLiteSession(t *testing.T, dbPath, sessionID string) {
	t.Helper()
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer db.Close()
	_, err = db.ExecContext(t.Context(),
		`DELETE FROM transcript_events WHERE session_id = ?`, sessionID,
	)
	require.NoError(t, err)
}

func createOpenClawSQLiteSchema(t *testing.T, dbPath, schemaSQL string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), 0o755))
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer db.Close()
	_, err = db.ExecContext(t.Context(), schemaSQL)
	require.NoError(t, err)
}

func openClawSQLiteFixtureEvents(sessionID string) []string {
	return []string{
		`{"type":"session","version":3,"id":"` + sessionID + `","timestamp":"2026-09-22T10:00:00Z","cwd":"/workspace/project-a"}`,
		`{"type":"message","id":"m1","timestamp":"2026-09-22T10:00:01Z","message":{"role":"user","content":"hello from sqlite","timestamp":"2026-09-22T10:00:01Z"}}`,
		`{"type":"message","id":"m2","timestamp":"2026-09-22T10:00:02Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"tool-1","name":"read","input":{"path":"/tmp/file"}}],"timestamp":"2026-09-22T10:00:02Z"}}`,
		`{"type":"message","id":"m3","timestamp":"2026-09-22T10:00:03Z","message":{"role":"toolResult","toolCallId":"tool-1","content":[{"type":"toolResult","text":"contents"}],"timestamp":"2026-09-22T10:00:03Z"}}`,
		`{"type":"message","id":"m4","timestamp":"2026-09-22T10:00:04Z","message":{"role":"assistant","content":[{"type":"text","text":"done"}],"timestamp":"2026-09-22T10:00:04Z"}}`,
	}
}
