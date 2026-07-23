package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/artifact"
)

type artifactStoreCursorRegistry struct {
	mu     sync.Mutex
	tokens map[string]*artifactStoreCursorLease
	active int
	closed bool
}

type artifactStoreCursorLease struct {
	registry       *artifactStoreCursorRegistry
	store          artifact.ArtifactStore
	originIterator artifact.OriginIterator
	entryIterator  artifact.EntryIterator
	scope          string
	kindIndex      int
	token          string
	timer          *time.Timer
	released       bool
	registered     bool
}

func newArtifactStoreCursorRegistry() *artifactStoreCursorRegistry {
	return &artifactStoreCursorRegistry{tokens: make(map[string]*artifactStoreCursorLease)}
}

func (s *Server) currentArtifactStoreCursorRegistry() *artifactStoreCursorRegistry {
	s.artifactStoreCursorsMu.Lock()
	defer s.artifactStoreCursorsMu.Unlock()
	return s.artifactStoreCursors
}

func (s *Server) replaceArtifactStoreCursorRegistry() {
	s.artifactStoreCursorsMu.Lock()
	previous := s.artifactStoreCursors
	replacement := newArtifactStoreCursorRegistry()
	s.artifactStoreCursors = replacement
	closed := s.artifactStoreCursorsClosed
	s.artifactStoreCursorsMu.Unlock()
	if previous != nil {
		previous.close()
	}
	if closed {
		replacement.close()
	}
}

func (s *Server) closeArtifactStoreCursorRegistry() {
	s.artifactStoreCursorsMu.Lock()
	registry := s.artifactStoreCursors
	s.artifactStoreCursorsClosed = true
	s.artifactStoreCursorsMu.Unlock()
	if registry != nil {
		registry.close()
	}
}

func (r *artifactStoreCursorRegistry) claim(
	token, scope string,
) (*artifactStoreCursorLease, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, fs.ErrClosed
	}
	lease, ok := r.tokens[token]
	if !ok || lease.scope != scope || lease.released {
		return nil, fmt.Errorf("%w: invalid or expired artifact cursor", artifact.ErrArtifactInvalid)
	}
	delete(r.tokens, token)
	lease.token = ""
	if lease.timer != nil {
		lease.timer.Stop()
		lease.timer = nil
	}
	return lease, nil
}

func (r *artifactStoreCursorRegistry) retain(
	lease *artifactStoreCursorLease,
) (string, error) {
	token, err := artifactCursorToken()
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || lease == nil || lease.released || lease.token != "" {
		return "", fs.ErrClosed
	}
	if !lease.registered && r.active >= maxArtifactCursors {
		return "", fmt.Errorf("%w: too many active artifact cursors", artifact.ErrArtifactConflict)
	}
	if !lease.registered {
		lease.registered = true
		r.active++
	}
	lease.registry = r
	lease.token = token
	r.tokens[token] = lease
	lease.timer = time.AfterFunc(artifactCursorTTL, func() { r.release(token) })
	return token, nil
}

func (r *artifactStoreCursorRegistry) release(token string) bool {
	r.mu.Lock()
	lease, ok := r.tokens[token]
	if !ok || lease.released || lease.token != token {
		r.mu.Unlock()
		return false
	}
	delete(r.tokens, token)
	lease.token = ""
	lease.released = true
	if lease.registered {
		lease.registered = false
		r.active--
	}
	if lease.timer != nil {
		lease.timer.Stop()
		lease.timer = nil
	}
	r.mu.Unlock()
	_ = lease.closeIterators()
	return true
}

func (r *artifactStoreCursorRegistry) abandon(lease *artifactStoreCursorLease) error {
	if lease == nil {
		return nil
	}
	r.mu.Lock()
	if lease.released {
		r.mu.Unlock()
		return nil
	}
	lease.released = true
	if lease.registered {
		lease.registered = false
		r.active--
	}
	if lease.token != "" {
		delete(r.tokens, lease.token)
		lease.token = ""
	}
	if lease.timer != nil {
		lease.timer.Stop()
		lease.timer = nil
	}
	r.mu.Unlock()
	return lease.closeIterators()
}

func (r *artifactStoreCursorRegistry) close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	leases := make([]*artifactStoreCursorLease, 0, len(r.tokens))
	for token, lease := range r.tokens {
		delete(r.tokens, token)
		lease.token = ""
		lease.released = true
		if lease.registered {
			lease.registered = false
			r.active--
		}
		if lease.timer != nil {
			lease.timer.Stop()
			lease.timer = nil
		}
		leases = append(leases, lease)
	}
	r.mu.Unlock()
	for _, lease := range leases {
		_ = lease.closeIterators()
	}
}

func (l *artifactStoreCursorLease) closeIterators() error {
	if l == nil {
		return nil
	}
	var err error
	if l.originIterator != nil {
		err = errors.Join(err, l.originIterator.Close())
		l.originIterator = nil
	}
	if l.entryIterator != nil {
		err = errors.Join(err, l.entryIterator.Close())
		l.entryIterator = nil
	}
	return err
}

