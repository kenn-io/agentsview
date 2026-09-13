package rawderive

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/rawsync"
)

func rawIdentityDigest(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = fmt.Fprintf(h, "%d:", len(part))
		_, _ = h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// SourceID returns the stable identity of one authenticated raw source.
func SourceID(manifest rawsync.CanonicalManifest) string {
	return rawIdentityDigest(
		"source-v1",
		manifest.Identity.TenantID,
		manifest.Identity.DeviceID,
		string(manifest.Manifest.Provider),
		manifest.Manifest.ConfiguredRootID,
		manifest.Manifest.SourceKey,
	)
}

// GroupID returns the stable group and logical key for one parsed session.
func GroupID(manifest rawsync.CanonicalManifest, session db.Session) (groupID, logicalKey string) {
	logicalKey = session.SourceSessionID
	if logicalKey == "" {
		logicalKey = rawIdentityDigest("source-local-v1", SourceID(manifest), session.ID)
	}
	return rawIdentityDigest("group-v1", manifest.Identity.TenantID, session.Agent, logicalKey), logicalKey
}
