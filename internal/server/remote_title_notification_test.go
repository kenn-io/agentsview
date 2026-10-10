package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/remotesync"
)

func TestRemoteTitleOnlySyncNotifiesSessions(t *testing.T) {
	broadcaster := NewBroadcaster(0)
	fixture := newSyncRouteFixture(t, withBroadcasterForSyncRoutes(broadcaster))
	engine := fixture.srv.syncEngineForLocal(t.Context(), fixture.db)
	changed := 1
	stubRunHTTPRemoteSync(t, func(context.Context, config.RemoteHost, bool) (remotesync.SyncStats, error) {
		return remotesync.SyncStats{TitlesUpdated: changed}, nil
	})
	events, unsubscribe := broadcaster.Subscribe()
	defer unsubscribe()
	run := func() {
		response := fixture.srv.runRemoteSyncRequest(t.Context(), fixture.db, engine,
			remoteSyncRequest{Hosts: []config.RemoteHost{{Host: "alpha"}}}, nil)
		assert.Empty(t, response.Failures)
		assert.Empty(t, response.Error)
	}
	run()
	select {
	case event := <-events:
		assert.Equal(t, "sessions", event.Scope)
	default:
		t.Fatal("a title-only remote sync must publish a session refresh")
	}
	changed = 0
	run()
	select {
	case event := <-events:
		t.Fatalf("unchanged remote sync published %s", event.Scope)
	default:
	}
}
