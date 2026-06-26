package artifact

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
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

type peerArtifactSpec struct {
	origin      string
	kind        string
	dir         string
	name        string
	hash        string
	path        string
	contentType string
}

// ListOrigins returns valid origin directories in the artifact store.
func ListOrigins(root string) ([]string, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("%w: artifact root is required", ErrArtifactInvalid)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []string{}, nil
		}
		return nil, err
	}
	origins := make([]string, 0, len(entries))
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		origin := ent.Name()
		if validateOriginID(origin) == nil {
			origins = append(origins, origin)
		}
	}
	sort.Strings(origins)
	return origins, nil
}

// OriginArtifactIndex lists the artifact filenames an origin currently holds,
// grouped by kind. It is the enumeration the HTTP peer transport needs for a
// set-union pull, since metadata events are not referenced by the checkpoint
// and therefore cannot be discovered from it.
type OriginArtifactIndex struct {
	Origin      string   `json:"origin"`
	Checkpoints []string `json:"checkpoints"`
	Manifests   []string `json:"manifests"`
	Segments    []string `json:"segments"`
	Meta        []string `json:"meta"`
	Raw         []string `json:"raw"`
}

// ListArtifacts enumerates the valid artifact filenames an origin holds, grouped
// by kind, skipping temp files and entries that do not match a kind's naming
// rules. Missing kind directories yield empty lists, not errors.
func ListArtifacts(root, origin string) (OriginArtifactIndex, error) {
	if strings.TrimSpace(root) == "" {
		return OriginArtifactIndex{}, fmt.Errorf("%w: artifact root is required", ErrArtifactInvalid)
	}
	if err := validateOriginID(origin); err != nil {
		return OriginArtifactIndex{}, fmt.Errorf("%w: %v", ErrArtifactInvalid, err)
	}
	idx := OriginArtifactIndex{Origin: origin}
	var err error
	if idx.Checkpoints, err = listArtifactKind(root, origin, KindCheckpoints); err != nil {
		return OriginArtifactIndex{}, err
	}
	if idx.Manifests, err = listArtifactKind(root, origin, KindManifests); err != nil {
		return OriginArtifactIndex{}, err
	}
	if idx.Segments, err = listArtifactKind(root, origin, KindSegments); err != nil {
		return OriginArtifactIndex{}, err
	}
	if idx.Meta, err = listArtifactKind(root, origin, KindMeta); err != nil {
		return OriginArtifactIndex{}, err
	}
	if idx.Raw, err = listArtifactKind(root, origin, KindRaw); err != nil {
		return OriginArtifactIndex{}, err
	}
	return idx, nil
}

func listArtifactKind(root, origin, kind string) ([]string, error) {
	dir := filepath.Join(root, origin, kind)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []string{}, nil
		}
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, ent := range entries {
		if ent.IsDir() || isTempArtifactEntry(ent.Name()) {
			continue
		}
		if !validArtifactKindName(kind, ent.Name()) {
			continue
		}
		names = append(names, ent.Name())
	}
	sort.Strings(names)
	return names, nil
}

func validArtifactKindName(kind, name string) bool {
	switch kind {
	case KindCheckpoints:
		return isGCCheckpointName(name)
	case KindManifests:
		return isGCManifestName(name)
	case KindSegments:
		return isGCSegmentName(name)
	case KindRaw:
		return isGCRawName(name)
	case KindMeta:
		_, _, err := normalizeMetadataName(name)
		return err == nil
	default:
		return false
	}
}

