package ledger

import (
	"math"
	"slices"
	"strconv"
)

// VerifyCheckpoint is one source's slice of jilog's
// .verify-checkpoint.json (store.rs:536-549): the highest seq that
// verified clean, known failures [source, seq, message] and gaps below
// the watermark [source, seq]. Seqs are decimal strings.
type VerifyCheckpoint struct {
	VerifiedSeq uint64
	Failures    [][3]string
	Missing     [][2]string
}

// VerifyReport ports VerifyReport (store.rs:553-561) for one source.
type VerifyReport struct {
	NewlyVerified int         `json:"newly_verified"`
	Skipped       int         `json:"skipped"`
	Failures      [][3]string `json:"failures"`
}

// ZoneStatus summarizes one zone for `ledger status`.
type ZoneStatus struct {
	Zone     string            `json:"zone"`
	Segments int               `json:"segments"`
	Events   int               `json:"events"`
	Sources  map[string]uint64 `json:"sources"`  // source -> latest seq
	Gaps     [][2]string       `json:"gaps"`     // [source, missing seq]
	Failures [][3]string       `json:"failures"` // [source, seq, message] from the verify state
}

// VerifySource ports verify_with_checkpoint (store.rs:415-492) for one
// source. seqs are the source's present segment seqs; check returns "" for
// a clean segment or jilog's failure text ("checksum mismatch",
// "verify error: …", "read error: …"). A segment is skipped only when it
// is at or below the watermark, is not a prior failure and was not a
// prior gap (a backfill). Pass a zero checkpoint for a full verify.
func VerifySource(source string, seqs []uint64, ckpt VerifyCheckpoint,
	check func(seq uint64) string,
) (VerifyCheckpoint, VerifyReport) {
	sorted := slices.Clone(seqs)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)

	priorFailures := map[uint64]bool{}
	for _, f := range ckpt.Failures {
		if n, err := strconv.ParseUint(f[1], 10, 64); err == nil {
			priorFailures[n] = true
		}
	}
	priorMissing := map[uint64]bool{}
	for _, m := range ckpt.Missing {
		if n, err := strconv.ParseUint(m[1], 10, 64); err == nil {
			priorMissing[n] = true
		}
	}

	watermark := ckpt.VerifiedSeq
	var report VerifyReport
	for _, seq := range sorted {
		if seq <= watermark && !priorFailures[seq] && !priorMissing[seq] {
			report.Skipped++
			continue
		}
		if msg := check(seq); msg != "" {
			report.Failures = append(report.Failures,
				[3]string{source, strconv.FormatUint(seq, 10), msg})
			continue
		}
		report.NewlyVerified++
		watermark = max(watermark, seq)
	}

	var missing [][2]string
	for _, seq := range gapsUpTo(sorted, watermark) {
		missing = append(missing, [2]string{source, strconv.FormatUint(seq, 10)})
	}
	return VerifyCheckpoint{
		VerifiedSeq: watermark,
		Failures:    report.Failures,
		Missing:     missing,
	}, report
}

// MaxGapsPerSource bounds gap enumeration. jilog enumerates every missing
// seq (store.rs:382-384, 471-477), which never finishes for a foreign
// segment with a huge seq; past this many gaps the rest are not listed.
const MaxGapsPerSource = 1000

// DetectGaps ports detect_gaps (store.rs:366-389) for one source: every
// seq from 1 up to the highest present seq that is absent, at most
// MaxGapsPerSource of them.
func DetectGaps(seqs []uint64) []uint64 {
	if len(seqs) == 0 {
		return nil
	}
	return gapsUpTo(seqs, slices.Max(seqs))
}

// gapsUpTo lists the seqs in [1, limit] absent from seqs, at most
// MaxGapsPerSource of them.
func gapsUpTo(seqs []uint64, limit uint64) []uint64 {
	sorted := slices.Clone(seqs)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)
	var gaps []uint64
	full := false
	add := func(seq uint64) {
		if len(gaps) == MaxGapsPerSource {
			full = true
			return
		}
		gaps = append(gaps, seq)
	}
	next := uint64(1)
	for _, seq := range sorted {
		if seq == 0 || seq < next {
			continue
		}
		if seq > limit {
			break
		}
		for s := next; s < seq && !full; s++ {
			add(s)
		}
		if full || seq == math.MaxUint64 {
			return gaps
		}
		next = seq + 1
	}
	for s := next; s <= limit && s != 0 && !full; s++ {
		add(s)
	}
	return gaps
}
