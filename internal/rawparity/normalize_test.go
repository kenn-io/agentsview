package rawparity

import (
	"encoding/json/v2"
	"math"
	"os"
	"runtime"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
	"hegel.dev/go/hegel"
)

func sample() Snapshot {
	return Snapshot{
		Session:  db.Session{Agent: "codex", Project: "sample", Cwd: "/project", ParentSessionID: new("parent"), RelationshipType: "subagent"},
		Messages: []db.Message{{Ordinal: 3, Role: "assistant", Content: "answer", Timestamp: "2026-01-01T00:00:00Z", TokenUsage: []byte(`{"input":1,"output":2}`), ToolCalls: []db.ToolCall{{CallIndex: 0, ToolName: "Bash", InputJSON: `{"a":1,"b":2}`, SubagentSessionID: "child", ResultEvents: []db.ToolResultEvent{{EventIndex: 0, Content: "done", SubagentSessionID: "child"}}}}}},
		Usage:    []db.UsageEvent{{Source: "api", InputTokens: 1, Cost: &money.Money{Microdollars: 1}}},
		Secrets:  []db.SecretFinding{{RuleName: "token", MessageOrdinal: 3}},
		Friction: []db.FrictionFinding{{Kind: "failure", Detector: "tool", MessageOrdinal: new(3)}},
	}
}

// Each witness catches a missing compared part; the other five must stay equal.
func TestSemanticMutations(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Snapshot, string)
	}{
		{"metadata", func(s *Snapshot, v string) { s.Session.Project = v }},
		{"messages", func(s *Snapshot, v string) { s.Messages[0].Content = v }},
		{"tool_calls", func(s *Snapshot, v string) { s.Messages[0].ToolCalls[0].ResultEvents[0].Content = v }},
		{"usage", func(s *Snapshot, v string) { s.Usage[0].Model = v }},
		{"findings", func(s *Snapshot, v string) { s.Secrets[0].RuleName = v }},
		{"relationships", func(s *Snapshot, v string) { s.Messages[0].ToolCalls[0].SubagentSessionID = v }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			propertyTest(t, func(ht *hegel.T) {
				a, b := sample(), sample()
				tc.change(&b, "changed:"+hegel.Draw(ht, hegel.Text()))
				got, err := Compare(a, b)
				require.NoError(ht, err)
				assert.Equal(ht, []string{tc.name}, got.Differing)
			}, hegel.WithTestCases(1000))
		})
	}
}

func TestRepresentationRewrites(t *testing.T) {
	propertyTest(t, func(ht *hegel.T) {
		a, b := sample(), sample()
		n := hegel.Draw(ht, hegel.Integers(math.MinInt64, math.MaxInt64))
		text := hegel.Draw(ht, hegel.Text())
		left, err := json.Marshal(map[string]any{"a": n, "b": text})
		require.NoError(ht, err)
		val, err := json.Marshal(text)
		require.NoError(ht, err)
		right := []byte(`{ "b":` + string(val) + `, "a":` + string(mustJSON(ht, n)) + ` }`)
		a.Messages[0].TokenUsage = left
		b.Messages[0].TokenUsage = right
		a.Messages[0].ToolCalls[0].InputJSON = string(left)
		b.Messages[0].ToolCalls[0].InputJSON = string(right)
		got, err := Compare(a, b)
		require.NoError(ht, err)
		assert.Empty(ht, got.Differing)
	}, hegel.WithTestCases(1000))
}

