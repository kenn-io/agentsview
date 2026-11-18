package importer

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v7"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/httputil"
	"go.kenn.io/agentsview/internal/parser"
)

// BrowserResponseLimit caps one browser response, from Chrome or the desktop relay.
const BrowserResponseLimit = 32 << 20

var (
	// ErrBrowserAuthRequired reports a missing or expired browser session.
	ErrBrowserAuthRequired = errors.New("sign-in required")
	// ErrBrowserResponseTooLarge marks a response over BrowserResponseLimit.
	ErrBrowserResponseTooLarge = errors.New("response exceeds 32 MiB")
	// ErrBrowserFormat reports a conversation shape this version cannot read.
	ErrBrowserFormat = errors.New("returned a conversation format this version can't read. Update AgentsView and try again")
)

// BrowserResponse carries one browser response without credentials.
type BrowserResponse struct {
	Status     int
	Body       []byte
	RetryAfter string
}

// BrowserFetch requests path from the platform's site in the user's browser.
type BrowserFetch func(ctx context.Context, path string) (BrowserResponse, error)

// BrowserSyncStore keeps browser freshness alongside the local archive.
type BrowserSyncStore interface {
	db.Store
	IsSessionTrashed(context.Context, string) bool
	IsSessionExcluded(context.Context, string) bool
	GetProviderStatHash(context.Context, parser.AgentType, string) (uint64, bool, error)
	UpsertProviderStatHash(context.Context, parser.AgentType, string, uint64) error
}

// BrowserSyncOptions carries per-run settings for a browser sync.
type BrowserSyncOptions struct {
	// Machine overrides the machine recorded on synced sessions when set.
	Machine string
	// ClaudeAIOrganizations lists lowercase organization UUIDs to import;
	// empty imports every chat organization. Only Claude.ai reads it.
	ClaudeAIOrganizations []string
	// Now reads the current time; nil means time.Now. Only tests set it.
	Now func() time.Time
}

func (o BrowserSyncOptions) now() time.Time {
	if o.Now == nil {
		return time.Now()
	}
	return o.Now()
}

// BrowserPlatform describes one site that Sync imports through the browser.
type BrowserPlatform struct {
	Agent parser.AgentType
	// Label names the service in errors and messages, such as "Claude.ai".
	Label string
	// ClientRelay lets a client without Chrome answer fetches itself (the
	// AgentsView desktop client); other platforms sync only through Chrome.
	ClientRelay bool
	// StopOnRateLimit ends the run when HTTP 429 outlasts the request's
	// retries or Retry-After asks for more than a minute, instead of moving
	// on to the next conversation.
	StopOnRateLimit bool
	// AuthRequired reports whether a non-2xx response means the sign-in
	// expired. Nil means no response does.
	AuthRequired func(status int, body []byte) bool
	Sync         func(ctx context.Context, store BrowserSyncStore, fetch BrowserFetch, opts BrowserSyncOptions, cb *ImportCallbacks) (ImportStats, error)
}

// BrowserPlatforms lists the platforms Sync supports, keyed by agent type.
func BrowserPlatforms() []BrowserPlatform {
	return []BrowserPlatform{claudeAIPlatform(), chatGPTPlatform(), geminiAppsPlatform()}
}

// LookupBrowserPlatform finds a platform by its agent type name.
func LookupBrowserPlatform(name string) (BrowserPlatform, bool) {
	for _, platform := range BrowserPlatforms() {
		if string(platform.Agent) == name {
			return platform, true
		}
	}
	return BrowserPlatform{}, false
}

func (p BrowserPlatform) wrap(err error) error {
	return fmt.Errorf("%s %w", p.Label, err)
}

// browserRequestFiles holds each platform's request allowlist. The Chrome
// extension and the desktop client read the same files, so a path Sync
// builds is a path they accept.
//
//go:embed browser_requests/*.txt
var browserRequestFiles embed.FS

// browserRequests returns the allowlist for agent. Every registered platform
// has one; TestBrowserRequestFilesMatchPlatforms checks it.
func browserRequests(agent parser.AgentType) string {
	body, err := browserRequestFiles.ReadFile("browser_requests/" + string(agent) + ".txt")
	if err != nil {
		panic(err)
	}
	return string(body)
}

func browserRequest(shapes string, shape int, substitutions ...string) string {
	return strings.NewReplacer(substitutions...).Replace(strings.Split(strings.TrimSpace(shapes), "\n")[shape])
}

// browserFreshness hashes a list marker with the stored session fields Sync
// changes, so a stored hash goes stale when either side moves. A nil session
// hashes as an empty one.
func browserFreshness(marker string, session *db.Session) uint64 {
	if session == nil {
		session = &db.Session{}
	}
	hash := fnv.New64a()
	fmt.Fprintf(hash, "%s\x00%d", marker, session.MessageCount)
	for _, field := range []*string{session.TranscriptRevision, session.SessionName, session.EndedAt} {
		fmt.Fprint(hash, "\x00")
		if field != nil {
			fmt.Fprint(hash, *field)
		}
	}
	return hash.Sum64()
}

