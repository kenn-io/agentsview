// ABOUTME: startup-state.json publishes the starting daemon's pid, phase,
// ABOUTME: and progress so `serve status` can report them during startup.
package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

// startupStateFileName is the data-dir file holding the starting
// daemon's progress snapshot. It exists exactly as long as the daemon
// start lock is held; readers must only trust it while
// IsDaemonStarting reports true.
const startupStateFileName = "startup-state.json"

// startupDetailThrottle bounds how often detail-only updates rewrite
// the state file during high-frequency sync progress callbacks.
const startupDetailThrottle = time.Second

type startupState struct {
	PID              int       `json:"pid"`
	StartedAt        time.Time `json:"started_at"`
	Phase            string    `json:"phase"`
	Detail           string    `json:"detail,omitempty"`
	LogPath          string    `json:"log_path,omitempty"`
	Host             string    `json:"host,omitempty"`
	Port             int       `json:"port,omitempty"`
	RuntimeError     string    `json:"runtime_error,omitempty"`
	CreateTime       string    `json:"create_time,omitempty"`
	APIVersion       int       `json:"api_version,omitempty"`
	DataVersion      int       `json:"data_version,omitempty"`
	CaddyPID         int       `json:"caddy_pid,omitempty"`
	CaddyCreateTime  string    `json:"caddy_create_time,omitempty"`
	RequireAuth      bool      `json:"require_auth,omitempty"`
	RequireAuthKnown bool      `json:"require_auth_known,omitempty"`
	NoSync           bool      `json:"no_sync,omitempty"`
	NoSyncKnown      bool      `json:"no_sync_known,omitempty"`
	UpdatedAt        time.Time `json:"updated_at"`
}

func startupStatePath(dataDir string) string {
	return filepath.Join(dataDir, startupStateFileName)
}

// serveLogPath is where a background-launched serve child writes its
// startup output.
func serveLogPath(dataDir string) string {
	return filepath.Join(dataDir, "serve.log")
}

// startupStateWriter persists throttled startup progress snapshots.
// Write failures are logged once and otherwise ignored: startup
// transparency must never break startup.
type startupStateWriter struct {
	mu        sync.Mutex
	path      string
	state     startupState
	lastWrite time.Time
	warnOnce  sync.Once
	now       func() time.Time
}

func newStartupStateWriter(
	dataDir string, now func() time.Time,
) *startupStateWriter {
	w := &startupStateWriter{
		path: startupStatePath(dataDir),
		now:  now,
	}
	w.state.PID = os.Getpid()
	w.state.StartedAt = now()
	if runningAsBackgroundChild() {
		// Only a background child's output lands in serve.log; a
		// foreground serve prints to the invoking terminal.
		w.state.LogPath = serveLogPath(dataDir)
	}
	return w
}

// SetPhase records a phase transition and clears the previous phase's
// detail. Phase changes persist immediately, bypassing the throttle.
func (w *startupStateWriter) SetPhase(phase string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.state.Phase = phase
	w.state.Detail = ""
	w.write()
}

// SetDetail records fine-grained progress within the current phase,
// persisted at most once per startupDetailThrottle.
func (w *startupStateWriter) SetDetail(detail string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	// state.Detail only changes when a write happens, so this dedup
	// compares against what a reader can actually see. Storing a
	// throttled detail in memory first would make a stable detail
	// match itself on every later call and never persist.
	if detail == "" || detail == w.state.Detail {
		return
	}
	if w.now().Sub(w.lastWrite) < startupDetailThrottle {
		return
	}
	w.state.Detail = detail
	w.write()
}

// write persists the current state atomically via temp file + rename
// so readers never observe a partial JSON document. Callers hold w.mu.
func (w *startupStateWriter) write() {
	w.state.UpdatedAt = w.now()
	data, err := json.Marshal(w.state)
	if err != nil {
		w.warn(err)
		return
	}
	tmp := w.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		w.warn(err)
		return
	}
	if err := os.Rename(tmp, w.path); err != nil {
		w.warn(err)
		return
	}
	w.lastWrite = w.now()
}

func (w *startupStateWriter) warn(err error) {
	w.warnOnce.Do(func() {
		log.Printf("warning: cannot write startup state: %v", err)
	})
}

// readStartupState loads the startup snapshot, or nil when the file is
// missing or unreadable (legacy daemon version, mid-write race).
func readStartupState(dataDir string) *startupState {
	data, err := os.ReadFile(startupStatePath(dataDir))
	if err != nil {
		return nil
	}
	var st startupState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil
	}
	return &st
}

// publishStartupStateFallback records the endpoint after bind when the
// definitive runtime record cannot be written. Keep the existing snapshot so
// lifecycle readers can still report startup progress and require a daemon-
// authored snapshot before trusting this fallback.
func publishStartupStateFallback(
	dataDir, host string, port int, requireAuth, noSync bool, caddyPID int, runtimeErr error,
) {
	st := readStartupState(dataDir)
	if st == nil || host == "" || port <= 0 || runtimeErr == nil {
		return
	}
	st.Host = host
	st.Port = port
	st.RuntimeError = runtimeErr.Error()
	st.RequireAuth = requireAuth
	st.RequireAuthKnown = true
	st.NoSync = noSync
	st.NoSyncKnown = true
	st.APIVersion = daemonAPIVersion
	st.DataVersion = db.CurrentDataVersion()
	st.CaddyPID = caddyPID
	st.CaddyCreateTime = ""
	if caddyPID > 0 {
		if caddyCreateTime, ok := processCreateTimeMillis(caddyPID); ok {
			st.CaddyCreateTime = strconv.FormatInt(caddyCreateTime, 10)
		}
	}
	if createTime, ok := processCreateTimeMillis(st.PID); ok {
		st.CreateTime = strconv.FormatInt(createTime, 10)
	}
	st.UpdatedAt = time.Now()
	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	tmp := startupStatePath(dataDir) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, startupStatePath(dataDir)); err != nil {
		_ = os.Remove(tmp)
	}
}

func removeStartupState(dataDir string) {
	_ = os.Remove(startupStatePath(dataDir))
}
