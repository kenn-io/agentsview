package main

import (
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"runtime/trace"
	"sync"
	"time"
)

type options struct {
	Files, Passes, MaxWatches, Queue int
	CacheBytes                       int64
	Output                           string `json:"-"`
	Pattern                          string
	Duration                         time.Duration `json:"-"`
	ScanInterval                     time.Duration `json:"-"`
	Profiles, Trace                  bool
}
type phaseReport struct {
	Name               string
	Runs               int
	WallS, ProcessCPUS float64
	Scan               scanStats
}
type probeReport struct {
	Artifacts                                   string `json:"-"`
	ScanIntervalSeconds                         float64
	Operations                                  scanStats
	Version                                     int
	RequestedDurationSeconds                    float64
	OS, Arch, Go, Revision                      string
	Modified                                    bool
	LogicalCPUs, Directories                    int
	Options                                     options
	Phases                                      []phaseReport
	Checks                                      map[string]bool
	Native                                      nativeReport
	Latencies                                   map[string]latency
	Peak, Final, AfterGC                        resourceSample
	MaxPageRecords, MaxPagePayloadEstimateBytes int
	PeakCacheDiskBytes                          int64
	ResourceSamples                             int64
	NativeObservedDuringCoverage                int64
	Limitations                                 []string
}

