//go:build (linux && !android && (amd64 || arm64)) || (darwin && !ios && arm64) || (windows && (amd64 || arm64))

package parser

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
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

func TestMusePersonalHegelIndependentWriteOrder(t *testing.T) {
	path := musePersonalPropertyFile(t)
	hegel.Test(t, func(ht *hegel.T) {
		n := hegel.Draw(ht, hegel.Integers(1, 64))
		remaining := make([]int, n)
		for i := range remaining {
			remaining[i] = i
		}
		var body strings.Builder
		for len(remaining) > 0 {
			pick := hegel.Draw(ht, hegel.Integers(0, len(remaining)-1))
			seq := remaining[pick]
			fmt.Fprint(&body, musePersonalTestItem(seq,
				fmt.Sprintf(`{"type":"message","role":"user","text":"item-%d"}`, seq)))
			remaining = append(remaining[:pick], remaining[pick+1:]...)
		}
		require.NoError(ht, os.WriteFile(path, []byte(musePersonalTestHeader+body.String()), 0o600))
		sess, msgs, err := parseMusePersonalSession(t.Context(), path, "fixture")
		require.NoError(ht, err)
		require.NotNil(ht, sess)
		assert.Zero(ht, sess.MalformedLines)
		require.Len(ht, msgs, n)
		for i, m := range msgs {
			assert.Equal(ht, fmt.Sprintf("item-%d", i), m.Content)
			assert.Contains(ht, m.SourceUUID, fmt.Sprintf(":item:%d:1", i))
		}
	}, hegel.WithTestCases(2000), hegel.WithDatabase(""))
}

