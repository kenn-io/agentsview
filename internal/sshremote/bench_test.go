package sshremote

import (
	"bytes"
	"io"
	"os"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/remotesync"
)

// BenchmarkServeArchiveZstd measures response size and time per zstd level.
// Point AGENTSVIEW_BENCH_REMOTE_HOME at a scratch copy of real agent data;
// never at a live home directory (docs/agents/background-work.md).
func BenchmarkServeArchiveZstd(b *testing.B) {
	home := os.Getenv("AGENTSVIEW_BENCH_REMOTE_HOME")
	if home == "" {
		b.Skip("set AGENTSVIEW_BENCH_REMOTE_HOME to a scratch copy of agent data")
	}
	b.Setenv("HOME", home)
	cfg, err := config.LoadReadOnly()
	require.NoError(b, err)
	targets, err := remotesync.ResolveTargets(cfg)
	require.NoError(b, err)
	var raw countingWriter
	require.NoError(b, remotesync.WriteArchive(b.Context(), &raw, targets))
	for _, level := range []zstd.EncoderLevel{
		zstd.SpeedFastest, zstd.SpeedDefault,
		zstd.SpeedBetterCompression, zstd.SpeedBestCompression,
	} {
		var compressed bytes.Buffer
		b.Run(level.String()+"/encode", func(b *testing.B) {
			for b.Loop() {
				compressed.Reset()
				zw, err := remotesync.NewZstdWriter(&compressed, level)
				require.NoError(b, err)
				require.NoError(b, remotesync.WriteArchive(b.Context(), zw, targets))
				require.NoError(b, zw.Close())
			}
			b.ReportMetric(float64(raw.n), "raw-bytes")
			b.ReportMetric(float64(compressed.Len()), "zstd-bytes")
			b.ReportMetric(float64(raw.n)/float64(compressed.Len()), "ratio")
		})
		b.Run(level.String()+"/decode", func(b *testing.B) {
			require.NotZero(b, compressed.Len(), "run the matching encode benchmark first")
			for b.Loop() {
				zr, err := remotesync.NewZstdReader(bytes.NewReader(compressed.Bytes()))
				require.NoError(b, err)
				n, err := io.Copy(io.Discard, zr)
				zr.Close()
				require.NoError(b, err)
				require.Equal(b, raw.n, n)
			}
		})
	}
}

type countingWriter struct{ n int64 }

func (w *countingWriter) Write(p []byte) (int, error) { w.n += int64(len(p)); return len(p), nil }
