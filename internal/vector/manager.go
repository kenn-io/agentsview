package vector

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

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

// ErrUnknownServer indicates a build request named an embeddings server that
// is not defined in the manager's encoder set — caller input, not a manager
// fault. Match it with errors.Is; the error's own message carries the
// offending name.
var ErrUnknownServer = errors.New("unknown embeddings server")

type unknownServerError struct {
	name string
}

func (e *unknownServerError) Error() string {
	return fmt.Sprintf("no embeddings server named %q", e.name)
}

func (e *unknownServerError) Is(target error) bool { return target == ErrUnknownServer }

// Manager serializes embedding builds over one Index: only one Build call
// may run at a time, whether triggered via StartBuild (async, for the HTTP
// API) or TryBuild (sync, for a periodic scheduler). Activate and Retire
// are likewise serialized against each other and against build starts, so
// their check-then-act refusal invariants (Missing coverage, the
// active-generation check) hold under concurrent calls.
type Manager struct {
	ix       *Index
	src      UnitSource
	encoders EncoderSet
	gen      kitvec.Generation

	// opMu serializes lifecycle operations: build starts (begin) and the
	// whole of Activate/Retire. It is never held across a running build —
	// begin releases it once running is set — so StartBuild stays
	// non-blocking while a build is in flight.
	opMu sync.Mutex

	// mu guards running and status; held only for short field updates so
	// Status() stays responsive during a build.
	mu      sync.Mutex
	running bool
	status  BuildStatus
	eta     buildETAEstimator

	// now stamps BuildStatus.StartedAt when a build begins; a test hook
	// defaulting to time.Now.
	now func() time.Time
}

// BuildRequest is the caller-controlled subset of BuildOptions the manager
// exposes; encode settings and Progress are the manager's own concerns.
type BuildRequest struct {
	FullRebuild bool `json:"full_rebuild,omitempty"`
	Backstop    bool `json:"backstop,omitempty"`
	// IncludeAutomated is the resolved include-automated scope for this
	// build (caller-resolved from config and, for the CLI's one-off
	// --include-automated flag, its override). See BuildOptions.IncludeAutomated.
	IncludeAutomated bool `json:"include_automated,omitempty"`
	// Using names the embeddings server (an EncoderSet entry) this build
	// encodes against; empty selects the set's default. Server choice is
	// transport only — every server encodes the same model, so it never
	// affects the generation fingerprint.
	Using string `json:"using,omitempty"`
}

// BuildStatus reports the manager's current build state, for polling
// clients (CLI and HTTP API).
type BuildStatus struct {
	Running bool `json:"running"`
	// BuildID identifies one build within this daemon process: it increments
	// each time a build starts, so a polling client can tell two builds
	// apart (e.g. to reset a progress-rate estimator across page reloads)
	// instead of treating unrelated builds as continuous. Zero until the
	// first build of the process.
	BuildID int64 `json:"build_id,omitempty"`
	// StartedAt is when the current (or, once it finished, most recent)
	// build began, RFC3339 UTC. Empty until the first build of the process.
	StartedAt string `json:"started_at,omitempty"`
	Phase     string `json:"phase,omitempty"`
	Done      int64  `json:"done"`
	Total     int64  `json:"total"`
	// EstimateReady is true once the daemon has enough positive progress
	// samples to publish a stable rate and ETA for the current build phase.
	EstimateReady   bool         `json:"estimate_ready,omitempty"`
	RatePerSecond   float64      `json:"rate_per_second,omitempty"`
	ETAMilliseconds int64        `json:"eta_milliseconds"`
	LastError       string       `json:"last_error,omitempty"`
	LastResult      *BuildResult `json:"last_result,omitempty"`
	// Model and Dimension identify the configured embedding space the
	// manager builds ([vector.embeddings] model/dimension), so status
	// consumers can display the target space even before any generation
	// exists. They describe config, not a specific build.
	Model     string `json:"model,omitempty"`
	Dimension int    `json:"dimension,omitempty"`
}