// pendingWindow bounds how long a chat whose newest reply looks unfinished
// stays stale. A reply that never completes is then cached like any other.
const pendingWindow = 24 * time.Hour

// recentlyUpdated reports whether a list update time is within pendingWindow
// of now. A zero or future time counts as recent.
func recentlyUpdated(updatedAt, now time.Time) bool {
	return updatedAt.IsZero() || now.Sub(updatedAt) < pendingWindow
}

// waitBrowserGap paces detail requests: it waits gap with up to 400 ms of
// jitter either way. A zero gap does not wait.
func waitBrowserGap(ctx context.Context, gap time.Duration) error {
	if gap <= 0 {
		return nil
	}
	jitter := time.Duration(rand.Int64N(int64(800*time.Millisecond))) - 400*time.Millisecond
	timer := time.NewTimer(gap + jitter)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type browserHTTPError struct {
	label       string
	status      int
	rateLimited bool
}

func (e *browserHTTPError) Error() string {
	if e.status == 403 {
		return e.label + " access denied (HTTP 403)"
	}
	return fmt.Sprintf("%s returned HTTP %d", e.label, e.status)
}

// fetchBrowser applies the response limit, sign-in detection, and retries
// shared by every platform.
func fetchBrowser(ctx context.Context, platform BrowserPlatform, fetch BrowserFetch, path string) ([]byte, error) {
	attempt := 0
	body, err := backoff.Retry(ctx, func() ([]byte, error) {
		response, err := fetch(ctx, path)
		if err != nil {
			return nil, backoff.Permanent(err)
		}
		if len(response.Body) > BrowserResponseLimit {
			return nil, backoff.Permanent(platform.wrap(ErrBrowserResponseTooLarge))
		}
		status := response.Status
		if status >= 200 && status < 300 {
			return response.Body, nil
		}
		if platform.AuthRequired != nil && platform.AuthRequired(status, response.Body) {
			return nil, backoff.Permanent(platform.wrap(ErrBrowserAuthRequired))
		}
		if status == 413 {
			return nil, backoff.Permanent(platform.wrap(ErrBrowserResponseTooLarge))
		}
		httpErr := &browserHTTPError{label: platform.Label, status: status, rateLimited: platform.StopOnRateLimit && status == 429}
		if status != 429 && status < 500 {
			return nil, backoff.Permanent(httpErr)
		}
		delay := time.Duration(1<<attempt) * time.Second
		attempt++
		if retryAfter := httputil.ParseRetryAfter(response.RetryAfter); retryAfter > 0 {
			if platform.StopOnRateLimit && retryAfter > 60*time.Second {
				httpErr.rateLimited = true
				return nil, backoff.Permanent(httpErr)
			}
			delay = min(retryAfter, 60*time.Second)
		}
		return nil, backoff.RetryAfter(delay, httpErr)
	}, backoff.WithMaxTries(5), backoff.WithMaxElapsedTime(0))
	if retryErr := backoff.AsRetryError(err); retryErr != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		err = retryErr.LastErr
	}
	if httpErr, ok := errors.AsType[*browserHTTPError](err); ok && httpErr.rateLimited {
		err = fmt.Errorf("%s rate limited; sync stopped early, run Sync again later: %w", platform.Label, err)
	}
	return body, err
}

// browserSync runs the per-conversation steps every platform shares: skip
// trashed, excluded, and fresh chats; fetch, parse, and write the rest; record
// freshness; and stop after two failed conversations in a row.
type browserSync struct {
	store      BrowserSyncStore
	agent      parser.AgentType
	stats      *ImportStats
	cb         *ImportCallbacks
	machine    string
	failedLast bool
	// refused marks a conversation the service declined to return. It counts
	// as that conversation's failure and does not end the run.
	refused error
	// formatErr keeps the last unreadable conversation format. It counts
	// toward the two-failures stop like any failed chat; adapters return it
	// when the list ends, so the user learns that an update is needed.
	formatErr error
}

func newBrowserSync(store BrowserSyncStore, platform BrowserPlatform, stats *ImportStats, opts BrowserSyncOptions, cb *ImportCallbacks) *browserSync {
	return &browserSync{store: store, agent: platform.Agent, stats: stats, cb: cb, machine: opts.Machine}
}

type browserConversation struct {
	freshness func(*db.Session) uint64
	fetch     func(context.Context) ([]byte, error)
	parse     func([]byte) (parser.ParseResult, error) // an empty session ID means nothing to import
	write     func(context.Context, db.Store, parser.ParseResult) (importStatus, error)
	// matches, when set, reports whether the detail still describes the
	// listed version; freshness is recorded only when it does.
	matches func([]byte, parser.ParseResult) bool
	// pending, when set, reports a reply still being written. The result is
	// written but freshness is not recorded, so the next Sync fetches it again.
	pending func([]byte) bool
}

