package parser

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	_ "github.com/mattn/go-sqlite3"
)

// Antigravity IDE sessions live under ~/.gemini/antigravity/:
//
//   conversations/<uuid>.db        SQLite, one per session
//   annotations/<uuid>.pbtxt       last_user_view_time + flags
//   brain/<uuid>/*.md(+.json)      plaintext task/plan artifacts
//   implicit/<uuid>.pb             encrypted (handled like CLI)
//
// We treat the .db as the canonical session file (like Gemini's
// per-session JSON). Each row of `steps` becomes one ParsedMessage.

const antigravityIDPrefix = "antigravity:"

var antigravityUUIDLikeRE = regexp.MustCompile(
	`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`,
)

// AntigravityFileInfo returns the effective file info for an IDE
// session .db, combining the main file with its -wal/-shm sidecars,
// the annotations/<id>.pbtxt sidecar, and the brain/<id> artifacts
// the parse renders as messages. WAL-only commits and annotation or
// brain updates do not touch the main file, so skip checks and
// persisted file metadata must use this composite or live sessions
// never reparse.
func AntigravityFileInfo(path string) (os.FileInfo, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	return antigravityCLICombinedFileInfo(
		info,
		antigravityIDECompanionPaths(path)...,
	), nil
}

func antigravityIDECompanionPaths(path string) []string {
	id := strings.TrimSuffix(filepath.Base(path), ".db")
	root := filepath.Dir(filepath.Dir(path))
	companions := []string{
		path + "-wal",
		path + "-shm",
		filepath.Join(root, "annotations", id+".pbtxt"),
		// The agy-reader trajectory sidecar is a transcript source for
		// IDE sessions too (see parseSession), so a sidecar write must
		// change the fingerprint even when the database files themselves
		// are untouched.
		strings.TrimSuffix(path, ".db") + ".trajectory.json",
	}
	return append(companions, antigravityBrainCompanions(
		filepath.Join(root, "brain", id),
	)...)
}

// parseSession parses one IDE session DB. It is owned by the
// antigravityProvider; the package-level ParseAntigravitySession
// entrypoint was folded onto the provider.
func (p *antigravityProvider) parseSession(
	path, project, machine string,
) (*ParsedSession, []ParsedMessage, []ParsedUsageEvent, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("stat %s: %w", path, err)
	}
	id := strings.TrimSuffix(filepath.Base(path), ".db")
	if !IsValidSessionID(id) {
		return nil, nil, nil, fmt.Errorf(
			"invalid Antigravity IDE session filename: %s", path,
		)
	}
	root := filepath.Dir(filepath.Dir(path))

	// Open read-only; SQLite session files have WAL/SHM
	// sidecars that the driver expects in the same dir.
	dsn := "file:" + sqliteURIPath(path) + "?mode=ro&immutable=0"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, nil, nil, fmt.Errorf(
			"open antigravity db %s: %w", path, err,
		)
	}
	defer db.Close()

	// Schema-fingerprint label for the producing agy build. Computed from
	// the open DB so IDE and CLI classify identically; empty when the
	// schema cannot be read.
	sourceVersion := antigravitySourceVersion(db)

	dbResult, err := loadAntigravityStepsWithRawCount(db)
	if err != nil {
		// Fail closed on an unreadable steps table, deliberately: a
		// covering sidecar cannot rescue an unreadable DB because
		// coverage is unprovable without the DB's raw step count (a
		// displayable sidecar may lag a live session), and this
		// provider force-replaces on success (the engine's
		// shouldReplaceFullParseMessages plus the unconditional
		// ForceReplace outcome), so any rescue would risk overwriting a
		// previously complete stored transcript with a stale sidecar
		// (roborev jobs 1982 and 2112, both high). Safe rescue needs
		// engine-level no-clobber support, tracked separately. The
		// parse error preserves stored data and the engine retries
		// failed files.
		return nil, nil, nil, err
	}
	messages := dbResult.messages
	// gen_metadata token usage describes the session's actual
	// consumption no matter which transcript source wins below. The
	// trajectory sidecar also extracts generatorMetadata usage, but the
	// .db gen_metadata events win and sidecar events only fill the gap
	// (missing gen_metadata table) so the same generation is never
	// counted twice -- mirroring the CLI path's merge behavior.
	usageEvents := dbResult.usageEvents
	hasGenMetadata := dbResult.hasGenMetadata
	// TranscriptFidelity is left empty (treated as full) for the heuristic
	// decode, matching prior IDE behavior; a covering sidecar sets it to
	// TranscriptFidelityFull explicitly below.
	transcriptFidelity := ""

	// Prefer the agy-reader trajectory sidecar: it is the daemon's own
	// decode, with structured tool calls/results and thinking, where the
	// heuristic DB decode only recovers loose strings. Selection is
	// content-based, not mtime-based: the sidecar wins only when it covers
	// at least as many steps as the raw DB decode, so a sidecar lagging
	// behind a live session loses until agy-reader catches up. When the
	// sidecar is absent, malformed, or fails the coverage gate the parser
	// falls back to the heuristic decode exactly as before.
	sidecarPath := strings.TrimSuffix(path, ".db") + ".trajectory.json"
	tRes, tErr := parseAntigravityCLITrajectory(
		sidecarPath, dbResult.executors,
	)
	sidecarOK := tErr == nil &&
		hasDisplayableAntigravityCLITrajectoryMessage(tRes.messages)
	sidecarCovers := dbResult.rawStepCount == 0 ||
		tRes.rawSteps >= dbResult.rawStepCount
	if sidecarOK && sidecarCovers {
		messages = tRes.messages
		transcriptFidelity = TranscriptFidelityFull
	}
	// Coverage gates usage just like the transcript: a lagging sidecar
	// carries only the generations it has seen, so persisting those would
	// underreport totals on a row that looks current. sidecarCovers stays
	// true when the DB offers no coverage signal (zero rows), so gap-fill
	// still applies there.
	if len(usageEvents) == 0 && tErr == nil && sidecarCovers {
		usageEvents = tRes.usageEvents
	}

	messages = append(messages,
		collectAntigravityBrainMessages(
			filepath.Join(root, "brain", id),
		)...,
	)

	sort.SliceStable(messages, func(i, j int) bool {
		return messages[i].Timestamp.Before(messages[j].Timestamp)
	})
	for i := range messages {
		messages[i].Ordinal = i
	}

	var firstMessage string
	var userCount int
	var startedAt, endedAt time.Time
	for _, m := range messages {
		if m.Role == RoleUser {
			userCount++
			if firstMessage == "" && m.Content != "" {
				firstMessage = truncate(
					strings.ReplaceAll(m.Content, "\n", " "),
					300,
				)
			}
		}
		if !m.Timestamp.IsZero() {
			if startedAt.IsZero() || m.Timestamp.Before(startedAt) {
				startedAt = m.Timestamp
			}
			if m.Timestamp.After(endedAt) {
				endedAt = m.Timestamp
			}
		}
	}
	if ann := readAntigravityAnnotation(
		filepath.Join(root, "annotations", id+".pbtxt"),
	); !ann.IsZero() && ann.After(endedAt) {
		endedAt = ann
	}
	if startedAt.IsZero() {
		startedAt = info.ModTime()
	}
	if endedAt.IsZero() {
		endedAt = info.ModTime()
	}

	var size int64
	var mtime int64
	if effInfo, statErr := AntigravityFileInfo(path); statErr == nil {
		size = effInfo.Size()
		mtime = effInfo.ModTime().UnixNano()
	} else {
		size = info.Size()
		mtime = info.ModTime().UnixNano()
	}

	sess := &ParsedSession{
		ID:                 antigravityIDPrefix + id,
		Project:            project,
		Machine:            machine,
		Agent:              AgentAntigravity,
		FirstMessage:       firstMessage,
		StartedAt:          startedAt,
		EndedAt:            endedAt,
		MessageCount:       len(messages),
		UserMessageCount:   userCount,
		SourceVersion:      sourceVersion,
		TranscriptFidelity: transcriptFidelity,
		File: FileInfo{
			Path:  path,
			Size:  size,
			Mtime: mtime,
		},
	}
	accumulateMessageTokenUsage(sess, messages)
	applyUsageEventTokenTotals(sess, usageEvents)
	// gen_metadata rows with zero decoded usage events flag a possible
	// token-block wire-format change. Derived from the final usageEvents.
	sess.GenMetadataWithoutUsage = hasGenMetadata && len(usageEvents) == 0
	for i := range usageEvents {
		usageEvents[i].SessionID = sess.ID
	}
	if len(messages) == 0 {
		// Usage events still flow for message-less parses (e.g. an
		// undecodable DB with gen_metadata) so daily usage analytics
		// match the event-derived session totals stamped above.
		return sess, nil, usageEvents, nil
	}
	return sess, messages, usageEvents, nil
}

