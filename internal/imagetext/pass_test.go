package imagetext

import (
	"database/sql"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/assets"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/recall/extract"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/glmocr"
)

func TestImageTextPassAndSearch(t *testing.T) {
	ctx := t.Context()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	assetDir := t.TempDir()
	d.SetAssetsDir(assetDir)
	png := []byte("\x89PNG\r\n\x1a\nsynthetic image")
	inline := fmt.Sprintf(`[{"type":"input_image","image_url":"data:image/png;base64,%s"}]`, base64.StdEncoding.EncodeToString(png))
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"model":"glm-ocr","deployment_fingerprint":%q,"json_result":[[{"index":0,"label":"text","content":"invoice 42 key AKIA7QHWN2DKR4FYPLJM"}]]}`, glmocr.DefaultDeploymentFingerprint)
	}))
	t.Cleanup(srv.Close)
	policy, err := (config.ImageTextConfig{Enabled: true, Endpoint: srv.URL + "/glmocr/parse"}).Policy()
	require.NoError(t, err)
	client, err := glmocr.NewClient(policy, glmocr.ClientConfig{})
	require.NoError(t, err)
	manager := Manager{DB: d, Client: client}
	add := func(id, content string) {
		t.Helper()
		require.NoError(t, d.UpsertSession(ctx, db.Session{ID: id, Project: "demo", Agent: "test", MessageCount: 1}))
		require.NoError(t, d.InsertMessages(ctx, []db.Message{{SessionID: id, Ordinal: 3, Role: "assistant", Content: "answer", ToolCalls: []db.ToolCall{{ToolName: "Read", Category: "Read", ToolUseID: "call", ResultContent: content, ResultEvents: []db.ToolResultEvent{{ToolUseID: "call", Source: "tool", Status: "completed", Content: content}}}}}}))
	}
	add("one", inline)
	_, _, err = manager.TryPass(ctx, extract.PassOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, requests)
	search, err := d.Search(ctx, db.SearchFilter{Query: "invoice"})
	require.NoError(t, err)
	require.Len(t, search.Results, 1)
	require.Equal(t, "one", search.Results[0].SessionID)
	require.Equal(t, 3, search.Results[0].Ordinal)
	require.Equal(t, "image_text", search.Results[0].MatchKind)
	require.Contains(t, search.Results[0].Snippet, "<mark>invoice</mark>")
	require.NoError(t, d.InsertMessages(ctx, []db.Message{{SessionID: "one", Ordinal: 4, Role: "assistant", Content: "later"}}))
	search, err = d.Search(ctx, db.SearchFilter{Query: "invoice"})
	require.NoError(t, err)
	require.Len(t, search.Results, 1)
	require.Equal(t, 3, search.Results[0].Ordinal)
	for _, mode := range []string{"substring", "regex"} {
		page, err := d.SearchContent(ctx, db.ContentSearchFilter{Pattern: "invoice 42", Mode: mode, Sources: []string{"tool_result"}, IncludeOneShot: true, IncludeAutomated: true})
		require.NoError(t, err)
		require.Len(t, page.Matches, 1)
		require.Equal(t, "one", page.Matches[0].SessionID)
		require.Equal(t, 3, page.Matches[0].Ordinal)
		require.NotContains(t, page.Matches[0].Snippet, "AKIA7QHWN2DKR4FYPLJM")
	}
	ref, _, err := assets.Put(assetDir, "image/png", png)
	require.NoError(t, err)
	offload := fmt.Sprintf(`[{"type":"agentsview_image","version":1,"image_ref":%q,"media_type":"image/png","byte_size":%d}]`, ref, len(png))
	add("two", offload)
	_, _, err = manager.TryPass(ctx, extract.PassOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, requests)
	name := "invoice"
	require.NoError(t, d.UpsertSession(ctx, db.Session{ID: "name", Project: "demo", Agent: "test", DisplayName: &name, MessageCount: 1}))
	require.NoError(t, d.InsertMessages(ctx, []db.Message{{SessionID: "name", Role: "assistant", Content: "answer"}}))
	require.NoError(t, d.RenameSession(ctx, "name", &name))
	require.NoError(t, d.UpsertSession(ctx, db.Session{ID: "message", Project: "demo", Agent: "test", MessageCount: 1}))
	require.NoError(t, d.InsertMessages(ctx, []db.Message{{SessionID: "message", Role: "assistant", Content: "invoice"}}))
	search, err = d.Search(ctx, db.SearchFilter{Query: "invoice", Limit: 1})
	require.NoError(t, err)
	require.Equal(t, "message", search.Results[0].SessionID)
	search, err = d.Search(ctx, db.SearchFilter{Query: "invoice", Limit: 1, Cursor: 1})
	require.NoError(t, err)
	require.Equal(t, "image_text", search.Results[0].MatchKind)
	search, err = d.Search(ctx, db.SearchFilter{Query: "invoice", Limit: 1, Cursor: 3})
	require.NoError(t, err)
	require.Len(t, search.Results, 1)
	require.Equal(t, "name", search.Results[0].SessionID)
	page, err := d.SearchContent(ctx, db.ContentSearchFilter{Pattern: "invoice 42", Sources: []string{"tool_result"}, IncludeOneShot: true, IncludeAutomated: true})
	require.NoError(t, err)
	require.Len(t, page.Matches, 2)
	page, err = d.SearchContent(ctx, db.ContentSearchFilter{Pattern: "invoice", Sources: []string{"tool_result"}, IncludeOneShot: true, IncludeAutomated: true, Project: "missing"})
	require.NoError(t, err)
	require.Empty(t, page.Matches)
	page, err = d.SearchContent(ctx, db.ContentSearchFilter{Pattern: "invoice", Sources: []string{"tool_result"}, IncludeOneShot: true, IncludeAutomated: true, Limit: 1})
	require.NoError(t, err)
	require.Len(t, page.Matches, 1)
	require.NotZero(t, page.NextCursor)
	require.NoError(t, d.ReplaceSessionMessages(ctx, "one", []db.Message{{SessionID: "one", Ordinal: 3, Role: "assistant", Content: "image removed"}}))
	_, _, err = manager.TryPass(ctx, extract.PassOptions{})
	require.NoError(t, err)
	page, err = d.SearchContent(ctx, db.ContentSearchFilter{Pattern: "invoice", Sources: []string{"tool_result"}, IncludeOneShot: true, IncludeAutomated: true})
	require.NoError(t, err)
	require.Len(t, page.Matches, 1)
	require.Equal(t, "two", page.Matches[0].SessionID)
}

func TestImageTextFailureAndRetry(t *testing.T) {
	ctx := t.Context()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		if requests == 1 {
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
	png := []byte("\x89PNG\r\n\x1a\nsynthetic")
	content := fmt.Sprintf(`[{"type":"file","uri":"data:image/png;base64,%s"},{"type":"input_image","image_url":"data:image/gif;base64,R0lGODlh"}]`, base64.StdEncoding.EncodeToString(png))
	require.NoError(t, d.UpsertSession(ctx, db.Session{ID: "one", Project: "demo", Agent: "test", MessageCount: 1}))
	require.NoError(t, d.InsertMessages(ctx, []db.Message{{SessionID: "one", Ordinal: 1, Role: "assistant", Content: "answer", ToolCalls: []db.ToolCall{{ToolName: "Read", Category: "Read", ResultContent: content}}}}))
	_, _, err = manager.TryPass(ctx, extract.PassOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, requests)
	_, _, err = manager.TryPass(ctx, extract.PassOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, requests)
	var failures int
	require.NoError(t, d.Reader().QueryRow(ctx, `SELECT failures FROM image_text_cache`).Scan(&failures))
	require.Equal(t, 1, failures)
	raw, err := sql.Open("sqlite3", d.Path())
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, `UPDATE image_text_cache SET retry_at='2000-01-01T00:00:00Z'`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())
	_, _, err = manager.TryPass(ctx, extract.PassOptions{})
	require.NoError(t, err)
	require.Equal(t, 2, requests)
	page, err := d.SearchContent(ctx, db.ContentSearchFilter{Pattern: "retry succeeded", Sources: []string{"tool_result"}, IncludeOneShot: true, IncludeAutomated: true})
	require.NoError(t, err)
	require.Len(t, page.Matches, 1)
}

func TestImageTextProcessorKey(t *testing.T) {
	first, err := (config.ImageTextConfig{Enabled: true, Endpoint: "http://127.0.0.1:30004/glmocr/parse"}).Policy()
	require.NoError(t, err)
	second, err := (config.ImageTextConfig{Enabled: true, Endpoint: "http://127.0.0.1:30005/glmocr/parse"}).Policy()
	require.NoError(t, err)
	firstKey, err := ProcessorKey(first)
	require.NoError(t, err)
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

func TestImageTextPassDrainsPagesAndRebuildsFTS(t *testing.T) {
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
	_, _, err = manager.TryPass(ctx, extract.PassOptions{})
	require.NoError(t, err)
	require.Equal(t, 21, requests)
	require.NoError(t, d.DropFTS(ctx))
	require.NoError(t, d.RebuildFTS(ctx))
	page, err := d.Search(ctx, db.SearchFilter{Query: "invoice", Limit: 10, Sort: "recency"})
	require.NoError(t, err)
	require.Len(t, page.Results, 10)
	require.Equal(t, 10, page.NextCursor)
}

func TestImageTextRemovedDuringOCR(t *testing.T) {
	ctx := t.Context()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	started, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		fmt.Fprintf(w, `{"model":"glm-ocr","deployment_fingerprint":%q,"json_result":[[{"index":0,"label":"text","content":"invoice"}]]}`, glmocr.DefaultDeploymentFingerprint)
	}))
	t.Cleanup(srv.Close)
	policy, err := (config.ImageTextConfig{Enabled: true, Endpoint: srv.URL + "/glmocr/parse"}).Policy()
	require.NoError(t, err)
	client, err := glmocr.NewClient(policy, glmocr.ClientConfig{})
	require.NoError(t, err)
	content := `[{"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgp4"}]`
	require.NoError(t, d.UpsertSession(ctx, db.Session{ID: "one", Project: "demo", Agent: "test", MessageCount: 1}))
	require.NoError(t, d.InsertMessages(ctx, []db.Message{{SessionID: "one", Role: "assistant", Content: "answer", ToolCalls: []db.ToolCall{{ToolName: "Read", ResultContent: content}}}}))
	manager := Manager{DB: d, Client: client}
	done := make(chan error, 1)
	go func() { _, _, err := manager.TryPass(ctx, extract.PassOptions{}); done <- err }()
	<-started
	require.NoError(t, d.ReplaceSessionMessages(ctx, "one", []db.Message{{SessionID: "one", Role: "assistant", Content: "removed"}}))
	close(release)
	require.NoError(t, <-done)
	var count int
	require.NoError(t, d.Reader().QueryRow(ctx, `SELECT count(*) FROM image_text_cache`).Scan(&count))
	require.Zero(t, count)
}
