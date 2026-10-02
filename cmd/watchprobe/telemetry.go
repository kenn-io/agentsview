package main

import (
	"context"
	"encoding/json/v2"
	"math/bits"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/process"
)

type histogram struct {
	buckets                      [64]uint64
	count, total, min, max, zero uint64
}

func (h *histogram) observe(d time.Duration) {
	n := uint64(max(d.Nanoseconds(), 0))
	h.count++
	h.total += n
	if h.count == 1 || n < h.min {
		h.min = n
	}
	h.max = max(h.max, n)
	if n == 0 {
		h.zero++
		return
	}
	h.buckets[bits.Len64(n)-1]++
}

type latency struct {
	Count, ZeroDurationCount                                 uint64
	MeanNS, MinNS, MaxNS, P50UpperNS, P95UpperNS, P99UpperNS uint64
}

func (h *histogram) summary() latency {
	if h.count == 0 {
		return latency{}
	}
	quantile := func(percent uint64) uint64 {
		target := (h.count*percent + 99) / 100
		if target <= h.zero {
			return 0
		}
		cumulative := h.zero
		for i, n := range h.buckets {
			cumulative += n
			if cumulative >= target {
				if i == 63 {
					return ^uint64(0)
				}
				return uint64(1) << (i + 1)
			}
		}
		return h.max
	}
	return latency{h.count, h.zero, h.total / h.count, h.min, h.max, quantile(50), quantile(95), quantile(99)}
}

type resourceSample struct {
	ElapsedS, ProcessCPUS, HostCPUUsedS, HostCPUTotalS                          float64
	HeapAlloc, HeapInuse, TotalAlloc, Mallocs, RSS, HostAvailable, HostSwapUsed uint64
	Goroutines, GC                                                              uint64
	ResourceErrors                                                              int
	OpenDescriptorsOrHandles                                                    int32
}
type recorder struct {
	mu      sync.Mutex
	file    *os.File
	process *process.Process
	start   time.Time
	peak    resourceSample
	samples int64
}

func newRecorder(ctx context.Context, path string) (*recorder, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	p, err := process.NewProcessWithContext(ctx, int32(os.Getpid()))
	if err != nil {
		f.Close()
		return nil, err
	}
	return &recorder{file: f, process: p, start: time.Now()}, nil
}
func (r *recorder) snapshot(ctx context.Context) resourceSample {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	s := resourceSample{ElapsedS: time.Since(r.start).Seconds(), HeapAlloc: m.HeapAlloc, HeapInuse: m.HeapInuse, TotalAlloc: m.TotalAlloc, Mallocs: m.Mallocs, GC: uint64(m.NumGC), Goroutines: uint64(runtime.NumGoroutine())}
	if v, e := r.process.TimesWithContext(ctx); e == nil {
		s.ProcessCPUS = v.User + v.System
	} else {
		s.ResourceErrors++
	}
	if v, e := r.process.MemoryInfoWithContext(ctx); e == nil {
		s.RSS = v.RSS
	} else {
		s.ResourceErrors++
	}
	if v, e := r.process.NumFDsWithContext(ctx); e == nil {
		s.OpenDescriptorsOrHandles = v
	} else {
		s.ResourceErrors++
	}
	if v, e := mem.VirtualMemoryWithContext(ctx); e == nil {
		s.HostAvailable = v.Available
	} else {
		s.ResourceErrors++
	}
	if v, e := mem.SwapMemoryWithContext(ctx); e == nil {
		s.HostSwapUsed = v.Used
	} else {
		s.ResourceErrors++
	}
	if v, e := cpu.TimesWithContext(ctx, false); e == nil && len(v) > 0 {
		t := v[0]
		s.HostCPUTotalS = t.User + t.System + t.Idle + t.Nice + t.Iowait + t.Irq + t.Softirq + t.Steal
		s.HostCPUUsedS = s.HostCPUTotalS - t.Idle - t.Iowait
	} else {
		s.ResourceErrors++
	}
	return s
}
func (r *recorder) emit(kind string, value any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := json.MarshalWrite(r.file, struct {
		Kind  string
		Value any
	}{kind, value}); err != nil {
		return err
	}
	_, err := r.file.WriteString("\n")
	return err
}
func (r *recorder) sample(ctx context.Context) error {
	s := r.snapshot(ctx)
	r.mu.Lock()
	r.samples++
	r.peak.HeapAlloc = max(r.peak.HeapAlloc, s.HeapAlloc)
	r.peak.HeapInuse = max(r.peak.HeapInuse, s.HeapInuse)
	r.peak.RSS = max(r.peak.RSS, s.RSS)
	r.peak.Goroutines = max(r.peak.Goroutines, s.Goroutines)
	r.peak.OpenDescriptorsOrHandles = max(r.peak.OpenDescriptorsOrHandles, s.OpenDescriptorsOrHandles)
	r.mu.Unlock()
	return r.emit("resource", s)
}
func (r *recorder) close() error { return r.file.Close() }
