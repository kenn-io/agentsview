package rawderive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
)

const (
	maxParitySourcesPerBatch = 128
	maxParityHistoryEntries  = 1024
	maxParitySourceMembers   = 4096
	maxParityCandidateBytes  = 32 << 20
)

var ErrParityExclusionProvenanceUnavailable = errors.New("migration parity provider exclusion provenance unavailable")

type ParityQueue interface {
	ClaimParity(context.Context, string, time.Duration) (*ParityLease, error)
	HeartbeatParity(context.Context, ParityLease, time.Duration) error
	FinishParityRequest(context.Context, ParityLease, string) error
}

type ParityEvidence interface {
	NextParitySources(context.Context, ParityLease, string, int) ([]ParitySource, error)
	ReserveParitySource(context.Context, ParityLease, ParitySource) error
	NextParityHistory(context.Context, ParityLease, string, int64, int) ([]ParityHistoryEntry, error)
	PrepareParityGraph(context.Context, ParityLease, ParitySource, ParityGraph) (ParityGraph, error)
	RecordParitySource(context.Context, ParityLease, ParitySource, ParitySourceResult) error
}

type ExactManifestSource interface {
	LoadManifest(context.Context, rawsync.AuthIdentity, string) (rawsync.CanonicalManifest, error)
}

type ParityWorkerConfig struct {
	Evidence       ParityEvidence
	Manifests      ExactManifestSource
	Materializer   SourceMaterializer
	Parser         SourceParser
	Content        ingest.ContentOptions
	Binding        ParityBinding
	AttemptTimeout time.Duration
}

type ParityWorker struct {
	evidence       ParityEvidence
	manifests      ExactManifestSource
	materializer   SourceMaterializer
	parser         SourceParser
	content        ingest.ContentOptions
	binding        ParityBinding
	bindingDigest  ParityDigest
	attemptTimeout time.Duration
}

func NewParityWorker(config ParityWorkerConfig) (*ParityWorker, error) {
	if config.Evidence == nil || config.Manifests == nil || config.Materializer == nil || config.Parser == nil ||
		config.AttemptTimeout <= 0 || config.AttemptTimeout > 30*time.Minute {
		return nil, fmt.Errorf("invalid migration parity worker dependencies")
	}
	digest, err := DigestParityBinding(config.Binding)
	if err != nil {
		return nil, err
	}
	return &ParityWorker{evidence: config.Evidence, manifests: config.Manifests,
		materializer: config.Materializer, parser: config.Parser, content: config.Content,
		binding: config.Binding, bindingDigest: digest, attemptTimeout: config.AttemptTimeout}, nil
}

// RunBatch processes at most the lease's finite source budget. Sources and
// every retained manifest are replayed sequentially, so only one parser and
// one materialization can be active at a time.
func (w *ParityWorker) RunBatch(ctx context.Context, lease ParityLease) (int, error) {
	if w == nil || lease.BatchSize < 1 || lease.BatchSize > maxParitySourcesPerBatch ||
		lease.Request != w.binding.Request || !lease.ObservedAt.Equal(w.binding.ObservedAt) {
		return 0, fmt.Errorf("invalid migration parity worker lease")
	}
	if lease.BindingDigest != w.bindingDigest {
		return 0, fmt.Errorf("invalid migration parity worker lease binding")
	}
	sources, err := w.evidence.NextParitySources(ctx, lease, "", lease.BatchSize)
	if err != nil {
		return 0, parityWorkerBoundaryError(err)
	}
	if len(sources) > lease.BatchSize {
		return 0, fmt.Errorf("migration parity evidence returned an oversized source page")
	}
	processed := 0
	for _, source := range sources {
		if err = ctx.Err(); err != nil {
			return processed, err
		}
		if err = w.evidence.ReserveParitySource(ctx, lease, source); err != nil {
			return processed, parityWorkerBoundaryError(err)
		}
		sourceCtx, cancel := context.WithTimeout(ctx, w.attemptTimeout)
		var result ParitySourceResult
		var replayErr error
		if source.DependencyDigest == (ParityDigest{}) {
			result = paritySourceFailure("invalid")
		} else {
			result, replayErr = w.replaySource(sourceCtx, lease, source)
		}
		cancel()
		if replayErr != nil {
			if errors.Is(sourceCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				result = paritySourceFailure("parse_failed")
			} else {
				return processed, parityWorkerBoundaryError(replayErr)
			}
		}
		if err = ctx.Err(); err != nil {
			return processed, err
		}
		if err = w.evidence.RecordParitySource(ctx, lease, source, result); err != nil {
			return processed, parityWorkerBoundaryError(err)
		}
		processed++
	}
	return processed, nil
}

