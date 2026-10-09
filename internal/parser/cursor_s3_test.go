package parser

import (
	"strings"
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
			want: true,
		},
		{
			name: "subagent legacy text",
			rel:  "demo-proj/agent-transcripts/sess/subagents/child.txt",
			segs: []string{"demo-proj", "agent-transcripts", "sess", "subagents", "child.txt"},
			want: true,
		},
		{
			name: "subagents without parent",
			rel:  "demo-proj/agent-transcripts/subagents/child.jsonl",
			segs: []string{"demo-proj", "agent-transcripts", "subagents", "child.jsonl"},
		},
		{
			name: "subagent named after parent",
			rel:  "demo-proj/agent-transcripts/sess/subagents/sess.jsonl",
			segs: []string{"demo-proj", "agent-transcripts", "sess", "subagents", "sess.jsonl"},
		},
		{
			name: "subagent invalid stem",
			rel:  "demo-proj/agent-transcripts/sess/subagents/ch ild.jsonl",
			segs: []string{"demo-proj", "agent-transcripts", "sess", "subagents", "ch ild.jsonl"},
		},
		{
			name: "grandchild",
			rel:  "demo-proj/agent-transcripts/sess/subagents/child/subagents/grand.jsonl",
			segs: []string{"demo-proj", "agent-transcripts", "sess", "subagents", "child", "subagents", "grand.jsonl"},
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
	for _, tt := range []struct {
		name, loser, winner string
	}{
		{"JSONL over text", "sess.txt", "sess.jsonl"},
		{"own text over subagent JSONL", "agent-transcripts/aaa/subagents/sess.jsonl", "agent-transcripts/sess.txt"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			oldList := listS3Objects
			t.Cleanup(func() { listS3Objects = oldList })
			const root = "s3://bucket/laptop/raw/cursor"
			projectRoot := root + "/Users-fiona-Documents-demo/"
			winner, other := projectRoot+tt.winner, projectRoot+"other.txt"
			listS3Objects = func(got string) ([]S3Object, error) {
				require.Equal(t, root, got)
				return []S3Object{
					{URI: projectRoot + tt.loser},
					{URI: winner},
					{URI: projectRoot + "logs/trace.txt"},
					{URI: other},
				}, nil
			}
			sources, err := newCursorSourceSet([]string{root}).Discover(t.Context())
			require.NoError(t, err)
			require.Len(t, sources, 2)
			assert.ElementsMatch(t, []string{winner, other}, []string{sources[0].DisplayPath, sources[1].DisplayPath})
		})
	}
}

func TestCursorS3DiscoverPreservesSameStemAcrossProjects(t *testing.T) {
	for _, tt := range []struct {
		roots    []string
		projects [2]string
		harvest  bool
	}{
		{roots: []string{"s3://bucket/archive"}},
		{roots: []string{"s3://bucket/laptop/raw/cursor", "s3://bucket/laptop/raw/cursor/project-one/agent-transcripts"}},
		{roots: []string{"s3://bucket/laptop/raw/cursor/project-one/agent-transcripts", "s3://bucket/laptop/raw/cursor"}},
		{roots: []string{"s3://bucket/host-a/raw/cursor"}, projects: [2]string{"agent-transcripts", "cursor"}, harvest: true},
		{roots: []string{"s3://bucket/archive/agent-transcripts/laptop/raw/cursor"}, projects: [2]string{"Users-fiona-Documents-demo", "home-user-a-Documents-demo"}},
	} {
		roots := tt.roots
		projects := tt.projects
		if projects[0] == "" {
			projects = [2]string{"project-one", "project-two"}
		}
		root := roots[0]
		if strings.HasSuffix(root, "/agent-transcripts") {
			root = roots[1]
		}
		t.Run(root, func(t *testing.T) {
			oldList := listS3Objects
			t.Cleanup(func() { listS3Objects = oldList })
			firstURI := root + "/" + projects[0] + "/agent-transcripts/11111111-1111-4111-8111-111111111111/11111111-1111-4111-8111-111111111111.jsonl"
			secondURI := root + "/" + projects[1] + "/agent-transcripts/11111111-1111-4111-8111-111111111111/11111111-1111-4111-8111-111111111111.jsonl"
			otherURI := root + "/" + projects[0] + "/other.txt"
			if tt.harvest {
				firstURI = root + "/" + projects[0] + "/11111111-1111-4111-8111-111111111111.txt"
				secondURI = root + "/" + projects[1] + "/11111111-1111-4111-8111-111111111111.txt"
			}
			listS3Objects = func(got string) ([]S3Object, error) {
				require.Contains(t, roots, got)
				return []S3Object{
					{URI: secondURI, LastModified: time.Unix(200, 0)},
					{URI: firstURI, LastModified: time.Unix(100, 0)},
					{URI: otherURI, LastModified: time.Unix(100, 0)},
				}, nil
			}
			sourceSet := newCursorSourceSet(roots)
			sources, err := sourceSet.Discover(t.Context())
			require.NoError(t, err)
			require.Len(t, sources, 3)
			for _, source := range sources {
				if source.DisplayPath == firstURI {
					assert.Equal(t, roots[0], source.ConfiguredRoot)
				} else {
					assert.Equal(t, root, source.ConfiguredRoot)
				}
			}
			assert.ElementsMatch(t, []string{firstURI, secondURI, otherURI}, []string{sources[0].DisplayPath, sources[1].DisplayPath, sources[2].DisplayPath})
		})
	}
}

