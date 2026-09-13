package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"fmt"

	"go.kenn.io/agentsview/internal/rawderive"
)

func (s *MigrationParityStore) NextParitySources(ctx context.Context, lease rawderive.ParityLease, after string, limit int) ([]rawderive.ParitySource, error) {
	if limit < 1 || limit > 128 {
		return nil, fmt.Errorf("invalid migration parity source page")
	}
	tx, err := s.pg.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err = s.checkParityLeaseReadTx(ctx, tx, lease, true); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT source_id,device_id,head_manifest,head_generation,dependency_digest,required
		FROM `+s.quoted+`.migration_parity_sources
		WHERE run_id=$1 AND init_epoch=$2 AND source_kind='raw' AND source_id>$3
		AND evidence_generation<$5 AND NOT candidate_complete AND verdict IS DISTINCT FROM 'stale'
		ORDER BY source_id LIMIT $4`, lease.RunID, lease.InitEpoch, after, limit, lease.RequestGeneration)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sources []rawderive.ParitySource
	for rows.Next() {
		var source rawderive.ParitySource
		var digest []byte
		if err = rows.Scan(&source.ID, &source.Identity.DeviceID, &source.HeadManifestID, &source.HeadGeneration, &digest, &source.Required); err != nil {
			return nil, err
		}
		if len(digest) != len(source.DependencyDigest) {
			return nil, fmt.Errorf("invalid migration parity source dependency")
		}
		copy(source.DependencyDigest[:], digest)
		source.Identity.TenantID = s.tenant
		sources = append(sources, source)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return sources, tx.Commit()
}

func readParityLinkTarget(ctx context.Context, tx *sql.Tx, ownerSource string, link rawderive.ParityLink) (rawderive.ParityLink, error) {
	target, _, err := readHostedLinkTarget(ctx, tx, ownerSource, link.Unresolved)
	if err != nil {
		return link, err
	}
	if target.SourceID != "" {
		link.Target = target
		link.Unresolved = ""
	}
	return link, nil
}

func encodeParityRelationshipLookup(owner rawderive.ParityMemberKey, link rawderive.ParityLink) []byte {
	ownerKey, _ := encodeParityMemberKey(owner)
	var buf bytes.Buffer
	_, _ = buf.WriteString("agentsview-parity-relationship-lookup-v1\x00")
	putParityOpaqueString(&buf, string(ownerKey))
	putParityOpaqueString(&buf, link.Kind)
	_ = binary.Write(&buf, binary.BigEndian, int64(link.Ordinal))
	_ = binary.Write(&buf, binary.BigEndian, int64(link.CallIndex))
	_ = binary.Write(&buf, binary.BigEndian, int64(link.EventIndex))
	putParityOpaqueString(&buf, link.Unresolved)
	return buf.Bytes()
}

func encodeParityRelationshipDependency(owner rawderive.ParityMemberKey, original, resolved rawderive.ParityLink) ([]byte, []byte) {
	lookup := encodeParityRelationshipLookup(owner, original)
	var buf bytes.Buffer
	_, _ = buf.Write(lookup)
	putParityOpaqueString(&buf, resolved.Target.SourceID)
	putParityOpaqueString(&buf, resolved.Target.LogicalKey)
	putParityOpaqueString(&buf, resolved.Target.Kind)
	return lookup, buf.Bytes()
}

func decodeParityRelationshipDependency(encoded []byte, lookup []byte) (rawderive.ParityMemberKey, error) {
	if len(encoded) < len(lookup) || !bytes.Equal(encoded[:len(lookup)], lookup) {
		return rawderive.ParityMemberKey{}, fmt.Errorf("invalid parity relationship dependency")
	}
	reader := bytes.NewReader(encoded[len(lookup):])
	var target rawderive.ParityMemberKey
	var err error
	if target.SourceID, err = readParityOpaqueString(reader); err != nil {
		return rawderive.ParityMemberKey{}, err
	}
	if target.LogicalKey, err = readParityOpaqueString(reader); err != nil {
		return rawderive.ParityMemberKey{}, err
	}
	if target.Kind, err = readParityOpaqueString(reader); err != nil || reader.Len() != 0 {
		return rawderive.ParityMemberKey{}, fmt.Errorf("invalid parity relationship dependency")
	}
	return target, nil
}

func encodeParityOverlayLookup(key rawderive.ParityMemberKey) []byte {
	member, _ := encodeParityMemberKey(key)
	return append([]byte("agentsview-parity-overlay-v1\x00"), member...)
}

func encodeParityOverlayDependency(key rawderive.ParityMemberKey, overlay rawderive.ParityOverlay) ([]byte, []byte) {
	lookup := encodeParityOverlayLookup(key)
	encoded := append([]byte(nil), lookup...)
	for _, value := range []bool{overlay.Excluded, overlay.SourceDeleted, overlay.ProviderExcluded} {
		if value {
			encoded = append(encoded, 1)
		} else {
			encoded = append(encoded, 0)
		}
	}
	return lookup, encoded
}

func (s *MigrationParityStore) PrepareParityGraph(ctx context.Context, lease rawderive.ParityLease, source rawderive.ParitySource, graph rawderive.ParityGraph) (rawderive.ParityGraph, error) {
	if source.ID == "" || graph.Key.SourceID != source.ID {
		return graph, fmt.Errorf("invalid migration parity graph source")
	}
	tx, err := s.pg.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return graph, err
	}
	defer func() { _ = tx.Rollback() }()
	if err = s.checkParityLeaseReadTx(ctx, tx, lease, true); err != nil {
		return graph, err
	}
	for i := range graph.Links {
		link := &graph.Links[i]
		if link.Unresolved == "" {
			continue
		}
		lookup := encodeParityRelationshipLookup(graph.Key, *link)
		keyDigest := sha256.Sum256(lookup)
		var encoded []byte
		err = tx.QueryRowContext(ctx, `SELECT dependency_key FROM `+s.quoted+`.migration_parity_dependencies
			WHERE run_id=$1 AND init_epoch=$2 AND source_id=$3 AND kind='relationship' AND key_digest=$4`, lease.RunID, lease.InitEpoch, source.ID, keyDigest[:]).Scan(&encoded)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return graph, err
		}
		target, decodeErr := decodeParityRelationshipDependency(encoded, lookup)
		if decodeErr != nil {
			return graph, decodeErr
		}
		if target.SourceID != "" {
			link.Target = target
			link.Unresolved = ""
		}
	}
	overlayLookup := encodeParityOverlayLookup(graph.Key)
	overlayDigest := sha256.Sum256(overlayLookup)
	var overlay []byte
	providerExclusionProven := false
	err = tx.QueryRowContext(ctx, `SELECT dependency_key FROM `+s.quoted+`.migration_parity_dependencies
		WHERE run_id=$1 AND init_epoch=$2 AND source_id=$3 AND kind='overlay' AND key_digest=$4`, lease.RunID, lease.InitEpoch, source.ID, overlayDigest[:]).Scan(&overlay)
	if err != nil && err != sql.ErrNoRows {
		return graph, err
	}
	if err == nil {
		if len(overlay) != len(overlayLookup)+3 || !bytes.Equal(overlay[:len(overlayLookup)], overlayLookup) {
			return graph, fmt.Errorf("invalid parity overlay dependency")
		}
		graph.Overlay.Excluded = overlay[len(overlayLookup)] == 1
		graph.Overlay.SourceDeleted = overlay[len(overlayLookup)+1] == 1
		providerExclusionProven = overlay[len(overlayLookup)+2] == 1
	}
	if err = tx.Commit(); err != nil {
		return graph, err
	}
	if graph.Overlay.ProviderExcluded && !providerExclusionProven {
		return graph, rawderive.ErrParityExclusionProvenanceUnavailable
	}
	return graph, nil
}
