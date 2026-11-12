package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/server"
)

// Each record names its contract and summary digests, allowing identical
// contracts with different summaries to merge without a file conflict.
const apiVersionRecordDir = "cmd/agentsview/testdata/api_versions"

type apiVersionRecord struct {
	version       int
	digest        string
	summaryDigest string
}

func (r apiVersionRecord) name() string {
	return fmt.Sprintf("%d-%s-%s", r.version, strings.TrimPrefix(r.digest, "sha256:"), r.summaryDigest)
}

func summaryDigest(summary string) string {
	digest := sha256.Sum256([]byte(summary))
	return hex.EncodeToString(digest[:])
}

func loadAPIVersionRecords(dir string) ([]apiVersionRecord, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var records []apiVersionRecord
	for _, e := range entries {
		parts := strings.Split(e.Name(), "-")
		if len(parts) != 3 {
			return nil, fmt.Errorf("API version record %s must be named <version>-<contract digest>-<summary digest>", e.Name())
		}
		version, err := strconv.Atoi(parts[0])
		if err != nil || version < 1 || len(parts[1]) != 64 || len(parts[2]) != 64 {
			return nil, fmt.Errorf("invalid API version record path %s", e.Name())
		}
		if _, err := hex.DecodeString(parts[1]); err != nil {
			return nil, fmt.Errorf("invalid API version record path %s", e.Name())
		}
		content, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		summary := strings.TrimSpace(string(content))
		if summary == "" || strings.ContainsAny(summary, "\r\n") || (string(content) != summary && string(content) != summary+"\n") || summaryDigest(summary) != parts[2] {
			return nil, fmt.Errorf("API version record %s must contain a trimmed one-line summary matching its summary digest", e.Name())
		}
		records = append(records, apiVersionRecord{version, "sha256:" + parts[1], parts[2]})
	}
	return records, nil
}

func baseAPIVersionRecords(ctx context.Context) ([]string, error) {
	base := os.Getenv("API_VERSION_BASE")
	if base == "" {
		if os.Getenv("CI") != "" {
			return nil, fmt.Errorf("API_VERSION_BASE is required in CI")
		}
		return nil, nil
	}
	output, err := exec.CommandContext(ctx, "git", "-C", "../..", "ls-tree", "-r", "--name-only", base, "--", apiVersionRecordDir).Output()
	if err != nil {
		return nil, fmt.Errorf("read API version records from base %q: %w", base, err)
	}
	var names []string
	for _, path := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if path != "" {
			names = append(names, filepath.Base(path))
		}
	}
	return names, nil
}

func checkAPIVersionContract(apiVersion int, digest string, records []apiVersionRecord, base []string) error {
	byVersion := map[int]apiVersionRecord{}
	paths := map[string]bool{}
	for _, r := range records {
		paths[r.name()] = true
		if previous, ok := byVersion[r.version]; ok && previous.digest != r.digest {
			return fmt.Errorf("API version %d has different contract digests (%s, %s); delete the file this PR added, rebase onto main after the earlier PR merges, and rerun make test-api-version. The next free version is %d", r.version, previous.name(), r.name(), apiVersion+1)
		}
		byVersion[r.version] = r
	}
	used := false
	for _, name := range base {
		if !paths[name] {
			return fmt.Errorf("API version record %s from the target base is missing; restore it", name)
		}
		version, _, _ := strings.Cut(name, "-")
		if version == strconv.Itoa(apiVersion) {
			used = true
		}
	}
	path := func(version int) string { return apiVersionRecord{version, digest, "<summary digest>"}.name() }
	record, ok := byVersion[apiVersion]
	if !ok {
		return fmt.Errorf("API version %d has no contract record; in %s add %s with a one-line summary and its SHA-256 digest in the filename", apiVersion, apiVersionRecordDir, path(apiVersion))
	}
	if record.digest == digest {
		return nil
	}
	if !used {
		return fmt.Errorf("generated OpenAPI contract changed under API version %d; replace this branch's record with %s in %s, with a one-line summary and its SHA-256 digest in the filename", apiVersion, path(apiVersion), apiVersionRecordDir)
	}
	next := apiVersion + 1
	return fmt.Errorf("generated OpenAPI contract changed under API version %d; in %s add %s with a one-line summary and its SHA-256 digest in the filename, set server.APIVersion to %d, and move any feature minimum this PR introduced to %d (see docs/agents/storage.md)", apiVersion, apiVersionRecordDir, path(next), next, next)
}

func TestAPIVersionContract(t *testing.T) {
	data, err := server.OpenAPIJSON(server.VersionInfo{}, pushBackendOptions()...)
	require.NoError(t, err)
	document, err := stripOpenAPIProse(jsontext.Value(data), false)
	require.NoError(t, err)
	digest, err := export.DigestCanonical(document)
	require.NoError(t, err)
	records, err := loadAPIVersionRecords(strings.TrimPrefix(apiVersionRecordDir, "cmd/agentsview/"))
	require.NoError(t, err)
	base, err := baseAPIVersionRecords(t.Context())
	require.NoError(t, err)
	require.NoError(t, checkAPIVersionContract(server.APIVersion, digest, records, base))
}

