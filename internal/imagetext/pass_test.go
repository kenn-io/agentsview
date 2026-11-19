package imagetext

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/assets"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/glmocr"
)

// localOCRClient answers OCR requests in process, including inside a synctest bubble.
func localOCRClient(t *testing.T, handler http.HandlerFunc) *glmocr.Client {
	t.Helper()
	policy, err := (config.ImageTextConfig{Enabled: true}).Policy()
	require.NoError(t, err)
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		handler(recorder, r)
		return recorder.Result(), nil
	})
	client, err := glmocr.NewClient(policy, glmocr.ClientConfig{HTTPClient: &http.Client{Transport: transport}})
	require.NoError(t, err)
	return client
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func imageTextTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	return d
}

func TestImageTextPassStopsOnServiceError(t *testing.T) {
	ctx := t.Context()
	d := imageTextTestDB(t)
	require.NoError(t, d.UpsertSession(ctx, db.Session{ID: "one", Project: "demo", Agent: "test"}))
	for i := range 3 {
		content := fmt.Sprintf(`[{"type":"input_image","image_url":"data:image/png;base64,%s"}]`, base64.StdEncoding.EncodeToString(fmt.Appendf(nil, "\x89PNG\r\n\x1a\nimage%d", i)))
		require.NoError(t, d.InsertMessages(ctx, []db.Message{{SessionID: "one", Ordinal: i, Role: "assistant", ToolCalls: []db.ToolCall{{ToolName: "Read", ResultContent: content}}}}))
	}
	requests := 0
	client := localOCRClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		if requests == 1 {
			fmt.Fprintf(w, `{"model":"glm-ocr","deployment_fingerprint":%q,"json_result":[[]]}`, glmocr.DefaultDeploymentFingerprint)
			return
		}
		fmt.Fprintf(w, `{"model":"glm-ocr","deployment_fingerprint":%q,"json_result":[[{"index":0,"label":"text","content":"recovered"}]]}`, glmocr.DefaultDeploymentFingerprint)
	})
	require.Error(t, (&Manager{DB: d, Client: client}).Run(ctx))
	require.Equal(t, 1, requests)
	var failures int
	require.NoError(t, d.Reader().QueryRow(ctx, `SELECT count(*) FROM image_text_cache WHERE failures > 0`).Scan(&failures))
	require.Equal(t, 1, failures)
	require.NoError(t, (&Manager{DB: d, Client: client}).Run(ctx))
	require.Equal(t, 3, requests)
	require.NoError(t, (&Manager{DB: d, Client: client}).Run(ctx))
	require.Equal(t, 3, requests)
	require.NoError(t, d.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE image_text_cache SET retry_at='2000-01-01T00:00:00Z' WHERE failures>0`)
		return err
	}))
	require.NoError(t, (&Manager{DB: d, Client: client}).Run(ctx))
	require.Equal(t, 4, requests)
}

func TestImageTextPassDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		d := imageTextTestDB(t)
		client := localOCRClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			select {
			case <-time.After(70 * time.Second):
				fmt.Fprintf(w, `{"model":"glm-ocr","deployment_fingerprint":%q,"json_result":[[{"index":0,"label":"text","content":"example"}]]}`, glmocr.DefaultDeploymentFingerprint)
			case <-r.Context().Done():
			}
		})
		require.NoError(t, d.UpsertSession(ctx, db.Session{ID: "one", Project: "demo", Agent: "test"}))
		for i := range 22 {
			content := fmt.Sprintf(`[{"type":"file","uri":"data:image/png;base64,%s"}]`, base64.StdEncoding.EncodeToString(fmt.Appendf(nil, "\x89PNG\r\n\x1a\nimage%d", i)))
			require.NoError(t, d.InsertMessages(ctx, []db.Message{{SessionID: "one", Ordinal: i, Role: "assistant", ToolCalls: []db.ToolCall{{ToolName: "Read", ResultContent: content}}}}))
		}
		start := time.Now()
		require.NoError(t, (&Manager{DB: d, Client: client}).Run(ctx))
		require.Equal(t, 140*time.Second, time.Since(start))
		pending, err := d.PendingImageText(ctx, client.Identity().Fingerprint, time.Now(), 30)
		require.NoError(t, err)
		require.Len(t, pending, 20)
		var failures int
		require.NoError(t, d.Reader().QueryRow(ctx, `SELECT count(*) FROM image_text_cache WHERE failures > 0`).Scan(&failures))
		require.Zero(t, failures)
		var succeeded int
		require.NoError(t, d.Reader().QueryRow(ctx, `SELECT count(*) FROM image_text_cache WHERE succeeded=1`).Scan(&succeeded))
		require.Equal(t, 2, succeeded)
		require.NoError(t, (&Manager{DB: d, Client: client}).Run(ctx))
		require.NoError(t, d.Reader().QueryRow(ctx, `SELECT count(*) FROM image_text_cache WHERE succeeded=1`).Scan(&succeeded))
		require.Equal(t, 4, succeeded)
	})
}

func TestImageTextTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		d := imageTextTestDB(t)
		for _, id := range []string{"fast", "slow"} {
			body := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n" + id))
			content := fmt.Sprintf(`[{"type":"input_image","image_url":"data:image/png;base64,%s"}]`, body)
			require.NoError(t, d.UpsertSession(ctx, db.Session{ID: id, Project: "demo", Agent: "test"}))
			require.NoError(t, d.InsertMessages(ctx, []db.Message{{SessionID: id, Role: "assistant", ToolCalls: []db.ToolCall{{ToolName: "Read", ResultContent: content}}}}))
		}
		slow := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nslow"))
		client := localOCRClient(t, func(w http.ResponseWriter, r *http.Request) {
			request, _ := io.ReadAll(r.Body)
			if strings.Contains(string(request), slow) {
				<-r.Context().Done()
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"model":"glm-ocr","deployment_fingerprint":%q,"json_result":[[{"index":0,"label":"text","content":"invoice"}]]}`, glmocr.DefaultDeploymentFingerprint)
		})
		manager := Manager{DB: d, Client: client}
		require.Error(t, manager.Run(ctx))
		var failures int
		content := fmt.Sprintf(`[{"type":"input_image","image_url":"data:image/png;base64,%s"}]`, slow)
		hash := db.RetainedToolResultImages(content)[0].SHA256
		require.NoError(t, d.Reader().QueryRow(ctx, `SELECT failures FROM image_text_cache WHERE image_hash=?`, hash).Scan(&failures))
		require.Equal(t, 1, failures)
		require.NoError(t, manager.Run(ctx))
		page, err := d.Search(ctx, db.SearchFilter{Query: "invoice"})
		require.NoError(t, err)
		require.Len(t, page.Results, 1)
		require.Equal(t, "fast", page.Results[0].SessionID)
	})
}

func TestImageTextPassKeepsRetryCutoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		d := imageTextTestDB(t)
		requests := 0
		client := localOCRClient(t, func(w http.ResponseWriter, r *http.Request) {
			requests++
			w.Header().Set("Content-Type", "application/json")
			time.Sleep(2500 * time.Millisecond)
			fmt.Fprintf(w, `{"model":"glm-ocr","deployment_fingerprint":%q,"json_result":[[{"index":0,"label":"text","content":"example"}]]}`, glmocr.DefaultDeploymentFingerprint)
		})
		require.NoError(t, d.UpsertSession(ctx, db.Session{ID: "one", Project: "demo", Agent: "test"}))
		var hashes []string
		for i := range 2 {
			content := fmt.Sprintf(`[{"type":"file","uri":"data:image/png;base64,%s"}]`, base64.StdEncoding.EncodeToString(fmt.Appendf(nil, "\x89PNG\r\n\x1a\nimage%d", i)))
			hashes = append(hashes, db.RetainedToolResultImages(content)[0].SHA256)
			require.NoError(t, d.InsertMessages(ctx, []db.Message{{SessionID: "one", Ordinal: i, Role: "assistant", ToolCalls: []db.ToolCall{{ToolName: "Read", ResultContent: content}}}}))
		}
		require.NoError(t, d.BackfillImageTextOccurrences(ctx))
		require.NoError(t, d.SaveImageText(ctx, hashes[0], client.Identity(), "", true))
		require.NoError(t, d.Update(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE image_text_cache SET retry_at=? WHERE image_hash=?`, time.Now().UTC().Add(time.Second).Format(time.RFC3339), hashes[0])
			return err
		}))
		require.NoError(t, (&Manager{DB: d, Client: client}).Run(ctx))
		require.Equal(t, 1, requests)
		pending, err := d.PendingImageText(ctx, client.Identity().Fingerprint, time.Now(), 20)
		require.NoError(t, err)
		require.Equal(t, []string{hashes[0]}, pending)
	})
}

func TestImageTextCorruptAssetRetriesAfterRepair(t *testing.T) {
	var logs bytes.Buffer
	writer := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(writer) })
	ctx := t.Context()
	d := imageTextTestDB(t)
	dir := t.TempDir()
	d.SetAssetsDir(dir)
	png := []byte("\x89PNG\r\n\x1a\nimage")
	ref, _, err := assets.Put(dir, "image/png", png)
	require.NoError(t, err)
	path := filepath.Join(dir, strings.TrimPrefix(ref, "asset://"))
	corrupt := append([]byte(nil), png...)
	corrupt[len(corrupt)-1] ^= 1
	require.NoError(t, os.WriteFile(path, corrupt, 0o600))
	content := fmt.Sprintf(`[{"type":"agentsview_image","version":1,"image_ref":%q,"media_type":"image/png","byte_size":%d}]`, ref, len(png))
	require.NoError(t, d.UpsertSession(ctx, db.Session{ID: "one", Project: "demo", Agent: "test", MessageCount: 1}))
	require.NoError(t, d.InsertMessages(ctx, []db.Message{{SessionID: "one", Role: "assistant", ToolCalls: []db.ToolCall{{ToolName: "Read", ResultContent: content}}}}))
	healthy := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nhealthy"))
	require.NoError(t, d.UpsertSession(ctx, db.Session{ID: "healthy", Project: "demo", Agent: "test"}))
	require.NoError(t, d.InsertMessages(ctx, []db.Message{{SessionID: "healthy", Role: "assistant", ToolCalls: []db.ToolCall{{ToolName: "Read", ResultContent: fmt.Sprintf(`[{"type":"input_image","image_url":"data:image/png;base64,%s"}]`, healthy)}}}}))
	requests := 0
	client := localOCRClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"model":"glm-ocr","deployment_fingerprint":%q,"json_result":[[{"index":0,"label":"text","content":"retry succeeded"}]]}`, glmocr.DefaultDeploymentFingerprint)
	})
	manager := Manager{DB: d, Client: client}
	require.NoError(t, manager.Run(ctx))
	require.Contains(t, logs.String(), "no readable copy")
	require.Contains(t, logs.String(), "content hash mismatch")
	require.Equal(t, 1, requests)
	var failures int
	var retryAt string
	hash := db.RetainedToolResultImages(content)[0].SHA256
	require.NoError(t, d.Reader().QueryRow(ctx, `SELECT failures, retry_at FROM image_text_cache WHERE image_hash=?`, hash).Scan(&failures, &retryAt))
	require.Equal(t, 1, failures)
	retry, err := time.Parse(time.RFC3339, retryAt)
	require.NoError(t, err)
	require.True(t, retry.After(time.Now()))
	require.NoError(t, os.WriteFile(path, png, 0o600))
	require.NoError(t, d.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE image_text_cache SET retry_at='2000-01-01T00:00:00Z' WHERE image_hash=?`, hash)
		return err
	}))
	require.NoError(t, manager.Run(ctx))
	require.Equal(t, 2, requests)
	var text string
	require.NoError(t, d.Reader().QueryRow(ctx, `SELECT text FROM image_text_cache WHERE image_hash=?`, hash).Scan(&text))
	require.Equal(t, "retry succeeded", text)
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

func TestImageTextPassReadsImagesFromOneResult(t *testing.T) {
	for _, tc := range []struct {
		name     string
		count    int
		repeated bool
	}{{"multiple", 40, false}, {"repeated", 20, true}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			d := imageTextTestDB(t)
			var blocks []string
			want := make(map[string]bool)
			for i := range tc.count {
				if tc.repeated {
					i = 0
				}
				image := base64.StdEncoding.EncodeToString(fmt.Appendf(nil, "\x89PNG\r\n\x1a\nimage%d%s", i, strings.Repeat("x", 16384)))
				want[image] = true
				blocks = append(blocks, fmt.Sprintf(`{"type":"input_image","image_url":"data:image/png;base64,%s"}`, image))
			}
			content := "[" + strings.Join(blocks, ",") + "]"
			require.NoError(t, d.UpsertSession(ctx, db.Session{ID: "images", Project: "demo", Agent: "test", MessageCount: 1}))
			require.NoError(t, d.InsertMessages(ctx, []db.Message{{SessionID: "images", Ordinal: 1, Role: "assistant", Content: "answer", ToolCalls: []db.ToolCall{{ToolName: "Read", ResultContent: content}}}}))
			seen := make(map[string]bool)
			requests := 0
			var requestErr error
			client := localOCRClient(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				var request struct {
					Images []string `json:"images"`
				}
				requestErr = json.UnmarshalRead(r.Body, &request)
				if len(request.Images) == 1 {
					seen[strings.TrimPrefix(request.Images[0], "data:image/png;base64,")] = true
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"model":"glm-ocr","deployment_fingerprint":%q,"json_result":[[{"index":0,"label":"text","content":"invoice"}]]}`, glmocr.DefaultDeploymentFingerprint)
			})
			manager := Manager{DB: d, Client: client}
			require.NoError(t, manager.Run(ctx))
			require.NoError(t, requestErr)
			require.Equal(t, want, seen)
			require.Equal(t, len(want), requests)
			require.NoError(t, manager.Run(ctx))
			require.Equal(t, len(want), requests)
		})
	}
}

