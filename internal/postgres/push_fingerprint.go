package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
)

const pushFingerprintBatchSize = 900

type pushLocalMessageFingerprint struct {
	Sum           int64
	Max           int64
	Min           int64
	ContentHashFP string
	RoleTimeFP    string
	FlagsFP       string
	SystemFP      string
	ToolCallCount int
	ToolCallSum   int64
	ToolCallFP    string
	ToolResultFP  string
	TokenFP       string
	UsageEventFP  string
}

func localSessionDependencyPushFingerprint(
	ctx context.Context,
	local *db.DB,
	sessionID string,
	usageEventFingerprint string,
	usageKnown bool,
) (string, error) {
	msgFP, err := localPushMessageFingerprint(
		local, sessionID, usageEventFingerprint, usageKnown,
	)
	if err != nil {
		return "", err
	}
	findings, err := local.SessionSecretFindings(ctx, sessionID)
	if err != nil {
		return "", fmt.Errorf("reading local secret findings: %w", err)
	}
	pins, err := local.ListPinnedMessages(ctx, sessionID, "")
	if err != nil {
		return "", fmt.Errorf("reading local pinned messages: %w", err)
	}
	return hashLocalDependencyPayload(msgFP, findings, pins)
}

// hashLocalDependencyPayload is the shared final step of the per-session
// and batched dependency fingerprints. The JSON encoding is the persisted
// fingerprint format: any change re-pushes every session on the next push.
func hashLocalDependencyPayload(
	msgFP pushLocalMessageFingerprint,
	findings []db.SecretFinding,
	pins []db.PinnedMessage,
) (string, error) {
	payload := struct {
		Messages       pushLocalMessageFingerprint
		SecretFindings []db.SecretFinding
		Pins           []db.PinnedMessage
	}{
		Messages:       msgFP,
		SecretFindings: findings,
		Pins:           pins,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encoding local dependency fingerprint: %w", err)
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum), nil
}

// localPushDependencyState holds one chunk's worth of prefetched local
// fingerprint inputs. The push loop fingerprints every candidate session on
// every push; reading these per session (a dozen point queries each) took
// minutes on a full push, so the loop prefetches them in batched queries.
// Values must match the per-session methods byte for byte — see the
// batched twins in internal/db for the identity contract.
type localPushDependencyState struct {
	contentAgg     map[string]db.MessageContentAggregate
	contentHashFP  map[string]string
	roleTimeFP     map[string]string
	flagsFP        map[string]string
	systemFP       map[string]string
	tokenFP        map[string]string
	toolCallCount  map[string]int
	toolCallSum    map[string]int64
	toolCallFP     map[string]string
	toolResultFP   map[string]string
	secretFindings map[string][]db.SecretFinding
	pins           map[string][]db.PinnedMessage
}

func readLocalPushDependencyState(
	ctx context.Context, local *db.DB, sessionIDs []string,
) (*localPushDependencyState, error) {
	st := &localPushDependencyState{}
	var err error
	if st.contentAgg, err = local.MessageContentFingerprints(sessionIDs); err != nil {
		return nil, fmt.Errorf("computing local content fingerprints: %w", err)
	}
	if st.contentHashFP, err = local.MessageContentHashFingerprints(sessionIDs); err != nil {
		return nil, fmt.Errorf("computing local content hash fingerprints: %w", err)
	}
	if st.roleTimeFP, err = local.MessageRoleTimeFingerprintsWithTimestampNormalizer(
		sessionIDs, pgPushTimestampFingerprintText,
	); err != nil {
		return nil, fmt.Errorf("computing local role/time fingerprints: %w", err)
	}
	if st.flagsFP, err = local.MessageFlagsFingerprints(sessionIDs); err != nil {
		return nil, fmt.Errorf(
			"computing local message flags fingerprints: %w", err,
		)
	}
	if st.systemFP, err = local.SystemMessageFingerprints(sessionIDs); err != nil {
		return nil, fmt.Errorf(
			"computing local system message fingerprints: %w", err,
		)
	}
	if st.tokenFP, err = local.MessageTokenFingerprints(sessionIDs); err != nil {
		return nil, fmt.Errorf("computing local token fingerprints: %w", err)
	}
	if st.toolCallCount, err = local.ToolCallCounts(sessionIDs); err != nil {
		return nil, fmt.Errorf("counting local tool_calls: %w", err)
	}
	if st.toolCallSum, err = local.ToolCallContentFingerprints(sessionIDs); err != nil {
		return nil, fmt.Errorf(
			"computing local tool_call content fingerprints: %w", err,
		)
	}
	if st.toolCallFP, err = local.ToolCallFingerprints(sessionIDs); err != nil {
		return nil, fmt.Errorf("computing local tool_call fingerprints: %w", err)
	}
	if st.toolResultFP, err = local.ToolResultEventFingerprintsWithTimestampNormalizer(
		sessionIDs, pgPushTimestampFingerprintText,
	); err != nil {
		return nil, fmt.Errorf(
			"computing local tool_result_event fingerprints: %w", err,
		)
	}
	if st.secretFindings, err = local.SessionSecretFindingsBySession(
		ctx, sessionIDs,
	); err != nil {
		return nil, fmt.Errorf("reading local secret findings: %w", err)
	}
	if st.pins, err = local.PinnedMessagesBySession(ctx, sessionIDs); err != nil {
		return nil, fmt.Errorf("reading local pinned messages: %w", err)
	}
	return st, nil
}

