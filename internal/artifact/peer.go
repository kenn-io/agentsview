package artifact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

const (
	KindCheckpoints = "checkpoints"
	KindManifests   = "manifests"
	KindSegments    = "segments"
	KindMeta        = "meta"
	KindRaw         = "raw"
)

var (
	ErrArtifactInvalid  = errors.New("invalid artifact")
	ErrArtifactNotFound = errors.New("artifact not found")
	ErrArtifactConflict = errors.New("artifact conflict")
)

// PeerArtifact is one immutable artifact file served through the peer API.
type PeerArtifact struct {
	Origin      string
	Kind        string
	Name        string
	Hash        string
	ContentType string
	Data        []byte
}

// PeerArtifactWrite describes the result of a peer artifact write.
type PeerArtifactWrite struct {
	Origin    string
	Kind      string
	Name      string
	Hash      string
	Size      int64
	Duplicate bool
}

// PeerArtifactSpool is a fully verified, seekable wire response. Close removes
// its private backing file.
type PeerArtifactSpool struct {
	Origin      string
	Kind        string
	Name        string
	Hash        string
	ContentType string
	Size        int64
	File        *os.File
}

// OriginArtifactIndex groups an origin's artifact names by protocol kind.
type OriginArtifactIndex struct {
	Origin      string   `json:"origin"`
	Checkpoints []string `json:"checkpoints"`
	Manifests   []string `json:"manifests"`
	Segments    []string `json:"segments"`
	Meta        []string `json:"meta"`
	Raw         []string `json:"raw"`
}

// OriginCheckpointSummary describes the latest checkpoint published by one origin.
type OriginCheckpointSummary struct {
	Sequence     int
	SessionCount int
	ModTime      time.Time
	Found        bool
}

// OriginCheckpointLanding adds durable local provenance to a checkpoint summary.
type OriginCheckpointLanding struct {
	OriginCheckpointSummary
	LandedSessionCount int
}

func (s *PeerArtifactSpool) Close() error {
	if s == nil || s.File == nil {
		return nil
	}
	file := s.File
	s.File = nil
	return closeAndRemoveTransportSpool(file)
}

func CheckpointLandingStatusFromStore(
	ctx context.Context,
	store ArtifactStore,
	origin string,
	database any,
	isLocal bool,
) (OriginCheckpointLanding, error) {
	if err := ctx.Err(); err != nil {
		return OriginCheckpointLanding{}, err
	}
	if store == nil {
		return OriginCheckpointLanding{}, fmt.Errorf("%w: artifact store is required", ErrArtifactInvalid)
	}
	if err := validateOriginID(origin); err != nil {
		return OriginCheckpointLanding{}, fmt.Errorf("%w: %v", ErrArtifactInvalid, err)
	}
	summary, cp, err := latestStoreCheckpointSummary(ctx, store, origin)
	if err != nil {
		return OriginCheckpointLanding{}, err
	}
	return checkpointLandingFromSummary(ctx, summary, cp, origin, database, isLocal)
}