type parityCandidateState struct {
	prepared   ingest.PreparedSession
	logicalKey string
	active     bool
	bytes      int
	rows       int
}

func (w *ParityWorker) replaySource(ctx context.Context, lease ParityLease, source ParitySource) (ParitySourceResult, error) {
	history, code, err := w.loadHistory(ctx, lease, source)
	if err != nil || code != "" {
		return paritySourceFailure(parityWorkerFailureCode("history", err, code)), nil
	}
	states := make(map[string]parityCandidateState)
	exclusions := make(map[string]struct{})
	retainedBytes := 0
	retainedRows := 0
	for _, entry := range history {
		manifest, loadErr := w.manifests.LoadManifest(ctx, source.Identity, entry.ManifestID)
		if loadErr != nil {
			return paritySourceFailure(parityWorkerFailureCode("manifest", loadErr, "")), nil
		}
		if manifest.ManifestID != entry.ManifestID || manifest.Identity != source.Identity || SourceID(manifest) != source.ID {
			return paritySourceFailure("invalid"), nil
		}
		parsed, parseCode := w.parseManifest(ctx, manifest)
		if parseCode != "" {
			return paritySourceFailure(parseCode), nil
		}
		if parsed.Tombstone {
			for group, state := range states {
				state.active = false
				states[group] = state
			}
			clear(exclusions)
			continue
		}
		outcome := parsed.Outcome
		if !outcome.ResultSetComplete || len(outcome.SourceErrors) != 0 {
			return paritySourceFailure("parse_failed"), nil
		}
		if len(outcome.Results) > maxParitySourceMembers || len(outcome.ExcludedSessionIDs) > maxParitySourceMembers-len(outcome.Results) {
			return paritySourceFailure("limit_exceeded"), nil
		}
		nextActive := make(map[string]struct{}, len(outcome.Results))
		for _, parsedResult := range outcome.Results {
			if parsedResult.DataVersion == parser.DataVersionNeedsRetry || parsedResult.RetryReason != "" {
				return paritySourceFailure("parse_failed"), nil
			}
			candidate, candidateErr := ingest.PrepareCandidate(ctx, parsedResult.Result, w.content)
			if candidateErr != nil {
				return paritySourceFailure(parityWorkerFailureCode("prepare", candidateErr, "")), nil
			}
			var prior *ingest.PreparedSession
			candidateGroup, _ := GroupID(manifest, candidate.Session)
			if state, ok := states[candidateGroup]; ok {
				value := state.prepared
				prior = &value
			}
			prepared, prepareErr := PrepareParityCandidate(ctx, parsedResult.Result, prior, w.content, w.binding.ObservedAt)
			if prepareErr != nil {
				return paritySourceFailure(parityWorkerFailureCode("prepare", prepareErr, "")), nil
			}
			group, logicalKey := GroupID(manifest, prepared.Session)
			if _, duplicate := nextActive[group]; duplicate {
				return paritySourceFailure("invalid"), nil
			}
			nextActive[group] = struct{}{}
			if _, exists := states[group]; !exists && len(states)+len(exclusions) >= maxParitySourceMembers {
				return paritySourceFailure("limit_exceeded"), nil
			}
			graph := ParityGraph{Key: ParityMemberKey{SourceID: source.ID, LogicalKey: logicalKey, Kind: "session"},
				Prepared: prepared, PromptEvidenceDiscarded: w.content.ArchiveContent.UsageOnly(),
				Links: parityCandidateLinks(prepared)}
			bytes, sizeErr := parityCandidateGraphBytes(graph)
			if sizeErr != nil {
				return paritySourceFailure("limit_exceeded"), nil
			}
			if bytes > maxParityCandidateBytes-retainedBytes {
				return paritySourceFailure("limit_exceeded"), nil
			}
			rows := parityCandidateGraphRows(graph)
			if rows > maxParityChildRows-retainedRows {
				return paritySourceFailure("limit_exceeded"), nil
			}
			if previous, ok := states[group]; ok {
				retainedBytes -= previous.bytes
				retainedRows -= previous.rows
			}
			retainedBytes += bytes
			retainedRows += rows
			states[group] = parityCandidateState{prepared: prepared, logicalKey: logicalKey, active: true, bytes: bytes, rows: rows}
			delete(exclusions, prepared.Session.ID)
		}
		for group, state := range states {
			if _, ok := nextActive[group]; !ok {
				state.active = false
				states[group] = state
			}
		}
		clear(exclusions)
		for _, id := range outcome.ExcludedSessionIDs {
			if id == "" {
				return paritySourceFailure("invalid"), nil
			}
			exclusions[id] = struct{}{}
			if len(states)+len(exclusions) > maxParitySourceMembers {
				return paritySourceFailure("limit_exceeded"), nil
			}
			for group, state := range states {
				if state.prepared.Session.ID == id {
					state.active = false
					states[group] = state
				}
			}
		}
	}
	if len(states)+len(exclusions) > maxParitySourceMembers {
		return paritySourceFailure("limit_exceeded"), nil
	}
	result := ParitySourceResult{Complete: true, Code: "pending"}
	groups := make([]string, 0, len(states))
	for group, state := range states {
		if state.active {
			groups = append(groups, group)
		}
	}
	slices.Sort(groups)
	for _, group := range groups {
		state := states[group]
		graph := ParityGraph{Key: ParityMemberKey{SourceID: source.ID, LogicalKey: state.logicalKey, Kind: "session"},
			Prepared: state.prepared, PromptEvidenceDiscarded: w.content.ArchiveContent.UsageOnly(),
			Links: parityCandidateLinks(state.prepared)}
		preparedGraph, prepareErr := w.evidence.PrepareParityGraph(ctx, lease, source, graph)
		if prepareErr != nil {
			return ParitySourceResult{}, prepareErr
		}
		fingerprint, fingerprintErr := FingerprintParity(ctx, w.binding, preparedGraph)
		if fingerprintErr != nil {
			return paritySourceFailure(parityWorkerFailureCode("fingerprint", fingerprintErr, "")), nil
		}
		result.Members = append(result.Members, ParityMemberResult{Key: graph.Key, Fingerprint: &fingerprint})
	}
	excluded := make([]string, 0, len(exclusions))
	for id := range exclusions {
		excluded = append(excluded, id)
	}
	slices.Sort(excluded)
	for _, id := range excluded {
		key := ParityMemberKey{SourceID: source.ID, LogicalKey: id, Kind: "exclusion"}
		_, proofErr := w.evidence.PrepareParityGraph(ctx, lease, source, ParityGraph{
			Key: key, Overlay: ParityOverlay{ProviderExcluded: true},
		})
		member := ParityMemberResult{Key: key, Verdict: ParityMatched}
		if errors.Is(proofErr, ErrParityExclusionProvenanceUnavailable) {
			member.Verdict = ParityPartial
			result.Blocker = ParityPartial
			result.Code = "exclusion_provenance_unavailable"
		} else if proofErr != nil {
			return ParitySourceResult{}, proofErr
		}
		result.Members = append(result.Members, member)
	}
	return result, nil
}

