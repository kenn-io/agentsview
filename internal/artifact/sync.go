package artifact

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

const (
	formatVersion           = 1
	originStateKey          = "artifact_origin_id"
	importStatePrefix       = "artifact_import:"
	exportStatePrefix       = "artifact_export:"
	exportSourceStatePrefix = "artifact_export_source:"
	tempFilePrefix          = ".tmp-"
	segmentTargetSize       = int64(32 << 20)

	// Cardinality caps complement the byte caps: 4,096 records keeps one
	// segment's decoded object graph bounded, while 32,768 records and 256 MiB
	// leave ample room for unusually long sessions without letting many valid
	// chunks amplify during aggregation. Sixteen references accommodate uneven
	// 32 MiB chunks; the aggregate byte cap remains the final session bound.
	maxManifestSegments    = 16
	maxManifestUsageEvents = 32_768
	maxSegmentMessages     = 4_096
	maxSessionMessages     = 32_768
	maxSessionDecodedBytes = int64(256 << 20)

	// Nested collections need independent caps because compact empty objects can
	// amplify far beyond the decoded byte budget when unmarshaled. A message may
	// still describe unusually wide tool fan-out, and one tool may retain a long
	// result history. Segment totals keep one decoded chunk modest; session totals
	// allow eight full nested-budget segments, matching the message-count ratio.
	maxMessageToolCalls    = 256
	maxToolResultEvents    = 1_024
	maxSegmentToolCalls    = 8_192
	maxSegmentResultEvents = 32_768
	maxSessionToolCalls    = 65_536
	maxSessionResultEvents = 262_144
)

const (
	checkpointFloorPageSize = 128
	checkpointDecodedLimit  = int64(64 << 20)
	artifactImportPageSize  = 128
)

type artifactCheckpointSequenceDB interface {
	GetArtifactCheckpointFloor(context.Context, string) (int, bool, error)
	ReserveArtifactCheckpointSequence(context.Context, string, int) (int, error)
}

type checkpointFloorStore interface {
	checkpointFloor(context.Context, string) (int, error)
}

// artifactLimits bounds decoded collection cardinality in addition to raw
// bytes. The production values are intentionally generous for real sessions
// while preventing small JSON records from amplifying into unbounded Go
// object graphs.
type artifactLimits struct {
	manifestSegments    int
	manifestUsageEvents int
	segmentMessages     int
	sessionMessages     int
	sessionDecodedBytes int64
	messageToolCalls    int
	toolResultEvents    int
	segmentToolCalls    int
	segmentResultEvents int
	sessionToolCalls    int
	sessionResultEvents int
}

func productionArtifactLimits() artifactLimits {
	return artifactLimits{
		manifestSegments:    maxManifestSegments,
		manifestUsageEvents: maxManifestUsageEvents,
		segmentMessages:     maxSegmentMessages,
		sessionMessages:     maxSessionMessages,
		sessionDecodedBytes: maxSessionDecodedBytes,
		messageToolCalls:    maxMessageToolCalls,
		toolResultEvents:    maxToolResultEvents,
		segmentToolCalls:    maxSegmentToolCalls,
		segmentResultEvents: maxSegmentResultEvents,
		sessionToolCalls:    maxSessionToolCalls,
		sessionResultEvents: maxSessionResultEvents,
	}
}

type nestedCollectionCounts struct {
	toolCalls    int
	resultEvents int
}

type segmentPreflight struct {
	records [][]byte
	nested  nestedCollectionCounts
}

func exceedsCollectionLimit(current, additional, limit int) bool {
	return current > limit || additional > limit-current
}

var errIncompleteArtifact = errors.New("incomplete artifact")

var errFutureArtifactVersion = errors.New("future artifact version")

// SyncOptions configures a local-first artifact folder sync.
type SyncOptions struct {
	DataDir string
	Target  string
	Origin  string
	// Now is the wall-clock source for advancing the metadata HLC past
	// observed remote events. When nil, time.Now is used. Sharing it with the
	// local metadata recorder keeps import and local edits on one time base.
	Now func() time.Time
	// Token is the Bearer token for an HTTP peer target. It is ignored by
	// folder and object-store targets.
	Token string
	// AllowInsecure permits plaintext HTTP to a non-loopback peer. Loopback
	// HTTP remains allowed without this override.
	AllowInsecure bool
	// BaselineMetadata writes metadata events for existing local curation before
	// exchanging artifacts. It is intended for first-time initialization.
	BaselineMetadata bool
	// OnDataChanged is called after a foreign import writes local rows.
	OnDataChanged func()
}

// Sync runs one artifact sync, selecting the transport from the target shape:
// an http(s):// URL uses the HTTP peer transport, anything else is treated as a
// local folder target.
func Sync(ctx context.Context, database *db.DB, opts SyncOptions) (SyncResult, error) {
	if IsFolderTarget(opts.Target) {
		return syncFolderWithOwnedStore(ctx, database, opts)
	}
	tr, err := syncTransport(nil, opts)
	if err != nil {
		return SyncResult{}, err
	}
	return syncWithTransport(ctx, database, opts, tr)
}

// SyncWithStore runs one exchange through a caller-owned artifact store. The
// store remains open when the exchange returns.
func SyncWithStore(
	ctx context.Context, database *db.DB, store ArtifactStore, opts SyncOptions,
) (SyncResult, error) {
	if store == nil {
		return SyncResult{}, errors.New("artifact store is required")
	}
	tr, err := syncTransport(nil, opts)
	if err != nil {
		return SyncResult{}, err
	}
	return syncContentWithTransport(ctx, database, nil, store, nil, opts, tr)
}

// SyncWithRepository runs one exchange through a caller-owned local
// repository, including folder-target identity validation and batch packing.
// The repository remains open when the exchange returns.
func SyncWithRepository(
	ctx context.Context, database *db.DB, repository *Repository, opts SyncOptions,
) (SyncResult, error) {
	if repository == nil || repository.Closed() {
		return SyncResult{}, errors.New("artifact repository is required")
	}
	tr, err := syncTransport(repository, opts)
	if err != nil {
		return SyncResult{}, err
	}
	return syncRepositoryWithTransport(ctx, database, repository, opts, tr)
}

func syncTransport(repository *Repository, opts SyncOptions) (Transport, error) {
	if err := ValidateSyncTarget(opts.Target); err != nil {
		return nil, err
	}
	if IsHTTPTarget(opts.Target) {
		tr, err := newHTTPTransport(opts.Target, opts.Token, opts.AllowInsecure)
		if err != nil {
			return nil, err
		}
		return tr, nil
	}
	if IsObjectTarget(opts.Target) {
		tr, err := newObjectTransport(opts.Target, ObjectStoreOptionsFromEnv())
		if err != nil {
			return nil, err
		}
		return tr, nil
	}
	if repository == nil {
		return nil, fmt.Errorf(
			"%w: folder exchange requires retained repository identity",
			ErrArtifactUnsupported,
		)
	}
	transport, err := repository.NewFolderTransport(opts.Target)
	if err != nil {
		return nil, err
	}
	return transport, nil
}

// ValidateSyncTarget rejects URL components that can carry credentials or be
// confused with transport-owned API paths. Folder targets are left untouched.
func ValidateSyncTarget(target string) error {
	if target == "" {
		return fmt.Errorf("%w: artifact sync target is required", ErrArtifactInvalid)
	}
	if !IsHTTPTarget(target) && !IsObjectTarget(target) {
		return nil
	}
	u, err := url.Parse(target)
	if err != nil || u == nil || u.Host == "" {
		return fmt.Errorf("%w: artifact sync URL is invalid", ErrArtifactInvalid)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%w: artifact sync URL must not contain credentials, query, or fragment",
			ErrArtifactInvalid)
	}
	return nil
}

// SyncResult summarizes a folder artifact sync run.
type SyncResult struct {
	Origin           string
	ExportedSessions int
	ImportedSessions int
	ImportedMessages int
	ImportedMetadata int
}

// ImportResult summarizes local rows changed by artifact import.
type ImportResult struct {
	Sessions int
	Messages int
	Metadata int
	Deferred int
}

// Changed reports whether the import wrote user-visible local data.
func (r ImportResult) Changed() bool {
	return r.Sessions > 0 || r.Messages > 0 || r.Metadata > 0
}

// SyncFolder exports local sessions to the local artifact store, exchanges the
// store with target, and imports foreign origins from the exchanged artifacts.
func SyncFolder(ctx context.Context, database *db.DB, opts SyncOptions) (SyncResult, error) {
	return Sync(ctx, database, opts)
}

