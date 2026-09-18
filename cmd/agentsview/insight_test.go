package main

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/insight"
	"go.kenn.io/agentsview/internal/server"
)

func runInsightCommand(
	t *testing.T, args ...string,
) (string, string, error) {
	t.Helper()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	root := newRootCommand()
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs(args)
	_, err := root.ExecuteC()
	return stdout.String(), stderr.String(), err
}

func testInsight(id int64) db.Insight {
	project := "agentsview"
	model := "test-model"
	prompt := "focus"
	return db.Insight{
		ID:        id,
		Type:      "daily_activity",
		DateFrom:  "2026-09-15",
		DateTo:    "2026-09-15",
		Project:   &project,
		Agent:     "claude",
		Model:     &model,
		Prompt:    &prompt,
		Content:   "# Saved insight\n\nActivity summary.",
		CreatedAt: "2026-09-16T12:00:00Z",
	}
}

func writeInsightJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	w.Header().Set("Content-Type", "application/json")
	_, err = w.Write(data)
	require.NoError(t, err)
}

func insightSSEEvent(t *testing.T, event string, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	return "event: " + event + "\ndata: " + string(data) + "\n\n"
}

func TestNewInsightCommand_RegistersSubcommands(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := newRootCommand()
	insight, _, err := root.Find([]string{"insight"})
	require.NoError(err)
	require.NotNil(insight)
	assert.Equal("insight", insight.Name())

	for _, name := range []string{"list", "get", "generate"} {
		child, _, err := root.Find([]string{"insight", name})
		require.NoError(err)
		assert.Equal(name, child.Name())
	}
	for _, name := range []string{
		"format", "json", "server", "server-token-file",
	} {
		assert.NotNil(insight.PersistentFlags().Lookup(name), name)
	}
}

func TestInsightListCommand_ForwardsFiltersAndFormats(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	first := testInsight(7)
	second := testInsight(8)
	var requestCount int
	ts := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		requestCount++
		assert.Equal(http.MethodGet, r.Method)
		assert.Equal("/api/v1/insights", r.URL.Path)
		assert.Equal("Bearer file-token", r.Header.Get("Authorization"))
		if r.URL.Query().Get("type") == "empty" {
			writeInsightJSON(t, w, insightListResponse{Insights: []db.Insight{}})
			return
		}
		query := r.URL.Query()
		if query.Get("type") == "" {
			writeInsightJSON(t, w, insightListResponse{
				Insights: []db.Insight{first, second},
			})
			return
		}
		assert.Equal("daily_activity", query.Get("type"))
		assert.Equal("project with space", query.Get("project"))
		assert.Equal("2026-09-01", query.Get("date_from"))
		assert.Equal("2026-09-15", query.Get("date_to"))
		assert.Contains(r.URL.RawQuery, "project=project+with+space")
		writeInsightJSON(t, w, insightListResponse{
			Insights: []db.Insight{first, second},
		})
	}))
	t.Cleanup(ts.Close)
	tokenPath := filepath.Join(t.TempDir(), "token")
	require.NoError(os.WriteFile(tokenPath, []byte("file-token\n"), 0o600))

	stdout, stderr, err := runInsightCommand(t,
		"insight", "list", "--server", ts.URL,
		"--server-token-file", tokenPath, "--format", "json",
		"--type", "daily_activity", "--project", "project with space",
		"--date-from", "2026-09-01", "--date-to", "2026-09-15",
	)
	require.NoError(err)
	assert.Empty(stderr)
	var got insightListResponse
	require.NoError(json.Unmarshal([]byte(stdout), &got))
	assert.Equal([]db.Insight{first, second}, got.Insights)

	stdout, stderr, err = runInsightCommand(t,
		"insight", "list", "--server", ts.URL,
		"--server-token-file", tokenPath,
	)
	require.NoError(err)
	assert.Empty(stderr)
	assert.Contains(stdout, "ID")
	assert.Contains(stdout, "7")
	assert.Contains(stdout, "8")

	stdout, stderr, err = runInsightCommand(t,
		"insight", "list", "--server", ts.URL,
		"--server-token-file", tokenPath, "--type", "empty",
	)
	require.NoError(err)
	assert.Empty(stderr)
	assert.Equal("(no insights)\n", stdout)

	stdout, _, err = runInsightCommand(t,
		"insight", "list", "--server", ts.URL,
		"--server-token-file", tokenPath, "--json", "--type", "empty",
	)
	require.NoError(err)
	var empty insightListResponse
	require.NoError(json.Unmarshal([]byte(stdout), &empty))
	assert.NotNil(empty.Insights)
	assert.Empty(empty.Insights)
	assert.Equal(4, requestCount)
}

