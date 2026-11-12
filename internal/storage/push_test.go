package storage

import (
	"encoding/json/v2"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
)

// failingTargetsReplica fails to list targets, as a backend does when its
// config section is malformed.
type failingTargetsReplica struct {
	stubReplica
	err      error
	resolved *int
}

func (f failingTargetsReplica) Targets(config.Config) ([]ReplicaTargetRef, error) {
	return nil, f.err
}

func (f failingTargetsReplica) ResolveTarget(
	cfg config.Config, ref ReplicaTargetRef,
) (ConfiguredReplica, error) {
	*f.resolved++
	return f.stubReplica.ResolveTarget(cfg, ref)
}

func TestSelectTargetsFailures(t *testing.T) {
	tests := []struct {
		name    string
		r       Replica
		target  string
		all     bool
		wantErr string
	}{
		{
			name:    "targets error surfaces",
			r:       failingTargetsReplica{err: errors.New("pg.targets.work: url is not a string"), resolved: new(int)},
			wantErr: "pg.targets.work: url is not a string",
		},
		{
			name:    "no targets",
			r:       stubReplica{},
			wantErr: "stub: no targets configured",
		},
		{
			name:    "no targets with --all",
			r:       stubReplica{},
			all:     true,
			wantErr: "stub: no targets configured",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			refs, err := SelectTargets(tt.r, config.Config{}, tt.target, tt.all)
			require.EqualError(t, err, tt.wantErr)
			assert.Nil(t, refs)
		})
	}
}

func TestSelectTargetsAllOnLegacyBlock(t *testing.T) {
	r := stubReplica{refs: []ReplicaTargetRef{{IsDefault: true}}}
	refs, err := SelectTargets(r, config.Config{}, "  ", true)
	require.NoError(t, err)
	assert.Equal(t, []ReplicaTargetRef{{IsDefault: true}}, refs)
}

func TestDefaultTargetDoesNotResolveAfterSelectionFails(t *testing.T) {
	resolved := 0
	r := failingTargetsReplica{err: errors.New("bad config"), resolved: &resolved}
	target, err := DefaultTarget(r, config.Config{})
	require.EqualError(t, err, "bad config")
	assert.Equal(t, ConfiguredReplica{}, target)
	assert.Zero(t, resolved)
}

func TestValidateProjectFilters(t *testing.T) {
	tests := []struct {
		name     string
		projects []string
		exclude  []string
		wantErr  string
	}{
		{name: "none"},
		{name: "include only", projects: []string{"alpha"}},
		{name: "exclude only", exclude: []string{"beta"}},
		{
			name: "both", projects: []string{"alpha"}, exclude: []string{"beta"},
			wantErr: "projects and exclude_projects are mutually exclusive",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateProjectFilters(tt.projects, tt.exclude)
			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

// The daemon streams push results to the CLI as JSON; durations travel as
// integer nanoseconds so both sides agree regardless of encoder defaults.
func TestPushResultDurationWireFormat(t *testing.T) {
	encoded, err := json.Marshal(PushResult{SessionsPushed: 2, Duration: 1500 * time.Millisecond})
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"Duration":1500000000`)

	var decoded PushResult
	require.NoError(t, json.Unmarshal(
		[]byte(`{"SessionsPushed":3,"Duration":2000000000,"Vectors":{"Skipped":true,"SkippedReason":"no generation"}}`),
		&decoded,
	))
	assert.Equal(t, 3, decoded.SessionsPushed)
	assert.Equal(t, 2*time.Second, decoded.Duration)
	assert.True(t, decoded.Vectors.Skipped)
	assert.Equal(t, "no generation", decoded.Vectors.SkippedReason)

	var bad PushResult
	require.Error(t, json.Unmarshal([]byte(`{"Duration":"2s"}`), &bad),
		"a string duration is not the wire format")
}

func TestMirrorPushResultDurationWireFormat(t *testing.T) {
	encoded, err := json.Marshal(MirrorPushResult{SessionsPushed: 4, Duration: 250 * time.Millisecond})
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"Duration":250000000`)

	var decoded MirrorPushResult
	require.NoError(t, json.Unmarshal(
		[]byte(`{"SessionsPushed":4,"Duration":750000000,"ArchiveRebuilt":true}`), &decoded,
	))
	assert.Equal(t, 4, decoded.SessionsPushed)
	assert.Equal(t, 750*time.Millisecond, decoded.Duration)
	assert.True(t, decoded.ArchiveRebuilt)
}
