package postgres

import (
	"encoding/json"
	"testing"

	"github.com/wesm/agentsview/internal/db"
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
			if err != nil {
				t.Fatalf(
					"readBoundaryAndFingerprints: %v", err,
				)
			}
			if valid != tc.wantValid {
				t.Fatalf(
					"valid = %v, want %v",
					valid, tc.wantValid,
				)
			}
			if len(got) != tc.wantLen {
				t.Fatalf(
					"len(state) = %d, want %d",
					len(got), tc.wantLen,
				)
			}
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

	if got != endedAt {
		t.Fatalf(
			"localSessionSyncMarker = %q, want %q",
			got, endedAt,
		)
	}
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

	fp1 := sessionPushFingerprint(base)

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
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			modified := tc.modify(base)
			fp2 := sessionPushFingerprint(modified)
			if fp1 == fp2 {
				t.Fatalf(
					"fingerprint should differ after %s",
					tc.name,
				)
			}
		})
	}

	if fp1 != sessionPushFingerprint(base) {
		t.Fatal(
			"identical sessions should produce " +
				"identical fingerprints",
		)
	}
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
	if sessionPushFingerprint(s1) == sessionPushFingerprint(s2) {
		t.Fatal(
			"length-prefixed fingerprints should not collide",
		)
	}
}

func TestFinalizePushStatePersistsEmptyBoundary(
	t *testing.T,
) {
	const cutoff = "2026-03-11T12:34:56.123Z"

	store := &syncStateStoreStub{}
	if err := finalizePushState(
		store, cutoff, nil, nil,
	); err != nil {
		t.Fatalf("finalizePushState: %v", err)
	}
	if got := store.values["last_push_at"]; got != cutoff {
		t.Fatalf(
			"last_push_at = %q, want %q", got, cutoff,
		)
	}

	raw := store.values[lastPushBoundaryStateKey]
	if raw == "" {
		t.Fatal(
			"last_push_boundary_state should be written",
		)
	}

	var state pushBoundaryState
	if err := json.Unmarshal(
		[]byte(raw), &state,
	); err != nil {
		t.Fatalf("unmarshal boundary state: %v", err)
	}
	if state.Cutoff != cutoff {
		t.Fatalf(
			"boundary cutoff = %q, want %q",
			state.Cutoff, cutoff,
		)
	}
	if len(state.Fingerprints) != 0 {
		t.Fatalf(
			"boundary fingerprints = %v, want empty",
			state.Fingerprints,
		)
	}
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
	if err := finalizePushState(
		store, cutoff, cycle2Sessions,
		priorFingerprints,
	); err != nil {
		t.Fatalf("finalizePushState: %v", err)
	}

	raw := store.values[lastPushBoundaryStateKey]
	if raw == "" {
		t.Fatal(
			"last_push_boundary_state should be written",
		)
	}

	var state pushBoundaryState
	if err := json.Unmarshal(
		[]byte(raw), &state,
	); err != nil {
		t.Fatalf("unmarshal boundary state: %v", err)
	}

	if len(state.Fingerprints) != 2 {
		t.Fatalf(
			"len(fingerprints) = %d, want 2",
			len(state.Fingerprints),
		)
	}
	if state.Fingerprints["sess-001"] != "fp-001" {
		t.Fatalf(
			"sess-001 fingerprint = %q, want %q",
			state.Fingerprints["sess-001"], "fp-001",
		)
	}
	if _, ok := state.Fingerprints["sess-002"]; !ok {
		t.Fatal("sess-002 fingerprint should be present")
	}
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
			got := sanitizePG(tc.input)
			if got != tc.want {
				t.Errorf(
					"sanitizePG(%q) = %q, want %q",
					tc.input, got, tc.want,
				)
			}
		})
	}
}

func TestNilIfEmptySanitizes(t *testing.T) {
	got := nilIfEmpty("hello\x00world")
	if got != "helloworld" {
		t.Errorf(
			"nilIfEmpty with null byte = %q, want %q",
			got, "helloworld",
		)
	}

	if nilIfEmpty("") != nil {
		t.Error("nilIfEmpty(\"\") should be nil")
	}

	// A string that reduces to empty after sanitization
	// should return nil, not "".
	if nilIfEmpty("\x00") != nil {
		t.Error("nilIfEmpty(\"\\x00\") should be nil")
	}
}

func TestNilStrSanitizes(t *testing.T) {
	s := "hello\xe2world"
	got := nilStr(&s)
	if got != "helloworld" {
		t.Errorf(
			"nilStr with invalid UTF-8 = %q, want %q",
			got, "helloworld",
		)
	}

	// A *string that reduces to empty after sanitization
	// should return nil.
	nul := "\x00"
	if nilStr(&nul) != nil {
		t.Error("nilStr(\"\\x00\") should be nil")
	}
}