func addStats(a *scanStats, b scanStats) {
	a.Listings += b.Listings
	a.Stats += b.Stats
	a.Changed += b.Changed
	a.Verified += b.Verified
	a.ContentBytes += b.ContentBytes
	a.CacheHits += b.CacheHits
	a.CacheWrites += b.CacheWrites
	a.AdmissionRefusals += b.AdmissionRefusals
	a.Pages += b.Pages
	a.MaxPageRecords = max(a.MaxPageRecords, b.MaxPageRecords)
	a.MaxPagePayloadEstimateBytes = max(a.MaxPagePayloadEstimateBytes, b.MaxPagePayloadEstimateBytes)
}
func (p *probe) check(ctx context.Context, unit int, path string) (scanStats, error) {
	var stats scanStats
	defer func() { addStats(&p.total, stats) }()
	start := time.Now()
	s, err := freshSignature(path)
	p.observe("fresh_signature", time.Since(start))
	stats.Stats++
	name := filepath.Base(path)
	if errors.Is(err, os.ErrNotExist) {
		if p.cache != nil {
			err = p.cache.remove(ctx, unit, name)
		} else {
			err = nil
		}
		return stats, err
	}
	if err != nil {
		return stats, err
	}
	var old signature
	found := false
	if p.cache != nil {
		known, e := p.cache.load(ctx, unit, []string{name})
		if e != nil {
			return stats, e
		}
		old, found = known[name]
		if found {
			stats.CacheHits++
		}
	}
	if !found || old != s {
		stats.Changed++
	}
	n, err := p.verify(ctx, path)
	if err != nil {
		return stats, err
	}
	stats.Verified++
	stats.ContentBytes = n
	if p.failNextAck {
		p.failNextAck = false
		return stats, errInjectedAck
	}
	if stats.Changed > 0 && p.cache != nil {
		admitted, e := p.cache.write(ctx, unit, []record{{Name: name, Signature: s}})
		if e != nil {
			return stats, e
		}
		if admitted {
			stats.CacheWrites++
		} else {
			stats.AdmissionRefusals++
		}
	}
	return stats, nil
}
func runProbe(ctx context.Context, o options) (report probeReport, err error) {
	if o.ScanInterval < 0 {
		return report, errors.New("scan interval must not be negative")
	}
	if o.Files < 2 || o.Passes < 1 || o.Passes > 1000 || o.MaxWatches < 0 || o.Queue < 1 || o.Queue > 8192 || o.Duration < 0 || o.CacheBytes < 0 || o.CacheBytes > 0 && o.CacheBytes < 128<<10 {
		return report, errors.New("invalid workload bounds")
	}
	if o.Pattern != "repetitive" && o.Pattern != "entropy" {
		return report, errors.New("pattern must be repetitive or entropy")
	}
	if o.Output == "" {
		o.Output, err = os.MkdirTemp("", "agentsview-watchprobe-")
	} else {
		err = os.Mkdir(o.Output, 0o700)
	}
	if err != nil {
		return report, err
	}
	report = probeReport{Artifacts: o.Output, ScanIntervalSeconds: o.ScanInterval.Seconds(), Version: 3, RequestedDurationSeconds: o.Duration.Seconds(), OS: runtime.GOOS, Arch: runtime.GOARCH, Go: runtime.Version(), LogicalCPUs: runtime.NumCPU(), Options: o, Checks: make(map[string]bool), Latencies: make(map[string]latency), Limitations: []string{"Synthetic file sources only; no archive, provider parsing, or production coordinator.", "Native backend is fsnotify; Darwin uses kqueue, not production FSEvents.", "Injected loss/retry are separate from measured native delivery.", "Cache main file is page-capped; sampled auxiliary disk is not a hard peak guarantee.", "RSS is not retained Go heap or macOS physical footprint; inspect AfterGC and platform tools.", "Percentile values are upper bounds from fixed logarithmic buckets; phase times are exact durations."}}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				report.Revision = s.Value
			case "vcs.modified":
				report.Modified = s.Value == "true"
			}
		}
	}
	defer func() {
		file, e := os.OpenFile(filepath.Join(o.Output, "report.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if e == nil {
			e = errors.Join(json.MarshalWrite(file, report), file.Close())
		}
		err = errors.Join(err, e)
	}()
	rec, e := newRecorder(ctx, filepath.Join(o.Output, "metrics.jsonl"))
	if e != nil {
		return report, e
	}
	defer func() { err = errors.Join(err, rec.close()) }()
	sampleCtx, sampleCancel := context.WithCancel(ctx)
	var sampleWG sync.WaitGroup
	sampleErrors := make(chan error, 1)
	sampleWG.Go(func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-sampleCtx.Done():
				return
			case <-ticker.C:
				if e := rec.sample(sampleCtx); e != nil {
					sampleErrors <- e
					return
				}
			}
		}
	})
	defer func() {
		sampleCancel()
		sampleWG.Wait()
		select {
		case e := <-sampleErrors:
			err = errors.Join(err, e)
		default:
		}
	}()
	// Profiles contain only this synthetic process. Trace is limited to core scenarios.
	if o.Profiles {
		f, e := os.OpenFile(filepath.Join(o.Output, "cpu.pprof"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if e != nil {
			return report, e
		}
		if e = pprof.StartCPUProfile(f); e != nil {
			f.Close()
			return report, e
		}
		defer func() { pprof.StopCPUProfile(); err = errors.Join(err, f.Close()) }()
	}
	var traceFile *os.File
	if o.Trace {
		traceFile, e = os.OpenFile(filepath.Join(o.Output, "trace.out"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if e != nil {
			return report, e
		}
		if e = trace.Start(traceFile); e != nil {
			traceFile.Close()
			return report, e
		}
		defer func() {
			if traceFile != nil {
				trace.Stop()
				err = errors.Join(err, traceFile.Close())
			}
		}()
	}
	if o.ScanInterval == 0 {
		o.ScanInterval = 30 * time.Second
		report.ScanIntervalSeconds = 30
	}
	sources := filepath.Join(o.Output, "sources")
	if err = os.Mkdir(sources, 0o700); err != nil {
		return report, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(sources)) }()
	p := probe{}
	if o.CacheBytes > 0 {
		p.cache, e = openCache(ctx, filepath.Join(o.Output, "cache.sqlite"), o.CacheBytes)
		if e != nil {
			return report, e
		}
		defer func() {
			if p.cache != nil {
				err = errors.Join(err, p.cache.close())
			}
		}()
	}
	phase := func(name string, work func() (scanStats, error)) error {
		before := rec.snapshot(ctx)
		start := time.Now()
		region := trace.StartRegion(ctx, name)
		s, e := work()
		elapsed := time.Since(start)
		region.End()
		after := rec.snapshot(ctx)
		found := -1
		for i, x := range report.Phases {
			if x.Name == name {
				found = i
				break
			}
		}
		if found < 0 {
			report.Phases = append(report.Phases, phaseReport{Name: name})
			found = len(report.Phases) - 1
		}
		pr := &report.Phases[found]
		pr.Runs++
		pr.WallS += elapsed.Seconds()
		pr.ProcessCPUS += max(0, after.ProcessCPUS-before.ProcessCPUS)
		addStats(&pr.Scan, s)
		report.MaxPageRecords = max(report.MaxPageRecords, s.MaxPageRecords)
		report.MaxPagePayloadEstimateBytes = max(report.MaxPagePayloadEstimateBytes, s.MaxPagePayloadEstimateBytes)
		var disk int64
		for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
			if v, e := os.Stat(filepath.Join(o.Output, "cache.sqlite") + suffix); e == nil {
				disk += v.Size()
			}
		}
		report.PeakCacheDiskBytes = max(report.PeakCacheDiskBytes, disk)
		if emit := rec.emit("phase", struct {
			Name    string
			Scan    scanStats
			Seconds float64
		}{name, s, elapsed.Seconds()}); emit != nil {
			return emit
		}
		if sample := rec.sample(ctx); sample != nil {
			return sample
		}
		return e
	}
	dirs := make([]string, (o.Files+pageRecords-1)/pageRecords)
	report.Directories = len(dirs)
	err = phase("generate", func() (scanStats, error) {
		for i := range o.Files {
			if e := ctx.Err(); e != nil {
				return scanStats{}, e
			}
			unit := i / pageRecords
			if i%pageRecords == 0 {
				dirs[unit] = filepath.Join(sources, fmt.Sprintf("unit-%06d", unit))
				if e := os.Mkdir(dirs[unit], 0o700); e != nil {
					return scanStats{}, e
				}
			}
			if e := os.WriteFile(filepath.Join(dirs[unit], sourceName(i, o.Pattern)), []byte("0000"), 0o600); e != nil {
				return scanStats{}, e
			}
		}
		return scanStats{}, nil
	})
	if err != nil {
		return report, err
	}
	native, e := startNative(dirs, o.MaxWatches, o.Queue)
	if e != nil {
		return report, e
	}
	defer func() { err = errors.Join(err, native.close()) }()
	report.Native.Allocated = native.allocated
	report.Native.Unavailable = native.unavailable
	report.Native.BudgetExcluded = native.budgetExcluded
	report.Native.AllocationFailures = native.allocationFailures
	observed := int64(0)
	var equalSignature, changedSignature int64
	nativeState := func() nativeReport {
		state := native.snapshot()
		state.Observed = observed
		state.VerifiedEqualSignature = equalSignature
		state.VerifiedChangedSignature = changedSignature
		return state
	}
	var checkpointErr error
	drain := func() {
		if checkpointErr != nil {
			return
		}
		report.Native.MaxQueue = max(report.Native.MaxQueue, len(native.queue))
		pending := make(map[struct {
			unit int
			name string
		}]notice)
		for range o.Queue {
			select {
			case n := <-native.queue:
				key := struct {
					unit int
					name string
				}{n.Unit, n.Name}
				if _, ok := pending[key]; !ok {
					pending[key] = n
				}
			default:
				goto collected
			}
		}
	collected:
		for _, n := range pending {
			stats, e := p.check(ctx, n.Unit, filepath.Join(dirs[n.Unit], n.Name))
			if e != nil {
				checkpointErr = e
				return
			}
			observed++
			if stats.Verified != 0 {
				if stats.Changed == 0 {
					equalSignature++
				} else {
					changedSignature++
				}
			}
			p.observe("native_receipt_to_verified", time.Since(n.Received))
		}
	}
	scanAll := func(force bool, uncovered bool) (scanStats, error) {
		var total scanStats
		for i, d := range dirs {
			if uncovered && native.watched[i] {
				continue
			}
			s, e := p.scan(ctx, i, d, force, drain)
			addStats(&total, s)
			if e != nil {
				return total, e
			}
			if checkpointErr != nil {
				return total, checkpointErr
			}
		}
		return total, nil
	}
	if err = phase("cold_scan", func() (scanStats, error) { return scanAll(false, false) }); err != nil {
		return report, err
	}
	for range o.Passes {
		if err = phase("warm_scan", func() (scanStats, error) { return scanAll(false, false) }); err != nil {
			return report, err
		}
	}
	// Same source and same metadata work, without cache lookup or admission.
	if p.cache != nil {
		saved := p.cache
		p.cache = nil
		err = phase("uncached_scan", func() (scanStats, error) { return scanAll(false, false) })
		p.cache = saved
		if err != nil {
			return report, err
		}
	}
	target := filepath.Join(dirs[0], sourceName(0, o.Pattern))
	writer, e := os.OpenFile(target, os.O_WRONLY|os.O_APPEND, 0)
	if e != nil {
		return report, e
	}
	_, e = writer.Write([]byte("1"))
	if e != nil {
		writer.Close()
		return report, e
	}
	err = phase("open_writer_append", func() (scanStats, error) {
		s, e := p.check(ctx, 0, target)
		if e == nil {
			fresh, e := freshSignature(target)
			report.Checks["open_writer_append"] = e == nil && fresh.Size == 5 && p.lastHash == sha256.Sum256([]byte("00001"))
		}
		return s, e
	})
	err = errors.Join(err, writer.Close())
	if err != nil {
		return report, err
	}
	if native.watched[0] {
		deadline := time.NewTimer(5 * time.Second)
		defer deadline.Stop()
		for observed == 0 {
			select {
			case <-ctx.Done():
				return report, ctx.Err()
			case <-deadline.C:
				report.Checks["native_delivery"] = false
				goto delivered
			case <-time.After(5 * time.Millisecond):
				drain()
				if checkpointErr != nil {
					return report, checkpointErr
				}
			}
		}
		report.Checks["native_delivery"] = true
	}
delivered:
	if err = phase("coverage_unwatched", func() (scanStats, error) { return scanAll(false, true) }); err != nil {
		return report, err
	}
	if err = os.WriteFile(target, []byte("22222"), 0o600); err != nil {
		return report, err
	}
	if p.cache != nil {
		current, e := freshSignature(target)
		if e != nil {
			return report, e
		}
		_, e = p.cache.write(ctx, 0, []record{{Name: filepath.Base(target), Signature: current}})
		if e != nil {
			return report, e
		}
	}
	err = phase("explicit_dirty_equal_signature", func() (scanStats, error) {
		s, e := p.check(ctx, 0, target)
		report.Checks["explicit_dirty_same_stat_verified"] = e == nil && s.Verified == 1 && p.lastHash == sha256.Sum256([]byte("22222"))
		return s, e
	})
	if err != nil {
		return report, err
	}
	if err = os.WriteFile(target, []byte("retry-literal"), 0o600); err != nil {
		return report, err
	}
	p.failNextAck = true
	_, e = p.check(ctx, 0, target)
	if !errors.Is(e, errInjectedAck) {
		return report, errors.New("retry scenario did not fail at acknowledgement")
	}
	err = phase("retry", func() (scanStats, error) {
		s, e := p.check(ctx, 0, target)
		report.Checks["retry_preserves_baseline"] = e == nil && s.Changed == 1 && p.lastHash == sha256.Sum256([]byte("retry-literal"))
		return s, e
	})
	if err != nil {
		return report, err
	}
	if p.cache != nil {
		err = phase("cache_reopen", func() (scanStats, error) {
			if e := p.cache.close(); e != nil {
				return scanStats{}, e
			}
			p.cache = nil
			var e error
			p.cache, e = openCache(ctx, filepath.Join(o.Output, "cache.sqlite"), o.CacheBytes)
			if e != nil {
				return scanStats{}, e
			}
			s, e := p.scan(ctx, 0, dirs[0], false, nil)
			report.Checks["restart_preserves_baseline"] = e == nil && s.Changed == 0
			return s, e
		})
		if err != nil {
			return report, err
		}
	}
	err = phase("injected_lost_history", func() (scanStats, error) {
		s, e := p.scan(ctx, 0, dirs[0], true, nil)
		report.Checks["loss_verifies_owned_content"] = e == nil && s.Verified == int64(min(o.Files, pageRecords))
		return s, e
	})
	if err != nil {
		return report, err
	}
	// Report actual overlap; native delivery timing differs across platforms.
	if err = os.WriteFile(target, []byte("during-coverage"), 0o600); err != nil {
		return report, err
	}
	beforeCoverage := observed
	if err = phase("coverage_with_events", func() (scanStats, error) { return scanAll(false, false) }); err != nil {
		return report, err
	}
	report.NativeObservedDuringCoverage = observed - beforeCoverage
	if err = phase("file_lifecycle", func() (scanStats, error) {
		return p.lifecycle(ctx, 0, target, report.Checks)
	}); err != nil {
		return report, err
	}
	if traceFile != nil {
		trace.Stop()
		err = traceFile.Close()
		traceFile = nil
		if err != nil {
			return report, err
		}
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	end := time.Now().Add(o.Duration)
	nextCoverage := time.Now()
	var uncoveredPending time.Time
	uncoveredUnit := -1
	for i := range dirs {
		if !native.watched[i] {
			uncoveredUnit = i
			break
		}
	}
	for time.Now().Before(end) {
		select {
		case <-ctx.Done():
			return report, ctx.Err()
		case <-ticker.C:
			if err = rec.emit("native", nativeState()); err != nil {
				return report, err
			}
			if err = os.WriteFile(target, []byte("sustained-activity"), 0o600); err != nil {
				return report, err
			}
			drain()
			if checkpointErr != nil {
				return report, checkpointErr
			}
			if uncoveredUnit >= 0 {
				if uncoveredPending.IsZero() {
					uncoveredPending = time.Now()
				}
				if err = os.WriteFile(filepath.Join(dirs[uncoveredUnit], sourceName(uncoveredUnit*pageRecords, o.Pattern)), []byte("unwatched-activity"), 0o600); err != nil {
					return report, err
				}
			}
			if !time.Now().Before(nextCoverage) {
				if err = phase("sustained_coverage", func() (scanStats, error) { return scanAll(false, true) }); err != nil {
					return report, err
				}
				if !uncoveredPending.IsZero() {
					p.observe("unwatched_mutation_to_coverage", time.Since(uncoveredPending))
					uncoveredPending = time.Time{}
				}
				nextCoverage = time.Now().Add(o.ScanInterval)
			}
		}
	}
	drain()
	if checkpointErr != nil {
		return report, checkpointErr
	}
	for i := range native.loss {
		if stamp := native.loss[i].Swap(0); stamp != 0 {
			if err = phase("native_loss_recovery", func() (scanStats, error) { return p.scan(ctx, i, dirs[i], true, nil) }); err != nil {
				return report, err
			}
			p.observe("native_loss_mark_to_recovered", time.Since(native.started)-time.Duration(stamp-1))
		}
	}
	report.Native = nativeState()
	for k, h := range p.latencies {
		report.Latencies[k] = h.summary()
	}
	sampleCancel()
	sampleWG.Wait()
	report.Operations = p.total
	report.Final = rec.snapshot(ctx)
	runtime.GC()
	report.AfterGC = rec.snapshot(ctx)
	rec.mu.Lock()
	report.Peak = rec.peak
	report.ResourceSamples = rec.samples
	rec.mu.Unlock()
	if o.Profiles {
		f, e := os.OpenFile(filepath.Join(o.Output, "heap.pprof"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if e != nil {
			return report, e
		}
		err = errors.Join(pprof.WriteHeapProfile(f), f.Close())
		if err != nil {
			return report, err
		}
	}
	// Cache and source deletion is limited to the tester-owned new directory.
	for key, ok := range report.Checks {
		if !ok {
			return report, fmt.Errorf("scenario check failed: %s", key)
		}
	}
	return report, nil
}
