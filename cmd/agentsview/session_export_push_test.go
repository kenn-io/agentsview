package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/safefileio"
	"hegel.dev/go/hegel"
)

type exportPushFixtureSource struct {
	snapshot exportPushSnapshot
	err      error
	calls    int
}

func (s *exportPushFixtureSource) Capture(context.Context, string, string) (exportPushSnapshot, error) {
	s.calls++
	return s.snapshot, s.err
}

// Consumer contract: require exact selection and explicit frozen-prefix consent;
// never infer final completeness from a missing ended_at or sparse ordinals.
func TestExportPushPrefixConsentAndRefusals(t *testing.T) {
	for _, state := range []string{"prefix", "empty", "truncated", "malformed", "summary", "gaps", "identity", "agent", "unavailable", "oversize"} {
		t.Run(state, func(t *testing.T) {
			source := &exportPushFixtureSource{snapshot: exportPushSnapshot{SessionID: "pi:selected", Agent: "pi", Payload: []byte("synthetic selected payload"), Messages: 1, UnknownTail: true}}
			prefix := true
			switch state {
			case "prefix":
				prefix = false
			case "empty":
				source.snapshot.Messages = 0
			case "truncated":
				source.snapshot.Truncated = true
			case "malformed":
				source.snapshot.MalformedLines = 1
			case "summary":
				source.snapshot.Fidelity = "summary"
			case "gaps":
				source.snapshot.KnownUncaptured = true
			case "identity":
				source.snapshot.SessionID = "pi:other"
			case "agent":
				source.snapshot.Agent = "claude"
			case "unavailable":
				source.err = errors.New("source error includes private content")
			case "oversize":
				source.snapshot.Payload = make([]byte, exportPushMaxBytes+1)
			}
			path := filepath.Join(t.TempDir(), "snapshot.json")
			var out bytes.Buffer
			err := runExportPush(t.Context(), source, "pi:selected", "pi", prefix, path, &out)
			require.Error(t, err)
			require.NotContains(t, err.Error(), "private content")
			_, err = os.Stat(path)
			require.True(t, os.IsNotExist(err))
		})
	}
}

func TestExportPushExactBytesExclusivePrivateOutput(t *testing.T) {
	// Markers represent already codec-validated bytes: selected mixed-purpose
	// tool content remains visible; no content is redacted after consent.
	selected := []byte(`{"selected":"mixed-purpose-tool-result","passive_parent_id":"parent-id"}`)
	source := &exportPushFixtureSource{snapshot: exportPushSnapshot{SessionID: "pi:selected", Agent: "pi", Payload: selected, Messages: 2, UnknownTail: true}}
	path := filepath.Join(t.TempDir(), "snapshot.json")
	var out bytes.Buffer
	require.NoError(t, runExportPush(t.Context(), source, "pi:selected", "pi", true, path, &out))
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, selected, got)
	require.Contains(t, string(got), "mixed-purpose-tool-result")
	require.NotContains(t, string(got), "separately-stored-parent-text")
	file, err := safefileio.OpenCurrentUserFile(path)
	require.NoError(t, err)
	require.NoError(t, safefileio.ValidatePrivateCurrentUserFile(file))
	require.NoError(t, file.Close())
	require.Contains(t, out.String(), "unknown tail")
	require.Error(t, runExportPush(t.Context(), source, "pi:selected", "pi", true, path, &out))
	got, err = os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, selected, got)
}

func TestExportPushDefaultUnavailableDoesNotCreateOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.json")
	cmd := newExportPushCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"pi:selected", "--agent", "pi", "--snapshot-prefix", "--output", path})
	require.ErrorContains(t, cmd.ExecuteContext(t.Context()), "codec integration unavailable")
	_, err := os.Stat(path)
	require.True(t, os.IsNotExist(err))
}

// Arbitrary selected bytes survive without redaction or reserialization.
// The real codec closure/privacy proof is a separate unmerged dependency.
func TestExportPushPreservesEverySelectedByte(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		text := hegel.Draw(ht, hegel.Text())
		payload := []byte(text)
		if len(payload) == 0 {
			payload = []byte{0}
		}
		source := &exportPushFixtureSource{snapshot: exportPushSnapshot{SessionID: "pi:selected", Agent: "pi", Payload: payload, Messages: 1}}
		path := filepath.Join(t.TempDir(), "snapshot.json")
		var out bytes.Buffer
		require.NoError(ht, runExportPush(t.Context(), source, "pi:selected", "pi", true, path, &out))
		got, err := os.ReadFile(path)
		require.NoError(ht, err)
		require.Equal(ht, payload, got)
	}, hegel.WithTestCases(100))
}

func TestExportPushCancellationPublishesNoFile(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	path := filepath.Join(t.TempDir(), "snapshot.json")
	require.Error(t, publishExportPush(ctx, path, []byte("selected")))
	_, err := os.Stat(path)
	require.True(t, os.IsNotExist(err))
}
