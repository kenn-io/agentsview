package rawsync

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func TestNewAuthIdentityValidatesOpaqueIDs(t *testing.T) {
	t.Parallel()

	identity, err := NewAuthIdentity("tenant-a", "device-a")
	require.NoError(t, err)
	assert.Equal(t, AuthIdentity{TenantID: "tenant-a", DeviceID: "device-a"}, identity)

	for _, tc := range []struct {
		name   string
		tenant string
		device string
	}{
		{name: "missing tenant", device: "device-a"},
		{name: "missing device", tenant: "tenant-a"},
		{name: "leading whitespace", tenant: " tenant-a", device: "device-a"},
		{name: "trailing whitespace", tenant: "tenant-a", device: "device-a "},
		{name: "control character", tenant: "tenant-a", device: "bad\nvalue"},
		{name: "forward slash", tenant: "tenant/a", device: "device-a"},
		{name: "backslash", tenant: "tenant-a", device: `device\a`},
		{name: "oversized", tenant: strings.Repeat("x", 129), device: "device-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := NewAuthIdentity(tc.tenant, tc.device)
			assert.ErrorIs(t, err, ErrInvalid)
		})
	}
}

func TestNewObjectRefRequiresCanonicalSHA256AndNonNegativeLength(t *testing.T) {
	t.Parallel()

	ref, err := NewObjectRef(strings.Repeat("a", 64), 12)
	require.NoError(t, err)
	assert.Equal(t, ObjectRef{SHA256: strings.Repeat("a", 64), Length: 12}, ref)

	empty, err := NewObjectRef(strings.Repeat("b", 64), 0)
	require.NoError(t, err)
	assert.Zero(t, empty.Length)

	for _, tc := range []struct {
		name   string
		hash   string
		length int64
	}{
		{name: "short hash", hash: "abcd", length: 1},
		{name: "uppercase hash", hash: strings.Repeat("A", 64), length: 1},
		{name: "non-hex hash", hash: strings.Repeat("g", 64), length: 1},
		{name: "negative length", hash: strings.Repeat("a", 64), length: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := NewObjectRef(tc.hash, tc.length)
			assert.ErrorIs(t, err, ErrInvalid)
		})
	}
}

func TestValidateAndCanonicalizeProducesAuthenticatedStableEnvelope(t *testing.T) {
	t.Parallel()

	manifest := validManifest()
	identity, err := NewAuthIdentity("tenant-a", "device-a")
	require.NoError(t, err)

	got, err := ValidateAndCanonicalize(identity, manifest, DefaultManifestLimits())
	require.NoError(t, err)

	wantJSON := `{"schema_version":1,"tenant_id":"tenant-a","device_id":"device-a","provider":"codex","configured_root_id":"root-a","source_key":"sessions/demo.jsonl#main","capture_id":"capture-a","captured_at":"2026-08-13T12:34:56Z","kind":"snapshot","entries":[{"path":"a.jsonl","type":"file","length":3,"objects":[{"sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","length":3}]},{"path":"z.jsonl","type":"file","length":8,"objects":[{"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","length":4},{"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","length":4}]}]}` + "\n"
	assert.Equal(t, wantJSON, string(got.CanonicalJSON))
	wantSum := sha256.Sum256([]byte(wantJSON))
	assert.Equal(t, hex.EncodeToString(wantSum[:]), got.ManifestID)
	assert.Equal(t, []string{"a.jsonl", "z.jsonl"}, []string{
		got.Manifest.Entries[0].Path,
		got.Manifest.Entries[1].Path,
	})
	assert.Equal(t, []ObjectRef{
		{SHA256: strings.Repeat("a", 64), Length: 4},
		{SHA256: strings.Repeat("b", 64), Length: 3},
	}, got.Objects)
	assert.Len(t, got.Manifest.Entries[1].Objects, 2,
		"repeated chunks must remain in reconstruction order")
	assert.Equal(t, "z.jsonl", manifest.Entries[0].Path,
		"canonicalization must not mutate the caller's manifest")
}

