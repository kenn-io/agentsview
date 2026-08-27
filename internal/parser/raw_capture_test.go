package parser

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type rawCaptureTestProvider struct {
	ProviderBase
	plan   RawCapturePlan
	err    error
	called bool
}

func (p *rawCaptureTestProvider) Parse(
	context.Context,
	ParseRequest,
) (ParseOutcome, error) {
	return ParseOutcome{}, nil
}

func (p *rawCaptureTestProvider) PlanRawCapture(
	_ context.Context,
	_ SourceRef,
) (RawCapturePlan, error) {
	p.called = true
	return p.plan, p.err
}

type rawCaptureUndeclaredProvider struct {
	ProviderBase
}

func (p *rawCaptureUndeclaredProvider) Parse(
	context.Context,
	ParseRequest,
) (ParseOutcome, error) {
	return ParseOutcome{}, nil
}

func rawCaptureTestCapabilities() Capabilities {
	return Capabilities{RawCapture: RawCaptureCapabilities{
		Support:  CapabilitySupported,
		Shape:    RawCaptureShapeFiles,
		Append:   RawCaptureAppendOne,
		Snapshot: RawCaptureSnapshotNone,
	}}
}

func canonicalRawCaptureTestPath(t *testing.T, path string) string {
	t.Helper()
	canonical, err := filepath.EvalSymlinks(path)
	require.NoError(t, err)
	return canonical
}

func TestResolveRawCapturePlanLeavesUnsupportedProviderUntouched(t *testing.T) {
	provider := &rawCaptureTestProvider{
		Def: AgentDef{Type: AgentClaude}}

	plan, ok, err := ResolveRawCapturePlan(t.Context(), provider, SourceRef{
		Provider: AgentClaude,
		Key:      "source-1",
	})

	require.NoError(t, err)
	assert.False(t, ok)
	assert.Empty(t, plan)
	assert.False(t, provider.called, "unsupported capability must not invoke the planner")
}

func TestResolveRawCapturePlanRequiresDeclaredInterface(t *testing.T) {
	provider := &rawCaptureUndeclaredProvider{
		Def:  AgentDef{Type: AgentClaude},
		Caps: rawCaptureTestCapabilities()}

	_, ok, err := ResolveRawCapturePlan(t.Context(), provider, SourceRef{
		Provider: AgentClaude,
		Key:      "source-1",
	})

	require.Error(t, err)
	assert.False(t, ok)
	assert.ErrorIs(t, err, ErrUnsupportedProviderFeature)
}

