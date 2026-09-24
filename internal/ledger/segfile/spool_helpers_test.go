package segfile

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/ledger"
)

// spoolSegment ports the jilog test helper sealed_segment (renamed so it
// does not clash with PR 13's sealedSegment in this package)
// (commands/spool.rs:683-703, ingester.rs:400-405). Every call mints fresh
// event IDs and timestamps, so two calls with the same identity differ in
// content, as they do in jilog.
func spoolSegment(t *testing.T, source string, seq uint64, nEvents int) ledger.Segment {
	t.Helper()
	seg := ledger.NewSegment(source, seq, time.Now())
	for i := range nEvents {
		actor := fmt.Sprintf("test-%d", i)
		seg.Append(ledger.Event{
			EventID:     ledger.NewEventID(),
			Zone:        "test-zone",
			Source:      source,
			SourceSeq:   seq,
			Timestamp:   time.Now().UTC(),
			ActorRef:    &actor,
			EventClass:  ledger.ClassHealth,
			PayloadTier: ledger.TierMetadataOnly,
		})
	}
	require.NoError(t, seg.Seal())
	return seg
}

// writeSegmentFile has write_to_file's replace semantics (segment.rs:290-304):
// tests use it to plant files that the no-clobber paths must not overwrite.
func writeSegmentFile(t *testing.T, path string, seg ledger.Segment) {
	t.Helper()
	b, err := ledger.MarshalSegmentFile(seg)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, b, 0o644))
}

// tamperChecksum rewrites the checksum field so the file still parses but
// Verify fails, exactly like the jilog tests (ingester.rs:763-771).
func tamperChecksum(t *testing.T, path string, seg ledger.Segment) {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	old := fmt.Sprintf("\"checksum\": %d", seg.Checksum)
	require.Contains(t, string(b), old)
	require.NoError(t, os.WriteFile(path,
		[]byte(strings.Replace(string(b), old, "\"checksum\": 99999", 1)), 0o644))
}

func dirCount(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	require.NoError(t, err)
	return len(entries)
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// skipIfPermissionsIneffective mirrors jilog's running_as_root skip
// (ledger-core test_support.rs:13-16): read-only modes cannot induce
// failures as root, and Windows ignores POSIX modes.
func skipIfPermissionsIneffective(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits do not apply on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits cannot induce failures")
	}
}

// memLedger is an in-memory ledger store implementing Lister and Appender
// with PR 13's contract: identical content is AlreadyIdentical, different
// content under the same identity wraps ledger.ErrIntegrity.
type memLedger struct {
	mu   sync.Mutex
	segs map[string]map[uint64]ledger.Segment // key: zone + "\x00" + source
	// failAt makes ListLedgerSegments fail once a page would reach this
	// seq for the source, modeling a row that cannot be decoded.
	failAt map[string]uint64
}

func newMemLedger() *memLedger {
	return &memLedger{
		segs:   map[string]map[uint64]ledger.Segment{},
		failAt: map[string]uint64{},
	}
}

func memKey(zone, source string) string { return zone + "\x00" + source }

// put stores a segment verbatim, bypassing every check (used to plant
// corrupt store copies).
func (m *memLedger) put(zone string, seg ledger.Segment) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := memKey(zone, seg.Source)
	if m.segs[k] == nil {
		m.segs[k] = map[uint64]ledger.Segment{}
	}
	m.segs[k][seg.SourceSeq] = seg
}

func (m *memLedger) count(zone string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for k, bySeq := range m.segs {
		if strings.HasPrefix(k, zone+"\x00") {
			n += len(bySeq)
		}
	}
	return n
}

func (m *memLedger) ListLedgerSegments(
	_ context.Context, zone, source string, afterSeq uint64, limit int,
) ([]ledger.Segment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	bySeq := m.segs[memKey(zone, source)]
	seqs := make([]uint64, 0, len(bySeq))
	for seq := range bySeq {
		if seq > afterSeq {
			seqs = append(seqs, seq)
		}
	}
	slices.Sort(seqs)
	if bad, ok := m.failAt[source]; ok && afterSeq+1 >= bad {
		return nil, fmt.Errorf("decode ledger_segments row %s:%d: corrupt events_json", source, bad)
	}
	out := []ledger.Segment{}
	for _, seq := range seqs {
		if bad, ok := m.failAt[source]; ok && seq >= bad {
			break
		}
		if len(out) == limit {
			break
		}
		out = append(out, bySeq[seq])
	}
	return out, nil
}

func (m *memLedger) AppendLedgerSegment(
	_ context.Context, zone string, seg ledger.Segment, _ string,
) (ledger.PublishOutcome, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := memKey(zone, seg.Source)
	if m.segs[k] == nil {
		m.segs[k] = map[uint64]ledger.Segment{}
	}
	if prior, ok := m.segs[k][seg.SourceSeq]; ok {
		if prior.ContentMatches(seg) {
			return ledger.AlreadyIdentical, nil
		}
		return ledger.Published, fmt.Errorf("%w: segment %s:%d differs from the stored copy",
			ledger.ErrIntegrity, seg.Source, seg.SourceSeq)
	}
	m.segs[k][seg.SourceSeq] = seg
	return ledger.Published, nil
}

// LedgerSegmentSeqs and LedgerStatus complete PR 13's Appender and Lister
// interfaces; the spool code never calls them.
func (m *memLedger) LedgerSegmentSeqs(_ context.Context, zone, source string) ([]uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	seqs := slices.Collect(maps.Keys(m.segs[memKey(zone, source)]))
	slices.Sort(seqs)
	return seqs, nil
}

func (m *memLedger) LedgerStatus(_ context.Context, zone string) (ledger.ZoneStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := ledger.ZoneStatus{Zone: zone, Sources: map[string]uint64{}}
	for k, bySeq := range m.segs {
		source, ok := strings.CutPrefix(k, zone+"\x00")
		if !ok {
			continue
		}
		for seq, seg := range bySeq {
			st.Segments++
			st.Events += len(seg.Events)
			st.Sources[source] = max(st.Sources[source], seq)
		}
	}
	return st, nil
}
