package sync

// CodexExecMigrationKey exposes the pg_sync_state key used to
// gate the one-time codex_exec skip cache migration so tests
// can reset it between engine instantiations.
const CodexExecMigrationKey = codexExecMigrationKey

// FileChangeTime lets external integration tests observe the native timestamp.
var FileChangeTime = fileChangeTime
