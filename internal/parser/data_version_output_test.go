package parser_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/parser"
)

// storedFields includes exported storage fields even when the API omits them.
func storedFields(v reflect.Value, root string) any {
	if !v.IsValid() {
		return nil
	}
	switch v.Kind() {
	case reflect.Interface, reflect.Pointer:
		if v.IsNil() {
			return nil
		}
		return storedFields(v.Elem(), root)
	case reflect.Struct:
		fields := make(map[string]any)
		for i := range v.NumField() {
			field := v.Type().Field(i)
			if !field.IsExported() || field.Name == "FileMtime" || field.Name == "FileInode" || field.Name == "FileDevice" || field.Name == "FileHash" {
				continue
			}
			fields[field.Name] = storedFields(v.Field(i), root)
		}
		return fields
	case reflect.Slice, reflect.Array:
		if v.Type() == reflect.TypeOf(jsontext.Value{}) && json.Valid(v.Bytes()) {
			decoder := json.NewDecoder(bytes.NewReader(v.Bytes()))
			decoder.UseNumber()
			var value any
			if err := decoder.Decode(&value); err == nil {
				return value
			}
		}
		if v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.Uint8 {
			return hex.EncodeToString(v.Bytes())
		}
		values := make([]any, v.Len())
		for i := range v.Len() {
			values[i] = storedFields(v.Index(i), root)
		}
		return values
	case reflect.Map:
		values := make(map[string]any, v.Len())
		iter := v.MapRange()
		for iter.Next() {
			values[fmt.Sprint(iter.Key().Interface())] = storedFields(iter.Value(), root)
		}
		return values
	case reflect.String:
		if v.String() == filepath.Base(root) {
			return "$ROOT"
		}
		return strings.ReplaceAll(filepath.ToSlash(v.String()), filepath.ToSlash(root), "$ROOT")
	default:
		return v.Interface()
	}
}

func parsedOutputRows(t *testing.T, agent parser.AgentType) []string {
	t.Helper()
	root := parsedOutputFixture(t, agent)
	root, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	normalRoot := root
	if agent == parser.AgentCrush {
		normalRoot = filepath.Dir(root)
	}
	stamp := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink == 0 {
			return os.Chtimes(path, stamp, stamp)
		}
		return nil
	}))
	provider, ok := parser.NewProvider(agent, parser.ProviderConfig{
		Roots: []string{root}, Machine: "digest",
		PathRewriter: func(path string) string {
			return strings.ReplaceAll(filepath.ToSlash(path), filepath.ToSlash(normalRoot), "/digest")
		},
	})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, sources, "%s", agent)
	var rows []string
	var messages int
	for _, source := range sources {
		outcome, err := provider.Parse(parser.WithoutFilesystemProjectDiscovery(t.Context()), parser.ParseRequest{Source: source, Machine: "digest"})
		require.NoError(t, err)
		require.Empty(t, outcome.SourceErrors)
		for _, result := range outcome.Results {
			messages += len(result.Result.Messages)
			candidate, err := ingest.PrepareCandidate(t.Context(), result.Result, ingest.ContentOptions{})
			require.NoError(t, err)
			prepared, err := ingest.FinalizeRows(t.Context(), candidate, ingest.ContentOptions{})
			require.NoError(t, err)
			data, err := json.Marshal(storedFields(reflect.ValueOf([]any{prepared.Session, prepared.Messages, prepared.UsageEvents, prepared.PlanUsage}), normalRoot))
			require.NoError(t, err)
			rows = append(rows, string(data))
		}
	}
	require.NotEmpty(t, rows, "%s returned no sessions", agent)
	require.Positive(t, messages, "%s returned no messages", agent)
	slices.Sort(rows)
	return rows
}

type outputRecord struct {
	Version   int               `json:"version"`
	Digest    string            `json:"digest"`
	Providers map[string]string `json:"providers"`
}

