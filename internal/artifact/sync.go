package artifact

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/klauspost/compress/zstd"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

const (
	formatVersion     = 1
	originStateKey    = "artifact_origin_id"
	importStatePrefix = "artifact_import:"
	exportStatePrefix = "artifact_export:"
	metaStatePrefix   = "artifact_meta_import:"
	tempFilePrefix    = ".tmp-"
	manifestExtension = ".json.zst"
	segmentExtension  = ".ndjson.zst"
)

var errIncompleteArtifact = errors.New("incomplete artifact")
var errFutureArtifactVersion = errors.New("future artifact version")

var writeFileAtomicBeforeCommit func(path string)
var writeFileAtomicLink = os.Link

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
	// OnDataChanged is called after a foreign import writes local rows.
	OnDataChanged func()
}

// Sync runs one artifact sync, selecting the transport from the target shape:
// an http(s):// URL uses the HTTP peer transport, anything else is treated as a
// local folder target.
func Sync(ctx context.Context, database *db.DB, opts SyncOptions) (SyncResult, error) {
	if opts.Target == "" {
		return SyncResult{}, errors.New("artifact sync target is required")
	}
	if IsHTTPTarget(opts.Target) {
		tr, err := newHTTPTransport(opts.Target, opts.Token)
		if err != nil {
			return SyncResult{}, err
		}
		return syncWithTransport(ctx, database, opts, tr)
	}
	if IsObjectTarget(opts.Target) {
		tr, err := newObjectTransport(opts.Target, ObjectStoreOptionsFromEnv())
		if err != nil {
			return SyncResult{}, err
		}
		return syncWithTransport(ctx, database, opts, tr)
	}
	return syncWithTransport(ctx, database, opts, &folderTransport{target: opts.Target})
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
}

// Changed reports whether the import wrote user-visible local data.
func (r ImportResult) Changed() bool {
	return r.Sessions > 0 || r.Messages > 0 || r.Metadata > 0
}