func TestValidateAndCanonicalizeBindsAuthenticatedIdentity(t *testing.T) {
	t.Parallel()

	manifest := validManifest()
	a, err := NewAuthIdentity("tenant-a", "device-a")
	require.NoError(t, err)
	b, err := NewAuthIdentity("tenant-b", "device-a")
	require.NoError(t, err)

	first, err := ValidateAndCanonicalize(a, manifest, DefaultManifestLimits())
	require.NoError(t, err)
	again, err := ValidateAndCanonicalize(a, manifest, DefaultManifestLimits())
	require.NoError(t, err)
	otherTenant, err := ValidateAndCanonicalize(b, manifest, DefaultManifestLimits())
	require.NoError(t, err)

	assert.Equal(t, first.ManifestID, again.ManifestID)
	assert.Equal(t, first.CanonicalJSON, again.CanonicalJSON)
	assert.NotEqual(t, first.ManifestID, otherTenant.ManifestID)
}

func TestValidateAndCanonicalizeNormalizesCapturedInstantToUTC(t *testing.T) {
	t.Parallel()

	identity, err := NewAuthIdentity("tenant-a", "device-a")
	require.NoError(t, err)
	utc := validManifest()
	offset := cloneManifest(utc)
	offset.CapturedAt = utc.CapturedAt.In(time.FixedZone("offset", 2*60*60))

	first, err := ValidateAndCanonicalize(identity, utc, DefaultManifestLimits())
	require.NoError(t, err)
	second, err := ValidateAndCanonicalize(identity, offset, DefaultManifestLimits())
	require.NoError(t, err)

	assert.Equal(t, first.ManifestID, second.ManifestID)
	assert.Equal(t, first.CanonicalJSON, second.CanonicalJSON)
}

func TestValidateAndCanonicalizeRejectsMalformedManifest(t *testing.T) {
	t.Parallel()

	identity, err := NewAuthIdentity("tenant-a", "device-a")
	require.NoError(t, err)
	valid := validManifest()

	for _, tc := range []struct {
		name   string
		mutate func(*Manifest, *ManifestLimits)
	}{
		{name: "unsupported schema", mutate: func(m *Manifest, _ *ManifestLimits) { m.SchemaVersion = 2 }},
		{name: "missing provider", mutate: func(m *Manifest, _ *ManifestLimits) { m.Provider = "" }},
		{name: "unknown provider", mutate: func(m *Manifest, _ *ManifestLimits) { m.Provider = "not-an-agent" }},
		{name: "remote sync excluded provider", mutate: func(m *Manifest, _ *ManifestLimits) { m.Provider = parser.AgentOmnigent }},
		{name: "missing root", mutate: func(m *Manifest, _ *ManifestLimits) { m.ConfiguredRootID = "" }},
		{name: "missing source", mutate: func(m *Manifest, _ *ManifestLimits) { m.SourceKey = "" }},
		{name: "source control", mutate: func(m *Manifest, _ *ManifestLimits) { m.SourceKey = "bad\nsource" }},
		{name: "missing capture", mutate: func(m *Manifest, _ *ManifestLimits) { m.CaptureID = "" }},
		{name: "short parent receipt", mutate: func(m *Manifest, _ *ManifestLimits) { m.ExpectedParentReceipt = "abcd" }},
		{name: "uppercase parent receipt", mutate: func(m *Manifest, _ *ManifestLimits) { m.ExpectedParentReceipt = strings.Repeat("A", 64) }},
		{name: "zero captured time", mutate: func(m *Manifest, _ *ManifestLimits) { m.CapturedAt = time.Time{} }},
		{name: "absolute path", mutate: func(m *Manifest, _ *ManifestLimits) { m.Entries[0].Path = "/escape" }},
		{name: "parent path", mutate: func(m *Manifest, _ *ManifestLimits) { m.Entries[0].Path = "../escape" }},
		{name: "backslash path", mutate: func(m *Manifest, _ *ManifestLimits) { m.Entries[0].Path = `dir\file` }},
		{name: "alternate data path", mutate: func(m *Manifest, _ *ManifestLimits) { m.Entries[0].Path = "session.jsonl:stream" }},
		{name: "drive relative path", mutate: func(m *Manifest, _ *ManifestLimits) { m.Entries[0].Path = "C:session.jsonl" }},
		{name: "windows device path", mutate: func(m *Manifest, _ *ManifestLimits) { m.Entries[0].Path = "dir/CON.jsonl" }},
		{name: "windows trailing dot", mutate: func(m *Manifest, _ *ManifestLimits) { m.Entries[0].Path = "session.jsonl." }},
		{name: "windows trailing space", mutate: func(m *Manifest, _ *ManifestLimits) { m.Entries[0].Path = "session.jsonl " }},
		{name: "duplicate path", mutate: func(m *Manifest, _ *ManifestLimits) { m.Entries[1].Path = m.Entries[0].Path }},
		{name: "unsupported entry type", mutate: func(m *Manifest, _ *ManifestLimits) { m.Entries[0].Type = "directory" }},
		{name: "empty object list", mutate: func(m *Manifest, _ *ManifestLimits) { m.Entries[0].Objects = nil }},
		{name: "object sum mismatch", mutate: func(m *Manifest, _ *ManifestLimits) { m.Entries[0].Length++ }},
		{name: "invalid embedded object", mutate: func(m *Manifest, _ *ManifestLimits) { m.Entries[0].Objects[0].SHA256 = "bad" }},
		{name: "tombstone with entries", mutate: func(m *Manifest, _ *ManifestLimits) { m.Kind = ManifestTombstone }},
		{name: "snapshot without entries", mutate: func(m *Manifest, _ *ManifestLimits) { m.Entries = nil }},
		{name: "entry limit", mutate: func(_ *Manifest, limits *ManifestLimits) { limits.MaxEntries = 1 }},
		{name: "object limit", mutate: func(_ *Manifest, limits *ManifestLimits) { limits.MaxObjects = 2 }},
		{name: "path limit", mutate: func(_ *Manifest, limits *ManifestLimits) { limits.MaxPathBytes = 2 }},
		{name: "file limit", mutate: func(_ *Manifest, limits *ManifestLimits) { limits.MaxFileBytes = 7 }},
		{name: "canonical limit", mutate: func(_ *Manifest, limits *ManifestLimits) { limits.MaxCanonicalBytes = 10 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			manifest := cloneManifest(valid)
			limits := DefaultManifestLimits()
			tc.mutate(&manifest, &limits)
			_, err := ValidateAndCanonicalize(identity, manifest, limits)
			assert.ErrorIs(t, err, ErrInvalid)
		})
	}
}

