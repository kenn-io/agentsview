package gittrailer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"go.kenn.io/kit/atomicfile"
)

const maxMessageBytes = 2 << 20

var trailerLine = regexp.MustCompile(`(?m)^Agent-Session:[ \t]*([a-z0-9][a-z0-9-]{0,31}):([A-Za-z0-9][A-Za-z0-9._-]{0,127})[ \t]*$`)

// Stamp is the standalone message editor. A source maintains optional prepare
// context, while nil processes the final message without requiring a Git parent.
// Installed hooks use StampHook to require a successful matching preparation.
func Stamp(ctx context.Context, messageFile string, source *string, session Session) error {
	return stamp(ctx, messageFile, source, session, "")
}

func stamp(ctx context.Context, messageFile string, source *string, session Session, invocation string) error {
	ctx, cancel := context.WithTimeout(ctx, HookTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := session.validate(); err != nil {
		return err
	}
	path, err := filepath.Abs(messageFile)
	if err != nil {
		return errors.New("cannot resolve commit message path")
	}
	state, present, err := readPrepare(path)
	if err != nil {
		return err
	}
	preparing := source != nil && prepareSource(*source)
	managedFinal := source != nil && *source == "commit-msg"
	if source != nil && !preparing && !managedFinal {
		return nil
	}
	if !preparing && present {
		if err := os.Remove(path + prepareSuffix); err != nil {
			return errors.New("cannot clear prepare context")
		}
	}
	if managedFinal && (invocation == "" || !present || !state.Ready || state.Invocation != invocation) {
		return nil
	}
	if !preparing && present && !state.Ready {
		return nil
	}
	data, info, err := readRegular(path, maxMessageBytes)
	if err != nil {
		return errors.New("cannot read bounded regular commit message")
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return errors.New("commit message contains a NUL byte")
	}
	if preparing {
		state = prepareState{Source: *source, Invocation: invocation}
		if err := writePrepare(path, state); err != nil {
			return err
		}
	}
	busy, err := replaying(ctx)
	if err != nil || busy {
		return err
	}
	// Editor/amend/merge preparation needs no message edit. The pending record
	// becomes ready only after replay detection succeeds.
	if preparing && *source != "message" && *source != "template" {
		state.Ready = true
		if err := ctx.Err(); err != nil {
			return err
		}
		return writePrepare(path, state)
	}
	prefix, err := commentPrefix(ctx)
	if err != nil {
		return err
	}
	trailer := []byte("Agent-Session: " + session.Agent + ":" + session.ID)
	if bytes.HasPrefix(trailer, []byte(prefix)) {
		return errors.New("Git comment prefix would remove the Agent-Session trailer; skipping")
	}
	head, suffix := retainedMessage(data, prefix)
	stripped, err := runGit(ctx, "", nil, head, "stripspace", "--strip-comments")
	if err != nil {
		return errors.New("cannot check commit message text")
	}
	content := messageContent(stripped)
	sum := sha256.Sum256(content)
	fingerprint := hex.EncodeToString(sum[:])
	if preparing {
		state.Ready = true
		if *source == "template" {
			state.TemplateHash = fingerprint
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := writePrepare(path, state); err != nil {
			return err
		}
		// Git sets GIT_EDITOR=: when no editor will run. Deferring otherwise
		// prevents an editor from leaving only our automatically added footer.
		if *source == "template" || os.Getenv("GIT_EDITOR") != ":" {
			return nil
		}
	}
	if len(content) == 0 ||
		!preparing && present && state.Source == "template" && state.TemplateHash == fingerprint ||
		containsSession(head, session) {
		return nil
	}
	formatted, err := formatTrailer(ctx, head, prefix, session)
	if err != nil {
		return err
	}
	formatted = append(formatted, suffix...)
	if len(formatted) > maxMessageBytes {
		return errors.New("formatted commit message exceeds byte limit")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return atomicWrite(path, formatted, info.Mode())
}

// Git's ordinary empty/template checks ignore canonical sign-off lines.
// Comparison normalization never changes the author's message bytes.
func messageContent(stripped []byte) []byte {
	var content []byte
	for line := range bytes.SplitSeq(stripped, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("Signed-off-by: ")) {
			continue
		}
		content = append(content, line...)
		content = append(content, '\n')
	}
	content = bytes.Trim(content, " \t\r\n\v\f")
	// Git requires an amend! fixup message to retain a body. Do not add a
	// session trailer that would make a subject-only message appear nonempty.
	subject, body, hasBody := bytes.Cut(content, []byte("\n"))
	if bytes.HasPrefix(subject, []byte("amend! ")) && (!hasBody || len(bytes.TrimSpace(body)) == 0) {
		return nil
	}
	return content
}

func retainedMessage(data []byte, prefix string) (head, suffix []byte) {
	marker := []byte(prefix + " ------------------------ >8 ------------------------")
	offset := 0
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		if bytes.HasPrefix(line, marker) {
			return data[:offset], data[offset:]
		}
		offset += len(line) + 1
	}
	return data, nil
}

func containsSession(data []byte, session Session) bool {
	for _, match := range trailerLine.FindAllSubmatch(data, -1) {
		if string(match[1]) == session.Agent && string(match[2]) == session.ID {
			return true
		}
	}
	return false
}

func readRegular(path string, limit int64) ([]byte, os.FileInfo, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() || before.Size() > limit {
		return nil, nil, errors.New("file must be bounded and regular")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() || !os.SameFile(before, info) {
		return nil, nil, errors.New("file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(data)) > limit {
		return nil, nil, errors.New("file exceeds byte limit")
	}
	return data, info, nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".agentsview-trailer-*")
	if err != nil {
		return errors.New("cannot create atomic trailer update")
	}
	name := file.Name()
	defer os.Remove(name)
	_, err = file.Write(data)
	if err == nil {
		err = file.Chmod(mode)
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return errors.New("cannot write atomic trailer update")
	}
	if err := atomicfile.Replace(name, path); err != nil {
		return errors.New("cannot replace trailer file")
	}
	return nil
}
