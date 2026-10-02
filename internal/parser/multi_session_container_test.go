package parser

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const fakeContainerHash = "container-hash"

func fakeContainerResults(container string) []ParseResult {
	var results []ParseResult
	for _, id := range []string{"alpha", "beta", "gamma"} {
		results = append(results, ParseResult{Session: ParsedSession{
			ID:    "zed:" + id,
			Agent: AgentZed,
			File:  FileInfo{Path: VirtualSourcePath(container, id)},
		}})
	}
	return results
}

// newFakeContainerProvider wraps a minimal container source set in the
// production factory so ParseEach sees the same concrete types the engine does.
func newFakeContainerProvider(t *testing.T, container string, opts ...MultiSessionOption) Provider {
	t.Helper()
	base := []MultiSessionOption{
		WithContainerDiscovery(func(string) []string { return []string{container} }),
		WithWatchRoots(func([]string) []WatchRoot { return nil }),
		WithChangedPathClassifier(func(string, string, bool) (multiSessionMatch, bool) {
			return multiSessionMatch{}, false
		}),
		WithMemberLookup(func(context.Context, string, string) (multiSessionMatch, bool) {
			return multiSessionMatch{}, false
		}),
		WithFingerprint(func(multiSessionSource) (SourceFingerprint, error) {
			return SourceFingerprint{}, nil
		}),
		WithMemberParse(func(src multiSessionSource, _ ParseRequest) (*ParseResult, error) {
			return &ParseResult{Session: ParsedSession{
				ID: "zed:" + src.MemberID, Agent: AgentZed,
				File: FileInfo{Path: src.Path},
			}}, nil
		}),
		WithContainerHashStamping(),
	}
	factory := NewMultiSessionProviderFactory(
		AgentDef{Type: AgentZed, IDPrefix: "zed:"},
		Capabilities{},
		func(cfg ProviderConfig) multiSessionContainerSourceSet {
			return NewMultiSessionContainerSourceSet(AgentZed, cfg.Roots, append(base, opts...)...)
		},
	)
	return factory.NewProvider(ProviderConfig{Roots: []string{filepath.Dir(container)}})
}

func fakeContainerRequest(container, memberID string) ParseRequest {
	src := multiSessionSource{
		Root: filepath.Dir(container), Path: container,
		Container: container, MemberID: memberID,
	}
	if memberID != "" {
		src.Path = VirtualSourcePath(container, memberID)
	}
	return ParseRequest{
		Source:      SourceRef{Provider: AgentZed, Key: src.Path, Opaque: src},
		Fingerprint: SourceFingerprint{Hash: fakeContainerHash},
	}
}

func fakeContainerPath(t *testing.T) string {
	t.Helper()
	container := filepath.Join(t.TempDir(), "threads.db")
	require.NoError(t, os.WriteFile(container, nil, 0o600))
	return container
}

func collectParseEach(
	t *testing.T, provider Provider, req ParseRequest,
) (ParseOutcome, []ParseResultOutcome, error) {
	t.Helper()
	var got []ParseResultOutcome
	outcome, err := ParseEach(t.Context(), provider, req, func(r ParseResultOutcome) error {
		got = append(got, r)
		return nil
	})
	return outcome, got, err
}

func TestMultiSessionContainerParseEachMatchesParse(t *testing.T) {
	tests := []struct {
		name     string
		memberID string
		opt      func(container string) MultiSessionOption
	}{
		{name: "container parse", opt: func(container string) MultiSessionOption {
			return WithContainerParse(func(multiSessionSource, ParseRequest) ([]ParseResult, error) {
				return fakeContainerResults(container), nil
			})
		}},
		{name: "context container parse", opt: func(container string) MultiSessionOption {
			return WithContextContainerParse(func(context.Context, multiSessionSource, ParseRequest) ([]ParseResult, error) {
				return fakeContainerResults(container), nil
			})
		}},
		{name: "container parse each", opt: func(container string) MultiSessionOption {
			return WithContextContainerParseEach(func(
				_ context.Context, _ multiSessionSource, _ ParseRequest, yield func(ParseResult) error,
			) error {
				for _, r := range fakeContainerResults(container) {
					if err := yield(r); err != nil {
						return err
					}
				}
				return nil
			})
		}},
		{name: "container parse outcome", opt: func(container string) MultiSessionOption {
			return WithContainerParseOutcome(func(context.Context, multiSessionSource, ParseRequest) (ParseOutcome, error) {
				outcome := ParseOutcome{ResultSetComplete: true, ForceReplace: true}
				for _, r := range fakeContainerResults(container) {
					outcome.Results = append(outcome.Results, ParseResultOutcome{
						Result: r, DataVersion: DataVersionCurrent,
					})
				}
				return outcome, nil
			})
		}},
		{name: "member parse", memberID: "alpha", opt: func(container string) MultiSessionOption {
			return WithContainerParse(func(multiSessionSource, ParseRequest) ([]ParseResult, error) {
				return fakeContainerResults(container), nil
			})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			container := fakeContainerPath(t)
			provider := newFakeContainerProvider(t, container, tt.opt(container))
			req := fakeContainerRequest(container, tt.memberID)

			collected, err := provider.Parse(t.Context(), req)
			require.NoError(t, err)
			require.NotEmpty(t, collected.Results)

			outcome, got, err := collectParseEach(t, provider, req)
			require.NoError(t, err)
			assert.Equal(t, collected.Results, got)
			for _, r := range got {
				assert.Equal(t, fakeContainerHash, r.Result.Session.File.Hash, r.Result.Session.ID)
			}
			assert.Nil(t, outcome.Results)
			collected.Results = nil
			assert.Equal(t, collected, outcome)
		})
	}
}

