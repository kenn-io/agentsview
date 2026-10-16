package clickhouse

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/storage"
)

func TestReplicaTargetMapsPushVectors(t *testing.T) {
	assert.True(t, ReplicaTarget(config.ClickHouseConfig{URL: "clickhouse://h/db"}).PushVectors,
		"push_vectors defaults to enabled")
	off := false
	assert.False(t, ReplicaTarget(config.ClickHouseConfig{URL: "clickhouse://h/db", PushVectors: &off}).PushVectors)
	on := true
	assert.True(t, ReplicaTarget(config.ClickHouseConfig{URL: "clickhouse://h/db", PushVectors: &on}).PushVectors)
}

func TestReplicaTargetCarriesPushBatchSize(t *testing.T) {
	assert.Zero(t, ReplicaTarget(config.ClickHouseConfig{URL: "clickhouse://h/db"}).PushBatchSize,
		"an unset push_batch_size leaves the push default in place")
	size := 250
	assert.Equal(t, 250,
		ReplicaTarget(config.ClickHouseConfig{URL: "clickhouse://h/db", PushBatchSize: &size}).PushBatchSize)
}

func TestNewPusherRejectsNegativePushBatchSize(t *testing.T) {
	_, err := Backend{}.NewPusher(t.Context(), storage.ReplicaTarget{
		URL:           "clickhouse://localhost:9000/agentsview",
		MachineName:   "m",
		PushBatchSize: -1,
	}, nil, storage.PusherOptions{})
	require.EqualError(t, err, "clickhouse push_batch_size must be at least 1, got -1")
}
