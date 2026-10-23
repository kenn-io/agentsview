package sync

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"hegel.dev/go/hegel"
)

func TestHostedRawRejectsDerivationEntryPoints(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	database.DisableLocalDerivation()
	hegel.Test(t, func(ht *hegel.T) {
		provider := newProcessFixtureProvider(parser.SourceRef{Provider: parser.AgentCowork, Key: "fixture"}, parser.SourceFingerprint{}, parser.ParseOutcome{})
		engine := NewEngine(t.Context(), database, EngineConfig{ProviderFactories: []parser.ProviderFactory{processFixtureFactory{provider: provider}}, FrictionEnabled: hegel.Draw(ht, hegel.Booleans()), ArchiveReparse: hegel.Draw(ht, hegel.Booleans())})
		defer engine.Close()
		engine.ReconfigureSources(SourceConfig{})
		which := hegel.Draw(ht, hegel.Integers(0, 9))
		var err error
		switch which {
		case 0:
			err = engine.SyncPathsContext(t.Context(), []string{"fixture"})
		case 1:
			err = engine.SyncSingleSessionContext(t.Context(), "cowork:fixture")
		case 2:
			_, err = engine.ParseDiff(t.Context(), ParseDiffOptions{})
		case 3:
			_, err = engine.BackfillFriction(t.Context())
		case 4:
			err = engine.RunStartupMaintenance(t.Context(), func() error { provider.calls = append(provider.calls, "maintenance"); return nil })
		case 7:
			err = engine.RunExclusive(func() error { provider.calls = append(provider.calls, "exclusive"); return nil })
		case 8:
			err = engine.RunExclusiveCtx(t.Context(), func() error { provider.calls = append(provider.calls, "exclusive"); return nil })
		case 9:
			err = engine.RunExclusiveFlushed(func() error { provider.calls = append(provider.calls, "exclusive"); return nil })
		case 6:
			err = engine.TryRunExclusive(func() error { provider.calls = append(provider.calls, "exclusive"); return nil })
		case 5:
			_, err = engine.ResyncAllWithOptions(t.Context(), nil, RebuildOptions{})
		}
		require.ErrorIs(ht, err, config.ErrHostedRawMode)
		require.Empty(ht, provider.calls)
	}, hegel.WithTestCases(1000), hegel.WithDatabase(""))
}

func TestHostedRawParseDiffRegression(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	database.DisableLocalDerivation()
	engine := NewDiffEngine(t.Context(), database, EngineConfig{Ephemeral: true})
	defer engine.Close()
	_, err := engine.ParseDiff(t.Context(), ParseDiffOptions{})
	require.ErrorIs(t, err, config.ErrHostedRawMode)
}

func TestHostedRawParserCounterHasLocalPositiveControl(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		hosted := hegel.Draw(ht, hegel.Booleans())
		database := dbtest.OpenTestDB(t)
		if hosted {
			database.DisableLocalDerivation()
		}
		path := filepath.Join(t.TempDir(), "fixture.json")
		source := parser.SourceRef{Provider: parser.AgentCowork, Key: path, FingerprintKey: path}
		provider := newProcessFixtureProvider(source, parser.SourceFingerprint{Key: path, Size: 42, MTimeNS: 1}, parser.ParseOutcome{})
		witness := errors.New("parser witness")
		provider.parseErr = witness
		engine := NewEngine(t.Context(), database, EngineConfig{Ephemeral: true, ProviderFactories: []parser.ProviderFactory{processFixtureFactory{provider: provider}}, ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{parser.AgentCowork: parser.ProviderMigrationProviderAuthoritative}})
		defer engine.Close()
		result := engine.processFile(t.Context(), parser.DiscoveredFile{Path: path, Agent: parser.AgentCowork, ProviderSource: &source, ProviderProcess: true})
		if hosted {
			require.ErrorIs(ht, result.err, config.ErrHostedRawMode)
			require.Zero(ht, countProcessFixtureCalls(provider.calls, "parse"))
		} else {
			require.ErrorIs(ht, result.err, witness)
			require.Equal(ht, 1, countProcessFixtureCalls(provider.calls, "parse"))
		}
	}, hegel.WithTestCases(8), hegel.WithDatabase(""))
}

func TestHostedRawTryExclusiveRegression(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	database.DisableLocalDerivation()
	engine := NewEngine(t.Context(), database, EngineConfig{})
	defer engine.Close()
	require.ErrorIs(t, engine.TryRunExclusive(func() error { require.FailNow(t, "hosted callback ran"); return nil }), config.ErrHostedRawMode)
}
