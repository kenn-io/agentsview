package parser

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCursorS3ScannerRejectsNonTranscriptLayouts(t *testing.T) {
	scan := cursorS3Scanner()
	tests := []struct {
		name string
		rel  string
		segs []string
		want bool
	}{
		{
			name: "harvest jsonl",
			rel:  "demo-proj/11111111-1111-4111-8111-111111111111.jsonl",
			segs: []string{"demo-proj", "11111111-1111-4111-8111-111111111111.jsonl"},
			want: true,
		},
		{
			name: "harvest txt",
			rel:  "demo-proj/11111111-1111-4111-8111-111111111111.txt",
			segs: []string{"demo-proj", "11111111-1111-4111-8111-111111111111.txt"},
			want: true,
		},
		{
			name: "flat agent-transcripts",
			rel:  "demo-proj/agent-transcripts/sess.jsonl",
			segs: []string{"demo-proj", "agent-transcripts", "sess.jsonl"},
			want: true,
		},
		{
			name: "nested matching stem",
			rel:  "demo-proj/agent-transcripts/sess/sess.jsonl",
			segs: []string{"demo-proj", "agent-transcripts", "sess", "sess.jsonl"},
			want: true,
		},
		{
			name: "notes markdown",
			rel:  "demo-proj/notes.md",
			segs: []string{"demo-proj", "notes.md"},
		},
		{
			name: "bare file",
			rel:  "sess.jsonl",
			segs: []string{"sess.jsonl"},
		},
		{
			name: "nested mismatch",
			rel:  "demo-proj/agent-transcripts/sess/other.jsonl",
			segs: []string{"demo-proj", "agent-transcripts", "sess", "other.jsonl"},
		},
		{
			name: "subagent child",
			rel:  "demo-proj/agent-transcripts/sess/subagents/child.jsonl",
			segs: []string{"demo-proj", "agent-transcripts", "sess", "subagents", "child.jsonl"},
		},
		{
			name: "random nested txt",
			rel:  "demo-proj/logs/trace.txt",
			segs: []string{"demo-proj", "logs", "trace.txt"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, scan.Keep(tt.rel, tt.segs))
		})
	}
}

func TestCursorS3DiscoverPrefersJSONLForSameStem(t *testing.T) {
	oldList := listS3Objects
	t.Cleanup(func() { listS3Objects = oldList })

	root := "s3://bucket/laptop/raw/cursor"
	jsonlURI := root + "/demo-proj/shared.jsonl"
	txtURI := root + "/demo-proj/shared.txt"
	otherURI := root + "/demo-proj/other.txt"
	junkURI := root + "/demo-proj/logs/trace.txt"
	mtime := time.Unix(100, 0)
	listS3Objects = func(got string) ([]S3Object, error) {
		require.Equal(t, root, got)
		return []S3Object{
			{URI: txtURI, Size: 7, LastModified: mtime, Fingerprint: "s3-meta:txt"},
			{URI: junkURI, Size: 3, LastModified: mtime, Fingerprint: "s3-meta:junk"},
			{URI: jsonlURI, Size: 11, LastModified: mtime, Fingerprint: "s3-meta:jsonl"},
			{URI: otherURI, Size: 5, LastModified: mtime, Fingerprint: "s3-meta:other"},
		}, nil
	}

	sources, err := newCursorSourceSet([]string{root}).Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 2)
	assert.ElementsMatch(t, []string{jsonlURI, otherURI}, []string{
		sources[0].DisplayPath,
		sources[1].DisplayPath,
	})

	var streamed []string
	err = newCursorSourceSet([]string{root}).DiscoverEach(
		context.Background(),
		func(src SourceRef) error {
			streamed = append(streamed, src.DisplayPath)
			return nil
		},
	)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{jsonlURI, otherURI}, streamed)
}

func TestCursorS3DiscoverDeduplicatesSameStemAcrossProjectsDeterministically(t *testing.T) {
	oldList := listS3Objects
	t.Cleanup(func() { listS3Objects = oldList })

	root := "s3://bucket/archive/agent-transcripts/laptop/raw/cursor"
	firstURI := root + "/project-one/11111111-1111-4111-8111-111111111111.jsonl"
	secondURI := root + "/project-two/11111111-1111-4111-8111-111111111111.jsonl"
	listS3Objects = func(got string) ([]S3Object, error) {
		require.Equal(t, root, got)
		return []S3Object{
			{URI: secondURI, LastModified: time.Unix(200, 0)},
			{URI: firstURI, LastModified: time.Unix(100, 0)},
		}, nil
	}

	sources, err := newCursorSourceSet([]string{root}).Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, firstURI, sources[0].DisplayPath)
}

