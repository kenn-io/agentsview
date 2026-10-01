package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/trace"
	"strings"
	"time"
)

const pageRecords = 256

var errInjectedAck = errors.New("injected downstream acknowledgement failure")

type signature struct {
	Size, MtimeNS, ChangeNS    int64
	Volume                     uint64
	Identity                   [16]byte
	IdentityKnown, ChangeKnown bool
}
type record struct {
	Name      string
	Signature signature
}
type scanStats struct {
	Listings, Stats, Changed, Verified, ContentBytes, CacheHits, CacheWrites, AdmissionRefusals, Pages int64
	MaxPageRecords                                                                                     int
	MaxPagePayloadEstimateBytes                                                                        int
}
type probe struct {
	total       scanStats
	cache       *cache
	failNextAck bool
	buffer      [32 << 10]byte
	latencies   map[string]*histogram
	lastHash    [32]byte
}

func sourceName(i int, pattern string) string {
	if pattern == "entropy" {
		return fmt.Sprintf("%x.jsonl", sha256.Sum256([]byte(fmt.Sprint(i))))
	}
	return fmt.Sprintf("rollout-2026-10-01-%045d.jsonl", i)
}
func (p *probe) scan(ctx context.Context, unit int, dir string, force bool, checkpoint func()) (scanStats, error) {
	region := trace.StartRegion(ctx, "scan-directory")
	defer region.End()
	var stats scanStats
	defer func() { addStats(&p.total, stats) }()
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	f, err := os.Open(dir)
	if err != nil {
		return stats, err
	}
	defer f.Close()
	stats.Listings++
	for {
		if err = ctx.Err(); err != nil {
			return stats, err
		}
		names, e := f.Readdirnames(pageRecords)
		if e != nil && !errors.Is(e, io.EOF) {
			return stats, e
		}
		if len(names) == 0 {
			break
		}
		stats.Pages++
		stats.MaxPageRecords = max(stats.MaxPageRecords, len(names))
		candidates := names[:0]
		for _, name := range names {
			if strings.HasSuffix(name, ".jsonl") {
				candidates = append(candidates, name)
			}
		}
		known := map[string]signature{}
		if p.cache != nil {
			start := time.Now()
			known, err = p.cache.load(ctx, unit, candidates)
			p.observe("cache_lookup", time.Since(start))
			if err != nil {
				return stats, err
			}
		}
		pageBytes := 0
		for _, n := range candidates {
			pageBytes += len(n) + 58
		}
		stats.MaxPagePayloadEstimateBytes = max(stats.MaxPagePayloadEstimateBytes, pageBytes)
		changed := make([]record, 0, len(candidates))
		for _, name := range candidates {
			if err = ctx.Err(); err != nil {
				return stats, err
			}
			start := time.Now()
			s, e := freshSignature(filepath.Join(dir, name))
			p.observe("fresh_signature", time.Since(start))
			stats.Stats++
			if e != nil {
				return stats, e
			}
			old, found := known[name]
			if found {
				stats.CacheHits++
			}
			if !found || old != s {
				stats.Changed++
				changed = append(changed, record{Name: name, Signature: s})
			}
			if force {
				n, e := p.verify(ctx, filepath.Join(dir, name))
				if e != nil {
					return stats, e
				}
				stats.Verified++
				stats.ContentBytes += n
			}
		}
		if p.failNextAck {
			p.failNextAck = false
			return stats, errInjectedAck
		}
		if p.cache != nil && len(changed) > 0 {
			start := time.Now()
			admitted, e := p.cache.write(ctx, unit, changed)
			p.observe("cache_write", time.Since(start))
			if e != nil {
				return stats, e
			}
			if admitted {
				stats.CacheWrites += int64(len(changed))
			} else {
				stats.AdmissionRefusals++
			}
		}
		if checkpoint != nil {
			checkpoint()
		}
		if errors.Is(e, io.EOF) {
			break
		}
	}
	return stats, nil
}
func (p *probe) verify(ctx context.Context, path string) (int64, error) {
	start := time.Now()
	defer func() { p.observe("content_verify", time.Since(start)) }()
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	hash := sha256.New()
	var total int64
	for {
		if err = ctx.Err(); err != nil {
			return total, err
		}
		n, e := f.Read(p.buffer[:])
		if n > 0 {
			_, _ = hash.Write(p.buffer[:n])
			total += int64(n)
		}
		if errors.Is(e, io.EOF) {
			copy(p.lastHash[:], hash.Sum(nil))
			return total, nil
		}
		if e != nil {
			return total, e
		}
	}
}

func (p *probe) observe(kind string, d time.Duration) {
	if p.latencies == nil {
		p.latencies = make(map[string]*histogram)
	}
	h := p.latencies[kind]
	if h == nil {
		h = &histogram{}
		p.latencies[kind] = h
	}
	h.observe(d)
}
