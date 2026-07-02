package parser

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"strconv"
	"strings"
)

// Antigravity session databases carry no literal product version: PRAGMA
// user_version is a schema marker (1), not a dotted release, and neither the
// .db nor ~/.gemini state records the producing agy build. The only stable
// discriminator is the SQLite SCHEMA FINGERPRINT -- a sha256 over the database's
// CREATE statements (from sqlite_master) plus user_version. One schema spans
// several agy dot releases, so the fingerprint maps to a release range label.
//
// The fingerprint algorithm is kept byte-for-byte identical to agy-reader's
// agy-format-audit recorder (skills/agy-format-audit/scripts/audit_format.sh):
//
//	{ sqlite3 SELECT sql FROM sqlite_master WHERE sql IS NOT NULL ORDER BY type, name
//	  sqlite3 PRAGMA user_version } | sha256sum
//
// so labels seeded from agy-reader's COMPATIBILITY.md match real databases.

// antigravitySchemaPrefixLen is the number of leading hex characters of the
// full sha256 fingerprint kept for matching and for the unknown-schema marker.
// Short enough to keep labels well under 64 chars, long enough to stay unique.
const antigravitySchemaPrefixLen = 12

// antigravitySchemaVersions maps a short schema-fingerprint prefix to a known
// agy release-range label. Seeded from agy-reader COMPATIBILITY.md and verified
// against real ~/.gemini/antigravity[-cli] databases. Keys are the first
// antigravitySchemaPrefixLen hex chars of the full sha256.
//
// Full fingerprints (for traceability):
//   - sha256:1ca98426f561fe73223c8620a238405030fdb3014444d970e1300a6009f72f43
//     user_version=1, 7 tables (battle_mode_infos, executor_metadata,
//     gen_metadata, parent_references, steps, trajectory_meta,
//     trajectory_metadata_blob), indices idx_steps_status, idx_steps_step_type
//     == agy 1.0.7-1.0.10.
var antigravitySchemaVersions = map[string]string{
	"1ca98426f561": "1.0.7-1.0.10",
}

// antigravitySchemaUnknownPrefix tags an unrecognized schema fingerprint so new
// agy schemas stay visible in source_version (e.g. "agy-schema:abc123def456").
const antigravitySchemaUnknownPrefix = "agy-schema:"

// antigravitySchemaFingerprint computes the full hex sha256 schema fingerprint
// for an open Antigravity SQLite database. It mirrors the agy-reader audit
// recorder byte-for-byte: every non-null sqlite_master sql value ordered by
// (type, name), each followed by a newline, then the decimal user_version
// followed by a newline. Returns ("", err) when the schema cannot be read.
func antigravitySchemaFingerprint(db *sql.DB) (string, error) {
	rows, err := db.Query(
		`SELECT sql FROM sqlite_master ` +
			`WHERE sql IS NOT NULL ORDER BY type, name`,
	)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	h := sha256.New()
	for rows.Next() {
		var stmt string
		if err := rows.Scan(&stmt); err != nil {
			return "", err
		}
		h.Write([]byte(stmt))
		h.Write([]byte{'\n'})
	}
	if err := rows.Err(); err != nil {
		return "", err
	}

	var userVersion int64
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&userVersion); err != nil {
		return "", err
	}
	h.Write([]byte(strconv.FormatInt(userVersion, 10)))
	h.Write([]byte{'\n'})

	return hex.EncodeToString(h.Sum(nil)), nil
}

// antigravitySchemaLabel maps a full hex fingerprint to a short source_version
// label: a known release range, or an "agy-schema:<prefix>" marker for an
// unrecognized (newer) schema. Returns "" for an empty fingerprint so callers
// can leave SourceVersion blank when no schema was available.
func antigravitySchemaLabel(fingerprint string) string {
	if fingerprint == "" {
		return ""
	}
	prefix := fingerprint
	if len(prefix) > antigravitySchemaPrefixLen {
		prefix = prefix[:antigravitySchemaPrefixLen]
	}
	if label, ok := antigravitySchemaVersions[prefix]; ok {
		return label
	}
	return antigravitySchemaUnknownPrefix + prefix
}

// antigravitySourceVersion derives the source_version label for an open
// Antigravity database, shared by the IDE (.db) and CLI (.db) parse paths so
// both classify identically. Returns "" when the schema cannot be read, so a
// sidecar-only or undecodable session leaves SourceVersion empty rather than
// fabricating a label.
func antigravitySourceVersion(db *sql.DB) string {
	fp, err := antigravitySchemaFingerprint(db)
	if err != nil {
		return ""
	}
	return antigravitySchemaLabel(fp)
}

// DecodeConfidence values classify how much trust the schema-fingerprint
// decode of an Antigravity session warrants. They are consumed by the
// session-detail badge (as string literals) and the operator surfaces.
const (
	DecodeConfidenceHigh = "high"
	DecodeConfidenceLow  = "low"
)

// DecodeConfidence reports how much to trust the heuristic decode of an
// Antigravity session, derived purely from its already-computed source_version
// label (see antigravitySchemaLabel). It is the single source of truth for the
// rule, shared by the service layer (session-detail badge), the doctor
// diagnostics, and the sync anomaly counter, so the "agy-schema:" prefix
// knowledge stays in one place next to the label logic.
//
// The agent gate is mandatory: SourceVersion is a generic field that other
// parsers also populate (piebald.go sets "piebald-appdb-v1", commandcode.go
// sets "2"), so a non-empty label alone must never imply confidence. Both the
// IDE agent ("antigravity") and the CLI agent ("antigravity-cli") classify
// identically because both .db parse paths compute the same fingerprint.
//
//   - antigravity / antigravity-cli with an "agy-schema:" label (an
//     unrecognized, newer schema) -> "low": the decode is heuristic and may be
//     incomplete or wrong.
//   - antigravity / antigravity-cli with any other non-empty label (a known
//     release range) -> "high".
//   - empty label, or any other agent -> "" (no value, no badge).
func DecodeConfidence(agent, sourceVersion string) string {
	if agent != string(AgentAntigravity) && agent != string(AgentAntigravityCLI) {
		return ""
	}
	if sourceVersion == "" {
		return ""
	}
	if strings.HasPrefix(sourceVersion, antigravitySchemaUnknownPrefix) {
		return DecodeConfidenceLow
	}
	return DecodeConfidenceHigh
}
