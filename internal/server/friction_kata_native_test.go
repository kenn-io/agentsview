package server_test

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/friction/filing"
	"go.kenn.io/agentsview/internal/friction/review"
	"go.kenn.io/agentsview/internal/kata"
	"go.kenn.io/agentsview/internal/server"
	kg "go.kenn.io/kata/pkg/client/generated"
)

// The transport seam only simulates an outage. Successful responses and
// authentication come from the actual pinned Kata service and scratch store.
func TestFrictionAgainstNativeKataHub(t *testing.T) {
	const token = "synthetic-kata-token"
	nativeHub := startPinnedKataHub(t, token)
	upstream, err := url.Parse(nativeHub.Endpoint)
	require.NoError(t, err)
	var unavailable atomic.Bool
	var requests atomic.Int64
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	kataServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if unavailable.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(kataServer.Close)
	t.Setenv("AGENTSVIEW_TEST_KATA_TOKEN", token)
	cfg := config.KataConfig{Enabled: true, Endpoint: kataServer.URL, TokenEnv: "AGENTSVIEW_TEST_KATA_TOKEN", Project: "agentsview", Actor: "agentsview"}
	clientCfg := kata.ConfigFrom(cfg, true)
	clientCfg.HTTPClient = kataServer.Client() // trust only this fixture's TLS certificate
	conn := kata.NewConn(clientCfg)
	f := &filing.Filer{Kata: conn, Location: time.UTC, PublicURL: "https://archive.example.test", Instance: "synthetic-instance", Policy: filing.Policy{Kinds: filing.DefaultKinds(), ReopenOnRecurrence: true}}
	te := setupWithServerOpts(t, []server.Option{server.WithKataConn(conn), server.WithFrictionFiler(f)}, func(c *config.Config) {
		c.Kata = cfg
		c.RequireAuth, c.AuthToken = true, "synthetic-archive-token"
	})
	f.Store, f.Archive = te.db, te.db
	runner := &review.Runner{Store: te.db, Loc: time.UTC, Filer: f, AutoFile: true, PublicURL: f.PublicURL}
	f.Snapshot, f.Rerender = runner.Snapshot, runner.Rerender
	request := func(method, path, body, credential string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if credential != "" {
			req.Header.Set("Authorization", "Bearer "+credential)
		}
		w := httptest.NewRecorder()
		te.srv.Handler().ServeHTTP(w, req)
		return w
	}
	readKata := func(path string) []byte {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, kataServer.URL+path, nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := kataServer.Client().Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode, "%s", body)
		return body
	}
	show := func(uid string) kg.ShowIssueResponseBody {
		var response kg.ShowIssueResponseBody
		require.NoError(t, json.Unmarshal(readKata("/api/v1/issues/"+uid), &response))
		return response
	}
	seed := func(id, date, text string) friction.Signal {
		dbtest.SeedSession(t, te.db, id, "example", func(s *db.Session) {
			s.StartedAt, s.EndedAt = new(date+"T10:00:00Z"), new(date+"T11:00:00Z")
		})
		dbtest.SeedMessages(t, te.db,
			db.Message{SessionID: id, Ordinal: 0, Role: "assistant", Content: "I changed the wrong configuration."},
			db.Message{SessionID: id, Ordinal: 1, Role: "user", Content: text},
		)
		sig := friction.Signal{Kind: friction.KindCorrection, SubjectKind: friction.SubjectSession, SubjectID: id, Text: text, Ordinal: new(1)}
		require.NoError(t, te.db.ReplaceSessionFriction(t.Context(), id, []db.FrictionFinding{{SessionID: id, Kind: "correction", Text: text, MessageOrdinal: sig.Ordinal, Fingerprint: sig.Fingerprint()}}, nil, friction.RulesVersion, "synthetic-"+id))
		return sig
	}
	current := time.Now().UTC().Format(time.DateOnly)
	today, err := time.Parse(time.DateOnly, current)
	require.NoError(t, err)
	recurredDate := today.AddDate(0, 0, 1).Format(time.DateOnly)
	wontfixDate := today.AddDate(0, 0, 2).Format(time.DateOnly)
	sig := seed("claude:today", current, "No, edit the configuration first.")
	path := "/api/v1/friction/patterns/" + sig.Fingerprint() + "/file"
	unauthorized := request(http.MethodPost, path, `{}`, "")
	assert.Equal(t, http.StatusUnauthorized, unauthorized.Code)
	status := request(http.MethodGet, "/api/v1/kata/status", "", "synthetic-archive-token")
	require.Equal(t, http.StatusOK, status.Code, "%s", status.Body.String())
	assert.Contains(t, status.Body.String(), `"state":"ready"`)
	preview := request(http.MethodPost, path, `{"dry_run":true}`, "synthetic-archive-token")
	require.Equal(t, http.StatusOK, preview.Code, "%s", preview.Body.String())
	assert.Contains(t, preview.Body.String(), "I changed the wrong configuration.")
	filed := request(http.MethodPost, path, `{}`, "synthetic-archive-token")
	require.Equal(t, http.StatusOK, filed.Code, "%s", filed.Body.String())
	var result server.FrictionFileResponse
	require.NoError(t, json.Unmarshal(filed.Body.Bytes(), &result))
	require.NotNil(t, result.Link)
	require.Equal(t, "linked", result.Link.State, "%s", filed.Body.String())
	uid := result.Link.IssueUID
	issue := show(uid)
	assert.Contains(t, issue.Issue.Body, "I changed the wrong configuration.")
	assert.Contains(t, issue.Issue.Body, "Message 1 (user)")
	assert.Contains(t, issue.Issue.Body, "https://archive.example.test/sessions/claude/today?msg=1")
	assert.NotContains(t, issue.Issue.Body, "- Digest:")
	assert.Equal(t, "claude:today", issue.Issue.Metadata["agentsview.session_id"])
	assert.Equal(t, sig.Fingerprint(), issue.Issue.Metadata["friction.fingerprint"])
	assert.Equal(t, "https://archive.example.test/sessions/claude/today?msg=1", issue.Issue.Metadata["agentsview.session_url"])
	assertStatus(t, request(http.MethodPost, path, `{}`, "synthetic-archive-token"), http.StatusOK)
	require.NoError(t, f.Unlink(t.Context(), sig.Fingerprint()))
	assert.Equal(t, "open", show(uid).Issue.Status)
	assertStatus(t, request(http.MethodPost, path, `{}`, "synthetic-archive-token"), http.StatusOK)
	links, err := te.db.GetFrictionIssueLinks(t.Context(), []string{sig.Fingerprint()})
	require.NoError(t, err)
	assert.Equal(t, uid, links[sig.Fingerprint()].IssueUID, "metadata rediscovery reuses the issue")
	reopenedArchive, err := db.OpenReadOnly(t.Context(), te.db.Path())
	require.NoError(t, err)
	storedLinks, err := reopenedArchive.GetFrictionIssueLinks(t.Context(), []string{sig.Fingerprint()})
	require.NoError(t, err)
	assert.Equal(t, uid, storedLinks[sig.Fingerprint()].IssueUID, "a reopened archive retains the mapping")
	require.NoError(t, reopenedArchive.Close())
	var listing kg.ListIssuesResponseBody
	require.NoError(t, json.Unmarshal(readKata(fmt.Sprintf("/api/v1/projects/%d/issues", nativeHub.ProjectID)), &listing))
	assert.Len(t, listing.Issues, 1)

	closeIssue := func(reason string) {
		evidence := ""
		if reason == "done" {
			evidence = `,"evidence":[{"type":"test","command":"TestFrictionAgainstNativeKataHub"}]`
		}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
			fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/actions/close", kataServer.URL, nativeHub.ProjectID, uid),
			strings.NewReader(fmt.Sprintf(`{"actor":"agentsview","reason":%q,"message":"This closure is synthetic and intentionally exercised to verify recurrence policy."%s}`, reason, evidence)))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := kataServer.Client().Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, http.StatusOK, resp.StatusCode, "%s", body)
	}
	closeIssue("done")
	_, err = f.File(t.Context(), sig, filing.RunContext{Date: recurredDate, PublicURL: f.PublicURL})
	require.NoError(t, err)
	reopened := show(uid)
	assert.Equal(t, "open", reopened.Issue.Status)
	var comments []string
	for _, comment := range reopened.Comments {
		comments = append(comments, comment.Body)
	}
	assert.Contains(t, comments, fmt.Sprintf("Recurred on %s — closure may have been premature.\nSession: https://archive.example.test/sessions/claude/today?msg=1", recurredDate))
	var labels []string
	for _, label := range reopened.Labels {
		labels = append(labels, label.Label)
	}
	assert.Contains(t, labels, "friction:recurred")
	_, err = f.File(t.Context(), sig, filing.RunContext{Date: recurredDate, PublicURL: f.PublicURL})
	require.NoError(t, err)
	assert.Len(t, show(uid).Comments, len(reopened.Comments), "same-date retries do not duplicate recurrence notes")
	closeIssue("wontfix")
	_, err = f.File(t.Context(), sig, filing.RunContext{Date: wontfixDate, PublicURL: f.PublicURL})
	require.NoError(t, err)
	assert.Equal(t, "closed", show(uid).Issue.Status)

	// Native auth rejects a wrong bearer; a pusher does not contact Kata.
	badCfg := clientCfg
	badCfg.TokenEnv, badCfg.Token = "", "wrong-token"
	bad, _ := kata.Probe(t.Context(), badCfg)
	assert.Equal(t, kata.StateUnauthenticated, bad.State)
	before := requests.Load()
	pushCfg := clientCfg
	pushCfg.Hub = false
	pusher, _ := kata.Probe(t.Context(), pushCfg)
	assert.Equal(t, kata.StateNotHub, pusher.State)
	assert.Equal(t, before, requests.Load())

	// Automatic filing survives an outage and sends the same native request
	// through the persisted outbox when the hub returns.
	old := seed("claude:yesterday", "2026-09-20", "No, preserve the original settings before making edits.")
	unavailable.Store(true)
	assert.Equal(t, kata.StateUnavailable, conn.Status(t.Context(), true).State)
	report, err := runner.BuildDate(t.Context(), "2026-09-20", review.BuildOptions{})
	require.NoError(t, err)
	assert.True(t, report.Written)
	rows, err := te.db.GetFrictionIssueLinks(t.Context(), []string{old.Fingerprint()})
	require.NoError(t, err)
	assert.Equal(t, db.FrictionLinkStatePending, rows[old.Fingerprint()].State)
	unavailable.Store(false)
	assert.Equal(t, kata.StateReady, conn.Status(t.Context(), true).State)
	drained, err := f.Drain(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, drained.Created)
	rows, err = te.db.GetFrictionIssueLinks(t.Context(), []string{old.Fingerprint()})
	require.NoError(t, err)
	assert.Equal(t, db.FrictionLinkStateLinked, rows[old.Fingerprint()].State)
	assert.Contains(t, show(rows[old.Fingerprint()].IssueUID).Issue.Body, "https://archive.example.test/friction/2026-09-20")
	_, err = f.Drain(t.Context())
	require.NoError(t, err)
	var after kg.ListIssuesResponseBody
	require.NoError(t, json.Unmarshal(readKata(fmt.Sprintf("/api/v1/projects/%d/issues", nativeHub.ProjectID)), &after))
	assert.Len(t, after.Issues, 2)
}