// SyncFolder exports local sessions to the local artifact store, exchanges the
// store with target, and imports foreign origins from the exchanged artifacts.
func SyncFolder(ctx context.Context, database *db.DB, opts SyncOptions) (SyncResult, error) {
	if opts.Target == "" {
		return SyncResult{}, errors.New("artifact sync target is required")
	}
	return syncWithTransport(ctx, database, opts, &folderTransport{target: opts.Target})
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
) (SyncResult, error) {
	if opts.DataDir == "" {
		return SyncResult{}, errors.New("artifact sync data dir is required")
	}
	localRoot := filepath.Join(opts.DataDir, "artifacts")
	if err := tr.Prepare(localRoot); err != nil {
		return SyncResult{}, err
	}
	if err := os.MkdirAll(localRoot, 0o755); err != nil {
		return SyncResult{}, fmt.Errorf("creating local artifact store: %w", err)
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

	exported, err := Export(ctx, database, localRoot, origin)
	if err != nil {
		return SyncResult{}, err
	}
	if err := tr.Exchange(ctx, localRoot); err != nil {
		return SyncResult{}, err
	}
	clock := NewHLCClock(database, HLCClockOptions{Now: opts.Now})
	imported, err := importDetailed(ctx, database, clock, localRoot, origin)
	if err != nil {
		return SyncResult{}, err
	}
	if imported.Changed() && opts.OnDataChanged != nil {
		opts.OnDataChanged()
	}
	return SyncResult{
		Origin:           origin,
		ExportedSessions: exported,
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
	if localAbs == targetAbs || pathContains(localAbs, targetAbs) || pathContains(targetAbs, localAbs) {
		return fmt.Errorf(
			"artifact sync target %s must not overlap local artifact store %s",
			targetAbs, localAbs,
		)
	}
	return nil
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
	Session         db.Session           `json:"session"`
	SessionName     *string              `json:"session_name,omitempty"`
	Segments        []string             `json:"segments"`
	UsageEvents     []artifactUsageEvent `json:"usage_events,omitempty"`
	RawSource       *rawSourceRef        `json:"raw_source,omitempty"`
	DataVersion     int                  `json:"data_version"`
	Generation      int                  `json:"generation"`
	// Signal state persisted on the session row but dropped from the Session
	// JSON above, which omits these fields (json:"-"). Carried explicitly so an
	// imported session keeps its tool-call, context, and quality signal state
	// instead of resetting to false/zero. Secret-scan state is deliberately not
	// carried: findings live outside the manifest, so imported sessions are
	// treated as unscanned (see rewriteForImport).
	SessionHasToolCalls   bool               `json:"session_has_tool_calls,omitempty"`
	SessionHasContextData bool               `json:"session_has_context_data,omitempty"`
	SessionQualitySignals *db.QualitySignals `json:"session_quality_signals,omitempty"`
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

// Export writes current machine-owned sessions into root/origin.
func Export(ctx context.Context, database *db.DB, root, origin string) (int, error) {
	if err := validateOriginID(origin); err != nil {
		return 0, err
	}
	originRoot := filepath.Join(root, origin)
	for _, dir := range []string{"checkpoints", "manifests", "segments", "meta", "raw"} {
		if err := os.MkdirAll(filepath.Join(originRoot, dir), 0o755); err != nil {
			return 0, fmt.Errorf("creating %s: %w", dir, err)
		}
	}

	// Enumerate owned sessions from a raw query rather than the sidebar list API
	// so usage-only (zero-message) sessions are still exported.
	ids, err := database.ListOwnedSessionIDsForExport(ctx)
	if err != nil {
		return 0, err
	}
	var exported int
	sessions := map[string]string{}
	for _, id := range ids {
		stateKey := exportStateKey(origin, id)
		prevHash, err := database.GetSyncState(stateKey)
		if err != nil {
			return 0, fmt.Errorf("reading export state for %s: %w", id, err)
		}
		hash, changed, err := exportSession(ctx, database, originRoot, origin, id, prevHash)
		if err != nil {
			return 0, err
		}
		sessions[origin+"~"+id] = hash
		if changed {
			if err := database.SetSyncState(stateKey, hash); err != nil {
				return 0, fmt.Errorf("writing export state for %s: %w", id, err)
			}
			exported++
		}
	}
	seq, err := nextCheckpointSequence(originRoot)
	if err != nil {
		return 0, err
	}
	cp := checkpoint{Version: formatVersion, Origin: origin, Sequence: seq, Sessions: sessions}
	data, err := canonicalJSON(cp)
	if err != nil {
		return 0, err
	}
	checkpointPath := filepath.Join(originRoot, "checkpoints", fmt.Sprintf("cp-%010d.json", seq))
	if err := writeFileAtomic(checkpointPath, data, 0o644); err != nil {
		return 0, fmt.Errorf("writing checkpoint: %w", err)
	}
	return exported, nil
}

func nextCheckpointSequence(originRoot string) (int, error) {
	cp, err := readLatestCheckpoint(originRoot)
	if err != nil {
		return 0, err
	}
	if cp == nil {
		return 1, nil
	}
	return cp.Sequence + 1, nil
}

func exportStateKey(origin, sessionID string) string {
	return exportStatePrefix + origin + ":" + sessionID
}

func exportSession(
	ctx context.Context,
	database *db.DB,
	originRoot, origin, sessionID, prevManifestHash string,
) (string, bool, error) {
	sess, err := database.GetSessionFull(ctx, sessionID)
	if err != nil {
		return "", false, fmt.Errorf("loading session %s for artifact export: %w", sessionID, err)
	}
	if sess == nil {
		return "", false, fmt.Errorf("session %s disappeared during artifact export", sessionID)
	}
	msgs, err := database.GetAllMessages(ctx, sessionID)
	if err != nil {
		return "", false, fmt.Errorf("loading messages for artifact export %s: %w", sessionID, err)
	}
	usageEvents, err := database.GetUsageEvents(ctx, sessionID)
	if err != nil {
		return "", false, fmt.Errorf("loading usage events for artifact export %s: %w", sessionID, err)
	}

	segmentData, err := encodeSegment(canonicalMessages(msgs))
	if err != nil {
		return "", false, err
	}
	segmentHash := hashHex(segmentData)

	sess.Machine = origin
	normalizeManifestSessionLocalState(sess)
	m := manifest{
		Version:               formatVersion,
		Origin:                origin,
		NativeSessionID:       sessionID,
		Session:               *sess,
		SessionName:           sess.SessionName,
		Segments:              []string{segmentHash},
		UsageEvents:           canonicalUsageEvents(usageEvents),
		DataVersion:           sess.DataVersion,
		Generation:            1,
		SessionHasToolCalls:   sess.HasToolCalls,
		SessionHasContextData: sess.HasContextData,
		SessionQualitySignals: sess.StoredQualitySignals(),
	}
	manifestData, err := canonicalJSON(m)
	if err != nil {
		return "", false, err
	}
	manifestHash := hashHex(manifestData)
	if compatible, err := previousManifestMatchesAfterLocalStateNormalization(
		originRoot, prevManifestHash, manifestHash,
	); err != nil {
		return "", false, err
	} else if compatible {
		return prevManifestHash, false, nil
	}
	manifestPath := filepath.Join(originRoot, "manifests", manifestHash+manifestExtension)
	segmentPath := filepath.Join(originRoot, "segments", segmentHash+segmentExtension)
	if prevManifestHash == manifestHash && artifactFileExists(manifestPath) && artifactFileExists(segmentPath) {
		return manifestHash, false, nil
	}
	if err := writeCompressed(segmentPath, segmentData); err != nil {
		return "", false, fmt.Errorf("writing segment for %s: %w", sessionID, err)
	}
	if err := writeCompressed(manifestPath, manifestData); err != nil {
		return "", false, fmt.Errorf("writing manifest for %s: %w", sessionID, err)
	}
	return manifestHash, true, nil
}

func normalizeManifestSessionLocalState(sess *db.Session) {
	// Keep non-content, machine-local state out of the canonical manifest so a
	// source-only change to it does not alter the content hash and trigger a
	// re-import that clears the importer's local findings. secret_leak_count is
	// import-discarded secret state (see rewriteForImport); local_modified_at is
	// the local sync watermark, which import ignores (the importer stamps its
	// own) -- and a secret rescan bumps both even when no exported message
	// content changed.
	sess.SecretLeakCount = 0
	sess.LocalModifiedAt = nil
}

func previousManifestMatchesAfterLocalStateNormalization(
	originRoot, prevManifestHash, currentManifestHash string,
) (bool, error) {
	if prevManifestHash == "" || prevManifestHash == currentManifestHash {
		return false, nil
	}
	prev, err := readManifest(originRoot, prevManifestHash)
	if err != nil {
		return false, nil
	}
	for _, segmentHash := range prev.Segments {
		segmentPath := filepath.Join(originRoot, "segments", segmentHash+segmentExtension)
		if !artifactFileExists(segmentPath) {
			return false, nil
		}
	}
	normalizeManifestSessionLocalState(&prev.Session)
	data, err := canonicalJSON(prev)
	if err != nil {
		return false, err
	}
	return hashHex(data) == currentManifestHash, nil
}

func artifactFileExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Mode().IsRegular()
}

// Import reads every foreign origin under root and imports referenced sessions.
func Import(ctx context.Context, database *db.DB, root, localOrigin string) (int, int, error) {
	res, err := ImportDetailed(ctx, database, root, localOrigin)
	return res.Sessions, res.Messages, err
}

// ImportDetailed reads every foreign origin under root and imports referenced
// sessions plus metadata events.
func ImportDetailed(ctx context.Context, database *db.DB, root, localOrigin string) (ImportResult, error) {
	return importDetailed(ctx, database, nil, root, localOrigin)
}

// importDetailed imports foreign origins, advancing clock past observed remote
// metadata HLCs. When clock is nil a default clock backed by database is used so
// the persisted metadata clock is still advanced.
func importDetailed(
	ctx context.Context,
	database *db.DB,
	clock *HLCClock,
	root, localOrigin string,
) (ImportResult, error) {
	if clock == nil {
		clock = NewHLCClock(database, HLCClockOptions{})
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ImportResult{}, nil
		}
		return ImportResult{}, fmt.Errorf("reading artifact roots: %w", err)
	}
	var res ImportResult
	for _, ent := range entries {
		if !ent.IsDir() || ent.Name() == localOrigin {
			continue
		}
		originRes, err := importOrigin(ctx, database, clock, filepath.Join(root, ent.Name()), ent.Name(), localOrigin)
		if err != nil {
			return res, err
		}
		res.Sessions += originRes.Sessions
		res.Messages += originRes.Messages
		res.Metadata += originRes.Metadata
	}
	return res, nil
}

func importOrigin(ctx context.Context, database *db.DB, clock *HLCClock, originRoot, origin, localOrigin string) (ImportResult, error) {
	cp, err := readLatestCompatibleCheckpoint(originRoot, origin)
	if err != nil {
		return ImportResult{}, fmt.Errorf("reading checkpoint for %s: %w", origin, err)
	}
	var res ImportResult
	if cp != nil {
		keys := make([]string, 0, len(cp.Sessions))
		for k := range cp.Sessions {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		for _, gid := range keys {
			if err := ctx.Err(); err != nil {
				return res, err
			}
			manifestHash := cp.Sessions[gid]
			stateKey := importStateKey(origin, gid)
			prevHash, err := database.GetSyncState(stateKey)
			if err != nil {
				return res, fmt.Errorf("reading import state for %s: %w", gid, err)
			}
			if prevHash == manifestHash {
				continue
			}
			m, err := readManifest(originRoot, manifestHash)
			if err != nil {
				if errors.Is(err, errIncompleteArtifact) {
					continue
				}
				return res, err
			}
			if err := validateManifest(m, origin, gid); err != nil {
				if errors.Is(err, errFutureArtifactVersion) {
					continue
				}
				return res, err
			}
			msgs, err := readManifestMessages(originRoot, m)
			if err != nil {
				if errors.Is(err, errIncompleteArtifact) || errors.Is(err, errFutureArtifactVersion) {
					continue
				}
				return res, err
			}
			write := rewriteForImport(m, msgs)
			writeRes, err := database.WriteSessionBatchAtomic([]db.SessionBatchWrite{write})
			if err != nil {
				if errors.Is(err, db.ErrSessionExcluded) || errors.Is(err, db.ErrSessionTrashed) {
					continue
				}
				return res, fmt.Errorf("importing artifact session %s: %w", gid, err)
			}
			res.Sessions += writeRes.WrittenSessions
			res.Messages += writeRes.WrittenMessages
			if err := database.SetSyncState(stateKey, manifestHash); err != nil {
				return res, fmt.Errorf("writing import state for %s: %w", gid, err)
			}
		}
	}
	metadata, err := replayMetadata(ctx, database, clock, originRoot, origin, localOrigin)
	if err != nil {
		return res, err
	}
	res.Metadata += metadata
	return res, nil
}

func importStateKey(origin, gid string) string {
	return importStatePrefix + origin + ":" + gid
}

func metaStateKey(origin string) string {
	return metaStatePrefix + origin
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
	return nil
}

func rewriteForImport(m manifest, msgs []db.Message) db.SessionBatchWrite {
	importedID := m.Origin + "~" + m.NativeSessionID
	sess := m.Session
	sess.ID = importedID
	sess.Machine = m.Origin
	sess.SessionName = m.SessionName
	// Restore signal state dropped from the Session JSON; signalsFromSession
	// reads these fields below to persist the imported session's signal columns.
	sess.HasToolCalls = m.SessionHasToolCalls
	sess.HasContextData = m.SessionHasContextData
	sess.ApplyQualitySignals(m.SessionQualitySignals)
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

func readLatestCheckpoint(originRoot string) (*checkpoint, error) {
	paths, err := filepath.Glob(filepath.Join(originRoot, "checkpoints", "cp-*.json"))
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, nil
	}
	sort.Strings(paths)
	data, err := os.ReadFile(paths[len(paths)-1])
	if err != nil {
		return nil, err
	}
	var cp checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, err
	}
	return &cp, nil
}

func readLatestCompatibleCheckpoint(originRoot, origin string) (*checkpoint, error) {
	paths, err := filepath.Glob(filepath.Join(originRoot, "checkpoints", "cp-*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	for _, v := range slices.Backward(paths) {
		data, err := os.ReadFile(v)
		if err != nil {
			return nil, err
		}
		var cp checkpoint
		if err := json.Unmarshal(data, &cp); err != nil {
			return nil, err
		}
		if err := validateCheckpoint(&cp, origin); err != nil {
			if errors.Is(err, errFutureArtifactVersion) {
				continue
			}
			return nil, err
		}
		return &cp, nil
	}
	return nil, nil
}

func readManifest(originRoot, hash string) (manifest, error) {
	data, err := readCompressed(filepath.Join(originRoot, "manifests", hash+manifestExtension))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return manifest{}, fmt.Errorf("%w: manifest %s", errIncompleteArtifact, hash)
		}
		return manifest{}, fmt.Errorf("reading manifest %s: %w", hash, err)
	}
	if got := hashHex(data); got != hash {
		return manifest{}, fmt.Errorf("manifest %s hash mismatch: got %s", hash, got)
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return manifest{}, fmt.Errorf("decoding manifest %s: %w", hash, err)
	}
	return m, nil
}

