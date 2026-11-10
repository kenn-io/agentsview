package imagetext

import (
	"database/sql"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/assets"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/glmocr"
)

func TestImageTextPassKeepsRetryCutoff(t *testing.T) {
	ctx := t.Context()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		time.Sleep(2500 * time.Millisecond)
		fmt.Fprintf(w, `{"model":"glm-ocr","deployment_fingerprint":%q,"json_result":[[{"index":0,"label":"text","content":"example"}]]}`, glmocr.DefaultDeploymentFingerprint)
	}))
	t.Cleanup(srv.Close)
	policy, err := (config.ImageTextConfig{Enabled: true, Endpoint: srv.URL + "/glmocr/parse"}).Policy()
	require.NoError(t, err)
	client, err := glmocr.NewClient(policy, glmocr.ClientConfig{})
	require.NoError(t, err)
	require.NoError(t, d.UpsertSession(ctx, db.Session{ID: "one", Project: "demo", Agent: "test"}))
	var hashes []string
	for i := range 2 {
		content := fmt.Sprintf(`[{"type":"file","uri":"data:image/png;base64,%s"}]`, base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("\x89PNG\r\n\x1a\nimage%d", i))))
		hashes = append(hashes, db.RetainedToolResultImages(content)[0].SHA256)
		require.NoError(t, d.InsertMessages(ctx, []db.Message{{SessionID: "one", Ordinal: i, Role: "assistant", ToolCalls: []db.ToolCall{{ToolName: "Read", ResultContent: content}}}}))
	}
	require.NoError(t, d.SaveImageText(ctx, hashes[0], client.Identity(), "", true))
	raw, err := sql.Open("sqlite3", d.Path())
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, `UPDATE image_text_cache SET retry_at=? WHERE image_hash=?`, time.Now().UTC().Add(time.Second).Format(time.RFC3339), hashes[0])
	require.NoError(t, err)
	require.NoError(t, raw.Close())
	require.NoError(t, (&Manager{DB: d, Client: client}).Run(ctx))
	require.Equal(t, 1, requests)
	pending, err := d.PendingImageText(ctx, client.Identity().Fingerprint, time.Now(), 20)
	require.NoError(t, err)
	require.Equal(t, []string{hashes[0]}, pending)
}

