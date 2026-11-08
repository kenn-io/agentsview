package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/ctxio"
	"go.kenn.io/kit/safefileio"
)

const exportPushMaxBytes = 64 << 20

// Capture is an owner-maintained codec integration seam, not a new codec.
// It must select one exact canonical ID in one bounded read-only SQLite
// snapshot, reject usage-only archives, call #2811's existing encode/decode
// validation, and return quality evidence for those same captured bytes.
// No separate-session traversal, source sync, credentials, network or subprocess.
// Ordinal gaps and filtered carrier usage references alone are NOT lost turns.
type exportPushSource interface {
	Capture(context.Context, string, string) (exportPushSnapshot, error)
}
type exportPushSnapshot struct {
	SessionID, Agent string
	Payload          []byte
	Messages         int
	Truncated        bool
	MalformedLines   int
	Fidelity         string
	KnownUncaptured  bool
	UnknownTail      bool
}
type unavailableExportPushSource struct{}

func (unavailableExportPushSource) Capture(ctx context.Context, _, _ string) (exportPushSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return exportPushSnapshot{}, err
	}
	return exportPushSnapshot{}, errors.New("codec integration unavailable")
}

func newExportPushCommand() *cobra.Command {
	return newExportPushCommandWithSource(unavailableExportPushSource{})
}

func newExportPushCommandWithSource(source exportPushSource) *cobra.Command {
	var agent, output string
	var prefix bool
	cmd := &cobra.Command{
		Use: "export-push <session-id>", Short: "export one exact archived snapshot for explicit review without uploading", GroupID: groupData,
		Long: "Export one exact canonical archive ID. Claude IDs are unprefixed; other agents use agent:<native-id>.\n" +
			"This does not promise a complete running session.\n" +
			"Known truncated, malformed, summary-only or uncaptured data is refused. Separately stored parent/child/sibling sessions are excluded; text already embedded in the selected session remains for review.\n" +
			"Writes a new private file only; no source sync, receiver token or network push. The receiver's first accepted snapshot is immutable across later closes too.\n" +
			"Snapshot export is unavailable in this build.",
		Args: cobra.ExactArgs(1), SilenceUsage: true, SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := runExportPush(cmd.Context(), source, args[0], agent, prefix, output, cmd.ErrOrStderr()); err != nil {
				return err
			}
			if outputFormat(cmd) == "json" {
				return json.MarshalWrite(cmd.OutOrStdout(), struct {
					SessionID      string `json:"session_id"`
					Status         string `json:"status"`
					SnapshotPrefix bool   `json:"snapshot_prefix"`
				}{args[0], "exported", true})
			}
			_, err := fmt.Fprintln(cmd.OutOrStdout(), "Snapshot exported; review the retained bytes before confirming the digest.")
			return err
		},
	}
	cmd.Flags().StringVar(&agent, "agent", "", "require exact stored agent; does not resolve session IDs")
	cmd.Flags().BoolVar(&prefix, "snapshot-prefix", false, "acknowledge the captured prefix and any unknown tail")
	cmd.Flags().StringVar(&output, "output", "", "new exclusive 0600 snapshot file")
	registerFormatFlags(cmd.Flags())
	return cmd
}

func runExportPush(ctx context.Context, source exportPushSource, id, agent string, prefix bool, output string, status io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !prefix {
		return errors.New("export-push: --snapshot-prefix must acknowledge the captured point and any unknown tail")
	}
	if output == "" || id == "" || len(id) > 160 || strings.ContainsAny(id, "~/\\\x00\r\n\t ") {
		return errors.New("export-push: exact canonical session ID and new --output file are required")
	}
	snapshot, err := source.Capture(ctx, id, agent)
	if err != nil {
		if _, unavailable := source.(unavailableExportPushSource); unavailable {
			return errors.New("export-push: snapshot export is unavailable in this build")
		}
		return errors.New("export-push: cannot capture selected snapshot; no output published")
	}
	if snapshot.SessionID != id || snapshot.Agent == "" || (agent != "" && snapshot.Agent != agent) || snapshot.Messages <= 0 || snapshot.Truncated || snapshot.MalformedLines != 0 || snapshot.Fidelity == "summary" || snapshot.KnownUncaptured || len(snapshot.Payload) == 0 || len(snapshot.Payload) > exportPushMaxBytes {
		return errors.New("export-push: selected snapshot is empty, incomplete, mismatched or exceeds limits")
	}
	canonical := id
	if snapshot.Agent != "claude" {
		if !strings.HasPrefix(id, snapshot.Agent+":") || len(id) <= len(snapshot.Agent)+1 {
			return errors.New("export-push: require exact canonical session ID")
		}
	} else if strings.Contains(canonical, ":") {
		return errors.New("export-push: Claude session ID must be unprefixed")
	}
	// Report scope BEFORE publishing. No transcript bytes, paths or provider
	// errors are emitted. Unknown coverage is never upgraded to final completion.
	if _, err = fmt.Fprintln(status, "Frozen snapshot through its captured point; may omit later turns (unknown tail). First accepted receiver snapshot remains immutable."); err != nil {
		return err
	}
	if err = publishExportPush(ctx, output, snapshot.Payload); err != nil {
		return err
	}
	digest := sha256.Sum256(snapshot.Payload)
	_, err = fmt.Fprintf(status, "Exported session=%s snapshot_sha256=%s snapshot_bytes=%d\n", id, hex.EncodeToString(digest[:]), len(snapshot.Payload))
	return err
}

func publishExportPush(ctx context.Context, path string, payload []byte) (err error) {
	if err = ctx.Err(); err != nil {
		return err
	}
	file, err := safefileio.CreatePrivateFile(path)
	if err != nil {
		return errors.New("export-push: output must be a new file")
	}
	err = writeExportPushContent(ctx, file, payload)
	if err != nil {
		_ = os.Remove(path)
	}
	return err
}

type exportPushPrivateOutput interface {
	io.Writer
	Sync() error
	Close() error
}
type exportPushPublicationError struct{ cause error }

func (e *exportPushPublicationError) Error() string {
	return "export-push: snapshot publication failed"
}
func (e *exportPushPublicationError) Unwrap() error { return e.cause }
func writeExportPushContent(ctx context.Context, file exportPushPrivateOutput, payload []byte) (err error) {
	defer func() {
		if closeErr := file.Close(); err == nil && closeErr != nil {
			err = &exportPushPublicationError{cause: closeErr}
		}
	}()
	if _, err = io.Copy(file, ctxio.Reader{Context: ctx, Reader: bytes.NewReader(payload)}); err != nil {
		return &exportPushPublicationError{cause: err}
	}
	if err = file.Sync(); err != nil {
		return &exportPushPublicationError{cause: err}
	}
	return nil
}
