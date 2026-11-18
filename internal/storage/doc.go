// Package storage defines the Go contract a storage backend implements so the
// CLI and HTTP server can drive it without importing the backend package.
//
// Backends fill one of two roles. The roles are separate interfaces on
// purpose: no backend implements both.
//
//   - Archive: the writable SQLite archive (*db.DB). Parser ingestion, resync,
//     Codex checkpoints, and every local-only write stay here. The archive
//     has no interface beyond db.Store because there is exactly one.
//   - Replica: a remote database the archive pushes into and that serves the
//     web UI read-only (PostgreSQL, CockroachDB, and ClickHouse today). Replica
//     is the contract a new remote SQL backend implements.
//
// Every backend also implements db.Store, the HTTP read-and-curation surface.
// internal/backendcontract holds the compile-time assertions for all roles.
package storage