func TestCursorS3DiscoverDeduplicatesSameStemAcrossRootsByMachine(t *testing.T) {
	oldList := listS3Objects
	t.Cleanup(func() { listS3Objects = oldList })

	const stem = "11111111-1111-4111-8111-111111111111"
	laptopTxtRoot := "s3://bucket-a/archive/laptop/raw/cursor"
	laptopJSONLRoot := "s3://bucket-b/archive/laptop/raw/cursor"
	desktopRoot := "s3://bucket-c/archive/desktop/raw/cursor"
	laptopTxtURI := laptopTxtRoot + "/project-a/" + stem + ".txt"
	laptopJSONLURI := laptopJSONLRoot + "/project-a/" + stem + ".jsonl"
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
	sources, err := sourceSet.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 2)
	assert.ElementsMatch(t, []string{laptopJSONLURI, desktopURI}, []string{
		sources[0].DisplayPath,
		sources[1].DisplayPath,
	})
}

func TestCursorS3DiscoverDecodesAgentTranscriptsProject(t *testing.T) {
	oldList := listS3Objects
	t.Cleanup(func() { listS3Objects = oldList })

	root := "s3://bucket/laptop/raw/cursor"
	encoded := "Users-fiona-Documents-demo"
	harvestURI := root + "/my-cool-project/11111111-1111-4111-8111-111111111111.jsonl"
	localURI := root + "/" + encoded + "/agent-transcripts/sess.jsonl"
	subagentURI := root + "/" + encoded + "/agent-transcripts/sess/subagents/child.jsonl"
	mtime := time.Unix(100, 0)
	listS3Objects = func(got string) ([]S3Object, error) {
		require.Equal(t, root, got)
		return []S3Object{
			{URI: harvestURI, Size: 11, LastModified: mtime},
			{URI: localURI, Size: 7, LastModified: mtime},
			{URI: subagentURI, Size: 5, LastModified: mtime},
		}, nil
	}

	sources, err := newCursorSourceSet([]string{root}).Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 3)
	byPath := make(map[string]SourceRef, len(sources))
	for _, src := range sources {
		byPath[src.DisplayPath] = src
	}
	require.Contains(t, byPath, harvestURI)
	require.Contains(t, byPath, localURI)
	require.Contains(t, byPath, subagentURI)
	assert.Equal(t, "my-cool-project", byPath[harvestURI].ProjectHint)
	assert.Equal(t, "demo", byPath[localURI].ProjectHint)
	assert.Equal(t, "demo", byPath[subagentURI].ProjectHint)
}

func TestCursorS3LayoutRank(t *testing.T) {
	root := "s3://bucket/laptop/raw/cursor/"
	tests := []struct {
		name string
		uri  string
		want int
	}{
		{name: "nested", uri: root + "proj/agent-transcripts/sess/sess.jsonl", want: cursorS3LayoutNested},
		{name: "flat", uri: root + "proj/agent-transcripts/sess.jsonl", want: cursorS3LayoutFlat},
		{name: "harvest", uri: root + "proj/sess.jsonl", want: cursorS3LayoutFlat},
		{name: "subagent", uri: root + "proj/agent-transcripts/parent/subagents/sess.jsonl", want: cursorS3LayoutSubagent},
		{name: "harvest project named subagents", uri: root + "subagents/sess.jsonl", want: cursorS3LayoutFlat},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, cursorS3LayoutRank(tt.uri))
		})
	}
}

func TestCursorS3ParentFamily(t *testing.T) {
	for _, root := range []string{"s3://bucket/host-a/raw/cursor", "s3://bucket/archive"} {
		aliasRoot := strings.Replace(root, "bucket", "other-bucket", 1)
		roots := []string{root + "/agent-transcripts/agent-transcripts", aliasRoot, root}
		parent := root + "/agent-transcripts/agent-transcripts/shared/shared.jsonl"
		child := root + "/agent-transcripts/agent-transcripts/shared/subagents/child.txt"
		key, id, prefixes := CursorS3ParentFamily(roots, parent)
		childKey, childID, childPrefixes := CursorS3ParentFamily(roots, child)
		assert.Equal(t, key, childKey)
		assert.Equal(t, id, childID)
		assert.ElementsMatch(t, prefixes, childPrefixes)
		assert.Contains(t, prefixes, CursorS3ChildPrefix{Path: root + "/agent-transcripts/agent-transcripts/shared/subagents/", Root: root})
		assert.Contains(t, prefixes, CursorS3ChildPrefix{Path: aliasRoot + "/agent-transcripts/agent-transcripts/shared/subagents/", Root: aliasRoot})
		aliasKey, canonicalRoot := CursorS3SourceKey(roots, aliasRoot+"/agent-transcripts/shared.txt")
		assert.Equal(t, key, aliasKey)
		assert.Equal(t, aliasRoot, canonicalRoot)
		otherKey, _ := CursorS3SourceKey(roots, root+"/cursor/shared.txt")
		assert.NotEqual(t, key, otherKey)
		otherKey, canonicalRoot = CursorS3SourceKey(roots, root+"/agent-transcripts/other.txt")
		assert.NotEqual(t, key, otherKey)
		assert.Equal(t, root, canonicalRoot)
		key, _, prefixes = CursorS3ParentFamily(roots, "s3://unconfigured/project/shared.txt")
		assert.Empty(t, key)
		assert.Empty(t, prefixes)
	}
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
