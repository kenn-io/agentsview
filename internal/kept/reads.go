package kept

import (
	"context"
	"encoding/json/v2"
	"sync"

	"go.kenn.io/agentsview/internal/db"
)

// Reads keeps one finished value per selection and version and shares equal loads.
// Its zero value is ready to use.
type Reads[T any] struct {
	// Size overrides JSON sizing. Set it before the first Do call.
	Size    func(T) int64
	once    sync.Once
	memo    Memo[T]
	flights Flights[T]
}

// Do returns a kept value or loads it. Empty slots or versions bypass reuse.
func (r *Reads[T]) Do(ctx context.Context, slot, version string, load func(context.Context) (T, error)) (T, error) {
	if slot == "" || version == "" {
		return load(ctx)
	}
	r.once.Do(func() {
		if r.Size != nil {
			r.memo.Size = r.Size
			return
		}
		r.memo.Size = func(value T) int64 {
			if sized, ok := any(value).(interface{ Bytes() int64 }); ok {
				return sized.Bytes()
			}
			encoded, err := json.Marshal(value)
			if err != nil {
				return memoBytes + 1
			}
			return int64(len(encoded))
		}
	})
	if values, ok := r.memo.Get(slot, version); ok {
		return values[0], nil
	}
	return r.flights.Do(ctx, version+"|"+slot, func(ctx context.Context) (T, error) {
		if values, ok := r.memo.Get(slot, version); ok {
			return values[0], nil
		}
		value, err := load(ctx)
		if err == nil {
			r.memo.Put(slot, version, []T{value})
		}
		return value, err
	})
}

// Clear drops all finished values.
func (r *Reads[T]) Clear() {
	r.memo.DeleteSlots(func(string) bool { return true })
}

// UsageSlot names the selection, excluding options applied after the read.
// Time-dependent or unencodable filters bypass reuse.
func UsageSlot(f db.UsageFilter, keepBreakdowns bool) string {
	if db.TerminationUsesTime(f.Termination) {
		return ""
	}
	f.Progress = nil
	f.ModelAliases = nil
	f.TopSessionsByGroup = false
	f.TopSessionsSort = ""
	f.TopSessionsTokenTypes = 0
	f.SkipSessionCounts = false
	if !keepBreakdowns {
		f.Breakdowns = false
	}
	key, err := json.Marshal(f, json.Deterministic(true), json.FormatNilSliceAsNull(true))
	if err != nil {
		return ""
	}
	return string(key)
}