func readManifestMessages(originRoot string, m manifest) ([]db.Message, error) {
	var msgs []db.Message
	for _, segmentHash := range m.Segments {
		data, err := readCompressed(filepath.Join(originRoot, "segments", segmentHash+segmentExtension))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("%w: segment %s", errIncompleteArtifact, segmentHash)
			}
			return nil, fmt.Errorf("reading segment %s: %w", segmentHash, err)
		}
		if got := hashHex(data); got != segmentHash {
			return nil, fmt.Errorf("segment %s hash mismatch: got %s", segmentHash, got)
		}
		segmentMsgs, err := decodeSegment(data)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, segmentMsgs...)
	}
	return msgs, nil
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
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	var msgs []db.Message
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var record segmentMessage
		if err := json.Unmarshal(line, &record); err != nil {
			return nil, fmt.Errorf("decoding message segment: %w", err)
		}
		if record.Version > formatVersion {
			return nil, fmt.Errorf(
				"%w: message segment has artifact version %d",
				errFutureArtifactVersion, record.Version,
			)
		}
		if record.Version != formatVersion {
			return nil, fmt.Errorf(
				"message segment has unsupported artifact version %d",
				record.Version,
			)
		}
		msgs = append(msgs, record.dbMessage())
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading message segment: %w", err)
	}
	return msgs, nil
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
	case reflect.Struct:
		return writeCanonicalStruct(buf, v)
	default:
		return fmt.Errorf("unsupported canonical JSON kind %s", v.Kind())
	}
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

