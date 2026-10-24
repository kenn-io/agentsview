package main

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/remotecred"
)

// The command boundary owns endpoint, filter and credential handoff. Lower-layer
// report tests cover aggregation and formatting against seeded archives.
func TestRemoteArchiveCommandsReadOnlyServer(t *testing.T) {
	localDir := t.TempDir()
	t.Setenv("HOME", localDir)
	t.Setenv("AGENTSVIEW_DATA_DIR", localDir)
	t.Setenv("TZ", "UTC")
	today := time.Now().UTC().Format(time.DateOnly)
	for _, tc := range []struct {
		name, path, query, body string
		args                    []string
		probe                   bool
	}{
		{"daily", "/api/v1/usage/summary/stream", "from=2026-06-01&to=2026-06-02&timezone=UTC&agent=codex&no_default_range=true&breakdowns=false&session_counts=true&include_one_shot=true&include_automated=true", "event: done\ndata: {\"daily\":[],\"totals\":{}}\n\n", []string{"usage", "daily", "--format", "json", "--since", "2026-06-01", "--until", "2026-06-02", "--timezone", "UTC", "--agent", "codex", "--offline", "--no-sync"}, false},
		{"statusline", "/api/v1/usage/summary", "from=" + today + "&to=" + today + "&timezone=UTC&agent=claude&no_default_range=true&breakdowns=false&session_counts=false&include_one_shot=true&include_automated=true", `{"daily":[],"totals":{}}`, []string{"usage", "statusline", "--format", "json", "--agent", "claude", "--offline", "--no-sync"}, false},
		{"activity", "/api/v1/activity/report", "date=2026-06-01&timezone=UTC&agent=codex&project=demo&preset=day&bucket=1h", `{}`, []string{"activity", "report", "--format", "json", "--date", "2026-06-01", "--timezone", "UTC", "--agent", "codex", "--project", "demo", "--preset", "day", "--bucket", "1h", "--offline", "--no-sync"}, false},
		{"projects", "/api/v1/projects", "include_one_shot=true&include_automated=true", `{"projects":[]}`, []string{"projects", "--format", "json"}, false},
		{"health", "/api/v1/sessions", "limit=7&include_one_shot=true&include_automated=true", `{"sessions":[]}`, []string{"health", "--format", "json", "--limit", "7"}, false},
		{"stats", "/api/v1/session-stats", "since=2026-06-01&until=2026-06-02&agent=codex&timezone=UTC&include_project=demo&include_one_shot=false&include_automated=false&include_git_outcomes=false&include_github_outcomes=false", `{}`, []string{"stats", "--format", "json", "--since", "2026-06-01", "--until", "2026-06-02", "--agent", "codex", "--timezone", "UTC", "--include-project", "demo"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantQuery, err := url.ParseQuery(tc.query)
			require.NoError(t, err)
			var paths []string
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				assert.Equal(t, http.MethodGet, r.Method)
				assert.Equal(t, "Bearer hub-token", r.Header.Get("Authorization"))
				w.Header().Set("Content-Type", "application/json")
				if tc.probe && r.URL.Path == "/api/v1/version" {
					assert.Empty(t, r.URL.RawQuery)
					fmt.Fprint(w, `{"api_version":4,"read_only":true,"session_stats_available":true}`)
					return
				}
				assert.Equal(t, tc.path, r.URL.Path)
				assert.Equal(t, wantQuery, r.URL.Query())
				if tc.name == "daily" {
					w.Header().Set("Content-Type", "text/event-stream")
				}
				fmt.Fprint(w, tc.body)
			}))
			defer ts.Close()
			tokenPath := filepath.Join(localDir, "hub-token")
			require.NoError(t, os.WriteFile(tokenPath, []byte("hub-token\n"), 0o600))
			// This misplaced local vector option is rejected by full config loading.
			require.NoError(t, os.WriteFile(filepath.Join(localDir, "config.toml"), []byte("auth_token='wrong-local-token'\n[remote]\nurl='"+ts.URL+"'\ntoken_file='"+tokenPath+"'\n[vector.embeddings]\nmax_batch_tokens=123\n"), 0o600))
			stdout, stderr, err := runScopedConfigCLI(t, nil, tc.args...)
			require.NoError(t, err, stderr)
			assert.NotEmpty(t, stdout)
			ts.Close()
			wantPaths := []string{tc.path}
			if tc.probe {
				wantPaths = append([]string{"/api/v1/version"}, wantPaths...)
			}
			assert.Equal(t, wantPaths, paths)
			assert.NoFileExists(t, filepath.Join(localDir, "sessions.db"))
		})
	}
}

