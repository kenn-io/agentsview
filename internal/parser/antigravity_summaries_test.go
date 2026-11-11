package parser

import (
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/testjsonl"
)

const (
	summaryTestID     = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	summaryTestCLIID  = "44444444-5555-6666-7777-888888888888"
	summaryTestParent = "cccccccc-dddd-eeee-ffff-000000000000"
)

func TestAntigravityProviderAppliesSummaryStore(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "conversations", summaryTestID+".db")
	writeAntigravityIDEProviderFixture(t, root, summaryTestID)
	testjsonl.WriteAntigravitySummaries(t, root, map[string]testjsonl.AntigravitySummary{
		// Only the first folder of a multi-folder workspace becomes the cwd,
		// and a parent ID is canonicalized.
		summaryTestID: {
			WorkspaceURIs: `["file:///tmp/ws-proj","file:///tmp/second-folder"]`,
			ParentID:      strings.ToUpper(summaryTestParent),
		},
		"bbbbbbbb-cccc-dddd-eeee-ffffffffffff": {WorkspaceURIs: `["file:///tmp/other-proj"]`},
	})
	provider, ok := NewProvider(AgentAntigravity, ProviderConfig{Roots: []string{root}, Machine: "devbox"})
	require.True(t, ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	streamed := streamAntigravitySources(t, provider)
	require.Len(t, streamed, 1)
	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: summaryTestID})
	require.NoError(t, err)
	require.True(t, ok)
	changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{Path: dbPath + "-wal", EventKind: "write"})
	require.NoError(t, err)
	require.Len(t, changed, 1)
	for name, source := range map[string]SourceRef{
		"discover": discovered[0], "discover each": streamed[0], "find source": found, "changed path": changed[0],
	} {
		assert.Equal(t, SourceCwdResolution{State: SourceCwdResolved, Path: "/tmp/ws-proj"}, source.CwdResolution, name)
		assert.Equal(t, "/tmp/ws-proj", source.ProjectHint, name)
	}

	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: discovered[0]})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	session := outcome.Results[0].Result.Session
	assert.Equal(t, "/tmp/ws-proj", session.Cwd)
	assert.Equal(t, "ws_proj", session.Project)
	assert.Equal(t, "antigravity:"+summaryTestParent, session.ParentSessionID)
	assert.Equal(t, RelSubagent, session.RelationshipType)
}

func TestAntigravityProviderSummaryResolutionStates(t *testing.T) {
	tests := map[string]struct {
		store func(t *testing.T, root string)
		want  SourceCwdState
	}{
		"no summary store": {
			store: func(*testing.T, string) {},
			want:  SourceCwdUnspecified,
		},
		"conversation not listed": {
			store: func(t *testing.T, root string) {
				t.Helper()
				testjsonl.WriteAntigravitySummaries(t, root, map[string]testjsonl.AntigravitySummary{
					"bbbbbbbb-cccc-dddd-eeee-ffffffffffff": {WorkspaceURIs: `["file:///tmp/other-proj"]`},
				})
			},
			want: SourceCwdUnspecified,
		},
		"no workspace folder, parent naming itself": {
			store: func(t *testing.T, root string) {
				t.Helper()
				testjsonl.WriteAntigravitySummaries(t, root, map[string]testjsonl.AntigravitySummary{
					summaryTestID: {ParentID: summaryTestID},
				})
			},
			// A row naming no folder means the conversation has none.
			want: SourceCwdNone,
		},
		"store predates the workspace and parent columns": {
			store: func(t *testing.T, root string) {
				t.Helper()
				conn, err := sql.Open("sqlite3", filepath.Join(root, AntigravitySummariesDBName))
				require.NoError(t, err)
				defer conn.Close()
				_, err = conn.ExecContext(t.Context(), testjsonl.AntigravityTitleSchema)
				require.NoError(t, err)
				_, err = conn.ExecContext(t.Context(), `INSERT INTO conversation_summaries VALUES (?, 'Title')`, summaryTestID)
				require.NoError(t, err)
			},
			want: SourceCwdUnspecified,
		},
		// A store that records parents but not workspaces says nothing
		// about the cwd.
		"store without a workspace column": {
			store: func(t *testing.T, root string) {
				t.Helper()
				conn, err := sql.Open("sqlite3", filepath.Join(root, AntigravitySummariesDBName))
				require.NoError(t, err)
				defer conn.Close()
				_, err = conn.ExecContext(t.Context(), `CREATE TABLE conversation_summaries (
					conversation_id TEXT PRIMARY KEY, title TEXT, parent_conversation_id TEXT NOT NULL DEFAULT '')`)
				require.NoError(t, err)
				_, err = conn.ExecContext(t.Context(), `INSERT INTO conversation_summaries VALUES (?, 'Title', '')`, summaryTestID)
				require.NoError(t, err)
			},
			want: SourceCwdUnspecified,
		},
		"remote development folder": {
			store: func(t *testing.T, root string) {
				t.Helper()
				testjsonl.WriteAntigravitySummaries(t, root, map[string]testjsonl.AntigravitySummary{
					summaryTestID: {WorkspaceURIs: `["vscode-remote://ssh-remote%2Bbuildbox/home/dev/proj"]`},
				})
			},
			want: SourceCwdAmbiguous,
		},
		// Never read before, so there is no good read to stand in.
		"unreadable store": {
			store: func(t *testing.T, root string) {
				t.Helper()
				mustWrite(t, filepath.Join(root, AntigravitySummariesDBName),
					[]byte("not a sqlite database, long enough to fill a header page"))
			},
			want: SourceCwdUnavailable,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeAntigravityIDEProviderFixture(t, root, summaryTestID)
			tc.store(t, root)
			provider, ok := NewProvider(AgentAntigravity, ProviderConfig{Roots: []string{root}, Machine: "devbox"})
			require.True(t, ok)

			discovered, err := provider.Discover(t.Context())
			require.NoError(t, err)
			require.Len(t, discovered, 1)
			assert.Equal(t, SourceCwdResolution{State: tc.want}, discovered[0].CwdResolution)
			assert.Empty(t, discovered[0].ProjectHint)
			fingerprint, err := provider.Fingerprint(t.Context(), discovered[0])
			require.NoError(t, err)
			outcome, err := provider.Parse(t.Context(), ParseRequest{Source: discovered[0], Fingerprint: fingerprint})
			require.NoError(t, err)
			require.Len(t, outcome.Results, 1)
			session := outcome.Results[0].Result.Session
			assert.Empty(t, session.Cwd)
			assert.Empty(t, session.Project)
			assert.Empty(t, session.ParentSessionID)
			assert.Equal(t, fingerprint.Hash, session.File.Hash)
		})
	}
}