func mustJSON(t require.TestingT, v any) []byte {
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

func TestMaskedFields(t *testing.T) {
	propertyTest(t, func(ht *hegel.T) {
		a, b := sample(), sample()
		v := hegel.Draw(ht, hegel.Text())
		b.Session.ID = v
		b.Session.Machine = v
		b.Session.FilePath = &v
		b.Session.DisplayName = &v
		b.Session.DeletedAt = &v
		b.Session.DeletionCause = &v
		b.Session.CreatedAt = v
		b.Session.DataVersion = 17
		b.Session.FileSize = new(int64(99))
		b.Session.TranscriptRevision = &v
		b.Messages[0].ID = 99
		b.Messages[0].SessionID = v
		b.Messages[0].DialogueText = &v
		c := &b.Messages[0].ToolCalls[0]
		c.MessageID = 99
		c.SessionID = v
		c.Rendering = v
		c.ResultEvents[0].RawContentDigest = []byte(v)
		c.ResultEvents[0].SummaryParticipates = new(true)
		b.Usage[0].ID = 99
		b.Usage[0].SessionID = v
		b.Secrets[0].SessionID = v
		b.Secrets[0].RulesVersion = v
		b.Friction[0].RulesVersion = v
		got, err := Compare(a, b)
		require.NoError(ht, err)
		assert.Empty(ht, got.Differing)
	}, hegel.WithTestCases(1000))
}

func TestRelationshipMatchMap(t *testing.T) {
	propertyTest(t, func(ht *hegel.T) {
		a, b := sample(), sample()
		id := "physical:" + hegel.Draw(ht, hegel.Text())
		b.Session.ParentSessionID = nil
		b.Session.ParentSessionIDs = []string{id, id}
		b.Messages[0].ToolCalls[0].SubagentSessionID = "call:" + id
		b.Messages[0].ToolCalls[0].ResultEvents[0].SubagentSessionID = "event:" + id
		a.MatchIDs = map[string]string{"parent": "public-parent", "child": "public-child"}
		b.MatchIDs = map[string]string{id: "public-parent", "call:" + id: "public-child", "event:" + id: "public-child"}
		got, err := Compare(a, b)
		require.NoError(ht, err)
		assert.Empty(ht, got.Differing)
	}, hegel.WithTestCases(1000))
}

func TestEmptyCollections(t *testing.T) {
	got, err := Compare(Snapshot{}, Snapshot{Messages: []db.Message{}, Usage: []db.UsageEvent{}, Secrets: []db.SecretFinding{}, Friction: []db.FrictionFinding{}})
	require.NoError(t, err)
	assert.Empty(t, got.Differing)
	assert.Len(t, got.Left, 6)
}

func TestMessageAndToolMetadataMutations(t *testing.T) {
	for _, tc := range []struct {
		name, part string
		change     func(*Snapshot, string)
	}{
		{"model", "messages", func(s *Snapshot, v string) { s.Messages[0].Model = v }},
		{"reasoning", "messages", func(s *Snapshot, v string) { s.Messages[0].ReasoningEffort = v }},
		{"source_uuid", "messages", func(s *Snapshot, v string) { s.Messages[0].SourceUUID = v }},
		{"context_presence", "messages", func(s *Snapshot, _ string) { s.Messages[0].HasContextTokens = !s.Messages[0].HasContextTokens }},
		{"output_presence", "messages", func(s *Snapshot, _ string) { s.Messages[0].HasOutputTokens = !s.Messages[0].HasOutputTokens }},
		{"system", "messages", func(s *Snapshot, _ string) { s.Messages[0].IsSystem = !s.Messages[0].IsSystem }},
		{"sidechain", "messages", func(s *Snapshot, _ string) { s.Messages[0].IsSidechain = !s.Messages[0].IsSidechain }},
		{"target_path", "tool_calls", func(s *Snapshot, v string) { s.Messages[0].ToolCalls[0].FilePath = v }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			propertyTest(t, func(ht *hegel.T) {
				a, b := sample(), sample()
				flag := hegel.Draw(ht, hegel.Booleans())
				a.Messages[0].HasContextTokens, a.Messages[0].HasOutputTokens, a.Messages[0].IsSystem, a.Messages[0].IsSidechain = flag, flag, flag, flag
				b.Messages[0].HasContextTokens, b.Messages[0].HasOutputTokens, b.Messages[0].IsSystem, b.Messages[0].IsSidechain = flag, flag, flag, flag
				tc.change(&b, "changed:"+hegel.Draw(ht, hegel.Text()))
				got, err := Compare(a, b)
				require.NoError(ht, err)
				assert.Equal(ht, []string{tc.part}, got.Differing)
			}, hegel.WithTestCases(1000))
		})
	}
}

func TestTimePrecision(t *testing.T) {
	a, b := sample(), sample()
	a.Messages[0].Timestamp = "2026-01-01T00:00:00.123456789Z"
	b.Messages[0].Timestamp = "2025-12-31T19:00:00.123456-05:00"
	a.Session.StartedAt = &a.Messages[0].Timestamp
	b.Session.StartedAt = &b.Messages[0].Timestamp
	now := time.Date(2026, 1, 1, 0, 0, 0, 123456789, time.UTC)
	a.Friction[0].OccurredAt = &now
	other := now.Truncate(time.Microsecond)
	b.Friction[0].OccurredAt = &other
	got, err := Compare(a, b)
	require.NoError(t, err)
	assert.Empty(t, got.Differing)
}