// EncodeSettings groups the encode-shape knobs of one embeddings server,
// resolved from config ([vector.embeddings.servers.<name>] batch_size and
// concurrency).
type EncodeSettings struct {
	// BatchSize is the number of inputs sent per HTTP call.
	BatchSize int
	// Concurrency is the number of documents encoded in parallel.
	Concurrency int
}

// ManagedEncoder pairs one embeddings server's encoder with the encode
// settings tuned for that server.
type ManagedEncoder struct {
	Encode   kitvec.EncodeFunc
	Settings EncodeSettings
}

// EncoderSet is the named embeddings servers a Manager can build with. All
// entries encode the same model — the embedding-space identity is global
// config — so a build may use any of them interchangeably; Default names
// the entry used when a BuildRequest doesn't select one.
type EncoderSet struct {
	Default string
	ByName  map[string]ManagedEncoder
}

// NewManager creates a Manager that builds gen's embedding space over ix,
// scanning src and encoding with one of encoders' entries per build (the
// default, or BuildRequest.Using). Each encoder is wrapped so a panic
// inside it surfaces as an encode error rather than crashing the process
// (see recoveringEncoder).
func NewManager(
	ix *Index, src UnitSource, encoders EncoderSet, gen kitvec.Generation,
) *Manager {
	wrapped := EncoderSet{Default: encoders.Default, ByName: make(map[string]ManagedEncoder, len(encoders.ByName))}
	for name, me := range encoders.ByName {
		me.Encode = recoveringEncoder(me.Encode)
		wrapped.ByName[name] = me
	}
	return &Manager{ix: ix, src: src, encoders: wrapped, gen: gen, now: time.Now}
}

// resolveEncoder picks the encoder a build request encodes with: the named
// entry when Using is set, the set's default otherwise. It fails when the
// name is unknown so a mistyped --using errors before a build starts.
func (m *Manager) resolveEncoder(req BuildRequest) (ManagedEncoder, error) {
	name := req.Using
	if name == "" {
		name = m.encoders.Default
	}
	me, ok := m.encoders.ByName[name]
	if !ok {
		return ManagedEncoder{}, &unknownServerError{name: name}
	}
	return me, nil
}

// recoveringEncoder converts a panic in enc (a caller-supplied network
// client) into an ordinary encode error. This must wrap the encoder itself
// rather than rely on runBuild's recover: kit's EncodeBatched invokes
// encoders on its own worker goroutines, where a recover on the manager's
// build goroutine cannot reach.
func recoveringEncoder(enc kitvec.EncodeFunc) kitvec.EncodeFunc {
	return func(ctx context.Context, texts []string) (vectors [][]float32, err error) {
		defer func() {
			if r := recover(); r != nil {
				vectors = nil
				err = fmt.Errorf("encoder panicked: %v", r)
			}
		}()
		return enc(ctx, texts)
	}
}

// StartBuild launches a Build in a background goroutine, returning
// ErrBuildRunning if one is already in flight rather than queuing behind it.
// The goroutine runs against context.Background() so it outlives the HTTP
// request that triggered it.
func (m *Manager) StartBuild(req BuildRequest) error {
	me, err := m.resolveEncoder(req)
	if err != nil {
		return err
	}
	if err := m.begin(); err != nil {
		return err
	}
	go func() {
		result, err := m.runBuild(context.Background(), req, me)
		m.finish(result, err)
	}()
	return nil
}

// TryBuild runs one Build synchronously, for a periodic scheduler that
// should drop a scheduled run rather than queue it: it returns (false, nil)
// without starting anything if a build is already running.
func (m *Manager) TryBuild(ctx context.Context, req BuildRequest) (bool, error) {
	me, err := m.resolveEncoder(req)
	if err != nil {
		return false, err
	}
	if err := m.begin(); err != nil {
		return false, nil
	}
	result, err := m.runBuild(ctx, req, me)
	m.finish(result, err)
	return true, err
}