func TestInsightGetCommand_FoundAndMissing(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	want := testInsight(42)
	ts := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		assert.Equal(http.MethodGet, r.Method)
		switch r.URL.Path {
		case "/api/v1/insights/42":
			writeInsightJSON(t, w, want)
		case "/api/v1/insights/404":
			w.WriteHeader(http.StatusNotFound)
			writeInsightJSON(t, w, map[string]string{
				"error": "insight not found",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)

	stdout, stderr, err := runInsightCommand(t,
		"insight", "get", "42", "--server", ts.URL, "--json",
	)
	require.NoError(err)
	assert.Empty(stderr)
	var got db.Insight
	require.NoError(json.Unmarshal([]byte(stdout), &got))
	assert.Equal(want, got)

	stdout, _, err = runInsightCommand(t,
		"insight", "get", "not-an-id", "--server", ts.URL,
	)
	require.Error(err)
	assert.Contains(err.Error(), "invalid insight ID")
	assert.Empty(stdout)

	stdout, _, err = runInsightCommand(t,
		"insight", "get", "404", "--server", ts.URL,
	)
	require.Error(err)
	assert.Equal("insight 404 not found", err.Error())
	assert.Empty(stdout)
}

func TestInsightGenerateCommand_StreamsAndSaves(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	want := testInsight(99)
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		assert.Equal(http.MethodPost, r.Method)
		assert.Equal("/prefix/path/api/v1/insights/generate", r.URL.Path)
		assert.Empty(r.URL.RawQuery)
		assert.Equal("application/json", r.Header.Get("Content-Type"))
		assert.Equal("text/event-stream", r.Header.Get("Accept"))
		assert.Equal(ts.URL, r.Header.Get("Origin"))
		var body map[string]jsontext.Value
		require.NoError(json.UnmarshalRead(r.Body, &body))
		assert.Len(body, 9)
		for key, value := range map[string]string{
			"type":            "agent_analysis",
			"date_from":       "2026-09-01",
			"date_to":         "2026-09-15",
			"project":         "agentsview",
			"prompt":          "find regressions",
			"session_id":      "session-1",
			"agent":           "codex",
			"automated_scope": "all",
			"timezone":        "America/New_York",
		} {
			var got string
			require.NoError(json.Unmarshal(body[key], &got), key)
			assert.Equal(value, got, key)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, err := io.WriteString(w,
			insightSSEEvent(t, "status", map[string]string{
				"phase": "generating",
			})+
				insightSSEEvent(t, "log", map[string]string{
					"stream": "stdout", "line": "agent started",
				})+
				insightSSEEvent(t, "log", map[string]string{
					"stream": "stderr", "line": "agent finished",
				})+
				insightSSEEvent(t, "done", want),
		)
		require.NoError(err)
	}))
	t.Cleanup(ts.Close)
	explicitServerURL := ts.URL + "/prefix/path/?ignored=yes#fragment"

	stdout, stderr, err := runInsightCommand(t,
		"insight", "generate", "--server", explicitServerURL, "--json",
		"--type", "agent_analysis", "--date-from", "2026-09-01",
		"--date-to", "2026-09-15", "--project", "agentsview",
		"--prompt", "find regressions", "--session-id", "session-1",
		"--agent", "codex", "--automated-scope", "all",
		"--timezone", "America/New_York",
	)
	require.NoError(err)
	var got db.Insight
	require.NoError(json.Unmarshal([]byte(stdout), &got))
	assert.Equal(want, got)
	assert.Contains(stderr, "generating")
	assert.Contains(stderr, "agent started")
	assert.Contains(stderr, "agent finished")
	assert.NotContains(stdout, "agent started")
}