func TestMultiSessionContainerParseEachCompleteness(t *testing.T) {
	emptyEach := WithContextContainerParseEach(func(
		context.Context, multiSessionSource, ParseRequest, func(ParseResult) error,
	) error {
		return nil
	})

	t.Run("zero yields with the container present", func(t *testing.T) {
		container := fakeContainerPath(t)
		provider := newFakeContainerProvider(t, container, emptyEach)
		outcome, got, err := collectParseEach(t, provider, fakeContainerRequest(container, ""))
		require.NoError(t, err)
		assert.Empty(t, got)
		assert.True(t, outcome.ResultSetComplete)
		assert.True(t, outcome.ForceReplace)
		assert.Equal(t, SkipNoSession, outcome.SkipReason)
	})

	t.Run("zero yields with the container removed", func(t *testing.T) {
		container := fakeContainerPath(t)
		provider := newFakeContainerProvider(t, container, emptyEach)
		require.NoError(t, os.Remove(container))
		outcome, got, err := collectParseEach(t, provider, fakeContainerRequest(container, ""))
		require.NoError(t, err)
		assert.Empty(t, got)
		assert.True(t, outcome.ResultSetComplete)
		assert.False(t, outcome.ForceReplace)
		assert.Equal(t, SkipNoSession, outcome.SkipReason)
	})

	t.Run("outcome option flags pass through", func(t *testing.T) {
		container := fakeContainerPath(t)
		want := ParseOutcome{
			ExcludedSessionIDs: []string{"zed:excluded"},
			SkipReason:         SkipUnsupportedSource,
		}
		provider := newFakeContainerProvider(t, container, WithContainerParseOutcome(
			func(context.Context, multiSessionSource, ParseRequest) (ParseOutcome, error) {
				outcome := want
				outcome.Results = []ParseResultOutcome{{
					Result: fakeContainerResults(container)[0], DataVersion: DataVersionCurrent,
				}}
				return outcome, nil
			},
		))
		outcome, got, err := collectParseEach(t, provider, fakeContainerRequest(container, ""))
		require.NoError(t, err)
		assert.Len(t, got, 1)
		assert.Equal(t, want, outcome)
	})

	t.Run("yield error returns unchanged", func(t *testing.T) {
		container := fakeContainerPath(t)
		provider := newFakeContainerProvider(t, container, WithContextContainerParseEach(func(
			_ context.Context, _ multiSessionSource, _ ParseRequest, yield func(ParseResult) error,
		) error {
			for _, r := range fakeContainerResults(container) {
				if err := yield(r); err != nil {
					return err
				}
			}
			return nil
		}))
		sentinel := errors.New("stop")
		calls := 0
		outcome, err := ParseEach(t.Context(), provider, fakeContainerRequest(container, ""),
			func(ParseResultOutcome) error {
				calls++
				return sentinel
			})
		require.ErrorIs(t, err, sentinel)
		assert.Same(t, sentinel, err)
		assert.Equal(t, 1, calls)
		assert.Equal(t, ParseOutcome{}, outcome)
	})
}

func TestParseEachKeepsOmnigentParseOverride(t *testing.T) {
	path := writeOmnigentSingleWorkspaceCardinalityDB(t, 5)
	provider, ok := NewProvider(AgentOmnigent, ProviderConfig{
		Roots: []string{filepath.Dir(path)}, Machine: "host",
	})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	req := ParseRequest{Source: sources[0]}

	collected, err := provider.Parse(t.Context(), req)
	require.NoError(t, err)
	var want []string
	for _, r := range collected.Results {
		want = append(want, r.Result.Session.ID)
	}
	require.NotEmpty(t, want)

	_, got, err := collectParseEach(t, provider, req)
	require.NoError(t, err)
	var gotIDs []string
	for _, r := range got {
		gotIDs = append(gotIDs, r.Result.Session.ID)
	}
	assert.Equal(t, want, gotIDs)
}

// TestSQLiteContainerPathForEventIgnoresFramelessWAL pins the shared rule
// every SQLite container provider routes events through: while the database
// exists, a "-wal" event for a missing or header-only WAL (what a read
// connection creates on open and deletes on close) resolves to nothing, and
// a WAL holding frames, the database itself, or a deleted database's WAL
// still resolves to the container.
func TestSQLiteContainerPathForEventIgnoresFramelessWAL(t *testing.T) {
	tests := []struct {
		name   string
		noDB   bool
		wal    []byte
		event  string
		wantOK bool
	}{
		{name: "missing WAL", event: "-wal"},
		{name: "empty WAL", wal: []byte{}, event: "-wal"},
		{name: "header-only WAL", wal: make([]byte, 32), event: "-wal"},
		{name: "WAL with frames", wal: []byte(walWithFramesFixture), event: "-wal", wantOK: true},
		{name: "database file", event: "", wantOK: true},
		{name: "journal", event: "-journal", wantOK: true},
		{name: "deleted database WAL", noDB: true, event: "-wal", wantOK: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			dbPath := filepath.Join(root, "threads", "threads.db")
			require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), 0o755))
			if !tt.noDB {
				require.NoError(t, os.WriteFile(dbPath, []byte("db"), 0o644))
			}
			if tt.wal != nil {
				require.NoError(t, os.WriteFile(dbPath+"-wal", tt.wal, 0o644))
			}
			got, ok := sqliteContainerPathForEvent(
				root, dbPath+tt.event, "threads/threads.db", true,
			)
			assert.Equal(t, tt.wantOK, ok)
			if tt.wantOK {
				assert.Equal(t, dbPath, got)
			}
		})
	}
}