func TestMusePersonalHegelCallOccurrences(t *testing.T) {
	path := musePersonalPropertyFile(t)
	hegel.Test(t, func(ht *hegel.T) {
		n := hegel.Draw(ht, hegel.Integers(0, 64))
		type call struct {
			seq     int
			id      string
			matched bool
		}
		var calls []call
		expected := map[int]string{}
		var orphan []string
		var records []string
		for seq := range n {
			id := strconv.Itoa(hegel.Draw(ht, hegel.Integers(0, 4)))
			if hegel.Draw(ht, hegel.Booleans()) {
				calls = append(calls, call{seq: seq, id: id})
				records = append(records, musePersonalTestItem(seq, fmt.Sprintf(
					`{"type":"function_call","call_id":%q,"name":"exec","arguments":"{}"}`, id)))
			} else {
				value := fmt.Sprintf("output-%d", seq)
				only, count := -1, 0
				for i, c := range calls {
					if c.id == id && !c.matched {
						only = i
						count++
					}
				}
				if count == 1 {
					calls[only].matched = true
					expected[calls[only].seq] = value
				} else {
					orphan = append(orphan, value)
				}
				records = append(records, musePersonalTestItem(seq, fmt.Sprintf(
					`{"type":"function_call_output","call_id":%q,"output":%q,"success":true}`, id, value)))
			}
		}
		var body strings.Builder
		for len(records) > 0 {
			pick := hegel.Draw(ht, hegel.Integers(0, len(records)-1))
			body.WriteString(records[pick])
			records = append(records[:pick], records[pick+1:]...)
		}
		require.NoError(ht, os.WriteFile(path, []byte(musePersonalTestHeader+body.String()), 0o600))
		_, msgs, err := parseMusePersonalSession(t.Context(), path, "fixture")
		require.NoError(ht, err)
		var gotOrphans []string
		var gotCalls int
		for _, m := range msgs {
			assert.Empty(ht, m.ToolResults)
			if m.Role == RoleTool {
				gotOrphans = append(gotOrphans, m.Content)
			}
			for _, c := range m.ToolCalls {
				require.Less(ht, gotCalls, len(calls))
				want := calls[gotCalls]
				assert.Equal(ht, want.id, c.ToolUseID)
				if value, paired := expected[want.seq]; paired {
					require.Len(ht, c.ResultEvents, 1)
					assert.Equal(ht, value, c.ResultEvents[0].Content)
				} else {
					assert.Empty(ht, c.ResultEvents)
				}
				gotCalls++
			}
		}
		assert.Equal(ht, len(calls), gotCalls)
		assert.Equal(ht, orphan, gotOrphans)
	}, hegel.WithTestCases(2000), hegel.WithDatabase(""))
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
		assert.Equal(ht, "unsupported", msgs[0].SourceSubtype)
		assert.Contains(ht, msgs[0].SourceUUID, ":item:0:1")
		assert.Contains(ht, msgs[0].Content, "future_")
		assert.LessOrEqual(ht, len(msgs[0].Content), 160)
		assert.NotContains(ht, msgs[0].Content, "\n")
		assert.NotContains(ht, msgs[0].Content, "opaque-sentinel-body")
		assert.Empty(ht, msgs[0].ToolResults)
		assert.Empty(ht, msgs[0].ToolCalls)
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

func TestMusePersonalHegelMixedAccounting(t *testing.T) {
	path := musePersonalPropertyFile(t)
	hegel.Test(t, func(ht *hegel.T) {
		type witness struct {
			raw, label, id   string
			kind, seq, order int
		}
		var physical []witness
		seq := 0
		n := hegel.Draw(ht, hegel.Integers(6, 40))
		for step := range n {
			kind := hegel.Draw(ht, hegel.Integers(0, 5))
			if step < 6 {
				kind = step
			}
			seq += hegel.Draw(ht, hegel.Integers(2, 5))
			w := witness{
				kind: kind, seq: seq, label: fmt.Sprintf("record-%d", step),
				id: strconv.Itoa(hegel.Draw(ht, hegel.Integers(0, 3))),
			}
			switch kind {
			case 0:
				w.raw = musePersonalTestItem(seq, fmt.Sprintf(`{"type":"message","role":"user","text":%q}`, w.label))
			case 1:
				w.raw = musePersonalTestItem(seq, fmt.Sprintf(`{"type":"function_call","call_id":%q,"name":"exec","arguments":"{}"}`, w.id))
			case 2:
				w.raw = musePersonalTestItem(seq, fmt.Sprintf(`{"type":"function_call_output","call_id":%q,"output":%q,"success":true}`, w.id, w.label))
			case 3:
				w.raw = musePersonalTestItem(seq, `{"type":"future_mix","text":"opaque-mixed-body"}`)
			case 4:
				w.raw = fmt.Sprintf(`{"type":"compaction_checkpoint","checkpoint_seq":0,"first_kept_seq":0,"created_at":1791212760,"summary":%q,"details_json":"opaque-mixed-body"}`+"\n", w.label)
			case 5:
				w.raw = "{malformed candidate\n"
			}
			physical = append(physical, w)
			if kind < 4 && (step == 0 || hegel.Draw(ht, hegel.Booleans())) {
				physical = append(physical, w)
			}
		}
		var shuffled []witness
		for len(physical) > 0 {
			i := hegel.Draw(ht, hegel.Integers(0, len(physical)-1))
			shuffled = append(shuffled, physical[i])
			physical = append(physical[:i], physical[i+1:]...)
		}
		var body strings.Builder
		var logical []witness
		seen := map[string]bool{}
		highWater, items, checkpoints, malformed, copies := -1, 0, 0, 0, 0
		for _, w := range shuffled {
			body.WriteString(w.raw)
			if w.kind == 4 {
				checkpoints++
				w.order = highWater
				logical = append(logical, w)
				continue
			}
			items++
			if w.kind == 5 {
				malformed++
				continue
			}
			if seen[w.raw] {
				copies++
				continue
			}
			seen[w.raw] = true
			if w.seq > highWater {
				highWater = w.seq
			}
			w.order = w.seq
			logical = append(logical, w)
		}
		sort.SliceStable(logical, func(i, j int) bool { return logical[i].order < logical[j].order })
		// Pair only after logical ordering. Scan prior occurrences as an oracle.
		var prior []int
		matched := map[int]bool{}
		paired := map[int]string{}
		var rows []witness
		pairedOutputs := 0
		for _, w := range logical {
			if w.kind == 1 {
				prior = append(prior, len(rows))
			}
			if w.kind == 2 {
				candidate, count := -1, 0
				for _, i := range prior {
					if rows[i].id == w.id && !matched[i] {
						candidate = i
						count++
					}
				}
				if count == 1 {
					matched[candidate] = true
					paired[candidate] = w.label
					pairedOutputs++
					continue
				}
			}
			rows = append(rows, w)
		}
		require.NoError(ht, os.WriteFile(path, []byte(musePersonalTestHeader+body.String()), 0o600))
		sess, msgs, err := parseMusePersonalSession(t.Context(), path, "fixture")
		require.NoError(ht, err)
		assert.Equal(ht, malformed, sess.MalformedLines)
		assert.Len(ht, msgs, items-pairedOutputs-copies-malformed+checkpoints)
		require.Len(ht, msgs, len(rows))
		for i, w := range rows {
			m := msgs[i]
			assert.Empty(ht, m.ToolResults)
			if w.kind != 4 {
				assert.Contains(ht, m.SourceUUID, fmt.Sprintf(":item:%d:1", w.seq))
			}
			switch w.kind {
			case 0:
				assert.Equal(ht, w.label, m.Content)
			case 1:
				require.Len(ht, m.ToolCalls, 1)
				assert.Equal(ht, w.id, m.ToolCalls[0].ToolUseID)
				if value, ok := paired[i]; ok {
					require.Len(ht, m.ToolCalls[0].ResultEvents, 1)
					assert.Equal(ht, value, m.ToolCalls[0].ResultEvents[0].Content)
				} else {
					assert.Empty(ht, m.ToolCalls[0].ResultEvents)
				}
			case 2:
				assert.Equal(ht, RoleTool, m.Role)
				assert.Equal(ht, w.label, m.Content)
			case 3:
				assert.Contains(ht, m.Content, "future_mix")
				assert.NotContains(ht, m.Content, "opaque-mixed-body")
			case 4:
				assert.True(ht, m.IsCompactBoundary)
				assert.Equal(ht, w.label, m.Content)
				assert.NotContains(ht, m.Content, "opaque-mixed-body")
			}
		}
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
		assert.Contains(ht, msgs[0].Content, "future_")
		assert.NotContains(ht, msgs[0].Content, "sentinel-body")
		assert.Equal(ht, source, msgs[0].SourceType)
		assert.LessOrEqual(ht, len(msgs[0].Content), 160)
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
