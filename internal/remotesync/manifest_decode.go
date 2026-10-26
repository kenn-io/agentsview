package remotesync

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
)

// manifestEntryMaxBytes bounds one manifest entry as JSON. The longest path
// any supported system allows is a Windows long path of 32,767 UTF-16 units,
// which stays under this even with every character escaped.
const manifestEntryMaxBytes = 256 << 10

// errManifestEntryTooLarge reports a manifest entry past
// manifestEntryMaxBytes.
var errManifestEntryTooLarge error = manifestEntryTooLargeError{}

type manifestEntryTooLargeError struct{}

func (manifestEntryTooLargeError) Error() string {
	return fmt.Sprintf("manifest entry is larger than %d KiB", manifestEntryMaxBytes>>10)
}

// FailureSummary names the remote, as no real file path is this long.
func (manifestEntryTooLargeError) FailureSummary() string {
	return fmt.Sprintf(
		"remote sync failed: the remote host sent a file entry larger than "+
			"%d KiB, which no real file path needs; check that agentsview on "+
			"the remote host is up to date", manifestEntryMaxBytes>>10,
	)
}

// DecodeManifest reads a manifest reply and passes each entry to visit as it
// arrives, so a manifest of any length is never held in memory. A visit error
// stops decoding and is returned unchanged.
func DecodeManifest(r io.Reader, visit func(ManifestEntry) error) error {
	budget := &readBudget{r: r}
	dec := jsontext.NewDecoder(budget)
	// Each step may read at most one entry's worth past where the decoder
	// stands, so no single JSON value can grow the decoder's buffer further.
	arm := func() { budget.limit = dec.InputOffset() + manifestEntryMaxBytes }
	fail := func(err error) error {
		if budget.exceeded {
			return errManifestEntryTooLarge
		}
		return err
	}
	arm()
	if tok, err := dec.ReadToken(); err != nil {
		return fail(err)
	} else if tok.Kind() != '{' {
		return fmt.Errorf("manifest is a JSON %v, want an object", tok.Kind())
	}
	for {
		arm()
		if dec.PeekKind() == '}' {
			break
		}
		name, err := dec.ReadToken()
		if err != nil {
			return fail(err)
		}
		if name.String() != "files" {
			arm()
			if err := dec.SkipValue(); err != nil {
				return fail(err)
			}
			continue
		}
		if err := decodeManifestFiles(dec, arm, fail, visit); err != nil {
			return err
		}
	}
	if _, err := dec.ReadToken(); err != nil {
		return fail(err)
	}
	arm()
	if _, err := dec.ReadToken(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("manifest has data after the JSON object")
		}
		return fail(err)
	}
	return nil
}

func decodeManifestFiles(
	dec *jsontext.Decoder, arm func(), fail func(error) error,
	visit func(ManifestEntry) error,
) error {
	arm()
	tok, err := dec.ReadToken()
	if err != nil {
		return fail(err)
	}
	switch tok.Kind() {
	case 'n':
		return nil
	case '[':
	default:
		return fmt.Errorf("manifest files is a JSON %v, want an array", tok.Kind())
	}
	for {
		arm()
		if dec.PeekKind() == ']' {
			break
		}
		var entry ManifestEntry
		if err := json.UnmarshalDecode(dec, &entry); err != nil {
			return fail(err)
		}
		if err := visit(entry); err != nil {
			return err
		}
	}
	if _, err := dec.ReadToken(); err != nil {
		return fail(err)
	}
	return nil
}

// readBudget fails a read past limit, the absolute offset into r that the
// caller allows.
type readBudget struct {
	r        io.Reader
	read     int64
	limit    int64
	exceeded bool
}

func (b *readBudget) Read(p []byte) (int, error) {
	remaining := b.limit - b.read
	if remaining <= 0 {
		b.exceeded = true
		return 0, errManifestEntryTooLarge
	}
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := b.r.Read(p)
	b.read += int64(n)
	return n, err
}
