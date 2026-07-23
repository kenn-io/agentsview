package artifact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	artifactAPIPath        = "/api/v1/artifacts"
	httpTransportTimeout   = 120 * time.Second
	httpCursorCleanupLimit = 750 * time.Millisecond
	httpTransportMaxErrLen = 512
	peerImportModeHeader   = "X-Agentsview-Artifact-Import"
	peerImportModeDeferred = "deferred"
)

var errHTTPPeer = errors.New("artifact peer request failed")

func IsHTTPTarget(target string) bool {
	return strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://")
}

type httpTransport struct {
	stateMu            sync.Mutex
	base               string
	origin             string
	token              string
	client             *http.Client
	preparedOrigins    []string
	preparedCursor     string
	hasPreparedOrigins bool
	closed             bool
}

func newHTTPTransport(target, token string, allowInsecure bool) (*httpTransport, error) {
	u, err := url.Parse(strings.TrimRight(target, "/"))
	if err != nil {
		return nil, fmt.Errorf("parsing artifact peer URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("artifact peer target must use http:// or https://")
	}
	if u.Host == "" {
		return nil, errors.New("artifact peer target is missing a host")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("artifact peer target must not contain credentials, query, or fragment")
	}
	if u.Scheme == "http" && !allowInsecure && !isLoopbackEndpointHost(u.Hostname()) {
		return nil, errors.New("insecure artifact peer requires HTTPS")
	}
	if u.Scheme == "http" && allowInsecure && !isLoopbackEndpointHost(u.Hostname()) {
		log.Print("warning: artifact sync uses plaintext HTTP; credentials and archive content are not encrypted in transit")
	}
	base := strings.TrimRight(u.String(), "/")
	if !strings.HasSuffix(base, artifactAPIPath) {
		base += artifactAPIPath
	}
	return &httpTransport{
		base:   base,
		origin: (&url.URL{Scheme: u.Scheme, Host: u.Host}).String(),
		token:  token,
		client: &http.Client{
			Timeout: httpTransportTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (t *httpTransport) Prepare(ctx context.Context, _ ArtifactStore) error {
	if err := requireTransportContext(ctx); err != nil {
		return err
	}
	t.stateMu.Lock()
	if t.closed {
		t.stateMu.Unlock()
		return fs.ErrClosed
	}
	previousCursor := t.preparedCursor
	t.preparedOrigins = nil
	t.preparedCursor = ""
	t.hasPreparedOrigins = false
	t.stateMu.Unlock()
	if previousCursor != "" {
		if err := t.releaseCursor(previousCursor); err != nil {
			return fmt.Errorf("releasing previous artifact peer cursor: %w", err)
		}
	}
	page, err := t.getOriginsPage(ctx, "")
	if err != nil {
		return fmt.Errorf("connecting to artifact peer: %w", err)
	}
	t.stateMu.Lock()
	if t.closed {
		t.stateMu.Unlock()
		return errors.Join(fs.ErrClosed, t.releaseCursor(page.NextCursor))
	}
	t.preparedOrigins = append(t.preparedOrigins[:0], page.Origins...)
	t.preparedCursor = page.NextCursor
	t.hasPreparedOrigins = true
	t.stateMu.Unlock()
	return nil
}

func (t *httpTransport) Exchange(ctx context.Context, local ArtifactStore) (retErr error) {
	if err := validateTransportStore(ctx, local); err != nil {
		return err
	}
	t.stateMu.Lock()
	closed := t.closed
	t.stateMu.Unlock()
	if closed {
		return fs.ErrClosed
	}
	remoteIt := t.exchangeOrigins()
	localIt := &storeOriginIterator{store: local}
	defer func() {
		retErr = errors.Join(retErr, remoteIt.Close(ctx), localIt.Close())
	}()
	remoteOrigin, remoteOK, err := remoteIt.Next(ctx)
	if err != nil {
		return fmt.Errorf("fetching artifact origins from peer: %w", err)
	}
	localOrigin, localOK, err := localIt.Next(ctx)
	if err != nil {
		return fmt.Errorf("listing local artifact origins: %w", err)
	}
	for remoteOK || localOK {
		var origin string
		switch {
		case !localOK || (remoteOK && remoteOrigin < localOrigin):
			origin = remoteOrigin
			remoteOrigin, remoteOK, err = remoteIt.Next(ctx)
		case !remoteOK || localOrigin < remoteOrigin:
			origin = localOrigin
			localOrigin, localOK, err = localIt.Next(ctx)
		default:
			origin = remoteOrigin
			remoteOrigin, remoteOK, err = remoteIt.Next(ctx)
			if err == nil {
				localOrigin, localOK, err = localIt.Next(ctx)
			}
		}
		if err != nil {
			return fmt.Errorf("advancing artifact origins: %w", err)
		}
		if err := t.exchangeOrigin(ctx, local, origin); err != nil {
			return fmt.Errorf("exchanging peer artifacts for %s: %w", origin, err)
		}
	}
	if err := t.finalizePush(ctx); err != nil {
		return fmt.Errorf("finalizing peer artifact batch: %w", err)
	}
	return nil
}

func (t *httpTransport) exchangeOrigins() *httpOriginIterator {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	if t.hasPreparedOrigins {
		iterator := &httpOriginIterator{
			transport: t,
			items:     append([]string(nil), t.preparedOrigins...),
			cursor:    t.preparedCursor,
		}
		t.preparedOrigins = nil
		t.preparedCursor = ""
		t.hasPreparedOrigins = false
		if iterator.cursor == "" {
			iterator.done = true
		}
		return iterator
	}
	return &httpOriginIterator{transport: t}
}

func (t *httpTransport) Close() error {
	if t == nil {
		return nil
	}
	t.stateMu.Lock()
	if t.closed {
		t.stateMu.Unlock()
		return nil
	}
	t.closed = true
	cursor := t.preparedCursor
	t.preparedOrigins = nil
	t.preparedCursor = ""
	t.hasPreparedOrigins = false
	t.stateMu.Unlock()
	return t.releaseCursor(cursor)
}

func (t *httpTransport) exchangeOrigin(
	ctx context.Context, local ArtifactStore, origin string,
) (retErr error) {
	localIt := newStoreWireIterator(local, origin)
	remoteIt := &httpWireIterator{transport: t, origin: origin}
	defer func() {
		retErr = errors.Join(retErr, localIt.Close(), remoteIt.Close())
	}()
	localWire, localEntry, localOK, err := localIt.Next(ctx)
	if err != nil {
		return err
	}
	remoteWire, remoteOK, err := remoteIt.Next(ctx)
	if err != nil {
		return err
	}
	for localOK || remoteOK {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch {
		case !localOK || (remoteOK && compareWireRefs(remoteWire, localWire) < 0):
			if err := t.receiveArtifact(ctx, local, remoteWire); err != nil {
				if errors.Is(err, ErrArtifactNotFound) {
					remoteWire, remoteOK, err = remoteIt.Next(ctx)
					if err != nil {
						return err
					}
					continue
				}
				return err
			}
			remoteWire, remoteOK, err = remoteIt.Next(ctx)
			if err != nil {
				return err
			}
		case !remoteOK || compareWireRefs(localWire, remoteWire) < 0:
			if err := t.postEntry(ctx, local, localEntry); err != nil {
				return err
			}
			localWire, localEntry, localOK, err = localIt.Next(ctx)
			if err != nil {
				return err
			}
		default:
			repaired, err := repairQueuedTransportArtifact(ctx, local, remoteWire,
				func(consume func(io.Reader) error) error {
					return t.withArtifact(ctx, remoteWire, consume)
				})
			if err != nil {
				return err
			}
			if !repaired && localWire.Kind == KindCheckpoints {
				if err := t.compareCheckpoint(ctx, local, localEntry, remoteWire); err != nil {
					return err
				}
			}
			localWire, localEntry, localOK, err = localIt.Next(ctx)
			if err != nil {
				return err
			}
			remoteWire, remoteOK, err = remoteIt.Next(ctx)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

type httpOriginsPage struct {
	Origins    []string `json:"origins"`
	NextCursor string   `json:"next_cursor,omitempty"`
}

func (t *httpTransport) getOriginsPage(ctx context.Context, cursor string) (httpOriginsPage, error) {
	u := t.base + "/origins"
	query := url.Values{}
	query.Set("limit", strconv.Itoa(transportPageSize))
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	u += "?" + query.Encode()
	var page httpOriginsPage
	if err := t.getJSON(ctx, u, &page); err != nil {
		return httpOriginsPage{}, err
	}
	if len(page.Origins) > transportPageSize {
		return httpOriginsPage{}, fmt.Errorf("%w: peer origin page exceeds %d entries", ErrArtifactInvalid, transportPageSize)
	}
	for index, origin := range page.Origins {
		if err := validateOriginID(origin); err != nil {
			return httpOriginsPage{}, fmt.Errorf("%w: invalid peer origin: %v", ErrArtifactInvalid, err)
		}
		if index > 0 && page.Origins[index-1] >= origin {
			return httpOriginsPage{}, fmt.Errorf("%w: peer origins are not strictly increasing", ErrArtifactInvalid)
		}
	}
	return page, nil
}

type httpOriginIterator struct {
	transport *httpTransport
	cursor    string
	items     []string
	next      int
	done      bool
	guard     boundedCursorCycleGuard
	last      string
}

func (i *httpOriginIterator) Next(ctx context.Context) (string, bool, error) {
	for {
		if i.next < len(i.items) {
			origin := i.items[i.next]
			i.next++
			if i.last != "" && origin <= i.last {
				return "", false, fmt.Errorf("%w: peer origins are not strictly increasing", ErrArtifactInvalid)
			}
			i.last = origin
			return origin, true, nil
		}
		if i.done {
			return "", false, nil
		}
		page, err := i.transport.getOriginsPage(ctx, i.cursor)
		if err != nil {
			return "", false, err
		}
		i.items = page.Origins
		i.next = 0
		if page.NextCursor == "" {
			i.cursor = ""
			i.done = true
		} else {
			if i.guard.Observe(Cursor(page.NextCursor)) {
				return "", false, errors.New("artifact peer origin cursor cycle")
			}
			i.cursor = page.NextCursor
		}
	}
}

func (i *httpOriginIterator) Close(_ context.Context) error {
	if i == nil || i.cursor == "" {
		return nil
	}
	err := i.transport.releaseCursor(i.cursor)
	i.cursor = ""
	return err
}

type storeOriginIterator struct {
	store    ArtifactStore
	iterator OriginIterator
	items    []string
	next     int
	done     bool
	last     string
}

func (i *storeOriginIterator) Next(ctx context.Context) (string, bool, error) {
	for {
		if i.next < len(i.items) {
			origin := i.items[i.next]
			i.next++
			if i.last != "" && origin <= i.last {
				return "", false, fmt.Errorf("%w: local origins are not strictly increasing", ErrArtifactInvalid)
			}
			i.last = origin
			return origin, true, nil
		}
		if i.done {
			return "", false, nil
		}
		if i.iterator == nil {
			iterator, err := openStoreOriginIterator(ctx, i.store)
			if err != nil {
				return "", false, err
			}
			i.iterator = iterator
		}
		origins, nextErr := i.iterator.Next(ctx, transportPageSize)
		if nextErr != nil && !errors.Is(nextErr, io.EOF) {
			return "", false, nextErr
		}
		if len(origins) == 0 && !errors.Is(nextErr, io.EOF) {
			return "", false, fmt.Errorf("%w: local origin iterator made no progress", ErrArtifactInvalid)
		}
		if len(origins) > transportPageSize {
			return "", false, fmt.Errorf("%w: local origin page exceeds %d entries", ErrArtifactInvalid, transportPageSize)
		}
		for _, origin := range origins {
			if err := validateOriginID(origin); err != nil {
				return "", false, fmt.Errorf("%w: invalid local origin: %v", ErrArtifactInvalid, err)
			}
		}
		i.items = origins
		i.next = 0
		if errors.Is(nextErr, io.EOF) {
			i.done = true
		}
	}
}

func (i *storeOriginIterator) Close() error {
	if i.iterator == nil {
		return nil
	}
	return i.iterator.Close()
}

type httpIndexPage struct {
	OriginArtifactIndex
	NextCursor string `json:"next_cursor,omitempty"`
}

func (t *httpTransport) getIndexPage(
	ctx context.Context, origin, cursor string,
) (httpIndexPage, error) {
	u := t.base + "/" + url.PathEscape(origin) + "/index"
	q := url.Values{}
	q.Set("limit", strconv.Itoa(transportPageSize))
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	u += "?" + q.Encode()
	var page httpIndexPage
	if err := t.getJSON(ctx, u, &page); err != nil {
		return httpIndexPage{}, err
	}
	if page.Origin != origin {
		return httpIndexPage{}, fmt.Errorf("%w: peer index origin mismatch", ErrArtifactInvalid)
	}
	return page, nil
}

func (t *httpTransport) receiveArtifact(
	ctx context.Context, local ArtifactStore, wire WireRef,
) error {
	return t.withArtifact(ctx, wire, func(body io.Reader) error {
		_, err := createTransportArtifactFromWire(ctx, local, wire, body)
		if errors.Is(err, ErrArtifactInvalid) || errors.Is(err, ErrArtifactCorrupt) {
			log.Printf("artifact: skipping corrupt artifact %s/%s/%s from peer: %v",
				wire.Origin, wire.Kind, wire.Name, err)
			return nil
		}
		return err
	})
}

func (t *httpTransport) compareCheckpoint(
	ctx context.Context, store ArtifactStore, local Entry, wire WireRef,
) error {
	return t.withArtifact(ctx, wire, func(body io.Reader) error {
		return compareOrRepairCheckpoint(ctx, store, local, wire, body,
			"remote", wire.Origin, local.Ref.Name)
	})
}

func (t *httpTransport) withArtifact(
	ctx context.Context, wire WireRef, consume func(io.Reader) error,
) error {
	u := t.base + "/" + url.PathEscape(wire.Origin) + "/" +
		url.PathEscape(string(wire.Kind)) + "/" + url.PathEscape(wire.Name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	t.authorize(req)
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %s", ErrArtifactNotFound, resp.Status)
	}
	if resp.StatusCode != http.StatusOK {
		return httpStatusError(resp)
	}
	return consume(resp.Body)
}

func (t *httpTransport) postEntry(
	ctx context.Context, local ArtifactStore, entry Entry,
) (retErr error) {
	spool, size, _, err := spoolWireArtifact(ctx, local, entry)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, closeAndRemoveTransportSpool(spool)) }()
	wire, err := ToWireRef(entry.Ref)
	if err != nil {
		return err
	}
	return t.postArtifact(ctx, wire.Origin, string(wire.Kind), wire.Name, spool, size)
}

func (t *httpTransport) postArtifact(
	ctx context.Context,
	origin, kind, name string,
	body io.ReadSeeker,
	size int64,
) error {
	u := t.base + "/" + url.PathEscape(origin) + "/" + url.PathEscape(kind) + "/" + url.PathEscape(name)
	readerAt := bodyReaderAt(body)
	section := io.NewSectionReader(readerAt, 0, size)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, section)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(io.NewSectionReader(readerAt, 0, size)), nil
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Origin", t.origin)
	req.Header.Set(peerImportModeHeader, peerImportModeDeferred)
	t.authorize(req)
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return httpStatusError(resp)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

type readSeekerAt interface {
	io.ReadSeeker
	io.ReaderAt
}

func bodyReaderAt(body io.ReadSeeker) io.ReaderAt {
	if at, ok := body.(io.ReaderAt); ok {
		return at
	}
	return &lockedReadSeekerAt{body: body}
}

type lockedReadSeekerAt struct {
	mu   sync.Mutex
	body io.ReadSeeker
}

func (r *lockedReadSeekerAt) ReadAt(p []byte, off int64) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.body.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	return io.ReadFull(r.body, p)
}

func (t *httpTransport) finalizePush(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.base+"/finalize", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Origin", t.origin)
	t.authorize(req)
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return httpStatusError(resp)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func (t *httpTransport) getJSON(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	t.authorize(req)
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return httpStatusError(resp)
	}
	body, err := readTransportPage(resp.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}

func (t *httpTransport) authorize(req *http.Request) {
	if t.token != "" {
		req.Header.Set("Authorization", "Bearer "+t.token)
	}
}

func (t *httpTransport) releaseCursor(cursor string) error {
	if cursor == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), httpCursorCleanupLimit)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		t.base+"/cursors/"+url.PathEscape(cursor), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Origin", t.origin)
	t.authorize(req)
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return httpStatusError(resp)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func httpStatusError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, httpTransportMaxErrLen))
	msg := strings.TrimSpace(string(body))
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("%w: peer rejected the bearer token (401)", errHTTPPeer)
	}
	if msg == "" {
		return fmt.Errorf("%w: %s", errHTTPPeer, resp.Status)
	}
	return fmt.Errorf("%w: %s: %s", errHTTPPeer, resp.Status, msg)
}