// stops reports errors that end the run instead of one conversation.
func stops(err error) bool {
	httpErr, _ := errors.AsType[*browserHTTPError](err)
	return errors.Is(err, ErrBrowserAuthRequired) || httpErr != nil && httpErr.rateLimited
}

func (s *browserSync) conversation(ctx context.Context, id string, chat browserConversation) (retErr error) {
	defer func() {
		s.cb.progress(*s.stats)
		if retErr == nil && ctx.Err() != nil {
			retErr = ctx.Err()
		}
	}()
	if s.store.IsSessionTrashed(ctx, id) || s.store.IsSessionExcluded(ctx, id) {
		s.failedLast = false
		s.stats.record(id, importSkipped, nil)
		return nil
	}
	before, err := s.store.GetSessionFull(ctx, id)
	if err != nil {
		return err
	}
	storedHash, fresh, err := s.store.GetProviderStatHash(ctx, s.agent, id)
	if err != nil {
		return err
	}
	if fresh && storedHash == chat.freshness(before) {
		s.failedLast = false
		s.stats.record(id, importSkipped, nil)
		return nil
	}
	detail, err := chat.fetch(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if stops(err) {
			if httpErr, ok := errors.AsType[*browserHTTPError](err); ok && httpErr.rateLimited {
				s.stats.record(id, importSkipped, err)
			}
			return err
		}
		if httpErr, ok := errors.AsType[*browserHTTPError](err); ok && httpErr.status == 404 {
			s.failedLast = false
			s.stats.record(id, importSkipped, nil)
			return nil
		}
		s.stats.record(id, importSkipped, err)
		if errors.Is(err, ErrBrowserFormat) {
			s.formatErr = err
		}
		if errors.Is(err, ErrBrowserResponseTooLarge) || s.refused != nil && errors.Is(err, s.refused) {
			s.failedLast = false
			return nil
		}
		if s.failedLast {
			return err
		}
		s.failedLast = true
		return nil
	}
	pending := chat.pending != nil && chat.pending(detail)
	var detailErr error
	write := func() error {
		result, err := chat.parse(detail)
		if errors.Is(err, ErrBrowserFormat) {
			s.formatErr = err
		}
		if err != nil {
			s.stats.record(id, importSkipped, err)
			detailErr = err
			return nil
		}
		if result.Session.ID == "" {
			// The chat has nothing to import yet. Unless a reply is still being
			// written, record freshness for what is stored so it is not fetched
			// again until the list shows a change.
			s.stats.record(id, importSkipped, nil)
			if pending {
				return nil
			}
			stored, err := s.store.GetSessionFull(ctx, id)
			if err == nil {
				err = s.store.UpsertProviderStatHash(ctx, s.agent, id, chat.freshness(stored))
			}
			detailErr = err
			return nil
		}
		result.Session.Machine = resolvedImportMachine(result.Session.Machine, []string{s.machine})
		// Read the title baseline inside the serialized write, so a title an
		// archive import changed during the fetch still counts as an update.
		before, err := s.store.GetSessionFull(ctx, id)
		if err != nil {
			s.stats.record(id, importSkipped, err)
			detailErr = err
			return nil
		}
		status, err := chat.write(ctx, s.store, result)
		if errors.Is(err, db.ErrSessionTrashed) {
			s.stats.record(id, importSkipped, nil)
			return nil
		}
		// The write has committed. Keep its count even if bookkeeping fails.
		s.stats.record(id, status, err)
		if err == nil {
			stored, readErr := s.store.GetSessionFull(ctx, id)
			err = readErr
			if err == nil && stored != nil {
				// Equal messages can still hide a title repair. Compare stored
				// names so titles discarded by archive policy count as unchanged.
				if status == importSkipped && before != nil && !ptrEqual(before.SessionName, stored.SessionName) {
					s.stats.Skipped--
					s.stats.Updated++
				}
				// The chat can change between list and detail; cache only what was imported.
				if (chat.matches == nil || chat.matches(detail, result)) && !pending {
					err = s.store.UpsertProviderStatHash(ctx, s.agent, id, chat.freshness(stored))
				}
			}
			if err != nil {
				s.stats.record(id, importSkipped, err)
			}
		}
		detailErr = err
		return nil
	}
	if s.cb != nil && s.cb.SerializeWrite != nil {
		err = s.cb.SerializeWrite(write)
	} else {
		err = write()
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	if detailErr != nil && (stops(detailErr) || s.failedLast) {
		return detailErr
	}
	s.failedLast = detailErr != nil
	return nil
}
