//go:build pgtest

package postgres

// PGTestURL gives tests in the external postgres_test package the calling
// test's own database (see testPGURL).
var PGTestURL = testPGURL