type pinnedKataHub struct {
	Endpoint  string `json:"endpoint"`
	ProjectID int64  `json:"project_id"`
}

func startPinnedKataHub(t *testing.T, token string) pinnedKataHub {
	t.Helper()
	helperDir := filepath.Join("testdata", "kata-v0180")
	serviceName := "kata-test-service"
	if runtime.GOOS == "windows" {
		serviceName += ".exe"
	}
	servicePath := filepath.Join(t.TempDir(), serviceName)
	build := exec.CommandContext(t.Context(), "go", "build", "-mod=readonly", "-o", servicePath, ".")
	build.Dir = helperDir
	build.Env = append(os.Environ(), "GOWORK=off")
	output, err := build.CombinedOutput()
	require.NoError(t, err, "build pinned Kata v0.18.0 test service: %s", output)

	dir := t.TempDir()
	readyPath := filepath.Join(dir, "ready.json")
	logPath := filepath.Join(dir, "kata-service.log")
	logFile, err := os.Create(logPath)
	require.NoError(t, err)
	cmdCtx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	cmd := exec.CommandContext(cmdCtx, servicePath)
	cmd.Env = append(os.Environ(),
		"GOWORK=off",
		"AGENTSVIEW_TEST_KATA_DSN="+filepath.Join(dir, "kata.db"),
		"AGENTSVIEW_TEST_KATA_TOKEN="+token,
		"AGENTSVIEW_TEST_KATA_READY="+readyPath,
	)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	require.NoError(t, cmd.Start())
	_ = logFile.Close()

	finished := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(finished)
	}()
	t.Cleanup(func() {
		defer cancel()
		select {
		case <-finished:
			if waitErr != nil {
				assert.NoError(t, waitErr, "pinned Kata test service exited: %s", readTestServiceLog(t, logPath))
			}
		default:
			shutdownRequested := false
			shutdownReq, err := http.NewRequestWithContext(cmdCtx, http.MethodPost, nativeEndpoint(t, readyPath)+"/__test/shutdown", nil)
			if err == nil {
				shutdownReq.Header.Set("Authorization", "Bearer "+token)
				if resp, requestErr := http.DefaultClient.Do(shutdownReq); requestErr == nil {
					shutdownRequested = resp.StatusCode == http.StatusNoContent
					_ = resp.Body.Close()
				}
			}
			if !shutdownRequested {
				_ = cmd.Process.Signal(os.Interrupt)
			}
			select {
			case <-finished:
			case <-time.After(5 * time.Second):
				_ = cmd.Process.Kill()
				<-finished
			}
			if shutdownRequested && waitErr != nil {
				assert.NoError(t, waitErr, "stop pinned Kata test service: %s", readTestServiceLog(t, logPath))
			}
		}
	})

	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(45 * time.Second)
	defer timeout.Stop()
	for {
		if data, err := os.ReadFile(readyPath); err == nil {
			var ready pinnedKataHub
			if json.Unmarshal(data, &ready) == nil && ready.Endpoint != "" && ready.ProjectID > 0 {
				return ready
			}
		}
		select {
		case <-finished:
			require.FailNowf(t, "pinned Kata test service exited", "%v\n%s", waitErr, readTestServiceLog(t, logPath))
		case <-timeout.C:
			_ = cmd.Process.Kill()
			<-finished
			require.FailNowf(t, "pinned Kata test service did not start", "%s", readTestServiceLog(t, logPath))
		case <-ticker.C:
		}
	}
}

func nativeEndpoint(t *testing.T, readyPath string) string {
	t.Helper()
	data, err := os.ReadFile(readyPath)
	if err != nil {
		return ""
	}
	var ready pinnedKataHub
	if err := json.Unmarshal(data, &ready); err != nil {
		return ""
	}
	return ready.Endpoint
}

func readTestServiceLog(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}