func TestResolveRawCapturePlanValidatesProviderOwnedPlan(t *testing.T) {
	captureRoot := t.TempDir()
	localPath := filepath.Join(captureRoot, "project", "session.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(localPath), 0o755))
	require.NoError(t, os.WriteFile(localPath, []byte("one\n"), 0o600))

	valid := RawCapturePlan{
		ConfiguredRoot: captureRoot,
		CaptureRoot:    captureRoot,
		SourceKey:      "source-1",
		Entries: []RawCaptureEntry{{
			Path:       "project/session.jsonl",
			LocalPath:  localPath,
			Appendable: true,
		}},
	}

	tests := []struct {
		name       string
		source     SourceRef
		mutatePlan func(*RawCapturePlan)
	}{
		{
			name:   "provider mismatch",
			source: SourceRef{Provider: AgentCodex, Key: "source-1"},
		},
		{
			name:   "absolute logical path",
			source: SourceRef{Provider: AgentClaude, Key: "source-1"},
			mutatePlan: func(plan *RawCapturePlan) {
				plan.Entries[0].Path = "/session.jsonl"
			},
		},
		{
			name:   "parent traversal",
			source: SourceRef{Provider: AgentClaude, Key: "source-1"},
			mutatePlan: func(plan *RawCapturePlan) {
				plan.Entries[0].Path = "../session.jsonl"
			},
		},
		{
			name:   "control character",
			source: SourceRef{Provider: AgentClaude, Key: "source-1"},
			mutatePlan: func(plan *RawCapturePlan) {
				plan.Entries[0].Path = "project/session\x01.jsonl"
			},
		},
		{
			name:   "alternate data stream",
			source: SourceRef{Provider: AgentClaude, Key: "source-1"},
			mutatePlan: func(plan *RawCapturePlan) {
				plan.Entries[0].Path = "project/session.jsonl:secret"
			},
		},
		{
			name:   "Windows reserved component",
			source: SourceRef{Provider: AgentClaude, Key: "source-1"},
			mutatePlan: func(plan *RawCapturePlan) {
				plan.Entries[0].Path = "project/CON.txt"
			},
		},
		{
			name:   "Windows trailing dot",
			source: SourceRef{Provider: AgentClaude, Key: "source-1"},
			mutatePlan: func(plan *RawCapturePlan) {
				plan.Entries[0].Path = "project/session."
			},
		},
		{
			name:   "duplicate logical path",
			source: SourceRef{Provider: AgentClaude, Key: "source-1"},
			mutatePlan: func(plan *RawCapturePlan) {
				plan.Entries = append(plan.Entries, plan.Entries[0])
			},
		},
		{
			name:   "outside capture root",
			source: SourceRef{Provider: AgentClaude, Key: "source-1"},
			mutatePlan: func(plan *RawCapturePlan) {
				outside := filepath.Join(t.TempDir(), "session.jsonl")
				require.NoError(t, os.WriteFile(outside, []byte("two\n"), 0o600))
				plan.Entries[0].LocalPath = outside
			},
		},
		{
			name:   "two appendable entries",
			source: SourceRef{Provider: AgentClaude, Key: "source-1"},
			mutatePlan: func(plan *RawCapturePlan) {
				other := filepath.Join(captureRoot, "project", "other.jsonl")
				require.NoError(t, os.WriteFile(other, []byte("two\n"), 0o600))
				plan.Entries = append(plan.Entries, RawCaptureEntry{
					Path:       "project/other.jsonl",
					LocalPath:  other,
					Appendable: true,
				})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := valid
			plan.Entries = append([]RawCaptureEntry(nil), valid.Entries...)
			if tt.mutatePlan != nil {
				tt.mutatePlan(&plan)
			}
			provider := &rawCaptureTestProvider{
				Def:  AgentDef{Type: AgentClaude},
				Caps: rawCaptureTestCapabilities(),
				plan: plan,
			}

			_, ok, err := ResolveRawCapturePlan(t.Context(), provider, tt.source)

			require.Error(t, err)
			assert.False(t, ok)
			assert.ErrorIs(t, err, ErrInvalidRawCapturePlan)
		})
	}
}

func TestResolveRawCapturePlanReturnsIndependentValidatedCopy(t *testing.T) {
	captureRoot := t.TempDir()
	localPath := filepath.Join(captureRoot, "session.jsonl")
	require.NoError(t, os.WriteFile(localPath, []byte("one\n"), 0o600))
	provider := &rawCaptureTestProvider{
		Def:  AgentDef{Type: AgentClaude},
		Caps: rawCaptureTestCapabilities(),
		plan: RawCapturePlan{
			ConfiguredRoot: captureRoot,
			CaptureRoot:    captureRoot,
			SourceKey:      "source-1",
			Entries: []RawCaptureEntry{{
				Path:       "session.jsonl",
				LocalPath:  localPath,
				Appendable: true,
			}},
		},
	}

	plan, ok, err := ResolveRawCapturePlan(t.Context(), provider, SourceRef{
		Provider: AgentClaude,
		Key:      "source-1",
	})

	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, plan.Entries, 1)
	assert.Equal(t, "session.jsonl", plan.Entries[0].Path)
	provider.plan.Entries[0].Path = "mutated.jsonl"
	assert.Equal(t, "session.jsonl", plan.Entries[0].Path)
}

func TestResolveRawCapturePlanRejectsUnknownAppendPolicy(t *testing.T) {
	captureRoot := t.TempDir()
	localPath := filepath.Join(captureRoot, "session.jsonl")
	require.NoError(t, os.WriteFile(localPath, []byte("one\n"), 0o600))
	capabilities := rawCaptureTestCapabilities()
	capabilities.RawCapture.Append = RawCaptureAppendPolicy(99)
	provider := &rawCaptureTestProvider{
		Def:  AgentDef{Type: AgentClaude},
		Caps: capabilities,
		plan: RawCapturePlan{
			ConfiguredRoot: captureRoot,
			CaptureRoot:    captureRoot,
			SourceKey:      "source-1",
			Entries: []RawCaptureEntry{{
				Path:       "session.jsonl",
				LocalPath:  localPath,
				Appendable: true,
			}},
		},
	}

	_, supported, err := ResolveRawCapturePlan(t.Context(), provider, SourceRef{
		Provider: AgentClaude,
		Key:      "source-1",
	})

	require.Error(t, err)
	assert.False(t, supported)
	assert.ErrorIs(t, err, ErrInvalidRawCapturePlan)
}

