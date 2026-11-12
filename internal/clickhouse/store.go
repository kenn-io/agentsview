package clickhouse

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	chdriver "github.com/ClickHouse/clickhouse-go/v2"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/kept"
	"go.kenn.io/agentsview/internal/readbase"
	"golang.org/x/sync/singleflight"
)

// Compile-time check: *Store satisfies db.Store.
var _ db.Store = (*Store)(nil)

func (s *Store) MemoryBackendName() string { return "clickhouse" }

// errNotImplemented marks db.Store methods the ClickHouse reader does not
// serve yet. The HTTP layer surfaces it as a server error; later tasks
// replace each stub with a real query.
var errNotImplemented = errors.New("not implemented for the ClickHouse backend")

// Store serves the read-only HTTP API from a ClickHouse mirror. Every
// connection carries final=1 so reads see one row per ReplacingMergeTree key
// without FINAL in the query text.
type Store struct {
	readbase.MirrorStubs
	conn *sql.DB
	// pages serves the session list, sidebar filters, and one-session reads
	// apart from conn's dashboard and Usage scans; see pageMaxOpenConns.
	pages *sql.DB
	// bulk reads the Activity usage rows; see openBulk.
	bulk chdriver.Conn
	// usageSliceRows is the fewest stored rows worth a slice of their own in
	// an Activity range read; see readActivityUsageSlices.
	usageSliceRows int
	// afterUsageSlices, when set, runs once a sliced range read has read
	// every slice. Tests use it to change the rows mid-read.
	afterUsageSlices func()
	// beforeUsageExtent lets tests change source rows after the request snapshot.
	beforeUsageExtent func()
	// afterTrackerReads, when set, runs after the row trackers read and
	// before the parts are checked again. Tests use it to move parts.
	afterTrackerReads func()
	cursorMu          sync.RWMutex
	cursorSecret      []byte
	customPricing     map[string]config.CustomModelRate

	// vectorMu guards the searcher and reason installed by the serve gate.
	vectorMu                  sync.RWMutex
	vectorSearcher            db.VectorSearcher
	semanticUnavailableReason string
	closeOnce                 sync.Once
	closeErr                  error
	probeCache                activityProbeCache
	// frustrationMarkers memoizes signals marker counts per session version.
	frustrationMarkers frustrationMarkerMemo
	// pricing memoizes the pricing catalog per set of active pricing parts.
	pricing pricingCache
	// coverageCache memoizes complete usage snapshot coverage per rows.
	coverageCache usageCoverageCache
	// usageVersions keeps one parts digest and row identity per small usage
	// table, and the trackers of the sessions, snapshot, and price record
	// rows; see finalRows.
	usageVersions struct {
		sync.Mutex
		tables              map[string]usageTableVersion
		sessions, snapshots *finalRows
		prices              map[string]*finalRows
		priceOrder          []string
	}
	// deltaCache keeps the prepared rows of changed snapshots.
	deltaCache usageDeltaCache
	// The usage row memos keep recent range reads per rows and filter.
	dailyUsageRows kept.Memo[chDailyUsageGroupRow]
	usageDisk      kept.Disk
	usageExtent    struct {
		sync.Mutex
		version string
		latest  time.Time
		unknown bool
		valid   bool
	}
	sessionAggregateRows kept.Memo[CHUsageAggregateRow]
	usageSessionRows     kept.Memo[chUsageSessionRow]
	// topSessionTotals keeps each session's totals per top-sessions read.
	topSessionTotals kept.Memo[db.TopSessionEntry]
	// analyticsSessionRows keeps recent analytics session listings.
	analyticsSessionRows kept.Memo[readbase.AnalyticsSession]
	// analyticsListings shares one listing read among concurrent requests.
	analyticsListings singleflight.Group
	// activitySessions keeps activity pairing inputs per session version.
	activitySessions activitySessionMemo
	// activityInputQueries counts pairing input reads that reached ClickHouse.
	activityInputQueries atomic.Int64
	// activityUsageRows keeps activity usage reads per source, set, and range.
	activityUsageRows kept.Memo[*activityUsageKept]
	// activityUsageRanges keeps every session's prepared usage rows and
	// their survivors per range in progress; see activityUsageRange.
	activityUsageRanges kept.Memo[*activityUsageRange]
	// readinessLog holds the last prepared usage readiness logged.
	readinessLog struct {
		sync.Mutex
		last string
	}
	// activitySessionListings keeps candidate listings per parts and predicate.
	activitySessionListings kept.Memo[activitySessionListing]
	projectIdentityMaps     kept.Memo[map[string]export.ProjectMapEntry]
	reports                 kept.Reports
	// activityChecks records the parts each report was last checked against.
	activityChecks       kept.Memo[string]
	activityUsageQueries atomic.Int64
	// activityUsageRowsRead counts the activity usage rows scanned.
	activityUsageRowsRead  atomic.Int64
	activitySessionQueries atomic.Int64
}