func writeCompressed(path string, data []byte) error {
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf)
	if err != nil {
		return err
	}
	if _, err := enc.Write(data); err != nil {
		enc.Close()
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return writeFileAtomic(path, buf.Bytes(), 0o644)
}

func readCompressed(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	return io.ReadAll(dec)
}

func writeFileAtomic(path string, data []byte, perm fs.FileMode) error {
	if done, err := existingArtifactMatches(path, data); err != nil || done {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), tempFilePrefix+"*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if done, err := existingArtifactMatches(path, data); err != nil || done {
		return err
	}
	if writeFileAtomicBeforeCommit != nil {
		writeFileAtomicBeforeCommit(path)
	}
	if err := writeFileAtomicLink(tmpName, path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			if done, matchErr := existingArtifactMatches(path, data); matchErr != nil || done {
				return matchErr
			}
			return fmt.Errorf("artifact path conflict at %s", path)
		}
		if isHardLinkUnsupported(err) {
			return writeFileNoReplace(path, data, perm)
		}
		return err
	}
	return nil
}

func isHardLinkUnsupported(err error) bool {
	return errors.Is(err, syscall.ENOTSUP) ||
		errors.Is(err, syscall.EOPNOTSUPP) ||
		errors.Is(err, syscall.EXDEV) ||
		errors.Is(err, syscall.EPERM)
}