func TestValidateAndCanonicalizeAcceptsEmptyTombstone(t *testing.T) {
	t.Parallel()

	identity, err := NewAuthIdentity("tenant-a", "device-a")
	require.NoError(t, err)
	manifest := validManifest()
	manifest.Kind = ManifestTombstone
	manifest.Entries = nil

	got, err := ValidateAndCanonicalize(identity, manifest, DefaultManifestLimits())
	require.NoError(t, err)
	assert.Empty(t, got.Objects)
	assert.Empty(t, got.Manifest.Entries)
}

func TestValidateManifestForUploadAllowsProvisionalParentReceipt(t *testing.T) {
	t.Parallel()

	manifest := validManifest()
	manifest.ExpectedParentReceipt = ""

	require.NoError(t, ValidateManifestForUpload(manifest, DefaultManifestLimits()))
}

func TestValidateManifestForUploadEnforcesProspectiveObjectLimit(t *testing.T) {
	t.Parallel()

	manifest := validManifest()
	limits := DefaultManifestLimits()
	limits.MaxObjects = 2

	err := ValidateManifestForUpload(manifest, limits)

	require.ErrorIs(t, err, ErrInvalid)
}

func TestValidateManifestForUploadReservesWorstCaseEscapedAuthenticationIDs(t *testing.T) {
	t.Parallel()

	manifest := validManifest()
	manifest.ExpectedParentReceipt = ""
	prospective := cloneManifest(manifest)
	prospective.ExpectedParentReceipt = strings.Repeat("0", 64)
	asciiIdentity, err := NewAuthIdentity(
		strings.Repeat("t", maxOpaqueIDBytes),
		strings.Repeat("d", maxOpaqueIDBytes),
	)
	require.NoError(t, err)
	ascii, err := ValidateAndCanonicalize(
		asciiIdentity, prospective, DefaultManifestLimits(),
	)
	require.NoError(t, err)
	limits := DefaultManifestLimits()
	limits.MaxCanonicalBytes = len(ascii.CanonicalJSON)
	escapedIdentity, err := NewAuthIdentity(
		strings.Repeat(`"`, maxOpaqueIDBytes),
		strings.Repeat(`"`, maxOpaqueIDBytes),
	)
	require.NoError(t, err)
	_, err = ValidateAndCanonicalize(escapedIdentity, prospective, limits)
	require.ErrorIs(t, err, ErrInvalid, "test limit must reject valid escaped IDs")

	err = ValidateManifestForUpload(manifest, limits)

	require.ErrorIs(t, err, ErrInvalid)
}