type antigravityStepLoadResult struct {
	messages    []ParsedMessage
	usageEvents []ParsedUsageEvent
	// executors is retained for preferred trajectory sidecars so their
	// generatorMetadata uses the same range-qualified model attribution as
	// SQLite gen_metadata.
	executors    []antigravityExecutorMetadata
	rawStepCount int
	// hasGenMetadata reports whether the steps DB carried a non-empty
	// gen_metadata table. Paired with an empty usageEvents slice it flags a
	// session whose gen_metadata rows failed to decode into usage -- an early
	// warning that a newer agy build changed the token-block wire format.
	hasGenMetadata bool
	// sourceVersion is the schema-fingerprint label of the .db, set by the
	// CLI loader while the DB is open. The IDE path computes it directly
	// from its own handle via antigravitySourceVersion, so both classify
	// identically.
	sourceVersion string
}

type antigravityStepKind int

const (
	antigravityStepKindUserInput       antigravityStepKind = 14
	antigravityStepKindPlannerResponse antigravityStepKind = 15
)

// These field numbers come from the FileDescriptorProto records embedded in
// the official Antigravity CLI 1.1.16 Go binary. Keep the identifiers aligned
// with the producer's protobuf names so the parser does not turn schema paths
// back into anonymous numeric guesses.
const (
	agCortexStepGeneratorMetadataChatModelField   = 1
	agCortexStepGeneratorMetadataStepIndicesField = 2

	agChatModelMetadataUsageField            = 4
	agChatModelMetadataResponseModelField    = 19
	agChatModelMetadataModelDisplayNameField = 21

	agModelUsageStatsModelField            = 1
	agModelUsageStatsInputTokensField      = 2
	agModelUsageStatsOutputTokensField     = 3
	agModelUsageStatsCacheWriteTokensField = 4
	agModelUsageStatsCacheReadTokensField  = 5

	agExecutorMetadataLastStepIndexField = 3
	agExecutorMetadataCascadeConfigField = 10
	agCascadeConfigPlannerConfigField    = 1
	agCascadePlannerConfigModelNameField = 28
)

type antigravityStep struct {
	idx       int
	kind      antigravityStepKind
	fields    []agProtoField
	timestamp time.Time
	role      RoleType
}

func newAntigravityStep(
	idx, stepType int, payload []byte,
) (antigravityStep, bool) {
	if len(payload) == 0 {
		return antigravityStep{}, false
	}
	fields, err := agProtoParse(payload)
	if err != nil || len(fields) == 0 {
		return antigravityStep{}, false
	}

	kind := antigravityStepKindFromProto(fields, stepType)
	role := roleForAntigravityStepKind(kind)

	return antigravityStep{
		idx:       idx,
		kind:      kind,
		fields:    fields,
		timestamp: earliestAntigravityTimestamp(fields),
		role:      role,
	}, true
}

func antigravityStepKindFromProto(
	fields []agProtoField, fallbackStepType int,
) antigravityStepKind {
	if f, ok := agProtoFind(fields, 1); ok && f.Wire == pbWireVarint {
		return antigravityStepKind(f.Varint)
	}
	return antigravityStepKind(fallbackStepType)
}