func (w *ParityWorker) loadHistory(ctx context.Context, lease ParityLease, source ParitySource) ([]ParityHistoryEntry, string, error) {
	var history []ParityHistoryEntry
	var after int64
	for {
		page, err := w.evidence.NextParityHistory(ctx, lease, source.ID, after, 128)
		if err != nil {
			return nil, "", err
		}
		if len(page) == 0 {
			break
		}
		for _, entry := range page {
			if entry.Generation <= after || entry.ManifestID == "" {
				return nil, "missing_history", nil
			}
			if len(history) == maxParityHistoryEntries {
				return nil, "limit_exceeded", nil
			}
			history = append(history, entry)
			after = entry.Generation
		}
		if len(page) < 128 {
			break
		}
	}
	if len(history) == 0 || history[0].Generation != 1 ||
		history[len(history)-1].Generation != source.HeadGeneration ||
		history[len(history)-1].ManifestID != source.HeadManifestID {
		return nil, "missing_history", nil
	}
	return history, "", nil
}

func (w *ParityWorker) parseManifest(ctx context.Context, manifest rawsync.CanonicalManifest) (ParsedManifest, string) {
	if manifest.Manifest.Kind == rawsync.ManifestTombstone {
		return ParsedManifest{Tombstone: true}, ""
	}
	tree, err := w.materializer.Materialize(ctx, manifest)
	if err != nil {
		return ParsedManifest{}, parityWorkerFailureCode("materialize", err, "")
	}
	parsed, parseErr := w.parser.Parse(ctx, manifest, tree)
	cleanupErr := tree.Cleanup()
	if cleanupErr != nil {
		_ = tree.Cleanup()
	}
	if parseErr != nil {
		return ParsedManifest{}, parityWorkerFailureCode("parse", parseErr, "")
	}
	if cleanupErr != nil {
		return ParsedManifest{}, "cleanup_failed"
	}
	return parsed, ""
}