func TestInsightGenerateCommand_ErrorEvent(t *testing.T) {
	assert := assert.New(t)

	ts := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		assert.Equal(http.MethodPost, r.Method)
		w.Header().Set("Content-Type", "text/event-stream")
		_, err := io.WriteString(w,
			insightSSEEvent(t, "status", map[string]string{
				"phase": "generating",
			})+
				insightSSEEvent(t, "error", map[string]string{
					"message": "agent returned empty content",
				}),
		)
		require.NoError(t, err)
	}))
	t.Cleanup(ts.Close)

	stdout, stderr, err := runInsightCommand(t,
		"insight", "generate", "--server", ts.URL, "--json",
	)
	require.Error(t, err)
	assert.Contains(err.Error(), "agent returned empty content")
	assert.Empty(stdout)
	assert.Contains(stderr, "generating")
}

type insightReadError struct{ err error }

func (r insightReadError) Read([]byte) (int, error) { return 0, r.err }

func TestConsumeInsightGenerateSSE_Boundaries(t *testing.T) {
	t.Run("framing and large final frame", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		want := testInsight(123)
		want.Content = strings.Repeat("x", 70*1024)
		done, err := json.Marshal(want)
		require.NoError(err)
		stream := ": ignored\r\n" +
			"event: progress\r\n" +
			"data: malformed unknown payload\r\n\r\n" +
			"event: status\r\n" +
			"data: {\"phase\":\r\n" +
			"data: \"generating\"}\r\n\r\n" +
			"event: done\r\n" +
			"data: " + string(done)
		var progress bytes.Buffer
		got, err := consumeInsightGenerateSSE(
			strings.NewReader(stream), &progress,
		)
		require.NoError(err)
		require.NotNil(got)
		assert.Equal(want, *got)
		assert.Equal("generating\n", progress.String())
	})

	tests := []struct {
		name string
		body io.Reader
		want string
	}{
		{
			name: "malformed recognized event",
			body: strings.NewReader("event: status\ndata: {bad}\n\n"),
			want: "decoding insight status event",
		},
		{
			name: "malformed done event",
			body: strings.NewReader("event: done\ndata: {bad}\n\n"),
			want: "decoding insight done event",
		},
		{
			name: "nonpositive done ID",
			body: strings.NewReader(
				"event: done\ndata: {\"id\":0}\n\n",
			),
			want: "missing saved insight",
		},
		{
			name: "recognized event without data",
			body: strings.NewReader("event: status\n\n"),
			want: "insight status event has no data",
		},
		{
			name: "read error",
			body: insightReadError{err: io.ErrUnexpectedEOF},
			want: "reading insight generation stream",
		},
		{
			name: "cancellation",
			body: insightReadError{err: context.Canceled},
			want: "context canceled",
		},
		{
			name: "missing done",
			body: strings.NewReader(
				"event: status\ndata: {\"phase\":\"generating\"}\n\n",
			),
			want: "missing done event",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := consumeInsightGenerateSSE(tc.body, io.Discard)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestInsightGenerateCommand_RejectsCompletionWithoutOutput(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{
			name: "malformed done",
			body: "event: done\ndata: {bad}\n\n",
		},
		{
			name: "nonpositive done ID",
			body: "event: done\ndata: {\"id\":0}\n\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(
				w http.ResponseWriter, r *http.Request,
			) {
				assert.Equal(t, http.MethodPost, r.Method)
				w.Header().Set("Content-Type", "text/event-stream")
				_, err := io.WriteString(w, tc.body)
				require.NoError(t, err)
			}))
			t.Cleanup(ts.Close)

			stdout, _, err := runInsightCommand(t,
				"insight", "generate", "--server", ts.URL, "--json",
			)
			require.Error(t, err)
			assert.Empty(t, stdout)
		})
	}
}

