package rawderive

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
)

func TestRawIdentityUsesStableLengthPrefixedDigests(t *testing.T) {
	manifest := rawsync.CanonicalManifest{
		Identity: rawsync.AuthIdentity{TenantID: "tenant-a", DeviceID: "device-a"},
		Manifest: rawsync.Manifest{Provider: parser.AgentCodex, ConfiguredRootID: "root-a", SourceKey: "source/key"},
	}
	assert.Equal(t, "e5bf6a59697d0ed269ca788904fd9ec5959751ace3ad7f9cecfc99b83a181314", SourceID(manifest))

	group, key := GroupID(manifest, db.Session{ID: "physical-id", Agent: "codex", SourceSessionID: "portable-id"})
	assert.Equal(t, "portable-id", key)
	assert.Equal(t, "2eb0f0c42ce21dd996d3cdc7fa4f431edc15f0f2b0ff39d4a46f81579746ab88", group)

	group, key = GroupID(manifest, db.Session{ID: "physical-id", Agent: "codex"})
	assert.Equal(t, "28d259c0ba5694959186fdb1515d8225b18686c677c5ca9d66c869ea199de4d3", key)
	assert.Equal(t, "76479fa531a62e6beef876b209e2d2afca445f34c794c935f73a0e8217fea546", group)
}

func TestRawIdentityNonportableIDsAreSourceScoped(t *testing.T) {
	manifest := rawsync.CanonicalManifest{
		Identity: rawsync.AuthIdentity{TenantID: "tenant-a", DeviceID: "device-a"},
		Manifest: rawsync.Manifest{Provider: parser.AgentCodex, ConfiguredRootID: "root-a", SourceKey: "source/a"},
	}
	other := manifest
	other.Manifest.SourceKey = "source/b"
	left, leftKey := GroupID(manifest, db.Session{ID: "same", Agent: "codex"})
	right, rightKey := GroupID(other, db.Session{ID: "same", Agent: "codex"})
	assert.NotEqual(t, leftKey, rightKey)
	assert.NotEqual(t, left, right)

	portableLeft, _ := GroupID(manifest, db.Session{ID: "one", Agent: "codex", SourceSessionID: "portable"})
	portableRight, _ := GroupID(other, db.Session{ID: "two", Agent: "codex", SourceSessionID: "portable"})
	assert.Equal(t, portableLeft, portableRight)
}

func TestRawIdentityUsesSourceKeyBytesInsteadOfStoredDigest(t *testing.T) {
	manifest := rawsync.CanonicalManifest{
		Identity:      rawsync.AuthIdentity{TenantID: "tenant-a", DeviceID: "device-a"},
		Manifest:      rawsync.Manifest{Provider: parser.AgentCodex, ConfiguredRootID: "root-a", SourceKey: "source/key"},
		CanonicalJSON: []byte("unrelated stored bytes"),
	}
	first := SourceID(manifest)
	manifest.CanonicalJSON = []byte("different stored bytes")
	assert.Equal(t, first, SourceID(manifest))
}