// CheckpointLandingStatusAtStoreHead reads exactly one provenance-selected
// checkpoint. It never enumerates checkpoint history, so status work is
// bounded by the requested origin page and the selected checkpoint's session
// map. A missing or corrupt selected head is returned to the caller instead of
// silently falling back to older history.
func CheckpointLandingStatusAtStoreHead(
	ctx context.Context,
	store ArtifactStore,
	origin string,
	sequence int,
	expected Identity,
	database any,
	isLocal bool,
) (OriginCheckpointLanding, error) {
	if err := ctx.Err(); err != nil {
		return OriginCheckpointLanding{}, err
	}
	if store == nil || sequence < 1 {
		return OriginCheckpointLanding{}, fmt.Errorf("%w: artifact store and checkpoint sequence are required",
			ErrArtifactInvalid)
	}
	ref, err := NewRef(origin, KindCheckpoints, fmt.Sprintf("cp-%010d.json", sequence))
	if err != nil {
		return OriginCheckpointLanding{}, err
	}
	listed, err := store.Stat(ctx, ref)
	if err != nil {
		return OriginCheckpointLanding{}, err
	}
	if expected.SHA256 != "" && listed.Identity != expected {
		return OriginCheckpointLanding{}, fmt.Errorf("%w: recorded checkpoint identity changed",
			ErrArtifactCorrupt)
	}
	if listed.Identity.Size > checkpointDecodedLimit {
		return OriginCheckpointLanding{}, fmt.Errorf("%w: checkpoint exceeds decoded limit",
			ErrArtifactInvalid)
	}
	opened, reader, err := store.Open(ctx, ref)
	if err != nil {
		return OriginCheckpointLanding{}, err
	}
	if opened != listed {
		_ = reader.Close()
		return OriginCheckpointLanding{}, fmt.Errorf("%w: checkpoint catalog identity changed",
			ErrArtifactCorrupt)
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, checkpointDecodedLimit+1))
	verifyErr := reader.Verify()
	closeErr := reader.Close()
	if err := errors.Join(readErr, verifyErr, closeErr); err != nil {
		return OriginCheckpointLanding{}, fmt.Errorf("%w: reading checkpoint: %v",
			ErrArtifactCorrupt, err)
	}
	if int64(len(data)) != listed.Identity.Size || int64(len(data)) > checkpointDecodedLimit {
		return OriginCheckpointLanding{}, fmt.Errorf("%w: checkpoint size mismatch",
			ErrArtifactCorrupt)
	}
	var cp checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return OriginCheckpointLanding{}, fmt.Errorf("%w: decoding checkpoint: %v",
			ErrArtifactCorrupt, err)
	}
	if err := validateCheckpoint(&cp, origin); err != nil {
		return OriginCheckpointLanding{}, err
	}
	if err := validateCheckpointSequenceIdentity(cp, ref.Name); err != nil {
		return OriginCheckpointLanding{}, err
	}
	summary := OriginCheckpointSummary{
		Sequence: cp.Sequence, SessionCount: len(cp.Sessions),
		ModTime: listed.Modified, Found: true,
	}
	return checkpointLandingFromSummary(ctx, summary, &cp, origin, database, isLocal)
}

func latestStoreCheckpointSummary(
	ctx context.Context, store ArtifactStore, origin string,
) (_ OriginCheckpointSummary, _ *checkpoint, retErr error) {
	iterator, err := openStoreEntryIterator(ctx, store, origin, KindCheckpoints)
	if err != nil {
		return OriginCheckpointSummary{}, nil, err
	}
	defer func() { retErr = errors.Join(retErr, iterator.Close()) }()
	var latest OriginCheckpointSummary
	var latestCheckpoint *checkpoint
	for {
		entries, nextErr := iterator.Next(ctx, checkpointFloorPageSize)
		if nextErr != nil && !errors.Is(nextErr, io.EOF) {
			return OriginCheckpointSummary{}, nil, nextErr
		}
		for _, entry := range entries {
			if entry.Identity.Size > checkpointDecodedLimit {
				continue
			}
			opened, reader, err := store.Open(ctx, entry.Ref)
			if errors.Is(err, ErrArtifactNotFound) || errors.Is(err, ErrArtifactCorrupt) {
				continue
			}
			if err != nil {
				return OriginCheckpointSummary{}, nil, err
			}
			if opened.Ref != entry.Ref || opened.Identity != entry.Identity {
				_ = reader.Close()
				continue
			}
			data, readErr := io.ReadAll(io.LimitReader(reader, checkpointDecodedLimit+1))
			verifyErr := reader.Verify()
			closeErr := reader.Close()
			if err := ctx.Err(); err != nil {
				return OriginCheckpointSummary{}, nil, err
			}
			if readErr != nil || verifyErr != nil || closeErr != nil ||
				int64(len(data)) > checkpointDecodedLimit {
				continue
			}
			var candidate checkpoint
			if err := json.Unmarshal(data, &candidate); err != nil {
				continue
			}
			if err := validateCheckpoint(&candidate, origin); err != nil {
				continue
			}
			if err := validateCheckpointSequenceIdentity(candidate, entry.Ref.Name); err != nil {
				continue
			}
			if !latest.Found || candidate.Sequence > latest.Sequence {
				copy := candidate
				latestCheckpoint = &copy
				latest = OriginCheckpointSummary{
					Sequence: candidate.Sequence, SessionCount: len(candidate.Sessions),
					ModTime: entry.Modified, Found: true,
				}
			}
		}
		if errors.Is(nextErr, io.EOF) {
			return latest, latestCheckpoint, nil
		}
	}
}