func TestImageTextFailureClassification(t *testing.T) {
	for _, name := range []string{"service error", "corrupt offloaded asset", "invalid inline bytes"} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			d, err := db.Open(ctx, filepath.Join(t.TempDir(), "archive.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, d.Close()) })
			requests := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.Header().Set("Content-Type", "application/json")
				if name == "service error" && requests == 1 {
					fmt.Fprintf(w, `{"model":"glm-ocr","deployment_fingerprint":%q,"json_result":[[]]}`, glmocr.DefaultDeploymentFingerprint)
				} else {
					fmt.Fprintf(w, `{"model":"glm-ocr","deployment_fingerprint":%q,"json_result":[[{"index":0,"label":"text","content":"retry succeeded"}]]}`, glmocr.DefaultDeploymentFingerprint)
				}
			}))
			t.Cleanup(srv.Close)
			policy, err := (config.ImageTextConfig{Enabled: true, Endpoint: srv.URL + "/glmocr/parse"}).Policy()
			require.NoError(t, err)
			client, err := glmocr.NewClient(policy, glmocr.ClientConfig{})
			require.NoError(t, err)
			manager := Manager{DB: d, Client: client}
			png := []byte("\x89PNG\r\n\x1a\nimage")
			content := fmt.Sprintf(`[{"type":"file","uri":"data:image/png;base64,%s"}]`, base64.StdEncoding.EncodeToString(png))
			var path string
			if name == "corrupt offloaded asset" {
				dir := t.TempDir()
				d.SetAssetsDir(dir)
				ref, _, err := assets.Put(dir, "image/png", png)
				require.NoError(t, err)
				path = filepath.Join(dir, strings.TrimPrefix(ref, "asset://"))
				corrupt := append([]byte(nil), png...)
				corrupt[len(corrupt)-1] ^= 1
				require.NoError(t, os.WriteFile(path, corrupt, 0600))
				content = fmt.Sprintf(`[{"type":"agentsview_image","version":1,"image_ref":%q,"media_type":"image/png","byte_size":%d}]`, ref, len(png))
			} else if name == "invalid inline bytes" {
				content = `[{"type":"input_image","image_url":"data:image/png;base64,dGhpcyBpcyBub3QgYSBwbmc="}]`
			}
			require.NoError(t, d.UpsertSession(ctx, db.Session{ID: "one", Project: "demo", Agent: "test", MessageCount: 1}))
			require.NoError(t, d.InsertMessages(ctx, []db.Message{{SessionID: "one", Ordinal: 1, Role: "assistant", Content: "answer", ToolCalls: []db.ToolCall{{ToolName: "Read", ResultContent: content}}}}))
			require.NoError(t, manager.Run(ctx))
			var failures int
			var retry string
			require.NoError(t, d.Reader().QueryRow(ctx, `SELECT failures,retry_at FROM image_text_cache`).Scan(&failures, &retry))
			require.Equal(t, 1, failures)
			if name == "invalid inline bytes" {
				require.Zero(t, requests)
				require.Equal(t, "9999-12-31T23:59:59Z", retry)
				return
			}
			require.NotEmpty(t, retry)
			require.NotEqual(t, "9999-12-31T23:59:59Z", retry)
			if name == "service error" {
				require.Equal(t, 1, requests)
				require.NoError(t, manager.Run(ctx))
				require.Equal(t, 1, requests)
			} else {
				require.Zero(t, requests)
				require.NoError(t, os.WriteFile(path, png, 0600))
			}
			raw, err := sql.Open("sqlite3", d.Path())
			require.NoError(t, err)
			_, err = raw.ExecContext(ctx, `UPDATE image_text_cache SET retry_at='2000-01-01T00:00:00Z'`)
			require.NoError(t, err)
			require.NoError(t, raw.Close())
			require.NoError(t, manager.Run(ctx))
			if name == "service error" {
				require.Equal(t, 2, requests)
			} else {
				require.Equal(t, 1, requests)
			}
			var succeeded int
			require.NoError(t, d.Reader().QueryRow(ctx, `SELECT succeeded FROM image_text_cache`).Scan(&succeeded))
			require.Equal(t, 1, succeeded)
		})
	}
}

func TestImageTextProcessorKey(t *testing.T) {
	first, err := (config.ImageTextConfig{Enabled: true, Endpoint: "http://127.0.0.1:30004/glmocr/parse"}).Policy()
	require.NoError(t, err)
	second, err := (config.ImageTextConfig{Enabled: true, Endpoint: "http://127.0.0.1:30005/glmocr/parse"}).Policy()
	require.NoError(t, err)
	firstKey, err := ProcessorKey(first)
	require.NoError(t, err)
	require.Equal(t, first.Fingerprint(), firstKey)
	secondKey, err := ProcessorKey(second)
	require.NoError(t, err)
	require.Equal(t, firstKey, secondKey)
	normalize, err := document.NewNormalizePolicy(100)
	require.NoError(t, err)
	changed, err := glmocr.NewPolicy(glmocr.PolicyConfig{NormalizePolicy: normalize, MaxDocumentBytes: glmocr.MaxDocumentBytes, MaxResponseBytes: glmocr.MaxResponseBytes, MaxUnits: glmocr.MaxUnits})
	require.NoError(t, err)
	changedKey, err := ProcessorKey(changed)
	require.NoError(t, err)
	require.NotEqual(t, firstKey, changedKey)
}

