package clickhouse

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
)

// usageWarmer re-runs the usage reads clients asked for recently once the
// mirror's parts change, so the memo answers the next request for them.
// A dashboard's day view is a few reads that every push invalidates; a
// query the client is about to repeat is cheapest to run right after the
// push lands rather than while the client waits.
type usageWarmer struct {
	mu          sync.Mutex
	recent      map[string]usageWarmRequest
	fingerprint string
	// lastRead is when a client last made a read the warmer records.
	lastRead time.Time
	cancel   context.CancelFunc
	done     chan struct{}
	// days holds the filters and timezones clients asked day reports for.
	days map[string]activityDaySelection
	// swept is when the kept reports on disk were last swept. Only the
	// warmer goroutine uses it.
	swept time.Time
}

// activityDaySelection is a filter and timezone a client asked day
// reports for. It is kept on disk so a restart warms the same day view.
type activityDaySelection struct {
	Filter   db.AnalyticsFilter
	Timezone string
	Last     time.Time
}

type usageWarmRequest struct {
	kind   string
	filter db.UsageFilter
	limit  int
	last   time.Time
	// activity holds an activity report's selection.
	activity activityWarmRequest
}

type activityWarmRequest struct {
	filter db.AnalyticsFilter
	query  activity.Query
}

const (
	// usageWarmInterval is how often the warmer checks the mirror's parts;
	// a check that finds them unchanged is one small query.
	usageWarmInterval = 500 * time.Millisecond
	// usageWarmRecency bounds how long a read is refilled after its last
	// client request.
	usageWarmRecency = 30 * time.Minute
	usageWarmLimit   = 16
)

// evictOldestWarm removes the least recently used entries of m, by last,
// until at most usageWarmLimit remain.
func evictOldestWarm[V any](m map[string]V, last func(V) time.Time) {
	for len(m) > usageWarmLimit {
		oldest, oldestAt := "", time.Time{}
		for k, v := range m {
			if oldest == "" || last(v).Before(oldestAt) {
				oldest, oldestAt = k, last(v)
			}
		}
		delete(m, oldest)
	}
}

// recordUsageRead notes a client's read for the warmer. A read whose rows
// are not kept (see usageRowMemoKeyFor) gains nothing from warming.
func (s *Store) recordUsageRead(kind string, f db.UsageFilter, limit int) {
	if chTerminationUsesTime(f.Termination) {
		return
	}
	w := &s.usageWarmer
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.recent == nil {
		w.recent = map[string]usageWarmRequest{}
	}
	key := fmt.Sprintf("%s|%d|%#v", kind, limit, f)
	w.lastRead = time.Now()
	w.recent[key] = usageWarmRequest{kind: kind, filter: f, limit: limit, last: w.lastRead}
	evictOldestWarm(w.recent, func(r usageWarmRequest) time.Time { return r.last })
}

// recordActivityRead notes a client's activity report for the warmer.
func (s *Store) recordActivityRead(f db.AnalyticsFilter, q activity.Query) {
	w := &s.usageWarmer
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.recent == nil {
		w.recent = map[string]usageWarmRequest{}
	}
	// The effective end of a range in progress moves with every request;
	// the warmer re-resolves it, so it is not part of the key.
	key := fmt.Sprintf("activity|%#v|%s|%s|%s|%#v|%v", f, q.Timezone,
		q.RangeStart.UTC().Format(time.RFC3339Nano), q.RangeEnd.UTC().Format(time.RFC3339Nano),
		q.Bucket, q.GapCapSeconds)
	w.lastRead = time.Now()
	w.recent[key] = usageWarmRequest{kind: "activity", activity: activityWarmRequest{filter: f, query: q}, last: w.lastRead}
	evictOldestWarm(w.recent, func(r usageWarmRequest) time.Time { return r.last })
	if !activityDayPreset(q, time.Now()) {
		return
	}
	dayKey := fmt.Sprintf("%#v|%s", f, q.Timezone)
	_, known := w.days[dayKey]
	if w.days == nil {
		w.days = map[string]activityDaySelection{}
	}
	w.days[dayKey] = activityDaySelection{Filter: f, Timezone: q.Timezone, Last: time.Now()}
	evictOldestWarm(w.days, func(d activityDaySelection) time.Time { return d.Last })
	if !known {
		s.saveActivityDaySelections(slices.Collect(maps.Values(w.days)))
	}
}

