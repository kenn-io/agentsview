// Command watchprobe diagnoses source observation using isolated synthetic files.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/pflag"
)

func main() {
	var o options
	pflag.IntVar(&o.Files, "files", 50000, "Synthetic physical file count")
	pflag.IntVar(&o.Passes, "passes", 3, "Repeated warm complete scans")
	pflag.IntVar(&o.MaxWatches, "max-watches", 64, "Native directory watch allocation ceiling; zero tests coverage only")
	pflag.IntVar(&o.Queue, "event-queue", 128, "Bounded native notice queue capacity")
	pflag.Int64Var(&o.CacheBytes, "cache-bytes", 48<<20, "SQLite main-file page cap; zero disables persistence")
	pflag.StringVar(&o.Pattern, "name-pattern", "repetitive", "Synthetic basenames: repetitive or entropy, equal length")
	pflag.StringVar(&o.Output, "output-dir", "", "NEW private artifact directory; defaults to system temporary storage")
	pflag.DurationVar(&o.Duration, "duration", 0, "Additional sustained observation window, e.g. 5m or 2h")
	pflag.DurationVar(&o.ScanInterval, "scan-interval", 30*time.Second, "Completion-based coverage cadence during sustained observation")
	pflag.BoolVar(&o.Profiles, "profiles", false, "Write local CPU and post-GC heap profiles")
	pflag.BoolVar(&o.Trace, "trace", false, "Write a local runtime trace of core scenarios; excludes sustained window")
	pflag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	start := time.Now()
	report, err := runProbe(ctx, o)
	cancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "Completed %d files in %.2fs. Artifacts: %s\n", o.Files, time.Since(start).Seconds(), report.Artifacts)
	fmt.Fprintf(os.Stderr, "Native observations: %d; peak sampled Go heap: %.1f MiB; post-GC heap: %.1f MiB\n", report.Native.Observed, float64(report.Peak.HeapAlloc)/(1<<20), float64(report.AfterGC.HeapAlloc)/(1<<20))
}