func TestRemoteStatsCapabilityRefusesFallback(t *testing.T) {
	t.Setenv("AGENTSVIEW_DATA_DIR", t.TempDir())
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/version", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"api_version":4,"read_only":true,"session_stats_available":false}`))
	}))
	t.Cleanup(ts.Close)
	_, stderr, err := runScopedConfigCLI(t, []string{"AGENTSVIEW_REMOTE_URL=" + ts.URL}, "stats", "--format", "json")
	require.Error(t, err)
	assert.Contains(t, stderr, "not available from this server")
	assert.Contains(t, stderr, "--local")
}

func TestRemoteLocalOnlyNoticePreservesOutput(t *testing.T) {
	seedScopedConfigArchive(t)
	database, err := db.Open(t.Context(), filepath.Join(os.Getenv("AGENTSVIEW_DATA_DIR"), "sessions.db"))
	require.NoError(t, err)
	seedUsageDailyExportMetadataFixture(t, database, fallbackPricedModel(t))
	require.NoError(t, database.Close())
	for _, args := range [][]string{{"export", "sessions", "--format", "json"}, {"token-use", "usage-meta-fallback-cost"}} {
		t.Run(args[0], func(t *testing.T) {
			stdout, _, err := runScopedConfigCLI(t, nil, args...)
			require.NoError(t, err)
			remote, stderr, err := runScopedConfigCLI(t, []string{"AGENTSVIEW_REMOTE_URL=https://hub.example", "AGENTSVIEW_REMOTE_TOKEN_FILE=" + filepath.Join(t.TempDir(), "missing")}, args...)
			require.NoError(t, err, stderr)
			assert.Equal(t, stdout, remote)
			assert.Contains(t, stderr, "local archive")
		})
	}
}