// activityDaySelectionsFile names the file that keeps the day selections
// beside the reports.
const activityDaySelectionsFile = "day-selections.json"

// saveActivityDaySelections replaces the kept day selections. A list that
// cannot be written only costs the warm start after the next restart.
func (s *Store) saveActivityDaySelections(days []activityDaySelection) {
	if s.reportDisk.dir == "" {
		return
	}
	data, err := json.Marshal(days)
	if err == nil {
		err = writeFileAtomic(s.reportDisk.dir, filepath.Join(s.reportDisk.dir, activityDaySelectionsFile), data)
	}
	if err != nil {
		log.Printf("clickhouse: keeping activity day selections: %v", err)
	}
}

// localTimezoneName returns the IANA name of the process's timezone, from
// TZ or the /etc/localtime link, or "" when neither names one.
func localTimezoneName() string {
	name := strings.TrimPrefix(os.Getenv("TZ"), ":")
	if name == "" {
		target, err := os.Readlink("/etc/localtime")
		if err != nil {
			return ""
		}
		_, name, _ = strings.Cut(target, "zoneinfo/")
	}
	if _, err := time.LoadLocation(name); err != nil || name == "" || name == "Local" {
		return ""
	}
	return name
}

// loadActivityDaySelections records today's report of every kept day
// selection as a client read, so the warmer's first pass prepares the day
// views clients looked at before the restart.
func (s *Store) loadActivityDaySelections() error {
	if s.reportDisk.dir == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(s.reportDisk.dir, activityDaySelectionsFile))
	if errors.Is(err, os.ErrNotExist) {
		// No client has asked for a day yet. The default day view in the
		// server's own timezone is the likeliest first request.
		if timezone := localTimezoneName(); timezone != "" {
			data, err = json.Marshal([]activityDaySelection{{
				Filter: db.AnalyticsFilter{Timezone: timezone}, Timezone: timezone,
			}})
		}
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading activity day selections: %w", err)
	}
	var days []activityDaySelection
	if err := json.Unmarshal(data, &days); err != nil {
		return fmt.Errorf("decoding activity day selections: %w", err)
	}
	now := time.Now()
	for _, d := range days {
		loc, err := time.LoadLocation(d.Timezone)
		if err != nil {
			return fmt.Errorf("loading kept activity timezone %q: %w", d.Timezone, err)
		}
		q, err := activity.ResolveQuery(activity.QueryInput{
			Preset: "day", Date: now.In(loc).Format("2006-01-02"), Timezone: d.Timezone,
		}, now)
		if err != nil {
			return fmt.Errorf("resolving kept activity day selection: %w", err)
		}
		s.recordActivityRead(d.Filter, q)
	}
	return nil
}

// activityWarmQuery re-resolves a recorded query's effective end at now,
// as a request arriving now would.
func activityWarmQuery(q activity.Query, now time.Time) activity.Query {
	nowUTC := now.UTC()
	end := q.RangeEnd
	if nowUTC.Before(end) {
		end = nowUTC
	}
	if end.Before(q.RangeStart) {
		end = q.RangeStart
	}
	q.EffectiveEnd = end
	q.Partial = nowUTC.Before(q.RangeEnd)
	return q
}

func resolveActivityDay(date time.Time, timezone string, now time.Time) (activity.Query, error) {
	return activity.ResolveQuery(activity.QueryInput{
		Preset: "day", Date: date.Format("2006-01-02"), Timezone: timezone,
	}, now)
}

// activityDayPreset reports whether q is what a plain day request produces.
func activityDayPreset(q activity.Query, now time.Time) bool {
	if q.Loc == nil {
		return false
	}
	base, err := resolveActivityDay(q.RangeStart.In(q.Loc), q.Timezone, now)
	return err == nil && base.RangeStart.Equal(q.RangeStart) && base.RangeEnd.Equal(q.RangeEnd) &&
		base.Bucket == q.Bucket && base.GapCapSeconds == q.GapCapSeconds
}

