package ledger

import (
	"bufio"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/serdejson"
)

// rustFixtures are the files in testdata/segments, written by jilog
// ledger-core 9e8e094 (see testdata/README.md). The checksums are the
// generator's own output (checksums.txt).
var rustFixtures = []struct {
	file     string
	checksum uint32
	events   int
}{
	{"fixture-a-000001.json", 1796932786, 5},
	{"fixture-a-000002.json", 3548185085, 3},
	{"host-with-dash-000007.json", 1192239657, 10},
	{"fixture-empty-000001.json", 223132457, 0},
	{"fixture-bigseq-000001.json", 3553725883, 1},
	{"fixture-lossy-000001.json", 4223796471, 1},
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "segments", name))
	require.NoError(t, err)
	return b
}

func loadFixture(t *testing.T, name string) Segment {
	t.Helper()
	seg, err := ParseSegmentFile(readFixture(t, name))
	require.NoError(t, err)
	return seg
}

func TestRustFixturesVerifyAndRemarshal(t *testing.T) {
	for _, tt := range rustFixtures {
		t.Run(tt.file, func(t *testing.T) {
			raw := readFixture(t, tt.file)
			seg, err := ParseSegmentFile(raw)
			require.NoError(t, err)
			assert.Equal(t, tt.checksum, seg.Checksum)
			assert.Len(t, seg.Events, tt.events)
			assert.Equal(t, tt.file, seg.Filename())
			source, seq, ok := ParseFilename(tt.file)
			require.True(t, ok)
			assert.Equal(t, seg.Source, source)
			assert.Equal(t, seg.SourceSeq, seq)

			ok, err = seg.Verify()
			require.NoError(t, err)
			assert.True(t, ok, "Go must reproduce serde's compact bytes")

			out, err := MarshalSegmentFile(seg)
			require.NoError(t, err)
			assert.Equal(t, string(raw), string(out), "re-marshal must be byte-identical")

			resealed := seg
			require.NoError(t, resealed.Seal())
			assert.Equal(t, tt.checksum, resealed.Checksum)
		})
	}
}

func TestRustFixtureEventsJSONBytes(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("testdata", "fixture-a-000001.events.json"))
	require.NoError(t, err)
	got, err := loadFixture(t, "fixture-a-000001.json").EventsJSON()
	require.NoError(t, err)
	assert.Equal(t, string(want), string(got))
}

func TestRustFixtureTamperingFailsVerify(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(string) string
	}{
		{"payload_text", func(s string) string { return strings.Replace(s, `"floats and ints"`, `"floats and ints!"`, 1) }},
		{"float_value", func(s string) string { return strings.Replace(s, "1e+16", "2e+16", 1) }},
		{"class", func(s string) string { return strings.Replace(s, `"state_change"`, `"health"`, 1) }},
		{"checksum", func(s string) string { return strings.Replace(s, "1796932786", "1796932787", 1) }},
	}
	raw := string(readFixture(t, "fixture-a-000001.json"))
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mutated := tt.mutate(raw)
			require.NotEqual(t, raw, mutated)
			seg, err := ParseSegmentFile([]byte(mutated))
			require.NoError(t, err)
			ok, err := seg.Verify()
			require.NoError(t, err)
			assert.False(t, ok)
		})
	}
}

// serde_json 1.0.149 is built without float_roundtrip, so it parses some
// of its own shortest float output one ulp off: jilog's own verify() of
// this jilog-written file fails (testdata/jilog-verify.txt), while the
// seal CRC over the in-memory value is right. Go parses exactly, so it
// verifies the segment and reproduces the file.
func TestRustLossyFloatFixture(t *testing.T) {
	note, err := os.ReadFile(filepath.Join("testdata", "jilog-verify.txt"))
	require.NoError(t, err)
	assert.Equal(t, "fixture-lossy-000001.json jilog_verify=false\n", string(note))
	seg := loadFixture(t, "fixture-lossy-000001.json")
	ok, err := seg.Verify()
	require.NoError(t, err)
	assert.True(t, ok)
}

// An equivalent literal re-serializes to the same canonical bytes, so the
// CRC still matches: verify hashes serde's output, never the file bytes.
func TestRustFixtureEquivalentLiteralStillVerifies(t *testing.T) {
	raw := strings.Replace(string(readFixture(t, "fixture-a-000001.json")), "1e+16", "1E16", 1)
	seg, err := ParseSegmentFile([]byte(raw))
	require.NoError(t, err)
	ok, err := seg.Verify()
	require.NoError(t, err)
	assert.True(t, ok)
}

// TestSerdeF64Corpus pins serde_json 1.0.149's float output (zmij: exponent
// with an explicit sign, decimal notation for 1e-5 <= |x| < 1e16) through
// serdejson, from both the serde literal and Go's shortest form.
func TestSerdeF64Corpus(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "serde-f64.txt"))
	require.NoError(t, err)
	defer f.Close()
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		bitsHex, want, ok := strings.Cut(sc.Text(), " ")
		require.True(t, ok)
		bits, err := strconv.ParseUint(bitsHex, 16, 64)
		require.NoError(t, err)
		v := math.Float64frombits(bits)
		fromSerde, err := serdejson.Compact(serdejson.Number(want))
		require.NoError(t, err)
		require.Equal(t, want, string(fromSerde), "bits %s", bitsHex)
		fromGo, err := serdejson.Compact(serdejson.Number(strconv.FormatFloat(v, 'g', -1, 64)))
		require.NoError(t, err)
		require.Equal(t, want, string(fromGo), "bits %s", bitsHex)
		n++
	}
	require.NoError(t, sc.Err())
	assert.Equal(t, 2000, n)
}

