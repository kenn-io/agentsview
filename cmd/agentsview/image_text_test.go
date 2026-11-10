package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"log"
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
	"go.kenn.io/docbank/document/ocr"
)

func TestImageTextRespectsArchiveContent(t *testing.T) {
	var logs bytes.Buffer
	writer := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(writer) })
	ctx := t.Context()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	first := d
	t.Cleanup(func() { require.NoError(t, first.Close()) })
	cfg := config.Config{ArchiveContent: config.ArchiveContentTranscripts, ImageText: config.ImageTextConfig{Enabled: true}}
	manager, err := setupImageText(t.Context(), cfg, d)
	require.NoError(t, err)
	require.Nil(t, manager)
	require.Contains(t, logs.String(), "image_text: disabled because archive_content omits tool results")
	logs.Reset()
	cfg.ArchiveContent = config.ArchiveContentFull
	d.SetArchiveContent(config.ArchiveContentTranscripts)
	manager, err = setupImageText(t.Context(), cfg, d)
	require.NoError(t, err)
	require.Nil(t, manager)
	require.Contains(t, logs.String(), "image_text: disabled because archive_content omits tool results")
	logs.Reset()
	d, err = db.Open(ctx, filepath.Join(t.TempDir(), "full.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	manager, err = setupImageText(t.Context(), cfg, d)
	require.NoError(t, err)
	require.NotNil(t, manager)
	require.NoError(t, d.BackfillImageTextOccurrences(ctx))
	require.NoError(t, d.UpsertSession(ctx, db.Session{ID: "one", Project: "demo", Agent: "test"}))
	content := `[{"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgppbWFnZQ=="}]`
	require.NoError(t, d.InsertMessages(ctx, []db.Message{{SessionID: "one", Role: "assistant", ToolCalls: []db.ToolCall{{ToolName: "Read", ResultContent: content}}}}))
	identity, err := ocr.NewIdentity("test", "model", "revision")
	require.NoError(t, err)
	require.NoError(t, d.SaveImageText(ctx, db.RetainedToolResultImages(content)[0].SHA256, identity, "invoice", false))
	cfg.ImageText.Enabled = false
	logs.Reset()
	manager, err = setupImageText(ctx, cfg, d)
	require.NoError(t, err)
	require.Nil(t, manager)
	require.Empty(t, logs.String())
	var count int
	require.NoError(t, d.Reader().QueryRow(ctx, `SELECT count(*) FROM image_text_cache`).Scan(&count))
	require.Zero(t, count)
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
	manager, err := setupImageText(t.Context(), config.Config{ImageText: config.ImageTextConfig{Enabled: true, Endpoint: srv.URL + "/glmocr/parse"}}, d)
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
		require.FailNow(t, "daemon idled during OCR")
	case <-time.After(2 * time.Second):
	}
	close(release)
	require.NoError(t, <-finished)
	policy, err := (config.ImageTextConfig{Enabled: true}).Policy()
	require.NoError(t, err)
	var processor string
	require.NoError(t, d.Reader().QueryRow(ctx, `SELECT processor FROM image_text_cache`).Scan(&processor))
	require.Equal(t, policy.Fingerprint(), processor)
	cancel()
	<-trackerDone
}
