package main

import (
	"encoding/json/jsontext"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/server"
)

// apiVersionRecordDir holds one <version>-<digest hex>.json file per API
// version and generated OpenAPI contract. Identical contracts produce the same
// file, so two branches recording them merge cleanly; different contracts under
// one version add separate files that this test rejects.
const apiVersionRecordDir = "testdata/api_versions"

type apiVersionRecord struct {
	version int
	digest  string
	path    string
}

func generatedAPIContractDigest(t *testing.T) string {
	t.Helper()
	data, err := server.OpenAPISpec(server.VersionInfo{}, pushBackendOptions()...).MarshalJSON()
	require.NoError(t, err)
	digest, err := export.DigestCanonical(jsontext.Value(data))
	require.NoError(t, err)
	return digest
}

func apiVersionRecordPath(version int, digest string) string {
	return filepath.Join(apiVersionRecordDir,
		fmt.Sprintf("%d-%s.json", version, strings.TrimPrefix(digest, "sha256:")))
}

func loadAPIVersionRecords(t *testing.T, dir string) []apiVersionRecord {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	require.NoError(t, err)
	records := make([]apiVersionRecord, 0, len(paths))
	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), ".json")
		versionText, hex, ok := strings.Cut(name, "-")
		version, err := strconv.Atoi(versionText)
		require.True(t, ok && err == nil && hex != "",
			"API version record %s must be named <version>-<digest hex>.json", path)
		records = append(records, apiVersionRecord{
			version: version, digest: "sha256:" + hex, path: path,
		})
	}
	return records
}

// checkAPIVersionContract fails when one API version has more than one
// contract digest, or when the generated contract differs from the record for
// apiVersion. Every failure names the next free version.
func checkAPIVersionContract(apiVersion int, digest string, records []apiVersionRecord) error {
	digests := map[int]map[string]string{}
	highest := 0
	for _, r := range records {
		if digests[r.version] == nil {
			digests[r.version] = map[string]string{}
		}
		digests[r.version][r.digest] = r.path
		highest = max(highest, r.version)
	}
	next := max(apiVersion, highest) + 1
	move := func(old string) string {
		return fmt.Sprintf("this PR's contract (%s) needs API version %d: %s"+
			"add %s containing {\"api_version\":%d,\"openapi_digest\":%q}, "+
			"set server.APIVersion to %d, and move any feature minimum this PR "+
			"introduced to %d (see docs/agents/storage.md)",
			digest, next, old, apiVersionRecordPath(next, digest), next, digest, next, next)
	}

	versions := make([]int, 0, len(digests))
	for v := range digests {
		versions = append(versions, v)
	}
	slices.Sort(versions)
	for _, v := range versions {
		if len(digests[v]) < 2 {
			continue
		}
		old := fmt.Sprintf("delete the API version %d record matching this PR's contract, ", v)
		if path, ok := digests[v][digest]; ok {
			old = fmt.Sprintf("delete %s, ", path)
		}
		return fmt.Errorf("API version %d has %d different contract digests; %s",
			v, len(digests[v]), move(old))
	}

	if _, ok := digests[apiVersion][digest]; ok {
		return nil
	}
	for _, v := range versions {
		if _, ok := digests[v][digest]; ok && v > apiVersion {
			return fmt.Errorf("generated contract %s is recorded as API version %d; "+
				"set server.APIVersion to %d", digest, v, v)
		}
	}
	if apiVersion > highest {
		return fmt.Errorf("API version %d has no contract record; add %s containing "+
			"{\"api_version\":%d,\"openapi_digest\":%q}",
			apiVersion, apiVersionRecordPath(apiVersion, digest), apiVersion, digest)
	}
	return fmt.Errorf("generated OpenAPI contract changed under API version %d; %s",
		apiVersion, move(""))
}

func TestAPIVersionContract(t *testing.T) {
	digest := generatedAPIContractDigest(t)
	require.Equal(t, digest, generatedAPIContractDigest(t), "contract digest must be stable")
	records := loadAPIVersionRecords(t, apiVersionRecordDir)
	require.NoError(t, checkAPIVersionContract(server.APIVersion, digest, records))
}

func TestCheckAPIVersionContract(t *testing.T) {
	const (
		base  = "sha256:aaaa"
		left  = "sha256:bbbb"
		right = "sha256:cccc"
	)
	rec := func(version int, digest string) apiVersionRecord {
		return apiVersionRecord{
			version: version, digest: digest, path: apiVersionRecordPath(version, digest),
		}
	}

	t.Run("matching baseline passes", func(t *testing.T) {
		assert.NoError(t, checkAPIVersionContract(15, base, []apiVersionRecord{rec(15, base)}))
	})

	t.Run("changed contract under same version names next", func(t *testing.T) {
		err := checkAPIVersionContract(15, left, []apiVersionRecord{rec(15, base)})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "changed under API version 15")
		assert.Contains(t, err.Error(), "needs API version 16")
		assert.Contains(t, err.Error(), apiVersionRecordPath(16, left))
		assert.Contains(t, err.Error(), "feature minimum")
	})

	t.Run("two version 16 contracts name 17", func(t *testing.T) {
		records := []apiVersionRecord{rec(15, base), rec(16, left), rec(16, right)}
		for _, apiVersion := range []int{16, 15} {
			err := checkAPIVersionContract(apiVersion, right, records)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "API version 16 has 2 different contract digests")
			assert.Contains(t, err.Error(), "delete "+apiVersionRecordPath(16, right))
			assert.Contains(t, err.Error(), apiVersionRecordPath(17, right))
			assert.Contains(t, err.Error(), "set server.APIVersion to 17")
		}
	})

	t.Run("equal version 16 contracts pass", func(t *testing.T) {
		records := []apiVersionRecord{rec(15, base), rec(16, left), rec(16, left)}
		assert.NoError(t, checkAPIVersionContract(16, left, records))
	})

	t.Run("current version without record asks for one", func(t *testing.T) {
		err := checkAPIVersionContract(16, left, []apiVersionRecord{rec(15, base)})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "API version 16 has no contract record")
		assert.Contains(t, err.Error(), apiVersionRecordPath(16, left))
	})

	t.Run("stale constant matching higher record", func(t *testing.T) {
		err := checkAPIVersionContract(15, left, []apiVersionRecord{rec(15, base), rec(16, left)})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "set server.APIVersion to 16")
	})

	t.Run("stale constant with changed contract names next after highest", func(t *testing.T) {
		err := checkAPIVersionContract(15, right, []apiVersionRecord{rec(15, base), rec(16, left)})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "needs API version 17")
	})
}