func TestInsightTransportModes(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		name := "sqlite"
		if readOnly {
			name = "postgres-capable-readonly-runtime"
		}
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			dataDir := t.TempDir()
			t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
			var listRequests, generateRequests int
			ts := daemonRouteTestServer(t, map[string]http.HandlerFunc{
				"/api/v1/insights": func(
					w http.ResponseWriter, r *http.Request,
				) {
					listRequests++
					writeInsightJSON(t, w, insightListResponse{
						Insights: []db.Insight{},
					})
				},
				"/api/v1/insights/generate": func(
					w http.ResponseWriter, r *http.Request,
				) {
					generateRequests++
					w.Header().Set("Content-Type", "text/event-stream")
					_, err := io.WriteString(w, insightSSEEvent(
						t, "done", testInsight(5),
					))
					require.NoError(err)
				},
			})
			registerTestRuntime(t, dataDir, ts.URL, readOnly)

			stdout, _, err := runInsightCommand(t, "insight", "list", "--json")
			require.NoError(err)
			assert.Contains(stdout, "insights")
			stdout, _, err = runInsightCommand(t,
				"insight", "generate", "--json",
			)
			require.NoError(err)
			assert.Contains(stdout, "\"id\":5")
			assert.Equal(1, listRequests)
			assert.Equal(1, generateRequests)
		})
	}

	t.Run("generation capability error stays server-owned", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(
			w http.ResponseWriter, r *http.Request,
		) {
			assert.Equal(t, "/api/v1/insights/generate", r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
			writeInsightJSON(t, w, map[string]string{
				"error": "insight generation is not available for this archive",
			})
		}))
		t.Cleanup(ts.Close)
		stdout, _, err := runInsightCommand(t,
			"insight", "generate", "--server", ts.URL, "--json",
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "insight generation is not available")
		assert.Empty(t, stdout)
	})

	t.Run("disabled autostart preserves local error", func(t *testing.T) {
		t.Setenv("AGENTSVIEW_DATA_DIR", t.TempDir())
		t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
		stdout, _, err := runInsightCommand(t, "insight", "list", "--json")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "daemon autostart is disabled")
		assert.Empty(t, stdout)
	})
}

func TestInsightCommandsDiscoverBasePathDaemon(t *testing.T) {
	for _, token := range []string{"", "test-token"} {
		t.Run(token, func(t *testing.T) {
			require := require.New(t)

			dataDir := t.TempDir()
			t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
			t.Setenv("AGENTSVIEW_AUTH_TOKEN", token)
			t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
			database := dbtest.OpenTestDB(t)
			id, err := database.InsertInsight(testInsight(0))
			require.NoError(err)
			ts := httptest.NewUnstartedServer(nil)
			host, port := splitTestServerURL(t, "http://"+ts.Listener.Addr().String())
			srv := server.New(config.Config{
				Host: host, Port: port, DataDir: dataDir,
				AuthToken: token, RequireAuth: token != "",
			}, database, nil, server.WithBasePath("/viewer"),
				server.WithGenerateFunc(func(context.Context, string, string) (insight.Result, error) {
					return insight.Result{Agent: "claude", Content: "Activity summary."}, nil
				}),
			)
			ts.Config.Handler = srv.Handler()
			ts.Start()
			t.Cleanup(ts.Close)
			_, err = WriteDaemonRuntimeWithAuth(
				dataDir, host, port, "test", ts.URL+"/viewer", true, token != "",
			)
			require.NoError(err)

			for _, args := range [][]string{
				{"insight", "list", "--json"},
				{"insight", "get", strconv.FormatInt(id, 10), "--json"},
				{"insight", "generate", "--date-from", "2026-09-15", "--date-to", "2026-09-15", "--json"},
			} {
				stdout, _, err := runInsightCommand(t, args...)
				require.NoError(err)
				assert.Contains(t, stdout, "Activity summary.")
			}
		})
	}
}

