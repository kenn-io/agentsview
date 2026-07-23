package artifact

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"testing"
	"time"

	"go.kenn.io/docbank"
)

var artifactBenchmarkSizes = []struct {
	name string
	size int
}{
	{name: "32KiB", size: 32 << 10},
	{name: "1MiB", size: 1 << 20},
	{name: "16MiB", size: 16 << 20},
}

func BenchmarkWireEncode(b *testing.B) {
	for _, codec := range []struct {
		name string
		kind Kind
	}{
		{name: "identity", kind: KindRaw},
		{name: "zstd", kind: KindSegments},
	} {
		for _, size := range artifactBenchmarkSizes {
			b.Run(codec.name+"/"+size.name, func(b *testing.B) {
				body := deterministicDocbankBytes(size.size)
				ref := benchmarkArtifactRef(b, codec.kind, body)
				b.ReportAllocs()
				b.SetBytes(int64(len(body)))
				b.ResetTimer()
				for b.Loop() {
					if err := EncodeWire(context.Background(), ref, bytes.NewReader(body), io.Discard); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func BenchmarkWireDecode(b *testing.B) {
	for _, codec := range []struct {
		name string
		kind Kind
	}{
		{name: "identity", kind: KindRaw},
		{name: "zstd", kind: KindSegments},
	} {
		for _, size := range artifactBenchmarkSizes {
			b.Run(codec.name+"/"+size.name, func(b *testing.B) {
				body := deterministicDocbankBytes(size.size)
				ref := benchmarkArtifactRef(b, codec.kind, body)
				wire, err := ToWireRef(ref)
				if err != nil {
					b.Fatal(err)
				}
				var encoded bytes.Buffer
				if err := EncodeWire(context.Background(), ref, bytes.NewReader(body), &encoded); err != nil {
					b.Fatal(err)
				}
				limits := WireLimits{
					MaxEncodedBytes: int64(encoded.Len()),
					MaxDecodedBytes: int64(len(body)),
				}
				b.ReportAllocs()
				b.SetBytes(int64(len(body)))
				b.ResetTimer()
				for b.Loop() {
					if err := DecodeWire(context.Background(), wire,
						bytes.NewReader(encoded.Bytes()), io.Discard, limits); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func BenchmarkDocbankVerifiedRead(b *testing.B) {
	for _, size := range artifactBenchmarkSizes {
		b.Run(size.name, func(b *testing.B) {
			ctx := context.Background()
			vault, err := docbank.New(ctx, docbank.Config{
				Root: b.TempDir(),
				LooseCompression: docbank.LooseCompressionOptions{
					Enabled:           true,
					MinBytes:          1,
					MinSavingsPercent: 1,
				},
			})
			if err != nil {
				b.Fatal(err)
			}
			store := newDocbankContent(vault)
			b.Cleanup(func() {
				if err := store.Close(); err != nil {
					b.Error(err)
				}
			})
			pattern := []byte("agent session message with repeated text\n")
			body := bytes.Repeat(pattern, (size.size+len(pattern)-1)/len(pattern))[:size.size]
			ref := benchmarkArtifactRef(b, KindRaw, body)
			identity, err := NewIdentity(hashHex(body), int64(len(body)))
			if err != nil {
				b.Fatal(err)
			}
			created, err := store.Create(ctx, ref, identity,
				canonicalArtifactMediaType(ref.Kind), bytes.NewReader(body))
			if err != nil {
				b.Fatal(err)
			}
			if created.Physical.Encoding != "zstd" {
				b.Fatalf("expected compressed loose object, got %q", created.Physical.Encoding)
			}
			packed, err := store.Pack(ctx, int64(len(body))+1)
			if err != nil {
				b.Fatal(err)
			}
			if packed.PackedObjects != 1 {
				b.Fatalf("expected one packed object, got %d", packed.PackedObjects)
			}

			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			stopHeapSamples := startArtifactHeapSamples(b)
			b.ResetTimer()
			for b.Loop() {
				entry, reader, err := store.Open(ctx, ref)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := io.Copy(io.Discard, reader); err != nil {
					b.Fatal(err)
				}
				if err := reader.Verify(); err != nil {
					b.Fatal(err)
				}
				if err := reader.Close(); err != nil {
					b.Fatal(err)
				}
				if entry.Identity != identity {
					b.Fatalf("verified read identity changed: got %+v want %+v", entry.Identity, identity)
				}
			}
			b.StopTimer()
			stopHeapSamples()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			runtime.GC()
			runtime.ReadMemStats(&after)
			if os.Getenv("AGENTSVIEW_ARTIFACT_PROFILE_SAMPLES") != "" {
				b.Logf("forced-gc heap_alloc_before=%d heap_alloc_after=%d heap_objects_before=%d heap_objects_after=%d",
					before.HeapAlloc, after.HeapAlloc, before.HeapObjects, after.HeapObjects)
			}
			b.ReportMetric(float64(before.HeapAlloc), "live-heap-before-final-gc-B")
			b.ReportMetric(float64(after.HeapAlloc), "live-heap-after-final-gc-B")
			runtime.KeepAlive(body)
		})
	}
}

func startArtifactHeapSamples(b *testing.B) func() {
	b.Helper()
	if os.Getenv("AGENTSVIEW_ARTIFACT_PROFILE_SAMPLES") == "" {
		return func() {}
	}
	started := time.Now()
	done := make(chan struct{})
	stopped := make(chan struct{})
	sample := func(label string) {
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		b.Logf("retention-sample label=%s elapsed=%s heap_alloc=%d heap_objects=%d heap_sys=%d",
			label, time.Since(started).Round(time.Second), stats.HeapAlloc, stats.HeapObjects, stats.HeapSys)
	}
	sample("start")
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				sample("periodic")
			case <-done:
				return
			}
		}
	}()
	return func() {
		close(done)
		<-stopped
		sample("end")
	}
}

func benchmarkArtifactRef(b *testing.B, kind Kind, body []byte) Ref {
	b.Helper()
	name := hashHex(body)
	switch kind {
	case KindSegments:
		name += ".ndjson"
	case KindManifests:
		name += ".json"
	case KindRaw:
	default:
		b.Fatalf("unsupported benchmark artifact kind %q", kind)
	}
	ref, err := NewRef(contractOrigin, kind, name)
	if err != nil {
		b.Fatal(fmt.Errorf("creating benchmark ref: %w", err))
	}
	return ref
}
