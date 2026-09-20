package clickhouse

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/agentsview/internal/config"
)

func TestReplicaTargetMapsPushVectors(t *testing.T) {
	assert.True(t, ReplicaTarget(config.ClickHouseConfig{URL: "clickhouse://h/db"}).PushVectors,
		"push_vectors defaults to enabled")
	off := false
	assert.False(t, ReplicaTarget(config.ClickHouseConfig{URL: "clickhouse://h/db", PushVectors: &off}).PushVectors)
	on := true
	assert.True(t, ReplicaTarget(config.ClickHouseConfig{URL: "clickhouse://h/db", PushVectors: &on}).PushVectors)
}