func roleForAntigravityStepKind(kind antigravityStepKind) RoleType {
	switch kind {
	case antigravityStepKindUserInput:
		return RoleUser
	case antigravityStepKindPlannerResponse:
		return RoleAssistant
	default:
		return RoleAssistant
	}
}

func loadAntigravityStepsWithRawCount(
	db *sql.DB,
) (antigravityStepLoadResult, error) {
	generations := loadAntigravityGenerationMetadata(db)
	executors := loadAntigravityExecutorMetadata(db)
	result := antigravityStepLoadResult{
		executors:      executors,
		hasGenMetadata: len(generations) > 0,
	}
	rows, err := db.Query(
		`SELECT idx, step_type, step_payload FROM steps ` +
			`ORDER BY idx`,
	)
	if err != nil {
		return result, fmt.Errorf("query steps: %w", err)
	}
	defer rows.Close()
	steps := make([]antigravityLoadedStep, 0)
	stepPositions := make(map[int]int)
	for rows.Next() {
		var (
			idx      int
			stepType int
			payload  []byte
		)
		if err := rows.Scan(&idx, &stepType, &payload); err != nil {
			return result, fmt.Errorf("scan step: %w", err)
		}
		parsedStep, parsed := newAntigravityStep(idx, stepType, payload)
		var msg ParsedMessage
		var decoded bool
		kind := antigravityStepKind(stepType)
		if parsed {
			kind = parsedStep.kind
			msg, decoded = decodeAntigravityParsedStep(parsedStep)
		}
		stepPositions[idx] = len(steps)
		steps = append(steps, antigravityLoadedStep{
			kind: kind, msg: msg, decoded: decoded,
		})
		result.rawStepCount++
	}
	if err := rows.Err(); err != nil {
		return result, fmt.Errorf("iterate steps: %w", err)
	}

	for _, generation := range generations {
		position, found := generationPlannerStep(
			generation, steps, stepPositions,
		)
		executorModel := ""
		if stepIndex, known := generation.maxStepIndex(); known {
			executorModel = executorModelForStep(executors, stepIndex)
		}
		if !found {
			result.appendGenMetadataUsage(
				generation.data, ParsedMessage{}, false, executorModel,
			)
			continue
		}
		step := &steps[position]
		step.msg = result.appendGenMetadataUsage(
			generation.data, step.msg, step.decoded, executorModel,
		)
	}
	for _, step := range steps {
		if step.decoded {
			result.messages = append(result.messages, step.msg)
		}
	}
	return result, nil
}

type antigravityLoadedStep struct {
	kind    antigravityStepKind
	msg     ParsedMessage
	decoded bool
}

type antigravityGenerationMetadata struct {
	idx              int
	data             []byte
	stepIndices      []int
	hasStepIndices   bool
	stepIndicesValid bool
}

func (g antigravityGenerationMetadata) maxStepIndex() (int, bool) {
	if !g.hasStepIndices {
		return g.idx, true
	}
	if !g.stepIndicesValid {
		return 0, false
	}
	return maxAntigravityStepIndex(g.stepIndices)
}

func maxAntigravityStepIndex(indices []int) (int, bool) {
	maxIdx := -1
	for _, idx := range indices {
		if idx > maxIdx {
			maxIdx = idx
		}
	}
	return maxIdx, maxIdx >= 0
}

type antigravityExecutorMetadata struct {
	lastStepIndex int
	modelName     string
}

func loadAntigravityGenerationMetadata(
	db *sql.DB,
) []antigravityGenerationMetadata {
	rows, err := db.Query("SELECT idx, data FROM gen_metadata ORDER BY idx")
	if err != nil {
		return nil
	}
	defer rows.Close()

	var generations []antigravityGenerationMetadata
	for rows.Next() {
		var generation antigravityGenerationMetadata
		if err := rows.Scan(&generation.idx, &generation.data); err != nil {
			continue
		}
		generation.stepIndices,
			generation.hasStepIndices,
			generation.stepIndicesValid =
			extractAntigravityStepIndices(generation.data)
		generations = append(generations, generation)
	}
	return generations
}

func loadAntigravityExecutorMetadata(
	db *sql.DB,
) []antigravityExecutorMetadata {
	rows, err := db.Query("SELECT data FROM executor_metadata ORDER BY idx")
	if err != nil {
		return nil
	}
	defer rows.Close()

	var executors []antigravityExecutorMetadata
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			continue
		}
		executor, ok := extractAntigravityExecutorMetadata(data)
		if ok {
			executors = append(executors, executor)
		}
	}
	sort.SliceStable(executors, func(i, j int) bool {
		return executors[i].lastStepIndex < executors[j].lastStepIndex
	})
	return executors
}

func extractAntigravityStepIndices(
	data []byte,
) (indices []int, present bool, valid bool) {
	fields, err := agProtoParse(data)
	if err != nil {
		return nil, false, false
	}
	maxInt := uint64(^uint(0) >> 1)
	for _, field := range fields {
		if field.Number != agCortexStepGeneratorMetadataStepIndicesField {
			continue
		}
		present = true
		if field.Wire == pbWireVarint {
			if field.Varint > maxInt {
				return nil, true, false
			}
			indices = append(indices, int(field.Varint))
			continue
		}
		if field.Wire != pbWireBytes {
			return nil, true, false
		}
		for packed := field.Bytes; len(packed) > 0; {
			value, size := binary.Uvarint(packed)
			if size <= 0 || value > maxInt {
				return nil, true, false
			}
			indices = append(indices, int(value))
			packed = packed[size:]
		}
	}
	return indices, present, true
}

