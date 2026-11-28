package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/rawarchive"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/docbank/document/glmocr"
	"go.kenn.io/docbank/document/ocr"
)

func TestImageTextRespectsArchiveContent(t *testing.T) {
	logs := captureLogOutput(t)
	ctx := t.Context()
	for _, tc := range []struct {
		name                           string
		configContent, databaseContent config.ArchiveContent
	}{
		{"config omits tool results", config.ArchiveContentTranscripts, config.ArchiveContentFull},
		{"archive omits tool results", config.ArchiveContentFull, config.ArchiveContentTranscripts},
		{"disabled", config.ArchiveContentFull, config.ArchiveContentFull},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := db.Open(ctx, filepath.Join(t.TempDir(), "archive.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, d.Close()) })
			require.NoError(t, d.BackfillImageTextOccurrences(ctx))
			require.NoError(t, d.UpsertSession(ctx, db.Session{ID: "one", Project: "demo", Agent: "test"}))
			content := `[{"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgppbWFnZQ=="}]`
			require.NoError(t, d.InsertMessages(ctx, []db.Message{{SessionID: "one", Role: "assistant", ToolCalls: []db.ToolCall{{ToolName: "Read", ResultContent: content}}}}))
			identity, err := ocr.NewIdentity("test", "model", "revision")
			require.NoError(t, err)
			require.NoError(t, d.SaveImageText(ctx, db.RetainedToolResultImages(content)[0].SHA256, identity, "invoice", false))
			d.SetArchiveContent(tc.databaseContent)
			cfg := config.Config{ArchiveContent: tc.configContent, ImageText: config.ImageTextConfig{Enabled: tc.name != "disabled"}}
			logs.Reset()
			manager, err := setupImageText(ctx, cfg, d)
			require.NoError(t, err)
			require.Nil(t, manager)
			if cfg.ImageText.Enabled {
				require.Contains(t, logs.String(), "image_text: disabled because archive_content omits tool results")
			} else {
				require.Empty(t, logs.String())
			}
			var count int
			require.NoError(t, d.Reader().QueryRow(ctx, `SELECT count(*) FROM image_text_cache`).Scan(&count))
			require.Zero(t, count)
		})
	}
}

func TestDetachedArchiveImageTextConfiguration(t *testing.T) {
	ctx := t.Context()
	source := t.TempDir()
	d, err := db.Open(ctx, filepath.Join(source, "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() {
		if d != nil {
			require.NoError(t, d.Close())
		}
	})
	require.NoError(t, d.BackfillImageTextOccurrences(ctx))
	require.NoError(t, d.UpsertSession(ctx, db.Session{ID: "one", Project: "demo", Agent: "test"}))
	body := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nimage"))
	content := fmt.Sprintf(`[{"type":"input_image","image_url":"data:image/png;base64,%s"}]`, body)
	require.NoError(t, d.InsertMessages(ctx, []db.Message{{SessionID: "one", Role: "assistant", ToolCalls: []db.ToolCall{{ToolName: "Read", ResultContent: content}}}}))
	identity, err := ocr.NewIdentity("test", "model", "revision")
	require.NoError(t, err)
	require.NoError(t, d.SaveImageText(ctx, db.RetainedToolResultImages(content)[0].SHA256, identity, "invoice", false))
	require.NoError(t, d.Close())
	d = nil
	dest := filepath.Join(t.TempDir(), "copy")
	_, err = rawarchive.Copy(ctx, rawarchive.CopyOptions{DataDir: source, Destination: dest, Settings: rawarchive.RecoverySettings{LocalMachineName: "source"}})
	require.NoError(t, err)
	configPath := filepath.Join(dest, "config.toml")
	originalConfig, err := os.ReadFile(configPath)
	require.NoError(t, err)
	openCopy := func() config.Config {
		cfg, err := config.LoadReadOnlyAt(dest)
		require.NoError(t, err)
		d, err = db.Open(ctx, filepath.Join(dest, "sessions.db"))
		require.NoError(t, err)
		return cfg
	}
	search := func(query string, want int) {
		page, err := d.Search(ctx, db.SearchFilter{Query: query})
		require.NoError(t, err)
		require.Len(t, page.Results, want)
	}
	cfg := openCopy()
	require.False(t, cfg.ImageTextConfigured)
	manager, err := setupImageText(ctx, cfg, d)
	require.NoError(t, err)
	require.Nil(t, manager)
	search("invoice", 1)
	require.NoError(t, d.Close())
	d = nil

	require.NoError(t, os.WriteFile(configPath, append(originalConfig, []byte("\n[image_text]\nenabled = false\n")...), 0o600))
	cfg = openCopy()
	require.True(t, cfg.ImageTextConfigured)
	manager, err = setupImageText(ctx, cfg, d)
	require.NoError(t, err)
	require.Nil(t, manager)
	for _, table := range []string{"image_text_cache", "image_text_occurrences"} {
		var count int
		require.NoError(t, d.Reader().QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count))
		require.Zero(t, count)
	}
	search("invoice", 0)
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