// NewStore connects to the mirror named by t and refuses schemas or data
// versions this binary cannot serve, or accounts that cannot read Activity
// report metadata.
func NewStore(ctx context.Context, t Target) (*Store, error) {
	conn, err := Open(ctx, t)
	if err != nil {
		return nil, err
	}
	if err := CheckSchemaCompat(ctx, conn); err != nil {
		conn.Close()
		return nil, err
	}
	if err := CheckDataVersionCompat(ctx, conn); err != nil {
		conn.Close()
		return nil, err
	}
	var parts uint64
	if err := conn.QueryRowContext(ctx,
		`SELECT count() FROM system.parts WHERE database = currentDatabase() AND active`,
	).Scan(&parts); err != nil {
		conn.Close()
		if IsPermissionError(err) {
			return nil, fmt.Errorf("clickhouse Activity reports require SELECT ON system.parts; ask an administrator to run GRANT SELECT ON system.parts TO <serve_user>, or add <query>GRANT SELECT ON system.parts</query> to the XML user's grants and reload users: %w", err)
		}
		return nil, fmt.Errorf("checking clickhouse Activity metadata access: %w", err)
	}
	pages, err := openPages(ctx, t)
	if err != nil {
		conn.Close()
		return nil, err
	}
	bulk, err := openBulk(ctx, t)
	if err != nil {
		return nil, errors.Join(err, conn.Close(), pages.Close())
	}
	s := NewStoreFromDB(conn, bulk)
	s.pages = pages
	return s, nil
}

// NewStoreFromDB wraps already open connections: conn serves page reads too,
// and bulk serves the reads that return millions of rows; see openBulk. The
// caller owns the connections' compatibility checks.
func NewStoreFromDB(conn *sql.DB, bulk chdriver.Conn) *Store {
	s := &Store{conn: conn, pages: conn, bulk: bulk, usageSliceRows: activityUsageSliceRows}
	s.usageVersions.tables = map[string]usageTableVersion{}
	s.usageVersions.prices = map[string]*finalRows{}
	s.dailyUsageRows.Size, s.dailyUsageRows.MaxBytes = dailyUsageRowBytes, dailyUsageRowsBytes
	s.sessionAggregateRows.Size = aggregateRowBytes
	s.usageSessionRows.Size = usageSessionRowBytes
	s.topSessionTotals.Size = db.TopSessionBytes
	s.analyticsSessionRows.Size = analyticsSessionBytes
	s.activityUsageRows.Size, s.activityUsageRows.MaxBytes = keptUsageBytes, activityUsageRowsBytes
	s.activityUsageRanges.Size, s.activityUsageRanges.MaxBytes = usageRangeBytes, activityUsageRangeBytes
	s.activityUsageRanges.Limit = activityUsageRangeLimit
	s.activitySessionListings.Size = sessionListingBytes
	return s
}

