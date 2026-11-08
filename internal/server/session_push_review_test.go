package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

// Mutate valid wire bytes without calling the decoder/guard under test.
func sessionPushResultPacket(t *testing.T, valid []byte, content string, event bool) []byte {
	t.Helper()
	var envelope artifact.SessionPushEnvelope
	require.NoError(t, json.Unmarshal(valid, &envelope))
	var segments []struct {
		SHA256 string `json:"sha256"`
		NDJSON string `json:"ndjson"`
	}
	require.NoError(t, json.Unmarshal(envelope.Segments, &segments))
	require.Len(t, segments, 1)
	var message map[string]any
	require.NoError(t, json.Unmarshal([]byte(segments[0].NDJSON), &message))
	call := message["tool_calls"].([]any)[0].(map[string]any)
	if event {
		result := call["result_events"].([]any)[0].(map[string]any)
		result["content"], result["content_length"] = content, len(content)
	} else {
		call["result_content"], call["result_content_length"] = content, len(content)
	}
	body, err := json.Marshal(message)
	require.NoError(t, err)
	digest := sha256.Sum256(body)
	segments[0].NDJSON, segments[0].SHA256 = string(body), hex.EncodeToString(digest[:])
	var manifest map[string]any
	require.NoError(t, json.Unmarshal(envelope.Manifest, &manifest))
	manifest["segments"] = []string{segments[0].SHA256}
	envelope.Manifest, err = json.Marshal(manifest)
	require.NoError(t, err)
	envelope.Segments, err = json.Marshal(segments)
	require.NoError(t, err)
	packet, err := json.Marshal(envelope)
	require.NoError(t, err)
	return packet
}

func TestSessionPushNormalizedCollectionsRejectAtomically(t *testing.T) {
	source := testServer(t, 30*time.Second)
	d := source.db.(*db.DB)
	_, err := d.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{
		Session:         db.Session{ID: "selected", Agent: "claude", Machine: "source", MessageCount: 1},
		Messages:        []db.Message{{SessionID: "selected", Ordinal: 0, Role: "assistant", ToolCalls: []db.ToolCall{{ToolName: "Read", ResultContent: "summary", ResultEvents: []db.ToolResultEvent{{EventIndex: 0, Content: "event"}}}}}},
		ReplaceMessages: true,
	}})
	require.NoError(t, err)
	valid, err := artifact.EncodeSessionPush(t.Context(), d, "selected", "")
	require.NoError(t, err)
	fields := make([]string, 257)
	for i := range fields {
		fields[i] = fmt.Sprintf(`"field%d":0`, i)
	}
	array := "[" + strings.Repeat("{},", 4096) + "{}]"
	for name, content := range map[string]string{
		"root-object":        "{" + strings.Join(fields, ",") + "}",
		"nul-array":          "\x00" + array,
		"controls-array":     "\x01\x7f\u0085" + array,
		"normalized-section": "\x00worker:\n" + array,
	} {
		for _, event := range []bool{false, true} {
			for _, policy := range []config.ToolResultImages{config.ToolResultImagesKeep, config.ToolResultImagesDrop, config.ToolResultImagesOffload} {
				t.Run(fmt.Sprintf("%s/event=%t/policy=%s", name, event, policy), func(t *testing.T) {
					s := testServer(t, 30*time.Second)
					s.cfg.AcceptSessionPush, s.cfg.RequireAuth, s.cfg.AuthToken = true, true, "fixture"
					receiver := s.db.(*db.DB)
					assets := t.TempDir()
					receiver.SetAssetsDir(assets)
					receiver.SetToolResultImages(policy)
					request := func(packet []byte) *httptest.ResponseRecorder {
						r := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
							"/api/v1/sessions/ingest", bytes.NewReader(packet))
						r.Header.Set("Authorization", "Bearer fixture")
						r.Header.Set("Content-Type", "application/json")
						w := httptest.NewRecorder()
						s.Handler().ServeHTTP(w, r)
						return w
					}
					w := request(sessionPushResultPacket(t, valid, content, event))
					assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
					stored, err := receiver.GetSessionFull(t.Context(), "selected")
					require.NoError(t, err)
					assert.Nil(t, stored)
					files, err := os.ReadDir(assets)
					require.NoError(t, err)
					assert.Empty(t, files)
					w = request(valid)
					assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
					assert.Contains(t, w.Body.String(), `"status":"created"`, "rejection must leave no receipt")
				})
			}
		}
	}
}
