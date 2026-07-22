package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeClock returns a now func that advances only via the returned
// step function, keeping throttle tests deterministic.
func fakeClock(start time.Time) (now func() time.Time, step func(time.Duration)) {
	current := start
	return func() time.Time { return current },
		func(d time.Duration) { current = current.Add(d) }
}

func TestStartupStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 7, 2, 22, 0, 0, 0, time.UTC)
	now, _ := fakeClock(base)

	w := newStartupStateWriter(dir, now)
	w.SetPhase("opening database")

	st := readStartupState(dir)
	require.NotNil(t, st, "state must be readable after SetPhase")
	assert.Equal(t, os.Getpid(), st.PID)
	assert.Equal(t, "opening database", st.Phase)
	assert.Empty(t, st.Detail)
	assert.True(t, st.StartedAt.Equal(base), "started_at = %v", st.StartedAt)
	assert.True(t, st.UpdatedAt.Equal(base), "updated_at = %v", st.UpdatedAt)
}

func TestStartupStateDetailThrottle(t *testing.T) {
	dir := t.TempDir()
	now, step := fakeClock(time.Date(2026, 7, 2, 22, 0, 0, 0, time.UTC))

	w := newStartupStateWriter(dir, now)
	w.SetPhase("full resync")

	// The first detail for a phase is immediately useful and persists even
	// though the phase snapshot was just written.
	w.SetDetail("1/100 sessions")
	st := readStartupState(dir)
	require.NotNil(t, st)
	assert.Equal(t, "1/100 sessions", st.Detail)

	// Repeating a phase must not clear its detail or bypass the throttle for a
	// counter-only update.
	w.SetPhase("full resync")
	w.SetDetail("2/100 sessions")
	st = readStartupState(dir)
	require.NotNil(t, st)
	assert.Equal(t, "1/100 sessions", st.Detail)

	// Past the window the next detail persists.
	step(startupDetailThrottle)
	w.SetDetail("50/100 sessions")
	st = readStartupState(dir)
	require.NotNil(t, st)
	assert.Equal(t, "50/100 sessions", st.Detail)

	// A phase change bypasses the throttle and clears the detail.
	w.SetDetail("60/100 sessions")
	w.SetPhase("starting HTTP server")
	st = readStartupState(dir)
	require.NotNil(t, st)
	assert.Equal(t, "starting HTTP server", st.Phase)
	assert.Empty(t, st.Detail)
}

func TestReadStartupStateMissingOrCorrupt(t *testing.T) {
	dir := t.TempDir()
	assert.Nil(t, readStartupState(dir), "missing file must read as nil")

	require.NoError(t, os.WriteFile(
		startupStatePath(dir), []byte("{not json"), 0o600,
	))
	assert.Nil(t, readStartupState(dir), "corrupt file must read as nil")
}

func TestRemoveStartupState(t *testing.T) {
	dir := t.TempDir()
	now, _ := fakeClock(time.Now())
	w := newStartupStateWriter(dir, now)
	w.SetPhase("opening database")
	require.NotNil(t, readStartupState(dir))

	removeStartupState(dir)
	assert.Nil(t, readStartupState(dir))
	assert.NoFileExists(t, startupStatePath(dir))
}

func TestStartupStateWriterNilReceiver(t *testing.T) {
	var w *startupStateWriter
	assert.NotPanics(t, func() {
		w.SetPhase("opening database")
		w.SetDetail("1/2 sessions")
	})
}

func TestServeStartingStatusLines(t *testing.T) {
	base := time.Date(2026, 7, 2, 22, 0, 0, 0, time.UTC)
	now := base.Add(72 * time.Second)
	logPath := filepath.Join("data", "serve.log")

	tests := []struct {
		name string
		st   *startupState
		want []string
	}{
		{
			name: "nil state",
			st:   nil,
			want: nil,
		},
		{
			name: "full state",
			st: &startupState{
				PID:       48151,
				StartedAt: base,
				Phase:     "full resync",
				Detail:    "claude: 12/38 sessions (32%)",
				LogPath:   logPath,
			},
			want: []string{
				"  pid:     48151",
				"  elapsed: 1m12s",
				"  phase:   full resync: claude: 12/38 sessions (32%)",
				"  log:     " + logPath,
			},
		},
		{
			name: "no detail no log",
			st: &startupState{
				PID:       7,
				StartedAt: base,
				Phase:     "opening database",
			},
			want: []string{
				"  pid:     7",
				"  elapsed: 1m12s",
				"  phase:   opening database",
			},
		},
		{
			name: "zero fields render nothing",
			st:   &startupState{},
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, serveStartingStatusLines(tt.st, now))
		})
	}
}