func TestSerdeNumberEdgeCases(t *testing.T) {
	tests := []struct{ in, want string }{
		{"1e2", "100.0"},
		{"1E2", "100.0"},
		{"1.50", "1.5"},
		{"0.1e1", "1.0"},
		{"-0", "-0.0"},
		{"-0.0", "-0.0"},
		{"18446744073709551615", "18446744073709551615"},
		{"18446744073709551616", "1.8446744073709552e+19"},
		{"-9223372036854775808", "-9223372036854775808"},
		{"-9223372036854775809", "-9.223372036854776e+18"},
		{"9999999999999998.0", "9999999999999998.0"},
		{"1e16", "1e+16"},
		{"0.00001", "0.00001"},
		{"0.000001", "1e-6"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := serdejson.Compact(serdejson.Number(tt.in))
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(got))
		})
	}
}

func TestParseEventJSONRoundTrip(t *testing.T) {
	for _, seg := range []string{"fixture-a-000001.json", "host-with-dash-000007.json"} {
		for i, e := range loadFixture(t, seg).Events {
			b, err := e.MarshalSerde()
			require.NoError(t, err)
			back, err := ParseEventJSON(b)
			require.NoError(t, err, "%s[%d]", seg, i)
			again, err := back.MarshalSerde()
			require.NoError(t, err)
			assert.Equal(t, string(b), string(again), "%s[%d]", seg, i)
		}
	}
	_, err := ParseEventJSON([]byte(`[]`))
	require.ErrorContains(t, err, "event is not an object")
}

// A file re-indented by another tool (CRLF, 4 spaces, trailing newline)
// parses, verifies and re-marshals to jilog's canonical bytes.
func TestForeignWhitespaceNormalizes(t *testing.T) {
	raw := string(readFixture(t, "fixture-a-000001.json"))
	reindented := strings.ReplaceAll(raw, "\n  ", "\r\n    ") + "\n"
	seg, err := ParseSegmentFile([]byte(reindented))
	require.NoError(t, err)
	ok, err := seg.Verify()
	require.NoError(t, err)
	assert.True(t, ok)
	out, err := MarshalSegmentFile(seg)
	require.NoError(t, err)
	assert.Equal(t, raw, string(out))
}

// A newer writer's extra event field is dropped on parse, exactly like
// serde without deny_unknown_fields; the CRC was computed without it.
func TestUnknownEventFieldIsDropped(t *testing.T) {
	raw := strings.Replace(string(readFixture(t, "fixture-a-000001.json")),
		`"zone": "default",`, `"zone": "default", "added_later": 1,`, 1)
	seg, err := ParseSegmentFile([]byte(raw))
	require.NoError(t, err)
	ok, err := seg.Verify()
	require.NoError(t, err)
	assert.True(t, ok, "serde drops unknown fields before hashing, so the CRC still matches")
	out, err := MarshalSegmentFile(seg)
	require.NoError(t, err)
	assert.Equal(t, string(readFixture(t, "fixture-a-000001.json")), string(out))
}

// created_at written with an offset is the same instant: it is stored in
// chrono's canonical Z form and still content-matches.
func TestCreatedAtOffsetCanonicalizes(t *testing.T) {
	raw := string(readFixture(t, "fixture-a-000001.json"))
	orig, err := ParseSegmentFile([]byte(raw))
	require.NoError(t, err)
	shifted := strings.Replace(raw, `"created_at": "2026-01-02T03:05:00Z"`,
		`"created_at": "2026-01-02T04:05:00+01:00"`, 1)
	require.NotEqual(t, raw, shifted)
	seg, err := ParseSegmentFile([]byte(shifted))
	require.NoError(t, err)
	assert.Equal(t, "2026-01-02T03:05:00Z", seg.CreatedAt)
	assert.True(t, seg.ContentMatches(orig))
}

// This roundtrip moved from Task 2 because it needs the file codec built here.
func TestSegmentFileRoundTrip(t *testing.T) {
	seg := NewSegment("host-a", 1, segNow)
	seg.Append(withID(testEvent("zone-a", 1), 1))
	seg.Append(withID(testEvent("zone-a", 2), 2))
	require.NoError(t, seg.Seal())
	b, err := MarshalSegmentFile(seg)
	require.NoError(t, err)
	loaded, err := ParseSegmentFile(b)
	require.NoError(t, err)
	assert.Equal(t, "host-a", loaded.Source)
	assert.Equal(t, uint64(1), loaded.SourceSeq)
	assert.Len(t, loaded.Events, 2)
	assert.Equal(t, seg.Checksum, loaded.Checksum)
	ok, err := loaded.Verify()
	require.NoError(t, err)
	assert.True(t, ok)
	assert.True(t, loaded.ContentMatches(seg))
}
