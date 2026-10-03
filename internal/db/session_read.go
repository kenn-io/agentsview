package db

import (
	"context"
	"errors"
	"fmt"
)

// ErrSessionChanged means a sync rewrote the session while it was read, so the result could mix two versions.
var ErrSessionChanged = errors.New("session changed while it was read; try again")

// ErrSessionRevisionUnavailable means the backend records no transcript revision, so a read can't prove it saw one version.
var ErrSessionRevisionUnavailable = errors.New("this backend records no transcript revision")

// SessionSourceBinder is implemented by stores whose session IDs resolve to a physical source that can move between reads.
type SessionSourceBinder interface {
	SessionSourceBinding(ctx context.Context, sessionID string) (string, error)
	SessionSourceChanged(err error) bool
}

// ReadSessionChecked runs read between two looks at the session's transcript
// revision, end state and source binding, and retries once when a sync lands
// in between. It returns (nil, nil) when the session doesn't exist.
func ReadSessionChecked(ctx context.Context, store Store, id string, read func(*Session) error) (*Session, error) {
	sess, err := readSessionOnce(ctx, store, id, read)
	if errors.Is(err, ErrSessionChanged) {
		sess, err = readSessionOnce(ctx, store, id, read)
	}
	return sess, err
}

func readSessionOnce(ctx context.Context, store Store, id string, read func(*Session) error) (*Session, error) {
	binder, _ := store.(SessionSourceBinder)
	classify := func(err error) error {
		if binder != nil && binder.SessionSourceChanged(err) {
			return fmt.Errorf("%w: %w", ErrSessionChanged, err)
		}
		return err
	}
	binding := func() (string, error) {
		if binder == nil {
			return "", nil
		}
		return binder.SessionSourceBinding(ctx, id)
	}
	before, err := binding()
	if err != nil {
		return nil, classify(err)
	}
	sess, err := store.GetSession(ctx, id)
	if err != nil || sess == nil {
		return nil, classify(err)
	}
	if sessionRevision(sess) == "" {
		return nil, ErrSessionRevisionUnavailable
	}
	if err := read(sess); err != nil {
		return nil, classify(err)
	}
	after, err := store.GetSession(ctx, id)
	if err != nil {
		return nil, classify(err)
	}
	if after == nil || sessionRevision(after) != sessionRevision(sess) || sessionTermination(after) != sessionTermination(sess) {
		return nil, ErrSessionChanged
	}
	current, err := binding()
	if err != nil {
		return nil, classify(err)
	}
	if current != before {
		return nil, ErrSessionChanged
	}
	return sess, nil
}

func sessionRevision(s *Session) string {
	if s.TranscriptRevision == nil {
		return ""
	}
	return *s.TranscriptRevision
}

func sessionTermination(s *Session) string {
	if s.TerminationStatus == nil {
		return ""
	}
	return *s.TerminationStatus
}
