package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/update"
)

// downloadProgressInterval limits how often the progress line is redrawn.
// Downloads report every 32 KB, far more often than a reader can follow.
const downloadProgressInterval = 200 * time.Millisecond

// maxEstimateSeconds caps the shown estimate at 99:59:59. A longer
// estimate early in a stalled download is noise, so it shows as unknown.
const maxEstimateSeconds = 99*3600 + 59*60 + 59

// downloadProgress draws the single status line `agentsview update` shows
// while downloading: size, percent, average speed, and the elapsed time
// against the estimated total time.
type downloadProgress struct {
	w        io.Writer
	now      func() time.Time
	start    time.Time
	lastDraw time.Time
	lastLen  int
}

func newDownloadProgress(w io.Writer, now func() time.Time) *downloadProgress {
	return &downloadProgress{w: w, now: now}
}

// update redraws the line in place. The first report starts the clock.
// Reports inside downloadProgressInterval of the last redraw are skipped,
// except the one that completes the download.
func (p *downloadProgress) update(downloaded, total int64) {
	now := p.now()
	if p.start.IsZero() {
		p.start = now
	}
	complete := total > 0 && downloaded >= total
	if !complete && !p.lastDraw.IsZero() &&
		now.Sub(p.lastDraw) < downloadProgressInterval {
		return
	}
	p.lastDraw = now

	line := formatDownloadProgress(downloaded, total, now.Sub(p.start))
	// Pad over the rest of a longer previous line; "\r" only moves the
	// cursor back and leaves old characters behind.
	pad := max(p.lastLen-len(line), 0)
	p.lastLen = len(line)
	fmt.Fprintf(p.w, "\r%s%s", line, strings.Repeat(" ", pad))
}

// formatDownloadProgress renders one progress line, for example
// "  40.0 MB / 138.4 MB (29%)  2.1 MB/s  0:19 elapsed / ~1:06 total".
// Speed and times appear once some bytes have arrived over measurable
// time; the total and estimate appear only when the size is known.
func formatDownloadProgress(downloaded, total int64, elapsed time.Duration) string {
	var b strings.Builder
	b.WriteString("  ")
	b.WriteString(update.FormatSize(downloaded))
	if total > 0 {
		fmt.Fprintf(&b, " / %s (%.0f%%)",
			update.FormatSize(total),
			float64(downloaded)/float64(total)*100,
		)
	}

	secs := elapsed.Seconds()
	if downloaded <= 0 || secs <= 0 {
		return b.String()
	}
	rate := float64(downloaded) / secs
	fmt.Fprintf(&b, "  %s/s  %s elapsed",
		update.FormatSize(int64(rate)), formatClock(secs),
	)
	if total > 0 {
		estimate := "--:--"
		if est := float64(total) / rate; est <= maxEstimateSeconds {
			estimate = "~" + formatClock(est)
		}
		fmt.Fprintf(&b, " / %s total", estimate)
	}
	return b.String()
}

// formatClock renders seconds as m:ss, or h:mm:ss from one hour.
func formatClock(seconds float64) string {
	s := max(int64(seconds+0.5), 0)
	h, m, sec := s/3600, s/60%60, s%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, sec)
	}
	return fmt.Sprintf("%d:%02d", m, sec)
}
