package requestsign

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrivateKeyPolicyRotationAndRevocation(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "secret")
	key := signingKey(t)
	require.NoError(t, os.WriteFile(file, []byte(base64.StdEncoding.EncodeToString(key.Secret)), 0o600))
	policyPath := filepath.Join(dir, "policy.json")
	policy := Policy{ExternalURL: "https://example.com/native", ReplayDB: filepath.Join(dir, "replay.db"), Keys: []PolicyKey{{ID: key.ID, File: file, Grant: "reader"}}}
	write := func() {
		data, err := json.Marshal(policy)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(policyPath, data, 0o600))
	}
	write()
	_, keys, err := ReadPolicy(policyPath)
	require.NoError(t, err)
	assert.Equal(t, key.Secret, keys[key.ID].Secret)
	replacement := signingKey(t)
	require.NoError(t, os.WriteFile(file, []byte(base64.StdEncoding.EncodeToString(replacement.Secret)), 0o600))
	_, keys, err = ReadPolicy(policyPath)
	require.NoError(t, err)
	assert.Equal(t, replacement.Secret, keys[key.ID].Secret)
	if runtime.GOOS != "windows" {
		require.NoError(t, os.Chmod(file, 0o644))
		_, _, err = ReadPolicy(policyPath)
		require.Error(t, err)
		require.NoError(t, os.Chmod(file, 0o600))
	}
	policy.Keys[0].Grant = "admin"
	write()
	_, _, err = ReadPolicy(policyPath)
	require.Error(t, err)
	policy.Keys = nil
	write()
	_, _, err = ReadPolicy(policyPath)
	require.Error(t, err)
}

func TestReadPolicyRejectsDuplicateJSONMembers(t *testing.T) {
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "secret")
	key := signingKey(t)
	require.NoError(t, os.WriteFile(secretFile, []byte(base64.StdEncoding.EncodeToString(key.Secret)), 0o600))
	policy := Policy{
		ExternalURL: "https://example.com/native",
		ReplayDB:    filepath.Join(dir, "replay.db"),
		Keys:        []PolicyKey{{ID: key.ID, File: secretFile, Grant: "reader"}},
	}
	data, err := json.Marshal(policy)
	require.NoError(t, err)
	data = bytes.Replace(data,
		[]byte(`"external_url":"https://example.com/native"`),
		[]byte(`"external_url":"https://attacker.example","external_url":"https://example.com/native"`),
		1,
	)
	policyPath := filepath.Join(dir, "policy.json")
	require.NoError(t, os.WriteFile(policyPath, data, 0o600))
	_, _, err = ReadPolicy(policyPath)
	require.Error(t, err)
}

func TestConfiguredSignerRejectsLocalHTTPSDowngrade(t *testing.T) {
	file := filepath.Join(t.TempDir(), "key")
	require.NoError(t, GenerateSecretFile(file))
	t.Setenv("AGENTSVIEW_SIGNING_URL", "https://127.0.0.1:8443/native")
	t.Setenv("AGENTSVIEW_SIGNING_KEY_ID", "reader")
	t.Setenv("AGENTSVIEW_SIGNING_KEY_FILE", file)
	_, err := ClientFromEnvironment("http://127.0.0.1:8443/native", nil)
	require.Error(t, err)
}