// dependencyFingerprint assembles the same fingerprint as
// localSessionDependencyPushFingerprint from the prefetched maps. local is
// only queried on the usageKnown=false fallback, which the push loop never
// takes (it prefetches usage fingerprints for every candidate).
func (st *localPushDependencyState) dependencyFingerprint(
	local *db.DB,
	sessionID string,
	usageEventFingerprint string,
	usageKnown bool,
) (string, error) {
	agg := st.contentAgg[sessionID]
	msgFP := pushLocalMessageFingerprint{
		Sum:           agg.Sum,
		Max:           agg.Max,
		Min:           agg.Min,
		ContentHashFP: st.contentHashFP[sessionID],
		RoleTimeFP:    st.roleTimeFP[sessionID],
		FlagsFP:       st.flagsFP[sessionID],
		SystemFP:      st.systemFP[sessionID],
		ToolCallCount: st.toolCallCount[sessionID],
		ToolCallSum:   st.toolCallSum[sessionID],
		ToolCallFP:    st.toolCallFP[sessionID],
		ToolResultFP:  st.toolResultFP[sessionID],
		TokenFP:       st.tokenFP[sessionID],
	}
	if usageKnown {
		msgFP.UsageEventFP = usageEventFingerprint
	} else {
		var err error
		msgFP.UsageEventFP, err = local.UsageEventFingerprint(sessionID)
		if err != nil {
			return "", fmt.Errorf(
				"computing local usage event fingerprint: %w", err,
			)
		}
	}
	return hashLocalDependencyPayload(
		msgFP, st.secretFindings[sessionID], st.pins[sessionID],
	)
}

func localPushMessageFingerprint(
	local *db.DB,
	sessionID string,
	usageEventFingerprint string,
	usageKnown bool,
) (pushLocalMessageFingerprint, error) {
	fp := pushLocalMessageFingerprint{}
	var err error
	fp.Sum, fp.Max, fp.Min, err = local.MessageContentFingerprint(sessionID)
	if err != nil {
		return fp, fmt.Errorf("computing local content fingerprint: %w", err)
	}
	fp.ContentHashFP, err = local.MessageContentHashFingerprint(sessionID)
	if err != nil {
		return fp, fmt.Errorf("computing local content hash fingerprint: %w", err)
	}
	fp.RoleTimeFP, err = localMessageRoleTimePGFingerprint(local, sessionID)
	if err != nil {
		return fp, fmt.Errorf("computing local role/time fingerprint: %w", err)
	}
	fp.FlagsFP, err = local.MessageFlagsFingerprint(sessionID)
	if err != nil {
		return fp, fmt.Errorf("computing local message flags fingerprint: %w", err)
	}
	fp.SystemFP, err = local.SystemMessageFingerprint(sessionID)
	if err != nil {
		return fp, fmt.Errorf("computing local system message fingerprint: %w", err)
	}
	fp.ToolCallCount, err = local.ToolCallCount(sessionID)
	if err != nil {
		return fp, fmt.Errorf("counting local tool_calls: %w", err)
	}
	fp.ToolCallSum, err = local.ToolCallContentFingerprint(sessionID)
	if err != nil {
		return fp, fmt.Errorf("computing local tool_call content fingerprint: %w", err)
	}
	fp.ToolCallFP, err = local.ToolCallFingerprint(sessionID)
	if err != nil {
		return fp, fmt.Errorf("computing local tool_call fingerprint: %w", err)
	}
	fp.ToolResultFP, err = localToolResultEventPGFingerprint(local, sessionID)
	if err != nil {
		return fp, fmt.Errorf("computing local tool_result_event fingerprint: %w", err)
	}
	fp.TokenFP, err = local.MessageTokenFingerprint(sessionID)
	if err != nil {
		return fp, fmt.Errorf("computing local token fingerprint: %w", err)
	}
	if usageKnown {
		fp.UsageEventFP = usageEventFingerprint
		return fp, nil
	}
	fp.UsageEventFP, err = local.UsageEventFingerprint(sessionID)
	if err != nil {
		return fp, fmt.Errorf("computing local usage event fingerprint: %w", err)
	}
	return fp, nil
}

func localToolResultEventPGFingerprint(
	local *db.DB, sessionID string,
) (string, error) {
	return local.ToolResultEventFingerprintWithTimestampNormalizer(
		sessionID,
		pgPushTimestampFingerprintText,
	)
}
