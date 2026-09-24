// cmd/agentsview/friction_filing_test.go
package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/apiclient"
)

func runFrictionCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	root := newRootCommand()
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs(args)
	_, err := root.ExecuteC()
	return out.String(), err
}

func TestFrictionFilingCommands(t *testing.T) {
	type call struct{ method, path, body string }
	tests := []struct {
		name     string
		args     []string
		status   int
		response string
		want     call
		wantOut  string
		wantErr  string
	}{
		{name: "file", args: []string{"friction", "file", "fl1:aa"}, status: 200,
			response: `{"link":{"fingerprint":"fl1:aa","state":"linked","qualified_id":"agentsview#f001","web_url":"https://kata.example.test/issues/x"}}`,
			want:     call{"POST", "/api/v1/friction/patterns/fl1:aa/file", `{}`}, wantOut: "fl1:aa  linked  agentsview#f001  https://kata.example.test/issues/x\n"},
		{name: "file_force_new", args: []string{"friction", "file", "fl1:aa", "--force-new"}, status: 200,
			response: `{"link":{"fingerprint":"fl1:aa","state":"linked","qualified_id":"agentsview#f002"}}`,
			want:     call{"POST", "/api/v1/friction/patterns/fl1:aa/file", `{"force_new":true}`}},
		{name: "file_dry_run", args: []string{"friction", "file", "fl1:aa", "--dry-run"}, status: 200,
			response: `{"preview":{"title":"[friction/error] Bash: boom","body":"Detected by agentsview Friction Log on 2026-09-20.","priority":3,"labels":["friction","friction:error"],"metadata":{"friction.fingerprint":"fl1:aa"},"force_new":false}}`,
			want:     call{"POST", "/api/v1/friction/patterns/fl1:aa/file", `{"dry_run":true}`}, wantOut: "Title:    [friction/error] Bash: boom\n"},
		{name: "file_needs_human", args: []string{"friction", "file", "fl1:aa"}, status: 200,
			response: `{"link":{"fingerprint":"fl1:aa","state":"needs_human","last_error_code":"duplicate_candidates","last_error":"kata: 409 duplicate_candidates: similar"}}`,
			want:     call{"POST", "/api/v1/friction/patterns/fl1:aa/file", `{}`}, wantOut: "needs_human  duplicate_candidates"},
		{name: "file_on_pusher", args: []string{"friction", "file", "fl1:aa"}, status: 503,
			response: `{"code":"kata_unavailable","kata_state":"not_hub","error":"this instance pushes its archive to PostgreSQL; only the agentsview hub files to Kata"}`,
			want:     call{"POST", "/api/v1/friction/patterns/fl1:aa/file", `{}`}, wantErr: "only the agentsview hub files to Kata"},
		{name: "link", args: []string{"friction", "link", "fl1:aa", "agentsview#f009"}, status: 200,
			response: `{"fingerprint":"fl1:aa","state":"linked","qualified_id":"agentsview#f009","link_source":"manual"}`,
			want:     call{"PUT", "/api/v1/friction/patterns/fl1:aa/link", `{"issue_ref":"agentsview#f009"}`}, wantOut: "agentsview#f009"},
		{name: "unlink", args: []string{"friction", "unlink", "fl1:aa"}, status: 200,
			response: `{"fingerprint":"fl1:aa","unlinked":true}`,
			want:     call{"DELETE", "/api/v1/friction/patterns/fl1:aa/link", ``}, wantOut: "unlinked fl1:aa\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got call
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				got = call{r.Method, r.URL.Path, string(b)}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.response))
			}))
			t.Cleanup(ts.Close)
			out, err := runFrictionCmd(t, append(tt.args, "--server", ts.URL)...)
			assert.Equal(t, tt.want.method, got.method)
			assert.Equal(t, tt.want.path, got.path)
			if tt.want.body != "" {
				assert.JSONEq(t, tt.want.body, got.body)
			}
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Contains(t, out, tt.wantOut)
		})
	}
}

func TestFrictionFileRequiresDaemon(t *testing.T) {
	t.Setenv("AGENTSVIEW_DATA_DIR", t.TempDir())
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	_, err := runFrictionCmd(t, "friction", "file", "fl1:aa")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires the running agentsview hub daemon")
}

func TestFrictionFileDate(t *testing.T) {
	var posted []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			assert.Equal(t, "/api/v1/friction/digests/2026-09-20", r.URL.Path)
			_, _ = w.Write([]byte(`{"date":"2026-09-20","signals":[{"fingerprint":"fl1:aa","kind":"error"},{"fingerprint":"fl1:aa","kind":"error"},{"fingerprint":"fl1:bb","kind":"correction"}]}`))
			return
		}
		posted = append(posted, r.URL.Path)
		_, _ = w.Write([]byte(`{"link":{"fingerprint":"x","state":"linked","qualified_id":"agentsview#f001"}}`))
	}))
	t.Cleanup(ts.Close)
	_, err := runFrictionCmd(t, "friction", "file", "--date", "2026-09-20", "--server", ts.URL)
	require.NoError(t, err)
	assert.Equal(t, []string{"/api/v1/friction/patterns/fl1:aa/file", "/api/v1/friction/patterns/fl1:bb/file"}, posted)
}

func TestFrictionFilePreviewMetadata(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"preview":{"title":"example","body":"example","priority":3,"labels":[],"metadata":{"friction.fingerprint":"fl1:aa"},"force_new":false}}`))
	}))
	t.Cleanup(ts.Close)
	api, err := apiclient.NewHTTPClient(ts.URL, "", ts.Client())
	require.NoError(t, err)
	resp, err := api.PostAPIV1FrictionPatternsFingerprintFileWithResponse(t.Context(), &apiclient.PostAPIV1FrictionPatternsFingerprintFileRequestOptions{
		PathParams: &apiclient.PostAPIV1FrictionPatternsFingerprintFilePath{Fingerprint: "fl1:aa"},
		Body:       &apiclient.FrictionFileRequest{DryRun: new(true)},
	})
	require.NoError(t, err)
	require.NotNil(t, resp.JSON200)
	require.NotNil(t, resp.JSON200.Preview)
	assert.Equal(t, "fl1:aa", resp.JSON200.Preview.Metadata["friction.fingerprint"])
}
