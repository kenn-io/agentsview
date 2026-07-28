package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/service"
)

func TestPrintSessionDetailShowsSecretLeak(t *testing.T) {
	d := &service.SessionDetail{}
	d.ID = "s1"
	d.SecretLeakCount = 3
	var buf bytes.Buffer
	require.NoError(t, printSessionDetailHuman(&buf, d))
	out := buf.String()
	assert.Contains(t, out, "Secrets")
	assert.Contains(t, out, "3")
}

func TestPrintSessionDetailHidesZeroSecretLeak(t *testing.T) {
	d := &service.SessionDetail{}
	d.ID = "s1"
	d.SecretLeakCount = 0
	var buf bytes.Buffer
	require.NoError(t, printSessionDetailHuman(&buf, d))
	assert.NotContains(t, buf.String(), "Secrets",
		"clean session should not show a Secrets line")
}

// stubGetService implements service.SessionService. Only Get is wired up;
// every other method panics to surface unintended use. Tests must not
// exercise them.
type stubGetService struct {
	// getDetails is keyed by ID; Get returns the pointer when present
	// and nil + nil when absent.
	getDetails map[string]*service.SessionDetail
	// getCalls records every (id) pair passed to Get.
	getCalls []string
	// getErr, when non-nil, is returned by Get in place of the lookup.
	getErr error
}

func (s *stubGetService) Get(
	_ context.Context, id string,
) (*service.SessionDetail, error) {
	s.getCalls = append(s.getCalls, id)
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.getDetails[id], nil
}

// Stubs for the remaining SessionService methods — resolveBareCodebuffID
// and resolveCodebuffBareID do not exercise them, so they panic.
func (s *stubGetService) List(context.Context, service.ListFilter) (*service.SessionList, error) {
	panic("List not expected")
}
func (s *stubGetService) Messages(context.Context, string, service.MessageFilter) (*service.MessageList, error) {
	panic("Messages not expected")
}
func (s *stubGetService) ToolCalls(context.Context, string) (*service.ToolCallList, error) {
	panic("ToolCalls not expected")
}
func (s *stubGetService) Sync(context.Context, service.SyncInput) (*service.SessionDetail, error) {
	panic("Sync not expected")
}
func (s *stubGetService) Watch(context.Context, string) (<-chan service.Event, error) {
	panic("Watch not expected")
}
func (s *stubGetService) Stats(context.Context, service.StatsFilter) (*service.SessionStats, error) {
	panic("Stats not expected")
}
func (s *stubGetService) Search(context.Context, service.SearchRequest) (*service.SessionSearchResult, error) {
	panic("Search not expected")
}
func (s *stubGetService) SearchContent(context.Context, service.ContentSearchRequest) (*service.ContentSearchResult, error) {
	panic("SearchContent not expected")
}
func (s *stubGetService) UsageSummary(context.Context, service.UsageRequest) (*service.UsageSummaryResult, error) {
	panic("UsageSummary not expected")
}
func (s *stubGetService) UsagePairwiseComparison(context.Context, service.UsagePairwiseComparisonRequest) (*service.UsagePairwiseComparisonResponse, error) {
	panic("UsagePairwiseComparison not expected")
}
func (s *stubGetService) FindSessionIDsByPartial(context.Context, string, int) ([]string, error) {
	panic("FindSessionIDsByPartial not expected")
}
func (s *stubGetService) ListRecallEntries(context.Context, service.RecallFilter) (*service.RecallList, error) {
	panic("ListRecallEntries not expected")
}
func (s *stubGetService) GetRecallEntry(context.Context, string) (*db.RecallEntry, error) {
	panic("GetRecallEntry not expected")
}
func (s *stubGetService) QueryRecallEntries(context.Context, service.RecallQuery) (*service.RecallQueryResult, error) {
	panic("QueryRecallEntries not expected")
}
func (s *stubGetService) ImportRecallEntries(context.Context, io.Reader, db.RecallImportOptions) (*db.RecallImportResult, error) {
	panic("ImportRecallEntries not expected")
}
func (s *stubGetService) ListSecrets(context.Context, service.SecretListFilter) (*service.SecretFindingList, error) {
	panic("ListSecrets not expected")
}
func (s *stubGetService) ScanSecrets(context.Context, service.SecretScanInput, func(service.SecretScanProgress)) (*service.SecretScanSummary, error) {
	panic("ScanSecrets not expected")
}

