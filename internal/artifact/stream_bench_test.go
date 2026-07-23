package artifact

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"
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