func writeFileNoReplace(path string, data []byte, perm fs.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			if done, matchErr := existingArtifactMatches(path, data); matchErr != nil || done {
				return matchErr
			}
			return fmt.Errorf("artifact path conflict at %s", path)
		}
		return err
	}
	created := true
	defer func() {
		if created {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Chmod(perm); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	created = false
	return nil
}

func existingArtifactMatches(path string, data []byte) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if info.IsDir() {
		return false, fmt.Errorf("artifact destination %s is a directory", path)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("artifact destination %s is not a regular file", path)
	}
	existing, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	if bytes.Equal(existing, data) {
		return true, nil
	}
	return false, fmt.Errorf("artifact path conflict at %s", path)
}

// CopyUnion copies files from src into dst without deleting files that only
// exist in dst. Existing identical files are left in place.
func CopyUnion(src, dst string) error {
	return filepath.WalkDir(src, func(path string, ent fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if isTempArtifactEntry(ent.Name()) {
			if ent.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		to := filepath.Join(dst, rel)
		if ent.IsDir() {
			return os.MkdirAll(to, 0o755)
		}
		info, err := ent.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("artifact source %s is not a regular file", path)
		}
		if existing, err := os.Lstat(to); err == nil {
			if existing.IsDir() {
				return fmt.Errorf("artifact destination %s is a directory", to)
			}
			if !existing.Mode().IsRegular() {
				return fmt.Errorf("artifact destination %s is not a regular file", to)
			}
			if existing.Size() == info.Size() {
				same, err := sameFileContent(path, to)
				if err != nil {
					return err
				}
				if same {
					return nil
				}
			}
			return fmt.Errorf("artifact path conflict at %s", to)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		// Read the whole file and close it before writing the destination so a
		// large traversal never holds many source descriptors open at once.
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return writeFileAtomic(to, data, info.Mode().Perm())
	})
}

func isTempArtifactEntry(name string) bool {
	return strings.HasPrefix(name, tempFilePrefix)
}

func sameFileContent(a, b string) (bool, error) {
	aData, err := os.ReadFile(a)
	if err != nil {
		return false, err
	}
	bData, err := os.ReadFile(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(aData, bData), nil
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
