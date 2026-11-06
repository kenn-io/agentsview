package kept

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/kit/atomicfile"
)

// Report holds finished artifacts and their final progress. Callers only read them.
type Report struct {
	Artifacts activity.CandidateArtifacts
	Done      activity.Progress
}

type activityReportDisk struct{ dir, build string }

// Reports keeps ended ranges in memory and requested days on disk.
// Each memo holds up to 128 MiB; disk-backed reports also have an eight-entry cap.
type Reports struct {
	once               sync.Once
	memory, diskMemory Memo[Report]
	disk               activityReportDisk
	subdir             string
	cancel             context.CancelFunc
	done               chan struct{}
}

func (s *Reports) memo(onDisk bool) *Memo[Report] {
	s.once.Do(func() {
		size := func(r Report) int64 { return activity.EstimatedArtifactBytes(r.Artifacts) }
		s.memory.Size, s.memory.MaxBytes = size, 128<<20
		s.diskMemory.Size, s.diskMemory.MaxBytes, s.diskMemory.Limit = size, 128<<20, 8
	})
	if onDisk && s.disk.dir != "" {
		return &s.diskMemory
	}
	return &s.memory
}

func (s *Reports) Get(selection, key string, onDisk bool) (Report, bool) {
	memo := s.memo(onDisk)
	if rows, ok := memo.Get(selection, key); ok {
		if onDisk {
			s.touchActivityReport(selection, key, rows[0])
		}
		return rows[0], true
	}
	if onDisk {
		if entry, ok := s.loadActivityReport(selection, key); ok {
			memo.Put(selection, key, []Report{entry})
			s.touchActivityReport(selection, key, entry)
			return entry, true
		}
	}
	return Report{}, false
}

func (s *Reports) Put(selection, key string, onDisk bool, entry Report) {
	s.memo(onDisk).Put(selection, key, []Report{entry})
	if onDisk {
		s.saveActivityReport(selection, key, entry)
	}
}

func (s *Reports) Clear() {
	s.memo(false).DeleteSlots(func(string) bool { return true })
	s.diskMemory.DeleteSlots(func(string) bool { return true })
}

// Path returns the selection's disk filename.
func (s *Reports) Path(selection string) string { return s.disk.path(selection) }

func (s *Reports) Sweep(now time.Time) (int, error) { return s.disk.sweep(now) }

// StartSweep owns the sweep until Close, independently of the startup context.
func (s *Reports) StartSweep(ctx context.Context) {
	if s.disk.dir == "" || s.cancel != nil {
		return
	}
	// The store owns the sweep; a startup-scoped cancellation must not stop it.
	ctx, s.cancel = context.WithCancel(context.WithoutCancel(ctx))
	s.done = make(chan struct{})
	go func() {
		defer close(s.done)
		ticker := time.NewTicker(activityReportSweepInterval)
		defer ticker.Stop()
		for {
			removed, err := s.Sweep(time.Now())
			if err != nil {
				log.Printf("%s: sweeping kept activity reports: %v", s.subdir, err)
			}
			if removed > 0 {
				log.Printf("%s: removed %d activity reports no one opened in %s", s.subdir, removed, activityReportUnopened)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (s *Reports) Close() {
	if s.cancel != nil {
		s.cancel()
		<-s.done
	}
}

// OpenDisk selects the cache directory and the running binary identity.
// Call it before serving requests or starting the sweep.
func (s *Reports) OpenDisk(subdir, id string) error {
	base, _, _ := strings.Cut(os.Getenv("CACHE_DIRECTORY"), ":")
	if base == "" {
		userCache, err := os.UserCacheDir()
		if err != nil {
			return fmt.Errorf("locating the activity report cache: %w", err)
		}
		base = filepath.Join(userCache, "agentsview")
	}
	dir := filepath.Join(base, subdir, id)
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
	s.disk = activityReportDisk{dir: dir, build: hex.EncodeToString(build[:])}
	s.subdir = subdir
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
func (s *Reports) loadActivityReport(selection, key string) (Report, bool) {
	d := s.disk
	if d.dir == "" {
		return Report{}, false
	}
	data, err := os.ReadFile(d.path(selection))
	if errors.Is(err, os.ErrNotExist) {
		return Report{}, false
	}
	if err != nil {
		log.Printf("%s: reading a kept activity report: %v", s.subdir, err)
		return Report{}, false
	}
	header := d.header(key)
	if !bytes.HasPrefix(data, header) {
		return Report{}, false
	}
	var file Report
	if err := json.Unmarshal(data[len(header):], &file); err != nil {
		log.Printf("%s: decoding a kept activity report: %v", s.subdir, err)
		return Report{}, false
	}
	return file, true
}

// saveActivityReport replaces the selection's file. A report that cannot
// be written is still served; the next build of the selection tries again.
func (s *Reports) saveActivityReport(selection, key string, entry Report) {
	d := s.disk
	if d.dir == "" {
		return
	}
	body, err := json.Marshal(entry)
	if err == nil {
		err = atomicfile.WriteFile(d.path(selection), append(d.header(key), body...), atomicfile.WithPrivate(), atomicfile.WithoutSync())
	}
	if err != nil {
		log.Printf("%s: keeping an activity report on disk: %v", s.subdir, err)
	}
}

// touchActivityReport records that a client opened the selection's kept
// report now. A file the sweep has already removed is written again.
func (s *Reports) touchActivityReport(selection, key string, entry Report) {
	d := s.disk
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
		log.Printf("%s: recording an activity report open: %v", s.subdir, err)
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
		case strings.HasPrefix(name, ".") && strings.Contains(name, ".json.tmp-"):
			// atomicfile stages reports as .<report>.json.tmp-*.
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

// DayPreset reports whether q is what a plain day request produces.
func DayPreset(q activity.Query, now time.Time) bool {
	if q.Loc == nil {
		return false
	}
	base, err := activity.ResolveQuery(activity.QueryInput{
		Preset: "day", Date: q.RangeStart.In(q.Loc).Format("2006-01-02"), Timezone: q.Timezone,
	}, now)
	return err == nil && base.RangeStart.Equal(q.RangeStart) && base.RangeEnd.Equal(q.RangeEnd) &&
		base.Bucket == q.Bucket && base.GapCapSeconds == q.GapCapSeconds
}

// Selection names an ended range's report by everything the
// request chose: the filter as the build applies it, and the range.
func Selection(f any, q activity.Query) string {
	return fmt.Sprintf("%#v|%s|%s|%s|%s|%#v|%v", f, q.Timezone,
		q.RangeStart.UTC().Format(time.RFC3339Nano), q.RangeEnd.UTC().Format(time.RFC3339Nano),
		q.EffectiveEnd.UTC().Format(time.RFC3339Nano), q.Bucket, q.GapCapSeconds)
}
