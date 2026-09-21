package storage

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

// stubReplica is the smallest Replica: it proves the contract needs no SQL
// and drives the target-selection helpers.
type stubReplica struct {
	refs []ReplicaTargetRef
}

func (stubReplica) Name() string        { return "stub" }
func (stubReplica) DisplayName() string { return "Stub" }
func (s stubReplica) Targets(config.Config) ([]ReplicaTargetRef, error) {
	return s.refs, nil
}

func (stubReplica) ResolveTarget(
	_ config.Config, ref ReplicaTargetRef,
) (ConfiguredReplica, error) {
	return ConfiguredReplica{
		ReplicaTargetRef: ref,
		Target:           ReplicaTarget{URL: "stub://" + ref.Name},
	}, nil
}

func (stubReplica) ValidateTarget(ReplicaTarget) error { return nil }

func (stubReplica) NewPusher(context.Context, ReplicaTarget, *db.DB, PusherOptions) (Pusher, error) {
	return nil, nil
}
func (stubReplica) OpenStore(ReplicaTarget) (ReplicaStore, error) { return nil, nil }
func (stubReplica) OpenServeStore(context.Context, ReplicaTarget) (ReplicaStore, error) {
	return nil, nil
}

func (stubReplica) Status(context.Context, *db.DB, ConfiguredReplica, []string, []string) (ReplicaStatus, error) {
	return ReplicaStatus{}, nil
}

func (stubReplica) LastPushAt(context.Context, *db.DB, ConfiguredReplica, []string, []string) (string, error) {
	return "", nil
}

var _ Replica = stubReplica{}

func namedStub() stubReplica {
	return stubReplica{refs: []ReplicaTargetRef{
		{Name: "archive", IsDefault: true},
		{Name: "work"},
	}}
}

func TestSelectTargetsNamed(t *testing.T) {
	r := namedStub()
	cfg := config.Config{}

	all, err := SelectTargets(r, cfg, "", true)
	require.NoError(t, err)
	assert.Equal(t, r.refs, all)

	def, err := SelectTargets(r, cfg, "", false)
	require.NoError(t, err)
	assert.Equal(t, []ReplicaTargetRef{{Name: "archive", IsDefault: true}}, def)

	named, err := SelectTargets(r, cfg, " Work ", false)
	require.NoError(t, err)
	assert.Equal(t, []ReplicaTargetRef{{Name: "work"}}, named)

	_, err = SelectTargets(r, cfg, "missing", false)
	require.EqualError(t, err, `stub target "missing" is not configured`)

	_, err = SelectTargets(r, cfg, "work", true)
	require.EqualError(t, err, "target name cannot be combined with --all")
}

func TestSelectTargetsLegacyBlockRejectsNames(t *testing.T) {
	r := stubReplica{refs: []ReplicaTargetRef{{IsDefault: true}}}
	refs, err := SelectTargets(r, config.Config{}, "", false)
	require.NoError(t, err)
	assert.Equal(t, []ReplicaTargetRef{{IsDefault: true}}, refs)

	_, err = SelectTargets(r, config.Config{}, "work", false)
	require.EqualError(t, err,
		`stub target "work" is not configured; config uses a single legacy [stub] block`)
}

func TestDefaultTargetResolvesTheDefaultRef(t *testing.T) {
	target, err := DefaultTarget(namedStub(), config.Config{})
	require.NoError(t, err)
	assert.Equal(t, "archive", target.Name)
	assert.True(t, target.IsDefault)
	assert.Equal(t, "stub://archive", target.Target.URL)
}

func TestReplicaTargetRefSyncStateScope(t *testing.T) {
	tests := []struct {
		name    string
		ref     ReplicaTargetRef
		scope   string
		migrate bool
		label   string
	}{
		{"legacy", ReplicaTargetRef{IsDefault: true}, "", false, "default"},
		{"named default", ReplicaTargetRef{Name: "archive", IsDefault: true}, "archive", true, "archive (default)"},
		{"named other", ReplicaTargetRef{Name: "work"}, "work", false, "work"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.scope, tt.ref.SyncStateTarget())
			assert.Equal(t, tt.migrate, tt.ref.MigrateLegacySyncState())
			assert.Equal(t, tt.label, tt.ref.Label())
		})
	}
}
