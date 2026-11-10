//go:build (linux && !android && (amd64 || arm64)) || (darwin && !ios && arm64) || (windows && (amd64 || arm64))

package parser

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func musePersonalPropertyFile(t *testing.T) string {
	t.Helper()
	_, path := musePersonalTestProvider(t, "")
	return path
}

func TestMusePersonalHegelDuplicateAndGapAccounting(t *testing.T) {
	path := musePersonalPropertyFile(t)
	hegel.Test(t, func(ht *hegel.T) {
		seq := hegel.Draw(ht, hegel.Integers[int64](0, math.MaxInt64))
		text := hegel.Draw(ht, hegel.Text().MaxSize(256))
		payload, err := json.Marshal(map[string]string{"type": "message", "role": "user", "text": text})
		require.NoError(ht, err)
		// seq comes from the full accepted non-negative int64 domain.
		one := fmt.Sprintf(`{"type":"item","seq":%d,"source":"scheduler.cron","created_at":"2026-10-05T15:04:06Z","item":%s}`+"\n", seq, payload)
		other := fmt.Sprintf(`{"type":"item","seq":%d,"source":"scheduler.cron","created_at":"2026-10-05T15:04:06Z","item":{"type":"message","role":"assistant","text":"different"}}`+"\n", seq)
		require.NoError(ht, os.WriteFile(path, []byte(musePersonalTestHeader+one+one+other), 0o600))
		sess, msgs, err := parseMusePersonalSession(t.Context(), path, "fixture")
		require.NoError(ht, err)
		assert.Zero(ht, sess.MalformedLines)
		require.Len(ht, msgs, 2)
		assert.Equal(ht, text, msgs[0].Content)
		assert.Equal(ht, "different", msgs[1].Content)
		assert.NotEqual(ht, msgs[0].SourceUUID, msgs[1].SourceUUID)
	}, hegel.WithTestCases(2000), hegel.WithDatabase(""))
}

func TestMusePersonalHegelOrderedParts(t *testing.T) {
	path := musePersonalPropertyFile(t)
	hegel.Test(t, func(ht *hegel.T) {
		before := hegel.Draw(ht, hegel.Text().MaxSize(256))
		after := hegel.Draw(ht, hegel.Text().MaxSize(256))
		roles := []string{"user", "assistant", "developer", "system"}
		role := roles[hegel.Draw(ht, hegel.Integers(0, len(roles)-1))]
		item, err := json.Marshal(map[string]any{
			"type": "message_parts", "role": role,
			"parts": []map[string]string{
				{"type": "text", "text": before},
				{"type": "file_ref", "filename": "fixture.txt", "mime_type": "text/plain", "path": "/example/inert.txt"},
				{"type": "text", "text": after},
			},
		})
		require.NoError(ht, err)
		require.NoError(ht, os.WriteFile(path, []byte(musePersonalTestHeader+
			musePersonalTestItem(0, string(item))), 0o600))
		_, msgs, err := parseMusePersonalSession(t.Context(), path, "fixture")
		require.NoError(ht, err)
		require.Len(ht, msgs, 1)
		want := before + "\n[File: fixture.txt (text/plain), path: /example/inert.txt]\n" + after
		assert.Equal(ht, want, msgs[0].Content)
		require.NotNil(ht, msgs[0].DialogueText)
		assert.Equal(ht, want, *msgs[0].DialogueText)
		wantRole := RoleSystem
		if role == "user" {
			wantRole = RoleUser
		}
		if role == "assistant" {
			wantRole = RoleAssistant
		}
		assert.Equal(ht, wantRole, msgs[0].Role)
	}, hegel.WithTestCases(2000), hegel.WithDatabase(""))
}

func TestMusePersonalHegelRejectsNegativeSequence(t *testing.T) {
	path := musePersonalPropertyFile(t)
	hegel.Test(t, func(ht *hegel.T) {
		seq := hegel.Draw(ht, hegel.Integers[int64](math.MinInt64, -1))
		bad := fmt.Sprintf(`{"type":"item","seq":%d,"source":"scheduler.cron","created_at":"2026-10-05T15:04:06Z","item":{"type":"message","role":"user","text":"invalid"}}`+"\n", seq)
		valid := musePersonalTestItem(0, `{"type":"message","role":"user","text":"valid"}`)
		require.NoError(ht, os.WriteFile(path, []byte(musePersonalTestHeader+bad+valid), 0o600))
		sess, msgs, err := parseMusePersonalSession(t.Context(), path, "fixture")
		require.NoError(ht, err)
		assert.Equal(ht, 1, sess.MalformedLines)
		require.Len(ht, msgs, 1)
		assert.Equal(ht, "valid", msgs[0].Content)
	}, hegel.WithTestCases(2000), hegel.WithDatabase(""))
}