func TestClaudeProviderPlansAndParsesNestedToolResults(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "project", "session.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(sourcePath), 0o755))
	toolResultPath := filepath.Join(
		root, "project", "session", "tool-results", "batches", "result.txt",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(toolResultPath), 0o755))
	fullOutput := "full nested output\n"
	require.NoError(t, os.WriteFile(toolResultPath, []byte(fullOutput), 0o600))
	resultPathJSON := mustJSONString(t, toolResultPath)
	persistedContentJSON := mustJSONString(t,
		"<persisted-output>\n"+
			"Output too large. Full output saved to: "+toolResultPath+
			"\n\nPreview (first 2KB):\npreview only\n</persisted-output>")
	content := strings.Join([]string{
		`{"type":"user","timestamp":"2024-01-01T00:00:00Z","uuid":"u1","message":{"content":"run it"},"cwd":"/tmp/project"}`,
		`{"type":"assistant","timestamp":"2024-01-01T00:00:01Z","uuid":"a1","parentUuid":"u1","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"make logs"}}]}}`,
		`{"type":"user","timestamp":"2024-01-01T00:00:02Z","uuid":"u2","parentUuid":"a1","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":` + persistedContentJSON + `,"is_error":false}]},"toolUseResult":{"persistedOutputPath":` + resultPathJSON + `,"persistedOutputSize":19}}`,
	}, "\n")
	require.NoError(t, os.WriteFile(sourcePath, []byte(content), 0o600))
	provider, ok := NewProvider(AgentClaude, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	plan, supported, err := ResolveRawCapturePlan(t.Context(), provider, sources[0])

	require.NoError(t, err)
	require.True(t, supported)
	assert.Equal(t, canonicalRawCaptureTestPath(t, root), plan.ConfiguredRoot)
	assert.Equal(t, canonicalRawCaptureTestPath(t, root), plan.CaptureRoot)
	assert.Equal(t, sources[0].Key, plan.SourceKey)
	require.Len(t, plan.Entries, 2)
	assert.Equal(t, "project/session.jsonl", plan.Entries[0].Path)
	assert.Equal(t, canonicalRawCaptureTestPath(t, sourcePath), plan.Entries[0].LocalPath)
	assert.True(t, plan.Entries[0].Appendable)
	assert.Equal(t, "project/session/tool-results/batches/result.txt", plan.Entries[1].Path)
	assert.Equal(t, canonicalRawCaptureTestPath(t, toolResultPath), plan.Entries[1].LocalPath)
	assert.False(t, plan.Entries[1].Appendable)

	parsed, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	require.Len(t, parsed.Results, 1)
	messages := parsed.Results[0].Result.Messages
	require.Len(t, messages, 3)
	require.Len(t, messages[2].ToolResults, 1)
	assert.Equal(t, fullOutput, DecodeContent(messages[2].ToolResults[0].ContentRaw))
}

func TestCodexProviderPlansTranscriptWithOptionalIndex(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "sessions")
	sourcePath := filepath.Join(
		root,
		"2026", "08", "25",
		"rollout-2026-08-25T10-00-00-11111111-1111-4111-8111-111111111111.jsonl",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(sourcePath), 0o755))
	require.NoError(t, os.WriteFile(sourcePath, []byte("{}\n"), 0o600))
	indexPath := filepath.Join(base, CodexSessionIndexFilename)
	require.NoError(t, os.WriteFile(indexPath, []byte("{}\n"), 0o600))
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	plan, supported, err := ResolveRawCapturePlan(t.Context(), provider, sources[0])

	require.NoError(t, err)
	require.True(t, supported)
	assert.Equal(t, canonicalRawCaptureTestPath(t, root), plan.ConfiguredRoot)
	assert.Equal(t, canonicalRawCaptureTestPath(t, base), plan.CaptureRoot)
	assert.Equal(t, sources[0].Key, plan.SourceKey)
	require.Len(t, plan.Entries, 2)
	assert.Equal(t, "session_index.jsonl", plan.Entries[0].Path)
	assert.Equal(t, canonicalRawCaptureTestPath(t, indexPath), plan.Entries[0].LocalPath)
	assert.False(t, plan.Entries[0].Appendable)
	assert.Equal(t, "sessions/2026/08/25/"+filepath.Base(sourcePath), plan.Entries[1].Path)
	assert.Equal(t, canonicalRawCaptureTestPath(t, sourcePath), plan.Entries[1].LocalPath)
	assert.True(t, plan.Entries[1].Appendable)

	require.NoError(t, os.Remove(indexPath))
	plan, supported, err = ResolveRawCapturePlan(t.Context(), provider, sources[0])
	require.NoError(t, err)
	require.True(t, supported)
	require.Len(t, plan.Entries, 1)
	assert.Equal(t, "sessions/2026/08/25/"+filepath.Base(sourcePath), plan.Entries[0].Path)
	assert.True(t, plan.Entries[0].Appendable)
}