// StartBackground reads the first prepared state and starts the warmer.
// Serve calls it once, after SetCustomPricing, so no background read
// prices usage without the operator's rates. The first prepared state
// loads the pricing catalog and reads usage coverage, a few hundred
// milliseconds; reading it before the server listens keeps that off the
// first request. A failure here is the one the first read would meet, so
// it is reported and left to that read.
func (s *Store) StartBackground(ctx context.Context) {
	if _, err := s.preparedUsageState(ctx); err != nil {
		log.Printf("clickhouse: preparing usage state at startup: %v", err)
	}
	s.startUsageWarmer()
}

// startUsageWarmer runs the warmer until Close.
func (s *Store) startUsageWarmer() {
	w := &s.usageWarmer
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel, w.done = cancel, make(chan struct{})
	go func() {
		defer close(w.done)
		ticker := time.NewTicker(usageWarmInterval)
		defer ticker.Stop()
		// The first pass runs at once: after a restart it sweeps the kept
		// reports and prepares the kept day selections before the first
		// request arrives.
		for {
			s.sweepActivityReports(time.Now())
			if err := s.warmUsageReads(ctx); err != nil && ctx.Err() == nil {
				log.Printf("clickhouse: warming usage reads: %v", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (s *Store) stopUsageWarmer() {
	w := &s.usageWarmer
	if w.cancel == nil {
		return
	}
	w.cancel()
	<-w.done
}

// sweepActivityReports removes the kept reports no client has opened
// lately, at most once per activityReportSweepInterval.
func (s *Store) sweepActivityReports(now time.Time) {
	w := &s.usageWarmer
	if s.reportDisk.dir == "" || now.Sub(w.swept) < activityReportSweepInterval {
		return
	}
	w.swept = now
	removed, err := s.reportDisk.sweep(now)
	if err != nil {
		log.Printf("clickhouse: sweeping kept activity reports: %v", err)
	}
	if removed > 0 {
		log.Printf("clickhouse: removed %d activity reports no one opened in %s",
			removed, activityReportUnopened)
	}
}

// warmUsageReads refills the memos for the recent reads when the mirror's
// parts changed since the last warm. Reads that would use the raw rows are
// not warmed; the prepared refresh that follows a push makes them cheap
// first. A pass after a merge alone finds its reads in the memos.
func (s *Store) warmUsageReads(ctx context.Context) error {
	w := &s.usageWarmer
	w.mu.Lock()
	idle := time.Since(w.lastRead) > usageWarmRecency
	w.mu.Unlock()
	if idle {
		// Every recorded read has aged out, so a change would refill
		// nothing; the warmer does not query the mirror until a client
		// reads again.
		return nil
	}
	fingerprint, err := s.partsFingerprint(ctx)
	if err != nil {
		return err
	}
	w.mu.Lock()
	unchanged := fingerprint == w.fingerprint
	requests := make([]usageWarmRequest, 0, len(w.recent))
	for key, r := range w.recent {
		if time.Since(r.last) > usageWarmRecency {
			delete(w.recent, key)
			continue
		}
		requests = append(requests, r)
	}
	w.mu.Unlock()
	if unchanged {
		return nil
	}
	// The prepared state's coverage is read once per change for every
	// read that follows, including a client's first read after a restart.
	state, err := s.preparedUsageState(ctx)
	if err != nil || !state.ready {
		return err
	}
	probed := false
	for _, r := range requests {
		switch r.kind {
		case "daily":
			_, err = s.dailyUsage(ctx, r.filter)
		case "top":
			_, err = s.topSessionsByCost(ctx, r.filter, r.limit)
		case "counts":
			_, err = s.usageSessionCounts(ctx, r.filter)
		case "activity":
			// The server probes the source before every report.
			if !probed {
				if _, err = s.ActivityReportSourceProbe(ctx); err != nil {
					return err
				}
				probed = true
			}
			_, err = s.buildActivityReportArtifacts(ctx, r.activity.filter,
				activityWarmQuery(r.activity.query, time.Now()), nil)
		}
		if err != nil {
			return err
		}
	}
	// The fingerprint is the one seen before the reads, so a change that
	// lands during them is warmed on the next pass; it is recorded only
	// once every read succeeded, so a failed read is retried.
	w.mu.Lock()
	w.fingerprint = fingerprint
	w.mu.Unlock()
	return nil
}
