package parser

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	piEvidenceHeader = `{"type":"session","version":3,"id":"evidence","cwd":"/workspace"}`
	piEvidenceCall   = `{"type":"message","id":"a","parentId":null,"message":{"role":"assistant","content":[{"type":"toolCall","id":"t","name":"read","arguments":{"path":"example.txt"}}]}}`
	piEvidenceResult = `{"type":"message","id":"b","parentId":"a","message":{"role":"toolResult","toolCallId":"t","toolName":"read","content":[{"type":"text","text":""}],"structuredContent":"","isError":false}}`
)

func piEvidenceParse(t *testing.T, entries string, agent AgentType) []ParsedMessage {
	t.Helper()
	p := filepath.Join(t.TempDir(), "evidence.jsonl")
	require.NoError(t, os.WriteFile(p, []byte(piEvidenceHeader+"\n"+entries+"\n"), 0o600))
	_, ms, err := parsePiLikeSession(p, "", "", agent, "pi:")
	require.NoError(t, err)
	return ms
}

func TestPiFileEvidenceEmptyAndAncestry(t *testing.T) {
	ms := piEvidenceParse(t, piEvidenceCall+"\n"+piEvidenceResult, AgentPi)
	require.Len(t, ms, 2)
	require.Len(t, ms[0].FileEvidence, 1)
	r := ms[0].FileEvidence[0].Record
	require.NotNil(t, r.Size)
	assert.Zero(t, *r.Size)
	assert.Equal(t, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", r.SHA256)
	for name, entries := range map[string]string{
		"duplicate": piEvidenceCall + "\n" + piEvidenceResult + "\n" + piEvidenceResult,
		"sibling":   piEvidenceCall + "\n" + piEvidenceResult + "\n" + `{"type":"model_change","id":"c","parentId":"a"}`,
		"context":   piEvidenceCall + "\n" + piEvidenceResult + "\n" + `{"type":"context_edit","id":"c","parentId":"b"}`,
		"broken":    strings.Replace(piEvidenceCall, `"parentId":null`, `"parentId":"absent"`, 1) + "\n" + piEvidenceResult,
	} {
		t.Run(name, func(t *testing.T) {
			messages := piEvidenceParse(t, entries, AgentPi)
			if name == "broken" {
				require.Len(t, messages, 2)
				require.Len(t, messages[0].FileEvidence, 1)
			}
			for _, m := range messages {
				for _, c := range m.FileEvidence {
					assert.Empty(t, c.Record.SHA256)
				}
			}
		})
	}
	for _, m := range piEvidenceParse(t, piEvidenceCall+"\n"+piEvidenceResult, AgentOMP) {
		assert.Empty(t, m.FileEvidence)
	}
}

func TestPiFileEvidenceRejectsAmbiguousNativeResults(t *testing.T) {
	for name, change := range map[string]func(string) string{
		"offset": func(s string) string {
			return strings.Replace(s, `"path":"example.txt"`, `"path":"example.txt","offset":1`, 1)
		},
		"numeric call ID": func(s string) string { return strings.Replace(s, `"id":"t"`, `"id":1`, 1) },
		"missing success": func(s string) string { return strings.Replace(s, `,"isError":false`, "", 1) },
		"image failure notice": func(s string) string {
			return strings.ReplaceAll(s, `""`, `"Read image file [image/png] could not be decoded"`)
		},
		"failed": func(s string) string { return strings.Replace(s, `"isError":false`, `"isError":true`, 1) },
		"details": func(s string) string {
			return strings.Replace(s, `"isError":false`, `"isError":false,"details":{"truncation":{}}`, 1)
		},
		"structured mismatch": func(s string) string {
			return strings.Replace(s, `"structuredContent":""`, `"structuredContent":"other"`, 1)
		},
		"invalid Unicode": func(s string) string { return strings.Replace(s, `"text":""`, `"text":"\ud800"`, 1) },
		"replacement rune": func(s string) string {
			return strings.Replace(strings.Replace(s, `"text":""`, `"text":"\ufffd"`, 1), `,"structuredContent":""`, "", 1)
		},
		"notice": func(s string) string {
			return strings.ReplaceAll(s, `""`, `"[Showing lines 1-2. Use offset=3 to continue.]"`)
		},
		"missing parent": func(s string) string { return strings.Replace(s, `"parentId":null,`, "", 1) },
		"reused call": func(s string) string {
			second := strings.ReplaceAll(strings.Replace(piEvidenceCall, `"id":"a"`, `"id":"c"`, 1), `"parentId":null`, `"parentId":"a"`)
			return piEvidenceCall + "\n" + second + "\n" + strings.Replace(piEvidenceResult, `"parentId":"a"`, `"parentId":"c"`, 1)
		},
		"result before call": func(s string) string {
			return piEvidenceResult + "\n" + piEvidenceCall + "\n" + `{"type":"model_change","id":"c","parentId":"b"}`
		},
		"cycle": func(s string) string { return strings.Replace(s, `"parentId":null`, `"parentId":"b"`, 1) },
	} {
		t.Run(name, func(t *testing.T) {
			for _, m := range piEvidenceParse(t, change(piEvidenceCall+"\n"+piEvidenceResult), AgentPi) {
				for _, c := range m.FileEvidence {
					assert.Empty(t, c.Record.SHA256)
				}
			}
		})
	}
}

func TestPiFileEvidenceRejectedDescriptor(t *testing.T) {
	ms := piEvidenceParse(t, piEvidenceCall, AgentPi)
	require.Len(t, ms[0].FileEvidence, 1)
	assert.NotEmpty(t, ms[0].FileEvidence[0].Record.IneligibleReason)
	assert.Empty(t, ms[0].FileEvidence[0].Record.SHA256)
	entries := strings.Replace(piEvidenceCall, `"arguments":`, `"override":true,"arguments":`, 1) + "\n" + piEvidenceResult
	for _, m := range piEvidenceParse(t, entries, AgentPi) {
		for _, c := range m.FileEvidence {
			assert.Empty(t, c.Record.SHA256)
		}
	}
}

func TestPiFileEvidenceNativeReadWriteBytes(t *testing.T) {
	for _, name := range []string{"read", "write"} {
		call := strings.ReplaceAll(piEvidenceCall, `"read"`, `"`+name+`"`)
		result := strings.ReplaceAll(piEvidenceResult, `"read"`, `"`+name+`"`)
		if name == "write" {
			call = strings.Replace(call, `"path":"example.txt"`, `"path":"example.txt","content":"alpha\r\nbeta\r\n"`, 1)
		} else {
			result = strings.ReplaceAll(result, `""`, `"alpha\r\nbeta\r\n"`)
		}
		for _, separator := range []string{"\n", "\r\n", "\n\n"} {
			ms := piEvidenceParse(t, call+separator+result, AgentPi)
			require.Len(t, ms[0].FileEvidence, 1)
			r := ms[0].FileEvidence[0].Record
			require.NotNil(t, r.Size, name)
			assert.Equal(t, int64(13), *r.Size, name)
			assert.Equal(t, "98ab4d3aeab1e120560e942e2df6a0db1147bf94bafcf1590000ffb3c2b6fc80", r.SHA256, name)
		}
	}
}

func TestPiStringUserMessageDoesNotInvalidateNativeProof(t *testing.T) {
	user := `{"type":"message","id":"u","parentId":null,"message":{"role":"user","content":"hello"}}`
	call := strings.Replace(piEvidenceCall, `"parentId":null`, `"parentId":"u"`, 1)
	result := strings.ReplaceAll(piEvidenceResult, `""`, `"alpha\r\nbeta\r\n"`)
	messages := piEvidenceParse(t, user+"\n"+call+"\n"+result, AgentPi)
	require.Len(t, messages, 3)
	require.Len(t, messages[1].FileEvidence, 1)
	assert.Equal(t, "98ab4d3aeab1e120560e942e2df6a0db1147bf94bafcf1590000ffb3c2b6fc80", messages[1].FileEvidence[0].Record.SHA256)
}

func TestPiToolResultImageCarriesCallOwnership(t *testing.T) {
	result := strings.Replace(
		piEvidenceResult,
		`"content":[{"type":"text","text":""}]`,
		`"content":[{"type":"image","mimeType":"image/png","data":"YWxwaGEK"}]`,
		1,
	)
	messages := piEvidenceParse(t, piEvidenceCall+"\n"+result, AgentPi)
	require.Len(t, messages, 2)
	require.Len(t, messages[1].FileEvidence, 1)
	candidate := messages[1].FileEvidence[0]
	assert.Equal(t, "result", candidate.Support)
	assert.Equal(t, "t", candidate.ToolUseID)
	assert.Equal(t, "b", candidate.Record.Position.ResultEntry)
}

func TestPiFileEvidenceMemory(t *testing.T) {
	measure := func(count int) int64 {
		f, err := os.Create(filepath.Join(t.TempDir(), "evidence.jsonl"))
		require.NoError(t, err)
		defer f.Close()
		_, err = fmt.Fprintln(f, piEvidenceHeader)
		require.NoError(t, err)
		padding := strings.Repeat("x", 1<<20)
		for i := range count {
			_, err = fmt.Fprintf(f, `{"type":"custom","id":"entry%d","parentId":null,"data":"%s"}`+"\n", i, padding)
			require.NoError(t, err)
		}
		_, err = f.Seek(0, io.SeekStart)
		require.NoError(t, err)
		info, err := f.Stat()
		require.NoError(t, err)
		runtime.GC()
		var before, during runtime.MemStats
		runtime.ReadMemStats(&before)
		session, messages, err := parsePiLikeReader(f.Name(), "", "", AgentPi, "pi:", info, evidenceSeedReader{Reader: f, ReaderAt: f, atEOF: func() {
			runtime.GC()
			runtime.ReadMemStats(&during)
		}})
		require.NoError(t, err)
		require.NotNil(t, session)
		assert.Empty(t, messages)
		return int64(during.HeapAlloc) - int64(before.HeapAlloc)
	}
	small, large := measure(2), measure(20)
	assert.Less(t, large-small, int64(8<<20), "Pi proof bookkeeping must not retain unrelated raw rows")
}

func TestPiFileEvidenceRejectsChangedSource(t *testing.T) {
	path := createTestFile(t, "evidence.jsonl", piEvidenceHeader+"\r\n\r\n"+piEvidenceCall+"\r\n"+piEvidenceResult)
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	defer f.Close()
	info, err := f.Stat()
	require.NoError(t, err)
	_, messages, err := parsePiLikeReader(path, "", "", AgentPi, "pi:", info, evidenceSeedReader{Reader: f, ReaderAt: f, atEOF: func() {
		// A same-size rewrite between the scan and proof extraction must fail closed.
		offset := int64(len(piEvidenceHeader) + 4 + len(piEvidenceCall) + 2)
		_, err := f.WriteAt([]byte(strings.Replace(piEvidenceResult, `"id":"b"`, `"id":"c"`, 1)), offset)
		require.NoError(t, err)
	}})
	require.NoError(t, err)
	require.Len(t, messages, 2)
	require.Len(t, messages[0].FileEvidence, 1)
	assert.Empty(t, messages[0].FileEvidence[0].Record.SHA256)
}
