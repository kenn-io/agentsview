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

func TestImageTextJobKeepsDaemonAwake(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	manager, err := setupImageText(config.Config{}, d)
	require.NoError(t, err)
	require.Nil(t, manager)
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
	manager, err = setupImageText(config.Config{ImageText: config.ImageTextConfig{Enabled: true, Endpoint: srv.URL + "/glmocr/parse"}}, d)
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