func TestCompareDoesNotMutateInputs(t *testing.T) {
	propertyTest(t, func(ht *hegel.T) {
		text := hegel.Draw(ht, hegel.Text())
		a, want := collectionSample(text), collectionSample(text)
		_, err := Compare(a, sample())
		require.NoError(ht, err)
		assert.Equal(ht, want, a)
	}, hegel.WithTestCases(1000))
}

func TestTrailingJSONRemainsMalformed(t *testing.T) {
	for _, field := range []string{"messages", "tool_calls"} {
		t.Run(field, func(t *testing.T) {
			propertyTest(t, func(ht *hegel.T) {
				a, b := sample(), sample()
				suffix := " !" + hegel.Draw(ht, hegel.Text())
				if field == "messages" {
					b.Messages[0].TokenUsage = append(b.Messages[0].TokenUsage, []byte(suffix)...)
				} else {
					b.Messages[0].ToolCalls[0].InputJSON += suffix
				}
				got, err := Compare(a, b)
				require.NoError(ht, err)
				assert.Equal(ht, []string{field}, got.Differing)
			}, hegel.WithTestCases(1000))
		})
	}
}

func TestSQLTimestampRepresentations(t *testing.T) {
	propertyTest(t, func(ht *hegel.T) {
		micros := hegel.Draw(ht, hegel.Integers(0, 999999))
		second := hegel.Draw(ht, hegel.Integers(0, 86399))
		now := time.Date(2026, 1, 1, 0, 0, second, micros*1000, time.UTC)
		a, b := sample(), sample()
		a.Messages[0].Timestamp = now.Format(time.RFC3339Nano)
		b.Messages[0].Timestamp = now.Format("2006-01-02 15:04:05.999999999")
		got, err := Compare(a, b)
		require.NoError(ht, err)
		assert.Empty(ht, got.Differing)
	}, hegel.WithTestCases(1000))
}

func TestFrictionPhysicalIdentityFormatting(t *testing.T) {
	propertyTest(t, func(ht *hegel.T) {
		a, b := sample(), sample()
		id := "raw-row-" + hegel.Draw(ht, hegel.Text())
		a.Friction[0].Kind, b.Friction[0].Kind = "deferral", "deferral"
		a.Friction[0].Label, b.Friction[0].Label = "next session", "next session"
		a.Friction[0].Title = "[friction/deferral] pushed: next session"
		a.Friction[0].Fingerprint = "pushed-title-hash"
		b.Friction[0].Title = "[friction/deferral] " + id + ": next session"
		b.Friction[0].Fingerprint = "raw-title-hash"
		got, err := Compare(a, b)
		require.NoError(ht, err)
		assert.Empty(ht, got.Differing)
	}, hegel.WithTestCases(1000))
}

// Minimal shrunk witnesses: an empty generated suffix after " !", and midnight
// with zero fractional seconds in SQLite's space-separated timestamp form.
func TestTrailingJSONMinimalCounterexamples(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Snapshot)
	}{
		{"token_usage", func(s *Snapshot) { s.Messages[0].TokenUsage = []byte(`{"input":1,"output":2} !`) }},
		{"tool_input", func(s *Snapshot) { s.Messages[0].ToolCalls[0].InputJSON = `{"a":1,"b":2} !` }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := sample(), sample()
			tc.change(&b)
			got, err := Compare(a, b)
			require.NoError(t, err)
			assert.NotEmpty(t, got.Differing)
		})
	}
}

func TestSQLiteMidnightCounterexample(t *testing.T) {
	a, b := sample(), sample()
	b.Messages[0].Timestamp = "2026-01-01 00:00:00"
	got, err := Compare(a, b)
	require.NoError(t, err)
	assert.Empty(t, got.Differing)
}

func TestFrictionIdentityMinimalCounterexample(t *testing.T) {
	a, b := sample(), sample()
	a.Friction[0].Kind, b.Friction[0].Kind = "deferral", "deferral"
	a.Friction[0].Label, b.Friction[0].Label = "next session", "next session"
	a.Friction[0].Title = "[friction/deferral] pushed: next session"
	a.Friction[0].Fingerprint = "pushed-title-hash"
	b.Friction[0].Title = "[friction/deferral] raw-row-: next session"
	b.Friction[0].Fingerprint = "raw-title-hash"
	got, err := Compare(a, b)
	require.NoError(t, err)
	assert.Empty(t, got.Differing)
}