// ReadLatestCheckpoint returns the newest checkpoint file for an origin.
func ReadLatestCheckpoint(root, origin string) (PeerArtifact, error) {
	if strings.TrimSpace(root) == "" {
		return PeerArtifact{}, fmt.Errorf("%w: artifact root is required", ErrArtifactInvalid)
	}
	if err := validateOriginID(origin); err != nil {
		return PeerArtifact{}, fmt.Errorf("%w: %v", ErrArtifactInvalid, err)
	}
	originRoot := filepath.Join(root, origin)
	path, err := latestCheckpointPath(originRoot)
	if err != nil {
		return PeerArtifact{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return PeerArtifact{}, ErrArtifactNotFound
		}
		return PeerArtifact{}, err
	}
	if err := validateCheckpointData(data, origin, filepath.Base(path)); err != nil {
		return PeerArtifact{}, err
	}
	return PeerArtifact{
		Origin:      origin,
		Kind:        KindCheckpoints,
		Name:        filepath.Base(path),
		ContentType: "application/json",
		Data:        data,
	}, nil
}

// OriginCheckpointSummary describes the latest checkpoint published by one
// origin. Found is false when the origin has no checkpoint yet.
type OriginCheckpointSummary struct {
	Sequence     int
	SessionCount int
	ModTime      time.Time
	Found        bool
}

// CheckpointSummary returns summary information about an origin's latest
// checkpoint without decoding any session bundles. It is the read side of the
// peers status view.
func CheckpointSummary(root, origin string) (OriginCheckpointSummary, error) {
	if strings.TrimSpace(root) == "" {
		return OriginCheckpointSummary{}, fmt.Errorf("%w: artifact root is required", ErrArtifactInvalid)
	}
	if err := validateOriginID(origin); err != nil {
		return OriginCheckpointSummary{}, fmt.Errorf("%w: %v", ErrArtifactInvalid, err)
	}
	originRoot := filepath.Join(root, origin)
	path, err := latestCheckpointPath(originRoot)
	if err != nil {
		if errors.Is(err, ErrArtifactNotFound) {
			return OriginCheckpointSummary{}, nil
		}
		return OriginCheckpointSummary{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return OriginCheckpointSummary{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return OriginCheckpointSummary{}, err
	}
	var cp checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return OriginCheckpointSummary{}, fmt.Errorf("%w: decoding checkpoint: %v", ErrArtifactInvalid, err)
	}
	return OriginCheckpointSummary{
		Sequence:     cp.Sequence,
		SessionCount: len(cp.Sessions),
		ModTime:      info.ModTime(),
		Found:        true,
	}, nil
}

// ReadArtifact returns one artifact file by kind and name.
func ReadArtifact(root, origin, kind, name string) (PeerArtifact, error) {
	spec, err := makePeerArtifactSpec(root, origin, kind, name)
	if err != nil {
		return PeerArtifact{}, err
	}
	data, err := os.ReadFile(spec.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return PeerArtifact{}, ErrArtifactNotFound
		}
		return PeerArtifact{}, err
	}
	if err := validateArtifactData(spec, data); err != nil {
		return PeerArtifact{}, err
	}
	return PeerArtifact{
		Origin:      spec.origin,
		Kind:        spec.kind,
		Name:        spec.name,
		Hash:        spec.hash,
		ContentType: spec.contentType,
		Data:        data,
	}, nil
}

// WriteArtifact verifies and stores one peer artifact. Existing identical
// content is accepted as a duplicate; existing different content is rejected.
func WriteArtifact(root, origin, kind, name string, data []byte) (PeerArtifactWrite, error) {
	spec, err := makePeerArtifactSpec(root, origin, kind, name)
	if err != nil {
		return PeerArtifactWrite{}, err
	}
	if len(data) == 0 {
		return PeerArtifactWrite{}, fmt.Errorf("%w: artifact body is empty", ErrArtifactInvalid)
	}
	if err := validateArtifactData(spec, data); err != nil {
		return PeerArtifactWrite{}, err
	}
	if duplicate, err := existingArtifactMatches(spec.path, data); err != nil {
		if isArtifactPathConflict(err) {
			return PeerArtifactWrite{}, fmt.Errorf("%w: %v", ErrArtifactConflict, err)
		}
		return PeerArtifactWrite{}, err
	} else if duplicate {
		return PeerArtifactWrite{
			Origin:    spec.origin,
			Kind:      spec.kind,
			Name:      spec.name,
			Hash:      spec.hash,
			Size:      int64(len(data)),
			Duplicate: true,
		}, nil
	}
	if err := writeFileAtomic(spec.path, data, 0o644); err != nil {
		if isArtifactPathConflict(err) {
			return PeerArtifactWrite{}, fmt.Errorf("%w: %v", ErrArtifactConflict, err)
		}
		return PeerArtifactWrite{}, err
	}
	return PeerArtifactWrite{
		Origin: spec.origin,
		Kind:   spec.kind,
		Name:   spec.name,
		Hash:   spec.hash,
		Size:   int64(len(data)),
	}, nil
}

