package parser

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// memoryFreshnessPager serves rows in ascending path order, honoring afterPath
// and limit, and never more than pageSize rows per call when pageSize > 0.
func memoryFreshnessPager(
	rows []StoredMemberFreshness, pageSize int,
) StoredMemberFreshnessPager {
	sorted := append([]StoredMemberFreshness(nil), rows...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	return func(
		_ context.Context, afterPath string, limit int,
	) ([]StoredMemberFreshness, bool, error) {
		if pageSize > 0 && pageSize < limit {
			limit = pageSize
		}
		start := sort.Search(len(sorted), func(i int) bool {
			return sorted[i].Path > afterPath
		})
		end := min(start+limit, len(sorted))
		return append([]StoredMemberFreshness(nil), sorted[start:end]...),
			end == len(sorted), nil
	}
}

type memberTokenFixture struct {
	root    string
	dbPath  string
	members []multiSessionMemberToken
}

func newMemberTokenFixture(t *testing.T) memberTokenFixture {
	t.Helper()
	root := t.TempDir()
	dbPath := filepath.Join(root, "store.db")
	require.NoError(t, os.WriteFile(dbPath, nil, 0o644))
	var members []multiSessionMemberToken
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		token := "t-" + id
		if id == "d" {
			token = ""
		}
		members = append(members, multiSessionMemberToken{
			Path: VirtualSourcePath(dbPath, id), MemberID: id, Token: token,
		})
	}
	return memberTokenFixture{root: root, dbPath: dbPath, members: members}
}

func (f memberTokenFixture) path(id string) string {
	return VirtualSourcePath(f.dbPath, id)
}

func (f memberTokenFixture) sourceSet(
	listErr error, onList func(), withTokens bool, absent map[string]bool,
) multiSessionContainerSourceSet {
	classify := func(root, path string, allowMissing bool) (multiSessionMatch, bool) {
		return classifySQLiteContainerPath(root, path, "store.db", allowMissing, true,
			func(p string) (string, string, bool) {
				return ParseVirtualSourcePathForBase(p, "store.db")
			})
	}
	opts := []MultiSessionOption{
		WithContainerDiscovery(func(string) []string { return []string{f.dbPath} }),
		WithWatchRoots(func([]string) []WatchRoot { return nil }),
		WithChangedPathClassifier(classify),
		WithMemberLookup(func(context.Context, string, string) (multiSessionMatch, bool) {
			return multiSessionMatch{}, false
		}),
		WithFingerprint(func(multiSessionSource) (SourceFingerprint, error) {
			return SourceFingerprint{}, nil
		}),
		WithContainerParse(func(multiSessionSource, ParseRequest) ([]ParseResult, error) {
			return nil, nil
		}),
		WithMemberParse(func(multiSessionSource, ParseRequest) (*ParseResult, error) {
			return nil, nil
		}),
		WithBatchMemberPresence(func(
			_ context.Context, _ multiSessionSource, members []multiSessionSource,
		) map[string]bool {
			present := make(map[string]bool, len(members))
			for _, member := range members {
				present[member.Path] = !absent[member.MemberID]
			}
			return present
		}),
	}
	if withTokens {
		opts = append(opts, WithMemberChangeTokens(
			func(
				_ context.Context, _ multiSessionSource,
				yield func(multiSessionMemberToken) error,
			) error {
				if onList != nil {
					onList()
				}
				if listErr != nil {
					return listErr
				}
				for _, member := range f.members {
					if err := yield(member); err != nil {
						return err
					}
				}
				return nil
			},
			func(hash string) (string, bool) {
				token, ok := strings.CutPrefix(hash, "ok:")
				return token, ok && token != ""
			},
		))
	}
	return NewMultiSessionContainerSourceSet(AgentCursorIDE, []string{f.root}, opts...)
}

