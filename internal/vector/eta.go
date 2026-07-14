package vector

import (
	"math"
	"time"
)

const (
	buildETASmoothing          = 0.3
	buildETAMinPositiveSamples = 2
)

type buildETAEstimate struct {
	Ready         bool
	RatePerSecond float64
	Remaining     time.Duration
}

// buildETAEstimator tracks one build phase's progress rate in daemon memory.
// Zero-delta observations do not advance its baseline, so a later positive
// delta includes time spent stalled.
type buildETAEstimator struct {
	phase           string
	lastDone        int64
	lastTotal       int64
	lastObservedAt  time.Time
	ewmaRatePerNano float64
	positiveSamples int
	initialized     bool
}

func (e *buildETAEstimator) sample(
	phase string, done, total int64, observedAt time.Time,
) buildETAEstimate {
	if !e.initialized || phase != e.phase || total != e.lastTotal || done < e.lastDone {
		e.resetTo(phase, done, total, observedAt)
		return buildETAEstimate{}
	}

	delta := done - e.lastDone
	elapsed := observedAt.Sub(e.lastObservedAt)
	if delta > 0 && elapsed > 0 {
		rate := float64(delta) / float64(elapsed)
		if e.positiveSamples == 0 {
			e.ewmaRatePerNano = rate
		} else {
			e.ewmaRatePerNano = buildETASmoothing*rate +
				(1-buildETASmoothing)*e.ewmaRatePerNano
		}
		e.positiveSamples++
		e.lastDone = done
		e.lastObservedAt = observedAt
	}

	return e.estimate()
}

func (e *buildETAEstimator) estimate() buildETAEstimate {
	if e.positiveSamples < buildETAMinPositiveSamples || e.lastTotal <= 0 ||
		e.ewmaRatePerNano <= 0 || math.IsNaN(e.ewmaRatePerNano) ||
		math.IsInf(e.ewmaRatePerNano, 0) {
		return buildETAEstimate{}
	}

	remaining := max(int64(0), e.lastTotal-e.lastDone)
	remainingNanos := float64(remaining) / e.ewmaRatePerNano
	if math.IsNaN(remainingNanos) || math.IsInf(remainingNanos, 0) ||
		remainingNanos > float64(math.MaxInt64) {
		return buildETAEstimate{}
	}

	return buildETAEstimate{
		Ready:         true,
		RatePerSecond: e.ewmaRatePerNano * float64(time.Second),
		Remaining:     time.Duration(math.Round(remainingNanos)),
	}
}

func (e *buildETAEstimator) reset() {
	*e = buildETAEstimator{}
}

func (e *buildETAEstimator) resetTo(
	phase string, done, total int64, observedAt time.Time,
) {
	e.phase = phase
	e.lastDone = done
	e.lastTotal = total
	e.lastObservedAt = observedAt
	e.ewmaRatePerNano = 0
	e.positiveSamples = 0
	e.initialized = true
}
