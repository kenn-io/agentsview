package postgres

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

type syncStateReaderStub struct {
	value string
	err   error
}

func (s syncStateReaderStub) GetSyncState(
	key string,
) (string, error) {
	return s.value, s.err
}

func (s syncStateReaderStub) SetSyncState(
	string, string,
) error {
	return nil
}

type syncStateStoreStub struct {
	values map[string]string
}

func (s *syncStateStoreStub) GetSyncState(
	key string,
) (string, error) {
	return s.values[key], nil
}

func (s *syncStateStoreStub) SetSyncState(
	key, value string,
) error {
	if s.values == nil {
		s.values = make(map[string]string)
	}
	s.values[key] = value
	return nil
}

func TestReadPushBoundaryStateValidity(t *testing.T) {
	const cutoff = "2026-03-11T12:34:56.123Z"

	tests := []struct {
		name      string
		raw       string
		wantValid bool
		wantLen   int
	}{
		{
			name:      "missing state",
			raw:       "",
			wantValid: false,
			wantLen:   0,
		},
		{
			name:      "bare map without cutoff",
			raw:       `{"sess-001":"fingerprint"}`,
			wantValid: false,
			wantLen:   0,
		},
		{
			name:      "malformed payload",
			raw:       `{`,
			wantValid: false,
			wantLen:   0,
		},
		{
			name:      "stale cutoff",
			raw:       `{"cutoff":"2026-03-11T12:34:56.122Z","fingerprints":{"sess-001":"fingerprint"}}`,
			wantValid: false,
			wantLen:   0,
		},
		{
			name:      "matching cutoff",
			raw:       `{"cutoff":"2026-03-11T12:34:56.123Z","fingerprints":{"sess-001":"fingerprint"}}`,
			wantValid: true,
			wantLen:   1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, got, valid, err := readBoundaryAndFingerprints(
				syncStateReaderStub{value: tc.raw},
				cutoff,
			)
			require.NoError(t, err)
			require.Equal(t, tc.wantValid, valid)
			require.Len(t, got, tc.wantLen)
		})
	}
}

func TestLocalSessionSyncMarkerNormalizesSecondPrecisionTimestamps(t *testing.T) {
	startedAt := "2026-03-11T12:34:56Z"
	endedAt := "2026-03-11T12:34:56.123Z"

	got := localSessionSyncMarker(db.Session{
		CreatedAt: "2026-03-11T12:34:55Z",
		StartedAt: &startedAt,
		EndedAt:   &endedAt,
	})

	require.Equal(t, endedAt, got)
}

func TestSessionPushFingerprintDiffers(t *testing.T) {
	base := db.Session{
		ID:               "sess-001",
		Project:          "proj",
		Machine:          "laptop",
		Agent:            "claude",
		MessageCount:     5,
		UserMessageCount: 2,
		CreatedAt:        "2026-03-11T12:00:00Z",
	}

	fp1 := sessionPushFingerprint(base, "")

	tests := []struct {
		name   string
		modify func(s db.Session) db.Session
	}{
		{
			name: "message count change",
			modify: func(s db.Session) db.Session {
				s.MessageCount = 6
				return s
			},
		},
		{
			name: "display name change",
			modify: func(s db.Session) db.Session {
				name := "new name"
				s.DisplayName = &name
				return s
			},
		},
		{
			name: "session_name change",
			modify: func(s db.Session) db.Session {
				n := "agent-provided-title"
				s.SessionName = &n
				return s
			},
		},
		{
			name: "ended at change",
			modify: func(s db.Session) db.Session {
				ended := "2026-03-11T13:00:00Z"
				s.EndedAt = &ended
				return s
			},
		},
		{
			name: "file hash change",
			modify: func(s db.Session) db.Session {
				hash := "abc123"
				s.FileHash = &hash
				return s
			},
		},
		{
			name: "termination_status change",
			modify: func(s db.Session) db.Session {
				ts := "tool_call_pending"
				s.TerminationStatus = &ts
				return s
			},
		},
		{
			name: "automated classification change",
			modify: func(s db.Session) db.Session {
				s.IsAutomated = true
				return s
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			modified := tc.modify(base)
			fp2 := sessionPushFingerprint(modified, "")
			require.NotEqual(t, fp1, fp2,
				"fingerprint should differ after %s", tc.name)
		})
	}

	assert.Equal(t, fp1, sessionPushFingerprint(base, ""),
		"identical sessions should produce identical fingerprints")
}