type httpWireIterator struct {
	transport *httpTransport
	origin    string
	cursor    string
	items     []WireRef
	next      int
	done      bool
	guard     boundedCursorCycleGuard
	last      WireRef
	hasLast   bool
}

func (i *httpWireIterator) Next(ctx context.Context) (WireRef, bool, error) {
	for {
		if i.next < len(i.items) {
			wire := i.items[i.next]
			i.next++
			return wire, true, nil
		}
		if i.done {
			return WireRef{}, false, nil
		}
		page, err := i.transport.getIndexPage(ctx, i.origin, i.cursor)
		if err != nil {
			return WireRef{}, false, err
		}
		i.items, err = wireRefsFromIndex(page.OriginArtifactIndex)
		if err != nil {
			return WireRef{}, false, err
		}
		i.next = 0
		for _, item := range i.items {
			if i.hasLast && compareWireRefs(i.last, item) >= 0 {
				return WireRef{}, false, fmt.Errorf("%w: peer artifact index is not strictly increasing", ErrArtifactInvalid)
			}
			i.last = item
			i.hasLast = true
		}
		if page.NextCursor == "" {
			i.done = true
		} else {
			if i.guard.Observe(Cursor(page.NextCursor)) {
				return WireRef{}, false, errors.New("artifact peer index cursor cycle")
			}
			i.cursor = page.NextCursor
		}
	}
}