func extractAntigravityExecutorMetadata(
	data []byte,
) (antigravityExecutorMetadata, bool) {
	fields, err := agProtoParse(data)
	if err != nil {
		return antigravityExecutorMetadata{}, false
	}
	lastStep, ok := agProtoFind(
		fields, agExecutorMetadataLastStepIndexField,
	)
	if !ok || lastStep.Wire != pbWireVarint {
		return antigravityExecutorMetadata{}, false
	}
	cascadeConfig, ok := agProtoFind(
		fields, agExecutorMetadataCascadeConfigField,
	)
	if !ok || cascadeConfig.Nested == nil {
		return antigravityExecutorMetadata{}, false
	}
	plannerConfig, ok := agProtoFind(
		cascadeConfig.Nested, agCascadeConfigPlannerConfigField,
	)
	if !ok || plannerConfig.Nested == nil {
		return antigravityExecutorMetadata{}, false
	}
	modelNameField, ok := agProtoFind(
		plannerConfig.Nested, agCascadePlannerConfigModelNameField,
	)
	if !ok {
		return antigravityExecutorMetadata{}, false
	}
	modelName, ok := agProtoString(modelNameField)
	if !ok || !isPlausibleModelName(modelName) {
		return antigravityExecutorMetadata{}, false
	}
	return antigravityExecutorMetadata{
		lastStepIndex: int(lastStep.Varint),
		modelName:     modelName,
	}, true
}

func generationPlannerStep(
	generation antigravityGenerationMetadata,
	steps []antigravityLoadedStep,
	positions map[int]int,
) (int, bool) {
	for _, idx := range generation.stepIndices {
		position, ok := positions[idx]
		if !ok {
			continue
		}
		step := steps[position]
		if step.kind == antigravityStepKindPlannerResponse &&
			step.decoded {
			return position, true
		}
	}
	if generation.hasStepIndices {
		return 0, false
	}
	position, ok := positions[generation.idx]
	return position, ok
}

func executorModelForStep(
	executors []antigravityExecutorMetadata, stepIndex int,
) string {
	for _, executor := range executors {
		if executor.lastStepIndex >= stepIndex {
			return executor.modelName
		}
	}
	return ""
}

// appendGenMetadataUsage records a usage event from one gen_metadata
// payload and, when the step decoded into a message, attaches token
// counts and the model name to the returned copy. Usage extraction is
// deliberately independent of message decoding: a step the heuristic
// cannot render can still be rescued by the CLI trajectory sidecar
// transcript, and its usage must not be dropped.
func (r *antigravityStepLoadResult) appendGenMetadataUsage(
	data []byte,
	msg ParsedMessage,
	decoded bool,
	executorModel string,
) ParsedMessage {
	genModel := resolveAntigravityGenerationModel(data, executorModel)
	block, okUsage := extractTokenUsage(data)
	if okUsage {
		// ModelUsageStats.input_tokens is uncached input,
		// output_tokens includes thinking in the observed SQLite blocks,
		// and cache_read_tokens is absent when there are no cache hits.
		// Those blocks do not provide a separate persisted reasoning count.
		context := block.UncachedInput + block.CacheRead
		eventModel := genModel
		var occurredAt string
		if decoded {
			if eventModel == "" {
				eventModel = msg.Model
			}
			if !msg.Timestamp.IsZero() {
				occurredAt = msg.Timestamp.Format(time.RFC3339Nano)
			}
			msg.ContextTokens = context
			msg.OutputTokens = block.TotalOutput
			msg.HasContextTokens = context > 0
			msg.HasOutputTokens = block.TotalOutput > 0

		}
		r.usageEvents = append(r.usageEvents, ParsedUsageEvent{
			Source:               "generation",
			Model:                eventModel,
			InputTokens:          block.UncachedInput,
			OutputTokens:         block.TotalOutput,
			CacheReadInputTokens: block.CacheRead,
			ReasoningTokens:      0, // not available in gen_metadata
			OccurredAt:           occurredAt,
		})
	}
	if decoded && genModel != "" {
		msg.Model = genModel
	}
	return msg
}

// agTokenBlock carries the decoded token usage extracted from one
// gen_metadata blob. Field semantics are cross-validated against sidecar
// ground truth (generatorMetadata[].chatModel.usage matches in 550/550
// blocks):
//
//	UncachedInput = ModelUsageStats.input_tokens
//	TotalOutput   = ModelUsageStats.output_tokens, including thinking in
//	                observed SQLite blocks
//	CacheRead     = ModelUsageStats.cache_read_tokens
//
// No per-field reasoning breakdown is available in gen_metadata;
// TotalOutput already includes thinking tokens.
type agTokenBlock struct {
	UncachedInput int // ModelUsageStats.input_tokens
	TotalOutput   int // ModelUsageStats.output_tokens
	CacheRead     int // ModelUsageStats.cache_read_tokens
}

// maxPlausibleTokens caps the token values accepted by the heuristic.
// Other nested messages can coincidentally satisfy field1 ∈ [1000, 5000)
// while carrying large integers (e.g. a nanosecond latency).
// No real LLM generation involves more than a few million tokens,
// so blocks with values above this threshold are treated as false
// positives and skipped.
const maxPlausibleTokens = 2_000_000

func extractTokenUsage(data []byte) (agTokenBlock, bool) {
	fields, err := agProtoParse(data)
	if err != nil {
		return agTokenBlock{}, false
	}
	if chatModel, ok := extractAntigravityChatModel(fields); ok {
		usage, ok := agProtoFind(
			chatModel, agChatModelMetadataUsageField,
		)
		if !ok || usage.Nested == nil {
			return agTokenBlock{}, false
		}
		return tokenBlockFrom(usage.Nested)
	}
	return extractLegacyAntigravityTokenUsage(fields)
}

// extractLegacyAntigravityTokenUsage preserves support for older persisted
// records that predate CortexStepGeneratorMetadata.chat_model. Their enclosing
// message is not represented by the current embedded descriptors, so finding
// ModelUsageStats still requires the established bounded recursive walk.
func extractLegacyAntigravityTokenUsage(
	fields []agProtoField,
) (agTokenBlock, bool) {
	var found bool
	var block agTokenBlock
	var walk func([]agProtoField)
	walk = func(fs []agProtoField) {
		if found {
			return
		}
		if b, ok := tokenBlockFrom(fs); ok {
			block = b
			found = true
			return
		}
		for _, f := range fs {
			if f.Nested != nil {
				walk(f.Nested)
			}
		}
	}
	walk(fields)
	return block, found
}