func TestInsightCredentials(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var seen []string
	ts := daemonRouteTestServer(t, map[string]http.HandlerFunc{
		"/api/v1/insights": func(w http.ResponseWriter, r *http.Request) {
			seen = append(seen, r.Header.Get("Authorization"))
			writeInsightJSON(t, w, insightListResponse{
				Insights: []db.Insight{},
			})
		},
	})
	t.Setenv("AGENTSVIEW_AUTH_TOKEN", "local-token")
	t.Setenv("AGENTSVIEW_SERVER_TOKEN", "environment-token")
	tokenPath := filepath.Join(t.TempDir(), "server-token")
	require.NoError(os.WriteFile(
		tokenPath, []byte("file-token\n"), 0o600,
	))

	stdout, stderr, err := runInsightCommand(t,
		"insight", "list", "--server", ts.URL,
		"--server-token-file", tokenPath, "--json",
	)
	require.NoError(err)
	assert.Contains(stdout, `"insights"`)
	assert.Empty(stderr)
	assert.Equal([]string{"Bearer file-token"}, seen)

	_, _, err = runInsightCommand(t,
		"insight", "list", "--server", ts.URL, "--json",
	)
	require.NoError(err)
	assert.Equal("Bearer environment-token", seen[1])

	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	registerTestRuntime(t, dataDir, ts.URL, false)
	stdout, stderr, err = runInsightCommand(t, "insight", "list", "--json")
	require.NoError(err)
	assert.Equal("Bearer local-token", seen[2])
	for _, value := range []string{
		"local-token", "environment-token", "file-token",
	} {
		assert.NotContains(stdout, value)
		assert.NotContains(stderr, value)
	}
}

func TestInsightHTTPErrorPreservesServerMessage(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter, _ *http.Request,
	) {
		w.WriteHeader(http.StatusForbidden)
		writeInsightJSON(t, w, map[string]string{"error": "access denied"})
	}))
	t.Cleanup(ts.Close)
	_, _, err := runInsightCommand(t,
		"insight", "list", "--server", ts.URL,
	)
	require.Error(t, err)
	assert.Equal(t, "access denied", err.Error())
}

func TestInsightContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := doInsightRequest(
		ctx, transport{Mode: transportHTTP, URL: "http://127.0.0.1:1"},
		"", http.MethodGet, "/api/v1/insights", nil, nil,
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestInsightHumanOutputSanitizesStoredFields(t *testing.T) {
	assert := assert.New(t)

	value := testInsight(17)
	value.Project = new("project\x1b[31m")
	value.Content = "safe\r\x1b[2Jcontent"
	var out bytes.Buffer
	require.NoError(t, printInsightHuman(&out, &value))
	assert.NotContains(out.String(), "\x1b")
	assert.NotContains(out.String(), "\r")
	assert.Contains(out.String(), "project[31m")
	assert.Contains(out.String(), "safe[2Jcontent")
}

func TestInsightHumanOutputOmitsNilAndEmptyModelPrompt(t *testing.T) {
	empty := ""
	for _, tc := range []struct {
		name   string
		model  *string
		prompt *string
	}{
		{name: "nil", model: nil, prompt: nil},
		{name: "empty", model: &empty, prompt: &empty},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)

			value := testInsight(18)
			value.Model = tc.model
			value.Prompt = tc.prompt
			var out bytes.Buffer
			require.NoError(t, printInsightHuman(&out, &value))
			assert.NotContains(out.String(), "Model:")
			assert.NotContains(out.String(), "Prompt:")
			assert.Contains(out.String(), value.Content)
		})
	}
}

func TestInsightGenerateRequestJSONTags(t *testing.T) {
	data, err := json.Marshal(insightGenerateRequest{
		Type: "daily_activity", DateFrom: "2026-09-15", DateTo: "2026-09-15",
	})
	require.NoError(t, err)
	assert.JSONEq(t,
		`{"type":"daily_activity","date_from":"2026-09-15","date_to":"2026-09-15"}`,
		string(data),
	)
}

func TestInsightCommandHelpListsScope(t *testing.T) {
	assert := assert.New(t)

	cmd := newInsightCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"generate", "--help"})
	_, err := cmd.ExecuteC()
	require.NoError(t, err)
	for _, flag := range []string{
		"--type", "--date-from", "--date-to", "--project", "--prompt",
		"--session-id", "--agent", "--automated-scope", "--timezone",
	} {
		assert.Contains(out.String(), flag)
	}
	assert.NotContains(out.String(), "--kind")
	assert.NotContains(out.String(), "--force-refresh")
	assert.NotContains(out.String(), "--llm-opt-in")
}