func (i *httpWireIterator) Close() error {
	return i.transport.releaseCursor(i.cursor)
}

func wireRefsFromIndex(index OriginArtifactIndex) ([]WireRef, error) {
	total := len(index.Segments) + len(index.Raw) + len(index.Manifests) + len(index.Meta) + len(index.Checkpoints)
	if total > transportPageSize {
		return nil, fmt.Errorf("%w: peer artifact index page exceeds %d entries", ErrArtifactInvalid, transportPageSize)
	}
	groups := [...]struct {
		kind  Kind
		names []string
	}{
		{KindSegments, index.Segments},
		{KindRaw, index.Raw},
		{KindManifests, index.Manifests},
		{KindMeta, index.Meta},
		{KindCheckpoints, index.Checkpoints},
	}
	items := make([]WireRef, 0, total)
	for _, group := range groups {
		for _, name := range group.names {
			ref, err := FromWireRef(index.Origin, group.kind, name)
			if err != nil {
				return nil, err
			}
			wire, err := ToWireRef(ref)
			if err != nil {
				return nil, err
			}
			items = append(items, wire)
		}
	}
	for index := 1; index < len(items); index++ {
		if compareWireRefs(items[index-1], items[index]) >= 0 {
			return nil, fmt.Errorf("%w: peer artifact index is not strictly increasing", ErrArtifactInvalid)
		}
	}
	return items, nil
}

var _ readSeekerAt = (*os.File)(nil)
