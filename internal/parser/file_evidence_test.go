package parser

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/fileevidence"
)

const evidenceImage = `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"YWxwaGEK"}}`

func TestFileEvidenceNativeAttachments(t *testing.T) {
	ms := nativeClaudeEvidence(t, `{"type":"user","uuid":"u","sessionId":"evidence","message":{"role":"user","content":[{"type":"text","text":"inspect"},`+evidenceImage+`]}}`)
	require.Len(t, ms, 1)
	require.Len(t, ms[0].FileEvidence, 1)
	assert.Equal(t, "b6a98d9ce9a2d9149288fa3df42d377c3e42737afdcdaf714e33c0a100b51060", ms[0].FileEvidence[0].Record.SHA256)
	assert.Equal(t, fileevidence.Position{Entry: "u", Call: -1, ResultEntry: "", Result: -1, Block: 1}, ms[0].FileEvidence[0].Record.Position)
	for _, url := range []string{"data:image/png;base64,YWxwaGEK", "https://example.test/image.png", "file:///workspace/image.png"} {
		_, ms := runCodexParserTest(t, "evidence.jsonl", `{"type":"session_meta","payload":{"id":"evidence","cwd":"/workspace"}}`+"\n"+fmt.Sprintf(`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"inspect"},{"type":"input_image","image_url":%q}]}}`, url), false)
		require.Len(t, ms, 1)
		require.Len(t, ms[0].FileEvidence, 1)
		if strings.HasPrefix(url, "data:") {
			assert.Equal(t, "b6a98d9ce9a2d9149288fa3df42d377c3e42737afdcdaf714e33c0a100b51060", ms[0].FileEvidence[0].Record.SHA256)
		} else {
			assert.Empty(t, ms[0].FileEvidence[0].Record.SHA256)
		}
	}
	ms = piEvidenceParse(t, `{"type":"message","id":"u","parentId":null,"message":{"role":"user","content":[{"type":"image","mimeType":"image/png","data":"YWxwaGEK"}]}}`, AgentPi)
	require.Len(t, ms, 1)
	require.Len(t, ms[0].FileEvidence, 1)
	assert.Equal(t, "alpha\n", string(ms[0].FileEvidence[0].Payload))
}