// DB exposes the underlying connection for tests and status commands.
func (s *Store) DB() *sql.DB { return s.conn }

func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.stopBackground()
		s.closeErr = errors.Join(s.conn.Close(), s.bulk.Close())
		if s.pages != s.conn {
			s.closeErr = errors.Join(s.closeErr, s.pages.Close())
		}
	})
	return s.closeErr
}

func (s *Store) queryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.conn.QueryContext(ctx, query, args...)
}

func (s *Store) queryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return s.conn.QueryRowContext(ctx, query, args...)
}

// pageQueryContext runs a page read on the pages pool.
func (s *Store) pageQueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.pages.QueryContext(ctx, query, args...)
}

func (s *Store) pageQueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return s.pages.QueryRowContext(ctx, query, args...)
}

func (s *Store) ReadOnly() bool { return true }

func (s *Store) HasFTS(_ context.Context) bool { return true }

// SetCustomPricing installs the operator's model rates. Call it before
// StartBackground: the background work caches the pricing catalog.
func (s *Store) SetCustomPricing(p map[string]config.CustomModelRate) {
	s.customPricing = p
}

func (s *Store) SetCursorSecret(secret []byte) {
	s.cursorMu.Lock()
	defer s.cursorMu.Unlock()
	s.cursorSecret = append([]byte(nil), secret...)
}

func (s *Store) EncodeCursor(c db.SessionCursor) string {
	data, _ := json.Marshal(c)
	s.cursorMu.RLock()
	secret := append([]byte(nil), s.cursorSecret...)
	s.cursorMu.RUnlock()
	mac := hmac.New(sha256.New, secret)
	mac.Write(data)
	sig := mac.Sum(nil)
	return base64.RawURLEncoding.EncodeToString(data) + "." +
		base64.RawURLEncoding.EncodeToString(sig)
}

func (s *Store) DecodeCursor(raw string) (db.SessionCursor, error) {
	parts := strings.Split(raw, ".")
	if len(parts) == 1 {
		data, err := base64.RawURLEncoding.DecodeString(parts[0])
		if err != nil {
			return db.SessionCursor{}, fmt.Errorf("%w: %w", db.ErrInvalidCursor, err)
		}
		var c db.SessionCursor
		if err := json.Unmarshal(data, &c); err != nil {
			return db.SessionCursor{}, fmt.Errorf("%w: %w", db.ErrInvalidCursor, err)
		}
		c.Total = 0
		return c, nil
	}
	if len(parts) != 2 {
		return db.SessionCursor{}, fmt.Errorf("%w: invalid format", db.ErrInvalidCursor)
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return db.SessionCursor{}, fmt.Errorf("%w: invalid payload: %w", db.ErrInvalidCursor, err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return db.SessionCursor{}, fmt.Errorf("%w: invalid signature: %w", db.ErrInvalidCursor, err)
	}
	s.cursorMu.RLock()
	secret := append([]byte(nil), s.cursorSecret...)
	s.cursorMu.RUnlock()
	mac := hmac.New(sha256.New, secret)
	mac.Write(data)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return db.SessionCursor{}, fmt.Errorf("%w: signature mismatch", db.ErrInvalidCursor)
	}
	var c db.SessionCursor
	if err := json.Unmarshal(data, &c); err != nil {
		return db.SessionCursor{}, fmt.Errorf("%w: invalid json: %w", db.ErrInvalidCursor, err)
	}
	return c, nil
}

// formatDBTime renders a scanned ClickHouse value as the RFC 3339 text the
// API returns. DateTime64 columns scan as time.Time; NULL scans as nil.
func formatDBTime(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case time.Time:
		return t.UTC().Format(time.RFC3339Nano)
	case *time.Time:
		if t == nil {
			return ""
		}
		return t.UTC().Format(time.RFC3339Nano)
	case string:
		return t
	case []byte:
		return string(t)
	default:
		return fmt.Sprint(t)
	}
}