func TestCursorS3DiscoverDeduplicatesSameStemAcrossRootsByMachine(t *testing.T) {
	oldList := listS3Objects
	t.Cleanup(func() { listS3Objects = oldList })

	const stem = "11111111-1111-4111-8111-111111111111"
	laptopTxtRoot := "s3://bucket-a/archive/laptop/raw/cursor"
	laptopJSONLRoot := "s3://bucket-b/archive/laptop/raw/cursor"
	desktopRoot := "s3://bucket-c/archive/desktop/raw/cursor"
	laptopTxtURI := laptopTxtRoot + "/project-a/" + stem + ".txt"
	laptopJSONLURI := laptopJSONLRoot + "/project-b/" + stem + ".jsonl"
	desktopURI := desktopRoot + "/project-c/" + stem + ".jsonl"
	objectsByRoot := map[string][]S3Object{
		laptopTxtRoot: {
			{URI: laptopTxtURI, LastModified: time.Unix(100, 0)},
		},
		laptopJSONLRoot: {
			{URI: laptopJSONLURI, LastModified: time.Unix(200, 0)},
		},
		desktopRoot: {
			{URI: desktopURI, LastModified: time.Unix(300, 0)},
		},
	}
	listS3Objects = func(root string) ([]S3Object, error) {
		objects, ok := objectsByRoot[root]
		require.True(t, ok, "unexpected S3 root %q", root)
		return objects, nil
	}

	sourceSet := newCursorSourceSet([]string{
		laptopTxtRoot,
		desktopRoot,
		laptopJSONLRoot,
	})
	sources, err := sourceSet.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 2)
	assert.ElementsMatch(t, []string{laptopJSONLURI, desktopURI}, []string{
		sources[0].DisplayPath,
		sources[1].DisplayPath,
	})

	var streamed []string
	err = sourceSet.DiscoverEach(context.Background(), func(source SourceRef) error {
		streamed = append(streamed, source.DisplayPath)
		return nil
	})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{laptopJSONLURI, desktopURI}, streamed)
}

func TestCursorS3DiscoverDecodesAgentTranscriptsProject(t *testing.T) {
	oldList := listS3Objects
	t.Cleanup(func() { listS3Objects = oldList })

	root := "s3://bucket/laptop/raw/cursor"
	encoded := "Users-fiona-Documents-demo"
	harvestURI := root + "/my-cool-project/11111111-1111-4111-8111-111111111111.jsonl"
	localURI := root + "/" + encoded + "/agent-transcripts/sess.jsonl"
	mtime := time.Unix(100, 0)
	listS3Objects = func(got string) ([]S3Object, error) {
		require.Equal(t, root, got)
		return []S3Object{
			{URI: harvestURI, Size: 11, LastModified: mtime},
			{URI: localURI, Size: 7, LastModified: mtime},
		}, nil
	}

	sources, err := newCursorSourceSet([]string{root}).Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 2)
	byPath := make(map[string]SourceRef, len(sources))
	for _, src := range sources {
		byPath[src.DisplayPath] = src
	}
	require.Contains(t, byPath, harvestURI)
	require.Contains(t, byPath, localURI)
	assert.Equal(t, "my-cool-project", byPath[harvestURI].ProjectHint)
	assert.Equal(t, "demo", byPath[localURI].ProjectHint)
}

func TestCursorS3DiscoverPrefersNestedOverFlat(t *testing.T) {
	oldList := listS3Objects
	t.Cleanup(func() { listS3Objects = oldList })

	root := "s3://bucket/laptop/raw/cursor"
	project := "Users-fiona-Documents-demo"
	flatURI := root + "/" + project + "/agent-transcripts/sess.jsonl"
	nestedURI := root + "/" + project + "/agent-transcripts/sess/sess.jsonl"
	mtime := time.Unix(100, 0)
	listS3Objects = func(got string) ([]S3Object, error) {
		require.Equal(t, root, got)
		return []S3Object{
			{URI: flatURI, Size: 11, LastModified: mtime},
			{URI: nestedURI, Size: 13, LastModified: mtime},
		}, nil
	}

	sources, err := newCursorSourceSet([]string{root}).Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, nestedURI, sources[0].DisplayPath)
	assert.Equal(t, "demo", sources[0].ProjectHint)
}

func TestCursorS3ProviderRejectsNonTranscriptLayout(t *testing.T) {
	p, ok := S3ProviderFor(AgentCursor)
	require.True(t, ok)
	scan := p.S3Scanner()
	assert.True(t, scan.Keep(
		"demo-proj/shared.jsonl",
		[]string{"demo-proj", "shared.jsonl"},
	))
	assert.False(t, scan.Keep(
		"demo-proj/logs/trace.txt",
		[]string{"demo-proj", "logs", "trace.txt"},
	))
}