// Status returns a snapshot of the manager's current build state, stamped
// with the configured embedding space's identity.
func (m *Manager) Status() BuildStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	status := m.status
	if status.LastResult != nil {
		result := *status.LastResult
		status.LastResult = &result
	}
	status.Model = m.gen.Model
	status.Dimension = m.gen.Dimensions
	return status
}

// Generations delegates to the underlying Index, listing every generation
// with its coverage of the current mirror.
func (m *Manager) Generations(ctx context.Context) ([]GenerationInfo, error) {
	return m.ix.Generations(ctx)
}

// Activate transitions the generation identified by id to active, retiring
// whichever generation was previously active (in one transaction, via the
// same activateGeneration primitive Build's auto-activation uses, so two
// generations can never end up active simultaneously). Without force, it
// refuses when id's generation has documents still needing embedding
// (Missing > 0) or while a build is running. Serialized against Retire,
// other Activate calls, and build starts via opMu.
func (m *Manager) Activate(ctx context.Context, id int64, force bool) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if m.isRunning() {
		return ErrBuildRunning
	}

	target, err := m.ix.GenerationByID(ctx, id)
	if err != nil {
		return err
	}
	if !force && target.Missing > 0 {
		return refusedf("generation %d still has %d documents needing embedding; use --force",
			id, target.Missing)
	}
	return m.ix.activateGeneration(ctx, target.Fingerprint)
}

// Retire transitions the generation identified by id to retired. Without
// force, it refuses when id is the active generation or while a build is
// running. Serialized against Activate, other Retire calls, and build
// starts via opMu.
func (m *Manager) Retire(ctx context.Context, id int64, force bool) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
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
// without changing anything if a build is already in flight. Taking opMu
// first serializes build starts behind any in-flight Activate/Retire.
func (m *Manager) begin() error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running {
		return ErrBuildRunning
	}
	m.running = true
	m.status.Running = true
	m.status.BuildID++
	m.status.StartedAt = m.now().UTC().Format(time.RFC3339)
	m.status.Phase = ""
	m.status.Done = 0
	m.status.Total = 0
	m.clearETA()
	return nil
}

// runBuild performs the actual Index.Build call, wiring the manager's
// reportProgress method in as the BuildOptions.Progress callback so Status
// reflects incremental progress while the build is in flight. It converts
// a panic (e.g. from the caller-supplied encoder, a network client) into an
// error so StartBuild's detached goroutine can never crash the process and
// TryBuild's caller sees a failure rather than a propagating panic; either
// way finish records it in LastError and clears the running state.
func (m *Manager) runBuild(
	ctx context.Context, req BuildRequest, me ManagedEncoder,
) (result BuildResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("build panicked: %v", r)
		}
	}()
	return m.ix.Build(ctx, m.src, me.Encode, m.gen, BuildOptions{
		FullRebuild:      req.FullRebuild,
		Backstop:         req.Backstop,
		IncludeAutomated: req.IncludeAutomated,
		BatchSize:        me.Settings.BatchSize,
		Concurrency:      me.Settings.Concurrency,
		Progress:         m.reportProgress,
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
	estimate := m.eta.sample(p.Phase, p.Done, p.Total, m.now())
	m.status.EstimateReady = estimate.Ready
	m.status.RatePerSecond = estimate.RatePerSecond
	m.status.ETAMilliseconds = estimate.Remaining.Milliseconds()
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
	m.clearETA()
	if err != nil {
		m.status.LastError = err.Error()
		return
	}
	m.status.LastError = ""
	r := result
	m.status.LastResult = &r
}

// clearETA resets both the private accumulator and its public status snapshot.
// The caller must hold m.mu.
func (m *Manager) clearETA() {
	m.eta.reset()
	m.status.EstimateReady = false
	m.status.RatePerSecond = 0
	m.status.ETAMilliseconds = 0
}

func (m *Manager) isRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}
