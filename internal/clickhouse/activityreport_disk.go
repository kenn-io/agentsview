package clickhouse

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/activity"
)

// activityReportDisk keeps the reports of ended days that clients open on
// disk, one file per selection, so reopening a day after a restart starts
// from a finished report. A file names the binary that wrote it and the
// report's key; a file from another binary or under another key is rebuilt
// and replaced. Nothing writes a report no client asked for, and a file's
// modification time records its last open, so a sweep removes the reports
// no one has opened for activityReportUnopened.
type activityReportDisk struct {
	dir, build string
}

type activityReportFile struct {
	Artifacts activity.CandidateArtifacts
	Done      activity.Progress
}

// openActivityReportDisk places the reports in the service's cache
// directory, which systemd names in CACHE_DIRECTORY, or else in the user's
// cache directory, under a directory per mirror.
func (s *Store) openActivityReportDisk(t Target) error {
	base, _, _ := strings.Cut(os.Getenv("CACHE_DIRECTORY"), ":")
	if base == "" {
		userCache, err := os.UserCacheDir()
		if err != nil {
			return fmt.Errorf("locating the activity report cache: %w", err)
		}
		base = filepath.Join(userCache, "agentsview")
	}
	mirror, err := url.Parse(t.URL)
	if err != nil {
		return fmt.Errorf("parsing the clickhouse URL for the activity report cache: %w", err)
	}
	database, err := t.DatabaseName()
	if err != nil {
		return err
	}
	// The mirror is named by its host and database only; the URL's
	// credentials stay out of the path.
	mirrorID := sha256.Sum256([]byte(mirror.Host + "/" + database))
	dir := filepath.Join(base, "clickhouse-activity-reports", hex.EncodeToString(mirrorID[:8]))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating the activity report cache: %w", err)
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("naming the binary for the activity report cache: %w", err)
	}
	info, err := os.Stat(exe)
	if err != nil {
		return fmt.Errorf("naming the binary for the activity report cache: %w", err)
	}
	build := sha256.Sum256(fmt.Appendf(nil, "%s|%d|%d", exe, info.Size(), info.ModTime().UnixNano()))
	s.reportDisk = activityReportDisk{dir: dir, build: hex.EncodeToString(build[:])}
	return nil
}

func (d activityReportDisk) path(selection string) string {
	name := sha256.Sum256([]byte(selection))
	return filepath.Join(d.dir, hex.EncodeToString(name[:16])+".json")
}

func (d activityReportDisk) header(key string) []byte {
	return []byte(d.build + " " + key + "\n")
}

// loadActivityReport returns the selection's report kept on disk under
// key by this binary.
func (s *Store) loadActivityReport(selection, key string) (activityReportEntry, bool) {
	d := s.reportDisk
	if d.dir == "" {
		return activityReportEntry{}, false
	}
	data, err := os.ReadFile(d.path(selection))
	if errors.Is(err, os.ErrNotExist) {
		return activityReportEntry{}, false
	}
	if err != nil {
		log.Printf("clickhouse: reading a kept activity report: %v", err)
		return activityReportEntry{}, false
	}
	header := d.header(key)
	if !bytes.HasPrefix(data, header) {
		return activityReportEntry{}, false
	}
	var file activityReportFile
	if err := json.Unmarshal(data[len(header):], &file); err != nil {
		log.Printf("clickhouse: decoding a kept activity report: %v", err)
		return activityReportEntry{}, false
	}
	return activityReportEntry{artifacts: file.Artifacts, done: file.Done}, true
}

// saveActivityReport replaces the selection's file. A report that cannot
// be written is still served; the next build of the selection tries again.
func (s *Store) saveActivityReport(selection, key string, entry activityReportEntry) {
	d := s.reportDisk
	if d.dir == "" {
		return
	}
	if err := d.write(selection, key, entry); err != nil {
		log.Printf("clickhouse: keeping an activity report on disk: %v", err)
	}
}

func (d activityReportDisk) write(selection, key string, entry activityReportEntry) error {
	body, err := json.Marshal(activityReportFile{Artifacts: entry.artifacts, Done: entry.done})
	if err != nil {
		return err
	}
	return writeFileAtomic(d.dir, d.path(selection), append(d.header(key), body...))
}

// touchActivityReport records that a client opened the selection's kept
// report now. A file the sweep has already removed is written again.
func (s *Store) touchActivityReport(selection, key string, entry activityReportEntry) {
	d := s.reportDisk
	if d.dir == "" {
		return
	}
	now := time.Now()
	err := os.Chtimes(d.path(selection), now, now)
	if errors.Is(err, os.ErrNotExist) {
		s.saveActivityReport(selection, key, entry)
		return
	}
	if err != nil {
		log.Printf("clickhouse: recording an activity report open: %v", err)
	}
}

