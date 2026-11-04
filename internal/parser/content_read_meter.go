package parser

import (
	"context"
	"sync/atomic"
)

// ContentReadMeter counts the source bytes a provider reads while it
// fingerprints a source. Fingerprints built from stats or reused stored hashes
// read no content and leave the meter at zero.
type ContentReadMeter struct {
	reads atomic.Int64
	bytes atomic.Int64
}

type contentReadMeterContextKey struct{}

// WithContentReadMeter returns a context whose fingerprint content reads are
// recorded on meter.
func WithContentReadMeter(
	ctx context.Context, meter *ContentReadMeter,
) context.Context {
	return context.WithValue(ctx, contentReadMeterContextKey{}, meter)
}

// Reads reports how many files the provider read.
func (m *ContentReadMeter) Reads() int64 { return m.reads.Load() }

// Bytes reports how many bytes the provider read.
func (m *ContentReadMeter) Bytes() int64 { return m.bytes.Load() }

func recordContentRead(ctx context.Context, n int64) {
	meter, _ := ctx.Value(contentReadMeterContextKey{}).(*ContentReadMeter)
	if meter == nil {
		return
	}
	meter.reads.Add(1)
	meter.bytes.Add(n)
}
