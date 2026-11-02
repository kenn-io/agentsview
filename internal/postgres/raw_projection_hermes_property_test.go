//go:build (linux && !android && (amd64 || arm64)) || (darwin && !ios && arm64) || (windows && (amd64 || arm64))

package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"hegel.dev/go/hegel"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
)

func TestHermesPropertyProjectionIdentitySurvivesDatabaseFallback(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		id := "hermes:" + hegel.Draw(ht, hegel.Text())
		native := hegel.Draw(ht, hegel.Text())
		m := rawsync.CanonicalManifest{Manifest: rawsync.Manifest{Provider: parser.AgentHermes, ConfiguredRootID: "root-a", SourceKey: "/fixture/home/state.db"}}
		session := db.Session{ID: id, Agent: string(parser.AgentHermes), SourceSessionID: native}
		group, _ := rawGroupID(m, session)
		alias := rawBaseAlias(m, session)
		session.SourceSessionID = ""
		fallbackGroup, _ := rawGroupID(m, session)
		assert.Equal(ht, group, fallbackGroup)
		assert.Equal(ht, alias, rawBaseAlias(m, session))
	}, hegel.WithTestCases(1000))
}

func TestHermesPropertyProjectionIdentitySeparatesHomes(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		session := db.Session{ID: "hermes:" + hegel.Draw(ht, hegel.Text()), Agent: string(parser.AgentHermes), SourceSessionID: hegel.Draw(ht, hegel.Text())}
		m := rawsync.CanonicalManifest{Manifest: rawsync.Manifest{Provider: parser.AgentHermes, ConfiguredRootID: "root-a", SourceKey: "/fixture/alpha/state.db"}}
		group, _ := rawGroupID(m, session)
		alias := rawBaseAlias(m, session)
		m.Manifest.SourceKey = "/fixture/beta/state.db"
		otherGroup, _ := rawGroupID(m, session)
		assert.NotEqual(ht, group, otherGroup)
		assert.NotEqual(ht, alias, rawBaseAlias(m, session))
	}, hegel.WithTestCases(1000))
}