func checkpointLandingFromSummary(
	ctx context.Context,
	summary OriginCheckpointSummary,
	cp *checkpoint,
	origin string,
	database any,
	isLocal bool,
) (OriginCheckpointLanding, error) {
	status := OriginCheckpointLanding{OriginCheckpointSummary: summary}
	if cp == nil || len(cp.Sessions) == 0 {
		return status, nil
	}
	if isLocal {
		reader, ok := database.(interface {
			GetArtifactCheckpointHead(context.Context, string) (db.ArtifactCheckpointHead, bool, error)
			StreamArtifactPublications(context.Context, string, func(db.ArtifactPublication) error) (int64, error)
		})
		if !ok {
			return status, nil
		}
		head, found, err := reader.GetArtifactCheckpointHead(ctx, origin)
		if err != nil || !found || head.Sequence != cp.Sequence {
			return status, err
		}
		landed := 0
		revision, err := reader.StreamArtifactPublications(ctx, origin,
			func(publication db.ArtifactPublication) error {
				if cp.Sessions[origin+"~"+publication.SessionID] == publication.ManifestHash {
					landed++
				}
				return nil
			})
		if err != nil || revision != head.PublicationRevision {
			return status, err
		}
		status.LandedSessionCount = landed
		return status, nil
	}
	reader, ok := database.(interface {
		StreamArtifactCheckpointLanding(
			context.Context, string, func(string, string) error,
		) (db.ArtifactCheckpointLanding, bool, error)
	})
	if !ok {
		return status, nil
	}
	landed := 0
	landing, found, err := reader.StreamArtifactCheckpointLanding(ctx, origin,
		func(gid, manifestHash string) error {
			if cp.Sessions[gid] == manifestHash {
				landed++
			}
			return nil
		})
	if err != nil || !found || landing.Sequence != cp.Sequence {
		return status, err
	}
	status.LandedSessionCount = landed
	return status, nil
}

func validateArtifactName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("%w: artifact name is required", ErrArtifactInvalid)
	}
	if strings.Contains(name, "/") || strings.Contains(name, "\\") || strings.Contains(name, "..") {
		return fmt.Errorf("%w: invalid artifact name", ErrArtifactInvalid)
	}
	return nil
}

func normalizeMetadataName(name string) (filename, hash string, err error) {
	base := strings.TrimSuffix(name, metadataEventExtension)
	idx := strings.LastIndex(base, "-")
	if idx < 0 {
		return "", "", fmt.Errorf("%w: metadata artifact missing hash suffix", ErrArtifactInvalid)
	}
	hash = base[idx+1:]
	if err := validateHashHex(hash); err != nil {
		return "", "", err
	}
	return base + metadataEventExtension, hash, nil
}

func normalizeCheckpointName(name string) (string, error) {
	base := strings.TrimSuffix(name, ".json")
	if _, err := checkpointSequence(base + ".json"); err != nil {
		return "", err
	}
	return base + ".json", nil
}

func checkpointSequence(filename string) (int, error) {
	base := strings.TrimSuffix(filename, ".json")
	if len(base) != len("cp-0000000000") || !strings.HasPrefix(base, "cp-") {
		return 0, fmt.Errorf("%w: invalid checkpoint name", ErrArtifactInvalid)
	}
	seq := 0
	for _, r := range base[len("cp-"):] {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("%w: invalid checkpoint name", ErrArtifactInvalid)
		}
		seq = seq*10 + int(r-'0')
	}
	if seq <= 0 {
		return 0, fmt.Errorf("%w: invalid checkpoint sequence", ErrArtifactInvalid)
	}
	return seq, nil
}

func validateHashHex(hash string) error {
	if len(hash) != 64 {
		return fmt.Errorf("%w: invalid artifact hash", ErrArtifactInvalid)
	}
	for _, r := range hash {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return fmt.Errorf("%w: invalid artifact hash", ErrArtifactInvalid)
		}
	}
	return nil
}

func validateCheckpointData(data []byte, origin, filename string) error {
	var cp checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return fmt.Errorf("%w: decoding checkpoint: %v", ErrArtifactInvalid, err)
	}
	if cp.Version > formatVersion {
		if cp.Origin != origin {
			return fmt.Errorf(
				"%w: checkpoint origin mismatch for %s: got %q",
				ErrArtifactInvalid, origin, cp.Origin,
			)
		}
		if err := validateCheckpointSequenceIdentity(cp, filename); err != nil {
			return fmt.Errorf("%w: %v", ErrArtifactInvalid, err)
		}
		if err := validateCheckpointReferences(&cp, origin); err != nil {
			return fmt.Errorf("%w: %v", ErrArtifactInvalid, err)
		}
		return nil
	}
	if err := validateCheckpoint(&cp, origin); err != nil {
		return fmt.Errorf("%w: %v", ErrArtifactInvalid, err)
	}
	if err := validateCheckpointSequenceIdentity(cp, filename); err != nil {
		return fmt.Errorf("%w: %v", ErrArtifactInvalid, err)
	}
	return nil
}