func TestMultiSessionChangedPathMemberTokens(t *testing.T) {
	storedRows := func(f memberTokenFixture) []StoredMemberFreshness {
		return []StoredMemberFreshness{
			{Path: f.path("a"), FingerprintHash: "ok:t-a"},
			{Path: f.path("b"), FingerprintHash: "ok:old"},
			{Path: f.path("d"), FingerprintHash: "ok:"},
			{Path: f.path("e"), FingerprintHash: "bare"},
		}
	}
	errPager := func(context.Context, string, int) ([]StoredMemberFreshness, bool, error) {
		return nil, false, errors.New("pager failed")
	}

	tests := []struct {
		name        string
		withoutOpt  bool
		listErr     error
		nilPager    bool
		emptyStored bool
		pagerErr    bool
		eventKind   string
		memberPath  string
		removeDB    bool
		absent      map[string]bool
		storedPaths []string
		stored      func(f memberTokenFixture) []StoredMemberFreshness
		pageSize    int
		want        func(f memberTokenFixture) []string
	}{
		{
			name: "only changed members",
			want: func(f memberTokenFixture) []string {
				return []string{f.path("b"), f.path("c"), f.path("d"), f.path("e")}
			},
		},
		{
			name: "suppressed members omitted whatever their token",
			stored: func(f memberTokenFixture) []StoredMemberFreshness {
				return []StoredMemberFreshness{
					{Path: f.path("a"), FingerprintHash: "ok:t-a"},
					{Path: f.path("b"), FingerprintHash: "ok:old", Suppressed: true},
					{Path: f.path("c"), Suppressed: true},
				}
			},
			want: func(f memberTokenFixture) []string {
				return []string{f.path("d"), f.path("e")}
			},
		},
		{
			name: "only suppressed rows across pages keeps container",
			stored: func(f memberTokenFixture) []StoredMemberFreshness {
				var rows []StoredMemberFreshness
				for _, id := range []string{"a", "b", "c"} {
					rows = append(rows, StoredMemberFreshness{Path: f.path(id), Suppressed: true})
				}
				return rows
			},
			pageSize: 2,
			want:     func(f memberTokenFixture) []string { return []string{f.dbPath} },
		},
		{
			name: "unsuppressed row past a suppressed page merges from the start",
			stored: func(f memberTokenFixture) []StoredMemberFreshness {
				return []StoredMemberFreshness{
					{Path: f.path("a"), Suppressed: true},
					{Path: f.path("b"), FingerprintHash: "ok:t-b"},
				}
			},
			pageSize: 1,
			want: func(f memberTokenFixture) []string {
				return []string{f.path("c"), f.path("d"), f.path("e")}
			},
		},
		{
			name:     "nil pager keeps container",
			nilPager: true,
			want:     func(f memberTokenFixture) []string { return []string{f.dbPath} },
		},
		{
			name:      "remove event keeps container",
			eventKind: "remove",
			want:      func(f memberTokenFixture) []string { return []string{f.dbPath} },
		},
		{
			name:       "member path unchanged",
			memberPath: "a",
			want:       func(f memberTokenFixture) []string { return []string{f.path("a")} },
		},
		{
			name:     "missing container keeps container",
			removeDB: true,
			want:     func(f memberTokenFixture) []string { return []string{f.dbPath} },
		},
		{
			name:        "empty stored side keeps container",
			emptyStored: true,
			want:        func(f memberTokenFixture) []string { return []string{f.dbPath} },
		},
		{
			name:     "pager error keeps container",
			pagerErr: true,
			want:     func(f memberTokenFixture) []string { return []string{f.dbPath} },
		},
		{
			name:    "listing error keeps container",
			listErr: errors.New("listing failed"),
			want:    func(f memberTokenFixture) []string { return []string{f.dbPath} },
		},
		{
			name:       "without option pager ignored",
			withoutOpt: true,
			want:       func(f memberTokenFixture) []string { return []string{f.dbPath} },
		},
		{
			name:        "tombstones appended",
			absent:      map[string]bool{"f": true},
			storedPaths: []string{"f"},
			want: func(f memberTokenFixture) []string {
				return []string{f.path("b"), f.path("c"), f.path("d"), f.path("e"), f.path("f")}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newMemberTokenFixture(t)
			set := f.sourceSet(tt.listErr, nil, !tt.withoutOpt, tt.absent)
			req := ChangedPathRequest{
				Path:                      f.dbPath,
				EventKind:                 "write",
				AllowWatermarkOnlySources: true,
			}
			if tt.eventKind != "" {
				req.EventKind = tt.eventKind
			}
			if tt.memberPath != "" {
				req.Path = f.path(tt.memberPath)
			}
			for _, id := range tt.storedPaths {
				req.StoredSourcePaths = append(req.StoredSourcePaths, f.path(id))
			}
			switch {
			case tt.nilPager:
			case tt.pagerErr:
				req.StoredMemberFreshnessPage = errPager
			case tt.emptyStored:
				req.StoredMemberFreshnessPage = memoryFreshnessPager(nil, 0)
			case tt.stored != nil:
				req.StoredMemberFreshnessPage = memoryFreshnessPager(tt.stored(f), tt.pageSize)
			default:
				req.StoredMemberFreshnessPage = memoryFreshnessPager(storedRows(f), 0)
			}
			if tt.removeDB {
				require.NoError(t, os.Remove(f.dbPath))
			}
			sources, err := set.SourcesForChangedPath(t.Context(), req)
			require.NoError(t, err)
			assert.Equal(t, tt.want(f), sourceKeys(sources))
		})
	}

	t.Run("canceled context", func(t *testing.T) {
		f := newMemberTokenFixture(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		set := f.sourceSet(context.Canceled, cancel, true, nil)
		_, err := set.SourcesForChangedPath(ctx, ChangedPathRequest{
			Path:                      f.dbPath,
			EventKind:                 "write",
			AllowWatermarkOnlySources: true,
			StoredMemberFreshnessPage: memoryFreshnessPager(storedRows(f), 0),
		})
		require.ErrorIs(t, err, context.Canceled)
	})

	t.Run("stored freshness container", func(t *testing.T) {
		f := newMemberTokenFixture(t)
		set := f.sourceSet(nil, nil, true, nil)
		writeSourceFile(t, f.dbPath+"-wal", walWithFramesFixture)
		for _, path := range []string{f.dbPath, f.dbPath + "-wal"} {
			container, ok := set.StoredMemberFreshnessContainer(path)
			assert.True(t, ok, path)
			assert.Equal(t, f.dbPath, container, path)
		}
		for _, path := range []string{f.dbPath + "-shm", f.path("a")} {
			_, ok := set.StoredMemberFreshnessContainer(path)
			assert.False(t, ok, path)
		}
		_, ok := f.sourceSet(nil, nil, false, nil).StoredMemberFreshnessContainer(f.dbPath)
		assert.False(t, ok)
	})
}

func TestStoredMemberFreshnessCursorLookup(t *testing.T) {
	rows := []StoredMemberFreshness{
		{Path: "c#1", CoveredThroughNS: 100},
		{Path: "c#2", CoveredThroughNS: 100},
		{Path: "c#4", CoveredThroughNS: 100},
		{Path: "c#5", CoveredThroughNS: 100, Suppressed: true},
	}
	cursor := storedMemberFreshnessCursor{pager: memoryFreshnessPager(rows, 2)}
	ctx := t.Context()

	row, ok, err := cursor.lookup(ctx, "c#1")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "c#1", row.Path)

	_, ok, err = cursor.lookup(ctx, "c#3")
	require.NoError(t, err)
	assert.False(t, ok, "path between two stored pages is absent")

	row, ok, err = cursor.lookup(ctx, "c#5")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "c#5", row.Path)

	_, ok, err = cursor.lookup(ctx, "c#6")
	require.NoError(t, err)
	assert.False(t, ok)

	failing := storedMemberFreshnessCursor{
		pager: func(context.Context, string, int) ([]StoredMemberFreshness, bool, error) {
			return nil, false, errors.New("pager failed")
		},
	}
	_, _, err = failing.lookup(ctx, "c#1")
	require.Error(t, err)

	for _, tc := range []struct {
		path      string
		watermark int64
		want      bool
	}{
		{"c#1", 99, true},
		{"c#2", 100, true},
		{"c#4", 101, false},
		{"c#5", 101, true},
		{"c#6", 0, false},
	} {
		c := storedMemberFreshnessCursor{pager: memoryFreshnessPager(rows, 2)}
		got, err := c.covers(ctx, tc.path, tc.watermark)
		require.NoError(t, err)
		assert.Equal(t, tc.want, got, tc.path)
	}
}
