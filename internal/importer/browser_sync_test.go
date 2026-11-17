package importer

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

// The stored Claude.ai freshness hashes must not change, or every synced chat
// would be fetched again. The values were computed with the formula Claude.ai
// Sync used before the shared runner.
func TestClaudeAIFreshnessGolden(t *testing.T) {
	full := &db.Session{
		MessageCount:       2,
		TranscriptRevision: new("rev-1"),
		SessionName:        new("Chat"),
		EndedAt:            new("2026-01-02T10:05:00.123456Z"),
	}
	for _, tc := range []struct {
		name    string
		policy  config.ArchiveContent
		updated string
		leaf    string
		session *db.Session
		want    uint64
	}{
		{"full archive", config.ArchiveContentFull, "2026-01-02T10:05:00.123456Z", "reply", full, 0x7f8734e1f0d009e4},
		{"usage archive marker", config.ArchiveContentUsage, "2026-01-02T10:05:00.123456Z", "reply", full, 0xeec446da29d8743f},
		{"unset fields", config.ArchiveContentFull, "2026-01-02T10:05:00Z", "leaf", &db.Session{}, 0xdc1844fc4cd126cc},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			d.SetArchiveContent(tc.policy)
			assert.Equal(t, tc.want, claudeAIFreshness(d, tc.updated, tc.leaf, tc.session))
		})
	}
}
