package sync

// CodexExecMigrationKey exposes the pg_sync_state key used to
// gate the one-time codex_exec skip cache migration so tests
// can reset it between engine instantiations.
const CodexExecMigrationKey = codexExecMigrationKey

// FileChangeTime lets external integration tests observe the native timestamp.
var FileChangeTime = fileChangeTime

// SetParseAdmissionObserver installs the result-admission cardinality probe.
func SetParseAdmissionObserver(e *Engine, fn func(yielded, retained int)) {
	e.parseAdmissionObserver = fn
}

// SetChangedPathListedHook installs the post-listing changed-path test seam.
func SetChangedPathListedHook(e *Engine, fn func(path string)) {
	e.changedPathListedHook = fn
}