func (r *artifactStoreCursorRegistry) originPage(
	ctx context.Context,
	store artifact.ArtifactStore,
	token string,
	limit int,
) (_ []string, next string, retErr error) {
	return r.originPageForScope(ctx, store, token, limit, "origins")
}

func (r *artifactStoreCursorRegistry) peerOriginPage(
	ctx context.Context,
	store artifact.ArtifactStore,
	token string,
	limit int,
) (_ []string, next string, retErr error) {
	return r.originPageForScope(ctx, store, token, limit, "peers")
}

func (r *artifactStoreCursorRegistry) originPageForScope(
	ctx context.Context,
	store artifact.ArtifactStore,
	token string,
	limit int,
	scope string,
) (_ []string, next string, retErr error) {
	var lease *artifactStoreCursorLease
	if token != "" {
		var err error
		lease, err = r.claim(token, scope)
		if err != nil {
			return nil, "", err
		}
	} else {
		iterator, err := store.Origins(ctx)
		if err != nil {
			return nil, "", err
		}
		lease = &artifactStoreCursorLease{
			store: store, scope: scope, originIterator: iterator,
		}
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, r.abandon(lease))
		}
	}()
	origins, err := lease.originIterator.Next(ctx, limit)
	done := errors.Is(err, io.EOF)
	if err != nil && !done {
		return nil, "", err
	}
	if len(origins) > limit {
		return nil, "", fmt.Errorf("%w: artifact origin page exceeds requested limit", artifact.ErrArtifactInvalid)
	}
	if done {
		return origins, "", r.abandon(lease)
	}
	next, err = r.retain(lease)
	if err != nil {
		return nil, "", errors.Join(err, r.abandon(lease))
	}
	return origins, next, nil
}

func (r *artifactStoreCursorRegistry) indexPage(
	ctx context.Context,
	store artifact.ArtifactStore,
	origin, token string,
	limit int,
) (_ artifact.OriginArtifactIndex, next string, retErr error) {
	scope := "index:" + origin
	var lease *artifactStoreCursorLease
	if token != "" {
		var err error
		lease, err = r.claim(token, scope)
		if err != nil {
			return artifact.OriginArtifactIndex{}, "", err
		}
	} else {
		lease = &artifactStoreCursorLease{store: store, scope: scope}
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, r.abandon(lease))
		}
	}()
	index := artifact.OriginArtifactIndex{Origin: origin}
	remaining := limit
	for lease.kindIndex < len(artifactPeerKinds) && remaining > 0 {
		kind := artifactPeerKinds[lease.kindIndex]
		if lease.entryIterator == nil {
			var err error
			lease.entryIterator, err = lease.store.Entries(ctx, origin, kind)
			if err != nil {
				return artifact.OriginArtifactIndex{}, "", err
			}
		}
		items, err := lease.entryIterator.Next(ctx, remaining)
		done := errors.Is(err, io.EOF)
		if err != nil && !done {
			return artifact.OriginArtifactIndex{}, "", err
		}
		if len(items) > remaining {
			return artifact.OriginArtifactIndex{}, "", fmt.Errorf(
				"%w: artifact index page exceeds requested limit", artifact.ErrArtifactInvalid,
			)
		}
		if len(items) == 0 && !done {
			return artifact.OriginArtifactIndex{}, "", fmt.Errorf(
				"%w: artifact iterator made no progress", artifact.ErrArtifactInvalid,
			)
		}
		for _, entry := range items {
			wire, err := artifact.ToWireRef(entry.Ref)
			if err != nil {
				return artifact.OriginArtifactIndex{}, "", err
			}
			if err := appendArtifactWireName(&index, kind, wire.Name); err != nil {
				return artifact.OriginArtifactIndex{}, "", err
			}
		}
		remaining -= len(items)
		if done {
			if err := lease.entryIterator.Close(); err != nil {
				return artifact.OriginArtifactIndex{}, "", err
			}
			lease.entryIterator = nil
			lease.kindIndex++
		}
	}
	if lease.kindIndex == len(artifactPeerKinds) {
		return index, "", r.abandon(lease)
	}
	next, err := r.retain(lease)
	if err != nil {
		return artifact.OriginArtifactIndex{}, "", errors.Join(err, r.abandon(lease))
	}
	return index, next, nil
}

func appendArtifactWireName(
	index *artifact.OriginArtifactIndex, kind artifact.Kind, name string,
) error {
	switch kind {
	case artifact.KindSegments:
		index.Segments = append(index.Segments, name)
	case artifact.KindRaw:
		index.Raw = append(index.Raw, name)
	case artifact.KindManifests:
		index.Manifests = append(index.Manifests, name)
	case artifact.KindMeta:
		index.Meta = append(index.Meta, name)
	case artifact.KindCheckpoints:
		index.Checkpoints = append(index.Checkpoints, name)
	default:
		return fmt.Errorf("%w: unsupported artifact kind %q", artifact.ErrArtifactInvalid, kind)
	}
	return nil
}