func TestClaudeProviderPlansThroughSymlinkedRoot(t *testing.T) {
	base := t.TempDir()
	realRoot := filepath.Join(base, "real-projects")
	linkedRoot := filepath.Join(base, "linked-projects")
	sourcePath := filepath.Join(realRoot, "project", "session.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(sourcePath), 0o755))
	require.NoError(t, os.WriteFile(sourcePath, []byte("{}\n"), 0o600))
	if err := os.Symlink(realRoot, linkedRoot); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	provider, ok := NewProvider(AgentClaude, ProviderConfig{Roots: []string{linkedRoot}})
	require.True(t, ok)
	linkedSource := filepath.Join(linkedRoot, "project", "session.jsonl")
	sources, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path:      linkedSource,
		WatchRoot: linkedRoot,
		EventKind: "write",
	})
	require.NoError(t, err)
	require.Len(t, sources, 1)

	plan, supported, err := ResolveRawCapturePlan(t.Context(), provider, sources[0])

	require.NoError(t, err)
	require.True(t, supported)
	require.Len(t, plan.Entries, 1)
	assert.Equal(t, canonicalRawCaptureTestPath(t, realRoot), plan.ConfiguredRoot)
	assert.Equal(t, canonicalRawCaptureTestPath(t, sourcePath), plan.Entries[0].LocalPath)
}

func TestCodexProviderPlansThroughSymlinkedRoot(t *testing.T) {
	base := t.TempDir()
	realRoot := filepath.Join(base, "rollouts")
	linkedRoot := filepath.Join(base, "sessions")
	realSource := filepath.Join(
		realRoot,
		"2026", "08", "25",
		"rollout-2026-08-25T10-00-00-11111111-1111-4111-8111-111111111111.jsonl",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(realSource), 0o755))
	require.NoError(t, os.WriteFile(realSource, []byte("{}\n"), 0o600))
	if err := os.Symlink(realRoot, linkedRoot); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	indexPath := filepath.Join(base, CodexSessionIndexFilename)
	require.NoError(t, os.WriteFile(indexPath, []byte("{}\n"), 0o600))
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{linkedRoot}})
	require.True(t, ok)
	linkedSource := filepath.Join(
		linkedRoot,
		"2026", "08", "25",
		filepath.Base(realSource),
	)
	sources, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path:      linkedSource,
		WatchRoot: linkedRoot,
		EventKind: "write",
	})
	require.NoError(t, err)
	require.Len(t, sources, 1)

	plan, supported, err := ResolveRawCapturePlan(t.Context(), provider, sources[0])

	require.NoError(t, err)
	require.True(t, supported)
	assert.Equal(t, canonicalRawCaptureTestPath(t, realRoot), plan.ConfiguredRoot)
	assert.Equal(t, canonicalRawCaptureTestPath(t, base), plan.CaptureRoot)
	require.Len(t, plan.Entries, 2)
	assert.Equal(t, canonicalRawCaptureTestPath(t, indexPath), plan.Entries[0].LocalPath)
	assert.Equal(t, canonicalRawCaptureTestPath(t, realSource), plan.Entries[1].LocalPath)
}

func TestTraeXProviderDoesNotOptIntoRawCapture(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(
		root,
		"2026", "08", "25",
		"rollout-2026-08-25T10-00-00-11111111-1111-4111-8111-111111111111.jsonl",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(sourcePath), 0o755))
	require.NoError(t, os.WriteFile(sourcePath, []byte("{}\n"), 0o600))
	provider, ok := NewProvider(AgentTraeX, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	plan, supported, err := ResolveRawCapturePlan(t.Context(), provider, sources[0])

	require.NoError(t, err)
	assert.False(t, supported)
	assert.Empty(t, plan)
}

func TestResolveRawCapturePlanPropagatesProviderError(t *testing.T) {
	want := errors.New("capture plan failed")
	provider := &rawCaptureTestProvider{
		Def:  AgentDef{Type: AgentClaude},
		Caps: rawCaptureTestCapabilities(),
		err:  want,
	}

	_, supported, err := ResolveRawCapturePlan(t.Context(), provider, SourceRef{
		Provider: AgentClaude,
		Key:      "source-1",
	})

	assert.False(t, supported)
	assert.ErrorIs(t, err, want)
}

func TestResolveRawCapturePlanDoesNotExposeLocalPathInFilesystemError(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "private", "missing.jsonl")
	provider := &rawCaptureTestProvider{
		Def:  AgentDef{Type: AgentClaude},
		Caps: rawCaptureTestCapabilities(),
		plan: RawCapturePlan{
			ConfiguredRoot: root,
			CaptureRoot:    root,
			SourceKey:      "source-1",
			Entries: []RawCaptureEntry{{
				Path: "project/session.jsonl", LocalPath: missing, Appendable: true,
			}},
		},
	}

	_, _, err := ResolveRawCapturePlan(t.Context(), provider, SourceRef{
		Provider: AgentClaude,
		Key:      "source-1",
	})

	require.Error(t, err)
	assert.NotContains(t, err.Error(), root)
	assert.ErrorIs(t, err, ErrInvalidRawCapturePlan)
}
