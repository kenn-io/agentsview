package requestsign

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"io"
	"os"
	"strings"
)

func readPrivate(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("opening private signing file failed")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || !isPrivateRegularFile(f) {
		return nil, errors.New("signing files must be private regular files")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, errors.New("reading private signing file failed")
	}
	return b, nil
}

func ReadSecret(path string) ([]byte, error) {
	b, err := readPrivate(path, 4096)
	if err != nil {
		return nil, err
	}
	secret, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(secret) < 64 {
		return nil, errors.New("signing secret must contain at least 64 base64-decoded bytes")
	}
	return secret, nil
}

type Policy struct {
	ExternalURL string      `json:"external_url"`
	StripPrefix bool        `json:"strip_prefix"`
	ReplayDB    string      `json:"replay_db"`
	Listen      string      `json:"listen"`
	Keys        []PolicyKey `json:"keys"`
}

type PolicyKey struct {
	ID       string `json:"id"`
	File     string `json:"key_file"`
	Grant    string `json:"grant"`
	DeviceID string `json:"device_id,omitempty"`
}

// ReadPolicy rereads secrets so replacing/removing entries rotates/revokes keys.
// A malformed or unreadable entry denies the entire policy, never a cached key.
func ReadPolicy(file string) (Policy, map[string]Key, error) {
	var policy Policy
	b, err := readPrivate(file, 64<<10)
	if err != nil {
		return policy, nil, err
	}
	d := jsontext.NewDecoder(bytes.NewReader(b))
	if err = json.UnmarshalDecode(d, &policy, json.RejectUnknownMembers(true)); err != nil {
		return policy, nil, errors.New("invalid signing policy")
	}
	if _, err = d.ReadValue(); !errors.Is(err, io.EOF) {
		return policy, nil, errors.New("invalid signing policy")
	}
	if _, err = externalBase(policy.ExternalURL); err != nil {
		return policy, nil, err
	}
	if len(policy.Keys) == 0 || len(policy.Keys) > 64 || policy.ReplayDB == "" {
		return policy, nil, errors.New("signing policy requires keys and replay state")
	}
	keys := make(map[string]Key, len(policy.Keys))
	for _, entry := range policy.Keys {
		if !safeID.MatchString(entry.ID) || keys[entry.ID].ID != "" || entry.Grant != "reader" && entry.Grant != "contributor" || entry.Grant == "contributor" && entry.DeviceID == "" {
			return policy, nil, errors.New("invalid signing key grant")
		}
		secret, err := ReadSecret(entry.File)
		if err != nil {
			return policy, nil, err
		}
		keys[entry.ID] = Key{entry.ID, secret, entry.Grant, entry.DeviceID}
	}
	return policy, keys, nil
}

// GenerateSecretFile writes a new private random key exclusively, without
// returning or printing the encoded secret. Existing keys are never overwritten.
func GenerateSecretFile(path string) error {
	secret := make([]byte, 64)
	if _, err := rand.Read(secret); err != nil {
		return errors.New("generating signing key failed")
	}
	f, err := createPrivateFile(path)
	if err != nil {
		return errors.New("creating signing key file failed")
	}
	defer f.Close()
	if err = ensurePrivateFile(f); err != nil {
		return errors.New("signing files must be private regular files")
	}
	if _, err = io.WriteString(f, base64.StdEncoding.EncodeToString(secret)+"\n"); err != nil {
		return errors.New("writing signing key failed")
	}
	if err = f.Sync(); err != nil {
		return errors.New("persisting signing key failed")
	}
	return nil
}