// stageCodebuffSession creates <projectsRoot>/<project>/chats/<rawID>/
// chat-messages.json so the FS walk in resolveBareCodebuffID surfaces the
// location. Returns the project name.
func stageCodebuffSession(t *testing.T, projectsRoot, project, rawID string) {
	t.Helper()
	chatDir := filepath.Join(projectsRoot, project, "chats", rawID)
	require.NoError(t, os.MkdirAll(chatDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(chatDir, "chat-messages.json"),
		[]byte("[]"), 0o644,
	))
}

func TestResolveBareCodebuffID_LocalMachine_Match(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	stageCodebuffSession(t, tmp, "myproject", "1704067200")
	cfg := config.Config{
		// cfg.LocalMachineName drives the --machine=local filter:
		// a session whose detail.Machine equals cfg.LocalMachineName
		// passes the gate.
		LocalMachineName: "test-machine",
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodebuff: {tmp},
		},
	}
	candidate := "codebuff:myproject:1704067200"
	svc := &stubGetService{
		getDetails: map[string]*service.SessionDetail{
			candidate: {Session: db.Session{
				ID:      candidate,
				Machine: "test-machine",
			}},
		},
	}
	got, err := resolveBareCodebuffID(
		context.Background(), svc, &cfg, "1704067200", "local",
	)
	require.NoError(t, err)
	assert.Equal(t, candidate, got,
		"matching machine must yield the canonical ID")
}

func TestResolveBareCodebuffID_LocalMachine_Mismatch(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	stageCodebuffSession(t, tmp, "myproject", "1704067200")
	cfg := config.Config{
		LocalMachineName: "test-machine",
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodebuff: {tmp},
		},
	}
	candidate := "codebuff:myproject:1704067200"
	svc := &stubGetService{
		// detail.Machine is a remote-synced machine; the default
		// --machine=local filter must reject it.
		getDetails: map[string]*service.SessionDetail{
			candidate: {Session: db.Session{
				ID:      candidate,
				Machine: "remote-box",
			}},
		},
	}
	got, err := resolveBareCodebuffID(
		context.Background(), svc, &cfg, "1704067200", "local",
	)
	require.NoError(t, err)
	assert.Empty(t, got,
		"non-matching machine must not surface as a local match")
}

func TestResolveBareCodebuffID_WildcardMatch(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	stageCodebuffSession(t, tmp, "myproject", "1704067200")
	cfg := config.Config{
		LocalMachineName: "test-machine",
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodebuff: {tmp},
		},
	}
	candidate := "codebuff:myproject:1704067200"
	svc := &stubGetService{
		// --machine=* must accept any machine value, including a
		// remote-synced record.
		getDetails: map[string]*service.SessionDetail{
			candidate: {Session: db.Session{
				ID:      candidate,
				Machine: "remote-box",
			}},
		},
	}
	got, err := resolveBareCodebuffID(
		context.Background(), svc, &cfg, "1704067200", "*",
	)
	require.NoError(t, err)
	assert.Equal(t, candidate, got,
		"wildcard --machine=* must accept records from any machine")
}

func TestResolveBareCodebuffID_SpecificMachineMatch(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	stageCodebuffSession(t, tmp, "myproject", "1704067200")
	cfg := config.Config{
		LocalMachineName: "test-machine",
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodebuff: {tmp},
		},
	}
	candidate := "codebuff:myproject:1704067200"
	svc := &stubGetService{
		getDetails: map[string]*service.SessionDetail{
			candidate: {Session: db.Session{
				ID:      candidate,
				Machine: "laptop",
			}},
		},
	}
	got, err := resolveBareCodebuffID(
		context.Background(), svc, &cfg, "1704067200", "laptop",
	)
	require.NoError(t, err)
	assert.Equal(t, candidate, got,
		"exact --machine=laptop must accept records from laptop")
}

