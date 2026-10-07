package parser

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestNewPRLink(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		url        string
		repository string
		number     int
		want       PRLink
		wantOK     bool
	}{
		{
			name:       "github fields supplied",
			url:        "https://GitHub.com/owner/repo/pull/123/?tab=files#diff",
			repository: "owner/repo",
			number:     123,
			want: PRLink{
				URL: "https://github.com/owner/repo/pull/123", Host: "github.com",
				Repository: "owner/repo", Number: 123,
			},
			wantOK: true,
		},
		{
			name:       "explicit fields win over an unrecognized path",
			url:        "https://forge.example.com/r/9",
			repository: "team/repo",
			number:     9,
			want: PRLink{
				URL: "https://forge.example.com/r/9", Host: "forge.example.com",
				Repository: "team/repo", Number: 9,
			},
			wantOK: true,
		},
		{name: "missing repository", url: "https://github.com/owner/repo/pull/1", number: 1},
		{name: "missing number", url: "https://github.com/owner/repo/pull/1", repository: "owner/repo"},
		{name: "missing url", repository: "owner/repo", number: 1},
		{name: "non-web scheme", url: "file:///owner/repo/pull/1", repository: "owner/repo", number: 1},
		{name: "credentials in url", url: "https://user:secret@github.com/owner/repo/pull/1", repository: "owner/repo", number: 1},
		{name: "no number anywhere", url: "https://github.com/owner/repo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := NewPRLink(
				tt.url, tt.repository, tt.number,
			)
			require.Equal(t, tt.wantOK, ok)
			if !tt.wantOK {
				return
			}
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestClaudePRLinksDeduplicated(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "pr-links.jsonl")
	content := strings.Join([]string{
		`{"type":"user","sessionId":"pr-links","uuid":"u1","timestamp":"2026-10-05T03:00:00Z","message":{"content":"open a PR"}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2026-10-05T03:01:00Z","message":{"content":[{"type":"text","text":"done"}]}}`,
		`{"type":"pr-link","prNumber":123,"prUrl":"https://github.com/owner/repo/pull/123","prRepository":"owner/repo","sessionId":"pr-links","timestamp":"2026-10-05T03:21:20.583Z"}`,
		`{"type":"pr-link","prNumber":9,"prUrl":"https://github.com/owner/other/pull/9","prRepository":"owner/other","sessionId":"pr-links","timestamp":"2026-10-05T03:30:00Z"}`,
		`{"type":"pr-link","prNumber":123,"prUrl":"https://github.com/owner/repo/pull/123","prRepository":"owner/repo","sessionId":"pr-links","timestamp":"2026-10-05T03:40:00Z"}`,
		`{"type":"pr-link","prUrl":"not a url","sessionId":"pr-links"}`,
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	results, _, err := claudeParseWithExclusions(path, "project", "local")
	require.NoError(t, err)
	require.Len(t, results, 1)

	links := results[0].Session.PRLinks
	require.Len(t, links, 2)
	assert.Equal(t, "https://github.com/owner/repo/pull/123", links[0].URL)
	assert.Equal(t, "owner/repo", links[0].Repository)
	assert.Equal(t, 123, links[0].Number)
	assert.Equal(t, "owner/other", links[1].Repository)
	// pr-link records are metadata, not transcript messages.
	assert.Len(t, results[0].Messages, 2)
}

func TestClaudePRLinksKeepEveryDistinctURL(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "many-links.jsonl")
	var content strings.Builder
	content.WriteString(claudeProviderFixture("Open pull requests"))
	for i := 1; i <= 101; i++ {
		fmt.Fprintf(&content, "\n{\"type\":\"pr-link\",\"prUrl\":\"https://github.com/owner/repo/pull/%d\",\"prRepository\":\"owner/repo\",\"prNumber\":%d}", i, i)
	}
	content.WriteString("\n{\"type\":\"pr-link\",\"prUrl\":\"https://github.com/owner/repo/pull/101\",\"prRepository\":\"owner/repo\",\"prNumber\":101}\n")
	require.NoError(t, os.WriteFile(path, []byte(content.String()), 0o600))
	results, _, err := claudeParseWithExclusions(path, "project", "local")
	require.NoError(t, err)
	require.Len(t, results, 1)
	links := results[0].Session.PRLinks
	require.Len(t, links, 101)
	assert.Equal(t, "https://github.com/owner/repo/pull/101", links[100].URL)
}

func TestClaudeIncrementalEscalatesOnlyForPRLinkChanges(t *testing.T) {
	stored := map[string]struct{}{
		"https://github.com/owner/repo/pull/123": {},
	}
	full := make(map[string]struct{}, 100)
	for i := range 100 {
		full[fmt.Sprintf("https://github.com/owner/repo/pull/%d", 1000+i)] = struct{}{}
	}
	record := func(n int, ts string) string {
		line := fmt.Sprintf(`{"type":"pr-link","prNumber":%d,"prUrl":"https://github.com/owner/repo/pull/%d","prRepository":"owner/repo"`, n, n)
		if ts != "" {
			line += `,"timestamp":"` + ts + `"`
		}
		return line + "}"
	}
	tests := []struct {
		name       string
		stored     map[string]struct{}
		appended   string
		wantStatus IncrementalStatus
	}{
		{
			name: "repeated link stays incremental", stored: stored,
			appended: record(123, "2026-10-05T03:40:00Z"), wantStatus: IncrementalApplied,
		},
		{
			name: "new link needs full parse", stored: stored,
			appended: record(124, "2026-10-05T03:41:00Z"), wantStatus: IncrementalNeedsFullParse,
		},
		{
			name: "untimed repeat stays incremental", stored: stored,
			appended: record(123, ""), wantStatus: IncrementalApplied,
		},
		{
			name: "101st link needs full parse", stored: full,
			appended: record(124, "2026-10-05T03:41:00Z"), wantStatus: IncrementalNeedsFullParse,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "project", "incremental.jsonl")
			writeSourceFile(t, path, claudeProviderFixture("First question"))
			info, err := os.Stat(path)
			require.NoError(t, err)
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
			require.NoError(t, err)
			_, err = f.WriteString(tt.appended + "\n" +
				testjsonl.ClaudeUserJSON("Appended question", tsEarlyS5) + "\n")
			require.NoError(t, err)
			require.NoError(t, f.Close())
			current, err := os.Stat(path)
			require.NoError(t, err)

			provider, ok := NewProvider(AgentClaude, ProviderConfig{Roots: []string{root}})
			require.True(t, ok)
			source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: "incremental"})
			require.NoError(t, err)
			require.True(t, ok)
			outcome, status, err := provider.ParseIncremental(t.Context(), IncrementalRequest{
				Source: source, Fingerprint: SourceFingerprint{Key: path, Size: current.Size()},
				SessionID: "incremental", Offset: info.Size(), StartOrdinal: 2,
				StoredPRLinks: tt.stored,
			})
			require.NoError(t, err)
			assert.Equal(t, tt.wantStatus, status)
			if tt.wantStatus == IncrementalNeedsFullParse {
				assert.True(t, outcome.ForceReplace)
			}
		})
	}
}
