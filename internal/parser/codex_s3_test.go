package parser

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscoverCodexS3RequiresFullRootPrefix(t *testing.T) {
	oldList := listS3Objects
	t.Cleanup(func() { listS3Objects = oldList })

	mtime := time.Unix(100, 0)
	listS3Objects = func(root string) ([]S3Object, error) {
		require.Equal(t, "s3://bucket/root/codex", root)
		return []S3Object{
			{
				URI:          "s3://bucket/root/codex/2026/06/24/rollout-2026-06-24T00-00-00-good.jsonl",
				Size:         11,
				LastModified: mtime,
			},
			{
				URI:          "s3://bucket/root/codex-backup/rollout-2026-06-24T00-00-00-backup.jsonl",
				Size:         22,
				LastModified: mtime.Add(time.Second),
			},
			{
				URI:          "s3://bucket/root/codex2/rollout-2026-06-24T00-00-00-two.jsonl",
				Size:         33,
				LastModified: mtime.Add(2 * time.Second),
			},
		}, nil
	}

	got := s3PrefixScan("s3://bucket/root/codex", codexS3Scanner())
	require.Len(t, got, 1)
	assert.Equal(t, "s3://bucket/root/codex/2026/06/24/rollout-2026-06-24T00-00-00-good.jsonl", got[0].Path)
	assert.Equal(t, int64(11), got[0].SourceSize)
	assert.Equal(t, mtime.UnixNano(), got[0].SourceMtime)
}

func TestDiscoverCodexS3KeepsSessionIndexMetadataSeparate(t *testing.T) {
	oldList := listS3Objects
	oldStat := statS3Object
	t.Cleanup(func() {
		listS3Objects = oldList
		statS3Object = oldStat
	})

	root := "s3://bucket/laptop/raw/codex"
	rolloutURI := root + "/2026/06/24/rollout-2026-06-24T00-00-00-" +
		"11111111-1111-4111-8111-111111111111.jsonl"
	rolloutMtime := time.Unix(100, 0)
	listS3Objects = func(got string) ([]S3Object, error) {
		require.Equal(t, root, got)
		return []S3Object{{
			URI:          rolloutURI,
			Size:         11,
			LastModified: rolloutMtime,
			Fingerprint:  "s3-meta:rollout",
		}}, nil
	}
	statS3Object = func(got string) (S3Object, error) {
		require.Failf(t, "unexpected index stat", "stat %s", got)
		return S3Object{}, nil
	}

	got := s3PrefixScan(root, codexS3Scanner())

	require.Len(t, got, 1)
	assert.Equal(t, rolloutURI, got[0].Path)
	assert.Equal(t, int64(11), got[0].SourceSize)
	assert.Equal(t, rolloutMtime.UnixNano(), got[0].SourceMtime)
	assert.Contains(t, got[0].SourceFingerprint, "rollout")
	assert.NotContains(t, got[0].SourceFingerprint, "index")
}

func TestCodexS3SessionIndexURIPrefersRawCodexLayout(t *testing.T) {
	got, ok := CodexS3SessionIndexURI(
		"s3://bucket/backups/sessions/laptop/raw/codex/2026/06/24/" +
			"rollout-2026-06-24T00-00-00-11111111-1111-4111-8111-111111111111.jsonl",
	)

	require.True(t, ok)
	assert.Equal(
		t,
		"s3://bucket/backups/sessions/laptop/raw/session_index.jsonl",
		got,
	)
}