func TestAntigravityProviderMarksRemoteWorkspace(t *testing.T) {
	tests := map[string]func(string) ProviderConfig{
		"path rewriter": func(root string) ProviderConfig {
			return ProviderConfig{
				Roots: []string{root}, Machine: "localbox",
				PathRewriter: func(path string) string { return "remote:" + path },
			}
		},
		"foreign source machine": func(root string) ProviderConfig {
			return ProviderConfig{
				Roots: []string{root}, Machine: "localbox",
				SourceMachines: map[string]string{root: "archivebox"},
			}
		},
	}
	for name, config := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeAntigravityIDEProviderFixture(t, root, summaryTestID)
			// A git repository on this machine at the recorded path's parent
			// must not decide the remote session's project.
			localRepo := filepath.Join(t.TempDir(), "conflicting-local-repo")
			workspace := filepath.Join(localRepo, "recorded-project")
			mustMkdir(t, filepath.Join(localRepo, ".git"))
			mustMkdir(t, workspace)
			testjsonl.WriteAntigravitySummaries(t, root, map[string]testjsonl.AntigravitySummary{
				summaryTestID: {WorkspaceURIs: `["` + fileURI(workspace) + `"]`},
			})

			provider, ok := NewProvider(AgentAntigravity, config(root))
			require.True(t, ok)
			source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: summaryTestID})
			require.NoError(t, err)
			require.True(t, ok)
			assert.Equal(t, SourceCwdResolution{State: SourceCwdRemote}, source.CwdResolution)
			assert.Equal(t, workspace, source.ProjectHint)

			outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source})
			require.NoError(t, err)
			require.Len(t, outcome.Results, 1)
			assert.Empty(t, outcome.Results[0].Result.Session.Cwd)
			assert.Equal(t, "recorded_project", outcome.Results[0].Result.Session.Project)
		})
	}
}

