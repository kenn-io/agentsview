package service

import (
	"context"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

type MemoryReadinessState string

const (
	MemoryReady       MemoryReadinessState = "ready"
	MemoryPartial     MemoryReadinessState = "partial"
	MemoryUnavailable MemoryReadinessState = "unavailable"
	MemoryUnknown     MemoryReadinessState = "unknown"
)

type MemoryCapabilityStatus struct {
	Status MemoryReadinessState `json:"status" enum:"ready,partial,unavailable,unknown"`
	Reason string               `json:"reason,omitempty"`
}

type MemoryVectorStatus struct {
	Status     MemoryReadinessState `json:"status" enum:"ready,partial,unavailable,unknown"`
	Reason     string               `json:"reason,omitempty"`
	Generation string               `json:"generation,omitempty"`
	Embedded   int64                `json:"embedded,omitempty"`
	Missing    int64                `json:"missing,omitempty"`
}

type MemoryArchiveStatus struct {
	Backend  string `json:"backend"`
	ReadOnly bool   `json:"read_only"`
	Identity string `json:"identity,omitempty"`
}

type MemorySourceStatus struct {
	Status MemoryReadinessState `json:"status" enum:"ready,partial,unavailable,unknown"`
	Reason string               `json:"reason,omitempty"`
}

type MemoryCoverage struct {
	Status   MemoryReadinessState   `json:"status" enum:"ready,partial,unavailable,unknown"`
	Lexical  MemoryCapabilityStatus `json:"lexical"`
	Semantic MemoryVectorStatus     `json:"semantic"`
}

type MemoryStatus struct {
	Status        MemoryReadinessState   `json:"status" enum:"ready,partial,unavailable,unknown"`
	ObservedAt    time.Time              `json:"observed_at"`
	ServerVersion string                 `json:"server_version,omitempty"`
	Archive       MemoryArchiveStatus    `json:"archive"`
	Lexical       MemoryCapabilityStatus `json:"lexical"`
	Semantic      MemoryVectorStatus     `json:"semantic"`
	Sources       MemorySourceStatus     `json:"sources"`
}

func (s MemoryStatus) Coverage() MemoryCoverage {
	return MemoryCoverage{Status: s.Status, Lexical: s.Lexical, Semantic: s.Semantic}
}

// NormalizeMemoryCoverage gives older remote responses and legacy service
// fakes an explicit state instead of serializing empty readiness strings.
func NormalizeMemoryCoverage(in MemoryCoverage) MemoryCoverage {
	if in.Status != "" {
		return in
	}
	unsupported := MemoryCapabilityStatus{Status: MemoryUnknown, Reason: "unsupported"}
	return MemoryCoverage{
		Status: MemoryUnknown, Lexical: unsupported,
		Semantic: MemoryVectorStatus{Status: unsupported.Status, Reason: unsupported.Reason},
	}
}

type MemoryStatusProvider interface {
	MemoryStatus(context.Context) (MemoryStatus, error)
}

func GetMemoryStatus(ctx context.Context, svc SessionService) (MemoryStatus, error) {
	provider, ok := svc.(MemoryStatusProvider)
	if !ok {
		return UnsupportedMemoryStatus(time.Now()), nil
	}
	return provider.MemoryStatus(ctx)
}

// UnsupportedMemoryStatus describes an older service or remote that does not
// expose the aggregate readiness contract.
func UnsupportedMemoryStatus(now time.Time) MemoryStatus {
	unsupported := MemoryCapabilityStatus{Status: MemoryUnknown, Reason: "unsupported"}
	return MemoryStatus{
		Status:     MemoryUnknown,
		ObservedAt: now.UTC(),
		Archive:    MemoryArchiveStatus{Backend: "unknown"},
		Lexical:    unsupported,
		Semantic:   MemoryVectorStatus{Status: unsupported.Status, Reason: unsupported.Reason},
		Sources:    MemorySourceStatus{Status: MemoryUnknown, Reason: "source_telemetry_unavailable"},
	}
}

func memoryStateFromDB(state string) MemoryReadinessState {
	switch state {
	case string(MemoryReady):
		return MemoryReady
	case string(MemoryPartial):
		return MemoryPartial
	case string(MemoryUnavailable):
		return MemoryUnavailable
	default:
		return MemoryUnknown
	}
}

func vectorStatusFromDB(in db.SemanticReadiness) MemoryVectorStatus {
	out := MemoryVectorStatus{
		Generation: in.Generation,
		Embedded:   in.Embedded,
		Missing:    in.Missing,
	}
	out.Status = memoryStateFromDB(in.State)
	out.Reason = in.Reason
	return out
}

func aggregateMemoryStatus(lexical MemoryCapabilityStatus, semantic MemoryVectorStatus) MemoryReadinessState {
	if lexical.Status == MemoryReady && semantic.Status == MemoryReady {
		return MemoryReady
	}
	if lexical.Status == MemoryUnavailable && semantic.Status == MemoryUnavailable {
		return MemoryUnavailable
	}
	if lexical.Status == MemoryUnknown && semantic.Status == MemoryUnknown {
		return MemoryUnknown
	}
	return MemoryPartial
}