func TestImageTextPassDrainsPages(t *testing.T) {
	ctx := t.Context()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"model":"glm-ocr","deployment_fingerprint":%q,"json_result":[[{"index":0,"label":"text","content":"invoice"}]]}`, glmocr.DefaultDeploymentFingerprint)
	}))
	t.Cleanup(srv.Close)
	policy, err := (config.ImageTextConfig{Enabled: true, Endpoint: srv.URL + "/glmocr/parse"}).Policy()
	require.NoError(t, err)
	client, err := glmocr.NewClient(policy, glmocr.ClientConfig{})
	require.NoError(t, err)
	for i := 0; i < 21; i++ {
		id := fmt.Sprintf("session-%02d", i)
		body := []byte(fmt.Sprintf("\x89PNG\r\n\x1a\n%d", i))
		content := fmt.Sprintf(`[{"type":"input_image","image_url":"data:image/png;base64,%s"}]`, base64.StdEncoding.EncodeToString(body))
		require.NoError(t, d.UpsertSession(ctx, db.Session{ID: id, Project: "demo", Agent: "test", MessageCount: 1}))
		require.NoError(t, d.InsertMessages(ctx, []db.Message{{SessionID: id, Ordinal: 3, Role: "assistant", Content: "answer", ToolCalls: []db.ToolCall{{ToolName: "Read", ResultContent: content}}}}))
	}
	manager := Manager{DB: d, Client: client}
	err = manager.Run(ctx)
	require.NoError(t, err)
	require.Equal(t, 21, requests)
}

func TestImageTextUsesSameSessionRetainedCopy(t *testing.T) {
	ctx := t.Context()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	assetDir := t.TempDir()
	d.SetAssetsDir(assetDir)
	png := []byte("\x89PNG\r\n\x1a\nimage")
	ref, _, err := assets.Put(assetDir, "image/png", png)
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(assetDir, strings.TrimPrefix(ref, "asset://"))))
	offloaded := fmt.Sprintf(`[{"type":"agentsview_image","version":1,"image_ref":%q,"media_type":"image/png","byte_size":%d}]`, ref, len(png))
	inline := fmt.Sprintf(`[{"type":"input_image","image_url":"data:image/png;base64,%s"}]`, base64.StdEncoding.EncodeToString(png))
	require.NoError(t, d.UpsertSession(ctx, db.Session{ID: "same", Project: "demo", Agent: "test", MessageCount: 21}))
	var messages []db.Message
	for i := 0; i < 20; i++ {
		messages = append(messages, db.Message{SessionID: "same", Ordinal: i, Role: "assistant", Content: "missing", ToolCalls: []db.ToolCall{{ToolName: "Read", ResultContent: offloaded}}})
	}
	messages = append(messages, db.Message{SessionID: "same", Ordinal: 20, Role: "assistant", Content: "inline", ToolCalls: []db.ToolCall{{ToolName: "Read", ResultContent: inline}}})
	require.NoError(t, d.InsertMessages(ctx, messages))
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"model":"glm-ocr","deployment_fingerprint":%q,"json_result":[[{"index":0,"label":"text","content":"invoice"}]]}`, glmocr.DefaultDeploymentFingerprint)
	}))
	t.Cleanup(srv.Close)
	policy, err := (config.ImageTextConfig{Enabled: true, Endpoint: srv.URL + "/glmocr/parse"}).Policy()
	require.NoError(t, err)
	client, err := glmocr.NewClient(policy, glmocr.ClientConfig{})
	require.NoError(t, err)
	require.NoError(t, (&Manager{DB: d, Client: client}).Run(ctx))
	require.Equal(t, 1, requests)
	page, err := d.Search(ctx, db.SearchFilter{Query: "invoice"})
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
}