func validManifest() Manifest {
	return Manifest{
		SchemaVersion:    ManifestSchemaVersion,
		Provider:         parser.AgentCodex,
		ConfiguredRootID: "root-a",
		SourceKey:        "sessions/demo.jsonl#main",
		CaptureID:        "capture-a",
		CapturedAt:       time.Date(2026, 8, 13, 12, 34, 56, 0, time.UTC),
		Kind:             ManifestSnapshot,
		Entries: []Entry{
			{
				Path:   "z.jsonl",
				Type:   "file",
				Length: 8,
				Objects: []ObjectRef{
					{SHA256: strings.Repeat("a", 64), Length: 4},
					{SHA256: strings.Repeat("a", 64), Length: 4},
				},
			},
			{
				Path:   "a.jsonl",
				Type:   "file",
				Length: 3,
				Objects: []ObjectRef{
					{SHA256: strings.Repeat("b", 64), Length: 3},
				},
			},
		},
	}
}

func cloneManifest(source Manifest) Manifest {
	cloned := source
	cloned.Entries = make([]Entry, len(source.Entries))
	for i, entry := range source.Entries {
		cloned.Entries[i] = entry
		cloned.Entries[i].Objects = append([]ObjectRef(nil), entry.Objects...)
	}
	return cloned
}

func TestValidateCanonicalManifestRejectsNoncanonicalValues(t *testing.T) {
	t.Parallel()

	identity, err := NewAuthIdentity("tenant-a", "device-a")
	require.NoError(t, err)
	canonical, err := ValidateAndCanonicalize(identity, validManifest(), DefaultManifestLimits())
	require.NoError(t, err)
	require.NoError(t, ValidateCanonicalManifest(canonical))

	for _, tc := range []struct {
		name   string
		mutate func(*CanonicalManifest)
	}{
		{name: "unsorted entries", mutate: func(m *CanonicalManifest) {
			m.Manifest.Entries[0], m.Manifest.Entries[1] = m.Manifest.Entries[1], m.Manifest.Entries[0]
		}},
		{name: "non-utc captured time", mutate: func(m *CanonicalManifest) {
			m.Manifest.CapturedAt = m.Manifest.CapturedAt.In(time.FixedZone("offset", 2*60*60))
		}},
		{name: "wrong manifest id", mutate: func(m *CanonicalManifest) {
			m.ManifestID = strings.Repeat("0", 64)
		}},
		{name: "tampered canonical json", mutate: func(m *CanonicalManifest) {
			m.CanonicalJSON = append([]byte(nil), m.CanonicalJSON...)
			m.CanonicalJSON[len(m.CanonicalJSON)-2] = 'x'
		}},
		{name: "extra object", mutate: func(m *CanonicalManifest) {
			m.Objects = append(m.Objects, ObjectRef{SHA256: strings.Repeat("c", 64), Length: 1})
		}},
		{name: "wrong identity", mutate: func(m *CanonicalManifest) {
			m.Identity.DeviceID = "device-b"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mutated := canonical
			mutated.Manifest = cloneManifest(canonical.Manifest)
			mutated.Objects = append([]ObjectRef(nil), canonical.Objects...)
			tc.mutate(&mutated)
			assert.ErrorIs(t, ValidateCanonicalManifest(mutated), ErrInvalid)
		})
	}
}