const (
	// activityReportUnopened is how long a kept report stays on disk after
	// its last open.
	activityReportUnopened = 30 * 24 * time.Hour
	// activityReportSweepInterval is how often serve sweeps the kept
	// reports.
	activityReportSweepInterval = 24 * time.Hour
	// activityReportTempAge is how old a temporary file must be before the
	// sweep removes it; a write still in progress is younger.
	activityReportTempAge = time.Hour
)

// sweep removes the kept reports whose last open is more than
// activityReportUnopened before now, and the temporary files interrupted
// writes left behind. It returns how many files it removed.
func (d activityReportDisk) sweep(now time.Time) (int, error) {
	entries, err := os.ReadDir(d.dir)
	if err != nil {
		return 0, fmt.Errorf("listing kept activity reports: %w", err)
	}
	removed := 0
	for _, entry := range entries {
		name := entry.Name()
		var maxAge time.Duration
		switch {
		case entry.IsDir():
			continue
		case strings.HasPrefix(name, activityReportTempPrefix):
			maxAge = activityReportTempAge
		case strings.HasSuffix(name, ".json"):
			maxAge = activityReportUnopened
		default:
			continue
		}
		info, err := entry.Info()
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return removed, fmt.Errorf("reading a kept activity report: %w", err)
		}
		if now.Sub(info.ModTime()) <= maxAge {
			continue
		}
		if err := os.Remove(filepath.Join(d.dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return removed, fmt.Errorf("removing an unopened activity report: %w", err)
		}
		removed++
	}
	return removed, nil
}

// activityReportTempPrefix starts the names of writeFileAtomic's temporary
// files.
const activityReportTempPrefix = ".write-"

// writeFileAtomic writes data to a private temporary file in dir and
// renames it over path, so a reader sees the old file or the new one.
func writeFileAtomic(dir, path string, data []byte) error {
	tmp, err := os.CreateTemp(dir, activityReportTempPrefix+"*")
	if err != nil {
		return err
	}
	_, writeErr := tmp.Write(data)
	closeErr := tmp.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return errors.Join(err, os.Remove(tmp.Name()))
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return errors.Join(err, os.Remove(tmp.Name()))
	}
	return nil
}

// storeBackground runs the kept reports' sweep until Close.
type storeBackground struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// StartBackground reads the first prepared state and starts the sweep of
// the kept reports. Serve calls it once, after SetCustomPricing, so the
// prepared state prices usage with the operator's rates. The first
// prepared state loads the pricing catalog and reads usage coverage, a few
// hundred milliseconds; reading it before the server listens keeps that
// off the first request. A failure here is the one the first read would
// meet, so it is reported and left to that read.
func (s *Store) StartBackground(ctx context.Context) {
	if _, err := s.preparedUsageState(ctx); err != nil {
		log.Printf("clickhouse: preparing usage state at startup: %v", err)
	}
	if s.reportDisk.dir == "" {
		return
	}
	sweepCtx, cancel := context.WithCancel(context.Background())
	s.background = storeBackground{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(s.background.done)
		ticker := time.NewTicker(activityReportSweepInterval)
		defer ticker.Stop()
		for {
			s.sweepActivityReports()
			select {
			case <-sweepCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (s *Store) stopBackground() {
	if s.background.cancel == nil {
		return
	}
	s.background.cancel()
	<-s.background.done
}

// sweepActivityReports removes the kept reports no client has opened
// lately.
func (s *Store) sweepActivityReports() {
	removed, err := s.reportDisk.sweep(time.Now())
	if err != nil {
		log.Printf("clickhouse: sweeping kept activity reports: %v", err)
	}
	if removed > 0 {
		log.Printf("clickhouse: removed %d activity reports no one opened in %s",
			removed, activityReportUnopened)
	}
}

// activityDayPreset reports whether q is what a plain day request produces.
func activityDayPreset(q activity.Query, now time.Time) bool {
	if q.Loc == nil {
		return false
	}
	base, err := activity.ResolveQuery(activity.QueryInput{
		Preset: "day", Date: q.RangeStart.In(q.Loc).Format("2006-01-02"), Timezone: q.Timezone,
	}, now)
	return err == nil && base.RangeStart.Equal(q.RangeStart) && base.RangeEnd.Equal(q.RangeEnd) &&
		base.Bucket == q.Bucket && base.GapCapSeconds == q.GapCapSeconds
}