func syncFolderWithOwnedStore(
	ctx context.Context, database *db.DB, opts SyncOptions,
) (_ SyncResult, retErr error) {
	if opts.DataDir == "" {
		return SyncResult{}, errors.New("artifact sync data dir is required")
	}
	repository, err := OpenRepository(ctx, opts.DataDir)
	if err != nil {
		return SyncResult{}, fmt.Errorf("opening artifact repository: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, repository.Close()) }()
	transport, err := syncTransport(repository, opts)
	if err != nil {
		return SyncResult{}, err
	}
	return syncRepositoryWithTransport(ctx, database, repository, opts, transport)
}

// syncWithTransport runs one artifact sync over any transport: export local
// sessions, exchange the store with the remote via set-union, then import
// foreign origins. Folder, HTTP peer, and object-store targets differ only in
// the transport's Prepare and Exchange.
func syncWithTransport(
	ctx context.Context,
	database *db.DB,
	opts SyncOptions,
	tr Transport,
) (_ SyncResult, retErr error) {
	if opts.DataDir == "" {
		return SyncResult{}, errors.New("artifact sync data dir is required")
	}
	repository, err := OpenRepository(ctx, opts.DataDir)
	if err != nil {
		return SyncResult{}, fmt.Errorf("opening artifact repository: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, repository.Close()) }()
	return syncRepositoryWithTransport(ctx, database, repository, opts, tr)
}

func syncRepositoryWithTransport(
	ctx context.Context,
	database *db.DB,
	repository *Repository,
	opts SyncOptions,
	tr Transport,
) (SyncResult, error) {
	return syncContentWithTransport(
		ctx, database, repository, repository.Content(), repository.NotifyBatch, opts, tr,
	)
}

func syncContentWithTransport(
	ctx context.Context,
	database *db.DB,
	repository *Repository,
	localStore ArtifactStore,
	notifyBatch func(context.Context),
	opts SyncOptions,
	tr Transport,
) (_ SyncResult, retErr error) {
	notificationCtx := ctx
	defer func() {
		if retErr == nil && notifyBatch != nil {
			notifyBatch(notificationCtx)
		}
	}()
	if closer, ok := tr.(io.Closer); ok {
		defer func() { retErr = errors.Join(retErr, closer.Close()) }()
	}
	ctx = SuppressArtifactMaintenance(ctx)
	if err := tr.Prepare(ctx, localStore); err != nil {
		return SyncResult{}, err
	}
	origin := opts.Origin
	if origin == "" {
		var err error
		origin, err = EnsureOrigin(database)
		if err != nil {
			return SyncResult{}, err
		}
	} else if err := validateOriginID(origin); err != nil {
		return SyncResult{}, err
	}
	coordinator := NewStoreImportCoordinator(database, localStore, origin)
	coordinator.now = opts.Now
	transportStore := newCoordinatedTransportStore(database, localStore, coordinator)

	var imported ImportResult
	var baselineSnapshot db.MetadataBaselineSnapshot
	if opts.BaselineMetadata {
		var err error
		baselineSnapshot, err = database.MetadataBaselineSnapshot(ctx)
		if err != nil {
			return SyncResult{}, err
		}
		if err := tr.Exchange(ctx, transportStore); err != nil {
			return SyncResult{}, err
		}
		preBaselineImported, err := coordinator.Finalize(ctx)
		if err != nil {
			return SyncResult{}, err
		}
		imported.Sessions += preBaselineImported.Sessions
		imported.Messages += preBaselineImported.Messages
		imported.Metadata += preBaselineImported.Metadata

		recorder := NewMetadataRecorder(database, MetadataRecorderOptions{
			Origin: origin,
			Store:  localStore,
			Now:    opts.Now,
		})
		if _, err := recorder.AppendBaselineSnapshot(ctx, baselineSnapshot); err != nil {
			return SyncResult{}, err
		}
	}
	var (
		exported ExportResult
		err      error
	)
	if repository != nil {
		exported, err = PublishRepositoryArtifacts(ctx, database, repository, ExportOptions{
			Origin: origin,
		})
	} else {
		exported, err = ExportToStore(ctx, database, localStore, ExportOptions{
			Origin: origin,
		})
	}
	if err != nil {
		return SyncResult{}, err
	}
	if err := tr.Exchange(ctx, transportStore); err != nil {
		return SyncResult{}, err
	}
	postExportImported, err := coordinator.Finalize(ctx)
	if err != nil {
		return SyncResult{}, err
	}
	imported.Sessions += postExportImported.Sessions
	imported.Messages += postExportImported.Messages
	imported.Metadata += postExportImported.Metadata
	if imported.Changed() && opts.OnDataChanged != nil {
		opts.OnDataChanged()
	}
	return SyncResult{
		Origin:           origin,
		ExportedSessions: exported.ExportedSessions,
		ImportedSessions: imported.Sessions,
		ImportedMessages: imported.Messages,
		ImportedMetadata: imported.Metadata,
	}, nil
}

// EnsureOrigin returns the persisted origin ID, creating one when absent.
func EnsureOrigin(database *db.DB) (string, error) {
	origin, err := StoredOrigin(database)
	if err != nil {
		return "", err
	}
	if origin != "" {
		return origin, nil
	}
	origin, err = newOriginID()
	if err != nil {
		return "", err
	}
	if err := validateOriginID(origin); err != nil {
		return "", fmt.Errorf("generated artifact origin: %w", err)
	}
	if err := database.SetSyncState(originStateKey, origin); err != nil {
		return "", fmt.Errorf("persisting artifact origin: %w", err)
	}
	return origin, nil
}

// AdoptOrigin persists origin as this machine's artifact origin in the database
// sync state so DB-derived lookups (EnsureOrigin and its callers) agree with the
// authoritative config origin. It validates the input and is idempotent: it only
// writes when the stored value differs. The config origin always wins, so a
// previously stored value is overwritten to converge on a single origin.
func AdoptOrigin(database *db.DB, origin string) error {
	if err := validateOriginID(origin); err != nil {
		return fmt.Errorf("adopting artifact origin: %w", err)
	}
	existing, err := StoredOrigin(database)
	if err != nil {
		return err
	}
	if existing == origin {
		return nil
	}
	if err := database.SetSyncState(originStateKey, origin); err != nil {
		return fmt.Errorf("persisting artifact origin: %w", err)
	}
	return nil
}

// StoredOrigin returns the persisted origin ID without creating one.
func StoredOrigin(database *db.DB) (string, error) {
	origin, err := database.GetSyncState(originStateKey)
	if err != nil {
		return "", fmt.Errorf("reading artifact origin: %w", err)
	}
	if origin != "" {
		if err := validateOriginID(origin); err != nil {
			return "", fmt.Errorf("stored artifact origin: %w", err)
		}
		return origin, nil
	}
	return "", nil
}

type syncStateValueReader interface {
	SyncStateValues(keys []string) (map[string]string, error)
}

// ImportedSessionIDs returns the candidate session IDs with durable artifact
// import provenance. A foreign machine~id shape is shared by other import
// mechanisms, so callers must query the exact provenance keys rather than
// infer artifact ownership from the session row or scan all historical imports.
func ImportedSessionIDs(
	database syncStateValueReader, candidateIDs []string,
) (map[string]struct{}, error) {
	ids := make(map[string]struct{})
	if len(candidateIDs) == 0 {
		return ids, nil
	}
	keys := make([]string, 0, len(candidateIDs))
	keyToID := make(map[string]string, len(candidateIDs))
	for _, gid := range candidateIDs {
		origin, nativeID, ok := strings.Cut(gid, "~")
		if !ok || origin == "" || nativeID == "" {
			continue
		}
		key := importStateKey(origin, gid)
		keys = append(keys, key)
		keyToID[key] = gid
	}
	if len(keys) == 0 {
		return ids, nil
	}
	states, err := database.SyncStateValues(keys)
	if err != nil {
		return nil, fmt.Errorf("reading artifact import provenance: %w", err)
	}
	for key := range states {
		if gid, ok := keyToID[key]; ok {
			ids[gid] = struct{}{}
		}
	}
	return ids, nil
}

func newOriginID() (string, error) {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		host = "machine"
	}
	host = sanitizeOriginPart(host)
	if host == "" || host == "local" {
		host = "machine"
	}
	var suffix [3]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("generating artifact origin suffix: %w", err)
	}
	return fmt.Sprintf("%s-%s", host, hex.EncodeToString(suffix[:])), nil
}

func validateOriginID(origin string) error {
	return config.ValidateArtifactOriginID(origin)
}

func validateDisjointRoots(localRoot, target string) error {
	localAbs, err := filepath.Abs(localRoot)
	if err != nil {
		return fmt.Errorf("resolving local artifact store: %w", err)
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return fmt.Errorf("resolving artifact sync target: %w", err)
	}
	localAbs = filepath.Clean(localAbs)
	targetAbs = filepath.Clean(targetAbs)
	localCanonical, err := canonicalArtifactPath(localAbs)
	if err != nil {
		return fmt.Errorf("resolving local artifact store symlinks: %w", err)
	}
	targetCanonical, err := canonicalArtifactPath(targetAbs)
	if err != nil {
		return fmt.Errorf("resolving artifact sync target symlinks: %w", err)
	}
	if rootsOverlap(localAbs, targetAbs) || rootsOverlap(localCanonical, targetCanonical) {
		return fmt.Errorf(
			"artifact sync target %s must not overlap local artifact store %s",
			targetCanonical, localCanonical,
		)
	}
	return nil
}

func canonicalArtifactPath(path string) (string, error) {
	missing := make([]string, 0, 2)
	current := path
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for _, part := range slices.Backward(missing) {
				resolved = filepath.Join(resolved, part)
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func rootsOverlap(a, b string) bool {
	return a == b || pathContains(a, b) || pathContains(b, a)
}

func pathContains(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func sanitizeOriginPart(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

type checkpoint struct {
	Version  int               `json:"v"`
	Origin   string            `json:"origin"`
	Sequence int               `json:"seq"`
	Sessions map[string]string `json:"sessions"`
}

type manifest struct {
	Version         int                  `json:"v"`
	Origin          string               `json:"origin"`
	NativeSessionID string               `json:"native_session_id"`
	Session         manifestSession      `json:"session"`
	SessionName     *string              `json:"session_name,omitempty"`
	Segments        []string             `json:"segments"`
	UsageEvents     []artifactUsageEvent `json:"usage_events,omitempty"`
	RawSource       *rawSourceRef        `json:"raw_source,omitempty"`
	DataVersion     int                  `json:"data_version"`
	Generation      int                  `json:"generation"`
	// Signal state persisted on the session row but absent from the wire
	// Session above, which mirrors only db.Session's JSON-visible fields.
	// Carried explicitly so an imported session keeps its tool-call, context,
	// and quality signal state instead of resetting to false/zero. Secret-scan
	// state is deliberately not carried: findings live outside the manifest,
	// so imported sessions are treated as unscanned (see rewriteForImport).
	SessionHasToolCalls   bool                    `json:"session_has_tool_calls,omitempty"`
	SessionHasContextData bool                    `json:"session_has_context_data,omitempty"`
	SessionQualitySignals *manifestQualitySignals `json:"session_quality_signals,omitempty"`
}

type artifactUsageEvent struct {
	MessageOrdinal           *int     `json:"message_ordinal,omitempty"`
	Source                   string   `json:"source"`
	Model                    string   `json:"model"`
	InputTokens              int      `json:"input_tokens,omitempty"`
	OutputTokens             int      `json:"output_tokens,omitempty"`
	CacheCreationInputTokens int      `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int      `json:"cache_read_input_tokens,omitempty"`
	ReasoningTokens          int      `json:"reasoning_tokens,omitempty"`
	CostUSD                  *float64 `json:"cost_usd,omitempty"`
	CostStatus               string   `json:"cost_status,omitempty"`
	CostSource               string   `json:"cost_source,omitempty"`
	OccurredAt               string   `json:"occurred_at,omitempty"`
	DedupKey                 string   `json:"dedup_key,omitempty"`
}

type rawSourceRef struct {
	Hash      string `json:"hash"`
	Size      int64  `json:"size"`
	MediaType string `json:"media_type,omitempty"`
	Path      string `json:"path,omitempty"`
}

type metadataEvent struct {
	Version    int             `json:"v"`
	HLC        string          `json:"hlc"`
	Origin     string          `json:"origin"`
	SessionGID string          `json:"session_gid"`
	Op         string          `json:"op"`
	Value      json.RawMessage `json:"value,omitempty"`
	Pin        *MetadataPin    `json:"pin,omitempty"`
}

type segmentMessage struct {
	Version           int               `json:"v"`
	Ordinal           int               `json:"ordinal"`
	Role              string            `json:"role"`
	Content           string            `json:"content"`
	ThinkingText      string            `json:"thinking_text,omitempty"`
	Timestamp         string            `json:"timestamp,omitempty"`
	HasThinking       bool              `json:"has_thinking,omitempty"`
	HasToolUse        bool              `json:"has_tool_use,omitempty"`
	ContentLength     int               `json:"content_length,omitempty"`
	Model             string            `json:"model,omitempty"`
	TokenUsage        json.RawMessage   `json:"token_usage,omitempty"`
	ContextTokens     int               `json:"context_tokens,omitempty"`
	OutputTokens      int               `json:"output_tokens,omitempty"`
	HasContextTokens  bool              `json:"has_context_tokens,omitempty"`
	HasOutputTokens   bool              `json:"has_output_tokens,omitempty"`
	ClaudeMessageID   string            `json:"claude_message_id,omitempty"`
	ClaudeRequestID   string            `json:"claude_request_id,omitempty"`
	ToolCalls         []segmentToolCall `json:"tool_calls,omitempty"`
	IsSystem          bool              `json:"is_system,omitempty"`
	SourceType        string            `json:"source_type,omitempty"`
	SourceSubtype     string            `json:"source_subtype,omitempty"`
	SourceUUID        string            `json:"source_uuid,omitempty"`
	SourceParentUUID  string            `json:"source_parent_uuid,omitempty"`
	IsSidechain       bool              `json:"is_sidechain,omitempty"`
	IsCompactBoundary bool              `json:"is_compact_boundary,omitempty"`
}

type segmentToolCall struct {
	CallIndex           int                  `json:"call_index"`
	ToolName            string               `json:"tool_name"`
	Category            string               `json:"category,omitempty"`
	ToolUseID           string               `json:"tool_use_id,omitempty"`
	InputJSON           string               `json:"input_json,omitempty"`
	FilePath            string               `json:"file_path,omitempty"`
	SkillName           string               `json:"skill_name,omitempty"`
	ResultContentLength int                  `json:"result_content_length,omitempty"`
	ResultContent       string               `json:"result_content,omitempty"`
	SubagentSessionID   string               `json:"subagent_session_id,omitempty"`
	ResultEvents        []segmentResultEvent `json:"result_events,omitempty"`
}

type segmentResultEvent struct {
	ToolUseID         string `json:"tool_use_id,omitempty"`
	AgentID           string `json:"agent_id,omitempty"`
	SubagentSessionID string `json:"subagent_session_id,omitempty"`
	Source            string `json:"source"`
	Status            string `json:"status"`
	Content           string `json:"content"`
	ContentLength     int    `json:"content_length,omitempty"`
	Timestamp         string `json:"timestamp,omitempty"`
	EventIndex        int    `json:"event_index"`
}

type artifactExportStore interface {
	ListOwnedSessionIDsForExport(context.Context) ([]string, error)
	PendingArtifactExports(context.Context, int) ([]db.ArtifactExportQueueItem, error)
	ArtifactExportClaims(context.Context, []string) ([]db.ArtifactExportQueueItem, error)
	GetSessionFull(context.Context, string) (*db.Session, error)
	GetAllMessages(context.Context, string) ([]db.Message, error)
	GetUsageEvents(context.Context, string) ([]db.UsageEvent, error)
	ApplyArtifactPublicationChanges(context.Context, string, []db.ArtifactPublicationChange) (int64, bool, error)
	AcknowledgeArtifactExports(context.Context, []db.ArtifactExportQueueItem) error
	GetArtifactCheckpointHead(context.Context, string) (db.ArtifactCheckpointHead, bool, error)
	StreamArtifactPublications(context.Context, string, func(db.ArtifactPublication) error) (int64, error)
	RecordArtifactCheckpointHead(context.Context, db.ArtifactCheckpointHead, []db.ArtifactExportQueueItem) error
	GetArtifactCheckpointFloor(context.Context, string) (int, bool, error)
	ReserveArtifactCheckpointSequence(context.Context, string, int) (int, error)
}

const artifactExportBatchSize = 128

// ExportOptions selects artifact publication work. The default and explicit-ID
// modes process one bounded batch. Full drains bounded claim pages before
// recreating every currently-owned session's immutable dependencies one body
// at a time; publication authority still changes only through guarded claims.
type ExportOptions struct {
	Origin     string
	SessionIDs []string
	Full       bool
}

// ExportResult summarizes one canonical store export.
type ExportResult struct {
	ExportedSessions   int
	CheckpointCreated  bool
	CheckpointSequence int
}

type queuedArtifactExportStore interface {
	PendingArtifactExports(context.Context, int) ([]db.ArtifactExportQueueItem, error)
	GetSessionFull(context.Context, string) (*db.Session, error)
	GetAllMessages(context.Context, string) ([]db.Message, error)
	GetUsageEvents(context.Context, string) ([]db.UsageEvent, error)
}

type queuedArtifactExport struct {
	Item        db.ArtifactExportQueueItem
	Session     *db.Session
	Messages    []db.Message
	UsageEvents []db.UsageEvent
}

// forEachQueuedArtifactExport loads only the bounded dirty batch and at most
// one complete session body at a time. A missing session represents a pending
// publication deletion and deliberately performs no message or usage reads.
func forEachQueuedArtifactExport(
	ctx context.Context,
	store queuedArtifactExportStore,
	limit int,
	visit func(queuedArtifactExport) error,
) error {
	if visit == nil {
		return errors.New("queued artifact export visitor is required")
	}
	items, err := store.PendingArtifactExports(ctx, limit)
	if err != nil {
		return fmt.Errorf("reading queued artifact exports: %w", err)
	}
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		work := queuedArtifactExport{Item: item}
		work.Session, err = store.GetSessionFull(ctx, item.SessionID)
		if err != nil {
			return fmt.Errorf("loading queued artifact session %s: %w", item.SessionID, err)
		}
		if work.Session != nil &&
			(work.Session.Machine != "local" || work.Session.DeletedAt != nil) {
			work.Session = nil
		}
		if work.Session != nil {
			work.Messages, err = store.GetAllMessages(ctx, item.SessionID)
			if err != nil {
				return fmt.Errorf("loading queued artifact messages %s: %w", item.SessionID, err)
			}
			work.UsageEvents, err = store.GetUsageEvents(ctx, item.SessionID)
			if err != nil {
				return fmt.Errorf("loading queued artifact usage %s: %w", item.SessionID, err)
			}
		}
		if err := visit(work); err != nil {
			return err
		}
	}
	return nil
}

// ExportToStore publishes generation-guarded work into the canonical artifact
// store. Immutable dependencies are created before their manifest, and each
// bounded page's checkpoint is created last. Full mode may publish several
// bounded pages before its dependency-recovery pass completes.
func ExportToStore(
	ctx context.Context,
	database artifactExportStore,
	store ArtifactStore,
	opts ExportOptions,
) (_ ExportResult, retErr error) {
	if database == nil {
		return ExportResult{}, errors.New("artifact export database is required")
	}
	if store == nil {
		return ExportResult{}, errors.New("artifact export store is required")
	}
	if err := validateOriginID(opts.Origin); err != nil {
		return ExportResult{}, err
	}
	if len(opts.SessionIDs) > 1024 {
		return ExportResult{}, errors.New("artifact export session batch exceeds 1024 rows")
	}
	if opts.Full && len(opts.SessionIDs) > 0 {
		return ExportResult{}, errors.New("full artifact export cannot select session IDs")
	}
	if opts.Full {
		return exportFullToStore(ctx, database, store, opts.Origin)
	}

	var claims []db.ArtifactExportQueueItem
	var err error
	if len(opts.SessionIDs) > 0 {
		claims, err = database.ArtifactExportClaims(ctx, opts.SessionIDs)
	} else {
		queueLimit := artifactExportBatchSize
		if opts.Full {
			queueLimit = 1024
		}
		claims, err = database.PendingArtifactExports(ctx, queueLimit)
	}
	if err != nil {
		return ExportResult{}, fmt.Errorf("reading artifact export queue: %w", err)
	}
	claimByID := make(map[string]db.ArtifactExportQueueItem, len(claims))
	for _, claim := range claims {
		claimByID[claim.SessionID] = claim
	}

	selected, err := selectArtifactExportSessionIDs(ctx, database, opts, claims, claimByID)
	if err != nil {
		return ExportResult{}, err
	}
	changes := make([]db.ArtifactPublicationChange, 0, len(claims))
	acknowledged := make([]db.ArtifactExportQueueItem, 0, len(claims))
	result := ExportResult{}
	for _, sessionID := range selected {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		claim, claimed := claimByID[sessionID]
		sess, err := database.GetSessionFull(ctx, sessionID)
		if err != nil {
			return result, fmt.Errorf("loading artifact export session %s: %w", sessionID, err)
		}
		if sess == nil || sess.Machine != "local" || sess.DeletedAt != nil {
			if claimed {
				changes = append(changes, db.ArtifactPublicationChange{
					SessionID: sessionID, Generation: claim.Generation, Delete: true,
				})
				acknowledged = append(acknowledged, claim)
			}
			continue
		}
		messages, err := database.GetAllMessages(ctx, sessionID)
		if err != nil {
			return result, fmt.Errorf("loading artifact export messages %s: %w", sessionID, err)
		}
		usageEvents, err := database.GetUsageEvents(ctx, sessionID)
		if err != nil {
			return result, fmt.Errorf("loading artifact export usage %s: %w", sessionID, err)
		}
		manifestHash, created, err := exportLoadedSessionToStore(
			ctx, store, opts.Origin, sess, messages, usageEvents,
			productionArtifactLimits(),
		)
		if err != nil {
			return result, err
		}
		if created && claimed {
			result.ExportedSessions++
		}
		if claimed {
			changes = append(changes, db.ArtifactPublicationChange{
				SessionID: sessionID, Generation: claim.Generation,
				ManifestHash: manifestHash, SourceFingerprint: manifestHash,
			})
			acknowledged = append(acknowledged, claim)
		}
	}

	head, hadHead, err := database.GetArtifactCheckpointHead(ctx, opts.Origin)
	if err != nil {
		return result, fmt.Errorf("reading artifact checkpoint head: %w", err)
	}
	publicationRevision, changed, err := database.ApplyArtifactPublicationChanges(
		ctx, opts.Origin, changes,
	)
	if err != nil {
		return result, err
	}
	if !changed && hadHead && head.PublicationRevision == publicationRevision {
		verified, err := statRecordedCheckpoint(ctx, store, head)
		if err != nil {
			return result, err
		}
		if verified {
			if err := database.AcknowledgeArtifactExports(ctx, acknowledged); err != nil {
				return result, err
			}
			return result, nil
		}
	}

	comparableHead := !changed && hadHead
	if !hadHead {
		head, comparableHead, err = latestValidCheckpointHead(ctx, store, opts.Origin)
		if err != nil {
			return result, err
		}
	}
	mapSpool, mapDigest, mapRevision, err := spoolArtifactPublicationMap(ctx, database, opts.Origin)
	if err != nil {
		return result, err
	}
	defer func() {
		if mapSpool != nil {
			retErr = errors.Join(retErr, closeAndRemoveExportSpool(mapSpool))
		}
	}()
	if comparableHead && head.SessionMapSHA256 == mapDigest {
		checkpointSpool, checkpointIdentity, err := spoolArtifactCheckpoint(
			ctx, mapSpool, opts.Origin, head.Sequence,
		)
		if err != nil {
			return result, err
		}
		defer func() {
			if checkpointSpool != nil {
				retErr = errors.Join(retErr, closeAndRemoveExportSpool(checkpointSpool))
			}
		}()
		if checkpointIdentity.SHA256 != head.CheckpointSHA256 {
			return result, fmt.Errorf(
				"%w: recorded checkpoint %d hash differs from canonical publications",
				ErrArtifactCorrupt, head.Sequence,
			)
		}
		head.PublicationRevision = mapRevision
		head.CheckpointSize = checkpointIdentity.Size
		checkpointRef, err := NewRef(opts.Origin, KindCheckpoints,
			fmt.Sprintf("cp-%010d.json", head.Sequence))
		if err != nil {
			return result, err
		}
		if err := closeAndRemoveExportSpool(mapSpool); err != nil {
			return result, fmt.Errorf("cleaning artifact session map spool: %w", err)
		}
		mapSpool = nil
		create, err := store.Create(ctx, checkpointRef, checkpointIdentity,
			canonicalArtifactMediaType(KindCheckpoints), checkpointSpool)
		if err != nil {
			return result, fmt.Errorf("recreating artifact checkpoint: %w", err)
		}
		if err := closeAndRemoveExportSpool(checkpointSpool); err != nil {
			return result, fmt.Errorf("cleaning artifact checkpoint spool: %w", err)
		}
		checkpointSpool = nil
		if err := database.RecordArtifactCheckpointHead(ctx, head, acknowledged); err != nil {
			return result, err
		}
		result.CheckpointCreated = create.Created
		result.CheckpointSequence = head.Sequence
		return result, nil
	}

	sequence, err := reserveCheckpointSequenceFromStore(ctx, database, store, opts.Origin)
	if err != nil {
		return result, err
	}
	checkpointSpool, checkpointIdentity, err := spoolArtifactCheckpoint(
		ctx, mapSpool, opts.Origin, sequence,
	)
	if err != nil {
		return result, err
	}
	defer func() {
		if checkpointSpool != nil {
			retErr = errors.Join(retErr, closeAndRemoveExportSpool(checkpointSpool))
		}
	}()
	checkpointRef, err := NewRef(opts.Origin, KindCheckpoints,
		fmt.Sprintf("cp-%010d.json", sequence))
	if err != nil {
		return result, err
	}
	if err := closeAndRemoveExportSpool(mapSpool); err != nil {
		return result, fmt.Errorf("cleaning artifact session map spool: %w", err)
	}
	mapSpool = nil
	if _, err := store.Create(ctx, checkpointRef, checkpointIdentity,
		canonicalArtifactMediaType(KindCheckpoints), checkpointSpool,
	); err != nil {
		return result, fmt.Errorf("creating artifact checkpoint: %w", err)
	}
	if err := closeAndRemoveExportSpool(checkpointSpool); err != nil {
		return result, fmt.Errorf("cleaning artifact checkpoint spool: %w", err)
	}
	checkpointSpool = nil
	if err := database.RecordArtifactCheckpointHead(ctx, db.ArtifactCheckpointHead{
		Origin: opts.Origin, Sequence: sequence, PublicationRevision: mapRevision,
		SessionMapSHA256: mapDigest, CheckpointSHA256: checkpointIdentity.SHA256,
		CheckpointSize: checkpointIdentity.Size,
	}, acknowledged); err != nil {
		return result, err
	}
	result.CheckpointCreated = true
	result.CheckpointSequence = sequence
	return result, nil
}

func exportFullToStore(
	ctx context.Context,
	database artifactExportStore,
	store ArtifactStore,
	origin string,
) (ExportResult, error) {
	result := ExportResult{}
	processed := make(map[string]struct{})
	drain := func() error {
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			claims, err := database.PendingArtifactExports(ctx, maxArtifactExportBatchSize)
			if err != nil {
				return fmt.Errorf("reading full artifact export queue: %w", err)
			}
			if len(claims) == 0 {
				return nil
			}
			ids := make([]string, len(claims))
			for i, claim := range claims {
				ids[i] = claim.SessionID
				processed[claim.SessionID] = struct{}{}
			}
			page, err := ExportToStore(ctx, database, store, ExportOptions{
				Origin: origin, SessionIDs: ids,
			})
			if err != nil {
				return err
			}
			mergeArtifactExportResult(&result, page)
		}
	}
	if err := drain(); err != nil {
		return result, err
	}
	ids, err := database.ListOwnedSessionIDsForExport(ctx)
	if err != nil {
		return result, fmt.Errorf("listing sessions for full artifact export: %w", err)
	}
	for _, sessionID := range ids {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if _, ok := processed[sessionID]; ok {
			continue
		}
		sess, err := database.GetSessionFull(ctx, sessionID)
		if err != nil {
			return result, fmt.Errorf("loading full artifact export session %s: %w", sessionID, err)
		}
		if sess == nil || sess.Machine != "local" || sess.DeletedAt != nil {
			continue
		}
		messages, err := database.GetAllMessages(ctx, sessionID)
		if err != nil {
			return result, fmt.Errorf("loading full artifact export messages %s: %w", sessionID, err)
		}
		usageEvents, err := database.GetUsageEvents(ctx, sessionID)
		if err != nil {
			return result, fmt.Errorf("loading full artifact export usage %s: %w", sessionID, err)
		}
		if _, _, err := exportLoadedSessionToStore(
			ctx, store, origin, sess, messages, usageEvents, productionArtifactLimits(),
		); err != nil {
			return result, err
		}
	}
	if err := drain(); err != nil {
		return result, err
	}
	for {
		final, err := ExportToStore(ctx, database, store, ExportOptions{Origin: origin})
		if err != nil {
			return result, err
		}
		mergeArtifactExportResult(&result, final)
		pending, err := database.PendingArtifactExports(ctx, 1)
		if err != nil {
			return result, fmt.Errorf("checking concurrent full artifact work: %w", err)
		}
		if len(pending) == 0 {
			return result, nil
		}
		if err := drain(); err != nil {
			return result, err
		}
	}
}

const maxArtifactExportBatchSize = 1024

func mergeArtifactExportResult(total *ExportResult, page ExportResult) {
	total.ExportedSessions += page.ExportedSessions
	if page.CheckpointCreated {
		total.CheckpointCreated = true
	}
	if page.CheckpointSequence > total.CheckpointSequence {
		total.CheckpointSequence = page.CheckpointSequence
	}
}

func selectArtifactExportSessionIDs(
	ctx context.Context,
	database artifactExportStore,
	opts ExportOptions,
	claims []db.ArtifactExportQueueItem,
	claimByID map[string]db.ArtifactExportQueueItem,
) ([]string, error) {
	selected := make(map[string]struct{})
	switch {
	case opts.Full:
		ids, err := database.ListOwnedSessionIDsForExport(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing sessions for full artifact export: %w", err)
		}
		for _, id := range ids {
			selected[id] = struct{}{}
		}
		for _, claim := range claims {
			selected[claim.SessionID] = struct{}{}
		}
	case len(opts.SessionIDs) > 0:
		for _, id := range opts.SessionIDs {
			if _, ok := claimByID[id]; ok {
				selected[id] = struct{}{}
			}
		}
	default:
		for _, claim := range claims {
			selected[claim.SessionID] = struct{}{}
		}
	}
	ids := make([]string, 0, len(selected))
	for id := range selected {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func exportLoadedSessionToStore(
	ctx context.Context,
	store ArtifactStore,
	origin string,
	sess *db.Session,
	messages []db.Message,
	usageEvents []db.UsageEvent,
	limits artifactLimits,
) (string, bool, error) {
	if len(messages) > limits.sessionMessages {
		return "", false, fmt.Errorf(
			"session message limit exceeded for %s: got %d, limit %d",
			sess.ID, len(messages), limits.sessionMessages,
		)
	}
	if len(usageEvents) > limits.manifestUsageEvents {
		return "", false, fmt.Errorf(
			"manifest usage event limit exceeded for %s: got %d, limit %d",
			sess.ID, len(usageEvents), limits.manifestUsageEvents,
		)
	}
	if err := validateExportNestedCollections(messages, limits); err != nil {
		return "", false, fmt.Errorf("validating nested collections for %s: %w", sess.ID, err)
	}

	segmentHashes, err := exportMessageSegmentsToStore(
		ctx, store, origin, sess.ID, messages, limits,
	)
	if err != nil {
		return "", false, err
	}

	wireSession := manifestSessionFromDB(*sess)
	wireSession.Machine = origin
	normalizeManifestSessionLocalState(&wireSession)
	m := manifest{
		Version: formatVersion, Origin: origin, NativeSessionID: sess.ID,
		Session: wireSession, SessionName: sess.SessionName,
		Segments: segmentHashes, UsageEvents: canonicalUsageEvents(usageEvents),
		DataVersion: sess.DataVersion, Generation: 1,
		SessionHasToolCalls:   sess.HasToolCalls,
		SessionHasContextData: sess.HasContextData,
		SessionQualitySignals: manifestQualitySignalsFromDB(sess.StoredQualitySignals()),
	}
	data, err := canonicalJSON(m)
	if err != nil {
		return "", false, err
	}
	if int64(len(data)) > manifestDecodedLimit {
		return "", false, fmt.Errorf(
			"generated manifest exceeds %d-byte readable limit: got %d bytes",
			manifestDecodedLimit, len(data),
		)
	}
	hash := hashHex(data)
	identity, err := NewIdentity(hash, int64(len(data)))
	if err != nil {
		return "", false, err
	}
	ref, err := NewRef(origin, KindManifests, hash+".json")
	if err != nil {
		return "", false, err
	}
	created, err := store.Create(ctx, ref, identity,
		canonicalArtifactMediaType(KindManifests), bytes.NewReader(data))
	if err != nil {
		return "", false, fmt.Errorf("creating manifest for %s: %w", sess.ID, err)
	}
	return hash, created.Created, nil
}

func exportMessageSegmentsToStore(
	ctx context.Context,
	store ArtifactStore,
	origin string,
	sessionID string,
	messages []db.Message,
	limits artifactLimits,
) (_ []string, retErr error) {
	segmentHashes := make([]string, 0, 1)
	seen := make(map[string]struct{})
	var decodedBytes int64
	var spool *os.File
	var hasher hash.Hash
	var segmentBytes int64
	segmentMessages := 0
	segmentNested := nestedCollectionCounts{}
	defer func() {
		if spool != nil {
			retErr = errors.Join(retErr, closeAndRemoveExportSpool(spool))
		}
	}()

	start := func() error {
		var err error
		spool, err = os.CreateTemp("", "agentsview-artifact-segment-*")
		if err != nil {
			return fmt.Errorf("creating artifact segment spool: %w", err)
		}
		if err := spool.Chmod(0o600); err != nil {
			return fmt.Errorf("securing artifact segment spool: %w", err)
		}
		hasher = sha256.New()
		segmentBytes = 0
		segmentMessages = 0
		segmentNested = nestedCollectionCounts{}
		return nil
	}
	flush := func() error {
		if len(segmentHashes) >= limits.manifestSegments {
			return fmt.Errorf("manifest segment reference limit exceeded for %s: limit %d",
				sessionID, limits.manifestSegments)
		}
		digest := hex.EncodeToString(hasher.Sum(nil))
		if _, duplicate := seen[digest]; duplicate {
			return fmt.Errorf("generated duplicate segment reference %s", digest)
		}
		identity, err := NewIdentity(digest, segmentBytes)
		if err != nil {
			return err
		}
		ref, err := NewRef(origin, KindSegments, digest+".ndjson")
		if err != nil {
			return err
		}
		if _, err := spool.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("rewinding artifact segment spool: %w", err)
		}
		if _, err := store.Create(ctx, ref, identity,
			canonicalArtifactMediaType(KindSegments), spool); err != nil {
			return fmt.Errorf("creating segment for %s: %w", sessionID, err)
		}
		cleanupErr := closeAndRemoveExportSpool(spool)
		spool = nil
		if cleanupErr != nil {
			return fmt.Errorf("cleaning artifact segment spool: %w", cleanupErr)
		}
		seen[digest] = struct{}{}
		segmentHashes = append(segmentHashes, digest)
		return nil
	}

	if err := start(); err != nil {
		return nil, err
	}
	for _, message := range messages {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		messageNested, err := dbMessageNestedCounts(message, limits)
		if err != nil {
			return nil, err
		}
		if err := validateMessageFitsSegment(message.Ordinal, messageNested, limits); err != nil {
			return nil, err
		}
		data, err := canonicalJSON(segmentMessageFromDB(message))
		if err != nil {
			return nil, fmt.Errorf("encoding message segment: %w", err)
		}
		if int64(len(data)) > segmentDecodedLimit {
			return nil, fmt.Errorf(
				"encoded message record at ordinal %d exceeds %d-byte readable limit",
				message.Ordinal, segmentDecodedLimit,
			)
		}
		if segmentBytes > 0 && (segmentBytes+int64(len(data)) > segmentTargetSize ||
			segmentMessages >= limits.segmentMessages ||
			exceedsCollectionLimit(segmentNested.toolCalls,
				messageNested.toolCalls, limits.segmentToolCalls) ||
			exceedsCollectionLimit(segmentNested.resultEvents,
				messageNested.resultEvents, limits.segmentResultEvents)) {
			if err := flush(); err != nil {
				return nil, err
			}
			if err := start(); err != nil {
				return nil, err
			}
		}
		if int64(len(data)) > limits.sessionDecodedBytes-decodedBytes {
			return nil, fmt.Errorf("session decoded byte limit exceeded for %s: limit %d",
				sessionID, limits.sessionDecodedBytes)
		}
		if _, err := io.MultiWriter(spool, hasher).Write(data); err != nil {
			return nil, fmt.Errorf("writing artifact segment spool: %w", err)
		}
		segmentBytes += int64(len(data))
		decodedBytes += int64(len(data))
		segmentMessages++
		segmentNested.toolCalls += messageNested.toolCalls
		segmentNested.resultEvents += messageNested.resultEvents
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return segmentHashes, nil
}

func spoolArtifactPublicationMap(
	ctx context.Context,
	database artifactExportStore,
	origin string,
) (_ *os.File, _ string, _ int64, retErr error) {
	spool, err := os.CreateTemp("", "agentsview-artifact-map-*")
	if err != nil {
		return nil, "", 0, fmt.Errorf("creating artifact session map spool: %w", err)
	}
	failed := true
	defer func() {
		if failed {
			retErr = errors.Join(retErr, exportSpoolCleanup(spool))
		}
	}()
	if err := exportSpoolChmod(spool); err != nil {
		return nil, "", 0, fmt.Errorf("securing artifact session map spool: %w", err)
	}
	hasher := sha256.New()
	writer := io.MultiWriter(spool, hasher)
	if _, err := io.WriteString(writer, "{"); err != nil {
		return nil, "", 0, err
	}
	first := true
	revision, err := database.StreamArtifactPublications(ctx, origin, func(publication db.ArtifactPublication) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if publication.Origin != origin {
			return fmt.Errorf("artifact publication origin mismatch: got %q", publication.Origin)
		}
		gid, err := json.Marshal(origin + "~" + publication.SessionID)
		if err != nil {
			return err
		}
		hash, err := json.Marshal(publication.ManifestHash)
		if err != nil {
			return err
		}
		if !first {
			if _, err := io.WriteString(writer, ","); err != nil {
				return err
			}
		}
		first = false
		_, err = writer.Write(append(append(gid, ':'), hash...))
		return err
	})
	if err != nil {
		return nil, "", 0, fmt.Errorf("streaming artifact session map: %w", err)
	}
	if _, err := io.WriteString(writer, "}"); err != nil {
		return nil, "", 0, err
	}
	if _, err := hasher.Write([]byte{'\n'}); err != nil {
		return nil, "", 0, err
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return nil, "", 0, fmt.Errorf("rewinding artifact session map spool: %w", err)
	}
	failed = false
	return spool, hex.EncodeToString(hasher.Sum(nil)), revision, nil
}

func spoolArtifactCheckpoint(
	ctx context.Context,
	mapSpool *os.File,
	origin string,
	sequence int,
) (_ *os.File, _ Identity, retErr error) {
	if _, err := mapSpool.Seek(0, io.SeekStart); err != nil {
		return nil, Identity{}, fmt.Errorf("rewinding artifact session map: %w", err)
	}
	spool, err := os.CreateTemp("", "agentsview-artifact-checkpoint-*")
	if err != nil {
		return nil, Identity{}, fmt.Errorf("creating artifact checkpoint spool: %w", err)
	}
	failed := true
	defer func() {
		if failed {
			retErr = errors.Join(retErr, exportSpoolCleanup(spool))
		}
	}()
	if err := exportSpoolChmod(spool); err != nil {
		return nil, Identity{}, fmt.Errorf("securing artifact checkpoint spool: %w", err)
	}
	hasher := sha256.New()
	writer := io.MultiWriter(spool, hasher)
	originJSON, err := json.Marshal(origin)
	if err != nil {
		return nil, Identity{}, err
	}
	if _, err := fmt.Fprintf(writer, `{"origin":%s,"seq":%d,"sessions":`, originJSON, sequence); err != nil {
		return nil, Identity{}, err
	}
	if _, err := io.Copy(writer, &contextArtifactReader{ctx: ctx, reader: mapSpool}); err != nil {
		return nil, Identity{}, fmt.Errorf("copying artifact session map: %w", err)
	}
	if _, err := io.WriteString(writer, `,"v":1}`+"\n"); err != nil {
		return nil, Identity{}, err
	}
	info, err := spool.Stat()
	if err != nil {
		return nil, Identity{}, fmt.Errorf("stating artifact checkpoint spool: %w", err)
	}
	identity, err := NewIdentity(hex.EncodeToString(hasher.Sum(nil)), info.Size())
	if err != nil {
		return nil, Identity{}, err
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return nil, Identity{}, fmt.Errorf("rewinding artifact checkpoint spool: %w", err)
	}
	failed = false
	return spool, identity, nil
}

func closeAndRemoveExportSpool(file *os.File) error {
	if file == nil {
		return nil
	}
	name := file.Name()
	closeErr := file.Close()
	removeErr := os.Remove(name)
	if errors.Is(removeErr, fs.ErrNotExist) {
		removeErr = nil
	}
	return errors.Join(closeErr, removeErr)
}

var (
	exportSpoolChmod   = func(file *os.File) error { return file.Chmod(0o600) }
	exportSpoolCleanup = closeAndRemoveExportSpool
)

// statRecordedCheckpoint trusts the store's catalog identity, which is
// established by verified immutable creation and checked again on normal
// reads. Periodic unchanged export must remain constant work; full physical
// verification belongs bootstrap and maintenance.
func statRecordedCheckpoint(
	ctx context.Context,
	store ArtifactStore,
	head db.ArtifactCheckpointHead,
) (bool, error) {
	ref, err := NewRef(head.Origin, KindCheckpoints,
		fmt.Sprintf("cp-%010d.json", head.Sequence))
	if err != nil {
		return false, err
	}
	entry, err := store.Stat(ctx, ref)
	if errors.Is(err, ErrArtifactNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stating recorded artifact checkpoint: %w", err)
	}
	if entry.Identity.SHA256 != head.CheckpointSHA256 || entry.Identity.Size != head.CheckpointSize {
		quarantineErr := store.Quarantine(ctx, ref, "recorded checkpoint identity mismatch")
		return false, quarantineErr
	}
	return true, nil
}

func latestValidCheckpointHead(
	ctx context.Context,
	store ArtifactStore,
	origin string,
) (_ db.ArtifactCheckpointHead, _ bool, retErr error) {
	var head db.ArtifactCheckpointHead
	iterator, err := openStoreEntryIterator(ctx, store, origin, KindCheckpoints)
	if err != nil {
		return db.ArtifactCheckpointHead{}, false, fmt.Errorf("listing artifact checkpoints: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, iterator.Close()) }()
	for {
		entries, nextErr := iterator.Next(ctx, checkpointFloorPageSize)
		if nextErr != nil && !errors.Is(nextErr, io.EOF) {
			return db.ArtifactCheckpointHead{}, false, fmt.Errorf("listing artifact checkpoints: %w", nextErr)
		}
		for _, entry := range entries {
			sequence, err := checkpointSequence(entry.Ref.Name)
			if err != nil || sequence <= head.Sequence {
				continue
			}
			if entry.Identity.Size > checkpointDecodedLimit {
				continue
			}
			_, reader, err := store.Open(ctx, entry.Ref)
			if errors.Is(err, ErrArtifactNotFound) || errors.Is(err, ErrArtifactCorrupt) {
				continue
			}
			if err != nil {
				return db.ArtifactCheckpointHead{}, false,
					fmt.Errorf("opening artifact checkpoint: %w", err)
			}
			candidate, decodeErr := decodeCanonicalCheckpointHead(
				reader, origin, entry.Ref.Name, entry.Identity,
			)
			verifyErr := reader.Verify()
			closeErr := reader.Close()
			if closeErr != nil && !errors.Is(closeErr, ErrArtifactCorrupt) {
				return db.ArtifactCheckpointHead{}, false,
					fmt.Errorf("closing artifact checkpoint: %w", closeErr)
			}
			if verifyErr != nil && !errors.Is(verifyErr, ErrArtifactCorrupt) {
				return db.ArtifactCheckpointHead{}, false,
					fmt.Errorf("verifying artifact checkpoint: %w", verifyErr)
			}
			if errors.Is(decodeErr, errFutureArtifactVersion) {
				return db.ArtifactCheckpointHead{}, false, decodeErr
			}
			if decodeErr != nil || verifyErr != nil || closeErr != nil {
				continue
			}
			head = candidate
		}
		if errors.Is(nextErr, io.EOF) {
			break
		}
	}
	return head, head.Sequence > 0, nil
}

func decodeCanonicalCheckpointHead(
	reader io.Reader,
	origin string,
	name string,
	identity Identity,
) (db.ArtifactCheckpointHead, error) {
	decoder := json.NewDecoder(reader)
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return db.ArtifactCheckpointHead{}, errors.New("checkpoint is not a JSON object")
	}
	expectedFields := []string{"origin", "seq", "sessions", "v"}
	var sequence int
	var mapDigest string
	for _, expected := range expectedFields {
		token, err := decoder.Token()
		if err != nil {
			return db.ArtifactCheckpointHead{}, err
		}
		field, ok := token.(string)
		if !ok || field != expected {
			return db.ArtifactCheckpointHead{}, fmt.Errorf(
				"checkpoint is not canonical: expected field %q", expected,
			)
		}
		switch field {
		case "origin":
			var got string
			if err := decoder.Decode(&got); err != nil {
				return db.ArtifactCheckpointHead{}, err
			}
			if got != origin {
				return db.ArtifactCheckpointHead{}, fmt.Errorf(
					"checkpoint origin mismatch for %s: got %q", origin, got,
				)
			}
		case "seq":
			var number json.Number
			if err := decoder.Decode(&number); err != nil {
				return db.ArtifactCheckpointHead{}, err
			}
			value, err := strconv.ParseInt(number.String(), 10, 32)
			if err != nil || value < 1 {
				return db.ArtifactCheckpointHead{}, errors.New("checkpoint sequence is invalid")
			}
			sequence = int(value)
		case "sessions":
			mapDigest, err = decodeCanonicalCheckpointSessionMap(decoder, origin)
			if err != nil {
				return db.ArtifactCheckpointHead{}, err
			}
		case "v":
			var number json.Number
			if err := decoder.Decode(&number); err != nil {
				return db.ArtifactCheckpointHead{}, err
			}
			version, err := strconv.Atoi(number.String())
			if err != nil || version < 1 {
				return db.ArtifactCheckpointHead{}, errors.New("checkpoint version is unsupported")
			}
			if version > formatVersion {
				return db.ArtifactCheckpointHead{}, fmt.Errorf(
					"%w: checkpoint version %d", errFutureArtifactVersion, version,
				)
			}
			if version != formatVersion {
				return db.ArtifactCheckpointHead{}, errors.New("checkpoint version is unsupported")
			}
		}
	}
	token, err = decoder.Token()
	if err != nil || token != json.Delim('}') {
		return db.ArtifactCheckpointHead{}, errors.New("checkpoint object is incomplete")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return db.ArtifactCheckpointHead{}, errors.New("checkpoint has trailing JSON")
		}
		return db.ArtifactCheckpointHead{}, err
	}
	if fmt.Sprintf("cp-%010d.json", sequence) != name {
		return db.ArtifactCheckpointHead{}, fmt.Errorf(
			"checkpoint sequence identity mismatch: got %s", name,
		)
	}
	return db.ArtifactCheckpointHead{
		Origin: origin, Sequence: sequence,
		SessionMapSHA256: mapDigest, CheckpointSHA256: identity.SHA256,
		CheckpointSize: identity.Size,
	}, nil
}

func decodeCanonicalCheckpointSessionMap(
	decoder *json.Decoder,
	origin string,
) (string, error) {
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return "", errors.New("checkpoint sessions is not an object")
	}
	hasher := sha256.New()
	_, _ = io.WriteString(hasher, "{")
	first := true
	previous := ""
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return "", err
		}
		gid, ok := token.(string)
		if !ok || gid == "" || !strings.HasPrefix(gid, origin+"~") {
			return "", errors.New("checkpoint session identity is invalid")
		}
		if !first && gid <= previous {
			return "", errors.New("checkpoint sessions are not in canonical order")
		}
		var manifestHash string
		if err := decoder.Decode(&manifestHash); err != nil {
			return "", err
		}
		if err := validateHashHex(manifestHash); err != nil {
			return "", fmt.Errorf("checkpoint manifest hash is invalid: %w", err)
		}
		if !first {
			_, _ = io.WriteString(hasher, ",")
		}
		gidJSON, _ := json.Marshal(gid)
		hashJSON, _ := json.Marshal(manifestHash)
		_, _ = hasher.Write(gidJSON)
		_, _ = io.WriteString(hasher, ":")
		_, _ = hasher.Write(hashJSON)
		first = false
		previous = gid
	}
	token, err = decoder.Token()
	if err != nil || token != json.Delim('}') {
		return "", errors.New("checkpoint sessions object is incomplete")
	}
	_, _ = io.WriteString(hasher, "}\n")
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// Export temporarily preserves the root-based API while canonical publication
// migrates to ArtifactStore. The reference filesystem store is isolated from
// the legacy wire tree, then encoded into that tree for existing transports.
func reserveCheckpointSequenceFromStore(
	ctx context.Context,
	database artifactCheckpointSequenceDB,
	store ArtifactStore,
	origin string,
) (_ int, retErr error) {
	_, bootstrapped, err := database.GetArtifactCheckpointFloor(ctx, origin)
	if err != nil {
		return 0, fmt.Errorf("reading checkpoint floor for %s: %w", origin, err)
	}
	if bootstrapped {
		sequence, err := database.ReserveArtifactCheckpointSequence(ctx, origin, 0)
		if err != nil {
			return 0, fmt.Errorf("reserving checkpoint sequence for %s: %w", origin, err)
		}
		return sequence, nil
	}
	observedFloor := 0
	if observer, ok := store.(checkpointFloorStore); ok {
		floor, err := observer.checkpointFloor(ctx, origin)
		if err != nil {
			return 0, fmt.Errorf("listing checkpoint floor for %s: %w", origin, err)
		}
		observedFloor = floor
	} else {
		iterator, err := openStoreEntryIterator(ctx, store, origin, KindCheckpoints)
		if err != nil {
			return 0, fmt.Errorf("listing checkpoint floor for %s: %w", origin, err)
		}
		defer func() { retErr = errors.Join(retErr, iterator.Close()) }()
		for {
			entries, nextErr := iterator.Next(ctx, checkpointFloorPageSize)
			if nextErr != nil && !errors.Is(nextErr, io.EOF) {
				return 0, fmt.Errorf("listing checkpoint floor for %s: %w", origin, nextErr)
			}
			for _, entry := range entries {
				sequence, err := checkpointSequence(entry.Ref.Name)
				if err != nil {
					continue
				}
				observedFloor = max(observedFloor, sequence)
			}
			if errors.Is(nextErr, io.EOF) {
				break
			}
		}
	}
	sequence, err := database.ReserveArtifactCheckpointSequence(ctx, origin, observedFloor)
	if err != nil {
		return 0, fmt.Errorf("reserving checkpoint sequence for %s: %w", origin, err)
	}
	return sequence, nil
}

func normalizeManifestSessionLocalState(sess *manifestSession) {
	// Keep non-content, machine-local state out of the canonical manifest so a
	// source-only change to it does not alter the content hash and trigger a
	// re-import that clears the importer's local findings. secret_leak_count is
	// import-discarded secret state (see rewriteForImport); local_modified_at is
	// the local sync watermark, which import ignores (the importer stamps its
	// own) -- and a secret rescan bumps both even when no exported message
	// content changed. The file_* fields are source-file bookkeeping that
	// import clears (see clearImportedSessionSourceState); a touch, move, or
	// re-download of the source file changes them without changing any
	// exported content.
	sess.SecretLeakCount = 0
	sess.LocalModifiedAt = nil
	sess.FilePath = nil
	sess.FileSize = nil
	sess.FileMtime = nil
	sess.FileInode = nil
	sess.FileDevice = nil
	sess.FileHash = nil
}

type boundedCursorCycleGuard struct {
	anchor Cursor
	power  uint64
	length uint64
}

// Observe implements Brent's cycle detector over a deterministic cursor chain
// while retaining constant state regardless of traversal cardinality.
func (g *boundedCursorCycleGuard) Observe(cursor Cursor) bool {
	if cursor == "" {
		return false
	}
	if g.anchor == "" {
		g.anchor = cursor
		g.power = 1
		return false
	}
	g.length++
	if cursor == g.anchor {
		return true
	}
	if g.length == g.power {
		g.anchor = cursor
		if g.power <= ^uint64(0)/2 {
			g.power *= 2
		}
		g.length = 0
	}
	return false
}

func readVerifiedStoreArtifact(
	ctx context.Context,
	database *db.DB,
	store ArtifactStore,
	listed Entry,
	limit int64,
) ([]byte, error) {
	if listed.Identity.Size > limit {
		return nil, fmt.Errorf("%w: artifact exceeds %d-byte decoded limit",
			ErrArtifactInvalid, limit)
	}
	if err := validateRefIdentity(listed.Ref, listed.Identity); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrArtifactInvalid, err)
	}
	entry, reader, err := store.Open(ctx, listed.Ref)
	if err != nil {
		if errors.Is(err, ErrArtifactNotFound) {
			return nil, fmt.Errorf("%w: %s", errIncompleteArtifact, listed.Ref.Name)
		}
		if errors.Is(err, ErrArtifactCorrupt) {
			if qerr := enqueueArtifactRepair(ctx, database, listed); qerr != nil {
				return nil, errors.Join(err, qerr)
			}
		}
		return nil, err
	}
	if entry.Ref != listed.Ref || entry.Identity != listed.Identity {
		closeErr := reader.Close()
		repairErr := enqueueArtifactRepair(ctx, database, listed)
		return nil, errors.Join(
			fmt.Errorf("%w: artifact catalog identity changed", ErrArtifactCorrupt),
			closeErr, repairErr,
		)
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, limit+1))
	verifyErr := reader.Verify()
	closeErr := reader.Close()
	readErr = errors.Join(readErr, verifyErr, closeErr)
	if readErr != nil {
		if errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded) {
			return nil, readErr
		}
		if qerr := enqueueArtifactRepair(ctx, database, entry); qerr != nil {
			return nil, errors.Join(fmt.Errorf("%w: %v", ErrArtifactCorrupt, readErr), qerr)
		}
		return nil, fmt.Errorf("%w: %v", ErrArtifactCorrupt, readErr)
	}
	if int64(len(data)) != entry.Identity.Size {
		if qerr := enqueueArtifactRepair(ctx, database, entry); qerr != nil {
			return nil, errors.Join(fmt.Errorf("%w: artifact size mismatch", ErrArtifactCorrupt), qerr)
		}
		return nil, fmt.Errorf("%w: artifact size mismatch", ErrArtifactCorrupt)
	}
	return data, nil
}

func enqueueArtifactRepair(ctx context.Context, database *db.DB, entry Entry) error {
	return database.EnqueueArtifactRepair(ctx, db.ArtifactRepair{
		Origin: entry.Ref.Origin,
		Kind:   string(entry.Ref.Kind),
		Name:   entry.Ref.Name,
		SHA256: entry.Identity.SHA256,
		Size:   entry.Identity.Size,
	})
}

type artifactImportRetryScheduler interface {
	RecordChanged(context.Context, Entry) error
}

// StoreImportCoordinator coalesces dependency arrivals and repairs into one
// explicit store import at the end of a transfer batch.
type StoreImportCoordinator struct {
	database    *db.DB
	store       ArtifactStore
	localOrigin string
	now         func() time.Time

	runMu sync.Mutex
	mu    sync.Mutex

	generation uint64
	completed  uint64
}

type coordinatedTransportStore struct {
	ArtifactStore
	database    *db.DB
	coordinator *StoreImportCoordinator
}

func newCoordinatedTransportStore(
	database *db.DB,
	store ArtifactStore,
	coordinator *StoreImportCoordinator,
) *coordinatedTransportStore {
	return &coordinatedTransportStore{
		ArtifactStore: store,
		database:      database,
		coordinator:   coordinator,
	}
}

func (s *coordinatedTransportStore) RecordTransportChanged(
	ctx context.Context, entry Entry,
) error {
	return s.coordinator.RecordChanged(ctx, entry)
}

func (s *coordinatedTransportStore) PendingTransportRepair(
	ctx context.Context, ref Ref,
) (Entry, bool, error) {
	repair, found, err := s.database.ArtifactRepairForRef(
		ctx, ref.Origin, string(ref.Kind), ref.Name,
	)
	if err != nil || !found {
		return Entry{}, found, err
	}
	identity, err := NewIdentity(repair.SHA256, repair.Size)
	if err != nil {
		return Entry{}, false, err
	}
	return Entry{Ref: ref, Identity: identity}, true, nil
}

func (s *coordinatedTransportStore) RepairTransportArtifact(
	ctx context.Context, entry Entry, trusted io.Reader,
) error {
	return s.RepairContent(ctx, entry.Identity, trusted)
}

func (s *coordinatedTransportStore) AcknowledgeTransportRepair(
	ctx context.Context, entry Entry,
) error {
	return s.database.AcknowledgeArtifactRepair(ctx, db.ArtifactRepair{
		Origin: entry.Ref.Origin,
		Kind:   string(entry.Ref.Kind),
		Name:   entry.Ref.Name,
		SHA256: entry.Identity.SHA256,
		Size:   entry.Identity.Size,
	})
}

func NewStoreImportCoordinator(
	database *db.DB, store ArtifactStore, localOrigin string,
) *StoreImportCoordinator {
	return &StoreImportCoordinator{
		database: database, store: store, localOrigin: localOrigin,
		generation: 1,
	}
}

// requestDrain marks the current transfer batch for another import.
func (c *StoreImportCoordinator) requestDrain() error {
	if c == nil {
		return errors.New("artifact import coordinator is required")
	}
	c.mu.Lock()
	c.generation++
	c.mu.Unlock()
	return nil
}

// Finalize consumes one coalesced retry signal. A transient import failure
// retains the signal for a later finalize attempt.
func (c *StoreImportCoordinator) Finalize(ctx context.Context) (ImportResult, error) {
	if c == nil {
		return ImportResult{}, errors.New("artifact import coordinator is required")
	}
	c.runMu.Lock()
	defer c.runMu.Unlock()

	c.mu.Lock()
	generation := c.generation
	if c.completed >= generation {
		c.mu.Unlock()
		return ImportResult{}, nil
	}
	c.mu.Unlock()

	result, err := c.drainQueuedImports(ctx)
	if err == nil {
		c.mu.Lock()
		c.completed = generation
		c.mu.Unlock()
	}
	return result, err
}

type checkpointClosureOutcome uint8

const (
	checkpointClosureComplete checkpointClosureOutcome = iota
	checkpointClosureDeferred
	checkpointClosureCurrentInvalid
)

func inspectCheckpointClosureFromStore(
	ctx context.Context,
	database *db.DB,
	store ArtifactStore,
	origin string,
	cp checkpoint,
) (checkpointClosureOutcome, error) {
	keys := make([]string, 0, len(cp.Sessions))
	for gid := range cp.Sessions {
		keys = append(keys, gid)
	}
	sort.Strings(keys)
	importStates, err := loadImportStates(database, origin, keys)
	if err != nil {
		return checkpointClosureComplete, fmt.Errorf("reading import state for %s: %w", origin, err)
	}
	for _, gid := range keys {
		if err := ctx.Err(); err != nil {
			return checkpointClosureComplete, err
		}
		manifestHash := cp.Sessions[gid]
		if importStates[importStateKey(origin, gid)] == manifestHash {
			continue
		}
		_, _, outcome, err := readStoreSession(
			ctx, database, store, origin, gid, manifestHash,
		)
		if err != nil {
			return checkpointClosureComplete, err
		}
		if outcome != checkpointClosureComplete {
			return outcome, nil
		}
	}
	return checkpointClosureComplete, nil
}

// RepairArtifactFromTrustedPeer repairs one queued physical identity from a
// trusted peer stream and acknowledges the exact SQLite claim only afterward.
func RepairArtifactFromTrustedPeer(
	ctx context.Context,
	database *db.DB,
	store ArtifactStore,
	repair db.ArtifactRepair,
	trusted io.Reader,
	retry artifactImportRetryScheduler,
) error {
	if database == nil || store == nil || trusted == nil || retry == nil || isTypedNil(retry) {
		return errors.New("artifact repair requires database, store, trusted content, and retry coordinator")
	}
	ref, err := NewRef(repair.Origin, Kind(repair.Kind), repair.Name)
	if err != nil {
		return err
	}
	identity, err := NewIdentity(repair.SHA256, repair.Size)
	if err != nil {
		return err
	}
	if err := validateRefIdentity(ref, identity); err != nil {
		return err
	}
	if err := store.RepairContent(ctx, identity, trusted); err != nil {
		return fmt.Errorf("repairing artifact content: %w", err)
	}
	if err := retry.RecordChanged(ctx, Entry{Ref: ref, Identity: identity}); err != nil {
		return fmt.Errorf("scheduling artifact import retry: %w", err)
	}
	if err := database.AcknowledgeArtifactRepair(ctx, repair); err != nil {
		return fmt.Errorf("acknowledging artifact repair: %w", err)
	}
	return nil
}

func isTypedNil(value any) bool {
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

func importCheckpointFromStore(
	ctx context.Context,
	database *db.DB,
	store ArtifactStore,
	origin string,
	cp checkpoint,
) (ImportResult, error) {
	keys := make([]string, 0, len(cp.Sessions))
	for gid := range cp.Sessions {
		keys = append(keys, gid)
	}
	sort.Strings(keys)
	importStates, err := loadImportStates(database, origin, keys)
	if err != nil {
		return ImportResult{}, fmt.Errorf("reading import state for %s: %w", origin, err)
	}
	result := ImportResult{}
	complete := true
	for _, gid := range keys {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		manifestHash := cp.Sessions[gid]
		stateKey := importStateKey(origin, gid)
		if importStates[stateKey] == manifestHash {
			continue
		}
		m, msgs, outcome, err := readStoreSession(
			ctx, database, store, origin, gid, manifestHash,
		)
		if err != nil {
			return result, err
		}
		if outcome != checkpointClosureComplete {
			result.Deferred++
			complete = false
			continue
		}
		write := rewriteForImport(m, msgs)
		writeResult, err := database.WriteSessionBatchAtomic([]db.SessionBatchWrite{write})
		if err != nil {
			if errors.Is(err, db.ErrSessionTrashed) {
				continue
			}
			if errors.Is(err, db.ErrSessionExcluded) {
				complete = false
				continue
			}
			return result, fmt.Errorf("importing artifact session %s: %w", gid, err)
		}
		result.Sessions += writeResult.WrittenSessions
		result.Messages += writeResult.WrittenMessages
		if _, err := database.ReapplyMetadataReplayState(ctx, gid, write.Session.ID); err != nil {
			return result, fmt.Errorf("reapplying metadata after importing artifact session %s: %w", gid, err)
		}
		if err := database.SetSyncState(stateKey, manifestHash); err != nil {
			return result, fmt.Errorf("writing import state for %s: %w", gid, err)
		}
	}
	if complete {
		err := database.RecordArtifactCheckpointLanding(ctx, db.ArtifactCheckpointLanding{
			Origin: origin, Sequence: cp.Sequence,
		}, cp.Sessions)
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

func readStoreSession(
	ctx context.Context,
	database *db.DB,
	store ArtifactStore,
	origin, gid, manifestHash string,
) (manifest, []db.Message, checkpointClosureOutcome, error) {
	manifestRef, err := NewRef(origin, KindManifests, manifestHash+".json")
	if err != nil {
		return manifest{}, nil, checkpointClosureComplete, err
	}
	manifestEntry, err := store.Stat(ctx, manifestRef)
	if errors.Is(err, ErrArtifactNotFound) {
		return manifest{}, nil, checkpointClosureDeferred, nil
	}
	if err != nil {
		return manifest{}, nil, checkpointClosureComplete, err
	}
	data, err := readVerifiedStoreArtifact(ctx, database, store, manifestEntry, manifestDecodedLimit)
	if errors.Is(err, errIncompleteArtifact) {
		return manifest{}, nil, checkpointClosureDeferred, nil
	}
	if err != nil {
		if errors.Is(err, ErrArtifactInvalid) {
			if qerr := store.Quarantine(ctx, manifestRef, err.Error()); qerr != nil {
				return manifest{}, nil, checkpointClosureComplete, errors.Join(err, qerr)
			}
			return manifest{}, nil, checkpointClosureCurrentInvalid, nil
		}
		return manifest{}, nil, checkpointClosureComplete, err
	}
	m, err := decodeManifestWithLimits(data, productionArtifactLimits())
	if err != nil {
		if qerr := store.Quarantine(ctx, manifestRef, "manifest JSON is invalid"); qerr != nil {
			return manifest{}, nil, checkpointClosureComplete, errors.Join(err, qerr)
		}
		return manifest{}, nil, checkpointClosureCurrentInvalid, nil
	}
	if m.Version > formatVersion {
		return manifest{}, nil, checkpointClosureDeferred, nil
	}
	canonical, canonicalErr := canonicalJSON(m)
	if canonicalErr != nil || !bytes.Equal(canonical, data) {
		if qerr := store.Quarantine(ctx, manifestRef, "manifest JSON is not canonical"); qerr != nil {
			return manifest{}, nil, checkpointClosureComplete, errors.Join(canonicalErr, qerr)
		}
		return manifest{}, nil, checkpointClosureCurrentInvalid, nil
	}
	if err := validateManifest(m, origin, gid); err != nil {
		if errors.Is(err, errFutureArtifactVersion) {
			return manifest{}, nil, checkpointClosureDeferred, nil
		}
		if qerr := store.Quarantine(ctx, manifestRef, err.Error()); qerr != nil {
			return manifest{}, nil, checkpointClosureComplete, errors.Join(err, qerr)
		}
		return manifest{}, nil, checkpointClosureCurrentInvalid, nil
	}
	var messages []db.Message
	var decodedBytes int64
	totalNested := nestedCollectionCounts{}
	for _, segmentHash := range m.Segments {
		segmentRef, err := NewRef(origin, KindSegments, segmentHash+".ndjson")
		if err != nil {
			if errors.Is(err, ErrArtifactInvalid) {
				if qerr := store.Quarantine(ctx, segmentRef, err.Error()); qerr != nil {
					return manifest{}, nil, checkpointClosureComplete, errors.Join(err, qerr)
				}
				return manifest{}, nil, checkpointClosureCurrentInvalid, nil
			}
			return manifest{}, nil, checkpointClosureComplete, err
		}
		segmentEntry, err := store.Stat(ctx, segmentRef)
		if errors.Is(err, ErrArtifactNotFound) {
			return manifest{}, nil, checkpointClosureDeferred, nil
		}
		if err != nil {
			return manifest{}, nil, checkpointClosureComplete, err
		}
		segmentData, err := readVerifiedStoreArtifact(
			ctx, database, store, segmentEntry, segmentDecodedLimit,
		)
		if errors.Is(err, errIncompleteArtifact) {
			return manifest{}, nil, checkpointClosureDeferred, nil
		}
		if err != nil {
			if errors.Is(err, ErrArtifactInvalid) {
				if qerr := store.Quarantine(ctx, segmentRef, err.Error()); qerr != nil {
					return manifest{}, nil, checkpointClosureComplete, errors.Join(err, qerr)
				}
				return manifest{}, nil, checkpointClosureCurrentInvalid, nil
			}
			return manifest{}, nil, checkpointClosureComplete, err
		}
		segmentMessages, err := decodeSegment(segmentData)
		if errors.Is(err, errFutureArtifactVersion) {
			return manifest{}, nil, checkpointClosureDeferred, nil
		}
		if err != nil {
			if qerr := store.Quarantine(ctx, segmentRef, "segment NDJSON is invalid"); qerr != nil {
				return manifest{}, nil, checkpointClosureComplete, errors.Join(err, qerr)
			}
			return manifest{}, nil, checkpointClosureCurrentInvalid, nil
		}
		canonical, canonicalErr := encodeSegment(segmentMessages)
		if canonicalErr != nil || !bytes.Equal(canonical, segmentData) {
			if qerr := store.Quarantine(ctx, segmentRef, "segment NDJSON is not canonical"); qerr != nil {
				return manifest{}, nil, checkpointClosureComplete, errors.Join(canonicalErr, qerr)
			}
			return manifest{}, nil, checkpointClosureCurrentInvalid, nil
		}
		if int64(len(segmentData)) > maxSessionDecodedBytes-decodedBytes {
			if qerr := store.Quarantine(ctx, manifestRef, "session decoded byte limit exceeded"); qerr != nil {
				return manifest{}, nil, checkpointClosureComplete, qerr
			}
			return manifest{}, nil, checkpointClosureCurrentInvalid, nil
		}
		for _, message := range segmentMessages {
			counts, err := dbMessageNestedCounts(message, productionArtifactLimits())
			if err != nil {
				if qerr := store.Quarantine(ctx, manifestRef, err.Error()); qerr != nil {
					return manifest{}, nil, checkpointClosureComplete, errors.Join(err, qerr)
				}
				return manifest{}, nil, checkpointClosureCurrentInvalid, nil
			}
			if exceedsCollectionLimit(totalNested.toolCalls, counts.toolCalls, maxSessionToolCalls) ||
				exceedsCollectionLimit(totalNested.resultEvents, counts.resultEvents, maxSessionResultEvents) {
				if qerr := store.Quarantine(ctx, manifestRef, "session nested collection limit exceeded"); qerr != nil {
					return manifest{}, nil, checkpointClosureComplete, qerr
				}
				return manifest{}, nil, checkpointClosureCurrentInvalid, nil
			}
			totalNested.toolCalls += counts.toolCalls
			totalNested.resultEvents += counts.resultEvents
		}
		decodedBytes += int64(len(segmentData))
		messages = append(messages, segmentMessages...)
	}
	if len(messages) > maxSessionMessages {
		if qerr := store.Quarantine(ctx, manifestRef, "session message limit exceeded"); qerr != nil {
			return manifest{}, nil, checkpointClosureComplete, qerr
		}
		return manifest{}, nil, checkpointClosureCurrentInvalid, nil
	}
	return m, messages, checkpointClosureComplete, nil
}

func loadImportStates(
	database syncStateValueReader, origin string, gids []string,
) (map[string]string, error) {
	if len(gids) == 0 {
		return map[string]string{}, nil
	}
	keys := make([]string, len(gids))
	for i, gid := range gids {
		keys[i] = importStateKey(origin, gid)
	}
	return database.SyncStateValues(keys)
}

func importStateKey(origin, gid string) string {
	return importStatePrefix + origin + ":" + gid
}

func validateCheckpoint(cp *checkpoint, origin string) error {
	if cp.Version > formatVersion {
		return fmt.Errorf(
			"%w: checkpoint for %s has artifact version %d",
			errFutureArtifactVersion, origin, cp.Version,
		)
	}
	if cp.Version != formatVersion {
		return fmt.Errorf(
			"checkpoint for %s has unsupported artifact version %d",
			origin, cp.Version,
		)
	}
	if cp.Origin != origin {
		return fmt.Errorf(
			"checkpoint origin mismatch for %s: got %q",
			origin, cp.Origin,
		)
	}
	return validateCheckpointReferences(cp, origin)
}

func validateCheckpointReferences(cp *checkpoint, origin string) error {
	for gid, manifestHash := range cp.Sessions {
		if gid == "" {
			return fmt.Errorf("checkpoint for %s contains empty session id", origin)
		}
		if !strings.HasPrefix(gid, origin+"~") {
			return fmt.Errorf(
				"checkpoint session %s does not belong to origin %s",
				gid, origin,
			)
		}
		if strings.TrimSpace(manifestHash) == "" {
			return fmt.Errorf("checkpoint session %s has empty manifest hash", gid)
		}
		if err := validateHashHex(manifestHash); err != nil {
			return fmt.Errorf("checkpoint session %s has invalid manifest hash: %w", gid, err)
		}
	}
	return nil
}

func validateManifest(m manifest, origin, gid string) error {
	if m.Version > formatVersion {
		return fmt.Errorf(
			"%w: manifest %s has artifact version %d",
			errFutureArtifactVersion, gid, m.Version,
		)
	}
	if m.Version != formatVersion {
		return fmt.Errorf(
			"manifest %s has unsupported artifact version %d",
			gid, m.Version,
		)
	}
	if m.Origin != origin {
		return fmt.Errorf(
			"manifest origin mismatch for %s: got %q",
			gid, m.Origin,
		)
	}
	if m.NativeSessionID == "" {
		return fmt.Errorf("manifest %s has empty native session id", gid)
	}
	expectedGID := origin + "~" + m.NativeSessionID
	if gid != expectedGID {
		return fmt.Errorf(
			"manifest session id mismatch: checkpoint has %s, manifest has %s",
			gid, expectedGID,
		)
	}
	if m.Session.ID != m.NativeSessionID {
		return fmt.Errorf(
			"manifest %s session row id mismatch: got %q",
			gid, m.Session.ID,
		)
	}
	if m.Session.Machine != origin {
		return fmt.Errorf(
			"manifest %s session row machine mismatch: got %q",
			gid, m.Session.Machine,
		)
	}
	if len(m.Segments) == 0 {
		return fmt.Errorf("manifest %s has no message segments", gid)
	}
	if err := validateManifestReferences(m); err != nil {
		return err
	}
	return nil
}

func validateManifestReferences(m manifest) error {
	return validateManifestReferencesWithLimits(m, productionArtifactLimits())
}

func validateManifestReferencesWithLimits(m manifest, limits artifactLimits) error {
	if len(m.Segments) > limits.manifestSegments {
		return fmt.Errorf(
			"manifest segment reference limit exceeded: got %d, limit %d",
			len(m.Segments), limits.manifestSegments,
		)
	}
	seen := make(map[string]struct{}, len(m.Segments))
	for _, segmentHash := range m.Segments {
		if err := validateHashHex(segmentHash); err != nil {
			return fmt.Errorf("manifest segment has invalid hash: %w", err)
		}
		if _, ok := seen[segmentHash]; ok {
			return fmt.Errorf("manifest has duplicate segment reference %s", segmentHash)
		}
		seen[segmentHash] = struct{}{}
	}
	if len(m.UsageEvents) > limits.manifestUsageEvents {
		return fmt.Errorf(
			"manifest usage event limit exceeded: got %d, limit %d",
			len(m.UsageEvents), limits.manifestUsageEvents,
		)
	}
	if m.RawSource != nil && m.RawSource.Hash != "" {
		if err := validateHashHex(m.RawSource.Hash); err != nil {
			return fmt.Errorf("manifest raw source has invalid hash: %w", err)
		}
	}
	return nil
}

func rewriteForImport(m manifest, msgs []db.Message) db.SessionBatchWrite {
	importedID := m.Origin + "~" + m.NativeSessionID
	sess := m.Session.dbSession()
	sess.ID = importedID
	sess.Machine = m.Origin
	sess.SessionName = m.SessionName
	clearImportedSessionSourceState(&sess)
	// Restore signal state dropped from the Session JSON; signalsFromSession
	// reads these fields below to persist the imported session's signal columns.
	sess.HasToolCalls = m.SessionHasToolCalls
	sess.HasContextData = m.SessionHasContextData
	sess.ApplyQualitySignals(m.SessionQualitySignals.dbQualitySignals())
	// Secret findings are not carried in the manifest, so an imported session has
	// no finding rows. Treat it as unscanned rather than trusting the source scan:
	// clear the rules version (json:"-", so already absent) and the leak count
	// (carried in the Session JSON) so the count stays consistent with the zero
	// findings and `secrets scan --backfill` rescans it with local rules. Stamping
	// it scanned-at-source-version would make backfill (secrets_rules_version !=
	// current) skip a secret-bearing session, leaving no revealable findings.
	sess.SecretsRulesVersion = ""
	sess.SecretLeakCount = 0
	sess.SourceSessionID = prefixImportedSessionID(m.Origin, sess.SourceSessionID)
	if sess.ParentSessionID != nil {
		prefixed := prefixImportedSessionID(m.Origin, *sess.ParentSessionID)
		sess.ParentSessionID = &prefixed
	}
	for i := range msgs {
		msgs[i].ID = 0
		msgs[i].SessionID = importedID
		for j := range msgs[i].ToolCalls {
			msgs[i].ToolCalls[j].MessageID = 0
			msgs[i].ToolCalls[j].SessionID = importedID
			msgs[i].ToolCalls[j].SubagentSessionID = prefixImportedSessionID(
				m.Origin,
				msgs[i].ToolCalls[j].SubagentSessionID,
			)
			for k := range msgs[i].ToolCalls[j].ResultEvents {
				ev := &msgs[i].ToolCalls[j].ResultEvents[k]
				ev.SubagentSessionID = prefixImportedSessionID(m.Origin, ev.SubagentSessionID)
			}
		}
	}
	usageEvents := dbUsageEvents(m.UsageEvents, importedID)
	return db.SessionBatchWrite{
		Session:         sess,
		Messages:        msgs,
		UsageEvents:     usageEvents,
		Signals:         signalsFromSession(sess),
		DataVersion:     m.DataVersion,
		ReplaceMessages: true,
	}
}

func clearImportedSessionSourceState(sess *db.Session) {
	sess.FilePath = nil
	sess.FileSize = nil
	sess.FileMtime = nil
	sess.NextOrdinal = 0
	sess.LastEntryUUID = nil
	sess.FileInode = nil
	sess.FileDevice = nil
	sess.FileHash = nil
}

func prefixImportedSessionID(origin, id string) string {
	if id == "" || strings.Contains(id, "~") {
		return id
	}
	return origin + "~" + id
}

func signalsFromSession(s db.Session) db.SessionSignalUpdate {
	update := db.SessionSignalUpdate{
		ToolFailureSignalCount: s.ToolFailureSignalCount,
		ToolRetryCount:         s.ToolRetryCount,
		EditChurnCount:         s.EditChurnCount,
		ConsecutiveFailureMax:  s.ConsecutiveFailureMax,
		Outcome:                s.Outcome,
		OutcomeConfidence:      s.OutcomeConfidence,
		EndedWithRole:          s.EndedWithRole,
		FinalFailureStreak:     s.FinalFailureStreak,
		SignalsPendingSince:    s.SignalsPendingSince,
		CompactionCount:        s.CompactionCount,
		MidTaskCompactionCount: s.MidTaskCompactionCount,
		ContextPressureMax:     s.ContextPressureMax,
		HealthScore:            s.HealthScore,
		HealthGrade:            s.HealthGrade,
		HasToolCalls:           s.HasToolCalls,
		HasContextData:         s.HasContextData,
		SecretLeakCount:        s.SecretLeakCount,
		SecretsRulesVersion:    s.SecretsRulesVersion,
	}
	if qs := s.StoredQualitySignals(); qs != nil {
		update.QualitySignals = *qs
	}
	return update
}

func decodeManifestWithLimits(data []byte, limits artifactLimits) (manifest, error) {
	var envelope struct {
		Version     int             `json:"v"`
		Origin      string          `json:"origin"`
		Segments    json.RawMessage `json:"segments"`
		UsageEvents json.RawMessage `json:"usage_events"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return manifest{}, err
	}
	// Future manifests are retained for forward compatibility. Reading only
	// their scalar header avoids allocating collections whose schema this
	// version does not understand.
	if envelope.Version > formatVersion {
		return manifest{Version: envelope.Version, Origin: envelope.Origin}, nil
	}
	if err := preflightManifestCollections(
		envelope.Segments, envelope.UsageEvents, limits,
	); err != nil {
		return manifest{}, err
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return manifest{}, err
	}
	return m, nil
}

func preflightManifestCollections(
	segments, usageEvents json.RawMessage,
	limits artifactLimits,
) error {
	if err := preflightSegmentReferences(segments, limits.manifestSegments); err != nil {
		return err
	}
	return preflightJSONArrayCount(
		usageEvents, "manifest usage event", limits.manifestUsageEvents,
	)
}

func preflightSegmentReferences(data json.RawMessage, limit int) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	token, err := dec.Token()
	if err != nil {
		return err
	}
	if token != json.Delim('[') {
		return errors.New("manifest segments must be an array")
	}
	seen := make(map[string]struct{}, min(limit, 16))
	count := 0
	for dec.More() {
		if count >= limit {
			return fmt.Errorf("manifest segment reference limit exceeded: limit %d", limit)
		}
		var hash string
		if err := dec.Decode(&hash); err != nil {
			return fmt.Errorf("decoding manifest segment reference: %w", err)
		}
		if _, ok := seen[hash]; ok {
			return fmt.Errorf("manifest has duplicate segment reference %s", hash)
		}
		seen[hash] = struct{}{}
		count++
	}
	_, err = dec.Token()
	return err
}

func preflightJSONArrayCount(data json.RawMessage, name string, limit int) error {
	_, err := countJSONArrayElements(data, name, limit)
	return err
}

func countJSONArrayElements(
	data json.RawMessage,
	name string,
	limit int,
) (int, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return 0, nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	token, err := dec.Token()
	if err != nil {
		return 0, err
	}
	if token != json.Delim('[') {
		return 0, fmt.Errorf("%ss must be an array", name)
	}
	count := 0
	for dec.More() {
		if count >= limit {
			return 0, fmt.Errorf("%s limit exceeded: limit %d", name, limit)
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return 0, fmt.Errorf("decoding %s: %w", name, err)
		}
		count++
	}
	if _, err := dec.Token(); err != nil {
		return 0, err
	}
	return count, nil
}

func canonicalMessages(msgs []db.Message) []db.Message {
	out := make([]db.Message, len(msgs))
	for i, msg := range msgs {
		msg.ID = 0
		msg.SessionID = ""
		if len(msg.ToolCalls) > 0 {
			calls := make([]db.ToolCall, len(msg.ToolCalls))
			copy(calls, msg.ToolCalls)
			for j := range calls {
				calls[j].MessageID = 0
				calls[j].SessionID = ""
			}
			msg.ToolCalls = calls
		}
		out[i] = msg
	}
	return out
}

func canonicalUsageEvents(events []db.UsageEvent) []artifactUsageEvent {
	out := make([]artifactUsageEvent, len(events))
	for i, ev := range events {
		out[i] = artifactUsageEvent{
			MessageOrdinal:           ev.MessageOrdinal,
			Source:                   ev.Source,
			Model:                    ev.Model,
			InputTokens:              ev.InputTokens,
			OutputTokens:             ev.OutputTokens,
			CacheCreationInputTokens: ev.CacheCreationInputTokens,
			CacheReadInputTokens:     ev.CacheReadInputTokens,
			ReasoningTokens:          ev.ReasoningTokens,
			CostUSD:                  ev.CostUSD,
			CostStatus:               ev.CostStatus,
			CostSource:               ev.CostSource,
			OccurredAt:               ev.OccurredAt,
			DedupKey:                 ev.DedupKey,
		}
	}
	return out
}

func validateExportNestedCollections(msgs []db.Message, limits artifactLimits) error {
	total := nestedCollectionCounts{}
	for _, msg := range msgs {
		messageNested, err := dbMessageNestedCounts(msg, limits)
		if err != nil {
			return err
		}
		if err := validateMessageFitsSegment(msg.Ordinal, messageNested, limits); err != nil {
			return err
		}
		if exceedsCollectionLimit(
			total.toolCalls, messageNested.toolCalls, limits.sessionToolCalls,
		) {
			return fmt.Errorf(
				"session tool call limit exceeded at message ordinal %d: limit %d",
				msg.Ordinal, limits.sessionToolCalls,
			)
		}
		if exceedsCollectionLimit(
			total.resultEvents, messageNested.resultEvents, limits.sessionResultEvents,
		) {
			return fmt.Errorf(
				"session result event limit exceeded at message ordinal %d: limit %d",
				msg.Ordinal, limits.sessionResultEvents,
			)
		}
		total.toolCalls += messageNested.toolCalls
		total.resultEvents += messageNested.resultEvents
	}
	return nil
}

func dbMessageNestedCounts(
	msg db.Message,
	limits artifactLimits,
) (nestedCollectionCounts, error) {
	if len(msg.ToolCalls) > limits.messageToolCalls {
		return nestedCollectionCounts{}, fmt.Errorf(
			"tool call limit exceeded for message ordinal %d: got %d, limit %d",
			msg.Ordinal, len(msg.ToolCalls), limits.messageToolCalls,
		)
	}
	counts := nestedCollectionCounts{toolCalls: len(msg.ToolCalls)}
	for toolIndex, call := range msg.ToolCalls {
		if len(call.ResultEvents) > limits.toolResultEvents {
			return nestedCollectionCounts{}, fmt.Errorf(
				"result event limit exceeded for tool call %d in message ordinal %d: got %d, limit %d",
				toolIndex, msg.Ordinal, len(call.ResultEvents), limits.toolResultEvents,
			)
		}
		counts.resultEvents += len(call.ResultEvents)
	}
	return counts, nil
}

func validateMessageFitsSegment(
	ordinal int,
	counts nestedCollectionCounts,
	limits artifactLimits,
) error {
	if counts.toolCalls > limits.segmentToolCalls {
		return fmt.Errorf(
			"message ordinal %d cannot fit in one segment: got %d tool calls, segment limit %d",
			ordinal, counts.toolCalls, limits.segmentToolCalls,
		)
	}
	if counts.resultEvents > limits.segmentResultEvents {
		return fmt.Errorf(
			"message ordinal %d cannot fit in one segment: got %d result events, segment limit %d",
			ordinal, counts.resultEvents, limits.segmentResultEvents,
		)
	}
	return nil
}

func encodeSegment(msgs []db.Message) ([]byte, error) {
	var buf bytes.Buffer
	for _, msg := range msgs {
		data, err := canonicalJSON(segmentMessageFromDB(msg))
		if err != nil {
			return nil, fmt.Errorf("encoding message segment: %w", err)
		}
		buf.Write(data)
	}
	return buf.Bytes(), nil
}

func decodeSegment(data []byte) ([]db.Message, error) {
	return decodeSegmentWithLimits(data, productionArtifactLimits())
}

func decodeSegmentWithLimits(data []byte, limits artifactLimits) ([]db.Message, error) {
	preflight, err := preflightSegmentData(data, limits)
	if err != nil {
		return nil, err
	}
	return decodePreflightedSegment(preflight)
}

func decodePreflightedSegment(preflight segmentPreflight) ([]db.Message, error) {
	msgs := make([]db.Message, 0, len(preflight.records))
	for _, line := range preflight.records {
		var record segmentMessage
		if err := json.Unmarshal(line, &record); err != nil {
			return nil, fmt.Errorf("decoding message segment: %w", err)
		}
		msgs = append(msgs, record.dbMessage())
	}
	return msgs, nil
}

func segmentRecords(data []byte, limit int) ([][]byte, error) {
	capacity := min(max(limit, 0), 64)
	records := make([][]byte, 0, capacity)
	remaining := data
	lineNumber := 0
	for len(remaining) > 0 {
		lineNumber++
		newline := bytes.IndexByte(remaining, '\n')
		line := remaining
		if newline >= 0 {
			line = remaining[:newline]
			remaining = remaining[newline+1:]
		} else {
			remaining = nil
		}
		if len(records) >= limit {
			return nil, fmt.Errorf(
				"message record limit exceeded: limit %d per segment", limit,
			)
		}
		if len(bytes.TrimSpace(line)) == 0 {
			return nil, fmt.Errorf("blank message record at line %d", lineNumber)
		}
		records = append(records, line)
	}
	return records, nil
}

func preflightSegmentData(data []byte, limits artifactLimits) (segmentPreflight, error) {
	records, err := segmentRecords(data, limits.segmentMessages)
	if err != nil {
		return segmentPreflight{}, err
	}
	preflight := segmentPreflight{records: records}
	for _, line := range records {
		var header struct {
			Version int `json:"v"`
		}
		if err := json.Unmarshal(line, &header); err != nil {
			return segmentPreflight{}, fmt.Errorf("decoding message segment header: %w", err)
		}
		if header.Version > formatVersion {
			return segmentPreflight{}, fmt.Errorf(
				"%w: message segment has artifact version %d",
				errFutureArtifactVersion, header.Version,
			)
		}
		if header.Version != formatVersion {
			return segmentPreflight{}, fmt.Errorf(
				"message segment has unsupported artifact version %d",
				header.Version,
			)
		}
		messageNested, err := preflightMessageNestedCollections(line, limits)
		if err != nil {
			return segmentPreflight{}, err
		}
		if exceedsCollectionLimit(
			preflight.nested.toolCalls,
			messageNested.toolCalls,
			limits.segmentToolCalls,
		) {
			return segmentPreflight{}, fmt.Errorf(
				"segment tool call limit exceeded: limit %d", limits.segmentToolCalls,
			)
		}
		if exceedsCollectionLimit(
			preflight.nested.resultEvents,
			messageNested.resultEvents,
			limits.segmentResultEvents,
		) {
			return segmentPreflight{}, fmt.Errorf(
				"segment result event limit exceeded: limit %d",
				limits.segmentResultEvents,
			)
		}
		preflight.nested.toolCalls += messageNested.toolCalls
		preflight.nested.resultEvents += messageNested.resultEvents
	}
	return preflight, nil
}

func preflightMessageNestedCollections(
	line []byte,
	limits artifactLimits,
) (nestedCollectionCounts, error) {
	var envelope struct {
		Ordinal   int             `json:"ordinal"`
		ToolCalls json.RawMessage `json:"tool_calls"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		return nestedCollectionCounts{}, fmt.Errorf(
			"decoding message segment collections: %w", err,
		)
	}
	return preflightToolCallCollections(envelope.ToolCalls, envelope.Ordinal, limits)
}

func preflightToolCallCollections(
	data json.RawMessage,
	ordinal int,
	limits artifactLimits,
) (nestedCollectionCounts, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nestedCollectionCounts{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	token, err := dec.Token()
	if err != nil {
		return nestedCollectionCounts{}, err
	}
	if token != json.Delim('[') {
		return nestedCollectionCounts{}, errors.New("message tool_calls must be an array")
	}
	counts := nestedCollectionCounts{}
	for dec.More() {
		if counts.toolCalls >= limits.messageToolCalls {
			return nestedCollectionCounts{}, fmt.Errorf(
				"tool call limit exceeded for message ordinal %d: limit %d per message",
				ordinal, limits.messageToolCalls,
			)
		}
		var toolEnvelope struct {
			ResultEvents json.RawMessage `json:"result_events"`
		}
		if err := dec.Decode(&toolEnvelope); err != nil {
			return nestedCollectionCounts{}, fmt.Errorf(
				"decoding tool call %d in message ordinal %d: %w",
				counts.toolCalls, ordinal, err,
			)
		}
		resultEvents, err := countJSONArrayElements(
			toolEnvelope.ResultEvents, "result event", limits.toolResultEvents,
		)
		if err != nil {
			return nestedCollectionCounts{}, fmt.Errorf(
				"preflighting tool call %d in message ordinal %d: %w",
				counts.toolCalls, ordinal, err,
			)
		}
		counts.toolCalls++
		counts.resultEvents += resultEvents
	}
	if _, err := dec.Token(); err != nil {
		return nestedCollectionCounts{}, err
	}
	return counts, nil
}

func dbUsageEvents(events []artifactUsageEvent, sessionID string) []db.UsageEvent {
	out := make([]db.UsageEvent, len(events))
	for i, ev := range events {
		out[i] = db.UsageEvent{
			SessionID:                sessionID,
			MessageOrdinal:           ev.MessageOrdinal,
			Source:                   ev.Source,
			Model:                    ev.Model,
			InputTokens:              ev.InputTokens,
			OutputTokens:             ev.OutputTokens,
			CacheCreationInputTokens: ev.CacheCreationInputTokens,
			CacheReadInputTokens:     ev.CacheReadInputTokens,
			ReasoningTokens:          ev.ReasoningTokens,
			CostUSD:                  ev.CostUSD,
			CostStatus:               ev.CostStatus,
			CostSource:               ev.CostSource,
			OccurredAt:               ev.OccurredAt,
			DedupKey:                 ev.DedupKey,
		}
	}
	return out
}

func segmentMessageFromDB(msg db.Message) segmentMessage {
	record := segmentMessage{
		Version:           formatVersion,
		Ordinal:           msg.Ordinal,
		Role:              msg.Role,
		Content:           msg.Content,
		ThinkingText:      msg.ThinkingText,
		Timestamp:         msg.Timestamp,
		HasThinking:       msg.HasThinking,
		HasToolUse:        msg.HasToolUse,
		ContentLength:     msg.ContentLength,
		Model:             msg.Model,
		TokenUsage:        msg.TokenUsage,
		ContextTokens:     msg.ContextTokens,
		OutputTokens:      msg.OutputTokens,
		HasContextTokens:  msg.HasContextTokens,
		HasOutputTokens:   msg.HasOutputTokens,
		ClaudeMessageID:   msg.ClaudeMessageID,
		ClaudeRequestID:   msg.ClaudeRequestID,
		IsSystem:          msg.IsSystem,
		SourceType:        msg.SourceType,
		SourceSubtype:     msg.SourceSubtype,
		SourceUUID:        msg.SourceUUID,
		SourceParentUUID:  msg.SourceParentUUID,
		IsSidechain:       msg.IsSidechain,
		IsCompactBoundary: msg.IsCompactBoundary,
	}
	if len(msg.ToolCalls) > 0 {
		record.ToolCalls = make([]segmentToolCall, len(msg.ToolCalls))
		for i, call := range msg.ToolCalls {
			record.ToolCalls[i] = segmentToolCall{
				CallIndex:           i,
				ToolName:            call.ToolName,
				Category:            call.Category,
				ToolUseID:           call.ToolUseID,
				InputJSON:           call.InputJSON,
				FilePath:            call.FilePath,
				SkillName:           call.SkillName,
				ResultContentLength: call.ResultContentLength,
				ResultContent:       call.ResultContent,
				SubagentSessionID:   call.SubagentSessionID,
			}
			if len(call.ResultEvents) > 0 {
				record.ToolCalls[i].ResultEvents = make([]segmentResultEvent, len(call.ResultEvents))
				for j, ev := range call.ResultEvents {
					record.ToolCalls[i].ResultEvents[j] = segmentResultEvent{
						ToolUseID:         ev.ToolUseID,
						AgentID:           ev.AgentID,
						SubagentSessionID: ev.SubagentSessionID,
						Source:            ev.Source,
						Status:            ev.Status,
						Content:           ev.Content,
						ContentLength:     ev.ContentLength,
						Timestamp:         ev.Timestamp,
						EventIndex:        ev.EventIndex,
					}
				}
			}
		}
	}
	return record
}

func (m segmentMessage) dbMessage() db.Message {
	msg := db.Message{
		Ordinal:           m.Ordinal,
		Role:              m.Role,
		Content:           m.Content,
		ThinkingText:      m.ThinkingText,
		Timestamp:         m.Timestamp,
		HasThinking:       m.HasThinking,
		HasToolUse:        m.HasToolUse,
		ContentLength:     m.ContentLength,
		Model:             m.Model,
		TokenUsage:        m.TokenUsage,
		ContextTokens:     m.ContextTokens,
		OutputTokens:      m.OutputTokens,
		HasContextTokens:  m.HasContextTokens,
		HasOutputTokens:   m.HasOutputTokens,
		ClaudeMessageID:   m.ClaudeMessageID,
		ClaudeRequestID:   m.ClaudeRequestID,
		IsSystem:          m.IsSystem,
		SourceType:        m.SourceType,
		SourceSubtype:     m.SourceSubtype,
		SourceUUID:        m.SourceUUID,
		SourceParentUUID:  m.SourceParentUUID,
		IsSidechain:       m.IsSidechain,
		IsCompactBoundary: m.IsCompactBoundary,
	}
	if len(m.ToolCalls) > 0 {
		msg.ToolCalls = make([]db.ToolCall, len(m.ToolCalls))
		for i, call := range m.ToolCalls {
			msg.ToolCalls[i] = db.ToolCall{
				ToolName:            call.ToolName,
				Category:            call.Category,
				ToolUseID:           call.ToolUseID,
				InputJSON:           call.InputJSON,
				FilePath:            call.FilePath,
				SkillName:           call.SkillName,
				ResultContentLength: call.ResultContentLength,
				ResultContent:       call.ResultContent,
				SubagentSessionID:   call.SubagentSessionID,
			}
			if len(call.ResultEvents) > 0 {
				msg.ToolCalls[i].ResultEvents = make([]db.ToolResultEvent, len(call.ResultEvents))
				for j, ev := range call.ResultEvents {
					msg.ToolCalls[i].ResultEvents[j] = db.ToolResultEvent{
						ToolUseID:         ev.ToolUseID,
						AgentID:           ev.AgentID,
						SubagentSessionID: ev.SubagentSessionID,
						Source:            ev.Source,
						Status:            ev.Status,
						Content:           ev.Content,
						ContentLength:     ev.ContentLength,
						Timestamp:         ev.Timestamp,
						EventIndex:        ev.EventIndex,
					}
				}
			}
		}
	}
	return msg
}

func canonicalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := writeCanonicalJSON(&buf, reflect.ValueOf(v)); err != nil {
		return nil, fmt.Errorf("encoding canonical artifact JSON: %w", err)
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

func writeCanonicalJSON(buf *bytes.Buffer, v reflect.Value) error {
	if !v.IsValid() {
		buf.WriteString("null")
		return nil
	}
	if v.Kind() == reflect.Interface {
		if v.IsNil() {
			buf.WriteString("null")
			return nil
		}
		return writeCanonicalJSON(buf, v.Elem())
	}
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			buf.WriteString("null")
			return nil
		}
		return writeCanonicalJSON(buf, v.Elem())
	}
	if v.Type() == reflect.TypeFor[json.RawMessage]() {
		raw := v.Interface().(json.RawMessage)
		if len(raw) == 0 {
			buf.WriteString("null")
			return nil
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var decoded any
		if err := dec.Decode(&decoded); err != nil {
			return err
		}
		return writeCanonicalJSON(buf, reflect.ValueOf(decoded))
	}
	if v.Type() == reflect.TypeFor[json.Number]() {
		buf.WriteString(v.Interface().(json.Number).String())
		return nil
	}
	switch v.Kind() {
	case reflect.Bool:
		buf.WriteString(strconv.FormatBool(v.Bool()))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		buf.WriteString(strconv.FormatInt(v.Int(), 10))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		buf.WriteString(strconv.FormatUint(v.Uint(), 10))
	case reflect.Float32, reflect.Float64:
		data, err := json.Marshal(v.Interface())
		if err != nil {
			return err
		}
		buf.Write(data)
	case reflect.String:
		data, err := json.Marshal(v.String())
		if err != nil {
			return err
		}
		buf.Write(data)
	case reflect.Slice, reflect.Array:
		buf.WriteByte('[')
		for i := 0; i < v.Len(); i++ {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonicalJSON(buf, v.Index(i)); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case reflect.Map:
		return writeCanonicalMap(buf, v)
	case reflect.Struct:
		return writeCanonicalStruct(buf, v)
	default:
		return fmt.Errorf("unsupported canonical JSON kind %s", v.Kind())
	}
	return nil
}

func writeCanonicalMap(buf *bytes.Buffer, v reflect.Value) error {
	if v.IsNil() {
		buf.WriteString("null")
		return nil
	}
	if v.Type().Key().Kind() != reflect.String {
		return fmt.Errorf("unsupported canonical map key type %s", v.Type().Key())
	}
	keys := make([]string, 0, v.Len())
	for _, key := range v.MapKeys() {
		keys = append(keys, key.String())
	}
	sort.Strings(keys)
	buf.WriteByte('{')
	for i, key := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		keyData, err := json.Marshal(key)
		if err != nil {
			return err
		}
		buf.Write(keyData)
		buf.WriteByte(':')
		if err := writeCanonicalJSON(buf, v.MapIndex(reflect.ValueOf(key))); err != nil {
			return err
		}
	}
	buf.WriteByte('}')
	return nil
}

type canonicalField struct {
	name  string
	value reflect.Value
}

func writeCanonicalStruct(buf *bytes.Buffer, v reflect.Value) error {
	fields := make([]canonicalField, 0, v.NumField())
	t := v.Type()
	for i := 0; i < v.NumField(); i++ {
		field := t.Field(i)
		if field.PkgPath != "" {
			continue
		}
		name, omitEmpty, skip := jsonField(field)
		if skip {
			continue
		}
		value := v.Field(i)
		if omitEmpty && isCanonicalEmpty(value) {
			continue
		}
		fields = append(fields, canonicalField{name: name, value: value})
	}
	sort.Slice(fields, func(i, j int) bool {
		return fields[i].name < fields[j].name
	})

	buf.WriteByte('{')
	for i, field := range fields {
		if i > 0 {
			buf.WriteByte(',')
		}
		name, err := json.Marshal(field.name)
		if err != nil {
			return err
		}
		buf.Write(name)
		buf.WriteByte(':')
		if err := writeCanonicalJSON(buf, field.value); err != nil {
			return err
		}
	}
	buf.WriteByte('}')
	return nil
}

func jsonField(field reflect.StructField) (name string, omitEmpty bool, skip bool) {
	name = field.Name
	tag := field.Tag.Get("json")
	if tag == "-" {
		return "", false, true
	}
	if tag == "" {
		return name, false, false
	}
	parts := strings.Split(tag, ",")
	if parts[0] != "" {
		name = parts[0]
	}
	for _, opt := range parts[1:] {
		if opt == "omitempty" {
			omitEmpty = true
		}
	}
	return name, omitEmpty, false
}

func isCanonicalEmpty(v reflect.Value) bool {
	if !v.IsValid() {
		return true
	}
	switch v.Kind() {
	case reflect.Array:
		return v.Len() == 0
	case reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	case reflect.Bool:
		return !v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return v.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return v.Float() == 0
	case reflect.Interface, reflect.Pointer:
		return v.IsNil()
	}
	return false
}

func hashHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// reconcileArtifactConflict handles a same-name, different-content pair. For a
// recognized artifact whose source validates and destination does not, the
// destination is repaired in place; a corrupt source is skipped instead of
// mirrored. Everything else keeps the write-once conflict error.
func isTempArtifactEntry(name string) bool {
	return strings.HasPrefix(name, tempFilePrefix)
}

// IsFolderTarget reports whether target is a local filesystem target rather
// than a future HTTP or object-store target.
func IsFolderTarget(target string) bool {
	if target == "" || strings.Contains(target, "://") {
		return false
	}
	if isWindowsDrivePath(target) {
		return true
	}
	_, _, err := net.SplitHostPort(target)
	return err != nil
}

func isWindowsDrivePath(target string) bool {
	if len(target) < 3 || target[1] != ':' {
		return false
	}
	c := target[0]
	if (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') {
		return false
	}
	return target[2] == '\\' || target[2] == '/'
}
