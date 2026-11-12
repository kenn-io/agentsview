package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/server"
)

// apiVersionRecordDir holds one empty <version>-<digest hex> file per API
// version and generated OpenAPI contract; the name is the whole record.
// Identical contracts produce the same file, so two branches recording them
// merge cleanly; different contracts under one version add separate files
// that this test rejects.
const apiVersionRecordDir = "cmd/agentsview/testdata/api_versions"

type apiVersionRecord struct {
	version int
	digest  string
}

func (r apiVersionRecord) name() string {
	return fmt.Sprintf("%d-%s", r.version, strings.TrimPrefix(r.digest, "sha256:"))
}

func loadAPIVersionRecords(t *testing.T) []apiVersionRecord {
	t.Helper()
	entries, err := os.ReadDir(strings.TrimPrefix(apiVersionRecordDir, "cmd/agentsview/"))
	require.NoError(t, err)
	records := make([]apiVersionRecord, 0, len(entries))
	for _, e := range entries {
		versionText, hex, ok := strings.Cut(e.Name(), "-")
		version, err := strconv.Atoi(versionText)
		require.True(t, ok && err == nil && hex != "",
			"API version record %s must be named <version>-<digest hex>", e.Name())
		records = append(records, apiVersionRecord{version: version, digest: "sha256:" + hex})
	}
	return records
}

// checkAPIVersionContract fails when one API version has more than one
// contract digest, when apiVersion is below the highest recorded version, or
// when the generated contract differs from the record for apiVersion. Every
// failure that needs a new version names the next free one.
func checkAPIVersionContract(apiVersion int, digest string, records []apiVersionRecord) error {
	byVersion := map[int]apiVersionRecord{}
	var conflict, first apiVersionRecord
	highest := 0
	for _, r := range records {
		if previous, ok := byVersion[r.version]; ok && previous.digest != r.digest {
			first, conflict = previous, r
		} else {
			byVersion[r.version] = r
		}
		highest = max(highest, r.version)
	}
	next := max(apiVersion, highest) + 1
	if conflict.digest != "" {
		return fmt.Errorf("API version %d has different contract digests (%s, %s); "+
			"delete the file this PR added, rebase onto main after the earlier PR merges, "+
			"and rerun make test-api-version. The next free version is %d",
			conflict.version, first.name(), conflict.name(), next)
	}
	move := fmt.Sprintf("this PR's contract (%s) needs API version %d: in %s add an empty "+
		"file %s, set server.APIVersion to %d, and move any feature minimum this PR "+
		"introduced to %d (see docs/agents/storage.md)",
		digest, next, apiVersionRecordDir, apiVersionRecord{next, digest}.name(), next, next)

	if apiVersion < highest {
		return fmt.Errorf("server.APIVersion %d is below recorded API version %d; %s",
			apiVersion, highest, move)
	}
	if byVersion[apiVersion].digest == digest {
		return nil
	}
	if apiVersion > highest {
		return fmt.Errorf("API version %d has no contract record; in %s add an empty file %s",
			apiVersion, apiVersionRecordDir, apiVersionRecord{apiVersion, digest}.name())
	}
	return fmt.Errorf("generated OpenAPI contract changed under API version %d; %s",
		apiVersion, move)
}

func TestAPIVersionContract(t *testing.T) {
	data, err := server.OpenAPIJSON(server.VersionInfo{}, pushBackendOptions()...)
	require.NoError(t, err)
	document, err := stripOpenAPIProse(jsontext.Value(data), false)
	require.NoError(t, err)
	digest, err := export.DigestCanonical(document)
	require.NoError(t, err)
	require.NoError(t, checkAPIVersionContract(server.APIVersion, digest, loadAPIVersionRecords(t)))
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
	}
	return value, nil
}

func TestCheckAPIVersionContract(t *testing.T) {
	const (
		base   = "sha256:aaaa"
		left   = "sha256:bbbb"
		right  = "sha256:cccc"
		merged = "sha256:dddd"
	)
	rec := func(version int, digest string) apiVersionRecord {
		return apiVersionRecord{version: version, digest: digest}
	}

	t.Run("changed contract under same version names next", func(t *testing.T) {
		err := checkAPIVersionContract(15, left, []apiVersionRecord{rec(15, base)})
		require.ErrorContains(t, err, "changed under API version 15")
		assert.ErrorContains(t, err, "add an empty file 16-bbbb, set server.APIVersion to 16")
		assert.ErrorContains(t, err, "feature minimum")
	})

	t.Run("divergent version 16 contracts name both records and 17, then repair passes", func(t *testing.T) {
		// The queue merges both PRs, so the generated contract carries both
		// changes and matches neither version 16 record.
		records := []apiVersionRecord{rec(15, base), rec(16, left), rec(16, right)}
		err := checkAPIVersionContract(16, merged, records)
		require.ErrorContains(t, err, "API version 16 has different contract digests (16-bbbb, 16-cccc)")
		assert.ErrorContains(t, err, "delete the file this PR added, rebase onto main after the earlier PR merges")
		assert.ErrorContains(t, err, "rerun make test-api-version. The next free version is 17")

		repaired := []apiVersionRecord{rec(15, base), rec(16, left), rec(17, merged)}
		assert.NoError(t, checkAPIVersionContract(17, merged, repaired))
	})

	t.Run("older version fails once a newer one is recorded", func(t *testing.T) {
		err := checkAPIVersionContract(15, base, []apiVersionRecord{rec(15, base), rec(16, left)})
		require.ErrorContains(t, err, "server.APIVersion 15 is below recorded API version 16")
		assert.ErrorContains(t, err, "needs API version 17")
	})

	t.Run("current version without record asks for one", func(t *testing.T) {
		err := checkAPIVersionContract(16, left, []apiVersionRecord{rec(15, base)})
		assert.ErrorContains(t, err, "API version 16 has no contract record; in "+apiVersionRecordDir+" add an empty file 16-bbbb")
	})
}