func validateCheckpointSequenceIdentity(cp checkpoint, filename string) error {
	seq, err := checkpointSequence(filename)
	if err != nil {
		return err
	}
	if cp.Sequence != seq {
		return fmt.Errorf(
			"checkpoint sequence mismatch: name has %d, body has %d",
			seq, cp.Sequence,
		)
	}
	return nil
}

func validateCanonicalManifestArtifactData(decoded []byte, origin string) error {
	m, err := decodeManifestWithLimits(decoded, productionArtifactLimits())
	if err != nil {
		return fmt.Errorf("%w: decoding manifest: %v", ErrArtifactInvalid, err)
	}
	if m.Version > formatVersion {
		if m.Origin != origin {
			return fmt.Errorf("%w: manifest origin mismatch for %s: got %q", ErrArtifactInvalid, origin, m.Origin)
		}
		return nil
	}
	if m.Origin != origin {
		return fmt.Errorf("%w: manifest origin mismatch for %s: got %q", ErrArtifactInvalid, origin, m.Origin)
	}
	if m.NativeSessionID == "" || m.Session.ID != m.NativeSessionID || m.Session.Machine != origin {
		return fmt.Errorf("%w: manifest session identity mismatch", ErrArtifactInvalid)
	}
	if len(m.Segments) == 0 {
		return fmt.Errorf("%w: manifest has no message segments", ErrArtifactInvalid)
	}
	if err := validateManifestReferences(m); err != nil {
		return fmt.Errorf("%w: %v", ErrArtifactInvalid, err)
	}
	return nil
}

func validateCanonicalSegmentArtifactData(decoded []byte) error {
	if _, err := decodeSegment(decoded); err != nil {
		if errors.Is(err, errFutureArtifactVersion) {
			if ferr := validateFutureSegmentData(decoded); ferr != nil {
				return fmt.Errorf("%w: decoding segment: %v", ErrArtifactInvalid, ferr)
			}
			return nil
		}
		return fmt.Errorf("%w: decoding segment: %v", ErrArtifactInvalid, err)
	}
	return nil
}

func validateFutureSegmentData(data []byte) error {
	records, err := segmentRecords(data, maxSegmentMessages)
	if err != nil {
		return err
	}
	for _, line := range records {
		var record struct {
			Version int `json:"v"`
		}
		if err := json.Unmarshal(line, &record); err != nil {
			return err
		}
		if record.Version <= formatVersion {
			return fmt.Errorf("message segment has unsupported artifact version %d", record.Version)
		}
	}
	return nil
}

func validateMetadataArtifactData(data []byte, origin, filename, hash string) error {
	if got := hashHex(data); got != hash {
		return fmt.Errorf("%w: metadata artifact hash mismatch: got %s", ErrArtifactInvalid, got)
	}
	var envelope metadataEventEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("%w: decoding metadata event: %v", ErrArtifactInvalid, err)
	}
	if envelope.Version > formatVersion {
		return nil
	}
	base := strings.TrimSuffix(filename, metadataEventExtension)
	hlc := strings.TrimSuffix(base, "-"+hash)
	var event metadataEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return fmt.Errorf("%w: decoding metadata event: %v", ErrArtifactInvalid, err)
	}
	art := metadataArtifact{
		path:  filename,
		hlc:   hlc,
		hash:  hash,
		event: event,
	}
	if err := validateMetadataArtifactEvent(art, origin); err != nil {
		if errors.Is(err, errFutureArtifactVersion) {
			return nil
		}
		return fmt.Errorf("%w: %v", ErrArtifactInvalid, err)
	}
	// Unknown ops stay accepted for forward compatibility (replay marks
	// them applied and skips them), but a known op must carry a payload
	// that projects cleanly or replay could never apply it.
	if err := validateMetadataOp(event.Op); err == nil {
		if _, _, _, _, err := metadataProjectionFields(event); err != nil {
			return fmt.Errorf("%w: metadata event payload: %v", ErrArtifactInvalid, err)
		}
	}
	return nil
}