func outputDigest(providers map[string]string) string {
	data, _ := json.Marshal(providers)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestParsedOutputMatchesDataVersion(t *testing.T) {
	providers := make(map[string]string)
	for _, factory := range parser.ProviderFactories() {
		agent := factory.Definition().Type
		if parser.ProviderMigrationModes()[agent] == parser.ProviderMigrationImportOnly {
			continue
		}
		t.Run(string(agent), func(t *testing.T) {
			first := parsedOutputRows(t, agent)
			second := parsedOutputRows(t, agent)
			require.Equal(t, first, second, "%s output varies across temporary roots", agent)
			sum := sha256.Sum256([]byte(strings.Join(first, "\n")))
			providers[string(agent)] = hex.EncodeToString(sum[:])
		})
	}
	if t.Failed() {
		return
	}
	records, err := readOutputRecords("testdata/data-version-output-digests")
	require.NoError(t, err)
	require.NoError(t, checkDataVersionDigest(db.CurrentDataVersion(), providers, records))
}

func readOutputRecords(dir string) ([]outputRecord, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var records []outputRecord
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		var record outputRecord
		if err := json.Unmarshal(data, &record); err != nil {
			return nil, err
		}
		if record.Digest != outputDigest(record.Providers) || entry.Name() != fmt.Sprintf("%d-%s.json", record.Version, record.Digest) {
			return nil, fmt.Errorf("invalid data version output record %s", entry.Name())
		}
		records = append(records, record)
	}
	return records, nil
}

func checkDataVersionDigest(version int, providers map[string]string, records []outputRecord) error {
	byVersion := make(map[int]map[string]string)
	conflicts := make(map[int]map[string]bool)
	for _, record := range records {
		if record.Version > version {
			return fmt.Errorf("data version %d has a newer record for version %d", version, record.Version)
		}
		if byVersion[record.Version] == nil {
			byVersion[record.Version] = make(map[string]string)
		}
		for name, digest := range record.Providers {
			if previous, ok := byVersion[record.Version][name]; ok && previous != digest {
				if conflicts[record.Version] == nil {
					conflicts[record.Version] = make(map[string]bool)
				}
				conflicts[record.Version][name] = true
			}
			byVersion[record.Version][name] = digest
		}
	}
	for v, changed := range conflicts {
		names := make([]string, 0, len(changed))
		for name := range changed {
			names = append(names, name)
		}
		slices.Sort(names)
		return fmt.Errorf("data version %d has conflicting output records for %s; keep the record from main, remove your unmerged conflicting record, and take version %d", v, strings.Join(names, ", "), version+1)
	}
	current, ok := byVersion[version]
	if !ok {
		return fmt.Errorf("record data version %d with digest %s and provider digests %s", version, outputDigest(providers), providerJSON(providers))
	}
	changed := make([]string, 0)
	for name, digest := range current {
		if providers[name] != digest {
			changed = append(changed, name)
		}
	}
	if len(changed) > 0 {
		slices.Sort(changed)
		return fmt.Errorf("parsed output changed for %s under data version %d; take version %d, declare its scope, and record digest %s with provider digests %s", strings.Join(changed, ", "), version, version+1, outputDigest(providers), providerJSON(providers))
	}
	missing := make(map[string]string)
	for name, digest := range providers {
		if _, ok := current[name]; !ok {
			missing[name] = digest
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("record new providers at data version %d with digest %s and provider digests %s", version, outputDigest(missing), providerJSON(missing))
	}
	return nil
}

func providerJSON(providers map[string]string) string {
	data, _ := json.Marshal(providers)
	return string(data)
}

func TestDataVersionDigestGuidance(t *testing.T) {
	claude := outputRecord{Version: 177, Providers: map[string]string{"claude": "old"}}
	added := outputRecord{Version: 177, Providers: map[string]string{"example": "first"}}
	both := map[string]string{"claude": "old", "example": "first"}
	require.NoError(t, checkDataVersionDigest(177, both, []outputRecord{claude, added}))
	require.ErrorContains(t, checkDataVersionDigest(177, both, []outputRecord{claude}), "record new providers")
	err := checkDataVersionDigest(177, map[string]string{"claude": "old", "example": "changed"}, []outputRecord{claude, added})
	require.ErrorContains(t, err, "example")
	require.ErrorContains(t, err, "take version 178")
	err = checkDataVersionDigest(177, map[string]string{"claude": "changed", "example": "first"}, []outputRecord{claude, added})
	require.ErrorContains(t, err, "claude")
	require.ErrorContains(t, err, "take version 178")
	conflict := outputRecord{Version: 177, Providers: map[string]string{"claude": "changed"}}
	err = checkDataVersionDigest(177, both, []outputRecord{claude, added, conflict})
	require.ErrorContains(t, err, "conflicting output records for claude")
	require.ErrorContains(t, err, "take version 178")
	require.NoError(t, checkDataVersionDigest(177, both, []outputRecord{claude, added}))
	require.ErrorContains(t, checkDataVersionDigest(176, both, []outputRecord{claude}), "newer record for version 177")
}
