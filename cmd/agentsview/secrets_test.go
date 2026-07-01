package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
)

func TestNewSecretsListCommandFlags(t *testing.T) {
	cmd := newSecretsListCommand()
	// confidence is validated server-side, so cobra must accept any value.
	cmd.SetArgs([]string{"--confidence", "bogus", "--reveal", "--limit", "5"})
	for _, name := range []string{"project", "agent", "rule", "confidence",
		"reveal", "limit", "cursor", "date-from", "date-to"} {
		assert.NotNil(t, cmd.Flags().Lookup(name),
			"secrets list missing --%s flag", name)
	}
}

func TestNewSecretsScanCommandFlags(t *testing.T) {
	cmd := newSecretsScanCommand()
	for _, name := range []string{"backfill", "project", "agent",
		"date-from", "date-to"} {
		assert.NotNil(t, cmd.Flags().Lookup(name),
			"secrets scan missing --%s flag", name)
	}
}

func syntheticAWSAccessKey(seed string) string {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	sum := sha256.Sum256([]byte(seed))
	body := make([]byte, 16)
	for i := range body {
		body[i] = alphabet[int(sum[i])%len(alphabet)]
	}
	return "AKIA" + string(body)
}

func TestSecretsScanFixture(t *testing.T) {
	const candidateSecret = "SECRET=Xa9Kd03Lm5Qp7Rt2Vw8Zb4Nc6 here"
	fixtureSecret := strings.Join([]string{
		"ghp_", "M7qL8r", "P2sT5u", "V9wX3y",
		"Z6aB1c", "D4eF7g", "H0iJ2k",
	}, "")

	setupSecretsScanFixture(t,
		secretsScanSeed{
			id:      "leaky-json",
			project: "scan-positive",
			content: "my key " + syntheticAWSAccessKey(t.Name()+"/positive") + " here",
		},
		secretsScanSeed{
			id:      "fixture",
			project: "fixture-deny",
			content: "fixture token " + fixtureSecret,
		},
		secretsScanSeed{
			id:      "candidate-human",
			project: "hint-candidate-human",
			content: candidateSecret,
		},
		secretsScanSeed{
			id:      "definite-human",
			project: "hint-definite",
			content: "my key " + syntheticAWSAccessKey(t.Name()+"/definite") + " here",
		},
		secretsScanSeed{
			id:      "candidate-json",
			project: "hint-candidate-json",
			content: candidateSecret,
		},
	)

	t.Run("direct mode scans", func(t *testing.T) {
		out, err := executeCommand(newRootCommand(),
			"secrets", "scan", "--backfill",
			"--project", "scan-positive", "--format", "json")
		require.NoError(t, err, "secrets scan failed (engine not plumbed?)")
		var got struct {
			Scanned       int `json:"scanned"`
			WithSecrets   int `json:"with_secrets"`
			TotalFindings int `json:"total_findings"`
		}
		require.NoError(t, json.Unmarshal([]byte(out), &got),
			"scan output not JSON: %q", out)
		assert.GreaterOrEqual(t, got.Scanned, 1,
			"expected the seeded secret to be found, got %+v", got)
		assert.GreaterOrEqual(t, got.WithSecrets, 1,
			"expected the seeded secret to be found, got %+v", got)
		assert.GreaterOrEqual(t, got.TotalFindings, 1,
			"expected the seeded secret to be found, got %+v", got)
	})

	t.Run("denies agentsview fixtures", func(t *testing.T) {
		out, err := executeCommand(newRootCommand(),
			"secrets", "scan", "--backfill",
			"--project", "fixture-deny", "--format", "json")
		require.NoError(t, err, "secrets scan failed")
		var got struct {
			Scanned       int `json:"scanned"`
			WithSecrets   int `json:"with_secrets"`
			TotalFindings int `json:"total_findings"`
		}
		require.NoError(t, json.Unmarshal([]byte(out), &got),
			"scan output not JSON: %q", out)
		assert.Equal(t, 1, got.Scanned, "fixture should be suppressed, got %+v", got)
		assert.Equal(t, 0, got.WithSecrets, "fixture should be suppressed, got %+v", got)
		assert.Equal(t, 0, got.TotalFindings, "fixture should be suppressed, got %+v", got)
	})

	t.Run("hint shown on candidate", func(t *testing.T) {
		out, err := executeCommand(newRootCommand(),
			"secrets", "scan", "--backfill",
			"--project", "hint-candidate-human")
		require.NoError(t, err, "secrets scan")
		assert.Contains(t, out, "Candidate findings are hidden")
		assert.Contains(t, out, "--confidence all")
	})

	t.Run("hint suppressed when definite only", func(t *testing.T) {
		out, err := executeCommand(newRootCommand(),
			"secrets", "scan", "--backfill",
			"--project", "hint-definite")
		require.NoError(t, err, "secrets scan")
		assert.NotContains(t, out, "Candidate findings are hidden")
	})

	t.Run("hint suppressed in json", func(t *testing.T) {
		out, err := executeCommand(newRootCommand(),
			"secrets", "scan", "--backfill",
			"--project", "hint-candidate-json", "--format", "json")
		require.NoError(t, err, "secrets scan")
		assert.NotContains(t, out, "Candidate findings are hidden")
		var sum struct {
			CandidateFindings int `json:"candidate_findings"`
		}
		require.NoError(t, json.Unmarshal([]byte(out), &sum),
			"expected JSON output, got: %s", out)
		assert.NotZero(t, sum.CandidateFindings)
	})
}

type secretsScanSeed struct {
	id      string
	project string
	content string
}

func setupSecretsScanFixture(t *testing.T, seeds ...secretsScanSeed) {
	t.Helper()
	dataDir := testDataDir(t)
	sessionSeeds := make([]sessionSeed, 0, len(seeds))
	messages := make([]db.Message, 0, len(seeds))
	for _, seed := range seeds {
		sessionSeeds = append(sessionSeeds, sessionSeed{
			id:      seed.id,
			project: seed.project,
		})
		messages = append(messages, db.Message{
			SessionID: seed.id,
			Ordinal:   0,
			Role:      "user",
			Content:   seed.content,
		})
	}
	seedSessionArchiveRows(t, dataDir, sessionSeeds...)
	dbtest.EnsureTestDBAt(t, sessionsDBPath(dataDir))
	d, err := db.Open(sessionsDBPath(dataDir))
	require.NoError(t, err)
	require.NoError(t, d.InsertMessages(messages))
	require.NoError(t, d.Close())
	registerSQLiteWritableDaemonRuntime(t, dataDir)
}

func TestPrintSecretFindingsHuman(t *testing.T) {
	var buf bytes.Buffer
	res := &service.SecretFindingList{
		Findings: []db.SecretFindingRow{},
	}
	require.NoError(t, printSecretFindingsHuman(&buf, res))
	assert.Contains(t, buf.String(), "(no findings)",
		"empty list should print (no findings)")
}
