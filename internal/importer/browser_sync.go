package importer

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// BrowserSyncStore keeps browser freshness alongside the local archive.
type BrowserSyncStore interface {
	db.Store
	IsSessionTrashed(context.Context, string) bool
	IsSessionExcluded(context.Context, string) bool
	GetProviderStatHash(context.Context, parser.AgentType, string) (uint64, bool, error)
	UpsertProviderStatHash(context.Context, parser.AgentType, string, uint64) error
}

type browserHTTPError struct {
	service     string
	status      int
	rateLimited bool
}

func (e *browserHTTPError) Error() string {
	if e.service == "claude" && e.status == 403 {
		return "claude.ai access denied (HTTP 403)"
	}
	return fmt.Sprintf("%s returned HTTP %d", e.service, e.status)
}

type browserSync struct {
	store      BrowserSyncStore
	agent      parser.AgentType
	stats      *ImportStats
	cb         *ImportCallbacks
	machine    []string
	tooLarge   error
	stop       func(error) bool
	failedLast bool
}

type browserConversation struct {
	freshness func(*db.Session) uint64
	fetch     func(context.Context) ([]byte, error)
	parse     func([]byte) (parser.ParseResult, error) // an empty session ID skips
	write     func(context.Context, db.Store, parser.ParseResult) (importStatus, error)
	matches   func([]byte, parser.ParseResult) bool
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
	if before != nil && fresh && storedHash == chat.freshness(before) {
		s.failedLast = false
		s.stats.record(id, importSkipped, nil)
		return nil
	}
	detail, err := chat.fetch(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Authentication errors retain the existing sign-in flow.
		if s.stop(err) {
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
		if errors.Is(err, s.tooLarge) {
			s.failedLast = false
			return nil
		}
		if s.failedLast {
			return err
		}
		s.failedLast = true
		return nil
	}
	var detailErr error
	write := func() error {
		result, err := chat.parse(detail)
		if err != nil || result.Session.ID == "" {
			s.stats.record(id, importSkipped, err)
			detailErr = err
			return nil
		}
		result.Session.Machine = resolvedImportMachine(result.Session.Machine, s.machine)
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
				if status == importSkipped && before != nil && !ptrEqual(before.SessionName, stored.SessionName) {
					s.stats.Skipped--
					s.stats.Updated++
				}
				if chat.matches(detail, result) {
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
	if detailErr != nil && (s.stop(detailErr) || s.failedLast) {
		return detailErr
	}
	s.failedLast = detailErr != nil
	return nil
}