func latestCheckpointPath(originRoot string) (string, error) {
	paths, err := filepath.Glob(filepath.Join(originRoot, "checkpoints", "cp-*.json"))
	if err != nil {
		return "", err
	}
	if len(paths) == 0 {
		return "", ErrArtifactNotFound
	}
	sort.Strings(paths)
	return paths[len(paths)-1], nil
}

func makePeerArtifactSpec(root, origin, kind, name string) (peerArtifactSpec, error) {
	if strings.TrimSpace(root) == "" {
		return peerArtifactSpec{}, fmt.Errorf("%w: artifact root is required", ErrArtifactInvalid)
	}
	if err := validateOriginID(origin); err != nil {
		return peerArtifactSpec{}, fmt.Errorf("%w: %v", ErrArtifactInvalid, err)
	}
	if err := validateArtifactName(name); err != nil {
		return peerArtifactSpec{}, err
	}
	kind = normalizeArtifactKind(kind)
	spec := peerArtifactSpec{
		origin: origin,
		kind:   kind,
	}
	switch kind {
	case KindCheckpoints:
		filename, err := normalizeCheckpointName(name)
		if err != nil {
			return peerArtifactSpec{}, err
		}
		spec.dir = "checkpoints"
		spec.name = filename
		spec.contentType = "application/json"
	case KindManifests:
		filename, hash, err := normalizeHashName(name, manifestExtension)
		if err != nil {
			return peerArtifactSpec{}, err
		}
		spec.dir = "manifests"
		spec.name = filename
		spec.hash = hash
		spec.contentType = "application/zstd"
	case KindSegments:
		filename, hash, err := normalizeHashName(name, segmentExtension)
		if err != nil {
			return peerArtifactSpec{}, err
		}
		spec.dir = "segments"
		spec.name = filename
		spec.hash = hash
		spec.contentType = "application/zstd"
	case KindMeta:
		filename, hash, err := normalizeMetadataName(name)
		if err != nil {
			return peerArtifactSpec{}, err
		}
		spec.dir = "meta"
		spec.name = filename
		spec.hash = hash
		spec.contentType = "application/json"
	case KindRaw:
		if err := validateHashHex(name); err != nil {
			return peerArtifactSpec{}, err
		}
		spec.dir = "raw"
		spec.name = name
		spec.hash = name
		spec.contentType = "application/octet-stream"
	default:
		return peerArtifactSpec{}, fmt.Errorf("%w: unsupported artifact kind %q", ErrArtifactInvalid, kind)
	}
	spec.path = filepath.Join(root, origin, spec.dir, spec.name)
	return spec, nil
}