func TestExactNumericMutations(t *testing.T) {
	for _, tc := range []struct {
		name, part string
		set        func(*Snapshot, int64)
	}{
		{"cost", "usage", func(s *Snapshot, n int64) { s.Usage[0].Cost = &money.Money{Microdollars: n} }},
		{"tokens", "usage", func(s *Snapshot, n int64) { s.Usage[0].InputTokens = int(n) }},
		{"json_number", "tool_calls", func(s *Snapshot, n int64) { s.Messages[0].ToolCalls[0].InputJSON = strconv.FormatInt(n, 10) }},
		{"call_coordinate", "tool_calls", func(s *Snapshot, n int64) { s.Messages[0].ToolCalls[0].CallIndex = int(n) }},
		{"event_coordinate", "tool_calls", func(s *Snapshot, n int64) { s.Messages[0].ToolCalls[0].ResultEvents[0].EventIndex = int(n) }},
		{"finding_coordinate", "findings", func(s *Snapshot, n int64) { s.Friction[0].MessageOrdinal = new(int(n)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			propertyTest(t, func(ht *hegel.T) {
				a, b := sample(), sample()
				n := hegel.Draw(ht, hegel.Integers(int64(math.MinInt64), int64(math.MaxInt64)))
				tc.set(&a, n)
				tc.set(&b, n^1)
				got, err := Compare(a, b)
				require.NoError(ht, err)
				want := []string{tc.part}
				if tc.name == "call_coordinate" || tc.name == "event_coordinate" {
					want = append(want, "relationships")
				}
				assert.Equal(ht, want, got.Differing)
			}, hegel.WithTestCases(1000))
		})
	}
}

func TestNullableAccounting(t *testing.T) {
	for _, field := range []string{"cost", "message_ordinal"} {
		t.Run(field, func(t *testing.T) {
			propertyTest(t, func(ht *hegel.T) {
				a, b := sample(), sample()
				n := hegel.Draw(ht, hegel.Integers(int64(math.MinInt64), int64(math.MaxInt64)))
				if field == "cost" {
					a.Usage[0].Cost = nil
					b.Usage[0].Cost = &money.Money{Microdollars: n}
				} else {
					a.Usage[0].MessageOrdinal = nil
					b.Usage[0].MessageOrdinal = new(int(n))
				}
				got, err := Compare(a, b)
				require.NoError(ht, err)
				assert.Equal(ht, []string{"usage"}, got.Differing)
			}, hegel.WithTestCases(1000))
		})
	}
}

func collectionSample(text string) Snapshot {
	s := sample()
	s.Session.ParentSessionIDs = append(make([]string, 0, 8), "parent", "another", "parent")
	s.Messages[0].Content = text
	s.Messages = append(s.Messages, db.Message{Ordinal: 4, Content: text})
	c := &s.Messages[0].ToolCalls[0]
	c.ResultEvents = append(c.ResultEvents, db.ToolResultEvent{EventIndex: 1, Content: text})
	s.Messages[0].ToolCalls = append(s.Messages[0].ToolCalls, db.ToolCall{CallIndex: 1, InputJSON: `[]`})
	s.Usage = append(s.Usage, db.UsageEvent{Model: text})
	s.Secrets = append(s.Secrets, db.SecretFinding{RuleName: text})
	s.Friction = append(s.Friction, db.FrictionFinding{Text: text})
	return s
}

func TestCollectionOrder(t *testing.T) {
	propertyTest(t, func(ht *hegel.T) {
		text := hegel.Draw(ht, hegel.Text())
		a, b := collectionSample(text), collectionSample(text)
		// Bound allocation after drawing, preserving the full generator domain.
		count := hegel.Draw(ht, hegel.Integers(uint(0), ^uint(0))) % 32
		for range count {
			a.Messages = append(a.Messages, db.Message{Ordinal: 5 + len(a.Messages), Content: text})
			b.Messages = append(b.Messages, db.Message{Ordinal: 5 + len(b.Messages), Content: text})
		}
		slices.Reverse(b.Messages[0].ToolCalls[0].ResultEvents)
		slices.Reverse(b.Messages[0].ToolCalls)
		slices.Reverse(b.Messages)
		slices.Reverse(b.Usage)
		slices.Reverse(b.Secrets)
		slices.Reverse(b.Friction)
		slices.Reverse(b.Session.ParentSessionIDs)
		got, err := Compare(a, b)
		require.NoError(ht, err)
		assert.Empty(ht, got.Differing)
	}, hegel.WithTestCases(1000))
}

func TestMultiplicityMatters(t *testing.T) {
	for _, part := range []string{"usage", "findings"} {
		t.Run(part, func(t *testing.T) {
			propertyTest(t, func(ht *hegel.T) {
				a, b := sample(), sample()
				text := hegel.Draw(ht, hegel.Text())
				a.Messages[0].Content, b.Messages[0].Content = text, text
				a.Usage[0].Model, b.Usage[0].Model = text, text
				a.Secrets[0].RuleName, b.Secrets[0].RuleName = text, text
				switch part {
				case "usage":
					b.Usage = append(b.Usage, b.Usage[0])
				case "findings":
					b.Secrets = append(b.Secrets, b.Secrets[0])
				}
				got, err := Compare(a, b)
				require.NoError(ht, err)
				assert.Contains(ht, got.Differing, part)
			}, hegel.WithTestCases(1000))
		})
	}
}

func TestUnmappedRelationshipChanges(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Snapshot, string)
	}{
		{"parent", func(s *Snapshot, v string) { s.Session.ParentSessionID = &v }},
		{"call", func(s *Snapshot, v string) { s.Messages[0].ToolCalls[0].SubagentSessionID = v }},
		{"result", func(s *Snapshot, v string) { s.Messages[0].ToolCalls[0].ResultEvents[0].SubagentSessionID = v }},
		{"type", func(s *Snapshot, v string) { s.Session.RelationshipType = v }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			propertyTest(t, func(ht *hegel.T) {
				a, b := sample(), sample()
				tc.change(&b, "unmatched:"+hegel.Draw(ht, hegel.Text()))
				got, err := Compare(a, b)
				require.NoError(ht, err)
				assert.Equal(ht, []string{"relationships"}, got.Differing)
			}, hegel.WithTestCases(1000))
		})
	}
}