func TestAntigravityWorkspacePath(t *testing.T) {
	tests := []struct {
		uri  string
		want string
	}{
		{uri: "file:///home/dev/proj", want: "/home/dev/proj"},
		{uri: "file:///home/dev/proj/", want: "/home/dev/proj"},
		{uri: "file:///home/dev/my%20proj", want: "/home/dev/my proj"},
		{uri: "file://localhost/home/dev/proj", want: "/home/dev/proj"},
		{uri: "file:///", want: "/"},
		{uri: "file:///c%3A/Users/dev/proj", want: `c:\Users\dev\proj`},
		{uri: "file:///C:/Users/dev/proj/", want: `C:\Users\dev\proj`},
		{uri: "file:///d%3A", want: `d:\`},
		{uri: "file://fileserver/share/proj", want: `\\fileserver\share\proj`},
		{uri: "vscode-remote://ssh-remote%2Bbuildbox/home/dev/proj", want: ""},
		{uri: "https://example.com/proj", want: ""},
		{uri: "file:relative/proj", want: ""},
		{uri: "", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.uri, func(t *testing.T) {
			assert.Equal(t, tc.want, antigravityWorkspacePath(tc.uri))
		})
	}
}

func TestAntigravityCLIProviderFallsBackToRecordedWorkspace(t *testing.T) {
	tests := map[string]struct {
		history bool
		cache   string
		want    string
	}{
		// The CLI keeps only each folder's latest conversation in its cache,
		// and current history rows carry no conversation ID, so an older
		// conversation is mapped by the summary store alone.
		"only the summary store maps it": {want: "/tmp/store-proj"},
		"history maps it":                {history: true, want: "/tmp/db-proj"},
		"cache maps it": {
			cache: `{"/tmp/cache-proj":"` + summaryTestCLIID + `"}`,
			want:  "/tmp/cache-proj",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeAntigravityCLIProviderFixture(t, root, summaryTestCLIID)
			if !tc.history {
				require.NoError(t, os.Remove(filepath.Join(root, "history.jsonl")))
			}
			if tc.cache != "" {
				cachePath := filepath.Join(root, "cache", "last_conversations.json")
				mustMkdir(t, filepath.Dir(cachePath))
				mustWrite(t, cachePath, []byte(tc.cache))
			}
			testjsonl.WriteAntigravitySummaries(t, root, map[string]testjsonl.AntigravitySummary{
				summaryTestCLIID: {WorkspaceURIs: `["file:///tmp/store-proj"]`},
			})
			provider, ok := NewProvider(AgentAntigravityCLI, ProviderConfig{Roots: []string{root}, Machine: "devbox"})
			require.True(t, ok)

			discovered, err := provider.Discover(t.Context())
			require.NoError(t, err)
			conversation := filepath.Join(root, "conversations", summaryTestCLIID+".db")
			for name, sources := range map[string][]SourceRef{
				"discover": discovered, "discover each": streamAntigravitySources(t, provider),
			} {
				assert.Equal(t, SourceCwdResolution{State: SourceCwdResolved, Path: tc.want},
					antigravitySourceAt(t, sources, conversation).CwdResolution, name)
			}

			// The parse resolves the workspace itself when the source carries
			// none, as for a source handed over without discovery.
			source := antigravitySourceAt(t, discovered, conversation)
			source.CwdResolution = SourceCwdResolution{}
			source.ProjectHint = ""
			outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source})
			require.NoError(t, err)
			require.Len(t, outcome.Results, 1)
			assert.Equal(t, tc.want, outcome.Results[0].Result.Session.Cwd)
		})
	}
}

func TestAntigravityCLIProviderUnreadableStoreKeepsStoredWorkspace(t *testing.T) {
	tests := map[string]struct {
		history bool
		want    SourceCwdResolution
	}{
		// Only the store could name this conversation's folder, so its
		// stored working directory must survive the outage.
		"only the summary store could map it": {want: SourceCwdResolution{State: SourceCwdUnavailable}},
		"history maps it": {
			history: true,
			want:    SourceCwdResolution{State: SourceCwdResolved, Path: "/tmp/db-proj"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeAntigravityCLIProviderFixture(t, root, summaryTestCLIID)
			if !tc.history {
				require.NoError(t, os.Remove(filepath.Join(root, "history.jsonl")))
			}
			mustWrite(t, filepath.Join(root, AntigravitySummariesDBName),
				[]byte("not a sqlite database, long enough to fill a header page"))
			provider, ok := NewProvider(AgentAntigravityCLI, ProviderConfig{Roots: []string{root}, Machine: "devbox"})
			require.True(t, ok)

			discovered, err := provider.Discover(t.Context())
			require.NoError(t, err)
			conversation := filepath.Join(root, "conversations", summaryTestCLIID+".db")
			for name, sources := range map[string][]SourceRef{
				"discover": discovered, "discover each": streamAntigravitySources(t, provider),
			} {
				assert.Equal(t, tc.want, antigravitySourceAt(t, sources, conversation).CwdResolution, name)
			}
		})
	}
}

// A conversation's workspace and parent decide its stored session, so they
// are part of its fingerprint; its title reconciles on its own and is not.
func TestAntigravitySummaryFingerprint(t *testing.T) {
	for name, tc := range antigravitySummaryProviderCases() {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			tc.fixture(t, root, tc.id)
			write := func(row testjsonl.AntigravitySummary) {
				testjsonl.WriteAntigravitySummaries(t, root, map[string]testjsonl.AntigravitySummary{tc.id: row})
			}
			provider, ok := NewProvider(tc.agent, ProviderConfig{Roots: []string{root}, Machine: "devbox"})
			require.True(t, ok)
			fingerprint := func() string {
				t.Helper()
				discovered, err := provider.Discover(t.Context())
				require.NoError(t, err)
				got, err := provider.Fingerprint(t.Context(), antigravitySourceAt(t, discovered, tc.path(root)))
				require.NoError(t, err)
				return got.Hash
			}

			write(testjsonl.AntigravitySummary{Title: "First", WorkspaceURIs: `["file:///tmp/a"]`})
			base := fingerprint()
			write(testjsonl.AntigravitySummary{Title: "Renamed", WorkspaceURIs: `["file:///tmp/a"]`})
			assert.Equal(t, base, fingerprint(), "a title change leaves the fingerprint")
			write(testjsonl.AntigravitySummary{Title: "Renamed", WorkspaceURIs: `["file:///tmp/b"]`})
			moved := fingerprint()
			assert.NotEqual(t, base, moved, "a workspace change reparses")
			write(testjsonl.AntigravitySummary{Title: "Renamed", WorkspaceURIs: `["file:///tmp/b"]`, ParentID: summaryTestParent})
			assert.NotEqual(t, moved, fingerprint(), "a new parent reparses")
		})
	}
}

// An unreadable store marks the fingerprint, so the conversation reparses
// once the store reads again.
func TestAntigravitySummaryFingerprintChangesWhenStoreRecovers(t *testing.T) {
	for name, tc := range antigravitySummaryProviderCases() {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			tc.fixture(t, root, tc.id)
			store := testjsonl.WriteAntigravitySummaries(t, root, map[string]testjsonl.AntigravitySummary{
				tc.id: {WorkspaceURIs: `["file:///tmp/ws-proj"]`},
			})
			provider, ok := NewProvider(tc.agent, ProviderConfig{Roots: []string{root}, Machine: "devbox"})
			require.True(t, ok)
			fingerprint := func() string {
				t.Helper()
				discovered, err := provider.Discover(t.Context())
				require.NoError(t, err)
				got, err := provider.Fingerprint(t.Context(), antigravitySourceAt(t, discovered, tc.path(root)))
				require.NoError(t, err)
				return got.Hash
			}
			good := fingerprint()
			data, err := os.ReadFile(store)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(store, []byte("not a sqlite database, long enough to fill a header page"), 0o644))
			assert.NotEqual(t, good, fingerprint(), "an unreadable store changes the fingerprint")
			require.NoError(t, os.WriteFile(store, data, 0o644))
			assert.Equal(t, good, fingerprint(), "a recovered store restores it")
		})
	}
}

type antigravitySummaryProviderCase struct {
	agent   AgentType
	id      string
	fixture func(t *testing.T, root, id string)
	path    func(root string) string
}

func antigravitySummaryProviderCases() map[string]antigravitySummaryProviderCase {
	return map[string]antigravitySummaryProviderCase{
		"app and IDE": {
			agent: AgentAntigravity, id: summaryTestID, fixture: writeAntigravityIDEProviderFixture,
			path: func(root string) string { return filepath.Join(root, "conversations", summaryTestID+".db") },
		},
		"CLI": {
			agent: AgentAntigravityCLI, id: summaryTestCLIID, fixture: writeAntigravityCLIProviderFixture,
			path: func(root string) string { return filepath.Join(root, "conversations", summaryTestCLIID+".db") },
		},
	}
}

func streamAntigravitySources(t *testing.T, provider Provider) []SourceRef {
	t.Helper()
	var sources []SourceRef
	require.NoError(t, provider.(StreamingDiscoverer).DiscoverEach(t.Context(), func(source SourceRef) error {
		sources = append(sources, source)
		return nil
	}))
	return sources
}

func antigravitySourceAt(t *testing.T, sources []SourceRef, path string) SourceRef {
	t.Helper()
	for _, source := range sources {
		if source.DisplayPath == path {
			return source
		}
	}
	require.Failf(t, "source not found", "no source at %s", path)
	return SourceRef{}
}

// fileURI builds the file URI Antigravity records for a local folder. Its path
// starts with "/", before a Windows drive letter too.
func fileURI(path string) string {
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	if !strings.HasPrefix(u.Path, "/") {
		u.Path = "/" + u.Path
	}
	return u.String()
}

// A read that fails while another process holds the store stands only for
// the rest of a sync pass: later reads in the pass reuse the failure instead
// of waiting on the lock again, and a read after the failure window succeeds
// once the lock is released, although the store's file did not change.
func TestAntigravitySummariesRetryAfterTransientLock(t *testing.T) {
	root := t.TempDir()
	store := testjsonl.WriteAntigravitySummaries(t, root, map[string]testjsonl.AntigravitySummary{
		summaryTestID: {WorkspaceURIs: `["file:///tmp/ws-proj"]`},
	})
	locker, err := sql.Open("sqlite3", store)
	require.NoError(t, err)
	t.Cleanup(func() { _ = locker.Close() })
	conn, err := locker.Conn(t.Context())
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), "BEGIN EXCLUSIVE")
	require.NoError(t, err)

	now := time.Now()
	clock := now
	prev := antigravitySummariesNow
	antigravitySummariesNow = func() time.Time { return clock }
	t.Cleanup(func() { antigravitySummariesNow = prev })

	locked := loadAntigravitySummaries(t.Context(), root)
	require.Error(t, locked.err, "an exclusively locked store cannot be read")

	_, err = conn.ExecContext(t.Context(), "ROLLBACK")
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	require.Error(t, loadAntigravitySummaries(t.Context(), root).err, "the same pass reuses the failure")

	clock = now.Add(antigravitySummariesFailureRetry + time.Second)
	recovered := loadAntigravitySummaries(t.Context(), root)
	require.NoError(t, recovered.err)
	assert.Equal(t, "/tmp/ws-proj", recovered.byID[summaryTestID].workspace)
}

// A store that is unreadable or gone when a source is parsed, though it was
// readable when the source was fingerprinted, leaves the parse without the
// parent the fingerprint covered. The session is still published, but
// without the source hash, so the next sync reparses it once the store is
// back. A store unreadable at both points keeps the hash, so it does not
// reparse every sync.
func TestAntigravityStoreOutageAfterFingerprint(t *testing.T) {
	providers := map[string]struct {
		agent AgentType
		id    string
		write func(t *testing.T, root, id string)
	}{
		"app and IDE": {AgentAntigravity, summaryTestID, writeAntigravityIDEProviderFixture},
		"CLI":         {AgentAntigravityCLI, summaryTestCLIID, writeAntigravityCLIProviderFixture},
	}
	outages := map[string]func(t *testing.T, store string){
		"unreadable": func(t *testing.T, store string) {
			t.Helper()
			mustWrite(t, store, []byte("not a sqlite database, long enough to fill a header page"))
		},
		"removed": func(t *testing.T, store string) {
			t.Helper()
			require.NoError(t, os.Remove(store))
		},
	}
	for providerName, tc := range providers {
		for outageName, outage := range outages {
			t.Run(providerName+"/"+outageName, func(t *testing.T) {
				root := t.TempDir()
				tc.write(t, root, tc.id)
				store := testjsonl.WriteAntigravitySummaries(t, root, map[string]testjsonl.AntigravitySummary{
					tc.id: {WorkspaceURIs: `["file:///tmp/ws-proj"]`, ParentID: summaryTestParent},
				})
				provider, ok := NewProvider(tc.agent, ProviderConfig{Roots: []string{root}, Machine: "devbox"})
				require.True(t, ok)
				discovered, err := provider.Discover(t.Context())
				require.NoError(t, err)
				source := antigravitySourceAt(t, discovered, filepath.Join(root, "conversations", tc.id+".db"))
				readable, err := provider.Fingerprint(t.Context(), source)
				require.NoError(t, err)

				outage(t, store)
				outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source, Fingerprint: readable})
				require.NoError(t, err)
				require.Len(t, outcome.Results, 1)
				assert.Empty(t, outcome.Results[0].Result.Session.ParentSessionID)
				assert.Empty(t, outcome.Results[0].Result.Session.File.Hash)

				during, err := provider.Fingerprint(t.Context(), source)
				require.NoError(t, err)
				assert.NotEqual(t, readable.Hash, during.Hash)
				outcome, err = provider.Parse(t.Context(), ParseRequest{Source: source, Fingerprint: during})
				require.NoError(t, err)
				require.Len(t, outcome.Results, 1)
				assert.Equal(t, during.Hash, outcome.Results[0].Result.Session.File.Hash)
			})
		}
	}
}