func TestImageTextCachesEmptyNormalizedOutput(t *testing.T) {
	ctx := t.Context()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	png := []byte("\x89PNG\r\n\x1a\nimage")
	content := fmt.Sprintf(`[{"type":"input_image","image_url":"data:image/png;base64,%s"}]`, base64.StdEncoding.EncodeToString(png))
	require.NoError(t, d.UpsertSession(ctx, db.Session{ID: "empty", Project: "demo", Agent: "test", MessageCount: 1}))
	require.NoError(t, d.InsertMessages(ctx, []db.Message{{SessionID: "empty", Role: "assistant", Content: "answer", ToolCalls: []db.ToolCall{{ToolName: "Read", ResultContent: content}}}}))
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"model":"glm-ocr","deployment_fingerprint":%q,"json_result":[[{"index":0,"label":"text","content":"<script>hidden</script>"}]]}`, glmocr.DefaultDeploymentFingerprint)
	}))
	t.Cleanup(srv.Close)
	policy, err := (config.ImageTextConfig{Enabled: true, Endpoint: srv.URL + "/glmocr/parse"}).Policy()
	require.NoError(t, err)
	client, err := glmocr.NewClient(policy, glmocr.ClientConfig{})
	require.NoError(t, err)
	manager := Manager{DB: d, Client: client}
	require.NoError(t, manager.Run(ctx))
	require.NoError(t, manager.Run(ctx))
	require.Equal(t, 1, requests)
	var text string
	var succeeded int
	require.NoError(t, d.Reader().QueryRow(ctx, `SELECT text,succeeded FROM image_text_cache`).Scan(&text, &succeeded))
	require.Empty(t, text)
	require.Equal(t, 1, succeeded)
}

func TestImageTextTerminalBecomesEligible(t *testing.T) {
	for _, name := range []string{"new occurrence", "changed media type"} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			d, err := db.Open(ctx, filepath.Join(t.TempDir(), "archive.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, d.Close()) })
			encoded := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nimage"))
			gif := fmt.Sprintf(`[{"type":"input_image","image_url":"data:image/gif;base64,%s"}]`, encoded)
			png := fmt.Sprintf(`[{"type":"input_image","image_url":"data:image/png;base64,%s"}]`, encoded)
			requests := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"model":"glm-ocr","deployment_fingerprint":%q,"json_result":[[{"index":0,"label":"text","content":"recovered invoice"}]]}`, glmocr.DefaultDeploymentFingerprint)
			}))
			t.Cleanup(srv.Close)
			policy, err := (config.ImageTextConfig{Enabled: true, Endpoint: srv.URL + "/glmocr/parse"}).Policy()
			require.NoError(t, err)
			client, err := glmocr.NewClient(policy, glmocr.ClientConfig{})
			require.NoError(t, err)
			manager := Manager{DB: d, Client: client}
			require.NoError(t, d.UpsertSession(ctx, db.Session{ID: "session", Project: "demo", Agent: "test", MessageCount: 1}))
			require.NoError(t, d.InsertMessages(ctx, []db.Message{{SessionID: "session", Ordinal: 1, Role: "assistant", Content: "first", ToolCalls: []db.ToolCall{{ToolUseID: "call", ToolName: "Read", ResultContent: gif}}}}))
			require.NoError(t, manager.Run(ctx))
			require.Zero(t, requests)
			var retry string
			require.NoError(t, d.Reader().QueryRow(ctx, `SELECT retry_at FROM image_text_cache`).Scan(&retry))
			require.Equal(t, "9999-12-31T23:59:59Z", retry)
			require.NoError(t, d.InsertMessages(ctx, []db.Message{{SessionID: "session", Ordinal: 2, Role: "assistant", Content: "append"}}))
			require.NoError(t, d.Reader().QueryRow(ctx, `SELECT retry_at FROM image_text_cache`).Scan(&retry))
			require.Equal(t, "9999-12-31T23:59:59Z", retry)
			if name == "changed media type" {
				var before, after int64
				require.NoError(t, d.Reader().QueryRow(ctx, `SELECT id FROM tool_calls WHERE tool_use_id='call'`).Scan(&before))
				_, err = d.WriteSessionIncremental(ctx, "session", nil, db.IncrementalSessionUpdate{SubagentLinks: []db.ToolCallSubagentLink{{ToolUseID: "call", HasResult: true, ResultContent: png}}})
				require.NoError(t, err)
				require.NoError(t, d.Reader().QueryRow(ctx, `SELECT id FROM tool_calls WHERE tool_use_id='call'`).Scan(&after))
				require.Equal(t, before, after)
			} else {
				require.NoError(t, d.InsertMessages(ctx, []db.Message{{SessionID: "session", Ordinal: 3, Role: "assistant", Content: "second", ToolCalls: []db.ToolCall{{ToolName: "Read", ResultContent: png}}}}))
			}
			require.NoError(t, manager.Run(ctx))
			require.Equal(t, 1, requests)
			var succeeded int
			require.NoError(t, d.Reader().QueryRow(ctx, `SELECT succeeded FROM image_text_cache`).Scan(&succeeded))
			require.Equal(t, 1, succeeded)
		})
	}
}