func TestFailureDoesNotMutateInput(t *testing.T) {
	propertyTest(t, func(ht *hegel.T) {
		a, want := sample(), sample()
		text := "\xff" + hegel.Draw(ht, hegel.Text())
		a.Friction[0].Text = text
		want.Friction[0].Text = text
		_, err := Compare(a, sample())
		require.Error(ht, err)
		assert.Equal(ht, want, a)
	}, hegel.WithTestCases(1000))
}

func TestResultPrivacy(t *testing.T) {
	propertyTest(t, func(ht *hegel.T) {
		a, b := sample(), sample()
		sentinel := "transcript-sentinel:" + hegel.Draw(ht, hegel.Text())
		a.Messages[0].Content = sentinel
		got, err := Compare(a, b)
		require.NoError(ht, err)
		assert.NotContains(ht, string(mustJSON(ht, got)), sentinel)
		a.Friction[0].Text = "\xff" + sentinel
		_, err = Compare(a, b)
		require.Error(ht, err)
		assert.NotContains(ht, err.Error(), sentinel)
	}, hegel.WithTestCases(1000))
}

func TestComparisonSymmetry(t *testing.T) {
	propertyTest(t, func(ht *hegel.T) {
		a, b := sample(), sample()
		b.Messages[0].Content = hegel.Draw(ht, hegel.Text())
		ab, err := Compare(a, b)
		require.NoError(ht, err)
		ba, err := Compare(b, a)
		require.NoError(ht, err)
		assert.Equal(ht, ab.Left, ba.Right)
		assert.Equal(ht, ab.Right, ba.Left)
		assert.Equal(ht, ab.Differing, ba.Differing)
	}, hegel.WithTestCases(1000))
}

func TestToolCoordinateRelationshipCounterexamples(t *testing.T) {
	for _, field := range []string{"call", "event"} {
		t.Run(field, func(t *testing.T) {
			a, b := sample(), sample()
			if field == "call" {
				b.Messages[0].ToolCalls[0].CallIndex = 1
			} else {
				b.Messages[0].ToolCalls[0].ResultEvents[0].EventIndex = 1
			}
			got, err := Compare(a, b)
			require.NoError(t, err)
			assert.Equal(t, []string{"tool_calls", "relationships"}, got.Differing)
		})
	}
}

// Hegel 0.9.27 has no bundled Intel macOS library. Ordinary regression tests
// still run there; generated checks can run with a locally supplied library.
func propertyTest(t *testing.T, fn func(*hegel.T), opts ...hegel.Option) {
	t.Helper()
	if runtime.GOOS == "darwin" && runtime.GOARCH == "amd64" && os.Getenv("HEGEL_LIBHEGEL_PATH") == "" {
		t.Skip("Hegel requires HEGEL_LIBHEGEL_PATH on darwin/amd64")
	}
	hegel.Test(t, fn, opts...)
}