func TestFileEvidenceCodexIncrementalStringImageOutput(t *testing.T) {
	prefix := `{"type":"session_meta","payload":{"id":"evidence","cwd":"/workspace"}}` + "\n" +
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"inspect"}]}}` + "\n"
	for name, block := range map[string]string{
		"input image":  `{"type":"input_image","image_url":"data:image/png;base64,YWxwaGEK"}`,
		"source image": evidenceImage,
		"data image":   `{"type":"image","mimeType":"image/png","data":"YWxwaGEK"}`,
		"document":     `{"type":"document","source":{"type":"base64","media_type":"text/plain","data":"YWxwaGEK"}}`,
		"owned":        `{"type":"agentsview_image","media_type":"image/png","image_ref":"asset://b6a98d9ce9a2d9149288fa3df42d377c3e42737afdcdaf714e33c0a100b51060.png","byte_size":6}`,
	} {
		for _, encoded := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/string=%t", name, encoded), func(t *testing.T) {
				output := "[" + block + "]"
				if encoded {
					output = strconv.Quote(output)
				}
				// The call arrives in the append too, bypassing unknown-call fallback.
				appended := `{"type":"response_item","payload":{"type":"function_call","call_id":"t","name":"view_image","arguments":"{}"}}` + "\n" +
					fmt.Sprintf(`{"type":"response_item","payload":{"type":"function_call_output","call_id":"t","output":%s}}`, output) + "\n"
				path := createTestFile(t, "evidence.jsonl", prefix+appended)
				_, _, _, err := parseCodexTestSessionFrom(t, path, int64(len(prefix)), 1, false)
				require.ErrorIs(t, err, errCodexIncrementalNeedsFullParse)
				_, messages := runCodexParserTest(t, "evidence.jsonl", prefix+appended, false)
				require.Len(t, messages, 2)
				require.Len(t, messages[1].FileEvidence, 1)
				want := "b6a98d9ce9a2d9149288fa3df42d377c3e42737afdcdaf714e33c0a100b51060"
				if name == "owned" {
					want = "" // A producer's asset reference does not establish bytes.
				}
				assert.Equal(t, want, messages[1].FileEvidence[0].Record.SHA256)
			})
		}
	}
}

type evidenceSeedReader struct {
	io.ReaderAt
	io.Reader
	atEOF func()
}

func (r evidenceSeedReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err == io.EOF {
		r.atEOF()
	}
	return n, err
}

func TestFileEvidenceCodexColdCursorMemory(t *testing.T) {
	measure := func(count int) int64 {
		f, err := os.Create(filepath.Join(t.TempDir(), "prefix.jsonl"))
		require.NoError(t, err)
		defer f.Close()
		encoded := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 1<<20)))
		for i := range count {
			_, err := fmt.Fprintf(f, `{"type":"response_item","payload":{"type":"function_call","call_id":"t%d","name":"view_image","arguments":"{}"}}`+"\n"+`{"type":"response_item","payload":{"type":"function_call_output","call_id":"t%d","output":[{"type":"input_image","image_url":"data:image/png;base64,%s"}]}}`+"\n", i, i, encoded)
			require.NoError(t, err)
		}
		_, err = f.Seek(0, io.SeekStart)
		require.NoError(t, err)
		runtime.GC()
		var before, during runtime.MemStats
		runtime.ReadMemStats(&before)
		seed, err := seedCodexIncrementalStateFromReader(t.Context(), AgentCodex, evidenceSeedReader{Reader: f, atEOF: func() {
			// Sample while the reconstruction builder is still live on its stack.
			runtime.GC()
			runtime.ReadMemStats(&during)
		}}, nil)
		require.NoError(t, err)
		runtime.KeepAlive(seed)
		return int64(during.HeapAlloc) - int64(before.HeapAlloc)
	}
	small, large := measure(2), measure(20)
	assert.Less(t, large-small, int64(8<<20), "cold cursor reconstruction must not retain attachment payloads")
}

func TestFileEvidenceOccurrenceLimit(t *testing.T) {
	content := `[` + strings.Repeat(evidenceImage+",", 64) + evidenceImage + `]`
	m := ParsedMessage{}
	captureFileEvidence(&m, content, "u")
	require.Len(t, m.FileEvidence, 64)
	for i, c := range m.FileEvidence {
		assert.Equal(t, i, c.Record.Position.Block)
	}
}

func TestFileEvidenceCodexToolOutputAndCompaction(t *testing.T) {
	lines := `{"type":"session_meta","payload":{"id":"evidence","cwd":"/workspace"}}` + "\n" + `{"type":"response_item","payload":{"type":"function_call","call_id":"t","name":"view_image","arguments":"{}"}}` + "\n" + `{"type":"response_item","payload":{"type":"function_call_output","call_id":"t","output":[{"type":"input_image","image_url":"data:image/png;base64,YWxwaGEK"}]}}` + "\n" + `{"type":"compacted","payload":{"replacement_history":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,YWxwaGEK"}]}]}}`
	_, ms := runCodexParserTest(t, "evidence.jsonl", lines, false)
	require.Len(t, ms, 1)
	require.Len(t, ms[0].FileEvidence, 1)
	assert.Equal(t, "b6a98d9ce9a2d9149288fa3df42d377c3e42737afdcdaf714e33c0a100b51060", ms[0].FileEvidence[0].Record.SHA256)
	assert.Equal(t, fileevidence.Position{Entry: "line:2", Call: 0, ResultEntry: "line:3", Result: 0, Block: 0}, ms[0].FileEvidence[0].Record.Position)
}

func TestFileEvidenceClaudeNativeProviderAndSharedCallers(t *testing.T) {
	prefix := `{"type":"user","uuid":"u","sessionId":"evidence","message":{"role":"user","content":"hello"}}` + "\n"
	line := `{"type":"user","uuid":"v","parentUuid":"u","sessionId":"evidence","message":{"role":"user","content":[` + evidenceImage + `]}}` + "\n"
	root := t.TempDir()
	path := filepath.Join(root, "project", "evidence.jsonl")
	writeSourceFile(t, path, prefix+line)
	provider, ok := NewProvider(AgentClaude, ProviderConfig{Roots: []string{root}, Machine: "local"})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	full, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	require.Len(t, full.Results, 1)
	appendResult, status, err := provider.ParseIncremental(t.Context(), IncrementalRequest{Source: sources[0], Offset: int64(len(prefix)), StartOrdinal: 1, LastEntryUUID: "u", Fingerprint: SourceFingerprint{Size: int64(len(prefix + line))}})
	require.NoError(t, err)
	require.Equal(t, IncrementalApplied, status)
	require.Len(t, appendResult.Messages, 1)
	require.Len(t, full.Results[0].Result.Messages[1].FileEvidence, 1)
	assert.Equal(t, full.Results[0].Result.Messages[1].FileEvidence, appendResult.Messages[0].FileEvidence)
	shared, _, err := claudeParseWithExclusions(path, "example", "local")
	require.NoError(t, err)
	require.Len(t, shared[0].Messages, 1)
	assert.Empty(t, shared[0].Messages[0].FileEvidence)
}

func nativeClaudeEvidence(t *testing.T, content string) []ParsedMessage {
	t.Helper()
	path := createTestFile(t, "evidence.jsonl", content)
	results, _, err := claudeParseFile(path, "example", "local", claudeParseOptions{ctx: nativeFileEvidenceContext(t.Context())})
	require.NoError(t, err)
	require.Len(t, results, 1)
	return results[0].Messages
}

func TestFileEvidenceAggregateLimit(t *testing.T) {
	m := ParsedMessage{}
	payload := make([]byte, fileevidence.MaxAttachmentBytes)
	for range 3 {
		appendFileEvidence(&m, fileevidence.Candidate{Record: fileevidence.Proof(payload), Payload: payload})
	}
	require.Len(t, m.FileEvidence, 3)
	require.NotNil(t, m.FileEvidence[1].Record.Size)
	assert.Nil(t, m.FileEvidence[2].Record.Size)
	assert.Empty(t, m.FileEvidence[2].Record.SHA256)
	assert.Empty(t, m.FileEvidence[2].Payload)
	assert.Equal(t, "message_limit", m.FileEvidence[2].Record.IneligibleReason)
}

func TestFileEvidenceTypedOwnedImageAndEditDescriptor(t *testing.T) {
	m := ParsedMessage{}
	captureFileEvidence(&m, `[{"type":"agentsview_image","image_ref":"asset://b6a98d9ce9a2d9149288fa3df42d377c3e42737afdcdaf714e33c0a100b51060.png","media_type":"image/png","byte_size":6},{"type":"tool_use","id":"edit","name":"Edit","input":{}}]`, "entry")
	require.Len(t, m.FileEvidence, 2)
	r := m.FileEvidence[0].Record
	assert.Equal(t, "image", r.Kind)
	assert.Equal(t, "image/png", r.MediaType)
	assert.Equal(t, "asset://b6a98d9ce9a2d9149288fa3df42d377c3e42737afdcdaf714e33c0a100b51060.png", r.AssetRef)
	require.NotNil(t, r.Size)
	assert.Equal(t, int64(6), *r.Size)
	assert.Empty(t, r.SHA256)
	assert.Equal(t, "unverified_asset", r.IneligibleReason)
	assert.Equal(t, "write", m.FileEvidence[1].Record.Kind)
}

func TestFileEvidenceCodexImagePromptAfterAbort(t *testing.T) {
	prompt := func(data string) string {
		return fmt.Sprintf(`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,%s"}]}}`, data)
	}
	prefix := `{"type":"session_meta","payload":{"id":"evidence","cwd":"/workspace"}}` + "\n" + prompt("YWxwaGEK") + "\n" + `{"type":"event_msg","payload":{"type":"turn_aborted"}}` + "\n"
	for _, tc := range []struct {
		name, data string
		count      int
	}{{"distinct", "YmV0YQo=", 2}, {"replay", "YWxwaGEK", 1}} {
		t.Run(tc.name, func(t *testing.T) {
			_, messages := runCodexParserTest(t, "evidence.jsonl", prefix+prompt(tc.data)+"\n", false)
			require.Len(t, messages, tc.count)
			require.Len(t, messages[tc.count-1].FileEvidence, 1)
			if tc.count == 2 {
				assert.Equal(t, "beta\n", string(messages[1].FileEvidence[0].Payload))
			}
		})
	}
	path := createTestFile(t, "cursor-images.jsonl", prefix)
	provider := newCodexTestProvider(t)
	_, _, err := provider.parseSession(path, "local", false)
	require.NoError(t, err)
	info, err := os.Stat(path)
	require.NoError(t, err)
	inode, device := sourceFileIdentityForPath(path, info)
	warm, ok := provider.cursorCache.Get(path, info.Size(), inode, device)
	require.True(t, ok)
	cold, err := provider.seedCodexIncrementalState(t.Context(), path, info.Size())
	require.NoError(t, err)
	assert.Equal(t, warm.firstUserDigest, cold.firstUserDigest)
	encoded, err := cold.MarshalBinary()
	require.NoError(t, err)
	var restored codexCursorState
	require.NoError(t, restored.UnmarshalBinary(encoded))
	for _, state := range []codexCursorState{warm, cold.codexCursorState, restored} {
		sink := NewCodexCollectingSink(1)
		builder := newCodexSessionBuilder(nativeFileEvidenceContext(t.Context()), AgentCodex, false, nil, sink)
		builder.codexCursorState = state
		_, err := builder.processLine(t.Context(), prompt("YmV0YQo="))
		require.NoError(t, err)
		require.Len(t, sink.messages, 1)
		require.Len(t, sink.messages[0].FileEvidence, 1)
		assert.Equal(t, "beta\n", string(sink.messages[0].FileEvidence[0].Payload))
	}
}