func parityCandidateLinks(prepared ingest.PreparedSession) []ParityLink {
	var links []ParityLink
	if prepared.Session.ParentSessionID != nil && *prepared.Session.ParentSessionID != "" {
		links = append(links, ParityLink{Kind: "parent", Ordinal: -1, CallIndex: -1, EventIndex: -1, Unresolved: *prepared.Session.ParentSessionID})
	}
	if prepared.Session.ParserParentSessionID != nil && *prepared.Session.ParserParentSessionID != "" {
		links = append(links, ParityLink{Kind: "parser-parent", Ordinal: -1, CallIndex: -1, EventIndex: -1, Unresolved: *prepared.Session.ParserParentSessionID})
	}
	for _, message := range prepared.Messages {
		for callIndex, call := range message.ToolCalls {
			if call.SubagentSessionID != "" {
				links = append(links, ParityLink{Kind: "call", Ordinal: message.Ordinal, CallIndex: callIndex, EventIndex: -1, Unresolved: call.SubagentSessionID})
			}
			for eventIndex, event := range call.ResultEvents {
				if event.SubagentSessionID != "" {
					links = append(links, ParityLink{Kind: "event", Ordinal: message.Ordinal, CallIndex: callIndex, EventIndex: eventIndex, Unresolved: event.SubagentSessionID})
				}
			}
		}
	}
	return links
}

func parityCandidateGraphBytes(graph ParityGraph) (int, error) {
	if err := validateParityGraphBounds(graph); err != nil {
		return 0, err
	}
	encoded, err := json.Marshal(graph)
	if err != nil {
		return 0, err
	}
	if len(encoded) > maxParityCandidateBytes {
		return 0, errParityLimit
	}
	return len(encoded), nil
}

func parityCandidateGraphRows(graph ParityGraph) int {
	rows := len(graph.Prepared.Messages) + len(graph.Prepared.UsageEvents) + len(graph.Prepared.Findings) + len(graph.Links)
	for _, message := range graph.Prepared.Messages {
		rows += len(message.ToolCalls) + len(message.ToolResults)
		for _, call := range message.ToolCalls {
			rows += len(call.ResultEvents)
		}
	}
	return rows
}

func paritySourceFailure(code string) ParitySourceResult {
	return ParitySourceResult{Complete: false, Blocker: ParityPartial, Code: code}
}

func parityWorkerFailureCode(stage string, err error, explicit string) string {
	if explicit != "" {
		return explicit
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, errParityLimit):
		return "limit_exceeded"
	case errors.Is(err, errMaterializationCleanup):
		return "cleanup_failed"
	case errors.Is(err, rawsync.ErrMissingObject), errors.Is(err, rawsync.ErrNotFound):
		return "missing_object"
	case errors.Is(err, ErrSandboxUnavailable):
		return "sandbox_unavailable"
	case stage == "manifest":
		return "invalid"
	case stage == "parse":
		return "parse_failed"
	default:
		return "internal"
	}
}

func parityWorkerBoundaryError(err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return fmt.Errorf("migration parity worker boundary failed: internal")
}