func normalizeArtifactKind(kind string) string {
	switch strings.TrimSpace(strings.ToLower(kind)) {
	case "checkpoint", "checkpoints":
		return KindCheckpoints
	case "manifest", "manifests":
		return KindManifests
	case "segment", "segments":
		return KindSegments
	case "meta", "metadata":
		return KindMeta
	case "raw":
		return KindRaw
	default:
		return strings.TrimSpace(strings.ToLower(kind))
	}
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

func normalizeHashName(name, extension string) (filename, hash string, err error) {
	hash = strings.TrimSuffix(name, extension)
	if err := validateHashHex(hash); err != nil {
		return "", "", err
	}
	return hash + extension, hash, nil
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

func validateArtifactData(spec peerArtifactSpec, data []byte) error {
	switch spec.kind {
	case KindCheckpoints:
		return validateCheckpointData(data, spec.origin, spec.name)
	case KindManifests:
		return validateManifestArtifactData(data, spec.origin, spec.hash)
	case KindSegments:
		return validateSegmentArtifactData(data, spec.hash)
	case KindMeta:
		return validateMetadataArtifactData(data, spec.origin, spec.name, spec.hash)
	case KindRaw:
		if got := hashHex(data); got != spec.hash {
			return fmt.Errorf("%w: raw artifact hash mismatch: got %s", ErrArtifactInvalid, got)
		}
		return nil
	default:
		return fmt.Errorf("%w: unsupported artifact kind %q", ErrArtifactInvalid, spec.kind)
	}
}

func validateCheckpointData(data []byte, origin, filename string) error {
	var cp checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return fmt.Errorf("%w: decoding checkpoint: %v", ErrArtifactInvalid, err)
	}
	seq, err := checkpointSequence(filename)
	if err != nil {
		return err
	}
	if cp.Version > formatVersion {
		if cp.Origin != origin {
			return fmt.Errorf(
				"%w: checkpoint origin mismatch for %s: got %q",
				ErrArtifactInvalid, origin, cp.Origin,
			)
		}
		if cp.Sequence != seq {
			return fmt.Errorf(
				"%w: checkpoint sequence mismatch: name has %d, body has %d",
				ErrArtifactInvalid, seq, cp.Sequence,
			)
		}
		return nil
	}
	if err := validateCheckpoint(&cp, origin); err != nil {
		return fmt.Errorf("%w: %v", ErrArtifactInvalid, err)
	}
	if cp.Sequence != seq {
		return fmt.Errorf(
			"%w: checkpoint sequence mismatch: name has %d, body has %d",
			ErrArtifactInvalid, seq, cp.Sequence,
		)
	}
	return nil
}

func validateManifestArtifactData(data []byte, origin, hash string) error {
	decoded, err := readCompressedBytes(data)
	if err != nil {
		return fmt.Errorf("%w: decoding manifest compression: %v", ErrArtifactInvalid, err)
	}
	if got := hashHex(decoded); got != hash {
		return fmt.Errorf("%w: manifest hash mismatch: got %s", ErrArtifactInvalid, got)
	}
	var header struct {
		Version int    `json:"v"`
		Origin  string `json:"origin"`
	}
	if err := json.Unmarshal(decoded, &header); err != nil {
		return fmt.Errorf("%w: decoding manifest: %v", ErrArtifactInvalid, err)
	}
	if header.Version > formatVersion {
		if header.Origin != origin {
			return fmt.Errorf("%w: manifest origin mismatch for %s: got %q", ErrArtifactInvalid, origin, header.Origin)
		}
		return nil
	}
	var m manifest
	if err := json.Unmarshal(decoded, &m); err != nil {
		return fmt.Errorf("%w: decoding manifest: %v", ErrArtifactInvalid, err)
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
	return nil
}

func validateSegmentArtifactData(data []byte, hash string) error {
	decoded, err := readCompressedBytes(data)
	if err != nil {
		return fmt.Errorf("%w: decoding segment compression: %v", ErrArtifactInvalid, err)
	}
	if got := hashHex(decoded); got != hash {
		return fmt.Errorf("%w: segment hash mismatch: got %s", ErrArtifactInvalid, got)
	}
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
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
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
	return nil
}

func readCompressedBytes(data []byte) ([]byte, error) {
	dec, err := zstd.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	return io.ReadAll(dec)
}

func isArtifactPathConflict(err error) bool {
	return err != nil && strings.Contains(err.Error(), "artifact path conflict")
}
