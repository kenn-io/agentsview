package vector

import (
	"context"
	"errors"
	"fmt"
	"sync"

	kitvec "go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"
)

// ErrBuildRunning is returned by StartBuild when a build is already in
// flight, and by Activate/Retire when they are called while one is running.
var ErrBuildRunning = errors.New("an embeddings build is already running")

// ErrGenerationRefused indicates Activate or Retire declined to change a
// generation's state without --force (incomplete coverage, or retiring the
// active generation). Match it with errors.Is; the error's own message
// carries the specific, user-facing reason rather than a fixed string.
var ErrGenerationRefused = errors.New("generation state change refused")

// refusalError carries a specific, literal refusal message while still
// satisfying errors.Is(err, ErrGenerationRefused), so callers get an
// unprefixed message (e.g. for direct display) alongside a stable sentinel
// for status-code mapping.
type refusalError struct {
	msg string
}

func refusedf(format string, args ...any) error {
	return &refusalError{msg: fmt.Sprintf(format, args...)}
}

func (e *refusalError) Error() string { return e.msg }

func (e *refusalError) Is(target error) bool { return target == ErrGenerationRefused }

// Manager serializes embedding builds over one Index: only one Build call
// may run at a time, whether triggered via StartBuild (async, for the HTTP
// API) or TryBuild (sync, for a periodic scheduler).
type Manager struct {
	ix        *Index
	src       MessageSource
	enc       kitvec.EncodeFunc
	gen       kitvec.Generation
	batchSize int

	mu      sync.Mutex
	running bool
	status  BuildStatus
}

// BuildRequest is the caller-controlled subset of BuildOptions the manager
// exposes; BatchSize and Progress are the manager's own concerns.
type BuildRequest struct {
	FullRebuild bool `json:"full_rebuild,omitempty"`
	Backstop    bool `json:"backstop,omitempty"`
}

// BuildStatus reports the manager's current build state, for polling
// clients (CLI and HTTP API).
type BuildStatus struct {
	Running    bool         `json:"running"`
	Phase      string       `json:"phase,omitempty"`
	Done       int64        `json:"done"`
	Total      int64        `json:"total"`
	LastError  string       `json:"last_error,omitempty"`
	LastResult *BuildResult `json:"last_result,omitempty"`
}

// NewManager creates a Manager that builds gen's embedding space over ix,
// scanning src and encoding with enc in batches of batchSize.
func NewManager(
	ix *Index, src MessageSource, enc kitvec.EncodeFunc,
	gen kitvec.Generation, batchSize int,
) *Manager {
	return &Manager{ix: ix, src: src, enc: enc, gen: gen, batchSize: batchSize}
}

// StartBuild launches a Build in a background goroutine, returning
// ErrBuildRunning if one is already in flight rather than queuing behind it.
// The goroutine runs against context.Background() so it outlives the HTTP
// request that triggered it.
func (m *Manager) StartBuild(req BuildRequest) error {
	if err := m.begin(); err != nil {
		return err
	}
	go func() {
		result, err := m.runBuild(context.Background(), req)
		m.finish(result, err)
	}()
	return nil
}

// TryBuild runs one Build synchronously, for a periodic scheduler that
// should drop a scheduled run rather than queue it: it returns (false, nil)
// without starting anything if a build is already running.
func (m *Manager) TryBuild(ctx context.Context, req BuildRequest) (bool, error) {
	if err := m.begin(); err != nil {
		return false, nil
	}
	result, err := m.runBuild(ctx, req)
	m.finish(result, err)
	return true, err
}

// Status returns a snapshot of the manager's current build state.
func (m *Manager) Status() BuildStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	status := m.status
	if status.LastResult != nil {
		result := *status.LastResult
		status.LastResult = &result
	}
	return status
}

// Generations delegates to the underlying Index, listing every generation
// with its coverage of the current mirror.
func (m *Manager) Generations(ctx context.Context) ([]GenerationInfo, error) {
	return m.ix.Generations(ctx)
}

// Activate transitions the generation identified by id to active, retiring
// whichever generation was previously active. Without force, it refuses
// when id's generation has documents still needing embedding (Missing > 0)
// or while a build is running.
func (m *Manager) Activate(ctx context.Context, id int64, force bool) error {
	if m.isRunning() {
		return ErrBuildRunning
	}

	target, err := m.ix.GenerationByID(ctx, id)
	if err != nil {
		return err
	}
	if !force && target.Missing > 0 {
		return refusedf("generation %d still has %d messages needing embedding; use --force",
			id, target.Missing)
	}

	gens, err := m.ix.Generations(ctx)
	if err != nil {
		return err
	}
	for _, g := range gens {
		if g.ID != id && g.State == string(sqlitevec.StateActive) {
			if err := m.ix.SetStateByID(ctx, g.ID, sqlitevec.StateRetired); err != nil {
				return err
			}
		}
	}
	return m.ix.SetStateByID(ctx, id, sqlitevec.StateActive)
}

// Retire transitions the generation identified by id to retired. Without
// force, it refuses when id is the active generation or while a build is
// running.
func (m *Manager) Retire(ctx context.Context, id int64, force bool) error {
	if m.isRunning() {
		return ErrBuildRunning
	}

	target, err := m.ix.GenerationByID(ctx, id)
	if err != nil {
		return err
	}
	if !force && target.State == string(sqlitevec.StateActive) {
		return refusedf("generation %d is active; use --force to retire it", id)
	}
	return m.ix.SetStateByID(ctx, id, sqlitevec.StateRetired)
}

// begin transitions the manager into the running state, resetting the
// progress fields of status for the new run. It returns ErrBuildRunning
// without changing anything if a build is already in flight.
func (m *Manager) begin() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running {
		return ErrBuildRunning
	}
	m.running = true
	m.status.Running = true
	m.status.Phase = ""
	m.status.Done = 0
	m.status.Total = 0
	return nil
}

// runBuild performs the actual Index.Build call, wiring the manager's
// reportProgress method in as the BuildOptions.Progress callback so Status
// reflects incremental progress while the build is in flight.
func (m *Manager) runBuild(ctx context.Context, req BuildRequest) (BuildResult, error) {
	return m.ix.Build(ctx, m.src, m.enc, m.gen, BuildOptions{
		FullRebuild: req.FullRebuild,
		Backstop:    req.Backstop,
		BatchSize:   m.batchSize,
		Progress:    m.reportProgress,
	})
}

// reportProgress updates status's progress fields under the manager's lock;
// it is passed as BuildOptions.Progress and so runs on the build goroutine.
func (m *Manager) reportProgress(p BuildProgress) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status.Phase = p.Phase
	m.status.Done = p.Done
	m.status.Total = p.Total
}

// finish records a completed build's outcome and clears the running state.
// A successful build sets LastResult and clears any previous LastError; a
// failed build sets LastError and leaves the last successful LastResult (if
// any) untouched.
func (m *Manager) finish(result BuildResult, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.running = false
	m.status.Running = false
	if err != nil {
		m.status.LastError = err.Error()
		return
	}
	m.status.LastError = ""
	r := result
	m.status.LastResult = &r
}

func (m *Manager) isRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}
