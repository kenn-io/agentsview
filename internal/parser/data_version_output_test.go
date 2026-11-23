package parser_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"io/fs"
	"maps"
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
		if v.Type() == reflect.TypeFor[jsontext.Value]() && json.Valid(v.Bytes()) {
			decoder := json.NewDecoder(bytes.NewReader(v.Bytes()))
			decoder.UseNumber()
			var value any
			if err := decoder.Decode(&value); err == nil {
				return storedFields(reflect.ValueOf(value), root)
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
		if v.Type() == reflect.TypeFor[json.Number]() {
			return v.Interface()
		}
		if v.String() == filepath.Base(root) {
			return "$ROOT"
		}
		value := v.String()
		for _, prefix := range []string{root, strings.ReplaceAll(root, `\`, "/")} {
			if strings.HasPrefix(value, prefix+"/") || strings.HasPrefix(value, prefix+`\`) {
				return "$ROOT" + strings.ReplaceAll(value[len(prefix):], `\`, "/")
			}
			value = strings.ReplaceAll(value, prefix, "$ROOT")
		}
		return value
	default:
		return v.Interface()
	}
}

func parsedOutputRows(t *testing.T, agent parser.AgentType) map[string]string {
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
	rows := make(map[string][]string)
	var messages int
	for _, source := range sources {
		input, err := filepath.Rel(root, source.DisplayPath)
		require.NoError(t, err)
		input = filepath.ToSlash(input)
		outcome, err := provider.Parse(parser.WithoutFilesystemProjectDiscovery(t.Context()), parser.ParseRequest{Source: source, Machine: "digest"})
		require.NoError(t, err)
		require.Empty(t, outcome.SourceErrors)
		require.True(t, len(outcome.Results) > 0 || len(outcome.ExcludedSessionIDs) > 0, "%s: %s returned no sessions or exclusions", agent, input)
		slices.Sort(outcome.ExcludedSessionIDs)
		excluded, err := json.Marshal(outcome.ExcludedSessionIDs)
		require.NoError(t, err)
		rows[input] = append(rows[input], string(excluded))
		for _, result := range outcome.Results {
			messages += len(result.Result.Messages)
			candidate, err := ingest.PrepareCandidate(t.Context(), result.Result, ingest.ContentOptions{})
			require.NoError(t, err)
			prepared, err := ingest.FinalizeRows(t.Context(), candidate, ingest.ContentOptions{})
			require.NoError(t, err)
			data, err := json.Marshal(storedFields(reflect.ValueOf([]any{prepared.Session, prepared.Messages, prepared.UsageEvents, prepared.PlanUsage, prepared.SessionTotal}), normalRoot))
			require.NoError(t, err)
			rows[input] = append(rows[input], string(data))
		}
	}
	require.NotEmpty(t, rows, "%s returned no sessions", agent)
	require.Positive(t, messages, "%s returned no messages", agent)
	digests := make(map[string]string, len(rows))
	for input, values := range rows {
		slices.Sort(values)
		sum := sha256.Sum256([]byte(strings.Join(values, "\n")))
		digests[input] = hex.EncodeToString(sum[:])
	}
	return digests
}

type outputRecord struct {
	Version   int                          `json:"version"`
	Bump      bool                         `json:"bump,omitempty"`
	Digest    string                       `json:"digest"`
	Providers map[string]map[string]string `json:"providers"`
}

func outputDigest(providers map[string]map[string]string) (string, string, error) {
	data, err := json.Marshal(providers)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), string(data), nil
}

func TestParsedOutputMatchesDataVersion(t *testing.T) {
	local := time.Local                      //nolint:forbidigo // Pin local parser timestamps for a stable digest.
	time.Local = time.UTC                    //nolint:forbidigo // Pin local parser timestamps for a stable digest.
	t.Cleanup(func() { time.Local = local }) //nolint:forbidigo // Restore the test process timezone.
	providers := make(map[string]map[string]string)
	for _, factory := range parser.ProviderFactories() {
		agent := factory.Definition().Type
		if parser.ProviderMigrationModes()[agent] == parser.ProviderMigrationImportOnly {
			continue
		}
		t.Run(string(agent), func(t *testing.T) {
			first := parsedOutputRows(t, agent)
			second := parsedOutputRows(t, agent)
			require.Equal(t, first, second, "%s output varies across temporary roots", agent)
			providers[string(agent)] = first
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
		digest, _, err := outputDigest(record.Providers)
		if err != nil {
			return nil, err
		}
		if record.Digest != digest || entry.Name() != fmt.Sprintf("%d-%s.json", record.Version, record.Digest) {
			return nil, fmt.Errorf("invalid data version output record %s", entry.Name())
		}
		records = append(records, record)
	}
	return records, nil
}

func checkDataVersionDigest(version int, providers map[string]map[string]string, records []outputRecord) error {
	byVersion := make(map[int]map[string]map[string]string)
	bumps := 0
	for _, record := range records {
		if record.Version > version {
			return fmt.Errorf("data version %d has a newer record for version %d", version, record.Version)
		}
		if record.Version == version && record.Bump {
			bumps++
			if bumps > 1 {
				return fmt.Errorf("data version %d has multiple bump records; keep the record from main, remove yours, and take version %d", version, version+1)
			}
		}
		if byVersion[record.Version] == nil {
			byVersion[record.Version] = make(map[string]map[string]string)
		}
		for _, provider := range slices.Sorted(maps.Keys(record.Providers)) {
			inputs := record.Providers[provider]
			if byVersion[record.Version][provider] == nil {
				byVersion[record.Version][provider] = make(map[string]string)
			}
			for _, input := range slices.Sorted(maps.Keys(inputs)) {
				digest := inputs[input]
				if previous, ok := byVersion[record.Version][provider][input]; ok && previous != digest {
					return fmt.Errorf("data version %d has conflicting output records for %s: %s; keep the record from main, remove your unmerged conflicting record, and take version %d", record.Version, provider, input, version+1)
				}
				byVersion[record.Version][provider][input] = digest
			}
		}
	}
	digest, providerJSON, err := outputDigest(providers)
	if err != nil {
		return err
	}
	current, ok := byVersion[version]
	if !ok {
		return fmt.Errorf("record data version %d with digest %s and provider digests %s", version, digest, providerJSON)
	}
	for _, provider := range slices.Sorted(maps.Keys(current)) {
		inputs := current[provider]
		for _, input := range slices.Sorted(maps.Keys(inputs)) {
			recorded := inputs[input]
			got, exists := providers[provider][input]
			if !exists {
				return fmt.Errorf("recorded input missing for %s: %s under data version %d", provider, input, version)
			}
			if got != recorded {
				return fmt.Errorf("parsed output changed for %s: %s under data version %d; take version %d, declare its scope, and record digest %s with provider digests %s", provider, input, version, version+1, digest, providerJSON)
			}
		}
	}
	missing := make(map[string]map[string]string)
	for provider, inputs := range providers {
		for input, digest := range inputs {
			if _, ok := current[provider][input]; !ok {
				if missing[provider] == nil {
					missing[provider] = make(map[string]string)
				}
				missing[provider][input] = digest
			}
		}
	}
	if len(missing) > 0 {
		digest, data, err := outputDigest(missing)
		if err != nil {
			return err
		}
		return fmt.Errorf("record new inputs at data version %d with digest %s and provider digests %s", version, digest, data)
	}
	return nil
}

func TestDataVersionDigestGuidance(t *testing.T) {
	old := outputRecord{Version: 177, Bump: true, Providers: map[string]map[string]string{"claude": {"old.jsonl": "old"}}}
	added := outputRecord{Version: 177, Providers: map[string]map[string]string{"claude": {"new.jsonl": "first"}, "example": {"session.jsonl": "first"}}}
	otherBump := outputRecord{Version: 177, Bump: true, Providers: added.Providers}
	err := checkDataVersionDigest(177, nil, []outputRecord{old, otherBump})
	require.ErrorContains(t, err, "multiple bump records")
	require.ErrorContains(t, err, "take version 178")
	both := map[string]map[string]string{"claude": {"old.jsonl": "old", "new.jsonl": "first"}, "example": {"session.jsonl": "first"}}
	require.NoError(t, checkDataVersionDigest(177, both, []outputRecord{old, added}))
	require.ErrorContains(t, checkDataVersionDigest(177, both, []outputRecord{old}), "record new inputs")
	changed := map[string]map[string]string{"claude": {"old.jsonl": "old", "new.jsonl": "changed"}, "example": {"session.jsonl": "first"}}
	err = checkDataVersionDigest(177, changed, []outputRecord{old, added})
	require.ErrorContains(t, err, "claude: new.jsonl")
	require.ErrorContains(t, err, "take version 178")
	changed["claude"]["new.jsonl"] = "first"
	changed["claude"]["old.jsonl"] = "changed"
	err = checkDataVersionDigest(177, changed, []outputRecord{old, added})
	require.ErrorContains(t, err, "claude: old.jsonl")
	conflict := outputRecord{Version: 177, Providers: map[string]map[string]string{"claude": {"old.jsonl": "changed"}}}
	err = checkDataVersionDigest(177, both, []outputRecord{old, added, conflict})
	require.ErrorContains(t, err, "conflicting output records for claude: old.jsonl")
	require.ErrorContains(t, err, "take version 178")
	require.ErrorContains(t, checkDataVersionDigest(177, map[string]map[string]string{}, []outputRecord{old}), "recorded input missing for claude: old.jsonl")
	require.ErrorContains(t, checkDataVersionDigest(176, both, []outputRecord{old}), "newer record for version 177")
}

func TestStoredFieldsNormalizesPaths(t *testing.T) {
	root := `C:\tmp\r`
	value := map[string]any{"path": `C:\tmp\r\a\b.jsonl`, "text": `keep C:\other\file`, "json": jsontext.Value(`{"path":"C:\\tmp\\r\\a\\b.jsonl","text":"C:\\other\\file","count":1}`)} //nolint:misspell // This is a Windows path.
	got := storedFields(reflect.ValueOf(value), root).(map[string]any)
	require.Equal(t, "$ROOT/a/b.jsonl", got["path"])
	require.Equal(t, `keep C:\other\file`, got["text"])                                                                          //nolint:misspell // This is a Windows path.
	require.Equal(t, map[string]any{"path": "$ROOT/a/b.jsonl", "text": `C:\other\file`, "count": json.Number("1")}, got["json"]) //nolint:misspell // This is a Windows path.
	require.Equal(t, "$ROOT/a/b.jsonl", storedFields(reflect.ValueOf("/tmp/fixture/a/b.jsonl"), "/tmp/fixture"))
	require.Equal(t, "read $ROOT/a then continue", storedFields(reflect.ValueOf("read /tmp/fixture/a then continue"), "/tmp/fixture"))
	require.Equal(t, "$ROOT", storedFields(reflect.ValueOf("fixture"), "/tmp/fixture"))
}