func TestSessionPushFingerprintIncludesUsageEventFingerprint(
	t *testing.T,
) {
	base := db.Session{
		ID:               "sess-001",
		Project:          "proj",
		Machine:          "laptop",
		Agent:            "claude",
		MessageCount:     5,
		UserMessageCount: 2,
		CreatedAt:        "2026-03-11T12:00:00Z",
	}

	withoutUsage := sessionPushFingerprint(base, "")
	withUsage := sessionPushFingerprint(base, "usage-fp")
	assert.NotEqual(t, withoutUsage, withUsage,
		"usage event fingerprint should affect session fingerprint")
}

func TestSessionPushFingerprintNoFieldCollisions(
	t *testing.T,
) {
	s1 := db.Session{
		ID:        "ab",
		Project:   "cd",
		CreatedAt: "2026-03-11T12:00:00Z",
	}
	s2 := db.Session{
		ID:        "a",
		Project:   "bcd",
		CreatedAt: "2026-03-11T12:00:00Z",
	}
	assert.NotEqual(t,
		sessionPushFingerprint(s1, ""),
		sessionPushFingerprint(s2, ""),
		"length-prefixed fingerprints should not collide")
}

func TestFinalizePushStatePersistsEmptyBoundary(
	t *testing.T,
) {
	const cutoff = "2026-03-11T12:34:56.123Z"

	store := &syncStateStoreStub{}
	require.NoError(t, finalizePushState(
		store, cutoff, nil, nil, map[string]string{},
	))
	assert.Equal(t, cutoff, store.values["last_push_at"])

	raw := store.values[lastPushBoundaryStateKey]
	require.NotEmpty(t, raw, "last_push_boundary_state should be written")

	var state pushBoundaryState
	require.NoError(t, json.Unmarshal([]byte(raw), &state))
	assert.Equal(t, cutoff, state.Cutoff)
	assert.Empty(t, state.Fingerprints)
}

func TestFinalizePushStateMergesPriorFingerprints(
	t *testing.T,
) {
	const cutoff = "2026-03-11T12:34:56.123Z"

	priorFingerprints := map[string]string{
		"sess-001": "fp-001",
	}

	cycle2Sessions := []db.Session{
		{
			ID:           "sess-002",
			CreatedAt:    "2026-03-11T12:00:00Z",
			MessageCount: 3,
		},
	}

	store := &syncStateStoreStub{}
	require.NoError(t, finalizePushState(
		store, cutoff, cycle2Sessions,
		priorFingerprints,
		map[string]string{"sess-002": sessionPushFingerprint(cycle2Sessions[0], "")},
	))

	raw := store.values[lastPushBoundaryStateKey]
	require.NotEmpty(t, raw, "last_push_boundary_state should be written")

	var state pushBoundaryState
	require.NoError(t, json.Unmarshal([]byte(raw), &state))

	require.Len(t, state.Fingerprints, 2)
	assert.Equal(t, "fp-001", state.Fingerprints["sess-001"])
	_, ok := state.Fingerprints["sess-002"]
	assert.True(t, ok, "sess-002 fingerprint should be present")
}

func TestSanitizePG(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "clean string",
			input: "hello world",
			want:  "hello world",
		},
		{
			name:  "null bytes stripped",
			input: "hello\x00world",
			want:  "helloworld",
		},
		{
			name:  "multiple null bytes",
			input: "\x00a\x00b\x00",
			want:  "ab",
		},
		{
			name:  "truncated 3-byte sequence",
			input: "hello\xe2world",
			want:  "helloworld",
		},
		{
			name:  "truncated 2 of 3 bytes",
			input: "hello\xe2\x80world",
			want:  "helloworld",
		},
		{
			name: "valid multibyte preserved",
			// U+2026 HORIZONTAL ELLIPSIS = e2 80 a6
			input: "hello\xe2\x80\xa6world",
			want:  "hello\xe2\x80\xa6world",
		},
		{
			name:  "null and invalid combined",
			input: "a\x00b\xe2c",
			want:  "abc",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, sanitizePG(tc.input))
		})
	}
}

func TestNilIfEmptySanitizes(t *testing.T) {
	assert.Equal(t, any("helloworld"), nilIfEmpty("hello\x00world"))

	assert.Nil(t, nilIfEmpty(""), "nilIfEmpty(\"\") should be nil")

	// A string that reduces to empty after sanitization
	// should return nil, not "".
	assert.Nil(t, nilIfEmpty("\x00"), "nilIfEmpty(\"\\x00\") should be nil")
}

func TestNilStrSanitizes(t *testing.T) {
	s := "hello\xe2world"
	assert.Equal(t, any("helloworld"), nilStr(&s))

	// A *string that reduces to empty after sanitization
	// should return nil.
	nul := "\x00"
	assert.Nil(t, nilStr(&nul), "nilStr(\"\\x00\") should be nil")
}
