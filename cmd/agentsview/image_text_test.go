package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/docbank/document/glmocr"
)

func TestImageTextScheduler(t *testing.T) {
	ctx := t.Context()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	sched, err := setupImageText(config.Config{}, d)
	require.NoError(t, err)
	require.Nil(t, sched)
	requests := make(chan struct{}, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- struct{}{}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"model":"glm-ocr","deployment_fingerprint":%q,"json_result":[[{"index":0,"label":"text","content":"known text"}]]}`, glmocr.DefaultDeploymentFingerprint)
	}))
	t.Cleanup(srv.Close)
	cfg := config.Config{ImageText: config.ImageTextConfig{Enabled: true, Endpoint: srv.URL + "/glmocr/parse"}}
	add := func(id string, body []byte) {
		t.Helper()
		content := fmt.Sprintf(`[{"type":"input_image","image_url":"data:image/png;base64,%s"}]`, base64.StdEncoding.EncodeToString(body))
		require.NoError(t, d.UpsertSession(ctx, db.Session{ID: id, Project: "demo", Agent: "test", MessageCount: 1}))
		require.NoError(t, d.InsertMessages(ctx, []db.Message{{SessionID: id, Role: "assistant", Content: "answer", ToolCalls: []db.ToolCall{{ToolName: "Read", Category: "Read", ResultContent: content}}}}))
	}
	add("one", []byte("\x89PNG\r\n\x1a\nfirst"))
	sched, err = setupImageText(cfg, d)
	require.NoError(t, err)
	job := imageTextJob(sched, nil)
	require.NoError(t, job.Run(ctx))
	wait := func() {
		t.Helper()
		select {
		case <-requests:
		case <-time.After(5 * time.Second):
			t.Fatal("OCR request did not arrive")
		}
	}
	wait()
	add("two", []byte("\x89PNG\r\n\x1a\nsecond"))
	require.NoError(t, job.Run(ctx))
	wait()
}

func TestImageTextJobKeepsDaemonAwake(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	started, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"model":"glm-ocr","deployment_fingerprint":%q,"json_result":[[{"index":0,"label":"text","content":"known"}]]}`, glmocr.DefaultDeploymentFingerprint)
	}))
	t.Cleanup(srv.Close)
	body := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nimage"))
	content := fmt.Sprintf(`[{"type":"input_image","image_url":"data:image/png;base64,%s"}]`, body)
	require.NoError(t, d.UpsertSession(ctx, db.Session{ID: "one", Project: "demo", Agent: "test", MessageCount: 1}))
	require.NoError(t, d.InsertMessages(ctx, []db.Message{{SessionID: "one", Role: "assistant", Content: "answer", ToolCalls: []db.ToolCall{{ToolName: "Read", ResultContent: content}}}}))
	manager, err := setupImageText(config.Config{ImageText: config.ImageTextConfig{Enabled: true, Endpoint: srv.URL + "/glmocr/parse"}}, d)
	require.NoError(t, err)
	idled := make(chan struct{})
	tracker := server.NewIdleTracker(time.Second, func() { close(idled) })
	finished := make(chan error, 1)
	go func() { finished <- imageTextJob(manager, tracker).Run(ctx) }()
	<-started
	trackerDone := make(chan struct{})
	go func() { tracker.Run(ctx); close(trackerDone) }()
	select {
	case <-idled:
		t.Fatal("daemon idled during OCR")
	case <-time.After(2 * time.Second):
	}
	close(release)
	require.NoError(t, <-finished)
	select {
	case <-idled:
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not idle after OCR")
	}
	cancel()
	<-trackerDone
}