// tokenBlockFrom reports whether fs is a plausible token usage block.
//
// The embedded ModelUsageStats descriptor names the fields below. Their usage
// semantics are cross-validated against sidecar ground truth
// (generatorMetadata[].chatModel.usage matches in 550/550 blocks):
//
//	model             = enum varint in [1000, 5000)
//	input_tokens      = uncached input
//	output_tokens     = total output including thinking
//	cache_write_tokens = deprecated and ignored
//	cache_read_tokens = cache-read input, absent when there are no cache hits
//
// No per-field reasoning breakdown is available in gen_metadata;
// the reasoning return value is always 0.
//
// input_tokens and output_tokens are required. cache_read_tokens is optional:
// proto3 omits zero-valued fields, and a fresh session with no cache hits omits
// it entirely. Requiring cache_read_tokens (the previous heuristic) caused the
// parser to miss token blocks in such sessions.
func tokenBlockFrom(fs []agProtoField) (agTokenBlock, bool) {
	model, hasModel := agProtoFind(fs, agModelUsageStatsModelField)
	inputTokens, hasInput := agProtoFind(
		fs, agModelUsageStatsInputTokensField,
	)
	outputTokens, hasOutput := agProtoFind(
		fs, agModelUsageStatsOutputTokensField,
	)
	// cache_read_tokens is optional: proto3 omits zero-valued fields.
	cacheReadTokens, hasCacheRead := agProtoFind(
		fs, agModelUsageStatsCacheReadTokensField,
	)

	if !hasModel || !hasInput || !hasOutput ||
		model.Wire != pbWireVarint || inputTokens.Wire != pbWireVarint ||
		outputTokens.Wire != pbWireVarint {
		return agTokenBlock{}, false
	}
	if model.Varint < 1000 || model.Varint >= 5000 {
		return agTokenBlock{}, false
	}
	if inputTokens.Varint > maxPlausibleTokens ||
		outputTokens.Varint > maxPlausibleTokens {
		return agTokenBlock{}, false
	}
	// Input and output are independent quantities, but an implausibly large
	// combined footprint signals a decoy block where both values individually
	// pass the per-field cap.
	if inputTokens.Varint+outputTokens.Varint > maxPlausibleTokens {
		return agTokenBlock{}, false
	}
	// cache_write_tokens is deprecated. Observed gen_metadata blocks leave it
	// absent or zero; validate it when present but do not report another class.
	if cacheWriteTokens, hasCacheWrite := agProtoFind(
		fs, agModelUsageStatsCacheWriteTokensField,
	); hasCacheWrite {
		if cacheWriteTokens.Wire != pbWireVarint ||
			cacheWriteTokens.Varint > maxPlausibleTokens {
			return agTokenBlock{}, false
		}
	}
	if hasCacheRead {
		if cacheReadTokens.Wire != pbWireVarint ||
			cacheReadTokens.Varint > maxPlausibleTokens {
			return agTokenBlock{}, false
		}
	}

	block := agTokenBlock{
		UncachedInput: int(inputTokens.Varint),
		TotalOutput:   int(outputTokens.Varint),
	}
	if hasCacheRead {
		block.CacheRead = int(cacheReadTokens.Varint)
	}
	return block, true
}

// extractModelName prefers ChatModelMetadata.model_display_name and falls back
// to ChatModelMetadata.response_model.
func extractModelName(data []byte) string {
	model, _ := extractAntigravityGenerationModel(data)
	return model
}

func extractAntigravityGenerationModel(data []byte) (string, bool) {
	fields, err := agProtoParse(data)
	if err != nil {
		return "", false
	}
	if chatModel, ok := extractAntigravityChatModel(fields); ok {
		return extractAntigravityChatModelName(chatModel)
	}
	return extractLegacyAntigravityGenerationModel(fields)
}

func extractAntigravityChatModel(
	fields []agProtoField,
) ([]agProtoField, bool) {
	chatModel, ok := agProtoFind(
		fields, agCortexStepGeneratorMetadataChatModelField,
	)
	if !ok || chatModel.Nested == nil {
		return nil, false
	}
	if usage, ok := agProtoFind(
		chatModel.Nested, agChatModelMetadataUsageField,
	); ok && usage.Nested != nil {
		return chatModel.Nested, true
	}
	if extractModelNameField(
		chatModel.Nested, agChatModelMetadataModelDisplayNameField,
	) != "" {
		return chatModel.Nested, true
	}
	if extractModelNameField(
		chatModel.Nested, agChatModelMetadataResponseModelField,
	) != "" {
		return chatModel.Nested, true
	}
	return nil, false
}

func extractAntigravityChatModelName(
	fields []agProtoField,
) (string, bool) {
	if model := extractModelNameField(
		fields, agChatModelMetadataModelDisplayNameField,
	); model != "" {
		return model, true
	}
	return extractModelNameField(
		fields, agChatModelMetadataResponseModelField,
	), false
}

func extractModelNameField(fields []agProtoField, fieldNumber int) string {
	field, ok := agProtoFind(fields, fieldNumber)
	if !ok {
		return ""
	}
	model, ok := agProtoString(field)
	if !ok || !isPlausibleModelName(model) {
		return ""
	}
	return model
}

// extractLegacyAntigravityGenerationModel preserves the recursive decoder for
// older persisted records that do not contain
// CortexStepGeneratorMetadata.chat_model.
func extractLegacyAntigravityGenerationModel(
	fields []agProtoField,
) (string, bool) {
	if model := extractModelNameFromFields(
		fields, agChatModelMetadataModelDisplayNameField,
	); model != "" {
		return model, true
	}
	return extractModelNameFromFields(
		fields, agChatModelMetadataResponseModelField,
	), false
}

