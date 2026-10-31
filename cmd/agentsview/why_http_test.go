package main

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWhyCLIExplicitServerUsesOneStoredLookup(t *testing.T) {
	dir, sha := whyCLIRepo(t)
	t.Chdir(dir)
	tokenFile := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("fixture-token\n"), 0600))
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/commits/sessions", r.URL.Path)
		assert.Equal(t, "Bearer fixture-token", r.Header.Get("Authorization"))
		raw, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			http.Error(w, "invalid fixture request", 400)
			return
		}
		var body struct {
			SHAs []string `json:"shas"`
		}
		if !assert.NoError(t, json.Unmarshal(raw, &body)) {
			http.Error(w, "invalid fixture JSON", 400)
			return
		}
		assert.Equal(t, []string{sha}, body.SHAs)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"commits":{"%s":[{"session_id":"codex:00000000-0000-4000-8000-000000000001","commit_sha":"%s","resolved":true,"source":"transcript","message_ordinal":0,"call_index":0,"repository_path":"/example/repository","branch":"main","subject":"Initial","committed_at":"2026-10-30T12:00:00Z","ambiguous":false,"agent":"codex","project":"fixture","session_started_at":"2026-10-30T11:00:00Z","web_url":"/sessions/codex:00000000-0000-4000-8000-000000000001","session":{"id":"codex:00000000-0000-4000-8000-000000000001"},"gone":false,"turn":{"ordinal":0,"at":"2026-10-30T11:59:00Z","prompt":"Recorded fixture prompt","why":"commit-call"}}]}}`, sha, sha)
	}))
	defer server.Close()
	cmd := newWhyCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--commit", sha, "--json", "--no-sync", "--server", server.URL, "--server-token-file", tokenFile})
	require.NoError(t, cmd.Execute())
	assert.Equal(t, int32(1), calls.Load())
	var got struct {
		Explained bool `json:"explained"`
		Refs      []struct {
			Card struct {
				MessageOrdinal *int `json:"message_ordinal"`
				CallIndex      *int `json:"call_index"`
				Turn           *struct {
					Prompt string `json:"prompt"`
				} `json:"turn"`
			} `json:"card"`
		} `json:"refs"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &got))
	assert.True(t, got.Explained)
	require.Len(t, got.Refs, 1)
	require.NotNil(t, got.Refs[0].Card.MessageOrdinal)
	assert.Zero(t, *got.Refs[0].Card.MessageOrdinal)
	require.NotNil(t, got.Refs[0].Card.CallIndex)
	assert.Zero(t, *got.Refs[0].Card.CallIndex)
	require.NotNil(t, got.Refs[0].Card.Turn)
	assert.Equal(t, "Recorded fixture prompt", got.Refs[0].Card.Turn.Prompt)
}

func TestWhyCLIUnsupportedArchiveFailsWithUpgradeHint(t *testing.T) {
	dir, sha := whyCLIRepo(t)
	t.Chdir(dir)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/commits/sessions", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"unsupported lookup route"}`)
	}))
	defer server.Close()
	cmd := newWhyCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--commit", sha, "--json", "--server", server.URL})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "upgrade")
	assert.Empty(t, out.String())
	assert.Equal(t, int32(1), calls.Load())
}
