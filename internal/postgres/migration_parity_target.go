package postgres

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"hash"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"go.kenn.io/agentsview/internal/rawderive"
)

// ParityTargetConfigDigest binds a named, provisioned target to the effective
// endpoint, database, schema, tenant and TLS policy parsed by the PG driver.
// Authentication material and unrelated connection parameters are excluded.
func ParityTargetConfigDigest(dsn, schema, tenant, targetName, targetID string, allowInsecure bool) (rawderive.ParityDigest, error) {
	id, idErr := uuid.Parse(targetID)
	if idErr != nil || id.String() != targetID || schema == "" || tenant == "" || targetName == "" {
		return rawderive.ParityDigest{}, fmt.Errorf("invalid migration parity target configuration")
	}
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return rawderive.ParityDigest{}, fmt.Errorf("invalid migration parity target configuration")
	}
	if cfg.TLSConfig != nil && cfg.TLSConfig.RootCAs != nil {
		return rawderive.ParityDigest{}, fmt.Errorf("unsupported migration parity custom trust configuration")
	}
	for _, fallback := range cfg.Fallbacks {
		if fallback.TLSConfig != nil && fallback.TLSConfig.RootCAs != nil {
			return rawderive.ParityDigest{}, fmt.Errorf("unsupported migration parity custom trust configuration")
		}
	}
	h := sha256.New()
	parityTargetField(h, "agentsview-parity-target-v1")
	parityTargetField(h, targetName)
	parityTargetField(h, targetID)
	parityTargetField(h, cfg.Database)
	parityTargetField(h, schema)
	parityTargetField(h, tenant)
	parityTargetBool(h, allowInsecure)
	parityTargetField(h, cfg.SSLNegotiation)
	parityTargetEndpoint(h, cfg.Host, cfg.Port, cfg.TLSConfig)
	for _, fallback := range cfg.Fallbacks {
		parityTargetEndpoint(h, fallback.Host, fallback.Port, fallback.TLSConfig)
	}
	var digest rawderive.ParityDigest
	copy(digest[:], h.Sum(nil))
	return digest, nil
}

func parityTargetEndpoint(h hash.Hash, host string, port uint16, tlsConfig *tls.Config) {
	if host != "" && !strings.HasPrefix(host, "/") {
		host = strings.ToLower(host)
	}
	parityTargetField(h, host)
	var encoded [2]byte
	binary.BigEndian.PutUint16(encoded[:], port)
	_, _ = h.Write(encoded[:])
	if tlsConfig == nil {
		parityTargetField(h, "plaintext")
		return
	}
	parityTargetField(h, "tls")
	parityTargetBool(h, tlsConfig.InsecureSkipVerify)
	parityTargetField(h, strings.ToLower(tlsConfig.ServerName))
	parityTargetBool(h, tlsConfig.VerifyPeerCertificate != nil)
	parityTargetBool(h, tlsConfig.VerifyConnection != nil)
	binary.BigEndian.PutUint16(encoded[:], tlsConfig.MinVersion)
	_, _ = h.Write(encoded[:])
	binary.BigEndian.PutUint16(encoded[:], tlsConfig.MaxVersion)
	_, _ = h.Write(encoded[:])
}

func parityTargetField(h hash.Hash, value string) {
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(value)))
	_, _ = h.Write(size[:])
	_, _ = h.Write([]byte(value))
}

func parityTargetBool(h hash.Hash, value bool) {
	if value {
		_, _ = h.Write([]byte{1})
	} else {
		_, _ = h.Write([]byte{0})
	}
}