func TestMusePersonalHegelFutureBodyShapes(t *testing.T) {
	path := musePersonalPropertyFile(t)
	hegel.Test(t, func(ht *hegel.T) {
		scope := hegel.Draw(ht, hegel.Integers(0, 3))
		kind := "future_" + hegel.Draw(ht, hegel.Text().MaxSize(256))
		source := hegel.Draw(ht, hegel.Text().MaxSize(256))
		var opaque any = map[string]any{"opaque": "sentinel-body"}
		switch hegel.Draw(ht, hegel.Integers(0, 5)) {
		case 0:
			opaque = nil
		case 1:
			opaque = hegel.Draw(ht, hegel.Text().MaxSize(256))
		case 2:
			opaque = []any{true, 17, "sentinel-body"}
		case 3:
			opaque = false
		case 4:
			opaque = 17
		}
		record := map[string]any{"type": "item", "seq": 0, "source": source, "created_at": "2026-10-05T15:04:06Z"}
		switch scope {
		case 0:
			record["item"] = map[string]any{"type": kind, "text": opaque, "role": opaque, "parts": opaque}
		case 1:
			record["item"] = map[string]any{"type": "message", "role": kind, "text": opaque}
		case 2:
			record["item"] = map[string]any{"type": "message_parts", "role": "user", "parts": []any{map[string]any{"type": kind, "text": opaque, "filename": opaque}}}
		case 3:
			record["type"] = kind
			record["item"] = opaque
			record["summary"] = opaque
		}
		raw, err := json.Marshal(record)
		require.NoError(ht, err)
		require.NoError(ht, os.WriteFile(path, []byte(musePersonalTestHeader+string(raw)+"\n"), 0o600))
		sess, msgs, err := parseMusePersonalSession(t.Context(), path, "fixture")
		require.NoError(ht, err)
		assert.Zero(ht, sess.MalformedLines)
		require.Len(ht, msgs, 1)
		assert.Equal(ht, musePersonalUnsupportedContent, msgs[0].Content)
		assert.True(ht, strings.HasPrefix(msgs[0].SourceSubtype, "unsupported:"), msgs[0].SourceSubtype)
		assert.Contains(ht, msgs[0].SourceSubtype, ":future_")
		assert.LessOrEqual(ht, len(msgs[0].SourceSubtype), len("unsupported:record:")+64)
		assert.Equal(ht, source, msgs[0].SourceType)
	}, hegel.WithTestCases(2000), hegel.WithDatabase(""))
}

func TestMusePersonalHegelRejectsInvalidTimestamps(t *testing.T) {
	path := musePersonalPropertyFile(t)
	hegel.Test(t, func(ht *hegel.T) {
		invalid, err := json.Marshal("invalid_" + hegel.Draw(ht, hegel.Text().MaxSize(256)))
		require.NoError(ht, err)
		switch hegel.Draw(ht, hegel.Integers(0, 5)) {
		case 0:
			invalid = []byte("null")
		case 1:
			invalid = []byte("true")
		case 2:
			invalid = []byte("[]")
		case 3:
			invalid = []byte("{}")
		case 4:
			invalid = []byte("1.5")
		}
		body := fmt.Sprintf(`{"type":"item","seq":0,"created_at":%s,"item":{"type":"message","role":"user","text":"invalid"}}`+"\n"+`{"type":"compaction_checkpoint","checkpoint_seq":0,"created_at":%s,"summary":"invalid"}`+"\n", invalid, invalid)
		body += musePersonalTestItem(1, `{"type":"message","role":"user","text":"valid"}`)
		require.NoError(ht, os.WriteFile(path, []byte(musePersonalTestHeader+body), 0o600))
		sess, msgs, err := parseMusePersonalSession(t.Context(), path, "fixture")
		require.NoError(ht, err)
		assert.Equal(ht, 2, sess.MalformedLines)
		require.Len(ht, msgs, 1)
		assert.Equal(ht, "valid", msgs[0].Content)
	}, hegel.WithTestCases(2000), hegel.WithDatabase(""))
}

func TestMusePersonalHegelOpaqueBodiesStayOpaque(t *testing.T) {
	path := musePersonalPropertyFile(t)
	hegel.Test(t, func(ht *hegel.T) {
		suffix := hegel.Draw(ht, hegel.Text().MaxSize(256))
		source := hegel.Draw(ht, hegel.Text().MaxSize(256))
		kind := "future_" + suffix
		item, err := json.Marshal(map[string]string{
			"type": kind, "text": "opaque-sentinel-body", "output": "opaque-sentinel-body",
		})
		require.NoError(ht, err)
		rawSource, err := json.Marshal(source)
		require.NoError(ht, err)
		record := fmt.Sprintf(`{"type":"item","seq":0,"source":%s,"created_at":"2026-10-05T15:04:06Z","item":%s}`+"\n", rawSource, item)
		require.NoError(ht, os.WriteFile(path, []byte(musePersonalTestHeader+record), 0o600))
		_, msgs, err := parseMusePersonalSession(t.Context(), path, "fixture")
		require.NoError(ht, err)
		require.Len(ht, msgs, 1)
		assert.Equal(ht, source, msgs[0].SourceType)
		assert.Contains(ht, msgs[0].SourceSubtype, "unsupported:item:future_")
		assert.LessOrEqual(ht, len(msgs[0].SourceSubtype), len("unsupported:item:")+64)
		assert.Contains(ht, msgs[0].SourceUUID, ":item:0:1")
		assert.Equal(ht, musePersonalUnsupportedContent, msgs[0].Content)
		assert.Empty(ht, msgs[0].ToolResults)
		assert.Empty(ht, msgs[0].ToolCalls)
	}, hegel.WithTestCases(2000), hegel.WithDatabase(""))
}