// Named maps contain contract names, including names that match prose fields.
func stripOpenAPIProse(value jsontext.Value, namedMap bool) (jsontext.Value, error) {
	switch value.Kind() {
	case '[':
		var items []jsontext.Value
		if err := json.Unmarshal(value, &items); err != nil {
			return nil, err
		}
		for i, item := range items {
			var err error
			items[i], err = stripOpenAPIProse(item, namedMap)
			if err != nil {
				return nil, err
			}
		}
		return json.Marshal(items)
	case '{':
		var fields map[string]jsontext.Value
		if err := json.Unmarshal(value, &fields); err != nil {
			return nil, err
		}
		for key, item := range fields {
			if !namedMap && (key == "title" || key == "description" || key == "summary" || key == "example" || key == "examples") {
				delete(fields, key)
				continue
			}
			if !namedMap && (key == "default" || key == "const" || key == "enum") {
				continue
			}
			childNamedMap := !namedMap && (key == "security" || (item.Kind() == '{' && (key == "paths" || key == "schemas" || key == "properties" || key == "patternProperties" || key == "$defs" || key == "dependentRequired" || key == "mapping" || key == "parameters" || key == "responses" || key == "headers" || key == "content" || key == "securitySchemes" || key == "requestBodies" || key == "links" || key == "callbacks" || key == "pathItems" || key == "encoding")))
			var err error
			fields[key], err = stripOpenAPIProse(item, childNamedMap)
			if err != nil {
				return nil, err
			}
		}
		return json.Marshal(fields)
	default:
		return value, nil
	}
}

func TestCheckAPIVersionContract(t *testing.T) {
	base := apiVersionRecord{15, "sha256:aaaa", "summary-a"}
	left := apiVersionRecord{16, "sha256:bbbb", "summary-b"}
	right := apiVersionRecord{16, "sha256:cccc", "summary-c"}
	merged := "sha256:dddd"

	t.Run("used version requires next version", func(t *testing.T) {
		err := checkAPIVersionContract(15, left.digest, []apiVersionRecord{base}, []string{base.name()})
		require.ErrorContains(t, err, "16-bbbb-<summary digest>")
		require.ErrorContains(t, err, "set server.APIVersion to 16")
		require.ErrorContains(t, err, "feature minimum")
	})
	t.Run("base record cannot be replaced or deleted", func(t *testing.T) {
		err := checkAPIVersionContract(15, left.digest, []apiVersionRecord{{15, left.digest, "summary-b"}}, []string{base.name()})
		require.ErrorContains(t, err, base.name()+" from the target base is missing; restore it")
		err = checkAPIVersionContract(15, base.digest, nil, []string{base.name()})
		require.ErrorContains(t, err, "restore it")
	})
	t.Run("branch record can be replaced at the same version", func(t *testing.T) {
		err := checkAPIVersionContract(16, merged, []apiVersionRecord{base, left}, []string{base.name()})
		require.ErrorContains(t, err, "replace this branch's record with 16-dddd-<summary digest>")
	})
	t.Run("collision and repair", func(t *testing.T) {
		err := checkAPIVersionContract(16, merged, []apiVersionRecord{base, left, right}, []string{base.name()})
		require.ErrorContains(t, err, left.name()+", "+right.name())
		require.ErrorContains(t, err, "rebase onto main")
		require.ErrorContains(t, err, "next free version is 17")
		err = checkAPIVersionContract(16, merged, []apiVersionRecord{base, left}, []string{base.name(), left.name()})
		require.ErrorContains(t, err, "17-dddd-<summary digest>")
		require.NoError(t, checkAPIVersionContract(17, merged, []apiVersionRecord{base, left, {17, merged, "summary-d"}}, []string{base.name(), left.name()}))
	})
	t.Run("same contract with different summaries", func(t *testing.T) {
		require.NoError(t, checkAPIVersionContract(16, left.digest, []apiVersionRecord{left, {16, left.digest, "other"}}, nil))
	})
	t.Run("missing current record", func(t *testing.T) {
		err := checkAPIVersionContract(16, left.digest, []apiVersionRecord{base}, nil)
		require.ErrorContains(t, err, "16-bbbb-<summary digest>")
		require.ErrorContains(t, err, "one-line summary")
	})
}

func TestLoadAPIVersionRecords(t *testing.T) {
	for _, tc := range []struct {
		name, content, hash string
		valid               bool
	}{
		{"valid", "Initial contract\n", summaryDigest("Initial contract"), true},
		{"empty", "", summaryDigest(""), false},
		{"multiline", "first\nsecond\n", summaryDigest("first\nsecond"), false},
		{"wrong digest", "Initial contract\n", summaryDigest("other"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			name := "15-" + strings.Repeat("a", 64) + "-" + tc.hash
			require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(tc.content), 0600))
			_, err := loadAPIVersionRecords(dir)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, name)
			}
		})
	}
}

func TestBaseAPIVersionRecords(t *testing.T) {
	t.Setenv("CI", "true")
	t.Setenv("API_VERSION_BASE", "")
	_, err := baseAPIVersionRecords(t.Context())
	require.ErrorContains(t, err, "API_VERSION_BASE is required in CI")

	t.Setenv("API_VERSION_BASE", "missing-api-version-base")
	_, err = baseAPIVersionRecords(t.Context())
	require.ErrorContains(t, err, "read API version records from base")
}