// TestResolveBareCodebuffID_SpecificMachineMismatch pins the inverse of
// the previous test: a specific machine whose value doesn't match any
// record must not produce a candidate.
func TestResolveBareCodebuffID_SpecificMachineMismatch(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	stageCodebuffSession(t, tmp, "myproject", "1704067200")
	cfg := config.Config{
		LocalMachineName: "test-machine",
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodebuff: {tmp},
		},
	}
	candidate := "codebuff:myproject:1704067200"
	svc := &stubGetService{
		getDetails: map[string]*service.SessionDetail{
			candidate: {Session: db.Session{
				ID:      candidate,
				Machine: "desktop",
			}},
		},
	}
	got, err := resolveBareCodebuffID(
		context.Background(), svc, &cfg, "1704067200", "laptop",
	)
	require.NoError(t, err)
	assert.Empty(t, got)
}

// TestResolveBareCodebuffID_FreebuffPrefixProbeFromCodebuffRoots pins
// the dual-agent probe in resolveBareCodebuffID: when only Codebuff
// roots are configured (the realistic case — Freebuff shares the
// same on-disk layout but is absent from parser.Registry), the probe
// still tries both codebuff and freebuff prefixes against the
// service. This guards against single-prefix regression where the
// dual-agent probe would mis-classify Freebuff sessions as Codebuff.
//
// AgentDirs deliberately registers only codebuff; if both agents
// point at the same root FindCodebuffFreebuffMatches returns
// duplicate locations and the resolver fires the ambiguity error,
// which is correct (two agents see the same on-disk dir) but is
// out of scope for this particular test.
func TestResolveBareCodebuffID_FreebuffPrefixProbeFromCodebuffRoots(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	stageCodebuffSession(t, tmp, "myproject", "1704067200")
	cfg := config.Config{
		LocalMachineName: "test-machine",
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodebuff: {tmp},
		},
	}
	codebuffID := "codebuff:myproject:1704067200"
	freebuffID := "freebuff:myproject:1704067200"
	svc := &stubGetService{
		getDetails: map[string]*service.SessionDetail{
			codebuffID: nil,
			freebuffID: {Session: db.Session{
				ID:      freebuffID,
				Machine: "test-machine",
			}},
		},
	}
	got, err := resolveBareCodebuffID(
		context.Background(), svc, &cfg, "1704067200", "local",
	)
	require.NoError(t, err)
	assert.Equal(t, freebuffID, got,
		"Freebuff dual-prefix probe must surface a freebuff row")
}

// TestResolveCodebuffBareID_ServerBareReturnsError pins the E half of
// E+C: a non-canonical input against --server must NOT silently fall
// through to findSessionIDsByPartial (which was the previous bug
// surface). Instead it returns an action-oriented error pointing at
// `session list` and the canonical ID shapes.
func TestResolveCodebuffBareID_ServerBareReturnsError(t *testing.T) {
	t.Parallel()
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().String("server", "", "")
	cmd.Flags().String("machine", "local", "")
	cmd.Flags().Bool("pg", false, "")
	require.NoError(t, cmd.Flags().Set("server", "http://remote.example"))
	// svc.Get would panic because Get panics in stubGetService when
	// the path is wrong. We must never reach it. The bare input is
	// an ISO 8601 timestamp ("YYYY-MM-DDTHH-MM-SS.fffZ") — the
	// shape parseCodebuffSessionDate accepts as a Codebuff/Freebuff
	// session-dir name. A numeric-Unix-epoch like "1704067200" is
	// NOT a Codebuff timestamp and would short-circuit before the
	// remote-error branch (covered by
	// TestResolveCodebuffBareID_ServerBareUUIDForOtherAgent).
	svc := &stubGetService{}
	got, err := resolveCodebuffBareID(
		cmd, svc, "2026-07-16T00-09-00.236Z",
	)
	require.Error(t, err)
	assert.Empty(t, got)
	assert.Contains(t, err.Error(), "session list")
	// Regression-test every canonical-ID shape the error
	// enumerates. Stripping any of these lines from
	// errBareCodebuffRemoteUnsupported must break this test.
	assert.Contains(t, err.Error(), "codebuff:<project>:<ts>")
	assert.Contains(t, err.Error(), "freebuff:<project>:<ts>")
	assert.Contains(t, err.Error(), "host~codebuff:<project>:<ts>")
	assert.Contains(t, err.Error(), "host~freebuff:<project>:<ts>")
	// No Get calls must occur on the remote-error path.
	assert.Empty(t, svc.getCalls,
		"--server must not probe svc.Get for bare timestamps")
}

