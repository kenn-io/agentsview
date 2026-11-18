package kept

import (
	"context"
	"encoding/json/v2"
	"sync"
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

func (r *Reads[T]) init() {
	r.once.Do(func() {
		if r.Size != nil {
			r.memo.Size = r.Size
			return
		}
		r.memo.Size = func(value T) int64 {
			if sized, ok := any(value).(interface{ Bytes() int64 }); ok {
				return sized.Bytes()
			}
			return JSONBytes(value)
		}
	})
}

// Get returns a retained value without starting detached work.
func (r *Reads[T]) Get(slot, version string) (T, bool) {
	var zero T
	if slot == "" || version == "" {
		return zero, false
	}
	r.init()
	values, ok := r.memo.Get(slot, version)
	if !ok {
		return zero, false
	}
	return values[0], true
}

// Put publishes a value whose owning operation has completed.
func (r *Reads[T]) Put(slot, version string, value T) {
	if slot == "" || version == "" {
		return
	}
	r.init()
	r.memo.Put(slot, version, []T{value})
}

// Do returns a kept value or loads it. Empty slots or versions bypass reuse.
func (r *Reads[T]) Do(ctx context.Context, slot, version string, load func(context.Context) (T, error)) (T, error) {
	return r.DoChecked(ctx, slot, version, func(ctx context.Context) (T, bool, error) {
		value, err := load(ctx)
		return value, true, err
	})
}

// DoChecked retains a successful load only when it permits retention.
func (r *Reads[T]) DoChecked(ctx context.Context, slot, version string, load func(context.Context) (T, bool, error)) (T, error) {
	return r.do(ctx, slot, version, nil, load)
}

func (r *Reads[T]) do(ctx context.Context, slot, version string, keep func(T), load func(context.Context) (T, bool, error)) (T, error) {
	if err := ctx.Err(); err != nil {
		var zero T
		return zero, err
	}
	if slot == "" || version == "" {
		value, _, err := load(ctx)
		return value, err
	}
	r.init()
	if values, ok := r.memo.Get(slot, version); ok {
		if keep != nil {
			keep(values[0])
		}
		return values[0], nil
	}
	return r.flights.Do(ctx, version+"|"+slot, func(ctx context.Context) (T, error) {
		if values, ok := r.memo.Get(slot, version); ok {
			if keep != nil {
				keep(values[0])
			}
			return values[0], nil
		}
		value, retain, err := load(ctx)
		if err == nil && retain {
			r.memo.Put(slot, version, []T{value})
		}
		return value, err
	})
}

// DoSaved keeps finished reads on disk as well as in memory.
func (r *Reads[T]) DoSaved(ctx context.Context, disk *Disk, slot, memoryVersion, savedVersion string, encode func(T) ([]byte, error), decode func([]byte) (T, error), load func(context.Context) (T, error)) (T, error) {
	if disk == nil || !disk.Enabled() || slot == "" || memoryVersion == "" || savedVersion == "" {
		return r.Do(ctx, slot, memoryVersion, load)
	}
	return r.do(ctx, slot, memoryVersion, func(value T) {
		disk.Keep(slot, savedVersion, func() ([]byte, error) { return encode(value) })
	}, func(ctx context.Context) (T, bool, error) {
		if data, ok := disk.Get(slot, savedVersion); ok {
			if value, err := decode(data); err == nil {
				return value, true, nil
			}
		}
		value, err := load(ctx)
		if err != nil {
			return value, false, err
		}
		if data, err := encode(value); err == nil {
			_ = disk.Put(slot, savedVersion, data)
		}
		return value, true, nil
	})
}

// JSONBytes sizes a public result, exceeding the cache cap on encoding errors.
func JSONBytes(value any) int64 {
	encoded, err := json.Marshal(value)
	if err != nil {
		return DefaultMaxBytes + 1
	}
	return int64(len(encoded))
}

// Clear drops all finished values.
func (r *Reads[T]) Clear() {
	r.memo.DeleteSlots(func(string) bool { return true })
}