func TestImageTextFallsBackToReadableCopy(t *testing.T) {
	ctx := t.Context()
	d := imageTextTestDB(t)
	assetDir := t.TempDir()
	d.SetAssetsDir(assetDir)
	png := []byte("\x89PNG\r\n\x1a\nimage")
	ref, _, err := assets.Put(assetDir, "image/png", png)
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(assetDir, strings.TrimPrefix(ref, "asset://"))))
	offloaded := fmt.Sprintf(`[{"type":"agentsview_image","version":1,"image_ref":%q,"media_type":"image/png","byte_size":%d}]`, ref, len(png))
	inline := fmt.Sprintf(`[{"type":"input_image","image_url":"data:image/png;base64,%s"}]`, base64.StdEncoding.EncodeToString(png))
	require.NoError(t, d.UpsertSession(ctx, db.Session{ID: "same", Project: "demo", Agent: "test", MessageCount: 2}))
	require.NoError(t, d.InsertMessages(ctx, []db.Message{
		{SessionID: "same", Ordinal: 0, Role: "assistant", ToolCalls: []db.ToolCall{{ToolName: "Read", ResultContent: offloaded}}},
		{SessionID: "same", Ordinal: 1, Role: "assistant", ToolCalls: []db.ToolCall{{ToolName: "Read", ResultContent: inline}}},
	}))
	requests := 0
	client := localOCRClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"model":"glm-ocr","deployment_fingerprint":%q,"json_result":[[{"index":0,"label":"text","content":"invoice"}]]}`, glmocr.DefaultDeploymentFingerprint)
	})
	require.NoError(t, (&Manager{DB: d, Client: client}).Run(ctx))
	require.Equal(t, 1, requests)
}

func TestImageTextCachesEmptyNormalizedOutput(t *testing.T) {
	ctx := t.Context()
	d := imageTextTestDB(t)
	png := []byte("\x89PNG\r\n\x1a\nimage")
	content := fmt.Sprintf(`[{"type":"input_image","image_url":"data:image/png;base64,%s"}]`, base64.StdEncoding.EncodeToString(png))
	require.NoError(t, d.UpsertSession(ctx, db.Session{ID: "empty", Project: "demo", Agent: "test", MessageCount: 1}))
	require.NoError(t, d.InsertMessages(ctx, []db.Message{{SessionID: "empty", Role: "assistant", Content: "answer", ToolCalls: []db.ToolCall{{ToolName: "Read", ResultContent: content}}}}))
	requests := 0
	client := localOCRClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"model":"glm-ocr","deployment_fingerprint":%q,"json_result":[[{"index":0,"label":"text","content":"<script>hidden</script>"}]]}`, glmocr.DefaultDeploymentFingerprint)
	})
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

