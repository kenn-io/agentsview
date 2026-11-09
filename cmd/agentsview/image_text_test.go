package main

import (
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
	job := imageTextJob(sched)
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