// TestResolveCodebuffBareID_PGBareReturnsError mirrors the previous
// test for the --pg path, which was the second roborev-flagged code
// path. Symmetric coverage guards against future divergence.
func TestResolveCodebuffBareID_PGBareReturnsError(t *testing.T) {
	t.Parallel()
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().String("server", "", "")
	cmd.Flags().String("machine", "local", "")
	cmd.Flags().Bool("pg", false, "")
	require.NoError(t, cmd.Flags().Set("pg", "true"))
	// Same ISO-8601 shape as the --server variant — see comment
	// on TestResolveCodebuffBareID_ServerBareReturnsError.
	svc := &stubGetService{}
	got, err := resolveCodebuffBareID(
		cmd, svc, "2026-07-16T00-09-00.236Z",
	)
	require.Error(t, err)
	assert.Empty(t, got)
	assert.Contains(t, err.Error(), "session list")
	assert.Contains(t, err.Error(), "codebuff:<project>:<ts>")
	assert.Contains(t, err.Error(), "freebuff:<project>:<ts>")
	assert.Contains(t, err.Error(), "host~codebuff:<project>:<ts>")
	assert.Contains(t, err.Error(), "host~freebuff:<project>:<ts>")
	assert.Empty(t, svc.getCalls)
}

// TestResolveCodebuffBareID_ServerBareUUIDForOtherAgent pins the
// regression fix: a non-canonical but non-Codebuff input (e.g. a
// bare Codex / Copilot / Gemini UUID) must NOT trigger the
// Codebuff-specific error on remote stores. It must pass through
// ("", nil) so the generic prefix resolver retries each registered
// agent. Without this guard, every --pg / --server bare lookup
// that previously fell through resolveServiceSessionID's prefix
// loop would short-circuit to the Codebuff error.
func TestResolveCodebuffBareID_ServerBareUUIDForOtherAgent(t *testing.T) {
	t.Parallel()
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().String("server", "", "")
	cmd.Flags().String("machine", "local", "")
	cmd.Flags().Bool("pg", false, "")
	require.NoError(t, cmd.Flags().Set("server", "http://remote.example"))
	svc := &stubGetService{}
	// 36-char hex with dashes — the shape of a real Codex /
	// Copilot / Gemini bare UUID. Definitely not a
	// Codebuff/Freebuff ISO timestamp.
	got, err := resolveCodebuffBareID(
		cmd, svc, "abcdef01-2345-6789-abcd-ef0123456789",
	)
	require.NoError(t, err,
		"non-Codebuff bare input must NOT fire the Codebuff error "+
			"on --server; it must fall through to the generic "+
			"resolver so resolveServiceSessionID can retry the "+
			"registered agent prefixes")
	assert.Empty(t, got,
		"resolveCodebuffBareID has no canonical ID to produce "+
			"for a non-Codebuff-shape input; calling code "+
			"preserves id for lookupSessionWithPrefixes")
	assert.Empty(t, svc.getCalls,
		"the early-exit path must not probe svc.Get")
}