func TestRemoteArchivePolicyIgnoresLocalGates(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	t.Setenv("AGENTSVIEW_SERVER_TOKEN", "remote-policy-token")
	t.Setenv("AGENTSVIEW_REMOTE_TOKEN_FILE", "")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/v1/usage/summary", r.URL.Path)
		assert.Equal(t, "Bearer remote-policy-token", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"daily":[],"totals":{"inputTokens":37}}`)
	}))
	t.Cleanup(ts.Close)
	t.Setenv("AGENTSVIEW_REMOTE_URL", ts.URL)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte("auth_token='wrong-local-token'\n[vector.embeddings]\nmax_batch_tokens=123\n"), 0o600))
	for _, tc := range []struct {
		name   string
		policy archiveQueryPolicy
	}{
		{"offline", archiveQueryPolicy{Offline: true}},
		{"no sync", archiveQueryPolicy{NoSync: true, AutoStart: true}},
		{"fresh", archiveQueryPolicy{AutoStart: true}},
		{"skip initial sync", archiveQueryPolicy{AutoStart: true, SkipInitialSync: true}},
		{"reject readonly daemon", archiveQueryPolicy{ReadOnlyDaemon: archiveQueryRejectReadOnlyDaemon}},
		{"skip readonly daemon", archiveQueryPolicy{ReadOnlyDaemon: archiveQuerySkipReadOnlyDaemon}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.policy.Command = targetCommand(t)
			backend, cleanup, err := resolveArchiveQueryBackend(t.Context(), tc.policy)
			require.NoError(t, err)
			defer closeArchiveQueryBackend(cleanup)
			result, err := backend.DailyUsage(t.Context(), dailyUsageQuery{})
			require.NoError(t, err)
			assert.Equal(t, 37, result.Totals.InputTokens)
			assert.NoFileExists(t, filepath.Join(dir, "sessions.db"))
		})
	}
}

func TestRemoteReadGroupHelp(t *testing.T) {
	for _, name := range []string{"usage", "activity"} {
		t.Run(name, func(t *testing.T) {
			cmd := newRootCommand()
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetArgs([]string{name})
			require.NoError(t, cmd.Execute())
			assert.Contains(t, out.String(), "Available Commands:")
		})
	}
}

func TestRemoteArchiveExplicitPG(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	t.Setenv("AGENTSVIEW_REMOTE_URL", "https://hub.example")
	t.Setenv("AGENTSVIEW_REMOTE_TOKEN_FILE", filepath.Join(dir, "missing"))
	t.Setenv("AGENTSVIEW_PG_URL", "postgres://reader@localhost/archive")
	database := seedExportSessionsArchiveAt(t, filepath.Join(t.TempDir(), "sessions.db"))
	seedUsageDailyExportMetadataFixture(t, database, fallbackPricedModel(t))
	stub := stubPGReadStore(t, database)
	cmd := targetCommand(t)
	require.NoError(t, cmd.Flags().Set("pg", "true"))
	backend, cleanup, err := resolveArchiveQueryBackend(t.Context(), archiveQueryPolicy{Command: cmd, Offline: true, AutoStart: true})
	require.NoError(t, err)
	defer closeArchiveQueryBackend(cleanup)
	result, err := backend.DailyUsage(t.Context(), dailyUsageQuery{NoDefaultRange: true})
	require.NoError(t, err)
	assert.Equal(t, 300, result.Totals.InputTokens)
	report, err := backend.ActivityReport(t.Context(), ActivityReportConfig{Date: "2026-06-01", Timezone: "UTC"})
	require.NoError(t, err)
	assert.NotEmpty(t, report.BySession)
	projects, err := readProjects(cmd)
	require.NoError(t, err)
	require.NotEmpty(t, projects)
	assert.True(t, stub.Opened)
	assert.NoFileExists(t, filepath.Join(dir, "sessions.db"))
}

type remoteDeadlineTransport func(*http.Request) (*http.Response, error)

func (f remoteDeadlineTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Capture the request deadline at the report transport boundary without waiting.
// A report selecting the ordinary 30-second client loses the 31-second deadline.
func TestRemoteReportDeadline(t *testing.T) {
	for _, duration := range []time.Duration{time.Second, 30 * time.Second, 31 * time.Second} {
		for _, consumer := range []string{"daily", "daily stream", "activity", "activity page"} {
			t.Run(consumer+"/"+duration.String(), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), duration)
				defer cancel()
				wantDeadline, _ := ctx.Deadline()
				client, err := remotecred.Client("https://hub.example", false)
				require.NoError(t, err)
				var calls int
				client.Transport = remoteDeadlineTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					deadline, ok := r.Context().Deadline()
					assert.True(t, ok)
					assert.Equal(t, wantDeadline, deadline)
					body := `{}`
					contentType := "application/json"
					switch consumer {
					case "daily":
						assert.Equal(t, "/api/v1/usage/summary", r.URL.Path)
					case "daily stream":
						assert.Equal(t, "/api/v1/usage/summary/stream", r.URL.Path)
						contentType = "text/event-stream"
						body = "event: done\ndata: {}\n\n"
					case "activity":
						assert.Equal(t, "/api/v1/activity/report", r.URL.Path)
					case "activity page":
						assert.Equal(t, "/api/v1/activity/report/generation/sessions", r.URL.Path)
						body = `{"report":{}}`
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
				})
				backend := daemonArchiveQueryBackend{tr: transport{URL: "https://hub.example", httpClient: client}}
				switch consumer {
				case "daily", "daily stream":
					query := dailyUsageQuery{}
					if consumer == "daily stream" {
						query.Progress = func(string) {}
					}
					_, err = backend.DailyUsage(ctx, query)
				case "activity":
					_, err = backend.ActivityReport(ctx, ActivityReportConfig{})
				case "activity page":
					_, err = backend.ActivityReport(ctx, ActivityReportConfig{SessionsReportID: "generation"})
				}
				require.NoError(t, err)
				assert.Equal(t, 1, calls)
			})
		}
	}
}

func TestUsageCursorRejectsRemoteFlagsBeforeWrite(t *testing.T) {
	for _, flag := range []string{"--server=https://hub.example", "--pg", "--server-token-file=missing.token"} {
		t.Run(flag, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("AGENTSVIEW_DATA_DIR", dir)
			_, stderr, err := runScopedConfigCLI(t, nil, "usage", "cursor", flag)
			require.Error(t, err)
			assert.Contains(t, stderr, "unknown flag: "+strings.Split(flag, "=")[0])
			assert.NoFileExists(t, filepath.Join(dir, "sessions.db"))
		})
	}
}

func TestUsageReadFlagsAndLocalCursor(t *testing.T) {
	for _, command := range []string{"daily", "statusline"} {
		cmd := newRootCommand()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs([]string{"usage", command, "--server=https://hub.example", "--server-token-file=missing.token", "--pg", "--help"})
		require.NoError(t, cmd.Execute())
		for _, flag := range []string{"--server ", "--server-token-file ", "--pg "} {
			assert.Contains(t, out.String(), flag)
		}
	}
	dir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	_, stderr, err := runScopedConfigCLI(t, nil, "--local", "usage", "cursor")
	require.Error(t, err)
	assert.Contains(t, stderr, "missing Cursor admin API key")
	assert.FileExists(t, filepath.Join(dir, "sessions.db"))
}

// Successful HTTP status does not make decoder diagnostics safe: duplicate
// member errors can quote private response content after the response policy.
func TestRemoteReportDecodeDiagnostics(t *testing.T) {
	for _, consumer := range []string{"projects", "daily", "daily stream", "activity", "activity stream", "activity page"} {
		for _, kind := range []readTargetKind{readTargetRemote, readTargetServer} {
			t.Run(consumer+"/"+string(kind), func(t *testing.T) {
				body := `{"private-member":1,"private-member":2}`
				ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if strings.HasSuffix(consumer, "stream") {
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprintf(w, "event: done\ndata: %s\n\n", body)
						return
					}
					fmt.Fprint(w, body)
				}))
				defer ts.Close()
				target := readTarget{Kind: kind, URL: ts.URL}
				if kind == readTargetRemote {
					var err error
					target.client, err = remotecred.Client(ts.URL, false)
					require.NoError(t, err)
				}
				tr := transport{URL: target.URL, httpClient: target.client}
				backend := daemonArchiveQueryBackend{target: target, tr: tr}
				var err error
				switch consumer {
				case "projects":
					t.Setenv("AGENTSVIEW_DATA_DIR", t.TempDir())
					cmd := targetCommand(t)
					if kind == readTargetRemote {
						t.Setenv("AGENTSVIEW_REMOTE_URL", ts.URL)
					} else {
						require.NoError(t, cmd.Flags().Set("server", ts.URL))
					}
					_, err = readProjects(cmd)
				case "daily", "daily stream":
					query := dailyUsageQuery{}
					if consumer == "daily stream" {
						query.Progress = func(string) {}
					}
					_, err = backend.DailyUsage(t.Context(), query)
				case "activity", "activity stream":
					_, err = backend.ActivityReport(t.Context(), ActivityReportConfig{})
				case "activity page":
					_, err = backend.ActivityReport(t.Context(), ActivityReportConfig{SessionsReportID: "generation"})
				}
				require.Error(t, err)
				var syntax *jsontext.SyntacticError
				assert.ErrorAs(t, err, &syntax, "preserve the decoder cause")
				if kind == readTargetRemote {
					assert.NotContains(t, err.Error(), "private-member")
					assert.Contains(t, err.Error(), "--local")
				} else {
					assert.Contains(t, err.Error(), "private-member", "preserve legacy explicit-server diagnostics")
				}
			})
		}
	}
}
