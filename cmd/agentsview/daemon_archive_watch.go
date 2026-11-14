package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/apiclient"
)

const (
	archiveWatchRetryMin = 2 * time.Second
	archiveWatchRetryMax = 30 * time.Second
)

type archiveNotificationGate struct {
	mu      sync.Mutex
	notify  func()
	opened  bool
	pending bool
}

func (g *archiveNotificationGate) changed() {
	g.mu.Lock()
	if !g.opened {
		g.pending = true
		g.mu.Unlock()
		return
	}
	g.mu.Unlock()
	g.notify()
}

func (g *archiveNotificationGate) open() {
	g.mu.Lock()
	g.opened = true
	pending := g.pending
	g.pending = false
	g.mu.Unlock()
	if pending {
		g.notify()
	}
}

func (b daemonArchiveWriteBackend) startArchiveNotifications(
	ctx context.Context, notify func(),
) (stop, open func()) {
	if b.watchHooks != nil && b.watchHooks.startArchiveNotifications != nil {
		return b.watchHooks.startArchiveNotifications(ctx, notify)
	}
	ctx, cancel := context.WithCancel(ctx)
	gate := &archiveNotificationGate{notify: notify}
	done := make(chan struct{})
	nextWriter := b.watchPushWriter()
	go func() {
		defer close(done)
		retry := archiveWatchRetryMin
		for ctx.Err() == nil {
			started := time.Now()
			if err := watchArchiveOwner(ctx, nextWriter, gate.changed); err == nil || ctx.Err() != nil {
				return
			}
			if time.Since(started) >= archiveWatchRetryMax {
				retry = archiveWatchRetryMin
			}
			log.Printf("archive push watch: notifications unavailable; retrying in %s", retry)
			timer := time.NewTimer(retry)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			retry = min(2*retry, archiveWatchRetryMax)
		}
	}()
	return func() {
		cancel()
		<-done
	}, gate.open
}

func watchArchiveOwner(
	ctx context.Context,
	resolve func(context.Context) (archiveWriteBackend, func(), error),
	notify func(),
) error {
	writer, cleanup, err := resolve(ctx)
	if err != nil {
		return err
	}
	defer cleanup()
	daemon, ok := writer.(daemonArchiveWriteBackend)
	if !ok {
		return errors.New("archive owner is not a daemon; periodic pushes remain enabled")
	}
	return watchDaemonArchive(ctx, daemon.tr, daemon.appCfg.AuthToken, notify)
}

func watchDaemonArchive(
	ctx context.Context, tr transport, token string, notify func(),
) error {
	api, err := apiclient.NewHTTPClient(tr.URL, token, &http.Client{Timeout: 0})
	if err != nil {
		return err
	}
	response, err := api.GetAPIV1EventsStreamWithResponse(ctx)
	if response != nil && response.HTTPResponse != nil {
		defer response.HTTPResponse.Body.Close()
	}
	if err != nil {
		return fmt.Errorf("opening archive notifications: %w", err)
	}
	if response == nil || response.Stream200 == nil {
		return errors.New("daemon did not provide archive notifications")
	}
	stream := response.Stream200
	defer stream.Close()
	for stream.Next() {
		switch stream.Event().Type {
		case "events_ready", "data_changed":
			notify()
		case "error":
			return errors.New("daemon archive notification stream failed")
		}
	}
	if err := stream.Err(); err != nil {
		return err
	}
	return io.ErrUnexpectedEOF
}
