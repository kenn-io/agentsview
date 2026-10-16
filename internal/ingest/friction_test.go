package ingest

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
)

func TestComputeSessionFrictionBoundsErrorFindingText(t *testing.T) {
	const sessionID = "long-error"
	longError := strings.Repeat("日", friction.MaxErrorMessageLength+1)
	update, err := ComputeSessionFriction(db.Session{
		ID: sessionID, Project: "project", Machine: "local", Agent: "claude",
	}, []db.Message{{
		SessionID: sessionID, Ordinal: 1, Role: "assistant",
		ToolCalls: []db.ToolCall{{
			ToolName: "Bash", Category: "Bash",
			ResultEvents: []db.ToolResultEvent{{
				Status: "errored", Content: longError,
			}},
		}},
	}}, nil, FrictionOptions{})
	require.NoError(t, err)
	require.Len(t, update.Findings, 1)

	finding := update.Findings[0]
	wantText := strings.Repeat("日", friction.MaxErrorMessageLength) + " … [truncated]"
	wantTitle := "[friction/error] Bash: " + strings.Repeat("日", 80)
	assert.Equal(t, wantText, finding.Text)
	assert.Equal(t, wantTitle, finding.Title)
	titleHash := sha256.Sum256([]byte(wantTitle))
	assert.Equal(t, "fl1:"+hex.EncodeToString(titleHash[:]), finding.Fingerprint)
}