func TestImageTextUsesVerifiedBytes(t *testing.T) {
	for _, tc := range []struct {
		name, label string
		body        []byte
		mediaType   string
	}{
		{"mislabeled png", "image/gif", []byte("\x89PNG\r\n\x1a\nimage"), "image/png"},
		{"unsupported gif", "image/png", []byte("GIF89a\x01\x00\x01\x00"), ""},
		{"non-image", "image/png", []byte("plain text"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			d := imageTextTestDB(t)
			var sent []string
			client := localOCRClient(t, func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Images []string `json:"images"`
				}
				if err := json.UnmarshalRead(r.Body, &request); err != nil || len(request.Images) == 0 {
					http.Error(w, "bad request", http.StatusBadRequest)
					return
				}
				sent = append(sent, strings.SplitN(strings.TrimPrefix(request.Images[0], "data:"), ";", 2)[0])
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"model":"glm-ocr","deployment_fingerprint":%q,"json_result":[[{"index":0,"label":"text","content":"invoice"}]]}`, glmocr.DefaultDeploymentFingerprint)
			})
			content := fmt.Sprintf(`[{"type":"input_image","image_url":"data:%s;base64,%s"}]`, tc.label, base64.StdEncoding.EncodeToString(tc.body))
			require.NoError(t, d.UpsertSession(ctx, db.Session{ID: "session", Project: "demo", Agent: "test", MessageCount: 1}))
			require.NoError(t, d.InsertMessages(ctx, []db.Message{{SessionID: "session", Ordinal: 1, Role: "assistant", Content: "answer", ToolCalls: []db.ToolCall{{ToolName: "Read", ResultContent: content}}}}))
			manager := Manager{DB: d, Client: client}
			require.NoError(t, manager.Run(ctx))
			require.NoError(t, manager.Run(ctx))
			var succeeded int
			var retry string
			require.NoError(t, d.Reader().QueryRow(ctx, `SELECT succeeded,retry_at FROM image_text_cache`).Scan(&succeeded, &retry))
			if tc.mediaType == "" {
				require.Empty(t, sent)
				require.Equal(t, 1, succeeded)
				require.Empty(t, retry)
				return
			}
			require.Equal(t, []string{tc.mediaType}, sent)
			require.Equal(t, 1, succeeded)
		})
	}
}

func TestImageTextImageSizeLimit(t *testing.T) {
	var logs bytes.Buffer
	writer := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(writer) })
	for _, extra := range []int{0, 1} {
		t.Run(fmt.Sprintf("extra-%d", extra), func(t *testing.T) {
			ctx := t.Context()
			d := imageTextTestDB(t)
			assetsDir := t.TempDir()
			d.SetAssetsDir(assetsDir)
			body := make([]byte, glmocr.MaxDocumentBytes+int64(extra))
			copy(body, "\x89PNG\r\n\x1a\n")
			ref, _, err := assets.Put(assetsDir, "image/png", body)
			require.NoError(t, err)
			content := fmt.Sprintf(`[{"type":"agentsview_image","version":1,"image_ref":%q,"media_type":"image/png","byte_size":%d}]`, ref, len(body))
			require.NoError(t, d.UpsertSession(ctx, db.Session{ID: "large", Project: "demo", Agent: "test", MessageCount: 1}))
			require.NoError(t, d.InsertMessages(ctx, []db.Message{{SessionID: "large", Role: "assistant", ToolCalls: []db.ToolCall{{ToolName: "Read", ResultContent: content}}}}))
			requests := 0
			client := localOCRClient(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"model":"glm-ocr","deployment_fingerprint":%q,"json_result":[[{"index":0,"label":"text","content":"large image"}]]}`, glmocr.DefaultDeploymentFingerprint)
			})
			manager := Manager{DB: d, Client: client}
			require.NoError(t, manager.Run(ctx))
			var text, retry string
			var succeeded, failures int
			require.NoError(t, d.Reader().QueryRow(ctx, `SELECT text,succeeded,failures,retry_at FROM image_text_cache`).Scan(&text, &succeeded, &failures, &retry))
			require.Equal(t, 1, succeeded)
			require.Zero(t, failures)
			require.Empty(t, retry)
			if extra == 0 {
				require.Equal(t, "large image", text)
				require.Equal(t, 1, requests)
			} else {
				require.Empty(t, text)
				require.NoError(t, manager.Run(ctx))
				require.Zero(t, requests)
				require.Equal(t, 1, strings.Count(logs.String(), "exceeds 67108864 bytes"))
			}
		})
	}
}