func TestFindCodexS3ParentSessionURI(t *testing.T) {
	const parentID = "11111111-1111-4111-8111-111111111111"
	const root = "s3://bucket/laptop/raw/codex"
	parentDated := root + "/2026/08/12/rollout-2026-08-12T00-00-00-" +
		parentID + ".jsonl"
	parentSessions := root + "/sessions/2026/08/12/" +
		"rollout-2026-08-12T00-00-00-" + parentID + ".jsonl"
	parentArchived := root + "/archived_sessions/" +
		"rollout-2026-08-12T00-00-00-" + parentID + ".jsonl"
	customRoot := "s3://bucket/root/codex"
	customParent := customRoot + "/team/b/" +
		"rollout-2026-08-12T00-00-00-" + parentID + ".jsonl"

	tests := []struct {
		name     string
		childURI string
		parentID string
		objects  []S3Object
		want     string
		wantRoot string
		wantList bool
	}{
		{
			name: "dated parent",
			childURI: root + "/2026/08/13/rollout-2026-08-13T00-00-00-" +
				"22222222-2222-4222-8222-222222222222.jsonl",
			parentID: parentID,
			objects:  []S3Object{{URI: parentDated}},
			want:     parentDated,
			wantList: true,
		},
		{
			name: "sessions parent",
			childURI: root + "/sessions/2026/08/13/" +
				"rollout-2026-08-13T00-00-00-22222222-2222-4222-8222-222222222222.jsonl",
			parentID: parentID,
			objects:  []S3Object{{URI: parentSessions}},
			want:     parentSessions,
			wantList: true,
		},
		{
			name: "archived parent",
			childURI: root + "/archived_sessions/" +
				"rollout-2026-08-13T00-00-00-22222222-2222-4222-8222-222222222222.jsonl",
			parentID: parentID,
			objects:  []S3Object{{URI: parentArchived}},
			want:     parentArchived,
			wantList: true,
		},
		{
			name: "live layout wins over archived duplicate",
			childURI: root + "/sessions/2026/08/13/" +
				"rollout-2026-08-13T00-00-00-22222222-2222-4222-8222-222222222222.jsonl",
			parentID: parentID,
			objects: []S3Object{
				{URI: parentArchived},
				{URI: parentSessions},
			},
			want:     parentSessions,
			wantList: true,
		},
		{
			name: "custom configured root at arbitrary depth",
			childURI: customRoot + "/team/a/" +
				"rollout-2026-08-13T00-00-00-22222222-2222-4222-8222-222222222222.jsonl",
			parentID: parentID,
			objects:  []S3Object{{URI: customParent}},
			want:     customParent,
			wantRoot: customRoot,
			wantList: true,
		},
		{
			name:     "child outside configured root",
			childURI: "s3://bucket/other/rollout-2026-08-13T00-00-00-22222222-2222-4222-8222-222222222222.jsonl",
			parentID: parentID,
			wantRoot: customRoot,
		},
		{
			name: "exact id only",
			childURI: root + "/2026/08/13/rollout-2026-08-13T00-00-00-" +
				"22222222-2222-4222-8222-222222222222.jsonl",
			parentID: parentID,
			objects: []S3Object{{URI: root + "/2026/08/12/" +
				"rollout-2026-08-12T00-00-00-11111111-1111-4111-8111-111111111110.jsonl"}},
			wantList: true,
		},
		{
			name: "empty parent id",
			childURI: root + "/2026/08/13/rollout-2026-08-13T00-00-00-" +
				"22222222-2222-4222-8222-222222222222.jsonl",
		},
		{
			name: "path-like parent id",
			childURI: root + "/2026/08/13/rollout-2026-08-13T00-00-00-" +
				"22222222-2222-4222-8222-222222222222.jsonl",
			parentID: "../" + parentID,
		},
		{
			name: "short parent id suffix",
			childURI: root + "/2026/08/13/rollout-2026-08-13T00-00-00-" +
				"22222222-2222-4222-8222-222222222222.jsonl",
			parentID: "111111111111",
			objects:  []S3Object{{URI: parentDated}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldList := listS3Objects
			t.Cleanup(func() { listS3Objects = oldList })
			listed := false
			listS3Objects = func(got string) ([]S3Object, error) {
				listed = true
				wantRoot := tt.wantRoot
				if wantRoot == "" {
					wantRoot = root
				}
				require.Equal(t, wantRoot, got)
				return tt.objects, nil
			}

			configuredRoot := tt.wantRoot
			if configuredRoot == "" {
				configuredRoot = root
			}
			got, ok := FindCodexS3ParentSessionURI(
				configuredRoot, tt.childURI, tt.parentID,
			)

			assert.Equal(t, tt.want != "", ok)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.wantList, listed)
		})
	}
}