// TestResolveCodebuffBareID_PGBareUUIDForOtherAgent mirrors the
// previous test for the --pg transport. Symmetric coverage guards
// against future divergence between --server and --pg paths.
func TestResolveCodebuffBareID_PGBareUUIDForOtherAgent(t *testing.T) {
	t.Parallel()
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().String("server", "", "")
	cmd.Flags().String("machine", "local", "")
	cmd.Flags().Bool("pg", false, "")
	require.NoError(t, cmd.Flags().Set("pg", "true"))
	svc := &stubGetService{}
	got, err := resolveCodebuffBareID(
		cmd, svc, "abcdef01-2345-6789-abcd-ef0123456789",
	)
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.Empty(t, svc.getCalls)
}

// TestResolveCodebuffBareID_CanonicalSkipsBare pins pass-through
// behaviour: canonical IDs must skip the resolver regardless of
// transport, because lookupSessionWithPrefixes handles them via the
// existing prefix-aware Get path. This protects against the Freebuff
// canonical-check fix regressing canonical-ID handling under --pg.
func TestResolveCodebuffBareID_CanonicalSkipsBare(t *testing.T) {
	t.Parallel()
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().String("server", "", "")
	cmd.Flags().String("machine", "local", "")
	cmd.Flags().Bool("pg", false, "")
	require.NoError(t, cmd.Flags().Set("pg", "true"))

	cases := []string{
		"codebuff:proj:1704067200",      // codebuff canonical
		"freebuff:proj:1704067200",      // freebuff canonical (new)
		"host~codebuff:proj:1704067200", // remote-synced canonical
		"host~freebuff:proj:1704067200", // remote-synced freebuff
	}
	for _, id := range cases {
		t.Run(id, func(t *testing.T) {
			t.Parallel()
			// svc.Get panics if called; canonical IDs must skip
			// the bare bridge entirely.
			svc := &stubGetService{}
			got, err := resolveCodebuffBareID(cmd, svc, id)
			require.NoError(t, err)
			assert.Empty(t, got)
			assert.Empty(t, svc.getCalls)
		})
	}
}

// TestIsCanonicalServiceSessionID_FreebuffPrefix pins the Freebuff
// special case. Without it, `freebuff:<proj>:<ts>` would be
// misclassified as a bare timestamp and --pg would reject it under
// option E.
func TestIsCanonicalServiceSessionID_FreebuffPrefix(t *testing.T) {
	t.Parallel()
	assert.True(t, isCanonicalServiceSessionID("freebuff:proj:1704067200"),
		"freebuff: prefix must be recognised as canonical")
	assert.True(t, isCanonicalServiceSessionID("host~freebuff:proj:1704067200"),
		"host~freebuff: remote-synced freebuff must be recognised")
}

// TestIsCanonicalServiceSessionID_BareTimestampNotCanonical pins the
// inverse: a bare timestamp must NOT be flagged as canonical so it
// enters the bare-resolution path on local reads.
func TestIsCanonicalServiceSessionID_BareTimestampNotCanonical(t *testing.T) {
	t.Parallel()
	assert.False(t, isCanonicalServiceSessionID("1704067200"))
	assert.False(t, isCanonicalServiceSessionID("2026-07-16T00-09-00.236Z"))
}

// TestCodebuffMachineMatches_EmptyFilter pins the empty-string arm
// of codebuffMachineMatches. The production --machine flag default is
// "local" so an empty filter never reaches the resolver naturally,
// but the helper itself treats "" and "local" identically; a future
// refactor that splits them would silently alter behaviour. This
// test guards the invariant.
func TestCodebuffMachineMatches_EmptyFilter(t *testing.T) {
	t.Parallel()
	assert.True(t, codebuffMachineMatches(
		"test-machine", "", "test-machine",
	), "empty filter must defer to localMachine like \"local\" does")
	assert.False(t, codebuffMachineMatches(
		"other", "", "test-machine",
	), "empty filter must still reject mismatched machines")
}
