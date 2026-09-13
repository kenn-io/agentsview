package postgres

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"

	"go.kenn.io/agentsview/internal/rawderive"
)

func encodeParityHistoryEntry(entry rawderive.ParityHistoryEntry) []byte {
	var buf bytes.Buffer
	_, _ = buf.WriteString("agentsview-parity-history-entry-v1\x00")
	putParityOpaqueString(&buf, entry.ManifestID)
	putParityOpaqueString(&buf, entry.Receipt)
	putParityOpaqueString(&buf, entry.ParentReceipt)
	_ = binary.Write(&buf, binary.BigEndian, entry.Generation)
	return buf.Bytes()
}

func decodeParityHistoryEntry(encoded []byte) (rawderive.ParityHistoryEntry, error) {
	const prefix = "agentsview-parity-history-entry-v1\x00"
	if len(encoded) < len(prefix) || string(encoded[:len(prefix)]) != prefix {
		return rawderive.ParityHistoryEntry{}, fmt.Errorf("invalid parity history dependency")
	}
	reader := bytes.NewReader(encoded[len(prefix):])
	var entry rawderive.ParityHistoryEntry
	var err error
	if entry.ManifestID, err = readParityOpaqueString(reader); err != nil {
		return rawderive.ParityHistoryEntry{}, err
	}
	if entry.Receipt, err = readParityOpaqueString(reader); err != nil {
		return rawderive.ParityHistoryEntry{}, err
	}
	if entry.ParentReceipt, err = readParityOpaqueString(reader); err != nil {
		return rawderive.ParityHistoryEntry{}, err
	}
	if err = binary.Read(reader, binary.BigEndian, &entry.Generation); err != nil || reader.Len() != 0 {
		return rawderive.ParityHistoryEntry{}, fmt.Errorf("invalid parity history dependency")
	}
	return entry, nil
}

func encodeParityHeadDependency(sourceID, manifestID string, generation int64, receipt string) []byte {
	var buf bytes.Buffer
	_, _ = buf.WriteString("agentsview-parity-head-v1\x00")
	putParityOpaqueString(&buf, sourceID)
	putParityOpaqueString(&buf, manifestID)
	_ = binary.Write(&buf, binary.BigEndian, generation)
	putParityOpaqueString(&buf, receipt)
	return buf.Bytes()
}

func (s *MigrationParityStore) NextParityHistory(ctx context.Context, lease rawderive.ParityLease, sourceID string, afterGeneration int64, limit int) ([]rawderive.ParityHistoryEntry, error) {
	if sourceID == "" || afterGeneration < 0 || limit < 1 || limit > 128 {
		return nil, fmt.Errorf("invalid migration parity history page")
	}
	tx, err := s.pg.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err = s.checkParityLeaseReadTx(ctx, tx, lease, true); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT dependency_key FROM `+s.quoted+`.migration_parity_dependencies
		WHERE run_id=$1 AND init_epoch=$2 AND source_id=$3 AND kind='manifest' AND sequence>$4 ORDER BY sequence LIMIT $5`, lease.RunID, lease.InitEpoch, sourceID, afterGeneration, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var history []rawderive.ParityHistoryEntry
	for rows.Next() {
		var encoded []byte
		if err = rows.Scan(&encoded); err != nil {
			return nil, err
		}
		entry, decodeErr := decodeParityHistoryEntry(encoded)
		if decodeErr != nil {
			return nil, decodeErr
		}
		history = append(history, entry)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return history, tx.Commit()
}