func extractModelNameFromFields(
	fields []agProtoField, fieldNumber int,
) string {
	for _, field := range fields {
		if field.Number == fieldNumber {
			if model, ok := agProtoString(field); ok &&
				isPlausibleModelName(model) {
				return model
			}
		}
	}
	for _, field := range fields {
		if field.Nested != nil {
			if model := extractModelNameFromFields(
				field.Nested, fieldNumber,
			); model != "" {
				return model
			}
		}
	}
	return ""
}

func resolveAntigravityGenerationModel(
	data []byte, executorModel string,
) string {
	generationModel, hasDisplayLabel :=
		extractAntigravityGenerationModel(data)
	return resolveAntigravityModelName(
		generationModel, executorModel, hasDisplayLabel,
	)
}

// resolveAntigravityModelName reconciles the model reported by downstream LLM
// RPC generation metadata with the user's intended effort-qualified model from
// the covering executor range.
//
// Antigravity CLI does not record reasoning effort in generation metadata
// (field 19 response_model), recording only the base model slug or an internal
// serving canary identifier (e.g. "gemini-3.7-flash-exp-b"). The effort
// qualification (-low, -medium, -high) is recorded only in the covering
// ExecutorMetadata. If the generation model matches the executor's base model
// after stripping known experimental serving variants, the effort-qualified
// executor model is returned so dashboard usage accounting is not fragmented.
// If hasDisplayLabel is true or the models do not match, generationModel is
// preserved unchanged.
func resolveAntigravityModelName(
	generationModel, executorModel string, hasDisplayLabel bool,
) string {
	if hasDisplayLabel || executorModel == "" {
		return generationModel
	}
	normalizedGeneration := stripAntigravityExperimentalVariant(generationModel)
	if antigravityBaseModel(executorModel) == normalizedGeneration {
		return executorModel
	}
	return generationModel
}

func antigravityBaseModel(model string) string {
	for _, suffix := range []string{"-low", "-medium", "-high"} {
		if base, ok := strings.CutSuffix(model, suffix); ok {
			return base
		}
	}
	return model
}

// stripAntigravityExperimentalVariant strips internal backend serving
// experiment / canary suffixes (such as "-exp-b") observed in Antigravity CLI
// RPC responses so the model can be matched against covering executor ranges.
//
// Guidance for adding future suffixes:
//   - Only add exact, observed serving canary suffixes here (e.g. "-exp-a",
//     "-exp-c") once verified against upstream captures.
//   - DO NOT strip generic "-exp": standalone models exist whose canonical
//     name ends in "-exp" (e.g. "gemini-2.0-flash-exp"). Stripping generic
//     "-exp" causes false-positive matches against -high/-medium/-low executors
//     and incorrectly overrides the user's chosen model.
//   - Adding or modifying suffixes alters historical session parsing: always
//     bump dataVersion in internal/db/db.go, update the provenance notes in
//     docs/internal/session-format-sources.md, and add regression tests in
//     internal/parser/antigravity_test.go.
func stripAntigravityExperimentalVariant(model string) string {
	if base, ok := strings.CutSuffix(model, "-exp-b"); ok {
		return base
	}
	return model
}

// isPlausibleModelName reports whether s looks like a human-readable
// model identifier. The model_display_name and response_model fields can carry
// a nested protobuf message in older records whose low bytes are valid UTF-8.
// agProtoString cannot tell those apart from text, and the raw bytes
// previously leaked into messages.model (and broke `pg push`, which
// rejects NUL bytes). Require every rune to be printable, at least
// one letter to be present, and a reasonable length (<= 64 chars).
func isPlausibleModelName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	hasLetter := false
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
		if unicode.IsLetter(r) {
			hasLetter = true
		}
	}
	return hasLetter
}

// decodeAntigravityStep extracts a ParsedMessage from one step's
// protobuf payload. Without an upstream .proto we use heuristics:
//   - role: protobuf field 1 carries CortexStepType when present;
//     USER_INPUT (14) is user, and PLANNER_RESPONSE (15) plus other
//     non-user step kinds are assistant.
//   - content: best-effort human-facing strings found in the
//     payload tree. Internal ids, local Antigravity config paths,
//     model placeholders, and duplicate payload echoes are filtered
//     out. User-input steps prefer a single prompt-like string.
//   - timestamp: earliest google.protobuf.Timestamp-shaped field.
//   - tool calls: assistant steps whose payloads contain known tool
//     name strings emit structured ParsedToolCall entries so that
//     the timing panel can compute turns, categories, and counts.
func decodeAntigravityStep(
	idx, stepType int, payload []byte,
) (ParsedMessage, bool) {
	step, ok := newAntigravityStep(idx, stepType, payload)
	if !ok {
		return ParsedMessage{}, false
	}
	return decodeAntigravityParsedStep(step)
}

func decodeAntigravityParsedStep(
	step antigravityStep,
) (ParsedMessage, bool) {
	// Extract tool calls for assistant steps before the content guard
	// so that tool-only steps (no displayable text) are not silently
	// dropped.
	var calls []ParsedToolCall
	if step.role == RoleAssistant {
		calls = extractAntigravityToolCalls(step.idx, step.fields)
	}

	strs, urlOnly := cleanAntigravityStepStrings(step)

	// A non-user step whose only displayable content is a URL would
	// otherwise vanish: the URL noise filter drops it and there are no
	// tool calls to carry the step. Keep the URL rather than losing the
	// message. Steps with other prose or a tool call keep the URL
	// suppressed, since the noise filter still applies there.
	if len(strs) == 0 && len(calls) == 0 {
		strs = urlOnly
	}

	// Emit the message if it has displayable content OR tool calls.
	// Tool-only assistant steps (empty prose) are valid.
	if len(strs) == 0 && len(calls) == 0 {
		return ParsedMessage{}, false
	}

	content := strings.Join(strs, "\n\n")
	msg := ParsedMessage{
		Role:          step.role,
		Content:       content,
		ContentLength: len(content),
		Timestamp:     step.timestamp,
	}
	if len(calls) > 0 {
		msg.ToolCalls = calls
		msg.HasToolUse = true
	}
	return msg, true
}

