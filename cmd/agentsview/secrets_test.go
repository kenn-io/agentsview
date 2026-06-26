package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
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

// TestSecretsScan_DirectMode_Scans verifies `secrets scan` is wired with a
// real sync.Engine in direct mode (no daemon). A nil-engine direct backend
// would make ScanSecrets return db.ErrReadOnly; instead the scan must run and
// find the seeded secret.
func TestSecretsScan_DirectMode_Scans(t *testing.T) {
	dataDir := testDataDir(t)
	seedSession(t, dataDir, "leaky", "proj")

	d, err := db.Open(filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, err)
	secret := syntheticAWSAccessKey(t.Name())
	require.NoError(t, d.InsertMessages([]db.Message{{
		SessionID: "leaky", Ordinal: 0, Role: "user",
		Content: "my key " + secret + " here",
	}}))
	require.NoError(t, d.Close())

	out, err := executeCommand(newRootCommand(),
		"secrets", "scan", "--backfill", "--format", "json")
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
}

func TestSecretsScan_DirectMode_DeniesAgentsviewFixtures(t *testing.T) {
	dataDir := testDataDir(t)
	seedSession(t, dataDir, "fixture", "proj")

	d, err := db.Open(filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, err)
	secret := strings.Join([]string{
		"ghp_", "M7qL8r", "P2sT5u", "V9wX3y",
		"Z6aB1c", "D4eF7g", "H0iJ2k",
	}, "")
	require.NoError(t, d.InsertMessages([]db.Message{{
		SessionID: "fixture", Ordinal: 0, Role: "user",
		Content: "fixture token " + secret,
	}}))
	require.NoError(t, d.Close())

	out, err := executeCommand(newRootCommand(),
		"secrets", "scan", "--backfill", "--format", "json")
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
}

// TestSecretsScanHint_ShownOnCandidate verifies the hint is printed
// when at least one candidate finding exists and output is not JSON.
func TestSecretsScanHint_ShownOnCandidate(t *testing.T) {
	dataDir := testDataDir(t)
	seedSession(t, dataDir, "leaky", "proj")
	d, err := db.Open(filepath.Join(dataDir, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	// Candidate finding only (high-entropy assignment), no definite leak.
	if err := d.InsertMessages([]db.Message{{
		SessionID: "leaky", Ordinal: 0, Role: "user",
		Content: "SECRET=Xa9Kd03Lm5Qp7Rt2Vw8Zb4Nc6 here",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := executeCommand(newRootCommand(),
		"secrets", "scan", "--backfill")
	if err != nil {
		t.Fatalf("secrets scan: %v", err)
	}
	if !strings.Contains(out, "Candidate findings are hidden") {
		t.Errorf("expected hint in output, got: %s", out)
	}
	if !strings.Contains(out, "--confidence all") {
		t.Errorf("expected hint to mention --confidence all, got: %s", out)
	}
}

// TestSecretsScanHint_SuppressedWhenDefiniteOnly verifies the hint is
// NOT printed when only definite findings exist.
func TestSecretsScanHint_SuppressedWhenDefiniteOnly(t *testing.T) {
	dataDir := testDataDir(t)
	seedSession(t, dataDir, "leaky", "proj")
	d, err := db.Open(filepath.Join(dataDir, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	secret := syntheticAWSAccessKey(t.Name())
	if err := d.InsertMessages([]db.Message{{
		SessionID: "leaky", Ordinal: 0, Role: "user",
		Content: "my key " + secret + " here",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := executeCommand(newRootCommand(),
		"secrets", "scan", "--backfill")
	if err != nil {
		t.Fatalf("secrets scan: %v", err)
	}
	if strings.Contains(out, "Candidate findings are hidden") {
		t.Errorf("hint should be absent for definite-only scan, got: %s", out)
	}
}

// TestSecretsScanHint_SuppressedInJSON verifies the hint is NOT printed
// in JSON mode even when candidates exist.
func TestSecretsScanHint_SuppressedInJSON(t *testing.T) {
	dataDir := testDataDir(t)
	seedSession(t, dataDir, "leaky", "proj")
	d, err := db.Open(filepath.Join(dataDir, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.InsertMessages([]db.Message{{
		SessionID: "leaky", Ordinal: 0, Role: "user",
		Content: "SECRET=Xa9Kd03Lm5Qp7Rt2Vw8Zb4Nc6 here",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := executeCommand(newRootCommand(),
		"secrets", "scan", "--backfill", "--format", "json")
	if err != nil {
		t.Fatalf("secrets scan: %v", err)
	}
	if strings.Contains(out, "Candidate findings are hidden") {
		t.Errorf("hint should be absent in JSON mode, got: %s", out)
	}
	var sum struct {
		CandidateFindings int `json:"candidate_findings"`
	}
	if err := json.Unmarshal([]byte(out), &sum); err != nil {
		t.Fatalf("expected JSON output, got: %s", out)
	}
	if sum.CandidateFindings == 0 {
		t.Errorf("expected candidate_findings > 0, got %d",
			sum.CandidateFindings)
	}
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
