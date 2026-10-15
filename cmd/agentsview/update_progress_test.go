package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

const mib = 1 << 20

func TestFormatDownloadProgress(t *testing.T) {
	tests := []struct {
		name       string
		downloaded int64
		total      int64
		elapsed    time.Duration
		want       string
	}{
		{
			name:  "before the first byte",
			total: 100 * mib,
			want:  "  0 B / 100.0 MB (0%)",
		},
		{
			name:       "midway",
			downloaded: 25 * mib,
			total:      100 * mib,
			elapsed:    10 * time.Second,
			want:       "  25.0 MB / 100.0 MB (25%)  2.5 MB/s  0:10 elapsed / ~0:40 total",
		},
		{
			name:       "complete",
			downloaded: 100 * mib,
			total:      100 * mib,
			elapsed:    40 * time.Second,
			want:       "  100.0 MB / 100.0 MB (100%)  2.5 MB/s  0:40 elapsed / ~0:40 total",
		},
		{
			name:       "estimate past an hour",
			downloaded: 1 * mib,
			total:      100 * mib,
			elapsed:    time.Minute,
			want:       "  1.0 MB / 100.0 MB (1%)  17.1 KB/s  1:00 elapsed / ~1:40:00 total",
		},
		{
			name:       "stalled download has no estimate",
			downloaded: 1,
			total:      100 * mib,
			elapsed:    time.Minute,
			want:       "  1 B / 100.0 MB (0%)  0 B/s  1:00 elapsed / --:-- total",
		},
		{
			name:       "unknown size",
			downloaded: 25 * mib,
			elapsed:    10 * time.Second,
			want:       "  25.0 MB  2.5 MB/s  0:10 elapsed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want,
				formatDownloadProgress(tt.downloaded, tt.total, tt.elapsed))
		})
	}
}

func TestDownloadProgressRedrawsInPlace(t *testing.T) {
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	clock := start
	var out bytes.Buffer
	p := newDownloadProgress(&out, func() time.Time { return clock })

	// The first report starts the clock.
	p.update(0, 100*mib)
	clock = start.Add(time.Second)
	p.update(10*mib, 100*mib)
	// Within the redraw interval: skipped.
	clock = start.Add(time.Second + 100*time.Millisecond)
	p.update(11*mib, 100*mib)
	// A slower average shortens the line, so padding clears the old tail.
	clock = start.Add(2 * time.Second)
	p.update(19*mib, 100*mib)
	// Completion draws even within the redraw interval.
	clock = start.Add(2*time.Second + 50*time.Millisecond)
	p.update(100*mib, 100*mib)

	assert.Equal(t,
		"\r  0 B / 100.0 MB (0%)"+
			"\r  10.0 MB / 100.0 MB (10%)  10.0 MB/s  0:01 elapsed / ~0:10 total"+
			"\r  19.0 MB / 100.0 MB (19%)  9.5 MB/s  0:02 elapsed / ~0:11 total "+
			"\r  100.0 MB / 100.0 MB (100%)  48.8 MB/s  0:02 elapsed / ~0:02 total",
		out.String(),
	)
}