// knownAntigravityToolNames is the set of tool names that Antigravity
// actually uses. Only strings present in this set are accepted as tool
// calls; generic taxonomy matches without a known Antigravity name
// are rejected. This prevents generic strings like "read", "write",
// "message", or "process" from being falsely matched.
var knownAntigravityToolNames = map[string]bool{
	// Antigravity-specific tools
	"view_file":                  true,
	"read_url_content":           true,
	"replace_file_content":       true,
	"multi_replace_file_content": true,
	"write_to_file":              true,
	"define_subagent":            true,
	"invoke_subagent":            true,
	"manage_subagents":           true,
	"send_message":               true,
	"manage_task":                true,
	"ask_permission":             true,
	"ask_question":               true,
	"schedule":                   true,
	"search_web":                 true,
	"generate_image":             true,
	// Gemini/Antigravity shared tools (also appear in CLI variant)
	"run_command":       true,
	"execute_command":   true,
	"run_shell_command": true,
	"grep_search":       true,
	"search_files":      true,
	"list_directory":    true,
	// Known CLI JSON structure tool names
	"edit_file":  true,
	"read_file":  true,
	"write_file": true,
}

// isAntigravityToolName reports whether s is a known Antigravity tool
// name. Only strings present in knownAntigravityToolNames are accepted;
// generic taxonomy matches are rejected.
func isAntigravityToolName(s string) bool {
	return knownAntigravityToolNames[s]
}

// extractAntigravityToolCalls walks the decoded protobuf field tree
// and returns one ParsedToolCall per tool invocation found. Uses the
// same heuristic-walker approach as extractTokenUsage / extractModelName:
// we identify strings that exactly match known tool names, collect any
// adjacent UUID-like string as the ToolUseID, and any adjacent JSON
// object string as the InputJSON.
//
// When no UUID-like ID is found, a synthetic deterministic ID is
// generated so the timing pipeline still has a stable key per call.
//
// Only strings matching Antigravity-known tool names are accepted.
func extractAntigravityToolCalls(
	stepIdx int, fields []agProtoField,
) []ParsedToolCall {
	// Collect all string values reachable from this step's field tree.
	// minLen=1 so we catch even short tool names like "Bash" or "Read".
	all := agProtoCollectStrings(fields, 1)

	var calls []ParsedToolCall
	seen := map[string]bool{}
	for i, s := range all {
		// Reject generic taxonomy matches that are not known Antigravity tools.
		if !isAntigravityToolName(s) {
			continue
		}
		cat := NormalizeToolCategory(s)

		// Look for an adjacent UUID-like string to use as ToolUseID.
		// We scan the neighbouring strings (within a small window on
		// either side) since the proto walker returns siblings in
		// encounter order. Prefer following siblings so a flat sequence
		// of tools doesn't mistakenly pick up previous IDs.
		toolUseID := ""
		for _, offset := range []int{1, 2, -1, -2} {
			j := i + offset
			if j < 0 || j >= len(all) {
				continue
			}
			// Check for intervening tool names to avoid stealing UUID of another tool call
			interveningTool := false
			if offset > 0 {
				for k := i + 1; k < j; k++ {
					if isAntigravityToolName(all[k]) {
						interveningTool = true
						break
					}
				}
			} else {
				for k := j + 1; k < i; k++ {
					if isAntigravityToolName(all[k]) {
						interveningTool = true
						break
					}
				}
			}
			if interveningTool {
				continue
			}

			if antigravityUUIDLikeRE.MatchString(all[j]) {
				toolUseID = all[j]
				break
			}
		}

		// Look for an adjacent JSON-object string to use as InputJSON.
		inputJSON := ""
		for _, offset := range []int{1, 2, -1} {
			j := i + offset
			if j < 0 || j >= len(all) {
				continue
			}
			// Check for intervening tool names to avoid stealing InputJSON of another tool call
			interveningTool := false
			if offset > 0 {
				for k := i + 1; k < j; k++ {
					if isAntigravityToolName(all[k]) {
						interveningTool = true
						break
					}
				}
			} else {
				for k := j + 1; k < i; k++ {
					if isAntigravityToolName(all[k]) {
						interveningTool = true
						break
					}
				}
			}
			if interveningTool {
				continue
			}

			if strings.HasPrefix(strings.TrimSpace(all[j]), "{") {
				inputJSON = all[j]
				break
			}
		}

		// Assign a synthetic ID when no UUID was found in the payload,
		// using the string index to make each invocation unique.
		if toolUseID == "" {
			toolUseID = fmt.Sprintf("ag-step-%d-%d", stepIdx, i)
		}

		// Avoid emitting duplicate tool hits from the same payload
		// (the walker may surface the same string via multiple paths).
		// We deduplicate by tool name + ID + Input JSON to avoid collapsing
		// multiple distinct invocations of the same tool in one step.
		// This runs after synthetic-ID assignment so that calls without
		// adjacent UUIDs still get position-unique keys.
		dedupKey := s + ":" + toolUseID + ":" + inputJSON
		if seen[dedupKey] {
			continue
		}
		seen[dedupKey] = true

		calls = append(calls, ParsedToolCall{
			ToolUseID: toolUseID,
			ToolName:  s,
			Category:  cat,
			InputJSON: inputJSON,
		})
	}
	return calls
}

func dedupeStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// cleanAntigravityStepStrings returns the displayable strings for a step
// and, separately, any bare-URL strings that the non-user noise filter
// removed. Callers fall back to urlOnly when a step would otherwise have
// no content, so URL-only assistant messages are not silently dropped.
func cleanAntigravityStepStrings(step antigravityStep) (cleaned, urlOnly []string) {
	for _, s := range dedupeStrings(agProtoCollectStrings(step.fields, 20)) {
		s = strings.TrimSpace(s)
		if isNoisyAntigravityStepString(s) {
			continue
		}
		if step.role != RoleUser && isNoisyAntigravityNonUserStepString(s) {
			continue
		}
		cleaned = append(cleaned, s)
	}
	cleaned = dedupeStrings(cleaned)
	bareURLs := collectAntigravityBareURLs(step.fields)
	if step.role == RoleUser {
		// A short URL-only prompt (e.g. "https://go.dev") falls below the
		// 20-rune prose threshold, so include bare URLs as prompt
		// candidates; prose, when present, still outscores a bare link.
		candidates := append(append([]string{}, cleaned...), bareURLs...)
		if prompt := bestAntigravityUserPrompt(candidates); prompt != "" {
			return []string{prompt}, nil
		}
		return cleaned, nil
	}
	return cleaned, bareURLs
}

// collectAntigravityBareURLs returns bare-URL strings from the step
// tree regardless of the 20-rune prose threshold used for general
// content. Short links such as "https://go.dev" fall below that
// threshold yet are real assistant content, so a URL-only step needs a
// dedicated low-threshold pass to survive the content guard.
func collectAntigravityBareURLs(fields []agProtoField) []string {
	var out []string
	for _, s := range agProtoCollectStrings(fields, 1) {
		s = strings.TrimSpace(s)
		if isNoisyAntigravityNonUserStepString(s) {
			out = append(out, s)
		}
	}
	return dedupeStrings(out)
}

func isNoisyAntigravityStepString(s string) bool {
	if s == "" {
		return true
	}
	if antigravityUUIDLikeRE.MatchString(s) {
		return true
	}
	if strings.HasPrefix(s, "MODEL_PLACEHOLDER_") {
		return true
	}
	if strings.HasPrefix(s, "{") &&
		(strings.Contains(s, `"toolAction"`) ||
			strings.Contains(s, `"toolSummary"`) ||
			strings.Contains(s, `"DirectoryPath"`)) {
		return true
	}
	if looksLikeAntigravityOpaqueID(s) {
		return true
	}
	if strings.HasPrefix(s, "file:///home/") {
		return true
	}
	if strings.HasPrefix(s, "/home/") &&
		strings.Contains(s, "/.gemini/") {
		return true
	}
	if strings.HasPrefix(s, "/Users/") &&
		strings.Contains(s, "/.gemini/") {
		return true
	}
	if strings.HasPrefix(s, `C:\Users\`) &&
		strings.Contains(s, `\.gemini\`) {
		return true
	}
	if strings.HasPrefix(s, "command(") ||
		strings.HasPrefix(s, "execute_url(") ||
		strings.HasPrefix(s, "read_url(") ||
		strings.HasPrefix(s, "mcp(") {
		return true
	}
	return false
}

func isNoisyAntigravityNonUserStepString(s string) bool {
	if !strings.HasPrefix(s, "http://") &&
		!strings.HasPrefix(s, "https://") {
		return false
	}
	// Only a bare URL is metadata noise (the target echoed by tool
	// actions). Assistant prose that merely begins with a link, which
	// always contains whitespace, is real content and must be kept.
	return !strings.ContainsAny(s, " \t\n")
}

func looksLikeAntigravityOpaqueID(s string) bool {
	if strings.ContainsAny(s, " \n\t") {
		return false
	}
	if len(s) < 16 || len(s) > 128 {
		return false
	}
	var alpha, digit, symbol int
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			alpha++
		case r >= '0' && r <= '9':
			digit++
		case r == '_' || r == '-' || r == '.':
			symbol++
		default:
			return false
		}
	}
	if alpha+digit+symbol != len(s) {
		return false
	}
	if digit == len(s) || digit+symbol == len(s) {
		return true
	}
	return alpha > 0 && digit > 0
}

func bestAntigravityUserPrompt(strs []string) string {
	var best string
	bestScore := -1
	for _, s := range strs {
		score := antigravityPromptScore(s)
		if score > bestScore {
			best = s
			bestScore = score
		}
	}
	if bestScore <= 0 {
		return ""
	}
	return best
}

func antigravityPromptScore(s string) int {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" || isNoisyAntigravityStepString(trimmed) {
		return -1
	}
	score := len(trimmed)
	if strings.ContainsAny(trimmed, " \n\t") {
		score += 50
	}
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		score -= 100
	}
	if strings.HasPrefix(trimmed, "/") || strings.HasPrefix(trimmed, "file://") {
		score -= 100
	}
	if !strings.ContainsAny(trimmed, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ") {
		score -= 100
	}
	return score
}

// earliestAntigravityTimestamp walks the field tree and returns
// the earliest plausible google.protobuf.Timestamp value.
// Plausible = seconds field in the year 2000..2100 range.
func earliestAntigravityTimestamp(
	fields []agProtoField,
) time.Time {
	var best time.Time
	var walk func([]agProtoField)
	walk = func(fs []agProtoField) {
		for _, f := range fs {
			if f.Nested != nil {
				if sec, nanos, ok := agProtoTimestamp(f.Nested); ok {
					if sec > 946_684_800 && sec < 4_102_444_800 {
						t := time.Unix(sec, int64(nanos))
						if best.IsZero() || t.Before(best) {
							best = t
						}
					}
				}
				walk(f.Nested)
			}
		}
	}
	walk(fields)
	return best
}

// readAntigravityAnnotation parses last_user_view_time from a
// pbtxt annotation file. Returns zero time on any failure.
func readAntigravityAnnotation(path string) time.Time {
	data, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}
	}
	// last_user_view_time:{seconds:1779326586 nanos:959000000}
	i := strings.Index(string(data), "last_user_view_time")
	if i < 0 {
		return time.Time{}
	}
	rest := string(data[i:])
	j := strings.Index(rest, "seconds:")
	if j < 0 {
		return time.Time{}
	}
	rest = rest[j+len("seconds:"):]
	end := strings.IndexAny(rest, " \n\t}")
	if end < 0 {
		return time.Time{}
	}
	var sec int64
	if _, err := fmt.Sscanf(rest[:end], "%d", &sec); err != nil {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}
